package collectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// K8sCollector 从 Kubernetes API 采两类证据：**Warning 事件**与
// **Deployment rollout 记录**。
//
// **默认不注册**（偏差 B1）：本仓库零 K8s 依赖 —— compose 里没有 k8s 服务、
// 没有 kubeconfig、没有 RBAC 清单。写一个接不上任何真实集群却「看起来在工作」
// 的采集器比不写更危险：运维会以为「为什么采不到证据」是数据的问题。
// 接入是**部署侧的事**（用户自带集群 + kubeconfig + 打开开关），
// 因此本采集器只用 mock server 单测证明解析与降级逻辑正确。
//
// **无状态且并发安全**：所有可变状态都在方法内，结构体只有只读配置。
type K8sCollector struct {
	baseURL string
	// namespace 是要查的命名空间。**不支持跨命名空间查询**：
	// K8s 的 events 与 deployments 权限是按命名空间授权的，
	// 跨命名空间查会稳定返回 403，而 403 与「命名空间不存在」的 404
	// 在排查时极易混淆 —— 配置里就要求用户明确给出这一个。
	namespace string
	client    *http.Client
	topN      int
}

// NewK8sCollector 建一个 K8s 采集器。baseURL 是 apiserver 根地址
// （形如 `https://k8s.internal:6443`），namespace 不能为空。
func NewK8sCollector(baseURL, namespace string, opts ...Option) *K8sCollector {
	o := resolveOptions(opts)
	return &K8sCollector{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		namespace: strings.TrimSpace(namespace),
		client:    &http.Client{Timeout: o.timeout},
		topN:      o.topN,
	}
}

// Source 返回来源标识。
func (c *K8sCollector) Source() string { return SourceK8s }

// Collect 采 Warning 事件与 rollout 记录。
//
// 顺序上有一处刻意的安排：**先取 Deployment 列表，再用它判定事件归属**。
// 纯靠字符串前缀匹配事件对象名是不可靠的 —— Pod 名形如
// `<deployment>-<hash>-<suffix>`，而 Deployment 名由用户随意取，
// 于是 `checkout-staging-1` 会以 `checkout-` 开头被误判成 checkout 的事件。
// 那样等于把一个服务的事故证据挂到另一个服务上，而页面上完全看不出来。
//
// 三类查询的成败互不影响：events 查不到不代表 deployments 也查不到
// （前者权限缺失极常见），而 rollout 往往是定位事故原因的关键。
// 三者都失败才算整个采集失败。
func (c *K8sCollector) Collect(ctx context.Context, in evidence.Input) ([]evidence.Evidence, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, errors.New("k8s 采集器未配置 apiserver 地址")
	}
	if c.namespace == "" {
		return nil, errors.New("k8s 采集器未配置 namespace")
	}

	deployments, depErr := c.listDeployments(ctx)
	evs, evErr := c.collectEvents(ctx, in, deployments)
	rolls, rollErr := c.collectRollouts(in, deployments)

	if evErr != nil && depErr != nil {
		return nil, fmt.Errorf("k8s 事件与 Deployment 查询均失败: %v / %v", evErr, depErr)
	}
	if evErr != nil && rollErr != nil {
		return nil, fmt.Errorf("k8s 事件与 rollout 查询均失败: %v / %v", evErr, rollErr)
	}

	out := make([]evidence.Evidence, 0, len(evs)+len(rolls))
	out = append(out, evs...)
	out = append(out, rolls...)

	// 部分失败要记下来但不阻断：页面得能说明「为什么少了这一半」，
	// 而不能只显示「采到 2 条证据」让人以为集群里就这些。
	var failures []string
	if evErr != nil {
		failures = append(failures, "Warning 事件查询失败: "+evErr.Error())
	}
	if rollErr != nil {
		failures = append(failures, "rollout 解析失败: "+rollErr.Error())
	}
	if depErr != nil {
		failures = append(failures,
			"Deployment 列表不可用，事件归属只能按名称近似判定: "+depErr.Error())
	}
	if len(failures) > 0 {
		for i := range out {
			out[i].Metadata = withFailures(out[i].Metadata, failures)
		}
	}
	return out, nil
}

