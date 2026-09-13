#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 一键安装脚本（macOS）
#
#  用法（任意 Mac，无需先装任何东西）：
#     curl -fsSL https://你的地址/install.sh | sudo bash
#     或者下载后：sudo bash install.sh
#
#  可用环境变量覆盖：
#     ZIZPANEL_DOWNLOAD_BASE   二进制下载地址前缀（默认 GitHub Releases）
#     ZIZPANEL_VERSION          指定版本，如 0.1.0（默认 latest）
#     ZIZPANEL_ROOT             安装根目录（默认 /opt/zizpanel）
#     ZIZPANEL_LISTEN           面板监听地址（默认 :8443）
#     ZIZPANEL_SKIP_DEPS=1      跳过 Homebrew 依赖安装
#     ZIZPANEL_SKIP_FIREWALL=1  跳过防火墙处理
#
#  设计要点：
#   1. 幂等：重复执行不会破坏已有数据与配置，会保留 panel.db 与 config.json
#   2. 可离线：若同目录存在 dist/zizpanel 则直接本地安装，不走网络
#   3. 可自愈：任何一步失败都给出明确的下一步命令，而不是留下半装状态
# =============================================================================
set -uo pipefail

SCRIPT_VERSION="0.1.0"

# ----------------------------------------------------------------- 基础变量 --
ZIZPANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
ZIZPANEL_LISTEN="${ZIZPANEL_LISTEN:-:8443}"
# 本机访问路径：新面板会接管旧面板，并在该路径提供入口
PANEL_PATH="${ZIZPANEL_PANEL_PATH:-/_panel}"
ZIZPANEL_VERSION="${ZIZPANEL_VERSION:-latest}"
# 下载源：默认 GitHub Releases；可换成自己的服务器
ZIZPANEL_DOWNLOAD_BASE="${ZIZPANEL_DOWNLOAD_BASE:-https://github.com/zizdog/zizpanel/releases}"
# 服务器模式：安装面板的同时把系统配置成适合长期无人值守运行
# 可通过 --server-mode 参数或 ZIZPANEL_SERVER_MODE=1 开启
ZIZPANEL_SERVER_MODE="${ZIZPANEL_SERVER_MODE:-0}"
# --with-lnmp：安装时顺带装上 nginx/PHP/MySQL。
# 默认关闭，因为国内镜像下这三个包要装十几到几十分钟，
# 放在安装脚本里会长时间没有输出，容易让人以为卡死。
# 不传这个参数时面板仍可用，LNMP 可以在面板「应用市场」里逐个安装。
ZIZPANEL_WITH_LNMP="${ZIZPANEL_WITH_LNMP:-0}"
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
  if curl -fsSI --max-time 10 "$ZIZPANEL_DOWNLOAD_BASE" >/dev/null 2>&1; then
    SOURCE_KIND="download"
    return 0
  fi

  # 3) 用 Go 从源码构建
  if command -v go >/dev/null 2>&1; then
    SOURCE_KIND="build"
    return 0
  fi

  err "无法获取面板程序，请选择其一："
  err "  1) 把 dist/ 目录与 install.sh 放在一起（离线安装）"
  err "  2) 检查网络能否访问 $ZIZPANEL_DOWNLOAD_BASE"
  err "  3) 安装 Go 后重试：brew install go"
  exit 1
}

