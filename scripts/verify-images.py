"""IIP v1 镜像与容器端到端验证。

用途：在 Docker daemon 可用后，一条命令完成「构建镜像 -> 起全部服务 ->
逐项断言 -> 打印证据 -> 清理」的完整闭环，产出可直接归档的验证输出。

为什么是脚本而不是一串手敲命令：
  1. 断言要逐项判定 pass/fail 并留证据，不能靠肉眼扫输出；
  2. 任何一步失败要立刻定位到具体服务，而不是等 compose 超时；
  3. 结尾必须回收容器，避免残留占端口影响后续验证。

前置条件：
  - Docker daemon 正在运行（`docker info` 返回 0）
  - 端口 8080 未被占用（compose 的 iip-server 要 bind 它）

用法（仓库根执行）：
  python scripts/verify-images.py

环境变量（全部可选）：
  IIP_DOCKER_BIN   docker.exe 绝对路径（不在 PATH 时必填）
  IIP_TAG          镜像标签          默认 0.2.0（与 compose / Makefile / chart 联动）
  IIP_HTTP_PORT    宿主机映射端口    默认 8080
  IIP_KEEP         设为 1 则验证后不清理容器（便于人工排查）
  IIP_LOG_DIR      报告输出目录      默认 = 仓库根
  IIP_GOPROXY      构建期 Go 模块代理 默认 https://goproxy.cn,direct
  IIP_GOMODCACHE   宿主 Go modcache  默认 %USERPROFILE%\\go\\pkg\\mod
                   4.8 段靠它 + GOPROXY=off 在容器内零网络跑 go test；
                   找不到时该段判FAIL（不静默跳过）
  IIP_CHROME_BIN   宿主 Chrome 绝对路径。**不设则跳过无头浏览器截图**
                   （4.8.2 段，OPEN-M5-8）。设了才对compose 起着的
                   8080 端口截图，并断言截图颜色种类 >= 4（非空白页）
  IIP_NODE_BIN     托管 node 的绝对路径。不设则回退 PATH 查找；
                   找不到则跳过 S3 交互截图（4.8.3 段）。
                   **S3 不依赖 IIP_CHROME_BIN**：它自己带默认路径
                   （宿主 Chrome 的常见安装位置），需要 node +
                   已装好的 @playwright/test（web/package.json 的
                   devDependency）。宿主 Chrome 装在别处时，
                   给 s3-shots.mjs 的 IIP_CHROME_BIN 传绝对路径即可
"""
import json
import os
import re
import shutil
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
PROJ = os.environ.get("IIP_PROJ", os.path.dirname(HERE))
COMPOSE_FILE = os.path.join(PROJ, "deploy", "docker-compose.yml")

# docker 的定位与环境装配与 scripts/prepull-mirror.py 共用一处实现，
# 避免两个脚本对同一台机器得出不一致的结果。详见 lib_docker.py 的模块注释。
sys.path.insert(0, HERE)
from lib_docker import connect, run as _run  # noqa: E402

# T7.6（2026-10-14）：默认标签随发版切到 0.1.0。与 deploy/docker-compose.yml、
# Makefile 的 IMAGE_TAG、deploy/helm/iip 的 appVersion/tag 四处联动 ——
# 改任何一处而忘了其余，compose 会去找一个并未构建的标签，up 直接失败。
TAG = os.environ.get("IIP_TAG", "0.2.0")
HTTP_PORT = int(os.environ.get("IIP_HTTP_PORT", "8080"))
KEEP = os.environ.get("IIP_KEEP", "") == "1"

# 构建期 Go 模块代理。Dockerfile 的默认值是官方 proxy.golang.org（生产/CI 用），
# 但容器内直连它在国内不通（实测超时），而 goproxy.cn 通。
# 本地验收默认走 goproxy.cn，可用 IIP_GOPROXY 覆盖（如设成官方值验证生产路径）。
GOPROXY = os.environ.get("IIP_GOPROXY", "https://goproxy.cn,direct")

REPORT = os.path.join(os.environ.get("IIP_LOG_DIR", PROJ), "_verify_images.txt")

# 本轮运行的唯一令牌，注入告警的非动态标签值里。
#
# 为什么需要它：去重是按**指纹**判定的，且**没有时间窗**（V1.0 §4.2：同一指纹
# 的 firing 已在库中即 skipped）。指纹又包含全部非动态标签，所以只要标签值
# 逐字相同，本轮告警就会被上一轮留在库里、且仍处于 firing 的行吃掉——
# compose 用的是 named volume，脚本第二次执行时数据仍在。
# 实测：不加令牌时第二轮 POST 返回 skipped:2，落库 0 行，
# 「本轮新增 2 条」的前提不成立，聚合断言必然假失败。
# 故令牌必须**跨进程调用**唯一，而不只是跨轮次唯一。
RUN_TOKEN = time.strftime("%m%d-%H%M%S")

L: list[str] = []
FAILURES: list[str] = []


def w(s: str = "") -> None:
    """写一行到报告并立即落盘，便于外部进程实时观察。"""
    L.append(str(s))
    try:
        with open(REPORT, "w", encoding="utf-8") as fh:
            fh.write("\n".join(L))
    except OSError:
        pass


def check(name: str, ok: bool, detail: str = "") -> bool:
    """记录一条断言。ok=False 累积到 FAILURES，最终决定退出码。"""
    mark = "PASS" if ok else "FAIL"
    w("  [%s] %s%s" % (mark, name, ("  -- " + detail) if detail else ""))
    if not ok:
        FAILURES.append(name)
    return ok


def _stat_int(line: str, key: str) -> int:
    """从 rca.Stats 日志行里取一个计数，解析失败返回 -1。

    **为什么要解析而不是 `in` 判断**：`"unassessable=0" not in line`
    只排除「恰好为 0」，`unassessable=10` 照样通过 —— 那是恒真判据。
    判据要问的是「unassessable > 0」，只有解析出数值才能判。

    解析失败返回 -1（不是 0）：**「解析不出」与「值是 0」是两件事**，
    混成0 会让一条坏日志看起来像「不可评估」。
    """
    m = re.search(key + r"=(\d+)", line)
    return int(m.group(1)) if m else -1


def _stats_total(line: str) -> int:
    return _stat_int(line, "total")


def _stats_unassessable(line: str) -> int:
    return _stat_int(line, "unassessable")


# ------------------------------------------------------------------ docker
DOCKER, ENV = connect()
# cli-plugins 目录的 PATH 装配也在 lib_docker.make_env() 里完成，
# 这里拿到的 ENV 已经能直接跑 `docker compose`。

def run(args: list[str], timeout: int = 900, cwd: str | None = None,
        extra_env: dict | None = None):
    """执行命令，返回 (rc, stdout+stderr)。默认在仓库根执行。

    **extra_env 是叠加而非替换**（2026-10-12 加）：底层 ENV 里含
    lib_docker.make_env() 装配的 cli-plugins PATH，直接整体替换会把
    docker 的 PATH 弄丢。所以这里以 ENV 为底、extra_env 覆盖同名键。

    为什么需要它：S3 段要把自己要的 S3_BASE_URL / S3_REPORT /
    S3_OUT_DIR / IIP_CHROME_BIN 传给子进程。此前它构造了 _env 却
    **从未传给 run()** —— run 的签名里根本没有 env 参数，于是
    「不传就静默用默认值」：报告落到 web/ 下、截图落到
    docs/screenshots/，而判据读的是仓库根，永远读不到。
    症状是「S3 脚本自己报 10 PASS / 0 FAIL，外层却说报告未生成」。
    """
    _e = ENV
    if extra_env:
        _e = dict(ENV)
        _e.update(extra_env)
    return _run(args, _e, timeout=timeout, cwd=cwd or PROJ)


def docker(*args: str, timeout: int = 900):
    assert DOCKER is not None
    return run([DOCKER, *args], timeout=timeout)


def compose(*args: str, timeout: int = 900):
    return docker("compose", "-f", COMPOSE_FILE, *args, timeout=timeout)


# -------------------------------------------------------------------- http
def http_request(url: str, method: str = "GET", data: bytes | None = None,
                 timeout: float = 5.0, headers: dict[str, str] | None = None):
    """发起 HTTP 请求，返回 (status, body)。status 为 None 表示连接层失败。

    M2 的配置接口是 PUT，而 urllib 的 urlopen 默认只发 GET。
    原先只有一个 http_get，无法验证「改配置无需重启即生效」这条 M2 验收——
    故统一为带 method 的实现，GET 保留 http_get 这个薄封装以兼容既有调用。

    headers（S4）：鉴权断言必须带 X-IIP-Token 才能验证「带了头才放行」这一侧。
    只验证「没带头→401」是半个判据 —— 一个「所有请求都 401」的实现同样能通过，
    而真正要区分的是「凭据正确时能通」。
    """
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    # 逐个 add_header 而非一次性设置 dict：Request.add_header 会追加，
    # 而这里要保证调用方传入的头覆盖掉上面那个 Content-Type 默认值。
    # 顺序上先设默认再设调用方的，因此同名以后者为准。
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, (e.read() or b"").decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return None, repr(e)


def http_get(url: str, timeout: float = 5.0):
    return http_request(url, "GET", None, timeout)


def port_in_use(port: int) -> bool:
    """判断 127.0.0.1:port 上是否已有服务在监听。

    注意：这是「能否连上」的探测，用于确认待用端口是否被占用。
    早先这个函数名叫 port_free 但语义相反，容易误读，已改名。

    **超时必须算「未占用」**（2026-10-03 修）：Windows 防火墙会对本机某些
    端口的 SYN 静默丢包，connect() 抛 TimeoutError 而非 ConnectionRefused。
    原实现只 except OSError 就一律判「占用」，于是 8080 因一次
    `netstat` 里只见到 TIME_WAIT 残留（无 LISTEN）却被判为占用，
    整个容器链路 BLOCKED。丢包说明**没有任何进程在那个端口上**——
    真有服务在监听时 connect 一定成功（回 RST 或握手完成）。
    """
    sk = socket.socket()
    sk.settimeout(1.0)
    try:
        sk.connect(("127.0.0.1", port))
        return True
    except (ConnectionRefusedError, socket.timeout, TimeoutError):
        # 连不上 / 超时 = 没人监听。ConnectionRefused 是「明确无人」，
        # 超时是「被丢包」，两者对「能否 bind」而言答案相同：可以 bind。
        return False
    except OSError:
        # 其余 OSError（权限、地址族不匹配等）保守判为占用 ——
        # 宁可误报阻塞，也不要在判断错误时把容器拉起来撞端口。
        return True
    finally:
        sk.close()


def latest_migration_version() -> int:
    """从 migrations/ 目录推断最新迁移号，供断言动态使用。

    为什么不能写死：M0 时代这里断言的是「版本 1」，M1 加 0002、M2 加 0003 之后，
    这条断言就变成「永远失败」的过期断言——输出看着像产品回归，实际是脚本
    没跟上里程碑（scripts/verify-runtime.py 里 OPEN-8 修的是同一个毛病）。
    改为动态读取后，断言随里程碑自动前移，不需要每次发版都回来改这一行。

    读不到目录时返回 0，由调用方按「期望 0 必然不匹配」自然报 FAIL，
    而不是抛异常把整轮验收打断。
    """
    mig_dir = os.path.join(PROJ, "migrations")
    best = 0
    try:
        for fn in os.listdir(mig_dir):
            m = re.match(r"(\d+)_.*\.up\.sql$", fn)
            if m:
                best = max(best, int(m.group(1)))
    except OSError:
        return 0
    return best


def svc_container(service: str) -> str:
    """取 compose 服务的真实容器标识，供 docker exec/logs 使用。

    不要硬编码容器名：compose 的 container_name 与「服务名」经常不同
    （本项目 postgres 服务的实际容器名是 iip-postgres）。
    早先这里写死了 "postgres"，导致两条断言直接报
    `No such container: postgres`，看起来像产品失败、实际是脚本空转。
    取不到时回退到服务名，保持可读的报错。
    """
    rc, out = compose("ps", "-q", service, timeout=60)
    cid = out.strip().splitlines()[0].strip() if rc == 0 and out.strip() else ""
    return cid or service


def compose_ps() -> list[dict]:
    """取 compose 服务状态（JSON 数组）。"""
    rc, out = compose("ps", "--format", "json", timeout=120)
    if rc != 0 or not out:
        return []
    rows = []
    for line in out.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return rows


