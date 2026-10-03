package correlation

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/incident"
)

// 本文件覆盖 M3 挂到关联路径上的两项能力（手册 §4.2 / §4.5）：
//   1. 时间线补写：NEW 写 alert_fired，MERGE / REVIEW 写 alert_linked；
//   2. REVIEW 抑制：命中则降级为 NEW 档。
// 并重点守住「best-effort」这一性质：这两项失败都**不能**影响关联结果，
// 否则一次留痕抖动就会让 webhook 失败、触发 Alertmanager 重投。

// fakeTimeline 是 TimelineWriter 的内存替身。
type fakeTimeline struct {
	entries []incident.Entry
	err     error
}

func (f *fakeTimeline) AppendTimelineEntry(_ context.Context, e incident.Entry) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

// suppressionCall 记录一次抑制写入。
type suppressionCall struct {
	Fingerprint string
	IncidentID  int64
	Actor       string
	Reason      string
}

// fakeSuppressions 是 SuppressionStore 的内存替身。
type fakeSuppressions struct {
	hit      bool     // IsSuppressed 的返回值
	queryErr error    // IsSuppressed 的故障注入
	queries  []string // 形如 "fp#7"
	added    []suppressionCall
	addErr   error
}

func (f *fakeSuppressions) IsSuppressed(_ context.Context, fingerprint string, incidentID int64) (bool, error) {
	f.queries = append(f.queries, fingerprint+"#"+strconv.FormatInt(incidentID, 10))
	if f.queryErr != nil {
		return false, f.queryErr
	}
	return f.hit, nil
}

func (f *fakeSuppressions) AddSuppression(_ context.Context, fingerprint string, incidentID int64, actor, reason string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, suppressionCall{Fingerprint: fingerprint, IncidentID: incidentID, Actor: actor, Reason: reason})
	return nil
}

var (
	_ TimelineWriter   = (*fakeTimeline)(nil)
	_ SuppressionStore = (*fakeSuppressions)(nil)
)

// fixedClock 是注入用的固定时钟，让留痕时间可被精确断言。
var fixedClock = time.Date(2026, 10, 2, 21, 0, 0, 0, time.UTC)

// newLifecycleService 组装一个带固定时钟的关联服务。
func newLifecycleService(store *fakeIncidentStore, cfgStore *fakeConfigStore, tl *fakeTimeline, sup *fakeSuppressions) *Service {
	svc := newCorrService(store, cfgStore, nil)
	svc.now = func() time.Time { return fixedClock }
	if tl != nil {
		svc.WithTimeline(tl)
	}
	if sup != nil {
		svc.WithSuppressions(sup)
	}
	return svc
}

// TestTimelineWrittenOnNew 断言 NEW 档写的是「事件创建」而非「告警合入」。
func TestTimelineWrittenOnNew(t *testing.T) {
	store := &fakeIncidentStore{}
	tl := &fakeTimeline{}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig()}, tl, nil)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 失败: %v", err)
	}
	if len(tl.entries) != 1 {
		t.Fatalf("应写 1 条留痕，实得 %d", len(tl.entries))
	}
	e := tl.entries[0]
	if e.EventType != incident.EventAlertFired {
		t.Errorf("事件类型 = %q，期望 %q", e.EventType, incident.EventAlertFired)
	}
	if e.Actor != incident.SystemActor {
		t.Errorf("actor = %q，期望 %q", e.Actor, incident.SystemActor)
	}
	if e.IncidentID != 1 {
		t.Errorf("incident_id = %d，期望 1（fake 自增的第一个 ID）", e.IncidentID)
	}
	if e.Metadata["alert_id"] != "101" {
		t.Errorf("metadata.alert_id = %q，期望 101", e.Metadata["alert_id"])
	}
	if e.Metadata["fingerprint"] == "" {
		t.Error("metadata 应带 fingerprint（供判断同一指纹再次起火）")
	}
	if !e.Timestamp.Equal(fixedClock) {
		t.Errorf("留痕时间 = %v，期望注入的时钟 %v", e.Timestamp, fixedClock)
	}
}

// TestTimelineWrittenOnMergeAndReview 断言两种建边场景各写一条 alert_linked，
// 且 needs_review 与 score 如实落进 metadata。
func TestTimelineWrittenOnMergeAndReview(t *testing.T) {
	cases := []struct {
		name       string
		co, total  float64
		wantScore  string
		wantReview string
	}{
		{"MERGE 档（有历史加分，0.80）", 1, 1, "0.8000", "false"},
		{"REVIEW 档（无历史加分，0.70）", 0, 0, "0.7000", "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
			tl := &fakeTimeline{}
			svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig(), co: tc.co, total: tc.total}, tl, nil)

			if err := svc.CorrelateAlerts(context.Background(),
				[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
				t.Fatalf("CorrelateAlerts 失败: %v", err)
			}
			if len(tl.entries) != 1 {
				t.Fatalf("应写 1 条留痕，实得 %d", len(tl.entries))
			}
			e := tl.entries[0]
			if e.EventType != incident.EventAlertLinked {
				t.Errorf("事件类型 = %q，期望 %q", e.EventType, incident.EventAlertLinked)
			}
			if e.IncidentID != 7 {
				t.Errorf("incident_id = %d，期望 7", e.IncidentID)
			}
			if e.Metadata["score"] != tc.wantScore {
				t.Errorf("metadata.score = %q，期望 %q", e.Metadata["score"], tc.wantScore)
			}
			if e.Metadata["needs_review"] != tc.wantReview {
				t.Errorf("metadata.needs_review = %q，期望 %q", e.Metadata["needs_review"], tc.wantReview)
			}
		})
	}
}

