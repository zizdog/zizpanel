# ZizPanel

macOS 上的网站与服务管理面板（类似宝塔面板，但为 macOS 与 Apple Silicon 原生打造）。

一条命令安装，装完即可在任意设备上用浏览器远程管理这台 Mac。

---

## 快速开始

在目标 Mac 上打开「终端」，执行：

```bash
sudo bash install.sh
```

安装脚本会自动完成：

1. 检测系统与真实用户（`sudo` 下 `$HOME` 是 `/var/root`，脚本会自动找回你的账号）
2. 安装程序到 `/opt/zizpanel`，控制命令软链到 `/usr/local/bin/zizpanel`
3. 注册 LaunchDaemon：**开机自启 + 崩溃自动重启**
4. 写入最小化 sudoers 授权（只授权一个受限助手，不授权通用 shell）
5. 生成本机受信 HTTPS 证书（装了 mkcert 时）或自签证书
6. 把面板加入 macOS 防火墙允许列表
7. **用真实 HTTP 请求探活**，确认服务真的可用（而不是只看进程在不在）

安装完成后终端会直接打印访问地址，形如：

```
远程访问   https://192.168.1.100:8443
本机访问   https://127.0.0.1:8443
```

首次打开会进入初始化向导，设置管理员账号即可使用。

### 访问入口

安装完成后有三个入口，用哪个都行：

| 入口 | 地址 | 说明 |
|------|------|------|
| 本机习惯入口 | `http://localhost/_panel` | **接管原简易面板的路径**，经 nginx 反向代理 |
| 直接访问 | `https://<本机IP>:8443` | 面板自带 HTTPS，不依赖 nginx |
| 本机直连 | `https://127.0.0.1:8443` | 同上，仅回环 |

安装脚本会把原来的 `www/_panel` 改名为 `www/_panel.legacy-bak` 保留（可回滚），
然后在 nginx 默认站点里把 `/_panel` 从"指向旧面板目录"改为"反向代理到新面板"。

> 回滚方式：把 `_panel.legacy-bak` 改回 `_panel`，
> 并恢复 `vhosts/000-default.conf.zizpanel.bak`，然后 reload nginx。

### 在线升级（面板内一键完成）

装好之后就不需要终端了：设置 → 关于与运维 → 在线升级。

```
检查更新 → 下载并校验 → 立即升级 → 面板自动重启 → 看门狗验证
                                        └─ 失败则自动回滚到旧版本
```

安全设计（这个功能本质上是"用面板自己的权限替换自己的 root 二进制"，
做错了就是远程代码执行后门，所以每一步都是 fail-closed）：

| 环节 | 做法 |
|------|------|
| 发布 | `make keys` 生成 Ed25519 密钥对；`make release` 签名 `manifest.json` 并把公钥注入二进制 |
| 校验 | 面板用**内嵌公钥**验签清单，再按清单里的 SHA-256 校验安装包 |
| 没有公钥 | **拒绝**从网络升级（不提供"跳过验证"开关） |
| 离线 | 支持管理员直接上传发布包（上传者已鉴权，等价于 root） |
| 替换前 | 先跑 `version --json` 自检，确认新包能执行且版本正确 |
| 替换时 | 同目录临时文件 + `rename` 原子替换，旧版备份为 `*.bak` |
| 替换后 | 独立 LaunchDaemon 看门狗验证健康检查；失败则还原 `*.bak` 并重启 |
| 判定 | 看门狗比对**健康检查里的版本号**，而不是"HTTP 200" —— 否则旧进程应答会被误判成功 |

发布流程：

```bash
make keys          # 只做一次；私钥在 .release-key/（已 gitignore），务必离线备份
make release       # 产出安装包 + 签名清单，并断言公钥确实嵌进了二进制
make upgrade-e2e   # 真机演练：升级到 0.2.0 并验证面板带着新版本回来
```

> 私钥丢失后，已安装的面板将无法再接受你签出的升级包（这是设计如此）。
> 此时仍可用「上传升级包」这条离线路径。

### 从发布包安装

```bash
tar -xzf zizpanel_0.1.0_darwin_arm64.tar.gz
cd zizpanel_0.1.0_darwin_arm64
sudo bash install.sh
```

### 装到另一台 Mac（例如 Mac mini 当服务器）

工作电脑上开一个只读安装源：

```bash
make serve-install
```

它会构建发布包、探测局域网 IP，并打印出目标机要执行的命令：

```bash
# 在 Mac mini 上执行（地址以上一步打印的为准）
curl -fsSL http://192.168.1.100:8899/install-remote.sh | sudo bash
# 顺便配好服务器模式（禁睡眠、关自动更新重启等）
curl -fsSL http://192.168.1.100:8899/install-remote.sh | sudo bash -s -- --server-mode
```

`sudo` 只需要输**一次**开机密码，之后安装全程无人值守。

> 完整的部署、长期稳定运行设置、SSH/Tailscale 与故障排查：
> 见 **[Mac-mini部署指南.md](Mac-mini部署指南.md)**。

### 可用的环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ZIZPANEL_ROOT` | `/opt/zizpanel` | 安装根目录（可装到外置盘） |
| `ZIZPANEL_LISTEN` | `:8443` | 监听地址，如 `:9000` 或 `127.0.0.1:8443` |
| `ZIZPANEL_VERSION` | `latest` | 指定版本（下载安装时） |
| `ZIZPANEL_DOWNLOAD_BASE` | GitHub Releases | 二进制下载源 |
| `ZIZPANEL_SKIP_DEPS` | - | 设为 `1` 跳过依赖检查 |
| `ZIZPANEL_SKIP_FIREWALL` | - | 设为 `1` 跳过防火墙配置 |
| `ZIZPANEL_SERVER_MODE` | - | 设为 `1` 等价于 `--server-mode` |
| `ZIZPANEL_NO_SSH` | - | 配合 `--server-mode`：设为 `1` 则**不**开启 SSH |

安装脚本参数：

| 参数 | 说明 |
|------|------|
| `--server-mode` | 装完顺带执行服务器模式配置（禁睡眠/关自动更新重启/崩溃报告不弹窗/开启 SSH） |
| `--no-server-mode` | 显式关闭服务器模式（默认） |
| `--listen <地址>` | 指定面板监听地址，等价于 `ZIZPANEL_LISTEN` |
| `--help` | 打印用法 |

---

## 常用命令

```bash
zizpanel status                       # 查看运行状态与访问地址
zizpanel info                         # 打印环境路径（排障用）
sudo zizpanel gen-cert                # 重新生成自签证书
sudo zizpanel hash-password <密码>     # 生成 bcrypt 哈希（手工配置用）

sudo launchctl kickstart -k system/cn.zizpanel.panel   # 重启面板
tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log   # 查看日志

sudo /opt/zizpanel/uninstall.sh          # 卸载（保留数据）
sudo /opt/zizpanel/uninstall.sh --purge  # 彻底卸载（含数据）
```

### 忘记密码怎么办

面板不提供"网页重置密码"入口（那等于给攻击者留后门）。请在**本机终端**执行：

```bash
# 推荐：交互输入，不回显，不会留在 shell 历史里
sudo zizpanel reset-password admin

# 或在脚本里用管道（避免密码被 shell 转义截断）
printf '%s' '你的新密码' | sudo zizpanel reset-password admin --stdin
```

重置后该账号的所有登录会话立即失效，需要用新密码重新登录。

> 不要写成 `sudo zizpanel reset-password admin 'p#ss'`：
> 密码里的 `#` 在 shell 中可能被当注释截断，`$`、`*`、空格也会被展开。
> 交互模式或 `--stdin` 没有这个问题。

### 消除浏览器的证书警告

安装脚本会优先用 **mkcert** 签发证书。但 `mkcert -install`（把本地 CA 写入系统信任库）
需要一次**图形界面授权**，因此在 SSH / 远程会话里无法自动完成，脚本会跳过并告知。

在 Mac 上打开「终端」（图形界面），执行一次即可，之后不会再有任何证书提示：

```bash
mkcert -install
sudo launchctl kickstart -k system/cn.zizpanel.panel
```

或者手动信任：打开「钥匙串访问」→ 系统 → 把
`~/Library/Application Support/mkcert/rootCA.pem` 拖进去 → 双击 → 信任 → 「始终信任」。

不做这一步也能正常使用，只是浏览器首次访问会多一次「继续前往」的确认。

---

## 目录结构

```
/opt/zizpanel/
├── bin/
│   ├── zizpanel           主程序（Web 服务 + CLI）
│   └── zizpanel-helper    受限提权助手（仅 root 可调用）
├── data/
│   ├── config.json        配置（权限 600，含会话签名密钥）
│   ├── panel.db           SQLite 数据库（用户/站点/服务/审计/会话）
│   └── tls/               HTTPS 证书
├── logs/                  按天滚动的日志
├── run/                   pid 等运行时文件
├── work/                  compose 文件、服务数据、备份
├── tools/                 随包分发的辅助脚本
├── takeover-panel-entry.sh  nginx 入口接管工具（可单独执行）
└── uninstall.sh           卸载脚本
```

安装脚本还会确保 nginx 具备以下环境（幂等，可重复执行）：

```
/opt/homebrew/etc/nginx/
├── conf.d/
│   └── upgrade-map.conf          # 反向代理的 WebSocket 升级 map（http 上下文）
├── includes/
│   └── php-fpm.conf              # PHP-FPM 转发参数（location 上下文）
└── vhosts/
    └── 000-default.conf          # 其中 /_panel 被改为反代到面板
```

**为什么把 php-fpm.conf 从 conf.d 移到 includes**：
`conf.d/*.conf` 是被 `http` 块 include 的，而 `fastcgi_pass` 只在 `location` 上下文合法。
如果把含 `fastcgi_pass` 的文件放在 conf.d，nginx 会直接报
`"fastcgi_pass" directive is not allowed here` 而无法启动。
安装脚本会自动完成迁移并改写所有引用。

浏览器界面、静态资源全部通过 Go `embed` 打进主程序，
因此**升级只需要替换一个二进制文件**，不存在"忘了拷某个资源文件"的问题。

---

## 为什么这样设计

### 为什么用 Go 单二进制

- **零运行时依赖**：目标机器不需要先装 PHP/Node/Python 才能跑面板。
  "一条命令装到任意 Mac"这个需求，用解释型语言做的话安装脚本要处理的环境
  组合会成倍增加。
- **面板必须比它管理的服务活得久**：面板管理 nginx/PHP/MySQL/Docker，
  如果面板和它们共享运行时，服务一崩面板也打不开，就彻底失去了远程恢复的手段。
- **自带 HTTP/SSE/进程管理**：不需要额外装 web 服务器和反代。

### 为什么用 SQLite 单文件而不是 MySQL 或 JSON

- **JSON**：面板要存会话、审计、计划任务，写入是常态，并发写会损坏文件。
- **MySQL**：面板依赖它管理的数据库 → MySQL 一停，面板也进不去，形成循环依赖。
- **SQLite 单文件**：零外部依赖，备份就是拷一个文件，迁移换机也只拷这一个文件。

### 为什么前端不引入框架和构建步骤

面板需要在目标机器上稳定运行数年。无 `node_modules`、无构建链意味着：
改一个 CSS 变量不需要保证 Node 版本、不需要 CI 产物、不会因为依赖链腐坏而无法构建。
前端用原生 ESM 模块 + 一个 80 行的 DOM 构建器，足够撑起宝塔级别的界面。

### 为什么提权要单独做一个受限助手

面板需要 root 才能读写 `/opt/homebrew/etc/nginx`、操作 launchd、改 `/etc/hosts`。
最危险的做法是给面板进程一个"可以执行任意命令"的 sudo 授权——
那样任何一处命令拼接疏漏都等于 root 命令注入。

