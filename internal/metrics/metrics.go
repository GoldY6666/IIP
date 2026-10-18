// Package metrics 定义 IIP 的自监控指标（M6 T6.3）。
//
// 设计要点（改之前先读完，这几条都是踩过的）：
//
//  1. **不用全局默认注册表**（prometheus.DefaultRegisterer）。
//     全局注册表是进程单例，单测里每构造一个 Service 就注册一次，
//     第二轮必然 panic「duplicate metrics collector registration」；
//     更糟的是两个测试的数值会互相污染，断言只能写成「大于 0」这种空话。
//     故每个 New() 自带一个独立 Registry，测试各拿各的。
//
//  2. **标签基数必须无界**。本包任何指标都不允许出现 incident_id /
//     alert_id / fingerprint / service 名这类每事件递增的值 ——
//     它们会让 Prometheus 的时序数随业务量线性增长，最终把监控自己拖死。
//     metrics_test.go 的 TestLabelNamesAreBounded 就是这条的守卫。
//
//  3. **两个进程各有一份**：iip-server 暴露接入/关联/事件类指标，
//     iip-worker 暴露采集/重试类指标。同一个指标名不会在两个进程同时出现，
//     T6.4 的 scrape 配置因此需要两个 target —— 这是刻意的，
//     因为「server 里没有采集数据」和「采集挂了」必须能区分。
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// 指标名。导出成常量是因为探针脚本与单测要按名字断言，
// 而「改了名字但断言还写着旧串」是一条必然静默的漂移。
const (
	AlertIngestTotal     = "iip_alert_ingest_total"
	AlertDedupTotal      = "iip_alert_dedup_total"
	WebhookDurationSecs  = "iip_webhook_duration_seconds"
	CorrelationScore     = "iip_correlation_score"
	IncidentCreatedTotal = "iip_incident_created_total"
	EvidenceDurationSecs = "iip_evidence_collect_duration_seconds"
	TaskRetryTotal       = "iip_task_retry_total"
)

// iip_alert_ingest_total 的 result 取值。
//
// 「stored」同时覆盖「新插入」与「把已有 firing 置为 resolved」：
// 两者都是**库被成功写入**，也是运维唯一关心的二分（写进去了 vs 没写）。
// 想区分插入/更新请看日志的 alert_id 与状态流转，不该再开一个标签 ——
// 标签一旦为「调试方便」而加，就会有人继续加第三个、第四个。
const (
	IngestStored    = "stored"
	IngestDuplicate = "duplicate"
	IngestInvalid   = "invalid"
	IngestError     = "error" // 存储层故障（Ingest 提前返回 error 的那些条目）
)

// iip_alert_dedup_total 的 result 取值。
//
// 不变量（单测与探针都断言它）：
//
//	hit + miss == ingest{stored} + ingest{duplicate}
//
// 即「每条进入去重判定的告警恰好计一次」。invalid 与 error 两类不进 persist，
// 故不在等式内 —— 这个等式本身就是判据，比分别断言四个数更能发现漏埋点。
//
// race 是 miss 的**子集**，不是第三类：它表示「判 miss 之后插入时撞了唯一约束」，
// 也就是并发去重兜底真的生效了。因为有重合，race 不参与上面的等式。
const (
	DedupHit  = "hit"  // 库中已有该指纹
	DedupMiss = "miss" // 库中无历史（新指纹）
	DedupRace = "race" // 插入时撞唯一约束（并发兜底，同时已计入 miss）
)

// iip_task_retry_total 的 reason 取值。
const (
	RetryStaleReclaim  = "stale_reclaim"  // 回收持有者已消失的 in_flight 任务
	RetryAttemptFailed = "attempt_failed" // 单次执行失败（含随后进死信的那些）
)

// 直方图桶。
//
// 三组桶的选取依据各不相同，不要「统一成 Prometheus 默认桶」：
var (
	// webhookBuckets 覆盖 5ms~5s。本项目实测 16 个真实请求 P50=2.1ms、
	// 最大 72.7ms，故前几档很密；5s 之上没有业务意义（HTTP 超时 15s 会先咬）。
	webhookBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

	// evidenceBuckets 覆盖 50ms~30s：采集器自身超时是 5s，
	// 但「全部采集器依次超时」可能累积到数十秒，桶的上界要留出这段。
	evidenceBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

	// scoreBuckets 是关联分数的值域 [0,1]，步长 0.1。
	// 分数是加权和，不是耗时，套耗时桶只会得到一堆挤在 le=0.1 的样本。
	// 0 分（无候选事件）落在 le=0.1 桶内，这是正确的：它确实 ≤0.1。
	scoreBuckets = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1}
)

