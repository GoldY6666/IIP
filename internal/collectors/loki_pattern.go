package collectors

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// lokiEntry 是一条错误日志。
type lokiEntry struct {
	// ts 是日志自身的时间（纳秒精度来自 Loki，落库时截到秒）。
	ts time.Time
	// line 是日志正文，归一化后用于聚合。
	line string
}

// logPattern 是一个归一化后的错误模式及其统计。
type logPattern struct {
	// key 是归一化后的模式（数字与引号已抹掉）。
	key string
	// count 是事故窗口内命中该模式的条数。
	count int
	// sample 是该模式的一条**原文**样本，进evidence.value 供人阅读。
	//
	// 保留原文而不是归一化后的 key：key 是给聚合用的，
	// 「10.0.0.5 连接超时」与「10.0.0.9 连接超时」聚合后是同一个 key，
	// 但运维需要看到真实的那一行。
	sample string
	// last 是该模式最后一次出现的时刻。
	last time.Time
}

// repr 返回适合放进错误信息的短标识。
func (p logPattern) repr() string {
	return truncate(p.key, 60)
}

// topPatterns 把日志按模式聚合并取前 n 个。
func topPatterns(entries []lokiEntry, n int) []logPattern {
	if n <= 0 {
		n = 10
	}
	buckets := make(map[string]*logPattern, len(entries))
	for _, e := range entries {
		k := normalizeLogLine(e.line)
		b, ok := buckets[k]
		if !ok {
			b = &logPattern{key: k, sample: e.line}
			buckets[k] = b
		}
		b.count++
		if e.ts.After(b.last) {
			b.last = e.ts
		}
	}

	out := make([]logPattern, 0, len(buckets))
	for _, p := range buckets {
		out = append(out, *p)
	}
	// 按条数降序；条数相同时按 key 升序 —— 字典序兜底让结果**可复现**，
	// 否则 map 遍历顺序会让每次采集的 Top N 顺序都不同，
	// 页面上的证据顺序会随机跳动，测试也无法断言。
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].key < out[j].key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// normalizeLogLine 抹掉会��每条日志都不同的部分，让同类错误聚到一起。
//
// 具体抹三类：
//   - 十六进制/UUID 形态的 token（trace id、span id）；
//   - 纯数字（含小数，如耗时 12.34ms、端口 8080、错误码 500）；
//   - 连续引号内的内容（常含 id、路径、sql片段）。
//
// 目的是让「同一条业务逻辑抛出的 1000 条日志」聚成 1 条。
// 抹得太狠会���不同故障合并（那比漏聚合更糟），
// 因此只抹**格式上确定是变量**的部分，不做语义猜测。
func normalizeLogLine(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		switch ch := line[i]; {
		case ch == '"' || ch == '\'':
			// 引号内内容整体抹成 ?，含引号本身。
			quote := ch
			j := i + 1
			for j < len(line) && line[j] != quote {
				j++
			}
			if j < len(line) {
				j++ // 把结束引号也吃掉
			}
			b.WriteString("?")
			i = j
		case ch >= '0' && ch <= '9':
			j := i
			for j < len(line) && (isDigit(line[j]) || line[j] == '.') {
				j++
			}
			// 结尾的点是标点而非小数部分（"value is 1."）时不能整段吃掉，
			// 否则相邻的两个句子会被粘成一个 token，聚类就失真了。
			if line[j-1] == '.' {
				j--
			}
			// 判据是数字**前面**紧跟字母，说明它是标识符的一部分
			// （err500、http2）—— 整段保留，拆开反而制造出假的「同一模式」。
			//
			// 注意不能看后面：`5000ms` 后面也跟字母，但那是**单位**不是标识符，
			// 抹掉数字才正是我们想要的（耗时会变，错误类型不会变）。
			if i > 0 && isASCIILetter(line[i-1]) {
				b.WriteString(line[i:j])
				i = j
				continue
			}
			b.WriteString("#")
			i = j
		default:
			b.WriteByte(ch)
			i++
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// buildLogql 构造可复现的 LogQL 查询（手册验收标准第 2 条）。
//
// 窗口**不进**表达式：Loki 的时间范围是 query_range 的 start/end 参数，
// 写进 LogQL 反而会让「复制到 Grafana 复现」变复杂。
// 证据的 window 字段已经记下了确切区间。
//
// 服务名交给 %q 转义而不是手工拼引号：Go 的 strconv.Quote 与 LogQL
// 字符串字面量的转义规则一致（\" \\ \n），两者手工对齐过一次，
// 但手工那版漏过「服务名含换行」这种边角。
func buildLogql(service string) string {
	return fmt.Sprintf(`{service=%q} |~ %q`, service, errorPattern)
}
