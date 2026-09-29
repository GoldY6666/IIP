package config

import (
	"testing"
	"time"
)

// TestLoad_DefaultsAndOverrides 表驱动验证：默认值、flag 覆盖、必填校验。
// 规格依据：docs/IIP-M0详细开发计划.md §T0.3（"表驱动验证默认值与必填校验"）。
func TestLoad_DefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name:    "必填缺失_无PGURL_应报错",
			args:    nil,
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "仅提供PGURL_其余取默认值",
			args: []string{"--pg-url=postgres://u:p@localhost:5432/iip"},
			env:  map[string]string{},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://u:p@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "flag覆盖默认值",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--http-addr=:9999",
				"--log-level=debug",
				"--worker-poll=5s",
				"--db-poll-only=false",
				"--webhook-max-bytes=2048",
				"--topology-file=/etc/iip/topo.yaml",
				"--correlation-enabled=false",
			},
			env: map[string]string{},
			want: Config{
				HTTPAddr:           ":9999",
				PGURL:              "postgres://u:p@localhost:5432/iip",
				LogLevel:           "debug",
				WorkerPoll:         5 * time.Second,
				DBPollOnly:         false,
				WebhookMaxBytes:    2048,
				TopologyFile:       "/etc/iip/topo.yaml",
				CorrelationEnabled: false,
			},
		},
		{
			name: "环境变量提供PGURL",
			args: nil,
			env:  map[string]string{"IIP_PG_URL": "postgres://env@localhost:5432/iip"},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "环境变量提供webhook上限",
			args: nil,
			env: map[string]string{
				"IIP_PG_URL":            "postgres://env@localhost:5432/iip",
				"IIP_WEBHOOK_MAX_BYTES": "4096",
			},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    4096,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "flag优先于环境变量",
			args: []string{"--log-level=warn"},
			env: map[string]string{
				"IIP_PG_URL":    "postgres://env@localhost:5432/iip",
				"IIP_LOG_LEVEL": "debug",
			},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://env@localhost:5432/iip",
				LogLevel:           "warn",
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
			},
		},
		{
			name: "webhook上限为零_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--webhook-max-bytes=0",
			},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "webhook上限为负_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--webhook-max-bytes=-1",
			},
			env:     map[string]string{},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 设置环境变量并在子测试结束后清理，避免污染其他用例。
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// 确保必填相关变量不被宿主环境意外注入。
			if _, ok := tc.env["IIP_PG_URL"]; !ok {
				t.Setenv("IIP_PG_URL", "")
			}

			got, err := Load(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功，got=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("未期望报错，实得: %v", err)
			}
			if *got != tc.want {
				t.Fatalf("配置不匹配\n got=%+v\nwant=%+v", *got, tc.want)
			}
		})
	}
}

// TestValidate 单独验证字段校验逻辑。
func TestValidate(t *testing.T) {
	// base 是除被测字段外全部合法的配置，避免每个用例重复书写。
	base := Config{
		PGURL:           "postgres://x",
		HTTPAddr:        ":8080",
		WorkerPoll:      time.Second,
		WebhookMaxBytes: DefaultWebhookMaxBytes,
	}
	withWebhook := func(n int64) Config {
		c := base
		c.WebhookMaxBytes = n
		return c
	}

	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"空PGURL_报错", Config{PGURL: "", HTTPAddr: ":8080", WorkerPoll: time.Second, WebhookMaxBytes: DefaultWebhookMaxBytes}, true},
		{"空白PGURL_报错", Config{PGURL: "   ", HTTPAddr: ":8080", WorkerPoll: time.Second, WebhookMaxBytes: DefaultWebhookMaxBytes}, true},
		{"空HTTPAddr_报错", Config{PGURL: "postgres://x", HTTPAddr: "", WorkerPoll: time.Second, WebhookMaxBytes: DefaultWebhookMaxBytes}, true},
		{"非正轮询间隔_报错", Config{PGURL: "postgres://x", HTTPAddr: ":8080", WorkerPoll: 0, WebhookMaxBytes: DefaultWebhookMaxBytes}, true},
		{"webhook上限为零_报错", withWebhook(0), true},
		{"webhook上限为负_报错", withWebhook(-1024), true},
		{"webhook上限为正_通过", withWebhook(1), false},
		{"全部合法_通过", base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("未期望报错，实得: %v", err)
			}
		})
	}
}
