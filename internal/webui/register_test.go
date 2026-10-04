package webui

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRouter 记录 NotFound 注册，用于验「注册了什么、注册了几次」。
type fakeRouter struct {
	handlers []http.HandlerFunc
}

func (f *fakeRouter) NotFound(h http.HandlerFunc) { f.handlers = append(f.handlers, h) }

// TestRegisterEmptyRootRegistersNothing 是 Register 的核心契约：
// webRoot 为空时**一次都不注册**。
//
// 为什么重要：注册了「总是 404 的 handler」与「不注册」在外部表现上
// 很像（都是 404），但后者不影响任何既有路由的行为 ——
// 若注册了，将来某个未命中 /api 的请求会走进静态层拿 index.html，
// 那就正是本包要防的故障。
func TestRegisterEmptyRootRegistersNothing(t *testing.T) {
	for _, root := range []string{"", "  ", "\t\n"} {
		f := &fakeRouter{}
		Register(f, root, nil)
		if len(f.handlers) != 0 {
			t.Errorf("Register(root=%q) 注册了 %d 个 handler，期望 0（未配置时不该介入）",
				root, len(f.handlers))
		}
	}
}

func TestRegisterNonEmptyRootRegistersOnce(t *testing.T) {
	f := &fakeRouter{}
	Register(f, newTestRoot(t), nil)
	if len(f.handlers) != 1 {
		t.Fatalf("注册了 %d 个 handler，期望恰好 1 个", len(f.handlers))
	}
	if f.handlers[0] == nil {
		t.Fatal("注册的 handler 为 nil")
	}
}

// TestRegisterKeepsAPIAheadOfStaticFallback 验那条最关键的性质：
// **注册静态托管后，已注册的 /api 路由仍优先命中**。
//
// 判据三重，缺一不可：
//  1. /api 命中 API handler（不是 index.html）
//  2. 未命中的 /api 路径回落到 JSON 404（不是 index.html）
//  3. 未命中的非 /api 路径回落到 index.html（SPA 回退仍生效）
//
// 只验第 1 条不够 —— 某天有人把静态层改成通配路由，
// /api 仍能命中已注册的那条，但**没注册的那个**会被吞成 HTML。
func TestRegisterKeepsAPIAheadOfStaticFallback(t *testing.T) {
	r := chiRouter()
	// API 路由先注册
	r.Get("/api/v1/incidents", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"incidents":[],"count":0}`)
	})
	// 静态托管**最后**注册（与 main.go 的顺序一致）
	Register(r, newTestRoot(t), nil)

	t.Run("已注册 API 命中自己的 handler", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents", http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d，期望 200", rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type = %q，期望 JSON", rec.Header().Get("Content-Type"))
		}
		if !strings.Contains(rec.Body.String(), `"incidents"`) {
			t.Errorf("body 不含 API 数据，前 100: %q", truncate(rec.Body.String(), 100))
		}
		if strings.Contains(rec.Body.String(), "id=\"root\"") {
			t.Error("API 请求被静态层吞掉了（返回了 index.html）")
		}
	})

	t.Run("未注册的 /api 路径回落 JSON 404 而非 index.html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", http.NoBody))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("= %d，期望 404", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<html") {
			t.Error("未注册 API 返回了 HTML —— 前端会报 JSON 解析错误而问题在后端")
		}
	})

	t.Run("非 API 的未命中路径仍走 SPA 回退", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/incidents/12", http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d，期望 200（SPA 回退失效）", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `id="root"`) {
			t.Error("未回退到 index.html")
		}
	})
}

// TestRegisterLogsOnlyOnce 验日志不重复刷。
//
// 装配代码每跑一次就写一条日志，而它只在进程启动时跑一次 ——
// 但这条断言防的是「有人把 Register 挪进请求处理路径」那种改法，
// 那种改法会让每次请求都打一条日志，把日志淹掉。
func TestRegisterLogsOnlyOnce(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))

	Register(&fakeRouter{}, "", logger)
	if got := strings.Count(sb.String(), "\n"); got != 1 {
		t.Errorf("空 root 应恰好 1 条日志，实际 %d 条: %q", got, sb.String())
	}
	if !strings.Contains(sb.String(), "未启用前端静态托管") {
		t.Errorf("日志文案未说明未启用。实际: %q", sb.String())
	}

	sb.Reset()
	Register(&fakeRouter{}, newTestRoot(t), logger)
	if got := strings.Count(sb.String(), "\n"); got != 1 {
		t.Errorf("非空 root 应恰好 1 条日志，实际 %d 条: %q", got, sb.String())
	}
	if !strings.Contains(sb.String(), "已启用前端静态托管") {
		t.Errorf("日志文案未说明已启用。实际: %q", sb.String())
	}
	// 必须带出 web_root —— 部署出问题时这一行就是唯一线索
	if !strings.Contains(sb.String(), "web_root") {
		t.Errorf("启用日志缺 web_root 字段。实际: %q", sb.String())
	}
}
