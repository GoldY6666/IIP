package metrics

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// familyLabelAllow 是**业务指标**→允许的标签名白名单。
//
// 它的作用是守卫「标签基数无界」这条纪律：任何新增业务指标、或给已有指标
// 加标签，都必须先在本表登记 —— 而登记时必须面对「这个值有上界吗」这个问题。
// 不登记却上线，本测试立刻红。
//
// 空切片表示「该指标不应有任何标签」（耗时直方图不带路数/路径标签：
// 路径在本项目是常量，加它只会在将来加出第二、第三个 webhook 端点时炸基数）。
var familyLabelAllow = map[string][]string{
	AlertIngestTotal:                  {"result"},
	AlertDedupTotal:                   {"result"},
	WebhookDurationSecs:               {},
	CorrelationScore:                  {"action"},
	IncidentCreatedTotal:              {},
	CorrelationCandidateFallbackTotal: {},
	EvidenceDurationSecs:              {"source"},
	TaskRetryTotal:                    {"reason"},
}

// touchAll 给每个带标签的指标各点一次（每个允许的取值一次），
// 使 CounterVec / HistogramVec 真正产出样本。
//
// **不点的话 Gather 里根本不会出现这些族**：Prometheus 的 *Vec 是惰性的，
// 没有子标签就没有子 metric。因此下面所有断言都必须先调本函数，
// 否则「族不存在」会让断言以「找不到」的形式假红，
// 掩盖掉「指标压根没注册」这个真问题。
func touchAll(m *Metrics) {
	for _, v := range []string{IngestStored, IngestDuplicate, IngestInvalid, IngestError} {
		m.AlertIngest.WithLabelValues(v).Inc()
	}
	for _, v := range []string{DedupHit, DedupMiss, DedupRace} {
		m.AlertDedup.WithLabelValues(v).Inc()
	}
	m.WebhookDuration.Observe(0.01)
	for _, v := range []string{"new", "review", "merge"} {
		m.CorrelationScore.WithLabelValues(v).Observe(0.5)
	}
	m.IncidentCreated.Inc()
	for _, v := range []string{"prometheus", "loki", "k8s", "trace"} {
		m.EvidenceDuration.WithLabelValues(v).Observe(0.2)
	}
	for _, v := range []string{RetryStaleReclaim, RetryAttemptFailed} {
		m.TaskRetry.WithLabelValues(v).Inc()
	}
}

// gatherCtx 取一次采集结果，并把族按名字建索引。
func gatherByName(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather 失败: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(fams))
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

// runtimeFamily 判断是否为 client_golang 自带的运行时指标族。
//
// 用前缀判断而不是逐个列举：Go 收集器的族名随版本增减
// （go_gc_gogc_percent 之类），写死清单会在升级依赖时假红。
func runtimeFamily(name string) bool {
	return strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_")
}

// TestExposedFamiliesAreExactlyTheBusinessOnes 守住「/metrics 上暴露什么」。
//
// 断言的是**集合相等**而非包含：多暴露一个族（比如给调试临时加的
// iip_debug_xxx）同样要过审 —— 否则它会悄悄留在生产端点里。
func TestExposedFamiliesAreExactlyTheBusinessOnes(t *testing.T) {
	m := New()
	touchAll(m)

	var got []string
	for name := range gatherByName(t, m) {
		if runtimeFamily(name) {
			continue
		}
		got = append(got, name)
	}
	sort.Strings(got)

	want := make([]string, 0, len(familyLabelAllow))
	for name := range familyLabelAllow {
		want = append(want, name)
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("暴露的业务指标族不一致\n实际: %v\n期望: %v", got, want)
	}
}

// TestRuntimeFamiliesAreRegistered 确认运行时指标真的在。
//
// 这条不是「多测一点」：自监控若只有业务计数，goroutine 泄漏与内存增长
// 就要等 OOM 才被发现。也正因为有它，Handler() 里误用 promhttp.Handler()
// （绑全局注册表）造成的「业务指标全丢」才会被上一条抓出来。
func TestRuntimeFamiliesAreRegistered(t *testing.T) {
	m := New()
	fams := gatherByName(t, m)
	if _, ok := fams["go_goroutines"]; !ok {
		t.Error("缺少 go_goroutines：Go 运行时收集器没注册上")
	}
	if _, ok := fams["process_cpu_seconds_total"]; !ok {
		// Windows 上 process_* 部分指标不可用，但 cpu 与 rss 是可用的。
		t.Error("缺少 process_cpu_seconds_total：进程收集器没注册上")
	}
}

