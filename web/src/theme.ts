// 深色专业控制台主题（ADR-016 裁定：深色专业控制台）。
//
// 为什么自己写而不用 antd 的 darkAlgorithm：
// antd 的默认暗色主题偏「中灰」，适合通用后台；而这是**值守用的控制台**——
// 工程师在夜间低照度环境下盯屏，背景越低对比越省眼，但**告警的严重度颜色
// 必须在暗底上依然一眼可辨**，这两件事对配色的要求正好相反。
// 所以这里只用 antd 的 token 结构（保证组件行为正确），
// 颜色全部自己定，关键色按暗底重新校准过亮度。

import { theme, type ThemeConfig } from 'antd'

// 底色阶：越靠前的层越亮（用于卡片浮起），越靠后越沉（用于页面底）。
// 这套「同色相、明度分层」的底色能让卡片边界靠**明度差**而非边框可见，
// 在暗底上比加灰边框更干净。
const surface = {
  base: '#0d1117', // 页面底（最沉）
  raised: '#161b22', // 卡片
  overlay: '#1c2128', // 弹层 / 下拉
  hover: '#21262d',
  border: '#30363d',
  borderStrong: '#3d444d',
}

// 文字阶：四级足够，从「标题」到「禁用」不该更多。
const text = {
  primary: '#e6edf3', // 正文与标题
  secondary: '#8b949e', // 次要说明
  tertiary: '#6e7681', // 占位、禁用
  inverse: '#0d1117', // 落在主色块上的文字
}

// 语义色：按暗底重新校准明度。
//
// 关键点：`critical` 用 #ff6b6b 而非纯红 —— 纯红 (#f5222d) 在 #0d1117 上
// 对比度不足 4.5:1，长时间看刺眼且读不清；'#f85149' (GitHub danger) 是
// 在暗底上验证过可读性的一档。
const semantic = {
  critical: '#f85149',
  warning: '#d29922',
  success: '#3fb950',
  info: '#58a6ff',
  neutral: '#6e7681',
  accent: '#316dca',
}

export const iipDarkTheme: ThemeConfig = {
  algorithm: theme.darkAlgorithm,
  token: {
    colorPrimary: semantic.accent,
    colorInfo: semantic.info,
    colorSuccess: semantic.success,
    colorWarning: semantic.warning,
    colorError: semantic.critical,

    colorBgBase: surface.base,
    colorBgContainer: surface.raised,
    colorBgElevated: surface.overlay,
    colorBgLayout: surface.base,

    colorText: text.primary,
    colorTextSecondary: text.secondary,
    colorTextTertiary: text.tertiary,
    colorTextQuaternary: text.tertiary,

    colorBorder: surface.border,
    colorBorderSecondary: surface.border,
    colorSplit: surface.border,

    // 圆角收窄到 6：控制台的信息密度高，大圆角会显得「玩具感」。
    borderRadius: 6,
    fontSize: 13,

    // 表格行高放宽一点：事件列表每行信息多，行距不足会挤成一团。
    controlHeight: 32,
    wireframe: false,
  },
  components: {
    Layout: {
      headerBg: surface.raised,
      siderBg: surface.base,
      bodyBg: surface.base,
      headerHeight: 52,
    },
    Menu: {
      darkItemBg: surface.base,
      darkSubMenuItemBg: surface.base,
      darkItemSelectedBg: '#1f6feb33',
      darkItemSelectedColor: text.primary,
      darkItemColor: text.secondary,
      itemHeight: 38,
    },
    Table: {
      headerBg: surface.raised,
      headerColor: text.secondary,
      rowHoverBg: surface.hover,
      // 行分隔线用极低对比：表格已经很密，重分隔线会把行切成碎片。
      borderColor: surface.border,
      cellPaddingBlockSM: 8,
    },
    Card: {
      colorBgContainer: surface.raised,
      headerBg: 'transparent',
    },
    Tabs: {
      itemColor: text.secondary,
      itemSelectedColor: text.primary,
      itemHoverColor: text.primary,
      inkBarColor: semantic.info,
    },
    Statistic: {
      contentFontSize: 22,
      titleFontSize: 12,
    },
    Tag: {
      defaultBg: surface.overlay,
    },
    Descriptions: {
      labelBg: surface.raised,
      titleMarginBottom: 12,
    },
    Timeline: {
      tailColor: surface.border,
    },
  },
}

// 暴露给非 antd 场景（如内联 style 的分隔线、图表底色）。
export const palette = { surface, text, semantic }