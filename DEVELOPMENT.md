# ZizPanel 开发说明

> 给**改这个项目的人**：设计取舍、构建、测试/门禁、发布、已修复的坑。
> 使用者看 [`README.md`](README.md)；进度与凭据看 `ZizPanel-当前状态.md`（gitignored）；规则看 `AGENTS.md`。
> 专题文档：`docs/新增应用工作流.md`、`docs/应用市场下载点清点.md`、`docs/离线打包与迁移.md`、`docs/磁盘工具.md`。
> **历史过程不进本文**，只留结论与坑；坑清单见 `docs/坑清单.md`（可删过时条目，判据见 `AGENTS.md` 第六节）。

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

其他常用：`make market-audit` / `market-audit-offline` / `market-audit-verify`、`make bump`。
zizvideo 是**独立项目**（本机 `../zizvideo`，GitHub `zizdog/zizvideo`）：它自己的
`make check / release / publish` 都在那边跑；本仓库只用它的产物（读镜像索引 `apps/zizvideo/manifest.json`）
——见 `AGENTS.md` 的「zizvideo 是独立项目」。

---

## 四、测试策略与门禁

- **`make check`（日常，约 1 分钟）**：版本号一致性 → `gofmt` → `bash -n` 全部脚本 →
  `check-shell-vars` → `shellcheck` → `check-no-real-credentials` → `check-future-dates` →
  Python 语法 → acorn 两项（语法 + 未声明赋值）→ `go vet` → `go test ./...`（含真实家目录指纹门禁）→
  卸载脚本三档沙箱 → 服务器模式（SSH/电源）→ 写 check 指纹。
- **`make check-full`（发版前）**：在 `check` 之上加真起进程的重活 —— receiver `/jobs`、
  安装脚本端到端、远程一键安装、发布说明门禁。这些一次几分钟，日常提交不跑。
- 退出码**不许过管道**：`go build ... | head`、`make check | tail` 都会掩盖失败。
  固定写法：`make check > /tmp/check.log 2>&1; echo "EXIT=$?"`。
- 为什么留这几道（历史事故）：`check-js-syntax.mjs`（acorn）—— `node --check` 放过浏览器拒绝的语法 =
  整页白屏且不给行号；`check-test-pollution.sh` —— 2026-09-14 测试把真实
  `~/Library/LaunchAgents/*.plist` 覆盖成空文件，服务在跑所以零症状、重启后永久起不来；
  `check-no-real-credentials.sh` —— 真实口令进仓库复发过两次。
  **这几条删了会出事的清单在 `AGENTS.md` 第三节**，其余纪律细节见 `docs/坑清单.md`。
- 前端接线只靠一条运行体门禁（`internal/web/frontend_assets_gate_test.go`：真 ES 解析 +
  模块引用存在 + appKeyOf 稳定键），不再堆"读源码找字符串"的静态断言。

## 五、发布与升级（现状：只在本机，不发 GitHub）

仓库只在本机，**不配远程、不 push、不发 GitHub Releases**。发布链路：

```bash
make publish         # ⚡发版就这一条（用户 2026-09-22 定的顺序）：
#   门禁（同一棵树跑过 ⇒ 按指纹跳过）→ 工作树必须干净 → bump + 只提交版本文件 →
#   构建 → **先发镜像站**（成功即报"发布完成"，用户当场可以升级测试）→ zizdog.com **后台**补推
#   想省门禁（确认过这棵树是绿的）：SKIP_CHECK=1 make publish
make bump            # 只在用户同意发版时；patch 到 10 进位（见 AGENTS 第二节）
make check           # 必须真绿（第四节）
make release         # dist/release/：darwin/arm64+amd64 包、签名清单
make mirror-public   # 生成"指向公网镜像"的清单（url=download/<版本>/）并签名，回读断言无内网地址
make deploy          # release + 推镜像机 + 升级本机 + 验证（不发公网，用于只更新本机）
# 分步亦可：bash tools/publish-release.sh build|push-mirror|push-zizdog|verify
#   push-mirror = 直传 mini 面板文件接口（实测 5.9 MB/s，53MB≈9s；latest 走服务端复制）
#   push-zizdog = 只传版本包（latest 在源站本地复制）+ 两架构并行；verify 在源站算 sha256（零公网带宽）
```