本项目的边界是：

- 面板以 root 运行，但**只能调用 `zizpanel-helper` 这一个程序**（sudoers 白名单）
- 助手**只接受子命令 + 结构化参数，不接受 shell 字符串**
- 每个子命令在助手内部重新校验参数：域名走正则、路径走 `realpath` + 目录白名单
- 不提供"执行任意命令"入口；新增能力必须显式加子命令

---

## 账号

面板是单管理员模型，账号在「面板设置 → 账号与安全」里管理：

| 操作 | 说明 |
|---|---|
| 修改用户名 | 3-32 位字母数字与 `. _ - @`；**要当前密码确认** |
| 修改密码 | 修改后所有会话立即失效，需要重新登录 |
| 两步验证 | 可开启 TOTP；开启后登录要求动态验证码 |
| 登录会话 | 列出活跃会话，可强制关闭 |

改用户名为什么也要当前密码：用户名是登录凭据的一半，而且审计里"谁做了这件事"
记的就是它 —— 不能让一个被劫持的会话把账号改成事后追查对不上人的名字。
改完**不吊销会话**（会话按 user_id 关联），界面上侧边栏那一行会就地更新。

## 安全设计

| 项目 | 做法 |
|------|------|
| 密码存储 | bcrypt（cost 12） |
| 会话令牌 | 32 字节随机串，数据库只存 SHA-256 哈希；HttpOnly + SameSite Cookie |
| CSRF | 双提交模式，**只认请求头**（不回退读 Cookie，否则浏览器自动带 Cookie 会让校验失效） |
| 登录防护 | 连续失败按账号锁定（持久化）+ 内存级 IP 限流（防 bcrypt 打满 CPU） |
| 用户名枚举 | 用户不存在时也执行一次 bcrypt，响应时间不可区分 |
| 两步验证 | 自实现 RFC 6238 TOTP，验证码窗口 ±1 步容忍时钟漂移，challenge 令牌防重放 |
| 远程访问 | 默认 `any`；可切 `local`/`whitelist`（支持 Tailscale 网段）；白名单模式下回环地址永远放行 |
| 代理头 | 默认不信任 `X-Forwarded-For`（否则 IP 白名单可被伪造绕过），仅在反代后开启 |
| 文件写入 | 所有受保护配置（sudoers/plist）先写临时文件再原子替换 |
| 配置权限 | `config.json` 600（含会话签名密钥） |

---

## 开发

```bash
make help            # 查看所有目标
make dev             # 本地构建
make check           # 提交前检查：gofmt + shell 校验 + vet + 单测 + 两套安装端到端测试
make run-local       # 在临时目录以调试模式启动（端口 18443）
make smoke           # 启动本地实例并做浏览器端到端 UI 验证
make release         # 产出 darwin/arm64 与 darwin/amd64 发布包
make serve-install   # 把本机变成安装源，供另一台 Mac 一条 curl 命令安装
make remote-test     # 远程一键安装的端到端测试（本地 HTTP + 沙箱安装）
```

### 测试策略

| 测试 | 覆盖内容 |
|------|----------|
| `go test ./...` | 认证、TOTP、会话、配置、数据层、系统采集解析、提权边界、Docker socket 探测 |
| `tools/sandbox-install-test.sh` | **跑真实 install.sh**：路径处理、plist/sudoers 内容、HTTPS 探活、重复安装幂等性、卸载 |
| `tools/remote-install-test.sh` | **跑完整 curl 一键安装链路**：构建发布包 → 起本地 HTTP → 下载 → 校验和 → 解压 → 沙箱安装 |
| `tools/uitest.mjs`（`make smoke`） | **真实浏览器**（本地临时实例）：初始化 → 登录 → 仪表盘 → 设置 → 服务/市场 → 文件 → 数据库 → 日志 → 会话保持 → 移动端 |
| `tools/uitest.mjs`（`make uitest-live`） | 同上，但对**本机真实安装实例**跑，额外覆盖提权链路：建站 → 诊断 → nginx 校验 → 删站 → 建/删定时任务 |
| `internal/upgrade`（33 项） | 版本比较、清单验签（含篡改拒绝）、解包安全（路径穿越/符号链接/归档炸弹）、**真实执行看门狗**验证回滚与自清理 |
| `tools/upgrade-e2e.sh` | 在线升级**真机演练**：造一个更高版本的签名发布包 → 起 HTTP 源 → 走 API 检查/下载/升级 → 确认面板带着新版本回来且旧站点不受影响 |

> **提权步骤的诚实处理**：建站和定时任务要写 `/Library/LaunchDaemons`
> 与 nginx vhost，必须经提权助手，而助手硬性要求以 root 运行。
> 本地临时实例是普通用户身份，所以 `make smoke` 会把这 6 步**显式标记为"跳过"**
> 并在结尾汇总里列出来，绝不把跳过当通过。
> 完整覆盖请对真实安装跑 `make uitest-live`（本机实测 52/52 全通过）。

沙箱安装测试的做法：把系统路径（含 **nginx vhost 目录**）重定向到临时目录，
并用桩件替换 `launchctl`/`visudo`/`socketfilterfw`/`nginx`，
因此在没有 root 的机器上也能跑完整的安装回归，且**不会碰到生产环境**。

> 这一点必须显式保证：早期版本漏掉了 nginx 目录的沙箱化，
> 测试直接把生产的 `000-default.conf` 改写成反代到测试端口，
> 造成 `http://localhost/_panel` 全部 502。
> 现在测试里有两道断言专门盯这件事：
> ① 沙箱 vhost 必须被正确改写；② 生产 vhost 校验和必须一字未变。

`launchctl` 桩件故意把 `bootout` 实现成**异步**的（1.2 秒后才真正收掉进程），
并且在此期间让 `print` 报告"服务仍在"、`bootstrap` 返回失败 ——
完全复刻真实 launchd 的时序。原因是升级路径上真出过一次事故：
`bootout` 返回后立刻 `bootstrap`，因为服务还没卸载完而失败，
失败后又没有对象可 `kickstart`，最后**面板彻底没起来**。
桩件不同步复现这个时序，这类 bug 在沙箱里永远测不出来。

唯一没被自动化覆盖的是"macOS 真的会加载这个 plist"，这部分需要真机安装验证。

### 真机验证记录

安装脚本在本机（Mac mini M4 / macOS 15.6.1）实际跑通，验证项：

- LaunchDaemon `cn.zizpanel.panel` 注册成功，`KeepAlive` 生效（kill 后自动拉起）
- 生成的自签/ mkcert 证书被服务端正确加载（`openssl s_client` 校验 issuer）
- **普通用户** 执行 `zizpanel status` 正确报告"运行中"（root 进程探测）
- `sudo -n zizpanel-helper selftest` 免密通过，助手以 `uid=0` 运行
- 越权参数被拒：`hosts-add '../../../etc/passwd'` → `域名格式不合法`
- 现有环境零影响：zizdog.cn 返回 200、vhost 与 MySQL 库原样、nginx 未重启
- 浏览器端到端 17 步全通过（真实 HTTPS 地址 + 真实数据目录）

### 已修复的典型坑（有测试锁死）

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
    条件渲染 `cond ? el : null` 是最常用写法，于是工具栏上真的显示了一个 `null`。
    新增 `appendAll()` 并在服务/市场页统一使用。
20. **登录 Cookie 带 `Secure`，但经 nginx 是 HTTP 访问** →
    浏览器直接丢弃 Cookie，表现为"密码正确但登录后立刻回到登录页"。
    改为按 `X-Forwarded-Proto` 判断实际访问协议。
21. **JS/CSS 被浏览器长缓存** → 面板就地升级后前端仍是旧代码，
    出现"后端已修好、页面还是老行为"的诡异现象。改为 `no-cache`（每次校验）。
22. **应用市场对"纳管类"应用也做端口检查** → 报"端口被占用"，
    而那个端口正是它自己在用。纳管类应用跳过端口检查。
23. **可纳管扫描把面板自身与 nginx 也列进去了** → 用户在面板里停掉自己，
    会立刻失去访问入口。已加入保护名单。
24. **可纳管列表因新旧 label 重复**（`homebrew.mxcl.php` 与 `sh.brew.php@8.3`
    指向同一个进程）→ 按可执行文件去重，运行中的优先。
25. **沙箱安装测试改写了生产的 nginx 配置**（最严重的一次）→
    测试用 18444 端口，但入口接管没有沙箱化，于是把生产
    `000-default.conf` 的 `proxy_pass` 端口改成了 18444，
    导致 `http://localhost/_panel` 全部 502。
    已把 vhost 目录与 nginx 前缀纳入沙箱，并新增两条断言：
    「沙箱内 vhost 必须被改写」+「生产 vhost 必须一字未动」。
26. **每次启动都无条件重写 WebSocket map 并 reload nginx** →
    开机时给所有站点造成一次无谓的请求抖动。改为仅在内容变化时写入。
27. **`config.json` 由 root 进程写出（0600）** → 普通用户执行
    `zizpanel status` 报 permission denied，而这条命令正是排障要用的。
    启动时自愈归属，权限仍保持 0600。
28. **Playwright 的 `has-text` 是子串匹配** → `button:has-text("新建文件")`
    同时命中"新建文件夹"，测试建出的是目录，"编辑"步骤自然找不到文件。
    改用 `getByRole(..., { exact: true })`。
29. **测试不幂等会自我掩盖** → 上一次失败留下的文件让这次"新建"返回 400，
    但断言只检查"链接存在"，于是失败点被推迟到下一步，难以定位。
    现在测试开头会先清理上次残留。
30. **cron 的 `7` 与 `0` 都是周日** → 直接透传给 launchd 会得到无效的 Weekday，
    任务永不触发。解析时归一化，并加了去重。
31. **组合展开可能爆炸** → `*/1 * * * *` 会生成 1440 个日历条件、
    几 MB 的 plist。加了 400 条上限并排序截断。
32. **mysql 的 `-N` 与 `-B` 一起用会丢列名** → 代码把第一行数据当成了列名，
    表现为"列名是数据、数据少一行"。去掉 `-N` 后 `-B` 会输出正确的列头。
33. **前端 `stopFollow` 定义在函数内部** → 外层调用直接抛 ReferenceError，
    整页白屏。提到模块级后修复。
34. **macOS 的 `-N`**：`zizpanel status` 在普通用户下读不到由 root 写出的
    config.json（0600）→ 启动时自愈文件归属，权限不变。
35. **`launchctl bootout` 是异步的**（最危险的一次，真机升级时发生）→
    `bootout` 返回 ≠ 服务已卸载。紧接着 `bootstrap` 会以
    `5: Input/output error` 失败，失败后又没有对象可 `kickstart`，
    最终**面板彻底没起来**，只能手工 `launchctl bootstrap` 恢复。
    修法：`wait_service_stopped()` 轮询等 launchd 摘掉服务 + 等端口释放，
    `start_service()` 带重试且 `bootstrap`/`kickstart` 互为兜底；
    全部失败时**自动回滚到升级前的二进制**再启动 ——
    宁可停在旧版本，也不能让用户连面板入口都没有。
    沙箱 `launchctl` 桩件同步改成异步时序，旧逻辑会让测试直接失败。
36. **发布包里漏打 `tools/` 下的运行期脚本** →
    发布流程只手工 `install` 了 `server-mode.sh`，漏了
    `takeover-panel-entry.sh` / `panel-entry.awk` / `check-shell-vars.py`。
    后果是**从发布包安装时 `/_panel` 入口根本没被接管**，
    而安装过程照样"成功"退出，用户只拿到一个打不开的地址。
    修法：Makefile 里集中声明 `RUNTIME_TOOLS`，
    并由 `tools/remote-install-test.sh` 逐个断言压缩包里存在。
