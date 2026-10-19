package correlation

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// unlimitedLimit 是不限额通道的传参。SQL 约定 limit<=0 即不限额
// （见 internal/db/queries/incidents.sql 的 ListOpenIncidentsWithAlerts），
// 取 0 是这个约定的显式命名，避免调用点出现说不清含义的裸 0。
const unlimitedLimit int32 = 0

// groupByIncident 把仓储返回的候选告警按 Incident 聚合成 map。
// 批次内新建/合并的事件由调用方实时并入同一 map，使同批告警可互相累积。
func groupByIncident(cands []CandidateAlert) map[int64][]AlertView {
	byIncident := make(map[int64][]AlertView)
	for _, c := range cands {
		byIncident[c.IncidentID] = append(byIncident[c.IncidentID], c.View)
	}
	return byIncident
}

// bestMatch 在候选集中逐事件取最大分，返回全局最高分及其所属事件 ID。
//
// 纯函数（不碰存储、不写日志）：限额路径与回退路径共用同一个判定内核，
// 两段式结论因此是同构的——回退只是换了一份更大的候选集。
// 无候选时返回 (0, 0)，调用方据此走 NEW 档。
func bestMatch(byIncident map[int64][]AlertView, scorer *Scorer, v AlertView) (score float64, incidentID int64) {
	for incID, members := range byIncident {
		maxForIncident := 0.0
		for _, existing := range members {
			if sc := scorer.Score(v, existing); sc > maxForIncident {
				maxForIncident = sc
			}
		}
		if maxForIncident > score {
			score = maxForIncident
			incidentID = incID
		}
	}
	return score, incidentID
}

// anyIncidentTruncated 判断限额候选集里是否存在「成员被限额裁掉」的事件。
//
// 判据：某事件在候选集里的条数 == limit+1。依据 SQL 语义
// （ListOpenIncidentsWithAlerts：每事件返回「最近 limit 条 ∪ 种子 min(a.id)」）：
//
//	成员数 m ≤ limit ：全部返回，条数 = m（无所隐藏）；
//	m == limit       ：种子就在最近 limit 条内，条数 = limit（无所隐藏）；
//	m == limit+1     ：种子恰在最近 limit 条之外，条数 = limit+1（恰好返回
//	                   全部成员，无所隐藏——见下面的假阳性说明）；
//	m >  limit+1     ：条数 = limit+1，m-(limit+1) 名老成员被裁掉。
//
// 故「条数 == limit+1」是「m > limit」的**可靠信号**：真截断时必为 limit+1
// （不漏报）；唯一假阳性是 m == limit+1——无所隐藏却也会触发回退，代价是
// 一次返回同样本的回退查询、结论不变，可接受。
//
// 为什么不按 ID 连续性找间隙：告警 ID 是全局序列，同一事件的成员 ID 天然
// 有洞（别的事件穿插其间），连续性判据会把「没截断」系统性误判成「截断」。
func anyIncidentTruncated(byIncident map[int64][]AlertView, limit int) bool {
	if limit <= 0 {
		// 调用方传 <=0 时候选集本就不限额，无所谓截断。
		return false
	}
	for _, members := range byIncident {
		if len(members) == limit+1 {
			return true
		}
	}
	return false
}

// mergeViews 返回 bounded 与 unlimited 两份候选集的并集（按告警 ID 去重），
// 不修改任何入参。供回退路径做一次性的全量重算。
func mergeViews(bounded, unlimited map[int64][]AlertView) map[int64][]AlertView {
	merged := make(map[int64][]AlertView, len(bounded)+len(unlimited))
	for incID, views := range bounded {
		cp := make([]AlertView, len(views))
		copy(cp, views)
		merged[incID] = cp
	}
	for incID, views := range unlimited {
		if len(views) == 0 {
			continue
		}
		seen := make(map[int64]struct{}, len(merged[incID]))
		for _, v := range merged[incID] {
			seen[v.ID] = struct{}{}
		}
		for _, v := range views {
			if _, dup := seen[v.ID]; dup {
				continue
			}
			merged[incID] = append(merged[incID], v)
			seen[v.ID] = struct{}{}
		}
	}
	return merged
}

