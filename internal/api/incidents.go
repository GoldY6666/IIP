package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	appmw "github.com/yu/iip/internal/middleware"

	"github.com/yu/iip/internal/incident"
)

// incidentBodyMaxBytes 是 M3 写接口（transition / comments）的请求体上限。
//
// 与关联配置接口取同一个量级：这两个接口的请求体只有一个短文本字段，
// 1 MiB 已经远超合理范围，留出余量只为兼容未来的扩展字段。
const incidentBodyMaxBytes = 1 << 20

// 下列四个接口都定义在**消费方**（本包），与 M1 的 AlertIngestor 同一手法：
// handler 只依赖它真正用到的那一个方法，单测注入替身即可，不必连库。

// IncidentLister 是列表查询能力。
type IncidentLister interface {
	List(ctx context.Context, f incident.ListFilter) (incident.ListResult, error)
}

// IncidentDetailer 是详情查询能力。
type IncidentDetailer interface {
	Detail(ctx context.Context, id int64) (incident.Detail, error)
}

// IncidentTransitioner 是状态流转能力。
type IncidentTransitioner interface {
	Transition(ctx context.Context, id int64, to incident.Status, actor string) (incident.Snapshot, error)
}

// IncidentCommenter 是加备注能力。
type IncidentCommenter interface {
	Comment(ctx context.Context, id int64, actor, note string) (incident.Entry, error)
}

// --- 响应体 ---

type incidentListResponse struct {
	Incidents []incident.Summary `json:"incidents"`
	Count     int                `json:"count"`
	HasMore   bool               `json:"has_more"`
}

// truncatedInfo 明确告诉前端「内联列表被截断了」。
//
// 手册 §4.3 要求前端在截断时显式提示「仅显示最近 N 条」；
// 若这里不回传，前端无从判断，用户会以为看到的就是全部。
type truncatedInfo struct {
	Alerts   bool `json:"alerts"`
	Timeline bool `json:"timeline"`
}

type incidentDetailResponse struct {
	Incident  incident.Summary     `json:"incident"`
	Alerts    []incident.AlertLink `json:"alerts"`
	Timeline  []incident.Entry     `json:"timeline"`
	Truncated truncatedInfo        `json:"truncated"`
}

type transitionRequest struct {
	To    string `json:"to"`
	Actor string `json:"actor"`
	Note  string `json:"note,omitempty"`
}

type transitionResponse struct {
	// 用 Snapshot 而不是 Summary：Summary 多了 AlertCount / NeedsReviewCount
	// 两个 JOIN 聚合列，而流转路径不做 JOIN，填 0 会被前端渲染成
	//「0 条关联告警」——把「不知道」说成了「没有」。Snapshot 的字段
	// 恰好就是这次能确定给出的全部。
	Incident incident.Snapshot `json:"incident"`
}

type commentRequest struct {
	Actor string `json:"actor"`
	Note  string `json:"note"`
}

type commentResponse struct {
	Entry incident.Entry `json:"entry"`
}

// --- handlers ---

// IncidentList 返回 GET /api/v1/incidents 的 handler。
//
// 过滤参数（status / severity / service / from / to）与分页参数（limit / offset）
// 一律「脏则退化」而非 400：列表是只读查询，因参数脏挡掉页面加载得不偿失。
func IncidentList(lister IncidentLister, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		f := parseIncidentFilter(r, logger)

		res, err := lister.List(r.Context(), f)
		if err != nil {
			logger.Error("查询事件列表失败",
				slog.String("error", err.Error()),
				slog.String("peer_addr", appmw.PeerAddr(r)))
			writeErrorCode(w, logger, http.StatusServiceUnavailable, CodeStorageUnavail, msgStorageUnavailable)
			return
		}

		writeJSON(w, logger, http.StatusOK, incidentListResponse{
			Incidents: res.Items,
			Count:     len(res.Items),
			HasMore:   res.HasMore,
		})
	}
}

// IncidentDetail 返回 GET /api/v1/incidents/{id} 的 handler。
func IncidentDetail(detailer IncidentDetailer, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseIncidentID(w, r, logger)
		if !ok {
			return
		}

		detail, err := detailer.Detail(r.Context(), id)
		if err != nil {
			writeIncidentError(w, logger, err, "查询事件详情失败")
			return
		}

		writeJSON(w, logger, http.StatusOK, incidentDetailResponse{
			Incident:  detail.Summary,
			Alerts:    detail.Alerts,
			Timeline:  detail.Timeline,
			Truncated: truncatedInfo{Alerts: detail.TruncatedAlerts, Timeline: detail.TruncatedTimeline},
		})
	}
}

// IncidentTransition 返回 POST /api/v1/incidents/{id}/transition 的 handler。
//
// 状态码：200 成功 / 400 请求体问题 / 404 事件不存在 / 409 并发冲突 /
// 422 非法转换。把 409 与 422 分开是有意的：前者要「刷新后重试」，
// 后者要「换个目标状态」，前端的提示完全不同。
func IncidentTransition(svc IncidentTransitioner, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseIncidentID(w, r, logger)
		if !ok {
			return
		}

		var req transitionRequest
		if !decodeStrict(w, r, logger, &req, "状态流转") {
			return
		}
		to, err := incident.ParseStatus(req.To)
		if err != nil {
			// 目标状态非法属于「请求体问题」而非「非法转换」：
			// 后者指的是合法状态之间的禁止组合，前者根本不是一个状态。
			writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, err.Error())
			return
		}

		snap, err := svc.Transition(r.Context(), id, to, req.Actor)
		if err != nil {
			writeIncidentError(w, logger, err, "状态流转失败")
			return
		}

		logger.Info("事件状态已流转",
			slog.Int64("incident_id", id),
			slog.String("to", string(to)))
		writeJSON(w, logger, http.StatusOK, transitionResponse{Incident: snap})
	}
}

// IncidentComment 返回 POST /api/v1/incidents/{id}/comments 的 handler。
func IncidentComment(svc IncidentCommenter, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseIncidentID(w, r, logger)
		if !ok {
			return
		}

		var req commentRequest
		if !decodeStrict(w, r, logger, &req, "事件备注") {
			return
		}

		entry, err := svc.Comment(r.Context(), id, req.Actor, req.Note)
		if err != nil {
			switch {
			case errors.Is(err, incident.ErrEmptyNote):
				writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, err.Error())
			default:
				writeIncidentError(w, logger, err, "写入事件备注失败")
			}
			return
		}

		writeJSON(w, logger, http.StatusCreated, commentResponse{Entry: entry})
	}
}

// --- 工具 ---