37. **面板以 root 运行时 `os.UserHomeDir()` 是 `/var/root`** →
    探测 Docker 时拼出 `/var/root/.orbstack/run/docker.sock`，
    于是 OrbStack 明明在跑、面板却报"未安装 Docker"。
    修法：`DetectDocker(socket, userHome)` 显式接收 `Cfg.UserHome`
    （`user.Lookup` 在无 CGO 下也能正确解析 macOS 用户家目录）。
38. **`pmset` 不支持的键会静默返回成功** →
    `pmset -a autorestart 1` 在笔记本上退出码 0 但什么都没做，
    服务器模式脚本却报"已开启断电自恢复"——这类谎报最致命
    （真跳闸那天才发现从没生效）。修法：先查 `pmset -g cap`，
    不支持的键明确报告"本机不支持"，并说明是笔记本而非台式机。
39. **`launchctl enable` 单独用不能让 SSH 起来**（一次误判的更正）→
    曾据此认定"开 SSH 必须人工点系统设置"。实际上 `systemsetup -setremotelogin`
    那条路确实要完全磁盘访问（TCC），但 root 还有第二条路：
    `launchctl enable system/com.openssh.sshd` **之后必须再**
    `launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist`。
    只 `enable` 会**返回成功但 22 端口不监听** —— 这正是当初误判的原因。
    实测（macOS 15.6.1）两条命令后 22 立即监听、`systemsetup -getremotelogin`
    报告 On。现在 `server-mode.sh` 自动完成，且**只认"端口真的在监听"**，
    不认 launchctl 的退出码。
40. **`launchctl` 的退出码不可信** → 启动服务、开关服务都必须用
    "端口/进程真的处于目标状态"来判定，不能看退出码。
    上面第 35 条（bootout 异步）和第 39 条都是这个问题的不同表现。
41. **`go build -X` 会静默失效**（在线升级的致命前提）→
    `-trimpath` 把不可达符号做了死代码消除，而 `-X` 找不到符号时
    **退出码依然是 0、日志毫无异常**。后果是发布出来的二进制没带上
    发布公钥，面板将永远拒绝网络升级且看不出原因。
    修法：让 `main` 直接引用 `upgrade.PublicKeyHex()`（`version --json` 会输出它），
    并在 `make release` 里**运行产物核对公钥**，不一致直接让发布失败。
42. **看门狗 bootout 了自己要保护的面板**（真机演练中发现，最严重）→
    生成的看门狗脚本里只用一个 `$LABEL`，自清理时 `bootout` 的却是
    面板的 label。结果是**升级成功之后面板直接从 launchd 消失**、
    `/_panel` 502，而且没有任何报错。
    修法：拆成 `PANEL_LABEL` 与 `SELF_LABEL` 两个变量；
    测试不再只断言"调用过 bootout"，而是断言 **bootout 的是谁**。
43. **升级结论没有归属，旧结论会劫持新尝试**（真机演练中发现）→
    看门狗的 `result.txt` 留在磁盘上（用户没打开设置页就不会被消费），
    下次升级点"立即升级"时读到的是**上一次**的 success，
    状态被改写成 success，于是拒绝执行并提示"还没有已准备好的升级包"。
    修法：结论绑定 `RunID`，且只有"进行中"的状态才接受结论。
44. **看门狗清理 plist 的语句永远执行不到**（真机演练中发现）→
    `launchctl bootout` 摘掉自己时会**当场杀掉进程**，
    写在它后面的 `rm -f "$PLIST"` 永远不执行，
    `/Library/LaunchDaemons/cn.zizpanel.upgrade-watchdog.plist` 永久残留。
    修法：先删文件再 bootout；另外面板每次启动都清扫一次残骸，
    并加测试断言"rm 必须出现在 bootout 之前"。
45. **运行时文件归属漂移，面板在能写日志之前就死了**（真机升级时发生）→
    现象极具误导性：在线升级到新版本后面板从 8443 消失，
    `launchctl print` 只给 `last exit code = 78: EX_CONFIG`（配置类退出），
    而**面板日志里什么都没有** —— 于是看起来"新版本是坏的"，
    连升级看门狗都因为等不到健康检查而回滚了一次（回滚当然也起不来）。
    根因是三个身份在打架：安装脚本按"真实用户"`chown -R` 整个安装目录，
    面板进程却按 plist **以 root 运行**（`log_dir`/`data` 是 0700），
    而 `data/tls/panel.key` 又是 `600 root`。一旦本次进程以另一个身份被拉起，
    读证书就 `permission denied`，而这一步发生在 `logx.Init` **之前**，
    所以连一行日志都留不下。
    修法：（a）安装收尾把 `logs`/`run`/`data/tls` 的归属与权限一次校准；
    （b）面板启动时先跑 `cfg.RepairRuntimeAccess()` 再打开日志 ——
    自己的文件被改坏能自动救回（归属 + 权限位两种都覆盖），
    确实是别人的文件且自己无权 chown 时，会打印一条**可直接照抄的 chown 指令**，
    而不是留一个空白日志让人猜。
    教训：**"没有日志"本身就是一条信息** —— 说明失败发生在日志初始化之前，
    应该往启动最早期（配置、目录、证书）而不是业务逻辑上找。
46. **`node --check` 会放过浏览器拒绝的语法，导致整个面板白屏**（写 Docker 页时踩到）→
    两个前端文件里各少了一个 `}`（对象字面量写成
    `h('div', { style: { fontWeight: '600', text: p.name })`）。
    症状极具误导性：面板**白屏**，`pageerror: Unexpected token ')'`，
    控制台**不给出错文件也不给行号**；而逐个文件跑 `node --check` **全部返回 0**。
    排查时一度怀疑路由、缓存、内嵌产物，都不是。
    修法：加 `tools/check-js-syntax.mjs`（用 **acorn** 按 ES module 解析），
    接进 `make check` 作为门禁 —— 它毫秒级给出精确行列。
    教训：**"校验通过"取决于用谁校验**。检查前端语法要用真正的 ES 解析器，
    不能用 Node 的宽松检查（它接受很多浏览器不接受的东西）。

47. **在线升级反复回滚：看门狗把"新版起来了"误判成"启动失败"**（真机连续回滚三次）→
    现象：升级后新版**明明在正常服务**，约 90 秒后被看门狗回滚，面板升不上去。
    根因是**字符串比较**：看门狗拿 `/api/v1/health` 的 `version` 与清单版本号比对，
    而健康检查返回的是 `version.Full()`；正式发布的二进制带 git commit
    （`0.3.1+9a304af`），于是精确匹配 `"version":"0.3.1"` 永远不成立。
    以前没暴露，是因为那些二进制 `commit=dev`，`Full()` 不带后缀。
    **难点是自举**：看门狗脚本由**升级前的旧版本**生成，只改新版本的匹配逻辑
    救不了当次升级 —— 必须让健康检查返回旧版断言所期望的形式。
    修法：`handleHealth` 返回 `version.Version`（纯版本号），同时看门狗脚本
    也接受 `$WANT+` 后缀（兜底）；两侧各有回归测试。
    教训：**跨版本协作的断言不能假设"判断方是新版本"**。
48. **Docker 把内置网络的驱动返回成字符串 `"null"`** →
    `none` 网络的 `/networks` 响应里是 `"Driver":"null"`（不是 JSON null），
    于是 `n.Driver || '—'` 放过它，表格里真的渲染出一个字面量 `null`。
    修法：加 `textOr()` 把 `null`/`undefined`/空串统一成 `—`。
    这条是 UI 测试的"页面不允许出现字面量 null"断言抓出来的。
50. **功能交付了，占位文案还留着**（写审计页时被 UI 测试抓到）→
    仪表盘上有一张"服务状态"卡片写着「此模块正在开发中」，而**服务管理早就交付了**
    （P3）—— 仪表盘在告诉用户功能没做，侧边栏里它却好好地在用。
    根因是这条断言写得太弱：测试注释写着"遍历侧边栏确认没有占位页"，
    实际只点开了「数据库」一项，于是漏了仪表盘上这张卡。
    修法：（a）把仪表盘那张卡换成真实的服务概览（运行中 N/M、端口、跳转）；
    （b）断言改成**真的逐项点开每个导航页**再查"正在开发中"。
    教训：**占位文案要跟着功能一起清理**，而"没有占位页"这种全局断言
    必须真遍历 —— 抽查一项等于没查。
51. **UI 测试里的两处"错误信息与真实原因无关"** →
    （a）应用市场"安装前检查"用 `button:has-text("安装")` 命中的是顶部的
    「⚡ 一键安装 LNMP 环境」，点开了 LNMP 向导 → 改用 `:text-is("安装")` 精确匹配；
    （b）Docker 分区循环把变量命名为 `shot`，**遮蔽了截图函数**，
    `await shot(shot)` 报 `shot is not a function` → 改名 `shotName`。
    教训：**宽松匹配与变量遮蔽都会把排查方向带偏**，测试代码同样要精确。

52. **brew 服务的 launchd 标签被硬编码成 `homebrew.mxcl.*`** →
    应用市场里 PHP/nginx 显示「已安装·未纳管」，点「纳管」报
    "找不到 homebrew.mxcl.php@8.3 的 plist，且该服务未在 launchd 中加载"。
    真因：这台机器的 Homebrew 生成的是 **`sh.brew.php@8.3`**，而同一系统上
    httpd 又是 `homebrew.mxcl.httpd` —— **两套前缀并存**，猜不得。
    修法：按 `~/Library/LaunchAgents` 与 `/Library/LaunchDaemons` 里**真实存在的
    plist** 反推标签（先试两个已知前缀，再按 `*.<formula>.plist` 兜底）；
    市场的 `installed/adopted` 与「纳管」按钮都用这个真实标签。
53. **"装了但服务没注册"的孤儿态没有出口** →
    安装产物还在（`~/iopaint/.venv`），但 plist 没了/从没注册成功。
    判断"已安装"只看服务记录与 plist，于是市场说"未安装"（用户会重装已有的东西），
    服务管理里没有它，**纳管按钮也因为 installed=false 而不显示** —— 用户被卡在中间。
    修法：按安装器的路径约定探测产物，市场增加 `artifacts` 与 `service_in_launchd`
    两个状态；孤儿态显示「已安装·服务未注册」并给「重新部署」（安装器幂等）。
54. **服务管理页每次刷新都要 1.4 秒，其中 1.07 秒是 `colima status`** →
    这个 CLI 要起进程、读配置、连 API，而服务列表每次刷新都会查它。
    修法：改用**文件系统判据** —— Lima 的实例目录（`~/.colima/_lima/<inst>/`）里有
    hostagent 的 `ha.pid` 与 `ssh.sock`，两者都对才算在跑（只判 pid 会被 PID 复用骗到，
    只判 socket 会被残留文件骗到），耗时从 1070ms 降到几毫秒。
55. **健康检查失败只给一个红标签，用户不知道该做什么** →
    页头显示"1 个健康检查失败"，点不开、也没有解释与动作。
    修法：标签变成入口（点它只筛出失败的服务）；失败卡片里给出检查地址、
    返回内容、**按失败种类给的建议**（401/403 专门解释为"该地址要求身份验证，
    服务很可能是好的"），以及重新检查 / 查看日志 / 改检查地址三个动作。
56. **可纳管扫描把面板自己的定时任务也列了出来** →
    `cn.zizpanel.cron.daily-backup` 出现在"可纳管服务"里；它由面板的计划任务
    页面管理，纳管进来毫无意义。
    修法：`isSelfLabel` 除了点名的两个 label，再一刀切排除 `cn.zizpanel.` 前缀。
