#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""T7.7 E2E 回归编排器 —— M1-M6 验收项 + MVP 总验收一键化。

**这个脚本是什么**：把仓库里已经存在的验证资产（6 条链路 + 2 类测试）
编排成一条命令可跑的回归，并按开发计划的 M1-M6 验收项 / MVP 总验收
逐项给出状态。它**不重复实现任何判据**——每个阶段的判据仍归各脚本
所有（verify-func 的 90 条、fault_drill 的 33 条……），本脚本只负责：
起停编排、端口防抢占、结果汇总、验收项映射、诚实的 BLOCKED 语义。

**为什么必须分批跑**（`--stages` 的由来）：
沙箱/工具调用有 ~10 分钟前台上限，全量 E2E 约 15-20 分钟。因此每个
阶段的结果独立落盘 `scripts/_e2e/<id>.json`，可以分多次调用跑完，
最后 `--summary` 汇总。在无此限制的机器（CI / 禹本机）上直接
`python scripts/verify-e2e.py` 一把跑完。

**端口纪律**：5434 被三条链共用（run_pgtests / verify-func / playwright），
8083 被两条链共用（fault_drill / ingest_bench）——所以本编排器
**只串行**，不并行；每阶段开跑前重新检查端口（上一阶段崩溃残留会
在这里被可读地拦住，而不是让下一阶段跑到一半才炸）。

**退出码语义**（默认宽松 + --strict 严格）：
  0 = 所选阶段全部 PASS（或 BLOCKED 但非 strict）
  1 = 任一阶段 FAIL；strict 下 BLOCKED 也算
  2 = 用法/内部错误（未知阶段名、没有任何阶段结果等）
BLOCKED ≠ PASS：环境缺条件（daemon 不在、node 不在）时阶段记
BLOCKED，汇总里大字警告「MVP 未完整验收」，绝不冒充通过。

**判据自证**（本脚本自己的门禁，不是产品的）：
  ① 变异 A：把任一阶段的命令换成必然失败的替身 -> 汇总必须 FAIL+rc=1；
  ② 变异 B：把 values.yaml 的 tag 改成 0.9.9 -> S1 标签四联动必须红
     （证明真的在读文件，不是恒绿）；
  ③ BLOCKED 实证：daemon 缺席时 S7 必须报 BLOCKED 而非 PASS/FAIL，
     --strict 下 rc=1；
  ④ --summary 在没有某阶段 JSON 时记 MISSING，不假装 PASS；
  ⑤ 端口占位：占住 5434 时对应阶段必须给出可读错误（见上）。
