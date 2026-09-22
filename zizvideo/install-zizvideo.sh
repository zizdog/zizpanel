#!/usr/bin/env bash
# zizvideo 独立部署安装器（单文件，可 curl | bash）。
# 默认 system 模式：系统级 LaunchDaemon，开机即起，不需要任何人登录（无头 Mac 可用）。
# --user 模式：用户级 LaunchAgent，只在有人登录后运行，仅作无 sudo 的备选。
# 两种模式都与面板托管（cn.zizpanel.zizvideo）互斥，见 check_conflicts。
set -euo pipefail

INSTALLER_VERSION="1.1.0"
DEFAULT_MIRROR="https://mirror.zizdog.com:8888"
DEFAULT_LABEL="com.zizvideo.server"
PANEL_LABEL="cn.zizpanel.zizvideo"
APP_ID="zizvideo"
HEALTH_PATH="/healthz"
DEFAULT_LISTEN="127.0.0.1:7766"
CRT_NAME="zizvideo-codesign.crt"
SIGN_IDENTIFIER="$DEFAULT_LABEL"
SYSTEM_KEYCHAIN="/Library/Keychains/System.keychain"

# ---------------------------------------------------------------------------
#  仅测试用的覆盖口（生产环境一律不要设置）
#    ZV_INSTALL_ROOT       二进制根，默认 $HOME/.local（产物落 <root>/bin/zizvideo）
#    ZV_TEST_LABEL         覆盖 launchd label，默认 com.zizvideo.server
#    ZV_FAKE_LAUNCHCTL     假 launchctl 可执行文件（绝不碰真 launchd）
#    ZV_SYSTEM_DAEMON_DIR  覆盖系统 LaunchDaemons 目录，默认 /Library/LaunchDaemons
#    ZV_FAKE_SUDO=1        特权命令直接执行、不调 sudo（沙箱专用）
#    ZV_TEST_KEYCHAIN      覆盖证书导入的钥匙串路径，默认系统钥匙串（沙箱专用）
#    ZV_ASSUME_YES=1       非交互确认（只给 --purge 用）
# ---------------------------------------------------------------------------

MIRROR="$DEFAULT_MIRROR"
LABEL="${ZV_TEST_LABEL:-$DEFAULT_LABEL}"
HOME_DIR="${HOME:-}"
INSTALL_ROOT="${ZV_INSTALL_ROOT:-$HOME_DIR/.local}"
SYSTEM_DAEMON_DIR="${ZV_SYSTEM_DAEMON_DIR:-/Library/LaunchDaemons}"
LISTEN=""
MODE="system"
ACTION="install"
DRY_RUN=0
LISTEN_GIVEN=0
FAKE_SUDO=0
TARGET_USER=""

# 运行期状态（回滚用）。
BIN=""
PLIST=""
OTHER_PLIST=""
CONFIG_PATH=""
DATA_DIR=""
LOG_DIR=""
CONFIG_NEW=0
DATA_DIR_NEW=0
BIN_NEW=0
BIN_BACKUP=""
PLIST_NEW=0
PLIST_BACKUP=""
BOOTED=0
EFFECTIVE_LISTEN=""
HAVE_CRT=0
INSTALL_STARTED=0
INSTALL_DONE=0

say()  { printf '%s\n' "$*"; }
warn() { printf '警告：%s\n' "$*" >&2; }
die()  { printf '错误：%s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
zizvideo 独立部署安装器

用法：
  install-zizvideo.sh [--system|--user] [--mirror <base>] [--listen <host:port>] [--dry-run]
  install-zizvideo.sh --upgrade   [--system|--user] [--dry-run]
  install-zizvideo.sh --uninstall [--system|--user] [--dry-run]
  install-zizvideo.sh --purge     [--system|--user] [--dry-run]
  install-zizvideo.sh --version | --help

模式：
  --system（默认）系统级 LaunchDaemon，开机即起、不需要登录，无头 Mac 用这个。
                   写 /Library/LaunchDaemons 并 bootstrap 系统域，需要 sudo。
  --user          用户级 LaunchAgent，只在有人登录后运行；无 sudo 才用它。

其它：
  默认镜像 https://mirror.zizdog.com:8888，读 apps/zizvideo/manifest.json 的应用级索引。
  装到 ~/.local/bin/zizvideo；日志在 ~/Library/Logs/。
  数据目录 ~/Library/Application Support/zizvideo，卸载默认保留，--purge 才删（需二次确认）。
  与面板托管互斥：系统域有 cn.zizpanel.zizvideo 时拒绝独立安装。
  签名：镜像同目录有 zizvideo-codesign.crt 时导入系统钥匙串并复核签名，外置盘完全磁盘访问授权一次跨升级有效；
        镜像没有 crt 则降级为未签名部署，每次升级都要重新授权。
EOF
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --help|-h) usage; exit 0 ;;
      --version) say "install-zizvideo.sh $INSTALLER_VERSION"; exit 0 ;;
      --dry-run) DRY_RUN=1 ;;
      --system) MODE="system" ;;
      --user) MODE="user" ;;
      --upgrade) ACTION="upgrade" ;;
      --uninstall) ACTION="uninstall" ;;
      --purge) ACTION="purge" ;;
      --mirror) shift; [ $# -gt 0 ] || die "--mirror 缺参数"; MIRROR="$1" ;;
      --mirror=*) MIRROR="${1#--mirror=}" ;;
      --listen) shift; [ $# -gt 0 ] || die "--listen 缺参数"; LISTEN="$1"; LISTEN_GIVEN=1 ;;
      --listen=*) LISTEN="${1#--listen=}"; LISTEN_GIVEN=1 ;;
      *) die "未知参数：$1（--help 看用法）" ;;
    esac
    shift
  done
  MIRROR="${MIRROR%/}"
  [ -n "$HOME_DIR" ] || die "HOME 为空，无法确定用户目录（请以真实用户身份运行）"
}

