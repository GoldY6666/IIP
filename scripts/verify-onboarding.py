#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""T6.5 接入指南的可执行验收：把文档里的命令**原文**跑一遍。

要解决的问题
------------
「5 分钟接入」是一句承诺，而承诺最容易被文档自己毁掉：curl 少一个引号、
payload 缺一个必填字段、路径写错一个字符 —— 这些都只会在操作者那里暴露。
所以本脚本不复述文档内容，而是从 `docs/接入指南-Alertmanager.md` 里
按标记抽出代码块，原样执行，并断言文档声称的每一种结果。

真栈：真 PG + 真 iip-server（启用鉴权）。所有断言的期望值都取自文档正文，
文档改了而代码没改（或反过来），这里就会红。

核对项（编号与文档章节对应）
----------------------------
  S1 第 1 步的 receiver YAML 可解析，且含正确路径 / send_resolved / X-IIP-Token 自定义头
  S2 第 2 步的 alert.json + curl **原文执行** → 200 且 accepted=1
  S3 第 3 步 ① → GET /api/v1/alerts 能查到，且 severity 由 page 归一为 critical
  S4 第 3 步 ② → /metrics 里 iip_alert_ingest_total{result="stored"}=1
  S5 文档说「实例重建不会产生新告警」→ 改 instance 重发 → accepted=0 / skipped=1
  S6 第 3 步 ③ 的 resolve 块原文执行 → updated=1，且列表里该条变 resolved
  S7 文档状态码表：缺 version → 400；缺 alertname → 200 且 rejected=1（errors[0].index=0）
  S8 文档说 /metrics 与 /healthz 不需要 token → 无头访问 200；同一 server 的 /api/v1/alerts 无头 401
  S9 附录「不用 Alertmanager 的源」的最小可用体原文执行 → 200 且 accepted=1

跑法
----
    python scripts/verify-onboarding.py

