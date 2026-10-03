package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yu/iip/internal/evidence"
)

// 本文件覆盖 M4 T4.6 的证据查询端点。
//
// 重点不在「能返回 200」，而在三个**容易被静默做错**的地方：
//  1. 截断必须如实标记，且 total 是真计数（不是 len(items)）；
//  2. 非法 type 参数必须 400，不能静默返回空列表；
//  3. by_type 是辅助信息，它失败不该让主列表变成 503。

// fakeEvidenceLister 是 EvidenceLister 的内存替身。
type fakeEvidenceLister struct {
	list     evidence.EvidenceList
	listErr  error
	counts   map[evidence.Type]int
	countErr error
	// 记录收到的参数，供断言「handler 真的把 query 参数传下去了」。
	gotIncidentID int64
	gotType       evidence.Type
	gotLimit      int
	calls         int
}

func (f *fakeEvidenceLister) ListByIncident(
	_ context.Context, incidentID int64, typ evidence.Type, limit int,
) (evidence.EvidenceList, error) {
	f.calls++
	f.gotIncidentID = incidentID
	f.gotType = typ
	f.gotLimit = limit
	return f.list, f.listErr
}

func (f *fakeEvidenceLister) CountByType(_ context.Context, _ int64) (map[evidence.Type]int, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	if f.counts == nil {
		return evidence.FillTypeCounts(nil), nil
	}
	return f.counts, nil
}

// evidenceTestTime 是本文件用的固定时刻。
var evidenceTestTime = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// serveEvidence 装配一个带 chi 路由的测试服务器。
func serveEvidence(t *testing.T, lister EvidenceLister, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/api/v1/incidents/{id}/evidence", EvidenceList(lister, nil))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, http.NoBody))
	return rec
}

// sampleEvidence 造一条已落库的证据。
func sampleEvidence(id int64, typ evidence.Type, conf float64, valid bool) evidence.StoredEvidence {
	return evidence.StoredEvidence{
		ID:        id,
		Type:      typ,
		Source:    "prometheus",
		Query:     `{service="api"}`,
		Value:     "5xx 错误速率：0.25",
		Timestamp: evidenceTestTime,
		Metadata:  map[string]string{"metric": "errors"},
		// CollectedAt 与 Timestamp 差 3 小时 —— 补采场景。
		// 两者混成一个字段就会让运维以为「证据是 3 小时前发生的」。
		CreatedAt:  evidenceTestTime.Add(3 * time.Hour),
		Confidence: conf,
		Valid:      valid,
	}
}

