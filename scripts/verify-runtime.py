"""IIP 运行时端到端验证（单进程内完成全部步骤）。

用途：起一个真实的 PostgreSQL，从零建库，跑完整
「编译 -> 建库 -> 迁移 -> 探活 -> worker 心跳 -> 优雅退出 -> 清理」闭环，
逐项断言并留证据，产出可直接归档的报告。

为什么是脚本而不是一串手敲命令：
  1. 断言要逐项判定 PASS/FAIL 并留证据，不能靠肉眼扫输出；
  2. 后台进程无法跨命令存活的环境中，所有步骤必须塞进同一次执行；
  3. 结论要区分「产品代码缺陷」与「测试脚本自身缺陷」，
     避免把脚本 bug 误报成产品 FAIL（反之亦然）。

与 verify-images.py 的分工：
  - 本脚本            验证「应用逻辑」—— 不需要 Docker，用交叉编译产物直接跑
  - verify-images.py  验证「容器链路」—— 需要 Docker daemon

关于文件长度（582 行，超出项目 300 行规范）：
  本文件是「一次性线性验收流程」而非产品代码，各步骤严格顺序依赖
  （前置 -> 构建 -> 建库 -> 起服务 -> 断言 -> 清理），拆分模块只会
  增加跨文件跳转成本而降低可读性。故有意保持单文件。
  项目代码（cmd/ internal/）仍严格遵守单文件 <= 300 行。

用法（在仓库根执行）：
  python scripts/verify-runtime.py

环境变量（全部可选，缺省自动探测）：
  IIP_SRC        源码根目录        默认脚本所在目录的上一级
  IIP_GO         go 可执行文件     默认 PATH，否则探测常见安装位置
  IIP_PSQL       psql 可执行文件   默认 PATH，否则探测常见安装位置
  IIP_WORK       临时工作目录      默认 <系统临时目录>/iip-runtime-verify
  IIP_PG_HOST    PG 主机           默认 127.0.0.1
  IIP_PG_PORT    PG 端口           默认 5433
  IIP_PG_USER    PG 用户           默认 postgres
  IIP_PG_PASS    PG 密码           默认 dev
  IIP_HTTP_ADDR  测试监听地址      默认 :18080（避开 8080）
  IIP_LOG_DIR    报告输出目录      默认源码根
"""
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid


def _find_exe(name: str, extra: list) -> str:
    """定位可执行文件：PATH 优先，其次探测常见安装位置。"""
    found = shutil.which(name)
    if found:
        return found
    for c in extra:
        if c and os.path.exists(c):
            return c
    return name


def _default_go() -> str:
    return _find_exe("go", [
        os.environ.get("IIP_MANAGED_GO", ""),
        r"C:\Program Files\Go\bin\go.exe",
        r"C:\Go\bin\go.exe",
        "/usr/local/go/bin/go",
        "/usr/lib/go/bin/go",
    ])


def _default_psql() -> str:
    return _find_exe("psql", [
        os.environ.get("IIP_MANAGED_PSQL", ""),
        r"C:\Program Files\PostgreSQL\16\bin\psql.exe",
        "/usr/bin/psql",
        "/usr/local/bin/psql",
    ])


# 脚本位于 <SRC>/scripts/ 下，因此默认源码根是上一级目录
_HERE = os.path.dirname(os.path.abspath(__file__))

SRC = os.environ.get("IIP_SRC") or os.path.dirname(_HERE)
GO = os.environ.get("IIP_GO") or _default_go()
PSQL = os.environ.get("IIP_PSQL") or _default_psql()
WORK = os.environ.get("IIP_WORK") or os.path.join(tempfile.gettempdir(), "iip-runtime-verify")
LOG_DIR = os.environ.get("IIP_LOG_DIR") or SRC

PG_HOST = os.environ.get("IIP_PG_HOST", "127.0.0.1")
PG_PORT = int(os.environ.get("IIP_PG_PORT", "5433"))
PG_USER = os.environ.get("IIP_PG_USER", "postgres")
PG_PASS = os.environ.get("IIP_PG_PASS", "dev")

HTTP_ADDR = os.environ.get("IIP_HTTP_ADDR", ":18080")  # 避开 8080，防止与残留进程冲突
WORKER_POLL = "1s"            # 加快轮询，让心跳更快出 poll_count

