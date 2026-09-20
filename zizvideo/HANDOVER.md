# zizvideo 交接文档

> 写于 2026-09-20。接手方的唯一入口。只记录**结论 + 未验证项**，过程不进这里。
> 真值：配置 `internal/config/config.go`、路由 `internal/web/router.go`、CLI `cmd/server/main.go`。

---

## 1. 一句话定位

**本地局域网短播放器**：把自己的视频目录扫进 SQLite，用浏览器上下滑着看。明确**不做**：

| 不做 | 说明 |
|---|---|
| 上传 | 服务器不接收任何用户上传，前端无入口。**故意不做**，不是待办 |
| 推荐 / 分类 / 标签 | Phase2 以后再说 |
| 社交（评论 / 关注 / 分享） | 只有单人 like/dislike，给本人排序用 |
| 公网分发 | 默认只绑 `127.0.0.1`；要给别人看靠反代，不靠本进程 |
| 多实例 | SQLite 单写者，两个进程写同一份 DB 会互相干扰 |
| 转码 / HLS | `compatibility.direct=false` 只如实提示，不生成兼容版本 |

Docker 与飞牛 fnOS **属于分发适配**，不是新功能。本项目只做 macOS 原生；Linux 适配是接手方的活。

---

## 2. 当前状态（诚实版）
### 2.1 版本

`zizvideo 0.1.0-mvp`。真值有四处，**当前一致**：

| 来源 | 值 |
|---|---|
| `internal/api/api.go:25` | `var Version = "0.1.0-mvp"`（编译期默认） |
| `Makefile:2` | `VERSION ?= 0.1.0-mvp`（`-ldflags -X` 覆盖） |
| `deploy/fnos/Dockerfile:10` | `ARG VERSION=dev`（**不一致：容器默认是 `dev`**） |
| `./dist/zizvideo --version` 实跑 | `zizvideo 0.1.0-mvp` |
### 2.2 已实现功能

- **鉴权**：首启初始化（PBKDF2-SHA256，210000 次迭代 + 随机盐）、登录/登出、会话 cookie、CSRF 双提交、登录退避 + 账号锁定；用户管理（建号/改角色/禁用/改密），禁用与降权**下一次请求**即生效（鉴权每次回查 DB）。
- **媒体库 + 扫描**：库 CRUD + 后台异步扫描（202 + task_id，进度写库，前端**轮询**，无 SSE）；
  忽略规则、扩展名白名单、`size+mtime_ns` 增量跳过、ffprobe 元数据、ffmpeg 抽首帧封面、
  失败分类 + 1s/4s/16s 重试、**两阶段删除**、根路径消失/设备号变化判 `interrupted` 且不删。
- **允许根**：HTTP 增删 + `roots` 子命令 + 直接改 JSON 三条路；原子写回 + 回读复核。
- **播放**：`http.ServeContent`，天然 `Accept-Ranges` / 206 / `Content-Range` / 416；多段 Range 回退 200 全量。
- **feed**：游标分页、进度续播、收藏、点赞；后台页含库/媒体/允许根/用户/系统信息。
- **可观测**：JSON 结构日志（不写媒体路径，只写 `media_id`）+ 写操作审计；
  `/healthz` 只查进程，`/readyz` 查 DB + ffmpeg/ffprobe + 允许根 + 磁盘余量，**工具缺失时如实报 not ready**。
### 2.3 已验证到什么程度（都是本机 2026-09-20 真跑）

测试环境：临时 data 目录 `/tmp/zv-e2e/data*`、空闲端口 `17766`/`17767`；
**媒体素材是 `scripts/gen-fixtures.sh` 生成到 `/tmp/zv-e2e/media` 的 3 个真视频**；
没有碰 `/Volumes/ZPMirror/video`、本机 7766 实例、MariaDB、面板。

