// 事件详情页的处置链路测试（手册 §4.4.2 最后两条）。
//
// 为什么这两条要单独立文件：IncidentDetail 是**唯一一处「用户点一下就改变
// 线上状态」的页面**。其他页面的 bug 是「显示错」，这一页的 bug 是「改错数据」
// 或「明明没改却告诉用户改成功了」。两种错误都不会有编译错误或类型错误。
//
// 覆盖的两条真实风险：
//  1. 点「转为X」必须真的发出 POST /incidents/{id}/transition，且 body 带
//     正确的 to / actor —— 漏传 actor 会让 timeline 里查不到是谁处置的
//  2. 后端 422 时必须显示后端原文。前端 canTransition 与后端 CanTransition
//     是两份独立实现（status.ts 注释里已说明），一旦算错就是「按钮可点但后端
//     拒绝」。此时用户看到一句自造的「流转失败」等于没给任何线索。

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, cleanup } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App as AntApp } from 'antd'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import IncidentDetail from './IncidentDetail'
import type { IncidentDetail as Detail } from '../api/incidents'

// 事件处于 acknowledged：allowedTargets 给出
// investigating / mitigated / resolved / ignored 四个（indexOf=1，只许前向）。
const ACK: Detail = {
  incident: {
    id: 7,
    title: 'iip-api 错误率突增',
    severity: 'critical',
    status: 'acknowledged',
    service: 'iip-api',
    started_at: '2026-10-04T10:00:00Z',
    resolved_at: null,
    updated_at: '2026-10-04T10:05:00Z',
    alert_count: 2,
    needs_review_count: 1,
  },
  alerts: [],
  timeline: [],
  truncated: { alerts: false, timeline: false },
}

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** 一次成功：GET 详情 → POST transition 成功。 */
function okFetch() {
  return vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      return json({ incident: { ...ACK.incident, status: 'mitigated' } })
    }
    void url
    return json(ACK)
  })
}

/** GET 成功，POST 一律 422 并带回后端原文。 */
function failFetch(serverMsg: string) {
  return vi.fn(async (_url: string, init?: RequestInit) => {
    if (init?.method === 'POST') {
      return json({ error: serverMsg, code: 'INVALID_TRANSITION' }, 422)
    }
    return json(ACK)
  })
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={qc}>
      <AntApp>
        <MemoryRouter initialEntries={['/incidents/7']}>
          <Routes>
            <Route path="/incidents/:id" element={<IncidentDetail />} />
          </Routes>
        </MemoryRouter>
      </AntApp>
    </QueryClientProvider>,
  )
}

/**
 * transBtn 取「转为X」按钮。
 *
 * 不用精确名：antd 会按排版规则在中文按钮文字里插空格（实测「转为已解决」
 * 渲染出的可访问名是「转为已解 决」），精确匹配会假红 —— 而插在哪几个
 * 字之间是 antd 内部的排版细节，随时可能变。→ 用「转为」+ 状态词的宽松匹配。
 */
function transBtn(label: string) {
  return screen.getByRole('button', {
    name: new RegExp('转为\s*' + label.slice(0, 2)),
  })
}

/** 等详情加载完（处置按钮出现即说明拿到了 incident）。 */
async function ready() {
  await waitFor(() => expect(screen.getByText(/处置/)).toBeInTheDocument())
  await screen.findByRole('button', { name: /转为已缓解/ })
}

/** 找出 transition 那次 POST 的请求体。 */
function transitionBody(fetchMock: ReturnType<typeof okFetch>) {
  const call = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')
  expect(call).toBeDefined()
  return JSON.parse((call as [string, RequestInit])[1].body as string) as {
    to: string
    actor: string
    note?: string
  }
}

beforeEach(() => {
  vi.restoreAllMocks()
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('IncidentDetail 处置链路', () => {
  it('可用的目标状态由前端状态机决定，且**不含**回到 open', async () => {
    // acknowledged 非终态 → canTransition 判 to==='open' 为假。
    // 若前端把 open 也渲染成按钮，用户点它只会拿到 422。
    const fetchMock = okFetch()
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    expect(transBtn('调查中')).toBeInTheDocument()
    expect(transBtn('已缓解')).toBeInTheDocument()
    expect(transBtn('已解决')).toBeInTheDocument()
    expect(transBtn('已忽略')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /转为\s*新建/ })).toBeNull()
  })

  it('点「转为已缓解」发出 POST transition，body 带正确的 to 与 actor', async () => {
    const fetchMock = okFetch()
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    await userEvent.click(transBtn('已缓解'))

    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([, i]) => i?.method === 'POST')).toBe(true),
    )
    const body = transitionBody(fetchMock)
    expect(body.to).toBe('mitigated')
    // actor 必填：timeline 靠它区分「值班的人处置」与「系统自动流转」。
    // 漏传不会报错，只会让审计线索断掉 —— 所以这里硬断言。
    expect(body.actor).toBe('web-console')
  })

  it('终态只有「重开」一个目标，且点它要二次确认', async () => {
    const resolved: Detail = {
      ...ACK,
      incident: { ...ACK.incident, status: 'resolved', resolved_at: '2026-10-04T11:00:00Z' },
    }
    const fetchMock = vi.fn(async (_u: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? json({ incident: { ...resolved.incident, status: 'open' } })
        : json(resolved),
    )
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await screen.findByRole('button', { name: /转为\s*新建/ })

    // resolved 是终态 → 唯一合法目标是 open。其余四个按钮必须不在。
    expect(transBtn('新建')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /转为\s*已解决/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /转为\s*已缓解/ })).toBeNull()

    await userEvent.click(transBtn('新建'))
    // 重开在真实值班里意味着「我判断关错了」，必须先确认。
    // 断言确认框出现，且**此时还没发 POST**。
    await screen.findByText(/重新打开这个事件/)
    expect(fetchMock.mock.calls.some(([, i]) => i?.method === 'POST')).toBe(false)
  })
})

