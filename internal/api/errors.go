package api

import (
	"log/slog"
	"net/http"
)

// errorBody 是全站统一的错误响应体（原定义在 alert_webhook.go，M3 迁到本文件）。
//
// Code 带 omitempty：既有调用点只写 Error，序列化结果仍是 {"error":"..."}，
// 与 M1/M2 的契约完全一致——**既有端点不追加 code**（决策 D2），
// 避免为了一个新字段去扩大既有回归面。
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// writeError 写出不带 code 的错误响应（既有端点沿用，行为一字未变）。
func writeError(w http.ResponseWriter, logger *slog.Logger, status int, msg string) {
	writeJSON(w, logger, status, errorBody{Error: msg})
}

// writeErrorCode 写出带 code 的错误响应（M3 新端点使用）。
//
// 为什么要 code：状态流转这类错误有很多种（非法转换 / 并发冲突 / 事件不存在），
// 前端若只能靠 HTTP 状态码区分，就得去解析中文文案——而文案是要给人看的，不该被当成协议。
func writeErrorCode(w http.ResponseWriter, logger *slog.Logger, status int, code, msg string) {
	writeJSON(w, logger, status, errorBody{Error: msg, Code: code})
}

// M3 新端点的错误码（手册 §4.3 错误码表）。
const (
	CodeInvalidTransition = "invalid_transition"
	CodeInvalidBody       = "invalid_body"
	CodeIncidentNotFound  = "incident_not_found"
	CodeStatusConflict    = "status_conflict"
	CodeStorageUnavail    = "storage_unavailable"
)
