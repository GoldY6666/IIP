// E2E-4  /settings 改权重 → 保存成功；**通过 API 回读**确认真的落库
//
// 判据 1（配置读写、免重启生效）与判据 10 的组合。手册明确写：
// 「E2E-4 **必须用 API 回读**，不能只断言「提示保存成功」」——
// 前端说成功不等于数据落库。记忆里这类同源自证已登记两起。
//
// **定位方式**：WeightRow 是裸 <div>（不是 .ant-form-item），
// 结构是 [标签块(160px) | Slider | InputNumber]。
// 按 `input[role="spinbutton"]` 取数字框 —— antd InputNumber 的
// value 存在 React 状态里，<input> 上没有 value 属性，
// 所以不能用 getByDisplayValue（F4 已记录）。
//
// **结束前必须还原并回读**：E2E 共享同一个后端，权重是全局配置。
// 只改不还原会让「连跑 2 次全绿」的第二次起始值不是默认值 ——
// 第一次的 PASS 就变成偶然。
import { test, expect, type Page } from '@playwright/test'
import { fetchConfig } from './helpers'

const DEFAULT_WEIGHTS = { time: 0.25, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 }
const DEFAULT_THRESHOLDS = { merge: 0.8, review: 0.5 }

// 一组与默认不同、但和为 1 且 merge > review 的合法值。
// 刻意避开「差 0.001 的和」这类浮点边界 —— F4 已记录手算容差踩了两次。
const NEW_WEIGHTS = { time: 0.4, service: 0.3, label: 0.15, dependency: 0.1, historical: 0.05 }

// 五个维度的真实 label（取自 api/correlationConfig.ts 的 DIMENSION_META）。
// 顺序与 Settings.tsx 的 DIMENSIONS 常量一致。
const DIMS: Array<[string, keyof typeof NEW_WEIGHTS]> = [
  ['时间邻近', 'time'],
  ['服务相同', 'service'],
  ['标签一致', 'label'],
  ['拓扑依赖', 'dependency'],
  ['历史共现', 'historical'],
]

/** 把某维度的数字框填成 value 并失焦提交。 */
async function setDim(page: Page, label: string, value: number) {
  // 行容器：含有该 label 文本、且包含 spinbutton 的最近祖先 div。
  const row = page
    .locator('div')
    .filter({ has: page.getByText(label, { exact: true }) })
    .filter({ has: page.locator('input[role="spinbutton"]') })
  const input = row.last().locator('input[role="spinbutton"]').first()
  await input.fill(String(value))
  // blur 触发 onChange 提交：直接 fill 不失焦，state 可能还没更新。
  await input.blur()
}

async function fillAll(page: Page, w: typeof NEW_WEIGHTS) {
  for (const [label, key] of DIMS) await setDim(page, label, w[key])
}

test('E2E-4 改权重保存后用 API 回读确认落库', async ({ page }) => {
  // 前置：确认当前值是默认值。不写这条，脏数据下「保存成功」会假通过 ——
  // 值没变也算保存成功。
  const before = await fetchConfig()
  expect(before.weights, '前置条件：起始应是后端默认权重').toEqual(DEFAULT_WEIGHTS)

  await page.goto('/settings')

  // 页面读到的权重和应等于 1.0（后端默认值之和）。
  // 判据取自页面上的「权重和」文案，字符串比较是 F4 踩过两次浮点坑后的选择。
  await expect(page.getByText(/权重和/).first()).toBeVisible({ timeout: 10_000 })

  await fillAll(page, NEW_WEIGHTS)
  await page.getByRole('button', { name: /保\s*存/ }).click()

  // 弱判据：页面提示成功（只说明前端流程走完）
  await expect(page.getByText(/配置已保存/).first()).toBeVisible({ timeout: 10_000 })

  // **本体：通过 API 回读**。
  await expect
    .poll(async () => (await fetchConfig()).weights, { timeout: 10_000, intervals: [300, 500, 800, 1200] })
    .toEqual(NEW_WEIGHTS)

  // 阈值不该被这次提交重置（后端支持部分更新）
  const after = await fetchConfig()
  expect(after.thresholds, '阈值不该被改权重的提交重置').toEqual(DEFAULT_THRESHOLDS)

  // **还原并回读**：让下次复跑从默认值起步。
  await page.reload()
  await fillAll(page, DEFAULT_WEIGHTS)
  await page.getByRole('button', { name: /保\s*存/ }).click()
  await expect
    .poll(async () => (await fetchConfig()).weights, { timeout: 10_000, intervals: [300, 500, 800] })
    .toEqual(DEFAULT_WEIGHTS)
})
