package collectors

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

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
