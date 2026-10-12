/**
 * S3 容器内真浏览器截图（DoD 3 / F5，收 OPEN-M5-8）。
 *
 * 容器链路的服务已经跑起来了（compose 映射 8080），这里走一遍真实用户流程
 * 并把每一步截下来存档。
 *
 * ## 为什么不用 e2e/ 下的 spec
 * e2e 那套自带依赖栈（stack.mjs 起真 PG + 真 iip-server，端口 8087），
 * 而这里要打的是**已经跑着的容器形态**（端口 8080）。
 * 复用 e2e 的 spec 会起两套栈、且被它的 webServer 抢端口。
 *
 * ## 为什么不用 chrome --headless --screenshot=URL
 * 那条路只能截单 URL 首屏，验不了「点了按钮之后页面变成什么样」。
 * DoD 3 要的是「列表 → 详情 → 处置」走一遍 ⇒ 必须有真实点击。
 *
 * ## 判据设计（每一条都在防自证失效）
 *  - **不用「文件非空」当「截到了东西」** —— 一张纯白页也非空。
 *    判**DOM 内容**（出现哪个标题/ 哪个状态标签），截图只是**存档**。
 *  - 处置那一步**额外回读 API**：页面变了不等于数据落库了
 *    （这条教训 e2e-03 已经踩过并写进注释）。
 *  - 每步截图后断言「颜色种类 >= 4」，排除白屏/黑屏。
 *    阈值取极低，因为要抓的是「有没有渲染出来」，不是「好不好看」。
 */
// 包名对齐项目现状：web/package.json 里装的是 @playwright/test（devDependencies），
// 没有裸 `playwright`。写 `from 'playwright'` 会 ERR_MODULE_NOT_FOUND。
// 不新增依赖 —— 跟 e2e-01..06 保持同一套。
import { chromium } from '@playwright/test'
import { mkdirSync, writeFileSync } from 'node:fs'
import { inflateSync } from 'node:zlib'
import { pathToFileURL } from 'node:url'

const BASE = process.env.S3_BASE_URL || 'http://127.0.0.1:8080'
const OUT = process.env.S3_OUT_DIR || 'docs/screenshots'
const REPORT = process.env.S3_REPORT || '_verify_s3.txt'
const SERVICE_PREFIX = process.env.S3_SERVICE_PREFIX || 'e2e-svc-'
const COLOR_MIN = 4 // 纯白/纯黑页只有 1 种；真实控制台远多于 4

const CHROME =
  process.env.IIP_CHROME_BIN ||
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe'

let pass = 0
let fail = 0
const lines = []

/**
 * 写一行到报告文件。
 *
 * **log() 必须自己落盘，不能只 push 进内存**（2026-10-12 修）：
 * 原实现里只有 check() 会重写文件，而 log() 仅 push 到 lines。
 * 于是「汇总行」——判据 `=== S3 汇总：N PASS / N FAIL ===` 的唯一
 * 数据来源——只在进程正常走到末尾时才存在于内存；一旦中途异常
 * （或 browser.close() 卡住被外部超时杀掉），文件里就永远缺这一行，
 * 外层判据只能报「未在报告中找到汇总行」，而实际断言可能全 PASS。
 * 症状是「S3 全绿但外层判红」，且报告本身毫无线索。
 *
 * 顺手让写入变成「尽量完成」：报告是取证材料，写失败不该把
 * 整个脚本带崩（原来的 writeFileSync 抛出会让最后一条判据丢失）。
 */
function flush() {
  try {
    writeFileSync(REPORT, lines.join('\n') + '\n', 'utf8')
  } catch (e) {
    console.error(`[warn] 报告写入失败 ${REPORT}: ${e && e.message}`)
  }
}

function log(s) {
  lines.push(s)
  console.log(s)
  flush()
}

/** 判据统一在这里落报告 —— 顺手全量重写，进程被杀也能拿到最新进度。 */
function check(name, ok, detail = '') {
  if (ok) {
    pass++
    log(`  [PASS] ${name}${detail ? '  -- ' + detail : ''}`)
  } else {
    fail++
    log(`  [FAIL] ${name}  -- ${detail}`)
  }
  flush()
}

/**
 * 数 PNG 的颜色种类（排除白屏/黑屏）。
 *
 * 自己解 PNG：宿主没有 Pillow/numpy/canvas。
 * IDAT 拼接 → inflate → 逐行反滤波（5 种 filter）→ 采像素。
 * 调色板类型（colorType=3）**抛异常而非返回 0** ——
 * 返回 0 会让调用方把「解不出来」当成「0 种颜色」，
 * 而「解不出」与「就是单色」是两件事。
 */
