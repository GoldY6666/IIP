// E2E-6  M5 判据 1/2/3 的行为层验证（2026-10-04 补）
//
// **为什么要有这条**：三条判据原先只有「间接证据」——
//   判据 1：E2E-4 验了 PUT 落库，但**没验「不重启后关联行为真的变了」**。
//           落库 ≠ 生效。若引擎把配置缓存在进程里，PUT 照样 200、库里照样是新值，
//           而行为用旧值 —— 这类缺陷 E2E-4 抓不到。
//   判据 2：Settings 组件测试验了「422 原样显示后端文案」，
//           但那是**用 mock 喂的 422**；后端真实文案是否「指明字段」没验过。
//   判据 3：EvidenceLinks 组件测试验了 href 拼装，
//           但「点开真链接能不能到面板」只在 jsdom 里验过属性，没有真点过。
//
// **判据 1 的可观测信号选 `enabled` 而不是权重**：
// 权重改动要「得分跨过阈值」才看得见，而得分依赖时间/共现等随机因素，
// 断言会不稳。`enabled=false` 时 `CorrelateAlerts` 在
// internal/correlation/service.go:66 直接 `return nil`——
// **告警照落 alerts 表，但不建任何 incident**。
//
// **踩过的坑（第一版断言错了）**：我最初假设「关掉后 3 条告警会落成 3 个事件、
// 只是不建关联边」，实测拿到 **0 个事件**。
// 读代码才看清：`correlateOne`（真正建 incident 的那个函数）在
// `CorrelateAlerts` 内部，引擎一停就整段跳过 —— **事件根本不会被创建**。
// 正确的不变量是「告警落库、事件数为 0」，这与 Settings 页的文案
// 「已停用：告警仍落库，但不参与关联」一致。
//
// **必须还原 enabled**：E2E 共享同一个后端，全局配置。
// 只关不还原会让后续用例在「引擎停用」状态下跑——
// 而 globalSetup 造的 A 组事件依赖关联（4 条并成 1 个），
// 下一轮 globalSetup 的 `alert_count === 4` 断言会假红。
import { test, expect } from '@playwright/test'
import { fetchConfig } from './helpers'
import { buildExternalLinks, DEFAULT_UI_ENDPOINTS } from '../src/api/externalLinks'
import type { Evidence } from '../src/api/evidence'

const APP = 'http://127.0.0.1:8087'

/** PUT 配置（部分更新：只带 enabled）。返回 HTTP 码与响应体。 */
async function putConfig(body: unknown): Promise<{ status: number; text: string }> {
  const r = await fetch(`${APP}/api/v1/correlation/config`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  return { status: r.status, text: await r.text() }
}

/** 投一条 firing 告警。三个计数都要返回 —— 只看 200 会把「落进 rejected」读成成功。 */
async function postAlert(
  labels: Record<string, string>,
): Promise<{ accepted?: number; rejected?: number; skipped?: number }> {
  const r = await fetch(`${APP}/api/v1/alerts/webhook`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      version: '4',
      alerts: [
        {
          status: 'firing',
          labels,
          annotations: { summary: 'E2E-6 判据验证告警' },
          startsAt: new Date(Date.now() - 120_000).toISOString().replace(/\.\d+Z$/, 'Z'),
          endsAt: new Date(Date.now() + 3600_000).toISOString().replace(/\.\d+Z$/, 'Z'),
        },
      ],
    }),
  })
  return JSON.parse(await r.text())
}

/** 按 service 找出该服务下的事件列表。 */
async function incidentsOf(service: string): Promise<Array<{ id: number; alert_count: number }>> {
  // 用服务端过滤而不是拉全量再客户端筛：
  // 客户端筛要靠limit=100 覆盖全部数据，数据一多就会漏。
  const r = await fetch(`${APP}/api/v1/incidents?limit=100&service=${encodeURIComponent(service)}`)
  const body = (await r.json()) as {
    incidents: Array<{ id: number; service: string; alert_count: number }>
  }
  return body.incidents || []
}

/**
 * 该服务下的告警条数（查 alerts 表，与事件表是两个东西）。
 *
 * 引擎停用时**告警照落库、只是不建事件** —— 所以判据 1 必须分别看这两处，
 * 只看事件会得出「什么都没落」的误判（第一版就是这么错的）。
 */
async function alertCountOf(service: string): Promise<number> {
  const r = await fetch(`${APP}/api/v1/alerts?limit=1000`)
  const body = (await r.json()) as { alerts: Array<{ labels: Record<string, string> }> }
  return (body.alerts || []).filter((a) => a.labels?.service === service).length
}

