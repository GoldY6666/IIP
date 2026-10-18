// Package notify 提供事件通知的异步派发（T6.1，M6 §10，ADR-024）。
//
// 定位是**旁路**：ingest 与状态流转的主路径绝不等待通知。
// 两条纪律来自实测教训而非洁癖：
//   - 关联引擎曾在 webhook 请求路径上同步做事，把吞吐从 1785 条/s 拖到
//     ~6 条/s（R7）——凡是「主路径等外部系统」的形态都出过这事；
//   - Slack 挂掉时通知必须静默丢失（记日志），而不是让告警接入失败
//     （否则 Alertmanager 会重投整批，双倍放大故障）。
//
// 因此本包只依赖标准库：两个消费包（correlation / incident）定义各自的
// 最小接口并由 wiring 把 *Service 注入；sink 可替换（T6.2 的通用出站
// webhook 就是再加一个 Sink 实现）。
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultQueueSize 是派发队列容量。
//
// 256 的取舍：正常形态下通知量 = 事件创建 + 状态流转，分钟级个位数；
// 队列只需吸收「 Slack 短暂不可用期间的积压」。满了**丢弃最旧等待中的**
// 不适用——简单丢弃当前这条并记 WARN，因为通知无顺序保证，
// 而「积压几百条过时通知」本身没有价值。
const defaultQueueSize = 256

// defaultHandleTimeout 是单条通知的处理兜底超时（计数查询 + 全部 sink）。
//
// sink 有自己的超时（Slack 5s），这里是**总账**：防止某个 sink 忘了设
// 超时把派发线程永久占住，后面的通知全部堵死。
const defaultHandleTimeout = 15 * time.Second

// Kind 是通知类型。落日志与消息模板都用它区分。
type Kind string

const (
	// KindIncidentCreated 新事件创建（NEW 档建档成功）。
	KindIncidentCreated Kind = "incident_created"
	// KindStatusChanged 事件状态流转（acknowledged / resolved 等）。
	KindStatusChanged Kind = "status_changed"
)

// IncidentRef 是通知所需的事件最小信息集。
//
// 刻意不引用 incident.Snapshot / correlation.Incident：本包是叶子包，
// 不反向依赖领域类型；两个消费包各自把领域对象转成它（装配处两行映射）。
// 代价是字段重复声明，收益是 notify 的测试不需要 correlation/incident。
type IncidentRef struct {
	ID        int64
	Title     string
	Severity  string
	Service   string
	Status    string
	StartedAt time.Time
}

// Event 是一条待派发的通知。
type Event struct {
	Kind       Kind
	Incident   IncidentRef
	FromStatus string // 仅 KindStatusChanged：原状态
	ToStatus   string // 仅 KindStatusChanged：新状态
	Actor      string // 状态变更操作人；空 = 系统自动

	// AlertCount / EvidenceCount 由 Service 在派发线程里填充
	// （CountsProvider 可用时）；hook 调用方不填。
	AlertCount    int64
	EvidenceCount int64
}

// Sink 是一个通知出口。T6.1 只有 slack；T6.2 加通用出站 webhook。
type Sink interface {
	// Name 用于日志区分出口。
	Name() string
	// Send 发送一条通知。ctx 由 Service 提供（带兜底超时）。
	// 返回的错误只被记日志，不上行给调用方。
	//
	// 取 *Event 而非值：Event 约 176 B，超 ADR-012 的 128 B 阈值，
	// 按「真正超重入参改指针」的裁定处理，不写 nolint。
	Send(ctx context.Context, ev *Event) error
}

// CountsProvider 提供事件计数（通知消息里的「关联告警数 / 证据数」）。
//
// 由 internal/repository.Incidents 实现。**查询失败时 Service 放弃这条
// 通知**而不是按 0 发送：一条写着「证据 0 条」的通知会让 on-call
// 误判事件形态，比漏发更糟（与 confidence「给不出依据必须留空」同律）。
type CountsProvider interface {
	IncidentCounts(ctx context.Context, incidentID int64) (alertCount, evidenceCount int64, err error)
}

// Service 是异步派发器：Notify 入队即返回，worker goroutine 逐条发送。
//
// 零值不可用，用 New 构造。**nil *Service 的所有方法均可调用**
// （见 NotifyIncidentCreated 等），未配置 webhook 时 wiring 直接传 nil，
// 两个消费包 therefore 不需要「配了没有」的分支。
type Service struct {
	sinks  []Sink
	counts CountsProvider
	logger *slog.Logger

	queue      chan Event
	stopped    chan struct{}
	workerDone chan struct{}
	closeOnce  sync.Once

	// mu 保护 closed，并让「判关闭 + 入队」成为一个原子段。
	//
	// 为什么不能用 select 同时等 queue 与 stopped：两者同时就绪时 Go
	// 随机选一个分支，worker 排空期间 enqueue 会有一半概率把事件塞进
	// 已无人消费的队列并返回 true（实测 10 次跑 5 次红）。通知谎报成功
	// 比丢通知更糟——调用方以为发出去了。
	mu     sync.Mutex
	closed bool
}

