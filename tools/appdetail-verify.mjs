// appdetail-verify.mjs —— 端到端验证「应用」页四个一级 Tab（已安装 / 应用市场 /
// docker / 一键建站）与「应用管理面板」在**唯一**入口下是同一个，卡片按钮清单
// 完全由数据决定。
//
// 为什么这么搭：
//   · 真 acorn（tools/check-js-syntax.mjs）只能证明语法合法，证明不了"点开是同一个面板"，
//     所以这里用 Playwright 加载**真实的** apps.js / services.js / servicePanel.js；
//   · app.js 是应用外壳（会 boot、拉会话、渲染登录页），在这里只会捣乱，
//     所以用 importmap 把它换成一个最小替身（只提供 state / panelPath /
//     registerCleanup / routeFor）—— **被验证的三个文件本身一个字符都没有改**；
//   · 后端用假 fetch：要验的是"按钮由数据决定"，所以给几个代表性应用就够。
//
// 历史需求（仍然锁着）：已安装卡片上只有「打开」+ 启停/重启/刷新/管理，
// 「重装/卸载/文档/直链」收进「⚙️ 管理」面板；刷新按钮文案是「⟳ 刷新」；ffmpeg
// （no_daemon）不显示"读取中/未在服务管理里"，面板也不去查不存在的记录。
//
// 2026-09-17 第二版信息架构（四个一级 Tab）新增的验收：
//   ① 四个 Tab 存在（已安装 / 应用市场 / docker / 一键建站）且默认选中「已安装」，
//      `#/services` 与 `#/apps/docker` 都能落到对的 Tab；
//   ② 卡片网格（`.grid` 下的卡片）而不是行；
//   ③ 已安装卡片**只有「打开」、没有「直链」**；
//   ④ 不支持子路径的卡片上有那句**逐字**提示
//      "该应用不支持子路径，请用端口访问，或自行配置反代。"；
//   ⑤ 应用市场是**一个列表**，同一时刻既有已安装也有未安装条目
//      （没有安装状态筛选 / 分类筛选下拉）；
//   ⑥ docker Tab 里**没有任何安装按钮**，且有"预配置 docker compose 文件"的醒目提示，
//      且每张卡片给出默认端口 / 需要的镜像 / compose 文件的操作；
//   ⑦ 去重仍然生效：php82 只有一张卡片（两条服务记录 + 市场条目合并成一条）。
//
// 用法： export PATH=/opt/homebrew/bin:$PATH; node tools/appdetail-verify.mjs
import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const JS_DIR = path.join(ROOT, 'internal/web/assets/js');

