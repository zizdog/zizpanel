# Zizvideo

> 📄 接手/二次开发请先读 **[HANDOVER.md](HANDOVER.md)**（配置 schema、API 契约、未验证项、fnOS/Docker 适配）。

本地短视频服务：把自己的视频目录扫进 SQLite，用浏览器上下滑着看。
Go 单二进制 + 内嵌原生 ESM 前端（无构建步骤）+ SQLite（纯 Go 驱动，无 CGO）。
从 govideo 改名而来；旧数据目录 `~/Library/Application Support/govideo` 需手工迁移（改名不会自动搬）。

## 快速开始

```bash
export GOPROXY=https://goproxy.cn,direct
make build                       # 产出 dist/zizvideo
cp config.example.json config.json   # 按需改 media_allow_roots / 端口
./dist/zizvideo --config config.json
```

打开 <http://127.0.0.1:7766>。首次访问进入初始化页，创建管理员后即可登录。
默认只监听 127.0.0.1、以当前登录用户身份运行，不需要 root。

媒体目录不用手改 JSON：后台「媒体允许根」页签点「浏览目录…」选好目录，
不在允许根内时点「加为允许根」，再回「媒体库」新建即可。

`make fixtures` 会用 ffmpeg 生成几个几秒的测试视频，默认写进 `fixtures/`；
也可以 `./scripts/gen-fixtures.sh /某个目录`。

## 配置

JSON 文件（`--config` 或 `ZV_CONFIG`）+ `ZV_` 环境变量覆盖，两者都走同一份校验。
完整字段见 `config.example.json`。常用变量：

| 变量 | 说明 |
|---|---|
| `ZV_LISTEN` | 监听地址，默认 `127.0.0.1:7766` |
| `ZV_DATA_DIR` | 数据目录（DB、封面、会话密钥） |
| `ZV_DATABASE_PATH` | SQLite 路径，默认 `<data_dir>/zizvideo.db` |
| `ZV_MEDIA_ALLOW_ROOTS` | 媒体根白名单，逗号分隔；设置后覆盖配置文件 |
| `ZV_MEDIA_EXTENSIONS` | 扩展名白名单，逗号分隔 |
| `ZV_SCAN_WORKERS` | 扫描并发，1–16，默认 4 |
| `ZV_FFMPEG_BIN` / `ZV_FFPROBE_BIN` | 外部工具路径 |
| `ZV_LOG_LEVEL` | debug / info / warn / error |
| `ZV_TRUSTED_PROXIES` | 可信反代 CIDR，逗号分隔；为空则不信任任何 `X-Forwarded-*` |

### 面板/脚本可以安全改的字段

配置文件是唯一真源。下面这些字段可被面板或脚本直接改写（改完的生效方式见最后一列）：

| 键 | 含义 | 示例 | 生效 |
|---|---|---|---|
| `media_allow_roots` | 媒体根白名单（JSON 字符串数组） | `["/Volumes/ZPMirror/video"]` | 立即（服务每次读快照）；也可用界面/CLI 改 |
| `listen` | 监听地址 | `"127.0.0.1:7766"` | 重启后 |
| `scan_workers` | 扫描并发 1–16 | `4` | 重启后 |
| `media_extensions` | 扩展名白名单 | `["mp4","mkv"]` | 重启后 |
| `scan_delete_threshold_ratio` / `_count` | 两阶段删除阈值 | `0.1` / `50` | 重启后 |
| `log_level` | 日志级别 | `"info"` | 重启后 |
| `ffmpeg_bin` / `ffprobe_bin` | 外部工具路径 | `"/opt/homebrew/bin/ffmpeg"` | 重启后（`/readyz` 会复核） |
| `trusted_proxies` | 可信反代 CIDR | `["127.0.0.1/32"]` | 重启后 |

改 `media_allow_roots` 的三种等价途径：后台「媒体允许根」界面、
`zizvideo roots add|remove`、直接编辑 JSON（服务读快照，`GET /api/v1/media/roots` 可回读）。
`ZV_MEDIA_ALLOW_ROOTS` 一旦设置就覆盖文件里的值，此时界面与 CLI 会明确拒绝写入。

