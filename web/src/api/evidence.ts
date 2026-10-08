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

/** ConfidenceBand 是置信度的**展示档位**，只影响配色与文案，不影响数值。 */
export type ConfidenceBand = 'high' | 'medium' | 'low' | 'unassessable'

/** CONFIDENCE_BAND_LABEL 是四档的中文标签。 */
export const CONFIDENCE_BAND_LABEL: Record<ConfidenceBand, string> = {
  high: '强',
  medium: '中',
  low: '弱',
  unassessable: '未评估',
}

/**
 * CONFIDENCE_BAND_THRESHOLD 是「高」与「中」的分界。
 *
 * **为什么不按后端的 coverageBase 档（0.45/0.60/0.75/0.90）分界**：
 * 那些是**覆盖度**的档位，置信度是「档位 × 覆盖度」的合成结果，
 * 两者的分界点不在同一处。前端若照抄后端常量，改后端规则就会连带改前端，
 * 而用户看到的是「0.62 算强还是算中」这种从没被定义过的问题。
 *
 * 取 0.75 的理由：合成后的实际取值集合是
 * {0.45,0.50,0.55,0.60,0.65,0.70,0.75,0.80,0.85,0.90,0.95}，
 * 0.75 把它们切成「偏强的 4 档」与「偏弱的 7 档」，
 * 落在分界两侧的取值都是 0.70 与 0.75，粒度已经是最细的了。
 */
const CONFIDENCE_HIGH_FROM = 0.75

/** CONFIDENCE_MEDIUM_FROM 是「中」与「弱」的分界。 */
const CONFIDENCE_MEDIUM_FROM = 0.6

/**
 * confidenceBand 把置信度数值映射到展示档位。
 *
 * **三条设计约束**（每条都对应一个真实的踩坑形态）：
 *
 * 1. **null 单列一档**，不参与数值比较。
 *    把 null 当 0 会显示成「弱」—— 那等于说这条证据**没有支撑力**，
 *    而真相是「没有基线，没资格判断」。两者在处置上的含义完全相反。
 *
 * 2. **不解析 `confidence_basis` 里的 `（band=strong）` 文本**。
 *    那是给人看的说明文案，不是给程序读的字段。后端改一次措辞
 *    （比如把 band 换成中文），前端的分档就会静默全错，
 *    而且不会有任何报错——颜色还在，只是全变成了同一档。
 *
 * 3. **NaN 与越界值归入「弱」而不是「强」**。
 *    `NaN >= 0.75` 是 false，天然落到 else；但显式处理
 *    是为了让「越界的高值」（比如后端算错成 1.2）不会因为
 *    「越大越可信」的单调假设而被显示成最强。
 */
export function confidenceBand(v: number | null | undefined): ConfidenceBand {
  if (v === null || v === undefined) return 'unassessable'
  if (!Number.isFinite(v)) return 'low'
  if (v >= CONFIDENCE_HIGH_FROM) return 'high'
  if (v >= CONFIDENCE_MEDIUM_FROM) return 'medium'
  return 'low'
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
