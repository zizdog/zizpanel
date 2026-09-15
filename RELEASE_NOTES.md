v0.9.4 · 应用包镜像：安装前先检查镜像站，镜像上没有就明确失败（不再走 GitHub）

**需求（用户明确要求）**："所有安装过程先检查 https://mirror.zizdog.com:8888 的资源能不能访问，不能再走其它。"

- 新增「应用包镜像基址」配置（设置页可改可清空，默认 `https://mirror.zizdog.com:8888`）+
  探测超时（默认 4 秒）。**留空 = 关闭镜像**（回到内置的公网/国内镜像，仅用于镜像站故障时应急）。
- 布局固定为 `<基址>/apps/<应用>/<版本>/<文件名>`，同目录放 `manifest.json`（sha256/大小）。
- 安装 Lucky / frps / frpc / Orbien 之前先 HEAD 包与清单：**缺任何一个就明确失败**并提示
  `make sync-apps`，不回退 GitHub —— 回退会让"这台机器到底能不能装"变得不可预测。
- 校验改用镜像清单里的 sha256（比原来只对 frp 校验更严：Lucky / Orbien 上游没有 checksums 文件）。
- 新增 `tools/sync-nas-apps.sh` + `make sync-apps` + `cmd/zizpanel-assets`：把 5 个应用包同步到
  NAS；应用与版本从代码注册表导出，不手抄。脚本已纳入 `make check` 门禁（bash -n / 多字节变量 / shellcheck）。
- `internal/services/mirror_test.go`：5 条测试锁住布局、镜像唯一性、缺资源时的报错文案与 sha256 校验。

**尚未接入**：pip / HF / brew 三个来源仍走内置的国内镜像 —— 等 NAS 上把 `/pypi`、`/hf`、
`/brew` 反代配好再接（接早了会让 Qwen / IOPaint 直接装不上）。
**尚未部署**：本机与 mini 都还是 0.9.2，镜像功能要下次发布才生效。

v0.9.3 · 应用市场：卸载（保留数据）之后必须能重装

- 根因：后端把"磁盘上有安装产物"也算成已安装，于是卸载（保留数据）之后卡片永远停在
  "已安装·服务未注册"，既没有「安装」入口也没法重装（用户原话："卸载完成后连安装的
  入口都没有，用户怎么重装"）。
- 修法：`installed` 只认服务记录 / launchd plist / brew formula；产物单独用 `artifacts` 报出来，
  卡片对"残留数据"给「安装」+「删除残留数据」；卸载任务的成败显式提示并立即刷新列表。
- 回归测试：`TestMarketResidualDataOffersReinstall`（新增）、
  `TestMarketDetectsOrphanInstall`（旧断言正是这条 bug 的规则化，已改锁新契约）。

---

v0.9.1 · 模型下载卡住的真因：这台网络会解析出"不可达但也不拒绝"的 IPv6 地址

**现象（抹机后的 mini）**：Qwen3 TTS 的 14 个模型文件下载反复失败，报

```
Error: Local entry not found. [Errno 60] Operation timed out
```

重试 3 次都一样。但同一个 URL 用 `curl` 走 IPv4 是 **5 MB/s**。

**根因**：这台网络里的域名会解析出 IPv6 地址，而那条 IPv6 路径
**不可达、却也不立刻拒绝**（不是 connection refused，是干等），
Python 客户端就一直挂在 `connect()` 上直到超时。
`curl` 默认高兴地先试 IPv4，所以看不出问题 —— 这类故障"换一个客户端就好了"，
最容易误判成"服务端不稳"。

**修法（0.9.1）**：给 Qwen 的虚拟环境写一份 `sitecustomize.py`，
把 `socket.getaddrinfo` 限制到 `AF_INET`。真机复测：**14 个文件一次下全（6 分钟）**。

为什么是 `sitecustomize` 而不是 `PYTHONSTARTUP`：后者**只在交互式解释器里生效**
（实测 `python script.py` 下根本不执行），而 `sitecustomize` 是 `site` 模块
启动时自动 import 的，对 `hf` 这种控制台入口脚本一定生效；
放在 venv 自己的 `site-packages` 里，只影响这个虚拟环境，不动系统 Python。