// Metrics 是一组指标句柄 + 它们所属的注册表。
//
// 字段全部是具体类型（而非接口）：埋点处是热路径，能省一次接口调用是一次；
// 且 nil 永远不会出现 —— New 与 Noop 都保证七个仪器全部非 nil，
// 故埋点代码里不需要任何 nil 判断（这类判断一旦漏写就是运行期 panic）。
type Metrics struct {
	reg *prometheus.Registry

	// AlertIngest 按处理结论计数（internal/alert.Ingest）。
	AlertIngest *prometheus.CounterVec
	// AlertDedup 按去重判定计数（internal/alert.persist）。
	AlertDedup *prometheus.CounterVec
	// WebhookDuration 是告警 webhook 的端到端处理时长（含关联）。
	WebhookDuration prometheus.Histogram
	// CorrelationScore 按最终档位记录关联分数（internal/correlation）。
	CorrelationScore *prometheus.HistogramVec
	// IncidentCreated 是新建事件计数（CREATE 档，不含合入）。
	IncidentCreated prometheus.Counter
	// EvidenceDuration 按采集器来源记录单次采集耗时（internal/worker）。
	EvidenceDuration *prometheus.HistogramVec
	// TaskRetry 按原因记录重试/回收次数（internal/worker）。
	TaskRetry *prometheus.CounterVec
}

// New 构造一组指标与它独占的注册表。
//
// 同时注册 Go 运行时与进程指标（go_goroutines / process_resident_memory_bytes 等）：
// 「自监控」若只有业务计数，goroutine 泄漏与内存增长就只能靠 OOM 才发现。
// 这不是本项目的业务指标，故不进上面的常量表，也不需要断言。
func New() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}

	m.AlertIngest = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: AlertIngestTotal,
		Help: "告警接入的逐条处理结论（stored/duplicate/invalid/error）。",
	}, []string{"result"})

	m.AlertDedup = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: AlertDedupTotal,
		Help: "告警去重判定（hit/miss；race 为 miss 的子集，表示插入时撞唯一约束）。",
	}, []string{"result"})

	m.WebhookDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    WebhookDurationSecs,
		Help:    "告警 webhook 的端到端处理时长（秒），含同批告警的关联。",
		Buckets: webhookBuckets,
	})

	m.CorrelationScore = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    CorrelationScore,
		Help:    "关联评分分布，按最终档位（new/review/merge）分组。",
		Buckets: scoreBuckets,
	}, []string{"action"})

	m.IncidentCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: IncidentCreatedTotal,
		Help: "新建事件数（关联结论为 new 的事件，不含合入已有事件）。",
	})

	m.EvidenceDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    EvidenceDurationSecs,
		Help:    "单次证据采集耗时（秒），按采集器来源分组。",
		Buckets: evidenceBuckets,
	}, []string{"source"})

	m.TaskRetry = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: TaskRetryTotal,
		Help: "任务重试相关计数（stale_reclaim=回收僵死任务数，attempt_failed=执行失败次数）。",
	}, []string{"reason"})

	m.reg.MustRegister(
		m.AlertIngest,
		m.AlertDedup,
		m.WebhookDuration,
		m.CorrelationScore,
		m.IncidentCreated,
		m.EvidenceDuration,
		m.TaskRetry,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// noop 是 Noop() 的共享实例；sync.OnceValue 保证只构造一次。
var noop = sync.OnceValue(New)

// Noop 返回一组**不会被任何人暴露**的指标。
//
// 这是「未注入指标」的默认值，不是空实现：埋点照常计数，只是没有任何
// HTTP 端点采集它。这样做的收益是**埋点侧零分支** —— 若改成返回 nil，
// 每一处埋点都要写 `if m != nil`，而漏写一处就是运行期 panic，
// 且 panic 只在「恰好没配指标」的部署里出现，本地永远复现不了。
//
// 代价是每个未注入的 Service 共享同一份计数器（数值跨实例累加）。
// 这无害：它永远不会被 /metrics 读到，也无人断言它的值。
func Noop() *Metrics { return noop() }

// Handler 返回采集本组指标（含运行时指标）的 HTTP handler。
//
// 用 HandlerFor 而不是 promhttp.Handler()：后者绑全局注册表，
// 与 New() 的独立注册表不是同一个东西，接错了会得到一个**只有 go_* 指标、
// 业务指标一条都没有**的 /metrics —— 看起来在工作，实际什么也测不到。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		// 采集出错时返回 5xx 而不是静默给半份数据：
		// 半份数据会让 Prometheus 把缺失的序列当成「指标消失了」，
		// 触发假的 absent() 告警，比直接报错难查得多。
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Registry 暴露注册表，供单测 Gather 断言。
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }
