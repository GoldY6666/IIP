// IIP 事件（Incident）相关 API 封装。
//
// 本文件是**前端与后端契约的唯一副本**，字段名逐字对齐
// internal/api/incidents.go 与 internal/api/reviews.go 的 json tag。
//
// 不要凭印象改字段名：后端在 2026-10-03 修过一次真实缺陷 —— 领域结构体漏写
// json tag 时 Go 会退回用字段名做键（`AlertID` 而非 `alert_id`），
// handler 层的 round-trip 测试**测不出来**（同一套结构体自洽），
// 只有真实进程的字节流才暴露。前端这边同理：字段名对不上，页面不会报错，
// 只会静默显示空值。所以这里逐字段标注了后端出处。

const API_BASE = '/api/v1'

// ---- 领域类型（与 internal/incident 的 json tag 一一对应）----

// IncidentStatus 是六个合法状态，与 0004 迁移的 CHECK 约束一致。
// ladder（阶序）是判定合法转换的唯一真源，见 canTransition。
export type IncidentStatus =
  | 'open'
  | 'acknowledged'
  | 'investigating'
  | 'mitigated'
  | 'resolved'
  | 'ignored'

// ALL_STATUSES 顺序**必须**与后端 ladder 一致（下标即阶序）。
// 顺序错了会让「只许前向」的判定在前端给出与后端相反的按钮布局。
export const ALL_STATUSES: IncidentStatus[] = [
  'open',
  'acknowledged',
  'investigating',
  'mitigated',
  'resolved',
  'ignored',
]

export const ALL_SEVERITIES = ['critical', 'warning', 'info'] as const
export type Severity = (typeof ALL_SEVERITIES)[number]

// Summary 是列表页一行的形状（internal/incident.Summary）。
export interface IncidentSummary {
  id: number
  title: string
  severity: string
  status: IncidentStatus
  service: string
  started_at: string
  // resolved_at 可空：未 resolved 时为 null。注意 ignored 也是 null
  //（进入 ignored 刻意不写该列，见后端 ResolveAtEffect）。
  resolved_at: string | null
  updated_at: string
  alert_count: number
  needs_review_count: number
}

// AlertLink 是详情页一条关联告警（internal/incident.AlertLink）。
export interface AlertLink {
  alert_id: number
  fingerprint: string
  severity: string
  labels: Record<string, string>
  starts_at: string
  ends_at: string | null
  // correlation_score 可空：NEW 档（该告警是事件起因）按设计为 NULL，
  // 前端必须显示「—」而不是 0 —— 0 会被读成「评分为零」，是另一回事。
  correlation_score: number | null
  needs_review: boolean
  correlated_at: string
}

// TimelineEntry 是一条留痕（internal/incident.Entry）。
export interface TimelineEntry {
  id?: number
  event_type: string
  actor: string
  metadata: Record<string, string>
  timestamp: string
}

// IncidentDetail 是详情聚合体（internal/api.incidentDetailResponse）。
export interface IncidentDetail {
  incident: IncidentSummary
  alerts: AlertLink[]
  timeline: TimelineEntry[]
  // truncated 必须显式读：后端只内联最近 200 条，被截断时回 true。
  // 吞掉它会让用户以为看到的就是全部（后端手册 §4.3 明确要求前端提示）。
  truncated: { alerts: boolean; timeline: boolean }
}

export interface IncidentListResponse {
  incidents: IncidentSummary[]
  count: number
  has_more: boolean
}

// ReviewEdge 是待确认队列的一条边（internal/incident.ReviewEdge）。
export interface ReviewEdge {
  incident_id: number
  alert: AlertLink
}

export interface ReviewListResponse {
  reviews: ReviewEdge[]
  count: number
  has_more: boolean
}

// ReviewResult 是一次复核的结果（internal/incident.ReviewResult）。
export interface ReviewResult {
  decision: 'confirm' | 'reject'
  incident_id: number
  alert_id: number
  // reject 时才可能为 true —— 用来告诉用户「这条告警已从事件中摘掉」
  // 或「同类告警以后不再提示」。
  suppressed: boolean
  unlinked: boolean
}