补丁有单测：真的用 `python3 -m py_compile` 编译一遍，并在子进程里断言
`getaddrinfo` 被限制到 IPv4 —— 因为 `sitecustomize` 导入失败只打一句警告，
太容易被忽略。

---

v0.9.0 · Homebrew 装上了但面板找不到它："面板先于 Homebrew 存在"写下的错路径不会自我修正

**真机（抹机后的 mini）进展**：0.8.10 的临时免密 sudo 生效了 ——
Homebrew 的官方安装脚本这次**真的把 brew 装进 /opt/homebrew**（实机确认
`/opt/homebrew/bin/brew --version` → `Homebrew 7.0.2`），
安装结束后的 sudoers 也干净（`/etc/sudoers.d/` 里只剩面板自己那条）。

**但它紧接着报了另一个错**：

```
Homebrew 装完了但执行不了：env: /usr/local/bin/brew: No such file or directory
```

根因不在 Homebrew，在**面板自己**：`BrewPrefix` 是面板**第一次启动那一刻**
探测出来的 —— 全新机器上那一刻 `/opt/homebrew` 还不存在，于是它退回了
`/usr/local`，并且**把这个值持久化进了 `config.json`**。
之后不管装多少次 Homebrew，配置里的路径都不会自我修正。

**修法（0.9.0）**：
- `Config.ReconcilePaths()`：每次加载配置时做一次很便宜的校正
  （两次 `stat`，只认真实存在的 `bin/brew`），有变化才写回文件；
- 由 Homebrew 前缀推导出来的路径（`BrewBin / NginxBin / NginxConf / VhostDir /
  PHPEtc / MySQLBin / PmaDir`）**一起**改 —— 只改前缀不改派生路径，
  症状会是"某个功能莫名找不到文件"，更难查；
- 用户手工填了自定义前缀、而且那个前缀**真实可用**时，**不动它**；
- 机器上完全没装 Homebrew 时不瞎改（返回 false，不写盘）。
- `config.Load` 与面板每次构造服务管理器时都会走这一步。

有单测锁住"过期前缀 + 派生路径 + 写回文件 + 幂等 + 不瞎改"这五种情形。

---

v0.8.10 · Homebrew 的卡点找到了：官方安装脚本硬编码 /usr/bin/sudo，PATH 垫片拦不住

**这一版是"上一版没解决、在真机上再试一次并找到根因"的结果。**

0.8.9 我加了一个 `sudo` 垫片（往 PATH 里塞一个脚本），以为能拦下降权后那几步 root 操作。
真机复跑后，任务日志里那一行**一个字符都没变**：

```
==> /usr/bin/sudo /usr/bin/install -d -o root -g wheel -m 0755 /opt/homebrew
sudo: a terminal is required to read the password
```

原因：**Homebrew 官方安装脚本硬编码调用 `/usr/bin/sudo`**（它自己的 `execute_sudo()`），
根本不看 PATH。而且它开头就用 `sudo -n -l mkdir` 探一次权限，不通就直接
`abort "Need sudo access on macOS"`。

**修法（0.8.10）**：安装期间**临时**给面板用户免密 sudo，装完立即撤销：
- 写在专用文件 `/etc/sudoers.d/zizpanel-homebrew-install`，不碰用户自己的 sudoers；
- 权限 0440 + root 拥有；**先 `visudo -c` 校验再启用**（写坏 sudoers 会让整个系统的 sudo 失效）；
- 授权内容绑定到具体安装用户（不是"所有人"）；
- 无论成功失败都 `defer` 撤销，撤销时用 `sudo -k` 清掉已缓存的时间戳；
- 授权与撤销都写进任务步骤，用户看得见"这期间发生了什么"。

事实前提：面板用户本来就是管理员、本来就拥有 `(ALL) ALL`（真机 `sudo -n -l` 确认），
这里改的只是"安装期间不必输密码"，**不改变长期提权策略**。

