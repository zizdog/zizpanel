# Mac mini M4 服务器管理面板 — 开发指引

> 版本：v1.0
> 更新日期：2026-09-12
> 适用环境：Mac mini M4 / macOS 15.7

---

## 一、项目概述

### 1.1 背景
- 硬件：Mac mini M4
- 系统：macOS 15.7 (Sequoia)
- 定位：个人/小型团队服务器
- 主业务：网站部署（LNMP）
- 副业务：提供可远程调用的 AI/工具服务（图片压缩、语音转文字、文字转语音、图片去水印等）

### 1.2 现状
- 已有 PHP 编写的 LNMP 管理面板（简陋，仅管理站点/环境）
- 需要演进成类似宝塔面板的一体化管理面板
- 支持裸装服务和 Docker 服务的统一管理

### 1.3 目标
打造一个集网站部署 + 服务管理 + AI 能力调用于一体的自托管面板，具备：
- 统一的服务管理（裸装 + Docker）
- WebUI 操作，支持远程
- 可扩展的服务注册机制
- 与现有 LNMP 面板平滑集成

---

## 二、核心架构原则

### 2.1 服务部署决策原则

| 判断维度 | 裸装（系统安装） | Docker |
|----------|------------------|--------|
| 需要 Metal/GPU 加速 | 必须裸装 | 用不了 |
| LNMP 主业务 | 优先 | 一般 |
| 纯 CPU/网络服务 | 一般 | 优先 |
| 需要 WebUI 的现成工具 | 一般 | 优先 |
| 需要深度集成进面板 | 一般 | 优先 |
| 需要频繁换版本/试错 | 一般 | 优先 |
| 需要隔离依赖 | 一般 | 优先 |

### 2.2 一句话原则
能用 Metal 的 → 裸装；纯 CPU/网络服务 → Docker；LNMP 主业务 → 裸装。

### 2.3 统一抽象
无论裸装还是 Docker，都抽象为「服务（Service）」对象，面板用同一套 UI 和接口管理。

Service (抽象)
├── NativeService  (launchctl 管理)
└── DockerService  (Docker API 管理)

---

## 三、服务清单与部署方式

### 3.1 裸装服务（Native）

| 服务 | 端口 | 说明 | 管理方式 |
|------|------|------|----------|
| Nginx | 80/443 | 主 Web 服务器 | 现有面板 |
| MySQL | 3306 | 数据库 | 现有面板 |
| PHP-FPM | 9000 | 多版本共存 | 现有面板 |
| whisper.cpp + FastAPI | 9001 | 语音转文字（Metal 加速） | launchctl |
| edge-tts + FastAPI | 9002 | 文字转语音 | launchctl |
| IOPaint | 8080 | 图片去水印（MPS） | launchctl |
| Ollama | 11434 | 本地大模型（可选） | brew services |

### 3.2 Docker 服务

| 服务 | 端口 | 说明 |
|------|------|------|
| Pic Smaller | 8081 | 图片压缩（类 TinyPNG） |
| Stirling PDF | 8082 | PDF 工具箱 |
| MinIO | 9000 | 对象存储 |
| n8n | 5678 | 自动化工作流 |
| Uptime Kuma | 3001 | 服务监控 |
| Gitea | 3000 | 自建 Git |

### 3.3 网络层

Nginx 反代：所有服务统一走域名

| 域名 | 目标服务 |
|------|----------|
| img.domain.com | Pic Smaller |
| tts.domain.com | TTS API |
| stt.domain.com | STT API |
| pdf.domain.com | Stirling PDF |
| panel.domain.com | 管理面板 |

远程访问：
- Tailscale（推荐，零配置）
- Cloudflare Tunnel（可绑定公网域名）

---

## 四、面板功能需求

### 4.1 已有功能（保留）
- LNMP 环境管理
- 站点管理（增删改查、SSL）

### 4.2 新增功能

#### 4.2.1 服务管理模块（核心）
- 服务列表：卡片式展示，显示图标、名称、类型（native/docker）、状态灯、端口
- 服务详情：启停/重启按钮、实时日志、健康检查结果
- 服务注册：表单添加新服务（选择类型、填端口、命令等）
- Compose 部署：页面上贴 docker-compose.yml，一键部署
- 镜像管理：拉取/删除/清理 Docker 镜像
- 端口占用检测：部署前校验端口冲突

