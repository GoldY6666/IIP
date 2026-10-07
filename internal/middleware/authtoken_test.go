package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler 是一个必然返回 204 的下游处理器。
//
// 用 204 而不是 200：断言「下游有没有被调到」时，204 + 空体
// 让「被拦」与「放行但下游没干活」这两件事不会混成同一个现象。
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

// TestRequireToken_EmptyTokenMeansOpen 不设 token 时必须完全放行。
//
// 这是「本地开发零改动可用」这条产品承诺的守卫。若将来有人把
// 「空 token」改成「空 token = 拒绝所有」，本条会红。
func TestRequireToken_EmptyTokenMeansOpen(t *testing.T) {
	h := RequireToken("")(okHandler())

	// 三种请求都该放行：不带头、带空头、带任意值。
	// 只测「不带头」不够 —— 带空值与带错值走的分支可能不同。
	cases := []struct {
		name   string
		header string
	}{
		{"不带头", ""},
		{"带空值", "   "},
		{"带任意值", "whatever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
			if tc.header != "" {
				req.Header.Set(AuthHeader, tc.header)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Errorf("未配置 token 时应放行（204），实得 %d", rec.Code)
			}
		})
	}
}

// TestRequireToken_CorrectTokenPasses 带上正确 token 时放行。
func TestRequireToken_CorrectTokenPasses(t *testing.T) {
	h := RequireToken("s3cr3t")(okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	req.Header.Set(AuthHeader, "s3cr3t")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("正确 token 应放行（204），实得 %d", rec.Code)
	}
}

// TestRequireToken_Rejects 拒绝缺头/ 空值 / 错值三种，并断言下游**没被调用**。
//
// 「assert 状态码 401」是不够的：中间件可以先调下游再覆盖状态码，
// 那样业务副作用已经发生（虽然本项目全是读端点，但一旦加写端点
// 就会变成真实漏洞）。故用一个会记账的 handler 来验「确实没进去」。
func TestRequireToken_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		header string
		set    bool
	}{
		{"缺头", "", false},
		{"空值", "", true},
		{"空白值", "   ", true},
		{"错值", "wrong-token", true},
		{"前缀相同但不等", "s3cr3", true},   // 少写一段
		{"尾部多一个空格", "s3cr3t ", true}, // 去空格会凭空放宽字符集
		{"大小写不同", "S3CR3T", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := RequireToken("s3cr3t")(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
			if tc.set {
				req.Header.Set(AuthHeader, tc.header)
			}
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("应返回 401，实得 %d", rec.Code)
			}
			if called {
				t.Error("下游被调用了：鉴权必须在业务逻辑之前拦住，否则带副作用的端点会成为真实漏洞")
			}
		})
	}
}

// TestRequireToken_ResponseContract 锁定 401 响应体形状。
//
// **为什么必须锁**：前端对所有 API 错误走同一条解析路径。
// 若这里返回的形状与 internal/api 的 errorBody 不同，「token 错了」
// 会表现为「页面崩了」而不是「请检查 token」—— 那是最难排查的一类症状。
//
// 用「恰好含有」而非「至少」：多出来的键同样会暴露给前端。
func TestRequireToken_ResponseContract(t *testing.T) {
	h := RequireToken("s3cr3t")(okHandler())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("401 的 Content-Type = %q，应为 application/json —— "+
			"前端会 JSON.parse，拿到 HTML 时报「Unexpected token '<'」", ct)
	}

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("读 401 响应体失败: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("401 响应体不是合法 JSON: %v；原文=%s", err, body)
	}

	for _, k := range []string{"error", "code"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("401 响应体缺少键 %q；实得键=%v", k, keysOf(obj))
		}
	}
	if len(obj) != 2 {
		t.Errorf("401 响应体应恰好含 error/code 两个键，实得 %v", keysOf(obj))
	}
	if got := obj["code"]; got != authCodeUnauthorized {
		t.Errorf("code = %v，期望 %q（前端按它分支处理，不能让前端去匹配中文文案）",
			got, authCodeUnauthorized)
	}
}

// TestRequireToken_ResponseDoesNotLeakToken 401 响应里不得出现 token。
//
// 401 响应常被反向代理与客户端日志记录，回显凭据等于再泄一次。
// 这是回归防护：不验「当前没泄」，而是给将来加日志/回显的人设闸。
func TestRequireToken_ResponseDoesNotLeakToken(t *testing.T) {
	const secret = "tok-leak-canary-7c1f"
	h := RequireToken(secret)(okHandler())

	for _, hdr := range []string{"", "wrong"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
		if hdr != "" {
			req.Header.Set(AuthHeader, hdr)
		}
		h.ServeHTTP(rec, req)

		body, _ := io.ReadAll(rec.Body)
		if strings.Contains(string(body), secret) {
			t.Errorf("401 响应体泄露了服务端 token（请求头=%q）: %s", hdr, body)
		}
	}
}

