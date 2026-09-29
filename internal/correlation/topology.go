package correlation

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Topology 是服务依赖图，用于 DependencySimilarity。
//
// 边视为**无向**：`checkout: [payment]` 与 `payment: [checkout]` 等价，
// HasEdge 双向均返回 true。这是刻意的——告警传播方向在评分阶段并不重要，
// 只要二者存在依赖关系就应加分。
type Topology struct {
	edges map[string]map[string]struct{}
}

// topologyFile 是 YAML 的映射结构。
//
//	services:
//	  checkout: [payment]
//	  payment:  [mysql]
type topologyFile struct {
	Services map[string][]string `yaml:"services"`
}

// LoadTopology 从 YAML 文件加载拓扑。
//
// 容错约定（手册 §4.5）：文件不存在 → 返回空拓扑且 **不报错**，
// 使「无拓扑也能跑」成为默认行为（依赖维恒 0）；只有解析失败才返回错误。
func LoadTopology(path string) (*Topology, error) {
	t := &Topology{edges: map[string]map[string]struct{}{}}
	if path == "" {
		return t, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, fmt.Errorf("读取拓扑文件 %q 失败: %w", path, err)
	}

	var f topologyFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析拓扑文件 %q 失败: %w", path, err)
	}
	t.AddEdges(f.Services)
	return t, nil
}

// AddEdges 批量加入依赖边（供单测直接构造拓扑，无需落文件）。
func (t *Topology) AddEdges(services map[string][]string) {
	if t.edges == nil {
		t.edges = map[string]map[string]struct{}{}
	}
	for from, deps := range services {
		for _, to := range deps {
			if from == "" || to == "" || from == to {
				continue // 空名与自环无意义，静默忽略
			}
			t.addEdge(from, to)
		}
	}
}

func (t *Topology) addEdge(a, b string) {
	if t.edges[a] == nil {
		t.edges[a] = map[string]struct{}{}
	}
	if t.edges[b] == nil {
		t.edges[b] = map[string]struct{}{}
	}
	t.edges[a][b] = struct{}{}
	t.edges[b][a] = struct{}{}
}

// HasEdge 判断两服务是否存在直接依赖（无向）。nil 接收者安全。
func (t *Topology) HasEdge(a, b string) bool {
	if t == nil || a == "" || b == "" {
		return false
	}
	_, ok := t.edges[a][b]
	return ok
}

// CommonNeighbors 返回两服务的共同邻居（按字典序）。nil 接收者安全。
func (t *Topology) CommonNeighbors(a, b string) []string {
	if t == nil {
		return nil
	}
	na, nb := t.edges[a], t.edges[b]
	if len(na) == 0 || len(nb) == 0 {
		return nil
	}
	out := make([]string, 0, len(na))
	for k := range na {
		if _, ok := nb[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Size 返回图中出现过的服务节点数（供启动日志判断拓扑是否为空）。
func (t *Topology) Size() int {
	if t == nil {
		return 0
	}
	return len(t.edges)
}
