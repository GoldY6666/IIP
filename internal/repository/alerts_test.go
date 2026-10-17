package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/testsupport"
)

// 基准起始时间：刻意取过去，便于用「updated_at > starts_at」证明显式 NOW() 生效。
var baseStartsAt = time.Date(2026, 9, 24, 18, 31, 0, 0, time.UTC)

// newAlert 造一条领域对象。仓储不做指纹计算，故 fingerprint 由用例直接指定，
// 从而让每个用例只验证自己关心的那一条规则。
func newAlert(fingerprint, status string) *alert.Alert {
	a := &alert.Alert{
		Fingerprint: fingerprint,
		Source:      alert.SourceAlertmanager,
		Severity:    alert.SeverityWarning,
		Status:      status,
		Labels:      json.RawMessage(`{"alertname":"HighLatency"}`),
		Annotations: json.RawMessage(`{}`),
		StartsAt:    baseStartsAt,
		RawPayload:  json.RawMessage(`{"status":"` + status + `"}`),
	}
	if status == alert.StatusResolved {
		endsAt := baseStartsAt.Add(30 * time.Minute)
		a.EndsAt = &endsAt
	}
	return a
}

// TestLatestByFingerprintNotFound 未命中必须返回领域层哨兵错误，而不是零值。
func TestLatestByFingerprintNotFound(t *testing.T) {
	repo := NewAlerts(testsupport.NewDB(t))

	_, err := repo.LatestByFingerprint(context.Background(), "fp-never-seen")
	if !errors.Is(err, alert.ErrNotFound) {
		t.Fatalf("期望 alert.ErrNotFound，实际 %v", err)
	}
}

// TestInsertRejectsNil 防御性检查：nil 入参应给出可读错误而非 panic。
func TestInsertRejectsNil(t *testing.T) {
	repo := NewAlerts(testsupport.NewDB(t))

	_, _, err := repo.Insert(context.Background(), nil)
	if err == nil {
		t.Fatal("nil 告警应报错")
	}
}

// I-01：同指纹连插两次 firing → 第二次被跳过，库内仍只有 1 行。
func TestInsertSkipsDuplicateFiring(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()
	const fp = "fp-i-01"

	first, inserted, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
	if err != nil {
		t.Fatalf("首条插入失败: %v", err)
	}
	if !inserted {
		t.Fatal("首条应插入成功")
	}
	if first.ID == 0 {
		t.Error("应返回自增主键")
	}

	_, insertedAgain, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
	if err != nil {
		t.Fatalf("重复插入不应报错（属正常业务分支）: %v", err)
	}
	if insertedAgain {
		t.Error("第二条同指纹 firing 应被唯一约束跳过")
	}
	if n := countAlerts(t, pool, fp); n != 1 {
		t.Errorf("期望库内 1 行，实际 %d 行", n)
	}
}

// I-02：firing 之后收到 resolved → 更新既有行，不新增行；ends_at 落值。
func TestResolveUpdatesExistingRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()
	const fp = "fp-i-02"

	first, _, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
	if err != nil {
		t.Fatalf("插入 firing 失败: %v", err)
	}
	endsAt := baseStartsAt.Add(45 * time.Minute)
	if err = repo.Resolve(ctx, first.ID, &endsAt); err != nil {
		t.Fatalf("置为 resolved 失败: %v", err)
	}

	latest, err := repo.LatestByFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("查询最新一条失败: %v", err)
	}
	if latest.Status != alert.StatusResolved {
		t.Errorf("status 应为 %s，实际 %s", alert.StatusResolved, latest.Status)
	}
	if latest.EndsAt == nil {
		t.Fatal("ends_at 应非空")
	}
	if !latest.EndsAt.Equal(endsAt) {
		t.Errorf("ends_at = %v，期望 %v", latest.EndsAt, endsAt)
	}
	if n := countAlerts(t, pool, fp); n != 1 {
		t.Errorf("resolved 不应新增行，期望 1 行，实际 %d 行", n)
	}
	if !updatedAtAfterStartsAt(t, pool, fp) {
		t.Error("updated_at 应被显式置为 NOW()（0001 无触发器，漏写会永远停在插入时刻）")
	}
}

