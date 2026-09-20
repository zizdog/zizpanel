# zizvideo 迭代 2 设计（一键识别 / 指定用户可见媒体库 / 新用户默认可看）

> 基线：本仓库 `8efcd8a`（工作树干净）。行号按此版本给出；若有并行编辑者改动，
> **以符号名（函数/路由/表名）为准**，行号只作定位证据。
> 原则沿用 `docs/ITERATION-1.md`：无构建步骤（原生 ESM）、文案 ≤40 字、**不许谎报**、
> 长任务走 202 + `task_id` + 进度。**本文件只做设计：不改代码、不提交、不发版、不碰 mini。**

## 0. 结论先行

| 项 | 结论 | 关键锚点 |
|---|---|---|
| A 一键识别 | **做**。批量接口与现有单剧场 `detect` **共用同一个解析/写入内核**，不写第二份 | `handlers_series.go:393`、`handlers_series.go:417`、`storage/series.go:268` |
| A 后台自动识别 | **做**，时机定为「**扫描任务成功结束后补齐**」+「加入剧场时（已有）」；**不做服务启动全量** | `scanner.go:54`、`handlers_series.go:318` |
| B 库级授权 | **做，且是最高优先级**。fail-closed：普通用户默认 0 个库；管理员全见 | 迁移 `0006`、新增 `authz.go` |
| B 越权返回码 | **一律 404**（与"不存在"不可区分）；列表类**静默过滤**不报错 | `domain.ErrNotFound`（`domain.go:173`） |
| C 库分组 + 按组授权 | **本轮不做**（用户 2026-09-20 决定）；诉求改用下面的"新用户默认可见库"满足 | 见 C |
| 新用户默认可见库（替代分组） | **做**。`media_libraries.default_for_new_users=1` 的集合；空集合 = 未设置（fail-closed）；自助注册时**同事务**写成显式授权（`user_libraries.source='default'`），之后改开关**不追溯** | `0008`、`authz.go`、`handlers_admin.go:84` |
| 迁移默认 | 老库升级后历史普通用户**被挡在门外**（有意），配一键补授兜底 | `0006` + 管理端入口 |
| 任务载体 | 跨库识别任务**新建 `job_tasks`**，不复用 `scan_tasks`（后者 `library_id NOT NULL` + 外键指向单库，装不下跨库任务；现有去重已因此把 `library_id` 塞成 `found[0]`，`handlers_admin.go:273`） | 迁移 `0007` |

---

## A 一键识别 + 后台自动识别

### A.1 现状基线（必须复用，不许另写）

| 能力 | 位置 | 语义 |
|---|---|---|
| 解析器 | `internal/media/episode.go:41` `ParseEpisode`、`:176` `EpisodeLabel`、`:187` `EpisodePointers`、`:209` `SortOrderKeys` | 唯一解析真源 |
| 单剧场识别接口 | 路由 `router.go:74` → `handlers_series.go:393` `HandleDetectSeries` | `confirm=false` 只回变化清单；`true` 才落库 |
| 加入剧场时识别 | `handlers_series.go:264` `HandleAddSeriesMedia` → `:318` `seriesMediaFromMedia` | 加入即写入 |
| 唯一写入口 | `storage/series.go:268` `ApplySeriesEpisodes` | SQL 带 `AND COALESCE(episode_source,'') <> 'manual'`（`:277`）→ **自动识别永不覆盖 manual** |
| 未识别语义 | `series_media.season/episode` 可空（`0004_episode.sql:3-4`），`NULL = 未识别，绝不猜`（`handlers_series.go:419`） | |

**目标**：管理员一次把所有剧场（或选中剧场）重跑识别；同一套解析在扫描结束后也能在后台自动补齐。

### A.2 共同内核（消除"第二份"）

把 `HandleDetectSeries` 里 `409-437` 行那段（遍历成员 → 跳过 manual → `ParseEpisode` → 组装
`changes`/`assigns`）抽成**一个**函数：

```go
// internal/detect/detect.go（新包，只依赖 storage + media，不依赖 api）
func Series(ctx, db, seriesID) (SeriesResult, error)   // 只读，返回 changes/assigns/manualSkipped
func Apply(ctx, db, seriesID, assigns) (int, error)    // 透传 storage.ApplySeriesEpisodes
```

- 单剧场 handler、批量任务、后台补齐**三方都调它**；
- 判据函数 `detect.Series` 必须**只用** `media.ParseEpisode`（`:41`）与 `seriesMediaName`（`handlers_series.go:129`）；
- 门禁：`grep -rn "ParseEpisode" internal/ --include=*.go | grep -v _test.go` 只允许命中
  `media/episode.go`、`media/scanner` 无关处、`detect/`、`handlers_series.go:417`（handler 改为调 `detect` 后此处也应消失）。

### A.3 接口（批量 + 变化清单 + confirm + 幂等）

| 方法 | 路径 | 权限 | 行为 |
|---|---|---|---|
| POST | `/api/v1/admin/series/detect` | admin | body `{"series_ids":[],"library_id":"","confirm":false}`；`series_ids` 空 = 全部剧场；`library_id` 非空 = 该库下的剧场 |
| GET | `/api/v1/admin/tasks/{id}` | admin | 查 `job_tasks` 进度（**新增**） |
| POST | `/api/v1/admin/series/{id}/detect` | admin | **保持原样**（R1 兼容），内部改调 `detect.Series` |

