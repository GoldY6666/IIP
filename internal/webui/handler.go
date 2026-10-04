package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 索引文件名。前端产物根下必有它，SPA 回退也返回它。
const indexFile = "index.html"

// ServeHTTP 实现三段行为：
//
//  1. 路径逃逸一律拒绝（安全红线）
//  2. 命中真实文件 → 按扩展名给 Content-Type
//  3. 未命中且非 /api → SPA 回退到 index.html
//
// 不实现 Range 请求：dist 是构建产物，用户不会对压缩后的 js 做分段拉取。
// 少一条路径就少一处能写错的地方。
func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 只接 GET/HEAD。distroless 里没有 sh/curl/wget，
	// 若某个部署方误配了会直接打 /api，会让 405 比 404 更早暴露问题 ——
	// 这正是我们要的：让误用在响应里可见。
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel, ok := safeRel(r.URL.Path)
	if !ok {
		// 明确返回 400 而不是 404：这是**请求本身不合法**，
		// 不是「文件不存在」。两者混同会让排查方向完全跑偏
		//（去找文件是否存在，而问题在客户端拼 URL）。
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	full := filepath.Join(h.root, filepath.FromSlash(rel))

	// 二次防线：Clean 后的结果仍须在 root 之内。
	// 第一道（safeRel）已挡住字面 ".."，但符号链接会绕过它 ——
	// 那种情况下 http.ServeFile 自己做的校验在这里不生效，因为
	// 我们先做了 Join 再判断。留着这行是为了让逃逸无论从哪条路进来都无效。
	if !withinRoot(h.root, full) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(full)
	switch {
	case err == nil && !info.IsDir():
		// 直接请求到 index.html（如 /index.html）时也要注入，
		// 否则会出现「根路径有注入、深链接没有」的诡异不一致。
		if filepath.Base(full) == indexFile {
			serveIndex(w, r, h.root, h.ui)
			return
		}
		serveFile(w, r, full, rel)
		return
	case err == nil && info.IsDir():
		// 目录：补尾斜杠，让 indexFile 作为目录索引被取出。
		// 少这个 301 的话，浏览器请求 /assets 会拿到 404，
		// 表现为「构建产物里明明有 assets 却说找不到」。
		if strings.HasSuffix(r.URL.Path, "/") {
			index := filepath.Join(full, indexFile)
			st, statErr := os.Stat(index)
			if statErr == nil && !st.IsDir() {
				// **必须走 serveIndex 而非 serveFile**：注入只发生在
				// serveIndex 里。走 serveFile 的话，请求根路径 /
				// （它命中的是「目录 + 尾斜杠」分支）会拿到未注入的
				// index.html —— 症状是「UI 能开，但链接全是默认值」，
				// 且**没有任何报错**。
				serveIndex(w, r, h.root, h.ui)
				return
			}
		} else {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
	case errors.Is(err, fs.ErrNotExist):
		// 落到下面的 SPA 回退
	default:
		// 权限不足等真实 I/O 错误。不回退到 index.html ——
		// 那样会把「文件存在但读不了」伪装成「路径不存在」。
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// SPA 回退：前端用 react-router，/incidents/12 是真实路由而非文件。
	// 不回退的话用户刷新深链接直接 404，且 404 页是浏览器默认页，
	// 看起来像服务坏了。
	//
	// 但 /api/* **不能**回退 —— 调方会拿到 index.html 然后报
	// JSON 解析错误。调用方应把 /api 路由注册在本 handler 之前，
	// 走到这里就说明确实是未注册的 API 路径，此时返回 404 JSON 才诚实。
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON404(w)
		return
	}
	serveIndex(w, r, h.root, h.ui)
}
