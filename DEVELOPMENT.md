# ZizPanel 开发说明

> 给**改这个项目的人**：设计取舍、构建、测试/门禁、发布、已修复的坑。
> 使用者看 [`README.md`](README.md)；进度与凭据看 `ZizPanel-当前状态.md`（gitignored）；规则看 `AGENTS.md`。
> 专题文档：`docs/新增应用工作流.md`、`docs/应用市场下载点清点.md`、`docs/离线打包与迁移.md`。
> **历史过程不进本文**，只留结论与坑；**坑清单只增不减**。

---

## 一、目录地图

| 路径 | 作用 |
|---|---|
| `cmd/zizpanel` | 面板主程序（HTTP/API + 内嵌前端） |
| `cmd/zizpanel-helper` | 受限提权助手（白名单动作，sudoers 精确授权） |
| `cmd/zizpanel-assets` | 发布/镜像工具（`plan`、`audit`；`apps` 子命令兼容 `sync-nas-apps.sh`） |
| `internal/web` | API 与内嵌 ESM 前端 |
| `internal/web/assets/js/*.js` | 前端源码（无构建步骤）；`servicePanel.js` 是应用/服务**唯一**详情面板 |
| `internal/services` | 服务管理、应用市场、各安装器、镜像 |
| `internal/tasks` | 任务中心（`launchTask` → 202 + `task_id`，进度 SSE） |
| `internal/{sites,mysql,acme,tlsx,proxies,files,term,scheduler,sysconfig,sysinfo,logs,store,auth,priv,appproxy,upgrade,version}` | 各子系统 |
| `tools/` | 检查门禁、同步/打包/部署、真机验证脚本 |
| `install.sh` | 一键安装（`curl \| sudo bash`） |

---

## 二、设计取舍

| 决定 | 为什么 |
|---|---|
| Go 单二进制 + `//go:embed` 前端 | 目标机不一定有 Go/Node/Python；升级=换一个文件，无运行时依赖 |
| SQLite 单文件 + WAL | JSON 并发写/原子性不够；MySQL 等于"先装数据库才能管机器"；备份=复制文件 |
| 前端零构建（原生 ESM + 极小 `h()`） | 装完面板的机器不该有 `node_modules`，发布不依赖 `pnpm build`；代价是手写响应式更新 |
| 受限 helper，而不是通用 root shell | 面板主体是 root LaunchDaemon；网页拿通用 root shell 等于把机器交出去。Web 终端默认关闭，用登录用户 shell |
| 长任务走任务中心 | 安装动辄几分钟；同步请求挂在 `r.Context()` 上，**用户一刷新就杀掉 brew/docker** 并留下半装状态。秒级动作（如 `compose stop`）仍同步返回 |
| 应用市场：能原生就原生，否则 Docker | 原生=brew formula 或官方 darwin-arm64 产物 + launchd；Docker **必须自带 `linux/arm64`**，禁 `platform: linux/amd64`/Rosetta（测试锁死） |
| 计划任务用 launchd 而不是 crontab | crontab 在 macOS 要额外授权，且合盖/休眠不补跑；面板把 cron 翻译成 `StartCalendarInterval` 并如实报告支持哪些写法 |
| 不接管已有环境 | 只动 `/opt/zizpanel`、自己的 plist/数据/nginx include；`~/www/zizdog.cn` 是 TtsVoice 的，绝不改代码/配置/DB |

---

## 三、开发与构建

```bash
export GOPROXY=https://goproxy.cn,direct   # 本机 proxy.golang.org 不可达，必须
make dev          # 构建到 dist/
make run-local    # /tmp/zizpanel-dev 调试模式（固定后缀 dev），不碰 /opt/zizpanel
# 浏览器打开 https://127.0.0.1:18443/dev/
make check        # 提交前唯一入口（见第四节）
make test-short   # 只跑 Go 单测
```

其他常用：`make uitest`（需先 `make run-local`）、`make uitest-live`（对真实安装跑，含特权步骤）、
`make market-audit` / `market-audit-offline` / `market-audit-verify`、`make bump`。

---

## 四、测试策略与门禁

`make check` 的每一步（缺工具时**明确打印跳过**，不假装通过）：

| # | 步骤 | 拦什么 |
|---|---|---|
| 1 | `gofmt -l .` | 未格式化 |
| 2 | `bash -n` 全部脚本 | shell 语法 |
| 3 | `tools/check-shell-vars.py` | `$VAR` 后紧跟中文标点 → 多字节并入变量名，`set -u` 直接退出 |
| 4 | `shellcheck -S warning` | shell 常见错误（未装则跳过） |
| 5 | `tools/check-no-real-credentials.sh` | 真实面板口令进仓库（见下） |
| 6 | `python3 -m py_compile tools/make-manifest.py` | Python 工具语法 |
| 7 | `tools/check-js-syntax.mjs`（acorn） | 前端语法（见下） |
| 8 | `go vet ./...` | 静态检查 |
| 9 | `make test` → `tools/check-test-pollution.sh` | 真实家目录被测试污染（见下） |
| 10 | `make jobs-test` | receiver `/jobs`（假上游，不碰真实服务） |
| 11 | `make install-test` / `remote-test` / `server-mode-test` | 安装脚本沙箱、远程一键安装、服务器模式 |

