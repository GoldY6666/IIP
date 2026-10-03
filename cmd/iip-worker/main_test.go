package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// 本文件只测 **polling 循环自身的机制**：退出、drain 等待、panic 恢复。
// 「怎么采集证据」是 internal/worker 的事，在那边用真库与假采集器测，
// 不在这里重复 —— 这里连数据库都不碰。
//
// M0 时这些用例靠 dbPinger 替身驱动（runOnce 里的 SELECT 1 占位）。
// M4 起 runOnce 委托给 taskRunner，替身随之改成实现 RunOnce 的假执行器。

// blockingRunner 在 RunOnce 里阻塞，直到 release 被关闭，用于模拟 in-flight 任务。
//
// **刻意不因 ctx 取消而返回**：loop 传下来的是 WithoutCancel 的 ctx
// （见 main.go 的注释），收SIGTERM 时在跑的采集不会被打断。
// 若这里加了 `case <-ctx.Done(): return`，测试就与生产行为相反 ——
// 而它恰好能「测过」一个 drain 永远等不到东西的实现。
type blockingRunner struct {
	entered chan struct{}
	release chan struct{}
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (b *blockingRunner) RunOnce(context.Context) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return 0, nil
}

// simpleRunner 立即返回，用于普通退出测试。
type simpleRunner struct{}

func (simpleRunner) RunOnce(context.Context) (int, error) { return 0, nil }

// erroringRunner 每轮都返回错误。
type erroringRunner struct{ err error }

func (e erroringRunner) RunOnce(context.Context) (int, error) { return 0, e.err }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPoller_Loop_ExitsOnContextCancel：ctx 取消后 loop 必须返回（不挂死）。
func TestPoller_Loop_ExitsOnContextCancel(t *testing.T) {
	p := newPoller(testLogger(), simpleRunner{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- p.loop(ctx, 10*time.Millisecond) }()

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

// TestPoller_Drain_WaitsForInFlight：drain 应等待 in-flight 任务完成后才返回。
//
// 这是 M0 起就有的用例，M4 未改语义 —— worker 退出时必须等正在采集的任务
// 跑完，否则进程被 SIGTERM 杀掉会让任务停在 in_flight，
// 只能等超时回收（默认 5 分钟）才有人再领它。
func TestPoller_Drain_WaitsForInFlight(t *testing.T) {
	br := newBlockingRunner()
	p := newPoller(testLogger(), br)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.loop(ctx, 10*time.Millisecond) }()

	// 等第一个任务进入（in-flight = 1）。
	select {
	case <-br.entered:
	case <-time.After(time.Second):
		t.Fatal("任务未进入 RunOnce")
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

	close(br.release) // 放行 in-flight 任务

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop 返回错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release 后 loop 仍未退出")
	}

	if n := p.inFlight.Load(); n != 0 {
		t.Fatalf("退出时 in-flight 应为 0，实得 %d", n)
	}
}

// panicRunner 在 RunOnce 里 panic，验证 recovery。
type panicRunner struct{}

func (panicRunner) RunOnce(context.Context) (int, error) { panic("boom") }

func TestPoller_RunOnce_RecoversPanic(t *testing.T) {
	p := newPoller(testLogger(), panicRunner{})
	// 不应 panic 到测试层面。
	p.runOnce(context.Background())
}

// TestPoller_RunOnce_DoesNotCrashOnTaskError 验证任务级错误不会带崩循环。
//
// 错误已在 Processor 内逐条记过，poller 只记一条汇总；
// 若这里让它冒泡，worker 会因一个任务失败而退出 ——
// 而任务失败（数据源挂了）是**预期内**的常态，不是致命错误。
//
// 断言方式只能是「没有 panic」：runOnce 不返回错误，也不改任何可观测状态
// （pollCount 由 loop 递增，不在 runOnce 里）。这条用例的价值恰恰在于
// 它不需要任何断言就能失败 —— panic 会直接让测试失败。
func TestPoller_RunOnce_DoesNotCrashOnTaskError(t *testing.T) {
	p := newPoller(testLogger(), erroringRunner{err: errors.New("Prometheus 超时")})
	p.runOnce(context.Background())

	if n := p.inFlight.Load(); n != 0 {
		t.Errorf("runOnce 结束后 in-flight 应为 0，实得 %d —— 计数只在 loop 里加减", n)
	}
}
