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

## 八、双机环境

| | 本机 MacBook Air M4（开发机） | Mac mini M4 `192.168.1.4`（测试环境） |
|---|---|---|
| 面板 | `https://127.0.0.1:8443/<后缀>` | `https://192.168.1.4:8443/<后缀>` |
| 可破坏性操作 | **否，绝不重启**（DSH 会话跑在上面） | **是**：重启/卸载/重装/断网/合盖/断电恢复都在这做 |
| SSH | — | `ssh zizdog@192.168.1.4`；外网 `ssh -p 22004 zizdog@zizdog.com` |

非交互 SSH 里 `docker`/`colima` **不在 PATH**（用 `/opt/homebrew/bin/...`）；mini 上没有 `timeout`
（curl 用自带 `--max-time`）。破坏性操作前先快照，并先 `git diff` 看清将要发生什么。

---

## 九、已修复的典型坑（只增不减）

> 都是在真机踩到并修复的，回归测试会拦住复发。原文 #91/#92 各重复一次，此处已合并；
> 编号保持原样，末尾 #105–#107 为第九轮新增，#108–#111 为后续轮次新增。

1. **Apple Silicon 上 `sysctl kern.cp_time` 已移除** → CPU 使用率恒为 0。改用 `top -l 1 -n 0` 解析，`kern.cp_time` 仅兜底。
2. **`ps` 的 `%CPU` 不可信** → 面板 fork `top` 的那一刻自己被显示成 73.7%（实际 0.0%）。改用两次 `cputime` 采样差值。
3. **`$VAR` 后紧跟中文标点** → bash 把多字节并入变量名，`set -u` 报错退出。字节级扫描器接入 `make check`。
4. **`local` 声明写在循环体内** → 每次迭代重新声明并清空变量。
5. **受保护文件无法直接重定向覆盖**（sudoers 0440）→ 重装时权限拒绝。改临时文件 + 原子替换。
6. **`status` 用硬编码配置路径** → 面板以 root 跑时 `$HOME` 是 `/var/root`，自定义根目录安装后误报"未初始化"。改为跟随 `ZIZPANEL_ROOT`。
7. **macOS 上 `/var` 是 `/private/var` 的软链接** → 路径前缀校验误判"越界"。两侧都做 `realpath`。
8. **CSRF 校验回退读 Cookie** → 双提交形同虚设（Cookie 浏览器自动带）。改为只认 `X-CSRF-Token` 头。
9. **panel 以 root 跑把网站根算成 `/var/root/www`** → plist 显式传递 `ZIZPANEL_USER`。
10. **Homebrew 拒绝以 root 运行** → sudo 安装时全部 `brew list/--prefix` 查询失败，误报"都没装"。改 `sudo -u <真实用户> brew ...`。
11. **`kill(pid, 0)` 探测 root 进程返回 EPERM** → 服务在跑却报"已停止"。改用 `ps -p <pid>` + launchd 作第二证据源。
12. **`mkcert -install` 在非图形会话永久阻塞** → 安装卡死。加 20 秒超时 + 自签回退，并告知用户手动执行一次。
13. **`conf.d` 被 http 级 include，但里面 `php-fpm.conf` 含 `fastcgi_pass`** → nginx 无法启动。自动迁移到 `includes/` 并改写引用。
14. **反向代理的 `$connection_upgrade` 无处定义** → 一建反代站 `nginx -t` 就报 unknown variable。面板写 `conf.d/upgrade-map.conf`，并反向验证"缺 map 必须报错"。
15. **nginx 反代"强制 HTTPS"的面板时用了 `http://`** → 返回 400。改 `https://` + `proxy_ssl_verify off` + `proxy_ssl_server_name on`。
16. **子路径部署 `proxy_pass` 未剥离前缀** → 后端收到 `/_panel/api/...` 并 307。改 `rewrite ... break` + 无 URI 的 `proxy_pass`。
17. **前端用绝对路径**（`/app.css`、`/api/v1/...`）→ 子路径部署全 404。改相对路径 + 301 补齐末尾斜杠。
18. **`h(spec, node)` 把 DOM Node 当 props** → 弹窗内容整体静默消失。已修并加 UI 回归断言。
19. **`Element.append(null)` 渲染成文本 "null"** → 条件渲染位置冒字面量。统一过滤。
20. **SSE 进度流被 nginx 缓冲** → 日志成批到达、看着像卡住。加 `X-Accel-Buffering: no` 与 `proxy_buffering off`。
21. **`launchctl load` 退出码多次谎报成功** → 改 `launchctl print` 真查状态 + 端口探活。
22. **phpMyAdmin 错误页也返回 HTTP 200** → 健康检查不能只看状态码。
23. **`-X` 在 `-trimpath` 下静默失效** → 发布包没内嵌公钥、退出码仍 0。`make release` 现在运行产物核对公钥。
24. **`go build ... | head` 掩盖失败退出码** → 构建脚本禁止这么写。
25. **面板重启后历史任务丢失** → 如实说"重启后不再保留历史任务"，而不是显示永远 0% 的僵尸任务。
26. **同步请求安装，用户一刷新就杀掉 brew/docker** → 全部改 `launchTask` 返回 202 + `task_id`，进度 SSE。
27. **任务日志的命令标签手写** → 与实际 `cmd.Args` 不一致（`runColima` 漏过 `sudo -n -u <user>`）。标签一律从 `cmd.Args` 派生。
28. **Colima 在非交互 SSH 里找不到 `docker`** → 面板代码显式注入 PATH。
29. **Docker 源只用"第一个能应答的"** → 装到一半发现是慢源。改**测速后排序**并把实测结果打进任务日志。
30. **镜像加速列表里有地址不可达** → 必须有"全部不可达"的如实报告。
31. **应用市场按 `platform: linux/amd64` 转译 arm64** → 明确禁止，测试扫目录条目锁死。
32. **声明了 Docker 路径但镜像没有 arm64 清单** → 测试直接失败，不等到用户点安装才失败。
33. **装完的应用没有卸载入口** → 按来源分三类（托管/面板安装器/纳管第三方），卸载前把"执行什么、删哪些路径、保留什么"列进确认框。
34. **`VerifyUpdatesBlocked` 写死 `/usr/bin/softwareupdate`**（实际在 `/usr/sbin`）→ 必然失败且报错是错的。改按候选路径解析。
35. **把"命令没跑起来/退出码非 0"当成"结论是否定"** → 阻断生效时 `softwareupdate --list` 打印错误的 URL 但退出码 0。抽出 `judgeSoftwareupdate` 纯函数分开报。
36. **phpMyAdmin 显示"已安装·服务未注册"** → 它本来就没有守护进程。目录标 `NoDaemon`，界面改说"已安装·网页入口"。
37. **回滚把用户原本的选择改掉** → `RestoreUpdates` 早期一律写默认值。现在按快照逐项恢复。
38. **多站点共用一个音色样本，后传的覆盖先传的** → 样本按来源分目录，上传按魔数 + ffprobe 校验并归一化。
39. **上游返回 HTTP 200 + 0 字节**（mlx-audio 按扩展名选解码器）→ 接收端自己校验音频内容，不看上游状态码。
40. **接收端把"最后一个未完成的 chunk"算进用量** → 改提交时**预留**、终态**结算**，失败作业不吃额度。
41. **密钥与额度没有热加载** → 改文件后要重启才生效。改按 mtime 热加载 + 面板侧提示。
42. **不允许停用/删除最后一条启用的密钥** → 那等于一键让所有网站 401。
43. **默认音色要"不覆盖用户自己的样本"** → 只在 `default/ref.wav` 不存在、或带 `builtin-voice.json` 标记时才写入。
44. **TTS 安装要 `HF_HUB_DISABLE_XET=1`** → 否则新版 huggingface_hub 去连 hf-mirror 不代理的 Xet 后端。
45. **Lucky 的「编辑配置文件」指向不存在的 `lucky.conf`** → 配置是加密的 `lucky_*.lkcf`。条目不再声明 `ConfigPath`，可视化配置走它自带 Web UI。
46. **健康检查的 URL 只填不清** → 应用换端口后旧地址一直亮红灯。改双向 reconcile（填**和**清）。
47. **frpc 在没有服务端时退出** → 生成的 `frpc.toml` 明确写 `loginFailExit = false`。
48. **Lucky 的"安全入口"/IP 白名单对所有路径返回 404** → 条目不能声明 `HealthPath`，否则永远红灯。
49. **`install.sh` "探目录 HEAD 失败"就转源码构建** → 普通用户机器没有 Go，等于装不上。改探**真实 tarball 地址**，官方不通就用内置镜像。
50. **测试夹具里复制了真实口令与令牌** → 历史提交里也有。已改假值并清理历史与 blob（后续又复发，见 #105）。
51. **`install.sh` 在 `sudo` 下找不回真实用户** → `$HOME` 是 `/var/root`。
52. **面板是 LaunchDaemon，读不到用户 shell 的 rc** → 国内镜像必须由面板自己注入环境变量（`HOMEBREW_API_DOMAIN` 等）。
53. **`sudo` 会清空环境** → `brewRun` 必须在 `sudo -u` **之后**再用 `env` 注入。
54. **macOS 防火墙静默拦截面板端口** → 安装时加入允许列表并校验。
55. **删 `/opt/zizpanel` 不影响已注册的 LaunchDaemon** → 卸载流程显式 bootout，并区分"保留数据 / 彻底删除"。
56. **`000-default.conf` 迁移只做了一半** → 站点目录与 include 目录对不上，`nginx -t` 通过但站点 404。
57. **端口冲突（Stirling PDF 8082）** → 探测失败时报告"端口被谁占用"，不只说"启动失败"。
58. **Squoosh 是第三方镜像的页面** → 目录里标注来源，不假装自研。
59. **一键建站（Typecho/WordPress）vhost 未生效** → 见 `ZizPanel-当前状态.md` 残留项。
60. **备份包含口令却没提示** → 导出前明确列出包含哪些敏感文件。
61. **`receiver /jobs` 测试用了真实端口** → 改假上游 + 临时目录。
62. **接收端测试影响真实 receiver 进程** → 隔离到临时 key/usage 文件。
63. **单测污染真实 `~/Library/LaunchAgents/`** → 护栏测试 + 指纹门禁（第四节）。
64. **`present` 的文件没写全就宣称交付** → 交付前逐项核对真实状态。
65. **面板升级后旧进程还在跑** → apply 后必须等新版本健康检查回来（看版本号）。
66. **健康检查的版本号是唯一"真的换了二进制"的证据** → 只看端口通了不够。
67. **`launchctl` 的 `RunAtLoad` 与 `KeepAlive` 都要有** → 少 `KeepAlive` 崩溃后不自起，少 `RunAtLoad` 开机不自启。
68. **`sudoers` 权限必须 0440 且属主 `root:wheel`** → 否则 sudo 拒绝加载。
69. **面板证书到期时间要如实显示** → 自签 10 年也要写清是自签。
70. **`mkcert` 没装时不能反复尝试调用** → 每次安装都等 20 秒超时会拖慢。
71. **前端 `node --check` 放过浏览器拒绝的语法** → 整页白屏且不给行号。必须用真正的 ES 解析器（acorn）。
72. **`present` 之外的"文件已生成"都不算交付** → 最终回复必须点名文件。
73. **`install.sh` 用 `curl | sudo bash` 时 `$0` 不可用** → 脚本不能靠 `$0` 找自己。
74. **`install.sh` 下载失败要重试并换源** → 一次抖动就失败会反复困扰用户。
75. **面板"打开"按钮给出不通的入口** → 探测失败时把「直连端口」作为首选。
76. **"没有 X"不等于"X 坏了"** → 先问"这个应用本来该有 X 吗"（phpMyAdmin 无守护进程）。
77. **"纳管"不等于"用户装的"** → 面板自己的安装器也用 `Adopted`，能否卸载看 `PanelInstaller`。
78. **找不到命令 ≠ 结论是否定** → 分开报（见 #34/#35）。
79. **`limit_conn`/`limit_rate` 不是慢的唯一原因** → 先用测速把"谁慢"钉死。
80. **回滚要按快照逐项恢复**（见 #37）。
81. **测速排序别用 `sort.Strings`** → "顺序有语义"的配置要显式排序并记录理由。
82. **"共享一份可变文件"的接口迟早出事** → 按来源分目录（见 #38）。
83. **"无界面安装开发者工具"在真机上不成立，而任务日志只说"没成功"** → 抹机 mini 上装 CLT：`softwareupdate -i` 实测 **15 分钟只下 1 MB** 后停住，回退 `xcode-select --install` 是**图形对话框**，无人值守就永远停着；而任务日志只有一句"这条没成功"，苹果那侧的原话一个字没有。修法（0.8.8）：给苹果原始 pkg 加**镜像整包安装**的路（`clt/index.json` + `clt/<产品号>/*.pkg`），顺序 **镜像 → softwareupdate → 弹窗**，两条苹果路径的**真实输出**都写进任务步骤，成功判定改成 `python3Works()`（全新 macOS 的 `/usr/bin/python3` 存在但会弹对话框）。教训：**"找到了官方静默开关"和"这开关在这条网络上能用"是两件事**。
84. **"降权运行"和"需要 root"在无终端环境互相锁死** → Homebrew 安装脚本 EUID=0 拒绝安装，降权后它自己又调 `sudo install -d -o <user> /opt/homebrew`，而 LaunchDaemon 没有终端读密码 → 必然失败（`sudo: a terminal is required`）。修法（0.8.9）：安装期间放一个只作用于脚本 PATH 的 `sudo` 垫片，装完即删，不动系统 sudoers；安装脚本输出末尾带进任务日志。
85. **大文件"断了就重来"在慢链路上等于永远装不完** → 576 MB CLT 包实测偶发 `Connection reset by peer`，而重试从 0 开始（已下 200 多 MB 全废）。修法（0.8.9）：镜像切 32 MB 分片，并行下载 + 逐片校验 + 断点续传，拼接后仍核对整体 sha256（分片校验说明不了顺序），清单没有 `parts` 时退回单文件。
86. **"往 PATH 里塞一个 sudo"治不了硬编码路径的官方脚本** → Homebrew 安装脚本用的是写死的 `/usr/bin/sudo`，垫片一行都没被走到，且开头 `sudo -n -l mkdir` 探权限失败就 `abort`。修法（0.8.10）：安装期间临时给面板用户免密 sudo（专用文件名 + 0440 + root + 先 `visudo -c` + 绑定具体用户 + 无论成败都 `defer` 撤销 + `sudo -k`），授权/撤销都写进任务步骤。教训：**先确认"我拦的那条路径是不是真的被走到"**。
87. **`make release` 两次构建哈希不一致** → `-ldflags` 注入了 `BuildTime`，本就不该期望字节一致。以 `manifest.json` 记录的 sha256 为准，不靠"记住上次的哈希"。
88. **"启动时探测一次"的路径配置，在"先装面板、后装依赖"的机器上永久错下去** → `BrewPrefix` 在首次启动那刻探测并持久化进 config.json，全新机器上 `/opt/homebrew` 还不存在 → 以后报 `env: /usr/local/bin/brew: No such file or directory`。修法（0.8.11）：`Config.ReconcilePaths()` 每次加载重新探测（只认真实存在的 `bin/brew`），派生路径一起改，有变化才写回。
90. **"换一个客户端就好了"是网络故障里最会误导人的一类** → Qwen 模型下载反复超时，同一 URL 用 `curl` 走 IPv4 有 5 MB/s：本机网络把域名解析到**不可达但也不拒绝**的 IPv6，Python 客户端一直挂在 `connect()`。修法（0.9.1）：给 venv 写 `sitecustomize.py` 把 `socket.getaddrinfo` 限制到 `AF_INET`（14 个文件 6 分钟下全）。`PYTHONSTARTUP` 不行——只在交互式解释器生效。
91. **`brew reinstall nginx` 静默丢掉面板的两条 include**（2026-09-16）→ 配置被还原成 brew 默认版，`include conf.d/*.conf;` 与 `include vhosts/*.conf;` 一起没了：文件在、`nginx -t` ok，站点与反代全 404。第二层：同时存在两个 nginx master（旧的占 8080），旧 master 退出清空共享 pid，`nginx -s reload` 一直报 `invalid PID number ""`。修法：`EnsureVhostsInclude`（幂等 + 先备份）与 `ensureNginxRuntimeDirs` 纳入**每次启动自愈**；只对具体目录 chown，**绝不 `chown -R` 到上层**（那次就是这么把 etc 弄坏的）。教训：**"配置文件在"不等于"配置被加载"**。
92. **"镜像上没有这个包"不该让用户装不上** → 最初的镜像语义是"唯一来源，缺资源就明确失败"。真机暴露代价：镜像站临时挂掉或不在同一网络就彻底装不上，哪怕公网源可用。用户随后要求"能用这个地址的尽量用，但每次都要判断通不通，不通走国内其它路线"。修法：**镜像优先 + 按资源探测 + 自动回落**，并把"这次没走镜像、为什么"写进任务步骤。教训：**"统一管控"和"可用性"要分开设计**。
93. **同一个应用的能力被拆到两个页面还各写一份按钮逻辑** → 市场卡片只跳 `#/services`，真正的操作全在服务管理页；frpc 的配置入口在市场、接收端的在服务管理。修法：`servicePanel.js` 做**唯一**详情面板，两处都通向它，按钮清单由数据决定（`config_path`/`managed`/`ui.slug`/`ui.console_only`/凭据接口/appWidgets`）。教训：**同一对象的不同视图必须共用组件与动作实现**。
94. **市场条目里的 `name` 是展示名，不是服务记录名** → `serviceNameOf` 拿 `name`（"frpc（frp 客户端）"）去调 `GET /services/<名>` 必然 404，面板显示"未在服务管理里"、配置与日志都拿不到。**JS 语法检查是绿的**，是 `tools/appdetail-verify.mjs` 抓到的。修法：标识类字段逐个试（`uninstall.service` → `service_label` → `name` → `id`）+ `svcNameCache`。教训：目录条目的 `Name` 是给人看的，调接口要用 `ServiceLabel`。
95. **"面板持有的 MySQL 口令"与"MySQL 实际的口令"会不一致**（2026-09-16 mini，有测试锁死）→ 旧实现「ALTER USER 后再 FLUSH PRIVILEGES」，而 FLUSH 会**重新连接**、带的是面板手里的旧口令（当时是空）：ALTER 已生效、调用方却收到 1045，新口令既没写回配置也没显示给用户 → 面板永久锁在门外。修法四处：去掉 FLUSH（ALTER USER 即刻生效），口令语句走 **stdin** 不出现在 argv；凭据状态改成**真连一次**验出的四态（已验证可用/确实无口令/面板没口令而服务器要 `unconfigured`/配置了但认证失败）+ `unreachable`；改的是面板自己的账号时必须**用新口令自检 → 写回 config.json**（写盘失败不回滚内存值，新口令留一次性凭据区块）；装 MySQL 时闭环（数据目录是我们 `--initialize-insecure` 初始化的，起来后限时询问 root 口令、默认 60 秒超时自动生成 26 位 base32）。教训：**"装完再对齐"一定留下不一致窗口**；"配置里没有"与"服务器上没有"是两件事。
96. **任务不能只有"输出"，还要能"问一句"**（有测试锁死）→ 装 MySQL 必须拿到 root 口令，而"先装完、之后再让用户去别的页面填"正是 #95 的成因。任务中心补输入通道：`tasks.Task.WaitInput`（超时=正常路径，返回 ok=false 让业务给默认值继续，**绝不卡住**）、`POST /api/v1/tasks/{id}/input`（只有正在等该 key 的任务才收，晚到/答错题一律 409）、SSE 的 `input_required` 事件。安全约定：**值只走 SubmitInput**，Meta/SSE/步骤/日志/审计里都不许出现（唯一例外是任务结果里的一次性凭据区块）。
97. **"模型权重走镜像"配了 `HF_ENDPOINT` 却完全没生效——IOPaint 的 LaMa 来自 GitHub**（2026-09-16 mini，有测试锁死）→ 日志打印着镜像端点，实际 `iopaint/model/lama.py` 的 `LAMA_MODEL_URL` 指向 GitHub release（196MiB），`HF_ENDPOINT` 一个字节都不影响；模型落在 `~/.cache/torch/hub/checkpoints/`（不是 `~/.cache/iopaint`），只看那两个目录会误判"一个字节都没下"。更糟：`torch.hub` 下载**没有超时**，而 `waitPort` 超时后只写 Warning 就 `return nil` → 任务显示**"任务完成 ✅"**，服务根本不可用。修法：面板在服务启动前自己把权重下好（`<镜像>/models/iopaint/big-lama.pt` 优先、GitHub 回落、探通才算、30 分钟总超时、90 秒停滞看门狗、MD5 必须等于上游公布的 `e3aa4aaa…`）；plist 写上游**官方**读的 `LAMA_MODEL_URL`/`LAMA_MODEL_MD5`（不 patch 用户 venv）；就绪等待超时改成**返回错误**并写明端口、日志、权重路径。教训：**"我配了环境变量"不等于"它读这个环境变量"**。
98. **`make check | tail` 会掩盖退出码** → 回显的 `[exit code: 0]` 是 `tail` 的，`make` 实际 `Error 2`，差点据此认为门禁绿了。固定写法见第四节。
99. **面板 API 的键名与基址（我因此报出 3 个假缺陷）** → 基址必须带面板后缀（根路径 `/api/v1/login` 是 404）；`GET /api/v1/services` 的响应键是 **`list`**；同一天还有 Typecho 镜像文件按 `.tar.gz` 去下、实际是 `typecho.zip` 拿到 404。教训：**"看起来不对"先怀疑自己的探测方式**。
100. **macOS 上 Docker 的网络与 Linux 完全不同，两台 Mac 之间还会不一样**（2026-09-16）→ Colima/Lima 的 VM 是 `192.168.5.1/24`：mini 上 `VM → NAS(8090)` 是 `Connection refused`，本机同一条却是 200（**差异原因未查明**）→ **"把 Docker registry 放 NAS"不能依赖**，镜像源不要自动指向 NAS；Docker 用公网多源（实测 `docker.m.daocloud.io` 拉 `alpine` 7 秒、arm64）。更关键：**docker 只对 manifest 做多源回落，层数据一旦选定某个源就只会失败/卡住**，所以"清单能取、层数据取不到"的源有害（`docker.1panel.live` 层数据 300 秒 0 进度、`docker.1ms.run` blob 404）。镜像源要有**能力等级**，排序主键是能力、延迟只做次键。
101. **Colima mounts 写入有两个致命细节**（有单测锁死）→ ① `location: "~"` 的**引号不能省**（裸 `~` 在 YAML 是 `null`，colima 读成空串后启动直接失败）；② **跑着的实例上 `colima start` 是空操作**，改完 mounts 必须 `stop && start`（**不需要重建 VM**——`mounts` 不在 `setFixedConfigs()` 里）；③ 不要用 `colima start --mount`：它默认 `--save-config`，会把 `mounts` 段覆盖成只有这一条。这一条修的是 D13：compose 的 `./data` 原先落在 VM 内部，面板显示的路径上没有数据、卸载删空壳、**备份会漏、删 VM 会静默毁掉全部应用数据**。
102. **8091 pull-cache 的三个固有缺陷**（三个都真撞到了）→ ① **不流式**：整份文件落盘后才回第一个字节，冷缓存大文件首字节延迟=整个上游下载时间（实测 opencv 48MB 冷 ttfb **357 秒**），面板镜像路径因此加 `--timeout 180 --retries 3`，根治要改流式（**需重建容器**）；② **没有 TTL**：索引页落盘后不再回源，上游发新版本镜像上看不到；③ **裸目录请求 502**：缓存布局里 `/simple` 是目录而裸路径缓存文件同名 → `rename … file exists`，`/brew/` 与 `/pypi/simple/` 撞同一个 bug，裸路径要改成 exact-match 说明页。
103. **`huggingface_hub` 会校验响应头，缺 `X-Repo-Commit` 就拒绝整个端点**（2026-09-16）→ NAS 上 2.9GB 模型**全在、大小全对**，真实客户端直接失败。根因是旧版 pullcache 写的 meta 缺这个头。修法是**只补元数据、不动文件本体**（18 个 meta），修完用真实 `hf_hub_download`/`snapshot_download` 验证，还特意拿一个**完全没缓存**的仓库跑一遍证明机制没问题。教训：**"文件在、大小对"远不等于"客户端能用"**——镜像功能必须用真实客户端验收。
104. **同一个文件被两个代理并发修改会互相破坏**（本轮多次出现）→ 瞬时编译失败（`"errors" imported and not used`），以及"我改了会被别人整份写回覆盖"。正确做法是给每个代理**划死文件所有权**；文件已归别人时把补丁发给主人，而不是自己动手。另外：**反漂移测试真的抓到了并发改动**——有代理把 n8n 镜像改成 `n8nio/n8n:latest` 而声明没跟上，`make check` 立刻变红。
105. **真实面板口令长期硬编码在测试夹具里**（本轮新增，门禁锁死）→ 自 v0.3.1 基线起、**9 个已跟踪文件**带着真实口令，仓库曾公开发布过。`af2b887` 只清理了一部分，`tools/uitest.mjs` 甚至留了「已改掉」的**假注释而值没改**；第九轮又发现 3 个新文件带着同一个口令。**教训：靠注释和记性不算数，必须做成门禁；且门禁要扫「将要提交的文件」（tracked + untracked 非忽略），只扫 `git ls-files` 会漏掉新增文件。** 口令一旦提交过就必须轮换（改历史不算修好）。
106. **`tools/deploy.sh` 原来强制要求 `NAS_PASS`**（本轮新增）→ 本机对 NAS 早有 SSH 密钥登录（`ssh -o BatchMode=yes` 可通），导致"一条命令部署"在没口令时直接退出。现在改成**密钥优先、口令兜底**；探测**必须带 `BatchMode=yes`**，否则 ssh 会挂在那里等输入、看起来像卡死。
107. **面板写操作要 CSRF 双提交**（本轮新增）→ 从可读的 `zp_csrf` cookie 取值放到 `X-CSRF-Token` 头，否则接口返回 403「CSRF 校验失败」。手工用 `curl` 调 `/api/v1/*` 的写接口时必须带。
108. **dns-01 在"IPv6 DNS 不可达"的网络里必然失败**（2026-09-16 mini，有测试锁死）→ 申请证书报 `DNS call error: read udp [fd58:...]->[fd58:cbf6:7fb8::1]:53: i/o timeout`。根因：lego 的默认递归解析器是 `google-public-dns-a.google.com:53` 这类**主机名**，用它之前要先做一次系统解析；而路由器通告的 IPv6 DNS 不可达 → UDP 直接超时（mini 的 `/etc/resolv.conf` 只有 macOS 注释，lego 因此回落到它自己的主机名默认值）。修法：dns-01 时显式传 **IPv4 字面量**解析器（`internal/acme/resolvers.go`：223.5.5.5 / 223.6.6.6 / 119.29.29.29 / 1.1.1.1 / 8.8.8.8），过滤器只接受 IP 字面量，主机名一律丢弃并告警；可用 `Manager.DNS01Resolvers` 覆盖。门禁：`TestDefaultDNS01ResolversAreIPLiterals`、`TestDNS01Resolvers`。教训：**默认值是主机名，就会把一条本地网络故障变成签发故障。**
109. **泛解析 CNAME 让 dns-01 永远失败**（同机，有测试锁死）→ 面板日志显示"正在写入 TXT 记录"→"记录已提交"，任务却以 `403 :: unauthorized :: No TXT record found at _acme-challenge.<域名>` 失败；用腾讯云 API 逐秒盯记录列表，域里始终**没有** `_acme-challenge` 记录。根因：该域有泛解析 `* CNAME lede.zizdog.com`，lego 的 dns-01 支持 CNAME 委派，于是把 TXT 写到了 CNAME 目标 `lede.zizdog.com`；而 **Let's Encrypt 不跟随由泛解析产生的 CNAME**，它直接查 `_acme-challenge.<域名>` → 空（可逆实验证实：手工在该挑战名插 TXT，权威 NS 立刻正常返回 —— **显式记录能覆盖泛解析 CNAME**）。修法：dns-01 默认设 `LEGO_DISABLE_CNAME_SUPPORT=1`（TXT 写回挑战名本身），需要 CNAME 委派的用户把 `Manager.DNS01FollowCNAME` 设为 true。门禁：`TestCNAMEPolicyDisablesFollowByDefault`、`TestCNAMEPolicyUnsetsWhenItWasUnset`、`TestCNAMEPolicyFollowLeavesEnvUntouched`。教训：**"记录已提交"不等于"CA 看得到"。**
110. **ACME 证书属主导致 nginx 读不到证书**（同机，站点 SSL 绑定返回 500，有测试锁死）→ 接口报"配置已写入、nginx reload 成功，但复核发现新配置没有生效（探测期望 403、实际 000）"；443 不在监听；以 zizdog 身份 `nginx -t` 得 `cannot load certificate ... Permission denied`。根因：面板以 root 运行，写出的证书目录是 **0700 root、私钥 0600 root**；而 macOS 上 homebrew 的 nginx 是**以普通用户**（LaunchAgent）运行的 —— **本机 nginx 恰好是 root 起的，所以只在 mini 暴露**（又一次"一台机器的结论不能当普遍规律"）。修法：`saveCert` 落盘后把证书根目录及内容 `chown` 成与 `DataDir` 相同的属主（`alignOwnerWithDataDir`），**不把私钥放宽**（仍 0600）；失败只告警，由绑定站点时的 403 探针如实挡下。门禁：`TestAlignOwnerWithDataDirKeepsModes`、`TestAlignOwnerWithDataDirWarnsWhenDataDirMissing`。
111. **ACME 引擎日志只进面板日志，任务中心只显示"申请失败"**（同轮，可观测性）→ 排查 dns-01 失败时，真正有用的"正在写入 TXT 记录 / 等待 DNS 传播"两行只在面板文件日志里，任务窗口只有一句"申请失败"，用户只能登机器翻日志。修法：签发任务的引擎日志**同时**作为任务步骤下发（`certManagerForIssue`，按需新建实例、不污染共享 `Manager`；续期仍走 `certManagerForRenew`）。教训：**失败信息必须出现在用户正在看的地方。**
112. **同一台机器重复安装被报 failed（幂等缺陷）**（2026-09-16 mini 真机取数，8 个场景，有测试锁死）→ 已装应用再点安装：端口类报 `安装前检查未通过：端口 80 已被面板管理的服务占用：homebrew-mxcl-nginx`（nginx/mysql84/ollama/uptime-kuma），php 类报 `服务已安装但写入注册表失败: 服务名 php83 已存在`（php81-84）。用户看到红色 failed，会以为把机器弄坏了。修法：`Manager.Install` 顶部加**幂等闸门**（`installedSkipResult`：命中面板记录或 launchd 里存在该应用候选 plist → 终态 succeeded + 「已经装过了，本次跳过，没有重复安装」，**不执行任何安装命令**）；`Preflight` 把"端口被**本应用自己**的服务占用"判为已安装（只看面板记录、不看进程名）；`registerAppService` 同名同应用幂等。**真冲突必须继续失败并点名占用者**（别的服务/未知进程）。门禁：`TestInstallSkipsWhenAppAlreadyInstalled`、`TestPreflightPortHeldByOwnServiceIsNotAConflict`、`TestPreflightAndInstallStillFailOnRealPortConflict`、`TestRegisterAppServiceIsIdempotentForSameApp`、`TestReinstallAfterUninstallKeepDataIsNotSkipped`。教训：**重复操作必须与首次操作区分开，幂等是安装器的第一属性。**
113. **compose 超时把失败原因丢掉了（20 分钟硬上限）**（同轮）→ n8n / stirling-pdf 在 mini 上恰好都在 **~1202s** 失败，任务里只有一句 `compose 命令失败: … Downloading 128MB`，看不出是"拉取超时被中止"。根因：`internal/services/docker.go` 的 `composeDriver.run` 把 `err`（ctx 超时时是 `signal: killed`）整个丢弃，只贴最后 400 字节输出；20 分钟上限硬编码在调用点 `install.go`（`drv.run(ctx, 20*time.Minute, "up", "-d")`），`Options` 里没有可调字段（对比 `colimaStartTimeout = 90*time.Minute`）。修法：错误信息带上**用时 + 原因**（超时 → "超过 20m0s 上限被中止（镜像层没拉完；可稍后重试，或先手动 docker pull）"）。待办：把 20 分钟提成命名常量/可配置项；拉取期间加"仍在拉取，已用 X 分钟"心跳，区分"慢但活着"与"卡死"。教训：**超时不是错误原因，被中止才是；错误里必须能看出是哪一种。**
114. **macOS 15「本地网络」隐私门会拦 nginx 的局域网反代（无头机器无法授权）**（2026-09-16 两台机器交叉验证，有基准与测试锁死）→ 现象：反代局域网目标一律 502，错误日志 `connect() to 192.168.1.8:8090 failed (65: No route to host)`，系统日志同一秒记录 `LocalNetwork: bundle id nginx-<UUID>`。关键事实（都实测过）：① **不是二进制升级导致**——同一二进制（1.31.5，03:56 装）在 08:26/11:53 还能反代，21:15 那次 nginx 重启/重注册之后才废；② **不是 root/用户差异**——本机 root nginx 同样被拦（你点"允许"后立刻 502→200）；③ **授权是给"进程/启动上下文"的**——同一次授权后 nginx 通了，而"从非交互 SSH 起的转发器"仍然被拒；④ **面板守护进程从未被拦**（两台机器 6 小时系统日志里除 nginx 外 0 条 LocalNetwork 记录，面板换二进制+重启 7 次以上仍正常）。**修法（0.12.3）**：把局域网出口收回面板 —— `internal/proxies/forwarder.go` 在 `127.0.0.1:47000-47999` 起 TCP 转发器（双向 io.Copy、TCP_NODELAY、半关闭、空闲 120s、每规则并发上限 256、字节计数），生成的 nginx 配置只连回环（回环不受此门限制）；`Rule.LANForward` 三态 `auto/on/off`（private/链路本地自动转发、回环与公网直连）。**基准**：纯回环 512MB 直连 ~6.8–7.2 GB/s vs 经转发 ~7.1–7.4 GB/s（噪声内）；单请求 +120µs；真实 NAS 波动 ±30% 完全淹没。**教训**：凡是"需要某个系统权限才能工作"的能力，都不要交给 Homebrew 的二进制（它没有稳定标识），要放在自己的常驻进程里；Lucky 之所以稳，就是因为它自己就是那个被授权的常驻进程。
115. **macOS 15「本地网络」授权库写不进去，但官方另有一扇"可编程预授权"的门**（2026-09-17 mini 真机 A/B，含官方原文与实测）→ 起因：用户问"既然能删授权就能授予授权"，于是先试删 Lucky 的三条记录。**结论一：那扇门是关的**——`/Library/Preferences/com.apple.networkextension.plist`（NSKeyedArchiver 的 `NEConfiguration`，`com.apple.preferences.networkprivacy-<UUID>` → `PathController.Rules`）以 root 直接原地写、`rm` 都是 `Operation not permitted`；`defaults import`（cfprefsd 正规路径）会把结构弄坏（演练：`$objects` 138→0）。**结论二：官方有另一扇门，且实测可编程生效**——`com.apple.network.local-network` 域的 `AllowedEthernetLocalNetworkAddresses` / `AllowedWiFiLocalNetworkAddresses`（CIDR 数组，`defaults write` 即可，**重启后生效**，15.5+）。官方 TN3179 原文："the system treats every address on that network as if it were not a local network address. **Every program can access that address, regardless of its Local Network privilege state.**" **A/B 证据**（探针 = 自建 ad-hoc Go 二进制 + `UserName=zizdog` 的 LaunchDaemon，与当初被拒的 nginx 同形态、且从未在授权库里登记）：写入前 `RESULT=DENIED err=dial tcp 192.168.1.8:8090: connect: no route to host`；写入 `192.168.1.0/24`（系统域+用户域）+ 重启后 `RESULT=ALLOWED connected in 2ms`；设置跨重启保留；面板与全部服务自愈正常。**代价（必须写清）**：该网段在这台机器上对**所有程序**都失去这道隐私门（不是只对 nginx）。**产品结论**：默认仍用面板侧 loopback 转发器（细粒度、无需重启）；把预授权做成**可选**设置项「允许免授权访问内网段」（含一键回滚），给"就是要 nginx 直连、且不想每次 nginx 升级后点授权"的用户——它也顺手根治了 nginx 升级丢授权的根因（不再依赖 nginx 自己的签名标识/路径）。教训：**"删不掉"不等于"写不进"**，要找系统真正认可的写入路径（`defaults`/官方 defaults 域），而不是硬碰受保护的文件。
116. **"formula 有 service 块"不等于"装完就能用"**（2026-09-17 新增 Miniflux / Syncthing 时发现）→ 通用 brew 流程只做 `brew install` + `brew services start`；而 Miniflux 没有配置文件根本起不来（service 块写死 `-c /opt/homebrew/etc/miniflux.conf`），Syncthing 起来后 GUI 只绑 127.0.0.1（局域网打不开）。两者都需要"装完之后的收尾"，而收尾要建库/写配置/生成初始口令/改绑定 —— **通用流程做不了，必须走自研安装器**（`PanelInstaller` + `handleMarketInstall` 分流，与 iopaint/qwen3tts 同一条路）。加应用时的判据因此是三层，不是两层：**① 有 brew formula？→ 用；没有再看官方 darwin-arm64 release 产物；② 两条原生路都问一句"装完还需要数据库/配置/初始口令/改绑定吗"——需要就不是填空，是自研安装器；③ 都不行才 Docker。**（卸载侧同一条道理：`PanelInstaller` 非空的应用必须在 `installerPlan` + `UninstallApp` 两处都登记，漏一处就是"市场点了没反应"或"删不掉"。）
117. **Miniflux 的管理员口令无法从命令行非交互创建**（同轮）→ 上游 `-create-admin` 的帮助文本是 "Create an admin user from an interactive terminal"，走的是终端读口令（管道喂不进去）；而本项目**不许把口令放进 argv**（吃过 `ps` 泄露真凭据的事故）。正解是官方另一条路：配置文件里的 `CREATE_ADMIN=1` + `ADMIN_USERNAME` + `ADMIN_PASSWORD`，守护进程启动时 `createAdminUserFromEnvironmentVariables` 会建号。关键事实（读上游 `internal/cli/create_admin.go` 确认）：它**先 `UserExists(username)`，存在就跳过、绝不重置口令** —— 所以这三个键可以长期留在 0600 的配置里，重复安装幂等。门禁：`TestMiniflux*`。
118. **Syncthing 的 GUI 要"地址 + 口令"同时改，且 config.xml 只认 bcrypt**（同轮，真机实测）→ 上游默认 `<address>127.0.0.1:8384</address>`，局域网用户打不开；只把地址改成 `0.0.0.0:8384` 会得到一个**无口令的远程控制台**（Syncthing 能用 API Key 直接操作），所以安装器必须同时写 `<user>` + `<password>`。实测两个坑：① **`<password>` 只接受 bcrypt 哈希**（明文被拒：`GET /rest/system/status` 401 / JSON 登录 403；`$2a$10$…` 由面板用 `golang.org/x/crypto/bcrypt` 生成，与上游 `CompareHashedPassword` 一致）；② **验收探针不能用 `/rest/system/*`**——即使口令正确它也因为 CSRF 保护回 403，正确的探针是 `POST /rest/noauth/auth/password`（正确 → 204、错误 → 403），探活则用免鉴权的 `GET /rest/noauth/health`。门禁：`TestSyncthing*`。
119. **上游只发 md5 的 release 产物：如实降级，不许假装校验过**（同轮，Alist v3.64.0）→ 上游 `AlistGo/alist` 的 release 只有 `md5.txt`（`sha256.txt`/`checksums.txt` 实测 404），而 `binary_release.go` 的 tarball 轨在 `ChecksumAsset==""` 时**不做内容校验**、只做 `file(1)` 架构复核。处理方式：① 声明里把 `Checksum.SHA256` 留**空**并写清"上游没有 sha256"，不编造；② 本机把整包下下来**实算 sha256**，并与上游 md5 互证（两条独立算法都吻合）后写进镜像站 `manifest.json`（镜像存在时面板按它校验）；③ 目录/说明里明说这条路的校验强度弱于 frpc / ddns-go。教训：**"这里没有校验"是可以接受的结论，"假装校验过了"不行。**
120. **杀掉 Syncthing 只杀父进程会留下守护进程，把新配置的验证结论带偏**（同轮，自建实验里踩到）→ `syncthing serve` 会 fork 出 monitor + 子进程；只 `kill` 父 PID 后旧实例仍在监听 8384，于是"我改了 config.xml 并重启，bcrypt 口令却被拒"看起来像配置格式不对（实际是**旧实例仍在服务**，用旧口令）。`pkill -f` 之后同样的配置立刻通过。教训：**验证"新配置是否生效"之前，先证明"旧进程真的没了"（`lsof -nP -iTCP:<port> -sTCP:LISTEN` + `pgrep -fl`），否则会把 A/B 结论搞反。**
121. **brew 应用的配置文件不在家目录里，「编辑配置文件」入口需要绝对路径支持**（同轮）→ `ConfigFilePath` 原来只认两类位置（compose 的 `<workDir>/compose/<id>/` 与 release 二进制应用的 `<home>/<RootDir>/`），于是 brew 应用（Miniflux 在 `/opt/homebrew/etc/miniflux.conf`、Syncthing 在 `~/Library/Application Support/Syncthing/config.xml`）**根本没有「📝 编辑配置文件」入口**，用户只能 SSH。修法：`ConfigPath` 支持绝对路径与 `~/` 前缀，`appConfigRoots()` 会把它们所在目录加进文件管理器白名单（越界校验/软链接解析仍由 `files.Manager` 负责）。
122. **`launchctl bootout` 打偏域 + 把"查不到"当"没加载" = 停止服务谎报成功**（2026-09-17 mini，面板 0.12.9，有测试锁死）→ 现象：`POST /api/v1/services/sh-brew-syncthing/stop` 返回 **HTTP 200 `ok:true`（cost≈8s）**，而进程仍以同一 PID 监听 `*:8384`；`start` 报 `Bootstrap failed: 125: Domain does not support specified action`。根因两条：① 面板以 root 跑，`sudo -n -u <user> brew services …` 把用户代理加载进 **`user/<uid>`** 域，而旧 `launchDomain()` 只看 plist 路径就断定 **`gui/<uid>`**；② 旧 `LaunchStatus` 把 `launchctl print` 的**任何**错误都当成 `Loaded=false`，于是 `bootout` 打偏域报错后复核得到"未加载"→ `LaunchUnload` 返回 nil。修法（`internal/priv/priv.go`）：候选域按 `system → user/<uid> → gui/<uid>` 逐个探测（user 优先，brew 就落在这里；plist 不在也要探测），`launchctl print` 只有 `Could not find service`/`service not found`/`Could not find domain` 才算"没有"，`125 Domain does not support specified action`、`Operation not permitted` 一律当错误；unload 逐个卸载**真正加载了它**的域并**轮询等它真的消失**（`bootout` 是异步的，install.sh 里早有同样的等待），load/kickstart 命令成功后也必须复核终态。门禁：`internal/priv/launchd_test.go`（进程内假 launchctl，含"bootout 失败且仍在→必须报错"与"异步卸载→等待后成功"）、`internal/web/api_services_launchd_test.go`（假 launchctl 走完整 HTTP 通路；实测旧代码下这条测试返回 200 变红）。教训：**"操作命令的退出码"不是结论，"复核终态"才是；"查不到"与"查不了"必须分开。**
123. **"启用 HTTPS"只勾了开关、创建请求里没带证书 → 同端口已有 HTTPS 规则时永远保存不了**（2026-09-17 用户真机报障，有测试锁死）→ 8889 上已有三条 HTTPS 规则时，用户新建一条**也想开 HTTPS** 的规则（选了 `*.zizdog.com`）却一直弹出"端口 8889 上已有HTTPS规则「wp.zizdog.com」，而这条是HTTP"。根因在**前端**：启用 HTTPS 是"先建规则（`POST /api/v1/proxies`）→ 再调 `/proxies/{id}/ssl` 绑证书"两步，而第一步的 payload 里**没有任何 ssl 字段**，后端看来它就是一条 HTTP 规则 → 被"同端口不能混用 HTTP/HTTPS"的保护直接 409，第二步永远没机会跑。修法：前端在"要启用/更换 HTTPS"时把证书信息**一起**放进主接口的 payload（面板证书库从证书列表取 `cert_path/key_path`，manual 用粘贴的证书；两者都是 `/ssl` 会写入的同一份），`self/mkcert` 因为证书要现生成，后端错误信息里明说"先取消勾选「启用」建成规则 → 绑好 HTTPS → 再启用"。**另外修了那条报错自身把方向写反的 bug**（用户实测：他这条是 HTTP、已有的是 HTTPS，报错却说"已有HTTP规则…这条是HTTPS"）。教训：**两步式"先创建再补属性"的接口，第一步必须能表达第二步的意图**，否则保护逻辑会把正常操作锁死在门外。

124. **站点 80 块的 301 用 `$host` 会丢端口，把用户送到没放行的 443**（同轮，真机报障的"另一个发源地"）→ 站点开了 SSL 时模板写的是 `return 301 https://$host$request_uri;`，而 `$host` **不含端口**：站点被反代到 `https://<域名>:8889` 时，这条跳转会把浏览器送到 `https://<域名>/`（443），公网只放行 8889 → 直接打不开。改成 `$http_host`（客户端本来没带端口时等于 `$host`，直连 443 的站点行为不变）。配套：反代规则在「透传原始 Host」打开时，额外注入 `proxy_redirect ~^https?://<本规则域名>(:[0-9]+)?(/.*)$ $scheme://$http_host$2;` —— 上游应用若按**自己保存的、不带端口的**站点地址生成绝对跳转（WordPress siteurl、Typecho 站点地址），也会被改写成客户端真实 authority；**只改写本规则域名的地址，跨域跳转（SSO/CDN）不碰**。⚠️ 301 会被浏览器**永久缓存**，所以改了服务端也要清一次缓存/用无痕窗口验证 —— 否则看到的仍是旧的跳转（本轮就是这么误判了一轮）。
