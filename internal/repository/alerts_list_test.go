package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/testsupport"
)

// TestAlertsList_OrderingAndPaging 验证 ListAlerts 的 SQL 语义：
// 排序为 starts_at DESC, id DESC，且 limit/offset 分页正确。
//
// 依赖真实 PostgreSQL（见 testsupport）—— 无 IIP_TEST_PG_URL 时整体跳过，
// 不把「没库」伪装成「测试通过」。
func TestAlertsList_OrderingAndPaging(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewAlerts(pool)
	ctx := context.Background()

	base := time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)
	mk := func(n int, starts time.Time) *alert.Alert {
		lab, _ := json.Marshal(map[string]string{"alertname": "ListProbe", "instance": "i"})
		return &alert.Alert{
			Fingerprint: "fp-list-" + string(rune('a'+n)),
			Source:      alert.SourceAlertmanager,
			Severity:    alert.SeverityWarning,
			Status:      alert.StatusFiring,
			Labels:      lab,
			Annotations: json.RawMessage(`{}`),
			StartsAt:    starts,
			RawPayload:  json.RawMessage(`{}`),
		}
	}

	// 倒序写入：id 递增但 starts_at 也递增，使 id 顺序与 starts_at 顺序一致，
	// 仍能验证「排序不依赖插入顺序」由 SQL 决定。
	for i, d := range []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)} {
		if _, _, err := repo.Insert(ctx, mk(i, d)); err != nil {
			t.Fatalf("插入第 %d 条失败: %v", i, err)
		}
	}

	// 全量：3 条，按 starts_at DESC。
	all, err := repo.List(ctx, 100, 0)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if !all[i-1].StartsAt.After(all[i].StartsAt) {
			t.Errorf("第 %d 条应晚于第 %d 条（starts_at DESC），实际 %v / %v",
				i-1, i, all[i-1].StartsAt, all[i].StartsAt)
		}
	}

	// limit=2：只返回最新的 2 条。
	top2, err := repo.List(ctx, 2, 0)
	if err != nil {
		t.Fatalf("List(limit=2) 失败: %v", err)
	}
	if len(top2) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d", len(top2))
	}
	if top2[0].ID != all[0].ID {
		t.Errorf("limit=2 的首条应与全量首条一致，实际 %d vs %d", top2[0].ID, all[0].ID)
	}

	// offset=2：跳过最新的 2 条，返回最旧的 1 条。
	rest, err := repo.List(ctx, 100, 2)
	if err != nil {
		t.Fatalf("List(offset=2) 失败: %v", err)
	}
	if len(rest) != 1 {
		t.Fatalf("offset=2 应返回 1 条，实际 %d", len(rest))
	}
	if rest[0].ID != all[2].ID {
		t.Errorf("offset=2 应返回最旧的那条，实际 %d vs %d", rest[0].ID, all[2].ID)
	}
}
