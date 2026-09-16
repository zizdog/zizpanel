// db-password-verify.mjs —— 验收「数据库账号密码列」（用户明确要求：像宝塔那样能看密码）。
//
// 为什么这么搭（照 tasks-input-verify.mjs / appdetail-verify.mjs 的样子）：
//   · 真 acorn（tools/check-js-syntax.mjs）只能证明语法合法，
//     证明不了"默认打码、点 👁 才显示、未知态不伪造、改完会重新拉取"；
//   · 所以用 Playwright 加载**真实的** internal/web/assets/js/database.js，
//     只把 app.js 换成最小替身（它只管启动外壳，会把登录页拖进来），
//     再把 fetch 换成假后端（不碰真实面板、不碰真实 MySQL）。
//     database.js / api.js / ui.js **一个字符都没有为测试改过**。
//
// 覆盖：
//   ① 「账号与权限」表新增密码列；
//   ② 已知口令：默认打码（DOM 里没有明文）、点 👁 显示/隐藏、点 📋 复制；
//   ③ 未知口令：显示「外部设置，不可回显」+「重设密码」，**绝不**伪造 password；
//   ④ 已知为空口令：显示「空密码（无口令）」而不是"不可回显"；
//   ⑤ root 行：显示口令并标「面板连接用」；
//   ⑥ 改密码后**重新拉取 GET /api/v1/database**（列表刷新，不是只改内存）；
//   ⑦ 安全：口令不进 URL / localStorage / sessionStorage / DOM data 属性 / console。
//
// 用法： export PATH=/opt/homebrew/bin:$PATH; node tools/db-password-verify.mjs
import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const JS_DIR = path.join(ROOT, 'internal/web/assets/js');

// ---------- 测试用的假口令（真实口令不入库、不入仓库） ----------
const ROOT_PW = 'Root-Secret-Pw-1';
const KNOWN_PW = 'Known-Secret-Pw-2';
const NEW_PW = 'Re5et-Ext-Pw-9';
const SECRETS = [ROOT_PW, KNOWN_PW, NEW_PW];

// app.js 是应用外壳（会 boot、拉会话、渲染登录页）。database.js 只用到
// state / registerCleanup / panelPath，这里给最小替身。
const APP_STUB = `
export const state = { session: { user: { username: 'admin' }, config: {
  panel_entry: '/', mysql_host: '127.0.0.1', mysql_port: 3306,
  mysql_socket: '/tmp/mysql.sock', mysql_user: 'root', mysql_password: 'pw-from-config',
} }, metrics: null, route: '' };
export const NAV = [];
export function panelPath(sub = '') { return '/' + String(sub).replace(/^\\/+/, ''); }
export function registerCleanup() { return () => {}; }
`;

