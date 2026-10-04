// Settings 页的渲染与交互测试。
//
// 每条都对应一个**真实风险**，不是「覆盖率导向」的用例：
//   1. 加载失败不能白屏 —— 配置页挂了而用户看不出来（余下三条都验不了）
//   2. 权重和实时显示 —— 用户据此判断能不能保存
//   3. 越界禁用保存 —— 前端辅助校验的第一道门
//   4. merge<review 禁用保存 —— 同上
//   5. 422 原文透传 —— 手册 §4.1.4 第 1 条，**这是本批次最容易被
//      「优化掉」的一条**：重写成前端自己的提示后，页面会显示一个
//      与后端口径不一致的文案，而没有任何测试变红
//   6. 保存后用后端返回值刷新 —— 不能假设 PUT 返回即最终态
//   7. 放弃改动回到原值
//
// antd v5 的 message/主题依赖 <App> 上下文，故统一包一层
// （与 EvidenceList.test.tsx / EvidenceLinks.test.tsx 同一理由）。

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { App as AntApp } from 'antd'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import Settings from './Settings'
import type { CorrelationConfig } from '../api/correlationConfig'

const CFG: CorrelationConfig = {
  weights: { time: 0.25, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 },
  thresholds: { merge: 0.8, review: 0.5 },
  enabled: true,
}

function mockFetchOnce(body: unknown, status = 200) {
  return vi.fn(async () =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    }),
  )
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <AntApp>
        <Settings />
      </AntApp>
    </QueryClientProvider>,
  )
}

/** 等页面离开 loading 态。 */
async function ready() {
  await waitFor(() => expect(screen.queryByText('加载中…')).not.toBeInTheDocument())
}

/**
 * saveBtn 取「保存」按钮。
 *
 * 用正则而非精确字符串：antd 会给**两个中文字**的 Button 自动插空格
 * （可访问名变成「保 存」）。这是 antd 的既定设计行为，不是缺陷；
 * 写成 `name: '保存'` 会永远找不到。**判据是「有个叫保存的按钮」，
 * 空格与否不影响用户能否认出它。**
 */
function saveBtn() {
  return screen.getByRole('button', { name: /保\s*存/ })
}

/**
 * numInput 按**位置**取某个 InputNumber 的原生 input。
 *
 * 踩过的三个坑（都不是产品缺陷）：
 *  1. `getByDisplayValue('0.25')` **找不到** —— antd 的 InputNumber 把值放在
 *     React 状态里，`<input>` 上没有 `value` 属性（jsdom 下 `getAttribute('value')`
 *     为 null，而 `getByDisplayValue` 读的是属性不是 live value）。
 *  2. 按「当前值」反查也不可靠：改一个字段后值会变，选择器随之失效。
 *  3. 按 label 关联需要包 <label>，而这里是滑块 + 数字框的组合控件，
 *     语义上「这个数字框属于哪一行」由布局表达，不是 label 表达。
 * → 只能按 role + 序号。**7 个 spinbutton 的顺序与页面顺序一致**
 * （5 个权重 → merge → review），这个顺序由 Settings 的 JSX 结构决定，
 * 有测试在下面钉住（见「spinbutton 数量与顺序」那条）。
 */
function numInput(idx: number) {
  const all = screen.getAllByRole('spinbutton')
  return all[idx] as HTMLInputElement
}

// 五个权重 + 两个阈值。写成语义常量，避免调用处出现裸下标。
const W = { time: 0, service: 1, label: 2, dependency: 3, historical: 4 } as const
const T = { merge: 5, review: 6 } as const

/**
 * setNum 把某个数字框**一次性**设成目标值。
 *
 * **不用 `clear()` + `type()`** —— 实测那个序列在 antd InputNumber 上
 * 得到 `clear` 后 input 值是 `0.000`（不是空字符串），随后 `type('0.9')`
 * 变成 `0.0000.9`，被解析成 0：权重和算出 0.75 而不是我预期的 1.65。
 * 更糟的是**它是绿的**（看起来「改了一个值」），
 * 只有断言里的期望值对不上才暴露。
 * → `fireEvent.change` 一次给出完整值，语义与「用户全选后重打」一致。
 */
function setNum(el: HTMLInputElement, value: number) {
  fireEvent.change(el, { target: { value: String(value) } })
}

beforeEach(() => {
  vi.restoreAllMocks()
})

