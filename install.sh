#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 一键安装脚本（macOS）
#
#  用法（任意 Mac，无需先装任何东西）：
#     curl -fsSL <镜像地址>/install.sh | sudo bash
#     或者下载后：sudo bash install.sh
#
#  交互：有终端时会依次询问管理员用户名、登录口令、面板后缀、监听端口、
#        是否开启 SSH、是否开启「免授权访问内网段」（后两项按场景跳过）。
#        没有终端（curl | bash、CI、管道）或设了 ZP_YES=1 时不问任何问题，
#        一律走默认值/环境变量，绝不阻塞。
#
#  可用环境变量覆盖（ZP_* 是 ZIZPANEL_* 的简写别名，两者等价）：
#     ZP_USER / ZIZPANEL_ADMIN_USER        管理员用户名（默认：本机短名或 admin）
#     ZP_PASS / ZIZPANEL_ADMIN_PASSWORD    登录口令（不设则交互询问；无终端必须设）
#     ZP_SUFFIX / ZIZPANEL_PANEL_SUFFIX    面板路径后缀（默认随机 8 位）
#     ZP_PORT / ZIZPANEL_LISTEN            监听端口，默认 8443
#     ZP_YES=1                             全程不提问，全用默认值
#     ZP_SSH=1|0                           是否开启 SSH（不设时：本机安装才问）
#     ZP_LAN_PREAUTH=1|0                   是否开启「免授权访问内网段」（默认 0）
#     ZP_LAN_CIDR                          免授权网段，如 192.168.1.0/24（默认自动探测）
#     ZIZPANEL_DOWNLOAD_BASE               二进制下载源（默认走镜像，见下）
#     ZIZPANEL_MIRROR_BASE / ZIZPANEL_MIRROR_BASE_DEFAULT
#                                          镜像站基址（默认 https://mirror.zizdog.com:8888）
#     ZIZPANEL_SERVER_MODE=1               装完顺带配成服务器模式（含开 SSH）
#     ZIZPANEL_NO_SSH=1                    服务器模式里不开 SSH
#     ZIZPANEL_SKIP_DEPS=1                 跳过 Homebrew 依赖安装（含 ffmpeg）
#     ZIZPANEL_SKIP_FIREWALL=1             跳过防火墙处理
#     ZIZPANEL_DRY_RUN=1                   只打印将要做的事，不做任何修改
#
#  下载点（全部"镜像优先 + 探测不通再回落"，基址集中定义见下方 MIRROR_* 常量）：
#     · 面板二进制  → 镜像 /zizpanel/download/... → GitHub Releases（最后兜底）
#     · 应用包/brew → 镜像 /brew（NAS 优先）→ 国内公共镜像 → 官方源
#     · pip / HF / Docker 由面板侧按同一套镜像语义处理（不在本脚本内）
#
#  设计要点：
#   1. 幂等：重复执行不会破坏已有数据与配置，会保留 panel.db 与 config.json
#   2. 可离线：若同目录存在 dist/zizpanel 则直接本地安装，不走网络
#   3. 可自愈：任何一步失败都给出明确的下一步命令，而不是留下半装状态
# =============================================================================
set -uo pipefail

SCRIPT_VERSION="1.4.5"

# ----------------------------------------------------------------- 基础变量 --
ZIZPANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
ZIZPANEL_LISTEN="${ZIZPANEL_LISTEN:-}"
# ZIZPANEL_LISTEN_EXPLICIT=1 表示端口是"人给的"（ZP_PORT/--port），不是默认 8443 ——
# 只影响提示语（安装界面不再提问端口，用户要求"默认 8443 不提问"）。
ZIZPANEL_LISTEN_EXPLICIT=""
if [ -z "$ZIZPANEL_LISTEN" ] && [ -n "${ZP_PORT:-}" ]; then
  ZIZPANEL_LISTEN=":$ZP_PORT"
  ZIZPANEL_LISTEN_EXPLICIT=1
fi
ZIZPANEL_LISTEN="${ZIZPANEL_LISTEN:-:8443}"
# 本机访问路径：新面板会接管旧面板，并在该路径提供入口
PANEL_PATH="${ZIZPANEL_PANEL_PATH:-/_panel}"
ZIZPANEL_VERSION="${ZIZPANEL_VERSION:-latest}"
# 下载源（**镜像优先**，官方源只作最后兜底）：候选顺序见 detect_source。
# 为什么不在这里直接写镜像：镜像固定为下面两个常量，用户另有指定时才覆盖。
ZIZPANEL_DOWNLOAD_BASE="${ZIZPANEL_DOWNLOAD_BASE:-}"
# BUILTIN_MIRROR 是内置的国内镜像（同一个项目的自建源，即 NAS 的公网入口）。
#
# 为什么要有它：中国大陆无代理时 GitHub Release **完全不通**（2026-09 实测：
# 20 秒 0 字节），而"从 GitHub 下载"和"从源码构建"两条路都绕不开 GitHub。
# 有了它，用户只要能从任意一个地方拿到 install.sh，安装就能自动完成。
BUILTIN_MIRROR="${ZIZPANEL_BUILTIN_MIRROR:-https://zizdog.com/zizpanel}"
# 官方源（GitHub Releases）：**只做最后兜底**，国内直连通常会卡。
GITHUB_RELEASE_BASE="${ZIZPANEL_GITHUB_BASE:-https://github.com/zizdog/zizpanel/releases}"
# 服务器模式：安装面板的同时把系统配置成适合长期无人值守运行
# 可通过 --server-mode 参数或 ZIZPANEL_SERVER_MODE=1 开启
ZIZPANEL_SERVER_MODE="${ZIZPANEL_SERVER_MODE:-0}"
# --with-lnmp：安装时顺带装上 nginx/PHP/MySQL。
# 默认关闭，因为国内镜像下这三个包要装十几到几十分钟，
# 放在安装脚本里会长时间没有输出，容易让人以为卡死。
# 不传这个参数时面板仍可用，LNMP 可以在面板「应用市场」里逐个安装。
ZIZPANEL_WITH_LNMP="${ZIZPANEL_WITH_LNMP:-0}"

# ------------------------------------------------------------ 交互输入变量 --
# 全部支持 ZP_* 简写别名（用户原话里用的就是 ZP_*），两者等价。
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
# 说明：安装器**不再生成口令**（账号交给面板初始化向导）。
# 安装结果里必须显著打印一次，之后不再显示。
# PANEL_UPGRADE_SOURCE 是探测后真正写进 config.json 的在线升级源（main 里填）。
# 空串 = 公网与 NAS 都不可用：宁可不写，也不写一个探不通的地址。
PANEL_UPGRADE_SOURCE=""

# ---------------------------------------------------------- 基础依赖（ffmpeg）--

# ffmpeg 是"装了面板就该有"的基础环境，所以由安装脚本直接装上；
# 为什么是基础环境而不是某个应用的私有依赖，见 ensure_base_deps 的注释
# （2026-09-16 真机事故：它被弄丢后 TTS 合成返回 HTTP 200 + 0 字节 body，
#  所有作业全败而健康检查全绿）。
# 这份清单与面板 Go 侧 internal/services/basedep.go 的 baseDependencies 对应。
BASEDEP_FORMULAS=(ffmpeg)
# ============================================================================
#  镜像地址：**集中在这一处定义**（用户明确要求"放在镜像上的内容尽量统一"）。
#
#  换镜像只需要改这几个常量（或用同名环境变量覆盖），脚本里不许再出现
#  散落的镜像域名；探测/回落的顺序也由下面这几个数组唯一决定。
#
#  路径约定（与 NAS 上的实际布局、以及 Go 侧 mirror.go / homebrew_clt_mirror.go
#  的语义严格一致，不另造一套）：
#    <MIRROR>/zizpanel/download/<版本>/     面板发布包（Makefile: NAS_ROOT/zizpanel）
#    <MIRROR>/zizpanel/clt/index.json        Command Line Tools 清单与载荷
#    <MIRROR>/brew/api/formula/<f>.json      Homebrew bottles API
#    <MIRROR>/brew/v2/...                    Homebrew bottles（OCI 布局）
#    <MIRROR>/apps/<app>/<版本>/             应用市场安装包
#    <MIRROR>/pypi/  <MIRROR>/hf/            pip / HuggingFace（面板侧使用）
# ============================================================================