三道专项门禁：

- **`tools/check-test-pollution.sh`（真实家目录指纹）**：给 `~/Library/LaunchAgents`、`~/www`、
  安装产物根拍**内容**指纹 → `go test ./... -count=1` → 再拍一次比对，`go test` 退出码原样保留。
  来历：2026-09-14 `newTestServer` 漏隔离 `UserHome`，`make check` 把真实
  `~/Library/LaunchAgents/sh.brew.*.plist` 覆盖成空 plist —— 服务在跑所以**零症状**，
  但只要重启或点一次"重启服务"就永久起不来。另有护栏测试 `TestTestServerSandboxedAwayFromRealHome`。
- **`tools/check-js-syntax.mjs`（acorn）**：`node --check` 会放过浏览器拒绝的语法，表现是整页白屏且
  **不给文件名、不给行号**；acorn 精确到行列。未装 acorn 时跳过并打印提示。
- **`tools/check-no-real-credentials.sh`（新增）**：拿 `.panel-credential.local` 里每个长度 ≥6 的值，
  去搜**将要提交的文件**（`git ls-files --cached --others --exclude-standard` = tracked + untracked 非忽略），
  命中即失败，只回显掩码。**只扫 `git ls-files` 会漏掉新增文件**。没有凭据文件时明确打印跳过。

其他硬纪律：

- **单测不许碰真实服务、生产配置、用户真实家目录**：本机 8880 是 TtsVoice 的 Qwen（用
  `qwenPortOverride` 隔离）；nginx vhost 测试必须沙箱化并断言生产 `000-default.conf` 一字未变。
- **退出码不能过管道**：`go build ... | head`、`make check | tail` 都会掩盖失败。
  固定写法 `make check > /tmp/check.log 2>&1; echo "EXIT=$?"; tail -40 /tmp/check.log`。
- **前端"哪个按钮出现"必须用真模块端到端验**：`node tools/appdetail-verify.mjs`
  （Playwright + 假 fetch，加载真实 `apps.js`/`services.js`/`servicePanel.js`，对比两处按钮清单）。
  它当场抓到过静态检查抓不到的 bug（市场条目 `name` 是展示名）——**语法检查永远是绿的**。
- **手工调面板 API**：基址必须带面板后缀；写接口带 `X-CSRF-Token`（值取可读的 `zp_csrf` cookie），
  否则 403「CSRF 校验失败」；`GET /api/v1/services` 的键是 `list`。

---

## 五、发布与升级（现状：只在本机，不发 GitHub）

仓库只在本机，**不配远程、不 push、不发 GitHub Releases**。发布链路：

```bash
make bump            # 0.11.3 → 0.11.4 … → 0.11.10 → 0.12.0
make check           # 必须真绿（第四节）
make release         # dist/release/：darwin/arm64+amd64 包、签名清单
make mirror-nas      # 生成"指向 NAS 镜像"的清单（url=download/<版本>/）并签名
make deploy          # release + 推 NAS(单流 tar) + 并行升级两台 + 验证版本
# 分步亦可：make publish-nas（仍要求 NAS_PASS）/ bash tools/deploy.sh（密钥优先、口令兜底）
```

- `make release` 会把发布公钥注入二进制，并**运行产物核对公钥**：`-X` 在 `-trimpath` 下会静默失效
  （符号被死代码消除，退出码仍是 0）。
- 私钥在 `.release-key/`（gitignored）。丢了就再也签不出被已装面板接受的升级包。
- **Makefile 与 `install.sh` 里仍有 GitHub 默认值，但不属于发布链路**：
  `RELEASE_BASE_URL ?= https://github.com/zizdog/zizpanel/releases/download/$(VERSION)`；
  `install.sh` 的 `ZIZPANEL_DOWNLOAD_BASE` 默认也指向 `https://github.com/zizdog/zizpanel/releases`，
  启动后回落内置镜像 `https://zizdog.com/zizpanel`。`make release` 还会多产一份 `manifest-github.json`
  （同一批包、同一把私钥，只有 url 不同）。**历史入口已废弃**（不发 GitHub，那里没有新版本）；
  实际下发走 `make mirror-nas` 的 NAS 版清单与内置镜像。
- 面板"在线升级"读 `manifest.json` + `manifest.json.sig`；升级后**必须等新版本健康检查回来**
  （看版本号，不是端口通）。
- 国内网络实测：GitHub 直连 20 秒 0 字节；NAS 局域网 ~87 MB/s、公网镜像 ~8.6 MB/s。
  CLT 整包、brew 瓶、pip、HF、Docker 都走镜像。

---

## 六、应用市场、离线与镜像

