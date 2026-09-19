#!/usr/bin/env bash
# 把 Homebrew 瓶**真的预置到 NAS 镜像**里（用户 2026-09-18 要求："同步给 nas 一份镜像"）。
#
# 为什么需要它：NAS 的 `/brew/` 是**按需缓存** —— 第一次有人请求才回源抓取。
# 于是"装机那一刻上游镜像手上有没有这个瓶"直接决定成败：python@3.11 3.11.16
# 就是这么卡住的（面板在用户机器上装的那一刻，镜像侧还没有/取不到）。
# 本脚本主动把指定 formula（默认连同它的**运行时依赖闭包**）在指定 bottle tag 上的
# 瓶各取一遍，之后任何机器安装都命中 NAS，不再看上游当时的脸色。
#
# 判据不是"命令没报错"（这个项目最贵的教训），每个文件必须同时满足：
#   ① 下载完成后实算 sha256 == 镜像 API 声明的 digest；
#   ② 再请求一次得到 `X-Cache: HIT`（证明 NAS 上真的存下来了，而不是每次回源）。
# 任一条不过就计失败，脚本退出码非 0。
#
# 用法：
#   MIRROR='https://<你的镜像机>' bash tools/seed-nas-brew.sh python@3.11   # 含依赖闭包；tag 由本机系统推导
#   MIRROR='https://…' bash tools/seed-nas-brew.sh python@3.12 --no-deps
#   MIRROR='https://…' bash tools/seed-nas-brew.sh python@3.11 --tags arm64_sequoia,sonoma
#   MIRROR='https://…' bash tools/seed-nas-brew.sh python@3.11 --dry-run    # 只列出要预置什么，不下载
#   MIRROR='https://…' bash tools/seed-nas-brew.sh python@3.11 --also-oci   # 连 OCI 路径也各预热一份
#
# 预热的是**哪条 URL**（很重要，2026-09-18 读 Homebrew 7 源码后才确认）：
# 默认预热 **legacy 平铺** `<%40编码的 formula>-<版本>.<tag>.bottle[.<rebuild>].tar.gz`
# —— 面板设了自定义 HOMEBREW_BOTTLE_DOMAIN 时，brew 真正请求的就是这条；
# `--also-oci` 额外把 `/v2/homebrew/core/<name>/<ver>/blobs/sha256:<digest>` 也预热
# （老 brew 或直接按 OCI 取的服务会用到）。缓存按**路径**存，两条路各存一份。
#
# 备注：
#   · 地址由调用者提供，仓库里不留任何内网默认值：MIRROR=<你自己的镜像基址>。
#   · 只读本地 brew（`brew deps`）；不装、不删、不改本机任何东西。
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

MIRROR="${MIRROR:-}"
: "${MIRROR:?请传 MIRROR=<你自己的镜像基址，如 https://mirror.example.com>（本机可覆盖）}"
BREW_BIN="${BREW_BIN:-/opt/homebrew/bin/brew}"
TMPDIR_SEED="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_SEED"' EXIT

formulas=()
tags=""
with_deps=1
dry_run=0
also_oci=0

while [ $# -gt 0 ]; do
  case "$1" in
    --tags) tags="${2:-}"; shift 2 ;;
    --no-deps) with_deps=0; shift ;;
    --dry-run) dry_run=1; shift ;;
    --also-oci) also_oci=1; shift ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    -*) echo "未知参数：$1" >&2; exit 1 ;;
    *) formulas+=("$1"); shift ;;
  esac
done

[ ${#formulas[@]} -gt 0 ] || { echo "用法：bash tools/seed-nas-brew.sh <formula> [--no-deps] [--tags a,b] [--dry-run]" >&2; exit 1; }

# default_tags 按本机 macOS 版本 + 架构推导 brew 的 bottle tag。
# 为什么按本机推导而不是写死：面板的用户机器就是 Apple Silicon + 当前系统，
# 预置别的 tag 对这台机器没用（而 amd64 的瓶可以另外用 --tags 显式指定）。
default_tags() {
  local product major name arch
  product="$(sw_vers -productVersion 2>/dev/null || echo 15.0)"
  major="${product%%.*}"
  case "$major" in
    26) name="tahoe" ;;
    15) name="sequoia" ;;
    14) name="sonoma" ;;
    13) name="ventura" ;;
    12) name="monterey" ;;
    11) name="big_sur" ;;
    *) name="" ;;
  esac
  arch="$(uname -m)"
  if [ -n "$name" ]; then
    if [ "$arch" = "arm64" ]; then echo "arm64_$name"; else echo "$name"; fi
  fi
}

