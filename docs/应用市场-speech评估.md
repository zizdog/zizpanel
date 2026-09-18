# 应用市场选型：「macOS 语音合成（say）」为什么不用第三方 macos-speech-server

> 2026-09-25 用户要求："**评估 macos-speech-server 加入应用市场。并写好 webui。**"
> 这份文档是那次评估的**事实底稿**（含引用与实测），结论落在
> `internal/services/catalog.go` 的 `macspeech` 条目上。
> 结论一句话：**引擎用 macOS 自带的 `/usr/bin/say`**；第三方项目作为
> "什么条件下才选它"的备选写清在下面。

## 一、候选事实（都在 2026-09-25 核对过，只读，未安装任何一件）

| 候选 | 语言/运行时 | 许可证 | darwin-arm64 原生产物 | 安装 / 卸载 | 离线 | 结论 |
|---|---|---|---|---|---|---|
| **`/usr/bin/say`（Apple 自带）** | 系统二进制（Mach-O universal，含 arm64e） | 随 macOS（系统组件） | ✅ 本来就装好了（0 字节下载） | **零安装**；单条 `plist` 注册面板托管的界面即可，卸载=摘服务 | ✅ 本机 Speech Synthesis Manager 合成，不联网 | **选它** |
| `dokterbob/macos-speech-server`（**同名项目**） | Swift 6.2 / FluidAudio（ANE） | **AGPL-3.0** | ✅ 有瓶，但只有 `arm64_sequoia`（无 tahoe；macOS 14 只能源码编译） | `brew install dokterbob/macos-speech-server/...`（**第三方 tap**）+ `brew services start`；卸载 README 没写（结构上是 `brew services stop` + `brew uninstall`） | ⚠️ 引擎在本机，但**首次启动下载 ~700MB 模型**（QWEN3 f32 到 ~1.75GB） | 不选（见下） |
| `alvinj/MacSpeechServer` | Scala 2.10 / JVM + Akka | GPL-3.0 | ❌ 无（只有一个 JVM jar；sbt 自建） | 无安装方式（README 是 sbt 构建脚本）；卸载未文档化 | 引擎是 AppleScript `say`（本地） | 不选（JVM + 无 arm64 产物 + 2015 年后无提交） |
| `easychen/miniaiapi` | **Node.js 18+**（Express）+ Python(MLX) + ffmpeg | 声明 MIT，但**仓库里没有 LICENSE 文件**（GitHub 判定 null） | ❌ 无 release / 不在 npm / 无 Dockerfile | `git clone` + `npm install` + `pip install mlx-*` + LM Studio + Draw Things；卸载未文档化 | `say` 那条路离线，模型 TTS/STT 要下 HF 权重 | 不选（引入 Node 运行时；不是"一个可安装的包"） |
| Homebrew `speech`（`soniqo/speech-swift`） | Swift / MLX + CoreML | Apache-2.0 | ✅ **homebrew-core**（无需 tap），瓶只有 `arm64_tahoe` / `arm64_sequoia`；另有 release 资产 `speech-macos-arm64.tar.gz`（v0.0.27 ≈ 98MB） | `brew install speech` / `brew uninstall speech`（**没有** `brew services`，要自己拉起） | 引擎本地；模型权重首次使用从 HF 下载到 `~/Library/Caches/qwen3-speech/` | **备选（升级路径）**：要更好音质时再上，见下 |

补充事实：
- 目录里**没有**任何名为 `macos-speech-server` 的完全同名项目以外的候选；
  `macos-speech-server` 这个仓库名确实存在（dokterbob 那个），且是唯一同名项。
- `say` 的语音清单：本机 `say -v '?'` = **177 个音色**，其中中文（zh_CN/zh_TW/zh_HK）**19 个**。
- 用户要的 OpenAI 兼容 `POST /v1/audio/speech`：`say` 路线由面板自己实现；
  `dokterbob` 项目自带；`speech-swift` 的 `speech-server` 只文档化了
  `/v1/realtime` 与 `/v1/audio/transcriptions`（**没有** `/v1/audio/speech`），
  TTS 只能走 `speech speak` CLI / Swift API。

## 二、为什么选 `say`（对照 AGENTS 铁律 10）

1. **"能原生就原生"**：`say` 不是"第三方原生"，它是**操作系统的一部分** ——
   比 Homebrew formula 更原生：零下载、零依赖、随系统更新。
2. **不许引入运行时**：Docker（amd64/Rosetta）本就禁止；Node（miniaiapi）、
   JVM（MacSpeechServer）都等于给"合成一句话"装一整套运行时。
