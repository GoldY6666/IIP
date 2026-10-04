// IIP 控制台主题（暗色默认 + 亮色可选，ADR-021）。
//
// 暗色那套是 M3 定下的「行业形态」（ADR-016）—— 底色按同色相明度分层、
// 语义色按暗底重新校准。亮色是 ADR-021 新增的 GitHub Light 同源配色。
//
// **两套配色都过对比度，亮色不是把暗色值搬过来**：
// 实测把暗底语义色搬到白底后 critical 3.35:1、warning 2.52:1、
// success 2.54:1、info 2.53:1，**全部不达 WCAG AA 的 4.5:1**。
// 亮色那套是同色相（HSV 只降 L、保持 H/S）重新压深得来的，逐个值见注释。

import { theme as antdTheme, type ThemeConfig } from 'antd'

/** 主题模式。存 localStorage 的就是这个字符串。 */
export type ThemeMode = 'dark' | 'light'

/** THEME_STORAGE_KEY 是 localStorage 的键。带 `iip.` 前缀避免与同域其它应用撞。 */
export const THEME_STORAGE_KEY = 'iip.theme'

// --- 暗色（默认，M3 起沿用）---
//
// 底色阶：越靠前的层越亮（卡片浮起），越靠后越沉（页面底）。
// 这套「同色相、明度分层」让卡片边界靠**明度差**而非边框可见，
// 在暗底上比加灰边框更干净。
const darkSurface = {
  base: '#0d1117', // 页面底（最沉）
  raised: '#161b22', // 卡片
  overlay: '#1c2128', // 弹层 / 下拉
  hover: '#21262d',
  border: '#30363d',
  borderStrong: '#3d444d',
}

// 文字阶：四级足够，从「标题」到「禁用」不该更多。
//
// 实测对比度（对 #161b22 卡片底）：primary 14.64:1、secondary 5.62:1、
// tertiary 3.77:1。**tertiary 低于 4.5:1** —— 它只用于「占位/禁用」
// 这类非必读信息（WCAG 对禁用态本就豁免），故保持 M3 原值不动。
const darkText = {
  primary: '#e6edf3', // 14.64:1
  secondary: '#8b949e', // 5.62:1
  tertiary: '#6e7681', // 3.77:1 —— 仅占位/禁用
  inverse: '#0d1117', // 落在主色块上的文字
}

// 语义色：按暗底重新校准明度。
//
// 关键点：`critical` 用 #f85149 而非纯红 —— 纯红在 #0d1117 上对比度不足，
// 长时间看刺眼且读不清；#f85149 (GitHub danger) 是在暗底上验证过的一档。
//
// 实测（对 #0d1117 页面底）：critical 5.65:1、warning 7.50:1、
// success 7.45:1、info 7.49:1，均达 4.5:1。
const darkSemantic = {
  critical: '#f85149',
  warning: '#d29922',
  success: '#3fb950',
  info: '#58a6ff',
  neutral: '#6e7681',
  accent: '#316dca',
}

// --- 亮色（ADR-021 新增，GitHub Light 同源）---
//
// 底色关系与暗色那套是**同一思路的两面**：亮色下页面底是**白**、
// 卡片是**浅灰** —— 卡片靠「比页面底深一点」浮起，而不是靠阴影浮起。
// 侧栏则是白底 + 边框，与页面的白底靠一条细线分开。
const lightSurface = {
  base: '#ffffff', // 页面底
  raised: '#f6f8fa', // 卡片（比页面底略沉，靠明度差区分）
  overlay: '#ffffff', // 弹层 / 下拉 —— 亮色下用白，靠阴影分层
  hover: '#eaeef2',
  // 1.45:1 对白底。**只做辅助边界**：卡片与页面的区分靠明度差，
  // 边框不承担「看得见卡片边界」的职责，故低于 3:1 可接受。
  border: '#d0d7de',
  borderStrong: '#afb8c1',
}

const lightText = {
  primary: '#1f2328', // 14.84:1
  secondary: '#59636e', // 5.74:1
  // GitHub 自己的 #6e7781 对 #f6f8fa 只有 4.27:1，差 0.23。
  // 压深到 #6b737d 达 4.51:1。差这一点点不算功能缺陷，
  // 但控制台是长时间盯屏的地方，按「能达标就达标」处理。
  tertiary: '#6b737d', // 4.51:1
  inverse: '#ffffff', // 落在主色块上的文字
}

