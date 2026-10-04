package webui

import (
	"path"
	"path/filepath"
	"strings"
)

// safeRel 把 URL 路径转成 root 下的相对路径，失败时返回 ok=false。
//
// **这是本包唯一的安全边界**，判定逻辑集中在这里，便于测试穷举。
//
// 拒绝的输入分三类，覆盖了 URL 规范里所有能让路径「往上走」或改变语义的形式：
//
//  1. 任何 .. 段           —— 目录穿越的直接形式
//  2. 反斜杠 \             —— Windows 上 filepath 会当成路径分隔符，
//     而 URL 里的 \ 本身非法，不存在「合法但要支持」的用法
//  3. Clean 后为绝对路径    —— 覆盖形如 "/../etc"（Clean 后仍是 ../etc）
//     与 "/a/../../etc" 这类多个 .. 组合
//
// 刻意**不**做的：
//
//   - URL 解码不做。Go 的 net/http 已在 ServeHTTP 之前把
//     r.URL.Path 解码过一次（%2e%2e 早在那里就变成了 ..），
//     所以对 Path 做检查是有效的；再手工解码一次反而会双重解码，
//     让 %252e 变成 %2e 后被当成正常字符放过。
//   - 空字节 \x00 不检查。Go 的 os.Stat/Lstat 对含 NUL 的路径直接返回
//     ErrInvalid，而 ServeContent 在 open 失败时也不会读到一半。
//     手工检查会引入一个「我们处理了 NUL」的错觉，实际并不需要。
//
// rel 为空时（URL 是根路径 "/"）返回 "index.html" 之外的调用方需自行处理，
// 本函数返回 ("", true)，由调用方决定根路径语义。
func safeRel(urlPath string) (rel string, ok bool) {
	// 反斜杠：URL 规范里它不是分隔符，出现在 path 中即为构造痕迹。
	if strings.ContainsRune(urlPath, '\\') {
		return "", false
	}

	// 空路径与 "/" 交给调用方（根路径 = 目录索引）。
	if urlPath == "" || urlPath == "/" {
		return "", true
	}

	// 去掉前导斜杠：URL path 是绝对的，root 下的相对路径不带。
	p := strings.TrimPrefix(urlPath, "/")

	// 第一层：逐段看有没有 ".."。
	// 必须逐段而不是直接 strings.Contains(p, "..") ——
	// 后者会把合法文件名 "foo..bar" 也拒掉。
	// path.Clean 在这里用作**分段器**（它保留 ../ 前缀），
	// 不能只依赖 Clean 的结果，那正是第三层要兜的。
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}

	// 第二层：Clean 归一化掉 "." 与重复斜杠。
	cleaned := path.Clean(p)
	if cleaned == "." {
		return indexFile, true
	}
	// 防御：Clean 掉所有 ".." 后若仍以 "../" 开头，说明输入畸形。
	// 上一层已逐段拒绝 ".."，正常到不了这里；留着是防止未来
	// 有人放宽上一层时静默失守。
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", false
	}
	// 绝对路径不该出现在这里（已 TrimPrefix 前导斜杠），
	// 但一旦出现说明上游做了奇怪的处理，直接拒。
	if strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

// withinRoot 报告 path 是否位于 root 之内（含 root 自身）。
//
// 用 filepath.Rel 而非字符串 HasPrefix：后者挡不住
// root="/var/www" 而 path="/var/www-evil" 的情况 —— 前缀匹配只看开头。
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// Rel 成功但结果以上 ".." 开头 = 逃出了 root。
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
