// S1 关联配置页。
//
// 规格：docs/IIP-M5详细开发手册.md §4.1。
// 后端契约：internal/api/correlation_config.go（已实现，无后端改动）。
// 校验口径：internal/correlation.Config.Validate —— 前端只做**辅助**校验，
// 后端 422 才是权威（§4.1.4 第 2 条）。

import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Card, Divider, InputNumber, Slider, Space, Switch, Typography } from 'antd'
import WeightRow from '../components/WeightRow'
import {
  ConfigApiError,
  DIMENSION_META,
  fetchCorrelationConfig,
  putCorrelationConfig,
  validateConfig,
  weightsSumVerdict,
  WEIGHT_SUM_EPSILON,
  type CorrelationConfig,
  type DimensionKey,
} from '../api/correlationConfig'
import { palette } from '../themeMode'

const { Title, Text, Paragraph } = Typography

// 维度的固定展示顺序（与 DIMENSION_META 的键顺序一致）。
// 显式列出而不是 Object.keys 遍历：遍历顺序取决于对象的键插入顺序，
// 将来有人调整 DIMENSION_META 的定义顺序，页面上的行顺序就会跟着变 ——
// 而权重区的顺序对应「用户对五个维度的心智顺序」，不该由实现细节决定。
const DIMENSIONS: DimensionKey[] = ['time', 'service', 'label', 'dependency', 'historical']

/** 权重的可选值域刻度。给滑块一个刻度尺，用户才知道自己拖到了哪。 */
const MARKS = { 0: '0', 0.5: '0.5', 1: '1' }

