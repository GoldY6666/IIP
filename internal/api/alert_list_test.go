package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
)

// fakeQuerier 是 AlertQuerier 的内存实现，用于列表 handler 的单测（无需连库）。
type fakeQuerier struct {
	alerts []alert.StoredAlert
	err    error
	// 记录上次调用收到的 limit / offset，便于断言规整逻辑。
	gotLimit  int32
	gotOffset int32
}

func (q *fakeQuerier) List(_ context.Context, limit, offset int32) ([]alert.StoredAlert, error) {
	q.gotLimit = limit
	q.gotOffset = offset
	if q.err != nil {
		return nil, q.err
	}
	return q.alerts, nil
}

// sampleStoredAlert 构造一条用于断言的已落库告警。
func sampleStoredAlert(id int64) alert.StoredAlert {
	starts := time.Date(2026, 9, 24, 18, 31, 0, 0, time.UTC)
	ends := time.Date(2026, 9, 24, 19, 0, 0, 0, time.UTC)
	return alert.StoredAlert{
		ID: id,
		Alert: alert.Alert{
			Fingerprint: "fp-" + string(rune('a'+id)),
			Source:      alert.SourceAlertmanager,
			Severity:    alert.SeverityCritical,
			Status:      alert.StatusFiring,
			Labels:      []byte(`{"alertname":"HighLatency","instance":"pod-a"}`),
			Annotations: []byte(`{"summary":"p99 高"}`),
			StartsAt:    starts,
			EndsAt:      &ends,
		},
	}
}

// L-01：正常返回 200 且响应体为 {alerts, count}，字段扁平、时间 RFC3339。
func TestAlertListL01_ReturnsAlerts(t *testing.T) {
	q := &fakeQuerier{alerts: []alert.StoredAlert{sampleStoredAlert(1), sampleStoredAlert(2)}}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}

	var body listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v（原始: %s）", err, rec.Body.String())
	}
	if body.Count != 2 {
		t.Errorf("count 应为 2，实际 %d", body.Count)
	}
	if len(body.Alerts) != 2 {
		t.Fatalf("alerts 长度应为 2，实际 %d", len(body.Alerts))
	}
	// 关键契约：嵌入式 Alert 字段被提升为顶层，alertname 出现在 labels 内。
	if body.Alerts[0].ID != 1 {
		t.Errorf("首条 ID 应为 1，实际 %d", body.Alerts[0].ID)
	}
	if body.Alerts[0].Fingerprint != "fp-b" {
		t.Errorf("首条指纹应为 fp-b，实际 %s", body.Alerts[0].Fingerprint)
	}
	// 时间字段可解析为 RFC3339。
	if _, err := time.Parse(time.RFC3339, body.Alerts[0].EndsAt.Format(time.RFC3339)); err != nil {
		t.Errorf("EndsAt 应为可解析时间: %v", err)
	}
}

// L-02：缺省参数时 limit 取默认 100、offset 取 0。
func TestAlertListL02_DefaultParams(t *testing.T) {
	q := &fakeQuerier{}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
	h(rec, req)

	if q.gotLimit != defaultListLimit {
		t.Errorf("缺省 limit 应为 %d，实际 %d", defaultListLimit, q.gotLimit)
	}
	if q.gotOffset != 0 {
		t.Errorf("缺省 offset 应为 0，实际 %d", q.gotOffset)
	}
}

// L-03：limit 超出上限被夹紧到 maxListLimit；offset 负数被夹紧到 0。
func TestAlertListL03_ClampsParams(t *testing.T) {
	q := &fakeQuerier{}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts?limit=99999&offset=-5", http.NoBody)
	h(rec, req)

	if q.gotLimit != maxListLimit {
		t.Errorf("limit 超过上限应被夹紧到 %d，实际 %d", maxListLimit, q.gotLimit)
	}
	if q.gotOffset != 0 {
		t.Errorf("负 offset 应被夹紧到 0，实际 %d", q.gotOffset)
	}
}

// L-04：limit/offset 非法（非整数）退化为默认值，而不是 400。
func TestAlertListL04_InvalidParamsDegrade(t *testing.T) {
	q := &fakeQuerier{}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts?limit=abc&offset=xyz", http.NoBody)
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("参数脏不应 400，期望 200，实际 %d", rec.Code)
	}
	if q.gotLimit != defaultListLimit || q.gotOffset != 0 {
		t.Errorf("非法参数应退化为默认 limit=%d offset=0，实际 limit=%d offset=%d",
			defaultListLimit, q.gotLimit, q.gotOffset)
	}
}

// L-05：存储层故障 → 503（与 webhook 一致），让调用方可重试。
func TestAlertListL05_StorageErrorIs503(t *testing.T) {
	q := &fakeQuerier{err: errors.New("模拟：存储不可用")}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
	h(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("存储不可用期望 503，实际 %d（体: %s）", rec.Code, rec.Body.String())
	}
	if msg := decodeError(t, rec); msg != msgStorageUnavailable {
		t.Errorf("503 响应体应为 %q，实际 %q", msgStorageUnavailable, msg)
	}
}

// L-06：空结果返回 200 + 空数组（而非 null），前端可直接 map。
func TestAlertListL06_EmptyResult(t *testing.T) {
	q := &fakeQuerier{alerts: []alert.StoredAlert{}}
	h := AlertList(q, discardLogger())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", http.NoBody)
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var body listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体非法: %v（原始: %s）", err, rec.Body.String())
	}
	if body.Alerts == nil {
		t.Error("空结果应为空数组而非 null，否则前端 map 会 panic")
	}
	if body.Count != 0 {
		t.Errorf("count 应为 0，实际 %d", body.Count)
	}
}
