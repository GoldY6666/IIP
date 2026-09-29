package correlation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// 本文件实现 M2 手册 §7 的「回放评估」：读取 testdata/replay/*.json，
// 用与线上一致的评分内核（ClusterInMemory）聚类，再与期望分组比对，
// 计算精确率（不错并）与召回率（不漏并）。
//
// 门槛（手册 §八）：精确率必须 = 100%，召回率 ≥ 90%。
// 精确率不给余量是刻意的——错并会让值班人员看到无关告警被塞进同一事件，
// 比漏并更伤信任；漏并还可由人工合并兜底。

// replayScenario 是回放夹具的形态（与 testdata/replay/*.json 一一对应）。
type replayScenario struct {
	Name           string              `json:"name"`
	Description    string              `json:"description"`
	Topology       map[string][]string `json:"topology"`
	ExpectedGroups [][]int             `json:"expected_groups"`
	Alerts         []replayAlert       `json:"alerts"`
}

// replayAlert 是夹具中的一条告警（字段名刻意用 snake_case，与夹具一致）。
type replayAlert struct {
	Alertname string            `json:"alertname"`
	Service   string            `json:"service"`
	Namespace string            `json:"namespace"`
	Severity  string            `json:"severity"`
	Labels    map[string]string `json:"labels"`
	StartsAt  time.Time         `json:"starts_at"`
	EndsAt    *time.Time        `json:"ends_at"`
}

// replayScenarios 是全部回放场景；新增场景须同时补夹具与登记在此。
var replayScenarios = []string{
	"scenario1_service_storm",
	"scenario2_cascade",
	"scenario3_unrelated",
}

func loadReplayScenario(t *testing.T, name string) replayScenario {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "replay", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取回放夹具 %s 失败: %v", path, err)
	}
	var sc replayScenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("解析回放夹具 %s 失败: %v", path, err)
	}
	if len(sc.Alerts) == 0 {
		t.Fatalf("回放夹具 %s 不含任何告警", path)
	}
	return sc
}

// toViews 把夹具告警投影为评分视图，并返回参考时刻（全部窗口的最晚结束时刻）。
//
// 参考时刻取「最晚结束」而非 time.Now()：回放必须是确定性的，
// 否则同一份夹具在不同时刻跑出不同结果，评估就失去意义。
func toViews(alerts []replayAlert) ([]AlertView, time.Time) {
	views := make([]AlertView, 0, len(alerts))
	ref := time.Time{}
	for i, a := range alerts {
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		// 夹具的顶层字段与 labels 应自洽；若 labels 缺键则补齐，
		// 使 AlertView 的 Service/Namespace 与评分用的 labels 不产生分歧。
		if _, ok := labels["service"]; !ok && a.Service != "" {
			labels["service"] = a.Service
		}
		if _, ok := labels["namespace"]; !ok && a.Namespace != "" {
			labels["namespace"] = a.Namespace
		}
		if _, ok := labels["alertname"]; !ok && a.Alertname != "" {
			labels["alertname"] = a.Alertname
		}

		views = append(views, AlertView{
			ID:          int64(i + 1),
			Fingerprint: fmt.Sprintf("%s/%s/%s#%d", a.Namespace, a.Service, a.Alertname, i),
			Severity:    a.Severity,
			Service:     labels["service"],
			Namespace:   labels["namespace"],
			Labels:      labels,
			StartsAt:    a.StartsAt,
			EndsAt:      a.EndsAt,
		})

		end := a.StartsAt
		if a.EndsAt != nil {
			end = *a.EndsAt
		}
		if end.After(ref) {
			ref = end
		}
	}
	return views, ref
}

// pairSet 把分组展开成「同组告警对」集合，用于计算精确率/召回率。
func pairSet(groups []Group) map[[2]int]struct{} {
	out := make(map[[2]int]struct{})
	for _, g := range groups {
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				a, b := g[i], g[j]
				if a > b {
					a, b = b, a
				}
				out[[2]int{a, b}] = struct{}{}
			}
		}
	}
	return out
}

