#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""应用市场"逐个真机安装"取数工具。

为什么需要它：用户要求"确保应用市场里的所有软件都能顺利安装"。
"我读过代码，应该能装"不算证据，`brew` / `docker` / `pip` 的退出码也不算
（历史上多次出现退出码 0 而实际什么都没装成）。所以这里做的是：

  1. 通过面板自己的 API 逐个触发安装（走任务中心，绝不绕过）；
  2. 把任务的**终态、错误、警告行、耗时**原样记下来；
  3. 装完再**独立复核真实状态**（服务是否登记、市场是否标记已装、
     HTTP 是否真能打开），而不是相信任务的自我汇报；
  4. 输出一张表 + 一份 JSON，失败的应用单独列出来。

用法：
  python3 tools/market-install-verify.py <base_url> <user> <pass> [选项] [应用ID...]

  --only a,b,c     只测这几个（逗号分隔）
  --exclude a,b    排除这几个
  --timeout 900    单个应用等待上限（秒）
  --json out.json  结果写到 JSON
  --dry-run        只列出将要安装的应用，不真的装
  --keep-going     某个失败也继续（默认就是继续；保留此开关是为了显式）

注意：**它会真的在目标机器上安装软件**。这是真机验证工具，不是单测。
"""

import http.cookiejar
import json
import secrets
import ssl
import string
import sys
import time
import urllib.error
import urllib.request

TERMINAL = {"succeeded", "failed", "canceled"}


class Panel:
    def __init__(self, base, user, password):
        self.base = base.rstrip("/")
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar),
            urllib.request.HTTPSHandler(context=ctx),
        )
        self.user = user
        self.password = password

    def call(self, method, path, body=None, timeout=60):
        req = urllib.request.Request(self.base + path, method=method)
        req.add_header("Content-Type", "application/json")
        csrf = next((c.value for c in self.jar if c.name == "zp_csrf"), None)
        if csrf:
            req.add_header("X-CSRF-Token", csrf)
        data = json.dumps(body).encode() if body is not None else None
        try:
            with self.op.open(req, data=data, timeout=timeout) as r:
                raw = r.read().decode("utf-8", "replace")
                return r.status, raw
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")
        except Exception as e:  # 网络层失败也要变成数据，不能把整轮测试炸掉
            return 0, json.dumps({"error": f"{type(e).__name__}: {e}"})

    def login(self):
        st, body = self.call("POST", "/api/v1/login",
                             {"username": self.user, "password": self.password})
        if st != 200:
            raise SystemExit(f"登录失败 HTTP {st}: {body[:300]}")
        return body

    def json(self, method, path, body=None, timeout=60):
        """返回 (http_status, data)。

        面板的响应外层统一是 {"ok":bool,"data":{…}}，这里把 data 拆出来，
        并把外层的 ok/msg 保留在 _ok/_msg 里 —— 不能因为"拆信封"就把
        "这次请求其实失败了"这条信息丢掉。
        """
        st, raw = self.call(method, path, body, timeout)
        try:
            d = json.loads(raw)
        except Exception:
            return st, {"_raw": raw}
        if isinstance(d, dict) and "data" in d:
            inner = d["data"]
            if isinstance(inner, dict):
                inner.setdefault("_ok", d.get("ok"))
                if d.get("msg"):
                    inner.setdefault("_msg", d.get("msg"))
                return st, inner
            return st, {"_data": inner, "_ok": d.get("ok")}
        return st, d

    def market_apps(self):
        _, d = self.json("GET", "/api/v1/market")
        lst = d.get("list") if isinstance(d, dict) else None
        return lst if isinstance(lst, list) else []

    def services(self):
        """服务登记列表。

        注意键名是 `list` 而不是 `services` —— 写错会永远读到空数组，
        然后把"每个应用都没登记"当成缺陷报出去（我自己就先踩了一次）。
        """
        _, d = self.json("GET", "/api/v1/services")
        lst = d.get("list") if isinstance(d, dict) else None
        return lst if isinstance(lst, list) else []


def gen_password(n=24):
    alphabet = string.ascii_letters + string.digits + "!@#%^*-_=+"
    return "".join(secrets.choice(alphabet) for _ in range(n))


def poll_task(p, task_id, timeout, log):
    """轮询任务到终态。返回 (task_dict, answers) —— answers 记录我们代填过的输入。"""
    answers = []
    started = time.time()
    last_line = 0
    while True:
        if time.time() - started > timeout:
            st, t = p.json("GET", f"/api/v1/tasks/{task_id}")
            t = t.get("task", t) if isinstance(t, dict) else {}
            t["_timeout"] = True
            return t, answers
        st, t = p.json("GET", f"/api/v1/tasks/{task_id}")
        if isinstance(t, dict) and "task" in t:
            t = t["task"]
        if not isinstance(t, dict):
            return {"status": "unknown", "_raw": t}, answers

        # 关键：任务在等用户输入（例如 MySQL root 密码）。真机验证必须有人应答，
        # 否则任务会一直挂到超时 —— 那看起来像"安装慢"，实际是"没人回话"。
        ir = t.get("input_required")
        if ir:
            key = ir.get("key") or "input"
            if ir.get("secret"):
                val = gen_password()
            else:
                val = "yes"
            answers.append({"key": key, "label": ir.get("label"), "value": val,
                            "secret": bool(ir.get("secret"))})
            log(f"      ↳ 任务在等待输入「{ir.get('label') or key}」，已代填"
                f"{'（已生成随机密码）' if ir.get('secret') else ''}")
            p.call("POST", f"/api/v1/tasks/{task_id}/input", {"key": key, "value": val})
            time.sleep(1)
            continue

        if t.get("status") in TERMINAL:
            return t, answers
        time.sleep(2)


def task_lines(p, task_id):
    """取任务日志行。

    面板**没有** /tasks/{id}/lines 这个路由：日志在
    `GET /api/v1/tasks/{id}?after=&limit=` 的 data.lines 里。
    """
    _, d = p.json("GET", f"/api/v1/tasks/{task_id}?after=0&limit=4000")
    lines = d.get("lines") if isinstance(d, dict) else None
    return lines if isinstance(lines, list) else []


def probe_http(url, timeout=8):
    """从**跑这个脚本的机器**独立发一个 HTTP 请求。

    为什么必须做这一步：任务的自我汇报、市场卡片的 installed 标记、面板的健康检查
    都是"面板自己的说法"。用户要的是"这东西现在真的能用吗"。所以这里绕开面板，
    直接从网络另一头请求一次 —— 这才是独立证据。
    历史上吃过亏：phpMyAdmin 的错误页也返回 200，所以这里**不把 200 当成功**，
    而是把状态码和正文特征都记下来，由人判断。
    """
    if not url:
        return None
    # 面板与应用多半用自签名证书。这是**验证工具**，不是生产客户端，
    # 所以不校验证书；但要把这一点写在明处，别让人以为它验过证书链。
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "zizpanel-verify"})
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
            body = r.read(4096).decode("utf-8", "replace")
            return {"status": r.status, "len_first": len(body),
                    "head": " ".join(body.split())[:160], "tls_verified": False}
    except urllib.error.HTTPError as e:
        try:
            body = e.read(2048).decode("utf-8", "replace")
        except Exception:
            body = ""
        return {"status": e.code, "head": " ".join(body.split())[:160], "tls_verified": False}
    except Exception as e:
        return {"status": 0, "error": f"{type(e).__name__}: {e}"}


def check_state(p, app):
    """独立复核：不看任务怎么说，看机器现在是什么样。

    任务报成功不等于装上了（历史上 phpMyAdmin 的 404 页、IOPaint 的
    "端口未监听但报成功"都骗过一次）。所以这里分开复核三件事：
    服务有没有登记、launchd 里状态如何、市场条目是否标记已装。
    """
    checks = {}
    label = app.get("service_label") or ""
    hit = None
    for it in p.services():
        if label and it.get("launch_label") == label:
            hit = it
            break
    if hit is None:
        for it in p.services():
            if it.get("name") == app.get("id"):
                hit = it
                break
    checks["service_registered"] = hit is not None
    if hit:
        st = hit.get("state") or {}
        h = hit.get("health") or {}
        checks["service_label"] = hit.get("launch_label")
        checks["service_status"] = st.get("status")
        checks["service_detail"] = st.get("detail")
        checks["service_running"] = st.get("running")
        checks["health_ok"] = h.get("ok")
        checks["health_message"] = h.get("message")
    for a in p.market_apps():
        if a.get("id") == app.get("id"):
            checks["market_installed"] = bool(a.get("installed"))
            checks["market_available"] = bool(a.get("available"))
            checks["market_note"] = a.get("note")
            checks["market_artifacts"] = bool(a.get("artifacts"))
            # 直连端口入口（面板给的局域网地址）—— 拿它做绕开面板的独立探测
            checks["http_probe"] = probe_http(a.get("port_url") or a.get("proxy_url"))
            break
    return checks


def main():
    args = sys.argv[1:]
    if len(args) < 3:
        raise SystemExit(__doc__)
    base, user, password = args[0], args[1], args[2]
    rest = args[3:]

    only, exclude = None, set()
    timeout, json_out, dry = 900, None, False
    ids = []
    i = 0
    while i < len(rest):
        a = rest[i]
        if a == "--only":
            i += 1
            only = [x for x in rest[i].split(",") if x]
        elif a == "--exclude":
            i += 1
            exclude = {x for x in rest[i].split(",") if x}
        elif a == "--timeout":
            i += 1
            timeout = int(rest[i])
        elif a == "--json":
            i += 1
            json_out = rest[i]
        elif a == "--dry-run":
            dry = True
        elif a == "--keep-going":
            pass
        else:
            ids.append(a)
        i += 1

    p = Panel(base, user, password)
    p.login()
    apps = p.market_apps()
    if not apps:
        raise SystemExit(f"拿不到应用市场清单（{base}）—— 注意 base 要带面板后缀，"
                         f"例如 https://127.0.0.1:8443/jab5c63")

    chosen = []
    for a in apps:
        aid = a.get("id")
        if only and aid not in only:
            continue
        if not only and ids and aid not in ids:
            continue
        if aid in exclude:
            continue
        chosen.append(a)

    # 路由与参数：绝大多数应用走同一个入口，只有两类例外。
    # 一键建站类（Typecho / WordPress）必须带域名，且站点不能已存在，
    # 所以用一个明显属于本次验证的域名，方便事后清理。
    def route(a, aid):
        if aid == "lnmp":
            return "/api/v1/market/install-lnmp", {}
        if a.get("site_app") or a.get("kind") == "site":
            return f"/api/v1/market/{aid}/install-site", {"domain": f"zpverify-{aid}.test"}
        return f"/api/v1/market/{aid}/install", {}

    print(f"目标面板：{base}")
    print(f"市场条目：{len(apps)}，本次测试：{len(chosen)}")
    for a in chosen:
        print(f"  - {a.get('id'):22s} {a.get('name','')}")
    if dry:
        return

    results = []
    for a in chosen:
        aid = a.get("id")
        name = a.get("name") or aid
        print(f"\n=== {aid} ({name}) ===")
        t0 = time.time()
        path, body_req = route(a, aid)
        st, body = p.json("POST", path, body_req)
        task_id = None
        if isinstance(body, dict):
            task_id = body.get("task_id") or body.get("id")
        if not task_id:
            # 有些安装器是秒级同步返回（没有任务中心）——记录原始响应
            results.append({
                "id": aid, "name": name, "started": st,
                "task_id": None, "status": "no-task",
                "http": st, "raw": str(body)[:600],
                "elapsed_s": round(time.time() - t0, 1),
                "state": check_state(p, a),
            })
            print(f"  ! 没有拿到 task_id（HTTP {st}）：{str(body)[:200]}")
            continue

        print(f"  task_id={task_id}")
        task, answers = poll_task(p, task_id, timeout, print)
        elapsed = round(time.time() - t0, 1)
        status = task.get("status")
        err = task.get("error") or ""
        last = task.get("last") or ""
        warn = [l for l in task_lines(p, task_id)
                if isinstance(l, dict) and l.get("level") in ("warn", "warning", "error")]
        state = check_state(p, a)
        rec = {
            "id": aid, "name": name, "started": st, "task_id": task_id,
            "status": status, "error": err, "last": last,
            "elapsed_s": elapsed, "answers": answers,
            "warnings": [w.get("text") for w in warn][:12],
            "state": state,
            "timed_out": bool(task.get("_timeout")),
        }
        results.append(rec)
        flag = "✓" if status == "succeeded" else "✗"
        probe = state.get("http_probe") or {}
        print(f"  {flag} status={status} 耗时={elapsed}s 登记={state.get('service_registered')} "
              f"市场已装={state.get('market_installed')} 独立HTTP={probe.get('status')}")
        if err:
            print(f"    错误：{err[:300]}")
        if last:
            print(f"    最后一行：{last[:200]}")
        for w in rec["warnings"][:5]:
            print(f"    警告：{str(w)[:200]}")

    ok = [r for r in results if r["status"] == "succeeded"]
    bad = [r for r in results if r["status"] != "succeeded"]
    print(f"\n===== 汇总：{len(ok)} 成功 / {len(bad)} 未成功 / 共 {len(results)} =====")
    for r in bad:
        print(f"  ✗ {r['id']:22s} status={r['status']} err={(r.get('error') or '')[:120]}")
    # 任务成功但没有任何真实痕迹的，也要单独点出来 —— 这正是"谎报成功"的形态
    sus = [r for r in ok
           if not r["state"].get("service_registered") and not r["state"].get("market_installed")]
    if sus:
        print("\n！任务报成功但复核不到任何真实痕迹（需要人工判断）：")
        for r in sus:
            print(f"  ? {r['id']:22s} {r['state']}")

    if json_out:
        with open(json_out, "w", encoding="utf-8") as f:
            json.dump({"base": base, "results": results}, f,
                      ensure_ascii=False, indent=2)
        print(f"\n结果已写入 {json_out}")


if __name__ == "__main__":
    main()
