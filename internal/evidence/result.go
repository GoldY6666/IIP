package evidence

import (
	"context"
	"fmt"
	"time"
)

// DefaultMaxPerIncident 是单个事件保留的证据上限（手册 §4.2）。
//
// 为什么必须有上限：Prometheus 的高频指标在 1 小时窗口里能出上千个点，
// 不截断的话一次事件就把证据表撑爆，而后续采集全部变慢。
const DefaultMaxPerIncident = 50

// Result 是一次采集的汇总结果。
type Result struct {
	// Evidence 是实际保留的证据。
	Evidence []Evidence

	// Truncated 报告是否因超限被截断。
	//
	// 必须能被 API 读到：把「只保留了 50 条」说成「一共 50 条」，
	// 用户会以为没有更多证据（与 M3 的 truncated 同一理由）。
	Truncated bool

	// TotalCollected 是截断**前**的采集总数。
	TotalCollected int
}

// Apply 汇总多个采集器的产出，并施加单事件上限。
//
// 刻意**不去重**：同一个 (type, source, timestamp, query) 出现两次，
// 可能是真的采到了两条（比如两个 Prometheus 分片都返回了该样本），
// 去重会静默丢数据。若将来确认要按某键去重，须先在手册里写明判据。
func Apply(results [][]Evidence, maxPerIncident int) Result {
	total := 0
	for _, r := range results {
		total += len(r)
	}

	if maxPerIncident <= 0 {
		maxPerIncident = DefaultMaxPerIncident
	}

	flat := make([]Evidence, 0, total)
	for _, r := range results {
		flat = append(flat, r...)
	}

	if len(flat) <= maxPerIncident {
		return Result{Evidence: flat, Truncated: false, TotalCollected: total}
	}
	return Result{
		Evidence:       flat[:maxPerIncident],
		Truncated:      true,
		TotalCollected: total,
	}
}

// CollectorResult 是单个采集器的产出与失败原因。
type CollectorResult struct {
	// Source 是该采集器的来源标识。
	Source Source

	// Evidence 是采到的合法证据。
	Evidence []Evidence

	// Err 是采集本身的失败原因（连接不上、超时、解析不了等）。
	// nil 表示采集成功——**包括采到空结果**。
	Err error

	// Dropped 是被丢弃的非法证据条数。
	//
	// 单独计数而不只记在Err 里：丢弃是**部分成功**，
	// 若与 Err 混在一起，调用方无法区分「整个采集器挂了」与
	// 「拿到了 8 条、其中 1 条时间戳非法」。前者该重试，后者不该。
	Dropped int

	// DropReason 说明丢弃原因（第一条）。
	DropReason error

	// Duration 是本次采集调用的耗时（含超时与失败的那些）。
	//
	// 由 CollectAll 测量而不是让采集器自己报：超时/失败时采集器根本没机会
	// 返回任何东西，而「失败也耗时」恰恰是排查「谁把整轮拖慢了」的关键。
	// 在本层测还能保证口径一致 —— 各处自己计时，有人算上解析、有人只算 HTTP。
	Duration time.Duration
}

// CollectAll 依次调用注册表里的每个采集器，**单个失败不影响其余**。
//
// 这是手册 §4.3 的关键约束：三类数据源任一挂掉，其余两类仍要落库。
// 若**全部**失败，调用方应把整个任务判为失败（而不是静默成功）——
// 「什么都没采到」和「采到了 0 条」是两回事。
func CollectAll(ctx context.Context, reg *Registry, in Input) ([]Evidence, []CollectorResult) {
	collectors := reg.Collectors()
	results := make([]CollectorResult, 0, len(collectors))
	all := make([]Evidence, 0, len(collectors))

	for _, c := range collectors {
		// 计时包住整个 Collect 调用（含超时等待）：失败耗时同样是有效信息，
		// 只给成功的那些计时会让「某个采集器把整轮拖到超时」在指标上消失。
		started := time.Now()
		got, err := c.Collect(ctx, in)
		res := CollectorResult{Source: c.Source(), Err: err, Duration: time.Since(started)}
		if err == nil {
			// 单条非法证据不该拖垮整个采集器：跳过它，其余照收。
			for _, e := range got {
				if verr := e.Validate(); verr != nil {
					res.Dropped++
					if res.DropReason == nil {
						res.DropReason = fmt.Errorf("采集到非法证据已丢弃: %w", verr)
					}
					continue
				}
				res.Evidence = append(res.Evidence, e)
			}
		}
		all = append(all, res.Evidence...)
		results = append(results, res)
	}
	return all, results
}

// AllFailed 报告是否**所有**采集器都失败了。
//
// 判据是「有采集器被注册 且 每个都Err != nil」：
//   - 一个都没注册 → false（那是配置问题，不是采集失败，不该触发重试风暴）；
//   - 都成功但采到 0 条 → false（「采到了但无内容」是正常结果，手册 §七）。
func AllFailed(results []CollectorResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Err == nil {
			return false
		}
	}
	return true
}
