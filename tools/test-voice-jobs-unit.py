#!/usr/bin/env python3
"""
receiver.py 的 /jobs 相关单元测试（进程内，快）。

用 importlib 直接从文件路径加载模块（文件名带连字符，不能普通 import）。
这里测的是**不依赖上游**的部分：ID3 剥离、退化判据边界、job_id 校验、
提交校验、队列上限、TTL 回收、重启恢复。

依赖上游的部分（逐块合成、取消、Range）由 tools/test-voice-jobs.py 端到端覆盖。
"""

import importlib.util
import argparse
import json
import os
import shutil
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RECEIVER = os.path.join(HERE, "internal", "services", "voice-receiver.py")


def load_module():
    spec = importlib.util.spec_from_file_location("voice_receiver", RECEIVER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class C:
    def __init__(self):
        self.fails = []

    def ok(self, label, cond, extra=""):
        print(("  ✓ " if cond else "  ✗ ") + label + ("" if cond else "  " + str(extra)))
        if not cond:
            self.fails.append(label)


def id3_tag(n):
    """造一个长度为 n（含 10 字节头）的 ID3v2 标签。"""
    body = b"\x00" * max(0, n - 10)
    size = len(body)
    ss = bytes([(size >> 21) & 0x7F, (size >> 14) & 0x7F, (size >> 7) & 0x7F, size & 0x7F])
    return b"ID3\x04\x00\x00" + ss + body


def main():
    m = load_module()
    c = C()

    print("\n【strip_id3】")
    frames = b"\xff\xfb\x90\x00" + b"A" * 500
    c.ok("标签+帧 → 只留帧", m.strip_id3(id3_tag(64) + frames) == frames)
    c.ok("无标签 → 原样返回", m.strip_id3(frames) == frames)
    c.ok("空输入 → 空", m.strip_id3(b"") == b"")
    c.ok("ID3v1（末尾 128 字节）被去掉",
         m.strip_id3(frames + b"TAG" + b"\x00" * 125) == frames)
    c.ok("只有标签没有帧 → 空（而不是抛异常）", m.strip_id3(id3_tag(64)) == b"")
    c.ok("标签长度超过文件 → 空（防御损坏文件）", m.strip_id3(b"ID3\x04\x00\x00" + b"\x7f\x7f\x7f\x7f" + b"x") == b"")

    print("\n【is_degenerate：边界必须与网站 PHP 的浮点判据一致】")
    # max_tokens=400 → cap = 32s，判据线 = 30.4s → 字节数 30.4*16000 = 486400
    c.ok("正好在判据线上 → 退化", m.is_degenerate(486400, 400) is True)
    c.ok("刚好差 1 字节 → 不退化", m.is_degenerate(486399, 400) is False)
    c.ok("远小于上限 → 不退化", m.is_degenerate(102400, 400) is False)
    c.ok("超过上限（不可能，但别崩）→ 退化", m.is_degenerate(600000, 400) is True)
    c.ok("max_tokens=0 → 不退化（避免除零式误判）", m.is_degenerate(10, 0) is False)
    # 与 PHP 的写法对照：est >= cap*0.95。
    # 注意不要断言"恰好等于判据线"—— 那一处有 IEEE754 舍入噪声
    # （1200*0.08*0.95*16000 算出来是 1459200.0000000005），两侧都可能落。
    # 有意义的是"线的两侧"：略微超过 → 退化；略微不足 → 不退化。
    for mt in (400, 1200, 2454):
        cap = mt * 0.08
        edge = int(cap * 0.95 * 16000)
        c.ok("max_tokens=%d 略超判据线 → 退化" % mt, m.is_degenerate(edge + 2, mt) is True)
        c.ok("max_tokens=%d 略低于判据线 → 不退化" % mt, m.is_degenerate(edge - 2, mt) is False)

    print("\n【valid_job_id：job_id 会拼进路径，必须严格】")
    good = ["j-1789317000-a1b2c3", "j-123456-abcdef"]
    bad = ["", "..", "../../etc/passwd", "j-1-a", "j-123456-ZZZZZZ",
           "j-123456-abcdef/../x", "x-123456-abcdef", None]
    for g in good:
        c.ok("接受 %s" % g, m.valid_job_id(g) is True)
    for b in bad:
        c.ok("拒绝 %r" % (b,), m.valid_job_id(b) is False)

    print("\n【JobManager 提交校验】")
    work = tempfile.mkdtemp(prefix="zpjobunit-")
    jobs = os.path.join(work, "jobs")
    samples = os.path.join(work, "samples")
    os.makedirs(samples, exist_ok=True)
    ref = os.path.join(samples, "ref.wav")
    with open(ref, "wb") as fh:
        fh.write(b"RIFF")

    mgr = m.JobManager(jobs, "http://127.0.0.1:1")   # 上游故意不可达：不跑 worker

    # v1.5.0：submit 的 source 解析要用全局 ARGS（脚本里由 argparse 填），
    # 单元测试必须自己指向临时目录，否则会去读**真实的** ~/tts/voice-samples。
    m.ARGS = argparse.Namespace(dir=samples, jobs_dir=jobs, token="", upstream="")

    def submit(payload):
        return mgr.submit(payload)

    base = {"model": "m", "voice": "vivian",
            "chunks": [{"text": "你好", "max_tokens": 400}]}

    def expect_error(label, payload, keyword):
        try:
            submit(payload)
        except m.QueueFullError:
            c.ok(label, keyword == "queue full", "queue full")
        except ValueError as exc:
            c.ok(label, keyword in str(exc), str(exc))
        else:
            c.ok(label, False, "没有报错")

    expect_error("voice 与 ref_audio 同时给被拒", {**base, "ref_audio": ref}, "exactly one")
    # v1.5.0：两个都不给 = 按 source=default 解析。**不做旧版回退**（不读根目录
    # 那份 v1.4.0 老文件），所以这里必须是明确的 400，而不是悄悄用旧文件。
    expect_error("两个都不给且 default 没有样本 → 报错（不回退旧根目录文件）",
                 {"model": "m", "chunks": base["chunks"]}, "voice sample not found")
    expect_error("缺 model 被拒", {"voice": "v", "chunks": base["chunks"]}, "model")
    expect_error("chunks 为空被拒", {**base, "chunks": []}, "chunks")
    expect_error("chunks 不是数组被拒", {**base, "chunks": "x"}, "chunks")
    expect_error("text 为空被拒",
                 {**base, "chunks": [{"text": "   ", "max_tokens": 400}]}, "empty")
    expect_error("max_tokens 缺失被拒", {**base, "chunks": [{"text": "x"}]}, "max_tokens")
    expect_error("max_tokens=0 被拒",
                 {**base, "chunks": [{"text": "x", "max_tokens": 0}]}, "max_tokens")
    expect_error("ref_audio 不存在被拒", {**base, "voice": "", "ref_audio": "/nope.wav"},
                 "ref_audio not found")
    expect_error("超过 2000 块被拒",
                 {**base, "chunks": [{"text": "x", "max_tokens": 1}] * 2001}, "too many")

    # ================= v1.5.0 =================
    print("\n【v1.5.0 sanitize_source：来源标识是目录名，不能带路径语义】")
    # 这组用例与 Go 侧 TestSanitizeVoiceSource 必须一致（两份实现，一套期望）
    c.ok("路径穿越被换成连字符（首尾连字符也去掉）",
         m.sanitize_source("../../etc/passwd") == "etc-passwd",
         m.sanitize_source("../../etc/passwd"))
    c.ok("空 / 纯符号 → default",
         m.sanitize_source("") == "default" and m.sanitize_source("///") == "default"
         and m.sanitize_source(None) == "default",
         (m.sanitize_source(""), m.sanitize_source("///"), m.sanitize_source(None)))
    c.ok("刻意不复用 safe_name：点号不保留（a.wav → a-wav）",
         m.sanitize_source("a.wav") == "a-wav", m.sanitize_source("a.wav"))
    c.ok("长度截到 64", len(m.sanitize_source("x" * 200)) == 64)
    c.ok("纯中文站点名被换成连字符后回落 default（不产生不可见目录名）",
         m.sanitize_source("我的站") == "default", m.sanitize_source("我的站"))
    c.ok("非 ASCII 段被削掉、ASCII 段保留",
         m.sanitize_source("站点-a") == "a", m.sanitize_source("站点-a"))

    print("\n【v1.5.0 sniff_audio：用内容而不是扩展名判断】")
    c.ok("RIFF → wav", m.sniff_audio(b"RIFF\x00\x00\x00\x00WAVE") == "wav")
    c.ok("ID3 → mp3", m.sniff_audio(b"ID3\x04\x00\x00\x00\x00") == "mp3")
    c.ok("裸 mp3 帧（0xFF 0xFB）→ mp3", m.sniff_audio(b"\xff\xfb\x90\x00rest") == "mp3")
    c.ok("ftyp → m4a", m.sniff_audio(b"\x00\x00\x00\x20ftypM4A ") == "m4a")
    c.ok("fLaC / OggS 认得", m.sniff_audio(b"fLaC\x00\x00") == "flac"
         and m.sniff_audio(b"OggS\x00\x02") == "ogg")
    c.ok("纯文本 → 空（会被 400 拒掉）", m.sniff_audio(b"hello world..... ") == "")
    c.ok("空输入 → 空", m.sniff_audio(b"") == "")

    print("\n【v1.5.0 /jobs 的来源解析（显式 ref_audio → <dir>/<source>/ref.wav → 老根目录）】")
    src_dir = os.path.join(samples, "site-a")
    os.makedirs(src_dir, exist_ok=True)
    src_ref = os.path.join(src_dir, "ref.wav")
    with open(src_ref, "wb") as fh:
        fh.write(b"RIFFsite-a")

    ja = submit({"model": "m", "source": "site-a", "chunks": base["chunks"]})
    c.ok("只给 source → 用 <dir>/<source>/ref.wav", ja["ref_audio"] == src_ref,
         ja.get("ref_audio"))
    c.ok("source 记进 job（面板的「最后使用」靠它）", ja["source"] == "site-a", ja.get("source"))

    explicit = submit({"model": "m", "source": "site-a", "ref_audio": ref,
                       "chunks": base["chunks"]})
    c.ok("显式 ref_audio 优先于 source", explicit["ref_audio"] == ref, explicit.get("ref_audio"))

    expect_error("指定了没有样本的来源被拒（错误里要有查找路径）",
                 {"model": "m", "source": "site-zzz", "chunks": base["chunks"]},
                 "voice sample not found")

    c.ok("source 里的脏字符在路径解析前就被洗掉",
         m.source_ref_path("../../etc") == os.path.join(samples, "etc", "ref.wav"),
         m.source_ref_path("../../etc"))

    print("\n【幂等与队列上限】")
    j1 = submit({**base, "client_id": "c1"})
    j2 = submit({**base, "client_id": "c1"})
    c.ok("同一 client_id 返回同一条", j1["job_id"] == j2["job_id"], (j1["job_id"], j2["job_id"]))
    c.ok("文件已落盘", os.path.exists(mgr.json_path(j1["job_id"])))
    c.ok("块目录已创建", os.path.isdir(mgr.chunk_dir(j1["job_id"])))

    old_max = m.JOB_QUEUE_MAX
    m.JOB_QUEUE_MAX = 1
    try:
        submit({**base, "client_id": "c2"})
        c.ok("超过队列上限抛 QueueFullError", False, "没有抛")
    except m.QueueFullError:
        c.ok("超过队列上限抛 QueueFullError", True)
    finally:
        m.JOB_QUEUE_MAX = old_max

    print("\n【重启恢复：running → queued，且保留 done】")
    job = mgr.load(j1["job_id"])
    job["status"] = m.JOB_RUNNING
    job["done"] = 1
    job["chunk_bytes"] = [123]
    mgr.save(job)
    resumed = mgr.recover()
    again = mgr.load(j1["job_id"])
    c.ok("running 被改回 queued", again["status"] == m.JOB_QUEUED, again["status"])
    c.ok("已完成的块数保留", again["done"] == 1 and again["chunk_bytes"] == [123], again)
    c.ok("recover 返回恢复条数", resumed >= 1, resumed)

    print("\n【TTL 回收：只清旧的终态任务】")
    fresh = submit({**base, "client_id": "fresh"})
    fresh_job = mgr.load(fresh["job_id"])
    fresh_job["status"] = m.JOB_READY
    mgr.save(fresh_job)

    old = submit({**base, "client_id": "old"})
    old_job = mgr.load(old["job_id"])
    old_job["status"] = m.JOB_READY
    # 直接写文件把它"变旧"：不能先 mgr.save() —— save() 会把 updated 刷成当前时间
    # （第一版就是这么写的，于是 TTL 永远判定为"还新鲜"，测试失败）
    old_job["updated"] = int(time.time()) - m.JOB_TTL_SECONDS - 60
    with open(mgr.json_path(old["job_id"]), "w") as fh:
        json.dump(old_job, fh)

    mgr.cleanup_ttl()
    c.ok("超过 TTL 的 ready 被清掉", mgr.load(old["job_id"]) is None)
    c.ok("其产物目录也被删掉", not os.path.isdir(mgr.chunk_dir(old["job_id"])))
    c.ok("新任务不受影响", mgr.load(fresh["job_id"]) is not None)

    print("\n【取消标志】")
    mgr.cancel(fresh["job_id"])
    c.ok("cancel 后 is_cancelled 为真", mgr.is_cancelled(fresh["job_id"]) is True)
    c.ok("cancel 未知 job 不抛异常", (mgr.cancel("j-1-abcdef") is None))
    mgr.cancel(fresh["job_id"])
    c.ok("重复 cancel 幂等", mgr.is_cancelled(fresh["job_id"]) is True)

    # ================= v1.4.0 =================
    print("\n【v1.4.0 normalize_sampling：缺省/部分/脏值】")
    c.ok("None → 全默认",
         m.normalize_sampling(None) == {"temperature": 0.7, "top_p": 0.9, "top_k": 40,
                                        "repetition_penalty": 1.05},
         m.normalize_sampling(None))
    got = m.normalize_sampling({"temperature": 0.3})
    c.ok("只给 temperature → 其余补默认",
         got["temperature"] == 0.3 and got["top_k"] == 40 and got["top_p"] == 0.9, got)
    got = m.normalize_sampling({"top_k": "40", "temperature": True, "top_p": None, "repetition_penalty": 1})
    c.ok("脏值不采用（字符串/bool/None 一律忽略，用默认）",
         got["top_k"] == 40 and got["temperature"] == 0.7 and got["top_p"] == 0.9
         and got["repetition_penalty"] == 1.0, got)
    got = m.normalize_sampling({"top_k": 12.9})
    c.ok("top_k 取整", got["top_k"] == 12 and isinstance(got["top_k"], int), got)
    got = m.normalize_sampling({"temperature": 1})
    c.ok("temperature 归一成浮点", isinstance(got["temperature"], float), got)

    print("\n【v1.4.0 wav 的时长换算与退化判定（字节率 48000、先减 44 字节头）】")
    c.ok("1 秒 wav（48000 字节 + 44 头）≈ 1.0 秒",
         abs(m.audio_seconds(48044, "wav") - 1.0) < 1e-9, m.audio_seconds(48044, "wav"))
    c.ok("只有头 → 0 秒（不会因为减成负数而崩）",
         m.audio_seconds(44, "wav") == 0.0 and m.audio_seconds(10, "wav") == 0.0)
    c.ok("mp3 口径不变（16000）", m.audio_seconds(16000, "mp3") == 1.0)
    # 判据线：cap = 400*0.08 = 32 秒；wav 需要 32*0.95*48000 + 44 字节
    edge_wav = int(32 * 0.95 * 48000) + 44
    c.ok("wav 正好在判据线上 → 退化", m.is_degenerate(edge_wav, 400, "wav") is True)
    c.ok("wav 差 1 字节 → 不退化", m.is_degenerate(edge_wav - 1, 400, "wav") is False)
    # 不减去文件头的话，判据线会落在 1459200（= 30.4 秒 × 48000）；
    # 正确的逻辑在这个长度上应当判"不退化"（真实时长 30.3994 秒）。
    no_hdr_edge = int(32 * 0.95 * 48000)
    c.ok("少减 44 字节头会把「刚好差一点」的块误判成退化（锁住这个坑）",
         m.is_degenerate(no_hdr_edge, 400, "wav") is False
         and no_hdr_edge / 48000.0 >= 32 * 0.95 - 1e-9,
         (m.is_degenerate(no_hdr_edge, 400, "wav"), no_hdr_edge / 48000.0))

    print("\n【v1.4.0 merge_chunks：wav 去头拼接再补新头】")
    import wave as _wave
    d = tempfile.mkdtemp(prefix="zpwav-")
    paths = []
    pcm_all = b""
    for i, n in enumerate((1000, 1500, 500)):
        frames = bytes(((i * 7 + k) % 256) for k in range(n * 2))   # 16bit 单声道
        pcm_all += frames
        p = os.path.join(d, "%d.wav" % i)
        with _wave.open(p, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(24000)
            w.writeframes(frames)
        paths.append(p)
    merged = m.merge_chunks(paths, "wav")
    mp = os.path.join(d, "merged.wav")
    with open(mp, "wb") as fh:
        fh.write(merged)
    with _wave.open(mp, "rb") as w:
        c.ok("拼出来的 wav 能被标准库读（头合法）", True)
        c.ok("采样率/位深/声道保持 24000/2/1",
             (w.getframerate(), w.getsampwidth(), w.getnchannels()) == (24000, 2, 1),
             (w.getframerate(), w.getsampwidth(), w.getnchannels()))
        c.ok("帧数是各块之和（%d）" % (3000), w.getnframes() == 3000, w.getnframes())
        c.ok("纯音频数据逐字节等于各块相加", w.readframes(3000) == pcm_all)
    c.ok("只保留一个文件头（不是简单相接）",
         merged.count(b"RIFF") == 1 and len(merged) == 44 + len(pcm_all), len(merged))
    c.ok("mp3 路径仍是直接相接",
         m.merge_chunks([paths[0], paths[1]], "mp3")
         == open(paths[0], "rb").read() + open(paths[1], "rb").read())

    # 子块按偶数字节对齐：塞一个奇数长度的 LIST 块，解析不能错位
    odd = bytearray()
    with open(paths[0], "rb") as fh:
        raw = fh.read()
    fmt_body = raw[20:20 + int.from_bytes(raw[16:20], "little")]
    pcm0 = raw[44:]
    odd += b"RIFF" + b"\x00\x00\x00\x00" + b"WAVE"
    odd += b"fmt " + len(fmt_body).to_bytes(4, "little") + fmt_body
    odd += b"LIST" + (5).to_bytes(4, "little") + b"abcde" + b"\x00"      # 奇数长度 + 填充字节
    odd += b"data" + len(pcm0).to_bytes(4, "little") + pcm0
    odd_path = os.path.join(d, "odd.wav")
    with open(odd_path, "wb") as fh:
        fh.write(bytes(odd))
    merged_odd = m.merge_chunks([odd_path], "wav")
    with open(os.path.join(d, "odd-out.wav"), "wb") as fh:
        fh.write(merged_odd)
    with _wave.open(os.path.join(d, "odd-out.wav"), "rb") as w:
        c.ok("奇数长度子块后的 data 仍能正确解析（按偶数对齐）",
             w.getnframes() == 1000 and w.readframes(1000) == pcm0, w.getnframes())
    shutil.rmtree(d, ignore_errors=True)

    shutil.rmtree(work, ignore_errors=True)

    print("\n" + ("全部通过 ✅" if not c.fails else "存在失败项 ❌ %s" % c.fails))
    return 0 if not c.fails else 1


if __name__ == "__main__":
    sys.exit(main())
