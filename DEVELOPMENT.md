# ZizPanel 开发说明

> 这份文档是给**改这个项目的人**看的：设计取舍、测试策略、真机验证结论、
> 踩过的坑与修法。使用者请直接看 [`README.md`](README.md)。
>
> 文档分工见 `AGENTS.md` 第六节：
> `AGENTS.md` 放不变的规则；`ZizPanel-当前状态.md`（gitignored）放进度与残留；
> `README.md` 只放使用者需要的东西；**这份文件放开发过程本身**。

---

## 为什么这样设计

### 为什么用 Go 单二进制

目标机器是 macOS，用户不一定有 Go / Node / Python。发布一个**静态链接的单二进制**
意味着：没有运行时依赖、没有"先装 X 才能装 Y"、升级就是换一个文件。
前端内嵌进二进制（`//go:embed`），所以连静态资源目录都不需要。

### 为什么用 SQLite 单文件而不是 MySQL 或 JSON

JSON 的问题是并发写与原子性（面板是多用户 + 后台任务）。
MySQL 的问题是"为了管理系统先得装一个数据库"，而且面板挂了数据库也可能挂。
SQLite 单文件 + WAL 足够（写入量小、读多），备份就是复制一个文件。

### 为什么前端不引入框架和构建步骤

**没有构建步骤**是这个项目的一个硬约束：装完面板的机器上不该有 `node_modules`，
发布流程也不该依赖 `pnpm build` 能跑通。前端用原生 ESM + 一个极小的
`h()` 函数（见 `internal/web/assets/js/`），浏览器直接跑源码。
代价是手写响应式更新，收益是"改一行前端刷新即可、发布产物仍是一个二进制"。

### 为什么提权要单独做一个受限助手

面板主体以 root 跑（LaunchDaemon），但**不能**给网页一个通用 root shell ——
那等于把机器完全交出去。所以有 `zizpanel-helper`：只接受**白名单动作**
（写某个 plist、重启某个服务…），参数受校验，通过 sudoers 精确授权。
Web 终端是另一个开关（默认关闭），且用的是登录用户的 shell，不是 root。

---

## 开发

### 本地跑起来

```bash
make dev          # 构建到 dist/
make run-local    # 在 /tmp/zizpanel-dev 以调试模式启动（固定后缀 dev）
# 浏览器打开 https://127.0.0.1:18443/dev/
```

`make run-local` 用独立的数据目录与端口，不会碰到真实安装（`/opt/zizpanel`）。

### 测试策略

`make check` 是唯一的"提交前必须真绿"入口，包含：

- `gofmt` 检查 + `go vet` + 全部 Go 单测
- shell 变量扫描（`tools/check-shell-vars.py`）：历史上 `$VAR` 后紧跟中文标点
  会让 bash 把多字节并入变量名，`set -u` 下直接退出
- 前端语法检查（`tools/check-js-syntax.mjs`，走真正的 acorn 解析器）
- 接收端 `/jobs` 端到端测试（假上游，不碰真实服务）
- **测试污染检查**（`tools/check-test-pollution.sh`）：跑测试前后给真实目录拍指纹，
  防止单测写到用户的 `~/Library/LaunchAgents/`

几条铁律：

- **单测不许碰真实服务。** 本机 8880 上有 TtsVoice 的 Qwen（3 个模型），
  测试要用 `qwenPortOverride` 这类手段隔离。
- **测试不许碰生产配置。** 涉及 nginx vhost 的测试必须沙箱化，
  并断言生产的 `000-default.conf` 一字未变。
- **测试不许碰用户真实家目录。** `config.Default()` 解析的是真实家目录，
  测试服务器必须把 `UserHome`/`WWWRoot` 指向 `t.TempDir()`。

### 发布

```bash
make bump               # 0.8.3 → 0.8.4 … → 0.8.10 → 0.9.0
make check
make release            # 产出 dist/release/（两个架构 + 签名清单）
make publish-mirror     # 生成"指向国内镜像"的清单并打印上传命令
```

- `make release` 会把发布公钥注入二进制，并**运行产物核对公钥**
  （`-X` 在 `-trimpath` 下会静默失效）。
- 私钥在 `.release-key/`（gitignored）。丢了就再也签不出被已装面板接受的升级包。
- GitHub 与国内镜像需要**两份清单**：`manifest-github.json` 与 `manifest.json`
  （同一批包、同一把私钥，只有 url 不同）。国内无代理时 GitHub 直连不通，
  若清单里的 url 指向 GitHub，面板能读到清单却下不动包。

### 真机验证记录

