// 事件列表页。
//
// 过滤参数与后端 internal/api/incidents.go parseIncidentFilter 一一对应：
// status 可多选（重复 key）、severity 逗号分隔、service 精确、from/to RFC3339、
// limit/offset 分页。
//
// 两个刻意的取舍：
// 1. **不显示 total**。后端用 limit+1 探测出 has_more，不返回可能失真的总数
//    （M3 手册 §4.3）。前端若显示「第 1-100 条」这种文案就等于把
//    「不知道总数」说成「一共这么多」，与后端的克制相悖。
// 2. **过滤变更回到第一页**。停在第 3 页改过滤条件，用户看到的是新结果的第 3 页
//    —— 通常是空的，会以为查询坏了。

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Button,
  Card,
  Input,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import type { ColumnsType } from 'antd/es/table'
import {
  fetchIncidents,
  ALL_STATUSES,
  ALL_SEVERITIES,
  type IncidentSummary,
  type IncidentStatus,
} from '../api/incidents'
import { statusLabel, statusColor, severityColor } from '../api/status'
import { palette } from '../theme'

const { Text } = Typography

const PAGE_SIZE = 50

export default function Incidents() {
  const nav = useNavigate()
  const [page, setPage] = useState(1)
  const [statuses, setStatuses] = useState<IncidentStatus[]>([])
  const [severities, setSeverities] = useState<string[]>([])
  const [service, setService] = useState('')

  const { data, isLoading, isError, error, refetch, isFetching } = useQuery({
    queryKey: ['incidents', page, statuses, severities, service],
    queryFn: () =>
      fetchIncidents({
        status: statuses,
        severity: severities,
        service: service.trim() || undefined,
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
      }),
  })

  // 任何过滤条件变化都回到第一页：否则停在第 3 页时收紧过滤，
  // 用户看到的是空页，会误判成「查不到」。
  const resetToFirst = <T,>(setter: (v: T) => void, v: T) => {
    setter(v)
    setPage(1)
  }

  const columns: ColumnsType<IncidentSummary> = [
    {
      title: 'ID',
      dataIndex: 'id',
      key: 'id',
      width: 72,
      render: (id: number) => <Text code>{id}</Text>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 92,
      render: (s: string) => <Tag color={statusColor(s)}>{statusLabel(s)}</Tag>,
    },
    {
      title: '级别',
      dataIndex: 'severity',
      key: 'severity',
      width: 76,
      render: (s: string) => (
        <Tag color={severityColor(s)} style={{ marginInlineEnd: 0 }}>
          {s}
        </Tag>
      ),
    },
    {
      title: '标题',
      dataIndex: 'title',
      key: 'title',
      // 长标题省略并挂 tooltip：标题是自由文本，历史上出现过超长值，
      // 不截断会把整行撑开把其它列挤走。点击跳转由 onRow 统一负责。
      ellipsis: true,
      render: (t: string) => (
        <Tooltip title={t} placement="topLeft">
          <span>{t}</span>
        </Tooltip>
      ),
    },
    { title: '服务', dataIndex: 'service', key: 'service', width: 120 },
    {
      title: '告警',
      dataIndex: 'alert_count',
      key: 'alert_count',
      width: 78,
      align: 'right',
      // 数字列的表头一并右对齐：表头左对齐 + 数字右对齐时，
      // 「告警」两字会飘在左边而数字贴着右边线，看着像两列错位。
      className: 'num-col',
    },
    {
      title: '待处理边',
      dataIndex: 'needs_review_count',
      key: 'needs_review_count',
      width: 92,
      align: 'right',
      className: 'num-col',
      // 列名刻意叫「待处理边」而不是「待确认」：侧栏徽标也写「待确认」，
      // 但两者数的东西不同 —— 这里是**这一个事件**下待人工判定的关联边数，
      // 侧栏是**整个队列**的条数。同名会让人以为是同一个数对不上。
      // 0 渲染成「-」而不是「0」：这一列的意义是「有没有需要你动手的」，
      // 0 会被扫成有内容。
      render: (n: number) =>
        n > 0 ? (
          <Tag color="warning" style={{ marginInlineEnd: 0 }}>
            {n}
          </Tag>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: '开始时间',
      dataIndex: 'started_at',
      key: 'started_at',
      width: 170,
      render: (v: string) => new Date(v).toLocaleString(),
    },
  ]

  if (isError) {
    return (
      <Card>
        <Text type="danger">加载事件列表失败：{(error as Error).message}</Text>
      </Card>
    )
  }

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Space wrap style={{ width: '100%' }}>
        <Select
          mode="multiple"
          allowClear
          placeholder="状态"
          style={{ minWidth: 180 }}
          value={statuses}
          onChange={(v) => resetToFirst(setStatuses, v)}
          options={ALL_STATUSES.map((s) => ({ value: s, label: statusLabel(s) }))}
        />
        <Select
          mode="multiple"
          allowClear
          placeholder="级别"
          style={{ minWidth: 140 }}
          value={severities}
          onChange={(v) => resetToFirst(setSeverities, v)}
          options={ALL_SEVERITIES.map((s) => ({ value: s, label: s }))}
        />
        <Input.Search
          placeholder="按服务过滤"
          allowClear
          style={{ width: 200 }}
          // 回车才搜，不做每键一查：服务名是精确匹配，
          // 打到一半就查几乎必然无结果，只会把列表闪成空。
          enterButton="搜索"
          onSearch={(v) => resetToFirst(setService, v)}
        />
        <div style={{ flex: 1 }} />
        <Button icon={<ReloadOutlined />} onClick={() => refetch()} loading={isFetching}>
          刷新
        </Button>
      </Space>

      <Card styles={{ body: { padding: 0 } }}>
        <Table<IncidentSummary>
          rowKey="id"
          loading={isLoading}
          columns={columns}
          dataSource={data?.incidents ?? []}
          size="small"
          onRow={(rec) => ({
            onClick: () => nav(`/incidents/${rec.id}`),
            style: { cursor: 'pointer' },
          })}
          pagination={false}
          // 后端只给 has_more、不给总数（避免失真的 total，M3 手册 §4.3），
          // 因此翻页由下面的「下一页」按钮显式驱动，不用 antd 的分页器 ——
          // 它需要 total 才能算页数。
          locale={{
            emptyText: (
              <span style={{ color: palette.text.tertiary }}>
                {isLoading ? '加载中…' : '没有符合条件的事件'}
              </span>
            ),
          }}
          rowClassName={() => 'clickable-row'}
        />
        {/* 页码 +翻页。没有分页器是因为后端不给总数（见上），
            所以「我在第几页 / 还有没有下一页」必须显式告诉用户，
            否则第 3 页之后就成了黑箱。 */}
        <div
          style={{
            padding: '8px 16px',
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            justifyContent: 'flex-end',
          }}
        >
          <Text type="secondary" style={{ fontSize: 12 }}>
            第 {page} 页 · 本页 {data?.incidents.length ?? 0} 条
            {data?.has_more ? '（还有更多）' : '（已到末页）'}
          </Text>
          <Button disabled={page === 1} onClick={() => setPage(page - 1)}>
            上一页
          </Button>
          <Button
            type="primary"
            disabled={!data?.has_more}
            loading={isFetching}
            onClick={() => setPage(page + 1)}
          >
            下一页
          </Button>
        </div>
      </Card>
    </Space>
  )
}