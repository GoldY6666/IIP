package alert

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yu/iip/internal/metrics"
)

// 本文件覆盖 T6.3 的告警接入埋点。
//
// 判据的取向：**不只断言「计数器变了」，还要断言两条不变量**
// （hit+miss 等于 stored+duplicate；race 是 miss 的子集不额外计数）。
// 只断言单点数值时，把埋点挪错分支（例如把 duplicate 记成 invalid）
// 依然可能通过；而不变量会把这类错直接顶出来。

// ingestCount 读 iip_alert_ingest_total{result} 的当前值。
func ingestCount(t *testing.T, m *metrics.Metrics, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.AlertIngest.WithLabelValues(result))
}

// dedupCount 读 iip_alert_dedup_total{result} 的当前值。
func dedupCount(t *testing.T, m *metrics.Metrics, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.AlertDedup.WithLabelValues(result))
}

// TestMetricsIngestCountsByOutcome 覆盖同一批里的三种结论：
// 新指纹落库、同指纹重复、字段非法被拒。
func TestMetricsIngestCountsByOutcome(t *testing.T) {
	m := metrics.New()
	repo := newFakeRepo()
	svc := newTestService(repo)
	svc.SetMetrics(m)

	// 第一条 rawFiring 落库；第二条同指纹 firing 应被判重复；
	// 第三条缺 alertname 应被判非法。三条在同一请求里，最贴近真实重投形态：
	// Alertmanager 重投时整批重发，而不是只补发失败的那条。
	got, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring, rawFiring, rawNoAlertname))
	if err != nil {
		t.Fatalf("Ingest 返回错误: %v", err)
	}
	if got.Accepted != 1 || got.Skipped != 1 || got.Rejected != 1 {
		t.Fatalf("计数不符：accepted=%d skipped=%d rejected=%d，期望 1/1/1",
			got.Accepted, got.Skipped, got.Rejected)
	}

	if v := ingestCount(t, m, metrics.IngestStored); v != 1 {
		t.Errorf("ingest{stored} = %v，期望 1", v)
	}
	if v := ingestCount(t, m, metrics.IngestDuplicate); v != 1 {
		t.Errorf("ingest{duplicate} = %v，期望 1", v)
	}
	if v := ingestCount(t, m, metrics.IngestInvalid); v != 1 {
		t.Errorf("ingest{invalid} = %v，期望 1", v)
	}
	if v := ingestCount(t, m, metrics.IngestError); v != 0 {
		t.Errorf("ingest{error} = %v，期望 0（本批无存储故障）", v)
	}

	// 去重侧：第一条判 miss，第二条判 hit，第三条（非法）压根不进 persist。
	if v := dedupCount(t, m, metrics.DedupMiss); v != 1 {
		t.Errorf("dedup{miss} = %v，期望 1", v)
	}
	if v := dedupCount(t, m, metrics.DedupHit); v != 1 {
		t.Errorf("dedup{hit} = %v，期望 1", v)
	}

	// 不变量：每条进入去重判定的告警恰好计一次。
	hit, miss := dedupCount(t, m, metrics.DedupHit), dedupCount(t, m, metrics.DedupMiss)
	stored := ingestCount(t, m, metrics.IngestStored)
	dup := ingestCount(t, m, metrics.IngestDuplicate)
	if hit+miss != stored+dup {
		t.Errorf("不变量被破坏：hit+miss=%v，stored+duplicate=%v（应相等）", hit+miss, stored+dup)
	}
}

// TestMetricsResolvedUpdateCountsAsStored 守住 stored 的口径：
// 「把已有 firing 置为 resolved」也是成功写入，不能被漏计。
//
// 若哪天有人把它改成独立的 updated 标签，这条会红 —— 那时要同步
// 修改 metrics.IngestStored 的注释与 README 的口径说明，而不是只把测试改绿。
func TestMetricsResolvedUpdateCountsAsStored(t *testing.T) {
	m := metrics.New()
	svc := newTestService(newFakeRepo())
	svc.SetMetrics(m)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("首次 firing 失败: %v", err)
	}
	if _, err := svc.Ingest(context.Background(), makePayload("resolved", rawResolved)); err != nil {
		t.Fatalf("resolved 失败: %v", err)
	}

	if v := ingestCount(t, m, metrics.IngestStored); v != 2 {
		t.Errorf("ingest{stored} = %v，期望 2（插入 1 + 置 resolved 1）", v)
	}
	// 两次都撞上了已有指纹判定路径：第一次 miss，第二次 hit。
	if v := dedupCount(t, m, metrics.DedupHit); v != 1 {
		t.Errorf("dedup{hit} = %v，期望 1", v)
	}
	if v := dedupCount(t, m, metrics.DedupMiss); v != 1 {
		t.Errorf("dedup{miss} = %v，期望 1", v)
	}
}

