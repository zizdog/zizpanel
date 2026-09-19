// certs-verify.mjs —— 端到端验证「SSL 证书」页与站点侧的证书选择器。
//
// 为什么这么搭（与 tools/appdetail-verify.mjs 同一套路数）：
//   · tools/check-js-syntax.mjs（真 acorn）只能证明语法合法，证明不了
//     "选 dns-01 真的会按后端给的键名生成输入框""删除真的有确认框"，
//     所以这里用 Playwright 加载**真实的** certs.js / sites.js；
//   · 只替换两个 import 目标，被验证的文件本身一个字符都不改：
//       - certs.js 里的 './tasks.js' → __tasks__（记录 taskCenter.start 的调用，
//         并真的执行其中的 start()，让请求真的打到假 fetch 上）；
//       - sites.js 里的 './app.js'  → __shell__（app.js 是应用外壳，会 boot、拉会话、
//         渲染登录页，在这里只会捣乱；sites.js 只用到 registerCleanup）。
//   · 后端用假 fetch，返回契约里的字段（含 days_left / challenge / ca），
//     顺便故意让 dns-providers 用两种不同形状（env 数组 / fields 对象 / 键值映射），
//     以证明前端的字段渲染是**数据驱动**的，而不是照着某一种 JSON 写死的。
//
// 用法： export PATH=/opt/homebrew/bin:$PATH; node tools/certs-verify.mjs
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
  if (rel === '' || rel === 'index.html') {
    res.writeHead(200, { 'Content-Type': MIME['.html'] });
    res.end('<!doctype html><html><head><meta charset="utf-8">'
      + '<script type="importmap">{"imports":{"__shell__":"/__shell__.js","__tasks__":"/__tasks__.js"}}</script>'
      // #toasts 是 ui.toast 的挂载点（真实外壳由 app.js 建，这里必须补上，
      // 否则任何 toast 一调用就 TypeError，会把脚本整段崩掉）。
      + '</head><body><div id="root"></div><div id="toasts"></div></body></html>');
    return;
  }
  if (rel === '__shell__.js') {
    res.writeHead(200, { 'Content-Type': MIME['.js'] });
    // app.js 的最小替身：sites.js 只用到 registerCleanup（state 在本文件里没被用到）。
    res.end(`
export const state = { session: { user: { username: 'admin' }, config: { panel_entry: '/' } }, metrics: null, route: '' };
export function registerCleanup() { return () => {}; }
`);
    return;
  }
  if (rel === '__tasks__.js') {
    res.writeHead(200, { 'Content-Type': MIME['.js'] });
    // 任务中心的替身：只做两件被验证需要的事 ——
    //   ① 记录 certs.js 传给 taskCenter.start 的参数（kind/target/title）；
    //   ② 真的调用其中的 start()，让 api.certApply 的请求真的发出去。
    // 这样"提交走任务中心"与"提交内容符合契约"是分开独立断言的两件事。
    res.end(`
export const taskCenter = {
  start(opts) {
    window.__taskCalls.push({
      kind: opts && opts.kind, target: opts && opts.target, title: opts && opts.title,
      hasStart: typeof (opts && opts.start) === 'function',
    });
    let p = null;
    try { p = opts.start(); } catch (e) { window.__taskError = String((e && e.message) || e); }
    return Promise.resolve(p).then((r) => r && (r.task_id || null))
      .catch((e) => { window.__taskError = String((e && e.message) || e); return null; });
  },
  findByTarget() { return null; },
  button() { return document.createElement('button'); },
  init() {}, openList() {}, openTask() {}, onChange() { return () => {}; },
};
`);
    return;
  }
  const file = path.join(JS_DIR, rel);
  if (!file.startsWith(JS_DIR) || !fs.existsSync(file)) { res.writeHead(404); res.end('nope'); return; }
  let body = fs.readFileSync(file).toString();
  // 只做 import 目标的替换，业务代码一行不动。
  if (rel === 'certs.js') body = body.replace(/from '\.\/tasks\.js'/g, "from '__tasks__'");
  if (rel === 'sites.js') body = body.replace(/from '\.\/app\.js'/g, "from '__shell__'");
  // 反向代理页的 HTTPS 区块也要在本脚本里被真实加载（它 import 了 certs.js 的
  // 证书匹配纯函数，所以这里必须让它走真实的 certs.js，而不是替身）。
  if (rel === 'reverseproxy.js') body = body.replace(/from '\.\/app\.js'/g, "from '__shell__'");
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
  res.end(body);
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch();
const page = await browser.newPage();
page.on('console', (m) => { if (m.type() === 'error') console.log('  [console]', m.text()); });
page.on('pageerror', (e) => console.log('  [pageerror]', e.message));

// ---------- 2. 假数据：字段与后端契约一一对应 ----------
// 证书列表：故意包含 3 种到期状态（<15 天 / 充裕 / 已过期），
// http-01 与 dns-01 两种验证方式，以及一条 status=failed 的「申请失败、可重试」条目。
const CERTS = [
  {
    primary: 'demo.test', domains: ['demo.test', 'www.demo.test'], issuer: "Let's Encrypt",
    not_after: '2026-10-01T00:00:00Z', days_left: 12, challenge: 'http-01', ca: 'letsencrypt',
    needs_renewal: true, status: 'issued',
    cert_path: '/opt/zizpanel/certs/demo.test/fullchain.pem',
    key_path: '/opt/zizpanel/certs/demo.test/privkey.pem', // 契约里列表只含路径，不含私钥内容
  },
  {
    primary: 'api.test', domains: ['api.test', '*.api.test'], issuer: 'ZeroSSL',
    not_after: '2026-11-18T00:00:00Z', days_left: 60, challenge: 'dns-01', ca: 'zerossl',
    needs_renewal: false, status: 'issued',
    cert_path: '/opt/zizpanel/certs/api.test/fullchain.pem',
    key_path: '/opt/zizpanel/certs/api.test/privkey.pem',
  },
  {
    primary: 'old.test', domains: ['old.test'], issuer: "Let's Encrypt",
    not_after: '2026-09-13T00:00:00Z', days_left: -3, challenge: 'http-01', ca: 'letsencrypt',
    needs_renewal: true, status: 'issued',
  },
  {
    // 失败条目：没有证书/私钥，只有可重试的申请信息（**凭据值绝不出现在这里**）。
    primary: 'fail.test', domains: ['fail.test', 'www.fail.test'], status: 'failed',
    email: 'ops@example.com', challenge: 'dns-01', ca: 'letsencrypt', dns_provider: 'cloudflare',
    failures: 2, last_error: '向 CA 申请证书失败: provider rejected token ***',
    created_at: '2026-09-19T10:00:00Z', updated_at: '2026-09-20T10:00:00Z', last_error_at: '2026-09-20T10:00:00Z',
  },
];