垫片保留为第二道保险（让 `sudo -u <user>` 保持"降权"的本意），
`HOMEBREW_ALLOW_ROOT=1` 作为第三道。三道都不成立时才会失败，且失败时会
把安装脚本**输出末尾 800 字**带进任务日志。

---

v0.8.9 · 全新安装跑通了：Homebrew 在"面板里没人能输密码"的环境下也能装 + CLT 走分片并行下载 + README 重写

**这一版是抹机后的 mini 上真机跑出来的结论**，三处都是"只在真机上才会暴露"的问题：

**1. Homebrew 安装必失败**（真机复现两次）
- 现象：CLT 装好之后，Homebrew 安装脚本走到
  `/usr/bin/sudo /usr/bin/install -d -o root -g wheel /opt/homebrew` 就死，
  报 `sudo: a terminal is required to read the password`，任务里只剩一句"安装 Homebrew 失败"。
- 根因是三个条件同时成立：Homebrew **拒绝以 root 运行**（必须降权）、
  降权后它自己又有几步要 root、而面板是 LaunchDaemon **没有终端可以输密码**。
- 修法：安装期间放一个只对安装脚本生效的 `sudo` 垫片（`brewSudoShim`）——
  带 `-u` 的调用拿掉 `-u` 原样执行（脚本本意就是降权），
  不带 `-u` 的清掉 `HOME` 后以面板用户执行（建目录这类操作结果一样），
  再叠加 Homebrew 自己的 `AllowRoot`；**装完立即删除**。
  不动的系统的 sudoers，不改变用户机器的提权策略。
- 失败时把安装脚本**输出末尾 800 字**带进任务步骤（以前只有一句"失败"）。

**2. CLT 大包下载会抖动，断一次就前功尽弃**
- 真机实测：576 MB 单连接约 460 KB/s，中途出现 `curl: (56) Recv failure: Connection reset by peer`，
  而那一次重试是**从 0 开始**（已经下掉的 200 多 MB 全废）。
- 修法：镜像上把大包切成 32 MB 分片（`clt/index.json` 的 `parts`），
  面板**4 路并行 + 逐片校验 + 断点续传**：已下好且大小正确的分片直接跳过，
  断一片只重下那一片；拼回后仍然核对**整体** sha256（分片校验说明不了顺序对不对）。
  清单没给 `parts` 的包自动退回单文件下载，所以镜像可以分阶段升级。

**3. README 重写**
- 之前 1700 多行，把设计论证、测试策略、80 多条坑清单全塞在一起，
  第一次来的人找不到"我该执行哪一行"。
- 现在 `README.md` **只讲安装与使用**：开头就是那一行安装命令，
  然后明确写出"安装过程中可能需要点一次哪里"（CLT 弹窗、可选的可信证书）。
- 开发过程、设计取舍、测试策略、坑清单全部搬到 `DEVELOPMENT.md`。

---

v0.8.8 · 全新安装不再需要代理、也不需要在 Mac 上点任何弹窗：命令行开发者工具改走镜像整包安装

**问题（抹机 mini 实测）**：0.8.7 之前"无界面装 CLT"依赖苹果自己的通道，
而这条通道在国内无代理时不通 ——
`softwareupdate -i "Command Line Tools for Xcode-16.2"` **15 分钟只下了 1 MB 然后停住**
（`swdist.apple.com` 通但极慢，`updates.cdn-apple.com` 的 DNS 还被解析到假地址
`198.18.0.67`）；失败后回退到 `xcode-select --install`，那是个**图形对话框**，
无人值守的远程安装就永远停在那里。而全新 macOS 上 TTS 两件套都要真 Python
（接收端 plist 是 `/usr/bin/python3`，Qwen3 TTS 要 venv + pip）——
于是"输入一次密码后全程自动完成"在最后一步断掉。

**修法**：把苹果**原始**的 CLT 安装包放到项目自己的国内镜像上，按
**镜像整包 → softwareupdate → 弹窗** 的顺序安装，三条路都试过才算失败：
- 清单 `https://zizdog.com/zizpanel/clt/index.json`：每项给
  `name` / `path`（或 `dir`）/ `max_os`（**上限**）/ `pkgs` / `bytes` / `sha256`。
  面板按 `sysctl -n kern.osrelease` 推 macOS 主版本（24 → 15）来挑，**排在前面的优先**，
  所以"在哪台机器上成功过的条目"往前排。
