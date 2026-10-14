#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""IIP 故障演练（T7.4）—— 真实进程级故障注入 + 恢复行为验证。

与既有验证链路的分工（别混用）：
  verify-func.py    业务闭环（接入->关联->处置->抑制）与 M4 采集闭环
  verify-runtime.py 宿主机全栈单点校验（M1/M2）
  verify-images.py  容器链路与镜像内容物
  check-binaries.py 二进制完整性
  本脚本            **故障演练**：把人祸（进程崩溃、库断连、组件缺席）
                    真的造出来，看系统是否按设计恢复。

三个场景：
  D1 worker 崩溃与任务回收
     硬杀正在采集的 worker（TerminateProcess，不留 drain 机会）-> 任务
     停在 in_flight；起新 worker（stale_after=10s）-> ReclaimStale 回收 ->
     重新领取 -> 采集完成。验的是「worker 崩溃不会让队列僵死」。
     崩溃现场是**真杀出来的**，不是 SQL 预置的。
  D2 PG 断连与自动重连
     pg_ctl stop 停库约 25s -> 断连期 webhook 503、server/worker 进程
     存活、队列任务不丢；重启 PG -> **不重启 server** 自动恢复 ->
     补采落库。验的是「库抖动不拖垮进程、不丢任务」。
  D3 Redis 缺席
     静态：worker 依赖闭包与 go.mod 零 Redis 客户端（结构上不存在
     Redis 代码路径）；运行时：本演练全程未提供任何 Redis 进程/
     环境变量，D1/D2 的轮询采集闭环全部成立。

诚实声明（哪些是真的、哪些是注入的）：
  - D1/D2 的故障都是**真实注入**（真 kill / 真停库），恢复行为全部由
    产品自身完成，脚本只观测不干预（唯一例外见下条）。
  - D2 的「断连期入队」用 SQL 直插一条 available_at 在未来的任务：
    断连期 webhook 本身就是 503，走不了正常入队路径，这是**注入**而非
    伪装，已在判据 detail 里标注。
  - D2 停库用 pg_ctl stop -m fast（计划性停库）模拟断连；不模拟磁盘
    损坏/脑裂等更深故障，那些属于 PG 自身领域而非 IIP。
  - D1 为拉宽崩溃观察窗注入 --evidence-timeout=30s（生产默认 5s），
    判据 detail 里有标注；除此之外全部用生产默认值。
  - D3 的「缺席」在本机当前无 redis-server 进程的前提下观测；若本机
    跑着别的业务的 Redis，不影响判据（主判据是依赖闭包为静态事实）。