| 验证项 | 结论 |
|---|---|
| `gofmt -l .` / `go vet ./...` | 无输出 / exit 0 |
| `go test ./... -count=1` | **7 个包 ok，0 失败**（共 138 个测试函数） |
| `./scripts/gen-fixtures.sh /tmp/zv-e2e/media` | 3/3 成功：h264+aac 5s、h264 无音轨 4s、**hevc+aac 4s**（本机有 libx265） |
| `./dist/zizvideo --version` | `zizvideo 0.1.0-mvp` |
| `./dist/zizvideo --help` | 用法打 **stderr**，exit **0**（未知 flag 则 exit 2） |
| 起服务 `--config /tmp/zv-e2e/config.json` | `17766` 在听，`/healthz`→`ok` 200 |
| `GET /readyz` | `{"status":"ready"}` 200；`ffmpeg`/`ffprobe` **9.0.1**；`videotoolbox_h264=true` |
| `GET /api/v1/setup/status` → `POST /api/v1/setup` | `needs_setup:true` → 201，下发 `zv_session`(HttpOnly)+`zv_csrf` |
| `POST /api/v1/libraries` | 201，`root_path=/tmp/zv-e2e/media` |
| **CSRF 真拦**：`POST .../scan` 不带 `X-CSRF-Token` | **403**；带上 → 202 |
| **扫描 3 个真视频**：`POST .../scan {"kind":"full"}` | `status=success, total=3, scanned=3, failed=0`；3 条 codec/duration 与 ffprobe 一致 |
| `GET /api/v1/media/{id}/cover` | 200 `image/jpeg` 11522B，`file` 判为 `JPEG ... 640x360`；3 个 `.jpg` 落在 `<data>/covers/` |
| **`curl -r 0-9 .../stream`** | **`HTTP/1.1 206 Partial Content`**，`Content-Range: bytes 0-9/123799`，`Content-Length: 10`；字节 `0000 0020 6674 7970 6973`（`....ftypis`） |
| 越界/多段 Range | `bytes=99999999-` → **416**；`bytes=0-9,20-29` → 回退 **200** 全量 123799 字节 |
| 进度 / 收藏 / 点赞 | `PATCH .../me/progress/{id}` 后回读 `position_ms=2500`；favorites 与 reactions 各 200 |
| `GET /api/v1/feed/next?limit=2` | 200，`next_cursor` + `has_more` 正确；h264 判 `direct:true` |
| 匿名 `GET /api/v1/media` | **401** |
| **Playwright 真播**：挂真 `<video src=stream_url>` | `state=playing, readyState=4, duration=5, 640x360, currentTime=0.76`（**真在播**） |
| `node tools/check-js-syntax.mjs zizvideo/internal/web/assets` | `✓ JS 语法 OK（7 个文件，acorn 校验）` |
| `node tools/check-js-undeclared.mjs zizvideo/internal/web/assets` | `✓ 未声明赋值检查通过（7 个文件）` |
| `roots list/add/remove` | 各一行 JSON；`add` 存软链真实路径；重复/不存在如实拒绝；库在用 → `{"ok":false,...,"in_use":["e2e"]}` 且 **exit 0** |

收尾：两个自起实例都收到 `SIGTERM` 并打印 `已停止` 后退出；`17766`/`17767` 已释放；
本机 7766 上的既有实例（PID 26101）**全程未动**。
### 2.4 未验证 / 未做

| 项 | 状态 |
|---|---|
| fnOS / Docker **构建** | **从未跑过**。`deploy/fnos/*` 只是草图 |
| fnOS 上运行 | **从未跑过** |
| macOS plist 加载 | `deploy/macos/cn.zizvideo.serve.plist` **从未安装、从未 launchctl load** |
| MariaDB 下写路径 | **与 zizvideo 无关**：本仓库无 MySQL/MariaDB 驱动（`go.mod` 只有纯 Go SQLite），没有可跑的写路径 |
| 前端自动存进度 | 浏览器里只验证了「视频在播」+ API 的 PATCH 回读；**「播完自动上报」的整链未在浏览器里走完** |
| 多用户 / 权限交叉 | 只建了 1 个 admin，未验证 user 角色下的界面与越权 |
| 大目录 / 大文件 | 只测了 123799 字节的素材；未测 10GB+ 文件的拖动与内存 |
| 升级 / 迁移 | 只有 `0001_init.sql`，**没有第二次迁移**，没验证过真实升级 |
| Docker 里的版本号 | `ARG VERSION=dev` ⇒ **镜像版本号不会自动对齐** `0.1.0-mvp` |
| 转码 / HLS、上传 | **明确不做**（见 §8.3） |

---

## 3. 架构与目录

单二进制 + 内嵌原生 ESM（**无构建步骤**）+ SQLite(WAL) + 外部 `ffmpeg`/`ffprobe`
+ 内存任务中心（前端轮询）+ 路径白名单。约 9350 行 Go。

```
zizvideo/
  cmd/server/        main.go(CLI/启动/优雅退出) + roots.go(roots 子命令) + main_test.go
  internal/{config,api,auth,domain,ffmpeg,fsbrowse,media,storage,task,web}/  见下表
  migrations/        embed 进来的 .sql（当前只有 0001_init.sql）
  deploy/fnos/       Dockerfile + docker-compose.yml（**草图**）
  deploy/macos/      LaunchAgent plist（**草图**）
  scripts/           gen-fixtures.sh
```

| 包 | 一句话职责 |
|---|---|
| `config` | 读 JSON + 环境变量覆盖 + 校验；`Roots` 是允许根的**运行时唯一快照** |
| `api` / `web` | `/api/v1` 全部 handler、统一信封、会话/CSRF/日志中间件；路由表 + 内嵌前端资源 |
| `auth` / `domain` | 口令哈希、会话签发/校验、CSRF 派生、登录限速；实体 + 错误码（HTTP 状态由 `domain.Error.Status` 决定） |
| `media` / `task` | 路径白名单校验 + 扫描器（遍历/增量/重试/两阶段删除）；扫描任务生命周期与并发去重 |
| `storage` | SQLite(WAL) 连接、迁移、所有 SQL 仓储 |
| `ffmpeg` / `fsbrowse` | 外部工具调用与能力探测（**参数列表直传 exec，不经 shell**）；后台目录浏览器，**只回目录名** |

