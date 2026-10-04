package config

import (
	"strings"

	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

// M5 的配置项单独成文件（手册 §4.3）。
//
// 为什么不都塞在 config.go：那个文件已 340+ 行（超 AGENTS.md 的 300 行约束，
// 且该超限自 M2 起就存在）。每加一项都往上堆，只会让「该拆了」这件事
// 一直没人认领。**新功能不带旧债进来** 是更省事的做法。
//
// 本文件只放 M5 相关的「声明 + 注册 + 读取」，不参与校验 ——
// 校验也无处可加：WebRoot 为空是完全合法的配置（不启用静态托管），
// 非空时目录是否存在也**不**在启动期校验（容器里 dist 可能是后挂的，
// 启动时还没有；存在性错误交给请求期报「前端产物不完整，请先 pnpm run build」）。

// DefaultWebRoot 默认空 = **不启用**前端静态托管（M5 手册 §4.3）。
//
// 不用 "./web/dist" 作默认值：那样会让 go test / 既有验证链路
// 隐式依赖前端是否构建过，而「后端能跑」不该要求「先构建前端」。
// 部署形态由 compose 显式传 IIP_WEB_ROOT 打开。
const DefaultWebRoot = ""

// --- M5 字段 ---
//
// 放在 Config 结构体外的独立文件里不可能（字段必须在结构体内），
// 但**声明块在 config.go、其余都在本文件** —— 这是折中：
// config.go 仍有 8 行 M5 字段，完整实现不堆在它里面。

// registerM5Flags 注册 M5 的命令行参数。默认空 = 不启用静态托管。
func registerM5Flags(fs *pflag.FlagSet) {
	fs.String("web-root", DefaultWebRoot,
		"前端产物目录（vite build 输出），非空则启用静态托管与 SPA 回退")
}

// readM5 从 koanf 读取 M5 配置并写回 cfg。
//
// TrimSpace：环境变量 `IIP_WEB_ROOT=" "`（等号后多个空格是 YAML 与 shell
// 都很常见的写法）不该被当成有效路径。留一个空格路径下去，
// 症状是「服务起了但 UI 404」，而配置看起来是「设了的」。
func readM5(k *koanf.Koanf, cfg *Config) {
	cfg.WebRoot = strings.TrimSpace(k.String("web-root"))
}
