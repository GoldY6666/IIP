// UI 运行时配置读取（M5 S2，规格见 docs/IIP-M5详细开发手册.md §4.2.2.1）。
//
// 配置由后端在返回 index.html 时内联成 `window.__IIP_UI__`。
// 为什么走这条路而不是 vite 的 VITE_*：VITE_* 是**构建时**值，
// 改一次面板地址就要重新构建前端并重新部署产物，而面板地址恰恰是
// 最常随环境变化的东西（容器内 prometheus:9090 / 宿主机 localhost:9090 /
// 生产另一套）。详见 Go 侧 internal/webui/uiconfig.go 的说明。

import { DEFAULT_UI_ENDPOINTS, type UIEndpoints } from './externalLinks'

// 全局变量名必须与 Go 侧 uiConfigGlobalName 逐字一致。
// 两处各写一份字面量而不是让前端从 .env 读 —— 那会变成两份配置
// 而不是一个契约，改一处漏一处的症状是「配置设了没生效」。
const GLOBAL_NAME = '__IIP_UI__'

/** 在 Window 上声明注入的全局（dev 模式与单测下不存在）。 */
declare global {
  interface Window {
    __IIP_UI__?: Partial<UIEndpoints>
  }
}

/**
 * uiEndpoints 返回当前生效的面板地址。
 *
 * **必须容忍全局不存在**（三种真实情况都会发生）：
 *   1. vite dev（没有后端注入，只有本模块的默认值）
 *   2. 单元测试（jsdom 里没有注入）
 *   3. 后端启用静态托管但 index.html 没有占位符（用户自定义页面）
 *
 * 这三种情况下都用 DEFAULT_UI_ENDPOINTS（localhost 系列）——
 * 链接仍能拼出来，点了打不开的话会提示检查配置，
 * 比「整个 UI 白屏」好得多。
 *
 * 逐字段回落而不是整体回落：后端只配了 Prometheus 时，
 * Grafana / Loki 仍该用自己的默认值。
 */
export function uiEndpoints(): UIEndpoints {
  const injected = typeof window === 'undefined' ? undefined : window[GLOBAL_NAME]
  if (!injected || typeof injected !== 'object') {
    return { ...DEFAULT_UI_ENDPOINTS }
  }
  return {
    prometheus: pick(injected.prometheus, DEFAULT_UI_ENDPOINTS.prometheus),
    grafana: pick(injected.grafana, DEFAULT_UI_ENDPOINTS.grafana),
    loki: pick(injected.loki, DEFAULT_UI_ENDPOINTS.loki),
    lokiTenantId: pick(injected.lokiTenantId, DEFAULT_UI_ENDPOINTS.lokiTenantId),
  }
}

// pick 取第一个「非空字符串」。
//
// 刻意不写 `injected.x ?? DEFAULT`：`??` 只挡 null/undefined，
// 而注入值可能是空串（环境变量设成 `IIP_UI_GRAFANA_URL=`）——
// 那时 `??` 会把空串当有效值，链接就变成 `/explore`（相对路径）
// 打到 UI 自己的端口上，症状是「点了跳到本应用的 404」。
function pick(v: string | undefined, fallback: string): string {
  return typeof v === 'string' && v.trim() !== '' ? v.trim() : fallback
}
