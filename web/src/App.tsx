// 应用路由表。
//
// 规格依据：docs/IIP-M3详细开发手册.md §3.1 T3.5（S3 = D8 前端路由 + 布局壳）。
//
// 页面划分对应三条真实的数据入口：
//   /incidents        事件列表（列表 API）
//   /incidents/:id    事件详情（详情 API，含双 Tab 与处置）
//   /reviews          待确认队列（REVIEW API）
//   /alerts告警列表（M1 已有页面，保留不动）
//
// 入库前 App.tsx 是 7 行的 `return <Alerts />`（没有路由、没有壳），
// 三个页面 import 都在但从无入口。

import { Navigate, Route, Routes } from 'react-router-dom'
import Layout from './components/Layout'
import Incidents from './pages/Incidents'
import IncidentDetail from './pages/IncidentDetail'
import Reviews from './pages/Reviews'
import Alerts from './pages/Alerts'

export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/incidents" replace />} />
        <Route path="/incidents" element={<Incidents />} />
        <Route path="/incidents/:id" element={<IncidentDetail />} />
        <Route path="/reviews" element={<Reviews />} />
        {/* M1 的告警页原样保留：它是 S3 之前唯一的页面，
            删掉会让 M1 的验收方式（发送示例告警看列表增长）失效。 */}
        <Route path="/alerts" element={<Alerts />} />
      </Route>
    </Routes>
  )
}