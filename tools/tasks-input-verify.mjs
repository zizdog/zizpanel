// tasks-input-verify.mjs —— 验收「任务限时输入」的 UI 与「安装结果凭据必须可见」（缺陷 D11）。
//
// 为什么这么搭（照 appdetail-verify.mjs / certs-verify.mjs 的样子）：
//   · 真 acorn（tools/check-js-syntax.mjs）只能证明语法合法，证明不了
//     "倒计时归零后不再提交""落定事件到了才收框""credentials 真的渲染出来了"；
//   · 所以这里用 Playwright 加载**真实的** internal/web/assets/js/tasks.js，
//     只把两样东西换成替身：
//       ① EventSource —— 真实 SSE 无法在测试里被精确地"推出"一个 timeout 落定事件；
//       ② fetch —— 免得测试去碰真实面板（要求：不要碰真实面板、不要真的安装东西）。
//     tasks.js / api.js / ui.js **一个字符都没有为测试改过**。
//
// 覆盖：
//   ① 待输入状态：password 框 + label/hint + 以 deadline 为准的倒计时 + "不填也会继续"
//   ② 提交失败：后端 msg 原样显示、输入框不消失、后端并未回显 value
//   ③ 提交成功：**不立刻收框**，等 input_required→null 落定事件才收
//   ④ 倒计时归零：显示"已超时，任务会自动生成并继续"，并且**不再提交**
//   ⑤ 凭据渲染：credentials[]（label/value）与一键建站 db_pass 都渲染出来，
//      即使结果里没有 token/address；复制按钮在剪贴板不可用时降级为选中文本
//   ⑥ 刷新恢复：新页面先 taskCenter.init()（= 拉列表）再 openTask，仍能看到待输入状态
//   ⑦ 安全：值不进 URL / localStorage / DOM data 属性 / console
//
// 用法： export PATH=/opt/homebrew/bin:$PATH; node tools/tasks-input-verify.mjs
import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const JS_DIR = path.join(ROOT, 'internal/web/assets/js');

// ---------- 1. 静态服务器（ESM 必须走 http，file:// 会被 CORS 挡） ----------
const MIME = { '.js': 'text/javascript; charset=utf-8', '.html': 'text/html; charset=utf-8' };
// app.js 是应用外壳（会 boot、拉会话、渲染登录页）。这个脚本只验证 pages 里
// 真实存在的模块，所以把 app.js 换成最小替身（照 appdetail-verify.mjs 的做法）——
// tasks.js 不 import app.js，不受影响；database.js 用它取 state.session.config。
const APP_STUB = `
export const state = { session: { user: { username: 'admin' }, config: {
  panel_entry: '/', mysql_host: '127.0.0.1', mysql_port: 3306,
  mysql_socket: '/tmp/mysql.sock', mysql_user: 'root', mysql_password: 'pw-from-config',
} }, metrics: null, route: '' };
export const NAV = [];
export function panelPath(sub = '') { return '/' + String(sub).replace(/^\\/+/, ''); }
export function registerCleanup() { return () => {}; }
`;
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