批量语义：

1. `confirm=false` → 同步返回**变化清单**（不落库）：`{series_total, series_with_changes, changes_total, manual_skipped_total, per_series:[{series_id,title,changed,manual_skipped,changes:[...]}]}`；
2. `confirm=true` → 建 `job_tasks`，**202 + `{task_id,total}`**，后台逐剧场调用 `detect.Apply`；
3. **幂等**：同参数重复提交，第二次 `changed=0`（第一次已写 `filename`，`handlers_series.go:422` 的"值相同即跳过"生效），`updated=0`；`ApplySeriesEpisodes` 的 `<> 'manual'`（`series.go:277`）保证 manual 行永远 0 行受影响的重复写入；
4. **部分失败如实**：逐剧场结算，单个剧场失败记入 `per_series[].error` 并计入 `failed`；任务终态 `status='failed'` + `error` 非空，**不谎报 success**（见 A.5）。

### A.4 后台自动识别：时机、理由、代价

**选定时机（唯一）**：扫描任务成功结束后补齐。实现点 = `scanner.go:48` `Run` 写完终态
（`:54` `FinishScanTask`）之后，投递一个 `job_tasks(kind='episode_detect', trigger='scan_finished')`。

| 候选时机 | 采纳 | 理由 / 代价 |
|---|---|---|
| 扫描结束（成功） | ✅ | 重扫是唯一会让 `series_media` 的 `path/title` 变化从而"解析结果该变"的批量事件；扫描已是后台任务，挂在它后面不引入新同步路径 |
| 加入剧场时 | ✅（已有） | `handlers_series.go:318` 已实现，保留 |
| 服务启动 | ❌ | 每次重启全量重扫，**没有新信息**；只增加启动时间与 DB 写 |
| 新增媒体加入剧场 | ✅（=加入剧场时） | 同一入口 |

**代价（如实写）**：扫描任务本身**不因识别失败而回滚**——扫描结果已落库并已报 success；
补齐识别另建 `job_tasks` 独立结算。所以"扫描成功 + 识别失败"是合法组合，前端必须**分别显示**。

**安全规则（写死）**：

1. 只对 `episode_source IS NULL`（未识别/为空）的成员尝试写入；
2. `episode_source='manual'` 的行**跳过并计数**，绝不覆盖（`series.go:277`）；
3. 已有 `filename` 值且与本次解析一致 → **不产生 change、不写库**（`handlers_series.go:422`）；
4. 解析不到 → 留空，界面标「未识别」（`series.js:362`、`series-admin.js:139`），**不许猜**；
5. 不碰 `media` 表、不碰磁盘文件。

### A.5 失败与降级（DegradeReason 语义）

zizvideo 目前**没有** `DegradeReason`（`grep -rn Degrade zizvideo/` 为空）；父面板的语义在
`internal/services/ready.go:83-85`（"降级必须显式给理由，理由为空即错误"）。本轮在 `job_tasks`
上镜像该语义：

- `status='failed'` ⇒ `error` 必须非空（写库前校验，空则 409）；
- `degraded=1` ⇒ `degrade_reason` 必须非空（写库前校验 + 单测）；
- **禁止**：`failed>0` 却报 `success`；`status='success'` 而 `error != ''`；
- 允许的降级举例：某剧场解析器返回"不支持的命名"⇒ `degraded=1`、`degrade_reason='该剧场 N 集文件名不匹配任何模式，已保留未识别'`。

### A.6 前端

| 位置 | 改动 | 文案（≤40 字） |
|---|---|---|
| `series.js:26-31` 页头 | 加「一键识别全部」按钮（仅 admin） | 一键识别全部 |
| 变化清单 | 先 `confirm=false` 预览，确认弹窗再 `confirm=true` | 将更新 N 集，跳过 M 集手动 |
| 进度 | 复用 `admin.js:157-177` 的轮询写法查 `/admin/tasks/{id}` | 识别中 N/M… |
| 单剧场 | `series-admin.js:33-40/202-258` 保持不变 | 自动识别剧集 |

---

## B 指定用户可访问的媒体库（最重要、风险最大）

### B.1 数据模型（迁移草案 `0006_library_access.sql`）

```sql
-- 0006_library_access: 指定用户可访问的媒体库。无行 = 无任何库（fail-closed）。
-- 不写 role='admin' 的授权行：管理员由代码判据直接放行（见 authz.go）。
CREATE TABLE IF NOT EXISTS user_libraries (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  library_id TEXT NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY (user_id, library_id)
);
CREATE INDEX IF NOT EXISTS idx_user_libraries_library ON user_libraries(library_id);
```

- 复合主键 ⇒ 重复授权天然幂等（`INSERT ... ON CONFLICT DO NOTHING`）；
- `ON DELETE CASCADE` 覆盖"硬删用户"；但 `media_libraries` 是**软删**（`0001_init.sql:42`），
  所以库被软删后授权行仍在 ⇒ **判据必须 `JOIN media_libraries ... AND l.deleted_at IS NULL`**，
  否则软删库的授权会变成幽灵授权；
- 不新增列到 `users`：避免"用户表加布尔/JSON"导致的越权默认值问题。

### B.2 授权模型与默认

