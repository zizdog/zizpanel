// tools/ui-audit.mjs —— 面板 UI 的**可重复客观检查**。
//
// 为什么要这个脚本（用户 2026-09-22："整个 zizpanel 项目的 UI 又生硬又难看。
// 各种样式错乱。各种不合理的设置：Nginx 管理和上传大小/执行时间严重重复。
// 我不想一个一个找出来让你改"）：
//   "难看"是主观的，但**样式错乱**与**设置重复**不是 —— 它们可以被机器判定。
//   靠人一遍遍找，永远是漏一个改一个；把判据写下来，才能一轮修一整类、并且不再复发。
//
// 三类判据（都不依赖人的眼睛）：
//   ① 布局溢出：页面出现横向滚动条，或弹窗内容超出视口 —— "样式错乱"最硬的证据。
//   ② 无名字的按钮：可见按钮里既有文字为空、又没有 title / aria-label 的
//      —— 用户只能靠猜，属于"生硬"里最该修的一种。
//   ③ 重复设置：同一个配置项在**两个都能打开的入口**里各有可编辑控件
//      —— 典型就是 nginx `client_max_body_size`（Nginx 管理 · 性能调整 与
//      上传大小/执行时间 各一份）。两份 UI 必然走样成两种行为。
//
// 用法：
//   make dev && make run-local          # 先起调试实例
//   node tools/ui-audit.mjs http://127.0.0.1:18443/dev /tmp/zp-ui-audit
//   ZP_PASS=... node tools/ui-audit.mjs https://127.0.0.1:8443 /tmp/zp-ui-audit   # 真机
//
// 退出码非 0 表示有硬问题（① 溢出 > 2px / ② 无名按钮 / ③ 重复设置项）。

import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const base = (process.argv[2] || 'http://127.0.0.1:18443/dev').replace(/\/+$/, '');
const outDir = process.argv[3] || '/tmp/zp-ui-audit';
const PASS = process.env.ZP_PASS || 'zizpanel-test-fixture-pass';
mkdirSync(outDir, { recursive: true });

// 页面清单：改导航时要一起维护，否则审计会静默漏掉整页
const ROUTES = [
  ['dashboard', '仪表盘'],
  ['files', '文件管理'],
  ['sites', '网站管理'],
  ['apps', '应用'],
  ['database', '数据库'],
  ['cron', '计划任务'],
  ['docker', 'Docker'],
  ['logs', '日志'],
  ['settings', '面板设置'],
];

// 需要检查"同一个配置项有没有两个入口"的弹窗：(入口按钮文案, 给人看的名字, 路由)
const MODALS = [
  ['sites', 'Nginx 管理 · 性能调整', '⚙️ Nginx 管理', '性能调整'],
  ['sites', '上传大小 / 执行时间', '⚡ 上传大小 / 执行时间'],
  ['sites', '配置文件', '⚙️ 配置文件'],
  ['sites', '默认站点', '🏠 默认站点'],
];

const hard = [];
const soft = [];
const browser = await chromium.launch();
const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 } });
const page = await ctx.newPage();
const shot = (n) => page.screenshot({ path: `${outDir}/${n}.png` });

async function login() {
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1500);
  const setupPwd = page.locator('input[placeholder="至少 8 位"]');
  const pwd = page.locator('input[autocomplete="current-password"]');
  if (await setupPwd.count()) {
    await page.fill('input[autocomplete="username"]', 'admin');
    await setupPwd.fill(PASS);
    await page.fill('input[placeholder="再次输入"]', PASS);
    await page.click('button:has-text("创建管理员并进入面板")');
  } else if (await pwd.count()) {
    await page.fill('input[autocomplete="username"]', 'admin');
    await pwd.fill(PASS);
    await page.click('button:has-text("登 录")');
  }
  await page.waitForSelector('.layout', { timeout: 20000 });
  await page.waitForTimeout(1000);
}

// 每个宽度都查一次横向溢出：窄屏才是"样式错乱"最容易露馅的地方
const WIDTHS = [1440, 1280, 1024];

