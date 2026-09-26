package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeRow 实现 pgx.Row，仅 Scan 返回 nil。
type fakeRow struct{}

func (fakeRow) Scan(dest ...any) error {
	// 模拟 SELECT 1 成功：把整数 1 写入首个目标。
	if len(dest) > 0 {
		if p, ok := dest[0].(*int); ok {
			*p = 1
		}
	}
	return nil
}

// blockingPinger 在 QueryRow 时阻塞，直到 release 被关闭，用于模拟 in-flight 任务。
type blockingPinger struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingPinger() *blockingPinger {
	return &blockingPinger{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (b *blockingPinger) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return fakeRow{}
}

// simplePinger 立即返回，用于普通退出测试。
type simplePinger struct{}

func (simplePinger) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return fakeRow{} }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWorker_Loop_ExitsOnContextCancel：ctx 取消后 loop 必须返回（不挂死）。
func TestWorker_Loop_ExitsOnContextCancel(t *testing.T) {
	w := newWorker(testLogger(), simplePinger{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- w.loop(ctx, 10*time.Millisecond) }()

	// 让循环跑几轮，再取消。
	time.Sleep(35 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop 返回错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 loop 未退出（疑似挂死）")
	}
}

// TestWorker_Drain_WaitsForInFlight：drain 应等待 in-flight 任务完成后才返回。
func TestWorker_Drain_WaitsForInFlight(t *testing.T) {
	bp := newBlockingPinger()
	w := newWorker(testLogger(), bp)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.loop(ctx, 10*time.Millisecond) }()

	// 等第一个任务进入（in-flight = 1）。
	select {
	case <-bp.entered:
	case <-time.After(time.Second):
		t.Fatal("任务未进入 QueryRow")
	}

	// 此时取消 ctx：drain 应阻塞，直到我们 release 掉在跑的任务。
	cancel()

	// 确认 drain 确实在等（短时间内不应返回）。
	select {
	case <-done:
		t.Fatal("drain 未等待 in-flight 任务即返回")
	case <-time.After(150 * time.Millisecond):
		// 预期：仍在等待
	}

	close(bp.release) // 放行 in-flight 任务

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop 返回错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release 后 loop 仍未退出")
	}

	if n := w.inFlight.Load(); n != 0 {
		t.Fatalf("退出时 in-flight 应为 0，实得 %d", n)
	}
}

// TestWorker_RunOnce_RecoversPanic 通过注入 panic 的 pinger 验证 recovery。
type panicPinger struct{}

func (panicPinger) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	panic("boom")
}

func TestWorker_RunOnce_RecoversPanic(t *testing.T) {
	w := newWorker(testLogger(), panicPinger{})
	// 不应 panic 到测试层面。
	w.runOnce(context.Background())
}