// ---------- 1. 静态服务器（ESM 必须走 http，file:// 会被 CORS 挡） ----------
const MIME = { '.js': 'text/javascript; charset=utf-8', '.html': 'text/html; charset=utf-8' };
const server = http.createServer((req, res) => {
  const rel = decodeURIComponent(new URL(req.url, 'http://x').pathname).replace(/^\/+/, '');
  if (rel === '' || rel === 'index.html') {
    res.writeHead(200, { 'Content-Type': MIME['.html'] });
    // #toasts 是 ui.toast 的挂载点（真实外壳由 app.js 建，这里必须补上）。
    res.end('<!doctype html><html><head><meta charset="utf-8"></head>'
      + '<body><div id="root"></div><div id="toasts"></div></body></html>');
    return;
  }
  if (rel === 'app.js') {
    res.writeHead(200, { 'Content-Type': MIME['.js'] });
    res.end(APP_STUB);
    return;
  }
  const file = path.join(JS_DIR, rel);
  if (!file.startsWith(JS_DIR) || !fs.existsSync(file)) { res.writeHead(404); res.end('nope'); return; }
  res.writeHead(200, { 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream' });
  res.end(fs.readFileSync(file));
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const base = `http://127.0.0.1:${server.address().port}/`;

// ---------- 2. 假后端 ----------
// 账号列表刻意覆盖四种状态：
//   root      —— 面板自己的连接账号（password_known=true, panel_account=true）
//   knownuser —— 面板创建/改过（password_known=true）
//   extuser   —— 别人手工建的（password_known=false，响应里**没有** password 字段）
//   emptyuser —— 面板创建时就没设口令（password_known=true, password=''）
const OVERVIEW = {
  connected: true, version: '8.4.11', has_password: true,
  databases: [{ name: 'blog', tables: 3, size: 1024, charset: 'utf8mb4', collation: 'utf8mb4_general_ci' }],
  users: [
    {
      user: 'root', host: 'localhost', databases: ['*'], privileges: ['ALL'],
      is_locked: false, has_password: true, auth_plugin: 'caching_sha2_password', system: true,
      password_known: true, password: ROOT_PW, password_source: 'panel', panel_account: true,
    },
    {
      user: 'knownuser', host: 'localhost', databases: ['blog'], privileges: ['SELECT'],
      is_locked: false, has_password: true, auth_plugin: 'caching_sha2_password', system: false,
      password_known: true, password: KNOWN_PW, password_source: 'panel',
    },
    {
      user: 'extuser', host: 'localhost', databases: ['blog'], privileges: ['SELECT'],
      is_locked: false, has_password: true, auth_plugin: 'caching_sha2_password', system: false,
      password_known: false,
      // 注意：**没有** password 字段 —— 后端对不可知口令就是这么返回的。
    },
    {
      user: 'emptyuser', host: '%', databases: [], privileges: [],
      is_locked: false, has_password: false, auth_plugin: 'caching_sha2_password', system: false,
      password_known: true, password: '', password_source: 'panel',
    },
  ],
};

const browser = await chromium.launch();
const context = await browser.newContext();
// addInitScript 的函数体在**浏览器**里执行，看不到 Node 侧的变量 ——
// 所以假后端数据必须当参数传进去（第一版就是直接引用了外面的常量，
// 结果 fetch 一执行就 ReferenceError，页面退化成了「连接设置」表单）。
await context.addInitScript((overview) => {
  window.__calls = [];
  window.__clipboard = [];
  // 假剪贴板：无头 http 下 navigator.clipboard 本来可能是 undefined，
  // 这里钉住"支持剪贴板"的分支，好断言复制按钮真的把口令写进了剪贴板。
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: (t) => { window.__clipboard.push(String(t)); return Promise.resolve(); } },
  });
  window.fetch = async (url, init = {}) => {
    const u = String(url);
    const method = (init.method || 'GET').toUpperCase();
    window.__calls.push({ method, url: u, body: init.body || '' });
    const reply = (envelope, ok = true, status = 200) => ({
      ok, status, statusText: ok ? 'OK' : 'ERR',
      text: async () => JSON.stringify(envelope),
    });
    if (method === 'GET' && u.includes('/database') && !u.includes('/database/')) {
      return reply({ ok: true, data: overview });
    }
    if (method === 'POST' && u.includes('/database/user/password')) {
      return reply({ ok: true, data: { msg: '密码已重置' } });
    }
    return reply({ ok: false, msg: '测试后端没有这个路由：' + method + ' ' + u }, false, 404);
  };
}, OVERVIEW);

const page = await context.newPage();
const errs = [];
page.on('console', (m) => errs.push('[' + m.type() + '] ' + m.text()));
page.on('pageerror', (e) => errs.push('[pageerror] ' + e.message));
await page.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });

