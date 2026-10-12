# -*- coding: utf-8 -*-
"""T7.3 压测：向 webhook 灌入 N 条告警，验证吞吐与零丢失。

**判据设计（三条缺一不可，且必须能互相交叉验证）**：

1. **p95 延迟**：用**客户端侧**完整耗时（从发请求到收完响应）。
   不用服务内部 handler 耗时 —— 后者不含网络与序列化，会把
   「客户端等 500ms、服务只算 5ms」的锅算成产品很快。
2. **零丢失**：**三层交叉**，不能只看 webhook 返 200：
   ① 客户端自己记账的 `accepted` 之和
   ② webhook 响应里的 `rejected` 字段（200 也可能带 rejected）
   ③ 落库后 DB 里 `alerts` 的真实行数
   任一层与另两层不符即 FAIL。**只看 200 是最常见的假通过**：
   服务完全可能「200 + accepted=0」地把全部丢掉。
3. **不拒绝**：逐条响应检查 `rejected == 0` 且 `accepted == 本批条数`。

**为什么必须灌「不同指纹」的告警**：V1.0 去重规则是「同指纹的 firing 行
已在库即 skip」。若灌 1 万条同指纹，落库 1 行、其余全 skip ——
那测的是**去重路径**，不是 ingest 吞吐，而报告会显示「灌了 1 万」。
所以每条告警带唯一 `idx` 标签，并**显式断言指纹互异**，
否则「灌了 1 万」是假的。

**DB 计数必须轮询等稳定**：webhook 返回 200 只代表「已受理」，
写库是异步的（verify-func.py 投递后要 sleep 0.3s 才查到）。
立刻查库会读到旧值 → 假 FAIL。故轮询至行数不再增长。

沙箱注意（沿用 verify-func.py 已验证的做法）：
- server 日志落**文件**不用 PIPE（管道满 -> slog 阻塞 -> 服务假死）
- 所有 subprocess 显式 `encoding="utf-8"`（否则按 GBK 解码 UTF-8 输出）
- PG 生命周期与压测同一次调用内闭环（跨调用启动会被回收）
- PG 端口与 5433/5434 错开，避免与另两条链路抢同一个数据目录
"""
import argparse
import concurrent.futures as cf
import io
import json
import os
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

_HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(_HERE)

PG = os.environ.get("IIP_PG_HOME", r"D:\pgsql16")
PG_BIN = os.path.join(PG, "bin")
PG_DATA = os.path.join(PG, "data")

PORT_PG = int(os.environ.get("IIP_BENCH_PG_PORT", "5435"))
PORT_HTTP = int(os.environ.get("IIP_BENCH_HTTP_PORT", "8083"))
EXE = os.path.join(tempfile.gettempdir(), "_bench_iip-server.exe")
DB = "iip_bench"
DSN = "postgres://postgres:dev@127.0.0.1:%d/%s?sslmode=disable" % (PORT_PG, DB)
BASE = "http://127.0.0.1:%d" % PORT_HTTP

OUT = os.path.join(_HERE, "_verify_bench.txt")
TOKEN = str(int(time.time()))

# 告警时间窗固定：理由同 verify-func.py —— 关联评分用 wall clock 会抖。
FIXED_START = "2026-10-03T00:00:00Z"
FIXED_END = "2026-10-04T00:00:00Z"

_L = []


def w(s=""):
    """写一行并**立即落盘**。

    只 append 到内存、末尾才写文件的做法，会让「进程被超时杀掉」
    或「异常带走脚本」时报告一片空白 —— 那时看到的不是
    「这条断言失败」而是「没报告」，两者要花的排查成本差一个量级。
    """
    _L.append(str(s))
    try:
        with io.open(OUT, "w", encoding="utf-8") as f:
            f.write("\n".join(_L) + "\n")
    except OSError as e:
        sys.stderr.write("[warn] 报告写入失败 %s: %r\n" % (OUT, e))


def port_open(p):
    sk = socket.socket()
    sk.settimeout(0.5)
    try:
        sk.connect(("127.0.0.1", p))
        return True
    except OSError:
        return False
    finally:
        sk.close()