// 语义色：**同色相重新压深**，不是搬暗色那套。
// 实测（对白底；括号内是暗底原值直接搬过去的对比度）：
//   critical 3.35:1 → 4.50:1
//   warning  2.52:1 → 4.52:1
//   success  2.54:1 → 4.52:1
//   info     2.53:1 → 4.51:1
//   neutral  4.59:1 → 4.59:1（已达标，不动）
const lightSemantic = {
  critical: '#ec1309',
  warning: '#996f19',
  success: '#2e873b',
  info: '#0071f3',
  neutral: '#6e7681',
  // Accent 在亮色下**仍作按钮底色**，白字压在上面：
  // #ffffff on #316dca = 5.03:1，达标，故不压深。
  accent: '#316dca',
}

/** Palette 是「某套主题」的调色板形状。 */
export interface Palette {
  surface: typeof darkSurface
  text: typeof darkText
  semantic: typeof darkSemantic
}

export const darkPalette: Palette = {
  surface: darkSurface,
  text: darkText,
  semantic: darkSemantic,
}

export const lightPalette: Palette = {
  surface: lightSurface,
  text: lightText,
  semantic: lightSemantic,
}

/**
 * PALETTES 是「模式 → 调色板」的唯一真源。
 *
 * 导出名是 `PALETTES`（大写）而**不是**原来的 `palette` ——
 * 因为 `palette` 现在是「随当前模式变化的活引用」（见 `themeMode.tsx`），
 * 名字大小写区分「静态表」与「活引用」两个东西，避免误用。
 * 既有 17 处 `palette.*` 内联引用因此**一行都不用改**。
 */
export const PALETTES: Record<ThemeMode, Palette> = {
  dark: darkPalette,
  light: lightPalette,
}

// --- antd ThemeConfig ---

export const iipDarkTheme: ThemeConfig = {
  algorithm: antdTheme.darkAlgorithm,
  token: {
    colorPrimary: darkSemantic.accent,
    colorInfo: darkSemantic.info,
    colorSuccess: darkSemantic.success,
    colorWarning: darkSemantic.warning,
    colorError: darkSemantic.critical,

    colorBgBase: darkSurface.base,
    colorBgContainer: darkSurface.raised,
    colorBgElevated: darkSurface.overlay,
    colorBgLayout: darkSurface.base,

    colorText: darkText.primary,
    colorTextSecondary: darkText.secondary,
    colorTextTertiary: darkText.tertiary,
    colorTextQuaternary: darkText.tertiary,

    colorBorder: darkSurface.border,
    colorBorderSecondary: darkSurface.border,
    colorSplit: darkSurface.border,

    // 圆角收窄到 6：控制台的信息密度高，大圆角会显得「玩具感」。
    borderRadius: 6,
    fontSize: 13,

    // 表格行高放宽一点：事件列表每行信息多，行距不足会挤成一团。
    controlHeight: 32,
    wireframe: false,
  },
  components: {
    Layout: {
      headerBg: darkSurface.raised,
      siderBg: darkSurface.base,
      bodyBg: darkSurface.base,
      headerHeight: 52,
    },
    Menu: {
      // 侧栏在暗色下是页面底色，选中项用「蓝 + 半透明」而不是实心块 ——
      // 实心块在整片暗底上会显得像按钮而不是「当前位置」。
      darkItemBg: darkSurface.base,
      darkSubMenuItemBg: darkSurface.base,
      darkItemSelectedBg: '#1f6feb33',
      darkItemSelectedColor: darkText.primary,
      darkItemColor: darkText.secondary,
      itemHeight: 38,
    },
    Table: {
      headerBg: darkSurface.raised,
      headerColor: darkText.secondary,
      rowHoverBg: darkSurface.hover,
      // 行分隔线用极低对比：表格已经很密，重分隔线会把行切成碎片。
      borderColor: darkSurface.border,
      cellPaddingBlockSM: 8,
    },
    Card: {
      colorBgContainer: darkSurface.raised,
      headerBg: 'transparent',
    },
    Tabs: {
      itemColor: darkText.secondary,
      itemSelectedColor: darkText.primary,
      itemHoverColor: darkText.primary,
      inkBarColor: darkSemantic.info,
    },
    Statistic: {
      contentFontSize: 22,
      titleFontSize: 12,
    },
    Tag: {
      defaultBg: darkSurface.overlay,
    },
    Descriptions: {
      labelBg: darkSurface.raised,
      titleMarginBottom: 12,
    },
    Timeline: {
      tailColor: darkSurface.border,
    },
  },
}

