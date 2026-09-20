#!/usr/bin/env python3
"""把一版发布件落到"镜像站文档根"那个目录里（`make publish-mirror-local` 用）。

为什么要有它：镜像站与本机同一台机器时（外置镜像盘 `/Volumes/ZPMirror/mirror`），
发版最后一步是"把包和清单放进那个目录"。以前靠手工 rsync，路径写死在别处，
盘一搬走就没人知道该放哪；而且**没人核对"清单里的 sha"与"真放进去的包"是不是一回事**。

三条硬规矩（都真出过事）：
  1. 清单说哪个 sha，就只放 sha 对得上的包 —— 对不上当场失败，绝不"先放过去再说"。
  2. 目标目录里已有**同版本但不同字节**的产物时拒绝覆盖（同版本不同字节 = 用户下到的
     和别人下到的不是一份；要覆盖必须显式 --force）。
  3. 缺哪个平台的包就报哪个平台（清单列了却 404 是坑，不许静默跳过）。

用法：
  python3 tools/publish-local-mirror.py --release-dir dist/release --dest <镜像文档根> \
      --install-sh install.sh [--force]
"""

import argparse
import hashlib
import json
import os
import shutil
import sys


def sha256_file(path, chunk=1 << 20):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(chunk)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def load_manifest(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--release-dir", required=True)
    ap.add_argument("--dest", required=True)
    ap.add_argument("--install-sh", default="install.sh")
    ap.add_argument("--force", action="store_true")
    args = ap.parse_args()

    rel = args.release_dir
    dest = args.dest
    manifest_path = os.path.join(rel, "manifest.json")
    if not os.path.isfile(manifest_path):
        print(f"!! 没有清单：{manifest_path}（先跑 make mirror-public）", file=sys.stderr)
        return 1
    man = load_manifest(manifest_path)
    version = str(man.get("version") or "")
    assets = man.get("assets") or {}
    if not version or not assets:
        print(f"!! 清单缺 version/assets：{manifest_path}", file=sys.stderr)
        return 1

    # 规矩 2：同版本不同字节 → 拒绝（防止"版本号没变、内容变了"的静默替换）。
    dest_man_path = os.path.join(dest, "manifest.json")
    if os.path.isfile(dest_man_path):
        try:
            old = load_manifest(dest_man_path)
        except (OSError, ValueError) as e:
            print(f"!! 目标清单读不出来（{dest_man_path}）：{e}", file=sys.stderr)
            return 1
        if str(old.get("version")) == version:
            for key, info in assets.items():
                o = (old.get("assets") or {}).get(key) or {}
                if o.get("sha256") and o.get("sha256") != (info or {}).get("sha256"):
                    if not args.force:
                        print(
                            f"!! 同版本（{version}）不同产物，拒绝覆盖：\n"
                            f"   目标已有 {key} {str(o.get('sha256'))[:16]}…\n"
                            f"   本次产物 {key} {str((info or {}).get('sha256'))[:16]}…\n"
                            f"   要覆盖请显式加 FORCE=1（确认这是你要发的那一份）",
                            file=sys.stderr,
                        )
                        return 1
                    print(f"   ! FORCE=1：覆盖同版本 {key} 的不同字节产物")

    # 规矩 1 + 3：先全部核对，再动手复制（半个镜像比没镜像更难查）。
    plan = []
    for key, info in sorted(assets.items()):
        name = (info or {}).get("name") or os.path.basename((info or {}).get("url") or "")
        if not name:
            print(f"!! 清单里 {key} 没给出文件名", file=sys.stderr)
            return 1
        src = os.path.join(rel, name)
        if not os.path.isfile(src):
            print(f"!! 清单列了 {key}（{name}）但产物不在：{src}", file=sys.stderr)
            return 1
        want = (info or {}).get("sha256") or ""
        got = sha256_file(src)
        if want and got != want:
            print(
                f"!! {name} 的 sha256 与清单不符（清单 {want[:16]}… / 实际 {got[:16]}…）\n"
                f"   这份包不是清单描述的那一份，拒绝发布",
                file=sys.stderr,
            )
            return 1
        plan.append((key, name, src, got))

    os.makedirs(os.path.join(dest, "download", version), exist_ok=True)
    os.makedirs(os.path.join(dest, "download", "latest"), exist_ok=True)

    copied = []
    for key, name, src, digest in plan:
        for target in (
            os.path.join(dest, name),
            os.path.join(dest, "download", version, name),
        ):
            shutil.copy2(src, target)
        # latest 命名的同名文件：install.sh 的固定 URL 用它。
        latest = name.replace(f"_{version}_", "_latest_")
        latest_src = os.path.join(rel, latest)
        if os.path.isfile(latest_src):
            for target in (
                os.path.join(dest, latest),
                os.path.join(dest, "download", "latest", latest),
            ):
                shutil.copy2(latest_src, target)
        else:
            print(f"   ! 缺 {latest}（未复制到 latest/）")
        copied.append((key, name, digest))

    shutil.copy2(manifest_path, os.path.join(dest, "manifest.json"))
    sig = manifest_path + ".sig"
    if os.path.isfile(sig):
        shutil.copy2(sig, os.path.join(dest, "manifest.json.sig"))
    else:
        print("!! 清单没有签名文件 .sig（面板会拒绝这份升级源）", file=sys.stderr)
        return 1
    if os.path.isfile(args.install_sh):
        shutil.copy2(args.install_sh, os.path.join(dest, "install.sh"))
    else:
        print(f"!! 找不到 {args.install_sh}（镜像上少了安装脚本）", file=sys.stderr)
        return 1

    # 回读校验：落到目标目录里的那份清单必须是这个版本、这个 sha。
    back = load_manifest(dest_man_path)
    if str(back.get("version")) != version:
        print(f"!! 回读版本不符：{back.get('version')} != {version}", file=sys.stderr)
        return 1
    for key, name, digest in copied:
        info = (back.get("assets") or {}).get(key) or {}
        if info.get("sha256") and info["sha256"] != digest:
            print(f"!! 回读 {key} 的 sha 不符", file=sys.stderr)
            return 1

    print(f"   ✓ 已放到 {dest}")
    print(f"   ✓ 版本 {version}，说明 {len(str(back.get('notes') or ''))} 字，"
          f"平台 {', '.join(k for k, _, _ in copied)}")
    for key, name, digest in copied:
        print(f"     {key}: {name} {digest[:16]}…")
    print(f"   清单 URL 基址：{next(iter((back.get('assets') or {}).values())).get('url', '')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
