#!/bin/bash
# plugin-author-check.sh —— 「按文档从零接入一个应用」的全链路自检（C1/B 的验收面）。
#
# 用户的目标是「通过标准化的内容让开发变得相对流程化」。这条链路的判据只能是：
# **照着 docs/插件规范.md 走一遍**，中间不靠口口相传。这个脚本就把那几步原样跑一遍：
#   ① `plugin init` 生成模板；② 按作者会做的方式改几处；③ `plugin validate` + `plugin plan`；
#   ④ 放进面板插件目录 → 在面板里**启用**；⑤ 从市场接口回读卡片，逐字段核对声明。
#
# 有意**不装**（安装会真的动 brew/系统）：这一步是 B2c，要真机 + 用户点头。
#
# 用法：先 make run-local（默认 /tmp/zizpanel-dev，18443/dev），然后
#   bash tools/plugin-author-check.sh [面板入口]
set -u
ENTRY="${1:-http://127.0.0.1:18443/dev}"
ENTRY="${ENTRY%/}"
ROOT="${ZP_LOCAL_ROOT:-/tmp/zizpanel-dev}"
PLUGDIR="$ROOT/plugins"
PASS='zizpanel-test-fixture-pass'
JAR=$(mktemp)
WORK=$(mktemp -d)
FAILED=0

say() { printf '\n=== %s\n' "$*"; }
ok() { printf '  ✅ %s\n' "$*"; }
bad() { printf '  ❌ %s\n' "$*"; FAILED=1; }

say "① 按文档生成模板（plugin init）"
cd "$(dirname "$0")/.." || exit 1
go run ./cmd/zizpanel plugin init --out "$WORK/authorapp.json" >/dev/null || { bad "plugin init 失败"; exit 1; }
[ -s "$WORK/authorapp.json" ] && ok "模板已生成：$WORK/authorapp.json" || bad "模板为空"

say "② 像作者那样改几处（id/名字/摘要/formula/端口/健康路径）"
python3 - "$WORK/authorapp.json" <<'PY'
import json,sys
p=sys.argv[1]
d=json.load(open(p))
d['id']='authorapp'; d['name']='作者自测应用'; d['summary']='照文档接入的示例（不安装）'
d['source']['formula']='mosquitto'          # 用一个存在的 formula，避免"formula 不存在"的假失败
d['expose']['port']=18884                   # 与 catalog/内置表都不冲突
d['health']={'kind':'http','path':'/healthz','expect':'ok','interval':'30s'}
d['verify']={'any_of':[{'kind':'http','path':'/healthz','expect':'ok','timeout':'60s'}]}
d['requires']={'system_daemon':True,'ports':[18884]}
json.dump(d,open(p,'w'),ensure_ascii=False,indent=2)
print('  已改：id=authorapp port=18884 formula=mosquitto')
PY

say "③ 校验与干跑（validate / plan）"
go run ./cmd/zizpanel plugin validate "$WORK/authorapp.json" || bad "validate 失败"
PLAN=$(go run ./cmd/zizpanel plugin plan "$WORK/authorapp.json" 2>&1)
echo "$PLAN" | grep -q "18884" && ok "plan 里写出了声明的端口 18884" || bad "plan 里没有端口"
echo "$PLAN" | grep -q "健康判据" && ok "plan 里写出了健康判据" || bad "plan 里没有健康判据"

say "④ 放进插件目录并在面板里启用"
mkdir -p "$PLUGDIR"
cp "$WORK/authorapp.json" "$PLUGDIR/authorapp.json"
# 调试实例每次都是全新的：先登录，未初始化就补一次 setup 再登录（与其它自检脚本同一套）。
curl -s -c "$JAR" -b "$JAR" -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" "$ENTRY/api/v1/login" -o "$WORK/login1.json"
if ! grep -q '"ok":true' "$WORK/login1.json" 2>/dev/null; then
  curl -s -c "$JAR" -b "$JAR" -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" "$ENTRY/api/v1/setup" -o /dev/null
  curl -s -c "$JAR" -b "$JAR" -H 'Content-Type: application/json' \
    -d "{\"username\":\"admin\",\"password\":\"$PASS\"}" "$ENTRY/api/v1/login" -o "$WORK/login1.json"
fi
csrf=$(awk '/zp_csrf/ {print $7}' "$JAR" | tail -1)
if grep -q '"ok":true' "$WORK/login1.json" 2>/dev/null; then ok "已登录调试实例"; else bad "登录失败（先 make run-local？）"; fi
curl -s -b "$JAR" "$ENTRY/api/v1/plugins" -o "$WORK/plugins.json"
python3 - "$WORK/plugins.json" <<'PY' && ok "列表里能看到这份声明且可安装" || bad "列表里没有它 / 不可安装"
import json,sys
d=(json.load(open(sys.argv[1])).get('data') or {})
it=[x for x in (d.get('items') or []) if x.get('id')=='作者自测应用' or x.get('id')=='authorapp']
assert it, d
assert it[0].get('valid') and it[0].get('installable'), it[0]
PY
curl -s -b "$JAR" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  -d '{"enabled":true}' "$ENTRY/api/v1/plugins/authorapp/toggle" -o "$WORK/toggle.json"
grep -q '"ok":true' "$WORK/toggle.json" && ok "已启用" || bad "启用失败：$(cat "$WORK/toggle.json")"

say "⑤ 从市场接口回读卡片，逐字段核对声明"
curl -s -b "$JAR" "$ENTRY/api/v1/market?fresh=1" -o "$WORK/market.json"
python3 - "$WORK/market.json" <<'PY' && ok "卡片字段与声明一致" || bad "卡片字段与声明不一致"
import json,sys
d=(json.load(open(sys.argv[1])).get('data') or {})
items = d.get('list') if isinstance(d,dict) else d
items = items or []
card=[x for x in items if x.get('id')=='authorapp']
assert card, '市场里没有 authorapp 卡片：'+str([x.get('id') for x in items])[:200]
c=card[0]
assert c.get('name')=='作者自测应用', c.get('name')
assert c.get('summary')=='照文档接入的示例（不安装）', c.get('summary')
assert c.get('port')==18884, c.get('port')
assert c.get('health_path')=='/healthz', c.get('health_path')
print('  卡片：', c.get('name'), c.get('port'), c.get('health_path'))
PY

say "⑥ 收尾：停用并移出（不留残留）"
curl -s -b "$JAR" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  -d '{"enabled":false}' "$ENTRY/api/v1/plugins/authorapp/toggle" -o /dev/null
rm -f "$PLUGDIR/authorapp.json"
python3 - "$PLUGDIR/enabled.json" <<'PY' 2>/dev/null || true
import json,sys,os
p=sys.argv[1]
if os.path.exists(p):
    d=json.load(open(p)); d.pop('authorapp',None); json.dump(d,open(p,'w'),ensure_ascii=False,indent=2)
PY
echo "  （已停用并删除 authorapp.json；enabled.json 里的条目也清掉了）"

rm -rf "$WORK" "$JAR"
if [ "$FAILED" = 0 ]; then
  echo -e "\n照文档从零接入一个应用：全链路通过 ✅（安装那一步不在本脚本里 —— 见 B2c）"
else
  echo -e "\n有失败项（见上面的 ❌）"; exit 1
fi
