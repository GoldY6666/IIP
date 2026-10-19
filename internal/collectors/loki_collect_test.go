package collectors

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// TestLokiCollectsTopPatternsAndBurst 是核心用例：
// 同一窗口内两类错误各多条 → 聚合成 2 条模式证据 + 1 条突增证据。
func TestLokiCollectsTopPatternsAndBurst(t *testing.T) {
	// 日志时间必须落在**被查询的窗口内**。
	//
	// 这一点很容易写错：mock 随手用 time.Now() 造时间戳，
	// 而 testWindow 是固定在 2023 年的，于是返回的日志全在窗口之外。
	// 采集器信任 Loki、不会校验区间，测试照样绿 ——
	// 但那样的测试证明不了「我们把证据关联到了正确的时间段」。
	base := testWindow().Start
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			// 基线：只有 1 条 db 超时。
			// 时间取 Start-30min —— 基线窗口是 [Start-1h, Start)，
			// 用 Start 本身会正好压在边界上（两窗首尾相接、不重叠）。
			writeLokiResult(t, w, lokiLine{
				ts: base.Add(-30 * time.Minute), line: "db timeout after 5000ms",
			})
			return
		}
		// 事故窗口：db 超时 3 条 + 空指针 2 条
		//
		// 两类都写成「同类不同变量」：超时的耗时不同、空指针的行号不同 ——
		// 这才是真实的突增形态。若写成两条字面完全一样的日志，
		// 测的就不是归一化而只是计数了。
		writeLokiResult(t, w,
			lokiLine{ts: base.Add(10 * time.Minute), line: "db timeout after 5000ms"},
			lokiLine{ts: base.Add(11 * time.Minute), line: "db timeout after 7000ms"},
			lokiLine{ts: base.Add(12 * time.Minute), line: "db timeout after 3000ms"},
			lokiLine{ts: base.Add(13 * time.Minute), line: "nil pointer dereference at handler.go:142"},
			lokiLine{ts: base.Add(14 * time.Minute), line: "nil pointer dereference at handler.go:207"},
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集不应失败: %v", err)
	}
	// 2 类模式 + 1 条突增
	if len(got) != 3 {
		t.Fatalf("采到 %d 条证据，期望 3 条（2 类模式 + 1 条突增）:\n%s",
			len(got), dumpEvidence(got))
	}

	// 第 1 类应是 db timeout（3 条 > 2 条）
	first := got[0]
	if first.Type != evidence.TypeLog {
		t.Errorf("第 0 条 type = %q，期望 %q", first.Type, evidence.TypeLog)
	}
	if first.Source != SourceLoki {
		t.Errorf("第 0 条 source = %q，期望 %q", first.Source, SourceLoki)
	}
	if first.Metadata["count"] != "3" {
		t.Errorf("第 0 类错误条数 = %q，期望 3（db timeout 出现 3 次）", first.Metadata["count"])
	}
	if first.Metadata["rank"] != "1" {
		t.Errorf("第 0 类 rank = %q，期望 1", first.Metadata["rank"])
	}
	// 归一化后 "db timeout after #ms" 三条必须聚成一条
	if !strings.Contains(first.Metadata["pattern"], "db timeout") {
		t.Errorf("第 0 类pattern = %q，期望含 \"db timeout\"（数字应被归一化）",
			first.Metadata["pattern"])
	}
	// 数字必须已被抹掉，否则三条不会聚成一条
	if strings.Contains(first.Metadata["pattern"], "5000") {
		t.Errorf("第 0 类 pattern 仍含具体数字 %q —— 归一化没生效，同类错误没聚到一起",
			first.Metadata["pattern"])
	}
	// 基线有 1 条 → 不是新出现的错误
	if first.Metadata["novelty"] != "" {
		t.Errorf("基线已有同类错误，不该标记 novelty=%q", first.Metadata["novelty"])
	}
	if first.Metadata["count_before"] != "1" {
		t.Errorf("第 0 类 count_before = %q，期望 1", first.Metadata["count_before"])
	}

	// 突增证据：5 条 vs 1 条 = 5 倍
	burst := got[2]
	if burst.Metadata["count_after"] != "5" {
		t.Errorf("突增 count_after = %q，期望 5", burst.Metadata["count_after"])
	}
	if burst.Metadata["count_before"] != "1" {
		t.Errorf("突增 count_before = %q，期望 1", burst.Metadata["count_before"])
	}
	if burst.Metadata["change_ratio"] != "5.0000" {
		t.Errorf("突增 change_ratio = %q，期望 5.0000", burst.Metadata["change_ratio"])
	}

	// query 字段必须可复现：含服务名与行过滤正则
	if !strings.Contains(first.Query, `service="api"`) {
		t.Errorf("query 未含服务名 label 匹配: %q", first.Query)
	}
	if !strings.Contains(first.Query, errorPattern) {
		t.Errorf("query 未含错误行过滤正则: %q", first.Query)
	}
	// 证据时间必须来自日志自身，且不得为采集时刻
	for i, e := range got {
		if e.Timestamp.IsZero() {
			t.Errorf("第 %d 条证据时间为空 —— 违反 §4.4", i)
		}
		if e.Timestamp.After(time.Now()) {
			t.Errorf("第 %d 条证据时间 %v 晚于当前时刻 —— 疑似用了采集时刻",
				i, e.Timestamp)
		}
		// 时间必须落在被查询的窗口内。
		//
		// 这条断言是补上一个真实的测试盲区：mock 曾用 time.Now() 造时间戳，
		// 而窗口固定在2023 年 —— 返回的日志全在区间之外。
		// 采集器信任 Loki 不做校验，测试照样绿，
		// 于是「证据关联到了正确时间段」这件事**从未被验证过**。
		if e.Timestamp.Before(testWindow().Start) || !e.Timestamp.Before(testWindow().End) {
			t.Errorf("第 %d 条证据时间 %v 不在采集窗口 [%v, %v) 内",
				i, e.Timestamp.UTC(), testWindow().Start.UTC(), testWindow().End.UTC())
		}
	}
	// 突增证据的时间应是最后一条错误日志的时刻，而不是窗口终点。
	//
	// 比较前把期望值也截到微秒：Loki 给纳秒而 PG 只存微秒，
	// 采集器侧已按 PG 精度截断（见 loki.go 的注释），两边必须同一口径。
	wantLast := base.Add(14 * time.Minute).UTC().Truncate(time.Microsecond)
	if !burst.Timestamp.Equal(wantLast) {
		t.Errorf("突增证据时间 = %v，期望等于最后一条错误日志时刻 %v",
			burst.Timestamp, wantLast)
	}
}

