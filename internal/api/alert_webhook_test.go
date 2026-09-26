package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/repository"
	"github.com/yu/iip/internal/testsupport"
)

// webhookPath 是被测端点路径（与 main.go 装配处一致）。
const webhookPath = "/api/v1/alerts/webhook"

// discardLogger 让测试输出不被服务层日志淹没。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- 内存版 Repository（handler 单测用，无需连库） ---

// fakeRepo 是 alert.Repository 的内存实现。
//
// 它刻意复刻真库的**关键语义**而非仅是"能存能取"：Insert 在同指纹已有 firing 时
// 返回 inserted=false（对应 0002 的部分唯一索引），Resolve 原地改行不新增。
// 若这里写松了，H-03 这类幂等用例就会变成假绿。
type fakeRepo struct {
	rows   map[string]alert.StoredAlert
	nextID int64
	// failErr 非 nil 时所有方法直接返回它，用于模拟存储不可用。
	failErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]alert.StoredAlert{}}
}

func (f *fakeRepo) LatestByFingerprint(_ context.Context, fingerprint string) (alert.StoredAlert, error) {
	if f.failErr != nil {
		return alert.StoredAlert{}, f.failErr
	}
	row, ok := f.rows[fingerprint]
	if !ok {
		return alert.StoredAlert{}, alert.ErrNotFound
	}
	return row, nil
}

func (f *fakeRepo) Insert(_ context.Context, in *alert.Alert) (alert.StoredAlert, bool, error) {
	if f.failErr != nil {
		return alert.StoredAlert{}, false, f.failErr
	}
	if row, ok := f.rows[in.Fingerprint]; ok && row.Status == alert.StatusFiring {
		// 同指纹已有 firing —— 等价于 ON CONFLICT DO NOTHING 不返回行。
		return alert.StoredAlert{}, false, nil
	}
	f.nextID++
	stored := alert.StoredAlert{ID: f.nextID, Alert: *in}
	f.rows[in.Fingerprint] = stored
	return stored, true, nil
}

func (f *fakeRepo) Resolve(_ context.Context, id int64, endsAt *time.Time) error {
	if f.failErr != nil {
		return f.failErr
	}
	for fp, row := range f.rows {
		if row.ID == id {
			row.Status = alert.StatusResolved
			row.EndsAt = endsAt
			f.rows[fp] = row
			return nil
		}
	}
	return alert.ErrNotFound
}

// --- 请求辅助 ---

// postWebhook 投递一个请求体，返回响应记录器。
func postWebhook(t *testing.T, h http.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// decodeResult 解出成功响应体中的计数。
func decodeResult(t *testing.T, rec *httptest.ResponseRecorder) alert.Result {
	t.Helper()
	var got alert.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 Result JSON: %v（原始: %s）", err, rec.Body.String())
	}
	return got
}

// decodeError 解出错误响应体中的 error 字段。
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("错误响应体不是合法 JSON: %v（原始: %s）", err, rec.Body.String())
	}
	return got.Error
}

// assertCounts 断言四个计数。
func assertCounts(t *testing.T, label string, got alert.Result, accepted, skipped, updated, rejected int) {
	t.Helper()
	if got.Accepted != accepted || got.Skipped != skipped || got.Updated != updated || got.Rejected != rejected {
		t.Errorf("%s: 计数不符，期望 accepted=%d skipped=%d updated=%d rejected=%d，实际 accepted=%d skipped=%d updated=%d rejected=%d (%v)",
			label, accepted, skipped, updated, rejected,
			got.Accepted, got.Skipped, got.Updated, got.Rejected, got.Errors)
	}
}

// 与手册 §4.1 样例一致的合法 payload（instance 刻意保留，用于验证指纹排除表）。
const firingBody = `{
  "version": "4",
  "groupKey": "namespace:production/alertname:HighLatency",
  "status": "firing",
  "receiver": "iip-webhook",
  "groupLabels": {"alertname": "HighLatency", "namespace": "production"},
  "commonLabels": {"service": "checkout", "team": "platform"},
  "commonAnnotations": {"summary": "High latency"},
  "alerts": [{
    "status": "firing",
    "labels": {"alertname": "HighLatency", "service": "checkout",
               "instance": "checkout-abc123", "namespace": "production",
               "severity": "critical"},
    "annotations": {"summary": "High latency on checkout-abc123", "value": "1250ms"},
    "startsAt": "2026-09-24T18:31:00Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "fingerprint": "abc123def456"
  }]
}`

