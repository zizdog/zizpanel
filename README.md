# ZizPanel

macOS 上的网站与服务管理面板（类宝塔），原生支持 Apple Silicon。装完用浏览器远程管这台 Mac，
不用再开终端。单条命令安装，面板自身零运行时依赖（一个 Go 二进制，前端内嵌），
开机自启、崩溃自动拉起。

## 一、安装

在目标 Mac 的「终端」里粘贴这一行（会要求输入一次开机密码）：

```bash
curl -fsSL https://zizdog.com/zizpanel/install.sh | sudo bash
```

安装过程**不问任何配置**：监听端口默认 **8443**，你只需要在"确认安装"那一步回车（`ZP_YES=1` 则完全不问）。
**管理员账号、面板路径后缀、SSH、内网预授权都不在安装期问** —— 它们都在面板里点一下就能做，
而且账号本来就是"面板首次访问时设置"、后缀在「面板设置 → 访问与安全」随时改：

| 安装后再做的事 | 去哪里 |
|---|---|
| 设置管理员用户名与口令 | 打开面板，**首次访问的初始化向导** |
| 开启「后缀安全加强」（面板路径带一串随机后缀） | 面板设置 → 访问与安全（留空即不启用） |
| 开启 SSH（远程登录） | mac设置 → 远程登录 |
| 开启「允许免授权访问内网段」 | mac设置 → 局域网访问（**需要重启才生效**） |
| 装 Homebrew / nginx / PHP / MySQL（基础环境），或 CLI 开发工具链（CLT） | 面板「仪表盘」的引导横幅（走国内镜像 / 镜像整包，任务中心可见进度） |

全自动安装（CI 或无人值守）：`ZP_YES=1` 一个问题都不问；要给账号就同时给 `ZP_USER` + `ZP_PASS`。
**不给账号时安装器什么都不问、也不会卡住** —— 账号留给面板。全部变量见下一节。

给另一台机器装：`make serve-install` 会构建发布包并打印目标机要执行的
`curl … | sudo bash` 命令。

### 安装时用户需要手动做什么

| 时机 | 你会看到 | 怎么做 |
|---|---|---|
| 想换监听端口 | 安装器默认 8443、不提问 | 装之前给 `ZP_PORT=9000`；装之后改 `/opt/zizpanel/data/config.json` 的 `listen` 再 `sudo launchctl kickstart -k system/cn.zizpanel.panel`（运行中改端口会失联，所以设置页不提供该开关） |
| 浏览器提示证书不受信任 | 「您的连接不是私密连接」 | 点「继续前往」即可；想彻底消除见「五、证书」 |
| 需要 CLI/开发工具链（CLT）时 | 在面板「仪表盘」的引导横幅里点安装 | 面板走**镜像整包**静默安装（632MB 分片），不会弹 Apple 的对话框 |

> 设计原则（2026-09-17 真机安装后按用户要求收敛）：**安装器只做"把面板装起来并启动"这一件事**，
> 凡"能在线做、能改、需要重启、会弹窗"的一律留给面板 —— 所以安装界面不弹任何窗口、
> 不调用 `python3`（全新 macOS 上它只是个空壳，一调用就弹"需要命令行开发者工具"对话框）。

### 可选参数

