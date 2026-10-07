// Package middleware 存放 HTTP 中间件。
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.4。
package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// accessLog 记录每个请求的访问日志（结构化 JSON）。
//
// 使用 chi 的 RequestID 中间件写入的 request id 关联本次请求。
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			logger.Info("http_request",
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// AuthHeader 是客户端携带 token 的请求头名。
//
// 刻意**不用** `Authorization: Bearer`：那条头在浏览器 devtools、
// 反向代理与中间件日志里更常被自动记录与转发，而本项目部署形态是
// 内网裸跑、不经过统一网关 —— 少一处自动泄漏就少一处。
const AuthHeader = "X-IIP-Token"

// authCodeUnauthorized 是 401 响应的错误码。
//
// 前端按 code 分支处理（与 internal/api 的既有错误码同一套约定）；
// 没有它前端只能匹配中文文案 —— 而文案是要给人看的，不该被当成协议。
const authCodeUnauthorized = "unauthorized"

// AuthEnabled 报告「是否启用了鉴权」——**唯一的判定口径**。
//
// 单独导出而不是让各处各写 `token != ""`：本批有三处需要这个判断
// （中间件放行、启动日志、/auth/session 的 auth_required），
// 而 TrimSpace 一旦只在其中一处生效，就会出现
// 「配了空白token → 日志说已启用、会话端点也说已启用，中间件却放行」
// 这种自相矛盾状态，症状是「明明没配却全站 401」且无从查起。
//
// 规则很简单：去掉首尾空白后非空即为启用。空串与纯空白都算「没配」。
func AuthEnabled(token string) bool {
	return strings.TrimSpace(token) != ""
}

// RequireToken 校验请求头里的静态 token（M6 S4）。
//
// **空 token = 不开鉴权**（直接放行）。这不是「忘了配置」的兜底，
// 而是刻意设计：本地开发与 `docker compose --profile app up` 的
// 现有用法必须零改动可用，判定「是不是漏配」需要猜用户意图 ——
// 猜错的后果要么是 UI 打不开，要么是部署到内网却以为有鉴权。
//
// **只挂在 /api/v1/* 上**，不挂 /healthz：否则容器的 healthcheck 与
// 外部探针都会因缺 token 判定服务不健康（compose 的 healthcheck.test
// 不可能带自定义头），表现为「服务起不来」而真因是鉴权配置。
//
// 比较用 crypto/subtle.ConstantTimeCompare：朴素字符串比较会
// 在返回第一个不同字节处耗时，理论上可被逐字节爆破。
func RequireToken(token string) func(http.Handler) http.Handler {
	// 配置值做TrimSpace：env 里手抄常带尾随空格（`IIP_AUTH_TOKEN=abc `），
	// 而 HTTP 库不会替我们去掉请求头里的空格。若两侧都不去空格，
	// 症状是「配了 token 但前端怎么都过不了」——极难排查。
	want := strings.TrimSpace(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want == "" {
				next.ServeHTTP(w, r)
				return
			}
			// 只保护 /api/v1/*：/healthz 与静态托管必须放行，
			// 否则容器的 healthcheck.test（不可能带自定义头）
			// 会判定服务不健康，症状是「服务起不来」而真因是鉴权。
			// 前缀判断放在这里而不是靠 chi 的路由分组，是为了不必改
			// 十几处既有 r.Get("/api/v1/...") 声明（回归面最小）。
			if !underProtectedPrefix(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// /api/v1/auth/session 是唯一豁免：前端要靠它拿到 token，
			// 若也要求 token 则启用鉴权后前端永远拿不到 —— 死锁，
			// 且症状是「页面全白」而非任何可读的错误。
			if isAuthSessionPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			// 请求头**原样**比较，不 TrimSpace：
			// 去空格会让「s3cr3t 」与「s3cr3t」等价，
			// 那等于凭空放宽了 token 的字符集 —— 与 TrimSpace 的目的正相反。
			got := r.Header.Get(AuthHeader)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				// 刻意**不**在这里打日志：AccessLog 已记录了
				// method/path/status，足以定位是谁被拦了；而任何
				// 带 token 值的日志都是新的泄漏面。
				writeUnauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// protectedPrefix 是受鉴权保护的路径前缀。
const protectedPrefix = "/api/v1"

// underProtectedPrefix 判断路径是否落在受保护前缀下。
//
// 刻意**要求前缀后紧跟'/' 或字符串结束**：裸 HasPrefix 会把
// /api/v1x（假想的其它资源）也保护起来。当前没有这类路由，
// 但那属于「靠不存在的东西保证安全」—— 一旦有人加了
// /api/v1internal 就会静默被鉴权拦下，症状极难定位。
func underProtectedPrefix(path string) bool {
	if !strings.HasPrefix(path, protectedPrefix) {
		return false
	}
	rest := path[len(protectedPrefix):]
	return rest == "" || rest[0] == '/'
}

// authSessionPath 是唯一豁免鉴权的 API 路径。
//
// 字面量与 internal/api 的 authSessionPath 各写一份：跨包共享常量
// 会让 middleware 反向依赖 api，而 api 已经依赖 middleware 的响应形状 ——
// 那是个环。改这里时必须同步改 internal/api/auth.go，
// 那边有断言锁住路径（TestAuthSessionPathMatchesMiddleware）。
const authSessionPath = "/api/v1/auth/session"

// isAuthSessionPath 判断是否该豁免路径。
//
// 允许尾部斜杠：浏览器与前端库偶尔会自动补 '/'，
// 若严格相等会表现为「本地能过、部署后 401」。
func isAuthSessionPath(path string) bool {
	return path == authSessionPath || path == authSessionPath+"/"
}

// writeUnauthorized 写 401 响应。
//
// **响应体形状必须与 internal/api 的 errorBody 一致**（{"error":…, "code":…}）：
// 前端对所有 API 错误走同一个解析路径，这里换成另一种形状会让
// 「token 错了」表现为「页面崩了」而不是「请检查 token」——
// 那正是最难排查的一类症状。
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusUnauthorized)

	// 注意：**不回显期望的 token**，也不回显请求方带上来的值。
	// 401 响应常被代理与客户端日志记录，回显等于把凭据再泄一次。
	// 错误文案要说清「用户接下来该做什么」，这是判据，不是措辞偏好。
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "缺少或错误的访问 token（请求头 " + AuthHeader + "）",
		"code":  authCodeUnauthorized,
	})
}
