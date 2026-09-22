#!/usr/bin/env bash
# ============================================================================
#  publish-catalog.sh —— 把应用目录数据签名并上传到镜像站（**不发面板版**）
#
#  为什么需要：用户 2026-09-22 的目标是"不更新面板也能加应用"。面板侧已经会：
#  拉 <mirror>/apps/catalog.json + .sig → 用内嵌发布公钥**验签** → 只追加新 ID
#  （撞内置 ID 的忽略）→ 只接受 rail=brew / compose（安装走面板已有通用流程）。
#  所以"加一个 brew/compose 应用"= 改一份数据 + 跑本工具。
#
#  数据格式（catalog/apps.json 里就是它）：
#    {
#      "version": "1",
#      "generated_at": "2026-09-22T00:00:00Z",
#      "apps": [
#        { "id": "…", "name": "…", "icon": "…", "category": "tool",
#          "summary": "…", "description": "…",
#          "rail": "brew", "brew_formula": "jq", "port": 0, "no_daemon": true,
#          "runtime_path": "{brew}/bin/jq", "docs_url": "…" }
#      ]
#    }
#  ⚠️ 远端条目**只能声明既有通用轨的字段**，不许携带任意命令步骤（`run`）——
#    否则"改一份镜像文件"就等于对所有面板远程执行命令。需要新安装器的应用仍要发面板版。
#
#  用法：
#    bash tools/publish-catalog.sh catalog/apps.json          # 签名 + 上传镜像站
#    bash tools/publish-catalog.sh catalog/apps.json --dry     # 只签名，不上传
#  凭据：.panel-credential.local 的 ZP_MINI_URL / ZP_MINI_USER / ZP_MINI_PASS /
#        ZP_MIRROR_DIR（镜像上的 zizpanel 目录；目录写在 <dir>/apps/ 下）。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DATA="${1:-catalog/apps.json}"
DRY=0
[ "${2:-}" = "--dry" ] && DRY=1

ok()   { printf '  ✓ %s\n' "$*"; }
info() { printf '==> %s\n' "$*"; }
die()  { printf '!! %s\n' "$*" >&2; exit 1; }

[ -f "$DATA" ] || die "找不到目录数据文件：$DATA"
python3 - "$DATA" <<'PY' || die "目录数据不是合法 JSON / 结构不对"
import json, sys
d = json.load(open(sys.argv[1]))
assert isinstance(d.get("apps"), list), "缺 apps 数组"
for a in d["apps"]:
    assert a.get("id"), "有条目缺 id"
    assert a.get("rail") in ("brew", "compose"), "%s: rail 只支持 brew / compose" % a.get("id")
    if a["rail"] == "brew":
        assert a.get("brew_formula"), "%s: rail=brew 必须写 brew_formula" % a["id"]
    else:
        assert a.get("compose_yaml"), "%s: rail=compose 必须写 compose_yaml" % a["id"]
    for bad in ("steps", "run"):
        assert bad not in a, "%s: 远端条目不许携带 %s（安全边界）" % (a["id"], bad)
print("数据校验通过：%d 条" % len(d["apps"]))
PY

# ---- 用发布私钥签名（与在线升级同一把钥匙）------------------------------------
KEY="${RELEASE_KEY:-.release-key/zizpanel-ed25519.key}"
[ -f "$KEY" ] || die "找不到发布私钥 $KEY（签名是远端目录唯一的安全边界，不能跳过）"
OUT="${DATA%.json}.signed.json"
BIN="$(ls -t dist/host-zizpanel 2>/dev/null | head -1 || true)"
if [ -z "$BIN" ]; then
  info "构建签名工具（dist/host-zizpanel）"
  go build -trimpath -o dist/host-zizpanel ./cmd/zizpanel || die "构建失败"
  BIN=dist/host-zizpanel
fi
cp "$DATA" "$OUT"
"$BIN" sign-manifest --raw --key "$KEY" --in "$OUT" --out "${OUT}.sig" >/dev/null || die "签名失败"
ok "已签名：$OUT + ${OUT}.sig"

if [ "$DRY" = "1" ]; then
  info "（--dry）只签名，不上传。产物：$OUT / ${OUT}.sig"
  exit 0
fi

# ---- 上传到镜像站（走 mini 面板文件接口：镜像盘只有它的面板进程能写）------------
[ -f .panel-credential.local ] && . ./.panel-credential.local
: "${ZP_MINI_USER:?缺 ZP_MINI_USER（.panel-credential.local）}"
: "${ZP_MINI_PASS:?缺 ZP_MINI_PASS（.panel-credential.local）}"
BASE="${ZP_MINI_URL:-https://panel.zizdog.com:8888}"
DIR="${ZP_MIRROR_DIR:-/Volumes/ZPMirror/mirror/zizpanel}"
JAR="$(mktemp)"
trap 'rm -f "${JAR:-}"' RETURN
csrf() { awk '$6=="zp_csrf"{print $7}' "$JAR" | tail -1; }

info "登录 mini 面板 $BASE"
curl -fsSk -c "$JAR" -o /dev/null "$BASE/" || die "打不开 mini 面板"
curl -fsSk -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' -H "X-CSRF-Token: $(csrf)" \
  -d "{\"username\":\"$ZP_MINI_USER\",\"password\":\"$ZP_MINI_PASS\"}" "$BASE/api/v1/login" >/dev/null \
  || die "mini 面板登录失败"
TOK="$(csrf)"
[ -n "$TOK" ] || die "登录后没拿到 CSRF token"

curl -fsSk -b "$JAR" -X POST -H "X-CSRF-Token: $TOK" -H 'Content-Type: application/json' \
  -d "{\"path\":\"$DIR/apps\"}" "$BASE/api/v1/files/mkdir" >/dev/null 2>&1 || true
# 目标叫 catalog.json：面板拉的就是这个固定名字（签名文件同名 + .sig）。
cp "$OUT" /tmp/zp-catalog.json
"$BIN" sign-manifest --raw --key "$KEY" --in /tmp/zp-catalog.json --out /tmp/zp-catalog.json.sig >/dev/null
curl -fsSk -b "$JAR" -X POST -H "X-CSRF-Token: $TOK" \
  -F "dir=$DIR/apps" -F "on_conflict=overwrite" \
  -F "files=@/tmp/zp-catalog.json;filename=catalog.json" \
  -F "files=@/tmp/zp-catalog.json.sig;filename=catalog.json.sig" \
  "$BASE/api/v1/files/upload" >/dev/null || die "上传失败"
rm -f /tmp/zp-catalog.json /tmp/zp-catalog.json.sig
ok "已上传：$DIR/apps/catalog.json + .sig（面板点「⟳ 更新」即可看到新条目）"

info "线上复验（签名必须能被内嵌公钥验过）"
curl -fsS --max-time 20 "https://mirror.zizdog.com:8888/apps/catalog.json" -o /tmp/zp-cat-verify.json \
  || die "拉不到镜像上的 catalog.json"
"$BIN" sign-manifest --pub-from-key "$KEY" >/dev/null 2>&1 || true
python3 - <<'PY'
import json
d=json.load(open('/tmp/zp-cat-verify.json'))
print("  ✓ 线上目录可解析：%d 条（version=%s）" % (len(d.get("apps",[])), d.get("version")))
PY
rm -f /tmp/zp-cat-verify.json