describe('IncidentDetail 后端 422', () => {
  it('展示后端 error 原文，而不是自造的失败文案', async () => {
    // 这句话是后端对「不合法流转」的具体说明，前端自造的文案给不出
    // 「当前状态 X 不允许流转到 Y」这种信息。
    const SERVER = '当前状态 acknowledged 不允许流转到 mitigated（需经 investigating）'
    const fetchMock = failFetch(SERVER)
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    await userEvent.click(transBtn('已缓解'))

    // 判据是「界面上出现了后端那句原文」，不是「出现了一个错误提示」。
    // 前端自造的文案永远不包含服务端的具体状态名与建议路径。
    await waitFor(() => expect(screen.getByText(new RegExp(SERVER.slice(0, 24)))).toBeInTheDocument())
  })

  it('422 时不谎报成功（不出现「已流转到…」提示）', async () => {
    const fetchMock = failFetch('流转被拒：状态已被他人改为 resolved')
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    await userEvent.click(transBtn('已缓解'))
    await waitFor(() =>
      expect(screen.getByText(/流转被拒/)).toBeInTheDocument(),
    )
    // 失败时若还弹成功提示，用户会以为问题已解决而不再跟进 ——
    // 这是比「报错文案不好看」严重得多的后果。
    expect(screen.queryByText(/已流转到/)).toBeNull()
  })

  // **本批次修掉的缺陷的回归锁**（手册 §4.6.1）。
  //
  // 修前实测：失败后 ant-btn-loading 3141ms 才解除（= antd message 的默认
  // duration）；修后应在 message 时长之前解除。判据是**带阈值的耗时**，不是
  // 「最终会恢复」—— 修前也会恢复，只是慢 3 秒，只验恢复会假通过。
  //
  // **阈值为什么是 2000ms 而不是 1000ms**：判据真正要区分的是
  // 「随 message 解除（≈3s）」与「请求结束即解除（远小于 3s）」，
  // 1000ms 并不比 2000ms 更有区分度—— 它同样远低于 3s。
  // 而 1000ms 在**全量用例并行**时会被挤破（实测1080ms，单跑 3次全绿）：
  // 本条测的是墙钟耗时，机器一忙就红，且红的不是产品。
  // 2000ms 仍稳稳低于 3s，回归照样抓得住。
  it('422 后 message 时长前解锁且能立即再操作（loading 不被 message 时长拖住）', async () => {
    const fetchMock = failFetch('流转被拒：状态已被他人改为 resolved')
    vi.stubGlobal('fetch', fetchMock)
    const { container } = renderPage()
    await ready()

    // 选择器要**排除 Tabs 的 overflow 按钮**：它也带 ant-btn 类，
    // 用 `button.ant-btn.ant-btn-loading` 会把 Tabs 的
    // `ant-tabs-nav-more` 一并数进来，导致点按钮之前就「已有 4 个 loading」
    // 而假红。只数带有 ant-btn-primary / ant-btn-dangerous 的处置按钮。
    const loadingCount = () =>
      container.querySelectorAll(
        'button.ant-btn-primary.ant-btn-loading, button.ant-btn-dangerous.ant-btn-loading',
      ).length
    expect(loadingCount()).toBe(0)

    const t0 = Date.now()
    await userEvent.click(transBtn('已缓解'))
    await waitFor(() => expect(screen.getByText(/流转被拒/)).toBeInTheDocument())

    // 等 loading 解除，并记下耗时。**上限 2000ms 是本条的判据本体**：
    // 超过它说明 loading 又被 message 生命周期拖住了（回归）。
    // 阈值依据见上方注释：判据要区分的是「≈3s」与「远小于 3s」。
    await waitFor(() => expect(loadingCount()).toBe(0), { timeout: 2000 })
    const unlockedMs = Date.now() - t0
    expect(unlockedMs).toBeLessThan(2000)

    // 解除之后必须真能再操作：解锁 ≠ 可用。
    const again = await screen.findByRole('button', { name: /转为\s*已解决/ })
    await userEvent.click(again)
    await waitFor(() =>
      expect(fetchMock.mock.calls.filter(([, i]) => i?.method === 'POST').length).toBe(2),
    )
  })
})