"""

import argparse
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time

_HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(_HERE)
E2E_DIR = os.path.join(_HERE, "_e2e")           # 阶段 JSON（gitignore 覆盖）
REPORT = os.path.join(_HERE, "_verify_e2e.txt")  # 汇总报告（_*.txt 已覆盖）

# 各阶段端口（开跑前逐一探测；被占 -> 该阶段 FAIL 并给出占用提示）
STAGE_PORTS = {
    "s2": [5434],
    "s3": [5434, 8082],
    "s4": [5434, 8087],
    "s5": [5436, 8083],
    "s6": [5435, 8083],
    "s7": [5432, 8080],
}

# ---------------------------------------------------------------- 工具发现
# 沙箱会清空 PATH：所有外部工具都必须能用绝对路径找到，
# 找不到时给可读的 BLOCKED/FAIL，不让 FileNotFoundError 伪装成产品缺陷。

_GO_CACHE = None


def find_go():
    """go 工具链：env > 已知 toolchain 缓存 glob > PATH。"""
    global _GO_CACHE
    if _GO_CACHE is not None:
        return _GO_CACHE
    cand = os.environ.get("IIP_GO_BIN")
    if cand and os.path.isfile(cand):
        _GO_CACHE = cand
        return cand
    home = os.environ.get("USERPROFILE") or os.path.expanduser("~")
    base = os.path.join(home, "go", "pkg", "mod", "golang.org")
    if os.path.isdir(base):
        for name in sorted(os.listdir(base), reverse=True):
            if name.startswith("toolchain@v0.0.1-go"):
                p = os.path.join(base, name, "bin", "go.exe")
                if os.path.isfile(p):
                    _GO_CACHE = p
                    return p
    _GO_CACHE = shutil.which("go")
    return _GO_CACHE


def find_tool(env_key, rel_candidates, which_name):
    """通用工具发现：env 绝对路径 > 仓库外已知相对路径 > PATH。"""
    cand = os.environ.get(env_key)
    if cand and os.path.isfile(cand):
        return cand
    for rel in rel_candidates:
        p = os.path.join(REPO, rel)
        if os.path.isfile(p):
            return p
    return shutil.which(which_name)


def find_helm():
    """helm：env > 探针机约定绝对路径（D:\\_iip_setup）> 仓库外相对路径 > PATH。"""
    cands = [os.environ.get("IIP_HELM_BIN"),
             r"D:\_iip_setup\bin\windows-amd64\helm.exe",
             os.path.join(REPO, "..", "..", "_iip_setup", "bin",
                          "windows-amd64", "helm.exe")]
    for c in cands:
        if c and os.path.isfile(c):
            return c
    return shutil.which("helm")


def find_docker():
    cands = [os.environ.get("IIP_DOCKER_BIN"),
             r"D:\Program Files\Docker\Docker\resources\bin\docker.exe",
             r"C:\Program Files\Docker\Docker\resources\bin\docker.exe"]
    for c in cands:
        if c and os.path.isfile(c):
            return c
    return shutil.which("docker")


def find_pkgmgr():
    """前端包管理器：pnpm 优先（仓库有 pnpm-lock.yaml），npm 兜底。"""
    for name in ("pnpm", "npm"):
        p = shutil.which(name)
        if p:
            return name, p
        # npm 随 Node 安装，可能在 Program Files
        for c in (r"C:\Program Files\nodejs\%s.cmd" % name,
                  os.path.join(os.environ.get("APPDATA", ""), "npm",
                               "%s.cmd" % name)):
            if os.path.isfile(c):
                return name, c
    return None, None


def find_chrome():
    cands = [os.environ.get("IIP_CHROME_BIN"),
             r"C:\Program Files\Google\Chrome\Application\chrome.exe",
             r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe"]
    for c in cands:
        if c and os.path.isfile(c):
            return c
    return None


def port_free(port):
    s = socket.socket()
    s.settimeout(0.3)
    try:
        return s.connect_ex(("127.0.0.1", int(port))) != 0
    finally:
        s.close()


def run_cmd(cmd, cwd=REPO, timeout=120, env_extra=None):
    """跑一个短命令，返回 (rc, 合并输出)。输出按 UTF-8 显式解码。"""
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    try:
        r = subprocess.run(cmd, cwd=cwd, capture_output=True, timeout=timeout,
                           env=env, text=True, encoding="utf-8",
                           errors="replace")
        return r.returncode, (r.stdout or "") + (r.stderr or "")
    except subprocess.TimeoutExpired:
        return 124, "timeout after %ss" % timeout
    except FileNotFoundError:
        return 127, "command not found: %s" % cmd[0]


# ---------------------------------------------------------------- 结果落盘

def stage_json_path(sid):
    return os.path.join(E2E_DIR, "%s.json" % sid)


def write_stage_json(sid, title, status, rc, elapsed, detail,
                     subchecks=None, hint=""):
    os.makedirs(E2E_DIR, exist_ok=True)
    data = {
        "id": sid, "title": title, "status": status, "rc": rc,
        "elapsed": round(elapsed, 1), "detail": detail,
        "subchecks": subchecks or [], "hint": hint,
        "at": time.strftime("%Y-%m-%d %H:%M:%S"),
    }
    with open(stage_json_path(sid), "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
    return data


def extract_counts(report_path):
    """从阶段报告里尽力提取判据计数（[PASS]/[FAIL] 行或 PASS=N FAIL=M）。

    提取不到返回 None——汇总时显示 '—'，不用 0 冒充（0 和未知是两件事）。
    """
    if not report_path or not os.path.isfile(report_path):
        return None
    try:
        with open(report_path, "r", encoding="utf-8", errors="replace") as f:
            text = f.read()
    except OSError:
        return None
    n_pass = len(re.findall(r"\[PASS\]", text))
    n_fail = len(re.findall(r"\[FAIL\]", text))
    n_skip = len(re.findall(r"\[SKIP\]", text))
    m = re.search(r"PASS=(\d+)\s+FAIL=(\d+)\s+SKIP=(\d+)", text)
    if m:
        return (int(m.group(1)), int(m.group(2)), int(m.group(3)))
    m = re.search(r"(\d+)\s*/\s*(\d+)\s*判据", text)
    if m:
        return (int(m.group(1)), int(m.group(2)) - int(m.group(1)), 0)
    if n_pass or n_fail:
        return (n_pass, n_fail, n_skip)
    # go test 输出：ok 行 = 通过的包，--- FAIL / FAIL 行 = 失败的包
    n_ok = len(re.findall(r"^ok\s+\S", text, re.M))
    n_tf = len(re.findall(r"^--- FAIL", text, re.M))
    n_pf = len(re.findall(r"^FAIL\s+\S", text, re.M))
    n_sk = len(re.findall(r"^--- SKIP", text, re.M))
    if n_ok or n_tf or n_pf:
        return (n_ok, n_tf + n_pf, n_sk)
    return None


def kill_tree(pid):
    """Windows 上没有 killpg：taskkill /T 杀整树，防子进程（PG/server）残留。"""
    if os.name == "nt":
        subprocess.run(["taskkill", "/F", "/T", "/PID", str(pid)],
                       capture_output=True)
    else:
        try:
            os.killpg(os.getpgid(pid), 9)
        except (ProcessLookupError, PermissionError):
            pass


def run_script_stage(sid, title, cmd, timeout, ports, rc_map,
                     report, env_extra=None):
    """通用「跑一个既有验证脚本」的阶段。

    rc_map: dict rc -> status（PASS/FAIL/BLOCKED）；未列出的 rc 一律 FAIL。
    端口被占 -> FAIL（可读错误，不让脚本跑到一半才炸）。
    """
    t0 = time.time()
    for p in ports:
        if not port_free(p):
            return write_stage_json(
                sid, title, "FAIL", None, time.time() - t0,
                "端口 %d 被占用" % p,
                hint="上一阶段崩溃残留？netstat -ano | findstr %d 找到进程，"
                     "或重跑本阶段前先确认无 iip 相关进程" % p)
    log_path = os.path.join(E2E_DIR, "%s.log" % sid)
    os.makedirs(E2E_DIR, exist_ok=True)
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    # 输出落文件不用 PIPE：管道满会阻塞子进程的 slog，把服务「假死」——
    # 这条教训来自 ingest_bench 的踩坑记录。
    with open(log_path, "w", encoding="utf-8", errors="replace") as log:
        proc = subprocess.Popen(cmd, cwd=REPO, stdout=log, stderr=log,
                                env=env)
        try:
            proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            kill_tree(proc.pid)
            return write_stage_json(
                sid, title, "FAIL", None, time.time() - t0,
                "超时 %ss（阶段挂住=缺陷，不判 BLOCKED）" % timeout,
                hint="日志 %s；若是产品挂死，这是真红" % log_path)
    rc = proc.returncode
    status = rc_map.get(rc, "FAIL")
    counts = extract_counts(report)
    detail = "rc=%s" % rc
    if counts:
        detail += "，判据 %d PASS / %d FAIL / %d SKIP" % counts
    hint = ""
    if status == "BLOCKED":
        hint = ("环境缺条件（详见报告）；条件满足后单独重跑："
                "python scripts/verify-e2e.py --stages %s" % sid)
    return write_stage_json(sid, title, status, rc, time.time() - t0,
                            detail, hint=hint)


# ---------------------------------------------------------------- S0 前置

def stage_s0():
    sid = "s0"
    t0 = time.time()
    title = "S0 前置：工具链 / 端口 / 仓库状态"
    subs = []
    fails = []

    def sub(name, ok, detail, warn_only=False):
        subs.append({"name": name,
                     "status": "pass" if ok else ("warn" if warn_only else "fail"),
                     "detail": detail})
        if not ok and not warn_only:
            fails.append(name)

    go = find_go()
    sub("go 工具链", bool(go), go or "找不到（大部分阶段需要它）")
    helm = find_helm()
    sub("helm", bool(helm), helm or "找不到（S1 的 helm 子检查将 BLOCKED）",
        warn_only=True)
    docker = find_docker()
    sub("docker CLI", bool(docker), docker or "找不到（S7 将 BLOCKED）",
        warn_only=True)
    pkg, pkg_path = find_pkgmgr()
    sub("前端包管理器", bool(pkg),
        "%s (%s)" % (pkg, pkg_path) if pkg
        else "pnpm/npm 都找不到（S4 将 BLOCKED）", warn_only=True)
    chrome = find_chrome()
    sub("Chrome（playwright 用）", bool(chrome),
        chrome or "找不到（S4 将 BLOCKED）", warn_only=True)
    sub("PG 绿色版 bin", os.path.isdir(r"D:\pgsql16\bin"),
        r"D:\pgsql16\bin（可用 IIP_PG_HOME 覆盖）", warn_only=True)
    sub("python >= 3.9", sys.version_info >= (3, 9), sys.version.split()[0])

    busy = [p for p in sorted(set(sum(STAGE_PORTS.values(), [])))
            if not port_free(p)]
    sub("E2E 端口当前空闲", not busy,
        "全部空闲" if not busy else "被占：%s（阶段开跑前会再查一次）"
        % ", ".join(str(b) for b in busy), warn_only=True)

    rc, out = run_cmd(["git", "describe", "--tags", "--abbrev=0"], timeout=30)
    tag = out.strip() if rc == 0 else ""
    sub("git tag（最新）", bool(tag), tag or "无 tag 或非 git 仓库",
        warn_only=True)
    rc, out = run_cmd(["git", "status", "--porcelain"], timeout=30)
    dirty = [l for l in out.splitlines() if l.strip()]
    sub("工作树状态", True,
        "干净" if not dirty else "%d 个未提交项（不影响 E2E，"
        "但 S1 克隆构建只认已提交状态）" % len(dirty), warn_only=True)

    status = "FAIL" if fails else "PASS"
    detail = "；".join(s["name"] + "=" + s["status"] for s in subs)
    hint = "" if not fails else "缺：%s" % "、".join(fails)
    return write_stage_json(sid, title, status, None, time.time() - t0,
                            detail, subchecks=subs, hint=hint)


# ---------------------------------------------------------------- S1 静态

# 标签四联动的全部位置（T7.6 对齐过的 9 处 + compose 内 3 处引用 +
# deploy/README 的构建示例）。每处必须「恰好匹配一次」——0 次=定义被删，
# 多次=出现歧义，两者都是 FAIL 而不是跳过。
TAG_LOCS = [
    ("Makefile", r'^IMAGE_TAG\s*\?=\s*(\S+)'),
    ("deploy/docker-compose.yml", r'^x-app-image:.*iip-server:(\S+)'),
    ("deploy/docker-compose.yml", r'^\s+image:\s*iip-worker:(\S+)'),
    ("deploy/docker-compose.yml", r'--target server -t iip-server:(\S+)'),
    ("deploy/docker-compose.yml", r'--target worker -t iip-worker:(\S+)'),
    ("deploy/README.md", r'--target server -t iip-server:(\S+)'),
    ("deploy/README.md", r'--target worker -t iip-worker:(\S+)'),
    ("deploy/helm/iip/Chart.yaml", r'^appVersion:\s*"?([^"\s]+)"?'),
    ("deploy/helm/iip/Chart.yaml", r'^version:\s*(\S+)'),
    ("deploy/helm/iip/values.yaml", r'^\s+tag:\s*"?([^"\s]+)"?'),
    ("scripts/verify-images.py",
     r'os\.environ\.get\("IIP_TAG",\s*"([^"]+)"\)'),
    ("scripts/check-binaries.py",
     r'os\.environ\.get\("IIP_VERSION",\s*"([^"]+)"\)'),
    ("scripts/tasks.ps1", r"\$ImageTag\s*=\s*'([^']+)'"),
]


def check_tag_consistency():
    """返回 (status, detail, subs)。status: PASS/FAIL/BLOCKED。"""
    subs = []
    values = {}
    for rel, pat in TAG_LOCS:
        path = os.path.join(REPO, rel)
        if not os.path.isfile(path):
            subs.append({"name": rel, "status": "fail",
                         "detail": "文件不存在"})
            continue
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            hits = re.findall(pat, f.read(), re.M)
        if len(hits) != 1:
            subs.append({"name": rel, "status": "fail",
                         "detail": "匹配 %d 次（应恰好 1 次）：%s"
                                   % (len(hits), pat)})
        else:
            values["%s :: %s" % (rel, pat)] = hits[0]
            subs.append({"name": rel, "status": "pass", "detail": hits[0]})
    if any(s["status"] == "fail" for s in subs):
        return "FAIL", "标签位置缺失或歧义（见子检查）", subs
    distinct = sorted(set(values.values()))
    if len(distinct) != 1:
        return ("FAIL",
                "%d 个位置出现 %d 个不同标签：%s"
                % (len(values), len(distinct),
                   "；".join("%s=%s" % kv for kv in values.items())),
                subs)
    common = distinct[0]
    # 与最新 git tag 对齐（剥掉前导 v）：防「七处一起漂到一个没人发过的值」
    rc, out = run_cmd(["git", "describe", "--tags", "--abbrev=0"], timeout=30)
    if rc != 0:
        subs.append({"name": "git tag", "status": "warn",
                     "detail": "取不到最新 tag（跳过对齐检查）"})
        return "PASS", "七处一致 = %s（未与 git tag 对齐：无 tag）" % common, subs
    tag = out.strip()
    tag_ver = tag[1:] if tag.startswith("v") else tag
    if tag_ver != common:
        subs.append({"name": "git tag 对齐", "status": "fail",
                     "detail": "tag=%s 但代码标签=%s" % (tag_ver, common)})
        return ("FAIL", "git tag %s 与七处标签 %s 不一致" % (tag_ver, common),
                subs)
    subs.append({"name": "git tag 对齐", "status": "pass",
                 "detail": "%s == %s" % (tag, common)})
    return "PASS", "七处 + git tag 全部一致 = %s" % common, subs


def check_clean_clone_build():
    """干净克隆 + 全新 GOCACHE 下 go build —— MVP 总验收第一项的前半句。

    「本地能编译」不等于「干净机器 clone 完能编译」：后者才会暴露
    未提交的生成物/漏 git add 的文件。网络类错误记 BLOCKED（环境问题），
    编译错误记 FAIL（仓库真的不完整）。
    """
    go = find_go()
    if not go:
        return "BLOCKED", "找不到 go 工具链", []
    tmp = tempfile.mkdtemp(prefix="iip_e2e_clone_")
    dest = os.path.join(tmp, "iip")
    t0 = time.time()
    rc, out = run_cmd(["git", "clone", "--quiet", REPO, dest], timeout=180)
    if rc != 0:
        shutil.rmtree(tmp, ignore_errors=True)
        return "FAIL", "git clone 失败：%s" % out[-300:], []
    env_extra = {
        "GOCACHE": os.path.join(tmp, "gocache"),
        "GOPROXY": "off",       # 模块缓存不命中即失败，不偷偷走网络
        "GOTOOLCHAIN": "local",
        "GOFLAGS": "-mod=mod",
    }
    rc, out = run_cmd([go, "build", "./..."], cwd=dest, timeout=480,
                      env_extra=env_extra)
    dt = time.time() - t0
    shutil.rmtree(tmp, ignore_errors=True)
    if rc == 0:
        return "PASS", "clone + 全量 build 成功（%.0fs，GOPROXY=off）" % dt, []
    net_flavor = any(k in out for k in
                     ("GOPROXY=off", "dial tcp", "connection refused",
                      "i/o timeout", "TLS handshake", "proxyconnect",
                      "unrecognized import path"))
    if net_flavor:
        return ("BLOCKED",
                "模块缓存不命中（GOPROXY=off 下无法联网补齐）：%s"
                % out.strip().splitlines()[-1][:200], [])
    return "FAIL", "干净克隆构建失败：%s" % out.strip()[-600:], []


def stage_s1():
    sid = "s1"
    t0 = time.time()
    title = "S1 静态：二进制/标签/helm/compose/文件规模/观测配置/克隆构建"
    subs = []

    def sub(name, status, detail):
        subs.append({"name": name, "status": status, "detail": detail})

    # 1) 交叉编译 + ELF 静态校验（distroless 前提）
    py = sys.executable
    rc, out = run_cmd([py, os.path.join("scripts", "check-binaries.py")],
                      timeout=600)
    sub("check-binaries（静态链接校验）",
        "pass" if rc == 0 else "fail",
        "rc=%d %s" % (rc, "" if rc == 0 else out[-200:]))

    # 2) 标签四联动（T7.6 漂移病的守卫判据）
    st, detail, tag_subs = check_tag_consistency()
    subs.extend(tag_subs)
    sub("标签四联动 + git tag 对齐", st, detail)

    # 3) helm lint + 两组 template
    helm = find_helm()
    if not helm:
        sub("helm lint + template", "blocked", "找不到 helm 二进制")
    else:
        rc, out = run_cmd([helm, "lint", os.path.join("deploy", "helm", "iip")],
                          timeout=120)
        ok_lint = rc == 0
        rc2, _ = run_cmd([helm, "template", "t1",
                          os.path.join("deploy", "helm", "iip")], timeout=120)
        rc3, _ = run_cmd([helm, "template", "t2",
                          os.path.join("deploy", "helm", "iip"),
                          "--set", "database.embedded=false",
                          "--set", "database.external.host=pg.example.com",
                          "--set", "database.external.password=secret"],
                         timeout=120)
        ok_t = rc2 == 0 and rc3 == 0
        sub("helm lint + 两组 template",
            "pass" if (ok_lint and ok_t) else "fail",
            "lint rc=%d / 默认组 rc=%d / 外置组 rc=%d" % (rc, rc2, rc3))

    # 4) compose config（不需要 daemon 的静态校验）
    docker = find_docker()
    if not docker:
        sub("compose config --quiet", "blocked", "找不到 docker CLI")
    else:
        rc, out = run_cmd([docker, "compose", "-f",
                           os.path.join("deploy", "docker-compose.yml"),
                           "config", "--quiet"], timeout=120)
        sub("compose config --quiet", "pass" if rc == 0 else "fail",
            "rc=%d %s" % (rc, "" if rc == 0 else out[-200:]))

    # 5) 单文件 <= 300 行门禁（OPEN-M5-7 / OPEN-M6-1 的守卫）
    #    纯静态、秒级。它的价值不在「今天全绿」，而在「下次有人把文件写长时立刻红」——
    #    这条约束在 M2–M6 之间没人守，于是 main.go 从 318 涨到 485 也没人发现。
    rc, out = run_cmd([py, os.path.join("scripts", "check-file-size.py")],
                      timeout=60)
    sub("单文件 <= 300 行门禁", "pass" if rc == 0 else "fail",
        "rc=%d %s" % (rc, "" if rc == 0 else out[-200:]))

    # 6) 观测配置静态核对（T6.4）：面板/规则引用的指标名、标签值、记录规则、
    #    抓取端口是否与代码里的定义一致。这些错法**全都不报错**，只表现为面板永久空图，
    #    所以它是纯静态也能抓到东西的那一类，不需要 Docker / Prometheus / Grafana。
    rc, out = run_cmd([py, os.path.join("scripts", "verify-observability.py")],
                      timeout=120)
    sub("观测配置静态核对（T6.4）", "pass" if rc == 0 else "fail",
        "rc=%d %s" % (rc, "" if rc == 0 else out[-200:]))

    # 7) 干净克隆构建
    st, detail, clone_subs = check_clean_clone_build()
    subs.extend(clone_subs)
    sub("干净克隆 + 全量 build", st, detail)

    if any(s["status"] == "fail" for s in subs):
        status = "FAIL"
    elif any(s["status"] == "blocked" for s in subs):
        status = "BLOCKED"
    else:
        status = "PASS"
    hint = ""
    if status != "PASS":
        hint = "修完重跑：python scripts/verify-e2e.py --stages s1"
    return write_stage_json(sid, title, status, None, time.time() - t0,
                            "%d 子检查：%s" % (
                                len(subs),
                                "；".join("%s=%s" % (s["name"], s["status"])
                                          for s in subs)),
                            subchecks=subs, hint=hint)


# ---------------------------------------------------------------- 阶段注册

PY = sys.executable


def stages():
    """返回 {id: (title, runner)}。runner 是无参函数，返回 stage json dict。"""
    def script_stage(sid, cmd, ports, rc_map, report, title, timeout,
                     env_extra=None):
        def _run():
            return run_script_stage(
                sid, title, cmd, timeout, ports, rc_map, report,
                env_extra=env_extra)
        return _run

    reg = {
        "s0": ("S0 前置", stage_s0),
        "s1": ("S1 静态", stage_s1),
        "s2": ("S2 真库全量测试（go test -p 1，含 M2 Replay）",
               script_stage(
                   "s2",
                   [PY, os.path.join("scripts", "run_pgtests.py")],
                   STAGE_PORTS["s2"],
                   {0: "PASS", 1: "FAIL", 3: "FAIL"},
                   os.path.join(r"D:\_iip_setup", "_pgtest_final.txt"),
                   "S2 真库全量测试", 900)),
        "s3": ("S3 业务闭环（M1→M4 + F1/F2/F3，~90 判据）",
               script_stage(
                   "s3",
                   [PY, os.path.join("scripts", "verify-func.py")],
                   STAGE_PORTS["s3"],
                   {0: "PASS", 1: "FAIL", 5: "FAIL"},
                   os.path.join(_HERE, "_verify_func.txt"),
                   "S3 业务闭环", 900)),
        "s4": ("S4 UI（vitest 单测 + playwright 6 specs）", stage_s4),
        "s5": ("S5 故障演练（D1/D2/D3，33 判据）",
               script_stage(
                   "s5",
                   [PY, os.path.join("scripts", "fault_drill.py")],
                   STAGE_PORTS["s5"],
                   {0: "PASS", 1: "FAIL"},
                   os.path.join(_HERE, "_fault_drill.txt"),
                   "S5 故障演练", 600)),
        "s6": ("S6 压测（1 万条，T7.3 同参）",
               script_stage(
                   "s6",
                   [PY, os.path.join("scripts", "ingest_bench.py"),
                    "-n", "10000", "-b", "1", "-c", "8"],
                   STAGE_PORTS["s6"],
                   {0: "PASS", 1: "FAIL"},
                   os.path.join(_HERE, "_verify_bench.txt"),
                   "S6 压测", 600)),
        "s7": ("S7 容器链路（构建 + compose up + 逐项断言）",
               script_stage(
                   "s7",
                   [PY, os.path.join("scripts", "verify-images.py")],
                   STAGE_PORTS["s7"],
                   {0: "PASS", 2: "BLOCKED", 3: "FAIL", 1: "FAIL"},
                   os.path.join(REPO, "_verify_images.txt"),
                   "S7 容器链路", 1800)),
    }
    return reg


def pkgmgr_argv(pkg_path, args):
    """pnpm/npm 在 Windows 上是 .CMD：CreateProcess 不能直接跑，
    必须 cmd /c 包一层（rc=127 command not found 的根因）。"""
    if pkg_path.lower().endswith((".cmd", ".bat")):
        return ["cmd", "/c", pkg_path] + list(args)
    return [pkg_path] + list(args)


def stage_s4():
    """S4 UI：vitest 单测 + playwright E2E（真后端，6 specs）。

    playwright 栈默认找 PATH 里的 go——沙箱 PATH 是空的，所以显式传
    E2E_GO_BIN 绝对路径（stack.mjs 支持这个 env）。缺 node/Chrome/dist
    都是环境问题 -> BLOCKED；测试红 -> FAIL。
    """
    sid = "s4"
    t0 = time.time()
    title = "S4 UI（vitest + playwright）"
    subs = []

    def sub(name, status, detail):
        subs.append({"name": name, "status": status, "detail": detail})

    for p in STAGE_PORTS["s4"]:
        if not port_free(p):
            return write_stage_json(
                sid, title, "FAIL", None, time.time() - t0,
                "端口 %d 被占用" % p,
                subchecks=subs,
                hint="netstat -ano | findstr %d；playwright 栈用 5434/8087" % p)

    pkg, pkg_path = find_pkgmgr()
    if not pkg:
        return write_stage_json(sid, title, "BLOCKED", None,
                                time.time() - t0,
                                "找不到 pnpm/npm", subchecks=subs,
                                hint="装 Node.js 后重跑 --stages s4")
    if not os.path.isdir(os.path.join(REPO, "web", "node_modules")):
        return write_stage_json(sid, title, "BLOCKED", None,
                                time.time() - t0,
                                "web/node_modules 不存在", subchecks=subs,
                                hint="cd web && %s install 后重跑" % pkg)
    if not os.path.isfile(os.path.join(REPO, "web", "dist", "index.html")):
        rc, out = run_cmd(pkgmgr_argv(pkg_path, ["run", "build"]),
                          cwd=os.path.join(REPO, "web"), timeout=600)
        sub("pnpm build（dist 缺失，补构建）",
            "pass" if rc == 0 else "blocked",
            "rc=%d %s" % (rc, "" if rc == 0 else out[-200:]))
        if rc != 0:
            return write_stage_json(sid, title, "BLOCKED", None,
                                    time.time() - t0,
                                    "前端构建失败", subchecks=subs)
    chrome = find_chrome()
    if not chrome:
        return write_stage_json(sid, title, "BLOCKED", None,
                                time.time() - t0,
                                "找不到 Chrome（playwright channel: chrome）",
                                subchecks=subs,
                                hint="装 Chrome 或设 IIP_CHROME_BIN")

    go = find_go()
    env_extra = {"E2E_GO_BIN": go} if go else {}

    def persist(tag, out):
        """子检查输出落盘：成功的也留痕——「33 秒俩都过」这种快 PASS
        必须能独立复核（vitest 的 Test Files 行 / playwright 的 passed 行）。"""
        p = os.path.join(E2E_DIR, "s4_%s.log" % tag)
        try:
            with open(p, "w", encoding="utf-8", errors="replace") as f:
                f.write(out)
        except OSError:
            pass
        tail = [l for l in out.strip().splitlines() if l.strip()]
        return tail[-1][:120] if tail else ""

    # 1) vitest 单测
    rc, out = run_cmd(pkgmgr_argv(pkg_path, ["test"]),
                      cwd=os.path.join(REPO, "web"),
                      timeout=600, env_extra=env_extra)
    sub("vitest 单测", "pass" if rc == 0 else "fail",
        "rc=%d %s" % (rc, "" if rc == 0 else out[-300:])
        + (" | " + persist("vitest", out) if rc == 0 else ""))

    # 2) playwright E2E（真 PG + 真 server，stack.mjs 自建自收）
    rc, out = run_cmd(pkgmgr_argv(pkg_path, ["run", "test:e2e"]),
                      cwd=os.path.join(REPO, "web"),
                      timeout=600, env_extra=env_extra)
    sub("playwright E2E（6 specs）", "pass" if rc == 0 else "fail",
        "rc=%d %s" % (rc, "" if rc == 0 else out[-300:])
        + (" | " + persist("playwright", out) if rc == 0 else ""))

    status = "FAIL" if any(s["status"] == "fail" for s in subs) else "PASS"
    hint = "" if status == "PASS" else "修完重跑 --stages s4"
    return write_stage_json(sid, title, status, None, time.time() - t0,
                            "；".join("%s=%s" % (s["name"], s["status"])
                                      for s in subs),
                            subchecks=subs, hint=hint)


# ---------------------------------------------------------------- 汇总

# M1-M6 验收项 -> 覆盖阶段（空列表 = 本 E2E 有意不覆盖，必须写原因）
M_MAP = [
    ("M0", "干净 PG 上迁移可重复执行", ["s2"], ""),
    ("M0", "server/worker 启动且 /healthz 200", ["s3"], ""),
    ("M0", "CI 三色通过", [], "仓库无 git remote，ci.yml 从未在 GitHub 上跑过"),
    ("M1", "真实告警 3s 内落库，labels/severity/fingerprint 正确", ["s3"], ""),
    ("M1", "重复 firing 只 1 行、starts_at 为首次时间", ["s2", "s3"], ""),
    ("M1", "并发重复 webhook 无脏数据（单测+集成）", ["s2"], ""),
    ("M1", "ingest 包覆盖率 ≥80%", ["s2"], ""),
    ("M2", "Replay 三场景：精确率 100% / 召回 ≥90% / 杂告警独立", ["s2"], ""),
    ("M2", "权重阈值修改后下条告警即按新值（热生效）", ["s2"], ""),
    ("M2", "correlation 包覆盖率 ≥90%", ["s2"], ""),
    ("M2", "1000 条告警回放 <60s", ["s2"], ""),
    ("M3", "合法转换全覆盖 + 每种非法转换被拒", ["s3", "s4"], ""),
    ("M3", "timeline 完整链路（关联/认领/状态变更）", ["s3"], ""),
    ("M3", "REVIEW 队列可处理，二次相同告警不重复打扰", ["s3"], ""),
    ("M4", "2 分钟内 ≥5 条证据、query 字段可复现", ["s3"], ""),
    ("M4", "外部源宕机零阻塞，恢复后自动补采", ["s5"], ""),
    ("M4", "三类采集器均有 mock server 单测", ["s2"], ""),
    ("M5", "UI 处置 incident 全流程（自动化部分）", ["s4"], ""),
    ("M5", "Detail 三要素：时间线/证据/关联告警及评分", ["s4"], ""),
    ("M5", "处置操作实时生效并回写 timeline", ["s4"], ""),
    ("M5", "新用户 30 分钟内完成处置（UX 计时）", [],
     "人工验收项：E2E 覆盖功能不覆盖人的操作时长"),
    ("M6", "手册 M6 S1–S5（证据深化/confidence 判据）", ["s3"], ""),
    # 计划 §10 的 M6（T6.1–T6.6）已全部收口。**此前的写法是 `([], "未开工")`** ——
    # 那段文字在 T6.1 落地当天就过期了，却一直躺在总验收表里，让汇总报告
    # 与人读的 README/CHANGELOG 直接矛盾。现按「实际被哪一阶段证明」逐项拆分：
    # 单测类挂在 S2（真库全量测试会跑它们），静态类挂在 S1；
    # 没有编排进 E2E 的，宁可写空列表 + 原因，也不给它一个假的 PASS。
    ("M6", "通知去风暴 / 终态必发 / 两个 sink 拒重定向（T6.1/T6.2）单测", ["s2"],
     "真栈收消息判据曾用一次性探针验过，未入库 ⇒ 本 E2E 只覆盖单测那一半"),
    ("M6", "`/metrics` 指标族、标签上界、worker 独立注册表（T6.3）单测", ["s2"], ""),
    ("M6", "观测配置与代码定义一致（T6.4，静态）", ["s1"], ""),
    ("M6", "接入指南代码块原文可执行（T6.5）", [],
     "有专项脚本 verify-onboarding.py（真 PG + 真 server），尚未编排进本 E2E"),
    ("M6", "非 UTF-8 → 400 / 超限 → 413 入口矩阵（T6.6）单测", ["s2"], ""),
]

MVP_MAP = [
    ("干净机器 git clone + docker compose up ≤15 分钟完成部署",
     ["s1", "s7"],
     "s1 覆盖 clone+构建；compose up+健康与计时在 s7（daemon 缺席则 BLOCKED）"),
    ("Replay 数据集 + 故障演练全部通过", ["s2", "s5"], ""),
    ("发布 v0.1.0", ["s1"], "S1 标签四联动一致 + git tag v0.1.0 存在"),
]

STATUS_ORDER = {"FAIL": 0, "BLOCKED": 1, "MISSING": 2, "PASS": 3}


def load_all_stages():
    out = {}
    if not os.path.isdir(E2E_DIR):
        return out
    for name in sorted(os.listdir(E2E_DIR)):
        if name.endswith(".json"):
            try:
                with open(os.path.join(E2E_DIR, name), "r",
                          encoding="utf-8") as f:
                    d = json.load(f)
                out[d.get("id", name[:-5])] = d
            except (OSError, ValueError):
                pass
    return out


def compose_status(stage_ids, results):
    """把若干阶段的状态合成一个验收项状态。缺阶段 = MISSING（不装 PASS）。"""
    if not stage_ids:
        return "N/A"
    sts = [results[s]["status"] if s in results else "MISSING"
           for s in stage_ids]
    if "FAIL" in sts:
        return "FAIL"
    if "MISSING" in sts:
        return "MISSING"
    if "BLOCKED" in sts:
        return "BLOCKED"
    return "PASS"


def build_summary(strict=False):
    results = load_all_stages()
    lines = []
    lines.append("=" * 74)
    lines.append(" IIP E2E 回归汇总   " + time.strftime("%Y-%m-%d %H:%M:%S"))
    lines.append("=" * 74)
    lines.append("")

    reg = stages()
    lines.append("### 阶段结果")
    lines.append("  %-4s %-46s %-8s %-7s %s"
                 % ("ID", "阶段", "状态", "耗时", "判据/明细"))
    for sid in sorted(reg):
        if sid not in results:
            lines.append("  %-4s %-46s %-8s %-7s %s"
                         % (sid, reg[sid][0], "MISSING", "—",
                            "本轮未跑（JSON 不存在，不冒充 PASS）"))
            continue
        d = results[sid]
        counts = ""
        rep = {"s2": r"D:\_iip_setup\_pgtest_final.txt",
               "s3": os.path.join(_HERE, "_verify_func.txt"),
               "s5": os.path.join(_HERE, "_fault_drill.txt"),
               "s6": os.path.join(_HERE, "_verify_bench.txt"),
               "s7": os.path.join(REPO, "_verify_images.txt")}.get(sid)
        c = extract_counts(rep)
        if c:
            counts = "%d/%d/%d" % c
            # BLOCKED 阶段的 FAIL 通常是「daemon 可用」这类环境检查项，
            # 标注出来防止误读成产品判据红。
            if d["status"] == "BLOCKED" and c[1]:
                counts += "（FAIL 为环境缺条件项）"
        lines.append("  %-4s %-46s %-8s %-7s %s"
                     % (sid, d["title"][:46], d["status"],
                        "%.0fs" % d.get("elapsed", 0),
                        (counts + " " if counts else "") + d.get("detail", "")[:60]))
    lines.append("")

    lines.append("### M1-M6 验收项映射")
    lines.append("  %-4s %-52s %-10s %s"
                 % ("里程碑", "验收项", "状态", "覆盖阶段"))
    for ms, item, covers, note in M_MAP:
        st = compose_status(covers, results) if covers else "N/A"
        extra = note if note else ("、".join(covers) if covers else "")
        lines.append("  %-4s %-52s %-10s %s"
                     % (ms, item[:52], st, extra[:40]))
    lines.append("")

    lines.append("### MVP 总验收（开发计划 §11）")
    for crit, covers, note in MVP_MAP:
        st = compose_status(covers, results)
        lines.append("  [%s] %s" % (st, crit))
        if note:
            lines.append("        备注: %s" % note)
    lines.append("")

    all_sts = [results[s]["status"] for s in results]
    n_fail = sum(1 for s in all_sts if s == "FAIL")
    n_block = sum(1 for s in all_sts if s == "BLOCKED")
    n_missing = sum(1 for sid in reg if sid not in results)
    lines.append("### 结论")
    if n_fail:
        verdict = "FAIL（%d 个阶段红）" % n_fail
        rc = 1
    elif n_block and strict:
        verdict = "FAIL（strict 模式：%d 个阶段 BLOCKED）" % n_block
        rc = 1
    elif n_missing:
        verdict = "未完整（%d 个阶段从未跑过）" % n_missing
        rc = 1
    elif n_block:
        verdict = ("PASS-WITH-BLOCKED（无红，但 %d 个阶段缺环境未实证，"
                   "MVP 未完整验收）" % n_block)
        rc = 0
    else:
        verdict = "PASS（全部阶段通过）"
        rc = 0
    lines.append("  " + verdict)
    if n_block:
        lines.append("  BLOCKED 阶段及补救：")
        for sid in sorted(results):
            if results[sid]["status"] == "BLOCKED":
                lines.append("    - %s: %s" % (sid, results[sid].get("hint", "")))
    lines.append("  退出码: %d（--strict 可把 BLOCKED 也判红）" % rc)
    lines.append("=" * 74)
    return rc, "\n".join(lines)


def cmd_summary(strict):
    if not os.path.isdir(E2E_DIR) or not load_all_stages():
        print("没有任何阶段结果（scripts/_e2e/ 为空）。先跑阶段，"
              "例如：python scripts/verify-e2e.py --stages s2,s3")
        return 2
    rc, text = build_summary(strict=strict)
    with open(REPORT, "w", encoding="utf-8") as f:
        f.write(text + "\n")
    print(text)
    print("\n报告: %s" % REPORT)
    return rc


def main():
    ap = argparse.ArgumentParser(
        description="IIP E2E 回归编排器（T7.7）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="阶段: s0 前置 | s1 静态 | s2 真库测试 | s3 业务闭环 | "
               "s4 UI | s5 故障演练 | s6 压测 | s7 容器链路\n"
               "示例: --stages s2,s3 分批跑；--summary 汇总；--dry-run 看计划")
    ap.add_argument("--stages", default="",
                    help="逗号分隔的阶段 id（默认全部）")
    ap.add_argument("--summary", action="store_true",
                    help="只汇总已有阶段 JSON，不跑任何阶段")
    ap.add_argument("--no-bench", action="store_true",
                    help="跳过 S6 压测（记 SKIP）")
    ap.add_argument("--strict", action="store_true",
                    help="BLOCKED 也判红（CI/完整环境用）")
    ap.add_argument("--dry-run", action="store_true",
                    help="只打印阶段计划，不执行")
    args = ap.parse_args()

    if args.summary:
        return cmd_summary(args.strict)

    reg = stages()
    if args.stages:
        wanted = [s.strip() for s in args.stages.split(",") if s.strip()]
        unknown = [s for s in wanted if s not in reg]
        if unknown:
            print("未知阶段: %s\n可用: %s" % (", ".join(unknown),
                                             ", ".join(sorted(reg))))
            return 2
        run_ids = wanted
    else:
        run_ids = sorted(reg)

    if args.dry_run:
        print("E2E 阶段计划（不执行）：")
        for sid in run_ids:
            ports = STAGE_PORTS.get(sid, [])
            print("  %-4s %-46s 端口=%s" % (sid, reg[sid][0][:46],
                                            ",".join(map(str, ports)) or "—"))
        return 0

    if args.no_bench and "s6" in run_ids:
        run_ids.remove("s6")
        write_stage_json("s6", reg["s6"][0], "SKIP", None, 0.0,
                         "--no-bench 跳过", hint="显式开启：去掉 --no-bench")

    t_all = time.time()
    results = []
    for sid in run_ids:
        title, runner = reg[sid]
        print("[e2e] %s %s ..." % (sid, title))
        d = runner()
        results.append(d)
        print("[e2e] %s -> %s（%.0fs）%s"
              % (sid, d["status"], d.get("elapsed", 0),
                 "" if d["status"] == "PASS" else " " + d.get("hint", "")))
    print("[e2e] 全部所选阶段跑完，总耗时 %.0fs" % (time.time() - t_all))

    # 分批模式也要把阶段失败反映到退出码：脚本化消费（CI/别的脚本）
    # 靠 rc 判断，不能跑完红阶段还返 0。
    if any(d["status"] == "FAIL" for d in results):
        return 1
    if args.strict and any(d["status"] == "BLOCKED" for d in results):
        return 1

    if not args.stages:  # 一把跑完才自动汇总；分批跑由用户最后 --summary
        return cmd_summary(args.strict)
    print("分批模式：跑完其余阶段后执行 --summary 出总报告")
    return 0


if __name__ == "__main__":
    sys.exit(main())
