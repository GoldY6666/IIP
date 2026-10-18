#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""静态核对 deploy 侧可观测性配置与代码里的指标定义是否一致（M6 T6.4）。

为什么需要这个脚本
------------------
面板与规则里写错指标名、标签值、记录规则名，**全部是静默失败**：
Grafana 显示 "No data"、Prometheus 照常不报错、日志里什么都没有。
唯一的发现途径是有人盯着空图猜。本脚本把这类漂移变成可在提交前判定的错误。

它核对六件事，每一条都对应一种"看起来没问题"的缺陷
----------------------------------------------------
1. 引用闭包：面板/规则里出现的 iip_* 名字必须来自 internal/metrics 的常量
   （直方图的 _bucket/_sum/_count 视作同一指标的派生序列）。
2. 覆盖：代码里的每个业务指标至少被面板引用一次 —— 漏一个面板就是漏一个信号。
3. 标签：选择器里的标签名必须是该指标声明过的标签（外加 le/job/instance），
   标签值必须属于代码常量给出的值域；`sum by (...)` 的字段同理。
4. 记录规则：面板引用的 iip:xxx 必须在规则文件里有 record 定义，
   且规则文件必须真的被 prometheus.yml 的 rule_files 通配匹配到
   （写成 .yaml 就永远不加载，而 Prometheus 不会提示）。
5. 端口：prometheus.yml 里 target 的端口必须等于 config 里的默认监听端口
   （改了默认端口忘了改抓取配置 = "target down 但服务健康"的标准成因）。
6. 结构：JSON 可解析、panel 有 title/expr 且 id 唯一、数据源变量已被定义。

本脚本**证明不了**的三件事（不许含糊）
--------------------------------------
- PromQL 语法是否合法：需要 promtool，本机与 CI 都没有；
- 面板能否真的渲染出图：需要 Grafana 实例；
- 抓取是否真的成功：需要 Docker daemon。
以上三条只能由真实栈验证。本脚本只保证「名字 / 标签 / 引用 / 端口 / 结构」
五类一致，页面上写成"已验证可用"是过度声称。

用法
----
    python scripts/verify-observability.py            # 正常核对
    python scripts/verify-observability.py --verbose  # 打印每项通过明细

