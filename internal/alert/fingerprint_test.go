package alert

import (
	"strings"
	"testing"
)

// baseLabels 是「同一条告警规则」的基准标签集合（含一个动态标签 instance）。
func baseLabels() map[string]string {
	return map[string]string{
		"alertname": "HighLatency",
		"service":   "checkout",
		"namespace": "production",
		"severity":  "critical",
		"instance":  "checkout-abc123",
	}
}

// TestGenerateFingerprint 覆盖手册 §7.2 的 U-01 ~ U-05。
func TestGenerateFingerprint(t *testing.T) {
	base := GenerateFingerprint(baseLabels())

	tests := []struct {
		name     string
		labels   map[string]string
		sameAsBa bool
	}{
		{
			name: "U-01 label 键顺序不同 → 同一指纹",
			labels: map[string]string{
				"instance":  "checkout-abc123",
				"severity":  "critical",
				"namespace": "production",
				"service":   "checkout",
				"alertname": "HighLatency",
			},
			sameAsBa: true,
		},
		{
			name: "U-02 仅 instance 变化 → 同一指纹（跨实例重建去重的前提）",
			labels: map[string]string{
				"alertname": "HighLatency",
				"service":   "checkout",
				"namespace": "production",
				"severity":  "critical",
				"instance":  "checkout-zzz999",
			},
			sameAsBa: true,
		},
		{
			name: "U-03 仅 pod/container/node 变化 → 同一指纹",
			labels: map[string]string{
				"alertname": "HighLatency",
				"service":   "checkout",
				"namespace": "production",
				"severity":  "critical",
				"instance":  "checkout-abc123",
				"pod":       "checkout-7f9c-x2k",
				"container": "app",
				"node":      "ip-10-0-1-23",
			},
			sameAsBa: true,
		},
		{
			name: "U-04 alertname 不同 → 不同指纹",
			labels: map[string]string{
				"alertname": "HighErrorRate",
				"service":   "checkout",
				"namespace": "production",
				"severity":  "critical",
				"instance":  "checkout-abc123",
			},
			sameAsBa: false,
		},
		{
			name: "U-05 新增非动态 label → 不同指纹（稳定标签变了就是另一条规则）",
			labels: map[string]string{
				"alertname": "HighLatency",
				"service":   "checkout",
				"namespace": "production",
				"severity":  "critical",
				"instance":  "checkout-abc123",
				"region":    "cn-north",
			},
			sameAsBa: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateFingerprint(tt.labels)
			switch {
			case tt.sameAsBa && got != base:
				t.Errorf("期望与基准指纹相同\n基准 = %s\n实际 = %s", base, got)
			case !tt.sameAsBa && got == base:
				t.Errorf("期望与基准指纹不同，实际相同 = %s", got)
			}
		})
	}
}

// TestGenerateFingerprintShape 校验指纹的外部形态：hex、定长 64。
func TestGenerateFingerprintShape(t *testing.T) {
	got := GenerateFingerprint(baseLabels())
	if len(got) != 64 {
		t.Fatalf("SHA256 hex 应为 64 字符，实际 %d 字符: %s", len(got), got)
	}
	if got != strings.ToLower(got) {
		t.Errorf("hex 应为小写，实际 %s", got)
	}
}

// TestGenerateFingerprintEmptyLabels 空标签集不得 panic，且结果确定。
func TestGenerateFingerprintEmptyLabels(t *testing.T) {
	first := GenerateFingerprint(nil)
	second := GenerateFingerprint(map[string]string{})
	if first != second {
		t.Errorf("nil 与空 map 应得到同一指纹: %s / %s", first, second)
	}
	if len(first) != 64 {
		t.Errorf("空标签集也应有定长指纹，实际 %d 字符", len(first))
	}
}

// TestDynamicLabelKeys 锁定排除表内容：改动它等于改动去重语义，必须显式改测试。
func TestDynamicLabelKeys(t *testing.T) {
	want := map[string]struct{}{"instance": {}, "pod": {}, "container": {}, "node": {}}
	if len(DynamicLabelKeys) != len(want) {
		t.Fatalf("DynamicLabelKeys 数量应为 %d，实际 %d", len(want), len(DynamicLabelKeys))
	}
	for _, k := range DynamicLabelKeys {
		if _, ok := want[k]; !ok {
			t.Errorf("DynamicLabelKeys 含预期外键 %q", k)
		}
	}
}