// DNS 服务商清单：故意用三种不同的 JSON 形状，验证前端"按键名动态渲染"不是写死的。
const PROVIDERS = {
  providers: [
    { name: 'cloudflare', label: 'Cloudflare', env: ['CF_Token', 'CF_Account_ID'] },
    {
      name: 'aliyun', label: '阿里云 DNS',
      fields: [
        { key: 'Ali_Key', label: 'AccessKey ID', required: true },
        { key: 'Ali_Secret', label: 'AccessKey Secret', required: true },
      ],
    },
    { name: 'dnspod', label: 'DNSPod', env: { DP_Id: 'DNSPod 账号 ID', DP_Key: 'DNSPod Token' } },
  ],
};

const SITE = (domain) => ({
  domain, aliases: '', root: '/Users/zizdog/www/' + domain, php_version: '', rewrite: '',
  enabled: true, conf_exists: true, ssl_enabled: false, ssl_provider: '', ssl_expires: '',
  proxy_pass: '', extra_conf: '', remark: '',
});
const SITES = [SITE('www.demo.test'), SITE('sub.api.test'), SITE('nope.test')];

// 反向代理规则：一条已启用 HTTPS（用于验证列表锁标志 + 证书域名/到期展示），
// 一条纯 HTTP（用于验证"没开 SSL 就不该出现锁标志"）。
const PROXIES = [
  {
    id: 21, name: 'NAS 镜像站', listen: 8090, domains: 'nas.zizdog.com', path: '',
    target: 'http://mirror.example.com:8090', preserve_host: false, websocket: true,
    enabled: true, remark: '', port_listening: true, target_ok: true,
    target_detail: '可达 mirror.example.com:8090（3ms）',
    ssl_enabled: true,
    ssl_cert: '/opt/zizpanel/certs/nas.zizdog.com/fullchain.pem',
    ssl_key: '/opt/zizpanel/certs/nas.zizdog.com/privkey.pem',
    ssl_provider: 'acme', ssl_expires: '2026-12-01 00:00:00',
    ssl: {
      enabled: true, provider: 'acme', provider_label: "ACME 自动证书（Let's Encrypt 等）",
      cert_path: '/opt/zizpanel/certs/nas.zizdog.com/fullchain.pem',
      key_path: '/opt/zizpanel/certs/nas.zizdog.com/privkey.pem',
      expires: '2026-12-01 00:00:00', not_after: '2026-12-01T00:00:00Z',
      days_left: 72, domains: ['nas.zizdog.com'], renew_hint: '',
    },
  },
  {
    id: 22, name: '内网面板', listen: 8443, domains: '', path: '',
    target: 'https://127.0.0.1:8443', preserve_host: true, websocket: true,
    enabled: true, remark: '', port_listening: false, target_ok: true,
    target_detail: '可达 127.0.0.1:8443（1ms）',
    ssl_enabled: false, ssl_cert: '', ssl_key: '', ssl_provider: '', ssl_expires: '',
    ssl: { enabled: false, provider: '', provider_label: '未配置', domains: [], days_left: -1 },
  },
];

await page.addInitScript(({ CERTS, PROVIDERS, SITES, PROXIES }) => {
  window.__fetches = [];
  window.__taskCalls = [];
  window.__taskError = '';
  window.fetch = async (url, init = {}) => {
    const u = String(url);
    const method = (init.method || 'GET').toUpperCase();
    let p = u;
    try { p = new URL(u, location.href).pathname; } catch { /* 相对路径 */ }
    let body = null;
    if (init.body) { try { body = JSON.parse(init.body); } catch { body = String(init.body); } }
    window.__fetches.push({ method, path: p, body, url: u });
    const done = (data, ok = true, status = 200) => ({
      ok, status, statusText: ok ? 'OK' : 'ERR', text: async () => JSON.stringify({ ok, data }),
    });

    if (p === '/api/v1/certs' && method === 'GET') {
      // __failCerts 用来验证"后端接口还没落地"时页面是错误态而不是白屏。
      if (window.__failCerts) return done(null, false, 500);
      return done({ list: window.__certsEmpty ? [] : CERTS });
    }
    if (p === '/api/v1/certs' && method === 'POST') {
      return done({ task_id: 'task-cert-1', title: '申请证书' });
    }
    if (p === '/api/v1/certs/dns-providers') return done(PROVIDERS);
    let m = p.match(/^\/api\/v1\/certs\/([^/]+)\/renew$/);
    if (m && method === 'POST') return done({ task_id: 'task-renew-1', title: '续期证书' });
    m = p.match(/^\/api\/v1\/certs\/([^/]+)\/retry$/);
    if (m && method === 'POST') return done({ task_id: 'task-retry-1', title: '重试申请证书' });
    m = p.match(/^\/api\/v1\/certs\/([^/]+)$/);
    if (m && method === 'DELETE') return done(null);
    // ---- 反向代理 ----
    if (p === '/api/v1/proxies' && method === 'GET') {
      return done({ list: window.__proxies || PROXIES, nginx: { installed: true, config: '/opt/homebrew/etc/nginx/nginx.conf' } });
    }
    if (p === '/api/v1/proxies' && method === 'POST') {
      // 新建规则：返回 proxyView 形状 —— 前端要靠响应里的 id 继续调 SSL 接口。
      const created = Object.assign({
        port_listening: true, target_ok: true, target_detail: '可达',
        enabled: true, preserve_host: false, websocket: true,
        ssl_enabled: false, ssl_cert: '', ssl_key: '', ssl_provider: '', ssl_expires: '',
        ssl: { enabled: false, provider: '', provider_label: '未配置', domains: [], days_left: -1 },
      }, body || {}, { id: 7 });
      window.__proxies = [created].concat(window.__proxies || PROXIES);
      return done(created);
    }
    let pm = p.match(/^\/api\/v1\/proxies\/(\d+)\/ssl$/);
    if (pm && method === 'POST') {
      return done({
        msg: 'HTTPS 已启用',
        cert: '/opt/zizpanel/certs/demo.test/fullchain.pem',
        key: '/opt/zizpanel/certs/demo.test/privkey.pem',
        expires: '2026-12-01 00:00:00', provider: 'acme',
        provider_label: "ACME 自动证书（Let's Encrypt 等）", days_left: 72,
        ssl: { enabled: true, provider: 'acme', domains: ['demo.test'], days_left: 72 },
      });
    }
    pm = p.match(/^\/api\/v1\/proxies\/(\d+)$/);
    if (pm && method === 'POST') {
      return done({ id: Number(pm[1]), port_listening: true, target_ok: true, ssl_enabled: false, ssl: {} });
    }
    if (p === '/api/v1/sites' && method === 'GET') {
      return done({ list: SITES, php_versions: [], presets: [] });
    }
    m = p.match(/^\/api\/v1\/sites\/([^/]+)$/);
    if (m && method === 'GET') {
      const d = decodeURIComponent(m[1]);
      const s = SITES.find((x) => x.domain === d);
      return s ? done({ site: s, presets: [], php_versions: [] }) : done(null, false, 404);
    }
    if (p === '/api/v1/sites' && method === 'POST') return done(null, false, 404);
    return done(null, false, 404);
  };
}, { CERTS, PROVIDERS, SITES, PROXIES });

await page.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });

