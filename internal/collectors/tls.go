// 本文件是采集目标的 TLS 配置构造（T7.5）。
//
// 为什么单独一个文件：四个采集器（prom / loki / k8s / trace）都要支持
// 「连 https 的内网数据源」，而「读 CA 文件」是会失败的动作。
// 失败必须在**启动期**变成 error —— 采集器构造器不返回 error，
// 也不能为读个文件就 panic，所以构造独立成 BuildTLSConfig，
// 由 worker 装配前调用并处理错误。
//
// 与 config.EvidenceTLSCAFile 的分工：config.Validate 用 os.Stat
// 在配置加载期拦「路径写错」；本函数在装配期真正读文件、解析 PEM，
// 拦「Stat 之后文件被删/权限变化」与「文件内容不是证书」。
// 两层都在启动期失败，全程无静默降级。
package collectors

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// BuildTLSConfig 把「是否跳过校验 + CA 文件路径」变成 *tls.Config。
//
// 返回 (nil, nil) 表示「无自定义 TLS 需求」：调用方拿 nil 传给
// WithTLSConfig，等价于系统默认（根证书校验、不跳过任何校验）。
//
// 返回 error 的两种情形：
//   - CA 文件给了却读不到（不存在 / 无权限 / 是目录）；
//   - 文件读到了但解析不出一条 PEM 证书。
//
// 刻意**不**在读失败时降级成「不校验」：那会把「配了 CA」的部署
// 悄悄变成「谁来都认」，而用户从日志里看不出任何异常 ——
// 这正是 T7.5 要堵的那条静默失效路径。
func BuildTLSConfig(insecure bool, caFile string) (*tls.Config, error) {
	path := strings.TrimSpace(caFile)
	if !insecure && path == "" {
		return nil, nil
	}

	cfg := &tls.Config{
		// MinVersion 钉 1.2：TLS 1.0/1.1 已被 RFC 8996 废弃。
		// 内网老设备若只支持 1.0，该升级设备，而不是在这里放开下限。
		MinVersion: tls.VersionTLS12,
	}
	if insecure {
		// insecure 优先于 CA：两个都配时，用户意图明确是「先连上」。
		// 调用方（worker 装配）负责在这种情况下打 WARN 日志。
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}

	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 文件失败：%s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		// 文件在、读到了，但一条证书都没吃进 —— 常见于给了私钥文件、
		// 给了 HTML 错误页导出物、或 PEM 头被编辑器改坏。
		// 若静默放过，采集会以 x509 错误失败在第一次查询，
		// 排查时完全看不出是「给错了文件」。
		return nil, fmt.Errorf("CA 文件 %s 不含任何可解析的 PEM 证书"+
			"（请确认给的是 .crt/.pem 证书文件，而非私钥或网页）", path)
	}
	cfg.RootCAs = pool
	return cfg, nil
}
