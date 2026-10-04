// buildExternalLinks 的单测。
//
// 这个模块的价值全部在「拼出来的链接能不能用」，而链接**不能用时不会报错**
// —— 浏览器只是打开一个空页面。因此断言必须验 URL 的**结构与编码**，
// 不能只验「返回了非空数组」。

import { describe, it, expect } from 'vitest'
import { buildExternalLinks, type UIEndpoints } from './externalLinks'
import type { Evidence } from './evidence'

const EP: UIEndpoints = {
  prometheus: 'http://prom.local:9090',
  grafana: 'http://grafana.local:3000',
  loki: 'http://loki.local:3100',
  lokiTenantId: '',
}

function ev(over: Partial<Evidence> = {}): Evidence {
  return {
    id: 1,
    type: 'metric',
    source: 'prometheus',
    query: 'up',
    value: '1',
    timestamp: '2026-10-04T10:30:00Z',
    confidence: null,
    metadata: {},
    collected_at: '2026-10-04T10:31:00Z',
    ...over,
  }
}

describe('buildExternalLinks', () => {
  describe('来源分流', () => {
    it('prometheus 出一条链接，指向 /graph', () => {
      const links = buildExternalLinks(ev(), EP)
      expect(links).toHaveLength(1)
      expect(links[0].href).toContain('/graph?')
      expect(links[0].href).toContain('g0.expr=up')
    })

    // Loki 给两个面板是有意的：Grafana 看图下钻、Loki 原生确认数据。
    // 只给一个的话运维得自己猜另一个在哪。
    it('loki 出两条链接：Grafana Explore + Loki 原生', () => {
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log' }), EP)
      expect(links).toHaveLength(2)
      expect(links[0].href).toContain('/explore?')
      expect(links[1].href).toContain('/loki/api/v1/query_range?')
    })

    it('source 大小写混杂仍能识别（DB 里存的是采集器常量，不保证大小写）', () => {
      const links = buildExternalLinks(ev({ source: 'Prometheus' }), EP)
      expect(links).toHaveLength(1)
      expect(links[0].href).toContain('/graph?')
    })

    it('k8s 等无对应面板的来源不产出链接 —— 不渲染好过渲染打不开的', () => {
      expect(buildExternalLinks(ev({ source: 'k8s', type: 'deployment' }), EP)).toEqual([])
    })

    it('source 为空串或未知值不产出链接，也不抛异常', () => {
      expect(buildExternalLinks(ev({ source: '' }), EP)).toEqual([])
      expect(buildExternalLinks(ev({ source: 'git' }), EP)).toEqual([])
    })
  })

  describe('空 query：不渲染链接', () => {
    // 渲染一个打不开的按钮比没有按钮更糟 —— 用户会以为面板挂了。
    it('空字符串不产出链接', () => {
      expect(buildExternalLinks(ev({ query: '' }), EP)).toEqual([])
    })

    it('纯空白视为空（采集器拼接很容易带空白）', () => {
      expect(buildExternalLinks(ev({ query: '   \n\t ' }), EP)).toEqual([])
    })
  })

  describe('URL 编码', () => {
    it('PromQL 的花括号被正确编码 —— 不编码必然拼出坏链接', () => {
      const q = 'sum(rate(http_requests_total{job="api",code=~"5.."}[5m]))'
      const links = buildExternalLinks(ev({ query: q }), EP)
      const href = links[0].href
      expect(href).not.toContain('{')
      expect(href).not.toContain('"')
      // URLSearchParams 把空格编成 +，Prometheus 接受
      expect(href).toContain('g0.expr=sum')
      // 反解回来必须与原串一致
      const sp = new URL(href).searchParams
      expect(sp.get('g0.expr')).toBe(q)
    })

    it('LogQL 的管道与花括号在 Grafana left 参数里被整体编码', () => {
      const q = '{job="api"} |= "error" | json | line_format "{{.msg}}"'
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log', query: q }), EP)
      const grafana = links.find((l) => l.href.includes('/explore'))!
      // left 是 JSON，必须整体编码：里面的 " 和 { 会截断参数
      expect(grafana.href).not.toContain('{"')
      const left = new URL(grafana.href).searchParams.get('left')
      expect(left).toBeTruthy()
      const parsed = JSON.parse(left!)
      expect(parsed.queries[0].expr).toBe(q)
    })

    it('Loki 原生链接的 query 同样被编码', () => {
      const q = '{job="api"} |= "error"'
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log', query: q }), EP)
      const direct = links.find((l) => l.href.includes('query_range'))!
      expect(new URL(direct.href).searchParams.get('query')).toBe(q)
    })
  })

  describe('时间范围', () => {
    const window = '2026-10-04T10:00:00Z/2026-10-04T11:00:00Z'

    it('有 window 时用它作两端时间', () => {
      const links = buildExternalLinks(ev({ metadata: { window } }), EP)
      const sp = new URL(links[0].href).searchParams
      expect(sp.get('g0.start_input')).toBe('2026-10-04T10:00:00Z')
      expect(sp.get('g0.end_input')).toBe('2026-10-04T11:00:00Z')
    })

    // Loki 原生接口只收**毫秒**时间戳。写 RFC3339 会被解析成 0 而返回空结果 ——
    // 症状是「链接能点开但什么都没有」，比点不开更难判断。
    it('Loki 原生链接把 RFC3339 转成毫秒时间戳', () => {
      const links = buildExternalLinks(
        ev({ source: 'loki', type: 'log', metadata: { window } }),
        EP,
      )
      const direct = links.find((l) => l.href.includes('query_range'))!
      const sp = new URL(direct.href).searchParams
      const start = Number(sp.get('start'))
      const end = Number(sp.get('end'))
      expect(start).toBe(Date.parse('2026-10-04T10:00:00Z'))
      expect(end).toBe(Date.parse('2026-10-04T11:00:00Z'))
      // 量级自检：毫秒是 13 位，微秒会变成 16 位。
      // 这条断言挡住「*1000 写反」这类错误 —— 它算出的数字仍是合法数字，
      // 只是差 1000 倍，没有量级断言就会静默通过。
      expect(String(start)).toHaveLength(13)
    })

    it('无 window 时退回 timestamp', () => {
      const links = buildExternalLinks(ev(), EP)
      const sp = new URL(links[0].href).searchParams
      expect(sp.get('g0.end_input')).toBe('2026-10-04T10:30:00Z')
      expect(sp.get('g0.start_input')).toBeNull()
    })

    it('Loki 无 window 时给 timestamp ±1h 窗口（单点几乎必然是空结果）', () => {
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log' }), EP)
      const direct = links.find((l) => l.href.includes('query_range'))!
      const sp = new URL(direct.href).searchParams
      const ts = Date.parse('2026-10-04T10:30:00Z')
      expect(Number(sp.get('start'))).toBe(ts - 3600_000)
      expect(Number(sp.get('end'))).toBe(ts + 3600_000)
    })

    // window 是 Record<string,string> 里的一个值，后端契约不保证它存在，
    // 格式也可能变。错解析比不解析更危险。
    it('window 格式异常（三段）时按「不可用」处理并退回 timestamp', () => {
      const links = buildExternalLinks(
        ev({ metadata: { window: 'a/b/c' } }),
        EP,
      )
      const sp = new URL(links[0].href).searchParams
      expect(sp.get('g0.start_input')).toBeNull()
      expect(sp.get('g0.end_input')).toBe('2026-10-04T10:30:00Z')
    })

    it('window 为空串时退回 timestamp', () => {
      const links = buildExternalLinks(ev({ metadata: { window: '' } }), EP)
      expect(new URL(links[0].href).searchParams.get('g0.start_input')).toBeNull()
    })

    it('metadata 缺失（undefined）不崩 —— 后端归一化但前端不能假设', () => {
      const bad = ev()
      // @ts-expect-error 刻意构造契约违例：验证前端不会因此崩
      bad.metadata = undefined
      expect(buildExternalLinks(bad, EP).length).toBeGreaterThan(0)
    })
  })

  describe('根地址处理', () => {
    // 少一次 trim 就是 http://h:3000//explore，Grafana 会 404 而
    // Prometheus 多数能容忍 —— 这类 bug 往往只在 Grafana 上暴露。
    it('末尾斜杠被 trim，不产生双斜杠', () => {
      const links = buildExternalLinks(
        ev(),
        { ...EP, grafana: 'http://g.local:3000/', prometheus: 'http://p.local:9090//' },
      )
      expect(links[0].href).toContain('http://p.local:9090/graph?')
      expect(links[0].href).not.toContain('9090//graph')

      const lokiLinks = buildExternalLinks(
        ev({ source: 'loki', type: 'log' }),
        { ...EP, grafana: 'http://g.local:3000/' },
      )
      expect(lokiLinks[0].href).toContain('http://g.local:3000/explore?')
    })
  })

  describe('多租户', () => {
    it('配了 tenant 时数据源用 Loki-{tenant} 形式', () => {
      const links = buildExternalLinks(
        ev({ source: 'loki', type: 'log' }),
        { ...EP, lokiTenantId: 'team-a' },
      )
      const left = JSON.parse(new URL(links[0].href).searchParams.get('left')!)
      expect(left.datasource).toBe('Loki-team-a')
    })

    it('Loki 原生链接带 tenant 参数', () => {
      const links = buildExternalLinks(
        ev({ source: 'loki', type: 'log' }),
        { ...EP, lokiTenantId: 'team-a' },
      )
      const direct = links.find((l) => l.href.includes('query_range'))!
      expect(new URL(direct.href).searchParams.get('tenant')).toBe('team-a')
    })
  })

  describe('链接文案与提示', () => {
    // 不做可达性探测（前端探测会引入新的失败模式），
    // 所以 title 必须说清前置条件，否则用户只会以为面板挂了。
    it('每条链接都有 label 与 title', () => {
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log' }), EP)
      for (const l of links) {
        expect(l.label.length).toBeGreaterThan(0)
        expect(l.title.length).toBeGreaterThan(0)
      }
    })

    it('Grafana 链接的 title 提示数据源 UID 可能不同', () => {
      const links = buildExternalLinks(ev({ source: 'loki', type: 'log' }), EP)
      const grafana = links.find((l) => l.href.includes('/explore'))!
      // 不同安装的 Loki 数据源 uid 不一致，传错时 Grafana 会静默回退到默认源
      expect(grafana.title).toContain('UID')
    })
  })

  describe('默认端点', () => {
    it('不传端点时用 localhost 默认值（对默认部署开箱可用）', () => {
      const links = buildExternalLinks(ev())
      expect(links[0].href).toContain('http://localhost:9090/')
    })
  })
})