// ---------- 3. 在真实页面里跑完整流程 ----------
const result = await page.evaluate(async () => {
  const root = document.getElementById('root');
  const out = {};
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const waitFor = async (fn, ms = 4000) => {
    const t0 = Date.now();
    for (;;) {
      let v = null;
      try { v = fn(); } catch { v = null; }
      if (v) return v;
      if (Date.now() - t0 > ms) return null;
      await sleep(25);
    }
  };
  const pillsOf = (node) => Array.from(node.querySelectorAll('.pill'))
    .map((p) => ({ text: (p.textContent || '').trim(), cls: p.className }));
  const btnByText = (node, text) => Array.from(node.querySelectorAll('button'))
    .find((b) => (b.textContent || '').trim() === text);
  const modals = () => Array.from(document.querySelectorAll('.modal'));
  const closeAllModals = () => { document.querySelectorAll('.modal-mask').forEach((m) => m.remove()); };

  const certs = await import('./certs.js');

  // ================= ① 证书列表 =================
  const box = document.createElement('div');
  root.appendChild(box);
  certs.CertsView(box);
  await waitFor(() => box.querySelectorAll('tbody tr').length >= 3);

  out.rows = Array.from(box.querySelectorAll('tbody tr')).map((tr) => ({
    text: (tr.innerText || '').replace(/\s+/g, ' ').trim(),
    pills: pillsOf(tr),
    actions: Array.from(tr.querySelectorAll('button')).map((b) => (b.textContent || '').trim()),
  }));
  out.headers = Array.from(box.querySelectorAll('thead th')).map((th) => th.textContent.trim());
  out.headerPills = pillsOf(box.querySelector('.card-head'));

  // ================= ② 空状态 =================
  window.__certsEmpty = true;
  const emptyBox = document.createElement('div');
  root.appendChild(emptyBox);
  certs.CertsView(emptyBox);
  await waitFor(() => (emptyBox.innerText || '').includes('还没有证书'));
  out.emptyText = (emptyBox.innerText || '').replace(/\s+/g, ' ').trim();
  out.emptyButtons = Array.from(emptyBox.querySelectorAll('button')).map((b) => (b.textContent || '').trim());
  window.__certsEmpty = false;

  // ================= ③ 申请表单：dns-01 → 服务商下拉 → 按键名动态渲染输入框 =================
  box.querySelector('#zp-cert-apply').click();
  await waitFor(() => modals().length > 0);
  const applyModal = modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '').includes('申请 SSL 证书'));
  const chal = document.getElementById('zp-cert-challenge');
  const provSel = document.getElementById('zp-dns-provider');
  const dnsRow = document.getElementById('zp-dns-row');
  out.dnsRowHiddenBefore = dnsRow.style.display === 'none';
  out.providerCountBefore = provSel.options.length;

  chal.value = 'dns-01';
  chal.dispatchEvent(new Event('change'));
  await waitFor(() => provSel.options.length >= 3);
  out.dnsRowVisibleAfter = dnsRow.style.display !== 'none';
  out.providerOptions = Array.from(provSel.options).map((o) => o.value + ' / ' + o.textContent.trim());
  out.providerHint = (document.querySelector('#zp-dns-row .hint')?.textContent || '').trim();

  const fieldKeys = () => Array.from(document.querySelectorAll('#zp-dns-fields input[data-env-key]'))
    .map((i) => i.getAttribute('data-env-key'));
  const fieldTypes = () => Array.from(document.querySelectorAll('#zp-dns-fields input[data-env-key]'))
    .map((i) => i.type);
  out.keysCloudflare = fieldKeys();
  out.typesCloudflare = fieldTypes();

  provSel.value = 'aliyun';
  provSel.dispatchEvent(new Event('change'));
  out.keysAliyun = fieldKeys();
  provSel.value = 'dnspod';
  provSel.dispatchEvent(new Event('change'));
  out.keysDnspod = fieldKeys();

  // ================= ④ 提交：走 taskCenter + 请求体符合契约 =================
  provSel.value = 'cloudflare';
  provSel.dispatchEvent(new Event('change'));
  await waitFor(() => fieldKeys().includes('CF_Token'));
  const inputs = Array.from(document.querySelectorAll('#zp-dns-fields input[data-env-key]'));
  const typed = { CF_Token: 'cf-token-abc', CF_Account_ID: 'cf-account-123' };
  inputs.forEach((i) => { i.value = typed[i.getAttribute('data-env-key')] || ''; });
  document.getElementById('zp-cert-domains').value = '*.api.test\napi.test';
  document.getElementById('zp-cert-email').value = 'ops@example.com';
  document.getElementById('zp-cert-ca').value = 'letsencrypt-staging';
  document.getElementById('zp-cert-ca').dispatchEvent(new Event('change'));
  out.summaryText = (applyModal.querySelector('.hint:last-of-type')?.textContent || '').trim();
  document.getElementById('zp-cert-submit').click();

  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && f.path === '/api/v1/certs'));
  await waitFor(() => window.__taskCalls.length >= 1);
  out.taskCalls = window.__taskCalls.slice();
  out.postBody = (window.__fetches.find((f) => f.method === 'POST' && f.path === '/api/v1/certs') || {}).body;
  // 等"窗口真的关掉"再继续：不然后面再开一个申请窗会出现重复 id，
  // getElementById 会命中旧窗，后续步骤全乱（第一版验证脚本就栽在这）。
  out.modalClosedAfterSubmit = !!(await waitFor(() => modals().length === 0));

  // ================= ⑤ http-01：不应带 dns 字段；且泛域名被拦下 =================
  box.querySelector('#zp-cert-apply').click();
  await waitFor(() => modals().length > 0);
  document.getElementById('zp-cert-domains').value = 'plain.test';
  document.getElementById('zp-cert-email').value = 'ops@example.com';
  document.getElementById('zp-cert-challenge').value = 'http-01';
  document.getElementById('zp-cert-challenge').dispatchEvent(new Event('change'));
  document.getElementById('zp-cert-submit').click();
  await waitFor(() => window.__fetches.filter((f) => f.method === 'POST' && f.path === '/api/v1/certs').length >= 2);
  out.postBodies = window.__fetches.filter((f) => f.method === 'POST' && f.path === '/api/v1/certs').map((f) => f.body);
  closeAllModals();

  // 泛域名 + http-01 必须在提交前被拦住（不发请求）
  const postedBefore = window.__fetches.filter((f) => f.method === 'POST' && f.path === '/api/v1/certs').length;
  box.querySelector('#zp-cert-apply').click();
  await waitFor(() => modals().length > 0);
  document.getElementById('zp-cert-domains').value = '*.blocked.test';
  document.getElementById('zp-cert-email').value = 'ops@example.com';
  document.getElementById('zp-cert-submit').click();
  await sleep(120);
  out.wildcardBlockedToast = (document.getElementById('toasts')?.innerText || '').replace(/\s+/g, ' ').trim();
  out.wildcardBlockedNoPost = window.__fetches.filter((f) => f.method === 'POST' && f.path === '/api/v1/certs').length === postedBefore;
  closeAllModals();
  document.querySelectorAll('#toasts .toast').forEach((t) => t.remove());

  // ================= ⑥ 续期：同样走任务中心 =================
  const apiRow = Array.from(box.querySelectorAll('tbody tr'))
    .find((tr) => (tr.textContent || '').includes('api.test'));
  btnByText(apiRow, '🔄 续期').click();
  await waitFor(() => window.__taskCalls.some((t) => t.target === 'cert:api.test'));
  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && f.path === '/api/v1/certs/api.test/renew'));
  out.renewCall = window.__taskCalls.find((t) => t.target === 'cert:api.test');
  out.renewPaths = window.__fetches.filter((f) => f.method === 'POST' && /\/renew$/.test(f.path)).map((f) => f.path);

  // ================= ⑦ 删除：必须有确认框（并说明站点会失去 HTTPS） =================
  const demoRow = Array.from(box.querySelectorAll('tbody tr'))
    .find((tr) => (tr.textContent || '').includes('demo.test'));
  btnByText(demoRow, '删除').click();
  const delModal = await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '') === '删除证书'));
  out.deleteConfirmText = (delModal.innerText || '').replace(/\s+/g, ' ').trim();
  out.deleteConfirmButtons = Array.from(delModal.querySelectorAll('.modal-foot button')).map((b) => (b.textContent || '').trim());
  // 先取消：不能发出 DELETE
  btnByText(delModal, '取消').click();
  await sleep(80);
  out.deleteAfterCancel = window.__fetches.filter((f) => f.method === 'DELETE').length;
  // 再确认：必须发出 DELETE /api/v1/certs/demo.test
  btnByText(demoRow, '删除').click();
  const delModal2 = await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '') === '删除证书'));
  btnByText(delModal2, '删除证书').click();
  await waitFor(() => window.__fetches.some((f) => f.method === 'DELETE'));
  out.deleteCalls = window.__fetches.filter((f) => f.method === 'DELETE').map((f) => f.path);

  // ================= ⑦ 失败条目：状态 / 一键重试 / 预填 =================
  const failRow = Array.from(box.querySelectorAll('tbody tr'))
    .find((tr) => (tr.textContent || '').includes('fail.test'));
  out.failRowText = (failRow.innerText || '').replace(/\s+/g, ' ').trim();
  out.failRowPills = pillsOf(failRow);
  out.failRowActions = Array.from(failRow.querySelectorAll('button')).map((b) => (b.textContent || '').trim());
  out.failRowShowsDays = /剩余|已过期/.test(failRow.innerText || '');

  // 一键重试：走任务中心，POST 请求体为空（用户不重填、凭据不经浏览器）。
  btnByText(failRow, '🔁 重试').click();
  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && f.path === '/api/v1/certs/fail.test/retry'));
  out.retryCall = window.__taskCalls.find((t) => t.target === 'cert:fail.test');
  out.retryBody = (window.__fetches.find((f) => f.method === 'POST' && f.path === '/api/v1/certs/fail.test/retry') || {}).body;
  closeAllModals();

  // 修改后重试：对话框预填上次的域名/邮箱/CA/校验方式，并预选 DNS 服务商。
  btnByText(failRow, '✏️ 修改后重试').click();
  await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '').includes('修改后重新申请证书')));
  await waitFor(() => (document.getElementById('zp-dns-provider') || {}).options
    && document.getElementById('zp-dns-provider').options.length >= 3);
  out.prefill = {
    domains: document.getElementById('zp-cert-domains').value,
    email: document.getElementById('zp-cert-email').value,
    ca: document.getElementById('zp-cert-ca').value,
    challenge: document.getElementById('zp-cert-challenge').value,
    provider: document.getElementById('zp-dns-provider').value,
    hint: (document.querySelector('#zp-dns-fields .hint')?.textContent || '').trim(),
  };
  closeAllModals();

  // ================= ⑦ 站点侧：SSL Tab 里按域名预选证书 =================
  const { SitesView } = await import('./sites.js');
  const sbox = document.createElement('div');
  root.appendChild(sbox);
  SitesView(sbox, {});
  await waitFor(() => btnByText(sbox, '管理'));
  const siteRow = Array.from(sbox.querySelectorAll('tbody tr'))
    .find((tr) => (tr.textContent || '').includes('www.demo.test'));
  btnByText(siteRow, '管理').click();
  const detail = await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '').startsWith('站点：')));
  out.detailTitle = (detail.querySelector('.modal-head h3')?.textContent || '').trim();
  out.detailTabs = Array.from(detail.querySelectorAll('.modal > .modal-body button'))
    .map((b) => (b.textContent || '').trim()).filter(Boolean).slice(0, 12);
  btnByText(detail, 'SSL 证书').click();
  const sel = await waitFor(() => detail.querySelector('#zp-acme-cert-select'));
  out.siteSectionText = (detail.innerText || '').replace(/\s+/g, ' ').trim();
  out.siteSelected = sel.value;
  out.siteSelectedLabel = (sel.options[sel.selectedIndex] || {}).textContent || '';
  out.sitePills = pillsOf(detail);
  closeAllModals();

  // ================= ⑧ 通配符匹配 + 无匹配时的「去申请」引导（直接驱动组件） =================
  const oneCase = async (domain) => {
    const w = document.createElement('div');
    root.appendChild(w);
    w.appendChild(certs.acmeSslSection({ domain }, {}));
    const s = await waitFor(() => w.querySelector('#zp-acme-cert-select'));
    await sleep(30);
    const applyBtn = btnByText(w, '应用选中的证书');
    return {
      text: (w.innerText || '').replace(/\s+/g, ' ').trim(),
      selected: s ? s.value : null,
      applyDisabled: applyBtn ? applyBtn.disabled : null,
      hasApplyBtn: !!applyBtn,
      hasGoApply: !!btnByText(w, '➕ 去申请新证书'),
      pills: pillsOf(w),
    };
  };
  out.wildcardCase = await oneCase('sub.api.test');
  out.noMatchCase = await oneCase('nope.test');
  // 失败条目绝不能进入"可应用证书"下拉：它根本没有证书文件。
  out.failOnlyCase = await oneCase('fail.test');

  // ================= ⑨ 后端未落地 / 数据形状不符时不白屏 =================
  // 这是用户明确点名要回答的问题：接口 404/500、或返回的 JSON 形状和契约不同时，
  // 页面必须**照常可用**（一个可读的错误态），而不是整页 TypeError。
  window.__failCerts = true;
  const failBox = document.createElement('div');
  root.appendChild(failBox);
  certs.CertsView(failBox);
  await waitFor(() => (failBox.innerText || '').includes('读取证书失败'));
  out.failText = (failBox.innerText || '').replace(/\s+/g, ' ').trim();
  window.__failCerts = false;
  // 归一化函数对"什么都不像"的输入必须返回空数组，绝不抛异常。
  out.norm = {
    certsNull: certs.normalizeCerts(null).length,
    certsString: certs.normalizeCerts('boom').length,
    certsWeird: certs.normalizeCerts({ list: [null, 42, {}, { domain: 'ok.test' }] }).length,
    providersNull: certs.normalizeProviders(null).length,
    providersStringField: certs.normalizeProviders({ providers: 'x' }).length,
    providersWeird: certs.normalizeProviders([{ nope: 1 }, 'cloudflare', null]).length,
  };

  // ================= ⑩ 反向代理：列表锁标志 + HTTPS 区块 + 绑定证书 =================
  // 这一节验证用户明确要求的能力："反向代理面板的内容也要能调用证书"。
  // 关键点是**两步保存**：先 POST /proxies 建规则，拿到 id 后再
  // POST /proxies/{id}/ssl 绑定证书 —— 两步都必须真的发生，且请求体符合契约。
  const { ReverseProxyView } = await import('./reverseproxy.js');
  const pbox = document.createElement('div');
  root.appendChild(pbox);
  ReverseProxyView(pbox);
  await waitFor(() => btnByText(pbox, '➕ 新建规则'));
  await sleep(60);
  out.proxyPills = pillsOf(pbox);
  out.proxyPillTitles = Array.from(pbox.querySelectorAll('.pill')).map((p) => p.getAttribute('title') || '');
  out.proxyText = (pbox.innerText || '').replace(/\s+/g, ' ').trim();
  out.proxyLockPillCount = (out.proxyPills || []).filter((x) => x.text.includes('🔒 HTTPS')).length;

  const newBtn = await waitFor(() => btnByText(pbox, '➕ 新建规则'));
  out.proxyNewButtonFound = !!newBtn;
  if (!newBtn) {
    throw new Error('反向代理页没有渲染出「新建规则」按钮：' + (pbox.innerText || '').slice(0, 200));
  }
  newBtn.click();
  const pmodal = await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '').includes('反向代理规则')));
  out.proxyHasSSLSwitch = !!pmodal.querySelector('#zp-proxy-ssl-on');
  out.sslDetailHiddenBefore = (pmodal.querySelector('#zp-proxy-ssl-detail')?.style.display || '') === 'none';

  pmodal.querySelector('input[placeholder="例如：镜像站"]').value = '演示反代';
  // 目标地址输入框的 placeholder 已随示例更新（不再是 127.0.0.1 那个旧示例），
  // 这里按前缀匹配，避免又被示例地址的改动带红。
  pmodal.querySelector('input[placeholder^="http://"]').value = 'http://127.0.0.1:9000';
  pmodal.querySelector('input[placeholder^="留空 = 该端口上所有域名"]').value = 'demo.test';

  document.getElementById('zp-proxy-ssl-on').checked = true;
  document.getElementById('zp-proxy-ssl-on').dispatchEvent(new Event('change'));
  document.getElementById('zp-proxy-ssl-provider').value = 'acme';
  document.getElementById('zp-proxy-ssl-provider').dispatchEvent(new Event('change'));
  const pcert = await waitFor(() => {
    const s = document.getElementById('zp-proxy-ssl-cert');
    return s && s.options.length > 0 ? s : null;
  });
  out.sslDetailVisibleAfter = (pmodal.querySelector('#zp-proxy-ssl-detail')?.style.display || '') !== 'none';
  out.sslAcmeVisible = (document.getElementById('zp-proxy-ssl-acme')?.style.display || '') !== 'none';
  out.sslManualHidden = (document.getElementById('zp-proxy-ssl-manual')?.style.display || 'none') === 'none';
  out.sslCertOptions = Array.from(pcert.options).map((o) => o.value);
  out.sslCertSelected = pcert.value;
  out.sslCertHint = (document.querySelector('#zp-proxy-ssl-acme .hint')?.textContent || '').trim();

  // 切到"粘贴自有证书"：手工区出现、证书库区隐藏（两个来源互斥）。
  document.getElementById('zp-proxy-ssl-provider').value = 'manual';
  document.getElementById('zp-proxy-ssl-provider').dispatchEvent(new Event('change'));
  out.sslManualVisibleAfterSwitch = (document.getElementById('zp-proxy-ssl-manual')?.style.display || '') !== 'none';
  out.sslAcmeHiddenAfterSwitch = (document.getElementById('zp-proxy-ssl-acme')?.style.display || 'none') === 'none';
  document.getElementById('zp-proxy-ssl-provider').value = 'acme';
  document.getElementById('zp-proxy-ssl-provider').dispatchEvent(new Event('change'));
  await waitFor(() => (document.getElementById('zp-proxy-ssl-cert') || {}).options
    && document.getElementById('zp-proxy-ssl-cert').options.length > 0);

  btnByText(pmodal, '创建').click();
  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && f.path === '/api/v1/proxies'));
  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && /\/proxies\/\d+\/ssl$/.test(f.path)));
  const isCreate = (f) => f.method === 'POST' && f.path === '/api/v1/proxies';
  const isSSL = (f) => f.method === 'POST' && /\/proxies\/\d+\/ssl$/.test(f.path);
  out.proxyCreateBody = (window.__fetches.find(isCreate) || {}).body;
  out.proxySSLPath = (window.__fetches.find(isSSL) || {}).path;
  out.proxySSLBody = (window.__fetches.find(isSSL) || {}).body;
  out.proxyModalClosed = !!(await waitFor(() => modals().length === 0));
  closeAllModals();

  // ---- 关闭 HTTPS：编辑一条已启用 SSL 的规则，保存必须带 ssl_enabled:false，
  //      且**不再**调 /ssl（关就是关，不能顺手又绑一张）。
  await waitFor(() => Array.from(pbox.querySelectorAll('button'))
    .filter((b) => (b.textContent || '').trim() === '✏️ 编辑').length >= 2);
  const editBtns = Array.from(pbox.querySelectorAll('button'))
    .filter((b) => (b.textContent || '').trim() === '✏️ 编辑');
  editBtns[1].click(); // [0] 是刚创建的无 SSL 规则，[1] 是预置的 NAS（SSL 已开）
  const emodal = await waitFor(() => modals().find((m) => (m.querySelector('.modal-head h3')?.textContent || '').includes('反向代理规则')));
  out.editSSLOnChecked = document.getElementById('zp-proxy-ssl-on').checked;
  out.editCurrentSSLHint = (emodal.innerText || '').includes('当前已启用');
  document.getElementById('zp-proxy-ssl-on').checked = false;
  document.getElementById('zp-proxy-ssl-on').dispatchEvent(new Event('change'));
  out.sslDetailHiddenAfterUncheck = (emodal.querySelector('#zp-proxy-ssl-detail')?.style.display || 'none') === 'none';
  const sslPostsBefore = window.__fetches.filter(isSSL).length;
  btnByText(emodal, '保存').click();
  await waitFor(() => window.__fetches.some((f) => f.method === 'POST' && f.path === '/api/v1/proxies/21'));
  out.proxyDisableBody = (window.__fetches.find((f) => f.method === 'POST' && f.path === '/api/v1/proxies/21') || {}).body;
  out.proxyDisableNoSSLPost = window.__fetches.filter(isSSL).length === sslPostsBefore;
  closeAllModals();

  out.calls = window.__fetches.map((f) => f.method + ' ' + f.path);
  out.taskError = window.__taskError;
  return out;
});