| 主体 | 可见范围 | 判据 |
|---|---|---|
| `role='admin'` | **全部库**（含禁用库的管理视图） | `scope.All=true` |
| `role='user'`，有授权 | 该用户 `user_libraries` 的**全部行**：`source='admin'`（管理员直授）+ `source='default'`（注册继承）；**来源不参与判据** | 一处 SQL，只在 `authz.go`（B.3 / B.8） |
| `role='user'`，0 行 | **0 个库、0 条媒体**（fail-closed） | 空集合 ⇒ 查询直接返回空，**不允许回落成"全部"** |

- 只有 admin 能授权（`RequireAdmin`，`middleware.go:79`）；
- admin 不能给自己"减权限"（管理视图恒全见），避免把自己锁死；
- 判据**只认库**：不引入"媒体级"白名单，也不引入"组"概念（v1 不做，见 C）。

### B.3 权限收敛层（唯一判据，必须只有一处）

新增 `internal/api/authz.go`：

```go
type LibraryScope struct { All bool; IDs map[string]bool }
func (sc LibraryScope) Allows(libraryID string) bool  // All || IDs[libraryID]

func (s *Server) resolveScope(u *domain.User) (LibraryScope, error)   // admin=>All；user=>SQL
func (s *Server) WithLibraryScope(h http.HandlerFunc) http.HandlerFunc // 挂在 RequireAuth 内层
func ScopeFrom(ctx context.Context) LibraryScope
func (s *Server) canAccessMedia(ctx, mediaID) (*domain.Media, error)  // 不存在与无权同码 404
```

用户集合 SQL（唯一判据；B.8 的 `source` 不参与）：

```sql
SELECT ul.library_id FROM user_libraries ul
JOIN media_libraries l ON l.id = ul.library_id AND l.deleted_at IS NULL
WHERE ul.user_id = ?
```

**"漏用就编译不过"的做法**：把库过滤推进 `storage` 边界，让读接口**必须**带 scope 参数，
不提供"无 scope"的重载；`GetMedia` 改名为包内私有 `getMedia`，对外只暴露
`GetMediaIn(scope, id)`。api 包调不到私有函数 ⇒ 新 handler 不传 scope 就编译失败。

| 存储接口 | 现在 | 改为 |
|---|---|---|
| `media.go:137` `ListMedia(f)` | 无过滤 | `ListMedia(scope, f)`，SQL 加 `AND library_id IN (...)` |
| `media.go:43` `GetMedia(id)` | 导出、无过滤 | 私有 `getMedia`；导出 `GetMediaIn(scope,id)` |
| `feed.go:133` `FeedPage(scope,seed,…)` | `scope` 是单个库或 `""`=全部 | 形参换成 `LibraryScope`；`""`⇒`IDs` 集合，空集合直接返回空 |
| `feed.go:114` `CountPlayable(scope)` | 同上 | 同步；`has_more` 基于过滤后计数（`handlers_feed.go:88`） |
| `feed.go:176` `feedScopeIDs(scope)` | 同上 | 同步 |
| `social.go:64` `ListProgress` / `social.go:169` `mediaJoin` | join 全库 | 加 scope 过滤 |
| `series.go:147` `ListSeriesEpisodes(seriesID)` | 全剧集 | 加 scope；剧场详情按可见集过滤，可见集为 0 ⇒ 剧场视为不可见 |
| `series.go:53` `ListSeries()` | 全部 | list 里剔除"无可见集且无可见封面"的剧场（`seriesJSON` 的 `cover_url` 见 `handlers_series.go:43-47`） |

### B.4 必须走判据的完整清单（`grep` 逐条，30 条路由 + 4 个共享汇点）

契约：**下表每条都必须经过 `ScopeFrom(ctx)` / `canAccessMedia`**。`类型`：R=读内容、E=存在性泄露（写路径）、A=管理端（scope=All）。