- **Arm64 CPU 使用率**：`sysctl kern.cp_time` 在 Apple Silicon 上已移除 →
  改用 `top -l 1 -n 0` 解析，`kern.cp_time` 仅作兜底。
- **`ps` 的 `%CPU`**：fork `top` 的那一刻面板进程会被显示成 73.7%（实际 0.0%）→
  用两次 `cputime` 采样差值计算。
- **国内网络实测（无代理）**：GitHub Release 直连 20 秒 0 字节；
  自建镜像 ~550 KB/s 且会抖动（偶发 `Connection reset by peer`）；
  `ghfast.top` ~250 KB/s；`gh-proxy.com` ~72 KB/s；阿里云 brew bottle API 0.17s。
- **全新 macOS（抹机后的 mini 15.7.9）**：没有 CLT、没有 Homebrew、
  `/usr/bin/python3` 是占位程序；`softwareupdate -i` 装 CLT 15 分钟只下 1 MB。
  见下面的坑 #83。

---

## 已修复的典型坑（有测试锁死）

这些都是在真实机器上踩到并修复的，回归测试会拦住它们再次出现：

1. **Apple Silicon 上 `sysctl kern.cp_time` 已移除** → CPU 使用率恒为 0。
   改用 `top -l 1 -n 0` 解析，`kern.cp_time` 仅作兜底。
2. **`ps` 的 `%CPU` 不可信** → 面板进程被显示成 73.7%（实际 0.0%，
   因为那一刻它 fork 了 `top`）。改用两次 `cputime` 采样差值计算。
3. **`$VAR` 后紧跟中文标点** → bash 把多字节字节并入变量名，
   `set -u` 下报 `st?: unbound variable` 并直接退出。
   已加入字节级扫描器 `tools/check-shell-vars.py` 并接入 `make check`。
4. **`local` 声明写在循环体内** → 每次迭代重新声明并清空变量。
5. **受保护文件无法直接重定向覆盖**（sudoers 0440）→ 重装时权限拒绝。
   改用临时文件 + 原子替换。
6. **`status` 用硬编码配置路径** → 面板以 root 运行时 `$HOME` 是 `/var/root`，
   自定义根目录安装后 `zizpanel status` 误报"未初始化"，导致安装脚本永远认为服务没起来。
   改为跟随 `ZIZPANEL_ROOT`。
7. **macOS 上 `/var` 是 `/private/var` 的软链接** → 路径前缀校验误判"越界"。
   两侧都做 `realpath` 解析。
8. **CSRF 校验回退读 Cookie** → 双提交形同虚设（Cookie 是浏览器自动带的）。
   改为只认 `X-CSRF-Token` 请求头。
9. **panel 进程在 root 下把网站根目录算成 `/var/root/www`** →
   plist 显式传递 `ZIZPANEL_USER`。
10. **Homebrew 拒绝以 root 运行** → 安装脚本用 sudo 执行时，
    所有 `brew list/--prefix` 查询全部失败，于是误报"nginx/PHP/MySQL 都未安装"。
    改为 `sudo -u <真实用户> brew ...` 降权查询。
11. **`kill(pid, 0)` 探测 root 进程会返回 EPERM** → 普通用户跑 `zizpanel status`
    时明明服务在运行却报"已停止"。改用 `ps -p <pid>`，并增加 launchd 作为第二证据源。
12. **`mkcert -install` 在非图形会话下永久阻塞** → 会把安装卡死。
    加了 20 秒超时并回退到自签证书，同时明确告知用户手动执行一次。
13. **`conf.d` 被 http 级 include，但里面的 `php-fpm.conf` 含 `fastcgi_pass`**
    → nginx 直接无法启动。自动迁移到 `includes/` 并改写全部引用。
14. **反向代理的 `$connection_upgrade` 变量无处定义**
    → 用户一建反代站点，`nginx -t` 就报 unknown variable。
    面板负责写入 `conf.d/upgrade-map.conf`，并**反向验证**"缺 map 必须报错"。
15. **nginx 反代到"强制 HTTPS"的面板时用了 `http://`** →
    面板返回 400 `Client sent an HTTP request to an HTTPS server`。
    改为 `https://` + `proxy_ssl_verify off` + `proxy_ssl_server_name on`。
16. **子路径部署时 `proxy_pass` 未剥离前缀** → 后端收到 `/_panel/api/...`，
    返回 307 重定向。改用显式 `rewrite ... break` + 无 URI 的 `proxy_pass`。
17. **前端用绝对路径（`/app.css`、`/api/v1/...`）** →
    在 `/_panel` 子路径部署下全部 404。改为相对路径 + 301 补齐末尾斜杠。