// I-03：resolved 之后重新起火 → 插新行，保留历史。
func TestRefireAfterResolveInsertsNewRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()
	const fp = "fp-i-03"

	first, _, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
	if err != nil {
		t.Fatalf("插入 firing 失败: %v", err)
	}
	endsAt := baseStartsAt.Add(45 * time.Minute)
	if err = repo.Resolve(ctx, first.ID, &endsAt); err != nil {
		t.Fatalf("置为 resolved 失败: %v", err)
	}

	second, inserted, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
	if err != nil {
		t.Fatalf("重新起火插入失败: %v", err)
	}
	if !inserted {
		t.Fatal("原 firing 行已转 resolved，新的 firing 应可插入")
	}
	if second.ID == first.ID {
		t.Error("重新起火必须产生新行（保留历史），不应复用旧行")
	}
	if n := countAlerts(t, pool, fp); n != 2 {
		t.Errorf("期望 2 行历史，实际 %d 行", n)
	}
}

// I-04：并发投递同一指纹的 firing → 数据库层兜底，恰好 1 行。
//
// 这是「应用层先查后写」防不住、必须靠唯一索引兜底的场景。
func TestConcurrentInsertKeepsSingleFiringRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()
	const fp = "fp-i-04"
	const workers = 10

	var wg sync.WaitGroup
	inserted := make([]bool, workers)
	errs := make([]error, workers)

	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, ok, err := repo.Insert(ctx, newAlert(fp, alert.StatusFiring))
			inserted[idx] = ok
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发插入报错: %v", i, err)
		}
	}
	okCount := 0
	for _, ok := range inserted {
		if ok {
			okCount++
		}
	}
	if okCount != 1 {
		t.Errorf("并发 %d 次应恰好 1 次真正插入，实际 %d 次", workers, okCount)
	}
	if n := countAlerts(t, pool, fp); n != 1 {
		t.Errorf("期望库内 1 行，实际 %d 行", n)
	}
}

// I-05（T7.1 / ADR-022）：目标月分区不存在时，Insert 必须补建分区后重试成功，
// 而不是把 SQLSTATE 23514 原样抛给调用方。
//
// 这条钉的是「插入路径兜底」：worker 维护 tick 理论上会预建未来分区，
// 但时钟漂移 / 手工 DROP / 跨月重火都可能让某个告警的 starts_at 落在
// 预建范围之外。没有重试时，那些告警会永久插不进去，而错误信息只说
// 「没有为行找到关系的分区」——排查时看不出是分区没建。
func TestInsertRetriesAfterEnsuringPartition(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()

	// +6 个月：超出迁移预建的 [当月, 当月+2]，必然无分区。
	// 截到微秒：PG timestamptz 精度就是微秒，Go 的纳秒尾巴过不了
	// 往返比较（Equal 比的是时刻，不是墙上显示）。
	future := time.Now().UTC().AddDate(0, 6, 0).Truncate(time.Microsecond)
	a := newAlert("fp-i-05-retry", alert.StatusFiring)
	a.StartsAt = future

	row, inserted, err := repo.Insert(ctx, a)
	if err != nil {
		t.Fatalf("目标月无分区时应补建并重试成功，实际报错: %v", err)
	}
	if !inserted {
		t.Fatal("重试后应真正插入")
	}

	// 分区必须真的被补建：行读得回，且 starts_at 与入参一致。
	got, err := repo.LatestByFingerprint(ctx, "fp-i-05-retry")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if !got.StartsAt.Equal(future) {
		t.Errorf("starts_at = %v，期望 %v", got.StartsAt, future)
	}
	if got.ID != row.ID {
		t.Errorf("读回 id = %d，期望 %d", got.ID, row.ID)
	}
}