- 加一个应用 = 5 步：**`docs/新增应用工作流.md`**（声明 → 静态门禁 → 在线审计 → 真机验收）。
- 离线/断网/迁移：**`docs/离线打包与迁移.md`**（`/offline/` 包格式 + `tools/build-offline-bundle.sh`
  + 面板「仅走 NAS」开关）。
- 每个应用从哪下、多快、超时多少：**`docs/应用市场下载点清点.md`**。
- 镜像布局：`/apps/<id>/<ver>/`、`/models/`、`/sites/`、`/brew/`、`/pypi/`、`/hf/`、`/docker/`、
  `/zizpanel/clt/`、`/offline/`。
- 两条不变量：**能原生就原生**；**Docker 必须自带 arm64**（`platform:` 由测试扫目录条目锁死）。

---

## 七、系统设置（把 macOS 配成服务器）

「系统设置」页把需要终端的事做成开关：合盖不睡、断电自恢复、关 Spotlight 索引、调 TCP 参数、开远程登录等。
底层命令在 `tools/system-services.sh`、`tools/server-mode.sh`：每一步都有"做了 / 跳过 / 失败"三态，
`make smoke` 覆盖（特权步骤跳过并单独列出）。参考命令：`pmset -a sleep 0 disksleep 0 displaysleep 0` /
`womp 1` / `autorestart 1` / `powernap 0`；`mdutil -a -i off`；`defaults write .../com.apple.SoftwareUpdate ...`。

**每一项都先探测"这台机器支不支持"**，不支持就如实报"不支持"。最危险的一次：`pmset -a autorestart 1`
在不支持的机器上**静默返回成功**却什么都没做，脚本却报告"已开启断电自恢复" —— 真跳闸那天才会发现。
**能谎报成功的功能，比没做更糟。**

---

## 八、环境（2026-09-17 起：**单机**）

| | 本机 MacBook Air M4（**唯一开发 + 测试机**） | Mac mini M4（**生产环境**） |
|---|---|---|
| 面板 | `https://127.0.0.1:8443/<后缀>` | **不记录、不访问**（用户自己在用） |
| 可破坏性操作 | **否，绝不重启**（DSH 会话跑在上面） | **禁止任何操作** |
| SSH | — | **禁止**（本机已删除它的主机记录与全部凭据） |

> **为什么从双机变单机**：1.0.0 起 mini 是用户的生产环境，AI 不许接触（用户 2026-09-17 明确要求）。
> `tools/deploy.sh` 的 mini 那一腿已删除、`Mac-mini部署指南.md` 已删除、原来的
> "需要重启/断网/断电恢复才能验证"这一类测试**从此没有真机可做** —— 只能近似验证或**如实标注"未验证"**。
> 教训见坑 151。

非交互 SSH 里 `docker`/`colima` **不在 PATH**（用 `/opt/homebrew/bin/...`）。
破坏性操作前先快照，并先 `git diff` 看清将要发生什么。

---

### 发布提速（2026-09-17，用户："每次发布新版都太耗时了"）

实测三段各自的耗时，以及现在的做法：

| 段 | 之前 | 现在 | 做法 |
|---|---|---|---|
| `make check` | 5–7 min（每次都跑） | 同一棵树**跳过** | `tools/check-stamp.sh`：check 成功时把**工作树指纹**（含未跟踪文件内容）写进 `dist/.check-stamp`；`make deploy` 先 `verify`，指纹一致才跳过并打印"哪个版本、什么时候跑的" |
| `make release` | 双架构，16s | **只 arm64**，8s | `ARCHS`（默认 `arm64 amd64`，`make deploy` 传 `arm64`）—— 本机与 mini 都是 Apple Silicon |
| 上传 NAS | 4 个包 ≈96MB | **1 个包 23MB** | 只传版本包；`latest` 与 `download/<版本>/` 的副本/软链由远端 LAYOUT 造 |
| 升级等待 | 每轮 sleep 2s | 前 20 轮 0.5s，之后 2s | `tools/panel-upgrade.py`；总窗口不变（≈3 min） |

**实测：一次 `make deploy` 从 ~1.5–2 min 降到 17s**（含构建 8s、上传 23MB、两台并行升级 4s、版本核对）。
⚠️ 两条纪律不能忘：① `ARCHS=arm64` 只适合"本机 + mini"这种全 arm64 部署，**正式发布仍建议双架构**（`make release` 默认就是）；
② check 标记是**便利**不是安全边界 —— 手动 `bash tools/check-stamp.sh write` 等价于 `SKIP_CHECK=1`，只在你自己确认过的情况下用。

## 九、已修复的典型坑

**清单已移到 `docs/坑清单.md`**（只增不减，当前 177 条）。那里记录了每个坑的现象、根因、
修法与对应门禁；本节只留指针，避免这份文档超出 450 行预算、把上下文挤爆。
新踩到坑时：先追加到 `docs/坑清单.md`；如果它会**伤到使用者**（安装卡住 / 谎报成功 /
破坏用户环境），再摘要一句进 `AGENTS.md` 第三节。