# 路径与运行身份（沙箱测试只需改 HOME / ZV_INSTALL_ROOT / ZV_SYSTEM_DAEMON_DIR）。
derive_paths() {
  TARGET_USER=$(id -un)
  BIN="$INSTALL_ROOT/bin/zizvideo"
  CONFIG_PATH="$HOME_DIR/Library/Application Support/zizvideo/config.json"
  DATA_DIR="$HOME_DIR/Library/Application Support/zizvideo"
  LOG_DIR="$HOME_DIR/Library/Logs"
  if [ "$MODE" = "system" ]; then
    PLIST="$SYSTEM_DAEMON_DIR/$LABEL.plist"
    OTHER_PLIST="$HOME_DIR/Library/LaunchAgents/$LABEL.plist"
  else
    PLIST="$HOME_DIR/Library/LaunchAgents/$LABEL.plist"
    OTHER_PLIST="$SYSTEM_DAEMON_DIR/$LABEL.plist"
  fi
  if [ "$MODE" = "system" ]; then
    DOMAIN="system"
  else
    DOMAIN="gui/$(id -u)"
  fi
}

# launchctl 封装（ZV_FAKE_LAUNCHCTL 仅测试用）。
LAUNCHCTL_BIN="/bin/launchctl"
if [ -n "${ZV_FAKE_LAUNCHCTL:-}" ]; then
  LAUNCHCTL_BIN="$ZV_FAKE_LAUNCHCTL"
fi
lc() { "$LAUNCHCTL_BIN" "$@"; }

# 特权命令：system 模式写 /Library/LaunchDaemons 与 bootstrap 系统域需要 sudo。
run_priv() {
  if [ "$FAKE_SUDO" = "1" ]; then
    "$@"
    return $?
  fi
  sudo "$@"
}

# ---------------------------------------------------------------------------
#  前置检查
# ---------------------------------------------------------------------------
check_arch() {
  [ "$(uname -s)" = "Darwin" ] || die "只支持 macOS（当前 $(uname -s)）"
  [ "$(uname -m)" = "arm64" ] || die "只支持 Apple 芯片（arm64）；当前架构 $(uname -m)，不跑 Rosetta 转译"
}

check_curl() {
  command -v curl >/dev/null 2>&1 || die "找不到 curl（macOS 自带 /usr/bin/curl），无法下载产物"
}

# 安装器必须以真实用户身份跑（这样 ~/.local 里的产物归该用户，daemon 也以该用户跑）。
# 特权步骤由 run_priv 内部按需 sudo，所以不要 sudo 整个脚本。
check_identity() {
  if [ "$(id -u)" = "0" ]; then
    die "请以真实用户身份运行本脚本（不要 sudo 整个脚本）；需要写系统目录时脚本会自己调 sudo"
  fi
  [ -n "$TARGET_USER" ] || die "取不到当前用户名（id -un 为空）"
  if [ "$MODE" = "system" ] && [ "$FAKE_SUDO" != "1" ]; then
    command -v sudo >/dev/null 2>&1 || die "system 模式需要 sudo（写 $SYSTEM_DAEMON_DIR 并 bootstrap 系统域）；无 sudo 请改用 --user"
  fi
}

# 磁盘余量 ≥ 产物 ×2（下载临时文件 + 安装位）。
check_disk() {
  size_bytes="$1"
  parent="$INSTALL_ROOT"
  if [ ! -d "$parent" ]; then parent="$HOME_DIR"; fi
  avail_kb=$(df -k "$parent" | awk 'NR==2 {print $4}')
  need_kb=$(( size_bytes * 2 / 1024 ))
  if [ "${avail_kb:-0}" -lt "$need_kb" ]; then
    die "磁盘空间不足：$parent 可用 ${avail_kb}KB，需要约 ${need_kb}KB（产物 ×2）"
  fi
}

port_of() { printf '%s' "${1##*:}"; }
host_of() { printf '%s' "${1%:*}"; }

port_in_use() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    return 1
  fi
}

panel_managed() {
  if [ -f "$SYSTEM_DAEMON_DIR/$PANEL_LABEL.plist" ]; then return 0; fi
  if lc print "system/$PANEL_LABEL" >/dev/null 2>&1; then return 0; fi
  return 1
}

own_system_installed() {
  if [ -f "$SYSTEM_DAEMON_DIR/$LABEL.plist" ]; then return 0; fi
  if lc print "system/$LABEL" >/dev/null 2>&1; then return 0; fi
  return 1
}

own_user_installed() {
  [ -f "$HOME_DIR/Library/LaunchAgents/$LABEL.plist" ]
}

refuse_panel() {
  say "拒绝安装：检测到面板托管的 zizvideo（系统域 $PANEL_LABEL 正在运行）。"
  say "独立部署与面板托管互斥：两者会争同一个端口与数据目录，绝不能同时跑。"
  say "两条出路："
  say "  ① 用面板托管：在面板「应用市场 → zizvideo」里安装/使用，本脚本不参与。"
  say "  ② 先独立装：先在面板里卸载 zizvideo，确认 \`launchctl print system/$PANEL_LABEL\` 失败后，再重跑本脚本。"
}