// TestServiceIngestPipeline 用**真实仓储**跑通 Service 的全部去重分支。
//
// 走 ParsePayload（而非直接构造结构体），顺便覆盖线上真实的解码路径。
func TestServiceIngestPipeline(t *testing.T) {
	pool := testsupport.NewDB(t)
	svc := alert.NewService(NewAlerts(pool), nil)

	const firingBody = `{
	  "version": "4",
	  "status": "firing",
	  "alerts": [{
	    "status": "firing",
	    "labels": {"alertname": "HighLatency", "service": "checkout",
	               "namespace": "production", "instance": "checkout-abc123",
	               "severity": "warning"},
	    "annotations": {"summary": "p99 > 500ms"},
	    "startsAt": "2026-09-24T18:31:00Z",
	    "endsAt": "0001-01-01T00:00:00Z",
	    "fingerprint": "alertmanager-self-computed-must-be-ignored"
	  }]
	}`

	const resolvedBody = `{
	  "version": "4",
	  "status": "resolved",
	  "alerts": [{
	    "status": "resolved",
	    "labels": {"alertname": "HighLatency", "service": "checkout",
	               "namespace": "production", "instance": "checkout-abc123",
	               "severity": "warning"},
	    "annotations": {},
	    "startsAt": "2026-09-24T18:31:00Z",
	    "endsAt": "2026-09-24T19:00:00Z"
	  }]
	}`

	fingerprint := alert.GenerateFingerprint(map[string]string{
		"alertname": "HighLatency",
		"service":   "checkout",
		"namespace": "production",
		"severity":  "warning",
	})

	// 1) 首次 firing → accepted=1
	first := runIngest(t, svc, firingBody)
	assertCounts(t, "首次 firing", first, 1, 0, 0, 0)
	if n := countAlerts(t, pool, fingerprint); n != 1 {
		t.Fatalf("期望库内 1 行，实际 %d 行", n)
	}

	// 2) 重复 firing → skipped=1，库内不增行（幂等）
	again := runIngest(t, svc, firingBody)
	assertCounts(t, "重复 firing", again, 0, 1, 0, 0)
	if n := countAlerts(t, pool, fingerprint); n != 1 {
		t.Errorf("重复投递不得增行，期望 1 行，实际 %d 行", n)
	}

	// 3) resolved → updated=1，仍不增行
	resolved := runIngest(t, svc, resolvedBody)
	assertCounts(t, "resolved", resolved, 0, 0, 1, 0)
	latest := latestStatus(t, pool, fingerprint)
	if latest != alert.StatusResolved {
		t.Errorf("期望 status=resolved，实际 %s", latest)
	}
	if n := countAlerts(t, pool, fingerprint); n != 1 {
		t.Errorf("resolved 不得增行，期望 1 行，实际 %d 行", n)
	}

	// 4) 再次 resolved → skipped=1（幂等）
	repeatedResolved := runIngest(t, svc, resolvedBody)
	assertCounts(t, "重复 resolved", repeatedResolved, 0, 1, 0, 0)

	// 5) 重新起火 → accepted=1，新增历史行
	refire := runIngest(t, svc, firingBody)
	assertCounts(t, "重新起火", refire, 1, 0, 0, 0)
	if n := countAlerts(t, pool, fingerprint); n != 2 {
		t.Errorf("重新起火应新增历史行，期望 2 行，实际 %d 行", n)
	}
}

