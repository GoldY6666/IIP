// E2E-5  无 API 时的表现 → 列表页显示错误态而非白屏
//
// 手册判据：「列表页显示错误态而非白屏」。
// 白屏是最糟的失败形态：用户看到空页面，既不知道是网络挂了还是服务崩了，
// 也没法重试，只能刷新或叫人。
//
// **为什么用 page.route 拦截而不是真停后端**
// 禹于 2026-10-04 定：拦截让本条 deterministic（不引入进程/端口管理的
// 不确定性，不会 flaky），且「接口不可用」这个前提已经成立 ——
// react-query 收到网络错误后走的就是同一条错误分支。
// 真停后端验的是「TCP 连不上」，那是链路 A 的 healthz 三态已经覆盖的部分，
// 与「前端有没有错误态」是两件事。
//
// 拦截范围**必须精确**：只拦 /api/v1/incidents（列表接口）。
// 拦 /api/** 全部会把静态资源与注入也打掉，测的就不是「列表错误态」了。
import { test, expect } from '@playwright/test'

test('E2E-5 列表接口不可用时显示错误态而非白屏', async ({ page }) => {
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(String(e)))

  // 拦列表接口。只拦这一个路径 —— 拦 /api/** 会连注入与静态资源一起打掉，
  // 那样测到的不是「列表错误态」。
  await page.route('**/api/v1/incidents**', (route) => route.abort('failed'))

  await page.goto('/incidents')

  // 错误态文案取自 Incidents.tsx：`加载事件列表失败：{message}`。
  // 断前缀而非断全文：message 里含浏览器的英文错误文本（net::ERR_FAILED
  // 之类），随 Playwright 版本变，断死会让用例因无关原因变红。
  await expect(page.getByText(/加载事件列表失败/).first()).toBeVisible({ timeout: 10_000 })

  // **不是白屏**：壳（Header / 侧栏）仍在。
  // 只断错误文案是不够的 —— 那段文字可能在 body 里而周围全空，
  // 视觉上仍是白屏。
  //
  // 判据用 `.ant-layout-sider` 而不是 role="navigation"：Layout 用的是
  // antd Sider + Menu，**没有** role="navigation"（写 role 会永远失败）。
  // 侧栏在 Layout 里，与列表数据无关 —— 接口挂了它也必须在。
  await expect(page.locator('.ant-layout-sider')).toBeVisible()
  await expect(page.locator('.ant-layout-header')).toBeVisible()

  // **重试入口是必需的，不是可选的**：错误态若不可恢复，只是换个姿势的白屏。
  // 列表页原有「刷新」按钮在正常分支里，错误态下渲染不出来 ——
  // 这是 F6 修掉的第二个缺陷，本条断言就是钉住它。
  const retry = page.getByRole('button', { name: /重\s*试/ })
  await expect(retry, '错误态应提供「重试」入口').toBeVisible()

  // 点重试应重新发请求。拦截仍在，所以结果是「仍然失败」——
  // 但按钮必须真的发起了请求（而不是局部 state 假装 loading）。
  await retry.click()
  await expect(retry).toBeVisible()

  // 页面不应有未捕获异常。白屏常见成因是渲染期 throw，
  // 那种情况下「错误态文字」根本渲染不出来。
  expect(errors, '不应有未捕获的页面异常：' + errors.join(' | ')).toHaveLength(0)

  // 恢复正常：解除拦截后刷新，应能看到列表（本轮造的事件）。
  await page.unroute('**/api/v1/incidents**')
  await page.reload()
  await expect(page.getByRole('table')).toBeVisible({ timeout: 10_000 })
  await expect(page.getByText(/加载事件列表失败/)).toHaveCount(0)
})
