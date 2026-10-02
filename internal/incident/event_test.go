package incident

import (
	"testing"
	"time"
)

// TestEventTypeForTransition 断言「目标状态 → 事件类型」的映射逐项成立。
//
// 映射用目标状态做键（而不是 from→to 组合），因此只需 6 条即可覆盖全部流转；
// 若哪天有人把它改回组合键，这张表会立刻转红提醒「映射口径变了」。
func TestEventTypeForTransition(t *testing.T) {
	cases := []struct {
		to   Status
		want EventType
	}{
		{StatusOpen, EventReopened},
		{StatusAcknowledged, EventAcknowledged},
		{StatusInvestigating, EventInvestigationStarted},
		{StatusMitigated, EventMitigated},
		{StatusResolved, EventResolved},
		{StatusIgnored, EventIgnored},
	}
	for _, tc := range cases {
		got, err := EventTypeForTransition(tc.to)
		if err != nil {
			t.Errorf("EventTypeForTransition(%s) 报错: %v", tc.to, err)
			continue
		}
		if got != tc.want {
			t.Errorf("EventTypeForTransition(%s) = %q，期望 %q", tc.to, got, tc.want)
		}
	}
}

// TestEventTypeForTransitionUnknown 断言未知状态不会静默产出一条语义不明的留痕。
func TestEventTypeForTransitionUnknown(t *testing.T) {
	for _, bad := range []Status{"", "triage", "OPEN"} {
		if _, err := EventTypeForTransition(bad); err == nil {
			t.Errorf("EventTypeForTransition(%q) 应报错，实际通过", bad)
		}
	}
}

// TestNewTransitionEntry 断言构造器产出的字段形状（手册 §4.2）。
func TestNewTransitionEntry(t *testing.T) {
	ts := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	entry, err := NewTransitionEntry(11, StatusInvestigating, StatusResolved, "yu", ts)
	if err != nil {
		t.Fatalf("NewTransitionEntry 失败: %v", err)
	}
	if entry.IncidentID != 11 {
		t.Errorf("IncidentID = %d，期望 11", entry.IncidentID)
	}
	if entry.EventType != EventResolved {
		t.Errorf("EventType = %q，期望 %q", entry.EventType, EventResolved)
	}
	if entry.Actor != "yu" {
		t.Errorf("Actor = %q，期望 yu", entry.Actor)
	}
	if !entry.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v，期望 %v", entry.Timestamp, ts)
	}
	// metadata 固定带 from / to：前端渲染「从 X 到 Y」不该去解析 description 文本。
	if entry.Metadata["from"] != string(StatusInvestigating) {
		t.Errorf("metadata.from = %q，期望 investigating", entry.Metadata["from"])
	}
	if entry.Metadata["to"] != string(StatusResolved) {
		t.Errorf("metadata.to = %q，期望 resolved", entry.Metadata["to"])
	}
	// description 要能直接给人看，故必须同时出现两个状态名。
	if entry.Description == "" {
		t.Error("description 不应为空")
	}

	// 非法目标状态：构造留痕必须失败，而不是产出一条 event_type 为空的行。
	if _, err = NewTransitionEntry(11, StatusOpen, "triage", "yu", ts); err == nil {
		t.Error("目标状态非法时 NewTransitionEntry 应报错，实际通过")
	}
}

// TestResolveAtEffectIgnoredIsInert 单独守住「ignored 不写 resolved_at」这条决策。
//
// 这条规则反直觉（同为终态，resolved 写而 ignored 不写），最容易被后人「顺手统一」，
// 故单独一条用例并注明理由，避免被当成笔误改掉。
func TestResolveAtEffectIgnoredIsInert(t *testing.T) {
	eff := ResolveAtEffect(StatusOpen, StatusIgnored)
	if eff.SetResolvedNow || eff.ClearResolved {
		t.Errorf("进入 ignored 时 resolved_at 应保持原状，实际 effect = %+v", eff)
	}
}
