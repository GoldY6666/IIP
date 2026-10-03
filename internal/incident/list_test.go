package incident

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件覆盖 Service.List 的三件事：limit+1 探测、脏参数夹紧、空过滤展开。
// 真库上的过滤结果由 internal/repository 的用例覆盖。

// newListService 组装一个列表服务：store 里预置 rows 条摘要。
func newListService(rows int) (*Service, *fakeTransactor) {
	store := &fakeStore{}
	for i := 0; i < rows; i++ {
		store.summaries = append(store.summaries, Summary{ID: int64(i + 1), Status: StatusOpen})
	}
	tx := &fakeTransactor{store: store}
	return NewService(tx), tx
}

// TestListHasMoreProbe 是分页的核心断言：多查一条来判定「还有没有下一页」。
func TestListHasMoreProbe(t *testing.T) {
	svc, tx := newListService(3)

	got, err := svc.List(context.Background(), ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(got.Items) != 2 {
		t.Errorf("应只返回 limit 条，实得 %d", len(got.Items))
	}
	if !got.HasMore {
		t.Error("多查到 1 条时应判定 has_more = true")
	}
	// 探测的关键：交给仓储的 Limit 必须是 limit+1。
	if len(tx.store.listArgs) != 1 || tx.store.listArgs[0].Limit != 3 {
		t.Errorf("应 Limit=3（limit+1 探测）查询，实得 %+v", tx.store.listArgs)
	}
}

// TestListNoMoreWhenUnderLimit 断言「不足一页」时 has_more 为 false。
func TestListNoMoreWhenUnderLimit(t *testing.T) {
	svc, _ := newListService(2)

	got, err := svc.List(context.Background(), ListFilter{Limit: 5})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(got.Items) != 2 {
		t.Errorf("应返回全部 2 条，实得 %d", len(got.Items))
	}
	if got.HasMore {
		t.Error("不足一页时 has_more 应为 false")
	}
}

// TestListClampsBadPaging 断言脏分页参数退化为默认值而不是报错。
//
// 列表是只读查询，参数脏不该挡掉页面加载（手册 §4.3）——
// 这条用例就是防止有人「顺手加个 400」。
func TestListClampsBadPaging(t *testing.T) {
	cases := []struct {
		name       string
		in         ListFilter
		wantLimit  int
		wantOffset int
	}{
		{"limit 为 0 → 默认 100", ListFilter{Limit: 0}, DefaultListLimit, 0},
		{"limit 为负 → 默认 100", ListFilter{Limit: -5}, DefaultListLimit, 0},
		{"limit 超上限 → 夹紧 1000", ListFilter{Limit: 99999}, MaxListLimit, 0},
		{"offset 为负 → 0", ListFilter{Offset: -1}, DefaultListLimit, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, tx := newListService(0)
			if _, err := svc.List(context.Background(), tc.in); err != nil {
				t.Fatalf("脏参数不应报错: %v", err)
			}
			arg := tx.store.listArgs[0]
			if arg.Limit != tc.wantLimit+1 { // +1 是探测余量
				t.Errorf("规范化后 Limit = %d，期望 %d（含探测 +1）", arg.Limit, tc.wantLimit+1)
			}
			if arg.Offset != tc.wantOffset {
				t.Errorf("Offset = %d，期望 %d", arg.Offset, tc.wantOffset)
			}
		})
	}
}

// TestListExpandsEmptyFilters 断言「不过滤」被展开成全部合法取值。
//
// 这样 SQL 只有一种执行形态，不必在 WHERE 里写「参数为 NULL 则不限」的分支。
func TestListExpandsEmptyFilters(t *testing.T) {
	svc, tx := newListService(0)

	if _, err := svc.List(context.Background(), ListFilter{}); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	arg := tx.store.listArgs[0]
	if len(arg.Statuses) != len(AllStatuses()) {
		t.Errorf("空 status 过滤应展开为 %d 个状态，实得 %d", len(AllStatuses()), len(arg.Statuses))
	}
	if len(arg.Severities) != len(AllSeverities()) {
		t.Errorf("空 severity 过滤应展开为 %d 个严重度，实得 %d", len(AllSeverities()), len(arg.Severities))
	}

	// 显式指定过滤时应原样透传，不被展开覆盖。
	svc2, tx2 := newListService(0)
	if _, err := svc2.List(context.Background(), ListFilter{
		Statuses: []Status{StatusResolved}, Severities: []string{"critical"},
	}); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(tx2.store.listArgs[0].Statuses) != 1 || tx2.store.listArgs[0].Statuses[0] != StatusResolved {
		t.Errorf("显式过滤不应被展开覆盖，实得 %v", tx2.store.listArgs[0].Statuses)
	}
}

// TestListPassesTimeWindow 断言时间窗与 service 过滤条件原样下传。
func TestListPassesTimeWindow(t *testing.T) {
	svc, tx := newListService(0)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)

	if _, err := svc.List(context.Background(), ListFilter{Service: "checkout", From: &from, To: &to}); err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	arg := tx.store.listArgs[0]
	if arg.Service != "checkout" {
		t.Errorf("Service = %q，期望 checkout", arg.Service)
	}
	if arg.From == nil || !arg.From.Equal(from) {
		t.Errorf("From = %v，期望 %v", arg.From, from)
	}
	if arg.To == nil || !arg.To.Equal(to) {
		t.Errorf("To = %v，期望 %v", arg.To, to)
	}
}

// TestListPropagatesError 断言仓储故障原样上抛（由 API 层映射为 503）。
func TestListPropagatesError(t *testing.T) {
	svc, tx := newListService(0)
	tx.store.listErr = errors.New("存储不可用")

	if _, err := svc.List(context.Background(), ListFilter{}); err == nil {
		t.Fatal("仓储故障应上抛，实际返回 nil")
	}
}
