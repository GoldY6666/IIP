package collectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// k8sServer 是可分别控制 events / deployments 两个端点的 mock apiserver。
//
// 刻意**分成两个独立开关**：真实集群里 RBAC 常只给其中之一
// （events 读权限比 deployments 宽，运维往往先只开一个），
// 「只挂一个」是必须能验的降级路径，不是边角情况。
type k8sServer struct {
	srv        *httptest.Server
	eventsBody atomic.Value // string
	deployBody atomic.Value // string

	eventsStatus atomic.Int32
	deployStatus atomic.Int32

	// 记录收到的 fieldSelector，用来断言「只查 Warning」这条过滤真的发出去了。
	lastFieldSelector atomic.Value
	hitEvents         atomic.Int64
	hitDeploy         atomic.Int64
}

func newK8sServer(t *testing.T) *k8sServer {
	t.Helper()
	s := &k8sServer{}
	s.eventsBody.Store(`{"items":[]}`)
	s.deployBody.Store(`{"items":[]}`)
	s.eventsStatus.Store(int32(http.StatusOK))
	s.deployStatus.Store(int32(http.StatusOK))
	s.lastFieldSelector.Store("")

	// **默认就放一个 Deployment**：事件归属靠 Deployment 列表消歧
	// （见 belongsTo），列表为空时会退化成精确匹配，Pod 名一律不收 ——
	// 那样大部分用例会因为「没造 Deployment」而假失败，掩盖真正要验的东西。
	// 用例要验「列表为空时的降级」时显式覆盖成空列表。
	s.setDeploys("checkout", "prod")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		s.hitEvents.Add(1)
		s.lastFieldSelector.Store(r.URL.Query().Get("fieldSelector"))
		st := int(s.eventsStatus.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		if st == http.StatusOK {
			_, _ = w.Write([]byte(s.eventsBody.Load().(string)))
		} else {
			_, _ = w.Write([]byte(`{"kind":"Status","message":"forbidden"}`))
		}
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/", func(w http.ResponseWriter, _ *http.Request) {
		s.hitDeploy.Add(1)
		st := int(s.deployStatus.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		if st == http.StatusOK {
			_, _ = w.Write([]byte(s.deployBody.Load().(string)))
		} else {
			_, _ = w.Write([]byte(`{"kind":"Status","message":"forbidden"}`))
		}
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// k8sDeployJSON / k8sEventJSON 是**测试侧**的构造结构。
//
// 与生产结构体刻意分开：共用一个结构体的话，改了 json tag
// 生产解析与测试构造会一起变，测试就永远发现不了 tag 写错。
// 这里直接手写 map（下面两个 marshal 方法）——字段名写错会在
// 编译期或断言处暴露，而共用结构体时是静默自洽。
type k8sEventJSON struct {
	Reason        string
	Message       string
	Count         int
	EventTime     string
	LastTimestamp string
	FirstTime     string
	InvolvedName  string
	ObjectKind    string
}

func (e k8sEventJSON) marshal() map[string]any {
	m := map[string]any{
		"reason":  e.Reason,
		"message": e.Message,
		"count":   e.Count,
		"involvedObject": map[string]any{
			"name": e.InvolvedName,
			"kind": orDefault(e.ObjectKind, "Pod"),
		},
	}
	if e.EventTime != "" {
		m["eventTime"] = e.EventTime
	}
	if e.LastTimestamp != "" {
		m["lastTimestamp"] = e.LastTimestamp
	}
	if e.FirstTime != "" {
		m["firstTimestamp"] = e.FirstTime
	}
	return m
}

type k8sCondJSON struct {
	Type       string
	Status     string
	Reason     string
	LastUpdate string
}

type k8sDeployJSON struct {
	Name        string
	Generation  int64
	Labels      map[string]string
	Replicas    int
	Ready       int
	Updated     int
	Unavailable int
	Conditions  []k8sCondJSON
}

func (d k8sDeployJSON) marshal() map[string]any {
	conds := make([]map[string]any, 0, len(d.Conditions))
	for _, c := range d.Conditions {
		conds = append(conds, map[string]any{
			"type":               c.Type,
			"status":             c.Status,
			"reason":             c.Reason,
			"lastUpdateTime":     c.LastUpdate,
			"lastTransitionTime": c.LastUpdate,
		})
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":       d.Name,
			"generation": d.Generation,
			"labels":     orEmptyMap(d.Labels),
		},
		"spec": map[string]any{"replicas": d.Replicas},
		"status": map[string]any{
			"replicas":            d.Replicas,
			"readyReplicas":       d.Ready,
			"updatedReplicas":     d.Updated,
			"unavailableReplicas": d.Unavailable,
			"conditions":          conds,
		},
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func setEventsRaw(s *k8sServer, items ...map[string]any) {
	s.eventsBody.Store(mustJSON(map[string]any{"items": items}))
}

func setDeployRaw(s *k8sServer, items ...map[string]any) {
	s.deployBody.Store(mustJSON(map[string]any{"items": items}))
}

// setDeploys 造一组「健康但存在」的 Deployment（rollout 时间落在窗口内）。
// 让用例可以只关心事件侧的逻辑，不必每次重复写 conditions。
func (s *k8sServer) setDeploys(namesAndLabels ...string) {
	items := make([]map[string]any, 0, len(namesAndLabels)/2)
	for i := 0; i+1 < len(namesAndLabels); i += 2 {
		name, label := namesAndLabels[i], namesAndLabels[i+1]
		items = append(items, k8sDeployJSON{
			Name: name, Replicas: 1, Ready: 1,
			Labels: map[string]string{"app": label},
			Conditions: []k8sCondJSON{{
				Type: "Available", Status: "True",
				LastUpdate: "2026-10-03T12:05:00Z",
			}},
		}.marshal())
	}
	s.deployBody.Store(mustJSON(map[string]any{"items": items}))
}

// clearDeployments 把 Deployment 列表置空 —— 用于验「拿不到列表时的降级」。
func (s *k8sServer) clearDeployments() {
	s.deployBody.Store(`{"items":[]}`)
}

// onlyWarnings 只保留 Warning 事件类证据。
//
// **断言条数前必须过这一层**：mock 默认带一个 Deployment，
// 于是每次采集除了事件还会多出一条 rollout。直接数 len(got)
// 会让「验事件过滤」的用例被无关的 rollout 干扰 ——
// 那种失败看不出是产品错还是测试没对齐。
func onlyWarnings(evs []evidence.Evidence) []evidence.Evidence {
	out := make([]evidence.Evidence, 0, len(evs))
	for _, e := range evs {
		if e.Metadata["kind"] == "warning_event" {
			out = append(out, e)
		}
	}
	return out
}

// k8sWindow 返回一个固定窗口，起点对齐到整点便于构造 RFC3339。
func k8sWindow() evidence.Window {
	start := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	return evidence.Window{Start: start, End: start.Add(time.Hour)}
}

func k8sInput(svc string) evidence.Input {
	return evidence.Input{IncidentID: 1, Service: svc, Window: k8sWindow()}
}

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

// 拿不到 Deployment 列表时（403）仍要出事件证据，但归属退化为精确匹配 ——
// 宁可少收，也不能把别的服务的证据挂上来。
func TestK8sFallsBackToExactMatchWhenDeploymentsUnavailable(t *testing.T) {
	s := newK8sServer(t)
	s.deployStatus.Store(int32(http.StatusForbidden))
	setEventsRaw(s,
		k8sEventJSON{Reason: "Exact", EventTime: "2026-10-03T12:30:00Z",
			InvolvedName: "checkout"}.marshal(),
		k8sEventJSON{Reason: "Prefixed", EventTime: "2026-10-03T12:31:00Z",
			InvolvedName: "checkout-7d9f-abc"}.marshal(),
	)
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1（只有精确匹配那条）\n%s", len(got), dumpEvidence(got))
	}
	if !strings.Contains(got[0].Value, "Exact") {
		t.Errorf("留下的不是精确匹配那条: %q", got[0].Value)
	}
	// 降级必须被记录：运维要知道事件归属是「近似判定」而非「已确认」。
	if got[0].Metadata["failures"] == "" {
		t.Error("failures 为空 —— 降级到近似判定却没有记录，运维无从得知")
	}
}

// 单边失败：events 403 但 deployments 通 —— 仍要出证据，并说清缺了什么。
func TestK8sEventsForbiddenStillReturnsRollouts(t *testing.T) {
	s := newK8sServer(t)
	s.eventsStatus.Store(int32(http.StatusForbidden))
	s.deployBody.Store(mustJSON(map[string]any{"items": []map[string]any{{
		"metadata": map[string]any{"name": "checkout", "generation": int64(2),
			"labels": map[string]string{"app": "checkout"}},
		"spec": map[string]any{"replicas": 1},
		"status": map[string]any{"replicas": 1, "readyReplicas": 1,
			"conditions": []map[string]any{{"type": "Available", "status": "True",
				"lastUpdateTime": "2026-10-03T12:10:00Z"}}},
	}}}))

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("单边失败不应让整个采集失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1（rollout 仍应采到）\n%s", len(got), dumpEvidence(got))
	}
	if got[0].Metadata["failures"] == "" {
		t.Error("failures 为空 —— 页面无从知道 Warning 事件为什么没采到")
	}
	if !strings.Contains(got[0].Metadata["failures"], "403") {
		t.Errorf("failures 未带上 HTTP 状态码: %q", got[0].Metadata["failures"])
	}
}

// 两边都失败才算采集失败。
func TestK8sBothFailIsError(t *testing.T) {
	s := newK8sServer(t)
	s.eventsStatus.Store(int32(http.StatusForbidden))
	s.deployStatus.Store(int32(http.StatusInternalServerError))
	c := NewK8sCollector(s.srv.URL, "prod")
	_, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err == nil {
		t.Fatal("两边都失败时必须返回错误 —— 静默返回空会让任务被标记成功，" +
			"证据就永远缺着而没人知道")
	}
	if !strings.Contains(err.Error(), "均失败") {
		t.Errorf("错误信息未说明是两边都失败: %v", err)
	}
}

// 空结果 = 「采到了但没有」，不是失败。
func TestK8sEmptyResultIsNotError(t *testing.T) {
	s := newK8sServer(t)
	// 显式清空 Deployment：这条要验的是「两个端点都空 → 0 条证据且不报错」，
	// 留着默认那个 Deployment 会多出一条 rollout，断言就得改成「1 条」，
	// 那样就验不到「真的什么都没有」这个状态了。
	s.clearDeployments()
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("证据条数 = %d，期望 0\n%s", len(got), dumpEvidence(got))
	}
}

// 配置缺失必须在采集时就说清，而不是静默返回空。
func TestK8sMissingConfigIsError(t *testing.T) {
	if _, err := NewK8sCollector("", "prod").
		Collect(context.Background(), k8sInput("checkout")); err == nil {
		t.Error("缺 apiserver 地址时应报错")
	} else if !strings.Contains(err.Error(), "apiserver") {
		t.Errorf("错误信息未指出缺什么: %v", err)
	}
	if _, err := NewK8sCollector("https://k8s", "  ").
		Collect(context.Background(), k8sInput("checkout")); err == nil {
		t.Error("缺 namespace 时应报错")
	}
}

// TopN 截断：Warning 事件可能上百条，全落库会吃光 50 条预算。
func TestK8sTopNCapsWarningEvents(t *testing.T) {
	s := newK8sServer(t)
	items := make([]map[string]any, 0, 25)
	for i := 0; i < 25; i++ {
		items = append(items, map[string]any{
			"reason":         fmt.Sprintf("Reason%02d", i),
			"eventTime":      "2026-10-03T12:%02d:00Z", // 12:00..12:24
			"involvedObject": map[string]any{"name": "checkout-1"},
		})
		items[i]["eventTime"] = fmt.Sprintf("2026-10-03T12:%02d:00Z", i)
	}
	setEventsRaw(s, items...)

	c := NewK8sCollector(s.srv.URL, "prod", WithTopN(5))
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	evs := onlyWarnings(got)
	if len(evs) != 5 {
		t.Fatalf("事件条数 = %d，期望 5（TopN 生效）\n%s", len(evs), dumpEvidence(evs))
	}
	// matched 必须报真实命中数，否则用户以为集群里只有 5 条 Warning。
	if evs[0].Metadata["matched"] != "25" {
		t.Errorf("matched = %q，期望 25（真实命中数，不是截断后的数）",
			evs[0].Metadata["matched"])
	}
	// 按时间倒序：最新的排最前。
	if !strings.Contains(evs[0].Value, "Reason24") {
		t.Errorf("首条不是最新的: %q", evs[0].Value)
	}
}

// Deployment 归属：label 优先于命名推断。
func TestK8sDeploymentMatchPrefersLabels(t *testing.T) {
	s := newK8sServer(t)
	items := []map[string]any{
		{
			// 名字不含 checkout，但 label 指明是 —— 必须收。
			"metadata": map[string]any{"name": "web-frontend",
				"labels": map[string]string{"app.kubernetes.io/name": "checkout"}},
			"spec": map[string]any{"replicas": 1},
			"status": map[string]any{"replicas": 1, "readyReplicas": 1,
				"conditions": []map[string]any{{"type": "Available", "status": "True",
					"lastUpdateTime": "2026-10-03T12:10:00Z"}}},
		},
		{
			// 同命名空间的无关服务 —— 必须排除。
			"metadata": map[string]any{"name": "payment", "labels": map[string]string{}},
			"spec":     map[string]any{"replicas": 1},
			"status": map[string]any{"replicas": 1, "readyReplicas": 1,
				"conditions": []map[string]any{{"type": "Available", "status": "True",
					"lastUpdateTime": "2026-10-03T12:10:00Z"}}},
		},
	}
	s.deployBody.Store(mustJSON(map[string]any{"items": items}))

	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1（payment 属于别的服务）\n%s",
			len(got), dumpEvidence(got))
	}
	if got[0].Metadata["deployment"] != "web-frontend" {
		t.Errorf("deployment = %q，期望 web-frontend", got[0].Metadata["deployment"])
	}
}

