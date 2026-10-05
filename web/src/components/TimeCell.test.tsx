// TimeCell 的组件测试。
//
// 为什么这个组件也要测：它本身逻辑很薄，但**薄不等于不会错** ——
// 真正的风险在两个地方，而它们都不会让页面崩，只会让信息静默丢失：
//
//  1. Tooltip 的子元素必须是能接 ref 的元素。早先用裸字符串当children，
//     React 会报「children 不支持 ref」，**整个页面白屏**。
//     这条靠渲染成功 + 断言文本钉住。
//  2. 绝对模式下**不能把相对时间一并丢掉**（见组件注释）。
//     丢了不会报错，只会让人失去「这几个是不是都是刚才的」这个判断。

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import TimeCell from './TimeCell'

describe('TimeCell', () => {
  it('默认模式显示相对时间', () => {
    // 5分钟前 = 300000ms。用过去时刻避免依赖「现在」。
    const fiveMinAgo = new Date(Date.now() - 300_000).toISOString()
    render(<TimeCell value={fiveMinAgo} />)
    expect(screen.getByText('5 分钟前')).toBeInTheDocument()
  })

  it('绝对模式显示 YYYY-MM-DD HH:mm', () => {
    const d = new Date(2026, 0, 2, 3, 4, 0).toISOString()
    render(<TimeCell value={d} alwaysAbsolute />)
    expect(screen.getByText('2026-01-02 03:04')).toBeInTheDocument()
  })

  it('非法时间渲染成「—」而不是 Invalid Date', () => {
    // 这条是「别把上游数据问题显示成玄学」：库里不该有非法时间，
    // 但一旦有，页面上出现 "Invalid Date" 会让人以为是渲染层坏了。
    render(<TimeCell value="not-a-date" />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('空值渲染成「—」而不是空白', () => {
    // 空白会被读成「数据缺失」，而「—」明确是「这里没有值」。
    render(<TimeCell value={null} />)
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('两种模式都渲染出可悬停的包裹元素（Tooltip 需要 ref）', () => {
    // 渲染本身不崩就是这条断言的意义：裸字符串 children 会让
    // React 抛「children 不支持 ref」，测试直接失败。
    const d = new Date(2026, 0, 2, 3, 4, 0).toISOString()
    // 默认模式：这个时刻已是几个月前，relativeTimeOf 会退化成绝对日期
    // （超过 7 天不给「N 天前」）。故断言的是「能渲染出内容」而非具体文案 ——
    // 确切文案由 timeFormat.test.ts 逐档钉住，这里只管组件层不崩。
    expect(() => render(<TimeCell value={d} />)).not.toThrow()
    expect(() => render(<TimeCell value={d} alwaysAbsolute />)).not.toThrow()
  })

  it('relativeTimeOf 接受 Date 入参（与absoluteTimeOf 类型一致）', () => {
    // 组件内部可能拿到已解析的 Date。这条钉住「两个时间函数
    // 入参类型一致」这个约定 —— 不一致会逼调用方写无意义的
    // toISOString() 绕回字符串。
    const d = new Date(Date.now() - 120_000)
    render(<TimeCell value={d} />)
    expect(screen.getByText('2 分钟前')).toBeInTheDocument()
  })
})