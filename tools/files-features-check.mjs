// tools/files-features-check.mjs —— 本轮三件事的**真浏览器**端到端断言（隔离实例）。
//
// 前置（脚本不自己起服务，避免误碰真机）：
//   make run-local LOCAL_PORT=18464 LOCAL_SUFFIX=devb LOCAL_ROOT=/tmp/zp-b
//   node tools/files-features-check.mjs http://127.0.0.1:18464/devb /private/tmp/zp-b
//
// 覆盖：
//   ① 去广告默认不保留 .bak（勾选框默认不勾；结果行说"未保留"；磁盘无 .bak），
//      勾选后 .bak 必须在且结果行写出备份文件名；
//   ② 排序只走表头点击（工具栏不得有「排序 ▾」）：「类型」列表头存在且可点、列位置在
//      大小之后修改时间之前、单元格=目录/小写扩展名/—；点「类型」表头后断言 **DOM 行
//      顺序**确实变了（不是只看箭头），同列再点切降序；刷新后仍保持（localStorage）；
//   ③ 文件操作进度显示速度：注入一个慢速假 file_copy 任务（拦截 /tasks 列表），
//      断言文件管理就地进度条与任务中心进度行都出现 "MB/s"。
//   另：390 / 360 宽度横向溢出必须为 0，pageerror 必须为 0。
//
// 退出码非 0 = 有硬失败。

