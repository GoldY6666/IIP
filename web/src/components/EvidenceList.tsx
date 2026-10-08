// 证据列表（M4）：卡片 + 可复现的查询串 + metadata 明细。
//
// 三个设计决定，每个都有具体理由，不是审美偏好：
//
// 1. **query 单独一行、用等宽字体、旁边给「复制」**。
//    证据的价值在于「运维能拿它回到原始数据源复现」。
//    埋在折叠面板里的查询串等于没有 —— 没有人愿意为了看一眼
//    PromQL 而点开三层。
//
// 2. **metadata 平铺成 `key=value`，不做成表格**。
//    各采集器的 metadata 键完全不同（prometheus 是 baseline/change_ratio，
//    loki 是 count，k8s 是 namespace/rollout_ok），做成表格就得为每种类型
//    写一套列定义，加一个采集器就得改这里。平铺是唯一不会过时的做法。
//
// 3. **采集不到时要说清是谁没采到**。
//    「暂无证据」有两种完全不同的成因：该事件确实没有可采的证据，
//    或者采集器没注册 / 数据源不可达。后者用户需要知道，
//    否则会去查自己的集群而找不到问题。所以 source 缺失与类型分布
//    都会显示出来，而不是只给一句「暂无」。

import { useMemo, useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Empty,
  Segmented,
  Space,
  Tag,
  Tooltip,
  Typography,
  App as AntApp,
} from 'antd'
import { CopyOutlined, ExportOutlined } from '@ant-design/icons'
import {
  CONFIDENCE_BAND_LABEL,
  EVIDENCE_TYPE_LABEL,
  confidenceBand,
  type ConfidenceBand,
  type Evidence,
  type EvidenceListResponse,
  type EvidenceType,
} from '../api/evidence'
import { buildExternalLinks } from '../api/externalLinks'
import { uiEndpoints } from '../api/uiConfig'
import { palette } from '../themeMode'
import type { Palette } from '../theme'

const { Text, Paragraph } = Typography

/**
 * CONFIDENCE_BAND_COLOR 是四档到语义色的映射。
 *
 * **配色方向是「越可信越绿」**：这是置信度，不是告警严重度。
 * 复用 critical/warning 表达「高置信度」会让运维把
 * 「这条证据很可靠」读成「这条证据很严重」—— 两个维度搞混的代价
 * 是处置动作反了。
 *
 * 用调色板而非 antd 的 `Tag color="success"`：后者是**写死的那套绿**，
 * 不随本项目双主题切换，在亮色卡片上对比度不达标。
 * 四个值都取自 theme.ts 的语义色，那套已按 WCAG AA 4.5:1 校准过。
 */
const CONFIDENCE_BAND_COLOR: Record<ConfidenceBand, keyof Palette['semantic']> = {
  high: 'success',
  medium: 'info',
  low: 'warning',
  unassessable: 'neutral',
}

/**
 * confidenceTooltip 拼出「这个数字是怎么来的」。
 *
 * **为什么必须把判据摊开给人看**：置信度是一个 0 到 1 之间的孤立数字，
 * 没人能从 `0.65` 推出任何结论。而它是由「变化幅度档位 × 证据覆盖度」
 * 算出来的——不展示这两项，用户就只能选择「信」或「不信」，
 * 那这个数字还不如不给。
 *
 * 读 metadata 里的 `confidence_basis` / `confidence_coverage` 两个键，
 * **原样展示、不解析**：basis 里含一句人话（如「变化 9.00 倍（band=strong）」），
 * 解析它就是把文案当契约（详见 api/evidence.ts 的 confidenceBand 注释）。
 */
function confidenceTooltip(e: Evidence): string {
  const parts: string[] = []
  const basis = e.metadata?.confidence_basis
  const coverage = e.metadata?.confidence_coverage
  if (basis) parts.push(`判据：${basis}`)
  if (coverage) parts.push(`证据覆盖度：${coverage} 种类型可评估`)
  if (parts.length === 0) {
    // 有置信度却没有判据键 = 后端填了值但没交代来源。
    // 直说「缺判据」而不是省略：省略会让这个数字看起来是自明的。
    parts.push('后端未提供判据明细，该数字无法复核')
  }
  return parts.join('；')
}

// SOURCE_LABEL 把来源码翻成中文；未收录的直接显示原值。
//
// 不做兜底成「未知来源」：那样会把「后端加了个新采集器」这件事
// 伪装成「数据有问题」。显示英文原值，运维至少认得出这是 k8s 还是别的。
const SOURCE_LABEL: Record<string, string> = {
  prometheus: 'Prometheus',
  loki: 'Loki',
  k8s: 'Kubernetes',
}

export interface EvidenceListProps {
  data: EvidenceListResponse | undefined
  loading: boolean
  error: Error | null
  filter: EvidenceType | undefined
  onFilterChange: (next: EvidenceType | undefined) => void
  reload: () => void
}