download_binaries() {
  TMP_DIR="$(mktemp -d)"
  local arch="arm64"
  [ "$(uname -m)" = "x86_64" ] && arch="amd64"
  local file="zizpanel_${ZIZPANEL_VERSION}_darwin_${arch}.tar.gz"
  local url="$ZIZPANEL_DOWNLOAD_BASE/download/$ZIZPANEL_VERSION/$file"

  info "下载：$url"
  if ! curl -fL --progress-bar --max-time 300 -o "$TMP_DIR/pkg.tar.gz" "$url"; then
    die "下载失败。请检查网络，或改用离线安装（把 dist/ 与 install.sh 放在同一目录）"
  fi
  if ! tar -xzf "$TMP_DIR/pkg.tar.gz" -C "$TMP_DIR"; then
    die "安装包解压失败，文件可能不完整"
  fi
  SOURCE_BIN="$TMP_DIR/zizpanel"
  SOURCE_HELPER="$TMP_DIR/$HELPER_NAME"
  [ -x "$SOURCE_BIN" ] || die "安装包中缺少 zizpanel 可执行文件"
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
#   1. 先探测 GitHub 是否可达，可达就走官方源（最可信）
#   2. 不可达时改用国内镜像：安装脚本走 USTC 镜像，
#      并同时把 brew/core 与 bottles 都指向镜像
#   3. 把镜像配置**写进用户的 shell 配置**，否则下次开终端又变回官方源
#
# 安全说明：镜像只替换"从哪里下载"，脚本内容与官方一致（USTC 同步自 GitHub）。
# 我们不做"从 gitee 拉第三方脚本"这种事 —— 那种脚本会以你的身份提权执行。
GITHUB_RAW_OK=""

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
    printf 'export HOMEBREW_API_DOMAIN="%s"\n' "$BREW_API_DOMAIN"
    printf 'export HOMEBREW_BOTTLE_DOMAIN="%s"\n' "$BREW_BOTTLE_DOMAIN"
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

  probe_github_raw
  local installer_url
  if [ "$GITHUB_RAW_OK" = "1" ]; then
    info "GitHub 可达，使用 Homebrew 官方安装源"
    installer_url="https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"
    BREW_API_DOMAIN=""; BREW_BOTTLE_DOMAIN=""
  else
    info "GitHub 不可达，改用国内镜像安装 Homebrew"
    info "  安装脚本：mirrors.ustc.edu.cn（与官方同步）"
    installer_url="https://mirrors.ustc.edu.cn/misc/brew-install.sh"
    BREW_API_DOMAIN="https://mirrors.aliyun.com/homebrew/homebrew-bottles/api"
    BREW_BOTTLE_DOMAIN="https://mirrors.aliyun.com/homebrew/homebrew-bottles"
    export HOMEBREW_BREW_GIT_REMOTE="https://mirrors.ustc.edu.cn/brew.git"
    # 注意：core 的仓库镜像用 TUNA —— USTC 的 homebrew-core.git 实测返回 404
    export HOMEBREW_CORE_GIT_REMOTE="https://mirrors.tuna.tsinghua.edu.cn/git/homebrew/homebrew-core.git"
    export HOMEBREW_API_DOMAIN HOMEBREW_BOTTLE_DOMAIN
  fi
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

  if [ -n "$BREW_API_DOMAIN" ]; then
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

# ------------------------------------------------------------ 安装依赖组件 --
install_deps() {
  title "检查系统依赖"

  if ! has_brew; then
    warn "未检测到 Homebrew。面板本身不依赖它，但网站管理（nginx/PHP/MySQL）需要。"
    if [ "${ZIZPANEL_INSTALL_BREW:-1}" = "1" ]; then
      setup_homebrew || warn "Homebrew 未能自动安装，可稍后在面板里重试或手工安装"
    else
      warn "已按 ZIZPANEL_INSTALL_BREW=0 跳过 Homebrew 安装"
    fi
  fi

  if ! has_brew; then
    warn "面板已可以正常使用（面板本身不依赖 Homebrew）。"
    warn "但「网站管理」与「数据库」需要 nginx、PHP、MySQL，装好 Homebrew 后执行："
    warn "  brew install nginx php@8.3 mysql@8.4 && brew services start nginx php@8.3 mysql@8.4"
    return 0
  fi
  ok "Homebrew：$(brew_prefix)"

  if [ "${ZIZPANEL_SKIP_DEPS:-0}" = "1" ]; then
    warn "已跳过依赖安装（ZIZPANEL_SKIP_DEPS=1）"
    return 0
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
    # 注意措辞：这里**不能**说"去应用市场装"。
    # 应用市场当前只收录了容器类应用（Ollama/Uptime Kuma/n8n…），
    # 并没有 nginx/php/mysql 的条目，指过去只会让用户在那里白找一圈。
    warn "缺少：${missing[*]}"
    info "面板本身已可用；但「网站管理」「数据库」这两块需要它们。"
    warn "本次不自动安装（国内网络下这几个包要装很久，放在安装脚本里会卡住不动）。"
    info "也可以在面板「应用市场 → 网站环境」里点安装（有进度、可重试）。"
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
    </dict>
    <key>ProcessType</key>
    <string>Background</string>
</dict>
</plist>
PLIST

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
  local port="${ZIZPANEL_LISTEN##*:}"
  local url="https://127.0.0.1:${port}/api/v1/health"
  # 诊断日志：探活失败时用户最需要知道"卡在哪一步"，而不是只看到"未就绪"
  # 注意：所有 local 声明必须在循环外。
  # 写在循环体内会导致每次迭代都重新声明并清空变量（bash 的 local 是函数作用域，
  # 但重新声明会重置值），后续引用就会触发 set -u 的 unbound variable。
  local diag="$LOG_DIR/install-check.log"
  local waited=0
  local st="" code="000" holder=""
  : > "$diag" 2>/dev/null || true

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
      echo "[${waited}s] 进程运行中，HTTP 状态码=$code" >> "$diag"
    else
      echo "[${waited}s] 进程未运行（${st}）" >> "$diag"
      # 未运行时记录端口占用，便于判断是"没起来"还是"端口被占"
      holder="$(/usr/sbin/lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $1" pid "$2}' | tr '\n' ' ')"
      [ -n "$holder" ] && echo "[${waited}s] 端口 $port 被占用：$holder" >> "$diag"
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

  printf '\n'
  printf '  %s🦊 ZizPanel 已就绪%s\n\n' "$C_BOLD" "$C_RESET"
  printf '  远程访问   %s%s://%s:%s%s\n' "$C_GREEN" "$scheme" "$ip" "$port" "$C_RESET"
  printf '  本机访问   %s%s://127.0.0.1:%s%s\n' "$C_BLUE" "$scheme" "$port" "$C_RESET"
  printf '\n'
  printf '  首次打开会进入初始化向导，请设置管理员账号。\n'
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
}

# ------------------------------------------- 接管旧面板（部署替换） --
# 目标：让新面板在原来习惯的访问路径（默认 http://localhost/_panel）直接可用，
# 同时把旧的简易面板目录改名保留（随时可回滚），而不是直接删掉。
#
# 为什么保留旧目录：升级第一原则是可回滚。旧面板只有几百 KB，
# 留着不占空间，却能在新面板出问题时立刻切回去。
takeover_legacy_panel() {
  title "接管面板入口"

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
      --listen)
        shift
        ZIZPANEL_LISTEN="${1:-:8443}"
        ;;
      -h|--help)
        sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
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

  # 服务器模式默认会开启 SSH。若不想开，设 ZIZPANEL_NO_SSH=1。
  # 之所以做成显式开关：开 SSH 会改变这台机器的网络暴露面，
  # 这种事必须让用户能拒绝，而不是替他决定。
  local ssh_args=()
  if [ "${ZIZPANEL_NO_SSH:-0}" = "1" ]; then
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
  detect_source
  install_deps

  case "$SOURCE_KIND" in
    local)    info "安装方式：本地二进制（离线）" ;;
    download) download_binaries ;;
    build)    build_from_source ;;
  esac

  install_binaries
  install_sudoers
  install_daemon
  setup_nginx_env
  setup_cert
  setup_firewall
  takeover_legacy_panel
  setup_server_mode
  write_uninstaller

  verify_running
  finish
}

main "$@"
