package repository

import (
	"context"
	"testing"

	"github.com/yu/iip/internal/testsupport"
)

// 本文件在真库上验证 REVIEW 抑制的两条性质：**按 (fingerprint, incident_id) 命中**
// 与**重复写入幂等**。领域侧的降级逻辑由 correlation 包的用例覆盖，此处不重复。

// TestReviewsIsSuppressed 断言未写抑制时不命中、写入后命中。
func TestReviewsIsSuppressed(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewReviews(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	const fp = "fp-suppress-me"

	hit, err := repo.IsSuppressed(ctx, fp, id)
	if err != nil {
		t.Fatalf("IsSuppressed 失败: %v", err)
	}
	if hit {
		t.Fatal("未写抑制时不该命中")
	}

	if err = repo.AddSuppression(ctx, fp, id, "yu", "不是同一个故障"); err != nil {
		t.Fatalf("AddSuppression 失败: %v", err)
	}
	hit, err = repo.IsSuppressed(ctx, fp, id)
	if err != nil {
		t.Fatalf("再次 IsSuppressed 失败: %v", err)
	}
	if !hit {
		t.Error("写入抑制后应命中")
	}

	// 抑制键必须包含 incident_id：同一指纹对**别的**事件不该被抑制。
	other := insertIncident(t, ctx, pool, "open")
	hit, err = repo.IsSuppressed(ctx, fp, other)
	if err != nil {
		t.Fatalf("查询另一事件失败: %v", err)
	}
	if hit {
		t.Error("抑制不应跨事件生效：同一指纹对另一事件仍可进入 REVIEW")
	}
}

// TestReviewsAddSuppressionIdempotent 断言重复写入不报错、也不产生重复行。
//
// 真实场景是「同一个人连点两次拒绝」——让它变成 500 或两条重复抑制都是缺陷。
func TestReviewsAddSuppressionIdempotent(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewReviews(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	const fp = "fp-double-click"

	for i := 0; i < 2; i++ {
		if err := repo.AddSuppression(ctx, fp, id, "yu", "重复点击"); err != nil {
			t.Fatalf("第 %d 次 AddSuppression 失败: %v", i+1, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM review_suppressions WHERE fingerprint = $1 AND incident_id = $2`,
		fp, id).Scan(&n); err != nil {
		t.Fatalf("统计抑制行失败: %v", err)
	}
	if n != 1 {
		t.Errorf("重复写入应仍是 1 行，实得 %d", n)
	}
}

// TestReviewsSuppressionCascade 断言事件删除时抑制行随之中清理（0004 的 ON DELETE CASCADE）。
func TestReviewsSuppressionCascade(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewReviews(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	if err := repo.AddSuppression(ctx, "fp-cascade", id, "yu", ""); err != nil {
		t.Fatalf("AddSuppression 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM incidents WHERE id = $1`, id); err != nil {
		t.Fatalf("删除事件失败: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM review_suppressions WHERE incident_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("统计抑制行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("事件删除后抑制行应被级联清理，实得 %d 行", n)
	}
}

// TestReviewsSuppressionColumns 断言 actor / reason 两个可空列如实落库。
func TestReviewsSuppressionColumns(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewReviews(pool)
	ctx := context.Background()

	id := insertIncident(t, ctx, pool, "open")
	if err := repo.AddSuppression(ctx, "fp-cols", id, "yu", "误报"); err != nil {
		t.Fatalf("AddSuppression 失败: %v", err)
	}
	// 无 actor / 无 reason 的一行（系统自动抑制的场景）。
	if err := repo.AddSuppression(ctx, "fp-cols-bare", id, "", ""); err != nil {
		t.Fatalf("AddSuppression（空 actor/reason）失败: %v", err)
	}

	var actor, reason *string
	err := pool.QueryRow(ctx,
		`SELECT actor, reason FROM review_suppressions WHERE fingerprint = 'fp-cols'`).Scan(&actor, &reason)
	if err != nil {
		t.Fatalf("查询抑制行失败: %v", err)
	}
	if actor == nil || *actor != "yu" {
		t.Errorf("actor = %v，期望 yu", actor)
	}
	if reason == nil || *reason != "误报" {
		t.Errorf("reason = %v，期望「误报」", reason)
	}

	var bareActor, bareReason *string
	err = pool.QueryRow(ctx,
		`SELECT actor, reason FROM review_suppressions WHERE fingerprint = 'fp-cols-bare'`).
		Scan(&bareActor, &bareReason)
	if err != nil {
		t.Fatalf("查询空字段抑制行失败: %v", err)
	}
	if bareActor != nil {
		t.Errorf("未指定 actor 时应落 NULL，实得 %v", *bareActor)
	}
	if bareReason != nil {
		t.Errorf("未指定 reason 时应落 NULL，实得 %v", *bareReason)
	}
}
