// EvidenceList 的空态与错误态（手册 §4.4.2 第 1 条，以及 §7.2 的「零值」风险）。
//
// **为什么第一组测试就测空态**：「缺数据被序列化成零值」是本仓库反复踩过的坑
// （M3 的 AlertCount、M4 的 evidence.count 都栽过）。
// 页面上的对应形态就是「什么都不显示」—— 用户无法区分
// 「查过了，确实没有证据」与「根本没查」。组件若只给一句空白，
// 就等于把这个区分抹掉了。
//
// 断言**逐条对应组件注释里写明的设计决定**（该组件第 15-19 行：
// 「采集不到时要说清是谁没采到」），不是断言「渲染没崩」——
// 后者只证明 import 路径对，什么都不证明。
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { EvidenceList } from './EvidenceList'
import {
  EVIDENCE_TYPE_LABEL,
  type EvidenceListResponse,
} from '../api/evidence'

// antd v5 的 Alert/Empty/message 等依赖 App 上下文提供主题与静态方法，
// 脱离 <App> 直接渲染会报找不到 holder —— 故每个用例都包一层。
function renderList(props: Partial<React.ComponentProps<typeof EvidenceList>> = {}) {
  const base: React.ComponentProps<typeof EvidenceList> = {
    data: undefined,
    loading: false,
    error: null,
    filter: undefined,
    onFilterChange: vi.fn(),
    reload: vi.fn(),
    ...props,
  }
  return render(
    <AntApp>
      <EvidenceList {...base} />
    </AntApp>,
  )
}

function emptyResponse(over: Partial<EvidenceListResponse> = {}): EvidenceListResponse {
  return {
    evidence: [],
    count: 0,
    total: 0,
    limit: 50,
    truncated: false,
    by_type: {
      config_change: 0,
      deployment: 0,
      historical: 0,
      log: 0,
      metric: 0,
      trace: 0,
    },
    ...over,
  }
}

describe('EvidenceList 空态', () => {
  it('0 条证据时显示「暂无证据」而不是空白页', () => {
    renderList({ data: emptyResponse() })
    // 空态必须**说人话**：用户要能区分「查过了没有」与「没查」。
    expect(screen.getByText(/暂无证据/)).toBeInTheDocument()
  })

  it('0 条证据时把六个类型的角标全显示为 0 —— 显示 0 与不显示同样重要', () => {
    renderList({ data: emptyResponse() })
    // 不显示角标 vs 显示 0 是两件事：前者用户不知道查没查过这一类。
    //
    // **标签取自 EVIDENCE_TYPE_LABEL 常量而非硬编码中文**：
    // 写死 '部署' 这类字符串时，后端改名（实测 deployment 的标签是「部署记录」
    // 而不是「部署」）测试就会假失败 —— 而失败的是测试，组件是对的。
    // 引用常量则随契约一起变，改类型时测试自动跟上。
    for (const label of Object.values(EVIDENCE_TYPE_LABEL)) {
      expect(screen.getByText(new RegExp(`${label}\\s*\\(0\\)`))).toBeInTheDocument()
    }
  })

  it('按类型筛选且该类型为 0 时，说明是「该类型没有」而不是「全部都没有」', () => {
    renderList({ data: emptyResponse(), filter: 'metric' })
    // 措辞必须区分筛选与非筛选，否则用户以为整个事件都没证据。
    expect(screen.getByText(/该事件没有/)).toBeInTheDocument()
    // 「指标」会出现多处（筛选器的角标 + 空态描述里的加粗），
    // 因此用 getAllByText 并**断言至少两处** ——
    // 这比 getByText 更能说明「筛选器与空态都在讲同一个类型」。
    const hits = screen.getAllByText(
      new RegExp(EVIDENCE_TYPE_LABEL.metric),
    )
    expect(hits.length).toBeGreaterThanOrEqual(2)
    // 且必须明确说的是「该类型没有」而不是「暂无证据」。
    expect(screen.queryByText(/暂无证据/)).not.toBeInTheDocument()
  })

  it('空态里说明证据来自 worker 自动采集、仍在进行中', () => {
    renderList({ data: emptyResponse() })
    // 手册判据 1 是「2 分钟内产出」，页面必须让用户知道这段时间的存在。
    expect(screen.getByText(/worker/)).toBeInTheDocument()
    expect(screen.getByText(/2 分钟/)).toBeInTheDocument()
  })
})

describe('EvidenceList 错误态', () => {
  it('查询失败时明确说「读不到证据」而非「没有证据」', () => {
    renderList({ error: new Error('HTTP 503 service unavailable') })
    expect(screen.getByText(/读取证据失败/)).toBeInTheDocument()
    // 这段区分是组件的设计决定（第 91-93 行）：
    // 采集与查询是两件事，查询失败不代表采集没发生。
    expect(screen.getByText(/不代表采集没发生/)).toBeInTheDocument()
  })

  it('错误态展示后端返回的原始 message，不自造文案', () => {
    renderList({ error: new Error('HTTP 503 service unavailable') })
    // 自造文案会丢掉可判别的细节（HTTP 503 与连接拒绝排查方向不同）。
    expect(screen.getByText(/HTTP 503/)).toBeInTheDocument()
  })

  it('错误态提供重试按钮并回调 reload', () => {
    const reload = vi.fn()
    renderList({ error: new Error('boom'), reload })
    // 正则里显式容纳 antd 的**两字中文按钮会自动插入空格**这一行为
    // （实测渲染为「重 试」）。这不是组件缺陷而是 antd 的排版规则，
    // 用 /重\s*试/ 兼容两种形态 —— 若哪天 antd 不插了，正则依然匹配。
    const btn = screen.getByRole('button', { name: /重\s*试/ })
    btn.click()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('错误态时**不**显示「暂无证据」—— 两者混淆会让用户查错方向', () => {
    renderList({ error: new Error('boom') })
    // 这是本组测试的核心断言：失败与「确实没有」必须长得不一样。
    expect(screen.queryByText(/暂无证据/)).not.toBeInTheDocument()
  })
})

describe('EvidenceList 截断态（手册 §4.2）', () => {
  it('truncated=true 时回显「共 N 条」而不是把上限说成总数', () => {
    renderList({
      data: emptyResponse({
        evidence: [],
        count: 50,
        total: 137,
        limit: 50,
        truncated: true,
      }),
    })
    // 吞掉截断 = 把「只保留了 50 条」说成「一共 50 条」。
    expect(screen.getByText(/共 137 条/)).toBeInTheDocument()
  })

  it('truncated=false 时不显示截断告警', () => {
    renderList({ data: emptyResponse({ count: 5, total: 5 }) })
    // 反向断言：不该在未截断时也提示「超过上限」，那会让人以为证据丢了。
    expect(screen.queryByText(/证据超过/)).not.toBeInTheDocument()
  })
})

describe('EvidenceList 加载态', () => {
  it('loading 时不显示空态（避免闪一下「暂无证据」）', () => {
    renderList({ loading: true, data: emptyResponse() })
    // 加载中就显示「暂无证据」会闪烁，用户会以为真的没有。
    expect(screen.getByText(/加载中/)).toBeInTheDocument()
    expect(screen.queryByText(/暂无证据/)).not.toBeInTheDocument()
  })
})
