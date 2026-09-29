package correlation

import (
	"math"
	"time"
)

// HistoricalFn 提供两个 fingerprint 的历史共现比例（无历史返回 0）。
//
// 抽象成函数是为了让评分内核与 DB 解耦：DB 服务注入真实查询，
// 回放评估注入恒 0（冷启动无历史），单测注入固定值。
type HistoricalFn func(fpA, fpB string) float64

// Scorer 绑定配置、拓扑、历史数据源与参考时刻，对任意一对告警打分。
type Scorer struct {
	cfg  Config
	topo *Topology
	hist HistoricalFn
	ref  time.Time
}

// NewScorer 构造评分器。hist 为 nil 时退化为「无历史」（恒 0）。
func NewScorer(cfg Config, topo *Topology, hist HistoricalFn, ref time.Time) *Scorer {
	if hist == nil {
		hist = func(_, _ string) float64 { return 0 }
	}
	return &Scorer{cfg: cfg, topo: topo, hist: hist, ref: ref}
}

// scoreScale 是总分的量化尺度（保留 6 位小数）。
//
// 不能量化到 4 位：五维权重虽只到两位小数，但时间维是「重叠/时长」、标签维是 Jaccard，
// 都是比值（有理数），总分本身可以有更多位小数。
// 取 6 位既远低于任何有意义的判定分辨率（默认阈值间隔 0.30），
// 又足以抹掉浮点加法的 1e-16 级毛刺。
const scoreScale = 1e6

// CalculateScore 按权重把五维相似度加权求和，夹紧到 [0,1]，再量化到 6 位小数。
//
// 量化不是美化，而是修一个真实踩到的边界缺陷：默认阈值 merge=0.80 与
// 「时间 0.25 + 服务 0.25 + 标签 0.20 + 历史 0.10」的数学结果**恰好相等**，
// 但 IEEE 754 加法会算出 0.7999999999999999（差 1 ulp），
// 使数学上恰好达阈、按 V1.0 §5.3 的 `>=` 语义应判 MERGE 的组合被误判为 REVIEW。
// 统一量化后，得分在日志、DECIMAL(5,4) 落库与阈值判定三处表现一致且可复现。
func CalculateScore(d DimensionScores, w Weights) float64 {
	score := w.Time*d.Time +
		w.Service*d.Service +
		w.Label*d.Label +
		w.Dependency*d.Dependency +
		w.Historical*d.Historical
	// 权重和≈1、各维∈[0,1]，理论结果已在 [0,1]；夹紧只为消除浮点毛刺。
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return math.Round(score*scoreScale) / scoreScale
}

// Dimensions 计算一对告警的五维相似度。
func (s *Scorer) Dimensions(a, b AlertView) DimensionScores {
	return DimensionScores{
		Time:       TimeSimilarity(a, b, s.ref),
		Service:    ServiceSimilarity(a, b),
		Label:      LabelSimilarity(a, b),
		Dependency: DependencySimilarity(a, b, s.topo),
		Historical: s.hist(a.Fingerprint, b.Fingerprint),
	}
}

// Score 返回一对告警的加权总分。
func (s *Scorer) Score(a, b AlertView) float64 {
	return CalculateScore(s.Dimensions(a, b), s.cfg.Weights)
}

// Config 暴露绑定的配置（调用方读阈值做决策）。
func (s *Scorer) Config() Config { return s.cfg }
