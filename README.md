# ZizPanel

macOS 上的网站与服务管理面板（类宝塔），原生支持 Apple Silicon。装完用浏览器远程管这台 Mac，
不用再开终端。单条命令安装，面板自身零运行时依赖（一个 Go 二进制，前端内嵌），
开机自启、崩溃自动拉起。

## 一、安装

在目标 Mac 的「终端」里粘贴这一行（会要求输入一次开机密码）：

```bash
curl -fsSL http://192.168.1.8:8090/zizpanel/install.sh | sudo bash -s -- --download-base http://192.168.1.8:8090/zizpanel
```

上面这条走内网 NAS 镜像；公网可用内置镜像 `https://zizdog.com/zizpanel`（内容与 NAS 一致）。
给另一台机器装、手上没有 NAS 时，在开发机上跑 `make serve-install`：它会构建发布包并打印
目标机要执行的 `curl … | sudo bash` 命令。仓库只在本地，不推远程仓库、不发 GitHub。

### 安装时用户需要手动做什么

| 时机 | 你会看到 | 怎么做 |
|---|---|---|
| macOS 从没装过「命令行开发者工具」（CLT） | 弹出「安装命令行开发者工具」对话框 | 点「安装」→ 同意许可。面板自己也会先试静默装（走镜像），弹窗只是兜底 |
| 第一次打开面板 | 「首次初始化」页 | 设置管理员账号（用户名 + 密码），以后用它登录 |
| 浏览器提示证书不受信任 | 「您的连接不是私密连接」 | 点「继续前往」即可；想彻底消除见「五、证书」 |

Homebrew、Python、ffmpeg 都不用手动装：面板会自己在任务里按 CLT → Homebrew → 基础依赖的顺序装，
进度在「任务中心」实时可见。

### 可选参数

| 参数 / 变量 | 说明 |
|---|---|
| `--server-mode` | 装完顺带把机器配成长期在线的服务器（关睡眠、阻断自动更新重启、开 SSH） |
| `--with-lnmp` | 装完顺带装 nginx + PHP + MySQL（国内网络下要十几到几十分钟） |
| `--download-base <地址>` | 指定二进制下载源 |
| `--listen <地址>` | 监听地址，默认 `:8443` |
| `ZIZPANEL_ROOT` | 安装根目录，默认 `/opt/zizpanel`（可装到外置盘） |
| `ZIZPANEL_SERVER_MODE=1` | 等价于 `--server-mode` |

离线安装：发布包在 NAS 的 `http://192.168.1.8:8090/zizpanel/download/latest/`，
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

- **总览**：`仪表盘`（负载/磁盘/站点概览）、`系统设置`（电源与睡眠、系统更新阻断、
  崩溃报告与 Spotlight、远程登录 SSH，含「一键设为服务器模式」）
- **网站**：`网站管理`、`反向代理`、`SSL 证书`、`数据库`
- **服务器**：`服务管理`、`应用市场`、`Docker`
- **运维**：`文件管理`、`Web 终端`、`计划任务`、`日志中心`
- **系统**：`操作审计`、`面板设置`

### 建站

「网站管理 → 新建站点」：填域名、运行目录、PHP 版本，选「路由 / 伪静态模板」
（Typecho、WordPress 等预设），也可以直接填一个反代地址。建好后到站点详情的
「SSL 证书」页启用 HTTPS。每个站点有「诊断」，一次跑完 HTTP 探测、PHP 探针、
证书检查与错误日志。

### 应用市场

分类：**网站环境**（Nginx、PHP 8.1/8.2/8.3/8.4、MySQL 8.4）、**AI 服务**
（Qwen3 TTS、TtsVoice 音色接收端）、**运维工具**（phpMyAdmin、IOPaint（图片去水印）、
Uptime Kuma、n8n、Gitea、MinIO 等）、**一键建站**（Typecho、WordPress）。

- 顶部「⚡ 一键安装 LNMP 环境」：装 nginx + PHP + MySQL 并做收尾配置。
- 「一键建站」会自动下载源码、建库、建站点并套用伪静态。
- 能原生装就原生装（Homebrew / 官方 darwin-arm64 产物），需要容器时才用 Docker，
  且镜像必须原生支持 arm64。

### 任务中心

安装、卸载、建站、证书申请、Docker 部署、系统设置动作这类**几分钟到十几分钟**的操作
都是后台任务：提交后立刻返回，进度走 SSE，顶栏「任务中心」有徽标。
**关掉窗口任务不会中断**，随时能从顶栏重新打开看；任务需要输入时会弹输入框。

### 在线升级

「面板设置 → 关于与运维 → 在线升级」：

1. 升级源填 `http://192.168.1.8:8090/zizpanel`（放着 `manifest.json` 与
   `manifest.json.sig` 的目录）
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

- 面板发布件（`install.sh`、升级清单、发布包）：NAS `http://192.168.1.8:8090/zizpanel`
- 面板设置里的「应用包镜像基址」默认 `https://mirror.zizdog.com:8888`（NAS 的公网入口，
  内容与局域网地址一致）

镜像优先，且按具体资源判断——镜像上没有的包会回落，不会卡住。装 Homebrew 会注入国内镜像，
Python 依赖走国内 PyPI，AI 模型走 `hf-mirror.com`。
