package incident

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件覆盖 Service.Detail 的**截断行为**（M3 验收清单第 7 条）：
// 超过 200 条时必须回 truncated=true，且回传的 alert_count 是**真总数**而非
// 截断后的切片长度。
//
// 2026-10-03 查漏补缺时发现这一条此前 0 覆盖，而它恰好是唯一会把
// 「只显示了前 200 条」说成「一共 200 条」的分支 —— 列表端用 SQL count(*)
// 是真值，详情端原先用 len(截断切片)，两个端点自相矛盾。
//
// 造不出 200+ 条真数据也没关系：本文件用假存储精确控制切片长度，
// 真库那一侧由 internal/repository/truncation_test.go 验 SQL 计数本身。

// makeAlertRows 造 n 条关联告警，其中每 3 条一条待确认。
func makeAlertRows(n int) []AlertLink {
	rows := make([]AlertLink, 0, n)
	for i := range n {
		rows = append(rows, AlertLink{
			AlertID:     int64(i + 1),
			Fingerprint: "fp-detail",
			NeedsReview: i%3 == 0,
		})
	}
	return rows
}

// intPtr 取 int 的指针（用于设置假存储的计数覆盖值）。
func intPtr(v int) *int { return &v }

// TestDetailTruncatesAlertsBeyondLimit 断言超过 200 条时截断且回传真总数。
//
// 这是核心用例：修复前 AlertCount 取 len(截断切片) = 200，
// 修复后必须是「实际条数」250。
func TestDetailTruncatesAlertsBeyondLimit(t *testing.T) {
	const total = 250
	store := &fakeStore{
		snap:                Snapshot{ID: 1, Status: StatusOpen},
		alertRows:           makeAlertRows(MaxIncidentAlerts + 1), // 201 -> 触发截断
		countAlertsOverride: intPtr(total),
		countNeedsOverride:  intPtr(84),
	}
	svc := NewService(&fakeTransactor{store: store})

	got, err := svc.Detail(context.Background(), 1)
	if err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}

	if !got.TruncatedAlerts {
		t.Error("201 条关联告警时 truncated.alerts 应为 true")
	}
	if len(got.Alerts) != MaxIncidentAlerts {
		t.Errorf("内联告警应截到 %d 条，实得 %d", MaxIncidentAlerts, len(got.Alerts))
	}
	// 这一条是本次修复的核心断言：真总数不是 200。
	if got.AlertCount != total {
		t.Errorf("AlertCount = %d，期望真总数 %d —— 用了截断后的切片长度，"+
			"前端会显示「%d 条关联告警」", got.AlertCount, total, got.AlertCount)
	}
	if got.NeedsReviewCount != 84 {
		t.Errorf("NeedsReviewCount = %d，期望真值 84（截断部分的待确认边不能漏）",
			got.NeedsReviewCount)
	}
}

// TestDetailNoTruncateReusesSliceStats 断言未截断时 AlertCount 直接取切片长度。
//
// 这里刻意不设 countAlertsOverride：假存储在无覆盖时返回 len(alertRows)，
// 与切片统计必然相等 —— 这条用例验的是「未截断时数值自洽且不报错」。
// 「不额外发 count 查询」由 TestDetailQueriesCountsOnlyWhenTruncated 单独断言
// （它靠统计调用次数，那才是能区分两种实现的判据）。
func TestDetailNoTruncateReusesSliceStats(t *testing.T) {
	store := &fakeStore{
		snap:      Snapshot{ID: 1, Status: StatusOpen},
		alertRows: makeAlertRows(5),
	}
	svc := NewService(&fakeTransactor{store: store})

	got, err := svc.Detail(context.Background(), 1)
	if err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}
	if got.TruncatedAlerts {
		t.Error("5 条关联告警不该报截断")
	}
	if got.AlertCount != 5 {
		t.Errorf("AlertCount = %d，期望 5", got.AlertCount)
	}
	wantNeeds := 0
	for i := range store.alertRows {
		if store.alertRows[i].NeedsReview {
			wantNeeds++
		}
	}
	if got.NeedsReviewCount != wantNeeds {
		t.Errorf("NeedsReviewCount = %d，期望 %d", got.NeedsReviewCount, wantNeeds)
	}
}

// countingStore 记录真计数方法被调用的次数。
// 嵌 **fakeTransactor**（不是 fakeStore）—— NewService 要的是 Transactor，
// 只有嵌它才同时具备 InTx 与全部 Store 方法。
type countingStore struct {
	*fakeTransactor
	countCalls      int
	needsCountCalls int
}

func (c *countingStore) CountAlerts(ctx context.Context, id int64) (int, error) {
	c.countCalls++
	return c.fakeTransactor.store.CountAlerts(ctx, id)
}