// TestAuthHeaderIsNotAuthorization 锁住请求头名的选择。
//
// 这条是**决策固化**：曾考虑用标准的 Authorization: Bearer，
// 但那在 devtools/代理/中间件日志里更常被自动记录与转发。
// 若将来有人「规范化」成 Bearer，本条会红并提醒他重读该理由。
func TestAuthHeaderIsNotAuthorization(t *testing.T) {
	if AuthHeader == "Authorization" {
		t.Error("AuthHeader 被改成了 Authorization —— 见常量处的理由：" +
			"该头在浏览器 devtools 与代理日志里更常被自动记录与转发")
	}
	if !strings.EqualFold(AuthHeader, "X-IIP-Token") {
		t.Errorf("AuthHeader = %q，与前端约定的字面量不一致（两侧必须逐字一致）", AuthHeader)
	}
}

// TestRequireToken_ProtectsOnlyAPIV1 锁定受保护路径的边界。
//
// 核心是 **/api/v1x 必须放行**：裸 HasPrefix("/api/v1") 会把它
// 也保护起来，而那属于「靠不存在的东西保证安全」——
// 将来有人加了 /api/v1internal 这类路由就会静默被拦，
// 症状是「接口 401 但路径看起来对」，极难定位。
//
// 另外三条守住真实需求：/healthz 必须放行（否则容器 healthcheck
// 判定服务不健康，表现为「服务起不来」而真因是鉴权），
// 以及 / 前缀本身要放行（静态托管首页）。
func TestRequireToken_ProtectsOnlyAPIV1(t *testing.T) {
	protected := []string{
		"/api/v1",
		"/api/v1/",
		"/api/v1/incidents",
		"/api/v1/incidents/42/evidence",
		"/api/v1/auth/sessions", // 只豁免精确路径，多个 s 不豁免
	}
	for _, p := range protected {
		t.Run("受保护:"+p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, p, nil)
			RequireToken("s3cr3t")(okHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s 应受保护（无 token 时 401），实得 %d", p, rec.Code)
			}
		})
	}

	open := []string{
		"/healthz",              // 容器 healthcheck
		"/",                     // 静态托管首页
		"/index.html",           // 静态托管
		"/assets/index.css",     // 静态资源
		"/api/v1/auth/session",  // 唯一豁免：前端靠它拿 token
		"/api/v1/auth/session/", // 同上（容忍尾部斜杠）
		"/api/v1x",              // 不是 /api/v1
		"/api/v1internal/x",     // 同上，更逼真的假设
		"/api/v2/incidents",     // 不存在的版本
		"/apiv1/incidents",      // 少一个斜杠
	}
	for _, p := range open {
		t.Run("放行:"+p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, p, nil)
			RequireToken("s3cr3t")(okHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Errorf("%s 不应受保护，应放行（204）；实得 %d —— "+
					"放行失败意味着 %s 会被鉴权拦下", p, rec.Code, p)
			}
		})
	}
}

// TestRequireToken_AllMethods 用多种 HTTP 方法重跑一遍。
//
// 只测 GET 是不够的：若将来实现里误用了 r.Method 判断
// （例如「只保护 GET」），写端点就会裸奔。
func TestRequireToken_AllMethods(t *testing.T) {
	methods := []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodPatch,
	}
	h := RequireToken("s3cr3t")(okHandler())
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(m, "/api/v1/incidents/1/transition", nil)
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s 无 token 时应 401，实得 %d", m, rec.Code)
			}
		})
	}
}

// TestRequireToken_TrimsConfiguredTokenNotHeader 锁定一个**不对称**的取舍：
// 配置侧去空格、请求头侧不去。
//
// 两边都去是错的：env 里手抄常带尾随空格，而 HTTP 库不会替我们去掉
// 请求头里的空格，两边都不去会让「配了 token 但怎么都过不了」。
// 但**请求头也去**同样是错的：它让 "s3cr3t "等价于 "s3cr3t"，
// 凭空放宽了 token 的字符集。
//
// 这条测试把两个方向分别钉住：配置带空格 → 仍能匹配正确请求头；
// 请求头带空格 → 被拒。
func TestRequireToken_TrimsConfiguredTokenNotHeader(t *testing.T) {
	h := RequireToken("  s3cr3t  ")(okHandler()) // 配置值两侧有空格

	// 配置的空格被去掉 → 干净的请求头应通过
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	req.Header.Set(AuthHeader, "s3cr3t")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("配置值的首尾空格应被忽略，干净的请求头应放行；实得 %d", rec.Code)
	}

	// 请求头的空格**不**被忽略 → 带空格应被拒
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
	req2.Header.Set(AuthHeader, "s3cr3t ")
	h2 := RequireToken("s3cr3t")(okHandler())
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("请求头尾部空格不应被忽略（否则凭空放宽字符集），实得 %d", rec2.Code)
	}
}

// keysOf 返回 map 的键列表（测试失败时用来报「实得哪些键」）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
