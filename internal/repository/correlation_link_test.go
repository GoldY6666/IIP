package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/testsupport"
)

// TestLinkAlertToIncidentReviewTierPersistsScoreAndFlag：REVIEW 档两样都要落：
// 分数（DECIMAL(5,4)，四位小数）与 needs_review 标记。
func TestLinkAlertToIncidentReviewTierPersistsScoreAndFlag(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	inc, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "复核档事件", Severity: alert.SeverityCritical, Service: "payment", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	alertID := insertAlertRow(t, pool, "fp-review-tier", alert.StatusFiring, `{"service":"payment"}`, corrBase, nil)

	score := 0.655
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, corrBase, &score, true); err != nil {
		t.Fatalf("REVIEW 档关联失败: %v", err)
	}

	var got float64
	var needsReview bool
	if err := pool.QueryRow(ctx,
		`SELECT correlation_score::float8, needs_review FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2`,
		inc.ID, alertID).Scan(&got, &needsReview); err != nil {
		t.Fatalf("读取关联边失败: %v", err)
	}
	if got != 0.6550 {
		t.Errorf("分数应落 0.6550（DECIMAL(5,4)），实得 %v", got)
	}
	if !needsReview {
		t.Error("REVIEW 档必须落 needs_review = true")
	}
	// 0003 建的部分索引只索引 needs_review 为真的行，这里顺带确认查询计划可用。
	if n := countRows(t, pool,
		`SELECT count(*) FROM incident_alerts WHERE needs_review AND incident_id = $1`, inc.ID); n != 1 {
		t.Errorf("按 needs_review 过滤应命中 1 行，实得 %d 行", n)
	}
}