server.close();
await browser.close();

// ---------- 4. 打印 + 断言 ----------
const checks = [];
const check = (label, cond, extra = '') => checks.push({ label, ok: !!cond, extra });
const row = (text) => (result.rows || []).find((r) => r.text.includes(text)) || {};
const pillWith = (pills, needle) => (pills || []).find((p) => p.text.includes(needle)) || {};

console.log('══════════ ① 证书列表 ══════════');
console.log('  表头：' + (result.headers || []).join(' | '));
for (const r of result.rows || []) console.log('  · ' + r.text + '   〔按钮：' + r.actions.join(' / ') + '〕');
console.log('  顶部统计：' + (result.headerPills || []).map((p) => p.text).join('  '));

console.log('\n══════════ ② 空状态 ══════════');
console.log('  ' + (result.emptyText || '').slice(0, 160));
console.log('  按钮：' + (result.emptyButtons || []).join(' / '));

console.log('\n══════════ ③ 申请表单（dns-01 动态字段）══════════');
console.log(`  默认(challenge=http-01)：DNS 服务商区域隐藏 = ${result.dnsRowHiddenBefore}，下拉选项 ${result.providerCountBefore} 个`);
console.log(`  切到 dns-01 后：区域可见 = ${result.dnsRowVisibleAfter}`);
console.log('  服务商下拉：' + (result.providerOptions || []).join('  |  '));
console.log('  Cloudflare 输入框：' + (result.keysCloudflare || []).join(', ') + '（type=' + (result.typesCloudflare || []).join(',') + '）');
console.log('  切到阿里云后输入框：' + (result.keysAliyun || []).join(', '));
console.log('  切到 DNSPod 后输入框：' + (result.keysDnspod || []).join(', '));