// TestEvidenceListReturnsTruncationTruth 核心用例：截断信息必须完整且真实。
//
// 这是 M3 AlertCount 教训的直接延续：详情页曾用 len(alerts) 当总数，
// 250 条显示成「200 条关联告警」。证据侧同理——
// 只回 truncated=true 而不回 total，前端仍只能猜；
// 只回 total 而不截断 items，页面会显示 80 条却只有 50 条数据。
func TestEvidenceListReturnsTruncationTruth(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{
				sampleEvidence(3, evidence.TypeMetric, 0.8, true),
				sampleEvidence(2, evidence.TypeMetric, 0, false),
			},
			Total:     73, // 截断前的真值
			Truncated: true,
		},
		counts: map[evidence.Type]int{evidence.TypeMetric: 60, evidence.TypeLog: 13},
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	obj := decodeObject(t, rec)

	if got, _ := obj["total"].(float64); int(got) != 73 {
		t.Errorf("total = %v，期望 73 —— 必须是截断前的真计数，不能是 len(evidence)", obj["total"])
	}
	if got, _ := obj["count"].(float64); int(got) != 2 {
		t.Errorf("count = %v，期望 2（本次实际返回条数）", obj["count"])
	}
	if got, _ := obj["truncated"].(bool); !got {
		t.Error("truncated = false，期望 true —— 手册 §4.2 要求截断可被 API 读到")
	}
	if got, _ := obj["limit"].(float64); int(got) != evidence.DefaultMaxPerIncident {
		t.Errorf("limit = %v，期望 %d（默认值）", obj["limit"], evidence.DefaultMaxPerIncident)
	}

	// count 与 len(evidence) 必须一致 —— 两者含义不同但同源，
	// 不一致说明序列化时有一处漏了。
	items := wantArray(t, "evidence", obj["evidence"], 2)
	_ = items

	// by_type 必须含**全部 6 个类型**，缺失的补 0。
	byType, ok := obj["by_type"].(map[string]any)
	if !ok {
		t.Fatalf("by_type 不是 JSON 对象，实际 %T", obj["by_type"])
	}
	if len(byType) != 6 {
		t.Errorf("by_type 有 %d 个类型，期望 6（合法类型全集，缺失的补 0）：%v", len(byType), byType)
	}
	for _, typ := range evidence.SortedTypes() {
		if _, present := byType[string(typ)]; !present {
			t.Errorf("by_type 缺少类型 %q —— 前端 Tab 角标会直接消失而不是显示 0", typ)
		}
	}
	if got, _ := byType["metric"].(float64); int(got) != 60 {
		t.Errorf("by_type.metric = %v，期望 60", byType["metric"])
	}
	// trace 本期无采集器，必须是 0 而不是缺失
	if got, present := byType["trace"]; !present {
		t.Error("by_type 缺少 trace（本期无采集器，应为 0）")
	} else if f, _ := got.(float64); int(f) != 0 {
		t.Errorf("by_type.trace = %v，期望 0", got)
	}
}

// TestEvidenceListConfidenceNullVersusZero 锁定「未评估」与「0」的区分。
//
// 合成 0 就是把「采集器没有这个判断能力」（OPEN-M4-1）说成
// 「这条证据明确没有支撑力」—— 前者是不确定性，后者是结论。
func TestEvidenceListConfidenceNullVersusZero(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{
				sampleEvidence(1, evidence.TypeMetric, 0.85, true), // 已评估
				sampleEvidence(2, evidence.TypeMetric, 0, false),   // 未评估
			},
			Total: 2,
		},
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", rec.Code, rec.Body.String())
	}

	items := wantArray(t, "evidence", decodeObject(t, rec)["evidence"], 2)

	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("第 0 条不是 JSON 对象")
	}
	if c, _ := first["confidence"].(float64); c != 0.85 {
		t.Errorf("已评估的 confidence = %v，期望 0.85", first["confidence"])
	}

	second, ok := items[1].(map[string]any)
	if !ok {
		t.Fatalf("第 1 条不是 JSON 对象")
	}
	// 必须是 JSON null：键**存在**但值为 null。
	// 若整个键被 omitempty 掉，前端拿到 undefined 与 null 会走出不同分支。
	c, present := second["confidence"]
	if !present {
		t.Error("未评估的证据缺 confidence 键 —— 前端拿不到字段会渲染成异常")
	}
	if c != nil {
		t.Errorf("未评估的 confidence = %v，期望 JSON null（不能是 0）", c)
	}
}

// TestEvidenceListRejectsInvalidType 非法 type 必须 400。
//
// 静默返回空列表会被前端渲染成「该类型没有证据」，
// 把参数拼写错误（?type=metirc）伪装成事实 —— 用户无从发现。
func TestEvidenceListRejectsInvalidType(t *testing.T) {
	lister := &fakeEvidenceLister{}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence?type=metirc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d，期望 400（body=%s）", rec.Code, rec.Body.String())
	}
	obj := decodeObject(t, rec)
	if obj["code"] != CodeEvidenceTypeInvalid {
		t.Errorf("code = %v，期望 %q", obj["code"], CodeEvidenceTypeInvalid)
	}
	if lister.calls != 0 {
		t.Errorf("参数非法时仍查了库 %d 次 —— 应该在查库前就拒掉", lister.calls)
	}
}

