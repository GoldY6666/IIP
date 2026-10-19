package correlation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yu/iip/internal/alert"
	"github.com/yu/iip/internal/incident"
	"github.com/yu/iip/internal/metrics"
)

// Result 是单条告警的关联结论。
type Result struct {
	Action     string  // merge / review / new / none
	IncidentID int64   // 命中或新建的 Incident；none 时为 0
	Score      float64 // 关联分数；NEW 档为 0
}

// Service 编排关联：读配置 → 取候选 → 逐对打分 → 三档决策 → 落库。
//
// 依赖方向：api → correlation → repository → db，与 M1 的 internal/alert 一致。
//
// timeline / suppressions 是 M3 挂上来的**可选**能力（见 lifecycle.go）：
// 为 nil 时关联行为与 M2 完全一致，便于既有调用点零改动地升级。
type Service struct {
	incidents IncidentStore
	config    ConfigStore
	topo      *Topology
	logger    *slog.Logger

	// timeline 为 nil 时不做时间线补写（M2 行为）。
	timeline TimelineWriter
	// suppressions 为 nil 时不查抑制（M2 行为）。
	suppressions SuppressionStore
	// evidenceTasks 为 nil 时不入队采集任务（M2 行为，M4 挂上）。
	evidenceTasks EvidenceEnqueuer
	// notifier 为 nil 时不发通知（M2/M3/M4 行为，M6 T6.1 挂上）。
	notifier Notifier
	// metrics 永不为 nil（未注入时为 metrics.Noop()），见 metrics.go。
	metrics *metrics.Metrics
	// now 是可注入时钟；nil 时取 time.Now().UTC()，让「留痕时间」可被精确断言。
	now func() time.Time
	// fallback 是 R7 两段式回退的候选快照源（Service 级共享 + TTL 复用，
	// 实测依据见 candidates.go 的 fallbackSnapshotTTL）。
	fallback *fallbackSource
}

// NewService 构造关联服务。logger 为 nil 时退化为 slog.Default()。
func NewService(incidents IncidentStore, cfgStore ConfigStore, topo *Topology, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		incidents: incidents,
		config:    cfgStore,
		topo:      topo,
		logger:    logger,
		metrics:   metrics.Noop(),
		fallback:  newFallbackSource(incidents, logger, fallbackSnapshotTTL),
	}
}

// CorrelateAlerts 对一批「新落库的告警」做关联（webhook persist 成功后调用）。
//
// 只处理 status='firing'：resolved 不建/不并事件（其生命周期归 M3 状态机）。
//
// 返回 error 仅表示**存储层故障**。调用方（handler）须只记日志——关联失败
// 绝不能让 webhook 失败，否则会触发 Alertmanager 重投、放大风暴（手册 §4.3 铁律）。
func (s *Service) CorrelateAlerts(ctx context.Context, alerts []alert.StoredAlert) error {
	if len(alerts) == 0 {
		return nil
	}

	cfg, err := s.config.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("读取关联配置失败: %w", err)
	}
	if !cfg.Enabled {
		return nil
	}

	candidates, err := s.incidents.ListOpenIncidentAlerts(ctx, CandidateLimitPerIncident)
	if err != nil {
		return fmt.Errorf("读取候选事件失败: %w", err)
	}

	// 按 Incident 聚合候选告警；批次内新建/合并的事件会实时并入，使同批告警可互相累积。
	byIncident := groupByIncident(candidates)
	// 回退源（s.fallback，Service 级共享 + TTL 复用）：限额集内最高分不足
	// merge 阈值且确有事件被限额截断时，按需补取不限额候选集重算，
	// 见 correlateOne 与 candidates.go。

	ref := time.Now().UTC()
	// **历史维不查库**（2026-10-12 R7 实测）：本批告警都是刚落库的，
	// 尚未 link 到任何 incident ⇒ 对「新指纹 × 候选指纹」查共现恒得 co=0。
	// 逐对查 M 次全是白干（实测占 correlate 耗时的大头且随候选数线性增长）。
	// 注入无历史闭包与逐对查询的**结果完全一致**（都是 0），
	// 与 NewScorer 既有的 hist==nil 退化路径同一条路。
	// 若将来需要真实历史维（如回放评估、告警 link 后重关联），
	// 在此处换成 s.config.CoOccurrence 的查表闭包即可。
	noHistory := func(_, _ string) float64 { return 0 }
	scorer := NewScorer(cfg, s.topo, noHistory, ref)

	// 按索引遍历并取址：StoredAlert 184 字节，`for _, a := range` 每轮复制一份
	// （gocritic rangeValCopy）；取址后直接喂给 NewAlertView，省掉第二份复制。
	for i := range alerts {
		a := &alerts[i]
		if a.Status != alert.StatusFiring {
			continue
		}
		v := NewAlertView(a)
		res, err := s.correlateOne(ctx, byIncident, s.fallback, scorer, cfg, v)
		if err != nil {
			return fmt.Errorf("告警 %d 关联失败: %w", v.ID, err)
		}
		s.logger.Info("告警关联完成",
			slog.String("action", res.Action),
			slog.Int64("alert_id", v.ID),
			slog.Int64("incident_id", res.IncidentID),
			slog.Float64("score", res.Score))
	}
	return nil
}

