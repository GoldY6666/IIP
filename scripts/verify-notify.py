#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""通知链路（T6.1 / T6.2）的真栈可执行验收。

**为什么单测不够**：`internal/notify` 的 15 个用例注入的是内存替身与假 sink，
它们证明的是「限流表算得对」「模板拼得对」；而这条链路上真正会坏的地方在两个
包之间的接缝处 —— hook 有没有被调用、调用时机在事务内还是事务外、装配有没有
把 sink 接错。这些只有真 PG + 真 server + 真 HTTP 出口才看得见（T6.1 的真缺陷
「标题重复」就是探针才发现、单测全绿的）。

判据链（9 条，全部对准**可观测的外部行为**）：
  S1  /healthz 200（栈真的起来了，否则后面全是假象）
  S2  两个出口基线为空（先证明"没配/没触发时不发"，否则 S3 的 1 条说明不了什么）
  S3  一条新告警 → 事件创建 → **两个出口各 1 条**，内容是 `新事件 #<id>`、`关联告警: 1 条`
  S4  open → acknowledged → 各 2 条，文本含 `open → acknowledged（操作人: probe）`
  S5  acknowledged → investigating → 各 3 条（中间态第 2 条，配额刚好用完）
  S6  investigating → mitigated → **仍是 3 条**（第 3 条被去风暴抑制），日志有抑制记录
  S7  mitigated → resolved → **4 条**（终态白名单必发，不受配额限制）
  S8  通用出口带 `Authorization: Bearer <token>`，且 server 日志**不含** token 明文
  S9  优雅退出 rc=0