# DEFAULT_MIRROR_BASE 是自建 NAS 镜像的默认基址（与面板 config.DefaultMirrorBase 一致）。
# 它就是"指定的那一个镜像"：brew、CLT、应用包、面板发布件都挂在它下面。
DEFAULT_MIRROR_BASE="${ZIZPANEL_MIRROR_BASE_DEFAULT:-https://mirror.zizdog.com:8888}"
# 面板发布件在镜像上的子目录（NAS_ROOT 的对外前缀）。
MIRROR_PANEL_SUBDIR="/zizpanel"
# 局域网直连的 NAS 镜像（同城/RFC1918 内比公网入口快约 26 倍，见 AGENTS.md 四）。
# 放在候选里但**只探通就用**：不在同一局域网时探测会很快失败，不影响安装。
NAS_LAN_MIRROR="http://192.168.1.8:8090"

# 面板**在线升级源** —— 写进 config.json 的 `upgrade_source` 字段。
# 用户明确要求："以后升级探测不应该再是本机了，应该是公网的 zizdog.com"。
# 公网优先（任何网络都能用），NAS 局域网地址只作回落加速。
# 实测（2026-09-17）：https://zizdog.com/zizpanel/manifest.json (+.sig) → 200；
# NAS 上同一份在 <NAS>/zizpanel/manifest.json → 200，而 <NAS>/manifest.json → 404。
PANEL_UPGRADE_SOURCE_PUBLIC="${ZIZPANEL_UPGRADE_SOURCE:-https://zizdog.com/zizpanel}"
PANEL_UPGRADE_SOURCE_LAN="${NAS_LAN_MIRROR}${MIRROR_PANEL_SUBDIR}"
# 升级源是否可用以"清单 + 签名都在"为准（只有清单没有签名，面板会拒绝升级）。
UPGRADE_MANIFEST_PATH="/manifest.json"

# 国内公共 Homebrew 镜像（Go 侧 brewMirrorCandidates 同一份顺序与地址）。
BREW_PUBLIC_MIRRORS=(
  "中科大|https://mirrors.ustc.edu.cn/homebrew-bottles"
  "清华大学|https://mirrors.tuna.tsinghua.edu.cn/homebrew-bottles"
  "阿里云|https://mirrors.aliyun.com/homebrew/homebrew-bottles"
)
# Homebrew 官方安装脚本镜像。NAS 上**没有**这份脚本（实测 404），所以只能走
# 中科大镜像（与官方同步、实测 200/0.18s）或 GitHub raw。USTC 的 brew.git /
# homebrew-core.git 也在这里一起定义，避免散落在 setup_homebrew 里。
BREW_INSTALL_SCRIPT_MIRROR="https://mirrors.ustc.edu.cn/misc/brew-install.sh"
BREW_GIT_REMOTE_MIRROR="https://mirrors.ustc.edu.cn/brew.git"
BREW_CORE_GIT_REMOTE_MIRROR="https://mirrors.tuna.tsinghua.edu.cn/git/homebrew/homebrew-core.git"
# CLT（Command Line Tools）清单在镜像上的相对路径（cltMirrorSubdirs 同一约定）。
CLT_MIRROR_SUBDIRS=("" "/zizpanel")
# 用户**自己**在环境里设过的 brew 镜像必须尊重。必须在 setup_homebrew 之前抓一份：
# 那一步会在 GitHub 不可达时自己 export HOMEBREW_*，之后再去读就分不清
# "用户设的"和"脚本设的"了（见 choose_brew_mirror）。
ENV_HOMEBREW_API_DOMAIN="${HOMEBREW_API_DOMAIN:-}"
ENV_HOMEBREW_BOTTLE_DOMAIN="${HOMEBREW_BOTTLE_DOMAIN:-}"
PANEL_LABEL="cn.zizpanel.panel"
HELPER_NAME="zizpanel-helper"
BIN_DIR="$ZIZPANEL_ROOT/bin"
DATA_DIR="$ZIZPANEL_ROOT/data"
LOG_DIR="$ZIZPANEL_ROOT/logs"
RUN_DIR="$ZIZPANEL_ROOT/run"
WORK_DIR="$ZIZPANEL_ROOT/work"
# 系统路径允许覆盖：生产用默认值；自动化测试时指向沙箱目录，
# 这样可以在没有 root 的环境里跑完整安装流程做回归验证。
# nginx 前缀与 vhost 目录也允许覆盖：自动化测试需要把入口接管
# 隔离到沙箱，否则会改写生产环境的 nginx 配置。
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

# dry_run：干跑模式。所有"会真的改系统"的动作都要先过这一关。
# 放在最前面定义：它被大量函数使用，且实现必须只有一个（避免各处各写一套判断）。
dry_run() { [ "$ZIZPANEL_DRY_RUN" = "1" ]; }

cleanup() {
  [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ] && rm -rf "$TMP_DIR"
  return 0
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------- 前置条件检查 --
require_root() {
  # 沙箱模式：仅用于自动化测试，把系统路径都改到临时目录后无需 root。
  # 生产安装绝不走这个分支（ZIZPANEL_SANDBOX 不设置）。
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

  # 2) 联网下载预编译包
  #
  # 注意：这里探的是**真实的 tarball 地址**，不是目录 —— 目录 HEAD 在 nginx 下
  # 常常 403，会造成"明明镜像可用却判定不可达"。而且必须**逐个候选源都试一遍**：
  # 官方不通时直接走"源码构建"是错的（普通用户机器上根本没有 Go 工具链），
  # 正确做法是接着试下一个镜像。
  #
  # 顺序 = "镜像优先"：用户指定 → 自建 NAS 公网入口 → 局域网 NAS → GitHub。
  # GitHub 放在最后只作兜底：国内无代理时它 20 秒 0 字节（2026-09 实测）。
  local arch="arm64" base
  [ "$(uname -m)" = "x86_64" ] && arch="amd64"
  local file="zizpanel_${ZIZPANEL_VERSION}_darwin_${arch}.tar.gz"
  local -a bases=()
  if [ -n "$ZIZPANEL_DOWNLOAD_BASE" ]; then
    bases+=("${ZIZPANEL_DOWNLOAD_BASE%/}")
  fi
  bases+=("$BUILTIN_MIRROR" "${NAS_LAN_MIRROR}${MIRROR_PANEL_SUBDIR}" "$GITHUB_RELEASE_BASE")
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


# zp_curl_progress <输出文件> <URL> [总字节数]：带**可见进度**的下载。
#
# 为什么要自己轮询：curl 的 --progress-bar 只在 stderr 是终端时才画，而
# `curl -fsSL install.sh | sudo bash` 这种用法下用户实测"只看到一行说明、没有进度条"。
# 这里每秒打印一次"已下载 X MB"，与 TTY 无关，进度**一定看得见**。
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
    local -a fallbacks=("$BUILTIN_MIRROR" "${NAS_LAN_MIRROR}${MIRROR_PANEL_SUBDIR}" "$GITHUB_RELEASE_BASE")
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
  # 把「脚本目录」指向解压目录：包里带着 tools/（server-mode.sh、system-services.sh …），
  # 而 install_binaries 复制这些工具的前置是 `[ -d "$SCRIPT_DIR/tools" ]`。
  # **真机实测踩到**：`curl … | sudo bash` 时 BASH_SOURCE[0] 是 "bash" → dirname 是 "." →
  # SCRIPT_DIR 变成用户当前目录（如 /Users/zizdog），那里没有 tools/ → 整段复制被跳过 →
  # 面板装完后「远程登录」报"找不到 server-mode.sh"、「一键 LNMP」也拿不到 system-services.sh。
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
  ) || die "构建失败，请检查 Go 环境"
  SOURCE_BIN="$TMP_DIR/zizpanel"
  SOURCE_HELPER="$TMP_DIR/$HELPER_NAME"
  [ -x "$SOURCE_BIN" ] || die "构建命令返回成功，但 $SOURCE_BIN 不存在 —— 如实报为失败"
  [ -x "$SOURCE_HELPER" ] || die "构建命令返回成功，但 $SOURCE_HELPER 不存在 —— 如实报为失败"
  ok "构建完成"
}