退出码：0 = 无 FAIL；1 = 有 FAIL；2 = 构建失败；3 = 环境问题（均非产品缺陷）。
"""

import io
import json
import os
import re
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

# 仓库根 = 本脚本的上级目录（脚本在 scripts/ 下），不硬编码绝对路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(_HERE)

# PostgreSQL 绿色版位置：可用环境变量覆盖，默认本机约定路径。
PG = os.environ.get("IIP_PG_HOME", r"D:\pgsql16")
PG_BIN = os.path.join(PG, "bin")
# 演练专用 PGDATA 与端口：**绝不与 verify-func(5434/D:\pgsql16\data)、
# verify-runtime(5433)、go test(5434/pgdata_unittest) 共用**——
# 两个 postmaster 共用同一份 PGDATA 会 fsync 竞态 -> PANIC -> 级联 FAIL。
PGDATA = os.environ.get("IIP_DRILL_PGDATA", r"D:\pgsql16\pgdata_s4_5436")
PORT_PG = int(os.environ.get("IIP_DRILL_PG_PORT", "5436"))
PORT_HTTP = int(os.environ.get("IIP_DRILL_HTTP_PORT", "8083"))
EXE = os.path.join(tempfile.gettempdir(), "_drill_iip-server.exe")
EXE_WORKER = os.path.join(tempfile.gettempdir(), "_drill_iip-worker.exe")
DB = "iip_drill"
DSN = "postgres://postgres:dev@127.0.0.1:%d/%s?sslmode=disable" % (PORT_PG, DB)
BASE = "http://127.0.0.1:%d" % PORT_HTTP
OUT = os.path.join(_HERE, "_fault_drill.txt")

# psql 超时：CREATE/DROP DATABASE 在慢 IO 环境实测可达 ~102s（见
# verify-func.py 的同一注释），固定 120s 会把环境问题误判成产品缺陷。
PSQL_TIMEOUT = int(os.environ.get("IIP_PSQL_TIMEOUT", "300"))

# go1.26.9 工具链绝对路径：沙箱 PATH 被清空，裸 go 会 FileNotFoundError，
# 且旧版 go 排在前面会触发 toolchain 下载（被 SIGTERM 掐断）。
TC = r"C:\Users\Yu\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.9.windows-amd64\bin"
GO_BIN = os.path.join(TC, "go.exe")
GO_ENV = {
    **os.environ,
    "GOTOOLCHAIN": "local",
    "GOCACHE": r"D:\_iip_setup\gocache",
    "GOTELEMETRY": "off",
    "GOFLAGS": "-mod=mod",
}

# 演练参数（与生产默认值的差异全部在此声明，判据 detail 里引用）：
STALE_AFTER = "10s"        # 生产默认 5m；压到 10s 让回收在本场景时限内发生
WORKER_POLL = "1s"         # 生产默认 2s
EVIDENCE_TIMEOUT = "30s"   # 生产默认 5s；拉宽 D1 崩溃观察窗
PG_DOWNTIME_S = 25         # D2 停库时长
FUTURE_TASK_DELAY_S = 45   # D2 断连期直插任务的可领时刻（相对插入时刻）

TOKEN = str(int(time.time()))  # 每轮唯一的 label，保证指纹不撞历史数据

_L = []
FAILS = []
SKIPS = []
_PSQL_ERR_SEEN = set()  # psql 失败消息去重（轮询期间同一错误只记一次）
_OUT_F = None    # 报告文件句柄：逐行落盘，进程被杀也不丢汇总行
_WORKERS = []    # 所有起过的 worker：finally 里统一收，防异常路径泄漏
_LOGF = []       # 日志文件句柄：持有引用，避免被 GC 关掉


# ================================================================
# 报告：逐行落盘 + 打印
#
# 为什么不能只攒在内存里最后写：脚本若被沙箱回收（后台任务约 10 分钟
# 上限）或异常带走，只写文件的 log 会连汇总行一起丢，看起来像「验收
# 卡住了」而不是「哪条断言失败了」。
# ================================================================

def w(s):
    global _OUT_F
    line = str(s)
    _L.append(line)
    if _OUT_F is None:
        _OUT_F = io.open(OUT, "w", encoding="utf-8", newline="\n")
    _OUT_F.write(line + "\n")
    _OUT_F.flush()
    print(line, flush=True)


def check(name, ok, detail=""):
    tag = "PASS" if ok else "FAIL"
    line = "  [%s] %s" % (tag, name)
    if detail:
        line += "  " + detail
    w(line)
    if not ok:
        FAILS.append(name)
    return ok


def skip(name, why):
    """跳过不等于通过：汇总里单列 SKIP 数，不许把跳过算进 PASS。"""
    w("  [SKIP] %s  %s" % (name, why))
    SKIPS.append(name)


def section(t):
    w("")
    w("=== %s ===" % t)


def port_open(p):
    s = socket.socket()
    s.settimeout(1.0)
    try:
        s.connect(("127.0.0.1", p))
        return True
    except OSError:
        return False
    finally:
        s.close()


# ================================================================
# SQL 助手（照 verify-func.py 已验证的形状）
# ================================================================

def psql(sql, db="postgres"):
    env = {**os.environ, "PGPASSWORD": "dev"}
    p = subprocess.run(
        [os.path.join(PG_BIN, "psql.exe"), "-h", "127.0.0.1", "-p", str(PORT_PG),
         "-U", "postgres", "-d", db, "-t", "-A", "-c", sql],
        capture_output=True, text=True, encoding="utf-8", errors="replace",
        env=env, timeout=PSQL_TIMEOUT)
    if p.returncode != 0:
        # 失败时把 stderr 落进报告：只回传 stdout 会让「SQL 写错了」
        # 和「库没连上」看起来一模一样（都只是一个非零 rc），
        # 排查时只能靠猜。按消息去重，防止轮询期间同一错误刷屏。
        msg = ((p.stderr or "") + "|" + sql[:80]).strip()
        if msg not in _PSQL_ERR_SEEN:
            _PSQL_ERR_SEEN.add(msg)
            w("  [psql rc=%d] %s" % (p.returncode, (p.stderr or "").strip()[:200]))
    return p.returncode, (p.stdout or "").strip()


def q(sql, db=DB):
    """取单值查询结果；失败返回 ERR(rc) 而不是抛异常——
    PG 断连期间所有查询都会走这个分支，脚本必须能活着观测完故障期。"""
    rc, out = psql(sql, db)
    return out if rc == 0 else "ERR(%d)" % rc


def qn(sql, db=DB):
    """取单值整数；「查询失败」「查到 0」「查到 3」必须可区分，
    所以失败返回 -1，不让 ValueError 把整个脚本带走。"""
    v = q(sql, db)
    return int(v) if v.isdigit() else -1


def req(method, path, body=None, timeout=30):
    """发 HTTP 请求，返回 (status, parsed)。

    连接失败/超时返回 (0, 错误串) 而不是抛异常：故障演练里
    「连不上」本身就是一种被测行为，不该把脚本带走。
    """
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    r = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        try:
            raw = e.read().decode("utf-8", errors="replace")
        except Exception:
            raw = ""
        status = e.code
    except Exception as e:  # noqa: BLE001 —— 连接失败也是被测行为
        return 0, "ERR: %r" % e
    try:
        return status, json.loads(raw)
    except Exception:
        return status, raw


def post_alert(alertname, service, tag, status="firing"):
    """投一条告警。tag 每轮唯一：指纹含全部 label（除 instance 等），
    tag 相同会被去重 skip，测到的就是 skip 分支而不是完整链路。"""
    labels = {"alertname": alertname, "service": service,
              "severity": "warning", "run": TOKEN, "drill": tag}
    payload = {
        "version": "4",
        "groupKey": "drill-%s" % TOKEN,
        "status": status,
        "receiver": "iip",
        "groupLabels": {},
        "commonLabels": {},
        "commonAnnotations": {},
        "externalURL": "",
        "alerts": [{
            "status": status,
            "labels": labels,
            "annotations": {"summary": "故障演练 %s" % tag},
            "startsAt": "2026-10-03T00:00:00Z",
            "endsAt": "2026-10-04T00:00:00Z",
            "generatorURL": "",
        }],
    }
    return req("POST", "/api/v1/alerts/webhook", payload)


# ================================================================
# 假采集器（照 verify-func.py 的形状，加 delay 故障注入）
#
# 用进程内假 HTTP 服务而不是 httptest：端到端验的就是「worker 拿着
# 配置里的 URL 真的把请求发出去」。
# delay > 0 时每个请求先睡 delay 秒——D1 靠它把任务按在 in_flight
# 状态里，制造「worker 采集到一半被杀」的崩溃现场。
# ================================================================

_PROM_SEEN = {}


def _prom_handler(path):
    """较晚的查询返 10、较早的返 1，让基线对比有区分度。"""
    import urllib.parse as up
    qs = up.parse_qs(up.urlparse(path).query)
    try:
        ts = int((qs.get("time") or ["0"])[0])
    except ValueError:
        ts = 0
    key = (qs.get("query") or [""])[0]
    seen = _PROM_SEEN.get(key)
    if seen is None or ts > seen:
        _PROM_SEEN[key] = ts
        val = "10"
    else:
        val = "1"
    return {"status": "success",
            "data": {"resultType": "vector",
                     "result": [{"metric": {}, "value": [ts, val]}]}}


def _loki_handler(path):
    import urllib.parse as up
    qs = up.parse_qs(up.urlparse(path).query)
    try:
        start_ns = int((qs.get("start") or ["0"])[0])
    except ValueError:
        start_ns = 0
    t0 = start_ns // 1000000000
    if t0 <= 0:
        t0 = int(time.time())
    return {"status": "success",
            "data": {"resultType": "streams",
                     "result": [{
                         "stream": {"service": "checkout"},
                         "values": [
                             ["%d000000000" % (t0 + 60), "db timeout after 5000ms"],
                             ["%d000000000" % (t0 + 120), "db timeout after 7000ms"],
                         ]}]}}


class _MockCollector(object):
    def __init__(self, name, handler):
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        self.name = name
        self.handler = handler
        self.delay = 0.0
        self.hits = 0
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler 规定名
                outer.hits += 1
                if outer.delay > 0:
                    time.sleep(outer.delay)
                raw = json.dumps(outer.handler(self.path)).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *_a):
                pass

        # handle_error 压掉 traceback：D1 里 worker 被硬杀时它发起的请求
        # 会断在 mock 侧，标准库默认往 stderr 打一整页——那是演练的预期
        # 产物不是缺陷，刷进报告只会淹没真正的判据。
        srv_cls = type("QuietThreadingHTTPServer", (ThreadingHTTPServer,), {
            "handle_error": lambda self, request, client_address: None,
            "daemon_threads": True,
        })
        self._srv = srv_cls(("127.0.0.1", 0), H)
        self.port = self._srv.server_address[1]
        threading.Thread(target=self._srv.serve_forever, daemon=True).start()
        w("  假 %s 已起：127.0.0.1:%d" % (name, self.port))

    @property
    def url(self):
        return "http://127.0.0.1:%d" % self.port

    def stop(self):
        try:
            self._srv.shutdown()
        except Exception:
            pass


# ================================================================
# 进程管理
# ================================================================

def pg_ready():
    ir = subprocess.run(
        [os.path.join(PG_BIN, "pg_isready.exe"), "-h", "127.0.0.1", "-p", str(PORT_PG)],
        capture_output=True, timeout=10)
    return ir.returncode == 0


def start_pg():
    """起 PG（Popen 子进程，与脚本同进程树——跨调用起会被沙箱回收）。"""
    logf = open(os.path.join(PGDATA, "pg_drill.log"), "ab")
    pg = subprocess.Popen(
        [os.path.join(PG_BIN, "postgres.exe"), "-D", PGDATA, "-p", str(PORT_PG)],
        stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN)
    t0 = time.time()
    for _ in range(180):
        if pg_ready():
            w("  PG 就绪（%.1fs, pid=%d）" % (time.time() - t0, pg.pid))
            return pg
        if pg.poll() is not None:
            w("  !! PG 进程秒退 code=%s —— 不空等，直接放弃" % pg.poll())
            return None
        time.sleep(1.0)
    w("  !! PG 180s 未就绪")
    return None


def stop_pg():
    """pg_ctl stop -m fast。停没停干净**以端口状态为准**，不以 rc 为准。"""
    try:
        subprocess.run(
            [os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PGDATA,
             "stop", "-m", "fast", "-t", "120"],
            capture_output=True, timeout=150)
    except subprocess.TimeoutExpired:
        w("  pg_ctl stop 超时（不代表没停，看端口状态）")
    for _ in range(30):
        if not port_open(PORT_PG):
            return True
        time.sleep(1.0)
    return not port_open(PORT_PG)


def start_proc(argv, log_path, cwd=REPO):
    """起子进程，日志落文件（不能用 PIPE：管道满 -> 写日志阻塞 -> 假死）。"""
    try:
        lf = open(log_path, "wb")
        _LOGF.append(lf)  # 持有引用，防 GC 关掉
    except OSError:
        lf = subprocess.DEVNULL
    return subprocess.Popen(argv, cwd=cwd, stdout=lf, stderr=subprocess.STDOUT)


def build_all():
    t0 = time.time()
    b = subprocess.run([GO_BIN, "build", "-o", EXE, "./cmd/iip-server"],
                       cwd=REPO, capture_output=True, text=True,
                       encoding="utf-8", errors="replace", timeout=600, env=GO_ENV)
    w("  go build iip-server rc=%d (%.1fs) %s"
      % (b.returncode, time.time() - t0, (b.stderr or "")[:300]))
    if b.returncode != 0:
        return False
    t0 = time.time()
    b = subprocess.run([GO_BIN, "build", "-o", EXE_WORKER, "./cmd/iip-worker"],
                       cwd=REPO, capture_output=True, text=True,
                       encoding="utf-8", errors="replace", timeout=600, env=GO_ENV)
    w("  go build iip-worker rc=%d (%.1fs) %s"
      % (b.returncode, time.time() - t0, (b.stderr or "")[:300]))
    return b.returncode == 0


def start_server():
    return start_proc(
        [EXE, "--http-addr=:%d" % PORT_HTTP, "--pg-url=" + DSN, "--log-level=info"],
        os.path.join(_HERE, "_fault_drill_srv.log"))


def start_worker(tag, stale_after=STALE_AFTER, prom_url="", loki_url=""):
    argv = [EXE_WORKER,
            "--pg-url=" + DSN,
            "--worker-poll=" + WORKER_POLL,
            "--worker-task-stale-after=" + stale_after,
            "--evidence-timeout=" + EVIDENCE_TIMEOUT,
            "--log-level=info"]
    if prom_url:
        argv.append("--prom-url=" + prom_url)
    if loki_url:
        argv.append("--loki-url=" + loki_url)
    wrk = start_proc(argv, os.path.join(_HERE, "_fault_drill_worker%s.log" % tag))
    _WORKERS.append(wrk)
    return wrk


def wait_healthz(want=200, timeout=60):
    deadline = time.time() + timeout
    last = "never"
    while time.time() < deadline:
        st, body = req("GET", "/healthz", timeout=5)
        last = "%s/%s" % (st, body if isinstance(body, str) else json.dumps(body))
        if st == want:
            return True, last
        time.sleep(0.5)
    return False, last


def wait_task(task_id, want_statuses, timeout):
    """等任务进入目标状态集合。返回 (是否达到, 最终 status)。"""
    deadline = time.time() + timeout
    st = ""
    while time.time() < deadline:
        st = q("SELECT status FROM tasks WHERE id = %d" % task_id)
        if st in want_statuses:
            return True, st
        time.sleep(0.5)
    return False, st


def read_worker_log(tag):
    p = os.path.join(_HERE, "_fault_drill_worker%s.log" % tag)
    try:
        with open(p, "rb") as f:
            return f.read().decode("utf-8", errors="replace")
    except OSError:
        return ""


def hard_kill(proc):
    """硬杀（TerminateProcess）：不给 drain 机会，模拟崩溃而非重启。"""
    try:
        proc.kill()
    except Exception:
        pass
    try:
        proc.wait(timeout=10)
    except Exception:
        pass


def stop_worker(wrk):
    if wrk is not None and wrk.poll() is None:
        try:
            wrk.terminate()
            wrk.wait(timeout=20)
        except Exception:
            hard_kill(wrk)


# ================================================================
# 场景 D1：worker 崩溃与任务回收
#
# 时序（全部真进程）：
#   假采集器挂起 60s -> worker1 领任务卡在 in_flight -> 硬杀 worker1
#   -> 任务留在 in_flight（持有者已消失）-> 起 worker2（stale=10s）
#   -> ReclaimStale 回收 -> worker2 重领重采 -> done。
# ================================================================

def scenario1(prom, loki):
    section("D1 worker 崩溃与任务回收（真 kill，非 SQL 预置）")

    # 崩溃现场靠假采集器挂住（delay=60s）把任务按在 in_flight：
    # 采集中的 worker 被硬杀 = 真实世界里最典型的「worker 挂在数据源上
    # 被运维 kill -9」。evidence-timeout 注入 30s 只是拉宽观察窗。
    prom.delay = 60.0
    loki.delay = 60.0

    st, body = post_alert("LatencyHigh", "checkout", "d1")
    ok = isinstance(body, dict) and body.get("accepted") == 1
    check("D1 前置：告警投递 200 且 accepted=1", st == 200 and ok,
          "status=%s body=%s" % (st, body if not isinstance(body, dict)
                                 else {k: body.get(k) for k in ("accepted", "rejected")}))

    a1 = q("SELECT id FROM alerts WHERE labels->>'drill' = 'd1' ORDER BY id DESC LIMIT 1")
    if not a1.isdigit():
        skip("D1 主体判据", "告警未落库（alert id=%s），后续判据失去对象" % a1)
        return
    inc = q("SELECT incident_id FROM alerts WHERE id = %s" % a1)
    if not inc.isdigit():
        skip("D1 主体判据", "告警未关联出事件（incident_id=%s）" % inc)
        return
    task_id = qn("SELECT id FROM tasks WHERE payload->>'incident_id' = '%s' "
                 "ORDER BY id ASC LIMIT 1" % inc)
    if task_id < 0:
        skip("D1 主体判据", "事件未入队采集任务（task 查询失败）")
        return
    w("  D1 事件 id=%s 告警 id=%s 任务 id=%d" % (inc, a1, task_id))

    # --- worker1：真领、真卡、真杀（此刻不允许有第二个 worker 在跑，
    #     否则 claimed_by 会指向别人，崩溃现场就张冠李戴了）---
    wrk1 = start_worker("d1a", prom_url=prom.url, loki_url=loki.url)
    w("  worker1(pid=%d) 已起（stale_after=%s, evidence_timeout=%s，仅此一个 worker）"
      % (wrk1.pid, STALE_AFTER, EVIDENCE_TIMEOUT))

    row = ""
    deadline = time.time() + 20
    while time.time() < deadline:
        row = q("SELECT status || '|' || coalesce(claimed_by,'') || '|' || attempts "
                "FROM tasks WHERE id = %d" % task_id)
        stt, cb = row.split("|")[:2]
        if stt == "in_flight":
            break
        if wrk1.poll() is not None:
            break
        time.sleep(0.15)
    stt, cb, att = (row.split("|") + ["", "", ""])[:3]
    check("D1 前置：worker1 领取任务后处于 in_flight（卡在挂起的采集器上）",
          stt == "in_flight", "status=%s attempts=%s" % (stt, att))
    if stt != "in_flight":
        skip("D1 主体判据", "未观测到 in_flight，崩溃现场没造出来")
        stop_worker(wrk1)
        return
    check("D1 前置：claimed_by 正是 worker1 进程（hostname-pid 后缀对得上）",
          cb.endswith("-%d" % wrk1.pid),
          "claimed_by=%s worker1.pid=%d" % (cb, wrk1.pid))

    hard_kill(wrk1)
    check("D1 前置：worker1 已被硬杀（poll 非 None）", wrk1.poll() is not None,
          "rc=%s" % wrk1.poll())
    st_after = q("SELECT status FROM tasks WHERE id = %d" % task_id)
    check("D1 崩溃现场：任务停在 in_flight（持有者已消失，无人完成它）",
          st_after == "in_flight", "status=%s" % st_after)

    # --- worker2：靠 ReclaimStale 接管 ---
    # 先恢复采集能力：否则新 worker 只是重复崩溃前的失败。
    prom.delay = 0.0
    loki.delay = 0.0
    wrk2 = start_worker("d1b", prom_url=prom.url, loki_url=loki.url)
    w("  worker2(pid=%d) 已起（同一库，仅进程换代）" % wrk2.pid)

    # 轮询期间顺便记录 transient claimed_by：完成时该列会被清空，
    # 想看到「新实例领过」只能在 in_flight 窗口内抓。抓不到不影响判据
    # （attempts 才是确定性证据），抓到了就是额外佐证。
    seen_by = set()
    ok_done, final_st = False, ""
    deadline = time.time() + 90
    while time.time() < deadline:
        row = q("SELECT status || '|' || coalesce(claimed_by,'') "
                "FROM tasks WHERE id = %d" % task_id)
        stt, cb = (row.split("|") + ["", ""])[:2]
        if stt in ("done", "dead"):
            ok_done, final_st = True, stt
            break
        if cb:
            seen_by.add(cb)
        if wrk2.poll() is not None:
            break
        time.sleep(0.2)
    check("D1 恢复：任务在 90s 内到达终态", ok_done, "status=%s" % final_st)
    check("D1 恢复：终态是 done 而不是 dead（回收后重采成功）",
          final_st == "done", "status=%s" % final_st)

    attempts = qn("SELECT attempts FROM tasks WHERE id = %d" % task_id)
    # attempts 在领取时 +1、回收时不 +1：worker1 领一次 + worker2 领一次 = 2。
    # 这个数字证明「任务确实被重新领取过」，而不是新 worker 无视僵死行。
    check("D1 恢复：attempts=2（崩溃者领 1 次 + 新 worker 领 1 次）",
          attempts == 2, "attempts=%s" % attempts)

    log2 = read_worker_log("d1b")
    check("D1 恢复：worker2 日志出现回收记录（ReclaimStale 真的触发了）",
          "回收了超时未完成" in log2,
          "log2 含回收行=%s" % ("回收了超时未完成" in log2))
    w("  [info] in_flight 窗口内观测到的 claimed_by：%s"
      % (sorted(seen_by) if seen_by else "（未抓到，判据以 attempts 为准）"))

    n_ev = qn("SELECT count(*) FROM evidence WHERE incident_id = %s" % inc)
    check("D1 恢复：证据已落库（重采成功，不是只把任务标完）",
          n_ev >= 1, "evidence=%s" % n_ev)
    cur = q("SELECT coalesce(to_char(window_end,'YYYY-MM-DD HH24:MI:SS'),'NULL') "
            "FROM tasks WHERE id = %d" % task_id)
    check("D1 恢复：采集游标 window_end 已回填", cur not in ("NULL", "ERR", ""),
          "window_end=%s" % cur)
    n_dead = q("SELECT count(*) FROM tasks WHERE status = 'dead'")
    check("D1 恢复：无死信任务（崩溃一次不该耗尽 3 次尝试）", n_dead == "0",
          "dead=%s" % n_dead)
    stop_worker(wrk2)
    w("  [D1] worker2 已停（优雅 terminate）")


# ================================================================
# 场景 D2：PG 断连与自动重连
# ================================================================

def scenario2(srv, wrk):
    section("D2 PG 断连与自动重连（真停库 %ds）" % PG_DOWNTIME_S)

    # 防御性复位：D1 若走了跳过分支，假采集器可能还挂在 60s 上，
    # 不复位会让本场景的采集全部超时，把环境残留伪装成产品缺陷。
    # （prom/loki 由 main 传入，这里只能通过属性改。）
    for m in _MOCKS:
        m.delay = 0.0

    # --- 基线：停库前链路是通的（否则恢复后的 200 没有对照）---
    # 用独立的 service+alertname：与 D1 的事件（checkout/LatencyHigh）
    # 不同服务不同告警名，评分低于阈值 => 建新事件，基线任务就是
    # 「常驻 worker 上岗后新入队的任务」，不会蹭到 D1 已完成的旧任务。
    st, body = post_alert("ErrorRateHigh", "gateway", "d2-base")
    ok = isinstance(body, dict) and body.get("accepted") == 1
    check("D2 前置：停库前 webhook 200 accepted=1", st == 200 and ok,
          "status=%s" % st)
    inc_a = q("SELECT incident_id FROM alerts WHERE labels->>'drill' = 'd2-base' "
              "ORDER BY id DESC LIMIT 1")
    if not inc_a.isdigit():
        skip("D2 主体判据", "基线告警未关联出事件")
        return None
    # 取**最新**那条任务：合并场景下最旧的可能早已 done（D1 遗留），
    # 最新的才是本次告警触发的入队，才是「常驻 worker 在岗」的证据。
    task_a = qn("SELECT id FROM tasks WHERE payload->>'incident_id' = '%s' "
                "ORDER BY id DESC LIMIT 1" % inc_a)
    if task_a < 0:
        skip("D2 主体判据", "基线任务未入队")
        return None
    ok_a, st_a = wait_task(task_a, {"done", "dead"}, 60)
    check("D2 前置：停库前基线任务已 done（常驻 worker 在岗）",
          ok_a and st_a == "done", "task=%s status=%s" % (task_a, st_a))
    ev_base = qn("SELECT count(*) FROM evidence WHERE incident_id = %s" % inc_a)
    w("  D2 基线事件 id=%s 任务 id=%s 证据=%s" % (inc_a, task_a, ev_base))

    # --- 断连期直插一条未来的任务（注入，非伪装）---
    # 为什么必须 SQL 直插：断连期 webhook 本身就是 503，走不了正常入队。
    # 这条任务验的是「停库前已入队的活在恢复后不丢」。
    # payload 带上基线任务的 window_end 游标：这样它就是一次**忠实的
    # 增量采集轮**（与 EnqueueEvidence 会构造的 payload 同形），
    # 而不是绕过游标重采一遍——后者会造出重复证据，那是注入的副作用，
    # 不是产品行为，别让它污染判据。
    # **必须显式传 db=DB**：psql() 默认连 postgres 库，那里没有 tasks 表。
    rc, cur = psql("SELECT coalesce(to_json(window_end)::text, '') "
                   "FROM tasks WHERE id = %d" % task_a, DB)
    cursor_part = ""
    if rc == 0 and cur and cur != "":
        cursor_part = ", \"window_start\": %s" % cur
    rc, out = psql(
        "INSERT INTO tasks (kind, payload, available_at) VALUES "
        "('collect_evidence', '{\"incident_id\": %s%s}'::jsonb, "
        "NOW() + interval '%d seconds') RETURNING id" % (inc_a, cursor_part, FUTURE_TASK_DELAY_S),
        DB)
    # **取 stdout 第一行**：psql 对 INSERT 会额外打印命令标签
    # （"INSERT 0 1"），整串 isdigit() 会把 "5\nINSERT 0 1" 判成非数字
    # —— 上一轮就是在这里假失败，SQL 本身毫无问题。
    first = out.splitlines()[0].strip() if out else ""
    future_task = first if rc == 0 and first.isdigit() else ""
    check("D2 注入：断连期任务已直插入队（available_at=+%ds）"
          % FUTURE_TASK_DELAY_S, bool(future_task),
          "task_id=%s cursor=%s (注入说明: 断连期无法走 webhook 入队)"
          % (future_task or "失败", cursor_part or "无"))

    # --- 停库 ---
    stopped = stop_pg()
    check("D2 注入：PG 端口已真的关闭（否则后面的 503 是假断连）",
          stopped and not port_open(PORT_PG),
          "port_open=%s" % port_open(PORT_PG))
    if not stopped:
        skip("D2 主体判据", "PG 没停成，断连场景不成立")
        return None
    w("  PG 已停，进入 %ds 断连期" % PG_DOWNTIME_S)

    # --- 断连期观测（只走 HTTP 与进程状态，不读库：库 intentional 不可用）---
    n_5xx = 0
    n_polls = 0
    health_503 = False
    t_end = time.time() + PG_DOWNTIME_S
    while time.time() < t_end:
        st, body = post_alert("DiskFull", "checkout", "d2-down-%d" % n_polls)
        n_polls += 1
        if 500 <= st <= 599:
            n_5xx += 1
        sth, _bh = req("GET", "/healthz", timeout=10)
        if sth == 503:
            health_503 = True
        time.sleep(2.0)

    check("D2 断连期：webhook 持续 5xx（%d 次探测 %d 次 5xx）" % (n_polls, n_5xx),
          n_5xx == n_polls and n_polls > 0, "5xx=%d/%d" % (n_5xx, n_polls))
    check("D2 断连期：healthz 转 503（真查库，不装健康）", health_503)
    check("D2 断连期：server 进程存活（库挂了不拖垮接入层）",
          srv.poll() is None, "srv.rc=%s" % srv.poll())
    check("D2 断连期：worker 进程存活（领不到任务也不退出）",
          wrk.poll() is None, "wrk.rc=%s" % wrk.poll())

    # --- 恢复 PG（不重启 server/worker）---
    pg2 = start_pg()
    check("D2 恢复：PG 重新就绪", pg2 is not None)
    if pg2 is None:
        skip("D2 后续判据", "PG 没起来，无法验恢复")
        return None

    ok_h, last = wait_healthz(200, 90)
    check("D2 恢复：不重启 server，healthz 自动回到 200（pool 自动重连）",
          ok_h, "last=%s" % last)

    st_a2 = q("SELECT status FROM tasks WHERE id = %d" % task_a)
    check("D2 恢复：停库前已完成的任务保持 done（恢复动作不改写既有行）",
          st_a2 == "done", "status=%s" % st_a2)

    st, body = post_alert("CPUHigh", "database", "d2-after")
    ok = isinstance(body, dict) and body.get("accepted") == 1
    check("D2 恢复：webhook 恢复 200 accepted=1", st == 200 and ok,
          "status=%s body=%s" % (st, body if not isinstance(body, dict)
                                 else {k: body.get(k) for k in ("accepted", "rejected")}))
    inc_b = q("SELECT incident_id FROM alerts WHERE labels->>'drill' = 'd2-after' "
              "ORDER BY id DESC LIMIT 1")
    if not inc_b.isdigit():
        skip("D2 补采判据", "恢复后告警未落库/未关联")
        return pg2
    task_b = qn("SELECT id FROM tasks WHERE payload->>'incident_id' = '%s' "
                "ORDER BY id DESC LIMIT 1" % inc_b)
    if task_b < 0:
        skip("D2 补采：新事件任务", "任务查询失败（inc_b=%s）" % inc_b)
        return pg2
    ok_b, st_b = wait_task(task_b, {"done", "dead"}, 90)
    check("D2 补采：恢复后新事件的任务被 worker 处理完",
          ok_b and st_b == "done", "task=%s status=%s" % (task_b, st_b))
    ev_b = qn("SELECT count(*) FROM evidence WHERE incident_id = %s" % inc_b)
    check("D2 补采：新事件证据落库", ev_b >= 1, "evidence=%s" % ev_b)

    if future_task:
        ok_f, st_f = wait_task(int(future_task), {"done", "dead"}, 90)
        check("D2 补采：断连期直插的任务恢复后被处理完（队列不丢）",
              ok_f and st_f == "done", "task=%s status=%s" % (future_task, st_f))
    else:
        skip("D2 补采：断连期直插任务", "插入失败，本条不验")

    n_dead = q("SELECT count(*) FROM tasks WHERE status = 'dead'")
    check("D2 恢复：全程无死信任务", n_dead == "0", "dead=%s" % n_dead)
    ev_a2 = qn("SELECT count(*) FROM evidence WHERE incident_id = %s" % inc_a)
    check("D2 恢复：基线事件证据只增不减（补采是增量，不重采覆写）",
          ev_a2 >= ev_base, "before=%s after=%s" % (ev_base, ev_a2))

    log_w = read_worker_log("d2")
    # worker 在断连期每轮领取都会失败并记一条汇总错误——
    # 这条日志证明它没有静默停摆，而是在反复重试。
    check("D2 恢复：worker 日志有断连期重试记录（不是静默退出）",
          "本轮采集有任务失败" in log_w,
          "含重试记录=%s" % ("本轮采集有任务失败" in log_w))
    return pg2


# ================================================================
# 场景 D3：Redis 缺席
# ================================================================

def scenario3():
    section("D3 Redis 缺席 -> DB polling 兜底")

    # 主判据 1（静态）：worker 的完整依赖闭包里没有任何 Redis 客户端。
    # 这是结构性事实：不存在「某条代码路径悄悄走 Redis」的可能。
    r = subprocess.run([GO_BIN, "list", "-deps", "./cmd/iip-worker"],
                       cwd=REPO, capture_output=True, text=True,
                       encoding="utf-8", errors="replace", timeout=300, env=GO_ENV)
    deps = (r.stdout or "") if r.returncode == 0 else ""
    hits = [ln for ln in deps.splitlines() if re.search(r"redis", ln, re.I)]
    check("D3 主判据：worker 依赖闭包零 Redis 客户端（go list -deps）",
          r.returncode == 0 and not hits,
          "rc=%d 命中=%s" % (r.returncode, hits[:5] if hits else "无"))

    # 主判据 2（静态）：go.mod 没有 redis require。
    try:
        with open(os.path.join(REPO, "go.mod"), "r", encoding="utf-8") as f:
            gomod = f.read()
    except OSError as e:
        gomod = ""
        w("  读 go.mod 失败: %r" % e)
    mod_hits = [ln.strip() for ln in gomod.splitlines()
                if re.search(r"redis", ln, re.I)]
    check("D3 主判据：go.mod 无 redis require", not mod_hits,
          "命中=%s" % (mod_hits[:5] if mod_hits else "无"))

    # 运行时判据：本演练全程没有起过任何 Redis 进程、没有设任何
    # Redis 环境变量，而 D1/D2 的轮询与采集闭环全部成立。
    # 「缺席场景即当前默认形态」由这两节的 PASS 直接支撑。
    w("  运行时佐证：本次演练未部署 Redis 进程、未设置 Redis 环境变量；")
    w("  D1（崩溃回收）与 D2（断连恢复）的 worker 轮询/采集判据即为其证据。")
    w("  外部佐证：verify-func.py 的 M4 采集闭环 89 PASS 同样运行在")
    w("  默认配置（IIP_REDIS_DISABLED 默认 true，DB polling）下。")

    # 信息项（不当判据）：本机是否跑着他人的 Redis——跑了也不影响结论，
    # 因为主判据是静态的；不跑则让「缺席」的观测环境更干净。
    try:
        tl = subprocess.run(["tasklist", "/FI", "IMAGENAME eq redis-server.exe"],
                            capture_output=True, text=True, timeout=30)
        running = "redis-server.exe" in (tl.stdout or "")
    except Exception:
        running = None
    w("  [info] 本机 redis-server 进程：%s（不影响判据，仅说明观测环境）"
      % ("在跑（属其他业务）" if running else
         ("未查询到" if running is None else "未运行")))


# ================================================================
# main
# ================================================================

_MOCKS = []  # 假采集器清单：D2 开场防御性复位用
_T0 = time.time()  # 总耗时计时（汇总里报，便于和预算对账）


def main():
    # ---------- 前置断言：半成品环境不许走到最后才炸 ----------
    assert os.path.isfile(GO_BIN), "go1.26.9 工具链不存在: %s" % GO_BIN
    assert os.path.isfile(os.path.join(PGDATA, "PG_VERSION")), \
        "演练 PGDATA 未初始化: %s" % PGDATA
    assert os.path.isfile(os.path.join(PG_BIN, "postgres.exe")), "缺 postgres.exe"
    assert os.path.isfile(os.path.join(PG_BIN, "pg_ctl.exe")), "缺 pg_ctl.exe"
    w("[pre] 前置断言全通过（go1.26.9 / PGDATA=%s / PG 二进制在位）" % PGDATA)

    if port_open(PORT_PG):
        w("[pre] !! 端口 %d 已被占用——拒绝继续。两个 postmaster 共用 "
          "PGDATA 会 fsync 竞态 PANIC；请先确认没有别的链路在跑。" % PORT_PG)
        return 3
    if port_open(PORT_HTTP):
        w("[pre] !! 端口 %d 已被占用（HTTP）" % PORT_HTTP)
        return 3
    pidf = os.path.join(PGDATA, "postmaster.pid")
    if os.path.isfile(pidf):
        os.remove(pidf)
        w("[pre] 已删残留 postmaster.pid（端口已关，属上一轮崩溃残留）")

    # 清掉上一轮的报告与日志：否则判据读到旧文件会把上轮伪装成本轮。
    for name in ("_fault_drill.txt", "_fault_drill_srv.log",
                 "_fault_drill_workerd1a.log", "_fault_drill_workerd1b.log",
                 "_fault_drill_workerd2.log"):
        try:
            os.remove(os.path.join(_HERE, name))
        except OSError:
            pass

    pg = None
    pg2 = None
    srv = None
    wrk = None
    try:
        # ---------- 起 PG ----------
        section("[0] 起 PG（%d，独立 PGDATA）" % PORT_PG)
        pg = start_pg()
        if pg is None:
            return 3

        psql("DROP DATABASE IF EXISTS %s WITH (FORCE)" % DB)
        psql("CREATE DATABASE %s" % DB)
        w("[1] 已建干净库 %s（server 启动时会自动迁移）" % DB)

        # ---------- 编译 ----------
        section("[2] 编译")
        if not build_all():
            return 2

        # ---------- 假采集器 ----------
        section("[3] 假采集器（进程内 HTTP，故障可注入）")
        prom = _MockCollector("Prometheus", _prom_handler)
        loki = _MockCollector("Loki", _loki_handler)
        _MOCKS.extend([prom, loki])

        # ---------- 起 server ----------
        srv = start_server()
        ok, last = wait_healthz(200, 90)
        w("[4] server 就绪=%s (pid=%d, %s)" % (ok, srv.pid, last))
        if not ok:
            return 3

        # ---------- D1：自带 worker，测完自收（期间不许有别的 worker）---
        scenario1(prom, loki)

        # ---------- 常驻 worker：D2 全程复用 ----------
        wrk = start_worker("d2", prom_url=prom.url, loki_url=loki.url)
        time.sleep(2.0)
        check("D2 前置：常驻 worker 已就绪（PG 在，进程没秒退）",
              wrk.poll() is None, "rc=%s" % wrk.poll())

        # ---------- D2 / D3 ----------
        pg2 = scenario2(srv, wrk)
        scenario3()

        # ---------- 汇总 ----------
        section("汇总")
        n_pass = len([l for l in _L if l.strip().startswith("[PASS]")])
        w("PASS=%d FAIL=%d SKIP=%d 总耗时=%.1fs"
          % (n_pass, len(FAILS), len(SKIPS), time.time() - _T0))
        if FAILS:
            w("失败判据：")
            for f in FAILS:
                w("  - %s" % f)
        if SKIPS:
            w("跳过判据（SKIP 不等于通过）：")
            for s in SKIPS:
                w("  - %s" % s)
        w("结论：%s" % ("全部通过" if not FAILS else "存在失败，见上"))
        return 0 if not FAILS else 1
    finally:
        # ---------- 收场：以端口/进程实际状态为准 ----------
        w("")
        w("=== 收场 ===")
        if srv is not None and srv.poll() is None:
            try:
                srv.terminate()
                srv.wait(timeout=20)
                w("  server 已优雅停止")
            except Exception:
                hard_kill(srv)
                w("  server 已强杀")
        for x in _WORKERS:
            stop_worker(x)
        w("  全部 worker 已收（%d 个）" % len(_WORKERS))
        target = pg2 if pg2 is not None else pg
        if target is not None and target.poll() is None:
            stopped = stop_pg()
            w("  PG 已停=%s（端口 %d 开=%s）"
              % (stopped, PORT_PG, port_open(PORT_PG)))
        for m in _MOCKS:
            m.stop()
        w("[end] 端口 PG=%d HTTP=%d 残留开=%s/%s"
          % (PORT_PG, PORT_HTTP, port_open(PORT_PG), port_open(PORT_HTTP)))


if __name__ == "__main__":
    try:
        rc = main()
    except Exception as e:  # noqa: BLE001 —— 落盘优先，别让异常吞掉报告
        import traceback
        w("[FATAL] %r" % e)
        w(traceback.format_exc())
        rc = 3
    sys.exit(rc)
