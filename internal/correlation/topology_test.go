package correlation

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTopologyHasEdgeIsUndirected(t *testing.T) {
	topo := &Topology{}
	topo.AddEdges(map[string][]string{
		"checkout": {"payment"},
		"payment":  {"mysql"},
	})

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"声明方向", "checkout", "payment", true},
		{"反向同样成立", "payment", "checkout", true},
		{"二级依赖", "payment", "mysql", true},
		{"反向二级依赖", "mysql", "payment", true},
		{"隔代不算直接依赖", "checkout", "mysql", false},
		{"自环不成立", "checkout", "checkout", false},
		{"无关服务", "checkout", "search", false},
		{"空名不成立", "", "payment", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := topo.HasEdge(tc.a, tc.b); got != tc.want {
				t.Fatalf("HasEdge(%q,%q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestTopologyCommonNeighbors：checkout 与 mysql 无直连，但共享 payment。
func TestTopologyCommonNeighbors(t *testing.T) {
	topo := &Topology{}
	topo.AddEdges(map[string][]string{
		"checkout": {"payment", "cache"},
		"payment":  {"mysql"},
		"mysql":    {"cache"},
	})

	got := topo.CommonNeighbors("checkout", "mysql")
	if want := []string{"cache", "payment"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CommonNeighbors = %v，期望 %v", got, want)
	}
	if got := topo.CommonNeighbors("checkout", "checkout"); !reflect.DeepEqual(got, []string{"cache", "payment"}) {
		t.Errorf("自身与自身应返回全部邻居，实得 %v", got)
	}
	if got := topo.CommonNeighbors("checkout", "nobody"); got != nil {
		t.Errorf("与未知服务应无共同邻居，实得 %v", got)
	}
}

func TestTopologySizeAndInvalidEdges(t *testing.T) {
	topo := &Topology{}
	topo.AddEdges(map[string][]string{
		"checkout": {"payment"},
		"payment":  {"mysql"},
	})
	if got := topo.Size(); got != 3 {
		t.Errorf("节点数 = %d，期望 3", got)
	}

	// 空名与自环一律静默忽略：它们不构成真实依赖，且会让「共同邻居」失真。
	bad := &Topology{}
	bad.AddEdges(map[string][]string{
		"":  {"a"},
		"b": {""},
		"c": {"c"},
	})
	if got := bad.Size(); got != 0 {
		t.Errorf("非法边不应产生节点，实得 %d", got)
	}
	if bad.HasEdge("c", "c") {
		t.Error("自环不应被记录")
	}
}

// TestTopologyNilSafety：无拓扑（nil）是合法状态（未配置 topology.yaml），
// 所有查询方法都必须安全返回零值而非 panic。
func TestTopologyNilSafety(t *testing.T) {
	var topo *Topology
	if topo.HasEdge("a", "b") {
		t.Error("nil 拓扑不应有边")
	}
	if got := topo.CommonNeighbors("a", "b"); got != nil {
		t.Errorf("nil 拓扑共同邻居应为 nil，实得 %v", got)
	}
	if got := topo.Size(); got != 0 {
		t.Errorf("nil 拓扑节点数应为 0，实得 %d", got)
	}
}

func TestLoadTopology(t *testing.T) {
	t.Run("空路径返回空拓扑且不报错", func(t *testing.T) {
		topo, err := LoadTopology("")
		if err != nil {
			t.Fatalf("空路径不应报错: %v", err)
		}
		if topo.Size() != 0 {
			t.Errorf("应为空拓扑，实得 %d 个节点", topo.Size())
		}
	})

	// 手册 §4.5：拓扑是可选增强，缺文件必须能正常启动（依赖维恒 0）。
	t.Run("文件不存在不报错", func(t *testing.T) {
		topo, err := LoadTopology(filepath.Join(t.TempDir(), "topology.yaml"))
		if err != nil {
			t.Fatalf("缺文件不应报错: %v", err)
		}
		if topo.Size() != 0 {
			t.Errorf("应为空拓扑，实得 %d 个节点", topo.Size())
		}
	})

	t.Run("目录不存在不报错", func(t *testing.T) {
		topo, err := LoadTopology(filepath.Join(t.TempDir(), "no-such-dir", "topology.yaml"))
		if err != nil {
			t.Fatalf("目录缺失不应报错: %v", err)
		}
		if topo.Size() != 0 {
			t.Errorf("应为空拓扑，实得 %d 个节点", topo.Size())
		}
	})

	t.Run("正常解析", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topology.yaml")
		content := "services:\n  checkout: [payment]\n  payment: [mysql]\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写入临时拓扑失败: %v", err)
		}
		topo, err := LoadTopology(path)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !topo.HasEdge("checkout", "payment") || !topo.HasEdge("mysql", "payment") {
			t.Errorf("解析后依赖边缺失，节点数 %d", topo.Size())
		}
	})

	t.Run("内容为空视为空拓扑", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topology.yaml")
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatalf("写入临时拓扑失败: %v", err)
		}
		topo, err := LoadTopology(path)
		if err != nil {
			t.Fatalf("空文件不应报错: %v", err)
		}
		if topo.Size() != 0 {
			t.Errorf("应为空拓扑，实得 %d 个节点", topo.Size())
		}
	})

	// 语法错误必须显性失败：静默吞掉会让「拓扑配错了」变成难以察觉的评分退化。
	t.Run("语法错误报错", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "topology.yaml")
		if err := os.WriteFile(path, []byte("services: [\n"), 0o600); err != nil {
			t.Fatalf("写入临时拓扑失败: %v", err)
		}
		if _, err := LoadTopology(path); err == nil {
			t.Fatal("语法错误应报错")
		}
	})
}