refuse_other_mode() {
  say "拒绝安装：检测到另一种模式的独立部署（$1）。"
  say "两种模式共用同一 label 与端口，不能并存。先卸载它，再用当前模式装："
  if [ "$MODE" = "system" ]; then
    say "  install-zizvideo.sh --user --uninstall"
  else
    say "  install-zizvideo.sh --system --uninstall（脚本会提示 sudo）"
  fi
}

# 安装前的互斥总检查：面板托管 > 另一种模式 > 同模式（走升级）。
check_conflicts_install() {
  if panel_managed; then
    refuse_panel
    exit 1
  fi
  say "互斥检查通过：系统域没有面板托管的 $PANEL_LABEL"
  if [ "$MODE" = "system" ]; then
    if own_user_installed; then
      refuse_other_mode "--user 模式的 LaunchAgent $OTHER_PLIST"
      exit 1
    fi
  else
    if own_system_installed; then
      refuse_other_mode "--system 模式的 LaunchDaemon $OTHER_PLIST"
      exit 1
    fi
  fi
}

# ---------------------------------------------------------------------------
#  镜像索引 + 产物
# ---------------------------------------------------------------------------
fetch_index() {
  index_tmp="$1"
  url="$MIRROR/apps/$APP_ID/manifest.json"
  say "读应用级索引：$url"
  if ! curl -fsSL --max-time 30 --retry 2 -o "$index_tmp" "$url"; then
    die "取索引失败（${url}）：镜像不可达或索引不存在（HTTP 错误见上）"
  fi
  [ -s "$index_tmp" ] || die "索引为空：$url"
}

