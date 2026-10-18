package main

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/metrics"
)

// 本文件验 T6.3 的 worker 指标监听装配：能起、能抓、关得掉，
// 且**端口被占时不拖死采集**（这是与「启动失败即退出」最重要的区别）。

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestStartWorkerMetricsServesMetrics 验「起了就能抓到真实数值」。
//
// 用 127.0.0.1:0 让内核分配端口，再从 srv.Addr 读出来 ——
// 写死端口会让并行的 CI 偶发撞端口，而那种红与本用例无关。
func TestStartWorkerMetricsServesMetrics(t *testing.T) {
	m := metrics.New()
	m.TaskRetry.WithLabelValues(metrics.RetryStaleReclaim).Add(2)

	srv := startWorkerMetrics(quietLogger(), "127.0.0.1:0", m)
	if srv == nil {
		t.Fatal("startWorkerMetrics 返回 nil：指标端点没起来")
	}
	defer shutdownWorkerMetrics(srv)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + srv.Addr + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics 返回 %d，期望 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取正文失败: %v", err)
	}
	if !strings.Contains(string(body), `iip_task_retry_total{reason="stale_reclaim"} 2`) {
		t.Errorf("正文缺少 task_retry 计数：%s", firstN(string(body), 400))
	}
}

// TestStartWorkerMetricsDisabledWhenAddrEmpty 验「空地址 = 关闭」。
//
// 空串必须是合法形态而不是启动错误：容器里只想采证据、不暴露指标的部署
// 是最常见的形态之一。
func TestStartWorkerMetricsDisabledWhenAddrEmpty(t *testing.T) {
	if srv := startWorkerMetrics(quietLogger(), "", metrics.New()); srv != nil {
		t.Error("地址为空时应返回 nil（关闭），而不是起一个监听")
	}
	if srv := startWorkerMetrics(quietLogger(), "   ", metrics.New()); srv != nil {
		t.Error("纯空白地址也应视为关闭")
	}
}

// TestStartWorkerMetricsOccupiedPortDoesNotFail 是「降级而非退出」的判据。
//
// 先占住一个端口，再让 worker 用同一端口起指标：
// 期望返回 nil（调用方跳过 defer 继续跑），而不是 panic 或返回 error 让
// main 退出。这条若红，说明有人把「监控起不来」升级成了「采集不干了」。
func TestStartWorkerMetricsOccupiedPortDoesNotFail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if srv := startWorkerMetrics(quietLogger(), ln.Addr().String(), metrics.New()); srv != nil {
		shutdownWorkerMetrics(srv)
		t.Fatal("端口被占用时应返回 nil（降级），实际起了监听")
	}
}

// TestShutdownWorkerMetricsNilSafe 验退出路径对 nil 安全。
//
// run() 里是 `if srv := ...; srv != nil { defer shutdown(srv) }`，
// 看似不会传 nil；但把判断挪进 defer 是很自然的改动，
// 那种改动后这里必须仍然不 panic。
func TestShutdownWorkerMetricsNilSafe(t *testing.T) {
	shutdownWorkerMetrics(nil) // 不 panic 即通过
}

// firstN 截取前 n 个字符，用于失败信息。
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