// ---------- 2. 替身：假后端 + 可控 EventSource ----------
const browser = await chromium.launch();
const context = await browser.newContext();
await context.addInitScript(() => {
  window.__calls = [];      // 收到的请求（method/url/body），用于断言"有没有再提交"
  window.__streams = [];    // 创建过的假 EventSource
  window.__backend = { tasks: {}, lines: {} };
  window.__inputRules = {}; // id -> () => {error, status}：让某个任务的提交失败

  // 假 fetch：只认任务中心的几个路由，形状与 SPEC 一致（{ok:true,data}）。
  window.fetch = async (url, init = {}) => {
    const u = String(url);
    const method = (init.method || 'GET').toUpperCase();
    window.__calls.push({ method, url: u, body: init.body || '' });
    const reply = (envelope, ok = true, status = 200) => ({
      ok, status, statusText: ok ? 'OK' : 'ERR',
      text: async () => JSON.stringify(envelope),
    });

    const mInput = u.match(/\/tasks\/([^/?]+)\/input$/);
    if (mInput && method === 'POST') {
      const id = decodeURIComponent(mInput[1]);
      const body = init.body ? JSON.parse(init.body) : {};
      const rule = window.__inputRules[id];
      if (rule) {
        const r = rule(body);
        if (r && r.error) return reply({ ok: false, msg: r.error }, false, r.status || 409);
      }
      return reply({ ok: true, data: { accepted: true, key: body.key } });
    }

    const mOne = u.match(/\/tasks\/([^/?]+)(\?|$)/);
    if (mOne && !u.includes('/tasks?')) {
      const id = decodeURIComponent(mOne[1]);
      const t = window.__backend.tasks[id];
      if (!t) return reply({ ok: false, msg: '任务不存在（面板重启后不再保留历史任务）' }, false, 404);
      return reply({
        ok: true,
        data: {
          task: t, lines: window.__backend.lines[id] || [],
          next_after: 0, has_more: false, oldest_seq: 1, done: t.status !== 'running',
        },
      });
    }
    if (/\/tasks(\?|$)/.test(u)) {
      return reply({ ok: true, data: { tasks: Object.values(window.__backend.tasks), running: 0 } });
    }
    // database.js 的页面数据（用于验证常驻「连接设置」入口）
    if (u.includes('/database')) {
      return reply({ ok: true, data: { connected: true, version: '8.0.36', databases: [], users: [], has_password: true } });
    }
    return reply({ ok: false, msg: '测试后端没有这个路由：' + method + ' ' + u }, false, 404);
  };

  class FakeEventSource {
    constructor(url) {
      this.url = String(url);
      this.listeners = {};
      this.closed = false;
      window.__streams.push(this);
    }
    addEventListener(name, fn) {
      (this.listeners[name] = this.listeners[name] || []).push(fn);
    }
    close() { this.closed = true; }
    fire(name, data) {
      const ev = { data: JSON.stringify(data) };
      for (const fn of (this.listeners[name] || []).slice()) fn(ev);
    }
  }
  FakeEventSource.CONNECTING = 0;
  FakeEventSource.OPEN = 1;
  FakeEventSource.CLOSED = 2;
  window.EventSource = FakeEventSource;
  window.__streamFor = (id) => window.__streams.filter((s) => s.url.includes('/tasks/' + id + '/')).pop();
});

const page1 = await context.newPage();
const page2 = await context.newPage();
const errs = [];
for (const p of [page1, page2]) {
  p.on('console', (m) => errs.push('[' + m.type() + '] ' + m.text()));
  p.on('pageerror', (e) => errs.push('[pageerror] ' + e.message));
}
await page1.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });
await page2.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });

// ---------- 3. 场景：限时输入（list / 倒计时 / 提交 / 落定） ----------
await page1.evaluate(() => {
  const now = Date.now();
  window.__backend.tasks['t-input'] = {
    id: 't-input', kind: 'install', target: 'mysql', title: '安装 MySQL',
    status: 'running', started_at: new Date(now - 5000).toISOString(),
    elapsed_ms: 5000, line_count: 3, last: '==> 正在初始化数据目录',
  };
  window.__backend.tasks['t-timeout'] = {
    id: 't-timeout', kind: 'install', target: 'mysql2', title: '安装 MySQL（超时验证）',
    status: 'running', started_at: new Date(now - 3000).toISOString(),
    elapsed_ms: 3000, line_count: 1, last: '等待 root 口令',
  };
});