| # | 路径 / 汇点 | 路由锚点 | 实现锚点 | 类型 | 收敛动作 |
|---|---|---|---|---|---|
| 1 | `GET /api/v1/media` | `router.go:44` | `handlers_media.go:96-116`；`media.go:137-198` | R | `ListMedia(scope,…)`；`q` 搜索（`media.go:145`）也必须同 scope |
| 2 | `GET /api/v1/media/{id}` | `router.go:45` | `handlers_media.go:131-140` | R | `GetMediaIn(scope,id)`；`path` 仍仅 admin（`:138`） |
| 3 | `GET /api/v1/media/{id}/stream` | `router.go:46` | `handlers_media.go:145-193`（`GetMedia:146`、`GetLibrary:151`、`os.Open:166`） | R | 先 `canAccessMedia` 再判 `Enabled`（`:156`） |
| 4 | `GET /api/v1/media/{id}/cover` | `router.go:47` | `handlers_media.go:254-274`（`os.Open:261`、占位 200 `:270-273`） | R | 无权 → 404，**不许回落成占位图** |
| 5 | `GET /api/v1/feed/next` | `router.go:48` | `handlers_feed.go:38-102`（`GetLibrary:42`、`FeedPage:57/65`、`CountPlayable:88`） | R | `library_id` 越权 → 404； `""` 表示 **scope 集合**而非全库 |
| 6 | `GET /api/v1/series` | `router.go:66` | `handlers_series.go:57-68`；`seriesJSON:43` | R | 剔除无可见内容的剧场；`cover_url` 指向无权 media 时置空 |
| 7 | `GET /api/v1/series/{id}` | `router.go:67` | `handlers_series.go:72-92`（`ListSeriesEpisodes:79`、`buildItems:86`） | R | 按 scope 过滤剧集；全不可见 → 404 |
| 8 | `GET /api/v1/me/progress` | `router.go:53` | `handlers_me.go:45-63`（`ListProgress:48`、`social.go:64-99`） | R | join 加 scope |
| 9 | `GET /api/v1/me/favorites` | `router.go:55` | `handlers_me.go:145-154`（`social.go:193`→`:173`） | R | 同上 |
| 10 | `GET /api/v1/me/likes` | `router.go:57` | `handlers_me.go:157-166`（`social.go:198`→`:173`） | R | 同上 |
| 11 | `PATCH /api/v1/me/progress/{mediaId}` | `router.go:52` | `handlers_me.go:16-42`（`GetMedia:28`） | E | 无权 → 404；不写入 `watch_progress` |
| 12 | `POST /api/v1/me/favorites/{mediaId}` | `router.go:59` | `handlers_me.go:66-78`（`GetMedia:68`） | E | 同上 |
| 13 | `DELETE /api/v1/me/favorites/{mediaId}` | `router.go:60` | `handlers_me.go:81-93`（`GetMedia:83`） | E | 同上（防"无权也能删行"探测） |
| 14 | `POST /api/v1/media/{id}/reactions` | `router.go:61` | `handlers_me.go:102-123`（`GetMedia:113`） | E | 同上 |
| 15 | `PATCH /api/v1/media/{id}/reactions` | `router.go:62` | 同 `HandleSetReaction` | E | 同上 |
| 16 | `DELETE /api/v1/media/{id}/reactions` | `router.go:63` | `handlers_me.go:126-138`（`GetMedia:128`） | E | 同上 |
| 17 | `GET /readyz` | `router.go:24`（匿名） | `handlers_system.go:25-78`（允许根 `:48-57`） | A | 与库授权无直接关系，但**匿名暴露允许根路径**；建议本轮至少不再新增泄露（是否收敛请用户拍板） |
| 18 | `GET /api/v1/libraries` | `router.go:36` | `handlers_libraries.go:13-20` | A | `RequireAdmin` 已挡（`:32`）；`root_path` 绝不进用户端 |
| 19 | `GET /api/v1/libraries/{id}` | `router.go:38` | `handlers_libraries.go:70-98`（`CountMedia:77`） | A | scope=All |
| 20 | `GET /api/v1/admin/system/info` | `router.go:76` | `handlers_system.go:111-139`（`:114-116`） | A | scope=All；全库计数是管理信息 |
| 21 | `GET /api/v1/media/roots` | `router.go:91` | `handlers_roots.go:27-52`（`ListLibraries:29`） | A | scope=All |
| 22 | `GET /api/v1/admin/duplicates` | `router.go:85` | `handlers_admin.go:145-179`（`DuplicateGroups:147`、`os.Stat:163`） | A | scope=All；返回**绝对路径** |
| 23 | `GET /api/v1/admin/audit` | `router.go:77` | `handlers_system.go:150-157` | A | `detail` 含路径/对象 ⇒ 仅 admin |
| 24 | `GET /api/v1/fs/browse` | `router.go:94` | `handlers_roots.go:205-215` | A | scope=All（允许根，非库） |
| 25 | `POST /api/v1/admin/series/{id}/media` | `router.go:71` | `handlers_series.go:264-315`（`GetMedia:281`） | A | scope=All |
| 26 | `POST /api/v1/admin/series/{id}/detect` | `router.go:74` | `handlers_series.go:393-451`（`ListSeriesEpisodes:404`） | A | scope=All（`detect` 新包同样传 All） |
| 27 | `POST /api/v1/admin/series` | `router.go:68` | `handlers_series.go:149-192`（`GetMedia:165`） | A | scope=All |
| 28 | `PATCH /api/v1/admin/series/{id}` | `router.go:69` | `handlers_series.go:195-248`（`GetMedia:221`） | A | scope=All |
| 29 | `POST /api/v1/admin/duplicates/delete-records` | `router.go:86` | `handlers_admin.go:201-232`（`MediaByIDs:212`） | A | scope=All |
| 30 | `POST /api/v1/admin/duplicates/delete-files` | `router.go:87` | `handlers_admin.go:237-349`（`MediaByIDs:254`、`os.Remove:311`） | A | scope=All + 保留手输确认（`:248`） |

共享汇点（不是路由，但**行数据必须已过滤**）：

| # | 汇点 | 锚点 | 风险 |
|---|---|---|---|
| S1 | `buildItems` 装饰器 | `handlers_media.go:48-93` | 被 5 个 handler 调用，自身不判权 ⇒ 调用方必须只传已过滤行 |
| S2 | `ProgressMap` / `Favorites` / `Reactions` | `social.go:42-61 / 116-131 / 150-165` | 按 `media_id` 全量返回用户态 ⇒ 即使行被过滤，也可能把"无权 media 的收藏/进度状态"带出 |
| S3 | `feed_state` scope 键 | `feed.go:38-67`（`scope` 即 library_id） | 未判权就写 `GetFeedState` ⇒ 可探测/污染库游标 |
| S4 | `seriesJSON` 的 `cover_url` | `handlers_series.go:43-46` | 直接拼 `/media/{cover_media_id}/cover`，封面属另一库时绕过剧场过滤 |