// ---------- 1. 静态服务器（ESM 必须走 http，file:// 会被 CORS 挡） ----------
const MIME = { '.js': 'text/javascript; charset=utf-8', '.html': 'text/html; charset=utf-8' };
const server = http.createServer((req, res) => {
  const rel = decodeURIComponent(new URL(req.url, 'http://x').pathname).replace(/^\/+/, '');
  // 合成一个壳页面：只需要同源 + 一个挂载点
  if (rel === '' || rel === 'index.html') {
    res.writeHead(200, { 'Content-Type': MIME['.html'] });
    res.end('<!doctype html><html><head><meta charset="utf-8">'
      + '<script type="importmap">{"imports":{"__stub__":"/__stub__.js"}}</script>'
      // #toasts 是 ui.toast 的挂载点：真实外壳由 app.js 建，这里必须补上，
      // 否则"超时兜底"那条路径一 toast 就 TypeError（脚本会直接崩）。
      + '</head><body><div id="root"></div><div id="toasts"></div></body></html>');
    return;
  }
  if (rel === '__stub__.js') {
    res.writeHead(200, { 'Content-Type': MIME['.js'] });
    // app.js 的最小替身：被验证的模块只用到这三样东西。
    res.end(`
export const state = { session: { user: { username: 'admin' }, config: { panel_entry: '/' } }, metrics: null, route: '' };
export const NAV = [];
export function panelPath(sub = '') { return '/' + String(sub).replace(/^\\/+/, ''); }
export function registerCleanup() { return () => {}; }
`);
    return;
  }
  const file = path.join(JS_DIR, rel);
  if (!file.startsWith(JS_DIR) || !fs.existsSync(file)) { res.writeHead(404); res.end('nope'); return; }
  let body = fs.readFileSync(file);
  // 只对 app.js 做替换：把它所有 import 换成那个替身，避免把整个应用外壳拖进来。
  if (rel === 'app.js') {
    body = body.toString().replace(/^import[^;]*;/gm, '');
    // app.js 里的 NAV 引用了各个页面的 view 组件（外壳才有），这里用不到、也没导入，
    // 直接换成空表，免得把整个应用外壳拖进来。
    const navStart = body.indexOf('export const NAV = [');
    const navEnd = body.indexOf('];', navStart);
    if (navStart >= 0 && navEnd > navStart) body = body.slice(0, navStart) + 'export const NAV = [];' + body.slice(navEnd + 2);
    body = "import * as __shell from '__stub__';\n" + body;
  }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
  res.end(body);
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch();
const page = await browser.newPage();
page.on('console', (m) => { if (m.type() === 'error') console.log('  [console]', m.text()); });
page.on('pageerror', (e) => console.log('  [pageerror]', e.message));

// ---------- 2. 假数据：字段与后端 JSON 一一对应 ----------
const MARKET = {
  docker: { available: true, version: '28.0.0', socket: '/var/run/docker.sock' },
  // 板块顺序与中文名由后端给出（services.MarketSections → GET /api/v1/market 的 sections）。
  // 前端**不该**自带分类中文名 —— 这一轮用户要求把最后一个板块从「其它」
  // 改名为「基础环境」，所以下面直接断言前端渲染出来的标题就是后端给的那份。
  sections: [
    { key: 'site', label: '一键建站' },
    { key: 'ai', label: 'AI 服务' },
    { key: 'tool', label: '运维工具' },
    { key: 'other', label: '基础环境', fallback: true },
  ],
  list: [
    {
      id: 'frpc', name: 'frpc（frp 客户端）', icon: '🧷', category: 'tool', kind: 'native',
      summary: '把本机端口映射到 frps（客户端，带 admin UI）', description: 'fatedier/frp 的客户端。',
      port: 7400, installed: true, adopted: true, available: true,
      service_label: 'com.zizdog.frpc', service_in_launchd: true, panel_installer: 'frpc',
      config_path: 'frpc.toml', post_install_hint: '① serverAddr/serverPort 要改成你自己 frps 的地址与端口',
      ui: { slug: 'frpc', console_only: true, prefer_direct: true, note: 'frpc 的 admin UI 在 7400' },
      port_url: 'http://192.168.1.4:7400/',
      uninstall: { kind: 'installer', service: 'com.zizdog.frpc', steps: ['停止并删除 launchd 服务', '删除 ~/frpc'], data_paths: ['/Users/zizdog/frpc'], keep_note: '配置与数据会保留' },
    },
    {
      id: 'orbien-client', name: 'Orbien 客户端（CLI）', icon: '🛰️', category: 'tool', kind: 'native',
      summary: '连上 Orbien 服务端，把本机端口穿透出去（客户端）', description: 'Orbien 的客户端（上游 CLI）。',
      port: 0, installed: true, adopted: true, available: true,
      service_label: 'com.zizdog.orbien-client', service_in_launchd: true, panel_installer: 'orbien-client',
      config_path: 'orbien.toml',
      uninstall: { kind: 'installer', service: 'com.zizdog.orbien-client', steps: ['停止并删除 launchd 服务'], data_paths: ['/Users/zizdog/orbien-client'] },
    },
    {
      id: 'qwen3tts', name: 'Qwen3 TTS（语音合成）', icon: '🗣️', category: 'ai', kind: 'native',
      summary: '本地语音合成，支持音色克隆', description: '按 TtsVoice 插件契约原生安装。',
      port: 8880, installed: true, adopted: true, available: true,
      service_label: 'com.zizdog.qwen3tts', service_in_launchd: true, panel_installer: 'qwen3tts',
      uninstall: { kind: 'installer', service: 'com.zizdog.qwen3tts', steps: ['停止并删除 launchd 服务'], data_paths: ['/Users/zizdog/qwen3tts'] },
    },
    {
      id: 'voicereceiver', name: 'TtsVoice 音色接收端', icon: '🔐', category: 'ai', kind: 'native',
      summary: '接收音色样本 + 带鉴权的反向代理', description: '给网站插件用的对外入口。',
      port: 8899, installed: true, adopted: true, available: true,
      service_label: 'com.zizdog.voicereceiver', service_in_launchd: true, panel_installer: 'voicereceiver',
      uninstall: { kind: 'installer', service: 'com.zizdog.voicereceiver', steps: ['停止并删除 launchd 服务'], data_paths: ['/Users/zizdog/voicereceiver'] },
    },
    {
      // 2026-09-17 用户点名的"反了"：IT-Tools 有面板子路径（ui.slug，且**没有**
      // prefer_direct），「打开」必须给 /it-tools/；卡片上不再有「直链」。
      // 它是 docker 推荐项目（docker_rec + 接口给的 images / compose_url）：
      // 只在「docker」Tab 出现，应用市场里不再有它。
      id: 'it-tools', name: 'IT-Tools（开发者工具箱）', icon: '🧰', category: 'tool', kind: 'compose',
      summary: '几十个开发者常用小工具，纯前端', description: '开发者小工具合集。',
      port: 8083, installed: true, adopted: true, available: true,
      service_label: 'it-tools', service_in_launchd: false, port_url: 'http://192.168.1.4:8083/',
      compose_yaml: 'services:\n  it-tools:\n    image: ghcr.io/corentinth/it-tools:latest\n',
      // 后端真实字段：App.DockerReference → docker_reference；
      // market item 还有别名 docker_recommended（见 api_services.go）。
      docker_reference: true,
      images: ['ghcr.io/corentinth/it-tools:latest'],
      compose_url: 'http://192.168.1.8:8090/compose/it-tools/docker-compose.yml',
      compose_env_url: 'http://192.168.1.8:8090/compose/it-tools/.env.example',
      compose_readme_url: 'http://192.168.1.8:8090/compose/README.md',
      ui: { slug: 'it-tools' },
      docs_url: 'https://github.com/CorentinTh/it-tools',
      uninstall: { kind: 'service', service: 'it-tools', steps: ['docker compose down（删除容器与网络）'], keep_note: 'compose 应用只删容器与网络，**具名卷（数据）保留**' },
    },
    {
      id: 'uptime-kuma', name: 'Uptime Kuma', icon: '📡', category: 'tool', kind: 'compose',
      summary: '自托管服务监控与告警', description: '监控网站与服务的可用性。',
      port: 3001, installed: true, adopted: true, available: true,
      service_label: 'uptime-kuma', service_in_launchd: false, port_url: 'http://192.168.1.4:3001/',
      compose_yaml: 'services:\n  uptime-kuma:\n    image: louislam/uptime-kuma:1\n',
      // 只有内容、没有下载地址：docker 卡片上必须给「复制 compose 配置」（不能是空操作）。
      // 标记走 market item 的别名 docker_recommended（前端两个字段都要认）。
      docker_recommended: true,
      ui: { slug: 'uptime-kuma', prefer_direct: true, note: 'Uptime Kuma 官方不支持子路径' },
      uninstall: { kind: 'service', service: 'uptime-kuma', steps: ['docker compose down（删除容器与网络）'], keep_note: 'compose 应用只删容器与网络，**具名卷（数据）保留**' },
    },
    {
      // docker 推荐项目：接口**既没给标记也没给 compose 内容/地址**（只靠 kind=compose
      // 兜底进 docker Tab）。卡片上必须把位置留出来并如实说明，而且**仍然不许出现
      // 任何安装按钮**（用户："不提供安装"）。
      id: 'n8n-docker', name: 'n8n（推荐 compose 项目）', icon: '🔗', category: 'tool', kind: 'compose',
      summary: '工作流自动化（推荐用 compose 自己跑）', description: '面板只提供预配置 compose 文件。',
      port: 5678, installed: false, adopted: false, available: true,
    },
    {
      // 用户点名的那条 bug：命令行工具、故意没有守护进程（目录里 NoDaemon、没有
      // service_label）。后端会报 installed=true / no_daemon=true / artifacts 到处都在，
      // 但**没有服务记录**。界面上必须说"命令行工具（无常驻进程）"，
      // 不能显示"读取中…"或"未在服务管理里"。
      id: 'ffmpeg', name: 'FFmpeg（音视频工具）', icon: '🎬', category: 'tool', kind: 'native',
      summary: '音视频转码基础工具（TTS 编码 mp3 依赖它）',
      description: '装好后供面板与其它应用在后台调用，没有网页界面。',
      port: 0, installed: true, adopted: false, available: true,
      no_daemon: true, panel_installer: 'ffmpeg',
      uninstall: { kind: 'installer', steps: ['卸载 brew 包'], data_paths: [], keep_note: '' },
      docs_url: 'https://ffmpeg.org',
    },
    {
      // 装了、但服务管理里**没有**记录（adopted=false）：配置文件按钮必须禁用，
      // 因为市场条目里的 config_path 只是文件名，拿不到绝对路径。
      id: 'stirling-pdf', name: 'Stirling PDF', icon: '📄', category: 'tool', kind: 'native',
      summary: 'PDF 工具箱', description: '自托管的 PDF 处理工具。', port: 8080,
      installed: true, adopted: false, available: true,
      service_label: 'com.zizdog.stirling-pdf', service_in_launchd: false,
      panel_installer: 'binary-release', config_path: 'stirling.toml',
      docs_url: 'https://docs.stirlingpdf.com',
      uninstall: { kind: 'installer', service: 'com.zizdog.stirling-pdf', steps: ['停止并删除 launchd 服务'], data_paths: ['/Users/zizdog/stirling-pdf'] },
    },
    {
      // 未安装（用户要求清单里"一个未安装的"）：主按钮必须是「安装」，
      // 而且不出现「管理」（没有东西可管）。
      id: 'ollama', name: 'Ollama', icon: '🦙', category: 'ai', kind: 'native',
      summary: '本地大模型推理，支持 Metal 加速', description: '一行命令跑起本地大模型。',
      port: 11434, installed: false, adopted: false, available: true,
      docs_url: 'https://ollama.com',
    },
    {
      // 2026-09-21 用户："弱化管纳这个概念 … 用户不需要知道什么是纳管。"
      // adopt_label 非空 = 后端对这条只做 AdoptApp（把本机**已经存在**的服务
      // 登记进面板，不重新安装）。所以卡片主按钮必须是用户语言的「添加到面板」，
      // 界面上不许出现"纳管 / 接入 / 托管"。
      id: 'ollama-adopt', name: 'Ollama（本机已有）', icon: '🦙', category: 'ai', kind: 'native',
      summary: '本机已经装了 Ollama，只把它加到面板里管理', description: '不重新安装，只登记已有服务。',
      port: 11434, installed: false, adopted: false, available: true,
      adopt_label: 'com.ollama.serve',
    },
    {
      id: 'nginx', name: 'Nginx', icon: '🌐', category: 'lnmp', kind: 'native',
      summary: 'Web 服务器', description: 'brew 装的 nginx。', port: 8080,
      installed: true, adopted: true, available: true,
      service_label: 'homebrew.mxcl.nginx', service_in_launchd: true,
      uninstall: { kind: 'forget', service: 'nginx', steps: ['从「服务管理」中删除这条记录'] },
    },
    {
      // 2026-09-17 合并验收：真机上 php81/82/83/84 每套在服务记录里都是**两条**
      // （php82 与 sh-brew-php8-2，launchd 标签一个 homebrew.mxcl.php@8.2、
      // 一个 sh.brew.php8-2）。合并后「我的应用」里必须只剩**一行**。
      // 这条市场条目故意带 ui.slug + port_url：用来断言合并后的同一行既有
      // 「打开 / 直链」又有「管理」（两边的元数据都拿到了）。
      id: 'php82', name: 'PHP 8.2 (FPM)', icon: '🐘', category: 'lnmp', kind: 'native',
      summary: 'PHP 8.2 运行环境', description: 'brew 装的 PHP 8.2（FPM）。', port: 9000,
      installed: true, adopted: true, available: true,
      service_label: 'homebrew.mxcl.php@8.2', service_in_launchd: true,
      ui: { slug: 'php82' }, port_url: 'http://192.168.1.4:9000/',
      uninstall: { kind: 'forget', service: 'php82', steps: ['从「服务管理」中删除这条记录'] },
    },
    {
      // 超时兜底验证：它的服务查询**永不返回**。面板必须在硬超时后落定成终态，
      // 而不是永远停在「正在读取服务状态…」。用户反馈的"卡在读取中"就是这一类。
      id: 'slow-app', name: 'Slow App（超时兜底验证）', icon: '🐢', category: 'tool', kind: 'native',
      summary: '假应用：服务查询故意永不返回', description: '只用于验证面板的硬超时终态。',
      port: 0, installed: true, adopted: true, available: true,
      service_label: 'com.zizdog.slowapp',
      uninstall: { kind: 'installer', service: 'com.zizdog.slowapp', steps: ['停止服务'] },
    },
    {
      // 一键建站类（category: site）：只在「一键建站」Tab 出现（市场里不再有）。
      id: 'typecho', name: 'Typecho', icon: '📝', category: 'site', kind: 'native',
      summary: '轻量博客程序', description: '一键装好并配好伪静态。', port: 0,
      installed: false, adopted: false, available: true,
      site_app: { rewrite: 'typecho', finish_path: '/install.php', needs_db: true, notes: [] },
    },
    {
      // 用户点名的一键建站程序之二：WordPress。
      id: 'wordpress', name: 'WordPress', icon: '📰', category: 'site', kind: 'native',
      summary: '最流行的建站程序', description: '一键装好并配好伪静态。', port: 0,
      installed: false, adopted: false, available: true,
      docs_url: 'https://wordpress.org',
      site_app: { rewrite: 'wordpress', finish_path: '/wp-admin/install.php', needs_db: true, notes: [] },
    },
    {
      // KindColima 的归类比对：它必须出现在「Docker」筛选档里（它就是 Docker 引擎）。
      // category: runtime 没有专属板块 → 落在兜底板块「基础环境」。
      id: 'docker-runtime', name: 'Docker 运行时（Colima）', icon: '🐳', category: 'runtime', kind: 'colima',
      summary: '容器引擎，Docker 类应用的前提', description: 'Colima 容器运行时。', port: 0,
      installed: false, adopted: false, available: true, panel_installer: 'docker-runtime',
    },
  ],
};

const SV = (o) => ({
  driver_ready: true, category: 'tool', managed: true,
  health: { checked: true, ok: true, message: 'HTTP 200', latency_ms: 12, url: 'http://127.0.0.1/' },
  state: { running: true, status: 'running', pid: 4242, endpoint: 'http://127.0.0.1:7400/' },
  ...o,
});

const SERVICES = {
  list: [
    SV({ name: 'com.zizdog.frpc', display_name: 'frpc（frp 客户端）', icon: '🧷', kind: 'native', port: 7400, launch_label: 'com.zizdog.frpc', config_path: '/Users/zizdog/frpc/frpc.toml', log_path: '/Users/zizdog/frpc/frpc.log', work_dir: '/Users/zizdog/frpc', plist_path: '/Library/LaunchDaemons/com.zizdog.frpc.plist' }),
    SV({ name: 'com.zizdog.orbien-client', display_name: 'Orbien 客户端（CLI）', icon: '🛰️', kind: 'native', port: 0, launch_label: 'com.zizdog.orbien-client', config_path: '/Users/zizdog/orbien-client/orbien.toml', state: { running: false, status: 'stopped', detail: '未在运行' } }),
    SV({ name: 'com.zizdog.qwen3tts', display_name: 'Qwen3 TTS（语音合成）', icon: '🗣️', kind: 'native', port: 8880, category: 'ai', launch_label: 'com.zizdog.qwen3tts' }),
    SV({ name: 'com.zizdog.voicereceiver', display_name: 'TtsVoice 音色接收端', icon: '🔐', kind: 'native', port: 8899, category: 'ai', launch_label: 'com.zizdog.voicereceiver', config_path: '/Users/zizdog/voicereceiver/receiver.toml' }),
    SV({ name: 'uptime-kuma', display_name: 'Uptime Kuma', icon: '📡', kind: 'compose', port: 3001, compose_file: '/Users/zizdog/compose/uptime-kuma/docker-compose.yml', container: 'uptime-kuma' }),
    SV({ name: 'nginx', display_name: 'Nginx（brew）', icon: '🌐', kind: 'native', port: 8080, category: 'lnmp', managed: false, launch_label: 'homebrew.mxcl.nginx', health: { checked: false } }),
    // 真机重复对的复刻：同一套 PHP 8.2 有两条服务记录、两套 launchd 标签写法
    // （点分前缀 + @ 的 homebrew.mxcl.php@8.2，与连字符写法 sh.brew.php8-2）。
    // 一条在跑、一条停着；**字段故意错开**（plist 路径只在被丢弃那条上）——
    // 合并后必须只剩一行，状态取在跑的那条、plist 路径补自另一条。
    SV({ name: 'php82', display_name: 'PHP 8.2 (FPM)', icon: '🐘', kind: 'native', port: 0, category: 'lnmp', managed: false, launch_label: 'homebrew.mxcl.php@8.2', config_path: '/opt/homebrew/etc/php/8.2/php-fpm.d/www.conf', log_path: '/opt/homebrew/var/log/php-fpm.log', state: { running: true, status: 'running', detail: 'pid 1234' } }),
    SV({ name: 'sh-brew-php8-2', display_name: 'PHP 8.2', icon: '🐘', kind: 'native', port: 0, category: 'lnmp', managed: false, launch_label: 'sh.brew.php8-2', plist_path: '/Users/zizdog/Library/LaunchAgents/homebrew.mxcl.php@8.2.plist', health: { checked: false }, state: { running: false, status: 'stopped', detail: '未在运行' } }),
    // ---- 「需要处理」筛选合并的夹具（2026-09-21）----
    // 三种"需要处理"必须**全部**被合并后的一个筛选命中：
    //   ① 启动失败（status=error）  ② 运行时不可用（status=unavailable）
    //   ③ 健康检查没通过（health.checked && !health.ok）
    // 合并前它们是两个筛选（「异常」= ①②， 「仅健康检查失败」= ③），而 ③ 是
    // ①②那个判据的一部分 —— 这里独立按**旧定义**重算一遍期望值，
    // 断言合并后一张都不少（用户：别把有问题却看起来正常的情况藏起来）。
    SV({ name: 'uitest-svc-error', display_name: 'UITEST 启动失败', icon: '💥', kind: 'native', port: 0, managed: false, state: { running: false, status: 'error', detail: '启动失败：退出码 1' }, health: { checked: false } }),
    SV({ name: 'uitest-svc-unavail', display_name: 'UITEST 运行时不可用', icon: '🧯', kind: 'native', port: 0, managed: false, state: { running: false, status: 'unavailable', detail: 'Docker 不可用' }, health: { checked: false } }),
    SV({ name: 'uitest-svc-health', display_name: 'UITEST 健康检查没过', icon: '🩺', kind: 'native', port: 0, managed: false, state: { running: true, status: 'running', detail: 'pid 777' }, health: { checked: true, ok: false, message: '连接被拒绝', url: 'http://127.0.0.1:9/' } }),
  ],
};

const CREDS = {
  'com.zizdog.frpc': { config_path: '/Users/zizdog/frpc/frpc.toml', ui: 'http://127.0.0.1:7400', credentials: [{ key: 'webServer.user', label: '管理界面用户名', value: 'admin' }, { key: 'webServer.password', label: '管理界面口令', value: 's3cret-pw' }] },
  'com.zizdog.orbien-client': { config_path: '/Users/zizdog/orbien-client/orbien.toml', ui: '', credentials: [] },
  // 接收端只报内部 token：**不该**出现「查看凭据」
  'com.zizdog.voicereceiver': { config_path: '/Users/zizdog/voicereceiver/receiver.toml', ui: '', credentials: [{ key: 'ttsv_token', label: '共享密钥', value: 'ttsv-deadbeef' }] },
  'com.zizdog.qwen3tts': { config_path: '', ui: '', credentials: [] },
  'uptime-kuma': { config_path: '', ui: '', credentials: [] },
  nginx: { config_path: '/opt/homebrew/etc/nginx/nginx.conf', ui: '', credentials: [] },
};

await page.addInitScript(({ MARKET, SERVICES, CREDS }) => {
  window.__calls = [];
  // 服务夹具也挂到 window 上：page.evaluate 里要用**旧定义**独立重算
  // "需要处理"筛选的期望值（SERVICES 本身只是 addInitScript 的闭包参数，
  // 在 evaluate 里取不到 —— 2026-09-21 这里踩过一次 ReferenceError）。
  window.__servicesFixture = SERVICES;
  // 每次写请求的**请求体**（"方法 路径" → body 字符串）。
  // 2026-09-19 起一键 LNMP 必须带上用户选的版本，所以必须能看到 body：
  // 只看"发过 POST"无法证明"发的是用户选的那个版本"。
  window.__bodies = {};
  // 网站管理页的假数据：默认"一个站点都没有"（要验空站点列表时的大按钮），
  // 测试里把它换成"有一个站点"再渲染一次（要验工具条上的次级按钮）。
  window.__sitesPayload = {
    list: [], presets: [], php_versions: [], www_root: '/Users/zizdog/www',
  };
  // 一键 LNMP 的候选版本（GET /market/lnmp-options 的桩数据）。
  // 与真实后端同形：三组 + 每组 recommended 一项（PHP 8.2 是默认）。
  window.__lnmpOptions = {
    groups: [
      { key: 'nginx', label: 'Nginx', selected: 'nginx',
        options: [{ formula: 'nginx', name: 'Nginx', summary: 'Web 服务器', recommended: true, installed: true }] },
      { key: 'php', label: 'PHP', selected: 'php@8.2',
        options: [
          { formula: 'php@8.4', name: 'PHP 8.4 (FPM)', summary: 'PHP 8.4', recommended: false, installed: false },
          { formula: 'php@8.2', name: 'PHP 8.2 (FPM)', summary: 'PHP 8.2', recommended: true, installed: true },
        ] },
      { key: 'mysql', label: 'MySQL', selected: 'mysql@8.4',
        options: [{ formula: 'mysql@8.4', name: 'MySQL 8.4', summary: '数据库', recommended: true, installed: false }] },
    ],
    default: { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' },
  };
  window.fetch = async (url, init = {}) => {
    const u = String(url);
    const method = (init.method || 'GET').toUpperCase();
    window.__calls.push(method + ' ' + u);
    if (init.body) window.__bodies[method + ' ' + u.split('?')[0]] = String(init.body);
    const done = (data, ok = true, status = 200) => ({
      ok, status, statusText: ok ? 'OK' : 'ERR', text: async () => JSON.stringify({ ok, data }),
    });
    // 超时验证：这条查询**永远不返回**（模拟后端假死/连接被挂住）。
    if (u.includes('/services/com.zizdog.slowapp')) return new Promise(() => {});
    if (u.includes('/session')) return done({ user: { username: 'admin' }, config: { panel_entry: '/' } });
    if (u.includes('/market/proxies')) return done({ enabled: true, items: [] });
    // 候选版本必须在 /market 的通配之前匹配（真实路由也是先注册它）。
    if (u.includes('/market/lnmp-options')) return done(window.__lnmpOptions);
    // 安装接口：回 202 + task_id（前端据此开任务进度窗）。
    if (u.includes('/market/install-lnmp')) {
      return {
        ok: true, status: 202, statusText: 'Accepted',
        text: async () => JSON.stringify({ ok: true, data: { task_id: 'stub-lnmp-1', title: '一键 LNMP' } }),
      };
    }
    if (/\/market(\?|$)/.test(u)) return done(MARKET);
    if (/\/sites(\?|$)/.test(u)) return done(window.__sitesPayload);
    const m = u.match(/\/services\/([^/?]+)\/credentials/);
    if (m) {
      const name = decodeURIComponent(m[1]);
      if (CREDS[name]) return done(CREDS[name]);
      return done(null, false, 404);
    }
    const one = u.match(/\/services\/([^/?]+)(\?|$)/);
    if (one && !u.includes('/services?')) {
      const name = decodeURIComponent(one[1]);
      const s = SERVICES.list.find((x) => x.name === name);
      if (s) return done(s);
      return done(null, false, 404);
    }
    if (u.includes('/services')) return done(SERVICES);
    if (u.includes('/voice/receiver/keys')) return done({ keys: [], total: { chars: 0, today_chars: 0, week_chars: 0, requests: 0, audio_bytes: 0, days: {} } });
    if (u.includes('/voice/receiver/sources')) return done({ sources: [] });
    if (u.includes('/qwen/models')) return done({ list: [], active: '' });
    if (u.includes('/files/read')) return done({ content: '# 假配置\n' });
    return done(null, false, 404);
  };
}, { MARKET, SERVICES, CREDS });

await page.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });

// ---------- 3. 渲染四个 Tab（+ 网站管理页），采集卡片与按钮清单 ----------
const result = await page.evaluate(async () => {
  const root = document.getElementById('root');
  // 这里**故意**清一次旧的筛选记忆（zp-market-kind-filter / 安装状态筛选已经随
  // 改版删除，如果哪天有人把它加回来，下面的"没有筛选下拉"断言会立刻发现）。
  try { localStorage.removeItem('zp-market-kind-filter'); } catch { /* 无 localStorage 也没关系 */ }
  const { AppsView } = await import('./apps.js');
  const { SitesView } = await import('./sites.js');
  const { openServicePanel, appKeyOf } = await import('./servicePanel.js');
  // 「应用」页外壳：只借 routeFor 验 `#/services` 的别名（app.js 被服务端换成
  // 最小替身，NAV 为空，但路由函数一字未改）。
  const { routeFor } = await import('./app.js');

  // 禁用状态也要看：用户的原话是"点了没用的按钮比没有按钮更糟"，
  // 所以清单里把 disabled 标出来（禁用是**如实说明**，不是漏做）。
  const btns = (el) => Array.from(el.querySelectorAll('button, a.btn'))
    .map((b) => ((b.textContent || '').trim() + (b.disabled ? '〔禁用〕' : '')))
    .filter((t) => t && t !== '×');
  // 卡片标题：优先取带 font-weight: 620 的那一行；市场卡片是 div，服务卡片是 span。
  // 兜底时用"按钮文案之前的文本"当名字，避免把整张卡片的文字都当成标题打出来。
  const cardName = (c) => {
    const el = c.querySelector('div[style*="font-weight: 620"] span, span[style*="font-weight: 620"]')
      || c.querySelector('div[style*="font-weight: 620"]');
    if (el && (el.textContent || '').trim()) return (el.textContent || '').trim();
    const txt = (c.textContent || '').trim();
    return txt.split(/重装|查看服务|安装|停止|启动|打开/)[0].trim();
  };

  // 面板的"操作"区：第一个 .section-title（标题是「操作」）到下一个 .section-title 之间
  const panelSnapshot = () => {
    const m = document.querySelector('.modal');
    if (!m) return null;
    const title = (m.querySelector('.modal-head h3')?.textContent || '').trim();
    const body = m.querySelector('.modal-body');
    const titles = Array.from(body.querySelectorAll('.section-title'));
    const op = titles.find((t) => t.textContent.trim() === '操作');
    const scope = document.createElement('div');
    if (op) {
      let n = op.nextElementSibling;
      while (n && !(n.classList && n.classList.contains('section-title'))) {
        scope.appendChild(n.cloneNode(true)); n = n.nextElementSibling;
      }
    }
    const statusLine = (body.firstElementChild?.textContent || '').trim();
    const panelText = (body.innerText || '').trim();
    const buttons = btns(scope);
    // 「打开 / 直链」的 href 与 title 也要采：合并后纯纳管服务（没有 port_url）
    // 的直链是用 location.hostname + 端口拼出来的，title 里必须说明这一点。
    const links = Array.from(scope.querySelectorAll('a.btn')).map((a) => ({
      text: (a.textContent || '').trim(),
      href: a.getAttribute('href') || '',
      title: a.getAttribute('title') || '',
    }));
    // scope 里的节点是 clone 出来临时挂的，用完必须摘掉：
    // 否则它们会留在文档里，被下一次 document.querySelectorAll 采到，
    // 于是"服务管理点开的面板"实际上一直采的是上一次市场的面板（第一版就栽在这）。
    scope.remove();
    return { title, status: statusLine.slice(0, 160), panelText: panelText.slice(0, 1200), buttons, links };
  };
  const closeModal = () => { const m = document.querySelector('.modal-mask'); if (m) m.remove(); };
  const settle = () => new Promise((r) => setTimeout(r, 120));

  const out = {
    tabs: {}, market: {}, installed: {}, docker: {}, sites: {}, route: {},
    panelFromMarket: {}, panelFromService: {}, timeout: {},
  };
  // 网站管理页 LNMP 按钮的引用（见 ①c / ①e）；DOM 节点不进 out。
  let lnmpListBtn = null;

  // 采集市场板块标题（顺序有意义：兜底板块「基础环境」必须在最后）。
  const sectionTitles = (box) => Array.from(box.querySelectorAll('.section-title'))
    .map((e) => (e.textContent || '').trim()).filter(Boolean);
  const linkList = (scope) => Array.from(scope.querySelectorAll('a.btn'))
    .map((a) => (a.textContent || '').trim() + ' → ' + (a.getAttribute('href') || ''));
  // 页内 Tab 条：当前选中项的 data-tab（btn-primary 是选中态）。
  const activeTabOf = (box) => Array.from(box.querySelectorAll('[data-tab]'))
    .find((b) => b.classList.contains('btn-primary'))?.getAttribute('data-tab') || null;
  // 卡片上的"已安装"判据：pill 文案里有「已安装」。
  // 2026-09-21 起面板里不再有「已纳管」这个 pill（内部词，用户要求弱化），
  // 面板有记录的应用显示的就是「已安装」。
  const looksInstalled = (c) => /已安装/.test(c.textContent || '');

  // ---- ① 四个一级 Tab：存在、默认「已安装」 ----
  const tabBox = document.createElement('div');
  root.appendChild(tabBox);
  AppsView(tabBox, {});
  await settle(); await settle();
  out.tabs.labels = Array.from(tabBox.querySelectorAll('[data-tab]')).map((b) => (b.textContent || '').trim());
  out.tabs.ids = Array.from(tabBox.querySelectorAll('[data-tab]')).map((b) => b.getAttribute('data-tab'));
  out.tabs.defaultActive = activeTabOf(tabBox);
  // 默认 Tab 渲染出来的是**卡片网格**（不是行）：#installed-grid 的 class 与卡片数。
  out.tabs.gridClass = tabBox.querySelector('#installed-grid')?.className || '';
  out.tabs.gridCards = tabBox.querySelectorAll('#installed-grid > div').length;
  tabBox.remove();

  // ---- ①a 应用市场：**一个列表**，同一时刻既有已安装也有未安装 ----
  const appsBox = document.createElement('div');
  root.appendChild(appsBox);
  AppsView(appsBox, { tab: 'market' });
  await settle();
  await settle(); // 服务状态是后台补的，等它回来再采按钮
  const acards = Array.from(appsBox.querySelectorAll('.grid > div'));
  out.market.cardNames = acards.map(cardName);
  out.market.buttons = {};
  out.market.links = {};
  out.market.text = {};
  for (const c of acards) {
    const name = cardName(c);
    out.market.buttons[name] = btns(c);
    out.market.links[name] = linkList(c);
    out.market.text[name] = (c.textContent || '').trim();
  }
  // 同一时刻两张清单混在一个 Tab 里：已安装的（frpc/IT-Tools 之外的原生）与
  // 未安装的（Ollama/Colima）必须同时出现。
  out.market.installedCards = acards.filter(looksInstalled).map(cardName);
  // 安装状态筛选 / 分类筛选两个下拉都必须**不存在**（用户："始终显示全部"）。
  out.market.hasInstallFilter = !!appsBox.querySelector('#apps-install-filter');
  out.market.hasKindFilter = !!appsBox.querySelector('#apps-kind-filter');
  out.market.sectionTitles = sectionTitles(appsBox);
  const headBox = appsBox.querySelector('#apps-head');
  out.market.headButtons = headBox
    ? Array.from(headBox.querySelectorAll('button, a.btn')).map((b) => (b.textContent || '').trim()).filter(Boolean)
    : null;
  // 点开每个应用卡片上的「⚙️ 管理」，逐个采面板快照（与「已安装」卡片那次对照）。
  for (const c of acards) {
    const name = cardName(c);
    const manage = Array.from(c.querySelectorAll('button')).find((b) => b.textContent.includes('管理'));
    if (!manage) continue;
    manage.click();
    await settle(); await settle();
    out.panelFromMarket[name] = panelSnapshot();
    closeModal();
  }
  appsBox.remove();

  // ---- ①b 网站管理：一键 LNMP 入口的新位置 ----
  //  空站点列表 → 中间的大按钮；有站点 → 工具条上的次级按钮。
  const sitesBox = document.createElement('div');
  root.appendChild(sitesBox);
  SitesView(sitesBox, {});
  await settle(); await settle();
  out.sites.emptyButtons = btns(sitesBox);
  // 说明文案必须如实写清"装什么 + 多久 + 长任务"。
  const lnmpBtn = Array.from(sitesBox.querySelectorAll('button')).find((b) => (b.textContent || '').includes('LNMP'));
  out.sites.hint = lnmpBtn ? (lnmpBtn.getAttribute('title') || '') : '';
  sitesBox.remove();

  window.__sitesPayload = {
    list: [{ domain: 'demo.test', root: '/Users/zizdog/www/demo.test', conf_exists: true, enabled: true, php_version: '8.2', rewrite: 'generic', ssl_enabled: false }],
    presets: [{ name: 'generic', label: '通用', description: '通用规则' }],
    php_versions: [{ version: '8.2', running: true, listen_ok: true, pass: '/tmp/php82.sock', is_default: true }],
    www_root: '/Users/zizdog/www',
  };
  const sitesBox2 = document.createElement('div');
  root.appendChild(sitesBox2);
  SitesView(sitesBox2, {});
  await settle(); await settle();
  out.sites.listButtons = btns(sitesBox2);
  // 留一个引用（**不能放进 out**：DOM 节点无法从 page.evaluate 序列化回来）：
  // 最后点击它，证明点下去走的是既有异步接口
  // （POST /api/v1/market/install-lnmp → 202 + task_id），而不是同步请求。
  //
  // 注意：这里**先不 remove()** —— 节点必须还在文档里，点击才会被浏览器派发，
  // 也才可能打开弹窗。③b 用完再 remove（以前是先 remove 再在最后点击，
  // 那一下点的是脱离文档的节点，什么都不会发生）。
  lnmpListBtn = Array.from(sitesBox2.querySelectorAll('button'))
    .find((b) => (b.textContent || '').includes('LNMP')) || null;
  sitesBox2.dataset.keep = '1';

  // ---- ② 已安装 Tab：合并去重后**一张卡片一个应用** ----
  const svcBox = document.createElement('div');
  root.appendChild(svcBox);
  AppsView(svcBox, { tab: 'installed' });
  await settle();
  await settle();
  out.installed.activeTab = activeTabOf(svcBox);
  out.installed.gridClass = svcBox.querySelector('#installed-grid')?.className || '';
  const mcards = Array.from(svcBox.querySelectorAll('#installed-grid > div'));
  // 卡片网格的证据：卡片数 = 网格直接子元素数 = [data-app-key] 总数（没有额外的行清单）。
  out.installed.gridChildren = svcBox.querySelectorAll('#installed-grid > div').length;
  out.installed.keysTotal = svcBox.querySelectorAll('[data-app-key]').length;
  out.installed.keys = mcards.map((r) => r.getAttribute('data-app-key'));
  out.installed.names = mcards.map((r) => r.getAttribute('data-app-name'));
  out.installed.toolbarButtons = btns(svcBox.querySelector('#installed-toolbar'));
  out.installed.buttons = {};
  out.installed.links = {};
  out.installed.text = {};
  for (const r of mcards) {
    const name = r.getAttribute('data-app-name');
    out.installed.buttons[name] = btns(r);
    out.installed.links[name] = linkList(r);
    out.installed.text[name] = (r.textContent || '').trim();
  }
  // 去重的直接证据：同一个 key 只能出现一次；php82 只能有一张卡片。
  out.installed.keyCounts = out.installed.keys.reduce((m, k) => { m[k] = (m[k] || 0) + 1; return m; }, {});
  out.installed.php82Cards = mcards.filter((r) => (r.textContent || '').includes('PHP 8.2')).length;
  // 「只显示打开、不显示直链」：任何一张已安装卡片都不许出现「直链」。
  out.installed.anyDirect = mcards.some((r) => btns(r).some((t) => t.includes('直链')));
  out.installed.withOpen = mcards.filter((r) => btns(r).some((t) => t === '打开')).map((r) => r.getAttribute('data-app-name'));
  // 从每一张卡片的「⚙️ 管理」点开面板，采快照（与市场卡片那次对照）。
  for (const r of mcards) {
    const name = r.getAttribute('data-app-name');
    const manage = Array.from(r.querySelectorAll('button')).find((b) => b.textContent.includes('管理'));
    if (!manage) continue;
    manage.click();
    await settle(); await settle();
    out.panelFromService[name] = panelSnapshot();
    closeModal();
  }
  svcBox.remove();

  // ---- ②f 筛选合并（2026-09-21）：4 项、且「需要处理」不漏卡 ----
  //  用户在 2026-09-21 指出「异常」与「仅健康检查失败」重复（后者是前者的子集），
  //  而且这两个名字对用户都没有意义。现在只剩 4 项，其中「需要处理」是唯一的
  //  "有问题"入口。这里独立按**旧定义**重算期望值（不复用被测代码的判据），
  //  断言：原本能被「异常」或「仅健康检查失败」命中的卡片，一张都不少。
  const filterBox = document.createElement('div');
  root.appendChild(filterBox);
  AppsView(filterBox, { tab: 'installed' });
  await settle(); await settle();
  const keyOfCard = (el) => el.getAttribute('data-app-key');
  const toolbarBtns = () => Array.from(filterBox.querySelectorAll('#installed-toolbar button'))
    .map((b) => ({ el: b, text: (b.textContent || '').trim() }));
  out.filter = {
    labels: toolbarBtns().map((b) => b.text),
    expectedKeys: (window.__servicesFixture.list || []).filter((s) => {
      const st = s.state || {};
      const h = s.health || {};
      return st.status === 'error' || st.status === 'unavailable' || (h.checked && !h.ok);
    }).map((s) => appKeyOf(s)),
    problems: [],
    countText: (toolbarBtns().find((b) => b.text.includes('需要处理') && b.text.includes('⚠')) || {}).text || '',
  };
  const problemBtn = toolbarBtns().find((b) => b.text === '需要处理');
  if (problemBtn) {
    problemBtn.el.click();
    await settle();
    out.filter.problems = Array.from(filterBox.querySelectorAll('#installed-grid > div')).map(keyOfCard);
  }
  // 复位成「全部」：stateFilter 是模块级变量，不复位会污染后面几次渲染
  const allBtn = toolbarBtns().find((b) => b.text === '全部');
  if (allBtn) { allBtn.el.click(); await settle(); }
  out.filter.afterReset = filterBox.querySelectorAll('#installed-grid > div').length;
  filterBox.remove();

  // ---- ②b docker Tab：纯展示，**没有任何安装动作** ----
  const dockerBox = document.createElement('div');
  root.appendChild(dockerBox);
  AppsView(dockerBox, { tab: 'docker' });
  await settle(); await settle();
  out.docker.activeTab = activeTabOf(dockerBox);
  // 醒目提示必须包含"预配置的 docker compose 文件"（口径见 apps.js 的 dockerCallout）。
  out.docker.callout = (dockerBox.querySelector('#docker-callout')?.textContent || '').trim();
  out.docker.calloutLinks = Array.from((dockerBox.querySelector('#docker-callout') || document.createElement('div')).querySelectorAll('a'))
    .map((a) => (a.textContent || '').trim() + ' → ' + (a.getAttribute('href') || ''));
  const dcards = Array.from(dockerBox.querySelectorAll('#docker-grid > div'));
  out.docker.names = dcards.map(cardName);
  out.docker.buttons = {};
  out.docker.links = {};
  out.docker.text = {};
  for (const c of dcards) {
    const name = cardName(c);
    out.docker.buttons[name] = btns(c);
    out.docker.links[name] = linkList(c);
    out.docker.text[name] = (c.textContent || '').trim();
  }
  out.docker.headButtons = btns(dockerBox.querySelector('#docker-head'));
  dockerBox.remove();

  // ---- ②c 一键建站 Tab：site_app 类条目（typecho / wordpress / freshrss） ----
  const siteTabBox = document.createElement('div');
  root.appendChild(siteTabBox);
  AppsView(siteTabBox, { tab: 'sites' });
  await settle(); await settle();
  out.sites.tabActive = activeTabOf(siteTabBox);
  const stcards = Array.from(siteTabBox.querySelectorAll('#sites-grid > div'));
  out.sites.cards = stcards.map(cardName);
  out.sites.buttons = {};
  for (const c of stcards) out.sites.buttons[cardName(c)] = btns(c);
  siteTabBox.remove();

  // ---- ②d hash → Tab：`#/services` 落「已安装」，`#/apps/<tab>` 直接指定 ----
  out.route.services = routeFor('#/services');
  out.route.apps = routeFor('#/apps');
  out.route.docker = routeFor('#/apps/docker');
  out.route.sites = routeFor('#/apps/sites');
  out.route.dashboard = routeFor('');
  const aliasBox = document.createElement('div');
  root.appendChild(aliasBox);
  AppsView(aliasBox, { tab: out.route.services.tab });
  await settle(); await settle();
  out.route.activeTab = activeTabOf(aliasBox);
  out.route.cards = aliasBox.querySelectorAll('#installed-grid > div').length;
  aliasBox.remove();
  const dockerAliasBox = document.createElement('div');
  root.appendChild(dockerAliasBox);
  AppsView(dockerAliasBox, { tab: out.route.docker.tab });
  await settle(); await settle();
  out.route.dockerActiveTab = activeTabOf(dockerAliasBox);
  dockerAliasBox.remove();

  // ---- ②e 归一化 key 本身（直接测函数，不依赖 fixture 的渲染布局）----
  out.keys = {
    phpDot: appKeyOf({ launch_label: 'homebrew.mxcl.php@8.2' }),
    phpDash: appKeyOf({ launch_label: 'sh.brew.php8-2' }),
    phpName: appKeyOf({ name: 'php82' }),
    phpPlain: appKeyOf({ name: 'php8.2' }),
    stirlingMarket: appKeyOf({ id: 'stirling-pdf', name: 'Stirling PDF' }),
    stirlingSvc: appKeyOf({ name: 'stirling-pdf' }),
    frpcMarket: appKeyOf({ service_label: 'com.zizdog.frpc' }),
    frpcSvc: appKeyOf({ launch_label: 'com.zizdog.frpc' }),
    cnfjSvc: appKeyOf({ name: 'com-zizdog-frpc' }),
  };

  // ---- ③ 超时兜底：服务查询永不返回，面板必须在硬超时后落定 ----
  const slow = {
    id: 'slow-app', name: 'Slow App（超时兜底验证）', icon: '🐢', category: 'tool', kind: 'native',
    summary: '假应用', port: 0, installed: true, adopted: true, available: true,
    service_label: 'com.zizdog.slowapp',
  };
  const t0 = Date.now();
  await openServicePanel({ market: slow });
  out.timeout.elapsedMs = Date.now() - t0;
  out.timeout.panel = panelSnapshot();
  closeModal();

  // ---- ③b 点一下 LNMP 按钮：必须先弹"选版本"的弹窗，**不**立刻发安装请求 ----
  //
  //  用户 2026-09-19 的要求：「点按钮 → 弹窗让用户选 nginx / PHP / MySQL 版本
  //  → 确认后才创建任务」。所以这里的断言顺序是：
  //    ① 点按钮后**没有** install-lnmp 请求，而出现了弹窗、里面能选版本；
  //    ② 选 PHP 8.4 再点「开始安装」→ 才发出请求，且 body 里是 php@8.4；
  //    ③ 请求仍是异步接口（后端回 202 + task_id），进度交给任务中心。
  out.sites.modalAfterClick = null;
  out.sites.php84Selected = false;
  out.sites.installBody = null;
  out.sites.lnmpPostCall = null;
  if (lnmpListBtn) {
    lnmpListBtn.click();
    await settle(); await settle();
    const mask = document.querySelector('.modal-mask');
    out.sites.modalAfterClick = mask ? (mask.innerText || '').slice(0, 800) : null;
    // 点按钮之后**绝不允许**已经发出安装请求
    out.sites.postCallBeforeConfirm = window.__calls.find((c) => c.includes('/market/install-lnmp')) || null;
    // 选 PHP 8.4（弹窗里第二组的第一个 radio）
    const php84 = mask && Array.from(mask.querySelectorAll('input[type=radio]'))
      .find((r) => r.value === 'php@8.4');
    if (php84) {
      php84.checked = true;
      php84.dispatchEvent(new Event('change', { bubbles: true }));
      out.sites.php84Selected = true;
      await settle();
    }
    // 只有「开始安装」这一颗按钮会提交
    const ok = mask && Array.from(mask.querySelectorAll('button'))
      .find((b) => (b.textContent || '').includes('开始安装'));
    out.sites.installButtonLabel = ok ? (ok.textContent || '').trim() : null;
    if (ok) {
      ok.click();
      await settle(); await settle();
    }
  }
  out.sites.lnmpPostCall = window.__calls.find((c) => c.includes('/market/install-lnmp')) || null;
  out.sites.installBody = Object.entries(window.__bodies)
    .filter(([k]) => k.includes('/market/install-lnmp')).map(([, v]) => v)[0] || null;
  // 关掉任务进度弹窗，避免影响后面的采集
  closeModal();
  if (sitesBox2 && sitesBox2.parentNode) sitesBox2.remove();

  out.calls = window.__calls.slice();
  return out;
});

server.close();
await browser.close();

// ---------- 4. 打印 + 断言 ----------
const show = (arr) => (arr || []).join('  |  ') || '（无）';
const checks = [];
const check = (label, cond, extra = '') => checks.push({ label, ok: !!cond, extra });

// 市场里用户点名要覆盖的应用：slug → 卡片标题。
// docker 推荐项目（it-tools / uptime-kuma / n8n）已搬去 docker Tab，不在这里。
const WANT = [
  ['qwen3tts', 'Qwen3 TTS（语音合成）', true],
  ['voicereceiver', 'TtsVoice 音色接收端', true],
  ['frpc', 'frpc（frp 客户端）', true],
  ['orbien-client', 'Orbien 客户端（CLI）', true],
  ['ffmpeg', 'FFmpeg（音视频工具）', true],
  ['stirling-pdf', 'Stirling PDF', true],
  ['ollama', 'Ollama', false],
];
const infoOf = (name) => {
  const b = result.market.buttons[name] || [];
  return {
    buttons: b,
    primary: b[0] || '',
    viewService: b.some((t) => t.includes('查看服务')),
    manageCount: b.filter((t) => t.includes('管理')).length,
    refresh: b.some((t) => t.includes('刷新')),
    // 2026-09-17：重装 / 文档 / 卸载**不再**摆卡片上，它们收进「⚙️ 管理」面板。
    // 2026-09-21：「取消纳管」这个词从界面上删掉了，收尾按钮叫「从列表移除（…）」。
    reinstall: b.some((t) => t.includes('重装')),
    docs: b.some((t) => t === '文档'),
    uninstall: b.some((t) => t.includes('卸载') || t.includes('从列表移除') || t.includes('删除残留')),
  };
};
// 面板里的按钮（从市场卡片的「⚙️ 管理」点开时采到的快照）。
const panelOf = (name) => (result.panelFromMarket[name] || {}).buttons || [];

console.log('══════════ ① 四个一级 Tab + 卡片网格 ══════════');
console.log(`  Tab（顺序）：${show(result.tabs.labels)}`);
console.log(`  Tab（data-tab）：${show(result.tabs.ids)}  默认选中=${result.tabs.defaultActive}`);
console.log(`  已安装网格 class：${result.tabs.gridClass}  卡片数=${result.tabs.gridCards}`);

console.log('\n══════════ ①a 应用市场：一个列表（同时含已安装与未安装）══════════');
for (const [n, b] of Object.entries(result.market.buttons)) console.log(`  ${n}\n      ${show(b)}`);
console.log(`  已安装的卡片：${show(result.market.installedCards)}`);
console.log(`  还有安装筛选下拉？ ${result.market.hasInstallFilter ? '有 ✗' : '没有 ✓'}；分类筛选下拉？ ${result.market.hasKindFilter ? '有 ✗' : '没有 ✓'}`);
console.log(`  板块顺序：${show(result.market.sectionTitles)}`);
console.log(`  市场顶部按钮：${show(result.market.headButtons)}`);

console.log('\n══════════ ② 已安装 Tab：去重后的卡片网格 ══════════');
for (const [n, b] of Object.entries(result.installed.buttons)) console.log(`  ${n}\n      ${show(b)}`);
console.log(`  key：${show(result.installed.keys)}`);
console.log(`  php82 卡片数：${result.installed.php82Cards}（应为 1）`);
console.log(`  每个 key 出现次数：${JSON.stringify(result.installed.keyCounts)}`);
console.log(`  卡片网格 class：${result.installed.gridClass}；网格里的卡片=${result.installed.gridChildren}，[data-app-key] 总数=${result.installed.keysTotal}`);
console.log(`  PHP 8.2 (FPM) 卡片链接：${show(result.installed.links['PHP 8.2 (FPM)'])}`);
console.log(`  Uptime Kuma 卡片链接：${show(result.installed.links['Uptime Kuma'])}`);
console.log(`  Uptime Kuma 卡片文本：${(result.installed.text['Uptime Kuma'] || '').slice(0, 260)}`);
console.log(`  有「打开」的卡片：${show(result.installed.withOpen)}`);
console.log(`  任何卡片出现过「直链」？ ${result.installed.anyDirect ? '是 ✗' : '否 ✓'}`);
console.log(`  已安装工具条：${show(result.installed.toolbarButtons)}`);
console.log(`  筛选按钮：${show(result.filter.labels)}`);
console.log(`  「需要处理」按钮文案：${result.filter.countText}`);
console.log(`  筛选期望（按旧定义独立重算）：${show(result.filter.expectedKeys)}`);
console.log(`  「需要处理」实际筛出：${show(result.filter.problems)}（复位后卡片数 ${result.filter.afterReset}）`);

console.log('\n══════════ ②b docker Tab：纯展示，没有安装动作 ══════════');
console.log(`  激活 Tab=${result.docker.activeTab}`);
console.log(`  醒目提示：${(result.docker.callout || '').slice(0, 200)}`);
for (const [n, b] of Object.entries(result.docker.buttons)) console.log(`  ${n}\n      按钮 ${show(b)}\n      链接 ${show(result.docker.links[n])}`);
console.log(`  docker 顶部按钮：${show(result.docker.headButtons)}`);
console.log(`  n8n 卡片文本：${(result.docker.text['n8n（推荐 compose 项目）'] || '').slice(0, 220)}`);

console.log('\n══════════ ②c 一键建站 Tab ══════════');
console.log(`  激活 Tab=${result.sites.tabActive}  卡片：${show(result.sites.cards)}`);
for (const [n, b] of Object.entries(result.sites.buttons)) console.log(`  ${n}\n      ${show(b)}`);

console.log('\n══════════ ③ 覆盖矩阵（用户点名的应用）══════════');
console.log('  卡片标题'.padEnd(26) + '首按钮'.padEnd(12) + '卡片重装?  面板重装?  卡片文档?');
for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  const panelReinstall = panelOf(name).some((t) => t.includes('重装'));
  console.log(`  ${name.padEnd(24)}${i.primary.padEnd(12)}${(i.reinstall ? '有✗' : '无✓').padEnd(10)}${(panelReinstall ? '有✓' : '无✗').padEnd(12)}${i.docs ? '有✗' : '无✓'}  (${slug})`);
}