3. **必须有可用的安装/卸载路径**：面板给的安装是"复核引擎 + 注册面板托管的
   界面服务"（`com.zizdog.macosspeech`，系统级 launchd），卸载就是摘服务；
   引擎本身是系统组件，**不该也不能**由面板删（确认框里逐字写明）。
4. **离线可行**：全程不联网。这也是它相对 Qwen3 TTS（2.9GB 权重 + Python + ffmpeg）
   与 dokterbob（首启 ~700MB 模型）最大的区别。
5. **与已有能力不重叠**：Qwen3 TTS 是"音色克隆 / 高质量模型"路线，
   `say` 是"装了就能出声"的轻量路线（网页朗读、提示音、通知播报）。

### 什么条件下才该换成别的
- 要**音质明显更好**又不克隆：`brew install speech`（homebrew-core，Apache-2.0，
  arm64 瓶；代价是首用下模型、macOS ≥15）。届时新增一个条目即可 —— 与 `say`
  条目**并存**（一个走"秒级/离线"，一个走"高质量/要模型"）。
- 要**语音识别（STT）**或 Home Assistant 的 Wyoming 语音管线：
  那就不是这个条目的事，应看 STT 侧的选型（另有条目/评估）。
- 只有当用户明确要 `dokterbob/macos-speech-server` 的 **STT+TTS+Wyoming 一体化**
  且接受：第三方 tap、AGPL-3.0、首启下 ~700MB 模型、系统级守护进程看不到
  增强/高级音色（其 README 明确写了这条限制）—— 才考虑单独上架它。

## 三、实现要点（踩过的坑，都在代码注释里）

| 坑 | 现象 | 处理 |
|---|---|---|
| `say` 对**未知音色**静默成功 | 退出码 0，**不产出文件** → 会把 0 字节当成功 | 请求先对 `say -v '?'` 清单校验音色；合成后再复核产物存在且非空 |
| `say` 对**空文本**"成功" | 产出一个只有头的 4096B 文件 | 面板在请求前就拒绝空文本（400） |
| `say` **不能编码 mp3** | `say -o x.mp3` 返回 0 却写 16 字节非音频桩 | 只用 `say` 产出 AIFF；wav/m4a 走 `afconvert`；mp3 走 ffmpeg（没有就**如实报错**） |
| `afconvert` **不能编码 mp3** | `Error: ExtAudioFileSetProperty ('cfmt') failed` | 同上：mp3 是唯一需要 ffmpeg 的格式，healthz 里逐个格式报告可用性 |
| `afconvert` 的 WAV 里 `data` 不在 44 字节处 | 中间有 `fLLR` 填充块（实测 data 从 4096 开始） | 分段拼接走真正的 chunk 链解析（`parseWAVPCM`），不硬跳 44 |
| `-r` 语速不是倍率 | OpenAI 的 `speed` 是倍率；`say -r` 是词/分钟 | 换算 + 夹取 80~400，并把**实际值**通过 `X-Zizpanel-Effective-Speed` 回给调用方 |
| 服务端给的绝对路径在别名下失效 | `/v1/tasks/…` 是根绝对路径，`/speech/` 下会 404 | 前端**只用相对当前目录的路径**自己拼（真机验证时就是这样发现的） |
| 长文本假进度 | 一个转圈界面说不清在干什么 | 按句分段合成（每段 ≤300 字），SSE 报真实 `已完成 N/M 段`；>600 字走 `202 + task_id` |

## 四、引用来源

- Apple `say` man page（镜像）：https://keith.github.io/xcode-man-pages/say.1.html
- `dokterbob/macos-speech-server`：https://github.com/dokterbob/macos-speech-server
  （安装、TTS 引擎与限制、AGPL-3.0 均取自该 README）
- `alvinj/MacSpeechServer`：https://github.com/alvinj/MacSpeechServer
- `easychen/miniaiapi`：https://github.com/easychen/miniaiapi
- Homebrew `speech` 瓶与依赖：https://formulae.brew.sh/formula/speech
- `soniqo/speech-swift`：https://github.com/soniqo/speech-swift

> 本机实测（2026-09-25）：`/usr/bin/say -v '?'` → 177 音色 / 19 中文；
> 四种格式各自产出真实音频（同一段文本时长一致：aiff=wav=m4a=6.2177s，mp3=6.2955s）；
> 1200 字 → 5 段拼接 → 244.5s / 1.93MB m4a。
