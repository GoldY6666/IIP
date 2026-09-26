// Package api 承载 IIP 的 HTTP handler 层。
//
// 本层只做 HTTP 该做的事：解析请求体、限制大小、决定状态码、把领域结果翻成响应体。
// 业务规则（校验/归一化/去重）一律在 internal/alert，SQL 一律在 internal/repository——
// 依赖方向 api → alert → repository → db/store，只向下（手册 §4.5）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yu/iip/internal/alert"
)

const (
	// webhookTimeout 是单次 webhook 处理的上限（手册 §4.1）。
	//
	// 超时后 Ingest 内的 DB 操作随 ctx 取消而失败，handler 回 503，
	// 进而触发 Alertmanager 重试——宁可重试，也不能把告警悄悄丢掉。
	webhookTimeout = 5 * time.Second

	// contentTypeJSON 是全部响应（含错误）统一的 Content-Type。
	contentTypeJSON = "application/json; charset=utf-8"

	// msgPayloadTooLarge 与手册 §4.1 状态码矩阵逐字一致。
	msgPayloadTooLarge = "payload too large"

	// msgStorageUnavailable 与手册 §4.1 状态码矩阵逐字一致。
	msgStorageUnavailable = "storage unavailable"

	// msgReadBodyFailed 是「请求体既非超限也非 JSON 问题」时的兜底说明。
	msgReadBodyFailed = "读取请求体失败"
)

// AlertIngestor 是 handler 所需的最小能力，定义在**消费方**（本包，而非 alert 包）。
//
// *alert.Service 天然满足它。这样 handler 单测可注入任意实现，
// handler 也不必关心 Service 的具体构造方式。
type AlertIngestor interface {
	Ingest(ctx context.Context, p *alert.Payload) (alert.Result, error)
}

// errorBody 是错误响应的统一形状。
type errorBody struct {
	Error string `json:"error"`
}

// AlertWebhook 返回 POST /api/v1/alerts/webhook 的 handler。
//
// maxBytes 为请求体上限（来自 IIP_WEBHOOK_MAX_BYTES）。超限回 413 而非截断——
// 截断会产出"半条 JSON"，把上游一条合法告警变成语义不明的 400。
//
// 状态码矩阵（手册 §4.1）：200 成功（含全部重复、部分条目被拒）、400 非 JSON 或
// 结构缺失、413 超限、503 存储不可用。
//
// **重复投递必须回 200**：Alertmanager 把非 2xx 视为投递失败并重试，
// 若重复回 409 就会陷入无限重试。
func AlertWebhook(ingestor AlertIngestor, maxBytes int64, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
		defer cancel()

		body, ok := readBody(w, r, maxBytes, logger)
		if !ok {
			return
		}

		payload, err := alert.ParsePayload(body)
		if err != nil {
			// 非 JSON 属上游问题，但必须明确回 400（不能 200）：
			// 回 200 会让 Alertmanager 认为投递成功，那条告警就永久丢了。
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		if err = alert.ValidatePayload(payload); err != nil {
			// 结构级缺失（如无 version / 无 alerts）同样整批 400；
			// 单条字段问题不在此拦，由 Service 降级为该条 rejected。
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		result, err := ingestor.Ingest(ctx, payload)
		if err != nil {
			// Ingest 只在存储层故障时返回 error → 回 503 让 Alertmanager 重试。
			// 日志必须带根因，否则线上只见一片 503，无从定位。
			logger.Error("告警 webhook 处理失败",
				slog.String("error", err.Error()),
				slog.String("remote_addr", r.RemoteAddr))
			writeError(w, logger, http.StatusServiceUnavailable, msgStorageUnavailable)
			return
		}

		writeJSON(w, logger, http.StatusOK, result)
	}
}

// readBody 读取请求体并施加大小上限；返回 false 表示已写出错误响应。
//
// 用 http.MaxBytesReader 而非先 io.ReadAll 再判断长度：前者在超限处即停止读取，
// 不会先把超大体积读进内存——否则"限制大小"本身就成了一次内存打满攻击的入口。
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64, logger *slog.Logger) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			logger.Warn("告警 webhook 请求体超限",
				slog.Int64("limit_bytes", maxBytes),
				slog.String("remote_addr", r.RemoteAddr))
			writeError(w, logger, http.StatusRequestEntityTooLarge, msgPayloadTooLarge)
			return nil, false
		}
		// 上游中途断开连接等：客户端已收不到响应，但仍要留日志。
		logger.Warn("读取告警 webhook 请求体失败",
			slog.String("error", err.Error()),
			slog.String("remote_addr", r.RemoteAddr))
		writeError(w, logger, http.StatusBadRequest, msgReadBodyFailed)
		return nil, false
	}
	return body, true
}

// writeJSON 写出 JSON 响应。
func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 响应体已开始写出，状态码无法再改；只能留痕。
		logger.Error("写出告警 webhook 响应失败", slog.String("error", err.Error()))
	}
}

// writeError 写出统一的错误响应体。
func writeError(w http.ResponseWriter, logger *slog.Logger, status int, msg string) {
	writeJSON(w, logger, status, errorBody{Error: msg})
}
