package collectors

import (
	"strings"
	"time"
)

type k8sEventList struct {
	Items []k8sEvent `json:"items"`
}

type k8sEvent struct {
	// K8s 事件的时间字段有历史包袱：lastTimestamp（v1 核心字段）、
	// eventTime（较新）、firstTimestamp。真实集群里三种都见过，
	// 只认一种会漏掉大部分事件。
	LastTimestamp  string `json:"lastTimestamp"`
	EventTime      string `json:"eventTime"`
	FirstTime      string `json:"firstTimestamp"`
	Count          int    `json:"count"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	InvolvedObject struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"involvedObject"`
}

// eventTime 返回事件的真实发生时间。
//
// 优先 eventTime（RFC3339Nano），其次 lastTimestamp / firstTimestamp。
// 三个都拿不到时返回 false —— 调用方跳过这条，**不用采集时刻兜底**。
func (e k8sEvent) eventTime() (time.Time, bool) {
	for _, raw := range []string{e.EventTime, e.LastTimestamp, e.FirstTime} {
		if raw == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700",
		} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// belongsTo 判断事件是否属于本服务。
//
// 判定必须两步，顺序不能换：
//
//  1. **先在 allNames（全命名空间范围）里**为事件对象定出唯一主人（最长匹配）；
//  2. 再看这个主人是否在 owners（本服务拥有的 Deployment）里。
//
// 为什么不能只在 owners 里找最长匹配：命名空间里同时存在
// `checkout` 与 `checkout-staging` 时，事件对象 `checkout-staging-5f8c`
// 在 owners=[checkout] 里找不到 checkout-staging（它不在集合内），
// 于是被 `checkout` 认领 —— 恰好是本函数要防的那类错挂。
// 先定主人再判归属，`checkout-staging-5f8c` 的主人是 checkout-staging，
// 不在 owners 里，正确排除。
//
// owners 为空时（拿不到 Deployment 列表）退化为「对象名等于 service」——
// **不用前缀匹配**，那正是把 checkout-staging 挂到 checkout 上的原因。
func (e k8sEvent) belongsTo(allNames, owners []string, service string) bool {
	if service == "" {
		// 事件没有服务名（service 为空）：不过滤。
		// 静默返回「无证据」会让用户以为是集群里没有 Warning 事件。
		return true
	}
	if len(owners) == 0 {
		return e.InvolvedObject.Name == service
	}
	owner, ok := e.ownerIn(allNames)
	if !ok {
		// 命名空间里没有任何 Deployment 认领这个对象（可能是 ConfigMap、
		// Node 等非 Deployment 资源的事件）。此时按「名字前缀属于本服务」
		// 兜底 —— 严格相等会漏掉绝大多数 Pod 事件。
		owner, _ = e.ownerIn(owners)
	}
	if owner == "" {
		return false
	}
	for _, n := range owners {
		if n == owner {
			return true
		}
	}
	return false
}

// ownerIn 在给定 Deployment 名集合里找出该事件对象的主人（最长匹配）。
func (e k8sEvent) ownerIn(candidates []string) (string, bool) {
	best := ""
	for _, n := range candidates {
		if n == "" {
			continue
		}
		if e.InvolvedObject.Name != n &&
			!strings.HasPrefix(e.InvolvedObject.Name, n+"-") {
			continue
		}
		if len(n) > len(best) {
			best = n
		}
	}
	return best, best != ""
}

// allDeploymentNames 返回命名空间内全部 Deployment 名。
func allDeploymentNames(deployments []k8sDeployment) []string {
	out := make([]string, 0, len(deployments))
	for _, d := range deployments {
		if d.Metadata.Name != "" {
			out = append(out, d.Metadata.Name)
		}
	}
	return out
}

// deploymentNamesFor 找出属于该服务的 Deployment 名集合。
//
// 归属判定与事件侧同构（最长匹配），这样「事件的 owner」与
// 「rollout 的 Deployment」必然落在同一个集合里 ——
// 否则会出现「采到了 rollout 却说事件不归我」这种自相矛盾的输出。
//
// service 为空时返回全部：不过滤。
func deploymentNamesFor(service string, deployments []k8sDeployment) []string {
	if service == "" {
		out := make([]string, 0, len(deployments))
		for _, d := range deployments {
			if d.Metadata.Name != "" {
				out = append(out, d.Metadata.Name)
			}
		}
		return out
	}
	// 先按 label 判（作者声明的归属，最可靠），再回退到名称推断。
	var byLabel []k8sDeployment
	for _, d := range deployments {
		if d.hasServiceLabel(service) {
			byLabel = append(byLabel, d)
		}
	}
	if len(byLabel) > 0 {
		out := make([]string, 0, len(byLabel))
		for _, d := range byLabel {
			out = append(out, d.Metadata.Name)
		}
		return out
	}
	var byName []string
	for _, d := range deployments {
		n := d.Metadata.Name
		if n == service || strings.HasPrefix(n, service+"-") {
			byName = append(byName, n)
		}
	}
	return byName
}
