package correlation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/metrics"
)

// 本文件是回退快照 TTL 的收口判据（2026-10-19 压测实测后引入，动机与实测
// 数据见 candidates.go 的 fallbackSnapshotTTL）：不限额候选快照从「每次
// CorrelateAlerts 调用取一次」改为「Service 级共享 + TTL 内复用」。
//
// 动机实测：标准风暴场景组内同质告警恒处 REVIEW 档（低于 merge 阈值）、
// 大事件恒被限额截断——触发条件两个合取项恒真，1 万条灌入触发回退 9840
// 次、结论 100% 零变化。快照不复用时吞吐 346→34 条/s、折算单条 p95
// 45.6→406.2ms（T7.3 第 5 判据由 PASS 转 FAIL）。
//
// 四条判据：
//
//	① TTL 内复用：连续两次 unlimited 只查一次库，且第二次返回冻结快照
//	   （看不到 TTL 内新入伙的成员——staleness 有界是设计语义，不是缺陷）；
//	② TTL 过期重查：快照老化后重新打库，并看得到新成员；
//	③ 存储故障按 TTL 缓存且到期自愈：瞬时故障不会把回退通道永久关死；
//	④ Service 级共享：同一 Service 上两条告警先后触发回退只查一次库，
//	   且第二条的合并结论仍然正确（共享没有把回退变成马后炮）。

// TestUnlimitedSnapshotReusedWithinTTL：TTL 内的第二次 unlimited 必须复用
// 第一次的快照——既不打第二次库（查询计数），也看不到 TTL 内新入伙的成员
// （返回的是冻结快照，行为级证明「复用」而不是「恰好查到了同样的数据」）。
func TestUnlimitedSnapshotReusedWithinTTL(t *testing.T) {
	ctx := context.Background()
	store := newRealtimeStore(limitWithSeed)
	// 预置一名成员让快照非空——空 map 的复用断言（空==空）没有鉴别力。
	store.members[1] = []CandidateAlert{{IncidentID: 1, View: snapshotView(t, 1)}}
	f := newFallbackSource(store, discardLogger(), fallbackSnapshotTTL)

	first, err := f.unlimited(ctx)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	// TTL 内库状态变化（新成员入伙）：第二次调用必须看不到它。
	store.members[1] = append(store.members[1], CandidateAlert{IncidentID: 1, View: snapshotView(t, 2)})
	second, err := f.unlimited(ctx)
	if err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if got := f.snapshotQueries(); got != 1 {
		t.Fatalf("TTL 内应复用快照（只查 1 次库），实得 %d", got)
	}
	if len(second[1]) != 1 || len(first[1]) != 1 {
		t.Fatalf("TTL 内应返回冻结快照（1 名成员），实得首次 %d、二次 %d——复用了却打到了新库？",
			len(first[1]), len(second[1]))
	}
}

// TestUnlimitedSnapshotRefreshedAfterTTL：快照老化超过 TTL 后必须重查，
// 并看得到老化期间新入伙的成员。用人工老化 snapshotAt 代替 sleep——测试
// 不能把 correctness 绑在 wall-clock 等待上。
func TestUnlimitedSnapshotRefreshedAfterTTL(t *testing.T) {
	ctx := context.Background()
	store := newRealtimeStore(limitWithSeed)
	store.members[1] = []CandidateAlert{{IncidentID: 1, View: snapshotView(t, 1)}}
	f := newFallbackSource(store, discardLogger(), fallbackSnapshotTTL)

	if _, err := f.unlimited(ctx); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	f.snapshotAt = time.Now().Add(-2 * fallbackSnapshotTTL)
	store.members[1] = append(store.members[1], CandidateAlert{IncidentID: 1, View: snapshotView(t, 2)})
	second, err := f.unlimited(ctx)
	if err != nil {
		t.Fatalf("过期重查失败: %v", err)
	}
	if got := f.snapshotQueries(); got != 2 {
		t.Fatalf("快照过期后应重查（累计 2 次），实得 %d", got)
	}
	if len(second[1]) != 2 {
		t.Fatalf("过期重查应看得到新成员（2 名），实得 %d", len(second[1]))
	}
}

