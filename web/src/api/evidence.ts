// IIP 证据（Evidence）相关 API 封装。
//
// 本文件是**前端与后端契约的唯一副本**，字段名逐字对齐
// internal/api/evidence.go 的 json tag。
//
// 与 incidents.ts 同一纪律：不要凭印象改字段名。字段对不上时页面
// 不会报错，只会静默显示空值 —— 而「显示 0 条证据」与「没有证据」
// 在页面上长得几乎一样（详见 incidents.ts 头部注释）。
//
// 三个最容易踩的字段，各有专门理由：
//   - confidence 是 `number | null`，null 表示「采集器没有这个判断能力」
//     （OPEN-M4-1），**不能当 0 用**：0 会被读成「这条证据明确没有支撑力」。
//   - timestamp 是**证据自身发生的时间**，collected_at 才是落库时刻。
//     补采一条三小时前的日志时两者差三小时，合成一个字段会让人
//     以为「这条证据是三小时前发生的」。
//   - count 是**本次返回的条数**，total 才是真计数。两者混用就是
//     M3 的 AlertCount 缺陷重演（250 条显示成 200 条）。

import { readError } from './incidents'
// 前缀与 token 头统一由 client.ts 提供（M6 S4），不再本文件自带API_BASE。
import { apiFetch } from './client'

// EvidenceType 的取值必须与 internal/evidence 的 6 个常量一致
// （0001 迁移的 chk_evidence_type 约束）。
export const ALL_EVIDENCE_TYPES = [
  'metric',
  'log',
  'trace',
  'deployment',
  'config_change',
  'historical',
] as const
export type EvidenceType = (typeof ALL_EVIDENCE_TYPES)[number]

// EVIDENCE_TYPE_LABEL 把类型码翻成中文。
//
// **没有后端提供这份映射**：类型是存储层的英文常量，
// 硬编码在前端就等于把契约抄一份 —— 后端加一个类型时前端不会报错，
// 只会显示英文原值。所以映射与 ALL_EVIDENCE_TYPES 放在一起，
// 改类型时两处一起改。
export const EVIDENCE_TYPE_LABEL: Record<EvidenceType, string> = {
  metric: '指标',
  log: '日志',
  trace: '链路追踪',
  deployment: '部署记录',
  config_change: '配置变更',
  historical: '历史共现',
}

// Evidence 是响应里的一条证据（internal/api.evidenceBody）。
export interface Evidence {
  id: number
  type: EvidenceType
  // source 是外部系统名（prometheus / loki / k8s），不是显示用的中文。
  source: string
  // query 是**可复制复现**的查询串（后端保证非空）。
  // 运维排查时要能直接粘到 Grafana / Loki 里重跑，所以原样展示不加工。
  query: string
  value: string
  // timestamp 是证据自身的时间（如 Prometheus 样本时间戳、K8s 事件
  // 发生时刻），**不是采集时刻**。
  timestamp: string
  // null = 未评估（采集器没有这个判断能力），不是 0。
  confidence: number | null
  // metadata 保证是对象而非 null：后端已归一化，
  // 直接 Object.keys 不会抛错。
  metadata: Record<string, string>
  // collected_at 是落库时刻，与 timestamp 差值即「证据有多新」的判断依据。
  collected_at: string
}

export interface EvidenceListResponse {
  evidence: Evidence[]
  // count 是本次返回的条数（= evidence.length）。
  count: number
  // total 是未被截断前的真计数（SQL count(*)）。
  total: number
  // truncated 为 true 表示超限被截断，**必须回显**：
  // 把「只保留了 50 条」说成「一共 50 条」会让用户以为没有更多证据。
  truncated: boolean
  // limit 是本次生效的上限，用于显示「仅显示最近 N 条」。
  limit: number
  // by_type 覆盖全部 6 种类型（含 0 条的），供类型筛选器的角标。
  // 统计失败时后端给全 0 而不是空对象 —— 空对象会让前端把
  // 「统计失败」与「真的是 0」渲染成同一个样子。
  by_type: Record<string, number>
  // type 是本次生效的类型过滤（空串 = 不过滤）。
  type?: string
}

export interface ListEvidenceParams {
  type?: EvidenceType
  limit?: number
}

export async function fetchEvidence(
  incidentId: number,
  params: ListEvidenceParams = {},
): Promise<EvidenceListResponse> {
  // 手工拼而不是复用 incidents.ts 的 buildQuery：
  // 那个是模块私有的，为一个参数把它导出等于扩大耦合面。
  const sp = new URLSearchParams()
  if (params.type) sp.append('type', params.type)
  if (params.limit !== undefined) sp.append('limit', String(params.limit))
  const qs = sp.toString()

  const res = await apiFetch(`/incidents/${incidentId}/evidence${qs ? `?${qs}` : ''}`)
  if (!res.ok) throw await readError(res, `获取事件 #${incidentId} 的证据失败`)
  return (await res.json()) as EvidenceListResponse
}
