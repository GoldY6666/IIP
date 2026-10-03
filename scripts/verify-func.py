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

        # ============ 场景 6：healthz ============
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
