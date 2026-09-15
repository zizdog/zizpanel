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
    GET  /voice/health              → 健康检查（无需密钥）
    GET  /health                    → 同上
    GET  /voice/status              → 上游模型驻留状态（v1.2.0 新增，只读）
                                      回答"网站下一个请求要不要等冷加载"
    GET  /usage                     → 各密钥的额度/已用(含在跑作业占用)/今日/近 7 天 + 总量
    POST /usage/reset               → 清零用量（需 X-TtsVoice-Admin，v1.7.0）
    POST /voice                     Header: X-TtsVoice-Token / X-TtsVoice-Name
                                           / X-TtsVoice-Source（v1.5.0 新增）
                                    Body: 音频二进制
                                    落盘：<dir>/<source>/ref.wav（校验 + 归一化之后）
    GET  /voice/sources             Header: X-TtsVoice-Token
                                    → 列出各来源（v1.5.0 新增，面板"各来源"用）
    DELETE /voice/sources/{source}  Header: X-TtsVoice-Token
                                    → 删除某个来源（幂等）
    ANY  /v1/...                    Header: Authorization: Bearer <token>
                                          或 X-TtsVoice-Token: <token>
                                    → 原样转发到 upstream
                                      响应上会加一个 X-TtsVoice-Model-Cold 头
                                      （true = 本次触发了模型冷加载，约 25 秒）

【兼容性】v1.2.0 相对 v1.1.0 只做**增量**：原有的路径、参数名、响应字段、
状态码全部保持不变，所以还没升级的插件调用它不会有任何行为变化。

【v1.4.0 增量】① 支持 response_format（wav/mp3，wav 拼整段要去头补头）；
② sampling 四个参数（temperature/top_p/top_k/repetition_penalty）随块透传给上游，
缺省 0.7/0.9/40/1.05 —— 不传上游 repetition_penalty 是 1.0，长句会整段变噪音；
③ 退化判定的字节率按格式走（wav 48000 且先减 44 字节头）。
**不做变速**：变速由网站用 ffmpeg 在下载后做，这里再变会双重变速。
见 usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md。

【v1.5.0 音色来源隔离】修掉一个真实故障：以前所有站点都写同一个
<dir>/ref.wav，后传的覆盖先传的；更糟的是**非 wav 字节被命名成 .wav**
交给上游，mlx-audio 按扩展名选解码器 → 解码失败 → 上游返回 200 + 0 字节
→ 网站只看到 IncompleteRead(0 bytes read)。现在：

    POST /voice   多一个 X-TtsVoice-Source 头（缺省 default），样本落到
                  <dir>/<source>/ref.wav；先按**内容**校验（ffprobe 解码 /
                  魔数），不能解码的直接 400 并说明原因；再归一化成
                  ≤12s / 24kHz / 单声道 / PCM s16le WAV，原子替换。
                  <1s → 400；1–3s → 200 但带 warning（太短音色不像本人）。
                  **不做旧版兼容**：不再给根目录 ref.wav 写副本。v1.4.0 留在
                  根目录的那份若还在，只在 /voice/sources 里作为"旧版残留"
                  列出来（不参与解析），可以在面板里删掉。
    POST /jobs    payload 多一个可选 source（缺省 default），参考音频顺序：
                  显式 ref_audio → <dir>/<source>/ref.wav
                  （**不**读根目录那份 v1.4.0 老文件：插件还在开发期，
                   不为旧版本保留隐式回退，免得"用的是谁的音色"说不清）
                  job 记录 source，GET /jobs/{id} 原样返回，日志也打出来。

  没有 ffmpeg 时不假装成功：只接受已是 24kHz/16bit/单声道的 wav，其余
  明确报错让用户去装 ffmpeg（宁可用不了，也不要再产出一个解码不了的文件）。

【v1.6.0 多密钥 + 额度 + 用量】密钥从 plist 的单个 --token 挪到 keys.json：

    · **多密钥**：每个网站（或每个调用方）一把密钥，面板里"添加密钥"即可，
      按 mtime **热加载**，加/停/删都不需要重启正在跑合成的服务。
    · **每密钥额度（单位：字）**：`quota_chars`，0 = 不限。按**实际合成完成的
      块**计费（`len(text)`，中文 1 字算 1、含标点）。超额返回 **429** 且 error 以
      `quota exceeded:` 开头（与 403 密钥错误区分开，否则调用方会把"额度用完"
      当成"密钥错"去重试）。
      **计费口径（v1.7.0 改过）**：提交时只**占额度**（防止同一把密钥并发提交
      把总额度撑爆），作业到终态（完成/失败/取消）时按**已经合成完的块**结算，
      没跑到的部分自动退回 —— 生成失败或中途取消不该按整篇收费。
      直连 `/v1/audio/speech` 只在响应 2xx 时计费。
    · **用量统计**：usage.json 按密钥累计 chars / requests / jobs / audio_bytes，
      另存最近 60 天的按天明细；`GET /usage` 返回每个密钥的额度、已用、剩余、
      今日、近 7 天，以及全局合计与 30 天曲线。
    · 调用被拒（密钥错/停用）**不计**用量；上游失败只计一次请求、不计字数与字节。
    · 幂等重放（同一 client_id）不重复扣额度。
    · `--token` 仍可用（兼容已部署的 plist）；面板重新部署接收端时会把它
      迁移成 keys.json 里的一条 default 记录，之后密钥完全由文件管理。
    · **手动清零**：`POST /usage/reset`（body：`{"key_id":"…"}` 或 `{"all":true}`）
      需要 `X-TtsVoice-Admin: <--admin-token>`。普通调用密钥**不能**清零自己的额度，
      否则额度就只是建议；管理密钥由面板在部署时生成并写进 plist，网站拿不到。

【v1.6.1 访问日志】每次请求一行（健康检查与 /jobs 轮询除外），含状态码、
方法、路径、用到的密钥 id 与来源 IP，比如：

    POST /voice → 403 key=- ip=192.168.1.1 ｜ 密钥不匹配

  加它的直接原因：插件报过一次"接收端拒绝：HTTP 200"，而接收端这边一条
  日志都没有 —— 只能靠"没有日志"反推"请求根本没到这台机器"（真因是插件把
  服务器地址填错了，请求打到了别处）。有了访问日志，"到没到、被谁拒的"
  一眼就能看出来。

