// M1 告警列表页。
//
// 只读展示后端返回的最新一批告警（排序、过滤策略由后端决定），并提供
// 「发送示例告警」按钮，便于在没有真实 Alertmanager 时也能驱动端到端演示。

import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Card, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { fetchAlerts, sendSampleAlert, type IIPAlert } from '../api/alerts'

const { Title, Text } = Typography

// 状态色：firing=红（活跃告警），resolved=绿（已恢复），其余=默认。
const statusColor: Record<string, string> = {
  firing: 'red',
  resolved: 'green',
  suppressed: 'default',
}

// 级别色：critical=红，warning=橙，info=蓝。
const severityColor: Record<string, string> = {
  critical: 'red',
  warning: 'orange',
  info: 'blue',
}

const columns: ColumnsType<IIPAlert> = [
  { title: 'ID', dataIndex: 'id', key: 'id', width: 80 },
  {
    title: '状态',
    dataIndex: 'status',
    key: 'status',
    width: 100,
    render: (s: string) => <Tag color={statusColor[s] ?? 'default'}>{s}</Tag>,
  },
  {
    title: '级别',
    dataIndex: 'severity',
    key: 'severity',
    width: 100,
    render: (s: string) => <Tag color={severityColor[s] ?? 'default'}>{s}</Tag>,
  },
  {
    title: '告警名称',
    key: 'alertname',
    render: (_, r) => r.labels?.alertname ?? '-',
  },
  {
    title: '实例',
    key: 'instance',
    render: (_, r) => r.labels?.instance ?? '-',
  },
  {
    title: '开始时间',
    dataIndex: 'starts_at',
    key: 'starts_at',
    render: (v: string) => new Date(v).toLocaleString(),
  },
  {
    title: '结束时间',
    key: 'ends_at',
    render: (_, r) =>
      r.ends_at ? (
        new Date(r.ends_at).toLocaleString()
      ) : (
        <Text type="secondary">持续中</Text>
      ),
  },
]

export default function Alerts() {
  const { message } = App.useApp()
  const qc = useQueryClient()
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['alerts'],
    queryFn: () => fetchAlerts(),
  })

  const onSend = async () => {
    try {
      await sendSampleAlert()
      message.success('示例告警已发送')
      // 让列表立即反映新行。
      qc.invalidateQueries({ queryKey: ['alerts'] })
    } catch (e) {
      message.error((e as Error).message)
    }
  }

  return (
    <div style={{ padding: 24 }}>
      <Space
        style={{ justifyContent: 'space-between', width: '100%', marginBottom: 16 }}
      >
        <Title level={3} style={{ margin: 0 }}>
          告警列表 · M1
        </Title>
        <Space>
          <Button onClick={() => refetch()} loading={isLoading}>
            刷新
          </Button>
          <Button type="primary" onClick={onSend}>
            发送示例告警
          </Button>
        </Space>
      </Space>

      {isError && (
        <Text type="danger" style={{ display: 'block', marginBottom: 12 }}>
          加载失败：{(error as Error).message}
        </Text>
      )}

      <Card>
        <Table<IIPAlert>
          rowKey="id"
          loading={isLoading}
          columns={columns}
          dataSource={data?.alerts ?? []}
          pagination={false}
          size="middle"
          locale={{ emptyText: '暂无告警' }}
        />
      </Card>
    </div>
  )
}