const r1 = await page1.evaluate(async () => {
  const { taskCenter } = await import('./tasks.js');

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
  const closeAll = () => {
    for (const b of Array.from(document.querySelectorAll('.modal-head .modal-close'))) b.click();
  };
  const inputEl = () => document.querySelector('.tc-input .tc-input-card input');
  const inputBtn = () => document.querySelector('.tc-input .tc-input-card button.btn-primary');
  const inputErr = () => document.querySelector('.tc-input .tc-input-err');
  const inputNote = () => document.querySelector('.tc-input .tc-input-note');
  const postCount = (id) => window.__calls
    .filter((c) => c.method === 'POST' && c.url.includes('/tasks/' + id + '/input')).length;
  // 只关"任务中心"那个弹窗（列表），别把进度窗一起关了。
  const closeList = () => {
    for (const m of Array.from(document.querySelectorAll('.modal'))) {
      const h3 = m.querySelector('.modal-head h3');
      if (h3 && h3.textContent === '任务中心') m.querySelector('.modal-close').click();
    }
  };

  const out = {};

  // ---- ① 待输入：password 框 + label/hint + 倒计时 + "不填也会继续" ----
  const req1 = {
    key: 'mysql_root_password', label: 'MySQL root 口令',
    hint: '留空或超时＝自动生成强随机口令。口令只写进面板配置（0600）。',
    secret: true, timeout_seconds: 120,
    deadline: new Date(Date.now() + 120000).toISOString(),
  };
  await taskCenter.start({
    kind: 'install', target: 'mysql', title: '安装 MySQL',
    start: async () => ({ task_id: 't-input', title: '安装 MySQL' }),
  });
  const meta1 = Object.assign({}, window.__backend.tasks['t-input'], { input_required: req1 });
  const s1 = window.__streamFor('t-input');
  s1.fire('meta', { task: meta1, oldest_seq: 1 });
  s1.fire('status', meta1);
  await waitFor(inputEl);

  out.type = inputEl() && inputEl().type;
  out.cardText = (document.querySelector('.tc-input .tc-input-card') || {}).innerText || '';
  out.hasCountdown = /剩余\s+\S+/.test((document.querySelector('.tc-input-count') || {}).textContent || '');
  out.placeholder = inputEl() ? inputEl().placeholder : '';

  // 任务中心列表：**正在等输入**的任务要能一眼认出来（此刻 t-input 还没提交）
  taskCenter.openList();
  await waitFor(() => document.querySelector('.tc-row'));
  // 注意取**任务中心**那个弹窗的 body：进度窗也还开着，直接 querySelector('.modal-body')
  // 会拿到进度窗（第一版就栽在这 —— 断言看起来"没找到等待输入"，其实读错了 DOM）。
  const listModal = Array.from(document.querySelectorAll('.modal'))
    .find((m) => (m.querySelector('.modal-head h3') || {}).textContent === '任务中心');
  out.listText = listModal ? listModal.querySelector('.modal-body').innerText : '';
  closeList();

  // ---- ② 提交失败：后端 msg 原样显示，框不消失 ----
  const BACKEND_MSG = '任务已超时（等待输入已结束），本次提交未被接受；任务会按默认值继续';
  window.__inputRules['t-input'] = () => ({ error: BACKEND_MSG, status: 409 });
  const SECRET1 = 'Sup3r-Secret-Pw!';
  inputEl().value = SECRET1;
  inputBtn().click();
  await waitFor(() => inputErr() && inputErr().style.display !== 'none' && inputErr().textContent);
  out.failErrText = inputErr() ? inputErr().textContent : '';
  out.failBoxStillThere = !!inputEl();
  out.failToast = (document.getElementById('toasts') || {}).innerText || '';
  out.failInputValueKept = inputEl() ? inputEl().value : '';
  out.failPostCount = postCount('t-input');

  // ---- ③ 提交成功：**不立刻收框**，等落定事件才收 ----
  delete window.__inputRules['t-input'];
  const SECRET2 = 'An0ther-Secret-Pw!';
  inputEl().value = SECRET2;
  inputBtn().click();
  await waitFor(() => inputBtn() && /已提交/.test(inputBtn().textContent));
  out.afterSubmitBoxStillThere = !!inputEl();
  out.afterSubmitBtnText = inputBtn() ? inputBtn().textContent : '';
  out.afterSubmitFieldCleared = inputEl() ? inputEl().value === '' : false;
  out.afterSubmitPostCount = postCount('t-input');
  out.modalHtmlHasSecret2 = (document.querySelector('.modal') || {}).innerHTML
    ? document.querySelector('.modal').innerHTML.includes(SECRET2) : null;
  // 落定事件：input_required → null
  s1.fire('input_required', { input_required: null, input_result: 'submitted' });
  await waitFor(() => !inputEl());
  out.settledBoxGone = !inputEl();
  out.settledNote = (document.querySelector('.tc-input') || {}).innerText || '';
  closeAll();

  // ---- ④ 倒计时归零：显示"已超时…"，并且不再提交 ----
  const reqTimeout = {
    key: 'mysql_root_password', label: 'MySQL root 口令', hint: '留空＝自动生成',
    secret: true, timeout_seconds: 2,
    deadline: new Date(Date.now() + 1500).toISOString(),
  };
  await taskCenter.start({
    kind: 'install', target: 'mysql2', title: '安装 MySQL（超时验证）',
    start: async () => ({ task_id: 't-timeout', title: '安装 MySQL（超时验证）' }),
  });
  const metaT = Object.assign({}, window.__backend.tasks['t-timeout'], { input_required: reqTimeout });
  const sT = window.__streamFor('t-timeout');
  sT.fire('meta', { task: metaT, oldest_seq: 1 });
  sT.fire('status', metaT);
  await waitFor(inputEl);
  out.timeoutHadCountdown = /剩余/.test((document.querySelector('.tc-input-count') || {}).textContent || '');
  // 归零由 updateStatus 的 1 秒定时器推进，最多 ~3 秒
  await waitFor(() => /已超时/.test((document.querySelector('.tc-input') || {}).innerText || ''), 8000);
  out.timeoutText = (document.querySelector('.tc-input') || {}).innerText || '';
  out.timeoutSubmitDisabled = inputBtn() ? inputBtn().disabled : null;
  const before = postCount('t-timeout');
  inputBtn().click(); // 禁用按钮不会派发 click；即便派发了 submitInput 也会被 expired 挡住
  await new Promise((r) => setTimeout(r, 1200));
  out.timeoutNoSubmit = postCount('t-timeout') === before;
  out.timeoutPostCount = postCount('t-timeout');
  // 真实后端此时也会发落定事件；到了就收框
  sT.fire('input_required', { input_required: null, input_result: 'timeout' });
  await waitFor(() => !inputEl());
  out.timeoutSettledBoxGone = !inputEl();
  out.timeoutSettledNote = (document.querySelector('.tc-input') || {}).innerText || '';
  closeAll();

  out.secret1InUrl = location.href.includes(SECRET1) || location.href.includes(SECRET2);
  out.secretInLocalStorage = JSON.stringify(window.localStorage).includes(SECRET1)
    || JSON.stringify(window.localStorage).includes(SECRET2);
  out.secretInDataAttrs = Array.from(document.querySelectorAll('*')).some((el) => (
    Object.values(el.dataset || {}).some((v) => String(v).includes(SECRET1) || String(v).includes(SECRET2))
  ));
  return out;
});

