package repository

import (
	"context"
	"testing"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/correlation"
	"github.com/yu/iip/internal/testsupport"
)

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
		if err := incidents.LinkAlertToIncident(ctx, incX.ID, id, corrBase, nil, false); err != nil {
			t.Fatalf("关联到 X 失败: %v", err)
		}
	}
	if err := incidents.LinkAlertToIncident(ctx, incY.ID, alertD, corrBase, nil, false); err != nil {
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
