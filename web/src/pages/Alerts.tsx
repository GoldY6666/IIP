// M1 告警列表页。
//
// 只读展示后端返回的最新一批告警（排序、过滤策略由后端决定），并提供
// 「发送示例告警」按钮，便于在没有真实 Alertmanager 时也能驱动端到端演示。

import { useQuery, useQueryClient } from '@tanstack/react-query'
import { App, Button, Card, Space, Table, Tag, Typography } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { fetchAlerts, sendSampleAlert, type IIPAlert } from '../api/alerts'
import TimeCell from '../components/TimeCell'
import { LIST_MIN_HEIGHT } from '../components/listLayout'
import { palette } from '../themeMode'

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
    width: 118,
    // 相对时间：告警列表一屏几十行，值班扫的是「哪些是刚来的」。
    // 绝对时刻靠悬停（TimeCell 内挂 Tooltip）。
    render: (v: string) => <TimeCell value={v} />,
  },
  {
    title: '结束时间',
    key: 'ends_at',
    width: 118,
    render: (_, r) =>
      r.ends_at ? (
        <TimeCell value={r.ends_at} />
      ) : (
        // 「持续中」是**状态**而非时间，不能参与相对时间计算 ——
        // 对 null 调相对时间会得到「—」，那会把「还在烧」说成
        // 「没有结束时间」，两者含义完全不同。
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
        {/* minHeight 的理由同 Incidents.tsx：少量数据时表格会缩成
            几行高，下半屏全空，看起来像没加载完。 */}
        <div style={{ minHeight: LIST_MIN_HEIGHT }}>
          <Table<IIPAlert>
            rowKey="id"
            loading={isLoading}
            columns={columns}
            dataSource={data?.alerts ?? []}
            pagination={false}
            size="middle"
            locale={{
              emptyText: (
                <div style={{ padding: '120px 0', textAlign: 'center' }}>
                  {/* 用 palette 而不是字面量：空态文案颜色在两套主题下
                      不同（暗色 tertiary 3.77:1、亮色 4.51:1），
                      写死 '#8b949e' 会在亮色主题下偏亮。 */}
                  <span style={{ color: palette.text.tertiary }}>暂无告警</span>
                </div>
              ),
            }}
          />
        </div>
      </Card>
    </div>
  )
}
