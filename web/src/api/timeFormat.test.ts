// timeFormat 的单测。
//
// 为什么这个文件必须有测试：它全是**阈值分支**，而阈值分支的 bug
// 有一个共同特征 —— 绝大多数输入都走对了，只在边界上错。
// 「59 秒前」「60 秒前」「59 分钟前」「60 分钟前」四组，
// 少判一个跨位，显示就会跳变（「59 秒前」直接变「1 分钟前」
// 而中间那一秒去哪了没人看得出来）。
//
// `now` 一律显式传入，不用 Date.now()：否则测试会在跨秒/跨分时
// 偶发失败，且失败时无法复现（这类 flaky 测试最消耗信任）。

import { describe, expect, it } from 'vitest'
import { absoluteTimeOf, parseTime, relativeTimeOf, timeAgoTip } from './timeFormat'

// 固定基准时刻：2026-10-04 22:30:00 本地时间。
// 用 Date 构造本地时间而非字符串，避免时区偏移让期望值算错。
const NOW = new Date(2026, 9, 4, 22, 30, 0).getTime()
const MIN = 60_000
const HOUR = 60 * MIN
const DAY = 24 * HOUR

/** 距 NOW 若干毫秒前的 RFC3339 串。 */
function ago(ms: number): string {
  return new Date(NOW - ms).toISOString()
}

describe('parseTime', () => {
  it('合法输入返回 Date', () => {
    expect(parseTime('2026-10-04T10:00:00Z')).toBeInstanceOf(Date)
  })

  it('空值与非法值返回 null 而不是 Invalid Date', () => {
    // Invalid Date 的传播性极差：算术里变 NaN、格式化里变字符串 "Invalid Date"，
    // 排查时会被误判成格式化 bug。集中判掉。
    expect(parseTime(null)).toBeNull()
    expect(parseTime(undefined)).toBeNull()
    expect(parseTime('')).toBeNull()
    expect(parseTime('not-a-date')).toBeNull()
  })
})

describe('relativeTimeOf', () => {
  it('不足 10 秒（含未来与恰好相等）一律「刚刚」', () => {
    expect(relativeTimeOf(ago(0), NOW)).toBe('刚刚')
    expect(relativeTimeOf(ago(5_000), NOW)).toBe('刚刚')
    // **未来时间也必须给「刚刚」**：时钟漂移或时区错误会让 startsAt
    // 落在稍后，输出「-3 秒前」这种荒谬文案比不显示更糟。
    expect(relativeTimeOf(ago(-5_000), NOW)).toBe('刚刚')
  })

  it('秒档跨位正确：59 秒仍是「59 秒前」，60 秒变「1 分钟前」', () => {
    // 这条是本文件的核心断言：阈值少判一位会让中间那一秒凭空消失。
    expect(relativeTimeOf(ago(59_000), NOW)).toBe('59 秒前')
    expect(relativeTimeOf(ago(60_000), NOW)).toBe('1 分钟前')
  })

  it('分档跨位正确：59 分钟 / 60 分钟', () => {
    expect(relativeTimeOf(ago(59 * MIN), NOW)).toBe('59 分钟前')
    expect(relativeTimeOf(ago(60 * MIN), NOW)).toBe('1 小时前')
  })

  it('时档跨位正确：23 小时 / 24 小时', () => {
    expect(relativeTimeOf(ago(23 * HOUR), NOW)).toBe('23 小时前')
    expect(relativeTimeOf(ago(24 * HOUR), NOW)).toBe('1 天前')
  })

  it('天档跨位正确：6 天 / 7 天（7 天后退化为绝对日期）', () => {
    expect(relativeTimeOf(ago(6 * DAY), NOW)).toBe('6 天前')
    // 超过 7 天给绝对日期而非「N 天前」/「N 个月前」——
    // 值班要能区分「3 小时前」和「昨天」，而「45 天前」精度不够。
    expect(relativeTimeOf(ago(7 * DAY), NOW)).toBe('2026-09-27 22:30')
    expect(relativeTimeOf(ago(400 * DAY), NOW)).toMatch(/^\d{4}-\d{2}-\d{2} /)
  })

  it('非法时间给「—」而不是崩溃或空白', () => {
    expect(relativeTimeOf(null, NOW)).toBe('—')
    expect(relativeTimeOf('bad', NOW)).toBe('—')
  })
})

describe('absoluteTimeOf', () => {
  it('输出稳定的本地时刻（补零、不带秒）', () => {
    // 不用 toLocaleString：它在没装 ICU 的机器上会退化成 "10/4/2026"，
    // 截图与「复制给同事看」都需要稳定格式。
    const d = new Date(2026, 0, 2, 3, 4, 0)
    expect(absoluteTimeOf(d)).toBe('2026-01-02 03:04')
  })

  it('接受字符串与 Date 两种入参', () => {
    const d = new Date(2026, 0, 2, 3, 4, 0)
    expect(absoluteTimeOf(d.toISOString())).toBe('2026-01-02 03:04')
    expect(absoluteTimeOf(d)).toBe('2026-01-02 03:04')
  })

  it('非法输入给「—」', () => {
    expect(absoluteTimeOf(null)).toBe('—')
    expect(absoluteTimeOf('nope')).toBe('—')
  })
})

describe('timeAgoTip', () => {
  it('同时给出绝对与相对两个口径', () => {
    // Tooltip 的用途正是「刚才说的 5 分钟前具体是几点几分」，
    // 只给一个口径就失去了悬停的意义。
    expect(timeAgoTip(ago(5 * MIN), NOW)).toBe('2026-10-04 22:25（5 分钟前）')
  })

  it('非法时间给可读提示', () => {
    expect(timeAgoTip(null, NOW)).toBe('时间未知')
  })
})