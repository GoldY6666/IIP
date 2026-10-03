package evidence

import (
	"fmt"
	"time"
)

// 本文件是 M4 的**采集窗口推导**，纯函数、零依赖、可完整单测。
//
// 窗口推导是 M4 最容易出错的一处，三个决策各自都有代价，
// 且互相耦合（改一个会连带影响另外两个），所以集中在这里而不是散在 worker 里：
//
//  1. **首次采集**从「事故起点前 1 小时」开始，而不是「现在往前 1 小时」。
//     事故开始于 3 小时前的事件，若从 now-1h 开始采，第一轮会**完全采不到
//     事故时刻的数据** —— 页面上看不出「当时到底怎么了」，
//     而那正是证据存在的意义。
//
//  2. **窗口宽度封顶** EvidenceWindow（默认 1h）。
//     不封顶的话，持续 12 小时的事故会拉出一个 12 小时窗口，
//     Prometheus 查询变慢、证据量激增。宽度封顶后，
//     超长事故靠「游标式增量采集」一轮轮往前补，而不是一次拉一大段。
//
//  3. **后续采集从游标开始**，只采新增区间。
//     否则新告警合入时会再采一遍同样的区间，
//     同一批 RED 指标以完全相同的 (source, query, timestamp) 落库两份。

// DefaultWindow 是采集窗口的默认宽度（IIP_EVIDENCE_WINDOW，默认 1h）。
const DefaultWindow = time.Hour

// leadBeforeIncident 是首次采集时，窗口起点比事故起点提前多久。
//
// 提前而非对齐：事故的**根因**往往早于第一条告警 ——
// 数据库连接池在 10:00 开始变慢，10:05 才触发「响应超时」告警。
// 只从 10:05 开始采，就看不到指标开始劣化的那段爬升过程，
// 而「什么时候开始变的」正是判断根因时最先要回答的问题。
const leadBeforeIncident = time.Hour

// WindowRequest 是一次窗口推导的输入。
type WindowRequest struct {
	// IncidentStart 是事件的开始时间（incidents.started_at）。
	//
	// 零值表示取不到（事件行异常）。此时退化为「从 now 往前一个窗口宽」，
	// 而不是报错 —— 一个拿不到开始时间的事件仍然值得采，
	// 只是窗口不如有开始时间时那么贴近事故。
	IncidentStart time.Time

	// Cursor 是上一轮采到的窗口终点；零值表示首次采集。
	Cursor time.Time

	// Now 是推导的基准时刻（注入以便测试断言）。
	Now time.Time

	// Width 是**后续轮次**的窗口宽度上限；<= 0 时用 DefaultWindow。
	//
	// 它**不约束首轮回补**，理由见 ResolveWindow。
	Width time.Duration
}

// maxLookback 是首轮回补能回溯多久的**硬上限**。
//
// 存在的唯一理由：`incidents.started_at` 来自告警源提供的 startsAt，
// 是个外部输入。一个填错成 1970 年的 startsAt 若不设限，
// 会让第一次采集拉出一个 56 年的 Prometheus 区间 —— 查询要么超时，
// 要么把返回的每一条样本都当成证据写进库。
//
// 取 24h：真实事故不会在「创建 24 小时后才被发现」的情况下还需要回补，
// 而 24h 足以覆盖跨夜班的运维响应节奏。
const maxLookback = 24 * time.Hour

// ResolveWindow 推导本轮的采集窗口 [Start, End]。
//
// 返回的窗口**一定满足 Window.Validate()**（起点 != 终点、终点不早于起点），
// 哪怕输入退化到「now - width」这种刚好等于整个宽度的情形。
//
// 两条分支的语义与代价都不同：
//
//	有游标   → [游标, now]，宽度天然不超过「两次采集的间隔」
//	首次采集 → [事故起点 - 1h, now]，**允许超过 Width**
//
// 首轮为什么不受 Width 约束
// ------------------------
// Width 封顶是为了防住「反复采集时查询越拖越长」，而首轮是一次性回补。
// 若首轮也套 max(事故起点-1h, now-Width)，那个 max 在数学上**永远取不到**
// 「事故起点-1h」—— 它胜出的充要条件是事故开始时间晚于 now，
// 即事故还没开始。后果是「3 小时前才发现的事故」永远采不到事故起点，
// 而游标只往前走、不会回头补，那段数据**永久丢失**。
//
// 「首轮多花一次长查询」与「永久看不到事故何时开始劣化」相比，
// 前者是可控的成本，后者是产品缺陷。
//
// 唯一护栏是 maxLookback（24h），防 started_at 被填坏这类输入异常。
func ResolveWindow(req WindowRequest) (Window, error) {
	if req.Now.IsZero() {
		return Window{}, fmt.Errorf("推导采集窗口时基准时刻不能为空")
	}
	now := req.Now.UTC()
	width := req.Width
	if width <= 0 {
		width = DefaultWindow
	}

	// 情况一：已有游标，只采新增区间。
	//
	// 游标 >= now 视为不可用（时钟漂移，或并发补采时另一个 worker
	// 用更晚的时刻完成）：此时退回首次采集的推导，
	// 而不是返回一个 [now, now] 的空窗口 —— 那会让采集器查到零条数据，
	// 看起来像「这段时间没有异常」，实际是窗口退化。
	if !req.Cursor.IsZero() && req.Cursor.UTC().Before(now) {
		return Window{Start: req.Cursor.UTC(), End: now}, nil
	}

	// 情况二：首次采集，回溯到事故起点之前 1 小时。
	if !req.IncidentStart.IsZero() {
		start := req.IncidentStart.UTC().Add(-leadBeforeIncident)
		// 事故开始时间在将来（告警源时钟快于本机）时 start 会超过 now，
		// 夹回 now 往前一个宽度。
		if !start.Before(now) {
			return Window{Start: now.Add(-width), End: now}, nil
		}
		// 回溯超过 maxLookback 说明 started_at 不可信，夹住。
		if floor := now.Add(-maxLookback); start.Before(floor) {
			start = floor
		}
		return Window{Start: start, End: now}, nil
	}

	// 情况三：拿不到事故起点，退化为固定宽度。
	return Window{Start: now.Add(-width), End: now}, nil
}
