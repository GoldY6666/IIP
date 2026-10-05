// URL 筛选同步的测试。
//
// 为什么这些纯函数也值得测：URL 同步的失败**症状极具欺骗性** ——
// 「打开带筛选的链接是好的」会让它看起来已实现，只有真去点一下筛选
// 才发现 URL 不动。而那个 bug 恰恰出在**序列化/解析**这两步，
// 是纯函数，可以直接钉住。
//
// **不测 useSearchParams 那层**：那要起真实 Router + 渲染，
// 属于集成测试（E2E 已覆盖「打开带参数链接」这条路径）。
// 这里只钉「值怎么进出 query string」—— 那才是容易错的部分。

import { describe, expect, it } from 'vitest'
import { parseSeverity, parseStatus, serializeStatus } from './useFilterUrl'

describe('parseStatus', () => {
  it('空与 null 返回空数组', () => {
    expect(parseStatus(null)).toEqual([])
    expect(parseStatus('')).toEqual([])
  })

  it('单个与多个状态都能解析', () => {
    expect(parseStatus('open')).toEqual(['open'])
    expect(parseStatus('open|resolved')).toEqual(['open', 'resolved'])
  })

  it('忽略非法值 —— URL 是用户可编辑的，不能原样发给后端', () => {
    // 这条是安全边界：`?status=drop table` 若原样透传，
    // 后端会 422，而用户看到的是一个他"没填错"的地址。
    // 正确反应是当成没填。
    expect(parseStatus('open|bogus')).toEqual(['open'])
    expect(parseStatus('bogus')).toEqual([])
    expect(parseStatus('open|')).toEqual(['open'])   // 尾部空项被丢掉
  })

  it('容忍分隔符两侧的空格', () => {
    // 手写 URL 时很容易带上空格（复制粘贴的常见副作用）。
    expect(parseStatus('open | resolved')).toEqual(['open', 'resolved'])
  })
})

describe('parseSeverity', () => {
  it('只认critical / warning / info', () => {
    expect(parseSeverity('critical|warning')).toEqual(['critical', 'warning'])
    expect(parseSeverity('critical|nuclear')).toEqual(['critical'])
    expect(parseSeverity(null)).toEqual([])
  })
})

describe('serializeStatus', () => {
  it('空数组给空串而不是分隔符', () => {
    // 否则 URL 里会出现 `status=|` 这种看着像 bug 的尾巴。
    expect(serializeStatus([])).toBe('')
  })

  it('顺序保持，���重复排序', () => {
    // 刻意保序：排序会让「我刚点的顺序」丢失，
    // 而那正是用户心里筛选器的排列顺序。
    expect(serializeStatus(['resolved', 'open'])).toBe('resolved|open')
  })
})

describe('parse ↔ serialize 往返', () => {
  it('合法值往返不变', () => {
    const v = ['open', 'resolved']
    expect(parseStatus(serializeStatus(v))).toEqual(v)
  })

  it('空值往返后仍为空（URL 里不留 ? 尾巴的前提）', () => {
    expect(serializeStatus(parseStatus(null))).toBe('')
  })
})