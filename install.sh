#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 一键安装脚本（macOS）：curl -fsSL <镜像>/install.sh | sudo bash
#  环境变量见下方 ZP_*/ZIZPANEL_*；镜像优先，探不通回落 GitHub；幂等、可离线（dist/zizpanel）。
# =============================================================================
set -uo pipefail

SCRIPT_VERSION="1.5.10"

# ----------------------------------------------------------------- 基础变量 --
ZIZPANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
ZIZPANEL_LISTEN="${ZIZPANEL_LISTEN:-}"
# ZIZPANEL_LISTEN_EXPLICIT=1：端口是"人给的"（ZP_PORT/--port），只影响提示语。
ZIZPANEL_LISTEN_EXPLICIT=""
if [ -z "$ZIZPANEL_LISTEN" ] && [ -n "${ZP_PORT:-}" ]; then
  ZIZPANEL_LISTEN=":$ZP_PORT"
  ZIZPANEL_LISTEN_EXPLICIT=1
fi
ZIZPANEL_LISTEN="${ZIZPANEL_LISTEN:-:8443}"
# 本机访问路径：新面板会接管旧面板，并在该路径提供入口
PANEL_PATH="${ZIZPANEL_PANEL_PATH:-/_panel}"
ZIZPANEL_VERSION="${ZIZPANEL_VERSION:-latest}"
# 下载源：镜像优先，官方源只兜底；候选顺序见 detect_source。
ZIZPANEL_DOWNLOAD_BASE="${ZIZPANEL_DOWNLOAD_BASE:-}"
# 内置国内镜像（自建源/NAS 公网入口）。大陆无代理时 GitHub Release 完全不通（2026-09 实测）。
BUILTIN_MIRROR="${ZIZPANEL_BUILTIN_MIRROR:-https://zizdog.com/zizpanel}"
# 官方源（GitHub Releases）：**只做最后兜底**，国内直连通常会卡。
GITHUB_RELEASE_BASE="${ZIZPANEL_GITHUB_BASE:-https://github.com/zizdog/zizpanel/releases}"
# 服务器模式：--server-mode 或 ZIZPANEL_SERVER_MODE=1。
ZIZPANEL_SERVER_MODE="${ZIZPANEL_SERVER_MODE:-0}"
# --with-lnmp：顺带装 nginx/PHP/MySQL。默认关（国内镜像要装十几到几十分钟，像卡死）。
ZIZPANEL_WITH_LNMP="${ZIZPANEL_WITH_LNMP:-0}"

# ------------------------------------------------------------ 交互输入变量 --
ADMIN_USERNAME="${ZIZPANEL_ADMIN_USER:-${ZP_USER:-}}"
ADMIN_PASSWORD="${ZIZPANEL_ADMIN_PASSWORD:-${ZP_PASS:-}}"
PANEL_SUFFIX_INPUT="${ZIZPANEL_PANEL_SUFFIX:-${ZP_SUFFIX:-}}"
# ZP_YES / ZIZPANEL_YES：全程不提问
ZIZPANEL_YES="${ZIZPANEL_YES:-${ZP_YES:-0}}"
# ZP_SSH：1 开 / 0 关；空表示"按场景决定"（本机安装才问）
ZIZPANEL_SSH="${ZIZPANEL_SSH:-${ZP_SSH:-}}"
# ZP_LAN_PREAUTH：装完是否开启「免授权访问内网段」；空表示交互询问
ZIZPANEL_LAN_PREAUTH="${ZIZPANEL_LAN_PREAUTH:-${ZP_LAN_PREAUTH:-}}"
LAN_CIDR_INPUT="${ZIZPANEL_LAN_CIDR:-${ZP_LAN_CIDR:-}}"
# ZIZPANEL_DRY_RUN=1：只演练，不做任何修改（用于验证交互与默认值，不装任何东西）
ZIZPANEL_DRY_RUN="${ZIZPANEL_DRY_RUN:-0}"
# 安装器不再生成口令（账号交给面板初始化向导）；PANEL_UPGRADE_SOURCE 为空 = 不写探不通的地址。
PANEL_UPGRADE_SOURCE=""

# ---------------------------------------------------------- 基础依赖（ffmpeg）--

# ffmpeg 是基础环境，装面板时就装上（2026-09-16 事故：缺它时 TTS 返回 200 + 0 字节）。
# 清单与 Go 侧 internal/services/basedep.go 的 baseDependencies 对应。
BASEDEP_FORMULAS=(ffmpeg)
# ============================================================================
#  镜像地址集中定义在这一处，散落的域名不许出现；路径约定与 Go 侧 mirror.go /
#  homebrew_clt_mirror.go 严格一致：
#    zizpanel/download/<版本>/ 面板包 · zizpanel/clt/ CLT · brew/api|v2/ brew · apps/<app>/ 应用包 · pypi/ hf/
# ============================================================================

# DEFAULT_MIRROR_BASE：自建 NAS 镜像默认基址（与面板 config.DefaultMirrorBase 一致）。
DEFAULT_MIRROR_BASE="${ZIZPANEL_MIRROR_BASE_DEFAULT:-https://mirror.zizdog.com:8888}"
# 面板在线升级源（写进 config.json 的 upgrade_source）：**只有公网**。
# 判据是"清单 + 签名都在"；实测 2026-09-17 公网 manifest.json(+.sig) 200。
PANEL_UPGRADE_SOURCE_PUBLIC="${ZIZPANEL_UPGRADE_SOURCE:-https://zizdog.com/zizpanel}"
# 升级源是否可用以"清单 + 签名都在"为准（只有清单没有签名，面板会拒绝升级）。
UPGRADE_MANIFEST_PATH="/manifest.json"

# 国内公共 Homebrew 镜像（Go 侧 brewMirrorCandidates 同一份顺序与地址）。
BREW_PUBLIC_MIRRORS=(
  "中科大|https://mirrors.ustc.edu.cn/homebrew-bottles"
  "清华大学|https://mirrors.tuna.tsinghua.edu.cn/homebrew-bottles"
  "阿里云|https://mirrors.aliyun.com/homebrew/homebrew-bottles"
)
# Homebrew 安装脚本镜像：NAS 上没有（实测 404），走中科大或 GitHub raw；brew.git 同处定义。
BREW_INSTALL_SCRIPT_MIRROR="https://mirrors.ustc.edu.cn/misc/brew-install.sh"
BREW_GIT_REMOTE_MIRROR="https://mirrors.ustc.edu.cn/brew.git"
BREW_CORE_GIT_REMOTE_MIRROR="https://mirrors.tuna.tsinghua.edu.cn/git/homebrew/homebrew-core.git"
# CLT（Command Line Tools）清单在镜像上的相对路径（cltMirrorSubdirs 同一约定）。
CLT_MIRROR_SUBDIRS=("" "/zizpanel")
# 用户自己设过的 brew 镜像必须尊重：必须在 setup_homebrew 之前抓一份（见 choose_brew_mirror）。
ENV_HOMEBREW_API_DOMAIN="${HOMEBREW_API_DOMAIN:-}"
ENV_HOMEBREW_BOTTLE_DOMAIN="${HOMEBREW_BOTTLE_DOMAIN:-}"
PANEL_LABEL="cn.zizpanel.panel"
HELPER_NAME="zizpanel-helper"
BIN_DIR="$ZIZPANEL_ROOT/bin"
DATA_DIR="$ZIZPANEL_ROOT/data"
LOG_DIR="$ZIZPANEL_ROOT/logs"
RUN_DIR="$ZIZPANEL_ROOT/run"
WORK_DIR="$ZIZPANEL_ROOT/work"
# 系统路径允许覆盖：自动化测试指向沙箱目录（含 nginx 前缀与 vhost，避免改写生产配置）。
NGINX_PREFIX="${ZIZPANEL_NGINX_PREFIX:-}"
VHOST_DIR_OVERRIDE="${ZIZPANEL_VHOST_DIR:-}"
NGINX_ETC_OVERRIDE="${ZIZPANEL_NGINX_ETC:-}"
PLIST_DIR="${ZIZPANEL_PLIST_DIR:-/Library/LaunchDaemons}"
SUDOERS_DIR="${ZIZPANEL_SUDOERS_DIR:-/etc/sudoers.d}"
APPS_DIR="${ZIZPANEL_APPS_DIR:-/Applications}"
LINK_DIR="${ZIZPANEL_LINK_DIR:-/usr/local/bin}"
PLIST_PATH="$PLIST_DIR/$PANEL_LABEL.plist"
SUDOERS_PATH="$SUDOERS_DIR/zizpanel"
# PANEL_PORT 在 main 里由 panel_port() 填好；这里的空值只是给 shellcheck 的声明。
PANEL_PORT=""
TMP_DIR=""
# 外接盘授权状态：MODE = local/remote/""；ACK=1 表示用户当面同意申请一次授权。
ZP_EXTERNAL_MODE=""
ZP_EXTERNAL_ACK=0
# ZP_EXTERNAL_WANT：用户是否说"会用到外接硬盘"（1/0）。不用的人全程不碰外接卷、不弹任何窗。
ZP_EXTERNAL_WANT=0
# 脚本所在目录（用于定位随包分发的工具脚本）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "")"

# 安装来源（由 detect_source 决定）：local / download / build
SOURCE_KIND=""
SOURCE_BIN=""
SOURCE_HELPER=""

