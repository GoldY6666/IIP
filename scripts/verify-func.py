#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""IIP 功能测试（跨功能端到端）—— 真实 PG + 真实二进制 + 真实 HTTP。

与既有脚本的分工（别混用）：
  verify-runtime.py  链路 B 宿主机全栈，偏 M1/M2 的单点校验
  verify-images.py   链路 C 容器链路，偏部署形态与镜像内容物
  smoke_m3_s2.py     S2 六个端点的状态码与错误码
  本脚本            **业务闭环**：M1 接入 -> M2 关联 -> M3 处置 -> 抑制生效
                     走完一整条链路并核对**数据后果**，而不只是「返回 200」

本脚本重点覆盖三条**从未在真实进程里验证过**的路径：
  F1 抑制闭环：reject 一条 REVIEW 边 -> 同指纹新告警应降级为 NEW 档另起事件
                （只验了「reject 返回 200」，没验「抑制真的生效」）
  F2 跨事件指针：reject 后若该告警已无其他边，alerts.incident_id 应清空
  F3 resolved_at 语义：进入 ignored **不写** resolved_at（误报不该计入 MTTR）

复用 smoke_m3_s2.py 已验证过的部分（不在此重复），聚焦上面三条 + 一条完整闭环。
"""

import json
import os
import subprocess
import tempfile
import sys
import time
import urllib.error
import urllib.request

# 仓库根 = 本脚本的上级目录（脚本在 scripts/ 下），不硬编码绝对路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(_HERE)

# PostgreSQL 绿色版位置：可用环境变量 IIP_PG_HOME 覆盖，默认本机约定路径。
PG = os.environ.get("IIP_PG_HOME", r"D:\pgsql16")
PG_BIN = os.path.join(PG, "bin")
PG_DATA = os.path.join(PG, "data")
# 端口与 smoke_m3_s2.py（5433/8081）错开，避免两条验证链路互相抢占。
PORT_PG = int(os.environ.get("IIP_FUNC_PG_PORT", "5434"))
PORT_HTTP = int(os.environ.get("IIP_FUNC_HTTP_PORT", "8082"))
EXE = os.path.join(tempfile.gettempdir(), "_func_iip-server.exe")
DB = "iip_func"
DSN = "postgres://postgres:dev@127.0.0.1:%d/%s?sslmode=disable" % (PORT_PG, DB)
BASE = "http://127.0.0.1:%d" % PORT_HTTP
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_verify_func.txt")
TOKEN = str(int(time.time()))

# 固定的告警时间窗（2026-10-03 当天，两条都相同）——
# 必须固定，理由见 post_alert 里的注释。
FIXED_START = "2026-10-03T00:00:00Z"
FIXED_END = "2026-10-04T00:00:00Z"

_L = []
FAILS = []


def w(s):
    _L.append(str(s))
    print(s, flush=True)


def check(name, ok, detail=""):
    tag = "PASS" if ok else "FAIL"
    line = "  [%s] %s" % (tag, name)
    if detail:
        line += "  " + detail
    w(line)
    if not ok:
        FAILS.append(name)
    return ok


def section(t):
    w("")
    w("=== %s ===" % t)


# --- 进程与端口 ---

def port_open(p):
    import socket
    s = socket.socket()
    s.settimeout(0.5)
    try:
        s.connect(("127.0.0.1", p))
        return True
    except Exception:
        return False
    finally:
        s.close()


def psql(sql, db="postgres"):
    env = {**os.environ, "PGPASSWORD": "dev"}
    p = subprocess.run(
        [os.path.join(PG_BIN, "psql.exe"), "-h", "127.0.0.1", "-p", str(PORT_PG),
         "-U", "postgres", "-d", db, "-t", "-A", "-c", sql],
        capture_output=True, text=True, encoding="utf-8", errors="replace",
        env=env, timeout=120)
    return p.returncode, (p.stdout or "").strip()


def q(sql, db=DB):
    """取单值查询结果。"""
    rc, out = psql(sql, db)
    return out if rc == 0 else "ERR(%d)" % rc


def req(method, path, body=None):
    """发 HTTP 请求，返回 (status, parsed_json_or_text)。"""
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    r = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=30) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        status = e.code
    try:
        return status, json.loads(raw)
    except Exception:
        return status, raw


def post_alert(alertname, service="checkout", extra_labels=None, status="firing", end_at=None):
    """投一条告警。labels 里的 run=TOKEN 保证每轮指纹唯一。

    **去重规则**（V1.0 §4.2）：同一指纹的「仍在 firing」行已在库即 skip，
    不产生新行、也**不触发关联**。所以要造「同指纹的第二条告警」，
    必须先把前一条投递为 resolved，否则第二条根本不会落库。
    """
    labels = {"alertname": alertname, "service": service,
              "severity": "warning", "run": TOKEN}
    if extra_labels:
        labels.update(extra_labels)
    payload = {
        "version": "4",
        "groupKey": "func-%s" % TOKEN,
        "status": status,
        "receiver": "iip",
        "groupLabels": {},
        "commonLabels": {},
        "commonAnnotations": {},
        "externalURL": "",
        "alerts": [{
            "status": status,
            "labels": labels,
            "annotations": {"summary": "功能测试 %s" % alertname},
            # **时间基准必须固定**，否则关联结果不可复现：
            # firing 告警的 endsAt 落库为 NULL，评分时 endsAtOr() 用「当前时刻」
            # 兜底，于是窗口 = [startsAt, now]。两条告警投递间隔越久，
            # overlap/union 越小 -> time 维（权重 0.25）越低 -> 可能直接掉到
            # 0.5 阈值以下判成 NEW。实测同一条脚本多次跑出 0.72 / 0.52 / 不建边。
            # 单测用固定时刻所以稳定，真实进程用 wall clock 就会抖。
            # 这里给固定的 startsAt 与明确的未来 endsAt，让重叠比例恒定。
            "startsAt": FIXED_START,
            "endsAt": end_at or FIXED_END,
            "generatorURL": "",
        }],
    }
    return req("POST", "/api/v1/alerts/webhook", payload)


def alert_id_of(run_label=None, alertname=None, inst=None, newest=False):
    """按 labels 取 alerts.id。

    必须支持 newest：同一 alertname 在一轮测试里可能投多次（如抑制验证），
    只按 alertname 查会拿到**最早**那条，断言就会指向错误的对象。
    标量查询没有 ORDER BY 时返回顺序不保证，所以用显式排序。
    """
    sql = ("SELECT id FROM alerts WHERE labels->>'run' = '%s'"
           % (run_label or TOKEN))
    if alertname:
        sql += " AND labels->>'alertname' = '%s'" % alertname
    if inst:
        sql += " AND labels->>'instance' = '%s'" % inst
    sql += " ORDER BY id %s LIMIT 1" % ("DESC" if newest else "ASC")
    return q(sql)


def rev_alert_name(alert_id):
    """取某条告警的 alertname（用来照着它造同指纹的新告警）。"""
    return q("SELECT labels->>'alertname' FROM alerts WHERE id = %s" % alert_id)


# ================================================================
# M4：证据采集闭环
#
# 验的是**从未在真实进程里跑过**的那条链：
#   告警落库 -> 关联建事件 -> 入队 -> worker 领取 -> 采集器发真实 HTTP
#   -> 证据落库 -> 写 timeline -> 任务置 done -> API 读回
#
# 采集器用**进程内假 HTTP 服务**（不是 Go 单测里的 httptest）：
# 端到端验的就是「worker 拿着配置里的 URL 真的发出去请求」，
# 用 httptest 就等于把要验的东西替换掉了。
# ================================================================

EXE_WORKER = os.path.join(tempfile.gettempdir(), "_func_iip-worker.exe")
PORT_PROM = int(os.environ.get("IIP_FUNC_PROM_PORT", "9099"))
PORT_LOKI = int(os.environ.get("IIP_FUNC_LOKI_PORT", "3100"))
# worker 轮询间隔压到 1s：门禁要的是「2 分钟内 ≥5 条证据」，
# 用默认 30s 会让这一段白等 30 秒才跑第一轮。
WORKER_POLL = "1s"


class _MockDown(Exception):
    """假采集器被置为宕机态时由 handler 抛出，触发 503 响应。"""


class _MockCollector(object):
    """可控的假采集器。

    **用 503 而不是真的关掉端口**：真实事故里 Prometheus 挂掉通常是
    进程还在、查询返回 5xx 或超时；直接关端口会让 worker 拿到
    connection refused，走的是另一条错误分支。保持端口活着才能
    验到「数据源故障」这条真实路径。
    """

    def __init__(self, name, handler):
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        self.name = name
        self.handler = handler
        self.down = False
        self.hits = 0

        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler 规定名
                outer.hits += 1
                if outer.down:
                    raw = b'{"status":"error","error":"mock down"}'
                    self.send_response(503)
                else:
                    try:
                        raw = json.dumps(outer.handler(self.path)).encode("utf-8")
                    except Exception as e:  # handler 自身出错也要有响应
                        raw = json.dumps({"status": "error",
                                          "error": str(e)}).encode("utf-8")
                        self.send_response(500)
                        self.send_header("Content-Type", "application/json")
                        self.send_header("Content-Length", str(len(raw)))
                        self.end_headers()
                        self.wfile.write(raw)
                        return
                    self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *_a):
                pass  # 静默：采集器每轮打多次请求，不静音会淹没报告

        self._srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
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


def _prom_handler(path):
    """Prometheus /api/v1/query 的响应。

    值随「查询时刻」变化，好让 worker 的**基线对比**有东西可比。
    判据：查询时刻早于事故起点算基线窗口（返回 1），否则算事故窗口（返回 10）。

    用 calendar.timegm 而不是 time.mktime：FIXED_START 带 Z（UTC），
    而 mktime 把 struct_time 当**本地时间**解释 —— 本机在 UTC+8 时
    锚点会偏 8 小时，基线窗口也被判成事故窗口，对比就恒为 1.0 倍，
    断言「有变化幅度」会假失败。
    """
    import calendar
    import urllib.parse as up
    qs = up.parse_qs(up.urlparse(path).query)
    try:
        ts = int((qs.get("time") or ["0"])[0])
    except ValueError:
        ts = 0
    anchor = calendar.timegm(time.strptime(FIXED_START, "%Y-%m-%dT%H:%M:%SZ"))
    val = "1" if ts < anchor else "10"
    return {"status": "success",
            "data": {"resultType": "vector",
                     "result": [{"metric": {}, "value": [ts, val]}]}}


def _loki_handler(path):
    """Loki /loki/api/v1/query_range 的响应。

    返回两条同类错误（行内数字不同）—— 验证**归一化聚合**：
    两条若不被归一化成同一模式，Top1 就会是「各 1 条」而突增倍数也不对。
    """
    import urllib.parse as up
    qs = up.parse_qs(up.urlparse(path).query)
    try:
        start_ns = int((qs.get("start") or ["0"])[0])
    except ValueError:
        start_ns = 0
    # 纳秒 -> 秒，落在查询区间内（§4.4：证据时间必须来自日志自身）。
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


def _time_webhook(tag, service):
    """投一条告警并返回 webhook 往返耗时（毫秒）。

    `tag` 必须**每轮唯一**：指纹 = 除 instance/pod/container/node 外的全部
    label，alertname + service 相同就是同一指纹，而「仍在 firing」的行
    已在库时会**被去重 skip** —— 那样测到的是 skip 分支的耗时，
    不是完整的「落库 + 关联 + 入队」路径。判据 #3 会被这种假通过架空。

    采集完全不在这条路径上（采集在 worker 进程里），
    所以这个数字就是「告警接入链路自身的延迟」，数据源宕机时它不该变。
    """
    t0 = time.time()
    st, body = post_alert(tag, service)
    ms = int((time.time() - t0) * 1000)
    # 必须确认真的走完了全链路：accepted=0 说明被去重了，这个样本作废。
    ok = isinstance(body, dict) and body.get("accepted") == 1
    w("  [%s] 投递 %s 耗时 %d ms（accepted=%s）"
      % ("ok" if ok else "!!", tag, ms,
         body.get("accepted") if isinstance(body, dict) else body))
    return ms, ok


def _q1(sql):
    """取单值查询结果（q 的别名，语义上强调「拿来打日志/断言的小值」）。"""
    return q(sql)


def _wait_tasks_settled(timeout_s):
    """等所有任务离开 pending/in_flight。返回是否在超时前settle。"""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        if q("SELECT count(*) FROM tasks "
             "WHERE status IN ('pending','in_flight')") == "0":
            return True
        time.sleep(1.0)
    return False


def _wait_attempts(task_id, want, timeout_s):
    """等某任务的 attempts 涨到 want 以上。返回是否达到。"""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        v = _q1("SELECT attempts FROM tasks WHERE id = %s" % task_id)
        if v.isdigit() and int(v) >= want:
            return True
        time.sleep(0.5)
    return False


def _wait_task_done(task_id, timeout_s):
    """等某任务变 done（或 dead）。返回 (是否 done, 最终 status)。"""
    deadline = time.time() + timeout_s
    st = ""
    while time.time() < deadline:
        st = _q1("SELECT status FROM tasks WHERE id = %s" % task_id)
        if st in ("done", "dead"):
            return st == "done", st
        time.sleep(1.0)
    return False, st


def m4_section(inc):
    """M4 端到端。返回 0 继续，非 0 表示脚本应中止。"""
    section("F5b M4 证据采集闭环（真实 worker + 真实 HTTP 采集）")

    prom_stop = loki_stop = None
    wrk = None
    wrk_log = None
    wrk_log_path = os.path.splitext(OUT)[0] + "_worker.log"
    prom = loki = None
    try:
        prom = _MockCollector("Prometheus", _prom_handler)
        loki = _MockCollector("Loki", _loki_handler)
        prom_url, loki_url = prom.url, loki.url

        # --- 编译 worker ---
        t0 = time.time()
        b = subprocess.run(["go", "build", "-o", EXE_WORKER, "./cmd/iip-worker"],
                           cwd=REPO, capture_output=True, text=True,
                           encoding="utf-8", errors="replace", timeout=600)
        w("  go build iip-worker rc=%d (%.1fs) %s"
          % (b.returncode, time.time() - t0, (b.stderr or "")[:300]))
        if b.returncode != 0:
            return 20

        # --- 起 worker ---
        try:
            wrk_log = open(wrk_log_path, "wb")
            wrk_out = wrk_log
        except OSError:
            wrk_log = None
            wrk_out = subprocess.DEVNULL
        wrk = subprocess.Popen(
            [EXE_WORKER,
             "--pg-url=" + DSN,
             "--worker-poll=" + WORKER_POLL,
             "--prom-url=" + prom_url,
             "--loki-url=" + loki_url,
             "--log-level=info"],
            cwd=REPO, stdout=wrk_out, stderr=subprocess.STDOUT)

        # --- 等任务被处理完 ---
        # 不按固定 sleep 判成功：轮询「证据条数」直到达标或超时，
        # 固定 sleep 在慢机器上会假失败、快机器上白等。
        deadline = time.time() + 90
        n_ev = "0"
        while time.time() < deadline:
            n_ev = q("SELECT count(*) FROM evidence WHERE incident_id = %s" % inc)
            if n_ev.isdigit() and int(n_ev) >= 5:
                break
            if wrk.poll() is not None:
                w("  worker 提前退出 rc=%s" % wrk.poll())
                break
            time.sleep(1.0)
        w("  证据条数=%s（阈值 ≥5）" % n_ev)
        check("M4 验收标准：2 分钟内该事件证据 ≥5 条",
              n_ev.isdigit() and int(n_ev) >= 5, "count=%s" % n_ev)

        # --- 任务终态 ---
        st_tasks = q("SELECT status FROM tasks WHERE kind='collect_evidence' "
                     "ORDER BY id DESC LIMIT 1")
        check("采集任务已置 done（不是死信）", st_tasks == "done",
              "status=%s" % st_tasks)
        n_dead = q("SELECT count(*) FROM tasks WHERE status='dead'")
        check("无死信任务（采集器可达就不该重试到死）", n_dead == "0",
              "dead=%s" % n_dead)

        # --- 游标已回填 ---
        cur = q("SELECT coalesce(to_char(window_end, 'YYYY-MM-DD HH24:MI:SS'), 'NULL') "
                "FROM tasks WHERE kind='collect_evidence' ORDER BY id DESC LIMIT 1")
        check("游标 window_end 已回填（增量采集的前提）", cur not in ("NULL", "ERR", ""),
              "window_end=%s" % cur)

        # --- API 读回 ---
        st, ev = req("GET", "/api/v1/incidents/%d/evidence" % inc)
        check("证据 API 200", st == 200, "status=%s" % st)
        if isinstance(ev, dict):
            items = ev.get("evidence", [])
            check("API 返回的条数与库内一致（不截断到 50 以下）",
                  len(items) == int(n_ev or -1),
                  "api=%d db=%s" % (len(items), n_ev))
            # 键名契约：解成 dict 比，与结构体定义解耦，
            # 否则漏写 json tag 时读写都自洽、测试永远绿。
            need = {"id", "type", "source", "query", "value", "metadata", "timestamp"}
            check("每条证据含契约要求的全部键名",
                  all(need <= set(i.keys()) for i in items) if items else False,
                  "keys=%s" % (sorted(items[0].keys()) if items else []))
            check("by_type 覆盖全部 6 种类型（未采集的类型也应为 0）",
                  isinstance(ev.get("by_type"), dict)
                  and len(ev["by_type"]) == 6,
                  "by_type=%s" % (ev.get("by_type") if isinstance(ev, dict) else None))
            check("truncated=false（未超 50 条预算）",
                  ev.get("truncated") is False, "truncated=%s" % ev.get("truncated"))
            check("total 与 count 一致",
                  ev.get("total") == len(items),
                  "total=%s count=%d" % (ev.get("total"), len(items)))
            # 采集器来源必须都能区分，否则页面无法按来源过滤
            srcs = sorted(set(i.get("source") for i in items))
            check("证据来源含 prometheus 与 loki 两个采集器",
                  "prometheus" in srcs and "loki" in srcs, "sources=%s" % srcs)
            # 事故窗口 vs 基线窗口的对比幅度（RED 采集器的核心产出）
            metrics = [i for i in items if i.get("type") == "metric"]
            check("含 metric 类证据（RED 指标）", len(metrics) >= 1,
                  "metric=%d" % len(metrics))
            if metrics:
                md = metrics[0].get("metadata") or {}
                # 键名以实现为准（value_before / value_after），
                # 断言的是「有前后两个值 + 有变化幅度」这个语义，
                # 而不是某个具体键名 —— 改键名时这条不该跟着红。
                check("metric 证据带前后两个值（窗口对比而非单点取值）",
                      "value_before" in md and "value_after" in md,
                      "metadata keys=%s" % sorted(md.keys()))
                check("metric 证据算出变化幅度（RED 采集器的核心产出）",
                      "change_ratio" in md, "metadata keys=%s" % sorted(md.keys()))
            logs = [i for i in items if i.get("type") == "log"]
            check("含 log 类证据（Loki 错误模式）", len(logs) >= 1,
                  "log=%d" % len(logs))
            # 不按顺序取 logs[0]：log 证据有「模式 TopN」与「错误量突增」两类，
            # 排序（created_at DESC, id DESC）决定哪条在前面 ——
            # 写死 logs[0] 就是在断言顺序而非语义。
            # 改为「存在一条带 pattern 计数的证据，且计数 ≥2」。
            pat = [i for i in logs if "count" in (i.get("metadata") or {})]
            check("log 证据含按模式聚合的条目", len(pat) >= 1,
                  "带 count 的=%d / 共 %d" % (len(pat), len(logs)))
            if pat:
                cnt = pat[0]["metadata"]["count"]
                check("同类日志被归一化聚合（count ≥2 而非各为 1）",
                      cnt.isdigit() and int(cnt) >= 2,
                      "pattern=%s count=%s"
                      % (pat[0]["metadata"].get("pattern"), cnt))
            burst = [i for i in logs if "count_after" in (i.get("metadata") or {})]
            check("log 证据含独立的「错误量突增」条目（与 TopN 是两个问题）",
                  len(burst) == 1,
                  "burst 条数=%d" % len(burst))
        else:
            check("证据 API 返回 JSON 对象", False, "body=%r" % (ev,))

        # --- 类型过滤 ---
        st, ev2 = req("GET", "/api/v1/incidents/%d/evidence?type=metric" % inc)
        only_metric = all(i.get("type") == "metric"
                          for i in (ev2.get("evidence", []) if isinstance(ev2, dict) else []))
        check("type=metric 过滤只返回 metric", st == 200 and only_metric,
              "n=%d" % len(ev2.get("evidence", []) if isinstance(ev2, dict) else []))

        st, ev3 = req("GET", "/api/v1/incidents/%d/evidence?type=bogus" % inc)
        check("非法 type -> 400 且错误信息列出合法值", st == 400,
              "status=%s body=%r" % (st, ev3))

        # --- timeline 有 evidence_found ---
        st, det = req("GET", "/api/v1/incidents/%d" % inc)
        ev_types = [e.get("event_type") for e in (det.get("timeline", [])
                                                  if isinstance(det, dict) else [])]
        check("时间线记录了 evidence_found", "evidence_found" in ev_types,
              "timeline=%s" % ev_types)

        # ============ 判据 #3：数据源宕机时 webhook 延迟无变化 ============
        section("F5c 数据源宕机：webhook 零阻塞 + 任务重试 + 恢复后自动补采")
        # **先取基线，再制造故障**。且用一个**尚不存在的服务**，
        # 保证这条告警一定走完整链路（新建事件 + 入队），
        # 而不是被去重 skip 掉。
        base_ms, base_ok = _time_webhook("M4BaselineProbe", "m4-probe-svc")
        check("基线投递走了完整链路（未被去重）", base_ok, "accepted 见上一行")
        w("  数据源正常时 webhook 延迟 = %d ms" % base_ms)

        # 等基线任务的首次采集完成（数据源正常 → 应成功），
        # 这样后面「数据源宕机」造出的新任务才是干净的首次尝试。
        _wait_tasks_settled(60)

        # 两个数据源同时宕机。
        prom.down = True
        loki.down = True
        w("  已把 Prometheus 与 Loki 置为宕机（HTTP 503）")

        # 宕机期间投递：**另一个**新服务，确保是新事件新任务。
        down_ms, down_ok = _time_webhook("M4DownProbe", "m4-down-svc")
        check("宕机期间投递走了完整链路（未被去重）", down_ok, "accepted 见上一行")
        w("  数据源宕机时 webhook 延迟 = %d ms" % down_ms)
        # 判据是「没有量级差异」而不是「毫秒级相等」：本机负载会抖动，
        # 5s 级的超时阻塞一定看得出来，几十毫秒的波动看不出来。
        # 阈值 max(3x, +500ms)：能抓住阻塞，又不会被调度噪声误报。
        budget = max(base_ms * 3, base_ms + 500)
        check("判据#3 数据源宕机时 webhook 延迟无量级变化（零阻塞）",
              down_ok and down_ms <= budget,
              "before=%dms after=%dms 预算=%dms" % (base_ms, down_ms, budget))

        # 事件仍应创建（采集失败不得影响关联）。
        time.sleep(0.5)
        n_down_inc = q("SELECT count(*) FROM incidents WHERE service='m4-down-svc'")
        check("宕机期间事件照常创建（采集不阻断关联）", n_down_inc == "1",
              "m4-down-svc 事件数=%s" % n_down_inc)

        # 任务必须**真的被重试过**：attempts 递增 + 状态退回 pending。
        # 断言「attempts >= 1」太弱 —— 入队那一刻 attempts 就是 0，
        # 被领取一次就变 1，失败退回 pending 后还是 1。
        # 「被重试」的标志是 attempts 涨到 **2 以上**（第二次被领取）。
        down_task = q("SELECT id FROM tasks WHERE kind='collect_evidence' "
                      "ORDER BY id DESC LIMIT 1")
        retried = _wait_attempts(down_task, 2, 60)
        check("判据#4a 宕机期间任务被反复尝试（attempts 涨到 ≥2，即进入重试）",
              retried,
              "task=%s attempts=%s status=%s"
              % (down_task, _q1("SELECT attempts FROM tasks WHERE id=%s" % down_task),
                 _q1("SELECT status FROM tasks WHERE id=%s" % down_task)))
        check("宕机期间任务未进死信（还有重试机会）",
              _q1("SELECT status FROM tasks WHERE id=%s" % down_task) != "dead",
              "status=%s" % _q1("SELECT status FROM tasks WHERE id=%s" % down_task))
        # 有退避可见性：available_at 被推到未来
        av = _q1("SELECT (available_at > NOW())::text FROM tasks WHERE id=%s" % down_task)
        w("  宕机任务的 available_at 在未来 = %s（指数退避可见）" % av)

        # ============ 判据 #4：恢复后自动补采 ============
        prom.down = False
        loki.down = False
        w("  已恢复两个数据源，等 worker 自动补采")
        # 退避 5s → 10s → 死信（最多 3 次尝试）。等 120s 足够走完两轮退避。
        got, stt = _wait_task_done(down_task, 120)
        check("判据#4b 恢复后该任务自动补采成功（无需人工干预）", got,
              "task=%s status=%s attempts=%s"
              % (down_task, stt,
                 _q1("SELECT attempts FROM tasks WHERE id=%s" % down_task)))
        check("恢复后无残留未完成任务", q(
            "SELECT count(*) FROM tasks WHERE status IN ('pending','in_flight')") == "0",
            "pending+in_flight=%s"
            % q("SELECT count(*) FROM tasks WHERE status IN ('pending','in_flight')"))
        check("无任务进死信（数据源最终恢复，不该有任务被放弃）",
              q("SELECT count(*) FROM tasks WHERE status='dead'") == "0",
              "dead=%s" % q("SELECT count(*) FROM tasks WHERE status='dead'"))

        # 补采应真的写入了证据（该任务对应事件从 0 条变成有）。
        n_down_ev = q("SELECT count(*) FROM evidence e JOIN incidents i "
                      "ON i.id = e.incident_id WHERE i.service = 'm4-down-svc'")
        check("判据#4c 补采写入了证据（此前因数据源宕机一条都没采到）",
              n_down_ev.isdigit() and int(n_down_ev) >= 5,
              "m4-down-svc 证据数=%s" % n_down_ev)
        return 0
    finally:
        for mock in (prom, loki):
            try:
                if mock:
                    mock.stop()
            except Exception:
                pass
        if wrk is not None:
            # 先 terminate 再等：worker 的 drain 最多等 15s，
            # 直接 kill 会让「已采集但未落库」的证据看起来像没采到。
            try:
                wrk.terminate()
                wrk.wait(timeout=25)
            except Exception:
                try:
                    wrk.kill()
                except Exception:
                    pass
            w("  [m4] worker 已停")
        if wrk_log is not None:
            try:
                wrk_log.flush()
                with open(wrk_log_path, "rb") as f:
                    txt = f.read().decode("utf-8", errors="replace")
                w("  [m4] worker 日志 %d 字节，尾部 20 行：" % len(txt))
                for line in txt.splitlines()[-20:]:
                    w("        | " + line)
            except Exception as e:
                w("  [m4] 读 worker 日志失败: %s" % e)
        if os.path.exists(EXE_WORKER):
            try:
                os.remove(EXE_WORKER)
            except Exception:
                pass


def main():
    pg = None
    srv = None
    srv_log = None
    srv_log_path = ""
    try:
        # --- 起 PG ---
        if not port_open(PORT_PG):
            logf = open(os.path.join(PG, "pg_func.log"), "ab")
            pg = subprocess.Popen(
                [os.path.join(PG_BIN, "postgres.exe"), "-D", PG_DATA,
                 "-p", str(PORT_PG)],
                stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN)
            for _ in range(180):
                ir = subprocess.run([os.path.join(PG_BIN, "pg_isready.exe"),
                                     "-h", "127.0.0.1", "-p", str(PORT_PG)],
                                    capture_output=True, timeout=10)
                if ir.returncode == 0:
                    break
                if pg.poll() is not None:
                    break
                time.sleep(1.0)
        w("[0] PG(5434) 就绪")

        psql("DROP DATABASE IF EXISTS %s WITH (FORCE)" % DB)
        psql("CREATE DATABASE %s" % DB)
        w("[1] 已建干净库 %s" % DB)

        # --- 编译 ---
        t0 = time.time()
        b = subprocess.run(["go", "build", "-o", EXE, "./cmd/iip-server"],
                           cwd=REPO, capture_output=True, text=True,
                           encoding="utf-8", errors="replace", timeout=600)
        w("[2] go build rc=%d (%.1fs) %s"
          % (b.returncode, time.time() - t0, (b.stderr or "")[:300]))
        if b.returncode != 0:
            return 2

        # --- 起 server（日志落文件，不能用 PIPE：管道满 -> slog 阻塞 -> 服务假死）---
        srv_log_path = os.path.splitext(OUT)[0] + "_srv.log"
        try:
            srv_log = open(srv_log_path, "wb")
            srv_out = srv_log
        except OSError:
            srv_log = None
            srv_out = subprocess.DEVNULL
        srv = subprocess.Popen(
            [EXE, "--http-addr=:%d" % PORT_HTTP, "--pg-url=" + DSN,
             "--log-level=info"],
            cwd=REPO, stdout=srv_out, stderr=subprocess.STDOUT)
        ready = False
        for _ in range(120):
            if srv.poll() is not None:
                break
            try:
                with urllib.request.urlopen(BASE + "/healthz", timeout=3) as resp:
                    if resp.status == 200:
                        ready = True
                        break
            except Exception:
                time.sleep(0.5)
        w("[3] server 就绪=%s (pid=%s)" % (ready, srv.pid))
        if not ready:
            if srv_log is not None:
                srv_log.flush()
                with open(srv_log_path, "rb") as f:
                    w("    server 输出: %s"
                      % f.read().decode("utf-8", errors="replace")[:1500])
            return 3

        # ============ 场景 1：M1 接入 + M2 关联建边 ============
        section("F0 基础链路：M1 接入 -> M2 关联 -> 事件生成")
        st, b1 = post_alert("LatencyHigh", "checkout")
        check("投递首条告警 -> 200", st == 200, "status=%s" % st)
        check("webhook accepted 1 条",
              isinstance(b1, dict) and b1.get("accepted") == 1,
              "body=%s" % (b1 if not isinstance(b1, dict) else
                           {k: b1.get(k) for k in ("accepted", "rejected")}))
        time.sleep(0.3)

        a1 = alert_id_of(alertname="LatencyHigh")
        check("告警已落库", a1.isdigit(), "alerts.id=%s" % a1)

        st, b2 = post_alert("ErrorRateHigh", "checkout")
        check("投递同服务第二条 -> 200", st == 200, "status=%s" % st)
        time.sleep(0.3)

        a2 = alert_id_of(alertname="ErrorRateHigh")
        check("第二条已落库", a2.isdigit(), "alerts.id=%s" % a2)

        n_inc = q("SELECT count(*) FROM incidents")
        check("恰好生成 1 个事件（同服务两条应关联）", n_inc == "1",
              "incidents=%s" % n_inc)

        st, lst = req("GET", "/api/v1/incidents")
        rows = lst.get("incidents", []) if isinstance(lst, dict) else []
        check("列表返回 1 条", st == 200 and len(rows) == 1,
              "count=%d" % len(rows))
        if not rows:
            w("  事件列表为空，后续用例无法继续")
            return 4
        inc = rows[0]["id"]
        w("  事件 id=%s severity=%s alert_count=%s"
          % (inc, rows[0].get("severity"), rows[0].get("alert_count")))

        st, det = req("GET", "/api/v1/incidents/%d" % inc)
        check("详情聚合出 2 条关联告警",
              st == 200 and len(det.get("alerts", [])) == 2,
              "alerts=%d" % len(det.get("alerts", [])))
        # 这里不断言档位，而是断言「score 与 needs_review 自洽」——
        # 档位由 score 落在哪个区间决定，精确分数由单测锁（0.70 / 0.72 等），
        # 功能测试重复断言具体数值只会把「我猜错了档位」误报成产品缺陷。
        alerts_d = det.get("alerts", [])
        by_id = {a["alert_id"]: a for a in alerts_d}
        first, second = by_id.get(int(a1)), by_id.get(int(a2))
        check("首条（事件起因 NEW 档）score 为 NULL 而非 0",
              first is not None and first.get("correlation_score") is None,
              "score=%s" % (first.get("correlation_score") if first else None))
        check("首条 needs_review=false", first is not None
              and first.get("needs_review") is False, "")
        check("第二条带真实 score（非 NEW 档必有）",
              second is not None and second.get("correlation_score") is not None,
              "score=%s" % (second.get("correlation_score") if second else None))
        # 默认阈值 merge=0.80 review=0.50：score 落在 [0.50,0.80) 应判 REVIEW。
        sc = second.get("correlation_score") if second else None
        check("score 与 needs_review 自洽（[0.50,0.80) -> REVIEW）",
              sc is not None and 0.50 <= sc < 0.80
              and second.get("needs_review") is True,
              "score=%s needs_review=%s" % (sc, second.get("needs_review") if second else None))
        evs = [e.get("event_type") for e in det.get("timeline", [])]
        check("时间线含 alert_fired 与 alert_linked",
              "alert_fired" in evs and "alert_linked" in evs,
              "timeline=%s" % evs)

        # ============ 场景 2：REVIEW 档需要「无依赖」构造 ============
        # 同服务同标签无依赖 = 0.70，落在 REVIEW 档；
        # 上面那两条带了依赖边（checkout->payment）会升到 0.85 进 MERGE。
        # 这里用一个拓扑里不存在依赖的服务，保证落到 REVIEW。
        section("F1 抑制闭环：reject 后同指纹新告警应降级为 NEW 档")

        # 第二个服务：孤立服务，无任何依赖边。
        # 先记基线事件数：孤立服务要投**两条**告警才构成关联，
        # 绝对值容易记错，后面一律用增量断言。
        n_base = int(q("SELECT count(*) FROM incidents"))
        w("  抑制测试前事件数=%d" % n_base)

        st, _ = post_alert("QueueDepthHigh", "isolated-svc")
        time.sleep(0.3)
        a3 = alert_id_of(alertname="QueueDepthHigh")
        check("孤立服务首条告警落库", a3.isdigit(), "alerts.id=%s" % a3)

        st, _ = post_alert("CacheEvictionHigh", "isolated-svc")
        time.sleep(0.3)

        st, rv = req("GET", "/api/v1/reviews")
        revs = rv.get("reviews", []) if isinstance(rv, dict) else []
        check("REVIEW 队列非空（孤立服务同标签 -> 0.70 落 REVIEW 档）",
              st == 200 and len(revs) >= 1,
              "queue=%d" % len(revs))
        if not revs:
            w("  REVIEW 队列为空，无法验证抑制闭环")
            return 5

        # 队列首条就是 isolated-svc 的第二条告警（CacheEvictionHigh）：
        # QueueDepthHigh 是事件起因（NEW 档），不在队列里，别去找它。
        # 抑制验证要用**被 reject 这条自己的指纹**去造同指纹新告警。
        edge = revs[0]
        rev_inc = edge["incident_id"]
        rev_alert = edge["alert"]["alert_id"]
        rev_fp = edge["alert"]["fingerprint"]
        rev_score = edge["alert"]["correlation_score"]
        w("  取首条边 (incident=%s, alert=%s, score=%s, needs_review=%s)"
          % (rev_inc, rev_alert, rev_score, edge["alert"].get("needs_review")))
        check("REVIEW 档条目的 needs_review 为 true",
              edge["alert"].get("needs_review") is True, "")

        # reject 该边
        st, rj = req("POST", "/api/v1/reviews/%d/%d" % (rev_inc, rev_alert),
                     {"decision": "reject", "actor": "func", "reason": "功能测试：不是同一个问题"})
        check("reject -> 200", st == 200, "status=%s" % st)
        check("reject 返回 suppressed=true",
              isinstance(rj, dict) and rj.get("result", {}).get("suppressed") is True,
              "result=%s" % (rj.get("result") if isinstance(rj, dict) else rj))
        check("reject 返回 unlinked（该告警已无其他边）",
              isinstance(rj, dict) and rj.get("result", {}).get("unlinked") is True,
              "result=%s" % (rj.get("result") if isinstance(rj, dict) else rj))

        # 抑制行已落库
        n_supp = q("SELECT count(*) FROM review_suppressions "
                   "WHERE fingerprint = '%s' AND incident_id = %s" % (rev_fp, rev_inc))
        check("抑制行已落库（键为 fingerprint+incident_id）",
              n_supp == "1", "rows=%s" % n_supp)

        # F2：该告警已无其他边 -> alerts.incident_id 应清空
        ptr = q("SELECT coalesce(incident_id::text,'NULL') FROM alerts WHERE id = %s" % rev_alert)
        check("F2 reject 后该告警的 incident_id 已清空（反规范化指针）",
              ptr == "NULL", "incident_id=%s" % ptr)

        # 边已删除
        n_edge = q("SELECT count(*) FROM incident_alerts "
                   "WHERE incident_id = %s AND alert_id = %s" % (rev_inc, rev_alert))
        check("关联边已删除", n_edge == "0", "rows=%s" % n_edge)

        # **核心**：同指纹再来一条告警，应降级为 NEW 档（另起事件）而不是回 REVIEW 队列
        # 造「同指纹的第二条告警」要连做两步，缺一不可（2026-10-03 各踩一次）：
        #  1) 先把被 reject 的那条投递为 resolved —— 否则它「仍在 firing」，
        #     下一条同指纹告警会被去重 skip 掉，连库都不进，更谈不上抑制。
        #  2) 再用**动态键 instance** 区分新告警（不能用 round 之类：
        #     指纹 = 除 instance/pod/container/node 之外的全部 label，
        #     多一个非常规键就换了指纹，抑制场景根本不成立）。
        # resolved 那条仍用同一个固定窗口：它的作用只是把「仍在 firing」
        # 的行关掉，好让下一条同指纹告警能落库。
        # 注意不能把 endsAt 设成与 startsAt 相同 —— 那会让 windowDuration=0，
        # time 维直接归零。
        rev_name = rev_alert_name(rev_alert)
        w("  被 reject 的告警 alertname=%s（据其指纹造同指纹新告警）" % rev_name)
        st, rb = post_alert(rev_name, "isolated-svc",
                            {"instance": "smoke-round1"}, status="resolved",
                            end_at=FIXED_END)
        w("  旧条转 resolved -> %s（body=%s）" % (st, rb))
        time.sleep(0.3)

        st, _ = post_alert(rev_name, "isolated-svc",
                           {"instance": "smoke-round2"})
        time.sleep(0.5)

        st, rv2 = req("GET", "/api/v1/reviews")
        revs2 = rv2.get("reviews", []) if isinstance(rv2, dict) else []
        back = [e for e in revs2 if e["alert"]["fingerprint"] == rev_fp]
        check("F1 核心：同指纹新告警**未**回 REVIEW 队列（抑制生效）",
              len(back) == 0, "同指纹仍在队列的边=%d" % len(back))

        # 事件数只断言「至少 +1」（round2 那条确实另起了新事件）。
        # 断言精确 +1 是我算错：isolated-svc 自己也发过两条告警，
        # 第一条已建过一个事件，基线不能取在它之前。
        # 抑制是否真的生效，由下面「未建回原事件」那条断言负责 ——
        # 那是抑制的**定义**，事件数只是伴随现象。
        n_inc2 = int(q("SELECT count(*) FROM incidents"))
        check("抑制后新告警另起了新事件（事件数至少 +1）", n_inc2 >= n_base + 1,
              "before=%d after=%d" % (n_base, n_inc2))
        # 新事件必须是 isolated-svc 的（不是把抑制键搞错、建到别的服务下）
        new_svc = q("SELECT service FROM incidents ORDER BY id DESC LIMIT 1")
        check("新事件归属于被抑制的那个服务", new_svc == "isolated-svc",
              "service=%s" % new_svc)
        # 关键：它**没有**建回原事件（否则抑制就白写了）
        a_round2 = alert_id_of(alertname=rev_name, inst="smoke-round2")
        check("抑制验证用的新告警确已落库", a_round2.isdigit(),
              "alerts.id=%s" % a_round2)

        # 不能只按 fingerprint 过滤：若前置的同指纹断言失败，这里会**静默通过**。
        # 所以再按 alert_id 查一次：新告警既不在原事件边表里，也不在队列里。
        by_alert = [e for e in revs2 if e["alert"]["alert_id"] == int(a_round2)]
        check("F1 交叉验证：按 alert_id 查队列也不在（防同指纹断言假通过）",
              len(by_alert) == 0,
              "按 alert_id 仍在队列=%d" % len(by_alert))

        # **前置条件断言**：两条告警必须同指纹，否则下面验的不是抑制。
        # 上一版就是因为多加了 round=2 这个 label（它进指纹）导致指纹不同，
        # 抑制场景根本不成立，却因为按 fingerprint 过滤而「假通过」。
        fp_new = q("SELECT fingerprint FROM alerts WHERE id = %s" % a_round2)
        check("前置条件：新告警与被 reject 的告警**同指纹**",
              fp_new == rev_fp, "rejected=%s new=%s" % (rev_fp[:12], fp_new[:12]))
        # 且抑制行记的就是这个指纹
        n_supp2 = q("SELECT count(*) FROM review_suppressions "
                    "WHERE fingerprint = '%s'" % fp_new)
        check("抑制行的指纹与新告警一致（键用对了）", n_supp2 == "1",
              "rows=%s" % n_supp2)
        n_back = q("SELECT count(*) FROM incident_alerts "
                   "WHERE incident_id = %s AND alert_id = %s"
                   % (rev_inc, a_round2))
        check("未建回原事件（抑制真的挡住了这条边）", n_back == "0",
              "误建边数=%s" % n_back)

        # 抑制只对 REVIEW 档生效，MERGE 不受影响 —— 上面 checkout 那组应仍是 1 个事件
        n_ck = q("SELECT count(*) FROM incidents WHERE service = 'checkout'")
        check("抑制不影响其它事件（checkout 仍 1 个）", n_ck == "1",
              "checkout 事件数=%s" % n_ck)

        # ============ 场景 3：resolved_at 语义 ============
        section("F3 resolved_at 语义：ignored 不写 resolved_at")

        # 取一个 checkout 事件，流转到 ignored
        st, b = req("POST", "/api/v1/incidents/%d/transition" % inc,
                    {"to": "ignored", "actor": "func"})
        check("open -> ignored 200", st == 200, "status=%s" % st)

        ra = q("SELECT coalesce(resolved_at::text,'NULL') FROM incidents WHERE id = %s" % inc)
        check("ignored **不写** resolved_at（误报不该计入 MTTR）", ra == "NULL",
              "resolved_at=%s" % ra)

        # 正常解决：resolved_at 必须被写
        inc2 = None
        rows2 = (req("GET", "/api/v1/incidents?service=isolated-svc")[1]
                 .get("incidents", []))
        if rows2:
            inc2 = rows2[0]["id"]
            st, b = req("POST", "/api/v1/incidents/%d/transition" % inc2,
                        {"to": "resolved", "actor": "func"})
            check("open -> resolved 200", st == 200, "status=%s" % st)
            ra2 = q("SELECT coalesce(resolved_at::text,'NULL') FROM incidents WHERE id = %s" % inc2)
            check("resolved **写入** resolved_at", ra2 not in ("NULL", "", "ERR"),
                  "resolved_at=%s" % ra2)

        # ============ 场景 4：配置改后免重启生效（M2）============
        section("F4 关联配置：改后免重启生效 + 非法值拒绝")
        st, cfg = req("GET", "/api/v1/correlation/config")
        check("读配置 200", st == 200, "status=%s" % st)
        w("  当前配置: %s" % json.dumps(cfg, ensure_ascii=False)[:200])

        # PUT 是整体替换：weights/thresholds/enabled 三个字段都要给全，
        # 少给 enabled 会把关联开关抹成 false，后续断言全失效。
        st, _ = req("PUT", "/api/v1/correlation/config",
                    {"thresholds": {"merge": 0.99, "review": 0.99},
                     "weights": cfg.get("weights", {}),
                     "enabled": True})
        check("改阈值为 0.99 -> 200", st == 200, "status=%s" % st)
        st, cfg2 = req("GET", "/api/v1/correlation/config")
        mt = cfg2.get("thresholds", {}).get("merge") if isinstance(cfg2, dict) else None
        check("新阈值立即可读（免重启）", abs(float(mt) - 0.99) < 1e-6,
              "merge=%s" % mt)

        # 还原
        st, _ = req("PUT", "/api/v1/correlation/config",
                    {"thresholds": {"merge": 0.80, "review": 0.50},
                     "weights": cfg.get("weights", {}),
                     "enabled": True})
        check("还原阈值 -> 200", st == 200, "status=%s" % st)

        # ============ 场景 5：M1 告警列表 ============
        section("F5 告警列表（M1）")
        st, al = req("GET", "/api/v1/alerts?limit=5")
        items = al.get("alerts", []) if isinstance(al, dict) else []
        check("告警列表 200 且非空", st == 200 and len(items) > 0,
              "status=%s count=%d" % (st, len(items)))
        if items:
            it = items[0]
            # M1 起就带 tag（StoredAlert），这里顺带确认键名仍是 snake_case
            check("告警列表字段名为 snake_case",
                  "id" in it and "fingerprint" in it and "labels" in it,
                  "keys=%s" % sorted(it.keys())[:10])

        st, b = req("GET", "/api/v1/alerts?limit=abc&status=bogus")
        check("告警列表脏参数退化 200", st == 200, "status=%s" % st)

        # ============ 场景 6：M4 证据采集闭环 ============
        # 排在 healthz 之前：这一段要起 iip-worker 与两个假采集器，
        # 涉及进程生命周期，失败时不该让后面的 healthz 三态也一起丢。
        rc = m4_section(inc)
        if rc != 0:
            return rc

        # ============ 场景 7：healthz ============
        section("F6 健康检查")
        # 契约是 **text/plain 的 ok / unavailable**（README 与 M0 计划明文规定，
        # --healthcheck 与 compose healthcheck 依赖这个字符串），不是 JSON。
        # 断言 body 字符串本身，不要擅自改协议去迎合测试。
        st, hb = req("GET", "/healthz")
        check("healthz 200", st == 200, "status=%s" % st)
        check("healthz 返回 text/plain 的 ok", hb == "ok",
              "body=%r" % (hb,))
        # 三态闭环的 200 端已验；503 端（PG 不可用）由停库场景验，
        # 放在脚本末尾单独做，避免中途停库影响其它用例。

        # ============ 场景 7：healthz 三态闭环（停 PG -> 503）============
        # 排在最后：停库之后所有 DB 用例都跑不了。
        section("F7 healthz 真查库：停 PG 后应 503（不查库的 healthz 是假健康）")
        if pg is not None:
            subprocess.run([os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PG_DATA,
                            "stop", "-m", "fast", "-t", "60"],
                           capture_output=True, timeout=120)
            for _ in range(60):
                if not port_open(PORT_PG):
                    break
                time.sleep(1.0)
            st, hb = req("GET", "/healthz")
            check("PG 停后 healthz -> 503", st == 503, "status=%s" % st)
            check("PG 停后 body 为 unavailable", hb == "unavailable",
                  "body=%r" % (hb,))
            # 恢复
            logf = open(os.path.join(PG, "pg_func.log"), "ab")
            pg = subprocess.Popen(
                [os.path.join(PG_BIN, "postgres.exe"), "-D", PG_DATA,
                 "-p", str(PORT_PG)],
                stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN)
            back = False
            for _ in range(120):
                ir = subprocess.run([os.path.join(PG_BIN, "pg_isready.exe"),
                                     "-h", "127.0.0.1", "-p", str(PORT_PG)],
                                    capture_output=True, timeout=10)
                if ir.returncode == 0:
                    back = True
                    break
                time.sleep(1.0)
            check("PG 恢复", back, "")
            if back:
                st, hb = req("GET", "/healthz")
                check("恢复后 healthz 回到 200 ok", st == 200 and hb == "ok",
                      "status=%s body=%r" % (st, hb))
        else:
            w("  PG 非本脚本启动（复用已有实例），跳过停库场景")

        w("")
        w("===== 结论 =====")
        if FAILS:
            w("FAIL（%d 项）：%s" % (len(FAILS), ", ".join(FAILS)))
            return 1
        w("PASS（全部断言通过）")
        return 0
    finally:
        if srv is not None and srv_log is not None:
            try:
                srv_log.flush()
                with open(srv_log_path, "rb") as f:
                    log_txt = f.read().decode("utf-8", errors="replace")
                w("[srv] 服务日志 %d 字节，尾部 25 行：" % len(log_txt))
                for line in log_txt.splitlines()[-25:]:
                    w("      | " + line)
            except Exception as e:
                w("[srv] 读服务日志失败: %s" % e)
        if srv is not None and srv.poll() is None:
            srv.terminate()
            try:
                srv.wait(timeout=15)
            except Exception:
                srv.kill()
            w("[end] server 已停")
        if pg is not None:
            try:
                psql("DROP DATABASE IF EXISTS %s WITH (FORCE)" % DB)
            except Exception:
                pass
            subprocess.run([os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PG_DATA,
                            "stop", "-m", "fast", "-t", "300"],
                           capture_output=True, timeout=360)
            w("[end] PG 已停（仍监听=%s）" % port_open(PORT_PG))
        if os.path.exists(EXE):
            try:
                os.remove(EXE)
            except Exception:
                pass
        with open(OUT, "w", encoding="utf-8") as f:
            f.write("\n".join(_L) + "\n")


if __name__ == "__main__":
    sys.exit(main())