json_str() { sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$1" | head -n1; }
json_num() { sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' <<<"$1" | head -n1; }

# 只认 arch=="arm64"；优先取 version==latest 的那条。
resolve_release() {
  index="$1"
  LATEST=$(sed -n 's/.*"latest"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$index" | head -n1)
  [ -n "$LATEST" ] || die "索引里没有 latest 字段（$MIRROR/apps/$APP_ID/manifest.json）"
  # 先并成一行再按 } 切块：索引可能是多行美化 JSON，逐行 grep 会丢字段。
  arm64=$(tr '\n' ' ' <"$index" | tr '}' '\n' | grep '"arch"' | grep -F '"arm64"' || true)
  [ -n "$arm64" ] || die "索引 latest=$LATEST 里没有 arch==arm64 的产物（本机只装原生 arm64）"
  line=$(printf '%s\n' "$arm64" | grep -E '"version"[[:space:]]*:[[:space:]]*"'"$LATEST"'"' | head -n1 || true)
  if [ -z "$line" ]; then line=$(printf '%s\n' "$arm64" | head -n1); fi
  ASSET=$(json_str "$line" name)
  ASSET_VER=$(json_str "$line" version)
  ASSET_SHA=$(json_str "$line" sha256)
  ASSET_SIZE=$(json_num "$line" size)
  [ -n "$ASSET" ] || die "索引里那条 arm64 产物没有 name 字段"
  if [ -z "$ASSET_VER" ]; then ASSET_VER="$LATEST"; fi
  [ -n "$ASSET_SHA" ] || die "索引里 $ASSET 没有 sha256 字段，无法校验，拒绝安装"
  [ -n "$ASSET_SIZE" ] || die "索引里 $ASSET 没有 size 字段，无法做磁盘前置检查"
}

sha256_of() {
  command -v shasum >/dev/null 2>&1 || die "找不到 shasum，无法校验 sha256"
  shasum -a 256 "$1" | awk '{print $1}'
}

installed_version() {
  if [ ! -x "$BIN" ]; then printf ''; return 0; fi
  out=$("$BIN" --version 2>/dev/null || true)
  printf '%s' "${out##* }"
}

# 下载到临时文件并核 sha256；不通过就删掉，绝不落到安装位。
download_verify() {
  tmp="$1"
  url="$MIRROR/apps/$APP_ID/$ASSET_VER/$ASSET"
  say "下载 $url"
  if ! curl -fSL --max-time 300 --retry 2 -o "$tmp" "$url"; then
    rm -f "$tmp"; die "下载失败：${url}（HTTP 错误见上）"
  fi
  got=$(sha256_of "$tmp")
  if [ "$got" != "$ASSET_SHA" ]; then
    rm -f "$tmp"
    die "sha256 不匹配：期望 ${ASSET_SHA}，实际 ${got}（该文件已删除，未安装任何东西；镜像包与索引不一致，请重新同步 apps/${APP_ID}）"
  fi
  chmod 0755 "$tmp"
  say "sha256 校验通过：$got"
}

# ---------------------------------------------------------------------------
#  代码签名证书：镜像同目录取 crt → root 导入信任 → 复核（identifier + 证书链）
#  有固定证书签名后，完全磁盘访问授权一次跨升级有效；没有 crt 就降级为未签名。
# ---------------------------------------------------------------------------
fetch_cert() {
  dst="$1"
  url="$MIRROR/apps/$APP_ID/$ASSET_VER/$CRT_NAME"
  if curl -fsSL --max-time 30 --retry 1 -o "$dst" "$url" 2>/dev/null && [ -s "$dst" ]; then
    say "已取签名证书：$url"
    return 0
  fi
  rm -f "$dst"
  return 1
}

cert_keychain() {
  if [ -n "${ZV_TEST_KEYCHAIN:-}" ]; then printf '%s' "$ZV_TEST_KEYCHAIN"; return 0; fi
  printf '%s' "$SYSTEM_KEYCHAIN"
}

# 同名旧证书先删掉再加，保证升级时幂等（重复导入会 errSecDuplicate).
# ZV_TEST_KEYCHAIN 走 import 而不写系统信任设置：add-trusted-cert 在沙箱里会弹授权框。
import_trust_cert() {
  crt="$1"
  kc=$(cert_keychain)
  command -v security >/dev/null 2>&1 || { warn "找不到 security，无法导入证书"; return 1; }
  cn=""
  if command -v openssl >/dev/null 2>&1; then
    cn=$(openssl x509 -noout -subject -in "$crt" 2>/dev/null | sed -e 's/.*CN[[:space:]]*=[[:space:]]*//' -e 's/,.*//' -e 's/^[[:space:]]*//')
  fi
  if [ -n "${ZV_TEST_KEYCHAIN:-}" ]; then
    if [ -n "$cn" ]; then security delete-certificate -c "$cn" "$kc" >/dev/null 2>&1 || true; fi
    security import "$crt" -k "$kc" -T /usr/bin/codesign -A >/dev/null 2>&1
    return $?
  fi
  if [ -n "$cn" ]; then
    if [ "$FAKE_SUDO" = "1" ]; then
      security delete-certificate -c "$cn" -t "$kc" >/dev/null 2>&1 || true
    else
      sudo security delete-certificate -c "$cn" -t "$kc" >/dev/null 2>&1 || true
    fi
  fi
  say "导入并信任证书到钥匙串（需要 sudo）：$kc"
  if [ "$FAKE_SUDO" = "1" ]; then
    security add-trusted-cert -d -r trustRoot -k "$kc" "$crt" >/dev/null 2>&1
  else
    sudo security add-trusted-cert -d -r trustRoot -k "$kc" "$crt" >/dev/null 2>&1
  fi
}

# 复核：identifier 必须等于 com.zizvideo.server，且二进制里嵌的叶证书与镜像 crt 一致。
verify_signature() {
  bin="$1"; crt="$2"
  command -v codesign >/dev/null 2>&1 || { warn "找不到 codesign，无法复核签名"; return 1; }
  dv=$(codesign -dv --verbose=2 "$bin" 2>&1 || true)
  id=$(printf '%s\n' "$dv" | sed -n 's/^Identifier=//p' | head -n1)
  if [ "$id" != "$SIGN_IDENTIFIER" ]; then
    warn "签名 identifier=\"$id\"，期望 \"$SIGN_IDENTIFIER\""
    return 1
  fi
  # 沙箱用临时钥匙串时不在系统搜索列表里，跳过依赖信任链的 --verify（其余照查）。
  if [ -z "${ZV_TEST_KEYCHAIN:-}" ]; then
    if ! codesign --verify --verbose=2 "$bin" >/dev/null 2>&1; then
      warn "codesign 验签失败（证书可能没被信任）"
      return 1
    fi
  fi
  if command -v openssl >/dev/null 2>&1; then
    d=$(mktemp -d -t zv-cert.XXXXXX)
    ( cd "$d" && codesign -d --extract-certificates=cert "$bin" >/dev/null 2>&1 ) || true
    if [ -f "$d/cert0" ]; then
      openssl x509 -in "$crt" -outform der -out "$d/crt.der" >/dev/null 2>&1 || true
      if [ -f "$d/crt.der" ] && ! cmp -s "$d/cert0" "$d/crt.der"; then
        rm -rf "$d"; warn "签名证书与镜像 $CRT_NAME 不一致"; return 1
      fi
    fi
    rm -rf "$d"
  fi
  return 0
}

# 有 crt：导入 + 复核；没 crt：如实说"未签名"，绝不假装成功。
report_signature() {
  if [ "$HAVE_CRT" != "1" ]; then
    warn "镜像上没有 $CRT_NAME ⇒ 本次是**未签名独立部署**"
    warn "后果：读 /Volumes/* 外置盘需要的完全磁盘访问权限，每次升级都要重新授权"
    return 1
  fi
  kept="$DATA_DIR/$CRT_NAME"
  if ! import_trust_cert "$kept"; then
    warn "证书导入系统钥匙串失败（sudo 被拒或无权限）"
    warn "手动导入：sudo security add-trusted-cert -d -r trustRoot -k $SYSTEM_KEYCHAIN $kept"
    warn "未成功导入并信任前，读 /Volumes/* 的完全磁盘访问权限每次升级都要重新授权"
    return 1
  fi
  if verify_signature "$BIN" "$kept"; then
    say "签名复核通过：identifier=${SIGN_IDENTIFIER}，证书链来自 $CRT_NAME"
    say "完全磁盘访问授权一次跨升级有效"
    return 0
  fi
  warn "签名复核不过（原因见上）"
  warn "外置盘授权仍可用，但每次升级都要重新授权（完全磁盘访问）"
  return 1
}

# ---------------------------------------------------------------------------
#  配置文件 / plist
# ---------------------------------------------------------------------------
effective_listen() {
  if [ -n "$LISTEN" ]; then printf '%s' "$LISTEN"; return 0; fi
  if [ -f "$CONFIG_PATH" ]; then
    v=$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_PATH" | head -n1)
    if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  fi
  printf '%s' "$DEFAULT_LISTEN"
}

ensure_config() {
  if [ ! -f "$CONFIG_PATH" ]; then
    mkdir -p "$(dirname "$CONFIG_PATH")"
    cat >"$CONFIG_PATH" <<EOF
{
  "listen": "$(effective_listen)",
  "data_dir": "$DATA_DIR",
  "database_path": "$DATA_DIR/zizvideo.db"
}
EOF
    CONFIG_NEW=1
    say "已写默认配置：${CONFIG_PATH}（listen=$(effective_listen)）"
    return 0
  fi
  if [ "$LISTEN_GIVEN" = "1" ]; then
    tmp="$CONFIG_PATH.zv-tmp"
    if grep -q '"listen"' "$CONFIG_PATH"; then
      sed -e 's|"listen"[[:space:]]*:[[:space:]]*"[^"]*"|"listen": "'"$LISTEN"'"|' "$CONFIG_PATH" >"$tmp"
    else
      sed -e '0,/{/s|{|{\n  "listen": "'"$LISTEN"'",|' "$CONFIG_PATH" >"$tmp"
    fi
    mv "$tmp" "$CONFIG_PATH"
    say "已把配置里的 listen 改为 $LISTEN"
  fi
}

xml_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# system 模式写 UserName 让 launchd 在开机时以该用户身份启动（无需登录）；
# 同时显式给 HOME（LaunchDaemon 不保证带 HOME）；日志写用户目录，进程才写得进。
write_plist() {
  tmp="$PLIST.zv-tmp.$$"
  # LaunchAgent 目录可能还不存在（/Library/LaunchDaemons 系统自带）。
  if [ "$MODE" = "user" ]; then mkdir -p "$(dirname "$PLIST")"; fi
  {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$(xml_escape "$LABEL")</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(xml_escape "$BIN")</string>
        <string>--config</string>
        <string>$(xml_escape "$CONFIG_PATH")</string>
    </array>
EOF
    if [ "$MODE" = "system" ]; then
      printf '    <key>UserName</key>\n    <string>%s</string>\n' "$(xml_escape "$TARGET_USER")"
    fi
    cat <<EOF
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>WorkingDirectory</key>
    <string>$(xml_escape "$HOME_DIR")</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>$(xml_escape "$HOME_DIR")</string>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$(xml_escape "$LOG_DIR/zizvideo.out.log")</string>
    <key>StandardErrorPath</key>
    <string>$(xml_escape "$LOG_DIR/zizvideo.err.log")</string>
</dict>
</plist>
EOF
  } >"$tmp"
  if ! plutil -lint "$tmp" >/dev/null 2>&1; then
    rm -f "$tmp"; die "生成的 plist 不合法（${PLIST}）"
  fi
  if [ "$MODE" = "system" ]; then
    say "写系统 LaunchDaemon（需要 sudo）：$PLIST"
    if [ "$FAKE_SUDO" = "1" ]; then
      install -m 0644 "$tmp" "$PLIST" || { rm -f "$tmp"; die "写 $PLIST 失败"; }
    else
      sudo install -m 0644 -o root -g wheel "$tmp" "$PLIST" || { rm -f "$tmp"; die "写 $PLIST 失败（sudo 被拒或无权限）"; }
    fi
    say "已写 LaunchDaemon：$PLIST"
  else
    mkdir -p "$(dirname "$PLIST")"
    install -m 0644 "$tmp" "$PLIST" || { rm -f "$tmp"; die "写 $PLIST 失败"; }
    say "已写 LaunchAgent：$PLIST"
  fi
  rm -f "$tmp"
}

# ---------------------------------------------------------------------------
#  服务生命周期
# ---------------------------------------------------------------------------
service_running() {
  out=$(lc print "$DOMAIN/$LABEL" 2>/dev/null || true)
  case "$out" in *"state = running"*) return 0 ;; *) return 1 ;; esac
}

service_pid() {
  lc print "$DOMAIN/$LABEL" 2>/dev/null | sed -n 's/^[[:space:]]*pid = \([0-9][0-9]*\).*/\1/p' | head -n1
}

bootout_service() {
  if [ "$MODE" = "system" ]; then
    run_priv "$LAUNCHCTL_BIN" bootout "system/$LABEL" >/dev/null 2>&1 || true
  else
    lc bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
  fi
}

# bootout 旧的 → bootstrap → 只在没起来时补一次 kickstart（10 秒节流内不连踢）。
start_service() {
  bootout_service
  i=0
  while [ $i -lt 20 ] && service_running; do sleep 0.25; i=$((i + 1)); done
  if [ "$MODE" = "system" ]; then
    say "bootstrap 系统域（需要 sudo）：$PLIST"
    if ! run_priv "$LAUNCHCTL_BIN" bootstrap system "$PLIST" >/dev/null 2>&1; then
      return 1
    fi
  else
    if ! lc bootstrap "gui/$(id -u)" "$PLIST" >/dev/null 2>&1; then
      return 1
    fi
  fi
  BOOTED=1
  i=0
  while [ $i -lt 12 ]; do
    if service_running; then return 0; fi
    sleep 0.5; i=$((i + 1))
  done
  warn "bootstrap 后作业未进入 running，补一次 kickstart（只这一次，避免 10 秒节流）"
  if [ "$MODE" = "system" ]; then
    run_priv "$LAUNCHCTL_BIN" kickstart -k "system/$LABEL" >/dev/null 2>&1 || true
  else
    lc kickstart -k "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
  fi
  i=0
  while [ $i -lt 20 ]; do
    if service_running; then return 0; fi
    sleep 0.5; i=$((i + 1))
  done
  return 1
}

# ---------------------------------------------------------------------------
#  验收（判据贴运行体：版本 / 在跑 / 运行身份 / /healthz）
# ---------------------------------------------------------------------------
verify_acceptance() {
  v=$(installed_version)
  if [ "$v" != "$LATEST" ]; then
    say "验收第 1 步失败：$BIN --version 得到 \"$v\"，索引 latest 是 \"$LATEST\""
    return 1
  fi
  say "验收 1/4 通过：zizvideo --version = ${v}（= 索引 latest）"
  if ! service_running; then
    say "验收第 2 步失败：launchctl print $DOMAIN/$LABEL 里没有 state = running"
    return 1
  fi
  say "验收 2/4 通过：launchctl 里 $DOMAIN/$LABEL 在 running"
  pid=$(service_pid)
  if [ -z "$pid" ]; then
    say "验收第 3 步失败：launchctl print $DOMAIN/$LABEL 里读不到 pid（读不到就拒绝，不猜）"
    return 1
  fi
  run_user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
  if [ "$run_user" != "$TARGET_USER" ]; then
    say "验收第 3 步失败：pid $pid 的运行身份是 \"$run_user\"，期望 \"$TARGET_USER\"（数据/日志会被错的用户占有）"
    return 1
  fi
  say "验收 3/4 通过：pid $pid 以 $run_user 身份在跑"
  host=$(host_of "$EFFECTIVE_LISTEN")
  case "$host" in ""|"0.0.0.0"|"::"|"[::]") host="127.0.0.1" ;; esac
  port=$(port_of "$EFFECTIVE_LISTEN")
  url="http://$host:$port$HEALTH_PATH"
  i=0
  while [ $i -lt 60 ]; do
    body=$(curl -fsS --max-time 3 "$url" 2>/dev/null || true)
    case "$body" in *ok*) say "验收 4/4 通过：$url 返回 ok"; return 0 ;; esac
    sleep 0.5; i=$((i + 1))
  done
  say "验收第 4 步失败：$url 30 秒内没有返回 ok（最后响应：${body:-（空）}）"
  say "排查：tail -n 50 $LOG_DIR/zizvideo.err.log"
  return 1
}