- 只下白名单里的两个包：`CLTools_Executables.pkg`（604 MB，`python3`/`clang`/`git`）
  与 `CLTools_macOSNMOS_SDK.pkg`（55 MB，macOS SDK）。苹果同一产品里还有
  `SwiftBackDeploy` / `LMOS_SDK` / `DevSDK_Remove_*`，全下要多 105 MB。
- `sha256` 校验（清单提供就校验）、`curl -fL --retry-all-errors`、
  失败删半成品、装完删 `/tmp` 里的包（省 660 MB）。
- 成功判定改成**真的跑一次 `/usr/bin/python3`**：全新 macOS 上这个路径**存在**，
  只是跑起来会打印一句提示并弹对话框 —— 只看 `xcode-select -p` 会误判成功。
- 两条苹果路径的**真实输出**都写进任务步骤（以前只写"没成功"，
  真机上完全无法判断是 DNS、证书还是传输中断）。
- 自建镜像照同样的目录结构放文件即可；`ZIZPANEL_CLT_MIRROR=<地址>` 可覆盖内置地址。

**更正 0.8.7 的一处不实描述**：0.8.7 的说明里写了"真机验证了无界面装 CLT 有效"，
那是**只有放标记文件 + `softwareupdate -l` 能看到条目**被验证了，
安装本身在真机上没有走通过。已在 v0.8.7 段落里划掉并注明原因。

---

v0.8.7 · 全原生穿透/反代工具（frps / frpc / Lucky / Orbien 服务端+客户端）+ 内置默认音色 + 多密钥额度与用量

**全新机器上的真依赖：TTS 两件套需要命令行开发者工具**（抹机后的 mini 实测发现）
- 全新 macOS 上 `/usr/bin/python3` **只是个占位程序**：跑它只会打印
  "xcode-select: note: No developer tools were found, requesting install." 并弹图形对话框。
  而音色接收端的 plist 就是 `/usr/bin/python3`，Qwen3 TTS 也要真 Python（venv + pip）。
- 所以：**接收端安装前 `EnsureCLT`**（CLT 就够）；**Qwen3 TTS 安装前 `EnsureHomebrew`**
  （CLT → brew → python@3.11 一整条链）。以前在全新机器上会以"看不懂的方式"失败。
- ~~真机验证了"无界面装 CLT"这条路在 macOS 15.7.9 上有效~~ ——
  **更正（0.8.8）**：这只在"能连上苹果 CDN"的机器上成立。抹机后的 mini 上，
  `softwareupdate -i "Command Line Tools for Xcode-16.2"` **15 分钟只下了 1 MB
  然后停住**，最后回退到会弹图形对话框的 `xcode-select --install`，
  全新安装就卡在那里。0.8.8 起改为**镜像整包 → softwareupdate → 弹窗**。

**修掉一个"按钮点开就坏"的问题**：Lucky 的「📝 编辑配置文件」之前指向 `lucky.conf`，
而它的配置其实是**加密的** `lucky_*.lkcf`（真机快照核实：`~/lucky` 下没有 lucky.conf）。
文本编辑器打开只会是一堆二进制 —— 现在 Lucky 不再提供这个入口，
可视化配置走它自带的 Web UI（16601），条目说明里也写清了"不要手改、备份整个目录"。

**全新安装后"全部在面板里完成"的最后一个缺口补上了**：全新 macOS 上既没有 Homebrew
也没有命令行开发者工具（CLT），以前点「网站环境」只会得到一句"请先安装 Homebrew" ——
把用户赶回命令行。现在面板会**自己在任务里装好这两样**：
- CLT 优先走**无界面**路径（`softwareupdate` 静默安装，历史技巧：先放
  `.com.apple.dt.CommandLineTools.installondemand.in-progress`）；这条路在新版 macOS 上
  不一定还有效，所以保留回退：`xcode-select --install` + **明确提示"请点弹窗里的安装"**
  并轮询等待（最多 30 分钟），而不是死等。
