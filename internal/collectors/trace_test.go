package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 本文件是 TraceCollector 的唯一验收手段：
// 采 Philosophy 的降级裁定（M6 §5），本机没有任何真实链路后端，
// 因此**不做集成测试**，只证明「解析正确」与「降级行为正确」。
// 写「看起来连上了」的测试比不写更危险 —— 它会让人以为已验证真实链路。

// traceFixture 喂给 httptest 的假后端。
type traceFixture struct {
	// body 是 /api/traces 返回的完整 JSON。
	body string
	// status 非 0 时用它作为状态码，用于验「后端故障」分支。
	status int
	// hits 记录该后端被调用了几次。
	hits int
}

// newTraceServer 起一个假后端并返回一个指向它的采集器。
func newTraceServer(t *testing.T, fx *traceFixture) (*TraceCollector, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.hits++
		if fx.status != 0 {
			w.WriteHeader(fx.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fx.body))
	}))
	t.Cleanup(srv.Close)
	return NewTraceCollector(srv.URL), srv.Close
}

// traceAt 造一条 startTime 落在采集窗口内的 trace。
//
// offset 相对窗口起点；用固定窗口派生而不是 time.Now()，
// 理由同 helpers_test.go 的 testLine：**落在被查询的区间内**才有意义，
// 否则「证据关联到了正确时间段」这条从未被真正验证。
func traceAt(id string, offset time.Duration, durationUS int64, spansJSON string) string {
	start := testWindow().Start.Add(offset).UnixMicro()
	return `{"traceID":"` + id + `","startTimeFake":0,` +
		`"spans":[{"spanID":"s1","operationName":"HTTP GET /checkout",` +
		`"startTime":` + strconv.FormatInt(start, 10) + `,"duration":` +
		strconv.FormatInt(durationUS, 10) + `,"processID":"p1"` +
		spansJSON + `}],` +
		`"processes":{"p1":{"serviceName":"checkout"}}}`
}

func TestTraceCollector_HappyPath(t *testing.T) {
	// 两条 trace：一条慢且带错误，一条快且干净。
	body := `{"data":[` +
		traceAt("aaaaaaaaaaaaaaaa1111", 10*time.Minute, 812_300, `,"tags":[{"key":"error","type":"bool","value":true}]`) +
		`,` +
		traceAt("bbbbbbbbbbbbbbbb2222", 20*time.Minute, 1_500, `,"tags":[{"key":"http.status_code","type":"int64","value":200}]`) +
		`]}`
	fx := &traceFixture{body: body}
	c, _ := newTraceServer(t, fx)

	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 7,
		Service:    "checkout",
		Window:     testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}

	// 前置条件：后端返回的确实是两条。没有这条的话，
	// 「后端返回空」与「采集器丢弃了全部」都表现为 len==0，无法区分。
	if len(got) != 2 {
		t.Fatalf("证据条数 = %d，期望 2\n%s", len(got), dumpEvidence(got))
	}

	for i, e := range got {
		if e.Type != evidence.TypeTrace {
			t.Errorf("[%d] type = %q，期望 %q", i, e.Type, evidence.TypeTrace)
		}
		if e.Source != SourceTrace {
			t.Errorf("[%d] source = %q，期望 %q", i, e.Source, SourceTrace)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("[%d] 时间戳为空——不得为零值", i)
		}
		if e.Query == "" {
			t.Errorf("[%d] query 为空：运维无法复现", i)
		}
		if e.Confidence != nil {
			t.Errorf("[%d] confidence = %v，期望留空（OPEN-M4-1）", i, *e.Confidence)
		}
		if err := e.Validate(); err != nil {
			t.Errorf("[%d] 证据不合法: %v", i, err)
		}
	}

	// 时间戳必须来自链路自身，不是采集时刻。
	want := testWindow().Start.Add(10 * time.Minute).Truncate(time.Microsecond)
	if got[0].Timestamp.UTC() != want.UTC() {
		t.Errorf("首条时间戳 = %s，期望 %s（须取自 trace 的 startTime，不用 now 兜底）",
			got[0].Timestamp.UTC(), want.UTC())
	}

	// 带 error tag 的那条要记下出错 span 数；干净的那条不该有这个键。
	if v := got[0].Metadata["error_spans"]; v != "1" {
		t.Errorf("error_spans = %q，期望 %q\n%s", v, "1", dumpEvidence(got))
	}
	if _, bad := got[1].Metadata["error_spans"]; bad {
		t.Errorf("干净的 trace 不该带 error_spans\n%s", dumpEvidence(got))
	}
	if v := got[0].Metadata["trace_id"]; v != "aaaaaaaaaaaaaaaa1111" {
		t.Errorf("trace_id = %q，期望完整 ID（不可截断）", v)
	}
}