#### 4.2.2 反向代理管理
- 在站点管理里加「反向代理」Tab
- 选择目标服务 → 自动生成 nginx conf → reload

#### 4.2.3 API Key 管理
- 给每个内部服务生成/查看/重置 API Key
- 远程调用需带 X-API-Key

#### 4.2.4 监控集成
- 嵌入 Uptime Kuma（iframe）或自建简易监控

### 4.3 功能模块划分

面板
├── 系统概览（CPU/内存/磁盘/网络）
├── 网站管理（现有 LNMP）
│   ├── 站点列表
│   ├── SSL 证书
│   ├── 反向代理   ← 新增
│   └── 数据库
├── 服务管理   ← 重点新增
│   ├── 服务列表（裸装 + Docker 混合）
│   ├── 服务详情（启停/日志/健康）
│   ├── Compose 部署
│   └── 镜像管理
├── 文件管理（可选）
├── 监控告警（Uptime Kuma 嵌 iframe）
└── 设置（API Key、备份、通知）

---

## 五、AI 服务 API 规范

### 5.1 通用要求
- 路径前缀：/api/v1/
- 鉴权：Header X-API-Key: xxx
- 响应：JSON
- 健康检查：GET /health 返回 {"status":"ok"}

### 5.2 STT 服务（9001）
POST /api/v1/transcribe
  form-data: file=<audio>
  query: language=zh (可选)
返回: {"text": "...", "segments": [...]}

### 5.3 TTS 服务（9002）
GET /api/v1/tts?text=你好&voice=zh-CN-XiaoxiaoNeural
返回: audio/mpeg

### 5.4 去水印（IOPaint 8080）
POST /api/v1/inpaint
  form-data: image=<file>, mask=<file>
返回: image/png

### 5.5 API Key 鉴权中间件示例

from fastapi import FastAPI, Header, HTTPException

app = FastAPI()
API_KEY = "your-secret"

@app.middleware("http")
async def auth(request, call_next):
    if request.url.path == "/health":
        return await call_next(request)
    if request.headers.get("X-API-Key") != API_KEY:
        raise HTTPException(401)
    return await call_next(request)

---

## 六、PHP 面板演进建议

### 6.1 演进路线选择

| 方案 | 适合场景 | 建议 |
|------|----------|------|
| A. 保留 PHP，渐进增强 | 面板已稳定、你熟悉 PHP | 推荐先用 |
| B. PHP 后端 + 新前端 | 想换 UI 但保留后端逻辑 | 折中 |
| C. 完全重写（FastAPI + Vue） | 想长期做成产品级 | 后期再考虑 |

推荐路线：走 A 路线，但架构上做两件事：
1. 把「服务管理」抽成独立模块，不跟 LNMP 逻辑耦合
2. 后端加统一的「服务抽象层」，裸装和 Docker 用同一套接口

### 6.2 立即能做的改进

#### 6.2.1 建立服务注册表