**发版耗时（2026-09-22 实测，用户要求"大幅缩短发布时间"）**：发布本身 ≈ 40s
（构建 31s + 镜像 9s）；门禁是唯一的可变项 —— 同一棵树按指纹跳过，改过代码时
`make check` 130s（go 测试缓存 + 重包拆进程分片）＋ `check-full` 的端到端 ~130s。
历史对照：改造前一次发版 ≈ 9.5 分钟（门禁 456s、网络三条整包传输 355s、编译 31s）。

- **发布产物只允许公网地址**：`make mirror-public` 的基址直接取自
  `internal/upgrade/source.go` 的 `MirrorSource`（`https://mirror.zizdog.com:8888/zizpanel`），
  不写第二份；生成后 `grep` 回读，出现任何 RFC1918 地址就失败。
  **地址由开发者提供**：镜像机的主机/账号/目录在 Makefile 与 `tools/` 里都没有默认值，
  必须用环境变量传（旧的 `make mirror-nas` 保留为 `mirror-public` 的别名）。
- `make release` 会把发布公钥注入二进制，并**运行产物核对公钥**：`-X` 在 `-trimpath` 下会静默失效
  （符号被死代码消除，退出码仍是 0）。
- 私钥在 `.release-key/`（gitignored）。丢了就再也签不出被已装面板接受的升级包。
- **Makefile 与 `install.sh` 里仍有 GitHub 默认值，但不属于发布链路**：
  `RELEASE_BASE_URL ?= https://github.com/zizdog/zizpanel/releases/download/$(VERSION)`；
  `install.sh` 的 `ZIZPANEL_DOWNLOAD_BASE` 默认为空，GitHub 默认值在 `ZIZPANEL_GITHUB_BASE` /
  `ZIZPANEL_GITHUB_RELEASE_BASE`，启动后回落内置镜像 `https://zizdog.com/zizpanel`。`make release` 还会多产一份 `manifest-github.json`
  （同一批包、同一把私钥，只有 url 不同）。**历史入口已废弃**（不发 GitHub，那里没有新版本）；
  实际下发走 `make mirror-public` 的公网镜像版清单与内置镜像。
- 面板"在线升级"读 `manifest.json` + `manifest.json.sig`；升级后**必须等新版本健康检查回来**
  （看版本号，不是端口通）。
- 国内网络实测（历史，未复测）：GitHub 直连 20 秒 0 字节；公网镜像 ~8.6 MB/s。
  CLT 整包、brew 瓶、pip、HF、Docker 都走镜像。

---

## 五点五、用表新增应用：声明式配置补丁

表原来只能描述"装完不用改任何配置"的应用。需要改一行配置的（换监听端口/地址）过去只能
写一个面板内建补丁 —— 那就退回"一个一个造轮子"。现在表里有 `config.set`：

- 声明写法见 `docs/插件规范.md`；引擎是 `internal/plugins/patch.go`（纯函数 `ApplyPatch`）。
- **只换值**：缩进、`=`/`:` 的写法、行尾注释、其它键与注释一律原样保留（grafana.ini 整篇
  是注释掉的默认值，round-trip 解析会把它们吃掉 —— 那是面板偷偷重写用户配置）。
- 安装路径在 `brew services start` **之前**应用（`services.applyConfigPatchStep`）：服务起来
  就是对的配置；改动前留 `<file>.zizpanel.bak`（**只留第一次的原文**，反复安装不会把它覆盖成
  改过之后的版本）；面板是 root 时写完把归属交还真实用户（AGENTS 第三节第 8 条）。
- **失败不谎报**：读不到/写不进 → `Warning` 里写清"配置补丁没生效 + 原因"；文件还不存在且
  没声明 `if_missing=create` → 如实记"跳过（该应用通常是首次启动才生成配置）"。
- 门禁：`internal/plugins` 的补丁用例（kv/ini/yaml、前缀不误伤、section 隔离、幂等、缩进与
  注释保留、注入拒绝、PlanText 必须写出补丁）+ `internal/services` 的接线用例（写入/备份/幂等/
  跳过/写失败进 Warning/声明→App 的值是拷贝）。