退出码：0 全部通过；1 存在 FAIL；2 环境问题（缺 PyYAML / 找不到文件）。
"""

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

METRICS_GO = ROOT / "internal" / "metrics" / "metrics.go"
CORR_TYPES = ROOT / "internal" / "correlation" / "types.go"
CONFIG_GO = ROOT / "internal" / "config" / "config.go"
CONFIG_METRICS_GO = ROOT / "internal" / "config" / "metrics.go"
PROM_YML = ROOT / "deploy" / "prometheus" / "prometheus.yml"
RULES_DIR = ROOT / "deploy" / "prometheus" / "rules"
DASH_DIR = ROOT / "deploy" / "grafana"

# 直方图在抓取时展开成三条序列，名字带后缀；校验前先归一化回指标名。
HIST_SUFFIXES = ("_bucket", "_sum", "_count")

# 指标选择器里允许出现的非声明标签（Prometheus 注入的 target 标签）。
TARGET_LABELS = {"le", "job", "instance", "cluster", "env"}

# 指标字段名 → 该指标标签值常量的前缀（metrics.go 里的常量命名）。
# 这不是"硬编码业务知识"，而是**显式声明映射**：下面 _check_value_prefixes()
# 会验证每个前缀确实在代码里匹配到 ≥2 个常量，改名或挪常量都会红 ——
# 否则映射悄悄失效，标签值校验会退化成"什么都放行"。
VALUE_PREFIX = {
    "AlertIngest": "Ingest",
    "AlertDedup": "Dedup",
    "TaskRetry": "Retry",
    "CorrelationScore": "Action",
}

OK, FAIL, SKIP = "OK", "FAIL", "SKIP"
RESULTS = []


def record(state, name, detail=""):
    RESULTS.append((state, name, detail))


def read(path):
    if not path.exists():
        print("[verify-observability] 找不到文件：%s" % path, file=sys.stderr)
        sys.exit(2)
    return path.read_text(encoding="utf-8")


def strip_yaml_comments(text):
    """去掉 YAML 的整行/行尾注释。

    不处理引号内的 #（本仓库这几个文件里没有），但必须去掉注释：
    规则文件头部大段注释里写着 iip:xxx 与指标名，若不剥离，
    "引用了未定义的记录规则"会被注释里的示例文字误判成通过。
    """
    out = []
    for line in text.splitlines():
        cut = line.find("#")
        out.append(line if cut < 0 else line[:cut])
    return "\n".join(out)


def balanced_block(text, start):
    """从 start 处的 '(' 起，返回括号配平的整段文本。"""
    depth, i = 0, start
    while i < len(text):
        if text[i] == "(":
            depth += 1
        elif text[i] == ")":
            depth -= 1
            if depth == 0:
                return text[start : i + 1]
        i += 1
    return text[start:]


# ---------------------------------------------------------------- 代码侧解析


def parse_instruments(src):
    """解析 metrics.go 里每个 instrument 的名字常量、导出字段与标签名。

    返回 [{field, name_const, labels}]。用括号配平切分而不是跨行正则：
    `[]string{"action"}` 可能落在下一个 instrument 的窗口里，
    宽松正则会把它错配给前一个直方图 —— 那样标签校验就是在验证错的表。
    """
    out = []
    for m in re.finditer(r"m\.(\w+)\s*=\s*prometheus\.New\w+\(", src):
        field = m.group(1)
        # 只用 m.end() - 1 定位左括号。曾误写成 m.start() + m.end() - 1，
        # 越界后 balanced_block 立刻返回空串，于是 7 个 instrument 全被 continue 掉，
        # 而"覆盖检查"因为模型为空反而不报错（0 个指标自然 0 个未覆盖）——
        # 判据自己把自己变成了恒绿。
        block = balanced_block(src, m.end() - 1)
        name_m = re.search(r"Name:\s*(\w+)", block)
        if not name_m:
            continue
        labels_m = re.search(r"\[\]string\{([^}]*)\}", block)
        labels = []
        if labels_m:
            labels = re.findall(r'"([^"]+)"', labels_m.group(1))
        out.append({"field": field, "name_const": name_m.group(1), "labels": labels})
    return out


def parse_consts(src, pattern=r'^\s*(\w+)\s*=\s*"([^"]*)"'):
    return dict(re.findall(pattern, src, re.M))


def load_model():
    src = read(METRICS_GO)
    consts = parse_consts(src)
    instruments = parse_instruments(src)

    # 指标名常量 → 字面值
    metric_by_const = {}
    for name, value in consts.items():
        if value.startswith("iip_"):
            metric_by_const[name] = value
    if not metric_by_const:
        record(FAIL, "代码侧解析", "internal/metrics/metrics.go 里没解析到任何 iip_* 指标常量")
        return None
    # 解析到 0 个 instrument 时必须硬报：否则 model 为空，
    # "覆盖检查"（集合差为空）与"引用闭包"会一起变成恒红/恒绿的噪声，
    # 而真正的原因（解析器坏了）根本没有出现在报告里。
    if not instruments:
        record(
            FAIL,
            "代码侧解析",
            "metrics.go 里一个 instrument 都没解析到 —— 解析器与文件结构已不匹配，"
            "后续所有检查都不可信",
        )
        return None

    # 每个指标：字面值 → {field, labels, allowed_values(按标签)}
    corr_src = read(CORR_TYPES)
    corr_consts = parse_consts(corr_src)
    action_values = {
        v for k, v in corr_consts.items() if k.startswith("Action") and not v.startswith("iip_")
    }

    model = {}
    for inst in instruments:
        value = metric_by_const.get(inst["name_const"])
        if not value:
            record(
                FAIL,
                "代码侧解析",
                "instrument %s 的 Name 常量 %s 没有对应的 iip_* 字面值"
                % (inst["field"], inst["name_const"]),
            )
            continue
        prefix = VALUE_PREFIX.get(inst["field"])
        allowed = {}
        if prefix == "Action":
            # 档位值住在 correlation 包（ActionNew/Merge/Review），不在 metrics.go
            vals = set(action_values)
        elif prefix:
            vals = {v for k, v in consts.items() if k.startswith(prefix) and not v.startswith("iip_")}
        else:
            # 无映射的标签（如 evidence 的 source，值域来自运行期注册的采集器）
            # 静态不可知 —— 记为 UNKNOWN，标签校验对它会跳过而不是放行一切。
            vals = None
        if vals:
            for label in inst["labels"]:
                allowed[label] = vals
        model[value] = {"field": inst["field"], "labels": set(inst["labels"]), "allowed": allowed}
    return model


def check_value_prefixes(model, src):
    """守住 VALUE_PREFIX 本身：前缀匹配不到常量就红，防止映射悄悄失效。"""
    consts = parse_consts(src)
    corr_consts = parse_consts(read(CORR_TYPES))
    for field, prefix in VALUE_PREFIX.items():
        pool = corr_consts if prefix == "Action" else consts
        hits = [
            k
            for k, v in pool.items()
            if k.startswith(prefix) and not v.startswith("iip_") and re.fullmatch(r"[a-z_]+", v)
        ]
        if len(hits) < 2:
            record(
                FAIL,
                "值域映射",
                "字段 %s 的值前缀 %r 在代码里只匹配到 %d 个常量 —— "
                "常量被改名/搬走了？标签值校验会因此退化成放行一切。" % (field, prefix, len(hits)),
            )
            return
    record(OK, "值域映射", "%d 个字段的值前缀都还能在代码里对上常量" % len(VALUE_PREFIX))


# ---------------------------------------------------------------- 配置侧解析


def config_ports():
    """从 config 包取默认监听端口。"""
    ports = {}
    for path, const in ((CONFIG_GO, "DefaultHTTPAddr"), (CONFIG_METRICS_GO, "DefaultWorkerMetricsAddr")):
        src = read(path)
        m = re.search(r'%s\s*=\s*"([^"]+)"' % const, src)
        if not m:
            record(FAIL, "端口漂移", "%s 里找不到 %s 的定义" % (path.name, const))
            continue
        addr = m.group(1)
        if ":" not in addr:
            record(FAIL, "端口漂移", "%s = %r 形如缺少端口" % (const, addr))
            continue
        ports[const] = addr.rsplit(":", 1)[1]
    return ports


def collect_refs(texts):
    """从若干段文本里抽出引用的指标名 / 记录规则名 / 选择器 / by 列表。"""
    metrics, records, selectors, by_lists = set(), set(), [], []
    for tag, text in texts:
        for tok in re.findall(r"\biip_[a-z0-9_]+", text):
            base = tok
            for suf in HIST_SUFFIXES:
                if base.endswith(suf):
                    base = base[: -len(suf)]
                    break
            metrics.add((tag, tok, base))
        for tok in re.findall(r"\biip:[a-z0-9_:]+", text):
            records.add((tag, tok))
        for m in re.finditer(r"\b(iip_[a-z0-9_]+)((?:_bucket|_sum|_count)?)\s*\{([^}]*)\}", text):
            base = m.group(1)
            selectors.append((tag, base, m.group(3)))
        for m in re.finditer(r"\bby\s*\(([^)]*)\)", text):
            names = [n.strip() for n in m.group(1).split(",") if n.strip()]
            by_lists.append((tag, names))
    return metrics, records, selectors, by_lists


def main():
    verbose = "--verbose" in sys.argv
    try:
        import yaml  # noqa: F401
    except ImportError:
        print(
            "[verify-observability] 缺少 PyYAML，无法做结构校验。\n"
            "  安装：pip install pyyaml\n"
            "  刻意不提供「跳过结构校验」的开关：跳过会在报告里变成绿，而没人会读细节。",
            file=sys.stderr,
        )
        sys.exit(2)

    import yaml

    src = read(METRICS_GO)
    model = load_model()
    if model is None:
        print("[verify-observability] 代码侧解析失败，后续检查无意义", file=sys.stderr)
        sys.exit(1)
    check_value_prefixes(model, src)

    # ---- 读配置：面板 + 规则（注释剥离后）+ 抓取配置
    dash_files = sorted(DASH_DIR.glob("*.json"))
    if not dash_files:
        record(FAIL, "结构", "%s 下没有面板 JSON" % DASH_DIR)
    rule_files = sorted(p for p in RULES_DIR.glob("*.yml*"))
    active_rules = [p for p in rule_files if p.name.endswith(".yml")]
    if not active_rules:
        record(FAIL, "结构", "%s 下没有 .yml 规则文件（.yml.example 不会被加载）" % RULES_DIR)

    # 面板只取 target 的 expr 原文参与核对，**不 dump 整个 JSON**：
    # json.dumps 会把 PromQL 里的内部双引号转义成 \"，
    # 于是 {result="hit"} 变成 {result=\"hit\"}，选择器正则一条都匹配不到 ——
    # 标签校验会"0 个选择器全部通过"，这是判据最典型的自证失效形态。
    dash_docs = {}
    texts = []
    for p in dash_files:
        try:
            doc = json.loads(p.read_text(encoding="utf-8"))
        except json.JSONDecodeError as exc:
            record(FAIL, "结构", "%s 不是合法 JSON：%s" % (p.name, exc))
            continue
        dash_docs[p] = doc
        exprs = [
            t.get("expr", "")
            for pn in doc.get("panels") or []
            for t in pn.get("targets") or []
        ]
        texts.append(("grafana:" + p.name, "\n".join(exprs)))

    prom_text = strip_yaml_comments(read(PROM_YML))
    texts += [("rules:" + p.name, strip_yaml_comments(p.read_text(encoding="utf-8"))) for p in active_rules]
    texts.append(("prometheus.yml", prom_text))

    metrics, records, selectors, by_lists = collect_refs(texts)

    # 判据自身的守卫：一条选择器都没抽到 = 抽取逻辑失效，而不是"配置很干净"。
    if not selectors:
        record(
            FAIL,
            "选择器抽取",
            "从面板与规则里一条带标签的选择器都没抽到 —— 要么配置真的没有，"
            "要么抽取逻辑坏了；后者会让标签校验恒绿",
        )

    # ---- 1. 引用闭包
    unknown = [(tag, tok) for tag, tok, base in metrics if base not in model]
    if unknown:
        for tag, tok in unknown:
            record(FAIL, "引用闭包", "%s 引用了代码里不存在的指标 %r" % (tag, tok))
    else:
        record(OK, "引用闭包", "%d 个 iip_* 引用全部来自 internal/metrics 常量" % len(metrics))

    # ---- 2. 覆盖
    referenced = {base for _, _, base in metrics}
    missing = sorted(set(model) - referenced)
    if missing:
        record(FAIL, "覆盖", "这些指标没有任何面板引用（埋了但没人看）：%s" % ", ".join(missing))
    else:
        record(OK, "覆盖", "%d 个业务指标都至少被引用一次" % len(model))

    # ---- 3. 标签名与标签值
    label_err = 0
    for tag, metric, body in selectors:
        info = model.get(metric)
        if not info:
            continue  # 引用闭包已经报过
        allowed_labels = info["labels"] | TARGET_LABELS
        for lm in re.finditer(r'(\w+)\s*(=~?)\s*"([^"]*)"', body):
            label, op, raw = lm.group(1), lm.group(2), lm.group(3)
            if label not in allowed_labels:
                record(
                    FAIL,
                    "标签名",
                    "%s 里 %s{%s=...}：%s 没有这个标签（声明的是 %s），"
                    "Prometheus 不会报错，只会永远不匹配"
                    % (tag, metric, label, metric, sorted(info["labels"]) or "无"),
                )
                label_err += 1
                continue
            values = info["allowed"].get(label)
            if values is None:
                continue  # 该标签的值域来自运行期（如 source），静态不可知
            cands = raw.split("|") if op == "=~" else [raw]
            for cand in cands:
                if cand in values:
                    continue
                record(
                    FAIL,
                    "标签值",
                    "%s 里 %s{%s=%r}：不在代码常量值域 %s 内 —— 面板会永久空图"
                    % (tag, metric, label, cand, sorted(values)),
                )
                label_err += 1
    if label_err == 0:
        record(OK, "标签", "%d 个选择器的标签名与取值都在代码值域内" % len(selectors))

    # by(...) 列表：字段名写错不会报错，只会得到一条空标签的曲线
    by_err = 0
    all_labels = set()
    for info in model.values():
        all_labels |= info["labels"]
    all_labels |= TARGET_LABELS
    for tag, names in by_lists:
        for n in names:
            if n not in all_labels:
                record(FAIL, "聚合字段", "%s 的 by(%s)：%s 不是任何指标的标签" % (tag, ",".join(names), n))
                by_err += 1
    if by_err == 0 and by_lists:
        record(OK, "聚合字段", "%d 处 by(...) 的字段名都合法" % len(by_lists))

    # ---- 4. 记录规则：引用的必须在规则文件里定义
    defined = set()
    for p in active_rules:
        try:
            doc = yaml.safe_load(p.read_text(encoding="utf-8")) or {}
        except yaml.YAMLError as exc:
            record(FAIL, "结构", "%s YAML 解析失败：%s" % (p.name, exc))
            continue
        for group in doc.get("groups") or []:
            for rule in group.get("rules") or []:
                if "record" in rule:
                    defined.add(rule["record"])
                    if not rule.get("expr"):
                        record(FAIL, "记录规则", "%s 的 %s 没有 expr" % (p.name, rule["record"]))
                elif "alert" in rule:
                    if not rule.get("expr"):
                        record(FAIL, "记录规则", "%s 的告警 %s 没有 expr" % (p.name, rule["alert"]))
                else:
                    record(FAIL, "结构", "%s 里有一条规则既不是 record 也不是 alert" % p.name)

    # 面板侧的引用（规则文件内部的自引用也算）
    referenced_rules = {tok for tok in (t for _, t in records) if ":" in tok}
    # record: 行本身不构成"引用"，从待校验集合里去掉定义处
    referenced_rules -= defined
    undefined = sorted(referenced_rules - defined)
    if undefined:
        for tok in undefined:
            record(FAIL, "记录规则", "引用了未定义的记录规则 %r —— 面板永久空图" % tok)
    else:
        record(OK, "记录规则", "引用的 %d 个记录规则都有 record 定义" % len(defined))

    # rule_files 通配必须真的匹配到本地规则文件。
    # 只看 active_rules：.yml.example 是**刻意不加载**的，把它算进来等于
    # 逼着人删掉 M0 的联调样本（而它的价值正是"需要时一键恢复"）。
    prom = yaml.safe_load(prom_text)
    globs = prom.get("rule_files") or []
    for p in active_rules:
        name = p.name
        hit = any(
            re.fullmatch(g.rsplit("/", 1)[-1].replace(".", r"\.").replace("*", ".*"), name) for g in globs
        )
        if hit:
            record(OK, "规则加载", "%s 会被 rule_files 匹配" % name)
        else:
            record(
                FAIL,
                "规则加载",
                "%s 不会被 rule_files 的 %s 匹配到 —— 规则文件写了但 Prometheus 从不加载，且没有任何提示"
                % (name, globs),
            )

    # ---- 5. 端口漂移
    ports = config_ports()
    jobs = {j.get("job_name"): j for j in (prom.get("scrape_configs") or [])}
    expect = {"iip-server": ("DefaultHTTPAddr", "/metrics"), "iip-worker": ("DefaultWorkerMetricsAddr", "/metrics")}
    for job, (const, path) in expect.items():
        info = jobs.get(job)
        if not info:
            record(FAIL, "端口漂移", "prometheus.yml 里没有 job %r" % job)
            continue
        if info.get("metrics_path", "/metrics") != path:
            record(FAIL, "端口漂移", "job %s 的 metrics_path 是 %r，应为 %r" % (job, info.get("metrics_path"), path))
        targets = []
        for sc in info.get("static_configs") or []:
            targets += sc.get("targets") or []
        want = ports.get(const)
        got = {t.rsplit(":", 1)[-1] for t in targets}
        if want and got != {want}:
            record(
                FAIL,
                "端口漂移",
                "job %s 抓 %s，而 %s 默认是 :%s —— 改了一处没改另一处，"
                "症状是 target down 但服务健康" % (job, sorted(got), const, want),
            )
        else:
            record(OK, "端口漂移", "job %s → :%s 与 %s 一致" % (job, want, const))

    # ---- 6. 面板结构
    for p, doc in dash_docs.items():
        panels = doc.get("panels") or []
        if not panels:
            record(FAIL, "结构", "%s 没有 panels" % p.name)
            continue
        ids = [pn.get("id") for pn in panels]
        if len(ids) != len(set(ids)):
            record(FAIL, "结构", "%s 的 panel id 有重复：%s" % (p.name, ids))
        ds_vars = {v.get("name") for v in ((doc.get("templating") or {}).get("list") or [])}
        for pn in panels:
            title = pn.get("title")
            if not title:
                record(FAIL, "结构", "%s 有 panel 缺 title" % p.name)
            tgts = pn.get("targets") or []
            if not tgts or not any((t.get("expr") or "").strip() for t in tgts):
                record(FAIL, "结构", "%s 的 panel %r 没有任何 expr" % (p.name, title))
            uid = ((pn.get("datasource") or {}).get("uid") or "")
            ref = re.fullmatch(r"\$\{(\w+)\}", uid)
            if ref and ref.group(1) not in ds_vars:
                record(
                    FAIL,
                    "结构",
                    "%s 的 panel %r 用 ${%s} 引用数据源，但 templating 没定义该变量（导入后必然报 Datasource not found）"
                    % (p.name, title, ref.group(1)),
                )
        record(OK, "结构", "%s：%d 个 panel 结构完整" % (p.name, len(panels)))

    # ---- 汇总
    fails = [r for r in RESULTS if r[0] == FAIL]
    oks = [r for r in RESULTS if r[0] == OK]
    for state, name, detail in RESULTS:
        if state == FAIL or verbose:
            print("  [%s] %-10s %s" % (state, name, detail))
    print("[verify-observability] PASS %d / FAIL %d" % (len(oks), len(fails)))
    if fails:
        print(
            "\n上述 FAIL 全部属于「配置写了但永远不会生效」类：\n"
            "  Prometheus / Grafana 都不会报错，只会在页面上表现为 No data / 空图。",
            file=sys.stderr,
        )
        return 1
    print(
        "  注意：本脚本只证明名字/标签/引用/端口/结构一致，"
        "不证明 PromQL 语法合法、面板能渲染、抓取能成功（需 promtool / Grafana / Docker）。"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