export default function Settings() {
  const { message } = App.useApp()
  const qc = useQueryClient()

  // draft 是「正在编辑的副本」。刻意与 query 的 data 分开存：
  // 直接改 data 会让 query 缓存与用户输入互相污染 —— 取消编辑时
  // 就没有「回到原值」的依据了（query 被就地改过）。
  const [draft, setDraft] = useState<CorrelationConfig | null>(null)
  // baseline 是最近一次**从后端拿到**的值，用于「放弃改动」。
  const [baseline, setBaseline] = useState<CorrelationConfig | null>(null)

  const q = useQuery({
    queryKey: ['correlation-config'],
    queryFn: fetchCorrelationConfig,
  })

  // 首次拿到数据时填两个副本；之后不再同步 —— 否则用户正在输入的
  // 草稿会被后台 refetch 悄悄冲掉（queryKey 命中缓存或窗口聚焦都会触发）。
  useEffect(() => {
    if (q.data) {
      setDraft(q.data)
      setBaseline(q.data)
    }
  }, [q.data])

  const save = useMutation({
    mutationFn: putCorrelationConfig,
    onSuccess: (saved) => {
      // **用 PUT 的返回值刷新界面，而不是假设它就是最终态**（§4.1.2）。
      // 后端会补齐部分更新里没提供的字段，也可能规范化了数值。
      setDraft(saved)
      setBaseline(saved)
      qc.setQueryData(['correlation-config'], saved)
      message.success('配置已保存，下一轮关联即生效（无需重启）')
    },
    onError: (e) => {
      // **422 的原文直接显示**（§4.1.4 第 1 条）：后端给的是
      // 给用户看的中文文案，前端不该重写一套必然漂移的提示。
      if (e instanceof ConfigApiError) {
        message.error(e.serverMessage || e.message)
      } else {
        message.error((e as Error).message)
      }
    },
  })

  if (q.isLoading) {
    return (
      <div style={{ padding: 24 }}>
        <Text type="secondary">加载中…</Text>
      </div>
    )
  }

  if (q.isError || !draft || !baseline) {
    return (
      <div style={{ padding: 24 }}>
        <Text type="danger">
          加载失败：{q.error instanceof ConfigApiError ? q.error.serverMessage || q.error.message : String(q.error)}
        </Text>
      </div>
    )
  }

  const setW = (k: DimensionKey, v: number) =>
    setDraft({ ...draft, weights: { ...draft.weights, [k]: v } })

  const setT = (k: 'merge' | 'review', v: number) =>
    setDraft({ ...draft, thresholds: { ...draft.thresholds, [k]: v } })

  const verdict = weightsSumVerdict(draft.weights)
  // 前端只拦「明显不合法」。校验函数返回 null 表示通过。
  const blocking = validateConfig(draft)
  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline)

  return (
    <div style={{ padding: 24, maxWidth: 860 }}>
      <Space style={{ justifyContent: 'space-between', width: '100%', marginBottom: 16 }}>
        <Title level={3} style={{ margin: 0 }}>
          关联配置
        </Title>
        <Space>
          <Button onClick={() => setDraft(baseline)} disabled={!dirty}>
            放弃改动
          </Button>
          <Button
            type="primary"
            loading={save.isPending}
            disabled={!dirty || blocking !== null}
            onClick={() => save.mutate(draft)}
          >
            保存
          </Button>
        </Space>
      </Space>

      <Card title="权重" style={{ marginBottom: 16 }}>
        {DIMENSIONS.map((k) => (
          <WeightRow
            key={k}
            label={DIMENSION_META[k].label}
            hint={DIMENSION_META[k].hint}
            value={draft.weights[k]}
            onChange={(v) => setW(k, v)}
          />
        ))}

        <Divider style={{ margin: '12px 0' }} />

        {/* 权重和实时显示。**必须显示到能看出容差边界**：
            用户把五个 0.2 加成 1.0 就以为过了，而后端容差是 ±0.001，
            差 0.002 也是非法的。不显示等值 = 让用户拿真金白银试错。 */}
        <Text style={{ color: verdict.ok ? palette.text.secondary : palette.semantic.critical }}>
          权重和 {verdict.sum.toFixed(4)}（需为 1.0，允许误差 ±{WEIGHT_SUM_EPSILON}）
        </Text>
        {!verdict.ok && (
          <Text type="danger" style={{ display: 'block', marginTop: 4 }}>
            权重和偏离 1.0 达 {Math.abs(verdict.diff).toFixed(4)}，超出容差。
          </Text>
        )}
      </Card>

      <Card title="决策阈值" style={{ marginBottom: 16 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 16, padding: '6px 0' }}>
          <div style={{ width: 160, flexShrink: 0 }}>
            <Text strong style={{ color: palette.text.primary }}>
              合并阈值
            </Text>
            <br />
            <Text style={{ color: palette.text.tertiary, fontSize: 12 }}>
              得分 ≥ 此值直接合并到既有事件
            </Text>
          </div>
          <Slider
            style={{ flex: 1, marginInlineEnd: 8 }}
            min={0}
            max={1}
            step={0.01}
            marks={MARKS}
            value={draft.thresholds.merge}
            onChange={(v) => setT('merge', v)}
          />
          <InputNumber
            style={{ width: 96 }}
            min={0}
            max={1}
            step={0.001}
            value={draft.thresholds.merge}
            onChange={(v) => setT('merge', typeof v === 'number' ? v : 0)}
          />
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 16, padding: '6px 0' }}>
          <div style={{ width: 160, flexShrink: 0 }}>
            <Text strong style={{ color: palette.text.primary }}>
              待确认阈值
            </Text>
            <br />
            <Text style={{ color: palette.text.tertiary, fontSize: 12 }}>
              得分落在此值与合并阈值之间 → 关联但等人工确认
            </Text>
          </div>
          <Slider
            style={{ flex: 1, marginInlineEnd: 8 }}
            min={0}
            max={1}
            step={0.01}
            marks={MARKS}
            value={draft.thresholds.review}
            onChange={(v) => setT('review', v)}
          />
          <InputNumber
            style={{ width: 96 }}
            min={0}
            max={1}
            step={0.001}
            value={draft.thresholds.review}
            onChange={(v) => setT('review', typeof v === 'number' ? v : 0)}
          />
        </div>

        {/* merge ≥ review 的交叉提示。**常驻而非只在违规时出现** ——
            用户调 merge 时该同时看到 review 的位置，否则很容易调出
            一个「save 按钮莫名其妙变灰」的状态而无从判断原因。 */}
        {draft.thresholds.merge < draft.thresholds.review && (
          <Text type="danger" style={{ display: 'block', marginTop: 4 }}>
            合并阈值必须 ≥ 待确认阈值，否则低于待确认阈值的得分会既不合并也不待确认。
          </Text>
        )}
      </Card>

      <Card title="总开关">
        <Space>
          <Switch
            checked={draft.enabled}
            onChange={(v) => setDraft({ ...draft, enabled: v })}
          />
          <Text>
            {draft.enabled
              ? '已启用：告警落库后参与关联'
              : '已停用：告警仍落库，但不参与关联'}
          </Text>
        </Space>
        <Paragraph type="secondary" style={{ fontSize: 12, marginTop: 8, marginBottom: 0 }}>
          停用不是「关掉引擎」，而是「只收告警不做关联」—— 停用期间进来的告警不会补算。
        </Paragraph>
      </Card>

      {/* 阻断原因放最后而不是塞到按钮的 tooltip：tooltip 要悬停才看得到，
          而用户是在点「保存」发现按钮灰了之后才需要知道原因。 */}
      {dirty && blocking && (
        <Text type="danger" style={{ display: 'block', marginTop: 12 }}>
          无法保存：{blocking}
        </Text>
      )}
    </div>
  )
}