console.log('\n══════════ ④ 提交：走任务中心 + 请求体 ══════════');
console.log('  taskCenter.start 调用：' + JSON.stringify(result.taskCalls));
console.log('  POST /api/v1/certs 请求体：' + JSON.stringify(result.postBody));
console.log('  提交后申请窗自动关闭：' + result.modalClosedAfterSubmit);
console.log('  http-01 请求体（应无 dns）：' + JSON.stringify((result.postBodies || [])[1]));
console.log('  泛域名 + http-01 被拦下：' + result.wildcardBlockedNoPost + '；提示：' + (result.wildcardBlockedToast || '').slice(0, 80));

console.log('\n══════════ ⑤ 续期 ══════════');
console.log('  taskCenter.start 调用：' + JSON.stringify(result.renewCall));
console.log('  POST 续期路径：' + (result.renewPaths || []).join(', '));

console.log('\n══════════ ⑥ 删除确认 ══════════');
console.log('  确认框全文：' + (result.deleteConfirmText || '').slice(0, 200));
console.log('  确认框按钮：' + (result.deleteConfirmButtons || []).join(' / '));
console.log('  点「取消」后的 DELETE 次数：' + result.deleteAfterCancel);
console.log('  确认后的 DELETE：' + (result.deleteCalls || []).join(', '));