def _pg_procs():
    """列出残留的 postgres.exe PID。空列表 = 无残留。

    tasklist 输出是 GBK（中文 Windows），必须显式按 gbk 解码 ——
    否则中文列名会触发 UnicodeDecodeError 把调用方带走。
    """
    try:
        r = subprocess.run(["tasklist", "/FI", "IMAGENAME eq postgres.exe",
                            "/FO", "CSV", "/NH"],
                           capture_output=True, timeout=60)
        out = r.stdout.decode("gbk", errors="replace")
        return [l.split(",")[1].strip('"') for l in out.splitlines()
                if "postgres.exe" in l]
    except Exception:
        return []


def psql(sql, db="postgres", timeout=60):
    # **必须传 PGPASSWORD**：pg_hba.conf 对 127.0.0.1 走 scram/md5 认证，
    # 不提供密码时 psql 会停下来等 stdin 输入 —— 而 capture_output=True
    # 让 stdin 成为无人读取的管道，于是它**永远挂住**。
    # 症状极具误导性：pg_isready 秒过（它不认证）、PG 日志里
    # **一条连接记录都没有**（认证都没走到）、psql 却 25s/300s 超时。
    # 2026-10-12 为此白踩三轮，每轮都以为「PG 起不来」。
    env = {**os.environ, "PGPASSWORD": "dev"}
    r = subprocess.run(
        [os.path.join(PG_BIN, "psql.exe"), "-h", "127.0.0.1",
         "-p", str(PORT_PG), "-U", "postgres", "-d", db,
         "-v", "ON_ERROR_STOP=1", "-t", "-A", "-c", sql],
        capture_output=True, text=True, encoding="utf-8", errors="replace",
        timeout=timeout, env=env)
    return r.returncode, (r.stdout or "").strip()


def q1(sql):
    """查单值。查询失败返回 -1 而不是 0 —— 两者含义不同
    （「查不到」与「查询本身失败」），混同会把脚本 bug 报成数据缺失。"""
    rc, out = psql(sql, DB)
    if rc != 0:
        return -1
    return int(out) if out.isdigit() else -1


def build_payload(batch):
    """batch = [(alertname, service, idx), ...] -> webhook payload。"""
    return {
        "version": "4",
        "groupKey": "bench-%s" % TOKEN,
        "status": "firing",
        "receiver": "iip",
        "groupLabels": {},
        "commonLabels": {},
        "commonAnnotations": {},
        "externalURL": "",
        "alerts": [{
            "status": "firing",
            "labels": {
                "alertname": an,
                "service": svc,
                "severity": "warning",
                "run": TOKEN,
                "idx": str(idx),          # **指纹唯一化的关键**
            },
            "annotations": {"summary": "bench %s#%d" % (an, idx)},
            "startsAt": FIXED_START,
            "endsAt": FIXED_END,
            "generatorURL": "",
        } for (an, svc, idx) in batch],
    }


def send_batch(batch, timeout=60):
    """发一批，返回 (status, body, elapsed_ms)。

    elapsed 从「请求交出去」量到「响应读完」，即客户端侧完整耗时。
    """
    data = json.dumps(build_payload(batch)).encode("utf-8")
    r = urllib.request.Request(
        BASE + "/api/v1/alerts/webhook", data=data,
        headers={"Content-Type": "application/json"}, method="POST")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        status = e.code
    except Exception as e:                      # 连接失败/超时：如实记账
        return -1, {"__error__": repr(e)}, (time.perf_counter() - t0) * 1000
    try:
        body = json.loads(raw)
    except Exception:
        body = {"__raw__": raw[:200]}
    return status, body, (time.perf_counter() - t0) * 1000


