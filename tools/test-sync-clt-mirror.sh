#!/usr/bin/env bash
# ============================================================================
#  test-sync-clt-mirror.sh —— tools/sync-clt-mirror.sh 的**沙箱**测试
#
#  全程只用一个几十字节的假 CLT 包：
#    - 不走网络（源是本地目录，--source-dir）；
#    - 不碰真 NAS（目标是本地目录，--local-dest）；
#    - 不碰生产配置。
#
#  锁住的行为（都是真机上会要命的）：
#    1. 首次同步：4 个分片全部落地，生成的 index.json 里 sha256 是**实测值**
#       且与源声明的整体 sha256 一致；
#    2. 幂等：再跑一次不重复下载；
#    3. 增量修复：目标侧"大小对、内容坏"的分片会被发现并只重下这一片；
#    4. 源侧坏件：绝不写半成品清单，退出码非 0，并明确报告缺了什么。
#
#  用法：bash tools/test-sync-clt-mirror.sh
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOL="$REPO_ROOT/tools/sync-clt-mirror.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0; FAIL=0
ok()   { printf '  ✓ %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  ✗ %s\n' "$1" >&2; FAIL=$((FAIL+1)); }
want() { # want <说明> <期望子串> <文件>
  if grep -q -- "$2" "$3"; then ok "$1"; else bad "$1（输出里没有「$2」）"; fi
}

make_fixture() { # <目录> —— 造一个 2 包 / 4 分片的假 CLT
  python3 - "$1" <<'PY'
import hashlib, json, os, sys
root = sys.argv[1]
os.makedirs(os.path.join(root, "clt/FAKE-001"), exist_ok=True)
exe = [b"AAA", b"BBB"]          # 3 + 3 字节
sdk = [b"CCCC", b"D"]           # 4 + 1 字节
def put(rel, data):
    open(os.path.join(root, rel), "wb").write(data)
for i, p in enumerate(exe): put("clt/FAKE-001/CLTools_Executables.pkg.part-%03d" % i, p)
for i, p in enumerate(sdk): put("clt/FAKE-001/CLTools_macOSNMOS_SDK.pkg.part-%03d" % i, p)
def sha(b): return hashlib.sha256(b).hexdigest()
json.dump({
    "updated": "2026-01-01", "note": "fixture",
    "items": [{
        "name": "Fake CLT", "path": "clt/FAKE-001", "max_os": 15,
        "bytes": sum(map(len, exe)) + sum(map(len, sdk)),
        "pkgs": ["CLTools_Executables.pkg", "CLTools_macOSNMOS_SDK.pkg"],
        "sha256": ["CLTools_Executables.pkg=" + sha(b"".join(exe)),
                   "CLTools_macOSNMOS_SDK.pkg=" + sha(b"".join(sdk))],
        "parts": [
            {"pkg": "CLTools_Executables.pkg",
             "parts": [{"file": "CLTools_Executables.pkg.part-%03d" % i, "size": len(p)} for i, p in enumerate(exe)]},
            {"pkg": "CLTools_macOSNMOS_SDK.pkg",
             "parts": [{"file": "CLTools_macOSNMOS_SDK.pkg.part-%03d" % i, "size": len(p)} for i, p in enumerate(sdk)]},
        ],
    }],
}, open(os.path.join(root, "index.json"), "w"), indent=2)
PY
}

run_tool() { # run_tool <源目录> <目标目录> <工作目录> [额外参数...]
  local src="$1" dst="$2" work="$3"; shift 3
  bash "$TOOL" --source-dir "$src" --local-dest "$dst" --work "$work" "$@"
}

echo "== 沙箱测试 tools/sync-clt-mirror.sh（不联网、不碰 NAS）"

SRC="$TMP/src"; DST="$TMP/dst"; WORK="$TMP/work"
make_fixture "$SRC"

