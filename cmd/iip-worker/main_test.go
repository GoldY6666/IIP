package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yu/iip/internal/collectors"
	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/evidence"
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

// ---------------------------------------------------------------------------
// 装配（buildRegistry）
//
// 这一段单测存在的理由：装配函数**曾经完全没被测过**，而它恰恰是
// 「配置说开了、实际没生效」这类缺陷的唯一藏身处。
// 采集器自己的解析逻辑在 internal/collectors 里有 mock server 覆盖，
// 但那些测试全用**自己 new 出来的采集器**，绕过了 buildRegistry ——
// 于是「开关打开却从不注册」这种缺陷可以一路绿灯到生产。
// ---------------------------------------------------------------------------

// registryCfg 造一份能通过 Validate 的配置，三类采集器的开关与地址都可控。
//
// 从 config.Load 出发而不是手搓结构体：Validate 的必填项会随版本增补
// （M4 一次就补了 6 个），手搓结构体等于把这些约束复制一份到测试里，
// 迟早对不上——而对不上时表现是「测试红了但配置其实是对的」。
func registryCfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load([]string{
		"--pg-url", "postgres://u:p@127.0.0.1:5432/iip?sslmode=disable",
		"--prom-url", "http://prom:9090",
		"--loki-url", "http://loki:3100",
	})
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	return c
}

func hasSource(reg *evidence.Registry, src string) bool {
	_, ok := reg.Get(src)
	return ok
}

// TestBuildRegistry_DefaultHasNoK8s 钉住偏差 B1 的核心：**默认部署不注册
// K8s 采集器**。
//
// 这条与 TestBuildRegistry_K8sRegistersWhenEnabled 是一对：只写后者的话，
// 有人把默认值误改成 true 也会全绿，而那会让默认部署去打一个不存在的集群。
func TestBuildRegistry_DefaultHasNoK8s(t *testing.T) {
	c := registryCfg(t)
	if c.EvidenceK8sEnabled {
		t.Fatal("前提失效：K8s 默认应为关闭（B1 偏差）")
	}
	reg := buildRegistry(c, testLogger())

	if hasSource(reg, collectors.SourceK8s) {
		t.Error("默认配置下不应注册 K8s 采集器（偏差 B1 要求默认关闭）")
	}
	// Prom/Loki 给了地址就该真的注册 —— 顺带钉住「跳过注册」不是无条件跳过。
	for _, src := range []string{collectors.SourcePrometheus, collectors.SourceLoki} {
		if !hasSource(reg, src) {
			t.Errorf("给了地址的 %s 采集器应被注册，实际注册表=%v", src, reg.Sources())
		}
	}
}

// TestBuildRegistry_K8sRegistersWhenEnabled 是**本组测试的核心**：
// 开关打开且配置齐备时，K8s 采集器必须真的进注册表。
//
// 这条断言对应的实现缺陷是真实发生过的：原代码在
// `EvidenceK8sEnabled` 为真时只打一条 Info 日志，**从不调用注册**。
// 表现为「开关已开、地址已填、日志说已启用，但证据恒为 0 条且无报错」——
// 用户没有任何线索知道该查哪里。
func TestBuildRegistry_K8sRegistersWhenEnabled(t *testing.T) {
	c := registryCfg(t)
	c.EvidenceK8sEnabled = true
	c.K8sURL = "https://k8s.internal:6443"
	c.K8sNamespace = "prod"
	// 配置侧该组合是合法的（config 包另有断言），这里只验装配。
	if err := c.Validate(); err != nil {
		t.Fatalf("前提失效：开关+地址+命名空间的组合应通过配置校验: %v", err)
	}

	reg := buildRegistry(c, testLogger())
	if !hasSource(reg, collectors.SourceK8s) {
		t.Fatalf("开关已开且配置齐备，K8s 采集器必须注册；实际注册表=%v",
			reg.Sources())
	}
	// 注册了还不够：取出来必须真能采（证明不是塞了个空壳进去）。
	col, ok := reg.Get(collectors.SourceK8s)
	if !ok {
		t.Fatal("按来源取 K8s 采集器失败")
	}
	if col.Source() != collectors.SourceK8s {
		t.Errorf("采集器自报来源 = %q，期望 %q", col.Source(), collectors.SourceK8s)
	}
}

// TestBuildRegistry_PromLokiSkippedWithoutURL 钉住手册 §4.6 的修订：
// 开关默认 true 但没配地址时**跳过注册而不是拒绝启动**。
//
// 跳过与「注册了但采不到」必须能区分：前者注册表里没这个来源，
// 后者有。后者会让人以为集群里没有 Warning 事件。
func TestBuildRegistry_PromLokiSkippedWithoutURL(t *testing.T) {
	c := registryCfg(t)
	c.PromURL = ""
	c.LokiURL = ""
	reg := buildRegistry(c, testLogger())

	for _, src := range []string{collectors.SourcePrometheus, collectors.SourceLoki} {
		if hasSource(reg, src) {
			t.Errorf("无地址的 %s 不应被注册，实际注册表=%v", src, reg.Sources())
		}
	}
	if reg.Len() != 0 {
		t.Errorf("注册表大小 = %d，期望 0（全部采集器都该被跳过）: %v",
			reg.Len(), reg.Sources())
	}
}
