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
	ListOpenIncidentAlerts(ctx context.Context) ([]CandidateAlert, error)
	// LinkAlertToIncident 关联告警到 Incident（幂等）。
	// score 为 nil 表示无关联对象（NEW 档）；needsReview=true 表示 REVIEW 档。
	LinkAlertToIncident(ctx context.Context, incidentID, alertID int64, score *float64, needsReview bool) error
	// SetAlertIncident 回写 alerts.incident_id（反规范化指针）。
	SetAlertIncident(ctx context.Context, alertID, incidentID int64) error
}

// ConfigStore 是配置读写与历史共现查询能力。
type ConfigStore interface {
	// GetConfig 读单行配置；缺失时由实现回退到 DefaultConfig()。
	GetConfig(ctx context.Context) (Config, error)
	// PutConfig 写单行配置（幂等 upsert）。
	PutConfig(ctx context.Context, cfg Config) error
	// CoOccurrence 返回两个 fingerprint 的共现计数（co=同时出现，total=并集）。
	CoOccurrence(ctx context.Context, fpA, fpB string) (co, total float64, err error)
}
