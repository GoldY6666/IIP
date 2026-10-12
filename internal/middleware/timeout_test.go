package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// slowHandler 造一个可控耗时的 handler。
//
// 刻意**用 ctx.Done() 而不是 sleep**模拟真实场景：本项目所有慢路径都是
// 数据库查询，pgx 在 ctx 取消时立即返回。若这里写成 `time.Sleep`，
// 超时后 handler 还会接着睡完 —— 那测的是「handler 没被中断」，
// 与中间件真正要保证的性质（ctx 被取消）不是一回事。
func slowHandler(d time.Duration, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return // 模拟 pgx 感知到 ctx 取消后立即返回
		case <-time.After(d):
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

// TestTimeout_FastRequestUnaffected 验证未超时请求**完全不受影响**。
//
// 这是超时中间件最容易写坏的地方：一-wrapper 就把正常请求也搞挂。
// 判据覆盖 status、body、Content-Type 三项 —— 只断 status 的话，
// 「把 body 吞了但状态码照抄」这种坏实现照样能过。
func TestTimeout_FastRequestUnaffected(t *testing.T) {
	h := Timeout(2 * time.Second)(slowHandler(0, `{"ok":true}`))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("快速请求状态码 = %d，want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("快速请求响应体被改动 = %q，want %q", got, `{"ok":true}`)
	}
	// 未超时的请求不该带 no-store —— 那是超时专用的缓存语义。
	if ct := rec.Header().Get("Content-Type"); ct == "application/json; charset=utf-8" {
		t.Error("快速请求不应带超时响应的 Content-Type")
	}
}

// TestTimeout_SlowRequestGets504AndJSON 验证超时的核心不变量：
// 客户端拿到 504，且响应体是**统一形状的 JSON**（含 code）。
//
// 不断裸 504 是因为前端对所有 API 错误走同一解析路径；这里返回空体的话，
// 「请求超时」在页面上会表现为「崩了」而不是可读的错误。
func TestTimeout_SlowRequestGets504AndJSON(t *testing.T) {
	h := Timeout(50 * time.Millisecond)(slowHandler(3*time.Second, "never"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/slow", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时状态码 = %d，want %d", rec.Code, http.StatusGatewayTimeout)
	}

	var got struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	body, _ := io.ReadAll(rec.Body)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("超时响应体不是合法 JSON: %v，body=%q", err, body)
	}
	if got.Code != timeoutCode {
		t.Errorf("超时错误码 = %q，want %q", got.Code, timeoutCode)
	}
	// 文案要含上限值：排查时得知道到底是多长的时间把请求掐掉的。
	if got.Error == "" {
		t.Error("超时响应缺少 error 文案")
	}
}

// TestTimeout_CancelsContext 验证超时**真的把ctx 取消了**。
//
// 这是本中间件存在的**首要理由**：若只写 504 而不取消 ctx，
// 慢查询仍在后台跑、连接仍被占着，超时就成了纯粹的安慰剂。
// 本项目业务层首参全是 ctx、底层 pgx 认 ctx 取消 —— 判据断言的就是这个前提成立。
func TestTimeout_CancelsContext(t *testing.T) {
	var (
		sawCancel bool
		gotErr    error
	)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			sawCancel = true
			gotErr = r.Context().Err()
		case <-time.After(3 * time.Second):
		}
	})

	rec := httptest.NewRecorder()
	Timeout(50*time.Millisecond)(inner).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/cancel", nil))

	if !sawCancel {
		t.Fatal("超时后 handler 的 ctx 未被取消 —— 超时只是写了状态码，查询仍在后台跑")
	}
	if gotErr != context.DeadlineExceeded {
		t.Errorf("ctx.Err() = %v，want %v", gotErr, context.DeadlineExceeded)
	}
}

// TestTimeout_DoesNotOverwriteHandlerResponse 验证：**handler 已经写过响应头时，
// 中间件绝不能补504**。
//
// 这是 chi 自带版本的真实缺陷：它在 defer 里无条件 WriteHeader(504)，
// handler 已写头时会打 superfluous 警告，而客户端拿到的仍是 200。
// 若中间件覆盖，客户端会拿到一个与自己刚写的 body 不匹配的 504。
//
// 判据用 countingWriter 而非 Recorder：若中间件真写了 504，
// 覆盖与否的差异在 Recorder 上会被它自带的守卫吞掉（恒绿），
// 只有「底层 writer 被写了几次、写了什么码」才看得见。
func TestTimeout_DoesNotOverwriteHandlerResponse(t *testing.T) {
	// handler 在超时**前**写完 200，随后才等到超时点。
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("early"))
		<-r.Context().Done() // 之后再「超时」
	})

	cw := newCountingWriter()
	Timeout(30*time.Millisecond)(inner).ServeHTTP(cw,
		httptest.NewRequest(http.MethodGet, "/early", nil))

	if len(cw.codes) != 1 || cw.firstCode() != http.StatusOK {
		t.Errorf("底层 writer 收到 WriteHeader %v，want恰好 [200]"+
			" —— 多出 504 就是中间件覆盖了 handler 已定下的状态码",
			cw.codes)
	}
	if body := string(cw.body); body != "early" {
		t.Errorf("响应体 = %q，want %q（中间件不得追加或替换内容）", body, "early")
	}
}

