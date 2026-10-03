package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/incident"
)

// 本文件覆盖 REVIEW 队列两个端点的 HTTP 契约。
// 与 incidents_test.go 的分工一致：这里只管状态码与错误码，
// 「确认 / 拒绝各动了哪几张表」由 internal/incident 的用例覆盖。

// --- 替身 ---

type fakeReviewLister struct {
	res    incident.ReviewListResult
	err    error
	limit  int
	offset int
}

func (f *fakeReviewLister) ListReviews(_ context.Context, limit, offset int) (incident.ReviewListResult, error) {
	f.limit, f.offset = limit, offset
	return f.res, f.err
}

type fakeReviewDecider struct {
	res      incident.ReviewResult
	err      error
	incident int64
	alert    int64
	decision incident.ReviewDecision
	actor    string
	reason   string
}

func (f *fakeReviewDecider) Review(_ context.Context, incidentID, alertID int64,
	decision incident.ReviewDecision, actor, reason string) (incident.ReviewResult, error) {
	f.incident, f.alert, f.decision, f.actor, f.reason = incidentID, alertID, decision, actor, reason
	return f.res, f.err
}

// muxWithReviewRoutes 把带两个路径参数的复核路由挂到真实 chi 树上。
func muxWithReviewRoutes(h http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/reviews", h)
	r.Post("/api/v1/reviews/{incidentID}/{alertID}", h)
	return r
}

// --- 队列列表 ---

