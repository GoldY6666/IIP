package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yu/iip/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestBuildNotifierNilWhenNoWebhook 钉住默认关闭：
// 没配 webhook 时必须返回 nil，装配处因此不会注入——这是「不会意外发消息」
// 的结构性保证，而不是靠 sink 里再判一次空串。
func TestBuildNotifierNilWhenNoWebhook(t *testing.T) {
	cfg := &config.Config{SlackTimeout: config.DefaultSlackTimeout}
	if got := buildNotifier(cfg, nil, testLogger()); got != nil {
		t.Fatal("未配置 webhook 时应返回 nil")
	}
}

// TestBuildNotifierEnabledWhenWebhookSet 钉住开启路径：
// 配了 webhook 就构造派发器，且能正常关闭（不泄漏 goroutine）。
func TestBuildNotifierEnabledWhenWebhookSet(t *testing.T) {
	cfg := &config.Config{
		SlackWebhookURL: "https://hooks.slack.com/services/T000/B000/xxxx",
		UIBaseURL:       "http://localhost:5173",
		SlackTimeout:    config.DefaultSlackTimeout,
	}
	got := buildNotifier(cfg, nil, testLogger())
	if got == nil {
		t.Fatal("配置了 webhook 时应构造派发器")
	}
	// 不发任何通知，直接关闭：队列空，Close 应立即返回。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := got.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
