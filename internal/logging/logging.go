// Package logging 提供基于 log/slog 的 JSON 日志初始化。
//
// 规格依据：docs/IIP-M0详细开发计划.md §T0.4（"日志一律 log/slog JSON 模式"）。
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup 初始化全局 slog logger（JSON handler），返回 level 控制器以便运行时调级。
func Setup(level string) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	lv.Set(ParseLevel(level))

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: lv,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, lv
}

// ParseLevel 将字符串级别映射为 slog.Level，未知值回退到 Info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
