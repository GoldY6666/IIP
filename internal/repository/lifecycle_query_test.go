package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 S2 新增的**读侧 SQL**与 REVIEW 写侧 SQL 的真库行为：
// ListIncidents 的过滤/排序/聚合计数、ListIncidentAlertsByIncident 的字段投影、
// 以及 REVIEW 队列的「查—确认—拒绝」四张表联动。
//
// 为什么不能只靠内存假存储：这些都是手写 SQL（= ANY 数组兜底、narg 的 NULL 分支、
// LEFT JOIN 子查询聚合、:execrows 的乐观条件），编译通过完全不代表能跑，
// 列名写错要等到线上第一次打开详情页才暴露。

var queryBase = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// insertIncidentFull 插入一条字段可控的事件，返回主键。
func insertIncidentFull(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	title, severity, status, service string, startedAt time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO incidents (title, severity, status, service, started_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		title, severity, status, service, startedAt).Scan(&id)
	if err != nil {
		t.Fatalf("插入事件 %s 失败: %v", title, err)
	}
	return id
}

// linkAlert 直接插一条关联边。
//
// score 为 nil 表示 NEW 档（列写 NULL）。numeric 列用文本 + 显式 cast 写入，
// 避免 Go float64 与 PG numeric 之间的隐式转换歧义。
func linkAlert(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	incidentID, alertID int64, score *float64, needsReview bool, correlatedAt time.Time) {
	t.Helper()
	var scoreVal any
	if score != nil {
		scoreVal = fmt.Sprintf("%.4f", *score)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO incident_alerts (incident_id, alert_id, correlation_score, needs_review, correlated_at)
		 VALUES ($1, $2, $3::numeric, $4, $5)`,
		incidentID, alertID, scoreVal, needsReview, correlatedAt); err != nil {
		t.Fatalf("插入关联边 (%d,%d) 失败: %v", incidentID, alertID, err)
	}
}

// setAlertIncident 手工回写反规范化指针，模拟 M2 关联后的落库状态。
func setAlertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, alertID, incidentID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE alerts SET incident_id = $2 WHERE id = $1`, alertID, incidentID); err != nil {
		t.Fatalf("回写 alerts.incident_id 失败: %v", err)
	}
}

// alertIncident 读回告警当前的 incident_id（NULL 返回 nil）。
func alertIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, alertID int64) *int64 {
	t.Helper()
	var id *int64
	if err := pool.QueryRow(ctx, `SELECT incident_id FROM alerts WHERE id = $1`, alertID).Scan(&id); err != nil {
		t.Fatalf("读取 alerts.incident_id 失败: %v", err)
	}
	return id
}

// --- 列表 ---

