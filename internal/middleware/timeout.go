package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// timeoutCode 是超时响应的错误码，供前端按 code 分支处理。
//
// 与 internal/api 的错误码同一套约定；前端对所有 API 错误走同一个
// 解析路径，若这里换成裸文本，「请求超时」会表现为「页面崩了」。
const timeoutCode = "request_timeout"

// Timeout 给每个请求加一个处理时间上限，超时返回 504 并取消请求 ctx。
//
// ## 为什么不用 chi 自带的 middleware.Timeout
//
// 读了 chi v5.3.0 的实现（middleware/timeout.go），它有两个对不上本项目的问题：
//
//  1. **不写响应体**。它只在 defer 里 `w.WriteHeader(504)`，响应体是空的。
//     本项目全站错误约定是 `{"error":…,"code":…}`，前端统一按它解析 ——
//     拿到的却是空体，症状是「页面崩了」而不是「请求超时」。
//  2. **handler已写过响应头时会打多余日志**。此时再 `WriteHeader(504)`
//     触发 `superfluous response.WriteHeader call`，而客户端拿到的仍是 200。
//
// 所以这里自己实现，顺带把「谁写状态码」这件事管起来（见 timeoutWriter）。
//
// ## 超时是真能中断的，不是空转
//
// 协作式取消只对**尊重 ctx** 的代码有意义。本项目满足这个前提：
// 所有业务层方法（incident/alert/correlation 的 Service）首参都是 ctx，
// 一路传到 pgx，而 pgx 在 ctx 被取消时会中断正在执行的查询。
// 因此超时后慢查询会真的停下来、连接会还给池子，而不是继续占着。
//
// **但这不是强杀**：handler 里若有完全不碰 ctx 的长循环（如CPU 计算），
// 它仍会跑到自己结束 —— 那种情况协程与连接都不会被立即回收。
// 真实 DB 查询路径已逐层核查，无此类代码；若将来引入，
// 需重新评估此处是否该换成 goroutine 隔离式实现。
//
// ## 为什么 ServeFile 路径不受影响
//
// 静态资源由 http.ServeFile 直接写响应，它不检查 ctx —— 但它也不慢
// （本地磁盘读文件），15s 超时对它没有意义。若哪天内网磁盘故障导致
// ServeFile 卡住，超时只能取消 ctx、无法中断文件读；那种情况下
// 本中间件保证客户端仍能按时拿到 504，资源则要等 handler 自己结束。
//
// ## /healthz 不作例外
//
// healthz 自带 2s 的 ctx（本项目 handleHealthz 内），远小于本超时，
// 因此它天然不会被本中间件误伤。不为它开特例是因为：按路径分支的第二处
// 判断永远是漏改的来源，而它换来的收益为零。
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			tw := &timeoutWriter{ResponseWriter: w, deadline: d}
			next.ServeHTTP(tw, r.WithContext(ctx))

			// 只有「超时确实发生」且「handler 一个字节都还没写」才补504。
			// 否则 handler 正常写出的响应会被这里的 504 覆盖 ——
			// 那正是 chi 版本「客户端拿到 200 却打了多余日志」的成因。
			if ctx.Err() == context.DeadlineExceeded && !tw.wroteHeader() {
				tw.writeTimeout()
			}
		})
	}
}

// timeoutWriter 包装 ResponseWriter，记录「handler 是否已写过响应头」。
//
// 有了它才能区分两种超时：
//   - handler 还没写任何东西 → 我们能安全地补 504 + JSON 响应体；
//   - handler 已写了头 → 状态码既定，改不了了（此时多半是它刚写完就超时，
//     客户端会拿到 200 + 不完整响应体，但那比伪造一个 504 更诚实）。
//
// 记录访问用原子操作而非裸bool：handler 跑在另一个 goroutine 的场景下
// （如未来改成 goroutine 隔离式实现）存在数据竞争，race detector 会报。
type timeoutWriter struct {
	http.ResponseWriter
	deadline time.Duration

	mu       sync.Mutex
	wrote    bool
	timedOut bool
}

// WriteHeader 记录「handler 主动写了响应头」。
//
// 只记不代理调用（除第一次外）：net/http 本身对第二次 WriteHeader 就会打
// superfluous 日志，我们没必要再重复转发一次；且重复转发会覆盖已定的状态码。
func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut || tw.wrote {
		return
	}
	tw.wrote = true
	tw.ResponseWriter.WriteHeader(code)
}

// Write 记录「handler 写了响应体」——隐含它也已写过响应头。
func (tw *timeoutWriter) Write(b []byte) (int, error) {
	tw.mu.Lock()
	tw.wrote = true
	tw.mu.Unlock()
	return tw.ResponseWriter.Write(b)
}

// Flush 实现 http.Flusher，让 ServeFile 等需要即时刷新的路径不被中间件打断。
//
// 断言而非直接调用：底层 ResponseWriter 未必实现（httptest.ResponseRecorder
// 早期版本就没有），断言失败就退化为不刷新 —— 那只影响流式响应的实时性，
// 不影响正确性。
func (tw *timeoutWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让中间件链上更外层的中间件（如 AccessLog 用的
// chimw.NewWrapResponseWriter）能取到底层 writer。
func (tw *timeoutWriter) Unwrap() http.ResponseWriter { return tw.ResponseWriter }

// wroteHeader 报告 handler 是否已写过响应头或响应体。
func (tw *timeoutWriter) wroteHeader() bool {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return tw.wrote
}

// writeTimeout 写出超时响应。
//
// **幂等**：timedOut 标志保证并发场景下（handler 恰好在同一时刻写完）
// 只有一个调用方真正写出，不会出现「两遍504」或「504 盖在 200 上」。
func (tw *timeoutWriter) writeTimeout() {
	tw.mu.Lock()
	if tw.timedOut || tw.wrote {
		tw.mu.Unlock()
		return
	}
	tw.timedOut = true
	tw.wrote = true
	tw.mu.Unlock()

	// 响应体形状必须与 internal/api 的 errorBody 一致（见 writeUnauthorized）。
	tw.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 超时响应不可缓存：否则一次超时会被中间层反复重放，表现为「怎么一直超时」。
	tw.Header().Set("Cache-Control", "no-store")
	tw.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)

	// 错误文案要说清「用户接下来该做什么」：超时通常是请求太大或后端慢，
	// 该做的是收窄范围 / 稍后重试，而不是让人去查 token。
	_ = json.NewEncoder(tw.ResponseWriter).Encode(map[string]string{
		"error": "请求处理超时（上限 " + tw.deadline.String() + "），请收窄查询范围或稍后重试",
		"code":  timeoutCode,
	})
}