18. **`h(spec, node)` 把 DOM Node 当成 props 对象，内容被静默丢弃** →
    弹窗内容整体消失且不报错。已修并在 UI 测试里加回归断言。
19. **浏览器的 `Element.append()` 会把 `null` 渲染成文本 "null"** →
    所有"条件渲染"的位置都会冒出字面量 `null`。统一过滤。
20. **SSE 进度流被 nginx 缓冲** → 任务中心里日志成批到达、看着像卡住。
    加 `X-Accel-Buffering: no` 与 `proxy_buffering off`。
21. **`launchctl load` 的退出码多次谎报成功** →
    改为 `launchctl print` 真查状态 + 端口探活，不看退出码。
22. **phpMyAdmin 的错误页也返回 HTTP 200** → 健康检查不能只看状态码。
23. **`-X` 在 `-trimpath` 下静默失效** → 发布包没内嵌公钥，退出码仍是 0。
    `make release` 现在**运行产物核对公钥**。
24. **`go build ... | head` 掩盖失败退出码** → 构建脚本里禁止这样写。
25. **面板进程重启后历史任务丢失** → 任务中心如实说"面板重启后不再保留历史任务"，
    而不是显示一个永远 0% 的僵尸任务。
26. **同步请求安装应用，用户一刷新就杀掉 brew/docker** →
    全部改成 `launchTask` 返回 202 + `task_id`，进度走 SSE。
27. **任务日志的命令标签是手写的** → 与 `cmd.Args` 实际执行的不一致
    （`runColima` 漏过 `sudo -n -u <user>`）。标签一律从 `cmd.Args` 派生。
28. **Colima 在非交互 SSH 里找不到 `docker`** → 面板代码里必须显式注入 PATH。
29. **Docker 源只用"第一个能应答的"** → 装到一半发现是慢源。
    改成**测速后排序**并把实测结果打进任务日志。
30. **镜像加速列表里有地址不可达** → 必须有"全部不可达"的如实报告。
31. **应用市场按 `platform: linux/amd64` 转译 arm64** → 明确禁止，
    有测试扫目录条目锁死。
32. **目录里声明了 Docker 路径但镜像没有 arm64 清单** → 测试直接失败，
    而不是等到用户点安装才失败。
33. **装完的应用没有卸载入口** → 按来源分三类（托管 / 面板安装器 / 纳管第三方），
    卸载前把"会执行什么、删哪些路径、保留什么"列进确认框。
34. **`VerifyUpdatesBlocked` 写死 `/usr/bin/softwareupdate`**（实际在 `/usr/sbin`）→
    必然失败，报的话还是错的（"阻断可能没生效"）。改为按候选路径解析。
35. **把"命令没跑起来/退出码非 0"当成"结论是否定"** →
    `softwareupdate --list` 在阻断生效时打印"错误的URL"但退出码 0，
    于是"阻断有效"被判成"失效"。抽出 `judgeSoftwareupdate` 纯函数分开报。
36. **phpmMyAdmin 显示"已安装·服务未注册"** → 它本来就没有守护进程。
    目录里标 `NoDaemon`，界面改说"已安装·网页入口"。
37. **回滚把用户原本的选择改掉了** → `RestoreUpdates` 早期一律把偏好写成默认值。
    现在按快照逐项恢复。
38. **多站点共用一个音色样本：后传的覆盖先传的** → 样本按来源分目录，
    上传按魔数 + ffprobe 校验并统一归一化。
39. **上游返回 HTTP 200 + 0 字节**（mlx-audio 按扩展名选解码器）→
    接收端必须自己校验音频内容，不看上游状态码。
40. **接收端把"最后一个未完成的 chunk"也算进用量** →
    改成提交时**预留**、终态**结算**，失败的作业不吃额度。
41. **密钥与额度没有热加载** → 改文件后必须重启才能生效，
    用户在面板里加了密钥却不生效。改为按 mtime 热加载 + 面板侧提示。
42. **不允许停用/删除最后一条启用的密钥** → 那等于一键让所有网站 401。
43. **默认音色要"不覆盖用户自己的样本"** → 只在 `default/ref.wav` 不存在、
    或存在且带 `builtin-voice.json` 标记时才写入。
44. **TTS 安装要 `HF_HUB_DISABLE_XET=1`** → 否则新版 huggingface_hub
    会去连 hf-mirror 不代理的 Xet 后端。