- Homebrew 安装脚本从 raw.githubusercontent.com 下载（大陆直连不通）→ 官方优先 + 加速镜像兜底；
  安装时用国内 git 镜像（USTC/TUNA）+ `NONINTERACTIVE=1`（否则它会等"按回车继续"，
  在面板任务里就是永远卡住）。装完**真的跑一次 `brew --version`** 验证。
- 命令行开发者工具与 Homebrew 的解析/候选源都有单测锁死。

**国内网络专项（实测无代理环境）**：GitHub Release 直连**完全不通**（20 秒 0 字节），
自建镜像 3.7 秒下 2MB。据此改了三处：
- `install.sh`：**自动回退到内置镜像**（先探真实 tarball 地址，官方不通就用镜像；
  以前是"探目录 HEAD 失败 → 去源码构建"，而普通用户机器上没有 Go，等于装不上）；
  另加 `--download-base <镜像>` 参数。
- 面板跑 brew 时**注入国内镜像**（`HOMEBREW_API_DOMAIN`/`HOMEBREW_BOTTLE_DOMAIN`，
  阿里云，实测 0.17s）+ 关掉自动更新。以前只把镜像写进用户 shell 的 rc，
  而面板是 LaunchDaemon 读不到、`sudo` 又会清空环境 —— 于是面板装 nginx/PHP/MySQL
  走官方源，国内无代理基本不通，界面表现是"点了安装长时间没进度"。
- 市场下载 GitHub Release 的应用（lucky/frp/orbien…）：**先探测各源速度再按快慢排序**。
  以前官方永远排第一 + 150 秒超时，国内每个应用都要白等两分半。

这一版把前几轮的成果合并发布：**多密钥 + 每密钥额度（字）+ 用量统计**、
**内置默认音色（装完就能用）**、以及**应用市场新增 5 个穿透/反代条目（全部原生）**。

接收端 v1.6.0
- **多密钥**：密钥与额度存 `~/tts/voice-receiver/keys.json`，接收端**按 mtime 热加载** ——
  加/停/删/改额度**都不需要重启**（它可能正在替用户合成）。plist 里不再写明文密钥，
  只传 `--keys-file`。老的 `--token` 仍被接受（兼容已部署的机器）。
- **每密钥额度（单位：字）**：按提交文本的字符数计（`len(text)`，中文 1 字算 1、含标点），
  0 = 不限。超额返回 **429**，error 以 `quota exceeded:` 开头 —— 刻意与
  "密钥无效"的 **403** 分开，否则调用方会把"额度用完"当"密钥错"去重试。
  幂等重放（同一 client_id、任务仍在队列）不重复扣。
- **用量统计**：`GET /usage` 按密钥累计 chars / requests / jobs / audio_bytes，
  另存最近 60 天的按天明细。调用被拒不计用量；上游失败只计一次请求、不计字数与字节；
  已删除的密钥保留历史用量（所以总量对得上）。
- 停用的密钥**真的**失效（返回 403，不会落回兼容密钥）。

面板
- 服务详情原来的「🔑 更改共享密钥」改成 **「🔑 调用密钥」**，入口是**添加密钥**：
  名称 + 额度（字）+ 可选自定义值；新增后完整密钥显示一次（可复制），之后只给掩码。
- 列表里每把密钥能看到：额度、已用、剩余（带进度条，快满变黄、满变红）、今日、
  最后使用；可以编辑额度/名称、停用/启用、删除（删除＝立刻撤销访问）。
- 顶部是总览（今日 / 近 7 天 / 累计字数、调用次数、产出音频）与 14 天趋势条。
- **不会被自己锁死**：不允许停用或删除最后一条启用的密钥；同值密钥不允许重复添加。
- 重新部署接收端时会把 plist 里那把老密钥**原样迁移**成 keys.json 的第一条
  （网站不用改配置），随后 plist 不再写密钥；迁移完成前面板把它标成
  「来自服务配置，未迁移」并只读。
- 删掉了旧的"更改共享密钥"接口与弹窗（它靠重写 plist + 重启服务，与新模型重复）。