console.log('\n══════════ ⑦ 站点侧「使用已有证书」══════════');
console.log('  详情标题：' + result.detailTitle);
console.log('  站点 www.demo.test 预选：' + result.siteSelected + '  ←  ' + result.siteSelectedLabel);
console.log('  该站点 SSL Tab 里的匹配提示：' + (result.sitePills || []).map((p) => p.text).join('  |  '));
console.log('  通配符 *.api.test 覆盖 sub.api.test → 预选：' + result.wildcardCase.selected);
console.log('  无匹配 nope.test → 主按钮禁用=' + result.noMatchCase.applyDisabled
  + '，有「去申请」=' + result.noMatchCase.hasGoApply);
console.log('    提示：' + result.noMatchCase.pills.map((p) => p.text).join(' | '));

console.log('\n══════════ ⑧ 后端未落地 / 形状不符时的容错 ══════════');
console.log('  证书接口 500 时的页面：' + (result.failText || '').slice(0, 160));
console.log('  归一化垃圾输入：' + JSON.stringify(result.norm));

console.log('\n=== 后端收到的请求 ===');
for (const c of result.calls || []) console.log('  ' + c);
if (result.taskError) console.log('  [taskError] ' + result.taskError);

// ---------- 断言 ----------
// ① 列表：剩余天数 + <30 天醒目提示
check('列表渲染 4 条（3 张证书 + 1 条失败记录）', (result.rows || []).length === 4, JSON.stringify((result.rows || []).map((r) => r.text)));
check('表头包含「主域名/覆盖域名/签发机构/到期时间/验证方式/CA」',
  ['主域名', '覆盖域名', '签发机构', '到期时间', '验证方式', 'CA'].every((x) => (result.headers || []).includes(x)),
  (result.headers || []).join(','));
const demoTxt = row('demo.test').text || '';
const demoPill = pillWith(row('demo.test').pills, '12 天');
check('列表显示剩余天数（demo.test → 12 天）', /12 天/.test(demoTxt), demoTxt);
check('<30 天是醒目提示（warn/danger 徽标 + ⚠️）',
  demoPill.cls && (demoPill.cls.includes('warn') || demoPill.cls.includes('danger'))
  && demoPill.text.includes('⚠️'), JSON.stringify(demoPill));
check('充裕的证书用 ok 徽标（api.test → 剩余 60 天）',
  (pillWith(row('api.test').pills, '剩余 60 天').cls || '').includes('ok'),
  JSON.stringify(row('api.test').pills));
check('已过期的证书用 danger 徽标（old.test）',
  (pillWith(row('old.test').pills, '已过期').cls || '').includes('danger'),
  JSON.stringify(row('old.test').pills));
check('需要续期的证书有「需要续期」提示', /需要续期/.test(demoTxt), demoTxt);
check('每行都有「续期」「删除」',
  ['🔄 续期', '删除'].every((t) => (row('demo.test').actions || []).includes(t)),
  JSON.stringify(row('demo.test').actions));
check('顶部统计指出 30 天内到期的数量',
  (result.headerPills || []).some((p) => p.text.includes('30 天内到期')), JSON.stringify(result.headerPills));

// ② 空状态
check('空状态写明「还没有证书，点右上角申请」', /还没有证书，点右上角申请/.test(result.emptyText || ''), result.emptyText);
check('空状态给申请入口', (result.emptyButtons || []).some((t) => t.includes('申请')), JSON.stringify(result.emptyButtons));

// ③ dns-01 动态表单
check('默认 http-01 时 DNS 服务商区域隐藏', result.dnsRowHiddenBefore === true);
check('选 dns-01 后 DNS 服务商下拉出现', result.dnsRowVisibleAfter === true);
check('服务商来自接口（3 个，含 label）',
  (result.providerOptions || []).length === 3 && (result.providerOptions || []).some((o) => o.includes('阿里云 DNS')),
  JSON.stringify(result.providerOptions));
check('默认服务商（Cloudflare）按 env 数组渲染出 CF_Token / CF_Account_ID',
  JSON.stringify(result.keysCloudflare) === JSON.stringify(['CF_Token', 'CF_Account_ID']),
  JSON.stringify(result.keysCloudflare));
