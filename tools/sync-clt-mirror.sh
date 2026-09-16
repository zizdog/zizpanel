#!/usr/bin/env bash
# ============================================================================
#  sync-clt-mirror.sh —— 把苹果原始 Command Line Tools 安装包同步到 NAS 镜像
#
#  为什么需要这个工具：镜像上原本只有 clt/index.json，而 clt/<产品号>/ 是**空的**。
#  面板首次在某台机器上装 CLT 时，每个分片都会走 NAS nginx 的 @pull 现从
#  zizdog.com 拉（实测单连接 ~500 KB/s，631 MiB ≈ 23 分钟，而且是单点：
#  zizdog.com 一挂，首次安装就只能回落苹果 CDN，实测 15 分钟只下 1 MB）。
#  本工具把载荷**预先**放到 NAS 本地，面板之后就从同城/局域网取。
#
#  布局（与 internal/services/homebrew_clt_mirror.go 的约定一致）：
#      <clt-root>/index.json                         清单（本工具生成，sha256 真实计算）
#      <clt-root>/<产品号>/<pkg>.pkg.part-NNN        苹果原始包切成 32 MB 的分片
#  对外 URL：<mirror-base>/zizpanel/clt/<产品号>/<文件名>
#
#  用法：
#      bash tools/sync-clt-mirror.sh --dry-run              # 只打印计划，不下载不上传
#      NAS_PASS='...' bash tools/sync-clt-mirror.sh          # 真同步
#      NAS_PASS='...' bash tools/sync-clt-mirror.sh --product 072-44426-A
#      bash tools/sync-clt-mirror.sh --local-dest DIR --source-dir DIR   # 沙箱自测
#
#  环境变量：
#      NAS_HOST / NAS_USER / NAS_PASS / NAS_CLT_ROOT
#      SOURCE_BASE   （默认 https://zizdog.com/zizpanel，CLT 载荷的现存来源）
#      SOURCE_DIR    （沙箱：把本地目录当源，完全不碰网络）
#      MIRROR_CLT_URL（验收基址，默认 http://192.168.1.8:8090/zizpanel/clt）
#      WORK_DIR      （本地缓存；默认 $TMPDIR/zizpanel-clt-sync）
#
#  ⚠️ 口令只从环境变量进、只交给 sshpass，绝不写进任何文件、也不打印。
#
#  ⚠️ 关于 ssh 吞 stdin（tools/sync-nas-apps.sh 2026-09-16 的真实事故）：
#     在 `while read ... done < 文件` 循环里调用 ssh，ssh 默认会把循环的 stdin
#     读走，于是**后面的行被静默吃掉、脚本却报告成功**。本脚本两道防线：
#       1) nas_ssh 永远是 `ssh -n ... </dev/null`；
#       2) 下载循环里**根本不做 ssh**（远端快照在循环前一次取完，上传在循环后
#          一次 rsync）。即便以后有人在循环里加了 ssh，第 1 条也能兜住。
#
#  ---- 怎么新增一个系统版本（例如 macOS 26 / Tahoe）的条目 ----------------
#  现状（2026-09-16）：清单里只有 max_os=15 的一条，macOS 26 的机器没有可用条目。
#  补条目的步骤（本工具能做后半段，前半段必须在别的网络里做）：
#    1. 取苹果的 software update 目录（swscan 的 sucatalog），搜 "CLTools"
#       找到该版本的**产品号**（形如 072-44426-A）与 swdist 直链。
#       实测：本机连不上 swscan；mini 也连不上；NAS 能连到 ksyun 边缘，
#       但对 /content/catalogs/... 一律 404（swdist 的 pkg 反而 200）。
#       所以这一步不要在这两台机器上耗时间。
#    2. 把苹果**原始** pkg 放到源侧的 clt/<产品号>/ 下（或先切好 32MB 分片），
#       并在源侧 index.json 里加一条 item：pkgs 里至少要有
#       CLTools_Executables.pkg 与 CLTools_macOSNMOS_SDK.pkg，
#       max_os 填该版本对应的 macOS 主版本（macOS 26 → 26）。
#       **绝对不要手写 sha256**：留空即可，本工具会从目标侧的真实字节算出来。
#    3. NAS_PASS='...' bash tools/sync-clt-mirror.sh --product <产品号>
#    4. 清单**顺序即优先级**：老系统的条目留在前面、新系统的条目放后面 ——
#       pickCLTItem 的语义是"macOS 主版本 ≤ max_os"，于是 macOS 15 的机器命中
#       max_os=15 那条，macOS 26 的机器跳过它、命中新加的那条。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAS_HOST="${NAS_HOST:-192.168.1.8}"
NAS_USER="${NAS_USER:-zizdog}"
NAS_PASS="${NAS_PASS:-}"
NAS_CLT_ROOT="${NAS_CLT_ROOT:-/vol2/zizpanel-mirror/zizpanel/clt}"