# ---------------------------------------------------------------------------
#  回滚：把本次改动的文件恢复原状，并停掉本次装载的服务
# ---------------------------------------------------------------------------
remove_file() {
  path="$1"
  if [ "$MODE" = "system" ] && [ "$FAKE_SUDO" != "1" ]; then
    run_priv rm -f "$path" >/dev/null 2>&1 || true
  else
    rm -f "$path" >/dev/null 2>&1 || true
  fi
}

rollback() {
  say "开始回滚本次改动……"
  if [ "$BOOTED" = "1" ]; then
    bootout_service
    say "  - 已 bootout $DOMAIN/$LABEL"
  fi
  if [ "$PLIST_NEW" = "1" ] && [ -f "$PLIST" ]; then
    remove_file "$PLIST"; say "  - 已删除 $PLIST"
  elif [ -n "$PLIST_BACKUP" ] && [ -f "$PLIST_BACKUP" ]; then
    if [ "$MODE" = "system" ]; then
      if [ "$FAKE_SUDO" = "1" ]; then
        install -m 0644 "$PLIST_BACKUP" "$PLIST" >/dev/null 2>&1 || true
      else
        run_priv install -m 0644 -o root -g wheel "$PLIST_BACKUP" "$PLIST" >/dev/null 2>&1 || true
      fi
    else
      mv "$PLIST_BACKUP" "$PLIST"
    fi
    say "  - 已恢复旧 plist $PLIST"
  fi
  if [ "$BIN_NEW" = "1" ]; then
    if [ -n "$BIN_BACKUP" ] && [ -f "$BIN_BACKUP" ]; then
      mv "$BIN_BACKUP" "$BIN"; say "  - 已恢复旧二进制 $BIN"
    else
      rm -f "$BIN"; say "  - 已删除 $BIN"
    fi
  fi
  if [ -n "$BIN_BACKUP" ] && [ -f "$BIN_BACKUP" ]; then rm -f "$BIN_BACKUP"; fi
  if [ -n "$PLIST_BACKUP" ] && [ -f "$PLIST_BACKUP" ]; then remove_file "$PLIST_BACKUP"; fi
  if [ "$CONFIG_NEW" = "1" ] && [ -f "$CONFIG_PATH" ]; then
    rm -f "$CONFIG_PATH"; say "  - 已删除本次新建的 $CONFIG_PATH"
  fi
  if [ "$DATA_DIR_NEW" = "1" ] && [ -d "$DATA_DIR" ]; then
    rm -rf "$DATA_DIR"; say "  - 已删除本次新建的数据目录 $DATA_DIR"
  fi
  say "回滚完成：没有留下半截安装。"
}

