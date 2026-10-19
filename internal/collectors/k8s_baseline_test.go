package collectors

import (
	"context"
	"fmt"
	"testing"

	"github.com/yu/iip/internal/evidence"
)

// k8sWindow 是 [12:00, 13:00)，基线窗口因此是 [11:00, 12:00)。
// 造数据时用这两个整点，边界归属一眼可辨。
func baselineAt(hour, min, idx int) map[string]any {
	return map[string]any{
		"reason":    fmt.Sprintf("Base%02d", idx),
		"eventTime": fmt.Sprintf("2026-10-03T%02d:%02d:00Z", hour, min),
		"involvedObject": map[string]any{
			"name": fmt.Sprintf("checkout-%d", idx), "kind": "Pod",
		},
	}
}

func curAt(min, idx int) map[string]any {
	return map[string]any{
		"reason":         fmt.Sprintf("Cur%02d", idx),
		"eventTime":      fmt.Sprintf("2026-10-03T12:%02d:00Z", min),
		"involvedObject": map[string]any{"name": fmt.Sprintf("checkout-%d", idx)},
	}
}

// firstWarning 取第一条 Warning 事件证据，并断言其条数为 want。
//
// 条数必须显式断言而不是只取 [0]：若预期 2 条而实际出了 3 条，
// 只看 [0] 的判据仍然会「通过」，而多出来那条可能是别的窗口的
// 事件漏了进来 —— 那正是本组用例要防的错。
func firstWarning(t *testing.T, evs []evidence.Evidence, want int) evidence.Evidence {
	t.Helper()
	w := onlyWarnings(evs)
	if len(w) != want {
		t.Fatalf("Warning 事件条数 = %d，期望 %d\n%s", len(w), want, dumpEvidence(evs))
	}
	return w[0]
}

// 基线 > 0：给出可信倍数。6 条基线、3 条事故 ⇒0.5 倍。
func TestK8sEventChangeRatioAgainstBaseline(t *testing.T) {
	s := newK8sServer(t)
	items := []map[string]any{curAt(10, 1), curAt(20, 2), curAt(30, 3)}
	for i := 0; i < 6; i++ {
		items = append(items, baselineAt(11, 10+i, 100+i))
	}
	setEventsRaw(s, items...)

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 3)
	if ev.Metadata["matched_before"] != "6" {
		t.Errorf("matched_before = %q，期望 6（事故前窗口的 6 条）\n%s",
			ev.Metadata["matched_before"], dumpEvidence([]evidence.Evidence{ev}))
	}
	if ev.Metadata["change_ratio"] != "0.5000" {
		t.Errorf("change_ratio = %q，期望 0.5000（3/6）",
			ev.Metadata["change_ratio"])
	}
}

// **倍数必须随数据变化**：只断言「有 change_ratio」的话，
// 一个把change_ratio 写死成 1.0000 的退化实现会全绿。
// 因此必须与 TestK8sEventChangeRatioAgainstBaseline 并存，
// 两条用不同的真实数据算出不同的倍数。
func TestK8sEventChangeRatioVariesWithData(t *testing.T) {
	cases := []struct {
		name      string
		cur       int
		base      int
		wantRatio string
	}{
		{"事故前 8 条事故中 2 条", 2, 8, "0.2500"},
		{"事故前 2 条事故中 8 条", 8, 2, "4.0000"},
		{"前后各 3 条", 3, 3, "1.0000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newK8sServer(t)
			var items []map[string]any
			for i := 0; i < tc.cur; i++ {
				items = append(items, curAt(10+i, i))
			}
			for i := 0; i < tc.base; i++ {
				items = append(items, baselineAt(11, 10+i, 100+i))
			}
			setEventsRaw(s, items...)

			c := NewK8sCollector(s.srv.URL, "prod")
			got, err := c.Collect(context.Background(), k8sInput("checkout"))
			if err != nil {
				t.Fatalf("采集失败: %v", err)
			}
			ev := firstWarning(t, got, tc.cur)
			if ev.Metadata["change_ratio"] != tc.wantRatio {
				t.Errorf("change_ratio = %q，期望 %q（%d/%d）",
					ev.Metadata["change_ratio"], tc.wantRatio, tc.cur, tc.base)
			}
		})
	}
}