SOURCE_BASE="${SOURCE_BASE:-https://zizdog.com/zizpanel}"
SOURCE_DIR="${SOURCE_DIR:-}"
MIRROR_CLT_URL="${MIRROR_CLT_URL:-http://192.168.1.8:8090/zizpanel/clt}"
WORK_DIR="${WORK_DIR:-${TMPDIR:-/tmp}/zizpanel-clt-sync}"

PRODUCT=""
DRY_RUN=0
CLEAN=0
ALL_PKGS=0
LOCAL_DEST=""

# 面板真正会下载的组件（与 internal/services/homebrew_clt_mirror.go 的
# cltInstallPkgs 一致）。苹果同一产品里还有 SwiftBackDeploy / LMOS_SDK /
# DevSDK_Remove_*，都下要多 105 MB 而面板根本不装 —— 镜像它们纯属浪费。
WHITELIST="CLTools_Executables.pkg,CLTools_macOSNMOS_SDK.pkg"

usage() { sed -n '2,40p' "$0"; }
die() { printf '✗ %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)     DRY_RUN=1 ;;
    --product)     PRODUCT="${2:-}"; shift ;;
    --source-base) SOURCE_BASE="${2:-}"; shift ;;
    --source-dir)  SOURCE_DIR="${2:-}"; shift ;;
    --dest)        NAS_CLT_ROOT="${2:-}"; shift ;;
    --local-dest)  LOCAL_DEST="${2:-}"; shift ;;
    --mirror-url)  MIRROR_CLT_URL="${2:-}"; shift ;;
    --work)        WORK_DIR="${2:-}"; shift ;;
    --all-pkgs)    ALL_PKGS=1 ;;
    --clean)       CLEAN=1 ;;
    -h|--help)     usage; exit 0 ;;
    *) die "未知参数：$1（用 --help 看用法）" ;;
  esac
  shift
done

SOURCE_BASE="${SOURCE_BASE%/}"
MIRROR_CLT_URL="${MIRROR_CLT_URL%/}"
[ -n "$LOCAL_DEST" ] && LOCAL_DEST="${LOCAL_DEST%/}"

command -v python3 >/dev/null 2>&1 || die "需要 python3（用于清单解析/生成）"

if [ "$CLEAN" = "1" ]; then
  echo "==> 清理本地缓存 $WORK_DIR"
  rm -rf "$WORK_DIR"
fi
mkdir -p "$WORK_DIR/dl"
PLAN="$WORK_DIR/plan.tsv"; ITEMS="$WORK_DIR/items.json"
KNOWN="$WORK_DIR/known.tsv"
HASHES="$WORK_DIR/hashes.tsv"; OVERALL="$WORK_DIR/overall.tsv"

# --- 传输：两条路，沙箱用本地目录，真机用 ssh（永远 -n + </dev/null） -------
nas_ssh() { # nas_ssh '<远端 shell 命令>'
  [ -n "$NAS_PASS" ] || die "需要 NAS 口令：NAS_PASS='...' bash tools/sync-clt-mirror.sh"
  sshpass -p "$NAS_PASS" ssh -n -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 \
    -o PreferredAuthentications=password -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1 \
    "$NAS_USER@$NAS_HOST" "$1" </dev/null
}
sha_local() { /usr/bin/shasum -a 256 "$1" | awk '{print $1}'; }