45. **Lucky 的「编辑配置文件」指向不存在的 `lucky.conf`** →
    它的配置是加密的 `lucky_*.lkcf`，文本编辑器打开只有乱码。
    条目不再声明 `ConfigPath`，可视化配置走它自带的 Web UI。
46. **健康检查的 URL 只填不清** → 应用换了端口后旧地址一直亮红灯。
    改为双向 reconcile（填 **和** 清）。
47. **frpc 在没有服务端时退出**（`loginFailExit` 默认 true）→
    生成的 `frpc.toml` 明确写 `loginFailExit = false`。
48. **Lucky 的"安全入口"/IP 白名单会对所有路径返回 404** →
    条目不能声明 `HealthPath`，否则永远红灯。
49. **`install.sh` 里"探目录 HEAD 失败"就转去源码构建** → 普通用户机器上没有 Go，
    等于装不上。改为**探真实 tarball 地址**，官方不通就用内置镜像。
50. **测试夹具里复制了真实口令与令牌** → 历史提交里也有。
    已改成假值，并用 `git filter-branch` + 过期 reflog 清掉历史与 blob。
51. **`install.sh` 在 `sudo` 下找不回真实用户** → `$HOME` 是 `/var/root`。
52. **面板是 LaunchDaemon，读不到用户 shell 的 rc** → 国内镜像必须由面板
    自己注入环境变量（`HOMEBREW_API_DOMAIN` 等），不能指望用户的 `.zshrc`。
53. **`sudo` 会清空环境** → `brewRun` 必须在 `sudo -u` **之后**再用 `env` 注入。
54. **macOS 防火墙会静默拦截面板端口** → 安装时加入允许列表并校验。
55. **删除 `/opt/zizpanel` 的数据目录不影响已注册的 LaunchDaemon** →
    卸载流程要显式 bootout，且区分"卸载保留数据 / 彻底删除"。
56. **`000-default.conf` 的迁移只做了一半** → 站点目录与 include 目录对不上，
    nginx -t 通过但站点 404。
57. **端口冲突（Stirling PDF 8082）** → 探测失败时报告"端口被谁占用"，
    而不是只说"启动失败"。
58. **Squoosh 是第三方镜像的页面** → 目录里标注来源，不假装是自研。
59. **一键建站（Typecho/WordPress）的 vhost 未生效** → 见当前状态里的残留项。
60. **备份包含口令却没提示** → 导出前明确列出包含哪些敏感文件。
61. **`receiver /jobs` 的测试用了真实端口** → 改成假上游 + 临时目录。
62. **接收端的测试让真实的 receiver 进程受影响** → 测试隔离到临时 key/usage 文件。
63. **单测污染真实 `~/Library/LaunchAgents/`** → 护栏测试
    `TestTestServerSandboxedAwayFromRealHome` + `make check` 里的指纹检查。
64. **`present` 的文件没写全就宣称交付** → 交付前逐项核对真实状态。
65. **面板升级后旧进程还在跑** → apply 后必须等新版本健康检查回来（看版本号）。
66. **健康检查的版本号是唯一的"真的换了二进制"证据** → 只看端口通了不够。
67. **`launchctl` 的 `RunAtLoad` 与 `KeepAlive` 都要有** → 少了 KeepAlive
    崩溃后不会自己起来；少了 RunAtLoad 开机不自启。
68. **`sudoers` 的权限必须是 0440 且属主 root:wheel** → 否则 sudo 直接拒绝加载。
69. **面板证书到期时间要如实显示** → 自签 10 年也要写清是自签。
70. **`mkcert` 没装时不能反复尝试调用** → 每次安装都等 20 秒超时会拖慢。
71. **前端 `node --check` 放过了浏览器拒绝的语法** → 整页白屏且不给行号。
    必须用真正的 ES 解析器（acorn）。
72. **`present` 之外的"文件已生成"都不算交付** → 必须在最终回复里点名文件。
73. **`install.sh` 用 `curl | sudo bash` 时 `$0` 不可用** → 脚本不能靠 `$0` 找自己。
74. **`install.sh` 的下载失败要重试并换源** → 一次抖动就失败会反复困扰用户。
75. **面板的"打开"按钮给出的是不通的入口** → 探测失败时把「直连端口」作为首选，
    而不是给一个点开是错误页的按钮。
76. **"没有 X"不等于"X 坏了"** → 先问"这个应用本来该有 X 吗"（phpMyAdmin 无守护进程）。
77. **"纳管"不等于"用户装的"** → 面板自己的安装器也在用 `Adopted`，
    判断能否卸载要看 `PanelInstaller`。