// spec.replicas 缺省时不能拿 status.replicas 当期望值。
//
// Deployment 刚建好还没起 Pod 时 status.replicas = 0，
// 拿 0 当期望值会算出「0/0 就绪 = 一切正常」，而真实情况是「还没起来」。
func TestK8sRolloutWithZeroSpecReplicas(t *testing.T) {
	s := newK8sServer(t)
	s.deployBody.Store(mustJSON(map[string]any{"items": []map[string]any{{
		"metadata": map[string]any{"name": "checkout", "labels": map[string]string{"app": "checkout"}},
		"spec":     map[string]any{}, // 没写 replicas
		"status": map[string]any{"replicas": 0, "readyReplicas": 0,
			"conditions": []map[string]any{{"type": "Progressing", "status": "True",
				"lastUpdateTime": "2026-10-03T12:10:00Z"}}},
	}}}))
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1", len(got))
	}
	// rollout_ok 要求 replicas > 0，否则 0==0 会被算成「正常」。
	if got[0].Metadata["rollout_ok"] != "false" {
		t.Errorf("rollout_ok = %q，期望 false —— 0/0 不等于「发布正常」",
			got[0].Metadata["rollout_ok"])
	}
}

// 可复现性：query 字段必须带上 namespace，否则用户拿到 query 也不知道查哪。
func TestK8sQueryIsReproducible(t *testing.T) {
	c := NewK8sCollector("https://k8s.internal:6443/", "prod-01")
	if got := c.eventsQuery(); !strings.Contains(got, "prod-01") ||
		!strings.Contains(got, "type%3DWarning") {
		t.Errorf("eventsQuery = %q，缺 namespace 或未编码的 fieldSelector", got)
	}
	if got := c.rolloutsQuery(); !strings.Contains(got, "prod-01") ||
		!strings.Contains(got, "apps/v1") {
		t.Errorf("rolloutsQuery = %q", got)
	}
}