// TestLifecycleListFiltersAndCounts 覆盖手册 §4.3 的四个过滤维度、排序口径与两个聚合计数。
func TestLifecycleListFiltersAndCounts(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	// 三条事件：service / severity / status / 起始时刻各不相同。
	a := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	b := insertIncidentFull(t, ctx, pool, "payment 超时", "warning", "investigating", "payment", queryBase.Add(time.Hour))
	c := insertIncidentFull(t, ctx, pool, "search 5xx", "info", "resolved", "search", queryBase.Add(-time.Hour))

	// A 挂两条告警（其中一条待确认），B 挂一条，C 不挂——验证 LEFT JOIN 的 0 计数。
	a1 := insertAlertRow(t, pool, "fp-list-a1", "firing", `{"service":"checkout"}`, queryBase, nil)
	a2 := insertAlertRow(t, pool, "fp-list-a2", "firing", `{"service":"checkout"}`, queryBase, nil)
	b1 := insertAlertRow(t, pool, "fp-list-b1", "firing", `{"service":"payment"}`, queryBase, nil)
	linkAlert(t, ctx, pool, a, a1, nil, false, queryBase)
	score := 0.65
	linkAlert(t, ctx, pool, a, a2, &score, true, queryBase)
	linkAlert(t, ctx, pool, b, b1, nil, false, queryBase)

	all := incident.AllStatuses()

	// 默认（不过滤）：按 started_at DESC, id DESC —— B(10:00) > A(09:00) > C(08:00)。
	got, err := repo.List(ctx, incident.ListFilter{
		Statuses:   all,
		Severities: incident.AllSeverities(),
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("条数 = %d，期望 3", len(got))
	}
	if got[0].ID != b || got[1].ID != a || got[2].ID != c {
		t.Errorf("排序 = %d,%d,%d，期望 %d,%d,%d（started_at DESC）", got[0].ID, got[1].ID, got[2].ID, b, a, c)
	}
	for _, s := range got {
		switch s.ID {
		case a:
			if s.AlertCount != 2 || s.NeedsReviewCount != 1 {
				t.Errorf("A 的计数 = %d/%d，期望 2/1", s.AlertCount, s.NeedsReviewCount)
			}
		case b:
			if s.AlertCount != 1 || s.NeedsReviewCount != 0 {
				t.Errorf("B 的计数 = %d/%d，期望 1/0", s.AlertCount, s.NeedsReviewCount)
			}
		case c:
			// 没有关联边的事件必须还在列表里（LEFT JOIN 而非 INNER JOIN）。
			if s.AlertCount != 0 || s.NeedsReviewCount != 0 {
				t.Errorf("C 的计数 = %d/%d，期望 0/0", s.AlertCount, s.NeedsReviewCount)
			}
		}
	}

	// status 过滤：数组兜底口径要求「不过滤 = 传全部合法取值」，故只传一个就只出一个。
	got, err = repo.List(ctx, incident.ListFilter{
		Statuses:   []incident.Status{incident.StatusOpen},
		Severities: incident.AllSeverities(),
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("按 status 过滤失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != a {
		t.Errorf("status=open 的结果 = %v，期望只含 %d", ids(got), a)
	}

	// severity 过滤。
	got, err = repo.List(ctx, incident.ListFilter{
		Statuses:   all,
		Severities: []string{"critical"},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("按 severity 过滤失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != a {
		t.Errorf("severity=critical 的结果 = %v，期望只含 %d", ids(got), a)
	}

	// service 过滤（narg 的 NULL 分支：不传即不限，传了才生效）。
	got, err = repo.List(ctx, incident.ListFilter{
		Statuses:   all,
		Severities: incident.AllSeverities(),
		Service:    "payment",
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("按 service 过滤失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != b {
		t.Errorf("service=payment 的结果 = %v，期望只含 %d", ids(got), b)
	}

	// 时间窗：from 含边界。
	from := queryBase
	got, err = repo.List(ctx, incident.ListFilter{
		Statuses:   all,
		Severities: incident.AllSeverities(),
		From:       &from,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("按 from 过滤失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("from=%v 的结果 = %v，期望 2 条（8:00 的 C 应被排除）", from, ids(got))
	}

	to := queryBase
	got, err = repo.List(ctx, incident.ListFilter{
		Statuses:   all,
		Severities: incident.AllSeverities(),
		To:         &to,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("按 to 过滤失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("to=%v 的结果 = %v，期望 2 条（10:00 的 B 应被排除）", to, ids(got))
	}
}

// TestLifecycleListPaging 断言 limit / offset 真的下推到 SQL，而不是查完全量再切。
func TestLifecycleListPaging(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		insertIncidentFull(t, ctx, pool, fmt.Sprintf("事件-%d", i), "warning", "open",
			"checkout", queryBase.Add(time.Duration(i)*time.Minute))
	}

	filter := incident.ListFilter{
		Statuses:   incident.AllStatuses(),
		Severities: incident.AllSeverities(),
	}
	filter.Limit, filter.Offset = 2, 0
	got, err := repo.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(limit=2) 失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("第一页条数 = %d，期望 2", len(got))
	}

	filter.Offset = 2
	got, err = repo.List(ctx, filter)
	if err != nil {
		t.Fatalf("List(offset=2) 失败: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("第二页条数 = %d，期望 1", len(got))
	}
}

// --- 详情内联列表 ---

// TestLifecycleAlertsProjection 断言详情页关联告警的字段投影，
// 特别是「NEW 档 score 为 NULL」与「REVIEW 档 needs_review 为真」两种取值。
func TestLifecycleAlertsProjection(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	// NEW 档：score 为 NULL。
	newAlert := insertAlertRow(t, pool, "fp-detail-new", "firing", `{"service":"checkout","shard":"s1"}`, queryBase, nil)
	// REVIEW 档：有分数且待确认。
	revAlert := insertAlertRow(t, pool, "fp-detail-rev", "firing", `{"service":"checkout","shard":"s2"}`, queryBase, nil)
	linkAlert(t, ctx, pool, inc, newAlert, nil, false, queryBase)
	score := 0.6500
	linkAlert(t, ctx, pool, inc, revAlert, &score, true, queryBase.Add(time.Minute))

	got, err := repo.Alerts(ctx, inc, 10)
	if err != nil {
		t.Fatalf("Alerts 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("条数 = %d，期望 2", len(got))
	}
	// 排序是 correlated_at DESC：后关联的 REVIEW 档在前。
	if got[0].AlertID != revAlert {
		t.Fatalf("首条 = %d，期望 %d（correlated_at DESC）", got[0].AlertID, revAlert)
	}
	if !got[0].NeedsReview {
		t.Error("REVIEW 档的 needs_review 应为真")
	}
	if got[0].CorrelationScore == nil || *got[0].CorrelationScore != 0.65 {
		t.Errorf("REVIEW 档 score = %v，期望 0.65", got[0].CorrelationScore)
	}
	if got[0].Fingerprint != "fp-detail-rev" {
		t.Errorf("fingerprint = %s，期望 fp-detail-rev", got[0].Fingerprint)
	}
	if got[0].Labels["shard"] != "s2" {
		t.Errorf("labels = %v，应含 shard=s2", got[0].Labels)
	}
	if got[0].EndsAt != nil {
		t.Errorf("仍 firing 的告警 ends_at 应为 nil，实际 %v", got[0].EndsAt)
	}

	// NEW 档：score 必须是 nil（前端据此显示「—」而不是 0）。
	if got[1].CorrelationScore != nil {
		t.Errorf("NEW 档 score = %v，期望 nil", *got[1].CorrelationScore)
	}
	if got[1].NeedsReview {
		t.Error("NEW 档的 needs_review 应为假")
	}

	// limit 生效。
	got, err = repo.Alerts(ctx, inc, 1)
	if err != nil {
		t.Fatalf("Alerts(limit=1) 失败: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("limit=1 时条数 = %d，期望 1", len(got))
	}
}

// TestLifecycleTimelineLimit 断言 timeine 读取受 limit 约束（详情按 200 上限截断）。
func TestLifecycleTimelineLimit(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "payment 超时", "warning", "open", "payment", queryBase)
	for i := 0; i < 3; i++ {
		if err := repo.AppendTimelineEntry(ctx, incident.Entry{
			IncidentID:  inc,
			EventType:   incident.EventUserComment,
			Description: fmt.Sprintf("备注 %d", i),
			Actor:       "yu",
			Timestamp:   queryBase.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("写入第 %d 条留痕失败: %v", i, err)
		}
	}

	got, err := repo.Timeline(ctx, inc, 2)
	if err != nil {
		t.Fatalf("Timeline 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("条数 = %d，期望 2", len(got))
	}
	// 倒序：最晚写的「备注 2」在最前。
	if got[0].Description != "备注 2" {
		t.Errorf("首条 = %q，期望「备注 2」", got[0].Description)
	}
}

// --- REVIEW 队列 ---

// TestLifecycleReviewConfirmFlow 覆盖「查边 → 确认」：
// 确认只清标记、保留边，且已确认过的边不能再被操作第二次。
func TestLifecycleReviewConfirmFlow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	pending := insertAlertRow(t, pool, "fp-rev-pending", "firing", `{"service":"checkout"}`, queryBase, nil)
	confirmed := insertAlertRow(t, pool, "fp-rev-done", "firing", `{"service":"checkout"}`, queryBase, nil)
	score := 0.62
	linkAlert(t, ctx, pool, inc, pending, &score, true, queryBase)
	linkAlert(t, ctx, pool, inc, confirmed, &score, false, queryBase)

	// 队列只含待确认的边。
	edges, err := repo.ListReviews(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListReviews 失败: %v", err)
	}
	if len(edges) != 1 || edges[0].Alert.AlertID != pending {
		t.Errorf("队列 = %v，期望只含告警 %d", reviewPairs(edges), pending)
	}

	// 取边并确认。
	edge, err := repo.GetReviewEdge(ctx, inc, pending)
	if err != nil {
		t.Fatalf("GetReviewEdge 失败: %v", err)
	}
	if edge.Alert.Fingerprint != "fp-rev-pending" {
		t.Errorf("fingerprint = %s，期望 fp-rev-pending", edge.Alert.Fingerprint)
	}
	ok, err := repo.ConfirmReviewEdge(ctx, inc, pending)
	if err != nil {
		t.Fatalf("ConfirmReviewEdge 失败: %v", err)
	}
	if !ok {
		t.Fatal("首次确认应命中")
	}

	// 边还在（确认 = 保留关联），但标记已清。
	var stillLinked bool
	if err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2)`,
		inc, pending).Scan(&stillLinked); err != nil {
		t.Fatalf("检查关联边失败: %v", err)
	}
	if !stillLinked {
		t.Error("确认后关联边应保留")
	}

	// 第二次操作必须拿不到 —— 否则重复 confirm 会产生重复留痕。
	if _, err = repo.GetReviewEdge(ctx, inc, pending); !errors.Is(err, incident.ErrReviewEdgeNotFound) {
		t.Errorf("已确认的边应返回 ErrReviewEdgeNotFound，实际: %v", err)
	}
	ok, err = repo.ConfirmReviewEdge(ctx, inc, pending)
	if err != nil {
		t.Fatalf("重复确认不应报错: %v", err)
	}
	if ok {
		t.Error("重复确认不应命中")
	}
}

// TestLifecycleReviewRejectUnlinks 覆盖拒绝的三步写操作：删边 / 清指针 / 不残留待确认。
func TestLifecycleReviewRejectUnlinks(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	// 该告警只属于这一个事件：删边后指针应被清掉。
	solo := insertAlertRow(t, pool, "fp-rej-solo", "firing", `{"service":"checkout"}`, queryBase, nil)
	// 该告警同时属于两个事件：删掉一条边后指针必须**保留**。
	shared := insertAlertRow(t, pool, "fp-rej-shared", "firing", `{"service":"checkout"}`, queryBase, nil)
	other := insertIncidentFull(t, ctx, pool, "payment 超时", "warning", "open", "payment", queryBase)

	score := 0.6
	linkAlert(t, ctx, pool, inc, solo, &score, true, queryBase)
	linkAlert(t, ctx, pool, inc, shared, &score, true, queryBase)
	linkAlert(t, ctx, pool, other, shared, &score, true, queryBase)
	setAlertIncident(t, ctx, pool, solo, inc)
	setAlertIncident(t, ctx, pool, shared, inc)

	ok, err := repo.DeleteAlertEdge(ctx, inc, solo)
	if err != nil {
		t.Fatalf("DeleteAlertEdge 失败: %v", err)
	}
	if !ok {
		t.Fatal("删边应命中")
	}
	n, err := repo.CountAlertEdges(ctx, solo)
	if err != nil {
		t.Fatalf("CountAlertEdges 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("删边后剩余边数 = %d，期望 0", n)
	}
	if err = repo.ClearAlertIncident(ctx, solo); err != nil {
		t.Fatalf("ClearAlertIncident 失败: %v", err)
	}
	if got := alertIncident(t, ctx, pool, solo); got != nil {
		t.Errorf("唯一归属被删后 incident_id 应为 NULL，实际 %d", *got)
	}

	// 共享告警：删掉一条边后还剩一条，指针不该动。
	ok, err = repo.DeleteAlertEdge(ctx, inc, shared)
	if err != nil {
		t.Fatalf("DeleteAlertEdge(shared) 失败: %v", err)
	}
	if !ok {
		t.Fatal("删边(shared)应命中")
	}
	n, err = repo.CountAlertEdges(ctx, shared)
	if err != nil {
		t.Fatalf("CountAlertEdges(shared) 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("shared 剩余边数 = %d，期望 1", n)
	}
	if got := alertIncident(t, ctx, pool, shared); got == nil || *got != inc {
		t.Errorf("仍有归属时 incident_id 不应被清，实际 %v", got)
	}

	// 队列此刻应**只剩** other 的那条边：本次删的是 (inc, solo) 与 (inc, shared)，
	// 而 (other, shared) 既没被删、needs_review 仍为真，理所当然还在队列里。
	// 一开始我把这里写成「队列应为空」，是漏算了 other 那条边 —— 断言错，不是实现错。
	edges, err := repo.ListReviews(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListReviews 失败: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("队列条数 = %d，期望 1（只剩 other 的那条边），实际 %v",
			len(edges), reviewPairs(edges))
	}
	if edges[0].IncidentID != other || edges[0].Alert.AlertID != shared {
		t.Errorf("队列仅剩的一条 = %v，期望 (incident=%d, alert=%d)",
			reviewPairs(edges), other, shared)
	}
}

// TestLifecycleGetReviewEdgeMissing 断言三种「拿不到边」的情形统一映射为哨兵错误，
// 由 API 层映射成 404（对调用方而言「不存在」与「已处理」是同一个结果）。
func TestLifecycleGetReviewEdgeMissing(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	alert1 := insertAlertRow(t, pool, "fp-miss", "firing", `{"service":"checkout"}`, queryBase, nil)

	// ① 根本没这条边。
	if _, err := repo.GetReviewEdge(ctx, inc, alert1); !errors.Is(err, incident.ErrReviewEdgeNotFound) {
		t.Errorf("无边时应返回 ErrReviewEdgeNotFound，实际: %v", err)
	}

	// ② 边在但不需要确认（已确认过 / 是 MERGE 档）。
	linkAlert(t, ctx, pool, inc, alert1, nil, false, queryBase)
	if _, err := repo.GetReviewEdge(ctx, inc, alert1); !errors.Is(err, incident.ErrReviewEdgeNotFound) {
		t.Errorf("非待确认的边应返回 ErrReviewEdgeNotFound，实际: %v", err)
	}

	// ③ 事件 id 不存在（不是「边不存在」以外的任何错误类型）。
	if _, err := repo.GetReviewEdge(ctx, inc+99999, alert1); !errors.Is(err, incident.ErrReviewEdgeNotFound) {
		t.Errorf("事件不存在时应返回 ErrReviewEdgeNotFound，实际: %v", err)
	}
}

// TestLifecycleReviewRejectInTx 是「拒绝」的完整真库证明：
// 删边 + 写抑制 + 写留痕必须同事务 —— 抑制行与留痕都要能看到，
// 任一环失败则整体回滚（此处验证成功的那一半，回滚由领域层用例覆盖）。
func TestLifecycleReviewRejectInTx(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	svc := incident.NewService(repo)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "checkout 延迟", "critical", "open", "checkout", queryBase)
	a1 := insertAlertRow(t, pool, "fp-tx-reject", "firing", `{"service":"checkout"}`, queryBase, nil)
	score := 0.58
	linkAlert(t, ctx, pool, inc, a1, &score, true, queryBase)
	setAlertIncident(t, ctx, pool, a1, inc)

	res, err := svc.Review(ctx, inc, a1, incident.ReviewReject, "yu", "指纹不匹配")
	if err != nil {
		t.Fatalf("Review(reject) 失败: %v", err)
	}
	if !res.Suppressed || !res.Unlinked {
		t.Errorf("结果 = %+v，期望 suppressed=true unlinked=true", res)
	}

	// ① 边已删除。
	if n := countRows(t, pool,
		`SELECT count(*) FROM incident_alerts WHERE incident_id = $1 AND alert_id = $2`, inc, a1); n != 0 {
		t.Errorf("关联边应已删除，实际 %d 条", n)
	}
	// ② 抑制行已写入（键是 fingerprint + incident，可跨次复用）。
	if n := countRows(t, pool,
		`SELECT count(*) FROM review_suppressions WHERE fingerprint = 'fp-tx-reject' AND incident_id = $1`, inc); n != 1 {
		t.Errorf("抑制行 = %d 条，期望 1", n)
	}
	// ③ 指针已清。
	if got := alertIncident(t, ctx, pool, a1); got != nil {
		t.Errorf("incident_id 应为 NULL，实际 %d", *got)
	}
	// ④ 留痕已写且带动作与理由。
	var eventType, action, reason string
	err = pool.QueryRow(ctx,
		`SELECT event_type, metadata->>'action', metadata->>'reason'
		 FROM timeline WHERE incident_id = $1`, inc).
		Scan(&eventType, &action, &reason)
	if err != nil {
		t.Fatalf("查询留痕失败: %v", err)
	}
	if eventType != string(incident.EventUserComment) {
		t.Errorf("event_type = %s，期望 user_comment", eventType)
	}
	if action != string(incident.ReviewActionReject) {
		t.Errorf("metadata.action = %s，期望 %s", action, incident.ReviewActionReject)
	}
	if reason != "指纹不匹配" {
		t.Errorf("metadata.reason = %q，期望「指纹不匹配」", reason)
	}
}

// --- 工具 ---

func ids(rows []incident.Summary) []int64 {
	out := make([]int64, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].ID)
	}
	return out
}

// reviewPairs 把队列行打印成 (incident, alert) 对，便于失败时一眼看出「哪条边还在」——
// 只打印 alert id 会让人误判（同一个告警可以属于多个事件）。
func reviewPairs(rows []incident.ReviewEdge) string {
	parts := make([]string, 0, len(rows))
	for i := range rows {
		parts = append(parts, fmt.Sprintf("(%d,%d)", rows[i].IncidentID, rows[i].Alert.AlertID))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
