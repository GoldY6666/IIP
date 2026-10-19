package rca

import (
	"errors"
	"fmt"

	"github.com/yu/iip/internal/evidence"
)

// Assign 为一批同事件的证据填充 Confidence。
//
// **必须在汇总之后调用一次**（由编排层负责），不能在各采集器内部各调一次——
// 覆盖度要看全批证据命中了几种类型，单个采集器看不到别人。
//
// 返回统计信息，供日志与断言使用；不修改入参切片的底层数组以外的东西。
//
// 三条硬约束：
//  1. 不可评估的证据保持 nil，绝不写 0。
//  2. 不覆盖已有的非 nil 值——重复调用应当幂等，
//     否则第二批证据到达后会把第一批的结论改掉，而那没人能察觉。
//  3. 覆写路径写进 metadata 的 coverage 键，与置信度值同源同批计算。
func Assign(evs []evidence.Evidence) Stats {
	st := Stats{Total: len(evs)}
	if len(evs) == 0 {
		return st
	}

	// 先扫一遍算覆盖度：命中「可评估且带规则档位」的类型数。
	//
	// 注意分母口径——只数**可评估**的类型。不可评估的证据既不贡献覆盖度，
	// 也不算进分母，否则「采到 3 条但全无基线」会被算成覆盖度 3/6。
	assessed := map[evidence.Type]bool{}
	for _, e := range evs {
		if e.Confidence != nil {
			// 已有结论的证据不参与本次判定（幂等）。
			st.Skipped++
			continue
		}
		band, _ := Assess(e)
		if band == BandUnassessable {
			st.Unassessable++
			continue
		}
		assessed[e.Type] = true
	}
	st.CoverageHit = len(assessed)

	// 再扫一遍填值。这一遍必须重新 Assess，
	// 因为上一遍的结论只存在局部 map 里，没有挂在证据上。
	for i := range evs {
		if evs[i].Confidence != nil {
			continue
		}
		band, basis := Assess(evs[i])
		if band == BandUnassessable {
			continue
		}
		v, ok := Compose(band, st.CoverageHit)
		if !ok {
			continue
		}
		conf := v
		evs[i].Confidence = &conf
		st.Assigned++

		if evs[i].Metadata == nil {
			evs[i].Metadata = make(map[string]string, 2)
		}
		evs[i].Metadata[KeyConfidence] = basis + "（band=" + band.String() + "）"
		evs[i].Metadata[KeyCoverage] = fmt.Sprintf("%d/%d", st.CoverageHit, CoverageTotal)
	}
	return st
}

// Stats 是 Assign 的统计结果。
type Stats struct {
	// Total 是传入证据总数。
	Total int
	// Assigned 是本次真正写入 confidence 的条数。
	Assigned int
	// Unassessable 是判定为「无法评估」而保持留空的条数。
	Unassessable int
	// Skipped 是已有结论被跳过的条数（幂等保护）。
	Skipped int
	// CoverageHit 是本批证据命中的**可评估**证据类型数。
	CoverageHit int
}

// String 返回统计的可读形式，供日志使用。
func (s Stats) String() string {
	return fmt.Sprintf("total=%d assigned=%d unassessable=%d skipped=%d coverage=%d/%d",
		s.Total, s.Assigned, s.Unassessable, s.Skipped, s.CoverageHit, CoverageTotal)
}

// ErrNotAssessable 表示要求给出置信度，但该证据不可评估。
var ErrNotAssessable = errors.New("该证据缺少可复核的置信度来源，必须留空")