# ------------------------------------------------------------------- png
def png_color_count(path: str, cap: int = 4096) -> int:
    """返回 PNG 里出现的**不同颜色数**（上限 cap，避免大图耗时过久）。

    为什么要自己解 PNG：判据「截图不是空白页」不能用「文件非空」判断 ——
    一张纯白或纯黑的失败截图同样非空。宿主 Python 没装 Pillow/numpy
    （实测 zlib/struct 可用，PIL 与 numpy 都缺），而为了看一眼截图去
    装依赖不划算，故直接按 PNG 规范解。

    只支持 8bit 的 RGB / RGBA / 灰度 / 带 alpha 的灰度 —— 这覆盖
    Chrome headless 截图的全部实际形态。遇到调色板（PLTE）等类型
    直接抛异常，让调用方按「无法验像素」处理，而不是悄悄返回 0
    （返回 0 会被误读成「纯色空白页」，那是假的 FAIL）。
    """
    import struct
    import zlib

    with open(path, "rb") as fh:
        data = fh.read()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("不是 PNG 文件（魔数不对）")

    # 逐块扫描：IHDR 拿宽高/位深/颜色类型，IDAT 拼接后整体 inflate。
    pos = 8
    w = h = depth = ctype = None
    idat = bytearray()
    while pos + 8 <= len(data):
        (length,) = struct.unpack(">I", data[pos:pos + 4])
        ctag = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + length]
        if ctag == b"IHDR":
            w, h, depth, ctype = struct.unpack(">IIBB", body[:10])
        elif ctag == b"IDAT":
            idat += body
        elif ctag == b"IEND":
            break
        pos += 12 + length

    if w is None or not idat:
        raise ValueError("PNG 缺 IHDR 或 IDAT")
    if depth != 8:
        raise ValueError("只支持 8bit 位深，实测 %s" % depth)
    channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}.get(ctype)
    if channels is None:
        raise ValueError("不支持的颜色类型 %s" % ctype)
    if ctype == 3:
        raise ValueError("调色板 PNG 未支持（无法当作空白页判据）")

    raw = zlib.decompress(bytes(idat))

    # 每行开头有 1 字节 filter type。逐行按 PNG 规范反滤波。
    stride = w * channels
    seen = set()
    prev = bytearray(stride)
    off = 0
    for _y in range(h):
        if off >= len(raw):
            break
        ftype = raw[off]
        off += 1
        line = bytearray(raw[off:off + stride])
        off += stride
        if len(line) < stride:
            break
        if ftype == 1:            # Sub
            for i in range(channels, stride):
                line[i] = (line[i] + line[i - channels]) & 0xFF
        elif ftype == 2:          # Up
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif ftype == 3:          # Average
            for i in range(stride):
                left = line[i - channels] if i >= channels else 0
                line[i] = (line[i] + ((left + prev[i]) >> 1)) & 0xFF
        elif ftype == 4:          # Paeth
            for i in range(stride):
                a = line[i - channels] if i >= channels else 0
                b = prev[i]
                c = prev[i - channels] if i >= channels else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        elif ftype != 0:
            raise ValueError("未知的 filter type %d" % ftype)
        # 抽样：每 4 像素取一个，够判「是否空白」且快。
        for x in range(0, stride, channels * 4):
            seen.add(bytes(line[x:x + channels]))
            if len(seen) >= cap:
                return cap
        prev = line
    return len(seen)


def t75_static_checks() -> None:
    """T7.5 静态判据：不触达 docker daemon，纯文件与源码。

    放在 main() 最前面调用：daemon 不可用时（本机没跑 Docker Desktop）
    这几条仍能跑并落盘 —— 静态回归不该被「环境阻塞」整段掩埋，
    否则「真栈回归后补」会连带把「静态也回归了」这件事悄悄跳过。

    覆盖两类改动：
      A. compose 生产化：7 服务全有 resources + logging 轮转 + restart 非 no；
      B. 采集器 TLS 接线：config flag、装配调用、实现文件三处对得上。
    """
    w("### [0.0] T7.5 静态判据（compose 生产化 + 采集器 TLS 接线）")

    # --- A. compose 生产化 ---
    #
    # 判据形态说明：读 compose 文件本身（不跑 docker compose config ——
    # 那个需要 compose 插件且对 deploy.resources 语义不校验）。
    # 用 PyYAML 解析；**ImportError 时判 FAIL 而非 SKIP**：
    # 静默跳过会让「环境缺库」伪装成「判据通过」，
    # 而本文件其它段早已依赖 docker，多个软依赖没有好处。
    try:
        import yaml
    except ImportError:
        check("PyYAML 可用（compose 静态判据的前置）", False,
              "import yaml 失败；pip install pyyaml 后重跑")
        w()
        return

    with open(COMPOSE_FILE, encoding="utf-8") as f:
        compose_doc = yaml.safe_load(f)

    svcs = (compose_doc or {}).get("services") or {}
    expected = {"postgres", "redis", "prometheus", "alertmanager", "loki",
                "iip-server", "iip-worker"}
    check("compose 七服务齐全", expected <= set(svcs),
          "实际: %s" % sorted(svcs))

    no_limits, no_logging, bad_restart = [], [], []
    for name, svc in sorted(svcs.items()):
        if not isinstance(svc, dict):
            continue
        mem = (((svc.get("deploy") or {}).get("resources") or {})
               .get("limits") or {}).get("memory")
        if not mem:
            no_limits.append(name)
        logging = svc.get("logging") or {}
        if logging.get("driver") != "json-file" or \
                "max-size" not in (logging.get("options") or {}):
            no_logging.append(name)
        if svc.get("restart") in (None, "no"):
            bad_restart.append(name)

    check("compose 七服务全部有 deploy.resources.limits.memory（T7.5）",
          not no_limits, "缺: %s" % (no_limits if no_limits else "无"))
    check("compose 七服务全部有 json-file 日志轮转（T7.5）",
          not no_logging, "缺: %s" % (no_logging if no_logging else "无"))
    check("compose 七服务 restart 策略均显式且非 no",
          not bad_restart, "缺: %s" % (bad_restart if bad_restart else "无"))

    # --- B. 采集器 TLS 接线（T7.5）---
    #
    # 三处必须同时成立，缺一处就是「半接线」：
    #   config 注册 flag（用户能配）→ 装配读取并构造（配了能用）→
    #   实现文件存在（构造真有逻辑）。只 grep 其中一处会发现不了
    # 「flag 在、装配忘了传」这类真实缺陷。
    cfg_go = os.path.join(PROJ, "internal", "config", "config.go")
    main_go = os.path.join(PROJ, "cmd", "iip-worker", "main.go")
    tls_go = os.path.join(PROJ, "internal", "collectors", "tls.go")
    collectors_go = os.path.join(PROJ, "internal", "collectors", "collectors.go")

    def _read(p: str) -> str:
        with open(p, encoding="utf-8") as f:
            return f.read()

    cfg_src, main_src = _read(cfg_go), _read(main_go)
    check("config 注册 evidence-tls-insecure flag（T7.5）",
          '"evidence-tls-insecure"' in cfg_src)
    check("config 注册 evidence-tls-ca-file flag（T7.5）",
          '"evidence-tls-ca-file"' in cfg_src)
    check("worker 装配调用 BuildTLSConfig（T7.5）",
          "collectors.BuildTLSConfig(" in main_src)
    check("worker 装配把 TLS 选项传给采集器（WithTLSConfig，T7.5）",
          "collectors.WithTLSConfig(" in main_src)
    check("internal/collectors/tls.go 存在（BuildTLSConfig 实现，T7.5）",
          os.path.isfile(tls_go))
    if os.path.isfile(tls_go):
        tls_src = _read(tls_go)
        check("BuildTLSConfig 实现含 CA 解析与 insecure 分支（T7.5）",
              "AppendCertsFromPEM" in tls_src and "InsecureSkipVerify" in tls_src)
    if os.path.isfile(collectors_go):
        col_src = _read(collectors_go)
        check("collectors.go 定义 newHTTPClient（T7.5 收敛点）",
              "func newHTTPClient(o options) *http.Client" in col_src)
        # 收敛判据：四个采集器不再各自 `&http.Client{Timeout: ...}`，
        # 全部走 newHTTPClient —— 否则「加 TLS 只改了三个采集器」
        # 这种漏改在静态上看不出来，只在运行时暴露（第四个采集中途
        # 才以 x509 失败的形式出现）。
        # 注意判据必须落在四个采集器文件上：调用在它们那里，
        # collectors.go 自身只有定义（早第一版就把文件读错了，
        # 判据恒假——写判据前先确认字面量会出现在哪）。
        client_files = ["prometheus.go", "loki.go", "trace.go", "k8s.go"]
        missed = [fn for fn in client_files
                  if "newHTTPClient(o)" not in
                  _read(os.path.join(PROJ, "internal", "collectors", fn))]
        check("四个采集器的 client 构造已收敛到 newHTTPClient（T7.5）",
              not missed, "未收敛: %s" % (missed if missed else "无"))
    w()


