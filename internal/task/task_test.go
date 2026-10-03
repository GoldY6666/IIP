package task

import (
	"testing"
	"time"
)

// TestBackoffDelay钉死指数退避的每一步与封顶。
//
// 这是本包里最容易写错的算术，且它直接决定一个坏任务多久被重试一次：
// 算短了会让坏任务疯狂重试打满 CPU，算长了会让故障拖到很久才恢复。
func TestBackoffDelay(t *testing.T) {
	const (
		base = 5 * time.Second
		max  = BackoffMax
	)

	cases := []struct {
		name     string
		attempts int
		want     time.Duration
		why      string
	}{
		{"首次失败退base", 1, 5 * time.Second, "第 1 次不该翻倍：还没失败过多次"},
		{"第2 次翻倍", 2, 10 * time.Second, "5s × 2^1"},
		{"第 3 次", 3, 20 * time.Second, "5s × 2^2"},
		{"第 4 次", 4, 40 * time.Second, "5s × 2^3，仍未到 5 分钟上限"},
		{"第 7 次恰好到上限", 7, 5 * time.Minute, "5s × 2^6 = 320s > 300s，封顶"},
		{"远超上限仍封顶", 20, max, "绝不能按公式算出几小时，否则坏任务永远不再被看见"},
		{"超大 attempts 不溢出", 200, max,
			"2^199 的 time.Duration 会溢出成负数，那样 available_at 落到过去" +
				"—— 任务会被立刻反复领取，正是要防的故障"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BackoffDelay(c.attempts, base, max)
			if got != c.want {
				t.Errorf("BackoffDelay(%d) = %s，期望 %s —— %s",
					c.attempts, got, c.want, c.why)
			}
		})
	}
}

// TestBackoffDelayClampsAttemptsBelowOne 覆盖 attempts=0。
//
// 0 不会出现在正常路径上（入队即attempts=0，但 Fail 只在领取后调用，
// 而领取会把它 +1）。但这是个防御性边界：若有人误传 0，
// 不该算出 base/2 或 0。
func TestBackoffDelayClampsAttemptsBelowOne(t *testing.T) {
	for _, a := range []int{0, -1, -100} {
		got := BackoffDelay(a, 5*time.Second, BackoffMax)
		if got != 5*time.Second {
			t.Errorf("BackoffDelay(%d) = %s，期望退到 base 5s（attempts<1 按 1 处理）",
				a, got)
		}
	}
}

// TestBackoffDelayWithNonPositiveBaseAndMax 覆盖零值参数。
//
// 调用方可能传零值（配置项被显式设为 0）。此时必须回退到包级默认，
// 而不是返回 0 间隔 —— 后者会让失败任务立即重试，形成打满 CPU 的热循环。
func TestBackoffDelayWithNonPositiveBaseAndMax(t *testing.T) {
	if got := BackoffDelay(3, 0, 0); got != 20*time.Second {
		t.Errorf("零值 base/max 时 BackoffDelay(3) = %s，期望按默认算得 20s", got)
	}
}

// TestShouldDeadLetter 钉死「最大 3 次」的确切含义。
//
// 判定用>= 而非 >：最大 3 次 = 允许尝试 3 次 = 第 3 次失败后进死信
// （即只重试 2 次）。写成 > 会实际尝试 4 次，与计划不符。
func TestShouldDeadLetter(t *testing.T) {
	cases := []struct {
		attempts, max int
		want          bool
		why           string
	}{
		{1, 3, false, "第 1 次失败：还有 2 次机会"},
		{2, 3, false, "第 2 次失败：还有最后 1 次"},
		{3, 3, true, "第 3 次失败：达到上限，进死信"},
		{4, 3, true, "超过上限（超时回收后仍可能再失败）：仍是死信"},
		{3, 0, true, "max<=0 时用默认 3，attempts=3 恰好到上限"},
		{1, 1, true, "最大 1 次意味着首次失败即死信"},
	}
	for _, c := range cases {
		if got := ShouldDeadLetter(c.attempts, c.max); got != c.want {
			t.Errorf("ShouldDeadLetter(%d, %d) = %v，期望 %v —— %s",
				c.attempts, c.max, got, c.want, c.why)
		}
	}
}

// TestShouldDeadLetterIsZeroValueSafe 覆盖 maxAttempts 为负数。
func TestShouldDeadLetterIsZeroValueSafe(t *testing.T) {
	// 负数上限不该让所有任务立刻判死信（那会让队列彻底不干活）。
	// 回退默认 3 的语义：attempts=1 仍应重试。
	if ShouldDeadLetter(1, -5) {
		t.Error("maxAttempts 为负时不应把首次失败判成死信（否则队列彻底停摆）")
	}
}

// TestTaskIncidentIDRejectsWrongKind 覆盖拿错 kind 的情形。
//
// 返回 0 会让调用方去查 incident 0，然后拿到「事件不存在」，
// 掩盖真正的问题（这个任务压根不是采集任务）。
func TestTaskIncidentIDRejectsWrongKind(t *testing.T) {
	payload, err := NewCollectEvidencePayload(12, time.Time{})
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}

	bad := Task{ID: 1, Kind: Kind("some_other_kind"), Payload: payload}
	if _, err := bad.IncidentID(); err == nil {
		t.Error("非采集任务解析 incident_id 应报错，实得 nil —— 会让调用方误查 incident 0")
	}

	// 采集任务但payload 类型不对
	garbage := Task{ID: 2, Kind: KindCollectEvidence, Payload: []byte(`{"incident_id":"abc"}`)}
	if _, err := garbage.IncidentID(); err == nil {
		t.Error("incident_id 类型非法应报错，实得 nil")
	}

	// 采集任务但 incident_id 为 0
	zero := Task{ID: 3, Kind: KindCollectEvidence,
		Payload: []byte(`{"incident_id":0}`)}
	if _, err := zero.IncidentID(); err == nil {
		t.Error("incident_id 为 0 应报错，实得 nil —— 0 不是合法主键")
	}
}

// TestTaskIncidentIDRoundTrip 覆盖正常路径。
func TestTaskIncidentIDRoundTrip(t *testing.T) {
	payload, err := NewCollectEvidencePayload(42, time.Time{})
	if err != nil {
		t.Fatalf("构造 payload 失败: %v", err)
	}
	tk := Task{ID: 7, Kind: KindCollectEvidence, Payload: payload}
	got, err := tk.IncidentID()
	if err != nil {
		t.Fatalf("解析 incident_id 失败: %v", err)
	}
	if got != 42 {
		t.Errorf("incident_id = %d，期望 42", got)
	}
}

// TestNewCollectEvidencePayloadRejectsNonPositive 覆盖入队参数校验。
func TestNewCollectEvidencePayloadRejectsNonPositive(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := NewCollectEvidencePayload(id, time.Time{}); err == nil {
			t.Errorf("NewCollectEvidencePayload(%d) 应报错，实得 nil —— "+
				"0/负数主键会在执行时查不到事件", id)
		}
	}
}

// TestIsTerminal 覆盖终态判定。
func TestIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		StatusPending:  false,
		StatusInFlight: false,
		StatusDone:     true,
		StatusDead:     true,
	}
	for st, want := range cases {
		tk := Task{Status: st}
		if got := tk.IsTerminal(); got != want {
			t.Errorf("Task{Status:%q}.IsTerminal() = %v，期望 %v", st, got, want)
		}
	}
}