// TestLabelNamesAreBounded 是「标签基数」的守卫。
//
// 正向白名单 + 反向黑名单都查：
//   - 正向：每个族的标签名必须在 familyLabelAllow 里（新增标签必须登记）；
//   - 反向：明确禁止按事件/告警/服务维度打标签（这些值随业务量无界增长）。
//
// 反向那条即使在白名单下也不会漏 —— 但它的价值在**报错信息**：
// 直接说「不许用 incident_id」比「result 不在白名单里」好懂得多。
func TestLabelNamesAreBounded(t *testing.T) {
	m := New()
	touchAll(m)
	fams := gatherByName(t, m)

	forbidden := []string{"incident_id", "alert_id", "fingerprint", "service", "path"}
	for name, allow := range familyLabelAllow {
		fam, ok := fams[name]
		if !ok {
			t.Errorf("指标 %s 未产出样本（touchAll 没覆盖到，或注册失败）", name)
			continue
		}
		allowSet := make(map[string]bool, len(allow))
		for _, a := range allow {
			allowSet[a] = true
		}
		for _, mf := range fam.GetMetric() {
			for _, lp := range mf.GetLabel() {
				ln := lp.GetName()
				if !allowSet[ln] {
					t.Errorf("指标 %s 出现标签 %q，不在白名单 %v 内（新增标签须同步本测试）", name, ln, allow)
				}
				for _, bad := range forbidden {
					if ln == bad {
						t.Errorf("指标 %s 使用标签 %q：该值随业务量无界增长，会拖垮 Prometheus", name, ln)
					}
				}
			}
		}
	}
}

// TestInstrumentsRecordValues 断言埋点真的落到 /metrics 上（而非只调了方法）。
//
// 逐个族查「值是否等于我记的数」：这条同时覆盖了
// 「New 漏注册某个仪器」（Gather 里找不到）与「仪器接错」
// （记到别的族上）两类错误。
func TestInstrumentsRecordValues(t *testing.T) {
	m := New()
	for i := 0; i < 3; i++ {
		m.AlertIngest.WithLabelValues(IngestStored).Inc()
	}
	m.AlertIngest.WithLabelValues(IngestDuplicate).Add(2)
	m.AlertDedup.WithLabelValues(DedupHit).Add(7)
	m.WebhookDuration.Observe(0.3)
	m.WebhookDuration.Observe(0.7)
	m.CorrelationScore.WithLabelValues("merge").Observe(0.85)
	m.IncidentCreated.Add(4)
	m.CorrelationCandidateFallback.Add(3)
	m.EvidenceDuration.WithLabelValues("prometheus").Observe(2)
	m.TaskRetry.WithLabelValues(RetryStaleReclaim).Add(5)

	fams := gatherByName(t, m)

	if got := counterWith(t, fams, AlertIngestTotal, "result", IngestStored); got != 3 {
		t.Errorf("%s{result=stored} = %v，期望 3", AlertIngestTotal, got)
	}
	if got := counterWith(t, fams, AlertIngestTotal, "result", IngestDuplicate); got != 2 {
		t.Errorf("%s{result=duplicate} = %v，期望 2", AlertIngestTotal, got)
	}
	if got := counterWith(t, fams, AlertDedupTotal, "result", DedupHit); got != 7 {
		t.Errorf("%s{result=hit} = %v，期望 7", AlertDedupTotal, got)
	}
	if got := counterWith(t, fams, TaskRetryTotal, "reason", RetryStaleReclaim); got != 5 {
		t.Errorf("%s{reason=stale_reclaim} = %v，期望 5", TaskRetryTotal, got)
	}
	if got := simpleCounter(t, fams, IncidentCreatedTotal); got != 4 {
		t.Errorf("%s = %v，期望 4", IncidentCreatedTotal, got)
	}
	if got := simpleCounter(t, fams, CorrelationCandidateFallbackTotal); got != 3 {
		t.Errorf("%s = %v，期望 3", CorrelationCandidateFallbackTotal, got)
	}

	if n, sum := histogramOf(t, fams, WebhookDurationSecs, "", ""); n != 2 || sum < 0.99 || sum > 1.01 {
		t.Errorf("%s 样本数=%v 总和=%v，期望 2 与 1.0", WebhookDurationSecs, n, sum)
	}
	if n, _ := histogramOf(t, fams, CorrelationScore, "action", "merge"); n != 1 {
		t.Errorf("%s{action=merge} 样本数=%v，期望 1", CorrelationScore, n)
	}
	if n, _ := histogramOf(t, fams, EvidenceDurationSecs, "source", "prometheus"); n != 1 {
		t.Errorf("%s{source=prometheus} 样本数=%v，期望 1", EvidenceDurationSecs, n)
	}
}