// I-06：重新起火后再次 resolved，必须命中**最新那一行**（updated=1）。
//
// 回归背景（本用例专门钉死它）：Alertmanager 重发同一告警时 startsAt 与首次
// **完全相同**，于是「已 resolved 的旧行」与「重新起火的 firing 新行」在 starts_at
// 上并列。GetAlertByFingerprint 若只按 starts_at DESC 排序，返回哪一行是未定义的：
// 端到端实测中同一轮运行的两次调用分别取回了 firing 与 resolved 两行，
// 后果是 resolved 被误判为「重复 resolved」而跳过，告警永远停在 firing。
// 排序必须以 id DESC 收尾 —— 同一指纹内 id 单调递增，最大者即最近写入的那条。
func TestRefireThenResolveUsesLatestRow(t *testing.T) {
	pool := testsupport.NewDB(t)
	svc := alert.NewService(NewAlerts(pool), nil)

	// 两个 payload 的 startsAt 刻意取相同值 —— 这正是真实上游的行为。
	const startsAt = "2026-09-24T18:31:00Z"
	firing := `{"version":"4","status":"firing","alerts":[{"status":"firing",
	  "labels":{"alertname":"Flapping","service":"api"},
	  "startsAt":"` + startsAt + `"}]}`
	resolved := `{"version":"4","status":"resolved","alerts":[{"status":"resolved",
	  "labels":{"alertname":"Flapping","service":"api"},
	  "startsAt":"` + startsAt + `","endsAt":"2026-09-24T19:00:00Z"}]}`

	fingerprint := alert.GenerateFingerprint(map[string]string{
		"alertname": "Flapping",
		"service":   "api",
	})

	assertCounts(t, "首次 firing", runIngest(t, svc, firing), 1, 0, 0, 0)
	assertCounts(t, "首次 resolved", runIngest(t, svc, resolved), 0, 0, 1, 0)
	if n := countAlerts(t, pool, fingerprint); n != 1 {
		t.Fatalf("首次 resolved 不得增行，期望 1 行，实际 %d 行", n)
	}

	// 重新起火：插新行，且 starts_at 与旧行完全相同 —— 并列的起点。
	assertCounts(t, "重新起火", runIngest(t, svc, firing), 1, 0, 0, 0)
	if n := countAlerts(t, pool, fingerprint); n != 2 {
		t.Fatalf("重新起火应新增历史行，期望 2 行，实际 %d 行", n)
	}

	// 关键断言：必须 updated=1。取错行会退化成 skipped=1，告警永远停在 firing。
	assertCounts(t, "重新起火后 resolved", runIngest(t, svc, resolved), 0, 0, 1, 0)
	if got := latestStatus(t, pool, fingerprint); got != alert.StatusResolved {
		t.Errorf("最终状态应为 resolved，实际 %s", got)
	}
}

// I-05：库中从无该指纹时的 resolved（孤立 resolved）→ 跳过，不插孤儿行。
func TestServiceSkipsOrphanResolved(t *testing.T) {
	pool := testsupport.NewDB(t)
	svc := alert.NewService(NewAlerts(pool), nil)

	const orphanResolved = `{
	  "version": "4",
	  "status": "resolved",
	  "alerts": [{
	    "status": "resolved",
	    "labels": {"alertname": "NeverSeenBefore", "severity": "warning"},
	    "annotations": {},
	    "startsAt": "2026-09-24T18:31:00Z",
	    "endsAt": "2026-09-24T19:00:00Z"
	  }]
	}`

	got := runIngest(t, svc, orphanResolved)
	assertCounts(t, "孤立 resolved", got, 0, 1, 0, 0)

	if n := totalAlerts(t, pool); n != 0 {
		t.Errorf("孤立 resolved 不得写入任何行，实际 %d 行", n)
	}
}

// 单条非法不阻断整批：合法条目照常落库，非法条目计入 rejected 并带下标与原因。
func TestServiceRejectsOnlyBadItems(t *testing.T) {
	pool := testsupport.NewDB(t)
	svc := alert.NewService(NewAlerts(pool), nil)

	const mixedBody = `{
	  "version": "4",
	  "status": "firing",
	  "alerts": [
	    {"status": "firing",
	     "labels": {"alertname": "GoodOne", "service": "checkout", "severity": "warning"},
	     "startsAt": "2026-09-24T18:31:00Z"},
	    {"status": "firing",
	     "labels": {"service": "checkout"},
	     "startsAt": "2026-09-24T18:31:00Z"},
	    {"status": "firing",
	     "labels": {"alertname": "BadTime", "service": "checkout"},
	     "startsAt": "2026/09/24 18:31"}
	  ]
	}`

	got := runIngest(t, svc, mixedBody)
	assertCounts(t, "混合批", got, 1, 0, 0, 2)

	if len(got.Errors) != 2 {
		t.Fatalf("期望 2 条错误明细，实际 %d 条: %v", len(got.Errors), got.Errors)
	}
	if got.Errors[0].Index != 1 || got.Errors[1].Index != 2 {
		t.Errorf("错误明细应带正确下标 [1 2]，实际 %v", got.Errors)
	}
	if totalAlerts(t, pool) != 1 {
		t.Errorf("应只落库 1 条合法告警，实际 %d 条", totalAlerts(t, pool))
	}
}

