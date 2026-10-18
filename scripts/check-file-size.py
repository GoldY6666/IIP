#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""单文件 ≤ 300 行门禁（OPEN-M5-7 / OPEN-M6-1 的守卫）。

**为什么需要这个脚本**：约束原文在 `docs/IIP-M1详细开发手册.md` §4
（「单文件 ≤ 300 行」），但它在 M2–M6 之间**从未被任何脚本检查过** ——
于是 `cmd/iip-server/main.go` 从 318 涨到 485，`internal/config/config.go`
从 352 涨到 616，中间没有人发现。约束写在文档里不等于门禁。

**为什么不直接对全部文件判红**：存量欠债 44 个（含 14 个测试文件、
`loki.go` 551 行等），一次判红只会让门禁被 `# noqa` 掉。故采用**基线**：
`scripts/file-size-baseline.txt` 记录今天的欠债，规则是**只减不增**。

五条判据（每条都对应一种「让门禁形同虚设」的办法）：
  1. 新欠债：文件 > 300 行且不在基线        → 红（想偷偷加长）
  2. 已还清却未移除：在基线但实际 ≤ 300     → 红（白名单变永久豁免）
  3. 基线值过期：在基线且行数 ≠ 基线值      → 红（改代码不改账，账就不可信）
  4. 基线指向不存在的文件                   → 红（账目对不上现实）
  5. 扫描到 0 个文件                        → 红（路径写错时**不能全绿**）

**生成物不检查**：凡是头部含 `Code generated ... DO NOT EDIT` 的文件
（sqlc 产出的 `internal/db/*.sql.go`）一律跳过 —— 对着生成物报「请拆分」
是让人去改会被覆盖的文件，纯属噪音。判据绑**标记**而不是硬编码路径，
这样新生成的产物自动免疫。
"""

import io
import os
import sys

LIMIT = 300
ROOTS = ("cmd", "internal", "migrations")
BASELINE = os.path.join("scripts", "file-size-baseline.txt")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# 生成标记：文件前 5 行内出现即跳过（go 惯例写在 package 声明之前）。
GENERATED_MARKERS = ("Code generated", "DO NOT EDIT")


def say(s=""):
    print(s, flush=True)


def is_generated(path):
    try:
        with io.open(path, encoding="utf-8", errors="replace") as f:
            head = "".join([f.readline() for _ in range(5)])
    except OSError:
        return False
    return all(m in head for m in GENERATED_MARKERS)


def count_lines(path):
    """物理行数。与基线同一口径（splitlines），不用 wc 以免末尾换行差异。"""
    with io.open(path, encoding="utf-8", errors="replace") as f:
        return len(f.read().splitlines())


def scan():
    """返回 {相对路径: 行数}，只含真正受检的文件。"""
    out = {}
    for root in ROOTS:
        base = os.path.join(REPO, root)
        for dirpath, _, files in os.walk(base):
            for name in files:
                if not name.endswith(".go"):
                    continue
                full = os.path.join(dirpath, name)
                if is_generated(full):
                    continue
                rel = os.path.relpath(full, REPO).replace("\\", "/")
                out[rel] = count_lines(full)
    return out


def load_baseline():
    """返回 ({路径: 行数}, [解析失败的行])。"""
    path = os.path.join(REPO, BASELINE)
    rows, bad = {}, []
    if not os.path.exists(path):
        return rows, ["基线文件不存在：%s" % BASELINE]
    with io.open(path, encoding="utf-8") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            if len(parts) != 2 or not parts[1].isdigit():
                bad.append("%s:%d 解析失败：%r" % (BASELINE, lineno, line))
                continue
            rows[parts[0]] = int(parts[1])
    return rows, bad


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "--baseline-out":
        # 只打印建议基线，**不写文件**：自动改基线等于自己给自己盖章。
        files = scan()
        for p in sorted(files):
            if files[p] > LIMIT:
                say("%s %d" % (p, files[p]))
        return 0

    files = scan()
    base, bad = load_baseline()
    fails = list(bad)

    # 判据 5：自守卫。路径写错 / 目录改名时绝不能"零违规"通过。
    if not files:
        fails.append("扫描到 0 个 Go 文件（ROOTS=%s）—— 路径失效，判据无效" % (ROOTS,))

    # 判据 1：新欠债
    for p, n in sorted(files.items()):
        if n > LIMIT and p not in base:
            fails.append("新欠债：%s 有 %d 行（上限 %d），且不在基线" % (p, n, LIMIT))

    # 判据 2/3/4：基线自身的账目是否可信
    for p, recorded in sorted(base.items()):
        if p not in files:
            fails.append("基线指向不存在的文件：%s" % p)
            continue
        actual = files[p]
        if actual <= LIMIT:
            fails.append(
                "已还清却未移除：%s 现为 %d 行（≤%d），请从基线删掉这一行"
                % (p, actual, LIMIT))
        elif actual != recorded:
            fails.append(
                "基线过期：%s 记录 %d 行，实际 %d 行（改代码必须同步改账）"
                % (p, recorded, actual))

    over = sorted(((n, p) for p, n in files.items() if n > LIMIT), reverse=True)
    say("受检 Go 文件 %d 个（已排除生成物）；超限 %d 个，基线登记 %d 个"
        % (len(files), len(over), len(base)))
    if over:
        worst = over[0]
        say("最大文件：%s（%d 行）" % (worst[1], worst[0]))
    if fails:
        say()
        for msg in fails:
            say("[FAIL] " + msg)
        say()
        say("共 %d 项违规。基线只能变短：修好一个文件就从基线删一行。" % len(fails))
        return 1
    say("全部通过（阈值 %d 行，判据 5 条）" % LIMIT)
    return 0


if __name__ == "__main__":
    sys.exit(main())
