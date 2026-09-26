package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/yu/iip/internal/alert"
)

const (
	// defaultListLimit 是列表接口的默认单页大小（手册 §3.1 P1：返回最新 100 条）。
	defaultListLimit = 100

	// maxListLimit 是单页上限，防止一次拉取拖垮存储（也避免前端误传超大值）。
	maxListLimit = 1000
)

// AlertQuerier 是列表查询所需的最小能力，定义在**消费方**（本包）。
//
// *alert.Service 天然满足它（已暴露 List 方法）。handler 单测可注入任意实现，
// 不必连库，也不必关心 Service 的具体构造。
type AlertQuerier interface {
	List(ctx context.Context, limit, offset int32) ([]alert.StoredAlert, error)
}

// listResponse 是 GET /api/v1/alerts 的统一响应形状。
//
// alerts 直接是 StoredAlert 切片：StoredAlert 内嵌 Alert（匿名结构体），
// encoding/json 会把内层字段提升为顶层，所以是扁平的 JSON 对象数组；
// Labels/Annotations 是 json.RawMessage，原样回传；时间字段按 RFC3339 序列化。
type listResponse struct {
	Alerts []alert.StoredAlert `json:"alerts"`
	Count  int                 `json:"count"`
}

// AlertList 返回 GET /api/v1/alerts 的 handler。
//
// 查询参数（均有兜底，缺省或非法都退化为安全默认值）：
//   - limit：单页大小，默认 100，超出 [1, maxListLimit] 区间被夹紧到边界。
//   - offset：偏移量，默认 0，负数被夹紧到 0。
//
// 存储层故障时回 503（与 webhook 一致），让调用方可以重试，而不是拿到空列表后
// 误以为「系统里真的没有告警」。
func AlertList(querier AlertQuerier, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := parseListParams(r, logger)

		alerts, err := querier.List(r.Context(), limit, offset)
		if err != nil {
			logger.Error("查询告警列表失败",
				slog.String("error", err.Error()),
				slog.String("remote_addr", r.RemoteAddr))
			writeError(w, logger, http.StatusServiceUnavailable, msgStorageUnavailable)
			return
		}

		writeJSON(w, logger, http.StatusOK, listResponse{
			Alerts: alerts,
			Count:  len(alerts),
		})
	}
}

// parseListParams 从 query string 解析并规整 limit / offset。
//
// 缺省或无法解析一律退化为安全默认，而非报错——列表是只读查询，
// 因参数脏而直接 400 反而会挡掉正常的页面加载。
func parseListParams(r *http.Request, logger *slog.Logger) (limit, offset int32) {
	limit = defaultListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			logger.Warn("告警列表 limit 参数非法，使用默认值",
				slog.String("value", v))
		} else if n < 1 {
			limit = 1
		} else if n > maxListLimit {
			limit = maxListLimit
		} else {
			limit = int32(n)
		}
	}

	offset = 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			logger.Warn("告警列表 offset 参数非法，使用默认值 0",
				slog.String("value", v))
		} else if n > 0 {
			offset = int32(n)
		}
	}
	return limit, offset
}