// 空 alerts 数组属正常（Alertmanager 的空 group），应 200 且零计数、零报错。
func TestServiceEmptyAlertsIsOK(t *testing.T) {
	pool := testsupport.NewDB(t)
	svc := alert.NewService(NewAlerts(pool), nil)

	got := runIngest(t, svc, `{"version":"4","status":"firing","alerts":[]}`)
	assertCounts(t, "空 alerts", got, 0, 0, 0, 0)
	if len(got.Errors) != 0 {
		t.Errorf("空 alerts 不应产生错误明细，实际 %v", got.Errors)
	}
}

// --- 测试内小工具 ---

func runIngest(t *testing.T, svc *alert.Service, body string) alert.Result {
	t.Helper()

	payload, err := alert.ParsePayload([]byte(body))
	if err != nil {
		t.Fatalf("解析 payload 失败: %v", err)
	}
	if err = alert.ValidatePayload(payload); err != nil {
		t.Fatalf("payload 结构非法: %v", err)
	}

	got, err := svc.Ingest(context.Background(), payload)
	if err != nil {
		t.Fatalf("Ingest 返回错误: %v", err)
	}
	return got
}

func assertCounts(t *testing.T, label string, got alert.Result, accepted, skipped, updated, rejected int) {
	t.Helper()
	if got.Accepted != accepted || got.Skipped != skipped || got.Updated != updated || got.Rejected != rejected {
		t.Errorf("%s: 计数不符，期望 accepted=%d skipped=%d updated=%d rejected=%d，实际 accepted=%d skipped=%d updated=%d rejected=%d (%v)",
			label, accepted, skipped, updated, rejected,
			got.Accepted, got.Skipped, got.Updated, got.Rejected, got.Errors)
	}
}

func countAlerts(t *testing.T, pool *pgxpool.Pool, fingerprint string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alerts WHERE fingerprint = $1`, fingerprint).Scan(&n)
	if err != nil {
		t.Fatalf("统计告警行数失败: %v", err)
	}
	return n
}

func totalAlerts(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM alerts`).Scan(&n); err != nil {
		t.Fatalf("统计告警总数失败: %v", err)
	}
	return n
}

func latestStatus(t *testing.T, pool *pgxpool.Pool, fingerprint string) string {
	t.Helper()
	var status string
	// 与生产查询 GetAlertByFingerprint 保持同一排序语义（含 id DESC tie-breaker）：
	// 否则断言会"碰巧通过"——starts_at 并列时返回哪一行是未定义的。
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM alerts WHERE fingerprint = $1 ORDER BY starts_at DESC, id DESC LIMIT 1`,
		fingerprint).Scan(&status)
	if err != nil {
		t.Fatalf("查询最新状态失败: %v", err)
	}
	return status
}

func updatedAtAfterStartsAt(t *testing.T, pool *pgxpool.Pool, fingerprint string) bool {
	t.Helper()
	var ok bool
	err := pool.QueryRow(context.Background(),
		`SELECT updated_at > starts_at FROM alerts WHERE fingerprint = $1 ORDER BY starts_at DESC, id DESC LIMIT 1`,
		fingerprint).Scan(&ok)
	if err != nil {
		t.Fatalf("比较 updated_at 失败: %v", err)
	}
	return ok
}

// 保证 fmt 被使用（错误消息拼接），避免 import 漂移。
var _ = fmt.Sprintf
