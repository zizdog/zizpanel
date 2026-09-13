# 把 ZizPanel 部署到 Mac mini 当家用服务器

本文回答一个具体问题：**能不能把 ZizPanel 装到另一台 Mac mini 上，之后长期无人值守地跑，全部操作都在网页里完成？**

结论：**能，而且整个安装过程只需要你输一次开机密码。**
下面把"能自动化的"和"macOS 真的不允许自动化的"分清楚，不夸大。

> 本文 §2 第 2 条记录了一次**我自己的误判更正**：曾经以为 SSH 必须手工开启，
> 实测证明可以脚本化。留在这里是因为那个坑（`launchctl enable` 之后还需要
> `bootstrap`）非常容易再踩一次。

---

## 一、先给结论（对应最常问的 5 个问题）

| 问题 | 结论 |
|---|---|
| 1. 能否一条命令装完，之后全部在面板里操作？ | **能**。安装 + 面板内的站点/服务/文件/定时任务/终端/数据库/日志全部可网页操作。有 4 项例外见 §6 |
| 2. 能否把机器配成长期稳定的服务器？脚本能做完吗？ | **绝大多数能**。`tools/server-mode.sh` 一条命令搞定电源/更新/索引；**但"断电自恢复"只有台式机支持**，笔记本做不到 |
| 3. 能否只授权一次，之后全自动？ | **能**。`sudo` 只输一次开机密码，之后安装脚本内部不再要密码 |
| 4. macOS 有 root 用户吗？要开启吗？ | **有，但不要开**。面板本身已经通过 LaunchDaemon + 受限 sudoers 拿到了它需要的能力，开 root 只会放大风险 |
| 5. 怎么让 AI 连上去跑真实环境测试？ | **全部可脚本化**。`--server-mode` 会自动开启 SSH，见 §7 |

---

## 二、部署前：先在 Mac mini 上做一次「首次开机」

Mac 出厂第一次开机必须走完设置助手（选语言、建账号、连 Wi-Fi）。这一步无法跳过。

建议：

- 账号名用**纯 ASCII**，不要用中文或空格。安装脚本要拿它做 `/Users/<账号>/www` 路径，中文名虽然大概率能用，但会在各种 shell/nginx 场景里埋雷。本文假设账号是 `zizdog`。
- **不要**在设置助手里开启「自动登录」（服务器不需要）。
- 记住这个账号的开机密码 —— 装面板时要用一次。

---

## 三、推荐路径：从工作电脑一条命令远程安装

不需要 U 盘，也不需要先把安装包拷过去。

### 第 1 步：在工作电脑上启动"安装源"（只服务安装包，只读）

```bash
cd /path/to/zizpanel
make serve-install          # 等价于 bash tools/serve-for-install.sh
```

它会自动构建发布包（arm64 + amd64）、探测本机局域网 IP，并打印出 Mini 上要执行的命令，形如：

```
请在 Mac mini 上执行：
    curl -fsSL http://192.168.1.100:8899/install-remote.sh | sudo bash
```

### 第 2 步：在 Mac mini 上执行那条命令

```bash
curl -fsSL http://192.168.1.100:8899/install-remote.sh | sudo bash
```

想顺便把机器配成服务器模式（禁止睡眠、关自动更新重启等）：

```bash
curl -fsSL http://192.168.1.100:8899/install-remote.sh | sudo bash -s -- --server-mode
```

安装脚本会自动完成：

1. 检查系统、找回真实用户（`sudo` 下 `$HOME` 是 `/var/root`，脚本会自己纠正）
2. 装 Homebrew 依赖（nginx / PHP / MySQL 等，缺什么装什么）
3. 把程序装到 `/opt/zizpanel`，控制命令软链到 `/usr/local/bin/zizpanel`
4. 注册 LaunchDaemon：**开机自启 + 崩溃自动拉起**
5. 写入**最小化** sudoers 授权（只授权一个受限助手，不授权通用 shell）
6. 生成本机受信 HTTPS 证书
7. 把面板加入 macOS 防火墙允许列表
8. **用真实 HTTP 请求探活**，确认服务可用后再打印访问地址

装完终端会打印：

```
远程访问   https://192.168.x.x:8443
本机访问   https://127.0.0.1:8443
```

### 第 3 步：关掉安装源

在工作电脑上按 `Ctrl-C` 即可。这个服务是临时的、只读的，装完就没用了。

### 另一条路：从 GitHub Releases 安装

如果你已经把发布包传到 GitHub Releases，那么在任何 Mac 上都是一条命令：

```bash
curl -fsSL https://github.com/<你>/zizpanel/releases/latest/download/install.sh | sudo bash
```

