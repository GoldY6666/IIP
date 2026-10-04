// 主题模式的运行时状态（ADR-021）。
//
// **要解决的问题**：`web/src` 下有 17 处 `palette.text.primary` 这类
// **内联 style 里的字面颜色引用**（Layout 6 / Settings 6 / WeightRow 2 /
// Reviews 2 / Incidents 1）。`ConfigProvider` 换 `theme` **不会**让它们变——
// 只改 theme.ts 的后果是「切到亮色后卡片变白、但文字仍是暗色 #e6edf3」，
// 即亮色主题下白底白字。
//
// **采用的做法**（手册 §4.5.2 方案 B）：让 `palette` 变成**随当前模式
// 重建的活引用**。切主题时整体替换它的三个字段，既有 17 处调用点
// **一行都不用改**，下一次渲染读到的就是新值。
//
// **为什么不逐处改 `useToken()`**（方案 A）：要动 17 处，且 `useToken`
// 只能在组件内调用 —— 而 `palette` 在本项目里正是被用在组件内的内联 style，
// 逐处改会让「颜色定义」从一处散到 17 处，将来加第三套主题要改 17 次。
//
// **为什么不导出可变对象**（反模式但确实是这里的技术要求）：
// 关键在于**替换发生在 Provider 的 render 里**，而 React 只在
// 「状态变化导致重渲染」时才重新读这些值。Provider 切换 mode 时
// 整棵子树重渲染，各组件在 render 中读到的已经是新值 ——
// **不需要** `palette` 本身是响应式的。

import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import {
  DEFAULT_MODE,
  PALETTES,
  THEMES,
  loadMode,
  otherMode,
  saveMode,
  type Palette,
  type ThemeMode,
} from './theme'

/**
 * palette 是「当前模式」的活引用。
 *
 * **必须用 `let` 而非 `const` + 整体替换**：既有代码写的是
 * `palette.text.primary`，若 `palette` 是 const 对象、只在 Provider 里
 * `Object.assign` 它的字段，行为上也能对，但**类型会被推成 readonly**，
 * 将来有人 `const p = palette` 存起来就再也不会跟着变了 ——
 * 那种 bug 极难查（变量还在、值是旧的）。`let` + 显式重新赋值
 * 至少让「这里有个会变的对象」写在代码里。
 */
export let palette: Palette = PALETTES[DEFAULT_MODE]

/** ThemeModeContext 只提供「切换」能力；模式本身不进 context（见下方理由）。 */
interface ThemeModeCtx {
  mode: ThemeMode
  toggle: () => void
}

const Ctx = createContext<ThemeModeCtx | null>(null)

/** useThemeMode 返回当前模式与切换函数。 */
export function useThemeMode(): ThemeModeCtx {
  const v = useContext(Ctx)
  if (!v) {
    // 缺 Provider 是编程错误（漏了 main.tsx 的包裹），不是运行期状态 ——
    // 抛错比返回默认值更容易定位。
    throw new Error('useThemeMode 必须在 ThemeModeProvider 内使用')
  }
  return v
}

export function ThemeModeProvider({ children }: { children: ReactNode }) {
  // **初始值同步读 localStorage，不用 useEffect** ——
  // 放 useEffect 里会让首帧先用暗色渲染、挂载后再切亮色，
  // 白闪一下。主题是「首屏就要对」的东西，必须在第一次 render 前定下来。
  const [mode, setMode] = useState<ThemeMode>(() => loadMode())

  // 同步活引用：**render 期间**改，而不是 useEffect ——
  // 改在 effect 里会晚一拍：子树已经用旧 palette 渲染完了，
  // 要等下一次状态变化才会看到新颜色（表现为「切了没立刻变，再点一下才对」）。
  const current = PALETTES[mode]
  palette = current

  const toggle = useCallback(() => {
    setMode((m) => {
      const next = otherMode(m)
      saveMode(next)
      return next
    })
  }, [])

  // ConfigProvider 的 theme 传对象引用：mode 变 → 引用变 → antd 重算 token。
  const value = useMemo<ThemeModeCtx>(() => ({ mode, toggle }), [mode, toggle])

  return (
    <Ctx.Provider value={value}>
      {/* locale 挂在这里而非 main.tsx：主题与语言是同一层「全局外观」设置，
          放一起后 main.tsx 不再需要知道 antd 的任何细节。 */}
      <ConfigProvider locale={zhCN} theme={THEMES[mode]}>
        {children}
      </ConfigProvider>
    </Ctx.Provider>
  )
}