> `DELETE /me/progress|favorites|likes`（`router.go:54/56/58`）只清**自己的**行、不返回媒体内容，
> 不需要 scope（但计数是真实的，`favorites.js:101` 已按后端条数显示）。

**统计**：**30 条路由 + 4 个共享汇点 = 34 个必须收敛点**；其中 R 类 10 条（1-10）、E 类 6 条（11-16）、管理 A 类 13 条（17-30 去掉 R/E）、S 类 4 个。

### B.5 越权时返回什么

**定：一律 404，`code=VALIDATION_NOT_FOUND`（`domain.go:173`）**，与"对象真的不存在"完全一致
（`GetMedia` 走 `domain.ErrNotFound`，`media.go:47`）。理由：403 等于确认"这个 id 存在但你无权"，
可用来枚举库/媒体；404 不可区分。逐类：

| 场景 | 返回 |
|---|---|
| `media/{id}`、`stream`、`cover`、`reactions`、`me/progress/{id}`、`favorites/{id}` 指向无权 media | 404（**与不存在同码同文案**） |
| `feed/next?library_id=<无权库>` | 404（现状对不存在库已是 404，`handlers_feed.go:43`，行为一致） |
| `series/{id}` 全剧集不可见 / 无权剧场 | 404 |
| 列表类（`media`、`feed`、`series`、`me/*`） | **静默过滤**，200 + 更短的 list；不报 403、不说明"还有别的库" |
| 管理端接口被普通用户调用 | 403 `FORBIDDEN_ROLE`（现状，`middleware.go:83`）；这是角色错误，不是对象存在性 |
| 库被禁用（`enabled=0`）且有权 | 保持现状 403 `FORBIDDEN_LIBRARY_DISABLED`（`handlers_media.go:157`） |

前端对 404 的处理：不要显示"无权限"，统一显示**空态**（避免把 404 的语义反推给用户）。

### B.6 前端影响

| 页面 | 现状锚点 | 改动 | 文案（≤40 字） |
|---|---|---|---|
| 播放页范围 | `feed.js:227-244`（`libraryCorner`、`来自 X` / `全部库`） | 多库用户加「选库」入口，数据来自 `GET /api/v1/me/libraries` | 只看此库 / 全部库 |
| 播放页空态 | `feed.js:677-680`（"还没有视频"） | 无任何授权库时单独提示 | 没有可访问的媒体库，请联系管理员 |
| 剧场列表 | `series.js:53-71` | 后端已过滤 ⇒ 自然变短；全为空时文案 | 还没有剧场 |
| 剧场播放 | `series.js:350-378` | 404 ⇒ 空态，不重试 | 该剧场不可访问 |
| 收藏页 | `favorites.js:36-87` | 后端过滤后条数会变少；"清除记录"仍用后端 `cleared`（`:101`） | 已清除 N 条（不变） |
| 我的 | `me.js:12-37` | 账号面板加一行"可访问媒体库" | 可访问媒体库：N 个 / 未授权任何媒体库 |
| 新增 API | — | `GET /api/v1/me/libraries`（RequireAuth） | 返回 `[{id,name}]`，**不含 `root_path`** |
| API 客户端 | `api.js:101-107` | 加 `myLibraries()`、`userLibraries(id)`、`setUserLibraries(id, ids)` | CSRF 由 `api.js:42-45` 自动带 |

### B.7 管理界面（给某用户勾选可访问的库）

- 页面：**管理后台 → 用户页签**（`admin.js:396-470` `mountUsers`）；
- 每个用户行加按钮「媒体库权限」（admin 自己那行不显示，或置灰 + 提示"管理员恒可见全部"）；
- 点开抽屉：库列表（来自 `GET /api/v1/libraries`，`api.js:101`）+ 复选框 + 「全选 / 全不选」；底部只读显示**来源**：`管理员授权` / `注册时继承`（`user_libraries.source`，B.8）；
- **保存行为**：`PUT /api/v1/admin/users/{id}/libraries`，body `{"library_ids":[...]}`，**整体替换该用户的 `user_libraries` 行**（幂等，天然支持取消授权）；被管理员勾选的库 `source` 归一为 `'admin'`，未勾选的整行删除（B.8）；
- **失败提示**：保存失败 → banner「保存失败，权限未改」（`setBanner`，`admin.js:426-428`）；**成功必须回读**：响应返回从 DB 重新读出的 `library_ids`，前端**用响应渲染**而不是用本地勾选状态（对齐父面板"回读一致才算生效"，`config.go:324-352`）；
- 顶部提示：存在"0 授权的普通用户"时显示「N 个用户还没有任何库」，旁挂 B.8 的「补发默认可见库」。

### B.8 新注册用户默认可见库（替代分组；必须做）

**目标**：管理员勾若干个库为"新注册用户默认可看"；自助注册的用户自动获得这些库；**不引入任何"组"概念**。

**数据模型（迁移草案 `0008_default_visible.sql`）**：