const out = await page.evaluate(async ({ ROOT_PW, KNOWN_PW, NEW_PW }) => {
  const { DatabaseView } = await import('./database.js');
  const waitFor = async (fn, ms = 8000) => {
    const t0 = Date.now();
    for (;;) {
      let v = null;
      try { v = fn(); } catch { v = null; }
      if (v) return v;
      if (Date.now() - t0 > ms) return null;
      await new Promise((r) => setTimeout(r, 50));
    }
  };
  const box = document.getElementById('root');
  DatabaseView(box, {});
  // 等 tab 出现，再切到「账号与权限」
  const tabBtn = await waitFor(() => Array.from(box.querySelectorAll('button'))
    .find((b) => b.textContent === '账号与权限'));
  tabBtn.click();
  await waitFor(() => box.querySelector('table.table tbody tr'));

  const o = {};
  const headers = Array.from(box.querySelectorAll('table.table thead th')).map((th) => th.textContent);
  o.headers = headers;
  const rows = Array.from(box.querySelectorAll('table.table tbody tr'));
  const rowFor = (user) => rows.find((tr) => (tr.querySelector('td') || {}).textContent
    && tr.querySelector('td').textContent.includes(user));
  const pwdCell = (tr) => tr.querySelectorAll('td')[3];
  // 口令文本所在的节点（默认打码时里面只有圆点）。用 .mono 精确定位，
  // 免得把「👁 / 📋 / 面板连接用」这些按钮文案也算进来。
  const pwdSpan = (tr) => pwdCell(tr).querySelector('span.mono');
  const pwdText = (tr) => (pwdSpan(tr) || {}).textContent || '';
  const buttonsIn = (el) => Array.from(el.querySelectorAll('button'));

  // ② 已知口令：默认打码，DOM 里没有明文
  const knownRow = rowFor('knownuser');
  o.knownPwdText = pwdText(knownRow);
  o.knownCellHasPlain = pwdCell(knownRow).innerHTML.includes(KNOWN_PW);
  o.knownHasToggle = buttonsIn(pwdCell(knownRow)).some((b) => b.textContent === '👁');
  o.knownHasCopy = buttonsIn(pwdCell(knownRow)).some((b) => b.textContent === '📋');

  // 点 👁 → 显示；再点 → 回到打码
  buttonsIn(pwdCell(knownRow)).find((b) => b.textContent === '👁').click();
  o.knownAfterShow = pwdText(knownRow);
  const hideBtn = buttonsIn(pwdCell(knownRow)).find((b) => b.textContent === '🙈');
  o.knownShowSwitchesIcon = !!hideBtn;
  if (hideBtn) hideBtn.click();
  o.knownAfterHide = pwdText(knownRow);

  // 点 📋 → 写进剪贴板（隐藏状态下也能复制）
  buttonsIn(pwdCell(knownRow)).find((b) => b.textContent === '📋').click();
  await new Promise((r) => setTimeout(r, 100));
  o.clipboard = window.__clipboard.slice();

  // ③ 未知口令：明确写不可回显 + 重设按钮，且 DOM 里没有任何伪造口令
  const extRow = rowFor('extuser');
  o.extText = pwdCell(extRow).innerText;
  o.extHasReset = buttonsIn(pwdCell(extRow)).some((b) => b.textContent === '重设密码');
  o.extHasToggle = buttonsIn(pwdCell(extRow)).some((b) => b.textContent === '👁' || b.textContent === '📋');

  // ④ 已知为空
  const emptyRow = rowFor('emptyuser');
  o.emptyText = pwdCell(emptyRow).innerText;

  // ⑤ root 行：显示口令 + 「面板连接用」
  const rootRow = rowFor('root');
  o.rootPwdText = pwdText(rootRow);
  o.rootCellHasPlainHidden = pwdCell(rootRow).innerHTML.includes(ROOT_PW);
  o.rootHasPanelPill = pwdCell(rootRow).innerText.includes('面板连接用');
  buttonsIn(pwdCell(rootRow)).find((b) => b.textContent === '👁').click();
  o.rootAfterShow = pwdText(rootRow);

  // 隐藏时的 DOM 里不该有任何一个明文口令（上面刚显示过 known/root，先切回打码）
  const hideKnown = buttonsIn(pwdCell(knownRow)).find((b) => b.textContent === '🙈');
  if (hideKnown) hideKnown.click();
  const hideRoot = buttonsIn(pwdCell(rootRow)).find((b) => b.textContent === '🙈');
  if (hideRoot) hideRoot.click();
  o.hiddenHtmlHasSecret = [KNOWN_PW, ROOT_PW].some((s) => pwdCell(knownRow).innerHTML.includes(s)
    || pwdCell(rootRow).innerHTML.includes(s));

  // ⑥ 改密码后必须重新拉取 GET /database
  const getCount = () => window.__calls
    .filter((c) => c.method === 'GET' && c.url.includes('/database')).length;
  const before = getCount();
  buttonsIn(pwdCell(extRow)).find((b) => b.textContent === '重设密码').click();
  const dlgTitle = await waitFor(() => Array.from(document.querySelectorAll('.modal-head h3'))
    .find((h) => h.textContent.startsWith('重置密码')));
  const dlg = dlgTitle ? Array.from(document.querySelectorAll('.modal'))
    .find((m) => (m.querySelector('.modal-head h3') || {}).textContent.startsWith('重置密码')) : null;
  o.dialogOpened = !!dlg;
  if (dlg) {
    const input = dlg.querySelector('input.input');
    input.value = NEW_PW;
    Array.from(dlg.querySelectorAll('button')).find((b) => b.textContent === '确定').click();
    await waitFor(() => getCount() > before);
  }
  o.getCountBefore = before;
  o.getCountAfter = getCount();
  o.reloadedAfterChange = getCount() > before;
  o.passwordPost = window.__calls.filter((c) => c.method === 'POST' && c.url.includes('/user/password'))
    .map((c) => c.body);

  // ⑦ 安全
  o.secretInUrl = location.href.includes('Secret') || location.href.includes('Re5et');
  o.secretInLocalStorage = JSON.stringify(window.localStorage).includes(KNOWN_PW)
    || JSON.stringify(window.localStorage).includes(ROOT_PW);
  o.secretInSessionStorage = JSON.stringify(window.sessionStorage).includes(KNOWN_PW)
    || JSON.stringify(window.sessionStorage).includes(ROOT_PW);
  o.secretInDataAttrs = Array.from(document.querySelectorAll('*')).some((el) => (
    Object.values(el.dataset || {}).some((v) => [KNOWN_PW, ROOT_PW].some((s) => String(v).includes(s)))
  ));
  return o;
}, { ROOT_PW, KNOWN_PW, NEW_PW });