**端口**：PG 5437 + server 8091 + 假出口 8899（与 S3 的 5434/8082、S5 的 5436/8083、
S6 的 5435 全部错开）。PGDATA 复用 `pgdata_unittest`，故**同一时刻只能有一个 postmaster**：
若 5434 上已有 PG，本脚本直接报错而不是硬起（同一 PGDATA 双开会 fsync PANIC）。
"""

import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GO = r"C:\Users\Yu\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.9.windows-amd64\bin\go.exe"
PG_BIN = r"D:\pgsql16\bin"
PGDATA = r"D:\_iip_setup\pgdata_unittest"
PG_PORT = 5437
DB = "iip_notify_probe"
HTTP_PORT = 8091
SINK_PORT = 8899
TOKEN = "probe-notify-token-do-not-log"
SERVER_EXE = r"D:\_iip_setup\iip-server-notify.exe"
SERVER_LOG = r"D:\_iip_setup\iip-server-notify.log"
# 报告落盘：E2E 的 extract_counts 按 [PASS]/[FAIL] 或 PASS=N 计数，
# 不落盘的话汇总里会显示 "0 PASS / 0 FAIL"，那是把"未知"写成了"零"。
REPORT = os.path.join(REPO, "scripts", "_verify_notify.txt")
BASE = "http://127.0.0.1:%d" % HTTP_PORT

pg_proc = None
logf = None
records = []
rec_lock = threading.Lock()


_report = None


def say(s=""):
    print(s, flush=True)
    if _report is not None:
        _report.write(s + "\n")
        _report.flush()


def fatal(msg):
    say("[FAIL] " + msg)
    sys.exit(1)


# ---------------------------------------------------------------- 基础设施


def env_for_go():
    e = dict(os.environ)
    e["GOCACHE"] = r"D:\_iip_setup\gocache"
    e["GOTOOLCHAIN"] = "local"
    return e


def psql(sql, db="postgres"):
    env = dict(os.environ, PGPASSWORD="dev")
    p = subprocess.run(
        [os.path.join(PG_BIN, "psql.exe"), "-h", "127.0.0.1", "-p", str(PG_PORT),
         "-U", "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", sql],
        capture_output=True, text=True, encoding="utf-8", errors="replace", env=env, timeout=90)
    if p.returncode != 0:
        raise RuntimeError("psql 失败: %s\n%s" % (sql, p.stderr))
    return p.stdout.strip()


def pg_ready(port=PG_PORT):
    p = subprocess.run(
        [os.path.join(PG_BIN, "pg_isready.exe"), "-h", "127.0.0.1", "-p", str(port)],
        capture_output=True, timeout=10)
    return p.returncode == 0


def ensure_pg():
    """起 PG。同一 PGDATA 不能双开 —— 5434 已有 postmaster 时明确报错。"""
    global pg_proc
    if pg_ready():
        say("[0] PG 已在 %d 运行（沿用）" % PG_PORT)
        return
    if pg_ready(5434):
        fatal("5434 上已有 PG 占着同一个 PGDATA；先停掉它（同一 PGDATA 双开会 fsync PANIC）")
    log = open(os.path.join(PGDATA, "pg_notify_probe.log"), "ab")
    pg_proc = subprocess.Popen(
        [os.path.join(PG_BIN, "postgres.exe"), "-D", PGDATA, "-p", str(PG_PORT)],
        stdout=log, stderr=subprocess.STDOUT, cwd=PG_BIN)
    for _ in range(180):
        if pg_ready():
            say("[0] PG 已在 %d 启动（本脚本启动）" % PG_PORT)
            return
        if pg_proc.poll() is not None:
            fatal("PG 进程已退出 code=%s" % pg_proc.poll())
        time.sleep(1.0)
    fatal("PG 启动后 180s 仍不可连")


def stop_pg_if_ours():
    if pg_proc is None:
        return
    subprocess.run([os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PGDATA,
                    "stop", "-m", "fast", "-t", "120"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=150)
    say("[9] 本脚本启动的 PG 已停止")


def build_server():
    p = subprocess.run([GO, "build", "-o", SERVER_EXE, "./cmd/iip-server"], cwd=REPO,
                       capture_output=True, text=True, encoding="utf-8", errors="replace",
                       env=env_for_go())
    if p.returncode != 0:
        fatal("构建 iip-server 失败:\n%s" % p.stderr)


def wait_port(port, timeout=40.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        with socket.socket() as s:
            s.settimeout(0.5)
            if s.connect_ex(("127.0.0.1", port)) == 0:
                return True
        time.sleep(0.2)
    return False


# ---------------------------------------------------------------- 假出口


class FakeSink(BaseHTTPRequestHandler):
    """两个路径共用一个 server：/slack 与 /generic。

    记录**原始头**而不只是 body：`Authorization: Bearer` 有没有真的发出去，
    以及它会不会漏进日志，都是这条链路的判据。
    """

    protocol_version = "HTTP/1.0"

    def do_POST(self):  # noqa: N802 (http.server 约定)
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        with rec_lock:
            records.append({
                "path": self.path,
                "auth": self.headers.get("Authorization") or "",
                "body": raw.decode("utf-8", "replace"),
                "at": time.time(),
            })
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


def start_sink():
    srv = ThreadingHTTPServer(("127.0.0.1", SINK_PORT), FakeSink)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def of(path):
    with rec_lock:
        return [r for r in records if r["path"] == path]


def count(path):
    return len(of(path))


def wait_count(path, n, timeout=8.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if count(path) >= n:
            return True
        time.sleep(0.05)
    return False


# ---------------------------------------------------------------- HTTP


def post(path, payload, raw=None, timeout=10):
    data = raw if raw is not None else json.dumps(payload).encode()
    req = urllib.request.Request(BASE + path, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def get(path, timeout=10):
    try:
        with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def transition(incident_id, to, actor="probe"):
    return post("/api/v1/incidents/%d/transition" % incident_id,
                {"to": to, "actor": actor})


# ---------------------------------------------------------------- 主体

ALERT_NAME = "NotifyProbeHighLatency"


def fire_alert(instance="notify-probe:1"):
    """一条最小合法告警（新指纹 ⇒ 会走 NEW 档建档 ⇒ 触发创建通知）。"""
    return post("/api/v1/alerts/webhook", {
        "version": "4", "status": "firing",
        "alerts": [{
            "status": "firing",
            "labels": {"alertname": ALERT_NAME, "severity": "critical",
                       "instance": instance, "job": "notify-probe"},
            "annotations": {"summary": "通知链路探针"},
            "startsAt": "2026-10-18T00:00:00Z",
        }],
    })


def main():
    global logf, _report
    _report = open(REPORT, "w", encoding="utf-8")
    say("=== 通知链路真栈验收（T6.1 / T6.2）===")
    ensure_pg()
    psql("DROP DATABASE IF EXISTS %s" % DB)
    psql("CREATE DATABASE %s" % DB)
    say("[1] 探针库已建：%s" % DB)

    build_server()
    say("[2] iip-server 已构建")

    sink = start_sink()
    say("[3] 假出口已监听 :%d（/slack + /generic）" % SINK_PORT)

    env = dict(os.environ)
    env.update({
        "IIP_PG_URL": "postgres://postgres:dev@127.0.0.1:%d/%s?sslmode=disable" % (PG_PORT, DB),
        "IIP_LOG_LEVEL": "info",
        "IIP_TOPOLOGY_FILE": "config/topology.yaml",
        "IIP_HTTP_ADDR": ":%d" % HTTP_PORT,
        # 两个出口都开：判据要证明"两个 sink 都被接上了"，只配一个会漏掉装配错误。
        "IIP_SLACK_WEBHOOK_URL": "http://127.0.0.1:%d/slack" % SINK_PORT,
        "IIP_NOTIFY_WEBHOOK_URL": "http://127.0.0.1:%d/generic" % SINK_PORT,
        "IIP_NOTIFY_WEBHOOK_TOKEN": TOKEN,
        "IIP_UI_BASE_URL": "http://ui.example",
    })
    logf = open(SERVER_LOG, "w", encoding="utf-8")
    srv = subprocess.Popen([SERVER_EXE], cwd=REPO, env=env, stdout=logf,
                           stderr=subprocess.STDOUT,
                           creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
    try:
        if not wait_port(HTTP_PORT):
            fatal("server 未在 %d 监听" % HTTP_PORT)
        say("[4] server 已监听 :%d" % HTTP_PORT)

        st, _ = get("/healthz")
        if st != 200:
            fatal("S1 /healthz 应为 200，实际 %d" % st)
        say("[S1] PASS /healthz 200")

        # S2 基线：还没发告警，两个出口都不该有任何消息。
        # 少了这条，「S3 收到 1 条」可能只是上一轮的残留（数据库是新建的，但出口是内存态）。
        time.sleep(0.3)
        if count("/slack") or count("/generic"):
            fatal("S2 基线不为空：slack=%d generic=%d"
                  % (count("/slack"), count("/generic")))
        say("[S2] PASS 两个出口基线为空")

        # S3 一条告警 → 事件创建 → 两个出口各 1 条
        st, body = fire_alert()
        if st != 200:
            fatal("S3 告警投递应 200，实际 %d（%s）" % (st, body[:200]))
        if not wait_count("/slack", 1):
            fatal("S3 Slack 出口 8s 内没收到消息（hook 没被调用或 sink 没接上）")
        if not wait_count("/generic", 1):
            fatal("S3 通用出口 8s 内没收到消息")
        st, body = get("/api/v1/incidents")
        incs = json.loads(body).get("incidents") or []
        if not incs:
            fatal("S3 告警投递成功但没有建出事件，后续判据无从谈起")
        inc_id = incs[0]["id"]
        slack = of("/slack")[0]["body"]
        gen = json.loads(of("/generic")[0]["body"])
        if "新事件 #%d" % inc_id not in slack:
            fatal("S3 Slack 文本应含「新事件 #%d」，实际：%s" % (inc_id, slack))
        if "关联告警: 1 条" not in slack:
            fatal("S3 Slack 文本应含「关联告警: 1 条」，实际：%s" % slack)
        if gen.get("kind") != "incident_created":
            fatal("S3 通用出口 kind 应为 incident_created，实际 %r" % gen.get("kind"))
        if (gen.get("incident") or {}).get("id") != inc_id:
            fatal("S3 通用出口的 incident.id 与库里不一致：%r vs %d"
                  % ((gen.get("incident") or {}).get("id"), inc_id))
        say("[S3] PASS 事件创建 → 两出口各 1 条，内容含 #%d 与告警数" % inc_id)

        # S4 中间态第 1 条
        st, body = transition(inc_id, "acknowledged")
        if st != 200:
            fatal("S4 流转到 acknowledged 应 200，实际 %d（%s）" % (st, body[:200]))
        if not wait_count("/slack", 2) or not wait_count("/generic", 2):
            fatal("S4 中间态第 1 条没到达（slack=%d generic=%d）"
                  % (count("/slack"), count("/generic")))
        slack = of("/slack")[1]["body"]
        if "open → acknowledged（操作人: probe）" not in slack:
            fatal("S4 文本应含「open → acknowledged（操作人: probe）」，实际：%s" % slack)
        say("[S4] PASS acknowledged 送达，文本含 from→to 与操作人")

        # S5 中间态第 2 条（配额刚好用完）
        st, _ = transition(inc_id, "investigating")
        if st != 200:
            fatal("S5 流转到 investigating 应 200，实际 %d" % st)
        if not wait_count("/slack", 3) or not wait_count("/generic", 3):
            fatal("S5 中间态第 2 条没到达（slack=%d generic=%d）"
                  % (count("/slack"), count("/generic")))
        say("[S5] PASS investigating 送达（窗口内第 2 条，配额已满）")

        # S6 中间态第 3 条 → 必须被抑制。这条是整条判据链的核心：
        # 少了它，「限流没接上」也会让上面三条全绿。
        st, _ = transition(inc_id, "mitigated")
        if st != 200:
            fatal("S6 流转到 mitigated 应 200，实际 %d" % st)
        time.sleep(1.2)  # 给异步队列足够时间；抑制是同步判定，1.2s 远超必要
        if count("/slack") != 3 or count("/generic") != 3:
            fatal("S6 第 3 条中间态本应被去风暴抑制，实际 slack=%d generic=%d"
                  % (count("/slack"), count("/generic")))
        logf.flush()
        text = open(SERVER_LOG, encoding="utf-8", errors="replace").read()
        if "状态变更通知被去风暴抑制" not in text:
            fatal("S6 出口计数对但日志没有抑制记录 —— 可能是别的原因少发了")
        if '"to":"mitigated"' not in text and "mitigated" not in text:
            fatal("S6 日志里找不到被抑制的那条是 mitigated")
        say("[S6] PASS mitigated 被抑制（仍是 3 条，日志有抑制记录）")

        # S7 终态 → 必发，不受配额限制
        st, _ = transition(inc_id, "resolved")
        if st != 200:
            fatal("S7 流转到 resolved 应 200，实际 %d" % st)
        if not wait_count("/slack", 4) or not wait_count("/generic", 4):
            fatal("S7 终态必发未生效（slack=%d generic=%d，期望 4）"
                  % (count("/slack"), count("/generic")))
        slack = of("/slack")[3]["body"]
        if "mitigated → resolved" not in slack:
            fatal("S7 第 4 条应是 resolved，实际：%s" % slack)
        say("[S7] PASS resolved 必发（配额满仍送达，累计 4 条）")

        # S8 token 只走请求头，不落日志
        auth = of("/generic")[0]["auth"]
        if auth != "Bearer " + TOKEN:
            fatal("S8 通用出口的 Authorization 应为 Bearer <token>，实际 %r" % auth)
        logf.flush()
        text = open(SERVER_LOG, encoding="utf-8", errors="replace").read()
        if TOKEN in text:
            fatal("S8 server 日志里出现了 token 明文")
        say("[S8] PASS 出口带 Bearer token，且日志无明文")

        # S9 优雅退出
        srv.send_signal(signal.CTRL_BREAK_EVENT)
        try:
            rc = srv.wait(timeout=25)
        except subprocess.TimeoutExpired:
            srv.kill()
            rc = -9
        logf.close()
        if rc != 0:
            fatal("S9 退出码应为 0，实际 %d" % rc)
        say("[S9] PASS 优雅退出 rc=0")

        say("=== 9/9 PASS ===")
        say("PASS=9 FAIL=0 SKIP=0")
        return 0
    finally:
        if srv.poll() is None:
            srv.kill()
        try:
            sink.shutdown()
        except Exception:  # noqa: BLE001 - 收尾失败不该盖住真实结论
            pass
        stop_pg_if_ours()


if __name__ == "__main__":
    sys.exit(main())
