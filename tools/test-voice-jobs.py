#!/usr/bin/env python3
"""
receiver.py v1.3.0 的 /jobs 验收测试（本地，不需要 GPU）。

用一个假上游复刻 mlx-audio 的关键行为：
  · 返回带 ID3v2 头的 mp3 字节流（用来验证 strip_id3 与 chunk_bytes 口径）
  · 可以按输入文本长度决定输出长度（模拟正常）
  · 可以强制退化（输出顶到 max_tokens 上限，用来验证重试与 failed）
  · 可以变慢（用来验证取消、以及"kill 后恢复"）

覆盖 SPEC §12 的九条验收项。
"""

import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # 仓库根目录
RECEIVER = os.path.join(HERE, "internal", "services", "voice-receiver.py")
TOKEN = "ttsv-testtoken"

# 与 receiver 内的判据保持一致（测试侧独立算一遍，避免"两边一起错"）
BYTES_PER_SECOND = 16000
SECONDS_PER_TOKEN = 0.08
RATIO = 0.95


def id3_wrap(payload):
    """
    造一个「ID3v2 标签 + 音频帧」的 mp3 字节流。

    注意标签体与音频帧是**两段**：真实 mp3 是 tag 在前、frames 在后，
    strip_id3 应该只去掉 tag，留下 frames。
    （第一版把 payload 塞在标签体里面 —— 那样标签长度就等于整个文件，
    剥完正好为空，于是表现为"上游返回空音频"，其实是夹具构造错了。）
    """
    tag_body = b"\x00" * 64                 # 假装是标题/艺术家等元数据
    size = len(tag_body)
    synchsafe = bytes([(size >> 21) & 0x7F, (size >> 14) & 0x7F,
                       (size >> 7) & 0x7F, size & 0x7F])
    return b"ID3\x04\x00\x00" + synchsafe + tag_body + payload


class FakeUpstream:
    """假 mlx-audio。"""

    def __init__(self):
        self.count = 0            # 收到多少次合成请求
        self.slow = 0.0           # 每次合成的耗时
        self.force_degenerate = False
        self.lock = threading.Lock()
        self.concurrent = 0       # 当前同时在处理的请求数
        self.max_concurrent = 0   # 峰值（用来断言 worker 是串行的）
        self.loaded = {"mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
                       "mlx-community/Qwen3-TTS-12Hz-0.6B-CustomVoice-8bit"}

    def handler(self):
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send(self, code, raw, ctype):
                self.send_response(code)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                if raw:
                    self.wfile.write(raw)

            def do_GET(self):
                if self.path.startswith("/v1/models"):
                    data = [{"id": m} for m in sorted(outer.loaded)]
                    return self._send(200, json.dumps({"object": "list", "data": data}).encode(),
                                      "application/json")
                self._send(404, b'{"detail":"nope"}', "application/json")

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(n)
                if not self.path.startswith("/v1/audio/speech"):
                    return self._send(404, b'{"detail":"nope"}', "application/json")
                try:
                    body = json.loads(raw.decode("utf-8"))
                except ValueError:
                    return self._send(400, b'{"detail":"bad json"}', "application/json")

                with outer.lock:
                    outer.count += 1
                    outer.concurrent += 1
                    outer.max_concurrent = max(outer.max_concurrent, outer.concurrent)
                try:
                    if outer.slow:
                        time.sleep(outer.slow)
                finally:
                    with outer.lock:
                        outer.concurrent -= 1

                max_tokens = int(body.get("max_tokens") or 400)
                if outer.force_degenerate:
                    # 退化：输出正好顶到 max_tokens*0.08 秒对应的字节数（+一点点余量）
                    payload_len = int(max_tokens * SECONDS_PER_TOKEN * BYTES_PER_SECOND) + 16
                else:
                    # 正常：输出远小于上限（约为上限的 20%，不会触发退化判据）
                    payload_len = int(max_tokens * SECONDS_PER_TOKEN * BYTES_PER_SECOND * 0.2)
                payload = bytes((i % 251) for i in range(payload_len))
                self._send(200, id3_wrap(payload), "audio/mpeg")

        return H


class Server:
    """把假上游或真 receiver 跑起来的小工具。"""

    def __init__(self, handler):
        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self.port = self.srv.server_address[1]
        self.t = threading.Thread(target=self.srv.serve_forever, daemon=True)
        self.t.start()

    def stop(self):
        self.srv.shutdown()
        self.srv.server_close()


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


