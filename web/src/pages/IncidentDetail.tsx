// 事件详情页：处置区 + 三 Tab（关联告警 / 时间线 / 证据）。
//
// 「证据」Tab 自 M4 起接真实数据（GET /incidents/{id}/evidence），
// 组件在 components/EvidenceList.tsx。**空态仍必须说清原因**：
// 「采集器没注册」与「确实没有可采的证据」在页面上必须能区分，
// 否则用户会去查自己的集群而找不到问题（worker 才是该查的地方）。
//
// 处置按钮的可用性由 api/status.ts 的 canTransition 判定，与后端
// CanTransition 同构。若前端算错，用户点下去只会拿到一个 422。

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  App as AntApp,
  Button,
  Card,
  Descriptions,
  Input,
  Modal,
  Space,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { ArrowLeftOutlined } from '@ant-design/icons'
import { useNavigate, useParams } from 'react-router-dom'
import type { ColumnsType } from 'antd/es/table'
import {
  fetchIncident,
  transitionIncident,
  addIncidentComment,
  type AlertLink,
  type TimelineEntry,
  type IncidentStatus,
} from '../api/incidents'
import { fetchEvidence, type EvidenceType } from '../api/evidence'
import { EvidenceList } from '../components/EvidenceList'
import { allowedTargets, statusLabel, statusColor, severityColor } from '../api/status'

const { Title, Text } = Typography

