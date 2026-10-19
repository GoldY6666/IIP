package repository

import (
	"context"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/testsupport"
)

func TestIncidentsCreateRoundTrip(t *testing.T) {
	store := NewIncidents(testsupport.NewDB(t))
	ctx := context.Background()

	got, err := store.CreateIncident(ctx, correlation.Incident{
		Title:     "LatencyHigh (checkout)",
		Severity:  alert.SeverityCritical,
		Service:   "checkout",
		StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	if got.ID == 0 {
		t.Fatal("应返回自增主键")
	}
	if got.Status != "open" {
		t.Errorf("新事件 status 应为 open，实得 %q", got.Status)
	}
	if got.Title != "LatencyHigh (checkout)" || got.Severity != alert.SeverityCritical || got.Service != "checkout" {
		t.Errorf("字段回读不一致: %+v", got)
	}
	if !got.StartedAt.Equal(corrBase) {
		t.Errorf("started_at = %v，期望 %v", got.StartedAt, corrBase)
	}

	// 尚无关联告警时，候选集必须为空而不是报错。
	cands, err := store.ListOpenIncidentAlerts(ctx, correlation.CandidateLimitPerIncident)
	if err != nil {
		t.Fatalf("查询候选集失败: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("未关联告警时候选集应为空，实得 %d 条", len(cands))
	}
}

// TestIncidentsEmptyServiceStoredAsNull：无 service 时必须落 NULL 而非空串，
// 否则后续按 service 分组的统计会把"没有服务"混进"服务名为空的字符串"。
func TestIncidentsEmptyServiceStoredAsNull(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)

	got, err := store.CreateIncident(context.Background(), correlation.Incident{
		Title: "无服务事件", Severity: alert.SeverityInfo, StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	if got.Service != "" {
		t.Errorf("空 service 读回应为空串，实得 %q", got.Service)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM incidents WHERE id = $1 AND service IS NULL`, got.ID); n != 1 {
		t.Fatal("空 service 必须以 SQL NULL 落库")
	}
}

// TestListOpenIncidentAlertsProjectsFields 覆盖候选集查询的字段投影：
// labels 解析、ends_at 的可空语义（仍 firing 时为 NULL）都要正确。
func TestListOpenIncidentAlertsProjectsFields(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	inc, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "LatencyHigh (checkout)", Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}

	endsAt := corrBase.Add(10 * time.Minute)
	firingID := insertAlertRow(t, pool, "fp-proj-firing", alert.StatusFiring,
		`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod","instance":"pod-a"}`, corrBase, nil)
	resolvedID := insertAlertRow(t, pool, "fp-proj-resolved", alert.StatusResolved,
		`{"alertname":"LatencyHigh","service":"payment","namespace":"prod"}`, corrBase, &endsAt)

	for _, id := range []int64{firingID, resolvedID} {
		// 内层变量改名避免遮蔽外层的 err（govet shadow）：遮蔽虽不影响本用例
		// 结果，但会让后续新增断言时误用外层 err，属真实的隐患。
		if errLink := store.LinkAlertToIncident(ctx, inc.ID, id, corrBase, nil, false); errLink != nil {
			t.Fatalf("关联告警 %d 失败: %v", id, errLink)
		}
	}

	cands, err := store.ListOpenIncidentAlerts(ctx, correlation.CandidateLimitPerIncident)
	if err != nil {
		t.Fatalf("查询候选集失败: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("应返回 2 条候选告警，实得 %d 条", len(cands))
	}
	if cands[0].IncidentID != inc.ID || cands[1].IncidentID != inc.ID {
		t.Fatalf("候选告警的 incident_id 应为 %d，实得 %d / %d", inc.ID, cands[0].IncidentID, cands[1].IncidentID)
	}

	byID := map[int64]correlation.AlertView{}
	for _, c := range cands {
		byID[c.View.ID] = c.View
	}

	firing, ok := byID[firingID]
	if !ok {
		t.Fatalf("候选集缺少 firing 告警 %d", firingID)
	}
	if firing.Service != "checkout" || firing.Namespace != "prod" {
		t.Errorf("service/namespace 应从 labels 派生，实得 %q / %q", firing.Service, firing.Namespace)
	}
	if firing.Labels["instance"] != "pod-a" {
		t.Errorf("labels 未完整投影: %v", firing.Labels)
	}
	if firing.Fingerprint != "fp-proj-firing" {
		t.Errorf("fingerprint = %q", firing.Fingerprint)
	}
	if firing.Severity != alert.SeverityCritical {
		t.Errorf("severity = %q", firing.Severity)
	}
	if !firing.StartsAt.Equal(corrBase) {
		t.Errorf("starts_at = %v，期望 %v", firing.StartsAt, corrBase)
	}
	if firing.EndsAt != nil {
		t.Errorf("仍 firing 的告警 ends_at 应为 nil，实得 %v", firing.EndsAt)
	}

	resolved, ok := byID[resolvedID]
	if !ok {
		t.Fatalf("候选集缺少 resolved 告警 %d", resolvedID)
	}
	if resolved.EndsAt == nil {
		t.Fatal("已 resolved 的告警 ends_at 不应为 nil")
	}
	// 精度到微秒：TIMESTAMPTZ 的微秒精度足够，不引入容差会让断言变成"测时钟"。
	if !resolved.EndsAt.Truncate(time.Microsecond).Equal(endsAt) {
		t.Errorf("ends_at = %v，期望 %v", resolved.EndsAt, endsAt)
	}
}

// TestListOpenIncidentAlertsExcludesClosedIncidents：已 resolved 的事件不再参与关联，
// investigating 仍参与（故障仍在处理中，新告警应能并入）。
func TestListOpenIncidentAlertsExcludesClosedIncidents(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	mk := func(title, status string) int64 {
		inc, err := store.CreateIncident(ctx, correlation.Incident{
			Title: title, Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
		})
		if err != nil {
			t.Fatalf("建事件 %s 失败: %v", title, err)
		}
		if status != "open" {
			if _, err := pool.Exec(ctx, `UPDATE incidents SET status = $2 WHERE id = $1`, inc.ID, status); err != nil {
				t.Fatalf("改事件 %s 状态失败: %v", title, err)
			}
		}
		return inc.ID
	}

	openID := mk("open 事件", "open")
	investigatingID := mk("investigating 事件", "investigating")
	resolvedID := mk("resolved 事件", "resolved")
	ignoredID := mk("ignored 事件", "ignored")

	link := func(incidentID int64, fp string) {
		alertID := insertAlertRow(t, pool, fp, alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil)
		if err := store.LinkAlertToIncident(ctx, incidentID, alertID, corrBase, nil, false); err != nil {
			t.Fatalf("关联失败: %v", err)
		}
	}
	link(openID, "fp-state-open")
	link(investigatingID, "fp-state-investigating")
	link(resolvedID, "fp-state-resolved")
	link(ignoredID, "fp-state-ignored")

	cands, err := store.ListOpenIncidentAlerts(ctx, correlation.CandidateLimitPerIncident)
	if err != nil {
		t.Fatalf("查询候选集失败: %v", err)
	}
	got := map[int64]bool{}
	for _, c := range cands {
		got[c.IncidentID] = true
	}
	if !got[openID] || !got[investigatingID] {
		t.Fatalf("open / investigating 事件应在候选集中，实得 %v", got)
	}
	if got[resolvedID] || got[ignoredID] {
		t.Fatalf("resolved / ignored 事件不应参与关联，实得 %v", got)
	}
}

// TestLinkAlertToIncidentIsIdempotentFirstWins：重复链接静默跳过，
// 且**首次的分数与标记保留**（后续投递不会把已决结果改写）。
func TestLinkAlertToIncidentIsIdempotentFirstWins(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	inc, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "幂等事件", Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	alertID := insertAlertRow(t, pool, "fp-idem", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil)

	first := 0.85
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, corrBase, &first, false); err != nil {
		t.Fatalf("首次关联失败: %v", err)
	}
	second := 0.60
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, corrBase, &second, true); err != nil {
		t.Fatalf("二次关联应静默跳过而非报错，实得: %v", err)
	}

	if n := countRows(t, pool, `SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2`,
		inc.ID, alertID); n != 1 {
		t.Fatalf("重复关联不应产生第二行，实得 %d 行", n)
	}

	var score float64
	var needsReview bool
	if err := pool.QueryRow(ctx,
		`SELECT correlation_score::float8, needs_review FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2`,
		inc.ID, alertID).Scan(&score, &needsReview); err != nil {
		t.Fatalf("读取关联边失败: %v", err)
	}
	if score != 0.85 {
		t.Errorf("首次分数应保留 0.85，实得 %v", score)
	}
	if needsReview {
		t.Error("首次未标记 needs_review，二次投递不应改写为 true")
	}
}

// TestLinkAlertToIncidentNullScoreForNewTier：NEW 档没有关联对象，
// correlation_score 必须落 NULL（列上有 CHECK 约束，写非法值会直接报错）。
func TestLinkAlertToIncidentNullScoreForNewTier(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewIncidents(pool)
	ctx := context.Background()

	inc, err := store.CreateIncident(ctx, correlation.Incident{
		Title: "新建档事件", Severity: alert.SeverityWarning, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件失败: %v", err)
	}
	alertID := insertAlertRow(t, pool, "fp-new-tier", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil)

	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, corrBase, nil, false); err != nil {
		t.Fatalf("NEW 档关联失败: %v", err)
	}
	if n := countRows(t, pool,
		`SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2 AND correlation_score IS NULL`,
		inc.ID, alertID); n != 1 {
		t.Fatal("NEW 档的 correlation_score 必须落 NULL")
	}
}
