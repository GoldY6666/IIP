package collectors

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 核心用例：Warning 事件与 rollout 各产出一条，时间都来自源数据。
func TestK8sCollectsWarningEventsAndRollouts(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, k8sEventJSON{
		Reason: "Unhealthy", Message: "Liveness probe failed: HTTP 500",
		Count: 3, EventTime: "2026-10-03T12:30:00Z",
		InvolvedName: "checkout-7d9f-abc", ObjectKind: "Pod",
	}.marshal())
	setDeployRaw(s, k8sDeployJSON{
		Name: "checkout", Generation: 4,
		Labels:      map[string]string{"app": "checkout"},
		Replicas:    3,
		Ready:       1,
		Updated:     1,
		Unavailable: 2,
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
	if len(got) != 2 {
		t.Fatalf("证据条数 = %d，期望 2（1 事件 + 1 rollout）\n%s",
			len(got), dumpEvidence(got))
	}

	ev, roll := got[0], got[1]
	if ev.Metadata["kind"] != "warning_event" {
		t.Errorf("第 1 条 kind = %q，期望 warning_event", ev.Metadata["kind"])
	}
	if ev.Type != evidence.TypeDeployment {
		t.Errorf("事件证据 type = %q，期望 %q", ev.Type, evidence.TypeDeployment)
	}
	if ev.Source != SourceK8s {
		t.Errorf("source = %q，期望 %q", ev.Source, SourceK8s)
	}
	// §4.4：时间必须来自事件本身，不是采集时刻。
	if want := time.Date(2026, 10, 3, 12, 30, 0, 0, time.UTC); !ev.Timestamp.Equal(want) {
		t.Errorf("事件时间 = %v，期望 %v（必须来自 eventTime）", ev.Timestamp, want)
	}
	if ev.Metadata["occurrences"] != "3" {
		t.Errorf("occurrences = %q，期望 3（K8s 聚合次数必须透出）",
			ev.Metadata["occurrences"])
	}
	if ev.Query == "" {
		t.Error("事件证据 query 为空 —— 违反验收标准第 2 条（可复制复现）")
	}

	if roll.Metadata["kind"] != "rollout" {
		t.Errorf("第 2 条 kind = %q，期望 rollout", roll.Metadata["kind"])
	}
	if roll.Metadata["rollout_ok"] != "false" {
		t.Errorf("rollout_ok = %q，期望 false（3 个副本只就绪 1 个）",
			roll.Metadata["rollout_ok"])
	}
	if roll.Metadata["unavailable"] != "2" {
		t.Errorf("unavailable = %q，期望 2", roll.Metadata["unavailable"])
	}
	if roll.Metadata["condition_progressing"] != "ProgressDeadlineExceeded(False)" {
		t.Errorf("condition_progressing = %q，期望 ProgressDeadlineExceeded(False)"+
			"（Progressing=False 才是「发布卡住」的直接证据，过滤掉它根因线索就没了）",
			roll.Metadata["condition_progressing"])
	}
	// rollout 的时间取条件更新时刻。
	if want := time.Date(2026, 10, 3, 12, 40, 0, 0, time.UTC); !roll.Timestamp.Equal(want) {
		t.Errorf("rollout 时间 = %v，期望 %v（取 conditions 的 lastUpdateTime）",
			roll.Timestamp, want)
	}
	if roll.Query == "" {
		t.Error("rollout 证据 query 为空")
	}
}

// 只请求 Warning：Normal 事件数量是 Warning 的百倍，全拉回来会吃光预算。
func TestK8sOnlyRequestsWarningEvents(t *testing.T) {
	s := newK8sServer(t)
	c := NewK8sCollector(s.srv.URL, "prod")
	if _, err := c.Collect(context.Background(), k8sInput("checkout")); err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	sel, _ := s.lastFieldSelector.Load().(string)
	if !strings.Contains(sel, "type=Warning") {
		t.Errorf("fieldSelector = %q，期望含 type=Warning —— 不加这个过滤，"+
			"Normal 事件会把 50 条预算吃光", sel)
	}
}

// 时间窗口过滤：K8s 的 field selector 不支持时间范围，只能客户端过滤。
func TestK8sFiltersEventsOutsideWindow(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s,
		map[string]any{"reason": "TooOld", "message": "m",
			"eventTime":      "2026-10-03T11:00:00Z",
			"involvedObject": map[string]any{"name": "checkout-1"}},
		map[string]any{"reason": "InWindow", "message": "m",
			"eventTime":      "2026-10-03T12:30:00Z",
			"involvedObject": map[string]any{"name": "checkout-1"}},
		map[string]any{"reason": "TooNew", "message": "m",
			"eventTime":      "2026-10-03T13:30:00Z",
			"involvedObject": map[string]any{"name": "checkout-1"}},
	)
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	evs := onlyWarnings(got)
	if len(evs) != 1 {
		t.Fatalf("窗口内事件条数 = %d，期望 1（窗口外的必须被过滤掉）\n%s",
			len(evs), dumpEvidence(evs))
	}
	if !strings.Contains(evs[0].Value, "InWindow") {
		t.Errorf("留下的不是窗口内那条: %q", evs[0].Value)
	}
	// 窗口是半开区间 [Start, End)，End 本身不该被收。
	// K8s 事件时间精度到秒，正好落在 End 上的事件存在（如整点批处理）。
	if strings.Contains(evs[0].Value, "TooNew") {
		t.Error("落在窗口 End 上的事件被收进来了 —— 窗口应为半开区间 [Start, End)")
	}
}

