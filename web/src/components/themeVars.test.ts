import { describe, expect, it } from 'vitest'
import { CODE_BG_VAR, CODE_FG_VAR, themeVars } from './themeVars'
import { PALETTES, type ThemeMode } from '../theme'

/**
 * 这些断言护的是「切亮色后 antd token 覆盖不到的那部分 CSS」。
 *
 * 背景：表格内 code 的底色曾写死暗色值 #1c2128。切亮色后事件 ID
 * 渲染成一个个黑块—— 而 antd token 全对、tsc 全绿、组件测试全绿，
 * **只有真实截图能看出来**。所以这里锁住变量与调色板的对应关系。
 */
const MODES: ThemeMode[] = ['dark', 'light']

describe('themeVars', () => {
  it.each(MODES)('%s 模式同时给出底色与文字色变量', (mode) => {
    const v = themeVars(mode, PALETTES[mode])

    expect(v[CODE_BG_VAR]).toBeTruthy()
    expect(v[CODE_FG_VAR]).toBeTruthy()
  })

  it('两套主题的 code 底色不同（写死值会让亮色出现黑块）', () => {
    const dark = themeVars('dark', PALETTES.dark)
    const light = themeVars('light', PALETTES.light)

    expect(dark[CODE_BG_VAR]).not.toBe(light[CODE_BG_VAR])
    expect(dark[CODE_FG_VAR]).not.toBe(light[CODE_FG_VAR])
  })

  // 反向自证：底色必须**取自调色板**，不能是碰巧对上的常量。
  // 若有人改成写死字面量，这条会红。
  it('亮色下底色取自亮色调色板', () => {
    const v = themeVars('light', PALETTES.light)

    expect(Object.values(PALETTES.light.surface)).toContain(v[CODE_BG_VAR])
  })

  it('暗色下底色取自暗色调色板', () => {
    const v = themeVars('dark', PALETTES.dark)

    expect(Object.values(PALETTES.dark.surface)).toContain(v[CODE_BG_VAR])
  })

  // code 文字色**刻意不在 semantic 里**（亮色 info 对浅灰底只有 3.87:1），
  // 所以断言是「与某个可读的蓝不同、且对比度达标」，而不是「等于 info」。
  // 写成「等于 info」会把这次修复判成回归 —— 那种测试比没有更糟。
  it('亮色 code 文字不是 lightSemantic.info（那个对比度不够）', () => {
    const v = themeVars('light', PALETTES.light)

    expect(v[CODE_FG_VAR]).not.toBe(PALETTES.light.semantic.info)
  })

  // 亮色下若沿用暗色的 #1c2128，就是这次修的那个 bug。
  it('亮色底色不是暗色值（这就是黑块缺陷的守卫）', () => {
    const light = themeVars('light', PALETTES.light)

    expect(light[CODE_BG_VAR]).not.toBe('#1c2128')
  })

  it('亮色 code 文字在白底上够对比度', () => {
    const v = themeVars('light', PALETTES.light)
    const fg = String(v[CODE_FG_VAR])
    const bg = String(v[CODE_BG_VAR])

    // 相对亮度（WCAG 定义），够 4.5:1 即达标 —— 与 theme.ts 的
    // 对比度注释同一套口径。
    const lum = (hex: string) => {
      const n = hex.replace('#', '')
      const ch = [0, 2, 4].map((i) => {
        const v2 = parseInt(n.slice(i, i + 2), 16) / 255
        return v2 <= 0.03928 ? v2 / 12.92 : ((v2 + 0.055) / 1.055) ** 2.4
      })
      return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2]
    }
    const l1 = lum(fg)
    const l2 = lum(bg)
    const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05)

    expect(ratio).toBeGreaterThanOrEqual(4.5)
  })

  it('变量名以 --iip- 开头，不与 antd 变量空间撞名', () => {
    for (const name of [CODE_BG_VAR, CODE_FG_VAR]) {
      expect(name.startsWith('--iip-')).toBe(true)
    }
  })
})