// correlateOne 对单条告警视图执行「取最大分 →（必要时回退重算）→ 三档决策 → 落库」。
func (s *Service) correlateOne(
	ctx context.Context,
	byIncident map[int64][]AlertView,
	fallback *fallbackSource,
	scorer *Scorer,
	cfg Config,
	v AlertView,
) (Result, error) {
	bestScore, bestIncident := bestMatch(byIncident, scorer, v)

	// R7 残留边界收口（2026-10-19）：限额候选集（每事件最近 N 条 + 种子）会藏住
	// 「非种子的老成员」——新告警的最佳匹配落在最近 N 条之外时，集内最高分跌破
	// merge 阈值，自动合并降级为 REVIEW（更极端构造下漏成 NEW 重复事件）。
	// 收口方式是在引擎侧回退一次，而不是在 SQL 里继续加锚点——那会把候选集
	// 重新推回无上限，吞吐从 346 条/s 崩到 ~4.5 条/s（R7 压测实证）。
	//
	// 触发条件两个合取项都不可省：
	//   - 分数项：集内已能自动合并的告警，不白付一次回退查询；
	//   - 截断项：没有任何事件被限额裁过时，集内就是全量，回退毫无意义——
	//     少了它，异质环境里大量 REVIEW 档告警会被拖去全量扫描。
	// 结论只在全量**严格更优**时取代：并发关闭事件等使全量集反而更小的
	// 情况下，限额结论不会被更差的结论覆盖。
	if bestIncident != 0 &&
		bestScore < cfg.Thresholds.Merge &&
		anyIncidentTruncated(byIncident, int(CandidateLimitPerIncident)) {
		fullScore, fullIncident, err := s.recomputeWithFullCandidates(ctx, fallback, byIncident, scorer, v)
		if err != nil {
			return Result{}, err
		}
		s.observeCandidateFallback()
		s.logger.Info("候选限额回退：限额集内不足 merge 且事件被截断，按不限额结论重算",
			slog.Int64("alert_id", v.ID),
			slog.Float64("bounded_score", bestScore),
			slog.Float64("unlimited_score", fullScore),
			slog.Int64("unlimited_incident", fullIncident))
		// 仅在全量严格更优时取代：并发关闭事件等使全量集反而更小的情况下，
		// 限额结论不会被更差的结论覆盖。
		if fullIncident != 0 && fullScore > bestScore {
			bestScore, bestIncident = fullScore, fullIncident
		}
	}

	action := Decide(bestScore, cfg.Thresholds)
	if bestIncident == 0 {
		action = ActionNew // 无候选事件，必然新建（避免 0 分被判成 REVIEW）
	}

	// 抑制命中（M3 §4.5）：该指纹曾被人工判为「不该合入这个事件」，
	// 则本次不再建边，直接降级为 NEW 档另起一个事件。
	// 判据用 fingerprint 而非 alert_id——新告警的 ID 必然不同，按 ID 记的抑制永远命中不了。
	if action == ActionReview && s.isSuppressed(ctx, v.Fingerprint, bestIncident) {
		s.logger.Info("命中 REVIEW 抑制，降级为 NEW 档",
			slog.String("fingerprint", v.Fingerprint),
			slog.Int64("incident_id", bestIncident))
		action = ActionNew
	}

	// 记最终分数（抑制降级之后，见 metrics.go 的说明）。
	s.observeScore(action, bestScore)

	switch action {
	case ActionMerge:
		if err := s.link(ctx, bestIncident, v, &bestScore, false); err != nil {
			return Result{}, err
		}
		s.appendTimeline(ctx, incident.NewAlertLinkedEntry(bestIncident, v.ID, bestScore, false, s.nowUTC()), "merge")
		// 合入后重新入队：新告警可能来自另一个服务实例或另一个节点，
		// 指标形态与原事件不同时，已采到的证据不再完整。
		// 重复采集由 task 侧的窗口游标兜住（只采新增区间）。
		s.enqueueEvidence(ctx, bestIncident, "merge")
		byIncident[bestIncident] = append(byIncident[bestIncident], v)
		return Result{Action: ActionMerge, IncidentID: bestIncident, Score: bestScore}, nil

	case ActionReview:
		if err := s.link(ctx, bestIncident, v, &bestScore, true); err != nil {
			return Result{}, err
		}
		s.appendTimeline(ctx, incident.NewAlertLinkedEntry(bestIncident, v.ID, bestScore, true, s.nowUTC()), "review")
		s.enqueueEvidence(ctx, bestIncident, "review")
		byIncident[bestIncident] = append(byIncident[bestIncident], v)
		return Result{Action: ActionReview, IncidentID: bestIncident, Score: bestScore}, nil

	default:
		inc, err := s.incidents.CreateIncident(ctx, Incident{
			Title:     incidentTitle(v),
			Severity:  v.Severity,
			Service:   v.Service,
			StartedAt: v.StartsAt,
		})
		if err != nil {
			return Result{}, fmt.Errorf("新建事件失败: %w", err)
		}
		// 只数真正建出来的事件（CreateIncident 成功之后）：
		// 放在 error 提前返回之前会让「建失败」也算进事件数，
		// 而那个数字正是用来看「告警风暴有没有被正确聚合」的。
		s.metrics.IncidentCreated.Inc()
		// NEW 档写「事件创建」而不是「告警合入」：这条告警是事件的起因，
		// 语义上不同于「并入一个已存在的事件」，前端要能区分两者。
		s.appendTimeline(ctx, incident.NewAlertFiredEntry(inc.ID, v.ID, v.Fingerprint, s.nowUTC()), "create")
		if err := s.link(ctx, inc.ID, v, nil, false); err != nil {
			return Result{}, err
		}
		// 事件一创建就入队采集：证据的时效性要求是「2 分钟内出现」
		//（手册 DoD 第 4 条），而关联发生在 webhook 请求内 ——
		// 在这里入队是最早的时机，交给 worker 异步采。
		s.enqueueEvidence(ctx, inc.ID, "create")
		byIncident[inc.ID] = append(byIncident[inc.ID], v)
		s.notifyCreated(inc)
		return Result{Action: ActionNew, IncidentID: inc.ID, Score: 0}, nil
	}
}

