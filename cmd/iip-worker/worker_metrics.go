package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/yu/iip/internal/metrics"
)

// startWorkerMetrics 在独立端口上暴露 /metrics（T6.3）。
//
// 返回 nil 表示**未启用**（地址为空）或**起不来**（端口被占等），
// 两种情形调用方都只需跳过 defer，采集流程不受影响。
//
// 先 net.Listen 再 Serve，而不是直接 ListenAndServe：
// 后者的错误只出现在 goroutine 里，启动流程早已继续 —— 日志末尾一行 WARN
// 极容易被忽略，而「以为指标在跑、其实一条没抓」是自监控最坏的死法。
// 显式 Listen 让端口冲突在启动阶段就摆到台面上。
func startWorkerMetrics(logger *slog.Logger, addr string, m *metrics.Metrics) *http.Server {
	if strings.TrimSpace(addr) == "" {
		logger.Info("worker 指标端点未启用（IIP_WORKER_METRICS_ADDR 为空）")
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Warn("worker 指标监听失败，/metrics 不可用（采集不受影响）",
			slog.String("addr", addr),
			slog.String("error", err.Error()))
		return nil
	}
	// srv.Addr 记的是**实际**监听地址（端口 0 时由内核分配），
	// 便于日志与用例拿到真实端口；Serve 本身用的是 ln。
	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           m.Handler(),
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}
	go func() {
		logger.Info("worker 指标端点已启用",
			slog.String("addr", srv.Addr),
			slog.String("path", "/metrics"))
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// 走到这里说明监听已经建立、后来坏了（如 fd 耗尽）。
			// 仍不退出：指标坏掉不该连带停掉采集。
			logger.Warn("worker 指标服务异常退出", slog.String("error", err.Error()))
		}
	}()
	return srv
}

// shutdownWorkerMetrics 关闭指标监听；失败只记不返，退出路径不该被它拖住。
func shutdownWorkerMetrics(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