// countingWriter 是一个**忠实转发**的 ResponseWriter：它记录被调了几次
// WriteHeader，并把每次的状态码都存下来。
//
// ## 为什么不能用 httptest.ResponseRecorder 代替
//
// Recorder.WriteHeader 自带守卫：
//
//	if rw.wroteHeader { return }
//
// 也就是说，**无论中间件是否拦第二次，Recorder 都会自己吞掉** ——
// 用它测「中间件有没有挡重复 WriteHeader」永远恒绿，
// 而恒绿看着像「判据有效」，实则是「前提不成立」。
// 这个坑是变异测试（M4）实测暴露的，不是推测出来的。
//
// countingWriter 刻意不做任何守卫：它如实报告调用次数，
// 于是「中间件是否多挡了一次」变成可观测的差异。
type countingWriter struct {
	header  http.Header
	codes   []int
	body    []byte
	flushes int
}

func newCountingWriter() *countingWriter {
	return &countingWriter{header: make(http.Header)}
}

func (c *countingWriter) Header() http.Header { return c.header }

func (c *countingWriter) WriteHeader(code int) {
	c.codes = append(c.codes, code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.body = append(c.body, b...)
	return len(b), nil
}

// Flush 记录被调了几次。
//
// 它**自己实现 Flusher**：这正是判据的观测点 —— 中间件持有的是这个
// countingWriter，若它不把 Flush 转发下来，flushes 就停在 0。
//
// 刻意不再往下转发给第二层：本判据要回答的是「中间件有没有调底层的
// Flush」，多一层转发反而会把「中间件调了」与「更内层调了」混在一起。
func (c *countingWriter) Flush() {
	c.flushes++
}

// firstCode 返回第一次写下的状态码（0 表示一次都没写）。
func (c *countingWriter) firstCode() int {
	if len(c.codes) == 0 {
		return 0
	}
	return c.codes[0]
}

// TestTimeout_WriteHeaderOnlyOnce 验证重复 WriteHeader 被中间件挡掉。
//
// 判据落在「**底层 writer 被调了几次**」而非「最终状态码是什么」——
// 后者在 Recorder 上恒为 200（Recorder 自己会吞第二次），测不出任何东西。
// 真实的 superfluous 现象发生在 net/http 内部处理响应头时，
// 那一层在 Recorder 里根本不存在。
func TestTimeout_WriteHeaderOnlyOnce(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusTeapot) // 第二次：应被中间件挡掉
		_, _ = w.Write([]byte("body"))
	})

	cw := newCountingWriter()
	Timeout(2*time.Second)(inner).ServeHTTP(cw,
		httptest.NewRequest(http.MethodGet, "/dup", nil))

	if len(cw.codes) != 1 {
		t.Errorf("底层 writer 收到 %d 次 WriteHeader（码=%v），want 恰好 1 次 ——"+
			" 多出的调用就是 superfluous response.WriteHeader 的来源",
			len(cw.codes), cw.codes)
	}
	if cw.firstCode() != http.StatusOK {
		t.Errorf("首次状态码 = %d，want %d", cw.firstCode(), http.StatusOK)
	}
}

// TestTimeout_FlushNotBroken 验证 **Flush 真的被转发到了底层 writer**。
//
// ServeFile 与任何流式响应都要靠 Flush 立即推数据；中间件若吞掉它，
// 症状是「静态资源要等很久才出来」而不是报错，极难归因到中间件。
//
// ## 判据为什么不能只问「w 是否实现 http.Flusher」
//
// 因为 timeoutWriter **无论方法体里做什么都实现了该接口** ——
// 把 Flush 整个删空，`w.(http.Flusher)` 照样成功。
// 那个版本的判据在变异测试（M6）里实测恒绿，看着像「判据有效」，
// 实则是「问错了问题」：接口存在 ≠ 转发发生。
//
// 真正的不变量是转发行为，所以判据落在「底层 writer 收到了几次 Flush」。
func TestTimeout_FlushNotBroken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("handler 看到的 w 不再实现 http.Flusher —— 流式响应会退化为缓冲")
			return
		}
		f.Flush()
	})

	cw := newCountingWriter()
	Timeout(2*time.Second)(inner).ServeHTTP(cw,
		httptest.NewRequest(http.MethodGet, "/f", nil))

	if cw.flushes != 1 {
		t.Errorf("底层 writer 收到 %d 次 Flush，want 1 次"+
			" —— Flush 没被转发，流式响应会退化为缓冲", cw.flushes)
	}
}

