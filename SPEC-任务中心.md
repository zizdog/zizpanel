# SPEC：任务中心（安装/卸载的实时进度）

> 对标宝塔面板的工作逻辑：**装什么都要看得见过程，而且窗口随时能关、随时能重开**。
> 本文件是后端与前端之间的契约，改接口先改这里。

## 一、要解决的问题

1. **只有"请等待"**：安装是同步 HTTP 请求，`brew install` / `docker compose up` 的输出被
   `CombinedOutput()` 攒在内存里，直到几分钟后才作为 `steps` 数组一次性返回。
   用户在整个过程中看不到任何真实进展（下载了没、卡在哪一步）。
2. **关掉窗口就再也找不回来**：前端那个「后台继续（关闭窗口）」按钮只隐藏了 DOM，
   用户没有任何入口能看到后续进度。
3. **可能被浏览器"顺手杀掉"**：所有安装都用 `r.Context()` 执行。
   用户一刷新/关标签页，HTTP 请求被取消 → 正在跑的 `brew`/`docker` 子进程被杀，
   机器上留下**装到一半**的状态。

## 二、设计

### 后端：一个内存任务注册表

- 新包 `internal/tasks`：`Manager` 持有最近 N（30）个任务。
- 每个任务：`ID / Kind / Target / Title / Status / StartedAt / FinishedAt / Error / Lines[]`。
- `Lines` 是**有界环形缓冲**（每任务最多 4000 行），并带全局递增的 `seq`，
  前端据此断点续传（`after=` / SSE `Last-Event-ID`）。
- 长任务在独立 goroutine 里跑，用 `context.WithCancel(context.Background())` 派生 ——
  **不挂在 HTTP 请求上**，所以关窗口、刷新、换页面都不会中断安装。
- 每行日志通过订阅广播给所有 SSE 订阅者（非阻塞发送，慢订阅者丢行而不是卡住安装）。
- 任务只存内存：面板进程重启后，"正在安装"本身就是假的（子进程已随进程组结束），
  所以**不做持久化**，但进程重启会把上次残留的 running 任务丢弃 —— 这是有意的。

### 进度从哪来

1. **步骤消息**：`InstallResult.Steps` 的每一次 append 都同步发一条 `step` 行
   （`res.step(ctx, "...")`）。
2. **命令输出**：所有长命令的执行助手（`brewRun` / `runAsUser` / `runAsUserEnv` /
   `runRoot` / `runColima` / compose 的 `run`）改为**逐行流式**读取 stdout+stderr，
   ANSI 转义剥掉后原样进任务日志。brew 的下载百分比、docker 的层进度、
   pip 的收集/安装都因此可见。命令本身（可执行文件 + 参数）也记一行。

### 接口

全部需要登录（`requireAuth`）。形状遵循面板既有约定：`{ok:true,data:{...}}`。

```
GET  /api/v1/tasks                        # 最近任务（新的在前）
     data: { tasks: [TaskMeta...] }

GET  /api/v1/tasks/{id}?after=<seq>&limit=800
     data: { task: TaskMeta, lines: [Line...], next_after: <int>, done: <bool> }

GET  /api/v1/tasks/{id}/stream            # SSE
     event: lines   data: {"lines":[Line...]}         # 先补历史，再推增量
     event: status  data: TaskMeta                    # 状态变化（含结束）
     : hb                                              # 心跳注释，防代理断流

POST /api/v1/tasks/{id}/cancel            # 中断（会 kill 子进程，可能留下半装状态）
     data: { task: TaskMeta }

POST /api/v1/market/{id}/install          # 改为异步：立刻返回 task_id（HTTP 202）
POST /api/v1/market/install-lnmp          # 同上
POST /api/v1/market/install-phpmyadmin    # 同上
POST /api/v1/market/install-qwentts       # 同上
POST /api/v1/market/install-voicereceiver # 同上
POST /api/v1/market/install-iopaint       # 同上
DELETE /api/v1/services/{name}/uninstall  # 同上
     data: { task_id: "t-...", title: "安装 PHP 8.3 (FPM)" }
```

`TaskMeta`：

```json
{
  "id": "t-1757833071234-7",
  "kind": "install",              // install | uninstall | deploy
  "target": "php83",              // 应用 id 或服务名
  "title": "安装 PHP 8.3 (FPM)",
  "status": "running",            // running | succeeded | failed | canceled
  "started_at": "2026-09-14T10:31:02+08:00",
  "finished_at": "0001-01-01T00:00:00Z",
  "elapsed_ms": 12345,
  "line_count": 812,
  "last": "==> Downloading https://…",   // 最后一行，列表页直接展示
  "error": "",                            // 失败原因（status=failed 时）
  "result": { }                           // 成功后的 InstallResult（含 token/address/service）
}
```

`Line`：

```json
{ "seq": 42, "at": "2026-09-14T10:31:05+08:00", "level": "cmd|step|out|err|ok|warn", "text": "…" }
```

`level` 决定前端配色：`step`＝加粗小标题、`out`＝等宽正文、`err`＝红、
`ok`＝绿、`warn`＝黄、`cmd`＝灰（显示执行的命令）。

### 前端：任务中心

- `internal/web/assets/js/tasks.js` 暴露单例 `taskCenter`：
  - 顶栏按钮（`taskCenter.button()`）：有任务时显示 `⟳ N`，点击打开任务列表；
    **不依赖当前路由**，所以任何页面都能重开。
  - 任务列表：运行中在前，显示标题/状态/耗时/最后一行；点任意一项打开进度窗。
  - 进度窗：标题 + 状态徽标 + 实时耗时 + 步骤/日志（等宽、自动滚到底，
    用户手动上滚时暂停自动滚动并给「回到底部」）+ `复制日志` +
    「关闭窗口（后台继续）」+ 「中断」。
  - 页面加载时 `GET /api/v1/tasks`：有 running 任务就在顶栏亮起徽标
    （**不自动弹窗**，避免打断用户；但要能一眼看见）。
- 应用市场/服务管理里发起安装后：拿 `task_id` 直接打开进度窗；
  卡片上若该应用有进行中的任务，按钮变成「查看进度」。
- 进度窗用 `modal()`，`onClose` **不取消任务**（关闭＝收起窗口）。

## 三、非目标（本轮不做）

- 不做安装队列的串行化（并发安装各自独立；仅禁止**同一个 target** 重复启动）。
- 不做任务持久化（见上）。
- 不做断点续装、不做安装回滚。
- 不做假百分比：brew/docker 没有可信的总进度，进度＝当前步骤 + 真实输出流 + 已用时。
