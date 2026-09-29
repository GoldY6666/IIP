package correlation

import "time"

// Group 是一组「应属于同一 Incident」的告警（元素为输入下标）。
type Group []int

// ClusterInMemory 用与线上一致的评分内核，把一批告警按三档决策聚成若干组。
//
// 供回放评估使用（无 DB）：历史相似度由 hist 注入（回放多为 nil→恒 0）。
//
// 决策口径：MERGE 与 REVIEW 都视为「并入当前最佳组」——两者在线上都会建立关联边
// （REVIEW 只是多打 needs_review 标记），NEW 才开新组。评估时的「合并」即「同组」。
func ClusterInMemory(alerts []AlertView, cfg Config, topo *Topology, hist HistoricalFn, ref time.Time) []Group {
	scorer := NewScorer(cfg, topo, hist, ref)

	var groups []Group
	members := make([][]AlertView, 0)

	for i, a := range alerts {
		bestScore := 0.0
		bestGroup := -1
		for gi, ms := range members {
			maxForGroup := 0.0
			for _, ex := range ms {
				if sc := scorer.Score(a, ex); sc > maxForGroup {
					maxForGroup = sc
				}
			}
			if maxForGroup > bestScore {
				bestScore = maxForGroup
				bestGroup = gi
			}
		}

		if bestGroup >= 0 && Decide(bestScore, cfg.Thresholds) != ActionNew {
			groups[bestGroup] = append(groups[bestGroup], i)
			members[bestGroup] = append(members[bestGroup], a)
			continue
		}

		groups = append(groups, Group{i})
		members = append(members, []AlertView{a})
	}
	return groups
}
