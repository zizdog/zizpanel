#!/usr/bin/env bash
# =============================================================================
#  tools/takeover-panel-entry.sh —— 让新面板接管原来习惯的访问入口
#
#  背景：
#    这台机器上原本有一个简易 PHP 面板，通过 nginx 的
#        location ^~ /_panel { alias .../www/_panel; }
#    访问。换成 ZizPanel 后，希望 http://localhost/_panel 直接进新面板，
#    这样使用习惯不变，也不用记新端口。
#
#  为什么必须改 000-default.conf 而不是新增一个 vhost：
#    nginx 选择 server 块的规则是「精确 server_name > 通配 > 正则 > default_server」。
#    000-default.conf 声明了 listen 80 default_server，
#    任何 Host 为 localhost 但没被其它 server_name 精确匹配的请求都会落到它。
#    因此新建一个只写 server_name localhost 的文件并不能接管 —— 只会变成死配置。
#
#  幂等性：
#    用一个成对标记块（BEGIN/END ZIZPANEL PANEL ENTRY）标注我们的 location。
#    重复执行时会先删掉旧块（含无标记的历史块），再插入新块，不会产生重复。
#    脚本自身还会校验"入口块必须恰好一个"，不满足就不落盘。
#
#  实现说明：
#    具体的配置改写逻辑放在同目录的 panel-entry.awk 里。
#    拆成独立文件是因为这段逻辑需要在嵌套引号中嵌入 awk 程序，
#    内联在 shell 里转义极易出错（开发过程中确实反复踩坑）。
#
#  用法：
#    tools/takeover-panel-entry.sh --port 8443 [--path /_panel] [--dry-run] [--vhost-dir DIR]
# =============================================================================
set -uo pipefail

PORT="8443"
PANEL_PATH="/_panel"
VHOST_DIR=""
DRY_RUN=0
MARK_BEGIN="# BEGIN ZIZPANEL PANEL ENTRY"
MARK_END="# END ZIZPANEL PANEL ENTRY"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# awk 程序可能在同级目录，也可能在 tools/ 子目录（安装后的布局）
AWK_PROG="$SCRIPT_DIR/panel-entry.awk"
[ -f "$AWK_PROG" ] || AWK_PROG="$SCRIPT_DIR/tools/panel-entry.awk"

C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_YELLOW=$'\033[33m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
info() { printf '%s[信息]%s %s\n' "$C_BOLD" "$C_RESET" "$*"; }
ok()   { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --port)      PORT="${2:-}"; shift 2 ;;
    --path)      PANEL_PATH="${2:-}"; shift 2 ;;
    --vhost-dir) VHOST_DIR="${2:-}"; shift 2 ;;
    --dry-run)   DRY_RUN=1; shift ;;
    -h|--help)   sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) err "未知参数: $1"; exit 1 ;;
  esac
done