// ---------- 4. 场景：刷新恢复 + 凭据渲染（走真实刷新路径：init → 列表 → openTask） ----------
const CRED_VALUE = 'Gen3rated-Pa$$-9f';
const DBPASS_VALUE = 'db-Pa$$-word-7x';
const r2 = await page2.evaluate(async ({ CRED_VALUE, DBPASS_VALUE }) => {
  const now = Date.now();
  window.__backend.tasks['t-refresh'] = {
    id: 't-refresh', kind: 'install', target: 'mysql', title: '安装 MySQL（刷新恢复）',
    status: 'running', started_at: new Date(now - 2000).toISOString(),
    elapsed_ms: 2000, line_count: 2, last: '等待 root 口令',
    input_required: {
      key: 'mysql_root_password', label: 'MySQL root 口令', hint: '留空＝自动生成',
      secret: true, timeout_seconds: 120,
      deadline: new Date(now + 120000).toISOString(),
    },
  };
  // 缺陷 D11 的形状：**没有 token / address**，只有 credentials[] 与 db_pass。
  window.__backend.tasks['t-creds'] = {
    id: 't-creds', kind: 'install', target: 'lnmp', title: '一键 LNMP',
    status: 'succeeded', started_at: new Date(now - 60000).toISOString(),
    finished_at: new Date(now).toISOString(), elapsed_ms: 60000, line_count: 40,
    input_result: 'timeout',
    result: {
      app: 'lnmp', message: '装好了',
      credentials: [
        { key: 'mysql_root_password', label: 'MySQL root 口令（等待输入超时，已自动生成强随机口令）', value: CRED_VALUE },
      ],
      db_pass: DBPASS_VALUE,
      warning: 'root 口令已设置成功，但写入面板配置失败；请立刻复制本次任务结果里的一次性凭据',
    },
  };
  window.__backend.tasks['t-site'] = {
    id: 't-site', kind: 'site-install', target: 'blog.example.com', title: '一键建站 WordPress',
    status: 'succeeded', started_at: new Date(now - 30000).toISOString(),
    finished_at: new Date(now).toISOString(), elapsed_ms: 30000, line_count: 10,
    result: { app: 'wordpress', domain: 'blog.example.com', db_pass: DBPASS_VALUE },
  };

  const { taskCenter } = await import('./tasks.js');
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
  const closeAll = () => {
    for (const b of Array.from(document.querySelectorAll('.modal-head .modal-close'))) b.click();
  };

  const out = {};
  // 刷新后的真实顺序：app.js 会先 taskCenter.init()（= GET /tasks），再按需 openTask。
  await taskCenter.init();
  await taskCenter.openTask('t-refresh');
  await waitFor(() => document.querySelector('.tc-input .tc-input-card input'));
  out.refreshBox = !!document.querySelector('.tc-input .tc-input-card input');
  out.refreshType = (document.querySelector('.tc-input .tc-input-card input') || {}).type;
  out.refreshLabel = (document.querySelector('.tc-input .tc-input-card') || {}).innerText || '';
  out.refreshCountdown = (document.querySelector('.tc-input-count') || {}).textContent || '';
  closeAll();

  // 凭据渲染（走 REST 详情路径）
  await taskCenter.openTask('t-creds');
  await waitFor(() => document.querySelector('.tc-result-card'));
  const card = document.querySelector('.tc-result-card');
  out.credCardText = card ? card.innerText : '';
  out.credValues = Array.from(document.querySelectorAll('.tc-cred-value')).map((e) => e.textContent);
  out.credCopyButtons = Array.from(document.querySelectorAll('.tc-result-card button.tc-copy'))
    .map((b) => b.textContent);
  out.credHasTokenOrAddress = /token|共享密钥|访问地址/.test(out.credCardText);
  // 复制降级：剪贴板被拒时必须选中文本，而不是静默失败
  window.__origClipboard = navigator.clipboard;
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: () => Promise.reject(new Error('denied for test')) },
  });
  const firstCopy = document.querySelector('.tc-result-card button.tc-copy');
  firstCopy.click();
  await waitFor(() => (window.getSelection() || {}).toString());
  out.selectedText = (window.getSelection() || {}).toString();
  out.toastText = (document.getElementById('toasts') || {}).innerText || '';
  closeAll();

  // 一键建站的 db_pass：即使没有 credentials[] 也要渲染
  await taskCenter.openTask('t-site');
  await waitFor(() => document.querySelector('.tc-result-card'));
  out.siteCardText = (document.querySelector('.tc-result-card') || {}).innerText || '';
  closeAll();

  // 安全：值不进 data 属性 / localStorage / URL
  const secrets = [CRED_VALUE, DBPASS_VALUE];
  out.secretInDataAttrs = Array.from(document.querySelectorAll('*')).some((el) => (
    Object.values(el.dataset || {}).some((v) => secrets.some((s) => String(v).includes(s)))
  ));
  out.secretInLocalStorage = JSON.stringify(window.localStorage).includes(CRED_VALUE)
    || JSON.stringify(window.localStorage).includes(DBPASS_VALUE);
  out.secretInUrl = location.href.includes(CRED_VALUE) || location.href.includes(DBPASS_VALUE);
  return out;
}, { CRED_VALUE, DBPASS_VALUE });