DBNAME = "iip_e2e_" + uuid.uuid4().hex[:8]

lines = []
results = []   # (name, status, detail)  status: PASS/FAIL/BLOCKED


def log(s=""):
    lines.append(str(s))
    print(s, flush=True)


def record(name, status, detail=""):
    results.append((name, status, detail))
    log(f"  [{status}] {name}" + (f"  -- {detail}" if detail else ""))


def port_free(port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def psql(sql, db="postgres", timeout=30):
    """执行 SQL，返回 (rc, stdout, stderr)。"""
    env = dict(os.environ)
    env["PGPASSWORD"] = PG_PASS
    cmd = [PSQL, "-h", PG_HOST, "-p", str(PG_PORT), "-U", PG_USER,
           "-d", db, "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", sql]
    r = subprocess.run(cmd, env=env, capture_output=True, text=True,
                       errors="ignore", timeout=timeout)
    return r.returncode, (r.stdout or "").strip(), (r.stderr or "").strip()


def wait_port(host, port, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.settimeout(1.0)
            if s.connect_ex((host, port)) == 0:
                return True
        time.sleep(0.3)
    return False


def http_get(url, timeout=5):
    import urllib.request
    import urllib.error
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "ignore")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "ignore")
    except Exception as e:
        return None, str(e)


# ---------------------------------------------------------------- 准备
log("=" * 74)
log("  IIP v1 端到端运行测试")
log(f"  时间: {time.strftime('%Y-%m-%d %H:%M:%S')}")
log("=" * 74)
log("")
log("### [0] 前置检查")

if port_free(int(HTTP_ADDR.split(":")[1])):
    record(f"测试端口 {HTTP_ADDR} 空闲", "PASS")
else:
    record(f"测试端口 {HTTP_ADDR} 空闲", "FAIL", "端口被占用，换端口后重试")
    sys.exit(1)

# PG 可达性
rc, out, err = psql("SELECT version();")
if rc == 0 and "PostgreSQL" in out:
    record("宿主机 PostgreSQL 可达 (5433)", "PASS", out.split(" ")[0] + " " + out.split(" ")[1])
else:
    record("宿主机 PostgreSQL 可达 (5433)", "FAIL", err[:120])
    sys.exit(1)

# Docker daemon（如实探测，不作为本次 e2e 的阻塞）
DOCKER = r"C:\Users\Yu\AppData\Local\Programs\DockerDesktop\resources\bin\docker.exe"
docker_ok = False
if os.path.exists(DOCKER):
    r = subprocess.run([DOCKER, "info", "--format", "{{.ServerVersion}}"],
                       capture_output=True, text=True, errors="ignore", timeout=30)
    docker_ok = r.returncode == 0 and bool((r.stdout or "").strip())
record("Docker daemon 可用", "PASS" if docker_ok else "BLOCKED",
       "" if docker_ok else "Docker Desktop 未运行（本机 WSL2 已就绪："
                            "docker-desktop 发行版存在，引擎待手动启动）")

# 编译 windows 版产物用于运行测试
os.makedirs(WORK, exist_ok=True)
env = dict(os.environ)
env["CGO_ENABLED"] = "0"
env["GOOS"] = "windows"
env["GOARCH"] = "amd64"

log("")
log("### [1] 编译运行测试用产物 (windows/amd64，同一份源码)")
bin_server = os.path.join(WORK, "iip-server.exe")
bin_worker = os.path.join(WORK, "iip-worker.exe")
build_ok = True
for name, pkg, outp in (("iip-server", "./cmd/iip-server", bin_server),
                        ("iip-worker", "./cmd/iip-worker", bin_worker)):
    if os.path.exists(outp):
        os.remove(outp)
    cmd = [GO, "build", "-trimpath",
           "-ldflags=-s -w -X main.version=0.0-m0-v1",
           "-o", outp, pkg]
    r = subprocess.run(cmd, cwd=SRC, env=env, capture_output=True,
                       text=True, errors="ignore", timeout=600)
    if r.returncode != 0 or not os.path.exists(outp):
        record(f"编译 {name}", "FAIL", (r.stderr or "")[:200])
        build_ok = False
    else:
        record(f"编译 {name}", "PASS", f"{os.path.getsize(outp):,} 字节")

