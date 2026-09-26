"""从多个 Docker 镜像代理拉取基础镜像，并重打成项目所需的标准名字。

适用场景（中国大陆网络）：
  Docker Hub (docker.io) 直连返回 502 / 超时。mirror.gcr.io、public.ecr.aws
  这类源只覆盖 Docker Hub 官方 `library/*`，`prom/*`、`grafana/*` 拿不到。
  所以要走全量代理，而单一代理会限流/抖动，需要多源自动降级。

做法：
  依次尝试候选源，第一个成功的 pull 下来，再 `docker tag` 成标准名。
  之后本仓库的 `Dockerfile`（FROM golang/...、FROM gcr.io/distroless/...）
  与 `deploy/docker-compose.yml`（image: postgres/redis/prom/...）
  都能直接命中本地镜像，**无需修改任何项目文件**。

用法（仓库根执行）：
  python scripts/prepull-mirror.py            # 拉取缺失的镜像
  python scripts/prepull-mirror.py --force    # 强制重拉（镜像源变更后用）
  python scripts/prepull-mirror.py --check    # 只做清单漂移检查，不联网

环境变量：
  IIP_LOG_DIR     报告输出目录    默认 = 仓库根
  IIP_DOCKER_BIN  docker.exe 绝对路径（PATH 里没有时用）

幂等：标准名已存在则跳过（`--force` 强制重拉）。

为什么要有 `--check`：
  本脚本的镜像清单是「显式声明」的（因为要在镜像都还没有的时候运行，
  不能反过来去问 Docker）。显式清单天然会和 compose / Dockerfile 漂移 ——
  有人改了版本号却忘了改这里，拉取就会静默失效。`--check` 用来在提交前
  把这种漂移变成一次明确的失败。
"""
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lib_docker import connect, run as _run  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.environ.get("IIP_PROJ", os.path.dirname(HERE))
COMPOSE_FILE = os.path.join(PROJ, "deploy", "docker-compose.yml")
DOCKERFILE = os.path.join(PROJ, "Dockerfile")

REPORT = os.path.join(os.environ.get("IIP_LOG_DIR", PROJ), "_prepull.txt")

# 每个镜像的候选源，按优先级排列（实测速度：DaoCloud > 1Panel > dockerproxy > 轩辕）
SOURCES = [
    "docker.m.daocloud.io/{path}:{tag}",
    "docker.1panel.live/{path}:{tag}",
    "dockerproxy.net/{path}:{tag}",
    "docker.xuanyuan.me/{path}:{tag}",
    "mirror.gcr.io/{path}:{tag}",       # 仅对 library/* 有效
    "hub.rat.dev/{path}:{tag}",
]

# gcr.io 的候选源（distroless 不在 Docker Hub 上，registry-mirrors 也管不到它）
GCR_SOURCES = [
    "gcr.m.daocloud.io/{path}:{tag}",
    "gcr.io/{path}:{tag}",
]

# (Docker Hub 路径, 标签, 标准名, 说明)
HUB_IMAGES = [
    ("library/golang", "1.23-alpine", "golang:1.23-alpine", "构建用 Go 工具链"),
    ("library/postgres", "16-alpine", "postgres:16-alpine", "数据库"),
    ("library/redis", "7-alpine", "redis:7-alpine", "缓存"),
    ("prom/prometheus", "v2.54.0", "prom/prometheus:v2.54.0", "指标"),
    ("prom/alertmanager", "v0.27.0", "prom/alertmanager:v0.27.0", "告警"),
    ("grafana/loki", "3.1.0", "grafana/loki:3.1.0", "日志"),
]

# (gcr.io 路径, 标签, 标准名, 说明)
GCR_IMAGES = [
    ("distroless/static-debian12", "nonroot",
     "gcr.io/distroless/static-debian12:nonroot", "运行时基础镜像"),
]

DOCKER, ENV = connect()
FORCE = "--force" in sys.argv
CHECK_ONLY = "--check" in sys.argv

L: list[str] = []