| 参数 / 变量 | 说明 |
|---|---|
| `ZP_USER` / `ZP_PASS` | 管理员用户名 / 登录口令（**安装期建账号用，可选**；不设则留给面板初始化向导） |
| `ZP_SUFFIX` / `ZP_PORT` | 面板路径后缀（可选，默认不启用）/ 监听端口（可选，默认 8443） |
| `ZP_YES=1` | 所有提问都用默认答案（默认答案保证能装成功） |
| `ZP_SSH=1` | 安装期就开 SSH（不设时不动 SSH，留给面板） |
| `ZP_LAN_PREAUTH=1` / `ZP_LAN_CIDR` | 安装完写入「免授权访问内网段」（不设时不做；需重启生效） |
| `ZIZPANEL_INSTALL_BREW=1` | 安装期就走镜像装 CLT+Homebrew（默认不装，留给面板「基础环境」） |
| `--dry-run` / `ZIZPANEL_DRY_RUN=1` | 只演练整条流程，不做任何修改 |
| `--server-mode` | 装完顺带把机器配成长期在线的服务器（关睡眠、阻断自动更新重启、开 SSH） |
| `--with-lnmp` | 装完顺带装 nginx + PHP + MySQL（国内网络下要十几到几十分钟） |
| `--download-base <地址>` | 指定二进制下载源（默认公网 `https://zizdog.com/zizpanel`，探测不通才回落） |
| `--listen <地址>` | 监听地址，默认 `:8443` |
| `ZIZPANEL_ROOT` | 安装根目录，默认 `/opt/zizpanel`（可装到外置盘） |
| `ZIZPANEL_SERVER_MODE=1` | 等价于 `--server-mode` |

> `ZP_*` 与 `ZIZPANEL_*` 两组变量等价（例如 `ZP_SUFFIX` = `ZIZPANEL_PANEL_SUFFIX`）。

离线安装：发布包在 `https://zizdog.com/zizpanel/download/latest/`，
解开后 `cd` 进去执行 `sudo bash install.sh`。

## 二、使用

### 打开面板

安装结束终端会打印访问地址。面板带一个随机**安全后缀**（仿宝塔「安全入口」），
不知道后缀的人连登录页都看不到，地址形如 `https://<本机IP>:8443/<后缀>/`。
忘了后缀就在机器上执行 `zizpanel status`，输出的「本机访问 / 远程访问」是带后缀的完整地址。
局域网用 `https://<本机IP>:8443/<后缀>/`，在本机直连就把 IP 换成 `127.0.0.1`。
具体后缀不在文档里写死（它本身就是一道门），见机器上的 `zizpanel status` 输出。

面板是**单管理员**模型：改用户名/密码、开启两步验证（TOTP）、查看并强制关闭登录会话，
都在「面板设置 → 账号与两步验证」。

### 左侧导航

- **总览**：`仪表盘`、`导航页`（可当浏览器首页的独立导航）、`mac设置`（电源与睡眠、
  系统更新阻断、崩溃报告与 Spotlight、远程登录 SSH，含「一键设为服务器模式」）
- **网站**：`网站管理`、`反向代理`、`SSL 证书`、`数据库`
- **服务器**：`应用`（页内四个 Tab：已安装 / 应用市场 / docker / 一键建站）、`Docker`
- **运维**：`文件管理`、`Web 终端`、`计划任务`
- **系统**：`磁盘管理`、`面板设置`、`日志`（日志中心 + 操作审计）

### 建站

「网站管理 → 新建站点」：填域名、运行目录、PHP 版本，选「路由 / 伪静态模板」
（Typecho、WordPress 等预设），也可以直接填一个反代地址。建好后到站点详情的
「SSL 证书」页启用 HTTPS。每个站点有「诊断」，一次跑完 HTTP 探测、PHP 探针、
证书检查与错误日志。

### 应用市场

分类：**网站环境**（Nginx、PHP 8.2/8.4、MySQL 8.4、PostgreSQL、Python 3.10–3.13）、
**AI 服务**（Qwen3 TTS、TtsVoice 音色接收端、IOPaint、图片压缩）、**运维工具**
（phpMyAdmin、Uptime Kuma、Gitea、Miniflux、Syncthing、Alist、frpc、ddns-go 等）、
**一键建站**（Typecho、WordPress、FreshRSS、Flarum、Emlog、Kodbox 等）。

- 「网站管理」顶部有「⚡ 一键安装 LNMP 环境」：装 nginx + PHP + MySQL 并做收尾配置。
- 「一键建站」会自动下载源码、建库、建站点并套用伪静态。
- 能原生装就原生装（Homebrew / 官方 darwin-arm64 产物）；**Docker 应用面板不代装**，
  只在 Docker 页给一份可参考的预配置 compose，镜像必须原生支持 arm64。

