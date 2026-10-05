// E2E 造数：在真后端上造一批可预期的数据。
//
// **为什么造数在 globalSetup 而不是各用例 beforeAll**
// 五条用例共享同一批数据，各用例各造一次会产生多条事件，
// 而 E2E-2/3/4 各自的目标事件会互相干扰（详见下方「两组事件」）。
//
// **去重令牌必须落在 labels 里**
// 指纹只由非动态 labels 算（internal/alert/fingerprint.go 剔除
// instance/pod/container/node，annotations 与 payload 顶层字段一概不参与）。
// 注在 payload 顶层 = 没注入，同一 PGDATA 复用时必然 skipped。
// 记忆里也登记过：用 instance 区分同服务两条告警是**错的** ——
// instance 正是剔除项，两条 instance 不同但其余 labels 相同 → 指纹相同。
//
// **造两组互不相干的事件**（这是 F6 修掉的一个设计缺陷）
//   A 组 `e2e-svc-<run>`        —— 保持 open，E2E-2 验列表状态标签「新建」
//   B 组 `e2e-svc-<run>-claim` —— 会被推进到 investigating，E2E-3 用
// 共用一条会让 E2E-2 的断言依赖执行顺序（workers:1 下靠文件名字母序
// 恰好成立，但那是巧合不是判据）。
import { expect } from '@playwright/test'
import { findClaimIncident, findSeededIncident } from './helpers'

const APP = 'http://127.0.0.1:8087'

interface PostAlertOpts {
  summary?: string
}

/**
 * 造一条 firing 告警。
 *
 * 响应的 `accepted` / `rejected` / `skipped` 三个计数都要看 ——
 * 只看 HTTP 200 会把「200 但落进 rejected」读成成功
 * （记忆里登记过：手工投 webhook 时省略 status 就是这个形态）。
 */
async function postAlert(
  labels: Record<string, string>,
  opts: PostAlertOpts = {},
): Promise<{ accepted?: number; rejected?: number; skipped?: number; updated?: number }> {
  const body = {
    version: '4',
    alerts: [
      {
        status: 'firing',
        labels,
        annotations: { summary: opts.summary || 'E2E 造数告警' },
        startsAt: new Date(Date.now() - 120_000).toISOString().replace(/\.\d+Z$/, 'Z'),
        endsAt: new Date(Date.now() + 3600_000).toISOString().replace(/\.\d+Z$/, 'Z'),
      },
    ],
  }
  const r = await fetch(`${APP}/api/v1/alerts/webhook`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  const text = await r.text()
  try {
    return JSON.parse(text)
  } catch {
    throw new Error(`webhook 响应不是 JSON: ${text.slice(0, 200)}`)
  }
}

export default async function globalSetup() {
  // 1) 健康检查：必须 200 且 body 是 'ok'。
  //    只判 status 不够 —— 不查库的 healthz 会一直 200（链路 A 验过这个形态）。
  const hl = await fetch(`${APP}/healthz`)
  expect(hl.status, 'E2E 开始前 /healthz 必须 200').toBe(200)
  expect((await hl.text()).trim()).toBe('ok')

  // 2) 造数。**每次运行用全新 service 名**（带随机 run 段），
  //    这样即使 PGDATA 里有上次的行，指纹也不同，不会被去重 skip。
  const run = Math.random().toString(16).slice(2, 14)
  const svc = `e2e-svc-${run}`

  // A 组：4 条同服务告警。4 条是为了让关联打分够高、能建边，
  // E2E-2 的详情页才有「关联告警 (n)」可看。
  for (let i = 1; i <= 4; i++) {
    const res = await postAlert(
      {
        alertname: 'E2EFiring',
        service: svc,
        namespace: 'production',
        severity: i === 1 ? 'critical' : 'warning',
        // shard 参与指纹（instance 被剔除），用它区分同一服务下的多条。
        shard: String(i),
        run_token: run,
      },
      { summary: `E2E 告警 ${i}` },
    )
    expect(res.accepted, `第 ${i} 条告警应被 accepted，实际 ${JSON.stringify(res)}`).toBe(1)
    expect(res.rejected ?? 0, '不应有 rejected').toBe(0)
    expect(res.skipped ?? 0, '不应被去重 skip（service 已带随机 run 段）').toBe(0)
  }

  // 3) 确认两组事件都真的生成了。
  //    **不确认就往下走 = 把「造数失败」当成「页面问题」**：
  //    五条用例会分别报「列表没行」「按钮找不到」，报错完全指向错误的层。
  const a = await findSeededIncident()
  expect(a.status, 'A 组事件应处于 open（E2E-2 验列表状态标签「新建」）').toBe('open')
  expect(a.alert_count, `A 组事件应关联 4 条告警，实际 ${a.alert_count}`).toBe(4)

  // 4) B 组：E2E-3 专用，会被推进到 investigating。
  const claimSvc = `${svc}-claim`
  const claim = await postAlert(
    {
      alertname: 'E2EClaim',
      service: claimSvc,
      namespace: 'production',
      severity: 'critical',
      run_token: run,
    },
    { summary: 'E2E 待认领事件' },
  )
  expect(claim.accepted, 'B 组告警应被 accepted').toBe(1)
  expect(claim.rejected ?? 0, 'B 组不应有 rejected').toBe(0)

  const b = await findClaimIncident()
  expect(b.status, 'B 组事件应处于 open（E2E-3 要从 open 推进）').toBe('open')

  console.log(
    `[e2e] 造数完成：A 组 service=${a.service} id=${a.id} alerts=${a.alert_count}；` +
      `B 组 service=${b.service} id=${b.id}`,
  )
}