内置默认音色：装完就能用，不必先录一段（接收端 v1.7.1）
- 面板二进制内嵌一份音色样本（7.68s / 24kHz / 单声道 / 16bit PCM，"龙安灵心"，
  用户提供）与它实际念的那句参考文字。
- 在面板里装/重新部署接收端时自动写到 <样本目录>/default/ref.wav + ref.txt，
  于是**全新安装的机器装完就有可用音色**。
- 没指定音色的调用走它：`POST /jobs` 不带 `ref_audio`、`source` 省略或为 default
  即用这份样本，并自动带上 ref.txt 的参考文字（克隆时给对参考文字更稳）。
- **不覆盖用户的样本**：只有 default/ref.wav 不存在、或带 builtin-voice.json
  标记（面板自己放的）时才写；用户自己传过的机器会打印"保留了你自己上传的样本"。
- 「各来源」里该行标成「内置默认音色，含参考文字」。

应用市场新增 5 个穿透 / 反代条目：**全部原生**（官方 darwin-arm64 产物 + launchd，不碰 brew、不用 Docker）
- **frps / frpc**（frp 服务端与客户端）：官方 `frp_0.71.0_darwin_arm64.tar.gz` 里各取一个二进制。
  **frp 是这几个上游里唯一提供 SHA-256 清单的**（`frp_sha256_checksums.txt`）：面板在解压前
  核对下载到的 tarball，不一致就中止安装 —— 正好补上"加速镜像（第三方）"这个信任缺口。
  面板生成 `frps.toml`（bindPort 7000、随机 auth.token、dashboard 开在 7500）与
  `frpc.toml`（serverAddr/serverPort、**自动复用同机 frps 的 token**、admin UI 7400）。
- **Lucky**：官方 `lucky_2.27.2_darwin_arm64.tar.gz`，自带 Web UI（16601）。
- **Orbien 服务端 / 客户端**：官方 `orbien-server_3.6.0_darwin_arm64.tar.gz` 与
  `orbien_3.6.0_darwin_arm64.tar.gz`（CLI 客户端），面板生成配置与随机 Dashboard 口令。
- 为什么不用 Docker（用户拍板）：**服务端**的核心是动态入站端口（容器里必须预发布端口段）；
  **客户端**的活是把本机服务暴露出去（容器里每条隧道都得把 127.0.0.1 改写成
  `host.docker.internal`）。这两条正好卡在它们的主用途上。上游其实都有 arm64 镜像
  （lucky / snowdreamtech-frpc / orbien-client / orbien-server），README 里如实写了这一点
  以及"想容器化的人可以自己换 + 代价是什么"。
- 通用安装器 `InstallReleaseBinary`：下载（官方优先、150 秒没下完就换镜像并说明原因）→
  **可选 SHA-256 校验** → 解压 → **`file(1)` 复核确实是 arm64 才继续** → 生成配置 →
  launchd → 等端口 → 登记服务（带健康检查 URL）→ 卸载（勾"删除数据"连目录一起清）。
- **可视化配置**：每个条目都能进自带 Web UI（frps 7500 / frpc 7400 / lucky 16601 /
  orbien 8020）；另外服务详情新增「📝 编辑配置文件」，直接在面板里改
  `frps.toml` / `frpc.toml` / orbien 配置（复用文件读写接口 + "重启服务生效"按钮）。
- **Lucky 不做 HTTP 健康检查**：它的 Web UI 可以被用户设成「安全入口」（随机路径）或加
  IP 白名单，那时它对**任何**路径（含 127.0.0.1 的 `/`）都返回 404 而不是 403 ——
  真机上就遇到了（`/`、`/login`、`/api/base` 全 404，而端口在听、模块全部 started）。
  与其给一个会误导的红灯，不如只按端口判断存活、健康列如实显示"未配置"。
  而且健康地址现在会与目录**双向对齐**：目录说该有就补上/更新，目录说**不该有**就把
  旧记录里那条会误导的地址清掉（只补不清的话，策略一改老记录会永远带着一个假红灯）。
