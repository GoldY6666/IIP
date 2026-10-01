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
    """
    sk = socket.socket()
    sk.settimeout(1.0)
    try:
        sk.connect(("127.0.0.1", port))
        return True
    except OSError:
        return False
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
    got = sql_val(
        "SELECT name||'|'||(rule_config->'weights'->>'time')||'|'"
        "||(rule_config->'weights'->>'service')||'|'"
        "||(rule_config->'weights'->>'label')||'|'"
        "||(rule_config->'weights'->>'dependency')||'|'"
        "||(rule_config->'weights'->>'historical')||'|'"
        "||(rule_config->'thresholds'->>'merge')||'|'"
        "||(rule_config->'thresholds'->>'review')||'|'||enabled "
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

    def post_alerts(mark: str | None) -> tuple:
        """投递两条同服务(checkout)告警，返回 (status, body)。

        两条告警刻意只在**非动态**标签上不同：
        指纹计算会剔除 instance/pod/container/node，若两条只差 instance，
        它们会撞成同一指纹而被去重成 1 条——那这条端到端断言就白测了。
        故用 shard（非常规动态键）区分，保证落库 2 行。
        """
        labels_a = {"alertname": "HighLatency", "service": "checkout",
                    "namespace": "production", "severity": "critical"}
        alerts = [{"status": "firing", "labels": labels_a,
                   "annotations": {"summary": "p99 超阈"},
                   "startsAt": "2026-10-01T00:00:00Z",
                   "endsAt": "0001-01-01T00:00:00Z"}]
        if mark:
            labels_b = dict(labels_a, shard=mark)
            alerts.append({"status": "firing", "labels": labels_b,
                           "annotations": {"summary": "p99 超阈（同服务另一分片）"},
                           "startsAt": "2026-10-01T00:00:00Z",
                           "endsAt": "0001-01-01T00:00:00Z"})
        payload = json.dumps({"version": "4", "status": "firing",
                              "groupKey": "iip-m2-container-check",
                              "receiver": "iip-webhook", "alerts": alerts},
                             ensure_ascii=False).encode("utf-8")
        return http_request(hook_url, "POST", payload)

    # 4.5.7 先把两个阈值一起抬到 0.99 = 等价于「关闭聚合」。
    #
    # 必须**同时**抬 review：只抬 merge 的话，分数落在 [0.50, merge) 的告警
    # 会走 REVIEW 档——按设计 REVIEW 仍然链接到候选 Incident（只是打标待确认），
    # 于是「不合并」的预期不会成立，断言会误判成产品缺陷。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.99,"review":0.99}}')
    check("PUT merge/review=0.99 -> 200（模拟关闭聚合）", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    inc0, ia0 = db_count("incidents"), db_count("incident_alerts")
    st, body = post_alerts("b")
    check("POST 2 条同服务告警 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    inc1, ia1 = db_count("incidents"), db_count("incident_alerts")
    check("阈值 0.99 时两条告警各自新建 Incident（不聚合）",
          inc1 - inc0 == 2 and ia1 - ia0 == 2,
          "incidents +%d, incident_alerts +%d" % (inc1 - inc0, ia1 - ia0))
    got = sql_val("SELECT count(*) FROM incident_alerts WHERE score IS NULL;")
    check("NEW 档的关联边 score 为空（新建无相似度可言）", got == "2",
          "score IS NULL 条数 = %r" % got)

    # 4.5.8 还原默认阈值 → 同样两条告警应当聚合。阈值是**即时**生效的，
    # 中途没有重启任何容器，这正是手册 §八「改配置后无需重启」的验收。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.80,"review":0.50}}')
    check("PUT 还原 merge=0.80/review=0.50 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    inc2, ia2 = db_count("incidents"), db_count("incident_alerts")
    st, body = post_alerts("c")
    check("POST 2 条同服务告警（阈值还原后）-> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    inc3, ia3 = db_count("incidents"), db_count("incident_alerts")
    check("阈值还原后两条告警聚合为同一 Incident（配置热生效，无重启）",
          inc3 - inc2 == 1 and ia3 - ia2 == 2,
          "incidents +%d, incident_alerts +%d" % (inc3 - inc2, ia3 - ia2))

    # 4.5.9 needs_review 与 score 区间必须自洽：
    # 只有 REVIEW 档（0.50 ≤ score < 0.80）才允许打标。这条不变量能同时抓住
    # 「REVIEW 忘了打标」和「MERGE 被误打标」两个方向的缺陷。
    got = sql_val("SELECT count(*) FROM incident_alerts WHERE score IS NOT NULL "
                  "AND needs_review <> (score >= 0.50 AND score < 0.80);")
    check("needs_review 与 score 区间自洽（仅 REVIEW 档打标）", got == "0",
          "不自洽条数 = %r" % got)

    got = sql_val("SELECT coalesce(round(max(score)::numeric,4)::text,'-') "
                  "FROM incident_alerts;")
    w("        本次最高关联得分 = %s" % got)
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