check('凭据输入框是 type=password',
  (result.typesCloudflare || []).length === 2 && (result.typesCloudflare || []).every((t) => t === 'password'),
  JSON.stringify(result.typesCloudflare));
check('切到阿里云 → 输入框变成 Ali_Key / Ali_Secret（数据驱动，非写死）',
  JSON.stringify(result.keysAliyun) === JSON.stringify(['Ali_Key', 'Ali_Secret']),
  JSON.stringify(result.keysAliyun));
check('切到 DNSPod（键→说明映射形状）→ DP_Id / DP_Key',
  JSON.stringify(result.keysDnspod) === JSON.stringify(['DP_Id', 'DP_Key']),
  JSON.stringify(result.keysDnspod));

// ④ 提交走 taskCenter，参数含 challenge/ca/dns
const tc = (result.taskCalls || [])[0] || {};
check('提交走 taskCenter.start', (result.taskCalls || []).length >= 1 && tc.hasStart === true, JSON.stringify(result.taskCalls));
check('taskCenter 参数：kind=cert / target 用主域名 / 有标题',
  tc.kind === 'cert' && tc.target === 'cert:*.api.test' && /申请证书/.test(tc.title || ''),
  JSON.stringify(tc));
const pb = result.postBody || {};
check('POST /api/v1/certs 请求体含 challenge=dns-01', pb.challenge === 'dns-01', JSON.stringify(pb));
check('POST 请求体含 ca=letsencrypt-staging', pb.ca === 'letsencrypt-staging', JSON.stringify(pb));
check('POST 请求体含 dns:{name, env}（name=cloudflare）',
  pb.dns && pb.dns.name === 'cloudflare', JSON.stringify(pb.dns));
check('dns.env 的键与输入框键名一致、值是用户填的',
  pb.dns && JSON.stringify(pb.dns.env) === JSON.stringify({ CF_Token: 'cf-token-abc', CF_Account_ID: 'cf-account-123' }),
  JSON.stringify(pb.dns && pb.dns.env));
check('domains 按换行拆成数组', JSON.stringify(pb.domains) === JSON.stringify(['*.api.test', 'api.test']), JSON.stringify(pb.domains));
check('提交后申请窗自动关闭', result.modalClosedAfterSubmit === true);
const pb2 = (result.postBodies || [])[1] || {};
check('http-01 提交不带 dns 字段', pb2.challenge === 'http-01' && !('dns' in pb2), JSON.stringify(pb2));
check('泛域名 + http-01 在提交前被拦下（不发请求）', result.wildcardBlockedNoPost === true,
  result.wildcardBlockedToast);
check('拦截提示说明必须用 dns-01', /dns-01/.test(result.wildcardBlockedToast || ''), result.wildcardBlockedToast);

// ⑤ 删除确认
check('删除有确认框（标题「删除证书」）', /删除证书/.test(result.deleteConfirmText || ''), (result.deleteConfirmText || '').slice(0, 80));
check('确认框说明「删除后引用它的站点会失去 HTTPS」',
  /删除后引用它的站点会失去 HTTPS/.test(result.deleteConfirmText || ''), (result.deleteConfirmText || '').slice(0, 160));
check('取消不发 DELETE', result.deleteAfterCancel === 0, String(result.deleteAfterCancel));
check('确认后发出 DELETE /api/v1/certs/demo.test',
  (result.deleteCalls || []).includes('/api/v1/certs/demo.test'), JSON.stringify(result.deleteCalls));

// ⑥ 续期
check('续期走任务中心（target=cert:api.test）', result.renewCall && result.renewCall.target === 'cert:api.test',
  JSON.stringify(result.renewCall));
check('续期 POST 到 /api/v1/certs/api.test/renew',
  (result.renewPaths || []).includes('/api/v1/certs/api.test/renew'), JSON.stringify(result.renewPaths));

// ⑦ 失败条目：状态 + 一键重试 + 预填
const failTxt = result.failRowText || '';
check('失败条目显示「申请失败，可重试」', /申请失败，可重试/.test(failTxt), failTxt);
check('失败条目带上次错误（脱敏痕迹）', /申请证书失败.*\*\*\*/.test(failTxt), failTxt);
check('失败条目不出示到期天数（没有到期时间）', result.failRowShowsDays === false, failTxt);
check('失败条目有「重试」「修改后重试」「删除」',
  ['🔁 重试', '✏️ 修改后重试', '删除'].every((t) => (result.failRowActions || []).includes(t)),
  JSON.stringify(result.failRowActions));
check('顶部统计失败数量并提供重试说明',
  (result.headerPills || []).some((p) => /申请失败（可重试）/.test(p.text)), JSON.stringify(result.headerPills));
check('一键重试走任务中心（target=cert:fail.test）',
  result.retryCall && result.retryCall.target === 'cert:fail.test', JSON.stringify(result.retryCall));
check('重试 POST 到 /api/v1/certs/fail.test/retry 且请求体为空（用户不重填）',
  (result.calls || []).includes('POST /api/v1/certs/fail.test/retry')
  && Object.keys(result.retryBody || {}).length === 0, JSON.stringify(result.retryBody));
const pf = result.prefill || {};
check('「修改后重试」预填域名', pf.domains === 'fail.test\nwww.fail.test', JSON.stringify(pf));
check('「修改后重试」预填邮箱 / CA / 校验方式',
  pf.email === 'ops@example.com' && pf.ca === 'letsencrypt' && pf.challenge === 'dns-01', JSON.stringify(pf));
check('「修改后重试」预选 DNS 服务商并说明凭据已在服务端',
  pf.provider === 'cloudflare' && /已保存在服务端/.test(pf.hint || ''), JSON.stringify(pf));

// ⑦ 站点侧
check('站点详情里确实挂了「使用 Let\'s Encrypt 证书」这块（有证书下拉）',
  !!result.siteSelected, result.siteSelected);
check('站点 www.demo.test 按域名预选 demo.test（精确覆盖）',
  result.siteSelected === 'demo.test', result.siteSelected + ' / ' + result.siteSelectedLabel);
check('预选来源可解释（徽标说明按域名自动匹配）',
  (result.sitePills || []).some((p) => /已按域名自动匹配：demo\.test/.test(p.text)),
  JSON.stringify(result.sitePills));
check('泛域名证书 *.api.test 预选覆盖 sub.api.test（通配符匹配）',
  result.wildcardCase.selected === 'api.test', JSON.stringify(result.wildcardCase));
check('无匹配域名时不再给"必然报警告"的主按钮（应用按钮禁用）',
  result.noMatchCase.applyDisabled === true, JSON.stringify(result.noMatchCase));
check('无匹配域名时给出「去申请」引导',
  result.noMatchCase.hasGoApply === true && /没有与该域名匹配的证书/.test(result.noMatchCase.text),
  result.noMatchCase.text.slice(0, 160));
check('失败条目不会进入站点「可应用证书」下拉（主按钮禁用）',
  result.failOnlyCase.applyDisabled === true, JSON.stringify(result.failOnlyCase));

