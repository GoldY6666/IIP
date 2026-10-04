package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// mimeTypes 是显式扩展名表。
//
// 为什么不用 mime.TypeByExtension：它在 Unix 读 /etc/mime.types、
// 在 Windows 读注册表，**同一个二进制在不同基础镜像上行为不同**。
// 运行层是 distroless（连 /etc/mime.types 都没有），TypeByExtension
// 会返回空串 → Content-Type 为空 → 浏览器拒绝执行脚本。
// 显式表让行为与宿主环境无关。
var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".txt":   "text/plain; charset=utf-8",
	".xml":   "application/xml; charset=utf-8",
}

// contentType 返回 fullPath 的 Content-Type。
//
// 扩展名缺失或不认识时返回 application/octet-stream（浏览器会当下载处理，
// 表现为「下载了 index.html」而不是「页面打不开」）—— 这比返回空串好：
// 空串会被浏览器当 text/plain 猜测，可能执行出一个空白页。
func contentType(fullPath string) string {
	if ct, ok := mimeTypes[strings.ToLower(filepath.Ext(fullPath))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// immutableMaxAge 是 /assets/* 的缓存期（秒）。
//
// 一年 + immutable：vite 产物文件名带内容 hash，内容变了文件名就变，
// 因此可以永久缓存。index.html 绝不能这样缓存 —— 它引用那些 hash 文件名，
// 缓存住会让用户永远加载旧版本，且**没有任何报错**。
const immutableMaxAge = 31536000

// serveFile 服务一个真实文件。
//
// rel 用于判断是否在 /assets/ 下（决定缓存策略），full 是磁盘绝对路径。
func serveFile(w http.ResponseWriter, r *http.Request, full, rel string) {
	setCache(w, rel)
	w.Header().Set("Content-Type", contentType(full))

	// 用 ServeFile 而非手写 ServeContent：标准库会自己开并关文件，
	// 不会泄漏句柄。手写路径要自己管 Close，漏了就是文件句柄泄漏 ——
	// 表现为「测试跑到最后 t.TempDir() 删不掉目录」。
	//
	// 这里不设 Content-Length：ServeFile 会自己处理。
	// 也不设 Last-Modified：它同样由 ServeFile 从 Stat 结果推导。
	http.ServeFile(w, r, full)
}

// serveIndex 服务 index.html（SPA 回退目标）。
func serveIndex(w http.ResponseWriter, r *http.Request, root string) {
	// index.html 永不缓存。理由见 immutableMaxAge 的注释。
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", contentType(indexFile))

	full := filepath.Join(root, indexFile)
	f, err := os.Open(full) // #nosec G304 -- full 由 root 拼出，root 已 Abs 化
	if err != nil {
		// 产物不完整：只放了 assets 忘了 build 出 index.html。
		// 此时明确报 500 而非回退到空页 —— 「UI 404」会被误判成产品坏了，
		// 而这里能让人一眼看出是 dist 没构建完整。
		http.Error(w, "index.html not found: 前端产物不完整，请先执行 pnpm run build",
			http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// 传 f 而非 mustOpen：ServeContent 不接管所有权，Close 由上面的 defer 负责。
	http.ServeContent(w, r, indexFile, info.ModTime(), f)
}

// setCache 按路径设置缓存头。
//
// 判定只看 /assets/ 前缀而不是「文件名里有 hash」：
// vite 默认把带 hash 的产物放到 assets/，而 index.html 在根下。
// 少数配置（rollupOptions）会改目录名，那种情况下缓存策略偏保守
// （不缓存）—— 少缓存只是性能损失，缓存错了是功能故障，取安全侧。
func setCache(w http.ResponseWriter, rel string) {
	if strings.HasPrefix(rel, "assets/") {
		w.Header().Set("Cache-Control",
			"public, max-age="+itoa(immutableMaxAge)+", immutable")
		return
	}
	// 其余产物（favicon、静态图）不缓存：内容不保证带 hash，
	// 缓存住可能出现「改了图但用户看不到」。
	w.Header().Set("Cache-Control", "no-cache")
}

// writeJSON404 给未注册的 /api 路径返回 JSON 404。
//
// 必须是 JSON：前端 fetch 会尝试 JSON.parse，若拿到 index.html，
// 抛出的错是「Unexpected token '<'」—— 指向前端，而问题在后端路由。
// 返回 JSON + 可读 message，前端报错与后端日志就能对上。
func writeJSON404(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "not found",
		"path":  "该 API 路径未注册（静态托管已启用，但 /api 路由不存在）",
	})
}

// itoa 避免为一个常量引入 strconv 依赖到本文件热路径。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
