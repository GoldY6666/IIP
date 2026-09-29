package correlation

import (
	"testing"
	"time"
)

// 固定基准时刻，避免用例依赖真实时钟。
var baseT = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return baseT.Add(d) }
func tp(d time.Duration) *time.Time {
	t := at(d)
	return &t
}

// view 构造评分视图的便捷函数。
func view(service, ns string, labels map[string]string, start time.Time, end *time.Time) AlertView {
	if labels == nil {
		labels = map[string]string{}
	}
	if service != "" {
		labels["service"] = service
	}
	if ns != "" {
		labels["namespace"] = ns
	}
	return AlertView{Service: service, Namespace: ns, Labels: labels, StartsAt: start, EndsAt: end}
}

func TestTimeSimilarity(t *testing.T) {
	ref := at(10 * time.Minute)
	cases := []struct {
		name string
		a, b AlertView
		want float64
	}{
		{"完全相同窗口_1", view("", "", nil, at(0), tp(10*time.Minute)), view("", "", nil, at(0), tp(10*time.Minute)), 1.0},
		{"半重叠_0.5", view("", "", nil, at(0), tp(10*time.Minute)), view("", "", nil, at(5*time.Minute), tp(15*time.Minute)), 0.5},
		{"不重叠_0", view("", "", nil, at(0), tp(10*time.Minute)), view("", "", nil, at(20*time.Minute), tp(30*time.Minute)), 0},
		// 公式的并集取 max(时长) 而非区间并集：5 / max(20,5) = 0.25
		{"一全含另一_0.25", view("", "", nil, at(0), tp(20*time.Minute)), view("", "", nil, at(5*time.Minute), tp(10*time.Minute)), 0.25},
		// ends_at=NULL（仍 firing）按 ref 兜底：a 视为 [0,10m]，与 b [5,15] 重叠 5 / max(10,10) = 0.5
		{"结束时间为空_按当前时刻_0.5", view("", "", nil, at(0), nil), view("", "", nil, at(5*time.Minute), tp(15*time.Minute)), 0.5},
		{"零时长窗口_0", view("", "", nil, at(0), tp(0)), view("", "", nil, at(0), tp(0)), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TimeSimilarity(tc.a, tc.b, ref); !almostEq(got, tc.want) {
				t.Fatalf("TimeSimilarity = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestServiceSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b AlertView
		want float64
	}{
		{"同service_1", view("checkout", "prod", nil, at(0), nil), view("checkout", "other", nil, at(0), nil), 1.0},
		{"仅同namespace_0.6", view("checkout", "prod", nil, at(0), nil), view("payment", "prod", nil, at(0), nil), 0.6},
		{"全不同_0", view("checkout", "prod", nil, at(0), nil), view("payment", "stage", nil, at(0), nil), 0},
		{"service均空_退到namespace_0.6", view("", "prod", nil, at(0), nil), view("", "prod", nil, at(0), nil), 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServiceSimilarity(tc.a, tc.b); !almostEq(got, tc.want) {
				t.Fatalf("ServiceSimilarity = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestLabelSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b AlertView
		want float64
	}{
		{
			"完全相同_1",
			view("", "", map[string]string{"alertname": "X", "service": "checkout"}, at(0), nil),
			view("", "", map[string]string{"alertname": "X", "service": "checkout"}, at(0), nil),
			1.0,
		},
		{
			"完全不相交_0",
			view("", "", map[string]string{"alertname": "X"}, at(0), nil),
			view("", "", map[string]string{"alertname": "Y"}, at(0), nil),
			0,
		},
		{
			// 同 key 异 value 不算交集：{k=a} vs {k=b} → 交集 0 / 并集 2 = 0
			"同键异值不计入交集_0",
			view("", "", map[string]string{"k": "a"}, at(0), nil),
			view("", "", map[string]string{"k": "b"}, at(0), nil),
			0,
		},
		{
			// 动态键被排除：只有 alertname=X 参与 → 1
			"排除动态标签_1",
			view("", "", map[string]string{"alertname": "X", "instance": "pod-a", "pod": "p1"}, at(0), nil),
			view("", "", map[string]string{"alertname": "X", "instance": "pod-b", "pod": "p2"}, at(0), nil),
			1.0,
		},
		{
			// {a=1} vs {a=1,b=2} → 交集 1 / 并集 2 = 0.5
			"真子集_0.5",
			view("", "", map[string]string{"a": "1"}, at(0), nil),
			view("", "", map[string]string{"a": "1", "b": "2"}, at(0), nil),
			0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelSimilarity(tc.a, tc.b); !almostEq(got, tc.want) {
				t.Fatalf("LabelSimilarity = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestDependencySimilarity(t *testing.T) {
	// 主链路：checkout → payment → mysql；另有一组与主链路无关的 search → elasticsearch。
	// 关键：checkout 与 mysql **无直连**，但共享邻居 payment → 用于验证 0.5 分支。
	topo := &Topology{}
	topo.AddEdges(map[string][]string{
		"checkout": {"payment"},
		"payment":  {"mysql"},
		"search":   {"elasticsearch"},
	})

	cases := []struct {
		name string
		a, b AlertView
		want float64
	}{
		{"直接依赖_0.9", view("checkout", "", nil, at(0), nil), view("payment", "", nil, at(0), nil), 0.9},
		{"反向依赖也是_0.9", view("payment", "", nil, at(0), nil), view("checkout", "", nil, at(0), nil), 0.9},
		{"共同邻居_0.5", view("checkout", "", nil, at(0), nil), view("mysql", "", nil, at(0), nil), 0.5},
		{"无关_0", view("checkout", "", nil, at(0), nil), view("search", "", nil, at(0), nil), 0},
		{"service缺失_0", view("", "", nil, at(0), nil), view("payment", "", nil, at(0), nil), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DependencySimilarity(tc.a, tc.b, topo); !almostEq(got, tc.want) {
				t.Fatalf("DependencySimilarity = %v，期望 %v", got, tc.want)
			}
		})
	}

	// 无拓扑（nil）时应安全返回 0。
	if got := DependencySimilarity(view("a", "", nil, at(0), nil), view("b", "", nil, at(0), nil), nil); got != 0 {
		t.Fatalf("nil 拓扑应返回 0，实得 %v", got)
	}
}

func almostEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}
