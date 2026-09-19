#!/usr/bin/env bash
# ============================================================================
#  codesign-release.sh —— 用**固定的自签证书**给发布产物签名
#
#  为什么必须签（2026-09-19 实测结论，见 docs/坑清单.md #183）：
#    macOS 的 TCC（隐私保护）把"用户授权"绑定在**二进制的代码要求**上。
#    未签名的 Go 二进制是 adhoc 签名，判据是 `cdhash H"…"` —— **每次构建都变**，
#    于是用户每升级一次面板就要重新授权一次（实测：1.4.5→1.4.6、1.4.6→1.4.7 都失效，
#    连 System Settings 里的完全磁盘访问开关都跟着失效）。
#    用同一张证书 + 同一个 identifier 签名后，判据变成：
#      designated => identifier "com.zizpanel.panel" and certificate root = H"<证书指纹>"
#    —— 与 cdhash 无关 ⇒ **授权一次，之后升级都不用再授**。
#
#  用法：tools/codesign-release.sh <二进制> <identifier>
#    identifier 约定：面板 com.zizpanel.panel，提权助手 com.zizpanel.helper
#
#  身份从哪来：本机钥匙串里名为 "ZizPanel Release" 的代码签名身份
#    （私钥只在构建机，`.release-key/codesign/` 已 gitignore；目标机只需要证书公钥，
#      由 install.sh 以 root 导入系统钥匙串并设信任）。
#  可用 ZP_CODESIGN_ID 覆盖身份名。
# ============================================================================
set -euo pipefail

BIN="${1:?用法: codesign-release.sh <二进制> <identifier>}"
IDENT="${2:?用法: codesign-release.sh <二进制> <identifier>}"
SIGN_ID="${ZP_CODESIGN_ID:-ZizPanel Release}"

if [ ! -f "$BIN" ]; then
  echo "!! 找不到要签名的二进制：$BIN" >&2
  exit 1
fi

# 身份必须在钥匙串里且**受信任**（untested 状态 codesign 会报
# "The specified item could not be found in the keychain"，很容易误判成别的问题）。
if ! security find-identity -v -p codesigning 2>/dev/null | grep -qF "$SIGN_ID"; then
  echo "!! 找不到可用的代码签名身份：$SIGN_ID" >&2
  echo "   本机构建需要它（私钥不出构建机）。一次性设置：" >&2
  echo "   .release-key/codesign/ 下有自签证书；把它导入钥匙串并设为受信任后重试：" >&2
  echo "     security import .release-key/codesign/zp-codesign.p12 \\" >&2
  echo "       -k ~/Library/Keychains/login.keychain-db -P zizpanel -T /usr/bin/codesign" >&2
  echo "     security add-trusted-cert -r trustRoot -p codeSign \\" >&2
  echo "       -k ~/Library/Keychains/login.keychain-db .release-key/codesign/zp-codesign.crt" >&2
  echo "   确实要出**未签名**产物（用户升级后会需要重新授权）：SKIP_CODESIGN=1" >&2
  exit 1
fi

codesign --force --sign "$SIGN_ID" --identifier "$IDENT" "$BIN" >/dev/null 2>&1 || {
  echo "!! 签名失败：${BIN}（身份 ${SIGN_ID}）" >&2
  exit 1
}

# 回读验证：必须是**证书签名**（出现 Authority=），而不是 adhoc；identifier 要对。
INFO="$(codesign -dvvv "$BIN" 2>&1 || true)"
if ! printf '%s' "$INFO" | grep -q "^Identifier=$IDENT$"; then
  echo "!! 签名后 identifier 不对：期望 ${IDENT}" >&2
  printf '%s\n' "$INFO" | grep -E '^(Identifier|Signature|Authority)=' >&2 || true
  exit 1
fi
if ! printf '%s' "$INFO" | grep -q "^Authority=$SIGN_ID"; then
  echo "!! 签名后没有证书链（仍是 adhoc？）：${BIN}" >&2
  printf '%s\n' "$INFO" | grep -E '^(Identifier|Signature|Authority)=' >&2 || true
  exit 1
fi
# 判据里**不许**再出现 cdhash —— 出现就说明稳定签名的前提没达成（升级后授权必然失效）。
REQ="$(codesign -d -r- "$BIN" 2>&1 | sed -n 's/^designated => //p')"
case "$REQ" in
  *cdhash*) echo "!! 签名判据仍含 cdhash（${REQ}）——授权将随升级失效，拒绝产出" >&2; exit 1 ;;
esac
[ -n "$REQ" ] || { echo "!! 读不到签名判据（designated requirement）" >&2; exit 1; }

echo "    ✓ 已签名 ${IDENT}（${SIGN_ID}；判据：${REQ}）"