// ---- 请求封装 ----

// readError 从失败响应里抽出可读文案。
//
// 后端错误体是 {"error": "...", "code": "..."}；但反向代理或容器返回的
// 可能不是 JSON，所以 text() 失败时退回状态码 —— 宁可少说，也不能
// 让「解析失败」这个二次错误盖掉真实原因。
export async function readError(res: Response, what: string): Promise<Error> {
  const text = await res.text().catch(() => '')
  let detail = text
  try {
    const parsed = JSON.parse(text) as { error?: string; code?: string }
    detail = parsed.error ?? text
  } catch {
    // 保留原始 text
  }
  return new Error(`${what}：HTTP ${res.status}${detail ? ` ${detail}` : ''}`)
}

// readError 已导出：evidence.ts 也要用它，两处各写一份会让错误文案漂移。
// 错误文案是用户唯一能看到的失败原因，漂移等于用户看到两种说法。

// buildQuery 拼查询串，自动跳过空值。
//
// status 支持重复 key 与逗号分隔两种写法（后端 parseIncidentFilter 两者都收），
// 这里用重复 key —— URL 更短，且和后端注释里说的「多选控件」一致。
function buildQuery(params: Record<string, string | number | string[] | undefined>): string {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined) continue
    if (Array.isArray(v)) {
      v.forEach((item) => item && sp.append(k, item))
    } else if (v !== '') {
      sp.append(k, String(v))
    }
  }
  const q = sp.toString()
  return q ? `?${q}` : ''
}

export interface ListIncidentsParams {
  status?: IncidentStatus[]
  severity?: string[]
  service?: string
  from?: string
  to?: string
  limit?: number
  offset?: number
}

export async function fetchIncidents(params: ListIncidentsParams = {}): Promise<IncidentListResponse> {
  const qs = buildQuery({
    status: params.status,
    severity: params.severity,
    service: params.service,
    from: params.from,
    to: params.to,
    limit: params.limit,
    offset: params.offset,
  })
  const res = await fetch(`${API_BASE}/incidents${qs}`)
  if (!res.ok) throw await readError(res, '获取事件列表失败')
  return (await res.json()) as IncidentListResponse
}

export async function fetchIncident(id: number): Promise<IncidentDetail> {
  const res = await fetch(`${API_BASE}/incidents/${id}`)
  if (!res.ok) throw await readError(res, `获取事件 #${id} 详情失败`)
  return (await res.json()) as IncidentDetail
}

export interface TransitionResult {
  incident: IncidentSummary
}

export async function transitionIncident(
  id: number,
  to: IncidentStatus,
  actor: string,
  note?: string,
): Promise<TransitionResult> {
  const res = await fetch(`${API_BASE}/incidents/${id}/transition`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ to, actor, note }),
  })
  if (!res.ok) throw await readError(res, `流转事件 #${id} 失败`)
  return (await res.json()) as TransitionResult
}

export async function addIncidentComment(id: number, actor: string, note: string): Promise<void> {
  const res = await fetch(`${API_BASE}/incidents/${id}/comments`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ actor, note }),
  })
  if (!res.ok) throw await readError(res, `添加备注失败`)
}

export async function fetchReviews(limit = 100, offset = 0): Promise<ReviewListResponse> {
  const qs = buildQuery({ limit, offset })
  const res = await fetch(`${API_BASE}/reviews${qs}`)
  if (!res.ok) throw await readError(res, '获取待确认队列失败')
  return (await res.json()) as ReviewListResponse
}

export async function decideReview(
  incidentID: number,
  alertID: number,
  decision: 'confirm' | 'reject',
  actor: string,
  reason?: string,
): Promise<ReviewResult> {
  const res = await fetch(`${API_BASE}/reviews/${incidentID}/${alertID}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ decision, actor, reason }),
  })
  if (!res.ok) throw await readError(res, '复核失败')
  return (await res.json()) as ReviewResult
}