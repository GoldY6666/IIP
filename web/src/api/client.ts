// API 客户端：统一前缀与 token 头（M6 S4，规格见 docs/IIP-M6详细开发手册.md §八裁定 #2）。
//
// 为什么抽这一层：改造前 `/api/v1` 这个前缀在 4 个文件里各写一份字面量，
// 11 处 `fetch` 直接裸调。token 要加在**每一个**请求上，漏一处就是
// 「某个页面永远 401」——症状是局部的（别的页面正常），
// 逐个排查要读 11 个调用点。集中之后「加头」只有一处实现。
//
// 刻意不封装「解析响应体」与「错误转换」：各调用点要读的形状不同
// （列表/详情/空响应），且既有 `readError` 与 `ConfigApiError` 已在做错误文案，
// 再包一层只会让错误路径变深、让调用点更难读。本模块只管
// **前置**：拼前缀 + 加头。

/** API 前缀。与 Go 侧注册的所有 /api/v1 路由一致。 */
const API_BASE = '/api/v1'

/**
 * 访问 token。**模块级状态**，由 `fetchAuthSession()` 在应用启动时写入。
 *
 * 用模块变量而不是在每个调用点传参：token 对所有端点相同，
 * 让 11 个公开函数各自多一个参数会把签名变成噪音，
 * 而漏传的那个不会有编译错误 —— 症状是「某个页面 401」。
 */
let authToken = ''

/** 服务端是否已启用鉴权。`fetchAuthSession()` 写入，供 UI 决定要不要提示。 */
let authRequired = false

/**
 * 返回当前生效的 token（空串表示未启用鉴权）。
 *
 * 导出是为了让测试与 UI 能读到它，而不必去猜「请求到底带没带头」。
 */
export function currentToken(): string {
  return authToken
}

/** 返回服务端是否要求 token。 */
export function authIsRequired(): boolean {
  return authRequired
}

/**
 * 仅供测试：重置模块级状态。
 *
 * 生产代码不该调它。存在的原因是模块级状态在单测间会串味——
 * A 用例设置了 token，B 用例就莫名其妙带上头，于是「不带头也能过」
 * 这条断言在 B 里恒真。**没有这个函数，单测就测不出「该加头时加了没」。**
 */
export function __resetAuthForTest(): void {
  authToken = ''
  authRequired = false
}

/**
 * 读取服务端会话：拿到「是否启用鉴权」与 token 本身。
 *
 * 走的是 `GET /api/v1/auth/session` —— 该端点是唯一**不受鉴权保护**的
 * /api/v1 端点（否则启用鉴权后前端永远拿不到 token，形成死锁）。
 *
 * 失败时**不抛**：调用方（App 启动流程）不该因为「探测失败」
 * 就白屏 —— 那会让一个非关键的探测决定整个 UI 能否打开。
 * 失败的表现是后续请求 401，错误信息已经足够定位。
 */
export async function fetchAuthSession(): Promise<void> {
  try {
    // 刻意**不用** apiFetch：这个请求发生在 token 还没拿到的时候，
    // 用它反而多一层「加不了头」的特殊处理。
    const res = await fetch(`${API_BASE}/auth/session`)
    if (!res.ok) return
    const body = (await res.json()) as { auth_required?: unknown; token?: unknown }
    authRequired = body.auth_required === true
    // 归一化口径与后端一致：非字符串一律当「没配」。
    // 后端回传的是 TrimSpace 后的值，这里不再二次 trim，
    // 免得前后端各自修一次边角、规则又对不上。
    authToken = typeof body.token === 'string' ? body.token : ''
  } catch {
    // 网络错误、JSON 解析失败等：保持「未启用」状态往下走。
  }
}

/**
 * apiFetch 是所有 /api/v1 请求的唯一出口。
 *
 * 行为差异只有两处，其余与裸 `fetch(url, init)` 完全一致：
 *   1. 路径以 `/` 开头时自动补 `API_BASE`；已带前缀的原样使用。
 *   2. token 非空时注入 `X-IIP-Token` 头。
 *
 * **调用方仍然要自己判 `res.ok`**：本函数不抛错，因为各端点对
 * 失败的处理完全不同（有的抛 Error，有的抛带 code 的专用错误），
 * 统一在这里抛会把它们压成一种，丢掉「401 该提示重新配置」
 * 与「500 该看后端日志」的区分。
 */
export async function apiFetch(path: string, init?: RequestInit): Promise<Response> {
  const url = joinAPIPath(path)
  if (!authToken) {
    // 未启用鉴权时**不**发这个头：多发一个空值头会让
    // 「有没有配 token」这件事在前端侧也变成两义（发空头 vs 不发）。
    return fetch(url, init)
  }
  const headers = new Headers(init?.headers)
  headers.set('X-IIP-Token', authToken)
  return fetch(url, { ...init, headers })
}

/**
 * joinAPIPath 拼出最终 URL。
 *
 * 三条规则，缺一条就会出现「静默 404 且 URL 看着像对的」：
 *   - 绝对地址（http/https）原样返回；
 *   - 已带 `/api/v1` 前缀的原样返回 —— 否则会拼成 `/api/v1/api/v1/…`；
 *   - 其余以 `/` 开头的补上前缀。
 *
 * 第二条在当前 11 个调用点**没有一个会走到**（它们都传相对路径），
 * 但 apiFetch 是导出函数，将来有人写 `apiFetch('/api/v1/incidents')`
 * 时踩中的表现是 404 + URL 双重前缀 —— 排查时很容易先怀疑后端。
 * 成本是一个分支，故而保留。
 */
function joinAPIPath(path: string): string {
  if (/^https?:\/\//i.test(path)) return path
  if (path === API_BASE || path.startsWith(`${API_BASE}/`)) return path
  return path.startsWith('/') ? `${API_BASE}${path}` : path
}
