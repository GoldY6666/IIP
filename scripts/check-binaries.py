"""交叉编译 linux/amd64 产物并校验其满足 distroless/static 的硬前提。

这条链路验证的是「Docker 镜像里将要放的那个二进制」：
参数与 Dockerfile builder 阶段逐项一致，因此一旦 Docker daemon 可用，
容器链路的成败范围就被压缩到 Docker 本身，而不是我们的构建脚本。

校验内容：
  1. 真静态链接 —— PT_INTERP 缺失 + 无 libc/libpthread/libdl 引用
     （不满足则 distroless/static 会启动即崩）
  2. 版本号注入生效 —— 防 -ldflags "-X main.version=" 静默失效
  3. 迁移 SQL 嵌入 —— server 必须含；worker 不应含（见下方说明）

关于 worker 不含迁移 SQL：
  store.Migrate 只在 cmd/iip-server/main.go 被调用，worker 从不调用。
  Go 链接器的死代码消除会把未引用的 store.Migrate 及其 iofs 依赖整体剥离，
  因此 //go:embed 的迁移数据不会进入 worker 二进制。这是正确且更优的结果
  （worker 本就不该有建表能力，体积也更小）。

用法（仓库根执行）：
  python scripts/check-binaries.py

环境变量（全部可选）：
  IIP_SRC      源码根        默认脚本所在目录的上一级
  IIP_GO       go 可执行     默认 PATH，否则探测常见位置
  IIP_OUT      产物输出目录  默认 <系统临时目录>/iip-bin-check
  IIP_VERSION  注入的版本号  默认 0.1.0（发版时与镜像标签一致）
  IIP_ARCH     目标架构      默认 amd64（可选 arm64）
  IIP_LOG_DIR  报告输出目录  默认源码根
"""
import os
import shutil
import struct
import subprocess
import sys
import tempfile

GO = os.environ.get("IIP_GO") or shutil.which("go") or r"C:\Program Files\Go\bin\go.exe"
_HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.environ.get("IIP_SRC") or os.path.dirname(_HERE)
OUT = os.environ.get("IIP_OUT") or os.path.join(tempfile.gettempdir(), "iip-bin-check")
# T7.6（2026-10-14）：默认版本串随发版切到 0.1.0 —— 与镜像标签保持一致，
# 否则「版本串注入生效」验的是一个发版里并不存在的旧值。
VERSION = os.environ.get("IIP_VERSION", "0.1.0")
ARCH = os.environ.get("IIP_ARCH", "amd64")
LOG_DIR = os.environ.get("IIP_LOG_DIR") or SRC

PT_INTERP = 3
PT_DYNAMIC = 2
EM_MACHINE = {"amd64": 62, "arm64": 183}

MIG_MARKERS = [
    (b"0001_init.up.sql", "迁移文件名"),
    (b"schema_migrations", "golang-migrate 版本表"),
    (b"CREATE TABLE alerts", "alerts 建表语句"),
    (b"CREATE TABLE incidents", "incidents 建表语句"),
    (b"chk_alerts_status", "CHECK 约束名"),
    # M2（0003）：新增迁移必须一并纳入校验，否则「忘了把新迁移打进二进制」
    # 只会在容器首次启动跑迁移时才暴露。
    (b"0003_correlation_m2.up.sql", "M2 迁移文件名"),
    (b"idx_incident_alerts_needs_review", "M2 部分索引名"),
]

ROLE_SPEC = [
    ("iip-server", "./cmd/iip-server", True, "HTTP 服务端，负责执行迁移"),
    ("iip-worker", "./cmd/iip-worker", False, "后台 poller，不执行迁移（死代码消除剥离 embed，预期）"),
]

lines = []
failed = []


def log(s=""):
    lines.append(str(s))
    print(s, flush=True)


def fail(msg):
    failed.append(msg)
    log(f"  [FAIL] {msg}")


def read_elf(path):
    with open(path, "rb") as f:
        data = f.read()
    if data[:4] != b"\x7fELF" or data[4] != 2 or data[5] != 1:
        return None
    machine = struct.unpack_from("<H", data, 18)[0]
    phoff = struct.unpack_from("<Q", data, 32)[0]
    phentsize = struct.unpack_from("<H", data, 54)[0]
    phnum = struct.unpack_from("<H", data, 56)[0]
    types = []
    for i in range(phnum):
        off = phoff + i * phentsize
        types.append(struct.unpack_from("<I", data, off)[0])
    return {"machine": machine, "types": types, "blob": data}


os.makedirs(OUT, exist_ok=True)

env = dict(os.environ)
env["CGO_ENABLED"] = "0"
env["GOOS"] = "linux"
env["GOARCH"] = ARCH
env.pop("GOFLAGS", None)

