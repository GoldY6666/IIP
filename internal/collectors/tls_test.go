// 本文件是 T7.5 采集目标 TLS 的判据。
//
// 三层用例，缺一层都会漏掉一类失效：
//  1. BuildTLSConfig 纯函数：配置组合的语义（nil / insecure 优先 / 读失败报错）；
//  2. 端到端三态：同一自签 TLS server，「无配置必失败、给 CA 必成功、
//     insecure 必成功」—— 这一层才是杀手锏：若 newHTTPClient 把
//     tlsConfig 吞掉（改了选项却不生效），第 1 层全绿而这里全红；
//  3. （无配置路径不回归）由包内既有全部用例覆盖：tlsConfig 为 nil 时
//     newHTTPClient 与旧构造逐字节一致，不套 Transport。
package collectors

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonEcho 是共用的 TLS 测试服务：返回可解析的 JSON。
func jsonEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

// serverCertPEM 把 httptest TLS server 的自签证书提成 PEM 文件，
// 模拟「用户从服务器导出 CA 证书配到 IIP_EVIDENCE_TLS_CA_FILE」。
func serverCertPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	der := srv.Certificate().Raw
	if len(der) == 0 {
		t.Fatal("httptest server 没有证书，无法构造 CA 文件")
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("写 CA 文件失败: %v", err)
	}
	return p
}

// --- 第 1 层：BuildTLSConfig 配置语义 ---

func TestBuildTLSConfig_NoConfigReturnsNil(t *testing.T) {
	// 无配置必须返回 (nil, nil)：调用方拿 nil 去过 WithTLSConfig，
	// 等于系统默认。返回非 nil 空配置虽也能跑，但会让「有没有配 TLS」
	// 在日志里说不清，也掩盖调用方的分支错误。
	cfg, err := BuildTLSConfig(false, "")
	if err != nil {
		t.Fatalf("无配置不应报错: %v", err)
	}
	if cfg != nil {
		t.Fatalf("无配置应返回 nil，实得 %+v", cfg)
	}
}

func TestBuildTLSConfig_Insecure(t *testing.T) {
	cfg, err := BuildTLSConfig(true, "")
	if err != nil {
		t.Fatalf("insecure 不应报错: %v", err)
	}
	if cfg == nil {
		t.Fatal("insecure 应返回非 nil 配置")
	}
	if !cfg.InsecureSkipVerify {
		t.Error("insecure=true 应置 InsecureSkipVerify")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion 应钉 1.2，实得 %#x", cfg.MinVersion)
	}
}

func TestBuildTLSConfig_InsecureBeatsCAFile(t *testing.T) {
	// insecure + 一个**不存在**的 CA 路径：insecure 分支先返回，
	// 根本不该去读文件。若实现改成「先读 CA 再判 insecure」，
	// 这个用例会红 —— 防止把「连不上时先连上」的意图变成
	// 「连不上时就报错」，那 insecure 开关在 CA 文件写错时直接失效。
	cfg, err := BuildTLSConfig(true, filepath.Join(t.TempDir(), "no-such.pem"))
	if err != nil {
		t.Fatalf("insecure 优先时不应去读 CA 文件: %v", err)
	}
	if cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatalf("insecure 应先生效，实得 %+v", cfg)
	}
}

func TestBuildTLSConfig_CAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	cfg, err := BuildTLSConfig(false, missing)
	if err == nil {
		t.Fatal("CA 文件不存在必须报错，不能静默返回 nil 配置")
	}
	if cfg != nil {
		t.Error("报错时配置必须为 nil")
	}
	// 错误信息必须带路径：运维要知道去修哪个文件，
	// 只说「读取失败」等于让他去猜。
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("错误信息应含文件路径 %s，实得: %v", missing, err)
	}
}

func TestBuildTLSConfig_CANotPEM(t *testing.T) {
	// 文件在、读到了，但不是证书（模拟给成私钥/网页/文本）。
	// 必须报错：静默放过会让采集以 x509 错误失败在第一次查询，
	// 而排查时完全看不出是「给错了文件」。
	dir := t.TempDir()
	p := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(p, []byte("<html>not a certificate</html>"), 0o600); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	_, err := BuildTLSConfig(false, p)
	if err == nil {
		t.Fatal("非 PEM 内容必须报错")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("错误信息应点明 PEM，实得: %v", err)
	}
}

// --- 第 2 层：端到端三态（自签 TLS server）---

// getJSON 是端到端用例的公共动作：用给定 client 打 TLS server 并解析 JSON。
func getJSON(t *testing.T, client *http.Client, url string) error {
	t.Helper()
	var out struct {
		OK bool `json:"ok"`
	}
	return httpGetJSON(context.Background(), client, url, &out)
}

