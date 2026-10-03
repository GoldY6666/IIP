package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/testsupport"
)

// 本文件覆盖 M3 验收清单第 7 条：**详情 API 超 200 条时 truncated=true**。
//
// 这条判据此前 0 覆盖 —— 原因很直白：要触发它就得在真库里造 200+ 条关联边，
// 而写用例时最容易写成「造 5 条、断言不截断」这种没有信息量的检查。
// 结果就是「截断」这个分支从未被真库跑过一次，
// 而它恰恰是唯一会把「显示不全」说成「全部」的分支。
//
// 为什么必须真库：截断判定涉及 count(*) 与 limit+1 探测的配合，
// 内存假存储给不出「第 201 条才截断」这种边界。

// truncBase 是本文件用的固定时间基准。
var truncBase = time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

// TestCountAlertsReturnsTrueTotalBeyondInlineLimit 是本文件的核心用例。
//
// 缺陷背景（2026-10-03 查漏补缺时发现）：详情页的 AlertCount 原先取
// len(alerts)，而 alerts 已被截断到 200 条 → 实际 250 条的事件会显示
// 「200 条关联告警」。列表端走 SQL count(*) 是真值，两个端点自相矛盾。
//
// 本用例造 250 条关联边，断言 CountAlerts 返回真值 250 而非 200。
func TestCountAlertsReturnsTrueTotalBeyondInlineLimit(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	const total = 250
	inc := insertIncidentFull(t, ctx, pool, "截断计数事件", "critical", "open", "checkout", truncBase)

	score := 0.8
	for i := range total {
		fp := fmt.Sprintf("fp-trunc-%03d", i)
		aid := insertAlertRow(t, pool, fp, "firing", `{"service":"checkout"}`, truncBase, nil)
		// 每 3 条留一条待确认，用来同时验 needs_review 的真值。
		needsReview := i%3 == 0
		linkAlert(t, ctx, pool, inc, aid, &score, needsReview, truncBase.Add(time.Duration(i)*time.Second))
	}

	gotTotal, err := repo.CountAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountAlerts 失败: %v", err)
	}
	if gotTotal != total {
		t.Errorf("CountAlerts = %d，期望 %d —— 被截断的条数也算进了总数，"+
			"前端会据此显示「%d 条关联告警」", gotTotal, total, gotTotal)
	}

	// 期望的待确认数：i%3==0 的个数
	wantNeeds := (total + 2) / 3
	gotNeeds, err := repo.CountNeedsReviewAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountNeedsReviewAlerts 失败: %v", err)
	}
	if gotNeeds != wantNeeds {
		t.Errorf("CountNeedsReviewAlerts = %d，期望 %d", gotNeeds, wantNeeds)
	}
}

// TestCountAlertsMatchesInlineSliceWhenNotTruncated 断言未截断时
// 「真计数」与「切片长度」一致 —— 否则上面的修复就是纯粹的多余开销。
func TestCountAlertsMatchesInlineSliceWhenNotTruncated(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "未截断事件", "warning", "open", "payment", truncBase)
	score := 0.7
	for i := range 5 {
		aid := insertAlertRow(t, pool, fmt.Sprintf("fp-small-%d", i), "firing",
			`{"service":"payment"}`, truncBase, nil)
		linkAlert(t, ctx, pool, inc, aid, &score, i%2 == 0, truncBase.Add(time.Duration(i)*time.Second))
	}

	total, err := repo.CountAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountAlerts 失败: %v", err)
	}
	slice, err := repo.Alerts(ctx, inc, incident.MaxIncidentAlerts+1)
	if err != nil {
		t.Fatalf("Alerts 失败: %v", err)
	}
	if total != len(slice) {
		t.Errorf("未截断时真计数 %d 与切片长度 %d 不一致 —— 服务层应直接复用切片统计，"+
			"不该多查一次", total, len(slice))
	}

	needs, err := repo.CountNeedsReviewAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountNeedsReviewAlerts 失败: %v", err)
	}
	fromSlice := 0
	for i := range slice {
		if slice[i].NeedsReview {
			fromSlice++
		}
	}
	if needs != fromSlice {
		t.Errorf("未截断时待确认真值 %d 与切片统计 %d 不一致", needs, fromSlice)
	}
}

// TestCountAlertsOnIncidentWithoutEdges 返回 0 而非报错。
func TestCountAlertsOnIncidentWithoutEdges(t *testing.T) {
	pool := testsupport.NewDB(t)
	repo := NewLifecycle(pool)
	ctx := context.Background()

	inc := insertIncidentFull(t, ctx, pool, "无关联告警", "info", "open", "search", truncBase)

	total, err := repo.CountAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountAlerts 失败: %v", err)
	}
	if total != 0 {
		t.Errorf("无关联边的事件 CountAlerts = %d，期望 0", total)
	}
	needs, err := repo.CountNeedsReviewAlerts(ctx, inc)
	if err != nil {
		t.Fatalf("CountNeedsReviewAlerts 失败: %v", err)
	}
	if needs != 0 {
		t.Errorf("无关联边的事件 CountNeedsReviewAlerts = %d，期望 0", needs)
	}
}