console.log('\n══════════ ④ ffmpeg 那条 bug 的证明 ══════════');
const ff = infoOf('FFmpeg（音视频工具）');
const ffPanel = result.panelFromMarket['FFmpeg（音视频工具）'] || {};
console.log(`  卡片按钮：${show(ff.buttons)}`);
console.log(`  面板状态行：${ffPanel.status || '（没打开）'}`);
console.log(`  面板按钮：${show(ffPanel.buttons)}`);
console.log(`  面板里出现「纳管 / 面板里没有服务记录」？ ${/纳管|面板里没有服务记录|未在服务管理里/.test(ffPanel.panelText || '') ? '是 ✗' : '否 ✓'}`);
console.log(`  面板里出现「读取中」？        ${(ffPanel.panelText || '').includes('读取中') ? '是 ✗' : '否 ✓'}`);
const ffLookups = result.calls.filter((c) => /\/services\/ffmpeg(\?|$)/.test(c));
console.log(`  面板是否查过不存在的服务记录？ ${ffLookups.length ? '查了 ✗ ' + ffLookups.join(', ') : '没查 ✓（no_daemon 直接走终态）'}`);

console.log('\n══════════ ⑤ 超时兜底：请求永不返回也必须落定 ══════════');
const to = result.timeout.panel || {};
console.log(`  耗时：${result.timeout.elapsedMs} ms（硬上限 10000ms）`);
console.log(`  状态行：${to.status || '（空）'}`);
console.log(`  仍停在「读取中/正在读取」？ ${/读取中|正在读取/.test(to.panelText || '') ? '是 ✗' : '否 ✓'}`);

