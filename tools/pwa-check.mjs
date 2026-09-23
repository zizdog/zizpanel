// pwa-check.mjs —— PWA / 手机一屏（C4）的真浏览器自检（本机自用）。
//
// 为什么必须用真浏览器：manifest 的 start_url / scope、图标能不能解码、Service Worker
// 能不能注册 **都只有浏览器说了算** —— Go 单测只能证明"我们生成了这些字节"。
//
// 依赖：先 `make run-local`（调试实例，根目录 /tmp/zizpanel-dev）。
// 用法：node tools/pwa-check.mjs http://127.0.0.1:18443/dev
//
// 断言的六件事：
//   ① manifest 链接存在且地址带面板入口（安全后缀）—— 写死相对路径会 404；
//   ② 浏览器真的取到了 manifest，start_url/scope/display 与入口一致；
//   ③ 192/512 图标都能取到，且是**能解码的真 PNG**（不是 404 页面冒充图片）；
//   ④ Service Worker 注册成功并 activated（离线壳的前提）；
//   ⑤ 「手机一屏」在手机视口下渲染出状态（CPU/内存/磁盘）与常用操作；
//   ⑥ 出口码非 0 表示有断言失败（可接进 CI/门禁）。
import { chromium, devices } from 'playwright';

const BASE = (process.argv[2] || 'http://127.0.0.1:18443/dev').replace(/\/+$/, '');
// 面板入口（必须带结尾斜杠）：manifest/图标/SW 作用域都是它。
// 注意 new URL('http://h/dev').pathname === '/dev'（没有结尾斜杠），要用 BASE + '/'。
const ENTRY = new URL(BASE + '/').pathname; // 例如 /dev/
const PASS = 'zizpanel-test-fixture-pass';

// 每一步都带超时：`navigator.serviceWorker.ready` 在注册失败时**永不 resolve**
// （踩过：脚本挂死、什么也不输出），所以任何 await 浏览器的地方都必须有上限。
async function withTimeout(promise, ms, label) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, rej) => { timer = setTimeout(() => rej(new Error(label + ' 超时（' + ms + 'ms）')), ms); }),
    ]);
  } finally { clearTimeout(timer); }
}

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
}

const browser = await chromium.launch({ args: ['--disable-dev-shm-usage', '--disable-gpu'] });
const ctx = await browser.newContext({ ...devices['iPhone 13'] });
const page = await ctx.newPage();
page.on('pageerror', (e) => console.log('PAGEERROR', e.message));

// ---- 登录（调试实例每次都是全新的：首屏是初始化表单，所有密码框都要填）----
console.log('→ 打开', BASE + '/');
await withTimeout(page.goto(BASE + '/'), 30000, '打开面板');
await page.waitForTimeout(1200);
const pws = page.locator('input[type=password]');
if (await pws.count()) {
  const userBox = page.locator('input[type=text]').first();
  if (await userBox.count()) await userBox.fill('admin').catch(() => {});
  for (let i = 0; i < await pws.count(); i++) await pws.nth(i).fill(PASS);
  await pws.last().press('Enter');
  await page.waitForTimeout(3500);
}
const bodyText = await page.locator('body').innerText();
// 判据用"侧栏出现 + 初始化表单消失"：侧栏页脚本身就有「登录账号 admin」字样，
// 拿它当否定判据会假失败（踩过）。
check('登录后进入面板（不是初始化表单）', /仪表盘/.test(bodyText) && !/创建管理员并进入面板|设置管理员/.test(bodyText),
  bodyText.replace(/\n+/g, ' | ').slice(0, 120));

// ---- ① manifest 链接 ----
const manifestHref = await page.evaluate(() => {
  const el = document.querySelector('link[rel="manifest"]');
  return el ? el.getAttribute('href') : null;
});
check('页面里有 manifest 链接', !!manifestHref, String(manifestHref));
check('manifest 地址带面板入口（安全后缀）', !!manifestHref && new URL(manifestHref, page.url()).pathname === ENTRY + 'manifest.webmanifest',
  String(manifestHref));

// ---- ② 浏览器真的取到 manifest ----
const man = await page.evaluate(async (href) => {
  try {
    const res = await fetch(href);
    if (!res.ok) return { error: 'HTTP ' + res.status };
    return { ct: res.headers.get('content-type'), json: await res.json() };
  } catch (e) { return { error: String(e) }; }
}, manifestHref);
check('manifest 可被浏览器取到且是 JSON', !man.error && !!man.json, man.error || String(man.ct));
if (man.json) {
  check('start_url / scope 指向面板入口', man.json.start_url === ENTRY && man.json.scope === ENTRY,
    `start_url=${man.json.start_url} scope=${man.json.scope} 期望=${ENTRY}`);
  check('display=standalone（加主屏后全屏）', man.json.display === 'standalone', String(man.json.display));
  check('manifest 带 192 与 512 图标', Array.isArray(man.json.icons) && man.json.icons.length >= 2,
    JSON.stringify(man.json.icons));
}