func (c *countingStore) CountNeedsReviewAlerts(ctx context.Context, id int64) (int, error) {
	c.needsCountCalls++
	return c.fakeTransactor.store.CountNeedsReviewAlerts(ctx, id)
}

// TestDetailQueriesCountsOnlyWhenTruncated 断言「未截断时不多查真计数」。
//
// 详情页是运维值守时最常打开的接口；每次都多两条 count 查询没有理由。
func TestDetailQueriesCountsOnlyWhenTruncated(t *testing.T) {
	base := &fakeStore{
		snap:      Snapshot{ID: 1, Status: StatusOpen},
		alertRows: makeAlertRows(3),
	}
	cs := &countingStore{fakeTransactor: &fakeTransactor{store: base}}
	svc := NewService(cs)

	if _, err := svc.Detail(context.Background(), 1); err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}
	if cs.countCalls != 0 {
		t.Errorf("未截断时不应为 AlertCount 额外查真计数，实际查了 %d 次", cs.countCalls)
	}
	if cs.needsCountCalls != 0 {
		t.Errorf("未截断时不应为 NeedsReviewCount 额外查真计数，实际查了 %d 次",
			cs.needsCountCalls)
	}

	// 换成超限场景：两个真计数都必须查，且各查一次（不是每条告警查一次）。
	base.alertRows = makeAlertRows(MaxIncidentAlerts + 1)
	base.countAlertsOverride = intPtr(500)
	base.countNeedsOverride = intPtr(200)
	if _, err := svc.Detail(context.Background(), 1); err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}
	if cs.countCalls != 1 {
		t.Errorf("截断时 AlertCount 真计数应恰查 1 次，实际 %d 次", cs.countCalls)
	}
	if cs.needsCountCalls != 1 {
		t.Errorf("截断时 NeedsReviewCount 真计数应恰查 1 次，实际 %d 次",
			cs.needsCountCalls)
	}
}

// TestDetailTruncatesTimelineBeyondLimit 同样验 timeline 侧的截断。
func TestDetailTruncatesTimelineBeyondLimit(t *testing.T) {
	rows := make([]Entry, 0, MaxTimelineRows+1)
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	for i := range MaxTimelineRows + 1 {
		rows = append(rows, Entry{
			IncidentID: 1,
			EventType:  EventUserComment,
			Timestamp:  base.Add(time.Duration(i) * time.Second),
		})
	}
	store := &fakeStore{
		snap:         Snapshot{ID: 1, Status: StatusOpen},
		alertRows:    makeAlertRows(2),
		timelineRows: rows,
	}
	svc := NewService(&fakeTransactor{store: store})

	got, err := svc.Detail(context.Background(), 1)
	if err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}
	if !got.TruncatedTimeline {
		t.Error("201 条 timeline 时 truncated.timeline 应为 true")
	}
	if len(got.Timeline) != MaxTimelineRows {
		t.Errorf("内联 timeline 应截到 %d 条，实得 %d", MaxTimelineRows, len(got.Timeline))
	}
}

// TestDetailCountFailureIsError 断言真计数失败必须报错而不是静默给 0。
//
// 静默返回 0 会让详情页显示「0 条关联告警」——
// 这正是本次修复要根治的那类「把不知道说成没有」。
//
// 只在**发生截断**时才注入故障：未截断时实现根本不去查真计数
// （切片长度就是真值），注入的故障不会被触发，断言会变成假失败。
func TestDetailCountFailureIsError(t *testing.T) {
	sentinel := errors.New("统计关联告警数失败")
	store := &fakeStore{
		snap:           Snapshot{ID: 1, Status: StatusOpen},
		alertRows:      makeAlertRows(MaxIncidentAlerts + 1), // 触发截断 -> 会查真计数
		countAlertsErr: sentinel,
	}
	svc := NewService(&fakeTransactor{store: store})

	if _, err := svc.Detail(context.Background(), 1); !errors.Is(err, sentinel) {
		t.Errorf("真计数失败应原样返回错误，实得 %v", err)
	}

	// 截断场景下 NeedsReviewCount 的真计数失败同样不能被吞。
	cs := &countingStore{fakeTransactor: &fakeTransactor{store: &fakeStore{
		snap:                Snapshot{ID: 1, Status: StatusOpen},
		alertRows:           makeAlertRows(MaxIncidentAlerts + 1),
		countAlertsOverride: intPtr(500),
		countNeedsErr:       sentinel,
	}}}
	svc2 := NewService(cs)
	if _, err := svc2.Detail(context.Background(), 1); !errors.Is(err, sentinel) {
		t.Errorf("截断时待确认真计数失败应返回错误，实得 %v", err)
	}
}
