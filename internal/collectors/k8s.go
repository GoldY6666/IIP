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
		client:    newHTTPClient(o),
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

	// 分成事故窗口桶与基线桶（见 k8s_baseline.go）。
	// 归属判定、时间解析、半开区间三件事都在 classifyEvents 里，
	// 两个桶共用 —— 基线桶漏掉任何一件都会让change_ratio 系统性偏大。
	buckets := classifyEvents(list.Items, in, allNames, owners)
	matched := buckets.cur
	baseCount := len(buckets.prev)

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
			"object":    e.InvolvedObject.Name,
			"window":    formatWindow(in.Window),
		}
		// matched 与 change_ratio 用**截断前**的真实命中数 n 与基线条数，
		// 不用 len(matched)：TopN 砍的是展示条数，
		// 拿它当分子会把「20 条涨到 5 条」算成没变化。
		applyEventBaseline(md, n, baseCount)
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
			// rollout **刻意不给 change_ratio**：Deployment 只有当前副本数，
			// K8s API 不保留历史值，算不出「变化倍数」。
			// 写这个说明键让 Assess 落第①步（基线不可用）而不是第②步
			//（缺 change_ratio）—— 后者会把设计取舍说成采集器缺陷。
			// 详见 k8s_baseline.go 的 rolloutBaselineReason。
			"baseline": rolloutBaselineReason,
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
