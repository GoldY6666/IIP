package correlation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/yu/iip/internal/alert"
)

// 本文件回答 R7 的一个特定问题：**候选限额（每 incident 最近 20 条）会不会
// 改变实时路径的合并结局**。
//
// 为什么 replay_test.go 回答不了：回放走 ClusterInMemory（内存、全量可见），
// 限额只在 DB 实时路径（CorrelateAlerts → ListOpenIncidentAlerts）生效。
// 两个路径共享评分内核，但候选集可见性不同——必须让夹具走一遍实时路径才能度量。
// 限额的 SQL 语义由真库用例 TestListOpenIncidentAlertsPerIncidentLimit 负责；
// 本文件负责**组合语义**：限额后的候选集喂给编排层，分组结局是否不变。
//
// 方法：把回放夹具逐条灌入 service（每批 1 条，模拟 webhook 逐条投递），
// 跑两遍——一遍假存储**执行限额 + 种子锚定**（等价现行 SQL），一遍**忽略**
// 限额（等价 R7 之前的旧 SQL）——比较两次的事件分组：
//
//	① 两遍分组必须一致 ⇒ 限额没有改变任何一条告警的归属；
//	② 不限额那一遍的分组必须与夹具期望一致 ⇒ 实时路径与回放内核同构
//	   （防两个路径的评分/决策悄悄漂移，那会让「限额无害」的结论失去基准）；
//	③ 前置断言：风暴场景里限额必须真的裁过成员，且假存储必须累积全部告警
//	   ——否则本条用例对「限额漏合并」零鉴别力（空==空也会绿）。
//
// 确定性依据：三个夹具的每条告警都带 ends_at，时间窗固定 ⇒ TimeSimilarity
// 不依赖 service 里的 wall-clock ref，两遍与回放三方得分完全一致。
//
// 鉴别力证据见同文件 TestCandidateLimitBoundaryOnHeterogeneousIncident：
// 它构造了一个限额**确实**改变合并结局的场景，证明本文件的 A/B 机制不是
// 「结构上恒绿」——风暴夹具组内同质（仅 instance 动态标签不同，被评分排除），
// 限额对它必然等价；探针补上「异质大事件」这一盲区。

// limitMode 是候选集可见性的三种形态，对应 SQL 的三个版本：
//
//	limitUnlimited —— 不限额（R7 之前的旧 SQL：全量成员可见）；
//	limitWithSeed  —— 最近 N 条 + 种子锚定（现行 SQL，2026-10-12 收口补）；
//	limitNaive     —— 只取最近 N 条、不锚种子（已被取代的中间态，
//	                   保留它作为探针的阴性对照：证明「限额会漏合并」
//	                   这件事在当前用例集里确实可被观测）。
type limitMode int

const (
	limitUnlimited limitMode = iota
	limitWithSeed
	limitNaive
)

// realtimeStore 是带成员累积与限额仿真的 IncidentStore 假实现。
//
// 与 service_test.go 的 fakeIncidentStore 的区别：那个返回**预置**候选，
// 这个按 service 的真实调用序**动态累积**成员，并按 mode 参数仿真 SQL 的
// 候选集语义——members 按 link 顺序追加（= alert id 升序），
// 与 SQL 的 `ORDER BY a.id DESC` 取尾部一致；种子 = members[0]（首条）。
//
// links 记录每条关联边的（事件, 分数, 是否待确认）：探针要断言的不是
// 「分了几组」而是「N 以什么档位并入」——限额漏合并的病灶正在档位上
// （自动合并降级成 REVIEW），只记分组看不出来。
type realtimeStore struct {
	nextID  int64
	views   map[int64]AlertView
	members map[int64][]CandidateAlert
	links   map[int64]linkRecord

	mode      limitMode
	listCalls int
	truncated int // 被限额裁掉的成员总数（鉴别力前置断言用）
}

// linkRecord 是一条关联边的留痕，字段与 LinkAlertToIncident 的入参一致。
type linkRecord struct {
	incidentID  int64
	score       *float64
	needsReview bool
}

func newRealtimeStore(mode limitMode) *realtimeStore {
	return &realtimeStore{
		views:   map[int64]AlertView{},
		members: map[int64][]CandidateAlert{},
		links:   map[int64]linkRecord{},
		mode:    mode,
	}
}

func (r *realtimeStore) CreateIncident(_ context.Context, in Incident) (Incident, error) {
	r.nextID++
	in.ID = r.nextID
	if in.Status == "" {
		in.Status = "open"
	}
	return in, nil
}

