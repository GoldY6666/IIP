// 外部面板 URL 拼装（M5 S2，规格见 docs/IIP-M5详细开发手册.md §4.2）。
//
// 定位：把一条 evidence 的 `source` / `query` / `timestamp` / `metadata.window`
// 翻译成「在原始面板里打开这条查询」的链接。
//
// **为什么不放后端**：链接是给**人在浏览器里点**的。浏览器解析的是宿主机
// 地址，而 evidence 行的 `source`（prometheus/loki/k8s）只是存储层的
// 英文常量，不带任何地址信息 —— 地址是**部署形态**的事，同一份产物在
// 容器内、宿主机裸跑、生产三处的答案不同。后端只知道自己配的
// `IIP_PROM_URL`（那是**采集器**用的地址，可能是 `prometheus:9090`），
// 而用户浏览器该用 `localhost:9090`。用采集器地址拼给人点的链接 =
// 给出点不开的链接。故地址必须是**独立的 UI 侧配置**。

import type { Evidence } from './evidence'

/** UIEndpoints 是「人在浏览器里点」用的各面板根地址。 */
export interface UIEndpoints {
  prometheus: string
  grafana: string
  loki: string
  /** Loki 多租户时的 tenant id；空表示单租户。 */
  lokiTenantId: string
}

// DEFAULT_UI_ENDPOINTS 是默认值（手册 §4.2.2）。
//
// **取 localhost 而不是 compose 服务名**：UI 链接是给人在浏览器里点的，
// 浏览器解析的是宿主机地址，不是容器网络地址。compose 已声明宿主映射端口
// 9090/3000/3100，故 localhost 对默认部署开箱可用。
//
// 反过来若默认填 `prometheus:9090`，则「链接能拼出来但点了打不开」——
// 那种失败比「不渲染链接」更糟：用户会以为是 Grafana 挂了。
export const DEFAULT_UI_ENDPOINTS: UIEndpoints = {
  prometheus: 'http://localhost:9090',
  grafana: 'http://localhost:3000',
  loki: 'http://localhost:3100',
  lokiTenantId: '',
}

// SOURCE_PROMETHEUS / SOURCE_LOKI 必须与 internal/collectors 的常量逐字一致。
//
// 不用 `import type` 从后端拿（后端是 Go，没有 TS 类型可导入）；
// 只能在此处硬编码并在注释里指明来源。改后端常量时这里必须一起改。
const SOURCE_PROMETHEUS = 'prometheus'
const SOURCE_LOKI = 'loki'

/** ExternalLink 是一条可点击的外部面板链接。 */
export interface ExternalLink {
  href: string
  /** 链接文字，直接显示在按钮上。 */
  label: string
  /** title 提示（悬停可见），说明点开会看到什么、以及前置条件。 */
  title: string
}

// trimRoot 去掉根地址末尾的斜杠。
//
// 不做这件事的话 `http://host:9090/` + `graph?...` 会拼出
// `http://host:9090//graph?...` —— 多一个斜杠，Prometheus 多数能容忍，
// 但 Grafana 的 Explore 路由对双斜杠会 404。
function trimRoot(u: string): string {
  return u.replace(/\/+$/, '')
}

// parseWindow 解析 metadata.window（形如 `2026-10-04T10:00:00Z/2026-10-04T11:00:00Z`）。
//
// 返回 null 表示「不能用」，此时调用方应退回 evidence.timestamp。
// 不抛异常：metadata 是 `Record<string,string>`，后端契约不保证 window 存在
// （只有三个采集器都写，但 M4 前的历史数据没有；将来加新采集器也未必写）。
const parseWindow = (w: string | undefined): { from: string; to: string } | null => {
  if (!w) return null
  const parts = w.split('/')
  // 严格两段：形如 "a/b/c" 说明格式变了，宁可不解析也别错解析
  if (parts.length !== 2) return null
  const [from, to] = parts
  if (!from || !to) return null
  if (Number.isNaN(Date.parse(from)) || Number.isNaN(Date.parse(to))) return null
  return { from, to }
}

/** buildPrometheusLink 拼 Prometheus 表达式页链接。 */
function buildPrometheusLink(e: Evidence, ep: UIEndpoints): ExternalLink | null {
  const q = e.query.trim()
  if (!q) return null

  const w = parseWindow(e.metadata?.window)
  const sp = new URLSearchParams()
  // 0 号图元：新标签页打开的图元序号
  sp.set('g0.expr', q)
  // 时间范围：优先用采集窗口（两端都有），退回单点 timestamp。
  if (w) {
    sp.set('g0.range_input', '1')
    sp.set('g0.end_input', w.to)
    sp.set('g0.start_input', w.from)
  } else {
    sp.set('g0.end_input', e.timestamp)
  }

  return {
    href: `${trimRoot(ep.prometheus)}/graph?${sp.toString()}`,
    label: '在 Prometheus 打开',
    title: '用同一条 PromQL 打开 Prometheus 表达式页面，可直接确认曲线形状',
  }
}