```sql
-- 0008_default_visible: 默认可见库（给新注册用户）+ 授权来源标记。
-- 默认可见库集合 = media_libraries.default_for_new_users=1 的行；空集合 = 未设置（fail-closed）。
ALTER TABLE media_libraries ADD COLUMN default_for_new_users INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_libraries_default_new ON media_libraries(default_for_new_users)
  WHERE deleted_at IS NULL;

-- 授权来源：admin = 管理员直授；default = 注册时继承的默认可见库。
-- 仅用于显示/审计，不参与判据（B.3 的唯一判据仍是 user_libraries 的全部行）。
ALTER TABLE user_libraries ADD COLUMN source TEXT NOT NULL DEFAULT 'admin'
  CHECK (source IN ('admin', 'default'));
```

**语义（写死）**：

| # | 语义 | 落点 |
|---|---|---|
| 1 | "默认可见库集合" = `default_for_new_users=1` 且未软删的库；**空集合 = 未设置** | `media_libraries.default_for_new_users` |
| 2 | 自助注册时把默认库**在同一个事务里**写成该用户的显式授权行（`source='default'`） | `handlers_admin.go:84` → `CreateUserWithDefaults(u, libraryIDs)` |
| 3 | 管理员之后改"哪些库默认可看"**不追溯**已注册用户 | 授权已是 `user_libraries` 的行，与开关解耦 |
| 4 | 未设默认库 ⇒ 新注册用户看不到任何库（fail-closed） | 写入 0 行；`resolveScope` 空集合（B.2），**不回落成"全部"** |
| 5 | 最终可见 = 该用户 `user_libraries` 的全部行（`source` 只是标注） | B.3，只在 `authz.go` 一处 |

**`source` 列取舍结论：采纳"单表 + source 列"，不另开表。**

- 好处：`(user_id, library_id)` 仍只有一行，天然去重；判据 SQL 仍是单表
  `SELECT library_id FROM user_libraries WHERE user_id=?`，比"并集多一张表"更简单；界面能区分来源。
- 代价（如实写）：**同一个库无法同时留"继承"和"管理员直授"两条记录**。规则归一为：
  管理员保存时勾选项 `ON CONFLICT DO UPDATE SET source='admin'`，未勾选项整行删除。
  唯一失去的场景是"既保留继承来源、又额外直授同一库"——实际等价于直授，无功能损失。
- **不变量**：`source` **不参与判据**（`resolveScope` 内不得出现 `source`，否则就是第二个判据分支，
  违背 B.3"只有一处"）。门禁见 D.5。

**只对自助注册继承**：`POST /api/v1/auth/register`（`handlers_admin.go:84`）继承；
管理员创建用户 `POST /api/v1/users`（`handlers_users.go:30`）**不继承**（管理员显式建号、显式授权）。
（如需一致，可在 `createUserReq` 加 `inherit_defaults:true` —— 拍板点。）

**管理界面**：

- 位置：**管理后台 → 媒体库页签**（`admin.js:69-237` `mountLibraries`），每行加开关「新注册用户默认可看」；
- 保存：`PATCH /api/v1/libraries/{id}` 带 `default_for_new_users`（扩展现有 `libraryReq`/`LibraryPatch`，`handlers_libraries.go:22-28`、`libraries.go:96-103`），与 `enabled` 同路径；**回读生效值才算成功**（`setEditing`/`refresh` 既有回读，`admin.js:122-155`）；
- 未设任何默认库时，媒体库页与用户页顶部常驻：`未设置新用户默认可看库，新用户看不到任何内容`（≤40 字）；
- 文案：开关 `新注册用户默认可看`；抽屉来源 `管理员授权` / `注册时继承`。

**管理动作：把默认可见库补发给所有未授权用户**（fail-closed 迁移后老用户是空的，这是捞回入口）：

- 入口：「用户」页顶部（存在 0 授权普通用户时出现）按钮 `补发默认可见库`；
- 接口：`POST /api/v1/admin/libraries/defaults/backfill`，body `{"confirm":true}`；
- 前置校验：未设任何默认库 ⇒ 409 `DEFAULT_LIBRARIES_UNSET`，文案 `未设置默认可见库，无法补发`（**不许**"补发了 0 个库还报成功"）；
- 作用域：只处理 `role='user'` 且 `user_libraries` 行数为 0 的用户；已有任何授权的用户**跳过**（不追加、不覆盖）；
- 写入：把当前默认库逐行 `INSERT ... ON CONFLICT DO NOTHING`，`source='default'`（幂等）；
- 确认：二次确认弹窗显示预估 `将影响 N 个未授权用户（共 M 个用户）`；
- 结果如实：走 `job_tasks`（迁移 `0007`，kind=`default_backfill`，202+`task_id`），终态含 `{users_total, users_granted, users_skipped, rows_written}`；部分失败 ⇒ `status='failed'` + `error` 非空（A.5 语义）；
- 文案：结果 `已补发 N 个用户，跳过 M 个`（≤40 字）；幂等：重复点第二次 `users_granted=0`。

---

## C 媒体库分组 / 按组授权（本轮不做）

**不做（用户 2026-09-20 决定）**：分组 + 按组授权的增量虽小，但会引入多对多并集歧义、孤儿授权，
以及"没有有效权限预览就容易误配"的必要性；诉求（新用户可见某些库）已由 **B.8 默认可见库**满足。
原 D.5 中的多对多 / 孤儿授权 / 组相关门禁随之删除。