func (r *realtimeStore) ListOpenIncidentAlerts(_ context.Context, perIncidentLimit int32) ([]CandidateAlert, error) {
	r.listCalls++
	ids := make([]int64, 0, len(r.members))
	for id := range r.members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]CandidateAlert, 0, len(ids))
	for _, id := range ids {
		ms := r.members[id]
		if r.mode != limitUnlimited && perIncidentLimit > 0 && len(ms) > int(perIncidentLimit) {
			recent := ms[len(ms)-int(perIncidentLimit):] // 最近 N 条 = 尾部
			if r.mode == limitWithSeed {
				// 种子锚定：种子（首条）不在 recent 里才补，成本 +1 条/事件。
				seed := ms[0]
				if recent[0].View.ID != seed.View.ID {
					recent = append([]CandidateAlert{seed}, recent...)
				}
				r.truncated += len(ms) - int(perIncidentLimit) - 1 // 种子保留，少裁 1 条
			} else {
				r.truncated += len(ms) - int(perIncidentLimit)
			}
			ms = recent
		}
		out = append(out, ms...)
	}
	return out, nil
}

func (r *realtimeStore) LinkAlertToIncident(_ context.Context, incidentID, alertID int64, alertStartsAt time.Time, score *float64, needsReview bool) error {
	v, ok := r.views[alertID]
	if !ok {
		return fmt.Errorf("假存储未登记告警 %d 的视图", alertID)
	}
	// 分区键一致性：调用方传的 starts_at 必须与视图里的相等。
	// 复合 FK (alert_id, alert_starts_at) 要求二者精确匹配，
	// 这里替外键先拦一道「接线时传错月/传错行」的错误——
	// 否则要到真库才以 23503 暴露，而回放用例本可以零成本先发现。
	if !alertStartsAt.Equal(v.StartsAt) {
		return fmt.Errorf("告警 %d 的 starts_at 不一致: 传入 %s, 视图 %s", alertID, alertStartsAt, v.StartsAt)
	}
	r.members[incidentID] = append(r.members[incidentID], CandidateAlert{IncidentID: incidentID, View: v})
	r.links[alertID] = linkRecord{incidentID: incidentID, score: score, needsReview: needsReview}
	return nil
}

// linkOf 返回某告警的关联边留痕（供探针断言并入档位与分数）。
func (r *realtimeStore) linkOf(alertID int64) (linkRecord, bool) {
	rec, ok := r.links[alertID]
	return rec, ok
}

func (r *realtimeStore) SetAlertIncident(_ context.Context, _, _ int64) error { return nil }

// totalMembers 返回已累积的成员总数（供前置断言核对「一条都没丢」）。
func (r *realtimeStore) totalMembers() int {
	n := 0
	for _, ms := range r.members {
		n += len(ms)
	}
	return n
}

// partition 把「告警 → 事件」归属整理为 0 基下标分组（组内升序、组间按首元素
// 升序），口径与 normalizeGroups 一致，便于与夹具期望直接比对。
func (r *realtimeStore) partition() [][]int {
	byIncident := map[int64][]int{}
	for _, ms := range r.members {
		for _, c := range ms {
			byIncident[c.IncidentID] = append(byIncident[c.IncidentID], int(c.View.ID)-1)
		}
	}
	groups := make([][]int, 0, len(byIncident))
	for _, idx := range byIncident {
		groups = append(groups, idx)
	}
	return normalizeGroups(toGroups(groups))
}

// replayAlertToStored 把夹具告警转为已落库告警（firing）。
// 指纹公式与 toViews 逐字一致，保证两个路径面对「同一份数据」的同一视图。
func replayAlertToStored(a replayAlert, idx int) alert.StoredAlert {
	labels := map[string]string{}
	for k, v := range a.Labels {
		labels[k] = v
	}
	if _, ok := labels["alertname"]; !ok && a.Alertname != "" {
		labels["alertname"] = a.Alertname
	}
	if _, ok := labels["service"]; !ok && a.Service != "" {
		labels["service"] = a.Service
	}
	if _, ok := labels["namespace"]; !ok && a.Namespace != "" {
		labels["namespace"] = a.Namespace
	}
	// map[string]string 的 Marshal 不存在失败路径（同 corrAlert 的处理）。
	raw, _ := json.Marshal(labels)
	return alert.StoredAlert{
		ID: int64(idx + 1),
		Alert: alert.Alert{
			Fingerprint: fmt.Sprintf("%s/%s/%s#%d", a.Namespace, a.Service, a.Alertname, idx),
			Source:      alert.SourceAlertmanager,
			Severity:    a.Severity,
			Status:      alert.StatusFiring,
			Labels:      raw,
			Annotations: json.RawMessage(`{}`),
			StartsAt:    a.StartsAt,
			EndsAt:      a.EndsAt,
			RawPayload:  json.RawMessage(`{}`),
		},
	}
}

