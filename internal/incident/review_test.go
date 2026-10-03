package incident

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件覆盖人工复核的两条路径（confirm / reject）在**事务内**做了什么。
// 真库上的边操作由 internal/repository 覆盖。

// newReviewService 组装带固定时钟与一条待确认边的服务。
func newReviewService() (*Service, *fakeTransactor) {
	store := &fakeStore{
		reviewEdge: ReviewEdge{
			IncidentID: 7,
			Alert: AlertLink{
				AlertID:     101,
				Fingerprint: "fp-review",
				NeedsReview: true,
			},
		},
		confirmOK:      true,
		deleteOK:       true,
		alertEdgeCount: 0, // 默认：删掉这条边后该告警无其他归属
	}
	tx := &fakeTransactor{store: store}
	svc := NewService(tx)
	svc.now = func() time.Time { return fixedReviewClock }
	return svc, tx
}

var fixedReviewClock = time.Date(2026, 10, 2, 22, 0, 0, 0, time.UTC)

// TestReviewConfirmKeepsEdge 断言确认保留关联边、只清标记、并留痕。
func TestReviewConfirmKeepsEdge(t *testing.T) {
	svc, tx := newReviewService()

	res, err := svc.Review(context.Background(), 7, 101, ReviewConfirm, "yu", "确实相关")
	if err != nil {
		t.Fatalf("复核失败: %v", err)
	}
	if tx.store.confirmCalls != 1 {
		t.Errorf("应调用 1 次确认，实得 %d", tx.store.confirmCalls)
	}
	if tx.store.deleteCalls != 0 {
		t.Errorf("确认不该删除关联边，实得 %d 次删除", tx.store.deleteCalls)
	}
	if len(tx.store.suppressions) != 0 {
		t.Errorf("确认不该写抑制，实得 %v", tx.store.suppressions)
	}
	if res.Suppressed || res.Unlinked {
		t.Errorf("确认结果不应带 suppressed/unlinked，实得 %+v", res)
	}

	if len(tx.store.entries) != 1 {
		t.Fatalf("应写 1 条留痕，实得 %d", len(tx.store.entries))
	}
	e := tx.store.entries[0]
	if e.EventType != EventUserComment {
		t.Errorf("事件类型 = %q，期望 %q", e.EventType, EventUserComment)
	}
	if e.Metadata["action"] != ReviewActionConfirm {
		t.Errorf("metadata.action = %q，期望 %q", e.Metadata["action"], ReviewActionConfirm)
	}
	if !e.Timestamp.Equal(fixedReviewClock) {
		t.Errorf("留痕时间 = %v，期望注入时钟 %v", e.Timestamp, fixedReviewClock)
	}
}

// TestReviewRejectUnlinksAndSuppresses 断言拒绝做全四件事：删边 / 清指针 / 写抑制 / 留痕。
func TestReviewRejectUnlinksAndSuppresses(t *testing.T) {
	svc, tx := newReviewService()

	res, err := svc.Review(context.Background(), 7, 101, ReviewReject, "yu", "不是同一个故障")
	if err != nil {
		t.Fatalf("复核失败: %v", err)
	}
	if tx.store.deleteCalls != 1 {
		t.Errorf("应删除 1 条关联边，实得 %d 次", tx.store.deleteCalls)
	}
	if len(tx.store.clearedAlerts) != 1 || tx.store.clearedAlerts[0] != 101 {
		t.Errorf("告警已无其他归属，应清除其 incident_id，实得 %v", tx.store.clearedAlerts)
	}
	if len(tx.store.suppressions) != 1 || tx.store.suppressions[0] != "fp-review#7" {
		t.Errorf("应写入抑制 fp-review#7，实得 %v", tx.store.suppressions)
	}
	if !res.Suppressed || !res.Unlinked {
		t.Errorf("结果应标记 suppressed/unlinked，实得 %+v", res)
	}

	if len(tx.store.entries) != 1 {
		t.Fatalf("应写 1 条留痕，实得 %d", len(tx.store.entries))
	}
	if tx.store.entries[0].Metadata["action"] != ReviewActionReject {
		t.Errorf("metadata.action = %q，期望 %q", tx.store.entries[0].Metadata["action"], ReviewActionReject)
	}
	if tx.store.entries[0].Metadata["reason"] != "不是同一个故障" {
		t.Errorf("拒绝理由应写入 metadata，实得 %q", tx.store.entries[0].Metadata["reason"])
	}
}