// unlimitedCountingStore 包装 realtimeStore，只数 limit<=0 的不限额查询次数。
// 计数器放在本文件而不是加进 realtimeStore：后者所属的 replay_realtime_test.go
// 是已挂账的超标文件（基线只减不增），不该为加一个字段再涨。
type unlimitedCountingStore struct {
	IncidentStore
	unlimited int
}

func (u *unlimitedCountingStore) ListOpenIncidentAlerts(ctx context.Context, perIncidentLimit int32) ([]CandidateAlert, error) {
	if perIncidentLimit <= 0 {
		u.unlimited++
	}
	return u.IncidentStore.ListOpenIncidentAlerts(ctx, perIncidentLimit)
}

// failingStore 在前 failTimes 次不限额查询上返回存储故障，之后恢复正常；
// 其余方法转发给内层假存储。用于验证 fetchErr 按 TTL 失效（故障自愈）。
type failingStore struct {
	IncidentStore
	failTimes int
	calls     int
}

func (f *failingStore) ListOpenIncidentAlerts(ctx context.Context, perIncidentLimit int32) ([]CandidateAlert, error) {
	if perIncidentLimit <= 0 {
		f.calls++
		if f.calls <= f.failTimes {
			return nil, errors.New("模拟存储故障")
		}
	}
	return f.IncidentStore.ListOpenIncidentAlerts(ctx, perIncidentLimit)
}

// TestUnlimitedFetchErrorExpiresByTTL：存储故障在 TTL 内被缓存（不重复打库），
// TTL 到期后自动重试——Service 级共享后故障缓存的生命周期不再是「一次调用」，
// 不设 TTL 会把一次瞬时故障变成回退通道的永久关闭。
func TestUnlimitedFetchErrorExpiresByTTL(t *testing.T) {
	ctx := context.Background()
	store := &failingStore{IncidentStore: newRealtimeStore(limitWithSeed), failTimes: 1}
	f := newFallbackSource(store, discardLogger(), fallbackSnapshotTTL)

	if _, err := f.unlimited(ctx); err == nil {
		t.Fatalf("首次查询应返回存储故障")
	}
	if _, err := f.unlimited(ctx); err == nil {
		t.Fatalf("TTL 内应返回缓存的故障（不重复打库）")
	}
	if store.calls != 1 {
		t.Fatalf("TTL 内不应重复打库，实得 %d 次", store.calls)
	}
	// 人工老化故障缓存 → 自愈重试成功。
	f.fetchErrAt = time.Now().Add(-2 * fallbackSnapshotTTL)
	snap, err := f.unlimited(ctx)
	if err != nil {
		t.Fatalf("故障缓存过期后应自愈重试，实得错误: %v", err)
	}
	if got := f.snapshotQueries(); got != 1 {
		t.Fatalf("自愈后应只成功查到 1 次快照，实得 %d", got)
	}
	if snap == nil {
		t.Fatalf("自愈查询返回了 nil 快照")
	}
}

