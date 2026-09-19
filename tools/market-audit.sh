#!/usr/bin/env bash
# ============================================================================
#  market-audit.sh —— 应用市场审计的**一条命令**入口
#
#  用户的原话：
#    "对于应用市场，应该有能用高效的工作流，如果以后每加一个应用都要一点一点
#     慢慢调试，那这个应用市场就没什么实用价值了。"
#
#  默认行为：审**全部**在售应用（数量以代码注册表为准；真的去探镜像站、上游、registry manifest、
#  sha256），一张表告诉你哪个应用缺什么。有缺口就非零退出。
#
#  用法：
#    tools/market-audit.sh                  # 全部应用（在线）
#    tools/market-audit.sh --offline        # 只跑静态不变量（不联网，秒级）
#    tools/market-audit.sh --only squoosh   # 只审一个（加新应用时用）
#    tools/market-audit.sh --json           # 机器可读
#    tools/market-audit.sh --fast           # 跳过"下载整包实算 sha256"
#    tools/market-audit.sh --mirror https://<你自己的镜像机>
#    tools/market-audit.sh --install -- https://127.0.0.1:8443 admin pass --only squoosh
#                                           # 追加"真机安装验收"档（会真的装软件）
#
#  为什么要有这个 wrapper：
#    · 固定 GOPROXY（本机 proxy.golang.org 不可达），否则 go build 会卡住
#    · 先 build 成临时二进制再跑，避免 `go run` 把非零退出码包成
#      "exit status 1" 噪声（退出码要能被 make / CI 看见）
#    · 真机验收档**复用** tools/market-install-verify.py，不重写一套安装验证
# ============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

PASSTHRU=()
INSTALL=0
VERIFY_ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --install)
      INSTALL=1
      shift
      ;;
    --)
      shift
      VERIFY_ARGS=("$@")
      break
      ;;
    *)
      PASSTHRU+=("$1")
      shift
      ;;
  esac
done

BIN="$(mktemp -t zizpanel-assets)"
trap 'rm -f "$BIN"' EXIT

echo "==> 构建 zizpanel-assets" >&2
go build -o "$BIN" ./cmd/zizpanel-assets

echo "==> 审计应用市场（全部在售条目；--only 可只审一个）" >&2
set +e
if [ ${#PASSTHRU[@]} -gt 0 ]; then
  "$BIN" audit "${PASSTHRU[@]}"
else
  "$BIN" audit
fi
RC=$?
set -e

if [ "$INSTALL" -eq 1 ]; then
  echo >&2
  echo "==> 真机安装验收档（tools/market-install-verify.py）" >&2
  echo "    它会真的在目标机器上安装软件，并独立复核真实状态。" >&2
  echo "    静态 + 在线审计**不能**替代这一步：'服务起不来'只有真机能发现。" >&2
  if [ ${#VERIFY_ARGS[@]} -eq 0 ]; then
    echo "    （没给参数，跳过）用法：" >&2
    echo "      tools/market-audit.sh --install -- <base_url> <user> <pass> [--only id]" >&2
  else
    set +e
    python3 tools/market-install-verify.py "${VERIFY_ARGS[@]}"
    VRC=$?
    set -e
    echo "    真机验收退出码：${VRC}（它自己的输出才是结论）" >&2
  fi
fi

exit "$RC"