CREATE TABLE services (
    id INT PRIMARY KEY AUTO_INCREMENT,
    name VARCHAR(64),
    display_name VARCHAR(128),
    type ENUM('native','docker'),
    port INT,
    launchctl_label VARCHAR(128),
    plist_path VARCHAR(255),
    work_dir VARCHAR(255),
    start_cmd TEXT,
    container_name VARCHAR(128),
    compose_file VARCHAR(255),
    health_url VARCHAR(255),
    log_path VARCHAR(255),
    icon VARCHAR(255),
    enabled TINYINT DEFAULT 1,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

#### 6.2.2 抽象出统一的服务操作接口

interface ServiceDriver {
    public function start(): bool;
    public function stop(): bool;
    public function restart(): bool;
    public function status(): string;   // running/stopped/error
    public function logs(int $lines): string;
    public function health(): bool;
}

两个实现：
- NativeServiceDriver：调 launchctl
- DockerServiceDriver：调 Docker API（走 socket）

#### 6.2.3 前端加一个「服务」页面
- 卡片式展示所有服务（图标 + 名称 + 状态灯 + 端口）
- 点击进入：启停按钮、日志实时刷新、健康检查

### 6.3 PHP 调用 Docker（关键实现）

不要用 shell 调 docker 命令，直接走 socket 的 HTTP API。

class DockerClient {
    private string $socket = '/var/run/docker.sock';

    private function request(string $method, string $path): array {
        $ch = curl_init();
        curl_setopt_array($ch, [
            CURLOPT_UNIX_SOCKET_PATH => $this->socket,
            CURLOPT_URL => "http://localhost$path",
            CURLOPT_CUSTOMREQUEST => $method,
            CURLOPT_RETURNTRANSFER => true,
        ]);
        $res = curl_exec($ch);
        curl_close($ch);
        return json_decode($res, true) ?? [];
    }

    public function listContainers(): array {
        return $this->request('GET', '/containers/json?all=1');
    }

    public function start(string $name): void {
        $this->request('POST', "/containers/$name/start");
    }

    public function stop(string $name): void {
        $this->request('POST', "/containers/$name/stop");
    }
}

### 6.4 用 launchctl 管理裸装服务

macOS 15 推荐用 bootstrap/bootout：

- 启动：launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/xxx.plist
- 停止：launchctl bootout gui/$(id -u)/com.you.service
- 查看：launchctl list | grep com.you

plist 模板（~/Library/LaunchAgents/com.you.stt.plist）：

<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.you.stt</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/uvicorn</string>
        <string>stt_api:app</string>
        <string>--port</string><string>9001</string>
    </array>
    <key>WorkingDirectory</key><string>/opt/services/stt</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>/var/log/stt.log</string>
    <key>StandardErrorPath</key><string>/var/log/stt.err</string>
</dict>
</plist>

### 6.5 中期改进

| 改进 | 说明 |
|------|------|
| SSE 日志推送 | text/event-stream 轮询 |
| Compose 编辑器 | 页面贴 yml → 存文件 → docker compose up -d |
| 统一反代管理 | 站点管理加「反向代理」Tab |
| API Key 管理 | 服务生成 key，面板可查看/重置 |
| 端口冲突检测 | 部署前扫一遍已用端口 |

### 6.6 长期：是否换技术栈

- 如果只是自用 → PHP 继续用没问题，加现代前端（Vue3 + Element Plus）
- 如果想做成产品 → 建议后端换 FastAPI

迁移策略：先把「服务管理」模块用 FastAPI 单独写成微服务（panel-api），PHP 面板通过 HTTP 调它。

---

## 七、技术实现细节

### 7.1 日志实时推送（SSE）

header('Content-Type: text/event-stream');
header('Cache-Control: no-cache');
while (true) {
    $log = tail($logFile, 50);
    echo "data: " . json_encode($log) . "\n\n";
    ob_flush(); flush(); sleep(2);
}

### 7.2 端口冲突检测

function isPortFree(int $port): bool {
    exec("lsof -i :$port 2>&1", $out);
    return empty($out);
}

---

## 八、开发优先级

### 阶段 1（1-2 周）
1. 建 services 表 + ServiceDriver 接口
2. 实现 NativeServiceDriver（launchctl）
3. 前端「服务列表 + 详情」页面
4. 先接入 STT/TTS/IOPaint 三个裸装服务

### 阶段 2（2-4 周）
1. 实现 DockerServiceDriver（Docker socket API）
2. 服务列表支持 Docker 类型
3. Compose 编辑器页面
4. 日志 SSE 推送

### 阶段 3（按需）
1. 反向代理自动化
2. API Key 管理
3. 监控集成
4. 服务市场（预置模板）

---

## 九、技术栈约定

### 当前
- 后端：PHP
- 前端：原生 HTML/JS

### 建议演进
- 前端升级到 Vue3 + Element Plus（保持 PHP 后端）
- 服务管理模块可选独立为 FastAPI 微服务（长期）

### 代码规范
- 所有系统命令调用必须用 escapeshellarg 转义
- 敏感操作需二次确认
- 日志统一格式：[时间] [服务] [级别] 消息
- 配置文件放 /opt/panel/config/，数据放 /Volumes/Data/

---

## 十、安全要求

- 面板本身必须 HTTPS + 登录鉴权
- 远程 API 必须有 API Key
- Docker socket 权限谨慎
- 所有用户输入做校验，禁止命令注入
- 敏感信息加密存储

---

## 十一、文件目录约定

/opt/panel/                    面板代码
├── app/
│   ├── Services/              ServiceDriver 实现
│   ├── Controllers/
│   └── Models/
├── public/                    Web 根目录
├── config/
└── storage/logs/

/opt/services/                 裸装服务代码
├── stt/
├── tts/
└── iopaint/

/opt/compose/                  Docker compose 文件
├── pic-smaller.yml
├── stirling.yml
└── minio.yml

/Volumes/Data/
├── www/                       网站数据
├── docker/data/               Docker 数据卷
├── media/
└── backup/

~/Library/LaunchAgents/        裸装服务 plist
├── com.you.stt.plist
├── com.you.tts.plist
└── com.you.iopaint.plist

---

## 十二、系统基础配置

### 12.1 关闭睡眠
sudo pmset -a sleep 0 disksleep 0 displaysleep 0
sudo pmset -a womp 1
sudo pmset -a autorestart 1
sudo pmset -a powernap 0

### 12.2 关闭自动更新
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool false
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool false
sudo defaults write /Library/Preferences/com.apple.SoftwareUpdate CriticalUpdateInstall -bool false

### 12.3 关闭 Spotlight 索引
sudo mdutil -a -i off
sudo mdutil -i off /Volumes/Data

### 12.4 远程访问
系统设置 → 通用 → 共享
- 远程登录 (SSH)
- 屏幕共享 (VNC)
- 文件共享 (SMB)

固定 IP：系统设置 → 网络 → 以太网 → 详细信息 → TCP/IP → 手动配置

### 12.5 SSH 安全加固
sudo nano /etc/ssh/sshd_config
# Port 2222
# PasswordAuthentication no
# PermitRootLogin no

### 12.6 Docker 环境选择

| 方案 | 优点 | 缺点 | 推荐 |
|------|------|------|------|
| Docker Desktop | 官方、GUI 友好 | 商用需授权 | 个人免费 |
| OrbStack | 极快、省电 | 部分功能收费 | 推荐 |
| Colima | 开源免费 | 无 GUI | 命令行党 |

brew install orbstack

备选 Colima：
brew install colima docker docker-compose docker-buildx
colima start --cpu 6 --memory 12 --disk 200 --vm-type=vz --mount-type=virtiofs
brew services start colima

---

## 十三、端口规划表

| 端口 | 服务 |
|------|------|
| 80/443 | Nginx |
| 3306 | MySQL |
| 9000 | MinIO |
| 9001 | STT API |
| 9002 | TTS API |
| 8080 | IOPaint |
| 8081 | Pic Smaller |
| 8082 | Stirling PDF |
| 5678 | n8n |
| 3000 | Gitea |
| 3001 | Uptime Kuma |
| 11434 | Ollama |

---

## 十四、参考项目

| 项目 | 参考价值 |
|------|----------|
| 宝塔面板 | 功能参考（服务市场、站点管理） |
| Portainer | Docker 管理 UI 参考 |
| Dockge | Compose 管理交互参考 |
| Homepage | 服务导航卡片设计参考 |

---

## 十五、避坑总结

| 坑 | 解决 |
|----|------|
| Docker 用不了 Metal GPU | AI 服务原生安装 |
| M4 上 x86 镜像卡顿 | 强制 --platform linux/arm64 |
| 睡眠导致服务掉线 | pmset 关闭所有睡眠 |
| 端口冲突 80/443 | 关闭系统自带 Apache |
| Spotlight 拖慢外接盘 | mdutil -i off |
| 内网 IP 变动 | 路由器 DHCP 静态绑定 |
| macOS 15 launchctl load 弃用 | 用 bootstrap/bootout |
| PHP 调 Docker 命令注入 | 用 Docker socket API，不用 shell |

---

## 附录：常用速查

Docker 常用
docker compose up -d
docker compose logs -f
docker stats

查看端口占用
lsof -i :8080

系统监控
htop

launchctl（macOS 15）
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/xxx.plist
launchctl bootout gui/$(id -u)/com.you.service
launchctl list | grep com.you

Ollama
brew install ollama
brew services start ollama
ollama pull qwen2.5:14b

Whisper
brew install whisper-cpp
whisper-cpp -m ~/models/ggml-large-v3.bin -f audio.mp3

IOPaint
pip install iopaint
iopaint start --model=lama --device=mps --port=8080 --host=0.0.0.0

---

文档结束