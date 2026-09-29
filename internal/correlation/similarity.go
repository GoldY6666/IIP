package correlation

import "time"

// 本文件实现 V1.0 §5.2 中**无需外部数据**的四维相似度（纯函数，表驱动单测）。
// 第五维 HistoricalSimilarity 需要查询历史共现，由 Scorer 注入的数据源提供。

// endsAtOr 返回告警结束时刻；nil（仍 firing）按参考时刻兜底。
//
// 手册 §4.2 明确：ends_at=NULL 表示告警尚未恢复，应按"当前时刻"参与时间窗计算，
// 否则会把仍在持续的告警当作零时长，时间相似度恒为 0。
func endsAtOr(a AlertView, ref time.Time) time.Time {
	if a.EndsAt == nil {
		return ref
	}
	return *a.EndsAt
}

// windowDuration 返回告警窗口时长；负值（数据异常）归零。
func windowDuration(a AlertView, ref time.Time) time.Duration {
	d := endsAtOr(a, ref).Sub(a.StartsAt)
	if d < 0 {
		return 0
	}
	return d
}

// TimeSimilarity 计算两个告警时间窗口的重叠比例（overlap / max(时长)）。
func TimeSimilarity(a, b AlertView, ref time.Time) float64 {
	startMax := laterTime(a.StartsAt, b.StartsAt)
	endMin := earlierTime(endsAtOr(a, ref), endsAtOr(b, ref))
	if endMin.Before(startMax) {
		return 0
	}
	overlap := endMin.Sub(startMax)

	union := windowDuration(a, ref)
	if d := windowDuration(b, ref); d > union {
		union = d
	}
	if union <= 0 {
		return 0
	}
	return float64(overlap) / float64(union)
}

// ServiceSimilarity：同 service → 1.0；仅同 namespace → 0.6；否则 0。
func ServiceSimilarity(a, b AlertView) float64 {
	if a.Service != "" && a.Service == b.Service {
		return 1.0
	}
	if a.Namespace != "" && a.Namespace == b.Namespace {
		return 0.6
	}
	return 0
}

// LabelSimilarity：对 `k=v` 对做 Jaccard，排除动态 label（instance/pod/container/node）。
//
// 集合元素是**完整键值对**而非仅 key——手册 §4.2 明确要求：只有 k 与 v 都相同才算交集，
// 否则 "service=checkout" 与 "service=payment" 会被误判为相似。
func LabelSimilarity(a, b AlertView) float64 {
	setA := stablePairs(a.Labels)
	setB := stablePairs(b.Labels)
	if len(setA) == 0 && len(setB) == 0 {
		return 0
	}
	inter := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// stablePairs 把 labels 投影为排除动态键后的 `k=v` 集合。
func stablePairs(labels map[string]string) map[string]struct{} {
	out := make(map[string]struct{}, len(labels))
	for k, v := range labels {
		if _, dyn := dynamicLabelKeys[k]; dyn {
			continue
		}
		out[k+"="+v] = struct{}{}
	}
	return out
}

// DependencySimilarity：直接依赖边 → 0.9；有共同邻居 → 0.5；否则 0。
// topo 为 nil（无拓扑）时安全返回 0。
func DependencySimilarity(a, b AlertView, topo *Topology) float64 {
	if a.Service == "" || b.Service == "" {
		return 0
	}
	if topo.HasEdge(a.Service, b.Service) {
		return 0.9
	}
	if len(topo.CommonNeighbors(a.Service, b.Service)) > 0 {
		return 0.5
	}
	return 0
}

func laterTime(x, y time.Time) time.Time {
	if x.After(y) {
		return x
	}
	return y
}

func earlierTime(x, y time.Time) time.Time {
	if x.Before(y) {
		return x
	}
	return y
}
