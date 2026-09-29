package correlation

// Decide 按阈值把关联分数映射为三档决策（V1.0 §5.3）：
//
//	score ≥ merge  → MERGE
//	score ≥ review → REVIEW
//	否则            → NEW
//
// 边界取「大于等于」，与 V1.0 的 `>=` 语义一致（0.80 应判 MERGE，而非 REVIEW）。
func Decide(score float64, t Thresholds) string {
	switch {
	case score >= t.Merge:
		return ActionMerge
	case score >= t.Review:
		return ActionReview
	default:
		return ActionNew
	}
}
