// E2E-3  状态推进 → 状态变 investigating，时间线**新增一条**（回读验证）
//
// 判据 8 的核心。手册明确写：「**E2E-3 必须回读 timeline**，不能只断言
// 「按钮变色了」」—— 前端状态更新只证明渲染链路通，不证明数据落库。
// 记忆里这类同源自证已踩过两次（结构体 json tag 漏写 handler 层测不出；
// 流转失败后按钮被锁 3 秒只验「最终恢复」会假通过）。
//
// 因此本条有三层断言，缺一层就退化成「按钮点了没报错」：
//   1. 页面状态标签变「调查中」          —— 前端读到了新状态
//   2. **重新 GET /api/v1/incidents/{id}** —— 数据真落库，且 timeline 条数增加
//   3. 新增那条的 event_type 是 investigation_started 且 actor 非空
//
// 按钮文案：antd 会在中文 Button 里插空格（F4「保 存」、F5「转为已解 决」
// 都踩过），故用宽松正则，不赌它插在哪几个字之间。
import { test, expect } from '@playwright/test'
import { fetchIncident, findClaimIncident } from './helpers'

test('E2E-3 推进状态后回读 timeline 确认落库', async ({ page }) => {
  // **用自己那条**（service 以 -claim 结尾），不碰 E2E-2 的那条。
  // 共用一条会让 E2E-2 的「状态是新建」断言依赖执行顺序 ——
  // workers:1 下靠文件名字母序恰好成立，但那是巧合不是判据。
  const INCIDENT_ID = (await findClaimIncident()).id

  // 前置条件显式断言：事件必须处于 open，否则「点转为调查中」根本不会出现。
  // 不写这条，「按钮找不到」会被读成「E2E 失败」而实际是造数没到位。
  const before = await fetchIncident(INCIDENT_ID)
  expect(before.incident.status, '前置条件：事件应处于 open').toBe('open')
  const tlBefore = before.timeline.length

  await page.goto(`/incidents/${INCIDENT_ID}`)

  // open 状态下 allowedTargets 包含 investigating，按钮文案「转为调查中」。
  //
  // **不弹二次确认框**：confirmThen 只对 `to === 'open'`（终态重开）加确认，
  // 理由是「关错了得让人想清楚」。前向流转直接提交。E2E 若照想象加确认步骤，
  // 会因找不到弹窗而失败 —— 判据必须取自实现。
  const btn = page.getByRole('button', { name: /转为\s*调\s*查\s*中/ })
  await expect(btn, 'open 状态下应有「转为 调查中」按钮').toBeVisible()
  await btn.click()

  // 1) 页面状态标签变了
  await expect(page.getByText('调查中').first()).toBeVisible({ timeout: 10_000 })

  // 2) **回读落库** —— 这一步才是判据 8 的本体。
  //    用轮询等后端最终一致：react-query invalidate 触发的 refetch 有延迟，
  //    一次性 GET 可能读到流转前的数据（那会假红）。
  await expect
    .poll(
      async () => {
        const d = await fetchIncident(INCIDENT_ID)
        return { status: d.incident.status, n: d.timeline.length }
      },
      { timeout: 10_000, intervals: [300, 500, 800, 1200] },
    )
    .toEqual({ status: 'investigating', n: tlBefore + 1 })

  // 3) 新增那条的 event_type 与 actor
  const after = await fetchIncident(INCIDENT_ID)
  const fresh = after.timeline.find((e) => e.event_type === 'investigation_started')
  expect(fresh, '时间线应含 investigation_started 条目').toBeTruthy()
  expect(fresh!.actor, '留痕必须有 actor（审计线索，不能是系统空值）').toBeTruthy()
  expect(after.timeline.length, '恰好新增一条（不多不少）').toBe(tlBefore + 1)

  // 页面上的时间线 Tab 计数也要跟着涨 —— 这条验的是前端订阅到了新数据，
  // 上面 2/3 验的是数据真落库。两者缺一都可能是假绿。
  const tab = page.getByRole('tab', { name: /^时间线/ })
  await expect(tab).toContainText(String(tlBefore + 1))
})
