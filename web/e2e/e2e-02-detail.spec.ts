// E2E-2  列表有事件 → 点进去看到详情三要素（时间线 / 证据 / 关联告警）
//
// 三个 Tab 的标签取自 IncidentDetail.tsx 的 items 定义：
//   `关联告警${inc.alert_count}` / `时间线${data.timeline.length}` / `证据${total}`
// **带括号数字**，所以用正则而非精确文本 —— 数字会随数据变。
//
// 「点进去」的实现是 Table 的 onRow 整行可点（Incidents.tsx 注释：
// 「点击跳转由 onRow 统一负责」），所以点标题单元格，不要找链接。
import { test, expect } from '@playwright/test'
import { findSeededIncident } from './helpers'

test('E2E-2 列表页有造出的事件且能进详情看到三要素', async ({ page }) => {
  // 自己问后端要那条事件。找不到时**直接红**而不是 skip ——
  // skip 会让判据 10 的「5 条」悄悄变成 4 条而控制台照样全绿。
  const seeded = await findSeededIncident()
  const SERVICE = seeded.service
  const INCIDENT_ID = seeded.id

  await page.goto('/incidents')

  // 列表里能看到我们造的那条 —— 精确定位到「服务」那一格所在的行。
  //
  // **必须用 getByRole('cell', { name: SERVICE, exact: true }) 反推行**：
  // ① `filter({ hasText: SERVICE })` 是**子串**匹配，而 B 组事件的 service
  //    是 `e2e-svc-<run>-claim` —— 它包含 A 组的 service 字符串，
  //    于是两行都命中（实测 Received: 2）。
  // ② 用 locator('tr') 也不对：antd Table 额外渲染一个测宽用的隐藏 <tr>。
  // row.getByRole('cell', exact) 两个问题都绕开：cell 是语义元素、
  // 隐藏行不参与 role 查询、exact 让 `-claim` 那条不匹配。
  const cell = page.getByRole('cell', { name: SERVICE, exact: true })
  await expect(cell, '列表应有一行的「服务」列等于该 service').toHaveCount(1)
  const row = cell.locator('xpath=ancestor::tr[1]')

  // 状态标签应是「新建」（status.ts 的 STATUS_META.open.label）。
  // 这一条同时钉住「列表读的真是后端状态」——若后端返回英文 open，这里会红。
  // **必须带 -claim 的排除**（用 cell 锚定后天然满足）。
  await expect(row.getByText('新建')).toBeVisible()

  // 点击进详情。onRow 挂在整行上（Incidents.tsx：「点击跳转由 onRow 统一负责」），
  // 点「服务」那一格即可。
  //
  // **点上面已定位好的 cell，不要再 `row.getByRole('cell', {name: SERVICE})`**：
  // 「标题」列的 Tooltip 文本里也含 service 名（形如
  // `E2EFiring (e2e-svc-xxx)`），非精确匹配会命中 2 个 cell，
  // Playwright 报 strict mode violation 拒绝点击。
  await cell.click()
  await page.waitForURL(`**/incidents/${INCIDENT_ID}`, { timeout: 10_000 })

  // 三个 Tab 都在，且带括号计数。
  for (const label of ['关联告警', '时间线', '证据']) {
    await expect(page.getByRole('tab', { name: new RegExp(`^${label}`) })).toBeVisible()
  }

  // 关联告警 Tab 默认激活，内容应是 4 条（globalSetup 造了 4 条同服务告警）。
  // 验「>0」而不是「=4」：前者对关联算法的容差合理，后者会把实现细节钉死。
  const alertsTab = page.getByRole('tab', { name: /^关联告警/ })
  const alertsCount = await alertsTab.innerText()
  const n = Number(alertsCount.replace(/[^\d]/g, ''))
  expect(n, `关联告警数应 >0，实际「${alertsCount}」`).toBeGreaterThan(0)

  // 时间线：至少一条 alert_fired（造数时写入的）。
  // 这是「事件真由告警关联生成」的证据，不是页面自己编的。
  await page.getByRole('tab', { name: /^时间线/ }).click()
  await expect(page.getByText('告警触发').first()).toBeVisible()

  // 证据 Tab：本期采集器全关（E2E 不验证据采集，那是 M4 链路 C 的职责），
  // 所以这里断言的是**空态说清了原因**，而不是「有证据」。
  //
  // 判据取自 EvidenceList.tsx 的空态原文「暂无证据」与它的成因说明。
  // **不能断 /证据/ 这样的宽泛文本** —— Tab 标签本身就叫「证据 (0)」，
  // 断它等于什么都没验（恒真）。记忆里登记过「判据写成会随实现漂移的宽泛
  // 匹配 = 自证失效」。
  await page.getByRole('tab', { name: /^证据/ }).click()
  await expect(page.getByText('暂无证据')).toBeVisible()
  // 空态必须说清「可能是采集器没注册/不可达」——用户会去查自己的集群，
  // 而真相往往是 worker 侧没采到。不说清就会白跑一趟。
  await expect(page.getByText(/采集器未注册|数据源不可达/).first()).toBeVisible()
})