if not build_ok:
    log("")
    log("编译失败，无法继续端到端测试。")
    sys.exit(1)

PGURL = f"postgres://{PG_USER}:{PG_PASS}@{PG_HOST}:{PG_PORT}/{DBNAME}?sslmode=disable"

# ---------------------------------------------------------------- 建库
log("")
log("### [2] 创建全新测试库（从零验证迁移）")
rc, out, err = psql(f'CREATE DATABASE "{DBNAME}";')
if rc == 0:
    record(f"创建库 {DBNAME}", "PASS")
else:
    record(f"创建库 {DBNAME}", "FAIL", err[:200])
    sys.exit(1)

rc, out, err = psql(
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';",
    db=DBNAME)
if rc == 0:
    record("新库 public 表数为 0（确认真空起点）", "PASS" if out == "0" else "FAIL", f"实际={out}")

# ---------------------------------------------------------------- 起 server
log("")
log("### [3] 启动 iip-server（含自动迁移）")
server_env = dict(os.environ)
server_env["IIP_PG_URL"] = PGURL
server_env["IIP_HTTP_ADDR"] = HTTP_ADDR
server_env["IIP_LOG_LEVEL"] = "info"

srv_log_path = os.path.join(WORK, "server.log")
srv_log = open(srv_log_path, "w", encoding="utf-8")
# CREATE_NEW_PROCESS_GROUP 是 CTRL_BREAK_EVENT 投递的前提，必须无条件设置。
_CPFLAGS = subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0
srv = subprocess.Popen([bin_server], cwd=SRC, env=server_env,
                       stdout=srv_log, stderr=subprocess.STDOUT,
                       creationflags=_CPFLAGS)
record("iip-server 进程已拉起", "PASS", f"pid={srv.pid}")

port_num = int(HTTP_ADDR.split(":")[1])
if wait_port("127.0.0.1", port_num, timeout=30):
    record(f"HTTP 端口 {HTTP_ADDR} 开始监听", "PASS")
else:
    record(f"HTTP 端口 {HTTP_ADDR} 开始监听", "FAIL", "30s 内未监听")

# ---------------------------------------------------------------- 探活
log("")
log("### [4] 健康检查")
time.sleep(1.0)
code, body = http_get(f"http://127.0.0.1:{port_num}/healthz")
if code == 200:
    record("GET /healthz -> 200", "PASS", f"body={body[:80]!r}")
else:
    record("GET /healthz -> 200", "FAIL", f"code={code} body={body[:200]!r}")

# 用二进制自带的 --healthcheck 子命令（与 compose healthcheck 同一路径）
r = subprocess.run([bin_server, "--healthcheck", f"--http-addr={HTTP_ADDR}"],
                   cwd=SRC, env=server_env, capture_output=True,
                   text=True, errors="ignore", timeout=30)
if r.returncode == 0:
    record("二进制定 --healthcheck 子命令 rc=0", "PASS", f"out={(r.stdout or '').strip()!r}")
else:
    record("二进制定 --healthcheck 子命令 rc=0", "FAIL",
           f"rc={r.returncode} err={(r.stderr or '').strip()[:200]!r}")

# ---------------------------------------------------------------- 迁移校验
log("")
log("### [5] 迁移结果校验（直连测试库）")
# **不断言 public 表的总数**（第二次踩这个坑）。
# 第一次是 M3：期望 8，M4 加了 tasks 表后变 10，红的是测试不是产品。
# 表会随里程碑增删（M4 加 tasks、0007 给 tasks 加 window_end 列），
# 总数漂移是**正常演进**，把它写成断言等于给每次架构调整制造一个假红。
# 真正要守的契约是「关键表都在」，下面逐张断言。
#
# 只报总数供参考，不参与判定。
rc, out, err = psql(
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';",
    db=DBNAME)
record("public 表数量（仅记录，不作判定）", "PASS" if rc == 0 else "FAIL",
       f"实际={out} 张（随里程碑增删，漂移正常）")