export default function IncidentDetail() {
  const { id } = useParams<{ id: string }>()
  const incidentID = Number(id)
  const nav = useNavigate()
  const { message, modal } = AntApp.useApp()
  const qc = useQueryClient()
  const [noteOpen, setNoteOpen] = useState(false)
  const [note, setNote] = useState('')
  // 证据的类型筛选放在页面状态里而不是 URL query：
  // 它是纯 UI 关注点，放进 URL 会让「分享一个事件链接」带上
  // 与对方无关的筛选状态（对方打开后看到的是被筛过的子集）。
  const [evidenceType, setEvidenceType] = useState<EvidenceType | undefined>(
    undefined,
  )

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['incident', incidentID],
    queryFn: () => fetchIncident(incidentID),
    enabled: Number.isFinite(incidentID) && incidentID > 0,
  })

  // 证据单独查，不塞进事件详情响应。
  //
  // 理由：证据会随采集持续增长（单事件上限 50~200 条），
  // 与「看一眼事件基本信息」是两种访问节奏。合进详情响应会让
  // 每次打开详情都要拉全量证据，而多数人只是看一眼状态就走了。
  //
  // 轮询：事件创建后 2 分钟内证据才陆续出现，一次性拉取会看到空的
  // 证据 Tab 而让人以为没采到。12s 一次、仅在该 Tab 有过数据或
  // 正在看时轮询，避免长期挂着详情页还在打接口。
  const evidenceQuery = useQuery({
    queryKey: ['evidence', incidentID, evidenceType ?? 'all'],
    queryFn: () => fetchEvidence(incidentID, { type: evidenceType }),
    enabled: Number.isFinite(incidentID) && incidentID > 0,
    refetchInterval: 12_000,
    // 采集窗口内证据只增不减，重复拉到同样的内容没意义。
    refetchOnWindowFocus: false,
  })

  // 流转后要同时刷详情与列表：状态变了，列表那一行的状态/待确认数也变了。
  const afterMutate = () => {
    qc.invalidateQueries({ queryKey: ['incident', incidentID] })
    qc.invalidateQueries({ queryKey: ['incidents'] })
  }

  const doTransition = useMutation({
    mutationFn: (to: IncidentStatus) => transitionIncident(incidentID, to, 'web-console', note || undefined),
    onSuccess: (res) => {
      message.success(`已流转到「${statusLabel(res.incident.status)}」`)
      setNote('')
      afterMutate()
    },
    onError: (e: Error) => message.error(e.message),
  })

  const doComment = useMutation({
    mutationFn: () => addIncidentComment(incidentID, 'web-console', note),
    onSuccess: () => {
      message.success('备注已记录')
      setNote('')
      setNoteOpen(false)
      afterMutate()
    },
    onError: (e: Error) => message.error(e.message),
  })

  if (!Number.isFinite(incidentID) || incidentID <= 0) {
    return (
      <Card>
        <Alert type="error" showIcon message="事件 ID 无效" />
      </Card>
    )
  }
  if (isError) {
    return (
      <Card>
        <Alert
          type="error"
          showIcon
          message="加载事件详情失败"
          description={(error as Error).message}
        />
      </Card>
    )
  }

  const inc = data?.incident
  const targets = inc ? allowedTargets(inc.status) : []

  // 终态只能经 open 重开，这一步在真实值班里意味着「我判断关错了」。
  // 因此对终态加二次确认 —— 不是防误触，是让人想清楚。
  const confirmThen = (to: IncidentStatus) => {
    const go = () => doTransition.mutate(to)
    if (to === 'open') {
      modal.confirm({
        title: '重新打开这个事件？',
        content: `当前状态「${statusLabel(inc!.status)}」将被重开为「新建」。历史留痕会保留。`,
        okText: '重开',
        cancelText: '取消',
        onOk: go,
      })
      return
    }
    go()
  }

  const alertColumns: ColumnsType<AlertLink> = [
    {
      title: '告警 ID',
      dataIndex: 'alert_id',
      key: 'alert_id',
      width: 90,
      render: (v: number) => <Text code>{v}</Text>,
    },
    {
      title: '级别',
      dataIndex: 'severity',
      key: 'severity',
      width: 80,
      render: (s: string) => (
        <Tag color={severityColor(s)} style={{ marginInlineEnd: 0 }}>
          {s}
        </Tag>
      ),
    },
    {
      title: '告警名',
      key: 'alertname',
      render: (_, r) => r.labels?.alertname ?? '-',
    },
    { title: '实例', key: 'instance', render: (_, r) => r.labels?.instance ?? '-' },
    {
      title: '关联分数',
      dataIndex: 'correlation_score',
      key: 'score',
      width: 100,
      align: 'right',
      // null 显示「—」而不是 0：null 是 NEW 档（该告警是事件起因，
      // 本就没有关联分数），0 会被读成「算出来是零分」，是另一回事。
      render: (v: number | null) =>
        v === null ? (
          <Tooltip title="该告警是事件的起因（NEW 档），不参与关联评分">
            <Text type="secondary">—</Text>
          </Tooltip>
        ) : (
          <Text>{v.toFixed(4)}</Text>
        ),
    },
    {
      title: '待确认',
      dataIndex: 'needs_review',
      key: 'needs_review',
      width: 84,
      render: (v: boolean) =>
        v ? <Tag color="warning" style={{ marginInlineEnd: 0 }}>待确认</Tag> : null,
    },
  ]

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Space>
        <Button icon={<ArrowLeftOutlined />} onClick={() => nav('/incidents')}>
          返回
        </Button>
        <Title level={4} style={{ margin: 0 }}>
          {inc ? inc.title : '加载中…'}
        </Title>
        {inc && (
          <>
            <Tag color={statusColor(inc.status)}>{statusLabel(inc.status)}</Tag>
            <Tag color={severityColor(inc.severity)} style={{ marginInlineEnd: 0 }}>
              {inc.severity}
            </Tag>
          </>
        )}
      </Space>

      {inc && (
        <Card size="small">
          <Descriptions size="small" column={{ xs: 1, sm: 2, lg: 4 }}>
            <Descriptions.Item label="事件 ID">
              <Text code>{inc.id}</Text>
            </Descriptions.Item>
            <Descriptions.Item label="服务">{inc.service}</Descriptions.Item>
            <Descriptions.Item label="开始时间">
              {new Date(inc.started_at).toLocaleString()}
            </Descriptions.Item>
            <Descriptions.Item label="恢复时间">
              {inc.resolved_at ? (
                new Date(inc.resolved_at).toLocaleString()
              ) : (
                // 「恢复时间」为空与「忽略」是两回事：ignored 刻意不写该列
                // （进入 ignored 不是一次真实恢复，写进去会污染 MTTR）。
                <Text type="secondary">未恢复</Text>
              )}
            </Descriptions.Item>
          </Descriptions>
        </Card>
      )}

      <Card size="small" title="处置">
        <Space wrap>
          {targets.length === 0 && inc && (
            <Text type="secondary">当前状态无可用的下一步转换</Text>
          )}
          {targets.map((to) => (
            <Button
              key={to}
              // 「忽略」是误报处置，与「已解决」语义相反，
              // 放在最后并用默认样式，避免误点。
              type={to === 'ignored' || to === 'open' ? 'default' : 'primary'}
              danger={to === 'ignored'}
              loading={doTransition.isPending}
              onClick={() => confirmThen(to)}
            >
              转为{statusLabel(to)}
            </Button>
          ))}
          <Button onClick={() => setNoteOpen(true)}>添加备注</Button>
        </Space>
      </Card>

      <Card size="small" styles={{ body: { paddingTop: 8 } }}>
        <Tabs
          items={[
            {
              key: 'alerts',
              label: `关联告警${inc ? ` (${inc.alert_count})` : ''}`,
              children: (
                <>
                  {/* 截断必须提示：后端只内联最近 200 条，吞掉 truncated
                      会让人以为看到的就是全部（M3 手册 §4.3 明确要求）。 */}
                  {data?.truncated.alerts && (
                    <Alert
                      type="warning"
                      showIcon
                      style={{ marginBottom: 10 }}
                      message={`关联告警超过 200 条，此处仅显示最近 ${data.alerts.length} 条（共 ${inc?.alert_count} 条）`}
                    />
                  )}
                  <Table<AlertLink>
                    rowKey="alert_id"
                    size="small"
                    loading={isLoading}
                    columns={alertColumns}
                    dataSource={data?.alerts ?? []}
                    pagination={false}
                    locale={{ emptyText: '暂无关联告警' }}
                  />
                </>
              ),
            },
            {
              key: 'timeline',
              label: `时间线${data ? ` (${data.timeline.length})` : ''}`,
              children: (
                <>
                  {data?.truncated.timeline && (
                    <Alert
                      type="warning"
                      showIcon
                      style={{ marginBottom: 10 }}
                      message={`时间线超过 200 条，此处仅显示最近 ${data.timeline.length} 条`}
                    />
                  )}
                  <TimelineList items={data?.timeline ?? []} loading={isLoading} />
                </>
              ),
            },
            {
              key: 'evidence',
              // 角标用 total 而不是 count：筛选后 count 只反映当前视图，
              // 用它做 Tab 名会让人以为筛选把证据「弄没了」。
              label: `证据${evidenceQuery.data ? ` (${evidenceQuery.data.total})` : ''}`,
              children: (
                <EvidenceList
                  data={evidenceQuery.data}
                  loading={evidenceQuery.isLoading}
                  error={
                    evidenceQuery.isError
                      ? ((evidenceQuery.error as Error) ?? null)
                      : null
                  }
                  filter={evidenceType}
                  onFilterChange={setEvidenceType}
                  reload={() => void evidenceQuery.refetch()}
                />
              ),
            },
          ]}
        />
      </Card>

      <Modal
        title="添加备注"
        open={noteOpen}
        onCancel={() => setNoteOpen(false)}
        onOk={() => doComment.mutate()}
        confirmLoading={doComment.isPending}
        okText="记录"
        cancelText="取消"
      >
        <Input.TextArea
          rows={4}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder="写清这次判断的依据，便于交接与复盘"
        />
        <Text type="secondary" style={{ fontSize: 12 }}>
          空备注会被后端拒绝（400）—— 一条没有内容的留痕在页面上只是一行空白。
        </Text>
      </Modal>
    </Space>
  )
}