// ---------- 3. 打印 ----------
console.log('══════════ ① 密码列存在 ══════════');
console.log(`  表头                 : ${JSON.stringify(out.headers)}`);

console.log('\n══════════ ② 已知口令：默认打码 / 显示 / 复制 ══════════');
console.log(`  默认显示             : ${out.knownPwdText}`);
console.log(`  默认 DOM 含明文？    : ${out.knownCellHasPlain ? '含 ✗' : '不含 ✓'}`);
console.log(`  有 👁 切换 / 📋 复制  : ${out.knownHasToggle} / ${out.knownHasCopy}`);
console.log(`  点 👁 后             : ${out.knownAfterShow}`);
console.log(`  再点（变 🙈）后      : ${out.knownAfterHide}`);
console.log(`  剪贴板收到           : ${JSON.stringify(out.clipboard)}`);

console.log('\n══════════ ③ 未知口令：如实显示不可回显 ══════════');
console.log(`  单元格文案           : ${(out.extText || '').replace(/\n/g, ' / ')}`);
console.log(`  有「重设密码」？     : ${out.extHasReset ? '有 ✓' : '没有 ✗'}`);
console.log(`  有显示/复制按钮？    : ${out.extHasToggle ? '有 ✗（不该有）' : '没有 ✓'}`);

console.log('\n══════════ ④ 已知为空口令 ══════════');
console.log(`  单元格文案           : ${out.emptyText}`);

console.log('\n══════════ ⑤ root 行（面板连接用） ══════════');
console.log(`  默认显示             : ${out.rootPwdText}`);
console.log(`  默认 DOM 含明文？    : ${out.rootCellHasPlainHidden ? '含 ✗' : '不含 ✓'}`);
console.log(`  标「面板连接用」？   : ${out.rootHasPanelPill ? '是 ✓' : '否 ✗'}`);
console.log(`  点 👁 后             : ${out.rootAfterShow}`);
console.log(`  切回打码后含明文？   : ${out.hiddenHtmlHasSecret ? '含 ✗' : '不含 ✓'}`);

