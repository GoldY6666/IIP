package correlation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
)

// 本文件用假存储覆盖 Service.CorrelateAlerts 的编排逻辑：
// 三档决策各走哪条落库路径、非 firing 是否跳过、引擎开关、
// 各类存储故障是否按预期上抛（由 handler 记日志），以及批次内累积与历史共现记忆化。

// linkCall 记录一次「建立关联边」的调用参数。
type linkCall struct {
	IncidentID    int64
	AlertID       int64
	AlertStartsAt time.Time
	Score         *float64
	NeedsReview   bool
}

// fakeIncidentStore 是 IncidentStore 的内存替身。
type fakeIncidentStore struct {
	nextID     int64
	candidates []CandidateAlert

	created  []Incident
	links    []linkCall
	setCalls [][2]int64

	listCalls int

	listErr   error
	createErr error
	linkErr   error
	setErr    error
}

func (f *fakeIncidentStore) CreateIncident(_ context.Context, in Incident) (Incident, error) {
	if f.createErr != nil {
		return Incident{}, f.createErr
	}
	f.nextID++
	in.ID = f.nextID
	if in.Status == "" {
		in.Status = "open"
	}
	f.created = append(f.created, in)
	return in, nil
}

func (f *fakeIncidentStore) ListOpenIncidentAlerts(_ context.Context, _ int32) ([]CandidateAlert, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.candidates, nil
}

func (f *fakeIncidentStore) LinkAlertToIncident(_ context.Context, incidentID, alertID int64, alertStartsAt time.Time, score *float64, needsReview bool) error {
	if f.linkErr != nil {
		return f.linkErr
	}
	f.links = append(f.links, linkCall{IncidentID: incidentID, AlertID: alertID, AlertStartsAt: alertStartsAt, Score: score, NeedsReview: needsReview})
	return nil
}

func (f *fakeIncidentStore) SetAlertIncident(_ context.Context, alertID, incidentID int64) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, [2]int64{alertID, incidentID})
	return nil
}

// fakeConfigStore 是 ConfigStore 的内存替身。
type fakeConfigStore struct {
	cfg    Config
	getErr error
	puts   []Config
	putErr error

	co      float64
	total   float64
	coErr   error
	coCalls int

	getCalls int
}

func (f *fakeConfigStore) GetConfig(context.Context) (Config, error) {
	f.getCalls++
	if f.getErr != nil {
		return Config{}, f.getErr
	}
	return f.cfg, nil
}

func (f *fakeConfigStore) PutConfig(_ context.Context, cfg Config) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, cfg)
	return nil
}

func (f *fakeConfigStore) CoOccurrence(context.Context, string, string) (float64, float64, error) {
	f.coCalls++
	if f.coErr != nil {
		return 0, 0, f.coErr
	}
	return f.co, f.total, nil
}

