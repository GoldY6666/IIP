package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/yu/iip/internal/middleware"
)

// authSessionResponse 是 GET /api/v1/auth/session 的响应体。
//
// 字段名用小驼峰：它们要原样出现在 JSON 里给前端读，
// 用 Go 惯例的 PascalCase 会让前端拿到 "AuthRequired"/"Token"
// 这种与其它端点不一致的键 —— 而前端只认 snake_case 的其它端点
// 已经让这个问题付出过代价（见 response_contract_test.go 的说明）。
type authSessionResponse struct {
	// AuthRequired 为 true 表示服务端配置了 token，
	// 前端必须带上 X-IIP-Token 才能访问其它接口。
	AuthRequired bool `json:"auth_required"`
	// Token 是配置里的 token 本身。**只在鉴权已启用时非空。**
	//
	// 为什么把它回给浏览器：静态 token 的持有者本来就知道它，
	// 而前端需要在页面初始化时拿到它才能给后续请求加头。
	// 若鉴权未启用，这里是空串 —— 前端据此知道「不需要加头」。
	//
	// 它出现在响应体里就意味着**明文**。这是本方案「不做登录页」
	// 的直接后果（手册 §八裁定 #1）：任何能打开 UI 的人都能拿到它。
	// 因此该端点本身**不要求 token**（否则启用鉴权后前端拿不到 token，
	// 形成死锁），这正是「保护 /api/v1/*」这条规则里唯一的例外。
	Token string `json:"token"`
}

// AuthSession 返回 GET /api/v1/auth/session 的 handler（M6 S4）。
//
// 该端点**刻意不受鉴权保护**（其余 /api/v1/* 都受）：
// 它是前端获取 token 的唯一途径，若也要求 token，
// 启用鉴权后前端就永远拿不到 token —— 死锁且症状是「页面全白」。
//
// 语义与其余端点相反：它用 200 + auth_required=false 表达
// 「本机未启用鉴权」，而不是 401/403。因为「未启用鉴权」不是错误状态，
// 前端要据此正常走无凭据路径。
func AuthSession(token string, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	// 判「是否启用」必须与中间件用同一把尺子（middleware.AuthEnabled）：
	// 那里对配置值做了 TrimSpace（`IIP_AUTH_TOKEN=abc ` 这种手抄值要能正常
	// 工作）。若这里只判 `token != ""`，配置成纯空白时中间件会放行而本端点
	// 回答 auth_required=true，前端于是给每个请求加上一个必然被拒的头
	// —— 症状是「明明没配 token 却全站 401」，且极难自查。
	want := strings.TrimSpace(token)
	enabled := middleware.AuthEnabled(token)
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, logger, http.StatusOK, authSessionResponse{
			AuthRequired: enabled,
			// 回传归一化后的值：中间件比的是 TrimSpace 后的值，
			// 回传原始值会让前端带一个过不了校验的头。
			Token: want,
		})
	}
}

// authSessionPath 是该端点的路径，注册处与文档共用一个常量，
// 避免「路由改了而文档/前端还写着旧路径」。
const authSessionPath = "/api/v1/auth/session"

// AuthSessionPath 供装配层注册路由用。
//
// 单独导出而不是让装配层写字面量：字面量写两遍（这里与装配处）
// 就是两份配置而不是一个契约，改一处漏一处的症状是
// 「前端 404 而后端日志里根本没这个请求」。
func AuthSessionPath() string { return authSessionPath }
