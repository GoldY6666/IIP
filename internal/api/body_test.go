package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yu/iip/internal/alert"
)

// 本文件的判据只有一条不变量：
//
//	同一类坏输入（超限 / 非 UTF-8），在**任何** JSON 入口都必须得到
//	同一状态码与同一文案。
//
// 为什么不逐条写「webhook 超限回 413」这种点断言：
// 那种断言只保护今天这三个入口，加第四个入口的人照样会照抄一份新的读取逻辑。
// 矩阵断言把「新入口必须与旧入口表现一致」变成必须满足的条件，
// 它抓的是未来那个"又各写一套"的人。
//
// 边界说明：这里验的是 HTTP 层语义，不验落库。非 UTF-8 曾经要到 PG 才爆
// （SQLSTATE 22021 → 503「storage unavailable」），那条路径需要真库，
// 归 scripts/verify-onboarding.py 与真库回归覆盖。

// entry 是一个 JSON 入口的测试视图。
type entry struct {
	name   string
	h      http.Handler
	limit  int64
	method string
	path   string
}

// jsonEntries 返回全部 JSON 入口。**新增 JSON 入口时必须加到这里** ——
// 这是本文件唯一需要人维护的地方，也是它比三份点断言强的原因。
func jsonEntries() []entry {
	const webhookLimit = int64(1 << 20)

	comment := muxWithIncidentRoutes("/api/v1/incidents", IncidentComment(&fakeIncidentCommenter{}, discardLogger()))

	return []entry{
		{
			name:   "webhook",
			h:      AlertWebhook(alert.NewService(newFakeRepo(), discardLogger()), webhookLimit, discardLogger()),
			limit:  webhookLimit,
			method: http.MethodPost,
			path:   "/api/v1/alerts/webhook",
		},
		{
			name:   "correlation-config",
			h:      CorrelationConfigPut(&fakeCorrelationConfigStore{}, discardLogger()),
			limit:  correlationConfigMaxBytes,
			method: http.MethodPut,
			path:   "/api/v1/correlation/config",
		},
		{
			name:   "incident-comment",
			h:      comment,
			limit:  incidentBodyMaxBytes,
			method: http.MethodPost,
			path:   "/api/v1/incidents/7/comments",
		},
	}
}

func doEntry(e entry, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(e.method, e.path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// TestJSONEntriesRejectOversizeBodyWith413 断言所有入口把「体量超限」报成 413。
//
// 这条在 incidents 入口曾经是红的：decodeStrict 直接 Decode(r.Body)，
// MaxBytesReader 的错误被 Decode 一并返回，最后映射成
// 「请求体不是合法 JSON 或含未知字段」—— 体量问题被报成语法问题。
func TestJSONEntriesRejectOversizeBodyWith413(t *testing.T) {
	for _, e := range jsonEntries() {
		t.Run(e.name, func(t *testing.T) {
			body := []byte(`{"padding":"` + strings.Repeat("x", int(e.limit)+1024) + `"}`)
			rec := doEntry(e, body)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("超限应为 413，实际 %d（body=%s）", rec.Code, truncate(rec.Body.String()))
			}
			if msg := decodeError(t, rec); msg != msgPayloadTooLarge {
				t.Errorf("413 文案应为 %q，实际 %q", msgPayloadTooLarge, msg)
			}
		})
	}
}

// TestJSONEntriesRejectInvalidUTF8With400 断言所有入口在**解码之前**挡住非 UTF-8。
//
// 构造的是真实形态：合法的 JSON 外壳里包着 GBK 字节（0xD7 0xD4）。
// 这类请求体能通过 encoding/json（它不校验字符串的 UTF-8 合法性），
// 直到写库才被 PG 以 22021 拒掉 —— 那时错误已经变成 503「存储不可用」，
// 客户端编码问题被报成服务端故障。
func TestJSONEntriesRejectInvalidUTF8With400(t *testing.T) {
	gbk := []byte{0xd7, 0xd4, 0xb6, 0xaf} // “自动”的 GBK 编码
	body := append([]byte(`{"note":"`), gbk...)
	body = append(body, []byte(`"}`)...)
	if bytes.Contains(body, []byte("\uFFFD")) {
		t.Fatal("构造的请求体里出现了替换符，说明测试数据本身已不是原始字节")
	}

	for _, e := range jsonEntries() {
		t.Run(e.name, func(t *testing.T) {
			rec := doEntry(e, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("非 UTF-8 应为 400，实际 %d（body=%s）", rec.Code, truncate(rec.Body.String()))
			}
			if msg := decodeError(t, rec); msg != msgPayloadNotUTF8 {
				t.Errorf("400 文案应为 %q，实际 %q", msgPayloadNotUTF8, msg)
			}
		})
	}
}

// TestReadBodyAcceptsValidUTF8 是上面两条的反面：合法 UTF-8 不能被误杀。
//
// 少了这条，「一律 400」也能让上面两条全绿 —— 判据必须两侧都有。
func TestReadBodyAcceptsValidUTF8(t *testing.T) {
	body := []byte(`{"note":"自动扩容已触发"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))

	got, ok := readBody(rec, req, 1<<20, discardLogger())
	if !ok {
		t.Fatalf("合法 UTF-8 被拒：%d %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(got, body) {
		t.Errorf("读出的字节与写入不一致")
	}
}

// TestFirstInvalidUTF8 覆盖偏移计算：日志里要报"从哪一字节开始坏"，
// 报错了等于没报。
func TestFirstInvalidUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"全合法", []byte("abc自动"), -1},
		{"空体", nil, -1},
		{"首字节就坏", []byte{0xd7, 0xd4}, 0},
		{"坏在中间", append([]byte("ab"), 0xd7, 0xd4), 2},
		{"末尾半截多字节", append([]byte("ab"), 0xe4, 0xb8), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstInvalidUTF8(c.in); got != c.want {
				t.Errorf("偏移=%d，期望 %d", got, c.want)
			}
		})
	}
}

func truncate(s string) string {
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// 编译期确认替身确实实现了 handler 需要的接口（改了签名这里会红）。
var _ IncidentCommenter = (*fakeIncidentCommenter)(nil)
