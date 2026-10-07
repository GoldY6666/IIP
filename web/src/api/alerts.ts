// IIP 前端告警相关 API 封装。
// 后端契约见 internal/api/alert_list.go 与 internal/api/alert_webhook.go：
//   GET  /api/v1/alerts?limit=&offset=  → { alerts: IIPAlert[], count: number }
//   POST /api/v1/alerts/webhook         → 告警接入（Alertmanager 格式）

export interface IIPAlert {
  id: number
  fingerprint: string
  source: string
  severity: string
  status: string
  labels: Record<string, string>
  annotations: Record<string, string>
  starts_at: string
  ends_at: string | null
  raw_payload: unknown
  incident_id: number | null
}

export interface AlertListResponse {
  alerts: IIPAlert[]
  count: number
}

// 前缀与 token 头统一由 client.ts 提供（M6 S4），此处不再各写一份
// '/api/v1' —— 四份字面量意味着「加头」要改四个地方，漏一处无编译错误。
import { apiFetch } from './client'

// 拉取最新一批告警（默认 100 条、不做 status 过滤，与后端一致）。
export async function fetchAlerts(limit = 100, offset = 0): Promise<AlertListResponse> {
  const res = await apiFetch(`/alerts?limit=${limit}&offset=${offset}`)
  if (!res.ok) {
    throw new Error(`获取告警列表失败：HTTP ${res.status}`)
  }
  return (await res.json()) as AlertListResponse
}

// 一条贴近手册 §4.1 样例的示例告警，供「发送示例告警」按钮使用。
//
// 注意：IIP 自算指纹时会排除 instance/pod/container/node 等动态标签，
// 因此用 demo_id 作为每次唯一的指纹成分 —— 每次点击都会产生一条新告警，
// 便于直观看到列表增长（而不是被去重判为重发而 skipped）。
// endsAt 用 Alertmanager 的「尚无结束时间」哨兵值，落库后 ends_at 为 NULL，
// 前端据此显示「持续中」。
const buildSamplePayload = () => ({
  version: '4',
  groupKey: 'namespace:production/alertname:HighLatency',
  status: 'firing',
  receiver: 'iip-webhook',
  groupLabels: { alertname: 'HighLatency', namespace: 'production' },
  commonLabels: { service: 'checkout', team: 'platform' },
  commonAnnotations: { summary: 'High latency demo' },
  alerts: [
    {
      status: 'firing',
      labels: {
        alertname: 'HighLatency',
        service: 'checkout',
        instance: 'checkout-demo',
        namespace: 'production',
        severity: 'critical',
        demo_id: `demo-${Date.now()}`,
      },
      annotations: { summary: 'High latency on checkout', value: '1250ms' },
      startsAt: new Date().toISOString(),
      endsAt: '0001-01-01T00:00:00Z',
    },
  ],
})

export async function sendSampleAlert(): Promise<void> {
  // 走 apiFetch：webhook 也在 /api/v1 下，启用鉴权后同样要带 token。
  // 这里曾经是「示例按钮能用但列表空白」的隐患来源 —— 两条路径的前缀相同，
  // 很容易只改一处。
  const res = await apiFetch('/alerts/webhook', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(buildSamplePayload()),
  })
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    throw new Error(`发送示例告警失败：HTTP ${res.status} ${text}`)
  }
}