// TestEvidenceListAcceptsValidType 六种合法类型都要放行。
func TestEvidenceListAcceptsValidType(t *testing.T) {
	for _, typ := range evidence.SortedTypes() {
		lister := &fakeEvidenceLister{list: evidence.EvidenceList{Total: 0}}
		rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence?type="+string(typ))
		if rec.Code != http.StatusOK {
			t.Errorf("type=%s 返回 HTTP %d，期望 200：%s", typ, rec.Code, rec.Body.String())
		}
		if lister.gotType != typ {
			t.Errorf("type=%s 未原样传给仓储，实际 %q", typ, lister.gotType)
		}
	}
}

// TestEvidenceListRejectsBadParams 非法 incidentID 与 limit 都必须 400。
func TestEvidenceListRejectsBadParams(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{"incidentID 非数字", "/api/v1/incidents/abc/evidence"},
		{"incidentID 为 0", "/api/v1/incidents/0/evidence"},
		{"incidentID 为负", "/api/v1/incidents/-1/evidence"},
		{"limit 非整数", "/api/v1/incidents/12/evidence?limit=abc"},
		{"limit 为 0", "/api/v1/incidents/12/evidence?limit=0"},
		{"limit 为负", "/api/v1/incidents/12/evidence?limit=-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeEvidenceLister{}
			rec := serveEvidence(t, lister, tc.target)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("HTTP %d，期望 400（body=%s）", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEvidenceListClampsLimit 超大 limit 被夹紧而非照单全收。
//
// limit 来自 query string，照单全收等于让一个 HTTP 请求把整张表拉进内存。
// 而夹紧也**必须回传生效值**，否则前端显示的仍是它请求的那个数。
func TestEvidenceListClampsLimit(t *testing.T) {
	lister := &fakeEvidenceLister{list: evidence.EvidenceList{Total: 0}}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence?limit=999999")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", rec.Code, rec.Body.String())
	}
	if lister.gotLimit != maxEvidenceLimit {
		t.Errorf("传给仓储的 limit = %d，期望被夹到 %d", lister.gotLimit, maxEvidenceLimit)
	}
	obj := decodeObject(t, rec)
	if got, _ := obj["limit"].(float64); int(got) != maxEvidenceLimit {
		t.Errorf("响应里 limit = %v，期望 %d（生效值，不是请求值）", obj["limit"], maxEvidenceLimit)
	}
}

// TestEvidenceListByTypeFailureKeepsMainList by_type 失败不该让主列表 503。
//
// 主数据都拿到了，为一个角标计数把整页变成错误页，
// 是把「锦上添花」当成了「必需」。此时页面照常显示证据，只少一个角标。
func TestEvidenceListByTypeFailureKeepsMainList(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{sampleEvidence(1, evidence.TypeMetric, 0.5, true)},
			Total: 1,
		},
		countErr: errors.New("统计表锁超时"),
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	if rec.Code != http.StatusOK {
		t.Fatalf("by_type 统计失败时 HTTP %d，期望 200（主列表不该受影响）：%s",
			rec.Code, rec.Body.String())
	}
	obj := decodeObject(t, rec)
	if got, _ := obj["count"].(float64); int(got) != 1 {
		t.Errorf("count = %v，期望 1（主列表应照常返回）", obj["count"])
	}
	byType, ok := obj["by_type"].(map[string]any)
	if !ok {
		t.Fatalf("by_type 应为对象而非 null，实际 %T", obj["by_type"])
	}
	// 统计失败时给**全 0**而不是空对象：空对象会让前端把
	// 「统计失败」与「真的是 0 条」渲染成同一个样子。
	// 角标显示 0 + 主列表可信，是失败时最不容易误导用户的组合。
	if len(byType) != len(evidence.SortedTypes()) {
		t.Errorf("by_type 有 %d 个类型，期望 %d（失败时也要给全 0，否则"+
			"前端分不清「统计失败」与「真的是 0」）：%v",
			len(byType), len(evidence.SortedTypes()), byType)
	}
	for typ, v := range byType {
		if f, _ := v.(float64); f != 0 {
			t.Errorf("by_type.%s = %v，统计失败时该为 0（不能编数字）", typ, v)
		}
	}
}

