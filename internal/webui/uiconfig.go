// 运行时 UI 配置注入（M5 S2，规格见 docs/IIP-M5详细开发手册.md §4.2.2.1）。
//
// 解决的问题：前端的「跳原始面板」链接需要**各面板的浏览器可达地址**，
// 而这些地址是**部署形态**的事 —— 容器内是 prometheus:9090，
// 宿主机裸跑是 localhost:9090，生产又是另一套。产物是同一份。
//
// 做法：后端在返回 index.html 时，把四段地址内联成
// `window.__IIP_UI__ = {...}`。前端在 uiConfig.ts 里读取。
//
// **为什么不用其他方式**（初稿 §4.2.2 遗漏了这条通道，实现时才暴露）：
//   - vite VITE_*  → 改地址要重新构建前端，且构建机的值会被烤进产物
//   - 新增 API 端点 → 违反 M5 范围「不新增后端端点」
//   - 独立 config.js → 多一次异步加载，且 404 时要有兜底
//
// **注入内容不含任何密钥**：只有地址与 tenant id。
// 采集器的凭据不注入 —— 那些是服务端到服务端的连接配置，
// 泄露它们只会帮攻击者拿到 Prometheus / Loki 的写权限。
package webui

import (
	"fmt"
	"strings"
)

// uiConfigGlobalName 是前端读取用的全局变量名。
//
// 两侧必须一致：Go 侧在这里写，前端 api/uiConfig.ts 里读。
// 改了任何一边都要改另一边 —— 故此常量在 Go 与 TS 两侧各写一份字面量，
// 而不是让前端从某个 .env 读（那样就变成两份配置而不是一个契约）。
const uiConfigGlobalName = "__IIP_UI__"

// uiConfigScriptMarker 是 index.html 里注入点的占位符。
//
// **为什么用显式占位符而不是字符串替换 `<head>`**：改 head 会与 vite
// 未来调整 head 内容打架，且无法表达「我要插在 head 之后、body 之前」。
// 占位符写进 index.html 时必须以注释形式存在（合法 HTML、渲染无影响）。
const uiConfigScriptMarker = "<!--iip-ui-config-->"

// UIConfig 是注入给前端的运行时配置。
//
// 字段名用**小驼峰**而非 Go 惯例的 PascalCase：它们要原样出现在
// JavaScript 对象字面量里，用 Go 名会让注入的代码需要一层转换，
// 而那层转换就是「改名出错」的地方。
type UIConfig struct {
	Prometheus   string `json:"prometheus"`
	Grafana      string `json:"grafana"`
	Loki         string `json:"loki"`
	LokiTenantID string `json:"lokiTenantId"`
}

// defaultUIPrometheus 等是**无配置时的默认值**。
//
// 取 localhost：UI 链接是给人在浏览器里点的，浏览器解析宿主机地址，
// 不是容器网络地址。compose 已把 9090/3000/3100 映射到宿主机，故 localhost
// 对默认部署开箱可用。反之若填 `prometheus:9090`，用户点的每个链接都打不开 ——
// 那比「地址没配对」更难排查（会以为是 Grafana 挂了）。
const (
	defaultUIPrometheus   = "http://localhost:9090"
	defaultUIGrafana      = "http://localhost:3000"
	defaultUILoki         = "http://localhost:3100"
	defaultUILokiTenantID = ""
)

// WithDefaults 用默认值补齐空的字段。
//
// 逐字段补而**不是**整个对象回落默认值：只配了 Prometheus 时，
// Grafana 与 Loki 仍该用各自的 localhost 默认（全回落会让已配的那项也丢）。
func (c UIConfig) WithDefaults() UIConfig {
	if strings.TrimSpace(c.Prometheus) == "" {
		c.Prometheus = defaultUIPrometheus
	}
	if strings.TrimSpace(c.Grafana) == "" {
		c.Grafana = defaultUIGrafana
	}
	if strings.TrimSpace(c.Loki) == "" {
		c.Loki = defaultUILoki
	}
	return c
}

// RenderUIConfigScript 返回注入用的 <script> 片段（不含 <script> 标签本身）。
//
// 单独返回内容而不含标签，是为了替换占位符时由调用方决定
// 「连标签一起替换」还是「只替换标签内的内容」——
// 后者需要 index.html 里已有标签，本实现走的是**替换占位符**路线。
//
// 大小写固定为全局变量名 + `=`，末尾**不带分号**：不加分号能让
// 前端的 tsc 与 ESLint 少一类「 ASI 陷阱」相关的误报。
func RenderUIConfigScript(c UIConfig) string {
	c = c.WithDefaults()
	// 手写字面量而不是 json.Marshal：
	// 四个值都是「用户在 env 里写的字符串」，用 json.Marshal 反而要处理
	// HTML 上下文里的 < / > / & 转义（json.Marshal 不做 HTML 转义），
	// 那才是真正的注入风险点。手写时统一过一个转义函数即可，
	// 覆盖面更小、更容易审。
	return fmt.Sprintf("window.%s={prometheus:%s,grafana:%s,loki:%s,lokiTenantId:%s}",
		uiConfigGlobalName,
		jsString(c.Prometheus), jsString(c.Grafana),
		jsString(c.Loki), jsString(c.LokiTenantID))
}

// jsString 把 Go 字符串转成 JavaScript 字符串字面量（含引号）。
//
// 必做的两件事：
//  1. 反斜杠与引号转义 —— 否则地址里带 " 会截断整个 script
//  2. **HTML 上下文转义**：`</script>` 出现在字符串里会**提前结束 script 标签**，
//     后面内容变成正文。把 '<' 写成 JS 的 \u003c 转义形式、'&' 写成 \u0026，
//     浏览器解析出的字符串**完全等价**，但 HTML 解析器看不到字面的
//     '<' 与 '&'，因此不会提前闭合 script。
//     即便正常运维不会在地址里写这些，配置是外部输入。
//
// 注意第 2 点的转义序列是用**显式的反斜杠字符**写出来的，
// 而不是直接写 "<\u003c" —— 后者在编写本文档的工具链里会被提前解析成
// 真正的 '<' 字符，防护就悄悄失效了。
func jsString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '<':
			b.WriteString(jsEscLessThan)
		case '&':
			b.WriteString(jsEscAmpersand)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsEscLessThan 与 jsEscAmpersand 是 JS 的 \u 转义形式，
// 分别代表字符 < 与 &。
//
// 单独定义为常量、用 \u00XX 的字面写法而不是直接写字符：
// 编写本文档的工具链会把 \\u00XX 提前解析成真正的字符，
// 那样这层防护会**静默失效** —— 而失效的后果是 XSS。
// 用常量名表达意图，也让 review 时一眼能看出这里在防什么。
const (
	jsEscLessThan  = `\u003c`
	jsEscAmpersand = `\u0026`
)

// InjectUIConfig 把配置注入 index.html 的内容。
//
// 找不到占位符时**原样返回并说明**，不报错：index.html 可能是用户自定义的
// 合法页面（没放占位符），此时前端会退回 DEFAULT_UI_ENDPOINTS，
// 功能仍可用，只是不能用运行时配置。**为一个可选优化让整个 UI 500 是不划算的。**
func InjectUIConfig(html string, c UIConfig) (string, bool) {
	if !strings.Contains(html, uiConfigScriptMarker) {
		return html, false
	}
	script := "<script>" + RenderUIConfigScript(c) + "</script>"
	return strings.Replace(html, uiConfigScriptMarker, script, 1), true
}
