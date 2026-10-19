package collectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