# ------------------------------------------------------------ brew 调用助手 --
# Homebrew 明确拒绝以 root 运行：
#   "Error: Running Homebrew as root is extremely dangerous and no longer supported."
# 因此本脚本（以 sudo 运行时）所有 brew 查询都必须降权到真实用户执行，
# 否则会得到"所有组件都未安装"的错误结论。
has_brew() {
  # 注意：command 是 shell 内建命令，不能写成 `sudo -u user command -v`，
  # 因此直接判断可执行文件是否存在（Homebrew 的两个标准前缀）。
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
# Homebrew 官方安装脚本托管在 raw.githubusercontent.com。
# 国内网络下这个域名经常**完全不可达**（本机实测就是如此），
# 于是"一条命令装完"会在最开头就卡住，而用户看到的提示只是一个打不开的网址。
#
# 这里做三件事：
#   1. **镜像优先**：先探国内镜像的安装脚本（USTC，与官方同步）；
#      镜像不通才回落 GitHub 官方源（用户明确要求"放在镜像上的尽量统一"）
#   2. 同时把 brew/core 与 bottles 都指向镜像（NAS 优先，见 choose_brew_mirror）
#   3. 把镜像配置**写进用户的 shell 配置**，否则下次开终端又变回官方源
#
# 安全说明：镜像只替换"从哪里下载"，脚本内容与官方一致（USTC 同步自 GitHub）。
# 我们不做"从 gitee 拉第三方脚本"这种事 —— 那种脚本会以你的身份提权执行。
GITHUB_RAW_OK=""

# 探测 Homebrew 官方安装脚本是否可达（**只作为兜底候选**）。
# 为什么仍然保留：镜像偶尔会同步滞后或临时不可用，此时官方源是唯一出路。
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

  # 安装脚本：**镜像优先**。USTC 的 brew-install.sh 与官方同步（实测 200/0.18s），
  # 官方 raw.githubusercontent.com 只在前者不可达时兜底（国内经常完全不通）。
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

  # 无论走哪条安装脚本，brew/core 与 bottles 都指向国内镜像：
  # 瓶子（真正的下载量大头）用 NAS 优先，探不通再退各家公共镜像。
  # 这里探 ffmpeg（基础依赖，清单里一定有它），不探 git —— git formula
  # 在新版 Homebrew 里已不在 taps 清单，探它会得到"镜像不可用"的假结论。
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
  # 这里的重定向由**当前** shell 打开（安装脚本本身以 root 运行），
  # 写 /tmp 是故意的：日志要留给用户排障，同时不把它塞进用户家目录。
  # sudo 只影响它自己启动的命令，不影响重定向 —— 这正是我们要的行为。
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

# install_lnmp 实际安装 nginx / PHP / MySQL 并注册为后台服务。
#
# 只在明确要求时调用（--with-lnmp / ZIZPANEL_WITH_LNMP=1）：
# 国内镜像下这三个包要装十几到几十分钟，默认放进来会让"一条命令装完"
# 变成"一条命令卡住二十分钟不知道在干嘛"。
#
# 关键：brew 必须降权到真实用户执行（它明确拒绝 root），
# 而 `brew services start` 会把服务注册到**该用户**的 LaunchAgent 下 ——
# 这也是为什么必须指定用户，否则会装到 root 名下，用户登录后看不到。
install_lnmp() {
  local pkgs=("$@")
  [ ${#pkgs[@]} -gt 0 ] || return 0

  title "安装网站环境（nginx / PHP / MySQL）"
  warn "这一步需要下载较大的包，国内镜像下通常 10-40 分钟，请勿中断。"
  info "正在安装：${pkgs[*]}"

  local log="/tmp/zizpanel-lnmp-install.log"
  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，日志留给用户排障用
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

  # 注册为后台服务（开机自启）。失败不致命：面板仍能识别已安装的程序，
  # 只是需要用户手工启动，所以这里给出提示而不中断安装。
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

  # nginx 装好后，面板启动时会自动补齐 include 上下文与 WebSocket 升级映射；
  # 这里只提示，不去动它的配置（避免与面板的初始化流程打架）。
  info "面板启动后会自动补齐 nginx 的 include 上下文与 WebSocket 升级映射"
  return 0
}

# ---------------------------------------------------- 基础依赖（ffmpeg）安装 --
#
# 为什么 ffmpeg 属于"装了面板就该有"的基础环境，而不是等某个功能报错再补：
#   2026-09-16 真机事故：mini 被抹掉重装后，面板重装了 Qwen TTS，但整条安装链里
#   没有 ffmpeg。mlx_audio 编码 mp3 必须靠 ffmpeg —— 没有它时
#   POST /v1/audio/speech 仍然返回 **HTTP 200，但 body 是 0 字节**；网站的接收端
#   逐块 urlopen(...).read() 抛 IncompleteRead(0 bytes read)，用户**所有** TTS
#   作业全败，而面板上所有健康检查都是绿的（最难查的那种故障）。
#   手动 brew install ffmpeg 后立刻恢复。
#   它同时是后续音视频功能（转码、时长探测、缩略图）的公共前提，所以按基础环境管：
#   装面板时就装上，而不是等某个应用踩到才补。

# brew_api_ok <base> <formula>：4 秒内能拿到 formula 清单就算这家镜像可用。
#
# 只探清单、不探瓶路径是**刻意的**：瓶的路径布局各家不同（中科大提供 OCI 布局，
# 阿里云/清华 404），而 brew 在瓶取不到时会自行回落官方域 ——
# "不设"与"设一个取不到的"结果一样，只是白等一次探测。
brew_api_ok() {
  [ -n "${1:-}" ] || return 1
  curl -fsS --max-time 4 -o /dev/null \
    "${1%/}/api/formula/${2:-ffmpeg}.json" 2>/dev/null
}

# choose_brew_mirror [formula]：给这次 brew install 选镜像，选中结果写进
# HOMEBREW_API_DOMAIN / HOMEBREW_BOTTLE_DOMAIN 与 BREW_MIRROR_NAME。
#
# 优先级与面板 Go 侧的 probeBrewMirrors / brewMirrorCandidates 严格一致：
#   1. 自建 NAS 镜像的 <base>/brew 子路径 —— 用户明确要求"镜像优先"；
#      基址来源：ZIZPANEL_MIRROR_BASE → 面板已有配置里的 mirror_base → 内置默认；
#   2. 中科大 → 清华 → 阿里云（2026-09-16 实测中科大最快，阿里云只保证 API 可用）；
#   3. 都不通就**不设**，让 brew 回落官方源（比设一个取不到的好）。
# 返回 0 = 选中了镜像；1 = 都没通（调用方应如实说明走的是官方源）。
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

  # NAS 候选：先用户配置/内置默认，再局域网直连地址（同城快 26 倍，见 AGENTS.md）。
  # 全都**探通才用**；一个都不通就落到下面的公共镜像。
  local nas_candidates=()
  nas="${ZIZPANEL_MIRROR_BASE:-}"
  if [ -z "$nas" ] && [ -f "$DATA_DIR/config.json" ]; then
    # 升级/重装时沿用面板里配置过的镜像。只做一次最小的键值提取，
    # 不引入 python/jq 依赖（这一步可能发生在 CLT 还没装好的机器上）。
    nas="$(sed -n 's/.*"mirror_base"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      "$DATA_DIR/config.json" 2>/dev/null | head -1)"
  fi
  nas_candidates+=("${nas:-$DEFAULT_MIRROR_BASE}")
  [ -n "$nas" ] || nas_candidates+=("$NAS_LAN_MIRROR")
  for nas in "${nas_candidates[@]}"; do
    [ -n "$nas" ] || continue
    if brew_api_ok "$nas/brew" "$formula"; then
      HOMEBREW_API_DOMAIN="${nas%/}/brew/api"
      HOMEBREW_BOTTLE_DOMAIN="${nas%/}/brew"
      export HOMEBREW_API_DOMAIN HOMEBREW_BOTTLE_DOMAIN
      BREW_MIRROR_NAME="自建 NAS 镜像（${HOMEBREW_BOTTLE_DOMAIN}）"
      return 0
    fi
  done

  # 国内公共镜像：地址与顺序集中定义在文件头的 BREW_PUBLIC_MIRRORS
  # （与 Go 侧 brewMirrorCandidates 严格一致，别在两处各写一份）。
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
# 为什么要单独探一次：Homebrew 安装过程中会去装 CLT（576 MB + 55 MB），
# 那是整条安装链里最大的一笔下载。CLT 载荷放在镜像的
# <mirror>/zizpanel/clt/<产品号>/ 下（清单 <mirror>/zizpanel/clt/index.json，
# 与 Go 侧 cltMirrorSubdirs 同一约定；实测 <mirror>/clt/index.json 是 404）。
#
# install.sh **不自己装 CLT**（那件事由 Homebrew 安装器负责，它的安装脚本本身
# 已经走镜像）；这里只做两件事：
#   1) 判断镜像上到底有没有 CLT 清单，并把它报出来（"没有日志"本身就是信息）；
#   2) 把 <mirror>/zizpanel 作为 ZIZPANEL_CLT_MIRROR 写进 plist 环境变量 ——
#      面板 Go 侧的 cltMirrorBase() 读的正是这个变量，这样面板后续补装 CLT 时
#      用同一个基址，不必再对 404 的路径探一遍。
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
  elif [ -z "${ZIZPANEL_MIRROR_BASE:-}" ] && probe_clt_mirror "$NAS_LAN_MIRROR$MIRROR_PANEL_SUBDIR"; then
    ok "CLT 镜像可用（局域网 NAS）：${CLT_MIRROR_BASE}/clt/index.json"
  else
    warn "镜像上没有 CLT 清单：CLT 由 Homebrew 安装器负责（走 softwareupdate 或弹窗），"
    warn "  镜像路径：${base%/}/zizpanel/clt/index.json"
  fi
}

# ------------------------------------------------------------ 在线升级源 --
# 用户明确要求："以后升级探测不应该再是本机了，应该是公网的 zizdog.com。"
#
# 顺序：先用户显式指定的 ZIZPANEL_UPGRADE_SOURCE，再公网 zizdog.com，
# 最后局域网 NAS（NAS 只在同一局域网可达，所以只能作回落/加速）。
# 判据是"清单 + 签名都在"：只有清单没有签名时面板会拒绝升级，
# 把它当可用源写进配置，用户会看到一个查不出原因的失败。
probe_upgrade_source() {
  local cand
  for cand in "$PANEL_UPGRADE_SOURCE_PUBLIC" "$PANEL_UPGRADE_SOURCE_LAN"; do
    [ -n "$cand" ] || continue
    cand="${cand%/}"
    if curl -fsSI --max-time 8 "${cand}${UPGRADE_MANIFEST_PATH}" >/dev/null 2>&1 &&
       curl -fsSI --max-time 8 "${cand}${UPGRADE_MANIFEST_PATH}.sig" >/dev/null 2>&1; then
      PANEL_UPGRADE_SOURCE="$cand"
      return 0
    fi
  done
  PANEL_UPGRADE_SOURCE=""
  return 1
}

# upgrade_source_note：只读探测并如实汇报（探不到就说探不到，界面里再让用户填）。
upgrade_source_note() {
  if probe_upgrade_source; then
    if [ "$PANEL_UPGRADE_SOURCE" = "${PANEL_UPGRADE_SOURCE_PUBLIC%/}" ]; then
      ok "在线升级源：${PANEL_UPGRADE_SOURCE}（公网 zizdog.com，已确认清单与签名都在）"
    else
      warn "公网升级源不可达，回落局域网 NAS：${PANEL_UPGRADE_SOURCE}"
    fi
  else
    warn "公网与局域网升级源都没探通（面板仍可用，升级源留空，可在「面板设置 → 在线升级」里手工填写）"
  fi
}

# ensure_base_deps：幂等地装上缺失的基础依赖（目前只有 ffmpeg）。
#
# 两条纪律：
#   · **幂等**：已装就明确说"跳过"，不再 brew install 一遍；
#   · **如实**：失败必须报出来并给出可照抄的命令。ffmpeg 缺失恰好是
#     **不会自己报错**的那种故障（200 + 空 body），所以更不能在这里谎报成功。
# 返回 1 表示没装好（调用方决定是提示还是失败）。
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
  # 重定向由当前（root）shell 打开，日志留给用户排障（与 install_lnmp 一致）
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
    # CLT 是 Homebrew 的前置，也是整条链里最大的一笔下载：装它之前先探一次镜像，
    # 并把结论如实报出来（探不到就说探不到，绝不假装有镜像）。
    clt_mirror_note
    # 🛑 2026-09-17 用户明确要求（真机实测后）：**默认不在安装期装 Homebrew/CLT**。
    # 原因：CLT 的官方安装路径会弹 macOS 的「命令行开发者工具」GUI 对话框（要人点按钮、
    # 还要从 Apple 下载），把"一条命令装完"卡住 —— 而面板「基础环境」里**已经**有
    # 同一件事的完整实现（EnsureCLT 走 632MB 镜像分片 + EnsureHomebrew 走镜像，
    # 以 root 在任务中心里流式安装、不弹任何窗口）。
    # 所以这里默认跳过：唯一一份实现留在面板里，安装器只负责把面板装起来。
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

  # ---- 基础依赖（ffmpeg）：面板一装好就有，不留给某个功能按需补 ----
  # 放在 LNMP 之前：它小得多、且是 TTS 等功能的公共前提（用户原话：
  # "面板安装就应该安装 ffmpeg 这是基础环境"）。
  # 失败不中断安装（面板本身没有 ffmpeg 也能跑），但必须**如实**提示 ——
  # 缺它时的症状是"TTS 返回 200 + 空 body"，用户自己绝对查不出来。
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
    # 措辞核对过：面板的「应用市场 → 网站环境」里**确实**有 nginx/php/mysql 条目，
    # 所以这里可以放心把用户指过去。但国内网络下这三个包要装很久，
    # 放在安装脚本里会长时间没有输出（看着像卡死），所以默认不自动装。
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

  # 绝不允许"空源路径"走到 install(1)：那只会得到一句
  # `install: : No such file or directory`，把真正的失败原因（没找到可用的
  # 二进制来源）藏起来。这里显式挡住并说清试过哪些地址。
  if [ -z "$SOURCE_BIN" ] || [ ! -x "$SOURCE_BIN" ]; then
    err "没有可安装的面板程序（内部错误：来源为空或不可执行）。"
    err "已尝试的来源："
    err "  · 本地二进制：${SCRIPT_DIR}/dist、${SCRIPT_DIR}/../dist、${SCRIPT_DIR}"
    err "  · 下载：${ZIZPANEL_DOWNLOAD_BASE:-<未选定>} → $BUILTIN_MIRROR → ${NAS_LAN_MIRROR}${MIRROR_PANEL_SUBDIR} → $GITHUB_RELEASE_BASE"
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

  # 目录归属交给真实用户，便于用户直接查看日志/备份，也避免 root 独占。
  # 仅 root 需要做（沙箱模式本来就是该用户自己的文件）。
  if [ "$(id -u)" -eq 0 ]; then
    chown -R "${REAL_USER}:staff" "$ZIZPANEL_ROOT" 2>/dev/null || true
  fi
  chmod 0755 "$BIN_DIR/zizpanel" "$BIN_DIR/$HELPER_NAME"
  chmod 0700 "$DATA_DIR" "$DATA_DIR/tls" 2>/dev/null || true

  # 面板的运行时文件（日志、TLS 私钥）必须能被"实际跑面板的那个身份"打开。
  #
  # 真机事故：这套文件的归属在三条路径之间漂移 —— 安装脚本按真实用户 chown、
  # 面板进程按 plist 以 root 运行、用户又可能手工 chmod。一旦漂移，面板会在
  # **读到证书那一刻**直接退出，连一行日志都来不及写，launchd 只报
  # `last exit code = 78: EX_CONFIG`。从外面看就是"升级完面板凭空消失"，
  # 而日志是空的、完全无从查起（这一次真的发生了，回滚也没能救回来）。
  #
  # 这里在安装收尾把两件事一次做对：
  #   1) 日志目录与文件可写
  #   2) TLS 证书可读、私钥至少可被 root 读（0600）
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

# wait_service_stopped —— 等 launchd 真正把旧服务摘干净。
#
# 为什么必须等：`launchctl bootout` 是**异步**的，它一返回并不代表
# 服务已经卸载完成。此时立刻 bootstrap 会失败
# （Bootstrap failed: 5: Input/output error），
# 而 bootstrap 失败又让后面的 kickstart 无对象可踢 ——
# 结果是面板彻底不在了，用户只看到 http://localhost/_panel 502。
# 这不是理论风险：本机升级时真实发生过一次，靠手工 bootstrap 才恢复。
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

  # 服务从 launchd 消失 ≠ 进程已经退出，端口可能还被旧进程占着。
  # 新进程 bind 失败会立刻退出，KeepAlive 再拉起也是失败循环。
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
  # CLT 镜像项按"探到才写"处理：探不到就整项删掉（普通字符串替换，
  # 不用 sed -i —— 它的 GNU/BSD 两套写法容易踩坑）。
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
    # 兜底回滚：升级失败时宁可用回旧版本继续运行，
    # 也不能让用户连面板入口都没有。
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
  # 只授权 helper 这一个程序，且 helper 内部只做白名单操作。
  # 绝不授权 /bin/bash、/usr/bin/* 之类的通用命令。
  # 必须写临时文件再 mv：目标文件是 0440 只读，直接重定向会在重装时报 Permission denied
  local tmp_sudo="$SUDOERS_PATH.tmp.$$"
  cat > "$tmp_sudo" <<SUDOERS
# ZizPanel 面板提权授权（由 install.sh 生成，请勿手工编辑）
# 只允许面板用户免密调用受限助手，助手内部只执行白名单操作。
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
# 面板的反向代理功能依赖 WebSocket 升级 map（nginx 变量 connection_upgrade），
# 而 map 只能定义在 http 上下文，不能写在 server 块里。
# 因此这里确保 conf.d/upgrade-map.conf 存在，并确保 nginx.conf 的 http 块
# include 了 conf.d/*.conf —— 否则用户一建反代站点，nginx -t 就会报未知变量。
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

  # mkcert -install 会把 CA 写入系统钥匙串并要求管理员授权。
  # 在有图形界面的用户会话里会弹一次系统对话框；但在 SSH / 非交互会话里
  # 该步骤会一直阻塞（security add-trusted-cert 等待 GUI 授权，不会超时返回），
  # 从而把整个安装卡死。因此这里必须加超时，失败就回退自签证书。
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

  # 即使 CA 未能写入系统信任库，mkcert 依然可以签发证书。
  # 这种情况下浏览器仍会提示一次证书不受信任，但证书链比自签更规范，
  # 用户之后手工信任 mkcert 的 rootCA.pem 即可消除提示。
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
# 用真实 HTTP 请求确认面板确实在服务，而不是只相信 launchctl 报告 running。
# 这一步会发现"进程活着但端口没起来""TLS 证书错误"之类的问题。
verify_running() {
  title "校验面板是否可访问"
  if dry_run; then info "（干跑）将轮询 https://127.0.0.1:${PANEL_PORT}/api/v1/health 直到 200"; return 0; fi
  local port="${ZIZPANEL_LISTEN##*:}"
  local url="https://127.0.0.1:${port}/api/v1/health"
  # 诊断日志：探活失败时用户最需要知道"卡在哪一步"，而不是只看到"未就绪"
  # 注意：所有 local 声明必须在循环外。
  # 写在循环体内会导致每次迭代都重新声明并清空变量（bash 的 local 是函数作用域，
  # 但重新声明会重置值），后续引用就会触发 set -u 的 unbound variable。
  local diag="$LOG_DIR/install-check.log"
  local waited=0
  local st="" code="000" holder=""
  # 先确保日志目录存在，再往它里面写。
  # 真机/沙箱都踩过：升级路径下 logs/ 可能还不存在（第一次安装才建），
  # 于是这里的每一次 `>> "$diag"` 都失败并刷出同一行 `No such file or directory`
  # （30 秒轮询最多刷 30 行，把真正有用的诊断埋掉）。
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
    # 必须显式传 --config：安装脚本可能以 root 运行，
    # 而 status 的默认配置路径依赖 ZIZPANEL_ROOT 环境变量，不传会误报"未初始化"
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
  printf '    sudo %s/uninstall.sh                卸载（默认保留数据）\n' "$ZIZPANEL_ROOT"
  printf '\n'
  # 只有**显式要求**（ZP_LAN_PREAUTH=1）才写过预授权；写了就必须在最后再提醒一次重启
  # （真机反馈：之前提示只出现在中间那一步，装完的摘要里没有，用户以为装完就生效了）。
  if [ "${LAN_PREAUTH_APPLIED:-0}" = "1" ]; then
    printf '  %s⚠ 你要求写入的「免授权访问内网段」已写入，但要重启电脑才生效。%s\n' "$C_BOLD" "$C_RESET"
    printf '     重启前一切照旧（面板的 Plan B 回环转发器已经在工作），想生效就找时间重启一次。\n'
    printf '\n'
  fi
}

# ------------------------------------------- 接管旧面板（部署替换） --
# 目标：让新面板在原来习惯的访问路径（默认 http://localhost/_panel）直接可用，
# 同时把旧的简易面板目录改名保留（随时可回滚），而不是直接删掉。
#
# 为什么保留旧目录：升级第一原则是可回滚。旧面板只有几百 KB，
# 留着不占空间，却能在新面板出问题时立刻切回去。
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

  # 1) 把旧面板目录改名保留（只做一次）。
  #    升级第一原则是可回滚：旧面板只有几百 KB，
  #    留着不占空间，却能在新面板出问题时立刻切回去。
  if [ -d "$panel_dir" ] && [ -f "$panel_dir/index.php" ] && [ ! -d "$legacy_bak" ]; then
    if mv "$panel_dir" "$legacy_bak" 2>/dev/null; then
      ok "旧面板已备份为 _panel.legacy-bak（回滚：改回 _panel 并还原 000-default.conf）"
    else
      warn "旧面板目录移动失败，保持原样（新入口仍会生效）"
    fi
  fi

  # 2) 在默认站点里替换入口 location 块。
  #    具体的配置改写逻辑在 tools/takeover-panel-entry.sh 里，
  #    这样它既能安装时调用，也能单独执行与测试。
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
    # 关键：nginx -t 通过**不能**证明入口接管成功。
    # 如果 vhost 目录不存在、接管被跳过，nginx -t 照样通过 ——
    # 真机上就出现过"打印入口已生效、实际 http://localhost/_panel 打不开"。
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

  cat > "$ZIZPANEL_ROOT/uninstall.sh" <<'UNINSTALL'
#!/usr/bin/env bash
# ZizPanel 卸载脚本（由安装脚本生成）
#
# 默认保留网站数据与面板数据；加 --purge 才会一并删除。
set -uo pipefail
ROOT="/opt/zizpanel"
LABEL="cn.zizpanel.panel"

C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_RESET=$'\033[0m'

# 沙箱模式用于自动化测试；生产环境必须 root
if [ "${ZIZPANEL_SANDBOX:-0}" != "1" ]; then
  [ "$(id -u)" -eq 0 ] || { echo "${C_RED}请用 sudo 执行${C_RESET}"; exit 1; }
fi

PLIST_DIR="${ZIZPANEL_PLIST_DIR:-PLIST_DIR_PLACEHOLDER}"
SUDOERS_DIR="${ZIZPANEL_SUDOERS_DIR:-SUDOERS_DIR_PLACEHOLDER}"
LINK_DIR="${ZIZPANEL_LINK_DIR:-LINK_DIR_PLACEHOLDER}"
APPS_DIR="${ZIZPANEL_APPS_DIR:-APPS_DIR_PLACEHOLDER}"
ROOT="${ZIZPANEL_ROOT:-$ROOT}"

PURGE=0
for a in "$@"; do
  case "$a" in
    --purge|-p) PURGE=1 ;;
    -h|--help)
      echo "用法: sudo $0 [--purge]"
      echo "  --purge  同时删除面板数据（数据库、证书、日志）"
      exit 0 ;;
  esac