async function auditRoute(id, title) {
  await page.goto(`${base}/#/${id}`, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1800);
  for (const w of WIDTHS) {
    await page.setViewportSize({ width: w, height: 900 });
    await page.waitForTimeout(350);
    const m = await page.evaluate(() => {
      const de = document.documentElement;
      const over = de.scrollWidth - de.clientWidth;
      // 找出真正超宽的元素（最多报 5 个），否则只知道"页面溢出"却不知道是谁
      const wide = [];
      for (const el of document.querySelectorAll('body *')) {
        const r = el.getBoundingClientRect();
        if (r.width > 0 && r.right > de.clientWidth + 2 && getComputedStyle(el).position !== 'fixed') {
          wide.push(`${el.tagName.toLowerCase()}.${(el.className || '').toString().split(' ')[0]} right=${Math.round(r.right)}`);
          if (wide.length >= 5) break;
        }
      }
      return { over, wide };
    });
    if (m.over > 2) {
      hard.push(`① ${title}(${id}) 在 ${w}px 下横向溢出 ${m.over}px：${m.wide.join(' | ') || '（找不到具体元素）'}`);
      await shot(`overflow-${id}-${w}`);
    }
  }
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.waitForTimeout(250);

  // 无名按钮（可见的才算：隐藏的下拉项不该报）
  const unnamed = await page.evaluate(() => {
    const out = [];
    for (const b of document.querySelectorAll('button, a.btn')) {
      const cs = getComputedStyle(b);
      if (cs.display === 'none' || cs.visibility === 'hidden' || b.offsetParent === null) continue;
      const text = (b.textContent || '').trim();
      const label = text || b.getAttribute('title') || b.getAttribute('aria-label') || '';
      if (!label) out.push(`<${b.tagName.toLowerCase()} class="${b.className}">`);
    }
    return out;
  });
  if (unnamed.length) hard.push(`② ${title}(${id}) 有 ${unnamed.length} 个没有名字/提示的按钮：${unnamed.slice(0, 5).join(', ')}`);
  return { over: 0 };
}

// 收集一个已打开弹窗里的"可编辑配置项"名字（去重、归一化）
const collectFields = () => page.evaluate(() => {
  const norm = (s) => (s || '').toLowerCase().replace(/[\s（）()：:]/g, '');
  const labelFor = (el) => {
    // ① 表格行：第一个单元格就是字段名（nginx 性能调整那张表）
    const tr = el.closest('tr');
    if (tr) { const c = tr.querySelector('td,th'); if (c && c.textContent.trim()) return c.textContent.trim(); }
    // ② <label> 包裹：取 label 的文字，但必须去掉 input 自己的内容
    const lab = el.closest('label');
    if (lab) {
      const clone = lab.cloneNode(true);
      clone.querySelectorAll('input,select,textarea').forEach((n) => n.remove());
      const t = clone.textContent.trim();
      if (t) return t;
    }
    // ③ 旁边 1~3 层内的短文本节点（很多表单是 span + input 并排）
    let p = el.parentElement, hops = 0;
    while (p && hops++ < 3) {
      const cand = [...p.children].find((c) => c !== el && !c.contains(el)
        && /^(span|div|label|b|strong)$/i.test(c.tagName) && c.textContent.trim().length <= 60);
      if (cand) return cand.textContent.trim();
      p = p.parentElement;
    }
    return el.name || el.id || el.getAttribute('placeholder') || '';
  };
  const out = [];
  const modal = document.querySelector('.modal-mask:not([style*="display: none"]) .modal')
    || document.querySelector('.modal');
  if (!modal) return out;
  for (const el of modal.querySelectorAll('input, select, textarea')) {
    if (el.type === 'hidden' || el.disabled) continue;
    const l = norm(labelFor(el));
    if (l) out.push(l);
  }
  return out;
});