console.log('\n══════════ ⑥ 改密码后重新拉取列表 ══════════');
console.log(`  弹窗打开？           : ${out.dialogOpened ? '是 ✓' : '否 ✗'}`);
console.log(`  GET /database 次数   : ${out.getCountBefore} → ${out.getCountAfter}`);
console.log(`  POST body            : ${JSON.stringify(out.passwordPost)}`);

console.log('\n══════════ ⑦ 安全：口令不进 URL / 存储 / data 属性 / console ══════════');
console.log(`  URL=${out.secretInUrl ? '含 ✗' : '不含 ✓'} localStorage=${out.secretInLocalStorage ? '含 ✗' : '不含 ✓'
} sessionStorage=${out.secretInSessionStorage ? '含 ✗' : '不含 ✓'} data 属性=${out.secretInDataAttrs ? '含 ✗' : '不含 ✓'}`);

// ---------- 4. 断言 ----------
const checks = [];
const check = (label, cond, extra = '') => checks.push({ label, ok: !!cond, extra });

check('密码列存在', out.headers.includes('密码'), JSON.stringify(out.headers));
check('已知口令默认打码（不是明文）', /^[•·*]+$/.test((out.knownPwdText || '').trim()), out.knownPwdText);
check('已知口令默认 DOM 里不含明文', !out.knownCellHasPlain);
check('有 👁 显示切换', out.knownHasToggle);
check('有 📋 复制按钮', out.knownHasCopy);
check('点 👁 后显示明文', out.knownAfterShow === KNOWN_PW, out.knownAfterShow);
check('再点后回到打码', /^[•·*]+$/.test((out.knownAfterHide || '').trim()), out.knownAfterHide);
check('📋 把口令写进剪贴板', out.clipboard.includes(KNOWN_PW), JSON.stringify(out.clipboard));
check('未知口令显示「外部设置，不可回显」', (out.extText || '').includes('外部设置，不可回显'), out.extText);
check('未知口令给「重设密码」按钮', out.extHasReset);
check('未知口令不给显示/复制按钮（没有可显示的东西）', !out.extHasToggle);
check('未知口令的单元格里没有任何伪造值', !/[:：]\s*\S/.test(out.extText || '') || !(out.extText || '').includes('••'), out.extText);
check('已知为空口令显示「空密码」', (out.emptyText || '').includes('空密码'), out.emptyText);
check('root 行默认打码且不泄露明文', !out.rootCellHasPlainHidden);
check('root 行标「面板连接用」', out.rootHasPanelPill);
check('root 行可显示出口令', out.rootAfterShow === ROOT_PW, out.rootAfterShow);
check('切回打码后 DOM 里没有明文', !out.hiddenHtmlHasSecret);
check('「重设密码」能打开改密码弹窗', out.dialogOpened);
check('改密码后重新拉取 GET /api/v1/database', out.reloadedAfterChange,
  `${out.getCountBefore} → ${out.getCountAfter}`);
check('改密码的 POST body 带新口令', out.passwordPost.some((b) => b.includes(NEW_PW)), JSON.stringify(out.passwordPost));
check('口令不进 URL', !out.secretInUrl);
check('口令不进 localStorage', !out.secretInLocalStorage);
check('口令不进 sessionStorage', !out.secretInSessionStorage);
check('口令不进 DOM data 属性', !out.secretInDataAttrs);

const leakedConsole = errs.filter((m) => SECRETS.some((s) => m.includes(s)));
check('console 里没有口令', leakedConsole.length === 0, leakedConsole.join(' | '));
check('页面没有 pageerror', !errs.some((m) => m.startsWith('[pageerror]')), errs.filter((m) => m.startsWith('[pageerror]')).join(' | '));

console.log('\n══════════ 断言结果 ══════════');
let failed = 0;
for (const c of checks) {
  if (!c.ok) failed++;
  console.log(`  ${c.ok ? '✓' : '✗'} ${c.label}${c.ok ? '' : '  —— ' + c.extra}`);
}
console.log(`\n  ${checks.length - failed}/${checks.length} 通过`);

server.close();
await browser.close();
if (failed) process.exitCode = 1;
