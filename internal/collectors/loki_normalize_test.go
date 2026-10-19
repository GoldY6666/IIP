package collectors

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// TestNormalizeLogLine 归一化规则的直接单测。
//
// 这是整个采集器最容易被误改的地方：抹得太狠会把不同故障合并
// （那比漏聚合更糟），抹得太松则Top N 会被一条日志刷满。
func TestNormalizeLogLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "不同耗时的同类超时聚成一条",
			in:   "db timeout after 5000ms",
			want: "db timeout after #ms",
		},
		{
			name: "错误码与端口也被抹掉",
			in:   "connect to 10.0.0.5:8080 failed, code=500",
			want: "connect to #:# failed, code=#",
		},
		{
			name: "引号内的 id 被抹掉",
			in:   `user "u-8891" not found`,
			want: "user ? not found",
		},
		{
			name: "标识符里的数字不拆（err500 仍是一个词）",
			in:   "err500 raised",
			want: "err500 raised",
		},
		{
			name: "字母紧跟的数字不拆（http2）",
			in:   "http2 stream reset",
			want: "http2 stream reset",
		},
		{
			name: "无数字无引号的行原样保留",
			in:   "connection pool exhausted",
			want: "connection pool exhausted",
		},
		{
			name: "结尾是小数点时数字仍抹掉、点保留",
			in:   "value is 1.",
			want: "value is #.",
		},
		{
			name: "小数被归一化",
			in:   "took 12.34ms",
			want: "took #ms",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeLogLine(tc.in); got != tc.want {
				t.Errorf("normalizeLogLine(%q)\n got = %q\nwant = %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeLogLineGroupsRealisticBurst 验证真实突增场景能聚成一条。
//
// 数据库连接池打满会刷出成百上千条只有数字不同的日志，
// 不聚合的话 Top N 全是同一句话，等于没有信息。
func TestNormalizeLogLineGroupsRealisticBurst(t *testing.T) {
	var entries []lokiEntry
	now := time.Now()
	for i := 0; i < 200; i++ {
		entries = append(entries, lokiEntry{
			ts:   now.Add(-time.Duration(i) * time.Second),
			line: fmt.Sprintf("dial tcp 10.0.%d.%d:5432: connect: connection refused", i%250, (i*7)%250),
		})
	}
	patterns := topPatterns(entries, 10)
	if len(patterns) != 1 {
		t.Fatalf("200 条同类连接错误应聚成 1 类，实际 %d 类:\n%s",
			len(patterns), dumpPatterns(patterns))
	}
	if patterns[0].count != 200 {
		t.Errorf("聚类条数 = %d，期望 200", patterns[0].count)
	}
	// 样本应保留一条原文（供人阅读），而不是归一化后的 key
	if !strings.Contains(patterns[0].sample, "dial tcp") {
		t.Errorf("样本应是原文，实际 = %q", patterns[0].sample)
	}
}

// TestTopPatternsOrderIsStable 验证排序可复现。
//
// map 遍历顺序随机 → 每次采集 Top N 顺序都不同 →
// 页面证据顺序随机跳动，测试也无法断言「第 1 类是哪个」。
func TestTopPatternsOrderIsStable(t *testing.T) {
	now := time.Now()
	var entries []lokiEntry
	// 三类各2 条，条数完全相同 —— 此时只能靠 key 字典序决定顺序
	for _, line := range []string{"alpha error", "beta error", "gamma error"} {
		for i := 0; i < 2; i++ {
			entries = append(entries, lokiEntry{ts: now.Add(-time.Duration(i) * time.Minute), line: line})
		}
	}
	first := topPatterns(entries, 3)
	want := []string{"alpha error", "beta error", "gamma error"}
	if len(first) != len(want) {
		t.Fatalf("应聚成 %d 类，实际 %d 类:\n%s", len(want), len(first), dumpPatterns(first))
	}
	// 连跑 20 次：map 遍历顺序随机，单次比对可能碰巧通过。
	// 这里要证明的是**排序规则本身**稳定，而不是某一次的结果对。
	for run := 0; run < 20; run++ {
		got := topPatterns(entries, 3)
		for i := range want {
			if got[i].key != want[i] {
				t.Fatalf("第 %d 次运行第 %d 类 = %q，期望 %q —— 排序不稳定",
					run, i, got[i].key, want[i])
			}
		}
	}
}

// TestLokiImplementsCollector 编译期契约：Loki 采集器必须满足接口。
func TestLokiImplementsCollector(t *testing.T) {
	var c evidence.Collector = NewLokiCollector("http://localhost:3100")
	if c.Source() != SourceLoki {
		t.Errorf("Source() = %q，期望 %q", c.Source(), SourceLoki)
	}
	// 注册表接受它
	reg := evidence.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Errorf("注册失败: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("注册表长度 = %d，期望 1", reg.Len())
	}
}