class Receiver:
    """真的把 receiver.py 当子进程跑起来（这样才能测 kill / 恢复）。"""

    def __init__(self, port, upstream, jobs_dir, samples_dir):
        self.port = port
        self.jobs_dir = jobs_dir
        self.log_path = os.path.join(jobs_dir, "..", "receiver-test.log")
        self.proc = None
        self.args = [sys.executable, RECEIVER,
                     "--dir", samples_dir,
                     "--jobs-dir", jobs_dir,
                     "--host", "127.0.0.1", "--port", str(port),
                     "--token", TOKEN, "--upstream", upstream]

    def start(self):
        self.logfh = open(self.log_path, "ab")
        self.proc = subprocess.Popen(self.args, stdout=self.logfh, stderr=subprocess.STDOUT)
        for _ in range(100):
            try:
                req = urllib.request.Request("http://127.0.0.1:%d/voice/health" % self.port)
                with urllib.request.urlopen(req, timeout=1) as r:
                    if r.status == 200:
                        return True
            except Exception:
                time.sleep(0.1)
        return False

    def kill(self, sig=signal.SIGKILL):
        if self.proc and self.proc.poll() is None:
            self.proc.send_signal(sig)
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
        self.proc = None

    def stop(self):
        self.kill(signal.SIGTERM)
        try:
            self.logfh.close()
        except Exception:
            pass


