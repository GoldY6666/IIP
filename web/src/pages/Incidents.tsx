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
import { useFilterUrl } from '../api/useFilterUrl'
import TimeCell from '../components/TimeCell'
import { LIST_MIN_HEIGHT } from '../components/listLayout'
import { clickableRowProps } from '../components/clickableRow'
import { palette } from '../themeMode'

const { Text } = Typography

const PAGE_SIZE = 50


export default function Incidents() {
  const nav = useNavigate()
  const [page, setPage] = useState(1)

  // **初值来自 URL**：同事发来 /incidents?service=checkout 时，
  // 打开就该是过滤好的，而不是先显示全量再突然跳一下。
  const { initial, sync } = useFilterUrl()
  const [statuses, setStatuses] = useState<IncidentStatus[]>(initial.status)
  const [severities, setSeverities] = useState<string[]>(initial.severity)
  const [service, setService] = useState(initial.service)

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

  /** 改状态筛选：同步 state + 回第一页 + 写 URL。 */
  const changeStatus = (v: IncidentStatus[]) => {
    setStatuses(v)
    setPage(1)
    sync({ status: v, severity: severities, service: service.trim() })
  }

  /** 改级别筛选。 */
  const changeSeverity = (v: string[]) => {
    setSeverities(v)
    setPage(1)
    sync({ status: statuses, severity: v, service: service.trim() })
  }

  /** 改服务过滤（回车触发，见 Input.Search 的注释）。 */
  const changeService = (v: string) => {
    const trimmed = v.trim()
    setService(trimmed)
    setPage(1)
    sync({ status: statuses, severity: severities, service: trimmed })
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
      width: 118,
      // 刻意用相对时间（"6 分钟前"）而不是 `2026/10/4 22:24:39`：
      // 值班问的是「这条是不是刚出的」，相对时间一眼能答，
      // 绝对时间要���心做一次减法。精确时刻靠悬停给
      //（TimeCell 内部挂 Tooltip 显示绝对+相对两个口径）。
      //
      // 列宽从 170 收到 118：相对文本本来就短，给宽了会在
      // 「标题」列上留下大片空白（标题是自由文本，本就该占主要空间）。
      render: (v: string) => <TimeCell value={v} />,
    },
  ]

  if (isError) {
    // 错误态必须**自带恢复入口**。
    //
    // 原来这里只渲染一句错误文案，而「刷新」按钮（含 refetch）在下面的
    // 正常分支里 —— 提前 return 意味着错误态下它根本不渲染。
    // 后果不是「不好看」，是**不可恢复**：值班的人看到红色报错后
    // 只能手动刷浏览器（顺带丢掉所有筛选条件）。
    //
    // 后端挂了可能几秒后就恢复，也可能要等人处理；差别在于用户
    // 能不能自己试第二次。这条判据（F6 E2E-5）就是钉住这个入口。
    return (
      <Card>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Text type="danger">加载事件列表失败：{(error as Error).message}</Text>
          <Space>
            <Button icon={<ReloadOutlined />} onClick={() => refetch()} loading={isFetching}>
              重试
            </Button>
            <Text type="secondary">
              若后端正在重启，等它恢复后点「重试」即可，不必刷新整个页面。
            </Text>
          </Space>
        </Space>
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
          onChange={changeStatus}
          options={ALL_STATUSES.map((s) => ({ value: s, label: statusLabel(s) }))}
        />
        <Select
          mode="multiple"
          allowClear
          placeholder="级别"
          style={{ minWidth: 140 }}
          value={severities}
          onChange={changeSeverity}
          options={ALL_SEVERITIES.map((s) => ({ value: s, label: s }))}
        />
        <Input.Search
          placeholder="按服务过滤"
          allowClear
          style={{ width: 200 }}
          // 回车才搜，不做每键一查：服务名是精确匹配，
          // 打到一半就查几乎必然无结果，只会把列表闪成空。
          enterButton="搜索"
          onSearch={changeService}
        />
        <div style={{ flex: 1 }} />
        <Button icon={<ReloadOutlined />} onClick={() => refetch()} loading={isFetching}>
          刷新
        </Button>
      </Space>

      <Card styles={{ body: { padding: 0 } }}>
        {/* minHeight 挂在 Card 的 body 上而不是 Table 上：
            antd 的 Table 自带元素没有 min-height 生效的位置，
            挂外层容器才能连「空态」一起撑起来 —— 否则有数据时
            框架正常、一空表又缩回去了，空态比有数据时更矮。 */}
        <div style={{ minHeight: LIST_MIN_HEIGHT }}>
          <Table<IncidentSummary>
            rowKey="id"
            loading={isLoading}
            columns={columns}
            dataSource={data?.incidents ?? []}
            size="small"
            onRow={(rec) =>
              clickableRowProps({
                href: `/incidents/${rec.id}`,
                navigate: nav,
                accent: palette.semantic.info,
              })
            }
            pagination={false}
            // 后端只给 has_more、不给总数（避免失真的 total，M3 手册 §4.3），
            // 因此翻页由下面的「下一页」按钮显式驱动，不用 antd 的分页器 ——
            // 它需要 total 才能算页数。
            locale={{
              emptyText: (
                // 空态垂直居中：否则它贴在表格顶部第一行，
                // 在 minHeight 撑出的框架里显得头重脚轻。
                <div style={{ padding: '120px 0' }}>
                  <span style={{ color: palette.text.tertiary }}>
                    {isLoading ? '加载中…' : '没有符合条件的事件'}
                  </span>
                </div>
              ),
            }}
            rowClassName={() => 'clickable-row'}
          />
        </div>
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