# 取回源头清单：沙箱读本地文件，真机 curl。
echo "==> 读取源清单"
if [ -n "$SOURCE_DIR" ]; then
  SRC_INDEX="$SOURCE_DIR/index.json"
  [ -f "$SRC_INDEX" ] || die "沙箱源里没有 index.json：$SRC_INDEX"
else
  SRC_INDEX="$WORK_DIR/source-index.json"
  curl -fsSL --retry 3 --connect-timeout 20 -o "$SRC_INDEX" "$SOURCE_BASE/clt/index.json" \
    || die "取源清单失败：$SOURCE_BASE/clt/index.json（NAS 可达但源站不可达时，用 --source-dir 指向本地副本）"
fi

# --- 解析计划（纯函数，python 做 JSON） -------------------------------------
# plan.tsv 列：itemkey 产品目录 包名 相对文件名 相对源路径 字节 清单里声明的分片 sha
python3 - "$SRC_INDEX" "$PLAN" "$ITEMS" "$PRODUCT" "$WHITELIST" "$ALL_PKGS" <<'PY'
import json, sys
src, plan_out, items_out, only, whitelist, all_pkgs = sys.argv[1:7]
wl = [p for p in whitelist.split(',') if p]
idx = json.load(open(src, encoding='utf-8'))
sel, plan = [], []
for i, it in enumerate(idx.get('items') or []):
    p = (it.get('path') or '').strip('/')
    d = (it.get('dir') or '').strip('/')
    proddir = p if p else ('clt/' + d if d else '')
    if not proddir:
        sys.stderr.write('跳过条目 %r：path 与 dir 都是空的\n' % it.get('name'))
        continue
    prodname = proddir[4:] if proddir.startswith('clt/') else proddir
    prodname = prodname.strip('/')
    if only and prodname != only:
        continue
    pkgs = it.get('pkgs') or []
    want = pkgs if all_pkgs else [x for x in wl if x in pkgs]
    if not want:
        sys.stderr.write('条目 %r 的 pkgs 里没有白名单内的包（%s）\n' % (it.get('name'), ','.join(pkgs)))
        sys.exit(4)
    parts_by_pkg = {}
    for pp in (it.get('parts') or []):
        parts_by_pkg[pp.get('pkg')] = pp.get('parts') or []
    for pkg in want:
        parts = parts_by_pkg.get(pkg) or []
        if parts:
            for pt in parts:
                plan.append((str(i), prodname, pkg, pt['file'],
                             proddir + '/' + pt['file'], int(pt['size']),
                             (pt.get('sha256') or '').lower()))
        else:
            # 整包布局（清单没给 parts）：字节数清单里没按包给，先记 0（未知），
            # 下载后用真实大小回填。面板支持这种布局，工具也得支持。
            plan.append((str(i), prodname, pkg, pkg, proddir + '/' + pkg, 0, ''))
    rec = dict(it); rec['_key'] = str(i)
    sel.append(rec)
if not sel:
    sys.stderr.write('源清单里没有匹配 product=%r 的条目\n' % only)
    sys.exit(3)
with open(plan_out, 'w', encoding='utf-8') as f:
    for row in plan:
        f.write('\t'.join([row[0], row[1], row[2], row[3], row[4], str(row[5]), row[6]]) + '\n')
