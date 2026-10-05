// 表格「整行可导航」的交互契约。
//
// **为什么要抽出来而不是写在 Incidents.tsx 的 onRow 里**：
// 键盘可达这件事极易退化 —— 改列、改样式、复制粘贴 onRow 时
// `tabIndex`/`onKeyDown` 都不会报错，只是**默默不再能键盘操作**。
// 抽成独立函数后可以脱离整页渲染直接断言，
// 退化会红测试而不是等到有人用键盘才发现。
//
// **这个函数只管「可导航」，不管「像不像可导航」**（配色、左侧强调条）：
// 后者是 index.css + 主题 token 的职责，两者通过 CSS 变量衔接。

import type { CSSProperties, KeyboardEvent } from 'react'

/**
 * ROW_ACCENT_VAR 是 hover 左侧强调条取色的 CSS 变量名。
 *
 * CSS 里不能写死颜色（index.css 文件头的纪律：颜色只许在 theme.ts 定），
 * 但 hover 态是纯 CSS 行为、又在 TSX 之外 —— 唯一的干净接法是
 * **由这里把颜色注入成变量，CSS 只读变量**。切主题时 palette 变，
 * 变量随之变，两套主题各自正确。
 */
export const ROW_ACCENT_VAR = '--iip-row-accent'

export interface ClickableRowOptions {
  /** 行点击后要导航到的路径。 */
  href: string
  /** 导航函数（react-router 的 navigate）。 */
  navigate: (to: string) => void
  /** 强调条颜色，取自当前调色板。调用方传 palette.semantic.info。 */
  accent: string
}

/**
 * clickableRowProps 返回 antd Table `onRow` 需要的行属性。
 *
 * **为什么是 Enter + Space 都响应**：原生 `<a>` 只认 Enter，
 * 但这里承载交互的是 `<tr>` 而非链接，用户按 Space 的肌肉记忆
 * 落空时会以为页面卡住。代价是必须 `preventDefault`——
 * 否则 Space 会滚动页面。
 */
export function clickableRowProps({
  href,
  navigate,
  accent,
}: ClickableRowOptions) {
  const go = () => navigate(href)

  const onKeyDown = (e: KeyboardEvent<HTMLTableRowElement>) => {
    if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
      // Space 的默认行为是滚动；Enter 在部分浏览器里也有表单提交语义。
      e.preventDefault()
      go()
    }
  }

  return {
    onClick: go,
    onKeyDown,
    // 0（而非 -1）：整行是主要导航入口，排除出焦点序列等于键盘用户过不去。
    tabIndex: 0,
    // role=link 而非 button：动作是「换个页面」，不是「提交/执行」。
    // 屏幕阅读器会因此把 Enter 提示为可预期，而不是要求用户去试。
    role: 'link',
    style: {
      cursor: 'pointer',
      [ROW_ACCENT_VAR]: accent,
    } as CSSProperties,
  }
}
