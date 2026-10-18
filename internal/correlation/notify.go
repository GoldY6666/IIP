package correlation

import (
	"github.com/yu/iip/internal/notify"
)

// Notifier 是「事件创建」通知的派发能力（T6.1，ADR-024）。
//
// 接口定义在**消费方**而非 notify 包：本包只依赖它用到的那一个方法，
// 单测注入内存替身即可，不需要起派发器；notify.Service 天然满足它。
type Notifier interface {
	NotifyIncidentCreated(inc notify.IncidentRef) bool
}

// WithNotifier 挂上通知能力，返回自身以便链式装配。nil = 不通知（默认）。
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notifier = n
	return s
}

// notifyCreated 在 NEW 档建档并关联成功后派发通知。
//
// 时机放在 link 之后而不是 CreateIncident 之后：事件建好但告警没关联上时，
// 通知里的「关联告警 N 条」会与事实不符——这种通知比不发更误导。
//
// 绝不返回错误、绝不影响关联结果：notify.Service 的 Notify 是非阻塞入队，
// 这里再包一层 nil 判断只是为了让「没配 webhook」的装配不产生调用。
func (s *Service) notifyCreated(inc Incident) {
	if s.notifier == nil {
		return
	}
	s.notifier.NotifyIncidentCreated(notify.IncidentRef{
		ID:        inc.ID,
		Title:     inc.Title,
		Severity:  inc.Severity,
		Service:   inc.Service,
		Status:    inc.Status,
		StartedAt: inc.StartedAt,
	})
}