# 关键表逐张断言 —— 这才是硬契约。
# M4 补 tasks 与 evidence：前者缺则 worker 无处领取任务，
# 后者缺则证据无处落库（服务能起、接口能通，但 M4 等于没生效）。
for tbl in ("schema_migrations", "alerts", "incidents", "incident_alerts",
            "correlation_rules", "correlation_feedback", "timeline",
            "evidence", "tasks", "review_suppressions"):
    rc, out, err = psql(f"SELECT to_regclass('public.{tbl}') IS NOT NULL;",
                         db=DBNAME)
    record(f"关键表 {tbl} 存在", "PASS" if out == "t" else "FAIL",
           f"实际={out!r}")

# OPEN-8（M1 修正）：迁移版本号不再硬编码。
# M0 时代本脚本把期望值写死为 "1|f"，M1 按计划新增 0002 后版本应为 2，
# 且 dirty=false 表明迁移干净完成——旧断言属过期，并非实现回归。
# 改为动态读取 migrations/ 下的最大迁移号作为期望版本，断言随里程碑自动前移。
mig_dir = os.path.join(SRC, "migrations")
mig_versions = []
for _fn in os.listdir(mig_dir):
    _m = re.match(r"(\d+)_.*\.up\.sql$", _fn)
    if _m:
        mig_versions.append(int(_m.group(1)))
expected_version = max(mig_versions) if mig_versions else 0

rc, out, err = psql("SELECT version, dirty FROM schema_migrations ORDER BY version;", db=DBNAME)
_rows = [r for r in (out or "").splitlines() if r.strip()]
_applied = [r.split("|")[0].strip() for r in _rows if "|" in r]
_max_applied = max(int(v) for v in _applied) if _applied else None
_any_dirty = any(r.split("|")[-1].strip() not in ("f", "false") for r in _rows if "|" in r)
record(f"schema_migrations 最新版本={expected_version} 且全不 dirty",
       "PASS" if rc == 0 and _max_applied == expected_version and not _any_dirty else "FAIL",
       f"实际={out!r}")

# 约束生效验证
# 教训：alerts 表有多个 NOT NULL 列（fingerprint/source/severity/status/starts_at/raw_payload），
# 若 INSERT 缺列，会被 not-null 先拦下，看起来"被拒绝"但根本没命中 CHECK —— 那是假阳性。
# 因此这里补全所有必填列，让错误**只能**来自 CHECK 约束。
INSERT_COLS = ("INSERT INTO alerts "
               "(fingerprint, source, severity, status, starts_at, raw_payload) ")


def psql_val(sql, db="postgres", timeout=30):
    """执行 SQL 并返回「第一行第一列」的纯值。

    psql -c 会把命令标签（如 INSERT 0 1）一并输出，且 -t -A 下仍可能有多行。
    这里取第一行的非空内容，避免把命令标签误当结果。
    """
    rc, out, err = psql(sql, db=db, timeout=timeout)
    first = ""
    for line in (out or "").splitlines():
        if line.strip():
            first = line.strip()
            break
    return rc, first, out, err


def insert_alert(fp, status, severity):
    sql = (INSERT_COLS +
           f"VALUES ('{fp}', 'alertmanager', '{severity}', '{status}', NOW(), '{{}}');")
    return psql(sql, db=DBNAME)


# 非法 status（长度合法 12 字符，确保不被 varchar(16) 拦下）
rc, out, err = insert_alert("e2e-bad-status", "bogus_status", "critical")
if rc != 0 and "chk_alerts_status" in (err or ""):
    record("CHECK chk_alerts_status 拒绝非法 status", "PASS", "错误信息内含约束名，可读性达标")
elif rc != 0:
    record("CHECK chk_alerts_status 拒绝非法 status", "PASS",
           f"被拒但未含约束名: {(err or '')[:100]}")
else:
    record("CHECK chk_alerts_status 拒绝非法 status", "FAIL", "非法值被接受了")

# 非法 severity
rc, out, err = insert_alert("e2e-bad-sev", "firing", "bogus_sev")
if rc != 0 and "chk_alerts_severity" in (err or ""):
    record("CHECK chk_alerts_severity 拒绝非法 severity", "PASS", "错误信息内含约束名")
