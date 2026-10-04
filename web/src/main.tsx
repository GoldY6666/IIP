import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App as AntApp } from 'antd'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import { ThemeModeProvider } from './themeMode'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // 不自动重试：事件列表是只读查询，失败时让用户看到错误并手动刷新，
      // 默默重试三次只会让「后端挂了」看起来像「加载慢」。
      retry: false,
      // 事件详情在流转后由失效机制主动刷新，无需窗口聚焦重取 ——
      // 值守时切回标签页就重新拉一次会让页面无谓地闪。
      refetchOnWindowFocus: false,
      staleTime: 10_000,
    },
  },
})

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    {/* ThemeModeProvider 必须在最外层：它自己包了 ConfigProvider，
        而 17 处内联 style 的 `palette.*` 读的是它同步维护的活引用。
        放到 QueryClientProvider 里面会晚一层，不影响功能但不符合
        「外观设置是全局的」这个语义。 */}
    <ThemeModeProvider>
      {/* AntApp 提供 message/notification/modal 的 Context，供页面内 App.useApp() 调用 */}
      <AntApp>
        <QueryClientProvider client={queryClient}>
          {/* BrowserRouter 必须在 ConfigProvider 内层无要求，但放在这里
              与 QueryClientProvider 同级：两者的 Provider 顺序无所谓，
              写在一起便于一眼看全全局 Provider。 */}
          <BrowserRouter>
            <App />
          </BrowserRouter>
        </QueryClientProvider>
      </AntApp>
    </ThemeModeProvider>
  </React.StrictMode>,
)