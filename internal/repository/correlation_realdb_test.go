package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/testsupport"
)

// TestCorrelateAlertsSkipsResolvedOnRealDB：resolved 告警在真实链路上也不建事件。
func TestCorrelateAlertsSkipsResolvedOnRealDB(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	svc := correlation.NewService(
		NewIncidents(pool), NewCorrelationConfig(pool), nil, discardLogger())

	endsAt := corrBase.Add(5 * time.Minute)
	id := insertAlertRow(t, pool, "fp-e2e-resolved", alert.StatusResolved,
		`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod"}`, corrBase, &endsAt)

	if err := svc.CorrelateAlerts(ctx, []alert.StoredAlert{{ID: id, Alert: alert.Alert{
		Fingerprint: "fp-e2e-resolved", Status: alert.StatusResolved, Severity: alert.SeverityWarning,
		Labels:   json.RawMessage(`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod"}`),
		StartsAt: corrBase, EndsAt: &endsAt,
	}}}); err != nil {
		t.Fatalf("关联失败: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incidents`); n != 0 {
		t.Fatalf("resolved 告警不应建事件，实得 %d 个", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incident_alerts`); n != 0 {
		t.Fatalf("resolved 告警不应建关联边，实得 %d 条", n)
	}
}

// TestCorrelationStoresOnEmptyDB：库刚迁移完、尚无任何告警与事件时，
// 两个仓储都必须给出「空」而不是报错——这是服务首次启动时的真实形态。
func TestCorrelationStoresOnEmptyDB(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	cands, err := NewIncidents(pool).ListOpenIncidentAlerts(ctx, correlation.CandidateLimitPerIncident)
	if err != nil {
		t.Fatalf("空库查询候选集不应报错: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("空库候选集应为空，实得 %d 条", len(cands))
	}

	co, total, err := NewCorrelationConfig(pool).CoOccurrence(ctx, "fp-x", "fp-y")
	if err != nil {
		t.Fatalf("空库查询共现不应报错: %v", err)
	}
	if co != 0 || total != 0 {
		t.Fatalf("空库共现应为 (0,0)，实得 (%v,%v)", co, total)
	}

	// 编译期接口断言的运行时兜底：仓储实例必须能直接赋给消费方接口。
	var _ correlation.IncidentStore = NewIncidents(pool)
	var _ correlation.ConfigStore = NewCorrelationConfig(pool)
}

// ---------------------------------------------------------------------------
// R7：候选集限额（真库）
// ---------------------------------------------------------------------------

// TestListOpenIncidentAlertsPerIncidentLimit 覆盖 R7 候选限额的四条语义：
//
//	① 每个 incident 最多回 limit 条 —— 候选集 = Σ min(成员数, limit)，
//	   不再随告警量无上限膨胀（实测 1 万条时候选 7555 条，
//	   关联吞吐从 1785 条/s 崩到 ~4.5 条/s，见 internal/correlation 的
//	   CandidateLimitPerIncident 注释）；
//	② 保留的是**最近**的 limit 条（ROW_NUMBER 按 a.id 降序，id 自增）——
//	   时间窗/服务/标签形态由最近告警承载，取任意 limit 条会让评分依据过时数据；
//	③ 种子锚定：超限事件的**首条告警（种子）恒在候选集**，故大事件实际返回
//	   limit+1 条。种子是事件身份的来源（标题/severity 由它派生），
//	   漏掉它会让「与事件同根因的新告警」失去合并依据（R7 收口补，
//	   见 internal/correlation 的异质探针）；
//	④ limit<=0 不限额（回归对比用），且成员数 < limit 的 incident 必须全数
//	   返回、不被误伤——只测「超限的被砍」的话，把 SQL 写成「每 incident
//	   只回 1 条」也能绿。
func TestListOpenIncidentAlertsPerIncidentLimit(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	big, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "大事件", Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建大事件失败: %v", err)
	}
	small, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "小事件", Severity: alert.SeverityCritical, Service: "payment", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建小事件失败: %v", err)
	}

	const limit = correlation.CandidateLimitPerIncident
	var wantRecent []int64 // 大事件中应被保留的「最近 limit 条」
	var bigSeedID int64    // 大事件的种子 = 首条关联的告警（建事件时 link 的那条）
	for i := int32(0); i < limit+5; i++ {
		id := insertAlertRow(t, pool, fmt.Sprintf("fp-limit-big-%02d", i), alert.StatusFiring,
			`{"service":"checkout"}`, corrBase, nil)
		if errLink := store.LinkAlertToIncident(ctx, big.ID, id, corrBase, nil, false); errLink != nil {
			t.Fatalf("关联到大事件失败: %v", errLink)
		}
		if i == 0 {
			bigSeedID = id
		}
		if i >= 5 {
			wantRecent = append(wantRecent, id)
		}
	}
	smallIDs := make(map[int64]bool, 3)
	for i := 0; i < 3; i++ {
		id := insertAlertRow(t, pool, fmt.Sprintf("fp-limit-small-%02d", i), alert.StatusFiring,
			`{"service":"payment"}`, corrBase, nil)
		if errLink := store.LinkAlertToIncident(ctx, small.ID, id, corrBase, nil, false); errLink != nil {
			t.Fatalf("关联到小事件失败: %v", errLink)
		}
		smallIDs[id] = true
	}

	cands, err := store.ListOpenIncidentAlerts(ctx, limit)
	if err != nil {
		t.Fatalf("查询候选集失败: %v", err)
	}
	gotBig, gotSmall := 0, 0
	gotBigIDs := make(map[int64]bool, limit+1)
	for _, c := range cands {
		switch c.IncidentID {
		case big.ID:
			gotBig++
			gotBigIDs[c.View.ID] = true
		case small.ID:
			gotSmall++
			if !smallIDs[c.View.ID] {
				t.Errorf("小事件返回了未关联给它的告警 %d", c.View.ID)
			}
		default:
			t.Errorf("出现非本次创建的事件 %d", c.IncidentID)
		}
	}
	// ①+③：大事件返回 limit 条最近 + 1 条种子 = limit+1。
	if want := int(limit) + 1; gotBig != want {
		t.Errorf("大事件应返回 %d 条（最近 %d + 种子），实得 %d", want, limit, gotBig)
	}
	if gotSmall != 3 {
		t.Errorf("小事件应全数返回 3 条，实得 %d", gotSmall)
	}
	if len(gotBigIDs) != len(wantRecent)+1 {
		t.Fatalf("大事件返回 %d 条但去重后 %d 条（期望 %d）", gotBig, len(gotBigIDs), len(wantRecent)+1)
	}
	for _, id := range wantRecent {
		if !gotBigIDs[id] {
			t.Errorf("大事件应保留较新的告警 %d，实得集合 %v", id, gotBigIDs)
		}
	}
	// ③ 种子必须恒在候选集里——这是 R7 收口补的核心断言。
	if !gotBigIDs[bigSeedID] {
		t.Errorf("大事件的种子告警 %d 必须恒在候选集（种子锚定），实得集合 %v", bigSeedID, gotBigIDs)
	}

	// ④ limit<=0 不限额。
	all, err := store.ListOpenIncidentAlerts(ctx, 0)
	if err != nil {
		t.Fatalf("limit=0 查询失败: %v", err)
	}
	if want := int(limit + 5 + 3); len(all) != want {
		t.Errorf("limit<=0 应不限额（期望 %d 条），实得 %d", want, len(all))
	}
}