var (
	_ IncidentStore = (*fakeIncidentStore)(nil)
	_ ConfigStore   = (*fakeConfigStore)(nil)
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newCorrService(store *fakeIncidentStore, cfgStore *fakeConfigStore, topo *Topology) *Service {
	return NewService(store, cfgStore, topo, discardLogger())
}

// corrWindow 是所有关联用例共用的时间窗（固定值，避免依赖真实时钟）。
var (
	corrStart = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	corrEnd   = corrStart.Add(10 * time.Minute)
)

// corrAlert 构造一条已落库告警；instance 属动态键，不参与标签相似度。
func corrAlert(id int64, service, alertname, status, instance string, start, end time.Time, hasEnd bool) alert.StoredAlert {
	labels, _ := json.Marshal(map[string]string{
		"alertname": alertname,
		"service":   service,
		"namespace": "prod",
		"instance":  instance,
	})
	var endsAt *time.Time
	if hasEnd {
		e := end
		endsAt = &e
	}
	return alert.StoredAlert{
		ID: id,
		Alert: alert.Alert{
			Fingerprint: "fp-" + instance,
			Source:      alert.SourceAlertmanager,
			Severity:    alert.SeverityCritical,
			Status:      status,
			Labels:      labels,
			Annotations: json.RawMessage(`{}`),
			StartsAt:    start,
			EndsAt:      endsAt,
			RawPayload:  json.RawMessage(`{}`),
		},
	}
}

// newFiringOnCheckout 是「checkout 服务同规则、窗口一致」的 firing 告警。
func newFiringOnCheckout(id int64, instance string) alert.StoredAlert {
	return corrAlert(id, "checkout", "LatencyHigh", alert.StatusFiring, instance, corrStart, corrEnd, true)
}

// candidateOnCheckout 是已归属于某 Incident 的同类告警（评分口径：同服务同窗口同标签）。
func candidateOnCheckout(incidentID int64, instance string) CandidateAlert {
	// NewAlertView 收 *alert.StoredAlert（ADR-012）；函数返回值不可直接取址，
	// 故先落变量再取址。
	a := newFiringOnCheckout(900, instance)
	return CandidateAlert{
		IncidentID: incidentID,
		View:       NewAlertView(&a),
	}
}

// TestCorrelateAlerts_NewWhenNoCandidates：无候选事件时必须新建，且关联边不带分数。
func TestCorrelateAlerts_NewWhenNoCandidates(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil)

	in := newFiringOnCheckout(101, "pod-a")
	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{in}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if len(store.created) != 1 {
		t.Fatalf("应新建 1 个事件，实得 %d", len(store.created))
	}
	got := store.created[0]
	if got.Service != "checkout" {
		t.Errorf("事件 service = %q，期望 checkout", got.Service)
	}
	if got.Severity != alert.SeverityCritical {
		t.Errorf("事件 severity = %q，期望 critical", got.Severity)
	}
	if !got.StartedAt.Equal(corrStart) {
		t.Errorf("事件 started_at = %v，期望 %v", got.StartedAt, corrStart)
	}
	if got.Title == "" {
		t.Error("事件标题不应为空")
	}

	if len(store.links) != 1 {
		t.Fatalf("应建立 1 条关联边，实得 %d", len(store.links))
	}
	link := store.links[0]
	if link.IncidentID != got.ID || link.AlertID != 101 {
		t.Errorf("关联边 = (%d→%d)，期望 (%d→101)", link.AlertID, link.IncidentID, got.ID)
	}
	if link.Score != nil {
		t.Errorf("NEW 档的关联边不应带分数，实得 %v", *link.Score)
	}
	if link.NeedsReview {
		t.Error("NEW 档不应标记 needs_review")
	}
	if len(store.setCalls) != 1 || store.setCalls[0] != [2]int64{101, got.ID} {
		t.Errorf("应回写告警 incident_id 为 %d，实得 %v", got.ID, store.setCalls)
	}
}

// TestCorrelateAlerts_MergeOnHighScore：非历史维拉满时得分恰达 merge 阈值 0.80，应合并。
//
// R7 前后改写（2026-10-12）：旧版靠假存储注入 co=1/total=1 把历史维拉满凑 0.80，
// 但实时 webhook 路径查共现**恒得 0**（新告警尚未 link 到任何 incident）——
// 那个场景生产不可达。现改由拓扑共同邻居把依赖维拉到 0.5：
// 0.25(时间) + 0.25(服务) + 0.20(标签) + 0.5×0.20(依赖) = 0.80，恰达阈值。
// 「恰好达标」不是巧合：CalculateScore 的 6 位量化正是为这个边界修的
// （TestCalculateScoreThresholdBoundary），此处顺带在服务编排层回归它。
func TestCorrelateAlerts_MergeOnHighScore(t *testing.T) {
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}})
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, topo)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if len(store.created) != 0 {
		t.Fatalf("命中既有事件时不应新建，实得 %d 个", len(store.created))
	}
	if len(store.links) != 1 {
		t.Fatalf("应建立 1 条关联边，实得 %d", len(store.links))
	}
	link := store.links[0]
	if link.IncidentID != 7 {
		t.Errorf("应合并到事件 7，实得 %d", link.IncidentID)
	}
	if link.Score == nil || !almostEq(*link.Score, 0.80) {
		t.Errorf("关联分数 = %v，期望 0.80", link.Score)
	}
	if link.NeedsReview {
		t.Error("MERGE 档不应标记 needs_review")
	}
	if store.setCalls[0] != [2]int64{101, 7} {
		t.Errorf("应回写告警 incident_id 为 7，实得 %v", store.setCalls)
	}
}