export function EvidenceList({
  data,
  loading,
  error,
  filter,
  onFilterChange,
  reload,
}: EvidenceListProps) {
  const { message } = AntApp.useApp()
  const [expanded, setExpanded] = useState<Set<number>>(new Set())

  // by_type 的角标必须在「不筛选」时才显示全量，否则选了 metric
  // 之后其他类型的角标还是全量数，会让人以为筛选没生效。
  const typeCounts = useMemo(() => data?.by_type ?? {}, [data?.by_type])

  const toggle = (id: number) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) {
        next.delete(id)
      } else {
        next.add(id)
      }
      return next
    })
  }

  if (error) {
    // 采集与查询是两件事：这里失败只说明**读不到证据**，
    // 不代表采集没发生（证据可能已经落库了，只是这次没读出来）。
    // 所以错误提示要给「重试」而不是让用户去查数据源。
    return (
      <Alert
        type="error"
        showIcon
        message="读取证据失败"
        description={
          <Space direction="vertical" size={4}>
            <Text style={{ fontSize: 12 }}>{error.message}</Text>
            <Text type="secondary" style={{ fontSize: 12 }}>
              这只说明证据**读不出来**，不代表采集没发生 ——
              证据可能已落库，只是这次请求失败。
            </Text>
            <Button size="small" onClick={reload}>
              重试
            </Button>
          </Space>
        }
      />
    )
  }

  return (
    <Space direction="vertical" size={10} style={{ width: '100%' }}>
      {/* 截断必须回显（手册 §4.2）。吞掉它等于把「只保留了 N 条」
          说成「一共 N 条」，用户会以为没有更多证据。 */}
      {data?.truncated && (
        <Alert
          type="warning"
          showIcon
          message={`证据超过 ${data.limit} 条上限，此处仅显示最近 ${data.count} 条（共 ${data.total} 条）。可在下方按类型分别查看。`}
        />
      )}

      <Segmented
        size="small"
        value={filter ?? 'all'}
        onChange={(v) =>
          onFilterChange(v === 'all' ? undefined : (v as EvidenceType))
        }
        options={[
          { label: '全部', value: 'all' },
          ...Object.entries(EVIDENCE_TYPE_LABEL).map(([value, label]) => ({
            // 角标显示真实条数（含 0）：显示 0 与不显示同样重要 ——
            // 用户需要知道「查过了，确实没有这类证据」。
            label: `${label}${typeCounts[value] !== undefined ? ` (${typeCounts[value]})` : ''}`,
            value,
          })),
        ]}
      />

      {loading && <Text type="secondary">加载中…</Text>}

      {!loading && data && data.count === 0 && (
        <Empty
          image={Empty.PRESENTED_IMAGE_SIMPLE}
          description={
            <span style={{ fontSize: 13 }}>
              {filter ? (
                <>
                  该事件没有
                  <Text strong>{EVIDENCE_TYPE_LABEL[filter]}</Text>
                  类证据。
                  <Paragraph
                    type="secondary"
                    style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}
                  >
                    证据由 worker 自动采集，可能仍在进行中
                    （事件创建后 2 分钟内产出）。若长时间为空，
                    请查 worker 日志里各采集器的注册与采集结果。
                  </Paragraph>
                </>
              ) : (
                <>
                  暂无证据。
                  <Paragraph
                    type="secondary"
                    style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}
                  >
                    证据由 worker 侧的采集器自动拉取（指标 / 日志 / 部署记录），
                    事件创建后 2 分钟内出现。若始终为空，
                    请查 worker 日志里各采集器的注册与采集结果 ——
                    采集器未注册（无地址）与数据源不可达都会表现为这里为空。
                  </Paragraph>
                </>
              )}
            </span>
          }
        />
      )}

      {!loading &&
        data?.evidence.map((e) => (
          <EvidenceCard
            key={e.id}
            evidence={e}
            expanded={expanded.has(e.id)}
            onToggle={() => toggle(e.id)}
            onCopy={async () => {
              try {
                await navigator.clipboard.writeText(e.query)
                void message.success('查询串已复制，可直接粘到数据源复现')
              } catch {
                // 剪贴板不可用（HTTP 非安全上下文、权限被拒）是很常见的，
                // 必须给替代路径而不是让用户以为复制成功了。
                void message.warning('浏览器拒绝了剪贴板访问，请手动选中复制')
              }
            }}
          />
        ))}
    </Space>
  )
}