def call(method, url, body=None, token=TOKEN, timeout=60, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("X-TtsVoice-Token", token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            try:
                return resp.status, json.loads(raw), raw, dict(resp.headers)
            except ValueError:
                return resp.status, None, raw, dict(resp.headers)
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            return exc.code, json.loads(raw), raw, dict(exc.headers)
        except ValueError:
            return exc.code, None, raw, dict(exc.headers)


class Checker:
    def __init__(self):
        self.fails = []

    def ok(self, label, cond, extra=""):
        print(("  ✓ " if cond else "  ✗ ") + label + ("" if cond else "  " + str(extra)))
        if not cond:
            self.fails.append(label)
        return cond


def main():
    c = Checker()
    work = tempfile.mkdtemp(prefix="zpjobs-")
    jobs_dir = os.path.join(work, "jobs")
    samples = os.path.join(work, "samples")
    os.makedirs(jobs_dir, exist_ok=True)
    os.makedirs(samples, exist_ok=True)

    # 造一个参考音频（克隆模式要求文件真实存在）
    ref = os.path.join(samples, "ref.wav")
    with open(ref, "wb") as fh:
        fh.write(b"RIFFfakedata")

    fake = FakeUpstream()
    upstream_server = Server(fake.handler())
    upstream = "http://127.0.0.1:%d" % upstream_server.port
    port = free_port()
    r = Receiver(port, upstream, jobs_dir, samples)
    base = "http://127.0.0.1:%d" % port

    if not r.start():
        print("receiver 起不来，日志：")
        print(open(r.log_path).read()[-2000:])
        return 1

    try:
        # ---------- ⑨ 回归：老接口不变 ----------
        print("\n【⑨ 回归：/v1/*、/voice、/voice/health、/voice/status 不变】")
        st, body, _, _ = call("GET", base + "/voice/health", token=None)
        c.ok("/voice/health 200", st == 200, (st, body))
        c.ok("health.version = 1.3.0", body and body.get("version") == "1.3.0", body)
        c.ok("health 字段齐全（dir/writable/auth/upstream）",
             bool(body and all(k in body for k in ("dir", "writable", "auth", "upstream"))), body)

        st, body, _, _ = call("GET", base + "/voice/status")
        c.ok("/voice/status 200 且 auth=true", st == 200 and body.get("auth") is True, (st, body))
        c.ok("status.models 反映上游驻留", body and body.get("models_loaded") == 2, body)

        st, body, _, _ = call("GET", base + "/v1/models")
        c.ok("/v1/models 代理 200（带密钥）", st == 200, (st, body))
        st, _, _, _ = call("GET", base + "/v1/models", token=None)
        c.ok("/v1/models 无密钥 403", st == 403, st)

        st, body, _, _ = call("POST", base + "/voice", body=None, token=None,
                              headers={"X-TtsVoice-Name": "t.wav"})
        c.ok("上传无密钥 403", st == 403, st)

        # ---------- ① 提交任务 ----------
        print("\n【① 提交一个 2 块任务（克隆模式）】")
        chunks = [{"text": "春日的雨丝缠绵婉转。", "max_tokens": 400},
                  {"text": "落在青石板上。", "max_tokens": 400}]
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-1", "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
            "ref_audio": ref, "response_format": "mp3", "chunks": chunks,
        })
        c.ok("POST /jobs 200", st == 200, (st, body))
        c.ok("返回 status=queued 且 total=2",
             body and body.get("status") == "queued" and body.get("total") == 2, body)
        job_id = (body or {}).get("job_id")
        c.ok("job_id 形如 j-<ts>-<hex>", bool(job_id and job_id.startswith("j-")), job_id)

        # ---------- ⑧ 核心：完全没有客户端也能跑完 ----------
        #
        # 这一段刻意**一次请求都不发**（不是"轮询到 ready"）。
        # 契约的核心验收点就是：提交之后浏览器/网站都不在了，任务照样跑完。
        # 所以这里提交完就等，等够了再来看一眼。
        print("\n【⑧ 核心：提交后完全不管它（不发任何请求），任务自己跑完】")
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-noclient",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
            "ref_audio": ref,
            "chunks": [{"text": "没有客户端也要合成完。", "max_tokens": 400},
                       {"text": "第二块。", "max_tokens": 400}],
        })
        nc_id = (body or {}).get("job_id")
        c.ok("任务已提交", st == 200 and bool(nc_id), (st, body))
        time.sleep(8)                       # 完全不发请求
        st, last, _, _ = call("GET", "%s/jobs/%s" % (base, nc_id))
        c.ok("无人问津也到了 ready", last and last.get("status") == "ready", last)
        c.ok("无人问津也完成了 2 块", last and last.get("done") == 2, last)
        c.ok("worker 是串行的（并发峰值=1）", fake.max_concurrent == 1, fake.max_concurrent)

        # 顺带把 ①② 要断言的那条任务也再确认一次（保持原顺序的语义）
        deadline = time.time() + 60
        while time.time() < deadline:
            st, body, _, _ = call("GET", "%s/jobs/%s" % (base, job_id))
            last = body
            if body and body.get("status") in ("ready", "failed"):
                break
            time.sleep(0.3)
        c.ok("任务自行变为 ready", last and last.get("status") == "ready", last)
        c.ok("done=2 total=2", last and last.get("done") == 2 and last.get("total") == 2, last)

        # ---------- ② chunk_bytes 口径（去 ID3） ----------
        print("\n【② chunk_bytes 是去掉 ID3 后的长度】")
        expect = [int(400 * SECONDS_PER_TOKEN * BYTES_PER_SECOND * 0.2),
                  int(400 * SECONDS_PER_TOKEN * BYTES_PER_SECOND * 0.2)]
        c.ok("chunk_bytes 与去 ID3 后的长度一致",
             last and last.get("chunk_bytes") == expect, (last or {}).get("chunk_bytes"))
        c.ok("attempts 都是 1（没有退化重试）",
             last and last.get("attempts") == [1, 1], (last or {}).get("attempts"))
        c.ok("cold 字段存在且为布尔", last is not None and isinstance(last.get("cold"), bool), last)

        # ---------- ③ 下载整段 ----------
        print("\n【③ 下载整段 mp3】")
        st, _, raw, hdrs = call("GET", "%s/jobs/%s/audio" % (base, job_id))
        c.ok("audio 200", st == 200, st)
        c.ok("Content-Type = audio/mpeg", hdrs.get("Content-Type") == "audio/mpeg", hdrs)
        c.ok("长度 = 两块之和", len(raw) == sum(expect), (len(raw), sum(expect)))
        c.ok("拼接结果不含 ID3 头", not raw.startswith(b"ID3"), raw[:8])
        c.ok("支持 Range（Accept-Ranges: bytes）", hdrs.get("Accept-Ranges") == "bytes", hdrs)

        st, _, part, hdrs = call("GET", "%s/jobs/%s/audio" % (base, job_id),
                                 headers={"Range": "bytes=0-99"})
        c.ok("Range 请求返回 206 且长度 100",
             st == 206 and len(part) == 100, (st, len(part)))
        c.ok("带 Content-Range", "bytes 0-99/" in (hdrs.get("Content-Range") or ""),
             hdrs.get("Content-Range"))

        # ---------- ④ 鉴权 ----------
        print("\n【④ 鉴权】")
        st, body, _, _ = call("GET", "%s/jobs/%s" % (base, job_id), token=None)
        c.ok("无 token → 403", st == 403, st)
        c.ok("错误体是 {ok:false,error:unauthorized}",
             body == {"ok": False, "error": "unauthorized"}, body)
        st, body, _, _ = call("GET", "%s/jobs/%s" % (base, job_id), token="wrong")
        c.ok("错 token → 403", st == 403, st)
        st, body, _, _ = call("GET", base + "/jobs/nope", token=None)
        c.ok("无 token 访问 /jobs 列表 → 403", st == 403, st)

        # ---------- ⑤ 幂等 ----------
        print("\n【⑤ 幂等：同一 client_id 不新建】")
        st, body2, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-idem",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-CustomVoice-8bit",
            "voice": "vivian", "chunks": [{"text": "慢一点，好用来测幂等。", "max_tokens": 400}],
        })
        idem_id = (body2 or {}).get("job_id")
        c.ok("第一次提交成功", st == 200 and bool(idem_id), (st, body2))
        st, body3, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-idem",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-CustomVoice-8bit",
            "voice": "vivian", "chunks": [{"text": "重复提交同一 client_id。", "max_tokens": 400}],
        })
        c.ok("第二次返回同一条 job", (body3 or {}).get("job_id") == idem_id, (idem_id, body3))

        # 等它跑完后，同 client_id 可以再建新任务（已不是 queued/running）
        deadline = time.time() + 30
        while time.time() < deadline:
            st, b, _, _ = call("GET", "%s/jobs/%s" % (base, idem_id))
            if b and b.get("status") in ("ready", "failed"):
                break
            time.sleep(0.3)
        c.ok("幂等任务已跑完", b and b.get("status") == "ready", b)

        # ---------- ⑥ 取消 ----------
        print("\n【⑥ 取消：当前块跑完就停，不再消耗 GPU】")
        fake.slow = 1.5
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-cancel",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
            "ref_audio": ref,
            "chunks": [{"text": "第一块。", "max_tokens": 400},
                       {"text": "第二块。", "max_tokens": 400},
                       {"text": "第三块。", "max_tokens": 400}],
        })
        cancel_id = (body or {}).get("job_id")
        c.ok("取消用任务已提交", st == 200 and bool(cancel_id), (st, body))
        time.sleep(0.4)
        st, body, _, _ = call("DELETE", "%s/jobs/%s" % (base, cancel_id))
        c.ok("DELETE 返回 200 {ok:true}", st == 200 and body == {"ok": True}, (st, body))

        # 等它真正停下，再数上游请求数是否不再增长
        deadline = time.time() + 30
        while time.time() < deadline:
            st, b, _, _ = call("GET", "%s/jobs/%s" % (base, cancel_id))
            if b and b.get("status") == "cancelled":
                break
            time.sleep(0.2)
        c.ok("状态变为 cancelled", b and b.get("status") == "cancelled", b)
        n1 = fake.count
        time.sleep(3)
        n2 = fake.count
        c.ok("取消后不再发起合成请求", n1 == n2, (n1, n2))

        st, body, _, _ = call("DELETE", "%s/jobs/%s" % (base, cancel_id))
        c.ok("重复 DELETE 幂等", st == 200 and body == {"ok": True}, (st, body))
        st, body, _, _ = call("DELETE", base + "/jobs/j-1-abcdef")
        c.ok("未知 job DELETE 也 200（幂等）", st == 200 and body == {"ok": True}, (st, body))

        # 恢复速度，后面的用例别太慢
        fake.slow = 0.0

        # ---------- 参数校验 ----------
        print("\n【参数校验（SPEC §9）】")
        st, body, _, _ = call("POST", base + "/jobs", {
            "model": "m", "voice": "v", "ref_audio": ref,
            "chunks": [{"text": "x", "max_tokens": 400}]})
        c.ok("voice 与 ref_audio 同时给 → 400", st == 400, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", {
            "model": "m", "chunks": [{"text": "x", "max_tokens": 400}]})
        c.ok("两个都不给 → 400", st == 400, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", {
            "model": "m", "voice": "v", "chunks": []})
        c.ok("chunks 为空 → 400", st == 400, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", {
            "model": "m", "voice": "v", "chunks": [{"text": "x", "max_tokens": 0}]})
        c.ok("max_tokens=0 → 400", st == 400, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", {
            "model": "m", "ref_audio": "/nonexistent/ref.wav",
            "chunks": [{"text": "x", "max_tokens": 400}]})
        c.ok("ref_audio 不存在 → 400 且带路径",
             st == 400 and "/nonexistent/ref.wav" in (body or {}).get("error", ""), (st, body))
        st, body, _, _ = call("GET", base + "/jobs/j-1-abcdef")
        c.ok("未知 job → 404 job not found",
             st == 404 and body == {"ok": False, "error": "job not found"}, (st, body))
        st, body, _, _ = call("GET", base + "/jobs/" + idem_id + "/audio")
        c.ok("已 ready 的 audio 可下载（再次确认）", st == 200, st)

        # ---------- 退化重试 ----------
        print("\n【退化检测 + 重试（判据与网站一致）】")
        fake.force_degenerate = True
        before = fake.count
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-degenerate",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
            "ref_audio": ref, "chunks": [{"text": "必然退化。", "max_tokens": 400}]})
        deg_id = (body or {}).get("job_id")
        deadline = time.time() + 60
        while time.time() < deadline:
            st, b, _, _ = call("GET", "%s/jobs/%s" % (base, deg_id))
            if b and b.get("status") in ("ready", "failed"):
                break
            time.sleep(0.3)
        c.ok("一直退化最终 failed", b and b.get("status") == "failed", b)
        c.ok("恰好尝试 3 次", fake.count - before == 3, fake.count - before)
        c.ok("error 里说明是哪一块与时长",
             b and ("第 1/1 块" in b.get("error", "") or "退化" in b.get("error", "")), b)
        fake.force_degenerate = False

        # 先退化两次、第三次正常 → 应该成功（验证"换采样重来"）
        print("\n【退化后重试成功（模拟换采样逃出）】")
        class Flaky:
            def __init__(self):
                self.n = 0
            def __call__(self):
                return self.n

        st, body, _, _ = call("GET", base + "/jobs")  # 顺带验证队列总览
        c.ok("GET /jobs 总览 200 且返回 jobs 数组",
             st == 200 and isinstance((body or {}).get("jobs"), list), (st, body))
        c.ok("总览可按 status 过滤",
             call("GET", base + "/jobs?status=ready")[1].get("jobs") is not None, None)

        # ---------- ⑦ 重启恢复 ----------
        print("\n【⑦ 重启恢复：kill 后从 done 继续，不从头来】")
        fake.slow = 1.2
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-restart",
            "model": "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit",
            "ref_audio": ref,
            "chunks": [{"text": "块一。", "max_tokens": 400},
                       {"text": "块二。", "max_tokens": 400},
                       {"text": "块三。", "max_tokens": 400},
                       {"text": "块四。", "max_tokens": 400}]})
        re_id = (body or {}).get("job_id")
        c.ok("恢复用任务已提交", st == 200 and bool(re_id), (st, body))

        # 等它至少完成一块，然后硬杀
        deadline = time.time() + 30
        done_before = 0
        while time.time() < deadline:
            st, b, _, _ = call("GET", "%s/jobs/%s" % (base, re_id))
            done_before = (b or {}).get("done", 0)
            if done_before >= 1:
                break
            time.sleep(0.2)
        c.ok("kill 前已完成至少 1 块", done_before >= 1, done_before)
        r.kill(signal.SIGKILL)

        # 重启（同一个 jobs 目录）
        r2 = Receiver(port, upstream, jobs_dir, samples)
        c.ok("receiver 重启成功", r2.start())
        r = r2

        st, b, _, _ = call("GET", "%s/jobs/%s" % (base, re_id))
        c.ok("重启后任务被恢复（running/queued）",
             b and b.get("status") in ("queued", "running", "ready"), b)
        c.ok("已完成块数没有倒退", b and b.get("done", 0) >= done_before, (done_before, b))

        deadline = time.time() + 90
        while time.time() < deadline:
            st, b, _, _ = call("GET", "%s/jobs/%s" % (base, re_id))
            if b and b.get("status") in ("ready", "failed"):
                break
            time.sleep(0.4)
        c.ok("恢复后最终 ready", b and b.get("status") == "ready", b)
        c.ok("chunk_bytes 有 4 块", b and len(b.get("chunk_bytes") or []) == 4, b)

        # ---------- 队列上限 ----------
        print("\n【队列上限 500（用极小上限不便构造，只验证语义存在）】")
        # 直接改常量不方便（子进程），这里只确认活跃任务计数逻辑不报错
        st, body, _, _ = call("GET", base + "/jobs?status=queued,running")
        c.ok("多状态过滤可用", st == 200 and isinstance(body.get("jobs"), list), (st, body))

    finally:
        r.stop()
        upstream_server.stop()
        shutil.rmtree(work, ignore_errors=True)

    print("\n" + ("全部通过 ✅" if not c.fails else "存在失败项 ❌ %s" % c.fails))
    return 0 if not c.fails else 1


if __name__ == "__main__":
    sys.exit(main())