async function auditModals() {
  const seen = new Map(); // 字段名 → [入口名]
  for (const [route, name, btnText, tabText] of MODALS) {
    await page.goto(`${base}/#/${route}`, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(1500);
    const btn = page.locator(`button:has-text("${btnText}")`).first();
    if (!(await btn.count())) { soft.push(`· 找不到入口按钮「${btnText}」（改名了？审计清单要同步）`); continue; }
    await btn.click();
    await page.waitForTimeout(1200);
    if (tabText) {
      // 弹窗里带页签的（Nginx 管理）：必须切到有表单的那一页，否则一个字段都收不到
      const tab = page.locator('.modal button').filter({ hasText: tabText }).first();
      if (await tab.count()) { await tab.click(); await page.waitForTimeout(800); }
    }
    const open = await page.locator('.modal-mask').count();
    if (!open) { soft.push(`· 点「${btnText}」没有打开弹窗`); continue; }
    const fields = await collectFields();
    for (const f of new Set(fields)) {
      if (!seen.has(f)) seen.set(f, []);
      seen.get(f).push(name);
    }
    // 弹窗是否超出视口
    const bad = await page.evaluate(() => {
      const de = document.documentElement;
      const m = document.querySelector('.modal');
      if (!m) return null;
      const r = m.getBoundingClientRect();
      return r.bottom > window.innerHeight + 2 || r.right > de.clientWidth + 2
        ? { bottom: Math.round(r.bottom), right: Math.round(r.right), h: window.innerHeight } : null;
    });
    if (bad) hard.push(`① 弹窗「${name}」超出视口：${JSON.stringify(bad)}`);
    await shot(`modal-${route}-${name.replace(/[ /]/g, '_')}`);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
  }
  // 同一个配置项出现在多个入口 = 用户说的"严重重复"。
  //
  // ⚠️ 不能拿"字段名字符串相等"当判据：两个入口的写法天然不同
  //（nginx 那张表写 `client_max_body_size`，上传限制弹窗写
  // "nginx 请求体上限（client_max_body_size）"）—— 第一版就是这么漏掉的。
  // 判据改成"提取出**规范配置名**再比"：只要这个名字在两个入口里都能找到，
  // 就是同一个设置项被两处 UI 写。
  const CANON = [
    'client_max_body_size', 'client_body_buffer_size', 'client_header_buffer_size',
    'keepalive_timeout', 'worker_connections', 'worker_processes', 'gzip_comp_level',
    'upload_max_filesize', 'post_max_size', 'memory_limit', 'max_execution_time',
    'server_names_hash_bucket_size', 'gzip_min_length',
  ];
  const byCanon = new Map();
  for (const [field, entries] of seen) {
    for (const c of CANON) {
      if (!field.includes(c)) continue;
      if (!byCanon.has(c)) byCanon.set(c, new Map());
      for (const e of entries) byCanon.get(c).set(e, true);
    }
  }
  for (const [c, entries] of byCanon) {
    const uniq = [...entries.keys()];
    if (uniq.length > 1) {
      hard.push(`③ 配置项「${c}」有 ${uniq.length} 个可编辑入口：${uniq.join(' / ')} —— 同一个值只能有一个地方能改`);
    }
  }
  return seen;
}

console.log(`=== UI 审计 ===\n实例: ${base}\n截图: ${outDir}\n`);
await login();
for (const [id, title] of ROUTES) {
  process.stdout.write(`▸ ${title} … `);
  await auditRoute(id, title);
  console.log('OK');
}
console.log('\n▸ 弹窗入口与配置项重复检查 …');
const fields = await auditModals();
console.log('OK');

console.log('\n--- 可编辑字段清单（按弹窗）---');
for (const [f, entries] of [...fields].sort()) console.log(`  ${f}  ←  ${[...new Set(entries)].join(' / ')}`);

if (soft.length) { console.log('\n--- 提示 ---'); soft.forEach((s) => console.log('  ' + s)); }
if (hard.length) {
  console.log(`\n❌ 发现 ${hard.length} 个硬问题（这些正是"样式错乱 / 设置重复"的可判定形态）：`);
  hard.forEach((h) => console.log('  ' + h));
  await browser.close();
  process.exit(1);
}
console.log('\n✅ 没有发现可判定的 UI 硬问题（溢出 / 无名按钮 / 重复设置项）。');
await browser.close();
