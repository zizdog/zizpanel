#!/usr/bin/env python3
"""
TtsVoice 音色样本接收端 + 带鉴权的 TTS 反向代理（跑在 TTS 主机上）

这个脚本干两件事：

  ① 接收音色样本
     Qwen 服务端读参考音频的代码是 os.path.exists(ref_audio)，
     只认「跑 TTS 那台机器上的本地文件路径」，不接受 URL / base64。
     所以站点上传的样本必须真的落到这台机器上。

  ② 给 Qwen 服务做带鉴权的反向代理
     ★ 为什么必须要有这一层：
     mlx-audio 这个 pip 包**完全没有鉴权功能** —— 它本来就是给本机开发用的。
     实测：不带任何密钥 POST /v1/audio/speech 返回 200，
           带一个乱写的 Bearer 密钥也返回 200（请求头被直接忽略）。
     也就是说，谁能连上 8880，谁就能白用你的 GPU。
     这里在它前面加一层校验，Qwen 只监听 127.0.0.1，不再对外暴露。

     插件侧零改动：TtsVoice 在填了 openaiKey 时本来就会发
         Authorization: Bearer <key>
     这个代理认这个头（也认 X-TtsVoice-Token），对上就转发。

【接口】
    GET  /voice/health              → 健康检查
    GET  /health                    → 同上
    GET  /voice/status              → 上游模型驻留状态（v1.2.0 新增，只读）
                                      回答"网站下一个请求要不要等冷加载"
    POST /voice                     Header: X-TtsVoice-Token / X-TtsVoice-Name
                                    Body: 音频二进制
    ANY  /v1/...                    Header: Authorization: Bearer <token>
                                          或 X-TtsVoice-Token: <token>
                                    → 原样转发到 upstream
                                      响应上会加一个 X-TtsVoice-Model-Cold 头
                                      （true = 本次触发了模型冷加载，约 25 秒）

【兼容性】v1.2.0 相对 v1.1.0 只做**增量**：原有的路径、参数名、响应字段、
状态码全部保持不变，所以还没升级的插件调用它不会有任何行为变化。

【v1.3.0 新增 /jobs/*】把「任务队列 + 逐块合成」搬到本机（契约见网站侧
usr/plugins/TtsVoice/SPEC-TO-MINI-job-queue.md），这样**浏览器/网站关掉任务也能跑完**：

    POST   /jobs              提交任务（分块由网站负责，这里只逐块合成）
    GET    /jobs/{id}         查进度（要轻要快，网站会频繁轮询）
    GET    /jobs/{id}/audio   下载拼好的整段 mp3（支持 Range）
    DELETE /jobs/{id}         取消 / 清理（幂等）
    GET    /jobs              队列总览（运维排查用）

同样是**纯增量**：/v1/*、/voice、/voice/health、/voice/status 一行未改。
实现要点：单 worker 串行（与上游 "Keep all GPU work serialized" 一致）、
状态落盘到 ~/tts/jobs/（重启不丢、从 done 继续）、退化检测与重试判据
与网站侧完全一致（见 DEGENERATE_* 常量）。

【运行】
    python3 receiver.py \
        --dir ~/tts/voice-samples \
        --token 你的密钥 \
        --host 0.0.0.0 --port 8899 \
        --upstream http://127.0.0.1:8880

  然后把 Qwen 服务改成只监听 127.0.0.1（--host 127.0.0.1），
  对外只暴露本代理的端口。
"""

import argparse
import hashlib
import hmac
import http.client
import json
import os
import re
import shutil
import threading
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

VERSION = "1.3.0"

# 只接受这几种扩展名，避免变成任意文件投放点
ALLOWED_EXT = {".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg", ".opus", ".webm"}

# 单次上传上限（参考音频裁完只有几百 KB，20MB 足够宽松）
MAX_BYTES = 20 * 1024 * 1024

# 代理转发的超时：合成一块约 30~60 秒，给足余量
PROXY_TIMEOUT = 900

# 转发时不该带过去的逐跳头
HOP_BY_HOP = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailers", "transfer-encoding", "upgrade",
}

# ---------------- /jobs 队列（v1.3.0）----------------
#
# 退化判据必须与**网站侧**逐字一致，否则两边的时间轴与重试行为会对不上。
# 网站源码：usr/plugins/TtsVoice/Providers.php
#   BYTES_PER_SECOND → bitrateKbps('128kbps') = 128*1000/8 = 16000
#   DEGENERATE_RATIO = 0.95
#   SYNTH_ATTEMPTS   = 3
#   cap_seconds      = max_tokens * 0.08        （12.5 token/秒 = 模型名里的 12Hz）
#   退化 ⟺ clean_bytes / 16000 >= cap_seconds * 0.95
#
# 注意用**浮点**比较，不要用整除：PHP 那边是浮点，整除会把边界值判反。
BYTES_PER_SECOND = 16000
DEGENERATE_RATIO = 0.95
SECONDS_PER_TOKEN = 0.08
SYNTH_ATTEMPTS = 3

# 单块合成的超时。worker 走 127.0.0.1，不受 nginx 60s 限制
JOB_CHUNK_TIMEOUT = 300
# 上游失败后的退避（第 1、2 次失败后各等一次）
JOB_RETRY_BACKOFF = (2, 5)

# 队列上限与任务规模上限（SPEC §7）
JOB_QUEUE_MAX = 500
JOB_CHUNKS_MAX = 2000
# ready 产物兜底保留 7 天（正常情况下网站下载后会 DELETE）
JOB_TTL_SECONDS = 7 * 24 * 3600

# 作业状态
JOB_QUEUED = "queued"
JOB_RUNNING = "running"
JOB_READY = "ready"
JOB_FAILED = "failed"
JOB_CANCELLED = "cancelled"

