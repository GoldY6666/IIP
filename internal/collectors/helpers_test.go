package collectors

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yu/iip/internal/evidence"
)

// 本文件放**两个采集器共用**的测试辅助。
//
// 单独成文件而不是塞进 prometheus_test.go：
// Loki 的用例要是不依赖 Prometheus 的测试文件才能编译，
// 删掉 Prometheus 测试就会连带把 Loki 测试搞坏 ——
// 而两者本该能各自独立演进。

// testWindow 返回一个固定的采集窗口，起点为 Unix 整点便于构造样本时间。
//
// **必须是固定值而非 time.Now() 派生**：断言里要比对
// 「基线窗口 = [Start-width, Start)」，若窗口随调用时刻漂移，
// mock server 的分派逻辑与断言就要各算一遍时间，
// 两者一旦算得不一样，测试会假通过或假失败。
func testWindow() evidence.Window {
	end := time.Unix(1_700_000_000, 0).UTC()
	return evidence.Window{Start: end.Add(-time.Hour), End: end}
}

// testLine 造一条时间落在**事故窗口内**的日志。
//
// 为什么不用 time.Now() 随手造：testWindow 是固定在 2023 年的，
// 而 time.Now() 是 2026 年 —— 返回的日志全在查询区间之外。
// 采集器信任 Loki、不校验区间，测试照样绿，
// 但那样的测试证明不了「证据被关联到了正确的时间段」，
// 反而会在有人给采集器加上区间校验时集体炸掉。
//
// offset 是相对窗口起点的偏移，负值表示落在基线窗口。
func testLine(offset time.Duration, line string) lokiLine {
	return lokiLine{ts: testWindow().Start.Add(offset), line: line}
}

// testLineInWindow 造一条时间落在**基线窗口内**的日志。
//
// 单独一个函数而不是让人写负offset：基线窗口是 [Start-width, Start)，
// 「-30m」这种写法得先在脑子里换算一遍才知道落没落在里面，
// 而踩边界（正好用 Start）会因两窗首尾相接而出错。
func testLineInBaseline(line string) lokiLine {
	return lokiLine{ts: testWindow().Start.Add(-30 * time.Minute), line: line}
}

// dumpEvidence 把证据列表渲染成可读文本，用于失败信息。
//
// 断言失败时只说「期望 3 条，实际 2 条」是不够的 ——
// 调试者还得回去加临时打印才能看到到底采到了什么。
// 让失败信息自足能省掉一整轮往返。
func dumpEvidence(list []evidence.Evidence) string {
	if len(list) == 0 {
		return "  （空）"
	}
	var b strings.Builder
	for i, e := range list {
		b.WriteString(fmt.Sprintf("  [%d] type=%s src=%s ts=%s\n      value=%s\n      query=%s\n      md=%s\n",
			i, e.Type, e.Source, e.Timestamp.UTC().Format("15:04:05"),
			truncate(e.Value, 120), truncate(e.Query, 120), formatMetadata(e.Metadata)))
	}
	return b.String()
}

// dumpPatterns 把错误模式列表渲染成可读文本。
func dumpPatterns(list []logPattern) string {
	if len(list) == 0 {
		return "  （空）"
	}
	var b strings.Builder
	for i, p := range list {
		b.WriteString(fmt.Sprintf("  [%d] count=%d key=%q\n", i, p.count, p.key))
	}
	return b.String()
}

// formatMetadata 把 metadata 渲染成键值对，便于在失败信息里比对。
//
// 刻意排序输出：map 遍历顺序随机，
// 不排序的话同一份数据每次打印顺序都不同，diff 起来全是噪声。
func formatMetadata(md map[string]string) string {
	if len(md) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, md[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
