# ZizPanel

macOS 上的网站与服务管理面板（类宝塔），原生支持 Apple Silicon。
装完后用浏览器远程管理这台 Mac，不需要再开终端。

- 一条命令安装，面板自身**零运行时依赖**（单个 Go 二进制，前端内嵌）
- 开机自启、崩溃自动重启、断电恢复
- 网站（nginx/PHP/MySQL）、数据库、文件、计划任务、日志、审计
- 应用市场：原生优先，需要容器时才用 Docker
- 内置国内镜像：无代理也能装、能升级

---

## 一、安装

在目标 Mac 上打开「终端」，粘贴**这一行**（会要求输入一次开机密码）：

```bash
curl -fsSL https://zizdog.com/zizpanel/install.sh | sudo bash
```

> GitHub 能直连的话也可以用
> `curl -fsSL https://raw.githubusercontent.com/zizdog/zizpanel/main/install.sh | sudo bash`。
> **国内网络推荐用上面那条**：GitHub 在国内无代理时基本下不动，
> 镜像脚本会自动从国内源下载。

装完终端会打印访问地址，形如：

```
远程访问   https://192.168.1.100:8443
本机访问   https://127.0.0.1:8443
```

第一次打开会进入初始化向导，设置管理员账号即可使用。

### 安装过程中可能出现的一两次点击（**正常情况下不需要**）

安装脚本会尽量无人值守。只有下面两种情况会打断你，点了就行：

| 情况 | 你会看到什么 | 怎么做 |
|---|---|---|
| 这台 Mac 从没装过「命令行开发者工具」（CLT） | macOS 弹出「安装命令行开发者工具」对话框 | 点「安装」→ 同意许可。**面板自己也会尝试静默装**（走国内镜像下载苹果原包），弹窗这条路只是兜底 |
| 想消除浏览器的证书警告（可选） | 首次访问提示"证书不受信任" | 见下面「三、证书」；不想管就点「继续前往」 |

**Homebrew 与 Python 不需要你手动装**：面板会自己在任务里装
（CLT → Homebrew → Python），在「应用市场 → 网站环境 / TTS」里点一下就有进度。
你只需要在它弹窗时点一次「安装」。

> 如果安装脚本提示"Homebrew 未能自动安装"，**面板本身仍然可用**，
> 只是「网站管理 / 数据库」暂时不能建站点 —— 进面板后到
> 「应用市场 → 网站环境」点安装即可（那里有实时进度，失败可重试）。

### 装到第二台 Mac（可选）

工作电脑上开一个只读安装源：

```bash
make serve-install
```

它会构建发布包、探测局域网 IP，并打印目标机要执行的 `curl … | sudo bash` 命令。
完整流程见 [`Mac-mini部署指南.md`](Mac-mini部署指南.md)。

### 从发布包离线安装

```bash
tar -xzf zizpanel_0.8.8_darwin_arm64.tar.gz
cd zizpanel_0.8.8_darwin_arm64
sudo bash install.sh
```

---

## 二、安装后

### 升级

面板里点：「设置 → 关于与运维 → 在线升级」→ 检查更新 → 升级。
面板会下载、验签、自检、原子替换，失败自动回滚（独立看门狗比对**版本号**，
不是"HTTP 200"）。国内用户把升级源填 `https://zizdog.com/zizpanel` 即可。

### 常用命令

```bash
zizpanel status                       # 运行状态与访问地址
zizpanel info                         # 打印环境路径（排障用）
sudo zizpanel gen-cert                # 重新生成自签证书
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

重置后所有会话立即失效，需要用新密码重新登录。

> 别写成 `sudo zizpanel reset-password admin 'p#ss'`：`#`、`$`、`*`、空格
> 都可能被 shell 吃掉。用交互模式或 `--stdin`。

---

## 三、证书

安装脚本会优先用 **mkcert** 签发本机受信证书；但 `mkcert -install`
需要一次图形界面授权，SSH / 远程会话里做不到，脚本会跳过并告知。

在 Mac 的图形界面里执行一次，之后不再有任何提示：

```bash
mkcert -install
sudo launchctl kickstart -k system/cn.zizpanel.panel
```

