#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""一次性脚本：起真库 PG -> 跑 go test -p 1 -> 收 PG。

**为什么要这个脚本**（一条 go test 命令解决不了）：
PG 必须与测试**在同一个进程树**里存活。上一轮用 PowerShell `Start-Process`
起 PG、下一次工具调用再跑 test，PG 随父会话结束被回收 ⇒ 全部真库用例报
  `dial tcp 127.0.0.1:5434: connectex: No connection could be made`
看着像产品缺陷，实为进程生命周期问题。

本轮踩到并修正的两个坑（都写进纪律）：
1. **`-k` 不能传**：`-k` 是 unix_socket_directories，Windows 上会把
   `127.0.0.1` 当成**目录路径** ⇒ PG 秒退并报
   「无法创建锁文件 "127.0.0.1/.s.PGSQL.5434.lock"」。
   正确参数照 scripts/verify-func.py：只传 `-D` 与 `-p`，且 `cwd=PG_BIN`。
2. **进程秒退要立即放弃**：上一轮 PG 秒退后仍空等 300 秒才放弃，白等 5 分钟。
   判据 = `pg.poll() is not None`。

其余纪律（沿用项目记忆）：
- 前置必须 **assert**（go 工具链 / PGDATA 已初始化 / PG 二进制在位），
  别让半成品 PGDATA 走到最后才炸。
- **IIP_TEST_PG_URL 必须带 sslmode=disable**，否则全体报
  「SSL is not enabled」，症状伪装成「每个 schema 迁移失败」。