- 随机口令（`config.patches[].secrets`）：值由面板生成、写进配置、只在安装结果的凭据区出现一次；
  **已有非空值一律复用**（重装换口令 = 把应用弄坏），写盘失败时凭据区清空（不给用户一个假口令）。
- 一个文件要改多个 section 时用 `config.patches[]`（couchdb 那种形态：既要 `[admins]` 口令、
  又要 `[chttpd]` 监听地址）；多条补丁**只在最后落一次盘**（一次备份、一次写入）。
- **两个"摆设字段"的处置（2026-09-24）**：
  · `config.seed`（从模板生成配置）**没有执行通路** ⇒ 校验阶段**如实拒绝**它（旧的计划文本还
    写着"写入配置（模板 …）"，那是谎报），并加一条门禁盯着"带了 seed 的声明必须被拒"；
  · `run.hooks` 明确为**依赖声明**（插件说"这个应用要靠面板的某个内建补丁"），并加两条门禁：
    白名单里的每个名字都必须有实现登记（`services/hooks.go`）、登记簿里不许有白名单之外的名字、
    登记的应用必须在目录里 —— 谁加了名字没实现、或删了实现留着名字，都当场失败。
    刻意**不做**"按名字自动执行"：那四个补丁各自要改应用自己的配置格式，抽成通用动词等于
    给插件开一个"任意改配置"的口子（B0 明令禁止的方向）。

---

## 六、应用市场、离线与镜像

- 加一个应用 = 5 步：**`docs/新增应用工作流.md`**（声明 → 静态门禁 → 在线审计 → 真机验收）。
- 离线/断网/迁移：**`docs/离线打包与迁移.md`**（`/offline/` 包格式 + `tools/build-offline-bundle.sh`
  + 面板「仅走镜像站」开关）。
- 每个应用从哪下、多快、超时多少：**`docs/应用市场下载点清点.md`**。
- 镜像布局：`/apps/<id>/<ver>/`、`/models/`、`/sites/`、`/brew/`、`/pypi/`、`/hf/`、
  `/zizpanel/clt/`、`/offline/`（**没有 `/docker`**：docker 加速只用公网候选）。
- 两条不变量见 AGENTS 铁律 10（能原生就原生；Docker 必须自带 arm64，`platform:` 由测试扫目录条目锁死）。

---

## 六点五、主动通知（C3）

面板发现异常时**主动说一声**（服务异常 / 证书将到期 / 磁盘水位），而不是等用户点进来。
后端 `internal/notify`（纯规则 + 通道）＋ `internal/web/api_notify.go`（取事实、发、存设置）；
前端「面板设置 → 主动通知」。默认**关闭**，同一 key 30 分钟内只提醒一次，单轮最多 5 条（超出合并成一条汇总）。

- **判据必须与界面一致**：报的问题 = 服务页「需要处理」那一类（启动失败 / 运行时不可用 /
  健康检查没过），且**排除用户主动停掉的服务**（`stopped_by_user`）—— 两边漂移就会"界面说没事、通知说有事"。
- **通道两个**：macOS 系统通知（`osascript`）、Webhook（POST JSON）。面板是 root LaunchDaemon，
  不在用户的 Aqua 会话里，所以系统通知走 `launchctl asuser <uid>`（坑：直接 osascript 返回成功但屏幕上什么都没有）。
- **读不到 ≠ 没问题**：服务列表 / 证书 / statfs 任一部分失败都进 `errors` 并显示，绝不当成"一切正常"。
- 门禁：`go test ./internal/notify/`（冷却去重、通道失败如实返回、规则正反对照、AppleScript 转义、
  溢出合并）+ `go test ./internal/web/ -run Notify`（设置校验/落盘/生效、巡检发送链路、关闭时不发送、
  逐通道报错、GET 契约）。
- **本机实测（2026-09-23，root + `launchctl asuser 501`）**：`usernoted` 日志出现
  `Delivering … shouldDeliver: true` → `.alert .lockScreen .notificationCenter`，即通道真的投递了。
  两个坑：① 通知来源显示为**「脚本编辑器」（com.apple.ScriptEditor2）**——osascript 的固有身份，
  想显示 ZizPanel 需要 `terminal-notifier -sender`（未做）；② 退出码 0 **不能**证明投递
  （root 直接 `osascript` 也返回 0），判据只能看 `usernoted` 日志。
  当时系统处于「专注模式」（`dndEnabled: true`），所以横幅是否弹出取决于用户的专注设置。