func TestTraceCollector_QueryUsesMicrosecondsNotSeconds(t *testing.T) {
	body := `{"data":[` + traceAt("aaaaaaaaaaaaaaaa3333", 5*time.Minute, 100, ``) + `]}`
	fx := &traceFixture{body: body}
	c, _ := newTraceServer(t, fx)

	got, err := c.Collect(context.Background(), evidence.Input{
		Service: "checkout",
		Window:  testWindow(),
	})
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1\n%s", len(got), dumpEvidence(got))
	}

	u, err := url.Parse(got[0].Query)
	if err != nil {
		t.Fatalf("query 不是合法 URL: %v", err)
	}
	q := u.Query()

	// Jaeger 的 start/end 要 Unix **微秒**。写成秒级（10 位）会被当成 1970 年，
	// 症状是「接口 200 但永远没数据」——比直接报错难查得多。
	wantStart := strconv.FormatInt(testWindow().Start.UTC().UnixMicro(), 10)
	wantEnd := strconv.FormatInt(testWindow().End.UTC().UnixMicro(), 10)
	if q.Get("start") != wantStart {
		t.Errorf("start = %q，期望 %q（必须是微秒）", q.Get("start"), wantStart)
	}
	if q.Get("end") != wantEnd {
		t.Errorf("end = %q，期望 %q（必须是微秒）", q.Get("end"), wantEnd)
	}

	// 显式钉住「不能是秒级」：这条是为了让上面的陷阱回归时被抓住。
	if sec := strconv.FormatInt(testWindow().Start.UTC().Unix(), 10); q.Get("start") == sec {
		t.Errorf("start 用了秒级时间戳 %q —— Jaeger 需要微秒", sec)
	}
	if q.Get("service") != "checkout" {
		t.Errorf("service = %q，期望 checkout", q.Get("service"))
	}

	// 向后端请求的 limit 必须**大于**留给证据的 topN：
	// 只要 topN 条就没法在全部结果里挑最慢的，排序会失去意义。
	limit, convErr := strconv.Atoi(q.Get("limit"))
	if convErr != nil {
		t.Fatalf("limit %q 不是整数: %v", q.Get("limit"), convErr)
	}
	if limit <= c.topN {
		t.Errorf("向后端请求 %d 条 <= topN %d：排序只能作用在后端给的任意子集里", limit, c.topN)
	}
}

func TestTraceCollector_OrdersByDurationAndHonorsTopN(t *testing.T) {
	// 故意让快的排在前面、慢的排在后面（后端返回顺序不保证）。
	body := `{"data":[` +
		traceAt("aaaaaaaaaaaaaaaa4444", 1*time.Minute, 100, ``) + `,` +
		traceAt("bbbbbbbbbbbbbbbb5555", 2*time.Minute, 900_000, ``) + `,` +
		traceAt("cccccccccccccccc6666", 3*time.Minute, 50_000, ``) + `]}`
	fx := &traceFixture{body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fx.body))
	}))
	t.Cleanup(srv.Close)
	c := NewTraceCollector(srv.URL, WithTopN(2))

	got, err := c.Collect(context.Background(), evidence.Input{Service: "pay", Window: testWindow()})
	if err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("证据条数 = %d，期望按 topN 保留 2 条\n%s", len(got), dumpEvidence(got))
	}
	// 第一条必须是最慢的：值班要看的是「最慢的那几条」，
	// 只看 N 条而不排序等于把最关键的链路随机丢掉。
	if !strings.Contains(got[0].Metadata["trace_id"], "5555") {
		t.Errorf("首位应是最慢的 5555，实际 %s\n%s", got[0].Metadata["trace_id"], dumpEvidence(got))
	}
	if w := got[0].Metadata[traceWarnings]; !strings.Contains(w, "Top 2") {
		t.Errorf("截断时应留下说明，实际 metadata: %s", w)
	}
}

func TestTraceCollector_RequiresBackendAddress(t *testing.T) {
	c := NewTraceCollector("")
	if _, err := c.Collect(context.Background(), evidence.Input{Service: "x", Window: testWindow()}); err == nil {
		t.Fatal("未配置地址时应报错：静默返回空会让「没配地址」与「没有链路数据」长得一样")
	}
}

