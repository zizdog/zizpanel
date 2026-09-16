# ZizPanel — 工作区规则

> DSH 每个新会话自动加载本文件，所以**只放不变的规则**。
> 开工前先读 **`ZizPanel-当前状态.md`**（当前进度/待办/真机结论/凭据，已 gitignore）；
> 要细节再读 **`DEVELOPMENT.md`**（设计取舍、测试策略、发布流程、坑清单）。
> **`README.md` 只给使用者看**，别再往里塞开发过程。
>
> 文档存在的意义是**少占上下文**：每份文件都有行数预算（见第六节）。
> 历史过程不进文档 —— 进 git 历史，或压成 `DEVELOPMENT.md` 坑清单里的一条结论。

## 项目一句话

只做 macOS 的类宝塔面板：Go 单二进制 + 内嵌原生 ESM 前端（**无构建步骤**）+ SQLite，
一条 `curl | sudo bash` 装完即可远程网页操作。

- **本机（MacBook Air M4）＝ 开发机**：`https://127.0.0.1:8443/<后缀>`。
- **Mac mini M4（`192.168.1.4`）＝ 测试环境**：可以放手重启、卸载、重装、断网做破坏性验证。
- **NAS（`192.168.1.8`）＝ 镜像与备份**，不是本项目机器。
- 版本号以 `internal/version/version.go` 为准（本文不写死）。

---

## 一、铁律（违反会破坏用户环境，没有例外）

1. **只做 macOS。** 可以放心用 `launchctl` / `pmset` / `diskutil` 这类专属能力。
2. **绝不动 `~/www/zizdog.cn`。** 那是另一个项目（TtsVoice）。面板只负责它的 nginx vhost
   与站点注册记录，**不改它的代码、配置、数据库**。
3. **不改 Typecho 核心**（`var/`、`admin/`、`index.php`、`config.inc.php`）
   与**底层环境**（nginx 全局配置、系统 PHP/MySQL 的既有约定）。
4. **绝不重启本机（MacBook Air）** —— DSH 会话跑在它上面，一重启就断。
   需要重启才能验证的事（自启、断电恢复、合盖）**一律去 mini 上做**（它是测试环境）。
5. **`192.168.1.8` 是 `fnnas`（NAS）**：只当镜像/存储用。**不要重启上面的容器**
   （跑着用户的 TTS 等服务）；`nginx -s reload` 可以；新增容器可以；`nginx.conf` 是
   单文件 bind mount，改要**原地覆盖**保住 inode。
6. **破坏性操作前先快照**，并且先用 `git diff` 看清将要发生什么。
7. **不要把任何口令写进仓库。** 真实凭据放 `.panel-credential.local`（已 gitignore）；
   `.release-key/`、`dist/`、`node_modules/`、`ZizPanel-当前状态.md` 同样不进仓库。
   门禁 `tools/check-no-real-credentials.sh` 会拿凭据文件的值搜**将要提交的文件**，命中即失败。
   **口令一旦提交过就必须轮换**（改 git 历史不算修好，旧口令已经在别人手里）。
8. **不发 GitHub、不推任何远程仓库。** 仓库只在本机。发布链路＝
   **本机构建（`make release`）→ 推 NAS 镜像 → 面板自己的在线升级**。
9. **应用市场选品：能原生就原生，不能原生再 Docker，且 Docker 镜像必须自带 `linux/arm64`。**
   原生 = Homebrew formula（`brew info` 有就用）或官方 darwin-arm64 预编译产物 +
   **目录里已有的**安装路径。Docker 只允许镜像本身多架构原生支持 arm64 ——
   **不许 `platform: linux/amd64`、不许 Rosetta 转译**（有测试锁死）。
   加应用走 `docs/新增应用工作流.md`（5 步，每步一条命令），别每次手工摸索。
10. **功能不许谎报成功。** 做不到就返回 error；要降级必须显式写 `DegradeReason`
    （见 `internal/services/ready.go`）。"能谎报成功的功能，比没做更糟"。

---

## 二、每轮改动的收尾动作

1. **`make bump`** —— 功能性更新 `+0.0.1`，patch 到 10 后 `+0.1` 并归零
   （`0.3.0 → … → 0.3.10 → 0.4.0`）。两台机器同时在跑，粒度要细。
2. **`make check`** —— **必须真绿**。写法固定：
   `make check > /tmp/check.log 2>&1; echo "EXIT=$?"`；
   **不要用 `make check | tail`**（管道会掩盖退出码，我踩过）。
3. **部署本机与 mini**，都走**面板自己的在线升级**（`make deploy`），不手工替换二进制；
   并在**两台机器上各自**验证真实行为，结论分开记。
4. **写回 `ZizPanel-当前状态.md`**：分别写清"本机验证到的"与"mini 上验证到的"，
   不要用一台机器的结果代表另一台。
5. **本地提交**：`git diff` → `git add -A` → `git commit`。**不 push。**

---

## 三、验证纪律（这个项目最贵的教训，反复复发）

- **看真实状态，不看退出码、不看 HTTP 200。** 端口在听吗？进程在吗？健康检查里的
  **版本号**对吗？历史上：测试打印过"✓ 自启路径成立"但被测引擎**根本没停过**；
  phpMyAdmin 的错误页也**返回 200**；`launchctl` 的退出码**多次谎报成功**。
- **"没有日志"本身就是信息。** 面板起来后一行日志都没有 → 失败发生在日志初始化**之前**，
  去查配置/目录/证书/文件归属。