function TimelineList({ items, loading }: { items: TimelineEntry[]; loading: boolean }) {
  if (loading) {
    return <Text type="secondary">加载中…</Text>
  }
  if (items.length === 0) {
    return <Text type="secondary">暂无留痕</Text>
  }
  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {items.map((e, idx) => (
        <Card key={e.id ?? idx} size="small" styles={{ body: { padding: '8px 12px' } }}>
          <Space direction="vertical" size={2} style={{ width: '100%' }}>
            <Space size={8}>
              <Tag style={{ marginInlineEnd: 0 }}>{eventLabel(e.event_type)}</Tag>
              <Text type="secondary" style={{ fontSize: 12 }}>
                {new Date(e.timestamp).toLocaleString()}
              </Text>
              {/* actor 为空表示系统自动写入（落库为 NULL），
                  显示「系统」而不是空白 —— 空白会被读成「数据缺失」。 */}
              <Text type="secondary" style={{ fontSize: 12 }}>
                {e.actor || '系统'}
              </Text>
            </Space>
            {Object.keys(e.metadata ?? {}).length > 0 && (
              <Text type="secondary" style={{ fontSize: 12 }}>
                {Object.entries(e.metadata)
                  .map(([k, v]) => `${k}=${v}`)
                  .join('  ')}
              </Text>
            )}
          </Space>
        </Card>
      ))}
    </Space>
  )
}

// 时间线的 event_type 取值来自 0004 迁移的 CHECK 约束。
// 只列 M3 实际会写入的七类；evidence_found 属 M4，M3 不写。
function eventLabel(t: string): string {
  const map: Record<string, string> = {
    created: '事件创建',
    alert_linked: '告警关联',
    alert_fired: '告警触发',
    alert_resolved: '告警恢复',
    status_change: '状态变更',
    evidence_found: '发现证据',
    user_comment: '人工备注',
  }
  return map[t] ?? t
}