// TestLokiNoLogsIsNotFailure 锁定「无错误日志」是正常结果。
//
// 这是最容易搞错的语义：事故窗口没有错误日志说明服务很健康，
// 若当成采集失败，任务会白白重试三次进死信，而页面永远等不到证据。
func TestLokiNoLogsIsNotFailure(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		writeLokiEmpty(t, w)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("无错误日志不该算采集失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("采到 %d 条证据，期望 0 条", len(got))
	}
}

// TestLokiNovelPatternIsFlagged 验证「新出现的错误模式」被单独标记。
//
// 事故前没有、现在有 —— 几乎必然与本次事故相关，
// 而长期存在的错误是背景噪声，两者不该同等呈现。
func TestLokiNovelPatternIsFlagged(t *testing.T) {
	base := time.Now().Add(-2 * time.Hour)
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			// 基线只有一种「无害」错误
			writeLokiResult(t, w, lokiLine{ts: base, line: "cache miss for key abc"})
			return
		}
		writeLokiResult(t, w,
			lokiLine{ts: base.Add(5 * time.Minute), line: "connection pool exhausted size=10"},
			lokiLine{ts: base.Add(6 * time.Minute), line: "connection pool exhausted size=20"},
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	// 1 类模式（pool exhausted，基线没有）+ 1 条突增
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条:\n%s", len(got), dumpEvidence(got))
	}
	if got[0].Metadata["novelty"] == "" {
		t.Errorf("基线无同类错误，应标记 novelty，实际 novelty=%q count_before=%q",
			got[0].Metadata["novelty"], got[0].Metadata["count_before"])
	}
	// 突增：2 条 vs 1 条基线
	if got[1].Metadata["change_ratio"] != "2.0000" {
		t.Errorf("突增 change_ratio = %q，期望 2.0000", got[1].Metadata["change_ratio"])
	}
}

// TestLokiBurstFromZeroHasNoRatio 验证「从 0涨到 N」不给假倍数。
//
// 事故前 0 条时 ratio 无意义（0 或 +Inf），
// 说成「事故前无错误日志，本次为首次出现」比给一个假倍数有用。
func TestLokiBurstFromZeroHasNoRatio(t *testing.T) {
	srv := newLokiServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if isBaselineRequest(r) {
			writeLokiEmpty(t, w)
			return
		}
		writeLokiResult(t, w,
			testLine(20*time.Minute, "fatal: out of memory"),
		)
	}

	c := NewLokiCollector(srv.URL)
	got, err := c.Collect(context.Background(), evidence.Input{
		IncidentID: 1, Service: "api", Window: testWindow(),
	})
	if err != nil {
		t.Fatalf("采集失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("采到 %d 条，期望 2 条", len(got))
	}
	burst := got[1]
	if burst.Metadata["change_ratio"] != "" {
		t.Errorf("事故前 0 条时不该给 change_ratio，实际 = %q —— 那是假倍数",
			burst.Metadata["change_ratio"])
	}
	if burst.Metadata["count_before"] != "0" {
		t.Errorf("突增 count_before = %q，期望 0", burst.Metadata["count_before"])
	}
	if burst.Metadata["burst"] == "" {
		t.Errorf("事故前 0 条时应说明 burst 情况")
	}
}