// ---------- 4b. 场景：数据库页的常驻「连接设置」入口（安装时一次性口令的回填点） ----------
const page3 = await context.newPage();
page3.on('console', (m) => errs.push('[' + m.type() + '] ' + m.text()));
page3.on('pageerror', (e) => errs.push('[pageerror] ' + e.message));
await page3.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });
const r3 = await page3.evaluate(async () => {
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
  const box = document.createElement('div');
  document.getElementById('root').appendChild(box);
  DatabaseView(box, {});
  await waitFor(() => document.querySelector('#db-conn-settings'));
  const out = {};
  const btn = document.querySelector('#db-conn-settings');
  out.btnText = btn ? btn.textContent : '';
  out.btnShownWhenConnected = !!btn;
  if (btn) btn.click();
  await waitFor(() => Array.from(document.querySelectorAll('.modal-head h3'))
    .some((h) => h.textContent === 'MySQL 连接设置'));
  const m = Array.from(document.querySelectorAll('.modal'))
    .find((x) => (x.querySelector('.modal-head h3') || {}).textContent === 'MySQL 连接设置');
  out.modalTitle = m ? m.querySelector('.modal-head h3').textContent : '';
  out.modalText = m ? m.querySelector('.modal-body').innerText : '';
  out.fieldCount = m ? m.querySelectorAll('.modal-body input').length : 0;
  const pw = m ? m.querySelector('.modal-body input[type=password]') : null;
  out.pwPrefilled = pw ? pw.value : '';
  return out;
});