// TestEvidenceListMainQueryFailureIs503 主查询失败必须 503。
func TestEvidenceListMainQueryFailureIs503(t *testing.T) {
	lister := &fakeEvidenceLister{listErr: errors.New("连接池耗尽")}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d，期望 503（body=%s）", rec.Code, rec.Body.String())
	}
	if obj := decodeObject(t, rec); obj["code"] != CodeStorageUnavail {
		t.Errorf("code = %v，期望 %q", obj["code"], CodeStorageUnavail)
	}
}

// TestEvidenceListPassesIncidentIDFromPath 路径里的事件 id 必须传到仓储。
func TestEvidenceListPassesIncidentIDFromPath(t *testing.T) {
	lister := &fakeEvidenceLister{list: evidence.EvidenceList{Total: 0}}
	serveEvidence(t, lister, "/api/v1/incidents/4242/evidence")
	if lister.gotIncidentID != 4242 {
		t.Errorf("传给仓储的 incident_id = %d，期望 4242", lister.gotIncidentID)
	}
}

// TestEvidenceListTimestampsAreDistinctFields timestamp 与 collected_at 必须分别回传。
//
// 补采一条三小时前的日志时两者差三小时。合成一个字段，
// 运维会以为「这条证据是三小时前发生的」，
// 而实际上是「我们三小时前才采到一条更早的日志」。
func TestEvidenceListTimestampsAreDistinctFields(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{sampleEvidence(1, evidence.TypeMetric, 0.5, true)},
			Total: 1,
		},
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	obj := decodeObject(t, rec)
	items := wantArray(t, "evidence", obj["evidence"], 1)
	item, _ := items[0].(map[string]any)

	ts, _ := item["timestamp"].(string)
	collected, _ := item["collected_at"].(string)
	if ts == "" || collected == "" {
		t.Fatalf("timestamp=%q collected_at=%q，两者都必须有值", ts, collected)
	}
	if ts == collected {
		t.Errorf("timestamp 与 collected_at 相同（%q）—— 补采场景下它们应相差 3 小时", ts)
	}
	if !strings.HasPrefix(ts, "2026-10-03T12:00:00") {
		t.Errorf("timestamp = %q，期望证据自身时刻 12:00:00Z", ts)
	}
	if !strings.HasPrefix(collected, "2026-10-03T15:00:00") {
		t.Errorf("collected_at = %q，期望落库时刻 15:00:00Z", collected)
	}
	// 必须是 RFC3339 而非 Go 默认的 RFC3339Nano：
	// 后者在同一秒内的两条证据会给出长度不同的时间串，前端字符串排序会错。
	if strings.Contains(ts, ".") {
		t.Errorf("timestamp = %q 含纳秒小数，应统一为 RFC3339（秒精度）", ts)
	}
}

// TestEvidenceListResponseKeyContract 断言键名契约（snake_case，不多不少）。
//
// 复用 response_contract_test.go 的 assertKeys：解成 map 逐个比对，
// 与结构体定义解耦 —— 结构体 round-trip 在 tag 缺失时依然自洽，测不出来。
func TestEvidenceListResponseKeyContract(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{sampleEvidence(1, evidence.TypeMetric, 0.5, true)},
			Total: 1,
		},
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence?type=metric")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", rec.Code, rec.Body.String())
	}
	obj := decodeObject(t, rec)

	// 不传 type 时 type 键带 omitempty，不该出现；这里传了，必须出现。
	assertKeys(t, "顶层", obj, []string{
		"evidence", "count", "total", "truncated", "limit", "by_type", "type",
	})

	items := wantArray(t, "evidence", obj["evidence"], 1)
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("evidence[0] 不是 JSON 对象，实际 %T", items[0])
	}
	assertKeys(t, "evidence[0]", item, []string{
		"id", "type", "source", "query", "value",
		"timestamp", "confidence", "metadata", "collected_at",
	})
}

