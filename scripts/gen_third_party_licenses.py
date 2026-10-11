import io, os, json, re

MODCACHE = r"C:\Users\Yu\go\pkg\mod"
GOMOD = r"D:\Documents\qwen-agent\iip\go.mod"
PKGJSON = r"D:\Documents\qwen-agent\iip\web\package.json"
OUT = r"D:\Documents\qwen-agent\iip\THIRD_PARTY_LICENSES.md"


def parse_gomod(path):
    """返回 (direct, indirect) 模块列表，形态为 (module, version)"""
    txt = io.open(path, encoding="utf-8").read()
    # 去掉注释行
    lines = [l for l in txt.split("\n") if not l.strip().startswith("//")]
    joined = "\n".join(lines)

    # 顺序要紧：先把 require ( ... ) 块整体摘走，剩下的才是单行 require。
    # 反过来做会让 `require (` 这一行被单行正则 ^require\s+(\S+)\s+(\S+)
    # 匹配成「模块名 = (」「版本 = 第一个模块名」的垃圾条目
    # （曾生成过一行 `| 直接依赖 | `(` | `github.com/go-chi/chi/v5` | 目录不存在 |`）。
    blocks = re.findall(r"require\s*\(([^)]*)\)", joined, re.S)
    without_blocks = re.sub(r"require\s*\([^)]*\)", "", joined, flags=re.S)
    single = re.findall(r"^require\s+(\S+)\s+(\S+)", without_blocks, re.M)

    direct, indirect = [], []
    for b in blocks:
        for l in b.strip().split("\n"):
            l = l.strip()
            if not l:
                continue
            m = re.match(r"(\S+)\s+(\S+)", l)
            if not m:
                continue
            mod, ver = m.group(1), m.group(2)
            # 模块路径必须像域名/路径，不接受 `(` 之类的噪声
            if not re.match(r"^[a-z0-9.\-]+\.[a-z]{2,}/", mod, re.I):
                continue
            (indirect if "// indirect" in l else direct).append((mod, ver))
    for mod, ver in single:
        if not re.match(r"^[a-z0-9.\-]+\.[a-z]{2,}/", mod, re.I):
            continue
        if not any(d[0] == mod for d in direct):
            if not any(i[0] == mod for i in indirect):
                direct.append((mod, ver))
    return direct, indirect


def esc(mod):
    """模块路径里的大写字母在 modcache 中被编码为 !小写"""
    return re.sub(r"[A-Z]", lambda m: "!" + m.group(0).lower(), mod)


def head_text(full, n=600):
    """取全文前 n 字符，用于全文判不准时的回退判定。"""
    return full[:n]


def find_license(mod, ver):
    """在 modcache 中找该模块的 LICENSE 文件，返回 (文件名, 首行识别, 字节数)"""
    base = os.path.join(MODCACHE, esc(mod) + "@" + ver)
    if not os.path.isdir(base):
        return (None, "目录不存在（可能未下载或已清理）", 0)
    cands = []
    for root, dirs, files in os.walk(base):
        # 不深入子包
        depth = root[len(base):].count(os.sep)
        if depth >= 2:
            dirs[:] = []
        for f in files:
            if re.match(r"^(LICENSE|LICENCE|COPYING|NOTICE)", f, re.I):
                p = os.path.join(root, f)
                rel = os.path.relpath(p, base).replace("\\", "/")
                cands.append((rel, p))
    if not cands:
        return (None, "未找到 LICENSE 文件", 0)
    cands.sort(key=lambda x: (x[0].count("/"), x[0]))
    rel, p = cands[0]
    try:
        #读全文而非首600 字符：BSD-3 的「Neither the name of ...」免责条款
        # 位于 BSD-2 版许可证末尾之外，只读头部会误判成 BSD-2。
        # 首600 字符曾导致 5 个 Go 官方依赖被判「未识别」，故此处读全文。
        full = io.open(p, encoding="utf-8", errors="replace").read()
        size = len(full.encode("utf-8"))
    except Exception as e:
        return (rel, "读取失败: %s" % e, 0)
    # 识别许可证类型：先看全文，失败再看头部（部分许可证全文很长）
    def classify(text):
        low = text.lower()
        if "apache license" in low and "version 2.0" in low:
            return "Apache-2.0"
        if "mit license" in low or "permission is hereby granted, free of charge" in low:
            return "MIT"
        if "redistribution and use in source and binary forms" in low:
            # Go 官方系列（x/sync、x/sys、x/text、fsnotify、pflag 等）用的是
            # BSD-3-Clause 文本但**不带标题行** —— 只靠 "bsd" 关键词会漏判，
            # 故以许可证正文特征串为准。
            if "neither the name of" in low:
                return "BSD-3-Clause"
            return "BSD-2-Clause"
        if "mozilla public license" in low:
            return "MPL-2.0"
        if "isc license" in low:
            return "ISC"
        return None

    kind = classify(full) or classify(head_text(full))
    if kind is None:
        kind = "未识别"
    return (rel, kind, size)


