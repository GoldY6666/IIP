package correlation

import (
	"context"
	"log/slog"
	"time"

	"github.com/yu/iip/internal/incident"
)

// 本文件把 M3 的两项能力接到 M2 的关联路径上：**时间线补写**与**REVIEW 抑制**。
//
// 依赖方向说明：本包 import internal/incident（复用 Entry 与 EventType 常量），
// 反向不存在——incident 包不认识 correlation（手册 §4.6 的依赖不变式）。
// 这样「告警合入事件」与「状态流转」共用同一套时间线类型，前端只需认一套 event_type。
//
// 两个能力都做成**可选**（nil 即关闭），理由有二：
//   1. 既有 NewService 的调用点（服务装配、M2 全部单测）一行都不用改，回归面最小；
//   2. 留痕与抑制都不是关联的主责——它们失败时只记日志，绝不阻断告警入库
//      （手册 §4.3 铁律：关联失败不能让 webhook 失败，否则会触发 Alertmanager 重投）。

// TimelineWriter 是「往事件时间线补一条留痕」的能力。
//
// 由装配层注入 internal/repository.Lifecycle（它已实现该方法）；
// 单测里注入内存替身即可。
type TimelineWriter interface {
	AppendTimelineEntry(ctx context.Context, e incident.Entry) error
}

// SuppressionStore 是 REVIEW 拒绝后的抑制记录（手册 §4.5）。
//
// 抑制键是 (fingerprint, incident_id) 而非 alert_id 对——新告警到来时 ID 必然不同，
// 按 ID 记的抑制永远命中不了第二次。用 fingerprint 才能跨次复用。
type SuppressionStore interface {
	// IsSuppressed 报告该指纹是否已被该事件拒绝过。
	IsSuppressed(ctx context.Context, fingerprint string, incidentID int64) (bool, error)
	// AddSuppression 写一条抑制行；重复写入应静默跳过（唯一约束 + ON CONFLICT）。
	AddSuppression(ctx context.Context, fingerprint string, incidentID int64, actor, reason string) error
}

// WithTimeline 挂上时间线补写能力，返回自身以便链式装配。
func (s *Service) WithTimeline(w TimelineWriter) *Service {
	s.timeline = w
	return s
}

// WithSuppressions 挂上 REVIEW 抑制能力，返回自身以便链式装配。
func (s *Service) WithSuppressions(st SuppressionStore) *Service {
	s.suppressions = st
	return s
}

// appendTimeline 写一条留痕；失败**只记日志**。
//
// 绝不返回错误：留痕是旁支，把它变成错误会让「告警已正常入库并关联成功」
// 整条链路被判定为失败，进而触发上游重投——比少一条留痕严重得多。
func (s *Service) appendTimeline(ctx context.Context, e incident.Entry, what string) {
	if s.timeline == nil {
		return
	}
	if err := s.timeline.AppendTimelineEntry(ctx, e); err != nil {
		s.logger.Warn("时间线留痕写入失败（不影响关联结果）",
			slog.String("what", what),
			slog.Int64("incident_id", e.IncidentID),
			slog.String("event_type", string(e.EventType)),
			slog.String("error", err.Error()))
	}
}

// isSuppressed 查询抑制；查询失败**按未抑制处理**。
//
// 与留痕同理：查不到抑制只是「可能多产生一条待确认边」，代价远小于
// 让整批告警关联失败。故失败时返回 false 并记 WARN。
func (s *Service) isSuppressed(ctx context.Context, fingerprint string, incidentID int64) bool {
	if s.suppressions == nil {
		return false
	}
	hit, err := s.suppressions.IsSuppressed(ctx, fingerprint, incidentID)
	if err != nil {
		s.logger.Warn("抑制查询失败，按未抑制处理",
			slog.String("fingerprint", fingerprint),
			slog.Int64("incident_id", incidentID),
			slog.String("error", err.Error()))
		return false
	}
	return hit
}

// nowUTC 是关联路径取时刻的唯一入口，便于测试注入固定时钟。
func (s *Service) nowUTC() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now()
}
