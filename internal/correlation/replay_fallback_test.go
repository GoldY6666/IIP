package correlation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/metrics"
)

// 本文件是 R7 残留边界**第二半**的收口判据：最佳匹配是**非种子老成员**时的漏合并。
//
// 与 replay_realtime_test.go 的分工：那条证明「限额 + 种子锚定不改变标准夹具的
// 分组」，但只覆盖「最佳匹配是种子」这一半；本文件构造另一半——新告警的最佳
// 匹配是被限额裁出最近 20 条的**非种子**成员，限额集内最高分跌破 merge 阈值，
// 自动合并降级为 REVIEW（更极端构造下漏成 NEW 重复事件）。
//
// 修复（2026-10-19，见 service.go 的 correlateOne 与 candidates.go）：引擎两段式
// 回退——限额集内最高分 < merge 阈值**且**存在事件被限额截断（候选条数 ==
// limit+1，截断的精确信号）时，用不限额候选集重算，仅在全量严格更优时取代。
// 本用例验四件事：
//
//	① 回退恰好触发 1 次——只有 N 的到达同时满足两个合取项；
//	② N 以 0.80 **非待确认**并入事件 1，而不是 REVIEW 0.70；
//	③ 分组与不限额参考跑一致——回退恢复了 R7 修复前的语义；
//	④ 阴性对照以数据表达：限额集内最高分确实 < merge——不修就是缺陷。
//
// 场景（逐条走真实 CorrelateAlerts，时间窗全同，拓扑 payment→gateway；
// 得分依据 2026-10-12 实测值，勿凭记忆改）：
//
//	O1      payment/LatencyHigh/prod      无候选，新建事件 1（种子）
//	O2      gateway/LatencyHigh/prod      与 O1 得 0.70，REVIEW 并入事件 1
//	R1..R25 gateway/ErrorsHigh/staging    与 O2 得 0.67 REVIEW 入伙，把 O2 挤出最近 20 条
//	N       gateway/LatencyHigh/prod      O2 的同源克隆：限额集内最高 0.70（O1）/0.67（R），
//	                                      不限额集内与 O2 得 0.80
func TestCandidateLimitFallbackRecoversOldNonSeedMember(t *testing.T) {
	ctx := context.Background()
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"payment": {"gateway"}})

	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	end := base.Add(10 * time.Minute)
	mk := func(id int, service, alertname, namespace string) alert.StoredAlert {
		labels, _ := json.Marshal(map[string]string{
			"alertname": alertname,
			"service":   service,
			"namespace": namespace,
			"tier":      "web",
		})
		return alert.StoredAlert{
			ID: int64(id),
			Alert: alert.Alert{
				Fingerprint: fmt.Sprintf("%s/%s/%s#%d", namespace, service, alertname, id),
				Source:      alert.SourceAlertmanager,
				Severity:    alert.SeverityCritical,
				Status:      alert.StatusFiring,
				Labels:      labels,
				Annotations: json.RawMessage(`{}`),
				StartsAt:    base,
				EndsAt:      &end,
				RawPayload:  json.RawMessage(`{}`),
			},
		}
	}

	// 到达序：O1、O2 打底，R1..R25 把事件撑过 20 条限额，N 最后到达。
	// R 用 staging 命名空间是构造的关键：与 O2（同 service）得 0.67 能 REVIEW
	// 入伙，同时把 N 对 R 的得分压住；N 是 O2 的同源克隆，与 O2 得 0.80。
	seq := []alert.StoredAlert{
		mk(1, "payment", "LatencyHigh", "prod"),
		mk(2, "gateway", "LatencyHigh", "prod"),
	}
	for i := 0; i < 25; i++ {
		seq = append(seq, mk(3+i, "gateway", "ErrorsHigh", "staging"))
	}
	seq = append(seq, mk(28, "gateway", "LatencyHigh", "prod"))

	run := func(mode limitMode) (*realtimeStore, *metrics.Metrics) {
		store := newRealtimeStore(mode)
		svc := NewService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo, discardLogger())
		m := metrics.New()
		svc.WithMetrics(m)
		for i := range seq {
			store.views[seq[i].ID] = NewAlertView(&seq[i])
			if err := svc.CorrelateAlerts(ctx, seq[i:i+1]); err != nil {
				t.Fatalf("第 %d 条告警关联失败: %v", i, err)
			}
		}
		return store, m
	}

	store, m := run(limitWithSeed)
	refStore, _ := run(limitUnlimited)

	// 前置断言 1：28 条必须全部落进事件，否则分组比对失去意义。
	if n := store.totalMembers(); n != len(seq) {
		t.Fatalf("假存储只累积了 %d 条成员，期望 %d", n, len(seq))
	}
	// 前置断言 2：限额必须真的裁过成员（O2 被挤出最近 20 条），
	// 否则本条用例对「限额漏合并」零鉴别力。
	if store.truncated == 0 {
		t.Fatalf("限额未截断任何成员——探针失去鉴别力")
	}

	// ① 回退恰好触发一次：O2/R 的到达时事件未超限（或集内已是 MERGE 档——
	// 同类 R 互相 0.80），只有 N 同时满足「集内 < merge」与「事件被截断」。
	if got := testutil.ToFloat64(m.CorrelationCandidateFallback); got != 1 {
		t.Fatalf("候选限额回退应恰好触发 1 次，实得 %v——触发条件过宽还是过窄？", got)
	}
	// ② N 以 ≥0.80 非待确认并入（自动合并），而不是 REVIEW 0.70。
	rec, ok := store.linkOf(28)
	if !ok {
		t.Fatalf("N（告警 28）没有关联记录——它既没合并也没建事件？")
	}
	if rec.needsReview || rec.score == nil || *rec.score < DefaultConfig().Thresholds.Merge {
		t.Fatalf("N 应以 ≥%v 自动合并，实得 score=%v needsReview=%v",
			DefaultConfig().Thresholds.Merge, rec.score, rec.needsReview)
	}
	// ③ 分组与不限额参考跑一致（回退恢复了无限额语义）。
	if !reflect.DeepEqual(store.partition(), refStore.partition()) {
		t.Fatalf("回退后的分组应与不限额一致：\n回退 %v\n不限 %v",
			store.partition(), refStore.partition())
	}
	// ④ 阴性对照（数据形式）：限额集内最高分确实 < merge 阈值——
	// 证明不修就是缺陷（0.70 会走 REVIEW），而不是场景根本没踩到边界。
	// membersBefore = 27：N 到达时事件 1 的成员数（O1+O2+R1..R25）。
	// 不能跑完再取 members——那会把 N 自己算进去（它与自己全维同分 0.80），
	// 阴性对照就成了同义反复。生产无此问题：候选集在 link 之前取。
	bounded := boundedMembers(store, 1, len(seq)-1)
	scorer := NewScorer(DefaultConfig(), topo, func(_, _ string) float64 { return 0 }, base)
	if sc, _ := bestMatch(map[int64][]AlertView{1: bounded}, scorer, NewAlertView(&seq[27])); sc >= DefaultConfig().Thresholds.Merge {
		t.Fatalf("限额集内最高分 %v 已 ≥ merge——场景不再踩边界，探针失去鉴别力", sc)
	}
}

// boundedMembers 复算 SQL 的限额语义：目标告警**到达时**该事件的「最近 N 条 + 种子」。
// 供阴性对照使用——证明限额集里确实找不到更优匹配（病灶在档位上）。
//
// membersBefore 是目标告警并入前事件的成员数：跑完再取 members 会把目标
// 自己也算进去（它与自己全维同分，恰好 0.80），阴性对照就成了同义反复。
func boundedMembers(r *realtimeStore, incidentID int64, membersBefore int) []AlertView {
	members := r.members[incidentID]
	if membersBefore >= 0 && membersBefore < len(members) {
		members = members[:membersBefore]
	}
	if len(members) <= int(CandidateLimitPerIncident) {
		out := make([]AlertView, 0, len(members))
		for _, c := range members {
			out = append(out, c.View)
		}
		return out
	}
	recent := members[len(members)-int(CandidateLimitPerIncident):]
	seed := members[0]
	if recent[0].View.ID != seed.View.ID {
		recent = append([]CandidateAlert{seed}, recent...)
	}
	out := make([]AlertView, 0, len(recent))
	for _, c := range recent {
		out = append(out, c.View)
	}
	return out
}
