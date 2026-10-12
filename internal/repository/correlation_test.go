package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件对 M2 新增的两个仓储实现（Incidents / CorrelationConfig）做**真库**验证。
//
// 为什么必须有这一层：关联引擎的单测用的是内存假存储，只能证明编排逻辑对；
// 而 Incidents / CorrelationConfig 里的是手写 SQL（共现统计的 CTE、upsert、
// 部分索引回写），编译通过不代表 SQL 能跑。没有这层测试，一次列名写错
// 就要等到线上第一次关联时才暴露。

// corrBase 是关联用例共用的基准时刻。
var corrBase = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

// insertAlertRow 直插一条告警并返回主键。
//
// 刻意用裸 SQL 而非 NewAlerts：本文件的被测对象是 M2 仓储，
// 借道 M1 的 Insert 会引入额外的去重语义，让"插不进去"这类失败难以归因。
func insertAlertRow(t *testing.T, pool *pgxpool.Pool, fingerprint, status, labels string, startsAt time.Time, endsAt *time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO alerts (fingerprint, source, severity, status, labels, annotations, raw_payload, starts_at, ends_at)
VALUES ($1, 'alertmanager', 'critical', $2, $3::jsonb, '{}'::jsonb, '{}'::jsonb, $4, $5)
RETURNING id`, fingerprint, status, labels, startsAt, endsAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入告警 %s 失败: %v", fingerprint, err)
	}
	return id
}

// countRows 执行一次计数查询，用于断言库内行数。
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败 (%s): %v", sql, err)
	}
	return n
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------------------------------------------------------------------------
// CorrelationConfig（配置读写 + 历史共现）
// ---------------------------------------------------------------------------

// TestCorrelationConfigSeededDefaults：0003 迁移种子行必须能被读回，
// 且与内置 DefaultConfig 完全一致——否则"迁移种子"与"代码默认"会悄悄分叉。
func TestCorrelationConfigSeededDefaults(t *testing.T) {
	store := NewCorrelationConfig(testsupport.NewDB(t))

	cfg, err := store.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("读取种子配置失败: %v", err)
	}
	if cfg != correlation.DefaultConfig() {
		t.Fatalf("种子配置与内置默认不一致：\n实得 %+v\n期望 %+v", cfg, correlation.DefaultConfig())
	}
}

// TestCorrelationConfigFallbackWhenRowMissing：种子行被删掉时回退到内置默认值，
// 而不是报错——"库没种子"不该让关联引擎整体失效。
func TestCorrelationConfigFallbackWhenRowMissing(t *testing.T) {
	pool := testsupport.NewDB(t)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM correlation_rules WHERE name = 'default'`); err != nil {
		t.Fatalf("删除种子行失败: %v", err)
	}

	cfg, err := NewCorrelationConfig(pool).GetConfig(context.Background())
	if err != nil {
		t.Fatalf("缺行时应回退默认值而非报错，实得: %v", err)
	}
	if cfg != correlation.DefaultConfig() {
		t.Fatalf("应回退到内置默认值，实得 %+v", cfg)
	}
}

// TestCorrelationConfigPutIsUpsertSingleRow：反复写配置只能维持一行，
// 且 enabled 列与 rule_config 都要更新——这正是「改配置免重启生效」的基础。
func TestCorrelationConfigPutIsUpsertSingleRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewCorrelationConfig(pool)
	ctx := context.Background()

	first := correlation.DefaultConfig()
	first.Weights = correlation.Weights{Time: 0.30, Service: 0.30, Label: 0.15, Dependency: 0.15, Historical: 0.10}
	first.Thresholds = correlation.Thresholds{Merge: 0.75, Review: 0.45}
	first.Enabled = true
	if err := store.PutConfig(ctx, first); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}

	got, err := store.GetConfig(ctx)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got != first {
		t.Fatalf("写后读不一致：\n实得 %+v\n期望 %+v", got, first)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM correlation_rules WHERE name = 'default'`); n != 1 {
		t.Fatalf("默认配置行应恒为 1 行，实得 %d 行", n)
	}

	// 第二次写入：必须是 UPDATE 而非新增行。
	second := correlation.DefaultConfig()
	second.Enabled = false
	// 内层变量改名避免遮蔽上方 GetConfig 的 err（govet shadow）。
	if errPut := store.PutConfig(ctx, second); errPut != nil {
		t.Fatalf("二次写入失败: %v", errPut)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM correlation_rules WHERE name = 'default'`); n != 1 {
		t.Fatalf("二次写入不应新增行，实得 %d 行", n)
	}
	got2, err := store.GetConfig(ctx)
	if err != nil {
		t.Fatalf("二次读回失败: %v", err)
	}
	if got2 != second {
		t.Fatalf("二次写后读不一致：\n实得 %+v\n期望 %+v", got2, second)
	}
	// enabled 列的权威性：JSON 里的 enabled 也必须跟着变（本实现一并写入）。
	if got2.Enabled {
		t.Error("enabled=false 应被持久化")
	}
}

