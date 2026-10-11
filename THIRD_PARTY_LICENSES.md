# 第三方依赖许可证清单

> 生成方式：**从 `go mod` 声明与 modcache 实际 LICENSE 文件读取**，非人工填写。
> 核对命令：`python scripts/gen_third_party_licenses.py`
> 许可证识别依据各LICENSE 文件首600 字符的特征串，识别不到标为「未识别」**（不猜）**。

## 一、Go 依赖（25 个）

| 类型 | 模块 | 版本 | 许可证 | LICENSE 文件 |
|---|---|---|---|---|
| 直接依赖 | `github.com/go-chi/chi/v5` | `v5.3.0` | MIT | `LICENSE` |
| 直接依赖 | `github.com/golang-migrate/migrate/v4` | `v4.18.1` | MIT | `LICENSE` |
| 直接依赖 | `github.com/jackc/pgx/v5` | `v5.9.2` | MIT | `LICENSE` |
| 直接依赖 | `github.com/knadh/koanf/parsers/yaml` | `v0.1.0` | MIT | `LICENSE` |
| 直接依赖 | `github.com/knadh/koanf/providers/env` | `v1.0.0` | MIT | `LICENSE` |
| 直接依赖 | `github.com/knadh/koanf/providers/file` | `v1.1.2` | MIT | `LICENSE` |
| 直接依赖 | `github.com/knadh/koanf/providers/posflag` | `v0.1.0` | MIT | `LICENSE` |
| 直接依赖 | `github.com/knadh/koanf/v2` | `v2.1.2` | MIT | `LICENSE` |
| 直接依赖 | `github.com/lib/pq` | `v1.10.9` | MIT | `LICENSE.md` |
| 直接依赖 | `github.com/spf13/pflag` | `v1.0.5` | BSD-3-Clause | `LICENSE` |
| 直接依赖 | `gopkg.in/yaml.v3` | `v3.0.1` | Apache-2.0 | `LICENSE` |
| 间接依赖 | `github.com/fsnotify/fsnotify` | `v1.7.0` | BSD-3-Clause | `LICENSE` |
| 间接依赖 | `github.com/go-viper/mapstructure/v2` | `v2.3.0` | MIT | `LICENSE` |
| 间接依赖 | `github.com/hashicorp/errwrap` | `v1.1.0` | MPL-2.0 | `LICENSE` |
| 间接依赖 | `github.com/hashicorp/go-multierror` | `v1.1.1` | MPL-2.0 | `LICENSE` |
| 间接依赖 | `github.com/jackc/pgpassfile` | `v1.0.0` | MIT | `LICENSE` |
| 间接依赖 | `github.com/jackc/pgservicefile` | `v0.0.0-20240606120523-5a60cdf6a761` | MIT | `LICENSE` |
| 间接依赖 | `github.com/jackc/puddle/v2` | `v2.2.2` | MIT | `LICENSE` |
| 间接依赖 | `github.com/knadh/koanf/maps` | `v0.1.1` | MIT | `LICENSE` |
| 间接依赖 | `github.com/mitchellh/copystructure` | `v1.2.0` | MIT | `LICENSE` |
| 间接依赖 | `github.com/mitchellh/reflectwalk` | `v1.0.2` | MIT | `LICENSE` |
| 间接依赖 | `go.uber.org/atomic` | `v1.7.0` | MIT | `LICENSE.txt` |
| 间接依赖 | `golang.org/x/sync` | `v0.22.0` | BSD-3-Clause | `LICENSE` |
| 间接依赖 | `golang.org/x/sys` | `v0.25.0` | BSD-3-Clause | `LICENSE` |
| 间接依赖 | `golang.org/x/text` | `v0.41.0` | BSD-3-Clause | `LICENSE` |

**统计**：共 25 个 Go 依赖，其中「未识别」0 个。

## 二、前端 npm 依赖（19 个）

| 类型 | 包 | 版本 |
|---|---|---|
| 运行时 | `@ant-design/icons` | `5.6.1` |
| 运行时 | `@tanstack/react-query` | `5.59.16` |
| 运行时 | `antd` | `5.21.6` |
| 运行时 | `react` | `18.3.1` |
| 运行时 | `react-dom` | `18.3.1` |
| 运行时 | `react-router-dom` | `6.27.0` |
| 开发 | `@playwright/test` | `^1.63.0` |
| 开发 | `@testing-library/jest-dom` | `6.6.3` |
| 开发 | `@testing-library/react` | `16.1.0` |
| 开发 | `@testing-library/user-event` | `14.5.2` |
| 开发 | `@types/node` | `^26.6.4` |
| 开发 | `@types/react` | `18.3.12` |
| 开发 | `@types/react-dom` | `18.3.1` |
| 开发 | `@vitejs/plugin-react` | `4.3.3` |
| 开发 | `@vitest/coverage-v8` | `2.1.9` |
| 开发 | `jsdom` | `25.0.1` |
| 开发 | `typescript` | `5.6.3` |
| 开发 | `vite` | `5.4.10` |
| 开发 | `vitest` | `2.1.9` |

> npm 侧的许可证未逐包展开（`node_modules` 不入库，发布前用 `license-checker` 类工具核）。
> 主要依赖为 React / antd / Playwright / vitest，均为 MIT 系。

## 三、已知需要特别说明的一项

**`github.com/lib/pq`**：上游已归档不再维护，**但本项目无法移除** ——
`golang-migrate/migrate/v4` 的 postgres 驱动硬依赖它，
`internal/store/store.go` 的空白导入为 `database/sql` 注册 postgres 驱动。
许可证 MIT。移除需先替换 golang-migrate，属独立工作项。
