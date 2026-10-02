package incident

import (
	"errors"
	"strings"
	"testing"
)

// wantMatrix 是 6×6 转换矩阵的**期望真值表**，行 = from、列 = to，顺序与 ladder 一致
// （open, acknowledged, investigating, mitigated, resolved, ignored）。
//
// 直接抄自手册 §4.1 的规范表，而不是由 CanTransition 推导而来——若用实现自己
// 生成期望值，实现写错时矩阵会跟着一起错，测试就变成了同义反复。
var wantMatrix = [][]bool{
	//        open   ack    inv    mit    res    ign
	/*open*/ {false, true, true, true, true, true},
	/*ack*/ {false, false, true, true, true, true},
	/*inv*/ {false, false, false, true, true, true},
	/*mit*/ {false, false, false, false, true, true},
	/*res*/ {true, false, false, false, false, false},
	/*ign*/ {true, false, false, false, false, false},
}

// specTable 是手册 §4.1「转换白名单」逐行的允许集合，用于与矩阵交叉验证。
//
// 矩阵与规范表是同一规则的两种写法：两者都断言，任何一方被改错都会立刻暴露，
// 也顺带证明「阶序口径」与「规范表」确实等价（手册声称等价，这里让它可验证）。
var specTable = map[Status][]Status{
	StatusOpen:          {StatusAcknowledged, StatusInvestigating, StatusMitigated, StatusResolved, StatusIgnored},
	StatusAcknowledged:  {StatusInvestigating, StatusMitigated, StatusResolved, StatusIgnored},
	StatusInvestigating: {StatusMitigated, StatusResolved, StatusIgnored},
	StatusMitigated:     {StatusResolved, StatusIgnored},
	StatusResolved:      {StatusOpen},
	StatusIgnored:       {StatusOpen},
}

// TestCanTransitionMatrix 逐格断言 6×6 = 36 种组合，一格不落。
//
// 只测手册点名的几条（如 open→mitigated）会漏掉「某个格子被写反」的情况：
// 真实运维里越级流转很常见，每一格都可能被走到。
func TestCanTransitionMatrix(t *testing.T) {
	if len(wantMatrix) != len(ladder) {
		t.Fatalf("期望矩阵应有 %d 行，实际 %d 行", len(ladder), len(wantMatrix))
	}
	checked := 0
	for i, from := range ladder {
		if len(wantMatrix[i]) != len(ladder) {
			t.Fatalf("期望矩阵第 %d 行应有 %d 列，实际 %d 列", i, len(ladder), len(wantMatrix[i]))
		}
		for j, to := range ladder {
			checked++
			if got := CanTransition(from, to); got != wantMatrix[i][j] {
				t.Errorf("CanTransition(%s → %s) = %v，期望 %v", from, to, got, wantMatrix[i][j])
			}
		}
	}
	if checked != 36 {
		t.Fatalf("矩阵应覆盖 36 格，实际只检查了 %d 格", checked)
	}
}

// TestCanTransitionMatchesSpecTable 用规范表的「允许集合」独立复核矩阵。
func TestCanTransitionMatchesSpecTable(t *testing.T) {
	for _, from := range ladder {
		allowed := make(map[Status]bool, len(specTable[from]))
		for _, to := range specTable[from] {
			allowed[to] = true
		}
		for _, to := range ladder {
			want := allowed[to]
			if got := CanTransition(from, to); got != want {
				t.Errorf("规范表不一致: CanTransition(%s → %s) = %v，规范表期望 %v", from, to, got, want)
			}
		}
	}
}