echo "--- 1) 首次同步"
run_tool "$SRC" "$DST" "$WORK" > "$TMP/run1.log" 2>&1 || { bad "首次同步应当成功"; cat "$TMP/run1.log" >&2; exit 1; }
want "首次同步下载了 4 个文件" "下载 4 个文件" "$TMP/run1.log"
[ "$(ls "$DST/FAKE-001" | wc -l | tr -d ' ')" = "4" ] && ok "目标侧有 4 个分片" || bad "目标侧分片数不对"
python3 - "$SRC/index.json" "$DST/index.json" "$DST/FAKE-001" <<'PY' && ok "index.json 的 sha256 是实测值且与源声明一致" || bad "index.json 的 sha256 不对"
import hashlib, json, os, sys
src, dst, pdir = sys.argv[1:4]
sid = json.load(open(src))["items"][0]
did = json.load(open(dst))["items"][0]
declared = dict(s.split("=", 1) for s in sid["sha256"])
for pkg in did["pkgs"]:
    files = next(p["parts"] for p in did["parts"] if p["pkg"] == pkg)
    blob = b"".join(open(os.path.join(pdir, f["file"]), "rb").read() for f in files)
    assert hashlib.sha256(blob).hexdigest() == declared[pkg], pkg
    for f in files:
        real = hashlib.sha256(open(os.path.join(pdir, f["file"]), "rb").read()).hexdigest()
        assert f["sha256"] == real, f["file"]
        assert f["size"] == os.path.getsize(os.path.join(pdir, f["file"])), f["file"]
PY

echo "--- 2) 幂等：再跑一次不应重复下载"
run_tool "$SRC" "$DST" "$WORK" > "$TMP/run2.log" 2>&1 || { bad "第二次运行应当成功"; cat "$TMP/run2.log" >&2; }
want "第二次没有下载任何文件" "下载 0 个文件" "$TMP/run2.log"
want "第二次复用了 4 个已有分片" "复用已有 4 个" "$TMP/run2.log"

echo "--- 3) 增量修复：大小对、内容坏的分片只重下这一片"
printf 'XXX' > "$DST/FAKE-001/CLTools_Executables.pkg.part-000"   # 等长坏内容
run_tool "$SRC" "$DST" "$WORK" > "$TMP/run3.log" 2>&1 || { bad "修复运行应当成功"; cat "$TMP/run3.log" >&2; }
want "只重下了 1 个文件" "下载 1 个文件" "$TMP/run3.log"
[ "$(cat "$DST/FAKE-001/CLTools_Executables.pkg.part-000")" = "AAA" ] \
  && ok "坏片内容被修好" || bad "坏片没有被修好"

echo "--- 4) 源侧坏件（内容对不上整体 sha256）：必须失败且不写清单"
cp -R "$SRC" "$TMP/src-bad"
printf 'ZZZ' > "$TMP/src-bad/clt/FAKE-001/CLTools_Executables.pkg.part-000"  # 等长、内容错
# 必须用一个**空目标**：已有好分片时工具会按"目标侧已验证"跳过下载，
# 那样源侧的坏件根本不会被取用（这本身是正确行为，见用例 2/3）。
DST4="$TMP/dst-bad"
set +e
run_tool "$TMP/src-bad" "$DST4" "$TMP/work-bad" > "$TMP/run4.log" 2>&1
rc=$?
set -e
[ "$rc" != "0" ] && ok "整体 sha256 不符时退出码非 0" || bad "整体 sha256 不符时不该报成功"
want "失败原因写清了是整体 sha256 不一致" "整体 sha256 与源声明不一致" "$TMP/run4.log"
[ ! -f "$DST4/index.json" ] && ok "失败时没有写出 index.json" || bad "失败时写出了 index.json（会污染镜像）"

echo "--- 5) 源侧缺文件：明确报告缺了什么（同样用空目标，确保真的去取源）"
cp -R "$SRC" "$TMP/src-missing"
rm -f "$TMP/src-missing/clt/FAKE-001/CLTools_macOSNMOS_SDK.pkg.part-001"
DST5="$TMP/dst-missing"
set +e
run_tool "$TMP/src-missing" "$DST5" "$TMP/work-missing" > "$TMP/run5.log" 2>&1
rc=$?
set -e
[ "$rc" != "0" ] && ok "缺文件时退出码非 0" || bad "缺文件时不该报成功"
want "报告里点名缺了哪个文件" "CLTools_macOSNMOS_SDK.pkg.part-001" "$TMP/run5.log"
want "明确说了不上传半成品清单" "不上传半成品清单" "$TMP/run5.log"

echo
echo "结果：通过 $PASS 项，失败 $FAIL 项"
[ "$FAIL" = "0" ] || exit 1
echo "沙箱测试全部通过（没有联网、没有碰 NAS）。"
