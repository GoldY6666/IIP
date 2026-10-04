package webui

import (
	"reflect"
	"strings"
	"testing"
)

// TestWithDefaults 验逐字段回落。
//
// 关键：只配 Prometheus 时，Grafana 与 Loki 仍用自己的默认值 ——
// 整体回落会让「已配好的那项」也丢，那种症状是「明明配了却不生效」。
func TestWithDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   UIConfig
		want UIConfig
	}{
		{
			name: "全空则全用默认值",
			in:   UIConfig{},
			want: UIConfig{
				Prometheus: "http://localhost:9090",
				Grafana:    "http://localhost:3000",
				Loki:       "http://localhost:3100",
			},
		},
		{
			name: "只配 Prometheus，其余仍用默认值",
			in:   UIConfig{Prometheus: "http://p:9090"},
			want: UIConfig{
				Prometheus: "http://p:9090",
				Grafana:    "http://localhost:3000",
				Loki:       "http://localhost:3100",
			},
		},
		{
			name: "全配上则原样保留",
			in: UIConfig{
				Prometheus:   "http://p:9090",
				Grafana:      "http://g:3000",
				Loki:         "http://l:3100",
				LokiTenantID: "team-a",
			},
			want: UIConfig{
				Prometheus:   "http://p:9090",
				Grafana:      "http://g:3000",
				Loki:         "http://l:3100",
				LokiTenantID: "team-a",
			},
		},
		{
			name: "纯空白视为未配置（env 里常见 IIP_UI_LOKI_URL=）",
			in:   UIConfig{Loki: "   "},
			want: UIConfig{
				Prometheus: "http://localhost:9090",
				Grafana:    "http://localhost:3000",
				Loki:       "http://localhost:3100",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.WithDefaults()
			if got != tc.want {
				t.Errorf("WithDefaults() =\n %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestRenderUIConfigScriptBasic 验注入的基本形状。
func TestRenderUIConfigScriptBasic(t *testing.T) {
	got := RenderUIConfigScript(UIConfig{
		Prometheus: "http://p:9090",
		Grafana:    "http://g:3000",
		Loki:       "http://l:3100",
	})
	want := `window.__IIP_UI__={prometheus:"http://p:9090",grafana:"http://g:3000",` +
		`loki:"http://l:3100",lokiTenantId:""}`
	if got != want {
		t.Errorf("注入脚本 =\n %s\nwant %s", got, want)
	}
}

// TestRenderUIConfigScriptEscaping 是本文件最重要的一条。
//
// 环境变量的值是**外部输入**。若不转义，一个精心构造的
// IIP_UI_PROMETHEUS_URL 就能注入任意 JS —— 而这段脚本会内联进
// 每个用户打开的 index.html，等于持久化 XSS。
func TestRenderUIConfigScriptEscaping(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustNot  []string
		mustHave []string
	}{
		{
			name:    "双引号不截断脚本",
			in:      `http://x" ; alert(1) ; y="`,
			mustNot: []string{`x" ;`},
			// 引号被转义成 \" ，脚本结构保持完整
			mustHave: []string{`\"`},
		},
		{
			name:     "反斜杠被转义（否则 \\ 会吃掉后续引号的转义）",
			in:       `http://x\y`,
			mustHave: []string{`\\`},
		},
		{
			// </script> 出现在字符串里会**提前闭合 script 标签**，
			// 后面的内容变成页面正文（且我的配置被丢弃）。
			// 转义成 < 后 HTML 解析器看不到字面的 <。
			name:     "script 闭合标签不出现字面形式",
			in:       `http://x</script><img src=x onerror=alert(1)>`,
			mustNot:  []string{"</script>", "<img"},
			mustHave: []string{jsEscLessThan},
		},
		{
			// & 转义不是必需的（HTML 上下文里裸 & 基本无害），
			// 但与 < 一起转义能一劳永逸排除实体/编码歧义。
			// 断言的是「已转义成 JS 转义形式」这个事实，
			// 而不是「含 &」—— 转义后自然没有字面的 &。
			name:     "& 被转义为 JS 的 6 字符转义形式",
			in:       `http://x?a=1&b=2`,
			mustHave: []string{jsEscAmpersand},
		},
		{
			name:     "换行被转义（否则 JS 语法错误，整个脚本失效）",
			in:       "http://x\nalert(1)",
			mustHave: []string{`\n`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderUIConfigScript(UIConfig{Prometheus: tc.in})
			// script 标签本身由 InjectUIConfig 加，这里只验内容
			for _, bad := range tc.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("注入内容含危险子串 %q:\n%s", bad, got)
				}
			}
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("注入内容缺 %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestRenderUIConfigScriptNoSecrets 验注入内容里只有地址、没有采集器凭据。
//
// 采集器的 token / 租户密钥一旦内联进 HTML，等于公开给任何打开页面的人。
// 这条断言防的是「将来有人往 UIConfig 里加一个密钥字段」——
// 加的时候这条会红，迫使他先想清楚该不该内联进前端。
func TestRenderUIConfigScriptNoSecrets(t *testing.T) {
	allowed := map[string]bool{
		"Prometheus": true, "Grafana": true, "Loki": true, "LokiTenantID": true,
	}
	typ := reflect.TypeOf(UIConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !allowed[name] {
			t.Errorf("UIConfig 多了字段 %q。注入内容会出现在每个用户看到的 HTML 里，"+
				"确认它不是密钥；若是地址，请先加进上面的白名单并更新本用例", name)
		}
		if typ.NumField() != len(allowed) {
			t.Errorf("UIConfig 字段数 = %d，白名单 %d 个，两者应一致",
				typ.NumField(), len(allowed))
			break
		}
	}
}

// TestInjectUIConfig 验占位符替换。
func TestInjectUIConfig(t *testing.T) {
	cfg := UIConfig{Prometheus: "http://p:9090"}

	t.Run("有占位符时被替换且只替换一次", func(t *testing.T) {
		html := "<html><head>" + uiConfigScriptMarker + "<title>x</title></head><body></body></html>"
		got, ok := InjectUIConfig(html, cfg)
		if !ok {
			t.Fatal("ok = false，期望 true")
		}
		if strings.Contains(got, uiConfigScriptMarker) {
			t.Error("占位符未被替换")
		}
		if !strings.Contains(got, "<script>window.__IIP_UI__") {
			t.Errorf("未注入 script:\n%s", got)
		}
		// 页面其余内容必须完好
		if !strings.Contains(got, "<title>x</title>") {
			t.Errorf("注入破坏了页面其他内容:\n%s", got)
		}
	})

	// 找不到占位符时**不报错**：index.html 可能是用户自定义的合法页面。
	// 为一个可选优化让整个 UI 500 不划算。
	t.Run("无占位符时原样返回且 ok=false", func(t *testing.T) {
		html := "<html><body>custom</body></html>"
		got, ok := InjectUIConfig(html, cfg)
		if ok {
			t.Error("ok = true，期望 false")
		}
		if got != html {
			t.Errorf("内容被改动:\n%s", got)
		}
	})
}