# 走到这里已经有文件改动：任何退出（含 die/set -e）都经 EXIT trap 回滚，不留半截。
fail_with_rollback() {
  say "$1"
  exit 1
}

on_exit_install() {
  rc=$?
  rm -f "$index_tmp" "$bin_tmp" "$crt_tmp" || true
  if [ "$INSTALL_STARTED" = "1" ] && [ "$INSTALL_DONE" != "1" ]; then
    rollback || true
  fi
  exit "$rc"
}

# ---------------------------------------------------------------------------
#  安装 / 升级
# ---------------------------------------------------------------------------
do_install_or_upgrade() {
  check_arch
  check_curl
  check_identity
  check_conflicts_install

  if [ "$MODE" = "user" ]; then
    warn "当前是 --user 模式：用户级 LaunchAgent 只在有人登录后才运行；无头/不登录的机器请用默认 --system 模式"
  else
    say "system 模式：写 $SYSTEM_DAEMON_DIR 与 bootstrap 系统域时会调 sudo；开机即起、不需要登录"
  fi

  UPGRADING=0
  if [ "$MODE" = "system" ]; then
    if own_system_installed; then UPGRADING=1; fi
  else
    if own_user_installed; then UPGRADING=1; fi
  fi
  if [ "$UPGRADING" = "1" ] && [ "$ACTION" = "install" ]; then
    say "检测到已有 $MODE 模式独立部署（${PLIST}）⇒ 走升级流程（不重装）"
  fi
  if [ "$UPGRADING" = "0" ] && [ "$ACTION" = "upgrade" ]; then
    die "--upgrade 但没检测到 $MODE 模式独立部署（$PLIST 不存在）；首次安装请直接运行不带 --upgrade 的本脚本"
  fi

  index_tmp=$(mktemp -t zv-index.XXXXXX)
  bin_tmp=$(mktemp -t zv-bin.XXXXXX)
  crt_tmp=$(mktemp -t zv-crt.XXXXXX)
  trap 'rm -f "$index_tmp" "$bin_tmp" "$crt_tmp"' EXIT
  fetch_index "$index_tmp"
  resolve_release "$index_tmp"
  rm -f "$index_tmp"
  say "索引 latest=${LATEST}，arm64 产物=${ASSET}，size=$ASSET_SIZE"

  check_disk "$ASSET_SIZE"

  EFFECTIVE_LISTEN=$(effective_listen)
  port=$(port_of "$EFFECTIVE_LISTEN")
  if port_in_use "$port"; then
    if [ "$UPGRADING" = "1" ]; then
      say "端口 $port 被占用，按本作业重启处理（将在 bootout 后释放）"
    else
      die "端口 $port 已被别的进程占用（lsof -nP -iTCP:$port -sTCP:LISTEN）；先用 --listen 换端口，或先停掉占用者"
    fi
  fi

  if [ "$UPGRADING" = "1" ]; then
    cur=$(installed_version)
    if [ "$cur" = "$LATEST" ]; then
      say "已是最新（$cur = 索引 latest），未做任何改动。"
      trap - EXIT; rm -f "$bin_tmp" "$crt_tmp"
      exit 0
    fi
    say "已装版本 $cur ≠ 索引 latest $LATEST ⇒ 替换并重启"
  fi

  if [ "$DRY_RUN" = "1" ]; then
    say "[dry-run] 模式：${MODE}（$DOMAIN 域，plist=${PLIST}）"
    say "[dry-run] 会下载 $MIRROR/apps/$APP_ID/$ASSET_VER/$ASSET 并核 sha256 $ASSET_SHA"
    say "[dry-run] 会安装到 ${BIN}（0755）"
    say "[dry-run] 会写配置 ${CONFIG_PATH}（listen=${EFFECTIVE_LISTEN}）"
    if [ "$MODE" = "system" ]; then
      say "[dry-run] 会 sudo 写 $PLIST 并 sudo launchctl bootstrap system（UserName=${TARGET_USER}）"
    else
      say "[dry-run] 会写 $PLIST 并 launchctl bootstrap ${DOMAIN}（只在有人登录后运行）"
    fi
    say "[dry-run] 日志：$LOG_DIR/zizvideo.out.log / zizvideo.err.log；数据：$DATA_DIR"
    say "[dry-run] 会尝试取同目录 ${CRT_NAME}：有则导入系统钥匙串并复核签名（${SIGN_IDENTIFIER}），没有则降级为未签名部署"
    trap - EXIT; rm -f "$bin_tmp" "$crt_tmp"
    exit 0
  fi

  download_verify "$bin_tmp"
  # 证书和产物同目录；没有（老镜像/离线）不算失败，走未签名模式（见 report_signature）。
  if fetch_cert "$crt_tmp"; then HAVE_CRT=1; fi

  # 从这里开始会动文件：任何失败都回滚（见 fail_with_rollback 与 EXIT trap）。
  INSTALL_STARTED=1
  trap on_exit_install EXIT

  mkdir -p "$INSTALL_ROOT/bin"
  if [ -x "$BIN" ]; then
    BIN_BACKUP="$BIN.zv-backup.$$"
    cp -p "$BIN" "$BIN_BACKUP"
  fi
  mv "$bin_tmp" "$BIN"
  chmod 0755 "$BIN"
  BIN_NEW=1
  say "已安装二进制：$BIN"

  if [ ! -d "$DATA_DIR" ]; then DATA_DIR_NEW=1; fi
  ensure_config
  mkdir -p "$LOG_DIR"
  if [ "$HAVE_CRT" = "1" ]; then cp -f "$crt_tmp" "$DATA_DIR/$CRT_NAME"; fi

  if [ -f "$PLIST" ]; then
    PLIST_BACKUP="$PLIST.zv-backup.$$"
    if [ "$MODE" = "system" ]; then
      run_priv cp -p "$PLIST" "$PLIST_BACKUP" >/dev/null 2>&1 || PLIST_BACKUP=""
    else
      cp -p "$PLIST" "$PLIST_BACKUP" 2>/dev/null || PLIST_BACKUP=""
    fi
  else
    PLIST_NEW=1
  fi
  write_plist

  if ! start_service; then
    fail_with_rollback "验收第 2 步失败：bootstrap 后 $DOMAIN/$LABEL 仍未进入 running（看 $LOG_DIR/zizvideo.err.log 与 launchctl print $DOMAIN/${LABEL}）"
  fi

  if ! verify_acceptance; then
    fail_with_rollback "安装验收未通过（上面写明是第几步、为什么）"
  fi

  # 签名复核只是告警口径：复核不过时服务照常可用，但外置盘授权每次升级要重授。
  report_signature || true

  INSTALL_DONE=1
  trap - EXIT
  rm -f "$bin_tmp" "$crt_tmp"
  if [ -n "$BIN_BACKUP" ] && [ -f "$BIN_BACKUP" ]; then rm -f "$BIN_BACKUP"; fi
  if [ -n "$PLIST_BACKUP" ] && [ -f "$PLIST_BACKUP" ]; then remove_file "$PLIST_BACKUP"; fi
  say ""
  say "安装完成（$MODE 模式）：$BIN"
  case ":$PATH:" in
    *":$INSTALL_ROOT/bin:"*) ;;
    *) say "提示：把 $INSTALL_ROOT/bin 加进 PATH：echo 'export PATH=\"$INSTALL_ROOT/bin:\$PATH\"' >> ~/.zshrc" ;;
  esac
  say "打开 http://$(host_of "$EFFECTIVE_LISTEN"):$(port_of "$EFFECTIVE_LISTEN")/ 首次访问是初始化页"
  say "日志：$LOG_DIR/zizvideo.out.log / zizvideo.err.log"
  if [ "$MODE" = "user" ]; then
    warn "提醒：--user 模式的作业只在有人登录后运行；无头机器请改用默认 --system 模式重装"
  fi
}