57. **测试把用户的真实 launchd plist 覆盖成了空 plist**（本项目最贵的一次事故）→
    `internal/web/api_market_test.go` 为了复刻"本机实况"，往 `srv.Cfg.UserHome`
    下写 `sh.brew.php@8.3.plist` / `sh.brew.mysql@8.4.plist`；而测试服务器
    `newTestServer` 当时只把 `DataDir`/`LogDir`/`RunDir` 等隔离到 `t.TempDir()`，
    **唯独漏了 `UserHome`**（它来自 `config.Default()`，解析的是**真实**家目录）。
    于是 `make check` 把用户真实的两个 plist 覆盖成了 8 字节的 `<plist/>`。
    两个服务当时都在跑，所以**一点症状都没有**；但只要机器重启、或面板上点一次
    "重启服务"（`launchctl bootout` 之后没有 plist 可以 bootstrap），
    PHP-FPM 与 MySQL 就再也起不来了。同一轮测试还在 `~/iopaint/.venv/bin/`
    造了个假可执行文件，让应用市场误报"IOPaint 已安装但服务未注册"。

    修法（四层）：
    - `newTestServer` 把 `UserHome`/`WWWRoot`/`LogRoot` 全部指向 `t.TempDir()`；
    - `services_test.go` 不再用 `os.Getenv("HOME")`，`colima_test.go` 不再硬编码
      `/Users/zizdog`；
    - 新增护栏测试 `TestTestServerSandboxedAwayFromRealHome`：`Cfg.UserHome`
      一旦指回真实家目录就直接红；
    - 新增门禁 `tools/check-test-pollution.sh`（已并入 `make check`）：跑测试**前后**
      给 `~/Library/LaunchAgents`（含内容哈希）、`~/www`、安装产物根拍指纹，
      不一致就失败 —— 因为"看起来完全合理"的测试代码靠肉眼 review 是防不住的。

    真实 plist 的恢复办法（供下次参考，**不需要重启服务**）：`launchctl print
    gui/<uid>/<label>` 能打印出已加载作业的全部字段，再用 Homebrew 自己的
    `brew ruby -e 'f=Formula["php@8.3"]; puts f.service.to_plist'` 重新生成，
    逐字段核对后写回即可（`brew services restart` 也能修，但那会真的重启服务）。
58. **Web 终端的状态栏按钮从来没渲染出来过**（`statusBar is not defined`）→
    `terminal.js` 里 `statusBar` 是 `TerminalView()` 内部的 `const`，而往它里面
    塞按钮的 `renderStatus()` 是**模块级函数** —— 一调用就抛 ReferenceError。
    后果很隐蔽：终端能用，只是"清屏 / 复制全部 / 重连 / 关闭会话"这一排按钮
    从来不出现；页面上看不出任何异常，浏览器控制台里的报错**连文件名行号都没有**。
    修法：`statusBar` 与同文件的 `containerEl`/`infoEl` 一样提到模块级。
    附带改进：`tools/uitest.mjs` 的 `pageerror` 现在记录**堆栈并挑出 assets/js 那一帧**，
    否则下次还是只能靠猜是哪个文件。

    > 这条是 UI 测试的控制台错误检查抓到的。它的价值不在于"页面能打开"，
    > 而在于**任何一条 4xx/5xx 与 JS 运行时异常都会让整轮测试失败** ——
    > 只是以前只记 message，抓到了也定位不到。

59. **"流式"其实没流起来（四个细节全踩了一遍）** →
    给安装过程做实时输出时，最容易写出"看起来是流式、其实还是攒到最后"的代码：
    - **`\r` 不切行就等于没流式**：pip / docker 的进度条是用 `\r` 原地刷新的。
      只按 `\n` 切，整条进度会一直攒在缓冲区里，直到命令结束才出现。
    - **`bufio.Scanner` 默认 64KB 就报错**：压缩 JSON、超长 URL 会被判成
      `token too long` 并**静默截断后续输出**。改用 `ReadSlice` + `ErrBufferFull` 续读。
    - **行尾 `\n` 要剪掉**：`ReadSlice` 会把分隔符一起带回来，不剪的话每行日志都拖一个空行，
      列表里的"最后一行"和"复制日志"都会跟着脏。
    - **命令标签必须从 `cmd.Args` 派生，不要手写**：手写的标签会和实际执行的不一致 ——
      `runColima` 就漏了 `sudo -n -u <user>`，日志里显示成"没降权的直接调用"，
      用户照着复制完全是另一回事。派生出来的永远是真的。
    - 顺带：`$ ` 前缀前端后端各加了一次，界面上显示成 `$ $ docker-compose …`。
      现在只由后端写进文本（好处是"复制日志"粘出去自带命令标识）。
60. **改了接口契约，既有测试变红是正常的** →
    把 `docker compose up`（以及所有安装/卸载）改成异步任务后，
    `TestDockerComposeActionRequiresFile` 当场失败 —— 它断言的是"同步返回错误信息"。
    这类红**不是回归**，是契约变了：正确做法是**把测试改成断言新契约**
    （202 + 任务随后失败、失败原因里仍然有"不存在"），而不是为了让它绿而保留同步接口。

61. **把"要登录"当成了"服务坏了"**（用户报的误报）→
    HTTP 健康检查原来只认 2xx/3xx，于是 **401/403 一律报"健康检查失败"**。
    可 Stirling PDF 这类应用装完首次打开就要求设账号密码（那是它自己的登录），
    设完之后健康检查必然拿到 401 —— 服务完全正常，面板却一直标红，
    页头还统计进"N 个健康检查失败"。
    修法：**401/403 判为健康**（HTTP 服务能回应 401 就证明它活着、能处理请求），
    说明写成"HTTP 401（需要身份验证，服务本身正常）"，卡片上单独给一行解释，
    而不是只藏在标签的 tooltip 里。真想校验业务内容仍然用「期望包含内容」，
    那条分支优先于这个宽松判定。404/5xx 照旧算失败。
62. **curl 的报错措辞随版本变，人性化匹配只认老写法** →
    `humanizeCurlError` 只匹配 `"Connection refused"`，而 Homebrew 上的新版 curl
    说的是 `Failed to connect to 127.0.0.1 port 8080 after 0 ms: Couldn't connect to server`，
    于是整行原始报错被原样丢给用户（实测就是这样）。
    修法：新老措辞都认（`Couldn't connect to server` / `Failed to connect` /
    `Connection refused`），并区分超时、空响应、证书失败。

63. **终端"看不到上面的内容"和"没有光标"**（用户报的两个问题）→
    `ansi.js` 的 `scrollUp()` 是 `grid.shift()` —— 滚出屏幕顶部的行**直接丢掉**，
    所以根本没有回滚历史：`.term-screen` 虽然写着 `overflow: auto`，
    但内容永远刚好 rows 行，永远不出滚动条。光标则是**从来没画过**
    （`render()` 只画字符）。修法与三个必须一起处理的细节：
    - 回滚历史（上限 2000 行）+ `.term-screen` 内部滚动 + 自动贴底，
      用户上滚时**不能**把他拽回底部（给「↓ 回到底部」按钮）。
    - 光标要进**每行的渲染签名**：否则单元格内容没变、光标移动时那一行不会被重绘。
      空单元格上的光标要用反色块（`inline-block` + 行高，否则背景只有字身高）。
    - **备用屏（`?1049`）期间的滚动绝不能进历史**：vim/less 每重画一帧就滚一次，
      记进去会把真历史冲成一堆重画帧垃圾。这就是"加回滚"必须同时实现备用屏的原因。
    - 附带修掉一个真 bug：在 less 里改窗口宽度再退出，主屏网格停在旧列宽，
      渲染时 `row[c-1]` 是 undefined → `TypeError` 且屏幕恢复成垃圾；
      进出备用屏都要按当前尺寸归一化网格与历史行。
64. **"空闲超时：0 分钟"是句会被误读的话** → `terminal_idle_mins` 的 0 含义是
    "不限制"（`term.IdleTimeout <= 0` 直接跳过回收），但终端页原样显示成
    "空闲超时：0 分钟"，读起来像"会话立刻过期"。现在显示"不限制"，
    设置页也在输入框下注明"0 = 不自动回收"。

65. **终端会话会泄漏，最后让"终端打不开"** →
    两个根因叠加：① `api_terminal.go` 的 PTY 读协程**不监听 ctx**，
    对端断开后它仍阻塞在 `sess.Read` → `wg.Wait()` 永不返回 → 会话关闭代码不执行；
    ② 完全没有存活检测（`ws.ReadMessage()` 在没有 FIN 时永久阻塞），
    而 `terminal_idle_mins=0` 时连唯一的空闲回收协程都被跳过。
    实测连着 3 个空闲会话，而 `max_sessions=3` —— 用户再点终端只剩"会话上限"。
    修法：WS→PTY 协程设 **75 秒读超时** + `defer sess.Close(...)`（关 PTY 让阻塞的读返回），
    前端每 **25 秒发一次 ping** 给真实空闲的会话续期。
    验证：静置 100 秒会话仍在且还能输入；强杀浏览器后会话立即回收。

66. **跨机器在线升级：清单里的资源 URL 是构建时写死的** →
    `make release RELEASE_BASE_URL=http://127.0.0.1:18877` 会把该前缀写进
    `manifest.json` 的每个 asset URL。把同一份清单拷到 mini 上、用 mini 自己的
    127.0.0.1:18999 提供服务时，panel 仍然按清单里的 18877 去下载 →
    `dial tcp 127.0.0.1:18877: connect: connection refused`，stage 直接失败
    （表现成"点了升级没反应"，而 check 那一步是成功的，很容易误判）。
    解决：让目标机上的升级源端口与清单里的 URL 对齐（这次就是把 mini 的服务起在 18877），
    或者为每台机器分别构建清单。

67. **CPU 温度一直显示"不可读取（需 root）"，其实不是权限问题** →
    Apple Silicon 上 `powermetrics` **没有 smc 采样器**（M4 实测只支持
    tasks/battery/network/disk/interrupts/cpu_power/thermal/sfi/gpu_power/ane_power），
    而 `thermal` 给的是"热压力等级"不是温度。老代码写的是
    `powermetrics --samplers smc`，在 M 系列上永远拿不到值 → 前端把 0 显示成
    "不可读取（需 root）"，把人往权限方向带（实测**根本不需要 root**）。
    修法：Apple Silicon 走 **IOHID 的 AppleVendor 温度传感器**（page 0xff00 / usage 5），
    用系统自带的 `/usr/bin/python3` + ctypes 调 IOKit/CoreFoundation
    （脚本 `internal/sysinfo/temp-reader.py`，从 stdin 喂给 python3，零新增依赖）。
    为什么不写 Go 原生：要么引 cgo（会给"同时出 arm64/amd64 两个包"的发布流程
    引入交叉编译风险），要么引第三方 FFI 库（本机连 proxy.golang.org 都不通）。
    选哪个传感器由 Go 决定（可单测），规则有两条血泪：
    - **必须排除 `tcal`**：它是校准基准（M4 上恒定 51.8°C），当 CPU 温度会凭空高十几度；
      同理排除 `battery`/`gas gauge`/`NAND`；
    - **必须先过滤无效读数**：M4 上 `PMU tdev1`/`PMU2 tdev3` 会返回 **-22°C**，
      直接取最大值会把这些噪声当数据。取 `tdie*`（芯片核心，M4 上 24 个）的最大值。
    界面上同时显示来源（如 `35.0 °C（IOHID tdie×24）`）；取不到时如实说原因，
    不再写死一句可能错的"需 root"。

