// 调色板→ CSS 变量的映射。
//
// **存在理由**：index.css 里有若干 antd token 覆盖不到的规则（行 hover
// 强调条、表格内代码块底色）。这些规则只能用 CSS 写，
// 但那个文件顶部的纪律是「**不写死颜色常量**」—— 否则切亮色时
// 这些规则会留在暗色值上。
//
// **实际踩到过**：表格内 code 的底色写死 #1c2128（暗色值），
// 切到亮色后事件 ID 变成一个个**黑块**，底色比卡片还深。
// 截图才看得出来 —— antd token 全对、tsc 全绿、组件测试全绿。
//
// **为什么挂在 Layout 根节点而不是全局注入**：Layout 是所有页面的
// 共同祖先，变量在这里定义必然覆盖到每一页；若改成给 documentElement
// 设值，就得处理「谁先渲染」与「测试环境下没有 Layout」两件事，收益为零。
//
// **命名**：一律 `--iip-*`，避免与 antd 自身的变量空间撞名。

import type { CSSProperties } from 'react'
import { type Palette, type ThemeMode } from '../theme'

/** CODE_BG_VAR / CODE_FG_VAR 是表格内 code 的底色与文字色变量名。 */
export const CODE_BG_VAR = '--iip-code-bg'
export const CODE_FG_VAR = '--iip-code-fg'

/**
 * 亮色下的 code 文字色。
 *
 * **不能用 lightSemantic.info（#0071f3）**：它在 code 底色
 * p.surface.hover（#eaeef2）上只有 **3.87:1**，达不到 WCAG AA 的 4.5:1。
 * #0071f3 是 GitHub 的按钮蓝、专为白底设计，而这里的底色是浅灰。
 *
 * 实测（对 #eaeef2 / #f6f8fa / #ffffff）：
 *   #0071f3 3.87 / 4.24 / 4.51  ← 只在纯白上勉强够
 *   #0060c0 5.24 / 5.74 / 6.11  ← 三种底色全过
 * 取 #0060c0。**这不是「随便挑个更深的蓝」**，而是按对比度算出来的。
 */
const LIGHT_CODE_FG = '#0060c0'

/** 暗色下的 code 文字色。取自 GitHub 暗色语法高亮的字符串蓝。 */
const DARK_CODE_FG = '#79c0ff'

/** 暗色下的 code 底色（比卡片 #161b22 深一档，靠明度差看出是块代码）。 */
const DARK_CODE_BG = '#1c2128'

/**
 * ThemeVars 是「CSS 变量集合」的类型。
 *
 * **为什么要专门定义而不是直接返回 CSSProperties**：`CSSProperties`
 * 的键是 React 认识的那些属性名，用 `v['--iip-code-bg']` 索引它会报
 * TS7053（自定义属性不在该类型里），而 React **运行时完全支持**
 * 自定义属性 —— 纯粹是类型没跟上。所以这里把
 * 「`--iip-*` 键 + 字符串值」显式并进 CSSProperties：
 * 既能直接进 style，又能按下标取值（测试需要）。
 */
export type ThemeVars = CSSProperties & Record<`--iip-${string}`, string>

/**
 * themeVars 把当前调色板翻译成 CSS 变量。
 *
 * **在 render 期间调用**（返回对象直接进 style），与 themeMode.tsx
 * 「同步活引用」的同一套时序：切主题时子树重渲染，这里读到的是新调色板。
 *
 * **参数是 mode 而不是靠 hex 反推**：从 `surface.base === '#ffffff'`
 * 反推主题看起来可行，实际是把「配色值」当成「主题标识」——
 * 将来加第三套配色时这个判断会静默失配，且不会编译报错。
 */
export function themeVars(mode: ThemeMode, p: Palette): ThemeVars {
  return {
    // 亮色下用浅灰而不是白：白底白字会糊，
    // 且行 hover 变深时浅灰块才看得出这是段代码。
    [CODE_BG_VAR]: mode === 'light' ? p.surface.hover : DARK_CODE_BG,
    // 代码是标识符 / 指纹，用蓝系才与普通正文区分得开。
    // **注意亮色用的不是 p.semantic.info** —— 对比度不够，见 LIGHT_CODE_FG。
    [CODE_FG_VAR]: mode === 'light' ? LIGHT_CODE_FG : DARK_CODE_FG,
  }
}
