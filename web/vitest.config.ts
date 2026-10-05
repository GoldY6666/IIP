// vitest 配置 —— 与 vite.config.ts 分开，不混在一个文件里。
//
// **为什么不用 vite.config.ts 里的 test 字段**：
// vitest 的 `test` 配置类型来自 `vitest/config`，写进 vite.config.ts 需要
// 改 import 源头（`defineConfig` 换成 vitest 的），那会让 vite 本身的构建
// 也依赖 vitest 类型 —— 一旦 vitest 装不上，`vite build` 也编译不过。
// 分文件后两者互不影响，且 vite.config.ts 不用动（M3 定的代理配置不受影响）。
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: {
    // jsdom：组件测试要 DOM。不用 happy-dom 是因为 antd 的
    // 部分交互（Select/Dropdown/Slider）依赖真实的布局测量，happy-dom 支持不全，
    // 会出现「测试环境缺功能」而非「代码有 bug」的假失败。
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    // e2e/ 是 Playwright 的用例，跑在真浏览器 + 真后端上，
    // **绝不能被 vitest 收集**：那会开 jsdom 去跑
    // 「真起 PG、真启服务」的测试，报错完全指向 vitest 而非真因。
    // include 已限定 src/**，这里显式 exclude 是为了防「改 include 那天」。
    exclude: ['e2e/**', 'node_modules/**', 'dist/**'],
    // 覆盖率口径：只统计被测的 src，不统计测试文件自身与类型声明。
    coverage: {
      provider: 'v8',
      include: ['src/**/*.{ts,tsx}'],
      exclude: ['src/**/*.test.{ts,tsx}', 'src/test/**', 'src/vite-env.d.ts'],
    },
  },
})
