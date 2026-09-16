# Mac mini 测试环境：部署与操作

Mac mini M4（`192.168.1.4`）是 ZizPanel 的**测试环境**，不是生产服务器：可以在它上面
放手做重启、卸载、重装、断网等破坏性验证。本机（MacBook Air M4）是开发机，
**绝不重启本机**（DSH 会话跑在它上面，一重启就断）。

| | 本机（MacBook Air M4，开发机） | Mac mini M4（测试环境） |
|---|---|---|
| 面板 | `https://127.0.0.1:8443/<本机后缀>/` | `https://192.168.1.4:8443/<mini后缀>/` |
| SSH | — | `ssh zizdog@192.168.1.4`；外网 `ssh -p 22004 zizdog@zizdog.com` |
| 安装根 | `/opt/zizpanel`（二进制在 `/opt/zizpanel/bin/`） | 同左 |
| 控制命令 | `/usr/local/bin/zizpanel` | 同左 |
| 破坏性操作 | **不允许** | **允许** |

面板口令、安全后缀等凭据见 `ZizPanel-当前状态.md`（gitignored）。本文不写任何口令。

## 一、SSH 上去

```bash
ssh zizdog@192.168.1.4            # 局域网
ssh -p 22004 zizdog@zizdog.com    # 外网（路由器把 22004 映射到 mini 的 22）
```

确认面板在跑（`/api/v1/health` 故意留在**根路径**，不带安全后缀）：

```bash
curl -k --max-time 5 https://127.0.0.1:8443/api/v1/health
sudo launchctl print system/cn.zizpanel.panel | head -20
```

## 二、装面板

主力路径是从 NAS 镜像一条命令装（内网）：

```bash
curl -fsSL http://192.168.1.8:8090/zizpanel/install.sh \
  | sudo bash -s -- --download-base http://192.168.1.8:8090/zizpanel
```

想顺便配成服务器模式、或顺带装 LNMP，追加 `--server-mode` / `--with-lnmp`。

NAS 不通时用开发机当安装源：在开发机上 `make serve-install`（构建发布包 + 探测局域网 IP），
它会在屏幕上打印 mini 要执行的那条命令，形如：

```bash
curl -fsSL http://<开发机IP>:8899/install-remote.sh | sudo bash
```

安装脚本做的事：装依赖（CLT → Homebrew → ffmpeg 等）→ 装到 `/opt/zizpanel` →
控制命令软链到 `/usr/local/bin/zizpanel` → 注册 LaunchDaemon（开机自启 + 崩溃拉起）→
写最小化 sudoers（只授权受限 helper）→ 生成证书 → 配防火墙 → **用真实 HTTP 请求探活**。

- 脚本是**幂等**的：重装不会动已有的 `panel.db` 与 `config.json`。
- 安装过程用户需要手动点的东西（CLT 弹窗、初始化向导）见 `README.md`。
- 装完终端会打印地址；`sudo zizpanel status` 打印的才是**带安全后缀**的完整地址。

## 三、升级

两台机器都走**面板自己的在线升级**，不手工替换二进制：

1. 打开面板 →「面板设置 → 关于与运维 → 在线升级」。
2. 升级源填 `http://192.168.1.8:8090/zizpanel`。
3. 「检查更新」→「下载并准备升级」→「立即升级」。

发布链路是**本机构建 → 推 NAS 镜像 → 面板在线升级**；仓库不推远程仓库、不发 GitHub。
开发机上一条命令做完（构建 + 签名 + 推 NAS + 并行升级两台 + 读回真实版本）：

```bash
make deploy ZP_PASS='<见 ZizPanel-当前状态.md>'
```

`make deploy` 前会跑 `make check`；NAS 优先用 SSH 密钥登录，密钥不通才需要 `NAS_PASS`。
升级后核对**健康检查里的版本号**，不要只看 HTTP 200：

```bash
curl -k --max-time 5 https://192.168.1.4:8443/api/v1/health
```

新版本起不来时面板会自动回滚到升级前的版本，原因写进 `/opt/zizpanel/logs/launchd.err.log`。

## 四、服务器模式与开机自启

```bash
sudo bash /opt/zizpanel/server-mode.sh --dry-run          # 先看要改什么
sudo bash /opt/zizpanel/server-mode.sh                    # 真的执行
sudo bash /opt/zizpanel/server-mode.sh --no-spotlight     # 同时关 Spotlight 索引
sudo bash /opt/zizpanel/server-mode.sh --no-ssh           # 不开启远程登录
sudo bash /opt/zizpanel/server-mode.sh --with-tailscale   # 顺带装 Tailscale
```