console.log('\n══════════ ⑥ 同一个面板的证明 ══════════');
const svcFrpc = Object.entries(result.panelFromService).find(([k]) => k.includes('frpc'))?.[1];
const mktFrpc = result.panelFromMarket['frpc（frp 客户端）'];
const same = JSON.stringify(svcFrpc?.buttons) === JSON.stringify(mktFrpc?.buttons)
  && (svcFrpc?.title === mktFrpc?.title);
console.log(`  frpc：「应用市场 → ⚙️ 管理」与「已安装 → ⚙️ 管理」`);
console.log(`    标题  ${mktFrpc?.title}  ⇄  ${svcFrpc?.title}`);
console.log(`    市场按钮  ${show(mktFrpc?.buttons)}`);
console.log(`    已安装按钮  ${show(svcFrpc?.buttons)}`);
console.log(`    结论  ${same ? '✓ 完全一致（同一个面板组件 servicePanel.openServicePanel）' : '✗ 不一致'}`);

console.log('\n=== 后端收到的请求（用于确认面板自己补查了服务记录）===');
for (const c of result.calls) console.log('  ' + c);

console.log('\n══════════ ⑦ hash → Tab / LNMP 入口 / 板块改名 ══════════');
console.log(`  routeFor('#/services')   = ${JSON.stringify(result.route.services)}`);
console.log(`  routeFor('#/apps')       = ${JSON.stringify(result.route.apps)}`);
console.log(`  routeFor('#/apps/docker')= ${JSON.stringify(result.route.docker)}`);
console.log(`  routeFor('#/apps/sites') = ${JSON.stringify(result.route.sites)}`);
console.log(`  #/services 渲染后的激活 Tab：${result.route.activeTab}（卡片数 ${result.route.cards}）`);
console.log(`  #/apps/docker 渲染后的激活 Tab：${result.route.dockerActiveTab}`);
console.log(`  网站管理（无站点）按钮：${show(result.sites.emptyButtons)}`);
console.log(`  网站管理（有站点）按钮：${show(result.sites.listButtons)}`);
console.log(`  LNMP 按钮 title：${result.sites.hint || '（空）'}`);
console.log(`  LNMP 点击发出的请求：${result.sites.lnmpPostCall || '（无）'}`);
console.log(`  归一化 key：${JSON.stringify(result.keys)}`);
console.log(`  Qwen3 TTS 面板链接：${show((result.panelFromService['Qwen3 TTS（语音合成）']?.links || []).map((l) => l.text + ' → ' + l.href))}`);
console.log(`  Qwen3 TTS 面板直链 title：${(result.panelFromService['Qwen3 TTS（语音合成）']?.links || []).find((l) => l.text === '直链')?.title || '（无）'}`);

