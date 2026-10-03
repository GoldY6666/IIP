// 事件状态机的前端同构实现。
//
// 规格来源：internal/incident/status.go（后端唯一真源）与
// docs/IIP-M3详细开发手册.md §4.1。**判定口径必须与后端逐条一致** ——
// 前端算错的后果是「按钮显示可点、后端回 422」，用户白等一次往返。
//
// 手册 §4.8 要求本文件有单测覆盖边界；前端测试延到 M5（ADR-016），
// 届时 vitest 落地后应把 status_test.go 的 36 格矩阵照搬一份。
// 在那之前，下面 canTransition 的分叉顺序刻意与后端保持一致，
// 便于逐行对照人工复核。

import type { IncidentStatus } from './incidents'

// LADDER 是状态的阶序，下标越大越靠后。后端 ladder 把 ignored 放在末端
// 不是随意的：这样「任意非终态 → ignored」无需单列规则即天然成立。
// **顺序不可改** —— 前端据此算「可前进的目标」。
export const LADDER: IncidentStatus[] = [
  'open',
  'acknowledged',
  'investigating',
  'mitigated',
  'resolved',
  'ignored',
]

export function indexOf(s: IncidentStatus): number {
  return LADDER.indexOf(s)
}

export function isValid(s: string): s is IncidentStatus {
  return indexOf(s as IncidentStatus) >= 0
}

/** isTerminal 报告是否为终态（resolved / ignored）。 */
export function isTerminal(s: IncidentStatus): boolean {
  return s === 'resolved' || s === 'ignored'
}

/**
 * canTransition 判定 from → to 是否合法。
 *
 * 逐条对齐后端 internal/incident.CanTransition：
 *   from == to        → false  同状态不算转换
 *   任一未知           → false  未知状态一律不放行
 *   from 是终态       → 只允许回到 open（重开）
 *   to 是 open        → false  非终态不允许回退到初始态
 *   其余              → 下标递增（只许前向）
 */
export function canTransition(from: IncidentStatus, to: IncidentStatus): boolean {
  if (from === to) return false
  const fi = indexOf(from)
  const ti = indexOf(to)
  if (fi < 0 || ti < 0) return false
  if (isTerminal(from)) return to === 'open'
  if (to === 'open') return false
  return ti > fi
}

/**
 * allowedTargets 返回当前状态下所有合法的目标状态，供渲染按钮用。
 *
 * 顺序按 LADDER 排列，与后端提示的合法取值顺序一致。
 */
export function allowedTargets(from: IncidentStatus): IncidentStatus[] {
  return LADDER.filter((to) => canTransition(from, to))
}

// 状态展示元数据：中文标签 + antd 徽标色。
//
// 终态用 default（灰）而非绿/橙：终态含「误报忽略」，给颜色会被读成
// 「一切正常」或「出了问题」，都不准确。
export const STATUS_META: Record<IncidentStatus, { label: string; color: string }> = {
  open: { label: '新建', color: 'blue' },
  acknowledged: { label: '已确认', color: 'cyan' },
  investigating: { label: '调查中', color: 'gold' },
  mitigated: { label: '已缓解', color: 'orange' },
  resolved: { label: '已解决', color: 'green' },
  ignored: { label: '已忽略', color: 'default' },
}

export const SEVERITY_META: Record<string, { label: string; color: string }> = {
  critical: { label: '严重', color: 'red' },
  warning: { label: '警告', color: 'orange' },
  info: { label: '提示', color: 'blue' },
}

export function statusLabel(s: string): string {
  return isValid(s) ? STATUS_META[s].label : s
}

export function statusColor(s: string): string {
  return isValid(s) ? STATUS_META[s].color : 'default'
}

export function severityColor(s: string): string {
  return SEVERITY_META[s]?.color ?? 'default'
}