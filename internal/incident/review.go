package incident

import (
	"context"
	"fmt"
)

// ReviewDecision 是人工对一条待确认关联边的判定。
type ReviewDecision string

// 两种判定（手册 §4.5）。
const (
	ReviewConfirm ReviewDecision = "confirm" // 确认合并：保留关联边
	ReviewReject  ReviewDecision = "reject"  // 拒绝合并：删除关联边并记抑制
)

// ParseReviewDecision 解析复核动作；非法值返回错误并列出合法取值。
func ParseReviewDecision(raw string) (ReviewDecision, error) {
	switch ReviewDecision(raw) {
	case ReviewConfirm:
		return ReviewConfirm, nil
	case ReviewReject:
		return ReviewReject, nil
	default:
		return "", fmt.Errorf("复核动作 %q 非法，合法取值: confirm, reject", raw)
	}
}

// ReviewResult 是一次人工复核的结果。
//
// Suppressed / Unlinked 只在 reject 时有意义，回传给调用方是为了让日志与响应
// 能说清「这次拒绝到底动了什么」——否则排查时只能去翻数据库。
type ReviewResult struct {
	Decision   ReviewDecision
	IncidentID int64
	AlertID    int64
	Suppressed bool // 是否写入了抑制行（reject 时为真）
	Unlinked   bool // 是否把该告警从事件中解除（reject 且它已无其他关联边）
}

// ReviewListResult 是待确认队列的一页（与列表同样用 limit+1 探测）。
type ReviewListResult struct {
	Items   []ReviewEdge
	HasMore bool
}

// ListReviews 取待确认队列。
func (s *Service) ListReviews(ctx context.Context, limit, offset int) (ReviewListResult, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := s.tx.ListReviews(ctx, limit+1, offset)
	if err != nil {
		return ReviewListResult{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return ReviewListResult{Items: rows, HasMore: hasMore}, nil
}

// Review 处理一条待确认边：确认或拒绝。
//
// 全程在一个事务内（删边 / 回写指针 / 写抑制 / 写留痕），理由与状态流转一致：
// 这四步是**一件事**，只做成一半会让页面出现「边没了却没有留痕」这类幽灵状态。
func (s *Service) Review(
	ctx context.Context,
	incidentID, alertID int64,
	decision ReviewDecision,
	actor, reason string,
) (ReviewResult, error) {
	out := ReviewResult{Decision: decision, IncidentID: incidentID, AlertID: alertID}

	err := s.tx.InTx(ctx, func(st Store) error {
		edge, err := st.GetReviewEdge(ctx, incidentID, alertID)
		if err != nil {
			return err
		}

		switch decision {
		case ReviewConfirm:
			ok, errConfirm := st.ConfirmReviewEdge(ctx, incidentID, alertID)
			if errConfirm != nil {
				return errConfirm
			}
			if !ok {
				return ErrReviewEdgeNotFound
			}
			return st.AppendTimelineEntry(ctx, NewReviewCommentEntry(
				incidentID, alertID, ReviewActionConfirm, actor, reason, s.now()))

		case ReviewReject:
			ok, errDelete := st.DeleteAlertEdge(ctx, incidentID, alertID)
			if errDelete != nil {
				return errDelete
			}
			if !ok {
				return ErrReviewEdgeNotFound
			}

			// 该告警若已无其他归属，把反规范化指针清掉，
			// 否则前端按 alerts.incident_id 会跳到一个它已不属于的事件。
			left, errCount := st.CountAlertEdges(ctx, alertID)
			if errCount != nil {
				return errCount
			}
			if left == 0 {
				if errClear := st.ClearAlertIncident(ctx, alertID); errClear != nil {
					return errClear
				}
				out.Unlinked = true
			}

			// 抑制：让同指纹的下一条告警不再被判为这个事件的 REVIEW 候选。
			if errSupp := st.AddSuppression(ctx, edge.Alert.Fingerprint, incidentID, actor, reason); errSupp != nil {
				return errSupp
			}
			out.Suppressed = true

			return st.AppendTimelineEntry(ctx, NewReviewCommentEntry(
				incidentID, alertID, ReviewActionReject, actor, reason, s.now()))

		default:
			return ErrInvalidReviewDecision
		}
	})
	if err != nil {
		return ReviewResult{}, err
	}
	return out, nil
}
