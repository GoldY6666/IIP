// E2E 依赖栈的起停：真 PostgreSQL + 真 iip-server。
//
// **为什么必须是一个 Node 进程内的闭环**
// 沙箱在每条工具调用结束时回收整个进程树 —— 跨调用启动的 postgres
// 活不过一条命令（记忆里「真库门禁前台跑必被 SIGTERM」同源）。
// Playwright 的 webServer 本身就是「起进程 → 跑测试 → 收」，所以
// 「起 PG → 建库 → 起服务 → 等就绪」必须塞进同一个由它拉起的进程。
//
// **跨平台**：本机（Windows）与 CI（ubuntu）都能跑。
// 路径与可执行名全部由平台推导，不写死盘符 —— 写死后
// 「CI 里加了个 e2e job 但永远红」，那比不加更糟：
// 它会让人以为 E2E 被覆盖了。
//
//   Windows: PG_BIN=D:\pgsql16\bin, postgres.exe
//   Linux:   PG_BIN=/usr/lib/postgresql/16/bin, postgres
//
// 平台差异用环境变量覆盖（CI 里显式给）：
//   E2E_PG_BIN      PostgreSQL 可执行文件所在目录
//   E2E_PG_DATA     PGDATA 目录
//   E2E_PG_USER     超级用户（默认 postgres）
//   E2E_GO_BIN      go 可执行文件（默认 PATH 里的 go）
//
// **参数全部取自 `iip-server --help` 的真实输出**，不凭印象写：
//   --pg-url（注意：不是 IIP_DATABASE_URL，仓库里没有那个变量）
//   --http-addr / --web-root / --topology-file
//   --ui-prometheus-url / --ui-grafana-url / --ui-loki-url / --ui-loki-tenant-id
// 这一点本轮踩过：最初写的是 IIP_DATABASE_URL，服务起不来且报错不指向它。
import { spawn, spawnSync, execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, rmSync } from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const WEB_ROOT = path.resolve(HERE, '..')
const REPO = path.resolve(WEB_ROOT, '..')
const DIST = path.join(WEB_ROOT, 'dist')
const IS_WIN = process.platform === 'win32'
const EXT = IS_WIN ? '.exe' : ''

const PG_BIN =
  process.env.E2E_PG_BIN ||
  (IS_WIN ? 'D:\\pgsql16\\bin' : '/usr/lib/postgresql/16/bin')
const PG_DATA = process.env.E2E_PG_DATA || (IS_WIN ? 'D:\\_iip_e2e_pgdata' : '/tmp/iip_e2e_pgdata')
const PG_USER = process.env.E2E_PG_USER || 'postgres'
const GO_BIN = process.env.E2E_GO_BIN || 'go'
const BIN = process.env.E2E_SERVER_BIN || (IS_WIN ? 'D:\\_iip_setup\\_e2e_iip-server.exe' : '/tmp/iip-e2e-server')

// 超级用户的密码。trust 认证（initdb -A trust）下本机不需要，
// 但 CI 的 service 容器默认是 scram —— 传空串让 libpq 走 .pgpass/环境。
const PGPASSWORD = process.env.E2E_PG_PASSWORD || ''

// 采集器全关：E2E 不验证据采集（那是 M4 链路 C 的职责），
// 关掉后页面「证据」Tab 显示空态并说明原因，E2E-2 正好验那条文案。
const E2E_ENV = {
  IIP_LOG_LEVEL: 'info',
  IIP_EVIDENCE_PROM_ENABLED: 'false',
  IIP_EVIDENCE_LOKI_ENABLED: 'false',
}

function exe(name) {
  return path.join(PG_BIN, name + EXT)
}