ARGS = None
# 作业管理器（main() 里创建；模块级别是为了让 Handler 能直接用到）
JOB_MANAGER = None


def log(msg):
    """带时间戳写到 stdout，交给 launchd 的 StandardOutPath 收走"""
    print("[%s] %s" % (datetime.now().strftime("%Y-%m-%d %H:%M:%S"), msg), flush=True)


def safe_name(raw):
    """
    把请求头里的文件名洗成一个安全的 basename。

    只保留字母数字和 . _ -，其余换成下划线；再强制规定扩展名。
    这样即使有人传 ../../../../etc/passwd，也只会在目标目录里生成一个普通文件。
    """
    raw = (raw or "").strip()

    if not raw:
        raw = "ref.wav"

    raw = raw.replace("\\", "/").split("/")[-1]

    stem, ext = os.path.splitext(raw)
    ext = ext.lower()

    if ext not in ALLOWED_EXT:
        ext = ".wav"

    stem = re.sub(r"[^A-Za-z0-9._-]", "_", stem).strip("._-")

    if not stem:
        stem = "ref"

    return (stem[:64] + ext)


def upstream_models(timeout=15):
    """
    问上游"现在**内存里驻留**了哪些模型"，返回集合；问不到返回 None。

    为什么不是"磁盘上装了哪些"：mlx-audio 的 /v1/models 走的是
    ModelProvider.get_available_models()，返回的是**已加载进内存的 dict 的键**
    （load_model 只增不删，没有 LRU、没有上限）。所以这个集合正好回答了
    "下一个请求会不会触发 25 秒的冷加载"这个唯一要紧的问题。

    返回 None 与返回空集合必须区分开：
      · 空集合 = 上游活着，但一个模型都没加载 → 确实要冷加载
      · None   = 上游连不上 / 响应看不懂 → 状态未知，别急着报警
    """
    parts = urlsplit(ARGS.upstream)
    conn = None

    try:
        conn = http.client.HTTPConnection(parts.hostname, parts.port or 80,
                                          timeout=timeout)
        conn.request("GET", "/v1/models", headers={"Accept": "application/json"})
        resp = conn.getresponse()
        raw = resp.read()
    except Exception:
        return None
    finally:
        if conn is not None:
            try:
                conn.close()
            except Exception:
                pass

    if resp.status != 200:
        return None

    try:
        doc = json.loads(raw.decode("utf-8", "replace"))
    except ValueError:
        return None

    if not isinstance(doc, dict) or not isinstance(doc.get("data"), list):
        return None

    out = set()
    for item in doc["data"]:
        if isinstance(item, dict) and item.get("id"):
            out.add(str(item["id"]))

    return out


# ===========================================================================
#  作业队列（/jobs/*）—— 把「排队 + 逐块合成」搬到本机
#
#  为什么要有这一层（网站侧给的理由，认同）：
#    之前是「浏览器开着才跑」：网站的 status 请求在 PHP 里现跑一块，
#    顶着 nginx 60s，页面一跳走任务就停在那里。放到本机之后，
#    网站只负责提交/查询/下载/取消，合成由常驻 worker 串行跑完。
#
#  刻意的设计：
#    1. **单 worker、串行**。上游源码里写着 `# Keep all GPU work serialized`，
#       而且 Qwen3-TTS 没实现批处理（实测 4 并发反而更慢）。多开 worker 只会更糟。
#    2. **状态落盘**，每次完成一块就原子更新一次 json。
#       重启后从 done 继续，不从头来 —— 一篇长文重跑一遍是几分钟的 GPU 时间。
#    3. **不在本机重新分块**。分块与 max_tokens 由网站算好发过来（那边已实测），
#       这里只按顺序逐块合成 —— 职责边界清楚，也避免两边算法漂移。
#    4. **判据与网站一致**（见文件顶部常量）。退化重试是模型的确定性失败模式，
#       换一次随机采样有约 2/3 概率能逃出来；绝不设 temperature。
# ===========================================================================


def strip_id3(data):
    """
    去掉 ID3v2（开头）与 ID3v1（末尾 128 字节），返回纯音频帧。

    为什么要去：契约里 chunk_bytes 是「去掉 ID3 之后」的长度，
    网站拿它按 128kbps 算时长与章节时间轴。拼接时也要去掉 ——
    否则每一块都夹一段元数据，播放器中间会读到垃圾。
    """
    if not data:
        return b""

    # ID3v2：第 0-2 字节是 "ID3"，第 6-9 是 synchsafe 长度（每字节只用低 7 位）
    if len(data) >= 10 and data[0:3] == b"ID3":
        size = ((data[6] & 0x7F) << 21) | ((data[7] & 0x7F) << 14) | \
               ((data[8] & 0x7F) << 7) | (data[9] & 0x7F)
        total = 10 + size
        if data[5] & 0x10:  # footer present
            total += 10
        data = data[total:] if 0 < total < len(data) else b""

    # ID3v1：末尾 128 字节以 "TAG" 开头
    if len(data) > 128 and data[-128:-125] == b"TAG":
        data = data[:-128]

    return data


def is_degenerate(clean_len, max_tokens):
    """
    与网站 Providers::synthesizeChunk() 同一个判据。

    退化时输出的是整段静音，时长会**正好**顶到 max_tokens 换算的上限，
    所以这个判据很干净：不是「超过经验值」，而是「撞到硬上限」。
    """
    if max_tokens <= 0:
        return False
    est_seconds = clean_len / float(BYTES_PER_SECOND)
    cap_seconds = max_tokens * SECONDS_PER_TOKEN
    return est_seconds >= cap_seconds * DEGENERATE_RATIO