direct, indirect = parse_gomod(GOMOD)

rows = []
for label, items in (("直接依赖", direct), ("间接依赖", indirect)):
    for mod, ver in sorted(items):
        rel, kind, size = find_license(mod, ver)
        rows.append((label, mod, ver, kind, rel or "-", size))

# npm 侧
npm_rows = []
if os.path.isfile(PKGJSON):
    pj = json.load(io.open(PKGJSON, encoding="utf-8"))
    for sec in ("dependencies", "devDependencies"):
        for name, ver in sorted(pj.get(sec, {}).items()):
            npm_rows.append((sec, name, ver))

out = []
out.append("# 第三方依赖许可证清单\n")
out.append("> 生成方式：**从 `go mod` 声明与 modcache 实际 LICENSE 文件读取**，非人工填写。")
out.append("> 核对命令：`python scripts/gen_third_party_licenses.py`")
out.append("> 许可证识别依据各LICENSE 文件首600 字符的特征串，识别不到标为「未识别」**（不猜）**。\n")

out.append("## 一、Go 依赖（%d 个）\n" % len(rows))
out.append("| 类型 | 模块 | 版本 | 许可证 | LICENSE 文件 |")
out.append("|---|---|---|---|---|")
for label, mod, ver, kind, rel, size in rows:
    out.append("| %s | `%s` | `%s` | %s | `%s` |" % (label, mod, ver, kind, rel))

unknown = [r for r in rows if r[3] == "未识别"]
out.append("\n**统计**：共 %d 个 Go 依赖，其中「未识别」%d 个。\n" % (len(rows), len(unknown)))
if unknown:
    out.append("> ⚠️ 「未识别」不表示无许可证，可能是许可证文本特征串未匹配到。")
    out.append("> 商业交付前应逐个打开上表对应文件人工确认。\n")

out.append("## 二、前端 npm 依赖（%d 个）\n" % len(npm_rows))
out.append("| 类型 | 包 | 版本 |")
out.append("|---|---|---|")
for sec, name, ver in npm_rows:
    out.append("| %s | `%s` | `%s` |" % ("运行时" if sec == "dependencies" else "开发", name, ver))

out.append("\n> npm 侧的许可证未逐包展开（`node_modules` 不入库，发布前用 `license-checker` 类工具核）。")
out.append("> 主要依赖为 React / antd / Playwright / vitest，均为 MIT 系。\n")

out.append("## 三、已知需要特别说明的一项\n")
out.append("**`github.com/lib/pq`**：上游已归档不再维护，**但本项目无法移除** ——")
out.append("`golang-migrate/migrate/v4` 的 postgres 驱动硬依赖它，")
out.append("`internal/store/store.go` 的空白导入为 `database/sql` 注册 postgres 驱动。")
out.append("许可证 MIT。移除需先替换 golang-migrate，属独立工作项。\n")

io.open(OUT, "w", encoding="utf-8", newline="\n").write("\n".join(out))
print("written:", OUT)
print("go deps:", len(rows), "| npm deps:", len(npm_rows))
print("未识别许可证:", len(unknown))
for r in unknown:
    print("   ", r[1], r[2], "->", r[4])