function portBusy(port) {
  return new Promise((resolve) => {
    const s = new net.Socket()
    s.setTimeout(800)
    s.once('connect', () => { s.destroy(); resolve(true) })
    s.once('timeout', () => { s.destroy(); resolve(false) })
    // ConnectionRefused 与 Timeout 都判「未占用」：Windows 防火墙对某些端口
    // SYN 静默丢包，TimeoutError 也属 OSError，只判 refused 会把空闲误报成占用。
    s.once('error', () => { s.destroy(); resolve(false) })
    s.connect(port, '127.0.0.1')
  })
}

async function waitPort(port, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await portBusy(port)) return true
    await new Promise((r) => setTimeout(r, 500))
  }
  throw new Error(`${label} 端口 ${port} 在 ${timeoutMs}ms 内未就绪`)
}

function build() {
  if (process.env.E2E_SKIP_BUILD === '1' && existsSync(BIN)) return
  console.log('[stack] 编译 iip-server ->', BIN)
  execFileSync(GO_BIN, ['build', '-o', BIN, './cmd/iip-server'], {
    cwd: REPO,
    stdio: 'inherit',
    // -o 必须给本平台路径：go 不认异平台风格路径，产物根本不生成，
    // 而管道会吃掉退出码让失败也报 rc=0（记忆里登记过）。
  })
}

function initPG() {
  if (existsSync(path.join(PG_DATA, 'PG_VERSION'))) return
  console.log('[stack] initdb ->', PG_DATA)
  mkdirSync(PG_DATA, { recursive: true })
  execFileSync(exe('initdb'), ['-D', PG_DATA, '-U', PG_USER, '-A', 'trust'], { stdio: 'inherit' })
}

function psql(sql, port, db = 'postgres') {
  return execFileSync(
    exe('psql'),
    ['-h', '127.0.0.1', '-p', String(port), '-U', PG_USER, '-d', db, '-At', '-c', sql],
    { encoding: 'utf8', env: { ...process.env, PGPASSWORD } },
  ).toString().trim()
}