数据流：`HTTP → middleware → handler → storage/config`；扫描走 `task → media.Scanner → ffmpeg → storage`。

---

## 4. 对外契约（最重要）
### 4.1 配置文件 schema

真源：`internal/config/config.go` 的 `Config`（第 15–35 行）+ `Default()`（第 39–61 行）。
**19 个键，全部可选**；解析用 `DisallowUnknownFields`（第 79 行），**多写一个未知键直接启动失败**。
**所有键都在启动时读一次**：除 `media_allow_roots`（写文件后立即换快照）外，改任何键都要**重启**，
没有 `SIGHUP`、没有热更新接口。

| 键 | 类型 | 默认值 | 语义（校验） | 生效 |
|---|---|---|---|---|
| `listen` | string | `127.0.0.1:7766` | HTTP 监听地址（不能为空） | 重启 |
| `data_dir` | string | `$HOME/Library/Application Support/zizvideo` | 数据目录（绝对路径） | 重启 |
| `database_path` | string | `<data_dir>/zizvideo.db` | SQLite 文件（绝对路径） | 重启 |
| `media_allow_roots` | string[] | `[$HOME/Movies, $HOME/Downloads]` | 媒体根白名单（非空，逐项绝对） | **立即** |
| `media_extensions` | string[] | `mp4,mov,m4v,mkv,webm,ts,mts,avi,flv,hevc` | 扩展名白名单（非空；忽略大小写与点） | 重启 |
| `scan_workers` | int | `4` | 扫描并发（1–16） | 重启 |
| `scan_delete_threshold_ratio` | float | `0.10` | 两阶段删除的比例阈值 | 重启 |
| `scan_delete_threshold_count` | int | `50` | 两阶段删除的绝对条数阈值 | 重启 |
| `scan_retry_backoff_ms` | int[] | `[1000,4000,16000]` | 探测重试间隔；尝试次数 = `len+1` | 重启 |
| `probe_timeout_seconds` | int | `30` | ffprobe/ffmpeg 单次超时（>0） | 重启 |
| `cover_jpeg_quality` | int | `3` | ffmpeg `-q:v`（2–31） | 重启 |
| `ffmpeg_bin` | string | `ffmpeg` | 外部工具路径；`/readyz` 复核 | 重启 |
| `ffprobe_bin` | string | `ffprobe` | 同上 | 重启 |
| `log_level` | string | `info` | `debug/info/warn/error` 四选一 | 重启 |
| `trusted_proxies` | string[] | `[]` | 可信反代 CIDR；决定是否信 `X-Forwarded-For` | 重启 |
| `session_ttl_hours` | int | `336` | 会话有效期 | 重启 |
| `lockout_threshold` | int | `5` | 连续失败几次锁号 | 重启 |
| `lockout_window_minutes` | int | `15` | 锁定/退避窗口 | 重启 |
| `secure_cookie` | bool | `false` | 会话 cookie 强制 `Secure` | 重启 |

校验（`Validate()`，第 160–195 行）不通过**直接拒绝启动**。
#### 4.1.1 环境变量覆盖

`applyEnv()`（第 102–126 行）**在 JSON 之后执行**，所以 **`ZV_*` 一旦设置就赢过文件**（单项覆盖）。

| 变量 | 覆盖 | 解析 |
|---|---|---|
| `ZV_LISTEN` / `ZV_DATA_DIR` / `ZV_DATABASE_PATH` | 同名键 | 非空才生效 |
| `ZV_FFMPEG_BIN` / `ZV_FFPROBE_BIN` / `ZV_LOG_LEVEL` | 同名键 | 非空才生效 |
| `ZV_SCAN_WORKERS` / `ZV_PROBE_TIMEOUT_SECONDS` / `ZV_LOCKOUT_THRESHOLD` / `ZV_SESSION_TTL_HOURS` | 同名键 | `Atoi`，**失败静默忽略** |
| `ZV_SECURE_COOKIE` | `secure_cookie` | `1`/`true`（忽略大小写）=true，其余=false |
| `ZV_MEDIA_ALLOW_ROOTS` / `ZV_MEDIA_EXTENSIONS` / `ZV_TRUSTED_PROXIES` | 同名键 | **逗号分隔**（不是 JSON 数组），去空白丢空项 |
| `ZV_CONFIG` | —（非配置键） | 没有 `--config` 时的配置路径 |
| `ZV_AUTH_SECRET` | —（非配置键） | 直接当 CSRF 的 HMAC 密钥；设了就不读 `<data_dir>/secret.key` |

**没有环境变量覆盖的键**：`scan_delete_threshold_ratio`、`scan_delete_threshold_count`、
`scan_retry_backoff_ms`、`cover_jpeg_quality`、`lockout_window_minutes`。