// 超时：挂死的 apiserver 不能把 worker 永久占住（§4.5）。
func TestK8sRespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewK8sCollector(srv.URL, "prod", WithTimeout(150*time.Millisecond))
	start := time.Now()
	_, err := c.Collect(context.Background(), k8sInput("checkout"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("超时后必须返回错误")
	}
	if elapsed > time.Second {
		t.Errorf("耗时 %v，超时设置 150ms 却没生效", elapsed)
	}
}

// 响应体不是合法 JSON：报「解析失败」而不是空结果。
func TestK8sMalformedJSONIsError(t *testing.T) {
	s := newK8sServer(t)
	s.eventsBody.Store(`{"items": [ this is not json`)
	s.deployStatus.Store(int32(http.StatusInternalServerError))
	c := NewK8sCollector(s.srv.URL, "prod")
	_, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err == nil {
		t.Fatal("解析失败必须报错 —— 返回空会让「格式变了」被当成「没有事件」")
	}
	if !strings.Contains(err.Error(), "解析") {
		t.Errorf("错误信息未指出是解析问题: %v", err)
	}
}

// 有 items 但一条都解析不出：说明格式变了，必须报错而不是静默返回 0 条。
func TestK8sItemsPresentButNoneUsable(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, map[string]any{
		"reason": "NoTimeAtAll", "message": "m",
		"involvedObject": map[string]any{"name": "checkout-1"},
	})
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	// 这里允许返回 0 条且不报错 —— 因为 events 本身是 200 且格式合法，
	// 只是这些事件没有时间（按 §4.4 跳过）。但要验明**确实**是 0 而非 1 条兜底证据。
	if err != nil {
		t.Fatalf("采集不应报错: %v", err)
	}
	if evs := onlyWarnings(got); len(evs) != 0 {
		t.Errorf("事件条数 = %d，期望 0\n%s", len(evs), dumpEvidence(evs))
	}
}