done

echo "正在停止面板服务…"
launchctl bootout "system/$LABEL" 2>/dev/null || true

# bootout 是异步的：命令返回时服务可能还没卸载完。
# 不等它就走，会出现"卸载了但端口还被旧进程占着"，
# 紧接着重装就会因为端口冲突起不来。最多等 10 秒。
for _ in $(seq 1 40); do
  launchctl print "system/$LABEL" >/dev/null 2>&1 || break
  sleep 0.25
done

echo "移除开机自启配置…"
rm -f "$PLIST_DIR/$LABEL.plist"
rm -f "$SUDOERS_DIR/zizpanel"
rm -f "$LINK_DIR/zizpanel" "$LINK_DIR/zizpanel-helper"

echo "移除防火墙条目…"
/usr/libexec/ApplicationFirewall/socketfilterfw --remove "$APPS_DIR/ZizPanel.app" >/dev/null 2>&1 || true
rm -rf "$APPS_DIR/ZizPanel.app"

if [ "$PURGE" = "1" ]; then
  echo "${C_YELLOW}删除面板数据目录 $ROOT …${C_RESET}"
  rm -rf "$ROOT"
  echo "${C_GREEN}已彻底卸载（网站数据与 nginx 配置未受影响）${C_RESET}"
else
  # 只删程序，保留数据，方便重装后继续用
  rm -f "$ROOT/bin/zizpanel" "$ROOT/bin/zizpanel-helper"
  echo "${C_GREEN}已卸载面板程序。${C_RESET}"
  echo "数据保留在 ${ROOT}（含数据库与证书）。"
  echo "如需彻底删除：sudo rm -rf $ROOT"