优先级：`--config` > `ZV_CONFIG` > 无配置文件（全默认值）。配置**可以不存在**：`Load("")` 不报错，
直接走默认值 —— 但默认 `data_dir` 在 macOS 家目录下，**Linux 上必须显式覆盖**。
#### 4.1.2 与 `media_allow_roots` 有关的三个坑

1. `ZV_MEDIA_ALLOW_ROOTS` 设置后，界面与 CLI 的增删都**明确拒绝写入**
   （`MEDIA_ROOT_ENV_OVERRIDE` 409），不静默写一个不生效的文件。
2. 没有配置文件时（`Roots.fpath == ""`）界面加根会拒（`MEDIA_CONFIG_UNAVAILABLE` 409）。
3. 写配置只替换 `media_allow_roots` 一个键，**其它字段的拼写与顺序原样保留**（`roots.go:196`）；
   临时文件 + `rename` 原子替换、权限 `0600`、写完**回读比对**，不一致就报"未生效"。
#### 4.1.3 `config_version` 现状与建议

**当前没有版本字段。** 因为未知键会被 `DisallowUnknownFields` 拒绝，**不能**先往文件里塞
`config_version` —— 老代码会直接拒绝启动。建议接手后**第一刀**就加：

- `config_version`（int，**缺省视为 `1`**），并备好按版本升级的读法；
- 读到**更高**版本时**拒绝启动**并明说「配置由更新版本写的，请升级程序」；
- 同时加能力端点（见 §4.5），让外部一个 GET 就知道支持哪些键。
### 4.2 CLI 面

真源：`cmd/server/main.go` + `cmd/server/roots.go`。

```
zizvideo [--config <file>] [--version]
zizvideo roots list|add <绝对路径>|remove <绝对路径> [--config <file>]
```

| 入口 | 参数 | 行为 |
|---|---|---|
| （无） | `--config <file>` | 起 HTTP 服务；`--config` 缺省时看 `ZV_CONFIG`，都空则用全默认值 |
| `--version` | — | 打印 `zizvideo 0.1.0-mvp`，exit 0 |
| `--help` / `-h` | — | 用法打 **stderr**，exit **0**（不是 2） |
| 未知 flag | — | 打用法到 stderr，**exit 2** |
| `roots list` | `--config`/`ZV_CONFIG` **必需** | 回 `{"action":"list","ok":true,"roots":[...],"config_path":"..."}` |
| `roots add <绝对路径>` | 同上 | 校验目录存在/可读/是目录 → 存 `EvalSymlinks` **真实路径**；已在白名单内或已被覆盖 → 报错 exit 1 |
| `roots remove <绝对路径>` | 同上 | 被媒体库使用时**不删**，回 `{"ok":false,"in_use":[库名...]}` 但 **exit 0** |

- `--config` 在 `roots` 子命令里**可出现在 verb 前后**（`stripFlag`，`roots.go:115`）；主命令的 verb 探测是 `indexVerb`（`main.go:154`）。每条输出**一行 JSON**。
- **主命令没有** `--data-dir`、`--listen`、`--port` 之类的 flag；改这些只能走配置或环境变量。
### 4.3 HTTP API

真源：`internal/web/router.go`（路由表）+ `internal/api/middleware.go`（鉴权）。
统一响应信封 `data`/`meta`/`error` 三键；错误另有 `error.code`/`error.message`。

**鉴权三态**：`公开` = 不登录；`登录` = 需 `zv_session`；`admin` = 还要 `role=admin`。
**CSRF**：所有 `POST/PUT/PATCH/DELETE` 都要 `X-CSRF-Token` 头（值 = `zv_csrf` cookie），否则 403。
`RequireAuth`/`RequireAdmin` 每次请求**回查 DB**，所以禁用/改角色立即生效。