export function countColors(buf) {
  if (buf.length < 8 || buf.readUInt32BE(0) !== 0x89504e47) {
    throw new Error('不是 PNG（魔数不符）')
  }
  let pos = 8
  let width = 0
  let height = 0
  let bitDepth = 0
  let colorType = 0
  const idat = []
  while (pos + 8 <= buf.length) {
    const len = buf.readUInt32BE(pos)
    const type = buf.toString('ascii', pos + 4, pos + 8)
    const data = buf.subarray(pos + 8, pos + 8 + len)
    if (type === 'IHDR') {
      width = data.readUInt32BE(0)
      height = data.readUInt32BE(4)
      bitDepth = data[8]
      colorType = data[9]
    } else if (type === 'IDAT') {
      idat.push(data)
    } else if (type === 'IEND') {
      break
    }
    pos += 12 + len
  }
  if (colorType === 3) throw new Error('调色板 PNG（colorType=3）不支持，请用真彩色')
  if (bitDepth !== 8) throw new Error(`不支持位深 ${bitDepth}，只处理 8`)
  const channels = { 0: 1, 2: 3, 4: 2, 6: 4 }[colorType]
  if (!channels) throw new Error(`不支持 colorType ${colorType}`)
  if (!width || !height) throw new Error(`IHDR 尺寸异常 ${width}x${height}`)

  const raw = inflateSync(Buffer.concat(idat))
  const bpp = channels
  const stride = width * bpp
  const out = Buffer.alloc(height * stride)
  let rp = 0
  for (let y = 0; y < height; y++) {
    const filter = raw[rp++]
    const row = raw.subarray(rp, rp + stride)
    rp += stride
    const cur = out.subarray(y * stride, (y + 1) * stride)
    const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null
    for (let x = 0; x < stride; x++) {
      const a = x >= bpp ? cur[x - bpp] : 0
      const b = prev ? prev[x] : 0
      const c = prev && x >= bpp ? prev[x - bpp] : 0
      let v = row[x]
      if (filter === 1) v += a
      else if (filter === 2) v += b
      else if (filter === 3) v += (a + b) >> 1
      else if (filter === 4) {
        const p = a + b - c
        const pa = Math.abs(p - a)
        const pb = Math.abs(p - b)
        const pc = Math.abs(p - c)
        v += pa <= pb && pa <= pc ? a : pb <= pc ? b : c
      }
      cur[x] = v & 0xff
    }
  }
  const seen = new Set()
  for (let i = 0; i + 2 < out.length; i += bpp) {
    seen.add((out[i] << 16) | (out[i + 1] << 8) | out[i + 2])
    if (seen.size > 4096) break
  }
  return seen.size
}

/** 截图 + 断言非空白页。截图是存档，判据本体是 DOM（见文件头注释）。 */
async function shot(page, name) {
  const buf = await page.screenshot({ path: `${OUT}/${name}.png` })
  let colors = -1
  let err = ''
  try {
    colors = countColors(buf)
  } catch (e) {
    err = String(e.message || e)
  }
  // colors === -1 表示「解不出来」，不能当成「0 种颜色」
  check(
    `截图非空白页（${name}.png，颜色种类 >= ${COLOR_MIN}）`,
    colors >= COLOR_MIN,
    colors >= 0
      ? `颜色种类=${colors}，${buf.length} 字节。纯白/纯黑失败页只有 1 种`
      : `PNG 解码失败（截图存在但无法验像素）: ${err}`,
  )
  return colors
}

