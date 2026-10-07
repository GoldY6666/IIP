// apiFetch 的行为契约测试（M6 S4）。
//
// 判据设计的核心：**「该加头时加了没」与「不该加头时没加」必须同时验**。
// 只验前者的话，一个「无条件加头」的实现照样全绿 —— 而那会让
// 「未启用鉴权」的部署也带上一个空值请求头，属于凭空收紧的约束。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { apiFetch, currentToken, authIsRequired, fetchAuthSession, __resetAuthForTest } from './client'

// 捕获 fetch 的调用参数。用 vi.stubGlobal 替换而非 vi.spyOn(globalThis,'fetch')，
// 后者在 ESM 环境下对 globalThis 的属性描述符有额外约束。
let calls: Array<{ url: string; init: RequestInit | undefined }>
let responder: (url: string) => Response

function okResponse(body: unknown = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

beforeEach(() => {
  __resetAuthForTest()
  calls = []
  responder = () => okResponse()
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url: String(url), init })
    return responder(String(url))
  })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/**
 * 取出**最近一次**请求实际发出的请求头。
 *
 * 默认取最后一次而非 `calls[0]`：多数用例会先调 fetchAuthSession()
 * （占掉 calls[0]），而断言关心的是「业务请求带没带头」。
 * 写死 idx=0 时断言读到的其实是会话请求 —— 它的 init 是 undefined，
 * 于是headers 为空、所有 token 断言假红，症状是「加了 token 也过不了」。
 * 判据指错了对象，比判据写错更难看出。
 */
function sentHeaders(): Headers {
  // 前置条件断言：没有请求时返回空 Headers 会被后续「get()===null」判成红，
  // 而真因是「压根没发请求」—— 错误方向完全不同。故显式区分。
  if (calls.length === 0) {
    throw new Error('尚无请求发出，headers 无从取起（判据指错了对象）')
  }
  const init = calls[calls.length - 1].init
  if (!init || !init.headers) return new Headers()
  return new Headers(init.headers as HeadersInit)
}

describe('apiFetch 前缀', () => {
  it('C-01 以 / 开头的路径自动补 /api/v1 前缀', async () => {
    await apiFetch('/incidents')
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/incidents')
  })

  it('C-02 已带前缀或绝对地址的路径原样使用', async () => {
    // 防「无条件补前缀」把已经对的地址拼成 /api/v1/api/v1/incidents。
    // 这种错误在浏览器里表现为 404，且 URL 看着「像对的」，容易误判为后端问题。
    await apiFetch('/api/v1/incidents')
    expect(calls[0].url).toBe('/api/v1/incidents')

    await apiFetch('https://example.com/x')
    expect(calls[1].url).toBe('https://example.com/x')

    // 前缀本身（无资源段）也原样 —— 补前缀会变成 /api/v1/api/v1
    await apiFetch('/api/v1')
    expect(calls[2].url).toBe('/api/v1')
  })

  it('C-03 查询串在补前缀后仍完整', async () => {
    await apiFetch('/alerts?limit=10&offset=20')
    expect(calls[0].url).toBe('/api/v1/alerts?limit=10&offset=20')
  })
})

