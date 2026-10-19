package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	appmw "github.com/yu/iip/internal/middleware"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/incident"
)

// parseIncidentID 解析路径里的事件 id；非数字返回 400 并写出响应。
func parseIncidentID(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, "事件 id 必须是正整数")
		return 0, false
	}
	return id, true
}

// decodeStrict 解析严格 JSON 请求体（未知字段拒绝，ADR-010）。
func decodeStrict(w http.ResponseWriter, r *http.Request, logger *slog.Logger, dst any, what string) bool {
	// 先读字节再解码（而不是直接 Decode 流）：否则 MaxBytesReader 的超限错误
	// 会被 Decode 一并返回，最终映射成「不是合法 JSON 或含未知字段」——
	// 体量问题被报成语法问题，看日志的人会去改 JSON 而不是看 body 有多大。
	body, ok := readBody(w, r, incidentBodyMaxBytes, logger)
	if !ok {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		logger.Warn(what+"请求体解析失败",
			slog.String("error", err.Error()),
			slog.String("peer_addr", appmw.PeerAddr(r)))
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, "请求体不是合法 JSON 或含未知字段")
		return false
	}
	return true
}

// writeIncidentError 把领域层错误映射为 HTTP 状态码与错误码。
//
// 顺序有讲究：先判具体错误类型，最后才落到 503。
// 反过来的话「事件不存在」会被当成存储故障，前端就会提示「请重试」——
// 而重试一万个不存在的 id 也不会成功。
func writeIncidentError(w http.ResponseWriter, logger *slog.Logger, err error, what string) {
	var invalid *incident.InvalidTransitionError
	var conflict *incident.ConflictError

	switch {
	case errors.Is(err, incident.ErrIncidentNotFound):
		logger.Warn(what, slog.String("error", err.Error()))
		writeErrorCode(w, logger, http.StatusNotFound, CodeIncidentNotFound, err.Error())

	case errors.As(err, &invalid):
		logger.Warn(what, slog.String("code", invalid.Code()), slog.String("error", err.Error()))
		writeErrorCode(w, logger, http.StatusUnprocessableEntity, invalid.Code(), err.Error())

	case errors.As(err, &conflict):
		logger.Warn(what, slog.String("code", conflict.Code()), slog.String("error", err.Error()))
		writeErrorCode(w, logger, http.StatusConflict, conflict.Code(), err.Error())

	default:
		logger.Error(what, slog.String("error", err.Error()))
		writeErrorCode(w, logger, http.StatusServiceUnavailable, CodeStorageUnavail, msgStorageUnavailable)
	}
}

// parseIncidentFilter 解析列表的过滤与分页参数。
//
// status 支持「可重复」与「逗号分隔」两种写法（?status=open&status=resolved 与
// ?status=open,resolved 等价），因为前端的多选控件两种都会用到。
func parseIncidentFilter(r *http.Request, logger *slog.Logger) incident.ListFilter {
	q := r.URL.Query()

	var f incident.ListFilter
	for _, raw := range q["status"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			s, err := incident.ParseStatus(part)
			if err != nil {
				// 脏过滤值忽略而非 400：列表是只读查询，
				// 一个拼错的状态不该让整页加载失败。
				logger.Warn("忽略非法的 status 过滤值", slog.String("value", part))
				continue
			}
			f.Statuses = append(f.Statuses, s)
		}
	}

	if v := q.Get("severity"); v != "" {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				f.Severities = append(f.Severities, part)
			}
		}
	}

	f.Service = strings.TrimSpace(q.Get("service"))
	f.From = parseTimeParam(q.Get("from"), "from", logger)
	f.To = parseTimeParam(q.Get("to"), "to", logger)

	// 分页参数交给服务层夹紧；这里只做「能不能解析」，解析不了就传 0（= 默认）。
	f.Limit = parseIntParam(q.Get("limit"), "limit", logger)
	f.Offset = parseIntParam(q.Get("offset"), "offset", logger)
	return f
}

// parseTimeParam 解析 RFC3339 时间参数；空串或非法返回 nil（= 不过滤）。
func parseTimeParam(raw, name string, logger *slog.Logger) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		logger.Warn("忽略非法的时间参数", slog.String("name", name), slog.String("value", raw))
		return nil
	}
	return &t
}

// parseIntParam 解析整数参数；非法返回 0（由服务层退化为默认值）。
func parseIntParam(raw, name string, logger *slog.Logger) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		logger.Warn("忽略非法的整数参数", slog.String("name", name), slog.String("value", raw))
		return 0
	}
	return n
}
