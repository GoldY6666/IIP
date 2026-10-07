package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yu/iip/internal/middleware"
)

// authSessionBody 是解出来的原始 map，用于断言**键名契约**。
//
// 刻意不用 struct 直接反序列化：缺 json tag 时 Go 会退回字段名当键，
// 那样 struct 版断言会自洽通过而前端拿到的是 "AuthRequired"。
// 解成 map 逐键比对才是真的在验 JSON 契约。
func authSessionBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, 原文=%s", err, rec.Body.String())
	}
	return m
}

// sessionHandler 跑一次 AuthSession，返回 recorder。
func sessionHandler(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, AuthSessionPath(), http.NoBody)
	AuthSession(token, discardLogger())(rec, req)
	return rec
}

// S4-01：未配置 token 时返回 200 + auth_required=false + token 空串。
//
// 判据要点是**200 而不是 401/403**：「本机未启用鉴权」是合法状态，
// 前端据此正常走无凭据路径。若实现改用 401 表达，前端会把它当成
// 「token 错了」而弹出误导性的提示。
func TestAuthSessionNotEnabledL01(t *testing.T) {
	for _, token := range []string{"", "   ", "\t\n "} {
		rec := sessionHandler(t, token)
		if rec.Code != http.StatusOK {
			t.Errorf("未配 token 应返回 200（配置=%q），实得 %d", token, rec.Code)
		}
		m := authSessionBody(t, rec)
		if got, ok := m["auth_required"].(bool); !ok || got {
			t.Errorf("配置=%q 时 auth_required 应为 false，实得 %v", token, m["auth_required"])
		}
		if got, ok := m["token"].(string); !ok || got != "" {
			t.Errorf("配置=%q 时 token 应为空串，实得 %v", token, m["token"])
		}
	}
}

// S4-02：键名契约 —— 恰好是 auth_required 与 token 两个键。
//
// 用「恰好含有」而不是逐个断言存在：多出的键也要失败。
// 若实现改用 PascalCase 键（缺 json tag），这条会红而 struct 版不会。
func TestAuthSessionKeyContractL02(t *testing.T) {
	for _, token := range []string{"", "s3cr3t"} {
		m := authSessionBody(t, sessionHandler(t, token))
		if len(m) != 2 {
			t.Errorf("响应体应恰好含 2 个键（配置=%q），实得 %d 个: %v", token, len(m), m)
		}
		for _, k := range []string{"auth_required", "token"} {
			if _, ok := m[k]; !ok {
				t.Errorf("配置=%q 时响应体缺少键 %q，实得键集合: %v", token, k, m)
			}
		}
	}
}

// S4-03：配置了 token 时回传该 token 且 auth_required=true。
//
// 这条是本端点存在的意义 —— 前端靠它才知道要给后续请求加什么头。
func TestAuthSessionEnabledL03(t *testing.T) {
	rec := sessionHandler(t, "s3cr3t")
	if rec.Code != http.StatusOK {
		t.Fatalf("已配 token 时应返回 200，实得 %d", rec.Code)
	}
	m := authSessionBody(t, rec)
	if got, ok := m["auth_required"].(bool); !ok || !got {
		t.Errorf("已配 token 时 auth_required 应为 true，实得 %v", m["auth_required"])
	}
	if got, ok := m["token"].(string); !ok || got != "s3cr3t" {
		t.Errorf("已配 token 时 token 应回传 s3cr3t，实得 %v", m["token"])
	}
}

// S4-04：配置值带首尾空白时，回传的必须是 **TrimSpace 后的值**。
//
// 这条锁住本批修掉的一个自相矛盾：中间件比的是 TrimSpace 后的值，
// 若这里回传原始的 "s3cr3t "，前端就会带一个过不了校验的头，
// 症状是「配了 token 但怎么都登录不上」。
func TestAuthSessionTrimsConfiguredValueL04(t *testing.T) {
	m := authSessionBody(t, sessionHandler(t, "  s3cr3t  "))
	got, ok := m["token"].(string)
	if !ok {
		t.Fatalf("token 键不是字符串: %v", m["token"])
	}
	if got != "s3cr3t" {
		t.Errorf("配置值首尾空白应被去掉后回传，实得 %q", got)
	}
	// 前置条件断言：回传值必须真能通过中间件，否则「加了头也 401」。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
	req.Header.Set(middleware.AuthHeader, got)
	called := false
	middleware.RequireToken("  s3cr3t  ")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})).ServeHTTP(rec, req)
	if !called {
		t.Errorf("回传的 token %q 应能通过中间件校验，实被拒（%d）", got, rec.Code)
	}
}

// S4-05：本端点报告的「是否启用」与中间件的放行口径必须一致。
//
// 这是本批最容易被后续修改打破的不变量：两处各写一次判定，
// 只要其中一处忘了 TrimSpace，就会出现「日志说已启用、会话说要token、
// 中间件却放行」的三方矛盾。逐个配置值对照两种口径。
func TestAuthSessionAgreesWithMiddlewareL05(t *testing.T) {
	for _, token := range []string{"", " ", "\t", "s3cr3t", " s3cr3t ", "   x   "} {
		// 端点口径
		m := authSessionBody(t, sessionHandler(t, token))
		endpointSaysEnabled, _ := m["auth_required"].(bool)

		// 中间件口径：没放行即视为启用
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
		called := false
		middleware.RequireToken(token)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		})).ServeHTTP(rec, req)
		middlewareAllows := called

		if endpointSaysEnabled == middlewareAllows {
			t.Errorf("配置=%q 两侧口径矛盾：端点称启用=%v，中间件放行=%v",
				token, endpointSaysEnabled, middlewareAllows)
		}
	}
}

// S4-06：豁免路径必须是本端点自己，且只有一个。
//
// 中间件的豁免列表是「路径前缀 + 特例」，其中特例写错一个字
// （例如放行了 /api/v1/auth/ 而非 /api/v1/auth/session）
// 就会让 /api/v1/auth/session 变成 404 —— 而前端此时正需要它，
// 症状是「页面全白且只有这一个接口报错」，极难定位。
func TestAuthSessionPathIsExemptedL06(t *testing.T) {
	p := AuthSessionPath()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, p, http.NoBody)
	called := false
	middleware.RequireToken("s3cr3t")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})).ServeHTTP(rec, req)
	if !called {
		t.Errorf("%s 必须被中间件豁免（前端靠它拿 token），实被拦（%d）", p, rec.Code)
	}
	if p != "/api/v1/auth/session" {
		t.Errorf("豁免路径写成了 %q；与前端约定的 /api/v1/auth/session 不一致", p)
	}
}
