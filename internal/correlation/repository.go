package correlation

import (
	"context"
	"time"
)

// Incident 是关联引擎视角的 Incident（只带判定与建档所需字段）。
type Incident struct {
	ID        int64
	Title     string
	Severity  string
	Status    string
	Service   string
	StartedAt time.Time
}

// CandidateAlert 是候选集中一条「已关联告警」：评分视图 + 归属 Incident。
type CandidateAlert struct {
	IncidentID int64
	View       AlertView
}

// IncidentStore 是关联引擎所需的 Incident 持久化能力。
//
// 定义在**消费方**（本包），由 internal/repository 实现；单测可注入假实现，
// 与 M1 的 alert.Repository 保持同一风格。
type IncidentStore interface {
	// CreateIncident 新建 Incident（NEW 档）。
	CreateIncident(ctx context.Context, in Incident) (Incident, error)
	// ListOpenIncidentAlerts 取所有 open/investigating Incident 的已关联告警。
	//
	// perIncidentLimit 限定**每个 incident 最多返回多少条**（<=0 表示不限额）。
	// 必须限额：候选集 = 全部 open incident 的成员总数，无上限时它随告警量
	// 只增不减 —— 实测 1 万条告警时候选集 7555 条，单次查询 11ms、
	// 每条新告警还要与全部成员逐一评分，吞吐从 1785 条/s 崩到 ~4.5 条/s。
	// 调用方传 CandidateLimitPerIncident。
	ListOpenIncidentAlerts(ctx context.Context, perIncidentLimit int32) ([]CandidateAlert, error)
	// LinkAlertToIncident 关联告警到 Incident（幂等）。
	// score 为 nil 表示无关联对象（NEW 档）；needsReview=true 表示 REVIEW 档。
	LinkAlertToIncident(ctx context.Context, incidentID, alertID int64, score *float64, needsReview bool) error
	// SetAlertIncident 回写 alerts.incident_id（反规范化指针）。
	SetAlertIncident(ctx context.Context, alertID, incidentID int64) error
}

// CandidateLimitPerIncident 是每个 open incident 进入候选集的最大告警条数。
//
// 20 条的取值依据（2026-10-12 实测）：限额后候选集 ≤ incident 数 × 20。
// 压测场景 8 个 incident ⇒ 160 条候选，相比无上限的 7555 条降 47 倍，
// 且保留了每个事件最近 20 条告警的形态（时间窗/服务/标签），
// 足以支撑服务名与时间窗这两维的判定。
//
// 改这个常量即可调整，不必改数据库 schema（它不进 rule_config，
// 因为它是**性能参数**而非关联规则 —— 混在一起会让「调权重」意外改变负载）。
const CandidateLimitPerIncident int32 = 20

// ConfigStore 是配置读写与历史共现查询能力。
type ConfigStore interface {
	// GetConfig 读单行配置；缺失时由实现回退到 DefaultConfig()。
	GetConfig(ctx context.Context) (Config, error)
	// PutConfig 写单行配置（幂等 upsert）。
	PutConfig(ctx context.Context, cfg Config) error
	// CoOccurrence 返回两个 fingerprint 的共现计数（co=同时出现，total=并集）。
	//
	// **实时 webhook 路径不调用它**（2026-10-12 R7 实测）：新落库的告警尚未
	// link 到任何 incident，对「新指纹 × 候选指纹」恒返回 co=0 ⇒ 逐对查询
	// 是纯浪费。CorrelateAlerts 因此注入「无历史」闭包（与查询结果一致）。
	// 本方法保留给回放评估等「双方均已有历史」的场景。
	CoOccurrence(ctx context.Context, fpA, fpB string) (co, total float64, err error)
}