def w(s: str = "") -> None:
    """写一行到报告并立即落盘。"""
    L.append(str(s))
    print(s, flush=True)
    try:
        with open(REPORT, "w", encoding="utf-8") as fh:
            fh.write("\n".join(L))
    except OSError:
        pass


def run(args, timeout=1200):
    return _run(args, ENV, timeout=timeout)


def has_image(name: str) -> bool:
    rc, out = run([DOCKER, "image", "inspect", name, "--format", "{{.Id}}"], timeout=60)
    return rc == 0 and out.startswith("sha256:")


def short_err(out: str) -> str:
    for line in out.splitlines():
        line = line.strip()
        if any(k in line for k in ("Error", "error", "denied", "Timeout", "timed out")):
            return line[:160]
    lines = [ln.strip() for ln in out.splitlines() if ln.strip()]
    return lines[-1][:160] if lines else ""


# ------------------------------------------------------------ 清单漂移检查
def declared_in_compose() -> set:
    """从 compose 提取第三方镜像名。

    跳过两类：
      - `*anchor` 这类 YAML 别名（别名不是真实镜像名，且本项目里它指向的是
        自己构建的 iip-server，本来就不需要拉）；
      - `iip-*`（本项目构建产物，不是要拉的基础镜像）。
    """
    out = set()
    if not os.path.exists(COMPOSE_FILE):
        return out
    with open(COMPOSE_FILE, encoding="utf-8") as fh:
        for line in fh:
            m = re.match(r"\s*image:\s*(\S+)\s*(?:#.*)?$", line)
            if not m:
                continue
            val = m.group(1).strip().strip('"\'')
            if val.startswith("*") or val.startswith("iip-"):
                continue
            out.add(val)
    return out


def declared_in_dockerfile() -> set:
    """从 Dockerfile 提取 `FROM` 的镜像名（多阶段会重复，用集合去重）。"""
    out = set()
    if not os.path.exists(DOCKERFILE):
        return out
    with open(DOCKERFILE, encoding="utf-8") as fh:
        for line in fh:
            # 兼容 `FROM --platform=linux/amd64 img AS stage` 的写法
            m = re.match(r"\s*FROM\s+(?:--platform=\S+\s+)?(\S+)", line, re.IGNORECASE)
            if not m:
                continue
            val = m.group(1).strip()
            if val.lower() == "scratch":
                continue
            out.add(val)
    return out


def run_check() -> int:
    """比对「本脚本声明的清单」与「compose + Dockerfile 实际需要的清单」。

    compose 里的 `postgres:16-alpine` 与 Docker Hub 上的 `library/postgres`
    是同一个镜像的两种写法，所以比对时统一归一到「标准名」再比。
    """
    declared = set()
    declared |= declared_in_compose()
    declared |= declared_in_dockerfile()

    script_set = {c for _, _, c, _ in HUB_IMAGES} | {c for _, _, c, _ in GCR_IMAGES}

    # 归一：compose/Dockerfile 写的名字本身就是标准名，直接比。
    # 唯一差异来源是「脚本多写了」或「项目改了版本而脚本没跟」。
    missing = sorted(declared - script_set)   # 项目要，脚本没拉 -> 真问题
    extra = sorted(script_set - declared)     # 脚本拉，项目不要 -> 冗余

    w("--- 清单漂移检查（--check，不联网）---")
    w("  compose 声明:   %s" % (sorted(declared_in_compose()) or "（无）"))
    w("  Dockerfile 声明: %s" % (sorted(declared_in_dockerfile()) or "（无）"))
    w()
    for name in sorted(script_set):
        mark = "OK " if name in declared else "冗余"
        w("  [%s] %s" % (mark, name))
    w()
    if missing:
        w("  [FAIL] 项目需要但脚本清单缺失（拉取会漏）：")
        for m in missing:
            w("      - %s" % m)
    if extra:
        w("  [WARN] 脚本清单里有、但项目已不再引用（可删）：")
        for e in extra:
            w("      - %s" % e)
    if not missing and not extra:
        w("  [PASS] 脚本清单与 compose + Dockerfile 完全一致")
    w()
    return 2 if missing else 0