const resolvedBody = `{
  "version": "4",
  "status": "resolved",
  "alerts": [{
    "status": "resolved",
    "labels": {"alertname": "HighLatency", "service": "checkout",
               "instance": "checkout-abc123", "namespace": "production",
               "severity": "critical"},
    "annotations": {},
    "startsAt": "2026-09-24T18:31:00Z",
    "endsAt": "2026-09-24T19:00:00Z"
  }]
}`

// H-01：合法 payload → 200 且 accepted=1，响应体为计数 JSON。
func TestWebhookH01_AcceptsValidPayload(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	rec := postWebhook(t, h, []byte(firingBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ct)
	}
	assertCounts(t, "首次投递", decodeResult(t, rec), 1, 0, 0, 0)
}

// H-02：请求体非 JSON → 400，且不得把条目计为成功。
func TestWebhookH02_RejectsNonJSON(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	rec := postWebhook(t, h, []byte("not-json"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if msg := decodeError(t, rec); msg == "" {
		t.Error("400 响应体应包含 error 说明")
	}
	if len(repo.rows) != 0 {
		t.Errorf("非法请求不得写入任何行，实际 %d 行", len(repo.rows))
	}
}

// H-02b：结构缺失（无 version）同样整批 400。
func TestWebhookH02b_RejectsMissingVersion(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	rec := postWebhook(t, h, []byte(`{"status":"firing","alerts":[]}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
}

// H-03：同一 payload 连发两次 —— 两次均 200，第二次 skipped=1，且库内仍 1 行。
//
// 「两次均 200」是硬要求：回非 2xx 会让 Alertmanager 无限重试。
func TestWebhookH03_IsIdempotent(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	first := postWebhook(t, h, []byte(firingBody))
	if first.Code != http.StatusOK {
		t.Fatalf("首次投递期望 200，实际 %d", first.Code)
	}
	assertCounts(t, "首次投递", decodeResult(t, first), 1, 0, 0, 0)

	second := postWebhook(t, h, []byte(firingBody))
	if second.Code != http.StatusOK {
		t.Fatalf("重复投递必须回 200（否则 Alertmanager 会无限重试），实际 %d", second.Code)
	}
	assertCounts(t, "重复投递", decodeResult(t, second), 0, 1, 0, 0)

	if len(repo.rows) != 1 {
		t.Errorf("重复投递不得增行，期望 1 行，实际 %d 行", len(repo.rows))
	}
}

// H-03b：resolved 全生命周期 —— firing → resolved 各回 200，最终 updated=1 且不增行。
func TestWebhookH03b_ResolvedLifecycle(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	if rec := postWebhook(t, h, []byte(firingBody)); rec.Code != http.StatusOK {
		t.Fatalf("firing 投递期望 200，实际 %d", rec.Code)
	}

	rec := postWebhook(t, h, []byte(resolvedBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolved 投递期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	assertCounts(t, "resolved 投递", decodeResult(t, rec), 0, 0, 1, 0)

	if len(repo.rows) != 1 {
		t.Errorf("resolved 不得新增行，期望 1 行，实际 %d 行", len(repo.rows))
	}
	for _, row := range repo.rows {
		if row.Status != alert.StatusResolved {
			t.Errorf("行状态应为 resolved，实际 %s", row.Status)
		}
		if row.EndsAt == nil {
			t.Error("resolved 行的 ends_at 不应为空")
		}
	}
}

// H-04：存储不可用 → 503（而非 200/500），让 Alertmanager 重试。
func TestWebhookH04_StorageUnavailableIs503(t *testing.T) {
	repo := newFakeRepo()
	repo.failErr = errors.New("模拟：连接池已关闭")
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	rec := postWebhook(t, h, []byte(firingBody))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("存储不可用期望 503，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if msg := decodeError(t, rec); msg != msgStorageUnavailable {
		t.Errorf("503 响应体应为 %q，实际 %q", msgStorageUnavailable, msg)
	}
}

// H-04（真库版）：把连接池真的关掉后投递 —— 验证真驱动路径上也回 503。
//
// 与上面的假实现版互补：假实现证明「handler 把 error 映射为 503」，
// 本用例证明「真仓储在连接不可用时确实返回 error」。
func TestWebhookH04_ClosedPoolIs503(t *testing.T) {
	pool := testsupport.NewDB(t)
	h := newRealWebhookHandler(pool, 1<<20)

	// 先证明库是通的：此时应当 200。
	if rec := postWebhook(t, h, []byte(firingBody)); rec.Code != http.StatusOK {
		t.Fatalf("关库前投递期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}

	pool.Close()

	rec := postWebhook(t, h, []byte(firingBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("关库后期望 503，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
}

// H-05：请求体超过上限 → 413，且响应体与手册逐字一致。
func TestWebhookH05_PayloadTooLargeIs413(t *testing.T) {
	repo := newFakeRepo()
	const maxBytes = 1 << 20
	h := AlertWebhook(alert.NewService(repo, discardLogger()), maxBytes, discardLogger())

	// 刻意构造「合法 JSON 但超大」的体：若上限被实现成截断，这里会退化成 400，
	// 于是「期望 413」的断言就能把错误实现揪出来。
	oversized := []byte(`{"version":"4","status":"firing","alerts":[],"pad":"` +
		strings.Repeat("x", maxBytes+1) + `"}`)

	rec := postWebhook(t, h, oversized)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限期望 413，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if msg := decodeError(t, rec); msg != msgPayloadTooLarge {
		t.Errorf("413 响应体应为 %q，实际 %q", msgPayloadTooLarge, msg)
	}
	if len(repo.rows) != 0 {
		t.Errorf("超限请求不得写入任何行，实际 %d 行", len(repo.rows))
	}
}

// H-05b：恰好等于上限的请求体必须被接受（边界不外扩）。
//
// 上限判定差一字节就让一整批合法告警被拒，属于典型的"线上才发现"缺陷。
func TestWebhookH05b_ExactlyAtLimitIsAccepted(t *testing.T) {
	repo := newFakeRepo()
	const maxBytes = 1024
	h := AlertWebhook(alert.NewService(repo, discardLogger()), maxBytes, discardLogger())

	const prefix = `{"version":"4","status":"firing","alerts":[],"pad":"`
	const suffix = `"}`
	padLen := maxBytes - len(prefix) - len(suffix)
	if padLen < 0 {
		t.Fatalf("上限 %d 太小，无法构造边界体", maxBytes)
	}
	exact := []byte(prefix + strings.Repeat("x", padLen) + suffix)
	if len(exact) != maxBytes {
		t.Fatalf("构造的请求体长度应为 %d，实际 %d", maxBytes, len(exact))
	}

	rec := postWebhook(t, h, exact)

	if rec.Code != http.StatusOK {
		t.Fatalf("恰好等于上限期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
}

// 空 alerts 数组属正常（Alertmanager 的空 group）→ 200 且零计数。
func TestWebhookEmptyAlertsIsOK(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	rec := postWebhook(t, h, []byte(`{"version":"4","status":"firing","alerts":[]}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("空 alerts 期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	assertCounts(t, "空 alerts", decodeResult(t, rec), 0, 0, 0, 0)
}

// 部分条目非法：整批仍 200，合法条目照常落库，非法条目带下标与原因。
func TestWebhookPartialRejectionIsStillOK(t *testing.T) {
	repo := newFakeRepo()
	h := AlertWebhook(alert.NewService(repo, discardLogger()), 1<<20, discardLogger())

	const mixed = `{
	  "version": "4", "status": "firing",
	  "alerts": [
	    {"status":"firing","labels":{"alertname":"GoodOne","service":"checkout"},
	     "startsAt":"2026-09-24T18:31:00Z"},
	    {"status":"firing","labels":{"service":"checkout"},
	     "startsAt":"2026-09-24T18:31:00Z"},
	    {"status":"firing","labels":{"alertname":"BadTime","service":"checkout"},
	     "startsAt":"2026/09/24 18:31"}
	  ]
	}`

	rec := postWebhook(t, h, []byte(mixed))

	if rec.Code != http.StatusOK {
		t.Fatalf("部分非法不应导致整批失败，期望 200，实际 %d", rec.Code)
	}
	got := decodeResult(t, rec)
	assertCounts(t, "混合批", got, 1, 0, 0, 2)
	if len(got.Errors) != 2 {
		t.Fatalf("期望 2 条错误明细，实际 %v", got.Errors)
	}
	if got.Errors[0].Index != 1 || got.Errors[1].Index != 2 {
		t.Errorf("错误明细下标应为 [1 2]，实际 %v", got.Errors)
	}
	if len(repo.rows) != 1 {
		t.Errorf("应只落库 1 条合法告警，实际 %d 条", len(repo.rows))
	}
}

// newRealWebhookHandler 用**真仓储**装配 handler（依赖 testsupport 提供的一次性库）。
func newRealWebhookHandler(pool *pgxpool.Pool, maxBytes int64) http.HandlerFunc {
	svc := alert.NewService(repository.NewAlerts(pool), discardLogger())
	return AlertWebhook(svc, maxBytes, discardLogger())
}
