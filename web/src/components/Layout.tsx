// 应用外壳：顶部栏 + 左侧导航 + 内容区。
//
// 这是 S3 交付的第一块（ADR-016 定的「深色专业控制台 + 路由 + 布局壳」）。
// 壳里刻意只放导航与全局动作，**不放任何业务逻辑**——业务留在各页面，
// 这样换页不会连带重跑列表查询，也不会让「布局」和「数据」互相污染。

import { Link, Outlet, useLocation } from 'react-router-dom'
import { Badge, Layout, Menu, Tag, Typography } from 'antd'
import {
  AlertOutlined,
  ApiOutlined,
  FileSearchOutlined,
  SettingOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons'
import { palette } from '../theme'
import { useReviewQueueCount } from '../api/useReviewQueueCount'

const { Header, Sider, Content } = Layout
const { Text } = Typography

// 导航项。**按「日常操作 → 例外处置 → 系统」排列**：
// 事件/待确认/告警是每天点开的，配置是偶尔调一次的。
// 把配置放最后而不是插在最前，是因为它不是高频入口。
const NAV = [
  { key: '/incidents', icon: <AlertOutlined />, label: <Link to="/incidents">事件</Link> },
  { key: '/reviews', icon: <FileSearchOutlined />, label: <Link to="/reviews">待确认</Link> },
  { key: '/alerts', icon: <ApiOutlined />, label: <Link to="/alerts">告警</Link> },
  { key: '/settings', icon: <SettingOutlined />, label: <Link to="/settings">配置</Link> },
]

export default function Layout_() {
  const loc = useLocation()
  // 高亮当前页：以 pathname 前段匹配，避免详情页 /incidents/12 时高亮丢失。
  const selected = NAV.map((n) => n.key).find((k) => loc.pathname.startsWith(k)) ?? '/incidents'

  // 待确认数量直接显示在导航上：这是 S3 唯一「在壳层就需要知道」的全局数字 ——
  // 有待处理项时用户不必先进队列页才知道要干活。
  const { data: pending } = useReviewQueueCount()

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Header
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          paddingInline: 16,
          borderBottom: `1px solid ${palette.surface.border}`,
        }}
      >
        <ThunderboltOutlined style={{ color: palette.semantic.info, fontSize: 18 }} />
        <Text strong style={{ color: palette.text.primary, fontSize: 15 }}>
          IIP
        </Text>
        <Text style={{ color: palette.text.tertiary, fontSize: 12 }}>
          Incident Intelligence Platform
        </Text>
        <div style={{ flex: 1 }} />
        <Tag color="default" style={{ marginInlineEnd: 0 }}>
          M5 Web 控制台
        </Tag>
      </Header>

      <Layout>
        <Sider width={176} theme="dark" style={{ borderRight: `1px solid ${palette.surface.border}` }}>
          <Menu
            theme="dark"
            mode="inline"
            selectedKeys={[selected]}
            items={NAV.map((n) =>
              n.key === '/reviews' && pending ? { ...n, label: <PendingLabel count={pending} /> } : n,
            )}
            style={{ borderInlineEnd: 'none', paddingTop: 8 }}
          />
        </Sider>

        <Content style={{ padding: 20, overflow: 'auto' }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}

function PendingLabel({ count }: { count: number }) {
  return (
    // 徽标放在标签右侧而非文字里挤着：数字是**待人工判定的关联边条数**
    // （队列维度），与事件列表里的「待处理边」不是一个数 —— 那个是单个
    // 事件内的边数。两个数分开摆放，避免被当成同一个数在两处显示却不一致。
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      待确认
      <Badge
        count={count}
        overflowCount={99}
        style={{ backgroundColor: palette.semantic.warning }}
      />
    </span>
  )
}