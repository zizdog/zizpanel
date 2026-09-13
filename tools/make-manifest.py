#!/usr/bin/env python3
"""生成 ZizPanel 的发布清单 manifest.json（在线升级用）。

为什么单独一个脚本而不是写在 Makefile 里：
Makefile 的每条 recipe 行都会起一个新 shell，heredoc 会被拆成一条条命令执行
（我第一次就是因此在 `make release` 里意外调起了 ImageMagick）。
把生成逻辑放进脚本文件，既能被 make 调用，也能单独测试。

清单结构（与 internal/upgrade.Manifest 严格对应）：
    {
      "version": "0.2.0",
      "notes": "更新说明",
      "published_at": "2026-01-01T00:00:00Z",
      "assets": {
        "darwin_arm64": {"url": "...", "sha256": "...", "size": 123},
        "darwin_amd64": {...}
      }
    }

注意：面板侧用 DisallowUnknownFields 解析，所以这里**不能**添加清单里没有的字段，
否则旧面板会拒绝整个清单。

用法：
    python3 tools/make-manifest.py --version 0.2.0 --dir dist/release \
        --base-url https://example.com/releases --notes-file RELEASE_NOTES.md
"""
import argparse
import datetime
import hashlib
import json
import os
import sys

# 必须与 internal/upgrade 支持的架构键一致
ARCHES = ("arm64", "amd64")


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def build_manifest(version, rel_dir, base_url, notes=""):
    assets = {}
    for arch in ARCHES:
        name = f"zizpanel_{version}_darwin_{arch}.tar.gz"
        path = os.path.join(rel_dir, name)
        if not os.path.exists(path):
            raise SystemExit(f"缺少发布包：{path}")
        assets[f"darwin_{arch}"] = {
            "url": f"{base_url.rstrip('/')}/{name}",
            "sha256": sha256_of(path),
            "size": os.path.getsize(path),
        }
    return {
        "version": version,
        "notes": notes,
        "published_at": datetime.datetime.now(datetime.timezone.utc).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        ),
        "assets": assets,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--version", required=True)
    ap.add_argument("--dir", required=True, help="发布包所在目录")
    ap.add_argument("--base-url", required=True, help="发布包对外可下载的地址前缀")
    ap.add_argument("--notes-file", default="", help="可选的更新说明文件")
    args = ap.parse_args()

    notes = ""
    if args.notes_file and os.path.exists(args.notes_file):
        with open(args.notes_file, encoding="utf-8") as f:
            notes = f.read().strip()

    manifest = build_manifest(args.version, args.dir, args.base_url, notes)

    out = os.path.join(args.dir, "manifest.json")
    tmp = out + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, out)
    print(f"==> 已生成清单 {out}（{len(manifest['assets'])} 个架构）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
