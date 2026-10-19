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
		// wantHTTPTimeout 为 true 时把 HTTPTimeout 的默认值合并进 want。
		// 由用例按意图显式声明，不从字段值反推（见 mergeHTTPTimeout 的说明）。
		wantHTTPTimeout bool
	}{
		{
			name:    "必填缺失_无PGURL_应报错",
			args:    nil,
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:            "仅提供PGURL_其余取默认值",
			args:            []string{"--pg-url=postgres://u:p@localhost:5432/iip"},
			env:             map[string]string{},
			wantHTTPTimeout: true,
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
			name:            "flag覆盖默认值",
			wantHTTPTimeout: true,
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
			name:            "环境变量提供PGURL",
			wantHTTPTimeout: true,
			args:            nil,
			env:             map[string]string{"IIP_PG_URL": "postgres://env@localhost:5432/iip"},
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
			name:            "环境变量提供webhook上限",
			wantHTTPTimeout: true,
			args:            nil,
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
			name:            "flag优先于环境变量",
			wantHTTPTimeout: true,
			args:            []string{"--log-level=warn"},
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
		{
			// SlackTimeout 显式给非零值 ⇒ 不触发 mergeT61，
			// 故 SlackWebhookURL 必须在 want 里显式写空（默认关闭）。
			name:            "slack通知配置生效",
			wantHTTPTimeout: true,
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--slack-webhook-url=https://hooks.slack.com/services/T000/B000/XXX",
				"--slack-timeout=3s",
			},
			env: map[string]string{},
			want: Config{
				HTTPAddr:           DefaultHTTPAddr,
				PGURL:              "postgres://u:p@localhost:5432/iip",
				LogLevel:           DefaultLogLevel,
				WorkerPoll:         DefaultWorkerPoll,
				DBPollOnly:         DefaultDBPollOnly,
				WebhookMaxBytes:    DefaultWebhookMaxBytes,
				TopologyFile:       DefaultTopologyFile,
				CorrelationEnabled: DefaultCorrelationEnabled,
				SlackWebhookURL:    "https://hooks.slack.com/services/T000/B000/XXX",
				SlackTimeout:       3 * time.Second,
			},
		},
		{
			name: "slack_timeout为零_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--slack-timeout=0s",
			},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			// 从 Slack 页面复制时漏掉 https:// 是最常见的误配，
			// 必须在启动期拦下（见 validateSlack 的说明）。
			name: "slack_url缺scheme_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--slack-webhook-url=hooks.slack.com/services/T000/B000/XXX",
			},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			// 通用 webhook 与 Slack 独立：这里只开通用出口，Slack 保持关闭。
			name:            "通用webhook配置生效",
			wantHTTPTimeout: true,
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--notify-webhook-url=http://receiver.internal:9000/hook",
				"--notify-webhook-timeout=2s",
				"--notify-webhook-token=tok-123",
			},
			env: map[string]string{},
			want: Config{
				HTTPAddr:             DefaultHTTPAddr,
				PGURL:                "postgres://u:p@localhost:5432/iip",
				LogLevel:             DefaultLogLevel,
				WorkerPoll:           DefaultWorkerPoll,
				DBPollOnly:           DefaultDBPollOnly,
				WebhookMaxBytes:      DefaultWebhookMaxBytes,
				TopologyFile:         DefaultTopologyFile,
				CorrelationEnabled:   DefaultCorrelationEnabled,
				NotifyWebhookURL:     "http://receiver.internal:9000/hook",
				NotifyWebhookTimeout: 2 * time.Second,
				NotifyWebhookToken:   "tok-123",
			},
		},
		{
			name: "通用webhook_timeout为零_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--notify-webhook-timeout=0s",
			},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "通用webhook_url缺scheme_应报错",
			args: []string{
				"--pg-url=postgres://u:p@localhost:5432/iip",
				"--notify-webhook-url=receiver.internal:9000/hook",
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
			// M4 字段的期望值统一由 m4Defaults() 合并进来。
			// 刻意不在用例里内联书写那 11 个字段：一是冗长，二是漏掉任何一个
			// 都会让它以零值参与比较，而失败信息只说「配置不匹配」——
			// 看不出到底哪个字段对不上。集中合并后，新增字段只改一处。
			want := tc.want
			if want.WorkerTaskBatch == 0 {
				want = mergeM4(want)
			}
			// M5 同理：UI 四个地址有非空默认值（localhost 系列），
			// 漏合并必然让「期望全默认」的用例变红。
			if want.UIPrometheusURL == "" {
				want = mergeM5(want)
			}
			// S5 同理：HTTPTimeout 默认 15s，非零值，漏合并必然让
			// 「期望全默认」的用例变红。
			if tc.wantHTTPTimeout {
				want = mergeHTTPTimeout(want)
			}
			// T7.1 同理：三个分区维护参数默认全非零。
			if want.AlertRetentionDays == 0 {
				want = mergeT71(want)
			}
			// T6.1 同理：SlackTimeout 默认 5s 非零（SlackWebhookURL 默认空，
			// 不能作哨兵，理由见 mergeT61 的说明）。
			if want.SlackTimeout == 0 {
				want = mergeT61(want)
			}
			// T6.2 同理：NotifyWebhookTimeout 默认 5s 非零。
			if want.NotifyWebhookTimeout == 0 {
				want = mergeT62(want)
			}
			// T6.3 同理：MetricsEnabled 默认 true、WorkerMetricsAddr 默认 :9091。
			if !want.MetricsEnabled {
				want = mergeT63(want)
			}
			if *got != want {
				t.Fatalf("配置不匹配\n got=%+v\nwant=%+v", *got, want)
			}
		})
	}
}
