package rca

import (
	"github.com/yu/iip/internal/evidence"
)

// 覆盖度档位表：命中类型数 → 基础置信度。
//
// 分母固定为 6（evidence 表 CHECK 约束里的全部类型），**不随注册表变化**——
// 若分母取「当前已注册的采集器可评估的类型数」，那么关掉 k8s 采集器
// 会让同一批证据的置信度凭空变高，而数据本身没变。
// 可复核性要求分母是常数。
var coverageBase = map[int]float64{
	1: 0.45,
	2: 0.60,
	3: 0.75,
	4: 0.90,
}

// CoverageTotal 是覆盖度的分母：证据类型的合法总数。
//
// **由 evidence.SortedTypes() 的长度得出，而不是手写常量**——
// 手写的话，将来往 CHECK 约束里加了第七种类型，覆盖度会静默失真
// 且没有任何报错（分母永远停在6，每类的实际占比被悄悄压低）。
//
// 顺带说明为什么不取「当前已注册采集器的类型数」：
// 那样分母会随配置漂移——关掉 k8s 采集器会让同一批证据的置信度
// 凭空变高，而数据本身一条没变。
var CoverageTotal = len(evidence.SortedTypes())

// WeakCap 是弱规则命中的置信度上限。
//
// 一条「变化不足 2 倍」的证据，无论同事件还有多少其他类型的证据，
// 都不该把置信度推到 0.9 —— 那等于让一条噪声去代表整批证据的强度。
const WeakCap = 0.50

// StrongBonus 是强规则命中的加成。
const StrongBonus = 0.05

// MaxConfidence 是置信度上限。
const MaxConfidence = 0.95

// BaseForCoverage 返回覆盖度对应的基础置信度。
//
// 命中 0 种类型返回 0 —— 调用方必须先把「0 条命中」与
// 「命中但不可评估」分开处理，否则 0 会被当成合法的最低置信度。
func BaseForCoverage(hit int) float64 {
	return coverageBase[hit]
}

// Compose 把规则命中强度与覆盖度合成最终置信度。
//
// 三条规则（顺序即优先级）：
//
//	BandUnassessable → 0,false（不可评估，一律留空）
//	BandWeak         → min(base, WeakCap)
//	BandMedium       → base
//	BandStrong       → min(base+StrongBonus, MaxConfidence)
//
// 第二个返回值报告是否应当写入。**false 时调用方必须保持 nil**，
// 不得写入 0——「不可评估」与「置信度 0」在页面上必须能区分开。
func Compose(band Band, hit int) (float64, bool) {
	if band == BandUnassessable {
		return 0, false
	}
	base := BaseForCoverage(hit)
	if base <= 0 {
		// 有可评估的证据，却算出 0 种命中类型 —— 这说明调用方
		// 传进来的证据集合与它给的 hit 自相矛盾。按采集器缺陷处理。
		return 0, false
	}
	var out float64
	switch band {
	case BandWeak:
		if base > WeakCap {
			out = WeakCap
		} else {
			out = base
		}
	case BandMedium:
		out = base
	case BandStrong:
		out = base + StrongBonus
		if out > MaxConfidence {
			out = MaxConfidence
		}
	default:
		return 0, false
	}
	return out, true
}