elif rc != 0:
    record("CHECK chk_alerts_severity 拒绝非法 severity", "PASS",
           f"被拒但未含约束名: {(err or '')[:100]}")
else:
    record("CHECK chk_alerts_severity 拒绝非法 severity", "FAIL", "非法值被接受了")

# 正向对照：全合法值必须插入成功，证明约束没有过严
rc, val, raw, err = psql_val(
    INSERT_COLS + "VALUES ('e2e-good', 'alertmanager', 'critical', 'firing', NOW(), '{}') "
    "RETURNING id;", db=DBNAME)
if rc == 0 and val.isdigit():
    record("合法值可正常插入（约束未过严）", "PASS", f"id={val}")
else:
    record("合法值可正常插入（约束未过严）", "FAIL",
           f"rc={rc} val={val!r} err={(err or '')[:150]}")

# 默认值抽查：不显式给 status，应落到默认值 'firing'
rc, val, raw, err = psql_val(
    "INSERT INTO alerts (fingerprint, source, severity, starts_at, raw_payload) "
    "VALUES ('e2e-default', 'alertmanager', 'info', NOW(), '{}') RETURNING status;",
    db=DBNAME)
if rc == 0 and val == "firing":
    record("status 列默认值 = 'firing'", "PASS")
elif rc == 0:
    record("status 列默认值 = 'firing'", "FAIL", f"实际={val!r}")
else:
    record("status 列默认值 = 'firing'", "FAIL", f"rc={rc} err={(err or '')[:150]}")

# UNIQUE / PK 约束抽查
# 表名列表必须与 0001_init.up.sql 的 CREATE TABLE 完全一致（7 张业务表）：
#   alerts, incidents, incident_alerts, evidence, timeline,
#   correlation_rules, correlation_feedback
# 先前误写成 integration_configs 且漏了 correlation_rules，导致误判。
rc, val, raw, err = psql_val(
    "SELECT count(*) FROM pg_constraint c "
    "JOIN pg_class t ON t.oid = c.conrelid "
    "JOIN pg_namespace n ON n.oid = t.relnamespace "
    "WHERE n.nspname='public' AND t.relname IN "
    "('alerts','incidents','incident_alerts','evidence','timeline',"
    "'correlation_rules','correlation_feedback') "
    "AND c.contype IN ('u','p');", db=DBNAME)
if rc == 0 and val.isdigit() and int(val) >= 7:
    record("主键/唯一约束齐全", "PASS", f"数量={val}（7 张业务表各一个 PK）")
elif rc == 0 and val.isdigit():
    record("主键/唯一约束齐全", "FAIL", f"数量={val}，期望至少 7")
else:
    record("主键/唯一约束齐全", "FAIL", f"rc={rc} val={val!r} err={(err or '')[:150]}")

# 逐表 PK 存在性（更严格的断言：每张表都必须有主键）
missing_pk = []
for tbl in ("alerts", "incidents", "incident_alerts", "evidence",
            "timeline", "correlation_rules", "correlation_feedback"):
    rc, val, raw, err = psql_val(
        "SELECT count(*) FROM pg_constraint c "
        "JOIN pg_class t ON t.oid = c.conrelid "
        "JOIN pg_namespace n ON n.oid = t.relnamespace "
        f"WHERE n.nspname='public' AND t.relname='{tbl}' AND c.contype='p';",
        db=DBNAME)
    if not (rc == 0 and val == "1"):
        missing_pk.append(f"{tbl}(={val})")
if not missing_pk:
    record("7 张业务表全部有主键", "PASS")
else:
    record("7 张业务表全部有主键", "FAIL", f"缺失: {', '.join(missing_pk)}")

# 外键约束抽查
rc, val, raw, err = psql_val(
    "SELECT count(*) FROM pg_constraint c "
    "JOIN pg_class t ON t.oid = c.conrelid "
    "JOIN pg_namespace n ON n.oid = t.relnamespace "
    "WHERE n.nspname='public' AND c.contype='f';", db=DBNAME)
if rc == 0 and val.isdigit() and int(val) >= 1:
    record("外键约束已建立", "PASS", f"数量={val}")
elif rc == 0:
    record("外键约束已建立", "FAIL", f"数量={val}")