export async function runS3() {
  mkdirSync(OUT, { recursive: true })
  log('### S3 容器内真浏览器截图（DoD 3 / F5）')
  log(`  BASE=${BASE}`)
  log(`  OUT=${OUT}`)
  log(`  chrome=${CHROME}`)

  // **launch 必须放进 try**：Chromium 启动失败（浏览器缺失、版本不匹配、
  // 沙箱参数冲突）是最常见的失败形态，而它发生在 try 之外时，
  // catch 抓不到→ 报告文件一个字节都不落，调用方只看到
  // 「报告未生成 + rc=1」这种无信息量的组合，只能靠猜。
  // 反过来，launch 放进 try 后即便浏览器起不来，报告里也会留下
  // 「S3 脚本执行无异常 -- <真实错误>」这一行可直接定位的证据。
  let browser = null
  let page = null
  try {
    browser = await chromium.launch({
      executablePath: CHROME,
      args: ['--disable-gpu'],
    })
    page = await browser
      .newContext({ viewport: { width: 1440, height: 900 } })
      .then((c) => c.newPage())

    // ---- 1. 列表页 ----
    await page.goto(BASE + '/', { waitUntil: 'networkidle', timeout: 60_000 })
    await page.getByText('事件').first().waitFor({ timeout: 30_000 })
    check('S3-1 列表页渲染出事件表', true, '页面出现「事件」标题')
    await shot(page, '01-list')

    // ---- 2. 找一条事件（不写死 id：id 随运行次数变，写死会让第二次跑找不到）----
    const res = await page.request.get(BASE + '/api/v1/incidents?limit=100')
    check('S3-2 列表接口可读', res.status() === 200, 'status=' + res.status())
    const body = await res.json().catch(() => ({}))
    const all = body.incidents || []
    const mine = all.filter((i) => String(i.service || '').startsWith(SERVICE_PREFIX))
    check(
      'S3-2 前置条件：库里有 e2e 造的事件',
      mine.length > 0,
      `service 以 ${SERVICE_PREFIX} 开头 ${mine.length} 条 / 全库 ${all.length} 条`,
    )
    if (mine.length === 0) {
      // 不 close 浏览器：交给 finally 统一关，避免两处 close 竞态。
      log('')
      log(`=== S3 汇总：${pass} PASS / ${fail} FAIL ===`)
      return { pass, fail }
    }
    const inc = mine.sort((a, b) => b.id - a.id)[0]
    log(`  选中事件 id=${inc.id} status=${inc.status} service=${inc.service}`)

    // ---- 3. 详情页 ----
    await page.goto(BASE + `/incidents/${inc.id}`, { waitUntil: 'networkidle' })
    await page.getByText('处置').first().waitFor({ timeout: 30_000 })
    check('S3-3 详情页渲染出处置面板', true, `事件 id=${inc.id}`)
    await shot(page, '02-detail')

    // ---- 4. 处置（真实点击 + 回读落库）----
    const before = await (await page.request.get(BASE + `/api/v1/incidents/${inc.id}`)).json()
    const tlBefore = (before.timeline || []).length

    // antd 会在中文 Button 文字里插空格（e2e-03 已踩过），用宽松正则。
    // 按钮文案取自实现而非想象：open 状态允许 investigating。
    const btn = page.getByRole('button', { name: /转为\s*调\s*查\s*中/ })
    if ((await btn.count()) === 0) {
      check('S3-4 处置按钮存在（前置条件）', false,
        `事件状态 ${inc.status} 下无「转为调查中」按钮 —— 可能已被前一轮推进。` +
        '**如实记FAIL，不假装通过**')
    } else {
      await btn.click()
      await page.getByText('调查中').first().waitFor({ timeout: 30_000 })
      check('S3-4a 处置后页面状态变「调查中」', true, '前端读到了新状态')
      await shot(page, '03-transition')

      // **回读落库** —— 页面变了不等于数据落库了。
      let ok = false
      let n = -1
      for (let i = 0; i < 20; i++) {
        const d = await (await page.request.get(BASE + `/api/v1/incidents/${inc.id}`)).json()
        n = (d.timeline || []).length
        if (d.incident?.status === 'investigating' && n === tlBefore + 1) {
          ok = true
          break
        }
        await page.waitForTimeout(500)
      }
      check('S3-4b 处置后回读落库（状态 + 时间线各 +1）', ok,
        ok ? `status=investigating，timeline ${tlBefore} -> ${n}`
           : `轮询 10s 未见 status=investigating 且 timeline=${tlBefore}+1（实见 ${n}）`)
    }

    // ---- 5. 容器形态下 auth/session 可读（S4 默认关）----
    const rSess = await page.request.get(BASE + '/api/v1/auth/session')
    const j = await rSess.json().catch(() => ({}))
    check('S3-5 容器形态 auth/session 可读（默认关 => auth_required=false）',
      rSess.status() === 200 && j.auth_required === false,
      `status=${rSess.status()} auth_required=${j.auth_required}`)
  } catch (e) {
    check('S3 脚本执行无异常', false, String((e && e.message) || e))
  } finally {
    // browser 可能是 null（launch 本身失败）—— 直接 close 会再抛一次，
    // 那个二次异常会盖掉上面 catch 里刚写进报告的真实错误。
    if (browser) await browser.close().catch(() => {})
  }

  log('')
  log(`=== S3 汇总：${pass} PASS / ${fail} FAIL ===`)
  return { pass, fail }
}

/**
 * 直接 `node s3-shots.mjs` 执行时的入口。
 *
 * **为什么要判断 import.meta.url**：这个文件同时有两个用法——
 *  1) `verify-images.py` 4.8 段 `import { runS3 }` 后调用（容器链路自动跑）；
 *  2) 人工 `node e2e/s3-shots.mjs` 单独跑（环境恢复后手动补跑）。
 * 若不判断，被 import 时会自动执行一次，容器链路就会跑两遍。
 */
const isDirectRun =
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href

if (isDirectRun) {
  const { pass: p, fail: f } = await runS3()
  // 有 FAIL 用非零退出码：让调用方（CI / verify-images.py）能只看 rc 判成败
  process.exit(f > 0 ? 1 : 0)
}