---

## D 门禁清单（每条可断言、能抓复发）

### D.1 权限类

| 门禁 | 断言 | 位置 |
|---|---|---|
| **矩阵测试** | 4 用户（admin / alice→L1 / bob→L1+L2 / carol→无）× 16 条用户路径（B.4 的 1-16）× 每库各 1 条 media；越权必须 **404 且 code 相同**；列表路径断言无权 media id **不出现** | 新增 `internal/api/library_authz_test.go`，复用 `helpers_test.go:45 newEnv` / `:342 newLibrary` / `:353 newMedia` |
| **路由源扫描** | 解析 `internal/web/router.go`，凡路径含 `/media`、`/series`、`/feed`、`/me` 且非管理端的注册项，必须包 `s.WithLibraryScope`；新增接口漏包 → 测试红 | 新增 `internal/api/route_scope_test.go`（源码文本扫描；Go `ServeMux` 不可反射，如实说明这是"源码级"门禁） |
| **编译期强制** | 读接口必须带 scope（B.3 表）：不给无 scope 重载；`GetMedia` 私有化 ⇒ 漏传编译失败 | `storage/media.go:43`、`media.go:137`、`feed.go:114/133/176`、`social.go:64/169` |
| **All-scope 唯一构造点** | `grep -rn "LibraryScope{" internal/ --include=*.go` 只允许命中 `authz.go` 与 `_test.go` | `tools/check-*` 或 Go 测试 |
| **admin 不被误挡** | 矩阵里 admin 必须对**所有**库的 16 条路径 200；`scope.All` 与 `role` 判断写反会立刻红 | 同上矩阵 |
| **角色/CSRF 回归** | 保留 `authz_test.go:44`（普通用户进管理端 403）、`authz_test.go:77`（写操作必须带 CSRF）、`authz_test.go:59`（`/libraries` 必须 403） | 现有文件 |

### D.2 识别类

| 门禁 | 断言 | 锚点 |
|---|---|---|
| 只填未识别 | 预置空 season/episode → detect confirm 后填充；再跑一次 `changed=0 && updated=0` | `handlers_series.go:422`、`series.go:277` |
| 不覆盖 manual | 先 `PUT .../order` 标 manual（`series.go:361-363`）→ detect confirm → `manual_skipped≥1` 且该行 season/episode 不变 | `series.go:268-292` |
| 批量幂等 | 批量 confirm 连跑两次：第 2 次 `updated=0`；`series_ids` 子集不影响其它剧场 | 新端点 |
| 后台失败如实 | 注入 DB 错误 → `job_tasks.status='failed'` 且 `error!=''`；`degraded=1` 时 `degrade_reason` 非空 | 迁移 `0007` + A.5 |
| 解析表 | 复用 `internal/media/episode_test.go`（≥20 条真实命名，含 `Movie.2023.1080p.x265.mp4` 不得识别成 2023/1080） | 现有 |

### D.3 迁移类

| 门禁 | 断言 |
|---|---|
| **升级即 fail-closed（有意）** | `TestMigration0006FailClosed`：用 0005 结构的库 + 一个历史普通用户 → 迁移后该用户 `GET /media` 空、`GET /me/libraries` 空、`GET /libraries` 403；admin 全见。**这条测试把"历史用户被挡在门外"锁成预期行为**，防止以后有人"顺手"改成默认全放 |
| 迁移幂等 | 重复启动：`schema_version=6/7/8` 且不重复执行、不重复插行 |
| CASCADE | 硬删用户 → `user_libraries` 行消失 |
| 软删库 | 软删库后，判据立即不再返回其授权（JOIN `deleted_at IS NULL`）；已继承的 `user_libraries` 行保留但不可见（D.5 最后一条） |
| 幽灵授权 | 软删库后重新建同名库（新 id）：旧授权**不得**对新库生效 |

### D.4 前端与文案

- `node tools/check-js-syntax.mjs zizvideo/internal/web/assets` + `node tools/check-js-undeclared.mjs …`（写法见 `ITERATION-1.md:43`）；
- 用户可见文案 ≤40 字（本节所有文案已按此写）；
- 门禁可选：grep 前端不得出现"无权限/403"之类的存在性提示（统一空态）。

### D.5 默认可见库与注册继承类

| 门禁 | 断言 |
|---|---|
| **新用户能看到默认库（且只看得到这些）** | 设 L1、L2 为默认可见（L3 不是）→ 自助注册 → `GET /me/libraries` = {L1,L2}；`GET /media` 只含 L1/L2；`feed/next` 不返回 L3；`GET /media/{L3 的 id}` = 404 |
| **未设默认 ⇒ fail-closed** | 清空所有 `default_for_new_users` → 注册新用户 → `GET /me/libraries` 空、`GET /media` 空、`feed/next` 200 + 空 list（**不是 500、更不是全库**） |
| **改默认不追溯** | u 在 {L1} 为默认时注册 → 把默认改成 {L2} → u 仍只见 L1；此后注册的 v 只见 L2（两条断言都要有） |
| **管理员建号不继承** | `POST /api/v1/users` 建的普通用户 → `GET /me/libraries` 空（只有自助注册 `POST /auth/register` 继承），把"只对自助注册"锁死 |
| **`source` 不参与判据** | 把某用户 `source='default'` 的行手工改成 `'admin'`（或反之）→ 可见集合**不变**；grep `resolveScope` 内不得出现 `source` |
| **补发动作** | 未设默认 ⇒ 409 `DEFAULT_LIBRARIES_UNSET`；设默认后补发只影响 0 授权用户、已有授权用户**不变**；重复补发 `users_granted=0`；返回计数与 DB 行数一致（不许谎报） |
| **注册写路径事务** | 注入 `user_libraries` 写入失败 → **建用户整体回滚**（不得出现"有用户、无授权"） |
| **默认库软删** | 软删一个默认库后：已继承该库的用户仍保留 `user_libraries` 行，但判据 JOIN 掉软删库 ⇒ 立即不可见（不报错）；此后注册的新用户拿不到它 |

