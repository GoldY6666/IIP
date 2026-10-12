package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/yu/iip/internal/incident"
)

// 本文件只做一件事：断言响应体里**键名的字面形态**。
//
// 为什么不复用结构体做 round-trip：
//
//	结构体 -> JSON -> 同一个结构体
//
// 这种断言在 tag 缺失时依然自洽（Go 缺 tag 就用字段名，读回来也认），
// 所以它**测不出** tag 写没写、拼对没拼对。2026-10-03 的端到端冒烟正是
// 在真实进程里打出 `{"ID":1,"Title":...}` 才发现这批 tag 全缺，
// 而 14 个既有 handler 用例全绿。
//
// 这里刻意解成 map[string]any 后逐个比对键名：断言的是**字节流**，
// 与结构体定义解耦，tag 写错就会红。
//
// 命名即契约：前端只认 snake_case。

// assertKeys 断言 obj 恰好含有 want 这几个键（不多不少）。
//
// 用「恰好」而非「至少」：多出来的 Go 字段名（ID / AlertCount 之类）
// 同样会暴露给前端，正是这次要堵的洞。
func assertKeys(t *testing.T, path string, obj any, want []string) {
	t.Helper()
	m, ok := obj.(map[string]any)
	if !ok {
		t.Errorf("%s 不是 JSON 对象，实际类型 %T", path, obj)
		return
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("%s 的键 = %v，期望恰好 %v", path, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s 的键 = %v，期望恰好 %v", path, got, want)
			return
		}
	}
}

// decodeObject 解析响应体为 map。
func decodeObject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析响应体失败: %v（body=%s）", err, rec.Body.String())
	}
	return m
}

// wantArray 断言字段是含 n 个元素的数组并返回它。
func wantArray(t *testing.T, path string, v any, n int) []any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s 不是 JSON 数组，实际类型 %T（value=%#v）", path, v, v)
	}
	if len(arr) != n {
		t.Fatalf("%s 元素数 = %d，期望 %d", path, len(arr), n)
	}
	return arr
}

// summaryJSON 是一个带满字段的列表行 / 详情 incident 的期望键集。
var summaryKeys = []string{
	"id", "title", "severity", "status", "service",
	"started_at", "resolved_at", "updated_at",
	"alert_count", "needs_review_count",
}

// alertLinkKeys 是一条关联告警的期望键集。
var alertLinkKeys = []string{
	"alert_id", "fingerprint", "severity", "labels",
	"starts_at", "ends_at", "correlation_score", "needs_review", "correlated_at",
}

// entryKeys 是一条时间线留痕的期望键集。
var entryKeys = []string{
	"incident_id", "event_type", "description", "actor", "metadata", "timestamp",
}