68. **仪表盘每重渲染一次就漏一条 SSE，攒到 6 条整个面板"卡死"** →
    `renderApp()` 里按 `view.length >= 2` 判断这个页面要不要 `onLeave`
    （清理长连接的回调）。而 `DashboardView(content, ctx = {})` **第二个形参带默认值**，
    `Function.length` 是 **1** → 它注册清理函数的那个分支从来没走到过。
    后果：切主题、切路由每渲染一次仪表盘就多一条 `/api/v1/system/stream` 长连接，
    浏览器对同一主机只允许 6 条并行连接，占满之后**面板所有接口都不返回**：
    页面停在"正在读取…"，而**服务端一切正常**（同一时刻 `curl` 是 200 / 56ms）。
    排查时最容易走错方向 —— 会以为是后端挂了。
    修法：不再用形参个数做路由判断，所有页面统一拿到同一份 ctx。
    教训：**形参个数不是"能力声明"**；这类静默失效要有一眼能看出来的证据 ——
    现在 `make uitest` 会遍历全部导航项，漏连接会让后续步骤直接超时。

69. **日志流被服务端拒绝时，界面却说"正在重连…"** →
    `EventSource` 只在"连上过、后来断了"时才自动重连；服务端返回非 200
    （典型：这个服务没有任何可跟踪的日志文件，后端返回 400）时，
    浏览器把 readyState 置为 `CLOSED` 并**永久放弃**。
    老代码不区分这两种情况，一律显示"连接中断，正在重连…"，
    用户就一直等一件永远不会发生的事。
    修法：`sseServiceLogs` 的 onError 把 EventSource 一起回调给调用方，
    按 `readyState === EventSource.CLOSED` 区分，并去非流式接口取真实原因
    （"无法读取日志：该服务没有可跟踪的日志文件"），不把原因硬编码在文案里。

70. **`make uitest` 断言里写死 `login`：全新实例上必然失败** →
    审计页的关键词筛选测试原本搜 `login`。但全新实例第一次进面板走的是
    **初始化**（`setup`）而不是**登录**，一条 `login` 记录都没有 ——
    于是测试会在**完全正确的行为**上报错。这个坑只在干净实例上暴露，
    对着用过一段时间的真机跑一直是绿的。
    修法：从页面"动作"下拉里取一个**真实出现过**的动作名来筛，
    测的是"筛选"这件事本身。教训：测试别依赖"这台机器恰好有历史数据"。

71. **`pmset -g cap` 是多行输出，用整段做子串匹配会全军覆没** →
    能力探测写的是 `strings.Contains(" "+out+" ", " "+key+" ")`，
    而真机输出是**每行前带一个空格的多行文本**：
    `Capabilities for AC Power:\n displaysleep\n disksleep\n sleep\n womp\n autorestart\n…`。
    于是除首尾两个键外**全都匹配不上**，界面上把每一项都标成"本机不支持"，
    用户因此永远改不了电源设置 —— 而 `pmset -g cap` 明明列着 `autorestart`。
    单测没抓到（桩件当时是单行输出，等于按实现写了测试），
    **升级到 mini 上做真机验证时才暴露**。
    修法：按行切分、跳过表头，逐行取键；测试用**真机抓下来的原始字节**做固定样例
    （`od -c` 抓的），不再自己编一份"看起来像"的输出。

72. **`lsof` 参数被拼成一个字符串 → 端口探测永远返回 false** →
    `fmt.Sprintf("-nP -iTCP:%d -sTCP:LISTEN", port)` 整个作为一个 argv 传给 lsof，
    lsof 把它当成不认识的选项，报错退出、没有输出，于是"有没有人在听"永远是否。
    真机上的表现最具误导性：用户**正连着 SSH**，「系统设置」页却写
    "远程登录未监听（22 端口没人听）"—— 会把人骗去重开一遍 SSH。
    修法：参数必须是独立的三个（`lsofListenArgs`），并让单测断言
    "参数个数正确且每个参数里都不含空格"。

73. **升级后 launchd 里的面板 job 僵住：新旧版都起不来，二进制却是好的** →
    2026-09-14 mini 从 0.4.10 升 0.5.0 时现场：
    - 看门狗 90 秒等不到新版 → 回滚 → **旧版也等不到** → `result.txt` 写
      `回滚后旧版仍未通过健康检查，需要人工介入`，8443 彻底没人听；
    - `launchctl print system/cn.zizpanel.panel` 只有一句
      `last exit code = 78: EX_CONFIG`（`EX_CONFIG` = 配置错），
      而 `launchd.err.log` / 面板日志**一个字都没有**；
    - 关键反证：同一个二进制、同一份 `config.json`、同样的环境变量
      （`ZIZPANEL_USER` / `ZIZPANEL_ROOT` / PATH），手工执行能正常起来并返回健康页 ——
      所以**不是二进制、不是配置，是 launchd 里那个 job 僵住了**；
    - 现场用 `sudo launchctl bootout system/cn.zizpanel.panel` +
      `sudo launchctl bootstrap system /Library/LaunchDaemons/cn.zizpanel.panel.plist`
      重装一次，立刻恢复，随后重试升级一次通过。
    为什么 kickstart 救不回来：`kickstart -k` 只是"杀掉再拉起同一个 job"，
    job 的注册信息本身坏了时它照样坏。
    现在看门狗在等待期间（1/3 与 2/3 处）会主动做一次"bootout + bootstrap 重装"，
    并且有测试锁死两条约束：这条语句**只能**出现在 `recover_panel()`（健康检查已连续失败）
    里、且**必须紧跟 bootstrap 把它装回去** —— 只摘不装就是当年"升级成功后
    面板从 launchd 里消失、入口 502"那个事故。
    排查提示：遇到"升级完面板没了"，先手工跑一遍
    `/opt/zizpanel/bin/zizpanel serve --config /opt/zizpanel/data/config.json`；
    能起来就别再怀疑二进制，直接去重装 job。

74. **子路径反代：页面返回 200，打开却是白屏** →
    前端构建产物里是**绝对路径**（`<script src="/assets/index-xxx.js">`、
    `fetch("/api/v1")`、`io("/socket.io")`）。挂在 `/iopaint/` 下时这些请求会打到
    站点根路径，于是 HTML 正常、资源全 404 —— 页面一片空白。
    必须做四件事，缺一件就会坏，而且**都不会报错**：
      - 改写响应体里的绝对前缀（HTML/JS/CSS 都算，IOPaint 的 API 前缀就写在
        JS bundle 里）；
      - 改写 `Location` 响应头（Kuma 访问 `/` 会 302 到 `/dashboard`，
        不改就把用户甩出子路径）；
      - 出站请求摘掉 `Accept-Encoding` 并关掉 Transport 的自动压缩，
        否则上游给的是 gzip/br，正文里根本没有明文可替换
        （Go 的 Transport 在请求没带这个头时会自己加 `gzip` 并透明解压，
        所以要显式 `DisableCompression`，不能只 `Header.Del`）；
      - 放行 WebSocket 升级（socket.io）。
    实现见 `internal/appproxy`，有单测锁住每一条。

75. **"HTTP 200 就算通"的探测会骗人 —— 探测要看资源，还要承认人工结论** →
    第一次写的探测只看状态码，于是 `/uptime-kuma/` 判成"可用"：页面 200、
    标题正确、请求全 200，**正文却是 `Page Not Found`** —— 它的 Vue 路由不认这个前缀。
    修法两层：
      - 探测额外取页面引用的 js/css，同源下必须 200（能发现"白屏"那一类）；
      - 对"资源全对、但前端路由不认前缀"这类只有人眼能看出的问题，
        目录里用 `PreferDirect` **人工标记**（Uptime Kuma 就是这样，
        它的官方方案要改容器 entrypoint，面板不做那种侵入）。
        探测结果与人工标记冲突时，以人工标记为准 —— 前提是标记有真机依据。
    界面据此把「直连端口」作为首选入口，而不是给一个点开是错误页的按钮。

76. **phpMyAdmin 显示"已安装·服务未注册"** → 市场把"launchd 里找不到服务"
    当成孤儿态去警告，但 phpMyAdmin **本来就没有守护进程**
    （nginx alias + php-fpm，装完就是一个网页入口）。这是把正常状态报成故障，
    用户会去点「重新部署」，而重新部署也变不出一个不存在的服务。
    修法：目录里给这类应用标 `NoDaemon`，界面改说「已安装·网页入口」，
    并直接把「打开」作为主入口。教训：**"没有 X"不等于"X 坏了"** ——
    先问一句"这个应用本来该有 X 吗"。

---

## 开发路线

| 阶段 | 内容 | 状态 |
|------|------|------|
| P0 | 工程基座、一条命令安装、远程访问、登录鉴权、实时监控、操作审计 | ✅ 已完成 |
| P1 | 两步验证策略、访问策略 UI、审计检索 | 部分完成（2FA 与访问策略已可用；**审计检索与导出已交付**） |
| P2 | 网站管理：站点 CRUD、伪静态、多版本 PHP、SSL 签发、反向代理、诊断 | ✅ 已完成 |
| P3 | 服务管理：裸装 + Docker 统一抽象、Compose、应用市场 | ✅ 已完成 |
| P3 | **Docker 管理页**：容器/镜像/卷/网络、Compose 编辑器与一键部署、创建容器 | ✅ 已完成 |
| P4 | 文件管理、Web 终端 | ✅ 已完成 |
| P4 | 计划任务与备份 | ✅ 已完成 |
| P4 | 日志中心、数据库管理 | ✅ 已完成 |
| P5 | 分发打磨：Mac mini 长期运行、远程一键安装、**在线升级**、备份恢复、跨机迁移 | 部分完成（部署见 [Mac-mini部署指南.md](Mac-mini部署指南.md)；在线升级已完成并真机演练通过；备份/跨机迁移待做） |

---

## 系统设置（把 macOS 配成服务器）

侧边栏「系统设置」这一页**取代了原来的「系统监控」**：监控内容和仪表盘高度重复
（同样的 CPU/内存/磁盘/网络/进程，仪表盘还多了曲线），而"这台 macOS 该怎么配成
服务器"反倒只能靠装机时跑一次 `tools/server-mode.sh`。现在两件事各归其位：

- **看指标** → 仪表盘（原监控页独有的「系统负载」「Swap 使用」已并入仪表盘的「系统信息」，
  没有丢信息）；
- **改 macOS** → 系统设置。

页面上的每一项都遵循同一条纪律：**先显示当前真实值，再提供动作，执行完重新读回复核**。

| 分组 | 做什么 | 依据 |
|------|--------|------|
| 主机状态 | 机型、面板是否 root、SSH 是否在听、自动登录、Tailscale | 只读探测 |
| 一键设为服务器模式 | 电源 + 阻断更新 + 静默诊断 + 关索引 + 开 SSH，一条命令串起来 | `internal/sysconfig` |
| 电源与睡眠 | `sleep`/`disksleep`=0、`womp`=1、`powernap`=0、`autorestart`=1、`displaysleep`=10 | 与 `tools/server-mode.sh` 的取值一致 |
| 系统更新阻断 | 清掉安装类偏好 + hosts 里把 8 个更新域名指向 `0.0.0.0` | 见「阻止系统更新」一节 |
| 崩溃报告与索引 | 关闭崩溃弹窗与自动上报、Spotlight 索引开关 | `CrashReporter`/`mdutil` |
| 远程访问与登录 | 开启 SSH、关闭自动登录 | `launchctl` + `systemsetup` 回退 |

三个来自真机的设计约束：

1. **能力必须先探测。** `pmset -g cap` 决定哪些键这台机器支持 ——
   MacBook Air 上 `autorestart` 不被支持，而 `pmset -a autorestart 1` 会
   **静默返回 0 但什么都不做**。所以不支持的项在界面上直接标成"本机不支持"，
   而不是给一个点了会骗人的按钮。
