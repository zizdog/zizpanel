// appdetail-verify.mjs —— 端到端验证「应用管理面板」在市场与服务管理两处是同一个，
// 以及卡片按钮清单完全由数据决定（用户 2026-09-16 那一轮需求的证据脚本）。
//
// 为什么这么搭：
//   · 真 acorn（tools/check-js-syntax.mjs）只能证明语法合法，证明不了"点开是同一个面板"，
//     所以这里用 Playwright 加载**真实的** apps.js / services.js / servicePanel.js；
//   · app.js 是应用外壳（会 boot、拉会话、渲染登录页），在这里只会捣乱，
//     所以用 importmap 把它换成一个最小替身（只提供 state / panelPath /
//     registerCleanup）—— **被验证的三个文件本身一个字符都没有改**；
//   · 后端用假 fetch：要验的是"按钮由数据决定"，所以给几个代表性应用就够。
//
// 这一轮（截止 0.11.0 的显示问题）要证明的四件事：
//   ① 已安装应用的主按钮是「重装」，不再有「查看服务」；
//   ② 卡片上通往管理面板的入口只有**一个**「⚙️ 管理」（「详情」与「查看服务」合并）；
//   ③ 服务状态刷新按钮的文案是「⟳ 刷新」（原来是「刷新状态」）；
//   ④ ffmpeg（no_daemon 命令行工具）不再显示"读取中…/未在服务管理里"，
//      而且面板**根本不去查**不存在的服务记录；即使请求永不返回也有硬超时终态。
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
      // prefer_direct），「打开」必须给 /it-tools/、「直链」给端口 8083 ——
      // 不能因为 /market/proxies 的探测（不带面板会话，子路径 401）翻过来。
      id: 'it-tools', name: 'IT-Tools（开发者工具箱）', icon: '🧰', category: 'tool', kind: 'compose',
      summary: '几十个开发者常用小工具，纯前端', description: '开发者小工具合集。',
      port: 8083, installed: true, adopted: true, available: true,
      service_label: 'it-tools', service_in_launchd: false, port_url: 'http://192.168.1.4:8083/',
      compose_yaml: 'services:\n  it-tools:\n    image: ghcr.io/corentinth/it-tools:latest\n',
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
      ui: { slug: 'uptime-kuma', prefer_direct: true, note: 'Uptime Kuma 官方不支持子路径' },
      uninstall: { kind: 'service', service: 'uptime-kuma', steps: ['docker compose down（删除容器与网络）'], keep_note: 'compose 应用只删容器与网络，**具名卷（数据）保留**' },
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
      id: 'nginx', name: 'Nginx', icon: '🌐', category: 'lnmp', kind: 'native',
      summary: 'Web 服务器', description: 'brew 装的 nginx。', port: 8080,
      installed: true, adopted: true, available: true,
      service_label: 'homebrew.mxcl.nginx', service_in_launchd: true,
      uninstall: { kind: 'forget', service: 'nginx', steps: ['从「服务管理」中删除这条记录'] },
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
      // 一键建站类（category: site）：用来证明市场第一个板块「一键建站」确实渲染出来。
      id: 'typecho', name: 'Typecho', icon: '📝', category: 'site', kind: 'native',
      summary: '轻量博客程序', description: '一键装好并配好伪静态。', port: 0,
      installed: false, adopted: false, available: true,
      site_app: { rewrite: 'typecho', finish_path: '/install.php', needs_db: true, notes: [] },
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
  // 网站管理页的假数据：默认"一个站点都没有"（要验空站点列表时的大按钮），
  // 测试里把它换成"有一个站点"再渲染一次（要验工具条上的次级按钮）。
  window.__sitesPayload = {
    list: [], presets: [], php_versions: [], www_root: '/Users/zizdog/www',
  };
  window.fetch = async (url, init = {}) => {
    const u = String(url);
    const method = (init.method || 'GET').toUpperCase();
    window.__calls.push(method + ' ' + u);
    const done = (data, ok = true, status = 200) => ({
      ok, status, statusText: ok ? 'OK' : 'ERR', text: async () => JSON.stringify({ ok, data }),
    });
    // 超时验证：这条查询**永远不返回**（模拟后端假死/连接被挂住）。
    if (u.includes('/services/com.zizdog.slowapp')) return new Promise(() => {});
    if (u.includes('/session')) return done({ user: { username: 'admin' }, config: { panel_entry: '/' } });
    if (u.includes('/market/proxies')) return done({ enabled: true, items: [] });
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

// ---------- 3. 渲染两个页面，采集按钮清单 ----------
const result = await page.evaluate(async () => {
  const root = document.getElementById('root');
  // 先清掉上一轮/上次运行留下的筛选记忆，保证"默认是全部"这一条可测。
  try { localStorage.removeItem('zp-market-kind-filter'); } catch { /* 无 localStorage 也没关系 */ }
  const { AppsView } = await import('./apps.js');
  const { SitesView } = await import('./sites.js');
  const { ServicesView } = await import('./services.js');
  const { openServicePanel } = await import('./servicePanel.js');

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
    // scope 里的节点是 clone 出来临时挂的，用完必须摘掉：
    // 否则它们会留在文档里，被下一次 document.querySelectorAll 采到，
    // 于是"服务管理点开的面板"实际上一直采的是上一次市场的面板（第一版就栽在这）。
    scope.remove();
    return { title, status: statusLine.slice(0, 160), panelText: panelText.slice(0, 400), buttons };
  };
  const closeModal = () => { const m = document.querySelector('.modal-mask'); if (m) m.remove(); };
  const settle = () => new Promise((r) => setTimeout(r, 120));

  const out = { market: {}, sites: {}, service: {}, panelFromMarket: {}, panelFromService: {}, timeout: {} };
  // 网站管理页 LNMP 按钮的引用（见 ①c / ①e）；DOM 节点不进 out。
  let lnmpListBtn = null;

  // 采集市场板块标题（顺序有意义：兜底板块「基础环境」必须在最后）。
  const sectionTitles = (box) => Array.from(box.querySelectorAll('.section-title'))
    .map((e) => (e.textContent || '').trim()).filter(Boolean);
  const cardNamesNow = (box) => Array.from(box.querySelectorAll('.grid > div')).map(cardName);

  // ---- ① 应用市场 ----
  const appsBox = document.createElement('div');
  root.appendChild(appsBox);
  AppsView(appsBox, {});
  await settle();
  await settle(); // 服务状态是后台补的，等它回来再采按钮
  const acards = Array.from(appsBox.querySelectorAll('.grid > div'));
  out.market.cardNames = acards.map(cardName);
  out.market.buttons = {};
  for (const c of acards) out.market.buttons[cardName(c)] = btns(c);
  // 「打开 / 直链」的 href 也要采：文字相同但地址错了照样是 bug
  // （用户 2026-09-17 报的"it-tools 反了"就是地址被探测翻过来了）。
  out.market.links = {};
  for (const c of acards) {
    out.market.links[cardName(c)] = Array.from(c.querySelectorAll('a.btn'))
      .map((a) => (a.textContent || '').trim() + ' → ' + (a.getAttribute('href') || ''));
  }
  // 板块标题与头部按钮：用来验「其它」已改名「基础环境」（且在最后），
  // 以及市场顶部**不再有**「一键 LNMP」入口（它搬到了网站管理）。
  out.market.sectionTitles = sectionTitles(appsBox);
  const headBox = appsBox.querySelector('#apps-head');
  out.market.headButtons = headBox
    ? Array.from(headBox.querySelectorAll('button, a.btn')).map((b) => (b.textContent || '').trim()).filter(Boolean)
    : null;
  // 筛选下拉：默认「全部」，三个选项。
  const filter = appsBox.querySelector('#apps-head select');
  out.market.filterOptions = filter ? Array.from(filter.options).map((o) => o.textContent) : null;
  out.market.filterDefault = filter ? filter.value : null;
  // 点开每个应用卡片上的「⚙️ 管理」（面板按钮已改名），逐个采面板快照。
  for (const c of acards) {
    const name = cardName(c);
    const manage = Array.from(c.querySelectorAll('button')).find((b) => b.textContent.includes('管理'));
    if (!manage) continue;
    manage.click();
    await settle(); await settle();
    out.panelFromMarket[name] = panelSnapshot();
    closeModal();
  }

  // ---- ①b 筛选：只看原生 / 只看 Docker（纯前端过滤，按 Kind） ----
  if (filter) {
    filter.value = 'native';
    filter.dispatchEvent(new Event('change'));
    await settle();
    out.market.nativeCards = cardNamesNow(appsBox);
    out.market.nativeSections = sectionTitles(appsBox);

    filter.value = 'docker';
    filter.dispatchEvent(new Event('change'));
    await settle();
    out.market.dockerCards = cardNamesNow(appsBox);
    out.market.dockerSections = sectionTitles(appsBox);
    // 选择要能保留：重新渲染一个全新的 AppsView（模拟刷新/切页面回来），
    // 仍然停在 Docker 档（走 localStorage）；降级路径不会抛异常。
    out.market.savedFilter = (() => { try { return localStorage.getItem('zp-market-kind-filter'); } catch { return null; } })();

    const appsBox2 = document.createElement('div');
    root.appendChild(appsBox2);
    AppsView(appsBox2, {});
    await settle(); await settle();
    const filter2 = appsBox2.querySelector('#apps-head select');
    out.market.persistedFilter = filter2 ? filter2.value : null;
    out.market.persistedCards = cardNamesNow(appsBox2);
    // 复原成「全部」，别让后续采集受筛选影响。
    if (filter2) { filter2.value = ''; filter2.dispatchEvent(new Event('change')); }
    await settle();
    appsBox2.remove();
  }
  appsBox.remove();

  // ---- ①c 网站管理：一键 LNMP 入口的新位置 ----
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
  lnmpListBtn = Array.from(sitesBox2.querySelectorAll('button'))
    .find((b) => (b.textContent || '').includes('LNMP')) || null;
  sitesBox2.remove();

  // ---- ② 服务管理 ----
  const svcBox = document.createElement('div');
  root.appendChild(svcBox);
  ServicesView(svcBox, {});
  await settle();
  const scards = Array.from(svcBox.querySelectorAll('.grid > div'));
  out.service.cardNames = scards.map(cardName);
  out.service.buttons = {};
  for (const c of scards) out.service.buttons[cardName(c)] = btns(c);
  for (const c of scards) {
    const name = cardName(c);
    const manage = Array.from(c.querySelectorAll('button')).find((b) => b.textContent.includes('管理'));
    if (!manage) continue;
    manage.click();
    await settle(); await settle();
    out.panelFromService[name] = panelSnapshot();
    closeModal();
  }
  svcBox.remove();

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

  // ---- ①e 点一下 LNMP 按钮：必须调用既有的异步接口 ----
  //  用户明确要求"不要改成同步请求"：后端立刻返回 task_id（202），
  //  进度交给任务中心。这里断言请求真的发到了 /market/install-lnmp 且是 POST。
  if (lnmpListBtn) {
    lnmpListBtn.click();
    await settle();
  }
  out.sites.lnmpPostCall = window.__calls.find((c) => c.includes('/market/install-lnmp')) || null;

  out.calls = window.__calls.slice();
  return out;
});

