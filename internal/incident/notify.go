package incident

import (
	"github.com/yu/iip/internal/notify"
)

// Notifier 是「状态变更」通知的派发能力（T6.1，ADR-024）。
//
// 接口定义在消费方：本包只依赖这一个方法，单测注入内存替身即可。
// notify.Service 天然满足它，且其方法对 nil 接收者安全。
type Notifier interface {
	NotifyStatusChanged(inc notify.IncidentRef, from, to, actor string) bool
}

// WithNotifier 挂上通知能力，返回自身以便链式装配。nil = 不通知（默认）。
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notifier = n
	return s
}

// notifyChanged 在状态流转**提交成功后**派发通知。
//
// 必须放在事务之外：事务内发通知会让「回滚」与「已通知」并存，
// 用户看到一条并未发生的流转；而放进事务又会把外部系统的耗时
// 拉进数据库事务持有期。放在 InTx 返回 nil 之后是唯一正确的位置。
//
// 错误绝不上行：通知是旁路（ADR-024），失败只影响「这次没收到消息」。
// 取 *Snapshot：Snapshot 恰在 ADR-012 的 128 B 阈值上，
// 按「真正超重入参改指针」的裁定处理（gocritic hugeParam）。
func (s *Service) notifyChanged(snap *Snapshot, from, to Status, actor string) {
	if s.notifier == nil {
		return
	}
	s.notifier.NotifyStatusChanged(notify.IncidentRef{
		ID:        snap.ID,
		Title:     snap.Title,
		Severity:  snap.Severity,
		Service:   snap.Service,
		Status:    string(snap.Status),
		StartedAt: snap.StartedAt,
	}, string(from), string(to), actor)
}