fi

echo
echo "注意：nginx / PHP / MySQL 与网站数据未被改动，仍可正常使用。"
UNINSTALL
  # 把安装时的真实路径写进卸载脚本。
  # macOS 的 BSD sed 用 `-i ""`，GNU sed 用 `-i`，两者不兼容，必须判断后用。
  local sed_inplace=(-i "")
  if sed --version >/dev/null 2>&1; then
    sed_inplace=(-i)
  fi
  sed "${sed_inplace[@]}" \
    -e "s|PLIST_DIR_PLACEHOLDER|$PLIST_DIR|g" \
    -e "s|SUDOERS_DIR_PLACEHOLDER|$SUDOERS_DIR|g" \
    -e "s|LINK_DIR_PLACEHOLDER|$LINK_DIR|g" \
    -e "s|APPS_DIR_PLACEHOLDER|$APPS_DIR|g" \
    "$ZIZPANEL_ROOT/uninstall.sh" 2>/dev/null || true
  chmod 0755 "$ZIZPANEL_ROOT/uninstall.sh"
}

# ============================================================================
#  交互输入（用户明确要求："充分考虑互动与交互环节"）
#
#  四条纪律：
#   1. **绝不在管道里卡住**：每个 read 都是"能读就读、读不到用默认"。
#      判据是 zp_input_ok（/dev/tty 可打开 + 没设 ZP_YES + 不是沙箱/干跑），
#      不是简单的 [ -t 0 ]（curl | bash 时 stdin 是管道，但用户其实有终端）。
#   2. 环境变量优先：设了就**不问**（脚本化安装必须一次也不阻塞）。
#   3. 口令永不写进命令行参数、永不落到日志/交互记录：走 stdin（setup --stdin）。
#   4. 非交互时缺必填项（管理员口令）就**明确报错**，绝不悄悄用一个默认口令。
# ============================================================================