func TestSetAlertIncidentWritesBackPointer(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	inc, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "回写指针事件", Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	alertID := insertAlertRow(t, pool, "fp-backref", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil)

	if err := store.SetAlertIncident(ctx, alertID, inc.ID); err != nil {
		t.Fatalf("回写 incident_id 失败: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM alerts WHERE id = $1 AND incident_id = $2`,
		alertID, inc.ID); n != 1 {
		t.Fatal("alerts.incident_id 未正确回写")
	}

	// 目标告警不存在时应成功执行但影响 0 行（不报错）——调用方靠后续查询发现。
	if err := store.SetAlertIncident(ctx, 999999999, inc.ID); err != nil {
		t.Errorf("回写不存在的告警不应报错，实得: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 端到端：关联服务 + 真库仓储
// ---------------------------------------------------------------------------

// TestCorrelateAlertsEndToEnd 把 correlation.Service 接到真实仓储上跑完整链路：
// 落库告警 → 读配置 → 取候选 → 打分决策 → 建事件/建关联边/回写指针。
//
// 这一条是 M2 最关键的集成证据：它同时验证了手写的三条 SQL
// （CreateIncident / ListOpenIncidentsWithAlerts / LinkAlertToIncident）、
// 配置读取、以及编排逻辑在真实数据上的一致性。
func TestCorrelateAlertsEndToEnd(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	incidentStore := NewIncidents(pool)
	cfgStore := NewCorrelationConfig(pool)

	topo := &correlation.Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}, "payment": {"mysql"}})

	svc := correlation.NewService(incidentStore, cfgStore, topo, discardLogger())

	// 两条同命名空间、时间相邻的级联告警（checkout → payment）。
	startsA := corrBase
	endsA := corrBase.Add(10 * time.Minute)
	startsB := corrBase.Add(1 * time.Minute)
	endsB := corrBase.Add(11 * time.Minute)

	idA := insertAlertRow(t, pool, "fp-e2e-a", alert.StatusFiring,
		`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod","instance":"pod-a"}`, startsA, &endsA)
	idB := insertAlertRow(t, pool, "fp-e2e-b", alert.StatusFiring,
		`{"alertname":"LatencyHigh","service":"payment","namespace":"prod","instance":"pod-b"}`, startsB, &endsB)

	batch := []alert.StoredAlert{
		{ID: idA, Alert: alert.Alert{
			Fingerprint: "fp-e2e-a", Status: alert.StatusFiring, Severity: alert.SeverityCritical,
			Labels:   json.RawMessage(`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod","instance":"pod-a"}`),
			StartsAt: startsA, EndsAt: &endsA,
		}},
		{ID: idB, Alert: alert.Alert{
			Fingerprint: "fp-e2e-b", Status: alert.StatusFiring, Severity: alert.SeverityCritical,
			Labels:   json.RawMessage(`{"alertname":"LatencyHigh","service":"payment","namespace":"prod","instance":"pod-b"}`),
			StartsAt: startsB, EndsAt: &endsB,
		}},
	}
	if err := svc.CorrelateAlerts(ctx, batch); err != nil {
		t.Fatalf("关联失败: %v", err)
	}

	// 恰建 1 个事件。
	if n := countRows(t, pool, `SELECT count(*) FROM incidents`); n != 1 {
		t.Fatalf("应恰好建 1 个事件，实得 %d 个", n)
	}
	var incidentID int64
	var title, severity, status, service string
	if err := pool.QueryRow(ctx,
		`SELECT id, title, severity, status, service FROM incidents`).
		Scan(&incidentID, &title, &severity, &status, &service); err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	if status != "open" {
		t.Errorf("事件状态应为 open，实得 %q", status)
	}
	if service != "checkout" {
		t.Errorf("event service 应取自首条告警的 labels.service = checkout，实得 %q", service)
	}
	if title != "LatencyHigh (checkout)" {
		t.Errorf("事件标题 = %q，期望 %q", title, "LatencyHigh (checkout)")
	}
	if severity != alert.SeverityCritical {
		t.Errorf("事件 severity = %q", severity)
	}

	// 两条关联边：首条 NEW（score NULL），次条 REVIEW（score 0.6550 + needs_review）。
	if n := countRows(t, pool, `SELECT count(*) FROM incident_alerts WHERE incident_id = $1`, incidentID); n != 2 {
		t.Fatalf("应有 2 条关联边，实得 %d 条", n)
	}
	var newCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND correlation_score IS NULL AND NOT needs_review`,
		incidentID).Scan(&newCount); err != nil {
		t.Fatalf("统计 NEW 档关联边失败: %v", err)
	}
	if newCount != 1 {
		t.Errorf("NEW 档关联边应为 1 条（无分数、不待复核），实得 %d 条", newCount)
	}

	var score float64
	if err := pool.QueryRow(ctx,
		`SELECT correlation_score::float8 FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2`,
		incidentID, idB).Scan(&score); err != nil {
		t.Fatalf("读取 B 的关联分数失败: %v", err)
	}
	// 时间 0.9 × .25 + 服务 0.6 × .25 + 标签 0.5 × .20 + 依赖 0.9 × .20 = 0.655
	if score != 0.6550 {
		t.Errorf("B 的关联分数 = %v，期望 0.6550", score)
	}

	// 两条告警的 incident_id 都要回写。
	if n := countRows(t, pool,
		`SELECT count(*) FROM alerts WHERE id IN ($1, $2) AND incident_id = $3`, idA, idB, incidentID); n != 2 {
		t.Fatalf("两条告警的 incident_id 都应回写为 %d，实际命中 %d 条", incidentID, n)
	}

	// 再来一条同类告警（同事件内的第三条）→ 应并入既有事件，不新建。
	idC := insertAlertRow(t, pool, "fp-e2e-c", alert.StatusFiring,
		`{"alertname":"LatencyHigh","service":"mysql","namespace":"prod","instance":"pod-c"}`, startsB, &endsB)
	if err := svc.CorrelateAlerts(ctx, []alert.StoredAlert{{ID: idC, Alert: alert.Alert{
		Fingerprint: "fp-e2e-c", Status: alert.StatusFiring, Severity: alert.SeverityCritical,
		Labels:   json.RawMessage(`{"alertname":"LatencyHigh","service":"mysql","namespace":"prod","instance":"pod-c"}`),
		StartsAt: startsB, EndsAt: &endsB,
	}}}); err != nil {
		t.Fatalf("第二次关联失败: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incidents`); n != 1 {
		t.Fatalf("同源告警应并入既有事件，事件数仍为 1，实得 %d", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incident_alerts WHERE incident_id = $1`, incidentID); n != 3 {
		t.Fatalf("既有事件应累计 3 条关联边，实得 %d 条", n)
	}

	// 一条完全无关的告警 → 应新建第二个事件。
	unrelatedStart := corrBase.Add(3 * time.Hour)
	unrelatedEnd := unrelatedStart.Add(5 * time.Minute)
	idD := insertAlertRow(t, pool, "fp-e2e-d", alert.StatusFiring,
		`{"alertname":"DiskFull","service":"search","namespace":"staging","instance":"pod-d"}`, unrelatedStart, &unrelatedEnd)
	if err := svc.CorrelateAlerts(ctx, []alert.StoredAlert{{ID: idD, Alert: alert.Alert{
		Fingerprint: "fp-e2e-d", Status: alert.StatusFiring, Severity: alert.SeverityWarning,
		Labels:   json.RawMessage(`{"alertname":"DiskFull","service":"search","namespace":"staging","instance":"pod-d"}`),
		StartsAt: unrelatedStart, EndsAt: &unrelatedEnd,
	}}}); err != nil {
		t.Fatalf("第三次关联失败: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incidents`); n != 2 {
		t.Fatalf("无关告警应新建事件，事件数应为 2，实得 %d", n)
	}

	// 引擎关闭后不再产生新事件（配置读的是库，因此本条同时验证了"免重启生效"）。
	disabled := correlation.DefaultConfig()
	disabled.Enabled = false
	if err := cfgStore.PutConfig(ctx, disabled); err != nil {
		t.Fatalf("关闭引擎失败: %v", err)
	}
	idE := insertAlertRow(t, pool, "fp-e2e-e", alert.StatusFiring,
		`{"alertname":"DiskFull","service":"search","namespace":"staging","instance":"pod-e"}`, unrelatedStart, &unrelatedEnd)
	if err := svc.CorrelateAlerts(ctx, []alert.StoredAlert{{ID: idE, Alert: alert.Alert{
		Fingerprint: "fp-e2e-e", Status: alert.StatusFiring, Severity: alert.SeverityWarning,
		Labels:   json.RawMessage(`{"alertname":"DiskFull","service":"search","namespace":"staging","instance":"pod-e"}`),
		StartsAt: unrelatedStart, EndsAt: &unrelatedEnd,
	}}}); err != nil {
		t.Fatalf("关闭后关联不应报错，实得: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incidents`); n != 2 {
		t.Fatalf("引擎关闭后不应新建事件，事件数应为 2，实得 %d", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM alerts WHERE id = $1 AND incident_id IS NULL`, idE); n != 1 {
		t.Fatal("引擎关闭时不应回写 incident_id")
	}
}
