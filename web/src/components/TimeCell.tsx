// 时间单元格：相对时间 + 悬停给绝对时刻。
//
// 为什么做成组件而不是在每个页面里写 Tooltip：
// 时间列散在 4 个页面（事件列表 / 详情 / 待确认队列 / 告警），
// 每处各写一遍 Tooltip 包裹，两年内一定会有一处漏掉
// 「绝对时间可得」这个约定 —— 而漏掉的那处会成为
// 「查不到事发时刻」的排查黑洞。
//
// **不做实时刷新**：相对时间在页面停留期间会变旧
// （写着「1 分钟前」时实际已过了 5 分钟）。真要自动更新得挂定时器，
// 而值班页面通常开着不动 —— 等用户下一次操作（切页/刷新）自然就更新了。
// 为「文本可能不准几分钟」挂一个每分钟触发的重渲染不划算。
//
// **不导 React.memo**：父组件每次渲染都会新建这个元素，
// 而它只是两个文本节点的组合，memo 的比较成本高于收益。

import { useState } from 'react'
import { Tooltip } from 'antd'
import { absoluteTimeOf, relativeTimeOf, timeAgoTip } from '../api/timeFormat'

interface Props {
  /**
   * 时间值。后端给的是 RFC3339 字符串，但**同时接受 Date** ——
   * 调用方常手上已有 parseTime / timeFormat 的结果，类型上不允许
   * 会逼他们写一次无意义的 toISOString() 绕回字符串。
   * 非法值渲染成「—」而不是 "Invalid Date"。
   */
  value: string | Date | null | undefined
  /**
   * alwaysAbsolute 为 true 时**正文显示绝对时刻**（不给相对）。
   *
   * 用于「精确值比扫读更重要」的位置：详情页概况区与时间线 ——
   * 那里行数少、扫读压力小，而排查时正需要精确时刻。
   * 不传则正文显示相对时间（列表场景）。
   *
   * **注意：两种模式都保留 Tooltip**。绝对模式下悬停给
   * 「绝对 + 相对」双口径，用于快速判断「这几个是不是都是刚才的」——
   * 否则「绝对模式」会连带把相对时间这个信息彻底丢掉。
   */
  alwaysAbsolute?: boolean
}

export default function TimeCell({ value, alwaysAbsolute = false }: Props) {
  // 惰性初始化：**只在挂载时取一次「现在」**。
  // 直接写 Date.now() 会让每次渲染都拿到新值 —— 多数情况下无害，
  // 但会让 Tooltip 的 title 在无意义地变化，且让「这个组件不依赖
  // 外部时钟」这个性质变得不成立。长时间开着的标签页里，
  // 模块加载时的 Date.now() 会给出几小时前的基准，所有相对时间都是错的，
  // 故取「挂载时刻」而不是「模块加载时刻」。
  const [now] = useState(() => Date.now())

  return (
    <Tooltip title={timeAgoTip(value, now)} placement="topLeft">
      {/* 必须包一个 span：Tooltip 要给子元素挂 ref，
          裸字符串会报「children 不支持 ref」而整个页面崩掉。 */}
      <span>{alwaysAbsolute ? absoluteTimeOf(value) : relativeTimeOf(value, now)}</span>
    </Tooltip>
  )
}