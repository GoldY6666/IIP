package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	appmw "github.com/yu/iip/internal/middleware"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/incident"
)

// CodeReviewEdgeNotFound 是复核边不存在时的错误码（手册 §4.3）。
const CodeReviewEdgeNotFound = "review_edge_not_found"

// ReviewLister 是待确认队列的读取能力。
type ReviewLister interface {
	ListReviews(ctx context.Context, limit, offset int) (incident.ReviewListResult, error)
}

// ReviewDecider 是复核（确认 / 拒绝）能力。
type ReviewDecider interface {
	Review(ctx context.Context, incidentID, alertID int64,
		decision incident.ReviewDecision, actor, reason string) (incident.ReviewResult, error)
}

type reviewListResponse struct {
	Reviews []incident.ReviewEdge `json:"reviews"`
	Count   int                   `json:"count"`
	HasMore bool                  `json:"has_more"`
}

type reviewDecideRequest struct {
	Decision string `json:"decision"`
	Actor    string `json:"actor"`
	Reason   string `json:"reason,omitempty"`
}

type reviewDecideResponse struct {
	Result incident.ReviewResult `json:"result"`
}

// ReviewList 返回 GET /api/v1/reviews 的 handler（待确认队列）。
func ReviewList(lister ReviewLister, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := parseIntParam(q.Get("limit"), "limit", logger)
		offset := parseIntParam(q.Get("offset"), "offset", logger)

		res, err := lister.ListReviews(r.Context(), limit, offset)
		if err != nil {
			logger.Error("查询待确认队列失败",
				slog.String("error", err.Error()),
				slog.String("peer_addr", appmw.PeerAddr(r)))
			writeErrorCode(w, logger, http.StatusServiceUnavailable, CodeStorageUnavail, msgStorageUnavailable)
			return
		}

		writeJSON(w, logger, http.StatusOK, reviewListResponse{
			Reviews: res.Items,
			Count:   len(res.Items),
			HasMore: res.HasMore,
		})
	}
}

// ReviewDecide 返回 POST /api/v1/reviews/{incidentID}/{alertID} 的 handler。
//
// 404 的语义是「这条边不存在**或已被处理过**」——对调用方而言两者一样：
// 都不该再操作第二次。因此重复提交同一个 reject 会拿到 404，而不是静默成功。
func ReviewDecide(decider ReviewDecider, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		incidentID, alertID, ok := parseReviewIDs(w, r, logger)
		if !ok {
			return
		}

		var req reviewDecideRequest
		if !decodeStrict(w, r, logger, &req, "人工复核") {
			return
		}
		decision, err := incident.ParseReviewDecision(req.Decision)
		if err != nil {
			writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, err.Error())
			return
		}

		res, err := decider.Review(r.Context(), incidentID, alertID, decision, req.Actor, req.Reason)
		if err != nil {
			switch {
			case errors.Is(err, incident.ErrReviewEdgeNotFound):
				logger.Warn("复核的边不存在或已处理",
					slog.Int64("incident_id", incidentID),
					slog.Int64("alert_id", alertID))
				writeErrorCode(w, logger, http.StatusNotFound, CodeReviewEdgeNotFound, err.Error())
			case errors.Is(err, incident.ErrInvalidReviewDecision):
				writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, err.Error())
			default:
				logger.Error("人工复核失败", slog.String("error", err.Error()))
				writeErrorCode(w, logger, http.StatusServiceUnavailable, CodeStorageUnavail, msgStorageUnavailable)
			}
			return
		}

		logger.Info("人工复核完成",
			slog.String("decision", string(decision)),
			slog.Int64("incident_id", incidentID),
			slog.Int64("alert_id", alertID),
			slog.Bool("suppressed", res.Suppressed),
			slog.Bool("unlinked", res.Unlinked))
		writeJSON(w, logger, http.StatusOK, reviewDecideResponse{Result: res})
	}
}

// parseReviewIDs 解析复核路径上的 (incidentID, alertID)。
func parseReviewIDs(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (incidentID, alertID int64, ok bool) {
	incidentID, err := strconv.ParseInt(chi.URLParam(r, "incidentID"), 10, 64)
	if err != nil || incidentID <= 0 {
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, "事件 id 必须是正整数")
		return 0, 0, false
	}
	alertID, err = strconv.ParseInt(chi.URLParam(r, "alertID"), 10, 64)
	if err != nil || alertID <= 0 {
		writeErrorCode(w, logger, http.StatusBadRequest, CodeInvalidBody, "告警 id 必须是正整数")
		return 0, 0, false
	}
	return incidentID, alertID, true
}