// ---------- 断言 ----------
// 用户 2026-09-17 第六条（逐字提示）—— 断言直接比对这一句，改文案必须同步改断言。
const EXACT_SUBPATH_HINT = '该应用不支持子路径，请用端口访问，或自行配置反代。';

for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  if (installed) {
    check(`${name}：卡片上不再有「重装」（收进 ⚙️ 管理）`, !i.reinstall, show(i.buttons));
    check(`${name}：卡片上不再有「文档」（收进 ⚙️ 管理）`, !i.docs, show(i.buttons));
    check(`${name}：卡片上不再有「卸载 / 从列表移除」（收进 ⚙️ 管理）`, !i.uninstall, show(i.buttons));
    check(`${name}：卡片上有「⚙️ 管理」`, i.manageCount === 1, `实际 ${i.manageCount} 个`);
    check(`${name}：管理面板里有「重装」`, panelOf(name).some((t) => t.includes('重装')), show(panelOf(name)));
  } else {
    check(`${name}：未安装 → 主按钮「安装」`, i.primary === '安装', `实际「${i.primary}」`);
    check(`${name}：未安装 → 没有「管理」`, i.manageCount === 0, `实际 ${i.manageCount} 个`);
  }
}
// 有服务可管的已安装应用，刷新按钮文案必须是「⟳ 刷新」
for (const name of ['Qwen3 TTS（语音合成）', 'TtsVoice 音色接收端', 'frpc（frp 客户端）', 'Orbien 客户端（CLI）']) {
  const i = infoOf(name);
  check(`${name}：刷新按钮文案是「⟳ 刷新」`, i.refresh && !i.buttons.some((t) => t.includes('刷新状态')), show(i.buttons));
}