// 基线桶也必须过归属判定：别的服务的 Warning 不能进分母。
//
// 这与 belongsTo 里防的「checkout-staging 挂到 checkout 上」是同一类错挂，
// 方向相反：那边是多收证据，这边是**分母被撑大** ——
// 本服务其实一条没多，change_ratio 却显示暴涨，页面上完全看不出来。
func TestK8sBaselineBucketAlsoFiltersOwnership(t *testing.T) {
	s := newK8sServer(t)
	// 命名空间里同时有 checkout 与 checkout-staging。
	setDeployRaw(s,
		k8sDeployJSON{Name: "checkout", Replicas: 1, Ready: 1,
			Labels: map[string]string{"app": "checkout"},
			Conditions: []k8sCondJSON{{
				Type: "Available", Status: "True", LastUpdate: "2026-10-03T12:05:00Z",
			}}}.marshal(),
		k8sDeployJSON{Name: "checkout-staging", Replicas: 1, Ready: 1,
			Labels: map[string]string{"app": "checkout-staging"},
			Conditions: []k8sCondJSON{{
				Type: "Available", Status: "True", LastUpdate: "2026-10-03T12:06:00Z",
			}}}.marshal(),
	)
	// 基线窗口里 2 条属于 checkout、5 条属于 checkout-staging。
	var items []map[string]any
	items = append(items, curAt(10, 1), curAt(20, 2))
	for i := 0; i < 2; i++ {
		items = append(items, baselineAt(11, 10+i, 200+i))
	}
	for i := 0; i < 5; i++ {
		items = append(items, map[string]any{
			"reason":         fmt.Sprintf("StagingBase%02d", i),
			"eventTime":      fmt.Sprintf("2026-10-03T11:%02d:00Z", 20+i),
			"involvedObject": map[string]any{"name": fmt.Sprintf("checkout-staging-%d", i)},
		})
	}
	setEventsRaw(s, items...)

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 2)
	if ev.Metadata["matched_before"] != "2" {
		t.Errorf("matched_before = %q，期望 2 —— checkout-staging 的 5 条"+
			"混进分母了，那是别的服务的事故强度",
			ev.Metadata["matched_before"])
	}
	if ev.Metadata["change_ratio"] != "1.0000" {
		t.Errorf("change_ratio = %q，期望 1.0000（2/2，本服务其实没变）",
			ev.Metadata["change_ratio"])
	}
}

// 基线为 0：写 novelty，**不给** change_ratio，也**不写** baseline。
//
// 三者必须分别断言：把 novelty 换成 change_ratio="0.0000" 会让
// Assess 落进 BandWeak，语义从「没有可比数据」变成「评估结果很弱」。
func TestK8sEventBaselineZeroWritesNoveltyNotRatio(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, curAt(10, 1), curAt(20, 2))

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 2)
	if ev.Metadata["matched_before"] != "0" {
		t.Errorf("matched_before = %q，期望 0", ev.Metadata["matched_before"])
	}
	if _, ok := ev.Metadata["change_ratio"]; ok {
		t.Errorf("基线为 0 时不该给 change_ratio（实际 = %q）—— "+
			"「变化 0 倍」会被读成「强度很弱」",
			ev.Metadata["change_ratio"])
	}
	if ev.Metadata["novelty"] == "" {
		t.Error("基线为 0 时应写novelty 说明这是新出现的")
	}
	if _, ok := ev.Metadata["baseline"]; ok {
		t.Error("基线为 0 不等于「基线不可用」—— 写了 baseline 会让 Assess " +
			"落第①步并说「基线窗口不可用」，与事实相反")
	}
}

// TopN 截断不能污染基线：截断砍的是展示条数，不是真实命中数。
// 用截断后的条数当分子会让「20 条涨到 5 条」算成没变化。
func TestK8sTopNDoesNotPolluteRatio(t *testing.T) {
	s := newK8sServer(t)
	var items []map[string]any
	for i := 0; i < 20; i++ {
		items = append(items, curAt(i, i)) // 12:00..12:19
	}
	for i := 0; i < 5; i++ {
		items = append(items, baselineAt(11, 10+i, 300+i))
	}
	setEventsRaw(s, items...)

	c := NewK8sCollector(s.srv.URL, "prod", WithTopN(3))
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	w := onlyWarnings(got)
	if len(w) != 3 {
		t.Fatalf("事件条数 = %d，期望 3（TopN 生效）\n%s", len(w), dumpEvidence(w))
	}
	if w[0].Metadata["matched"] != "20" {
		t.Errorf("matched = %q，期望 20（截断前的真实命中数）",
			w[0].Metadata["matched"])
	}
	if w[0].Metadata["change_ratio"] != "4.0000" {
		t.Errorf("change_ratio = %q，期望 4.0000（20/5）—— "+
			"用截断后的 3 条当分子会算出 0.6000",
			w[0].Metadata["change_ratio"])
	}
	if w[0].Metadata["matched_before"] != "5" {
		t.Errorf("matched_before = %q，期望 5", w[0].Metadata["matched_before"])
	}
}