// TestCorrelateAlerts_ReviewOnMidScore：无历史加分时得分 0.70（review ≤ s < merge），
// 应关联到既有事件但标记 needs_review 待人工确认。
func TestCorrelateAlerts_ReviewOnMidScore(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if len(store.created) != 0 {
		t.Fatalf("REVIEW 档不应新建事件，实得 %d 个", len(store.created))
	}
	if len(store.links) != 1 {
		t.Fatalf("应建立 1 条关联边，实得 %d", len(store.links))
	}
	link := store.links[0]
	if link.IncidentID != 7 {
		t.Errorf("应关联到事件 7，实得 %d", link.IncidentID)
	}
	if link.Score == nil || !almostEq(*link.Score, 0.70) {
		t.Errorf("关联分数 = %v，期望 0.70", link.Score)
	}
	if !link.NeedsReview {
		t.Error("REVIEW 档必须标记 needs_review")
	}
}

// TestCorrelateAlerts_SkipsResolved：resolved 告警不建也不并事件（生命周期归 M3 状态机）。
func TestCorrelateAlerts_SkipsResolved(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil)

	resolved := corrAlert(102, "checkout", "LatencyHigh", alert.StatusResolved, "pod-c", corrStart, corrEnd, true)
	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{resolved}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}
	if len(store.created) != 0 || len(store.links) != 0 || len(store.setCalls) != 0 {
		t.Fatalf("resolved 告警不应触发任何落库：created=%d links=%d set=%d",
			len(store.created), len(store.links), len(store.setCalls))
	}
}

// TestCorrelateAlerts_EmptyAlertsNoOp：空批次直接返回，连配置都不读。
func TestCorrelateAlerts_EmptyAlertsNoOp(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil)

	if err := svc.CorrelateAlerts(context.Background(), nil); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}
	if cfgStore.getCalls != 0 {
		t.Errorf("空批次不应读配置，实读 %d 次", cfgStore.getCalls)
	}
	if store.listCalls != 0 {
		t.Errorf("空批次不应查候选事件，实查 %d 次", store.listCalls)
	}
}

// TestCorrelateAlerts_Disabled：引擎关闭时读配置即返回，不查候选、不落库。
func TestCorrelateAlerts_Disabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false

	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	cfgStore := &fakeConfigStore{cfg: cfg}
	svc := newCorrService(store, cfgStore, nil)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}
	if store.listCalls != 0 {
		t.Errorf("引擎关闭时不应查候选事件，实查 %d 次", store.listCalls)
	}
	if len(store.created) != 0 || len(store.links) != 0 {
		t.Fatalf("引擎关闭时不应落库：created=%d links=%d", len(store.created), len(store.links))
	}
}

// TestCorrelateAlerts_BatchAccumulates：同批次内先新建的事件应立刻成为后续告警的候选，
// 否则同一批次的多条同源告警会各自建事件，风暴下产生大量孤立事件。
func TestCorrelateAlerts_BatchAccumulates(t *testing.T) {
	store := &fakeIncidentStore{}
	cfgStore := &fakeConfigStore{cfg: DefaultConfig()}
	svc := newCorrService(store, cfgStore, nil)

	batch := []alert.StoredAlert{
		newFiringOnCheckout(101, "pod-a"),
		newFiringOnCheckout(102, "pod-b"),
	}
	if err := svc.CorrelateAlerts(context.Background(), batch); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}

	if len(store.created) != 1 {
		t.Fatalf("同批次同源告警应只新建 1 个事件，实得 %d", len(store.created))
	}
	first := store.created[0].ID
	if len(store.links) != 2 {
		t.Fatalf("应建立 2 条关联边，实得 %d", len(store.links))
	}
	if store.links[0].IncidentID != first || store.links[0].Score != nil {
		t.Errorf("第一条应按 NEW 落到事件 %d 且不带分数，实得 %+v", first, store.links[0])
	}
	if store.links[1].IncidentID != first {
		t.Errorf("第二条应并入同批次新建的事件 %d，实得 %d", first, store.links[1].IncidentID)
	}
	if store.links[1].Score == nil || !almostEq(*store.links[1].Score, 0.70) || !store.links[1].NeedsReview {
		t.Errorf("第二条应为 REVIEW（0.70 + needs_review），实得 %+v", store.links[1])
	}
}

