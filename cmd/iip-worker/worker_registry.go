package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yu/iip/internal/collectors"
	"github.com/yu/iip/internal/config"
	"github.com/yu/iip/internal/evidence"
)

// buildRegistry 按配置装配采集器注册表。
//
// **采集器缺席时打 INFO 而不是让 worker 起不来**（手册 §4.6 的修订）：
// 默认部署没有 Prometheus / Loki 地址，若因此拒绝启动，
// 就等于「为了让默认值好看而让默认部署不可用」。
// 采集器缺席时日志明确说明，运维一眼能看出「为什么没有证据」。
func buildRegistry(cfg *config.Config, logger *slog.Logger) (*evidence.Registry, error) {
	reg := evidence.NewRegistry()

	// T7.5：采集目标 TLS 配置。
	//
	// 与「采集器缺席打 INFO」是有意的不对称：缺席是默认部署的常态，
	// 而 TLS 配置错误是**显式配置写错**。放过它只剩两条路 ——
	// 不注册 TLS（配了白配）或静默用无校验客户端（信任模型降级），
	// 都比「worker 起不来、启动日志说清哪个文件哪一行写错」更糟。
	tlsCfg, err := collectors.BuildTLSConfig(cfg.EvidenceTLSInsecure, cfg.EvidenceTLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("采集目标 TLS 配置不可用: %w", err)
	}
	var tlsOpts []collectors.Option
	if tlsCfg != nil {
		tlsOpts = append(tlsOpts, collectors.WithTLSConfig(tlsCfg))
		if cfg.EvidenceTLSInsecure {
			// 必须留痕：insecure 把中间人攻击的面重新打开，
			// 事后排查「证书明明不对、采集却没报错」时这是唯一线索。
			logger.Warn("已跳过采集目标 TLS 证书校验（IIP_EVIDENCE_TLS_INSECURE=true），"+
				"仅限内网自签证书的过渡场景；生产环境应改用 IIP_EVIDENCE_TLS_CA_FILE 提供 CA",
				slog.String("env", "IIP_EVIDENCE_TLS_INSECURE"))
		} else {
			logger.Info("采集目标将使用自定义 CA 校验服务器证书",
				slog.String("ca_file", cfg.EvidenceTLSCAFile))
		}
	}
	// withTLS 把全局 TLS 选项附加到各采集器自己的选项后面。
	withTLS := func(extra ...collectors.Option) []collectors.Option {
		return append(append([]collectors.Option{}, extra...), tlsOpts...)
	}

	// Prometheus：开关默认 true，但没配地址就不注册（见函数头说明）。
	if cfg.EvidencePromEnabled {
		if strings.TrimSpace(cfg.PromURL) == "" {
			logger.Info("prometheus 采集器未配置地址，已跳过注册",
				slog.String("env", "IIP_PROM_URL"))
		} else {
			reg.MustRegister(collectors.NewPrometheusCollector(
				cfg.PromURL, withTLS(collectors.WithTimeout(cfg.EvidenceTimeout))...))
		}
	} else {
		logger.Info("prometheus 采集器已禁用", slog.String("env", "IIP_EVIDENCE_PROM_ENABLED"))
	}

	if cfg.EvidenceLokiEnabled {
		if strings.TrimSpace(cfg.LokiURL) == "" {
			logger.Info("loki 采集器未配置地址，已跳过注册",
				slog.String("env", "IIP_LOKI_URL"))
		} else {
			reg.MustRegister(collectors.NewLokiCollector(
				cfg.LokiURL, withTLS(collectors.WithTimeout(cfg.EvidenceTimeout))...))
		}
	} else {
		logger.Info("loki 采集器已禁用", slog.String("env", "IIP_EVIDENCE_LOKI_ENABLED"))
	}

	// K8s 采集器：M4 只做客户端 + mock 单测，**默认关闭**
	// （手册偏差 B1：仓库零 K8s 依赖，写一个接不上真实集群的采集器
	// 比不写更危险 —— 它看起来在工作，但永远采不到东西）。
	if !cfg.EvidenceK8sEnabled {
		logger.Info("k8s 采集器已禁用（M4 不接真实集群，见手册偏差 B1）",
			slog.String("env", "IIP_EVIDENCE_K8S_ENABLED"))
	} else {
		// 走到这里说明 config.Validate 已放行，URL 与 namespace 必非空。
		// 这里**必须真的注册**：开关是用户接集群的意图，
		// 若只打日志不注册，就成了死配置 —— 开关已开、地址已填、
		// 日志显示「已启用」，但采到的证据恒为 0 条且全程无报错。
		// 与 Prom/Loki 的「未配地址则跳过」是有意的不对称：
		// 那两个默认就是 true（不配属常态），K8s 默认 false（开了必是有意）。
		reg.MustRegister(collectors.NewK8sCollector(
			cfg.K8sURL, cfg.K8sNamespace,
			withTLS(collectors.WithTimeout(cfg.EvidenceTimeout),
				collectors.WithTopN(cfg.EvidenceMaxPerIncident))...))
		logger.Info("k8s 采集器已注册（接的是用户自带集群，本仓库不提供）",
			slog.String("namespace", cfg.K8sNamespace))
	}

	// trace 采集器：**默认关闭**（M6 §5 降级裁定）。
	//
	// 与 K8s 同为「默认关」，但**理由不同，别照抄注释**：
	// K8s 是本仓库零依赖（没有 kubeconfig、没有 RBAC 清单）；
	// trace 是协议通用（只依赖 Jaeger 兼容的 /api/traces 形状），
	// 本机只是**没有任何真实后端可验**。所以默认关是为了不声称已接入，
	// 而不是「接不上」。
	//
	// 同样**必须真的注册**：开了开关却只打日志，会得到一个
	// 「日志说已启用、trace 证据恒为 0 条、全程无报错」的死配置。
	if !cfg.EvidenceTraceEnabled {
		logger.Info("trace 采集器已禁用（无真实链路后端可验，见 M6 手册 §5）",
			slog.String("env", "IIP_EVIDENCE_TRACE_ENABLED"))
	} else {
		// config.Validate 已放行，TraceURL 必非空。
		reg.MustRegister(collectors.NewTraceCollector(
			cfg.TraceURL,
			withTLS(collectors.WithTimeout(cfg.EvidenceTimeout),
				collectors.WithTopN(cfg.EvidenceMaxPerIncident))...))
		logger.Info("trace 采集器已注册（后端由部署侧提供，本仓库不附带）",
			slog.String("url", cfg.TraceURL))
	}

	// 正向日志：只有「至少注册了一个」才打。
	// 「一个都没注册」要单独说，否则「采不到证据」与「采到了零条」在日志上无法区分。
	if reg.Len() == 0 {
		logger.Warn("没有任何采集器注册，采集任务将只能推进游标而采不到证据",
			slog.Bool("prom_enabled", cfg.EvidencePromEnabled),
			slog.Bool("loki_enabled", cfg.EvidenceLokiEnabled),
			slog.Bool("k8s_enabled", cfg.EvidenceK8sEnabled),
			slog.Bool("trace_enabled", cfg.EvidenceTraceEnabled))
	} else {
		logger.Info("证据采集器已注册", slog.Any("sources", reg.Sources()))
	}
	return reg, nil
}

// instanceID 生成本 worker 实例标识，落在 tasks.claimed_by 上。
//
// hostname-pid 而不是随机 UUID：出问题时能从库里看出
// 「这批任务被哪台机器领走过」，而随机 ID 只能告诉你「某个 worker 领走过」。
func instanceID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		// 拿不到主机名不该让 worker 起不来，退化成 pid 仍然可区分多实例。
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
