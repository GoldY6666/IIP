package collectors

import (
	"strings"
	"time"
)

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
