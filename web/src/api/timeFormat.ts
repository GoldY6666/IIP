// 时间的显示策略。
//
// 为什么列表用相对时间、详情用绝对时间：
//
//   值班场景里，用户问的是「这条是不是刚才出的」——相对时间一眼能答，
//   而 `2026/10/4 22:24:39` 要在心里做一次减法才知道。列表一屏几十行，
//   逐行做减法是不可接受的。
//
//   但相对时间会丢失精确值：**排查时你需要知道准确时刻**（去翻别的系统
//   的日志、对齐时间线）。所以两者不是二选一，而是
//   「相对时间 + 悬停给绝对时间」—— 想扫读用前者，要精确用后者。
//
// 为什么不用现成的 dayjs / antd 的 TimeAgo：
//   仓库里已有一个日期依赖（antd 自带 dayjs），但
//   * dayjs 的 relativeTime 插件对「超过 30 天」会退化成 `x 个月前`，
//     而本项目的事件可能挂几个月，退化后精度不够（值班要区分
//     「3 小时前」和「昨天」）；
//   * 它的阈值与文案不可控（想改成「不足 1 分钟」叫「刚刚」需要另配）；
//   * 引入它要多一个显式依赖，而这里的需求只有一个函数。
//   故自己实现 —— 全文不到40 行，且**可单测**（日期相关的逻辑最需要单测）。
//
// **不引 new Date() 的时区陷阱**：后端 RFC3339 带时区偏移（如 +08:00），
// `new Date()` 会正确转成绝对时刻再按本机时区格式化。
// 唯一要防的是**非法输入**：库里不该出现，但一旦出现 `new Date('x')` 是
// `Invalid Date`，`toLocaleString()` 会输出 "Invalid Date"，
// 而下面 getTime() 是 NaN —— 故所有分支都先判 isFinite。

/** MINUTE / HOUR / DAY 各自的毫秒数。写成命名常量，避免满屏 60000。 */
const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/**
 * parseTime 解析后端时间戳，失败返回 null 而非 Invalid Date。
 *
 * 为什么不直接返回 Date：`Invalid Date` 的传播性极差 ——
 * 它在算术里变NaN，在 toLocaleString 里变 "Invalid Date"，
 * 排查时会以为是格式化问题。集中在一处判掉更省事。
 */
export function parseTime(v: string | null | undefined): Date | null {
  if (!v) return null
  const d = new Date(v)
  return Number.isFinite(d.getTime()) ? d : null
}

/**
 * relativeTimeOf 返回「多久之前」的中文短句。
 *
 * 入参与 absoluteTimeOf 一致地收 `string | Date`：调用方常手上已有
 * parseTime 的结果（不必为再转一次字符串而绕回），两个函数的
 * 入参类型**刻意保持一致** —— 不一致会让调用方在类型上被迫做
 * 一次 `toISOString()`，而那一步没有语义。
 *
 * 阈值刻意按值班决策需要划分，而不是按「数学上自然」：
 *
 *   < 10 秒      「刚刚」      —— 反正在看，不用知道秒数
 *   < 60 秒      「N秒前」     —— 这几秒内的事态几乎必然是**正在发生**的
 *   < 60 分      「N分钟前」   —— 「是不是刚出的」这一问在这里被回答
 *   < 24 小时    「N小时前」
 *   < 7 天      「N天前」
 *   否则          具体日期    —— 再往后相对时间精度不够，
 *                              「45 天前」不如直接给日期
 *
 * **未来时间单独处理**：时钟漂移或客户端时区错误会让 startsAt 落在
 * 稍后，输出「-3秒前」这种明显荒谬的文案比不显示更糟。
 * 这种情况给「刚刚」—— 语义上「时间上就是此刻」。
 */
export function relativeTimeOf(
  v: string | Date | null | undefined,
  now: number = Date.now(),
): string {
  const d = v instanceof Date ? (Number.isFinite(v.getTime()) ? v : null) : parseTime(v)
  if (!d) return '—'
  const diff = now - d.getTime()
  if (diff < 10_000) return '刚刚'          // 含负数：未来时间也落这里
  if (diff < MINUTE) return `${Math.floor(diff / 1000)} 秒前`
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分钟前`
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 小时前`
  if (diff < 7 * DAY) return `${Math.floor(diff / DAY)} 天前`
  return absoluteTimeOf(d)
}

/**
 * absoluteTimeOf 返回绝对时刻字符串（本地时区）。
 *
 * 用手动格式化而不是 toLocaleString：后者在**不同机器上输出不同**
 * （有的环境没装 ICU，会退化成 "10/4/2026" 甚至 "Invalid Date"），
 * 而 E2E 截图与「复制给同事看」都需要稳定格式。
 *
 * 不带秒：秒在这类排查里用不上（真要精确对齐就去翻原始告警的
 * startsAt，那里有秒），带了反而让列宽多占 30px。
 */
export function absoluteTimeOf(v: string | Date | null | undefined): string {
  const d = v instanceof Date ? v : parseTime(v)
  if (!d || !Number.isFinite(d.getTime())) return '—'
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
    `${p(d.getHours())}:${p(d.getMinutes())}`
}

/**
 * timeAgoTip 是给 Tooltip 用的完整时刻串。
 *
 * 比 absoluteTimeOf 多带了「（相对）」，让悬停时能同时看到
 * 两个口径 —— 用户悬停往往正是想知道「刚才说的 5 分钟前
 * 具体是几点几分」。
 */
export function timeAgoTip(
  v: string | Date | null | undefined,
  now: number = Date.now(),
): string {
  const d = v instanceof Date ? (Number.isFinite(v.getTime()) ? v : null) : parseTime(v)
  if (!d) return '时间未知'
  return `${absoluteTimeOf(d)}（${relativeTimeOf(d, now)}）`
}