#!/usr/bin/env python3
"""从 internal/services/binary_release.go 抽出"原生 release 二进制"应用的元数据。

为什么用解析而不是手抄一份清单：手抄的那份**一定会漂移** ——
代码里把 lucky 从 v2.27.2 改到 v2.19.5（用户要求），同步工具却还去下 v2.27.2，
于是镜像上永远缺正确版本，而错误信息只会说"镜像上没有这个资源"。

输出（给 tools/sync-nas-apps.sh 用）：
    [{"id": "lucky", "repo": "gdy666/lucky", "tag": "v2.19.5",
      "asset": "lucky_2.19.5_darwin_arm64.tar.gz", "binary": "lucky",
      "checksums": "frp_sha256_checksums.txt"}, ...]
"""
import argparse
import json
import re
import sys

SRC = "internal/services/binary_release.go"

# 一个应用条目的形状（只取我们要的四项 + 可选的 checksums）
ENTRY = re.compile(
    r'"(?P<id>[a-z0-9-]+)":\s*\{(?P<body>.*?)\n\t\},',
    re.S,
)
FIELD = {
    "id": re.compile(r'ID:\s*"([^"]+)"'),
    "repo": re.compile(r'Repo:\s*"([^"]+)"'),
    "tag": re.compile(r'Tag:\s*"([^"]+)"'),
    "asset": re.compile(r'Asset:\s*"([^"]+)"'),
    "binary": re.compile(r'Binary:\s*"([^"]+)"'),
    "checksums": re.compile(r'ChecksumsAsset:\s*"([^"]+)"'),
}


def parse(path):
    text = open(path, encoding="utf-8").read()
    # 只解析 releaseBinaryApps 这个 map
    start = text.index("var releaseBinaryApps = map[string]releaseBinaryApp{")
    end = text.index("\n}\n", start)
    body = text[start:end]
    out = []
    for m in ENTRY.finditer(body):
        item = {}
        for key, rx in FIELD.items():
            mm = rx.search(m.group("body"))
            if mm:
                item[key] = mm.group(1)
        if "repo" not in item or "asset" not in item:
            continue
        item.setdefault("id", m.group("id"))
        if not item["asset"].endswith("darwin_arm64.tar.gz"):
            # 只要 Apple Silicon 产物：面板只跑 macOS，非 arm64 的条目
            # （比如只在 amd64 上存在的）同步过来也没用。
            continue
        out.append(item)
    # 同 id 去重（同一 id 只会有一条）
    seen, uniq = set(), []
    for it in out:
        if it["id"] in seen:
            continue
        seen.add(it["id"])
        uniq.append(it)
    return uniq


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", default=SRC)
    ap.add_argument("--json", action="store_true", help="输出 JSON（默认给人看）")
    args = ap.parse_args()
    items = parse(args.src)
    if args.json:
        json.dump(items, sys.stdout, ensure_ascii=False, indent=2)
        print()
    else:
        for it in items:
            print(f"{it['id']:16s} {it['repo']:22s} {it.get('tag','?'):10s} {it['asset']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