// TestTimelineFailureIsBestEffort 是这条性质的核心证明：
// 留痕写失败时，关联结果必须照常产出、CorrelateAlerts 必须返回 nil。
func TestTimelineFailureIsBestEffort(t *testing.T) {
	store := &fakeIncidentStore{}
	tl := &fakeTimeline{err: errors.New("timeline 不可用")}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig()}, tl, nil)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("留痕失败不应让关联失败，实际返回: %v", err)
	}
	// 主链路（建事件 + 建边 + 回写指针）必须照常完成。
	if len(store.created) != 1 || len(store.links) != 1 {
		t.Errorf("留痕失败不应影响主链路：created=%d links=%d，期望 1/1",
			len(store.created), len(store.links))
	}
}

// TestNoTimelineWriterKeepsM2Behavior 断言不挂 timeline 时行为与 M2 完全一致。
func TestNoTimelineWriterKeepsM2Behavior(t *testing.T) {
	store := &fakeIncidentStore{}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil, nil)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 失败: %v", err)
	}
	if len(store.created) != 1 {
		t.Errorf("未挂 timeline 时仍应正常建事件，实得 %d", len(store.created))
	}
}

// TestSuppressionDowngradesReviewToNew 断言抑制命中时不再往原事件建边，改为另起事件。
func TestSuppressionDowngradesReviewToNew(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	sup := &fakeSuppressions{hit: true}
	tl := &fakeTimeline{}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig()}, tl, sup)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 失败: %v", err)
	}
	// 抑制命中 → 不建边到事件 7，而是新建事件。
	if len(store.created) != 1 {
		t.Fatalf("抑制命中后应新建事件，实得 %d", len(store.created))
	}
	if len(store.links) != 1 || store.links[0].IncidentID == 7 {
		t.Errorf("不应再往被抑制的事件 7 建边，实得 %+v", store.links)
	}
	if len(sup.queries) != 1 || sup.queries[0] != "fp-pod-new#7" {
		t.Errorf("抑制查询键 = %v，期望按 (fingerprint, incident_id) 查询", sup.queries)
	}
	// 降级后写的是 alert_fired（新建事件），不是 alert_linked。
	if len(tl.entries) != 1 || tl.entries[0].EventType != incident.EventAlertFired {
		t.Errorf("降级为 NEW 档应写 alert_fired，实得 %+v", tl.entries)
	}
}

// TestSuppressionOnlyAppliesToReview 断言 MERGE 档**不查**抑制：
// 高分直接合并是引擎的确定性结论，不该被一次历史的人工判定推翻。
func TestSuppressionOnlyAppliesToReview(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	sup := &fakeSuppressions{hit: true}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig(), co: 1, total: 1}, nil, sup)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("CorrelateAlerts 失败: %v", err)
	}
	if len(sup.queries) != 0 {
		t.Errorf("MERGE 档不应查询抑制，实得 %v", sup.queries)
	}
	if len(store.created) != 0 {
		t.Errorf("MERGE 档应照常合并，不应新建事件")
	}
}

// TestSuppressionQueryFailureTreatedAsNotSuppressed 断言查询故障按未抑制处理。
func TestSuppressionQueryFailureTreatedAsNotSuppressed(t *testing.T) {
	store := &fakeIncidentStore{candidates: []CandidateAlert{candidateOnCheckout(7, "pod-old")}}
	sup := &fakeSuppressions{queryErr: errors.New("抑制表不可用")}
	svc := newLifecycleService(store, &fakeConfigStore{cfg: DefaultConfig()}, nil, sup)

	if err := svc.CorrelateAlerts(context.Background(),
		[]alert.StoredAlert{newFiringOnCheckout(101, "pod-new")}); err != nil {
		t.Fatalf("抑制查询失败不应让关联失败，实际返回: %v", err)
	}
	if len(store.created) != 0 {
		t.Error("抑制查询失败应按未抑制处理（仍走 REVIEW 档关联），不该新建事件")
	}
	if len(store.links) != 1 || !store.links[0].NeedsReview {
		t.Errorf("应按 REVIEW 档建边并标记待确认，实得 %+v", store.links)
	}
}