export const iipLightTheme: ThemeConfig = {
  algorithm: antdTheme.defaultAlgorithm,
  token: {
    colorPrimary: lightSemantic.accent,
    colorInfo: lightSemantic.info,
    colorSuccess: lightSemantic.success,
    colorWarning: lightSemantic.warning,
    colorError: lightSemantic.critical,

    colorBgBase: lightSurface.base,
    colorBgContainer: lightSurface.raised,
    colorBgElevated: lightSurface.overlay,
    colorBgLayout: lightSurface.base,

    colorText: lightText.primary,
    colorTextSecondary: lightText.secondary,
    colorTextTertiary: lightText.tertiary,
    colorTextQuaternary: lightText.tertiary,

    colorBorder: lightSurface.border,
    colorBorderSecondary: lightSurface.border,
    colorSplit: lightSurface.border,

    borderRadius: 6,
    fontSize: 13,
    controlHeight: 32,
    wireframe: false,
  },
  components: {
    Layout: {
      headerBg: lightSurface.raised,
      siderBg: lightSurface.base,
      bodyBg: lightSurface.base,
      headerHeight: 52,
    },
    Menu: {
      // 亮色下侧栏用亮色系菜单：antd 会跟随 algorithm 自动选，
      // 这里只调选中态与文字色。
      itemSelectedBg: '#0969da1a',
      itemSelectedColor: '#0969da',
      itemColor: lightText.secondary,
      itemHeight: 38,
    },
    Table: {
      headerBg: lightSurface.raised,
      headerColor: lightText.secondary,
      rowHoverBg: lightSurface.hover,
      borderColor: lightSurface.border,
      cellPaddingBlockSM: 8,
    },
    Card: {
      colorBgContainer: lightSurface.raised,
      headerBg: 'transparent',
    },
    Tabs: {
      itemColor: lightText.secondary,
      itemSelectedColor: lightText.primary,
      itemHoverColor: lightText.primary,
      inkBarColor: lightSemantic.info,
    },
    Statistic: {
      contentFontSize: 22,
      titleFontSize: 12,
    },
    Tag: {
      defaultBg: lightSurface.hover,
    },
    Descriptions: {
      labelBg: lightSurface.raised,
      titleMarginBottom: 12,
    },
    Timeline: {
      tailColor: lightSurface.border,
    },
  },
}

/** THEMES 是「模式 → antd 主题配置」。 */
export const THEMES: Record<ThemeMode, ThemeConfig> = {
  dark: iipDarkTheme,
  light: iipLightTheme,
}

/** DEFAULT_MODE 是首次访问（localStorage 无记录）时的模式（ADR-021 裁定：暗色）。 */
export const DEFAULT_MODE: ThemeMode = 'dark'

/**
 * loadMode 从 localStorage 读上次选择。
 *
 * **容错而不是抛错**：localStorage 在无痕 / 隐私模式下会抛
 * `SecurityError`，某些禁用存储的环境里 `window.localStorage` 本身是
 * `undefined`。一个「主题偏好读不到」就让整个应用白屏是不可接受的
 * 失败模式 —— 退化到默认即可。
 */
export function loadMode(): ThemeMode {
  try {
    const v = window.localStorage.getItem(THEME_STORAGE_KEY)
    return v === 'light' || v === 'dark' ? v : DEFAULT_MODE
  } catch {
    return DEFAULT_MODE
  }
}

/** saveMode 写入 localStorage；写不进去只吞异常（同上，不能因此崩）。 */
export function saveMode(mode: ThemeMode): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, mode)
  } catch {
    /* 存储不可用：本次会话仍能切换，只是不留到下次 */
  }
}

/** otherMode 返回另一个模式，供「切换」按钮直接用（免得按钮里写三元）。 */
export function otherMode(mode: ThemeMode): ThemeMode {
  return mode === 'dark' ? 'light' : 'dark'
}
