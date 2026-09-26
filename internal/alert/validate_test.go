package alert

import (
	"encoding/json"
	"testing"
)

// TestParsePayload 覆盖 U-10 / U-11 的前置：结构级解析。
func TestParsePayload(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "合法 payload", body: `{"version":"4","status":"firing","alerts":[]}`},
		{name: "非 JSON", body: `not-json`, wantErr: true},
		{name: "JSON 数组而非对象", body: `[1,2]`, wantErr: true},
		{
			name: "忽略未知字段（向前兼容）",
			body: `{"version":"4","alerts":[],"brandNewUpstreamField":{"x":1}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePayload([]byte(tt.body))
			if tt.wantErr && err == nil {
				t.Error("期望报错，实际成功")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("期望成功，实际报错: %v", err)
			}
		})
	}
}

// TestValidatePayload 结构级校验失败意味着整批 400。
func TestValidatePayload(t *testing.T) {
	tests := []struct {
		name    string
		payload *Payload
		wantErr bool
	}{
		{name: "完整 payload", payload: &Payload{Version: "4", Alerts: []json.RawMessage{}}},
		{name: "nil payload", payload: nil, wantErr: true},
		{name: "缺 version", payload: &Payload{Alerts: []json.RawMessage{}}, wantErr: true},
		{name: "version 仅空白", payload: &Payload{Version: "  ", Alerts: []json.RawMessage{}}, wantErr: true},
		{name: "alerts 缺失（null）", payload: &Payload{Version: "4"}, wantErr: true},
		{name: "alerts 为空数组属合法", payload: &Payload{Version: "4", Alerts: []json.RawMessage{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePayload(tt.payload)
			if tt.wantErr && err == nil {
				t.Error("期望报错，实际通过")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// TestDecodeRawAlert 单条原始 JSON 的解码。
func TestDecodeRawAlert(t *testing.T) {
	ok := json.RawMessage(`{"status":"firing","labels":{"alertname":"HighLatency"},"startsAt":"2026-09-24T18:31:00Z"}`)
	got, err := DecodeRawAlert(ok)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got.Labels[LabelAlertname] != "HighLatency" {
		t.Errorf("labels.alertname = %q", got.Labels[LabelAlertname])
	}

	if _, err := DecodeRawAlert(json.RawMessage(`{bad json`)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// TestValidateAlert 覆盖 U-10 / U-11：单条字段级校验。
func TestValidateAlert(t *testing.T) {
	valid := func() RawAlert {
		return RawAlert{
			Status:   StatusFiring,
			Labels:   baseLabels(),
			StartsAt: "2026-09-24T18:31:00Z",
		}
	}

	tests := []struct {
		name    string
		mutate  func(a *RawAlert)
		wantErr bool
	}{
		{name: "合法单条", mutate: func(*RawAlert) {}},
		{
			name: "U-10 labels 无 alertname",
			mutate: func(a *RawAlert) {
				a.Labels = map[string]string{"service": "checkout"}
			},
			wantErr: true,
		},
		{
			name: "U-10 alertname 仅空白",
			mutate: func(a *RawAlert) {
				a.Labels = map[string]string{LabelAlertname: "   "}
			},
			wantErr: true,
		},
		{
			name:    "status 为 resolved 合法",
			mutate:  func(a *RawAlert) { a.Status = StatusResolved },
			wantErr: false,
		},
		{
			name:    "status 缺失",
			mutate:  func(a *RawAlert) { a.Status = "" },
			wantErr: true,
		},
		{
			name:    "status 为 suppressed（Alertmanager 不会推）",
			mutate:  func(a *RawAlert) { a.Status = StatusSuppressed },
			wantErr: true,
		},
		{
			name:    "U-11 startsAt 非法格式",
			mutate:  func(a *RawAlert) { a.StartsAt = "2026/09/24 18:31" },
			wantErr: true,
		},
		{
			name:    "U-11 startsAt 为空",
			mutate:  func(a *RawAlert) { a.StartsAt = "" },
			wantErr: true,
		},
		{
			name:    "startsAt 带小数秒合法",
			mutate:  func(a *RawAlert) { a.StartsAt = "2026-09-24T18:31:00.5Z" },
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := valid()
			tt.mutate(&a)
			err := ValidateAlert(&a)
			if tt.wantErr && err == nil {
				t.Error("期望报错，实际通过")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
		})
	}

	if err := ValidateAlert(nil); err == nil {
		t.Error("nil 告警应报错")
	}
}
