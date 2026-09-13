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
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

VERSION = "1.2.0"

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

ARGS = None


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

        if path in ("/voice", "/voice/upload"):
            return self._receive_voice()

        if path.startswith("/v1"):
            return self._proxy()

        return self._fail(404, "接口不存在：%s" % self.path)

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
    global ARGS

    parser = argparse.ArgumentParser(description="TtsVoice 接收端 + 带鉴权的 TTS 代理")
    parser.add_argument("--dir", default=os.path.expanduser("~/tts/voice-samples"),
                        help="音色样本存放目录（默认 ~/tts/voice-samples）")
    parser.add_argument("--host", default="0.0.0.0",
                        help="监听地址（默认 0.0.0.0）")
    parser.add_argument("--port", type=int, default=8899, help="监听端口（默认 8899）")
    parser.add_argument("--token", default="", help="共享密钥，强烈建议设置")
    parser.add_argument("--upstream", default="http://127.0.0.1:8880",
                        help="上游 TTS 服务地址（默认 http://127.0.0.1:8880）")
    ARGS = parser.parse_args()

    os.makedirs(ARGS.dir, exist_ok=True)

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
