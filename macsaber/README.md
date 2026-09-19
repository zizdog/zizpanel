# mac军刀（MacSaber）

macOS 原生小工具箱：一个 Go 单二进制 + 内嵌网页，浏览器打开即用。
所有处理都在本机完成，不联网、不上传。

当前内置工具按分类列出（页面里每个工具都写明依赖、能不能用、不可用的真实原因）：

| 分类 | 工具 |
|---|---|
| 图片处理 | 格式转换、压缩、缩放、缩略图、图片信息、生成 icns 图标 |
| OCR 视觉 | 图片转文字（中文优先）、二维码/条码、文档扫描校正 |
| PDF | 合并、拆分、提取文本、信息、加打开口令 |
| 音视频 | 媒体信息、音频转码、文字转语音 |
| 文本与编码 | 文档互转、乱码修复、哈希、plist↔JSON、文件对比 |
| 文件与归档 | 查看/新建/解压压缩包、扩展属性 |
| 搜索 | Spotlight 全文搜索、文件元数据 |
| 系统与网络 | 系统概览、网络质量测量、DNS、进程占用、launchd 列表、统一日志、电源 |
| 安全与签名 | 验签与 Gatekeeper、证书列表、pkg 分析、系统安全开关、去隔离标记 |
| 自动化桥接 | 通知、快捷指令列表/运行、打开文件或网址、剪贴板读写 |

> 少数工具在系统缺命令或原生框架不可用时**会如实标"不可用"并说明原因**（例如视频抽音轨需要 ffmpeg）。
> 所有处理都在本机完成，不联网、不上传。

---

## 一、构建与启动

需要 Go 1.25 或更新（只用标准库，无第三方依赖）。

```bash
cd macsaber
go build -o macsaber .          # 产物就是单文件 macsaber
```

启动：

```bash
./macsaber serve --data ~/Library/Application\ Support/macsaber --listen 127.0.0.1:8895
```

打开 `http://127.0.0.1:8895`，首次访问会要求设置本机用户名与口令（口令至少 8 位）。

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--data <dir>` | `~/Library/Application Support/macsaber` | 放配置与审计日志 |
| `--listen <host:port>` | `127.0.0.1:8895` | 默认只监听本机回环 |
| `--read-root <path>` | `$HOME` | 允许读的根，可重复 |
| `--write-root <path>` | `~/MacSaberFiles` | 允许写的根，可重复；首次启动自动创建 |
| `--log-level <level>` | `info` | `debug` / `info` / `warn` / `error` |

另有 `macsaber version` 打印版本。

---

## 二、安全边界（请先读这一段）

- **只在本机回环监听**：默认 `127.0.0.1`，局域网/公网都访问不到。
  要远程用，请自己在前面加一层 HTTPS 反代（如 Caddy/Tailscale），别把端口直接暴露出去。
- **读**：只能读 `--read-root` 里面的文件；`~/Library/Keychains`、`~/.ssh`、
  `~/Library/Cookies`、`/etc`、`/var/db`、`/System` 等敏感目录**一律拒绝**，
  并会告诉你拒绝的真实原因。
- **写**：只能写 `--write-root` 里面；输出重名时自动加 `-1`、`-2`，**不会覆盖**已有文件。
- **危险工具**：元数据里标了 `danger` 的工具（去隔离、删除、改签名、跑快捷指令、
  `launchctl` 操作等）必须在页面上勾选「我已知晓风险」，后端也会强制校验；
  没有确认字段的请求直接被拒绝，且**不会执行任何动作**。
- **审计**：每次执行都追加一行 JSON 到 `<data>/audit.log`（谁、什么工具、参数摘要、
  成败、耗时）。口令类参数只记「已提交」，不记值。
- **不拼 shell**：所有命令都用 `exec.CommandContext` 逐个参数调用，绝不 `sh -c` 拼用户输入。

---

## 三、登录与会话

- 首次访问：设置用户名与口令（口令用 PBKDF2-SHA256 + 每用户随机盐存储）。
- 之后访问：口令登录；会话 Cookie 是 HttpOnly，写操作要求 CSRF 双提交头（前端自动带）。
- 忘记口令：停掉服务，删掉 `<data>/config.json`，重启后重新初始化。
  （审计日志 `audit.log` 不受影响，可单独保留。）

---

## 四、数据与文件位置

| 路径 | 内容 |
|---|---|
| `~/Library/Application Support/macsaber/config.json` | 账号哈希、盐、会话密钥（0600） |
| `~/Library/Application Support/macsaber/audit.log` | 审计日志（JSONL，0600） |
| `~/MacSaberFiles/` | 默认可写根，工具产物落在这里 |

卸载：删掉二进制、上面两个目录即可，不会在系统里留别的东西。

---

## 五、开机自启（由面板安装时创建）

以**当前用户**运行（LaunchAgent，**不要 root**）。
预留 label：`cn.macsaber.web`，将来由 ZizPanel 应用市场安装时写入
`~/Library/LaunchAgents/cn.macsaber.web.plist`，本仓库本轮不提供 plist。

手工临时自启可以参考（**本轮未在真机验证**）：

```bash
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/cn.macsaber.web.plist
launchctl kickstart gui/$UID/cn.macsaber.web
launchctl print gui/$UID/cn.macsaber.web
```

---

## 六、常见问题

**口令正确但登录后又回到登录页？**
确认浏览器没有禁用 Cookie；反代场景下不要给 `127.0.0.1` 的响应加 `Secure` Cookie。

**访问返回 403「不在可读根内」？**
路径不在 `--read-root` 里。启动时加上你要用的根，或把文件放进已有的根。

**工具显示不可用（灰色）？**
元数据里的 `unavailable_reason` 会说明真实原因，通常是系统缺对应命令。

**图片转换失败？**
检查输入是不是真的图片文件；`sips` 遇到损坏文件会报「Cannot extract image from file」。

**任务一直转圈？**
任务详情里有逐行日志与失败原因。任务跑在服务端，关掉浏览器不会中断；
重新打开页面可以在任务列表里找回（当前版本前端只显示刚提交的那个任务）。