// ---------- 5. 打印 + 断言 ----------
console.log('══════════ ① 待输入状态（password 框 / label / hint / 倒计时）══════════');
console.log(`  input type           : ${r1.type}`);
console.log(`  placeholder          : ${r1.placeholder}`);
console.log(`  卡片文案             : ${(r1.cardText || '').replace(/\n/g, ' / ')}`);
console.log(`  有倒计时             : ${r1.hasCountdown ? '是' : '否'}`);

console.log('\n══════════ ② 提交失败（后端 msg 原样显示、框不消失）══════════');
console.log(`  输入框错误文案       : ${r1.failErrText}`);
console.log(`  toast                : ${(r1.failToast || '').replace(/\n/g, ' ')}`);
console.log(`  框还在？             : ${r1.failBoxStillThere ? '在 ✓' : '没了 ✗'}`);
console.log(`  输入值保留？         : ${r1.failInputValueKept ? '保留 ✓' : '丢了'}`);
console.log(`  POST /input 次数     : ${r1.failPostCount}`);

console.log('\n══════════ ③ 提交成功：不立刻收框，等落定事件 ══════════');
console.log(`  提交后框还在？       : ${r1.afterSubmitBoxStillThere ? '在 ✓' : '没了 ✗'}`);
console.log(`  按钮文案             : ${r1.afterSubmitBtnText}`);
console.log(`  输入框已清空？       : ${r1.afterSubmitFieldCleared ? '是 ✓' : '否'}`);
console.log(`  modal HTML 含口令？  : ${r1.modalHtmlHasSecret2 ? '含 ✗' : '不含 ✓'}`);
console.log(`  落定后收框？         : ${r1.settledBoxGone ? '收了 ✓' : '没收 ✗'}`);
console.log(`  落定说明             : ${(r1.settledNote || '').replace(/\n/g, ' / ')}`);

console.log('\n══════════ ④ 倒计时归零：不再提交 ══════════');
console.log(`  归零文案             : ${(r1.timeoutText || '').replace(/\n/g, ' / ')}`);
console.log(`  提交按钮禁用？       : ${r1.timeoutSubmitDisabled ? '是 ✓' : '否 ✗'}`);
console.log(`  归零后 POST 次数     : ${r1.timeoutPostCount}（未增加=${r1.timeoutNoSubmit ? '是 ✓' : '否 ✗'}）`);
console.log(`  落定后收框？         : ${r1.timeoutSettledBoxGone ? '收了 ✓' : '没收 ✗'}`);
console.log(`  落定说明             : ${(r1.timeoutSettledNote || '').replace(/\n/g, ' / ')}`);

