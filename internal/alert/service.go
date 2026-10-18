package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yu/iip/internal/metrics"
)

// outcomeKind 是单条告警的处理归类，与 Result 的四个计数字段一一对应。
type outcomeKind int

const (
	outcomeAccepted outcomeKind = iota
	outcomeSkipped
	outcomeUpdated
	outcomeRejected
)

// itemOutcome 是单条告警的处理结论；rejected 时带原因。
type itemOutcome struct {
	kind   outcomeKind
	reason string
}

// Correlator 是 M2 关联引擎在 ingest 完成后的挂载点（由 correlation.Service 实现）。
//
// 接口定义在**消费方**（本包），使 internal/alert 不反向依赖 internal/correlation，
// 依赖方向仍是 alert ← correlation（只向下）。
type Correlator interface {
	CorrelateAlerts(ctx context.Context, alerts []StoredAlert) error
}

// Service 编排 M1 的五阶段流水线：
//
//	Validate → Normalize → Fingerprint → Deduplicate → Persist
//
// 它只做编排与计数：HTTP 解析在 internal/api，SQL 在 internal/repository。
// 这样一层的服务就能被完整单测覆盖（注入假 Repository 即可）。
//
// M2 起，落库成功的新 firing 告警会顺带触发关联（correlator）；
// 未注入 correlator 时行为与 M1 完全一致。
type Service struct {
	repo       Repository
	logger     *slog.Logger
	correlator Correlator
	// metrics 永不为 nil（未注入时是 metrics.Noop()），故埋点处无需 nil 判断。
	metrics *metrics.Metrics
}

// NewService 构造 Service。logger 传 nil 时退化为 slog.Default()，
// 避免调用方为了日志装配而多写分支。
func NewService(repo Repository, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, metrics: metrics.Noop()}
}

// SetMetrics 注入自监控指标（M6 T6.3）。传 nil 等价于不采集。
//
// 用 setter 而非改造 NewService 签名：与 SetCorrelator 同理 ——
// 保持 M1 起的所有调用点与测试零改动。
func (s *Service) SetMetrics(m *metrics.Metrics) {
	if m == nil {
		m = metrics.Noop()
	}
	s.metrics = m
}

// SetCorrelator 注入 M2 关联引擎。传 nil 等价于关闭关联。
//
// 用 setter 而非改造 NewService 签名：既保持 M1 既有调用/测试不变，
// 也让关联成为可选装配（未装配时行为与 M1 一致）。
func (s *Service) SetCorrelator(c Correlator) {
	s.correlator = c
}

// List 取最新一批告警，直接委托给仓储（排序、过滤策略见 Repository.List 与 SQL 定义）。
//
// 放在 Service 而非让 handler 直连 repo：保持 api → alert → repository 的依赖方向，
// handler 只依赖本包定义的窄接口（见 internal/api 的 AlertQuerier），单测可注入假实现。
func (s *Service) List(ctx context.Context, limit, offset int32) ([]StoredAlert, error) {
	return s.repo.List(ctx, limit, offset)
}

// Ingest 处理一批告警，返回计数结果。
//
// 单条失败**不阻断整批**：该条计入 Result.Rejected 并附原因，同批其他告警照常处理。
// 只有底层存储不可用才返回 error —— 那时 handler 会回 503，从而触发 Alertmanager 重试，
// 这是唯一正确的行为（把告警丢掉才是灾难）。
func (s *Service) Ingest(ctx context.Context, p *Payload) (Result, error) {
	var res Result
	if p == nil {
		return res, errors.New("payload 为空")
	}

	// 收集本次**新落库的 firing 告警**，供 M2 关联使用。
	var newFiring []StoredAlert
	for i, raw := range p.Alerts {
		outcome, stored, err := s.ingestOne(ctx, raw, p.Status)
		if err != nil {
			// 存储层故障：这一条连「结论」都没判出来（既不是 duplicate 也不是
			// invalid），故单独计 error —— 否则失败告警在指标上完全不可见，
			// 而「接入量突然变少」与「接入量本来就这么少」将无法区分。
			s.metrics.AlertIngest.WithLabelValues(metrics.IngestError).Inc()
			return res, fmt.Errorf("处理第 %d 条告警失败: %w", i, err)
		}
		if stored != nil {
			newFiring = append(newFiring, *stored)
		}
		s.metrics.AlertIngest.WithLabelValues(ingestResultLabel(outcome.kind)).Inc()
		res.record(i, outcome)
	}

	s.correlate(ctx, newFiring)
	return res, nil
}

