package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/incident"
)

// 本文件覆盖 M3 四个端点的 HTTP 契约：状态码、错误码、以及「脏参数退化」。
// 领域规则（谁能转到谁）由 internal/incident 的用例覆盖，此处不重复。

// --- 替身 ---

type fakeIncidentLister struct {
	res    incident.ListResult
	err    error
	filter incident.ListFilter
}

func (f *fakeIncidentLister) List(_ context.Context, arg incident.ListFilter) (incident.ListResult, error) {
	f.filter = arg
	return f.res, f.err
}

type fakeIncidentDetailer struct {
	detail incident.Detail
	err    error
	gotID  int64
}

func (f *fakeIncidentDetailer) Detail(_ context.Context, id int64) (incident.Detail, error) {
	f.gotID = id
	return f.detail, f.err
}

type fakeIncidentTransitioner struct {
	snap     incident.Snapshot
	err      error
	gotID    int64
	gotTo    incident.Status
	gotActor string
}

func (f *fakeIncidentTransitioner) Transition(_ context.Context, id int64, to incident.Status, actor string) (incident.Snapshot, error) {
	f.gotID, f.gotTo, f.gotActor = id, to, actor
	return f.snap, f.err
}

type fakeIncidentCommenter struct {
	entry    incident.Entry
	err      error
	gotID    int64
	gotActor string
	gotNote  string
}

func (f *fakeIncidentCommenter) Comment(_ context.Context, id int64, actor, note string) (incident.Entry, error) {
	f.gotID, f.gotActor, f.gotNote = id, actor, note
	return f.entry, f.err
}

// muxWithIncidentRoutes 把带 {id} 的路由挂到真实 chi 路由树上
// （handler 依赖 chi.URLParam 取路径参数，直接调 handler 拿不到）。
func muxWithIncidentRoutes(pattern string, h http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/incidents/{id}", h)
	r.Post("/api/v1/incidents/{id}/transition", h)
	r.Post("/api/v1/incidents/{id}/comments", h)
	if pattern == "/api/v1/incidents" {
		r.Get(pattern, h)
	}
	return r
}

// --- 列表 ---

func TestIncidentListOK(t *testing.T) {
	lister := &fakeIncidentLister{res: incident.ListResult{
		Items:   []incident.Summary{{ID: 1, Status: incident.StatusOpen, Title: "checkout 延迟"}},
		HasMore: true,
	}}
	h := IncidentList(lister, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents?status=open&limit=1", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	var got incidentListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.Count != 1 || len(got.Incidents) != 1 {
		t.Errorf("count=%d items=%d，期望 1/1", got.Count, len(got.Incidents))
	}
	if !got.HasMore {
		t.Error("has_more 应原样透传")
	}
	// 过滤参数应真的下传（否则前端的筛选控件是摆设）。
	if len(lister.filter.Statuses) != 1 || lister.filter.Statuses[0] != incident.StatusOpen {
		t.Errorf("status 过滤未下传，实得 %v", lister.filter.Statuses)
	}
	if lister.filter.Limit != 1 {
		t.Errorf("limit = %d，期望 1", lister.filter.Limit)
	}
}