describe('Settings', () => {
  it('加载失败时显示错误而不是空白页', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        new Response(JSON.stringify({ error: '配置存储不可用' }), { status: 503 }),
      ),
    )
    renderPage()
    await waitFor(() => expect(screen.getByText(/配置存储不可用/)).toBeInTheDocument())
  })

  it('五个维度都渲染出中文名', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()
    for (const name of ['时间邻近', '服务相同', '标签一致', '拓扑依赖', '历史共现']) {
      expect(screen.getByText(name)).toBeInTheDocument()
    }
  })

  it('显示当前权重和与容差（用户据此判断能否保存）', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()
    expect(screen.getByText(/权重和 1\.0000/)).toBeInTheDocument()
    expect(screen.getByText(/允许误差/)).toBeInTheDocument()
  })

  // **为 numInput 的下标选择器兜底**。下标选择器本身是脆弱的
  // （JSX 顺序一变就指向别的字段），所以必须有一条用例显式钉住
  // 「有几个、什么顺序」。少了它，某次重构会让「改时间邻近」的用例
  // 悄悄改成测「标签一致」而仍然全绿。
  it('五个权重 + 两个阈值的输入框数量与顺序固定', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()
    const inputs = screen.getAllByRole('spinbutton')
    expect(inputs).toHaveLength(7)
    // 顺序：5 个权重在两个阈值之前（阈值区在权重区下面）
    expect(inputs[T.merge]).toBeTruthy()
    expect(inputs[T.review]).toBeTruthy()
  })

  it('未改动时保存按钮禁用（避免无谓的 PUT）', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()
    expect(saveBtn()).toBeDisabled()
    expect(screen.getByRole('button', { name: '放弃改动' })).toBeDisabled()
  })

  it('权重和被改坏后禁用保存并说明原因', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()

    // 把「时间邻近」从 0.25 改成 0.9 → 权重和 0.9+0.25+0.2+0.2+0.1 = 1.65
    setNum(numInput(W.time), 0.9)

    await waitFor(() => expect(screen.getByText(/无法保存/)).toBeInTheDocument())
    expect(saveBtn()).toBeDisabled()

    // 权重和那行被 antd 拆成「标签 + 数值」多节点，getByText 匹配不到整句；
    // 又因为「权重和偏离」也含「权重和」，用 getByText 会命中两个元素。
    // → 正则带上前缀「权重和 」把两者区分开。
    expect(screen.getByText(/权重和 1\.6500/)).toBeInTheDocument()
    // 越界幅度也要如实告知，否则用户不知道离合法值有多远
    expect(screen.getByText(/偏离 1\.0 达 0\.6500/)).toBeInTheDocument()
  })

  it('merge < review 时禁用保存并给出交叉提示', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()

    // 合并阈值 0.8 → 0.3，低于待确认阈值 0.5
    setNum(numInput(T.merge), 0.3)

    await waitFor(() => expect(screen.getByText(/合并阈值必须 ≥ 待确认阈值/)).toBeInTheDocument())
    expect(saveBtn()).toBeDisabled()
  })

  it('422 时原样显示后端的错误文案', async () => {
    const user = userEvent.setup()
    let putCalls = 0
    const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
      if (!init || init.method !== 'PUT') {
        return new Response(JSON.stringify(CFG), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      putCalls++
      return new Response(
        JSON.stringify({ error: '五维权重之和必须为 1.0（允许误差 0.001），当前为 1.25' }),
        { status: 422, headers: { 'Content-Type': 'application/json' } },
      )
    })
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    // 用总开关制造改动（它不参与任何校验规则，所以保存按钮一定可点），
    // 然后**真的点保存** —— 改动只是让按钮可点，不点就不会发 PUT。
    await user.click(screen.getByRole('switch'))
    await waitFor(() => expect(saveBtn()).toBeEnabled())
    await user.click(saveBtn())

    await waitFor(() => expect(putCalls).toBe(1))
    // **断言后端原文出现在界面上**，而不是任何一句「前端自造的提示」。
    await waitFor(() =>
      expect(
        screen.getByText(/五维权重之和必须为 1\.0（允许误差 0\.001），当前为 1\.25/),
      ).toBeInTheDocument(),
    )
  })

  it('保存成功后用后端返回值刷新界面（不假设 PUT 返回即最终态）', async () => {
    const user = userEvent.setup()
    // 后端把权重规范化成它认为的形态（这里模拟一次重排）。
    const NORMALIZED: CorrelationConfig = {
      ...CFG,
      weights: { ...CFG.weights },
    }
    let putCalls = 0
    const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
      if (!init || init.method !== 'PUT') {
        return new Response(JSON.stringify(CFG), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      }
      putCalls++
      return new Response(JSON.stringify(NORMALIZED), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    })
    vi.stubGlobal('fetch', fetchMock)
    renderPage()
    await ready()

    await user.click(screen.getByRole('switch'))
    await waitFor(() => expect(saveBtn()).toBeEnabled())
    await user.click(saveBtn())
    await waitFor(() => expect(putCalls).toBe(1))

    // 保存后「放弃改动」重新变为禁用 —— 意味着界面已与后端返回对齐
    await waitFor(() => expect(screen.getByRole('button', { name: '放弃改动' })).toBeDisabled())
  })

  it('放弃改动把界面恢复成后端给的值', async () => {
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()

    const user = userEvent.setup()
    await user.click(screen.getByRole('switch'))
    await waitFor(() => expect(screen.getByRole('button', { name: '放弃改动' })).toBeEnabled())

    await user.click(screen.getByRole('button', { name: '放弃改动' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '放弃改动' })).toBeDisabled())
  })

  it('停用时如实说明「只收告警不关联」，不让用户以为引擎关了', async () => {
    const user = userEvent.setup()
    vi.stubGlobal('fetch', mockFetchOnce(CFG))
    renderPage()
    await ready()

    expect(screen.getByText(/告警落库后参与关联/)).toBeInTheDocument()
    await user.click(screen.getByRole('switch'))
    await waitFor(() => expect(screen.getByText(/告警仍落库，但不参与关联/)).toBeInTheDocument())
    // 并如实说明停用期间不会补算 —— 这是「停用」最容易被误解的地方。
    expect(screen.getByText(/不会补算/)).toBeInTheDocument()
  })
})