import { chromium } from 'playwright';
import { existsSync, statSync, mkdirSync, writeFileSync, rmSync, utimesSync, copyFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';

const BASE = (process.argv[2] || 'http://127.0.0.1:18464/devb').replace(/\/+$/, '');
const ROOT = process.argv[3] || '/private/tmp/zp-b';
const PASS = process.env.ZP_PASS || 'zizpanel-test-fixture-pass';
const FFMPEG = process.env.FFMPEG || 'ffmpeg';

const SORT_DIR = join(ROOT, 'e2e', 'sortdir');
const CLEAN_DIR = join(ROOT, 'e2e', 'task1');

// ------------------------------------------------------------------ 夹具（可复现） --
// 每次都重建：去广告会**不可逆**地替换原文件，所以样本必须每次重新生成。
function setupFixtures() {
  rmSync(join(ROOT, 'e2e'), { recursive: true, force: true });
  // 排序目录：6 个条目，名字/类型/时间/大小四个维度都能区分开。
  mkdirSync(join(SORT_DIR, 'zdir_a'), { recursive: true });
  mkdirSync(join(SORT_DIR, 'zdir_b'), { recursive: true });
  const put = (name, bytes, when) => {
    const p = join(SORT_DIR, name);
    writeFileSync(p, 'A'.repeat(bytes));
    utimesSync(p, new Date(when), new Date(when));
  };
  put('alpha.txt', 292, '2020-01-01T00:00:00');
  put('beta.mkv', 97, '2026-01-01T00:00:00');
  put('gamma.zip', 192, '2024-01-01T00:00:00');
  put('noext', 22, '2022-01-01T00:00:00');
  utimesSync(join(SORT_DIR, 'zdir_a'), new Date('2023-01-01T00:00:00'), new Date('2023-01-01T00:00:00'));
  utimesSync(join(SORT_DIR, 'zdir_b'), new Date('2021-01-01T00:00:00'), new Date('2021-01-01T00:00:00'));

  // 去广告目录：2 条视频流（第 2 条=多余视频流）+ 音轨 + 非字体附件 + 字体附件 +
  // 全局标签 + 轨道标题。字体附件必须被保留（门禁会看产物）。
  mkdirSync(CLEAN_DIR, { recursive: true });
  writeFileSync(join(CLEAN_DIR, 'README.txt'), 'promo attachment body');
  writeFileSync(join(CLEAN_DIR, 'font.ttf'), 'FAKEFONT');
  execFileSync(FFMPEG, [
    '-v', 'error', '-y',
    '-f', 'lavfi', '-i', 'testsrc=duration=2:size=64x64:rate=5',
    '-f', 'lavfi', '-i', 'testsrc2=duration=2:size=64x64:rate=5',
    '-f', 'lavfi', '-i', 'sine=frequency=440:duration=2',
    '-map', '0:v', '-map', '1:v', '-map', '2:a',
    '-c:v', 'libx264', '-preset', 'ultrafast', '-c:a', 'aac',
    '-metadata', 'title=promo title', '-metadata', 'comment=join group',
    '-metadata:s:v:0', 'title=V1', '-metadata:s:a:0', 'title=A1', '-metadata:s:a:0', 'language=eng',
    '-attach', 'README.txt', '-metadata:s:t:0', 'mimetype=text/plain',
    '-attach', 'font.ttf', '-metadata:s:t:1', 'mimetype=application/x-truetype-font',
    join(CLEAN_DIR, 'sample.mkv'),
  ], { stdio: 'ignore', cwd: CLEAN_DIR });
  copyFileSync(join(CLEAN_DIR, 'sample.mkv'), join(CLEAN_DIR, 'keep.mkv'));
}
setupFixtures();
const SAMPLE_SIZE = statSync(join(CLEAN_DIR, 'sample.mkv')).size;

const results = [];
const fails = [];
function check(name, ok, extra) {
  results.push({ name, ok: !!ok, extra: extra == null ? '' : String(extra) });
  console.log((ok ? '✅ ' : '❌ ') + name + (extra != null && extra !== '' ? '  → ' + extra : ''));
  if (!ok) fails.push(name + (extra ? ' → ' + extra : ''));
}

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await ctx.newPage();
let pageErrors = 0;
page.on('pageerror', (e) => { pageErrors++; console.log('  ⚠ pageerror: ' + e.message); });

// ------------------------------------------------------------------ 注入假任务 --
// 需求③：前端在"任务列表/SSE 进度"里拿不到足够数据时才允许改后端；这里证明不需要 ——
// 用一个假的 running file_copy（字节数随时间递增）就能验证前端的速度/ETA 显示。
let filesCwd = '';
let fakeDone = 512 * 1024 * 1024;
await page.route(/\/api\/v1\/files\?/, async (route) => {
  const resp = await route.fetch();
  try {
    const body = await resp.json();
    if (body && body.data && body.data.path) filesCwd = body.data.path;
  } catch { /* 非 JSON 就照原样返回 */ }
  await route.fulfill({ response: resp });
});
await page.route(/\/api\/v1\/tasks$/, async (route) => {
  fakeDone += 220 * 1024 * 1024;
  // 真任务的列表必须照常透传（否则面板里刚创建的去广告任务永远不会被判定结束）。
  let real = [];
  try {
    const resp = await route.fetch();
    const body = await resp.json();
    real = (body && body.data && body.data.tasks) || [];
  } catch { /* 拿不到就只用假任务 */ }
  const tasks = [{
    id: 'zp-speed-fake', kind: 'file_copy', target: (filesCwd && filesCwd.endsWith('/e2e/sortdir')) ? filesCwd : SORT_DIR,
    title: '复制（假任务·测速度）', status: 'running',
    started_at: new Date().toISOString(), finished_at: '0001-01-01T00:00:00Z',
    elapsed_ms: 0, line_count: 3,
    last: '复制 3/8 文件 · 1.2 GB / 3.0 GB · 40%',
    progress: {
      phase: 'copy', done: fakeDone, total: 3 * 1024 * 1024 * 1024,
      files_done: 3, files_total: 8,
      message: '复制 3/8 文件 · 1.2 GB / 3.0 GB · 40% · 当前：big.bin',
    },
  }];
  await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { tasks: [tasks[0], ...real] } }) });
});
// 只拦假任务的 SSE 流：真任务的流必须放行（去广告任务要靠它收敛状态）。
await page.route(/\/api\/v1\/tasks\/zp-speed-fake\/stream$/, (route) =>
  route.fulfill({ status: 200, headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' }, body: '' }));

// ------------------------------------------------------------------ 登录 --
async function login() {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
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
  await page.waitForTimeout(1200);
}

async function openFiles() {
  await page.goto(BASE + '/#/files', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('table.table', { timeout: 20000 });
  await page.waitForTimeout(800);
}

async function selectRoot() {
  await page.selectOption('select.select', ROOT);
  await page.waitForTimeout(700);
}

async function enterDir(name) {
  await page.click(`table.table tbody tr a:text-is("${name}")`);
  await page.waitForTimeout(700);
}

const rowNames = () => page.$$eval('table.table tbody tr', (rows) => rows.map((r) => {
  const tds = r.querySelectorAll('td');
  const a = tds[1] && tds[1].querySelector('a');
  return a ? a.textContent.trim() : '';
}));

// 「类型」列（大小后面、修改时间前面 = td[3]）。
const typeCells = () => page.$$eval('table.table tbody tr', (rows) => rows.map((r) => {
  const tds = r.querySelectorAll('td');
  return tds[3] ? tds[3].textContent.trim() : '';
}));

// 排序入口只有表头：点同一个表头两次 = 反向。
async function headerClick(label) {
  await page.click(`table.table thead th button:has-text("${label}")`);
  await page.waitForTimeout(600);
}

await login();
console.log('\n=== ② 排序（只走表头点击：名称 / 大小 / 类型 / 修改时间）===');
await openFiles();
await selectRoot();
await enterDir('e2e');
await enterDir('sortdir');

check('工具栏里没有「排序 ▾」按钮', await page.locator('button:has-text("排序 ▾")').count() === 0);
const typeHead = page.locator('table.table thead th button:has-text("类型")');
check('「类型」列表头存在且可点', (await typeHead.count()) === 1 && (await typeHead.first().isEnabled()));

// 表头从左到右：名称 / 大小 / 类型 / 权限 / 属主 / 修改时间 / 操作（「类型」在大小之后）。
const headLabels = await page.$$eval('table.table thead th', (ths) => ths.map((t) => t.textContent.trim()));
const idxSize = headLabels.findIndex((t) => t.startsWith('大小'));
const idxType = headLabels.findIndex((t) => t.startsWith('类型'));
const idxTime = headLabels.findIndex((t) => t.startsWith('修改时间'));
check('「类型」列位置在大小之后、修改时间之前',
  idxSize >= 0 && idxType === idxSize + 1 && idxTime > idxType, JSON.stringify(headLabels));

const base = await rowNames();
check('默认名字升序（目录永远在前）',
  JSON.stringify(base) === JSON.stringify(['zdir_a', 'zdir_b', 'alpha.txt', 'beta.mkv', 'gamma.zip', 'noext']),
  JSON.stringify(base));
check('「类型」列内容：目录=目录 / 小写扩展名 / 无扩展名=—',
  JSON.stringify(await typeCells()) === JSON.stringify(['目录', '目录', 'txt', 'mkv', 'zip', '—']),
  JSON.stringify(await typeCells()));

// 点「类型」表头 ⇒ 类型升序（扩展名序，无扩展名垫底），DOM 行顺序必须真的变化。
await headerClick('类型');
const typeAsc = await rowNames();
check('点「类型」表头后 DOM 行顺序确实变化',
  JSON.stringify(typeAsc) === JSON.stringify(['zdir_a', 'zdir_b', 'beta.mkv', 'alpha.txt', 'gamma.zip', 'noext'])
  && JSON.stringify(typeAsc) !== JSON.stringify(base), JSON.stringify(typeAsc));
// 再点一次同列 ⇒ 类型降序（扩展名反向，无扩展名仍垫底）。
await headerClick('类型');
const typeDesc = await rowNames();
check('再点「类型」表头 ⇒ 降序（无扩展名仍垫底）',
  JSON.stringify(typeDesc) === JSON.stringify(['zdir_a', 'zdir_b', 'gamma.zip', 'alpha.txt', 'beta.mkv', 'noext']),
  JSON.stringify(typeDesc));

// 修改时间：第一次升序、再点降序。
await headerClick('修改时间');
const timeAsc = await rowNames();
check('点「修改时间」表头 ⇒ 时间升序（旧→新，目录在前）',
  JSON.stringify(timeAsc) === JSON.stringify(['zdir_b', 'zdir_a', 'alpha.txt', 'noext', 'gamma.zip', 'beta.mkv']),
  JSON.stringify(timeAsc));
await headerClick('修改时间');
const timeDesc = await rowNames();
check('再点「修改时间」⇒ 时间降序（新→旧）',
  JSON.stringify(timeDesc) === JSON.stringify(['zdir_a', 'zdir_b', 'beta.mkv', 'gamma.zip', 'noext', 'alpha.txt']),
  JSON.stringify(timeDesc));

// 大小：第一次升序、再点降序。
await headerClick('大小');
const sizeAsc = await rowNames();
check('点「大小」表头 ⇒ 大小升序（小→大）',
  JSON.stringify(sizeAsc) === JSON.stringify(['zdir_a', 'zdir_b', 'noext', 'beta.mkv', 'gamma.zip', 'alpha.txt']),
  JSON.stringify(sizeAsc));
await headerClick('大小');
const sizeDesc = await rowNames();
check('再点「大小」表头 ⇒ 大小降序（大→小）',
  JSON.stringify(sizeDesc) === JSON.stringify(['zdir_a', 'zdir_b', 'alpha.txt', 'gamma.zip', 'beta.mkv', 'noext']),
  JSON.stringify(sizeDesc));

const stored = await page.evaluate(() => localStorage.getItem('zp-files-sort'));
check('表头选择持久化到 localStorage', stored === 'size:-1', 'zp-files-sort=' + stored);

// 刷新后保持：重新载入页面再进目录，顺序仍是上次选的降序
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector('table.table', { timeout: 20000 });
await page.waitForTimeout(600);
await selectRoot();
await enterDir('e2e');
await enterDir('sortdir');
const afterReload = await rowNames();
check('刷新后仍保持所选排序（localStorage 生效）',
  JSON.stringify(afterReload) === JSON.stringify(sizeDesc), JSON.stringify(afterReload));

console.log('\n=== ① 去广告默认不保留 .bak ===');
// 回到 e2e 再进 task1
await page.click('button:has-text("↑ 上一级")');
await page.waitForTimeout(600);
await enterDir('task1');

async function runClean(file, keepBackup) {
  await page.click(`table.table tbody tr:has-text("${file}") input[type=checkbox]`);
  await page.waitForTimeout(200);
  await page.click('button:has-text("魔法箱")');
  await page.waitForTimeout(250);
  await page.click('.zp-ctx-item:has-text("去广告")');
  await page.waitForSelector('.modal:has-text("保留原文件为 .bak")', { timeout: 30000 });
  const box = page.locator('.modal input[type=checkbox]').last();
  const before = await box.isChecked();
  if (keepBackup && !before) await box.check();
  const modalText = await page.locator('.modal').last().innerText();
  await page.click('button:has-text("开始处理")');
  await page.waitForSelector('.modal:has-text("去广告（无损）结果")', { timeout: 120000 });
  await page.waitForTimeout(500);
  const resultText = await page.locator('.modal:has-text("去广告（无损）结果")').last().innerText();
  return { defaultUnchecked: !before, confirmText: modalText, resultText };
}

const runDefault = await runClean('sample.mkv', false);
check('确认窗有勾选框且默认不勾', runDefault.defaultUnchecked);
check('确认窗写清默认直接替换', runDefault.confirmText.includes('默认直接替换原文件'), runDefault.confirmText.slice(0, 0));
check('结果行说"未保留"（不谎报已备份）',
  runDefault.resultText.includes('未保留') && !runDefault.resultText.includes('已保留为 .bak'));
check('结果 Note 逐项报去掉了什么',
  runDefault.resultText.includes('去掉多余视频流 1 条') && runDefault.resultText.includes('去掉附件 1 个')
  && runDefault.resultText.includes('清空轨道标题 2 条') && runDefault.resultText.includes('清空全局标签'));
check('默认成功：磁盘上没有任何 .bak', !existsSync(join(CLEAN_DIR, 'sample.mkv.bak')));
const cleaned = statSync(join(CLEAN_DIR, 'sample.mkv')).size;
check('默认成功：原文件已被新产物替换', cleaned !== SAMPLE_SIZE, 'size=' + cleaned + '（原 ' + SAMPLE_SIZE + '）');
// 关掉结果窗（modal 只有 × / Esc 两条关闭路径）
await page.keyboard.press('Escape');
await page.waitForTimeout(500);

const runKeep = await runClean('keep.mkv', true);
check('勾选后结果行写出备份文件名', runKeep.resultText.includes('keep.mkv.bak'), '');
check('勾选后结果行说明已保留 .bak', runKeep.resultText.includes('已保留为 .bak'));
check('勾选成功：.bak 必须在且是原文件内容', existsSync(join(CLEAN_DIR, 'keep.mkv.bak')) && statSync(join(CLEAN_DIR, 'keep.mkv.bak')).size === SAMPLE_SIZE);
await page.keyboard.press('Escape');
await page.waitForTimeout(400);

console.log('\n=== ③ 文件操作进度：速度 + 预计剩余 ===');
// 回到 sortdir（假任务的 target 就是它）；进入目录会 resumeOpBar 找回进度条。
await page.click('button:has-text("↑ 上一级")');
await page.waitForTimeout(600);
await enterDir('sortdir');
// 打开任务中心：它每 3 秒轮询 /tasks，让假任务的字节数递增 → 前端滑窗才有新样本。
await page.click('#zp-task-btn');
await page.waitForTimeout(1000);
let rateSeen = '';
try {
  await page.waitForFunction(() => document.body.innerText.includes('MB/s'), null, { timeout: 20000 });
  rateSeen = 'body';
} catch { /* 超时后下面照样断言 */ }
const bodyText = await page.locator('body').innerText();
check('任务中心进度行出现速度（MB/s）', /MB\/s/.test(bodyText), rateSeen);
check('速度带预计剩余（约剩）', /约剩/.test(bodyText) || /计算中/.test(bodyText), '');
const opBarText = await page.locator('.files-opbar').innerText().catch(() => '');
check('文件管理就地进度条出现速度（MB/s）', /MB\/s/.test(opBarText), JSON.stringify(opBarText.slice(0, 120)));
await page.keyboard.press('Escape');
await page.waitForTimeout(400);

console.log('\n=== 布局：390 / 360 横向溢出 + pageerror ===');
for (const w of [390, 360]) {
  await page.setViewportSize({ width: w, height: 844 });
  await page.waitForTimeout(500);
  // 新增「类型」列后窄屏更容易顶宽：检查列表页本身（含表头）就够。
  await page.waitForTimeout(300);
  const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  check(`文件管理 ${w}px 横向溢出 0`, over <= 2, 'over=' + over);
  await page.keyboard.press('Escape');
  await page.waitForTimeout(200);
}
await page.setViewportSize({ width: 1440, height: 900 });
check('pageerror = 0', pageErrors === 0, 'pageerror=' + pageErrors);

console.log('\n===== 汇总 =====');
const hard = results.filter((r) => !r.ok).length;
console.log(`通过 ${results.length - hard}/${results.length} 条断言`);
if (fails.length) {
  console.log('失败：');
  for (const f of fails) console.log('  - ' + f);
}
await browser.close();
process.exit(fails.length ? 1 : 0);
