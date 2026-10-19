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
