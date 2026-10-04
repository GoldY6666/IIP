package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandlerNilWhenRootEmpty(t *testing.T) {
	// 空 root 必须返回 nil 而不是「总是 404 的 handler」：
	// 前者让「忘配 IIP_WEB_ROOT」在装配处显形（一次都不注册），
	// 后者表现为「UI 全 404」，与「路径写错」无法区分。
	for _, root := range []string{"", "   ", "\t"} {
		if h := Handler(root, UIConfig{}); h != nil {
			t.Errorf("Handler(%q) 非 nil，期望 nil（未配置时不应启用静态托管）", root)
		}
	}
}

func TestHandlerServesIndexAtRoot(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})
	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q，期望 text/html", ct)
	}
	// index.html 绝不能长缓存：它引用带 hash 的产物文件名，
	// 缓存住会让用户永远加载旧版本，且**没有任何报错**。
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index.html 的 Cache-Control = %q，期望 no-cache", cc)
	}
}

func TestHandlerServesAssetsWithImmutable(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})
	rec := do(t, h, http.MethodGet, "/assets/index-aaa111.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/index-aaa111.js = %d，期望 200", rec.Code)
	}
	// vite 产物名带内容 hash，可永久缓存。
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q，期望 public, max-age=31536000, immutable", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q，期望 text/javascript; charset=utf-8", ct)
	}
}

func TestHandlerContentTypeByExtension(t *testing.T) {
	root := newTestRoot(t)
	h := Handler(root, UIConfig{})

	cases := []struct {
		file string
		want string
	}{
		{"app.js", "text/javascript; charset=utf-8"},
		{"app.css", "text/css; charset=utf-8"},
		{"app.json", "application/json; charset=utf-8"},
		{"favicon.svg", "image/svg+xml"},
		{"a.png", "image/png"},
		{"a.woff2", "font/woff2"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			write(t, root+"/"+tc.file, "x")
			rec := do(t, h, http.MethodGet, "/"+tc.file)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /%s = %d，期望 200", tc.file, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestHandlerSPAFallback 验 SPA 回退：前端用 react-router，
// /incidents/12 是路由不是文件，刷新必须能打开。
func TestHandlerSPAFallback(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})

	for _, p := range []string{
		"/incidents",
		"/incidents/12",
		"/reviews",
		"/alerts",
		"/settings", // M5 F4 会真做这个路由
		// 深链接带 query：回退必须只看 URL.Path，
		// 把 query 混进文件路径会 404。
		"/incidents/12?tab=evidence&sort=desc",
	} {
		t.Run(p, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, p)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d，期望 200（深链接刷新必须能打开）", p, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `id="root"`) {
				t.Errorf("未回退到 index.html。body 前 120: %q",
					truncate(rec.Body.String(), 120))
			}
		})
	}
}

// TestHandlerDoesNotSwallowAPI 是手册 §7.2 点名的反向验证：
// 静态托管若把 /api 吞掉，调用方拿到 index.html 而不是 JSON。
func TestHandlerDoesNotSwallowAPI(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})

	// 走到本 handler 的 /api 请求，**必须**是 JSON 404 而不是 index.html。
	// （正常情况下 /api/* 已被上层路由处理，走不到这里；
	//  这里验的是「万一走到这里」时不会伪装成功。）
	rec := do(t, h, http.MethodGet, "/api/v1/not-registered")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/not-registered = %d，期望 404", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 application/json（返回 HTML 会让前端 JSON.parse 崩）", ct)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("未注册的 API 路径返回了 HTML。前端会报 JSON 解析错误，"+
			"而问题在后端路由。body 前 120: %q", truncate(rec.Body.String(), 120))
	}
}

func TestHandlerRejectsNonGET(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodPatch, http.MethodOptions} {
		rec := do(t, h, m, "/incidents/12")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /incidents/12 = %d，期望 405", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("Allow = %q，期望 \"GET, HEAD\"", allow)
		}
	}
}

func TestHandlerHeadHasNoBody(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})
	rec := do(t, h, http.MethodHead, "/assets/index-aaa111.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d，期望 200", rec.Code)
	}
	if n := rec.Body.Len(); n != 0 {
		t.Errorf("HEAD 返回了 %d 字节 body，期望 0", n)
	}
}

// TestIndexAlwaysGetsUIConfigInjected 锁住一个真实缺陷：
//
// 根路径 `/` 走的是「目录 + 尾斜杠」分支，若那里取 index.html 时
// 调 serveFile 而非 serveIndex，注入就被跳过 —— 症状是
// 「UI 能打开，但所有链接都是默认值」且**没有任何报错**。
//
// 这条测试的价值在于它是**反向**的：它验「每一条通往 index.html 的路
// 都要注入」，而不只是验 serveIndex 自己会注入。
func TestIndexAlwaysGetsUIConfigInjected(t *testing.T) {
	root := newTestRoot(t)
	const want = "http://injected.prom:9090"
	h := Handler(root, UIConfig{Prometheus: want})
	if h == nil {
		t.Fatal("Handler 返回 nil")
	}

	// 三条路径都必须带注入：根路径、深链接回退、显式 /index.html
	for _, p := range []string{"/", "/incidents/12", "/index.html"} {
		t.Run(p, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, p)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d，期望 200", p, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, uiConfigGlobalName) {
				t.Errorf("GET %s 的响应未注入 UI 配置。body 前 200: %q",
					p, truncate(body, 200))
			}
			if !strings.Contains(body, want) {
				t.Errorf("GET %s 未带注入的地址 %q", p, want)
			}
			// 占位符必须被替换掉，不能与注入并存
			if strings.Contains(body, uiConfigScriptMarker) {
				t.Errorf("GET %s 的响应里占位符还在（注入没生效）", p)
			}
		})
	}

	// 反向：非 index.html 的产物**不该**被注入。
	// 给 assets/*.js 注入配置纯属浪费字节，且会让「assets 是否被注入」
	// 这条断言失去鉴别力（两个都注入时它恒为真）。
	t.Run("assets 不被注入", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/assets/index-aaa111.js")
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d，期望 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), uiConfigGlobalName) {
			t.Error("assets 文件被注入了 UI 配置（只该注入 HTML）")
		}
	})
}

func TestHandlerDirectoryRedirects(t *testing.T) {
	h := Handler(newTestRoot(t), UIConfig{})
	// 目录缺尾斜杠时 301：浏览器会自动补并再请求 index.html。
	// 少了这个 301，GET /assets 会 404，表现为「产物里明明有 assets」。
	rec := do(t, h, http.MethodGet, "/assets")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /assets = %d，期望 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/assets/" {
		t.Errorf("Location = %q，期望 \"/assets/\"", loc)
	}
}

// TestHandlerIncompleteDist 验产物不完整时的报错可读性。
func TestHandlerIncompleteDist(t *testing.T) {
	// 只有一个 assets 目录、**没有** index.html —— 模拟「只拷了 dist/assets」。
	root := t.TempDir()
	mkdirAll(t, root+"/assets")
	write(t, root+"/assets/app.js", "x")
	h := Handler(root, UIConfig{})

	rec := do(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET / = %d，期望 500", rec.Code)
	}
	// 错误信息必须指向「去 build」这个动作，
	// 否则用户看到 500 会以为产品坏了。
	if !strings.Contains(rec.Body.String(), "pnpm run build") {
		t.Errorf("错误信息未提示构建动作。body: %q", truncate(rec.Body.String(), 200))
	}
}