// TestCorrelationConfigEnabledColumnIsAuthoritative 覆盖一个容易踩的坑：
// 若有人只改 JSON 里的 enabled 而不同步列，读端必须以**列**为准。
func TestCorrelationConfigEnabledColumnIsAuthoritative(t *testing.T) {
	pool := testsupport.NewDB(t)
	ctx := context.Background()

	// 直接把列改成 false，同时把 JSON 里的 enabled 写成 true。
	if _, err := pool.Exec(ctx, `
UPDATE correlation_rules
SET enabled = FALSE,
    rule_config = jsonb_set(rule_config, '{enabled}', 'true'::jsonb)
WHERE name = 'default'`); err != nil {
		t.Fatalf("构造不一致状态失败: %v", err)
	}

	cfg, err := NewCorrelationConfig(pool).GetConfig(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("enabled 列是权威来源：列为 false 时读出的 Enabled 必须为 false")
	}
}

// TestCorrelationConfigCoOccurrence 是**手写 CTE 的专项验证**。
//
// 语义：co = 同时包含两个指纹的 Incident 数；total = 包含任一者的 Incident 数；
// 历史相似度 = co / total。SQL 写错（例如 COUNT 聚合写在内连接上）时，
// co/total 会退化成语义不明的数字，只有真库能发现。
func TestCorrelationConfigCoOccurrence(t *testing.T) {
	pool := testsupport.NewDB(t)
	store := NewCorrelationConfig(pool)
	incidents := NewIncidents(pool)
	ctx := context.Background()

	// 事件 X 含 A/B/C；事件 Y 只含 D。
	alertsX := []int64{
		insertAlertRow(t, pool, "fp-co-a", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil),
		insertAlertRow(t, pool, "fp-co-b", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil),
		insertAlertRow(t, pool, "fp-co-c", alert.StatusFiring, `{"service":"checkout"}`, corrBase, nil),
	}
	alertD := insertAlertRow(t, pool, "fp-co-d", alert.StatusFiring, `{"service":"payments"}`, corrBase, nil)

	incX, err := incidents.CreateIncident(ctx, correlation.Incident{
		Title: "X", Severity: alert.SeverityCritical, Service: "checkout", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件 X 失败: %v", err)
	}
	incY, err := incidents.CreateIncident(ctx, correlation.Incident{
		Title: "Y", Severity: alert.SeverityCritical, Service: "payments", StartedAt: corrBase,
	})
	if err != nil {
		t.Fatalf("建事件 Y 失败: %v", err)
	}
	for _, id := range alertsX {
		if err := incidents.LinkAlertToIncident(ctx, incX.ID, id, nil, false); err != nil {
			t.Fatalf("关联到 X 失败: %v", err)
		}
	}
	if err := incidents.LinkAlertToIncident(ctx, incY.ID, alertD, nil, false); err != nil {
		t.Fatalf("关联到 Y 失败: %v", err)
	}

	cases := []struct {
		name          string
		fpA, fpB      string
		wantCo, wantT float64
	}{
		{"同在一个事件内_共现满分", "fp-co-a", "fp-co-b", 1, 1},
		{"同事件但跨告警_仍共现", "fp-co-a", "fp-co-c", 1, 1},
		{"分属两个事件_共现为0_并集为2", "fp-co-a", "fp-co-d", 0, 2},
		{"其一从未出现_并集为1", "fp-co-a", "fp-never-seen", 0, 1},
		{"都从未出现_无历史", "fp-n1", "fp-n2", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			co, total, err := store.CoOccurrence(ctx, tc.fpA, tc.fpB)
			if err != nil {
				t.Fatalf("查询共现失败: %v", err)
			}
			if co != tc.wantCo || total != tc.wantT {
				t.Fatalf("共现 = (co=%v, total=%v)，期望 (co=%v, total=%v)", co, total, tc.wantCo, tc.wantT)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Incidents（建档 / 候选集 / 关联写入）
// ---------------------------------------------------------------------------

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
		if errLink := store.LinkAlertToIncident(ctx, inc.ID, id, nil, false); errLink != nil {
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
		if err := store.LinkAlertToIncident(ctx, incidentID, alertID, nil, false); err != nil {
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
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, &first, false); err != nil {
		t.Fatalf("首次关联失败: %v", err)
	}
	second := 0.60
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, &second, true); err != nil {
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

	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, nil, false); err != nil {
		t.Fatalf("NEW 档关联失败: %v", err)
	}
	if n := countRows(t, pool,
		`SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2 AND correlation_score IS NULL`,
		inc.ID, alertID); n != 1 {
		t.Fatal("NEW 档的 correlation_score 必须落 NULL")
	}
}

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
	if err := store.LinkAlertToIncident(ctx, inc.ID, alertID, &score, true); err != nil {
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

// TestListOpenIncidentAlertsPerIncidentLimit 覆盖 R7 候选限额的三条语义：
//
//	① 每个 incident 最多回 limit 条 —— 候选集 = Σ min(成员数, limit)，
//	   不再随告警量无上限膨胀（实测 1 万条时候选 7555 条，
//	   关联吞吐从 1785 条/s 崩到 ~4.5 条/s，见 internal/correlation 的
//	   CandidateLimitPerIncident 注释）；
//	② 保留的是**最近**的 limit 条（ROW_NUMBER 按 a.id 降序，id 自增）——
//	   时间窗/服务/标签形态由最近告警承载，取任意 limit 条会让评分依据过时数据；
//	③ limit<=0 不限额（回归对比用），且成员数 < limit 的 incident 必须全数
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
	for i := int32(0); i < limit+5; i++ {
		id := insertAlertRow(t, pool, fmt.Sprintf("fp-limit-big-%02d", i), alert.StatusFiring,
			`{"service":"checkout"}`, corrBase, nil)
		if errLink := store.LinkAlertToIncident(ctx, big.ID, id, nil, false); errLink != nil {
			t.Fatalf("关联到大事件失败: %v", errLink)
		}
		if i >= 5 {
			wantRecent = append(wantRecent, id)
		}
	}
	smallIDs := make(map[int64]bool, 3)
	for i := 0; i < 3; i++ {
		id := insertAlertRow(t, pool, fmt.Sprintf("fp-limit-small-%02d", i), alert.StatusFiring,
			`{"service":"payment"}`, corrBase, nil)
		if errLink := store.LinkAlertToIncident(ctx, small.ID, id, nil, false); errLink != nil {
			t.Fatalf("关联到小事件失败: %v", errLink)
		}
		smallIDs[id] = true
	}

	cands, err := store.ListOpenIncidentAlerts(ctx, limit)
	if err != nil {
		t.Fatalf("查询候选集失败: %v", err)
	}
	gotBig, gotSmall := 0, 0
	gotBigIDs := make(map[int64]bool, limit)
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
	if gotBig != int(limit) {
		t.Errorf("大事件应返回 %d 条（限额），实得 %d", limit, gotBig)
	}
	if gotSmall != 3 {
		t.Errorf("小事件应全数返回 3 条，实得 %d", gotSmall)
	}
	if len(gotBigIDs) != len(wantRecent) {
		t.Fatalf("大事件返回 %d 条但去重后 %d 条", gotBig, len(gotBigIDs))
	}
	for _, id := range wantRecent {
		if !gotBigIDs[id] {
			t.Errorf("大事件应保留较新的告警 %d，实得集合 %v", id, gotBigIDs)
		}
	}

	// ③ limit<=0 不限额。
	all, err := store.ListOpenIncidentAlerts(ctx, 0)
	if err != nil {
		t.Fatalf("limit=0 查询失败: %v", err)
	}
	if want := int(limit + 5 + 3); len(all) != want {
		t.Errorf("limit<=0 应不限额（期望 %d 条），实得 %d", want, len(all))
	}
}