// ---------- ① 四个一级 Tab：存在、默认「已安装」、hash 能指定 ----------
check('四个一级 Tab 的文案与顺序是「已安装 / 应用市场 / docker / 一键建站」',
  JSON.stringify(result.tabs.labels) === JSON.stringify(['已安装', '应用市场', 'docker', '一键建站']),
  show(result.tabs.labels));
check('四个 Tab 有稳定的 data-tab（installed / market / docker / sites）',
  JSON.stringify(result.tabs.ids) === JSON.stringify(['installed', 'market', 'docker', 'sites']),
  show(result.tabs.ids));
check('默认选中的是「已安装」（data-tab=installed）',
  result.tabs.defaultActive === 'installed', String(result.tabs.defaultActive));
check('#/services 落到「已安装」（routeFor + 渲染后的激活 Tab）',
  result.route.services.id === 'apps' && result.route.services.tab === 'installed'
  && result.route.activeTab === 'installed',
  `${JSON.stringify(result.route.services)} active=${result.route.activeTab}`);
check('#/apps 不带 Tab（由 AppsView 落回「已安装」）',
  result.route.apps.id === 'apps' && result.route.apps.tab === '', JSON.stringify(result.route.apps));
check('#/apps/docker 与 #/apps/sites 能直接指定页内 Tab',
  result.route.docker.id === 'apps' && result.route.docker.tab === 'docker'
  && result.route.sites.id === 'apps' && result.route.sites.tab === 'sites',
  `${JSON.stringify(result.route.docker)} ${JSON.stringify(result.route.sites)}`);
check('#/apps/docker 渲染后的激活 Tab 是 docker',
  result.route.dockerActiveTab === 'docker', String(result.route.dockerActiveTab));

// ---------- ② 卡片网格（不是行） ----------
check('已安装 Tab 渲染的是 .grid.grid-3 卡片网格',
  /(^|\s)grid(\s|$)/.test(result.installed.gridClass) && /(^|\s)grid-3(\s|$)/.test(result.installed.gridClass),
  result.installed.gridClass);
check('已安装 Tab 的每个 [data-app-key] 就是网格里的一张卡片（没有额外的行清单）',
  result.installed.gridChildren > 0 && result.installed.gridChildren === result.installed.keysTotal,
  `grid=${result.installed.gridChildren} keys=${result.installed.keysTotal}`);
check('默认 Tab（已安装）也是卡片网格（不是行）',
  /grid-3/.test(result.tabs.gridClass) && result.tabs.gridCards > 0,
  `${result.tabs.gridClass} cards=${result.tabs.gridCards}`);
check('应用市场的卡片挂在 .grid 下', (result.market.cardNames || []).length > 0, show(result.market.cardNames));
check('docker 的卡片挂在 .grid 下', (result.docker.names || []).length > 0, show(result.docker.names));
check('一键建站的卡片挂在 .grid 下', (result.sites.cards || []).length > 0, show(result.sites.cards));