> 发布包里必须包含 `tools/` 下的工具脚本，否则面板入口（`/_panel`）不会被接管。这一点已由 `tools/remote-install-test.sh` 自动校验。

---

## 四、服务器模式：脚本到底改了什么

```bash
sudo bash /opt/zizpanel/server-mode.sh --dry-run     # 先看要做什么
sudo bash /opt/zizpanel/server-mode.sh               # 真的执行
sudo bash /opt/zizpanel/server-mode.sh --no-spotlight  # 同时关闭 Spotlight 索引
```

| 设置 | 改成 | 为什么 |
|---|---|---|
| `pmset sleep` | `0` | 睡眠 = 服务离线，这是家用服务器第一杀手 |
| `pmset disksleep` | `0` | 磁盘休眠后首次访问要等唤醒，体验很差 |
| `pmset displaysleep` | `10` | 显示器关掉省电，不影响服务 |
| `pmset womp` | `1` | 网络唤醒，作为"万一手滑睡眠了"的双保险 |
| `pmset powernap` | `0` | Power Nap 会在睡眠中跑后台任务，干扰服务 |
| `pmset autorestart` | `1` | **断电/跳闸后自动开机**（仅台式机支持，见下） |
| 自动下载更新 | `关闭` | 避免不经你同意就下载/重启 |
| 自动安装系统更新 | `关闭` | 半夜自动重启 = 服务中断 |
| 安全响应安装 | **保持开启** | 服务器长期联网，缺安全补丁的风险**大于**半夜重启 |
| 崩溃报告弹窗 | `关闭` | 无人值守机器上弹窗只会堆积（日志仍保留） |
| 远程登录（SSH） | `开启` | 这是远程管理的前提；实测可由脚本完成（`--no-ssh` 可关） |
| Spotlight 索引 | `--no-spotlight` 时才关 | 全盘索引持续吃 CPU/磁盘；但关了系统搜索也会失效 |

### 必须知道的两条硬限制

（其中第 2 条已实测更正为"可自动化"，保留原文是为了说明当初为什么判断错。）

**1. `autorestart`（断电自恢复）只有台式机支持。**

脚本会真的去查 `pmset -g cap`，而不是假装设置成功。在笔记本上它会明确告诉你：

```
⛔ 断电恢复后自动开机：本机（Mac16,12）不支持该设置，脚本无法做到
⛔ 本机是笔记本：合盖会休眠，需外接电源+显示器+键鼠才能合盖当服务器
```

之所以强调这点：`pmset -a autorestart 1` 在不支持的机器上**会静默返回成功**却什么都不做。如果脚本不检查就报"已开启断电自恢复"，你会在真的跳闸那天才发现它从没生效。Mac mini 是支持的，所以 Mini 上这条会显示 ✅。

**2.「远程登录（SSH）」是能自动开的 —— 这里我要更正一个曾经的错误结论。**

一度以为它必须手工点，理由是唯一被文档化的命令 `systemsetup -setremotelogin on`
需要调用方拥有「完全磁盘访问权限」（TCC），脚本确实没有。

但那条路不是唯一的路。root 还可以直接操作 launchd：

```bash
sudo launchctl enable    system/com.openssh.sshd
sudo launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist
```

**只做 `enable` 是不够的** —— `enable` 会成功但 22 端口不监听，必须再 `bootstrap`
一次把服务真正加载进来。这个坑我踩过，也是当初误判"必须手工"的原因。

macOS 15.6.1 实测：两条命令后 launchd 立刻监听 22，`systemsetup -getremotelogin`
报告 `On`，与在系统设置里手工打开完全等价（已验证 SSH 横幅与认证流程）。
`server-mode.sh` 现在会自动做这件事，并且**只以"22 端口真的在监听"为准**，
不相信 `launchctl` 的退出码（实测它退出码相当不可靠）。

---

## 五、为什么不需要开启 root 用户

macOS 的 root 用户默认是**禁用**的（`dsenableroot` 才启用）。**不要启用**，原因：

- 面板需要的 root 能力已经通过**两条更窄的路径**拿到了：
  1. **LaunchDaemon**：plist 不带 `UserName` 键 → launchd 以 root 启动面板进程，所以面板能写 `/etc/hosts`、控制 nginx、管理防火墙。
  2. **受限 sudoers**：`/etc/sudoers.d/zizpanel` 只授权 `zizdog` 免密执行 `/opt/zizpanel/bin/zizpanel-helper` 这**一个**程序，而 helper 内部只做白名单操作（子命令 + 结构化参数，不接收 shell 字符串）。**没有**授权 `/bin/bash`、`/usr/bin/*` 这类通用命令。
- 启用 root 会同时开启一个无限制、无审计的账号。对一台**长期联网**的服务器来说，这是纯粹的风险增加，没有任何收益。