- `-p 1` 不可省：并行多包共用同一 iip_testsession 库 CREATE SCHEMA，整包全挂。
- go 构建缓存必须挪出 AppData（沙箱拒写）⇒ GOCACHE。
- 停完**以端口实际状态**为准，不以 pg_ctl 的 rc 为准。
- 全程落盘：异常也不能吞掉日志。
"""

import io
import os
import socket
import subprocess
import sys
import tempfile
import time

# ---- 路径：全部可用环境变量覆盖，默认值只对本机成立--------------------
# 与 scripts/verify-func.py 同一约定（IIP_PG_HOME 等），别人克隆即可改。
PG_HOME = os.environ.get("IIP_PG_HOME", r"D:\pgsql16")
PG_BIN = os.path.join(PG_HOME, "bin")
# **独立 PGDATA**：不与 verify-func.py 的 D:\pgsql16\data 共用——
# 两者会抢同一端口，且共用 PGDATA 时并发跑会互相把对方的 schema 建掉。
PGDATA = os.environ.get("IIP_PGTEST_DATA", r"D:\_iip_setup\pgdata_unittest")
PORT = int(os.environ.get("IIP_PGTEST_PORT", "5434"))
LOG = os.path.join(PGDATA, "pg_gotest.log")
# 报告落在 PGDATA 所在目录的父级，避免写进 git 工作树。
OUT = os.environ.get(
    "IIP_PGTEST_OUT", os.path.join(os.path.dirname(PGDATA), "_pgtest_final.txt")
)

# go 工具链：优先 IIP_GO_BIN；未设时在 modcache 里找已缓存的 1.26.x。
# **为什么要显式指定**：go.mod 的 toolchain 指令会触发自动下载，
# 而本机下载会被 SIGTERM 掐断（表现为测试「莫名其妙」不跑）。
TC = os.environ.get("IIP_GO_BIN", "")
if not TC:
    _mod = os.path.expanduser(
        r"~\go\pkg\mod\golang.org"
    )
    _cands = []
    if os.path.isdir(_mod):
        for _d in os.listdir(_mod):
            if _d.startswith("toolchain@v0.0.1-go1.26"):
                _cands.append(os.path.join(_mod, _d, "bin"))
    _cands.append(r"C:\Program Files\Go\bin")
    _cands.append(os.path.expanduser(r"~\go\bin"))
    TC = next(
        (c for c in _cands if os.path.isfile(os.path.join(c, "go.exe"))),
        _cands[-1],
    )
GO_BIN = os.path.join(TC, "go.exe")

# 仓库根 = 本脚本的上级目录，不硬编码绝对路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.environ.get("IIP_REPO", os.path.dirname(_HERE))
# 构建缓存：沙箱拒写 AppData\Local\go-build，故默认指到临时目录。
GOCACHE = os.environ.get("IIP_GOCACHE", os.path.join(tempfile.gettempdir(), "iip_gocache"))

L = []


def w(s):
    L.append(str(s))
    print(s, flush=True)


def port_open(p):
    """真实 TCP 探测。不用 socket.connect_ex —— 它在 Windows 上
    恒返 WSAEWOULDBLOCK（10035），会把「未监听」误报成「已连上」。"""
    s = socket.socket()
    s.settimeout(1.0)
    try:
        s.connect(("127.0.0.1", p))
        return True
    except OSError:
        return False
    finally:
        s.close()


def pg_ready():
    ir = subprocess.run(
        [os.path.join(PG_BIN, "pg_isready.exe"), "-h", "127.0.0.1", "-p", str(PORT)],
        capture_output=True, timeout=10,
    )
    return ir.returncode == 0


def main():
    # ---------- 前置断言（不 assert 就往下走 = 把问题推到最难读的地方）----------
    assert os.path.isfile(GO_BIN), "go1.26.9 工具链不存在: %s" % GO_BIN
    assert os.path.isfile(os.path.join(PGDATA, "PG_VERSION")), \
        "PGDATA 未初始化: %s" % PGDATA
    assert os.path.isfile(os.path.join(PGDATA, "postgresql.conf")), \
        "PGDATA 缺 postgresql.conf（initdb 半成品目录）"
    assert os.path.isfile(os.path.join(PG_BIN, "postgres.exe")), \
        "PG 二进制缺失: %s" % os.path.join(PG_BIN, "postgres.exe")
    w("[pre] 前置断言全通过（go1.26.9 / PGDATA 已初始化 / PG 二进制在位）")

    if port_open(PORT):
        w("[pre] 端口 %d 已被占用，先停残留 PG" % PORT)
        subprocess.run(
            [os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PGDATA,
             "stop", "-m", "immediate", "-t", "60"],
            capture_output=True, timeout=90,
        )
        time.sleep(2.0)
        w("[pre] 停后端口仍开 = %s" % port_open(PORT))

    pidf = os.path.join(PGDATA, "postmaster.pid")
    if os.path.isfile(pidf):
        os.remove(pidf)
        w("[pre] 已删残留 postmaster.pid")

    pg = None
    try:
        # ---------- 起 PG：参数照verify-func.py（**不传 -k**）----------
        logf = open(LOG, "ab")
        pg = subprocess.Popen(
            [os.path.join(PG_BIN, "postgres.exe"), "-D", PGDATA, "-p", str(PORT)],
            stdout=logf, stderr=subprocess.STDOUT, cwd=PG_BIN,
        )
        w("[1] postgres pid=%d 已拉起" % pg.pid)

        ready = False
        t0 = time.time()
        for _ in range(180):
            if pg_ready():
                ready = True
                w("[1] PG 就绪（%.1fs）" % (time.time() - t0))
                break
            if pg.poll() is not None:
                w("[1] !! PG 进程已退出 code=%s —— 立即放弃，不空等" % pg.poll())
                break
            time.sleep(1.0)
        if not ready:
            w("[1] PG 日志尾：")
            try:
                with open(LOG, "rb") as f:
                    w(f.read().decode("utf-8", "replace")[-1500:])
            except OSError as e:
                w("读日志失败: %s" % e)
            return 2

        # ---------- 跑真库测试 ----------
        env = dict(os.environ)
        env["PATH"] = TC + ";" + env.get("PATH", "")
        env["GOTOOLCHAIN"] = "local"
        env["GOCACHE"] = GOCACHE
        env["GOTELEMETRY"] = "off"
        env["GOFLAGS"] = "-mod=mod"
        env["IIP_TEST_PG_URL"] = (
            "postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable" % PORT
        )
        w("[2] IIP_TEST_PG_URL 已设（含 sslmode=disable）")
        # 追加命令行参数（如 -v 用于统计 SKIP 数）。
        # 理由：非 verbose 的 go test 输出**看不到 SKIP**，
        # 而 M6 DoD 要求真库 SKIP=0 ⇒ 必须 -v 才能核。
        extra = sys.argv[1:]
        w("[2] 开始 go test -p 1 -count=1 ./... %s （-p 1 不可省）" % " ".join(extra))

        t1 = time.time()
        r = subprocess.run(
            [GO_BIN, "test", "-p", "1", "-count=1"] + extra + ["./..."],
            cwd=REPO, capture_output=True, text=True,
            encoding="utf-8", errors="replace", timeout=3000, env=env,
        )
        el = time.time() - t1
        out = (r.stdout or "") + (r.stderr or "")
        w("[3] go test exit=%d elapsed=%.1fs" % (r.returncode, el))
        w(out)
        return r.returncode
    finally:
        # ---------- 收 PG（以端口状态为准）----------
        if pg is not None:
            try:
                subprocess.run(
                    [os.path.join(PG_BIN, "pg_ctl.exe"), "-D", PGDATA,
                     "stop", "-m", "fast", "-t", "300"],
                    capture_output=True, timeout=330,
                )
            except subprocess.TimeoutExpired:
                w("[end] pg_ctl stop 超时（不代表库没停，以端口为准）")
        time.sleep(2.0)
        w("[end] PG 端口 %d 仍在监听 = %s（False = 已停干净）"
          % (PORT, port_open(PORT)))


if __name__ == "__main__":
    try:
        rc = main()
    except Exception as e:  # noqa: BLE001 —— 落盘优先，别让异常吞掉日志
        w("[FATAL] %r" % e)
        rc = 3
    finally:
        io.open(OUT, "w", encoding="utf-8", newline="\n").write("\n".join(L))
    sys.exit(rc)