- **"校验通过"取决于用谁校验。** 前端语法必须用真正的 ES 解析器
  （`tools/check-js-syntax.mjs`，走 acorn）。`node --check` 曾放过浏览器拒绝的语法，
  结果是**整页白屏**且控制台不给文件名、不给行号。
- **`go build ... | head` 会掩盖失败的退出码**，不要这么写。
- **`-X` 在 `-trimpath` 下会静默失效**（退出码仍是 0），所以 `make release`
  必须**运行产物核对公钥**。
- **单测不许碰真实服务、生产配置、用户真实家目录。** 两道门禁：
  `TestTestServerSandboxedAwayFromRealHome` + `tools/check-test-pollution.sh`
  （跑测试前后给真实目录拍指纹）。本机 8880 上有 TtsVoice 的 Qwen，测试要用
  `Manager.qwenPortOverride` 之类手段隔离；涉及 nginx vhost 的测试必须沙箱化并断言
  生产的 `000-default.conf` 一字未变。
- **长任务必须走任务中心**（`launchTask(...)` → 202 + `task_id`，进度走 SSE）。
  同步请求会让用户只能看"请等待"、关掉窗口就找不回进度，而且任务挂在 `r.Context()` 上 ——
  **用户一刷新就把 brew/docker 杀了**。秒级动作（如 `compose stop`）仍可同步返回。
- **任务执行的命令标签必须从 `cmd.Args` 派生**，手写标签会和实际执行的不一致。
- **手工调面板 API 的三个坑**：基址必须带**面板后缀**；写操作要带 `X-CSRF-Token`
  （值取可读的 `zp_csrf` cookie，双提交模式），否则 403；`GET /api/v1/services`
  的键是 **`list`**，不是 `services`。
- **任何"看起来不对"的结论，先怀疑自己的探测方式**（键名写错、文件后缀猜错都真的发生过），
  再去怀疑对方。

---

## 四、环境速查

| | 本机（MacBook Air M4，开发机） | Mac mini M4（`192.168.1.4`，测试环境） |
|---|---|---|
| 面板 | `https://127.0.0.1:8443/<本机后缀>` | `https://192.168.1.4:8443/<mini后缀>` |
| SSH | — | `ssh zizdog@192.168.1.4`；外网 `ssh -p 22004 zizdog@zizdog.com` |
| 安装根 | `/opt/zizpanel`（二进制在 `/opt/zizpanel/bin/`） | 同左 |
| 控制命令 | `/usr/local/bin/zizpanel` | 同左 |
| 可破坏性操作 | **否**（绝不重启） | **是**（测试环境） |

- 后缀与账号口令见 `ZizPanel-当前状态.md`（**gitignored，不要提交、不要外发**）。
- 构建必须 `export GOPROXY=https://goproxy.cn,direct`（本机 proxy.golang.org 不可达）。
- 非交互 SSH 里 `docker` / `colima` **不在 PATH**，用 `/opt/homebrew/bin/...`；
  mini 上**没有 `timeout` 命令**（curl 用自带 `--max-time`）。
- NAS 镜像路径：`/zizpanel/`（面板发布件）、`/apps/`（应用包）、`/sites/`（Typecho/WordPress）、
  `/brew/`、`/pypi/`、`/hf/`、`/models/`、`/offline/`、`/docker/`、`/zizpanel/clt/`。
  离线/迁移场景用局域网 `http://192.168.1.8:8090`（比公网入口快约 26 倍）。

---

## 五、版本控制

仓库已 `git init`，**只在本机**（不配远程、不 push）。每轮改动：

```bash
git diff                 # 先看清楚自己改了什么
git add -A && git commit -m "一句话说明这一轮做了什么"
```

出错时**不要靠记忆排查**：`git checkout -- <file>` 回滚单个文件，
`git log` / `git diff HEAD~1` 看上一轮改了什么。没有版本控制时，每个新会话都在一个
说不清状态的代码库上继续加东西，错误只增不减、无法回退。

---

## 六、知识分层（改完记得同步，别让文件互相矛盾）

| 文件 | 放什么 | 行数预算 | 变化频率 |
|---|---|---|---|
| `AGENTS.md` | 规则、不变量、门禁（**本文件**） | ≤ 200 | 几乎不变 |
| `ZizPanel-当前状态.md` | **当前**部署版本、本轮真机结论、待办、凭据（gitignored） | ≤ 200 | 每轮更新 |
| `README.md` | **只给使用者**：安装、使用、常见问题 | ≤ 200 | 加功能时 |
| `DEVELOPMENT.md` | 设计取舍、测试策略、发布流程、已修复坑清单 | ≤ 450 | 加功能时 |
| `Mac-mini部署指南.md` | 测试环境的部署与操作 | ≤ 250 | 少 |
| `SPEC-任务中心.md` | 任务中心接口契约 | ≤ 250 | 改协议时 |
| `docs/*.md` | 专题（应用市场下载点、离线打包、新增应用工作流） | 各自 ≤ 600 | 少 |

**规律：坑清单只增不减。** 修掉一个新坑就追加到 `DEVELOPMENT.md`
「已修复的典型坑」一节；那条坑如果会伤到使用者（让安装卡住、或会谎报成功），
再摘要进本文件第三节。**文档超预算时先删过时的，不要新增文件。**

---

## 七、诚实原则

用户明确要过：**不夸大、不谎报**。做不到就如实说"跳过"或"失败"，并且**在汇总里单独列出**
（`make smoke` 就是这么处理特权步骤的）。历史上最危险的一次是 `pmset -a autorestart 1`
在不支持的机器上**静默返回成功**却什么都没做，脚本却报告"已开启断电自恢复" ——
真跳闸那天才会发现。**能谎报成功的功能，比没做更糟。**