# ZP_TTY_FD 是交互读入的文件描述符；打不开 /dev/tty 时为空。
ZP_TTY_FD=""
if { true >/dev/tty; } 2>/dev/null; then
  exec 3</dev/tty 2>/dev/null && ZP_TTY_FD=3
fi

# zp_input_ok：现在能不能问用户。
#
# 只取决于"有没有可用的终端"与"用户是否要求不提问"。
# **不**看沙箱/干跑：干跑也该照常提问（它只是不写系统），
# 沙箱测试的 stdin 是 /dev/null，问也读不到，会自动走默认值路径。
zp_input_ok() {
  [ "$ZIZPANEL_YES" = "1" ] && return 1
  [ -n "$ZP_TTY_FD" ] || return 1
  return 0
}

# zp_random_hex <字节数>：随机十六进制串（优先 /dev/urandom，退回 openssl）。
# 为什么不用 ${RANDOM}：bash 的 RANDOM 只有 15 位，拼出来的"安全后缀"可枚举。
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
# 返回 0 = 读到输入（空行也算，取默认值）
#      1 = 读到 EOF（终端/输入被关闭）
#      2 = 超时（用户在 180 秒内没有回答）
#
# 为什么要把 EOF 与超时分清楚：两者若都当成"取消"，
# 用户在真终端上敲一个回车就能把安装悄悄取消掉（真发生过）。
# 分开之后：超时按"接受默认值"处理，EOF 只在**非交互**场景下才终止。
zp_read() {
  local prompt="$1" def="$2" __var="$3" secret="${4:-0}" line="" rc=0 attempt=0
  local ttydev="/dev/tty" tty_saved=""
  if [ -n "$def" ]; then
    printf '%s%s%s %s[%s]%s: ' "$C_BOLD" "$prompt" "$C_RESET" "$C_YELLOW" "$def" "$C_RESET"
  else
    printf '%s%s%s: ' "$C_BOLD" "$prompt" "$C_RESET"
  fi
  # 重试循环：`read -t` 在真终端上会被信号打断（窗口大小变化、Ctrl-Z 之类），
  # 此时 bash 返回 >128 而**没有读到任何输入**。如果把它当成"超时/EOF"直接返回，
  # 用户就会看到"敲了回车但脚本跳到下一题/取消了" —— 这是必须避免的。
  # 处理方式：被打断就重试（最多 3 次），其它非零值才算 EOF。
  while :; do
    rc=0
    if [ "$secret" = "1" ]; then
      # 关回显**必须显式用 stty**：`read -s` 在 `-u <fd>` 下不生效
      # （本机 bash 3.2 只在读 stdin 时处理 echo），真机实测口令被明文回显。
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

# zp_ask <提示> <默认值> <变量名>：默认值可被回车接受；读不到就用默认值。
# 返回 zp_read 的码，让调用方自己决定"读不到"该怎么办。
zp_ask() {
  local prompt="$1" def="$2" __var="$3"
  zp_read "$prompt" "$def" "$__var" 0
}

# zp_confirm <提示> <默认 y|n>：返回 0=是，1=否，2=读不到（EOF）。
# 回车 = 默认；y/Y/yes/是 → 是；不认识的回答按默认（不反复追问，避免卡住）。
#
# 读不到（EOF）**不**当成"否"：调用方必须显式处理，
# 否则真终端上一敲回车就把安装取消了。非交互场景由调用方把 EOF 当"取消"。
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

# zp_yes <提示> <默认 y|n>：给"确实要问一句"的地方用。
# EOF（终端被关掉）按默认值处理 —— 交互场景下不该因为读不到就把整件事取消。
zp_yes() {
  local rc=0
  zp_confirm "$1" "${2:-y}" || rc=$?
  [ "$rc" -eq 0 ]
}

# zp_normalize_suffix：与 Go 侧 config.NormalizePanelSuffix 同一套规则
# （去斜杠与空白、只留小写字母数字下划线连字符、最长 32 位）。
# 放在安装脚本里做，是为了让用户**在装完之前**就知道后缀长什么样。
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
    # 语言/确认：默认中文，回车即继续。"英文"也接受 —— 但这轮只影响确认语，
    # 不做半套语言切换（半套比不切更容易误导）。
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

  # ---- 管理员账号：**安装器不再询问** ----
  #
  # 🛑 2026-09-17 用户明确要求（真机安装后）："既然面板初始访问要设置用户名和密码，
  # 安装过程就不要输入了！" 之前的做法确实是重复的：安装时问一次、打开面板初始化向导又问一次
  # （真机上那次安装期创建还因为接口缺后缀 404 而没生效，于是用户连着填了两遍）。
  # 现在：**默认什么都不问、也不生成口令** —— 账号统一由面板的首次初始化向导创建。
  # 只有**显式提供**（ZP_USER/ZP_PASS 或 --user/--password）时才在安装期建账号，供自动化/CI 用。
  if [ -n "$ADMIN_PASSWORD" ] && [ -z "$ADMIN_USERNAME" ]; then
    ADMIN_USERNAME="admin"
  fi
  if [ -n "$ADMIN_USERNAME" ] && [ -z "$ADMIN_PASSWORD" ]; then
    die "给了管理员用户名（ZP_USER/--user）就必须同时给口令（ZP_PASS/--password）"
  fi
  if [ -z "$ADMIN_USERNAME" ]; then
    info "管理员账号留给面板：装好后打开面板，首次访问会让你设置用户名与口令（安装器不再询问）。"
  fi
  # 提前校验：口令太短会让面板在**装完之后**才报错，用户得重装一遍。
  # 这里跟 auth.CreateUser 的规则保持一致（这条长度门禁可在这里挡住）。
  if [ -n "$ADMIN_PASSWORD" ] && [ "${#ADMIN_PASSWORD}" -lt 8 ]; then
    die "登录口令至少 8 位（当前 ${#ADMIN_PASSWORD} 位）"
  fi

  # ---- 面板路径后缀（安全入口）：**安装器不再询问** ----
  #
  # 🛑 2026-09-17 用户明确要求："后缀功能也不要在安装时要求，用户可以在后台选择是否开启后缀安全加强。"
  # 所以默认**不设后缀**（面板直接在 / 提供服务），装好后在面板「系统设置」里随时能开启
  # （views.js 里那句"留空 = 不启用安全入口"就是同一个开关，改完会把新地址摆到用户眼前）。
  # 升级/重装时**保留原有后缀**（用户可能已经在面板里设过）；只有显式给了
  # ZP_SUFFIX / --suffix 才用传入值（自动化通路）。
  if [ -f "$DATA_DIR/config.json" ] && [ -z "$PANEL_SUFFIX_INPUT" ]; then
    PANEL_SUFFIX_INPUT="$(zcfg_get panel_suffix)"
  fi
  if [ -n "$PANEL_SUFFIX_INPUT" ]; then
    PANEL_SUFFIX_INPUT="$(zp_normalize_suffix "$PANEL_SUFFIX_INPUT")"
    info "面板路径后缀（安全入口）：${PANEL_SUFFIX_INPUT}（来自已有配置或 ZP_SUFFIX）"
  else
    info "面板路径后缀未启用（安全加强默认关闭）：装好后可在面板「系统设置」里随时开启。"
  fi

  # ---- 监听端口：**不问，默认 8443** ----
  #
  # 🛑 2026-09-17 用户明确要求："默认 8443 不提问"。安装界面到此**没有任何技术提问**。
  # 想换端口：装之前用 ZP_PORT=9000 或 --port 9000；装之后改
  # `/opt/zizpanel/data/config.json` 里的 `listen` 再 `sudo launchctl kickstart -k system/cn.zizpanel.panel`
  # （面板运行中改端口会立刻失联，所以设置页刻意不提供这个开关 —— 见 server 的 handleSaveSettings 注释）。
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
# 判据（用户明确要求）：
#   · 通过 SSH 从别的电脑连过来（SSH_CONNECTION/SSH_CLIENT/SSH_TTY 非空）
#     → 不要再问"是否开启 SSH"：远程访问本来就通着；
#   · 在本机直接跑（Terminal.app / iTerm / 系统镜像终端）→ 才问。
#
# 为什么要多个信号：`curl | sudo bash` 里 sudo 会重置一部分环境，
# 单看 SSH_CONNECTION 在某些配置下会漏判。所以再加两条"本机"正向证据：
#   · TERM_PROGRAM / __CFBundleIdentifier —— macOS 终端 App 才会设；
#   · 当前用户 == 控制台登录用户（本机图形登录），而 SSH 会话通常是别人。
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

# ------------------------------------------------------- 是否开启 SSH --
setup_ssh_choice() {
  title "远程访问（SSH）"

  local tool="$ZIZPANEL_ROOT/server-mode.sh"
  if [ ! -x "$tool" ]; then
    local src_tool="$SCRIPT_DIR/tools/server-mode.sh"
    [ -x "$src_tool" ] && tool="$src_tool"
  fi

  # 🛑 2026-09-17 用户明确要求：**安装界面不问 SSH**。
  # 真机实测：面板「系统设置 → 远程登录」点一下就能开（成功），安装期再问一次是多余的步骤。
  # 只有显式要求时才动（`--ssh` / `ZP_SSH=1`），供无人值守安装与回归测试使用。
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
#
#  为什么安装脚本要自己写 config.json：面板的 config.Bootstrap 会在首次启动时
#  随机生成后缀 —— 那样用户在安装时填的后缀就白填了，而且没人知道随机值是什么。
#  这里先把最小配置写好（只覆盖后缀与监听），其余字段（secret/install_id/
#  各类路径）仍然由面板启动时的 Bootstrap 补齐，绝不与它打架。
#  已有配置 = 升级/重装：**原样保留**，一个字都不覆盖。
# ============================================================================

# zcfg_get <键>：从已有配置里取一个字符串字段（不引入 jq/python 依赖）。
#
# 模式里**不**给键名套双引号（写成 ["]\{0,1\} 形式太脆）—— 直接匹配键名即可：
# 键名不会出现在别处（例如 mirror_base 与 "mirror_base" 只差引号，都匹配得到）。
# 历史坑：写成 "s/.*"$1"[[:space:]]..." 时，$1 前后的双引号会把整段引号串切断，
# 后面的 \( 变成未加引号的分组括号，bash 直接报语法错误（整脚本跑不起来）。
zcfg_get() {
  [ -f "$DATA_DIR/config.json" ] || return 0
  sed -n "s/.*$1[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$DATA_DIR/config.json" 2>/dev/null | head -1
}

# zp_json_escape：把 shell 字符串转义成 JSON 字符串字面量（含首尾引号）。
# **刻意不用 python3**：全新 macOS 上 /usr/bin/python3 是个空壳，一调用就弹
# 「python3 命令需要使用命令行开发者工具，你要现在安装该工具吗？」的 GUI 弹窗 ——
# 真机实测把安装卡在一个需要人去点按钮的对话框上（而且它下载的是 Apple 的 CLT，国内很慢）。
# 用纯 shell 转义：反斜杠、双引号、以及 JSON 不允许的裸控制字符（\n \r \t）。
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

# write_raw_config：把"安装时确定的最小配置"写进 config.json。
# **不用 python3**（见上面 zp_json_escape 的说明）：这里走 sed 精确注入，
# 只碰后缀/监听/镜像基址/升级源四个键，其它字段一律不动。
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

  # 四个字段：面板后缀、监听、镜像基址、在线升级源。
  # 升级源为空串也照写（空 = 面板里没配升级源，界面会让用户自己填）；
  # 关键是**不写错** —— 写一个探不通的地址会让用户在升级页看到莫名其妙的失败。
  # 纯 shell 写配置：sed 精确替换（只碰这四个键，不动其它字段）。
  # 注意：模式里**不给键名套双引号** —— 键名已足够唯一，套引号会把整段
  # 引号串切断（历史坑：\( 变成未加引号的分组括号，bash 直接语法错误）。
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
#  管理员账号（用面板**已有的** POST /api/v1/setup 接口创建）
#
#  注意：这一步不在安装脚本里另建账号表 —— 只调用面板自己的初始化接口，
#  口令通过 stdin 传给 `zizpanel setup --stdin`（不出现在 argv、不进 ps 输出）。
#  已有账号（升级/重装）时**绝不覆盖**：那是用户的数据。
# ============================================================================
# panel_api_base：本机面板接口的基址 —— **必须带面板后缀**。
#
# 为什么单独抽一个函数：`internal/web/server.go` 的 panelGate 对 `/<后缀>/` 之外的
# 一切路径返回 **404**（只放行 `/api/v1/health` 与 `/api/v1/ping`，那是升级看门狗
# 与外部探活用的）。**真机实测踩到**：这里原来写 `https://127.0.0.1:<端口>`，
# 于是 setup/status 与 setup 都 404 → 脚本判定"读不到初始化状态" → 把用户推给
# 浏览器的初始化向导 → 用户看到的是"安装时填过一次账号，打开面板又要填一次"。
# 升级/重装时后缀以**已有配置**为准（用户可能在面板里改过后缀）。
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

  # 口令放进 JSON 请求体（走 stdin 交给 curl），**不放 argv** ——
  # argv 会出现在 ps 输出与审计里。
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
#
#  背景见 internal/sysconfig/lan_preauth.go（坑 #114/#115，两台机器真机验证）：
#  macOS 15 的「本地网络」隐私门会拦 nginx 的局域网反代；官方另有一扇可编程
#  预授权门（com.apple.network.local-network 域的两个键）。面板「系统设置」里
#  已经有这个开关，本脚本只是把同一件事搬到安装收尾，并**优先调用面板的
#  HTTP 接口**（POST /api/v1/system/settings/lan-preauth，与界面同一条代码路径）。
# ============================================================================

# 三件事必须**逐字**说清（用户原话的第 3 条）。
# 抽成一个函数是刻意的：选"是"与选"否"两条分支都要说到，
# 分成两处手写，迟早有一处漏一句。
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

# 调面板接口写预授权。成功（HTTP 2xx）返回 0 并把响应体写进 LAN_API_OUT。
# 面板侧的实现见 internal/web/api_systemsettings_lan.go（与界面同一条路径）。
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

# 自动探测本机局域网网段（与 Go 侧 DetectPrimaryCIDR 同一思路：物理 en* 优先，
# 只用 ipaddress 做"网络地址归一"这一步 —— 手写位运算在 shell 里容易错）。
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
      # 归一成"网络地址/前缀长度"：**纯 shell 位运算**，不用 python3
      # （python3 在全新 macOS 上会弹「命令行开发者工具」对话框，见坑 152）。
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

# readback：写完必须读回来核对（与 Go 侧 ApplyLANPreauth 同一条纪律：
# 读不回来 / 对不上就不许说"成功"）。
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

# 非 API 兜底：直接按 Apple TN3179 写 defaults（与 lan_preauth.go 的
# lanTargets/lanCommand 同一份命令形状：系统域用显式 plist 路径，用户域降权写）。
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

# 应用预授权（**默认什么都不做、也不问**）。
#
# 🛑 2026-09-17 用户明确要求："安装界面不需网络授权步骤了"。
# 理由站得住：这件事在面板「系统设置 → 局域网访问」里点一下就能做（同一条
# lan_preauth 代码路径），而且**必须重启才生效** —— 放在安装收尾既打断安装，
# 又容易让人以为"装完就生效了"（真机实测：用户选了"是"，装完却没看到重启提示）。
# 所以交互流程里**不再出现这一步**；只保留一条**纯环境变量**的自动化通路
# （ZP_LAN_PREAUTH=1 + ZP_LAN_CIDR=…），供无人值守安装与回归测试使用。
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
    zp_ask "请填写要免授权的网段（CIDR）" "192.168.1.0/24" cidrs
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
# 支持 --server-mode 等参数，也支持环境变量（便于 curl | bash 场景）
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
        # 注意：命令行参数会出现在 ps 输出与 shell 历史里。
        # 保留它是为了自动化安装（沙箱/真机回归），人工安装请用交互输入或 ZP_PASS。
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
        # 国内直连 GitHub Releases 经常很慢甚至不通，所以升级/安装都要能指向自建镜像。
        # 用法：curl -fsSL <镜像>/install.sh | sudo bash -s -- --download-base <镜像>
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
# 把系统配置成适合长期无人值守运行：关睡眠、断电自恢复、禁用自动更新重启。
#
# 为什么默认不开：这是"改变系统行为"的操作。装面板不该顺手改掉用户的电源设置，
# 用户应当明确选择。但如果是当服务器用，强烈建议开启。
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

  # 服务器模式默认会开启 SSH。若不想开，设 ZIZPANEL_NO_SSH=1，
  # 或者在交互里对"是否开启 SSH"回答 n（那会把 ZIZPANEL_SSH=0）。
  # 之所以做成显式开关：开 SSH 会改变这台机器的网络暴露面，
  # 这种事必须让用户能拒绝，而不是替他决定。
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

  # ---- 先问清楚（口径：环境变量 > 交互 > 默认值） ----
  # 放在最前面：用户还没等太久就拿到反馈，且口令/后缀有问题时不会白装一半。
  collect_basic_info

  # 在线升级源的探测放在最前面：它只做几次 HEAD 请求，很快，
  # 而且结果要写进 config.json（write_raw_config 会用它）。
  title "面板在线升级源"
  upgrade_source_note

  # 选定安装来源（本地 dist / 镜像下载 / 源码构建）。
  # **必须在 dry_run 判断之外**：干跑也要看到它会选哪条路，
  # 真正安装更是全靠这一步。曾经误放进 dry_run 块里，导致真实安装
  # 时 SOURCE_KIND 为空、SOURCE_BIN 为空，最后报出
  # `install: : No such file or directory`（排障时极难看出根因）。
  detect_source

  if dry_run; then
    # 干跑必须**把整条计划走完**（不是只打印几行就返回）：
    # SSH 提问与「免授权访问内网段」提示都是用户明确要求的分支，
    # 干跑的意义就是能在不碰系统的前提下把它们演练一遍。
    title "干跑：将要执行的动作"
    info "下载源候选（公网 zizdog.com 优先）：${ZIZPANEL_DOWNLOAD_BASE:+$ZIZPANEL_DOWNLOAD_BASE → }$BUILTIN_MIRROR → ${NAS_LAN_MIRROR}${MIRROR_PANEL_SUBDIR} → $GITHUB_RELEASE_BASE"
    info "镜像基址：${DEFAULT_MIRROR_BASE}（brew/CLT/应用包都挂它下面；公网优先，NAS 只作回落加速）"
    install_deps
  fi

  case "$SOURCE_KIND" in
    local)    info "安装方式：本地二进制（离线）" ;;
    download) download_binaries ;;
    build)    build_from_source ;;
  esac

  # 把后缀与监听端口先写进配置：面板启动时就不会再随机一个后缀出来，
  # 用户填的东西才会真的生效（已有配置一律保留，见 write_raw_config）。
  write_raw_config

  install_binaries

  # 补齐配置：预写的那份只有 4 个键，其余默认值（www_root / access_mode 等）
  # 只在 Go 侧定义。这里调**刚装好的**面板二进制把它补齐并落盘，避免磁盘上
  # 出现"看起来缺一大半"的配置（也避免安装脚本里手抄一份默认值）。
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
