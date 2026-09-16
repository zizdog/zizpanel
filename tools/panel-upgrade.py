#!/usr/bin/env python3
"""用面板自己的在线升级流程升级一台机器（check / stage / apply）。

用法:
  zp-upgrade.py <PANEL_BASE> <USER> <PASS> <SOURCE_URL> <WANT_VERSION> [SUFFIX]

第 6 个参数（可选）是面板的**安全后缀**：面板的界面与接口都在 /<suffix>/ 之下，
不传就表示没启用后缀（根路径）。
"""
import json
import ssl
import sys
import time
import urllib.error
import urllib.request
import http.cookiejar

BASE, USER, PASS, SRC, WANT = sys.argv[1:6]
SUFFIX = sys.argv[6].strip("/") if len(sys.argv) > 6 else ""
if SUFFIX:
    BASE = BASE.rstrip("/") + "/" + SUFFIX

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
cj = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj),
                                 urllib.request.HTTPSHandler(context=ctx))


def call(method, path, body=None, timeout=180):
    h = {"Content-Type": "application/json"}
    c = next((x.value for x in cj if x.name == "zp_csrf"), "")
    if c:
        h["X-CSRF-Token"] = c
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(body).encode() if body is not None else None,
        headers=h, method=method)
    try:
        with op.open(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def health():
    """健康检查留在根路径（升级流程的契约，不带后缀）。"""
    root = BASE
    if SUFFIX:
        root = BASE[: -(len(SUFFIX) + 1)]
    try:
        with urllib.request.urlopen(root + "/api/v1/health", timeout=8, context=ctx) as r:
            return json.loads(r.read())["data"]
    except Exception:
        return None


h = health()
print("== 升级前版本:", (h or {}).get("version"), " 目标:", WANT)
st, _ = call("POST", "/api/v1/login", {"username": USER, "password": PASS})
print("== 登录:", st)
st, r = call("POST", "/api/v1/system/upgrade/check", {"source": SRC})
print("== 检查更新:", st, "current=", (r.get("data") or {}).get("current"),
      "latest=", (r.get("data") or {}).get("latest"),
      "has_update=", (r.get("data") or {}).get("has_update"))
st, r = call("POST", "/api/v1/system/upgrade/stage", {"source": SRC})
print("== stage:", st, (r.get("data") or {}).get("message", r.get("msg", "")))
st, r = call("POST", "/api/v1/system/upgrade/apply", {})
print("== apply:", st, (r.get("data") or {}).get("message", r.get("msg", "")))
print("== 等新版本回来…")
for i in range(1, 91):
    time.sleep(2)
    h = health()
    v = (h or {}).get("version")
    if v and v.startswith(WANT):
        print(f"   OK 第 {i} 轮 -> {v}")
        sys.exit(0)
    if i % 10 == 0:
        print(f"   +{i*2}s 当前 {v}")
print("== 超时未回到", WANT)
sys.exit(1)