def main() -> int:
    w("=" * 70)
    w(" IIP v1 镜像与容器验证   " + time.strftime("%Y-%m-%d %H:%M:%S"))
    w("=" * 70)
    w()

    # ---------------------------------------------------------- 0. 前置
    # T7.5 静态判据放最前：daemon 不在时它照样能跑（见函数注释）。
    t75_static_checks()
    w("### [0] 前置检查")
    if not DOCKER:
        w("  FATAL: 找不到 docker 可执行文件")
        w("  hint: 设 IIP_DOCKER_BIN=<docker.exe 绝对路径>，或把 Docker Desktop 的")
        w("        resources\\bin 加入 PATH")
        return 2
    w("  docker = %s" % DOCKER)

    rc, out = docker("info", "--format", "{{.ServerVersion}}|{{.OSType}}|{{.Driver}}",
                     timeout=60)
    if not check("Docker daemon 可用", rc == 0, out[:200]):
        w()
        w("  daemon 不可用时无法继续。常见原因：")
        w("    - Docker Desktop 未启动 / 卡在 'Engine starting'")
        w("    - WSL2 缺失（需管理员启用 Microsoft-Windows-Subsystem-Linux + VirtualMachinePlatform）")
        return 2
    w("  daemon = %s" % out)

    check("端口 %d 未被占用（iip-server 要 bind 它）" % HTTP_PORT, not port_in_use(HTTP_PORT),
          "" if not port_in_use(HTTP_PORT) else "已被占用，先停掉占用进程")
    if port_in_use(HTTP_PORT):
        return 2
    w()

    # ------------------------------------------------------ 1. 构建镜像
    w("### [1] 构建镜像")
    for target, name in [("server", "iip-server"), ("worker", "iip-worker")]:
        image = "%s:%s" % (name, TAG)
        w("  building %s ..." % image)
        t0 = time.time()
        rc, out = docker("build", "--target", target, "-t", image,
                         "--build-arg", "VERSION=" + TAG,
                         "--build-arg", "GOPROXY=" + GOPROXY,
                         ".", timeout=1800)
        dt = time.time() - t0
        check("构建 %s" % image, rc == 0, "耗时 %.1fs" % dt)
        if rc != 0:
            w("  ---- build 输出（末 40 行）----")
            for line in out.splitlines()[-40:]:
                w("    " + line)
            return 3
        # 记录镜像体积（精简镜像的一个可观测指标）
        rc2, out2 = docker("images", image, "--format", "{{.Size}}", timeout=60)
        if rc2 == 0:
            w("        镜像体积 %s" % out2.strip())
    w()

    # -------------------------------------------------------- 2. 起服务
    w("### [2] 启动全部服务（--profile app）")
    rc, out = compose("--profile", "app", "up", "-d", timeout=900)
    check("compose up -d 返回成功", rc == 0, out[:300])
    if rc != 0:
        w("  ---- up 输出（末 40 行）----")
        for line in out.splitlines()[-40:]:
            w("    " + line)
    w()

    # 等健康：iip-server 依赖 postgres healthy，PG 首次启动要初始化数据目录
    #
    # 注意：必须等到「没有任何服务还停在 starting」才算收敛。
    # 早先只等 iip-server=healthy 就 break，会在 alertmanager 仍 starting 时
    # 通过，从而漏掉它随后变成 unhealthy —— 本项目的 alertmanager
    # healthcheck 文案不匹配缺陷就是这样被漏掉的（详见 deploy/docker-compose.yml）。
    w("### [3] 等待服务健康（最多 180s）")
    deadline = time.time() + 180
    last = ""
    ready = False
    states: dict = {}
    while time.time() < deadline:
        rows = compose_ps()
        if rows:
            summary = " ".join(
                "%s=%s" % (r.get("Service", "?"), r.get("Health") or r.get("State", "?"))
                for r in sorted(rows, key=lambda x: x.get("Service", ""))
            )
            if summary != last:
                last = summary
                w("  [%s] %s" % (time.strftime("%H:%M:%S"), summary))
            states = {r.get("Service"): (r.get("Health") or r.get("State")) for r in rows}
            settled = not any(v in ("starting", "created") for v in states.values())
            if states.get("iip-server") == "healthy" \
                    and states.get("iip-worker") == "running" and settled:
                ready = True
                break
        time.sleep(3)

    rows = compose_ps()
    for r in sorted(rows, key=lambda x: x.get("Service", "")):
        w("  %-14s %-10s %s" % (r.get("Service", "?"),
                                r.get("Health") or r.get("State", "?"),
                                r.get("Status", "")))
    states = {r.get("Service"): (r.get("Health") or r.get("State")) for r in rows}
    check("iip-server healthy", ready, "" if ready else "180s 内未达 healthy")

    unhealthy = sorted(s for s, v in states.items() if v == "unhealthy")
    check("无服务处于 unhealthy", not unhealthy,
          "" if not unhealthy else "unhealthy: %s" % ", ".join(unhealthy))

    still_starting = sorted(s for s, v in states.items() if v in ("starting", "created"))
    check("无服务停留在 starting/created", not still_starting,
          "" if not still_starting else "仍在启动: %s" % ", ".join(still_starting))
    w()

    # ---------------------------------------------------- 4. 逐项断言
    w("### [4] 功能断言")

    # postgres 服务的真实容器名是 iip-postgres（来自 compose 的 container_name），
    # 不能直接用服务名，否则 docker exec 会报 No such container。
    PG_C = svc_container("postgres")

    # 4.1 healthz 真查库（server 容器的 /healthz 会 SELECT 1）
    st, body = http_get("http://127.0.0.1:%d/healthz" % HTTP_PORT)
    check("GET /healthz -> 200 ok", st == 200 and body.strip() == "ok",
          "status=%s body=%r" % (st, body[:80]))

    # 4.2 容器内自检模式（--healthcheck 是 distroless 下的唯一探针手段）
    rc, out = docker("exec", "iip-server", "/usr/local/bin/iip-server",
                     "--healthcheck", "--http-addr=:8080", timeout=30)
    check("容器内 --healthcheck 自检退出码 0", rc == 0, out[:160])

    # 4.3 迁移已自动执行（server 启动时会跑 embed 的迁移）
    # 注意：psql 在拼接模式下输出 boolean 为 "false"，在列模式下为 "f"，
    # 两种都算通过，避免因输出格式差异误判。
    # 期望版本不写死，从 migrations/ 动态推断（M2 起为 3）。
    want_ver = latest_migration_version()
    rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                     "-t", "-A", "-c",
                     "SELECT version||'|'||dirty FROM schema_migrations;", timeout=60)
    got = out.strip()
    check("schema_migrations 已到版本 %d 且非 dirty" % want_ver,
          rc == 0 and got in ("%d|false" % want_ver, "%d|f" % want_ver),
          "实际 %r" % got)

    # 4.4 关键表齐全（不数张数，数张数必被下个迁移改红）
    #
    # 这条断言**三易其稿**，值得记下来：
    #   * M2 时是「public schema 表数 = 8」（7 业务 + schema_migrations），
    #     当时的注释还写着「M2 不新增表，别顺手改成 9」。
    #   * M3 的 0004 **新增了 review_suppressions** → 张数变 9，
    #     那句警告就此失效，但断言没跟着改，容器链路从 M3 起一直红着。
    #   * 现在改成断言**关键表名单齐全**，加迁移不会再无谓变红；
    #     真要少表（迁移漏跑 / 迁移失败）仍然会被抓到。
    #
    # 为什么不用「>= N」糊弄：少表与多表同样是故障，而 >= 对多表完全免疫。
    for tbl in ("schema_migrations", "alerts", "incidents", "incident_alerts",
                "correlation_rules", "timeline", "evidence", "review_suppressions"):
        rc_t, out_t = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                             "-t", "-A", "-c",
                             "SELECT count(*) FROM information_schema.tables "
                             "WHERE table_schema='public' AND table_name='%s';" % tbl,
                             timeout=60)
        check("关键表 %s 存在" % tbl, rc_t == 0 and out_t.strip() == "1",
              "count=%s" % out_t.strip())

    # ------------------------------------------- 4.5 M2：关联引擎（容器内语义验收）
    # 把《IIP-M2详细开发手册》§八 的 M2 完成定义搬到容器里复验：
    # 镜像内嵌的迁移确实建好了 needs_review 与 default 配置行，
    # 且「改配置无需重启 → 下一条告警按新阈值决策」这条主链路真的跑通。
    #
    # 为什么必须验到语义层：M1 的容器验收只到「进程活着 + 表存在」，
    # 那对 M2 是不够的——关联引擎的全部价值在决策行为上，
    # 进程健康但一条告警都不聚合，也能骗过纯存活探针。
    def sql_val(sql: str) -> str:
        rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                         "-t", "-A", "-c", sql, timeout=60)
        return out.strip() if rc == 0 else ""

    def db_count(table: str) -> int:
        v = sql_val("SELECT count(*) FROM %s;" % table)
        return int(v) if v.isdigit() else -1

    # 4.5.0 干净起点：抹掉 named volume 里历次验收留下的行。
    #
    # compose 走 named volume 且 down 不加 -v（M2 有意保数据便于排查），
    # 于是**第二次**执行本脚本时，上一轮的告警/事件仍在库里：
    #   * 4.5.10 依赖维口径要求「historical = 0（无共现历史）」，脏历史会把该维从 0
    #     喂到 ≈0.53，使跨服务直接边由 0.7133 抬到 0.7667 → 断言假 FAIL。
    #     2026-10-02 实测：残留 12 条告警时得 0.7667，清空后精确回到 0.7133。
    #   * 4.5.7 / 4.5.8 这类「本轮新增 N 条」的计数断言同样会被历史行带偏。
    # 只清本脚本会写的业务表；schema_migrations 与 correlation_rules 保留
    #（4.5.3 依赖其中的 default 种子行）。
    # CASCADE 不可省：incident_alerts 对 alerts / incidents 有外键。
    #
    # **tasks 与 evidence（M4 新表）必须一并清**：它们跨轮残留会造成两种假通过 ——
    #   * 残留的 done 任务让「本轮任务已全部完成」在 worker 还没领过时就成立；
    #   * 残留的 evidence 让「事件带 ≥N 条证据」在采集器根本没工作时也成立。
    # 两者都是「数字对，机制错」，比直接 FAIL 更坏。
    _rc, _out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                       "-t", "-A", "-c",
                       "TRUNCATE TABLE incident_alerts, alerts, incidents, "
                       "correlation_feedback, tasks, evidence, timeline CASCADE;",
                       timeout=60)
    check("干净起点：已清空历次验收残留行", _rc == 0,
          "实际 rc=%d %s" % (_rc, _out[:200]))
    _a, _i, _ia = db_count("alerts"), db_count("incidents"), db_count("incident_alerts")
    check("干净起点：alerts/incidents/incident_alerts 计数均为 0",
          _a == 0 and _i == 0 and _ia == 0,
          "alerts=%d incidents=%d incident_alerts=%d" % (_a, _i, _ia))

    # 4.5.1 needs_review 列：必须 NOT NULL + 默认 false（REVIEW 档落库的前提）
    got = sql_val("SELECT is_nullable||'|'||coalesce(column_default,'(无)') "
                  "FROM information_schema.columns WHERE table_schema='public' "
                  "AND table_name='incident_alerts' AND column_name='needs_review';")
    check("incident_alerts.needs_review 列存在且 NOT NULL DEFAULT false",
          got == "NO|false", "实际 %r" % got)

    # 4.5.2 部分索引：只索引 needs_review=TRUE 的行，供 M3 确认队列扫描
    got = sql_val("SELECT count(*) FROM pg_indexes WHERE schemaname='public' "
                  "AND indexname='idx_incident_alerts_needs_review';")
    check("needs_review 部分索引已建立", got == "1", "实际 %r" % got)

    # 4.5.3 default 配置行：五维权重与阈值必须逐位等于 V1.0 §5 锁定值
    # 注：JSONB 内 numeric 输出为最简形式（0.20 -> 0.2），故显式 to_char 归一为两位小数，
    #     保证断言比对的是「数值相等」而非「文本恰好同形」，且仍为精确相等比较。
    #     FM 前缀不可省：不带 FM 时 to_char 会为负号预留一格，输出 ' 0.25'（前导空格）。
    got = sql_val(
        "SELECT name||'|'||to_char((rule_config->'weights'->>'time')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'service')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'label')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'dependency')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'weights'->>'historical')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'thresholds'->>'merge')::numeric,'FM0.00')||'|'"
        "||to_char((rule_config->'thresholds'->>'review')::numeric,'FM0.00')||'|'||enabled "
        "FROM correlation_rules WHERE name='default';")
    check("correlation_rules 有 default 行且权重/阈值为 V1.0 §5 锁定值",
          got == "default|0.25|0.25|0.20|0.20|0.10|0.80|0.50|true",
          "实际 %r" % got)

    cfg_url = "http://127.0.0.1:%d/api/v1/correlation/config" % HTTP_PORT
    hook_url = "http://127.0.0.1:%d/api/v1/alerts/webhook" % HTTP_PORT

    # 4.5.4 GET 配置：默认值须与迁移种子一致（证明读写走的是同一份真相）
    st, body = http_get(cfg_url)
    ok_get = False
    if st == 200:
        try:
            cfg = json.loads(body)
            wts, ths = cfg.get("weights", {}), cfg.get("thresholds", {})
            ok_get = (wts.get("time") == 0.25 and wts.get("service") == 0.25
                      and wts.get("label") == 0.20 and wts.get("dependency") == 0.20
                      and wts.get("historical") == 0.10
                      and ths.get("merge") == 0.80 and ths.get("review") == 0.50
                      and cfg.get("enabled") is True)
        except (json.JSONDecodeError, AttributeError):
            pass
    check("GET /api/v1/correlation/config -> 200 且为默认权重/阈值", ok_get,
          "status=%s body=%s" % (st, body[:160]))

    # 4.5.5 非法配置必须 422：静默接受会让「可调」形同虚设
    st, body = http_request(cfg_url, "PUT", b'{"weights":{"time":2}}')
    check("PUT 非法权重(time=2) -> 422", st == 422,
          "status=%s body=%s" % (st, body[:120]))

    # 4.5.6 未知字段必须 400（ADR-010）：键名打错被静默忽略是最难排查的一类故障
    st, body = http_request(cfg_url, "PUT", b'{"threshods":{"merge":0.9}}')
    check("PUT 未知字段(threshods 拼错) -> 400", st == 400,
          "status=%s body=%s" % (st, body[:120]))

    def post_alerts(run: str) -> tuple:
        """投递两条同服务(checkout)告警，返回 (status, body)。

        两条告警做成「B = A 再多一个非动态标签 shard」，两个原因：
        * A 与 B 的指纹必须不同，否则 B 会被当成重复投递而跳过（skipped），
          落库只剩 1 行，聚合断言就白测了。指纹计算会剔除
          instance/pod/container/node，所以不能靠 instance 区分，必须用非常规键。
        * A 与 B 的非动态标签集合只差一个键 → 标签维 Jaccard ≈ 0.83，
          叠加时间/服务两维满分，总分稳在 0.66 以上（最差也是 REVIEW，
          而 REVIEW 仍会链接到候选 Incident），故「有没有被关联」这条断言
          不依赖拓扑文件是否存在。
          （ADR-013 后拓扑已挂载：这一对**同服务**告警会经共同邻居 payment
           额外拿到依赖维 0.5，总分由 0.6667 升到 0.7667 —— 这不是回归。
           本处只用它判「走没走关联档」，故不因该变化调整断言；
           依赖维自身的取证见 4.5.10，那里用的是鉴别力更强的直接边。）

        run 给每轮一个互不相同的非动态标签值。若两条用完全一样的标签，
        后一条会被前一条的去重逻辑吃掉（实测 skipped:1），
        「本轮新增 2 条」的前提就不成立——这是第一版实测踩到的坑。

        注意 run 还必须带上 RUN_TOKEN：跨**轮次**唯一不够，还得跨**进程调用**唯一，
        否则第二次执行本脚本时，上一轮留在 named volume 里的 firing 行会把
        本轮告警全部吃掉（实测 skipped:2、落库 0 行）。详见 RUN_TOKEN 处注释。
        """
        labels_a = {"alertname": "HighLatency", "service": "checkout",
                    "namespace": "production", "severity": "critical",
                    "run": run}
        labels_b = dict(labels_a, shard="s1")
        alerts = [{"status": "firing", "labels": lb,
                   "annotations": {"summary": "p99 超阈"},
                   "startsAt": "2026-10-01T00:00:00Z",
                   "endsAt": "0001-01-01T00:00:00Z"}
                  for lb in (labels_a, labels_b)]
        payload = json.dumps({"version": "4", "status": "firing",
                              "groupKey": "iip-m2-container-check",
                              "receiver": "iip-webhook", "alerts": alerts},
                             ensure_ascii=False).encode("utf-8")
        return http_request(hook_url, "POST", payload)

    def max_alert_id() -> int:
        v = sql_val("SELECT coalesce(max(id),0) FROM alerts;")
        return int(v) if v.isdigit() else -1

    def links_of_new_alerts(after_id: int) -> str:
        """统计「比 after_id 更新的告警」的关联边形态。

        返回 '关联边总数|带分数的条数|涉及的事件数'；查询失败时返回空串。

        列名是 correlation_score（0001_init 里定的），**不是** score：
        第一版这里写成 score，SQL 直接报错、sql_val 返回空串，
        于是「断言失败」看着像产品缺陷、实为脚本自己写错了列名。
        故这里保留空串这个可辨识的失败形态，并在断言信息里点明，
        避免把脚本 bug 误读成产品 FAIL。
        """
        return sql_val("SELECT count(*)||'|'||count(correlation_score)||'|'||"
                       "count(DISTINCT incident_id) FROM incident_alerts "
                       "WHERE alert_id > %d;" % after_id)

    def bad_sql_note(got: str) -> str:
        return "SQL 查询失败（脚本问题，非产品缺陷）" if got == "" \
            else "实际 %r" % got

    # 4.5.7 先把两个阈值一起抬到 0.99 = 等价于「关闭聚合」。
    #
    # 必须**同时**抬 review：只抬 merge 的话，分数落在 [0.50, merge) 的告警
    # 会走 REVIEW 档——按设计 REVIEW 仍然链接到候选 Incident（只是打标待确认），
    # 于是「不聚合」的预期不会成立，断言会误判成产品缺陷。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.99,"review":0.99}}')
    check("PUT merge/review=0.99 -> 200（模拟关闭聚合）", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    aid0 = max_alert_id()
    st, body = post_alerts("phase-a-" + RUN_TOKEN)
    check("POST 2 条同服务告警 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 阈值 0.99 时两条都不该被关联，各自新建 Incident：
    # 关联边 2 条、带分数的 0 条 —— 分数为空即 NEW 档（新建不产生相似度）。
    got = links_of_new_alerts(aid0)
    check("阈值 0.99：本轮 2 条告警均为 NEW 档（关联边 2 条 / 带分数 0 条）",
          got.startswith("2|0|"), bad_sql_note(got) + "（期望 2|0|*）")

    # 4.5.8 还原默认阈值 → 同样两条告警应当被关联。阈值是**即时**生效的，
    # 中途没有重启任何容器，这正是手册 §八「改配置后无需重启」的验收。
    st, body = http_request(cfg_url, "PUT",
                            b'{"thresholds":{"merge":0.80,"review":0.50}}')
    check("PUT 还原 merge=0.80/review=0.50 -> 200", st == 200,
          "status=%s body=%s" % (st, body[:120]))

    aid1 = max_alert_id()
    st, body = post_alerts("phase-b-" + RUN_TOKEN)
    check("POST 2 条同服务告警（阈值还原后）-> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 阈值还原后两条都应被关联到候选 Incident（走 REVIEW 或 MERGE）：
    # 关联边 2 条、带分数的 2 条 —— 分数非空即「关联档」，这就是恢复聚合的证据。
    #
    # 这里刻意**不**断言「incidents 只增 1」：本轮告警与上一轮同服务，
    # 会被关联到上一轮遗留的 open Incident 上（incidents +0），
    # 具体增减取决于库里的候选分布——断言它就等于把断言绑死在偶然状态上，
    # 而「有没有走关联档」才是配置热生效真正的判据。
    got = links_of_new_alerts(aid1)
    check("阈值还原：本轮 2 条告警均被关联（关联边 2 条 / 带分数 2 条）",
          got.startswith("2|2|"), bad_sql_note(got) + "（期望 2|2|*）")

    # 4.5.9 needs_review 与关联分数区间必须自洽：
    # 只有 REVIEW 档（0.50 ≤ score < 0.80）才允许打标。这条不变量能同时抓住
    # 「REVIEW 忘了打标」和「MERGE 被误打标」两个方向上的缺陷。
    got = sql_val("SELECT count(*) FROM incident_alerts "
                  "WHERE correlation_score IS NOT NULL "
                  "AND needs_review <> (correlation_score >= 0.50 "
                  "AND correlation_score < 0.80);")
    check("needs_review 与关联分数区间自洽（仅 REVIEW 档打标）", got == "0",
          bad_sql_note(got))

    # 4.5.10 依赖维必须真正参与评分（ADR-013：拓扑改由 compose 挂载，不再缺失）。
    #
    # 判据分两层，缺一不可：
    #   ① 启动日志不再出现「服务拓扑为空」/「加载服务拓扑失败」——证明文件被读到且解析成功；
    #      只做 ① 会漏掉「文件在、内容却没用上」。
    #   ② 分数里确实含依赖维——靠一对**跨服务但有直接依赖边**的告警取证。
    #
    # 分数口径（V1.0 §5；权重 time .25 / service .25 / label .20 / dependency .20 / historical .10）：
    #   time       = 0.25   （两条 startsAt 相同、endsAt 均为 NULL → 按 now 兜底，窗口重叠比 1.0）
    #   service    = 0.15   （不同 service 但同 namespace → 0.6 × 0.25）
    #   label      = 0.1333 （k=v 对做 Jaccard：只差 service=... 一对，交集 4 / 并集 6 → 0.6667 × 0.20）
    #   dependency = 0.18   （checkout↔payment 是拓扑里的直接边 → 0.9 × 0.20）  <-- 拓扑缺失时此项为 0
    #   historical = 0      （run 标签带 RUN_TOKEN，指纹每次全新，无共现历史）
    #   合计 ≈ 0.7133；**无拓扑时上限 ≈ 0.5333** —— 故 0.70 这条线只有依赖维真参与才过得去。
    rc, out = docker("logs", "--tail", "200", "iip-server", timeout=60)
    # 优先做**正向**断言：日志是 slog JSON，结构化属性渲染成 "services":4 这种键值。
    # （M2 起成功路径不出声，只能靠「没看到 WARN」反推；ADR-013 给 main.go 补了
    #  成功日志，验收才得以写成正向证据。）
    loaded = re.search(r'"msg":"服务拓扑已加载".*?"services":(\d+)', out)
    check("server 日志出现「服务拓扑已加载」且 services>=4",
          loaded is not None and int(loaded.group(1)) >= 4,
          # 中性措辞：detail 会被无条件拼到 PASS 行上（check() 不区分成败），
          # 写成「未见…」会在归档报告里留下 `[PASS] … -- 未见该 INFO`。
          ("services=%s" % loaded.group(1)) if loaded
          else "无「服务拓扑已加载」行；失败时看下方 w() 输出的 server 日志")
    if not loaded:
        w("  server 日志里没有「服务拓扑已加载」，若判据红请查"
          "容器内 config/topology.yaml 是否被挂载/是否为空")
    check("server 日志不含「服务拓扑为空」", "服务拓扑为空" not in out,
          "该 WARN 与上一条 INFO 不应同时出现")
    check("server 日志不含「加载服务拓扑失败」", "加载服务拓扑失败" not in out,
          "出现该 WARN → 文件在但内容无法解析")

    def post_dep_pair(run: str) -> tuple:
        """投两条**跨服务、且拓扑里有直接边**的告警（checkout / payment）。

        与上面的 post_alerts 刻意不同：那对是同服务(checkout)，在拓扑存在时
        只能靠「共同邻居 payment」拿到 0.5；本函数要的是**直接边**那 0.9，
        鉴别力更强。故两条 service 不同、且不加 shard —— 让标签维恰好只差
        service 一对，分数可以精确预测（见 4.5.10 的推导）。
        """
        base = {"alertname": "LatencyHigh", "namespace": "production",
                "severity": "critical", "run": run}
        alerts = [{"status": "firing", "labels": dict(base, service=svc),
                   "annotations": {"summary": "依赖维取证"},
                   "startsAt": "2026-10-01T00:00:00Z",
                   "endsAt": "0001-01-01T00:00:00Z"}
                  for svc in ("checkout", "payment")]
        payload = json.dumps({"version": "4", "status": "firing",
                              "groupKey": "iip-l2-topology-check",
                              "receiver": "iip-webhook", "alerts": alerts},
                             ensure_ascii=False).encode("utf-8")
        return http_request(hook_url, "POST", payload)

    aid2 = max_alert_id()
    st, body = post_dep_pair("topo-" + RUN_TOKEN)
    check("POST 2 条跨服务告警(checkout/payment) -> 200", st == 200,
          "status=%s body=%s" % (st, body[:160]))

    # 只取 payment 那一条的关联分数：它的最优匹配就是同批的 checkout 告警。
    # 用 run 标签精确定位（带 RUN_TOKEN，跨进程唯一），避免被其它轮次的行干扰。
    got = sql_val("SELECT coalesce(round(max(ia.correlation_score)::numeric,4)::text,'-') "
                  "FROM incident_alerts ia JOIN alerts a ON a.id = ia.alert_id "
                  "WHERE a.labels->>'run' = 'topo-%s' AND a.labels->>'service' = 'payment';"
                  % RUN_TOKEN)
    try:
        dep_score = float(got)
    except ValueError:
        dep_score = -1.0
    check("依赖维参与评分：跨服务直接边得分落在 [0.70, 0.73)", 0.70 <= dep_score < 0.73,
          "实际 %s（期望 ≈0.7133；拓扑缺失时上限 ≈0.5333）%s"
          % (got, bad_sql_note(got)))
    w()

    got = sql_val("SELECT coalesce(round(max(correlation_score)::numeric,4)::text,'-') "
                  "FROM incident_alerts;")
    w("        本次最高关联得分 = %s" % got)
    w("        incidents 总数 = %d, incident_alerts 总数 = %d"
      % (db_count("incidents"), db_count("incident_alerts")))
    w()

    # 4.6 worker 心跳（60s 周期，需等一轮）
    w("  等待 worker 心跳（最长 90s）...")
    rc, out = docker("logs", "--tail", "40", "iip-worker", timeout=60)
    got_beat = "worker alive" in out
    if not got_beat:
        deadline = time.time() + 90
        while time.time() < deadline:
            time.sleep(15)
            rc, out = docker("logs", "--tail", "40", "iip-worker", timeout=60)
            if "worker alive" in out:
                got_beat = True
                break
    hb = re.search(r"worker alive, poll_count=(\d+)", out)
    check("worker 输出心跳日志", got_beat,
          # 中性措辞（同上：detail 无条件拼到 PASS 行）。
          ("poll_count=%s" % hb.group(1)) if hb
          else "90s 窗口内未匹配到心跳行；失败时看下方 worker 日志")
    if not got_beat:
        w("  worker 在 90s 内未输出心跳，若判据红请查上面的 worker 日志"
          "（区分「进程死了」与「活着但不打印」）")

    check("worker 已连上 PostgreSQL", "已连接 PostgreSQL" in out)
    w()

    # ============================================ 4.7 M4：证据采集（容器内语义验收）
    # 手册 §七 要求「容器链路断言数从 41 起步（新增项见 §八）」。
    # 本段把 M4 的六条判据里**能在容器内判定的**五条搬过来，
    # 判据 3（宕机零阻塞）需要停容器，属破坏性流程，不在本脚本内做。
    #
    # 为什么必须在容器里验而不能靠真库门禁：真库门禁用的是**本地裸跑的二进制**，
    # 而容器链路验的是**镜像里的二进制 + compose 注入的环境变量**。
    # M4 的采集器地址全部来自环境变量，一旦 compose 漏给（本次就漏了
    # IIP_PROM_URL / IIP_LOKI_URL），裸跑测试全绿而容器里 evidence 恒空。
    # 这类缺陷只有容器链路能抓到。
    w("### [4.7] M4 证据采集链路")

    # --- 4.7.0 先给数据源造数，否则采集器「查对了但查不到东西」 ---
    #
    # 为什么必须造：容器里的 Prometheus 只抓自己（prometheus.yml 的
    # scrape_configs 仅 prometheus 自抓取），没有 http_requests_total；
    # Loki 则一条日志都没有。采集器逻辑是对的，但数据源是空的，
    # evidence 必然为 0 条 —— 这是**环境缺失**，不是产品缺陷。
    # 宿主机链路（verify-func.py）用进程内假 HTTP 服务造这批数据，
    # 容器链路不能用替身（那等于把要验的东西替换掉了），故灌进真 Loki。
    #
    # 只灌 Loki 不灌 Prometheus：Prometheus 没有 push API，要造
    # http_requests_total 得给它加 exporter 容器，代价远大于收益。
    # Prometheus 侧的正确性由 verify-func.py 的真实进程闭环覆盖，
    # 此处只保证「Loki 采集器在容器内确实能采到东西」。
    _LOKI_PORT = int(os.environ.get("IIP_LOKI_PORT", "3100"))
    _now_ns = time.time_ns()
    # 造 6 类不同错误模式 × 每类 3 条。TopN 默认 10，6 类全保留，
    # 再加 1 条「错误量突增」= 7 条证据，超过手册判据 1 的 ≥5 条。
    #
    # 模式文本刻意各不相同：normalizeLogLine 会把数字抹掉，
    # 若 6 条只差数字，会被聚合成 1 类，TopN 就只剩 2 条证据。
    _PATTERNS = [
        "error: connection refused by upstream",
        "error: failed to acquire distributed lock",
        "panic: runtime index out of range",
        "fatal: unrecoverable checkpoint corruption",
        "exception: payment gateway timeout",
        "failed to commit transaction on replica",
    ]
    _streams = []
    for _pi, _pat in enumerate(_PATTERNS):
        _vals = []
        for _k in range(3):
            # 每条间隔 1ms，且都落在「现在」附近 —— Loki 拒收过旧样本。
            _ts = _now_ns - (len(_PATTERNS) - _pi) * 10_000_000 + _k * 1_000_000
            _vals.append([str(_ts), "%s (attempt=%d)" % (_pat, _k + 1)])
        _streams.append({"stream": {"service": "container-check",
                                    "source": "iip-verify"},
                         "values": _vals})
    rc_push, body_push = http_request(
        "http://127.0.0.1:%d/loki/api/v1/push" % _LOKI_PORT,
        "POST", json.dumps({"streams": _streams}).encode(), timeout=15)
    check("M4 已向容器内真 Loki 灌入错误日志（造数前置）",
          rc_push in (200, 204),
          "rc=%s body=%s" % (rc_push, (body_push or "")[:160]))

    # push 返回成功不等于立刻可查：Loki 的 tsdb store 需要落盘。
    # 这里主动查一次，把「造数本身失败」与「采集器采不到」分开 ——
    # 否则证据为 0 时无法判断是环境没准备好还是产品坏了。
    _deadline = time.time() + 30
    _seen = "0"
    _q = urllib.parse.quote('{service="container-check"}')
    while time.time() < _deadline:
        rc_q, body_q = http_request(
            "http://127.0.0.1:%d/loki/api/v1/query_range?query=%s&limit=100"
            % (_LOKI_PORT, _q), "GET", None, timeout=10)
        if rc_q == 200:
            try:
                _seen = str(sum(len(s.get("values", []))
                                for s in json.loads(body_q).get("data", {})
                                .get("result", [])))
            except Exception:  # noqa: BLE001
                _seen = "0"
            if _seen != "0":
                break
        time.sleep(2)
    # **不断言绝对条数**：Loki 数据在 named volume 里跨轮累加
    #（compose down 不加 -v），第 2 轮起总数就是 36、54……
    # 写死「期望 18」会让报告出现「实测 36（期望 18）」这种
    # 看着像缺陷、实则正常的行。只判「本轮灌的确实可见」——
    # 即至少 18 条（本轮灌入量），与历史残留无关。
    _MIN_VISIBLE = len(_PATTERNS) * 3   # 本轮灌入量：6 类 x 3 条
    check("M4 灌入的日志可被 query_range 查到（造数真的生效）",
          _seen.isdigit() and int(_seen) >= _MIN_VISIBLE,
          "容器内 Loki 可见日志条数=%s（本轮灌入 %d 条；"
          "总量含历史残留，逐轮累加，故不比对绝对值）"
          % (_seen, _MIN_VISIBLE))

    # --- 4.7.1 M4 的表与索引确实在镜像的迁移里 ---
    for tbl in ("tasks", "evidence"):
        got = sql_val("SELECT count(*) FROM information_schema.tables "
                      "WHERE table_schema='public' AND table_name='%s';" % tbl)
        check("M4 关键表 %s 存在" % tbl, got == "1", "实际 %r" % got)

    # 4.7.2 窗口游标列（0007）：少这一列则第二轮采集会从事故起点重采，
    # 证据表出现两份完全相同的数据——而那不会报错，只会让证据表无声膨胀。
    got = sql_val("SELECT count(*) FROM information_schema.columns "
                  "WHERE table_schema='public' AND table_name='tasks' "
                  "AND column_name='window_end';")
    check("M4 窗口游标列 tasks.window_end 存在（0007）", got == "1",
          "实际 %r" % got)

    # 4.7.3 预算索引（0006）：证据列表按 created_at DESC, id DESC 排序，
    # 0005 建的 (incident_id, type) 不含排序列，用不上。
    got = sql_val("SELECT count(*) FROM pg_indexes WHERE schemaname='public' "
                  "AND indexname='idx_evidence_incident_created';")
    check("M4 证据预算索引 idx_evidence_incident_created 已建立（0006）",
          got == "1", "实际 %r" % got)

    # 4.7.4 采集器真的注册了 —— **本段最关键的一条**。
    #
    # 判据是「日志里没有『未配置地址，已跳过注册』」。反向断言（找 WARN）
    # 会漏掉「文件在、内容却没用上」的情况，这里绑的是**明确的降级文案**。
    #
    # 这条断言是本次补 compose 时才有的：worker 原本只拿到 IIP_PG_URL，
    # 于是 prometheus / loki 两个采集器都被跳过，worker 照常 healthy、
    # 照常心跳，而 evidence 表恒空——所有 M1~M3 的断言照样全绿。
    rc_l, out_l = docker("logs", "--tail", "200", "iip-worker", timeout=60)
    skipped = [ln.strip() for ln in out_l.splitlines()
               if "跳过注册" in ln]
    check("M4 worker 未出现「采集器跳过注册」（Prom/Loki 已配地址）",
          not skipped, "实际跳过: %s" % (skipped[:3] if skipped else "无"))

    reg = [ln.strip() for ln in out_l.splitlines() if "已注册" in ln]
    check("M4 worker 日志明确列出已注册采集器", bool(reg),
          "实际: %s" % (reg[:3] if reg else "无"))

    # 4.7.4b M6：trace 采集器在 compose 里**故意不配**，因此必须是
    # 「已禁用」而不是「未配置地址已跳过注册」。
    #
    # 为什么要单独断言这条：这两个文案在页面上看起来都像「没装trace」，
    # 但它们指向的排查方向完全不同 ——
    #   - 「已禁用」= 没开关，属预期（M6 §5 降级裁定，本仓库不提供后端）；
    #   - 「跳过注册」= 开关开了但没给地址，属漏配。
    # 反过来，若哪天有人给 compose 加了 IIP_TRACE_URL 却忘了开关，
    # 这里会看到「跳过注册」而容器链路本该发现的缺陷没被发现。
    # 不绑这个区分，compose 的注入错误就会静默通过全链路。
    _trace_off = [ln.strip() for ln in out_l.splitlines()
                  if "trace 采集器已禁用" in ln]
    # detail 一律**中性措辞**：check() 会把它无条件拼到 PASS 行上
    # （见其定义），写成「未见到…」这种失败时才该出现的句子，
    # 归档报告会印出 `[PASS] … -- 未见到禁用日志` 这种自相矛盾的行，
    # 下一个读的人会以为判据没验。失败时的具体缺失由 w() 另起一行给出。
    check("M6 trace 采集器在 compose 下为「已禁用」（无后端属预期）",
          bool(_trace_off),
          "已匹配禁用日志 %d 条；若为「跳过注册」则说明开关与地址不匹配"
          % len(_trace_off))
    _trace_reg = [ln.strip() for ln in out_l.splitlines()
                  if "trace 采集器已注册" in ln]
    check("M6 trace 采集器未在无后端时被注册（不得出现死配置）",
          not _trace_reg,
          "已确认无注册日志" if not _trace_reg
          else "实际注册日志: %s" % (_trace_reg[:2]))
    if _trace_reg:
        w("  !! compose 下不该有 trace 注册日志，实际有：%s" % _trace_reg[:2])

    # 4.7.4d T7.5：**镜像里的 worker 二进制**真的暴露 TLS flag。
    #
    # 与 0.0 段的源码 grep 分工：那条证明「装配接线在源码里」，
    # 这条证明「打进取证镜像的二进制认得这两个 flag」——
    # 容器链路验的正是镜像内二进制，而 flag 注册在 config.Load 里，
    # 漏注册（比如只改了 config 结构体忘了 fs.Bool）时源码 grep 全绿
    # 而二进制压根不认这两个环境变量。
    #
    # 探针形态：docker exec 跑 `--help`（distroless 无 shell，
    # exec 直接指定二进制路径即可，不需要 sh -c）。
    _wc = svc_container("iip-worker")
    rc_h, out_h = docker("exec", _wc, "/usr/local/bin/iip-worker",
                         "--help", timeout=60)
    _tls_flags = [ln.strip() for ln in out_h.splitlines()
                  if "evidence-tls" in ln]
    check("worker 二进制 --help 暴露 evidence-tls-insecure / evidence-tls-ca-file（T7.5）",
          rc_h == 0 and len(_tls_flags) >= 2,
          "匹配到 %d 条 evidence-tls flag 行" % len(_tls_flags))


    # 4.7.4c M6：**开关打开时真二进制会注册**（验「启用路径」而非只验禁用路径）。
    #
    # 上面两条只证明「默认关闭时不注册」—— 那是**半个判据**：
    # 一个「读了开关但注册分支没接线」的实现，同样能让那两条全绿。
    # 而 cmd/iip-worker 的 TestBuildRegistry 是**手工构造 cfg** 再调
    # buildRegistry()，它证明不了**真实二进制真的认
    # IIP_EVIDENCE_TRACE_ENABLED 这个环境变量名** ——
    # 而「认得并注册」正是「让 trace 不成为死代码」的全部意义。
    #
    # 探针形态：用已构建的 worker 镜像跑一次，开关打开 + trace 地址故意打不通，
    # 断言它照样打印「trace 采集器已注册」。
    # 采不到数据是**预期**（本仓库不附带后端，M6 §五降级裁定）；
    # 但「压根没注册」是缺陷。这两件事必须分开，否则会为了让它变绿
    # 而去造一个假后端 —— 那是「把判据改成恒真」。
    #
    # **探针必须能连上真PG**：main.go 里 store.Connect（第 59 行）在
    # buildRegistry（第 66 行）**之前**，连不上库进程就退出了，
    # 注册日志根本不会打出来。
    # （初版这里写反了顺序、且用 127.0.0.1，实测报错停在
    #  「连接数据库失败 ... dial tcp 127.0.0.1:5432」——
    #  那种红是探针没配环境，不是产品缺陷。故接compose 网络用真库。）
    #
    # 用**镜像名** iip-worker:<TAG>，不是容器名 —— docker run 只认镜像。
    # 先查镜像在不在：不存在时如实判 FAIL，而不是让 docker run 报错后
    # 继续往下跑（那会把后面的断言结果搅乱，且分不清是环境问题还是产品问题）。
    _wimg = "iip-worker:%s" % TAG
    _rc_img, _out_img = docker("image", "inspect", _wimg, timeout=60)
    if _rc_img != 0:
        check("M6 trace 采集器在开关打开时真注册（环境变量名被真二进制读到）",
              False,
              "worker 镜像 %s 不存在，无法验启用路径。先构建："
              "docker build --target worker -t %s ." % (_wimg, _wimg))
    else:
        # 取 PG 容器所在网络 + **从 compose 读真实的 IIP_PG_URL**。
        # 两者都不硬编码：
        #   网络 —— compose down 后会被删，下次跑必须重新取；
        #   连接串 —— 密码在 compose 里（POSTGRES_PASSWORD 与它必须一致，
        #     改了一处漏另一处时，探针会以「password authentication failed」
        #     报错，而那会被误读成产品缺陷。实测踩过：先抄成 `postgres:postgres`
        #     才想起来 compose 里是 `dev`。
        _net = ""
        _rc_net, _out_net = docker(
            "inspect", "-f",
            "{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}",
            PG_C, timeout=60)
        if _rc_net == 0 and _out_net.strip():
            _net = _out_net.strip().splitlines()[0].strip()

        _pgurl = ""
        _rc_cfg, _out_cfg = compose("config", timeout=120)
        if _rc_cfg == 0:
            _m = re.search(r"IIP_PG_URL:\s*[\"']?([^\"'\s]+)", _out_cfg)
            if _m:
                _pgurl = _m.group(1)

        if not _net or not _pgurl:
            check("M6 trace 采集器在开关打开时真注册（环境变量名被真二进制读到）",
                  False,
                  "取不到 compose 网络(%r)或 IIP_PG_URL(%r)，探针连不上库则"
                  "注册日志不会打印，本断言无法成立"
                  % (_net or '空', '取到' if _pgurl else '空'))
        else:
            # **输出必须落文件，不能 capture_output**：
            # 探针注册完不会自己退出（它去轮询任务、连那个打不通的 trace 地址），
            # 而 capture_output 会把输出缓冲在管道里 —— 一旦超时被杀，
            # 缓冲内容全丢，断言只看到「超时」而看不到「为什么」。
            # （实测 rc=-1 TimeoutExpired、输出 0 字符。）
            #
            # 用**具名容器 + 不用 --rm**：超时后进程被杀，但容器还在，
            # 可以用 docker logs 把完整启动日志捞出来 —— 这是「失败时也要
            # 拿得到诊断信息」的唯一可靠办法（脚本里没有 subprocess 可直接 Popen，
            # 而新增 import 只为一个探针不值得；见 connect() 在第 97 行）。
            _pname = "iip-traceprobe-%s" % RUN_TOKEN
            docker("rm", "-f", _pname, timeout=120)   # 清同名残留
            _rc3, _out3 = docker(
                "run", "-d", "--name", _pname, "--network", _net,
                "-e", "IIP_EVIDENCE_TRACE_ENABLED=true",
                # 故意打不通：验的是「接线通了」，不是「后端可达」。
                # 采不到数据是预期（本仓库不附带后端，M6 §五降级裁定）。
                "-e", "IIP_TRACE_URL=http://127.0.0.1:1/api/traces",
                "-e", "IIP_EVIDENCE_PROM_ENABLED=false",
                "-e", "IIP_EVIDENCE_LOKI_ENABLED=false",
                "-e", "IIP_PG_URL=%s" % _pgurl,
                _wimg,
                timeout=120)
            if _rc3 != 0:
                check("M6 trace 采集器在开关打开时真注册（环境变量名被真二进制读到）",
                      False, "探针容器起不来 rc=%s %s" % (_rc3, (_out3 or "")[:200]))
            else:
                # 注册日志在启动后毫秒级打出；轮询到出现或超时（45 秒）。
                _trace_on = []
                _deadline_p = time.time() + 45
                while time.time() < _deadline_p:
                    time.sleep(2)
                    _rc_l, _out_l3 = docker("logs", _pname, timeout=60)
                    _out_l3 = _out_l3 or ""
                    _trace_on = [ln.strip() for ln in _out_l3.splitlines()
                                 if "trace 采集器已注册" in ln]
                    if _trace_on:
                        break

                # 收摊：不留僵尸容器（与「每探一次多留一个 Created 僵尸」同源纪律）
                docker("rm", "-f", _pname, timeout=120)

                # **判据必须绑到「注册表的真实状态」，不能绑到某句日志。**
                #
                # 初版判据是「日志里出现 trace 采集器已注册」——
                # 变异测试证明那是**假绿**：把 MustRegister 摘掉后，
                # 紧随其后的 logger.Info("trace 采集器已注册…") 照旧执行，
                # 断言判 PASS，而此时 worker 实际打出的是
                # 「没有任何采集器注册」、sources 里也没有 trace。
                # 即：那句话的**代码邻居**才是被摘的那行。
                #
                # 正确判据取两条件同时成立：
                #   ① sources 数组里真有 trace（由 reg.Sources() 渲染，
                #      只有真注册才会有）；
                #   ② 不出现「没有任何采集器注册」这条 WARN
                #      （仅 reg.Len()==0 时才打，见 main.go）。
                _src_ok = [ln.strip() for ln in _out_l3.splitlines()
                           if '"证据采集器已注册"' in ln and '"trace"' in ln]
                _empty_warn = [ln.strip() for ln in _out_l3.splitlines()
                               if "没有任何采集器注册" in ln]
                _reg_ok = bool(_src_ok) and not _empty_warn

                check("M6 trace 采集器在开关打开时真注册（环境变量名被真二进制读到）",
                      _reg_ok,
                      # 中性措辞（同上）；失败细节由下面 w() 的探针日志承担。
                      "sources 含 trace 且无空注册表告警（sources 命中 %d 条）"
                      % len(_src_ok)
                      if _reg_ok
                      else "sources 含 trace 命中 %d 条、空注册表告警 %d 条。"
                           "探针启动日志前几行: %s"
                           % (len(_src_ok), len(_empty_warn),
                              " | ".join(_out_l3.splitlines()[:8])[:340]))
                w("  探针容器启动日志（前 10 行）：")
                for _l in _out_l3.splitlines()[:10]:
                    w("    " + _l[:190])





    # 4.7.5 tasks 表的 CHECK 约束真实生效：非法 status 必须被拒。
    # 只验「表存在」不够 —— 约束没建成功时表也在，脏数据照样能写进去。
    rc_c, out_c = docker(
        "exec", PG_C, "psql", "-U", "postgres", "-d", "iip", "-t", "-A", "-c",
        "INSERT INTO tasks (kind, status) VALUES ('collect_evidence','bogus');",
        timeout=60)
    check("M4 tasks.status CHECK 约束生效（非法值被拒）",
          rc_c != 0 and ("chk_tasks_status" in (out_c + "")
                         or "violates check constraint" in (out_c + "")),
          "rc=%d %s" % (rc_c, (out_c or "").strip()[:160]))

    # ============================================ 4.7.4cM6 S4：UI 鉴权
    #
    # 为什么必须跑真容器：单测里TestBuildRegistry 那种手工构造 cfg 的路子
    # 证明不了「真二进制认IIP_AUTH_TOKEN 这个环境变量名」，而那正是本批的全部意义。
    #
    # 形态：另起一个 server 容器挂同一份镜像，注入 IIP_AUTH_TOKEN，
    # 与宿主侧HTTP_PORT 错开（宿主那个仍是「未启用鉴权」的状态，
    # 用于验「默认关」那一半）。
    _AUTH_TOKEN = "S4Probe%sToken" % RUN_TOKEN
    _auth_port = HTTP_PORT + 1
    _apname = "iip-authprobe-%s" % RUN_TOKEN
    if not port_in_use(_auth_port):
        # compose() 返回 (rc, out)，不是裸字符串。
        # 这几处曾漏了解包 ⇒ `_net` 是 tuple、下面 `.strip()` 直接抛
        # AttributeError，且**崩在 F3 断言之前**，整条容器链路跑不到底。
        # 范式照抄 211 行 svc_container()：`rc, out = compose(...)`。
        #
        # **网络名必须用 1012 行那个取法**（docker inspect .NetworkSettings），
        # 不能用 `compose ps -q postgres` —— 后者返回的是**容器 ID**，
        # 把它当网络名传给 `--network` 会得到
        #「failed to set up container networking: network <64位hex> not found」，
        # 容器卡在 Created 状态、logs 为空。实测踩过两轮才发现。
        _rc_net, _out_net = docker(
            "inspect", "-f",
            "{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}",
            PG_C, timeout=60)
        _net = ((_out_net or "").strip().splitlines() or [""])[0].strip()
        if not _net:
            check("M6 真二进制认IIP_AUTH_TOKEN（启用路径·容器侧）",
                  False, "取不到 compose 网络，探针无法接入")
        else:
            # 探针只验 HTTP 鉴权、不碰采集器，但 server 启动早期会store.Connect
            # —— 连不上库就直接退出，于是「端口不监听」，症状与「鉴权没生效」
            # 完全无法区分。故必须接**真库**（与 trace 探针同一条教训）。
            # 连接串**从 compose 动态读**，不自己拼。
            # 硬编码踩过一次：compose 里是 postgres/dev，而 K8s 那套是别的组合。
            _rc_pw, _pg_env = compose("exec", "-T", "postgres", "printenv", "POSTGRES_PASSWORD")
            _rc_user, _pg_user = compose("exec", "-T", "postgres", "printenv", "POSTGRES_USER")
            _rc_dbn, _pg_db = compose("exec", "-T", "postgres", "printenv", "POSTGRES_DB")
            _pu = ((_pg_user or "").strip() or "postgres")
            _pd = ((_pg_db or "").strip() or "iip")
            _pp = ((_pg_env or "").strip() or "dev")
            # docker() 与 compose() 一样返回 (rc, out)。
            # 这里曾按裸 rc 用 ⇒ `_rc_a` 是 tuple，
            # `"..." % _rc_a` 抛 TypeError: not all arguments converted。
            # **教训：修一处漏解包时必须把整个函数的调用点一起扫**——
            # 同一段里连着三处 printenv 与一处 docker run，是同一种错。
            _rc_a, _out_a = docker(
                "run", "-d", "--name", _apname,
                "--network", _net,
                "-e", "IIP_AUTH_TOKEN=%s" % _AUTH_TOKEN,
                "-e", "IIP_HTTP_ADDR=0.0.0.0:%d" % _auth_port,
                "-e", "IIP_PG_URL=postgres://%s:%s@postgres:5432/%s?sslmode=disable"
                      % (_pu, _pp, _pd),
                "-p", "127.0.0.1:%d:%d" % (_auth_port, _auth_port),
                "iip-server:%s" % TAG,
                timeout=300)
            # 具名容器且**不**用 --rm：起来失败时还能 docker logs 看到原因。
            # （上一批踩过：常驻容器被超时杀掉后，一个字都拿不到。）
            if _rc_a != 0:
                check("M6 真二进制认 IIP_AUTH_TOKEN（启用路径·容器侧）",
                      False, "鉴权探针容器起不来 rc=%s out=%s"
                            % (_rc_a, (_out_a or "").strip()[:160]))
            else:
                # 等监听就绪
                _up = False
                _dl = time.time() + 90
                while time.time() < _dl:
                    if port_in_use(_auth_port):
                        _up = True
                        break
                    time.sleep(2)
                if not _up:
                    check("M6 真二进制认 IIP_AUTH_TOKEN（启用路径·容器侧）",
                          False, "鉴权探针 90s 内未监听 %d" % _auth_port)
                else:
                    _a_base = "http://127.0.0.1:%d" % _auth_port
                    # ① 不带 token → 401。这是启用路径的**核心**判据。
                    _s1, _b1 = http_request(_a_base + "/api/v1/alerts", timeout=8)
                    # ② 带对 token → 200。只验 ① 是半个判据：
                    #    一个「所有请求都 401」的实现同样能让① 通过。
                    _s2, _b2 = http_request(
                        _a_base + "/api/v1/alerts", timeout=8,
                        headers={"X-IIP-Token": _AUTH_TOKEN})
                    # ③ 带错 token → 401。验「不是只看头在不在」。
                    _s3, _b3 = http_request(
                        _a_base + "/api/v1/alerts", timeout=8,
                        headers={"X-IIP-Token": _AUTH_TOKEN + "x"})
                    check("M6 启用鉴权：无 token 请求被拒（401）",
                          _s1 == 401, "status=%r body=%s" % (_s1, (_b1 or "")[:120]))
                    check("M6 启用鉴权：带正确 token 放行（200）",
                          _s2 == 200, "status=%r body=%s" % (_s2, (_b2 or "")[:120]))
                    check("M6 启用鉴权：错误 token 被拒（401）",
                          _s3 == 401, "status=%r" % _s3)
                    # ④/healthz 不被拦：否则容器 healthcheck 判定不健康，
                    #    症状是「服务起不来」而真因是鉴权。
                    _s4, _b4 = http_request(_a_base + "/healthz", timeout=8)
                    check("M6 启用鉴权：/healthz 仍可访问（容器 healthcheck 依赖它）",
                          _s4 == 200, "status=%r" % _s4)
                    # ⑤/auth/session 豁免：前端靠它拿 token，被拦即死锁。
                    _s5, _b5 = http_request(
                        _a_base + "/api/v1/auth/session", timeout=8)
                    _ok5 = _s5 == 200 and '"auth_required":true' in (_b5 or "").replace(" ", "")
                    check("M6 启用鉴权：/auth/session 豁免且回传 auth_required=true",
                          _ok5, "status=%r body=%s" % (_s5, (_b5 or "")[:140]))
                    # ⑥ 未启用鉴权的那一半（宿主侧 compose server 仍是默认关）：
                    #    必须一切照常，否则「默认关」就是空头承诺。
                    _s6, _b6 = http_request(
                        "http://127.0.0.1:%d/api/v1/alerts" % HTTP_PORT, timeout=8)
                    check("M6 未配 token：一切照常（200，向后兼容）",
                          _s6 == 200, "status=%r" % _s6)
                    # 401 响应体必须符合全站 {error, code} 契约 ——
                    # 前端对所有 API 错误走同一个解析路径。
                    _b1s = (_b1 or "")
                    _c1 = ('"error"' in _b1s) and ('"code"' in _b1s) and \
                          ('"unauthorized"' in _b1s)
                    check("M6 401 响应体符合 {error, code} 契约且不回显 token",
                          _c1 and (_AUTH_TOKEN not in _b1s),
                          "body=%s" % _b1s[:160])
                    # 前置条件断言：探针真的起来了（否则上面全红是「没起」而非产品缺陷）
                    check("M6 鉴权探针容器运行中（前置条件）",
                          _s1 is not None and _s2 is not None,
                          "status1=%r status2=%r" % (_s1, _s2))
                docker("rm", "-f", _apname, timeout=120)
    else:
        check("M6 真二进制认 IIP_AUTH_TOKEN（启用路径·容器侧）",
              False, "端口 %d 已被占用，换一个再跑（本轮不判为产品缺陷）" % _auth_port)




    # 4.7.6 端到端：投一条告警 → 等worker 采到 → evidence 表出现行。
    #
    # 用与 4.5 段不同的 alertname/instance：去重按**指纹**判定且无时间窗，
    # 同指纹的 firing 行若仍在库会被 skip（RUN_TOKEN 只解决了「本轮 vs 上一轮」，
    # 不解决「本段 vs 4.5 段」）。用独立标签最省事。
    _inc_before = db_count("incidents")
    _tk_before = db_count("tasks")
    payload = json.dumps({
        "version": "4",
        "status": "firing",
        "alerts": [{
            "status": "firing",
            "labels": {
                "alertname": "M4ContainerProbeFail",
                "service": "container-check",
                "instance": "probe-%s" % RUN_TOKEN,
                "severity": "critical",
            },
            "annotations": {"summary": "M4 容器链路探针"},
            "startsAt": "2026-01-01T00:00:00Z",
        }],
    }).encode()
    # http_request 现在**接受** headers 参数（M6 S4 加的，用于鉴权断言）。
    rc_p, _ = http_request(
        "http://127.0.0.1:%d/api/v1/alerts/webhook" % HTTP_PORT,
        "POST", payload, timeout=10)
    check("M4 投递告警 -> 200", rc_p == 200, "rc=%d" % rc_p)

    # 任务入队：M4 的入队钩子在 server 侧（告警落库后触发），
    # 所以断言 tasks 表出现 collect_evidence 行，而不是等 worker 领取。
    _deadline = time.time() + 30
    _tk = _tk_before
    while time.time() < _deadline:
        _tk = db_count("tasks")
        if _tk > _tk_before:
            break
        time.sleep(2)
    check("M4 告警落库后自动入队 collect_evidence 任务",
          _tk > _tk_before,
          "tasks %d -> %d" % (_tk_before, _tk))

    # 采到证据：worker 默认轮询 2s，采集器超时 5s，最多重试 3 次。
    # 给 120s —— 手册判据 1 是「2 分钟内 ≥5 条证据」，这里取同量级上限。
    _deadline = time.time() + 120
    _ev = 0
    while time.time() < _deadline:
        _ev = db_count("evidence")
        if _ev > 0:
            break
        time.sleep(5)
    # 手册 §八判据 1 是「≥5 条」，不是「≥1 条」。写 _ev > 0 是弱断言：
    # 「TopN 被错误截断成 1」「只剩突增没有模式」都表现为「有证据」，
    # 一律放过。这里按判据原文取 5。
    check("M4 worker 自动采到证据并落库（容器内 2 分钟内 ≥5 条）",
          _ev >= 5, "evidence=%d（期望 >=5）" % _ev)

    # 交叉验证：必须有 loki 来源的证据。
    # 只判总数 >0 时，「Loki 采集器在容器里完全没工作、恰好 Prometheus
    # 采到了点东西」也会 PASS —— 断言必须绑到具体来源。
    # 本段造数只灌了 Loki，故 loki 缺失即采集器问题。
    _loci = sql_val("SELECT count(*) FROM evidence WHERE source='loki';")
    check("M4 证据里有 loki 来源（Loki 采集器在容器内确实工作）",
          _loci.isdigit() and int(_loci) > 0,
          "loki 来源证据数=%s" % _loci)

    # 6 类模式 + 1 条突增 = 7 条。TopN 默认 10，6 类不该被截断。
    # 若只剩 2 条（1 模式 + 1 突增），说明 normalizeLogLine 把 6 条
    # 错误信息聚合成 1 类 —— 那会让「Top N 错误模式」这一整条证据形同虚设。
    _pats = sql_val("SELECT count(DISTINCT metadata->>'pattern') FROM evidence "
                    "WHERE source='loki' AND metadata ? 'pattern';")
    check("M4 Loki 错误模式未被错误聚合（6 类模式各自独立成条）",
          _pats.isdigit() and int(_pats) >= 5,
          "不同 pattern 数=%s（期望 >=5）" % _pats)

    # 证据的 query 字段必须可复制复现（判据 2）—— 空串的话前端「复制」按钮
    # 复制到的是空白，运维拿不回原始数据源，证据就失去意义。
    if _ev > 0:
        _empty_q = sql_val("SELECT count(*) FROM evidence "
                           "WHERE query IS NULL OR btrim(query) = '';")
        check("M4 证据的 query 字段非空（可复制复现）", _empty_q == "0",
              "空 query 数=%s" % _empty_q)

        # 时间戳必须是**证据自身的时间**，不是采集时刻。
        # 用 NOW() 兜底的话所有证据的 timestamp 会挤在采集那一秒，
        # 与「RED 指标在事故窗口内」完全对不上，且这种错误不报错。
        _sane = sql_val("SELECT count(*) FROM evidence "
                        "WHERE timestamp > NOW() + INTERVAL '1 hour';")
        check("M4 证据 timestamp 无「未来时间」（未用采集时刻兜底）",
              _sane == "0", "异常条数=%s" % _sane)

        # ---- F3：置信度在容器链路必然全空，且必须说清「为什么」 ----
        #
        # 裁定（2026-10-07，禹）：保持「全空」这条结论，但把它从
        # 一句无依据的断言改成**有因的**断言。
        #
        # 为什么容器链路必然全空——完整推算链（任何一环变了结论就变，
        # 所以下面每一条断言都绑在某一环上，而不是笼统断「全空」）：
        #   1. 告警 startsAt = 2026-01-01T00:00:00Z（见本节投告警处），
        #      ResolveWindow 走「情况二」——start 早于 maxLookback(24h)，
        #      于是窗口被夹成 [now-24h, now]，**宽 24h**。
        #   2. Loki 基线窗口 = [now-48h, now-24h)。
        #   3. 本脚本灌入的 18 条日志时间戳全在 now 附近（毫秒级），
        #      落在**事故窗口**里，基线窗口 0 条。
        #   4. Loki 走 baselineEmpty 分支：写 novelty/burst，**不写 change_ratio**。
        #   5. rca.Assess 第②步「change_ratio 缺失」→ BandUnassessable
        #      → Assign 保持 nil。
        # ⇒ 7 条 loki 证据的 confidence 全是 NULL。
        #
        # 关键：**「全空」本身不能区分「Assign 没被调用」与
        # 「Assign 调了但输入不可评估」**——两种情况下都是全空。
        # 所以必须配一条交叉验证：worker 日志里出现 rca.Stats 的
        # confidence 字段，且 unassessable > 0、coverage=0/6。
        # 日志里有这个字段 ⇒ Assign 被调用了 ⇒ 全空是判据在起作用，
        # 而不是接线漏了。这与 verify-func.py 的断言互为对照：
        # 那里非空（mock 数据源两窗口都返东西），这里全空（真 Loki 灌不进基线）。
        _cf = sql_val("SELECT count(*) FROM evidence "
                      "WHERE confidence IS NOT NULL;")
        check("容器链路 evidence.confidence 全空（Loki 基线窗口无数据，见下因果断言）",
              _cf == "0", "非空条数=%s" % _cf)

        # 因果断言 1：这些证据确实**没有** change_ratio 键。
        # 若这一条红而上面那条也红，说明 Loki 采集器改了行为
        # （比如开始写 change_ratio），那「全空」就变成真缺陷了。
        _no_ratio = sql_val(
            "SELECT count(*) FROM evidence WHERE metadata ? 'change_ratio';")
        check("容器内证据均无 change_ratio 键（基线窗口无数据，非采集器缺陷）",
              _no_ratio == "0", "带 change_ratio 的条数=%s" % _no_ratio)

        # 因果断言 2：有 novelty/burst 键的证据，其 confidence 必须是 NULL。
        # 这条是「不可评估绝不写 0」的容器侧对照——
        # 只断「全空」的话，写成 0 也满足。
        _nov = sql_val("SELECT count(*) FROM evidence "
                       "WHERE (metadata ? 'novelty' OR metadata ? 'burst') "
                       "AND confidence IS NOT NULL;")
        check("novelty/burst 类证据的 confidence 为 NULL（未评估≠0）",
              _nov == "0", "被写入数=%s" % _nov)

        # 因果断言 3（交叉验证，最关键的一条）：worker 日志里有
        # rca.Stats 的输出。没有它，上面两条都可能是「Assign 压根没跑」。
        #
        # **必须轮询等，不能只读一次**（2026-10-08 实测踩过）：
        # 证据落库早于 worker 打出 Stats 日志 —— 上面那条
        #「worker 自动采到证据（evidence=7）」已经 PASS 时，
        # 带 confidence 的那行日志**还没出现**。原因是 Assign 在
        # Save 之后、logHypotheses 随其后，而断言跑在中间。
        # 实测时间线：断言处~09:28:0x，Stats 日志 09:29:15。
        # 单次读日志 ⇒ 「实际: 无」假红，看起来像接线漏了。
        _stat: list[str] = []
        _dl_stats = time.time() + 120
        while time.time() < _dl_stats:
            rc_c, out_c = docker("logs", "--tail", "300", "iip-worker", timeout=60)
            _stat = [ln.strip() for ln in (out_c or "").splitlines()
                     if '"confidence":"' in ln or "confidence=" in ln]
            if _stat:
                break
            time.sleep(5)
        check("M6 worker 日志含 rca.Stats 的 confidence 字段（Assign 已接线）",
              bool(_stat),
              "实际: %s" % (_stat[-1][:150] if _stat else "120s 内未见（等不到非代表采样）"))
        if _stat:
            # 只认「total 与 unassessable 都 > 0」的这条：
            # total=0 的那几条是**空采样**（采集器没采到东西，
            # unassessable 自然也是 0），拿它们当证据会把
            # 「没采到」当成「采到了但不可评估」。
            # 原写法 `"unassessable=0" not in _last` 只排除恰好为 0，
            # 而 unassessable=10 也照样通过 —— 改为解析数值。
            _reached = [ln for ln in _stat if _stats_total(ln) > 0]
            _last = (_reached or _stat)[-1]
            _t, _u = _stats_total(_last), _stats_unassessable(_last)
            check("Stats 显示采到证据且全部不可评估（全空是判据在起作用）",
                  bool(_reached) and _u > 0 and "assigned=0" in _last
                  and "coverage=0/6" in _last,
                  "total=%d unassessable=%d 行=%s" % (_t, _u, _last[:160]))

        # 因果断言 4：没有 coverage_basis/coverage_coverage 键。
        # Assign 的覆写路径会写这两个键；一条都没有 ⇒ 它一次都没写过值。
        _basis = sql_val("SELECT count(*) FROM evidence "
                         "WHERE metadata ? 'confidence_basis' "
                         "OR metadata ? 'confidence_coverage';")
        check("容器内证据无 confidence_basis/coverage 键（Assign 未覆写任何一条）",
              _basis == "0", "带键条数=%s" % _basis)

        w("  证据按来源分布：")
        rc_d, out_d = docker(
            "exec", PG_C, "psql", "-U", "postgres", "-d", "iip", "-t", "-A", "-c",
            "SELECT source||' = '||count(*) FROM evidence GROUP BY source "
            "ORDER BY source;", timeout=60)
        for ln in out_d.splitlines():
            if ln.strip():
                w("    " + ln.strip())

    # 证据查询 API 能读出真计数（判据 8 的容器侧）：
    # truncated 字段必须为 false 而 count/total 自洽 —— 容器里数据量小，
    # 不该触发截断；一旦「count != total 且 truncated=false」就是 bug。
    if _inc_before >= 0 and _ev > 0:
        _inc_id = sql_val("SELECT max(id) FROM incidents;")
        if _inc_id.isdigit():
            rc_a, body_a = http_get(
                "http://127.0.0.1:%d/api/v1/incidents/%s/evidence"
                % (HTTP_PORT, _inc_id), timeout=10)
            ok_api = rc_a == 200
            if ok_api:
                try:
                    j = json.loads(body_a)
                    ok_api = (isinstance(j, dict)
                              and "evidence" in j
                              and j.get("count") == len(j["evidence"])
                              and isinstance(j.get("total"), int)
                              and isinstance(j.get("truncated"), bool))
                    w("  证据 API 返回：count=%s total=%s truncated=%s limit=%s"
                      % (j.get("count"), j.get("total"),
                         j.get("truncated"), j.get("limit")))
                except Exception as e:  # noqa: BLE001
                    ok_api = False
                    w("  证据 API 响应解析失败: %r" % (e,))
            check("M4 证据查询 API 可用且 count/total/truncated 自洽", ok_api,
                  "http rc=%d" % rc_a)
    w()

    # ------------------------------------------- 4.8 M5：静态托管（Linux 语义验收）
    # 本段跑在**容器链路**里，验的是「Windows 上验不到的那部分」。
    #
    # 背景（M5 手册 OPEN-M5-6）：internal/webui 有一条
    # TestHandlerAllowsSymlinkedRoot，验的是「root 本身是符号链接时
    # Handler 不得把它判成目录穿越」—— 而这恰恰是部署常见形态
    # （dist 链接到 release 目录）。它在 Windows 上是 **SKIP**：
    # os.Symlink 需要 SeCreateSymbolicLink 权限，无管理员时建不出来。
    #
    # 为什么在这里跑而不是让用户手动执行：
    #   * 判据 13 要求「真库门禁 SKIP=0」，这条 SKIP 是唯一的例外项；
    #     不在 Linux 上真跑一遍，门禁项永远是「部分」。
    #   * 用 golang:1.23-alpine 跑的是**那条被SKIP 的测试本身**，
    #     比「手工造 symlink 再发 HTTP」更直接 —— 后者验的是症状，
    #     前者验的是那条断言真的成立。
    #
    # 断言口径：**零 SKIP + 那条 symlink 测试 PASS**。
    # 刻意不写死「PASS 数 = N」—— 加一条用例就得改断言，
    # 而 N 变化时到底该期望多少无从判断（见 4.4 的同类教训）。
    w("### [4.8] M5 静态托管：Linux 下的 symlink 语义（OPEN-M5-6）")

    _modcache = os.environ.get("IIP_GOMODCACHE",
                               os.path.join(os.path.expanduser("~"), "go", "pkg", "mod"))
    if not os.path.isdir(_modcache):
        # 找不到就**判FAIL 并如实标注**，不能默默pass ——
        # 「没跑」与「跑过且通过」在报告里必须能区分开。
        check("M5 webui 测试在 Linux 容器内跑（symlink 语义）", False,
              # 中性措辞（同上：detail 无条件拼到行上）。
              "环境缺失：IIP_GOMODCACHE=%s 不是有效目录，本段未执行"
              % _modcache)
        w("  环境缺失：Go modcache 路径 %s 无效，本段无法零网络跑 go test。"
          "设该变量指向宿主 modcache 后重跑。" % _modcache)

    else:
        w("  modcache = %s" % _modcache)

        # **工具链来源（2026-10-12 修）**：本段原本只用 golang:1.23-alpine
        # + GOPROXY=off，2026-10-12 项目升到 go1.26 后必然失败于
        # `go.mod requires go >= 1.26.0 (running go 1.23.12; GOTOOLCHAIN=local)`。
        #
        # 为什么不能靠 GOTOOLCHAIN=auto 自举：**GOPROXY=off 是本段刻意设的**
        # （容器内直连 proxy.golang.org 实测 connection refused），而工具链自举
        # 本身就要下载 —— 关掉代理就必然拉不到。
        #
        # 为什么不挂宿主工具链：宿主那份是 **windows-amd64** 版（只有 go.exe），
        # 挂进 Linux 容器执行不了。实测排除，别再走这条路。
        #
        # 采用：让本段用**构建期已验证可用的代理**跑一次 go test。
        # 构建阶段已实测 golang:1.23-alpine + goproxy.cn 能自举到 go1.26.9
        # （Dockerfile 第 4/9 层日志「工具链已自举并对齐：go1.26.9」为证），
        # 所以这里复用同一代理即可，与构建路径保持一致。
        _rc, _out = docker(
            "run", "--rm",
            "-v", PROJ.replace("\\", "/") + ":/src",
            "-v", _modcache.replace("\\", "/") + ":/go/pkg/mod",
            "-w", "/src",
            # 依赖走宿主 modcache（可写：go 可能要写 cache 之外的元数据），
            # 工具链走代理 —— 两者分工明确，不混成一个开关。
            "-e", "GOPROXY=" + GOPROXY,
            "-e", "GOFLAGS=-mod=mod",
            "-e", "GOCACHE=/tmp/gocache",
            "-e", "GOTOOLCHAIN=auto",
            "golang:1.23-alpine",
            "sh", "-c",
            # 显式打印实际版本：断言「用对编译器」不能靠假设，
            # 也不能靠「go.mod 会报错」间接推断。
            "go version && go test ./internal/webui/ -v -count=1",
            timeout=900)

        if _rc != 0:
            w("  ---- go test 输出（末 40 行）----")
            for _line in _out.splitlines()[-40:]:
                w("    " + _line)
        check("M5 webui 测试在 Linux 容器内 rc=0", _rc == 0,
              "" if _rc == 0 else "rc=%s，输出见上" % _rc)

        # 零SKIP：这条是判据 13 的直接依据。
        _skips = re.findall(r"^\s*--- SKIP: (\S+)", _out, re.M)
        check("M5 webui 测试在 Linux 下零 SKIP（判据 13）", not _skips,
              "" if not _skips else "SKIP: %s" % ", ".join(_skips))

        # 单独点出那条 symlink 测试：零 SKIP 只说明「没跳过」，
        # 不等于「它跑过了且成立」—— SKIP 数为 0 时它也可能压根不在
        # -run 范围内或被 build tag 排掉。故显式断言 PASS。
        #
        # detail 写成**中性措辞**：check() 会把 detail 无条件拼到
        # PASS 行上（见其定义），若这里写「未找到该用例的 PASS 行」，
        # 归档报告会印出 `[PASS] ... -- 未在输出中找到该用例的 PASS 行`
        # 这种自相矛盾的行，读的人会以为判据没验。
        # 失败时的具体缺失由下面这行 w() 单独给出。
        _m = re.search(r"^--- PASS: TestHandlerAllowsSymlinkedRoot\b", _out, re.M)
        check("TestHandlerAllowsSymlinkedRoot 在 Linux 下真 PASS（非 SKIP）",
              bool(_m),
              "已匹配 PASS 行" if _m else "未在输出中找到该用例的 PASS 行")

        _np = len(re.findall(r"^--- PASS:", _out, re.M))
        w("  webui 用例：PASS %d 条，SKIP %d 条" % (_np, len(_skips)))

    # 4.8.2 无头浏览器截图（OPEN-M5-8）
    #
    # **默认不跑**，需显式给 IIP_CHROME_BIN 指向宿主 Chrome。
    # 为什么不做成默认：容器内无头浏览器需要额外镜像，而本机镜像源
    # 不可达（2026-10-04 实测：registry-1.docker.io 经代理 502，
    # 四个加速源分别 403/401/超时）—— 默认跑会稳定红，而红的原因
    # 与产品质量无关。做成可选段后，镜像源恢复或 CI 环境可用时
    # 一条命令即可补上，不必重写。
    _chrome = os.environ.get("IIP_CHROME_BIN", "").strip()
    if not _chrome:
        w("  [跳过] 无头浏览器截图（OPEN-M5-8）：未设 IIP_CHROME_BIN。"
          "该段需宿主 Chrome；OPEN 保持未闭环。")
    elif not os.path.isfile(_chrome):
        check("无头浏览器截图（OPEN-M5-8）", False,
              "IIP_CHROME_BIN=%s 不存在" % _chrome)
    else:
        # 截图落盘到报告目录（同级），便于人工核对像素。
        _shot = os.path.join(os.path.dirname(REPORT), "_verify_ui.png")
        _rc2, _out2 = run([_chrome, "--headless=new", "--disable-gpu",
                           "--hide-scrollbars", "--window-size=1440,900",
                           "--screenshot=" + _shot,
                           "http://127.0.0.1:%d/" % HTTP_PORT],
                          timeout=180)
        _exists = os.path.isfile(_shot)
        _size = os.path.getsize(_shot) if _exists else 0
        # 像素断言：**不**用「文件非空」那种恒真判据。
        # 一张纯白/纯黑的失败截图也非空。改为解码 PNG 后统计颜色种类，
        # 真实页面（深色控制台 + 文字 + 表格线）必然远多于 1 种颜色；
        # 而「渲染失败成白页」只有 1 种。这里刻意用极低阈值 (>=4)，
        # 因为要抓的是「有没有渲染出来」，不是「好不好看」。
        _colors = 0
        if _exists and _size > 0:
            try:
                _colors = png_color_count(_shot)
            except Exception as e:  # noqa: BLE001
                w("  PNG 解码失败（截图存在但无法验像素）: %r" % (e,))
        check("无头浏览器截图已生成", _exists and _size > 0,
              "" if _exists else "未生成截图，chrome rc=%s %s" % (_rc2, _out2[:150]))
        check("截图非空白页（颜色种类 >= 4，判据 4）", _colors >= 4,
              "颜色种类=%d，文件 %d 字节。纯白/纯黑失败页只有 1 种颜色"
              % (_colors, _size))
        w("  截图：%s（%d 字节，%d 种颜色）" % (_shot, _size, _colors))

    # 4.8.3 S3：真浏览器走完「列表 → 详情 → 处置」（DoD 3，收 OPEN-M5-8）
    #
    # 为什么与 4.8.2 并存而不是替换：
    #   - 4.8.2 用 `chrome --headless --screenshot=URL` 截**单URL 首屏**，
    #     它验的是「渲染没白屏」；
    #   - 4.8.3 用 Playwright **真实点击**，验的是「点了按钮之后页面变成什么样」。
    # 两者是不同的不变量，互补，各自的判据都不能替代对方。
    #
    # 为什么走Playwright 而不是 chrome CLI：chrome CLI 无法点击，
    # 「处置」这一步就无从验证（DoD 3 明确要求走完处置）。
    #
    # 判据分层（每条都在防自证失效）：
    #   - 判**DOM 内容**而非「截图文件非空」—— 纯白页也非空；
    #   - 处置那一步**额外回读 API** —— 页面变了不等于数据落库；
    #   - 每步截图数颜色种类 >= 4 —— 排除白屏/黑屏；
    #   - 事件 id **不写死**（随运行次数变），按 service 前缀筛。
    _node = os.environ.get("IIP_NODE_BIN", "").strip() or shutil.which("node")
    _s3 = os.path.join(PROJ, "web", "e2e", "s3-shots.mjs")
    if not _node:
        w("  [跳过] S3 交互截图：未找到 node（设 IIP_NODE_BIN 指向托管 node）。"
          "OPEN-M5-8 保持未闭环。")
    elif not os.path.isfile(_s3):
        check("S3 交互截图脚本存在", False, "未找到 %s" % _s3)
    else:
        _s3_report = os.path.join(os.environ.get("IIP_LOG_DIR", PROJ),
                                  "_verify_s3.txt")
        _s3_shots = os.path.join(os.environ.get("IIP_LOG_DIR", PROJ),
                                 "screenshots")

        # ---- S3 专用数据注入（本段新增，2026-10-12）----
        #
        # **为什么必须在这里造，而不是「依赖上一段」**：S3 脚本按
        # service 前缀 `e2e-svc-` 挑事件，而本文件其它段注入的
        # service 分别是 checkout / payment / container-check ——
        # **没有一个带该前缀**。所以 S3-2「库里有 e2e 造的事件」
        # 在改动前恒为 0 条 → 恒假判据，S3 必然 FAIL 且 FAIL 得毫无意义
        # （不是产品缺陷，是前提从未成立）。
        #
        # 用独立 service 名而非复用现有的：① 不会与其它段的
        # 去重指纹（labels 参与指纹，fingerprint.go）互相干扰；
        # ② 前缀筛选天然把 S3 的用��圈在本段内，删库/重跑都干净。
        #
        # instance 带 RUN_TOKEN：同指纹的 firing 行若仍在库会被 skip，
        # 跨轮次必须唯一（与 4.5 段同一个坑）。
        _svc_s3 = "e2e-svc-s3"
        _inc_s3_before = db_count("incidents")
        _pl_s3 = json.dumps({
            "version": "4",
            "status": "firing",
            "alerts": [{
                "status": "firing",
                "labels": {
                    "alertname": "S3InteractiveProbe",
                    "service": _svc_s3,
                    "instance": "s3-%s" % RUN_TOKEN,
                    "severity": "critical",
                },
                "annotations": {"summary": "S3 交互截图探针"},
                "startsAt": "2026-01-01T00:00:00Z",
            }],
        }).encode()
        _rc_s3, _ = http_request(
            "http://127.0.0.1:%d/api/v1/alerts/webhook" % HTTP_PORT,
            "POST", _pl_s3, timeout=10)
        check("S3 前置：投递 e2e-svc- 告警 -> 200", _rc_s3 == 200,
              "rc=%d" % _rc_s3)
        # 事件是异步生成的（告警落库 → 关联去重 → 生成 incident），
        # 立刻查会读到 0 条 —— 那是时序没到，不是没造进去。
        _dl_s3 = time.time() + 30
        _got_s3 = 0
        while time.time() < _dl_s3:
            # 用 sql_val 直查而非 db_count：后者只接受表名、不支持 WHERE
            # （db_count 的签名是 db_count(table)）。
            _v_s3 = sql_val("SELECT count(*) FROM incidents "
                            "WHERE service = '%s';" % _svc_s3)
            _got_s3 = int(_v_s3) if _v_s3.isdigit() else -1
            if _got_s3 > 0:
                break
            time.sleep(2)
        check("S3 前置：e2e-svc- 事件已落库（等 30s）",
              _got_s3 > 0,
              # -1 = 查询本身失败（如列名不存在）。它与「真的 0 条」是
              # 两件事，混成同一句文案会把脚本 bug 误读成产品缺陷。
              "service=%s %s / incidents 总数 %d->%d"
              % (_svc_s3,
                 "查询失败(sql_val 返回空)" if _got_s3 < 0 else "%d 条" % _got_s3,
                 _inc_s3_before, db_count("incidents")))

        _env = dict(os.environ)
        _env["S3_BASE_URL"] = "http://127.0.0.1:%d" % HTTP_PORT
        _env["S3_REPORT"] = _s3_report
        _env["S3_OUT_DIR"] = _s3_shots
        # **显式传浏览器路径**：s3-shots.mjs 自带默认路径，但那是
        # 硬编码的 `C:\Program Files\...`；宿主装在别处时它会静默
        # launch 失败。由调用方注入，与 4.8.2 段用同一个来源。
        _chrome_s3 = os.environ.get("IIP_CHROME_BIN", "").strip() or \
            r"C:\Program Files\Google\Chrome\Application\chrome.exe"
        if os.path.isfile(_chrome_s3):
            _env["IIP_CHROME_BIN"] = _chrome_s3
        else:
            w("  [提示] 默认 Chrome 路径不存在：%s（S3 将如实 FAIL）"
              % _chrome_s3)
        _rc3, _out3 = run([_node, "e2e/s3-shots.mjs"],
                          extra_env=_env, timeout=600,
                          cwd=os.path.join(PROJ, "web"))
        # **必须读脚本自落的报告，不读 wrapper 的 stdout 副本**
        # （同上：验收数据以脚本自落报告为准）。
        _s3_txt = ""
        if os.path.isfile(_s3_report):
            with open(_s3_report, encoding="utf-8", errors="replace") as fh:
                _s3_txt = fh.read()
        else:
            w("  S3 报告未生成：%s" % _s3_report)

        _m3 = re.search(r"=== S3 汇总：(\d+) PASS / (\d+) FAIL ===", _s3_txt)
        check("S3 交互流程有可解析的汇总行（列表 → 详情 → 处置）",
              _m3 is not None,
              "已匹配汇总行" if _m3
              else "未在报告中找到汇总行，node rc=%s，stdout=%s"
                   % (_rc3, _out3[-200:]))

        # 三张截图各自要存在且非空白。三步是三个不同的不变量，
        # 合成一个「至少存在一张」是恒真判据。
        for _n, _label in (("01-list", "列表页"), ("02-detail", "详情页"),
                           ("03-transition", "处置后")):
            _p = os.path.join(_s3_shots, _n + ".png")
            _ok = os.path.isfile(_p)
            _c = -1  # -1 = 解不出来；**不能当 0 种颜色**（那是两件事）
            if _ok:
                try:
                    _c = png_color_count(_p)
                except Exception as e:  # noqa: BLE001
                    w("  PNG 解码失败（%s）: %r" % (_n, e))
            check("S3 %s截图已生成且非空白（颜色种类 >= 4）" % _label,
                  _ok and _c >= 4,
                  "颜色种类=%d，文件 %s" % (
                      _c, _p if _ok else "未生成"))

        if _m3:
            _p3, _f3 = int(_m3.group(1)), int(_m3.group(2))
            check("S3 全部断言通过", _f3 == 0 and _p3 > 0,
                  "%d PASS / %d FAIL" % (_p3, _f3))
            w("  S3 交互截图：%d PASS / %d FAIL，截图目录 %s"
              % (_p3, _f3, _s3_shots))
        w("  S3 报告：%s" % _s3_report)

    w()

    # ------------------------------------------------------ 5. 证据留存
    w("### [5] 日志证据")
    for svc in ("iip-server", "iip-worker"):
        rc, out = docker("logs", "--tail", "25", svc, timeout=60)
        w("  ---- %s (末 25 行) ----" % svc)
        for line in out.splitlines()[-25:]:
            w("    " + line)
    w()

    try:
        rc, out = docker("exec", PG_C, "psql", "-U", "postgres", "-d", "iip",
                         "-t", "-A", "-c",
                         "SELECT tablename FROM pg_tables WHERE schemaname='public' "
                         "ORDER BY tablename;", timeout=60)
        w("  容器内 iip 库的表清单：")
        for line in out.splitlines():
            if line.strip():
                w("    " + line.strip())
    except Exception:  # noqa: BLE001
        pass
    w()

    # --------------------------------------------------------- 6. 结论
    w("=" * 70)
    if FAILURES:
        w(" 结论：FAIL  （%d 项未通过）" % len(FAILURES))
        for f in FAILURES:
            w("   - %s" % f)
    else:
        w(" 结论：PASS  （全部断言通过）")
        w()
        w(" 一键启动（保持运行）：")
        w("   docker compose -f deploy/docker-compose.yml --profile app up -d")
        w(" 访问：")
        w("   curl.exe http://127.0.0.1:%d/healthz" % HTTP_PORT)
    w("=" * 70)
    return 1 if FAILURES else 0


if __name__ == "__main__":
    code = 1
    try:
        code = main()
    finally:
        # 除明确要求保留外，一律清理，避免残留容器占端口影响后续验证
        w()
        if KEEP:
            w("### 保留容器（IIP_KEEP=1），未清理")
        else:
            w("### 清理：compose down（保留 named volume 数据）")
            rc, out = compose("--profile", "app", "down", timeout=300)
            w("  rc=%s %s" % (rc, out[:200]))
        try:
            with open(REPORT, "w", encoding="utf-8") as fh:
                fh.write("\n".join(L))
        except OSError:
            pass
    sys.exit(code)
