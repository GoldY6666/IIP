package correlation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
)

// TestDefaultConfigMatchesSpec 锁定 V1.0 §5 的五维权重与三档阈值。
// 这些数值是对外承诺的一部分（迁移 0003 的种子值、手册 §4.1 的表格都以此为准），
// 任何改动都应先改规格，而不是顺手改代码。
func TestDefaultConfigMatchesSpec(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Weights.Time != 0.25 || cfg.Weights.Service != 0.25 ||
		cfg.Weights.Label != 0.20 || cfg.Weights.Dependency != 0.20 ||
		cfg.Weights.Historical != 0.10 {
		t.Errorf("五维权重与规格不符: %+v", cfg.Weights)
	}
	if cfg.Thresholds.Merge != 0.80 || cfg.Thresholds.Review != 0.50 {
		t.Errorf("阈值与规格不符: %+v", cfg.Thresholds)
	}
	if !cfg.Enabled {
		t.Error("默认应启用关联引擎")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置必须合法，实得: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"默认合法", func(*Config) {}, false},
		{"权重和偏小", func(c *Config) { c.Weights.Time = 0.10 }, true},
		{"权重和偏大", func(c *Config) { c.Weights.Time = 0.40 }, true},
		{"权重为负", func(c *Config) { c.Weights.Time = -0.25; c.Weights.Service = 0.50 }, true},
		{"权重超1", func(c *Config) { c.Weights.Time = 1.5; c.Weights.Service = -0.25 }, true},
		{"阈值超1", func(c *Config) { c.Thresholds.Merge = 1.2 }, true},
		{"阈值为负", func(c *Config) { c.Thresholds.Review = -0.1 }, true},
		{"merge小于review", func(c *Config) { c.Thresholds.Merge = 0.40; c.Thresholds.Review = 0.60 }, true},
		{"merge等于review合法", func(c *Config) { c.Thresholds.Merge = 0.60; c.Thresholds.Review = 0.60 }, false},
		{"权重和含浮点毛刺仍合法", func(c *Config) { c.Weights.Historical = 0.1001 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("期望校验失败，实得 nil（配置 %+v）", cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望校验通过，实得错误: %v", err)
			}
		})
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	in := DefaultConfig()
	in.Thresholds.Merge = 0.85
	in.Thresholds.Review = 0.55
	in.Weights.Historical = 0.1001

	raw, err := MarshalConfigJSON(in)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	out, err := UnmarshalConfigJSON(raw)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if out != in {
		t.Fatalf("往返不一致：\n实得 %+v\n期望 %+v", out, in)
	}
}

func TestUnmarshalConfigJSON(t *testing.T) {
	t.Run("空输入退到默认", func(t *testing.T) {
		got, err := UnmarshalConfigJSON(nil)
		if err != nil {
			t.Fatalf("空输入不应报错: %v", err)
		}
		if got != DefaultConfig() {
			t.Fatalf("空输入应返回默认配置，实得 %+v", got)
		}
	})

	t.Run("残缺配置保留默认维度", func(t *testing.T) {
		got, err := UnmarshalConfigJSON([]byte(`{"thresholds":{"merge":0.9,"review":0.6}}`))
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if got.Thresholds.Merge != 0.9 || got.Thresholds.Review != 0.6 {
			t.Errorf("阈值未生效: %+v", got.Thresholds)
		}
		if got.Weights != DefaultConfig().Weights {
			t.Errorf("缺失的权重应保留默认值，实得 %+v", got.Weights)
		}
		if !got.Enabled {
			t.Error("缺失的 enabled 应保留默认 true")
		}
	})

	t.Run("可关闭引擎", func(t *testing.T) {
		got, err := UnmarshalConfigJSON([]byte(`{"enabled":false}`))
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if got.Enabled {
			t.Error("enabled=false 应生效")
		}
	})

	t.Run("非法JSON报错", func(t *testing.T) {
		if _, err := UnmarshalConfigJSON([]byte(`{`)); err == nil {
			t.Fatal("非法 JSON 应报错")
		}
	})
}

func TestParseLabels(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want map[string]string
	}{
		{"正常对象", json.RawMessage(`{"service":"checkout","namespace":"prod"}`),
			map[string]string{"service": "checkout", "namespace": "prod"}},
		{"空输入", nil, map[string]string{}},
		{"空对象", json.RawMessage(`{}`), map[string]string{}},
		{"null", json.RawMessage(`null`), map[string]string{}},
		{"非法JSON退化为空", json.RawMessage(`not-json`), map[string]string{}},
		{"非对象退化为空", json.RawMessage(`["a"]`), map[string]string{}},
		{"值非字符串退化为空", json.RawMessage(`{"a":1}`), map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseLabels(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseLabels = %v，期望 %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("ParseLabels[%q] = %q，期望 %q", k, got[k], v)
				}
			}
		})
	}
}

// TestNewAlertView 确认 service / namespace 一律从 labels 派生，
// 而非依赖调用方另行传入——这保证评分与指纹口径一致。
func TestNewAlertView(t *testing.T) {
	starts := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	ends := starts.Add(5 * time.Minute)

	t.Run("从labels派生", func(t *testing.T) {
		in := alert.StoredAlert{
			ID: 42,
			Alert: alert.Alert{
				Fingerprint: "fp-1",
				Severity:    alert.SeverityWarning,
				Status:      alert.StatusFiring,
				Labels:      json.RawMessage(`{"alertname":"LatencyHigh","service":"checkout","namespace":"prod"}`),
				StartsAt:    starts,
				EndsAt:      &ends,
			},
		}
		got := NewAlertView(&in)

		if got.ID != 42 || got.Fingerprint != "fp-1" {
			t.Errorf("ID/指纹投影错误: %+v", got)
		}
		if got.Service != "checkout" || got.Namespace != "prod" {
			t.Errorf("service/namespace 派生错误: %+v", got)
		}
		if !got.StartsAt.Equal(starts) || got.EndsAt == nil || !got.EndsAt.Equal(ends) {
			t.Errorf("时间窗投影错误: %+v", got)
		}
	})

	t.Run("labels缺失不panic", func(t *testing.T) {
		got := NewAlertView(&alert.StoredAlert{ID: 1})
		if got.Service != "" || got.Namespace != "" {
			t.Errorf("无 labels 时 service/namespace 应为空，实得 %+v", got)
		}
		if got.Labels == nil {
			t.Error("Labels 应兜成空 map 而非 nil")
		}
		if got.EndsAt != nil {
			t.Error("EndsAt 应保持 nil（仍 firing）")
		}
	})
}
