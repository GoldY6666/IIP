// 主题切换的测试（ADR-021 / 手册 §4.5.4 的四条必验判据）。
//
// **文件名必须是 .tsx**：本文件里有 JSX（测试用组件），esbuild 不会在
// .ts 里解析 JSX，报的错是 'Expected ">" but found "/"' ——
// 那条报错完全看不出是文件后缀的问题。
//
// **本文件测的是「机制」而不是「好看」**：
//   - 机制可测：默认模式、localStorage 记忆、内联 palette 是否真的跟着变、对比度是否达标。
//   - 好看不可测：那要靠截图人工看，jsdom 读不到计算后的颜色。
//     **因此「亮色下白底白字」这类缺陷单测抓不到**（见本文件末尾的说明）。
//
// 关键判据是第 3 条：**内联 style 里的 `palette.text.primary` 必须跟着变**。
// 只测 antd token 的话，17 处内联色全都不动而测试照样绿 ——
// 那正是本次需求唯一的真风险。

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ConfigProvider } from 'antd'
import { ThemeModeProvider, palette, useThemeMode } from './themeMode'
import {
  DEFAULT_MODE,
  PALETTES,
  THEMES,
  THEME_STORAGE_KEY,
  darkPalette,
  iipDarkTheme,
  iipLightTheme,
  lightPalette,
  loadMode,
  otherMode,
  saveMode,
  type ThemeMode,
} from './theme'

// --- WCAG 2.1 对比度计算（与 D:\_iip_setup\_contrast.py 同算法）---

function srgbToLin(c: number): number {
  const v = c / 255
  return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
}

function luminance(hex: string): number {
  const h = hex.replace('#', '')
  const r = parseInt(h.slice(0, 2), 16)
  const g = parseInt(h.slice(2, 4), 16)
  const b = parseInt(h.slice(4, 6), 16)
  return 0.2126 * srgbToLin(r) + 0.7152 * srgbToLin(g) + 0.0722 * srgbToLin(b)
}

function contrast(fg: string, bg: string): number {
  const a = luminance(fg)
  const b = luminance(bg)
  const hi = Math.max(a, b)
  const lo = Math.min(a, b)
  return (hi + 0.05) / (lo + 0.05)
}

beforeEach(() => {
  window.localStorage.clear()
})

describe('默认主题（ADR-021 裁定：暗色）', () => {
  it('DEFAULT_MODE 是 dark', () => {
    expect(DEFAULT_MODE).toBe('dark')
  })

  it('localStorage 为空时读到 dark', () => {
    expect(loadMode()).toBe('dark')
  })

  it('首次渲染用的就是暗色（不是先暗后切）', () => {
    render(
      <ThemeModeProvider>
        <Probe />
      </ThemeModeProvider>,
    )
    // 判据是「第一次 render 就是 dark」——
    // 若实现改成 useEffect 里读，首帧会是 undefined/light 而这里就红。
    expect(screen.getByTestId('mode').textContent).toBe('dark')
    expect(palette).toBe(darkPalette)
  })
})