2. **复核真实值，不看退出码。** 每个动作跑完重新 `Probe()` 一次，
   界面显示的是系统现状，不是"我们请求过要改成什么"。
3. **动作全部走任务中心。** 用户能看到每条 `$ 命令` 与它的输出，
   关掉进度窗也不会中断（任务跑在 `context.Background()` 上）。

---

## 服务管理与应用市场

### 两种管理模式的严格区分

| 模式 | 含义 | 面板能做什么 |
|------|------|--------------|
| **仅纳管** | 你自己装好的服务（如 `com.zizdog.qwen3tts`） | 查状态、启停、看日志、健康检查。**不会卸载它** |
| **面板托管** | 通过应用市场安装的服务 | 完整生命周期，含卸载 |

这个区分不是形式主义：对纳管服务执行"卸载"会删掉用户自己装的东西。
所以面板在 `Uninstall` 这一层直接拒绝纳管服务，并在界面上明确标注。

### 三种驱动

| 驱动 | 适用 | 说明 |
|------|------|------|
| launchd | 有 plist 的原生服务 | 开机自启 + 崩溃自动拉起，由系统保证 |
| 命令 | 只有一条启动命令 | 进程随面板退出（界面会标注），适合先试跑 |
| Docker / Compose | 容器化服务 | 直接用 Docker socket HTTP API，不依赖 docker CLI |

### Docker 管理页

侧边栏「Docker」是一整页 Docker 管理界面，六个分区：

| 分区 | 能做什么 |
|---|---|
| 容器 | 列表（默认含已停止的）、启动/停止/重启、日志、详情（挂载/环境变量/网络）、删除、**创建容器**表单 |
| 镜像 | 列表（含悬空镜像标记）、拉取、删除、清理悬空层 |
| 数据卷 | 列表、删除、清理未使用卷 |
| 网络 | 列表（含子网与连接容器数）、删除、清理未使用网络；内置 `bridge`/`host`/`none` 禁删 |
| Compose | 项目列表、yml 编辑器、一键部署（`up -d`）、停止、删除项目 |
| 已纳管服务 | 列出 kind 为 docker/compose 的服务并跳转到服务管理 |

**它与「服务管理」的分工是刻意的**：服务管理把容器/compose 当**一条服务**管生命周期
（开机自启、健康检查、实时日志流），Docker 页管**引擎里的东西**（有哪些容器/镜像/卷/网络）。
两者会在同一批对象上重叠，所以 Docker 页只做索引与跳转，不把服务列表再抄一遍 ——
两份状态迟早会不一致。

几个刻意的取舍：

- **Docker 不可用不是错误，是要显示的信息。** macOS 上 Docker 不是系统自带能力，
  没装是常态。所有 Docker 接口在环境不可用时返回 **409 + 一句"去哪里装/怎么启动"**，
  Docker 页首屏显示引导卡片（去应用市场装 Colima / 去服务管理启动运行时），而不是红色报错。
- **路由用尾部通配 `{path...}`。** 容器名、镜像名、卷名都可能含 `/`
  （`nginx:1.27`、`library/redis`、`my_proj_db`），而 Go 1.22 的 `{id}` 路径参数
  **不跨 `/`**，用它会对这类名字 404。注意 `{path...}` 只能出现在模式末尾，
  所以"标识符后面还要跟子路径"的接口（容器日志/详情/动作）改成把标识符放查询参数。
- **创建容器只暴露常用字段**（镜像/端口/环境变量/挂载/命令/重启策略/网络/特权），
  不接受自由格式的 HostConfig —— 那等于把整个 Docker API 的 root 等价面开放给浏览器。
- **Compose 项目名是唯一的外部输入**，会参与文件路径拼接，所以做了两道校验
  （字符白名单 + 拼出的路径必须仍在 `<work_dir>/compose` 下）。文件先写临时文件再
  `rename`，避免 compose 读到半个文件。
- **危险操作按"能区分删了什么"设计**：清理只清悬空镜像/未使用卷/未连接网络；
  删正在运行的容器要显式强杀；镜像被占用时不自动强删，而是说明原因并二次确认。

> 创建容器的「启动命令」按**空白**切分成参数数组交给 Docker，不经过 shell、不解析引号。
> 所以 `sh -c 'echo a; sleep 1'` 这种写法在这里**不成立**（会被切成多段）。
> 需要复杂命令请用 Compose，或先写一个脚本文件再执行它。
| Colima | Docker 运行时本体 | 管理承载 Docker 的 Linux 虚拟机（启停、状态、日志） |

Docker 驱动走 socket API 而不是 CLI 有两个好处：用户只装了引擎（Colima 提供 socket）
也能用；参数是结构化的，不存在命令拼接风险。

**Docker 运行时本身也是一条受管服务。** 在 macOS 上 Docker 引擎不是系统自带的守护进程，
而是 Colima 起的 Linux 虚拟机；虚拟机没起来时所有容器一起消失。所以面板把运行时登记为
`docker-runtime`，像 nginx/MySQL 一样可启停、看日志、看状态 —— 否则用户只能 SSH 上去手敲
`colima start`，"全部网页操作"就漏了一块。

两个容易踩的坑，都已固化在代码里：

- **调用 colima 必须显式给 PATH。** colima 靠 PATH 查找 `limactl`，而面板、launchd、
  `sudo -n -u` 三者的默认 PATH 都不含 `/opt/homebrew/bin`，缺了会报
  `dependency check failed for VM: lima not found`——lima 其实是装好的，报错极具误导性。
- **运行时不检查 Docker socket。** 运行时停着时 socket 自然不存在，而这正是最需要启动它的
  时刻；若按其它 docker 驱动那样要求 socket，就会形成"引擎没起来 → 无法从面板启动引擎"的死锁。

开机自启用系统级 LaunchDaemon（`com.zizdog.colima`），开机即跑、不需要任何人登录桌面，
并且会在启动时自动补齐缺失的 plist —— 换一台 Mac 也能得到同样环境，不必手工配置。

#### 为什么是 Colima 而不是 OrbStack

OrbStack 依赖 GUI 会话与 Rosetta，在无人登录的 Mac mini 上安装即失败（实测报
`OSLaunchdErrorDomain Code=125 "Domain does not support specified action"`）。
Colima 基于 Lima，纯命令行、原生 aarch64，无 GUI 也能跑。

### 应用市场

预置应用都经过端口与健康检查路径的实测：

- **AI 服务（原生，可用 Metal）**：Ollama、Qwen3 TTS（纳管本机已有）
- **容器运行时**：Docker 运行时（Colima）—— 其余 Docker 应用的前提，可一键安装
- **运维工具（Docker）**：Uptime Kuma、MinIO、n8n、Gitea、Stirling PDF、
  IT-Tools、File Browser、MetaTube、Squoosh

> Pic Smaller 已移除：其目录条目的镜像 `joyqi/sfz` 在 Docker Hub 上并不存在，
> 该项目也没有官方镜像。保留一个点了必然失败的条目比没有更糟。

安装流程刻意分成两步：**先跑安装前检查，再安装**。
检查会把缺 Docker / 缺 Homebrew 包 / 端口冲突一次列清楚，
并给出可直接复制执行的修复命令 —— 而不是让用户点了安装等几分钟才发现缺依赖。

#### 选品与安装路径：原生优先，其次多架构 Docker

每个条目只走两条路之一，判断顺序固定（2026-09 定的长期规则，AGENTS.md 铁律 8）：

1. **原生（首选）**：`brew info <name>` 能查到 formula 就写 `KindNative` + `BrewFormula`
   （如 `filebrowser` 有 homebrew/core formula，bottle 覆盖 `arm64_tahoe/sequoia`）。
   但 **formula 未必实现 `brew services`**：`brew info --json=v2 <name>` 的 `service`
   字段为 `null`、源码里没有 `service do` 块时，面板的原生路径装完必然在
   `brew services start` 失败，只留下一条 label 指向不存在 plist 的"托管"记录 ——
   市场说已安装、服务管理里永远起不来。这种情况**宁可退到 Docker**，也不留假成功。
2. **原生二进制**：只有官方 GitHub release 的 `darwin-arm64` 产物、没有 formula 时，
   面板目前**没有**"下载 release 二进制 → 写 launchd plist"的通用安装器
   （`internal/upgrade` 那套只服务面板自身升级）。**不要为单个应用临时发明安装器**；
   这类应用先走 Docker，等通用安装器做出来再换（`metatube-server` 就是这种：
   上游发了 `metatube-server-darwin-arm64.zip`，但目录里没有对应的安装路径）。
3. **Docker（兜底）**：镜像必须**自带 `linux/arm64`**。用
   `docker manifest inspect <image>:<tag>` 看 platforms 列表，必须出现
   `linux/arm64`；compose 里**绝不写 `platform:`**，也不靠 Rosetta 转译
   （转译慢、占内存，违背"原生优先"）。`internal/services` 里有一条测试
   （`TestCatalogComposeNeverPinsPlatform`）锁死这一条。端口按既有写法绑
   `<host>:<container>`（0.0.0.0），健康检查路径要能在源码/healthcheck 里找到出处。

本轮 4 个新条目的判定结果（证据都写在 `internal/services/catalog.go` 的条目注释里）：

| 应用 | 路线 | 依据（实测输出摘要） |
|---|---|---|
| IT-Tools | Docker | 官方镜像，`ghcr.io/corentinth/it-tools:latest` → `linux/amd64、linux/arm64` |
| File Browser | Docker | brew formula 存在但无 `brew services` 定义 → 无法托管；官方镜像 `filebrowser/filebrowser:latest` → `linux/amd64、linux/arm64、linux/arm/v7` |
| MetaTube Server | Docker | 官方镜像 `ghcr.io/metatube-community/metatube-server:latest` → `linux/amd64、linux/arm64`（上游另有 darwin-arm64 release 二进制，见上第 2 条） |
| Squoosh | Docker（社区镜像） | 上游**没有**官方镜像（仓库里没有 Dockerfile）；社区 `pjmeca/squoosh:1.1.0` → `linux/amd64、linux/arm64、linux/arm/v7` |

> 网络实测：本机 `registry-1.docker.io` / `hub.docker.com` 直连超时，
> Docker Hub 上的 manifest 只能经镜像站读同一份 image index
> （`docker manifest inspect docker.1ms.run/<image>:<tag>`）；`ghcr.io` 可直连。
> 所以有官方 ghcr 镜像的项目（IT-Tools、MetaTube）优先写 ghcr 地址。
> 选镜像前先确认该仓库能从本机真的拉到。

### Qwen3 TTS：只有一个模型（1.7B-Base-8bit，音色克隆）

2026-09-14 起网站侧插件**只支持「自定义音色」（克隆）**，预置音色（CustomVoice）
整体下线（见 `usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md`），所以服务端只需要
**一个模型**：`mlx-community/Qwen3-TTS-12Hz-1.7B-Base-8bit`。

- 面板的模型清单里因此只有这一条。这不是"省事"，而是**避免渲染出假选项**：
  清单里留着 CustomVoice 的话，界面会显示一个可以点过去的模型，
  而它的权重已经从两台机器上删掉腾空间了 —— 点它就是失败。
  权重约 **2.9GB**（`hf` 缓存里是 blob + 快照软链）。
- **Base 没到位整条链路一音频都出不来**（不是"降级到预置音色"，而是直接失败）。
- 关于"要不要常驻"：结论来自读源码 + 真机实测，而不是估算 ——
  mlx-audio 的 `ModelProvider.load_model` 就是一个**普通 dict**
  （`if model_name not in self.models: ...`），没有 LRU、没有上限、没有 TTL，
  **只有显式 `DELETE /v1/models` 才会移除**。但它只活在**进程内存**里：
  服务一重启（重启机器、崩溃自愈、手动 kickstart）模型就变冷，
  冷加载实测 **25 秒**，热的时候同一句话只要 **2.1 秒** —— 叠加 30~60 秒的合成本身，
  会逼近网站插件那边的 60 秒超时，表现成"合成失败"。
  mlx 走内存映射，`ps` 的 RSS 常常只有几百 MB（权重按页换入），别拿 RSS 当占用。
