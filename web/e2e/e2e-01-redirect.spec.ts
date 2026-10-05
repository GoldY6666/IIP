// E2E-1  打开 / → 重定向到 /incidents
//
// 判据取自 App.tsx 的 `<Route index element={<Navigate to="/incidents" replace />} />`。
//
// **为什么验 URL 而不是验页面内容**：重定向这个行为的后果就是 URL 变了。
// 断言「页面上有事件表格」会在重定向失效时**照样通过**（列表页与重定向目标
// 长得不一样，但都渲染了表头）—— 那是判据自证失效。
import { test, expect } from '@playwright/test'

test('E2E-1 打开根路径重定向到 /incidents', async ({ page }) => {
  await page.goto('/')

  // replace 是「不留下历史记录」，所以不能靠 goBack 验 —— 直接断 URL。
  await page.waitForURL('**/incidents', { timeout: 10_000 })
  expect(page.url()).toContain('/incidents')

  // 顺带确认落地页真渲染了（不是白屏），且侧栏在。
  // 判据用「事件列表」这个表头 —— 它在 Incidents.tsx 的列定义里。
  await expect(page.getByRole('table')).toBeVisible()
})
