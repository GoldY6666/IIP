package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
)

// chiRouter 造一个**真实 chi** 路由。
//
// 只有注册顺序相关的测试需要它：chi 的 NotFound 语义、
// 路由匹配顺序都无法用替身代替（手记 §7.2 点的「只靠 handler 层
// 替身验不了装配链路」）。生产代码不 import chi，测试里 import 是可接受的 ——
// 它验的正是「本包的 Router 接口与真框架兼容」。
func chiRouter() *chi.Mux {
	return chi.NewRouter()
}

// newTestRoot 造一个最小可用的 dist 目录。
//
// 刻意包含 assets/ 与一个嵌套深链接路径：真实 vite 产物
// 是 index.html + assets/*.js，缺任一份都会让某些断言假通过。
//
// index.html 里**带 UI 配置注入占位符**，与 web/index.html 和真实
// dist 产物一致。写成不带会让「注入是否发生」的所有断言恒为假 ——
// 那种 fixture 造出来的绿是没有意义的绿。
func newTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "assets"))
	write(t, filepath.Join(root, indexFile),
		`<!doctype html><html><head><title>IIP</title>`+uiConfigScriptMarker+
			`</head><body><div id="root"></div></body></html>`)
	write(t, filepath.Join(root, "assets", "index-aaa111.js"), `console.log(1)`)
	write(t, filepath.Join(root, "assets", "index-aaa111.css"), `:root{}`)
	write(t, filepath.Join(root, "favicon.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	return root
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) 失败: %v", p, err)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) 失败: %v", p, err)
	}
}

// do 对 handler 发一次请求，返回响应。
//
// 路径故意带 query：SPA 回退必须只按 URL.Path 判断，
// 带 query 的请求若把 query 混进文件路径，会 404。
func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doRaw 用手工构造的 Request 发请求，**绕过 httptest.NewRequest 的 URI 校验**。
//
// 存在的理由：反斜杠（\..\SECRET.txt）在 URL 里是**非法**的，
// httptest.NewRequest 会直接 panic（"invalid URI for request"）。
// 但恰恰是这种非法输入最需要测 —— 真实服务器上它可能以
// 各种形式抵达 handler，而我们不能因为「构造请求的工具嫌它非法」
// 就不验证 handler 对它的处理。
//
// 这里刻意**不做 url.Parse**：它会把非法字符原样保留但也可能报错，
// 直接 &url.URL{Path: p} 最直接 —— 就是要「Path 里就是有反斜杠」这个事实。
func doRaw(t *testing.T, h http.Handler, rawPath string) *httptest.ResponseRecorder {
	t.Helper()
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: rawPath},
		Header: make(http.Header),
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