server.close();
await browser.close();

// ---------- 4. 打印 + 断言 ----------
const show = (arr) => (arr || []).join('  |  ') || '（无）';
const checks = [];
const check = (label, cond, extra = '') => checks.push({ label, ok: !!cond, extra });

// 用户点名要覆盖的应用：slug → 卡片标题
const WANT = [
  ['qwen3tts', 'Qwen3 TTS（语音合成）', true],
  ['voicereceiver', 'TtsVoice 音色接收端', true],
  ['frpc', 'frpc（frp 客户端）', true],
  ['orbien-client', 'Orbien 客户端（CLI）', true],
  ['ffmpeg', 'FFmpeg（音视频工具）', true],
  ['uptime-kuma', 'Uptime Kuma', true],
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
    reinstall: b.some((t) => t.includes('重装')),
    docs: b.some((t) => t === '文档'),
    uninstall: b.some((t) => t.includes('卸载') || t.includes('取消纳管') || t.includes('删除残留')),
  };
};
// 面板里的按钮（从市场卡片的「⚙️ 管理」点开时采到的快照）。
const panelOf = (name) => (result.panelFromMarket[name] || {}).buttons || [];

console.log('══════════ ① 应用市场：卡片上的按钮 ══════════');
for (const [n, b] of Object.entries(result.market.buttons)) console.log(`  ${n}\n      ${show(b)}`);
console.log('\n══════════ ② 服务管理：卡片上的按钮 ══════════');
for (const [n, b] of Object.entries(result.service.buttons)) console.log(`  ${n}\n      ${show(b)}`);

