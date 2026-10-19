package config

import (
	"testing"
	"time"
)

// TestK8sDefaultOffDoesNotRequireNamespace 钉住偏差 B1 的「默认关闭」语义：
// 默认部署（K8s 未开启）不该被要求配任何 K8s 项。
//
// 这条与 TestK8sCollectorRequiresURL 是**一对**：只写后者的话，
// 有人把默认值误改成 true 也会全绿，而那会让默认部署直接起不来。
func TestK8sDefaultOffDoesNotRequireNamespace(t *testing.T) {
	c := m4Valid()
	c.EvidenceK8sEnabled = DefaultEvidenceK8sEnabled
	c.K8sURL = ""
	c.K8sNamespace = ""
	if c.EvidenceK8sEnabled {
		t.Fatal("前提失效：K8s 采集器默认值应为 false（B1 偏差）")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("K8s 默认关闭时不应要求任何 K8s 配置项: %v", err)
	}
}

// TestHTTPTimeoutSources 钉住 HTTPTimeout 的三条来源都真的生效：
// 默认值、flag 覆盖、环境变量覆盖。
//
// **三条必须分别验**：koanf 的posflag provider 与 env provider 是两条独立
// 代码路径，只验其中一条时，另一条挂掉（比如新文件漏调 readHTTPTimeout
// 只接上flag、或env 键名映射写错）会全绿。而这类漏接的症状是
// 「配了超时但服务仍按默认跑」或「配了环境变量不生效」，极难排查。
func TestHTTPTimeoutSources(t *testing.T) {
	t.Run("默认15s", func(t *testing.T) {
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != DefaultHTTPTimeout {
			t.Errorf("默认 HTTPTimeout = %s，want %s", c.HTTPTimeout, DefaultHTTPTimeout)
		}
	})

	t.Run("flag覆盖", func(t *testing.T) {
		c, err := Load([]string{
			"--pg-url=postgres://u:p@localhost:5432/iip",
			"--http-timeout=42s",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != 42*time.Second {
			t.Errorf("flag 覆盖后 HTTPTimeout = %s，want 42s", c.HTTPTimeout)
		}
	})

	t.Run("环境变量覆盖", func(t *testing.T) {
		t.Setenv("IIP_HTTP_TIMEOUT", "7s")
		c, err := Load([]string{"--pg-url=postgres://u:p@localhost:5432/iip"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.HTTPTimeout != 7*time.Second {
			t.Errorf("环境变量覆盖后 HTTPTimeout = %s，want 7s", c.HTTPTimeout)
		}
	})
}
