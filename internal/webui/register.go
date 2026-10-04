package webui

import (
	"log/slog"
	"net/http"
)

// Router 是 chi 的最小形状约束。
//
// **为什么要自己定义而不直接用 chi.Router**：本包不 import chi
// （引入它只为约束一个方法签名不值当），且这样单元测试能用
// 普通 http.ServeMux 验证「注册后 /api 优先」这条性质。
//
// 签名必须逐字匹配 chi：chi.Mux.NotFound 收的是 **http.HandlerFunc**
// 而非 http.Handler —— 两者不同，写错会报
// 「does not implement webui.Router (wrong type for method NotFound)」。
// 这里刻意不用「宽接口 + 适配」绕开：那会让调用方以为可以传任意
// http.Handler，实际不行。
type Router interface {
	// NotFound 注册「所有路由都没命中」时的兜底处理器。
	NotFound(h http.HandlerFunc)
}

// Register 把静态托管挂到路由尾部。
//
// 之所以提供这个函数而不是让调用方自己写 `r.NotFound(h.ServeHTTP)`：
// **「静态托管必须最后注册」是本包的知识，不是调用方的。**
// 留在调用方那里的后果是：将来有人在 API 之后插一行新注册，
// 静态兜底顺序看似不变，但他不知道「把 NotFound 换成会吞掉 /api 的
// 通配路由」这个改动的风险。收进包内，这个顺序要求就无法被无声破坏 ——
// 唯一的调用点就是 main.go 里 newRouter 的最后一行。
//
// webRoot 为空时**不做任何注册**，服务行为与未引入本包时完全一致。
//
// 日志走 slog 而非返回 error：装配失败不该让服务起不来，
// 但必须留痕 —— 「UI 没起来」在部署时是最容易被忽略的问题
// （API 一切正常，只有打开页面才看到 404）。
func Register(r Router, webRoot string, ui UIConfig, logger *slog.Logger) {
	h := Handler(webRoot, ui)
	if h == nil {
		if logger != nil {
			logger.Info("未启用前端静态托管（IIP_WEB_ROOT 为空）")
		}
		return
	}
	r.NotFound(h.ServeHTTP)
	if logger != nil {
		// 把注入的地址也打出来：「链接点不开」时这一行是唯一线索。
		c := ui.WithDefaults()
		logger.Info("已启用前端静态托管",
			slog.String("web_root", webRoot),
			slog.String("ui_prometheus", c.Prometheus),
			slog.String("ui_grafana", c.Grafana),
			slog.String("ui_loki", c.Loki))
	}
}
