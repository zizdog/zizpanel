#!/usr/bin/env python3
# ============================================================================
#  offline-brew-resolve.py —— 把"一个 Homebrew formula"展开成离线包需要的文件清单
#
#  给 tools/build-offline-bundle.sh 用。单独一个脚本而不是全写进 bash：brew 的
#  依赖是**闭包**（ffmpeg 一个条目 = 15 个瓶），文件名规则也不是猜得出来的。
#
#  ★ 文件名规则（**从 mini 上 Homebrew 7.0.2 的源码逐行核对出来的**，不是猜的：
#    /opt/homebrew/Library/Homebrew/bottle.rb 的 Bottle::Filename、github_packages.rb
#    的 version_rebuild、以及 formula/pkg_version.rb）：
#
#      pkg_version = versions.stable + ("_"+revision if revision>0)
#      flat 文件名 = "<name>-<pkg_version>.<tag>.bottle[.<bottle_rebuild>].tar.gz"
#      manifest 路径 = "<name @→/ +→x>/manifests/<pkg_version>[-<bottle_rebuild>]"
#
#    真机对照（NAS 上 brew 自己的 access log，2026-09-16）：
#      ffmpeg      revision=1 rebuild=0 → /brew/ffmpeg/manifests/9.0.1_1
#                                        /brew/ffmpeg-9.0.1_1.arm64_sequoia.bottle.tar.gz
#      abseil      revision=0 rebuild=1 → /brew/abseil/manifests/20260817.0-1
#                                        /brew/abseil-20260817.0.arm64_sequoia.bottle.1.tar.gz
#      mysql@8.4   revision=4 rebuild=0 → /brew/mysql/8.4/manifests/8.4.11_4
#                                        /brew/mysql%408.4-8.4.11_4.arm64_sequoia.bottle.tar.gz
#
#    ⚠️ 这里最容易踩的坑：**只看 versions.stable 是不够的**。我第一次就是漏了
#    revision，算出 `ffmpeg-9.0.1...`（实际是 `9.0.1_1`），然后报"镜像上没有这个瓶"
#    —— 而镜像上其实有。少一个 revision 就会把一个装得上的应用误判成缺件。
#
#  输出（stdout，JSON）：见下方 return 的字段。
# ============================================================================
import argparse
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

UA = "zizpanel-offline-builder/1.0"


def http(url, method="GET", timeout=60, want_body=False):
    req = urllib.request.Request(url, method=method, headers={"User-Agent": UA})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read() if want_body else b""
            return r.status, dict(r.headers), body
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers or {}), b""
    except Exception:
        return 0, {}, b""


def api_formula(base, name):
    url = f"{base}/brew/api/formula/{urllib.parse.quote(name, safe='@')}.json"
    st, _, body = http(url, timeout=30, want_body=True)
    if st != 200 or not body:
        return None, url
    try:
        return json.loads(body), url
    except Exception:
        return None, url


def head_size(url):
    st, hdrs, _ = http(url, method="HEAD", timeout=90)
    if st != 200:
        return None
    try:
        return int(hdrs.get("Content-Length") or -1)
    except Exception:
        return -1


def pick_tag(files, tags):
    for t in tags:
        if t in files:
            return t
    if "all" in files:
        return "all"
    return None


def pkg_version(man):
    """Homebrew 的 PkgVersion#to_s：版本号带 formula revision。"""
    stable = ((man.get("versions") or {}).get("stable") or "")
    rev = int(man.get("revision") or 0)
    return f"{stable}_{rev}" if rev > 0 else stable


def flat_filename(name, pkg, tag, rebuild):
    """返回 (落盘文件名, URL 里的文件名)。

    ⚠️ 这两个**必须分开** —— 实测踩出来的坑：
    brew 请求的是 `.../brew/openssl%403-3.6.4.arm64_sequoia.bottle.tar.gz`
    （url_encode 过），nginx 会把 %40 解回 `@`，真正落盘的文件名是
    `openssl@3-3.6.4.arm64_sequoia.bottle.tar.gz`。
    第一版把 url_encode 的结果当成了落盘名 → 磁盘上真有个叫 `%40` 的文件，
    **本地核对全过、HTTP 侧核对立刻 404**。这正是"必须从 HTTP 侧重下重算"
    这条纪律的价值：只看本地文件永远发现不了。
    """
    ext = f".{tag}.bottle"
    if rebuild > 0:
        ext += f".{rebuild}"
    raw = f"{name}-{pkg}{ext}.tar.gz"
    return raw, urllib.parse.quote(raw, safe="")


def manifest_path(name, pkg, rebuild):
    image = name.replace("@", "/").replace("+", "x")
    tag = f"{pkg}-{rebuild}" if rebuild > 0 else pkg
    return f"{image}/manifests/{tag}"