// 服务名为空时不过滤：告警没带 service 标签是很常见的，
// 静默返回「无证据」会让人以为是集群里没有 Warning 事件。
func TestK8sEmptyServiceDoesNotFilter(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, map[string]any{
		"reason": "AnyService", "eventTime": "2026-10-03T12:30:00Z",
		"involvedObject": map[string]any{"name": "whatever-1"},
	})
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput(""))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if evs := onlyWarnings(got); len(evs) != 1 {
		t.Errorf("事件条数 = %d，期望 1（service 为空时不该过滤掉全部）\n%s",
			len(evs), dumpEvidence(evs))
	}
}

// 两个端点各被请求一次：漏掉任一个都说明 URL 拼错了。
func TestK8sHitsBothEndpoints(t *testing.T) {
	s := newK8sServer(t)
	c := NewK8sCollector(s.srv.URL, "prod")
	if _, err := c.Collect(context.Background(), k8sInput("checkout")); err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if s.hitEvents.Load() != 1 {
		t.Errorf("events 端点被请求 %d 次，期望 1", s.hitEvents.Load())
	}
	if s.hitDeploy.Load() != 1 {
		t.Errorf("deployments 端点被请求 %d 次，期望 1", s.hitDeploy.Load())
	}
}

// occurrences=1 时不写该字段：满屏的 "1" 是噪声。
func TestK8sOmitsOccurrencesWhenSingle(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s, map[string]any{
		"reason": "Once", "count": 1, "eventTime": "2026-10-03T12:30:00Z",
		"involvedObject": map[string]any{"name": "checkout-1"},
	})
	c := NewK8sCollector(s.srv.URL, "prod")
	got, err := c.Collect(context.Background(), k8sInput("checkout"))
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if _, ok := got[0].Metadata["occurrences"]; ok {
		t.Errorf("occurrences 不该出现（count=1）: %v", got[0].Metadata)
	}
}

