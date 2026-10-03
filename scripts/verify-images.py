"""IIP v1 镜像与容器端到端验证。

用途：在 Docker daemon 可用后，一条命令完成「构建镜像 -> 起全部服务 ->
逐项断言 -> 打印证据 -> 清理」的完整闭环，产出可直接归档的验证输出。

为什么是脚本而不是一串手敲命令：
  1. 断言要逐项判定 pass/fail 并留证据，不能靠肉眼扫输出；
  2. 任何一步失败要立刻定位到具体服务，而不是等 compose 超时；
  3. 结尾必须回收容器，避免残留占端口影响后续验证。

前置条件：
  - Docker daemon 正在运行（`docker info` 返回 0）
  - 端口 8080 未被占用（compose 的 iip-server 要 bind 它）

用法（仓库根执行）：
  python scripts/verify-images.py

环境变量（全部可选）：
  IIP_DOCKER_BIN   docker.exe 绝对路径（不在 PATH 时必填）
  IIP_TAG          镜像标签          默认 0.0-m2
  IIP_HTTP_PORT    宿主机映射端口    默认 8080
  IIP_KEEP         设为 1 则验证后不清理容器（便于人工排查）
  IIP_LOG_DIR      报告输出目录      默认 = 仓库根
  IIP_GOPROXY      构建期 Go 模块代理 默认 https://goproxy.cn,direct
"""
import json
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.environ.get("IIP_PROJ", os.path.dirname(HERE))
COMPOSE_FILE = os.path.join(PROJ, "deploy", "docker-compose.yml")

# docker 的定位与环境装配与 scripts/prepull-mirror.py 共用一处实现，
# 避免两个脚本对同一台机器得出不一致的结果。详见 lib_docker.py 的模块注释。
sys.path.insert(0, HERE)
from lib_docker import connect, run as _run  # noqa: E402

TAG = os.environ.get("IIP_TAG", "0.0-m2")
HTTP_PORT = int(os.environ.get("IIP_HTTP_PORT", "8080"))
KEEP = os.environ.get("IIP_KEEP", "") == "1"

# 构建期 Go 模块代理。Dockerfile 的默认值是官方 proxy.golang.org（生产/CI 用），
# 但容器内直连它在国内不通（实测超时），而 goproxy.cn 通。
# 本地验收默认走 goproxy.cn，可用 IIP_GOPROXY 覆盖（如设成官方值验证生产路径）。
GOPROXY = os.environ.get("IIP_GOPROXY", "https://goproxy.cn,direct")

REPORT = os.path.join(os.environ.get("IIP_LOG_DIR", PROJ), "_verify_images.txt")

# 本轮运行的唯一令牌，注入告警的非动态标签值里。
#
# 为什么需要它：去重是按**指纹**判定的，且**没有时间窗**（V1.0 §4.2：同一指纹
# 的 firing 已在库中即 skipped）。指纹又包含全部非动态标签，所以只要标签值
# 逐字相同，本轮告警就会被上一轮留在库里、且仍处于 firing 的行吃掉——
# compose 用的是 named volume，脚本第二次执行时数据仍在。
# 实测：不加令牌时第二轮 POST 返回 skipped:2，落库 0 行，
# 「本轮新增 2 条」的前提不成立，聚合断言必然假失败。
# 故令牌必须**跨进程调用**唯一，而不只是跨轮次唯一。
RUN_TOKEN = time.strftime("%m%d-%H%M%S")

L: list[str] = []
FAILURES: list[str] = []


def w(s: str = "") -> None:
    """写一行到报告并立即落盘，便于外部进程实时观察。"""
    L.append(str(s))
    try:
        with open(REPORT, "w", encoding="utf-8") as fh:
            fh.write("\n".join(L))
    except OSError:
        pass


def check(name: str, ok: bool, detail: str = "") -> bool:
    """记录一条断言。ok=False 累积到 FAILURES，最终决定退出码。"""
    mark = "PASS" if ok else "FAIL"
    w("  [%s] %s%s" % (mark, name, ("  -- " + detail) if detail else ""))
    if not ok:
        FAILURES.append(name)
    return ok


# ------------------------------------------------------------------ docker
DOCKER, ENV = connect()
# cli-plugins 目录的 PATH 装配也在 lib_docker.make_env() 里完成，
# 这里拿到的 ENV 已经能直接跑 `docker compose`。

def run(args: list[str], timeout: int = 900, cwd: str | None = None):
    """执行命令，返回 (rc, stdout+stderr)。默认在仓库根执行。"""
    return _run(args, ENV, timeout=timeout, cwd=cwd or PROJ)