func TestTraceCollector_EmptyServiceDoesNotQuery(t *testing.T) {
	fx := &traceFixture{body: `{"data":[]}`}
	c, _ := newTraceServer(t, fx)

	got, err := c.Collect(context.Background(), evidence.Input{Service: "   ", Window: testWindow()})
	if err != nil {
		t.Fatalf("服务名为空不是错误——那是「本事件没有可用服务标识」: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("证据条数 = %d，期望 0\n%s", len(got), dumpEvidence(got))
	}
	// 关键不是「返回空」，而是**没去问后端**：
	// 空 service 在不少后端等价于「不限服务」，会把整个集群的链路都捞回来。
	if fx.hits != 0 {
		t.Errorf("后端被调用了 %d 次，期望 0 —— 空服务名必须短路，不能拿空 label 去查", fx.hits)
	}
}

func TestTraceCollector_EmptyResultIsSuccessNotFailure(t *testing.T) {
	fx := &traceFixture{body: `{"data":[]}`}
	c, _ := newTraceServer(t, fx)

	got, err := c.Collect(context.Background(), evidence.Input{Service: "pay", Window: testWindow()})
	if err != nil {
		t.Fatalf("查成功但没数据属正常结果，不该报错: %v", err)
	}
	if got == nil {
		t.Fatal("返回 nil 切片：成功的空结果应为非 nil 空切片（调用方会对 nil 做 len==0 判断，此处保持一致）")
	}
	if len(got) != 0 {
		t.Errorf("证据条数 = %d，期望 0", len(got))
	}
	// 「零条」与「查询失败」必须是三态里的两态。
	// 这条与上面的 RequiresBackendAddress 互为对照：只会有一条成立。
}

func TestTraceCollector_BackendErrorIsReported(t *testing.T) {
	fx := &traceFixture{status: http.StatusInternalServerError}
	c, _ := newTraceServer(t, fx)
	if _, err := c.Collect(context.Background(), evidence.Input{Service: "pay", Window: testWindow()}); err == nil {
		t.Fatal("后端 500 应报错")
	}
}

func TestTraceCollector_SkipsTraceWithoutUsableStartTime(t *testing.T) {
	// startTime 给成不可解析的字符串：这条必须被丢弃（且不影响同批其他 trace），
	// 而不是拿窗口起点或 now 兜底 —— 那会让「这条 trace 什么时候发生的」变成假的。
	bad := `{"traceID":"deadbeef","spans":[{"spanID":"s1","operationName":"op","startTime":"oops","duration":10}]}`
	good := traceAt("aaaaaaaaaaaaaaaa7777", 4*time.Minute, 100, ``)
	fx := &traceFixture{body: `{"data":[` + bad + `,` + good + `]}`}
	c, _ := newTraceServer(t, fx)

	got, err := c.Collect(context.Background(), evidence.Input{Service: "pay", Window: testWindow()})
	if err != nil {
		t.Fatalf("部分数据不可用不该让整批失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("证据条数 = %d，期望 1（坏的那条应被丢弃）\n%s", len(got), dumpEvidence(got))
	}
	if got[0].Metadata["trace_id"] != "aaaaaaaaaaaaaaaa7777" {
		t.Errorf("留下的应是那条时间可用的 trace，实际 %s", got[0].Metadata["trace_id"])
	}
	if w := got[0].Metadata[traceWarnings]; !strings.Contains(w, "起始时间") {
		t.Errorf("丢弃应留下原因说明，实际 metadata: %s", w)
	}
}

func TestTraceErrorSpanCounting(t *testing.T) {
	cases := []struct {
		name string
		tags string
		want int
	}{
		{"bool 型 error", `,"tags":[{"key":"error","type":"bool","value":true}]`, 1},
		{"字符串型 error", `,"tags":[{"key":"error","type":"string","value":"true"}]`, 1},
		{"http 500", `,"tags":[{"key":"http.status_code","type":"int64","value":500}]`, 1},
		{"http 200 不算", `,"tags":[{"key":"http.status_code","type":"int64","value":200}]`, 0},
		{"无标签", ``, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr traceData
			body := traceAt("aaaaaaaaaaaaaaaa8888", 1*time.Minute, 10, tc.tags)
			if err := json.Unmarshal([]byte(body), &tr); err != nil {
				t.Fatalf("fixture 不是合法 JSON: %v", err)
			}
			if got := tr.errorSpanCount(); got != tc.want {
				t.Errorf("errorSpanCount() = %d，期望 %d", got, tc.want)
			}
		})
	}
}

func TestTraceRootSpanHeuristics(t *testing.T) {
	t.Run("有空父 id 的即根", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[
			{"spanID":"a","startTime":100,"duration":1},
			{"spanID":"b","parentSpanID":"zzz","startTime":90,"duration":9999}]}`, &tr)
		if r := tr.rootSpan(); r == nil || r.SpanID != "a" {
			t.Fatalf("root = %+v，期望 spanID=a", r)
		}
	})

	t.Run("父未被上报时退化取结束最晚的", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[
			{"spanID":"a","parentSpanID":"missing","startTime":100,"duration":1},
			{"spanID":"b","parentSpanID":"missing","startTime":90,"duration":50}]}`, &tr)
		// a: 100+1=101；b: 90+50=140 —— b 覆盖的时间更长。
		if r := tr.rootSpan(); r == nil || r.SpanID != "b" {
			t.Fatalf("root = %+v，期望 spanID=b（起始最早 + 结束最晚）", r)
		}
	})

	t.Run("全部时间不可用时返回 nil 而非乱指一个", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[{"spanID":"a","parentSpanID":"x","startTime":"nope","duration":1}]}`, &tr)
		if r := tr.rootSpan(); r != nil {
			t.Fatalf("root = %+v，期望 nil：没有可用时间就不该猜", r)
		}
	})
}

func TestTraceServiceNameResolution(t *testing.T) {
	t.Run("span 内联 process 优先", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[{"spanID":"a","processID":"p1",
			"process":{"serviceName":"inline"}}],"processes":{"p1":{"serviceName":"pool"}}}`, &tr)
		if got := tr.Spans[0].serviceName(tr); got != "inline" {
			t.Errorf("serviceName = %q，期望 inline", got)
		}
	})
	t.Run("降级到 process 池", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[{"spanID":"a","processID":"p1"}],
			"processes":{"p1":{"serviceName":"pool"}}}`, &tr)
		if got := tr.Spans[0].serviceName(tr); got != "pool" {
			t.Errorf("serviceName = %q，期望 pool", got)
		}
	})
	t.Run("两者都没有时返回空而不是猜一个", func(t *testing.T) {
		var tr traceData
		parseFixture(t, `{"traceID":"t","spans":[{"spanID":"a","processID":"missing"}]}`, &tr)
		if got := tr.Spans[0].serviceName(tr); got != "" {
			t.Errorf("serviceName = %q，期望空", got)
		}
	})
}

func TestTraceCollector_Registration(t *testing.T) {
	reg := evidence.NewRegistry()
	if err := reg.Register(NewTraceCollector("http://x")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	col, ok := reg.Get(SourceTrace)
	if !ok {
		t.Fatalf("按 %q 取不到采集器", SourceTrace)
	}
	if col.Source() != SourceTrace {
		t.Errorf("自报来源 = %q，期望 %q", col.Source(), SourceTrace)
	}
	// 重复注册必须报错而非静默覆盖 —— 两个采集器写同一个 source
	// 会让「证据来自哪个系统」变得事后无法追查。
	if err := reg.Register(NewTraceCollector("http://y")); err == nil {
		t.Error("重复注册应报错")
	}
}

func TestParseUnixMicros(t *testing.T) {
	cases := []struct {
		raw    string
		want   int64
		usable bool
	}{
		{`1699999999000000`, 1699999999000000, true},
		{`"1699999999000000"`, 1699999999000000, true},
		{`1.699999999e15`, 1699999999000000, true},
		{`"oops"`, 0, false},
		{`null`, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseUnixMicros(json.RawMessage(tc.raw))
		if ok != tc.usable {
			t.Errorf("parseUnixMicros(%s) 可用性 = %v，期望 %v", tc.raw, ok, tc.usable)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseUnixMicros(%s) = %d，期望 %d", tc.raw, got, tc.want)
		}
	}
}

// parseFixture 把测试用的 JSON 灌进结构体。
//
// 名字刻意不叫 mustJSON：**包内已有一个同名的反向 helper**
// （k8s_test.go 的 mustJSON(v any) string 是「序列化」）。
// 两个方向相反的操作同名，看调用点的人会猜错方向。
//
// fixture 解析失败直接判测试失败：那是测试自身的 bug，
// 与被测逻辑无关，应当第一时间以最响的方式暴露。
func parseFixture(t *testing.T, body string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("fixture 解析失败: %v\n%s", err, body)
	}
}
