// Package correlation 实现 IIP 的关联引擎（Correlation Engine）。
//
// 它把一条新告警与既有 Incident 的告警逐对打分（五维加权），再按阈值做三档决策
// （MERGE / REVIEW / NEW），并把结果落库。
//
// 规格依据：docs/IIP-M2详细开发手册.md §四；算法源自 V1.0 §5。
//
// 分层：本包只做「评分 + 决策 + 编排」，SQL 在 internal/repository，
// HTTP 契约在 internal/api —— 与 M1 的 internal/alert 保持同一依赖方向。
package correlation

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yu/iip/internal/alert"
)

// 三档决策结果（外加「不处理」）。
const (
	// ActionMerge：score ≥ merge 阈值，直接合并到既有 Incident。
	ActionMerge = "merge"
	// ActionReview：review 阈值 ≤ score < merge 阈值，关联但标记待人工确认。
	ActionReview = "review"
	// ActionNew：score < review 阈值，新建 Incident。
	ActionNew = "new"
	// ActionNone：非 firing 或引擎关闭，跳过。
	ActionNone = "none"
)

// Weights 是五维权重，需满足 Σ ≈ 1.0。
type Weights struct {
	Time       float64 `json:"time"`
	Service    float64 `json:"service"`
	Label      float64 `json:"label"`
	Dependency float64 `json:"dependency"`
	Historical float64 `json:"historical"`
}

// sum 返回五维权重之和，用于校验。
func (w Weights) sum() float64 {
	return w.Time + w.Service + w.Label + w.Dependency + w.Historical
}

// Thresholds 是决策阈值；merge 必须 ≥ review。
type Thresholds struct {
	Merge  float64 `json:"merge"`
	Review float64 `json:"review"`
}

// Config 是关联引擎的可配置参数（存于 correlation_rules 单行 rule_config）。
type Config struct {
	Weights    Weights    `json:"weights"`
	Thresholds Thresholds `json:"thresholds"`
	// Enabled 为 false 时引擎整体停用（webhook 落库后不关联）。
	Enabled bool `json:"enabled"`
}

// 权重和的允许误差：JSON 浮点往返会引入极小偏差，严格 == 1.0 会误杀合法配置。
const weightSumEpsilon = 0.001

// DefaultConfig 返回 V1.0 §5 的默认权重与阈值（与 0003 迁移的种子值一致）。
func DefaultConfig() Config {
	return Config{
		Weights: Weights{
			Time:       0.25,
			Service:    0.25,
			Label:      0.20,
			Dependency: 0.20,
			Historical: 0.10,
		},
		Thresholds: Thresholds{Merge: 0.80, Review: 0.50},
		Enabled:    true,
	}
}

// Validate 校验权重与阈值的合法区间，非法返回可读错误（供配置 API 回 422）。
func (c Config) Validate() error {
	for name, v := range map[string]float64{
		"weights.time":       c.Weights.Time,
		"weights.service":    c.Weights.Service,
		"weights.label":      c.Weights.Label,
		"weights.dependency": c.Weights.Dependency,
		"weights.historical": c.Weights.Historical,
		"thresholds.merge":   c.Thresholds.Merge,
		"thresholds.review":  c.Thresholds.Review,
	} {
		if v < 0 || v > 1 {
			return fmt.Errorf("%s 必须在 [0,1] 区间，当前为 %g", name, v)
		}
	}
	if diff := c.Weights.sum() - 1.0; diff > weightSumEpsilon || diff < -weightSumEpsilon {
		return fmt.Errorf("五维权重之和必须为 1.0（允许误差 %g），当前为 %g", weightSumEpsilon, c.Weights.sum())
	}
	if c.Thresholds.Merge < c.Thresholds.Review {
		return fmt.Errorf("merge 阈值 (%g) 不能小于 review 阈值 (%g)", c.Thresholds.Merge, c.Thresholds.Review)
	}
	return nil
}

// MarshalConfigJSON 把配置序列化为入库形态（jsonb）。
func MarshalConfigJSON(c Config) ([]byte, error) {
	// 只持久化 weights/thresholds；enabled 由 correlation_rules.enabled 列承载，
	// 但一并写入 rule_config 便于读端自洽。
	return json.Marshal(c)
}

// UnmarshalConfigJSON 解析入库的 rule_config；字段缺失时以默认值补齐，
// 避免早期/手工写入的残缺配置让引擎整体失效。
func UnmarshalConfigJSON(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析关联配置失败: %w", err)
	}
	return cfg, nil
}

// DimensionScores 是五维相似度的取值容器。
type DimensionScores struct {
	Time       float64
	Service    float64
	Label      float64
	Dependency float64
	Historical float64
}

// DimensionDynamics 是评分中用到的动态 label 键（与指纹排除键一致）：
// 这些键随实例/容器变化，参与 Jaccard 会稀释真实相似度。
var dynamicLabelKeys = map[string]struct{}{
	"instance":  {},
	"pod":       {},
	"container": {},
	"node":      {},
}

// AlertView 是评分所需的告警视图（从 alert.StoredAlert 投影而来）。
//
// 单独定义而非直接用 alert.StoredAlert：评分是纯函数，视图只带算分所需字段，
// 既解耦又便于表驱动单测（无需构造 json.RawMessage）。
type AlertView struct {
	ID          int64
	Fingerprint string
	Severity    string
	Service     string
	Namespace   string
	Labels      map[string]string
	StartsAt    time.Time
	EndsAt      *time.Time
}

// NewAlertView 从落库告警投影出评分视图。
func NewAlertView(a alert.StoredAlert) AlertView {
	labels := parseLabels(a.Labels)
	return AlertView{
		ID:          a.ID,
		Fingerprint: a.Fingerprint,
		Severity:    a.Severity,
		Service:     labels["service"],
		Namespace:   labels["namespace"],
		Labels:      labels,
		StartsAt:    a.StartsAt,
		EndsAt:      a.EndsAt,
	}
}

// ParseLabels 把 jsonb 形态的 labels 解析为 map（供仓储层构造 AlertView 复用）。
func ParseLabels(raw json.RawMessage) map[string]string {
	return parseLabels(raw)
}

// parseLabels 把 jsonb 形态的 labels 解析为 map[string]string。
//
// 解析失败或非对象一律退化为空 map：labels 脏不应让关联整体失败，
// 无非是相似度偏低（并在日志侧由调用方按需观测）。
func parseLabels(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]string{}
	}
	return m
}