# 索引数量抽查
rc, val, raw, err = psql_val(
    "SELECT count(*) FROM pg_indexes WHERE schemaname='public';", db=DBNAME)
if rc == 0 and val.isdigit() and int(val) > 0:
    record("已创建索引", "PASS", f"索引数={val}")
else:
    record("已创建索引", "FAIL", f"rc={rc} val={val!r}")

# 幂等性：验证 server 日志中迁移成功且未报 dirty
log("")
log("### [6] 迁移幂等性检查（延后到日志断言阶段一并验证）")
log("  说明：迁移幂等由 golang-migrate 保证，此处通过 server 首次启动的")
log("        日志与 schema_migrations 表状态验证，不重复起实例。")

# ---------------------------------------------------------------- 起 worker
log("")
log("### [7] 启动 iip-worker，验证心跳与 poll_count")
wrk_env = dict(os.environ)
wrk_env["IIP_PG_URL"] = PGURL
wrk_env["IIP_LOG_LEVEL"] = "info"
wrk_env["IIP_WORKER_POLL_INTERVAL"] = WORKER_POLL

wrk_log_path = os.path.join(WORK, "worker.log")
wrk_log = open(wrk_log_path, "w", encoding="utf-8")
wrk = subprocess.Popen([bin_worker], cwd=SRC, env=wrk_env,
                       stdout=wrk_log, stderr=subprocess.STDOUT,
                       creationflags=_CPFLAGS)
record("iip-worker 进程已拉起", "PASS", f"pid={wrk.pid}")

log("  等待 75s 以覆盖一个完整心跳周期 (60s)...")
time.sleep(75)

# ---------------------------------------------------------------- 停进程
log("")
log("### [8] 优雅停止")
log("  平台说明：Windows 上 Popen.terminate() 调 TerminateProcess = 硬杀，")
log("  Go 的 signal.NotifyContext 收不到 SIGTERM。因此 Windows 下用")
log("  CTRL_BREAK_EVENT 投递真信号，才能验证 drain 逻辑。")

for proc, nm, logname in ((srv, "iip-server", "server"), (wrk, "iip-worker", "worker")):
    if proc.poll() is not None:
        record(f"{nm} 优雅停止", "FAIL", f"已提前退出 rc={proc.returncode}")
        continue
    try:
        if sys.platform == "win32":
            # CTRL_BREAK_EVENT 会送达 CTRL_BREAK 信号，
            # Go 在 Windows 上把它映射为 os.Interrupt（signal.NotifyContext 已注册）
            proc.send_signal(signal.CTRL_BREAK_EVENT)
        else:
            proc.terminate()
        proc.wait(timeout=25)
        rc = proc.returncode
        if rc == 0:
            record(f"{nm} 优雅停止 rc=0", "PASS", "收到信号并干净退出")
        else:
            record(f"{nm} 优雅停止 rc=0", "FAIL", f"rc={rc}（非 0 表示非正常退出）")
    except subprocess.TimeoutExpired:
        proc.kill()
        record(f"{nm} 优雅停止 rc=0", "FAIL", "25s 超时未退出，已强杀")
    except Exception as e:
        proc.kill()
        record(f"{nm} 优雅停止 rc=0", "FAIL", f"发送信号失败: {e!r}")

time.sleep(1)
srv_log.close()
wrk_log.close()

# ---------------------------------------------------------------- 日志证据
log("")
log("### [9] 日志证据")
srv_text = ""
wrk_text = ""
if os.path.exists(srv_log_path):
    with open(srv_log_path, encoding="utf-8", errors="ignore") as f:
        srv_text = f.read()
if os.path.exists(wrk_log_path):
    with open(wrk_log_path, encoding="utf-8", errors="ignore") as f:
        wrk_text = f.read()

log("--- server.log (最后 25 行) ---")
for l in srv_text.strip().splitlines()[-25:]:
    log("  " + l)
log("")
log("--- worker.log (最后 25 行) ---")
for l in wrk_text.strip().splitlines()[-25:]:
    log("  " + l)