function EvidenceCard({
  evidence,
  expanded,
  onToggle,
  onCopy,
}: {
  evidence: Evidence
  expanded: boolean
  onToggle: () => void
  onCopy: () => void
}) {
  const mdEntries = Object.entries(evidence.metadata ?? {})
  // 补采会让 collected_at 明显晚于 timestamp。差值超过 1 分钟才提示：
  // 正常采集是秒级，差太多说明这条是补采来的（或数据源延迟），
  // 而这会影响「证据有多新」的判断。
  const lagMs = Date.parse(evidence.collected_at) - Date.parse(evidence.timestamp)
  const backfilled = Number.isFinite(lagMs) && lagMs > 60_000

  // 外部面板链接。空数组表示这条没有可渲染的链接（k8s 等来源，
  // 或 query 为空）—— 此时**不渲染任何按钮**，而不是渲染一个灰掉的「打开」：
  // 那会让人以为面板挂了，而真相是「这条证据本来就没有对应面板」。
  const extLinks = buildExternalLinks(evidence, uiEndpoints())

  // 置信度分档。null 会落成 'unassessable'，但那条分支不渲染配色，
  // 这里算出来只是为了下面有值时的分支能用 —— 提前算而不是
  // 在 JSX 里写 confidence === null ? ... : confidenceBand(...)，
  // 是为了让「什么时候用档位」在一处就能看全。
  const band = confidenceBand(evidence.confidence)

  return (
    <Card
      size="small"
      styles={{ body: { padding: '8px 12px' } }}
      style={{ cursor: mdEntries.length > 0 ? 'pointer' : 'default' }}
      onClick={mdEntries.length > 0 ? onToggle : undefined}
    >
      <Space direction="vertical" size={4} style={{ width: '100%' }}>
        <Space size={6} wrap>
          <Tag color="blue" style={{ marginInlineEnd: 0 }}>
            {EVIDENCE_TYPE_LABEL[evidence.type] ?? evidence.type}
          </Tag>
          <Tag style={{ marginInlineEnd: 0 }}>
            {SOURCE_LABEL[evidence.source] ?? evidence.source}
          </Tag>
          <Text style={{ fontSize: 13 }}>{evidence.value}</Text>
        </Space>

        <Space size={8} wrap>
          <Tooltip title="证据自身发生的时间（不是采集时刻）">
            <Text type="secondary" style={{ fontSize: 12 }}>
              发生 {new Date(evidence.timestamp).toLocaleString()}
            </Text>
          </Tooltip>
          {backfilled && (
            <Tooltip
              title={`落库于 ${new Date(evidence.collected_at).toLocaleString()}，晚于发生时间较多，属补采或数据源延迟`}
            >
              <Tag color="orange" style={{ marginInlineEnd: 0, fontSize: 11 }}>
                补采
              </Tag>
            </Tooltip>
          )}
          {/* confidence 为 null = 没有基线可比、没资格评估（不是 0）。
              显示「未评估」而不是 0 或「—」：0 会被读成「没有支撑力」。 */}
          <Text type="secondary" style={{ fontSize: 12 }}>
            置信度{' '}
            {evidence.confidence === null ? (
              <Tooltip title="这条证据没有事故前的基线可比，判据无法给出置信度。未评估不等于没有支撑力，只等于「没资格判断」。">
                未评估
              </Tooltip>
            ) : (
              <Tooltip title={confidenceTooltip(evidence)}>
                <Text
                  style={{
                    fontSize: 12,
                    fontWeight: 600,
                    color: palette.semantic[CONFIDENCE_BAND_COLOR[band]],
                  }}
                >
                  {evidence.confidence.toFixed(2)}
                  <Text
                    type="secondary"
                    style={{ fontSize: 11, marginInlineStart: 4 }}
                  >
                    {CONFIDENCE_BAND_LABEL[band]}
                  </Text>
                </Text>
              </Tooltip>
            )}
          </Text>
          {mdEntries.length > 0 && (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {expanded ? '收起明细' : `明细 ${mdEntries.length} 项`}
            </Text>
          )}
        </Space>

        {/* 查询串常驻可见：这正是证据「可复现」的载体。 */}
        <Space size={4} style={{ width: '100%' }}>
          <Text
            copyable={false}
            style={{
              fontSize: 12,
              fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
              color: 'rgba(255,255,255,0.72)',
              wordBreak: 'break-all',
              flex: 1,
            }}
          >
            {evidence.query}
          </Text>
          <Tooltip title="复制查询串">
            <Button
              size="small"
              type="text"
              icon={<CopyOutlined />}
              onClick={(ev) => {
                // 不 stopPropagation 的话点复制会同时触发卡片展开，
                // 表现为「点一下复制，明细区莫名其妙张开了」。
                ev.stopPropagation()
                onCopy()
              }}
            />
          </Tooltip>
          {extLinks.map((l) => (
            <Tooltip key={l.href} title={l.title}>
              <Button
                size="small"
                type="text"
                href={l.href}
                target="_blank"
                // rel="noreferrer"：新标签打开时一并屏蔽 referrer，
                // 面板侧不该知道用户是从 IIP 的哪个事件点进来的。
                rel="noreferrer"
                icon={<ExportOutlined />}
                onClick={(ev) => {
                  // 必须 stopPropagation：整张卡片的 onClick 是「展开明细」，
                  // 不拦住的话点链接会同时展开卡片并打开新标签。
                  ev.stopPropagation()
                }}
              >
                {l.label}
              </Button>
            </Tooltip>
          ))}
        </Space>

        {expanded && mdEntries.length > 0 && (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {mdEntries.map(([k, v]) => `${k}=${v}`).join('　')}
          </Text>
        )}
      </Space>
    </Card>
  )
}