/**
 * 投 3 条同服务告警，返回「落库告警数」与「事件数」两个计数。
 *
 * **指纹必须每条不同**（靠 shard 参与指纹），否则第 2、3 条被去重 skip，
 * 测到的就不是「关联行为」而是「去重逻辑」（F6 踩过同类坑）。
 */
async function correlateProbe(
  run: string,
): Promise<{ alerts: number; events: number; alertCounts: number[] }> {
  const svc = `e2e-cfg-${run}`
  for (let i = 1; i <= 3; i++) {
    const res = await postAlert({
      alertname: 'E2ECfgProbe',
      service: svc,
      namespace: 'production',
      severity: 'critical',
      shard: String(i),
      run_token: run,
    })
    expect(res.accepted, `第 ${i} 条应被 accepted，实际 ${JSON.stringify(res)}`).toBe(1)
  }
  const list = await incidentsOf(svc)
  return {
    alerts: await alertCountOf(svc),
    events: list.length,
    alertCounts: list.map((i) => i.alert_count),
  }
}

test('判据 1：关掉引擎不重启 → 新告警不再建关联；打开后恢复（免重启生效）', async () => {
  const run = Math.random().toString(16).slice(2, 14)

  // ---- 前置：确认引擎当前是开的 ----
  // 不确认就往下走 = 脏数据下「关掉后不关联」可能只是本来就坏着。
  const before = await fetchConfig()
  expect(before.enabled, '前置条件：起始应为 enabled=true').toBe(true)

  try {
    // ---- ① 关掉引擎 ----
    const off = await putConfig({ enabled: false })
    expect(off.status, `关闭引擎应 200，实际 ${off.status} ${off.text}`).toBe(200)

    // 关键：**不重启服务**。整个用例期间没有任何进程重启动作，
    // 「生效」只能来自配置被重新读取。
    const offProbe = await correlateProbe(`${run}-off`)
    // 停用时：告警照落库（3 条），但**一个事件都不建**。
    expect(offProbe.alerts, '引擎停用时 3 条告警仍应落库').toBe(3)
    expect(offProbe.events, '引擎停用时不应创建任何事件（不参与关联）').toBe(0)

    // ---- ② 打开引擎（同样不重启） ----
    const on = await putConfig({ enabled: true })
    expect(on.status, `重开引擎应 200，实际 ${on.status}`).toBe(200)

    const onProbe = await correlateProbe(`${run}-on`)
    // 引擎恢复：3 条同服务告警落库且并进1 个事件。
    expect(onProbe.alerts, '引擎打开后 3 条告警应落库').toBe(3)
    expect(onProbe.events, '引擎打开后 3 条同服务告警应并成 1 个事件（关联恢复）').toBe(1)
    expect(
      onProbe.alertCounts[0],
      '并成的那 1 个事件应含 3 条告警（证明是真关联，不是各自独立）',
    ).toBe(3)

    // ---- ③ 库里确实是 true（排除「行为对但配置没存」） ----
    const after = await fetchConfig()
    expect(after.enabled, 'API 回读 enabled 应为 true').toBe(true)
  } finally {
    // 无论成败都必须还原，否则污染后续用例。
    await putConfig({ enabled: true })
    const restored = await fetchConfig()
    expect(restored.enabled, '收尾：enabled 必须还原为 true').toBe(true)
  }
})

