#!/usr/bin/env bash
# ============================================================================
#  deploy.sh —— 一条命令完成"发布 → 推你自己的镜像机 → 升级本机 → 验证"
#
#  为什么要有它：以前每次发布是 6~7 个手工步骤（bump/check/release/mirror-public/
#  逐文件 scp/各自 upgrade），其中**逐文件 scp 每个文件都要重新握一次手、
#  输一次密码**，是最大的时间浪费。这里全部改成：
#    · 上传用**一条 tar 流**（一次 ssh 会话）；
#    · 最后打印本机版本与市场条目做验证。
#
#  地址由调用者提供，仓库里不留任何内网默认值。
#  用法：
#    ZP_PASS='面板口令' NAS_HOST='<你的镜像机>' NAS_USER='<用户>' NAS_ROOT='<镜像目录>' \
#      MIRROR_URL='https://<你的镜像机>/zizpanel' NAS_PASS='镜像机口令' bash tools/deploy.sh
#    可选：ZP_USER（默认 admin）、SKIP_CHECK=1 跳过 make check、SKIP_BUILD=1 复用已有产物
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' internal/version/version.go | head -1)"
RELDIR="dist/release"
NAS_HOST="${NAS_HOST:-}"
NAS_USER="${NAS_USER:-}"
NAS_ROOT="${NAS_ROOT:-}"
MIRROR_URL="${MIRROR_URL:-}"
ZP_USER="${ZP_USER:-admin}"
: "${NAS_HOST:?请传 NAS_HOST=<你自己的镜像机>}"
: "${NAS_USER:?请传 NAS_USER=<镜像机用户>}"
: "${NAS_ROOT:?请传 NAS_ROOT=<镜像上的 zizpanel 目录>}"
: "${MIRROR_URL:?请传 MIRROR_URL=<镜像的 zizpanel 地址，如 https://mirror.example.com/zizpanel>}"
: "${ZP_PASS:?需要面板口令：ZP_PASS='...'}"

# NAS 登录方式：**优先 SSH 密钥**，密钥不通才要 NAS_PASS。
# 为什么不再强制口令：本机对 NAS 早有密钥，强制口令会让一条命令的部署在
# 没口令时直接 `:?` 退出（而口令在仓库里是禁止的，见 AGENTS.md 铁律 7）。
# 探测必须用 BatchMode=yes：否则 ssh 会挂在那里等输密码，看上去像卡死。
NAS_KEY_AUTH=0
if ssh -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new \
     "$NAS_USER@$NAS_HOST" true >/dev/null 2>&1; then
  NAS_KEY_AUTH=1
fi
if [ "$NAS_KEY_AUTH" != "1" ]; then
  : "${NAS_PASS:?SSH 密钥登不上 NAS，需要 NAS 口令：NAS_PASS='...'}"
fi

# nas_sh '<远端命令>'：按探测结果选密钥 / sshpass / expect 三种方式之一
nas_sh() {
  if [ "$NAS_KEY_AUTH" = "1" ]; then
    ssh -o StrictHostKeyChecking=accept-new "$NAS_USER@$NAS_HOST" "$1"
  elif command -v sshpass >/dev/null 2>&1; then
    sshpass -p "$NAS_PASS" ssh -o StrictHostKeyChecking=accept-new "$NAS_USER@$NAS_HOST" "$1"
  else
    # 用 expect 提供密码（macOS 自带 /usr/bin/expect）
    command -v expect >/dev/null 2>&1 || { echo "需要 sshpass 或 expect"; exit 1; }
    expect <<EOF >/dev/null
set timeout 300
spawn ssh -o StrictHostKeyChecking=accept-new $NAS_USER@$NAS_HOST "$1"
expect { -re "(?i)password:" { send "$NAS_PASS\r"; exp_continue } eof }
EOF
  fi
}

# 面板地址：**后缀从面板自己的配置读**（曾经写死 jab5c63，面板换后缀后就 404 了）。
# 面板后缀允许为空（面板设置里可以关掉），所以这里按"有则拼、无则不拼"处理。
PANEL_DATA="${PANEL_DATA:-/opt/zizpanel/data}"
LOCAL_SUFFIX="${LOCAL_SUFFIX:-$(sed -n 's/.*"panel_suffix"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$PANEL_DATA/config.json" 2>/dev/null | head -1)}"
LOCAL_URL="https://127.0.0.1:8443${LOCAL_SUFFIX:+/$LOCAL_SUFFIX}"

# 🛑 2026-09-17 用户明确要求：1.0.0 起 Mac mini 是**生产环境**，本脚本/本机**只升级本机**。
# 历史：这里曾有 MINI_URL（局域网 + 后缀）与"并行升级两台"—— 那会让一次 `make deploy`
# 直接动用户的生产面板，已按新规**整段删除**。生产机的升级由用户自己在面板里点「在线升级」。
# 教训记在 DEVELOPMENT.md：**"测试环境"变成生产环境时，要把工具里的目标一起删掉**，
# 否则下一个会话/下一个 agent 会照着旧脚本继续动生产。

step() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

# 部署只发本机架构（本机是 Apple Silicon / arm64）。
# 之前把 arm64+amd64 的「版本包 + latest 包」共 4 个 ≈96MB 全传一遍，
# 而 amd64 这台机器上永远用不到 —— 现在 2 个 ≈46MB。
ARCHS="${ARCHS:-arm64}"