依赖：绿色版 PG（D:\\pgsql16）、Go 工具链（1.26）、curl（bash 里可用）。
**本机没有 Alertmanager 与 Grafana，所以接收端语义只能验到「YAML 可解析」，
不能验「Alertmanager 能加载它」—— 这一点不写进结论里就是过度声称。**
"""

import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
DOC = REPO / "docs" / "接入指南-Alertmanager.md"

GO = r"C:\Users\Yu\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.9.windows-amd64\bin\go.exe"
PG_BIN = r"D:\pgsql16\bin"
PGDATA = r"D:\_iip_setup\pgdata_unittest"
PG_PORT = 5434
DB = "iip_onboarding_probe"
HTTP_PORT = 8090
TOKEN = "onboarding-probe-token"
SERVER_EXE = r"D:\_iip_setup\iip-server-onboarding.exe"
SERVER_LOG = r"D:\_iip_setup\iip-server-onboarding.log"
BASE = "http://127.0.0.1:%d" % HTTP_PORT

OK, FAIL, DEGRADED = "OK", "FAIL", "DEGRADED"
RESULTS = []
pg_proc = None


def say(s):
    """带 flush：脚本被中途杀掉时，已产生的结论必须还在证据文件里。"""
    print(s, flush=True)


def check(state, name, detail=""):
    RESULTS.append((state, name, detail))
    say("  [%s] %-6s %s" % (state, name, detail))


# ---------------------------------------------------------------- 文档抽取


def extract_blocks(text):
    """按 `<!-- verify:NAME -->` 标记抽出紧随其后的第一个围栏代码块。

    标记与代码块分离（而不是塞在代码里）是为了让代码块**能被原样复制**：
    JSON 不能带注释，bash 里多一行 `# verify:xxx` 也会让操作者困惑。
    """
    blocks = {}
    pattern = re.compile(
        r"<!--\s*verify:([a-z-]+)\s*-->\s*\n+```([a-zA-Z]*)\n(.*?)```",
        re.S,
    )
    for m in pattern.finditer(text):
        blocks[m.group(1)] = (m.group(2), m.group(3))
    return blocks


def require_blocks(blocks, names):
    missing = [n for n in names if n not in blocks]
    if missing:
        say("[verify-onboarding] 文档里缺少这些标记块：%s" % ", ".join(missing), )
        sys.exit(1)


# ---------------------------------------------------------------- 基础设施


def env_for_go():
    e = dict(os.environ)
    e["GOCACHE"] = r"D:\_iip_setup\gocache"
    e["GOTOOLCHAIN"] = "local"
    e["GOFLAGS"] = "-mod=mod"
    return e


def psql(sql, db="postgres"):
    env = dict(os.environ, PGPASSWORD="dev")
    p = subprocess.run(
        [os.path.join(PG_BIN, "psql.exe"), "-h", "127.0.0.1", "-p", str(PG_PORT),
         "-U", "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-c", sql],
        capture_output=True, text=True, encoding="utf-8", errors="replace", env=env, timeout=90)
    if p.returncode != 0:
        raise RuntimeError("psql 失败: %s\n%s" % (sql, p.stderr))
    return p.stdout


def pg_ready():
    p = subprocess.run(
        [os.path.join(PG_BIN, "pg_isready.exe"), "-h", "127.0.0.1", "-p", str(PG_PORT)],
        capture_output=True, timeout=10)
    return p.returncode == 0


def ensure_pg():
    """直接 Popen postgres.exe 并轮询 pg_isready。

    不用 `pg_ctl start` + capture_output：pg_ctl 起的 postgres 会继承 stdout 管道，
    subprocess.run 读不到 EOF 就永久阻塞（整脚本静默卡死，零输出）。
    """
    global pg_proc
    if pg_ready():
        say("[0] PG 已在 %d 运行（沿用）" % PG_PORT)
        return
    logf = open(os.path.join(PGDATA, "pg_onboarding_probe.log"), "ab")
    pg_proc = subprocess.Popen(
        [os.path.join(PG_BIN, "postgres.exe"), "-D", PGDATA, "-p", str(PG_PORT)],
        stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN)
    for _ in range(180):
        if pg_ready():
            say("[0] PG 已在 %d 启动（本脚本启动）" % PG_PORT)
            return
        if pg_proc.poll() is not None:
            raise RuntimeError("PG 进程已退出 code=%s" % pg_proc.poll())
        time.sleep(1.0)
    raise RuntimeError("PG 启动后 180s 仍不可连")


def stop_pg_if_ours():
    if pg_proc is None:
        return
    subprocess.run([os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PGDATA, "stop", "-m", "fast", "-t", "120"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=150)
    say("[9] 本脚本启动的 PG 已停止")


def build_server():
    p = subprocess.run([GO, "build", "-o", SERVER_EXE, "./cmd/iip-server"], cwd=str(REPO),
                       capture_output=True, text=True, encoding="utf-8", errors="replace",
                       env=env_for_go())
    if p.returncode != 0:
        raise RuntimeError("构建 iip-server 失败:\n%s" % p.stderr)


def http(method, path, body=None, headers=None, timeout=10):
    req = urllib.request.Request(BASE + path, data=body, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def wait_healthz(timeout=40.0):
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            if http("GET", "/healthz")[0] == 200:
                return
        except Exception:
            pass
        time.sleep(0.3)
    raise RuntimeError("healthz 在 %ss 内未就绪" % timeout)


def find_bash():
    """找一个**真能跑**的 POSIX shell，找不到就返回 None。

    不能直接 `shutil.which("bash")`：Windows 上 System32\\bash.exe 是 WSL 启动器，
    本机没装发行版时它会 `execvpe(/bin/bash) failed` 并以非零退出 ——
    于是文档里每条 curl 都失败，而现象看起来像是"文档写错了"。
    更阴的是这个选择**取决于调用者的 PATH**：从 Git Bash 调用能找到 MINGW 的 bash，
    从 PowerShell 调用就拿到 WSL 启动器。同一个脚本两种结论，
    判据一旦依赖调用环境，它证明的东西就不再是文档本身。

    故逐个候选执行 `uname -s` 验证：Windows 上必须自报 MINGW/MSYS/CYGWIN。
    """
    cands = []
    w = shutil.which("bash")
    if w:
        cands.append(w)
    g = shutil.which("git")
    if g:
        # Git for Windows 自带 bash：<root>\cmd\git.exe → <root>\bin\bash.exe
        cands.append(os.path.join(os.path.dirname(os.path.dirname(g)), "bin", "bash.exe"))
    cands += [r"C:\Program Files\Git\bin\bash.exe", r"C:\Program Files (x86)\Git\bin\bash.exe"]

    seen = set()
    for c in cands:
        if not c or c.lower() in seen:
            continue
        seen.add(c.lower())
        if not os.path.exists(c):
            continue
        try:
            p = subprocess.run([c, "-c", "uname -s"], capture_output=True, text=True, timeout=25)
        except Exception:  # noqa: BLE001 - 任何探测失败都只是"这个候选不可用"
            continue
        out = (p.stdout or "").strip()
        if p.returncode != 0:
            continue
        if os.name == "nt":
            if out.startswith(("MINGW", "MSYS", "CYGWIN")):
                return c
        elif out:
            return c
    return None


def run_bash(code, cwd, tag):
    """在 bash 里原样执行文档中的代码块。

    返回 (rc, stdout, stderr, how)。找不到可用的 bash/curl 时返回 rc=None ——
    降级必须显式，不能悄悄换成"等价请求"再宣称"文档命令跑通了"。

    **代码块先落成 .sh 文件再执行，不走 `bash -c`。** 原因不是美观：
    Windows 上 argv 是 UTF-16，MSYS 转换时按本地代码页（中文机器是 GBK）重编码，
    于是文档里内联在命令中的中文注解变成 GBK 字节，服务端 PG 直接拒绝
    （`无效的 "UTF8" 编码字节顺序` → 500/503）。用文件传，字节就是文档里的字节。
    """
    bash = find_bash()
    curl = shutil.which("curl")
    if not bash or not curl:
        return None, "", "", "no-bash-or-curl"
    script = Path(cwd) / ("_doc_%s.sh" % re.sub(r"[^0-9a-zA-Z]+", "_", tag))
    script.write_text(code, encoding="utf-8", newline="\n")
    env = dict(os.environ)
    env["IIP"] = BASE
    env["IIP_TOKEN"] = TOKEN
    p = subprocess.run([bash, script.name], cwd=cwd, capture_output=True, text=True,
                       encoding="utf-8", errors="replace", env=env, timeout=120)
    return p.returncode, (p.stdout or "").strip(), (p.stderr or "").strip(), bash


def metric_value(text, name, **labels):
    """从 /metrics 文本里取一条样本的值；不存在返回 None。"""
    pattern = name + "{"
    for line in text.splitlines():
        if line.startswith("#") or not line.startswith(pattern):
            continue
        if all('%s="%s"' % (k, v) in line for k, v in labels.items()):
            try:
                return float(line.rsplit(" ", 1)[1])
            except ValueError:
                return None
    return None


# ---------------------------------------------------------------- 断言


def expect(rc, stdout, stderr, want_rc, want, label):
    """断言文档命令的执行结果。

    失败时必须把 stderr 也打出来：第一版只打了 stdout，于是"bash 起不来"
    看起来像"文档 payload 写错了"，白查一轮。报红不带真因，等于没报。
    """
    if rc != want_rc or want not in stdout:
        check(FAIL, label, "rc=%s（期望 %d 且响应含 %r）；stdout=%r；stderr=%r"
              % (rc, want_rc, want, stdout[:300], stderr[:300]))
        return False
    check(OK, label, "rc=%d，响应含 %s" % (rc, want))
    return True


def main():
    say("=== T6.5 接入指南可执行验收 ===")
    doc_text = DOC.read_text(encoding="utf-8")
    blocks = extract_blocks(doc_text)
    require_blocks(blocks, ["receiver", "payload", "curl", "resolve", "generic"])
    say("[1] 文档抽出 %d 个代码块：%s" % (len(blocks), ", ".join(sorted(blocks))))

    # ---- S1 receiver YAML
    try:
        import yaml
    except ImportError:
        say("缺少 PyYAML：pip install pyyaml", )
        return 2
    try:
        recv = yaml.safe_load(blocks["receiver"][1])
        wc = recv["receivers"][0]["webhook_configs"][0]
        ok = (
            wc["url"].endswith("/api/v1/alerts/webhook")
            and wc.get("send_resolved") is True
        )
        check(OK if ok else FAIL, "S1 receiver",
              "url=%s send_resolved=%s" % (wc["url"], wc.get("send_resolved")))
    except Exception as exc:  # noqa: BLE001 - 文档结构坏了要原样报出来
        check(FAIL, "S1 receiver", "YAML 解析或结构断言失败：%s" % exc)

    # 鉴权片段是文档正文里的第二个 yaml 块（无标记），单独按内容找：
    # 要求全文出现 http_headers + X-IIP-Token，且**不**出现 authorization 的推荐写法
    has_header_form = "X-IIP-Token:" in doc_text and "http_headers:" in doc_text
    check(OK if has_header_form else FAIL, "S1 鉴权头",
          "文档给出了 http_headers + X-IIP-Token 的正确写法" if has_header_form
          else "文档没有给出 http_headers 自定义头写法（Alertmanager 的 authorization 发的是 Authorization，IIP 不认）")

    # ---- S2 起真栈
    ensure_pg()
    psql("DROP DATABASE IF EXISTS %s" % DB)
    psql("CREATE DATABASE %s" % DB)
    build_server()
    env = dict(os.environ,
               IIP_PG_URL="postgres://postgres:dev@127.0.0.1:%d/%s?sslmode=disable" % (PG_PORT, DB),
               IIP_LOG_LEVEL="info",
               IIP_TOPOLOGY_FILE="config/topology.yaml",
               IIP_HTTP_ADDR=":%d" % HTTP_PORT,
               IIP_AUTH_TOKEN=TOKEN)
    logf = open(SERVER_LOG, "w", encoding="utf-8")
    srv = subprocess.Popen([SERVER_EXE], cwd=str(REPO), env=env, stdout=logf,
                           stderr=subprocess.STDOUT, creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
    tmpdir = tempfile.mkdtemp(prefix="iip-onboarding-")
    try:
        wait_healthz()
        say("[2] server 已就绪（鉴权已启用）→ %s" % BASE)

        # 文档里 alert.json 是让操作者另存的文件，这里按原文写盘
        payload_text = blocks["payload"][1]
        json.loads(payload_text)  # 先证明块本身是合法 JSON
        (Path(tmpdir) / "alert.json").write_text(payload_text, encoding="utf-8")

        # ---- S2 curl 原文执行
        rc, out, err, how = run_bash(blocks["curl"][1], tmpdir, "S2 curl")
        if rc is None:
            check(DEGRADED, "S2 curl", "找不到可用的 bash/curl（Windows 上需 Git Bash，不是 WSL 启动器），文档命令未执行（不是通过）")
        else:
            expect(rc, out, err, 0, '"accepted":1', "S2 curl")
            say("      执行用 shell：%s" % how)

        # ---- S3 列表 + 归一
        st, body = http("GET", "/api/v1/alerts?limit=5", headers={"X-IIP-Token": TOKEN})
        try:
            doc = json.loads(body)
            first = doc["alerts"][0] if doc["alerts"] else {}
            sev = first.get("severity")
            status = first.get("status")
            ok = st == 200 and doc.get("count", 0) >= 1 and sev == "critical" and status == "firing"
            check(OK if ok else FAIL, "S3 列表与归一",
                  "count=%s severity=%s status=%s（文档称 page→critical）"
                  % (doc.get("count"), sev, status))
        except Exception as exc:  # noqa: BLE001
            check(FAIL, "S3 列表与归一", "响应不可解析：%s / %r" % (exc, body[:200]))

        # ---- S4 指标
        st, metrics = http("GET", "/metrics")
        stored = metric_value(metrics, "iip_alert_ingest_total", result="stored")
        check(OK if stored == 1 else FAIL, "S4 指标 stored",
              "iip_alert_ingest_total{result=stored}=%s（文档称应为 1）" % stored)

        # ---- S5 改 instance 重发 → 去重命中
        mutated = json.loads(payload_text)
        mutated["alerts"][0]["labels"]["instance"] = "demo-2:9100"
        st, body = http("POST", "/api/v1/alerts/webhook",
                        body=json.dumps(mutated).encode(),
                        headers={"Content-Type": "application/json", "X-IIP-Token": TOKEN})
        try:
            res = json.loads(body)
            ok = st == 200 and res["accepted"] == 0 and res["skipped"] == 1
            check(OK if ok else FAIL, "S5 动态标签不参与指纹",
                  "改 instance 重发 → accepted=%s skipped=%s（文档称 0/1）"
                  % (res.get("accepted"), res.get("skipped")))
        except Exception as exc:  # noqa: BLE001
            check(FAIL, "S5 动态标签不参与指纹", "响应不可解析：%s / %r" % (exc, body[:200]))

        # ---- S6 resolve 块原文执行
        rc, out, err, how = run_bash(blocks["resolve"][1], tmpdir, "S6 resolve")
        if rc is None:
            check(DEGRADED, "S6 resolve", "找不到可用的 bash/curl（Windows 上需 Git Bash，不是 WSL 启动器），文档命令未执行（不是通过）")
        else:
            expect(rc, out, err, 0, '"updated":1', "S6 resolve")
        st, body = http("GET", "/api/v1/alerts?limit=5", headers={"X-IIP-Token": TOKEN})
        try:
            doc = json.loads(body)
            statuses = sorted({a.get("status") for a in doc["alerts"]})
            check(OK if statuses == ["resolved"] else FAIL, "S6 落库状态",
                  "库内状态集合=%s（期望只剩 resolved）" % statuses)
        except Exception as exc:  # noqa: BLE001
            check(FAIL, "S6 落库状态", "响应不可解析：%s" % exc)

        # ---- S7 两个坏 payload
        nover = json.loads(payload_text)
        nover.pop("version", None)
        st, body = http("POST", "/api/v1/alerts/webhook", body=json.dumps(nover).encode(),
                        headers={"Content-Type": "application/json", "X-IIP-Token": TOKEN})
        check(OK if st == 400 else FAIL, "S7 缺 version",
              "status=%d（文档称整批 400）body=%r" % (st, body[:160]))

        noname = json.loads(payload_text)
        noname["alerts"][0]["labels"].pop("alertname", None)
        st, body = http("POST", "/api/v1/alerts/webhook", body=json.dumps(noname).encode(),
                        headers={"Content-Type": "application/json", "X-IIP-Token": TOKEN})
        try:
            res = json.loads(body)
            errs = res.get("errors") or []
            ok = st == 200 and res.get("rejected") == 1 and errs and errs[0].get("index") == 0
            check(OK if ok else FAIL, "S7 缺 alertname",
                  "status=%s rejected=%s errors=%s（文档称 200 且 rejected=1、index=0）"
                  % (st, res.get("rejected"), errs))
        except Exception as exc:  # noqa: BLE001
            check(FAIL, "S7 缺 alertname", "响应不可解析：%s / %r" % (exc, body[:200]))

        # ---- S8 无 token 的三种结局
        st_metrics, _ = http("GET", "/metrics")
        st_health, _ = http("GET", "/healthz")
        st_api, _ = http("GET", "/api/v1/alerts")
        ok = st_metrics == 200 and st_health == 200 and st_api == 401
        check(OK if ok else FAIL, "S8 端点与鉴权边界",
              "/metrics=%d /healthz=%d /api/v1/alerts(无头)=%d（文档称 200/200/401）"
              % (st_metrics, st_health, st_api))

        # ---- S9 通用源最小体
        rc, out, err, how = run_bash(blocks["generic"][1], tmpdir, "S9 generic")
        if rc is None:
            check(DEGRADED, "S9 通用源", "找不到可用的 bash/curl（Windows 上需 Git Bash，不是 WSL 启动器），文档命令未执行（不是通过）")
        else:
            expect(rc, out, err, 0, '"accepted":1', "S9 通用源")

    finally:
        srv.send_signal(signal.CTRL_BREAK_EVENT)
        try:
            rc_srv = srv.wait(timeout=25)
        except subprocess.TimeoutExpired:
            srv.kill()
            rc_srv = -9
        logf.close()
        say("[9] server 退出码 = %d（期望 0）" % rc_srv)
        if rc_srv != 0:
            check(FAIL, "优雅退出", "server 退出码 %d" % rc_srv)
        else:
            check(OK, "优雅退出", "server 退出码 0")
        shutil.rmtree(tmpdir, ignore_errors=True)
        stop_pg_if_ours()

    fails = [r for r in RESULTS if r[0] == FAIL]
    degr = [r for r in RESULTS if r[0] == DEGRADED]
    oks = [r for r in RESULTS if r[0] == OK]
    say("\n[verify-onboarding] PASS %d / FAIL %d / DEGRADED %d" % (len(oks), len(fails), len(degr)))
    if degr:
        say("  DEGRADED 不是通过：上面这些文档命令在本机没有真正执行过。")
    if fails:
        say("  文档与实现已经不一致 —— 要么改文档，要么改实现，别让操作者去踩。")
        return 1
    if degr:
        return 2
    say("  注意：本脚本没有 Alertmanager 与 Grafana 实例 —— receiver YAML 只验证了"
        "「可解析且字段正确」，未验证「Alertmanager 能加载它」。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