### 任务中心

安装、卸载、建站、证书申请、Docker 部署、系统设置动作这类**几分钟到十几分钟**的操作
都是后台任务：提交后立刻返回，进度走 SSE，顶栏「任务中心」有徽标。
**关掉窗口任务不会中断**，随时能从顶栏重新打开看；任务需要输入时会弹输入框。

### 在线升级

「面板设置 → 检查更新」：

1. 升级源默认已填好公网 `https://zizdog.com/zizpanel`（放着 `manifest.json` 与
   `manifest.json.sig` 的目录）。面板会按「你填的源 → zizdog.com → 备用镜像 →
   GitHub」的顺序探测，装面板时脚本已把可用源写进配置。
2. 「检查更新」→「下载并准备升级」→「立即升级」

面板会验签、试运行自检、原子替换；新版本起不来会自动回滚到升级前的版本。
没有外网或升级源不通时，用同一页的「离线升级（上传发布包）」。

## 三、常用命令

```bash
zizpanel status                       # 运行状态与访问地址（带安全后缀）
zizpanel info                         # 打印环境路径（排障用）
sudo zizpanel gen-cert                # 重新生成自签证书
sudo zizpanel reset-password admin    # 重置密码（交互输入，不回显）
sudo launchctl kickstart -k system/cn.zizpanel.panel   # 重启面板
tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log   # 查看日志

sudo /opt/zizpanel/uninstall.sh          # 卸载（保留数据）
sudo /opt/zizpanel/uninstall.sh --purge  # 彻底卸载（含数据）
```

### 忘记密码

面板**不提供**网页重置密码入口（那等于给攻击者留后门）。在本机终端执行：

```bash
sudo zizpanel reset-password admin          # 交互输入，不回显
printf '%s' '新密码' | sudo zizpanel reset-password admin --stdin   # 脚本里用这个
```

重置后所有会话立即失效。别写成 `sudo zizpanel reset-password admin 'p#ss'` ——
`#`、`$`、`*`、空格都可能被 shell 吃掉。

## 四、目录结构

```
/opt/zizpanel/
├── bin/
│   ├── zizpanel           主程序（Web 服务 + CLI）
│   └── zizpanel-helper    受限提权助手（只接受白名单子命令）
├── data/
│   ├── config.json        配置（权限 600，含会话签名密钥）
│   ├── panel.db           SQLite（用户/站点/服务/审计/会话）
│   └── tls/               HTTPS 证书
├── logs/                  按天滚动的日志
├── run/  work/            运行时文件、compose 与备份
├── tools/                 随包分发的辅助脚本
└── uninstall.sh           卸载脚本
```

界面与静态资源全部通过 Go `embed` 打进主程序，**升级只需换一个二进制**。

## 五、证书

安装时若有 mkcert，脚本会用它签发本机受信证书；但 `mkcert -install` 需要一次图形界面授权，
SSH / 远程会话里做不到，脚本会跳过并告知。在图形界面里执行一次即可，之后不再有提示：

```bash
mkcert -install
sudo launchctl kickstart -k system/cn.zizpanel.panel
```

不做也行，只是浏览器首次访问多一次「继续前往」。

## 六、镜像与国内网络

发布件与应用包都走自建镜像，仓库不发布到 GitHub、不推任何远程仓库：

- 面板发布件（`install.sh`、升级清单、发布包）：**公网 `https://zizdog.com/zizpanel`**
  （任何网络都能用）
- 面板设置里的「应用包镜像基址」默认 `https://mirror.zizdog.com:8888`（公网入口）

**镜像优先，且按具体资源判断**：每一项都先探镜像、探不通才回落官方源，不会卡住。
装 Homebrew 会注入国内镜像（安装脚本本身也走中科大镜像），
Python 依赖走国内 PyPI，AI 模型走 `hf-mirror.com`。
