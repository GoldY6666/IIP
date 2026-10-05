// Playwright 配置 —— M5 F6 E2E。
//
// **为什么打真后端而不是 page.route 全 mock**
// 判据 8 与 E2E-4 的核心是「回读确认落库」：前端说「保存成功」不等于数据真落库。
// 记忆里已登记两起同源自证陷阱（结构体 json tag 漏写 handler 层测不出、
// 流转失败后按钮被锁 3 秒只验「最终恢复」会假通过）。若用 route 拦截造响应，
// 这 5 条用例就只剩「前端渲染对不对」，验不到任何落库行为。
//
// **直接打 iip-server 的 8087，不另起反代**
// `--web-root` 已让 iip-server 自己做静态托管与 SPA 回退（F2 的能力），
// 实测 `GET /` 200 且含 __IIP_UI__ 注入。baseURL 指向它即是**部署形态本身**，
// 比「静态服务器 + 反代」更接近用户实际打开的地址，且少一个进程、少一个失败点。
//
// **浏览器用系统 Chrome（channel: 'chrome'）**
// 手册 §4.4.4 明确：`npx playwright install chromium` 需联网，本机用已有 Chrome。
//
// **端口不复用既有链路**
// 5434/8087 是 E2E 专用。既有四条链路各用各的（verify-func 占 5434、
// 容器链路映射 5432），E2E 单独占一套 —— 但两者不能同时跑（都占 5434）。
import { defineConfig } from '@playwright/test'

const PG_PORT = 5434
const APP_PORT = 8087
const BASE = `http://127.0.0.1:${APP_PORT}`

export default defineConfig({
  testDir: './e2e',
  // 五条用例共享同一批造出来的数据（globalSetup 造一次），
  // 且 E2E-3 会真实改一条事件的状态。fullyParallel 会让
  // 「E2E-3 依赖事件处于 open」与其他用例抢同一条数据。
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  // 每条用例自带断言。重跑只会掩盖真实原因 —— 记忆里
  // 「连跑 3~4 次全绿」是对**门禁**的要求，不是对单条 flaky 用例的宽容。
  reporter: [['list']],
  timeout: 30_000,
  expect: { timeout: 10_000 },

  use: {
    baseURL: BASE,
    // **channel 必须可覆盖**，两个环境拿到的东西不一样：
    //   本机：无网络，`playwright install` 下不了浏览器 → 用系统 Chrome
    //         （手册 §4.4.4 就是这么定的）
    //   CI：ubuntu runner 没有系统 Chrome → 必须用 Playwright 自带的 chromium
    // 写成硬编码 'chrome' 会让 CI 那个 job 必然红，而「CI 里加了个
    // 跑不通的 e2e job」比不加更糟：它会让人以为 E2E 被 CI 覆盖了。
    // CI 侧设 PLAYWRIGHT_CHANNEL='' 即表示「用自带的」。
    ...(process.env.PLAYWRIGHT_CHANNEL !== undefined
      ? { channel: process.env.PLAYWRIGHT_CHANNEL || undefined }
      : { channel: 'chrome' }),
    headless: true,
    // 断言中文按钮时不能靠精确名（antd 会插空格），
    // 但失败时要能看清现场。
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],

  // 路径用**相对字符串**而不是 require.resolve()：
  // package.json 里有 "type": "module"，整个 web/ 是 ESM，
  // require 在这里不存在（实测报 ReferenceError: require is not defined）。
  globalSetup: './e2e/global-setup.ts',

  webServer: [
    {
      // 真 PG + 真 iip-server（带 --web-root）。stack.mjs 负责
      // 「起 PG → 建库 → 起服务 → 等就绪 → 回收 PG」全在一个进程内闭环：
      // 跨调用启动的 postgres 会被沙箱回收（记忆里「真库门禁前台跑必被
      // SIGTERM」同源），所以必须同一次调用。
      command: `node e2e/stack.mjs up ${PG_PORT} ${APP_PORT}`,
      url: `${BASE}/healthz`,
      reuseExistingServer: false,
      timeout: 180_000,
      stdout: 'pipe',
      stderr: 'pipe',
    },
  ],
})
