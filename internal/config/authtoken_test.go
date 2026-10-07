package config

import (
	"strings"
	"testing"
)

// TestAuthTokenFromEnv 是本批（M6 S4）最基础的一条：证明 IIP_AUTH_TOKEN
// 这个环境变量名**真的**被读进 cfg.AuthToken。
//
// 为什么要单独一条而不能靠「中间件能拦住请求」间接证明：
// 中间件拿到的是 cfg.AuthToken，若这个字段因key 映射或环境变量名写错而恒为空，
// 中间件会走「空 token = 不开鉴权」分支 —— **鉴权形同虚设而全部测试照绿**。
// 即「没接上」与「没启用」在下游表现完全一样，必须在配置层就区分开。
func TestAuthTokenFromEnv(t *testing.T) {
	t.Setenv("IIP_PG_URL", "postgres://u:p@localhost:5432/iip")
	t.Setenv("IIP_AUTH_TOKEN", "s3cr3t-from-env")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.AuthToken != "s3cr3t-from-env" {
		t.Errorf("AuthToken = %q，期望从 IIP_AUTH_TOKEN 读到 %q",
			cfg.AuthToken, "s3cr3t-from-env")
	}
}

// TestAuthTokenEmptyMeansNoAuth 锁定「空串是合法状态」这一设计。
//
// 这条是**反向断言**：若将来有人给 AuthToken 加 Validate 必填规则，
// 这里会红，从而暴露「默认关的开关被误改成默认开」。
func TestAuthTokenEmptyMeansNoAuth(t *testing.T) {
	t.Setenv("IIP_PG_URL", "postgres://u:p@localhost:5432/iip")
	t.Setenv("IIP_AUTH_TOKEN", "")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("未设 token 时应能正常加载（空串=不开鉴权），实得错误: %v", err)
	}
	if cfg.AuthToken != "" {
		t.Errorf("AuthToken = %q，期望为空串", cfg.AuthToken)
	}
	// 空 token 必须能过 Validate：它是「未启用鉴权」而非「漏配」。
	if err := cfg.Validate(); err != nil {
		t.Errorf("空 token 应通过 Validate（它表示不开鉴权，不是漏配），实得: %v", err)
	}
}

// TestAuthTokenFromFlag 证明 flag 通道也通。
//
// 环境变量与 flag 是两条独立的加载路径（见 Load 的注释：env 覆盖 yaml、
// flag 覆盖 env）。只验 env 时，flag 侧写错不会被发现 —— 而运维在
// compose 里更可能用 flag。
func TestAuthTokenFromFlag(t *testing.T) {
	t.Setenv("IIP_PG_URL", "postgres://u:p@localhost:5432/iip")
	// 显式清空，确保测的是 flag 而不是宿主环境里的 IIP_AUTH_TOKEN。
	t.Setenv("IIP_AUTH_TOKEN", "")

	cfg, err := Load([]string{"--auth-token=tok-via-flag"})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.AuthToken != "tok-via-flag" {
		t.Errorf("AuthToken = %q，期望 flag 值 %q", cfg.AuthToken, "tok-via-flag")
	}
}

// TestAuthTokenFlagOverridesEnv 锁定优先级：flag > env。
//
// 与上面两条分开写，因为「flag 能否覆盖 env」是独立的契约；
// 弄反了会让运维「命令行里明明写了 token 却仍鉴权失败」。
func TestAuthTokenFlagOverridesEnv(t *testing.T) {
	t.Setenv("IIP_PG_URL", "postgres://u:p@localhost:5432/iip")
	t.Setenv("IIP_AUTH_TOKEN", "tok-from-env")

	cfg, err := Load([]string{"--auth-token=tok-from-flag"})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.AuthToken != "tok-from-flag" {
		t.Errorf("AuthToken = %q，期望 flag 覆盖 env 得 %q",
			cfg.AuthToken, "tok-from-flag")
	}
}

// TestAuthTokenKeptOutOfLogs 是**回归防护**：token 是凭据，
// 绝不能出现在任何日志或错误信息里。
//
// 这条断言的意义在于「一旦有人为了调试把 token 塞进日志就会红」；
// 它不验当前行为（当前确实没打），而是给未来加日志的人设一道闸。
func TestAuthTokenKeptOutOfLogs(t *testing.T) {
	const secret = "tok-must-not-leak-9f3a"
	t.Setenv("IIP_PG_URL", "postgres://u:p@localhost:5432/iip")
	t.Setenv("IIP_AUTH_TOKEN", secret)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	// 校验失败时的错误信息是最容易泄露配置值的地方：
	// 造一个必然失败的配置，看错误里有没有 token。
	cfg.EvidenceK8sEnabled = true
	cfg.K8sURL = ""
	err = cfg.Validate()
	if err == nil {
		t.Fatal("构造的非法配置应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Validate 的错误信息泄露了 token: %s", err.Error())
	}
}