def docker(*args: str, timeout: int = 900):
    assert DOCKER is not None
    return run([DOCKER, *args], timeout=timeout)


def compose(*args: str, timeout: int = 900):
    return docker("compose", "-f", COMPOSE_FILE, *args, timeout=timeout)


# -------------------------------------------------------------------- http
def http_request(url: str, method: str = "GET", data: bytes | None = None,
                 timeout: float = 5.0):
    """发起 HTTP 请求，返回 (status, body)。status 为 None 表示连接层失败。

    M2 的配置接口是 PUT，而 urllib 的 urlopen 默认只发 GET。
    原先只有一个 http_get，无法验证「改配置无需重启即生效」这条 M2 验收——
    故统一为带 method 的实现，GET 保留 http_get 这个薄封装以兼容既有调用。
    """
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, (e.read() or b"").decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return None, repr(e)


def http_get(url: str, timeout: float = 5.0):
    return http_request(url, "GET", None, timeout)


def port_in_use(port: int) -> bool:
    """判断 127.0.0.1:port 上是否已有服务在监听。

    注意：这是「能否连上」的探测，用于确认待用端口是否被占用。
    早先这个函数名叫 port_free 但语义相反，容易误读，已改名。

    **超时必须算「未占用」**（2026-10-03 修）：Windows 防火墙会对本机某些
    端口的 SYN 静默丢包，connect() 抛 TimeoutError 而非 ConnectionRefused。
    原实现只 except OSError 就一律判「占用」，于是 8080 因一次
    `netstat` 里只见到 TIME_WAIT 残留（无 LISTEN）却被判为占用，
    整个容器链路 BLOCKED。丢包说明**没有任何进程在那个端口上**——
    真有服务在监听时 connect 一定成功（回 RST 或握手完成）。
    """
    sk = socket.socket()
    sk.settimeout(1.0)
    try:
        sk.connect(("127.0.0.1", port))
        return True
    except (ConnectionRefusedError, socket.timeout, TimeoutError):
        # 连不上 / 超时 = 没人监听。ConnectionRefused 是「明确无人」，
        # 超时是「被丢包」，两者对「能否 bind」而言答案相同：可以 bind。
        return False
    except OSError:
        # 其余 OSError（权限、地址族不匹配等）保守判为占用 ——
        # 宁可误报阻塞，也不要在判断错误时把容器拉起来撞端口。
        return True
    finally:
        sk.close()


def latest_migration_version() -> int:
    """从 migrations/ 目录推断最新迁移号，供断言动态使用。

    为什么不能写死：M0 时代这里断言的是「版本 1」，M1 加 0002、M2 加 0003 之后，
    这条断言就变成「永远失败」的过期断言——输出看着像产品回归，实际是脚本
    没跟上里程碑（scripts/verify-runtime.py 里 OPEN-8 修的是同一个毛病）。
    改为动态读取后，断言随里程碑自动前移，不需要每次发版都回来改这一行。

    读不到目录时返回 0，由调用方按「期望 0 必然不匹配」自然报 FAIL，
    而不是抛异常把整轮验收打断。
    """
    mig_dir = os.path.join(PROJ, "migrations")
    best = 0
    try:
        for fn in os.listdir(mig_dir):
            m = re.match(r"(\d+)_.*\.up\.sql$", fn)
            if m:
                best = max(best, int(m.group(1)))
    except OSError:
        return 0
    return best


def svc_container(service: str) -> str:
    """取 compose 服务的真实容器标识，供 docker exec/logs 使用。

    不要硬编码容器名：compose 的 container_name 与「服务名」经常不同
    （本项目 postgres 服务的实际容器名是 iip-postgres）。
    早先这里写死了 "postgres"，导致两条断言直接报
    `No such container: postgres`，看起来像产品失败、实际是脚本空转。
    取不到时回退到服务名，保持可读的报错。
    """
    rc, out = compose("ps", "-q", service, timeout=60)
    cid = out.strip().splitlines()[0].strip() if rc == 0 and out.strip() else ""
    return cid or service


def compose_ps() -> list[dict]:
    """取 compose 服务状态（JSON 数组）。"""
    rc, out = compose("ps", "--format", "json", timeout=120)
    if rc != 0 or not out:
        return []
    rows = []
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return rows


