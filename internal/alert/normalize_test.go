package alert

import (
	"encoding/json"
	"testing"
	"time"
)

// TestMapSeverity 覆盖 severity 归一映射表与兜底（手册 §7.2 的 U-07 / U-08）。
func TestMapSeverity(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
		known bool
	}{
		{name: "critical 原样", in: "critical", want: SeverityCritical, known: true},
		{name: "大写输入", in: "CRITICAL", want: SeverityCritical, known: true},
		{name: "首尾空格", in: "  warning  ", want: SeverityWarning, known: true},
		{name: "page 归 critical", in: "page", want: SeverityCritical, known: true},
		{name: "emergency 归 critical", in: "emergency", want: SeverityCritical, known: true},
		{name: "fatal 归 critical", in: "fatal", want: SeverityCritical, known: true},
		{name: "error 归 critical", in: "error", want: SeverityCritical, known: true},
		{name: "warn 归 warning", in: "warn", want: SeverityWarning, known: true},
		{name: "notice 归 info", in: "notice", want: SeverityInfo, known: true},
		{name: "none 归 info", in: "none", want: SeverityInfo, known: true},
		{name: "debug 归 info", in: "debug", want: SeverityInfo, known: true},
		{name: "空串走兜底", in: "", want: unknownSeverityFallback, known: false},
		{name: "未知值走兜底", in: "sev2", want: unknownSeverityFallback, known: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := MapSeverity(tt.in)
			if got != tt.want {
				t.Errorf("MapSeverity(%q) = %q，期望 %q", tt.in, got, tt.want)
			}
			if known != tt.known {
				t.Errorf("MapSeverity(%q) known = %v，期望 %v", tt.in, known, tt.known)
			}
		})
	}
}

// TestNormalizeEndsAt 覆盖 U-06：endsAt 哨兵值必须映射为 NULL。
func TestNormalizeEndsAt(t *testing.T) {
	tests := []struct {
		name   string
		endsAt string
		want   bool // true = 期望为 NULL
	}{
		{name: "U-06 哨兵值 0001-01-01 → NULL", endsAt: endsAtSentinel, want: true},
		{name: "字段缺失（空串）→ NULL", endsAt: "", want: true},
		{name: "真实结束时间 → 非 NULL", endsAt: "2026-09-24T19:00:00Z", want: false},
		{name: "带小数秒 → 非 NULL", endsAt: "2026-09-24T19:00:00.123456789Z", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := RawAlert{
				Status:   StatusFiring,
				Labels:   baseLabels(),
				StartsAt: "2026-09-24T18:31:00Z",
				EndsAt:   tt.endsAt,
			}
			got, _, err := Normalize(&raw, json.RawMessage(`{}`), StatusFiring)
			if err != nil {
				t.Fatalf("Normalize 返回意外错误: %v", err)
			}
			if tt.want && got.EndsAt != nil {
				t.Errorf("期望 ends_at 为 NULL，实际 %v", got.EndsAt)
			}
			if !tt.want && got.EndsAt == nil {
				t.Errorf("期望 ends_at 非 NULL，实际为 NULL")
			}
		})
	}
}

// TestNormalizeSeverityFallback 覆盖 U-08：未知 severity 兜底为 info 并给出 warning。
func TestNormalizeSeverityFallback(t *testing.T) {
	tests := []struct {
		name         string
		severity     string
		wantSeverity string
		wantWarnings int
	}{
		{name: "已知值无告警", severity: "page", wantSeverity: SeverityCritical, wantWarnings: 0},
		{name: "U-08 缺失 severity", severity: "", wantSeverity: SeverityInfo, wantWarnings: 1},
		{name: "未知 severity", severity: "sev2", wantSeverity: SeverityInfo, wantWarnings: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := baseLabels()
			if tt.severity == "" {
				delete(labels, labelSeverity)
			} else {
				labels[labelSeverity] = tt.severity
			}
			raw := RawAlert{
				Status:   StatusFiring,
				Labels:   labels,
				StartsAt: "2026-09-24T18:31:00Z",
			}
			got, warnings, err := Normalize(&raw, json.RawMessage(`{}`), StatusFiring)
			if err != nil {
				t.Fatalf("Normalize 返回意外错误: %v", err)
			}
			if got.Severity != tt.wantSeverity {
				t.Errorf("severity = %q，期望 %q", got.Severity, tt.wantSeverity)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings = %d 条（%v），期望 %d 条", len(warnings), warnings, tt.wantWarnings)
			}
		})
	}
}