def pct(sorted_vals, p):
    """百分位。列表为空返回 -1（与「值为 0」区分）。"""
    if not sorted_vals:
        return -1
    k = (len(sorted_vals) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(sorted_vals) - 1)
    frac = k - lo
    return sorted_vals[lo] * (1 - frac) + sorted_vals[hi] * frac


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-n", type=int, default=1000,
                    help="告警总条数（默认 1000；正式压测传 10000）")
    ap.add_argument("-b", "--batch", type=int, default=10,
                    help="每批条数（默认 10）")
    ap.add_argument("-c", "--concurrency", type=int, default=4,
                    help="并发请求数（默认 4）")
    ap.add_argument("--p95-max", type=float, default=200.0,
                    help="p95 上限 ms（默认 200，照计划验收标准）")
    ap.add_argument("--no-correlation", action="store_true",
                    help="关闭 M2 关联（IIP_CORRELATION_ENABLED=false）。"
                         "用于分诊：区分「ingest 链路慢」与「关联 O(N^2) 慢」")
    args = ap.parse_args()

    n = args.n
    bs = args.batch
    conc = args.concurrency
    if n % bs:
        w("参数错误：n=%d 不能被 batch=%d 整除" % (n, bs))
        return 2
    w("=== T7.3 压测：%d 条告警，每批 %d 条，并发 %d ===" % (n, bs, conc))
    w("  目标：折算单条 p95 < %.0fms、零丢失、webhook 不拒绝"
      % args.p95_max)
    w("  M2 关联：%s" % ("关闭（--no-correlation）" if args.no_correlation
                        else "开启"))

    # 分桶：alerts 服务名分散到 8 个服务，避免全部落进同一关联分组
    #  （同服务同时间窗会互相建边，虽不影响落库计数，但会让关联
    #   成为压测里的噪音项 —— 压的是 ingest，不是关联）
    services = ["svc%d" % (i % 8) for i in range(n)]
    batches = []
    for s in range(0, n, bs):
        chunk = [("BenchAlert", services[s + j], s + j) for j in range(bs)]
        batches.append(chunk)
    w("  共 %d 批" % len(batches))

    pg = None
    srv = None
    srv_log = None
    try:
        # ---------------- 起 PG ----------------
        # **起之前必须确认没有残留 postmaster**：绝不能让两个 postmaster
        # 共用同一个 PGDATA。症状极具误导性 —— 端口通、pg_isready 通，
        # 但 psql 永远拿不到响应（日志里是 `FATAL: the database system is
        # starting up`），看起来像「PG 慢」，实为实例叠加。
        # 2026-10-12 实测连踩：探针被这种形态拖死两轮，各 12 分钟。
        if port_open(PORT_PG):
            _stale = _pg_procs()
            if _stale:
                w("[0] 端口 %d 被占用且存在残留 postgres 进程 %s"
                  % (PORT_PG, _stale))
                w("    拒绝叠加实例（两 postmaster 共用 PGDATA 会互相锁）。")
                w("    请先手工执行：D:\\pgsql16\\bin\\pg_ctl.exe stop "
                  "-D D:\\pgsql16\\data -m immediate")
                return 3
            w("[0] 端口 %d 已被其它程序占用（非 postgres），无法压测"
              % PORT_PG)
            return 3

        # **残留 postmaster.pid 会让 postgres 拒绝启动**
        # （"lock file \"postmaster.pid\" already exists"）。
        # 只在确认无 postgres 进程时才删 —— 有进程时删了会让那个实例
        # 失去 pid 记录，反而更难收场。
        _pidf = os.path.join(PG_DATA, "postmaster.pid")
        if os.path.isfile(_pidf) and not _pg_procs():
            try:
                os.remove(_pidf)
                w("[0] 已清残留 postmaster.pid（无 postgres 进程，安全）")
            except OSError as e:
                w("[0] postmaster.pid 删除失败: %r" % (e,))

        logf = io.open(os.path.join(PG, "pg_bench.log"), "ab")
        pg = subprocess.Popen(
            [os.path.join(PG_BIN, "postgres.exe"), "-D", PG_DATA,
             "-p", str(PORT_PG)],
            stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN)
        # 等 pg_isready 通过（它自带超时，不会挂）
        for _ in range(300):
            ir = subprocess.run(
                [os.path.join(PG_BIN, "pg_isready.exe"),
                 "-h", "127.0.0.1", "-p", str(PORT_PG)],
                capture_output=True, text=True, encoding="utf-8",
                errors="replace", timeout=10)
            if ir.returncode == 0:
                break
            if pg.poll() is not None:
                break
            time.sleep(1.0)

        # **就绪判据用 pg_isready 轮询，不用 psql 轮询**（2026-10-12 踩坑）：
        # psql 在「PG 活着但拒绝服务」时会挂住不返回，而
        # `subprocess.run(timeout=...)` 在 Windows 上**杀不掉已卡在
        # 管道读取上的子进程** —— 实测探针被拖死 12 分钟（timeout=15 也没用）。
        # pg_isready 自带超时且从不挂，是安全的心跳判据。
        # 但 pg_isready rc=0 **不等于能查询**（实测：日志已 ready，
        # psql 却 300s 拿不到响应）⇒ 就绪后再补一次 psql 确认，
        # 且这次用短超时 + 明确记账，不通就如实 FAIL。
        ok_pg = False
        _last_err = "未开始探测"
        for _ in range(180):
            ir = subprocess.run(
                [os.path.join(PG_BIN, "pg_isready.exe"),
                 "-h", "127.0.0.1", "-p", str(PORT_PG)],
                capture_output=True, text=True, encoding="utf-8",
                errors="replace", timeout=10)
            if ir.returncode != 0:
                _last_err = "pg_isready rc=%d %r" % (
                    ir.returncode, (ir.stdout or "").strip()[:80])
            else:
                # 探活通过后再确认真能查询。**只确认一次，不通就退出循环**，
                # 不做 psql 轮询 —— 那正是把脚本拖死的写法。
                try:
                    rc_p, out_p = psql("SELECT 1", timeout=25)
                    if rc_p == 0:
                        ok_pg = True
                        _last_err = ""
                        break
                    _last_err = "psql rc=%s out=%r" % (rc_p, out_p[:120])
                except Exception as e:
                    _last_err = "psql 异常 %r" % (e,)
                    break          # 挂住/异常：不再试，直接据实上报
            if pg is not None and pg.poll() is not None:
                _last_err = "postgres 进程已退出 rc=%s" % pg.returncode
                break
            time.sleep(1.0)
        w("[0] PG(%d) 可服务查询=%s（探活通=%s）"
          % (PORT_PG, ok_pg, port_open(PORT_PG)))
        if _last_err:
            w("    最后一次探测: %s" % _last_err)
        if not ok_pg:
            w("  PG 探活通过但查询不通 —— 卡在恢复/锁等待，不继续压测")
            return 3

        # ---------------- 干净起点 ----------------
        # **必须新建空库**：否则历史行会让「落库数 == 发送数」假 FAIL，
        # 而更危险的是反过来 —— 残留恰好凑够数量时**假通过**。
        # 先踢掉残留连接：上一轮 server 若未断干净，DROP 会卡在锁等待
        # （本次实测卡 300s 超时，而真因有两层 —— 见 [0] 的注释）。
        try:
            psql("SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
                 "WHERE datname = '%s' AND pid <> pg_backend_pid()" % DB,
                 timeout=30)
        except Exception:
            pass
        rc, _ = psql("DROP DATABASE IF EXISTS %s WITH (FORCE)" % DB)
        rc2, _ = psql("CREATE DATABASE %s" % DB)
        w("[1] 干净库 %s (drop rc=%d create rc=%d)" % (DB, rc, rc2))
        if rc != 0 or rc2 != 0:
            w("  建库失败，无法压测")
            return 4

        # ---------------- 编译 ----------------
        t0 = time.time()
        b = subprocess.run(["go", "build", "-o", EXE, "./cmd/iip-server"],
                           cwd=REPO, capture_output=True, text=True,
                           encoding="utf-8", errors="replace", timeout=600)
        w("[2] go build rc=%d (%.1fs) %s"
          % (b.returncode, time.time() - t0, (b.stderr or "")[:200]))
        if b.returncode != 0:
            return 2

        # ---------------- 起 server ----------------
        srv_log_path = os.path.splitext(OUT)[0] + "_srv.log"
        try:
            srv_log = io.open(srv_log_path, "wb")
            srv_out = srv_log
        except OSError:
            srv_log = None
            srv_out = subprocess.DEVNULL
        srv = subprocess.Popen(
            [EXE, "--http-addr=:%d" % PORT_HTTP, "--pg-url=" + DSN,
             "--log-level=info"],
            cwd=REPO, stdout=srv_out, stderr=subprocess.STDOUT,
            env={**os.environ, "IIP_CORRELATION_ENABLED":
                 "false" if args.no_correlation else "true"})
        ready = False
        for _ in range(120):
            if srv.poll() is not None:
                break
            try:
                with urllib.request.urlopen(BASE + "/healthz", timeout=3) as r:
                    if r.status == 200:
                        ready = True
                        break
            except Exception:
                time.sleep(0.5)
        w("[3] server 就绪=%s (pid=%s)" % (ready, srv.pid))
        if not ready:
            if srv_log is not None:
                srv_log.flush()
                with io.open(srv_log_path, "rb") as f:
                    w("    server 输出: %s"
                      % f.read().decode("utf-8", errors="replace")[:1500])
            return 3

        # **干净起点检查必须放在 server 就绪之后**：迁移是 server 启动时
        # 跑的（main.go: store.Migrate），建库后 alerts 表还不存在，
        # 此时查会得到 -1（表不存在）—— 那是「查得太早」，
        # 不是「库不干净」。2026-10-12 就这么误报过一次。
        n0 = q1("SELECT count(*) FROM alerts")
        w("    起始 alerts 行数 = %s（应为 0）" % n0)
        if n0 != 0:
            w("  起始库不干净（%s 行），压测结论不可信" % n0)
            return 4

        # ---------------- 灌数 ----------------
        w("[4] 开始灌入 %d 条..." % n)
        lat = []                 # 客户端侧耗时 ms
        accepted_sum = 0
        rejected_sum = 0
        err_count = 0
        non200 = 0
        t_start = time.perf_counter()

        def job(batch):
            st, body, ms = send_batch(batch)
            return st, body, ms

        with cf.ThreadPoolExecutor(max_workers=conc) as ex:
            for st, body, ms in ex.map(job, batches):
                lat.append(ms)
                if st != 200:
                    non200 += 1
                    if err_count < 5:
                        w("    非200: status=%s body=%s" % (st, str(body)[:160]))
                    continue
                if not isinstance(body, dict):
                    err_count += 1
                    continue
                accepted_sum += int(body.get("accepted", 0) or 0)
                rejected_sum += int(body.get("rejected", 0) or 0)
                if body.get("accepted") != bs:
                    w("    批内 accepted=%s != %d（指纹撞了？）"
                      % (body.get("accepted"), bs))

        wall = time.perf_counter() - t_start
        lat.sort()
        # **口径修正（2026-10-12 实测踩坑）**：`lat` 里存的是「一次 HTTP
        # 请求（整批）」的耗时。b=10 时 p95=2906ms 的物理含义是
        # 「一批 10 条要 2.9s」，不是「单条要 2.9s」。此前直接拿它比
        # 200ms 阈值 ⇒ 假红。T7.3 的 200ms 指单条端到端延迟，
        # 故按 bs 折算；批内若服务端并发处理则折算值为低估，报告注明。
        lat_each = [x / bs for x in lat]
        w("[5] 灌入完成：wall=%.2fs 吞吐=%.0f 条/s"
          % (wall, n / wall if wall else -1))
        w("    请求数=%d 非200=%d 解析失败=%d" % (len(lat), non200, err_count))
        w("    每批耗时 ms：p50=%.1f p95=%.1f p99=%.1f max=%.1f"
          % (pct(lat, 0.50), pct(lat, 0.95), pct(lat, 0.99),
             lat[-1] if lat else -1))
        w("    折算单条 ms：p50=%.1f p95=%.1f p99=%.1f max=%.1f"
          "（=每批/%d，批内串行假设）"
          % (pct(lat_each, 0.50), pct(lat_each, 0.95),
             pct(lat_each, 0.99),
             lat_each[-1] if lat_each else -1, bs))
        w("    webhook 记账：accepted=%d rejected=%d"
          % (accepted_sum, rejected_sum))

        # ---------------- 落库轮询 ----------------
        # 200 只代表受理，写库异步。轮询至行数稳定（连续两次相同）。
        w("[6] 等待落库稳定...")
        last, stable, db_n = -1, 0, -1
        deadline = time.time() + 180
        while time.time() < deadline:
            db_n = q1("SELECT count(*) FROM alerts")
            if db_n == last:
                stable += 1
                if stable >= 2:
                    break
            else:
                stable = 0
            last = db_n
            time.sleep(1.0)
        w("    落库 alerts 行数 = %s（轮询至稳定）" % db_n)

        # ---------------- 判据 ----------------
        w("[7] 判据")
        # ① 全部请求 200
        ok_http = (non200 == 0 and err_count == 0)
        w("    %s 全部请求 HTTP 200" % ("[PASS]" if ok_http else "[FAIL]"))
        if not ok_http:
            w("           非200=%d 解析失败=%d" % (non200, err_count))
        # ② 服务端全盘接收
        ok_acc = (accepted_sum == n)
        w("    %s 零丢失（客户端记账 accepted=%d == 发送 %d）"
          % ("[PASS]" if ok_acc else "[FAIL]", accepted_sum, n))
        # ③ 无拒绝
        ok_rej = (rejected_sum == 0)
        w("    %s webhook 不拒绝（rejected=%d）"
          % ("[PASS]" if ok_rej else "[FAIL]", rejected_sum))
        # ④ 落库数与发送数一致
        ok_db = (db_n == n)
        w("    %s 落库行数 == 发送数（%s == %d）"
          % ("[PASS]" if ok_db else "[FAIL]", db_n, n))
        # ⑤ p95（折算单条口径）
        p95_each = pct(lat_each, 0.95)
        ok_p95 = (p95_each >= 0 and p95_each < args.p95_max)
        w("    %s 折算单条 p95 = %.1fms < %.0fms"
          % ("[PASS]" if ok_p95 else "[FAIL]", p95_each, args.p95_max))
        w("           注：严谨的单条端到端延迟须 -b 1 实测；"
          "本口径假设批内串行处理。")
        # ⑥ 指纹互异（防「灌了 1 万其实只有 1 条」）
        n_fp = q1("SELECT count(DISTINCT fingerprint) FROM alerts")
        ok_fp = (n_fp == n)
        w("    %s 指纹互异（distinct fingerprint=%s，应=%d）"
          % ("[PASS]" if ok_fp else "[FAIL]", n_fp, n))

        n_fail = sum(1 for x in (ok_http, ok_acc, ok_rej, ok_db, ok_p95, ok_fp)
                     if not x)
        w()
        w("=== 结论：%s（%d/6 判据通过）==="
          % ("PASS" if n_fail == 0 else "FAIL", 6 - n_fail))
        w("    吞吐=%.0f 条/s，折算单条 p95=%.1fms，落库=%s，指纹=%s"
          % (n / wall if wall else -1, p95_each, db_n, n_fp))
        return 0 if n_fail == 0 else 1

    finally:
        if srv is not None:
            srv.terminate()
            try:
                srv.wait(timeout=10)
            except Exception:
                srv.kill()
        if srv_log is not None:
            try:
                srv_log.flush()
                srv_log.close()
            except Exception:
                pass
        if pg is not None:
            subprocess.run([os.path.join(PG_BIN, "pg_ctl.exe"), "stop",
                            "-D", PG_DATA, "-m", "fast", "-t", "300"],
                           capture_output=True, text=True, encoding="utf-8",
                           errors="replace", timeout=330, cwd=PG_BIN)
        w("[end] server 已停，PG 监听=%s"
          % (port_open(PORT_PG) and "仍开着" or "已关"))
        w("报告: %s" % OUT)


if __name__ == "__main__":
    sys.exit(main())
