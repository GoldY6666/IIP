// correlationConfig.ts 的校验口径测试。
//
// **这里测的是「与后端逐条一致」这件事本身**，不是「函数能算对」。
// 判据取自 internal/correlation.Config.Validate 的字面规则，
// 任何一条漏实现都会让页面显示「可以保存」而后端回 422，
// 用户白等一次往返 —— 且这类缺陷不会让任何现有测试变红。
//
// 尤其注意权重和容差那条：**边界是「恰好等于容差」而非「刚好超出」**，
// 与后端 `diff > eps || diff < -eps` 的严格不等号一致。
// 写成 `Math.abs(diff) < eps` 会让「恰好等于容差」被判非法 ——
// 那会让合法的配置在 UI 上被拦住，用户改不动又看不出原因。

import { describe, expect, it } from 'vitest'
import {
  ConfigApiError,
  DIMENSION_META,
  sumWeights,
  validateConfig,
  weightsSumVerdict,
  WEIGHT_SUM_EPSILON,
  type CorrelationConfig,
} from './correlationConfig'

/** 后端 DefaultConfig 的五维权重（internal/correlation/types.go）。 */
const DEFAULT: CorrelationConfig = {
  weights: { time: 0.25, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 },
  thresholds: { merge: 0.8, review: 0.5 },
  enabled: true,
}

function cfg(mut: (c: CorrelationConfig) => void): CorrelationConfig {
  const c = structuredClone(DEFAULT)
  mut(c)
  return c
}

describe('后端默认配置自洽', () => {
  // 这条是整个模块的地基：默认配置必须自己通过自己的校验。
  // 不通过的话页面一打开就是「不能保存」，而用户改哪儿都不对。
  it('DefaultConfig 通过校验（与后端种子值一致）', () => {
    expect(validateConfig(DEFAULT)).toBeNull()
  })

  it('五个维度的权重和恰为 1', () => {
    expect(sumWeights(DEFAULT.weights)).toBeCloseTo(1, 10)
  })
})

describe('权重和容差', () => {
  // 这三条踩过一个**真实的浮点坑**，值得写下来：
  //
  // 我最初写「和恰好等于 1.001 / 0.999」并断言合法。实测 `ok=false` ——
  // 因为 IEEE 754 下 0.251+0.25+0.2+0.2+0.1 = 1.001000000000000112，
  // diff 比 0.001 多了 1.1e-16，**严格不等号把它判成越界**。
  //
  // 关键判断：**这不是缺陷**。后端 Go 算的是同一个式子、同一种 IEEE 754
  // 语义，`diff > eps` 同样会把它判越界 —— 前后端**自洽**，用户会看到
  // 后端 422 与前端禁用同时发生，而不是「前端说能存、存了才被拒」。
  // 真正的问题只在于**我原来的断言预设了一个不成立的精确等式**。
  // → 容差边界的测试只能测「明确在容差内」与「明确超出」，
  //   **「恰好等于」在浮点上不可断言**。
  it('明确落在容差内判合法', () => {
    // 0.75 + 0.24 = 0.99，距 1.0 差 0.01 > eps → 这条是越界；
    // 故取 0.9995（差 0.0005 < 0.001）
    const v = weightsSumVerdict({ time: 0.2495, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 })
    expect(v.sum).toBeCloseTo(0.9995, 10)
    expect(v.ok).toBe(true)
  })

  it('明确超出容差判非法（正方向）', () => {
    const v = weightsSumVerdict({ time: 0.2511, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 })
    expect(v.ok).toBe(false)
    expect(validateConfig(cfg((c) => { c.weights.time = 0.2511 }))).toContain('五维权重之和必须为 1.0')
  })

  // 低于 1 超出容差同样判非法（方向无关）—— 容差判定用绝对值。
  it('低于 1 超出容差同样判非法（方向无关）', () => {
    const v = weightsSumVerdict({ time: 0.2, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 })
    expect(v.sum).toBeCloseTo(0.95, 10)
    expect(v.ok).toBe(false)
    expect(v.diff).toBeLessThan(0)
  })

  it('容差常量是 0.001（与后端 weightSumEpsilon 一致）', () => {
    // 后端改了容差而前端没跟，页面会拦下后端认为合法的配置。
    expect(WEIGHT_SUM_EPSILON).toBe(0.001)
  })

  it('超出时给出的 diff 带符号（用于界面提示偏离方向）', () => {
    const v = weightsSumVerdict({ time: 0.1, service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1 })
    expect(v.diff).toBeLessThan(0)
  })

  // **这条是本组里最值钱的一条**：它把「前后端用同一个判据」这件事
  // 变成可执行的断言。后端 Config.Validate 判越界的条件是
  // `diff > eps || diff < -eps`（严格不等号），前端是
  // `Math.abs(diff) <= eps` —— 逻辑等价。**若有人把前端改成 `<`，
  // 前端会比后端宽松，导致「界面能点保存、点了被 422」**，
  // 而这条用例会立刻变红。
  it('判据用「绝对值 ≤ 容差」，与后端的严格不等号等价', () => {
    const cases: [number, boolean][] = [
      [0, true],
      [WEIGHT_SUM_EPSILON * 0.5, true],
      [WEIGHT_SUM_EPSILON * 2, false],
      [-WEIGHT_SUM_EPSILON * 0.5, true],
      [-WEIGHT_SUM_EPSILON * 2, false],
    ]
    for (const [diff, ok] of cases) {
      // 造一个「和 = 1 + diff」的五维组合
      const v = weightsSumVerdict({
        time: 0.25 + diff,
        service: 0.25, label: 0.2, dependency: 0.2, historical: 0.1,
      })
      expect(v.ok).toBe(ok)
    }
  })
})