78. **找不到命令 ≠ 结论是否定** → 分开报（见 34/35）。
79. **`limit_conn`/`limit_rate` 不是慢的唯一原因** → 先用测速把"谁慢"钉死。
80. **回滚要按快照逐项恢复**（见 37）。
81. **测速排序别用 `sort.Strings`** → 这类"顺序有语义"的配置要显式排序并记录理由。
82. **"共享一份可变文件"的接口迟早出事** → 按来源分目录（见 38）。
83. **"无界面安装开发者工具"在真机上根本不成立，而任务日志只说"没成功"** →
    抹机后的 mini 上装音色接收端，卡在 CLT 这一步：面板先按 `touch` 标记 +
    `softwareupdate -l` 找到 `Command Line Tools for Xcode-16.2`，
    再 `softwareupdate -i` —— 真机实测 **15 分钟只下了 1 MB 然后彻底停住**
    （`swdist.apple.com` 通但极慢，`updates.cdn-apple.com` 还被 DNS 解析到
    假地址 `198.18.0.67`）；失败后回退到 `xcode-select --install`，
    那是个**图形对话框**，无人值守的远程安装就永远停在那里。
    更麻烦的是第二层：任务日志里只写"softwareupdate 这条路没成功"，
    **苹果那侧到底说了什么一个字都没有**，真机上完全无法判断是 DNS、证书还是传输中断。
    修法（0.8.8）：给苹果原始 pkg 加一条**镜像整包安装**的路（`clt/index.json` +
    `clt/<产品号>/*.pkg`），顺序改为 **镜像 → softwareupdate → 弹窗**；
    两条苹果路径的**真实输出**都写进任务步骤；成功判定从
    `xcode-select -p` 改成 `python3Works()` —— 因为全新 macOS 上
    `/usr/bin/python3` **存在**，只是跑起来会打印一句提示并弹对话框。
    教训：**"我们找到了官方静默开关"和"这个开关在这条网络上能用"是两件事** ——
    前者能在单测里证明，后者只能在真机、真网络、真等待里证明；
    而失败时必须把**被调用方的原话**带回来。

---

84. **"降权运行"和"需要 root"在无终端环境里会互相锁死** →
    Homebrew 安装脚本：EUID=0 时**拒绝安装**；降权后它自己又会调
    `sudo install -d -o <user> /opt/homebrew`；而面板是 LaunchDaemon，
    sudo **没有终端可以读密码** —— 三个条件同时成立，必然失败，
    而且失败信息只有一句 `sudo: a terminal is required to read the password`。
    修法（0.8.9）：安装期间放一个只作用于安装脚本 PATH 的 `sudo` 垫片
    （有 `-u` 就去掉后原样执行、没有 `-u` 就清 `HOME` 后以面板用户执行），
    装完立即删除，**不动系统 sudoers**；同时把安装脚本输出末尾带进任务日志。
    教训：**"不许以 root 跑"的工具 + "面板没有终端"是一个组合陷阱** ——
    遇到"必须以某个用户跑、但它自己又要提权"的安装器，先问一句
    "它到底哪几步需要 root，能不能用身份变换替掉"。

85. **大文件下载"断了就重来"在真机链路上等于永远装不完** →
    576 MB 的 CLT 包在真机实测中偶发 `Connection reset by peer`，
    而当时的重试是**从 0 开始**（已下的 200 多 MB 全废）。
    修法（0.8.9）：镜像上切 32 MB 分片，面板并行下载 + 逐片校验 +
    断点续传（已下好且大小对的分片直接跳过），拼接后仍核对整体 sha256
    （分片校验说明不了顺序对不对）。清单里没有 `parts` 时自动退回单文件下载。
    教训：**跨公网传大文件时，"重试"必须和"续传"成对出现** ——
    只重试不续传的脚本，在慢链路上是"越试越慢"的。


---


## 系统设置（把 macOS 配成服务器）

「系统设置」页把一堆需要终端操作的事做成了开关：合盖不睡、断电自恢复、
关闭 Spotlight 索引、调整 TCP 参数、开启远程登录等。**每一项都先探测"这台机器
支不支持"**，不支持就如实报"不支持"，绝不谎报成功。

历史上最危险的一次：`pmset -a autorestart 1` 在不支持的机器上
**静默返回成功**却什么都没做，脚本却报告"已开启断电自恢复" ——
真跳闸那天才会发现。**能谎报成功的功能，比没做更糟。**

`tools/system-services.sh` 与 `tools/server-mode.sh` 里的每一步都有
"做了 / 跳过 / 失败"三态，且被 `make smoke` 覆盖（特权步骤跳过并单独列出）。

---

## 服务管理与应用市场（实现细节）

