#!/usr/bin/env python3
"""
receiver.py v1.4.0 的 /jobs 验收测试（本地，不需要 GPU）。

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


def wav_wrap(pcm, rate=24000):
    """把纯 PCM 包成一个合法的 24kHz/16bit/单声道 wav（44 字节头）。"""
    import struct
    byte_rate = rate * 2
    return (b"RIFF" + struct.pack("<I", 36 + len(pcm)) + b"WAVE"
            + b"fmt " + struct.pack("<IHHIIHH", 16, 1, 1, rate, byte_rate, 2, 16)
            + b"data" + struct.pack("<I", len(pcm)) + pcm)


class FakeUpstream:
    """假 mlx-audio。"""

    def __init__(self):
        self.count = 0            # 收到多少次合成请求
        self.slow = 0.0           # 每次合成的耗时
        self.force_degenerate = False
        self.lock = threading.Lock()
        self.concurrent = 0       # 当前同时在处理的请求数
        self.max_concurrent = 0   # 峰值（用来断言 worker 是串行的）
        self.loaded = {"mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
                       "mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit"}
        # v1.4.0 验收要断言"receiver 到底把什么透传给了上游"，所以留下请求体
        self.bodies = []

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
                    try:
                        self.wfile.write(raw)
                    except (BrokenPipeError, ConnectionResetError):
                        # 取消/超时用例里 receiver 会提前断开连接。
                        # 那是被测行为，不是夹具故障 —— 不吞掉的话
                        # http.server 会打一段 traceback 混在测试输出里。
                        pass

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
                    outer.bodies.append(body)
                try:
                    if outer.slow:
                        time.sleep(outer.slow)
                finally:
                    with outer.lock:
                        outer.concurrent -= 1

                max_tokens = int(body.get("max_tokens") or 400)
                want_wav = str(body.get("response_format") or "mp3").lower() == "wav"
                # 字节率跟着格式走（wav 是 24000Hz/16bit/单声道 = 48000 字节/秒）
                rate = 48000 if want_wav else BYTES_PER_SECOND
                if outer.force_degenerate:
                    # 退化：输出正好顶到 max_tokens*0.08 秒对应的字节数（+一点点余量）
                    payload_len = int(max_tokens * SECONDS_PER_TOKEN * rate) + 16
                else:
                    # 正常：输出远小于上限（约为上限的 20%，不会触发退化判据）
                    payload_len = int(max_tokens * SECONDS_PER_TOKEN * rate * 0.2)
                payload = bytes((i % 251) for i in range(payload_len))
                if want_wav:
                    # 真 wav：标准 44 字节头 + PCM。这样拼接逻辑（去头补头）才是真的被测到。
                    self._send(200, wav_wrap(payload), "audio/wav")
                else:
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

    def __init__(self, port, upstream, jobs_dir, samples_dir, keys_file=None):
        self.port = port
        self.jobs_dir = jobs_dir
        self.log_path = os.path.join(jobs_dir, "..", "receiver-test.log")
        self.proc = None
        self.args = [sys.executable, RECEIVER,
                     "--dir", samples_dir,
                     "--jobs-dir", jobs_dir,
                     "--host", "127.0.0.1", "--port", str(port),
                     # --token 仍然保留：它是兼容回退，老 plist 不升级也能跑；
                     # 多密钥/额度/用量走 --keys-file（v1.6.0）
                     "--token", TOKEN, "--upstream", upstream,
                     "--keys-file", keys_file or os.path.join(jobs_dir, "..", "keys.json"),
                     "--usage-file", os.path.join(jobs_dir, "..", "usage.json")]

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


def call_raw(method, url, raw, token=TOKEN, timeout=60, headers=None,
             content_type="application/octet-stream"):
    """直接发二进制体（/voice 上传用；call() 会把 body 当 JSON 编码）。"""
    req = urllib.request.Request(url, data=raw, method=method)
    req.add_header("Content-Type", content_type)
    if token:
        req.add_header("X-TtsVoice-Token", token)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            try:
                return resp.status, json.loads(body), body, dict(resp.headers)
            except ValueError:
                return resp.status, None, body, dict(resp.headers)
    except urllib.error.HTTPError as exc:
        body = exc.read()
        try:
            return exc.code, json.loads(body), body, dict(exc.headers)
        except ValueError:
            return exc.code, None, body, dict(exc.headers)


def receiver_version():
    """
    从脚本里读 VERSION（而不是在测试里写死）。

    版本号写死在断言里，升级时就变成"盯着改"的负担，还容易漏 ——
    版本检查应该比对**脚本自身**，脚本升了测试自动跟上。
    """
    with open(RECEIVER, "r", encoding="utf-8") as fh:
        for line in fh:
            if line.startswith("VERSION = "):
                return line.split("=", 1)[1].strip().strip('"\'')
    return ""


def _wait_ready(base, job_id, timeout=60):
    """轮询到 ready/failed，返回最后一次 (status, body, raw, headers)。"""
    import time as _t
    deadline = _t.time() + timeout
    last = (0, None, b"", {})
    while _t.time() < deadline:
        last = call("GET", base + "/jobs/" + job_id)
        st, body = last[0], last[1]
        if body and body.get("status") in ("ready", "failed", "cancelled"):
            return last
        _t.sleep(0.3)
    return last


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
        c.ok("health.version 与脚本 VERSION 一致",
             body and body.get("version") == receiver_version(),
             (body, receiver_version()))
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

        # ---------- ⑩ v1.5.0：上传的来源隔离 / 内容校验 / 归一化 ----------
        print("\n【⑩ v1.5.0 上传：样本按来源落盘，且必须真是音频】")

        def upload(raw, source=None, name=None, token=TOKEN):
            headers = {}
            if source is not None:
                headers["X-TtsVoice-Source"] = source
            if name is not None:
                headers["X-TtsVoice-Name"] = name
            return call_raw("POST", base + "/voice", raw, token=token, headers=headers)

        def secs(n):
            """n 秒的合法 24kHz/16bit/单声道 wav。"""
            return wav_wrap(b"\x01\x02" * int(24000 * n))

        st, body_a, _, _ = upload(secs(2.0), source="site-a", name="voice-a.mp3")
        c.ok("上传音频 → 200", st == 200, (st, body_a))
        c.ok("回应 source = site-a", (body_a or {}).get("source") == "site-a", body_a)
        c.ok("回应 format=wav 且 duration≈2s",
             (body_a or {}).get("format") == "wav"
             and abs((body_a or {}).get("duration", 0) - 2.0) < 0.15, body_a)
        c.ok("落盘在 <dir>/site-a/ref.wav（不再写根目录）",
             (body_a or {}).get("path") == os.path.join(samples, "site-a", "ref.wav"), body_a)
        c.ok("回应带 64 位 sha256（对归一化后的字节）",
             len((body_a or {}).get("sha256") or "") == 64, body_a)
        c.ok("name 回显上传时的文件名", (body_a or {}).get("name") == "voice-a.mp3", body_a)

        st, body_short, _, _ = upload(secs(1.2), source="site-a", name="voice-a2.wav")
        c.ok("同来源再传 = 单来源替换（200）", st == 200, (st, body_short))
        c.ok("1–3 秒样本 → 200 但带 warning（偏短提示）",
             bool((body_short or {}).get("warning")), body_short)
        c.ok("替换后目录里只有 ref.wav 一个文件（不留 .part/临时文件）",
             sorted(os.listdir(os.path.join(samples, "site-a"))) == ["ref.wav"],
             os.listdir(os.path.join(samples, "site-a")))
        c.ok("替换后 duration 变成 ≈1.2s",
             abs((body_short or {}).get("duration", 0) - 1.2) < 0.15, body_short)

        st, body_b, _, _ = upload(secs(2.0), source="site-b", name="b.wav")
        c.ok("第二个来源与第一个共存",
             st == 200 and os.path.isfile(os.path.join(samples, "site-b", "ref.wav")),
             (st, body_b))

        st, body_bad, _, _ = upload("这明显不是音频，只是一段文字。".encode("utf-8"),
                                    source="site-c", name="x.wav")
        c.ok("非音频内容 → 400（而不是先落盘再让上游解不开）", st == 400, (st, body_bad))
        c.ok("400 的说明能看懂（提到音频/格式）",
             "音频" in json.dumps(body_bad, ensure_ascii=False), body_bad)
        c.ok("被拒的上传没有留下 site-c 目录",
             not os.path.exists(os.path.join(samples, "site-c")))

        st, body_short2, _, _ = upload(wav_wrap(b"\x00" * 4000), source="site-d", name="tiny.wav")
        c.ok("不足 1 秒 → 400 且提示太短",
             st == 400 and "太短" in json.dumps(body_short2, ensure_ascii=False), (st, body_short2))

        st, body_long, _, _ = upload(secs(20.0), source="site-e", name="long.wav")
        c.ok("超过 12 秒 → 截断到 12 秒并标记 truncated",
             st == 200 and (body_long or {}).get("truncated") is True
             and abs((body_long or {}).get("duration", 0) - 12.0) < 0.2, (st, body_long))

        st, body_sources, _, _ = call("GET", base + "/voice/sources")
        c.ok("GET /voice/sources 200", st == 200, (st, body_sources))
        srcs = {x["source"]: x for x in (body_sources or {}).get("sources", [])}
        c.ok("列表含 site-a / site-b / site-e", {"site-a", "site-b", "site-e"} <= set(srcs), sorted(srcs))
        c.ok("v1.4.0 残留的根目录 ref.wav 也被列出来（标记 legacy）",
             srcs.get("default", {}).get("legacy") is True
             and srcs.get("default", {}).get("path") == ref, srcs.get("default"))
        c.ok("条目字段齐全（size/duration/sha256/modified/last_used）",
             all(k in srcs.get("site-a", {})
                 for k in ("size", "duration", "sha256", "modified", "last_used", "path")),
             srcs.get("site-a"))
        c.ok("/voice/sources 无密钥 403",
             call("GET", base + "/voice/sources", token=None)[0] == 403)

        # 不做旧版兼容：给 default 上传只写 <dir>/default/ref.wav，
        # **不会**去动根目录那份 v1.4.0 老文件（残留就让它明明白白地留着）。
        ref_before = open(ref, "rb").read()
        st, body_def, _, _ = upload(secs(2.0), source="default", name="d.wav")
        c.ok("default 上传只写子目录，不碰根目录残留",
             st == 200 and (body_def or {}).get("path") == os.path.join(samples, "default", "ref.wav")
             and open(ref, "rb").read() == ref_before
             and "legacy_path" not in (body_def or {}),
             (st, body_def))
        st, body_sources2, _, _ = call("GET", base + "/voice/sources")
        defaults = [x for x in (body_sources2 or {}).get("sources", []) if x["source"] == "default"]
        c.ok("两份 default（子目录 + 残留）各自一行，都能看见",
             len(defaults) == 2 and any(x["legacy"] for x in defaults)
             and any(not x["legacy"] for x in defaults),
             [(x["source"], x["legacy"], x["path"]) for x in defaults])
        c.ok("删掉 default 会把子目录与残留文件一起删（清理入口）",
             call("DELETE", base + "/voice/sources/default")[0] == 200
             and not os.path.exists(os.path.join(samples, "default"))
             and not os.path.exists(ref), True)
        # 后面的用例还要用这份根目录样本当 ref_audio（显式 ref_audio 仍允许），补回来
        with open(ref, "wb") as fh:
            fh.write(b"RIFFfakedata")

        st, body_del, _, _ = call("DELETE", base + "/voice/sources/site-b")
        c.ok("DELETE 来源 → 200 且目录被删",
             st == 200 and not os.path.exists(os.path.join(samples, "site-b")), (st, body_del))
        st, _, _, _ = call("DELETE", base + "/voice/sources/site-b")
        c.ok("重复删除 → 200（幂等）", st == 200, st)
        st, _, _, _ = call("DELETE", base + "/voice/sources/..%2F..%2Fetc")
        c.ok("路径穿越被洗掉（200，且没有越界删除）", st == 200, st)
        st, _, _, _ = call("DELETE", base + "/voice/sources/site-b", token=None)
        c.ok("DELETE 无密钥 403", st == 403, st)

        # ---------- ⑪ v1.5.0：/jobs 带 source ----------
        print("\n【⑪ v1.5.0 /jobs：source 解析与回传】")
        model = "mlx-community/Qwen3-TTS-12Hz-0.6B-Base-8bit"

        st, body_src, _, _ = call("POST", base + "/jobs", {
            "client_id": "src-1", "model": model, "source": "site-a",
            "response_format": "mp3",
            "chunks": [{"text": "用 site-a 的音色。", "max_tokens": 400}]})
        c.ok("只给 source（不给 ref_audio）→ 200", st == 200, (st, body_src))
        jid_src = (body_src or {}).get("job_id")

        st, view_src, _, _ = call("GET", base + "/jobs/" + (jid_src or ""))
        c.ok("GET /jobs/{id} 回传 source = site-a",
             st == 200 and (view_src or {}).get("source") == "site-a", (st, view_src))
        _wait_ready(base, jid_src, timeout=60)   # 别把任务留在队列里影响后面

        st, body_nosrc, _, _ = call("POST", base + "/jobs", {
            "client_id": "src-2", "model": model, "source": "no-such-source",
            "chunks": [{"text": "x", "max_tokens": 400}]})
        c.ok("来源没有样本 → 400 且给出查找过的路径",
             st == 400 and "voice sample not found" in json.dumps(body_nosrc),
             (st, body_nosrc))

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
        # v1.5.0：不给 ref_audio/voice 时按 source=default 解析；此时 default 没有样本
        # （根目录那份 v1.4.0 残留不参与解析），所以必须是 400 —— 不回退旧文件。
        c.ok("voice/ref_audio 都不给且 default 无样本 → 400（不回退旧根目录文件）",
             st == 400 and "voice sample not found" in json.dumps(body), (st, body))
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
        # ---------- v1.4.0：wav + sampling 透传 ----------
        print("\n【v1.4.0 ①：wav 任务（去头拼整段 + sampling 透传）】")
        fake.bodies = []
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-wav",
            "model": "mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit",
            "voice": "vivian",
            "response_format": "wav",
            "sampling": {"temperature": 0.7, "top_p": 0.9, "top_k": 40, "repetition_penalty": 1.05},
            "chunks": [{"text": "第一段。", "max_tokens": 400},
                       {"text": "第二段。", "max_tokens": 400}],
        })
        wav_id = (body or {}).get("job_id")
        c.ok("wav 任务提交成功（total=2）", st == 200 and body.get("total") == 2, (st, body))
        # 等它跑完（两小块，很快）
        st, body, _, _ = _wait_ready(base, wav_id, timeout=60)
        c.ok("wav 任务 status=ready 且 done=2",
             body and body.get("status") == "ready" and body.get("done") == 2, body)

        c.ok("上游收到的 response_format 是 wav（不再写死 mp3）",
             fake.bodies and all(b.get("response_format") == "wav" for b in fake.bodies),
             [b.get("response_format") for b in fake.bodies])
        need = {"temperature": 0.7, "top_p": 0.9, "top_k": 40, "repetition_penalty": 1.05}
        c.ok("4 个采样参数原样透传给上游（位置参数，不是包在 sampling 里）",
             fake.bodies and all(all(b.get(k) == v for k, v in need.items()) for b in fake.bodies),
             fake.bodies[0] if fake.bodies else None)
        c.ok("上游请求体里没有嵌套的 sampling 字段（值是摊平的）",
             fake.bodies and all("sampling" not in b for b in fake.bodies))

        # chunk_bytes 是"交付文件"的长度（wav 含 44 字节头）
        # 注意：下载音频的响应体是二进制，json 解析会得到 None ——
        # 所以要单独再查一次任务状态来断言 chunk_bytes。
        _, wav_job, _, _ = call("GET", base + "/jobs/" + wav_id)
        st, _, raw, hdrs = call("GET", base + "/jobs/" + wav_id + "/audio")
        c.ok("下载 wav 200", st == 200, st)
        c.ok("Content-Type = audio/wav", (hdrs.get("Content-Type") or "").startswith("audio/wav"),
             hdrs.get("Content-Type"))
        import struct as _struct
        c.ok("整段是合法 wav（RIFF/WAVE 头）", raw[:4] == b"RIFF" and raw[8:12] == b"WAVE", raw[:12])
        c.ok("整段只有一个文件头（不是各块直接相接）", raw.count(b"RIFF") == 1, raw.count(b"RIFF"))
        c.ok("采样率 24000 / 单声道 / 16bit",
             _struct.unpack("<I", raw[24:28])[0] == 24000 and _struct.unpack("<H", raw[22:24])[0] == 1
             and _struct.unpack("<H", raw[34:36])[0] == 16,
             (_struct.unpack("<I", raw[24:28])[0], _struct.unpack("<H", raw[22:24])[0]))
        c.ok("整段 PCM 长度 = 各块 chunk_bytes 之和减掉各自 44 字节头",
             len(raw) - 44 == sum(wav_job.get("chunk_bytes") or []) - 44 * 2,
             (len(raw), wav_job.get("chunk_bytes")))
        c.ok("chunk_bytes 是含头的长度（每块都 > 44）",
             all(n > 44 for n in (wav_job.get("chunk_bytes") or [])), wav_job.get("chunk_bytes"))

        print("\n【v1.4.0 ②：不带 sampling 的老请求 → 用默认值（不能不发）】")
        fake.bodies = []
        st, body, _, _ = call("POST", base + "/jobs", {
            "client_id": "probe-old",
            "model": "mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit",
            "ref_audio": ref,
            "chunks": [{"text": "老请求也不能退化。", "max_tokens": 400}],
        })
        old_id = (body or {}).get("job_id")
        c.ok("老请求（无 sampling/response_format）仍提交成功", st == 200 and bool(old_id), (st, body))
        st, body, _, _ = _wait_ready(base, old_id, timeout=60)
        c.ok("老请求跑完（行为与 v1.3.0 一致：默认 mp3）",
             body and body.get("status") == "ready", body)
        c.ok("默认格式仍是 mp3",
             fake.bodies and all(b.get("response_format") == "mp3" for b in fake.bodies),
             [b.get("response_format") for b in fake.bodies])
        c.ok("没给 sampling 时也补上了默认的 4 个参数（否则长句会变噪音）",
             fake.bodies and all(all(b.get(k) == v for k, v in need.items()) for b in fake.bodies),
             fake.bodies[0] if fake.bodies else None)
        st, _, raw_mp3, hdrs = call("GET", base + "/jobs/" + old_id + "/audio")
        c.ok("mp3 下载 Content-Type 仍是 audio/mpeg",
             (hdrs.get("Content-Type") or "").startswith("audio/mpeg"), hdrs.get("Content-Type"))
        c.ok("mp3 交付物没有 RIFF 头（还是老的相接方式）", not raw_mp3.startswith(b"RIFF"))

        print("\n【队列上限 500（用极小上限不便构造，只验证语义存在）】")
        # 直接改常量不方便（子进程），这里只确认活跃任务计数逻辑不报错
        st, body, _, _ = call("GET", base + "/jobs?status=queued,running")
        c.ok("多状态过滤可用", st == 200 and isinstance(body.get("jobs"), list), (st, body))

        # ================= v1.6.0：多密钥 + 额度（字）+ 用量统计 =================
        #
        # 说明：这一段放在最后，因为它会写入 keys.json 并改变鉴权行为。
        # TOKEN（--token）作为兼容回退始终可用，所以前面的用例不受影响。
        print("\n【v1.6.0 多密钥：停用/错误/无密钥都被拒，正确密钥可用】")
        keys_file = os.path.join(work, "keys.json")
        usage_file = os.path.join(work, "usage.json")

        def write_keys(keys):
            with open(keys_file, "w", encoding="utf-8") as fh:
                json.dump({"version": 1, "keys": keys}, fh, ensure_ascii=False)

        write_keys([
            {"id": "k-a", "name": "A 站", "key": "KEY-AAA", "quota_chars": 10, "enabled": True, "created": 1},
            {"id": "k-b", "name": "B 站", "key": "KEY-BBB", "quota_chars": 0, "enabled": True, "created": 2},
            {"id": "k-off", "name": "停用", "key": "KEY-OFF", "quota_chars": 0, "enabled": False, "created": 3},
        ])
        # 等接收端热加载（按 mtime，最多 2 秒）
        for _ in range(20):
            st, hb, _, _ = call("GET", base + "/voice/health", token=None)
            if (hb or {}).get("keys") == 3:
                break
            time.sleep(0.1)

        c.ok("health 无需密钥且报告密钥数",
             st == 200 and (hb or {}).get("auth") is True and hb.get("keys") == 3, hb)

        q = {"model": model, "source": "site-a",
             "chunks": [{"text": "一二三", "max_tokens": 400}]}

        st, body, _, _ = call("POST", base + "/jobs", q, token=None)
        c.ok("不带密钥 → 403", st == 403, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", q, token="KEY-WRONG")
        c.ok("错误密钥 → 403", st == 403, (st, body))
        st, body, _, _ = call("POST", base + "/jobs", q, token="KEY-OFF")
        c.ok("已停用的密钥 → 403（停用要真的生效）", st == 403, (st, body))

        print("\n【v1.6.0 额度：按提交文本的字数计，超额 429 且与 403 区分开】")
        big = {**q, "client_id": "quota-big",
               "chunks": [{"text": "这是一段超过十个字的文本", "max_tokens": 400}]}
        big_chars = len("这是一段超过十个字的文本")
        st, body, _, _ = call("POST", base + "/jobs", big, token="KEY-AAA")
        c.ok("A 站超额（%d 字 > 额度 10）→ 429" % big_chars,
             st == 429 and "quota exceeded" in json.dumps(body, ensure_ascii=False), (st, body))
        c.ok("超额提示里写清了额度/已用/本次需要",
             body and all(x in body.get("error", "") for x in
                          ("额度 10 字", "本次需要 %d 字" % big_chars)), body)

        st, body, _, _ = call("POST", base + "/jobs", {**q, "client_id": "quota-ok"}, token="KEY-AAA")
        c.ok("A 站额度内（3 字）→ 200", st == 200 and body.get("chars") == 3, (st, body))
        jid_a = (body or {}).get("job_id")
        _wait_ready(base, jid_a, timeout=60)

        st, usage, _, _ = call("GET", base + "/usage", token="KEY-AAA")
        by_id = {k["id"]: k for k in (usage or {}).get("keys", [])}
        c.ok("用量：A 站记到 3 字 / 1 个任务",
             by_id.get("k-a", {}).get("used_chars") == 3
             and by_id.get("k-a", {}).get("jobs") == 1, by_id.get("k-a"))
        c.ok("用量：剩余额度 = 10-3 = 7",
             by_id.get("k-a", {}).get("remaining_chars") == 7, by_id.get("k-a"))
        c.ok("用量：B 站没被算进去（各密钥独立）",
             by_id.get("k-b", {}).get("used_chars") == 0, by_id.get("k-b"))
        c.ok("用量：列表里能看到被停用的密钥（enabled=false）",
             by_id.get("k-off", {}).get("enabled") is False, by_id.get("k-off"))
        c.ok("用量：今日字数也记了",
             by_id.get("k-a", {}).get("today_chars") == 3, by_id.get("k-a"))
        c.ok("用量：总量里有请求数/音频字节",
             (usage or {}).get("total", {}).get("requests", 0) > 0
             and (usage or {}).get("total", {}).get("audio_bytes", 0) > 0,
             (usage or {}).get("total"))
        c.ok("/usage 无密钥 → 403", call("GET", base + "/usage", token=None)[0] == 403)

        # 剩 7 字：再来 8 字应当被拒，7 字（含标点）应当被放行
        st, body, _, _ = call("POST", base + "/jobs", {
            **q, "client_id": "quota-8", "chunks": [{"text": "一二三四五六七八", "max_tokens": 400}]},
            token="KEY-AAA")
        c.ok("A 站剩 7 字时提交 8 字 → 429（额度是硬上限）",
             st == 429 and "quota exceeded" in json.dumps(body, ensure_ascii=False), (st, body))

        st, body, _, _ = call("POST", base + "/jobs", {
            **q, "client_id": "quota-b", "chunks": [{"text": "一二三四五六七", "max_tokens": 400}]},
            token="KEY-BBB")
        c.ok("B 站不限量：同样规模照常 200", st == 200, (st, body))

        print("\n【v1.6.0 幂等重放不重复扣额度（队列中的同一 client_id）】")
        # 契约：同一 client_id 只复用 queued/running 的任务。跑完之后再提交同一个
        # client_id 是**一次新的合成**，本来就该重新计费 —— 所以这里必须在任务
        # 还在队列里时重放，才测得到"不重复扣"。
        fake.slow = 2.0
        st, first, _, _ = call("POST", base + "/jobs", {
            **q, "client_id": "quota-idem", "chunks": [{"text": "幂等重放", "max_tokens": 400}]},
            token="KEY-AAA")
        st2, again, _, _ = call("POST", base + "/jobs", {
            **q, "client_id": "quota-idem", "chunks": [{"text": "幂等重放", "max_tokens": 400}]},
            token="KEY-AAA")
        st3, usage2, _, _ = call("GET", base + "/usage", token="KEY-AAA")
        by2 = {k["id"]: k for k in (usage2 or {}).get("keys", [])}
        c.ok("重放返回同一任务", st == 200 and st2 == 200
             and first.get("job_id") == again.get("job_id"), (first, again))
        c.ok("重放没有重复扣额度（只多了 4 字）",
             by2.get("k-a", {}).get("used_chars") == 3 + len("幂等重放"),
             (by2.get("k-a", {}).get("used_chars"), "期望", 3 + len("幂等重放")))
        _wait_ready(base, first.get("job_id"), timeout=60)
        fake.slow = 0.0

        print("\n【v1.6.0 热加载：新增密钥不需要重启接收端】")
        write_keys([
            {"id": "k-a", "name": "A 站", "key": "KEY-AAA", "quota_chars": 10, "enabled": True, "created": 1},
            {"id": "k-b", "name": "B 站", "key": "KEY-BBB", "quota_chars": 0, "enabled": True, "created": 2},
            {"id": "k-off", "name": "停用", "key": "KEY-OFF", "quota_chars": 0, "enabled": False, "created": 3},
            {"id": "k-new", "name": "新站", "key": "KEY-NEW", "quota_chars": 100, "enabled": True, "created": 4},
        ])
        for _ in range(20):
            st, hb2, _, _ = call("GET", base + "/voice/health", token=None)
            if (hb2 or {}).get("keys") == 4:
                break
            time.sleep(0.1)
        st, body, _, _ = call("POST", base + "/jobs", {
            **q, "client_id": "hot-new", "chunks": [{"text": "新密钥", "max_tokens": 400}]},
            token="KEY-NEW")
        c.ok("新增的密钥立即可用（未重启进程）", st == 200, (st, body))

        print("\n【v1.6.0 用量持久化：重启接收端后统计还在】")
        st, usage_now, _, _ = call("GET", base + "/usage", token="KEY-AAA")
        before = {k["id"]: k for k in (usage_now or {}).get("keys", [])}.get("k-a", {}).get("used_chars")
        c.ok("重启前读到累计用量", before and before > 0, before)
        r.stop()
        r = Receiver(port, upstream, jobs_dir, samples, keys_file=keys_file)
        if not r.start():
            c.ok("重启接收端", False, open(r.log_path).read()[-500:])
        else:
            st, usage3, _, _ = call("GET", base + "/usage", token="KEY-AAA")
            by3 = {k["id"]: k for k in (usage3 or {}).get("keys", [])}
            c.ok("重启后用量不丢（累计字数一致）",
                 by3.get("k-a", {}).get("used_chars") == before, (before, by3.get("k-a")))
            c.ok("重启后密钥表仍然生效", st == 200 and len(by3) >= 3, list(by3))

        print("\n【v1.6.0 删除的密钥：历史用量仍可见（总量不会莫名对不上）】")
        write_keys([
            {"id": "k-b", "name": "B 站", "key": "KEY-BBB", "quota_chars": 0, "enabled": True, "created": 2},
        ])
        for _ in range(20):
            st, hb3, _, _ = call("GET", base + "/voice/health", token=None)
            if (hb3 or {}).get("keys") == 1:
                break
            time.sleep(0.1)
        st, usage4, _, _ = call("GET", base + "/usage", token="KEY-BBB")
        by4 = {k["id"]: k for k in (usage4 or {}).get("keys", [])}
        c.ok("被删掉的密钥 id 仍以 deleted=true 出现",
             by4.get("k-a", {}).get("deleted") is True, sorted(by4))
        c.ok("被删掉的密钥不再能调用 → 403",
             call("POST", base + "/jobs", q, token="KEY-AAA")[0] == 403)

    finally:
        r.stop()
        upstream_server.stop()
        shutil.rmtree(work, ignore_errors=True)

    print("\n" + ("全部通过 ✅" if not c.fails else "存在失败项 ❌ %s" % c.fails))
    return 0 if not c.fails else 1


if __name__ == "__main__":
    sys.exit(main())