// registry 能装下 K8s 采集器：DoD 第 2 条要求「新增采集器不改既有代码」。
func TestK8sRegistersInRegistry(t *testing.T) {
	s := newK8sServer(t)
	reg := evidence.NewRegistry()
	reg.MustRegister(NewK8sCollector(s.srv.URL, "prod"))
	if reg.Len() != 1 {
		t.Fatalf("注册表大小 = %d，期望 1", reg.Len())
	}
	c, ok := reg.Get(SourceK8s)
	if !ok {
		t.Fatalf("按 %q 取不到采集器", SourceK8s)
	}
	if _, err := c.Collect(context.Background(), k8sInput("checkout")); err != nil {
		t.Fatalf("从注册表取出后采集失败: %v", err)
	}
}

// 采集器必须满足并发安全契约（evidence.Collector 的硬要求）：
// 同一实例被多个 goroutine 同时调用，产出的证据条数必须一致。
func TestK8sConcurrentCollectIsSafe(t *testing.T) {
	s := newK8sServer(t)
	setEventsRaw(s,
		map[string]any{"reason": "A", "eventTime": "2026-10-03T12:10:00Z",
			"involvedObject": map[string]any{"name": "checkout-1"}},
		map[string]any{"reason": "B", "eventTime": "2026-10-03T12:20:00Z",
			"involvedObject": map[string]any{"name": "checkout-2"}},
	)
	c := NewK8sCollector(s.srv.URL, "prod")
	const workers = 8
	results := make(chan int, workers)
	for i := 0; i < workers; i++ {
		go func() {
			got, err := c.Collect(context.Background(), k8sInput("checkout"))
			if err != nil {
				results <- -1
				return
			}
			results <- len(onlyWarnings(got))
		}()
	}
	for i := 0; i < workers; i++ {
		if n := <-results; n != 2 {
			t.Errorf("并发第 %d 次采集得到 %d 条，期望 2（采集器非并发安全）", i, n)
		}
	}
}

