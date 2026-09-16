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
  const { AppsView } = await import('./apps.js');
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

  const out = { market: {}, service: {}, panelFromMarket: {}, panelFromService: {}, timeout: {} };

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
  appsBox.remove();

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
    reinstall: b.some((t) => t.includes('重装')),
  };
};

console.log('══════════ ① 应用市场：卡片上的按钮 ══════════');
for (const [n, b] of Object.entries(result.market.buttons)) console.log(`  ${n}\n      ${show(b)}`);
console.log('\n══════════ ② 服务管理：卡片上的按钮 ══════════');
for (const [n, b] of Object.entries(result.service.buttons)) console.log(`  ${n}\n      ${show(b)}`);

console.log('\n══════════ ③ 覆盖矩阵（用户点名的应用）══════════');
console.log('  卡片标题'.padEnd(26) + '主按钮'.padEnd(8) + '查看服务?  管理数  ⟳刷新?  重装?');
for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  console.log(`  ${name.padEnd(24)}${i.primary.padEnd(8)}${(i.viewService ? '有✗' : '无✓').padEnd(10)}${String(i.manageCount).padEnd(8)}${(i.refresh ? '是' : '否').padEnd(8)}${i.reinstall ? '是' : '否'}  (${slug})`);
}

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

// ---------- 断言 ----------
for (const [slug, name, installed] of WANT) {
  const i = infoOf(name);
  if (installed) {
    check(`${name}：主按钮是「重装」`, i.primary === '重装', `实际「${i.primary}」`);
    check(`${name}：不再有「查看服务」`, !i.viewService);
    check(`${name}：只有一个「⚙️ 管理」`, i.manageCount === 1, `实际 ${i.manageCount} 个`);
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
check('ffmpeg 面板不显示「未在服务管理里」', !(ffPanel.panelText || '').includes('未在服务管理里'));
check('ffmpeg 面板不显示「读取中」', !(ffPanel.panelText || '').includes('读取中'));
check('ffmpeg 面板状态是「命令行工具（无常驻进程）」', (ffPanel.status || '').includes('命令行工具（无常驻进程）'), ffPanel.status);
check('ffmpeg 不查不存在的服务记录', ffLookups.length === 0, ffLookups.join(', '));
check('超时兜底：面板一定落定（无"读取中"）', !/读取中|正在读取/.test(to.panelText || ''), to.status);
check('超时兜底：在硬上限内落定', result.timeout.elapsedMs < 12000, result.timeout.elapsedMs + 'ms');
check('市场 / 服务管理打开的是同一个面板', same);

console.log('\n══════════ 断言结果 ══════════');
let failed = 0;
for (const c of checks) {
  if (!c.ok) failed++;
  console.log(`  ${c.ok ? '✓' : '✗'} ${c.label}${c.ok ? '' : '  —— ' + c.extra}`);
}
console.log(`\n  ${checks.length - failed}/${checks.length} 通过`);
if (failed) process.exitCode = 1;
