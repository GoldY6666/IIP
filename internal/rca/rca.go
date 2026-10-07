package rca

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yu/iip/internal/evidence"
)

// Hypothesis 是一条根因假设。
//
// **它不是结论**，是给人看的候选：处置前需要人确认。
// 手册 §2.2 把「自动写 root_causes」列为明确不做，理由是
// 错的根因会误导处置且难以发现——与 confidence 填 1.0 是同一类错误。
type Hypothesis struct {
	// Summary 是假设的一句话陈述。
	Summary string

	// SupportTypes 是支撑该假设的证据类型（按字典序）。
	SupportTypes []evidence.Type

	// Support 是支撑证据条数。
	Support int

	// Confidence 是该假设的置信度，**与 evidence.confidence 是两回事**：
	// 后者是「单条证据有多可信」，这里是「这个整体推断有多可信」。
	//
	// 取值同样受Compose 的约束：给不出可复核来源时必须为 nil，
	// 不能用「反正有证据撑着」随手填一个。
	Confidence *float64

	// Basis 是置信度的复核依据，供人工核对。
	Basis string
}

// Suggest 是 SuggestFrom 返回的结果。
type Suggest struct {
	// Hypotheses 是候选假设，按置信度从高到低、同值按 Support 从大到小排序。
	//
	// 排序理由同 evidence.SortedTypes()：并发或迭代顺序不确定会让
	// 「第一条是什么」变得不可预测，而人先看到的恰恰是第一条。
	Hypotheses []Hypothesis

	// AssessedCount 是参与评估的证据条数（不可评估的不计）。
	AssessedCount int

	// Skipped 是没有置信度因而无法参与推断的证据条数。
	Skipped int
}

// 假设模板表：按「本批命中的证据类型集合」挑出候选假设。
//
// 为什么用固定模板而不是自由文本生成：
//  1. 每条模板都能追溯到具体的证据类型，可核对；
//  2. 措辞稳定，前端可以按类型做高亮，不需要解析自然语言；
//  3. **不会出现「看起来很有道理但无从复核」的句子** —— 那是最危险的一类输出。
var templates = []struct {
	// RequireAll 是该假设成立所需的全部证据类型。
	RequireAll []evidence.Type
	// Template 是用 %s 填入证据类型列表的句式。
	Template string
}{
	{
		RequireAll: []evidence.Type{evidence.TypeDeployment, evidence.TypeMetric},
		Template:   "部署变更与指标突变在时间上重合（%s），变更可能是诱因",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeConfigChange, evidence.TypeLog},
		Template:   "配置变更后出现新的错误模式（%s）",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeLog, evidence.TypeTrace},
		Template:   "错误日志与调用链异常同时出现（%s），下游依赖可能已不可用",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeMetric, evidence.TypeLog},
		Template:   "指标突增伴随错误日志增长（%s）",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeHistorical},
		Template:   "与历史共现模式一致（%s）",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeLog},
		Template:   "仅日志类证据成立（%s），根因需人工补充其他侧信息",
	},
	{
		RequireAll: []evidence.Type{evidence.TypeMetric},
		Template:   "仅指标类证据成立（%s），根因需人工补充其他侧信息",
	},
}

// SuggestFrom 从一批证据生成根因假设候选。
//
// **不写库**。调用方拿到的是给人看的建议，
// 写不写进 incidents.root_causes 由人决定（手册 §2.2）。
//
// 三条设计约束：
//  1. 只用**有置信度**的证据。confidence 为 nil 的证据
//     说明「没有可复核来源」，拿它推断根因等于把缺数据当证据。
//  2. 每条假设必须列出支撑它的证据类型 —— 复核的人第一眼就要知道
//     「凭什么这么说」，否则这就是一个不可证伪的结论。
//  3. 支撑证据不足时**仍可给假设，但 Confidence 必为 nil**：
//     「有迹象但不足以定量」是真实状态，不该被伪装成「置信度 0.1」。
//
// 口径分离（两条，别混）：
//
//	Confidence 用**整批命中类型数** —— 覆盖度越高，结论越可信。
//	Support 用**该模板要求的类型**下的证据条数 —— 这条假设有多少支撑。
//
// 早先版本误用 len(RequireAll) 当覆盖度，导致命中三类反而不比命中两类高。
func SuggestFrom(evs []evidence.Evidence) Suggest {
	s := Suggest{}
	// typeSupport[t] = 该类型下有置信度的证据条数。
	typeSupport := map[evidence.Type]int{}
	for _, e := range evs {
		if e.Confidence == nil {
			s.Skipped++
			continue
		}
		typeSupport[e.Type]++
	}
	s.AssessedCount = len(evs) - s.Skipped

	hit := make([]evidence.Type, 0, len(typeSupport))
	for t := range typeSupport {
		hit = append(hit, t)
	}
	sort.Slice(hit, func(i, j int) bool { return hit[i] < hit[j] })

	hitSet := make(map[evidence.Type]bool, len(hit))
	for _, t := range hit {
		hitSet[t] = true
	}

	for _, tpl := range templates {
		if !coversAll(hitSet, tpl.RequireAll) {
			continue
		}
		support := 0
		types := make([]evidence.Type, 0, len(tpl.RequireAll))
		for _, need := range tpl.RequireAll {
			types = append(types, need)
			support += typeSupport[need]
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

		h := Hypothesis{
			Summary:      fmt.Sprintf(tpl.Template, joinTypes(types)),
			SupportTypes: types,
			Support:      support,
		}
		// 覆盖度决定置信度，用**整批命中的类型数** len(hit)，
		// 不用 len(tpl.RequireAll)——
		// 后者会让「命中三类」与「命中两类」算出同一个值：
		// 命中 metric+log+deployment 时，deployment+metric 模板只看见 2 类，
		// 于是多命中的 log 被白白丢掉。覆盖度一旦被模板绑架，
		// 「证据类型越多越可信」这条最基本的规律就不成立了。
		//
		// Support 与 Confidence 用不同口径是**有意的**：
		// Support = 这条假设被多少条证据支撑（只看模板要求的那些类型）；
		// Confidence = 整批证据的覆盖度（这批数据整体有多可信）。
		if v, ok := Compose(BandMedium, len(hit)); ok {
			conf := v
			h.Confidence = &conf
			h.Basis = fmt.Sprintf("本批命中 %d/%d 类证据类型，该假设由 %d 条证据支撑（%s）",
				len(hit), CoverageTotal, support, joinTypes(types))
		} else {
			h.Basis = "命中类型数为 0，无法给出置信度"
		}
		s.Hypotheses = append(s.Hypotheses, h)
	}

	sort.SliceStable(s.Hypotheses, func(i, j int) bool {
		a, b := s.Hypotheses[i], s.Hypotheses[j]
		if a.Confidence == nil && b.Confidence == nil {
			return a.Support > b.Support
		}
		if a.Confidence == nil {
			return false // nil 排最后
		}
		if b.Confidence == nil {
			return true
		}
		if *a.Confidence != *b.Confidence {
			return *a.Confidence > *b.Confidence
		}
		return a.Support > b.Support
	})
	return s
}

// coversAll 报告 got 是否包含 want 里的全部类型。
func coversAll(got map[evidence.Type]bool, want []evidence.Type) bool {
	for _, w := range want {
		if !got[w] {
			return false
		}
	}
	return true
}

// joinTypes 把类型列表拼成「metric/log」这样的可读串。
//
// 用 "/" 而非 "、" 是因为它同时出现在前端的中文界面上，
// 斜杠比顿号更不容易与中文标点混淆。
func joinTypes(ts []evidence.Type) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, "/")
}
