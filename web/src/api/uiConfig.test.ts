// uiConfig 的单测：核心是「注入不存在 / 注入不完整 / 注入含空串」三种降级。
//
// 这个模块的失败症状都是静默的（链接点不开），所以每条用例都必须
// 断言**回落到了哪个具体值**，而不是「没抛异常」。

import { describe, it, expect, afterEach } from 'vitest'
import { uiEndpoints } from './uiConfig'
import { DEFAULT_UI_ENDPOINTS } from './externalLinks'

const GLOBAL = '__IIP_UI__'

function inject(v: unknown) {
  ;(window as unknown as Record<string, unknown>)[GLOBAL] = v
}

afterEach(() => {
  delete (window as unknown as Record<string, unknown>)[GLOBAL]
})

describe('uiEndpoints', () => {
  it('无注入时全部回落默认值（vite dev / 单测 / 未启用静态托管）', () => {
    expect(uiEndpoints()).toEqual(DEFAULT_UI_ENDPOINTS)
  })

  it('注入完整时逐字段采用注入值', () => {
    inject({
      prometheus: 'http://prom:9090',
      grafana: 'http://grafana:3000',
      loki: 'http://loki:3100',
      lokiTenantId: 't1',
    })
    expect(uiEndpoints()).toEqual({
      prometheus: 'http://prom:9090',
      grafana: 'http://grafana:3000',
      loki: 'http://loki:3100',
      lokiTenantId: 't1',
    })
  })

  // 逐字段回落而不是整体回落：只配了 Prometheus 时，
  // Grafana / Loki 仍该用自己的默认值（全回落会让已配的那项也丢）。
  it('只注入部分字段时，其余逐字段回落而非整体回落', () => {
    inject({ prometheus: 'http://prom:9090' })
    const got = uiEndpoints()
    expect(got.prometheus).toBe('http://prom:9090')
    expect(got.grafana).toBe(DEFAULT_UI_ENDPOINTS.grafana)
    expect(got.loki).toBe(DEFAULT_UI_ENDPOINTS.loki)
  })

  // 注入值是空串时若当有效值，链接会变成相对路径 /explore
  // 打到 UI 自己的端口上 —— 症状是「点了跳到本应用的 404」。
  it('注入空串视为未配置（IIP_UI_GRAFANA_URL= 这种写法很常见）', () => {
    inject({ prometheus: '   ', grafana: '', loki: '\t' })
    expect(uiEndpoints()).toEqual(DEFAULT_UI_ENDPOINTS)
  })

  it('注入值前后空白被 trim（shell 里 export 极易带上）', () => {
    inject({ prometheus: '  http://prom:9090  ' })
    expect(uiEndpoints().prometheus).toBe('http://prom:9090')
  })

  it('注入非对象（字符串/数字/null）时回落默认值，不抛异常', () => {
    for (const bad of ['oops', 42, null, true]) {
      inject(bad)
      expect(uiEndpoints()).toEqual(DEFAULT_UI_ENDPOINTS)
    }
  })

  it('字段类型不符（数字冒充字符串）时回落默认值', () => {
    inject({ prometheus: 9090 })
    expect(uiEndpoints().prometheus).toBe(DEFAULT_UI_ENDPOINTS.prometheus)
  })

  it('返回值是新对象 —— 调用方改它不会污染后续读取', () => {
    const a = uiEndpoints()
    a.prometheus = 'http://mutated'
    expect(uiEndpoints().prometheus).toBe(DEFAULT_UI_ENDPOINTS.prometheus)
  })
})