json.dump({'note': idx.get('note', ''), 'items': sel},
          open(items_out, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
PY

N_PLAN="$(wc -l < "$PLAN" | tr -d ' ')"
N_PKG="$(cut -f3 "$PLAN" | sort -u | wc -l | tr -d ' ')"
TOTAL_WANT="$(awk -F'\t' '{s+=$6} END{print s+0}' "$PLAN")"
echo "    计划：$N_PKG 个包 / $N_PLAN 个文件 / $(printf '%s' "$TOTAL_WANT" | awk '{printf "%.1f MiB", $1/1048576}')（清单声明的字节数）"
cut -f2 "$PLAN" | sort -u | sed 's/^/    产品目录：/'

# --- 读已有清单里的“已知好 sha”，用于幂等 + 发现坏片 ------------------------
: > "$KNOWN"
if [ -n "$LOCAL_DEST" ]; then
  kn_src="$LOCAL_DEST/index.json"
elif command -v curl >/dev/null 2>&1; then
  kn_src="$WORK_DIR/dest-index.json"
  curl -fsSL --max-time 30 -o "$kn_src" "$MIRROR_CLT_URL/index.json" 2>/dev/null || : > "$kn_src"
else
  kn_src=""
fi
if [ -n "$kn_src" ] && [ -s "$kn_src" ]; then
  python3 - "$kn_src" "$KNOWN" <<'PY'
import json, sys
try:
    idx = json.load(open(sys.argv[1], encoding='utf-8'))
except Exception:
    raise SystemExit(0)
out = open(sys.argv[2], 'w', encoding='utf-8')
for it in idx.get('items') or []:
    p = (it.get('path') or '').strip('/')
    d = (it.get('dir') or '').strip('/')
    prod = (p[4:] if p.startswith('clt/') else p) if p else d
    prod = prod.strip('/')
    for pp in (it.get('parts') or []):
        for pt in pp.get('parts') or []:
            if pt.get('sha256'):
                out.write('%s\t%s\t%s\n' % (prod, pt['file'], pt['sha256'].lower()))
PY
  echo "    已读镜像上现有清单：$(wc -l < "$KNOWN" | tr -d ' ') 条分片 sha256 可作幂等基准"
fi

# --- 取目标侧快照（一次 ssh / 一次本地 stat），循环里不再碰远端 -------------
snapshot_dest() { # <目录> → 打印 "<name>|<size>"、---、"<sha>  <name>"
  local dir="$1"
  if [ -n "$LOCAL_DEST" ]; then
    [ -d "$dir" ] || return 0
    ( cd "$dir" && { /usr/bin/stat -f '%N|%z' * 2>/dev/null || true; echo '---'; /usr/bin/shasum -a 256 * 2>/dev/null || true; } )
  else
    # 结尾必须 exit 0：目录为空时 `stat *` / `sha256sum *` 会返回非 0，
    # ssh 会把这个退出码带回来，`set -e` 就会把整个脚本静默干掉
    # （空目录正是**第一次同步**的必然状态，实测踩到过）。
    nas_ssh "cd '$dir' 2>/dev/null || exit 0; stat -c '%n|%s' * 2>/dev/null; echo ---; sha256sum * 2>/dev/null; exit 0"
  fi
}
parse_snapshot() { # <原文文件> <输出 tsv>
  python3 - "$1" "$2" <<'PY'
import sys
sizes, shas, mode = {}, {}, 's'
for line in open(sys.argv[1], encoding='utf-8', errors='replace').read().split('\n'):
    if line.strip() == '---':
        mode = 'h'; continue
    if not line.strip():
        continue
    if mode == 's':
        n, _, s = line.rpartition('|')
        if n and s.isdigit():
            sizes[n] = int(s)
    else:
        p = line.split(None, 1)
        if len(p) == 2:
            shas[p[1].strip()] = p[0]
with open(sys.argv[2], 'w', encoding='utf-8') as f:
    for n, s in sizes.items():
        f.write('%s\t%d\t%s\n' % (n, s, shas.get(n, '')))
PY
}

# --- 逐文件：目标侧已好就跳过，否则下载到本地缓存 ---------------------------
MISSING_REPORT="$WORK_DIR/missing.txt"; : > "$MISSING_REPORT"
downloaded=0; skipped=0; bytes_down=0
echo
echo "==> 同步载荷到 ${LOCAL_DEST:-$NAS_USER@$NAS_HOST:$NAS_CLT_ROOT}"

# 每个产品目录的快照只取一次，而且**在循环之前**取完：这样下载循环里一次 ssh
# 都没有，彻底避开"ssh 吞掉 while-read 的 stdin"那条坑（见文件头）。
# 不用"循环内惰性取 + 记号文件"：记号文件会跨多次运行残留，第二次运行会拿着
# 第一次的空快照，把已经同步好的文件全部重下一遍（实测踩到过）。
snap_get() { # <产品目录名> <相对文件名> <字段 2=size 3=sha>
  awk -F'\t' -v k="$2" -v f="$3" '$1==k{print $f; exit}' "$WORK_DIR/snap-$1.tsv"
}
for _p in $(cut -f2 "$PLAN" | sort -u); do
  snapshot_dest "${LOCAL_DEST:-$NAS_CLT_ROOT}/$_p" > "$WORK_DIR/snap-$_p.raw"
  parse_snapshot "$WORK_DIR/snap-$_p.raw" "$WORK_DIR/snap-$_p.tsv"
done

while IFS=$'\t' read -r itemkey prodname pkg relfile srcrel size dsha; do
  [ -n "$relfile" ] || continue
  dl="$WORK_DIR/dl/$prodname/$relfile"
  mkdir -p "$(dirname "$dl")"

  s_size="$(snap_get "$prodname" "$relfile" 2)"
  s_sha="$(snap_get "$prodname" "$relfile" 3)"
  k_sha="$(awk -F'\t' -v p="$prodname" -v k="$relfile" '$1==p && $2==k{print $3; exit}' "$KNOWN")"

  good=0
  if [ -n "$s_size" ] && { [ "$size" = "0" ] || [ "$s_size" = "$size" ]; }; then
    if [ -n "$k_sha" ]; then
      [ "$s_sha" = "$k_sha" ] && good=1
    else
      good=1   # 清单没给分片 sha256（旧清单）：只按大小判定
    fi
  fi
  if [ "$good" = "1" ]; then
    printf '%s\t%s\t%s\t%s\n' "$itemkey" "$relfile" "$s_size" "$s_sha" >> "$HASHES"
    skipped=$((skipped+1))
    continue
  fi

  if [ "$DRY_RUN" = "1" ]; then
    echo "  [dry-run] 需要下载：$prodname/${relfile}（清单声明 $size B）"
    printf '%s\t%s\t%s\n' "$prodname" "$relfile" "$size" >> "$MISSING_REPORT"
    continue
  fi

  # 断点续传：本地缓存比目标大 → 丢掉重来；否则 -C - 接着下。
  if [ -f "$dl" ]; then
    cur="$(wc -c < "$dl" | tr -d ' ')"
    if [ "$size" != "0" ] && [ "$cur" -gt "$size" ]; then rm -f "$dl"; fi
  fi
  if [ -n "$SOURCE_DIR" ]; then
    cp -f "$SOURCE_DIR/$srcrel" "$dl" \
      || { echo "  ✗ 源目录里没有这个文件：$SOURCE_DIR/$srcrel" >&2; printf '%s\t%s\t%s\n' "$prodname" "$relfile" "$size" >> "$MISSING_REPORT"; continue; }
  else
    curl -fL --http1.1 --retry 5 --retry-delay 3 --retry-all-errors \
      --connect-timeout 20 --max-time 3600 --speed-limit 1024 --speed-time 60 \
      -C - -o "$dl" "$SOURCE_BASE/$srcrel" \
      || { echo "  ✗ 下载失败：$SOURCE_BASE/$srcrel" >&2; printf '%s\t%s\t%s\n' "$prodname" "$relfile" "$size" >> "$MISSING_REPORT"; continue; }
  fi
  got="$(wc -c < "$dl" | tr -d ' ')"
  if [ "$size" != "0" ] && [ "$got" != "$size" ]; then
    echo "  ✗ 大小不符：$prodname/$relfile 得到 ${got}，清单声明 $size" >&2
    mv "$dl" "$dl.bad" 2>/dev/null || true
    printf '%s\t%s\t%s\n' "$prodname" "$relfile" "$size" >> "$MISSING_REPORT"
    continue
  fi
  gsha="$(sha_local "$dl")"
  if [ -n "$dsha" ] && [ "$gsha" != "$dsha" ]; then
    echo "  ✗ 源声明 sha256 不符：$prodname/${relfile}（期望 ${dsha}，实际 ${gsha}）" >&2
    rm -f "$dl"
    printf '%s\t%s\t%s\n' "$prodname" "$relfile" "$size" >> "$MISSING_REPORT"
    continue
  fi
  printf '%s\t%s\t%s\t%s\n' "$itemkey" "$relfile" "$got" "$gsha" >> "$HASHES"
  bytes_down=$((bytes_down + got))
  downloaded=$((downloaded+1))
  printf '  ↓ %s/%s（%s MiB）\n' "$prodname" "$relfile" "$(awk -v n="$got" 'BEGIN{printf "%.1f", n/1048576}')"
done < "$PLAN"

echo "    下载 $downloaded 个文件（$(awk -v n="$bytes_down" 'BEGIN{printf "%.1f", n/1048576}') MiB），复用已有 $skipped 个"

if [ "$DRY_RUN" = "1" ]; then
  echo
  echo "==> dry-run 结束（未下载、未上传、未改动 NAS）"
  exit 0
fi

if [ -s "$MISSING_REPORT" ]; then
  echo
  echo "✗ 有文件没拿到，**不上传半成品清单**。缺这些：" >&2
  sed 's/^/    /' "$MISSING_REPORT" >&2
  exit 1
fi

# --- 上传载荷（一次 rsync / 一次本地拷贝） ----------------------------------
echo "==> 上传到 ${LOCAL_DEST:-$NAS_USER@$NAS_HOST:$NAS_CLT_ROOT}"
if [ -n "$LOCAL_DEST" ]; then
  while IFS= read -r d; do
    mkdir -p "$LOCAL_DEST/$d"
    cp -f "$WORK_DIR/dl/$d/"* "$LOCAL_DEST/$d/" 2>/dev/null || true
  done < <(cut -f2 "$PLAN" | sort -u)
else
  while IFS= read -r d; do
    # mkdir/rsync 都不在 while-read 循环里读数据；即便如此也要 -n / </dev/null：
    # 这是 sync-nas-apps.sh 事故的防线（见文件头）。
    nas_ssh "mkdir -p '$NAS_CLT_ROOT/$d'"
    sshpass -p "$NAS_PASS" rsync -a --no-perms --no-owner --no-group \
      -e "ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15" \
      "$WORK_DIR/dl/$d/" "$NAS_USER@$NAS_HOST:$NAS_CLT_ROOT/$d/" </dev/null
    echo "    ↑ $d"
  done < <(cut -f2 "$PLAN" | sort -u)
fi

# --- 用目标侧**真实字节**重算 sha256，再和源声明的整体 sha256 对账 ----------
# 注意：这里的哈希不是"我们下载时算的"，是**目标侧文件的真实值**（本地 stat/shasum
# 或 NAS 上的 sha256sum）。镜像要拿它当验收标准，就必须是目标上的真实值。
echo "==> 在目标侧重算 sha256（真实字节）"
dest_overall_sha() { # <产品目录> <文件列表（空格分隔，按清单顺序）>
  local prod="$1" files="$2"
  if [ -n "$LOCAL_DEST" ]; then
    ( cd "$LOCAL_DEST/$prod" && cat $files | /usr/bin/shasum -a 256 | awk '{print $1}' )
  else
    nas_ssh "cd '$NAS_CLT_ROOT/$prod' && cat $files | sha256sum | cut -d' ' -f1"
  fi
}
: > "$OVERALL"
while IFS= read -r pkgkey; do
  itemkey="${pkgkey%%|*}"; pkg="${pkgkey##*|}"
  prodname="$(awk -F'\t' -v k="$itemkey" -v p="$pkg" '$1==k && $3==p{print $2; exit}' "$PLAN")"
  files="$(awk -F'\t' -v k="$itemkey" -v p="$pkg" '$1==k && $3==p{print $4}' "$PLAN" | tr '\n' ' ')"
  osha="$(dest_overall_sha "$prodname" "$files")"
  bytes="$(awk -F'\t' -v k="$itemkey" -v p="$pkg" '$1==k && $3==p{s+=$6} END{print s+0}' "$PLAN")"
  printf '%s\t%s\t%s\t%s\n' "$itemkey" "$pkg" "$bytes" "$osha" >> "$OVERALL"
  # 源清单声明了整体 sha256 就必须对上；对不上说明镜像上的字节和源不一致，
  # 这时绝不能写清单（写了就是"镜像永远不命中、静默回落慢源"的老事故）。
  want="$(python3 - "$SRC_INDEX" "$pkg" <<'PY'
import json, sys
for it in json.load(open(sys.argv[1], encoding='utf-8')).get('items') or []:
    for s in it.get('sha256') or []:
        if '=' in s and s.split('=', 1)[0].strip() == sys.argv[2]:
            print(s.split('=', 1)[1].strip().lower())
PY
)"
  if [ -n "$want" ] && [ "$want" != "$osha" ]; then
    echo "✗ $prodname/$pkg 在目标侧的整体 sha256 与源声明不一致：" >&2
    echo "    源声明：$want" >&2
    echo "    目标实测：$osha" >&2
    echo "  已上传的字节是坏的，请检查传输；**不写清单**（写了会让面板永远命中不到）。" >&2
    exit 1
  fi
  echo "    ✓ $prodname/$pkg $(awk -v n="$bytes" 'BEGIN{printf "%.1f MiB", n/1048576}') sha256=${osha:0:16}…"
done < <(awk -F'\t' '{print $1"|"$3}' "$PLAN" | sort -u)

# --- 生成清单（所有 sha256 都是上面实测值） ---------------------------------
python3 - "$ITEMS" "$PLAN" "$HASHES" "$OVERALL" "$WORK_DIR/index.json" "${SOURCE_DIR:-$SOURCE_BASE}" <<'PY'
import json, sys, time, collections
items_f, plan_f, hashes_f, overall_f, out_f, src_base = sys.argv[1:7]
items = json.load(open(items_f, encoding='utf-8'))['items']
hashes, overall = {}, {}
for line in open(hashes_f, encoding='utf-8'):
    k, rel, size, sha = line.rstrip('\n').split('\t')
    hashes[(k, rel)] = (int(size), sha)
for line in open(overall_f, encoding='utf-8'):
    k, pkg, size, sha = line.rstrip('\n').split('\t')
    overall[(k, pkg)] = (int(size), sha)
pkgs_by_item = collections.OrderedDict()
parts_by_item = collections.OrderedDict()
for line in open(plan_f, encoding='utf-8'):
    k, prod, pkg, rel, srcrel, size, dsha = line.rstrip('\n').split('\t')
    pkgs_by_item.setdefault(k, [])
    if pkg not in pkgs_by_item[k]:
        pkgs_by_item[k].append(pkg)
    parts_by_item.setdefault((k, pkg), [])
    if rel != pkg:
        parts_by_item[(k, pkg)].append(rel)
out = []
for it in items:
    k = it.pop('_key')
    new = dict(it)
    new['pkgs'] = pkgs_by_item.get(k, [])
    total, shas, parts = 0, [], []
    for pkg in new['pkgs']:
        sz, sh = overall[(k, pkg)]
        total += sz
        shas.append('%s=%s' % (pkg, sh))
        rels = parts_by_item.get((k, pkg)) or []
        if rels:
            parts.append({'pkg': pkg,
                          'parts': [{'file': r, 'size': hashes[(k, r)][0], 'sha256': hashes[(k, r)][1]}
                                    for r in rels]})
    new['bytes'] = total
    new['sha256'] = shas
    if parts:
        new['parts'] = parts
    else:
        new.pop('parts', None)
    out.append(new)
note = ('苹果原始 Command Line Tools 安装包（未改动），仅供 ZizPanel 在国内网络下整包安装。'
        '每项按 max_os 上限匹配，排在前面的优先。 '
        '本清单由 tools/sync-clt-mirror.sh 生成：bytes / sha256 / parts[].sha256 '
        '都是镜像上实际字节的真实值（源：%s）。' % src_base)
json.dump({'updated': time.strftime('%Y-%m-%d'), 'note': note, 'items': out},
          open(out_f, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
PY

# 清单也要"原地替换"式上传：先传临时名，再在同目录 mv 覆盖（避免读到半截 JSON）。
TMPIDX="$WORK_DIR/index.json.upload"
cp "$WORK_DIR/index.json" "$TMPIDX"
if [ -n "$LOCAL_DEST" ]; then
  mkdir -p "$LOCAL_DEST"
  cp -f "$TMPIDX" "$LOCAL_DEST/index.json"
else
  sshpass -p "$NAS_PASS" rsync -a --no-perms --no-owner --no-group \
    -e "ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15" \
    "$TMPIDX" "$NAS_USER@$NAS_HOST:$NAS_CLT_ROOT/index.json.new" </dev/null
  nas_ssh "mv -f '$NAS_CLT_ROOT/index.json.new' '$NAS_CLT_ROOT/index.json'"
fi
echo "    ✓ index.json 已更新（$(wc -c < "$WORK_DIR/index.json" | tr -d ' ') B）"

# --- 验收：看真实状态，不看退出码 -------------------------------------------
echo "==> 验收（目标侧真实大小 + 对外 HTTP）"
NOTOK=0
while IFS=$'\t' read -r itemkey prodname pkg relfile srcrel size dsha; do
  [ -n "$relfile" ] || continue
  want="${LOCAL_DEST:-$NAS_CLT_ROOT}/$prodname/$relfile"
  if [ -n "$LOCAL_DEST" ]; then
    if [ -f "$want" ]; then got="$(wc -c < "$want" | tr -d ' ')"; else got="缺失"; fi
  else
    got="$(nas_ssh "stat -c%s '$want' 2>/dev/null || echo 缺失")"
  fi
  if { [ "$size" = "0" ] && [ "$got" != "缺失" ]; } || [ "$got" = "$size" ]; then
    :
  else
    echo "    ✗ $prodname/$relfile 目标侧大小 $got ≠ 期望 $size" >&2
    NOTOK=$((NOTOK+1))
  fi
done < "$PLAN"

# 对外 HTTP：面板真正请求的是这个地址，200 + 字节数对得上才算数。
# 沙箱（--local-dest）没有 HTTP 入口，跳过这一段。
if [ -n "$LOCAL_DEST" ]; then
  echo "    （沙箱模式：跳过对外 HTTP 验收）"
elif [ "$NOTOK" = "0" ]; then
  code="$(curl -s -o /dev/null -m 15 -w '%{http_code}' "$MIRROR_CLT_URL/index.json" 2>/dev/null || echo 000)"
  if [ "$code" = "200" ]; then
    echo "    ✓ 对外清单 $MIRROR_CLT_URL/index.json → HTTP 200"
  else
    echo "    ✗ 对外清单 $MIRROR_CLT_URL/index.json → HTTP $code" >&2
    NOTOK=$((NOTOK+1))
  fi
  # 抽查整份计划的头一个文件和最后一个文件：头一个证明"开始就能取"，
  # 最后一个证明"整包完整"（分片是按顺序排的，末片就是包的尾巴）。
  for probe in "$(head -1 "$PLAN")" "$(tail -1 "$PLAN")"; do
    [ -n "$probe" ] || continue
    pn="$(printf '%s' "$probe" | cut -f2)"; fl="$(printf '%s' "$probe" | cut -f4)"
    pc="$(curl -s -o /dev/null -m 20 -w '%{http_code} %{size_download}' -r 0-1023 "$MIRROR_CLT_URL/$pn/$fl" 2>/dev/null || echo '000 0')"
    echo "    抽查 $pn/$fl → HTTP ${pc%% *}（读 ${pc##* } B）"
  done
fi

echo
if [ "$NOTOK" != "0" ]; then
  echo "✗ 验收未通过：$NOTOK 项不对，请修好后重跑（重跑只会补缺失的部分）" >&2
  exit 1
fi
echo "完成：$(cut -f2 "$PLAN" | sort -u | tr '\n' ' ') 已在镜像上，清单 sha256 为实测值。"