/**
 * buildLokiLink 拼 Grafana Explore 链接。
 *
 * Loki 的原生界面只适合看原始日志流，实际排查几乎都在 Grafana 里做
 * （下钻、过滤、看模式切换），所以 loki 证据也指向 Grafana Explore。
 *
 * `left` 参数是 Grafana 自定义的老式 JSON 状态串，内部又含一层
 * datasource UID。这里用 URLSearchParams 整体编码 —— 手工拼
 * `{"` 与 `"` 一定拼出坏链接，且 PromQL 里的 `{`/`}` 会提前截断参数。
 */
function buildLokiLink(e: Evidence, ep: UIEndpoints): ExternalLink | null {
  const q = e.query.trim()
  if (!q) return null

  const w = parseWindow(e.metadata?.window)
  const range = w
    ? { from: w.from, to: w.to }
    : { from: e.timestamp, to: e.timestamp }

  // datasource: 用 uid 形式。Grafana 默认 Loki 数据源 uid 常常是 'loki'，
  // 但不同安装会不同 —— 传错时 Grafana 会回退到默认数据源而不是报错，
  // 表现为「打开了但查的是别的源」，因此在 title 里说明这一点。
  const left = {
    datasource: ep.lokiTenantId ? `Loki-${ep.lokiTenantId}` : 'Loki',
    queries: [
      {
        refId: 'A',
        expr: q,
        queryType: 'range',
        range: { from: range.from, to: range.to },
      },
    ],
    range: { from: range.from, to: range.to },
  }

  const sp = new URLSearchParams()
  sp.set('left', JSON.stringify(left))

  return {
    href: `${trimRoot(ep.grafana)}/explore?${sp.toString()}`,
    label: '在 Grafana 打开',
    title: '在 Grafana Explore 打开同一条 LogQL（需 Grafana 可达；数据源 UID 与本环境可能不同）',
  }
}

/** buildLokiDirectLink 拼 Loki 原生查询链接。 */
function buildLokiDirectLink(e: Evidence, ep: UIEndpoints): ExternalLink | null {
  const q = e.query.trim()
  if (!q) return null

  const w = parseWindow(e.metadata?.window)
  const sp = new URLSearchParams()
  sp.set('query', q)
  if (w) {
    // Loki 原生接口的 start/end 收**毫秒时间戳**（不是 RFC3339）。
    // 写成 RFC3339 会被 Loki 当作 0（解析失败）而返回空结果 ——
    // 症状是「链接能点开但什么都没有」，比链接打不开更难判断原因。
    sp.set('start', String(Date.parse(w.from)))
    sp.set('end', String(Date.parse(w.to)))
    sp.set('direction', 'backward')
  } else {
    // 无 window 时退回 timestamp ±1h。给个窗口而不是单点：
    // 用户点「打开 Loki」就是想看这段时间的日志，一个时间点几乎必然是空的。
    const ts = Date.parse(e.timestamp)
    sp.set('start', String(ts - 3600_000))
    sp.set('end', String(ts + 3600_000))
    sp.set('direction', 'backward')
  }
  if (ep.lokiTenantId) sp.set('tenant', ep.lokiTenantId)

  return {
    href: `${trimRoot(ep.loki)}/loki/api/v1/query_range?${sp.toString()}`,
    label: '在 Loki 打开',
    title: '在 Loki 原生界面查询同一条 LogQL（返回原始 JSON，适合确认数据是否存在）',
  }
}

/**
 * buildExternalLinks 为一条证据产出可点击的外部面板链接。
 *
 * 返回**数组**而非单个：日志类证据值得同时给出 Grafana（看图、下钻）
 * 与 Loki 原生（确认数据）在手边 —— 两个面板回答的是不同问题。
 * 只有一个面板可用时，运维得自己猜另一个在哪。
 *
 * 返回空数组表示「不渲染链接」。调用方**必须**据此隐藏链接控件：
 * 渲染一个打不开的按钮比没有按钮更糟（用户会以为面板挂了）。
 */
export function buildExternalLinks(
  e: Evidence,
  ep: UIEndpoints = DEFAULT_UI_ENDPOINTS,
): ExternalLink[] {
  const src = (e.source || '').trim().toLowerCase()

  switch (src) {
    case SOURCE_PROMETHEUS:
      return [buildPrometheusLink(e, ep)].filter(Boolean) as ExternalLink[]
    case SOURCE_LOKI: {
      const grafana = buildLokiLink(e, ep)
      const direct = buildLokiDirectLink(e, ep)
      return [grafana, direct].filter(Boolean) as ExternalLink[]
    }
    default:
      // k8s / 其他来源：没有通用「打开原始面板」的概念。
      // 手册 §4.2.4 明确不渲染 —— 与其给一个语义不明的链接，
      // 不如把 query 原文摆出来（M3 已有复制按钮）。
      return []
  }
}