// TestReplayFixturesThroughRealtimePathUnderCandidateLimit 是 R7 收口判据：
// 候选限额不改变实时路径的合并结局，且实时路径与回放内核同构。
func TestReplayFixturesThroughRealtimePathUnderCandidateLimit(t *testing.T) {
	ctx := context.Background()

	for _, name := range replayScenarios {
		t.Run(name, func(t *testing.T) {
			sc := loadReplayScenario(t, name)
			topo := &Topology{}
			topo.AddEdges(sc.Topology)

			stored := make([]alert.StoredAlert, len(sc.Alerts))
			for i, a := range sc.Alerts {
				stored[i] = replayAlertToStored(a, i)
			}

			// runRealtime 逐条灌入 service（每批 1 条 = webhook 逐条投递），
			// 返回归一化分组与假存储（供前置断言）。
			runRealtime := func(mode limitMode) ([][]int, *realtimeStore) {
				store := newRealtimeStore(mode)
				// 直接走 NewService（IncidentStore 接口）而非 newCorrService
				// （后者形参是 *fakeIncidentStore 具体类型）；realtimeStore
				// 满足同一接口，行为与 webhook 真实调用序一致。
				svc := NewService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo, discardLogger())
				for i := range stored {
					store.views[stored[i].ID] = NewAlertView(&stored[i])
					if err := svc.CorrelateAlerts(ctx, stored[i:i+1]); err != nil {
						t.Fatalf("第 %d 条告警关联失败: %v", i, err)
					}
				}
				return store.partition(), store
			}

			gotAnchored, anchoredStore := runRealtime(limitWithSeed)
			gotUnlimited, _ := runRealtime(limitUnlimited)

			// 前置断言 1：假存储必须一条不丢，否则分组比对空==空也绿。
			if n := anchoredStore.totalMembers(); n != len(stored) {
				t.Fatalf("假存储只累积了 %d 条成员，期望 %d——分组比对将失去意义", n, len(stored))
			}
			// 前置断言 2：风暴场景里限额必须真的裁过成员（truncated>0），
			// 否则「限额 vs 不限额」两跑完全等价，本条用例对限额零鉴别力。
			if name == "scenario1_service_storm" && anchoredStore.truncated == 0 {
				t.Fatalf("风暴场景限额未截断任何成员（truncated=0）——用例对「限额漏合并」没有鉴别力")
			}

			// ① 限额 + 种子锚定没有改变任何告警的归属（R7 核心问题）。
			if !reflect.DeepEqual(gotAnchored, gotUnlimited) {
				t.Fatalf("候选限额改变了实时路径的分组：\n限额 %v\n不限 %v", gotAnchored, gotUnlimited)
			}
			// ② 实时路径（不限额）与回放内核的期望一致。
			want := normalizeGroups(toGroups(sc.ExpectedGroups))
			if !reflect.DeepEqual(gotUnlimited, want) {
				t.Fatalf("实时路径分组与回放期望不一致：\n实得 %v\n期望 %v", gotUnlimited, want)
			}
			t.Logf("%s：%d 条 → %d 组；限额截断成员 %d 次（listCalls=%d）",
				name, len(stored), len(gotAnchored), anchoredStore.truncated, anchoredStore.listCalls)
		})
	}
}

