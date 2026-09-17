#!/usr/bin/env python3
"""按项目约定的规则递增版本号。

规则（由用户指定）：
    每次功能更新 **+0.0.1**，patch 最大到 10；
    到 10 之后 **+0.1** 并把 patch 归零。

    0.1.0 → 0.1.1 → … → 0.1.10 → 0.2.0 → 0.2.1 → …

为什么不用常规的语义化版本（随手 +0.1 或 +1.0）：
    迭代需要一个**足够细**的粒度（生产机由用户自己在面板里升级）
    才能一眼看出哪台旧了。按功能更新逐次 +1 最直观。

用法：
    python3 tools/bump-version.py            # 递增并写回
    python3 tools/bump-version.py --dry-run  # 只看会变成什么
    python3 tools/bump-version.py --show     # 只打印当前版本
"""
import argparse
import re
import sys
from pathlib import Path

VERSION_GO = Path(__file__).resolve().parent.parent / "internal" / "version" / "version.go"
# install.sh 顶部还有一份 SCRIPT_VERSION（安装横幅显示"安装脚本 vX.Y.Z"）。
# 它必须跟着涨：2026-09-17 实测它停在 1.0.0 而面板已经 1.0.1，用户看到的版本号是假的。
# 同一份事实有两个来源 → 必须由同一个脚本一起改，并在 make check 里互相校验。
INSTALL_SH = Path(__file__).resolve().parent.parent / "install.sh"
PATCH_MAX = 10


def read_version() -> str:
    m = re.search(r'var Version = "([^"]+)"', VERSION_GO.read_text(encoding="utf-8"))
    if not m:
        raise SystemExit(f"在 {VERSION_GO} 里找不到 Version 变量")
    return m.group(1)


def next_version(v: str) -> str:
    m = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)", v.strip())
    if not m:
        raise SystemExit(f"版本号格式不认识：{v}（应为 主.次.修订）")
    major, minor, patch = (int(x) for x in m.groups())
    if patch >= PATCH_MAX:
        # 到 10 就进一位
        return f"{major}.{minor + 1}.0"
    return f"{major}.{minor}.{patch + 1}"


def write_version(new: str) -> None:
    text = VERSION_GO.read_text(encoding="utf-8")
    text = re.sub(r'(var Version = ")[^"]+(")', rf"\g<1>{new}\g<2>", text, count=1)
    VERSION_GO.write_text(text, encoding="utf-8")

    # install.sh 的 SCRIPT_VERSION 同步（缺了它安装横幅会显示旧版本号，而没有任何报错）
    sh = INSTALL_SH.read_text(encoding="utf-8")
    sh2, n = re.subn(r'(SCRIPT_VERSION=")[^"]+(")', rf"\g<1>{new}\g<2>", sh, count=1)
    if n != 1:
        raise SystemExit(f"在 {INSTALL_SH} 里找不到 SCRIPT_VERSION=... —— 版本号会被改一半，已中止")
    INSTALL_SH.write_text(sh2, encoding="utf-8")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true", help="只显示，不写入")
    ap.add_argument("--show", action="store_true", help="只打印当前版本")
    args = ap.parse_args()

    cur = read_version()
    if args.show:
        print(cur)
        return 0

    new = next_version(cur)
    if args.dry_run:
        print(f"{cur} → {new}（未写入）")
        return 0

    write_version(new)
    print(f"版本号：{cur} → {new}")
    print("记得部署到**本机**并验证（生产机由用户自己在面板里升级；见 AGENTS 铁律 5）。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