describe('localStorage 记忆', () => {
  it('写入的键是 iip.theme，值是模式字符串', () => {
    saveMode('light')
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('存过的 light 会被读回来', () => {
    saveMode('light')
    expect(loadMode()).toBe('light')
  })

  it('**垃圾值回落到默认而不是被当模式**', () => {
    // 这一条防的是「有人往 localStorage 手塞 'DARK'（大写）或 'auto'」，
    // 那会让主题落到一个不存在的分支，页面白屏或配色崩。
    window.localStorage.setItem(THEME_STORAGE_KEY, 'DARK')
    expect(loadMode()).toBe('dark')
    window.localStorage.setItem(THEME_STORAGE_KEY, 'auto')
    expect(loadMode()).toBe('dark')
  })

  it('存储不可用时退化到默认而不是抛错', () => {
    // 无痕模式下 localStorage.getItem 抛 SecurityError。
    // 一个「主题偏好读不到」导致整页白屏是不可接受的失败模式。
    const spy = vi.spyOn(window.localStorage, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError: storage disabled')
    })
    expect(loadMode()).toBe('dark')
    // saveMode 同样要容错，否则点一下切换按钮就崩
    expect(() => saveMode('light')).not.toThrow()
    spy.mockRestore()
  })
})

describe('切换', () => {
  it('otherMode 在两种模式间往返', () => {
    expect(otherMode('dark')).toBe('light')
    expect(otherMode('light')).toBe('dark')
  })

  it('点按钮切到 light，palette 活引用同步换掉', async () => {
    const user = userEvent.setup()
    render(
      <ThemeModeProvider>
        <ToggleProbe />
      </ThemeModeProvider>,
    )
    expect(palette).toBe(darkPalette)

    await user.click(screen.getByRole('button', { name: 'toggle' }))

    expect(screen.getByTestId('mode').textContent).toBe('light')
    // **本文件最关键的一条断言**：活引用换成了亮色调色板。
    // 若这里没变，17 处内联色就还是暗色的，亮色下白底白字。
    expect(palette).toBe(lightPalette)
  })

  it('切换会写 localStorage（刷新后仍是新模式）', async () => {
    const user = userEvent.setup()
    render(
      <ThemeModeProvider>
        <ToggleProbe />
      </ThemeModeProvider>,
    )
    await user.click(screen.getByRole('button', { name: 'toggle' }))
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('连续两次切换回到 dark', async () => {
    const user = userEvent.setup()
    render(
      <ThemeModeProvider>
        <ToggleProbe />
      </ThemeModeProvider>,
    )
    await user.click(screen.getByRole('button', { name: 'toggle' }))
    await user.click(screen.getByRole('button', { name: 'toggle' }))
    expect(screen.getByTestId('mode').textContent).toBe('dark')
    expect(palette).toBe(darkPalette)
  })

  it('两种模式都提供 ConfigProvider（亮色用 defaultAlgorithm）', () => {
    // 断言 algorithm 不同：若两份配置都写 darkAlgorithm，
    // antd 侧的组件会仍然是暗色，而 palette 已经是亮色 —— 半个亮半个暗。
    expect(iipDarkTheme.algorithm).toBeDefined()
    expect(iipLightTheme.algorithm).toBeDefined()
    expect(THEMES.dark.algorithm).not.toBe(THEMES.light.algorithm)
  })
})

describe('内联色是否真的跟着变（本次需求的真风险）', () => {
  it('palette 的三个分组在两种模式下**不是同一对象**', () => {
    expect(PALETTES.dark).not.toBe(PALETTES.light)
    expect(PALETTES.dark.surface).not.toBe(PALETTES.light.surface)
    expect(PALETTES.dark.text).not.toBe(PALETTES.light.text)
    expect(PALETTES.dark.semantic).not.toBe(PALETTES.light.semantic)
  })

  it('17 处引用到的每个键在两套里都有定义（缺一个就是 undefined 内联色）', () => {
    // 实测那 17 处用到的全部键。**这个清单是从代码里 grep 出来的**，
    // 少一个键的表现是内联 style 拿到 undefined，React 会忽略该属性，
    // 于是「文字颜色没设置」→ 继承父级 → 亮色下变黑（不是变白，仍是缺陷）。
    const used = [
      'surface.border',
      'semantic.info',
      'semantic.warning',
      'semantic.critical',
      'text.primary',
      'text.secondary',
      'text.tertiary',
    ] as const
    for (const key of used) {
      const [g, k] = key.split('.') as ['surface' | 'text' | 'semantic', string]
      for (const mode of ['dark', 'light'] as ThemeMode[]) {
        const v = (PALETTES[mode][g] as Record<string, string>)[k]
        expect(v, `${mode}.${key} 未定义`).toBeTruthy()
        expect(v, `${mode}.${key} 不是合法颜色`).toMatch(/^#[0-9a-f]{6}$/i)
      }
    }
  })

  it('亮色下 text.primary 与页面底对比度 >= 4.5:1（否则白底浅字读不清）', () => {
    const c = contrast(lightPalette.text.primary, lightPalette.surface.base)
    expect(c).toBeGreaterThanOrEqual(4.5)
  })
})

describe('亮色语义色按白底重新校准（ADR-021 的硬要求）', () => {
  // 这组断言的由来：**把暗底值直接搬到白底会全部不达标**（实测
  // critical 3.35 / warning 2.52 / success 2.54 / info 2.53）。
  // 所以亮色那套必须是重新压深的，而不是照抄。
  const cases: [keyof typeof lightPalette.semantic, string][] = [
    ['critical', '严重'],
    ['warning', '警告'],
    ['success', '恢复'],
    ['info', '信息'],
    ['neutral', '中性'],
  ]

  for (const [key, label] of cases) {
    it(`${label}（${key}）在白底上 >= 4.5:1`, () => {
      const c = contrast(lightPalette.semantic[key], lightPalette.surface.base)
      expect(c, `${key} 对白底只有 ${c.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5)
    })
  }

  it('亮色文字三级都达 4.5:1（对卡片底）', () => {
    for (const k of ['primary', 'secondary', 'tertiary'] as const) {
      const c = contrast(lightPalette.text[k], lightPalette.surface.raised)
      expect(c, `light.text.${k} 只有 ${c.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5)
    }
  })

  it('亮色按钮底上的白字达 4.5:1（accent 作底色的场景）', () => {
    const c = contrast('#ffffff', lightPalette.semantic.accent)
    expect(c).toBeGreaterThanOrEqual(4.5)
  })

  it('**暗底那套不因本次改动被改坏**（回归保护）', () => {
    // 本次只加亮色，但顺手确认暗色四个语义色仍达标 ——
    // 若有人「顺手把暗色也压深了」，这里是唯一会红的地方。
    for (const k of ['critical', 'warning', 'success', 'info'] as const) {
      const c = contrast(darkPalette.semantic[k], darkPalette.surface.base)
      expect(c, `dark.semantic.${k} 只有 ${c.toFixed(2)}:1`).toBeGreaterThanOrEqual(4.5)
    }
  })
})

describe('Provider 缺失要显式报错', () => {
  it('缺 ThemeModeProvider 时 useThemeMode 抛错', () => {
    // 静默返回默认值会让「忘加 Provider」变成一个
    // 「主题永远切不了、但没有任何提示」的问题。
    expect(() => render(<Probe />)).toThrow(/ThemeModeProvider/)
  })
})

// --- 测试用组件 ---

function Probe() {
  const { mode } = useThemeMode()
  return <div data-testid="mode">{mode}</div>
}

function ToggleProbe() {
  const { mode, toggle } = useThemeMode()
  return (
    <ConfigProvider theme={THEMES[mode]}>
      <button type="button" onClick={toggle}>
        toggle
      </button>
      <div data-testid="mode">{mode}</div>
    </ConfigProvider>
  )
}