console.log('\n══════════ ⑤ 任务中心列表能认出"等待输入" ══════════');
console.log(`  ${(r1.listText || '').split('\n').filter((l) => l.includes('等待输入') || l.includes('MySQL')).join(' | ') || '（没找到）'}`);

console.log('\n══════════ ⑥ 刷新恢复（init → 列表 → openTask）══════════');
console.log(`  待输入框恢复？       : ${r2.refreshBox ? '恢复 ✓' : '没有 ✗'}`);
console.log(`  type                 : ${r2.refreshType}`);
console.log(`  倒计时               : ${r2.refreshCountdown}`);
console.log(`  卡片文案             : ${(r2.refreshLabel || '').replace(/\n/g, ' / ')}`);

console.log('\n══════════ ⑦ 凭据渲染（缺陷 D11：没有 token/address 也要显示）══════════');
console.log(`  结果卡片文案         : ${(r2.credCardText || '').replace(/\n/g, ' / ')}`);
console.log(`  凭据 value 节点      : ${JSON.stringify(r2.credValues)}`);
console.log(`  复制按钮             : ${JSON.stringify(r2.credCopyButtons)}`);
console.log(`  剪贴板被拒后选中     : ${r2.selectedText}`);
console.log(`  toast                : ${(r2.toastText || '').replace(/\n/g, ' ')}`);
console.log(`  站点 db_pass 文案    : ${(r2.siteCardText || '').replace(/\n/g, ' / ')}`);

console.log('\n══════════ ⑧ 安全：值不进 URL / localStorage / data 属性 / console ══════════');
console.log(`  page1：URL=${r1.secret1InUrl ? '含 ✗' : '不含 ✓'} localStorage=${r1.secretInLocalStorage ? '含 ✗' : '不含 ✓'} data 属性=${r1.secretInDataAttrs ? '含 ✗' : '不含 ✓'}`);
console.log(`  page2：URL=${r2.secretInUrl ? '含 ✗' : '不含 ✓'} localStorage=${r2.secretInLocalStorage ? '含 ✗' : '不含 ✓'} data 属性=${r2.secretInDataAttrs ? '含 ✗' : '不含 ✓'}`);

console.log('\n══════════ ⑨ 数据库页常驻「连接设置」入口（一次性口令的回填点）══════════');
console.log(`  入口按钮             : ${r3.btnText}`);
console.log(`  弹窗标题             : ${r3.modalTitle}`);
console.log(`  表单字段数           : ${r3.fieldCount}`);
console.log(`  口令预填             : ${r3.pwPrefilled}`);
console.log(`  弹窗文案             : ${(r3.modalText || '').replace(/\n/g, ' / ')}`);

const checks = [];
const check = (label, cond, extra = '') => checks.push({ label, ok: !!cond, extra });

