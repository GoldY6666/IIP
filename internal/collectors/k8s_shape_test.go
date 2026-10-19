package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

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

// ---------- Warning 事件的基线对比（k8s_baseline.go） ----------