[ -n "$tags" ] || tags="$(default_tags)"
[ -n "$tags" ] || { echo "!! 无法从本机推导 bottle tag（macOS $(sw_vers -productVersion) / $(uname -m)），请用 --tags 指定" >&2; exit 1; }

# expand_deps 把 formula 展开成"自己 + 运行时依赖闭包"（去重，保持顺序）。
expand_deps() {
  local root="$1" dep
  echo "$root"
  [ "$with_deps" -eq 1 ] || return 0
  command -v "$BREW_BIN" >/dev/null 2>&1 || { echo "（本机没有 brew，跳过依赖展开）" >&2; return 0; }
  # brew 的 deps 默认就给**递归**依赖（新版本已经没有 --recurse 这个开关，
  # 传了会直接打印用法并返回 0 —— 于是"依赖闭包"静默变成空，本机实测踩到）。
  # 所以：先按新写法跑，输出为空再试旧写法（--recurse）。
  local out
  out="$("$BREW_BIN" deps "$root" 2>/dev/null)"
  if [ -z "$out" ]; then
    out="$("$BREW_BIN" deps --recurse "$root" 2>/dev/null | grep -v '^Usage:' | grep -v '^$')"
  fi
  while IFS= read -r dep; do
    [ -n "$dep" ] && echo "$dep"
  done <<<"$out"
}

# bottle_targets_for <formula> <tag> → 每个候选 URL 一行："<url>\t<sha256>\t<tag>\t<布局>"
#
# 列表的来源是**镜像自己的 API 清单**（`<mirror>/brew/api/formula/<f>.json`）：
# 面板安装时读的就是这份清单，它才是最权威的复刻对象。清单里有三样要用：
#   versions.stable、bottle.stable.rebuild、bottle.stable.files[tag].sha256
#
# 为什么默认只预热平铺那条：brew 7 对**非 ghcr** 的 HOMEBREW_BOTTLE_DOMAIN 走旧式
# 平铺 URL（见 internal/services/install.go 的 brewBottleFilename），那才是 brew
# 真正请求的路径；OCI 路径用 --also-oci 额外预热。
bottle_targets_for() {
  local formula="$1" tag="$2" enc json
  enc="$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$formula")"
  json="$TMPDIR_SEED/api.json"
  if ! curl -fsS --max-time 40 "$MIRROR/brew/api/formula/$enc.json" -o "$json"; then
    return 1
  fi
  python3 - "$json" "$tag" "$MIRROR" "$also_oci" "$formula" <<'PYINNER'
import json, sys, urllib.parse
path, tag, mirror, also_oci, formula = (
    sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1", sys.argv[5])
try:
    d = json.load(open(path))
except Exception:
    print("BAD-JSON"); raise SystemExit(0)
stable = d.get("versions", {}).get("stable") or ""
bst = d.get("bottle", {}).get("stable", {}) or {}
files = bst.get("files", {}) or {}
rebuild = int(bst.get("rebuild") or 0)
effective = tag
f = files.get(tag)
# 有些 formula（ca-certificates 等）只有 **all** 瓶：与架构/系统无关的包，清单里
# 没有 arm64_* 那个 tag。不认这种情况会把"其实拿得到"的依赖误报成缺口（本机实测撞上）。
if not f and "all" in files:
    f = files["all"]
    effective = "all"
if not f:
    print("MISSING-TAG\t" + ",".join(sorted(files)))
    raise SystemExit(0)
if not stable:
    print("MISSING-VERSION"); raise SystemExit(0)
base = mirror.rstrip("/") + "/brew"
name_enc = urllib.parse.quote(formula, safe="")          # python@3.11 → python%403.11
suffix = ".bottle.%d.tar.gz" % rebuild if rebuild > 0 else ".bottle.tar.gz"
flat = "%s/%s-%s.%s%s" % (base, name_enc, stable, effective, suffix)
sha = f["sha256"]
print("%s\t%s\t%s\tflat" % (flat, sha, effective))
if also_oci:
    oci = "%s/v2/homebrew/core/%s/blobs/sha256:%s" % (base, formula.replace("@", "/"), sha)
    print("%s\t%s\t%s\toci" % (oci, sha, effective))
PYINNER
}