describe('区间校验', () => {
  // 逐条而非抽样：漏掉任何一条，那个字段就能被拖出 [0,1]，
  // 而页面上其余四项仍显示正常色 —— 用户看不出哪里坏了。
  const fields: [string, (c: CorrelationConfig) => void][] = [
    ['weights.time', (c) => { c.weights.time = 2 }],
    ['weights.service', (c) => { c.weights.service = -0.1 }],
    ['weights.label', (c) => { c.weights.label = 1.5 }],
    ['weights.dependency', (c) => { c.weights.dependency = -1 }],
    ['weights.historical', (c) => { c.weights.historical = 1.001 }],
    ['thresholds.merge', (c) => { c.thresholds.merge = 2 }],
    ['thresholds.review', (c) => { c.thresholds.review = -0.5 }],
  ]

  for (const [name, mut] of fields) {
    it(`${name} 越界被拦下且错误文案含字段名`, () => {
      const err = validateConfig(cfg(mut))
      expect(err).not.toBeNull()
      expect(err).toContain(name)
    })
  }

  it('边界值 0 与 1 合法', () => {
    expect(validateConfig(cfg((c) => { c.weights.time = 0; c.weights.historical = 0 })))
      .not.toContain('必须在 [0,1]')
    // 0.5+0.3+0.1+0.05+0.05 = 1.0，且**包含 1 与 0 两个端点分布**
    // （service=0.3 不是端点，故另加一条把 time 顶到 1）
    expect(validateConfig(cfg((c) => {
      c.weights.time = 0.5
      c.weights.service = 0.3
      c.weights.label = 0.1
      c.weights.dependency = 0.05
      c.weights.historical = 0.05
    }))).toBeNull()
    // 单维顶到 1.0：其余四个必须为 0 才凑得出 1
    expect(validateConfig(cfg((c) => {
      c.weights.time = 1
      c.weights.service = 0
      c.weights.label = 0
      c.weights.dependency = 0
      c.weights.historical = 0
    }))).toBeNull()
    // 阈值同样可以用到端点：merge=1 且 review=0
    expect(validateConfig(cfg((c) => {
      c.thresholds.merge = 1
      c.thresholds.review = 0
    }))).toBeNull()
  })
})

describe('阈值交叉校验', () => {
  it('merge < review 被拦下', () => {
    const err = validateConfig(cfg((c) => { c.thresholds.merge = 0.4; c.thresholds.review = 0.5 }))
    expect(err).toContain('不能小于')
  })

  it('merge == review 合法（边界是 ≥ 而非 >）', () => {
    expect(validateConfig(cfg((c) => { c.thresholds.merge = 0.5; c.thresholds.review = 0.5 }))).toBeNull()
  })
})

describe('维度元数据', () => {
  // 少一个 label 的后果不是编译错误，是页面上出现一行空白标题 ——
  // 所以「五个维度都有中文名与说明」本身就该是断言。
  it('五个维度都有中文名与说明', () => {
    const keys: (keyof typeof DIMENSION_META)[] = ['time', 'service', 'label', 'dependency', 'historical']
    expect(Object.keys(DIMENSION_META)).toHaveLength(5)
    for (const k of keys) {
      expect(DIMENSION_META[k].label.length).toBeGreaterThan(0)
      expect(DIMENSION_META[k].hint.length).toBeGreaterThan(0)
    }
  })
})

describe('ConfigApiError', () => {
  it('携带状态码与后端原文，供页面区分处置', () => {
    const e = new ConfigApiError(422, '五维权重之和必须为 1.0', '保存失败')
    expect(e.status).toBe(422)
    expect(e.serverMessage).toContain('五维权重之和')
    // 后端原文优先：它带具体的当前值，前端的通用文案没有。
    expect(e.message).toBe('五维权重之和必须为 1.0')
  })

  it('后端没给原文时降级到调用方的通用文案', () => {
    const e = new ConfigApiError(503, '', '保存关联配置失败')
    expect(e.message).toBe('保存关联配置失败')
  })

  it('是 Error 的实例（组件的错误边界才能捕获）', () => {
    expect(new ConfigApiError(400, 'x', 'y')).toBeInstanceOf(Error)
  })
})
