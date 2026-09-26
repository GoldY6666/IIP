import { Typography } from 'antd'

const { Title, Paragraph } = Typography

/**
 * M0 占位页面。
 * 本阶段禁止编写任何业务页面（AGENTS.md 约束 2 / 11），仅验证前端脚手架可渲染。
 */
export default function App() {
  return (
    <div style={{ padding: 48 }}>
      <Title level={3}>IIP 控制台 · M0 开发中</Title>
      <Paragraph type="secondary">
        Incident Intelligence Platform —— 项目骨架已就绪，业务功能将在后续里程碑接入。
      </Paragraph>
    </div>
  )
}