// TestCandidateLimitBoundaryOnHeterogeneousIncident 是 R7 候选限额的**边界探针**。
// 与上面的夹具 A/B 用例分工不同：那条证明「标准场景下限额无害」，本条在**异质场景**
// （新告警的最佳匹配是 20 条以前的老成员——限额唯一可能改结局的地方）里同时验证
// 两件事，否则上面那条的绿色分不清是「真的无害」还是「结构上不可能有害」。
//
// 场景（全部告警逐条走真实 CorrelateAlerts 路径，时间窗完全相同，拓扑 payment↔gateway）：
//
//	O1  payment/LatencyHigh/prod     到达 → 无候选，新建事件 1
//	O2  gateway/LatencyHigh/prod     到达 → 与 O1 得 0.70（同 ns 不同 service 0.6、
//	                                  标签 0.6、依赖边 0.9），REVIEW 并入事件 1
//	R1..R25  gateway/ErrorsHigh/staging  到达 → 与 O2 得 0.67、与同类 R 得 0.80，
//	                                  全部并入事件 1（事件 1 累计 27 名成员）
//	N   payment/LatencyHigh/prod（与 O1 评分完全等价）到达 →
//	    不限额（旧 SQL）       ：候选含 O1，最高 0.80 → MERGE，28 条 1 组
//	    限额 + 种子锚定（现行）：O1 是种子恒在候选集 ⇒ 同样 0.80 MERGE，1 组
//	    只限额不锚种子（旧案）：O1/O2 被裁（只剩 R6..R25），最高 0.4586 < 0.50
//	                          ⇒ NEW 另起事件 2，2 组
//
// 第三跑是**阴性对照**：它证明这个场景确实踩在限额边界上、且 A/B 机制能观测到
// 分歧——没有它，「锚定后 1 组」可能只是场景根本没触发限额（空==空也会绿）。
//
// 构造要点（2026-10-12 实测校正，勿凭记忆改）：ServiceSimilarity 同命名空间不同
// service 是 0.6 而非 0——R 必须换命名空间（staging）才能把 N 的得分压到 0.5 以下；
// 而 R 换命名空间后与 O2（同 service gateway）仍有 0.67，足够 REVIEW 入伙。
//
// 种子锚定后的**残留边界第二半**——最佳匹配是**非种子**老成员时的漏合并——
// 已由两段式回退收口（2026-10-19）：限额集内最高分 < merge 阈值且存在事件
// 被限额截断（候选条数 == limit+1，证明见 candidates.go）时，引擎用不限额
// 候选集重算，仅在全量严格更优时取代。构造与判据见同包
// TestCandidateLimitFallbackRecoversOldNonSeedMember。
// 修复后仍如实保留的残留：限额集内最高分已 ≥ merge 时，合并目标取限额集
// 最佳而非全局最佳（多个事件同处 MERGE 档时）——全闭合等于永不限额，
// 吞吐崩塌，不做。标准回放夹具组内同质不受影响；压测中仅 2 个 incident
// 超 20 条且同为同质风暴，未观测到质量变化。
func TestCandidateLimitBoundaryOnHeterogeneousIncident(t *testing.T) {
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
	// R 用 staging 命名空间是探针的关键：把 N 对 R 的得分压到 0.5 以下，
	// 同时 R 对 O2（同 service）仍有 0.67 能 REVIEW 入伙——两头都够。
	seq := []alert.StoredAlert{
		mk(1, "payment", "LatencyHigh", "prod"),
		mk(2, "gateway", "LatencyHigh", "prod"),
	}
	for i := 0; i < 25; i++ {
		seq = append(seq, mk(3+i, "gateway", "ErrorsHigh", "staging"))
	}
	seq = append(seq, mk(28, "payment", "LatencyHigh", "prod"))

	run := func(mode limitMode) ([][]int, *realtimeStore) {
		store := newRealtimeStore(mode)
		svc := NewService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo, discardLogger())
		for i := range seq {
			store.views[seq[i].ID] = NewAlertView(&seq[i])
			if err := svc.CorrelateAlerts(ctx, seq[i:i+1]); err != nil {
				t.Fatalf("第 %d 条告警关联失败: %v", i, err)
			}
		}
		return store.partition(), store
	}

	gotUnlimited, _ := run(limitUnlimited)
	gotAnchored, anchoredStore := run(limitWithSeed)
	gotNaive, naiveStore := run(limitNaive)

	// 前置断言 1：28 条必须全部落进事件，否则分组比对失去意义。
	if n := anchoredStore.totalMembers(); n != len(seq) {
		t.Fatalf("假存储只累积了 %d 条成员，期望 %d", n, len(seq))
	}
	// 前置断言 2：限额必须真的裁过成员，否则探针零鉴别力。
	if anchoredStore.truncated == 0 || naiveStore.truncated == 0 {
		t.Fatalf("限额未截断任何成员（anchored=%d naive=%d）——探针失去鉴别力",
			anchoredStore.truncated, naiveStore.truncated)
	}

	// ① 修复生效：限额 + 种子锚定与不限额分组一致（N 经种子 O1 以 0.80 并入）。
	if len(gotAnchored) != 1 {
		t.Fatalf("种子锚定后应为 1 组，实得 %d 组——锚定是否失效？\n%v", len(gotAnchored), gotAnchored)
	}
	if !reflect.DeepEqual(gotAnchored, gotUnlimited) {
		t.Fatalf("种子锚定未消除限额漏合并：\n锚定 %v\n不限 %v", gotAnchored, gotUnlimited)
	}
	// ② 阴性对照：不锚种子时必须分拆——证明场景确实踩在边界上、机制可观测分歧。
	if len(gotNaive) != 2 {
		t.Fatalf("不锚种子时应分拆为 2 组（阴性对照），实得 %d 组——场景是否已不触发限额？\n%v",
			len(gotNaive), gotNaive)
	}
	if reflect.DeepEqual(gotNaive, gotAnchored) {
		t.Fatalf("锚定与不锚定分组相同——探针失去鉴别力")
	}
	t.Logf("异质边界：不限额 %d 组 / 锚定 %d 组 / 不锚定 %d 组；截断成员 anchored=%d naive=%d（listCalls=%d）",
		len(gotUnlimited), len(gotAnchored), len(gotNaive),
		anchoredStore.truncated, naiveStore.truncated, anchoredStore.listCalls)
}
