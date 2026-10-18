// 请求体读取的统一入口（M6 T6.6，ADR-027）。
//
// 为什么把这件事收成一处
// ----------------------
// 改之前三个 JSON 入口各写一套 body 读取：`alert_webhook.go` 的 readBody、
// `correlation_config.go` 的内联块、`incidents.go` 的 decodeStrict。
// 三套实现的差异不只是重复代码，而是**错误语义不一致**，那是要人命的：
//
//   - 非 UTF-8 请求体三个入口都会「成功」通过解析（Go 的 encoding/json
//     按 JSON 规范应当拒绝，实际上它只把非法字节原样留在 string 里），
//     然后在落库时被 PostgreSQL 以 SQLSTATE 22021 拒掉 —— 于是接口回 503
//     「storage unavailable」。客户端编码错被报成服务端存储故障，
//     运维会去查 PG，而真因在自己的终端编码。
//   - `incidents.go` 的 decodeStrict 直接 `json.NewDecoder(r.Body)` 读流，
//     MaxBytesReader 的超限错误被 Decode 一并返回后，统一映射成
//     「请求体不是合法 JSON 或含未知字段」—— 体量问题被报成语法问题。
//     另外两个入口都有 413 用例，只有这个没有，所以它一直没人发现。
//
// 收成一处之后有一条可判定的不变量：**同一类坏输入在任何入口都得到同一状态码
// 与同一文案**。它抓的不是今天的三个入口，而是"以后加第四个入口又各写一套"。
package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	appmw "github.com/yu/iip/internal/middleware"
)

// readBody 读取并校验请求体；返回 false 表示已写出错误响应。
//
// 三层检查的顺序有讲究，不要调换：
//  1. MaxBytesReader 先设上限 —— 在超限处即停止读取，不会先把超大体积读进内存。
//     若改成"先 io.ReadAll 再判长度"，限制大小本身就成了一次内存打满攻击的入口。
//  2. ReadAll 的其他错误（上游中途断开）→ 400 并留日志：客户端已经收不到响应，
//     但服务端必须能查到"有人发了一半就断了"。
//  3. UTF-8 合法性 → 400。**必须在解码之前**：一旦解进 string，
//     非法字节就变成"看似正常的 Go 字符串"，直到写库才爆，且爆在错误的层。
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64, logger *slog.Logger) ([]byte, bool) {
	if logger == nil {
		logger = slog.Default()
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			logger.Warn("请求体超限",
				slog.Int64("limit_bytes", maxBytes),
				slog.String("path", r.URL.Path),
				slog.String("peer_addr", appmw.PeerAddr(r)))
			writeError(w, logger, http.StatusRequestEntityTooLarge, msgPayloadTooLarge)
			return nil, false
		}
		// 上游中途断开连接等：客户端已收不到响应，但仍要留日志。
		logger.Warn("读取请求体失败",
			slog.String("error", err.Error()),
			slog.String("path", r.URL.Path),
			slog.String("peer_addr", appmw.PeerAddr(r)))
		writeError(w, logger, http.StatusBadRequest, msgReadBodyFailed)
		return nil, false
	}

	if !utf8.Valid(body) {
		// 记下第一个非法字节的偏移，定位时不必猜「整段哪里坏了」。
		offset := firstInvalidUTF8(body)
		logger.Warn("请求体不是合法 UTF-8",
			slog.Int("first_invalid_offset", offset),
			slog.String("path", r.URL.Path),
			slog.String("peer_addr", appmw.PeerAddr(r)))
		writeError(w, logger, http.StatusBadRequest, msgPayloadNotUTF8)
		return nil, false
	}
	return body, true
}

// firstInvalidUTF8 返回第一个非法 UTF-8 字节的偏移；整段合法时返回 -1。
//
// 自己走一遍而不是用 utf8.DecodeRune 连调用：需要在日志里报偏移，
// 而调用方只想知道"从哪一字节开始坏"。
func firstInvalidUTF8(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}
