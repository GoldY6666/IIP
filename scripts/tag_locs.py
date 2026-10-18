#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""版本标签的唯一位置清单（发版脚本与 E2E 判据共用这一份）。

**为什么要单独成文件**：标签位置原本只在 `verify-e2e.py` 里写了一遍，
于是「发版时该改哪几处」只存在于判据里，人改版还得去读判据代码。
一旦再写一份给 `bump-version.py`，两份清单必然漂移——而漂移的后果
正是 T7.6 修过的那个病：某处停在两代前，没有任何东西会红。

现在只有这一份：`verify-e2e.py` 用它判，`bump-version.py` 用它改。
新增一处标签位置时，两者同时看到，做不到「只改一处」。

每项：(相对路径, 正则)。正则**必须恰含一个捕获组**，且在每个文件里
**恰好匹配一次**——0 次 = 定义被删；多次 = 意义歧义。两者都视为错误。
"""
import re

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
    # 下面三处是「活文档」而非代码：README 的构建示例注释、两个脚本的
    # `--help` 文本里写着「默认 0.1.0」。它们不在 E2E 一致性判据的原清单内，
    # 因为判据只认「会被执行的标签值」；但发版后它们会**主动骗人**（用户照
    # help 以为默认还是旧版）。故纳入 bump 的改动范围，与判据共用同一文件。
    ("deploy/README.md", r'^#\s*等价的一键方式（IMAGE_TAG 默认即 ([0-9][^）]*)）'),
    ("scripts/verify-images.py", r"IIP_TAG\s+镜像标签\s+默认\s+([0-9][^\s（]*)"),
    ("scripts/check-binaries.py", r"IIP_VERSION\s+注入的版本号\s+默认\s+([0-9][^\s（]*)"),
]

# 唯一的权威来源。其余 12 处是它的投影；bump 以它为起点推算出目标版本，
# 而不是让调用者手打一遍版本号（手打就可能只改对一半）。
AUTHORITY = ("Makefile", r'^IMAGE_TAG\s*\?=\s*(\S+)')


def scan(repo):
    """读取各处当前标签。

    返回 (values, errors)：
      values: [(rel, pattern, 当前值)]
      errors: [(rel, 说明)]   —— 文件缺失 / 匹配 0 次或多次
    """
    import os

    values, errors = [], []
    for rel, pat in TAG_LOCS:
        path = os.path.join(repo, rel)
        if not os.path.isfile(path):
            errors.append((rel, "文件不存在"))
            continue
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            hits = re.findall(pat, f.read(), re.M)
        if len(hits) != 1:
            errors.append((rel, "匹配 %d 次（应恰好 1 次）：%s" % (len(hits), pat)))
            continue
        values.append((rel, pat, hits[0]))
    return values, errors
