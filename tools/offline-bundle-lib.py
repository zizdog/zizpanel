#!/usr/bin/env python3
# ============================================================================
#  offline-bundle-lib.py —— 把"一个应用的离线打包计划"变成"真的文件 + 清单"
#
#  给 tools/build-offline-bundle.sh 调用。做三件**必须联网且必须真算**的事：
#    ① 按计划（brew 要展开依赖闭包）算出要哪些文件、从镜像的哪个 URL 取；
#    ② 逐个下载到暂存目录，**真的算 sha256**（绝不抄上游声明值）；
#    ③ 写 manifest.json —— 每个 artifact 的 path / url(上游) / sha256 / size / kind。
#
#  诚实纪律（这个仓库最贵的教训）：
#    · 任何"镜像上有声明、但取不到"的文件 → 进 missing，**不静默跳过**；
#    · 下载到的文件与声明的 sha256 不一致 → 立刻失败（宁可没有离线包，
#      也不发一个内容对不上的包）；
#    · 缺件数与制品数会在最后对账（见 shell 侧的"计数核对"）。
#
#  用法：
#    python3 tools/offline-bundle-lib.py --plan plan.json --app frpc \
#        --stage /tmp/stage --base http://192.168.1.8:8090 --tag arm64_sequoia
#  stdout：构建报告 JSON（供 shell 汇总）；进度写 stderr。
# ============================================================================
import argparse
import hashlib
import importlib.util
import json
import os
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
UA = "zizpanel-offline-builder/1.0"
CHUNK = 1 << 20


def _load_brew_resolver():
    """按路径加载同目录的 offline-brew-resolve.py（文件名带 -，不能用 import）。"""
    path = os.path.join(HERE, "offline-brew-resolve.py")
    spec = importlib.util.spec_from_file_location("offline_brew_resolve", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def fetch_to(url, dest, timeout=1800):
    """下载到 dest，返回 (sha256, size)。"""
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    h = hashlib.sha256()
    n = 0
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    with urllib.request.urlopen(req, timeout=timeout) as r, open(tmp, "wb") as f:
        while True:
            b = r.read(CHUNK)
            if not b:
                break
            h.update(b)
            n += len(b)
            f.write(b)
    os.replace(tmp, dest)
    return h.hexdigest(), n


def filelist_for(plan, base, tags, resolve_brew):
    """返回 (artifacts, missing, warnings, resolved_version)。

    resolved_version 只有 brew 才有值（要从公式清单里读 stable+revision）：
    离线包的目录名应当用**真实版本**，而不是含糊的 "current"。
    """
    arts, missing, warns = [], [], []
    resolved = ""

    if plan.get("brew_formula"):
        r = resolve_brew(base, plan["brew_formula"], tags)
        resolved = r.get("version") or ""
        missing.extend(r.get("missing") or [])
        warns.extend(r.get("warnings") or [])
        for a in r.get("artifacts") or []:
            arts.append({
                "kind": a["kind"],
                "path": a["serve_path"],
                "source_url": a["source_url"],
                "url": a.get("upstream_url") or "",
                "sha256": a.get("sha256") or "",
                "size": a.get("size") or -1,
                "formula": a.get("formula", ""),
                "tag": a.get("tag", ""),
                "note": a.get("note", ""),
            })
        return arts, missing, warns, resolved

    for a in plan.get("artifacts") or []:
        mp = (a.get("mirror_path") or "").strip()
        kind = a.get("kind") or "other"
        if kind == "other":
            # 说明性条目（例如 compose 的镜像名单、自包含说明），不落盘也不当缺件
            continue
        if not mp:
            missing.append({
                "kind": kind,
                "path": a.get("path") or "",
                "why": "镜像上还没有这个文件；建议路径见 gap 报告",
                "url": a.get("upstream_url") or "",
            })
            continue
        arts.append({
            "kind": kind,
            "path": a["path"],
            "source_url": base.rstrip("/") + "/" + mp.lstrip("/"),
            "url": a.get("upstream_url") or "",
            "sha256": "",
            "size": -1,
            "note": a.get("note") or "",
        })
    return arts, missing, warns, resolved


def build_app(plan, base, stage, tags, resolve_brew, dry_run=False):
    arts, missing, warns, resolved = filelist_for(plan, base, tags, resolve_brew)
    out = []
    for a in arts:
        rel = a["path"]
        dest = os.path.join(stage, rel)
        declared = a.get("sha256") or ""
        if dry_run:
            out.append(a)
            continue
        label = f"{plan['id']}/{rel}"
        print(f"  取件 {label}", file=sys.stderr, flush=True)
        try:
            sha, size = fetch_to(a["source_url"], dest)
        except urllib.error.HTTPError as e:
            missing.append({"kind": a["kind"], "path": rel,
                            "why": f"HTTP {e.code}：{a['source_url']}"})
            continue
        except Exception as e:
            missing.append({"kind": a["kind"], "path": rel,
                            "why": f"{type(e).__name__}：{e}（{a['source_url']}）"})
            continue
        if declared and declared != sha:
            # 声明值与实测值不一致：这是**不能放过**的一类错误。
            print(f"    ✗ sha256 不一致：声明 {declared[:12]}… 实测 {sha[:12]}…", file=sys.stderr)
            missing.append({"kind": a["kind"], "path": rel,
                            "why": f"sha256 不一致：清单声明 {declared}，实测 {sha}"})
            continue
        a = dict(a)
        a["sha256"] = sha
        a["size"] = size
        out.append(a)

    total = sum(int(a.get("size") or 0) for a in out)
    return {
        "app": plan["id"],
        "name": plan.get("name") or plan["id"],
        # brew 的版本要在构建时从公式清单里解析（stable + revision）；
        # 其余类型用计划里的版本，实在没有才退回 "current"。
        "version": resolved or plan.get("version") or "current",
        "install_method": plan.get("install_method") or "",
        "category": plan.get("category") or "",
        "brew": ({"formula": plan["brew_formula"],
                  "api_dir": "artifacts/brew/api",
                  "bottle_dir": "artifacts/brew"} if plan.get("brew_formula") else None),
        "docker_images": plan.get("docker_images") or [],
        "artifacts": out,
        "missing": missing,
        "warnings": warns,
        "gaps": plan.get("gaps") or [],
        "bytes": total,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", required=True)
    ap.add_argument("--app", required=True)
    ap.add_argument("--stage", required=True)
    ap.add_argument("--base", required=True)
    ap.add_argument("--tag", action="append", default=[])
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    with open(a.plan, encoding="utf-8") as f:
        doc = json.load(f)
    plan = None
    for p in doc.get("apps") or []:
        if p["id"] == a.app:
            plan = p
            break
    if plan is None:
        print(f"计划里没有应用 {a.app}", file=sys.stderr)
        return 2

    resolve_brew = _load_brew_resolver().resolve
    rep = build_app(plan, a.base, a.stage, a.tag or ["arm64_sequoia"],
                    resolve_brew, dry_run=a.dry_run)
    rep["schema"] = "zizpanel.offline/v1"
    rep["generated_at"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    rep["generated_by"] = "tools/build-offline-bundle.sh"
    rep["mirror_base"] = a.base.rstrip("/")
    rep["totals"] = {
        "artifacts": len(rep["artifacts"]),
        "bytes": rep["bytes"],
        "missing": len(rep["missing"]),
    }
    json.dump(rep, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