// ⑧ 接口未落地 / 形状不符
check('证书接口失败时页面是可读错误态（不白屏）',
  /读取证书失败/.test(result.failText || ''), result.failText);
const nm = result.norm || {};
check('归一化垃圾输入返回空数组而不是抛异常',
  nm.certsNull === 0 && nm.certsString === 0 && nm.providersNull === 0 && nm.providersStringField === 0,
  JSON.stringify(nm));
check('只保留能识别的条目（certs 垃圾 4 条 → 1 条合法；providers 3 条 → 1 条合法）',
  nm.certsWeird === 1 && nm.providersWeird === 1, JSON.stringify(nm));

// ⑨ 反向代理 HTTPS
console.log('\n══════════ ⑨ 反向代理 HTTPS ══════════');
console.log('  列表文本：' + (result.proxyText || '').slice(0, 220));
console.log('  锁标志：' + JSON.stringify(result.proxyPills || []));
console.log('  HTTPS 开关存在：' + result.proxyHasSSLSwitch
  + '；打开前详情隐藏=' + result.sslDetailHiddenBefore + '；打开后可见=' + result.sslDetailVisibleAfter);
console.log('  证书下拉：' + JSON.stringify(result.sslCertOptions || []) + ' → 预选 ' + result.sslCertSelected);
console.log('  证书说明：' + (result.sslCertHint || '').slice(0, 160));
console.log('  切到「粘贴自有证书」：手工区可见=' + result.sslManualVisibleAfterSwitch
  + '，证书库区隐藏=' + result.sslAcmeHiddenAfterSwitch);
console.log('  创建规则请求体：' + JSON.stringify(result.proxyCreateBody));
console.log('  绑定证书：' + result.proxySSLPath + ' 请求体=' + JSON.stringify(result.proxySSLBody));
console.log('  保存后弹窗关闭：' + result.proxyModalClosed);
console.log('  关闭 HTTPS：开关初始选中=' + result.editSSLOnChecked
  + '，请求体=' + JSON.stringify(result.proxyDisableBody)
  + '，未再调 /ssl=' + result.proxyDisableNoSSLPost);

check('规则列表显示锁标志（已启用 HTTPS 的规则有 🔒，纯 HTTP 的没有）',
  result.proxyLockPillCount === 1, JSON.stringify(result.proxyPills));
check('锁标志的悬浮说明给出证书路径',
  (result.proxyPillTitles || []).some((x) => /certs\/nas\.zizdog\.com\/fullchain\.pem/.test(x)),
  JSON.stringify(result.proxyPillTitles || []));
check('列表行显示证书覆盖域名与到期/剩余天数',
  /覆盖 nas\.zizdog\.com/.test(result.proxyText || '') && /2026-12-01/.test(result.proxyText || ''),
  (result.proxyText || '').slice(0, 240));
check('编辑表单里有「启用 HTTPS」开关', result.proxyHasSSLSwitch === true);
check('HTTPS 详情默认隐藏，打开开关后才出现', result.sslDetailHiddenBefore === true && result.sslDetailVisibleAfter === true,
  `before=${result.sslDetailHiddenBefore} after=${result.sslDetailVisibleAfter}`);
check('证书来源选 acme 时出现证书库下拉、手工粘贴区隐藏',
  result.sslAcmeVisible === true && result.sslManualHidden === true,
  JSON.stringify({ acme: result.sslAcmeVisible, manualHidden: result.sslManualHidden }));
check('证书下拉来自证书接口（3 张已签发证书）',
  JSON.stringify(result.sslCertOptions || []) === JSON.stringify(['demo.test', 'api.test', 'old.test']),
  JSON.stringify(result.sslCertOptions));
check('按规则域名自动预选证书（demo.test）', result.sslCertSelected === 'demo.test', result.sslCertSelected);
check('证书说明给出到期/剩余天数',
  /到期：2026-10-01/.test(result.sslCertHint || '') && /剩余 12 天/.test(result.sslCertHint || ''),
  result.sslCertHint);
check('切到「粘贴自有证书」后两个来源互斥',
  result.sslManualVisibleAfterSwitch === true && result.sslAcmeHiddenAfterSwitch === true,
  JSON.stringify({ manual: result.sslManualVisibleAfterSwitch, acmeHidden: result.sslAcmeHiddenAfterSwitch }));
// 2026-09-17 真机报障后的**当前**行为：启用 HTTPS 时，创建规则的 payload 就必须
// 带上 ssl_enabled + 证书路径 —— 否则后端把这条当 HTTP 规则，同端口已有 HTTPS 规则时
// 会被"同端口不能混用"直接 409，第二步 /ssl 永远没机会跑。证书仍由专用接口绑定（下一条断言）。
check('启用 HTTPS：创建规则时 payload 就带 ssl 信息（避免同端口 HTTP/HTTPS 冲突 409）',
  !!result.proxyCreateBody && result.proxyCreateBody.ssl_enabled === true
  && result.proxyCreateBody.ssl_provider === 'acme'
  && !!result.proxyCreateBody.ssl_cert
  && result.proxyCreateBody.target === 'http://127.0.0.1:9000'
  && result.proxyCreateBody.domains === 'demo.test',
  JSON.stringify(result.proxyCreateBody));
check('创建成功后紧接着 POST /api/v1/proxies/7/ssl 绑定证书',
  result.proxySSLPath === '/api/v1/proxies/7/ssl', String(result.proxySSLPath));
check('绑定请求体符合站点侧同构契约（provider=acme + cert_primary）',
  !!result.proxySSLBody && result.proxySSLBody.provider === 'acme'
  && result.proxySSLBody.cert_primary === 'demo.test',
  JSON.stringify(result.proxySSLBody));
check('绑定成功后弹窗自动关闭', result.proxyModalClosed === true);
check('编辑已开 SSL 的规则时开关处于选中态并提示当前状态',
  result.editSSLOnChecked === true && result.editCurrentSSLHint === true,
  JSON.stringify({ checked: result.editSSLOnChecked, hint: result.editCurrentSSLHint }));
check('关掉开关后 HTTPS 详情隐藏（不再是"看着开着其实没关"）',
  result.sslDetailHiddenAfterUncheck === true, String(result.sslDetailHiddenAfterUncheck));
check('关闭 HTTPS 走主接口且请求体带 ssl_enabled=false',
  !!result.proxyDisableBody && result.proxyDisableBody.ssl_enabled === false,
  JSON.stringify(result.proxyDisableBody));
check('关闭 HTTPS 时不再调 /ssl（不会顺手重绑一张证书）',
  result.proxyDisableNoSSLPost === true, String(result.proxyDisableNoSSLPost));

console.log('\n══════════ 断言结果 ══════════');
let failed = 0;
for (const c of checks) {
  if (!c.ok) failed++;
  console.log(`  ${c.ok ? '✓' : '✗'} ${c.label}${c.ok ? '' : '  —— ' + c.extra}`);
}
console.log(`\n  ${checks.length - failed}/${checks.length} 通过`);
if (failed) process.exitCode = 1;