// TestMetricsConcurrentInsertCountsRace 覆盖唯一约束兜底分支。
//
// race 必须是**额外**计数（不并入 hit/miss 的等比关系）：
// 它表示「判 miss 之后插入时撞了唯一约束」，即并发去重真的生效了。
// 若把 race 也算进 miss，hit+miss 之和会超过实际条数，比率就假了。
func TestMetricsConcurrentInsertCountsRace(t *testing.T) {
	m := metrics.New()
	repo := newFakeRepo()
	repo.insertConflict = true // 模拟 0002 部分唯一索引拦住并发重复
	svc := newTestService(repo)
	svc.SetMetrics(m)

	got, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring))
	if err != nil {
		t.Fatalf("Ingest 返回错误: %v", err)
	}
	if got.Skipped != 1 {
		t.Fatalf("skipped=%d，期望 1（唯一约束兜底）", got.Skipped)
	}

	if v := dedupCount(t, m, metrics.DedupRace); v != 1 {
		t.Errorf("dedup{race} = %v，期望 1", v)
	}
	if v := dedupCount(t, m, metrics.DedupMiss); v != 1 {
		t.Errorf("dedup{miss} = %v，期望 1（race 是 miss 的子集，不额外加）", v)
	}
	if v := ingestCount(t, m, metrics.IngestDuplicate); v != 1 {
		t.Errorf("ingest{duplicate} = %v，期望 1", v)
	}
}

// TestMetricsStorageFailureCountsError 覆盖存储故障分支。
//
// 这条特别重要：存储故障时 Ingest 提前 return，若不在那里补一次 error 计数，
// 「数据库挂了」在指标上表现为「接入量正常但全是 0」——
// 那与「上游真的没发告警」完全无法区分，是监控最不该有的盲区。
func TestMetricsStorageFailureCountsError(t *testing.T) {
	m := metrics.New()
	repo := newFakeRepo()
	repo.failInsert = errors.New("连接池耗尽")
	svc := newTestService(repo)
	svc.SetMetrics(m)

	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err == nil {
		t.Fatal("存储故障时 Ingest 应返回错误（handler 要回 503 触发 Alertmanager 重试）")
	}

	if v := ingestCount(t, m, metrics.IngestError); v != 1 {
		t.Errorf("ingest{error} = %v，期望 1", v)
	}
	if v := ingestCount(t, m, metrics.IngestStored); v != 0 {
		t.Errorf("ingest{stored} = %v，期望 0", v)
	}
	// 查询成功、插入失败：判了 miss 但没结论，dedup 仍记 miss 是对的
	//（去重判定确实发生了），stored/duplicate 都是 0，故不等式不成立 ——
	// 这正是 error 类不参与 hit+miss 等式的原因。
	if v := dedupCount(t, m, metrics.DedupMiss); v != 1 {
		t.Errorf("dedup{miss} = %v，期望 1", v)
	}
}

// TestSetMetricsNilFallsBackToNoop 守住「埋点侧零分支」的注入契约。
//
// 显式传 nil 是调用方最容易写出的形态（比如测试里传了个未初始化的变量）；
// 若不兜底成 Noop，下一次 Ingest 就会 nil 解引用 panic，
// 而那种 panic 只在「指标没配」的部署里出现。
func TestSetMetricsNilFallsBackToNoop(t *testing.T) {
	svc := newTestService(newFakeRepo())
	svc.SetMetrics(nil)
	if svc.metrics == nil {
		t.Fatal("SetMetrics(nil) 后 metrics 仍为 nil：下一次 Ingest 会 panic")
	}
	if _, err := svc.Ingest(context.Background(), makePayload("firing", rawFiring)); err != nil {
		t.Fatalf("未注入指标时 Ingest 失败: %v", err)
	}
}