- 下载一律走**国内镜像**：`HF_ENDPOINT=https://hf-mirror.com` + `HF_HUB_DISABLE_XET=1`
  （不设后者，新版 huggingface_hub 会走 Xet CDN，实测下到一半报错）。
  面板自己的下载路径与写进 plist 的服务环境都用这两个值，见 `qwenHFMirror`。

因此面板做两件事：安装时预热模型；**运行期每 2 分钟守温一次**
（`StartQwenKeepWarm` → `QwenWarmResident`），发现它不在 `/v1/models` 里就补载。
守温里还有一步 `QwenUnloadStale`：把**驻留在内存、但已不在清单里**的模型卸掉 ——
这正是预置音色下线后把 CustomVoice 占的内存还回去的那一步，
以后换模型也不必人工重启服务。界面上仍保留显式「释放内存」（同时跑 Docker、
数据库的机器上用户可能确实想腾地方）。

> 已经被删掉、不要再加回来的：`…1.7B-CustomVoice-8bit`、`…1.7B-CustomVoice-4bit`、
> `…0.6B-CustomVoice-8bit`、`…0.6B-Base-8bit`（`TestQwenManifestDropsRetiredModels` 锁死）。

接收端（8899）自 **v1.3.0** 起还提供**任务队列**（`/jobs/*`）：网站把分块后的文本提交过来，
由本机一个常驻 worker **串行**逐块合成、退化自动重试、拼成整段音频，网站只轮询进度与下载。
这样**浏览器/网站关掉任务也能跑完**（之前是"浏览器开着才跑"，页面一跳走就停在半路）。
状态落盘在 `~/tts/jobs/`，每完成一块原子更新，**进程重启后从已完成处继续**。
契约见网站侧 `usr/plugins/TtsVoice/SPEC-TO-MINI-job-queue.md`；
退化判据与网站逐字一致（`len/字节率 >= max_tokens*0.08*0.95`、最多 3 次）。

**v1.4.0 的三处增量**（见 `usr/plugins/TtsVoice/HANDOFF-TO-PANEL-1.7B.md`）：

1. **`response_format` 支持 wav**。拼接要分格式：mp3 首尾相接；wav 每块自带 44 字节头，
   必须先剥头、拼纯音频、再补一个新头（`merge_chunks`）。`chunk_bytes` 仍返回**交付文件**
   的字节数（wav 含头），下载接口 `Content-Type` 跟着格式走（`audio/wav` / `audio/mpeg`）。
   退化判定的字节率也要跟着走：mp3 是 16000，wav 是 **48000 且先减掉 44 字节头** ——
   否则每块都会算长一点点，短块上足以把边界判反。
2. **`sampling` 透传**：任务里带来的 `temperature/top_p/top_k/repetition_penalty`
   原样放进每块的 `/v1/audio/speech` 请求体；**没有这个字段就用默认值**
   `0.7/0.9/40/1.05`（老请求不带也得能跑）。为什么必须加：上游默认把
   `repetition_penalty` 压成 1.0（等于关掉重复惩罚），长句、中英数混排会**整段变噪音**
   —— 网站侧实测不加 4 次只正常 1 次，加了 4 次全正常。
3. **不做变速**。语速由网站下载后用本机 ffmpeg `atempo` 做（1.25×）；
   接收端再变一次就是双重变速。Qwen 的 `speed` 参数是空实现，保持 1.0。

> 改密钥：密钥是"网站 ↔ 接收端"的共享凭据，会泄露、要轮换。在
> 「服务管理 → TtsVoice 音色接收端 → 详情 → 🔑 更改共享密钥」里换
> （留空自动生成）。它**只重写守护进程的 `--token`**：保留监听地址、脚本、上游，
> 不重写 receiver.py，并且改完会用**新密钥真打一次 `/jobs`**（200）+ 用错误密钥确认仍被拒（403）。
> 换完网站插件的 `openaiKey` 必须同步改（`refUploadToken` 由它推导），否则上传/合成立刻 403。
接口：`POST /jobs`、`GET /jobs/{id}`、`GET /jobs/{id}/audio`（支持 Range）、`DELETE /jobs/{id}`、`GET /jobs`。

接收端（8899）另提供 `GET /voice/status`，只读回报上游**当前驻留**的模型，
并给合成响应加 `X-TtsVoice-Model-Cold` 头，让插件能区分"慢是因为冷加载"还是"真的坏了"。
两者都是纯增量，老插件不调用不受任何影响。

### 应用界面：一键「打开」与子路径代理

有界面的应用在市场卡片上都有「打开」。同一个应用给**两个入口**，用哪个由探测说了算：

| 入口 | 形式 | 说明 |
|---|---|---|
| 子路径 | `http://<主机>/iopaint/` | 经 nginx 的 80 端口（要先生成一次 nginx 入口） |
| 子路径 | `https://<面板地址>/iopaint/` | 面板自己反代，任何入口都通（含隧道 `panel.zizdog.com:8888`） |
| 直连端口 | `http://<主机>:8080/` | 应用自己的端口，永远可用（应用在跑的前提下） |

三个入口能同时成立，是因为**反代做在面板进程里**（`internal/appproxy`）：
面板有多个入口（`:8443` 直连、nginx 的 `/_panel/`、隧道），只在 nginx 里写
location 的话经隧道访问就没了。nginx 那一段只是把 `/<slug>/` 转给面板，
所以**改写规则只有一份实现**。

「生成 nginx 入口」按钮把 `location ^~ /<slug>/` 写进默认 vhost 的成对标记块
（幂等、插在 `location / {` 之前、`nginx -t` 不过就自动回滚），与面板入口
（`tools/takeover-panel-entry.sh`）同构。

**能不能挂子路径，由真机说了算**，不靠"配了就当能用"：
`GET /api/v1/market/proxies` 会逐个探测 —— 应用得先在跑（直连端口有响应），
再经代理取页面，并把它引用的 js/css 也取一遍（只看 200 会漏掉白屏那一类）。
探测不通过（或目录里人工标了 `PreferDirect`）时，界面把「直连端口」当首选入口
并说明原因。目前实测可用的是 **IOPaint**；需要应用侧自己设 base path 的
（Uptime Kuma / n8n / Gitea / MinIO / Stirling）如实标出来，不给假的可用结论。

> 不想让应用经面板暴露？设置里可以关掉「应用界面代理」，
> `/<slug>/` 与全部入口会一起消失。

### 显示名、纳管与健康检查

**界面上显示软件名，不显示 launchd 标签。** 注册表里存的是标签
（`sh.brew.mysql@8.4`、`com.zizdog.qwen3tts`），界面要显示的是「MySQL 8.4」。
映射在读取注册表时兜底应用，所以**已登记的老记录不用重新纳管也会变好看**；
用户手写的名字（如「Docker 运行时（Colima）」）不会被覆盖。

**纳管要按磁盘上真实的 plist 来。** 不能硬编码 `homebrew.mxcl.<formula>`：
实测同一台机器上既有 `homebrew.mxcl.httpd`，也有 `sh.brew.php@8.3`
（Homebrew 前缀被定制过）—— 硬编码的后果是应用市场显示
「已安装·未纳管」，点「纳管」却报"找不到 homebrew.mxcl.php@8.3 的 plist"。
现在按 plist 反推标签，并且**只有服务确实在 launchd 里时才给「纳管」按钮**。

**"装了但服务没注册"要给「重新部署」，而不是「安装」。** 安装产物还在
（`~/iopaint/.venv`），但 plist 没了/从没注册成功 —— 这种孤儿态以前既不在
服务管理里、也没有纳管按钮，用户什么都点不了。现在市场会如实说
「已安装·服务未注册」并给出「重新部署」（安装器是幂等的，会重建服务定义）。

**健康检查失败必须能"接着办"。** 一个红标签等于把排查全丢给用户，所以失败时
卡片里会摆出：检查地址、返回内容、**按失败种类给的建议**、以及三个动作
（重新检查 / 查看日志 / 改检查地址）。其中 `401/403` 专门解释成
"该地址要求身份验证，服务本身很可能是正常的" —— 很多服务（Stirling PDF、
Uptime Kuma 等）本就要求登录，这属于**检查地址选错了**，不是服务坏了。

### 服务日志

用 SSE 实时推送。launchd 与命令驱动用"轮询文件增量"（简单、可取消、
天然支持日志轮转）；compose 用 `docker compose logs -f` 子进程
（多容器日志的顺序只有 compose 自己能正确合并）。

---

## 文件管理与 Web 终端

### 文件管理器

可访问范围是**白名单**（网站目录、面板数据/日志/工作目录），
所有路径都经过 `files.Manager.Resolve` 校验：

1. 字符级检查（绝对路径、无 NUL）
2. **解析符号链接**（`EvalSymlinks`）
3. 确认结果仍落在白名单内

两道检查缺一不可：

- 只做字符串前缀检查 → 目录内的软链接 `link -> /etc` 会让
  `/www/link/passwd` 通过检查，实际读到 `/etc/passwd`
- 只做 realpath 检查 → 目标文件不存在时 realpath 失败，
  无法判断"将要创建的路径"是否越界

旧面板的 `site_del` 就是只做了 `str_starts_with` 前缀校验，
这在文件管理器上后果会严重得多（可删任意文件）。
测试里锁死了三条攻击路径：目录穿越、软链接逃逸、通过软链接写入。

其它安全细节：

- 删除根目录**永远被拒绝**；删除非空目录必须显式 `recursive=true`
- 上传文件名强制 `filepath.Base` 清洗，同名自动加序号（不静默覆盖）
- 解压前扫描归档内条目，拒绝绝对路径与 `../`（zip-slip）
- 写入用"临时文件 + rename"，中断不会留下半截文件
- 新建文件归属真实用户（否则你在 Finder 里编辑会报权限错误）

### 在线编辑器（代码高亮 + 两种全屏）

文件管理器里的"编辑"用的是一个**带代码高亮**的编辑器：

- **高亮**：按扩展名识别 `php` `js/mjs/cjs` `ts` `json` `go` `py` `sh/bash/zsh`
  `yaml/yml` `html/htm` `css` `sql` `ini/conf/cnf` `md`；不认识的语言按纯文本。
  着色覆盖注释/字符串/数字/关键字/函数名/类名（另按语言加变量、键名、标签、属性等）。
- **实现方式**：一个 `<pre>` 放彩色高亮层，上面叠一个**文字透明、只留光标**的 `<textarea>`
  负责真正的输入 —— textarea 是浏览器里行为最稳的输入控件，代价是两层必须逐像素对齐
  （共用同一套字体/行高/内边距/透明边框，滚动用 `transform` 平移高亮层与行号层，
  而不是同步 `scrollTop`：`<pre>` 隐藏滚动条后 `clientHeight` 比 textarea 大，滚到底会错位）。
- **两种全屏**：「最大化」把弹窗撑满视口（`min(96vw,1600px)` / `92vh`，编辑区 `flex:1`）；
  「屏幕全屏」调 Fullscreen API，`Esc` 退出后弹窗仍在。
- **大文件保护**：超过 200KB 跳过高亮（纯文本显示）；<200KB 但 token 过密
  （压缩/单行代码，>2 万）也退回纯文本并提示 —— 否则一个几 MB 的日志会把页面冻住。