// TestReviewRejectKeepsPointerWhenOtherEdgesRemain 断言该告警还有别的归属时不清除指针。
//
// 清早了会让前端按 alerts.incident_id 跳不到任何地方——告警明明还属于另一个事件。
func TestReviewRejectKeepsPointerWhenOtherEdgesRemain(t *testing.T) {
	svc, tx := newReviewService()
	tx.store.alertEdgeCount = 2

	res, err := svc.Review(context.Background(), 7, 101, ReviewReject, "yu", "")
	if err != nil {
		t.Fatalf("复核失败: %v", err)
	}
	if len(tx.store.clearedAlerts) != 0 {
		t.Errorf("仍有其他关联边时不应清除 incident_id，实得 %v", tx.store.clearedAlerts)
	}
	if res.Unlinked {
		t.Error("仍有其他关联边时 Unlinked 应为 false")
	}
	// 抑制与留痕仍要写：这次判定本身是有效的。
	if len(tx.store.suppressions) != 1 || len(tx.store.entries) != 1 {
		t.Errorf("仍应写抑制与留痕：suppressions=%v entries=%d", tx.store.suppressions, len(tx.store.entries))
	}
}

// TestReviewEdgeNotFound 断言边不存在时不产生任何写入。
func TestReviewEdgeNotFound(t *testing.T) {
	svc, tx := newReviewService()
	tx.store.reviewErr = ErrReviewEdgeNotFound

	_, err := svc.Review(context.Background(), 7, 101, ReviewReject, "yu", "")
	if !errors.Is(err, ErrReviewEdgeNotFound) {
		t.Fatalf("期望 ErrReviewEdgeNotFound，实得: %v", err)
	}
	if tx.store.deleteCalls != 0 || len(tx.store.suppressions) != 0 || len(tx.store.entries) != 0 {
		t.Error("边不存在时不应有任何写入")
	}
}

// TestReviewInvalidDecision 断言非法动作被拒（防御内部调用方写错）。
func TestReviewInvalidDecision(t *testing.T) {
	svc, tx := newReviewService()

	if _, err := svc.Review(context.Background(), 7, 101, "maybe", "yu", ""); !errors.Is(err, ErrInvalidReviewDecision) {
		t.Fatalf("期望 ErrInvalidReviewDecision，实得: %v", err)
	}
	if tx.store.deleteCalls != 0 || tx.store.confirmCalls != 0 {
		t.Error("非法动作不应产生任何写入")
	}
}

// TestListReviewsHasMore 断言队列同样用 limit+1 探测。
func TestListReviewsHasMore(t *testing.T) {
	store := &fakeStore{reviewEdges: []ReviewEdge{
		{IncidentID: 1, Alert: AlertLink{AlertID: 1}},
		{IncidentID: 2, Alert: AlertLink{AlertID: 2}},
		{IncidentID: 3, Alert: AlertLink{AlertID: 3}},
	}}
	tx := &fakeTransactor{store: store}
	svc := NewService(tx)

	got, err := svc.ListReviews(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("ListReviews 失败: %v", err)
	}
	if len(got.Items) != 2 || !got.HasMore {
		t.Errorf("应返回 2 条且 has_more=true，实得 %d 条 / %v", len(got.Items), got.HasMore)
	}
	// 探测：交给仓储的 limit 必须是 limit+1。
	if store.listReviewArgs == 0 {
		t.Error("未调用仓储")
	}
}

// TestListReviewsClampsPaging 断言队列的分页参数同样被夹紧。
func TestListReviewsClampsPaging(t *testing.T) {
	store := &fakeStore{}
	tx := &fakeTransactor{store: store}
	svc := NewService(tx)

	if _, err := svc.ListReviews(context.Background(), 99999, -5); err != nil {
		t.Fatalf("脏参数不应报错: %v", err)
	}
	if store.listReviewArgs == 0 {
		t.Error("未调用仓储")
	}
}

// TestParseReviewDecision 覆盖解析的正反例。
func TestParseReviewDecision(t *testing.T) {
	for raw, want := range map[string]ReviewDecision{
		"confirm": ReviewConfirm,
		"reject":  ReviewReject,
	} {
		got, err := ParseReviewDecision(raw)
		if err != nil {
			t.Errorf("ParseReviewDecision(%q) 不应报错: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseReviewDecision(%q) = %q，期望 %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "CONFIRM", "yes", "delete"} {
		if _, err := ParseReviewDecision(raw); err == nil {
			t.Errorf("ParseReviewDecision(%q) 应报错，实际通过", raw)
		}
	}
}