// mock 构造辅助自身的自检：确保 JSON 结构体与生产解析的 tag 约定没漂。
// 若这里失败，说明 mock 造的数据本身有问题，后面所有断言都不可信。
func TestK8sMockShapesAreValid(t *testing.T) {
	e := k8sEventJSON{Reason: "R", Message: "M", Count: 2,
		EventTime: "2026-10-03T12:00:00Z", InvolvedName: "checkout-1"}
	m := e.marshal()
	if m["reason"] != "R" || m["message"] != "M" {
		t.Errorf("事件 mock 字段不对: %v", m)
	}
	if _, ok := m["involvedObject"].(map[string]any); !ok {
		t.Errorf("involvedObject 结构不对: %v", m["involvedObject"])
	}
	d := k8sDeployJSON{Name: "checkout", Replicas: 2, Ready: 1,
		Conditions: []k8sCondJSON{{Type: "Progressing", Status: "False"}}}
	dm := d.marshal()
	st := dm["status"].(map[string]any)
	if st["replicas"] != 2 || st["readyReplicas"] != 1 {
		t.Errorf("Deployment mock 的 status 不对: %v", st)
	}
	if len(st["conditions"].([]map[string]any)) != 1 {
		t.Errorf("conditions 构造失败: %v", st["conditions"])
	}
}