### 两种管理模式的严格区分

| 模式 | 含义 | 面板能做什么 |
|------|------|--------------|
| **托管（Managed）** | 面板安装并登记的服务 | 启动/停止/重启/卸载/改配置 |
| **纳管（Adopted）** | 机器上本来就有的服务 | 只看状态、启停；**不卸载** |

### 三种驱动

`brew`（原生）、`docker`（容器）、`launchd`（自定义 plist）。
目录条目声明用哪一种，界面按驱动给出对应的操作。

### Docker 管理页

容器/镜像/卷/网络、Compose 编辑器与一键部署、创建容器。
跑在 Colima 上（不是 OrbStack：Colima 是纯 CLI、可无头运行，
OrbStack 需要 GUI 应用与商业许可）。

### 应用市场

**选品原则：能原生就原生，不能原生再 Docker，且 Docker 镜像必须自带 `linux/arm64`。**
原生 = Homebrew formula 或官方 darwin-arm64 预编译产物。
**不许 `platform: linux/amd64`、不许 Rosetta 转译**（有测试锁死）。

穿透/反代类（frps / frpc / Lucky / Orbien 服务端+客户端）全部走
`InstallReleaseBinary`：官方 darwin-arm64 tarball + launchd，
可选 SHA-256 校验、`file(1)` 复核架构、生成配置。

---

## Qwen3 TTS（实现细节）

只有一个模型（1.7B-Base-8bit，音色克隆）。安装走清华 PyPI +
`HF_ENDPOINT=https://hf-mirror.com` + `HF_HUB_DISABLE_XET=1`。
需要 Homebrew 的 `python@3.11`，而 Homebrew 需要 CLT —— 整条链由面板自己装。

Qwen 的依赖很小（`requirements-min.txt`），不含 mlx 全家桶；
音色来源：网站上传的样本、面板内置默认音色、或请求里显式给的 `ref_audio`。

### 接收端（TtsVoice 音色接收端）

- 多密钥（`keys.json`，按 mtime 热加载），每把密钥独立的字符额度与用量。
- 额度按**实际合成完成**的字数计（提交时预留、终态结算）。
- 访问日志记录每一个非 2xx，便于网站侧排查 401/403。
- 样本按来源分目录：`<samples>/<source>/ref.wav` + 可选 `ref.txt`。
- 内置默认音色写在 `default/ref.wav`（带 `builtin-voice.json` 标记，
  不覆盖用户自己的样本）。
- 调用方省略 `ref_audio` 且 `source` 为空或 `default` 时用内置音色；
  显式 `ref_audio` 优先；**没有旧版根目录回退**（宁可明确报错）。

---

## 国内网络：安装源与镜像（实现细节）

中国大陆**无代理**时 GitHub Release 直连**完全不通**（实测 20 秒 0 字节），
所以安装与升级都要能走国内源：

- `install.sh` 会**先探真实 tarball 地址**：官方源不通就自动改用内置镜像
  （`https://zizdog.com/zizpanel`）。也可以显式指定
  `--download-base <镜像>` 或 `ZIZPANEL_DOWNLOAD_BASE=<镜像>`。
- 面板跑 brew 时注入国内镜像（阿里云的 `HOMEBREW_API_DOMAIN` /
  `HOMEBREW_BOTTLE_DOMAIN`，实测 0.17s）并关掉自动更新。
  用户自己设过的以用户设置为准。
- 市场里从 GitHub Release 下载的应用会**先测各源速度再按快的排**。
- Qwen3 TTS 走清华 PyPI + `hf-mirror.com`。
- **命令行开发者工具（CLT）也整包走镜像**（0.8.8 起，见坑 #83）。

镜像目录约定：

```
<mirror>/
  manifest.json          升级清单（url 指向镜像本身，不是 GitHub）
  manifest.json.sig      Ed25519 签名
  install.sh             一键安装脚本
  download/<版本>/…       该版本的包
  download/latest/…      最新包（固定 URL）
  clt/index.json         CLT 清单
  clt/<产品号>/*.pkg     苹果原始 CLT 包
```

`make publish-mirror` 会生成指向镜像的清单并打印上传命令。
`ZIZPANEL_CLT_MIRROR=<地址>` 可临时覆盖 CLT 镜像地址（测试用）。

---

## 应用界面：一键「打开」与子路径代理

有界面的应用给**两个入口**，用哪个由探测说了算：

1. 直连端口（如 `http://<ip>:16601`）—— 应用自己的界面，最完整。
2. 面板子路径（如 `https://<ip>:8443/<slug>/`）—— 共用面板的 HTTPS，
   但需要应用支持子路径。探测不通就不给这个按钮。

