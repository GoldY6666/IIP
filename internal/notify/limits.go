package notify

import (
	"sync"
	"time"
)

// 去风暴的参数（T6.2，ADR-025）。
//
// 取值来自开发计划的原文「同 Incident 10 分钟内最多 2 条更新消息」。
// 刻意不做成配置键：这两项是「一条消息值不值得吵醒人」的经验值，
// 不是部署差异；多两个配置键就多两处可被改错的地方。T6.3 出 metrics
// 后若要按观测调，再考虑暴露。
const (
	// statusUpdateWindow 是滑动窗口长度。
	statusUpdateWindow = 10 * time.Minute
	// statusUpdateLimit 是窗口内允许的状态变更通知条数。
	statusUpdateLimit = 2
	// maxLimiterEntries 是限流表条目上限；超过时清理过期条目。
	//
	// 不用定时器：风暴/长期运行下「每条通知顺手清理一次」足够，
	// 定时器反而多一个要关的 goroutine 和一个要测的时钟。
	maxLimiterEntries = 4096
)

// alwaysNotifyStatus 是**不受限流**的目标状态。
//
// 白名单而非黑名单，理由是静默丢消息的代价不对称：
//   - resolved / ignored：on-call 只收到「已确认」却收不到「已解决」，
//     会以为故障还在，可能重复排查甚至升级；
//   - open：从终态重开说明问题复发，这正是最该被看见的消息。
//
// 被抑制的只应是中间态（acknowledged / investigating / mitigated）——
// 它们高频且信息量低，两条足够表达「有人在处理且推进到哪了」。
var alwaysNotifyStatus = map[string]bool{
	"resolved": true,
	"ignored":  true,
	"open":     true,
}

// statusLimiter 按事件维护状态变更通知的滑动窗口。
//
// 为什么是**滑动**而不是「从首条起算的固定窗口」：固定窗口在边界处
// 会漏掉已发出的消息。实证场景——t0 与 t0+9m59s 各发一条，
// 固定窗口在 t0+10m 一到就清零，于是 10 分 2 秒内可以发到 4 条；
// 滑动窗口只允许再发 1 条。判据里有一条专门抓这个差别。
type statusLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	seen map[int64][]time.Time // 每个事件最近的至多 statusUpdateLimit 个发送时刻（升序）
}

func newStatusLimiter(now func() time.Time) *statusLimiter {
	if now == nil {
		now = time.Now
	}
	return &statusLimiter{now: now, seen: make(map[int64][]time.Time)}
}

// allow 判定是否放行一条「状态变更」通知，放行时把当前时刻记入窗口。
//
// 返回的 sent 是**本次判定后**窗口内的条数（被抑制时是抑制前的条数），
// 供调用方写日志——「压了多少」得能看见，否则去风暴等于静默丢消息。
func (l *statusLimiter) allow(incidentID int64, to string) (ok bool, sent int) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	// 白名单：不判定、不记账。不记账是有意的——终态消息不该挤占中间态的配额，
	// 否则「确认 → 解决」两步就把窗口用完了。
	if alwaysNotifyStatus[to] {
		return true, 0
	}

	times := l.pruneLocked(incidentID, now)
	if len(times) >= statusUpdateLimit {
		return false, len(times)
	}
	l.seen[incidentID] = append(times, now)
	return true, len(times) + 1
}

// pruneLocked 丢掉窗口外的时刻，返回窗口内的切片（不改 map 的原有容量语义）。
func (l *statusLimiter) pruneLocked(incidentID int64, now time.Time) []time.Time {
	times := l.seen[incidentID]
	cutoff := now.Add(-statusUpdateWindow)
	keep := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	return keep
}

// sweepLocked 在条目过多时清理过期事件，给内存一个上界。
//
// 阈值 2×窗口：条目最后一次发送已早于两个窗口，说明该事件已经安静，
// 它的窗口状态对新判定没有影响，可以整条删掉。
func (l *statusLimiter) sweepLocked(now time.Time) {
	if len(l.seen) <= maxLimiterEntries {
		return
	}
	cutoff := now.Add(-2 * statusUpdateWindow)
	for id, times := range l.seen {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(l.seen, id)
		}
	}
}