# ---------------------------------------------------------------------------
#  卸载 / 清除
# ---------------------------------------------------------------------------
data_dir_from_config() {
  if [ -f "$CONFIG_PATH" ]; then
    v=$(sed -n 's/.*"data_dir"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONFIG_PATH" | head -n1)
    if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  fi
  printf '%s' "$DATA_DIR"
}

print_will_delete() {
  say "将删除（$MODE 模式）："
  if [ -f "$PLIST" ]; then
    if [ "$MODE" = "system" ]; then
      say "  - LaunchDaemon 定义 ${PLIST}（并 sudo launchctl bootout system/${LABEL}）"
    else
      say "  - LaunchAgent 定义 ${PLIST}（并 bootout $DOMAIN/${LABEL}）"
    fi
  fi
  if [ -f "$BIN" ]; then say "  - 二进制 $BIN"; fi
  if [ "$1" = "purge" ]; then
    say "  - 数据目录 $(data_dir_from_config)（含数据库、封面、配置）—— 不可恢复"
  else
    say "数据目录默认保留：$(data_dir_from_config)"
  fi
}

confirm_purge() {
  if [ "${ZV_ASSUME_YES:-}" = "1" ]; then return 0; fi
  # /dev/tty 存在不代表能打开（无控制终端时报 Device not configured），必须真开一次。
  if ! { true >/dev/tty; } 2>/dev/null; then
    die "--purge 需要二次确认，但当前没有终端；非交互请显式设 ZV_ASSUME_YES=1（表示你确认删数据）"
  fi
  printf '这会删除数据目录 %s 且不可恢复。输入 yes 确认：' "$(data_dir_from_config)" >/dev/tty
  read -r ans </dev/tty || true
  if [ "$ans" != "yes" ]; then die "未输入 yes，已取消，什么都没删"; fi
}

do_uninstall() {
  check_arch
  check_identity
  if panel_managed; then
    say "检测到面板托管的 zizvideo（系统域 ${PANEL_LABEL}）。本脚本只管独立部署，绝不碰面板托管的东西。"
    say "请在面板「应用市场 → zizvideo」里卸载。"
    exit 0
  fi
  installed=0
  if [ "$MODE" = "system" ]; then
    if own_system_installed; then installed=1; fi
  else
    if own_user_installed; then installed=1; fi
  fi
  if [ "$installed" = "0" ] && [ ! -f "$BIN" ]; then
    if [ "$1" != "purge" ]; then
      say "$MODE 模式独立部署未安装（$PLIST 与 $BIN 都不存在），无需卸载。"
      exit 0
    fi
    say "独立部署已不在，但 --purge 仍会清理数据目录。"
  fi
  if [ "$MODE" = "system" ] && own_user_installed; then
    warn "检测到 --user 模式也装过（${OTHER_PLIST}）；本脚本只卸 system，另一个请用 --user --uninstall"
  fi
  if [ "$MODE" = "user" ] && own_system_installed; then
    warn "检测到 --system 模式也装过（${OTHER_PLIST}）；本脚本只卸 user，另一个请用 --system --uninstall"
  fi
  print_will_delete "$1"
  if [ "$DRY_RUN" = "1" ]; then say "[dry-run] 以上为预演，未做任何改动。"; exit 0; fi
  if [ "$1" = "purge" ]; then confirm_purge; fi
  if [ -f "$PLIST" ] || [ "$installed" = "1" ]; then
    bootout_service
    pkill -f "$BIN" >/dev/null 2>&1 || true
    i=0
    while [ $i -lt 40 ] && lc print "$DOMAIN/$LABEL" >/dev/null 2>&1; do sleep 0.25; i=$((i + 1)); done
    remove_file "$PLIST"
    say "已 bootout 并删除 $PLIST"
  fi
  if [ -f "$BIN" ]; then rm -f "$BIN"; say "已删除 $BIN"; fi
  if [ "$1" = "purge" ]; then
    target=$(data_dir_from_config)
    rm -rf "$target"
    say "已删除数据目录 $target"
  else
    say "数据目录已保留：$(data_dir_from_config)"
  fi
  say "卸载完成。"
}

main() {
  parse_args "$@"
  if [ "${ZV_FAKE_SUDO:-}" = "1" ]; then FAKE_SUDO=1; fi
  derive_paths
  case "$ACTION" in
    install|upgrade) do_install_or_upgrade ;;
    uninstall) do_uninstall keep-data ;;
    purge) do_uninstall purge ;;
    *) die "未知动作：$ACTION" ;;
  esac
}

main "$@"