| 方法 | 路径 | 鉴权 / CSRF | 职责 |
|---|---|---|---|
| GET | `/healthz` | 公开 | 进程存活，纯文本 `ok` |
| GET | `/readyz` | 公开 | DB + ffmpeg/ffprobe + 允许根 + 磁盘余量；不满足回 **503 not_ready** |
| GET | `/api/v1/setup/status` | 公开 | `{"needs_setup":bool}`（身份端点） |
| POST | `/api/v1/setup` | 公开* | 创建第一个 admin；已有用户时 409 |
| POST | `/api/v1/auth/login` | 公开 | 登录，下发会话 + CSRF cookie |
| POST | `/api/v1/auth/logout` | 登录 + CSRF | 吊销当前会话 |
| GET | `/api/v1/auth/me` | 登录 | 当前用户（前端启动探针） |
| GET | `/api/v1/users` | admin | 用户列表 |
| POST | `/api/v1/users` | admin + CSRF | 建号 |
| PATCH | `/api/v1/users/{id}` | admin + CSRF | 改角色/状态/昵称/改密 |
| GET | `/api/v1/libraries` | admin | 媒体库列表 |
| POST | `/api/v1/libraries` | admin + CSRF | 建库（全路径校验） |
| GET | `/api/v1/libraries/{id}` | admin | 单库 + 计数 + 最近扫描 |
| PATCH | `/api/v1/libraries/{id}` | admin + CSRF | 改库（重校验新根路径） |
| DELETE | `/api/v1/libraries/{id}` | admin + CSRF | 软删库及其媒体行 |
| POST | `/api/v1/libraries/{id}/scan` | admin + CSRF | 起扫描，**202 + task_id**；同库已有扫描则 409 |
| GET | `/api/v1/scan-tasks/{id}` | admin | 扫描进度（前端轮询这个） |
| GET | `/api/v1/media` | 登录 | 分页列表；`page/per_page/sort/order/library_id/q/status` |
| GET | `/api/v1/media/{id}` | 登录 | 单条；**path 只对 admin 返回** |
| GET | `/api/v1/media/{id}/stream` | 登录 | Range 流；校验白名单后才 `open` |
| GET | `/api/v1/media/{id}/cover` | 登录 | JPEG 封面；没有则回 SVG 占位（200） |
| GET | `/api/v1/feed/next` | 登录 | 游标 feed；`library_id/last_id/limit(1–50)` |
| PATCH | `/api/v1/me/progress/{mediaId}` | 登录 + CSRF | 存播放位置 |
| GET | `/api/v1/me/progress` | 登录 | 最近进度列表 |
| POST | `/api/v1/me/favorites/{mediaId}` | 登录 + CSRF | 加收藏 |
| DELETE | `/api/v1/me/favorites/{mediaId}` | 登录 + CSRF | 取消收藏 |
| POST / PATCH | `/api/v1/media/{id}/reactions` | 登录 + CSRF | 点赞/点踩（共用一个 handler） |
| DELETE | `/api/v1/media/{id}/reactions` | 登录 + CSRF | 取消表态 |
| GET | `/api/v1/admin/system/info` | admin | 版本/Go 版本/uptime/工具能力/计数/schema 版本 |
| GET | `/api/v1/admin/audit` | admin | 写操作审计（`limit` 1–500） |
| GET | `/api/v1/media/roots` | admin | 允许根 + 真实状态 + `env_override` 标记 |
| POST | `/api/v1/media/roots` | admin + CSRF | 加允许根（原子写回 + 回读） |
| DELETE | `/api/v1/media/roots?path=` | admin + CSRF | 删允许根；被库占用则 409 |
| GET | `/api/v1/fs/browse?path=` | admin | 目录选择器，**只回子目录名** |
| 任意 | `/api/*`（未匹配） | 公开 | JSON 404，**不回 SPA** |
| 任意 | `/` 及其它 | 公开 | 内嵌前端静态资源（`/` 与 `*.html` 不缓存） |

`*` setup 无鉴权但**只在零用户时可成功**。**没有 `/api/version`**：版本从 `--version` 或
`admin/system/info` 拿（需 admin）。没有 `/metrics`、没有 `/socket`、没有 SSE 端点。
### 4.4 路径 / 端口 / 标识

| 项 | 值 |
|---|---|
| 默认监听 | `127.0.0.1:7766`（只绑回环） |
| 默认数据目录 | macOS：`$HOME/Library/Application Support/zizvideo`；**Linux 上必须显式指定** |
| DB 文件名 | `zizvideo.db`（WAL，伴生 `-wal`/`-shm`） |
| 会话密钥 | `<data_dir>/secret.key`（0600，32 字节随机） |
| 封面目录 | `<data_dir>/covers/<media_id>.jpg` |
| 日志 | **只打 stdout**，JSON 结构；无文件日志、无 syslog。plist 草图里把 stdout/stderr 落到 `~/Library/Logs/zizvideo.{out,err}.log` |
| Unix socket | **没有**（纯 HTTP） |
| launchd label | `cn.zizvideo.serve`（用户级 LaunchAgent，**草图未加载**） |
| compose 服务名 / 镜像 | `zizvideo` / `zizvideo:0.1.0-mvp` |
| 环境变量前缀 | `ZV_` |
| HTTP 超时 | ReadHeader/Read 15s、**Write 0（不限，给流用）**、Idle 120s |
| Cookie 名 | `zv_session`(HttpOnly) / `zv_csrf`(前端可读)，均 `SameSite=Strict` |
| ID 前缀 | `usr_` `lib_` `med_` `scn_` `ses_` |
### 4.5 与面板的接口 = **只有配置文件**

这是本次交接最关键的一条：

- 面板**只写一个键**：`media_allow_roots`（JSON 字符串数组），
  或用 `ZV_MEDIA_ALLOW_ROOTS` 环境变量。
- **没有代码依赖**：`zizvideo/go.mod` 是独立 module（`github.com/zizdog/zizvideo`），
  `grep zizpanel zizvideo` **无命中**。
- **没有托管**：面板不启动/不监视 zizvideo 进程，没有 plist 代管，没有健康轮询。
- **没有内嵌**：zizvideo 不进面板二进制，面板也不进 zizvideo。
- 分工：**面板写配置，zizvideo 自己读**。谁都不需要额外权限（见面板仓库的
  `docs/应用权限与文件访问.md` §6「简单场景别上重锤」）。