// ---- ③ 图标是真的 PNG ----
const iconProbe = await page.evaluate(async (entry) => {
  const out = [];
  for (const size of [192, 512]) {
    const url = entry + 'pwa/icon-' + size + '.png';
    try {
      const res = await fetch(url);
      const buf = await res.arrayBuffer();
      const b = new Uint8Array(buf.slice(0, 8));
      // PNG magic：89 50 4E 47 0D 0A 1A 0A
      const magic = b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47;
      out.push({ size, status: res.status, ct: res.headers.get('content-type'), bytes: buf.byteLength, magic });
    } catch (e) { out.push({ size, error: String(e) }); }
  }
  return out;
}, ENTRY);
for (const ic of iconProbe) {
  check(`${ic.size} 图标是真 PNG 且够大`, !ic.error && ic.status === 200 && ic.magic && ic.bytes > 500,
    ic.error || `status=${ic.status} ct=${ic.ct} bytes=${ic.bytes} magic=${ic.magic}`);
}

// ---- ④ Service Worker ----
const sw = await withTimeout(page.evaluate(async () => {
  try {
    const reg = await Promise.race([
      navigator.serviceWorker.ready,
      new Promise((_, rej) => setTimeout(() => rej(new Error('ready 未 resolve')), 15000)),
    ]);
    return { scope: reg.scope, state: reg.active && reg.active.state, script: reg.active && reg.active.scriptURL };
  } catch (e) { return { error: String(e) }; }
}), 25000, '读取 Service Worker 状态').catch((e) => ({ error: String(e) }));
check('Service Worker 已注册并 activated', !sw.error && sw.state === 'activated',
  sw.error || `scope=${sw.scope} state=${sw.state}`);
check('SW 作用域是面板入口（不是 /js/ 这种子目录）', !sw.error && sw.scope && new URL(sw.scope).pathname === ENTRY,
  String(sw.scope));

// ---- ⑤ 手机一屏 ----
console.log('→ 打开手机一屏');
await withTimeout(page.goto(BASE + '/#/mobile'), 30000, '打开手机一屏');
await page.waitForTimeout(2500);
const mobileText = (await page.locator('body').innerText()).replace(/\n+/g, ' | ');
check('手机一屏渲染出状态（CPU/内存/磁盘）',
  /CPU/.test(mobileText) && /内存/.test(mobileText) && /磁盘/.test(mobileText), mobileText.slice(0, 160));
check('手机一屏有常用操作（重启 Nginx）', /重启 Nginx/.test(mobileText), mobileText.slice(0, 200));
check('手机一屏没有报错文案（读取…失败）', !/读取(系统状态|服务列表)失败/.test(mobileText), mobileText.slice(0, 200));

// ---- 离线壳 ----
// 判据分两步，**分开如实报告**：
//   ① 缓存里真的有入口页（离线能打开的前提）—— 这是 Service Worker 自己写进去的；
//   ② 真断网导航能不能开 —— 受 Playwright 的网络模拟影响，只报告不当作通过条件。
const cached = await page.evaluate(async () => {
  const names = await caches.keys();
  const out = {};
  for (const n of names) {
    const c = await caches.open(n);
    out[n] = (await c.keys()).map((r) => new URL(r.url).pathname);
  }
  return out;
});
const shellCache = Object.entries(cached).find(([n]) => n.startsWith('zpanel-shell-'));
const cachedPaths = shellCache ? shellCache[1] : [];
check('离线壳缓存里有入口页与 index.html',
  cachedPaths.includes(ENTRY) && cachedPaths.includes(ENTRY + 'index.html'),
  JSON.stringify(cached));

// ② 离线壳：只报告**前提**（缓存里真有入口页），不声称"断网可用"。
//
// 为什么不把"断网后仍能打开"当判据：这个 Playwright/Chromium 环境对 Service Worker
// 的行为不可靠 —— 同一个探针（SW 里回一个常量）时而返回结果、时而 Failed to fetch；
// context.setOffline 与 page.route().abort() 两种断网方式都会在请求进入 SW 之前就拒掉它。
// 所以这里只做能重复观察到的两件事：SW 已 activated 且作用域正确（上面已断言）、
// 缓存里有入口页与 index.html（Service Worker 自己在 install 阶段写进去的）。
// **离线可用性未验证**，如实标出，不当成通过条件。
await page.route(new RegExp('\\' + ENTRY + '(index\\.html)?$'), (route) => route.abort());
let offlineNote = '未验证（本环境对 SW 断网行为的模拟不可靠）';
try {
  const res = await page.evaluate((entry) => fetch(entry + 'index.html').then((r) => r.status).catch((e) => String(e)), ENTRY);
  offlineNote = `未验证（掐断网络后页面请求返回 ${res}：本环境无法稳定模拟"SW 离线应答"）`;
} catch (e) {
  offlineNote = '未验证（' + String(e.message || e).split('\n')[0] + '）';
} finally {
  await page.unroute(new RegExp('\\' + ENTRY + '(index\\.html)?$')).catch(() => {});
}
console.log('ℹ️ 离线可用性：' + offlineNote);

await browser.close();
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} 项通过`);
if (failed.length) {
  console.log('失败项：' + failed.map((f) => f.name).join('；'));
  process.exit(1);
}