---

## 六点六、PWA 与手机一屏（C4）

「加到手机主屏」＋一页只为手机准备的状态/操作页。前端 `assets/sw.js`、`assets/js/pwa.js`、`assets/js/mobile.js`；
后端 `internal/web/pwa.go`（manifest 与图标，见「面板设置」之外的路由 `/manifest.webmanifest`、`/pwa/icon-*.png`）。

- **manifest 由后端生成**：面板可能挂在 `/<安全后缀>/` 下，`start_url`/`scope`/图标地址都必须带前缀 ——
  写成静态文件会让"加主屏"后打开 404（带后缀部署下必现）。
- **图标用代码画**（`image/png`，192/512，圆角蓝底 + 白色 Z）：不引入二进制资源、不加构建步骤，
  改配色只改一处。`purpose: any/maskable` 两种都给。
- **Service Worker 只碰面板自己的静态外壳**（入口页/CSS/JS/图标），`/api/` 一律走网络**绝不缓存**
  （拿旧数据糊弄用户比打不开更糟），代理出去的应用界面与导航页完全不插手；缓存名带面板版本，
  就地升级后自动换 worker 与空缓存。
- 注册放在 `renderApp()`（首次初始化/登录走的是它、不重跑 `boot()`，放 boot 里会漏掉这两种路径）。
- 门禁：`go test ./internal/web/ -run 'PWA|ServiceWorker'`（后缀感知的 manifest、PNG 真解码 + 圆角透明 +
  未支持尺寸 404、sw.js 必须绕开 `/api/` 且 no-cache）；真浏览器 `tools/pwa-check.mjs` 15 项
  （iPhone 13 视口登录 → manifest/图标/SW/手机一屏）。
- **未验证：离线可用性**。SW 注册、作用域、install 阶段预热的入口页都验到了，但这个 Playwright/Chromium
  对 SW 断网行为的模拟不可靠（同一探针时而应答时而 `Failed to fetch`；`setOffline` 与 `route().abort()`
  都会在请求进入 SW 之前拒掉它），所以**不声称断网可用**。

---

## 六点七、整机搬家 / 备份恢复（C2）

换机器的路径本来就是一条：**老机器 `zizpanel backup create` → 新机器装面板 → 备份页上传 → 恢复**。
引擎在 `internal/backup`（`Plan`/`Create`/`Verify`/`Extract`/`RestoreItem`/`CheckCompatibility`），
Web 接口在 `internal/web/api_backup.go`，CLI 在 `cmd/zizpanel/backup.go`。本轮做的是**端到端验收**（不改协议）：

- 范围：`panel`（config.json、panel.db 的 `VACUUM INTO` 一致性快照、tls/certs/site-certs/proxy-certs/
  proxy-auth/acme、filebrowser/compose）、`nginx`（nginx.conf + vhosts/conf.d/includes + phpMyAdmin 配置 +
  各 PHP 版本的上限片段）、`sites`（网站文件）、`mysql`（库导出）、`apps:<id>`（含密的客户端配置，默认不勾）。
- 归档带 `manifest.json`：逐文件 size+sha256、表清单、`contains_secrets`；`Verify` 整包校验；
  `CheckCompatibility` 比表结构（备份比本程序旧 → 恢复后重放迁移）。
- 恢复**先做快照**（`pre-restore-*.tar.gz`，回滚凭据），再替换数据库、落盘、重建 nginx、重放计划任务；
  结果按"真正失败（`unapplied`）/ 明确不做（`skipped`，带原因）/ 提示（`warnings`）/ `partial`"如实分类。
- **本机端到端验收（2026-09-24）**：用真实面板数据打 15 个文件/0.1 MB 的包 → `verify` 逐文件 sha256 一致 →
  恢复进**全新调试实例**（非 root）→ 任务 `succeeded`、`partial=false`、外键检查通过、14 张表 3125 行、
  2 个站点（zizdog.cn / mirror.zizdog.com）、1 条计划任务、10 条服务登记；恢复后**用来源机器的面板口令**登录
  （旧会话被清 126 条，这是对的）。