**契约变更时的握手建议**（给接手方）：

1. **加版本字段**：配置里加 `config_version`，保留「缺省 = 1」的兼容读法（见 §4.1.3）。
2. **加能力端点**：加 `GET /api/v1/capabilities`（可公开，只回版本 + 支持的配置键与环境变量），让面板**不写死版本号**。
3. **失败时如实报**：面板写配置前先 GET 一次能力端点；端点数不出来/版本对不上，**明说「适配器与 zizvideo 版本不匹配」**，不要静默写一个可能被 `DisallowUnknownFields` 拒绝的键 —— 那会让服务下次启动直接失败。
4. **保持向后兼容**：老键名不删；要改名就双读一段时间。

---

## 5. 开发与运行

```bash
export GOPROXY=https://goproxy.cn,direct   # 本机 proxy.golang.org 不可达
make build        # CGO_ENABLED=0 go build -trimpath -ldflags ... -o dist/zizvideo ./cmd/server
make test         # go test ./...      make fmt  # gofmt -l -w .
make fixtures     # ./scripts/gen-fixtures.sh（默认写 fixtures/）
```

真正的门禁（与面板 `make check` 里跑的一致）：

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
node tools/check-js-syntax.mjs    zizvideo/internal/web/assets   # 面板仓库里的工具
node tools/check-js-undeclared.mjs zizvideo/internal/web/assets
```

前端**没有构建步骤**：`internal/web/assets/**` 原样 `//go:embed` 进二进制，
所以**改完前端必须重新 `go build`**，没有热更新。
### 端到端验收步骤（下面每条都真跑过，输出见 §2.3）

```bash
./scripts/gen-fixtures.sh /tmp/zv-e2e/media           # 3 个真视频
./dist/zizvideo --config /tmp/zv-e2e/config.json      # listen=127.0.0.1:17766
curl -s http://127.0.0.1:17766/healthz                # ok
curl -s http://127.0.0.1:17766/readyz                 # status=ready
curl -s http://127.0.0.1:17766/api/v1/setup/status    # needs_setup=true
curl -s -c c.txt -X POST .../api/v1/setup -d '{"username":"admin","password":"...","display_name":"E2E"}'
curl -s -b c.txt -X POST .../api/v1/libraries -H "X-CSRF-Token: $C" -d '{"name":"e2e","root_path":"/tmp/zv-e2e/media"}'
curl -s -b c.txt -X POST .../api/v1/libraries/$LIB/scan -H "X-CSRF-Token: $C" -d '{"kind":"full"}'
curl -s -b c.txt .../api/v1/scan-tasks/$TASK          # status=success total=3
curl -s -b c.txt .../api/v1/media/$MID/cover -o cover.jpg   # JPEG
curl -s -b c.txt -r 0-9 .../api/v1/media/$MID/stream  # HTTP/1.1 206 Partial Content
curl -s -b c.txt -X PATCH .../api/v1/me/progress/$MID -H "X-CSRF-Token: $C" -d '{"position_ms":2500,...}'
curl -s -b c.txt '.../api/v1/feed/next?limit=2'       # list + next_cursor
```

CSRF 值取 `zv_csrf` cookie（`awk '$6=="zv_csrf"{print $7}' c.txt`）。
**验收用的 data 目录和端口必须临时化**，别用 7766。

---

## 6. fnOS / Docker 适配指南

现有草图：`deploy/fnos/Dockerfile`、`deploy/fnos/docker-compose.yml`。
**两者都从未构建、从未在飞牛上跑过**，注释里也这么写了。以下是草图里**已知的问题与要点**。

| 项 | 现状 / 要点 |
|---|---|
| 基础镜像 | builder `golang:1.25-bookworm`，运行 `debian:bookworm-slim` + `apt install ffmpeg`。**镜像里的 ffmpeg 版本从未验证** |
| 非 root | 草图 `useradd -u 1000` 后 `USER 1000:1000`。**UID/GID 必须与媒体目录属主一致**，现场确认 |
| 卷 | 数据卷 `/var/lib/zizvideo`（可写：DB + 封面 + secret.key）；媒体卷 `/media:ro`（**只读**，zizvideo 从不写媒体目录） |
| 端口 | Dockerfile `EXPOSE 7766` + `ZV_LISTEN=0.0.0.0:7766`；compose 绑 `127.0.0.1:7766:7766`。**这俩不一致**：绑回环则容器外（局域网）访问不到 |
| healthcheck | 草图用 `wget -qO- .../healthz`，但 **debian:bookworm-slim 默认没有 wget**，要装 `curl`/`wget` 或改自检。未验证 |
| 健康判据 | `/healthz`（存活）还是 `/readyz`（真就绪）要想清楚：`/readyz` 会因媒体根离线/磁盘不足回 503，适合做「能不能服务」的门槛 |
| `TZ` | compose 传 `TZ=Asia/Shanghai`；时间戳按 RFC3339 **UTC** 入库，显示时才本地化 |
### Linux 上没有 TCC（权限退化成 uid/组 + ACL）

macOS 上「面板能读的可移除宗卷，子进程也能读」靠 TCC 继承。**Linux 没有 TCC**，退化成：
compose 用**同一个服务用户**启动（`user:` / `group_add`），或用**文件 ACL**（`setfacl`）授权。
broker/fd 透传（`SCM_RIGHTS`）在 Linux 上语义相同、**协议层不用分平台**，但那是「必须越权访问时」
才做的形态 C，**zizvideo 当前不需要** —— 详见**面板仓库**的
`docs/应用权限与文件访问.md`（§4 broker 设计要点、§5 Linux/fnOS 对应）。
### 媒体目录挂载与 UID/GID 选择

1. 先确认飞牛上媒体目录属主：`ls -n /vol1/media`。
2. 容器 `user:` 用同一个 uid（或该 uid 进 `group_add`），否则扫描全部失败、
   `/readyz` 的 `media_roots` 报 `不存在`。
3. **媒体卷一律 `:ro`**：写路径只有数据卷，容器被攻破也改不了媒体库。
4. SQLite **不要放网络盘**（单写者，NFS/SMB 上 WAL 会出问题）。
### 硬件加速是增强不是前提

- `/dev/dri` 只影响**转码**；Phase1 **不转码**，所以**当前完全不需要**。
- 抽封面走 ffmpeg 解码，CPU 足够；`/dev/dri` 传了只是锦上添花。
- `/readyz` 的 `videotoolbox_h264` 是 **macOS 专属**探针，Linux 上恒为 `false` —— **正常，不是故障**。
### 多架构 manifest 要点

- `CGO_ENABLED=0` + 纯 Go SQLite ⇒ **交叉编译没障碍**；`GOARCH=amd64`/`arm64` 各出一份或走 buildx。
- Dockerfile 用 `--platform=$BUILDPLATFORM` + `ARG TARGETARCH`，避免 QEMU 模拟编译。
- 目标：`docker buildx build --platform linux/amd64,linux/arm64` 出**一个多架构 manifest**；
  **不要**只发 amd64 让飞牛（常见 arm64）去转译。

---

## 7. 独立出去的步骤

`zizvideo/` 是**自包含的真 git 目录**（66 个文件已被面板仓库跟踪，无面板依赖），可以直接拆。
### 7.1 拆成独立仓库

**方案 A：`git subtree split`（不改写面板历史，首选）**

```bash
cd /path/to/zizpanel
git subtree split -P zizvideo -b zizvideo-standalone   # 一条只含 zizvideo/ 的历史
mkdir -p /path/to/standalone && cd /path/to/standalone
git init && git pull /path/to/zizpanel zizvideo-standalone
# 校验：内容与面板里的 zizvideo/ 完全一致，且没有 zizpanel 的提交
```

split 出来的历史里**路径前缀会被剥掉**（独立仓库根 = 原来的 `zizvideo/`）；
子目录之外的东西不会跟过去 —— 面板的 `README.md`/`Makefile` 不会。

**方案 B：`git filter-repo`（历史更干净，会改写提交 id）**

```bash
git filter-repo --path zizvideo/ --path-rename zizvideo/:  # 只在面板仓库的**克隆**上跑
```

filter-repo 是破坏性改写，**永远不要在面板主仓库原地跑**。
### 7.2 拆出后必须同步改掉面板里的引用

| 位置 | 现状 | 拆出后要做 |
|---|---|---|
| `Makefile:138` / `:144` | acorn 语法与未声明赋值门禁都扫 `zizvideo/internal/web/assets` | **删掉这个路径参数（两处）** |
| `Makefile:152-158` | 「独立软件模块自测」循环 `for m in macsaber zizvideo` | **把 `zizvideo` 从循环里去掉** |
| `Makefile:136` | 注释里提到 zizvideo | 一并改注释 |
| `docs/应用权限与文件访问.md:65` | 拿 zizvideo 的 `media_allow_roots` 当例子 | 改成「已独立出去的项目」或换例子 |
| `ZizPanel-当前状态.md:24-25` | 记录 zizvideo 进程/端口 | 删除（该文件 gitignored） |
| `README.md` | **无 zizvideo 引用** | 不用动 |

拆出后面板的 `gofmt -l .` 也不再覆盖 zizvideo（`./...` 本来就扫不到独立 module）。
### 7.3 独立仓库自己的 CI / 门禁建议

```bash
gofmt -l .                                # 必须无输出
go vet ./...
go test ./... -count=1                    # 当前 7 包全绿
node tools/check-js-syntax.mjs    internal/web/assets   # 真 ES 解析器，不用 node --check
node tools/check-js-undeclared.mjs internal/web/assets  # 未声明赋值（防 "Can't find variable" 白屏）
```

前端两道要把面板的 `tools/check-js-*.mjs` **复制**过去（只依赖 `acorn`）；
`check-js-undeclared.mjs` 的等价物**必须一并拆走**，否则前端退化到只有语法检查。
另外建议加：`go build ./...`（防 `-trimpath` 下 `-X` 静默失效）、未来日期门禁
（`tools/check-future-dates.sh`）、以及 Docker 段
`docker buildx build --platform linux/amd64,linux/arm64`（**先跑通再加进必过门禁**）。

---

## 8. 已知问题 / TODO
### 8.1 「未验证」清单

**见 §2.4 的完整表** —— fnOS/Docker 从未构建或运行、macOS plist 从未 `launchctl load`、
前端「播完自动上报进度」整链未走完、多用户越权未验、大目录/大文件未验、真实升级未验。
### 8.2 「已知限制」清单

1. **SQLite 单写者**：不要把 DB 放网络盘；多实例会互相干扰。
2. **HEVC 直出靠 UA 粗判**：无解码能力的 Chrome「只出声不出画」且不触发 `error`，
   前端靠 `videoWidth===0` 兜底提示。真正的兼容版本要等转码。**无日志轮转**（只打 stdout，交给外部管）。
3. **`X-Forwarded-Proto` 不受 `trusted_proxies` 约束**：`forwardedHTTPS()`（`middleware.go:44`）
   只看头、不看对端是否可信 —— 任意客户端都能让 cookie 带上 `Secure`（HTTPS 反代想要的效果，
   直连 HTTP 最多导致 cookie 不下发）。**不是安全洞，但是个不一致**。
4. **`ZV_DATA_DIR` 与 `database_path` 的交互反直觉**：只有 `database_path == ""`
   或（`ZV_DATA_DIR` 非空且 `ZV_DATABASE_PATH` 为空）时 DB 才跟着 `data_dir` 走；
   **配置文件里显式写了 `database_path` 时，只设 `ZV_DATA_DIR` 不会搬 DB。**
5. **硬编码 macOS 路径**：目录选择器起始点写死 `/Volumes`
   （`handlers_roots.go:228`、`assets/js/roots.js:169`、`assets/js/admin.js:82`），Linux 上是死链。
6. **`roots` 子命令忽略环境变量覆盖**：它直接读写 JSON 文件，**不套 `ZV_*`**，
   所以设了 `ZV_MEDIA_ALLOW_ROOTS` 时 CLI 的 `list` 与运行时看到的可能不同。
7. **`setInt` 解析失败静默忽略**：`ZV_SCAN_WORKERS=abc` 不报错，悄悄用默认值。
### 8.3 「故意不做」清单（不要当 TODO 接）

- **上传**：服务器不接收任何用户上传，前端无入口。
- **转码 / HLS / 实时转码**：`direct:false` 只如实提示。
- **社交**（评论/关注/分享）、**推荐算法**、**分类标签**。
- **对象存储**、**多实例**、**Prometheus 指标**。
- **broker / fd 透传**：当前不需要（配置就够），别提前上重锤。

---

## 9. 安全基线

| 项 | 现状 |
|---|---|
| 监听 | 默认只绑 `127.0.0.1`；要暴露必须显式改 `listen` 或 `ZV_LISTEN` |
| 首启 | 零用户时走 `/api/v1/setup` 设第一个 admin；建过就不能再建 |
| 口令 | PBKDF2-SHA256，210000 次迭代，16 字节随机盐；**从不存明文** |
| 会话 + CSRF | 32 字节随机 token，DB 里只存 **SHA-256 哈希**；`HttpOnly` + `SameSite=Strict` + 可选 `Secure`；CSRF 双提交 HMAC 派生，**只认 `X-CSRF-Token` 头**，绝不认 cookie |
| 登录防护 + 越权 | IP+用户名指数退避（封顶 2s）+ 阈值锁定，账号不存在时也烧等量 PBKDF2；每次请求回查 DB 的角色/状态 ⇒ 禁用与降权**立即生效** |
| 路径白名单 | 媒体库路径必须绝对、`filepath.Clean` 稳定、存在、是目录、可读，且 **`EvalSymlinks` 前后都落在允许根内**；软链指向白名单外一律拒绝 |
| 敏感目录 + 流前校验 | 扫描**默认拒绝软链**，`.DS_Store`/`@eaDir`/`#recycle`/`lost+found`/`*.part` 默认忽略；`/stream` 在 `os.Open` **之前**重跑白名单校验 |
| 不拼 shell | 所有外部命令走 `exec.CommandContext` 参数列表，**文件名里的 shell 元字符只是普通参数** |
| 审计脱敏 | 审计与日志**只写 `media_id`，不写媒体文件路径**；`X-Request-Id` 关联 |
| 目录浏览器 | 只回**子目录名**，不回文件名/大小/内容 ⇒ 偷到 admin 会话也读不到文件 |
| 响应头 | `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: same-origin` |
| 危险操作 + 真实性 | 覆盖前白名单校验 + 回读，拒绝写「半截/坏配置」，被库占用的允许根删不掉；工具缺失/路径离线/磁盘不足**一律如实报 not ready**，绝不返回假的 200 |