case "$PANEL_PATH" in
  /*) ;;
  *) err "面板路径必须以 / 开头: $PANEL_PATH"; exit 1 ;;
esac
case "$PORT" in
  ''|*[!0-9]*) err "端口必须是数字: $PORT"; exit 1 ;;
esac
if [ ! -f "$AWK_PROG" ]; then
  err "缺少改写程序: $AWK_PROG"
  exit 1
fi

# vhost 目录的确定顺序：
#   1) --vhost-dir 参数
#   2) ZIZPANEL_VHOST_DIR 环境变量（安装脚本转发，沙箱测试依赖它做隔离）
#   3) nginx 的默认位置
#
# 第 2 条至关重要：沙箱测试必须能把改写限制在沙箱目录里。
# 早期版本忽略了环境变量，导致沙箱测试直接改写生产环境的
# 000-default.conf（把 proxy_pass 端口改成测试端口，全站 502）。
if [ -z "$VHOST_DIR" ] && [ -n "${ZIZPANEL_VHOST_DIR:-}" ]; then
  VHOST_DIR="$ZIZPANEL_VHOST_DIR"
fi
if [ -z "$VHOST_DIR" ]; then
  for p in /opt/homebrew/etc/nginx/vhosts /usr/local/etc/nginx/vhosts; do
    if [ -d "$p" ]; then VHOST_DIR="$p"; break; fi
  done
fi
if [ -z "$VHOST_DIR" ] || [ ! -d "$VHOST_DIR" ]; then
  warn "未找到 nginx vhost 目录，跳过入口接管"
  exit 0
fi

DEFAULT_CONF="$VHOST_DIR/000-default.conf"
if [ ! -f "$DEFAULT_CONF" ]; then
  warn "未找到 ${DEFAULT_CONF}，跳过入口接管"
  exit 0
fi

info "接管面板入口：$PANEL_PATH → http://127.0.0.1:$PORT/"

TMP="$(mktemp "${TMPDIR:-/tmp}/zp-entry.XXXXXX")"
BODY="$(mktemp "${TMPDIR:-/tmp}/zp-body.XXXXXX")"
trap 'rm -f "$TMP" "$BODY"' EXIT INT TERM

# 生成入口块内容。用带引号的 heredoc 保证 $host 等 nginx 变量不被 shell 展开。
cat > "$BODY" <<'ENTRY_EOF'
    # BEGIN ZIZPANEL PANEL ENTRY
    # 先补末尾斜杠：前端用的是相对路径（./app.css、api/v1/...），
    # 访问 "http://host/_panel"（无斜杠）时相对路径会解析到根路径 "/"，
    # 导致静态资源 404。重定向到带斜杠的形式可彻底避免这个问题。
    location = __PANEL_PATH__ {
        return 301 __PANEL_PATH__/;
    }

    location ^~ __PANEL_PATH__/ {
        # 反向代理到 ZizPanel。
        #
        # 两个必须注意的点：
        #  1) 面板服务端强制 HTTPS，所以这里是 https:// 而不是 http://。
        #     用 http:// 转发会被面板以 400 "Client sent an HTTP request to
        #     an HTTPS server" 拒绝 —— 是真机上踩过的坑。
        #  2) 面板证书是自签/mkcert 签发的本地证书，nginx 校验系统 CA 会失败，
        #     因此必须关闭对上游的证书校验（局域网回环链路，风险可接受）。
        #  3) proxy_ssl_server_name 让 SNI 带上域名，否则证书与请求不匹配。
        # 用 rewrite + 无 URI 的 proxy_pass 来剥离前缀。
        # 说明：理论上 `proxy_pass https://host:port/;`（带尾斜杠）就会剥掉
        # location 匹配到的前缀，但在实际环境里 Go 侧仍然收到了带前缀的路径
        # （表现为 307 重定向到 /api/...），因此这里显式 rewrite 一次，
        # 行为直观且不依赖 nginx 的隐式规则。
        rewrite ^__PANEL_PATH__/?(.*)$ /$1 break;
        proxy_pass https://127.0.0.1:__PORT__;
        proxy_ssl_verify off;
        proxy_ssl_server_name on;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        # 告诉面板"外部是 HTTP"，面板据此生成正确的跳转与 Cookie 属性
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        # connection_upgrade 由 conf.d/upgrade-map.conf 定义（面板安装时写入）
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 300s;
        proxy_buffering off;
        proxy_cache off;
    }
    # END ZIZPANEL PANEL ENTRY
ENTRY_EOF

# 替换占位符（BSD sed 与 GNU sed 参数不同，需要判断）
SED_INPLACE=(-i "")
if sed --version >/dev/null 2>&1; then
  SED_INPLACE=(-i)
fi
sed "${SED_INPLACE[@]}" \
  -e "s|__PANEL_PATH__|$PANEL_PATH|g" \
  -e "s|__PORT__|$PORT|g" \
  "$BODY"

/usr/bin/awk -v path="$PANEL_PATH" -v mb="$MARK_BEGIN" -v me="$MARK_END" \
  -v body="$BODY" -f "$AWK_PROG" "$DEFAULT_CONF" > "$TMP"

if ! grep -q "$MARK_BEGIN" "$TMP"; then
  err "生成的配置中未包含入口块，未做修改"
  exit 1
fi
ENTRIES="$(grep -c "location \^~ $PANEL_PATH/" "$TMP" || true)"
if [ "$ENTRIES" != "1" ]; then
  err "生成的配置中入口块数量为 ${ENTRIES}（应为 1），未做修改"
  exit 1
fi
if ! grep -q '^server {' "$TMP"; then
  err "生成的配置中丢失了 server 块，未做修改"
  exit 1
fi

if [ "$DRY_RUN" = "1" ]; then
  info "dry-run：将写入以下内容（未落盘）"
  cat "$TMP"
  exit 0
fi

if [ ! -f "$DEFAULT_CONF.zizpanel.bak" ]; then
  cp "$DEFAULT_CONF" "$DEFAULT_CONF.zizpanel.bak" 2>/dev/null || true
  info "原配置已备份为 $(basename "$DEFAULT_CONF").zizpanel.bak"
fi

chmod 0644 "$TMP" 2>/dev/null || true
mv -f "$TMP" "$DEFAULT_CONF"
ok "已写入入口配置：$DEFAULT_CONF"