console.log('\n══════════ ③ 覆盖矩阵（用户点名的应用）══════════');
console.log('  卡片标题'.padEnd(26) + '首按钮'.padEnd(12) + '卡片重装?  面板重装?  卡片文档?');
for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  const panelReinstall = panelOf(name).some((t) => t.includes('重装'));
  console.log(`  ${name.padEnd(24)}${i.primary.padEnd(12)}${(i.reinstall ? '有✗' : '无✓').padEnd(10)}${(panelReinstall ? '有✓' : '无✗').padEnd(12)}${i.docs ? '有✗' : '无✓'}  (${slug})`);
}

console.log('\n══════════ ③b 打开 / 直链：语义固定（用户 2026-09-17 的"it-tools 反了"）══════════');
console.log(`  IT-Tools 卡片链接   ${show(result.market.links['IT-Tools（开发者工具箱）'])}`);
console.log(`  Uptime Kuma 卡片链接 ${show(result.market.links['Uptime Kuma'])}`);
console.log(`  服务管理 Uptime Kuma ${show(result.service.buttons['Uptime Kuma'])}`);

console.log('\n══════════ ④ ffmpeg 那条 bug 的证明 ══════════');
const ff = infoOf('FFmpeg（音视频工具）');
const ffPanel = result.panelFromMarket['FFmpeg（音视频工具）'] || {};
console.log(`  卡片按钮：${show(ff.buttons)}`);
console.log(`  面板状态行：${ffPanel.status || '（没打开）'}`);
console.log(`  面板按钮：${show(ffPanel.buttons)}`);
console.log(`  面板里出现「未在服务管理里」？ ${(ffPanel.panelText || '').includes('未在服务管理里') ? '是 ✗' : '否 ✓'}`);
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
console.log(`  frpc：「应用市场 → ⚙️ 管理」与「服务管理 → ⚙️ 管理」`);
console.log(`    标题  ${mktFrpc?.title}  ⇄  ${svcFrpc?.title}`);
console.log(`    市场按钮  ${show(mktFrpc?.buttons)}`);
console.log(`    服务按钮  ${show(svcFrpc?.buttons)}`);
console.log(`    结论  ${same ? '✓ 完全一致（同一个面板组件 servicePanel.openServicePanel）' : '✗ 不一致'}`);