// correlate 触发 M2 关联；**失败只记日志，绝不冒泡**。
//
// 关联是告警接入的下游增强：若它出错导致 webhook 回 503，Alertmanager 会重投，
// 反而放大风暴。宁可这一轮不关联，也不能让接入链路受影响（手册 §4.3 铁律）。
func (s *Service) correlate(ctx context.Context, alerts []StoredAlert) {
	if s.correlator == nil || len(alerts) == 0 {
		return
	}
	if err := s.correlator.CorrelateAlerts(ctx, alerts); err != nil {
		s.logger.Error("告警关联失败（不影响接入）",
			slog.String("error", err.Error()),
			slog.Int("alert_count", len(alerts)))
	}
}

// record 把单条结论累加进 Result，并保留 rejected 的原因。
func (r *Result) record(index int, outcome itemOutcome) {
	switch outcome.kind {
	case outcomeAccepted:
		r.Accepted++
	case outcomeSkipped:
		r.Skipped++
	case outcomeUpdated:
		r.Updated++
	case outcomeRejected:
		r.Rejected++
		r.Errors = append(r.Errors, ItemError{Index: index, Error: outcome.reason})
	}
}

// ingestOne 走完单条告警的全部阶段。
//
// 返回的 error 仅表示**存储层故障**（应升级为整批失败）；字段级问题一律
// 返回 outcomeRejected + nil，从而不牵连同批其他告警。
//
// 第二个返回值仅在「本批次新插入了一行」时非 nil（供 M2 关联使用）；
// 跳过 / 更新 / 拒绝的情形均为 nil。
func (s *Service) ingestOne(ctx context.Context, raw json.RawMessage, topStatus string) (itemOutcome, *StoredAlert, error) {
	// 阶段一：解码 + 字段级校验
	rawAlert, err := DecodeRawAlert(raw)
	if err != nil {
		return itemOutcome{kind: outcomeRejected, reason: err.Error()}, nil, nil
	}
	if err = ValidateAlert(&rawAlert); err != nil {
		return itemOutcome{kind: outcomeRejected, reason: err.Error()}, nil, nil
	}

	// 阶段二 + 三：归一化（内部即用 GenerateFingerprint 计算指纹）
	normalized, warnings, err := Normalize(&rawAlert, raw, topStatus)
	if err != nil {
		return itemOutcome{kind: outcomeRejected, reason: err.Error()}, nil, nil
	}
	for _, w := range warnings {
		s.logger.Warn("告警归一化提示",
			slog.String("fingerprint", normalized.Fingerprint),
			slog.String("detail", w))
	}

	// 阶段四 + 五：去重判定与持久化
	return s.persist(ctx, &normalized)
}

// ingestResultLabel 把单条结论映射成 iip_alert_ingest_total 的 result 取值。
//
// stored 同时覆盖「新插入」与「把已有 firing 置为 resolved」：两者都是
// 库被成功写入，而运维关心的是二分（写进去了 vs 没写）。合并的正当性
// 由不变量背书 —— dedup{hit,miss} 之和恒等于 ingest{stored,duplicate} 之和。
func ingestResultLabel(k outcomeKind) string {
	switch k {
	case outcomeAccepted, outcomeUpdated:
		return metrics.IngestStored
	case outcomeSkipped:
		return metrics.IngestDuplicate
	case outcomeRejected:
		return metrics.IngestInvalid
	}
	// 不可达（outcomeKind 只有四个值），但返回 invalid 比 panic 安全：
	// 指标埋点永远不该成为接入链路的故障点。
	return metrics.IngestInvalid
}

// persist 实现手册 §4.4 的分支表：按「入站状态 × 库中最新一条状态」决定动作。
func (s *Service) persist(ctx context.Context, a *Alert) (itemOutcome, *StoredAlert, error) {
	latest, err := s.repo.LatestByFingerprint(ctx, a.Fingerprint)
	if errors.Is(err, ErrNotFound) {
		s.metrics.AlertDedup.WithLabelValues(metrics.DedupMiss).Inc()
		return s.persistWithoutHistory(ctx, a)
	}
	if err != nil {
		// 查询失败≠去重判定：这一条既没命中也没落空，不计入 dedup，
		// 否则 hit+miss 之和会超过实际进入去重判定的条数，比率就失真了。
		return itemOutcome{}, nil, err
	}
	s.metrics.AlertDedup.WithLabelValues(metrics.DedupHit).Inc()
	return s.persistAgainstHistory(ctx, a, &latest)
}