# ---------------------------------------------------------------- 构建 --
if [ "${SKIP_BUILD:-0}" != "1" ]; then
  # 门禁：同一棵树上重复跑 make check 没有新信息（5~7 分钟）。
  # 但"跳过"必须是**可核对的事实**而不是手写的 SKIP_CHECK —— 所以比对
  # make check 成功时写下的指纹（tools/check-stamp.sh）。
  if [ "${SKIP_CHECK:-0}" = "1" ]; then
    step "跳过 make check（SKIP_CHECK=1）"
  elif stamp_msg="$(bash tools/check-stamp.sh verify 2>&1)"; then
    step "跳过 make check：${stamp_msg}"
  else
    step "make check（门禁）—— ${stamp_msg}"
    make check
  fi
  step "make release + mirror-public（构建 ARCHS=${ARCHS} + 签公网镜像版清单）"
  make release ARCHS="$ARCHS" >/dev/null
  make mirror-public ARCHS="$ARCHS" >/dev/null
else
  step "跳过构建（SKIP_BUILD=1），复用 $RELDIR"
fi

# ---------------------------------------------------------------- 上传 --
# 一条 tar 流：只握一次手。登录方式见上面的 nas_sh（密钥优先）。
step "推送到镜像机（单流 tar，版本 ${VERSION}）"
# 只传**版本的**包（latest 与 download/<版本>/ 的软链/副本由下面的 LAYOUT 在 NAS 上造）。
# 以前连两份 latest 副本一起传，等于白传一份同样的 46MB。
FILES=(manifest.json manifest.json.sig install.sh)
for a in $ARCHS; do
  FILES+=("zizpanel_${VERSION}_darwin_${a}.tar.gz")
done
TAR_LIST=()
for f in "${FILES[@]}"; do [ -e "$RELDIR/$f" ] && TAR_LIST+=("$f"); done
# 注意：tar 在**本机**读，ssh 只负责接收 —— 不要再套一层 `sh -c`，
# 那样子进程的 stdin 不是数据流，tar 会读到空。
if [ "$NAS_KEY_AUTH" = "1" ]; then
  tar cf - -C "$REPO_ROOT/$RELDIR" "${TAR_LIST[@]}" \
    | ssh -o StrictHostKeyChecking=accept-new "$NAS_USER@$NAS_HOST" "cd $NAS_ROOT && tar xf -"
elif command -v sshpass >/dev/null 2>&1; then
  tar cf - -C "$REPO_ROOT/$RELDIR" "${TAR_LIST[@]}" \
    | sshpass -p "$NAS_PASS" ssh -o StrictHostKeyChecking=accept-new "$NAS_USER@$NAS_HOST" "cd $NAS_ROOT && tar xf -"
else
  command -v expect >/dev/null 2>&1 || { echo "需要 sshpass 或 expect"; exit 1; }
  # expect 路径仍需把整条管道交给 sh -c：expect 只能给"它自己 spawn 的那个 ssh"喂密码，
  # 而这里的 ssh 在管道右端，所以要让它成为 spawn 的直接子进程。
  TAR_CMD="cd '$REPO_ROOT/$RELDIR' && tar cf - ${TAR_LIST[*]} | ssh -o StrictHostKeyChecking=accept-new $NAS_USER@$NAS_HOST 'cd $NAS_ROOT && tar xf -'"
  expect <<EOF >/dev/null
set timeout 900
spawn sh -c "$TAR_CMD"
expect { -re "(?i)password:" { send "$NAS_PASS\r"; exp_continue } eof }
EOF
fi

# 远端铺 download/<版本>/ 与 latest 链接（一次 ssh）。
# 必须**按实际发布的架构**逐个铺：以前这里写的是 `zizpanel_${VERSION}_darwin_*.tar.gz`
# 通配，只发 arm64 时 cp 找不到 amd64 包 → `set -e` 直接失败（这个坑本轮实测踩到，
# 当时 NAS 上还留着旧 amd64 包才没暴露）。
LAYOUT="cd $NAS_ROOT && mkdir -p download/$VERSION download/latest"
for a in $ARCHS; do
  LAYOUT="$LAYOUT && cp -f zizpanel_${VERSION}_darwin_${a}.tar.gz download/$VERSION/"
  LAYOUT="$LAYOUT && ln -sfn ../$VERSION/zizpanel_${VERSION}_darwin_${a}.tar.gz download/latest/zizpanel_latest_darwin_${a}.tar.gz"
  LAYOUT="$LAYOUT && ln -sfn download/$VERSION/zizpanel_${VERSION}_darwin_${a}.tar.gz zizpanel_${VERSION}_darwin_${a}.tar.gz"
done
nas_sh "$LAYOUT"

# ---------------------------------------------------------------- 升级 --
# 只升级**本机**（生产机不许碰，见文件顶部说明）。
step "升级本机"
python3 tools/panel-upgrade.py "$LOCAL_URL" "$ZP_USER" "$ZP_PASS" "$MIRROR_URL" "$VERSION" > /tmp/zp-deploy-local.log 2>&1 &
P1=$!
wait $P1 || true
echo "  --- 本机 ---"; tail -2 /tmp/zp-deploy-local.log

# ---------------------------------------------------------------- 验证 --
step "验证真实版本"
# 注意带面板安全后缀：面板界面与接口都在 /<suffix>/ 之下，
# 不带后缀会 404（这里踩过一次，验证输出成了"(读不到)"）。
printf "  %-44s " "$LOCAL_URL"
curl -sS -k -m 10 "$LOCAL_URL/api/v1/health" 2>/dev/null | grep -oE '"version":"[^"]+"' || echo "(读不到)"
echo
echo "完成（生产机未触碰）。"