log("=" * 72)
log(f"  IIP 产物校验 — linux/{ARCH} 静态二进制（Docker 镜像内容物）")
log("=" * 72)
log(f"  GO   = {GO}")
log(f"  SRC  = {SRC}")
log(f"  OUT  = {OUT}")
log(f"  VERSION = {VERSION}")
log("")

# ---------------------------------------------------------------- 构建
log("-" * 72)
log("### 构建（参数与 Dockerfile builder 阶段一致）")
log("-" * 72)
log('  Dockerfile: CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \\')
log('                -trimpath -ldflags="-s -w -X main.version=${VERSION}"')
log("")

built = []
for name, pkg, _need_mig, _desc in ROLE_SPEC:
    outpath = os.path.join(OUT, name)
    if os.path.exists(outpath):
        os.remove(outpath)
    cmd = [GO, "build", "-trimpath",
           f"-ldflags=-s -w -X main.version={VERSION}",
           "-o", outpath, pkg]
    r = subprocess.run(cmd, cwd=SRC, env=env, capture_output=True,
                       text=True, encoding="utf-8", errors="replace", timeout=900)
    if r.returncode != 0 or not os.path.exists(outpath):
        fail(f"{name}: 构建失败 rc={r.returncode} {(r.stderr or '')[:200]}")
        built.append((name, None))
    else:
        sz = os.path.getsize(outpath)
        log(f"  [PASS] {name} 构建成功  {sz:,} 字节 ({sz/1024/1024:.2f} MB)")
        built.append((name, outpath))
log("")

# ---------------------------------------------------------------- 校验
for (name, path), (_, _, need_mig, desc) in zip(built, ROLE_SPEC):
    log("-" * 72)
    log(f"### {name}   ({desc})")
    log("-" * 72)
    if not path:
        continue

    info = read_elf(path)
    if info is None:
        fail(f"{name}: 不是合法 ELF64 LE 文件")
        continue
    blob = info["blob"]

    expect_m = EM_MACHINE.get(ARCH)
    if info["machine"] != expect_m:
        fail(f"{name}: e_machine={info['machine']}，期望 {expect_m} ({ARCH})")
    else:
        log(f"  [PASS] 架构正确 EM_{'X86_64' if ARCH == 'amd64' else 'AARCH64'}")

    if PT_INTERP in info["types"]:
        fail(f"{name}: 存在 PT_INTERP（动态链接）— distroless/static 无法运行")
    else:
        log("  [PASS] PT_INTERP 缺失 -> 无动态加载器")

    if PT_DYNAMIC in info["types"]:
        log("  [WARN] 存在 PT_DYNAMIC 段（静态 Go 二进制常见，非阻断）")
    else:
        log("  [PASS] PT_DYNAMIC 缺失")

    libs = [l.decode() for l in (b"libc.so", b"libpthread", b"libdl.so", b"ld-linux") if l in blob]
    if libs:
        fail(f"{name}: 引用动态库 {libs}")
    else:
        log("  [PASS] 无 libc/libpthread/libdl/ld-linux 引用 -> 纯静态")

    if VERSION.encode() in blob:
        log(f"  [PASS] 版本串 '{VERSION}' 已注入（-ldflags 生效）")
    else:
        fail(f"{name}: 版本串未注入 — -ldflags -X main.version 失效")

    log("")
    log(f"  迁移 SQL 检查（该角色期望: {'存在' if need_mig else '不存在'}）:")
    found = {label: (marker in blob) for marker, label in MIG_MARKERS}
    if need_mig:
        for label, ok in found.items():
            if ok:
                log(f"    [PASS] {label}")
            else:
                fail(f"{name}: 缺少 {label} — 运行时迁移会失败")
    else:
        present = [l for l, ok in found.items() if ok]
        if not present:
            log("    [PASS] 迁移数据已按预期被死代码消除剥离")
        else:
            log(f"    [WARN] 意外包含迁移数据: {present}（不影响正确性，体积偏大）")
    log("")

log("=" * 72)
if failed:
    log(f"  结论: FAIL — {len(failed)} 项未通过")
    for m in failed:
        log(f"    - {m}")
else:
    log("  结论: PASS — 全部检查项通过")
    log("")
    log("  提示：产物位于 %s" % OUT)
    log("        这些二进制即是 Docker 镜像里将要放入的那个文件。")
log("=" * 72)

report = os.path.join(LOG_DIR, "_verify_binaries.txt")
try:
    with open(report, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    print(f"\n报告: {report}")
except OSError as e:
    print(f"\n报告写入失败: {e}")

sys.exit(1 if failed else 0)