- **未覆盖/未验证**（如实）：① 站点文件与数据库内容要另行搬（面板只给清单，恢复不自动覆盖非面板数据）；
  ② 非 root 调试实例会跳过 nginx 重建与计划任务重放（真机是 root，这条在真机才成立）；
  ③ 「恢复后站点真的能访问」需要以 root 跑正式面板才验得了，本轮只验到"记录与文件回读到"。

---

## 六点八、多机管理（C5，只读聚合）

一台面板把另外几台 Mac 的状态集中在一屏。这一版**只读**：主面板定期拉子机的
版本/负载/服务计数，**不代它执行任何写操作** —— 写操作的信任模型（主面板持有什么权限、
子机怎么撤销、离线时怎么算）没定清楚之前不做，半成品的"远程控制"比没有更危险。

- 代码：`internal/web/api_peers.go`（两端都在这里）+ `assets/js/peers.js`（「多机」页）。
  子机侧 `GET /api/v1/agent/summary`（**不 requireAuth**，只认 `X-ZizPanel-Agent` 头里的只读凭证）；
  主面板侧 `GET/POST /api/v1/peers`、`DELETE /api/v1/peers/{id}`、`POST /api/v1/peers/refresh`、
  `POST /api/v1/agent/token`（开关"被主面板管理"）。
- **安全边界（四条，缺一不可）**：① 子机默认不接受任何主面板（凭证为空 ⇒ 一律 403）；
  ② 凭证只读、常量时间比较、可随时重新生成（旧的立即失效）；③ 摘要**只有聚合数字**
  （不含站点名/路径/口令/日志）；④ 主面板连子机：https **必须固定证书指纹**
  （`InsecureSkipVerify` + `VerifyPeerCertificate` 严格比 sha256），http 只放行回环
  —— 凭证不能明文过网。
- **失败必须如实**：拉不到就把错误原样写进 `peer.last_error`、**不保留旧摘要冒充最新**，
  界面显示「没连上」+ 原因（网络/凭证/指纹各不相同）。错误旁边再给一句 `last_advice`
  （面板的**推断**，与错误分开存）：403 → 去子机重新生成凭证、指纹不匹配 → 更新指纹、
  连不上 → 先用浏览器试那个地址…… 判据只匹配**本文件自己产生的**错误措辞，拿不准就不给建议
  （猜错方向比不给更糟）。
- 本机双实例验收（2026-09-24，两个**独立面板进程**，都在 /tmp 调试实例里）：
  子机未开凭证 403 → 开启后拿 64 位凭证 → 错凭证 403 / 对凭证 200 → 主面板添加并**真拉到**
  摘要（版本 1.8.9、服务 10/10、磁盘 68.5%、CPU 31.5%）→ 故意填错凭证时记下
  `子机返回 HTTP 403（凭证不对）` 且**没有摘要** → 非回环 http 与"https 缺指纹"都被 400 拒绝。
- 门禁：`internal/web -run 'TestPeer|TestAgent|TestValidatePeerURL|TestFetchPeerSummary'`
  6 条（含用 httptest 自签 TLS 服务器验"指纹对能连、指纹错必须拒"）。
  真浏览器 `tools/peers-check.mjs`（两个独立实例）**14/14**：侧栏入口、本机接入信息默认关闭、
  子机侧开启并读出 64 位凭证、父面板填表添加、卡片显示「已连接」与真实摘要、页面无报错；
  以及**诚实性**：凭证故意填错时卡片显示「没连上」+ 原因（403 凭证不对）、**不显示任何摘要数字**。
- 界面三态都如实（不拿旧数据装"现在是好的"）：`刚拉到` →「已连接」、`失败` →「没连上」、
  `从没拉到过` →「还没拉到数据」、`超过 15 分钟没更新` →「数据可能过期」+「x 分钟前」。
- **未做**：跨机的写操作（重启子机服务等）、子机侧的操作审计与授权范围、离线时的状态语义。

---

## 七、系统设置（把 macOS 配成服务器）

「系统设置」页把需要终端的事做成开关：合盖不睡、断电自恢复、关 Spotlight 索引、调 TCP 参数、开远程登录等。
底层命令在 `tools/system-services.sh`、`tools/server-mode.sh`：每一步都有"做了 / 跳过 / 失败"三态。
参考命令：`pmset -a sleep 0 disksleep 0 displaysleep 0` /
`womp 1` / `autorestart 1` / `powernap 0`；`mdutil -a -i off`；`defaults write .../com.apple.SoftwareUpdate ...`。