// ①
check('待输入时用 type=password', r1.type === 'password', r1.type);
check('卡片显示 label', (r1.cardText || '').includes('MySQL root 口令'));
check('卡片显示 hint', (r1.cardText || '').includes('自动生成强随机口令'));
check('显示留空＝自动生成', (r1.cardText || '').includes('不填也没关系'));
check('以 deadline 为准的倒计时在走', r1.hasCountdown);
// ②
check('提交失败：后端 msg 原样显示', r1.failErrText === '任务已超时（等待输入已结束），本次提交未被接受；任务会按默认值继续', r1.failErrText);
check('提交失败：toast 也是后端 msg', (r1.failToast || '').includes('任务已超时（等待输入已结束）'));
check('提交失败：输入框不消失', r1.failBoxStillThere);
check('提交失败：保留用户输入，不偷偷清空', r1.failInputValueKept === 'Sup3r-Secret-Pw!');
check('提交失败：确实发出了一次 POST', r1.failPostCount === 1, String(r1.failPostCount));
// ③
check('提交成功：不立刻收框', r1.afterSubmitBoxStillThere);
check('提交成功：按钮变成"已提交，等待任务继续…"', /已提交/.test(r1.afterSubmitBtnText || ''), r1.afterSubmitBtnText);
check('提交成功：输入框里的口令被清掉', r1.afterSubmitFieldCleared);
check('提交成功：modal HTML 里不残留口令', r1.modalHtmlHasSecret2 === false);
check('落定事件（input_required→null）后收框', r1.settledBoxGone);
check('落定后说明是"已提交"', /已提交/.test(r1.settledNote || ''), r1.settledNote);
// ④
check('倒计时归零：显示"已超时"', /已超时/.test(r1.timeoutText || ''), r1.timeoutText);
check('倒计时归零：明说会自动生成并继续', /自动生成并继续/.test(r1.timeoutText || ''));
check('倒计时归零：明说不需要刷新页面', /不需要刷新页面/.test(r1.timeoutText || ''));
check('倒计时归零：提交按钮禁用', r1.timeoutSubmitDisabled === true);
check('倒计时归零：不再提交（POST 次数不增加）', r1.timeoutNoSubmit, String(r1.timeoutPostCount));
check('超时落定后收框', r1.timeoutSettledBoxGone);
check('超时落定后说明会自动继续', /自动生成并继续/.test(r1.timeoutSettledNote || ''), r1.timeoutSettledNote);
// ⑤
check('任务中心列表标出"等待输入"', /等待输入/.test(r1.listText || ''));
// ⑥
check('刷新后重新拉详情仍能看到待输入', r2.refreshBox);
check('刷新恢复后仍是 password 框', r2.refreshType === 'password', r2.refreshType);
check('刷新恢复后倒计时在走', /剩余/.test(r2.refreshCountdown || ''), r2.refreshCountdown);
// ⑦
check('没有 token/address 也渲染凭据区块', !!r2.credCardText);
check('凭据 label 渲染出来', (r2.credCardText || '').includes('MySQL root 口令（等待输入超时'));
check('凭据 value 用等宽节点渲染', (r2.credValues || []).includes(CRED_VALUE), JSON.stringify(r2.credValues));
check('凭据有"复制"按钮', (r2.credCopyButtons || []).some((t) => t.includes('复制')), JSON.stringify(r2.credCopyButtons));
check('剪贴板被拒时降级为选中文本', r2.selectedText === CRED_VALUE, r2.selectedText);
check('降级时 toast 说明"已选中文本"', /已选中文本/.test(r2.toastText || ''), r2.toastText);
check('一键建站 db_pass 也显示', (r2.siteCardText || '').includes(DBPASS_VALUE), r2.siteCardText);
check('db_pass 说明是数据库口令', (r2.siteCardText || '').includes('数据库口令'));
check('db_pass 说明在站点配置文件里也能找到', /wp-config\.php/.test(r2.siteCardText || ''));
// ⑧
check('page1 值不进 URL', !r1.secret1InUrl);
check('page1 值不进 localStorage', !r1.secretInLocalStorage);
check('page1 值不进 DOM data 属性', !r1.secretInDataAttrs);
check('page2 凭据不进 data 属性', !r2.secretInDataAttrs);
check('page2 凭据不进 localStorage', !r2.secretInLocalStorage);
check('page2 凭据不进 URL', !r2.secretInUrl);
// ⑨
check('数据库页有常驻「连接设置」入口（连得上时也在）', r3.btnShownWhenConnected && /连接设置/.test(r3.btnText || ''), r3.btnText);
check('「连接设置」弹窗能打开', r3.modalTitle === 'MySQL 连接设置', r3.modalTitle);
check('弹窗含主机/端口/Socket/用户名/密码字段', (r3.fieldCount || 0) >= 5, String(r3.fieldCount));
check('弹窗带出面板当前口令（供核对/更正）', r3.pwPrefilled === 'pw-from-config', r3.pwPrefilled);
check('弹窗说明安装时的一次性口令填在这里', /一次性口令/.test(r3.modalText || ''), r3.modalText);

// console 里不许出现值。这里断言的是"页面里真正出现过的口令"没被 console.log 出来。
const leaked = errs.filter((m) => m.includes(CRED_VALUE) || m.includes(DBPASS_VALUE)
  || m.includes('Sup3r-Secret-Pw!') || m.includes('An0ther-Secret-Pw!'));
check('console 里没有出现口令', leaked.length === 0, leaked.join(' | '));
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