// TestCorrelateAlerts_HistoryNotQueriedInRealtimePath：实时 webhook 路径对历史
// 共现**一次查询都不发**。
//
// R7 背景：旧实现按「新指纹 × 每个候选指纹」逐对查共现（memo 仅批内有效，
// 1 万个不同指纹几乎不命中）。但刚落库的告警尚未 link 到任何 incident，
// 这些查询恒返回 co=0 —— 纯浪费，实测把吞吐从 1785 条/s 拖到 ~4.5 条/s。
// 新实现因此注入「无历史」闭包（与真实查询的结果完全一致：都是 0），
// coCalls 必须为 0。若将来有人把逐对查询加回实时路径，本条用例立刻变红。
//
// 同时锁定分数：历史按 0 计后，同源候选仍应按 0.70 判 REVIEW——
// 证明「不查历史」没有顺手改掉别的评分维（只断言查询次数的话，
// 把历史维整个删掉也能绿）。
func TestCorrelateAlerts_HistoryNotQueriedInRealtimePath(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	// co=1/total=1：若实现真的去查了历史，分数会偏离 0.70，本条用例抓住它。
	cfgStore := &fakeConfigStore{cfg: DefaultConfig(), co: 1, total: 1}
	svc := newCorrService(store, cfgStore, nil)

	if err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 返回意外错误: %v", err)
	}
	if cfgStore.coCalls != 0 {
		t.Errorf("实时路径不应查历史共现（实查 %d 次）——O(N²) 缺陷已回潮", cfgStore.coCalls)
	}
	if len(store.links) != 1 {
		t.Fatalf("应建立 1 条关联边，实得 %d", len(store.links))
	}
	if store.links[0].Score == nil || !almostEq(*store.links[0].Score, 0.70) {
		t.Errorf("历史按 0 计后分数应为 0.70，实得 %v", store.links[0].Score)
	}
}

// TestCorrelateAlerts_StorageFailuresPropagate：存储故障必须上抛（由 handler 记日志），
// 且错误信息要能定位到具体环节，便于排障。
func TestCorrelateAlerts_StorageFailuresPropagate(t *testing.T) {
	boom := errors.New("storage down")

	cases := []struct {
		name    string
		store   *fakeIncidentStore
		cfg     *fakeConfigStore
		wantSub string
	}{
		{"读配置失败", &fakeIncidentStore{}, &fakeConfigStore{cfg: DefaultConfig(), getErr: boom}, "读取关联配置失败"},
		{"读候选失败", &fakeIncidentStore{listErr: boom}, &fakeConfigStore{cfg: DefaultConfig()}, "读取候选事件失败"},
		{"新建事件失败", &fakeIncidentStore{createErr: boom}, &fakeConfigStore{cfg: DefaultConfig()}, "新建事件失败"},
		{"建关联边失败", &fakeIncidentStore{linkErr: boom}, &fakeConfigStore{cfg: DefaultConfig()}, "关联告警"},
		{"回写 incident_id 失败", &fakeIncidentStore{setErr: boom}, &fakeConfigStore{cfg: DefaultConfig()}, "回写告警"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newCorrService(tc.store, tc.cfg, nil)
			err := svc.CorrelateAlerts(context.Background(), []alert.StoredAlert{newFiringOnCheckout(101, "pod-new")})
			if err == nil {
				t.Fatal("期望返回错误，实得 nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestIncidentTitle 锁定事件标题的派生规则（可读性直接影响值班人员的辨识效率）。
func TestIncidentTitle(t *testing.T) {
	cases := []struct {
		name string
		view AlertView
		want string
	}{
		{
			"有service",
			AlertView{Service: "checkout", Labels: map[string]string{"alertname": "LatencyHigh"}},
			"LatencyHigh (checkout)",
		},
		{
			"仅namespace",
			AlertView{Namespace: "prod", Labels: map[string]string{"alertname": "LatencyHigh"}},
			"LatencyHigh (prod)",
		},
		{
			"退到指纹并截断到12位",
			AlertView{Fingerprint: "0123456789abcdef", Labels: map[string]string{"alertname": "LatencyHigh"}},
			"LatencyHigh #0123456789ab",
		},
		{
			"无任何标识",
			AlertView{Labels: map[string]string{"alertname": "LatencyHigh"}},
			"LatencyHigh",
		},
		{
			"缺alertname兜底",
			AlertView{Service: "checkout", Labels: map[string]string{}},
			"未命名告警 (checkout)",
		},
		{
			"labels为nil",
			AlertView{},
			"未命名告警",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := incidentTitle(tc.view); got != tc.want {
				t.Fatalf("incidentTitle = %q，期望 %q", got, tc.want)
			}
		})
	}
}
