// E2E 共用助手：发现 globalSetup 造出的那条事件。
//
// **为什么不让 spec 直接读 process.env.E2E_INCIDENT_ID**
// 初版是 globalSetup 里 `process.env.X = ...` 传给 spec，拿不到就 `test.skip()`。
// 那是**假绿的温床**：传参一失败，5 条用例变 4 条，控制台照样一片绿 ——
// 判据 10 要求的「E2E 5 条」就悄悄变成了 4 条。
// 记忆里已登记过「按某字段过滤后断言为空会假通过」这类同源问题。
//
// 现在的口径：**spec 自己问后端要那条事件**（按 service 前缀查）。
// 造数失败 → 查不到 → `expect(...).toBeGreaterThan(0)` 当场红，
// 而不是静默 skip。用例数恒为 5，判据 10 才成立。
import { expect } from '@playwright/test'

const APP = 'http://127.0.0.1:8087'
const SERVICE_PREFIX = 'e2e-svc-'

export interface E2EIncident {
  id: number
  status: string
  service: string
  alert_count: number
}

/** 通过 API 直接读事件详情（E2E-3 / E2E-4 的回读判据）。 */
export async function fetchIncident(id: number) {
  const r = await fetch(`${APP}/api/v1/incidents/${id}`)
  expect(r.status, `GET /api/v1/incidents/${id} 应 200`).toBe(200)
  return (await r.json()) as {
    incident: E2EIncident
    alerts: unknown[]
    timeline: Array<{ id?: number; event_type: string; actor?: string; metadata?: Record<string, string> }>
    truncated: { alerts: boolean; timeline: boolean }
  }
}

/** 通过 API 直接读关联配置（E2E-4 的回读判据）。 */
export async function fetchConfig() {
  const r = await fetch(`${APP}/api/v1/correlation/config`)
  expect(r.status, 'GET /api/v1/correlation/config 应 200').toBe(200)
  return (await r.json()) as {
    weights: { time: number; service: number; label: number; dependency: number; historical: number }
    thresholds: { merge: number; review: number }
    enabled: boolean
  }
}

/**
 * 找出 globalSetup 造的那条**保持 open** 的事件（E2E-2 用）。
 *
 * 判据取「service 以 e2e-svc- 开头」而不是固定 id —— id 会随运行次数变，
 * 写死会让「连跑 2 次」的第二次直接找不到。
 *
 * **排除 `-claim` 结尾的**：那是 E2E-3 专用事件，会被推进到 investigating。
 * 不排除的话 E2E-2 可能拿到它（id 更大更晚造，取 max 时会优先），
 * 于是「状态标签是新建」这条断言就与 E2E-3 抢同一条数据。
 *
 * 找不到时**直接红**，不 skip。
 */
export async function findSeededIncident(): Promise<E2EIncident> {
  const r = await fetch(`${APP}/api/v1/incidents?limit=100`)
  expect(r.status, '列表接口应 200').toBe(200)
  const body = (await r.json()) as { incidents: E2EIncident[] }
  const mine = (body.incidents || []).filter(
    (i) => i.service.startsWith(SERVICE_PREFIX) && !i.service.endsWith('-claim'),
  )
  expect(
    mine.length,
    'globalSetup 造数失败或数据被清空 —— 期望 service 以 ' +
      `${SERVICE_PREFIX} 开头且不以 -claim 结尾，当前库里有 ` +
      `${(body.incidents || []).length} 条事件。`,
  ).toBeGreaterThan(0)
  return mine.sort((a, b) => b.id - a.id)[0]
}

/** 找出 E2E-3 专用的事件（service 以 e2e-svc-…-claim 结尾）。 */
export async function findClaimIncident(): Promise<E2EIncident> {
  const r = await fetch(`${APP}/api/v1/incidents?limit=100`)
  expect(r.status, '列表接口应 200').toBe(200)
  const body = (await r.json()) as { incidents: E2EIncident[] }
  const mine = (body.incidents || []).filter((i) => i.service.endsWith('-claim'))
  expect(
    mine.length,
    'globalSetup 未造出 E2E-3 专用事件 —— 期望 service 以 -claim 结尾，' +
      `当前库里有 ${(body.incidents || []).length} 条事件。`,
  ).toBeGreaterThan(0)
  return mine.sort((a, b) => b.id - a.id)[0]
}
