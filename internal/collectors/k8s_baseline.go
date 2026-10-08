package collectors

import (
	"strconv"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 本文件是 K8s 采集器的**基线对比**部分（Warning 事件侧）。
//
// 为什么能算基线：Warning 事件自带时间戳（eventTime → lastTimestamp →
// firstTimestamp 三级回退），而 k8s.go 对 events 只发**一次**请求
// （fieldSelector=type=Warning，无 limit），时间过滤全在客户端做。
// 也就是说「事故前有没有同类事件」的数据**本来就在手里**，
// 只是被单桶过滤丢掉了 —— 分成两个桶不需要第二次请求、
// 不增加超时面、也不需要新的 RBAC 权限。
//
// 为什么 rollout 侧算不出来：见 rolloutBaselineReason。

// rolloutBaselineReason 是 rollout 证据的基线说明文案。
//
// rollout 给不出 change_ratio 不是实现偷懒，是数据源没有：
// Deployment 对象只有**当前**副本数（spec.replicas / readyReplicas /
// updatedReplicas），K8s API **不保留历史值**。算「变化倍数」必须有一个
// 事故前的副本数，而那个数不存在。
//
// 不写这个键的后果不是「少一个字段」：Assess 会落到第②步，
// basis 说「缺少 change_ratio，无法判断变化幅度」——
// 那把「设计上给不出」说成了「采集器忘了写」，
// 运维会去查采集器代码而不是查集群。写清楚才省这段时间。
const rolloutBaselineReason = "K8s API 不保留 Deployment 历史副本数，无法给出基线"

// baselineWindow 返回与事故窗口**等宽、紧邻、不重叠**的前窗口。
//
// 与 prometheus / loki 采集器同构。边界取 [Start-width, Start)：
// 若让基线窗口也含 Start 时刻，两窗会在该点重叠，
// 事故刚开始的事件会被算进「事故前」，把变化幅度压低。
func baselineWindow(w evidence.Window) evidence.Window {
	width := w.Duration()
	return evidence.Window{Start: w.Start.Add(-width), End: w.Start}
}

// inWindow 报告 ts 是否落在半开区间 [Start, End) 内。
//
// 半开而非闭区间：K8s 事件时间精度到秒，正好落在 End 上的事件存在
// （如整点批处理），闭区间会把它算进「事故后」，而它其实是窗口外的。
func inWindow(ts time.Time, w evidence.Window) bool {
	return !ts.Before(w.Start) && ts.Before(w.End)
}

// bucketStats 是双桶统计的结果。
type bucketStats struct {
	// cur 是事故窗口内、且归属本服务的事件。
	cur []k8sEvent
	// prev 是基线窗口内、且归属本服务的事件。
	prev []k8sEvent
}

// classifyEvents 把一份 events 响应分进「事故窗口桶」与「基线桶」。
//
// **两个桶都必须过归属判定**，且用同一套 allNames/owners。
// 漏掉基线桶的过滤会让别的服务的事件混进分母 ——
// 于是本服务 Warning 量「没涨」被算成「涨了很多」，
// 而页面上只显示一个倍数，完全看不出来。
// 这与 belongsTo 里防的「checkout-staging挂到 checkout 上」是同一类错挂，
// 只是方向相反（那边是多收，这边是分母被撑大）。
//
// 没有时间的事件在两个桶里都不计（§4.4），不拿采集时刻兜底。
func classifyEvents(items []k8sEvent, in evidence.Input,
	allNames, owners []string) bucketStats {
	before := baselineWindow(in.Window)
	var st bucketStats
	for _, e := range items {
		ts, ok := e.eventTime()
		if !ok {
			continue
		}
		if !e.belongsTo(allNames, owners, in.Service) {
			continue
		}
		switch {
		case inWindow(ts, in.Window):
			st.cur = append(st.cur, e)
		case inWindow(ts, before):
			st.prev = append(st.prev, e)
		}
	}
	return st
}

// applyEventBaseline 给 Warning 事件证据写基线对比字段。
//
// 只写两种状态，**没有「基线查询失败」这一支**——
// 基线与事故窗口来自同一次成功的查询，那个状态在这里不存在。
// 写一个不可达的分支等于凭空多一条判据，日后没人能验证它。
//
// 两个分支与 Assess 的判据顺序对齐：
//   - 基线 > 0 → matched_before + change_ratio，进入第④步落 Weak/Medium/Strong
//   - 基线 = 0 → matched_before=0 + novelty，**不给 change_ratio**
//
// 基线为 0 时不给倍数，是因为 ratio() 在分母为 0 时返回 0，
// 而「变化 0 倍」会被读成「强度很弱」—— 实际含义是「压根没有可比数据」。
// 同一情形在 loki.go 的错误模式证据里也是这么处理的（novelty 分支），
// 两处保持一致，避免同一个含义在两个采集器里长得不一样。
//
// cur 必须是**截断前**的真实命中数：TopN 砍掉的是展示条数，
// 拿它当分子会让「本服务 20 条 Warning」显示成「5 条」，
// 变化倍数随之虚低四倍。
func applyEventBaseline(md map[string]string, cur, prev int) {
	md["matched"] = strconv.Itoa(cur)
	md["matched_before"] = strconv.Itoa(prev)
	if prev == 0 {
		md["novelty"] = "事故前窗口无同类 Warning 事件，本次为新出现"
		return
	}
	md["change_ratio"] = formatFloat(ratio(float64(cur), float64(prev)))
}