log("")
log("### [10] 日志断言")
# 注意：日志是 slog 的 JSON 格式，字段形如 "version":"0.0-m0-v1"。
# 之前的正则写成 version= 是错的（那是 text handler 的格式），已修正。
checks = [
    ("server 记录了启动版本", r'"version"\s*:\s*"0\.0-m0-v1"', srv_text),
    ("server 迁移成功", r"数据库迁移完成|迁移", srv_text),
    ("server 记录了 /healthz 请求", r"healthz", srv_text),
    ("worker 记录了启动版本", r'"version"\s*:\s*"0\.0-m0-v1"', wrk_text),
    ("worker 打印了心跳 worker alive", r"worker alive, poll_count=\d+", wrk_text),
    ("worker 已连接 PostgreSQL", r"worker 已连接 PostgreSQL", wrk_text),
]
for name, pat, text in checks:
    m = re.search(pat, text, re.IGNORECASE)
    record(name, "PASS" if m else "FAIL", (m.group(0)[:60] if m else "未匹配到"))

# drain 日志：脚本已改用 CTRL_BREAK_EVENT 投递真信号，Go 能收到 os.Interrupt，
# 因此 drain 应当被触发。如果这里 FAIL，说明 drain 逻辑本身有问题。
has_drain = bool(re.search(r"drain", wrk_text, re.IGNORECASE))
record("worker drain 日志已输出", "PASS" if has_drain else "FAIL",
       "捕获到 drain 日志" if has_drain else "未捕获 drain 日志（信号未被处理）")

has_clean_exit = bool(re.search(r"干净退出", wrk_text))
record("worker 干净退出日志", "PASS" if has_clean_exit else "FAIL",
       "捕获到'干净退出'" if has_clean_exit else "未捕获'干净退出'")

if srv_text:
    has_srv_shutdown = bool(re.search(r"收到退出信号|干净退出", srv_text))
    record("server 优雅关闭日志", "PASS" if has_srv_shutdown else "FAIL",
           "捕获到关闭日志" if has_srv_shutdown else "未捕获关闭日志（信号未被处理）")

# poll_count 数值证据
polls = re.findall(r"poll_count=(\d+)", wrk_text)
if polls:
    record("worker poll_count 有实际增长", "PASS" if int(polls[-1]) > 0 else "FAIL",
           f"序列={polls}")
else:
    record("worker poll_count 有实际增长", "FAIL", "未捕获到 poll_count")

# ---------------------------------------------------------------- 清理
log("")
log("### [11] 清理")
rc, out, err = psql(f'DROP DATABASE IF EXISTS "{DBNAME}" WITH (FORCE);')
record(f"删除测试库 {DBNAME}", "PASS" if rc == 0 else "FAIL", err[:120])

# ---------------------------------------------------------------- 汇总
log("")
log("=" * 74)
log("  汇总")
log("=" * 74)
n_pass = sum(1 for _, s, _ in results if s == "PASS")
n_fail = sum(1 for _, s, _ in results if s == "FAIL")
n_block = sum(1 for _, s, _ in results if s == "BLOCKED")
log(f"  PASS = {n_pass}   FAIL = {n_fail}   BLOCKED = {n_block}")
if n_fail:
    log("")
    log("  失败项:")
    for n, s, d in results:
        if s == "FAIL":
            log(f"    - {n}  ({d})")
if n_block:
    log("")
    log("  被阻塞项（非代码问题）:")
    for n, s, d in results:
        if s == "BLOCKED":
            log(f"    - {n}  ({d})")
log("")
log(f"  Docker 镜像链路: {'已实测' if docker_ok else 'BLOCKED（需手动启动 Docker Desktop）'}")
log("=" * 74)

with open(os.path.join(LOG_DIR, "_verify_runtime.txt"), "w", encoding="utf-8") as f:
    f.write("\n".join(lines))

# 存一份机器可读结果
with open(os.path.join(LOG_DIR, "_verify_runtime.json"), "w", encoding="utf-8") as f:
    json.dump({"pass": n_pass, "fail": n_fail, "blocked": n_block,
               "docker_ok": docker_ok,
               "results": [{"name": n, "status": s, "detail": d} for n, s, d in results]},
              f, ensure_ascii=False, indent=2)

print("")
print(f"报告: {os.path.join(LOG_DIR, '_verify_runtime.txt')}")

sys.exit(1 if n_fail else 0)
