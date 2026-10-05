// URL 同步筛选：把filter 状态镜像到 query string。
//
// 为什么值得做（不只是「URL 好看」）：
//
//  1. **刷新不丢筛选**。值班的人看到「严重级 critical 的事件」，
//     按 F5 之后不该回到全量列表 —— 那意味着他得重新点一遍。
//  2. **可以把链接发给同事**。这是协作的前提：「你看看这几条」
//     说不清「哪几条」时，一个带筛选的链接就是完整的那句话。
//  3. **浏览器前进/后退可用**。这是 URL 作为状态载体的红利。
//
// ## 状态的方向：URL 是镜像，不是真源
//
// state 仍是唯一真源，每次变更后**由调用方主动调 sync 写一次 URL**。
// 反过来做（从URL 派生 state）有两个具体问题：
//
//   * 「首次进入无参数」与「用户清空筛选」在 URL 上都是空 query，
//     派生写法分不清二者；
//   * 每键一输入就要写历史记录 → 后退栈堆出一长串。
//
// 但**初值必须从 URL 读**（见 useFilterUrl 的 lazy initial），
// 否则「同事发来一个带筛选的链接」打开时会被立刻清空——
// 那等于白做第 2 条理由。
//
// ## 为什么是「主动 sync」而不是 useEffect 自动跟随
//
// 第一版把 state 变化放进 useEffect 里监听，结果**同步根本不发生**：
// effect 的依赖是 `initial`，而 initial 是 useState 存的对象、**永不变**，
// 于是 effect 只在挂载时跑一次，之后用户改筛选 URL 一动不动。
// 症状极具欺骗性：初次打开带筛选的链接是好的（初值确实读到了），
// 看起来像实现了；只有真去点一下筛选才发现URL 不动。
//
// 改成由调用方在「改 state 的那个函数里」顺手调 sync(next)——
// 这样「改 state」与「写 URL」在**同一个函数**里，不可能只做一半。
//
// ## 为什么用 replace 而不是 push
//
// 筛选变更逐条 push 会让后退键需要按十几次才能离开本页
// （改一次筛选就得按一次后退才能退出列表）。
// 那不是「历史」，是噪音。

import { useCallback, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import type { IncidentStatus } from '../api/incidents'

/** 状态值的分隔符。刻意不用逗号 —— 状态名里不含逗号，但用 | 更显眼。 */
const STATUS_SEP = '|'

/**
 * VALID_STATUS 是允许的状态全集。
 *
 * **必须与后端认的状态集合一致**，否则会出现
 * 「下拉框里选不到但 URL 里能填进来」的漏洞。
 * 这里显式列出而不 import ALL_STATUSES，是因为后者是
 * 「给用户选的子集」（可能只列常用几个），而这里要的是
 * 「后端认的全集」—— 两者语义不同。
 */
const VALID_STATUS = new Set<string>([
  'open', 'merged', 'resolved', 'ignored', 'suppressed',
])

/**
 * parseStatus 从 query 读回状态数组。
 *
 * **不认识的值直接丢弃**而不是原样传给后端：URL 是用户可编辑的，
 * 里面出现 `status=drop%20table` 时，正确的反应是「当成没填」，
 * 而不是把未知值发出去换一个 422。
 */
export function parseStatus(raw: string | null): IncidentStatus[] {
  if (!raw) return []
  return raw
    .split(STATUS_SEP)
    .map((s) => s.trim())
    .filter((s) => VALID_STATUS.has(s)) as IncidentStatus[]
}

/**
 * parseSeverity 从 query 读回级别数组。同样**丢弃非法值**（理由同 parseStatus）。
 */
export function parseSeverity(raw: string | null): string[] {
  if (!raw) return []
  return raw
    .split(STATUS_SEP)
    .map((s) => s.trim())
    .filter((s) => s === 'critical' || s === 'warning' || s === 'info')
}

/**
 * serializeStatus 把数组写回 query。
 *
 * 空数组给出 '' 而不是 STATUS_SEP —— 否则 URL 里会出现 `status=|`
 * 这种看着像 bug 的尾巴。
 *
 * **保序不排序**：排序会让「我刚点的顺序」丢失，
 * 而那正是用户心里筛选器的排列顺序。
 */
export function serializeStatus(list: readonly string[]): string {
  return list.join(STATUS_SEP)
}

export interface FilterState {
  status: IncidentStatus[]
  severity: string[]
  service: string
}

/**
 * useFilterUrl 返回两样东西：
 *
 *   * `initial` —— **只在挂载时**从 URL 读到的初值，用作 useState 的初值。
 *   * `sync(next)` —— **主动**把当前筛选写回 URL。
 *
 * 调用方必须在「改 state 的那个函数里」同时调 sync，
 * 见模块头「为什么是主动 sync」。
 */
export function useFilterUrl() {
  const [params, setParams] = useSearchParams()

  // 首次挂载时从 URL 取初值。**只在挂载时读一次** ——
  // 若每次渲染都重读，用户改筛选时 state 与 URL 会互相覆盖成死循环。
  const [initial] = useState<FilterState>(() => ({
    status: parseStatus(params.get('status')),
    severity: parseSeverity(params.get('severity')),
    service: params.get('service') ?? '',
  }))

  const sync = useCallback(
    (next: FilterState) => {
      setParams(
        (prev) => {
          const p = new URLSearchParams(prev)
          applyOrDelete(p, 'status', serializeStatus(next.status))
          applyOrDelete(p, 'severity', serializeStatus(next.severity))
          applyOrDelete(p, 'service', next.service)
          // 三个都清掉时把整串 query 去掉，不留 `?` 尾巴。
          return p.toString() ? p : new URLSearchParams()
        },
        // **replace 而不是 push**：见模块头「为什么用 replace」。
        { replace: true },
      )
    },
    [setParams],
  )

  return { initial, sync }
}

/**
 * applyOrDelete 设值或删键。
 *
 * 空字符串 / null 一律删键而不是设成空值 —— `?service=` 这种形态
 * 会被后端解析成「service 是空串」，而我们的语义是「不过滤」。
 * 两者不等价，差一个= 就会让列表返回全量。
 */
function applyOrDelete(p: URLSearchParams, key: string, value: string | null | undefined) {
  if (value) p.set(key, value)
  else p.delete(key)
}