// fallbackSnapshotTTL 是不限额候选快照的最大年龄（2026-10-19 压测实测后引入）。
//
// 为什么需要 TTL：标准风暴场景里组内同质告警的相似度恒在 REVIEW 档
// （低于 merge 阈值）、大事件恒被限额截断——回退触发条件的两个合取项
// 在该场景下恒真，回退从「例外通道」变成热路径（1 万条压测实测触发
// 9840/10000 次，且结论 100% 零变化）。快照若每次触发都重取，吞吐
// 从 346 条/s 崩到 34 条/s（R7 收口时的无回退版 vs 带回退版实测）。
//
// 代价（staleness ≤ TTL）：回退结论基于最多 TTL 前的全量快照；本批增量
// 由 mergeViews 与内存累积的 byIncident 取并集补回。上述压测同时实测
// 9840 次回退结论零变化，该窗口对实际结论无影响；且仍严格优于 R7 无
// 回退版（那版对被截断成员永不修复）——语义是「最多延迟 TTL 修复」，
// 不是放松正确性。
const fallbackSnapshotTTL = 2 * time.Second

// fallbackSource 按需补取「不限额」候选集快照；TTL 内跨调用复用，过期才
// 重查。由 Service 级共享（见 NewService 的装配），一批/多批告警踩中回退
// 时只打一次库。
//
// 为什么必须缓存：一批告警里可能有多条都踩在限额边界上（同批 webhook
// 投递），压测场景更是几乎每条都踩（见 fallbackSnapshotTTL 的实测），
// 不缓存就会对每条都跑一次全量扫描。缓存的是**取到的快照**；快照之后
// 本批新建/合入的成员不在其中，由调用方以 mergeViews 与内存累积的
// byIncident 取并集补回——并集而非二选一是必要的：快照之后库状态可能
// 又变了（别的事务合入新成员、或事件被转为 closed），单向覆盖会丢候选。
type fallbackSource struct {
	store  IncidentStore
	logger *slog.Logger

	mu sync.Mutex
	// ttl <= 0 时永不缓存（每次调用都重查），供探针与变异验证使用。
	ttl        time.Duration
	snapshot   map[int64][]AlertView
	snapshotAt time.Time
	// fetchErr/fetchErrAt 缓存存储故障并同样按 TTL 失效——Service 级共享后
	// 故障缓存的生命周期不再是「一次调用」，不设 TTL 会把一次瞬时故障
	// 变成回退通道的永久关闭（存储恢复后也不会自愈）。
	fetchErr   error
	fetchErrAt time.Time
	// snapshots 是累计快照查询次数（诊断与探针用）。触发次数看指标
	// iip_correlation_candidate_fallback_total，两者之比即缓存收益。
	snapshots int64
}

// newFallbackSource 构造回退源。logger 为 nil 时退化为 slog.Default()。
func newFallbackSource(store IncidentStore, logger *slog.Logger, ttl time.Duration) *fallbackSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &fallbackSource{store: store, logger: logger, ttl: ttl}
}

// unlimited 返回不限额候选集快照；TTL 内复用同一次查询结果，过期重查。
//
// 并发：Service 级共享后多个 webhook goroutine 会同时进入；持锁查询 +
// 双检——第一个 goroutine 查库期间其余等待，出来后发现快照已新直接
// 复用，无惊群。阻塞只发生在回退慢路径上。
//
// 存储故障时返回包装错误并按 TTL 缓存之——语义与限额查询的失败一致：
// 向上冒泡，由 handler 只记日志，绝不让关联失败变成 webhook 失败
// （手册 §4.3 铁律）。故障时不清除旧快照：TTL 判定保证过期快照不会被
// 读到，重试成功即被新快照取代。
func (f *fallbackSource) unlimited(ctx context.Context) (snapshot map[int64][]AlertView, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil && time.Since(f.fetchErrAt) < f.ttl {
		return nil, f.fetchErr
	}
	if f.snapshot != nil && time.Since(f.snapshotAt) < f.ttl {
		return f.snapshot, nil
	}
	started := time.Now()
	cands, listErr := f.store.ListOpenIncidentAlerts(ctx, unlimitedLimit)
	if listErr != nil {
		f.fetchErr = fmt.Errorf("回退查询候选事件失败: %w", listErr)
		f.fetchErrAt = time.Now()
		return nil, f.fetchErr
	}
	f.fetchErr = nil
	f.snapshot = groupByIncident(cands)
	f.snapshotAt = time.Now()
	f.snapshots++
	rows := 0
	for _, views := range f.snapshot {
		rows += len(views)
	}
	f.logger.Info("回退快照刷新",
		slog.Int("候选行数", rows),
		slog.Int64("耗时_ms", time.Since(started).Milliseconds()),
		slog.Int64("累计查询", f.snapshots))
	return f.snapshot, nil
}

// snapshotQueries 返回累计快照查询次数（探针与诊断用；并发安全）。
func (f *fallbackSource) snapshotQueries() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots
}
