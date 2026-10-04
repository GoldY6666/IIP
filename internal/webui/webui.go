// Package webui 提供前端静态托管与 SPA 回退（M5 S3，规格见
// docs/IIP-M5详细开发手册.md §4.3）。
//
// 定位：**可选**能力。仅当配置了非空的 WebRoot 时装配到路由尾部，
// 未配置时整个包不参与，裸跑测试与既有环境行为零变化。
//
// 为什么不 go:embed：M3 的决策是前端用 vite dev 独立迭代。
// 嵌入二进制会让「改一行 CSS」也要重编 Go 镜像，与该决策相悖；
// 且 dist 有 1.09 MB，embed 后 server 镜像无谓变大。
//
// 为什么不放 internal/api：那里只放业务 HTTP 契约（handler 薄、逻辑在
// service/repository）。静态托管不是业务契约，混进去会破坏分层。
package webui

import (
	"net/http"
	"path/filepath"
	"strings"
)

// Handler 返回静态文件处理器。
//
// root 是前端产物目录（对应 IIP_WEB_ROOT）。root 为空时返回 nil ——
// 调用方据此跳过装配，语义比「返回一个总是 404 的 handler」清楚：
// 后者会让「忘配 WebRoot」表现为「UI 全 404」，极难与「路径写错」区分。
//
// 路由优先级由调用方保证：/api/* 与 /healthz 必须先于本 handler 注册。
// 顺序颠倒会让 API 请求被 SPA 回退吞掉、返回 index.html 而非 JSON ——
// 那种失败的表现是「前端报 JSON 解析错误」，与「后端挂了」难分辨。
func Handler(root string, ui UIConfig) http.Handler {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	// Abs 在此求出，使后续前缀校验可以按绝对路径比较；
	// 否则 "../" 逃逸要用字符串前缀判断，而字符串前缀挡不住
	// "/var/www-evil" 匹配 "/var/www" 的情况。
	abs, err := filepath.Abs(root)
	if err != nil {
		// Abs 只在路径含非法字符时失败（如 NUL）。此时返回 nil 而不是
		// panic：静态托管是附属能力，不该让它的配置错误拖垮整个服务。
		return nil
	}
	return &staticHandler{root: abs, ui: ui.WithDefaults()}
}

// staticHandler 的零值不可用，必须经 Handler 构造。
type staticHandler struct {
	root string // 绝对路径，末尾无分隔符
	// ui 是要注入 index.html 的运行时配置（各面板的浏览器可达地址）。
	// 存而不取用：只在 serveIndex 读一次。
	ui UIConfig
}