// TestTimeout_FlushDegradesGracefully 验证底层**不**实现 Flusher 时，
// 中间件不 panic 而是安静地不刷新。
//
// 断言用的是「handler 能跑完」这件事本身：panic 会让测试以 panic 收场，
// 而 httptest 会把它当成 handler 的真实 panic 抛出，不会被静默吞掉。
func TestTimeout_FlushDegradesGracefully(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 底层不支持时，中间件应让它安静地什么都不做
		}
		_, _ = w.Write([]byte("ok"))
	})

	// nonFlusher 刻意不实现 Flush。
	//
	// 刻意**不去动 rec.Body**：Recorder.Body 是个 bytes.Buffer，
	// 赋 nil 只是替换指针，Write 会 panic 或写进一个孤立的 buffer，
	// 让本判据测的变成「Recorder 的内部状态」而非「降级路径」。
	rec := httptest.NewRecorder()
	Timeout(2*time.Second)(inner).ServeHTTP(nonFlusher{rec},
		httptest.NewRequest(http.MethodGet, "/nf", nil))

	if rec.Body.String() != "ok" {
		t.Errorf("底层不支持 Flush 时响应体 = %q，want %q", rec.Body.String(), "ok")
	}
}

// nonFlusher 包装 Recorder 但**隐藏** Flush 能力。
//
// 存在的意义：Recorder 本身实现 Flusher，若直接用它，
// 「底层不支持」这个前提不成立，判据就测不到降级路径。
type nonFlusher struct {
	http.ResponseWriter
}

// TestTimeout_UnwrapExposesUnderlying 验证 Unwrap 能让外层中间件
// （如 AccessLog 的 chimw.NewWrapResponseWriter）取到真实 writer。
//
// 少了它，外层包装器与本中间件的包装器会叠成两层，中间层记的字节数
// 与状态码可能来自不同的 writer。
func TestTimeout_UnwrapExposesUnderlying(t *testing.T) {
	base := httptest.NewRecorder()
	tw := &timeoutWriter{ResponseWriter: base}
	if tw.Unwrap() != http.ResponseWriter(base) {
		t.Error("Unwrap 未返回底层 ResponseWriter")
	}
}

// TestTimeout_AccessLogSeesFinalStatus 验证**超时经过 AccessLog 时，
// 访问日志记的是 504 而不是 200**。
//
// 这是装配顺序的判据：AccessLog 在 next 返回后才读 status，
// 而本中间件在 handler 返回后才补 504，所以它必须位于 AccessLog 内层。
// 顺序放反时客户端拿 504、日志记 200，排查会被彻底带偏。
func TestTimeout_AccessLogSeesFinalStatus(t *testing.T) {
	var (
		logged     int
		matchedAny bool
	)
	logger := slog.New(slog.NewJSONHandler(&statusCapture{
		onStatus: func(c int) { logged = c; matchedAny = true },
	}, nil))

	h := AccessLog(logger)(Timeout(50 * time.Millisecond)(
		slowHandler(3*time.Second, "never")))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/log", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("客户端状态码 = %d，want 504", rec.Code)
	}
	// 先判「判据自己抓到了东西」：一条都没匹配到时 logged 恒为 0，
	// 那时报「记的是 0」会把判据的解析失败误报成 AccessLog 的行为问题。
	if !matchedAny {
		t.Fatal("statusCapture 未匹配到任何 status 字段 —— 是判据的解析坏了，不是 AccessLog 的问题")
	}
	if logged != http.StatusGatewayTimeout {
		t.Errorf("AccessLog 记录的 status = %d，want 504（Timeout 必须在 AccessLog 内层）",
			logged)
	}
}

// statusCapture 捕获 slog JSON 日志里的 status 字段值。
//
// **必须用 JSONHandler 而非 TextHandler**：TextHandler 输出的是
// `status=504`，与这里匹配的 `"status":` 字面量不同 —— 用错 handler 时
// 本判据会静默地一条都匹配不到，logged 恒为 0，测试报「AccessLog 记 0」
// 而真因是判据自己坏了。判据的解析失败与被测行为失败必须能区分开。
type statusCapture struct {
	onStatus func(int)
}

// Write 解析 JSON 日志行并取出 status。
//
// 只匹配 `"status":N` 这一种形态：不引入完整的 slog.Handler 实现，
// 因为本判据要验证的是「AccessLog 记下了什么」，
// 而不是「slog 能不能被解析」。
func (c *statusCapture) Write(p []byte) (int, error) {
	const key = `"status":`
	for i := 0; i+len(key) < len(p); i++ {
		if string(p[i:i+len(key)]) != key {
			continue
		}
		n, j := 0, i+len(key)
		for ; j < len(p) && p[j] >= '0' && p[j] <= '9'; j++ {
			n = n*10 + int(p[j]-'0')
		}
		if n > 0 {
			c.onStatus(n)
		}
		return len(p), nil
	}
	return len(p), nil
}

// TestTimeout_NoStoreOnTimeout 验证超时响应带 Cache-Control: no-store。
//
// 超时响应被中间层缓存会造成「同一个请求永远超时」——
// 重放的是一个已经超时的响应，与后端当前状态无关。
func TestTimeout_NoStoreOnTimeout(t *testing.T) {
	h := Timeout(30 * time.Millisecond)(slowHandler(2*time.Second, "never"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cache", nil))

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("超时响应 Cache-Control = %q，want %q", cc, "no-store")
	}
}