console.log('\n=== 后端收到的请求（用于确认面板自己补查了服务记录）===');
for (const c of result.calls) console.log('  ' + c);

console.log('\n══════════ ⑦ 三处界面调整（LNMP 入口 / 筛选 / 板块改名）══════════');
console.log(`  市场板块顺序：${show(result.market.sectionTitles)}`);
console.log(`  市场顶部按钮：${show(result.market.headButtons)}`);
console.log(`  筛选选项：${show(result.market.filterOptions)}  默认值="${result.market.filterDefault}"`);
console.log(`  「原生」档卡片：${show(result.market.nativeCards)}`);
console.log(`  「原生」档板块：${show(result.market.nativeSections)}`);
console.log(`  「Docker」档卡片：${show(result.market.dockerCards)}`);
console.log(`  「Docker」档板块：${show(result.market.dockerSections)}`);
console.log(`  重渲染后筛选保留：${result.market.persistedFilter === 'docker' ? 'Docker ✓' : (result.market.persistedFilter || '(空)') + ' ✗'}`);
console.log(`  网站管理（无站点）按钮：${show(result.sites.emptyButtons)}`);
console.log(`  网站管理（有站点）按钮：${show(result.sites.listButtons)}`);
console.log(`  LNMP 按钮 title：${result.sites.hint || '（空）'}`);
console.log(`  LNMP 点击发出的请求：${result.sites.lnmpPostCall || '（无）'}`);