// TestCanTransitionTerminalAndReopen 单独守住两条最容易写反的规则。
func TestCanTransitionTerminalAndReopen(t *testing.T) {
	cases := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"终态只能重开为 open", StatusResolved, StatusIgnored, false},
		{"终态不可互转", StatusIgnored, StatusResolved, false},
		{"终态可重开", StatusResolved, StatusOpen, true},
		{"误报可重开", StatusIgnored, StatusOpen, true},
		{"非终态不能回到 open", StatusInvestigating, StatusOpen, false},
		{"同状态不算转换", StatusMitigated, StatusMitigated, false},
		{"越级前向合法", StatusOpen, StatusMitigated, true},
		{"回退非法", StatusInvestigating, StatusAcknowledged, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("CanTransition(%s → %s) = %v，期望 %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestCanTransitionUnknownStatus 断言未知状态一律不放行。
//
// 这条是防止「有人给 CanTransition 传了空串/拼错的值，indexOf 返回 -1，
// 却因为 -1 > -1 之类的比较被判成合法」。
func TestCanTransitionUnknownStatus(t *testing.T) {
	const bogus Status = "triage" // 手册举过的非法值，不在六个取值内
	for _, s := range ladder {
		if CanTransition(bogus, s) {
			t.Errorf("未知状态 %q 不应能转到 %s", bogus, s)
		}
		if CanTransition(s, bogus) {
			t.Errorf("%s 不应能转到未知状态 %q", s, bogus)
		}
	}
	if CanTransition("", "") {
		t.Error("空状态不应被判为合法转换")
	}
}

// TestNewTransitionError 断言每种非法原因都被正确归类，且错误文案含 from 与 to。
func TestNewTransitionError(t *testing.T) {
	cases := []struct {
		name   string
		from   Status
		to     Status
		reason Reason
	}{
		{"同状态", StatusOpen, StatusOpen, ReasonSameStatus},
		{"终态未重开", StatusResolved, StatusIgnored, ReasonTerminal},
		{"回退", StatusInvestigating, StatusAcknowledged, ReasonBackward},
		{"未知状态", StatusOpen, "triage", ReasonUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewTransitionError(tc.from, tc.to)
			if err == nil {
				t.Fatalf("NewTransitionError(%s → %s) 应返回错误，实际返回 nil", tc.from, tc.to)
			}
			var e *InvalidTransitionError
			if !errors.As(err, &e) {
				t.Fatalf("期望 *InvalidTransitionError，实际 %T", err)
			}
			if e.Reason != tc.reason {
				t.Errorf("原因 = %q，期望 %q", e.Reason, tc.reason)
			}
			// 手册 §4.1 要求 error 文案含 from 与 to，便于前端直接展示。
			msg := e.Error()
			if !strings.Contains(msg, string(tc.from)) || !strings.Contains(msg, string(tc.to)) {
				t.Errorf("错误文案应同时含 from 与 to，实际: %s", msg)
			}
			if e.Code() != CodeInvalidTransition {
				t.Errorf("错误码 = %q，期望 %q", e.Code(), CodeInvalidTransition)
			}
		})
	}
}

// TestNewTransitionErrorNilWhenLegal 断言合法转换不产生错误（避免调用方误报）。
func TestNewTransitionErrorNilWhenLegal(t *testing.T) {
	if err := NewTransitionError(StatusOpen, StatusInvestigating); err != nil {
		t.Errorf("合法转换不应返回错误，实际: %v", err)
	}
}

// TestParseStatus 覆盖解析的正反例。
func TestParseStatus(t *testing.T) {
	for _, s := range ladder {
		got, err := ParseStatus(string(s))
		if err != nil {
			t.Errorf("ParseStatus(%q) 不应报错: %v", s, err)
			continue
		}
		if got != s {
			t.Errorf("ParseStatus(%q) = %q，期望 %q", s, got, s)
		}
	}

	for _, raw := range []string{"", "TRIAGE", "Open", "closed"} {
		if _, err := ParseStatus(raw); err == nil {
			t.Errorf("ParseStatus(%q) 应报错，实际通过", raw)
		} else if !strings.Contains(err.Error(), StatusList()) {
			t.Errorf("错误信息应列出合法取值，实际: %v", err)
		}
	}
}

// TestIsTerminal 与 TestIsValid 守住两个判定辅助函数。
func TestIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		StatusOpen:          false,
		StatusAcknowledged:  false,
		StatusInvestigating: false,
		StatusMitigated:     false,
		StatusResolved:      true,
		StatusIgnored:       true,
	}
	for _, s := range ladder {
		if got := IsTerminal(s); got != cases[s] {
			t.Errorf("IsTerminal(%s) = %v，期望 %v", s, got, cases[s])
		}
		if !IsValid(s) {
			t.Errorf("IsValid(%s) 应为 true", s)
		}
	}
	if IsValid("triage") {
		t.Error("IsValid(\"triage\") 应为 false")
	}
}

// TestAllStatusesReturnsCopy 断言 AllStatuses 返回副本：
// 调用方改写返回值不应影响状态机（ladder 是本包的判定依据）。
func TestAllStatusesReturnsCopy(t *testing.T) {
	got := AllStatuses()
	if len(got) != len(ladder) {
		t.Fatalf("AllStatuses() 长度 = %d，期望 %d", len(got), len(ladder))
	}
	got[0] = "tampered"
	if ladder[0] != StatusOpen {
		t.Fatalf("改写 AllStatuses() 的返回值污染了 ladder[0]，实际 = %q", ladder[0])
	}
	if !CanTransition(StatusOpen, StatusMitigated) {
		t.Error("ladder 被污染后状态机行为已改变")
	}
}

// TestLadderOrder 固定阶序本身。
//
// 矩阵断言已经隐含了顺序，但顺序若被重排，矩阵会整体转红而看不出「只是顺序变了」；
// 单独固定它，能让「有人调整了状态阶序」这件事有一条清晰的失败信息。
func TestLadderOrder(t *testing.T) {
	want := []Status{
		StatusOpen, StatusAcknowledged, StatusInvestigating,
		StatusMitigated, StatusResolved, StatusIgnored,
	}
	if len(ladder) != len(want) {
		t.Fatalf("ladder 长度 = %d，期望 %d", len(ladder), len(want))
	}
	for i := range want {
		if ladder[i] != want[i] {
			t.Errorf("ladder[%d] = %q，期望 %q", i, ladder[i], want[i])
		}
	}
	// 两个终态必须位于阶序末端，否则「任意非终态 → ignored」不再天然成立。
	if !IsTerminal(ladder[len(ladder)-1]) || !IsTerminal(ladder[len(ladder)-2]) {
		t.Error("resolved / ignored 必须位于 ladder 末端")
	}
}