def atomic_write(path, data):
    """先写 .part 再 os.replace，避免被读到半个文件。"""
    tmp = path + ".part"
    with open(tmp, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


def job_id_new():
    """
    生成 job_id。带时间戳便于人工排查，带随机段避免同一秒内碰撞。

    刻意不用 uuid4 单独当 id：运维看 `ls ~/tts/jobs` 时，按时间排序更直观。
    """
    return "j-%d-%s" % (int(time.time()), uuid.uuid4().hex[:6])


def valid_job_id(job_id):
    """job_id 会直接参与路径拼接，必须严格校验。"""
    return bool(re.match(r"^j-[0-9]{6,}-[0-9a-f]{4,16}$", job_id or ""))


class JobManager:
    """
    作业的落盘、排队与执行。

    线程模型：一个执行线程（_loop）串行处理队列；HTTP 线程只读写状态文件。
    状态文件是唯一的真相来源，所以 HTTP 侧无需与执行线程共享内存锁 ——
    除了取消标志（那一份留在内存里，因为它是「立刻生效」的意图，不必落盘）。
    """

    def __init__(self, jobs_dir, upstream):
        self.dir = jobs_dir
        self.upstream = upstream
        self.lock = threading.Lock()
        self.cancel_flags = set()      # 已请求取消的 job_id
        self.wake = threading.Event()  # 有新任务时叫醒 worker
        self.stopping = False
        os.makedirs(self.dir, exist_ok=True)

    # ---------- 路径 ----------

    def json_path(self, job_id):
        return os.path.join(self.dir, job_id + ".json")

    def chunk_dir(self, job_id):
        return os.path.join(self.dir, job_id)

    def final_path(self, job_id):
        return os.path.join(self.chunk_dir(job_id), "final.mp3")

    # ---------- 状态读写 ----------

    def load(self, job_id):
        try:
            with open(self.json_path(job_id), "r", encoding="utf-8") as fh:
                return json.load(fh)
        except (OSError, ValueError):
            return None

    def save(self, job):
        job["updated"] = int(time.time())
        atomic_write(self.json_path(job["job_id"]),
                     json.dumps(job, ensure_ascii=False).encode("utf-8"))

    def list_jobs(self):
        out = []
        try:
            names = sorted(os.listdir(self.dir))
        except OSError:
            return out
        for n in names:
            if not n.endswith(".json"):
                continue
            job = self.load(n[:-5])
            if job:
                out.append(job)
        return out

    # ---------- 提交 ----------

    def find_by_client_id(self, client_id):
        """幂等：同一个 client_id 的 queued/running 任务直接复用（防网络重试造重复）。"""
        if not client_id:
            return None
        for job in self.list_jobs():
            if job.get("client_id") == client_id and job.get("status") in (JOB_QUEUED, JOB_RUNNING):
                return job
        return None

    def active_count(self):
        return sum(1 for j in self.list_jobs()
                   if j.get("status") in (JOB_QUEUED, JOB_RUNNING))

    def submit(self, payload):
        """校验并落盘一个新任务；返回 job dict。校验失败抛 ValueError。"""
        client_id = str(payload.get("client_id") or "").strip()
        if client_id:
            existing = self.find_by_client_id(client_id)
            if existing:
                return existing

        model = str(payload.get("model") or "").strip()
        if not model:
            raise ValueError("model is required")

        voice = str(payload.get("voice") or "").strip()
        ref_audio = str(payload.get("ref_audio") or "").strip()
        # 必须恰好给一个：两个都给会「静默按其中一个生效」，最难排查
        if bool(voice) == bool(ref_audio):
            raise ValueError("exactly one of voice / ref_audio is required")
        if ref_audio and not os.path.exists(ref_audio):
            raise ValueError("ref_audio not found: %s" % ref_audio)

        chunks_in = payload.get("chunks")
        if not isinstance(chunks_in, list) or not chunks_in:
            raise ValueError("chunks is required")
        if len(chunks_in) > JOB_CHUNKS_MAX:
            raise ValueError("too many chunks (max %d)" % JOB_CHUNKS_MAX)

        chunks = []
        for i, c in enumerate(chunks_in):
            if not isinstance(c, dict):
                raise ValueError("chunks[%d] must be an object" % i)
            text = str(c.get("text") or "")
            if not text.strip():
                raise ValueError("chunks[%d].text is empty" % i)
            try:
                mt = int(c.get("max_tokens"))
            except (TypeError, ValueError):
                raise ValueError("chunks[%d].max_tokens must be a positive integer" % i)
            if mt <= 0:
                raise ValueError("chunks[%d].max_tokens must be a positive integer" % i)
            chunks.append({"text": text, "max_tokens": mt})

        if self.active_count() >= JOB_QUEUE_MAX:
            raise QueueFullError("queue full")

        job_id = job_id_new()
        now = int(time.time())
        job = {
            "job_id": job_id,
            "client_id": client_id,
            "status": JOB_QUEUED,
            "model": model,
            "voice": voice,
            "ref_audio": ref_audio,
            "ref_text": str(payload.get("ref_text") or ""),
            "response_format": str(payload.get("response_format") or "mp3") or "mp3",
            "total": len(chunks),
            "done": 0,
            "chunk_bytes": [],
            "attempts": [],
            "error": "",
            "cold": False,
            "created": now,
            "updated": now,
            "chunks": chunks,
        }
        os.makedirs(self.chunk_dir(job_id), exist_ok=True)
        self.save(job)
        self.wake.set()
        log("作业已入队 %s（%d 块，model=%s）" % (job_id, len(chunks), model))
        return job

    # ---------- 取消 ----------

    def cancel(self, job_id):
        """
        幂等：未知 job 也返回成功（网站删除时不至于报错）。

        running 的取消是**置标志**，worker 在当前块跑完后停下 ——
        不能在 HTTP 线程里强杀，那会留下半个 mp3 与不一致的状态。
        """
        job = self.load(job_id)
        if not job:
            return
        with self.lock:
            self.cancel_flags.add(job_id)
        if job.get("status") == JOB_QUEUED:
            job["status"] = JOB_CANCELLED
            self.save(job)
            log("作业已取消 %s（尚未开始）" % job_id)
        self.wake.set()

    def is_cancelled(self, job_id):
        with self.lock:
            return job_id in self.cancel_flags

    def _clear_cancel(self, job_id):
        with self.lock:
            self.cancel_flags.discard(job_id)

    # ---------- 回收 ----------

    def reap(self, job_id):
        """删掉产物目录与状态文件（网站下载后调用）。"""
        shutil.rmtree(self.chunk_dir(job_id), ignore_errors=True)
        try:
            os.remove(self.json_path(job_id))
        except OSError:
            pass
        self._clear_cancel(job_id)

    def cleanup_ttl(self):
        """兜底 TTL：ready/failed/cancelled 超过 7 天就清掉，避免磁盘无限涨。"""
        deadline = time.time() - JOB_TTL_SECONDS
        for job in self.list_jobs():
            if job.get("status") in (JOB_READY, JOB_FAILED, JOB_CANCELLED) and \
                    job.get("updated", 0) < deadline:
                log("TTL 清理旧作业 %s（status=%s）" % (job.get("job_id"), job.get("status")))
                self.reap(job["job_id"])

    # ---------- 恢复 ----------

    def recover(self):
        """
        启动时恢复：running → queued（保留已完成的块，从 done 继续）。

        这一步是「重启不丢」的关键。不恢复的话，重启后所有 running 任务
        会永远停在 running，网站那边表现为「进度不动」。
        """
        resumed = 0
        for job in self.list_jobs():
            if job.get("status") == JOB_RUNNING:
                job["status"] = JOB_QUEUED
                self.save(job)
                resumed += 1
            elif job.get("status") == JOB_QUEUED:
                resumed += 1
        if resumed:
            log("恢复 %d 个未完成作业（从已完成的块继续）" % resumed)
            self.wake.set()
        return resumed

    # ---------- 执行 ----------

    def start(self):
        t = threading.Thread(target=self._loop, name="job-worker", daemon=True)
        t.start()
        return t

    def _loop(self):
        self.recover()
        last_ttl = 0.0
        while not self.stopping:
            job = None
            with self.lock:
                for cand in self.list_jobs():
                    if cand.get("status") == JOB_QUEUED:
                        job = cand
                        break
            if not job:
                # 没活干：等唤醒或 30s 超时（超时用于跑 TTL 与兜底轮询）
                self.wake.wait(30)
                self.wake.clear()
                if time.time() - last_ttl > 3600:
                    last_ttl = time.time()
                    try:
                        self.cleanup_ttl()
                    except Exception as exc:      # TTL 失败不能拖垮 worker
                        log("TTL 清理失败：%s" % exc)
                continue
            try:
                self._run(job["job_id"])
            except Exception as exc:
                # worker 绝不能被单个任务搞死，否则整条队列停摆
                log("作业 %s 异常：%s" % (job.get("job_id"), exc))
                cur = self.load(job["job_id"]) or job
                cur["status"] = JOB_FAILED
                cur["error"] = "worker error: %s" % exc
                self.save(cur)

    def _run(self, job_id):
        job = self.load(job_id)
        if not job or job.get("status") != JOB_QUEUED:
            return
        if self.is_cancelled(job_id):
            job["status"] = JOB_CANCELLED
            self.save(job)
            self._clear_cancel(job_id)
            return

        job["status"] = JOB_RUNNING
        self.save(job)
        log("开始作业 %s（%d 块，从第 %d 块继续）"
            % (job_id, job["total"], job.get("done", 0)))

        chunks = job.get("chunks") or []
        done = int(job.get("done", 0))
        chunk_bytes = list(job.get("chunk_bytes") or [])
        attempts_all = list(job.get("attempts") or [])

        # 冷加载标记：第一块开始前探一次上游驻留状态（与 /voice/status 同一判据）
        if done == 0 and not job.get("cold"):
            try:
                loaded = upstream_models()
                if loaded is not None and job["model"] not in loaded:
                    job["cold"] = True
            except Exception:
                pass

        for idx in range(done, len(chunks)):
            if self.is_cancelled(job_id):
                job["status"] = JOB_CANCELLED
                self.save(job)
                self._clear_cancel(job_id)
                log("作业 %s 已取消（停在第 %d 块）" % (job_id, idx + 1))
                return

            chunk = chunks[idx]
            try:
                audio, used, attempts = self._synth_chunk(job, chunk)
            except ChunkFailed as exc:
                # 取消不是失败：用户主动停的，不该在列表里留一条刺眼的 failed。
                # （取消是在 _synth_chunk 里通过 is_cancelled 抛出来的，
                #   这里必须再判一次，否则会被当成合成失败。）
                if self.is_cancelled(job_id):
                    job["status"] = JOB_CANCELLED
                    self.save(job)
                    self._clear_cancel(job_id)
                    log("作业 %s 已取消（停在第 %d 块）" % (job_id, idx + 1))
                    return
                job["status"] = JOB_FAILED
                job["error"] = "第 %d/%d 块合成失败：%s" % (idx + 1, len(chunks), exc)
                self.save(job)
                log("作业 %s 失败：%s" % (job_id, job["error"]))
                return

            atomic_write(os.path.join(self.chunk_dir(job_id), "%d.mp3" % idx), audio)
            chunk_bytes.append(len(audio))
            attempts_all.append(attempts)
            done = idx + 1
            job["done"] = done
            job["chunk_bytes"] = chunk_bytes
            job["attempts"] = attempts_all
            self.save(job)
            log("作业 %s 进度 %d/%d（%d 字节，尝试 %d 次）"
                % (job_id, done, len(chunks), len(audio), attempts))

        # 全部完成：拼接成 final.mp3
        parts = []
        for i in range(len(chunks)):
            p = os.path.join(self.chunk_dir(job_id), "%d.mp3" % i)
            try:
                with open(p, "rb") as fh:
                    parts.append(fh.read())
            except OSError as exc:
                job["status"] = JOB_FAILED
                job["error"] = "拼接时读不到第 %d 块：%s" % (i + 1, exc)
                self.save(job)
                return
        atomic_write(self.final_path(job_id), b"".join(parts))
        job["status"] = JOB_READY
        job["done"] = len(chunks)
        self.save(job)
        log("作业 %s 完成（%d 块，共 %d 字节）"
            % (job_id, len(chunks), os.path.getsize(self.final_path(job_id))))

    def _synth_chunk(self, job, chunk):
        """
        合成一块，含退化检测与重试。返回 (clean_mp3, used_bytes, attempts)。

        重试策略与网站一致：退化就换一次随机采样重来（不设 temperature），
        最多 SYNTH_ATTEMPTS 次；上游 5xx/连接错误按退避重试。
        """
        text = chunk["text"]
        max_tokens = int(chunk["max_tokens"])
        last_err = ""

        for attempt in range(1, SYNTH_ATTEMPTS + 1):
            if self.is_cancelled(job["job_id"]):
                raise ChunkFailed("cancelled")
            body = {
                "model": job["model"],
                "input": text,
                "max_tokens": max_tokens,
                "response_format": "mp3",
            }
            # voice 与 ref_audio 恰好一个（提交时已校验）
            if job.get("voice"):
                body["voice"] = job["voice"]
            if job.get("ref_audio"):
                body["ref_audio"] = job["ref_audio"]
                if job.get("ref_text"):
                    body["ref_text"] = job["ref_text"]
            # 刻意不传 temperature：设成 0 会 100% 退化，交给服务端默认随机采样

            try:
                raw = self._post_speech(body)
            except Exception as exc:
                last_err = "上游请求失败：%s" % exc
                if attempt < SYNTH_ATTEMPTS:
                    delay = JOB_RETRY_BACKOFF[min(attempt - 1, len(JOB_RETRY_BACKOFF) - 1)]
                    log("作业 %s 第 %d 次上游失败，%ds 后重试" % (job["job_id"], attempt, delay))
                    time.sleep(delay)
                    continue
                raise ChunkFailed(last_err)

            clean = strip_id3(raw)
            if not clean:
                last_err = "上游返回空音频"
                continue

            if is_degenerate(len(clean), max_tokens):
                last_err = "输出退化（%.1f 秒，顶到 %.1f 秒上限）" % (
                    len(clean) / float(BYTES_PER_SECOND), max_tokens * SECONDS_PER_TOKEN)
                log("作业 %s 第 %d 块退化（尝试 %d/%d）"
                    % (job["job_id"], job["done"] + 1, attempt, SYNTH_ATTEMPTS))
                continue

            return clean, len(clean), attempt

        raise ChunkFailed(last_err or "未知错误")

    def _post_speech(self, body):
        """调上游 /v1/audio/speech，返回原始响应体（未去 ID3）。"""
        parts = urlsplit(self.upstream)
        payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
        req = urllib.request.Request(
            "%s/v1/audio/speech" % self.upstream.rstrip("/"),
            data=payload, method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=JOB_CHUNK_TIMEOUT) as resp:
                if resp.status >= 400:
                    raise RuntimeError("HTTP %d" % resp.status)
                return resp.read()
        except urllib.error.HTTPError as exc:
            detail = ""
            try:
                detail = exc.read()[:200].decode("utf-8", "replace")
            except Exception:
                pass
            raise RuntimeError("HTTP %d %s" % (exc.code, detail))
        except urllib.error.URLError as exc:
            raise RuntimeError(str(exc.reason))


class QueueFullError(Exception):
    """队列已满（映射到 429）。"""


class ChunkFailed(Exception):
    """某一块重试耗尽（整个 job 判 failed）。"""


def header_safe(value, limit=200):
    """
    把可能来自请求体的字符串洗成能安全放进 HTTP 头的值。

    模型名是客户端给的，直接塞进响应头等于让调用方能注入换行 ——
    那不是注入到我们进程里，但会让下游解析出乱七八糟的头。
    """
    value = str(value or "")
    value = "".join(c for c in value if 32 <= ord(c) < 127)

    return value[:limit]


class Handler(BaseHTTPRequestHandler):
    server_version = "TtsVoiceReceiver/" + VERSION
    protocol_version = "HTTP/1.1"

    # ---------- 基础设施 ----------

    def _json(self, code, payload):
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")

        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _model_state_headers(self, model, models):
        """
        给响应加两个**增量**头，告诉调用方"这次请求的模型是冷的还是热的"。

        models 为 None（上游连不上）时不加头 —— 状态未知就不表态，
        免得下游把它当成"模型没装"。

        注意这里只加头、不改响应体：v1.1.0 的调用方按契约解析响应体，
        多出来的头对它们完全透明。
        """
        if not model or models is None:
            return

        if model in models:
            self.send_header("X-TtsVoice-Model-Cold", "false")
            return

        self.send_header("X-TtsVoice-Model-Cold", "true")
        self.send_header("X-TtsVoice-Model", header_safe(model))
        self.send_header("X-TtsVoice-Model-Loaded",
                         header_safe(",".join(sorted(models))))

    def _fail(self, code, error):
        self._json(code, {"ok": False, "error": error})

    def _presented_token(self):
        """
        取出请求携带的密钥。

        两种都认，这样插件不用改：
          - Authorization: Bearer <token>   （插件填了 openaiKey 时发的就是这个）
          - X-TtsVoice-Token: <token>       （插件上传音色样本时发的）
        """
        auth = self.headers.get("Authorization", "")

        if auth.lower().startswith("bearer "):
            return auth[7:].strip()

        got = self.headers.get("X-TtsVoice-Token", "")

        if got:
            return got.strip()

        return ""

    def _token_ok(self):
        """常数时间比较，避免时序侧信道"""
        expected = (ARGS.token or "")

        if not expected:
            # 没配密钥 = 不校验。启动时会打醒目警告。
            return True

        return hmac.compare_digest(self._presented_token(), expected)

    def log_message(self, fmt, *args):
        """默认实现会往 stderr 打每个请求，这里压掉，只留我们自己的 log()"""
        return

    def _read_body(self, limit=None):
        """按 Content-Length 读请求体（不处理 chunked，够用）"""
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            return None, "Content-Length 无效"

        if length < 0:
            return None, "Content-Length 无效"

        if limit is not None and length > limit:
            return None, "请求体过大（上限 %d 字节）" % limit

        if length == 0:
            return b"", ""

        data = self.rfile.read(length)

        if len(data) != length:
            return None, "请求体读取不完整"

        return data, ""

    # ---------- 路由 ----------

    def do_GET(self):
        path = self.path.split("?", 1)[0].rstrip("/")

        if path in ("/voice/health", "/health", ""):
            directory = ARGS.dir

            return self._json(200, {
                "ok": True,
                "version": VERSION,
                "dir": os.path.abspath(directory),
                "writable": bool(os.path.isdir(directory) and os.access(directory, os.W_OK)),
                "auth": bool(ARGS.token),
                "upstream": ARGS.upstream,
            })

        # ---------- /jobs/*（v1.3.0）----------
        # 统一用 X-TtsVoice-Token 鉴权（与 /voice 同一把 token）
        if path == "/jobs":
            if not self._token_ok():
                return self._fail_jobs(403, "unauthorized")
            status_filter = None
            if "?" in self.path:
                q = self.path.split("?", 1)[1]
                for kv in q.split("&"):
                    if kv.startswith("status="):
                        status_filter = set(
                            s.strip() for s in kv[len("status="):].split(",") if s.strip())
            jobs = []
            for job in JOB_MANAGER.list_jobs():
                if status_filter and job.get("status") not in status_filter:
                    continue
                jobs.append({
                    "job_id": job.get("job_id"), "status": job.get("status"),
                    "done": job.get("done"), "total": job.get("total"),
                    "model": job.get("model"), "created": job.get("created"),
                })
            jobs.sort(key=lambda j: j.get("created") or 0)
            return self._json(200, {"ok": True, "jobs": jobs})

        if path.startswith("/jobs/"):
            return self._jobs_get(path)

        # v1.2.0 新增：只读地告诉调用方"上游现在驻留了哪些模型"。
        # 给网站插件一个动手前的判断依据 —— 冷模型要等约 25 秒，
        # 知道了就能提前提示用户或把超时放宽，而不是让它超时失败。
        if path in ("/voice/status", "/status"):
            if not self._token_ok():
                return self._fail(403, "密钥不匹配")

            models = upstream_models()

            if models is None:
                return self._json(503, {
                    "ok": False,
                    "version": VERSION,
                    "upstream": ARGS.upstream,
                    "reachable": False,
                    "error": "连不上上游 TTS 服务，无法判断模型驻留状态",
                })

            return self._json(200, {
                "ok": True,
                "version": VERSION,
                "upstream": ARGS.upstream,
                "reachable": True,
                # 注意语义：已**驻留内存**的模型，不是磁盘上装了的模型。
                # 冷加载实测约 25 秒；这里为空/不含某个模型就说明下一个用到它
                # 的请求要付这个代价。
                "models": sorted(models),
                "models_loaded": len(models),
                "cold_load_seconds": 25,
                "auth": bool(ARGS.token),
            })

        # 其余 GET 走代理（/v1/models 等）
        if path.startswith("/v1"):
            return self._proxy()

        return self._fail(404, "接口不存在：%s" % self.path)

    def do_POST(self):
        path = self.path.split("?", 1)[0].rstrip("/")

        if path == "/jobs":
            return self._jobs_create()

        if path in ("/voice", "/voice/upload"):
            return self._receive_voice()

        if path.startswith("/v1"):
            return self._proxy()

        return self._fail(404, "接口不存在：%s" % self.path)

    def do_DELETE(self):
        path = self.path.split("?", 1)[0].rstrip("/")

        if path.startswith("/jobs/"):
            return self._jobs_delete(path)

        return self._fail(404, "接口不存在：%s" % self.path)

    # ---------- ② 作业队列（/jobs/*，v1.3.0）----------
    #
    # 与 /voice 用同一把 token，但**响应体格式按 SPEC**：
    # 失败一律 {"ok":false,"error":"..."}（英文短语，网站按它判断），
    # 与 /voice 那边的人话中文错误分开 —— 那边是给面板用户看的，
    # 这边是给程序解析的。

    def _fail_jobs(self, code, error):
        return self._json(code, {"ok": False, "error": error})

    def _jobs_view(self, job):
        """把内部 job 记录裁剪成 SPEC §4.2 的形状（不外泄 chunks 正文）。"""
        return {
            "ok": True,
            "job_id": job.get("job_id"),
            "status": job.get("status"),
            "done": job.get("done", 0),
            "total": job.get("total", 0),
            "chunk_bytes": job.get("chunk_bytes") or [],
            "attempts": job.get("attempts") or [],
            "error": job.get("error") or "",
            "model": job.get("model", ""),
            "created": job.get("created", 0),
            "updated": job.get("updated", 0),
            "cold": bool(job.get("cold")),
        }

    def _jobs_get(self, path):
        """GET /jobs/{id} 与 GET /jobs/{id}/audio"""
        if not self._token_ok():
            return self._fail_jobs(403, "unauthorized")

        rest = path[len("/jobs/"):]
        want_audio = rest.endswith("/audio")
        job_id = rest[:-len("/audio")] if want_audio else rest

        if not valid_job_id(job_id):
            return self._fail_jobs(404, "job not found")

        job = JOB_MANAGER.load(job_id)
        if not job:
            return self._fail_jobs(404, "job not found")

        if not want_audio:
            return self._json(200, self._jobs_view(job))

        if job.get("status") != JOB_READY:
            return self._fail_jobs(409, "not ready")

        final = JOB_MANAGER.final_path(job_id)
        try:
            size = os.path.getsize(final)
        except OSError:
            return self._fail_jobs(409, "not ready")

        # Range 支持：网站不需要，但浏览器试听拖动进度条会舒服很多。
        # 只实现单段 range（多段 range 极少见，返回整段也符合规范允许的退化行为）。
        start, end = 0, size - 1
        code = 200
        rng = self.headers.get("Range") or ""
        if rng.startswith("bytes="):
            spec = rng[len("bytes="):].split(",")[0].strip()
            if "-" in spec:
                a, _, b = spec.partition("-")
                try:
                    if a == "" and b != "":
                        start = max(0, size - int(b))     # bytes=-N 末尾 N 字节
                    else:
                        start = int(a)
                        if b != "":
                            end = min(size - 1, int(b))
                except ValueError:
                    start, end = 0, size - 1
                else:
                    if start >= size or start > end:
                        self.send_response(416)
                        self.send_header("Content-Range", "bytes */%d" % size)
                        self.send_header("Content-Length", "0")
                        self.end_headers()
                        return
                    code = 206

        length = end - start + 1
        self.send_response(code)
        self.send_header("Content-Type", "audio/mpeg")
        self.send_header("Content-Length", str(length))
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Cache-Control", "no-store")
        if code == 206:
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        self.end_headers()

        with open(final, "rb") as fh:
            fh.seek(start)
            remaining = length
            while remaining > 0:
                block = fh.read(min(64 * 1024, remaining))
                if not block:
                    break
                self.wfile.write(block)
                remaining -= len(block)

    def _jobs_create(self):
        if not self._token_ok():
            log("拒绝提交作业：密钥不匹配（来自 %s）" % self.client_address[0])
            return self._fail_jobs(403, "unauthorized")

        data, err = self._read_body(16 * 1024 * 1024)
        if data is None:
            return self._fail_jobs(413 if "过大" in err else 400, err)
        if not data:
            return self._fail_jobs(400, "empty body")

        try:
            payload = json.loads(data.decode("utf-8", "replace"))
        except ValueError as exc:
            return self._fail_jobs(400, "invalid json: %s" % exc)
        if not isinstance(payload, dict):
            return self._fail_jobs(400, "body must be a JSON object")

        try:
            job = JOB_MANAGER.submit(payload)
        except QueueFullError:
            return self._fail_jobs(429, "queue full")
        except ValueError as exc:
            return self._fail_jobs(400, str(exc))
        except Exception as exc:
            log("提交作业失败：%s" % exc)
            return self._fail_jobs(500, "internal error: %s" % exc)

        return self._json(200, {
            "ok": True,
            "job_id": job.get("job_id"),
            "status": job.get("status"),
            "total": job.get("total"),
        })

    def _jobs_delete(self, path):
        """取消/清理。幂等：未知 job 也返回 200（网站删除时不至于报错）。"""
        if not self._token_ok():
            return self._fail_jobs(403, "unauthorized")

        job_id = path[len("/jobs/"):]
        if not valid_job_id(job_id):
            return self._json(200, {"ok": True})

        job = JOB_MANAGER.load(job_id)
        if not job:
            return self._json(200, {"ok": True})

        status = job.get("status")
        if status in (JOB_QUEUED, JOB_RUNNING):
            JOB_MANAGER.cancel(job_id)
        else:
            # ready / failed / cancelled：直接清产物
            JOB_MANAGER.reap(job_id)
        return self._json(200, {"ok": True})

    # ---------- ① 接收音色样本 ----------

    def _receive_voice(self):
        if not self._token_ok():
            log("拒绝上传：密钥不匹配（来自 %s）" % self.client_address[0])
            return self._fail(403, "密钥不匹配")

        data, err = self._read_body(MAX_BYTES)

        if data is None:
            return self._fail(413 if "过大" in err else 400, err)

        if not data:
            return self._fail(400, "请求体为空")

        name = safe_name(self.headers.get("X-TtsVoice-Name"))
        target = os.path.join(ARGS.dir, name)

        try:
            os.makedirs(ARGS.dir, exist_ok=True)
        except OSError as exc:
            log("创建目录失败：%s" % exc)
            return self._fail(500, "创建目录失败：%s" % exc)

        # 先写临时文件再原子替换：避免被读到半个文件
        tmp = target + ".part"

        try:
            with open(tmp, "wb") as fh:
                fh.write(data)
                fh.flush()
                os.fsync(fh.fileno())

            os.replace(tmp, target)
        except OSError as exc:
            try:
                if os.path.exists(tmp):
                    os.remove(tmp)
            except OSError:
                pass

            log("写入失败：%s" % exc)
            return self._fail(500, "写入失败：%s" % exc)

        digest = hashlib.sha256(data).hexdigest()

        log("已接收 %s（%d 字节，sha256 %s…）" % (name, len(data), digest[:12]))

        return self._json(200, {
            "ok": True,
            "path": os.path.abspath(target),
            "name": name,
            "size": len(data),
            "sha256": digest,
        })

    # ---------- ② 转发到 Qwen 服务 ----------

    def _proxy(self):
        if not self._token_ok():
            log("拒绝代理：密钥不匹配（来自 %s，%s）"
                % (self.client_address[0], self.path.split("?")[0]))
            return self._fail(403, "密钥不匹配（该服务已启用鉴权，请配置密钥）")

        parts = urlsplit(ARGS.upstream)
        body, err = self._read_body()

        if body is None:
            return self._fail(400, err)

        # 合成请求会把模型名放在请求体里。转发出**之前**先问一次上游的驻留
        # 状态，这样才能判断本次请求会不会触发冷加载 —— 请求转发完再问就晚了
        # （那时模型必然已在内存里，永远报 false）。
        requested_model = ""
        model_state = None

        if self.path.split("?", 1)[0].rstrip("/") == "/v1/audio/speech" and body:
            try:
                requested_model = str(json.loads(body.decode("utf-8", "replace")).get("model") or "")
            except ValueError:
                requested_model = ""

            if requested_model:
                model_state = upstream_models()

        # 组转发头：去掉逐跳头，并删掉 Host / Content-Length 让 http.client 自己算
        headers = {}

        for key, value in self.headers.items():
            lower = key.lower()

            if lower in HOP_BY_HOP or lower in ("host", "content-length"):
                continue

            headers[key] = value

        headers["Content-Length"] = str(len(body))

        conn = None

        try:
            conn = http.client.HTTPConnection(parts.hostname, parts.port or 80,
                                              timeout=PROXY_TIMEOUT)
            conn.request(self.command, self.path, body=body, headers=headers)
            resp = conn.getresponse()

            payload = resp.read()

            self.send_response(resp.status)

            for key, value in resp.getheaders():
                if key.lower() in HOP_BY_HOP or key.lower() == "content-length":
                    continue

                self.send_header(key, value)

            self._model_state_headers(requested_model, model_state)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

            cold = ""
            if requested_model and model_state is not None:
                cold = "（冷加载）" if requested_model not in model_state else "（已驻留）"

            log("%s %s → %d（%d 字节）%s"
                % (self.command, self.path.split("?")[0], resp.status, len(payload), cold))
        except Exception as exc:
            log("转发失败：%s %s → %s" % (self.command, self.path, exc))
            return self._fail(502, "无法连接上游 TTS 服务：%s" % exc)
        finally:
            if conn is not None:
                try:
                    conn.close()
                except Exception:
                    pass


def main():
    global ARGS, JOB_MANAGER

    parser = argparse.ArgumentParser(description="TtsVoice 接收端 + 带鉴权的 TTS 代理")
    parser.add_argument("--dir", default=os.path.expanduser("~/tts/voice-samples"),
                        help="音色样本存放目录（默认 ~/tts/voice-samples）")
    parser.add_argument("--host", default="0.0.0.0",
                        help="监听地址（默认 0.0.0.0）")
    parser.add_argument("--port", type=int, default=8899, help="监听端口（默认 8899）")
    parser.add_argument("--token", default="", help="共享密钥，强烈建议设置")
    parser.add_argument("--upstream", default="http://127.0.0.1:8880",
                        help="上游 TTS 服务地址（默认 http://127.0.0.1:8880）")
    parser.add_argument("--jobs-dir", default=os.path.expanduser("~/tts/jobs"),
                        help="作业队列的落盘目录（默认 ~/tts/jobs）")
    ARGS = parser.parse_args()

    os.makedirs(ARGS.dir, exist_ok=True)
    os.makedirs(ARGS.jobs_dir, exist_ok=True)

    # 作业 worker 在进程内起一条线程（契约要求同一进程/端口）：
    # 单 worker、串行 —— 与上游 "Keep all GPU work serialized" 一致。
    JOB_MANAGER = JobManager(ARGS.jobs_dir, ARGS.upstream)
    JOB_MANAGER.start()

    if not ARGS.token:
        log("=" * 66)
        log("⚠️  没有设置 --token")
        log("    本代理将不做任何鉴权，而上游 Qwen 服务通常也没有鉴权 ——")
        log("    等于把语音合成能力完全敞开给能连到这个端口的人。")
        log("    请加上：--token <随机密钥>")
        log("=" * 66)

    server = ThreadingHTTPServer((ARGS.host, ARGS.port), Handler)

    log("TtsVoice 接收端 + 代理 v%s 已启动" % VERSION)
    log("  监听    : http://%s:%d" % (ARGS.host, ARGS.port))
    log("  样本目录: %s" % os.path.abspath(ARGS.dir))
    log("  上游    : %s" % ARGS.upstream)
    log("  作业目录: %s" % os.path.abspath(ARGS.jobs_dir))
    log("  鉴权    : %s" % ("已启用" if ARGS.token else "★ 未启用"))
    log("")
    log("  插件里这样填：")
    log("    openaiBaseUrl = http://<本机地址>:%d/v1" % ARGS.port)
    log("    openaiKey     = <上面的密钥>")
    log("")
    log("  接收端地址与密钥由插件自动从上面两项推导，不用另外填。")
    log("  模型驻留状态（要不要等冷加载）：GET /voice/status")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log("收到中断，退出")
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
