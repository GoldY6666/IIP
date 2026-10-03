// 待确认队列的数量轮询。
//
// 为什么单独一个 hook 而不是让侧栏直接用 fetchReviews：
// 侧栏在**每个页面**都在，队列页自己也在拉列表。若壳层再拉一次全量数据，
// 用户每切一次页面就多打一次无用请求 —— 而这里只需要一个数字。
//
// 轮询而非实时推送：S3 不引 WebSocket（无 SSE 端点，且这属于 M4 worker
// 才有数据可推的场景）。30s 一次足够，且用 refetchInterval 只在
// 浏览器标签页可见时生效（react-query 默认行为，后台标签页不轮询）。

import { useQuery } from '@tanstack/react-query'
import { fetchReviews } from './incidents'

export function useReviewQueueCount() {
  const { data } = useQuery({
    queryKey: ['reviews', 'count'],
    queryFn: () => fetchReviews(100, 0),
    refetchInterval: 30_000,
    // 只取 count：这个 query 的返回值只有一个数字被消费，
    // 缓存整个列表是浪费。用 select 收窄，避免每次缓存变动都触发重渲染。
    select: (res) => res.reviews.length,
    // 队列为空是常态而非错误，不该在侧栏冒红字。
    retry: false,
  })
  return { data }
}