package notify

import (
	"context"
	"testing"
	"time"
)

// fakeClock 是可控时钟：测试里手动推进，不依赖 sleep。
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// TestLimiterSuppressesThirdStatusUpdate 钉住计划的原文档位：
// 同事件 10 分钟内最多 2 条中间态更新，第 3 条被抑制。
func TestLimiterSuppressesThirdStatusUpdate(t *testing.T) {
	clk := newFakeClock()
	l := newStatusLimiter(clk.now)

	for i := 1; i <= statusUpdateLimit; i++ {
		ok, sent := l.allow(1, "acknowledged")
		if !ok {
			t.Fatalf("第 %d 条应放行", i)
		}
		if sent != i {
			t.Errorf("第 %d 条放行后窗口内条数应为 %d，实际 %d", i, i, sent)
		}
		clk.advance(time.Minute)
	}
	if ok, sent := l.allow(1, "acknowledged"); ok {
		t.Fatal("第 3 条应被抑制")
	} else if sent != statusUpdateLimit {
		t.Errorf("抑制时应报告窗口内已有 %d 条，实际 %d", statusUpdateLimit, sent)
	}
}

// TestLimiterWindowIsSlidingNotFixed 钉住**滑动**窗口语义。
//
// 判别场景（固定窗口与滑动窗口唯一分道扬镳的地方）：
//
//	t0        发第 1 条（窗口计数 1）
//	t0+9m59s  发第 2 条（计数 2）
//	t0+10m01s 若按「从首条起算的固定窗口」，窗口已翻篇 → 放行，计数重开
//	          按滑动窗口，t0 那条已过期、t0+9m59s 仍在窗内 → 放行（计数 1）
//	t0+10m02s 固定窗口会**再次放行**（第 4 条）；滑动窗口此时计数已到 2 → 抑制
//
// 所以「第 4 条被抑制」这一条断言，就能把固定窗口实现钉死。
func TestLimiterWindowIsSlidingNotFixed(t *testing.T) {
	clk := newFakeClock()
	l := newStatusLimiter(clk.now)

	if ok, _ := l.allow(1, "investigating"); !ok {
		t.Fatal("t0 应放行")
	}
	clk.advance(9*time.Minute + 59*time.Second)
	if ok, _ := l.allow(1, "investigating"); !ok {
		t.Fatal("t0+9m59s 应放行（窗口内仅 1 条）")
	}

	clk.advance(2 * time.Second) // t0+10m01s
	if ok, _ := l.allow(1, "investigating"); !ok {
		t.Fatal("t0+10m01s 应放行（t0 那条已滑出窗口）")
	}

	clk.advance(time.Second) // t0+10m02s
	if ok, _ := l.allow(1, "investigating"); ok {
		t.Fatal("t0+10m02s 应被抑制——固定窗口实现会在这里错误放行")
	}
}

// TestLimiterAlwaysAllowsTerminalAndReopen 钉住白名单：
// 终态与重开不受限流，且**不消耗**中间态的配额。
func TestLimiterAlwaysAllowsTerminalAndReopen(t *testing.T) {
	clk := newFakeClock()
	l := newStatusLimiter(clk.now)

	// 先把中间态配额用满。
	for i := 0; i < statusUpdateLimit; i++ {
		if ok, _ := l.allow(1, "acknowledged"); !ok {
			t.Fatalf("第 %d 条中间态应放行", i+1)
		}
	}
	if ok, _ := l.allow(1, "acknowledged"); ok {
		t.Fatal("配额已满，中间态应被抑制")
	}

	// 三个白名单目标状态都应放行。
	for _, to := range []string{"resolved", "ignored", "open"} {
		if ok, _ := l.allow(1, to); !ok {
			t.Errorf("终态/重开 %q 应放行", to)
		}
	}

	// 白名单不消耗配额：中间态仍然被抑制（没有因为终态消息而重置）。
	if ok, _ := l.allow(1, "acknowledged"); ok {
		t.Error("白名单消息不应重置中间态配额")
	}

	// 窗口滑过之后中间态恢复。
	clk.advance(statusUpdateWindow + time.Second)
	if ok, sent := l.allow(1, "acknowledged"); !ok {
		t.Errorf("窗口过后中间态应恢复放行，实际 sent=%d", sent)
	}
}

// TestLimiterIsolatesIncidents 钉住限流按事件隔离：
// 一个事件的风暴不能把另一个事件的消息压掉。
func TestLimiterIsolatesIncidents(t *testing.T) {
	clk := newFakeClock()
	l := newStatusLimiter(clk.now)

	for i := 0; i < statusUpdateLimit; i++ {
		l.allow(1, "acknowledged")
	}
	if ok, _ := l.allow(1, "acknowledged"); ok {
		t.Fatal("事件 1 应被抑制")
	}
	if ok, _ := l.allow(2, "acknowledged"); !ok {
		t.Fatal("事件 2 不应受事件 1 的影响")
	}
}

// TestLimiterSweepsStaleEntries 钉住内存上界：
// 条目超过上限时清理「最后一次发送早于两个窗口」的事件。
func TestLimiterSweepsStaleEntries(t *testing.T) {
	clk := newFakeClock()
	l := newStatusLimiter(clk.now)

	old := clk.t.Add(-3 * statusUpdateWindow)
	for i := int64(1); i <= maxLimiterEntries+10; i++ {
		l.seen[i] = []time.Time{old}
	}
	if len(l.seen) <= maxLimiterEntries {
		t.Fatalf("前置条件不成立：条目数 %d 未超上限", len(l.seen))
	}

	l.allow(maxLimiterEntries+100, "acknowledged") // 触发 sweep

	if len(l.seen) > maxLimiterEntries {
		t.Errorf("过期条目应被清理，实际仍有 %d 条", len(l.seen))
	}
	if _, ok := l.seen[1]; ok {
		t.Error("三窗口前的条目应被删除")
	}
	if _, ok := l.seen[maxLimiterEntries+100]; !ok {
		t.Error("刚记账的事件不应被误删")
	}
}

// TestServiceSuppressesStatusUpdatesButNotCreated 在 Service 层验证两条语义同时成立：
// 创建通知永不限流；同一事件的第 3 条状态变更到不了 sink。
func TestServiceSuppressesStatusUpdatesButNotCreated(t *testing.T) {
	sink := newFakeSink("slack")
	svc := New([]Sink{sink}, nil, nil)
	clk := newFakeClock()
	svc.limiter.now = clk.now
	defer func() { _ = svc.Close(context.Background()) }()

	if !svc.NotifyIncidentCreated(testRef()) {
		t.Fatal("创建通知应入队")
	}
	for i := 0; i < statusUpdateLimit; i++ {
		if !svc.NotifyStatusChanged(testRef(), "open", "acknowledged", "yu") {
			t.Fatalf("第 %d 条状态变更应入队", i+1)
		}
	}
	if svc.NotifyStatusChanged(testRef(), "acknowledged", "investigating", "yu") {
		t.Fatal("第 3 条状态变更应被去风暴抑制")
	}
	// 白名单仍可入队，验证抑制不是「整个事件静音」。
	if !svc.NotifyStatusChanged(testRef(), "investigating", "resolved", "yu") {
		t.Fatal("终态通知应放行")
	}

	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 收到：created + 2 条中间态 + resolved = 4 条
	if got := len(sink.received()); got != 4 {
		t.Errorf("sink 应收到 4 条（1 创建 + 2 更新 + 1 终态），实际 %d", got)
	}
}
