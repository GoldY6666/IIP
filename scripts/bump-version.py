#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""发版时统一改版本标签（唯一权威 = Makefile 的 IMAGE_TAG）。

**为什么需要这个脚本**：发版要改 16 处标签（Makefile / compose×4 /
deploy README×3 / Chart×2 / values / verify-images×2 / check-binaries×2 /
tasks.ps1）。T7.6 手工对齐时 `Makefile` 曾连续两代停在 `0.0-m1` 而无人
发现——因为当时**没有任何判据**。现在判据有了（E2E S1 的标签四联动），
但手工改仍然会漏：判据只能事后告诉你「不一致」，不能告诉你怎么改对。
位置清单在 `scripts/tag_locs.py`，**判据与脚本共用同一份**（两份清单必然漂移）。

脚本做三件事：
  1. `--check`：只报告，列出每处当前值 + 与权威值是否一致（**不改文件**）
  2. `--write`：把 16 处全部改成目标版本，改完立刻重扫并自证一致
  3. `--tag`：只打印建议的 git tag 命令（`git tag -a vX.Y.Z`），不执行

**为什么不让调用者手打版本号**：手打就可能只改对一半。目标版本由
`--to` 指定，或由 `--bump {major,minor,patch}` 从权威值推算。

**为什么 --write 后要重扫**：文件写成功不等于改对了——正则可能在替换后
匹配 0 次（比如把 `version:` 行连同缩进一起替换掉），那时必须自己报出来，
而不是让人去跑一遍 E2E 才发现。

**「没有改动可做」的判据看各处，不看权威处**：权威处已改成新版本、其余
仍停在旧版本时，若只比权威值就会误报「没改动」而把剩下几处留在旧版本里
（v0.2.0 发版真实踩到，W6 变异锁死）。

**基线自证**（`D:\\_iip_setup\\_mut_bump.py` 六条变异全红）：
  ① 只改权威处不改其余 -> --check 必须红
  ② 目标值与现有相同 -> --write 必须拒绝（否则会误以为「改过了」）
  ③ 某处正则匹配 0 次 -> 必须报错而非静默跳过
  ④ 某处匹配多次 -> 必须报歧义
  ⑤ --write 只改捕获组，行内注释里的旧版本号不许被连带改掉
  ⑥ 权威处已改、其余未改 -> 仍要改完（不许说「没有改动可做」）
