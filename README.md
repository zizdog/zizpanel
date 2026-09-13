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

---

## 开发路线

| 阶段 | 内容 | 状态 |
|------|------|------|
| P0 | 工程基座、一条命令安装、远程访问、登录鉴权、实时监控、操作审计 | ✅ 已完成 |
| P1 | 两步验证策略、访问策略 UI、审计检索 | 部分完成（2FA 与访问策略已可用） |
| P2 | 网站管理：站点 CRUD、伪静态、多版本 PHP、SSL 签发、反向代理、诊断 | ✅ 已完成 |
| P3 | 服务管理：裸装 + Docker 统一抽象、Compose、应用市场 | ✅ 已完成 |
| P3 | **Docker 管理页**：容器/镜像/卷/网络、Compose 编辑器与一键部署、创建容器 | ✅ 已完成 |
| P4 | 文件管理、Web 终端 | ✅ 已完成 |
| P4 | 计划任务与备份 | ✅ 已完成 |
| P4 | 日志中心、数据库管理 | ✅ 已完成 |
| P5 | 分发打磨：Mac mini 长期运行、远程一键安装、**在线升级**、备份恢复、跨机迁移 | 部分完成（部署见 [Mac-mini部署指南.md](Mac-mini部署指南.md)；在线升级已完成并真机演练通过；备份/跨机迁移待做） |

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
- **运维工具（Docker）**：Uptime Kuma、MinIO、n8n、Gitea、Stirling PDF

> Pic Smaller 已移除：其目录条目的镜像 `joyqi/sfz` 在 Docker Hub 上并不存在，
> 该项目也没有官方镜像。保留一个点了必然失败的条目比没有更糟。

安装流程刻意分成两步：**先跑安装前检查，再安装**。
检查会把缺 Docker / 缺 Homebrew 包 / 端口冲突一次列清楚，
并给出可直接复制执行的修复命令 —— 而不是让用户点了安装等几分钟才发现缺依赖。

### Qwen3 TTS：两个模型都必须常驻

网站插件（TtsVoice）会**按每次请求体里的 `model` 字段**在两种变体之间切换：
上传了音色样本用 `Base`（克隆），没上传用 `CustomVoice`（预置音色）。
两者能力互斥，只装一个必然有一半是坏的，所以面板装的是**两个**。

关于"要不要为了省内存互相驱逐"，结论来自读源码 + 真机实测，而不是估算：

- mlx-audio 的 `ModelProvider.load_model` 就是一个**普通 dict**：
  `if model_name not in self.models: self.models[model_name] = load_model(...)`，
  没有 LRU、没有上限、没有 TTL，**只有显式 `DELETE /v1/models` 才会移除**。
  所以交替请求不会互相挤掉，按请求切模型是安全的。
- 但它**只活在进程内存里**：Qwen 服务一重启（重启机器、崩溃自愈、手动 kickstart），
  两个模型全变冷。冷加载实测 **25 秒**，而热的时候同一句话只要 **2.1 秒** ——
  叠加 30~60 秒的合成本身，会逼近网站插件那边的 60 秒超时，表现成"合成失败"。
- 内存（0.6B-8bit，16GB 机器实测）：单个驻留 **2.14GB**、两个都驻留 **3.99GB**。
  mlx 走内存映射，`ps` 的 RSS 常常只有几百 MB（权重按页换入），别拿 RSS 当占用。
  > 手册里"各 5.6GB、同时 10GB"是网站侧 **1.7B** 模型的数据，不要拿它设计 0.6B 的策略。

因此面板做两件事：安装时预热两个模型；**运行期每 2 分钟守温一次**
（`StartQwenKeepWarm` → `QwenWarmResident`），发现哪个不在 `/v1/models` 里就补载哪个。
在服务管理里手动重启 Qwen 后也会立即补载一次，不必等下一轮。
界面上仍保留了显式「卸载」——同时跑 Docker、数据库的机器上，用户可能确实想腾内存。

接收端（8899）另提供 `GET /voice/status`，只读回报上游**当前驻留**的模型，
并给合成响应加 `X-TtsVoice-Model-Cold` 头，让插件能区分"慢是因为冷加载"还是"真的坏了"。
两者都是纯增量，老插件不调用不受任何影响。

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
