package collectors

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
	"github.com/yu/iip/internal/rca"
)

// 两窗必须不重叠：正好落在 Start 的事件算**事故桶**，不算基线桶。
//
// 数据：11:59:59（基线桶唯一一条）+ 12:00:00（事故桶唯一一条）。
// 正确实现的两个桶各1 条。若基线窗口写成 [Start-width, Start]（闭区间），
// 12:00:00 会被重复计入，matched_before 变成 2 ——
// 倍数被压低，而页面上看不出任何异常。
func TestK8sBaselineWindowDoesNotOverlapIncident(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s,
		map[string]any{"reason": "BeforeStart", "eventTime": "2026-10-03T11:59:59Z",
			"involvedObject": map[string]any{"name": "checkout-1"}},
		map[string]any{"reason": "AtStart", "eventTime": "2026-10-03T12:00:00Z",
			"involvedObject": map[string]any{"name": "checkout-2"}},
	)
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 1)
	if !strings.Contains(ev.Value, "AtStart") {
		t.Fatalf("留下的不是 AtStart 那条: %q", ev.Value)
	}
	if ev.Metadata["matched"] != "1" {
		t.Errorf("matched = %q，期望 1", ev.Metadata["matched"])
	}
	if ev.Metadata["matched_before"] != "1" {
		t.Errorf("matched_before = %q，期望 1 —— 基线桶只能有 11:59:59 那条；"+
			"若基线窗口是闭区间，落在 Start 的这条会被重复计入",
			ev.Metadata["matched_before"])
	}
}

// 基线窗口也要过时间解析：无时间戳的事件不能进基线桶当分母。
func TestK8sBaselineBucketSkipsEventsWithoutTimestamp(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s,
		map[string]any{"reason": "NoTime", "message": "m",
			"involvedObject": map[string]any{"name": "checkout-1"}},
		curAt(10, 1),
	)
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 1)
	if ev.Metadata["matched_before"] != "0" {
		t.Errorf("matched_before = %q，期望 0 —— 无时间的事件不能进基线桶"+
			"（§4.4：不用采集时刻兜底）", ev.Metadata["matched_before"])
	}
}

// rollout 必须说清「为什么给不出倍数」，而不是静默缺键。
//
// 静默缺键时 Assess 落第②步，basis 说「缺少 change_ratio，无法判断变化幅度」
// —— 那把设计取舍说成了采集器缺陷，运维会去查代码而不是查集群。
func TestK8sRolloutExplainsMissingBaseline(t *testing.T) {
	s := newK8sServer(t)
	setDeployRaw(s, k8sDeployJSON{
		Name: "checkout", Generation: 7, Labels: map[string]string{"app": "checkout"},
		Replicas: 3, Ready: 1, Updated: 1, Unavailable: 2,
		Conditions: []k8sCondJSON{{
			Type: "Progressing", Status: "False",
			Reason: "ProgressDeadlineExceeded", LastUpdate: "2026-10-03T12:40:00Z",
		}},
	}.marshal())
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	roll := onlyOneWarning2(t, got, "rollout")
	if roll.Metadata["baseline"] == "" {
		t.Error("rollout 缺 baseline 说明键 —— Assess 会说「缺少 change_ratio」，" +
			"把「K8s 没有历史数据」说成「采集器忘了写」")
	}
	if !strings.Contains(roll.Metadata["baseline"], "历史副本数") {
		t.Errorf("baseline 文案未说清原因（应指出 K8s API 不保留历史副本数）: %q",
			roll.Metadata["baseline"])
	}
	if _, ok := roll.Metadata["change_ratio"]; ok {
		t.Errorf("rollout 不该有 change_ratio（实际 = %q）—— "+
			"拿同一时刻的 ready/replicas 当比值，语义是「当前就绪比例」不是「变化」",
			roll.Metadata["change_ratio"])
	}
}

// rollout 与 Warning 事件同为 deployment 类型，但一个可评估一个不可评估。
// 必须分别验：混在一起数条数会让「事件侧补了基线」这个改动看不出效果。
func TestK8sEventAssessableAndRolloutNot(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, curAt(10, 1), curAt(20, 2), baselineAt(11, 10, 400))
	setDeployRaw(s, k8sDeployJSON{
		Name: "checkout", Generation: 3, Labels: map[string]string{"app": "checkout"},
		Replicas: 2, Ready: 2,
		Conditions: []k8sCondJSON{{
			Type: "Available", Status: "True", LastUpdate: "2026-10-03T12:30:00Z",
		}},
	}.marshal())

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	ev := firstWarning(t, got, 2)
	if band, _ := rca.Assess(ev); band == rca.BandUnassessable {
		t.Errorf("有基线的 Warning 事件被判为不可评估 —— matched_before=%q change_ratio=%q",
			ev.Metadata["matched_before"], ev.Metadata["change_ratio"])
	}
	roll := onlyOneWarning2(t, got, "rollout")
	band2, basis2 := rca.Assess(roll)
	if band2 != rca.BandUnassessable {
		t.Errorf("rollout band = %v，期望 Unassessable", band2)
	}
	// 第①步（baseline 键存在）而非第②步（缺 change_ratio）：
	// 前者说「基线不可用」，后者说「缺少 change_ratio」——
	// 后者会让人以为采集器漏写。
	if !strings.Contains(basis2, "基线窗口不可用") {
		t.Errorf("rollout 的 basis = %q，期望命中第①步「基线窗口不可用」", basis2)
	}
}

// onlyOneWarning2 取指定 kind 的唯一一条证据。
func onlyOneWarning2(t *testing.T, evs []evidence.Evidence, kind string) evidence.Evidence {
	t.Helper()
	var out []evidence.Evidence
	for _, e := range evs {
		if e.Metadata["kind"] == kind {
			out = append(out, e)
		}
	}
	if len(out) != 1 {
		t.Fatalf("kind=%s 的证据条数 = %d，期望 1\n%s", kind, len(out), dumpEvidence(evs))
	}
	return out[0]
}

// 基线窗口函数的边界：等宽、紧邻、不重叠。
func TestBaselineWindowShape(t *testing.T) {
	w := k8sWindow() // [12:00, 13:00)
	b := baselineWindow(w)
	if !b.Start.Equal(time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("基线起点 = %v，期望 11:00", b.Start)
	}
	if !b.End.Equal(w.Start) {
		t.Errorf("基线终点 = %v，期望等于事故起点 %v（两窗必须紧邻）", b.End, w.Start)
	}
	if b.Duration() != w.Duration() {
		t.Errorf("基线宽度 = %v，期望等于事故窗口宽度 %v（不等宽则总量不可比）",
			b.Duration(), w.Duration())
	}
	if b.Start.Before(b.End) != w.Start.Before(w.End) {
		t.Error("基线窗口方向与事故窗口不一致")
	}
}