或者手动信任：「钥匙串访问」→ 系统 → 拖入
`~/Library/Application Support/mkcert/rootCA.pem` → 双击 → 信任 → 「始终信任」。

不做也行，只是浏览器首次会多一次「继续前往」。

---

## 四、访问入口与账号

| 入口 | 地址 |
|---|---|
| 直接访问 | `https://<本机IP>:8443` |
| 本机直连 | `https://127.0.0.1:8443` |

面板是**单管理员**模型，账号在「面板设置 → 账号与安全」里管理：
改用户名/密码、开启两步验证（TOTP）、查看并强制关闭登录会话。

安全设计（摘要）：密码 bcrypt(cost 12)；会话令牌只存 SHA-256 哈希；
CSRF 只认请求头；连续失败按账号锁定；用户不存在时也跑一次 bcrypt
（响应时间不可区分）；远程访问默认 `any`，可切 `local` / `whitelist`；
默认**不信任** `X-Forwarded-For`（除非你确实挂在反代后面）。

---

## 五、可用的环境变量与参数

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ZIZPANEL_ROOT` | `/opt/zizpanel` | 安装根目录（可装到外置盘） |
| `ZIZPANEL_LISTEN` | `:8443` | 监听地址，如 `:9000` 或 `127.0.0.1:8443` |
| `ZIZPANEL_VERSION` | `latest` | 指定版本 |
| `ZIZPANEL_DOWNLOAD_BASE` | GitHub Releases | 二进制下载源（国内建议 `https://zizdog.com/zizpanel`） |
| `ZIZPANEL_INSTALL_BREW` | `1` | 设为 `0` 跳过自动安装 Homebrew |
| `ZIZPANEL_SKIP_DEPS` | - | 设为 `1` 跳过依赖检查 |
| `ZIZPANEL_SKIP_FIREWALL` | - | 设为 `1` 跳过防火墙配置 |
| `ZIZPANEL_SERVER_MODE` | - | 设为 `1` 等价于 `--server-mode` |
| `ZIZPANEL_NO_SSH` | - | 配合 `--server-mode`：设为 `1` 则不开启 SSH |

| 参数 | 说明 |
|------|------|
| `--server-mode` | 装完顺带配好服务器模式（禁睡眠/关自动更新重启/崩溃报告不弹窗/开 SSH） |
| `--with-lnmp` | 装完顺带装 nginx + PHP + MySQL（国内网络下较久） |
| `--download-base <地址>` | 指定下载源 |
| `--listen <地址>` | 指定监听地址 |
| `--help` | 打印用法 |

---

## 六、目录结构

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
├── run/                   运行时文件
├── work/                  compose 文件、服务数据、备份
├── tools/                 随包分发的辅助脚本
└── uninstall.sh           卸载脚本
```

界面与静态资源全部通过 Go `embed` 打进主程序，**升级只需换一个二进制**。

---

## 七、国内网络

中国大陆无代理时 GitHub 直连基本不通，所以：

- `install.sh` 会先探真实下载地址，官方不通就**自动改用内置镜像**
  （`https://zizdog.com/zizpanel`）。
- 面板升级源填 `https://zizdog.com/zizpanel`。
- 面板装 Homebrew 时会注入国内镜像并关掉自动更新；装 Python 依赖走清华 PyPI，
  模型走 `hf-mirror.com`。
- 应用市场的 GitHub 产物会**先测各源速度再按快的排**。

自建镜像只需要照这个目录放文件：

```
<镜像>/
  install.sh             一键安装脚本
  manifest.json          升级清单（url 指向镜像自己）
  manifest.json.sig      Ed25519 签名
  download/<版本>/…       该版本的包
  download/latest/…      最新包（固定 URL）
  clt/index.json         命令行开发者工具清单
  clt/<产品号>/*.pkg      苹果原始 CLT 包
```

---

## 八、开发

改这个项目请看 **[`DEVELOPMENT.md`](DEVELOPMENT.md)**：设计取舍、测试策略、
发布流程、真机验证记录，以及 80 多条"踩过的坑与修法"清单。

```bash
make dev          # 本地构建
make run-local    # 在 /tmp/zizpanel-dev 调试启动（固定后缀 dev）
make check        # 提交前必须真绿
make release      # 打发布包 + 签名清单
```