// TestEvidenceListOmitsEmptyTypeFilter 不传 type 时响应里不出现 type 键。
func TestEvidenceListOmitsEmptyTypeFilter(t *testing.T) {
	lister := &fakeEvidenceLister{list: evidence.EvidenceList{Total: 0}}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	obj := decodeObject(t, rec)
	if _, present := obj["type"]; present {
		t.Error("未传 type 时响应里不应出现 type 键（omitempty）")
	}
	if lister.gotType != "" {
		t.Errorf("传给仓储的 type = %q，期望空串（表示不过滤）", lister.gotType)
	}
}

// TestEvidenceListEmptyIsNotError 空结果返回 200 + 空数组，不是 404。
//
// 「采到了但无内容」是正常结果：事件刚创建、证据还没采到。
// 返回 404 会让前端把「还没采到」渲染成「事件不存在」。
func TestEvidenceListEmptyIsNotError(t *testing.T) {
	lister := &fakeEvidenceLister{list: evidence.EvidenceList{Items: nil, Total: 0}}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200", rec.Code)
	}
	obj := decodeObject(t, rec)
	// 必须是 [] 而不是 null：前端 forEach 遍历 null 会直接抛错。
	items, ok := obj["evidence"].([]any)
	if !ok {
		t.Fatalf("evidence 应为数组，实际 %T（null 会让前端遍历报错）", obj["evidence"])
	}
	if len(items) != 0 {
		t.Errorf("evidence 有 %d 条，期望 0", len(items))
	}
	if got, _ := obj["total"].(float64); int(got) != 0 {
		t.Errorf("total = %v，期望 0", obj["total"])
	}
}

// TestEvidenceListMetadataAlwaysObject metadata 必须是对象而非 null。
func TestEvidenceListMetadataAlwaysObject(t *testing.T) {
	item := sampleEvidence(1, evidence.TypeLog, 0, false)
	item.Metadata = nil // 仓储层可能给出 nil
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{Items: []evidence.StoredEvidence{item}, Total: 1},
	}
	rec := serveEvidence(t, lister, "/api/v1/incidents/12/evidence")
	items := wantArray(t, "evidence", decodeObject(t, rec)["evidence"], 1)
	m, ok := items[0].(map[string]any)["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata 应为对象，实际 %T（null 会让前端 Object.keys 报错）",
			items[0].(map[string]any)["metadata"])
	}
	if len(m) != 0 {
		t.Errorf("metadata = %v，期望空对象", m)
	}
}

// TestEvidenceListHandlerSatisfiesHTTPContract 端到端过一次真实 server。
//
// 前面所有用例都直接调 handler；这条走完整 chi 路由，
// 验「路径参数真的能取到」—— 参数名写错（如 incidentID vs id）
// 只有在真实路由下才会暴露。
func TestEvidenceListHandlerSatisfiesHTTPContract(t *testing.T) {
	lister := &fakeEvidenceLister{
		list: evidence.EvidenceList{
			Items: []evidence.StoredEvidence{sampleEvidence(1, evidence.TypeMetric, 0.5, true)},
			Total: 1,
		},
	}
	r := chi.NewRouter()
	r.Get("/api/v1/incidents/{id}/evidence", EvidenceList(lister, nil))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/incidents/77/evidence")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200", resp.StatusCode)
	}
	if lister.gotIncidentID != 77 {
		t.Errorf("incident_id = %d，期望 77（路径参数没取到）", lister.gotIncidentID)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if f, _ := got["count"].(float64); int(f) != 1 {
		t.Errorf("count = %v，期望 1", got["count"])
	}
}