// TestIncidentListBadParamsDegrade 断言脏参数退化为默认值而不是 400。
func TestIncidentListBadParamsDegrade(t *testing.T) {
	lister := &fakeIncidentLister{}
	h := IncidentList(lister, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/incidents?limit=abc&offset=-3&status=bogus&from=not-a-time", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("脏参数不应导致非 200，实得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if len(lister.filter.Statuses) != 0 {
		t.Errorf("非法 status 应被忽略，实得 %v", lister.filter.Statuses)
	}
	if lister.filter.From != nil {
		t.Errorf("非法时间参数应被忽略，实得 %v", lister.filter.From)
	}
}

// TestIncidentListStorageError 断言存储故障是 503（可重试），而不是空列表。
func TestIncidentListStorageError(t *testing.T) {
	h := IncidentList(&fakeIncidentLister{err: errors.New("库挂了")}, discardLogger())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeStorageUnavail {
		t.Errorf("错误码 = %q，期望 %q", got, CodeStorageUnavail)
	}
}

// --- 详情 ---

func TestIncidentDetailOK(t *testing.T) {
	detailer := &fakeIncidentDetailer{detail: incident.Detail{
		Summary:           incident.Summary{ID: 7, Status: incident.StatusInvestigating},
		Alerts:            []incident.AlertLink{{AlertID: 1}},
		Timeline:          []incident.Entry{{EventType: incident.EventAlertFired}},
		TruncatedTimeline: true,
	}}
	var h http.HandlerFunc = IncidentDetail(detailer, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/7", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var got incidentDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.Incident.ID != 7 {
		t.Errorf("incident.id = %d，期望 7", got.Incident.ID)
	}
	// 截断标志必须回传，否则前端无法提示「仅显示最近 N 条」。
	if !got.Truncated.Timeline || got.Truncated.Alerts {
		t.Errorf("truncated = %+v，期望 timeline=true / alerts=false", got.Truncated)
	}
	if detailer.gotID != 7 {
		t.Errorf("路径参数解析 = %d，期望 7", detailer.gotID)
	}
}

func TestIncidentDetailBadID(t *testing.T) {
	var h http.HandlerFunc = IncidentDetail(&fakeIncidentDetailer{}, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/not-a-number", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeInvalidBody {
		t.Errorf("错误码 = %q，期望 %q", got, CodeInvalidBody)
	}
}

func TestIncidentDetailNotFound(t *testing.T) {
	h := IncidentDetail(&fakeIncidentDetailer{err: incident.ErrIncidentNotFound}, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents/404", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeIncidentNotFound {
		t.Errorf("错误码 = %q，期望 %q", got, CodeIncidentNotFound)
	}
}

// --- 状态流转 ---

func TestIncidentTransitionOK(t *testing.T) {
	svc := &fakeIncidentTransitioner{snap: incident.Snapshot{ID: 7, Status: incident.StatusInvestigating}}
	h := IncidentTransition(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/transition",
		newBody(t, `{"to":"investigating","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	if svc.gotID != 7 || svc.gotTo != incident.StatusInvestigating || svc.gotActor != "yu" {
		t.Errorf("入参未正确下传: id=%d to=%s actor=%s", svc.gotID, svc.gotTo, svc.gotActor)
	}
	var got transitionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got.Incident.Status != incident.StatusInvestigating {
		t.Errorf("响应状态 = %s，期望 investigating", got.Incident.Status)
	}
}

// TestIncidentTransitionInvalidIs422 断言非法转换是 422 + invalid_transition，
// 且**不是** 409/404 —— 前端靠 code 决定「换个状态」还是「刷新重试」。
func TestIncidentTransitionInvalidIs422(t *testing.T) {
	svc := &fakeIncidentTransitioner{
		err: incident.NewTransitionError(incident.StatusInvestigating, incident.StatusAcknowledged),
	}
	h := IncidentTransition(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/transition",
		newBody(t, `{"to":"acknowledged","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422（body=%s）", rec.Code, rec.Body.String())
	}
	if got := decodeErrorCode(t, rec); got != CodeInvalidTransition {
		t.Errorf("错误码 = %q，期望 %q", got, CodeInvalidTransition)
	}
	// 文案须含 from 与 to（手册 §4.1），便于前端直接展示。
	if msg := decodeError(t, rec); !strings.Contains(msg, "investigating") || !strings.Contains(msg, "acknowledged") {
		t.Errorf("错误文案应含 from/to，实得 %q", msg)
	}
}

// TestIncidentTransitionConflictIs409 断言并发冲突是 409，与 422 区分开。
func TestIncidentTransitionConflictIs409(t *testing.T) {
	svc := &fakeIncidentTransitioner{err: &incident.ConflictError{ID: 7, Expected: incident.StatusOpen}}
	h := IncidentTransition(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/transition",
		newBody(t, `{"to":"resolved","actor":"yu"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeStatusConflict {
		t.Errorf("错误码 = %q，期望 %q", got, CodeStatusConflict)
	}
}

// TestIncidentTransitionBadBody 覆盖三类 400：非 JSON、未知字段、目标状态非法。
func TestIncidentTransitionBadBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 JSON", `{"to":`},
		{"未知字段（ADR-010）", `{"to":"resolved","actor":"yu","typo":1}`},
		{"目标状态非法", `{"to":"triage","actor":"yu"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := IncidentTransition(&fakeIncidentTransitioner{}, discardLogger())
			r := muxWithIncidentRoutes("", h)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/transition", newBody(t, tc.body))
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

// --- 备注 ---

func TestIncidentCommentCreated(t *testing.T) {
	svc := &fakeIncidentCommenter{entry: incident.Entry{
		IncidentID: 7, EventType: incident.EventUserComment, Description: "已联系支付团队",
	}}
	h := IncidentComment(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/comments",
		newBody(t, `{"actor":"yu","note":"已联系支付团队"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201（body=%s）", rec.Code, rec.Body.String())
	}
	if svc.gotNote != "已联系支付团队" || svc.gotActor != "yu" {
		t.Errorf("入参未正确下传: actor=%s note=%s", svc.gotActor, svc.gotNote)
	}
}

// TestIncidentCommentEmptyNoteIs400 断言空备注是 400 而不是写一条空留痕。
func TestIncidentCommentEmptyNoteIs400(t *testing.T) {
	svc := &fakeIncidentCommenter{err: incident.ErrEmptyNote}
	h := IncidentComment(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/7/comments",
		newBody(t, `{"actor":"yu","note":"   "}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	if got := decodeErrorCode(t, rec); got != CodeInvalidBody {
		t.Errorf("错误码 = %q，期望 %q", got, CodeInvalidBody)
	}
}

// TestIncidentCommentNotFoundIs404 断言备注到不存在的事件是 404，而不是 503。
func TestIncidentCommentNotFoundIs404(t *testing.T) {
	svc := &fakeIncidentCommenter{err: incident.ErrIncidentNotFound}
	h := IncidentComment(svc, discardLogger())
	r := muxWithIncidentRoutes("", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/incidents/999/comments",
		newBody(t, `{"actor":"yu","note":"hello"}`))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
}

// --- 工具 ---

// newBody 把字符串变成请求体。
func newBody(t *testing.T, raw string) io.Reader {
	t.Helper()
	return strings.NewReader(raw)
}

// decodeErrorCode 从响应体取出 code 字段。
func decodeErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应失败: %v（body=%s）", err, rec.Body.String())
	}
	return body.Code
}