// recomputeWithFullCandidates 用「不限额快照 ∪ 本批内存累积」重算单条告警的
// 最佳匹配，供 R7 回退路径使用。
//
// 取并集而不是直接用快照：本批在快照之后新建/合入的成员只存在于 byIncident
// （内存累积），不在快照里；并集保证重算面对的是「库全量 + 本批增量」。
// 回退查询本身失败时向上冒泡（存储故障语义与限额查询一致）。
func (s *Service) recomputeWithFullCandidates(
	ctx context.Context,
	fallback *fallbackSource,
	byIncident map[int64][]AlertView,
	scorer *Scorer,
	v AlertView,
) (score float64, incidentID int64, err error) {
	snapshot, err := fallback.unlimited(ctx)
	if err != nil {
		return 0, 0, err
	}
	full := mergeViews(byIncident, snapshot)
	score, incidentID = bestMatch(full, scorer, v)
	return score, incidentID, nil
}

// link 建立关联边并回写告警的 incident_id 反规范化指针。
//
// v.StartsAt 是分区键（T7.1 / ADR-022）：复合 FK 要求它与 alerts 行
// 精确匹配，而 AlertView 正是从刚落库的告警行构造的，天然一致。
func (s *Service) link(ctx context.Context, incidentID int64, v AlertView, score *float64, needsReview bool) error {
	if err := s.incidents.LinkAlertToIncident(ctx, incidentID, v.ID, v.StartsAt, score, needsReview); err != nil {
		return fmt.Errorf("关联告警 %d → 事件 %d 失败: %w", v.ID, incidentID, err)
	}
	if err := s.incidents.SetAlertIncident(ctx, v.ID, incidentID); err != nil {
		return fmt.Errorf("回写告警 %d 的 incident_id 失败: %w", v.ID, err)
	}
	return nil
}

// incidentTitle 由告警派生事件标题：优先 alertname，附 service/namespace 便于辨识。
func incidentTitle(v AlertView) string {
	name := strings.TrimSpace(v.Labels["alertname"])
	if name == "" {
		name = "未命名告警"
	}
	switch {
	case v.Service != "":
		return name + " (" + v.Service + ")"
	case v.Namespace != "":
		return name + " (" + v.Namespace + ")"
	case v.Fingerprint != "":
		fp := v.Fingerprint
		if len(fp) > 12 {
			fp = fp[:12]
		}
		return name + " #" + fp
	default:
		return name
	}
}