# ------------------------------------------------------------------ 拉取
def pull_with_fallback(canonical, path, tag, templates, label):
    """依次尝试候选源，成功则 tag 成 canonical。返回 (状态, 实际用的源)。"""
    w("--- %s  (%s) ---" % (canonical, label))

    if not FORCE and has_image(canonical):
        w("    [SKIP] 本地已有 %s" % canonical)
        return "skip", None

    last_err = ""
    for tpl in templates:
        ref = tpl.format(path=path, tag=tag)
        t0 = time.time()
        rc, out = run([DOCKER, "pull", ref], timeout=1200)
        dt = time.time() - t0
        if rc == 0:
            w("    [OK]   %s  用时 %.1fs" % (ref, dt))
            rc2, out2 = run([DOCKER, "tag", ref, canonical], timeout=60)
            if rc2 != 0:
                w("    [FAIL] 打标签 %s: %s" % (canonical, out2[:160]))
                return False, ref
            w("    [OK]   已标记为 %s" % canonical)
            return True, ref
        last_err = short_err(out)
        w("    [miss] %s  用时 %.1fs  %s" % (ref, dt, last_err))

    w("    [FAIL] 所有候选源均失败。最后一个错误: %s" % last_err)
    return False, None


def main() -> int:
    w("=" * 78)
    w("  多源镜像拉取 -> 重打标准标签")
    w("  " + time.strftime("%Y-%m-%d %H:%M:%S"))
    w("=" * 78)
    w()

    if not DOCKER:
        w("  FATAL: 找不到 docker 可执行文件。")
        w("        设 IIP_DOCKER_BIN=<docker.exe 绝对路径>，或把 Docker Desktop")
        w("        的 resources\\bin 加入 PATH。")
        return 2
    w("  docker = %s" % DOCKER)

    rc, out = run([DOCKER, "info", "--format", "{{.ServerVersion}}"], timeout=60)
    if rc != 0:
        w("  FATAL: Docker daemon 不可用：%s" % short_err(out))
        return 2
    w("  daemon = %s" % out.strip())
    w()

    if CHECK_ONLY:
        code = run_check()
        w("报告: " + REPORT)
        return code

    w("  候选源(Docker Hub): %d 个" % len(SOURCES))
    for t in SOURCES:
        w("      " + t)
    w("  候选源(gcr.io)    : %d 个" % len(GCR_SOURCES))
    for t in GCR_SOURCES:
        w("      " + t)
    w("  强制重拉: %s" % FORCE)
    w()

    n_ok = n_fail = n_skip = 0
    for path, tag, canonical, label in HUB_IMAGES:
        r, _ = pull_with_fallback(canonical, path, tag, SOURCES, label)
        if r == "skip":
            n_skip += 1
        elif r:
            n_ok += 1
        else:
            n_fail += 1
        w()

    for path, tag, canonical, label in GCR_IMAGES:
        r, _ = pull_with_fallback(canonical, path, tag, GCR_SOURCES, label)
        if r == "skip":
            n_skip += 1
        elif r:
            n_ok += 1
        else:
            n_fail += 1
        w()

    w("=" * 78)
    w("  成功 %d / 跳过 %d / 失败 %d" % (n_ok, n_skip, n_fail))
    w("=" * 78)
    w()

    rc, out = run([DOCKER, "images", "--format", "{{.Repository}}:{{.Tag}}\t{{.Size}}"],
                  timeout=60)
    w("--- 当前本地镜像（关键项）---")
    KEYS = ("golang", "postgres", "redis", "prom/", "grafana/", "distroless", "iip-")
    for line in out.splitlines():
        if any(k in line for k in KEYS):
            w("    " + line)
    w()
    w("报告: " + REPORT)
    return 0 if n_fail == 0 else 1


if __name__ == "__main__":
    code = 1
    try:
        code = main()
    finally:
        try:
            with open(REPORT, "w", encoding="utf-8") as fh:
                fh.write("\n".join(L))
        except OSError:
            pass
    sys.exit(code)