// New 构造派发器并启动 worker。sinks 为空时仍可构造（计数仍会跑，
// 但没有出口——正常用法是 wiring 层在 sinks 为空时直接传 nil）。
func New(sinks []Sink, counts CountsProvider, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Service{
		sinks:      sinks,
		counts:     counts,
		logger:     logger,
		queue:      make(chan Event, defaultQueueSize),
		stopped:    make(chan struct{}),
		workerDone: make(chan struct{}),
	}
	go s.run()
	return s
}

// NotifyIncidentCreated 入队一条「事件创建」通知（correlation 侧 hook）。
//
// 返回值恒 false 仅发生在关闭后；调用方**不需要**关心返回值——
// 通知是 best-effort，签名不返回 error 就是刻意不让它有上行压力。
func (s *Service) NotifyIncidentCreated(inc IncidentRef) bool {
	if s == nil {
		return false
	}
	return s.enqueue(&Event{Kind: KindIncidentCreated, Incident: inc})
}

// NotifyStatusChanged 入队一条「状态变更」通知（incident 侧 hook）。
func (s *Service) NotifyStatusChanged(inc IncidentRef, from, to, actor string) bool {
	if s == nil {
		return false
	}
	return s.enqueue(&Event{
		Kind:       KindStatusChanged,
		Incident:   inc,
		FromStatus: from,
		ToStatus:   to,
		Actor:      actor,
	})
}

// enqueue 非阻塞入队；已关闭或队列满时丢弃并返回 false。
//
// 关闭判定在锁内完成：Close 一旦拿到锁置 closed，此后的 enqueue 必失败；
// 而在 Close 置位前已进临界区的 enqueue 一定在 Close 继续之前结束
// （Close 的 Do 函数也要拿同一把锁），因此「Close 返回后入队成功」不可能发生。
func (s *Service) enqueue(ev *Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		// 关闭期间的丢弃不记 WARN：这是正常关机语义，记了只会让
		// 「优雅退出」的日志里混进吓人的条目。
		return false
	}
	select {
	case s.queue <- *ev:
		return true
	default:
		s.logger.Warn("通知队列已满，丢弃该条通知",
			slog.String("kind", string(ev.Kind)),
			slog.Int64("incident_id", ev.Incident.ID))
		return false
	}
}

// run 是派发线程：取一条 → 补计数 → 送全部 sink。
func (s *Service) run() {
	defer close(s.workerDone)
	for {
		select {
		case ev := <-s.queue:
			s.handle(&ev)
		case <-s.stopped:
			s.drain()
			return
		}
	}
}

// drain 排空停止信号到达时队列里残留的事件（best-effort，不保证全发完——
// 真的发不完由 Close 的 ctx 截止兜底）。
func (s *Service) drain() {
	for {
		select {
		case ev := <-s.queue:
			s.handle(&ev)
		default:
			return
		}
	}
}

// handle 处理单条通知。任何一步失败都只记日志，绝不 panic 逃逸到线程外。
func (s *Service) handle(ev *Event) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultHandleTimeout)
	defer cancel()

	// 计数失败 ⇒ 放弃这条通知（见 CountsProvider 的说明）。
	if s.counts != nil {
		alertCount, evidenceCount, err := s.counts.IncidentCounts(ctx, ev.Incident.ID)
		if err != nil {
			s.logger.Error("事件计数查询失败，放弃该条通知",
				slog.String("kind", string(ev.Kind)),
				slog.Int64("incident_id", ev.Incident.ID),
				slog.Any("error", err))
			return
		}
		ev.AlertCount = alertCount
		ev.EvidenceCount = evidenceCount
	}

	for _, sink := range s.sinks {
		if err := sink.Send(ctx, ev); err != nil {
			s.logger.Warn("通知发送失败",
				slog.String("sink", sink.Name()),
				slog.String("kind", string(ev.Kind)),
				slog.Int64("incident_id", ev.Incident.ID),
				slog.Any("error", err))
		}
	}
}

// Close 停止派发并等待 worker 排空。ctx 到期时返回 ctx.Err()
// （worker 可能仍在发最后几条——sink 有各自超时，不会永久悬挂）。
//
// 幂等：多次调用只关闭一次；首次之后的调用直接等待同一个 workerDone。
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stopped)
	})
	select {
	case <-s.workerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