"""

import argparse
import os
import re
import subprocess
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
from tag_locs import AUTHORITY, TAG_LOCS, scan  # noqa: E402

REPO = os.path.dirname(_HERE)
SEMVER = re.compile(r"^(\d+)\.(\d+)\.(\d+)$")


def say(s=""):
    print(s, flush=True)


def authority_value():
    """从权威处读当前版本。读不到就退出——没有权威值就没法推算目标。"""
    rel, pat = AUTHORITY
    path = os.path.join(REPO, rel)
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        hits = re.findall(pat, f.read(), re.M)
    if len(hits) != 1:
        raise SystemExit("权威处 %s 匹配 %d 次（应为 1）：%s"
                         % (rel, len(hits), pat))
    return hits[0]


def compute_target(current, bump, explicit):
    m = SEMVER.match(current)
    if not m:
        raise SystemExit("权威版本 %r 不是 X.Y.Z，无法推算；请用 --to 明确指定"
                         % current)
    if explicit:
        if not SEMVER.match(explicit):
            raise SystemExit("--to %r 不是 X.Y.Z" % explicit)
        return explicit
    major, minor, patch = (int(x) for x in m.groups())
    if bump == "major":
        return "%d.0.0" % (major + 1)
    if bump == "minor":
        return "%d.%d.0" % (major, minor + 1)
    return "%d.%d.%d" % (major, minor, patch + 1)


def git_tag():
    rc = subprocess.run(["git", "describe", "--tags", "--abbrev=0"], cwd=REPO,
                        capture_output=True, text=True, encoding="utf-8",
                        errors="replace")
    return rc.stdout.strip() if rc.returncode == 0 else ""


def report(values, errors, target=None):
    """打印每处当前值；target 给定时标注是否已是目标值。"""
    bad = 0
    for rel, pat, val in values:
        if target is None:
            mark = " "
        else:
            mark = "OK" if val == target else "!!"
            bad += 0 if val == target else 1
        say("  [%s] %-32s %-16s %s" % (mark, rel, val, pat))
    for rel, why in errors:
        say("  [ERR] %-32s %s" % (rel, why))
    return bad


def do_check(target):
    values, errors = scan(REPO)
    cur = authority_value()
    say("权威值（Makefile IMAGE_TAG）= %s" % cur)
    say("git 最新 tag = %s" % (git_tag() or "（无）"))
    bad = report(values, errors, target)
    distinct = sorted({v for _, _, v in values})
    if errors:
        say()
        say("结论：%d 处无法确定（见 ERR）" % len(errors))
        return 1
    if len(distinct) != 1:
        say()
        say("结论：出现 %d 个不同标签：%s" % (len(distinct), "、".join(distinct)))
        return 1
    if target is not None and bad:
        say()
        say("结论：%d 处还没改到 %s" % (bad, target))
        return 1
    say()
    say("结论：%d 处全部一致 = %s" % (len(values), distinct[0]))
    return 0


def do_write(target):
    values, errors = scan(REPO)
    if errors:
        say("写之前就有问题，拒绝写入（免得改到一半）：")
        report(values, errors)
        return 1

    stale = [(rel, pat, v) for rel, pat, v in values if v != target]
    if not stale:
        say("目标 %s 已全部就位，没有改动可做。" % target)
        return 2

    # 判断「没改动」只能看**各处是否都已是目标值**，不能只看权威处。
    # 只看权威处会漏一种真实情形：权威处已手改成新版本、其余还停在旧版本
    # （本次发版就这样发生过一次），那时说「没改动」会让 3 处活在旧版本里。
    cur = values[0][2] if values else "?"
    say("把 %d 处改到 %s（当前权威处 = %s）：" % (len(stale), target, cur))
    changed = []
    for rel, pat, old in stale:
        path = os.path.join(REPO, rel)
        with open(path, "r", encoding="utf-8", newline="") as f:
            src = f.read()
        # 只替换捕获组命中的那一处，保留整行其余内容（缩进、引号、行尾注释）。
        # 用 re.sub 且 count=1 而不是 str.replace：版本号可能在该行出现两次
        # （如 `iip-server:0.1.0` 与注释里的 0.1.0），字符串替换会改到注释。
        new_src, n = re.subn(pat,
                             lambda m: m.group(0).replace(m.group(1), target, 1),
                             src, count=1, flags=re.M)
        if n != 1:
            say("  [ERR] %s 替换失败（匹配 %d 次），已中止，未写入任何文件"
                % (rel, n))
            return 1
        if new_src == src:
            say("  [ERR] %s 内容未变（正则捕获组未命中？），已中止" % rel)
            return 1
        with open(path, "w", encoding="utf-8", newline="") as f:
            f.write(new_src)
        changed.append(rel)
        say("  [OK] %s  %s -> %s" % (rel, old, target))

    # 自证：写完必须重扫一遍。写成功 ≠ 改对了（正则可能把行改坏）。
    say()
    say("自证重扫：")
    rc = do_check(target)
    if rc != 0:
        say()
        say("写入完成但自证失败——文件已改，请按上面 ERR 修。")
        return 1
    say()
    say("改完 %d 个文件。下一步（我不执行，由你在本机跑）：" % len(set(changed)))
    say("  git add -A && git commit -m \"发版 %s\"" % target)
    say("  git tag -a v%s -m \"IIP v%s\"" % (target, target))
    say("  git push origin <要发布的提交> && git push origin v%s" % target)
    return 0


def main():
    ap = argparse.ArgumentParser(
        description="统一改版本标签（权威值 = Makefile IMAGE_TAG）")
    ap.add_argument("--to", help="目标版本 X.Y.Z")
    ap.add_argument("--bump", choices=["major", "minor", "patch"],
                    help="从权威值推算目标版本")
    ap.add_argument("--write", action="store_true",
                    help="真写文件（默认只检查）")
    ap.add_argument("--check", action="store_true",
                    help="只检查一致性（默认行为）")
    ap.add_argument("--tag", action="store_true",
                    help="只打印建议的 git tag 命令")
    args = ap.parse_args()

    cur = authority_value()
    if args.tag:
        say("git tag -a v%s -m 'IIP v%s'" % (cur, cur))
        say("git push origin v%s" % cur)
        return 0

    target = None
    if args.to or args.bump:
        target = compute_target(cur, args.bump, args.to)

    if args.write:
        if not target:
            say("--write 需要 --to 或 --bump 之一")
            return 2
        return do_write(target)
    return do_check(target)


if __name__ == "__main__":
    sys.exit(main())
