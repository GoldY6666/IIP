// IIP 前端「关联配置」API 封装。
// 后端契约见 internal/api/correlation_config.go：
//   GET /api/v1/correlation/config → 200 {weights:{time,service,label,dependency,historical},
//                                        thresholds:{merge,review}, enabled:bool}
//   PUT /api/v1/correlation/config → 200 同结构
//                                        400 请求体不成形（非法 JSON / 未知字段）
//                                        422 语义非法（权重和≠1 / merge<review）
//
// **PUT 是「以当前配置为底、用请求体覆盖所提供的字段」的部分更新**（后端注释
// 明写）。前端因此可以整份提交而不用自己算差量 —— 但这也意味着
// 「提交一份残缺的 body」会静默保留其余现值，而不是报错。

// 前缀与 token 头统一由 client.ts 提供（M6 S4），不再本文件自带 API_BASE。
import { apiFetch } from './client'

export interface Weights {
  time: number
  service: number
  label: number
  dependency: number
  historical: number
}

export interface Thresholds {
  merge: number
  review: number
}

export interface CorrelationConfig {
  weights: Weights
  thresholds: Thresholds
  enabled: boolean
}

/** 权重和的允许误差，与后端 correlation.weightSumEpsilon 逐位一致。 */
export const WEIGHT_SUM_EPSILON = 0.001

/**
 * ConfigApiError 携带后端的可读错误文案与 HTTP 状态码。
 *
 * 刻意做成**具名错误类型**而不是 `Error`：页面要区分
 * 「422 语义非法（该把后端原文显示给用户）」与
 * 「网络失败 / 503（该提示重试）」，两者的 UI 处置完全不同。
 * 只抛字符串的话，页面只能靠 message.includes 判断 —— 那是靠文案匹配，
 * 后端改一次文案就失效。
 */
export class ConfigApiError extends Error {
  readonly status: number
  /** serverMessage 是后端返回的原文；网络失败时为空串。 */
  readonly serverMessage: string

  constructor(status: number, serverMessage: string, fallback: string) {
    super(serverMessage || fallback)
    this.name = 'ConfigApiError'
    this.status = status
    this.serverMessage = serverMessage
  }
}

/**
 * readError 从失败响应里取出后端的 `{"error":"..."}`。
 *
 * **后端的 error 字段是给用户看的中文文案**（如
 * 「五维权重之和必须为 1.0（允许误差 0.001），当前为 0.9」），
 * 前端不该自己重写一套提示 —— 重写的版本必然与后端的校验口径漂移。
 * 所以这里原样取回交给页面显示。
 *
 * 取不到就返回空串（由调用方降级到自己的通用文案）：
 * 响应体可能不是 JSON（如反代返回 502 的 HTML），强解会抛异常，
 * 把「网络层出错」变成「解析出错」，掩盖真实原因。
 */
async function readError(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown }
    return typeof body.error === 'string' ? body.error : ''
  } catch {
    return ''
  }
}

export async function fetchCorrelationConfig(): Promise<CorrelationConfig> {
  const res = await apiFetch('/correlation/config')
  if (!res.ok) {
    throw new ConfigApiError(res.status, await readError(res), '获取关联配置失败')
  }
  return (await res.json()) as CorrelationConfig
}

export async function putCorrelationConfig(cfg: CorrelationConfig): Promise<CorrelationConfig> {
  const res = await apiFetch('/correlation/config', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(cfg),
  })
  if (!res.ok) {
    throw new ConfigApiError(res.status, await readError(res), '保存关联配置失败')
  }
  return (await res.json()) as CorrelationConfig
}

// --- 校验（镜像后端 internal/correlation.Config.Validate）---
//
// **这是辅助，不是权威**。手册 §4.1.4 第 2 条说得明白：
// 422 在多数情况**不该被前端拦下**（权重和 0.9994 在 UI 上
// 四舍五入显示为 0.999，用户以为「差一点点」，实际值仍在容差内）。
// 前端拦的是「明显不合法」，余下的交给后端 422。
//
// 判据必须与后端逐条一致：任一条规则漏实现，页面就会显示
// 「可以保存」而后端回 422，用户白等一次往返。

export type DimensionKey = keyof Weights

/** 各维度的中文名与语义说明，语义取自 V1.0 §5。 */
export const DIMENSION_META: Record<DimensionKey, { label: string; hint: string }> = {
  time: { label: '时间邻近', hint: '两条告警的发生时刻有多接近' },
  service: { label: '服务相同', hint: '是否来自同一个 service 标签' },
  label: { label: '标签一致', hint: '标签集合的 Jaccard 相似度' },
  dependency: { label: '拓扑依赖', hint: '服务在 config/topology.yaml 里是否连通' },
  historical: { label: '历史共现', hint: '历史上是否反复一起出现' },
}

/** sumWeights 返回五维之和。浮点加法顺序固定，避免各浏览器舍入不一致。 */
export function sumWeights(w: Weights): number {
  return w.time + w.service + w.label + w.dependency + w.historical
}

/**
 * weightsSumVerdict 判定权重和是否落在后端容差内。
 *
 * 边界用 `<=` / `>=` 而非严格不等：**恰好等于容差边界是合法的**，
 * 与后端 `diff > eps || diff < -eps` 的严格不等号一致。
 */
export function weightsSumVerdict(w: Weights): { ok: boolean; sum: number; diff: number } {
  const sum = sumWeights(w)
  const diff = sum - 1
  return { ok: Math.abs(diff) <= WEIGHT_SUM_EPSILON, sum, diff }
}

export function validateConfig(c: CorrelationConfig): string | null {
  const entries: [string, number][] = [
    ['weights.time', c.weights.time],
    ['weights.service', c.weights.service],
    ['weights.label', c.weights.label],
    ['weights.dependency', c.weights.dependency],
    ['weights.historical', c.weights.historical],
    ['thresholds.merge', c.thresholds.merge],
    ['thresholds.review', c.thresholds.review],
  ]
  for (const [name, v] of entries) {
    if (!(v >= 0 && v <= 1)) {
      // 文案对齐后端 fmt.Errorf("%s 必须在 [0,1] 区间，当前为 %g")
      return `${name} 必须在 [0,1] 区间，当前为 ${v}`
    }
  }
  const v = weightsSumVerdict(c.weights)
  if (!v.ok) {
    return `五维权重之和必须为 1.0（允许误差 ${WEIGHT_SUM_EPSILON}），当前为 ${v.sum}`
  }
  if (c.thresholds.merge < c.thresholds.review) {
    return `merge 阈值 (${c.thresholds.merge}) 不能小于 review 阈值 (${c.thresholds.review})`
  }
  return null
}