// TestNoopIsSharedAndNeverNil 守住「埋点侧零分支」这个前提。
//
// 若 Noop() 哪天改成返回 nil，所有埋点处的 m.Xxx.Inc() 都会 panic；
// 而那种 panic 只在「没配指标」的部署出现，本地单测跑不到。
// 故这里既查指针非 nil，也真调一遍全部仪器。
func TestNoopIsSharedAndNeverNil(t *testing.T) {
	a := Noop()
	if a == nil {
		t.Fatal("Noop() 返回 nil：埋点处将全部 panic")
	}
	if a != Noop() {
		t.Error("Noop() 每次返回新实例：未注入指标的 Service 会各建一个注册表，白占内存")
	}
	if a.AlertIngest == nil || a.AlertDedup == nil || a.WebhookDuration == nil ||
		a.CorrelationScore == nil || a.IncidentCreated == nil ||
		a.EvidenceDuration == nil || a.TaskRetry == nil {
		t.Fatal("Noop() 有仪器为 nil")
	}
	// 真调一遍：nil 接口/指针在这里就会炸出来。
	touchAll(a)
}

// TestHandlerServesBusinessMetrics 走一次真实 HTTP 曝光路径。
//
// 这条覆盖的是「注册表接错」：Handler() 若误用 promhttp.Handler()
// （绑全局注册表），指标计数一切正常但 /metrics 上一条业务指标都没有。
func TestHandlerServesBusinessMetrics(t *testing.T) {
	m := New()
	m.IncidentCreated.Add(2)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody)
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 返回 %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, IncidentCreatedTotal+" 2") {
		t.Errorf("/metrics 正文里找不到 %s 2:\n%s", IncidentCreatedTotal, firstLines(body, 40))
	}
	if !strings.Contains(body, "# TYPE "+IncidentCreatedTotal+" counter") {
		t.Errorf("/metrics 正文缺少 TYPE 声明，说明曝光路径不是本注册表")
	}
}

// ---- 断言辅助 ----

// counterWith 取带单标签的 counter 值；标签对不上时判失败并说明。
func counterWith(t *testing.T, fams map[string]*dto.MetricFamily, name, label, value string) float64 {
	t.Helper()
	fam, ok := fams[name]
	if !ok {
		t.Fatalf("指标族 %s 不存在", name)
	}
	for _, mf := range fam.GetMetric() {
		if labelValue(mf, label) == value {
			return mf.GetCounter().GetValue()
		}
	}
	t.Fatalf("指标 %s 找不到标签 %s=%q", name, label, value)
	return 0
}

// simpleCounter 取无标签 counter 值。
func simpleCounter(t *testing.T, fams map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	fam, ok := fams[name]
	if !ok {
		t.Fatalf("指标族 %s 不存在", name)
	}
	all := fam.GetMetric()
	if len(all) != 1 {
		t.Fatalf("指标 %s 有 %d 个样本，期望 1（它不该带标签）", name, len(all))
	}
	return all[0].GetCounter().GetValue()
}

// histogramOf 取直方图的样本数与总和；label 为空表示该族无标签。
func histogramOf(t *testing.T, fams map[string]*dto.MetricFamily, name, label, value string) (uint64, float64) {
	t.Helper()
	fam, ok := fams[name]
	if !ok {
		t.Fatalf("指标族 %s 不存在", name)
	}
	for _, mf := range fam.GetMetric() {
		if label == "" || labelValue(mf, label) == value {
			h := mf.GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	t.Fatalf("指标 %s 找不到标签 %s=%q", name, label, value)
	return 0, 0
}

// labelValue 取某个标签的值，不存在返回空串。
func labelValue(mf *dto.Metric, name string) string {
	for _, lp := range mf.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// firstLines 截取正文前 n 行，用于失败时给出可读上下文。
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