> 已知限制（如实说明）：200KB 判据按**字符数**而非 UTF-8 字节，中文文件字节超限但字符
> 未超时仍会高亮；没有做可视区窗口化，所以 100–200KB 的文件输入时有几十毫秒级的
> 重新分词开销（用 rAF 合并到一帧一次，不冻死也不掉字）。

### 任务中心（安装/卸载的实时进度）

装东西是面板里最慢的操作：`brew install`、`docker compose up`、`pip install`、
`hf download` 动辄几分钟到十几分钟。**所以这些操作全都在后台任务里跑**：

- **有过程的**：命令本身（`$ brew install mysql@8.4`）与它的 stdout/stderr
  逐行进任务日志，剥掉 ANSI 颜色，`\r` 进度条也按行切（否则 pip/docker 的进度
  会一直攒到结束才出现）。所以能看见 brew 的下载百分比、docker 的镜像层进度。
  安装代码里的每个"步骤"也实时外发 —— 进度 = 当前步骤 + 真实输出 + 已用时，
  **没有假百分比**（brew/docker 没有可信的总进度）。
- **窗口随时能关、随时能重开**：进度窗右上角关掉只是收起窗口，任务继续跑；
  顶栏的「⟳ N」按钮在任何页面都能重新打开，能看运行中的、也能回看已完成的。
- **不会因为刷新而中断**：任务用 `context.Background()` 派生，**不挂在 HTTP 请求上**。
  以前所有安装都用 `r.Context()`，用户一刷新/关标签页就把正在跑的
  `brew`/`docker` 杀掉了，机器上留下装到一半的状态。
- **可中断**：任务窗里有「中断」，会真的 kill 子进程（会警告可能留下半装状态）。
- **不重复装**：同一个应用已有任务在跑时，接口返回 409 并说明原因，
  而不是让第二个 brew 去抢全局锁然后失败。

范围：应用市场安装（原生 brew + Docker/Compose）、一键 LNMP、phpMyAdmin、
Qwen3 TTS、音色接收端、IOPaint、Docker 运行时，以及服务卸载与 Docker 页的
compose「部署」。`docker compose stop/restart` 这种秒级动作仍是同步返回的。

接口与事件形状见 [`SPEC-任务中心.md`](SPEC-任务中心.md)；
任务只存内存（最近 30 个），面板重启后不保留 —— 进程重启后"正在安装"本身
就是假的，显示成运行中才是谎报。

### Web 终端

**默认关闭**，需在「面板设置 → 文件与终端」显式开启。原因是它等于把本机 shell
交给浏览器 —— 任何能登录面板的人都能执行任意命令。

开启后的约束：

| 约束 | 说明 |
|------|------|
| 身份降权 | 默认以**真实用户**（如 `zizdog`）启动 shell，不是 root；需要 root 时在终端里 `sudo`（走系统审计） |
| 会话上限 | 默认 3 个并发会话 |
| 空闲超时 | 默认 30 分钟自动断开 |
| 完整审计 | 会话开始/结束/强制关闭都写审计日志；可实时查看并强制关闭活动会话 |
| 面板退出清理 | 面板停止时关闭所有会话，不留孤儿 shell |
| 环境补齐 | 面板由 launchd 启动，环境极简；终端会补上 `TERM`/`PATH`/`LANG`，否则中文乱码、找不到 brew 命令 |

前端是一个自实现的 ANSI 渲染器（`ansi.js`，约 400 行），支持 SGR 颜色与样式、
光标移动、擦除、插入/删除行列，用"字符网格 + 行级 diff"渲染以保证长会话不卡。
键盘输入全部透传（Ctrl+C / Ctrl+Z / Tab 都生效），窗口尺寸变化会同步 PTY。

> 不引入 xterm.js 的理由：面板要长期零构建、零外部依赖，而终端核心需求
> （跑命令、看清输出、颜色正确）用可控的 400 行实现即可满足。

---

## 计划任务与备份

### 为什么用 launchd 而不是 crontab

| 维度 | crontab | launchd（本面板的选择） |
|------|---------|------------------------|
| macOS 兼容性 | 受 SIP 与 Full Disk Access 限制，面板改 crontab 常静默失败 | 系统原生，行为确定 |
| 睡眠期间错过的任务 | 不补跑 | 唤醒后补跑 |
| 可观测性 | 出问题几乎无从排查 | 加载状态、退出码、日志都可查 |
| 是否依赖登录 | 依赖用户会话 | LaunchDaemon 在无人登录时也执行 |

每个任务生成一个 `LaunchDaemon` plist（`/Library/LaunchDaemons/cn.zizpanel.cron.<名称>.plist`），
所以面板重启、用户注销都不影响执行。

### cron 表达式 → launchd 的转换

这是本模块最容易出错的地方，因此有独立测试覆盖：

- 支持 `*`、`5`、`1,3,5`、`9-17`、`*/15`、`0-30/5`，以及英文周几（`mon-fri`）
- **`7` 会归一化为 `0`**（cron 里两者都是周日，launchd 只认 0-6）
- 组合展开：`*/5 * * * *` → 288 个日历条件（12 分 × 24 时）
- 组合爆炸保护：条件数上限 400，超出会截断并排序（避免生成巨大 plist）
- 「日」与「周」同时受限时按 cron 的历史语义取**或**

界面上的计划输入框会**实时预览**：中文描述 + 接下来 5 次执行时间，
所以不用猜自己写的表达式什么时候会跑。

### 备份

备份任务的脚本由面板生成，范围可选：网站文件 / 数据库 / nginx 配置 / 面板数据。
产物是单个 `tar.gz`，按保留天数自动清理旧的。

实测（只选 nginx + 面板）：产出 140KB 归档，解压后 vhosts 与面板数据完整可读。

两个刻意的实现选择：

- **数据库密码从不写进脚本**：备份脚本在运行时从 `~/www/.env.local` 读取
  （测试里有一条断言专门检查脚本中不含明文密码）
- 网站备份排除 `node_modules` / `.git` / `vendor`，否则归档会大到不可用

### 状态以系统为准

任务列表显示的是 **launchd 的真实加载状态**，不是数据库里的 `enabled` 字段。
两者不一致时会标红并提示「N 个未注册到系统」，可一键**重新注册全部**修复。
这避免了"界面说启用了、实际根本没在跑"这种最难发现的故障。

---

## 日志中心

聚合 7 类日志：nginx、站点访问/错误、PHP、MySQL、面板、计划任务、纳管服务。

**动态发现而不是硬编码清单**：扫描站点日志目录、面板日志目录、任务日志目录，
新站点或新任务产生的日志会自动出现，不需要改代码。只有少数固定位置
（`php-fpm.log`、MySQL 错误日志）写死在目录里。

读取策略是 **tail 语义**：从文件末尾往前读最多 4MB。php-fpm 的日志动辄几十 MB，
全量读会瞬间吃掉大量内存。同时限制行数与字节数，并给出「已截断」标记。

**清空而不是删除**：`Truncate` 用 `O_TRUNC` 而非 `rm`。删除文件后正在写入的
进程会继续往已删除的 inode 写，直到重启才重建，期间日志全部丢失且空间不释放。
清空前会自动把末尾 512KB 另存为 `.1` 备份 —— 避免"手一抖把事故现场清了"。

只有面板自己产生的日志允许删除；nginx/PHP/MySQL 的日志由第三方软件管理，
面板只提供清空。

## 操作审计

面板里每个敏感动作都会写一条审计记录（站点增删、服务启停、在线升级、文件删除、
SQL 执行、登录成败…共 129 处写入点），「操作审计」页就是用来**回答某一次操作**的：
谁、什么时候、动了什么、成没成。

| 能力 | 说明 |
|---|---|
| 关键词 | 一次搜动作/目标/说明/操作者/IP 六个字段 |
| 精确筛选 | 动作、操作者（下拉项来自库里**真实出现过**的值，带条数） |
| 结果 | 仅成功 / 仅失败 |
| 时间段 | 起止日期；只给日期时"到"补齐到当天 23:59:59（否则会漏掉当天的记录） |
| 概要 | 当前筛选命中多少条、其中失败多少条 |
| 导出 | CSV / JSON，**沿用当前筛选**（页面上看到什么就导出什么） |

两个刻意的设计：

- **游标分页（"加载更多"）而不是页码跳转。** 审计表是只增的：翻页期间新记录插进来，
  `OFFSET` 分页会出现重复或漏掉的记录。用 `id < before_id` 就不漂移了，
  代价是不能跳页 —— 而看审计本来就是从最新往回翻。
- **`LIKE` 通配符必须转义。** 不转义时用户搜一个 `%` 会匹配**全部**记录，
  看起来像搜索坏了；`_` 会变成任意单字符。这个坑有专门的回归测试
  （搜 `_create` 不能命中 `siteXcreate`）。
- 导出**本身也留痕**：把审计日志整段带走是敏感操作，会写一条 `audit_export`。

## 数据库管理

功能：库列表与表结构、账号与权限、SQL 执行、导入导出、慢查询。

**标识符白名单是这里的安全核心**。SQL 里最危险的不是"值"而是"标识符"
（库名、用户名、主机名）—— 值可以参数化，标识符直接进入 SQL 语法结构。
所以每个标识符都做字符白名单校验，而不是转义：

| 对象 | 规则 |
|------|------|
| 库名/表名 | `^[A-Za-z0-9_$]{1,64}$` |
| 用户名 | `^[A-Za-z0-9_.\-]{1,32}$` |
| 主机名 | `^[A-Za-z0-9_.%:/\-]{1,60}$`（支持网段与 IPv6） |
| 权限名 | 必须在已知权限集合内 |
| 字符集 | 白名单 |
| 排序规则 | 前缀必须是已知字符集 |

白名单校验比"加反引号转义"更可靠：转义要考虑内部反引号、字符集、
`NO_BACKSLASH_ESCAPES` 模式等边界，而白名单只有允许/拒绝两种结果。

其它约束：

- **只允许单条语句**，且**拒绝 SQL 注释**（`--` 与 `/* */`）。注释常被用来
  绕过"看起来只有一条语句"的检查
- 面板不允许删除系统库（`mysql` 等）与 `root` 账号
- 删库要求**输入库名确认**，而不只是点"确定"
- 导出用 `--single-transaction`，InnoDB 下不锁表，导出时业务仍可读写
- 数据库密码从 `~/www/.env.local` 读取，面板里不再存一份，避免同步问题
- 审计只记录 SQL 的开头 200 字符，不把数据写进日志

### 顺带修掉的一个真实问题

日志中心上线后立刻暴露：`php-fpm.log` 已涨到 **13MB**，
里面是 35122 条重复的 `unable to bind listening socket for 127.0.0.1:9000`。

根因是 `php`(8.4.7) 与 `php@8.3` 两个 brew 服务都在监听 9000，
8.4 反复启动失败并被 launchd 不断重试。已停掉不用的 8.4 服务、清空日志。

同时修了面板的 PHP 版本检测：原先按 formula 名逐个列出，
界面会出现两个都声称监听 9000 的版本，用户选错就会导致
"我明明选了 8.4 却跑的是 8.3"。现在按 **fastcgi 地址去重**，
并通过一次真实请求读 `X-Powered-By` 显示**实际运行的版本**。

---

## 与现有环境的关系

本面板**不替换**已有的 LNMP 环境。它对 nginx/PHP/MySQL 采用"接管管理"的方式：
沿用现有的 `/opt/homebrew/etc/nginx`、vhost 目录与 brew services，
只在其之上提供界面与自动化。

安装与卸载都**不会**改动：

- 网站源码与数据（`~/www`）
- nginx 主配置与现有 vhost
- MySQL 数据库
- 已有的 LaunchDaemon（如 `cn.zizdog.nginx`）