**验证当前授权边界：**

```bash
sudo cat /etc/sudoers.d/zizpanel          # 只应看到 helper 一条
sudo cat /Library/LaunchDaemons/cn.zizpanel.panel.plist | grep -A3 UserName   # 应该没有输出
```

第二条第没有输出 = 面板确实以 root 运行（这是设计如此）。这是有意的：面板是运维入口，需要能改系统配置；它通过"最小化 sudoers + helper 白名单"而不是"给用户开 root"来控制风险。

---

## 六、诚实的例外清单

面板不是万能的。以下 4 件事**不能**（或不该）在网页里做：

| 事项 | 原因 | 该怎么做 |
|---|---|---|
| ~~开启 SSH / 远程登录~~ | ~~macOS TCC 限制~~ | **已可脚本化**，见 §2 第 2 条；曾误判为不可自动化 |
| 装系统大版本更新 | 需要重启，会中断所有服务 | 面板里看到通知后，挑时间手工执行 |
| 磁盘分区 / 抹盘 / 恢复模式 | 需要重启进恢复环境 | 用「磁盘工具」 |
| 改 macOS 深层安全设置（SIP、FileVault 开关） | 需要恢复模式或本地授权 | 手工操作 |

另外：**面板管不了自己的升级失败**。如果新版本起不来，脚本会自动回滚到升级前的二进制并把面板拉起来（宁可停在旧版本，也不能让你连入口都没有）；这种情况需要看 `/opt/zizpanel/logs/launchd.err.log` 反馈。

---

## 七、让 AI（或你自己）远程连上去：SSH 与 Tailscale

### 7.1 开启 SSH（已可自动完成）

**推荐：什么都不用做。** `--server-mode` 会自动开启 SSH：

```bash
sudo bash /opt/zizpanel/server-mode.sh          # 默认就会开 SSH
sudo bash /opt/zizpanel/server-mode.sh --no-ssh # 不想开时显式关闭
```

底层就是这两条（想手工执行也可以）：

```bash
sudo launchctl enable    system/com.openssh.sshd
sudo launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist
```

**注意 `enable` 单独执行是没用的** —— 它会返回成功，但 22 端口不会监听，
必须再 `bootstrap` 一次。脚本以"22 端口真的在监听"作为唯一判定标准。

想关掉：

```bash
sudo launchctl bootout  system/com.openssh.sshd
sudo launchctl disable system/com.openssh.sshd
```

验证（脚本之外，在另一台机器上）：

```bash
ssh zizdog@<mini的IP>
```

> 备选路径：`sudo systemsetup -setremotelogin on` 也能开，但**要求调用方拥有
> 「完全磁盘访问权限」**，所以在脚本里通常不可用。上面那条 launchd 路径才是
> 从脚本里能走通的那条。

### 7.2 装 Tailscale（这样在外网也能连，不用做端口映射）

```bash
# 或用 server-mode 的选项：sudo bash /opt/zizpanel/server-mode.sh --with-tailscale
brew install --cask tailscale
```

装好后打开 App 登录一次，机器会获得一个固定 IP（`100.x.x.x`）。之后：

- 面板地址直接换成 `https://<tailscale-ip>:8443`
- SSH 也可以走 Tailscale，**不需要**在路由器上做任何端口转发

**为什么不建议直接做端口映射**：把 8443 暴露到公网，会让面板直接面对全网扫描。Tailscale 是私有网络，只有你自己的设备能连。

### 7.3 给 AI 测试用的最小通道

让 AI 在真实环境里跑测试，只需要你能 SSH 进去。推荐做法：

1. 在 Mac mini 上建一个**专用测试账号**（不要用管理员账号）：
   ```bash
   sudo sysadminctl -addUser ziztest -fullName "ZizPanel Test" -password '换成强密码'
   ```
2. 把它加入 `admin` 组（安装面板需要管理员权限）：
   ```bash
   sudo dseditgroup -o edit -a ziztest -t user admin
   ```
3. 从工作电脑连过去：
   ```bash
   ssh ziztest@<mini的IP>
   ```
4. 装面板、跑测试都在这个账号下进行。

**注意**：`sudo` 会要密码，AI 需要你**明确授权**才能使用你的开机密码。建议的做法是：需要 AI 跑安装/升级时，你本人在场或临时提供密码，跑完即改密码。不要把长期有效的密码写进脚本或仓库。

---

## 八、长期稳定运行检查清单

装完之后逐项确认（打勾即可）：