func TestReviewListOK(t *testing.T) {
	lister := &fakeReviewLister{res: incident.ReviewListResult{
		Items: []incident.ReviewEdge{
			{IncidentID: 7, Alert: incident.AlertLink{AlertID: 101, Fingerprint: "fp-a", NeedsReview: true}},
			{IncidentID: 7, Alert: incident.AlertLink{AlertID: 102, Fingerprint: "fp-b", NeedsReview: true}},
		},
		HasMore: true,
	}}
	h := ReviewList(lister, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/reviews?limit=2&offset=10", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}

	var got reviewListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.Count != 2 || len(got.Reviews) != 2 {
		t.Errorf("count=%d items=%d，期望 2/2", got.Count, len(got.Reviews))
	}
	if !got.HasMore {
		t.Error("has_more 应原样透传")
	}
	if lister.limit != 2 || lister.offset != 10 {
		t.Errorf("分页未下传: limit=%d offset=%d", lister.limit, lister.offset)
	}
}

// TestReviewListBadParamsDegrade 断言无法解析的分页参数退化为 0（= 交给服务层取默认）
// 而不是 400。注意夹紧本身在 internal/incident 里（ListReviews 的 limit/offset 归一化），
// 这里只验 HTTP 层不会因为脏参数报错——两层各管一段，避免重复断言。
func TestReviewListBadParamsDegrade(t *testing.T) {
	lister := &fakeReviewLister{}
	h := ReviewList(lister, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/reviews?limit=abc&offset=-5", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("脏参数不应导致非 200，实得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if lister.limit != 0 {
		t.Errorf("无法解析的 limit 应退化为 0，实得 %d", lister.limit)
	}
}

func TestReviewListStorageError(t *testing.T) {
	h := ReviewList(&fakeReviewLister{err: errors.New("库挂了")}, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/reviews", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeStorageUnavail {
		t.Errorf("错误码 = %q，期望 %q", got, CodeStorageUnavail)
	}
}

// --- 复核判定 ---

func TestReviewDecideConfirmOK(t *testing.T) {
	decider := &fakeReviewDecider{res: incident.ReviewResult{
		Decision: incident.ReviewConfirm, IncidentID: 7, AlertID: 101,
	}}
	r := muxWithReviewRoutes(ReviewDecide(decider, discardLogger()))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reviews/7/101",
		newBody(t, `{"decision":"confirm","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if decider.incident != 7 || decider.alert != 101 {
		t.Errorf("路径参数解析错误: incident=%d alert=%d", decider.incident, decider.alert)
	}
	if decider.decision != incident.ReviewConfirm || decider.actor != "yu" {
		t.Errorf("入参未正确下传: decision=%s actor=%s", decider.decision, decider.actor)
	}
	var got reviewDecideResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.Result.Decision != incident.ReviewConfirm {
		t.Errorf("响应 decision = %s，期望 confirm", got.Result.Decision)
	}
	if got.Result.Suppressed || got.Result.Unlinked {
		t.Errorf("confirm 不该动抑制与关联，实得 %+v", got.Result)
	}
}

// TestReviewDecideRejectReturnsEffects 断言 reject 的副作用回传给调用方，
// 否则前端只能靠刷新列表去猜这一步到底有没有生效。
func TestReviewDecideRejectReturnsEffects(t *testing.T) {
	decider := &fakeReviewDecider{res: incident.ReviewResult{
		Decision: incident.ReviewReject, IncidentID: 7, AlertID: 101,
		Suppressed: true, Unlinked: true,
	}}
	r := muxWithReviewRoutes(ReviewDecide(decider, discardLogger()))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reviews/7/101",
		newBody(t, `{"decision":"reject","actor":"yu","reason":"指纹不匹配"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if decider.reason != "指纹不匹配" {
		t.Errorf("reason 未下传，实得 %q", decider.reason)
	}
	var got reviewDecideResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !got.Result.Suppressed || !got.Result.Unlinked {
		t.Errorf("reject 的副作用应回传，实得 %+v", got.Result)
	}
}

// TestReviewDecideNotFoundIs404 断言「边不存在 / 已处理」是 404 且带专属错误码，
// 这样前端能区分「换个条目」和「重试」。
func TestReviewDecideNotFoundIs404(t *testing.T) {
	r := muxWithReviewRoutes(ReviewDecide(
		&fakeReviewDecider{err: incident.ErrReviewEdgeNotFound}, discardLogger()))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reviews/7/101",
		newBody(t, `{"decision":"reject","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeReviewEdgeNotFound {
		t.Errorf("错误码 = %q，期望 %q", got, CodeReviewEdgeNotFound)
	}
}

func TestReviewDecideStorageErrorIs503(t *testing.T) {
	r := muxWithReviewRoutes(ReviewDecide(
		&fakeReviewDecider{err: errors.New("写入失败")}, discardLogger()))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/reviews/7/101",
		newBody(t, `{"decision":"confirm","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeStorageUnavail {
		t.Errorf("错误码 = %q，期望 %q", got, CodeStorageUnavail)
	}
}

// TestReviewDecideBadRequest 覆盖三类 400：非法动作、非 JSON、未知字段。
func TestReviewDecideBadRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"动作非法", `{"decision":"maybe","actor":"yu"}`},
		{"非 JSON", `{"decision":`},
		{"未知字段（ADR-010）", `{"decision":"confirm","actor":"yu","typo":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := muxWithReviewRoutes(ReviewDecide(&fakeReviewDecider{}, discardLogger()))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/reviews/7/101", newBody(t, tc.body))
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（body=%s）", rec.Code, rec.Body.String())
			}
			if got := decodeErrorCode(t, rec); got != CodeInvalidBody {
				t.Errorf("错误码 = %q，期望 %q", got, CodeInvalidBody)
			}
		})
	}
}

// TestReviewDecideBadIDsIs400 断言两个路径参数都要校验，不能只校验前一个。
func TestReviewDecideBadIDsIs400(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"事件 id 非数字", "/api/v1/reviews/abc/101"},
		{"事件 id 非正", "/api/v1/reviews/0/101"},
		{"告警 id 非数字", "/api/v1/reviews/7/xyz"},
		{"告警 id 非正", "/api/v1/reviews/7/-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := muxWithReviewRoutes(ReviewDecide(&fakeReviewDecider{}, discardLogger()))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path,
				newBody(t, `{"decision":"confirm","actor":"yu"}`))
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（body=%s）", rec.Code, rec.Body.String())
			}
			if got := decodeErrorCode(t, rec); got != CodeInvalidBody {
				t.Errorf("错误码 = %q，期望 %q", got, CodeInvalidBody)
			}
		})
	}
}