它做的是：关睡眠/磁盘休眠/Power Nap、显示器可以关、开网络唤醒、关自动下载与自动安装系统更新
（安全响应更新保留）、关崩溃报告弹窗、开「远程登录（SSH）」。面板里的
「系统设置 → 一键设为服务器模式」是同一套动作，走任务中心。

- **断电自恢复（`pmset autorestart`）只有台式机支持。** 脚本会读 `pmset -g cap` 真实探测：
  不支持的机器上明确说做不到，不会假装设成功（`pmset -a autorestart 1` 在不支持的机器上
  会静默返回成功却什么都不做）。Mac mini 支持。
- **开机自启 + 崩溃拉起**由 LaunchDaemon `cn.zizpanel.panel` 负责（plist 里带 `RunAtLoad`
  与 `KeepAlive`）。mini 上已经验证过重启后自己起来。
- SSH 是脚本化开启的，底下是 `launchctl enable` + `launchctl bootstrap` 两条；
  只有 `enable` 不会让 22 端口真正监听。脚本以「22 端口在监听」为准，不信 `launchctl` 的退出码。

## 五、破坏性验证（只能在 mini 上做）

| 想验什么 | 怎么做 |
|---|---|
| 重启/开机自启 | `sudo reboot`，起来后先看健康检查**版本号**，再看站点与容器 |
| 断电恢复 | mini 支持 `autorestart`；拔电再插，确认自己起来 |
| 卸载（保留数据） | `sudo /opt/zizpanel/uninstall.sh` |
| 彻底卸载 | `sudo /opt/zizpanel/uninstall.sh --purge`（连数据库、证书、日志一起删） |
| 重装 | 再跑一遍安装命令（幂等；`--purge` 后是干净安装） |
| 断网/离线 | 只放行局域网（pf 规则），验证镜像与离线升级路径；**跑完立刻恢复并复验公网可达** |
| 升级/回滚 | 在面板里真的升级一次；或对 mini 跑 `tools/panel-upgrade.py <面板地址含后缀> <用户> <口令> <升级源> <目标版本>` |
| 系统设置动作 | 面板「系统设置」里跑，或直接 `server-mode.sh`；每条命令与输出都在任务中心 |

纪律：破坏性操作前先看清将要发生什么。NAS 不是本项目机器、跑着用户其它服务：
只把它当镜像/存储用，**不要重启上面的容器**（`nginx -s reload` 与新增容器是可以的）。

## 六、恢复

| 现象 | 处理 |
|---|---|
| 面板打不开 | `sudo launchctl print system/cn.zizpanel.panel` 看 `state`；再看 `/opt/zizpanel/logs/launchd.err.log` |
| 面板要重启 | `sudo launchctl kickstart -k system/cn.zizpanel.panel` |
| 忘了安全后缀 | `sudo zizpanel status`（「本机访问 / 远程访问」带后缀） |
| 忘了口令 | `sudo zizpanel reset-password <用户名>`（交互输入不回显；凭据见 `ZizPanel-当前状态.md`） |
| 证书报不受信任 | 图形界面里 `mkcert -install` 后重启面板；或 `sudo zizpanel gen-cert` 重新自签 |
| 数据坏了要重来 | mini 上可以 `uninstall.sh --purge` 后重装，再重新初始化管理员 |
| 升级后起不来 | 面板应已自动回滚；把 `/opt/zizpanel/logs/launchd.err.log` 反馈给开发者 |
| 断网验证后连不上外网 | 撤掉 pf 规则，复验公网可达再收工 |

## 七、非交互 SSH 的两个坑

- **`docker` / `colima` 不在 PATH**：非交互 SSH 里必须用绝对路径
  `/opt/homebrew/bin/docker`、`/opt/homebrew/bin/colima`。
- **mini 上没有 `timeout` 命令**：curl 用自带的 `--max-time`（`timeout 5 curl …` 会直接失败）。

另外两条手工验证时容易踩的：

- 手工调面板 API 时，基址必须带**安全后缀**（不带后缀会 404）；写操作要带 `X-CSRF-Token`
  （值取可读的 `zp_csrf` cookie，双提交模式），否则 403。
- `GET /api/v1/services` 返回的列表键是 **`list`**，不是 `services`。

## 八、排障速查

```bash
zizpanel status                                             # 状态与访问地址
tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log        # 面板日志
tail -f /opt/zizpanel/logs/launchd.err.log                  # 启动期错误
sudo launchctl print system/cn.zizpanel.panel               # launchd 视角
curl -k --max-time 5 https://127.0.0.1:8443/api/v1/health   # 根路径健康检查（带版本号）
```

「没有日志」本身就是信息：面板起来后一行日志都没有，说明失败发生在日志初始化之前，
去查配置、目录、证书和文件归属，别在业务逻辑里找。