// persistWithoutHistory 处理「该指纹在库中没有任何历史行」。
func (s *Service) persistWithoutHistory(ctx context.Context, a *Alert) (itemOutcome, *StoredAlert, error) {
	if a.Status == StatusResolved {
		// 孤立 resolved：M1 上线前该告警已在起火，或上游乱序/重复投递。
		// 不插入「只有结束、没有开始」的行——它会污染时间线，也无法参与后续关联。
		s.logger.Warn("收到无对应 firing 的 resolved 告警，已跳过",
			slog.String("fingerprint", a.Fingerprint))
		return itemOutcome{kind: outcomeSkipped}, nil, nil
	}
	return s.insert(ctx, a)
}

// persistAgainstHistory 处理「库中已有历史行」的三种组合。
func (s *Service) persistAgainstHistory(ctx context.Context, a *Alert, latest *StoredAlert) (itemOutcome, *StoredAlert, error) {
	switch a.Status {
	case StatusFiring:
		if latest.Status == StatusFiring {
			// 同一指纹的 firing 已在库中——V1.0 §4.2 的「跳过」。
			s.logger.Debug("重复的 firing 告警，已跳过", slog.String("fingerprint", a.Fingerprint))
			return itemOutcome{kind: outcomeSkipped}, nil, nil
		}
		// 已 resolved 的旧行之后重新起火：插新行，保留历史（否则会覆盖掉故障记录）。
		return s.insert(ctx, a)

	case StatusResolved:
		if latest.Status != StatusFiring {
			s.logger.Debug("重复的 resolved 告警，已跳过", slog.String("fingerprint", a.Fingerprint))
			return itemOutcome{kind: outcomeSkipped}, nil, nil
		}
		endsAt := a.EndsAt
		if endsAt == nil {
			// 上游未给结束时间：以接收时刻兜底，避免已恢复的告警永远停在 firing。
			now := time.Now().UTC()
			endsAt = &now
		}
		if err := s.repo.Resolve(ctx, latest.ID, endsAt); err != nil {
			return itemOutcome{}, nil, err
		}
		s.logger.Info("告警已置为 resolved",
			slog.Int64("alert_id", latest.ID),
			slog.String("fingerprint", a.Fingerprint))
		return itemOutcome{kind: outcomeUpdated}, nil, nil

	default:
		// ValidateAlert 已把 status 限定为 firing / resolved，此处纯属防御。
		return itemOutcome{
			kind:   outcomeRejected,
			reason: fmt.Sprintf("不支持的 status: %q", a.Status),
		}, nil, nil
	}
}

// insert 落库；命中唯一约束被跳过时计入 skipped（并发去重兜底生效的信号）。
func (s *Service) insert(ctx context.Context, a *Alert) (itemOutcome, *StoredAlert, error) {
	stored, inserted, err := s.repo.Insert(ctx, a)
	if err != nil {
		return itemOutcome{}, nil, err
	}
	if !inserted {
		// 判 miss 之后插入时撞了唯一约束：并发去重兜底真的生效了。
		// 单独计 race（miss 的子集，不参与 hit+miss 的等式），
		// 这样「判 miss 却插不进去」的次数不会淹没在正常 miss 里 ——
		// 它是唯一能证明「两个 webhook 同时到了」的信号。
		s.metrics.AlertDedup.WithLabelValues(metrics.DedupRace).Inc()
		s.logger.Warn("插入被唯一约束跳过（并发去重兜底生效）",
			slog.String("fingerprint", a.Fingerprint))
		return itemOutcome{kind: outcomeSkipped}, nil, nil
	}
	s.logger.Info("告警已落库",
		slog.Int64("alert_id", stored.ID),
		slog.String("fingerprint", a.Fingerprint),
		slog.String("severity", a.Severity))
	// 返回落库行（带 ID）：它是 M2 关联的输入。
	return itemOutcome{kind: outcomeAccepted}, &stored, nil
}
