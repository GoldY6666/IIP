// REVIEW 队列页：人工确认「这条告警该不该合进这个事件」。
//
// M3 手册 §4.5 的两个操作在此落地：confirm 保留关联边；reject 删除关联边
// 并写抑制规则（同指纹的同类告警以后不再提示）。
//
// reject 的后果必须**说清**，因为它不是撤销而是「以后都别再提」：
// 若该告警已无其它关联边，它会被从事件里摘掉（unlinked）。
// 因此按钮用 Popconfirm 要求确认，并在文案里写明后续影响。

import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, App as AntApp, Button, Card, Popconfirm, Space, Table, Tag, Tooltip, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { fetchReviews, decideReview, type ReviewEdge } from '../api/incidents'
import { severityColor } from '../api/status'
import { palette } from '../theme'

const { Title, Text } = Typography

export default function Reviews() {
  const { message } = AntApp.useApp()
  const qc = useQueryClient()

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['reviews', 'list'],
    queryFn: () => fetchReviews(100, 0),
  })

  // 判定后要刷三处：队列本身（少一条）、事件列表（待确认数变了）、
  // 侧栏徽标（数量变了）。
  const afterDecide = () => {
    qc.invalidateQueries({ queryKey: ['reviews'] })
    qc.invalidateQueries({ queryKey: ['incidents'] })
    qc.invalidateQueries({ queryKey: ['incident'] })
  }

  const columns: ColumnsType<ReviewEdge> = [
    {
      title: '事件',
      dataIndex: 'incident_id',
      key: 'incident_id',
      width: 90,
      render: (v: number) => <Text code>{v}</Text>,
    },
    {
      title: '级别',
      key: 'severity',
      width: 80,
      render: (_, r) => (
        <Tag color={severityColor(r.alert.severity)} style={{ marginInlineEnd: 0 }}>
          {r.alert.severity}
        </Tag>
      ),
    },
    { title: '告警名', key: 'alertname', render: (_, r) => r.alert.labels?.alertname ?? '-' },
    { title: '实例', key: 'instance', render: (_, r) => r.alert.labels?.instance ?? '-' },
    {
      title: '关联分数',
      key: 'score',
      width: 100,
      align: 'right',
      render: (_, r) => {
        const v = r.alert.correlation_score
        // 落在 REVIEW 档意味着分数在 [0.50, 0.80)，但仍可能为 NULL：
        // 分数缺失时不能让人以为「零分」，要说清是「无分数」。
        return v === null ? (
          <Text type="secondary">—</Text>
        ) : (
          <Text>{v.toFixed(4)}</Text>
        )
      },
    },
    {
      title: '关联时间',
      key: 'correlated_at',
      width: 170,
      render: (_, r) => new Date(r.alert.correlated_at).toLocaleString(),
    },
    {
      title: '操作',
      key: 'act',
      width: 190,
      render: (_, r) => (
        <Space size={6}>
          <Button
            size="small"
            type="primary"
            onClick={async () => {
              try {
                const res = await decideReview(r.incident_id, r.alert.alert_id, 'confirm', 'web-console')
                message.success(
                  res.unlinked
                    ? '已确认合并（该告警已从事件中摘除，请检查事件详情）'
                    : '已确认合并',
                )
                afterDecide()
              } catch (e) {
                message.error((e as Error).message)
              }
            }}
          >
            确认合并
          </Button>
          <Popconfirm
            title="拒绝这次合并？"
            description={
              <span style={{ fontSize: 12 }}>
                该关联边会被删除，并写下抑制规则：
                <b>同指纹的同类告警以后不再提示</b>。
                若这条告警已无其它关联边，它会从事件中被摘除。
              </span>
            }
            okText="确认拒绝"
            cancelText="取消"
            okButtonProps={{ danger: true }}
            onConfirm={async () => {
              try {
                const res = await decideReview(r.incident_id, r.alert.alert_id, 'reject', 'web-console')
                message.success(
                  res.unlinked ? '已拒绝，该告警已从事件中摘除' : '已拒绝，并记入抑制',
                )
                afterDecide()
              } catch (e) {
                message.error((e as Error).message)
              }
            }}
          >
            <Button size="small" danger>
              拒绝
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Space>
        <Title level={4} style={{ margin: 0 }}>
          待确认队列
        </Title>
        <Text type="secondary">
          关联引擎对置信度不足的边不做自动合并，交给人判断
        </Text>
      </Space>

      {isError && (
        <Alert
          type="error"
          showIcon
          message="加载待确认队列失败"
          description={(error as Error).message}
        />
      )}

      <Card styles={{ body: { padding: 0 } }}>
        <Table<ReviewEdge>
          rowKey={(r) => `${r.incident_id}:${r.alert.alert_id}`}
          loading={isLoading}
          columns={columns}
          dataSource={data?.reviews ?? []}
          pagination={false}
          size="small"
          locale={{
            emptyText: (
              <span style={{ color: palette.text.tertiary }}>
                {isLoading ? '加载中…' : '队列已清空 —— 没有需要人工判断的关联'}
              </span>
            ),
          }}
        />
        {data?.has_more && (
          <div style={{ padding: '8px 16px', fontSize: 12, color: palette.text.tertiary }}>
            仅显示前 {data.reviews.length} 条，可能还有更多
          </div>
        )}
      </Card>

      <Alert
        type="info"
        showIcon
        message={
          <Tooltip title="拒绝会写review_suppressions 表；此后同指纹的告警在关联阶段直接降级为新建事件">
            <span>为什么需要人工确认？</span>
          </Tooltip>
        }
        description="关联引擎对分数落在 0.50～0.80 之间的边不自动合并，也不直接丢弃——它们既可能是同根因，也可能是巧合。逐条判断是这个平台可信度的来源。"
      />
    </Space>
  )
}