// expectedPairSet 把夹具的 expected_groups 展开成同样的告警对集合。
func expectedPairSet(groups [][]int) map[[2]int]struct{} {
	out := make(map[[2]int]struct{})
	for _, g := range groups {
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				a, b := g[i], g[j]
				if a > b {
					a, b = b, a
				}
				out[[2]int{a, b}] = struct{}{}
			}
		}
	}
	return out
}

// ratio 计算 want（分子）在 base（分母）中的比例；分母为 0 时视为 1（空集视为完全命中）。
func ratio(want, base int) float64 {
	if base == 0 {
		return 1.0
	}
	return float64(want) / float64(base)
}

func TestReplayScenarios(t *testing.T) {
	const minRecall = 0.90

	for _, name := range replayScenarios {
		t.Run(name, func(t *testing.T) {
			sc := loadReplayScenario(t, name)
			views, ref := toViews(sc.Alerts)

			topo := &Topology{}
			topo.AddEdges(sc.Topology)

			// hist 传 nil → 恒 0：回放是冷启动视角，不给「历史共现」加分，
			// 避免用未来数据回测过去（会虚高）。
			groups := ClusterInMemory(views, DefaultConfig(), topo, nil, ref)

			got := pairSet(groups)
			want := expectedPairSet(sc.ExpectedGroups)

			hit := 0
			for p := range got {
				if _, ok := want[p]; ok {
					hit++
				}
			}
			precision := ratio(hit, len(got))
			recall := ratio(hit, len(want))

			t.Logf("%s：%d 条告警 → %d 组；预测同组对 %d，期望同组对 %d，命中 %d；精确率 %.4f，召回率 %.4f",
				sc.Description, len(views), len(groups), len(got), len(want), hit, precision, recall)

			// ① 精确率必须 100%：任何一对自己不该在一起的告警被并进同一组都视为失败。
			if precision < 1.0 {
				t.Fatalf("精确率 %.4f < 1.0：存在错误合并（%d/%d 对不属于同组）",
					precision, len(got)-hit, len(got))
			}
			// ② 召回率 ≥ 90%。
			if recall < minRecall {
				t.Fatalf("召回率 %.4f < %.2f：同根因告警被漏并", recall, minRecall)
			}
			// ③ 分组数量与期望一致（防止「全并成一组」这类精确率假达标）。
			if len(groups) != len(sc.ExpectedGroups) {
				t.Fatalf("分组数 %d，期望 %d", len(groups), len(sc.ExpectedGroups))
			}
			// ④ 分组内容与期望逐组一致（最强断言，锁定回归）。
			if gotNorm, wantNorm := normalizeGroups(groups), normalizeGroups(toGroups(sc.ExpectedGroups)); !reflect.DeepEqual(gotNorm, wantNorm) {
				t.Fatalf("分组结果与期望不一致：\n实得 %v\n期望 %v", gotNorm, wantNorm)
			}
		})
	}
}

// toGroups 把夹具的 [][]int 转成 []Group，便于与 ClusterInMemory 的输出统一比较。
func toGroups(in [][]int) []Group {
	out := make([]Group, 0, len(in))
	for _, g := range in {
		out = append(out, Group(g))
	}
	return out
}

// normalizeGroups 把分组归一为「组内升序 + 组间按首元素升序」，消除顺序依赖。
func normalizeGroups(in []Group) [][]int {
	out := make([][]int, 0, len(in))
	for _, g := range in {
		cp := append([]int(nil), g...)
		sort.Ints(cp)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == 0 || len(out[j]) == 0 {
			return len(out[i]) < len(out[j])
		}
		return out[i][0] < out[j][0]
	})
	return out
}