// ---------- ③ 已安装卡片只有「打开」、没有「直链」 ----------
check('已安装：任何卡片都没有「直链」按钮（用户第六条）', !result.installed.anyDirect,
  show(Object.entries(result.installed.buttons).filter(([, b]) => b.some((t) => t.includes('直链')))));
check('已安装：至少有一张卡片给了「打开」', (result.installed.withOpen || []).length > 0, show(result.installed.withOpen));
const phpBtns = result.installed.buttons['PHP 8.2 (FPM)'] || [];
check('已安装：php82 卡片是「打开 + 启停/重启/刷新/管理」（没有直链）',
  JSON.stringify(phpBtns) === JSON.stringify(['打开', '停止', '重启', '⟳ 刷新', '⚙️ 管理']),
  show(phpBtns));
const phpLinks = result.installed.links['PHP 8.2 (FPM)'] || [];
check('已安装：php82 支持子路径 → 「打开」是 /php82/',
  phpLinks.includes('打开 → /php82/'), show(phpLinks));
const stirlingBtns = result.market.buttons['Stirling PDF'] || [];
check('应用市场：已安装卡片也只有「打开」（没有直链）',
  stirlingBtns.includes('打开') && !stirlingBtns.includes('直链'), show(stirlingBtns));

// ---------- ④ 不支持子路径 → 逐字提示 + 端口地址 ----------
const kumaCard = result.installed.text['Uptime Kuma'] || '';
check('已安装：Uptime Kuma（prefer_direct）卡片上有那句逐字提示',
  kumaCard.includes(EXACT_SUBPATH_HINT), kumaCard.slice(0, 300));
const kumaLinks = result.installed.links['Uptime Kuma'] || [];
check('已安装：Uptime Kuma 不支持子路径 → 「打开」指向 port_url 的端口',
  kumaLinks.includes('打开 → http://192.168.1.4:3001/'), show(kumaLinks));
const stirlingText = result.installed.text['Stirling PDF'] || '';
check('已安装：没有面板界面但有端口（Stirling PDF）也走端口打开 + 逐字提示',
  stirlingText.includes(EXACT_SUBPATH_HINT)
  && (result.installed.links['Stirling PDF'] || []).includes('打开 → http://127.0.0.1:8080/'),
  `${show(result.installed.links['Stirling PDF'])} | ${stirlingText.slice(0, 200)}`);
check('已安装：支持子路径的卡片**不**显示那句提示（php82）',
  !(result.installed.text['PHP 8.2 (FPM)'] || '').includes(EXACT_SUBPATH_HINT),
  (result.installed.text['PHP 8.2 (FPM)'] || '').slice(0, 240));
// 管理面板里仍然保留「打开 + 直链」两颗（上一轮用户明确要的，不能删）。
// 面板里的「打开」沿用 openDirectActions 的固定语义：仍是子路径 /uptime-kuma/
// （prefer_direct 只加 ⚠️ 警示），「直链」给端口 —— 卡片上那颗「打开」才改走端口。
const kumaPanelLinks = result.panelFromService['Uptime Kuma']?.links || [];
check('管理面板：Uptime Kuma 仍然有「打开 + 直链」两颗（直链没被删掉）',
  kumaPanelLinks.some((l) => l.text.includes('打开') && l.href === '/uptime-kuma/')
  && kumaPanelLinks.some((l) => l.text === '直链' && l.href === 'http://192.168.1.4:3001/'),
  show(kumaPanelLinks.map((l) => l.text + ' → ' + l.href)));
check('管理面板里同样显示"不支持子路径"小字',
  (result.panelFromService['Uptime Kuma']?.panelText || '').includes('不支持子路径'),
  (result.panelFromService['Uptime Kuma']?.panelText || '').slice(0, 240));
check('已安装：IT-Tools 支持子路径 → 「打开」是 /it-tools/',
  (result.installed.links['IT-Tools（开发者工具箱）'] || []).includes('打开 → /it-tools/'),
  show(result.installed.links['IT-Tools（开发者工具箱）']));

// console_only（frpc 的自带控制台）两个入口都不给。
const frpcBtns = infoOf('frpc（frp 客户端）').buttons;
check('frpc（console_only）：不给「打开 / 直链」',
  !frpcBtns.some((t) => t.includes('打开') || t.includes('直链')), show(frpcBtns));
// 文档：已安装应用在管理面板里，未安装应用留在卡片上（否则没有入口）。
check('Stirling PDF（已安装）：卡片上没有「文档」，管理面板里有',
  !infoOf('Stirling PDF').docs && panelOf('Stirling PDF').includes('文档'),
  `card=${show(infoOf('Stirling PDF').buttons)} panel=${show(panelOf('Stirling PDF'))}`);
check('Ollama（未安装）：卡片上保留「文档」（没有管理入口可去）',
  infoOf('Ollama').docs, show(infoOf('Ollama').buttons));

// ---------- ⑤ 应用市场：一个列表，同时有已安装与未安装 ----------
const mktNames = result.market.cardNames || [];
check('应用市场同时列出已安装（frpc）与未安装（Ollama）的条目',
  mktNames.includes('frpc（frp 客户端）') && mktNames.includes('Ollama'), show(mktNames));
check('应用市场里确实有「已安装」pill 的卡片（不是只显示未安装）',
  (result.market.installedCards || []).includes('frpc（frp 客户端）'), show(result.market.installedCards));
check('应用市场没有安装状态筛选 / 分类筛选两个下拉（始终显示全部）',
  !result.market.hasInstallFilter && !result.market.hasKindFilter,
  `install=${result.market.hasInstallFilter} kind=${result.market.hasKindFilter}`);
check('未安装的 Ollama 卡片是「安装 + 文档」',
  JSON.stringify(result.market.buttons['Ollama']) === JSON.stringify(['安装', '文档']),
  show(result.market.buttons['Ollama']));
check('docker 推荐项目不出现在应用市场（it-tools / Uptime Kuma / n8n）',
  !mktNames.includes('IT-Tools（开发者工具箱）') && !mktNames.includes('Uptime Kuma')
  && !mktNames.includes('n8n（推荐 compose 项目）'),
  show(mktNames));
check('站点应用不出现在应用市场（typecho / wordpress）',
  !mktNames.includes('Typecho') && !mktNames.includes('WordPress'), show(mktNames));
check('Docker 运行时（Colima）留在应用市场（它是运行时，要能安装，不是推荐项目）',
  mktNames.includes('Docker 运行时（Colima）'), show(mktNames));
check('市场板块顺序（站点/docker 空板块被跳过）',
  JSON.stringify(result.market.sectionTitles) === JSON.stringify(['AI 服务', '运维工具', '基础环境']),
  show(result.market.sectionTitles));
check('市场最后一个板块仍是「基础环境」，且不再出现旧名「其它」',
  result.market.sectionTitles.at(-1) === '基础环境' && !result.market.sectionTitles.includes('其它'),
  show(result.market.sectionTitles));
check('市场顶部不再有「一键 LNMP」入口',
  !(result.market.headButtons || []).some((t) => t.includes('LNMP')), show(result.market.headButtons));

// ---------- ⑥ docker Tab：纯展示、没有安装动作、有醒目提示 ----------
check('docker Tab 有「预配置的 docker compose 文件」的醒目提示（含随机密钥说明）',
  (result.docker.callout || '').includes('预配置的 docker compose 文件')
  && (result.docker.callout || '').includes('随机密钥'),
  (result.docker.callout || '').slice(0, 200));
const dockerBad = /安装|部署|启停|停止|启动|重启|卸载|删除|管理/;
const dockerOffenders = Object.entries(result.docker.buttons).filter(([, b]) => b.some((t) => dockerBad.test(t)));
check('docker Tab 的卡片上没有任何安装/部署/启停/卸载按钮',
  dockerOffenders.length === 0, JSON.stringify(dockerOffenders));
check('docker Tab 顶部也没有安装动作（只有刷新）',
  JSON.stringify(result.docker.headButtons) === JSON.stringify(['⟳ 刷新']),
  show(result.docker.headButtons));
check('docker Tab 列出接口标记的推荐项目（it-tools / Uptime Kuma / n8n）',
  ['IT-Tools（开发者工具箱）', 'Uptime Kuma', 'n8n（推荐 compose 项目）'].every((n) => (result.docker.names || []).includes(n)),
  show(result.docker.names));
check('docker 卡片给出默认端口（it-tools 8083）',
  (result.docker.text['IT-Tools（开发者工具箱）'] || '').includes('默认端口 :8083'),
  (result.docker.text['IT-Tools（开发者工具箱）'] || '').slice(0, 200));
check('docker 卡片给出需要的镜像（接口给的 images 优先）',
  (result.docker.text['IT-Tools（开发者工具箱）'] || '').includes('ghcr.io/corentinth/it-tools:latest'),
  (result.docker.text['IT-Tools（开发者工具箱）'] || '').slice(0, 240));
check('docker 卡片：有 compose 内容 → 「复制 compose 配置」',
  (result.docker.buttons['Uptime Kuma'] || []).includes('复制 compose 配置'),
  show(result.docker.buttons['Uptime Kuma']));
check('docker 卡片：有 compose 地址 → 「打开 compose 文件」指向接口给的 URL',
  (result.docker.links['IT-Tools（开发者工具箱）'] || [])
    .includes('打开 compose 文件 → http://192.168.1.8:8090/compose/it-tools/docker-compose.yml'),
  show(result.docker.links['IT-Tools（开发者工具箱）']));
check('docker 卡片：有 compose_env_url → 「变量样例文件 ↗」（.env.example）',
  (result.docker.links['IT-Tools（开发者工具箱）'] || [])
    .includes('变量样例文件 ↗ → http://192.168.1.8:8090/compose/it-tools/.env.example'),
  show(result.docker.links['IT-Tools（开发者工具箱）']));
check('docker 顶部提示给出全部推荐项目的总索引（compose_readme_url）',
  (result.docker.calloutLinks || [])
    .includes('查看全部推荐项目的 compose 说明 ↗ → http://192.168.1.8:8090/compose/README.md'),
  show(result.docker.calloutLinks));
check('docker 卡片：接口没给 compose 内容/地址时留出位置并如实说明（n8n）',
  (result.docker.text['n8n（推荐 compose 项目）'] || '').includes('接口还没有提供这个项目的 compose 文件内容/地址'),
  (result.docker.text['n8n（推荐 compose 项目）'] || '').slice(0, 240));

// ---------- ⑦ 一键建站 Tab ----------
check('一键建站 Tab 列出 site_app 条目（Typecho / WordPress）',
  (result.sites.cards || []).includes('Typecho') && (result.sites.cards || []).includes('WordPress'),
  show(result.sites.cards));
check('一键建站卡片给「一键建站」按钮（WordPress 还有「文档」），没有「安装」',
  JSON.stringify(result.sites.buttons['Typecho']) === JSON.stringify(['一键建站'])
  && JSON.stringify(result.sites.buttons['WordPress']) === JSON.stringify(['一键建站', '文档']),
  `typecho=${show(result.sites.buttons['Typecho'])} wordpress=${show(result.sites.buttons['WordPress'])}`);