test('判据 2：非法输入被后端 422 拒绝，且错误文案指明是哪个字段', async () => {
  // 逐个非法输入 → 期望 422 + 文案点名具体字段。
  //
  // **判据用「中文说法」而不是 JSON 字段名**（第一版写错了）：
  // 我最初断言文案要含 "weights"，实测后端返回
  // 「五维权重之和必须为 1.0（允许误差 0.001），当前为 1.5」——
  // **它明确指出了是权重这一维、还给了当前值**，对用户完全够用，
  // 只是没说「weights」这个英文键名。断言成 `toContain('weights')`
  // 会把一条正确的错误文案判成缺陷。
  //
  // 判据的实质是「用户能据此知道改哪个字段」，不是「文案里出现某个英文词」。
  const cases: Array<{ name: string; body: unknown; mustMention: string[] }> = [
    {
      name: '权重和不为 1',
      body: { weights: { time: 0.5, service: 0.5, label: 0.2, dependency: 0.2, historical: 0.1 } },
      // 既要说清是权重，也要给出当前值——用户才知道差多少
      mustMention: ['权重', '1.5'],
    },
    {
      name: 'merge 超出区间',
      body: { thresholds: { merge: 1.4, review: 0.5 } },
      mustMention: ['thresholds.merge', '1.4'],
    },
    {
      name: 'review 超出区间',
      body: { thresholds: { merge: 0.8, review: -0.2 } },
      mustMention: ['thresholds.review'],
    },
  ]

  for (const c of cases) {
    const r = await putConfig(c.body)
    expect(r.status, `${c.name} 应回 422，实际 ${r.status} ${r.text}`).toBe(422)
    for (const kw of c.mustMention) {
      expect(r.text, `${c.name} 的错误文案应含「${kw}」，实际：${r.text}`).toContain(kw)
    }
  }

  // merge < review（值都在区间内但互相矛盾）也必须被拒：
  // 它不会触发任何单字段区间检查，只能靠交叉校验。
  const crossed = await putConfig({ thresholds: { merge: 0.3, review: 0.6 } })
  expect(crossed.status, `merge<review 应回 422，实际 ${crossed.status} ${crossed.text}`).toBe(422)
  expect(crossed.text, 'merge<review 的文案应说明两者关系').toMatch(/merge|review|阈值/)

  // 键名打错必须是 400 而不是 422：它压根没解析成功，
  // 若回 422 会让用户以为「值不对」而其实「字段名错了」。
  const typo = await putConfig({ threshods: { merge: 0.9 } })
  expect(typo.status, `键名拼错应回 400，实际 ${typo.status} ${typo.text}`).toBe(400)

  // **收尾**：上面每条失败请求都不该改动配置。
  // 不验这一条，就可能出现「测试全绿但配置被写坏」。
  const cfg = await fetchConfig()
  expect(cfg.enabled, '非法输入不得改动 enabled').toBe(true)
  expect(cfg.weights, '非法输入不得改动权重').toEqual({
    time: 0.25,
    service: 0.25,
    label: 0.2,
    dependency: 0.2,
    historical: 0.1,
  })
  expect(cfg.thresholds, '非法输入不得改动阈值').toEqual({ merge: 0.8, review: 0.5 })
})

/**
 * 把某个维度的数字框填成 value 并失焦提交。
 *
 * 结构取自 E2E-4（WeightRow 是裸 div，不是 .ant-form-item）：
 * 按「含该 label 文本 + 含 spinbutton」筛出最近祖先。
 * 不从 helpers 复用是因为那条helper 绑死了 E2E-4 的 DIMS 顺序。
 */
async function setDim(page: import('@playwright/test').Page, label: string, value: number) {
  const row = page
    .locator('div')
    .filter({ has: page.getByText(label, { exact: true }) })
    .filter({ has: page.locator('input[role="spinbutton"]') })
  const input = row.last().locator('input[role="spinbutton"]').first()
  await input.fill(String(value))
  // blur 触发 onChange 提交：只 fill 不失焦，React 状态可能还没更新。
  await input.blur()
}

test('判据 2b：页面在提交前就拦住非法输入（不必等服务端 422）', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByText(/权重和/).first()).toBeVisible({ timeout: 10_000 })

  const save = page.getByRole('button', { name: /保\s*存/ })

  // 起始是默认值 ⇒ 保存应禁用（未改动）
  await expect(save).toBeDisabled()

  // 改坏权重和：把「时间邻近」改成 0.9，和变成 1.4。
  const row = page
    .locator('div')
    .filter({ has: page.getByText('时间邻近', { exact: true }) })
    .filter({ has: page.locator('input[role="spinbutton"]') })
  const input = row.last().locator('input[role="spinbutton"]').first()
  await input.fill('0.9')
  await input.blur()

  // **提交前就被拦**：按钮禁用，且说明原因。
  // 若按钮仍可点，就只能靠服务端 422 兜底 —— 手册要求的是「提交前」。
  await expect(save).toBeDisabled()
  await expect(page.getByText(/权重和/).first()).toBeVisible()

  // **改一个「和仍为 1」的合法值 → 按钮应重新可用**。
  //
  // 踩过的坑（第一版两次都错）：先只把 time 改成 0.9、和变1.4 → 阻断正确；
  // 接着只把 time 还原成 0.25 → 和回到 1.0 但 draft 与 baseline 一致，
  // `dirty` 变 false，按钮**仍然正确地**灰着。
  // 于是我改断言「还原后应可点」——又错，因为那时确实没有待保存的改动。
  //
  // 正确做法：动**两个**维度让和保持 1.0
  //（time 0.25→0.35、service 0.25→0.15）⇒ dirty=true 且 blocking=null ⇒ 可点。
  // 这样才真的在验「合法改动可以保存」，而不是在验按钮的启用条件。
  await setDim(page, '时间邻近', 0.35)
  await setDim(page, '服务相同', 0.15)
  await expect(save).toBeEnabled()

  // 收尾：还原成默认值（此时 dirty 变false、按钮转灰属正确），
  // 并显式回读 API 确认没把 0.35/0.15 写进库。
  await setDim(page, '时间邻近', 0.25)
  await setDim(page, '服务相同', 0.25)
  const cfg = await fetchConfig()
  expect(cfg.weights.time, '本用例只验前端拦截，不该真的保存过').toBe(0.25)
  expect(cfg.weights.service, '本用例只验前端拦截，不该真的保存过').toBe(0.25)
})