ok_count=0
fail_count=0
total_bytes=0
skipped=0
seen=""

printf '==> 预置到 %s（tag: %s，依赖闭包: %s）\n' "$MIRROR" "$tags" "$([ "$with_deps" -eq 1 ] && echo 含 || echo 不含)"
for root in "${formulas[@]}"; do
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    case " $seen " in *" $f "*) continue ;; esac
    seen="$seen $f"
    for tag in ${tags//,/ }; do
      targets="$(bottle_targets_for "$f" "$tag")"
      if [ -z "$targets" ]; then
        printf '  ✗ %-28s %-16s 镜像清单里没有这个 tag 的瓶（未预置）\n' "$f" "$tag"
        fail_count=$((fail_count + 1))
        continue
      fi
      case "$targets" in
        MISSING-TAG*)
          printf '  ✗ %-28s %-16s 镜像 API 无该 tag：%s\n' "$f" "$tag" "${targets#MISSING-TAG}"
          fail_count=$((fail_count + 1))
          continue ;;
        MISSING-VERSION*|BAD-JSON*)
          printf '  ✗ %-28s %-16s 镜像 API 清单不完整（%s）\n' "$f" "$tag" "$targets"
          fail_count=$((fail_count + 1))
          continue ;;
      esac
      while IFS=$'\t' read -r url sha eff_tag layout; do
        [ -n "${url:-}" ] || continue
        if [ "$dry_run" -eq 1 ]; then
          printf '  · %-28s %-16s %-5s %s\n' "$f" "${eff_tag:-$tag}" "$layout" "${url#*"$MIRROR"}"
          skipped=$((skipped + 1))
          continue
        fi
        out="$TMPDIR_SEED/$(basename "${url%%\?*}" | tr ':' '_')"
        # 第一次：MISS 回源（NAS 从上游取回来并存下）
        first="$(curl -fsS --max-time 1800 -o "$out" -w '%{http_code} %header{x-cache} %{size_download}' "$url" 2>/dev/null)"
        rc=$?
        if [ $rc -ne 0 ]; then
          printf '  ✗ %-28s %-16s %-5s 下载失败（curl rc=%d）%s\n' "$f" "$tag" "$layout" "$rc" "$url"
          fail_count=$((fail_count + 1))
          continue
        fi
        got_sha="$(shasum -a 256 "$out" | cut -d' ' -f1)"
        if [ "$got_sha" != "$sha" ]; then
          printf '  ✗ %-28s %-16s %-5s sha256 不符：清单 %s / 实际 %s\n' "$f" "$tag" "$layout" "${sha:0:16}…" "${got_sha:0:16}…"
          fail_count=$((fail_count + 1))
          continue
        fi
        size="$(stat -f%z "$out" 2>/dev/null || wc -c <"$out")"
        # 第二次：必须是 HIT —— 否则"预置"是假的（每次仍在回源）
        second="$(curl -fsS --max-time 300 -o /dev/null -w '%header{x-cache}' "$url" 2>/dev/null)"
        total_bytes=$((total_bytes + size))
        if [ "$second" = "HIT" ]; then
          printf '  ✓ %-28s %-16s %-5s %10s B  首次[%s] 复核[HIT]\n' "$f" "${eff_tag:-$tag}" "$layout" "$size" "$first"
          ok_count=$((ok_count + 1))
        else
          printf '  ✗ %-28s %-16s %-5s sha256 对，但复核不是 HIT（X-Cache=%s）—— 没真正预置\n' "$f" "$tag" "$layout" "${second:-无}"
          fail_count=$((fail_count + 1))
        fi
      done <<<"$targets"
    done
  done < <(expand_deps "$root")
done

printf '\n==> 汇总：成功 %d，失败 %d，跳过 %d，合计 %.1f MiB\n' \
  "$ok_count" "$fail_count" "$skipped" "$(echo "$total_bytes" | awk '{print $1/1048576}')"
if [ "$dry_run" -eq 1 ]; then
  echo "（--dry-run：没有下载任何东西）"
  exit 0
fi
if [ "$fail_count" -gt 0 ]; then
  echo "!! 有 $fail_count 个瓶没有成功预置 —— 面板此刻装它仍可能撞上游不可用（如实报告，不当成功）"
  exit 1
fi
echo "全部预置完成 ✅（面板装这些包时会命中 NAS）"