func TestHTTPClientTLS_NoConfigFailsOnSelfSigned(t *testing.T) {
	srv := httptest.NewTLSServer(jsonEcho())
	defer srv.Close()

	// 无 TLS 配置 = 系统根证书校验，自签证书必须被拒。
	// 这一条同时是「回归防线」：若哪天默认行为被改成跳过校验，
	// （比如有人给 newHTTPClient 加了默认 InsecureSkipVerify），这里红。
	c := newHTTPClient(resolveOptions(nil))
	err := getJSON(t, c, srv.URL)
	if err == nil {
		t.Fatal("无 TLS 配置时自签证书必须失败（系统根证书不认它）")
	}
	if !strings.Contains(err.Error(), "x509") &&
		!strings.Contains(err.Error(), "certificate") {
		t.Errorf("期望证书校验错误，实得: %v", err)
	}
}

func TestHTTPClientTLS_WithCASucceeds(t *testing.T) {
	srv := httptest.NewTLSServer(jsonEcho())
	defer srv.Close()

	caFile := serverCertPEM(t, srv)
	cfg, err := BuildTLSConfig(false, caFile)
	if err != nil {
		t.Fatalf("合法 CA 文件不应报错: %v", err)
	}
	if cfg == nil {
		t.Fatal("有 CA 文件必须返回非 nil 配置")
	}

	c := newHTTPClient(resolveOptions([]Option{WithTLSConfig(cfg)}))
	if err := getJSON(t, c, srv.URL); err != nil {
		t.Fatalf("配了正确 CA 必须打通，实得: %v", err)
	}
}

func TestHTTPClientTLS_WithInsecureSucceeds(t *testing.T) {
	srv := httptest.NewTLSServer(jsonEcho())
	defer srv.Close()

	cfg, err := BuildTLSConfig(true, "")
	if err != nil {
		t.Fatalf("insecure 不应报错: %v", err)
	}

	c := newHTTPClient(resolveOptions([]Option{WithTLSConfig(cfg)}))
	if err := getJSON(t, c, srv.URL); err != nil {
		t.Fatalf("insecure 模式必须打通，实得: %v", err)
	}
}

func TestHTTPClientTLS_NoConfigPathUnchanged(t *testing.T) {
	// 零配置路径的行为锁定：不套 Transport、Timeout 仍生效。
	// 防的是「收敛 newHTTPClient 时顺手改了默认行为」这类无意识回归 ——
	// 既有几十个用例用的都是无配置 client，这里补一个显式断言。
	c := newHTTPClient(resolveOptions([]Option{WithTimeout(3 * time.Second)}))
	if c.Transport != nil {
		t.Error("无 TLS 配置时不应设置自定义 Transport（应走 DefaultTransport）")
	}
	if c.Timeout != 3*time.Second {
		t.Errorf("Timeout 应透传，实得 %s", c.Timeout)
	}
}

// --- 第 3 层：CA 文件内容真的被用上（而非只做了「文件存在」检查）---

// otherSelfSignedPEM 本地生成一张**与 httptest 内置证书无关**的自签证书，
// 当「内容合法但给错了对象」的 CA。
//
// 为什么不能拿第二个 httptest.NewTLSServer 的证书充数：httptest 复用
// 包级同一张内置证书（Go 源码 localhostCert），两个 server 的证书
// 逐字节相同 ——「另一把 CA」其实是同一把，用例会恒绿。
// （本文件第一版就踩了这个坑：WrongCARejected 曾因此假绿。）
func otherSelfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "iip-tls-test-wrong-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
		// BasicConstraintsValid 必须显式置 true，否则 IsCA 不生效 ——
		// x509.CreateCertificate 要求模板里两个字段配对出现。
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("自签测试证书失败: %v", err)
	}
	p := filepath.Join(t.TempDir(), "other-ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("写测试 CA 文件失败: %v", err)
	}
	return p
}

func TestHTTPClientTLS_WrongCARejected(t *testing.T) {
	srv := httptest.NewTLSServer(jsonEcho())
	defer srv.Close()

	// 给一张**别的**自签证书当 CA：内容合法（构造期不报错），
	// 但与 server 的不是同一把（握手必须被拒）。
	// 这一条杀死「只读文件不解析、RootCAs 没接上」这类
	// 「看起来配了 CA 实际谁都不验」的变异。
	wrongCA := otherSelfSignedPEM(t)
	cfg, err := BuildTLSConfig(false, wrongCA)
	if err != nil {
		t.Fatalf("合法但错误的 CA 文件不应在构造期报错（内容合法、只是对不上）: %v", err)
	}

	c := newHTTPClient(resolveOptions([]Option{WithTLSConfig(cfg)}))
	if err := getJSON(t, c, srv.URL); err == nil {
		t.Fatal("给错 CA 必须被拒（说明 RootCAs 真的生效了）")
	}
}
