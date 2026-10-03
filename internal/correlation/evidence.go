package correlation

import (
	"context"
	"log/slog"

	"github.com/yu/iip/internal/task"
)

// 本文件把 M4 的**证据采集入队**接到 M2 的关联路径上：
// 事件创建（NEW 档）与告警合入（MERGE / REVIEW 档）后各入队一个采集任务。
//
// 与 M3 的 timeline / suppressions 完全同构（见 lifecycle.go）：
// 可选钩子（nil 即关闭）、失败只记日志、绝不返回错误。
//
// 为什么入队失败绝不能阻断关联
// ------------------------------
// 关联发生在 webhook 请求内。关联失败会让 handler 返回错误 → Alertmanager
// 判定投递失败 → **重投整批告警** → 放大成风暴。
// 入队失败的后果只是「这个事件暂时没有证据」，而证据会在下一轮
// 由 worker 的超时回收 + 重试补上；两相比较，阻断的代价大得多。

// EvidenceEnqueuer 是「给事件入队一个证据采集任务」的能力。
//
// 由装配层注入 repository.Tasks（它已实现 task.Store）；单测注入内存替身。
type EvidenceEnqueuer interface {
	// EnqueueEvidence 为 incident 入队一个证据采集任务。
	//
	// 幂等由实现保证（同事件已有未完成的任务时不重复入队）。
	// 重复入队不会出错，但会让队列堆积并浪费一次采集。
	EnqueueEvidence(ctx context.Context, incidentID int64) error
}

// WithEvidenceTasks 挂上证据采集入队能力，返回自身以便链式装配。
func (s *Service) WithEvidenceTasks(e EvidenceEnqueuer) *Service {
	s.evidenceTasks = e
	return s
}

// enqueueEvidence 入队一个采集任务；未装配或失败都**只记日志**。
//
// 与 appendTimeline 同理：绝不返回错误，理由见本文件头的说明。
//
// 只对 incidentID > 0 调用：ActionNone 档没有目标事件，
// 而 incident_id=0 会让任务参数非法、入队直接失败并刷一条无意义的告警日志。
func (s *Service) enqueueEvidence(ctx context.Context, incidentID int64, what string) {
	if s.evidenceTasks == nil || incidentID <= 0 {
		return
	}
	if err := s.evidenceTasks.EnqueueEvidence(ctx, incidentID); err != nil {
		s.logger.Warn("证据采集任务入队失败（不影响关联结果）",
			slog.String("what", what),
			slog.Int64("incident_id", incidentID),
			slog.String("kind", string(task.KindCollectEvidence)),
			slog.String("error", err.Error()))
	}
}