def main() -> int:
    w("=" * 70)
    w(" IIP v1 镜像与容器验证   " + time.strftime("%Y-%m-%d %H:%M:%S"))
    w("=" * 70)
    w()

    # ---------------------------------------------------------- 0. 前置
    w("### [0] 前置检查")
    if not DOCKER:
        w("  FATAL: 找不到 docker 可执行文件")
        w("  hint: 设 IIP_DOCKER_BIN=<docker.exe 绝对路径>，或把 Docker Desktop 的")
        w("        resources\\bin 加入 PATH")
        return 2
    w("  docker = %s" % DOCKER)

    rc, out = docker("info", "--format", "{{.ServerVersion}}|{{.OSType}}|{{.Driver}}",
                     timeout=60)
    if not check("Docker daemon 可用", rc == 0, out[:200]):
        w()
        w("  daemon 不可用时无法继续。常见原因：")
        w("    - Docker Desktop 未启动 / 卡在 'Engine starting'")
        w("    - WSL2 缺失（需管理员启用 Microsoft-Windows-Subsystem-Linux + VirtualMachinePlatform）")
        return 2
    w("  daemon = %s" % out)

    check("端口 %d 未被占用（iip-server 要 bind 它）" % HTTP_PORT, not port_in_use(HTTP_PORT),
          "" if not port_in_use(HTTP_PORT) else "已被占用，先停掉占用进程")
    if port_in_use(HTTP_PORT):
        return 2
    w()

    # ------------------------------------------------------ 1. 构建镜像
    w("### [1] 构建镜像")
    for target, name in [("server", "iip-server"), ("worker", "iip-worker")]:
        image = "%s:%s" % (name, TAG)
        w("  building %s ..." % image)
        t0 = time.time()
        rc, out = docker("build", "--target", target, "-t", image,
                         "--build-arg", "VERSION=" + TAG,
                         "--build-arg", "GOPROXY=" + GOPROXY,
                         ".", timeout=1800)
        dt = time.time() - t0
        check("构建 %s" % image, rc == 0, "耗时 %.1fs" % dt)
        if rc != 0:
            w("  ---- build 输出（末 40 行）----")
            for line in out.splitlines()[-40:]:
                w("    " + line)
            return 3
        # 记录镜像体积（精简镜像的一个可观测指标）
        rc2, out2 = docker("images", image, "--format", "{{.Size}}", timeout=60)
        if rc2 == 0:
            w("        镜像体积 %s" % out2.strip())
    w()

    # -------------------------------------------------------- 2. 起服务
    w("### [2] 启动全部服务（--profile app）")
    rc, out = compose("--profile", "app", "up", "-d", timeout=900)
    check("compose up -d 返回成功", rc == 0, out[:300])
    if rc != 0:
        w("  ---- up 输出（末 40 行）----")
        for line in out.splitlines()[-40:]:
            w("    " + line)
    w()

    # 等健康：iip-server 依赖 postgres healthy，PG 首次启动要初始化数据目录
    #
    # 注意：必须等到「没有任何服务还停在 starting」才算收敛。
    # 早先只等 iip-server=healthy 就 break，会在 alertmanager 仍 starting 时
    # 通过，从而漏掉它随后变成 unhealthy —— 本项目的 alertmanager
    # healthcheck 文案不匹配缺陷就是这样被漏掉的（详见 deploy/docker-compose.yml）。
    w("### [3] 等待服务健康（最多 180s）")
    deadline = time.time() + 180
    last = ""
    ready = False
    states: dict = {}
    while time.time() < deadline:
        rows = compose_ps()
        if rows:
            summary = " ".join(
                "%s=%s" % (r.get("Service", "?"), r.get("Health") or r.get("State", "?"))
                for r in sorted(rows, key=lambda x: x.get("Service", ""))
            )
            if summary != last:
                last = summary
                w("  [%s] %s" % (time.strftime("%H:%M:%S"), summary))
            states = {r.get("Service"): (r.get("Health") or r.get("State")) for r in rows}
            settled = not any(v in ("starting", "created") for v in states.values())
            if states.get("iip-server") == "healthy" \
                    and states.get("iip-worker") == "running" and settled:
                ready = True
                break
        time.sleep(3)

    rows = compose_ps()
    for r in sorted(rows, key=lambda x: x.get("Service", "")):
        w("  %-14s %-10s %s" % (r.get("Service", "?"),
                                r.get("Health") or r.get("State", "?"),
                                r.get("Status", "")))
    states = {r.get("Service"): (r.get("Health") or r.get("State")) for r in rows}
    check("iip-server healthy", ready, "" if ready else "180s 内未达 healthy")

    unhealthy = sorted(s for s, v in states.items() if v == "unhealthy")
    check("无服务处于 unhealthy", not unhealthy,
          "" if not unhealthy else "unhealthy: %s" % ", ".join(unhealthy))

    still_starting = sorted(s for s, v in states.items() if v in ("starting", "created"))
    check("无服务停留在 starting/created", not still_starting,
          "" if not still_starting else "仍在启动: %s" % ", ".join(still_starting))
    w()

    # ---------------------------------------------------- 4. 逐项断言
    w("### [4] 功能断言")

    # postgres 服务的真实容器名是 iip-postgres（来自 compose 的 container_name），
    # 不能直接用服务名，否则 docker exec 会报 No such container。
    PG_C = svc_container("postgres")

    # 4.1 healthz 真查库（server 容器的 /healthz 会 SELECT 1）
    st, body = http_get("http://127.0.0.1:%d/healthz" % HTTP_PORT)
    check("GET /healthz -> 200 ok", st == 200 and body.strip() == "ok",
          "status=%s body=%r" % (st, body[:80]))

    # 4.2 容器内自检模式（--healthcheck 是 distroless 下的唯一探针手段）
    rc, out = docker("exec", "iip-server", "/usr/local/bin/iip-server",
                     "--healthcheck", "--http-addr=:8080", timeout=30)
    check("容器内 --healthcheck 自检退出码 0", rc == 0, out[:160])

    # 4.3 迁移已自动执行（server 启动时会跑 embed 的迁移）
    # 注意：psql 在拼接模式下输出 boolean 为 "false"，在列模式下为 "f"，
    # 两种都算通过，避免因输出格式差异误判。
    # 期望版本不写死，从 migrations/ 动态推断（M2 起为 3）。
    want_ver = latest_migration_version()
    rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                     "-t", "-A", "-c",
                     "SELECT version||'|'||dirty FROM schema_migrations;", timeout=60)
    got = out.strip()
    check("schema_migrations 已到版本 %d 且非 dirty" % want_ver,
          rc == 0 and got in ("%d|false" % want_ver, "%d|f" % want_ver),
          "实际 %r" % got)

    # 4.4 表数量（8 张：7 业务 + schema_migrations）
    # 注意：M2 的 0003 迁移**不新增表**——只给 incident_alerts 加 needs_review 列 +
    # 部分索引，并往既有的 correlation_rules 种一行 default 配置。
    # 所以 M2 之后这里仍然是 8，不是「断言过期」，别顺手把它改成 9。
    rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                     "-t", "-A", "-c",
                     "SELECT count(*) FROM information_schema.tables "
                     "WHERE table_schema='public';", timeout=60)
    check("public schema 表数 = 8", rc == 0 and out.strip() == "8", "实际 %s" % out.strip())

    # ------------------------------------------- 4.5 M2：关联引擎（容器内语义验收）
    # 把《IIP-M2详细开发手册》§八 的 M2 完成定义搬到容器里复验：
    # 镜像内嵌的迁移确实建好了 needs_review 与 default 配置行，
    # 且「改配置无需重启 → 下一条告警按新阈值决策」这条主链路真的跑通。
    #
    # 为什么必须验到语义层：M1 的容器验收只到「进程活着 + 表存在」，
    # 那对 M2 是不够的——关联引擎的全部价值在决策行为上，
    # 进程健康但一条告警都不聚合，也能骗过纯存活探针。
    def sql_val(sql: str) -> str:
        rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                         "-t", "-A", "-c", sql, timeout=60)
        return out.strip() if rc == 0 else ""

    def db_count(table: str) -> int:
        v = sql_val("SELECT count(*) FROM %s;" % table)
        return int(v) if v.isdigit() else -1

    # 4.5.0 干净起点：抹掉 named volume 里历次验收留下的行。
    #
    # compose 走 named volume 且 down 不加 -v（M2 有意保数据便于排查），
    # 于是**第二次**执行本脚本时，上一轮的告警/事件仍在库里：
    #   * 4.5.10 依赖维口径要求「historical = 0（无共现历史）」，脏历史会把该维从 0
    #     喂到 ≈0.53，使跨服务直接边由 0.7133 抬到 0.7667 → 断言假 FAIL。
    #     2026-10-02 实测：残留 12 条告警时得 0.7667，清空后精确回到 0.7133。
    #   * 4.5.7 / 4.5.8 这类「本轮新增 N 条」的计数断言同样会被历史行带偏。
    # 只清本脚本会写的四张业务表；schema_migrations 与 correlation_rules 保留
    #（4.5.3 依赖其中的 default 种子行）。
    # CASCADE 不可省：incident_alerts 对 alerts / incidents 有外键。
    _rc, _out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                       "-t", "-A", "-c",
                       "TRUNCATE TABLE incident_alerts, alerts, incidents, "
                       "correlation_feedback CASCADE;", timeout=60)
    check("干净起点：已清空历次验收残留行", _rc == 0,
          "实际 rc=%d %s" % (_rc, _out[:200]))
    _a, _i, _ia = db_count("alerts"), db_count("incidents"), db_count("incident_alerts")
    check("干净起点：alerts/incidents/incident_alerts 计数均为 0",
          _a == 0 and _i == 0 and _ia == 0,
          "alerts=%d incidents=%d incident_alerts=%d" % (_a, _i, _ia))

    # 4.5.1 needs_review 列：必须 NOT NULL + 默认 false（REVIEW 档落库的前提）
    got = sql_val("SELECT is_nullable||'|'||coalesce(column_default,'(无)') "
                  "FROM information_schema.columns WHERE table_schema='public' "
                  "AND table_name='incident_alerts' AND column_name='needs_review';")
    check("incident_alerts.needs_review 列存在且 NOT NULL DEFAULT false",
          got == "NO|false", "实际 %r" % got)

    # 4.5.2 部分索引：只索引 needs_review=TRUE 的行，供 M3 确认队列扫描
    got = sql_val("SELECT count(*) FROM pg_indexes WHERE schemaname='public' "
                  "AND indexname='idx_incident_alerts_needs_review';")
    check("needs_review 部分索引已建立", got == "1", "实际 %r" % got)

    # 4.5.3 default 配置行：五维权重与阈值必须逐位等于 V1.0 §5 锁定值
    # 注：JSONB 内 numeric 输出为最简形式（0.20 -> 0.2），故显式 to_char 归一为两位小数，
    #     保证断言比对的是「数值相等」而非「文本恰好同形」，且仍为精确相等比较。
    #     FM 前缀不可省：不带 FM 时 to_char 会为负号预留一格，输出 ' 0.25'（前导空格）。
    got = sql_val(
        "SELECT name||'|'||to_char((rule_config->'weights'->>'time')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'service')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'label')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'dependency')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'historical')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'thresholds'->>'merge')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'thresholds'->>'review')::numeric,'FM0.00')||'|'||enabled "
        "FROM correlation_rules WHERE name='default';")
    check("correlation_rules 有 default 行且权重/阈值为 V1.0 §5 锁定值",
          got == "default|0.25|0.25|0.20|0.20|0.10|0.80|0.50|true",
          "实际 %r" % got)

    cfg_url = "http://127.0.0.1:%d/api/v1/correlation/config" % HTTP_PORT
    hook_url = "http://127.0.0.1:%d/api/v1/alerts/webhook" % HTTP_PORT

    # 4.5.4 GET 配置：默认值须与迁移种子一致（证明读写走的是同一份真相）
    st, body = http_get(cfg_url)
    ok_get = False
    if st == 200:
        try:
            cfg = json.loads(body)
            wts, ths = cfg.get("weights", {}), cfg.get("thresholds", {})
            ok_get = (wts.get("time") == 0.25 and wts.get("service") == 0.25
                      and wts.get("label") == 0.20 and wts.get("dependency") == 0.20
                      and wts.get("historical") == 0.10
                      and ths.get("merge") == 0.80 and ths.get("review") == 0.50
                      and cfg.get("enabled") is True)
        except (json.JSONDecodeError, AttributeError):
            pass
    check("GET /api/v1/correlation/config -> 200 且为默认权重/阈值", ok_get,
          "status=%s body=%s" % (st, body[:160]))

    # 4.5.5 非法配置必须 422：静默接受会让「可调」形同虚设
    st, body = http_request(cfg_url, "PUT", b'{"weights":{"time":2}}')
    check("PUT 非法权重(time=2) -> 422", st == 422,
          "status=%s body=%s" % (st, body[:120]))

    # 4.5.6 未知字段必须 400（ADR-010）：键名打错被静默忽略是最难排查的一类故障
    st, body = http_request(cfg_url, "PUT", b'{"threshods":{"merge":0.9}}')
    check("PUT 未知字段(threshods 拼错) -> 400", st == 400,
          "status=%s body=%s" % (st, body[:120]))

    def post_alerts(run: str) -> tuple:
        """投递两条同服务(checkout)告警，返回 (status, body)。

        两条告警做成「B = A 再多一个非动态标签 shard」，两个原因：
        * A 与 B 的指纹必须不同，否则 B 会被当成重复投递而跳过（skipped），
          落库只剩 1 行，聚合断言就白测了。指纹计算会剔除
          instance/pod/container/node，所以不能靠 instance 区分，必须用非常规键。
        * A 与 B 的非动态标签集合只差一个键 → 标签维 Jaccard ≈ 0.83，
          叠加时间/服务两维满分，总分稳在 0.66 以上（最差也是 REVIEW，
          而 REVIEW 仍会链接到候选 Incident），故「有没有被关联」这条断言
          不依赖拓扑文件是否存在。
          （ADR-013 后拓扑已挂载：这一对**同服务**告警会经共同邻居 payment
           额外拿到依赖维 0.5，总分由 0.6667 升到 0.7667 —— 这不是回归。
           本处只用它判「走没走关联档」，故不因该变化调整断言；
           依赖维自身的取证见 4.5.10，那里用的是鉴别力更强的直接边。）

        run 给每轮一个互不相同的非动态标签值。若两条用完全一样的标签，
        后一条会被前一条的去重逻辑吃掉（实测 skipped:1），
        「本轮新增 2 条」的前提就不成立——这是第一版实测踩到的坑。

        注意 run 还必须带上 RUN_TOKEN：跨**轮次**唯一不够，还得跨**进程调用**唯一，
        否则第二次执行本脚本时，上一轮留在 named volume 里的 firing 行会把
        本轮告警全部吃掉（实测 skipped:2、落库 0 行）。详见 RUN_TOKEN 处注释。
        """
        labels_a = {"alertname": "HighLatency", "service": "checkout",
                    "namespace": "production", "severity": "critical",
                    "run": run}
        labels_b = dict(labels_a, shard="s1")
        alerts = [{"status": "firing", "labels": lb,
                   "annotations": {"summary": "p99 超阈"},
                   "startsAt": "2026-10-01T00:00:00Z",
                   "endsAt": "0001-01-01T00:00:00Z"}
                  for lb in (labels_a, labels_b)]
        payload = json.dumps({"version": "4", "status": "firing",
                              "groupKey": "iip-m2-container-check",
                              "receiver": "iip-webhook", "alerts": alerts},
                             ensure_ascii=False).encode("utf-8")
        return http_request(hook_url, "POST", payload)

    def max_alert_id() -> int:
        v = sql_val("SELECT coalesce(max(id),0) FROM alerts;")
        return int(v) if v.isdigit() else -1

    def links_of_new_alerts(after_id: int) -> str:
        """统计「比 after_id 更新的告警」的关联边形态。

        返回 '关联边总数|带分数的条数|涉及的事件数'；查询失败时返回空串。

        列名是 correlation_score（0001_init 里定的），**不是** score：
        第一版这里写成 score，SQL 直接报错、sql_val 返回空串，
        于是「断言失败」看着像产品缺陷、实为脚本自己写错了列名。
        故这里保留空串这个可辨识的失败形态，并在断言信息里点明，
        避免把脚本 bug 误读成产品 FAIL。
        """
        return sql_val("SELECT count(*)||'|'||count(correlation_score)||'|'||"
                       "count(DISTINCT incident_id) FROM incident_alerts "
                       "WHERE alert_id > %d;" % after_id)

    def bad_sql_note(got: str) -> str:
        return "SQL 查询失败（脚本问题，非产品缺陷）" if got == "" \
            else "实际 %r" % got

    # 4.5.7 先把两个阈值一起抬到 0.99 = 等价于「关闭聚合」。
    #
    # 必须**同时**抬 review：只抬 merge 的话，分数落在 [0.50, merge) 的告警
    # 会走 REVIEW 档——按设计 REVIEW 仍然链接到候选 Incident（只是打标待确认），
    # 于是「不聚合」的预期不会成立，断言会误判成产品缺陷。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.99,"review":0.99}}')
    check("PUT merge/review=0.99 -> 200（模拟关闭聚合）", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    aid0 = max_alert_id()
    st, body = post_alerts("phase-a-" + RUN_TOKEN)
    check("POST 2 条同服务告警 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 阈值 0.99 时两条都不该被关联，各自新建 Incident：
    # 关联边 2 条、带分数的 0 条 —— 分数为空即 NEW 档（新建不产生相似度）。
    got = links_of_new_alerts(aid0)
    check("阈值 0.99：本轮 2 条告警均为 NEW 档（关联边 2 条 / 带分数 0 条）",
          got.startswith("2|0|"), bad_sql_note(got) + "（期望 2|0|*）")

    # 4.5.8 还原默认阈值 → 同样两条告警应当被关联。阈值是**即时**生效的，
    # 中途没有重启任何容器，这正是手册 §八「改配置后无需重启」的验收。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.80,"review":0.50}}')
    check("PUT 还原 merge=0.80/review=0.50 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    aid1 = max_alert_id()
    st, body = post_alerts("phase-b-" + RUN_TOKEN)
    check("POST 2 条同服务告警（阈值还原后）-> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 阈值还原后两条都应被关联到候选 Incident（走 REVIEW 或 MERGE）：
    # 关联边 2 条、带分数的 2 条 —— 分数非空即「关联档」，这就是恢复聚合的证据。
    #
    # 这里刻意**不**断言「incidents 只增 1」：本轮告警与上一轮同服务，
    # 会被关联到上一轮遗留的 open Incident 上（incidents +0），
    # 具体增减取决于库里的候选分布——断言它就等于把断言绑死在偶然状态上，
    # 而「有没有走关联档」才是配置热生效真正的判据。
    got = links_of_new_alerts(aid1)
    check("阈值还原：本轮 2 条告警均被关联（关联边 2 条 / 带分数 2 条）",
          got.startswith("2|2|"), bad_sql_note(got) + "（期望 2|2|*）")

    # 4.5.9 needs_review 与关联分数区间必须自洽：
    # 只有 REVIEW 档（0.50 ≤ score < 0.80）才允许打标。这条不变量能同时抓住
    # 「REVIEW 忘了打标」和「MERGE 被误打标」两个方向上的缺陷。
    got = sql_val("SELECT count(*) FROM incident_alerts "
                  "WHERE correlation_score IS NOT NULL "
                  "AND needs_review <> (correlation_score >= 0.50 "
                  "AND correlation_score < 0.80);")
    check("needs_review 与关联分数区间自洽（仅 REVIEW 档打标）", got == "0",
          bad_sql_note(got))

    # 4.5.10 依赖维必须真正参与评分（ADR-013：拓扑改由 compose 挂载，不再缺失）。
    #
    # 判据分两层，缺一不可：
    #   ① 启动日志不再出现「服务拓扑为空」/「加载服务拓扑失败」——证明文件被读到且解析成功；
    #      只做 ① 会漏掉「文件在、内容却没用上」。
    #   ② 分数里确实含依赖维——靠一对**跨服务但有直接依赖边**的告警取证。
    #
    # 分数口径（V1.0 §5；权重 time .25 / service .25 / label .20 / dependency .20 / historical .10）：
    #   time       = 0.25   （两条 startsAt 相同、endsAt 均为 NULL → 按 now 兜底，窗口重叠比 1.0）
    #   service    = 0.15   （不同 service 但同 namespace → 0.6 × 0.25）
    #   label      = 0.1333 （k=v 对做 Jaccard：只差 service=... 一对，交集 4 / 并集 6 → 0.6667 × 0.20）
    #   dependency = 0.18   （checkout↔payment 是拓扑里的直接边 → 0.9 × 0.20）  <-- 拓扑缺失时此项为 0
    #   historical = 0      （run 标签带 RUN_TOKEN，指纹每次全新，无共现历史）
    #   合计 ≈ 0.7133；**无拓扑时上限 ≈ 0.5333** —— 故 0.70 这条线只有依赖维真参与才过得去。
    rc, out = docker("logs", "--tail", "200", "iip-server", timeout=60)
    # 优先做**正向**断言：日志是 slog JSON，结构化属性渲染成 "services":4 这种键值。
    # （M2 起成功路径不出声，只能靠「没看到 WARN」反推；ADR-013 给 main.go 补了
    #  成功日志，验收才得以写成正向证据。）
    loaded = re.search(r'"msg":"服务拓扑已加载".*?"services":(\d+)', out)
    check("server 日志出现「服务拓扑已加载」且 services>=4",
          loaded is not None and int(loaded.group(1)) >= 4,
          ("services=%s" % loaded.group(1)) if loaded
          else "未见该 INFO → 容器内 config/topology.yaml 未被挂载或为空")
    check("server 日志不含「服务拓扑为空」", "服务拓扑为空" not in out,
          "该 WARN 与上一条 INFO 不应同时出现")
    check("server 日志不含「加载服务拓扑失败」", "加载服务拓扑失败" not in out,
          "出现该 WARN → 文件在但内容无法解析")

    def post_dep_pair(run: str) -> tuple:
        """投两条**跨服务、且拓扑里有直接边**的告警（checkout / payment）。

        与上面的 post_alerts 刻意不同：那对是同服务(checkout)，在拓扑存在时
        只能靠「共同邻居 payment」拿到 0.5；本函数要的是**直接边**那 0.9，
        鉴别力更强。故两条 service 不同、且不加 shard —— 让标签维恰好只差
        service 一对，分数可以精确预测（见 4.5.10 的推导）。
        """
        base = {"alertname": "LatencyHigh", "namespace": "production",
                "severity": "critical", "run": run}
        alerts = [{"status": "firing", "labels": dict(base, service=svc),
                   "annotations": {"summary": "依赖维取证"},
                   "startsAt": "2026-10-01T00:00:00Z",
                   "endsAt": "0001-01-01T00:00:00Z"}
                  for svc in ("checkout", "payment")]
        payload = json.dumps({"version": "4", "status": "firing",
                              "groupKey": "iip-l2-topology-check",
                              "receiver": "iip-webhook", "alerts": alerts},
                             ensure_ascii=False).encode("utf-8")
        return http_request(hook_url, "POST", payload)

    aid2 = max_alert_id()
    st, body = post_dep_pair("topo-" + RUN_TOKEN)
    check("POST 2 条跨服务告警(checkout/payment) -> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 只取 payment 那一条的关联分数：它的最优匹配就是同批的 checkout 告警。
    # 用 run 标签精确定位（带 RUN_TOKEN，跨进程唯一），避免被其它轮次的行干扰。
    got = sql_val("SELECT coalesce(round(max(ia.correlation_score)::numeric,4)::text,'-') "
                  "FROM incident_alerts ia JOIN alerts a ON a.id = ia.alert_id "
                  "WHERE a.labels->>'run' = 'topo-%s' AND a.labels->>'service' = 'payment';"
                  % RUN_TOKEN)
    try:
        dep_score = float(got)
    except ValueError:
        dep_score = -1.0
    check("依赖维参与评分：跨服务直接边得分落在 [0.70, 0.73)", 0.70 <= dep_score < 0.73,
          "实际 %s（期望 ≈0.7133；拓扑缺失时上限 ≈0.5333）%s"
          % (got, bad_sql_note(got)))
    w()

    got = sql_val("SELECT coalesce(round(max(correlation_score)::numeric,4)::text,'-') "
                  "FROM incident_alerts;")
    w("        本次最高关联得分 = %s" % got)
    w("        incidents 总数 = %d, incident_alerts 总数 = %d"
      % (db_count("incidents"), db_count("incident_alerts")))
    w()

    # 4.6 worker 心跳（60s 周期，需等一轮）
    w("  等待 worker 心跳（最长 90s）...")
    rc, out = docker("logs", "--tail", "40", "iip-worker", timeout=60)
    got_beat = "worker alive" in out
    if not got_beat:
        deadline = time.time() + 90
        while time.time() < deadline:
            time.sleep(15)
            rc, out = docker("logs", "--tail", "40", "iip-worker", timeout=60)
            if "worker alive" in out:
                got_beat = True
                break
    hb = re.search(r"worker alive, poll_count=(\d+)", out)
    check("worker 输出心跳日志", got_beat,
          ("poll_count=%s" % hb.group(1)) if hb else "90s 内未见心跳")

    check("worker 已连上 PostgreSQL", "已连接 PostgreSQL" in out)
    w()

    # ------------------------------------------------------ 5. 证据留存
    w("### [5] 日志证据")
    for svc in ("iip-server", "iip-worker"):
        rc, out = docker("logs", "--tail", "25", svc, timeout=60)
        w("  ---- %s (末 25 行) ----" % svc)
        for line in out.splitlines()[-25:]:
            w("    " + line)
    w()

    try:
        rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                         "-t", "-A", "-c",
                         "SELECT tablename FROM pg_tables WHERE schemaname='public' "
                         "ORDER BY tablename;", timeout=60)
        w("  容器内 iip 库的表清单：")
        for line in out.splitlines():
            if line.strip():
                w("    " + line.strip())
    except Exception:  # noqa: BLE001
        pass
    w()

    # --------------------------------------------------------- 6. 结论
    w("=" * 70)
    if FAILURES:
        w(" 结论：FAIL  （%d 项未通过）" % len(FAILURES))
        for f in FAILURES:
            w("   - %s" % f)
    else:
        w(" 结论：PASS  （全部断言通过）")
        w()
        w(" 一键启动（保持运行）：")
        w("   docker compose -f deploy/docker-compose.yml --profile app up -d")
        w(" 访问：")
        w("   curl.exe http://127.0.0.1:%d/healthz" % HTTP_PORT)
    w("=" * 70)
    return 1 if FAILURES else 0


if __name__ == "__main__":
    code = 1
    try:
        code = main()
    finally:
        # 除明确要求保留外，一律清理，避免残留容器占端口影响后续验证
        w()
        if KEEP:
            w("### 保留容器（IIP_KEEP=1），未清理")
        else:
            w("### 清理：compose down（保留 named volume 数据）")
            rc, out = compose("--profile", "app", "down", timeout=300)
            w("  rc=%s %s" % (rc, out[:200]))
        try:
            with open(REPORT, "w", encoding="utf-8") as fh:
                fh.write("\n".join(L))
        except OSError:
            pass
    sys.exit(code)