# -------------------------------------------------------------------- 输出 --
if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_BLUE=""; C_BOLD=""
fi
info()  { printf '%s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()    { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn()  { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
err()   { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
title() { printf '\n%s━━━ %s ━━━%s\n' "$C_BOLD" "$*" "$C_RESET"; }
die()   { err "$*"; exit 1; }

# dry_run：所有"会真的改系统"的动作都要先过这一关；实现只有这一处。
dry_run() { [ "$ZIZPANEL_DRY_RUN" = "1" ]; }

cleanup() {
  [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ] && rm -rf "$TMP_DIR"
  return 0
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------- 前置条件检查 --
require_root() {
  # 沙箱模式仅用于自动化测试；生产安装绝不走这个分支。
  if [ "${ZIZPANEL_SANDBOX:-0}" = "1" ]; then
    warn "沙箱模式：不写入系统目录，仅供测试"
    return 0
  fi
  if [ "$(id -u)" -ne 0 ]; then
    err "本脚本需要管理员权限。"
    err "请用：sudo bash $0"
    exit 1
  fi
}

check_macos() {
  if [ "$(uname -s)" != "Darwin" ]; then
    die "本面板仅支持 macOS（当前系统：$(uname -s)）"
  fi
  local ver
  ver="$(sw_vers -productVersion 2>/dev/null || echo 0)"
  local major="${ver%%.*}"
  if [ "$major" -lt 13 ] 2>/dev/null; then
    warn "系统版本为 macOS ${ver}，面板在 macOS 13+ 上测试最充分，仍会继续安装。"
  else
    ok "系统：macOS ${ver}（$(uname -m)）"
  fi
}

# 定位"真实用户"：sudo 下 $HOME 是 root 的，必须通过 SUDO_USER 找回
resolve_real_user() {
  REAL_USER="${SUDO_USER:-}"
  if [ -z "$REAL_USER" ]; then
    REAL_USER="$(id -un 2>/dev/null || echo "")"
  fi
  if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
    # 退回到最近登录的普通用户
    REAL_USER="$(stat -f '%Su' /dev/console 2>/dev/null || echo "")"
  fi
  if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
    # 用 glob 而不是 `ls | grep`：能正确处理含空格/特殊字符的用户目录名
    for d in /Users/*/; do
      candidate="$(basename "$d")"
      case "$candidate" in
        Shared|Guest|.*) continue ;;
      esac
      REAL_USER="$candidate"
      break
    done
  fi
  [ -n "$REAL_USER" ] || die "无法确定当前用户，请用 sudo -u <用户名> 重新执行"

  REAL_HOME="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "$REAL_HOME" ] || REAL_HOME="/Users/$REAL_USER"
  REAL_UID="$(id -u "$REAL_USER" 2>/dev/null || echo 501)"
  ok "安装用户：$REAL_USER (uid=$REAL_UID, home=$REAL_HOME)"
}

# ------------------------------------------------------------ 检测安装来源 --
detect_source() {
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "")"

  # 1) 本地目录里已有编译好的二进制（开发/离线安装）
  for cand in "$script_dir/dist" "$script_dir/../dist" "$script_dir"; do
    if [ -x "$cand/zizpanel" ]; then
      SOURCE_KIND="local"
      SOURCE_BIN="$cand/zizpanel"
      SOURCE_HELPER="$cand/$HELPER_NAME"
      ok "使用本地二进制：$SOURCE_BIN"
      return 0
    fi
  done

  # 2) 联网下载预编译包：探真实 tarball 地址（目录 HEAD 在 nginx 下常 403 会误判）。
  # 逐个候选源都试一遍；顺序 = 用户指定 → 公网镜像 → GitHub（只兜底）。
  local arch="arm64" base
  [ "$(uname -m)" = "x86_64" ] && arch="amd64"
  local file="zizpanel_${ZIZPANEL_VERSION}_darwin_${arch}.tar.gz"
  local -a bases=()
  if [ -n "$ZIZPANEL_DOWNLOAD_BASE" ]; then
    bases+=("${ZIZPANEL_DOWNLOAD_BASE%/}")
  fi
  bases+=("$BUILTIN_MIRROR" "$GITHUB_RELEASE_BASE")
  for base in "${bases[@]}"; do
    [ -n "$base" ] || continue
    base="${base%/}"
    if curl -fsSI --max-time 10 "$base/download/$ZIZPANEL_VERSION/$file" >/dev/null 2>&1; then
      if [ -n "$ZIZPANEL_DOWNLOAD_BASE" ] && [ "$base" != "${ZIZPANEL_DOWNLOAD_BASE%/}" ]; then
        warn "指定的下载源不可达，改用镜像：$base"
      fi
      ZIZPANEL_DOWNLOAD_BASE="$base"
      SOURCE_KIND="download"
      return 0
    fi
  done
  warn "下载源都不可达（自建镜像与官方源），尝试从源码构建"

  # 3) 用 Go 从源码构建
  if command -v go >/dev/null 2>&1; then
    SOURCE_KIND="build"
    return 0
  fi

  err "无法获取面板程序，请选择其一："
  err "  1) 把 dist/ 目录与 install.sh 放在一起（离线安装）"
  err "  2) 检查网络能否访问 ${bases[0]:-<镜像>}"
  err "  3) 安装 Go 后重试：brew install go"
  exit 1
}


# zp_curl_progress <输出文件> <URL> [总字节数]：带可见进度的下载。
# 自己轮询：curl --progress-bar 只在 stderr 是终端时才画，管道安装下用户看不到进度。
zp_curl_progress() {
  local out="$1" url="$2" total="${3:-0}" pid rc=0 sz mb
  /usr/bin/curl -fsL --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 300 \
    -o "$out" "$url" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    sz="$(/usr/bin/stat -f%z "$out" 2>/dev/null || echo 0)"
    mb="$(awk -v b="$sz" 'BEGIN{printf "%.1f", b/1048576}')"
    if [ "$total" -gt 0 ]; then
      printf '\r  ⬇ 已下载 %s / %s MB（%d%%）   ' "$mb" \
        "$(awk -v b="$total" 'BEGIN{printf "%.1f", b/1048576}')" "$(( sz * 100 / total ))"
    else
      printf '\r  ⬇ 已下载 %s MB   ' "$mb"
    fi
    sleep 1
  done
  wait "$pid" || rc=$?
  printf '\r\033[K'
  return "$rc"
}

download_binaries() {
  TMP_DIR="$(mktemp -d)"
  local arch="arm64"
  [ "$(uname -m)" = "x86_64" ] && arch="amd64"
  local file="zizpanel_${ZIZPANEL_VERSION}_darwin_${arch}.tar.gz"
  local url="$ZIZPANEL_DOWNLOAD_BASE/download/$ZIZPANEL_VERSION/$file"

  # 让用户知道自己**正在装什么版本**：默认 ZIZPANEL_VERSION=latest，真实版本在清单里。
  local shown_ver="$ZIZPANEL_VERSION"
  if [ "$shown_ver" = "latest" ]; then
    shown_ver="$(/usr/bin/curl -fsSL --max-time 15 "${ZIZPANEL_DOWNLOAD_BASE%/}/manifest.json" 2>/dev/null \
      | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)"
    [ -n "$shown_ver" ] || shown_ver="（清单未取到，下载后确认）"
  fi
  info "即将安装：ZizPanel ${shown_ver}（架构 ${arch}）"
  info "下载面板安装包（约 24 MB；国内镜像下通常 30–60 秒，下面有实时进度）"
  info "下载：$url"
  if ! zp_curl_progress "$TMP_DIR/pkg.tar.gz" "$url"; then
    # 自动换到下一个候选源（镜像优先，官方兜底）：用户不必知道镜像地址
    local -a fallbacks=("$BUILTIN_MIRROR" "$GITHUB_RELEASE_BASE")
    local fb
    local done_ok=0
    for fb in "${fallbacks[@]}"; do
      [ -n "$fb" ] || continue
      fb="${fb%/}"
      [ "$fb" = "$ZIZPANEL_DOWNLOAD_BASE" ] && continue
      warn "从 $ZIZPANEL_DOWNLOAD_BASE 下载失败，改用：$fb"
      url="$fb/download/$ZIZPANEL_VERSION/$file"
      info "下载：$url"
      if zp_curl_progress "$TMP_DIR/pkg.tar.gz" "$url"; then
        ZIZPANEL_DOWNLOAD_BASE="$fb"
        done_ok=1
        break
      fi
    done
    if [ "$done_ok" != "1" ]; then
      die "下载失败（自建镜像与官方源都不通）。可改用离线安装：把 dist/ 与 install.sh 放在同一目录"
    fi
  fi
  if ! tar -xzf "$TMP_DIR/pkg.tar.gz" -C "$TMP_DIR"; then
    die "安装包解压失败，文件可能不完整"
  fi
  # 把 SCRIPT_DIR 指向解压目录：`curl | bash` 时 BASH_SOURCE[0] 是 "bash" ⇒ 变成用户当前目录，
  # 那里没有 tools/ ⇒ 复制整段被跳过，面板装完「远程登录」找不到 server-mode.sh（真机实测）。
  info "下载完成，正在解压与校验…"
  if v="$("$TMP_DIR/zizpanel" version 2>/dev/null | head -1)"; then
    info "安装包版本确认：${v}"
  fi
  SCRIPT_DIR="$TMP_DIR"
  SOURCE_BIN="$TMP_DIR/zizpanel"
  SOURCE_HELPER="$TMP_DIR/$HELPER_NAME"
  [ -x "$SOURCE_BIN" ] || die "安装包中缺少 zizpanel 可执行文件（来源：${ZIZPANEL_DOWNLOAD_BASE}）"
  [ -f "$SOURCE_HELPER" ] || die "安装包中缺少 ${HELPER_NAME}（来源：${ZIZPANEL_DOWNLOAD_BASE}）"
  ok "下载完成"
}

build_from_source() {
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "$PWD")"
  # 源码根目录：脚本所在目录，或其上级（脚本在 build/ 下时）
  local src_root="$script_dir"
  [ -f "$script_dir/../go.mod" ] && src_root="$(cd "$script_dir/.." && pwd)"
  [ -f "$src_root/go.mod" ] || die "找不到源码（go.mod），无法从源码构建"

  TMP_DIR="$(mktemp -d)"
  info "从源码构建（首次需要下载依赖，可能需要几分钟）…"
  # 国内网络默认走 goproxy.cn，否则模块下载会失败
  (
    cd "$src_root" || exit 1
    export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
    export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
    export GOFLAGS=-mod=mod
    go build -trimpath -ldflags "-s -w" -o "$TMP_DIR/zizpanel" ./cmd/zizpanel &&
      go build -trimpath -ldflags "-s -w" -o "$TMP_DIR/$HELPER_NAME" ./cmd/zizpanel-helper
    # zizvideo 是可选的面板托管模块：它自己的 module，编不出来不影响面板本身。
    if [ -d "$src_root/zizvideo" ]; then
      ( cd "$src_root/zizvideo" && go build -trimpath -ldflags "-s -w" -o "$TMP_DIR/zizvideo" ./cmd/server ) \
        || warn "zizvideo 模块构建失败（可选模块；面板本身不受影响）"
    fi
  ) || die "构建失败，请检查 Go 环境"
  SOURCE_BIN="$TMP_DIR/zizpanel"
  SOURCE_HELPER="$TMP_DIR/$HELPER_NAME"
  [ -x "$SOURCE_BIN" ] || die "构建命令返回成功，但 $SOURCE_BIN 不存在 —— 如实报为失败"
  [ -x "$SOURCE_HELPER" ] || die "构建命令返回成功，但 $SOURCE_HELPER 不存在 —— 如实报为失败"
  ok "构建完成"
}

# ------------------------------------------------------------ brew 调用助手 --
# Homebrew 拒绝以 root 运行，所有 brew 查询必须降权到真实用户，否则误报"都未安装"。
has_brew() {
  # command 是 shell 内建，不能 `sudo -u user command -v`，直接判可执行文件。
  [ -x /opt/homebrew/bin/brew ] && return 0
  [ -x /usr/local/bin/brew ] && return 0
  command -v brew >/dev/null 2>&1
}

# brew_prefix 返回 Homebrew 前缀（root 下必须降权查询）
brew_prefix() {
  local p=""
  if [ -n "$REAL_USER" ]; then
    p="$(sudo -u "$REAL_USER" brew --prefix 2>/dev/null || echo)"
  fi
  [ -n "$p" ] || p="$(brew --prefix 2>/dev/null || echo)"
  [ -n "$p" ] || p="/opt/homebrew"
  echo "$p"
}

# brew_has <formula> 判断是否已安装
brew_has() {
  if [ -n "$REAL_USER" ]; then
    sudo -u "$REAL_USER" brew list --versions "$1" >/dev/null 2>&1 && return 0
  fi
  return 1
}

# ------------------------------------------------------- 安装 Homebrew --
# 镜像优先：先探国内镜像的安装脚本，不通才回落 GitHub；同时把 brew/core 与 bottles 指向镜像，
# 并写进用户 shell 配置（否则下次开终端又变回官方源）。镜像只替换下载地址，脚本内容与官方一致。
GITHUB_RAW_OK=""

# 探测 Homebrew 官方安装脚本（只作兜底候选：镜像偶尔同步滞后）。
probe_github_raw() {
  [ -n "$GITHUB_RAW_OK" ] && return 0
  if curl -fsSI --max-time 8 https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh >/dev/null 2>&1; then
    GITHUB_RAW_OK=1
  else
    GITHUB_RAW_OK=0
  fi
}

# write_brew_mirrors 把镜像配置写进用户 shell 配置（幂等，带标记块）
write_brew_mirrors() {
  local home="$1" rc="$1/.zprofile"
  [ -f "$rc" ] || rc="$home/.zshrc"
  if [ -f "$rc" ] && grep -q "ZizPanel brew mirror" "$rc" 2>/dev/null; then
    return 0
  fi
  {
    printf '\n# >>> ZizPanel brew mirror (自动生成，可整段删除) >>>\n'
    printf 'export HOMEBREW_API_DOMAIN="%s"\n' "$HOMEBREW_API_DOMAIN"
    printf 'export HOMEBREW_BOTTLE_DOMAIN="%s"\n' "$HOMEBREW_BOTTLE_DOMAIN"
    printf '# <<< ZizPanel brew mirror <<<\n'
  } >> "$rc" || return 1
  # 文件属主必须是真实用户，否则用户自己的 shell 读不了
  [ "$(id -u)" -eq 0 ] && chown "$REAL_USER" "$rc" 2>/dev/null
  return 0
}

setup_homebrew() {
  has_brew && return 0
  # Homebrew 明确拒绝以 root 运行，必须降权到真实用户
  if [ "$(id -u)" -eq 0 ] && [ -z "$REAL_USER" ]; then
    warn "无法确定真实用户，跳过 Homebrew 安装"
    return 1
  fi

  # 安装脚本镜像优先，USTC 与官方同步（实测 200/0.18s）；官方 raw.githubusercontent.com 兜底。
  local installer_url="$BREW_INSTALL_SCRIPT_MIRROR" installer_src="国内镜像 mirrors.ustc.edu.cn（与官方同步）"
  if ! curl -fsSI --max-time 8 "$installer_url" >/dev/null 2>&1; then
    probe_github_raw
    if [ "$GITHUB_RAW_OK" = "1" ]; then
      warn "Homebrew 安装脚本镜像不可达，回落 GitHub 官方源"
      installer_url="https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"
      installer_src="GitHub 官方源"
    else
      warn "Homebrew 安装脚本（镜像与官方源）都不可达，稍后可手工安装"
      return 1
    fi
  fi
  info "Homebrew 安装脚本来源：${installer_src}"

  # brew/core 与 bottles 都指向国内镜像；探 ffmpeg 而不是 git —— git formula 已不在 taps 清单，
  # 探它会得到"镜像不可用"的假结论。
  if choose_brew_mirror "ffmpeg"; then
    info "  brew 部件镜像：${BREW_MIRROR_NAME}"
  else
    # 镜像都没探通：此时**不要**设 HOMEBREW_*，让 brew 走官方源（比设一个取不到的好）
    warn "  自建镜像与国内镜像都没探测通：brew 走官方源（国内可能很慢）"
    HOMEBREW_API_DOMAIN=""; HOMEBREW_BOTTLE_DOMAIN=""
  fi
  export HOMEBREW_BREW_GIT_REMOTE="$BREW_GIT_REMOTE_MIRROR"
  # 注意：core 的仓库镜像用 TUNA —— USTC 的 homebrew-core.git 实测返回 404
  export HOMEBREW_CORE_GIT_REMOTE="$BREW_CORE_GIT_REMOTE_MIRROR"
  # 安装过程禁止交互与自动更新，否则会卡在"按回车继续"
  export NONINTERACTIVE=1
  export HOMEBREW_NO_AUTO_UPDATE=1
  export HOMEBREW_NO_INSTALL_CLEANUP=1

  info "正在安装 Homebrew（国内网络下可能需要几分钟）…"
  local script
  script="$(curl -fsSL --max-time 60 "$installer_url" 2>/dev/null || true)"
  if [ -z "$script" ]; then
    warn "无法下载 Homebrew 安装脚本（${installer_url}）"
    return 1
  fi

  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，写 /tmp 是故意的：日志留给用户排障。
  if sudo -u "$REAL_USER" -H /bin/bash -c "$script" >/tmp/zizpanel-brew-install.log 2>&1; then
    ok "Homebrew 安装完成"
  else
    warn "Homebrew 安装失败，日志末尾："
    tail -15 /tmp/zizpanel-brew-install.log 2>/dev/null | while IFS= read -r line; do
      printf '    %s\n' "$line"
    done
    return 1
  fi

  # 立即让当前进程也能看到 brew
  export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

  if [ -n "$HOMEBREW_API_DOMAIN" ]; then
    if write_brew_mirrors "$REAL_HOME"; then
      ok "已把国内镜像写入 $REAL_HOME 的 shell 配置（下次开终端仍然生效）"
    else
      warn "镜像配置写入失败，请手工设置 HOMEBREW_BOTTLE_DOMAIN"
    fi
  fi
  return 0
}

# install_lnmp 实际安装 nginx/PHP/MySQL 并注册为后台服务（仅在 --with-lnmp 时调用）。
# brew 必须降权到真实用户：`brew services start` 会把服务注册到该用户的 LaunchAgent 下。
install_lnmp() {
  local pkgs=("$@")
  [ ${#pkgs[@]} -gt 0 ] || return 0

  title "安装网站环境（nginx / PHP / MySQL）"
  warn "这一步需要下载较大的包，国内镜像下通常 10-40 分钟，请勿中断。"
  info "正在安装：${pkgs[*]}"

  local log="/tmp/zizpanel-lnmp-install.log"
  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，日志留给用户排障。
  if ! sudo -u "$REAL_USER" -H "$(brew_prefix)/bin/brew" install "${pkgs[@]}" \
       >"$log" 2>&1; then
    warn "部分组件安装失败，日志末尾："
    tail -20 "$log" 2>/dev/null | while IFS= read -r line; do
      printf '    %s\n' "$line"
    done
    warn "可稍后在面板「应用市场 → 网站环境」里重试（那里能看到完整输出）"
    return 1
  fi
  ok "已安装：${pkgs[*]}"

  # 注册为后台服务。失败不致命（面板仍能识别已装程序），给出提示而不中断安装。
  local svc pkg
  for pkg in "${pkgs[@]}"; do
    svc="$pkg"
    case "$pkg" in php@*) svc="php" ;; esac   # brew services 里 php@8.3 的服务名是 php
    if sudo -u "$REAL_USER" -H "$(brew_prefix)/bin/brew" services start "$svc" \
         >/dev/null 2>&1; then
      ok "已注册后台服务：${svc}（开机自启）"
    else
      warn "brew services start $svc 失败，可稍后在面板「服务管理」里启动"
    fi
  done

  # nginx 的 include 上下文与 WebSocket 映射由面板启动时补齐，这里只提示、不动它的配置。
  info "面板启动后会自动补齐 nginx 的 include 上下文与 WebSocket 升级映射"
  return 0
}

# ---------------------------------------------------- 基础依赖（ffmpeg）安装 --
# 2026-09-16 事故：缺 ffmpeg 时 mlx_audio 编码 mp3 失败，TTS 返回 HTTP 200 + 0 字节 body，
# 所有作业全败而健康检查全绿；它也是后续音视频功能的公共前提，所以按基础环境管。

# brew_api_ok <base> <formula>：4 秒内能拿到 formula 清单就算可用。
# 只探清单不探瓶路径是刻意的：各家瓶布局不同，brew 瓶取不到会自行回落官方域。
brew_api_ok() {
  [ -n "${1:-}" ] || return 1
  curl -fsS --max-time 4 -o /dev/null \
    "${1%/}/api/formula/${2:-ffmpeg}.json" 2>/dev/null
}

# choose_brew_mirror [formula]：选镜像，结果写进 HOMEBREW_API_DOMAIN / HOMEBREW_BOTTLE_DOMAIN。
# 优先级与 Go 侧 probeBrewMirrors 一致：NAS <base>/brew → 中科大 → 清华 → 阿里云；
# 都不通就不设，让 brew 回落官方源。返回 0=选中镜像，1=都没通。
choose_brew_mirror() {
  local formula="${1:-ffmpeg}" nas="" entry name base
  BREW_MIRROR_NAME=""

  # 用户显式设过就一律以用户为准（与面板 Go 侧 brewEnv 的 pick() 语义一致）
  if [ -n "$ENV_HOMEBREW_API_DOMAIN" ] || [ -n "$ENV_HOMEBREW_BOTTLE_DOMAIN" ]; then
    BREW_MIRROR_NAME="环境变量已指定（API=${ENV_HOMEBREW_API_DOMAIN:-未设}，BOTTLE=${ENV_HOMEBREW_BOTTLE_DOMAIN:-未设}）"
    HOMEBREW_API_DOMAIN="$ENV_HOMEBREW_API_DOMAIN"
    HOMEBREW_BOTTLE_DOMAIN="$ENV_HOMEBREW_BOTTLE_DOMAIN"
    export HOMEBREW_API_DOMAIN HOMEBREW_BOTTLE_DOMAIN
    return 0
  fi

  # 镜像候选：用户配置 / 内置公网默认；探通才用。
  local nas_candidates=()
  nas="${ZIZPANEL_MIRROR_BASE:-}"
  if [ -z "$nas" ] && [ -f "$DATA_DIR/config.json" ]; then
    # 沿用面板里配置过的镜像；只做最小键值提取，不引入 python/jq。
    nas="$(sed -n 's/.*"mirror_base"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      "$DATA_DIR/config.json" 2>/dev/null | head -1)"
  fi
  nas_candidates+=("${nas:-$DEFAULT_MIRROR_BASE}")
  for nas in "${nas_candidates[@]}"; do
    [ -n "$nas" ] || continue
    if brew_api_ok "$nas/brew" "$formula"; then
      HOMEBREW_API_DOMAIN="${nas%/}/brew/api"
      HOMEBREW_BOTTLE_DOMAIN="${nas%/}/brew"
      export HOMEBREW_API_DOMAIN HOMEBREW_BOTTLE_DOMAIN
      BREW_MIRROR_NAME="自建镜像（${HOMEBREW_BOTTLE_DOMAIN}）"
      return 0
    fi
  done

  # 国内公共镜像：地址与顺序集中定义在文件头 BREW_PUBLIC_MIRRORS（与 Go 侧严格一致）。
  for entry in "${BREW_PUBLIC_MIRRORS[@]}"; do
    name="${entry%%|*}"
    base="${entry##*|}"
    if brew_api_ok "$base" "$formula"; then
      HOMEBREW_API_DOMAIN="$base/api"
      HOMEBREW_BOTTLE_DOMAIN="$base"
      export HOMEBREW_API_DOMAIN HOMEBREW_BOTTLE_DOMAIN
      BREW_MIRROR_NAME="国内镜像 ${name}（${base}）"
      return 0
    fi
  done
  return 1
}

# ---------------------------------------------------- CLT（命令行开发者工具）--
# install.sh 不自己装 CLT（由 Homebrew 安装器负责），只做两件事：判断镜像上有没有 CLT 清单并如实报出；
# 把 <mirror>/zizpanel 作为 ZIZPANEL_CLT_MIRROR 写进 plist（Go 侧 cltMirrorBase() 读它）。
CLT_MIRROR_BASE=""

probe_clt_mirror() {
  local base="$1" sub url
  for sub in "${CLT_MIRROR_SUBDIRS[@]}"; do
    url="${base%/}${sub}/clt/index.json"
    if curl -fsSI --max-time 6 "$url" >/dev/null 2>&1; then
      CLT_MIRROR_BASE="${base%/}${sub}"
      return 0
    fi
  done
  return 1
}

# clt_mirror_note：只读探测 + 如实汇报（探不到就说探不到，不假装有镜像）。
clt_mirror_note() {
  local base="${ZIZPANEL_MIRROR_BASE:-$DEFAULT_MIRROR_BASE}"
  if probe_clt_mirror "$base"; then
    ok "CLT 镜像可用：${CLT_MIRROR_BASE}/clt/index.json（面板补装 CLT 时会用它）"
  else
    warn "镜像上没有 CLT 清单：CLT 由 Homebrew 安装器负责（走 softwareupdate 或弹窗），"
    warn "  镜像路径：${base%/}/zizpanel/clt/index.json"
  fi
}

# ------------------------------------------------------------ 在线升级源 --
# 只有公网 zizdog.com 一个候选（可由 ZIZPANEL_UPGRADE_SOURCE 覆盖）。
# 判据是"清单 + 签名都在"，缺签名面板会拒绝升级。
probe_upgrade_source() {
  # 只有一个公网候选，不需要循环（写成 `for x in "$VAR"` 只会跑一次，shellcheck 会报 SC2066）。
  local cand="${PANEL_UPGRADE_SOURCE_PUBLIC%/}"
  if [ -n "$cand" ] &&
     curl -fsSI --max-time 8 "${cand}${UPGRADE_MANIFEST_PATH}" >/dev/null 2>&1 &&
     curl -fsSI --max-time 8 "${cand}${UPGRADE_MANIFEST_PATH}.sig" >/dev/null 2>&1; then
    PANEL_UPGRADE_SOURCE="$cand"
    return 0
  fi
  PANEL_UPGRADE_SOURCE=""
  return 1
}

# upgrade_source_note：只读探测并如实汇报（探不到就说探不到，界面里再让用户填）。
upgrade_source_note() {
  if probe_upgrade_source; then
    ok "在线升级源：${PANEL_UPGRADE_SOURCE}（公网 zizdog.com，已确认清单与签名都在）"
  else
    warn "公网升级源没探通（面板仍可用，升级源留空，可在「面板设置 → 在线升级」里手工填写）"
  fi
}

# ensure_base_deps：幂等装上缺失的基础依赖（目前只有 ffmpeg）。
# 已装就说"跳过"；失败必须报出来并给出可照抄的命令（ffmpeg 缺失不会自己报错）。
ensure_base_deps() {
  [ ${#BASEDEP_FORMULAS[@]} -gt 0 ] || return 0

  local missing=() f
  for f in "${BASEDEP_FORMULAS[@]}"; do
    brew_has "$f" || missing+=("$f")
  done
  if [ ${#missing[@]} -eq 0 ]; then
    ok "基础依赖已安装，跳过：${BASEDEP_FORMULAS[*]}（TTS 编码 mp3 与后续音视频功能需要它）"
    return 0
  fi

  warn "缺少基础依赖：${missing[*]}"
  info "  它为什么算基础依赖：TTS 用 mlx_audio 编码 mp3 必须靠 ffmpeg；"
  info "  没有它时合成接口会返回 HTTP 200 但 body 是 0 字节（用户只看到作业全败），"
  info "  后续的音视频功能也要用它。"

  local envs=(HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1)
  if choose_brew_mirror "${missing[0]}"; then
    info "  镜像：${BREW_MIRROR_NAME}"
    [ -n "${HOMEBREW_API_DOMAIN:-}" ] && envs+=("HOMEBREW_API_DOMAIN=$HOMEBREW_API_DOMAIN")
    [ -n "${HOMEBREW_BOTTLE_DOMAIN:-}" ] && envs+=("HOMEBREW_BOTTLE_DOMAIN=$HOMEBREW_BOTTLE_DOMAIN")
  else
    warn "  自建镜像与国内镜像都没探测通：让 Homebrew 走官方源（国内可能很慢）"
  fi

  local log="/tmp/zizpanel-basedep-install.log"
  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，日志留给用户排障。
  if ! sudo -u "$REAL_USER" -H /usr/bin/env "${envs[@]}" \
      "$(brew_prefix)/bin/brew" install "${missing[@]}" >"$log" 2>&1; then
    warn "基础依赖安装失败，日志末尾："
    tail -20 "$log" 2>/dev/null | while IFS= read -r line; do
      printf '    %s\n' "$line"
    done
    warn "缺 ffmpeg 会让 TTS 合成 mp3 返回 200 + 空 body（用户只看到作业全败）。"
    warn "可稍后在面板「应用市场 → FFmpeg（音视频工具）」重装，或手工执行："
    warn "  brew install ${missing[*]}"
    return 1
  fi

  # 不看退出码就下结论：brew 返回 0 也要再确认包真的在里面
  for f in "${missing[@]}"; do
    if ! brew_has "$f"; then
      warn "brew install ${f} 报告完成，但 brew list 里仍看不到它 —— 如实报为失败"
      return 1
    fi
  done
  ok "基础依赖已安装：${missing[*]}"
  return 0
}

# ------------------------------------------------------------ 安装依赖组件 --
install_deps() {
  title "检查系统依赖"
  if dry_run; then
    info "（干跑）将检查并（按需）安装：Homebrew / ffmpeg，以及可选的 nginx+PHP+MySQL"
    info "（干跑）Homebrew 安装脚本来源：${BREW_INSTALL_SCRIPT_MIRROR}（镜像优先，不通才回落官方）"
    return 0
  fi

  if ! has_brew; then
    info "未检测到 Homebrew。面板本身不依赖它，但网站管理（nginx/PHP/MySQL）需要。"
    # CLT 是 Homebrew 前置，也是整条链最大的一笔下载：先探镜像并如实报出结论。
    clt_mirror_note
    # 🛑 2026-09-17 用户要求：默认不在安装期装 Homebrew/CLT（官方路径会弹 CLT 的 GUI 对话框卡住安装）。
    # 同一件事面板「基础环境」已有完整实现，唯一一份实现留在面板里。
    if [ "${ZIZPANEL_INSTALL_BREW:-0}" = "1" ]; then
      setup_homebrew || warn "Homebrew 未能自动安装，可稍后在面板「基础环境」里重试"
    else
      info "按默认策略跳过（不在安装期装 Homebrew/CLT）："
      info "  面板装好后进「基础环境」→ 一键安装（走国内镜像，含 CLT；在任务中心里能看到进度）"
      info "  想在本步就装：ZIZPANEL_INSTALL_BREW=1 重跑（会走镜像，但 CLT 仍可能触发系统弹窗）"
    fi
  fi

  if ! has_brew; then
    info "面板已可以正常使用（面板本身不依赖 Homebrew）。"
    info "「网站管理 / 数据库 / 一键 LNMP」需要 nginx、PHP、MySQL —— 在面板「基础环境」里一键装。"
    return 0
  fi
  ok "Homebrew：$(brew_prefix)"

  if [ "${ZIZPANEL_SKIP_DEPS:-0}" = "1" ]; then
    warn "已跳过依赖安装（ZIZPANEL_SKIP_DEPS=1）"
    return 0
  fi

  # ---- 基础依赖（ffmpeg）：面板一装好就有，放在 LNMP 之前 ----
  # 失败不中断安装，但必须如实提示（缺它时 TTS 返回 200 + 空 body，用户查不出来）。
  if ! ensure_base_deps; then
    warn "基础依赖没装好：面板可用，但 TTS 编码 mp3 与音视频功能会失败（原因见上）。"
  fi

  # 汇总缺失的关键组件，一次性提示，避免反复打断
  local missing=()
  local pkg
  for pkg in nginx php@8.3 mysql@8.4; do
    brew_has "$pkg" || missing+=("$pkg")
  done

  if [ ${#missing[@]} -eq 0 ]; then
    ok "nginx / PHP / MySQL 均已安装"
  elif [ "$ZIZPANEL_WITH_LNMP" = "1" ]; then
    install_lnmp "${missing[@]}"
  else
    # 面板「应用市场 → 网站环境」确实有 nginx/php/mysql 条目，可以放心指过去；
    # 国内装这三个包很久，默认不自动装。
    warn "缺少：${missing[*]}"
    info "面板本身已可用；但「网站管理」「数据库」这两块需要它们。"
    warn "本次不自动安装（国内网络下这几个包要装很久，放在安装脚本里会卡住不动）。"
    info "推荐：装完面板后到「应用市场 → 网站环境」点安装（有实时进度、失败可重试）。"
    info "或者重跑安装脚本并加上 --with-lnmp 一次性装好。"
    info "需要时手工执行："
    info "  brew install ${missing[*]}"
    info "  brew services start $(printf '%s ' "${missing[@]}" | sed 's/php@[0-9.]*/php/')"
  fi

  # mkcert 用于生成浏览器信任的本地证书（可选，有了就没有证书警告）
  if ! command -v mkcert >/dev/null 2>&1; then
    info "可选：安装 mkcert 可让浏览器不再提示证书不受信任"
    info "  稍后可在面板中一键安装，或手动执行：brew install mkcert nss"
  fi
}

# ---------------------------------------------------------------- 安装文件 --
install_binaries() {
  title "安装面板程序"

  # 绝不允许空源路径走到 install(1)（只会得到 `install: : No such file or directory` 掩盖真因）。
  if [ -z "$SOURCE_BIN" ] || [ ! -x "$SOURCE_BIN" ]; then
    err "没有可安装的面板程序（内部错误：来源为空或不可执行）。"
    err "已尝试的来源："
    err "  · 本地二进制：${SCRIPT_DIR}/dist、${SCRIPT_DIR}/../dist、${SCRIPT_DIR}"
    err "  · 下载：${ZIZPANEL_DOWNLOAD_BASE:-<未选定>} → $BUILTIN_MIRROR → $GITHUB_RELEASE_BASE"
    err "  · 源码构建：需要本机有 go"
    die "无法获取面板程序，安装中止（没有改动任何东西）"
  fi

  if dry_run; then info "（干跑）将安装主程序与提权助手到 ${BIN_DIR}、建立 ${LINK_DIR}/zizpanel 软链"; return 0; fi
  local was_installed=0
  [ -f "$DATA_DIR/config.json" ] && was_installed=1

  mkdir -p "$BIN_DIR" "$DATA_DIR" "$LOG_DIR" "$RUN_DIR" \
           "$WORK_DIR/compose" "$WORK_DIR/services" "$WORK_DIR/backup" "$DATA_DIR/tls"

  # 升级前备份，出问题可回滚
  if [ -x "$BIN_DIR/zizpanel" ]; then
    cp "$BIN_DIR/zizpanel" "$BIN_DIR/zizpanel.bak" 2>/dev/null || true
    info "已备份旧版本到 $BIN_DIR/zizpanel.bak"
  fi

  install -m 0755 "$SOURCE_BIN" "$BIN_DIR/zizpanel" || die "安装主程序失败"
  # zizvideo（可选的面板托管模块）：发布包把它放在面板二进制旁边，面板安装模块时从这里取。
  if [ -x "$(dirname "$SOURCE_BIN")/zizvideo" ]; then
    install -m 0755 "$(dirname "$SOURCE_BIN")/zizvideo" "$BIN_DIR/zizvideo" || die "安装 zizvideo 模块失败"
  fi
  if [ -n "$SOURCE_HELPER" ] && [ -f "$SOURCE_HELPER" ]; then
    install -m 0755 "$SOURCE_HELPER" "$BIN_DIR/$HELPER_NAME" || die "安装提权助手失败"
  elif [ -x "$BIN_DIR/$HELPER_NAME" ]; then
    warn "未提供新的提权助手，沿用已有版本"
  else
    die "缺少 ${HELPER_NAME}，安装包不完整"
  fi

  # 把安装脚本与工具脚本复制到安装目录，便于用户直接查看/重跑
  if [ -n "$SCRIPT_DIR" ] && [ -d "$SCRIPT_DIR/tools" ]; then
    mkdir -p "$ZIZPANEL_ROOT/tools"
    cp -f "$SCRIPT_DIR/tools/takeover-panel-entry.sh" "$ZIZPANEL_ROOT/" 2>/dev/null || true
    cp -f "$SCRIPT_DIR/tools/panel-entry.awk" "$ZIZPANEL_ROOT/tools/" 2>/dev/null || true
    cp -f "$SCRIPT_DIR/tools/check-shell-vars.py" "$ZIZPANEL_ROOT/tools/" 2>/dev/null || true
    cp -f "$SCRIPT_DIR/tools/server-mode.sh" "$ZIZPANEL_ROOT/" 2>/dev/null || true
    # 一键 LNMP 要用它把服务注册成系统级 LaunchDaemon（面板运行时会调用）
    cp -f "$SCRIPT_DIR/tools/system-services.sh" "$ZIZPANEL_ROOT/" 2>/dev/null || true
    chmod 0755 "$ZIZPANEL_ROOT/server-mode.sh" 2>/dev/null || true
    chmod 0755 "$ZIZPANEL_ROOT/system-services.sh" 2>/dev/null || true
    chmod 0755 "$ZIZPANEL_ROOT/takeover-panel-entry.sh" 2>/dev/null || true
    ok "入口接管工具已安装到 $ZIZPANEL_ROOT"
  fi

  # 控制命令放到 PATH 目录，方便直接敲 zizpanel
  mkdir -p "$LINK_DIR"
  ln -sf "$BIN_DIR/zizpanel" "$LINK_DIR/zizpanel"
  ln -sf "$BIN_DIR/$HELPER_NAME" "$LINK_DIR/$HELPER_NAME"

  # 目录归属交给真实用户，避免 root 独占。
  if [ "$(id -u)" -eq 0 ]; then
    chown -R "${REAL_USER}:staff" "$ZIZPANEL_ROOT" 2>/dev/null || true
  fi
  chmod 0755 "$BIN_DIR/zizpanel" "$BIN_DIR/$HELPER_NAME"
  chmod 0700 "$DATA_DIR" "$DATA_DIR/tls" 2>/dev/null || true

  # 面板运行时文件（日志、TLS 私钥）必须能被实际运行身份打开。
  # 真机事故：归属漂移会让面板读到证书那一刻直接退出，日志为空、launchd 只报 exit code 78。
  # 收尾一次做对：日志目录可写 + 证书可读、私钥 0600。
  if [ "$(id -u)" -eq 0 ]; then
    for d in "$LOG_DIR" "$RUN_DIR"; do
      [ -d "$d" ] || continue
      chown -R "${REAL_USER}:staff" "$d" 2>/dev/null || true
      find "$d" -type f -exec chmod 0644 {} + 2>/dev/null || true
    done
    for f in "$DATA_DIR/tls/panel.crt" "$DATA_DIR/tls/panel.key"; do
      [ -f "$f" ] || continue
      chown "${REAL_USER}:staff" "$f" 2>/dev/null || true
    done
    [ -f "$DATA_DIR/tls/panel.key" ] && chmod 0600 "$DATA_DIR/tls/panel.key" 2>/dev/null || true
    [ -f "$DATA_DIR/tls/panel.crt" ] && chmod 0644 "$DATA_DIR/tls/panel.crt" 2>/dev/null || true
  fi

  ok "程序已安装到 $BIN_DIR"
  [ "$was_installed" = "1" ] && ok "检测到已有安装，数据与配置已保留（升级模式）"
  return 0
}

# ------------------------------------------------------- LaunchDaemon 服务 --

# 面板监听的端口号（从 ":8443" / "127.0.0.1:8443" 里取出来）
panel_port() {
  printf '%s' "${ZIZPANEL_LISTEN##*:}"
}

# wait_service_stopped：等 launchd 真正摘干净旧服务。
# `launchctl bootout` 是异步的，立刻 bootstrap 会 failed: 5，面板彻底不在（本机真实发生过）。
wait_service_stopped() {
  local waited=0
  while [ "$waited" -lt 60 ]; do
    if ! launchctl print "system/$PANEL_LABEL" >/dev/null 2>&1; then
      break
    fi
    sleep 0.25
    waited=$((waited + 1))
  done
  [ "$waited" -ge 60 ] && warn "等待旧服务卸载超时（15 秒），继续尝试启动"

  # 服务从 launchd 消失 ≠ 进程退出，端口可能还被占着，新进程 bind 失败进失败循环。
  local port; port="$(panel_port)"
  if [ -n "$port" ] && command -v lsof >/dev/null 2>&1; then
    waited=0
    while [ "$waited" -lt 40 ]; do
      if ! lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
        break
      fi
      sleep 0.25
      waited=$((waited + 1))
    done
    [ "$waited" -ge 40 ] && warn "端口 $port 仍被占用，新实例可能启动失败"
  fi
  return 0
}

# start_service —— 带重试地启动服务；bootstrap 与 kickstart 互为兜底。
start_service() {
  local attempt=0
  while [ "$attempt" -lt 6 ]; do
    if launchctl bootstrap system "$PLIST_PATH" >/dev/null 2>&1; then
      return 0
    fi
    # bootstrap 失败有两种可能：已经加载了（那就重启），或正在卸载（那就等一等重试）
    if launchctl print "system/$PANEL_LABEL" >/dev/null 2>&1; then
      launchctl kickstart -k "system/$PANEL_LABEL" >/dev/null 2>&1 && return 0
    fi
    attempt=$((attempt + 1))
    sleep 0.5
  done
  return 1
}

install_daemon() {
  title "注册后台服务（开机自启）"

  if dry_run; then info "（干跑）将写 ${PLIST_PATH} 并 bootstrap 服务 ${PANEL_LABEL}"; return 0; fi
  # 先停掉旧的，避免升级时二进制被占用
  launchctl bootout "system/$PANEL_LABEL" 2>/dev/null || true

  mkdir -p "$PLIST_DIR"

  # 同样写临时文件再替换（旧 plist 属 root:wheel，直接覆盖会失败）
  local tmp_plist="$PLIST_PATH.tmp.$$"
  cat > "$tmp_plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$PANEL_LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN_DIR/zizpanel</string>
        <string>serve</string>
        <string>--config</string>
        <string>$DATA_DIR/config.json</string>
        <string>--listen</string>
        <string>$ZIZPANEL_LISTEN</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <!-- 面板是运维入口：崩溃必须自动拉起，否则用户将失去唯一的远程管理手段 -->
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>WorkingDirectory</key>
    <string>$ZIZPANEL_ROOT</string>
    <key>StandardOutPath</key>
    <string>$LOG_DIR/launchd.out.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/launchd.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$(brew_prefix)/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>ZIZPANEL_ROOT</key>
        <string>$ZIZPANEL_ROOT</string>
        <!-- 面板以 root 运行时，家目录环境变量指向 /var/root，必须显式告知真实用户，
             否则会把网站根目录算成 /var/root/www，导致站点全部找不到 -->
        <key>ZIZPANEL_USER</key>
        <string>$REAL_USER</string>
        <!-- CLT（命令行开发者工具）镜像基址：面板 Go 侧 cltMirrorBase() 读它，
             这样面板补装 CLT 时走与安装脚本同一个镜像，不必再对 404 路径探一遍。
             探不到时留空（不写这一项），面板自己会回落到内置静态源。 -->
__PLIST_CLT_ENV__
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
</dict>
</plist>
PLIST
  # CLT 镜像项按"探到才写"处理：探不到就整项删掉（用字符串替换，不用 sed -i）。
  if [ -n "$CLT_MIRROR_BASE" ]; then
    local tmp_clt="$tmp_plist.clt"
    {
      printf '        <key>ZIZPANEL_CLT_MIRROR</key>\n'
      printf '        <string>%s</string>\n' "$CLT_MIRROR_BASE"
    } > "$tmp_clt"
    # awk 读进文件再替换：避免把满是斜杠的地址塞进 sed 表达式
    awk -v repl="$tmp_clt" '
      /__PLIST_CLT_ENV__/ { while ((getline l < repl) > 0) print l; next }
      { print }
    ' "$tmp_plist" > "$tmp_plist.new" && mv -f "$tmp_plist.new" "$tmp_plist"
    rm -f "$tmp_clt"
    info "已把 CLT 镜像写入 plist 环境变量：$CLT_MIRROR_BASE"
  else
    grep -v '__PLIST_CLT_ENV__' "$tmp_plist" > "$tmp_plist.new" 2>/dev/null \
      && mv -f "$tmp_plist.new" "$tmp_plist"
  fi

  chmod 0644 "$tmp_plist"
  if [ "$(id -u)" -eq 0 ]; then
    chown root:wheel "$tmp_plist"
  fi
  mv -f "$tmp_plist" "$PLIST_PATH"

  wait_service_stopped

  if ! start_service; then
    # 兜底回滚：升级失败宁可用回旧版本继续运行，也不能让用户没有面板入口。
    if [ -x "$BIN_DIR/zizpanel.bak" ]; then
      warn "新版服务启动失败，正在回滚到升级前的版本…"
      if cp -f "$BIN_DIR/zizpanel.bak" "$BIN_DIR/zizpanel" && chmod 0755 "$BIN_DIR/zizpanel"; then
        if start_service; then
          err "新版本无法启动，已回滚到旧版本（面板仍可用，但未升级）"
          err "请把 $LOG_DIR/launchd.err.log 的内容反馈给开发者"
          exit 1
        fi
      fi
    fi
    die "启动面板服务失败。请检查：$LOG_DIR/launchd.err.log"
  fi

  # 等端口真正可用，而不是只等进程存在
  local waited=0
  while [ "$waited" -lt 20 ]; do
    if launchctl print "system/$PANEL_LABEL" 2>/dev/null | grep -q "state = running"; then
      break
    fi
    sleep 0.5
    waited=$((waited + 1))
  done

  ok "服务已注册：${PANEL_LABEL}（开机自启 + 崩溃自动重启）"
}

# --------------------------------------------------------- sudoers 提权授权 --
install_sudoers() {
  title "配置面板提权权限"

  if dry_run; then info "（干跑）将写入 ${SUDOERS_PATH}（只授权受限提权助手）"; return 0; fi
  # 只授权 helper 这一个程序（内部只做白名单操作），绝不授权 /bin/bash 之类通用命令。
  # 必须写临时文件再 mv：目标 0440 只读，直接重定向重装时会 Permission denied。
  local tmp_sudo="$SUDOERS_PATH.tmp.$$"
  cat > "$tmp_sudo" <<SUDOERS
# ZizPanel 面板提权授权（由 install.sh 生成，请勿手工编辑）
$REAL_USER ALL=(root) NOPASSWD: $BIN_DIR/$HELPER_NAME
SUDOERS

  chmod 0440 "$tmp_sudo"
  if [ "$(id -u)" -eq 0 ]; then
    chown root:wheel "$tmp_sudo"
  fi
  mv -f "$tmp_sudo" "$SUDOERS_PATH"

  # 语法检查：写坏 sudoers 会导致所有 sudo 失效，必须验证
  if ! visudo -c -f "$SUDOERS_PATH" >/dev/null 2>&1; then
    rm -f "$SUDOERS_PATH"
    rm -f "$SUDOERS_PATH".tmp.* 2>/dev/null || true
    die "sudoers 配置语法检查失败，已撤销（不会影响系统 sudo）"
  fi
  ok "已授权：$REAL_USER 可免密调用受限提权助手"
}

# ------------------------------------------------------- nginx 环境准备 --
# 反代依赖 http 上下文的 WebSocket map；这里确保 conf.d/upgrade-map.conf 存在且 nginx.conf
# include 了 conf.d/*.conf —— 否则用户一建反代站点，nginx -t 就报未知变量。
setup_nginx_env() {
  title "准备 nginx 环境"

  if dry_run; then info "（干跑）将补齐 nginx 的 conf.d include 与 WebSocket 升级 map"; return 0; fi
  if [ ! -x "$BIN_DIR/$HELPER_NAME" ]; then
    warn "提权助手不可用，跳过 nginx 环境准备"
    return 0
  fi
  if [ ! -f "$(brew_prefix)/etc/nginx/nginx.conf" ]; then
    info "未检测到 nginx 配置，跳过（装好 nginx 后可在面板中一键修复）"
    return 0
  fi

  local out
  out="$("$BIN_DIR/$HELPER_NAME" nginx-ensure-env 2>&1 || true)"
  if echo "$out" | grep -q '"ok":true'; then
    ok "nginx 环境已就绪（WebSocket 升级支持 + conf.d 加载）"
  else
    warn "nginx 环境准备未完成：$out"
    warn "可在面板「网站管理」页点击「修复 nginx 环境」重试"
  fi
}

# ------------------------------------------------------------ 本地证书处理 --
# CERT_STATE: none（无 mkcert，靠面板自签）/ mkcert-untrusted / mkcert-trusted
CERT_STATE="none"

setup_cert() {
  if dry_run; then info "（干跑）将生成/复用 TLS 证书：$DATA_DIR/tls/panel.crt"; return 0; fi
  title "准备 HTTPS 证书"

  local mkcert_bin=""
  local hosts=""
  local ip=""
  for p in "$(brew_prefix)/bin/mkcert" /usr/local/bin/mkcert; do
    [ -x "$p" ] && mkcert_bin="$p" && break
  done

  if [ -z "$mkcert_bin" ]; then
    info "未安装 mkcert，将使用自签证书（浏览器首次访问会提示不受信任）"
    info "消除提示：brew install mkcert nss && sudo -u $REAL_USER mkcert -install"
    return 0
  fi

  info "检测到 mkcert，生成本机受信证书"

  # mkcert -install 在 SSH/非交互会话里会一直阻塞（等 GUI 授权、不超时），
  # 必须加超时，失败就回退自签证书。
  local trust_ok=0
  local waiting=0
  (
    sudo -u "$REAL_USER" "$mkcert_bin" -install >/dev/null 2>&1
    echo $? > "$DATA_DIR/.mkcert-install-rc"
  ) &
  local trust_pid=$!
  while kill -0 "$trust_pid" 2>/dev/null; do
    if [ "$waiting" -ge 20 ]; then
      warn "mkcert -install 超过 20 秒未返回（非图形会话下会等待系统授权）"
      pkill -P "$trust_pid" 2>/dev/null || true
      kill "$trust_pid" 2>/dev/null || true
      break
    fi
    sleep 1
    waiting=$((waiting + 1))
  done
  wait "$trust_pid" 2>/dev/null || true
  if [ -f "$DATA_DIR/.mkcert-install-rc" ] && [ "$(cat "$DATA_DIR/.mkcert-install-rc" 2>/dev/null)" = "0" ]; then
    trust_ok=1
  fi
  rm -f "$DATA_DIR/.mkcert-install-rc" 2>/dev/null || true

  # CA 未写进信任库时 mkcert 仍能签发；浏览器会提示一次不受信任，可手工信任 rootCA.pem。
  if [ "$trust_ok" = "1" ]; then
    CERT_STATE="mkcert-trusted"
    info "mkcert CA 已写入系统信任库"
  else
    CERT_STATE="mkcert-untrusted"
    warn "未能把 mkcert CA 写入系统信任库（需要图形界面下的一次系统授权）"
    info "  之后在「终端」执行一次 mkcert -install 即可消除浏览器证书提示"
  fi

  if true; then
    hosts="localhost 127.0.0.1 ::1"
    # 把本机所有 IP 与主机名都纳入证书，换网络后依然有效
    for ip in $(ifconfig 2>/dev/null | awk '/inet /{print $2}' | grep -v '^127\.'); do
      hosts="$hosts $ip"
    done
    hosts="$hosts $(hostname)"
    # shellcheck disable=SC2086
    if sudo -u "$REAL_USER" "$mkcert_bin" -cert-file "$DATA_DIR/tls/panel.crt" \
        -key-file "$DATA_DIR/tls/panel.key" $hosts >/dev/null 2>&1; then
      if [ "$(id -u)" -eq 0 ]; then
        chown "${REAL_USER}:staff" "$DATA_DIR/tls/panel.crt" "$DATA_DIR/tls/panel.key" 2>/dev/null || true
      fi
      if [ "$CERT_STATE" = "mkcert-trusted" ]; then
        ok "已生成本机受信证书（浏览器不会再有证书警告）"
      else
        ok "已用 mkcert 签发证书（CA 尚未写入系统信任库）"
      fi
      return 0
    fi
    warn "mkcert 签发失败，回退到自签证书"
  fi
  return 0
}

# ---------------------------------------------------------------- 防火墙 --
setup_firewall() {
  [ "${ZIZPANEL_SKIP_FIREWALL:-0}" = "1" ] && return 0
  title "处理系统防火墙"

  if dry_run; then info "（干跑）将把面板加入防火墙允许列表（若防火墙开启）"; return 0; fi
  local fw_state
  fw_state="$(/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate 2>/dev/null || echo "")"
  if echo "$fw_state" | grep -q "disabled"; then
    ok "防火墙未开启，远程访问无需额外配置"
    return 0
  fi

  info "防火墙已开启，正在把面板加入允许列表…"
  if "$BIN_DIR/$HELPER_NAME" firewall-ensure-app \
      --app "$BIN_DIR/zizpanel" --name "ZizPanel" --apps-dir "$APPS_DIR" >/dev/null 2>&1; then
    ok "已把面板加入防火墙允许列表"
  else
    warn "自动配置失败。若远程无法访问，请到："
    warn "  系统设置 → 网络 → 防火墙 → 选项 → 允许「ZizPanel」传入连接"
  fi
}

# --------------------------------------------------------------- 安装校验 --
# 用真实 HTTP 请求确认面板在服务，而不是只相信 launchctl 报 running。
verify_running() {
  title "校验面板是否可访问"
  if dry_run; then info "（干跑）将轮询 https://127.0.0.1:${PANEL_PORT}/api/v1/health 直到 200"; return 0; fi
  local port="${ZIZPANEL_LISTEN##*:}"
  local url="https://127.0.0.1:${port}/api/v1/health"
  # 诊断日志；所有 local 声明必须在循环外（循环内重复声明会清空值，触发 set -u）。
  local diag="$LOG_DIR/install-check.log"
  local waited=0
  local st="" code="000" holder=""
  # 先确保日志目录存在再写：升级路径下 logs/ 可能还不存在，否则每次 >> 都刷一行
  # No such file or directory 把真正的诊断埋掉。
  mkdir -p "$LOG_DIR" 2>/dev/null || true
  : > "$diag" 2>/dev/null || true
  # 目录都建不出来就把诊断改成"不落盘、只打印"，绝不再反复重试同一条失败命令。
  local diag_writable=0
  [ -w "$diag" ] && diag_writable=1
  # diag_note <文本>：写诊断日志（不可写时静默跳过，不刷屏）
  diag_note() {
    [ "$diag_writable" = "1" ] && printf '%s\n' "$1" >> "$diag"
    return 0
  }

  while [ "$waited" -lt 30 ]; do
    # 必须显式传 --config：status 的默认路径依赖环境变量，不传会误报"未初始化"。
    st="$(ZIZPANEL_ROOT="$ZIZPANEL_ROOT" "$BIN_DIR/zizpanel" status \
          --config "$DATA_DIR/config.json" 2>/dev/null | grep '状态' || echo '状态    : 未知')"
    if echo "$st" | grep -q '运行中'; then
      code="$(curl -fsSk --max-time 3 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || echo 000)"
      if [ "$code" = "200" ]; then
        ok "面板已就绪：$url"
        rm -f "$diag" 2>/dev/null || true
        return 0
      fi
      diag_note "[${waited}s] 进程运行中，HTTP 状态码=$code"
    else
      diag_note "[${waited}s] 进程未运行（${st}）"
      # 未运行时记录端口占用，便于判断是"没起来"还是"端口被占"
      holder="$(/usr/sbin/lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $1" pid "$2}' | tr '\n' ' ')"
      [ -n "$holder" ] && diag_note "[${waited}s] 端口 $port 被占用：$holder"
    fi
    sleep 1
    waited=$((waited + 1))
  done

  err "面板未能在 30 秒内就绪。诊断信息："
  err "  1) 端口 $port 是否被占用：lsof -nP -iTCP:$port -sTCP:LISTEN"
  err "  2) 服务日志：$LOG_DIR/launchd.err.log"
  err "  3) 面板日志：$LOG_DIR/panel-$(date +%Y%m%d).log"
  err "  4) 探活过程：$diag"
  err ""
  if [ -f "$diag" ]; then
    err "探活记录（最后 6 条）："
    tail -6 "$diag" | while IFS= read -r line; do err "    $line"; done
  fi
  err ""
  err "排查后可手动重启：sudo launchctl kickstart -k system/$PANEL_LABEL"
  exit 1
}

# ------------------------------------------------------------------ 收尾 --
finish() {
  title "安装完成"

  local ip
  ip="$(ifconfig 2>/dev/null | awk '/inet /{print $2}' | grep -v '^127\.' | head -1)"
  [ -z "$ip" ] && ip="127.0.0.1"

  local scheme="https"
  local port="${ZIZPANEL_LISTEN##*:}"
  # 后缀以面板配置为准（升级时用户填的后缀不会覆盖原有值）
  local suffix
  suffix="$(zcfg_get panel_suffix)"
  [ -n "$suffix" ] || suffix="$PANEL_SUFFIX_INPUT"

  printf '\n'
  printf '  %s🦊 ZizPanel 已就绪%s\n' "$C_BOLD" "$C_RESET"
  if v="$("$BIN_DIR/zizpanel" version 2>/dev/null | head -1)"; then
    printf '  面板版本   %s%s%s\n' "$C_BOLD" "$v" "$C_RESET"
  fi
  printf '\n'
  if [ -n "$suffix" ]; then
    printf '  远程访问   %s%s://%s:%s/%s/%s\n' "$C_GREEN" "$scheme" "$ip" "$port" "$suffix" "$C_RESET"
    printf '  本机访问   %s%s://127.0.0.1:%s/%s/%s\n' "$C_BLUE" "$scheme" "$port" "$suffix" "$C_RESET"
  else
    printf '  远程访问   %s%s://%s:%s%s\n' "$C_GREEN" "$scheme" "$ip" "$port" "$C_RESET"
    printf '  本机访问   %s%s://127.0.0.1:%s%s\n' "$C_BLUE" "$scheme" "$port" "$C_RESET"
  fi
  printf '\n'
  if [ -z "$suffix" ]; then
    printf '  安全加强   未启用（面板路径就是 /）—— 想加强安全可在面板「系统设置」里开启路径后缀。\n'
  fi
  if [ -n "$ADMIN_USERNAME" ] && [ -n "$suffix" ]; then
    printf '  管理员账号 %s%s%s（来自 ZP_USER/ZP_PASS，安装器不保存口令）\n' \
      "$C_BOLD" "$ADMIN_USERNAME" "$C_RESET"
    printf '  安全后缀   %s%s%s（忘了就执行 zizpanel status）\n' "$C_BOLD" "$suffix" "$C_RESET"
  else
    printf '  首次打开会进入初始化向导，请在那里设置管理员用户名与口令。\n'
  fi
  if [ "$CERT_STATE" = "mkcert-trusted" ]; then
    printf '  %s证书已受系统信任，浏览器不会提示。%s\n' "$C_GREEN" "$C_RESET"
  elif [ "$CERT_STATE" = "mkcert-untrusted" ]; then
    printf '  当前使用 mkcert 证书，但 CA 尚未写入系统信任库，浏览器首次会提示一次。\n'
    printf '  消除提示（只需一次）：在「终端」运行 %smkcert -install%s，再执行\n' "$C_BOLD" "$C_RESET"
    printf '    %ssudo launchctl kickstart -k system/%s%s\n' "$C_BOLD" "$PANEL_LABEL" "$C_RESET"
  else
    printf '  当前使用自签证书，浏览器首次会提示不受信任，点"继续访问"即可。\n'
    printf '  消除提示：%sbrew install mkcert nss && mkcert -install%s 后重启面板。\n' "$C_BOLD" "$C_RESET"
  fi
  printf '\n'
  printf '  常用命令：\n'
  printf '    zizpanel status                     查看状态与访问地址\n'
  printf '    sudo zizpanel reset-password <用户>  忘记密码时重置\n'
  printf '    sudo launchctl kickstart -k system/%s   重启面板\n' "$PANEL_LABEL"
  printf '    sudo %s/uninstall.sh                卸载（交互菜单选 1/2/3 三档，逐行显示进度）\n' "$ZIZPANEL_ROOT"
  printf '    sudo %s/uninstall.sh --dry-run 3    预演模式 3（不删任何东西）\n' "$ZIZPANEL_ROOT"
  printf '\n'
  # 只有显式要求（ZP_LAN_PREAUTH=1）才写过预授权；外接硬盘按用户自己的选择如实汇报。
  if [ "${ZP_EXTERNAL_WANT:-0}" != "1" ]; then
    printf '  外接硬盘   你说不用 —— 安装过程没有碰任何外接卷，也没有触发任何系统授权弹窗。\n'
    printf '             以后想用：插上盘，到「面板 → 磁盘」按页面指引授权一次即可。\n'
    printf '\n'
  elif [ "${ZP_EXTERNAL_MODE:-}" = "local" ] && [ "${ZP_EXTERNAL_ACK:-0}" = "1" ]; then
    printf '  外接硬盘   已按你的确认申请一次授权：若屏幕弹过请确认点了「允许」。\n'
    printf '             没点的话可在「磁盘管理 → 申请授权」重来一次；升级面板不用再授权。\n'
    printf '\n'
  elif [ "${ZP_EXTERNAL_MODE:-}" = "local" ]; then
    printf '  外接硬盘   你这次选择不申请授权 —— 安装过程没有触发任何系统授权弹窗。\n'
    printf '             想用的时候到「面板 → 磁盘管理 → 申请授权」按一下（要在真机屏幕前）。\n'
    printf '\n'
  elif [ "${ZP_EXTERNAL_MODE:-}" = "remote" ]; then
    printf '  %s外接硬盘   你说会用到，但本次远程安装没有（也不会）触发任何授权弹窗。%s\n' "$C_YELLOW" "$C_RESET"
    printf '             请到真机上授权一次：系统设置 → 隐私与安全性 → 完全磁盘访问权限\n'
    printf '             → 点「+」选中 %s/bin/zizpanel → 打开开关。\n' "$ZIZPANEL_ROOT"
    printf '             面板已用固定证书签名 —— 授权一次后升级不用再授。\n'
    printf '\n'
  fi
  if [ "${LAN_PREAUTH_APPLIED:-0}" = "1" ]; then
    printf '  %s⚠ 你要求写入的「免授权访问内网段」已写入，但要重启电脑才生效。%s\n' "$C_BOLD" "$C_RESET"
    printf '     重启前一切照旧（面板的 Plan B 回环转发器已经在工作），想生效就找时间重启一次。\n'
    printf '\n'
  fi
}

# ------------------------------------------- 接管旧面板（部署替换） --
# 新面板在原来习惯的路径（默认 http://localhost/_panel）直接可用；旧面板目录改名保留，
# 升级第一原则是可回滚（旧面板只有几百 KB）。
takeover_legacy_panel() {
  title "接管面板入口"
  if dry_run; then info "（干跑）将接管 nginx 入口 ${PANEL_PATH} 指向面板端口"; return 0; fi

  local port="${ZIZPANEL_LISTEN##*:}"
  local vhost_dir
  vhost_dir="${VHOST_DIR_OVERRIDE:-$(brew_prefix)/etc/nginx/vhosts}"
  local panel_dir="$REAL_HOME/www/_panel"
  local legacy_bak="$REAL_HOME/www/_panel.legacy-bak"

  local nginx_conf
  if [ -n "$NGINX_ETC_OVERRIDE" ]; then
    nginx_conf="$NGINX_ETC_OVERRIDE/nginx.conf"
  else
    nginx_conf="$(brew_prefix)/etc/nginx/nginx.conf"
  fi
  if [ ! -f "$nginx_conf" ]; then
    info "未检测到 nginx 配置，跳过入口接管（面板仍可用 https://<本机IP>:${port}）"
    return 0
  fi

  # 1) 把旧面板目录改名保留（只做一次）：升级出问题时可回滚。
  if [ -d "$panel_dir" ] && [ -f "$panel_dir/index.php" ] && [ ! -d "$legacy_bak" ]; then
    if mv "$panel_dir" "$legacy_bak" 2>/dev/null; then
      ok "旧面板已备份为 _panel.legacy-bak（回滚：改回 _panel 并还原 000-default.conf）"
    else
      warn "旧面板目录移动失败，保持原样（新入口仍会生效）"
    fi
  fi

  # 2) 在默认站点里替换入口 location 块（改写逻辑在 tools/takeover-panel-entry.sh）。
  local tool="$ZIZPANEL_ROOT/takeover-panel-entry.sh"
  if [ ! -x "$tool" ]; then
    # 从源码目录运行时（make install）回退到相对路径
    local src_tool="$SCRIPT_DIR/tools/takeover-panel-entry.sh"
    if [ -x "$src_tool" ]; then
      tool="$src_tool"
    else
      warn "找不到入口接管工具，跳过（面板仍可用 https://<本机IP>:${port}）"
      return 0
    fi
  fi

  local out
  out="$(bash "$tool" --port "$port" --path "$PANEL_PATH" --vhost-dir "$vhost_dir" 2>&1 || true)"
  echo "$out" | while IFS= read -r line; do
    case "$line" in
      *"[完成]"*) printf '%s\n' "$line" ;;
      *"[警告]"*|*"[错误]"*) printf '%s\n' "$line" >&2 ;;
    esac
  done

  # 3) 校验并重载；失败就还原备份，绝不把 nginx 弄挂
  local default_conf="$vhost_dir/000-default.conf"
  local nginx_bin
  if [ -n "$NGINX_PREFIX" ]; then
    nginx_bin="$NGINX_PREFIX/bin/nginx"
  else
    nginx_bin="$(brew_prefix)/bin/nginx"
  fi
  local test_out
  test_out="$("$nginx_bin" -c "$nginx_conf" -t 2>&1 || true)"
  if echo "$test_out" | grep -qE 'syntax is ok|test is successful'; then
    # nginx -t 通过不能证明入口接管成功：vhost 目录不存在、接管被跳过时它照样通过。
    # 判定标准必须是"配置里真的写进了指向面板端口的反代"。
    if ! grep -q "127.0.0.1:${port}" "$default_conf" 2>/dev/null; then
      warn "nginx 可用，但面板入口【尚未接管】：未找到可写的 vhost 配置"
      warn "  （通常是 nginx 的 vhosts 目录还没建出来）"
      warn "  面板本身完全可用：https://<本机IP>:${port}"
      warn "  装好 nginx 并生成 vhosts 目录后，重跑安装脚本即可接管 ${PANEL_PATH}"
      return 0
    fi
    "$nginx_bin" -c "$nginx_conf" -s reload >/dev/null 2>&1 || true
    ok "入口已生效：http://localhost${PANEL_PATH}"
  else
    if [ -f "$default_conf.zizpanel.bak" ]; then
      cp -f "$default_conf.zizpanel.bak" "$default_conf"
    fi
    "$nginx_bin" -c "$nginx_conf" -s reload >/dev/null 2>&1 || true
    warn "入口配置校验失败，已还原（其它站点不受影响）：$test_out"
  fi
}

# --------------------------------------------------------------- 卸载脚本 --
write_uninstaller() {
  if dry_run; then
    info "（干跑）将生成卸载脚本 $ZIZPANEL_ROOT/uninstall.sh"
    return 0
  fi

  # 三档卸载脚本：与仓库根 uninstall.sh **逐字节一致**（单一真源）。
  # 改 uninstall.sh 后必须重跑：bash tools/sync-embedded-uninstaller.sh
  # 门禁：bash tools/sync-embedded-uninstaller.sh --check（漂移即红）。
# >>> ZP_EMBEDDED_UNINSTALLER_BEGIN
  cat > "$ZIZPANEL_ROOT/uninstall.sh" <<'ZP_UNINSTALL_EOF'
#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 卸载脚本（三档 · 实时进度）
#
#  用法：
#    curl -fsSL https://zizdog.com/zizpanel/uninstall.sh | sudo bash
#    sudo bash uninstall.sh [1|2|3] [--yes] [--dry-run] [--purge-backups]
#    sudo bash uninstall.sh --mode 2 --dry-run
#
#  三档（交互菜单里选）：
#    1 仅卸载面板      删程序 / LaunchDaemon / sudoers / CLI 入口 / 证书信任；
#                      保留面板数据（配置·数据库·证书·备份）与全部运行环境
#    2 面板 + LNMP     再卸 nginx / 各版本 PHP(-FPM) / MySQL / MariaDB 及其配置；
#                      保留 ~/www（绝不删）与数据库数据目录（结尾会告知位置）
#    3 面板 + 所有环境 再卸面板登记表里"由面板装过"的全部软件，并删所有配置数据
#                      （面板数据·数据库数据目录·应用数据·证书信任）；需 DELETE-ALL
#
#  向后兼容：`--purge` == `--mode 3 --yes`（旧的"彻底卸载"语义）。
#  实时进度：每个动作动手前打印 `→ 正在…`，完成 `✓`，跳过 `·`，失败 `✗`；
#            删除前打印绝对路径与大小（du -sh）；结尾按"已卸载 / 已删除 / 已保留 / 失败"汇总。
#  铁律：绝不删 ~/www；绝不拿 `brew list` 反推要删的软件 —— 模式 3 只认面板自己的登记表
#        （<面板根>/data/panel.db 的 services 表）。读不到登记表就拒绝执行，绝不猜。
#  刻意不用 set -e：脚本里有多处 `cmd && ok "…"`，配 set -e 会**静默中止**
#  （真机事故：用户看到"卸载完成"，实际什么都没删）。改为每步显式判断并在结尾大声报错。
# =============================================================================
set -uo pipefail

PANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
PANEL_LABEL="${ZIZPANEL_PANEL_LABEL:-cn.zizpanel.panel}"
LINK_DIR="${ZIZPANEL_LINK_DIR:-/usr/local/bin}"
PLIST_DIR="${ZIZPANEL_PLIST_DIR:-/Library/LaunchDaemons}"
SUDOERS_DIR="${ZIZPANEL_SUDOERS_DIR:-/etc/sudoers.d}"
APPS_DIR="${ZIZPANEL_APPS_DIR:-/Applications}"
DB_FILE="${ZIZPANEL_DB_FILE:-$PANEL_ROOT/data/panel.db}"

# Homebrew 前缀：测试/沙箱用 ZIZPANEL_BREW_PREFIX 整体覆盖；设了它就**只认它**，
# 绝不回落到真实前缀（否则沙箱测试会去删真机的 /opt/homebrew）。
BREW_PREFIX_OVERRIDE="${ZIZPANEL_BREW_PREFIX:-}"
BREW_PREFIXES=()
if [ -n "$BREW_PREFIX_OVERRIDE" ]; then
  BREW_PREFIXES=("$BREW_PREFIX_OVERRIDE")
else
  BREW_PREFIXES=("/opt/homebrew" "/usr/local")
fi

# 面板自有命名空间（cn.zizpanel.*）。`cn.zizdog.*` **不是**面板独占 —— 用户自己的
# 服务也在那个前缀下，所以只能按明确名单处理，绝不一刀切。
PANEL_OWN_LABELS=("$PANEL_LABEL" "cn.zizpanel.upgrade-watchdog")
# 面板**应用**安装器登记的固定标签（来自 Go 侧常量；managed 字段不可靠，见文件尾说明）。
PANEL_APP_LABELS=(
  com.zizdog.iopaint com.zizdog.qwen3tts com.zizdog.voicereceiver
  com.zizdog.imgcompress com.zizdog.macosspeech com.zizdog.stt
  com.zizdog.colima com.zizdog.filebrowser com.zizdog.frpc
  com.zizdog.orbien-client com.zizdog.ddns-go com.zizdog.alist
  cn.zizpanel.macsaber cn.macsaber.web
)

DRY=0
ASSUME_YES=0
PURGE_BACKUPS=0
MODE=""
MODE_DESC=""
BACKUP_DIR=""

C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
C_RED=$'\033[31m'; C_BLUE=$'\033[34m'

say()  { printf '%s\n' "$*"; }
title(){ printf '\n%s▸ %s%s\n' "$C_BOLD" "$*" "$C_RESET"; }
begin(){ printf '  %s→%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }   # 动手前
ok()   { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
skip() { printf '  %s·%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
bad()  { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$*"; }

# --------------------------------------------------------------- 失败/结果 --
FAILED_COUNT=0
FAILED_LIST=""
DELETED_LIST=""
UNINSTALLED_LIST=""
record_failed() { FAILED_COUNT=$((FAILED_COUNT + 1)); FAILED_LIST="${FAILED_LIST}$1"$'\n'; }
record_deleted() { DELETED_LIST="${DELETED_LIST}$1"$'\n'; }
record_uninstalled() { UNINSTALLED_LIST="${UNINSTALLED_LIST}$1"$'\n'; }

# ---------------------------------------------------------------- 交互输入 --
# 与 install.sh 同一套：从 /dev/tty 读，管道执行时也能问人。
TTY_FD=""
if { exec 3</dev/tty; } 2>/dev/null; then TTY_FD=3; fi
have_tty() { [ -n "$TTY_FD" ]; }

ask_line() { # ask_line <提示> <变量名>；没有终端时返回失败（调用方据此安全取消）
  local prompt="$1" __var="$2" line=""
  have_tty || return 1
  printf '%s' "$prompt" >&2
  IFS= read -r -u "$TTY_FD" line || return 1
  printf -v "$__var" '%s' "$line"
}

confirm_phrase() { # confirm_phrase <要求输入的字> <说明>
  local want="$1" note="$2" got=""
  say ""
  say "  $note"
  ask_line "  请输入 ${C_BOLD}${want}${C_RESET} 确认（其它任何输入都会取消）：" got || return 1
  [ "$got" = "$want" ]
}

# ------------------------------------------------------------ 通用小工具 --
list_contains() { # list_contains <needle> <item>...
  local needle="$1"; shift
  local x
  for x in "$@"; do
    [ "$x" = "$needle" ] && return 0
  done
  return 1
}

path_size() {
  [ -e "$1" ] || return 0
  du -sh "$1" 2>/dev/null | awk 'NR==1{print $1}'
}

user_exists() { id -u "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------- 真实用户/家目录 --
# sudo 下 $HOME 是 /var/root，用它去拼 ~/www 会看错地方（甚至删错），所以必须解析真实用户。
detect_real_user() {
  local u="${SUDO_USER:-}"
  if [ -z "$u" ] || [ "$u" = "root" ]; then u="${ZIZPANEL_REAL_USER:-}"; fi
  if [ -z "$u" ] || [ "$u" = "root" ]; then
    u="$(stat -f '%Su' /dev/console 2>/dev/null || echo '')"
  fi
  if [ -z "$u" ] || [ "$u" = "root" ]; then u="$(id -un)"; fi
  printf '%s' "$u"
}

REAL_USER="${ZIZPANEL_REAL_USER:-}"
if [ -z "$REAL_USER" ]; then REAL_USER="$(detect_real_user)"; fi
REAL_HOME="${ZIZPANEL_REAL_HOME:-}"
if [ -z "$REAL_HOME" ]; then
  if [ "$(id -u)" -eq 0 ] && user_exists "$REAL_USER" && [ "$REAL_USER" != "root" ]; then
    REAL_HOME="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
    if [ -z "$REAL_HOME" ]; then REAL_HOME="$(eval echo "~$REAL_USER" 2>/dev/null || echo "$HOME")"; fi
  else
    REAL_HOME="$HOME"
  fi
fi

# ------------------------------------------------------------- 计划（清单） --
PLAN_STOP=(); PLAN_UNINSTALL=(); PLAN_DELETE=(); PLAN_BACKUP=(); PLAN_KEEP=()
PLAN_STOP_KEYS=""; PLAN_UNINSTALL_KEYS=""; PLAN_DELETE_KEYS=""; PLAN_BACKUP_KEYS=""; PLAN_KEEP_KEYS=""
# 面板根内被判定为"备份/副本"的条目：默认要**搬出去保留**，不能跟着根目录一起删
PANEL_ROOT_BACKUP_ITEMS=()
plan_stop() {
  [ -n "$1" ] || return 0
  case "$PLAN_STOP_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_STOP_KEYS="${PLAN_STOP_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_STOP+=("$1")
}
plan_uninstall() {
  [ -n "$1" ] || return 0
  case "$PLAN_UNINSTALL_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_UNINSTALL_KEYS="${PLAN_UNINSTALL_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_UNINSTALL+=("$1")
}
plan_delete() {
  [ -n "$1" ] || return 0
  case "$PLAN_DELETE_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_DELETE_KEYS="${PLAN_DELETE_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_DELETE+=("$1")
}
plan_backup() { # 备份/副本：默认保留，只有 --purge-backups 才删
  [ -n "$1" ] || return 0
  case "$PLAN_BACKUP_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_BACKUP_KEYS="${PLAN_BACKUP_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_BACKUP+=("$1")
}
plan_keep() {
  [ -n "$1" ] || return 0
  case "$PLAN_KEEP_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_KEEP_KEYS="${PLAN_KEEP_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_KEEP+=("$1")
}
# 面板根需要特殊处理（保留备份时先转移），但清单里要能看到
PANEL_ROOT_SPECIAL=0

emit_plan() { # emit_plan：把"将要删的清单"打到 stdout（dry-run 预览 / 写进备份）
  printf '# ZizPanel 卸载计划（本文件就是"将要删的清单"）\n'
  printf '# 生成时间：%s\n' "$(date '+%Y-%m-%d %H:%M:%S')"
  printf '# 模式：%s（%s）\n\n' "$MODE" "$MODE_DESC"
  printf '## 将停止的服务\n'
  local x
  for x in ${PLAN_STOP[@]+"${PLAN_STOP[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将卸载的软件\n'
  for x in ${PLAN_UNINSTALL[@]+"${PLAN_UNINSTALL[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将删除的路径（活动数据）\n'
  for x in ${PLAN_DELETE[@]+"${PLAN_DELETE[@]}"}; do printf '  - %s\n' "$x"; done
  if [ "$PURGE_BACKUPS" = "1" ]; then
    printf '\n## 备份/副本（--purge-backups：本次会删）\n'
  else
    printf '\n## 备份/副本（默认保留；--purge-backups 才删）\n'
  fi
  for x in ${PLAN_BACKUP[@]+"${PLAN_BACKUP[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将保留\n'
  for x in ${PLAN_KEEP[@]+"${PLAN_KEEP[@]}"}; do printf '  - %s\n' "$x"; done
}

write_plan_file() {
  local f="$1"
  emit_plan > "$f" 2>/dev/null || warn "写清单失败：$f"
}

# ------------------------------------------------------------- 危险路径护栏 --
is_safe_delete_path() { # 只用于**从登记表推导出来**的路径（work_dir 等）
  local p="$1" prot
  [ -n "$p" ] || return 1
  case "$p" in /) return 1 ;; /*) ;; *) return 1 ;; esac
  # 铁律：绝不动 ~/www 及其子路径
  case "$p" in
    "$REAL_HOME/www"|"$REAL_HOME/www/"*) return 1 ;;
  esac
  for prot in "/Users" "/opt" "/usr" "/usr/local" /opt/homebrew "/Library" "/Applications" \
              "/System" "/Volumes" "/private" "/etc" "/var" "/bin" "/sbin" "/tmp" \
              "$REAL_HOME" "$PANEL_ROOT"; do
    [ "$p" = "$prot" ] && return 1
  done
  # 至少 /a/b 两级，避免把顶层目录当数据目录删掉
  case "$p" in /*/*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------- 删除动作 --
remove_path() { # remove_path <路径> [说明]
  local path="$1" desc="${2:-}"
  case "$path" in
    ""|"/") bad "拒绝删除危险路径：${path}"; record_failed "拒绝删除危险路径"; return 1 ;;
  esac
  if [ ! -e "$path" ] && [ ! -L "$path" ]; then
    skip "本来就没有 ${path}"
    return 0
  fi
  local sz=""
  sz="$(path_size "$path")"
  if [ -n "$desc" ]; then
    begin "正在删除 ${desc}：${path}（${sz}）"
  else
    begin "正在删除 ${path}（${sz}）"
  fi
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：rm -rf %s\n' "$C_YELLOW" "$C_RESET" "$path"
    return 0
  fi
  if rm -rf -- "$path" 2>/dev/null; then
    ok "已删除 ${path}"
    record_deleted "$path"
  else
    bad "删除失败：${path}"
    record_failed "删除失败 ${path}"
    return 1
  fi
}

# ------------------------------------------------------------- launchd 动作 --
launchd_has() {
  launchctl list 2>/dev/null | awk '{print $3}' | grep -qx -- "$1"
}

bootout_label() {
  local label="$1"
  [ -n "$label" ] || return 0
  if [ "$DRY" = "1" ]; then
    begin "正在停止服务 ${label}"
    printf '    %s[dry-run]%s 将执行：launchctl bootout system/%s\n' "$C_YELLOW" "$C_RESET" "$label"
    return 0
  fi
  if ! launchd_has "$label"; then
    skip "本来就没有服务 ${label}"
    return 0
  fi
  begin "正在停止服务 ${label}"
  if launchctl bootout "system/$label" >/dev/null 2>&1; then
    ok "已停止 ${label}"
  else
    bad "停止失败：${label}"
    record_failed "停止服务失败 ${label}"
  fi
}

# --------------------------------------------------------------- brew 动作 --
brew_bin() {
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    if [ -x "$p/bin/brew" ]; then printf '%s' "$p/bin/brew"; return 0; fi
  done
  if command -v brew >/dev/null 2>&1; then command -v brew; return 0; fi
  return 1
}

# brew 明确拒绝 root 运行；以真实用户身份执行（面板侧同款做法）。
as_user_exec() {
  if [ "$(id -u)" -eq 0 ] && [ -n "$REAL_USER" ] && [ "$REAL_USER" != "root" ]; then
    /usr/bin/sudo -n -u "$REAL_USER" -H "$@"
  else
    "$@"
  fi
}

# 只读查询：dry-run 下也执行（用来算真实清单）。
brew_query() {
  local b=""
  b="$(brew_bin)" || return 1
  as_user_exec "$b" "$@"
}

brew_services_stop() {
  local f="$1"
  begin "正在停止 ${f} 服务（brew services stop）"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：brew services stop %s\n' "$C_YELLOW" "$C_RESET" "$f"
    return 0
  fi
  if brew_query services stop "$f" >/dev/null 2>&1; then
    ok "已停止 ${f}"
  else
    skip "${f} 本来就没有在跑（或 brew 未托管它）"
  fi
}

brew_uninstall_formula() {
  local f="$1"
  [ -n "$f" ] || return 0
  begin "正在卸载 brew 软件 ${f}"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：brew uninstall --ignore-dependencies %s\n' "$C_YELLOW" "$C_RESET" "$f"
    return 0
  fi
  local out=""
  if out="$(brew_query uninstall --ignore-dependencies "$f" 2>&1)"; then
    ok "已卸载 ${f}"
    record_uninstalled "${f}（brew formula）"
  else
    bad "卸载失败：${f} —— $(printf '%s' "$out" | tail -n 1)"
    record_failed "卸载失败 ${f}"
    return 1
  fi
}

# --------------------------------------------------------------- 软件探测 --
formula_from_label() { # 只认 brew 自己的两种 launchd 标签
  case "$1" in
    homebrew.mxcl.*) printf '%s' "${1#homebrew.mxcl.}" ;;
    sh.brew.*)       printf '%s' "${1#sh.brew.}" ;;
    *) printf '' ;;
  esac
}

is_lnmp_formula() {
  case "$1" in
    nginx|nginx@*|php|php@*|mysql|mysql@*|mariadb|mariadb@*) return 0 ;;
  esac
  return 1
}

# 少数面板应用的"引擎"是另一个 brew 包，登记表只记了应用的 launchd 标签。
# 这里是 Go 侧 ImgCompressFormula / STTBrewFormula 的镜像；只对登记表已确认由面板
# 安装的行生效（绝不用 brew list 反推）。新增这类应用时要同步这里。
engine_formula_for_label() {
  case "$1" in
    com.zizdog.imgcompress) printf '%s' "vips" ;;
    com.zizdog.stt)         printf '%s' "whisper.cpp" ;;
    *) printf '' ;;
  esac
}

is_panel_app_label() {
  [ -n "$1" ] || return 1
  list_contains "$1" ${PANEL_APP_LABELS[@]+"${PANEL_APP_LABELS[@]}"}
}

# LNMP = nginx + 各版本 PHP(-FPM) + MySQL/MariaDB。
# 刻意**只**按白名单匹配 `brew list` 的结果（不是"看到什么删什么"）：用户自己装的
# redis/ffmpeg/其它语言运行时一律不碰。PostgreSQL 不在此列（用户点名的 LNMP 只含
# nginx/PHP/MySQL/MariaDB；PG 多为某个应用（Miniflux）的依赖，轻率删会连带弄坏它）。
LNMP_DETECTED=()
detect_lnmp_formulas() {
  LNMP_DETECTED=()
  local b="" out="" f
  b="$(brew_bin)" || return 0
  out="$(as_user_exec "$b" list --formula 2>/dev/null || true)"
  for f in $out; do
    if is_lnmp_formula "$f"; then LNMP_DETECTED+=("$f"); fi
  done
}

# --------------------------------------------------- 面板登记表（唯一软件源） --
# 模式 3 的软件清单**只**来自这里：面板自己的 SQLite 库。绝不用 brew list 反推，
# 因为"用户自己 brew install 的"与"面板装的"在 brew 里长得一模一样。
REG_NAME=(); REG_DISPLAY=(); REG_KIND=(); REG_LABEL=(); REG_PLIST=()
REG_WORKDIR=(); REG_COMPOSE=(); REG_CONTAINER=(); REG_FORMULA=(); REG_ACT=()
REG_WHY=""

registry_collect() {
  REG_WHY=""
  REG_NAME=(); REG_DISPLAY=(); REG_KIND=(); REG_LABEL=(); REG_PLIST=()
  REG_WORKDIR=(); REG_COMPOSE=(); REG_CONTAINER=(); REG_FORMULA=(); REG_ACT=()
  if ! command -v sqlite3 >/dev/null 2>&1; then
    REG_WHY="系统里找不到 sqlite3，无法读取面板登记表"
    return 1
  fi
  if [ ! -f "$DB_FILE" ]; then
    REG_WHY="面板登记表不存在：${DB_FILE}（面板从未启动过，或数据已被删）"
    return 1
  fi
  if ! sqlite3 -readonly "$DB_FILE" "SELECT count(*) FROM services;" >/dev/null 2>&1; then
    REG_WHY="读不到面板登记表里的 services 表：${DB_FILE}"
    return 1
  fi
  # ⚠️ 分隔符必须是**非 IFS 空白**（用 0x1f 单元分隔符）：bash 的 read 会把
  # 连续 TAB（IFS 空白）折叠、并吃掉空字段 —— 登记表里"空的 launch_label / work_dir"
  # 一折叠，字段整体左移，compose_file 会被当成 launchd 标签去 bootout（真踩过）。
  # 行首放哨兵 ZPEND：保证字段数与 read 变量数一致，末尾空字段不会被并入上一个。
  local sep="" s="" n d k c m l pl w cf ct
  sep="$(printf '\037')"
  while IFS="$sep" read -r s n d k c m l pl w cf ct; do
    [ "$s" = "ZPEND" ] || continue
    local formula="" act=0
    formula="$(formula_from_label "$l")"
    if [ -n "$formula" ] && is_lnmp_formula "$formula"; then
      continue   # LNMP 层负责，登记表这里不重复
    fi
    if [ "$m" = "1" ]; then
      act=1
    elif is_panel_app_label "$l"; then
      act=1
    else
      act=0
    fi
    if [ "$act" = "1" ] && [ -z "$formula" ]; then
      formula="$(engine_formula_for_label "$l")"
    fi
    REG_NAME+=("$n"); REG_DISPLAY+=("$d"); REG_KIND+=("$k"); REG_LABEL+=("$l")
    REG_PLIST+=("$pl"); REG_WORKDIR+=("$w"); REG_COMPOSE+=("$cf")
    REG_CONTAINER+=("$ct"); REG_FORMULA+=("$formula"); REG_ACT+=("$act")
  done < <(sqlite3 -readonly -separator "$sep" "$DB_FILE" \
    "SELECT 'ZPEND',name,display_name,kind,category,managed,launch_label,plist_path,work_dir,compose_file,container FROM services;" 2>/dev/null)
  return 0
}

# --------------------------------------------------------------- 计划构建 --
collect_panel_program_paths() {
  local f
  for f in "$PLIST_DIR/$PANEL_LABEL.plist" "$SUDOERS_DIR/zizpanel" \
           "$LINK_DIR/zizpanel" "$LINK_DIR/zizpanel-helper" \
           "$APPS_DIR/ZizPanel.app" \
           "$PANEL_ROOT/bin/zizpanel" "$PANEL_ROOT/bin/zizpanel-helper" \
           "$PLIST_DIR/cn.zizpanel.upgrade-watchdog.plist"; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    plan_delete "$f"
  done
  for f in "$PLIST_DIR"/cn.zizpanel.cron.*.plist "$REAL_HOME"/Library/LaunchAgents/cn.zizpanel.cron.*.plist; do
    [ -e "$f" ] || continue
    plan_delete "$f"
  done
}

collect_panel_labels() {
  local l
  for l in ${PANEL_OWN_LABELS[@]+"${PANEL_OWN_LABELS[@]}"}; do plan_stop "$l"; done
  plan_stop "cn.zizpanel.cron.*（面板自建的计划任务作业）"
}

lnmp_config_paths() {
  local p f ver
  for p in "${BREW_PREFIXES[@]}"; do
    for f in "$p/etc/nginx" "$p/etc/my.cnf" "$p/etc/my.cnf.d"; do
      [ -e "$f" ] && plan_delete "$f"
    done
    for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do
      case "$f" in
        php@*) ver="${f#php@}"; [ -e "$p/etc/php/$ver" ] && plan_delete "$p/etc/php/$ver" ;;
        php)   [ -e "$p/etc/php" ] && plan_delete "$p/etc/php" ;;
      esac
    done
  done
}

plan_lnmp() {
  local f
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do
    plan_uninstall "${f}（brew formula）"
    plan_stop "homebrew.mxcl.${f}"
    plan_stop "sh.brew.${f}"
    [ -e "$PLIST_DIR/homebrew.mxcl.${f}.plist" ] && plan_delete "$PLIST_DIR/homebrew.mxcl.${f}.plist"
    [ -e "$PLIST_DIR/sh.brew.${f}.plist" ] && plan_delete "$PLIST_DIR/sh.brew.${f}.plist"
    [ -e "$REAL_HOME/Library/LaunchAgents/homebrew.mxcl.${f}.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/homebrew.mxcl.${f}.plist"
    [ -e "$REAL_HOME/Library/LaunchAgents/sh.brew.${f}.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/sh.brew.${f}.plist"
  done
  plan_stop "cn.zizdog.nginx"
  [ -e "$PLIST_DIR/cn.zizdog.nginx.plist" ] && plan_delete "$PLIST_DIR/cn.zizdog.nginx.plist"
  [ -e "$REAL_HOME/Library/LaunchAgents/cn.zizdog.nginx.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/cn.zizdog.nginx.plist"
  lnmp_config_paths
}

database_data_paths() { # 仅模式 3：数据库数据目录
  # var/mysql 是**当前活动**数据目录（模式 3 删）；
  # var/mysql.* 是切换 MySQL/MariaDB 时特意留下的**旧引擎数据副本**（等同备份）——
  # 与面板备份同类：默认保留，只有 --purge-backups 才删。
  local p f
  for p in "${BREW_PREFIXES[@]}"; do
    for f in "$p/var/mysql" "$p"/var/mysql.*; do
      [ -e "$f" ] || continue
      case "$f" in
        "$p/var/mysql") plan_delete "$f" ;;
        *) plan_backup "${f}（旧引擎数据副本）" ;;
      esac
    done
  done
}

collect_panel_root_backups() { # 面板根里的备份/副本：默认搬出去保留，不随根目录删
  PANEL_ROOT_BACKUP_ITEMS=()
  [ -d "$PANEL_ROOT" ] || return 0
  local f
  for f in "$PANEL_ROOT/work/backup" \
           "$PANEL_ROOT"/*.bak "$PANEL_ROOT"/*.bak-* \
           "$PANEL_ROOT"/bin/*.bak "$PANEL_ROOT"/bin/*.bak-* \
           "$PANEL_ROOT"/data/*.bak "$PANEL_ROOT"/data/*.bak-*; do
    [ -e "$f" ] || continue
    PANEL_ROOT_BACKUP_ITEMS+=("$f")
    plan_backup "${f}（面板备份/副本）"
  done
}

plan_old_uninstall_backups() { # 以前的卸载备份：默认保留，--purge-backups 才删
  local d
  for d in "$REAL_HOME"/zizpanel-uninstall-backup-*; do
    [ -d "$d" ] || continue
    plan_backup "${d}（旧卸载备份）"
  done
}

colima_data_paths() {
  local d
  for d in "$REAL_HOME/.colima" "$REAL_HOME/.lima/colima"; do
    case "$d" in "$REAL_HOME/www"*) continue ;; esac
    [ -e "$d" ] && plan_delete "$d"
  done
}

build_plan_mode1() {
  collect_panel_program_paths
  collect_panel_labels
  plan_keep "面板数据（配置/数据库/证书/备份）：${PANEL_ROOT}/data"
  plan_keep "全部运行环境（nginx / PHP / MySQL / MariaDB、Homebrew）"
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
}

build_plan_mode2() {
  detect_lnmp_formulas
  collect_panel_program_paths
  collect_panel_labels
  plan_lnmp
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    [ -d "$p/var/mysql" ] && plan_keep "数据库数据目录：${p}/var/mysql（保留；模式 3 才删）"
  done
  plan_keep "面板数据：${PANEL_ROOT}/data（保留；模式 3 才删）"
}

build_plan_mode3() {
  detect_lnmp_formulas
  collect_panel_program_paths
  collect_panel_labels
  plan_lnmp
  local i=0 label formula wd cf
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    label="${REG_LABEL[$i]}"; formula="${REG_FORMULA[$i]}"
    wd="${REG_WORKDIR[$i]}"; cf="${REG_COMPOSE[$i]}"
    if [ "${REG_ACT[$i]}" = "1" ]; then
      plan_uninstall "${REG_DISPLAY[$i]}（登记表 ${REG_NAME[$i]}）"
      if [ -n "$label" ]; then
        plan_stop "$label"
        [ -e "${REG_PLIST[$i]}" ] && plan_delete "${REG_PLIST[$i]}"
        [ -e "$PLIST_DIR/$label.plist" ] && plan_delete "$PLIST_DIR/$label.plist"
        [ -e "$REAL_HOME/Library/LaunchAgents/$label.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/$label.plist"
      fi
      [ -n "$formula" ] && plan_uninstall "${formula}（brew formula）"
      case "$label" in
        cn.zizpanel.macsaber|cn.macsaber.web)
          [ -e /opt/macsaber ] && plan_delete "/opt/macsaber" ;;
        com.zizdog.colima) : ;;   # 数据目录在 colima_data_paths 里
        *) [ -n "$wd" ] && is_safe_delete_path "$wd" && plan_delete "$wd" ;;
      esac
      if [ -n "$cf" ]; then
        local cdir=""
        cdir="$(dirname "$cf")"
        [ -n "$cdir" ] && is_safe_delete_path "$cdir" && plan_delete "$cdir"
      fi
    else
      plan_keep "未确认由面板安装：${REG_DISPLAY[$i]}（${label:-${REG_NAME[$i]}}）"
    fi
    i=$((i + 1))
  done
  colima_data_paths
  database_data_paths
  collect_panel_root_backups
  plan_old_uninstall_backups
  plan_delete "$PANEL_ROOT"
  PANEL_ROOT_SPECIAL=1
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
  plan_keep "Homebrew 本身，以及你自己 brew 装的软件（登记表里没有的一律不碰）"
}

build_plan() {
  PLAN_STOP=(); PLAN_UNINSTALL=(); PLAN_DELETE=(); PLAN_BACKUP=(); PLAN_KEEP=()
  PLAN_STOP_KEYS=""; PLAN_UNINSTALL_KEYS=""; PLAN_DELETE_KEYS=""; PLAN_BACKUP_KEYS=""; PLAN_KEEP_KEYS=""
  PANEL_ROOT_BACKUP_ITEMS=()
  PANEL_ROOT_SPECIAL=0
  case "$MODE" in
    1) build_plan_mode1 ;;
    2) build_plan_mode2 ;;
    3) build_plan_mode3 ;;
  esac
}

# ------------------------------------------------------------------ 备份 --
backup_everything() {
  BACKUP_DIR="${REAL_HOME}/zizpanel-uninstall-backup-$(date +%Y%m%d-%H%M%S)"
  title "备份（删之前先留一份，含待删清单）"
  if [ "$DRY" = "1" ]; then
    say "  [dry-run] 将备份到：${BACKUP_DIR}"
    say "  [dry-run] 备份内容：to-delete.txt（待删清单）、面板配置、面板数据库、"
    say "             MySQL/MariaDB 数据目录、nginx 配置、Brewfile、shell 配置"
    return 0
  fi
  mkdir -p "$BACKUP_DIR" 2>/dev/null && chmod 700 "$BACKUP_DIR" 2>/dev/null
  write_plan_file "$BACKUP_DIR/to-delete.txt"
  ok "待删清单 → to-delete.txt"
  if [ -f "$PANEL_ROOT/data/config.json" ] && cp -f "$PANEL_ROOT/data/config.json" "$BACKUP_DIR/panel-config.json" 2>/dev/null; then
    ok "面板配置 → panel-config.json"
  fi
  if [ -f "$DB_FILE" ] && cp -f "$DB_FILE" "$BACKUP_DIR/panel.db" 2>/dev/null; then
    ok "面板数据库 → panel.db"
  fi
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    if [ -d "$p/var/mysql" ]; then
      if tar -czf "$BACKUP_DIR/mysql-var.tar.gz" -C "$p" var/mysql 2>/dev/null; then
        ok "MySQL/MariaDB 数据 → mysql-var.tar.gz（$(path_size "$BACKUP_DIR/mysql-var.tar.gz")）"
      else
        warn "MySQL/MariaDB 数据备份失败（继续，但请自行确认）"
      fi
    fi
    if [ -d "$p/etc/nginx" ]; then
      if tar -czf "$BACKUP_DIR/nginx-etc.tar.gz" -C "$p" etc/nginx 2>/dev/null; then
        ok "nginx 配置 → nginx-etc.tar.gz"
      fi
    fi
    if [ -x "$p/bin/brew" ]; then
      if "$p/bin/brew" bundle dump --file="$BACKUP_DIR/Brewfile" --force 2>/dev/null; then
        ok "已装软件清单 → Brewfile（之后可 brew bundle install 一键装回）"
      fi
    fi
  done
  cp -f "$REAL_HOME/.zprofile" "$BACKUP_DIR/zprofile.bak" 2>/dev/null || true
  ok "备份目录：$BACKUP_DIR"
}

# ------------------------------------------------------------------ 执行 --
stop_services_from_plan() {
  title "停止服务"
  local l
  for l in ${PLAN_STOP[@]+"${PLAN_STOP[@]}"}; do
    case "$l" in
      *'*'*) : ;;   # 含通配的说明行（cron.*）由下面按前缀扫
      *) bootout_label "$l" ;;
    esac
  done
  # 面板自建的计划任务作业（cn.zizpanel.cron.*）
  local labels one
  labels="$(launchctl list 2>/dev/null | awk '{print $3}' | grep -E '^cn\.zizpanel\.cron\.' || true)"
  for one in $labels; do bootout_label "$one"; done
}

brew_services_stop_from_plan() {
  local f
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do brew_services_stop "$f"; done
}

uninstall_formulas_from_plan() {
  title "卸载软件"
  local f i=0
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do brew_uninstall_formula "$f"; done
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "${REG_FORMULA[$i]}" ]; then
      brew_uninstall_formula "${REG_FORMULA[$i]}"
    fi
    i=$((i + 1))
  done
}

uninstall_registry_apps() { # 结尾汇总要点名"面板装过的应用"（真正的删除动作在别处已做）
  [ "$DRY" = "1" ] && return 0
  local i=0
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    if [ "${REG_ACT[$i]}" = "1" ]; then
      record_uninstalled "${REG_DISPLAY[$i]}（登记表 ${REG_NAME[$i]}）"
    fi
    i=$((i + 1))
  done
}

compose_and_docker_down() {
  local i=0 cf ct
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    cf="${REG_COMPOSE[$i]}"; ct="${REG_CONTAINER[$i]}"
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "$cf" ] && command -v docker >/dev/null 2>&1; then
      begin "正在停止 compose 项目 ${REG_DISPLAY[$i]}"
      if [ "$DRY" = "1" ]; then
        printf '    %s[dry-run]%s 将执行：docker compose -f %s down\n' "$C_YELLOW" "$C_RESET" "$cf"
      elif as_user_exec docker compose -f "$cf" down >/dev/null 2>&1; then
        ok "已停止 ${REG_DISPLAY[$i]}"
      else
        skip "${REG_DISPLAY[$i]} 本来就没有在跑（或 docker 不可用）"
      fi
    fi
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "$ct" ] && command -v docker >/dev/null 2>&1; then
      begin "正在删除容器 ${ct}"
      if [ "$DRY" = "1" ]; then
        printf '    %s[dry-run]%s 将执行：docker rm -f %s\n' "$C_YELLOW" "$C_RESET" "$ct"
      elif as_user_exec docker rm -f "$ct" >/dev/null 2>&1; then
        ok "已删除容器 ${ct}"
      else
        skip "本来就没有容器 ${ct}"
      fi
    fi
    i=$((i + 1))
  done
}

colima_delete() {
  local has=0 i=0
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    [ "${REG_LABEL[$i]}" = "com.zizdog.colima" ] && has=1
    i=$((i + 1))
  done
  [ "$has" = "1" ] || return 0
  begin "正在删除 Colima 虚拟机（其中的容器/镜像/卷都会消失）"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：colima delete -f\n' "$C_YELLOW" "$C_RESET"
    return 0
  fi
  if command -v colima >/dev/null 2>&1 && as_user_exec colima delete -f >/dev/null 2>&1; then
    ok "已删除 Colima 虚拟机"
  else
    warn "colima delete 未成功（可能本来就没有虚拟机）；数据目录仍按清单处理"
  fi
}

remove_firewall_entry() {
  local fw=""
  fw="$(command -v socketfilterfw 2>/dev/null || true)"
  if [ -z "$fw" ]; then fw="/usr/libexec/ApplicationFirewall/socketfilterfw"; fi
  [ -x "$fw" ] || return 0
  begin "正在移除防火墙条目：${APPS_DIR}/ZizPanel.app"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：%s --remove %s\n' "$C_YELLOW" "$C_RESET" "$fw" "$APPS_DIR/ZizPanel.app"
    return 0
  fi
  "$fw" --remove "$APPS_DIR/ZizPanel.app" >/dev/null 2>&1 || true
}

remove_codesign_trust() {
  local crt="" c
  for c in "$PANEL_ROOT/data/zizpanel-codesign.crt" "$PANEL_ROOT/zizpanel-codesign.crt"; do
    if [ -f "$c" ]; then crt="$c"; break; fi
  done
  if [ -z "$crt" ]; then
    skip "本来就没有代码签名证书（无需撤销信任）"
    return 0
  fi
  begin "正在撤销代码签名证书信任：${crt}"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：security remove-trusted-cert -d %s\n' "$C_YELLOW" "$C_RESET" "$crt"
    return 0
  fi
  if security remove-trusted-cert -d "$crt" >/dev/null 2>&1; then
    ok "已撤销证书信任"
  else
    bad "撤销证书信任失败：${crt}（可手工在「钥匙串访问」里删掉 ZizPanel Release 证书）"
    record_failed "撤销证书信任失败 ${crt}"
  fi
}

delete_paths_from_plan() {
  title "删除文件与配置"
  local p
  for p in ${PLAN_DELETE[@]+"${PLAN_DELETE[@]}"}; do
    [ "$p" = "$PANEL_ROOT" ] && continue   # 由 remove_panel_root 处理（要先保住备份）
    remove_path "$p"
  done
}

remove_panel_root() {
  [ "$PANEL_ROOT_SPECIAL" = "1" ] || return 0
  # 备份/副本（work/backup、*.bak 等）默认保留：先搬到备份目录再删根；
  # --purge-backups 才让它们随根目录一起删。搬不动就**不删根**，绝不闷声丢备份。
  if [ "$PURGE_BACKUPS" != "1" ]; then
    local item rel dest
    for item in ${PANEL_ROOT_BACKUP_ITEMS[@]+"${PANEL_ROOT_BACKUP_ITEMS[@]}"}; do
      [ -e "$item" ] || continue
      rel="${item#"$PANEL_ROOT"/}"
      dest="$BACKUP_DIR/panel-preserved/$rel"
      begin "正在保留备份/副本（不删）：${item}"
      if [ "$DRY" = "1" ]; then
        printf '    %s[dry-run]%s 将把 %s 移到 %s\n' "$C_YELLOW" "$C_RESET" "$item" "$dest"
        continue
      fi
      mkdir -p "$(dirname "$dest")" 2>/dev/null
      if mv "$item" "$dest" 2>/dev/null; then
        ok "已保留备份/副本：${dest}"
      else
        bad "备份/副本转移失败：${item} —— 为避免丢备份，${PANEL_ROOT} 不再整体删除"
        record_failed "备份/副本转移失败 ${item}"
        return 0
      fi
    done
  fi
  remove_path "$PANEL_ROOT" "面板数据（配置/数据库/证书/日志/工作目录）"
}

delete_backups_from_plan() { # 面板根内的备份已由 remove_panel_root 搬走；这里处理根外的
  [ "$PANEL_ROOT_SPECIAL" = "1" ] || return 0
  local b path
  for b in ${PLAN_BACKUP[@]+"${PLAN_BACKUP[@]}"}; do
    path="${b%%（*}"   # 清单条目带中文注释后缀，取路径部分
    case "$path" in
      "$PANEL_ROOT"|"$PANEL_ROOT"/*) continue ;;
    esac
    if [ "$PURGE_BACKUPS" = "1" ]; then
      remove_path "$path" "备份/副本（--purge-backups）"
    else
      skip "保留备份/副本 ${path}（--purge-backups 才删）"
    fi
  done
}

mode2_data_notice() {
  say ""
  say "  ${C_BOLD}数据都在，没动：${C_RESET}"
  [ -d "$REAL_HOME/www" ] && say "    · 站点文件：${REAL_HOME}/www（$(path_size "$REAL_HOME/www")）"
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    [ -d "$p/var/mysql" ] && say "    · 数据库数据：${p}/var/mysql（$(path_size "$p/var/mysql")）"
  done
  [ -d "$PANEL_ROOT/data" ] && say "    · 面板数据：${PANEL_ROOT}/data"
  say "  要连这些一起清掉，请用模式 3：sudo bash uninstall.sh --mode 3"
}

execute_plan() {
  title "执行卸载（模式 ${MODE}：${MODE_DESC}）"
  stop_services_from_plan
  if [ "$MODE" != "1" ]; then
    brew_services_stop_from_plan
  fi
  if [ "$MODE" = "3" ]; then
    compose_and_docker_down
    colima_delete
  fi
  remove_firewall_entry
  remove_codesign_trust
  if [ "$MODE" != "1" ]; then
    uninstall_formulas_from_plan
  fi
  if [ "$MODE" = "3" ]; then
    uninstall_registry_apps
  fi
  delete_paths_from_plan
  if [ "$MODE" = "3" ]; then
    remove_panel_root
    delete_backups_from_plan
  fi
  if [ "$MODE" = "2" ]; then
    mode2_data_notice
  fi
}

# ------------------------------------------------------------------ 核对 --
verify() {
  [ "$DRY" = "1" ] && return 0
  title "核对结果"
  if [ -e "$LINK_DIR/zizpanel" ]; then
    bad "面板入口仍在：${LINK_DIR}/zizpanel"; record_failed "面板入口未移除"
  else
    ok "面板入口已移除"
  fi
  if [ "$MODE" = "1" ] || [ "$MODE" = "2" ]; then
    if [ -f "$DB_FILE" ]; then
      ok "面板数据按预期保留：${DB_FILE}"
    else
      warn "面板数据不在 ${DB_FILE}（模式 ${MODE} 本该保留它）"
    fi
  fi
  if [ "$MODE" = "3" ]; then
    if [ -e "$PANEL_ROOT" ]; then
      bad "面板目录仍在：${PANEL_ROOT}"; record_failed "面板目录未删除"
    else
      ok "面板目录已删除：${PANEL_ROOT}"
    fi
  fi
  if [ -d "$REAL_HOME/www" ]; then
    ok "站点目录 ~/www 未被触碰（$(path_size "$REAL_HOME/www")）"
  fi
}

summary() {
  printf '\n%s完成。%s\n' "$C_BOLD" "$C_RESET"
  if [ "$DRY" = "1" ]; then
    say "  （dry-run：以上全是预演，一个字节都没改）"
  fi
  if [ -n "$BACKUP_DIR" ] && [ "$DRY" != "1" ]; then
    say "  备份：${BACKUP_DIR}（含 to-delete.txt 待删清单）"
  fi
  say ""
  say "  ${C_BOLD}已卸载${C_RESET}"
  if [ -n "$UNINSTALLED_LIST" ]; then printf '%s' "$UNINSTALLED_LIST" | sed 's/^/    · /'; else say "    （无）"; fi
  say "  ${C_BOLD}已删除${C_RESET}"
  if [ -n "$DELETED_LIST" ]; then printf '%s' "$DELETED_LIST" | sed 's/^/    · /'; else say "    （无）"; fi
  say "  ${C_BOLD}已保留${C_RESET}"
  local any_kept=0 x
  for x in ${PLAN_KEEP[@]+"${PLAN_KEEP[@]}"}; do printf '    · %s\n' "$x"; any_kept=1; done
  if [ "$MODE" = "3" ] && [ "$PURGE_BACKUPS" != "1" ]; then
    for x in ${PLAN_BACKUP[@]+"${PLAN_BACKUP[@]}"}; do
      printf '    · 备份/副本保留：%s\n' "$x"; any_kept=1
    done
  fi
  [ "$any_kept" = "1" ] || say "    （无）"
  say "  ${C_BOLD}失败${C_RESET}"
  if [ "$FAILED_COUNT" != "0" ]; then
    printf '%s' "$FAILED_LIST" | sed 's/^/    ✗ /'
  else
    say "    （无）"
  fi
  say ""
  say "  重新安装：curl -fsSL https://zizdog.com/zizpanel/install.sh | sudo bash"
  printf '\n'
}

# ------------------------------------------------------------------ 菜单 --
usage() {
  cat <<'EOF'
用法：sudo bash uninstall.sh [1|2|3] [--mode 1|2|3] [--yes] [--dry-run] [--purge-backups]

  1  仅卸载面板         保留面板数据（配置/数据库/证书/备份）与全部运行环境
  2  卸载面板及 LNMP    再卸 nginx / PHP(-FPM) / MySQL / MariaDB 及其配置；
                       保留 ~/www 与数据库数据目录（结尾会告知位置）
  3  卸载面板及所有环境 再卸面板登记表里由面板装过的所有软件，并删所有配置数据
                       （面板数据、数据库数据目录、应用数据、证书信任）；不可逆

  --yes            非交互模式：跳过确认（模式 3 仍需 DELETE-ALL 或显式 --yes）
  --dry-run        只打印"将要做什么/删什么"，一个字节都不改（可在任何机器上预演）
  --purge          旧写法的向后兼容：等价于 --mode 3 --yes
  --purge-backups  连面板备份（<面板根>/work/backup）与旧卸载备份一起删；默认保留
  -h, --help       显示本帮助

  实时进度：逐行打印 `→ 正在…` / `✓ 完成` / `· 本来就没有` / `✗ 失败`，删目录带大小。
EOF
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      1|2|3) MODE="$1"; shift ;;
      --mode) MODE="${2:-}"; shift 2 || shift ;;
      --mode=*) MODE="${1#--mode=}"; shift ;;
      --yes|-y) ASSUME_YES=1; shift ;;
      --dry-run|-n) DRY=1; shift ;;
      --purge|-p) MODE=3; ASSUME_YES=1; shift ;;
      --purge-backups) PURGE_BACKUPS=1; shift ;;
      --keep-backups) PURGE_BACKUPS=0; shift ;;
      -h|--help) usage; exit 0 ;;
      *) bad "未知参数：$1"; usage; exit 2 ;;
    esac
  done
  case "$MODE" in
    ""|1|2|3) : ;;
    *) bad "模式只能是 1 / 2 / 3（收到：${MODE}）"; exit 2 ;;
  esac
}

choose_mode() {
  if ! have_tty; then
    bad "没有终端可交互。请显式指定模式：sudo bash uninstall.sh --mode 1|2|3"
    exit 1
  fi
  printf '\n  请选择要做什么：\n\n'
  printf '    %s1%s  仅卸载面板             保留面板数据与全部运行环境\n' "$C_BOLD" "$C_RESET"
  printf '    %s2%s  卸载面板及 LNMP        保留 ~/www 与数据库数据\n' "$C_BOLD" "$C_RESET"
  printf '    %s3%s  卸载面板及所有环境     删面板数据、数据库数据与面板装过的软件（不可逆）\n' "$C_BOLD" "$C_RESET"
  printf '    %s0%s  退出\n\n' "$C_BOLD" "$C_RESET"
  local pick=""
  ask_line "  输入 1 / 2 / 3 / 0：" pick || { bad "读取输入失败"; exit 1; }
  MODE="$pick"
}

check_macos() {
  if [ "$(uname -s)" != "Darwin" ]; then
    if [ "$DRY" = "1" ]; then
      warn "非 macOS（$(uname -s)）：dry-run 仍可预演，但真跑只支持 macOS"
      return 0
    fi
    bad "本脚本只支持 macOS（当前：$(uname -s)）"
    exit 1
  fi
}

require_root() {
  [ "$DRY" = "1" ] && return 0
  [ "${ZIZPANEL_SANDBOX:-0}" = "1" ] && return 0
  if [ "$(id -u)" -ne 0 ]; then
    bad "需要 root：请用  curl … | sudo bash   或   sudo bash uninstall.sh"
    exit 1
  fi
}

describe_mode() {
  case "$MODE" in
    1)
      MODE_DESC="仅卸载面板"
      title "模式 1：仅卸载面板"
      say "  会删除：面板程序、LaunchDaemon、sudoers 规则、CLI 入口、证书信任"
      say "  会保留：面板数据（${PANEL_ROOT}/data）与全部运行环境（nginx/PHP/MySQL/MariaDB）"
      ;;
    2)
      MODE_DESC="卸载面板及 LNMP"
      title "模式 2：卸载面板及 LNMP"
      say "  会删除：模式 1 的全部，加上 nginx / 各版本 PHP(-FPM) / MySQL / MariaDB 及其配置"
      warn "期间网站会中断；面板数据与 ~/www、数据库数据目录都保留"
      ;;
    3)
      MODE_DESC="卸载面板及所有环境"
      title "模式 3：卸载面板及所有环境（不可逆）"
      warn "会删除：面板数据、数据库数据目录、应用数据、证书信任、面板装过的全部软件"
      say "  软件清单只来自面板登记表 ${DB_FILE}（读不到就拒绝执行，绝不猜）"
      say "  不会删：~/www（你的站点文件）、Homebrew 本身、你自己 brew 装的软件"
      ;;
  esac
}

confirm_all() {
  [ "$DRY" = "1" ] && return 0
  if [ "$ASSUME_YES" != "1" ]; then
    if have_tty; then
      if ! confirm_phrase "YES" "确认按上面的说明继续？"; then
        say "已取消，什么都没做。"; exit 0
      fi
    else
      bad "非交互模式必须显式授权：请加 --yes（模式 3 还需 DELETE-ALL）"
      exit 1
    fi
  fi
  if [ "$MODE" = "3" ]; then
    if [ "$ASSUME_YES" = "1" ] && ! have_tty; then
      warn "非交互模式 + --yes：跳过 DELETE-ALL 手工确认（该组合本身就是明确授权）"
    else
      if ! confirm_phrase "DELETE-ALL" "这是**不可逆**的操作：面板数据、数据库数据目录与面板装过的软件都会被删除（~/www 不动）。"; then
        say "确认字不匹配，已取消，什么都没做。"; exit 0
      fi
    fi
  fi
}

main() {
  parse_args "$@"   # 先解析：--dry-run 要能在任何机器上预演（含非 macOS）
  check_macos

  printf '\n%s╭────────────────────────────────────────────╮%s\n' "$C_BOLD" "$C_RESET"
  printf '%s│   ZizPanel 卸载向导（三档）                │%s\n' "$C_BOLD" "$C_RESET"
  printf '%s╰────────────────────────────────────────────╯%s\n' "$C_BOLD" "$C_RESET"
  [ "$DRY" = "1" ] && warn "dry-run 模式：只打印，不做任何修改"

  if [ -z "$MODE" ]; then choose_mode; fi
  case "$MODE" in
    0|q|quit) say "已退出，什么都没做。"; exit 0 ;;
    1|2|3) : ;;
    *) bad "无法识别的选择：$MODE"; exit 2 ;;
  esac

  require_root
  describe_mode

  # 模式 3 的软件清单必须在**删掉面板之前**从登记表枚举；读不到就拒绝（不许用 brew list 猜）。
  if [ "$MODE" = "3" ]; then
    if ! registry_collect; then
      if [ "$DRY" = "1" ]; then
        warn "登记表不可读：${REG_WHY}"
        warn "（真跑时模式 3 会在此拒绝执行，不会删任何东西）"
      else
        bad "登记表不可读：${REG_WHY}"
        bad "为避免误删你自己装的软件，模式 3 拒绝执行；可用模式 2（面板 + LNMP）。"
        exit 1
      fi
    fi
  fi

  confirm_all
  build_plan

  if [ "$DRY" = "1" ]; then
    title "dry-run 清单（以下是将要删除/卸载的全部内容）"
    emit_plan
    if [ "$MODE" = "3" ]; then
      say ""
      say "  真跑时本次会要求手工输入 DELETE-ALL 确认（非交互需 --yes）"
    fi
  fi

  if [ "$MODE" != "1" ]; then
    backup_everything
  fi

  execute_plan
  verify
  summary

  if [ "$FAILED_COUNT" != "0" ] && [ "$DRY" != "1" ]; then
    printf '  %s上面有失败项：请把输出整段发给开发者%s\n\n' "$C_RED" "$C_RESET"
    exit 1
  fi
}

main "$@"
ZP_UNINSTALL_EOF
# <<< ZP_EMBEDDED_UNINSTALLER_END
  chmod 0755 "$ZIZPANEL_ROOT/uninstall.sh"
}

# ============================================================================
#  交互输入：绝不在管道里卡住（zp_input_ok 判据，不是简单 [ -t 0 ]）；环境变量优先；
#  口令永不进 argv/日志（走 stdin）；非交互缺必填项明确报错，绝不用默认口令。
# ============================================================================

# ZP_TTY_FD 是交互读入的文件描述符；打不开 /dev/tty 时为空。
ZP_TTY_FD=""
if { true >/dev/tty; } 2>/dev/null; then
  exec 3</dev/tty 2>/dev/null && ZP_TTY_FD=3
fi

# zp_input_ok：现在能不能问用户。只看有没有可用终端与是否要求不提问，**不**看沙箱/干跑。
zp_input_ok() {
  [ "$ZIZPANEL_YES" = "1" ] && return 1
  [ -n "$ZP_TTY_FD" ] || return 1
  return 0
}

# zp_random_hex <字节数>：随机十六进制串（/dev/urandom，退回 openssl）。
# 不用 ${RANDOM}：只有 15 位，拼出来的"安全后缀"可枚举。
zp_random_hex() {
  local n="${1:-4}" out=""
  if [ -r /dev/urandom ]; then
    out="$(LC_ALL=C od -An -tx1 -N "$n" /dev/urandom 2>/dev/null | tr -d ' \n')"
  fi
  if [ -z "$out" ] && command -v openssl >/dev/null 2>&1; then
    out="$(openssl rand -hex "$n" 2>/dev/null || true)"
  fi
  [ -n "$out" ] || out="$$$(date +%s)"
  printf '%s' "$out"
}

# zp_read <提示> <默认值> <变量名> [是否秘密]
# 返回 0=读到输入（空行取默认值）、1=EOF、2=超时。
# 必须分开：都当"取消"的话，用户敲一个回车就能把安装悄悄取消（真发生过）。
zp_read() {
  local prompt="$1" def="$2" __var="$3" secret="${4:-0}" line="" rc=0 attempt=0
  local ttydev="/dev/tty" tty_saved=""
  # 没有可交互终端：直接返回，让调用方走默认值分支。
  # 不能执行 read -u ""：bash 会打一行 invalid file descriptor 的噪音。
  if [ -z "$ZP_TTY_FD" ]; then
    printf -v "$__var" '%s' "${def:-}"
    return 1
  fi
  if [ -n "$def" ]; then
    printf '%s%s%s %s[%s]%s: ' "$C_BOLD" "$prompt" "$C_RESET" "$C_YELLOW" "$def" "$C_RESET"
  else
    printf '%s%s%s: ' "$C_BOLD" "$prompt" "$C_RESET"
  fi
  # read -t 在真终端上会被信号打断（返回 >128 且没读到输入），打断就重试（最多 3 次）。
  while :; do
    rc=0
    if [ "$secret" = "1" ]; then
      # 关回显必须显式 stty：read -s 在 -u <fd> 下不生效（bash 3.2），实测口令会明文回显。
      tty_saved="$(stty -g < "$ttydev" 2>/dev/null || true)"
      [ -n "$tty_saved" ] && stty -echo < "$ttydev" 2>/dev/null
      IFS= read -r -t 180 -u "$ZP_TTY_FD" line || rc=$?
      [ -n "$tty_saved" ] && stty "$tty_saved" < "$ttydev" 2>/dev/null
      printf '\n'
    else
      IFS= read -r -t 180 -u "$ZP_TTY_FD" line || rc=$?
    fi
    [ "$rc" -eq 0 ] && break
    if [ "$rc" -gt 128 ] && [ "$rc" -ne 142 ]; then
      # >128 且不是 142（142 = 128+14，read 自身的超时约定）→ 被信号打断，重试
      attempt=$((attempt + 1))
      if [ "$attempt" -le 2 ]; then
        continue
      fi
      return 2
    fi
    return 1
  done
  line="${line%$'\r'}"
  [ -n "$line" ] || line="$def"
  printf -v "$__var" '%s' "$line"
  return 0
}

# zp_ask <提示> <默认值> <变量名>：回车接受默认值；读不到就用默认值。
zp_ask() {
  local prompt="$1" def="$2" __var="$3"
  zp_read "$prompt" "$def" "$__var" 0
}

# zp_confirm <提示> <默认 y|n>：0=是，1=否，2=读不到（EOF）。
# 读不到**不**当"否"（否则真终端一敲回车就把安装取消了），调用方必须显式处理。
zp_confirm() {
  local prompt="$1" def="${2:-y}" ans="" rc=0
  local hint="[Y/n]"
  [ "$def" = "n" ] && hint="[y/N]"
  zp_read "$prompt $hint" "" ans 0 || rc=$?
  ans="$(printf '%s' "$ans" | tr '[:upper:]' '[:lower:]')"
  case "$ans" in
    y|yes|是|1) return 0 ;;
    n|no|否|0)  return 1 ;;
    "") [ "$def" = "y" ] && return 0; return 1 ;;
    *)  [ "$def" = "y" ] && return 0; return 1 ;;
  esac
}

# zp_yes <提示> <默认 y|n>：EOF 按默认值处理，交互场景不该因读不到就取消。
zp_yes() {
  local rc=0
  zp_confirm "$1" "${2:-y}" || rc=$?
  [ "$rc" -eq 0 ]
}

# zp_normalize_suffix：与 Go 侧 config.NormalizePanelSuffix 同一套规则，
# 放在这里是为了让用户在装完之前就知道后缀长什么样。
zp_normalize_suffix() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -d ' \t\r\n/' \
    | LC_ALL=C tr -cd 'a-z0-9_-' | cut -c1-32
}

# zp_random_suffix：与 Go 侧 RandomPanelSuffix 同一字符集（去掉 i/l/o/0/1）。
zp_random_suffix() {
  local alphabet="abcdefghjkmnpqrstuvwxyz23456789" hex i c out=""
  hex="$(zp_random_hex 16)"
  hex="$(printf '%s' "$hex" | tr -cd '0-9a-f')"
  while [ ${#hex} -lt 16 ]; do hex="${hex}0"; done
  for i in 0 1 2 3 4 5 6 7; do
    c=$(( 16#${hex:$((i*2)):2} % 31 ))
    out="${out}${alphabet:$c:1}"
  done
  printf '%s' "$out"
}

# zp_read_quiet <提示> <变量名>：读口令（不回显），返回 1 = EOF/超时。
zp_read_quiet() {
  local prompt="$1" __var="$2"
  zp_read "$prompt" "" "$__var" 1
}

# collect_basic_info：语言/确认 → 用户名 → 口令 → 后缀 → 端口。
collect_basic_info() {
  title "安装选项"

  if [ "$ZIZPANEL_DRY_RUN" = "1" ]; then
    info "干跑模式：只显示将要做的事，不写系统、不装软件"
  fi

  if zp_input_ok; then
    info "接下来只会问面板后缀与监听端口（管理员账号由面板首次访问时设置）。直接回车 = 用方括号里的默认值。"
    info "想全程不回答：Ctrl-C 后用 ZP_YES=1 重跑。"
    printf '\n'
    # 语言/确认：默认中文，回车即继续。"英文"也接受，但这轮只影响确认语。
    local ans=""
    zp_ask "安装界面语言（回车 = 中文）" "中文" ans
    case "$ans" in
      en|EN|En|english|English|英文) warn "本轮仍以中文输出（英文界面尚未提供，已在报告中列为待办）" ;;
    esac
    if ! zp_yes "确认在这台机器上安装 ZizPanel？" "y"; then
      info "已取消，未做任何修改。"
      exit 0
    fi
  fi

  # ---- 管理员账号：安装器不再询问 ----
  # 🛑 2026-09-17 用户要求：账号统一由面板首次初始化向导创建，安装期不问、也不生成口令；
  # 只有显式提供（ZP_USER/ZP_PASS 或 --user/--password）时才在安装期建号，供自动化用。
  if [ -n "$ADMIN_PASSWORD" ] && [ -z "$ADMIN_USERNAME" ]; then
    ADMIN_USERNAME="admin"
  fi
  if [ -n "$ADMIN_USERNAME" ] && [ -z "$ADMIN_PASSWORD" ]; then
    die "给了管理员用户名（ZP_USER/--user）就必须同时给口令（ZP_PASS/--password）"
  fi
  if [ -z "$ADMIN_USERNAME" ]; then
    info "管理员账号留给面板：装好后打开面板，首次访问会让你设置用户名与口令（安装器不再询问）。"
  fi
  # 提前校验口令长度（与 auth.CreateUser 规则一致）：装完才报错用户得重装一遍。
  if [ -n "$ADMIN_PASSWORD" ] && [ "${#ADMIN_PASSWORD}" -lt 8 ]; then
    die "登录口令至少 8 位（当前 ${#ADMIN_PASSWORD} 位）"
  fi

  # ---- 面板路径后缀：安装器不再询问 ----
  # 🛑 2026-09-17 用户要求：默认不设后缀，装好后在面板「系统设置」里随时能开启；
  # 升级/重装保留原有后缀，只有显式给了 ZP_SUFFIX/--suffix 才用传入值。
  if [ -f "$DATA_DIR/config.json" ] && [ -z "$PANEL_SUFFIX_INPUT" ]; then
    PANEL_SUFFIX_INPUT="$(zcfg_get panel_suffix)"
  fi
  if [ -n "$PANEL_SUFFIX_INPUT" ]; then
    PANEL_SUFFIX_INPUT="$(zp_normalize_suffix "$PANEL_SUFFIX_INPUT")"
    info "面板路径后缀（安全入口）：${PANEL_SUFFIX_INPUT}（来自已有配置或 ZP_SUFFIX）"
  else
    info "面板路径后缀未启用（安全加强默认关闭）：装好后可在面板「系统设置」里随时开启。"
  fi

  # ---- 监听端口：不问，默认 8443（🛑 2026-09-17 用户要求）----
  # 想换端口：装前 ZP_PORT=9000 / --port 9000；装后改 config.json 的 listen 再 kickstart。
  local final_port="${ZIZPANEL_LISTEN##*:}"
  if [ -n "${ZIZPANEL_LISTEN_EXPLICIT:-}" ]; then
    info "监听端口：${final_port}（来自 ZP_PORT/--port）"
  else
    info "监听端口：${final_port}（默认值；想改就在装之前给 ZP_PORT=端口）"
  fi
  printf '\n'
  printf '  %s将要安装：%s\n' "$C_BOLD" "$C_RESET"
  if [ -n "$ADMIN_USERNAME" ]; then
    printf '    管理员账号     %s（安装期创建，来自 ZP_USER/ZP_PASS）\n' "$ADMIN_USERNAME"
  else
    printf '    管理员账号     %s（由面板首次访问时的初始化向导创建）\n' '稍后设置'
  fi
  if [ -n "$PANEL_SUFFIX_INPUT" ]; then
    printf '    面板路径后缀   %s\n' "$PANEL_SUFFIX_INPUT"
  else
    printf '    面板路径后缀   %s\n' '未启用（可在面板「系统设置」里开启安全加强）'
  fi
  printf '    监听端口       %s\n' "$final_port"
  printf '    安装目录       %s\n' "$ZIZPANEL_ROOT"
  if [ "$(id -u)" -eq 0 ]; then
    printf '    安装用户       %s\n' "$REAL_USER"
  fi
  printf '\n'
}

# ---------------------------------------------------------- 安装本机判定 --
# SSH_CONNECTION/SSH_CLIENT/SSH_TTY 非空 = 远程；本机证据看 TERM_PROGRAM/__CFBundleIdentifier
# 与"当前用户 == 控制台登录用户"（curl | sudo bash 下 sudo 会重置环境，单看一个信号会漏判）。
is_remote_session() {
  # 返回 0 = 远程（从别的电脑连过来）
  [ -n "${SSH_CONNECTION:-}" ] && return 0
  [ -n "${SSH_CLIENT:-}" ] && return 0
  [ -n "${SSH_TTY:-}" ] && return 0
  # 明确的本机终端证据（这些变量只在本机终端 App 里存在）
  [ -n "${TERM_PROGRAM:-}" ] && return 1
  [ -n "${TERM_SESSION_ID:-}" ] && return 1
  [ -n "${__CFBundleIdentifier:-}" ] && return 1
  # 兜底：谁登录在控制台（物理/图形控制台）。当前用户就是它 → 本机操作。
  local console_user=""
  console_user="$(stat -f '%Su' /dev/console 2>/dev/null || echo "")"
  if [ -n "$console_user" ] && [ "$console_user" != "root" ] && [ "$console_user" = "${REAL_USER:-}" ]; then
    return 1
  fi
  return 0
}

# -------------------------------------- 代码签名证书信任（授权能否跨升级存活）--
# macOS 把授权绑在二进制的代码要求上：adhoc 的判据是 cdhash（实测升级即失效），固定证书 +
# 固定 identifier 后与它无关（2026-09-19 实测 1.4.8→1.4.9 成立）；这里以 root 把证书公钥装进系统钥匙串。
install_codesign_trust() {
  title "代码签名证书信任（让面板授权跨升级有效）"
  local crt=""
  for c in "$SCRIPT_DIR/zizpanel-codesign.crt" "$TMP_DIR/zizpanel-codesign.crt"; do
    [ -f "$c" ] && { crt="$c"; break; }
  done
  if [ -z "$crt" ]; then
    warn "包里没有 zizpanel-codesign.crt（旧包或不带签名的构建）"
    info "不影响安装；但面板升级后系统可能要求你重新授权一次外接盘/受保护目录。"
    return 0
  fi
  if [ "$(id -u)" != "0" ]; then
    warn "非 root，跳过证书信任导入；请用 sudo 重跑，或手工执行："
    info "  sudo security add-trusted-cert -d -r trustRoot -p codeSign -k /Library/Keychains/System.keychain $crt"
    return 0
  fi
  if dry_run; then
    info "（干跑）将导入并信任代码签名证书：$crt"
    return 0
  fi
  # 数据目录留副本：卸载脚本要凭它撤销信任，否则会把受信任的代码签名根永久留在系统里。
  mkdir -p "$DATA_DIR" 2>/dev/null || true
  cp -f "$crt" "$DATA_DIR/zizpanel-codesign.crt" 2>/dev/null || true
  if security add-trusted-cert -d -r trustRoot -p codeSign \
       -k /Library/Keychains/System.keychain "$crt" >/dev/null 2>&1; then
    ok "代码签名证书已受系统信任（面板授权可跨升级保持有效）"
    return 0
  fi
  # 已经信任过 / 首次失败都要如实说，不谎报
  if security verify-cert -c "$crt" >/dev/null 2>&1; then
    ok "代码签名证书已在系统信任库里（无需重复导入）"
    return 0
  fi
  warn "证书信任导入失败。影响：面板**升级后**系统可能要求重新授权一次（外接盘、受保护目录）。"
  warn "可手工执行：sudo security add-trusted-cert -d -r trustRoot -p codeSign \\"
  warn "              -k /Library/Keychains/System.keychain $crt"
  return 0
}

# --------------------------------------------- 外接硬盘与系统授权（真机/远程）--
# 🚨 用户 2026-09-19 定的铁律：只有真机安装才允许触发系统授权弹窗；远程/无人在场绝不触发
# （弹了没人点，系统记成 denial 反而让面板以后静默被拒）；两种都要先拿到用户确认。

# external_volume_list：列出当前挂载的非系统卷（与面板 files.NonSystemVolumeMounts 同口径）。
external_volume_list() {
  local v
  for v in /Volumes/*; do
    [ -e "$v" ] || continue
    [ -L "$v" ] && continue
    [ -d "$v" ] || continue
    printf '%s\n' "$v"
  done
}

# console_login_user：当前图形控制台（屏幕前）的登录用户；没人时为 root/loginwindow/空。
console_login_user() {
  local u=""
  u="$(/usr/bin/stat -f '%Su' /dev/console 2>/dev/null || echo "")"
  printf '%s' "$u"
}

# setup_external_volume_notice：安装最前面就问清楚（用户要求"过程中要让用户知道并确认"）。
setup_external_volume_notice() {
  title "外接硬盘（用不用由你决定）"

  # 先问明确的选择题，默认「不用」：不用的人全程不碰外接卷、不被弹窗打扰；
  # 环境变量优先（无人值守）：ZP_EXTERNAL_DISK=1/0。
  local want=0
  case "${ZP_EXTERNAL_DISK:-}" in
    1|yes|y|on)  want=1 ;;
    0|no|n|off)  want=0 ;;
    "")
      if zp_yes "这台机器会接外置硬盘吗？（做镜像盘 / 在文件管理里访问它）" "n"; then
        want=1
      fi
      ;;
    *) warn "无法识别的 ZP_EXTERNAL_DISK=${ZP_EXTERNAL_DISK}（按「不用外接盘」处理）"; want=0 ;;
  esac
  ZP_EXTERNAL_WANT="$want"

  if [ "$want" != "1" ]; then
    info "好 —— 本次安装**不会碰任何外接卷**，也**不会触发任何系统授权弹窗**。"
    info "以后想用了：插上盘，到「面板 → 磁盘」按页面上的指引授权一次即可。"
    if is_remote_session; then ZP_EXTERNAL_MODE="remote"; else ZP_EXTERNAL_MODE="local"; fi
    return 0
  fi

  # ---- 用户说会用到外接硬盘 ----
  if is_remote_session; then
    ZP_EXTERNAL_MODE="remote"
    warn "你是从别的电脑（SSH/远程）跑安装的：本次**不会触发任何系统授权弹窗** ——"
    info "没人能在机器前点按钮，弹窗只会被系统记成拒绝，反而让面板以后一碰外接盘就「静默被拒」。"
    info "要用外接盘，请到**真机**上授权一次（二选一）："
    info "  · 系统设置 → 隐私与安全性 → 完全磁盘访问权限 → 点「+」选中 $BIN_DIR/zizpanel"
    info "    （这块盘还要给站点/镜像站用，就把 nginx 的二进制也照同样方式加上）→ 打开开关"
    info "  · 或者在那台机器上跑到面板「磁盘」页，按页面指引操作（同样不需要弹窗）"
    info "授权是发给**面板这个二进制**的；面板已用固定证书签名，所以**一次授权后升级也不用再授**。"
    if zp_yes "明白了：外接盘需要我到真机上授权。继续安装？" "y"; then
      return 0
    fi
    die "已按你的要求停止安装（可以稍后到真机上再跑一次）"
  fi

  ZP_EXTERNAL_MODE="local"
  ok "你选了会用到外接硬盘。请**现在把它插上**（U 盘/硬盘盒插好后再继续）。"
  info "（面板已用固定证书签名：这一次授权之后，升级面板也不需要再授权。）"

  local vols="" try=0 _ignored=""
  while [ "$try" -lt 3 ]; do
    zp_read "插好后按回车继续（不想插了就再按一次回车，最多问 3 次）" "" _ignored || true
    vols="$(external_volume_list)"
    [ -n "$vols" ] && break
    try=$((try + 1))
    [ "$try" -lt 3 ] && warn "还没检测到外接卷（第 ${try}/3 次）。插好后再按一次回车。"
  done

  if [ -n "$vols" ]; then
    ok "检测到外接卷："
    printf '%s\n' "$vols" | sed 's/^/    /'
  else
    ZP_EXTERNAL_ACK=0
    warn "仍未检测到外接卷 → 这次不申请授权（安装照常继续，不会碰任何外接卷）。"
    info "插上盘以后：到「面板 → 磁盘」按页面指引授权一次即可，或重跑一次安装脚本。"
    return 0
  fi

  # ---- 写一次性标记之前的**显式确认**（用户点名：让用户知道接下来要点弹窗）----
  # 问题先打印出来（没有终端时 zp_yes 不回显，日志/远程都要能看到问的是什么），
  # 再问 yes/no：选"否"就不写标记 = 这次不申请授权；
  # 以后可在面板「磁盘管理 → 申请授权」补（同一套门禁）。
  warn "接下来安装过程中，系统会弹一次「…想要访问可移除宗卷上的文件」。"
  info "请**留在这台机器前**：弹出来时点「允许」，授权就做完了。"
  info "准备好了吗？（要在这台机器前点弹窗；没准备好就选 n）"
  if zp_yes "确认现在申请外接盘授权（接下来你要在屏幕上点「允许」）？" "n"; then
    ZP_EXTERNAL_ACK=1
  else
    ZP_EXTERNAL_ACK=0
    warn "好 —— 这次不申请授权（不会触发任何弹窗）。"
    info "以后想用：到「面板 → 磁盘管理 → 申请授权」按一下即可（同样要点屏幕上的「允许」）。"
  fi
  return 0
}

# request_external_volume_auth：把"一次性授权请求"标记写给面板。
# 只在①真机安装 ②用户在"准备好了吗"那道确认上选了是 ③确有外接卷 ④有图形登录会话
# ⑤非沙箱/干跑 时写；守护进程只读一次（internal/files/volumeauth.go），绝不反复弹窗。
request_external_volume_auth() {
  local marker="$DATA_DIR/request-volume-auth.once"
  [ "${ZIZPANEL_SANDBOX:-0}" = "1" ] && return 0
  [ "${ZP_EXTERNAL_MODE:-}" = "local" ] || return 0
  [ "${ZP_EXTERNAL_ACK:-0}" = "1" ] || return 0

  if dry_run; then
    info "（干跑）真机安装且已确认：将写入一次性授权请求 $marker"
    return 0
  fi

  local console_user="" vols=""
  console_user="$(console_login_user)"
  case "$console_user" in
    ""|root|loginwindow)
      info "当前没有图形登录会话（控制台用户：${console_user:-无}）→ 不写授权请求。"
      info "原因：没人在屏幕前时绝不触发系统弹窗（弹了只会被记成拒绝）。"
      info "需要时请到真机前：系统设置 → 隐私与安全性 → 完全磁盘访问权限 → 加上 $BIN_DIR/zizpanel"
      return 0
      ;;
  esac
  vols="$(external_volume_list)"
  if [ -z "$vols" ]; then
    info "当前没有外接卷，跳过授权请求（以后插上盘再到真机上授权即可）。"
    return 0
  fi
  mkdir -p "$DATA_DIR" 2>/dev/null || true
  if printf '1\n' > "$marker" 2>/dev/null; then
    chmod 600 "$marker" 2>/dev/null || true
    ok "已记录一次性授权请求（${marker}）"
    info "面板启动后会替你申请一次外接盘授权：屏幕上弹出「…想要访问可移除宗卷上的文件」时点「允许」。"
    info "没点或错过了也不怕：到「面板 → 磁盘管理 → 申请授权」可以再来一次。"
  else
    warn "写授权请求失败：${marker}（请稍后在真机的系统设置里手动授权）"
  fi
  return 0
}

# ------------------------------------------------------- 是否开启 SSH --
setup_ssh_choice() {
  title "远程访问（SSH）"

  local tool="$ZIZPANEL_ROOT/server-mode.sh"
  if [ ! -x "$tool" ]; then
    local src_tool="$SCRIPT_DIR/tools/server-mode.sh"
    [ -x "$src_tool" ] && tool="$src_tool"
  fi

  # 🛑 2026-09-17 用户要求：安装界面不问 SSH（面板「系统设置 → 远程登录」点一下就能开）。
  # 只有显式要求（--ssh / ZP_SSH=1）时才动，供无人值守与回归测试。
  if [ -z "$ZIZPANEL_SSH" ]; then
    info "SSH 保持系统默认；需要时在面板「系统设置 → 远程登录」一键开启（安装脚本不再询问）。"
    return 0
  fi

  case "$ZIZPANEL_SSH" in
    1|on|yes) ;;
    0|off|no) info "已按 ZP_SSH/ZIZPANEL_SSH 的要求不开启 SSH"; return 0 ;;
    *) warn "无法识别的 ZP_SSH/ZIZPANEL_SSH 值：${ZIZPANEL_SSH}（按不开启处理）"; return 0 ;;
  esac

  if [ ! -x "$tool" ]; then
    warn "找不到 server-mode.sh，无法自动开启 SSH"
    warn "请在 系统设置 → 通用 → 共享 → 远程登录 手工打开"
    return 0
  fi

  info "开启 SSH（复用 tools/server-mode.sh --ssh-only，只动 SSH，不改电源/更新设置）…"
  if [ "$ZIZPANEL_DRY_RUN" = "1" ]; then
    info "（干跑）将执行：sudo bash $tool --ssh-only"
    return 0
  fi
  if bash "$tool" --ssh-only; then
    ok "SSH 已开启（22 端口正在监听）"
  else
    warn "SSH 自动开启失败。请在 系统设置 → 通用 → 共享 → 远程登录 手工打开"
  fi
  return 0
}

# ============================================================================
#  配置预写（面板后缀 / 监听端口）
#  面板的 Bootstrap 首次启动会随机生成后缀，所以这里先写好最小配置（只覆盖后缀/监听/
#  镜像基址/升级源），其余字段仍由 Bootstrap 补齐。已有配置 = 升级/重装，原样保留。
# ============================================================================

# zcfg_get <键>：从已有配置取一个字符串字段（不引入 jq/python）。模式里不给键名套双引号。
zcfg_get() {
  [ -f "$DATA_DIR/config.json" ] || return 0
  sed -n "s/.*$1[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$DATA_DIR/config.json" 2>/dev/null | head -1
}

# zp_json_escape：把 shell 字符串转义成 JSON 字面量（含首尾引号）。
# 刻意不用 python3：全新 macOS 上它是空壳，一调用就弹「需要命令行开发者工具」的 GUI 对话框。
# 纯 shell 转义反斜杠、双引号与 JSON 不允许的裸控制字符（\n \r \t）。
zp_json_escape() {
  local s="$1"
  # shellcheck disable=SC1003
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '"%s"' "$s"
}

# write_raw_config：把安装时确定的最小配置写进 config.json（走 sed 精确注入，不用 python3）。
write_raw_config() {
  if [ "$ZIZPANEL_DRY_RUN" = "1" ]; then
    info "（干跑）将写入 $DATA_DIR/config.json：panel_suffix=${PANEL_SUFFIX_INPUT} listen=${ZIZPANEL_LISTEN} upgrade_source=${PANEL_UPGRADE_SOURCE:-<未探测到可用源>}"
    return 0
  fi
  if [ -f "$DATA_DIR/config.json" ]; then
    local have
    have="$(zcfg_get panel_suffix)"
    [ -n "$have" ] && ok "已保留原有面板后缀（${have}）与配置（升级模式）"
    return 0
  fi

  mkdir -p "$DATA_DIR" 2>/dev/null || true

  # 四个字段：后缀、监听、镜像基址、升级源；升级源为空串也照写。
  # sed 精确替换、模式里不给键名套双引号（历史坑：会切断引号串导致 bash 语法错误）。
  local sed_inplace=(-i "")
  sed --version >/dev/null 2>&1 && sed_inplace=(-i)
  if [ ! -f "$DATA_DIR/config.json" ]; then
    printf '{\n  "panel_suffix": "%s",\n  "listen": "%s",\n  "mirror_base": "%s",\n  "upgrade_source": "%s"\n}\n' \
      "$PANEL_SUFFIX_INPUT" "$ZIZPANEL_LISTEN" "$DEFAULT_MIRROR_BASE" "$PANEL_UPGRADE_SOURCE" \
      > "$DATA_DIR/config.json"
  else
    sed "${sed_inplace[@]}" \
      -e "s|panel_suffix\([[:space:]]*:[[:space:]]*\)\"[^\"]*\"|panel_suffix\1\"$PANEL_SUFFIX_INPUT\"|" \
      -e "s|listen\([[:space:]]*:[[:space:]]*\)\"[^\"]*\"|listen\1\"$ZIZPANEL_LISTEN\"|" \
      -e "s|mirror_base\([[:space:]]*:[[:space:]]*\)\"[^\"]*\"|mirror_base\1\"$DEFAULT_MIRROR_BASE\"|" \
      "$DATA_DIR/config.json"
    if grep -q 'upgrade_source' "$DATA_DIR/config.json" 2>/dev/null; then
      sed "${sed_inplace[@]}" \
        -e "s|upgrade_source\([[:space:]]*:[[:space:]]*\)\"[^\"]*\"|upgrade_source\1\"$PANEL_UPGRADE_SOURCE\"|" \
        "$DATA_DIR/config.json"
    else
      # 补一行：插在 listen 那一行之后（没有 listen 就不插，避免写出坏 JSON）
      sed "${sed_inplace[@]}" \
        -e "/listen\([[:space:]]*:[[:space:]]*\)\"[^\"]*\",/a\\
  \"upgrade_source\": \"$PANEL_UPGRADE_SOURCE\",
" "$DATA_DIR/config.json"
    fi
  fi
  chmod 0600 "$DATA_DIR/config.json" 2>/dev/null || true
  [ "$(id -u)" -eq 0 ] && [ -n "$REAL_USER" ] && chown "${REAL_USER}:staff" "$DATA_DIR/config.json" 2>/dev/null
  ok "已写入面板后缀、监听端口、镜像基址与升级源"
  return 0
}

# ============================================================================
#  管理员账号（调用面板自己的 POST /api/v1/setup）：口令走 stdin（不进 argv/ps），已有账号绝不覆盖。
# ============================================================================
# panel_api_base：本机接口基址 —— **必须带面板后缀**（panelGate 对后缀之外一律 404，
# 真机实测：无后缀导致 setup/status 404，用户被推去浏览器再填一次账号）。
panel_api_base() {
  local sfx="$PANEL_SUFFIX_INPUT"
  if [ -f "$DATA_DIR/config.json" ]; then
    local have
    have="$(zcfg_get panel_suffix)"
    [ -n "$have" ] && sfx="$have"
  fi
  sfx="${sfx#/}"; sfx="${sfx%/}"
  if [ -n "$sfx" ]; then
    printf 'https://127.0.0.1:%s/%s' "$PANEL_PORT" "$sfx"
  else
    printf 'https://127.0.0.1:%s' "$PANEL_PORT"
  fi
}

create_admin_account() {
  title "创建管理员账号"
  if [ "$ZIZPANEL_DRY_RUN" = "1" ]; then
    if [ -n "$ADMIN_USERNAME" ]; then
      info "（干跑）将创建管理员账号：${ADMIN_USERNAME}（来自 ZP_USER/ZP_PASS）"
    else
      info "（干跑）不创建管理员账号：面板首次访问时由初始化向导创建"
    fi
    return 0
  fi

  local base st
  base="$(panel_api_base)"
  st="$(curl -fsSk --max-time 8 "$base/api/v1/setup/status" 2>/dev/null || true)"
  case "$st" in
    *'"needs_setup":false'*)
      ok "面板已有管理员账号，保持原样（升级模式，不覆盖账号与数据）"
      return 0
      ;;
    *'"needs_setup":true'*)
      ;;
    *)
      warn "读不到初始化状态（${st:-无响应}），跳过账号创建"
      warn "请用浏览器打开面板，在首次初始化向导里创建管理员账号。"
      return 0
      ;;
  esac

  if [ -z "$ADMIN_USERNAME" ] || [ -z "$ADMIN_PASSWORD" ]; then
    warn "没有可用的管理员用户名/口令，跳过；请用浏览器首次初始化向导创建账号"
    return 0
  fi

  # 口令放进 JSON 请求体走 stdin，不放 argv（argv 会出现在 ps 与审计里）。
  local jar="$TMP_DIR/setup-cookies.txt" out=""
  if out="$(printf '{"username":%s,"password":%s}' \
        "$(zp_json_escape "$ADMIN_USERNAME")" "$(zp_json_escape "$ADMIN_PASSWORD")" \
      | curl -fsSk --max-time 20 -c "$jar" -o - \
        -X POST -H 'Content-Type: application/json' --data-binary @- \
        "$base/api/v1/setup" 2>&1)"; then
    ok "管理员账号已创建：$ADMIN_USERNAME"
    return 0
  fi
  warn "创建管理员账号失败：$(printf '%s' "$out" | head -c 300)"
  warn "不改任何东西，请用浏览器打开面板，在首次初始化向导里创建账号。"
  return 0
}

# ============================================================================
#  「允许免授权访问内网段」（安装完成后的可选动作）
#  背景见 internal/sysconfig/lan_preauth.go（坑 #114/#115）；优先调用面板的
#  POST /api/v1/system/settings/lan-preauth（与界面同一条代码路径）。
# ============================================================================

# 三件事必须逐字说清；抽成函数是刻意的：选"是"与选"否"两条分支都要说到，分开写迟早漏一句。
lan_preauth_notice() {
  printf '\n'
  printf '  %s关于「允许免授权访问内网段」，请先看清三件事：%s\n' "$C_BOLD" "$C_RESET"
  printf '    ① 这个改动%s需要重启电脑才生效%s（重启前一切照旧）。\n' "$C_BOLD" "$C_RESET"
  printf '    ② %s不必现在重启%s——面板的 %sPlan B（回环转发器）%s已经能让局域网反代正常工作，\n' \
    "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET"
  printf '       重启可以等你方便的时候再做。\n'
  printf '    ③ 你以后也能在%s面板「系统设置」里手动打开%s这个选项。\n' "$C_BOLD" "$C_RESET"
  printf '\n'
}

# 调面板接口写预授权。成功（HTTP 2xx）返回 0，响应体写进 LAN_API_OUT。
LAN_API_OUT=""
lan_preauth_api_apply() {
  local cidrs="$1" jar="$TMP_DIR/lan-cookies.txt" token body base
  # 基址必须带面板后缀（panelGate 对后缀之外一律 404），与 create_admin_account 同源。
  base="$(panel_api_base)"
  rm -f "$jar"
  # 1) 登录拿会话（口令走 stdin 的 JSON，不放 argv）
  if ! curl -fsSk --max-time 10 -c "$jar" -o /dev/null \
      -X POST -H 'Content-Type: application/json' \
      --data-binary "$(printf '{"username":%s,"password":%s}' \
        "$(zp_json_escape "$ADMIN_USERNAME")" "$(zp_json_escape "$ADMIN_PASSWORD")")" \
      "$base/api/v1/login" 2>/dev/null; then
    return 1
  fi
  # 2) CSRF 双提交：从 cookie jar 里取 zp_csrf，放进 X-CSRF-Token 头
  token="$(awk '$6=="zp_csrf"{print $7}' "$jar" 2>/dev/null | tail -1)"
  [ -n "$token" ] || return 1
  # 3) 写入
  body="$(printf '{"enabled":true,"cidrs":%s}' "$(zp_json_escape "$cidrs")")"
  if LAN_API_OUT="$(curl -fsSk --max-time 20 -b "$jar" -o - \
      -X POST -H 'Content-Type: application/json' \
      -H "X-CSRF-Token: $token" \
      --data-binary "$body" \
      "$base/api/v1/system/settings/lan-preauth" 2>/dev/null)"; then
    return 0
  fi
  return 1
}

# 自动探测本机局域网网段（与 Go 侧 DetectPrimaryCIDR 同一思路：物理 en* 优先）。
lan_detect_cidr() {
  local out iface ip mask cidr
  if [ -x /sbin/ifconfig ]; then
    for iface in $(/sbin/ifconfig -l 2>/dev/null); do
      case "$iface" in lo*|utun*|awdl*|llw*|bridge*|ap*|anpi*|gif*|stf*|vmenet*|p2p*) continue ;; esac
      out="$(/sbin/ifconfig "$iface" 2>/dev/null || true)"
      ip="$(printf '%s\n' "$out" | awk '/inet /{print $2; exit}')"
      [ -n "$ip" ] || continue
      case "$ip" in 127.*) continue ;; esac
      mask="$(printf '%s\n' "$out" | awk '/inet /{for(i=1;i<=NF;i++) if($i=="netmask") print $(i+1); exit}')"
      [ -n "$mask" ] || continue
      # 0xffffff00 → 255.255.255.0（Apple 默认十六进制），也支持点分十进制
      case "$mask" in
        0x*)
          local hex="${mask#0x}"
          while [ ${#hex} -lt 8 ]; do hex="0${hex}"; done
          mask="$(printf '%d.%d.%d.%d' \
            "$(( 16#${hex:0:2} ))" "$(( 16#${hex:2:2} ))" \
            "$(( 16#${hex:4:2} ))" "$(( 16#${hex:6:2} ))")"
          ;;
      esac
      # 归一成"网络地址/前缀长度"：纯 shell 位运算，不用 python3（坑 152）。
      local o1 o2 o3 o4 m1 m2 m3 m4 bits=0 v
      IFS=. read -r o1 o2 o3 o4 <<<"$ip"
      IFS=. read -r m1 m2 m3 m4 <<<"$mask"
      v=$(( (m1 << 24) | (m2 << 16) | (m3 << 8) | m4 ))
      while [ "$v" -gt 0 ]; do
        bits=$(( bits + (v & 1) ))
        v=$(( v >> 1 ))
      done
      cidr="$(( o1 & m1 )).$(( o2 & m2 )).$(( o3 & m3 )).$(( o4 & m4 ))/$bits"
      [ -n "$cidr" ] && { printf '%s' "$cidr"; return 0; }
    done
  fi
  return 1
}

# 校验/规范化 CIDR 列表（逗号或空格分隔），输出规范化后的列表。
lan_normalize_cidrs() {
  local raw="$1"
  printf '%s' "$raw" | tr ',;' '  ' | tr -s ' \t' ' ' | sed 's/^ //; s/ $//'
}

# readback：写完必须读回来核对（与 Go 侧 ApplyLANPreauth 同一条纪律）。
lan_readback_ok() {
  local want="$1" sysplist="/Library/Preferences/com.apple.network.local-network"
  local user_plist="$REAL_HOME/Library/Preferences/com.apple.network.local-network"
  local sys_out="" user_out=""
  [ "$ZIZPANEL_DRY_RUN" = "1" ] && return 0
  sys_out="$(defaults read "$sysplist" AllowedEthernetLocalNetworkAddresses 2>/dev/null || true)"
  user_out="$(sudo -n -u "$REAL_USER" defaults read "$user_plist" AllowedEthernetLocalNetworkAddresses 2>/dev/null || true)"
  case "$sys_out" in *"$want"*) ;; *) return 1 ;; esac
  case "$user_out" in *"$want"*) ;; *) return 1 ;; esac
  return 0
}

# 非 API 兜底：直接按 Apple TN3179 写 defaults（与 lan_preauth.go 的命令形状一致）。
lan_preauth_defaults() {
  local cidrs="$1" sysplist="/Library/Preferences/com.apple.network.local-network"
  local user_plist="$REAL_HOME/Library/Preferences/com.apple.network.local-network"
  local c
  for c in $cidrs; do
    if ! defaults write "$sysplist" AllowedEthernetLocalNetworkAddresses -array $cidrs 2>/dev/null; then
      return 1
    fi
    if ! defaults write "$sysplist" AllowedWiFiLocalNetworkAddresses -array $cidrs 2>/dev/null; then
      return 1
    fi
    if ! sudo -n -u "$REAL_USER" defaults write "$user_plist" AllowedEthernetLocalNetworkAddresses -array $cidrs 2>/dev/null; then
      return 1
    fi
    if ! sudo -n -u "$REAL_USER" defaults write "$user_plist" AllowedWiFiLocalNetworkAddresses -array $cidrs 2>/dev/null; then
      return 1
    fi
    break  # 四个键一次写完整份列表，不需要按每个 CIDR 重复
  done
  return 0
}

# 应用预授权（默认什么都不做、也不问）。
# 🛑 2026-09-17 用户要求：这件事面板「系统设置 → 局域网访问」点一下就能做，且必须重启才生效；
# 交互流程里不再出现，只保留 ZP_LAN_PREAUTH=1 + ZP_LAN_CIDR 的自动化通路。
prompt_lan_preauth() {
  local decided="$ZIZPANEL_LAN_PREAUTH"
  if [ "$decided" != "1" ] && [ "$decided" != "true" ] && [ "$decided" != "yes" ]; then
    return 0
  fi

  title "局域网访问（按 ZP_LAN_PREAUTH=1 写入）"
  # 显式要求时才说清三件事，然后再动手
  lan_preauth_notice

  local cidrs="$LAN_CIDR_INPUT"
  if [ -z "$cidrs" ]; then
    cidrs="$(lan_detect_cidr || true)"
  fi
  if [ -z "$cidrs" ] && zp_input_ok; then
    # 默认留空：网段因机器而异，不许把作者家里的网段当默认值塞给用户。
    zp_ask "请填写要免授权的网段（CIDR）" "" cidrs
  fi
  cidrs="$(lan_normalize_cidrs "$cidrs")"
  if [ -z "$cidrs" ]; then
    warn "没能自动探测出局域网段，也没有可用的输入。"
    warn "请在面板「系统设置 → 局域网访问」里手工填写网段后打开（同样需要重启才生效）。"
    return 0
  fi
  info "要免授权的网段：$cidrs"

  if [ "$ZIZPANEL_DRY_RUN" = "1" ]; then
    info "（干跑）将调用面板接口 POST /api/v1/system/settings/lan-preauth（enabled=true, cidrs=${cidrs}）"
    info "（干跑）接口不可用时改用：defaults write /Library/Preferences/com.apple.network.local-network …"
    return 0
  fi

  # 1) 优先走面板**已有的**接口（与界面同一条代码路径）
  LAN_PREAUTH_APPLIED=0
  if lan_preauth_api_apply "$cidrs"; then
    ok "已通过面板接口写入预授权（$(printf '%s' "$LAN_API_OUT" | head -c 200)…）"
    if lan_readback_ok "${cidrs%%,*}"; then
      ok "读回复核通过：系统域与用户域都已写入"
      LAN_PREAUTH_APPLIED=1
    else
      warn "接口返回成功，但 defaults 读回复核没通过 —— 请以面板「系统设置」里的状态为准"
    fi
  else
    warn "面板接口不可用（可能还没起来或未登录），改用脚本内 defaults 直接写入"
    if lan_preauth_defaults "$cidrs" && lan_readback_ok "${cidrs%%,*}"; then
      ok "已写入预授权（系统域 + 用户域），读回复核通过"
      LAN_PREAUTH_APPLIED=1
    else
      warn "写入或复核失败。请在面板「系统设置 → 局域网访问」里手工打开（会显示真实状态与错误）"
      return 0
    fi
  fi

  printf '\n'
  printf '  %s请记住：这个改动要重启电脑才生效，但不急 —— Plan B 已经在工作。%s\n' "$C_BOLD" "$C_RESET"
  printf '  想马上生效就重启；不方便就先这样，有空再重启。\n'
  printf '\n'
  return 0
}

# --------------------------------------------------------------- 参数解析 --
# 支持命令行参数与环境变量（便于 curl | bash 场景）
parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --with-lnmp)
        ZIZPANEL_WITH_LNMP=1
        ;;
      --server-mode)
        ZIZPANEL_SERVER_MODE=1
        ;;
      --no-server-mode)
        ZIZPANEL_SERVER_MODE=0
        ;;
      --listen|--port)
        shift
        ZIZPANEL_LISTEN="${1:-:8443}"
        ZIZPANEL_LISTEN_EXPLICIT=1
        ;;
      --user)
        shift
        ADMIN_USERNAME="${1:-}"
        [ -n "$ADMIN_USERNAME" ] || die "--user 后面要跟管理员用户名"
        ;;
      --password)
        # 命令行参数会出现在 ps 与 shell 历史里；保留它是为了自动化回归。
        shift
        ADMIN_PASSWORD="${1:-}"
        [ -n "$ADMIN_PASSWORD" ] || die "--password 后面要跟登录口令"
        warn "在命令行上传口令会被 ps 与 shell 历史看到，自动化之外建议交互输入"
        ;;
      --suffix)
        shift
        PANEL_SUFFIX_INPUT="${1:-}"
        [ -n "$PANEL_SUFFIX_INPUT" ] || die "--suffix 后面要跟面板后缀"
        ;;
      --ssh)
        ZIZPANEL_SSH=1
        ;;
      --no-ssh)
        ZIZPANEL_SSH=0
        ;;
      --lan-preauth)
        ZIZPANEL_LAN_PREAUTH=1
        ;;
      --no-lan-preauth)
        ZIZPANEL_LAN_PREAUTH=0
        ;;
      --yes|-y)
        ZIZPANEL_YES=1
        ;;
      --dry-run)
        ZIZPANEL_DRY_RUN=1
        ;;
      --download-base|--mirror)
        # 国内直连 GitHub Releases 经常不通，升级/安装要能指向自建镜像。
        shift
        ZIZPANEL_DOWNLOAD_BASE="${1:-}"
        if [ -z "$ZIZPANEL_DOWNLOAD_BASE" ]; then
          die "--download-base 后面要跟地址，例如 https://zizdog.com/zizpanel"
        fi
        ZIZPANEL_DOWNLOAD_BASE="${ZIZPANEL_DOWNLOAD_BASE%/}"
        ;;
      -h|--help)
        sed -n '2,46p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
      *)
        # 未知参数不报错：安装脚本可能被以各种方式调用，宽容一些更实用
        ;;
    esac
    shift
  done
}

# ------------------------------------------------------------ 服务器模式 --
# 把系统配置成适合长期无人值守运行（关睡眠、断电自恢复、禁用自动更新重启）。
# 默认不开：这是改变系统行为的操作，用户应当明确选择。
setup_server_mode() {
  title "服务器模式配置"
  if [ "$ZIZPANEL_SERVER_MODE" != "1" ]; then
    info "未启用服务器模式。如果这台机器要长期当服务器用，建议执行："
    info "  sudo bash $ZIZPANEL_ROOT/server-mode.sh"
    info "它会关闭睡眠、开启断电自恢复（台式机）并开启 SSH 远程登录。"
    return 0
  fi

  local tool="$ZIZPANEL_ROOT/server-mode.sh"
  if [ ! -x "$tool" ]; then
    local src_tool="$SCRIPT_DIR/tools/server-mode.sh"
    if [ -x "$src_tool" ]; then
      tool="$src_tool"
    else
      warn "找不到 server-mode.sh，跳过服务器模式配置"
      return 0
    fi
  fi

  # 服务器模式默认开 SSH；不想开设 ZIZPANEL_NO_SSH=1 或回答 n。
  # 做成显式开关：开 SSH 会改变机器的网络暴露面，必须让用户能拒绝。
  local ssh_args=()
  if [ "${ZIZPANEL_NO_SSH:-0}" = "1" ] || [ "$ZIZPANEL_SSH" = "0" ]; then
    ssh_args+=(--no-ssh)
  fi
  bash "$tool" "${ssh_args[@]+"${ssh_args[@]}"}" \
    || warn "服务器模式配置部分失败（不影响面板使用）"
}

# ------------------------------------------------------------------- 主流程 --
main() {
  printf '\n%s╭──────────────────────────────────────────╮%s\n' "$C_BOLD" "$C_RESET"
  printf '%s│   ZizPanel · macOS 网站与服务管理面板     │%s\n' "$C_BOLD" "$C_RESET"
  printf '%s│   安装脚本 v%-28s│%s\n' "$C_BOLD" "$SCRIPT_VERSION" "$C_RESET"
  printf '%s╰──────────────────────────────────────────╯%s\n' "$C_BOLD" "$C_RESET"

  parse_args "$@"
  require_root
  check_macos
  resolve_real_user
  PANEL_PORT="$(panel_port)"

  # ---- 先问清楚（口径：环境变量 > 交互 > 默认值）----
  collect_basic_info

  # 外接硬盘与授权必须早问：用户还得有时间把外接盘插上（用户 2026-09-19 的铁律）。
  setup_external_volume_notice

  # 在线升级源探测放在最前面：只做几次 HEAD，很快，且结果要写进 config.json。
  title "面板在线升级源"
  upgrade_source_note

  # 选定安装来源。**必须在 dry_run 判断之外**：干跑也要看到选哪条路。
  # 曾误放进 dry_run 块里，导致 SOURCE_BIN 为空、报 `install: : No such file or directory`。
  detect_source

  if dry_run; then
    # 干跑必须把整条计划走完（SSH 提问与内网预授权提示都是用户要求的分支）。
    title "干跑：将要执行的动作"
    info "下载源候选（公网 zizdog.com 优先）：${ZIZPANEL_DOWNLOAD_BASE:+$ZIZPANEL_DOWNLOAD_BASE → }$BUILTIN_MIRROR → $GITHUB_RELEASE_BASE"
    info "镜像基址：${DEFAULT_MIRROR_BASE}（brew/CLT/应用包都挂它下面；公网镜像站）"
    install_deps
  fi

  case "$SOURCE_KIND" in
    local)    info "安装方式：本地二进制（离线）" ;;
    download) download_binaries ;;
    build)    build_from_source ;;
  esac

  # 后缀与监听端口先写进配置：面板启动时就不会再随机一个后缀，用户填的才会生效。
  write_raw_config

  install_binaries

  # 让签名证书在本机受信任：这是"用户授权一次、以后升级都不用再授"的前提。
  install_codesign_trust

  # 调刚装好的面板二进制把配置补齐并落盘，避免磁盘上出现"看起来缺一大半"的配置。
  # 老二进制没有这个子命令时如实说明，绝不假装补齐了。
  if ! dry_run; then
    if "$BIN_DIR/zizpanel" reconcile-config --config "$DATA_DIR/config.json" >/dev/null 2>&1; then
      info "配置字段已补齐：$DATA_DIR/config.json"
    else
      warn "本版本面板二进制不支持 reconcile-config，其余字段将在首次启动时于内存中补默认值"
      warn "（磁盘上暂时只有安装脚本写入的字段；升级到含该子命令的版本后会自动补齐）"
    fi
  fi

  install_sudoers
  # 让面板（守护进程）在启动时替用户申请一次外接盘授权：标记必须在启动**之前**写好。
  request_external_volume_auth
  install_daemon
  setup_nginx_env
  setup_cert
  setup_firewall
  takeover_legacy_panel
  setup_server_mode
  # 本机直接安装才问 SSH（远程 SSH 连过来时问它是多余的，见 is_remote_session）
  setup_ssh_choice
  write_uninstaller

  verify_running
  # 面板起来之后再建账号：走面板自己的初始化接口，不另建一套账号表
  create_admin_account
  # 安装收尾的可选动作：免授权访问内网段（选"是"才写入，且必须说清要重启）
  prompt_lan_preauth
  finish
  return 0
}

main "$@"