【v1.3.0 新增 /jobs/*】把「任务队列 + 逐块合成」搬到本机（契约见网站侧
usr/plugins/TtsVoice/SPEC-TO-MINI-job-queue.md），这样**浏览器/网站关掉任务也能跑完**：

    POST   /jobs              提交任务（分块由网站负责，这里只逐块合成）
    GET    /jobs/{id}         查进度（要轻要快，网站会频繁轮询）
    GET    /jobs/{id}/audio   下载拼好的整段（wav/mp3，支持 Range）
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
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid
import wave
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit

VERSION = "1.7.0"

# 只接受这几种扩展名，避免变成任意文件投放点
ALLOWED_EXT = {".wav", ".mp3", ".m4a", ".aac", ".flac", ".ogg", ".opus", ".webm"}

# ---------------- 音色样本规范化（v1.5.0）----------------
#
# 上游 mlx-audio 按**扩展名**选解码器：给它一个叫 .wav 的 mp3，它会用
# miniaudio 的 wav 解码器去开，失败后**返回 200 但 0 字节**。所以样本
# 必须在接收端就转成真正的 wav，不能靠改名。
DEFAULT_SOURCE = "default"      # 不带 X-TtsVoice-Source 时的来源标识
REF_FILENAME = "ref.wav"        # 每个来源目录里的固定文件名
LEGACY_REF = "ref.wav"          # v1.4.0 及以前的根目录单文件（仍要能读）
NORM_RATE = 24000               # 归一化采样率：与上游 wav 输出一致
NORM_CHANNELS = 1
MIN_VOICE_SECONDS = 1.0         # 短于这个直接拒（1 秒以下克隆出来的音色不像本人）
WARN_VOICE_SECONDS = 3.0        # 1~3 秒可用但要提醒
MAX_VOICE_SECONDS = 12.0        # 超过就截断到 12 秒

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
BYTES_PER_SECOND = 16000        # mp3 128kbps 的字节率（名字不变，与网站侧口径一致）
# wav 是 24kHz / 16bit / 单声道 = 48000 字节/秒；算时长时**要先减掉 44 字节文件头**，
# 否则每块都会算长一点点（短块上足以把退化判定的边界判反）。v1.4.0 新增。
BYTES_PER_SECOND_WAV = 48000
WAV_HEADER_BYTES = 44
DEGENERATE_RATIO = 0.95
SECONDS_PER_TOKEN = 0.08
SYNTH_ATTEMPTS = 3

# ---------------- 采样参数（v1.4.0）----------------
#
# 网站现在随任务下发 sampling（见 HANDOFF-TO-PANEL-1.7B.md 第 2 条）：
# Qwen 服务端默认把 repetition_penalty 压成 1.0（等于关掉重复惩罚），
# 长句、中英数混排的段落会**整段变成噪音**（网站侧实测：不加 4 次只正常 1 次，
# 加了 4 次全正常）。所以这 4 个数必须原样透传给 /v1/audio/speech。
#
# 老请求不带这个字段 —— 那就用同一组默认值（"有则用、无则默认"），
# 这样 v1.3.0 的调用方不需要任何改动、也不会因为缺字段而退化。
DEFAULT_SAMPLING = {
    "temperature": 0.7,
    "top_p": 0.9,
    "top_k": 40,
    "repetition_penalty": 1.05,
}
_SAMPLING_INT_KEYS = ("top_k",)


def normalize_sampling(raw):
    '''
    把请求里的 sampling 归一成那 4 个键：缺的补默认值，非数值一律忽略。

    只认数值（bool 不算）—— 宁可用默认值，也不要把字符串塞进上游请求体。
    top_k 是整数，其余按浮点。
    '''
    out = dict(DEFAULT_SAMPLING)
    if isinstance(raw, dict):
        for key in DEFAULT_SAMPLING:
            value = raw.get(key)
            if isinstance(value, bool) or not isinstance(value, (int, float)):
                continue
            out[key] = int(value) if key in _SAMPLING_INT_KEYS else float(value)
    return out

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


# ---------------- 音色来源（v1.5.0）----------------


def sanitize_source(raw):
    """
    把 X-TtsVoice-Source / payload.source 洗成一个安全的目录名。

    **刻意不复用 safe_name()**：那个函数是为"文件名"写的（保点号、补扩展名），
    用它洗来源会出现 `a.wav` 这种来源名，还会留下 `.` / `..` 这类
    路径语义字符。来源标识只需要当目录名，字符集收紧到字母数字 _ -，
    其余一律换成连字符，再去掉首尾连字符，长度截到 64。

    这条规则与面板 Go 侧的 services.SanitizeVoiceSource() 必须逐字一致：
    不一致会出现"面板删 A、接收端理解成 B"。两边的单测用例是同一组。
    """
    src = re.sub(r"[^A-Za-z0-9_-]", "-", (raw or "").strip()).strip("-")

    if not src:
        return DEFAULT_SOURCE

    return src[:64]


def source_dir(source):
    return os.path.join(ARGS.dir, sanitize_source(source))


def source_ref_path(source):
    return os.path.join(source_dir(source), REF_FILENAME)


def legacy_ref_path():
    return os.path.join(ARGS.dir, LEGACY_REF)


def _first_existing(*paths):
    for p in paths:
        if p and os.path.exists(p):
            return p
    return ""


_TOOLS = {"checked": False, "ffmpeg": "", "ffprobe": ""}


def tool_path(name):
    """
    找 ffmpeg / ffprobe。

    显式列 /opt/homebrew/bin 与 /usr/local/bin：launchd 起的服务 PATH 由 plist
    给（面板写的那份已包含），但从终端手动跑时 PATH 可能不含 Homebrew，
    而这两个工具又恰好只装在 Homebrew 下。
    """
    if not _TOOLS["checked"]:
        _TOOLS["ffmpeg"] = shutil.which("ffmpeg") or _first_existing(
            "/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg")
        _TOOLS["ffprobe"] = shutil.which("ffprobe") or _first_existing(
            "/opt/homebrew/bin/ffprobe", "/usr/local/bin/ffprobe", "/usr/bin/ffprobe")
        _TOOLS["checked"] = True
        log("音频工具：ffmpeg=%s ffprobe=%s" % (_TOOLS["ffmpeg"] or "（缺失）",
                                            _TOOLS["ffprobe"] or "（缺失）"))
    return _TOOLS.get(name) or ""


# 魔数只用来给"这明显不是音频"一个更好懂的错误信息；
# 真正的判据是 ffprobe 能不能解码（魔数对得上的坏文件照样要拒）。
AUDIO_MAGIC = (
    (b"RIFF", "wav"),
    (b"ID3", "mp3"),
    (b"fLaC", "flac"),
    (b"OggS", "ogg"),
    (b"FORM", "aiff"),
    (b"\x1aE\xdf\xa3", "webm"),
    (b"ADIF", "aac"),
    (b"MAC ", "ape"),
)


def sniff_audio(head):
    for magic, fmt in AUDIO_MAGIC:
        if head.startswith(magic):
            return fmt

    if len(head) >= 2 and head[0] == 0xFF and (head[1] & 0xE0) == 0xE0:
        return "mp3"          # 无 ID3 的裸 mp3 / ADTS 帧

    if len(head) >= 12 and head[4:8] == b"ftyp":
        return "m4a"

    return ""


def wav_info(path):
    """
    内置 wav 解析（不依赖 ffprobe）：返回 dict 或 None。
    duration 用 data 块长度 / 字节率算，不信任头里的时长字段
    （头部时长在流式写入的文件里经常是 0 或 0xFFFFFFFF）。
    """
    try:
        with wave.open(path, "rb") as fh:
            frames = fh.getnframes()
            rate = fh.getframerate()
            channels = fh.getnchannels()
            width = fh.getsampwidth()
            comp = fh.getcomptype()
    except Exception:
        return None

    if not rate or comp != "NONE":
        return None

    return {
        "format": "wav",
        "codec": "pcm_s%dle" % (width * 8),
        "duration": float(frames) / float(rate),
        "rate": rate,
        "channels": channels,
        "width": width,
    }


def probe_audio(path):
    """
    用 ffprobe 真解一次；问不到就退回内置 wav 解析。
    返回 (info, err)：info 为 dict（含 duration/format/codec/rate/channels），
    err 为给用户看的中文原因（info 为 None 时才有值）。
    """
    ffprobe = tool_path("ffprobe")

    if not ffprobe:
        info = wav_info(path)
        if info:
            return info, ""
        return None, ("本机没找到 ffprobe，只能校验 wav。请装 ffmpeg"
                      "（brew install ffmpeg）后重试，或直接上传 24kHz/16bit/单声道 wav。")

    cmd = [ffprobe, "-v", "error", "-select_streams", "a:0",
           "-show_entries", "stream=codec_name,channels,sample_rate:format=duration,format_name",
           "-of", "json", path]

    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    except Exception as exc:
        return None, "调用 ffprobe 失败：%s" % exc

    if proc.returncode != 0:
        detail = (proc.stderr or "").strip().splitlines()
        detail = detail[-1] if detail else "未知原因"
        return None, "无法解码这个文件（不是有效的音频？）：%s" % detail

    try:
        doc = json.loads(proc.stdout or "{}")
    except ValueError:
        return None, "ffprobe 输出无法解析"

    streams = doc.get("streams") or []
    if not streams:
        return None, "文件里没有音频流（是不是传了图片或纯文本？）"

    stream = streams[0]
    try:
        duration = float((doc.get("format") or {}).get("duration") or 0.0)
    except (TypeError, ValueError):
        duration = 0.0

    if duration <= 0:
        # 少见的容器不给时长：退回内置解析（wav 时有效）
        wav = wav_info(path)
        if wav:
            duration = wav["duration"]

    try:
        rate = int(stream.get("sample_rate") or 0)
    except (TypeError, ValueError):
        rate = 0

    return {
        "format": (doc.get("format") or {}).get("format_name") or "",
        "codec": stream.get("codec_name") or "",
        "duration": duration,
        "rate": rate,
        "channels": int(stream.get("channels") or 0),
        "width": 2,
    }, ""


def normalize_voice(src, dst):
    """
    把任意解码得开的音频归一化成 <=MAX_VOICE_SECONDS / NORM_RATE / 单声道 /
    PCM s16le WAV。返回 (info, err)，info 是归一化**之后**的实测信息。

    没有 ffmpeg 时不假装成功：只有本来就是目标格式的 wav 才直接放行，
    其余明确报错 —— 再产出一次"解码不了的文件"正是 v1.5.0 要修的东西。
    """
    ffmpeg = tool_path("ffmpeg")

    if not ffmpeg:
        info = wav_info(src)
        if info and info["width"] == 2 and info["rate"] == NORM_RATE \
                and info["channels"] == NORM_CHANNELS:
            if info["duration"] <= MAX_VOICE_SECONDS:
                shutil.copyfile(src, dst)
                return info, ""
            # 超长也要截断 —— 没有 ffmpeg 就做不完整，是"能用"变"不可用"的偷懒，
            # 所以这里用标准库截（本来就是目标格式，不需要转码）。
            # 锚点：不截断的话上游会拿到 20 秒参考音频，合成会慢且不稳定。
            with wave.open(src, "rb") as reader:
                keep = int(MAX_VOICE_SECONDS * NORM_RATE)
                frames = reader.readframes(min(keep, reader.getnframes()))
            with wave.open(dst, "wb") as writer:
                writer.setnchannels(NORM_CHANNELS)
                writer.setsampwidth(2)
                writer.setframerate(NORM_RATE)
                writer.writeframes(frames)
            return (wav_info(dst) or info), ""
        return None, ("本机没找到 ffmpeg，无法转码。请装 ffmpeg（brew install ffmpeg）"
                      "后重试，或上传 %dkHz/16bit/单声道 wav。" % (NORM_RATE // 1000))

    cmd = [ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
           "-i", src,
           "-t", "%.3f" % MAX_VOICE_SECONDS,
           "-ac", str(NORM_CHANNELS), "-ar", str(NORM_RATE),
           "-c:a", "pcm_s16le", "-f", "wav", dst]

    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=180)
    except Exception as exc:
        return None, "调用 ffmpeg 失败：%s" % exc

    if proc.returncode != 0 or not os.path.exists(dst):
        detail = (proc.stderr or "").strip().splitlines()
        detail = detail[-1] if detail else "未知原因"
        return None, "转码失败：%s" % detail

    info = wav_info(dst)
    if not info:
        return None, "转码后的文件不是合法 wav（ffmpeg 未按预期输出）"

    return info, ""


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


def audio_seconds(num_bytes, ext="mp3"):
    """按格式把交付字节数换算成秒数（wav 要先减掉 44 字节文件头）。"""
    if ext == "wav":
        return max(0, num_bytes - WAV_HEADER_BYTES) / float(BYTES_PER_SECOND_WAV)
    return num_bytes / float(BYTES_PER_SECOND)


def is_degenerate(clean_len, max_tokens, ext="mp3"):
    """
    与网站 Providers::synthesizeChunk() 同一个判据。

    退化时输出的是整段静音，时长会**正好**顶到 max_tokens 换算的上限，
    所以这个判据很干净：不是「超过经验值」，而是「撞到硬上限」。

    ext 决定字节率：mp3 16000；wav 48000 且要先减掉文件头。
    """
    if max_tokens <= 0:
        return False
    est_seconds = audio_seconds(clean_len, ext)
    cap_seconds = max_tokens * SECONDS_PER_TOKEN
    return est_seconds >= cap_seconds * DEGENERATE_RATIO


def merge_chunks(paths, ext="mp3"):
    """
    把各块拼成整段。两种格式的差别是这次升级最容易错的地方：

      · mp3：首尾相接就行（每块都是纯音频帧，ID3 已在 _synth_chunk 里剥掉）。
      · wav：**每块都自带 44 字节文件头，不能直接接** —— 直接接的话第二块起
        会夹一个 RIFF 头，播放器（和网站的时长换算）都会错。
        做法：逐块解析 RIFF 子块，收下第一个 "fmt " 与所有 "data" 的纯音频，
        最后补一个**新的**头。子块按偶数字节对齐（sz 为奇数时后面有一字节填充）。
    """
    if ext != "wav":
        parts = []
        for path in paths:
            with open(path, "rb") as fh:
                parts.append(fh.read())
        return b"".join(parts)

    fmt = None
    pcm = b""
    for path in paths:
        with open(path, "rb") as fh:
            data = fh.read()
        off = 12                      # 跳过 "RIFF" + 长度 + "WAVE"
        while off + 8 <= len(data):
            cid = data[off:off + 4]
            size = int.from_bytes(data[off + 4:off + 8], "little")
            body = data[off + 8:off + 8 + size]
            if cid == b"fmt " and fmt is None:
                fmt = body
            elif cid == b"data":
                pcm += body
            off += 8 + size + (size % 2)      # 子块按偶数字节对齐
    if fmt is None:
        raise ValueError("没有找到 wav 的 fmt 子块（上游返回的可能不是合法 wav）")
    hdr = b"WAVE" + b"fmt " + len(fmt).to_bytes(4, "little") + fmt
    hdr += b"\x00" if len(fmt) % 2 else b""
    hdr += b"data" + len(pcm).to_bytes(4, "little")
    return b"RIFF" + len(hdr + pcm).to_bytes(4, "little") + hdr + pcm


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


# ---------------- 多密钥 + 额度 + 用量（v1.6.0）----------------
#
# 为什么把密钥从 plist 挪到文件：
#   plist 里的 --token 是"一个共享密钥"，改一次要重写 root 拥有的 plist 并重启服务。
#   多密钥 + 每密钥额度 + 用量统计这三件事都需要一个**可热更新**的存储，
#   所以：密钥与额度放 keys.json（面板写、接收端读，按 mtime 热加载），
#   用量放 usage.json（接收端写、面板通过 /usage 读）。
#
#   --token 仍然保留为兼容回退：老 plist 里那一份不升级也能继续用；
#   面板"重新部署接收端"时会把它迁移成 keys.json 里的一条 default 记录并清掉 plist 里的值。

KEYS_FILE_VERSION = 1
USAGE_FILE_VERSION = 1
USAGE_DAYS_KEEP = 60          # 只保留最近 60 天的按天统计，避免文件无限增长


def valid_key_id(key_id):
    """密钥 id 会出现在 URL 与日志里，限制成安全字符集。"""
    return bool(re.match(r"^[A-Za-z0-9_-]{1,64}$", key_id or ""))


def today_str(ts=None):
    return time.strftime("%Y-%m-%d", time.localtime(ts or time.time()))


class KeyStore:
    """
    密钥表（面板写的 keys.json）。

    按 mtime 热加载：面板加一条密钥后**不需要重启接收端** ——
    否则每加一个网站的密钥都要重启一次正在跑合成的服务。
    文件损坏/读不到时**不缓存空表**：宁可每次重试读盘，也不要因为一次
    瞬时读取失败就把所有人挡在门外（那时面板写一次文件即可恢复）。
    """

    def __init__(self, path):
        self.path = path
        self.lock = threading.Lock()
        self.mtime = None
        self.keys = []
        self.error = ""

    def _load(self):
        try:
            st = os.stat(self.path)
        except OSError:
            self.keys, self.mtime, self.error = [], None, ""
            return

        if self.mtime is not None and st.st_mtime == self.mtime:
            return

        try:
            with open(self.path, "r", encoding="utf-8") as fh:
                doc = json.load(fh)
            keys = doc.get("keys") if isinstance(doc, dict) else None
            if not isinstance(keys, list):
                raise ValueError("keys 不是数组")
        except (OSError, ValueError) as exc:
            # 读失败不改动已有内存表：一次读盘失败不该让线上所有密钥失效
            self.error = str(exc)
            log("⚠️ 密钥表读取失败（保留上一次的表）：%s" % exc)
            return

        self.keys = [k for k in keys if isinstance(k, dict)]
        self.mtime = st.st_mtime
        self.error = ""

    def all(self):
        with self.lock:
            self._load()
            return list(self.keys)

    def enabled_count(self):
        return sum(1 for k in self.all() if k.get("enabled", True))

    def match(self, presented):
        """
        返回 (结果, 密钥记录)：
          "ok"      验证通过（记录可能为 None = 兼容的 --token）
          "disabled"密钥存在但被停用
          "no"      没有匹配
        用常数时间比较，避免时序侧信道。
        """
        if not presented:
            return "no", None

        hit = None
        hit_disabled = False

        for k in self.all():
            value = str(k.get("key") or "")
            if value and hmac.compare_digest(presented, value):
                if k.get("enabled", True):
                    hit = k
                    break
                hit_disabled = True

        if hit is not None:
            return "ok", hit

        if hit_disabled:
            return "disabled", None

        # 兼容回退：plist 里的 --token（还没迁移的机器）。额度按 0=不限处理。
        if ARGS.token and hmac.compare_digest(presented, ARGS.token):
            return "ok", {"id": "default", "name": "默认密钥（来自服务配置）",
                          "quota_chars": 0, "enabled": True}

        return "no", None

    def quota(self, key_id):
        for k in self.all():
            if k.get("id") == key_id:
                try:
                    return max(0, int(k.get("quota_chars") or 0))
                except (TypeError, ValueError):
                    return 0
        return 0

    def name(self, key_id):
        for k in self.all():
            if k.get("id") == key_id:
                return str(k.get("name") or key_id)
        return key_id


class UsageStore:
    """
    用量统计（接收端写的 usage.json）。

    统计口径（面板与日志都按这套说）：
      · chars        = 提交文本的**字符数**（len(text)，中文 1 字算 1，含标点）
      · requests     = 鉴权通过的接口调用次数
      · jobs         = 提交的合成任务数
      · audio_bytes  = 下载出去的音频字节数
    按密钥 + 按天（保留 60 天）双份记账。
    """

    def __init__(self, path):
        self.path = path
        self.lock = threading.Lock()
        self.doc = {"version": USAGE_FILE_VERSION, "updated": 0, "keys": {}}
        self._load()

    def _load(self):
        try:
            with open(self.path, "r", encoding="utf-8") as fh:
                doc = json.load(fh)
            if isinstance(doc, dict) and isinstance(doc.get("keys"), dict):
                self.doc = doc
        except (OSError, ValueError):
            pass

    def _save(self):
        try:
            self.doc["updated"] = int(time.time())
            atomic_write(self.path, json.dumps(self.doc, ensure_ascii=False, indent=1).encode())
        except OSError as exc:
            # 统计写不进去不能影响合成：只记日志
            log("⚠️ 用量统计写入失败：%s" % exc)

    def _entry(self, key_id):
        keys = self.doc.setdefault("keys", {})
        entry = keys.get(key_id)
        if not isinstance(entry, dict):
            entry = {"chars": 0, "requests": 0, "jobs": 0, "audio_bytes": 0,
                     "first_used": 0, "last_used": 0, "days": {}}
            keys[key_id] = entry
        if not isinstance(entry.get("days"), dict):
            entry["days"] = {}
        return entry

    def record(self, key_id, chars=0, requests=0, jobs=0, audio_bytes=0):
        now = int(time.time())
        with self.lock:
            entry = self._entry(key_id)
            entry["chars"] += int(chars)
            entry["requests"] += int(requests)
            entry["jobs"] += int(jobs)
            entry["audio_bytes"] += int(audio_bytes)
            if not entry["first_used"]:
                entry["first_used"] = now
            entry["last_used"] = now

            if chars or requests or jobs or audio_bytes:
                day = today_str(now)
                d = entry["days"].get(day)
                if not isinstance(d, dict):
                    d = {"chars": 0, "requests": 0, "jobs": 0, "audio_bytes": 0}
                    entry["days"][day] = d
                d["chars"] += int(chars)
                d["requests"] += int(requests)
                d["jobs"] += int(jobs)
                d["audio_bytes"] += int(audio_bytes)

            self._prune(entry)
            self._save()

    def _prune(self, entry):
        days = entry.get("days") or {}
        if len(days) <= USAGE_DAYS_KEEP:
            return
        for day in sorted(days.keys())[:-USAGE_DAYS_KEEP]:
            days.pop(day, None)

    def reset(self, key_id=None):
        """
        清零用量（v1.7.0）。

        key_id 为空 = 清空**所有**密钥的用量。
        只删统计，不动密钥表与样本 —— "重置额度"不该顺手把配置也改了。
        在跑的作业占用不受影响：它们到终态时仍会按实际完成的字数记账
        （也就是从零重新开始计）。
        """
        with self.lock:
            keys = self.doc.setdefault("keys", {})
            if key_id:
                keys.pop(key_id, None)
            else:
                self.doc["keys"] = {}
            self._save()
        return bool(key_id)

    def chars(self, key_id):
        """已用字符数（累计）。"""
        entry = (self.doc.get("keys") or {}).get(key_id)
        if not isinstance(entry, dict):
            return 0
        try:
            return int(entry.get("chars") or 0)
        except (TypeError, ValueError):
            return 0

    def view(self):
        """给 /usage 用的快照：每个密钥的额度、已用、剩余、今日/近 7 天。"""
        now = time.time()
        today = today_str(now)
        week_ago = today_str(now - 6 * 86400)
        out = []
        seen = set()

        def build(key_id, name, quota, reserved=0):
            entry = (self.doc.get("keys") or {}).get(key_id) or {}
            days = entry.get("days") or {}
            used = int(entry.get("chars") or 0)
            # 在跑的作业占用的额度要算进"剩余"，否则面板会显示还有额度、
            # 实际提交却被 429 拒（v1.7.0）
            reserved = max(0, int(reserved or 0))
            today_chars = int((days.get(today) or {}).get("chars") or 0)
            week_chars = sum(int((v or {}).get("chars") or 0)
                             for d, v in days.items() if d >= week_ago)
            return {
                "id": key_id,
                "name": name,
                "quota_chars": quota,
                "used_chars": used,
                "reserved_chars": reserved,
                "remaining_chars": (max(0, quota - used - reserved) if quota > 0 else 0),
                "unlimited": quota <= 0,
                "today_chars": today_chars,
                "week_chars": week_chars,
                "requests": int(entry.get("requests") or 0),
                "jobs": int(entry.get("jobs") or 0),
                "audio_bytes": int(entry.get("audio_bytes") or 0),
                "first_used": int(entry.get("first_used") or 0),
                "last_used": int(entry.get("last_used") or 0),
                "days": {d: int((v or {}).get("chars") or 0) for d, v in days.items()},
            }

        for k in KEYSTORE.all():
            key_id = str(k.get("id") or "")
            if not key_id:
                continue
            seen.add(key_id)
            quota = 0
            try:
                quota = max(0, int(k.get("quota_chars") or 0))
            except (TypeError, ValueError):
                quota = 0
            reserved = 0
            if JOB_MANAGER is not None:
                try:
                    reserved = JOB_MANAGER.outstanding_chars(key_id)
                except Exception:
                    reserved = 0
            item = build(key_id, str(k.get("name") or key_id), quota, reserved)
            item["enabled"] = bool(k.get("enabled", True))
            out.append(item)

        # 兼容用的 --token 还在（面板还没迁移）时，把它也列出来：
        # 它**确实还能调用**，按"已删除/历史"显示就是谎报。
        if ARGS.token and "default" not in seen:
            seen.add("default")
            item = build("default", "默认密钥（来自服务配置，未迁移）", 0)
            item["enabled"] = True
            item["legacy"] = True
            out.append(item)

        # 已经不在密钥表里、但留有历史用量的 id（删掉的密钥）也报出来：
        # 否则"总量对不上"会让人以为统计坏了。界面按"已删除"展示。
        for key_id in sorted((self.doc.get("keys") or {}).keys()):
            if key_id in seen:
                continue
            item = build(key_id, "(已删除的密钥)", 0)
            item["enabled"] = False
            item["deleted"] = True
            out.append(item)

        total = {
            "chars": sum(i["used_chars"] for i in out),
            "today_chars": sum(i["today_chars"] for i in out),
            "week_chars": sum(i["week_chars"] for i in out),
            "requests": sum(i["requests"] for i in out),
            "jobs": sum(i["jobs"] for i in out),
            "audio_bytes": sum(i["audio_bytes"] for i in out),
        }
        # 全局按天曲线（面板画趋势用）
        days_total = {}
        for i in out:
            for d, c in (i.get("days") or {}).items():
                days_total[d] = days_total.get(d, 0) + c
        total["days"] = dict(sorted(days_total.items())[-30:])
        return out, total


KEYS_FILE = None
KEYSTORE = None
USAGE = None


def key_authed(handler):
    """
    统一的鉴权入口。返回 (key_id, 错误信息)：
      key_id 非空 = 通过（"default" 表示未配置鉴权时的匿名调用）
      错误信息非空 = 拒绝，调用方据此返回 403
    """
    store = KEYSTORE
    configured = bool(ARGS.token) or (store is not None and store.enabled_count() > 0)

    if not configured:
        # 没配任何密钥 = 不校验，与 v1.5.0 之前的行为一致（启动时打醒目警告）
        return "default", ""

    result, rec = store.match(handler._presented_token())

    if result == "ok":
        return (str(rec.get("id")) if rec else "default"), ""

    if result == "disabled":
        return "", "密钥已被停用"

    return "", "密钥不匹配"


def quota_check(key_id, chars, reserved=0):
    """
    额度检查。返回 (错误信息, 剩余额度)：
      错误信息非空 = 超额（调用方返回 429）
      剩余额度为 0 表示不限量

    reserved 是"已经在跑的作业占用的字数"（v1.7.0）。必须算进去，
    否则同一把密钥连提多个任务、每个都在额度内，合起来就超了。
    """
    quota = KEYSTORE.quota(key_id) if KEYSTORE else 0

    if quota <= 0:
        return "", 0

    used = USAGE.chars(key_id)
    reserved = max(0, int(reserved or 0))
    remaining = max(0, quota - used - reserved)

    if chars > remaining:
        detail = "已用 %d 字" % used
        if reserved:
            detail += "，在跑的作业占用 %d 字" % reserved
        return ("额度不足：本密钥额度 %d 字，%s，本次需要 %d 字，剩余 %d 字。"
                "（任务失败或中途取消时，只按**实际合成完成**的字数计费，"
                "没跑到的部分会自动退回。）请在面板「调用密钥」里调整额度、"
                "清零用量，或换一个密钥。" % (quota, detail, chars, remaining)), 0

    return "", remaining


def count_chars(text):
    """统计字符串的字符数（中文按 1 个字符算）。"""
    return len(text or "")


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

    def job_ext(self, job):
        """交付格式的扩展名。只认 wav/mp3，别的一律按 mp3。"""
        fmt = str((job or {}).get("response_format") or "mp3").lower()
        return "wav" if fmt == "wav" else "mp3"

    def chunk_path(self, job, idx):
        """单块文件路径：扩展名跟着格式走，wav 块自带 44 字节头。"""
        return os.path.join(self.chunk_dir(job.get("job_id")), "%d.%s" % (idx, self.job_ext(job)))

    def final_path(self, job):
        """整段成品路径。参数从 job_id 改成 job：文件名要跟着格式走（不再写死 final.mp3）。"""
        job_id = job.get("job_id") if isinstance(job, dict) else str(job)
        return os.path.join(self.chunk_dir(job_id), "final.%s" % self.job_ext(job))

    # ---------- 状态读写 ----------

    def load(self, job_id):
        try:
            with open(self.json_path(job_id), "r", encoding="utf-8") as fh:
                return json.load(fh)
        except (OSError, ValueError):
            return None

    def save(self, job):
        job["updated"] = int(time.time())
        # 结算钩子挂在 save 上，而不是散在 9 个改状态的地方：
        # 作业有三种终态（ready/failed/cancelled）、还有取消/超时清理/恢复三条路径，
        # 逐个改必然漏掉一条，而漏掉的后果是"该扣的没扣"或"该退的没退"。
        self.settle_if_terminal(job)
        atomic_write(self.json_path(job["job_id"]),
                     json.dumps(job, ensure_ascii=False).encode("utf-8"))

    def settle_if_terminal(self, job):
        """
        作业到达终态时按**实际合成完成的块**结算额度（v1.7.0）。

        为什么不是提交时就扣满：合成会失败、会被取消、会停机重启。用户提交 1000 字、
        跑到第 3 块就失败，却按 1000 字收费，那是把"我们的失败"算在用户头上。
        所以：提交时**占额度**（防止并发超支），终态时按已完成的块结算，
        没跑到的那部分原样退回。

        只处理"新格式"的作业（有 key/chars 字段）：v1.6 的老作业在提交时就已经
        扣过费了，再去结算会重复计费。
        """
        if job.get("charged") or "key" not in job:
            return

        if job.get("status") not in (JOB_READY, JOB_FAILED, JOB_CANCELLED):
            return

        chunks = job.get("chunks") or []
        try:
            done = max(0, min(int(job.get("done") or 0), len(chunks)))
        except (TypeError, ValueError):
            done = 0

        consumed = sum(count_chars(str((chunks[i] or {}).get("text") or ""))
                       for i in range(done))
        try:
            submitted = max(consumed, int(job.get("chars") or 0))
        except (TypeError, ValueError):
            submitted = consumed

        job["charged"] = True
        job["charged_chars"] = consumed

        key_id = str(job.get("key") or "default")
        if USAGE is not None:
            USAGE.record(key_id, chars=consumed, jobs=1)

        if consumed < submitted:
            log("作业 %s %s：按实际完成的 %d/%d 字结算（退回 %d 字）"
                % (job.get("job_id"), job.get("status"), consumed, submitted,
                   submitted - consumed))

    def outstanding_chars(self, key_id):
        """
        该密钥**在跑的作业占用的额度**（已提交、还没到终态）。

        没有这一步，同一把密钥连提 3 个任务、每个都在额度内，合起来就超了 ——
        额度会变成"看起来拦得住、实际拦不住"。
        """
        total = 0
        for j in self.list_jobs():
            if j.get("key") != key_id or j.get("charged"):
                continue
            if j.get("status") in (JOB_QUEUED, JOB_RUNNING):
                try:
                    total += int(j.get("chars") or 0)
                except (TypeError, ValueError):
                    pass
        return total

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

    def submit(self, payload, key_id="default"):
        """
        校验并落盘一个新任务；返回 job dict。校验失败抛 ValueError。

        key_id 与 chars 会写进 job：额度按**实际合成完成的块**结算
        （v1.7.0），所以作业自己必须记住"是谁提交的、一共多少字"，
        否则中途失败/取消时没人知道该退多少。
        """
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
        # v1.5.0：来源标识。取参考音频的优先级（交接文档 §3.5）：
        #   显式 ref_audio → <dir>/<source>/ref.wav（没有旧版回退）
        source = sanitize_source(payload.get("source"))
        # 两个都给会「静默按其中一个生效」，最难排查 —— 仍然直接拒
        if voice and ref_audio:
            raise ValueError("exactly one of voice / ref_audio is required")
        if ref_audio and not os.path.exists(ref_audio):
            raise ValueError("ref_audio not found: %s" % ref_audio)
        if not voice and not ref_audio:
            # 只给 source（或什么都没给，即 default）：按来源解析样本。
            # 不做旧版回退（不读根目录那份 v1.4.0 老文件）—— 插件还在开发期，
            # 与其猜"该用哪份"，不如明确报错让调用方先上传。
            candidate = source_ref_path(source)
            if os.path.exists(candidate):
                ref_audio = candidate
            else:
                raise ValueError(
                    "voice sample not found for source '%s' (looked at %s); "
                    "upload one first" % (source, candidate))

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
            # v1.5.0：来源标识要存进 job —— 面板"各来源"里的"最后使用时间"
            # 就是靠它反查（否则只能看文件 mtime，那是上传时间不是使用时间）
            "source": source,
            "ref_text": str(payload.get("ref_text") or ""),
            "response_format": str(payload.get("response_format") or "mp3") or "mp3",
            # 采样参数随任务下发（v1.4.0）：缺省就是那组默认值，见 DEFAULT_SAMPLING
            "sampling": normalize_sampling(payload.get("sampling")),
            "total": len(chunks),
            "done": 0,
            "chunk_bytes": [],
            "attempts": [],
            "error": "",
            "cold": False,
            "created": now,
            "updated": now,
            "chunks": chunks,
            # v1.7.0：额度结算用。key 是提交者，chars 是这次提交的总字数，
            # charged 表示"到终态时已经结算过"（避免恢复/清理时重复扣）。
            "key": key_id,
            "chars": sum(count_chars(c["text"]) for c in chunks),
            "charged": False,
        }
        os.makedirs(self.chunk_dir(job_id), exist_ok=True)
        self.save(job)
        self.wake.set()
        log("作业已入队 %s（%d 块，model=%s，来源=%s，样本=%s）"
            % (job_id, len(chunks), model, source, ref_audio or ("voice:" + voice)))
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

            atomic_write(self.chunk_path(job, idx), audio)
            chunk_bytes.append(len(audio))
            attempts_all.append(attempts)
            done = idx + 1
            job["done"] = done
            job["chunk_bytes"] = chunk_bytes
            job["attempts"] = attempts_all
            self.save(job)
            log("作业 %s 进度 %d/%d（%d 字节，尝试 %d 次）"
                % (job_id, done, len(chunks), len(audio), attempts))

        # 全部完成：拼接成整段（分格式，见 merge_chunks）
        paths = [self.chunk_path(job, i) for i in range(len(chunks))]
        for i, p in enumerate(paths):
            if not os.path.exists(p):
                job["status"] = JOB_FAILED
                job["error"] = "拼接时读不到第 %d 块：%s" % (i + 1, p)
                self.save(job)
                return
        try:
            merged = merge_chunks(paths, self.job_ext(job))
        except Exception as exc:      # noqa: BLE001 - 拼不出来必须如实报错，不能交付半个文件
            job["status"] = JOB_FAILED
            job["error"] = "拼接失败：%s" % exc
            self.save(job)
            log("作业 %s 拼接失败：%s" % (job_id, job["error"]))
            return
        final = self.final_path(job)
        atomic_write(final, merged)
        job["status"] = JOB_READY
        job["done"] = len(chunks)
        self.save(job)
        log("作业 %s 完成（%d 块，共 %d 字节，格式 %s）"
            % (job_id, len(chunks), len(merged), self.job_ext(job)))

    def _synth_chunk(self, job, chunk):
        """
        合成一块，含退化检测与重试。返回 (clean_audio, used_bytes, attempts)。

        重试策略与网站一致：退化就重来一次，最多 SYNTH_ATTEMPTS 次；
        上游 5xx/连接错误按退避重试。

        格式与采样参数都跟着 job 走（v1.4.0）：
          · response_format 用任务里的值，不再写死 mp3；
          · sampling 的 4 个参数原样放进请求体（缺省用 DEFAULT_SAMPLING）——
            不传的话上游 repetition_penalty 是 1.0，长句会整段变噪音。
        """
        text = chunk["text"]
        max_tokens = int(chunk["max_tokens"])
        ext = self.job_ext(job)
        last_err = ""

        for attempt in range(1, SYNTH_ATTEMPTS + 1):
            if self.is_cancelled(job["job_id"]):
                raise ChunkFailed("cancelled")
            body = {
                "model": job["model"],
                "input": text,
                "max_tokens": max_tokens,
                "response_format": ext,
            }
            body.update(job.get("sampling") or DEFAULT_SAMPLING)
            # voice 与 ref_audio 恰好一个（提交时已校验）
            if job.get("voice"):
                body["voice"] = job["voice"]
            if job.get("ref_audio"):
                body["ref_audio"] = job["ref_audio"]
                if job.get("ref_text"):
                    body["ref_text"] = job["ref_text"]
            # 采样参数已由上面的 body.update(sampling) 下发（v1.4.0）。
            # 仍然不要自己改成 temperature=0：那是贪心解码，实测 100% 退化。

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

            if is_degenerate(len(clean), max_tokens, ext):
                last_err = "输出退化（%.1f 秒，顶到 %.1f 秒上限）" % (
                    audio_seconds(len(clean), ext), max_tokens * SECONDS_PER_TOKEN)
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

        self._access_log(code, payload)

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
        """兼容旧调用点：只要能匹配任一启用的密钥就算通过。"""
        return self._auth_key()[0] != ""

    def _auth_key(self):
        """
        统一鉴权：返回 (key_id, error)。key_id 非空 = 通过。
        v1.6.0 起密钥表（keys.json）是唯一事实来源，--token 只作兼容回退。
        """
        key_id, err = key_authed(self)
        # 记在实例上，供访问日志标注"这条请求用的是哪把密钥"
        self._key_id = key_id or "-"
        return key_id, err

    def _access_log(self, code, payload):
        """
        访问日志：一次请求一行（健康检查与 /jobs 轮询除外）。

        为什么必须有：真实事故里插件报「接收端拒绝：HTTP 200」，
        而接收端这边**一条日志都没有** —— 于是只能靠"没有日志"反推
        "请求根本没到这台机器"（最后查明是插件把服务器地址填错了）。
        有这条日志就能一眼看出：请求到没到、带的是哪把密钥、被哪条规则拒的。
        """
        path = self.path.split("?")[0]

        if path in ("/voice/health", "/health"):
            return  # 健康检查会被轮询，记它只会把日志淹掉

        polling = path.startswith("/jobs/") and self.command == "GET"
        if code < 300 and (polling or (self.command == "GET"
                                       and path not in ("/voice/sources", "/usage", "/jobs"))):
            return  # 成功的查询/轮询不记，只记"有副作用"的与"被拒的"

        reason = ""
        if isinstance(payload, dict):
            for k in ("error", "msg", "message"):
                v = payload.get(k)
                if isinstance(v, str) and v.strip():
                    reason = " ".join(v.split())
                    break

        log("%s %s → %d key=%s ip=%s%s"
            % (self.command, path, code, getattr(self, "_key_id", "-"),
               self.client_address[0], (" ｜ " + reason[:200]) if reason else ""))

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
            keys = KEYSTORE.all() if KEYSTORE else []

            return self._json(200, {
                "ok": True,
                "version": VERSION,
                "dir": os.path.abspath(directory),
                "writable": bool(os.path.isdir(directory) and os.access(directory, os.W_OK)),
                # auth=true 表示"配置了鉴权"（密钥表里有启用的密钥，或还留着兼容的 --token）
                "auth": bool(ARGS.token) or any(k.get("enabled", True) for k in keys),
                "keys": len(keys),
                "keys_file": os.path.abspath(ARGS.keys_file),
                "upstream": ARGS.upstream,
            })

        # ---------- /usage（v1.6.0：用量统计 + 每密钥额度）----------
        if path == "/usage":
            key_id, err = self._auth_key()
            if not key_id:
                return self._fail(403, err)
            USAGE.record(key_id, requests=1)
            items, total = USAGE.view()
            return self._json(200, {"ok": True, "keys": items, "total": total})

        # ---------- /voice/sources（v1.5.0：面板"各来源"页）----------
        if path == "/voice/sources":
            return self._voice_sources()

        # ---------- /jobs/*（v1.3.0）----------
        # 统一用密钥表鉴权（与 /voice 同一套）
        if path == "/jobs":
            key_id, err = self._auth_key()
            if not key_id:
                return self._fail_jobs(403, "unauthorized" if not err else "unauthorized")
            USAGE.record(key_id, requests=1)
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
            key_id, err = self._auth_key()
            if not key_id:
                return self._fail(403, err or "密钥不匹配")
            USAGE.record(key_id, requests=1)

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
                "auth": bool(ARGS.token) or (KEYSTORE.enabled_count() > 0),
            })

        # 其余 GET 走代理（/v1/models 等）
        if path.startswith("/v1"):
            return self._proxy()

        return self._fail(404, "接口不存在：%s" % self.path)

    def do_POST(self):
        path = self.path.split("?", 1)[0].rstrip("/")

        if path == "/jobs":
            return self._jobs_create()

        if path == "/usage/reset":
            return self._usage_reset()

        if path in ("/voice", "/voice/upload"):
            return self._receive_voice()

        if path.startswith("/v1"):
            return self._proxy()

        return self._fail(404, "接口不存在：%s" % self.path)

    def do_DELETE(self):
        path = self.path.split("?", 1)[0].rstrip("/")

        if path.startswith("/voice/sources/"):
            return self._voice_source_delete(path)

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
            # v1.5.0：来源标识随任务一起回传，网站与面板都据此判断用的是哪份音色
            "source": job.get("source") or DEFAULT_SOURCE,
            "created": job.get("created", 0),
            "updated": job.get("updated", 0),
            "cold": bool(job.get("cold")),
        }

    def _jobs_get(self, path):
        """GET /jobs/{id} 与 GET /jobs/{id}/audio"""
        key_id, err = self._auth_key()
        if not key_id:
            return self._fail_jobs(403, "unauthorized")
        USAGE.record(key_id, requests=1)

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

        final = JOB_MANAGER.final_path(job)
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
        # Content-Type 跟着交付格式走（wav / mp3）——写死 audio/mpeg 会让浏览器
        # 把 wav 当 mp3 处理，`<audio>` 在部分浏览器上直接不播。
        self.send_header("Content-Type",
                         "audio/wav" if JOB_MANAGER.job_ext(job) == "wav" else "audio/mpeg")
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

        # 用量统计：下载出去的音频字节数（整段或 Range 片段的实际字节）
        USAGE.record(key_id, audio_bytes=length)

    def _jobs_create(self):
        key_id, err = self._auth_key()
        if not key_id:
            log("拒绝提交作业：%s（来自 %s）" % (err or "密钥不匹配", self.client_address[0]))
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

        # ---- 额度检查（v1.6.0）：按提交文本的字符数计 ----
        # 幂等重放（同一 client_id）不重复扣：submit() 会直接返回已有任务。
        client_id = str(payload.get("client_id") or "").strip()
        existing = JOB_MANAGER.find_by_client_id(client_id) if client_id else None
        chars = 0
        chunks_in = payload.get("chunks")
        if isinstance(chunks_in, list):
            for c in chunks_in:
                if isinstance(c, dict):
                    chars += count_chars(str(c.get("text") or ""))

        if not existing and chars > 0:
            reserved = JOB_MANAGER.outstanding_chars(key_id)
            qerr, _ = quota_check(key_id, chars, reserved)
            if qerr:
                log("拒绝提交作业：%s（key=%s，%d 字，占用 %d 字）"
                    % (qerr, key_id, chars, reserved))
                return self._fail_jobs(429, "quota exceeded: %s" % qerr)

        try:
            job = JOB_MANAGER.submit(payload, key_id)
        except QueueFullError:
            return self._fail_jobs(429, "queue full")
        except ValueError as exc:
            return self._fail_jobs(400, str(exc))
        except Exception as exc:
            log("提交作业失败：%s" % exc)
            return self._fail_jobs(500, "internal error: %s" % exc)

        if not existing:
            # v1.7.0：提交时**不扣字数额度**（只占用），字数在作业到终态时按
            # 实际完成的块结算；这里只记一次请求。
            USAGE.record(key_id, requests=1)

        return self._json(200, {
            "ok": True,
            "job_id": job.get("job_id"),
            "status": job.get("status"),
            "total": job.get("total"),
            "chars": chars,
        })

    def _jobs_delete(self, path):
        """取消/清理。幂等：未知 job 也返回 200（网站删除时不至于报错）。"""
        key_id, err = self._auth_key()
        if not key_id:
            return self._fail_jobs(403, "unauthorized")
        USAGE.record(key_id, requests=1)

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
        key_id, err = self._auth_key()
        if not key_id:
            log("拒绝上传：%s（来自 %s）" % (err or "密钥不匹配", self.client_address[0]))
            return self._fail(403, err or "密钥不匹配")
        USAGE.record(key_id, requests=1)

        data, err = self._read_body(MAX_BYTES)

        if data is None:
            return self._fail(413 if "过大" in err else 400, err)

        if not data:
            return self._fail(400, "请求体为空")

        # v1.5.0：来源隔离。name 只用于回显（用户传的原始文件名），
        # 落盘文件名固定是 ref.wav，来源决定落在哪个子目录。
        name = safe_name(self.headers.get("X-TtsVoice-Name"))
        source = sanitize_source(self.headers.get("X-TtsVoice-Source"))
        out_dir = source_dir(source)
        target = os.path.join(out_dir, REF_FILENAME)

        head = data[:16]
        sniffed = sniff_audio(head)

        if not sniffed:
            return self._fail(400, "这个文件不像音频（开头不是 wav/mp3/m4a/flac/ogg 等已知格式）。"
                                   "请上传真正的音频文件，不要改扩展名来糊弄 —— "
                                   "上游按扩展名选解码器，改名的假 wav 会让合成返回空音频。")

        try:
            os.makedirs(out_dir, exist_ok=True)
        except OSError as exc:
            log("创建目录失败：%s" % exc)
            return self._fail(500, "创建目录失败：%s" % exc)

        # 原始字节先落到目标目录里的临时文件（同一文件系统，后面 os.replace 才是原子的）
        raw = os.path.join(out_dir, ".upload-%d.%s" % (os.getpid(), sniffed or "bin"))
        part = target + ".part"

        try:
            with open(raw, "wb") as fh:
                fh.write(data)
                fh.flush()
                os.fsync(fh.fileno())
        except OSError as exc:
            log("写入上传临时文件失败：%s" % exc)
            return self._fail(500, "写入失败：%s" % exc)

        try:
            info, perr = probe_audio(raw)
            if not info:
                return self._fail(400, perr)

            # 归一化：真正的判据在这一步 —— ffmpeg 解得开才叫音频
            norm, nerr = normalize_voice(raw, part)
            if not norm:
                return self._fail(400, nerr)

            duration = float(norm.get("duration") or 0.0)
            # 「是否截断」要看**原始**时长：归一化之后的时长恒为 ≤12s，
            # 拿它判会把每个超长样本都判成没截断（第一版就是这么错的）。
            truncated = float(info.get("duration") or 0.0) > MAX_VOICE_SECONDS + 0.05

            if duration < MIN_VOICE_SECONDS:
                return self._fail(400, "参考音频太短（%.1f 秒，至少要 %.1f 秒）："
                                       "太短的样本克隆出来的音色不像本人。"
                                       % (duration, MIN_VOICE_SECONDS))

            os.replace(part, target)
        except OSError as exc:
            log("写入失败：%s" % exc)
            return self._fail(500, "写入失败：%s" % exc)
        finally:
            for p in (raw, part):
                try:
                    if os.path.exists(p):
                        os.remove(p)
                except OSError:
                    pass

        with open(target, "rb") as fh:
            norm_bytes = fh.read()

        digest = hashlib.sha256(norm_bytes).hexdigest()
        warning = ""
        if duration < WARN_VOICE_SECONDS:
            warning = ("样本只有 %.1f 秒，偏短（建议 %.0f 秒以上），克隆音色可能不太像本人。"
                       % (duration, WARN_VOICE_SECONDS))
        if truncated:
            warning = ((warning + " ") if warning else "") + \
                "原音频超过 %.0f 秒，已截断到 %.0f 秒。" % (MAX_VOICE_SECONDS, MAX_VOICE_SECONDS)

        log("已接收来源 %s 的音色样本 %s→%s（%d 字节原始 %s，归一化后 %.1fs / %dHz / %dch，sha256 %s…）"
            % (source, name, target, len(data), sniffed or "?",
               duration, NORM_RATE, NORM_CHANNELS, digest[:12]))

        resp = {
            "ok": True,
            "path": os.path.abspath(target),
            "name": name,
            "source": source,
            "format": "wav",
            "duration": round(duration, 3),
            "size": len(norm_bytes),
            "sha256": digest,
        }
        if warning:
            resp["warning"] = warning
        if truncated:
            resp["truncated"] = True

        return self._json(200, resp)

    # ---------- ③ 用量重置（v1.7.0，仅面板可用）----------

    def _usage_reset(self):
        """
        清零用量。**必须带管理密钥**（--admin-token）。

        为什么不能只认普通调用密钥：额度是给调用方设的约束，如果能用调用密钥
        把自己的已用量清零，额度就只是"建议"了。管理密钥只在面板手里
        （写在 plist 里，root 可读，网站插件拿不到）。
        """
        admin = (ARGS.admin_token or "")

        if not admin:
            return self._fail(503, "接收端没有配置管理密钥（--admin-token），"
                                   "无法重置用量。请在应用市场重新部署一次接收端。")

        presented = (self.headers.get("X-TtsVoice-Admin") or "").strip()
        if not presented or not hmac.compare_digest(presented, admin):
            log("拒绝重置用量：管理密钥不匹配（来自 %s）" % self.client_address[0])
            return self._fail(403, "管理密钥不匹配")

        body, err = self._read_body(64 * 1024)
        if body is None:
            return self._fail(400, err)

        key_id = ""
        reset_all = False
        if body:
            try:
                doc = json.loads(body.decode("utf-8", "replace"))
            except ValueError as exc:
                return self._fail(400, "invalid json: %s" % exc)
            if isinstance(doc, dict):
                key_id = str(doc.get("key_id") or "").strip()
                reset_all = bool(doc.get("all"))

        if not key_id and not reset_all:
            return self._fail(400, "要指定 key_id，或传 {\"all\": true} 清空全部")

        if reset_all:
            USAGE.reset(None)
            log("已清空**全部**密钥的用量统计（来自 %s）" % self.client_address[0])
            msg = "已清空全部用量统计"
        else:
            USAGE.reset(key_id)
            log("已清零密钥 %s 的用量统计（来自 %s）" % (key_id, self.client_address[0]))
            msg = "已清零密钥 %s 的用量统计" % key_id

        items, total = USAGE.view()
        return self._json(200, {"ok": True, "message": msg,
                                "keys": items, "total": total})

    # ---------- ①b 各来源列表 / 删除（v1.5.0，面板"各来源"用）----------

    def _voice_sources(self):
        """
        列出 <dir>/<source>/ref.wav 各来源。

        v1.4.0 留在根目录的 ref.wav 也列出来（source=default、legacy=true），
        但它**只是残留**：v1.5.0 不再读它（解析只认 <dir>/<source>/ref.wav）。
        列出来是为了"磁盘上还有什么"看得见、并且能在面板里删掉 ——
        藏起来会让用户以为那份坏文件已经没了。

        last_used 从作业记录里反查：**样本的使用时间**才是用户关心的
        （mtime 只是上传时间），而 job 里存了 source。
        """
        key_id, err = self._auth_key()
        if not key_id:
            return self._fail(403, err or "密钥不匹配")
        USAGE.record(key_id, requests=1)

        last_used = {}

        try:
            entries = sorted(os.listdir(ARGS.jobs_dir))
        except OSError:
            entries = []

        for entry in entries:
            if not entry.endswith(".json"):
                continue

            try:
                with open(os.path.join(ARGS.jobs_dir, entry), "r", encoding="utf-8") as fh:
                    job = json.load(fh)
            except (OSError, ValueError):
                continue

            src = sanitize_source(job.get("source"))
            when = int(job.get("created") or job.get("updated") or 0)

            if when > last_used.get(src, 0):
                last_used[src] = when

        items = []

        def add(src, path, legacy=False):
            try:
                st = os.stat(path)
            except OSError:
                return

            info = wav_info(path) or {}
            digest = ""
            try:
                with open(path, "rb") as fh:
                    digest = hashlib.sha256(fh.read()).hexdigest()
            except OSError:
                pass

            items.append({
                "source": src,
                "path": path,
                "size": st.st_size,
                "duration": round(float(info.get("duration") or 0.0), 3),
                "sha256": digest,
                "modified": int(st.st_mtime),
                "last_used": int(last_used.get(src, 0)),
                "legacy": bool(legacy),
            })

        try:
            subs = sorted(os.listdir(ARGS.dir))
        except OSError:
            subs = []

        for sub in subs:
            if sub.startswith("."):
                continue
            path = os.path.join(ARGS.dir, sub, REF_FILENAME)
            if os.path.isfile(path):
                add(sub, path)

        legacy = legacy_ref_path()
        if os.path.isfile(legacy):
            # v1.4.0 的残留文件：列出来（能在面板里删掉），但**不参与解析** ——
            # 它和 <dir>/default/ref.wav 可能同时存在，那时的两行是两份不同的文件。
            add(DEFAULT_SOURCE, legacy, legacy=True)

        items.sort(key=lambda i: (i["source"], i["legacy"]))
        return self._json(200, {"ok": True, "dir": os.path.abspath(ARGS.dir),
                                "sources": items})

    def _voice_source_delete(self, path):
        """DELETE /voice/sources/{source}：删掉该来源目录（幂等）。"""
        key_id, err = self._auth_key()
        if not key_id:
            return self._fail(403, err or "密钥不匹配")
        USAGE.record(key_id, requests=1)

        raw = path[len("/voice/sources/"):]
        source = sanitize_source(unquote(raw))
        target = source_dir(source)
        removed = []

        # 只删自己拼出来的绝对路径，且必须是 ARGS.dir 的直接子目录 ——
        # 双保险：sanitize_source 已去掉路径语义字符，这里再确认一次前缀。
        if os.path.isdir(target) and os.path.dirname(target) == os.path.abspath(ARGS.dir):
            shutil.rmtree(target, ignore_errors=True)
            removed.append(target)

        if source == DEFAULT_SOURCE:
            legacy = legacy_ref_path()
            if os.path.isfile(legacy):
                try:
                    os.remove(legacy)
                    removed.append(legacy)
                except OSError as exc:
                    return self._fail(500, "删除失败：%s" % exc)

        if removed:
            log("已删除来源 %s 的音色样本：%s" % (source, "、".join(removed)))

        return self._json(200, {"ok": True, "source": source, "removed": removed})

    # ---------- ② 转发到 Qwen 服务 ----------

    def _proxy(self):
        key_id, err = self._auth_key()
        if not key_id:
            log("拒绝代理：%s（来自 %s，%s）"
                % (err or "密钥不匹配", self.client_address[0], self.path.split("?")[0]))
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
        speech_chars = 0

        if self.path.split("?", 1)[0].rstrip("/") == "/v1/audio/speech" and body:
            try:
                speech_req = json.loads(body.decode("utf-8", "replace"))
                requested_model = str(speech_req.get("model") or "")
                # 额度也管直连 /v1/audio/speech 的调用（不走 /jobs 的那条路）
                speech_chars = count_chars(str(speech_req.get("input") or speech_req.get("text") or ""))
            except ValueError:
                requested_model = ""

            if requested_model:
                model_state = upstream_models()

            if speech_chars > 0:
                qerr, _ = quota_check(key_id, speech_chars,
                                      JOB_MANAGER.outstanding_chars(key_id))
                if qerr:
                    log("拒绝合成：%s（key=%s，%d 字）" % (qerr, key_id, speech_chars))
                    return self._fail(429, "quota exceeded: %s" % qerr)

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

            # 用量统计：只有上游成功（2xx）才算消耗；失败的调用只计一次请求
            if 200 <= resp.status < 300:
                USAGE.record(key_id, chars=speech_chars, requests=1,
                             audio_bytes=len(payload))
            else:
                USAGE.record(key_id, requests=1)

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
    global ARGS, JOB_MANAGER, KEYSTORE, KEYS_FILE, USAGE

    parser = argparse.ArgumentParser(description="TtsVoice 接收端 + 带鉴权的 TTS 代理")
    parser.add_argument("--dir", default=os.path.expanduser("~/tts/voice-samples"),
                        help="音色样本存放目录（默认 ~/tts/voice-samples）")
    parser.add_argument("--host", default="0.0.0.0",
                        help="监听地址（默认 0.0.0.0）")
    parser.add_argument("--port", type=int, default=8899, help="监听端口（默认 8899）")
    parser.add_argument("--token", default="",
                        help="兼容用的单个共享密钥（v1.6.0 起请改用 --keys-file）")
    parser.add_argument("--keys-file", default=os.path.expanduser("~/tts/voice-receiver/keys.json"),
                        help="多密钥 + 额度的 JSON（面板写、本服务按 mtime 热加载）")
    parser.add_argument("--usage-file", default="",
                        help="用量统计落盘路径（默认与 keys-file 同目录的 usage.json）")
    parser.add_argument("--admin-token", default="",
                        help="管理密钥：只有它（面板持有）能调用用量重置等管理接口；"
                             "普通调用密钥不能重置自己的额度，否则额度形同虚设")
    parser.add_argument("--upstream", default="http://127.0.0.1:8880",
                        help="上游 TTS 服务地址（默认 http://127.0.0.1:8880）")
    parser.add_argument("--jobs-dir", default=os.path.expanduser("~/tts/jobs"),
                        help="作业队列的落盘目录（默认 ~/tts/jobs）")
    ARGS = parser.parse_args()

    if not ARGS.usage_file:
        ARGS.usage_file = os.path.join(os.path.dirname(os.path.abspath(ARGS.keys_file)),
                                       "usage.json")

    os.makedirs(ARGS.dir, exist_ok=True)
    os.makedirs(ARGS.jobs_dir, exist_ok=True)
    os.makedirs(os.path.dirname(os.path.abspath(ARGS.keys_file)), exist_ok=True)

    KEYS_FILE = ARGS.keys_file
    KEYSTORE = KeyStore(ARGS.keys_file)
    USAGE = UsageStore(ARGS.usage_file)

    # 作业 worker 在进程内起一条线程（契约要求同一进程/端口）：
    # 单 worker、串行 —— 与上游 "Keep all GPU work serialized" 一致。
    JOB_MANAGER = JobManager(ARGS.jobs_dir, ARGS.upstream)
    JOB_MANAGER.start()

    enabled = [k for k in KEYSTORE.all() if k.get("enabled", True)]

    if not enabled and not ARGS.token:
        log("=" * 66)
        log("⚠️  没有任何可用的密钥（keys.json 里没有启用项，也没有 --token）")
        log("    本代理将不做任何鉴权，而上游 Qwen 服务通常也没有鉴权 ——")
        log("    等于把语音合成能力完全敞开给能连到这个端口的人。")
        log("    请在面板「服务管理 → 音色接收端 → 详情 → 🔑 调用密钥」里添加一个密钥。")
        log("=" * 66)

    server = ThreadingHTTPServer((ARGS.host, ARGS.port), Handler)

    log("TtsVoice 接收端 + 代理 v%s 已启动" % VERSION)
    log("  监听    : http://%s:%d" % (ARGS.host, ARGS.port))
    log("  样本目录: %s" % os.path.abspath(ARGS.dir))
    log("  上游    : %s" % ARGS.upstream)
    log("  作业目录: %s" % os.path.abspath(ARGS.jobs_dir))
    log("  密钥表  : %s（启用 %d 条%s）"
        % (os.path.abspath(ARGS.keys_file), len(enabled),
           "，另有兼容 --token" if ARGS.token else ""))
    log("  用量统计: %s" % os.path.abspath(ARGS.usage_file))
    log("  管理接口: %s" % ("已启用（用量重置需要 --admin-token）" if ARGS.admin_token
                            else "★ 未配置 --admin-token：用量重置不可用（在面板里重新部署接收端即可）"))
    log("  鉴权    : %s" % ("已启用" if (enabled or ARGS.token) else "★ 未启用"))
    log("")
    log("  插件里这样填：")
    log("    openaiBaseUrl = http://<本机地址>:%d/v1" % ARGS.port)
    log("    openaiKey     = <面板里为该站点生成的密钥>")
    log("")
    log("  接收端地址与密钥由插件自动从上面两项推导，不用另外填。")
    log("  模型驻留状态（要不要等冷加载）：GET /voice/status")
    log("  用量与额度：GET /usage（面板「调用密钥」页读的就是它）")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log("收到中断，退出")
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