`AppProxyAuth`（默认开）控制"子路径界面是否要求先登录面板"，
因为 Squoosh 这类完全没有自己的鉴权。

---

## 卸载：三种语义，绝不动用户自己的东西

| 动作 | 删除什么 | 保留什么 |
|------|----------|----------|
| 停止服务 | 只停进程 | 全部文件 |
| 卸载应用（面板装的） | 二进制、plist、面板生成的配置 | 用户自己改过的配置与数据 |
| 卸载第三方（纳管的） | 只注销登记 | 一切文件 |

卸载前把"会执行哪些步骤、会删哪些路径、会保留什么"列进确认框。

---

## 文件管理与 Web 终端

文件管理器：多根目录、在线编辑器（代码高亮 + 两种全屏）、
上传下载、权限与属主修改、压缩解压。路径校验两侧都做 `realpath` 解析
（macOS 上 `/var` 是 `/private/var` 的软链接）。

Web 终端默认**关闭**（等于把 shell 交给浏览器，必须显式开启），
可设空闲超时与最大会话数。

---

## 计划任务与备份

用 launchd 而不是 crontab：macOS 上 crontab 需要额外授权、
且**合盖/休眠后不会补跑**。面板把 cron 表达式翻译成 launchd 的
`StartCalendarInterval`，并如实报告"这次翻译支持哪些写法"。

备份是打包配置与数据（含口令文件），导出前明确列出包含哪些敏感文件。

---

## 与现有环境的关系

ZizPanel 设计成**不接管**已有的环境：

- 只负责自己安装的服务（`/opt/zizpanel`、自己的 plist、自己的数据目录）。
- 机器上已有的 nginx / PHP / MySQL 走"纳管"：能看状态、能启停，不改它的约定。
- 站点与 vhost 只改面板自己管理的 include，不重写全局 nginx 配置。
- `~/www/zizdog.cn` 属于另一个项目（TtsVoice），面板只登记它的 vhost，
  **不改它的代码、配置、数据库**。

86. **"往 PATH 里塞一个 sudo"治不了硬编码路径的官方脚本** →
    0.8.9 加了 `sudo` 垫片想让降权后的安装脚本借到 root，真机复跑时
    日志里那一行**一个字符都没变**：Homebrew 的安装脚本用的是
    `/usr/bin/sudo`（它自己的 `execute_sudo()` 写死了绝对路径），
    而且开头先 `sudo -n -l mkdir` 探权限，不通就
    `abort "Need sudo access on macOS"`。**垫片不是没用，是用错了地方。**
    修法（0.8.10）：安装期间临时给面板用户免密 sudo
    （专用文件名 + 0440 + root + 先 `visudo -c` + 绑定具体用户 +
    无论成败都 `defer` 撤销 + `sudo -k` 清时间戳），并把授权/撤销都写进任务步骤。
    教训：**先确认"我拦的那条路径是不是真的被走到"** —— 我第一版是在
    "看起来能被 PATH 影响"的假设上加的代码，而这类假设在真机上一行日志就能推翻。
    另一条：往用户机器上写 `NOPASSWD` 属于"离危险最近"的改动，
    必须限定文件、限定用户、限定时间，并且**先校验语法再启用**。

87. **`make release` 的两次构建产物哈希不一致**（0.8.8 首次发布时观察到）→
    第一次构建与"改了 README 再构建"的 sha256 不同。原因是
    `-ldflags` 里注入了 `BuildTime`，所以每次构建本来就不该期望字节一致。
    教训：**要复现的构建就得把时间戳排除在外**；现在发布时以
    `manifest.json` 里记录的 sha256 为准，不靠"记住上次的哈希"。

88. **"启动时探测一次"的路径配置，会在"先装面板、后装依赖"的机器上永久错下去** →
    真机：Homebrew 装好了、`brew --version` 也通了，面板却报
    `env: /usr/local/bin/brew: No such file or directory`。
    因为 `BrewPrefix` 是**面板第一次启动那一刻**探测的 —— 全新机器上那时
    `/opt/homebrew` 还不存在，于是退回 `/usr/local` 并**持久化进 config.json**，
    此后永不自我修正。
    修法（0.8.11）：`Config.ReconcilePaths()` 在每次加载配置时重新探测
    （只认真实存在的 `bin/brew`），把**所有派生路径一起**改，
    有变化才写回；用户自定义且真实可用的前缀不动；没装 Homebrew 时不瞎改。
    教训：**凡是"探测一次然后存起来"的环境信息，都要问一句
    "这个环境会不会在本程序运行期间发生变化"** —— 会变就必须有校正路径。
    这类 bug 的症状（找不到文件）和根因（配置在某个时刻被定死）离得很远。