// listDeployments 拉取命名空间内的 Deployment 列表。
func (c *K8sCollector) listDeployments(ctx context.Context) ([]k8sDeployment, error) {
	endpoint := c.baseURL + "/apis/apps/v1/namespaces/" +
		url.PathEscape(c.namespace) + "/deployments"
	var list k8sDeploymentList
	if err := httpGetJSON(ctx, c.client, endpoint, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// collectEvents 查 Warning 事件。
func (c *K8sCollector) collectEvents(ctx context.Context, in evidence.Input,
	deployments []k8sDeployment) ([]evidence.Evidence, error) {
	// fieldSelector 只做一段过滤：type=Warning。
	// 只有告警级事件才是证据，Normal 事件（调度、探针成功）数量是
	// Warning 的百倍，全拉回来会把 50 条预算耗在噪声上。
	//
	// K8s 的 field selector **不支持时间范围**，所以时间过滤只能在客户端做。
	// 这一点必须写清楚，否则下一个人会去找一个不存在的 field selector。
	sel := "type=Warning"
	endpoint := c.baseURL + "/api/v1/namespaces/" +
		url.PathEscape(c.namespace) + "/events?fieldSelector=" + url.QueryEscape(sel)

	var list k8sEventList
	if err := httpGetJSON(ctx, c.client, endpoint, &list); err != nil {
		return nil, err
	}

	// 事件归属要两步判定（见 belongsTo）：
	//  先在全命名空间范围内定出事件对象的唯一主人，再看主人是不是本服务的。
	// 所以这里需要**两个集合** —— allNames 是「主人候选池」，
	// owners 是「本服务真正拥有的 Deployment」。
	allNames := allDeploymentNames(deployments)
	owners := deploymentNamesFor(in.Service, deployments)

	matched := make([]k8sEvent, 0, len(list.Items))
	for _, e := range list.Items {
		ts, ok := e.eventTime()
		if !ok {
			// §4.4：给不出真实时间的证据一律不写。
			// K8s 事件偶有 lastTimestamp 为空（仅 firstTimestamp），
			// 这种情况跳过，不拿采集时刻兜底。
			continue
		}
		if ts.Before(in.Window.Start) || !ts.Before(in.Window.End) {
			continue
		}
		if !e.belongsTo(allNames, owners, in.Service) {
			continue
		}
		matched = append(matched, e)
	}

	// 按时间倒序：最近的排前面，便于页面直接看到「最新发生了什么」。
	sort.SliceStable(matched, func(i, j int) bool {
		ti, _ := matched[i].eventTime()
		tj, _ := matched[j].eventTime()
		return ti.After(tj)
	})

	n := len(matched)
	if n > c.topN {
		matched = matched[:c.topN]
	}

	out := make([]evidence.Evidence, 0, len(matched))
	for i, e := range matched {
		ts, _ := e.eventTime()
		md := map[string]string{
			"kind":      "warning_event",
			"namespace": c.namespace,
			"rank":      strconv.Itoa(i + 1),
			"matched":   strconv.Itoa(n),
			"object":    e.InvolvedObject.Name,
			"window":    formatWindow(in.Window),
		}
		if e.Count > 1 {
			// K8s 对同类事件做聚合，count>1 表示「这一类发生过 N 次」。
			// 不给这个数字的话，一次反复重启 50 次的 Pod 探针失败
			// 与一次偶发失败在页面上长得一样。
			md["occurrences"] = strconv.Itoa(e.Count)
		}
		out = append(out, evidence.Evidence{
			Type:      evidence.TypeDeployment,
			Source:    SourceK8s,
			Query:     c.eventsQuery(),
			Value:     fmt.Sprintf("%s：%s", e.Reason, truncate(e.Message, 200)),
			Timestamp: ts.UTC(),
			Metadata:  md,
		})
	}
	return out, nil
}

// collectRollouts 由已取到的 Deployment 列表产出 rollout 证据。
func (c *K8sCollector) collectRollouts(in evidence.Input,
	deployments []k8sDeployment) ([]evidence.Evidence, error) {
	// rollout 的时间锚点是最后一次条件更新时间。
	// K8s 没有「rollout 发生时刻」这个字段，用 updated 近似是
	// **最接近的真实时间**；没有它就跳过（§4.4），不拿采集时刻兜底。
	out := make([]evidence.Evidence, 0, len(deployments))
	for i := range deployments {
		d := deployments[i]
		// 只取属于本服务的 Deployment。同命名空间别的服务的发布
		// 与本事故无关 —— 全量带上会让每个事件都挂一堆无关 rollout。
		if !d.hasServiceLabel(in.Service) && !nameMatchesService(d.Metadata.Name, in.Service) {
			continue
		}
		ts, ok := d.rolloutTime()
		if !ok {
			continue
		}
		if ts.Before(in.Window.Start) || !ts.Before(in.Window.End) {
			continue
		}

		replicas, ready, updated, unavailable := d.replicaCounts()
		md := map[string]string{
			"kind":        "rollout",
			"namespace":   c.namespace,
			"deployment":  d.Metadata.Name,
			"generation":  strconv.FormatInt(d.Metadata.Generation, 10),
			"replicas":    strconv.Itoa(replicas),
			"ready":       strconv.Itoa(ready),
			"updated":     strconv.Itoa(updated),
			"window":      formatWindow(in.Window),
			"rollout_ok":  strconv.FormatBool(ready == replicas && replicas > 0),
			"last_update": ts.UTC().Format(time.RFC3339),
		}
		if unavailable > 0 {
			md["unavailable"] = strconv.Itoa(unavailable)
		}
		for _, cond := range d.Status.Conditions {
			// 只记**异常**条件。K8s 的 condition 语义是「期望为 True」：
			// Progressing=False 恰恰是「发布卡住」的直接证据，
			// 而 Available=True 是常态（每个正常 Deployment 都有），
			// 记它只是噪声。
			//
			// 判据不能写成「Status != True 就跳过」—— 那恰好把
			// Progressing=False 过滤掉，发布卡住这条根因线索就丢了。
			if cond.Type == "Available" && cond.Status == "True" {
				continue
			}
			if cond.Reason == "" {
				continue
			}
			md["condition_"+strings.ToLower(cond.Type)] =
				fmt.Sprintf("%s(%s)", cond.Reason, cond.Status)
		}

		out = append(out, evidence.Evidence{
			Type:   evidence.TypeDeployment,
			Source: SourceK8s,
			Query:  c.rolloutsQuery(),
			Value: fmt.Sprintf("Deployment %s：%d/%d 就绪",
				d.Metadata.Name, ready, replicas),
			// 时间取条件更新的时刻，不取采集时刻。
			Timestamp: ts.UTC(),
			Metadata:  md,
		})
	}
	return out, nil
}

// eventsQuery 返回可复制后复现的请求（手册验收标准第 2 条）。
func (c *K8sCollector) eventsQuery() string {
	return fmt.Sprintf("GET %s/api/v1/namespaces/%s/events?fieldSelector=type%%3DWarning",
		c.baseURL, c.namespace)
}

func (c *K8sCollector) rolloutsQuery() string {
	return fmt.Sprintf("GET %s/apis/apps/v1/namespaces/%s/deployments",
		c.baseURL, c.namespace)
}

func formatWindow(w evidence.Window) string {
	return w.Start.UTC().Format(time.RFC3339) + "/" + w.End.UTC().Format(time.RFC3339)
}

// ---------- K8s 响应结构（只声明用到的字段） ----------

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

type k8sDeploymentList struct {
	Items []k8sDeployment `json:"items"`
}

type k8sDeployment struct {
	Metadata struct {
		Name              string            `json:"name"`
		Generation        int64             `json:"generation"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		CreationTimestamp string            `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		Replicas            int `json:"replicas"`
		ReadyReplicas       int `json:"readyReplicas"`
		UpdatedReplicas     int `json:"updatedReplicas"`
		UnavailableReplicas int `json:"unavailableReplicas"`
		Conditions          []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			Reason             string `json:"reason"`
			Message            string `json:"message"`
			LastUpdateTime     string `json:"lastUpdateTime"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"conditions"`
	} `json:"status"`
}

// hasServiceLabel 判断 Deployment 的标准 label 是否指向该服务。
//
// label 是「作者声明的归属」，比从名字推断可靠得多 —— 真实集群里
// `app.kubernetes.io/name` 基本都打了，而 Deployment 名可能起得很随意。
func (d k8sDeployment) hasServiceLabel(service string) bool {
	if service == "" {
		return true
	}
	for _, key := range []string{
		"app.kubernetes.io/name", "app", "app.kubernetes.io/instance",
	} {
		if v, ok := d.Metadata.Labels[key]; ok && v == service {
			return true
		}
	}
	return false
}

// nameMatchesService 是 label 缺失时的回退判定。
func nameMatchesService(name, service string) bool {
	if service == "" {
		return true
	}
	return name == service || strings.HasPrefix(name, service+"-")
}

// rolloutTime 返回 rollout 的时间锚点：所有条件里最新的更新时刻。
func (d k8sDeployment) rolloutTime() (time.Time, bool) {
	var newest time.Time
	for _, c := range d.Status.Conditions {
		for _, raw := range []string{c.LastUpdateTime, c.LastTransitionTime} {
			if raw == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				continue
			}
			if t.After(newest) {
				newest = t
			}
		}
	}
	if newest.IsZero() {
		return time.Time{}, false
	}
	return newest.UTC(), true
}

func (d k8sDeployment) replicaCounts() (replicas, ready, updated, unavailable int) {
	// spec.replicas 是**期望值**；缺省时 K8s 默认为 1，
	// 但 Deployment 建好尚未起 Pod 时 status.replicas 可能是 0 ——
	// 那时拿 0 当期望值会算出「0/0 就绪 = 一切正常」，
	// 而真实情况是「还没起来」。
	if d.Spec.Replicas > 0 {
		replicas = d.Spec.Replicas
	} else {
		replicas = d.Status.Replicas
	}
	return replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas,
		d.Status.UnavailableReplicas
}