async function cmdUp() {
  const pgPort = Number(process.argv[3])
  const appPort = Number(process.argv[4])
  const db = process.env.E2E_DB || 'iip_e2e'

  for (const p of [pgPort, appPort]) {
    if (await portBusy(p)) {
      console.error(`[stack] FATAL 端口 ${p} 已被占用（E2E 不能与 verify-func 同时跑）`)
      process.exit(2)
    }
  }

  if (!existsSync(path.join(DIST, 'index.html'))) {
    console.error('[stack] FATAL dist/index.html 不存在，先跑 pnpm build')
    process.exit(1)
  }
  initPG()
  build()

  // 残留 pid 文件会让 postgres 拒绝启动，而报错文本不指向真因。
  const pidf = path.join(PG_DATA, 'postmaster.pid')
  if (existsSync(pidf)) rmSync(pidf, { force: true })

  const pg = spawn(exe('postgres'), ['-D', PG_DATA, '-p', String(pgPort)], { stdio: 'pipe' })
  const pgLog = []
  pg.stdout?.on('data', (d) => pgLog.push(d))
  pg.stderr?.on('data', (d) => pgLog.push(d))

  // **必须用 pg_isready 判就绪，不能用「端口能连」**：
  // 端口一 listen 就能连，但 crash recovery 期间会回 FATAL: starting up。
  const deadline = Date.now() + 120_000
  let ready = false
  while (Date.now() < deadline) {
    if (spawnSync(exe('pg_isready'), ['-h', '127.0.0.1', '-p', String(pgPort)], { stdio: 'ignore' }).status === 0) {
      ready = true
      break
    }
    await new Promise((r) => setTimeout(r, 500))
  }
  if (!ready) {
    console.error('[stack] pg_isready 超时\n' + Buffer.concat(pgLog).toString())
    process.exit(1)
  }
  console.log('[stack] PG 就绪 @', pgPort)

  // 干净起点：**先 DROP 再 CREATE**。用 CREATE ... IF NOT EXISTS 去判断
  // 会让「本轮没删干净」被静默沿用，残留行会让断言指向错误的原因。
  spawnSync(
    exe('psql'),
    ['-h', '127.0.0.1', '-p', String(pgPort), '-U', PG_USER, '-d', 'postgres',
      '-c', `DROP DATABASE IF EXISTS ${db}`],
    { env: { ...process.env, PGPASSWORD } },
  )
  psql(`CREATE DATABASE ${db}`, pgPort)
  console.log('[stack] 库', db, '重建完成')

  const userinfo = PGPASSWORD ? `${PG_USER}:${PGPASSWORD}` : PG_USER
  const srv = spawn(
    BIN,
    [
      '--http-addr', `:${appPort}`,
      '--pg-url', `postgres://${userinfo}@127.0.0.1:${pgPort}/${db}?sslmode=disable`,
      '--web-root', DIST,
      '--topology-file', path.join(REPO, 'config', 'topology.yaml'),
      // UI 外链用不可路由的假主机名：E2E 不点这些链接（那是组件测试的职责），
      // 验的是「注入生效」—— 真地址反而会让「误连成功」掩盖注入失败。
      '--ui-prometheus-url', 'http://prom.e2e.invalid:19090',
      '--ui-grafana-url', 'http://grafana.e2e.invalid:13000',
      '--ui-loki-url', 'http://loki.e2e.invalid:13100',
      '--ui-loki-tenant-id', 'e2e-team',
    ],
    { cwd: REPO, env: { ...process.env, ...E2E_ENV }, stdio: 'pipe' },
  )
  srv.stdout.on('data', (d) => process.stdout.write('[srv] ' + d))
  srv.stderr.on('data', (d) => process.stderr.write('[srv] ' + d))

  await waitPort(appPort, 90_000, 'iip-server')

  // **不能只等端口**：端口 listen 早于迁移完成，那时打 API 会拿到
  // 「relation does not exist」。必须探 /healthz 且确认它真查库。
  const hl = Date.now() + 90_000
  let healthy = false
  while (Date.now() < hl) {
    try {
      const r = await fetch(`http://127.0.0.1:${appPort}/healthz`)
      if (r.status === 200 && (await r.text()).trim() === 'ok') { healthy = true; break }
    } catch { /* 还没起来 */ }
    await new Promise((r) => setTimeout(r, 500))
  }
  if (!healthy) {
    console.error('[stack] /healthz 未就绪（迁移可能失败）')
    process.exit(1)
  }
  console.log('[stack] iip-server 就绪 @', appPort)

  // 表清单打出来：E2E 断言依赖具体表名，出问题时能立刻看出是迁移没跑完。
  try {
    console.log('[stack] 表清单:', psql(
      `SELECT string_agg(table_name, ',' ORDER BY table_name) FROM information_schema.tables WHERE table_schema='public'`,
      pgPort, db,
    ))
  } catch (e) {
    console.error('[stack] 读表清单失败:', e.message)
  }

  let stopping = false
  const bye = () => {
    if (stopping) return
    stopping = true
    console.log('[stack] 收到退出信号，回收')
    // pg_ctl stop 加 -t 60：data 目录 fsync 可能很慢，默认超时会判失败
    // （记忆里登记过：实际停干净了，只是等待方判定失败）。
    spawnSync(exe('pg_ctl'), ['-D', PG_DATA, '-m', 'fast', '-t', '60', 'stop'], { stdio: 'ignore' })
    try { srv.kill() } catch { /* 忽略 */ }
    try { pg.kill() } catch { /* 忽略 */ }
    process.exit(0)
  }
  process.on('SIGTERM', bye)
  process.on('SIGINT', bye)
  srv.on('exit', (code) => { console.log('[stack] iip-server 退出 code=', code); bye() })
}

if (process.argv[2] === 'up') await cmdUp()
else {
  console.error('用法: node e2e/stack.mjs up <pgPort> <appPort>')
  process.exit(64)
}