- [ ] `pmset -g | grep -E 'sleep|disksleep|powernap'` → 都是 `0`
- [ ] `pmset -g cap | grep autorestart` → Mac mini 上应该有输出；有的话确认已开启
- [ ] `defaults read /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates` → `0`
- [ ] 系统设置 → 通用 → 登录项 → 「自动登录」**关闭**
- [ ] 路由器里给 Mini 绑定 **DHCP 静态地址**（否则重启后 IP 变了，书签就失效）
- [ ] 面板里给管理员账号打开 **2FA**（面板在网络上，密码不够）
- [ ] 在面板里确认「终端」仍是你想要的状态（默认**关闭**）
- [ ] 关机再开机一次，确认面板自己起来了：`curl -k https://127.0.0.1:8443/api/v1/health`
- [ ] （可选）拔掉电源再插上，验证断电自恢复（**台式机才有**）
- [ ] （可选）装了 Docker：关机再开机后 `docker ps` 能列出容器（见第十节）

**关于 UPS**：如果想连跳闸都扛住，接一个 UPS 比任何软件设置都管用。

---

## 九、Docker（可选）

Mac 上没有系统自带的 Docker 引擎。面板用的是 **Colima**：它在一个轻量 Linux 虚拟机里跑
Docker，原生支持 Apple Silicon，纯命令行、不需要登录桌面。

**装法**：面板 →「应用市场」→「Docker 运行时（Colima）」→ 安装。脚本会依次
`brew install colima docker docker-compose`、配置开机自启、启动虚拟机，
最后**真的调一次 Docker API** 确认引擎可用（不只看命令返回码）。

装完之后，`docker-runtime` 会像 nginx/MySQL 一样出现在「服务管理」里，可以启停、看日志。

### 几个必须知道的点

- **停止运行时会连带停掉所有容器。** 这是虚拟机的性质，不是面板的限制。
- **开机自启走系统级 LaunchDaemon**（`com.zizdog.colima`），不需要任何人登录桌面 ——
  这正是 Mac mini 无人值守的前提。换机时会自动补齐，不必手工配置。
- **别手动去改 `~/.colima` 的归属**。面板以 root 运行，而 Colima 以你的账号运行；
  该目录必须属于你的账号，否则会报
  `mkdir /Users/<你>/.colima/default: permission denied`。面板已处理这一点。
- **为什么不用 OrbStack**：OrbStack 依赖 GUI 会话与 Rosetta，在无人登录的 Mac mini 上
  安装直接失败（实测 `OSLaunchdErrorDomain Code=125`）。

### Docker 相关故障

| 现象 | 原因与处理 |
|---|---|
| `dependency check failed ... lima not found` | colima 靠 PATH 找 `limactl`，而这个 PATH 里没有 `/opt/homebrew/bin`。面板已显式注入 PATH；手工在 SSH 里跑请先 `export PATH=/opt/homebrew/bin:$PATH` |
| `mkdir .../.colima/default: permission denied` | 该目录归属不对（被 root 建过）。`sudo chown -R $(whoami):staff ~/.colima` |
| 面板显示「未安装 Docker」但 `docker ps` 正常 | 可能只是几秒前的瞬时探测失败（VM 正在启动）。负结果只缓存 5 秒，稍等即自动恢复 |
| 容器开机后没起来 | `docker ps -a` 看退出原因；compose 条目默认带 `restart: unless-stopped` |
| 想看虚拟机日志 | 面板 →「服务管理」→ `docker-runtime` → 日志；或 `~/.colima/_lima/colima/ha.stderr.log` |

---

## 十、故障排查

| 现象 | 先看哪里 |
|---|---|
| 面板打不开 | `sudo launchctl print system/cn.zizpanel.panel` → 看 `state`；再看 `/opt/zizpanel/logs/launchd.err.log` |
| `/_panel` 显示 502 | nginx 反代没生效：`curl -k https://127.0.0.1:8443/api/v1/health` 先确认面板本身活着 |
| 装完不知去哪访问 | 终端里安装结束时会打印地址；也可 `sudo launchctl print system/cn.zizpanel.panel \| grep -A2 ProgramArguments` |
| 重启后没起来 | `sudo launchctl print system/cn.zizpanel.panel`；plist 里应有 `RunAtLoad` 和 `KeepAlive` |
| 找不到网站目录 | 面板「设置」里看「网站根目录」应是 `/Users/<你的账号>/www`，不是 `/var/root/www` |
| 升级后起不来 | 脚本会自动回滚到旧版本；把 `/opt/zizpanel/logs/launchd.err.log` 反馈给开发者 |

常用命令：

```bash
sudo launchctl kickstart -k system/cn.zizpanel.panel   # 重启面板
zizpanel status                                        # 查看面板状态
tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log    # 实时日志
sudo bash /opt/zizpanel/uninstall.sh                    # 卸载（保留数据）
```
