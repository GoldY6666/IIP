package config

import (
	"strings"
	"testing"
)

// 本文件钉住 T6.3 指标配置的默认值与校验规则。
//
// 为什么「默认开启」值得单独立一条：它是与通知类开关**方向相反**的默认，
// 后人若把 MetricsEnabled 改成默认 false「保持一致」，这条会红，
// 而红的时候必须回答「为什么指标要和数据出境类开关同默认」。

const testPGURLMetrics = "postgres://u:p@localhost:5432/iip"

// TestMetricsDefaults 断言默认形态：server 指标开、worker 监听 :9091。
func TestMetricsDefaults(t *testing.T) {
	t.Setenv("IIP_PG_URL", "")
	c, err := Load([]string{"--pg-url=" + testPGURLMetrics})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MetricsEnabled {
		t.Error("MetricsEnabled 默认应为 true（指标不出站、不含告警内容，默认关的代价是线上要改配置重启）")
	}
	if c.WorkerMetricsAddr != DefaultWorkerMetricsAddr {
		t.Errorf("WorkerMetricsAddr = %q，want %q", c.WorkerMetricsAddr, DefaultWorkerMetricsAddr)
	}
	if DefaultWorkerMetricsAddr != ":9091" {
		// 9090 是 Prometheus 自己的端口，本机开发常常同时跑着它。
		t.Errorf("DefaultWorkerMetricsAddr 被改成了 %q：9090 会与 Prometheus 撞端口", DefaultWorkerMetricsAddr)
	}
}

// TestMetricsEnvOverride 断言三个可关形态都能通过环境变量表达。
func TestMetricsEnvOverride(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		wantEnabled bool
		wantAddr    string
	}{
		{
			name:        "关闭 server 指标",
			env:         map[string]string{"IIP_METRICS_ENABLED": "false"},
			wantEnabled: false,
			wantAddr:    DefaultWorkerMetricsAddr,
		},
		{
			name:        "关闭 worker 指标（地址留空）",
			env:         map[string]string{"IIP_WORKER_METRICS_ADDR": ""},
			wantEnabled: true,
			wantAddr:    "",
		},
		{
			name:        "只监听本机",
			env:         map[string]string{"IIP_WORKER_METRICS_ADDR": "127.0.0.1:9191"},
			wantEnabled: true,
			wantAddr:    "127.0.0.1:9191",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IIP_PG_URL", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := Load([]string{"--pg-url=" + testPGURLMetrics})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.MetricsEnabled != tc.wantEnabled {
				t.Errorf("MetricsEnabled = %v，want %v", c.MetricsEnabled, tc.wantEnabled)
			}
			if c.WorkerMetricsAddr != tc.wantAddr {
				t.Errorf("WorkerMetricsAddr = %q，want %q", c.WorkerMetricsAddr, tc.wantAddr)
			}
		})
	}
}

// TestValidateMetricsAddr 钉住「漏写冒号必须在启动期拦下」。
//
// 放行的话错误会在一分钟后以 WARN 的形式出现在 worker 日志里
// （"missing port in address"），而那时运维已经以为指标在跑了。
func TestValidateMetricsAddr(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"", false},               // 空 = 关闭，合法
		{":9091", false},          // 监听所有网卡
		{"127.0.0.1:9091", false}, // 只听本机
		{"0.0.0.0:9091", false},   // 显式全网卡
		{"9091", true},            // 漏冒号
		{"localhost", true},       // 只有主机名
		{"  :9091  ", false},      // 前后空格不拦（net.Listen 自己会处理）
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			err := validateMetrics(&Config{WorkerMetricsAddr: tc.addr})
			if tc.wantErr && err == nil {
				t.Fatalf("addr=%q 期望报错，实际通过", tc.addr)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("addr=%q 期望通过，实得: %v", tc.addr, err)
			}
			if err != nil && !strings.Contains(err.Error(), "WORKER_METRICS_ADDR") {
				t.Errorf("错误信息应点名环境变量以便排查，实得: %v", err)
			}
		})
	}
}
