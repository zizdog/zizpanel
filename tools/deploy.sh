#!/usr/bin/env bash
# ============================================================================
#  deploy.sh —— 一条命令完成"发布 → 推 NAS → 升级两台机器 → 验证"
#
#  为什么要有它：以前每次发布是 6~7 个手工步骤（bump/check/release/mirror-nas/
#  逐文件 scp/两台各自 upgrade），其中**逐文件 scp 每个文件都要重新握一次手、
#  输一次密码**，是最大的时间浪费。这里全部改成：
#    · 上传用**一条 tar 流**（一次 ssh 会话）；
#    · 两台机器的升级**并行**跑；
#    · 最后打印两台的版本与市场条目做验证。
#
#  用法：
#    ZP_PASS='面板口令' NAS_PASS='NAS口令' bash tools/deploy.sh
#    可选：ZP_USER（默认 admin）、SKIP_CHECK=1 跳过 make check、SKIP_BUILD=1 复用已有产物
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' internal/version/version.go | head -1)"
RELDIR="dist/release"
NAS_HOST="${NAS_HOST:-192.168.1.8}"
NAS_USER="${NAS_USER:-zizdog}"
NAS_ROOT="${NAS_ROOT:-/vol2/zizpanel-mirror/zizpanel}"
MIRROR_URL="${MIRROR_URL:-http://192.168.1.8:8090/zizpanel}"
ZP_USER="${ZP_USER:-admin}"
: "${ZP_PASS:?需要面板口令：ZP_PASS='...'}"
: "${NAS_PASS:?需要 NAS 口令：NAS_PASS='...'}"

LOCAL_URL="https://127.0.0.1:8443/jab5c63"
MINI_URL="https://192.168.1.4:8443/6zfxgccj"

step() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------- 构建 --
if [ "${SKIP_BUILD:-0}" != "1" ]; then
  step "make check（门禁）"
  [ "${SKIP_CHECK:-0}" = "1" ] || make check
  step "make release + mirror-nas（构建 + 签 NAS 版清单）"
  make release >/dev/null
  make mirror-nas >/dev/null
else
  step "跳过构建（SKIP_BUILD=1），复用 $RELDIR"
fi

# ---------------------------------------------------------------- 上传 --
# 一条 tar 流：只握一次手。sshpass 可用就用它，否则用 expect（macOS 自带）。
step "推送到 NAS（单流 tar，版本 ${VERSION}）"
FILES=(manifest.json manifest.json.sig install.sh
  "zizpanel_${VERSION}_darwin_arm64.tar.gz" "zizpanel_${VERSION}_darwin_amd64.tar.gz"
  zizpanel_latest_darwin_arm64.tar.gz zizpanel_latest_darwin_amd64.tar.gz)
TAR_LIST=()
for f in "${FILES[@]}"; do [ -e "$RELDIR/$f" ] && TAR_LIST+=("$f"); done
TAR_CMD="cd '$REPO_ROOT/$RELDIR' && tar cf - ${TAR_LIST[*]} | ssh -o StrictHostKeyChecking=accept-new $NAS_USER@$NAS_HOST 'cd $NAS_ROOT && tar xf -'"
if command -v sshpass >/dev/null 2>&1; then
  sshpass -p "$NAS_PASS" sh -c "$TAR_CMD"
else
  # 用 expect 提供密码（macOS 自带 /usr/bin/expect）
  command -v expect >/dev/null 2>&1 || { echo "需要 sshpass 或 expect"; exit 1; }
  expect <<EOF >/dev/null
set timeout 900
spawn sh -c "$TAR_CMD"
expect { -re "(?i)password:" { send "$NAS_PASS\r"; exp_continue } eof }
EOF
fi

# 远端铺 download/<版本>/ 与 latest 链接（一次 ssh）
LAYOUT="cd $NAS_ROOT && mkdir -p download/$VERSION download/latest && cp -f zizpanel_${VERSION}_darwin_*.tar.gz download/$VERSION/ && ln -sfn ../$VERSION/zizpanel_${VERSION}_darwin_arm64.tar.gz download/latest/zizpanel_latest_darwin_arm64.tar.gz && ln -sfn ../$VERSION/zizpanel_${VERSION}_darwin_amd64.tar.gz download/latest/zizpanel_latest_darwin_amd64.tar.gz && ln -sfn download/$VERSION/zizpanel_${VERSION}_darwin_arm64.tar.gz zizpanel_${VERSION}_darwin_arm64.tar.gz && ln -sfn download/$VERSION/zizpanel_${VERSION}_darwin_amd64.tar.gz zizpanel_${VERSION}_darwin_amd64.tar.gz"
if command -v sshpass >/dev/null 2>&1; then
  sshpass -p "$NAS_PASS" ssh -o StrictHostKeyChecking=accept-new "$NAS_USER@$NAS_HOST" "$LAYOUT"
else
  expect <<EOF >/dev/null
set timeout 300
spawn ssh -o StrictHostKeyChecking=accept-new $NAS_USER@$NAS_HOST "$LAYOUT"
expect { -re "(?i)password:" { send "$NAS_PASS\r"; exp_continue } eof }
EOF
fi

# ---------------------------------------------------------------- 升级 --
# 两台**并行**升级，不再一台等完再等另一台。
step "并行升级两台机器"
python3 tools/panel-upgrade.py "$LOCAL_URL" "$ZP_USER" "$ZP_PASS" "$MIRROR_URL" "$VERSION" > /tmp/zp-deploy-local.log 2>&1 &
P1=$!
python3 tools/panel-upgrade.py "$MINI_URL" "$ZP_USER" "$ZP_PASS" "$MIRROR_URL" "$VERSION" > /tmp/zp-deploy-mini.log 2>&1 &
P2=$!
wait $P1 || true; wait $P2 || true
echo "  --- 本机 ---"; tail -2 /tmp/zp-deploy-local.log
echo "  --- mini ---"; tail -2 /tmp/zp-deploy-mini.log

# ---------------------------------------------------------------- 验证 --
step "验证真实版本"
# 注意带面板安全后缀：面板界面与接口都在 /<suffix>/ 之下，
# 不带后缀会 404（这里踩过一次，验证输出成了"(读不到)"）。
for u in "$LOCAL_URL" "$MINI_URL"; do
  printf "  %-44s " "$u"
  curl -sS -k -m 10 "$u/api/v1/health" 2>/dev/null | grep -oE '"version":"[^"]+"' || echo "(读不到)"
done
echo
echo "完成。"
