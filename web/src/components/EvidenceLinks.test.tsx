// 外部链接在证据卡上的集成测试。
//
// 与 externalLinks.test.ts 的分工：那边验「URL 拼得对不对」，
// 这边验「按钮渲不渲染、点了会不会误触卡片」。
//
// 集成层的两个真实风险：
//  1. 卡片整体 onClick 是「展开明细」，链接按钮不 stopPropagation 就会
//     同时展开卡片并打开新标签（用户看到的是「点一下链接，明细莫名张开了」）
//  2. k8s 类证据不该出现链接按钮 —— 出现一个打不开的按钮比没有更糟

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/react'
import { App as AntApp } from 'antd'
import { EvidenceList } from './EvidenceList'
import type { EvidenceListResponse, Evidence } from '../api/evidence'

const GLOBAL = '__IIP_UI__'

function ev(over: Partial<Evidence> = {}): Evidence {
  return {
    id: 1,
    type: 'metric',
    source: 'prometheus',
    query: 'up',
    value: '1',
    timestamp: '2026-10-04T10:30:00Z',
    confidence: null,
    // 带一项 metadata 让卡片可展开（否则 onClick 不挂）
    metadata: { metric: 'up' },
    collected_at: '2026-10-04T10:31:00Z',
    ...over,
  }
}

function resp(items: Evidence[]): EvidenceListResponse {
  const byType: Record<string, number> = {
    config_change: 0,
    deployment: 0,
    historical: 0,
    log: 0,
    metric: 0,
    trace: 0,
  }
  for (const e of items) byType[e.type] = (byType[e.type] ?? 0) + 1
  return {
    evidence: items, count: items.length, total: items.length,
    truncated: false, limit: 50, by_type: byType,
  }
}

// antd v5 的 Empty/Tag 等依赖 App 上下文提供主题与静态方法，
// 脱离 <App> 直接渲染会报找不到 holder —— 故包一层（与
// EvidenceList.test.tsx 同一理由）。
function renderList(items: Evidence[]) {
  return render(
    <AntApp>
      <EvidenceList
        data={resp(items)}
        loading={false}
        error={null}
        filter={undefined}
        onFilterChange={vi.fn()}
        reload={vi.fn()}
      />
    </AntApp>,
  )
}

beforeEach(() => {
  delete (window as unknown as Record<string, unknown>)[GLOBAL]
})

afterEach(() => {
  cleanup()
  delete (window as unknown as Record<string, unknown>)[GLOBAL]
})

describe('EvidenceList 外部链接', () => {
  it('prometheus 证据渲染出「在 Prometheus 打开」链接', () => {
    renderList([ev()])
    const link = screen.getByRole('link', { name: /Prometheus/ })
    expect(link.getAttribute('href')).toContain('/graph?')
  })

  it('新标签打开并屏蔽 referrer（面板侧不该知道用户从哪来）', () => {
    renderList([ev()])
    const link = screen.getByRole('link', { name: /Prometheus/ })
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.getAttribute('rel')).toBe('noreferrer')
  })

  it('loki 证据给出两个面板的链接', () => {
    renderList([ev({ source: 'loki', type: 'log', query: '{job="api"} |= "e"' })])
    expect(screen.getByRole('link', { name: /Grafana/ })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Loki/ })).toBeInTheDocument()
  })

  // 渲染一个点不开的按钮比没有按钮更糟 —— 用户会以为面板挂了
  it('k8s 证据不渲染任何链接按钮', () => {
    renderList([ev({ source: 'k8s', type: 'deployment' })])
    expect(screen.queryByRole('link')).toBeNull()
  })

  it('query 为空的证据不渲染链接按钮（不崩、不给死按钮）', () => {
    renderList([ev({ query: '   ' })])
    expect(screen.queryByRole('link')).toBeNull()
  })

  // 这是集成层最该防的一条：卡片整体可点（展开明细），
  // 链接不拦住冒泡就会「点链接顺便把卡片展开了」
  it('点链接不触发卡片展开（stopPropagation 生效）', async () => {
    const { container } = renderList([ev()])
    const card = container.querySelector('.ant-card') as HTMLElement
    // 展开前明细不可见
    expect(screen.queryByText(/metric=up/)).toBeNull()

    const link = screen.getByRole('link', { name: /Prometheus/ })
    // 手工派发冒泡点击：jsdom 里 <a target=_blank> 的默认行为不影响这里，
    // 我们验的是「冒泡有没有到卡片」。
    link.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))

    // 关键断言：明细仍不可见 —— 说明事件没冒泡到卡片的 onClick。
    // 少 stopPropagation 时这里会红，且症状与「点链接顺带展开了卡片」一致。
    expect(screen.queryByText(/metric=up/)).toBeNull()
    expect(card).toBeInTheDocument()
  })

  it('注入的运行时地址被采用（改 IIP_UI_* 后链接跟着变）', () => {
    ;(window as unknown as Record<string, unknown>)[GLOBAL] = {
      prometheus: 'http://prom.injected:9090',
      grafana: 'http://grafana.injected:3000',
      loki: 'http://loki.injected:3100',
    }
    renderList([ev()])
    const link = screen.getByRole('link', { name: /Prometheus/ })
    expect(link.getAttribute('href')).toContain('http://prom.injected:9090/')
  })

  it('无注入时用 localhost 默认值，链接仍可渲染', () => {
    renderList([ev()])
    const link = screen.getByRole('link', { name: /Prometheus/ })
    expect(link.getAttribute('href')).toContain('http://localhost:9090/')
  })
})
