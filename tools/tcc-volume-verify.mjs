// tcc-volume-verify.mjs —— 验收「外接卷被 macOS 隐私保护（TCC）拒绝」这条链路的 UI。
//
// 为什么这么搭（照 tasks-input-verify.mjs / certs-verify.mjs 的样子）：
//   · 真 acorn（tools/check-js-syntax.mjs）只能证明语法合法；
//   · 真机调试实例（make run-local）跑在**用户会话**里，读 /Volumes 不会被拒 ——
//     **这条路径在调试实例上永远复现不出来**（这正是当初的验证盲区）。
//   所以这里加载**真实的** files.js / disks.js（一个字符都没为测试改过），
//   用 Playwright 的 route 拦截伪造后端的 403 + 指引响应，断言页面真的把指引显示出来。
//
// 覆盖：
//   ① 文件管理拿到 403+指引 → 按行显示（不是一行 errno），含"挂载到自定义挂载点"解法；
//   ② 磁盘页有「外接盘/卷」静态提示；
//   ③ 「挂载到自定义挂载点…」按钮 → 弹窗预填 <安装根>/mnt/<卷名> → 提交后显示**回读**挂载点；
//   ④ 全程 0 控制台错误。
//
// 用法： export PATH=/opt/homebrew/bin:$PATH; node tools/tcc-volume-verify.mjs
import { chromium } from 'playwright';
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const JS_DIR = path.join(ROOT, 'internal/web/assets/js');
const APP_CSS = path.join(ROOT, 'internal/web/assets/app.css');
const SHOT_DIR = '/tmp/zp-tcc-shots';

// 后端 volumeTCCGuide() 的真实文案（internal/web/api_files.go）。
// 这里刻意只断言**关键指引**，文案细节由 Go 测试锁死。
const GUIDE = [
  'macOS 隐私保护拦住了对外接卷 /Volumes/ZPMirror 的访问：operation not permitted。'
    + '面板以 root 的 LaunchDaemon 运行、没有用户会话，读写 /Volumes 下的外接盘会被系统拒绝。',
  '解法：用面板「磁盘 → 挂载到自定义挂载点…」把这个卷挂到 /Volumes 之外，再访问那个路径（挂载本身不受隐私保护限制）。等价命令：',
  '  sudo diskutil mount -mountPoint /opt/zizpanel/mnt/mirror <卷标识>',
].join('\n');

const CUSTOM_BASE = '/opt/zizpanel/mnt';

