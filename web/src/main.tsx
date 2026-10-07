import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { App as AntApp } from 'antd'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import { ThemeModeProvider } from './themeMode'
import { fetchAuthSession } from './api/client'
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

// 树根元素。抽成常量是因为下面要挂 loading 态 ——
// 重复 document.getElementById('root') 有「后一次拿到 null」的余地，
// 而 null 传给 createRoot 报的是 react-dom 内部的错，很难反推到这里。
const rootEl = document.getElementById('root') as HTMLElement

// 鉴权令牌必须在**渲染之前**取好（M6 S4）。
//
// 为什么不能等挂载后再取：页面组件的查询 effect 与本函数是并发的，
// 若在 React 里用 useEffect 取 token，首批请求会不带 token 地打出去 →
// 全部 401，页面先闪一次错误再恢复。症状是「打开页面先红一下再正常」，
// 而真因是启动顺序，排查方向会被完全带偏。
//
// 失败不阻断启动：fetchAuthSession 内部已吞掉异常，失败的表现是
// 后续请求 401 —— 那是可读的业务错误，比「因为探测失败而白屏」好。
function render(): void {
  ReactDOM.createRoot(rootEl).render(
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
}

// 未启用鉴权（本机默认）时 fetchAuthSession 仍会打一次 /auth/session ——
// 后端返回 200 + auth_required=false，一次本地往返，可接受。
// 换来的是「前端不需要预知服务端是否配置了 token」这一性质。
void fetchAuthSession().then(render)