// TestResponseBodyUsesSnakeCase_List 列表端点的键名契约。
func TestResponseBodyUsesSnakeCase_List(t *testing.T) {
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	lister := &fakeIncidentLister{res: incident.ListResult{
		Items: []incident.Summary{{
			ID: 1, Title: "t", Severity: "critical", Status: incident.StatusOpen,
			Service: "api", StartedAt: fixed, UpdatedAt: fixed,
			AlertCount: 2, NeedsReviewCount: 1,
		}},
	}}
	r := muxWithIncidentRoutes("/api/v1/incidents", IncidentList(lister, discardLogger()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	body := decodeObject(t, rec)
	assertKeys(t, "列表响应体", body, []string{"incidents", "count", "has_more"})
	rows := wantArray(t, "列表 incidents", body["incidents"], 1)
	assertKeys(t, "列表 incidents[0]", rows[0], summaryKeys)
}

// TestResponseBodyUsesSnakeCase_Detail 详情端点的键名契约。
func TestResponseBodyUsesSnakeCase_Detail(t *testing.T) {
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	score := 0.42
	detailer := &fakeIncidentDetailer{detail: incident.Detail{
		Summary: incident.Summary{
			ID: 7, Title: "t", Severity: "critical", Status: incident.StatusOpen,
			Service: "api", StartedAt: fixed, UpdatedAt: fixed,
		},
		Alerts: []incident.AlertLink{{
			AlertID: 9, Fingerprint: "fp", Severity: "critical",
			Labels: map[string]string{"svc": "api"}, StartsAt: fixed,
			CorrelationScore: &score, NeedsReview: true, CorrelatedAt: fixed,
		}},
		Timeline: []incident.Entry{{
			IncidentID: 7, EventType: incident.EventAlertFired,
			Description: "d", Actor: "yu",
			Metadata: map[string]string{"k": "v"}, Timestamp: fixed,
		}},
		TruncatedAlerts: true, TruncatedTimeline: true,
	}}
	h := IncidentDetail(detailer, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/7", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	body := decodeObject(t, rec)
	assertKeys(t, "详情响应体", body, []string{"incident", "alerts", "timeline", "truncated"})
	assertKeys(t, "详情 truncated", body["truncated"], []string{"alerts", "timeline"})
	assertKeys(t, "详情 incident", body["incident"], summaryKeys)
	assertKeys(t, "详情 alerts[0]",
		wantArray(t, "详情 alerts", body["alerts"], 1)[0], alertLinkKeys)
	assertKeys(t, "详情 timeline[0]",
		wantArray(t, "详情 timeline", body["timeline"], 1)[0], entryKeys)
}

// TestResponseBodyUsesSnakeCase_ReviewList 复核队列端点的键名契约。
//
// 队列元素是 ReviewEdge 直接序列化（不经过 API 层自己的响应体结构），
// 是最容易漏 tag 的一处。
func TestResponseBodyUsesSnakeCase_ReviewList(t *testing.T) {
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	lister := &fakeReviewLister{res: incident.ReviewListResult{
		Items: []incident.ReviewEdge{{
			IncidentID: 1,
			Alert: incident.AlertLink{
				AlertID: 9, Fingerprint: "fp", Severity: "warning",
				Labels: map[string]string{"svc": "api"}, StartsAt: fixed,
				NeedsReview: true, CorrelatedAt: fixed,
			},
		}},
	}}
	r := muxWithReviewRoutes(ReviewList(lister, discardLogger()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/reviews", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	body := decodeObject(t, rec)
	assertKeys(t, "复核队列响应体", body, []string{"reviews", "count", "has_more"})
	rows := wantArray(t, "复核队列 reviews", body["reviews"], 1)
	assertKeys(t, "复核队列 reviews[0]", rows[0], []string{"incident_id", "alert"})

	edge, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("reviews[0] 不是对象: %#v", rows[0])
	}
	assertKeys(t, "复核队列 reviews[0].alert", edge["alert"], alertLinkKeys)
}

// TestResponseBodyUsesSnakeCase_ReviewDecide 复核判定端点的键名契约。
func TestResponseBodyUsesSnakeCase_ReviewDecide(t *testing.T) {
	decider := &fakeReviewDecider{res: incident.ReviewResult{
		Decision: incident.ReviewReject, IncidentID: 1, AlertID: 9,
		Suppressed: true, Unlinked: true,
	}}
	r := muxWithReviewRoutes(ReviewDecide(decider, discardLogger()))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/reviews/1/9",
		newBody(t, `{"decision":"reject","actor":"yu","reason":"误报"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	body := decodeObject(t, rec)
	assertKeys(t, "复核判定响应体", body, []string{"result"})
	assertKeys(t, "复核判定 result", body["result"], []string{
		"decision", "incident_id", "alert_id", "suppressed", "unlinked",
	})
}

// TestResponseBodyUsesSnakeCase_TransitionAndComment 流转与备注两个端点的键名契约。
//
// 这两个端点的响应体包着 incident.Summary / incident.Entry，
// 与列表、详情共用同一套 tag，单独过一遍是为了让失败时能一眼定位是哪个端点坏了。
func TestResponseBodyUsesSnakeCase_TransitionAndComment(t *testing.T) {
	fixed := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	transitioner := &fakeIncidentTransitioner{snap: incident.Snapshot{
		ID: 7, Title: "t", Severity: "critical", Status: incident.StatusResolved,
		Service: "api", StartedAt: fixed, UpdatedAt: fixed,
	}}
	h := IncidentTransition(transitioner, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/transition",
		newBody(t, `{"to":"resolved","actor":"yu"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("流转状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	transBody := decodeObject(t, rec)
	assertKeys(t, "流转响应体", transBody, []string{"incident"})
	assertKeys(t, "流转 incident", transBody["incident"], []string{
		"id", "title", "severity", "status", "service",
		"started_at", "resolved_at", "updated_at",
		// 流转响应刻意不含这两个聚合列（无 JOIN，填 0 反而误导前端显示"0 条关联"）。
	})

	commenter := &fakeIncidentCommenter{entry: incident.Entry{
		IncidentID: 7, EventType: incident.EventUserComment,
		Description: "n", Actor: "yu", Timestamp: fixed,
	}}
	hc := IncidentComment(commenter, discardLogger())
	rc := muxWithIncidentRoutes("", hc)

	recC := httptest.NewRecorder()
	rc.ServeHTTP(recC, httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/comments",
		newBody(t, `{"actor":"yu","note":"n"}`)))
	if recC.Code != http.StatusCreated {
		t.Fatalf("备注状态码 = %d，期望 201（body=%s）", recC.Code, recC.Body.String())
	}
	commentBody := decodeObject(t, recC)
	assertKeys(t, "备注响应体", commentBody, []string{"entry"})
	assertKeys(t, "备注 entry", commentBody["entry"], entryKeys)
}
