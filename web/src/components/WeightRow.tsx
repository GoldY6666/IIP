// 单个权重维度的行：名称 + 语义说明 + 滑块 + 数字输入。
//
// 拆成独立组件而不是把五行写在页面里，因为每一行的**交互是同一套**
// （滑块与数字框双向同步、越界标红）。写在一处的话这个同步逻辑
// 会被复制五遍，而复制出来的第五遍一定有细微不同 —— 比如少了越界提示。
//
// 滑块与输入框的步长刻意不同：滑块 0.01（拖动要细），输入框 0.001
// （容差就是 0.001，用 0.01 的步长会让「合法但不在步长上」的值打不进去）。

import { InputNumber, Slider, Tooltip, Typography } from 'antd'
import { palette } from '../themeMode'

const { Text } = Typography

export interface WeightRowProps {
  label: string
  hint: string
  value: number
  onChange: (v: number) => void
}

export default function WeightRow({ label, hint, value, onChange }: WeightRowProps) {
  // 越界判定写在这里而不是页面：五行共用，且「看起来合法但实际越界」
  // 是最需要在**输入的那一刻**就看出来的情况。
  const bad = !(value >= 0 && value <= 1)

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 16, padding: '6px 0' }}>
      <div style={{ width: 160, flexShrink: 0 }}>
        <Text strong style={{ color: palette.text.primary }}>
          {label}
        </Text>
        <br />
        {/* 说明用 tooltip 而非常驻文字：五行的说明加起来有一段篇幅，
            常驻会把权重区撑得很高，而它们是「读一次记住」型信息。 */}
        <Tooltip title={hint}>
          <Text style={{ color: palette.text.tertiary, fontSize: 12, cursor: 'help' }}>
            {hint}
          </Text>
        </Tooltip>
      </div>

      <Slider
        style={{ flex: 1, marginInlineEnd: 8 }}
        min={0}
        max={1}
        step={0.01}
        value={value}
        onChange={onChange}
      />

      <InputNumber
        style={{ width: 96 }}
        min={0}
        max={1}
        step={0.001}
        value={value}
        onChange={(v) => {
          // **null 必须回落成 0，而不是原样传给上层**：
          // 用户清空输入框时 antd 给的是 null，直接 set 进 state
          // 会让滑块拿到 null 并崩掉（Slider 的 value 是 number）。
          // 0 是这里最不坏的解释 —— 用户既然清空了，多半是想从 0 重新填。
          onChange(typeof v === 'number' && Number.isFinite(v) ? v : 0)
        }}
        status={bad ? 'error' : undefined}
      />
    </div>
  )
}