// ---------- 断言 ----------
// 2026-09-17 用户要求：卡片上每个应用只保留「打开 / 直链 / 刷新 / 重启 / 停止 /
// 管理」这一组固定语义动作；「重装 / 卸载 / 文档」全部收进「⚙️ 管理」面板。
for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  if (installed) {
    check(`${name}：卡片上不再有「重装」（收进 ⚙️ 管理）`, !i.reinstall, show(i.buttons));
    check(`${name}：卡片上不再有「文档」（收进 ⚙️ 管理）`, !i.docs, show(i.buttons));
    check(`${name}：卡片上不再有「卸载 / 取消纳管」（收进 ⚙️ 管理）`, !i.uninstall, show(i.buttons));
    check(`${name}：卡片上有「⚙️ 管理」`, i.manageCount === 1, `实际 ${i.manageCount} 个`);
    check(`${name}：管理面板里有「重装」`, panelOf(name).some((t) => t.includes('重装')), show(panelOf(name)));
  } else {
    check(`${name}：未安装 → 主按钮「安装」`, i.primary === '安装', `实际「${i.primary}」`);
    check(`${name}：未安装 → 没有「管理」`, i.manageCount === 0, `实际 ${i.manageCount} 个`);
  }
}
// 有服务可管的已安装应用，刷新按钮文案必须是「⟳ 刷新」
for (const name of ['Qwen3 TTS（语音合成）', 'TtsVoice 音色接收端', 'frpc（frp 客户端）', 'Orbien 客户端（CLI）', 'Uptime Kuma']) {
  const i = infoOf(name);
  check(`${name}：刷新按钮文案是「⟳ 刷新」`, i.refresh && !i.buttons.some((t) => t.includes('刷新状态')), show(i.buttons));
}
// ---------- 打开 / 直链：两颗按钮语义固定，不再按探测结果调换 ----------
// 用户原话："打开用 https://panel.zizdog.com:8888/it-tools/ 这种，直链用
// https://192.168.1.4:3000，it-tools 就反了"。所以：
//   · 打开 = 面板反代子路径 /<slug>/（相对路径），任何应用都一样；
//   · 直链 = port_url（端口直连）；
//   · prefer_direct（人工实测子路径不可用）只在「打开」上加 ⚠️ 与 title，
//     **不**把「打开」换成直连。
const itLinks = result.market.links['IT-Tools（开发者工具箱）'] || [];
check('IT-Tools：「打开」是面板子路径 /it-tools/',
  itLinks.includes('打开 → /it-tools/'), show(itLinks));
check('IT-Tools：「直链」是端口 8083',
  itLinks.includes('直链 → http://192.168.1.4:8083/'), show(itLinks));
const kumaLinks = result.market.links['Uptime Kuma'] || [];
check('Uptime Kuma（prefer_direct）：「打开」仍是面板子路径 /uptime-kuma/，带 ⚠️',
  kumaLinks.includes('⚠️ 打开 → /uptime-kuma/'), show(kumaLinks));
check('Uptime Kuma（prefer_direct）：「直链」是端口 3001',
  kumaLinks.includes('直链 → http://192.168.1.4:3001/'), show(kumaLinks));
check('Uptime Kuma：「打开」不再降级成"试试子路径"',
  !kumaLinks.some((l) => l.includes('试试子路径')), show(kumaLinks));
// 服务管理页用的是**同一份** openDirectActions（市场条目与 port_url 都对上号）。
const kumaSvc = result.service.buttons['Uptime Kuma'] || [];
check('服务管理：Uptime Kuma 也有「⚠️ 打开 + 直链」（同一份实现）',
  kumaSvc.includes('⚠️ 打开') && kumaSvc.includes('直链'), show(kumaSvc));
// console_only（frpc 的自带控制台）两个入口都不给。
const frpcBtns = infoOf('frpc（frp 客户端）').buttons;
check('frpc（console_only）：不给「打开 / 直链」',
  !frpcBtns.some((t) => t.includes('打开') || t.includes('直链')), show(frpcBtns));
// 文档：已安装应用在管理面板里，未安装应用留在卡片上（否则没有入口）。
check('IT-Tools：卡片上没有「文档」，管理面板里有',
  !infoOf('IT-Tools（开发者工具箱）').docs
  && panelOf('IT-Tools（开发者工具箱）').includes('文档'),
  `card=${show(infoOf('IT-Tools（开发者工具箱）').buttons)} panel=${show(panelOf('IT-Tools（开发者工具箱）'))}`);
check('Ollama（未安装）：卡片上保留「文档」（没有管理入口可去）',
  infoOf('Ollama').docs, show(infoOf('Ollama').buttons));
check('ffmpeg 面板不显示「未在服务管理里」', !(ffPanel.panelText || '').includes('未在服务管理里'));
check('ffmpeg 面板不显示「读取中」', !(ffPanel.panelText || '').includes('读取中'));
check('ffmpeg 面板状态是「命令行工具（无常驻进程）」', (ffPanel.status || '').includes('命令行工具（无常驻进程）'), ffPanel.status);
check('ffmpeg 不查不存在的服务记录', ffLookups.length === 0, ffLookups.join(', '));
check('超时兜底：面板一定落定（无"读取中"）', !/读取中|正在读取/.test(to.panelText || ''), to.status);
check('超时兜底：在硬上限内落定', result.timeout.elapsedMs < 12000, result.timeout.elapsedMs + 'ms');
check('市场 / 服务管理打开的是同一个面板', same);