```bash
zizvideo roots list                       --config config.json
zizvideo roots add    /Volumes/ZPMirror/video --config config.json
zizvideo roots remove /Volumes/ZPMirror/video --config config.json
# 每条输出一行 JSON：{"action":"add","ok":true,"path":"...","roots":[...],"config_path":"..."}
```

媒体库根路径必须：绝对路径、`filepath.Clean` 后与原串完全一致、存在且是目录、可读、
且落在允许根内（`EvalSymlinks` 之后仍在白名单内，`/tmp` → `/private/tmp` 这类别名按真实路径比对）。
新增允许根时按真实路径存（软链接存到它指向的目录）。越界返回 403 并给出真实原因；
被媒体库使用的允许根删不掉（会点名库名）。

## 这一轮能做什么

- 首启初始化（PBKDF2-SHA256 + 随机盐，无明文）、登录/登出、会话 cookie（HttpOnly + SameSite=Strict）、
  写操作 CSRF 双提交（只认 `X-CSRF-Token` 头）、登录失败指数退避 + 账号锁定。
- 用户管理：建号、改角色、禁用、重置口令；禁用与改角色下一次请求即生效（鉴权每次回查数据库）。
- 媒体库增删改查 + 后台异步扫描（默认 4 worker，1 秒内返回 202 + task_id，进度写库、前端轮询）。
- 媒体允许根：`GET|POST|DELETE /api/v1/media/roots`（admin）读/加/删，原子写回 config.json 并回读复核；
  `GET /api/v1/fs/browse?path=`（admin）只回子目录名，供后台目录浏览器用。
- 扫描：忽略规则、扩展名白名单、`size+mtime_ns` 增量跳过、ffprobe 元数据、ffmpeg 抽首帧封面、
  失败分类 + 1s/4s/16s 重试、两阶段删除、根路径消失/设备号变化判 `interrupted` 且不删；
  扫描前再校验一次允许根，越界直接拒绝且不遍历任何目录。
- 播放：`GET /api/v1/media/{id}/stream` 走 `http.ServeContent`，天然支持 `Accept-Ranges` / 206 /
  `Content-Range` / 416；多段 Range 回退 200 全量；文件被删则结束流并释放 fd。
- feed：游标分页、单一播放实例、进度续播、收藏、点赞；后台页有库/媒体/允许根/用户/系统信息。
- JSON 结构日志（不写媒体文件路径，只写 media_id）+ 写操作审计。
- `GET /healthz` 只查进程；`GET /readyz` 查 DB + ffmpeg/ffprobe + 允许根 + 磁盘余量，
  **工具缺失时如实报 not ready，绝不返回 200**。

## 还没做（Phase2/3）

- HLS / 后台转码 / 实时转码：`direct:false` 只如实提示，不生成兼容版本。
- 分类、标签、批量编辑、推荐排序、评论/关注等社交功能。
- **上传**：服务器不接收任何用户上传，前端也没有入口。
- 对象存储、多实例、Prometheus 指标、SBOM/CI、备份子命令。
- fnOS：`deploy/fnos/` 只有草图，**没有在飞牛设备上构建或运行过**。
  `deploy/macos/cn.zizvideo.serve.plist` 只是文件，**本轮没有安装、没有加载验证**。

## 已知限制

- HEVC/AV1 是否直出由服务端按 UA 家族粗判。无 HEVC 解码能力的 Chrome 会「只出声不出画」、
  且不触发 `error`，前端靠 `videoWidth === 0` 兜底提示「这台浏览器解不出 HEVC 画面」。
  真正的兼容版本要等 Phase2 转码。
- SQLite 单写者；不要把数据库放在网络盘上。
- 单实例，多开两个写同一份数据库会互相干扰。

## 测试

```bash
export GOPROXY=https://goproxy.cn,direct
make test        # go test ./...
make fmt         # gofmt
```

测试全部使用临时目录 + 临时数据库，不会读写真实媒体目录。
门禁覆盖：Range 206/416/多段回退、路径穿越与符号链接越界、CSRF、越权与禁用即时生效、
允许根增删的持久化与回读（被拒时配置文件字节不变）、有库在用拒绝删除、
目录浏览器只回目录、扫描前越界拒绝且不遍历、扫描忽略/增量/两阶段删除/interrupted、
ffprobe 失败分类、工具缺失时 `/readyz` 必须 not ready、迁移幂等、
以及「文件名里的 shell 元字符只会被当成一个普通参数」。