// TestFallbackSnapshotSharedAcrossService：Service 级共享的核心判据——同一
// Service 上两条告警先后踩中回退，不限额快照只查一次（第二条复用第一条的），
// 且第二条的合并结论不受共享影响（仍然 0.80 自动合并，而不是 REVIEW）。
//
// 场景（在 TestCandidateLimitFallbackRecoversOldNonSeedMember 的骨架上追加
// 一名第二触发者；O2 是被限额裁出最近 20 条的非种子老成员）：
//
//	O1       payment/LatencyHigh/prod      无候选，新建事件 1（种子）
//	O2       gateway/LatencyHigh/prod      与 O1 得 0.70，REVIEW 并入事件 1
//	R1..R25  gateway/ErrorsHigh/staging    与 O2 得 0.67 REVIEW 入伙，把 O2 挤出最近 20 条
//	N1       gateway/LatencyHigh/prod      O2 的同源克隆：触发回退 1，0.80 合并
//	R26..R45 gateway/ErrorsHigh/staging    互相 0.80 合并，再把 N1 挤出最近 20 条
//	N2       gateway/LatencyHigh/prod      O2 的同源克隆：触发回退 2（复用快照），0.80 合并
func TestFallbackSnapshotSharedAcrossService(t *testing.T) {
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

	seq := []alert.StoredAlert{
		mk(1, "payment", "LatencyHigh", "prod"),
		mk(2, "gateway", "LatencyHigh", "prod"),
	}
	for i := 0; i < 25; i++ {
		seq = append(seq, mk(3+i, "gateway", "ErrorsHigh", "staging"))
	}
	seq = append(seq, mk(28, "gateway", "LatencyHigh", "prod")) // N1
	for i := 0; i < 20; i++ {
		seq = append(seq, mk(29+i, "gateway", "ErrorsHigh", "staging")) // R26..R45
	}
	seq = append(seq, mk(49, "gateway", "LatencyHigh", "prod")) // N2

	inner := newRealtimeStore(limitWithSeed)
	store := &unlimitedCountingStore{IncidentStore: inner}
	svc := NewService(store, &fakeConfigStore{cfg: DefaultConfig()}, topo, discardLogger())
	m := metrics.New()
	svc.WithMetrics(m)
	for i := range seq {
		inner.views[seq[i].ID] = NewAlertView(&seq[i])
		if err := svc.CorrelateAlerts(ctx, seq[i:i+1]); err != nil {
			t.Fatalf("第 %d 条告警关联失败: %v", i, err)
		}
	}

	// 前置断言：49 条必须全部落进事件（一条不丢），否则后续断言失去意义。
	if n := inner.totalMembers(); n != len(seq) {
		t.Fatalf("假存储只累积了 %d 条成员，期望 %d", n, len(seq))
	}
	// 触发次数恰为 2：只有 N1/N2 同时满足「集内 < merge」与「事件被截断」
	// （R 们集内互相 0.80，不触发）。
	if got := testutil.ToFloat64(m.CorrelationCandidateFallback); got != 2 {
		t.Fatalf("候选限额回退应触发 2 次（N1/N2），实得 %v——触发条件过宽还是过窄？", got)
	}
	// 核心：不限额快照只查 1 次——N2 复用了 N1 触发的那次查询。
	if got := store.unlimited; got != 1 {
		t.Fatalf("不限额快照应只查 1 次（TTL 内跨告警共享），实得 %d——共享失效？", got)
	}
	// 共享不能把回退变成马后炮：N2 仍以 ≥merge 非待确认合并（复用快照里
	// 有 O2，结论与独立查询一致）。
	rec, ok := inner.linkOf(49)
	if !ok {
		t.Fatalf("N2（告警 49）没有关联记录——它既没合并也没建事件？")
	}
	if rec.needsReview || rec.score == nil || *rec.score < DefaultConfig().Thresholds.Merge {
		t.Fatalf("N2 应以 ≥%v 自动合并，实得 score=%v needsReview=%v",
			DefaultConfig().Thresholds.Merge, rec.score, rec.needsReview)
	}
}

// snapshotView 构造一条最小告警视图，供快照探针预置假存储成员用。
func snapshotView(t *testing.T, id int) AlertView {
	t.Helper()
	labels, err := json.Marshal(map[string]string{"alertname": "snapshot-probe"})
	if err != nil {
		t.Fatalf("构造标签失败: %v", err)
	}
	return NewAlertView(&alert.StoredAlert{
		ID: int64(id),
		Alert: alert.Alert{
			Fingerprint: fmt.Sprintf("snapshot-probe#%d", id),
			Labels:      labels,
			Annotations: json.RawMessage(`{}`),
			StartsAt:    time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		},
	})
}