// 三个时间字段任一可用即采：真实集群三种都见过，只认一种会漏大部分。
func TestK8sAcceptsAnyTimestampField(t *testing.T) {
	cases := []struct {
		name string
		item map[string]any
	}{
		{"eventTime", map[string]any{"reason": "R", "eventTime": "2026-10-03T12:30:00Z"}},
		{"lastTimestamp", map[string]any{"reason": "R", "lastTimestamp": "2026-10-03T12:30:00Z"}},
		{"firstTimestamp", map[string]any{"reason": "R", "firstTimestamp": "2026-10-03T12:30:00Z"}},
		{"带偏移的 RFC3339", map[string]any{"reason": "R", "eventTime": "2026-10-03T20:30:00+08:00"}},
		{"无小数秒", map[string]any{"reason": "R", "eventTime": "2026-10-03T12:30:00Z"}},
		{"带纳秒", map[string]any{"reason": "R", "eventTime": "2026-10-03T12:30:00.123456789Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newK8sServer(t)
			it := map[string]any{}
			for k, v := range tc.item {
				it[k] = v
			}
			it["involvedObject"] = map[string]any{"name": "checkout-1"}
			setEventsRaw(s, it)
			c := NewK8sCollector(s.srv.URL, "prod")
			got, err := c.Collect(context.Background(), k8sInput("checkout"))
			if err != nil {
				t.Fatalf("采集失败: %v", err)
			}
			if evs := onlyWarnings(got); len(evs) != 1 {
				t.Fatalf("事件条数 = %d，期望 1 —— 该时间字段未被识别\n%s",
					len(evs), dumpEvidence(evs))
			}
		})
	}
}

// 三个时间字段全缺：按 §4.4 跳过，**不用采集时刻兜底**。
func TestK8sSkipsEventsWithoutAnyTimestamp(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, map[string]any{
		"reason":         "NoTime",
		"message":        "m",
		"involvedObject": map[string]any{"name": "checkout-1"},
	})
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if evs := onlyWarnings(got); len(evs) != 0 {
		t.Errorf("无时间的事件条数 = %d，期望 0 —— §4.4 禁止用采集时刻兜底\n%s",
			len(evs), dumpEvidence(evs))
	}
}

// 服务归属必须靠 Deployment 列表消歧，不能靠字符串前缀。
//
// 背景：Pod 名形如 `<deployment>-<hash>-<suffix>`，而 Deployment 名由用户随意取。
// 于是 `checkout-staging-1` 以 `checkout-` 开头 —— 用前缀匹配会把它
// 算成 checkout 的事件，等于把一个服务的事故证据挂到另一个服务上，
// 而页面上完全看不出来（source 与 service 都对，看着毫无异常）。
//
// 正确做法：先解析出本服务真正拥有的 Deployment 集合，再按最长匹配判归属。
func TestK8sServiceMatchUsesDeploymentOwnership(t *testing.T) {
	s := newK8sServer(t)
	// 命名空间里同时有 checkout 与 checkout-staging 两个 Deployment。
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
	setEventsRaw(s,
		k8sEventJSON{Reason: "StagingOnly", EventTime: "2026-10-03T12:30:00Z",
			InvolvedName: "checkout-staging-5f8c-9x2kt"}.marshal(),
		k8sEventJSON{Reason: "ProdOnly", EventTime: "2026-10-03T12:31:00Z",
			InvolvedName: "checkout-7d9f-abc"}.marshal(),
	)

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	// 只该拿到 ProdOnly 那条事件（外加 checkout 自己的 rollout）。
	var events []evidence.Evidence
	for _, e := range got {
		if e.Metadata["kind"] == "warning_event" {
			events = append(events, e)
		}
	}
	if len(events) != 1 {
		t.Fatalf("Warning 事件条数 = %d，期望 1（checkout-staging 的不该被算进来）\n%s",
			len(events), dumpEvidence(events))
	}
	if !strings.Contains(events[0].Value, "ProdOnly") {
		t.Errorf("留下的不是 checkout 自己的那条: %q", events[0].Value)
	}
}
