// Package rca 承载 M6 S1 的根因分析领域逻辑：置信度判据与根因建议生成。
//
// 与 evidence / task 包同构：领域规则（档位怎么定、覆盖度怎么数、
// 什么情况下必须留空）一律零 SQL、零网络，可在无库无网环境下完整测试。
//
// 规格来源：docs/IIP-M6详细开发手册.md §4.1/ §4.2、docs/S1-置信度判据设计.md。
//
// 设计上分成两级，原因是覆盖度必须跨采集器才能算：
//
//	单条级 Assess  —— 读该条证据自己的 Metadata，判「规则命中强度」。
//	事件级 Assign  —— 统计同一事件命中了几种证据类型（覆盖度），合成最终置信度。
//
// 采集器只知道自己这一类证据，算不出覆盖度；所以 Assign 必须由
// 编排层（worker）在汇总之后调用一次，而不是各采集器各写各的。
package rca

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/yu/iip/internal/evidence"
)

// Band 是「规则命中强度」这一维的档位。
//
// 刻意与 confidence 的数值分开：Band 回答的是「这条证据的变化有多明显」，
// 而 confidence 还要叠加覆盖度。把两者混成一个 float 会让
// 「一条弱证据但类型齐全」与「一条强证据但孤证」算出同一个数，
// 而这两件事的可信度显然不同。
type Band int

const (
	// BandUnassessable 表示**无法评估**——缺少基线或数据不可用。
	//
	// 这是与BandWeak 语义完全不同的第三态：Weak 是「评估了，支撑力弱」，
	// Unassessable 是「压根没法评估」。两者若合并，
	// 「没有基线可比」会被读成「评估结果很弱」，那是把缺数据说成了负面结论。
	BandUnassessable Band = iota

	// BandWeak 是变化幅度不足阈值。
	BandWeak

	// BandMedium 是变化幅度达到中等阈值。
	BandMedium

	// BandStrong 是变化幅度达到强阈值。
	BandStrong
)

// String 返回档位的中文名，进错误信息与 metadata。
func (b Band) String() string {
	switch b {
	case BandUnassessable:
		return "unassessable"
	case BandWeak:
		return "weak"
	case BandMedium:
		return "medium"
	case BandStrong:
		return "strong"
	default:
		return fmt.Sprintf("unknown(%d)", int(b))
	}
}

// 规则命中的变化倍数阈值。
//
// 5 倍取自手册 §4.1 的原文例子（「错误量5 倍突增」⇒ 高）；
// 2 倍是「明显变化」与「噪声波动」的分界，低于它归为weak。
const (
	MediumRatio = 2.0
	StrongRatio = 5.0
)

// Metadata 键名。判据只读这几个键，不读别的。
const (
	// KeyBaseline 是采集器在「基线窗口取不到数据」时写入的说明键。
	//
	// **它存在即代表基线不可用** —— 这是本判据里最容易搞错的一处：
	// 采集器基线查询失败时会写 `baseline="基线窗口查询失败..."`，
	// 而 change_ratio 此时被 ratio() 置为 0。若不先看 baseline 就读
	// change_ratio，「没有基线」会被算成「变化为 0 ⇒ 弱证据」。
	KeyBaseline = "baseline"

	// KeyChangeRatio 是「后值/ 前值」的倍数，由采集器写入。
	KeyChangeRatio = "change_ratio"

	// KeyConfidence 是本判据写入档位说明的键，供人工复核。
	KeyConfidence = "confidence_basis"

	// KeyCoverage 是本判据写入覆盖度的键，供人工复核。
	KeyCoverage = "confidence_coverage"
)

// Assess 判定单条证据的规则命中强度。
//
// 三态返回，**不是两态**：BandUnassessable 必须在调用方与 Weak 分开处理。
// 判据（顺序不可调换）：
//
//  1. baseline 键存在 → 基线不可用 → Unassessable。
//  2. change_ratio 缺失或不是合法数字 → Unassessable。
//  3. change_ratio 是 NaN 或 ±Inf → Unassessable。
//     formatFloat 对这两者分别返回 "NaN" 与 "+Inf"/"-Inf"，
//     它们是**字符串**而非数字，直接 ParseFloat 会失败；但若哪个
//     采集器改用 strconv 格式化，就会得到真正的 ±Inf —— 故两种都要拒。
//  4. 其余按倍数落Weak/Medium/Strong。
//
// 第二个返回值是人可读的复核依据，会写进 metadata；
// 写它是为了让「这个 0.75 从哪来」在页面上有答案，而不是只能问作者。
func Assess(e evidence.Evidence) (band Band, basis string) {
	if _, hasBaseline := e.Metadata[KeyBaseline]; hasBaseline {
		return BandUnassessable, "基线窗口不可用，无可比数据"
	}
	raw, ok := e.Metadata[KeyChangeRatio]
	if !ok {
		return BandUnassessable, "缺少 " + KeyChangeRatio + "，无法判断变化幅度"
	}
	ratio, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return BandUnassessable, KeyChangeRatio + " 不是合法数字: " + raw
	}
	if isNotFinite(ratio) {
		return BandUnassessable, KeyChangeRatio + " 为非有限值: " + raw
	}
	switch {
	case ratio >= StrongRatio:
		return BandStrong, fmt.Sprintf("变化 %.2f 倍，达到强阈值 %.0f 倍", ratio, StrongRatio)
	case ratio >= MediumRatio:
		return BandMedium, fmt.Sprintf("变化 %.2f 倍，达到中等阈值 %.0f 倍", ratio, MediumRatio)
	default:
		return BandWeak, fmt.Sprintf("变化 %.2f 倍，未达中等阈值 %.0f 倍", ratio, MediumRatio)
	}
}

// isNotFinite 报告 f 是否为 NaN 或 ±Inf。
//
// 不用 math.IsInf(f,0) 一把抓：那只覆盖 ±Inf。NaN 单独判，
// 因为 NaN 的任何比较都返回 false —— 若漏判它，
// `ratio >= StrongRatio` 会一路落到 default 变成 BandWeak，
// 即「数据坏掉」被静默解释成「变化很弱」。
func isNotFinite(f float64) bool {
	if f != f { // NaN 的唯一可靠判据：自己不等自己
		return true
	}
	return f > maxFinite || f < -maxFinite
}

// maxFinite 是 float64 的最大有限值，用作 ±Inf 的比较界。
const maxFinite = 1.7976931348623157e308

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