check('一键建站 Tab 渲染后的激活 Tab 是 sites',
  result.sites.tabActive === 'sites', String(result.sites.tabActive));

// ---------- ⑧ 去重 + 同一个面板 ----------
check('已安装：php82 只有一张卡片（两条服务记录 + 市场条目合并成一条）',
  result.installed.php82Cards === 1, `实际 ${result.installed.php82Cards} 张`);
check('已安装：每个归一化 key 只出现一次',
  Object.values(result.installed.keyCounts || {}).every((n) => n === 1),
  JSON.stringify(result.installed.keyCounts));
check('已安装：php82 卡片状态取"在跑的那条记录"（运行中）',
  (result.installed.text['PHP 8.2 (FPM)'] || '').includes('运行中'),
  (result.installed.text['PHP 8.2 (FPM)'] || '').slice(0, 140));
check('已安装：php82 卡片如实标出被合并掉的记录条数',
  (result.installed.text['PHP 8.2 (FPM)'] || '').includes('已合并 1 条记录'),
  (result.installed.text['PHP 8.2 (FPM)'] || '').slice(0, 240));
check('去重：被丢弃记录的字段（plist 路径）并进了保留的那条',
  (result.panelFromService['PHP 8.2 (FPM)']?.panelText || '').includes('homebrew.mxcl.php@8.2.plist'),
  (result.panelFromService['PHP 8.2 (FPM)']?.panelText || '').slice(0, 400));
check('已安装工具条：不再有「🔍 扫描可纳管服务」，但保留了「+ 注册服务」（能力没丢，只换了说法）',
  !(result.installed.toolbarButtons || []).some((t) => t.includes('扫描') || t.includes('纳管'))
  && (result.installed.toolbarButtons || []).includes('+ 注册服务'),
  show(result.installed.toolbarButtons));

// ---------- ⑧b 筛选合并：4 项、且一个问题卡片都不许漏 ----------
check('状态筛选只剩 4 项（全部 / 运行中 / 已停止 / 需要处理）',
  ['全部', '运行中', '已停止', '需要处理'].every((n) => (result.filter.labels || []).includes(n)),
  show(result.filter.labels));
check('筛选里不再有「异常」与「仅健康检查失败」（用户说这两个重复且无意义）',
  !(result.filter.labels || []).some((t) => t.includes('异常') || t.includes('仅健康检查失败')),
  show(result.filter.labels));
check('工具条那颗按钮的文案是「⚠ N 个需要处理」（不再写"健康检查失败"这个半术语）',
  /^⚠\s*\d+\s*个需要处理$/.test(result.filter.countText || ''), result.filter.countText);
check('「需要处理」把旧「异常」（启动失败 / 运行时不可用）的卡片全部筛出来',
  ['uitestsvcerror', 'uitestsvcunavail'].every((k) => (result.filter.problems || []).includes(k)),
  `期望含 error/unavailable，实际 ${show(result.filter.problems)}`);
check('「需要处理」把旧「仅健康检查失败」的卡片也筛出来（合并不许藏起问题）',
  (result.filter.problems || []).includes('uitestsvchealth'),
  `实际 ${show(result.filter.problems)}`);
check('「需要处理」筛出的**正好**是旧判据命中的那些卡片（不多不漏）',
  JSON.stringify([...(result.filter.problems || [])].sort()) === JSON.stringify([...(result.filter.expectedKeys || [])].sort()),
  `期望 ${show(result.filter.expectedKeys)} 实际 ${show(result.filter.problems)}`);
check('点「全部」能复位（筛选状态不粘住）',
  result.filter.afterReset > (result.filter.problems || []).length,
  `复位后 ${result.filter.afterReset}，筛选时 ${(result.filter.problems || []).length}`);
check('本机已有但面板没记录的服务：市场卡片主按钮是「添加到面板」（不是"纳管/接入/安装"）',
  JSON.stringify(result.market.buttons['Ollama（本机已有）'] || []) === JSON.stringify(['添加到面板']),
  show(result.market.buttons['Ollama（本机已有）']));
check('Nginx（本机已有的服务）：收尾按钮是「从列表移除（不卸载软件）」，不再说"取消纳管"',
  panelOf('Nginx').includes('从列表移除（不卸载软件）') && !panelOf('Nginx').some((t) => t.includes('纳管')),
  show(panelOf('Nginx')));

// ---------- ⑧c 用户可见处一个内部词都不许留（2026-09-21） ----------
// 卡片、面板文本、工具条一起查。只查"纳管 / 面板托管 / 仅纳管 / 接入管理"，
// 不查"托管"两个字：应用摘要里有"自托管服务监控"这种正常说法。
const INTERNAL_WORD = /纳管|面板托管|仅纳管|接入管理/;
const internalOffenders = [];
for (const [where, text] of [
  ...Object.entries(result.installed.text || {}).map(([n, t]) => ['已安装卡片 ' + n, t]),
  ...Object.entries(result.market.text || {}).map(([n, t]) => ['市场卡片 ' + n, t]),
  ...Object.entries(result.panelFromMarket || {}).map(([n, p]) => ['市场面板 ' + n, (p || {}).panelText || '']),
  ...Object.entries(result.panelFromService || {}).map(([n, p]) => ['服务面板 ' + n, (p || {}).panelText || '']),
  ['已安装工具条', (result.installed.toolbarButtons || []).join(' ')],
]) {
  if (INTERNAL_WORD.test(text || '')) internalOffenders.push(where);
}
check('全部用户可见文本里不再出现「纳管 / 面板托管 / 仅纳管 / 接入管理」',
  internalOffenders.length === 0, show(internalOffenders));
check('市场 / 已安装打开的是同一个面板', same);

// ---------- ⑨ ffmpeg / 超时 / 归一化 / 管理面板 ----------
check('ffmpeg 面板不显示「纳管 / 面板里没有服务记录」（它是 CLI 工具，不是"没纳管"）',
  !/纳管|面板里没有服务记录|未在服务管理里/.test(ffPanel.panelText || ''));
check('ffmpeg 面板不显示「读取中」', !(ffPanel.panelText || '').includes('读取中'));
check('ffmpeg 面板状态是「命令行工具（无常驻进程）」', (ffPanel.status || '').includes('命令行工具（无常驻进程）'), ffPanel.status);
check('ffmpeg 不查不存在的服务记录', ffLookups.length === 0, ffLookups.join(', '));
check('超时兜底：面板一定落定（无"读取中"）', !/读取中|正在读取/.test(to.panelText || ''), to.status);
check('超时兜底：在硬上限内落定', result.timeout.elapsedMs < 12000, result.timeout.elapsedMs + 'ms');
check('归一化：php@8.2 / php8-2 / php82 / php8.2 收敛成同一个 key（=php82）',
  new Set([result.keys.phpDot, result.keys.phpDash, result.keys.phpName, result.keys.phpPlain]).size === 1
  && result.keys.phpDot === 'php82',
  JSON.stringify(result.keys));
check('归一化：市场展示名（Stirling PDF）与服务记录名（stirling-pdf）同 key',
  result.keys.stirlingMarket === result.keys.stirlingSvc && result.keys.stirlingMarket === 'stirlingpdf',
  JSON.stringify(result.keys));
check('归一化：com.zizdog.* 前缀的不同写法同 key',
  result.keys.frpcMarket === result.keys.frpcSvc && result.keys.cnfjSvc === result.keys.frpcSvc,
  JSON.stringify(result.keys));
const qwenPanel = result.panelFromService['Qwen3 TTS（语音合成）'] || {};
const qwenDirect = (qwenPanel.links || []).find((l) => l.text === '直链');
check('管理面板：只有面板记录、没有市场条目的服务（无 port_url）有端口 → 给出拼接的「直链」，title 说明局限',
  !!qwenDirect && qwenDirect.href === 'http://127.0.0.1:8880/' && /拼出来/.test(qwenDirect.title),
  JSON.stringify(qwenDirect));
const orbienPanel = result.panelFromService['Orbien 客户端（CLI）'] || {};
check('管理面板：既没有界面也没有端口 → 不给打开按钮，只给一行原因',
  (orbienPanel.panelText || '').includes('没有可用的打开入口')
  && !(orbienPanel.buttons || []).some((t) => t === '打开' || t === '直链'),
  `buttons=${show(orbienPanel.buttons)} text=${(orbienPanel.panelText || '').slice(0, 120)}`);

// 网站管理页的 LNMP 入口：空站点给大按钮，有站点给工具条次级按钮。
check('网站管理（空站点）：有「一键 LNMP」入口',
  (result.sites.emptyButtons || []).some((t) => t.includes('LNMP')), show(result.sites.emptyButtons));
check('网站管理（有站点）：工具条上仍有「一键 LNMP」',
  (result.sites.listButtons || []).some((t) => t.includes('LNMP')), show(result.sites.listButtons));
check('LNMP 按钮文案说清装什么（nginx / PHP / MySQL / phpMyAdmin）',
  ['nginx', 'PHP', 'MySQL', 'phpMyAdmin'].every((w) => result.sites.hint.includes(w)), result.sites.hint);
check('LNMP 按钮文案提示耗时与长任务特性（分钟级 + 不中断/任务中心）',
  /分钟/.test(result.sites.hint) && /(中断|任务中心)/.test(result.sites.hint), result.sites.hint);
// ---- 一键 LNMP 的"先弹窗选版本、确认后才安装"（2026-09-19 用户要求）----
check('点「一键 LNMP」先弹出选版本弹窗（不是直接开装）',
  !!result.sites.modalAfterClick && /选择要安装的版本/.test(result.sites.modalAfterClick),
  String(result.sites.modalAfterClick || '').slice(0, 200));
check('点按钮后、确认之前**没有**发出安装请求',
  !result.sites.postCallBeforeConfirm, String(result.sites.postCallBeforeConfirm || ''));
check('弹窗里能选到 PHP 8.4（候选来自目录，不是写死）',
  result.sites.php84Selected, String(result.sites.installButtonLabel || ''));
check('弹窗里看得到"已安装"标记与 MySQL root 口令说明',
  /已安装/.test(result.sites.modalAfterClick || '')
  && /root 口令/.test(result.sites.modalAfterClick || ''),
  String(result.sites.modalAfterClick || '').slice(0, 300));
check('确认后才走既有异步接口 POST /market/install-lnmp（不是同步请求）',
  /^POST .*\/market\/install-lnmp/.test(result.sites.lnmpPostCall || ''),
  String(result.sites.lnmpPostCall));
check('安装请求体里是用户选的 php@8.4（不是默认的 8.2）',
  /"php":"php@8\.4"/.test(result.sites.installBody || ''),
  String(result.sites.installBody || ''));

console.log('\n══════════ 断言结果 ══════════');
let failed = 0;
for (const c of checks) {
  if (!c.ok) failed++;
  console.log(`  ${c.ok ? '✓' : '✗'} ${c.label}${c.ok ? '' : '  —— ' + c.extra}`);
}
console.log(`\n  ${checks.length - failed}/${checks.length} 通过`);
if (failed) process.exitCode = 1;