def resolve(base, root, tags):
    seen, order, missing, warnings = set(), [], [], []

    def walk(name):
        if name in seen:
            return
        seen.add(name)
        man, url = api_formula(base, name)
        if man is None:
            missing.append({"formula": name,
                            "why": f"镜像上取不到公式清单 {url}（{root} 的依赖）"})
            return
        for dep in man.get("dependencies") or []:
            if dep.startswith("--"):
                continue
            walk(dep)
        order.append(name)

    walk(root)

    artifacts, version, chosen_tag = [], None, None

    for name in order:
        man, man_url = api_formula(base, name)
        if man is None:
            continue
        bs = ((man.get("bottle") or {}).get("stable") or {})
        files = bs.get("files") or {}
        rebuild = int(bs.get("rebuild") or 0)
        pkg = pkg_version(man)
        tag = pick_tag(files, tags)
        if name == root:
            version, chosen_tag = pkg, tag
        if tag is None:
            missing.append({"formula": name,
                            "why": f"公式清单里没有本机可用的瓶（tags={sorted(files)}）"})
            continue
        entry = files[tag] or {}
        sha = entry.get("sha256") or ""
        if not sha:
            missing.append({"formula": name, "why": f"清单里 {tag} 没有 sha256"})
            continue
        ghcr = entry.get("url") or ""

        # 1) 公式清单本身 —— 离线模式下它就是 HOMEBREW_API_DOMAIN 的内容。
        #    必须与我们要发的瓶**同源同版本**：brew 会拿它算瓶文件名。
        artifacts.append({
            "kind": "brew_formula_json",
            "serve_path": f"artifacts/brew/api/formula/{urllib.parse.quote(name, safe='@')}.json",
            "source_url": man_url,
            "upstream_url": f"https://formulae.brew.sh/api/formula/{urllib.parse.quote(name, safe='@')}.json",
            "sha256": "",
            "size": -1,
            "formula": name,
        })

        # 2) 瓶。先在镜像上按 brew 会请求的平铺路径找；找不到就退到 OCI blob 路径
        #    取文件，但**落盘仍用平铺名**（brew 在自定义 bottle domain 下请求的是
        #    平铺名，见 utils/bottles.rb 的 path_resolved_basename）。
        flat_raw, flat_enc = flat_filename(name, pkg, tag, rebuild)
        flat_url = f"{base}/brew/{flat_enc}"
        blob = f"{base}/brew/v2/homebrew/core/{urllib.parse.quote(name, safe='@')}/blobs/sha256:{sha}"
        size = head_size(flat_url)
        source, note = flat_url, ""
        if size is None:
            size = head_size(blob)
            if size is None:
                missing.append({"formula": name,
                                "why": f"镜像上既没有平铺瓶 {flat_url}，也没有 OCI blob {blob}"})
                continue
            source = blob
            note = (f"镜像的平铺路径 {flat_enc} 缺失（brew 自己在线装也会回落 ghcr！）；"
                    f"离线包改从 OCI blob 取件，仍按平铺名落盘")
            warnings.append(f"{name}: {note}")
        artifacts.append({
            "kind": "brew_bottle",
            # 落盘用**解码后的**名字（nginx 会把 %40 解回 @，磁盘上就是 @）
            "serve_path": f"artifacts/brew/{flat_raw}",
            "source_url": source,
            "upstream_url": ghcr or f"https://ghcr.io/v2/homebrew/core/{name}/blobs/sha256:{sha}",
            "sha256": sha,
            "size": size,
            "formula": name,
            "tag": tag,
            "pkg_version": pkg,
            "bottle_rebuild": rebuild,
            "note": note,
        })

        # 3) OCI manifest。brew 取瓶前会先拉它拿 relocation tab；缺了不致命
        #    （formula_installer.rb 只打一条 opoo 然后做 full relocation），
        #    但有了更接近原生、也更省事，所以尽力带上。
        mpath = manifest_path(name, pkg, rebuild)
        murl = f"{base}/brew/{mpath}"
        msize = head_size(murl)
        if msize is None:
            warnings.append(f"{name}: 镜像上没有 OCI manifest {murl}；"
                            f"瓶在，brew 会做 full relocation（不致命）")
        else:
            artifacts.append({
                "kind": "brew_manifest",
                "serve_path": f"artifacts/brew/{mpath}",
                "source_url": murl,
                "upstream_url": "",
                "sha256": "",
                "size": msize,
                "formula": name,
            })

    total = sum(a["size"] for a in artifacts if (a.get("size") or -1) > 0)
    return {
        "formula": root,
        "version": version or "",
        "tag": chosen_tag or "",
        "formulas": order,
        "artifacts": artifacts,
        "missing": missing,
        "warnings": warnings,
        "bytes": total,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True, help="镜像基址，例如 http://192.168.1.8:8090")
    ap.add_argument("--formula", required=True)
    ap.add_argument("--tag", action="append", default=[],
                    help="目标机器的瓶 tag，可重复（顺序即优先级）。默认 arm64_sequoia")
    a = ap.parse_args()
    out = resolve(a.base.rstrip("/"), a.formula, a.tag or ["arm64_sequoia"])
    json.dump(out, sys.stdout, ensure_ascii=False, indent=2)
    sys.stdout.write("\n")
    return 1 if out["missing"] else 0


if __name__ == "__main__":
    sys.exit(main())