// TestNormalizeNullLabels 覆盖 U-09：nil 的 labels/annotations 必须兜成 {}。
func TestNormalizeNullLabels(t *testing.T) {
	raw := RawAlert{
		Status:      StatusFiring,
		Labels:      baseLabels(),
		Annotations: nil,
		StartsAt:    "2026-09-24T18:31:00Z",
	}
	got, _, err := Normalize(&raw, json.RawMessage(`{"a":1}`), StatusFiring)
	if err != nil {
		t.Fatalf("Normalize 返回意外错误: %v", err)
	}

	if string(got.Annotations) != "{}" {
		t.Errorf("annotations 应兜成 {}，实际 %s", got.Annotations)
	}
	if len(got.Labels) == 0 || string(got.Labels) == "null" {
		t.Errorf("labels 不应为空或 null，实际 %s", got.Labels)
	}

	// 空 labels 也必须兜成 {}。
	empty := RawAlert{Status: StatusFiring, Labels: nil, StartsAt: "2026-09-24T18:31:00Z"}
	gotEmpty, _, err := Normalize(&empty, json.RawMessage(`{}`), StatusFiring)
	if err != nil {
		t.Fatalf("Normalize 返回意外错误: %v", err)
	}
	if string(gotEmpty.Labels) != "{}" {
		t.Errorf("空 labels 应兜成 {}，实际 %s", gotEmpty.Labels)
	}
}

// TestNormalizeStatusPrecedence 顶层 status 与单条不一致时以单条为准并给出 warning。
func TestNormalizeStatusPrecedence(t *testing.T) {
	raw := RawAlert{
		Status:   StatusResolved,
		Labels:   baseLabels(),
		StartsAt: "2026-09-24T18:31:00Z",
		EndsAt:   "2026-09-24T19:00:00Z",
	}
	got, warnings, err := Normalize(&raw, json.RawMessage(`{}`), StatusFiring)
	if err != nil {
		t.Fatalf("Normalize 返回意外错误: %v", err)
	}
	if got.Status != StatusResolved {
		t.Errorf("status 应以单条为准 = %q，实际 %q", StatusResolved, got.Status)
	}
	if len(warnings) == 0 {
		t.Error("顶层与单条不一致时应产生 warning")
	}
}

// TestNormalizeFixedFields 锁定不变式：source / incident_id / raw_payload / 指纹来源。
func TestNormalizeFixedFields(t *testing.T) {
	labels := baseLabels()
	rawJSON := json.RawMessage(`{"status":"firing","labels":{"alertname":"HighLatency"}}`)
	raw := RawAlert{
		Status:      StatusFiring,
		Labels:      labels,
		StartsAt:    "2026-09-24T18:31:00Z",
		Fingerprint: "alertmanager-自算指纹-必须被忽略",
	}
	got, _, err := Normalize(&raw, rawJSON, StatusFiring)
	if err != nil {
		t.Fatalf("Normalize 返回意外错误: %v", err)
	}

	if got.Source != SourceAlertmanager {
		t.Errorf("source 应为 %q，实际 %q", SourceAlertmanager, got.Source)
	}
	if got.IncidentID != nil {
		t.Errorf("M1 的 incident_id 必须为 nil，实际 %v", *got.IncidentID)
	}
	if string(got.RawPayload) != string(rawJSON) {
		t.Errorf("raw_payload 应原样保留，实际 %s", got.RawPayload)
	}
	if got.Fingerprint == raw.Fingerprint {
		t.Error("必须忽略 payload 自带的 fingerprint，改用自算值")
	}
	if want := GenerateFingerprint(labels); got.Fingerprint != want {
		t.Errorf("fingerprint = %s，期望自算值 %s", got.Fingerprint, want)
	}
	if wantStarts := time.Date(2026, 9, 24, 18, 31, 0, 0, time.UTC); !got.StartsAt.Equal(wantStarts) {
		t.Errorf("starts_at = %v，期望 %v", got.StartsAt, wantStarts)
	}
}