// ---------- 1. 静态服务器（ESM 必须走 http，file:// 会被 CORS 挡） ----------
const MIME = { '.js': 'text/javascript; charset=utf-8', '.html': 'text/html; charset=utf-8' };
// app.js 是应用外壳（会 boot、拉会话、渲染登录页）。这里只验证真实存在的页面模块，
// 所以把 app.js 换成最小替身（files.js 只从它取 registerCleanup）。
const APP_STUB = `
export const state = { session: null, metrics: null, route: '' };
export const NAV = [];
export function panelPath(sub = '') { return '/' + String(sub).replace(/^\\/+/, ''); }
export function registerCleanup() { return () => {}; }
`;
const server = http.createServer((req, res) => {
  const rel = decodeURIComponent(new URL(req.url, 'http://x').pathname).replace(/^\/+/, '');
  if (rel === '' || rel === 'index.html') {
    res.writeHead(200, { 'Content-Type': MIME['.html'] });
    // 带上真实样式表，截图才和面板里看到的一致（不是裸 DOM）。
    res.end('<!doctype html><html><head><meta charset="utf-8">'
      + '<link rel="stylesheet" href="app.css"></head>'
      + '<body><div id="root"></div><div id="toasts"></div></body></html>');
    return;
  }
  if (rel === 'app.css') {
    res.writeHead(200, { 'Content-Type': 'text/css; charset=utf-8' });
    res.end(fs.readFileSync(APP_CSS));
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

fs.mkdirSync(SHOT_DIR, { recursive: true });

// ---------- 2. 假后端（route 拦截，不碰真实面板） ----------
const disksSnapshot = {
  is_root: true,
  fstab_path: '/etc/fstab',
  fstab_readable: false,
  fstab_error: '测试替身没有真实 fstab',
  notes: [],
  custom_mount_base: CUSTOM_BASE,
  filesystems: [],
  protected: [],
  list: [{
    disk: {
      id: 'disk4', volume_name: 'ZPMirror', model: 'Test SSD', bus_protocol: 'USB',
      size_bytes: 1024 * 1024 * 1024 * 1024, whole_disk: true, internal: false,
      external: true, removable: true, encrypted_known: true, encrypted: false,
      smart_status: 'verified', system_disk: false, mountable: true, mounted: false,
    },
    partitions: [{
      id: 'disk4s1', volume_name: 'ZPMirror', filesystem: 'apfs', content: 'Apple_APFS',
      size_bytes: 1024 * 1024 * 1024 * 1024, mounted: false, mount_point: '',
      external: true, removable: true, system_disk: false, locked: false,
      uuid: 'CCCC1111-0000-0000-0000-000000000011', has_filesystem: true,
      apfs_volume: true, encrypted: false, encrypted_known: true, auto_mount: false,
    }],
  }],
};

const browser = await chromium.launch();
const context = await browser.newContext();
const page = await context.newPage();
const errs = [];
// 故意伪造的 403 会让浏览器自己打一条 "Failed to load resource" —— 预期内，不是页面 JS 错误。
const isExpectedNetLog = (t) => /Failed to load resource/.test(t);
page.on('console', (m) => { if (m.type() === 'error' && !isExpectedNetLog(m.text())) errs.push('[console] ' + m.text()); });
page.on('pageerror', (e) => errs.push('[pageerror] ' + e.message));

let mountPosts = 0;
await context.route('**/api/v1/system/disks**', (route) => {
  const req = route.request();
  const u = new URL(req.url());
  const reply = (data, status = 200) => route.fulfill({
    status, contentType: 'application/json; charset=utf-8',
    body: JSON.stringify(status === 200 ? { ok: true, data } : { ok: false, msg: data }),
  });
  if (req.method() === 'POST' && /\/system\/disks\/[^/]+\/mount$/.test(u.pathname)) {
    mountPosts++;
    const body = JSON.parse(req.postData() || '{}');
    const mp = String(body.mount_point || '');
    return reply({
      id: 'disk4s1', action: 'mount', verified: true, mounted: true, mount_point: mp,
      command: 'diskutil mount -mountPoint ' + mp + ' disk4s1', stdout: 'Volume disk4s1 mounted',
      stderr: '', message: '已挂载到 ' + mp,
    });
  }
  return reply(disksSnapshot);
});
await context.route('**/api/v1/files**', (route) => route.fulfill({
  status: 403, contentType: 'application/json; charset=utf-8',
  body: JSON.stringify({ ok: false, msg: GUIDE }),
}));

// ---------- 3. 文件管理：403 + 多行指引 ----------
await page.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });
await page.evaluate(async () => {
  const mod = await import('./files.js');
  mod.FilesView(document.getElementById('root'), {});
});
await page.waitForSelector('.files-denied', { timeout: 8000 });
const filesOut = await page.evaluate(() => {
  const box = document.querySelector('.files-denied');
  const root = document.getElementById('root');
  return {
    head: (root.querySelector('.empty h4') || {}).textContent || '',
    text: box.innerText,
    lines: Array.from(box.querySelectorAll('.files-denied-line')).length,
    disksBtn: Array.from(box.querySelectorAll('button')).some((b) => b.textContent.includes('磁盘工具')),
    jsonOneLine: (root.querySelector('.empty p') || {}).textContent || '',
  };
});
await page.screenshot({ path: path.join(SHOT_DIR, 'files-tcc-403.png'), fullPage: true });

// ---------- 4. 磁盘页：静态提示 + 挂载到自定义挂载点 ----------
const page2 = await context.newPage();
page2.on('console', (m) => { if (m.type() === 'error' && !isExpectedNetLog(m.text())) errs.push('[console] ' + m.text()); });
page2.on('pageerror', (e) => errs.push('[pageerror] ' + e.message));
// 同一 context 的路由对 page2 也生效。
await page2.goto(base + 'index.html', { waitUntil: 'domcontentloaded' });
await page2.evaluate(async () => {
  const mod = await import('./disks.js');
  mod.DisksView(document.getElementById('root'), {});
});
await page2.waitForSelector('.disk-card', { timeout: 8000 });

const hintText = await page2.evaluate(() => document.getElementById('root').innerText);
const mountBtn = page2.locator('button', { hasText: '挂载到自定义挂载点…' }).first();
await mountBtn.click();
await page2.waitForSelector('.modal input.input', { timeout: 8000 });
const dlg = await page2.evaluate(() => {
  const input = document.querySelector('.modal input.input');
  const body = document.querySelector('.modal .modal-body').innerText;
  return { value: input ? input.value : '', body };
});
await page2.screenshot({ path: path.join(SHOT_DIR, 'disks-custom-mount-dialog.png'), fullPage: true });

// 提交 → 断言页面显示**回读**的挂载点（持久结果卡片）。
await page2.locator('.modal .modal-foot button.btn-primary').click();
await page2.waitForSelector('.disk-task-result', { timeout: 8000 });
const resultText = await page2.evaluate(() => document.querySelector('.disk-task-result').innerText);
await page2.screenshot({ path: path.join(SHOT_DIR, 'disks-custom-mount-result.png'), fullPage: true });

await browser.close();
server.close();

// ---------- 5. 断言 ----------
const problems = [];
const must = (cond, msg) => { if (!cond) problems.push(msg); };
must(filesOut.text.includes('隐私保护'), '文件管理页没有显示「隐私保护」四个字');
must(filesOut.text.includes('挂载到自定义挂载点'), '文件管理页没有显示「挂载到自定义挂载点」解法');
must(filesOut.text.includes('/Volumes/ZPMirror'), '文件管理页没有显示被拒绝的路径');
must(filesOut.lines >= 2, `指引必须按行显示（不是一行 errno），实际 ${filesOut.lines} 行`);
must(filesOut.head.includes('被系统拒绝'), `403 时应提示"被系统拒绝"，实际 ${filesOut.head}`);
must(!filesOut.text.includes('完全磁盘访问权限'), '指引里不得出现人工授权（完全磁盘访问权限）');
must(filesOut.disksBtn, '指引旁应有「打开磁盘工具…」入口');
must(hintText.includes('外接盘/卷'), '磁盘页缺少「外接盘/卷」静态提示');
must(hintText.includes(CUSTOM_BASE), '磁盘页静态提示应带自定义挂载点前缀 ' + CUSTOM_BASE);
must(dlg.value === CUSTOM_BASE + '/ZPMirror', `弹窗应预填 ${CUSTOM_BASE}/ZPMirror，实际 ${dlg.value}`);
must(dlg.body.includes('-mountPoint'), '弹窗应显示将执行的 diskutil mount -mountPoint 命令');
must(mountPosts === 1, `应只提交一次挂载请求，实际 ${mountPosts}`);
must(resultText.includes('回读确认'), `结果卡片应显示回读确认，实际：${resultText}`);
must(resultText.includes(CUSTOM_BASE + '/ZPMirror'), `结果卡片应显示回读的挂载点，实际：${resultText}`);
must(errs.length === 0, '控制台错误：' + errs.join(' | '));

console.log('\n══════ 外接卷 TCC 错误映射（前端） ══════');
console.log(`文件管理 403 指引行数：${filesOut.lines}`);
console.log(`文件管理指引首行：${filesOut.head}`);
console.log(`  ${filesOut.text.split('\n').join('\n  ')}`);
console.log(`磁盘页静态提示：${hintText.includes('外接盘/卷') ? '有' : '没有'}`);
console.log(`弹窗预填挂载点：${dlg.value}`);
console.log(`挂载 POST 次数：${mountPosts}`);
console.log(`结果卡片：${resultText.replace(/\n/g, ' / ')}`);
console.log(`截图目录：${SHOT_DIR}`);
if (problems.length) {
  console.log('\n✗ 失败：');
  problems.forEach((p) => console.log('  - ' + p));
  process.exit(1);
}
console.log('\n✓ 全部通过（真实 root 面板 + 外接盘下的 EPERM 未在本会话复现）');