test('判据 3：证据面板链接的 query 被正确编码，点开能到目标面板', async ({ page }) => {
  // 这条验「URL 拼装 + 真点」。**不真点就只验了属性** ——
  // href 字符串对，不代表浏览器能打开它。
  //
  // **直接 import 生产代码的拼装函数**（而不是复制一份逻辑到用例里）：
  // 复制的话，用例验的是「用例自己拼的 URL 对」，与生产代码无关 ——
  // 那是自证（同源陷阱）。import 的是 `web/src/api/externalLinks.ts` 本体。

  // 真实采集器会带这些保留字符的 query（LogQL 尤其明显：
  // 有 `|=` `|~`、花括号、双引号）。
  const promQuery = 'rate(http_requests_total{job="api",code=~"5.."}[5m])'
  const lokiQuery = '{job="api"} |= "error" |~ "time.?out"'
  const window = '2026-10-04T10:00:00Z/2026-10-04T11:00:00Z'

  const promEvidence: Evidence = {
    id: 1,
    type: 'metric',
    source: 'prometheus',
    query: promQuery,
    value: '3x',
    timestamp: '2026-10-04T10:30:00Z',
    confidence: null,
    metadata: { window },
    collected_at: '2026-10-04T10:31:00Z',
  }
  const lokiEvidence: Evidence = {
    ...promEvidence,
    id: 2,
    type: 'log',
    source: 'loki',
    query: lokiQuery,
    value: 'count=5',
  }

  const promLinks = buildExternalLinks(promEvidence, DEFAULT_UI_ENDPOINTS)
  const lokiLinks = buildExternalLinks(lokiEvidence, DEFAULT_UI_ENDPOINTS)

  // 先确认「有链接可点」—— 若返回空数组，后面的导航断言全部无意义。
  expect(promLinks.length, 'prometheus 证据应给出 1 条链接').toBe(1)
  expect(lokiLinks.length, 'loki 证据应给出 2 条链接（Grafana Explore + Loki 直达）').toBe(2)

  // ---- 结构断言：保留字符必须被编码，且能反解回来 ----
  for (const [name, link] of [
    ['prometheus', promLinks[0]],
    ['loki-grafana', lokiLinks[0]],
    ['loki-direct', lokiLinks[1]],
  ] as const) {
    const parsed = new URL(link.href)
    expect(parsed.protocol, `${name} 应是 http/https 绝对地址，实际 ${link.href}`).toMatch(
      /^https?:$/,
    )
    // 裸空格会让浏览器截断链接；花括号/引号必须已被编码。
    expect(link.href.includes(' '), `${name} URL 不应含裸空格：${link.href}`).toBe(false)
  }

  // Prometheus：query 能在反解后还原成原文（编码正确、没丢字符）。
  //
  // **参数名是 `g0.expr` 不是 `query`**（Prometheus 图元参数约定，
  // 见 externalLinks.ts:89）—— 第一版按 `query` 取值拿到 null，
  // 一度以为编码坏了，其实是断言写错了。
  expect(
    new URL(promLinks[0].href).searchParams.get('g0.expr'),
    'prometheus 链接反解出的 PromQL 应与原文一致',
  ).toBe(promQuery)

  // Grafana Explore：left 是 JSON 串，反序列化后 expr 应还原成原文。
  // **这一条最有价值**：手工拼 `{"` 与 `"` 必然拼出坏链接，
  // 而坏链接在浏览器里表现为「打开了但查的是别的东西」—— 不反解看不出来。
  const leftRaw = new URL(lokiLinks[0].href).searchParams.get('left')
  expect(leftRaw, 'Grafana Explore 链接应带 left 参数').toBeTruthy()
  const left = JSON.parse(leftRaw as string) as {
    datasource: string
    queries: Array<{ refId: string; expr: string }>
  }
  expect(left.queries[0].expr, 'Grafana left 里的 expr 应与原 query 一致').toBe(lokiQuery)
  expect(left.datasource, '单租户时 datasource 应为 Loki').toBe('Loki')

  // Loki 直达：query 参数 + **毫秒时间戳**。
  // 时间戳这条很容易静默失效：Loki 原生接口只认毫秒，
  // 传 RFC3339 会被当0 → 链接点得开但永远返回空结果
  //（externalLinks.ts:160 记了这个坑）。
  const directUrl = new URL(lokiLinks[1].href)
  expect(directUrl.searchParams.get('query'), 'Loki 直达链接反解出的 query 应与原文一致').toBe(
    lokiQuery,
  )
  const startMs = Number(directUrl.searchParams.get('start'))
  const endMs = Number(directUrl.searchParams.get('end'))
  expect(Number.isFinite(startMs) && startMs > 1_000_000_000_000,
    `Loki start 应是毫秒时间戳（10 位秒级会被当 0），实际 ${directUrl.searchParams.get('start')}`).toBe(true)
  expect(endMs).toBeGreaterThan(startMs)
  // window 端点应与证据的 window 一致（毫秒 = Date.parse(RFC3339)）
  expect(startMs, 'start 应等于 window 起点').toBe(Date.parse('2026-10-04T10:00:00Z'))
  expect(endMs, 'end 应等于 window 终点').toBe(Date.parse('2026-10-04T11:00:00Z'))

  // ---- k8s 证据不渲染链接（不是所有来源都有「原始面板」概念） ----
  const k8sLinks = buildExternalLinks(
    { ...promEvidence, source: 'k8s', type: 'deployment' },
    DEFAULT_UI_ENDPOINTS,
  )
  expect(k8sLinks.length, 'k8s 证据不应渲染外部链接').toBe(0)

  // **本体：浏览器真的能「尝试」打开这个 href。**
  //
  // **踩过的坑（第一版我把 ERR_CONNECTION_REFUSED 当失败）**：
  // E2E 不起 prometheus/grafana（stack.mjs 只起 PG + iip-server），
  // 所以 localhost:9090 必然连不上。**但这恰恰证明 URL 是合法的**——
  // 非法 URL（乱码、控制字符、协议不对）在导航前就被浏览器拒绝，
  // 报错形态是 `ERR_INVALID_URL` / `ERR_ABORTED`，而不是「连不上」。
  //
  // 因此判据是「失败原因属于**连不上**」，而不是「导航成功」：
  //   ERR_CONNECTION_REFUSED（面板没部署）→ 通过
  //   ERR_INVALID_URL / ERR_ABORTED（URL 本身坏）→ 失败
  // 这个方向不能搞反：把「连不上」当失败，就等于要求 E2E 环境里
  // 必须有面板，那是把部署形态的验证错放到单条用例里。
  const URL_LEVEL_ERRORS = ['ERR_INVALID_URL', 'ERR_ABORTED', 'ERR_UNSUPPORTED_SCHEME']

  for (const [name, link] of [
    ['prometheus', promLinks[0]],
    ['loki-grafana', lokiLinks[0]],
    ['loki-direct', lokiLinks[1]],
  ] as const) {
    let navErr: string | null = null
    try {
      await page.goto(link.href, { waitUntil: 'commit', timeout: 15_000 })
    } catch (e) {
      navErr = (e as Error).message
    }
    if (navErr === null) {
      console.log(`[判据3] ${name} 导航成功（本机恰好有面板）`)
      continue
    }
    const isLevelError = URL_LEVEL_ERRORS.some((k) => navErr!.includes(k))
    expect(
      isLevelError,
      `判据3 ${name} 的 URL 本身不合法（应只是面板未部署）: ${navErr}\nURL=${link.href}`,
    ).toBe(false)
    // 必须是「连不上」这一类，那证明 URL 已被浏览器正常解析并发起连接。
    expect(
      navErr,
      `判据3 ${name} 的失败原因应是连不上（面板未部署），实际: ${navErr}`,
    ).toMatch(/ERR_CONNECTION_REFUSED|ERR_CONNECTION_RESET|ERR_ADDRESS_UNREACHABLE|ERR_EMPTY_RESPONSE/)
  }
})