**每一项都先探测"这台机器支不支持"**，不支持就如实报"不支持"。最危险的一次：`pmset -a autorestart 1`
在不支持的机器上**静默返回成功**却什么都没做，脚本却报告"已开启断电自恢复" —— 真跳闸那天才会发现。
**能谎报成功的功能，比没做更糟。**

**磁盘管理**（侧栏 系统 → 💾 磁盘管理）是另一类：只读枚举 + 危险操作（抹盘/格式化/建卷/删卷/重命名）+
对**非系统盘**开放的抹盘/格式化/建卷/删卷/重命名（系统盘一律 403，执行前重新枚举防 TOCTOU）。
安全边界、无头（无 GUI 授权弹窗）说明、API 契约与验证状态见 **`docs/磁盘工具.md`**；
后端 `internal/web/api_disks*.go`，前端 `assets/js/disks.js`。

---

## 八、环境（2026-09-17 起：**单机**）

| | 本机 MacBook Air M4（**唯一开发 + 测试机**） | Mac mini M4（**生产环境**） |
|---|---|---|
| 面板 | `https://127.0.0.1:8443/<后缀>` | **不记录、不访问**（用户自己在用） |
| 可破坏性操作 | **否，绝不重启**（DSH 会话跑在上面） | **禁止任何操作** |
| SSH | — | **禁止**（本机已删除它的主机记录与全部凭据） |

> **为什么从双机变单机**：1.0.0 起 mini 是用户的生产环境，AI 不许接触（用户 2026-09-17 明确要求）。
> `tools/deploy.sh` 的 mini 那一腿已删除；原来的
> "需要重启/断网/断电恢复才能验证"这一类测试**从此没有真机可做** —— 只能近似验证或**如实标注"未验证"**。
> 教训见坑 151。

非交互 SSH 里 `docker`/`colima` **不在 PATH**（用 `/opt/homebrew/bin/...`）。
破坏性操作前先快照，并先 `git diff` 看清将要发生什么。

---

### 发布提速（2026-09-17，用户："每次发布新版都太耗时了"）

实测三段各自的耗时，以及现在的做法：

| 段 | 之前 | 现在 | 做法 |
|---|---|---|---|
| `make check` | 5–7 min（每次都跑） | 同一棵树**跳过** | `tools/check-stamp.sh`：check 成功时把**工作树指纹**（含未跟踪文件内容）写进仓库根 `.zp-check-stamp`；`make deploy` 先 `verify`，指纹一致才跳过并打印"哪个版本、什么时候跑的" |
| `make release` | 双架构，16s | **只 arm64**，8s | `ARCHS`（默认 `arm64 amd64`，`make deploy` 传 `arm64`）—— 本机是 Apple Silicon |
| 上传镜像机 | 4 个包 ≈96MB | **1 个包 23MB** | 只传版本包；`latest` 与 `download/<版本>/` 的副本/软链由远端 LAYOUT 造 |
| 升级等待 | 每轮 sleep 2s | 前 20 轮 0.5s，之后 2s | `tools/panel-upgrade.py`；总窗口不变（≈3 min） |

**实测：一次 `make deploy` 从 ~1.5–2 min 降到 17s**（含构建 8s、上传 23MB、升级本机 4s、版本核对）。
⚠️ 两条纪律不能忘：① `ARCHS=arm64` 只适合只发本机（Apple Silicon）的场景，**正式发布仍建议双架构**（`make release` 默认就是）；
② check 标记是**便利**不是安全边界 —— 手动 `bash tools/check-stamp.sh write` 等价于 `SKIP_CHECK=1`，只在你自己确认过的情况下用。

## 九、已修复的典型坑

**清单已移到 `docs/坑清单.md`**（可删过时条目）。那里记录了每个坑的现象、根因、
修法与对应门禁；本节只留指针，避免这份文档超出 450 行预算、把上下文挤爆。
新踩到坑时：先追加到 `docs/坑清单.md`；如果它会**伤到使用者**（安装卡住 / 谎报成功 /
破坏用户环境），再摘要一句进 `AGENTS.md` 第三节。
