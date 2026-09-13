#!/usr/bin/env python3
"""
receiver.py 的 /jobs 相关单元测试（进程内，快）。

用 importlib 直接从文件路径加载模块（文件名带连字符，不能普通 import）。
这里测的是**不依赖上游**的部分：ID3 剥离、退化判据边界、job_id 校验、
提交校验、队列上限、TTL 回收、重启恢复。

依赖上游的部分（逐块合成、取消、Range）由 tools/test-voice-jobs.py 端到端覆盖。
"""

import importlib.util
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
    expect_error("两个都不给被拒", {"model": "m", "chunks": base["chunks"]}, "exactly one")
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

    shutil.rmtree(work, ignore_errors=True)

    print("\n" + ("全部通过 ✅" if not c.fails else "存在失败项 ❌ %s" % c.fails))
    return 0 if not c.fails else 1


if __name__ == "__main__":
    sys.exit(main())