- **frpc 连不上服务端时不再退出**：frp 的 `loginFailExit` 默认 true（第一次登录失败就
  整个进程退出）。面板托管下这是个陷阱 —— 用户先装 frpc、后装 frps（或 frps 暂时没起）时，
  frpc 会立刻死掉、launchd 又反复拉起、7400 打不开。生成的 frpc.toml 现在显式
  `loginFailExit = false`，它会安静地重连（真机踩到并已锁测试）。
- 顺带修：这类应用注册服务时**漏了健康检查 URL**（Lucky 明明 200 却显示"未配置"），现在会
  按界面端口补上；已存在但缺 URL 的旧记录，重装一次即自愈。
- 顺带改：`PreferDirect` 的应用（uptime-kuma / minio / n8n / gitea / stirling-pdf / 新的这四个）
  「打开」主按钮改成**端口直连**、子路径降级为"试试子路径" —— 与字段文档一致（原来主按钮
  指向已知打不开的子路径）。

计费口径改了：按**实际合成完成**的字数结算（接收端 v1.7.0）
- 以前是提交时按整篇扣：用户提交 1000 字、跑到第 3 块失败，1000 字照扣 —— 等于把
  "我们的失败"算在用户头上。现在：
  · 提交时只**占用**额度，防止同一把密钥并发提交把总额度撑爆（剩余额度里也会扣掉占用）；
  · 作业到终态（完成/失败/取消）时按**已经合成完的块**结算，没跑到的部分自动退回；
  · 直连 /v1/audio/speech 只在响应 2xx 时计费；
  · 结算标记写进作业文件，重启/清理/取消三条路径都不会重复扣（老作业不补结算）。
- 面板里每把密钥的"剩余"会显示「占用中 N 字」，额度进度条也跟着占用走。
  （真机验收抓到过一个自相矛盾的显示：面板自己重算剩余时漏扣了占用 —— 界面显示
  "还有 30 字"、实际只能提 3 字。现在接收端与面板两个口径都扣占用，并加了断言。）

新增手动清零（本轮第二个需求）
- 每把密钥行内有「清零」，顶部有「🧹 清零全部用量」：把已用字数与调用次数归零、
  重新开始计，**密钥与额度不变**。
- 清零是管理动作：接收端要求 `X-TtsVoice-Admin`（管理密钥 `--admin-token` 在部署时
  自动生成并写进 plist，只有面板拿得到）。**刻意不让普通调用密钥清自己的额度** ——
  否则额度就只是建议。普通密钥清零 → 403；管理密钥写错 → 403。
- 清零不影响正在跑的作业：它们完成后仍按实际完成的字数记进来（也就是从 0 重新开始计）。

接收端 v1.6.1：访问日志
- 每次请求一行（健康检查与 /jobs 轮询除外）：状态码、方法、路径、用到的密钥 id、来源 IP、
  被拒原因。起因是一次真实事故：插件报「接收端拒绝：HTTP 200」，而接收端这边一条日志都没有，
  只能靠"没有日志"反推"请求根本没到这台机器"（真因是插件把 TTS 服务器地址填错了）。
  现在"到没到、被谁拒的"一眼可见。

界面细节
- 14 天趋势改成**每天一根固定宽度的柱子 + 日期标签**：第一版用 flex:1，
  只有一天数据时那根柱子会铺满整条横带，看起来像一条蓝色横幅，读不出"这是一天"。

验证
- receiver：单元测试覆盖字符计数口径、密钥匹配（ok/停用/不存在）、密钥表热加载、
  密钥表读坏时保留上一份表、额度检查、用量累计/按天/落盘/60 天裁剪；
  端到端覆盖多密钥鉴权、额度 429、各密钥独立、热加载、重启后统计不丢、
  删除的密钥仍可见且立刻失效。
- Go：密钥表读写（含 0600 权限）、最后一条启用密钥的保护、重复密钥值、
  迁移幂等、id 校验；plist 断言"不再包含 --token 参数"。
- 顺带修一个真 bug：plist 解析用 `strings.Contains("--token")` 找参数行，
  而模板里新增的注释也含这个词 —— 会把注释当成参数行、取出错误的值。
  两处解析都改成精确匹配 `<string>--token</string>` / `<string>--host</string>`。