90. **"换一个客户端就好了"是网络故障里最会误导人的一类** →
    Qwen 模型下载反复 `Local entry not found ... Operation timed out`，
    而同一个 URL 用 `curl` 走 IPv4 是 5 MB/s。根因是这台网络会把域名解析到
    **不可达但也不拒绝**的 IPv6 地址，Python 客户端一直挂在 `connect()` 上；
    `curl` 默认先试 IPv4，所以完全看不出问题。
    修法（0.9.1）：给 Qwen 的 venv 写 `sitecustomize.py` 把
    `socket.getaddrinfo` 限制到 `AF_INET`（真机复测 14 个文件 6 分钟下全）。
    为什么不是 `PYTHONSTARTUP`：它**只在交互式解释器里生效**，
    `python script.py` 下不执行（实测）。
    教训：**同一个 URL 用两个客户端表现不同时，先怀疑解析/连接策略，别先怀疑服务端**；
    以及"修好了"要用**真机复测**证明，而不是"这段解释听起来对"。

---

## 应用包镜像（apps/）

**需求（2026-09-16，用户明确要求）**："所有安装过程先检查镜像站的资源能不能访问，不能再走其它。"

布局（`internal/services/mirror.go`、`tools/sync-nas-apps.sh`、`cmd/zizpanel-assets` 三方一致）：

```
<镜像基址>/apps/<应用 ID>/<版本>/<原始文件名>
<镜像基址>/apps/<应用 ID>/<版本>/manifest.json      # 每个包的 sha256/大小
```

- 镜像基址在「面板设置 → 应用包镜像基址」里配（`Config.MirrorBase`，默认
  `https://mirror.zizdog.com:8888`）。**留空 = 关闭镜像**（各来源回到内置的公网/国内镜像，
  仅用于镜像站故障时应急）。
- **语义是"唯一来源"，不是"加速源之一"**：安装前先 HEAD 包与清单
  （`preflightMirrorAsset`），缺任何一个就明确失败并提示 `make sync-apps`，
  **不回退 GitHub**。回退会让"这台机器到底能不能装"变得不可预测 —— 这正是用户要求避免的。
- 校验用**镜像清单**里的 sha256（`verifyMirrorChecksum`）。比原来只对 frp 校验更严：
  Lucky / Orbien 上游根本没有 checksums 文件。
- 探测超时 `Config.MirrorProbeSeconds`（默认 4 秒）：镜像不可达时不让安装白等。

### 新增一个应用到镜像

1. 在 `internal/services/binary_release.go` 的 `releaseBinaryApps` 里加条目（唯一事实来源）；
2. `make sync-apps SYNC_ARGS=--dry-run` 看计划（应用/版本/文件名都从注册表导出），
   确认后 `make sync-apps NAS_PASS='...'` 真同步；
3. NAS 上确认 `<apps-root>/<应用>/<版本>/` 下有包与 `manifest.json`。

**不要**在同步脚本里手抄版本号：脚本从注册表读，手抄的那份一定会漏，
而漏掉的后果是"镜像上没有这个包"，按设计那会**直接让安装失败**。

### 还没接镜像的来源（下一步）

`mirrorSubPath()` 已备好约定：pip → `<基址>/pypi/simple`、HF → `<基址>/hf`、
brew → `<基址>/brew`。**在 NAS 上把这三条反代配好之前不要接**：接早了就等于
"NAS 上没有就让 Qwen / IOPaint 装不上"。它们当前仍走内置的国内镜像
（清华 pypi / hf-mirror / 阿里云 brew）。

### 典型坑：**"磁盘上有产物"不等于"已安装"**（2026-09-16 用户反馈，有测试锁死）

应用市场原来把 `InstallerArtifactExists`（磁盘上有产物）直接算成 `installed=true`。
后果：卸载时没勾"删除数据"（或用户手动删了服务、目录还在）之后，卡片永久停在
"已安装·服务未注册"，前端只给「重新部署」，**没有「安装」入口 —— 用户无法重装**。
现在：`installed` 只认服务记录 / launchd 里的 plist / brew formula；产物单独用
`artifacts` 报出来，前端显示「残留数据」+「安装」+「删除残留数据」。
回归测试：`TestMarketResidualDataOffersReinstall`、
`TestMarketDetectsOrphanInstall`（旧断言正是这条 bug 的规则化，已改锁新契约）。
