"""Docker CLI 定位与环境装配 —— 供 `scripts/` 下的运维脚本复用。

为什么单独抽出来：
  Docker Desktop for Windows 默认**不把 docker.exe 写进 PATH**；
  而 compose / buildx 是以 CLI 插件形式提供的，插件目录也不在默认搜索路径里。
  如果每个脚本各写一份「探测可执行文件 + 把插件目录拼进 PATH」的逻辑，
  很容易出现「改了一处、漏了另一处」——本项目就出现过两个脚本对同一台机器的
  docker 定位结果不一致（一个找到、一个报找不到）。

调用方（保持这两处永远用同一套定位规则）：
  - scripts/verify-images.py   容器链路端到端验收
  - scripts/prepull-mirror.py  国内网络下多源拉取基础镜像

约定：
  - 所有函数不抛异常，返回 None / 空串表示「没找到」，由调用方决定怎么报错；
  - 环境变量 `IIP_DOCKER_BIN` 可显式指定，优先级最高（适配非默认安装路径）。
"""
import os
import shutil
import subprocess

# Windows 上 Docker Desktop 的常见安装位置。放在这里而不是散落各脚本，
# 是为了新增候选路径时只需改一处。
_WIN_CANDIDATES = (
    ("LOCALAPPDATA", "Programs", "DockerDesktop", "resources", "bin", "docker.exe"),
)
_WIN_ABSOLUTE = (
    r"C:\Program Files\Docker\Docker\resources\bin\docker.exe",
)


def find_docker() -> str | None:
    """定位 docker 可执行文件；找不到返回 None。

    探测顺序：显式环境变量 > PATH > 常见安装位置。
    显式优先是为了让 CI / 非默认安装路径能覆盖探测结果。
    """
    explicit = os.environ.get("IIP_DOCKER_BIN")
    if explicit and os.path.exists(explicit):
        return explicit

    found = shutil.which("docker")
    if found:
        return found

    for parts in _WIN_CANDIDATES:
        # 第一段是环境变量名，其余是路径片段
        base = os.environ.get(parts[0], "")
        if base:
            cand = os.path.join(base, *parts[1:])
            if os.path.exists(cand):
                return cand

    for cand in _WIN_ABSOLUTE:
        if os.path.exists(cand):
            return cand

    return None


def make_env(docker: str | None) -> dict:
    """构造带 docker / cli-plugins 的 PATH，返回新环境字典（不改 os.environ）。

    为什么必须补 PATH：`docker compose ...` 不是内置子命令，而是
    Docker Desktop 在 `resources/cli-plugins` 下放的插件。
    少了这一步会报 `docker: 'compose' is not a docker command`。
    """
    env = dict(os.environ)
    if not docker:
        return env

    bindir = os.path.dirname(docker)
    # .../DockerDesktop/resources/bin/docker.exe -> .../DockerDesktop
    desktop_root = os.path.dirname(os.path.dirname(bindir))
    plugdir = os.path.join(desktop_root, "resources", "cli-plugins")

    paths = [bindir]
    if os.path.isdir(plugdir):
        paths.append(plugdir)
    env["PATH"] = os.pathsep.join(paths + [env.get("PATH", "")])
    env.setdefault("DOCKER_CLI_PLUGIN_EXTRA_DIRS", plugdir)
    return env


def run(args: list[str], env: dict, timeout: int = 900, cwd: str | None = None):
    """执行命令，返回 (rc, 合并后的 stdout+stderr)。

    永不抛异常：超时/找不到可执行文件都用可辨识的非零 rc 表示
    （124 = 超时，沿用 GNU timeout 的约定；-998 = 其它异常）。
    这样调用方可以统一按 rc 判定，不必到处包 try。

    **encoding 必须显式给 "utf-8"（2026-10-12 修）**：
    只写 `text=True` 时 subprocess 用本机 locale 编码解码 —— Windows 上是 GBK，
    而容器 stdout 是 UTF-8，于是「worker 已连接 PostgreSQL」被解成乱码，
    判据按中文子串匹配自然全部落空（实测 11 项 FAIL，其中 4 项纯属此因，
    而容器日志里那些行**本来就存在**）。

    这个bug 极具欺骗性：它表现为「功能没实现」，真因却是「读日志时编码错了」。
    判据自身与被测行为必须能区分 —— 否则会照着不存在的缺陷去改产品代码。

    `errors="replace"` 而非 "ignore"：ignore 会把解不出的字节**静默丢弃**，
    那会让「日志里有这行」与「日志里没这行」变得无法区分；
    replace 至少留下替换符，看得出「这里本来有非 ASCII 字符」。
    """
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=timeout,
                           encoding="utf-8", errors="replace", env=env, cwd=cwd)
        return r.returncode, ((r.stdout or "") + (r.stderr or "")).strip()
    except subprocess.TimeoutExpired:
        return 124, "TIMEOUT after %ss" % timeout
    except Exception as exc:  # noqa: BLE001
        return -998, "EXC %r" % (exc,)


def connect() -> tuple[str | None, dict]:
    """一步到位：返回 (docker 路径, 环境字典)。找不到时 docker 为 None。"""
    d = find_docker()
    return d, make_env(d)