---

## E 工作量与风险

### E.1 分阶段（每阶段可独立验收）

| 阶段 | 内容 | 产出 | 独立验收 | 估时 |
|---|---|---|---|---|
| **P1（最大风险，先做）** | 迁移 `0006`；`authz.go` 判据；storage scope 签名改造；B.4 的 16 条用户路径 + `/me/libraries`；矩阵与路由门禁 | 16 条路径 + 4 汇点全部收敛 | 矩阵全绿 + 真机三用户（admin/alice/carol） | 1.5–2 轮 |
| **P2** | 迁移 `0007 job_tasks`；`detect` 内核抽取；批量接口；扫描结束补齐；前端按钮与进度 | A 全套 | 幂等 / manual / 失败如实门禁 | 1 轮 |
| **P3** | 迁移 `0008`（`default_for_new_users` + `user_libraries.source`）；用户库权限抽屉 + 回读；库上的「新注册用户默认可看」开关 + 未设置提示；注册继承（同事务快照）；「补发默认可见库」 | B 全套（含替代分组的 B.8） | D.5 八条门禁绿 + 勾选保存后回读一致 | 1 轮 |

> **C 取消后阶段从 4 段缩成 3 段**：原 P4（分组）整体删除，其中"默认可见库 + 注册继承 + 补发 +
> 用户授权界面"并入 **P3**；**P1/P2 不变**。P3 因此从 0.5 轮上调为 **1 轮**：它多碰一处**注册写路径**
> （`handlers_admin.go:84` → `CreateUserWithDefaults`），需要单独的用例（D.5 的「注册写路径事务」）。
> 「补发默认可见库」保留为 fail-closed 迁移的兜底：即使管理员没设默认库，也能显式选择怎么补。

### E.2 最容易出问题的点与兜底门禁

> 前三条是主风险（#1 漏路径、#2 fail-closed 迁移、#3 识别谎报），#4/#5 紧随其后。

| # | 风险 | 为什么会漏 | 兜底门禁 |
|---|---|---|---|
| 1 | **漏路径**：尤其 `me/progress|favorites|likes` 的 join 聚合（`social.go:64/173`）、`seriesJSON.cover_url`（`handlers_series.go:43-47`）、`cover` 的占位 200（`handlers_media.go:270-273`） | 这些看起来"已经是自己的数据/已经是剧场内部数据"，直觉上不需要库过滤 | 矩阵测试（D.1）+ 路由源扫描 + storage 签名强制 scope；P1 验收只看矩阵是否覆盖 16 条 |
| 2 | **老库升级把家人挡在门外** | fail-closed 是**有意**的，但用户会当成 bug 报"升级后没视频了" | 迁移门禁 D.3 锁死预期 + B.8「补发默认可见库」+ 管理端"未授权用户"/"未设默认可看库"提示 + 发布说明写清这是有意行为 |
| 3 | **自动识别覆盖 manual / 谎报成功** | 新增批量与后台路径时绕过 `ApplySeriesEpisodes` 自己写 SQL；或 `failed>0` 仍报 success | 唯一写入口 `series.go:268` + `WHERE <> 'manual'`；`job_tasks` 的 `degraded⇒reason` 校验；D.2 四条门禁 |
| 4（紧随） | **admin 被误挡 / 用户端读到 `root_path`** | scope 判据把 `role` 判断写反；或把 `/libraries` 直接给用户端复用 | 矩阵含 admin 用例；`/libraries` 保持 `RequireAdmin`（`router.go:36`）；`/me/libraries` 结构体里根本没有 `root_path` 字段 |
| 5 | **默认可见库语义写歪**：注册先建用户、后写授权（失败即"有用户无授权"）；把 `source` 带进判据造成第二套过滤；或改默认开关时顺手追溯了老用户 | 这是本轮唯一会碰**注册写路径**的改动，事务与"来源不参与判据"最容易被忽略 | `CreateUserWithDefaults` 单事务（`users.go:33`）+ `source` 只做标注（B.8 不变量）+ D.5 的"新用户默认可见 / 未设默认 fail-closed / 改默认不追溯 / 注册事务回滚"四条 |

---

## 硬约束（本轮）

- 本文件是唯一产物；**不改代码、不 commit、不 push、不 bump、不碰 mini、不跑 `make check`**
  （另有代理在改代码）。
- 任何"看起来不对"的结论先怀疑探测方式；本文所有行号来自 `8efcd8a`，符号名为准。
- 未实现/未验证项必须如实标注；不许把"设计了"写成"已实现"。