// TestReplayScenario1GroupSizes 对风暴场景额外断言「规模最大的组恰好包含 200 条」，
// 使「噪声被并进主组」这类问题在指标之外也能被直接定位。
func TestReplayScenario1GroupSizes(t *testing.T) {
	sc := loadReplayScenario(t, "scenario1_service_storm")
	views, ref := toViews(sc.Alerts)
	topo := &Topology{}
	topo.AddEdges(sc.Topology)

	groups := ClusterInMemory(views, DefaultConfig(), topo, nil, ref)

	maxLen, maxIdx := 0, -1
	for i, g := range groups {
		if len(g) > maxLen {
			maxLen, maxIdx = len(g), i
		}
	}
	if maxLen != 200 {
		t.Fatalf("最大的组包含 %d 条，期望 200 条", maxLen)
	}
	for _, idx := range groups[maxIdx] {
		if idx >= 200 {
			t.Fatalf("噪声告警下标 %d 被并进风暴主组", idx)
		}
	}
}

// TestClusterInMemoryScalesTo1000Alerts 覆盖手册 §八 的性能门禁：
// 1000 条告警聚类的耗时必须远低于 60s 预算（现实现为 O(n²) 贪心，
// 实测在秒级；这里留出充足余量以免在慢速 CI 上抖动误报）。
func TestClusterInMemoryScalesTo1000Alerts(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过 1000 条规模的性能用例（-short）")
	}

	const n = 1000
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	alerts := make([]AlertView, 0, n)
	for i := 0; i < n; i++ {
		start := base.Add(time.Duration(i) * time.Second)
		end := start.Add(10 * time.Minute)
		alerts = append(alerts, AlertView{
			ID:          int64(i + 1),
			Fingerprint: fmt.Sprintf("prod/checkout/LatencyHigh#%d", i),
			Severity:    "critical",
			Service:     "checkout",
			Namespace:   "prod",
			Labels: map[string]string{
				"alertname": "LatencyHigh",
				"service":   "checkout",
				"namespace": "prod",
				"tier":      "web",
				"instance":  fmt.Sprintf("pod-%04d", i),
			},
			StartsAt: start,
			EndsAt:   &end,
		})
	}
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}, "payment": {"mysql"}})

	started := time.Now()
	groups := ClusterInMemory(alerts, DefaultConfig(), topo, nil, base.Add(time.Hour))
	elapsed := time.Since(started)

	if elapsed > 60*time.Second {
		t.Fatalf("%d 条告警聚类耗时 %v，超出 60s 预算", n, elapsed)
	}
	if len(groups) != 1 {
		t.Fatalf("同服务同规则的风暴应聚为 1 组，实得 %d 组", len(groups))
	}
	t.Logf("%d 条告警聚类耗时 %v，共 %d 组", n, elapsed, len(groups))
}

// BenchmarkClusterInMemory1000 记录 1000 条规模的聚类基准，便于后续调参时对比。
func BenchmarkClusterInMemory1000(b *testing.B) {
	const n = 1000
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	alerts := make([]AlertView, 0, n)
	for i := 0; i < n; i++ {
		start := base.Add(time.Duration(i) * time.Second)
		end := start.Add(10 * time.Minute)
		alerts = append(alerts, AlertView{
			ID:          int64(i + 1),
			Fingerprint: fmt.Sprintf("fp-%d", i),
			Service:     "checkout",
			Namespace:   "prod",
			Labels: map[string]string{
				"alertname": "LatencyHigh",
				"service":   "checkout",
				"namespace": "prod",
				"instance":  fmt.Sprintf("pod-%04d", i),
			},
			StartsAt: start,
			EndsAt:   &end,
		})
	}
	topo := &Topology{}
	topo.AddEdges(map[string][]string{"checkout": {"payment"}})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(ClusterInMemory(alerts, DefaultConfig(), topo, nil, base.Add(time.Hour))); got != 1 {
			b.Fatalf("期望 1 组，实得 %d 组", got)
		}
	}
}