// ---------- 三处界面调整（用户本轮明确要求） ----------
//
// ① 一键 LNMP 入口搬到「网站管理」、市场里移除；
// ② 市场顶部加「全部 / 原生 / Docker」筛选（按 Kind，纯前端，选择保留）；
// ③ 市场最后一个板块「其它」改名「基础环境」（中文名只有后端一处定义）。
const secTitles = result.market.sectionTitles || [];
check('市场最后一个板块是「基础环境」', secTitles[secTitles.length - 1] === '基础环境', show(secTitles));
check('市场不再出现旧板块名「其它」', !secTitles.includes('其它'), show(secTitles));
check('市场板块顺序照后端来（一键建站 → AI 服务 → 运维工具 → 基础环境）',
  JSON.stringify(secTitles) === JSON.stringify(['一键建站', 'AI 服务', '运维工具', '基础环境']), show(secTitles));
check('市场顶部不再有「一键 LNMP」入口',
  !(result.market.headButtons || []).some((t) => t.includes('LNMP')), show(result.market.headButtons));
check('筛选下拉是「全部 / 原生 / Docker」三选一',
  JSON.stringify(result.market.filterOptions) === JSON.stringify(['全部', '原生', 'Docker']),
  show(result.market.filterOptions));
check('筛选默认「全部」', result.market.filterDefault === '', String(result.market.filterDefault));
// 原生档：原生应用在，compose 的 Uptime Kuma 与 KindColima 的 Docker 运行时都不在
check('「原生」档只留原生应用（frpc/Nginx 在，Kuma/Colima 不在）',
  (result.market.nativeCards || []).includes('frpc（frp 客户端）')
  && (result.market.nativeCards || []).includes('Nginx')
  && !(result.market.nativeCards || []).includes('Uptime Kuma')
  && !(result.market.nativeCards || []).includes('Docker 运行时（Colima）'),
  show(result.market.nativeCards));
// Docker 档：compose + KindColima 都在（Colima 归 Docker 档），原生不在
check('「Docker」档含 compose 与 KindColima（Colima 归 Docker 档）',
  (result.market.dockerCards || []).includes('Uptime Kuma')
  && (result.market.dockerCards || []).includes('Docker 运行时（Colima）')
  && !(result.market.dockerCards || []).includes('frpc（frp 客户端）'),
  show(result.market.dockerCards));
check('筛选选择写入 localStorage', result.market.savedFilter === 'docker', String(result.market.savedFilter));
check('重渲染后筛选选择保留（仍停在 Docker 档）',
  result.market.persistedFilter === 'docker'
  && (result.market.persistedCards || []).includes('Uptime Kuma'),
  `value=${result.market.persistedFilter} cards=${show(result.market.persistedCards)}`);

// 网站管理页的 LNMP 入口：空站点给大按钮，有站点给工具条次级按钮。
check('网站管理（空站点）：有「一键 LNMP」入口',
  (result.sites.emptyButtons || []).some((t) => t.includes('LNMP')), show(result.sites.emptyButtons));
check('网站管理（有站点）：工具条上仍有「一键 LNMP」',
  (result.sites.listButtons || []).some((t) => t.includes('LNMP')), show(result.sites.listButtons));
check('LNMP 按钮文案说清装什么（nginx / PHP / MySQL / phpMyAdmin）',
  ['nginx', 'PHP', 'MySQL', 'phpMyAdmin'].every((w) => result.sites.hint.includes(w)), result.sites.hint);
check('LNMP 按钮文案提示耗时与长任务特性（分钟级 + 不中断/任务中心）',
  /分钟/.test(result.sites.hint) && /(中断|任务中心)/.test(result.sites.hint), result.sites.hint);
check('LNMP 按钮走既有异步接口 POST /market/install-lnmp（不是同步请求）',
  /^POST .*\/market\/install-lnmp/.test(result.sites.lnmpPostCall || ''),
  String(result.sites.lnmpPostCall));

console.log('\n══════════ 断言结果 ══════════');
let failed = 0;
for (const c of checks) {
  if (!c.ok) failed++;
  console.log(`  ${c.ok ? '✓' : '✗'} ${c.label}${c.ok ? '' : '  —— ' + c.extra}`);
}
console.log(`\n  ${checks.length - failed}/${checks.length} 通过`);
if (failed) process.exitCode = 1;