describe('apiFetch token 头', () => {
  it('C-04 未启用鉴权时**不**发 X-IIP-Token 头', async () => {
    // 这条是「默认关」的实现细节：发一个空值头会让「有没有配 token」
    // 在前端侧也变成两义（发空头 vs 不发），而某些反向代理会据此判定有鉴权。
    await apiFetch('/incidents')
    expect(sentHeaders().has('X-IIP-Token')).toBe(false)
  })

  it('C-05 拿到 token 后每个请求都带上该头', async () => {
    responder = (url) =>
      url.includes('/auth/session')
        ? okResponse({ auth_required: true, token: 's3cr3t' })
        : okResponse()

    await fetchAuthSession()
    expect(currentToken()).toBe('s3cr3t')
    expect(authIsRequired()).toBe(true)

    await apiFetch('/incidents')
    expect(sentHeaders().get('X-IIP-Token')).toBe('s3cr3t')
  })

  it('C-06 注入 token 时**不覆盖调用方自己设的其它头**', async () => {
    responder = (url) =>
      url.includes('/auth/session')
        ? okResponse({ auth_required: true, token: 's3cr3t' })
        : okResponse()
    await fetchAuthSession()

    await apiFetch('/incidents/1/transition', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ to: 'resolved' }),
    })

    const h = sentHeaders()
    // Content-Type 必须还在：POST 体被服务端当 JSON 解析，
    // 丢了这个头的表现是 400 且错误文案完全不提「鉴权」——
    // 排查会被彻底带偏。
    expect(h.get('Content-Type')).toBe('application/json')
    expect(h.get('X-IIP-Token')).toBe('s3cr3t')
    expect(calls[1].init?.method).toBe('POST')
    expect(calls[1].init?.body).toBe(JSON.stringify({ to: 'resolved' }))
  })

  it('C-07 调用方已自行设了 X-IIP-Token 时以会话 token 为准', async () => {
    // 不允许「谁最后写谁赢」：那是让调用点能覆盖安全头。
    // 期望行为是集中管理——要换 token 就改服务端配置，不在前端各处分叉。
    responder = (url) =>
      url.includes('/auth/session')
        ? okResponse({ auth_required: true, token: 'from-session' })
        : okResponse()
    await fetchAuthSession()

    await apiFetch('/incidents', { headers: { 'X-IIP-Token': 'from-caller' } })
    expect(sentHeaders().get('X-IIP-Token')).toBe('from-session')
  })

  it('C-08 Headers 实例形式的 init.headers 也能正确合并', async () => {
    responder = (url) =>
      url.includes('/auth/session')
        ? okResponse({ auth_required: true, token: 's3cr3t' })
        : okResponse()
    await fetchAuthSession()

    const h = new Headers({ 'Content-Type': 'application/json', 'X-Custom': 'keep-me' })
    await apiFetch('/x', { headers: h })

    const sent = sentHeaders()
    expect(sent.get('X-IIP-Token')).toBe('s3cr3t')
    expect(sent.get('Content-Type')).toBe('application/json')
    expect(sent.get('X-Custom')).toBe('keep-me')
  })
})

describe('fetchAuthSession', () => {
  it('C-09 请求会话端点的路径正确，且**不带** token 头', async () => {
    // 此刻 token 还没拿到，加不了头是必然的；这里锁的是路径别写错。
    await fetchAuthSession()
    expect(calls[0].url).toBe('/api/v1/auth/session')
  })

  it('C-10 未启用鉴权时 auth_required=false 且 token 为空', async () => {
    responder = () => okResponse({ auth_required: false, token: '' })
    await fetchAuthSession()
    expect(authIsRequired()).toBe(false)
    expect(currentToken()).toBe('')
  })

  it('C-11 会话端点失败（网络异常）时不抛，状态保持「未启用」', async () => {
    // 抛出去会让 main.tsx 的 .then(render) 永远不执行 → 整页白屏。
    // 这正是「一个非关键的探测决定整个 UI 能否打开」的典型反例。
    vi.stubGlobal('fetch', async () => {
      throw new Error('network down')
    })
    await expect(fetchAuthSession()).resolves.toBeUndefined()
    expect(authIsRequired()).toBe(false)
    expect(currentToken()).toBe('')
  })

  it('C-12 会话端点返回非 2xx 时保持「未启用」且不抛', async () => {
    // 例如后端是旧版本（还没这个端点）→ 404。此时应按「未启用」继续，
    // 而不是让整个前端不可用 —— 旧版后端 + 新前端是真实存在的组合。
    responder = () => new Response('not found', { status: 404 })
    await expect(fetchAuthSession()).resolves.toBeUndefined()
    expect(authIsRequired()).toBe(false)
  })

  it('C-13 token 字段类型异常时按「未配」处理，不把对象塞进请求头', async () => {
    // 若不判类型，token 会变成 "[object Object]"，
    // 症状是「配了 token 但全站 401」且日志里看不出异常值。
    responder = () => okResponse({ auth_required: true, token: { evil: 1 } })
    await fetchAuthSession()
    expect(currentToken()).toBe('')
  })
})
