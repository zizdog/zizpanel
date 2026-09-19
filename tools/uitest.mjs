// tools/uitest.mjs —— 端到端 UI 验证脚本。
// 用法：node tools/uitest.mjs http://127.0.0.1:18443 /tmp/zp-shots
// 退出码非 0 = 流程失败，可直接接入 CI。接口 200 不代表页面渲染/按钮/会话正常。

import { chromium } from 'playwright';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const base = process.argv[2] || 'http://127.0.0.1:18443';
// base 带尾斜杠时直接拼 '/api/…' 会变 '//api/…'，ServeMux 先回 301，而健康检查把 301 记为"健康"
// ⇒「401 判为健康」这条断言根本走不到要测的分支（2026-09-19 实测）。
const apiURL = (path) => base.replace(/\/+$/, '') + path;
const TEST_SITE = process.env.ZP_TEST_SITE || 'zptest-demo.test';
// 口令**不写进仓库**：make smoke 用默认假口令；make uitest-live 由 Makefile 通过 ZP_PASS
// 传入 .panel-credential.local 里的真实口令（曾硬编码进版本库，已改掉）。
const PASSWORD = process.env.ZP_PASS || 'zizpanel-test-fixture-pass';
const outDir = process.argv[3] || '/tmp/zp-shots';
mkdirSync(outDir, { recursive: true });

const shots = [];
const step = async (name, fn) => {
  process.stdout.write(`▸ ${name} ... `);
  try {
    await fn();
    console.log('OK');
  } catch (e) {
    console.log('失败');
    throw e;
  }
};

// ZP_SKIP_PRIV=1 显式跳过需要 root 提权的步骤并如实打印"跳过"；完整特权链路跑 make uitest-live。
// closeAnyModal 关掉所有弹窗（含遮罩）：上一步留下的弹窗会挡住下一步按钮，Playwright 报
// "intercepts pointer events"，人看到的是"点了没反应"（2026-09-19 实测）。
const closeAnyModal = async (page) => {
  for (let i = 0; i < 6; i++) {
    // 文件编辑器是独立窗口 .zpf-win（铺满 content 区），不关掉后面按钮点不到；
    // 窗口里可能有未保存修改，所以临时接管对话框（确认框一律接受）再关。
    const wins = page.locator('.zpf-win');
    if (await wins.count()) {
      const accept = (d) => d.accept().catch(() => {});
      page.on('dialog', accept);
      await wins.last().locator('.zpf-iconbtn.zpf-close').click().catch(() => {});
      await page.waitForTimeout(300);
      page.off('dialog', accept);
      continue;
    }
    const masks = page.locator('.modal-mask');
    if (!(await masks.count())) return;
    const x = masks.last().locator('button.modal-close').first();
    if (await x.count()) { await x.click().catch(() => {}); }
    else { await page.keyboard.press('Escape').catch(() => {}); }
    await page.waitForTimeout(250);
  }
};

const SKIP_PRIV = process.env.ZP_SKIP_PRIV === '1';
const skipped = [];
const privStep = async (name, fn) => {
  if (SKIP_PRIV) {
    skipped.push(name);
    console.log(`▸ ${name} ... 跳过（本地非 root 实例无法执行提权操作）`);
    return;
  }
  await step(name, fn);
};

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  deviceScaleFactor: 1,
  ignoreHTTPSErrors: true,
  locale: 'zh-CN',
});
const page = await ctx.newPage();

const errors = [];
// expectAuthFailure 为 true 时 401/403 属预期。监听 response 而不是 console：浏览器对 401
// 只打印 "Failed to load resource"，既没有状态码也没有 URL，无法据此判断。
let expectAuthFailure = false;
// expectHTTPError：故意触发的 4xx 是断言对象，不应计为故障。
let expectHTTPError = false;

// pageerror 必须带堆栈：只记 message 不告诉你是哪个文件哪一行（真踩过）。
page.on('pageerror', (e) => {
  const at = (e.stack || '').split('\n').filter((l) => l.includes('assets/js/'))[0];
  errors.push('pageerror: ' + e.message + (at ? ' @ ' + at.trim() : ''));
});
page.on('requestfailed', (r) => {
  const url = r.url();
  const why = r.failure()?.errorText || '';
  // SSE 长连接被前端主动关闭时会报 ERR_ABORTED，属预期。
  if (why.includes('ERR_ABORTED') && (url.includes('/logs/stream') || url.includes('/system/stream'))) {
    return;
  }
  errors.push(`requestfailed: ${url} ${why}`);
});
page.on('response', (res) => {
  const code = res.status();
  if (code < 400) return;
  if (code === 401 && /\/api\/v1\/(session|login)/.test(res.url())) return;
  // 后台自动检查更新失败**不是**前端故障：无升级源时后端按设计返回 409，前端 silent 吞掉。
  // 它是周期性的，按 URL 显式豁免。
  if (/\/api\/v1\/system\/upgrade\/check$/.test(res.url())) return;
  if (expectAuthFailure && (code === 401 || code === 403)) return;
  if (expectHTTPError) return;
  errors.push(`HTTP ${code} ${res.url()}`);
});

async function shot(name) {
  const path = `${outDir}/${name}.png`;
  await page.screenshot({ path, fullPage: false });
  shots.push(path);
  return path;
}

try {
  await step('打开面板首页', async () => {
    await page.goto(base, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#app:not([hidden])', { timeout: 10000 });
  });

  await step('校验前端组件库基础行为', async () => {
    // 回归：h() 第二个参数是 DOM Node 时必须当子节点 —— 曾误判为 props，节点内容被静默丢弃。
    const r = await page.evaluate(async () => {
      const { h } = await import('./js/ui.js');
      const inner = h('div', [h('span', { text: 'x' })]);
      const withNulls = h('div', [h('span', { text: 'a' }), null, false, undefined, h('span', { text: 'b' })]);
      const out = {
        nodeAsChild: h('div', inner).childElementCount,
        arrayAsChild: h('div', [inner]).childElementCount,
        stringAsChild: h('div', 'txt').textContent,
        propsStillWork: h('div', { class: 'cls', text: 'T' }).outerHTML,
        nullText: withNulls.textContent,
        nullChildren: withNulls.childElementCount,
      };
      return out;
    });
    if (r.nullText.includes('null') || r.nullText.includes('undefined')) {
      throw new Error('数组中的 null/undefined 被渲染成了文本: ' + r.nullText);
    }
    if (r.nullChildren !== 2) throw new Error('数组过滤后子元素数应为 2，实际 ' + r.nullChildren);
    if (r.nodeAsChild !== 1) throw new Error('h(spec, node) 未把 Node 当作子节点');
    if (r.arrayAsChild !== 1) throw new Error('h(spec, [node]) 行为异常');
    if (r.stringAsChild !== 'txt') throw new Error('h(spec, string) 行为异常');
    if (!r.propsStillWork.includes('class="cls"')) throw new Error('h(spec, props) 行为异常');
  });

  // ---------- 首次初始化 ----------
  // 启动阶段 boot() 会先探测会话，未登录时必然收到 401 —— 这是预期行为
  expectAuthFailure = true;
  const needsSetup = await page.locator('text=首次初始化').count();
  if (needsSetup > 0) {
    await step('填写初始化表单', async () => {
      await page.fill('input[autocomplete="username"]', 'admin');
      await page.fill('input[placeholder="至少 8 位"]', PASSWORD);
      await page.fill('input[placeholder="再次输入"]', PASSWORD);
      await shot('01-setup');
    });
    await step('提交初始化', async () => {
      await page.click('button:has-text("创建管理员并进入面板")');
      await page.waitForSelector('.layout', { timeout: 12000 });
    });
  } else {
    await step('登录', async () => {
      await page.fill('input[autocomplete="username"]', 'admin');
      await page.fill('input[autocomplete="current-password"]', PASSWORD);
      await page.click('button:has-text("登 录")');
      await page.waitForSelector('.layout', { timeout: 12000 });
    });
  }

  await step('等待实时指标到达（SSE）', async () => {
    await page.waitForFunction(() => {
      const el = document.querySelector('.metric .value');
      return el && /%/.test(el.textContent);
    }, null, { timeout: 15000 });
    expectAuthFailure = false;
  });

  await step('仪表盘截图', async () => {
    await page.waitForTimeout(2500);
    await shot('02-dashboard');
  });

  await step('校验仪表盘关键内容', async () => {
    const txt = await page.locator('.content').innerText();
    const must = ['CPU 使用率', '内存使用', '磁盘占用', '网络吞吐', '系统信息', '进程'];
    for (const m of must) {
      if (!txt.includes(m)) throw new Error(`仪表盘缺少「${m}」`);
    }
    const host = await page.evaluate(() => document.body.innerText);
    if (!host.includes('macOS')) throw new Error('未显示操作系统信息');
  });

  // ---------- 运行依赖横幅（回归：2026-09 拆成"运行依赖"与"网站环境"两层）----------
  // 老横幅把「安装基础环境」写成「一键 LNMP」，标签在骗人（实际只装 CLT+Homebrew+ffmpeg）；
  // 新判据走 GET /api/v1/system/base-env。全部走桩：绝不真的安装任何东西。
  await step('运行依赖横幅：判据走 /system/base-env，按钮只调 base-env/install（桩数据）', async () => {
    const installCalls = [];
    const lnmpCalls = [];

    // 每次重载前清掉「稍后」标记，否则上次手工点过就再也看不到横幅（会漏测）。
    await page.evaluate(() => localStorage.removeItem('zp-baseenv-dismissed'));

    // 假任务的进度流：立刻回一条"成功"并结束 —— 否则假任务会一直重连一个不存在的流刷 404。
    await page.route('**/api/v1/tasks**', (route) => {
      const m = route.request().url().match(/\/api\/v1\/tasks\/([^/?]+)\/stream/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200,
          headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '安装基础环境', status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '安装基础环境', status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
      });
    });
    // 桩住真实安装入口：断言"没被调用"的同时，保证即使被误调用也不会真装东西。
    await page.route('**/api/v1/system/base-env/install', (route) => {
      if (route.request().method() !== 'POST') return route.continue();
      installCalls.push(route.request().url());
      return route.fulfill({
        status: 202, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-base-env-1', title: '安装基础环境' } }),
      });
    });
    await page.route('**/api/v1/market/install-lnmp', (route) => {
      lnmpCalls.push(route.request().url());
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-lnmp-should-not-run', title: '一键 LNMP' } }),
      });
    });

    const stubBaseEnv = (status, data) => (route) => route.fulfill({
      status,
      contentType: 'application/json',
      body: JSON.stringify(status === 200 ? { ok: true, data } : { ok: false, msg: 'UI 测试桩：接口不可用' }),
    });
    const reloadAndAwaitBaseEnv = async () => {
      await Promise.all([
        page.waitForResponse((r) => /\/api\/v1\/system\/base-env$/.test(r.url())
          && r.request().method() === 'GET'),
        page.reload({ waitUntil: 'domcontentloaded' }),
      ]);
      await page.waitForSelector('.content', { timeout: 10000 });
    };
    const closeAllModals = async () => {
      for (let i = 0; i < 4; i++) {
        const masks = page.locator('.modal-mask');
        if (!(await masks.count())) break;
        const x = masks.last().locator('button.modal-close').first();
        if (await x.count()) await x.click().catch(() => {});
        await page.waitForTimeout(200);
      }
    };

    // ① 缺依赖（ready:false + missing）→ 必须**逐项列出缺什么**，按钮是「安装基础环境」
    await page.route('**/api/v1/system/base-env',
      stubBaseEnv(200, { clt_ok: true, brew_ok: false, deps_ok: false, ready: false, missing: ['Homebrew', 'ffmpeg'] }));
    try {
      await reloadAndAwaitBaseEnv();
      await page.waitForSelector('text=缺少运行依赖', { timeout: 15000 });
      const txt = await page.locator('.content').innerText();
      const want = '缺少运行依赖：Homebrew、ffmpeg（命令行开发者工具已就绪）';
      if (!txt.includes(want)) {
        throw new Error('横幅没有逐项列出缺什么（应含「' + want + '」）：\n' + txt.slice(0, 400));
      }
      if (/缺少运行依赖[^\n]*nginx/.test(txt) || /缺少运行依赖[^\n]*MySQL/.test(txt)) {
        throw new Error('运行依赖横幅里又提到 nginx/MySQL 了（两层又混了）');
      }
      if (!txt.includes('不会询问 root 口令')) {
        throw new Error('没有说明"这一步不装 MySQL、不询问 root 口令"');
      }
      if (!(await page.locator('button:has-text("安装基础环境")').count())) {
        throw new Error('缺少「⚡ 安装基础环境」按钮');
      }
      await shot('03a-baseenv-missing');

      // ② 点按钮 → POST base-env/install 恰好一次，且**没有** install-lnmp
      await page.locator('button:has-text("安装基础环境")').first().click();
      await page.locator('.modal-mask').last().waitFor({ timeout: 10000 });
      await page.waitForTimeout(400);
      if (installCalls.length !== 1) {
        throw new Error('POST /system/base-env/install 调用次数应为 1，实际 ' + installCalls.length
          + '（' + JSON.stringify(installCalls) + '）');
      }
      if (lnmpCalls.length !== 0) {
        throw new Error('点「安装基础环境」却调用了 install-lnmp（标签与动作不符）：' + JSON.stringify(lnmpCalls));
      }
      await shot('03b-baseenv-install-task');
      await closeAllModals();
    } finally {
      await page.unroute('**/api/v1/system/base-env');
    }

    await page.route('**/api/v1/system/base-env',
      stubBaseEnv(200, { clt_ok: true, brew_ok: true, deps_ok: true, ready: true, missing: [] }));
    try {
      await reloadAndAwaitBaseEnv();
      await page.waitForTimeout(600);
      const txt = await page.locator('.content').innerText();
      if (txt.includes('缺少运行依赖')) throw new Error('ready:true 时仍出现缺依赖横幅');
      if (txt.includes('无法读取运行依赖状态')) throw new Error('ready:true 时出现"读不到状态"提示');
    } finally {
      await page.unroute('**/api/v1/system/base-env');
    }

    const prevExpectHTTPError = expectHTTPError;
    expectHTTPError = true; // 这次的 404 是断言对象，不是前端故障
    await page.route('**/api/v1/system/base-env', stubBaseEnv(404, null));
    try {
      await reloadAndAwaitBaseEnv();
      await page.waitForSelector('text=无法读取运行依赖状态', { timeout: 15000 });
      const txt = await page.locator('.content').innerText();
      if (txt.includes('缺少运行依赖')) {
        throw new Error('接口 404 时却显示了"缺少运行依赖"（等于凭空编状态）');
      }
      await shot('03c-baseenv-unavailable');
    } finally {
      await page.unroute('**/api/v1/system/base-env');
      expectHTTPError = prevExpectHTTPError;
    }

    await page.unroute('**/api/v1/system/base-env/install');
    await page.unroute('**/api/v1/market/install-lnmp');
    await page.unroute('**/api/v1/tasks**');
    await page.evaluate(() => localStorage.removeItem('zp-baseenv-dismissed'));
  });

  await step('深色主题截图', async () => {
    // 主题按钮 title 是**动态**文案；旧写法 button[title="切换主题"] 永远匹配不到（2026-09-17 抓到）。
    const themeBtn = page.locator('button[title^="主题："]').first();
    await themeBtn.click();
    await page.waitForTimeout(600);
    await shot('03-dashboard-dark');
    await themeBtn.click();
  });

  await step('切换到 mac设置页（旧名「系统设置」）', async () => {
    // 侧栏显示名 2026-09 改成「mac设置」；路由 id 仍是 system。
    if (await page.locator('.nav-item:has-text("系统设置")').count()) {
      throw new Error('侧栏已不应再有「系统设置」，应为「mac设置」');
    }
    await page.click('.nav-item:has-text("mac设置")');
    await page.waitForTimeout(2600);
    await shot('04-system-settings');
    const h2 = (await page.locator('.topbar h2').innerText()).trim();
    if (h2 !== 'mac设置') throw new Error(`页内标题应为「mac设置」，实际「${h2}」`);
    const txt = await page.locator('.content').innerText();
    // 这一页取代了原来的「系统监控」（与仪表盘重复）；内容必须真的渲染出来。
    for (const need of ['一键设为服务器模式', '电源与睡眠', '系统更新阻断', '远程访问与登录']) {
      if (!txt.includes(need)) throw new Error(`mac设置页缺少「${need}」`);
    }
  });

  await step('仪表盘确实包含负载与 Swap（没有因页面改造丢信息）', async () => {
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(2600);
    const txt = await page.locator('.content').innerText();
    for (const need of ['系统负载', 'Swap 使用']) {
      if (!txt.includes(need)) throw new Error(`仪表盘缺少「${need}」`);
    }
  });

  await step('打开面板设置', async () => {
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(1200);
    await shot('05-settings');
    // 面板设置只有 3 个 Tab：访问与安全 → 账号与两步验证 → 检查更新。
    const titles = (await page.locator('.content button.btn-sm').allInnerTexts()).map((x) => x.trim());
    const want = ['访问与安全', '账号与两步验证', '检查更新'];
    const got = titles.filter((t) => want.includes(t));
    if (got.join('|') !== want.join('|')) {
      throw new Error(`设置页 Tab 顺序不对：期望 ${want.join(' → ')}，实际 ${got.join(' → ')}`);
    }
    if (await page.locator('button:has-text("关于与运维")').count()) {
      throw new Error('面板设置里仍有「关于与运维」Tab（应由「检查更新」承担）');
    }
  });

  await step('切换设置内的各 Tab（含检查更新）', async () => {
    for (const t of ['账号与两步验证', '访问与安全']) {
      await page.click(`button:has-text("${t}")`);
      await page.waitForTimeout(700);
      await shot('06-settings-' + t);
    }
  });

  // ---------- 设置里不该再有「上传与执行限制」「文件与终端」（用户要求）----------
  // 只做只读断言：两个旧 Tab 消失，旧路由别名落地后不能白屏。
  await step('面板设置：旧 Tab（上传与执行限制 / 文件与终端）已移除且别名不白屏', async () => {
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(900);
    for (const gone of ['上传与执行限制', '文件与终端']) {
      const asTab = await page.locator(`.content button.btn-sm:has-text("${gone}")`).count();
      if (asTab > 0) {
        throw new Error(`面板设置里仍有「${gone}」Tab（用户要求移除/并入「访问与安全」）`);
      }
    }
    // 旧路由别名：不能白屏，要能落到设置页里可见的内容。
    for (const h of ['/settings/limits', '/settings/terminal']) {
      await page.evaluate((x) => { location.hash = '#' + x; }, h);
      await page.waitForTimeout(900);
      const txt = await page.locator('.content').innerText();
      if (!/访问与安全|账号与两步验证|检查更新/.test(txt)) {
        throw new Error(`旧路由 #${h} 落地后看不到设置页内容（可能白屏）：` + txt.slice(0, 200));
      }
    }
    await shot('06b-settings-no-limits');
  });

  // ---------- 检查更新（2026-09-19 收进「面板设置」第 3 个 Tab）----------
  await step('设置页「检查更新」Tab 可打开，且没有重复的更新按钮', async () => {
    if (await page.locator('.nav-item:has-text("检查更新")').count()) {
      throw new Error('侧栏不该再有「检查更新」入口（已收回面板设置，重复入口会让用户困惑）');
    }
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(600);
    await page.click('button:has-text("检查更新")');
    await page.waitForTimeout(1500);
    const txt = await page.locator('.content').innerText();
    for (const need of ['在线升级', '当前版本', '内嵌发布公钥', '检查更新']) {
      if (!txt.includes(need)) throw new Error(`检查更新 Tab 里缺少「${need}」`);
    }
    // 用户要求删掉「立即检测 + 一键更新到 vX」这对重复按钮（横幅里已有一键更新）。
    if (txt.includes('立即检测')) {
      throw new Error('检查更新里仍有被要求删除的「立即检测」按钮（与横幅的一键更新重复）');
    }
    if (!(await page.locator('[data-testid="zp-update-check-btn"]').count())) {
      throw new Error('检查更新里缺少「检查更新」按钮（再检测一次的唯一入口）');
    }
    if (txt.includes('操作审计')) throw new Error('检查更新页不应出现「操作审计」');
    // 当前版本必须和 session 里报告的一致，避免卡片显示了一个假的版本
    const ver = await page.locator('#zp-up-ver').innerText();
    if (!/^v\d+\.\d+\.\d+/.test(ver)) throw new Error('版本号显示异常: ' + ver);
    await shot('07-update-page');
  });

  await step('旧 hash 别名仍到「检查更新」（老书签不能 404）', async () => {
    const raw = page.url().split('#')[0];
    for (const h of ['#/settings/about', '#/about']) {
      await page.goto(raw + h, { waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(1200);
      const active = (await page.locator('.nav-item.active').innerText().catch(() => '')).trim();
      const body = await page.locator('.content').innerText();
      // 现在升级在「面板设置」里，所以侧栏高亮应该是面板设置，而不是曾经的独立页。
      if (!active.includes('面板设置') || !body.includes('在线升级')) {
        throw new Error(`旧 hash ${h} 没有落到「面板设置 → 检查更新」（active=${active}）`);
      }
      const tabActive = (await page.locator('button.btn-primary:has-text("检查更新")').count()) > 0;
      if (!tabActive) {
        throw new Error(`旧 hash ${h} 落到了面板设置，但没有选中「检查更新」Tab`);
      }
    }
    // 「已迁移的功能」指路卡片必须消失：功能已回到设置页（用户的要求）。
    await page.goto(raw + '#/settings', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(1000);
    if ((await page.locator('.content').innerText()).includes('已迁移的功能')) {
      throw new Error('面板设置里仍有「已迁移的功能」指路卡片（检查更新已经收回来了）');
    }
  });

  await step('升级源留空时自动走候选源（不再报“尚未配置升级源地址”）', async () => {
    // 2026-09-17 起升级源为空时不再 400，而是按候选链逐个试、每个候选都要过验签；
    // 所以断言的是"给出了结论"，而不是某个具体状态码。
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(500);
    await page.click('button:has-text("检查更新")');
    await page.waitForTimeout(800);
    await page.fill('input[placeholder^="https://example.com"]', '');
    expectHTTPError = true;
    try {
      await page.click('[data-testid="zp-update-check-btn"]');
      await page.waitForTimeout(2500);
      const body = await page.locator('.content').innerText();
      if (body.includes('尚未配置升级源地址')) {
        throw new Error('仍在报「尚未配置升级源地址」——候选源没有生效');
      }
      if (!/候选|升级源|已是最新|新版本|失败|未/.test(body)) {
        throw new Error('检查更新没有给出任何结论：' + body.slice(0, 200));
      }
    } finally {
      expectHTTPError = false;
    }
    await shot('07-update-no-source');
  });

  // ---------- 主动检测 / 徽标 / 一键更新（用户的第 4、5 条）----------
  // 全部用 page.route 桩伪造升级接口，**绝不真实升级**（apply 会重启面板）。
  await step('发现新版本：页内醒目提示 + 侧栏徽标（刷新后仍在）', async () => {
    const json = (route, data, status = 200) => route.fulfill({
      status, contentType: 'application/json', body: JSON.stringify({ ok: true, data }),
    });
    await page.route('**/api/v1/system/upgrade**', (route) => {
      const u = new URL(route.request().url());
      const m = route.request().method();
      if (u.pathname.endsWith('/upgrade/check') && m === 'POST') {
        return json(route, { current: '0.0.1', latest: '9.9.9', has_update: true, effective_source: 'ui-test-stub' });
      }
      if (u.pathname.endsWith('/upgrade') && m === 'GET') {
        return json(route, {
          current_version: '0.0.1', arch: 'darwin_arm64', build_time: '2026-01-01T00:00:00Z',
          can_remote: true, pubkey: 'stub', can_apply: false, is_root: false, plain_http: false,
          source: '', effective_source: 'ui-test-stub', staged: false, staged_version: '', notes: '',
          state: { status: 'idle' },
        });
      }
      return route.continue();
    });
    try {
      // 当前停在 #/settings/update（Tab 会把地址写进 URL）；整页刷新才会重挂载并跑启动自动检测
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2500);
      const dot = page.locator('[data-testid="zp-update-badge"]');
      if (!(await dot.count()) || !(await dot.first().isVisible())) {
        throw new Error('发现新版本后侧栏「面板设置」没有出现徽标');
      }
      const txt = await page.locator('.content').innerText();
      if (!txt.includes('发现新版本')) throw new Error('检查更新页没有醒目提示新版本');
      await shot('07c-update-available');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2000);
      if (!(await page.locator('[data-testid="zp-update-badge"]').first().isVisible())) {
        throw new Error('刷新后侧栏徽标丢了（检测结果没有持久化）');
      }
    } finally {
      await page.unroute('**/api/v1/system/upgrade**');
      await page.evaluate(() => localStorage.removeItem('zp-upgrade-check'));
    }
  });

  await step('升级成功提示会自动消失（回归：「成功绿条永不消失」）', async () => {
    const fresh = new Date().toISOString();
    // 桩要模拟真实语义：dismiss 后服务端终态清成 idle，再 GET 不该又读到同一个 success。
    let dismissed = false;
    const json = (route, data, status = 200) => route.fulfill({
      status, contentType: 'application/json', body: JSON.stringify({ ok: true, data }),
    });
    await page.route('**/api/v1/system/upgrade**', (route) => {
      const u = new URL(route.request().url());
      const m = route.request().method();
      if (u.pathname.endsWith('/upgrade/check') && m === 'POST') {
        return json(route, { current: '1.0.0', latest: '1.0.0', has_update: false });
      }
      if (u.pathname.endsWith('/upgrade/dismiss') && m === 'POST') { dismissed = true; return json(route, { status: 'idle' }); }
      if (u.pathname.endsWith('/upgrade') && m === 'GET') {
        return json(route, {
          current_version: '1.0.0', arch: 'darwin_arm64', build_time: 'x', can_remote: true, pubkey: 'stub',
          can_apply: false, is_root: false, plain_http: false, source: '', effective_source: 'stub',
          staged: false, staged_version: '', notes: '',
          state: dismissed
            ? { status: 'idle' }
            : { status: 'success', to: '1.0.0', message: '升级成功', finished_at: fresh },
        });
      }
      return route.continue();
    });
    try {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2000);
      if (!(await page.locator('text=升级成功：').count())) {
        throw new Error('刚刚完成的成功态应当显示横幅');
      }
      await page.waitForTimeout(9000); // 自动消失 7s + 余量
      if (await page.locator('text=升级成功：').count()) {
        throw new Error('成功提示 9 秒后仍未消失（回归 bug 复发）');
      }
      await shot('07d-success-auto-dismissed');
    } finally {
      await page.unroute('**/api/v1/system/upgrade**');
      await page.evaluate(() => localStorage.removeItem('zp-upgrade-check'));
    }
  });

  await step('「一键更新」一次点击走完 stage → apply → 整页刷新', async () => {
    const calls = [];
    let applied = false;
    const json = (route, data, status = 200) => route.fulfill({
      status, contentType: 'application/json', body: JSON.stringify({ ok: true, data }),
    });
    await page.route('**/api/v1/system/upgrade**', (route) => {
      const u = new URL(route.request().url());
      const m = route.request().method();
      if (u.pathname.endsWith('/upgrade/check') && m === 'POST') {
        return json(route, { current: '1.0.0', latest: '9.9.9', has_update: true, effective_source: 'stub' });
      }
      if (u.pathname.endsWith('/upgrade/stage') && m === 'POST') { calls.push('stage'); return json(route, { staged: true, version: '9.9.9' }); }
      if (u.pathname.endsWith('/upgrade/apply') && m === 'POST') { calls.push('apply'); applied = true; return json(route, { status: 'applying' }, 202); }
      if (u.pathname.endsWith('/upgrade/dismiss') && m === 'POST') return json(route, { status: 'idle' });
      if (u.pathname.endsWith('/upgrade') && m === 'GET') {
        return json(route, {
          current_version: '1.0.0', arch: 'darwin_arm64', build_time: 'x', can_remote: true, pubkey: 'stub',
          can_apply: true, is_root: true, plain_http: false, source: '', effective_source: 'stub',
          staged: false, staged_version: '', notes: '',
          state: applied
            ? { status: 'success', to: '9.9.9', message: '升级成功', finished_at: new Date().toISOString() }
            : { status: 'idle' },
        });
      }
      return route.continue();
    });
    await page.route('**/api/v1/health', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { status: 'ok' } }),
    }));
    let navs = 0;
    const onNav = (f) => { if (f === page.mainFrame()) navs += 1; };
    try {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2000);
      const btn = page.locator('[data-testid="zp-oneclick-update"]');
      if (!(await btn.count())) throw new Error('缺少「一键更新」按钮');
      page.on('framenavigated', onNav);
      navs = 0;
      await btn.first().click();
      // 用户要求「更新要提示更新内容」→ 一键更新会先弹带更新说明的确认框，
      // 断言要跟着走完这一步。
      const confirmOk = page.locator('[data-testid="zp-update-confirm-ok"]');
      await confirmOk.waitFor({ state: 'visible', timeout: 8000 });
      await confirmOk.click();
      await page.waitForTimeout(12000); // stage → apply → 等 status=success + health 200 → reload
      page.off('framenavigated', onNav);
      const si = calls.indexOf('stage');
      const ai = calls.indexOf('apply');
      if (si < 0 || ai < 0) throw new Error('一键更新没有走完 stage→apply：' + JSON.stringify(calls));
      if (si > ai) throw new Error('调用顺序错误（apply 在 stage 之前）：' + JSON.stringify(calls));
      if (navs < 1) throw new Error('升级成功后没有整页刷新');
    } finally {
      await page.unroute('**/api/v1/system/upgrade**');
      await page.unroute('**/api/v1/health');
      await page.evaluate(() => localStorage.removeItem('zp-upgrade-check'));
    }
  });

  await step('用户名表单可用（只验证校验，不改真实账号）', async () => {
    // 刻意**不提交合法的新用户名**（那会真的改名，后面还要用当前账号登录）；成功路径由 Go 测试覆盖。
    // 用户名卡片在「面板设置 → 账号与两步验证」，先回去再切 Tab。
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(800);
    await page.click('button:has-text("账号与两步验证")');
    await page.waitForTimeout(1000);
    const btn = page.locator('button:has-text("修改用户名")');
    if (!(await btn.count())) throw new Error('设置页缺少「修改用户名」入口');
    await shot('07b-username-card');

    const nameInput = page.locator('input[placeholder^="新用户名"]');
    const pwdInput = page.locator('input[placeholder^="当前密码（确认身份）"]');
    if (!(await nameInput.count()) || !(await pwdInput.count())) {
      throw new Error('用户名卡片缺少输入框');
    }

    // (a)(b) 两次点击都**故意**打回 400，这正是要断言的行为，用 expectHTTPError 排除。
    expectHTTPError = true;
    try {
      await nameInput.fill('a');
      await pwdInput.fill('whatever-wrong');
      await btn.click();
      await page.waitForTimeout(1200);
      let t = await page.locator('.toast').last().innerText().catch(() => '');
      if (!/不合法|相同|密码/.test(t)) {
        throw new Error('非法用户名没有给出明确提示，实际: ' + t);
      }

      await nameInput.fill('some-other-name');
      await pwdInput.fill('definitely-wrong-password');
      await btn.click();
      await page.waitForTimeout(1500);
      t = await page.locator('.toast').last().innerText().catch(() => '');
      if (!/密码|凭据/.test(t)) {
        throw new Error('当前密码错误时应提示凭据问题，实际: ' + t);
      }
      await shot('07c-username-rejected');
      await pwdInput.fill('');
    } finally {
      expectHTTPError = false;
    }
  });

  await step('验证访问策略可保存', async () => {
    // 访问策略控件在「访问与安全」Tab；上一步在「账号与两步验证」，先切回来。
    // （设置页的每个步骤都自己选 Tab，不要依赖上一步留在哪儿。）
    await page.click('button:has-text("访问与安全")');
    await page.waitForTimeout(1000);
    await page.selectOption('select.select', 'whitelist');
    await page.fill('textarea.textarea', '100.64.0.0/10\n192.168.1.0/24');
    await page.click('button:has-text("保存设置")');
    await page.waitForSelector('.toast.ok', { timeout: 8000 });
    await page.waitForTimeout(500);
    await shot('07-settings-saved');
    // 恢复默认，避免影响后续访问
    await page.selectOption('select.select', 'any');
    await page.click('button:has-text("保存设置")');
    await page.waitForTimeout(800);
  });

  await step('导航项全部为真实模块（不再有占位页）', async () => {
    // 现在真的逐项点开侧栏：占位页全部替换完了，这条断言才真正有保障。
    const items = await page.locator('.nav-item').allInnerTexts();
    if (items.length < 8) throw new Error('侧边栏导航项过少: ' + items.length);

    const navItems = page.locator('.nav-item');
    const n = await navItems.count();
    const placeholders = [];
    // 「检查更新」现在挂在侧栏：点开它会触发一次真实的 POST /upgrade/check
    // （无外网时可能是 502）。那是被探测页面的正常行为，不该记成前端故障。
    expectHTTPError = true;
    for (let i = 0; i < n; i++) {
      const label = (await navItems.nth(i).innerText()).trim();
      if (!label) continue;
      await navItems.nth(i).click();
      await page.waitForTimeout(1100);
      const txt = await page.locator('.content').innerText();
      if (txt.includes('正在开发中')) placeholders.push(label);
    }
    expectHTTPError = false;
    if (placeholders.length) {
      throw new Error('仍有占位页: ' + placeholders.join(' / '));
    }
    await shot('08-nav-all-real');
  });

  // ---------- 一键 LNMP：先弹窗选版本，确认后才安装（2026-09-19 用户要求）----------
  // 全部走桩，绝不真的安装。锁住三件事：① 点「一键 LNMP」出现选版本弹窗且**此刻没有**请求；
  // ② 选 PHP 8.4 后点「开始安装」才发请求、body 是 php@8.4；③ 取消/关闭绝不安装。
  await step('一键 LNMP：先出现版本选择弹窗、未确认不发安装请求、选 8.4 后请求体正确（桩数据）', async () => {
    const lnmpReqs = [];
    await page.route('**/api/v1/tasks**', (route) => {
      const m = route.request().url().match(/\/api\/v1\/tasks\/([^/?]+)\/stream/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200,
          headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '一键 LNMP', status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '一键 LNMP', status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
      });
    });
    const stubOptions = (status, data) => (route) => route.fulfill({
      status,
      contentType: 'application/json',
      body: JSON.stringify(status === 200 ? { ok: true, data } : { ok: false, msg: 'UI 测试桩：接口不可用' }),
    });
    await page.route('**/api/v1/market/install-lnmp', (route) => {
      lnmpReqs.push(route.request().postData() || '');
      return route.fulfill({
        status: 202, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-lnmp-1', title: '一键 LNMP' } }),
      });
    });
    const OPTIONS = {
      groups: [
        { key: 'nginx', label: 'Nginx', selected: 'nginx',
          options: [{ formula: 'nginx', name: 'Nginx', summary: 'Web 服务器', recommended: true, installed: true }] },
        { key: 'php', label: 'PHP', selected: 'php@8.2',
          options: [
            { formula: 'php@8.4', name: 'PHP 8.4 (FPM)', summary: 'PHP 8.4 运行环境', recommended: false, installed: false },
            { formula: 'php@8.2', name: 'PHP 8.2 (FPM)', summary: 'PHP 8.2 运行环境', recommended: true, installed: true },
          ] },
        { key: 'mysql', label: 'MySQL', selected: 'mysql@8.4',
          options: [{ formula: 'mysql@8.4', name: 'MySQL 8.4', summary: '关系型数据库', recommended: true, installed: false }] },
      ],
      default: { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' },
    };
    const openSitesAndClickLNMP = async () => {
      await page.click('.nav-item:has-text("网站管理")');
      await page.waitForSelector('button:has-text("一键 LNMP")', { timeout: 10000 });
      await page.waitForTimeout(600);
      await page.locator('button:has-text("一键 LNMP")').first().click();
      await page.locator('.modal-mask').last().waitFor({ timeout: 10000 });
      await page.waitForTimeout(500);
    };
    const closeModal = async () => {
      const x = page.locator('.modal-mask').last().locator('button.modal-close').first();
      if (await x.count()) await x.click().catch(() => {});
      await page.waitForTimeout(300);
    };

    await page.route('**/api/v1/market/lnmp-options', stubOptions(200, OPTIONS));
    try {
      await openSitesAndClickLNMP();
      const modalTitle = await page.locator('.modal-mask').last().locator('.modal-head h3').innerText();
      if (!modalTitle.includes('选择要安装的版本')) {
        throw new Error('弹窗标题不对（应说明这是版本选择）：' + modalTitle);
      }
      const modalText = await page.locator('.modal-body').last().innerText();
      if (lnmpReqs.length !== 0) {
        throw new Error('还没确认就发出了安装请求（用户没有选择机会）：' + JSON.stringify(lnmpReqs));
      }
      if (!/已安装/.test(modalText)) {
        throw new Error('弹窗里看不到"已安装"标记（用户重跑时最关心会不会动已装好的东西）');
      }
      if (!/root 口令/.test(modalText)) {
        throw new Error('弹窗里缺少 MySQL root 口令说明');
      }
      await shot('19a-lnmp-options');

      await page.locator('.modal-body input[type=radio][value="php@8.4"]').check();
      await page.waitForTimeout(200);
      const label = await page.locator('.modal-mask button:has-text("开始安装")').last().innerText();
      if (!label.includes('PHP 8.4')) {
        throw new Error('「开始安装」按钮没有写出本次选择（用户无法在点之前核对）：' + label);
      }
      await page.locator('.modal-mask button:has-text("开始安装")').last().click();
      await page.waitForTimeout(1200);
      if (lnmpReqs.length !== 1) {
        throw new Error('确认后应恰好发一次 install-lnmp，实际 ' + lnmpReqs.length + ' 次');
      }
      if (!/"php":"php@8\.4"/.test(lnmpReqs[0])) {
        throw new Error('请求体里应是用户选的 php@8.4（不能退回默认 8.2）：' + lnmpReqs[0]);
      }
      await closeModal();

      lnmpReqs.length = 0;
      await openSitesAndClickLNMP();
      await closeModal();
      await page.waitForTimeout(400);
      if (lnmpReqs.length !== 0) {
        throw new Error('只是打开又关闭弹窗却发了安装请求：' + JSON.stringify(lnmpReqs));
      }
    } finally {
      await page.unroute('**/api/v1/market/lnmp-options');
    }

    const prevExpectHTTPError = expectHTTPError;
    expectHTTPError = true; // 这次的 500 是断言对象，不是前端故障
    await page.route('**/api/v1/market/lnmp-options', stubOptions(500, null));
    try {
      await openSitesAndClickLNMP();
      const modalText = await page.locator('.modal-body').last().innerText();
      if (!/读取可选版本失败/.test(modalText)) {
        throw new Error('接口失败时应明确报错，实际：\n' + modalText.slice(0, 300));
      }
      if (!/用默认版本安装/.test(modalText)) {
        throw new Error('接口失败时应给"用默认版本安装"这个**显式**出口（而不是偷偷用默认值）');
      }
      if (lnmpReqs.length !== 0) {
        throw new Error('接口失败时不该自动开始安装：' + JSON.stringify(lnmpReqs));
      }
      await shot('19b-lnmp-options-failed');
      await closeModal();
    } finally {
      await page.unroute('**/api/v1/market/lnmp-options');
      await page.unroute('**/api/v1/market/install-lnmp');
      await page.unroute('**/api/v1/tasks**');
      expectHTTPError = prevExpectHTTPError;
    }
  });

  await step('打开网站管理', async () => {
    await page.click('.nav-item:has-text("网站管理")');
    await page.waitForSelector('button:has-text("新建站点")', { timeout: 10000 });
    await page.waitForTimeout(800);
    await shot('20-sites');
  });

  // ---------- 网站管理工具条改版（2026-09 / 用户要求）----------
  // ① 工具条只剩「⚙️ 调整配置」，nginx 状态变可点击按钮、二级操作挂进它的菜单；
  // ② nginx 状态只能来自真实探测（运行体优先），没有证据绝不写"运行"。
  const reloadSites = async () => {
    const u = page.url().split('#')[0] + '#/sites';
    if (page.url() !== u) await page.goto(u);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.card-head button.zp-run-btn', { timeout: 15000 });
    // 等 refreshWebEnv 跑完（状态按钮不再是"读取中"）。
    // ⚠️ waitForFunction 第 2 个参数是 arg，options 必须放第 3 个；本机 lnmp-options 约 30s，给 90s。
    await page.waitForFunction(() => {
      const b = document.querySelector('.card-head button.zp-run-btn');
      return !!b && !/读取中/.test(b.textContent || '');
    }, null, { timeout: 90000 });
    await page.waitForTimeout(300);
  };

  await step('网站管理：nginx 状态按钮的展开菜单里是二级操作；工具条只剩一颗「调整配置」（不桩）', async () => {
    await reloadSites();
    if (await page.locator('.card-head button:has-text("⋯ 更多")').count()) {
      throw new Error('工具条上仍有「⋯ 更多」按钮（用户要求用「nginx 运行」替换它）');
    }
    if (await page.locator('.card-head button:has-text("🐘 PHP 环境")').count()) {
      throw new Error('工具条上仍有「🐘 PHP 环境」按钮（用户要求直接去掉）');
    }
    if ((await page.locator('.card-head button:has-text("⚙️ 调整配置")').count()) !== 1) {
      throw new Error('工具条上「⚙️ 调整配置」不是恰好一颗（合并后只留这一个配置入口）');
    }
    const menuBtn = page.locator('.card-head button.zp-run-btn');
    if (!(await menuBtn.count())) {
      throw new Error('工具条上没有「nginx 运行 / nginx 停止」按钮（用户要求用它替换「⋯ 更多」）');
    }
    if (!(await page.locator('.card-head .zp-menu button:has-text("校验 nginx")').count())) {
      throw new Error('nginx 展开菜单里没有「🧪 校验 nginx」');
    }
    if (!(await page.locator('.card-head .zp-menu button:has-text("重建全部配置")').count())) {
      throw new Error('nginx 展开菜单里没有「♻️ 重建全部配置」');
    }
    const menu = page.locator('.card-head .zp-menu').first();
    if (await menu.isVisible()) throw new Error('展开菜单默认就是打开的（应当 hover / 点击才展开）');
    await menuBtn.first().click();
    await page.waitForTimeout(400);
    if (!(await menu.isVisible())) throw new Error('点击「nginx 运行」后菜单没有展开');
    const menuText = await menu.innerText();
    if (!menuText.includes('校验 nginx') || !menuText.includes('重建全部配置')) {
      throw new Error('展开菜单里缺少「校验 nginx」或「重建全部配置」：\n' + menuText);
    }
    if (!menuText.includes('nginx -t')) throw new Error('「校验 nginx」项缺少说明文案（nginx -t）');
    if (!menuText.includes('手工改坏') || !menuText.includes('修复 Nginx 环境')) {
      throw new Error('展开菜单里缺少「修复 Nginx 环境」及其说明文案：\n' + menuText);
    }
    await shot('20a-sites-nginx-menu');
    // 再点一次收起（触屏用户也要能关）。断言 .open 被去掉 + 指针移开、失焦后不可见；
    // 指针悬停或持有焦点时保持可见是有意的（键盘用户 Tab 进来时菜单不该自己关）。
    await menuBtn.first().click();
    await page.waitForTimeout(300);
    if (await page.locator('.card-head .zp-menu-wrap.open').count()) {
      throw new Error('再点一次之后仍然带着 .open（点击没能收起菜单）');
    }
    await page.evaluate(() => { if (document.activeElement) document.activeElement.blur(); });
    await page.mouse.move(5, 500);
    await page.waitForTimeout(400);
    if (await menu.isVisible()) throw new Error('移开鼠标并让按钮失焦后菜单仍然展开');

    // nginx 状态文案必须与**真实探测**一致。判据顺序与 sites.js 的 nginxState() 逐字对齐：
    // 运行体（/sites/runtime）优先，/services 的 state.running 只在读不到时兜底。
    const real = await page.evaluate(async () => {
      const j = async (p) => {
        const r = await fetch(new URL(p, document.baseURI),
          { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
        return r.json();
      };
      const [rt, svc] = await Promise.all([j('api/v1/sites/runtime'), j('api/v1/services?health=0')]);
      return { nginx: (rt.data || {}).nginx || null, list: (svc.data || {}).list || [] };
    });
    const ng = real.list.find((s) => /^nginx/i.test(String(s.name || ''))
      || /^nginx/i.test(String(s.display_name || '')));
    const want = real.nginx
      ? (real.nginx.running ? 'nginx 运行' : 'nginx 停止')
      : (!ng ? 'nginx 状态未知' : ((ng.state || {}).running ? 'nginx 运行' : 'nginx 停止'));
    const headText = await page.locator('.card-head').first().innerText();
    if (!headText.includes(want)) {
      throw new Error('nginx 状态文案与真实探测不一致（期望「' + want + '」）：\n' + headText);
    }
    await shot('20b-sites-nginx-status');
  });

  // ---------- 「⚙️ 配置文件」入口（用户要求）----------
  // 只验证入口在、清单来自后端、路径不是前端拼的；**不点「📝 编辑」**（会碰真机配置）。
  await step('网站管理：调整配置 → 配置文件 清单来自后端，且不写任何东西', async () => {
    await reloadSites();
    const btn = page.locator('.card-head button:has-text("⚙️ 调整配置")');
    if (!(await btn.count())) {
      throw new Error('工具条上没有「⚙️ 调整配置」入口（用户要求面板里能手动改 nginx/php）');
    }
    const tip = (await btn.first().getAttribute('title')) || '';
    if (!tip.includes('配置文件清单') || !tip.includes('性能调整')) {
      throw new Error('「⚙️ 调整配置」的 tooltip 没有说清它包含哪几块：' + tip);
    }
    await btn.first().click();
    await page.waitForSelector('.modal-mask:has-text("调整配置")', { timeout: 10000 });
    const filesTab = page.locator('.modal button.zp-seg-btn').filter({ hasText: '配置文件' }).first();
    if (!(await filesTab.count())) throw new Error('「调整配置」弹窗里没有「配置文件」页签');
    await filesTab.click();
    await page.waitForTimeout(1200);
    const bodyText = await page.locator('.modal-mask').last().innerText();
    if (!bodyText.includes('nginx')) throw new Error('配置文件清单里没有 nginx：\n' + bodyText);
    const listed = await page.locator('.modal-mask').last().locator('code.code').allInnerTexts();
    if (!listed.some((p) => p.endsWith('nginx.conf'))) {
      throw new Error('配置文件清单里没有 nginx.conf 的**绝对路径**：' + JSON.stringify(listed));
    }
    if (listed.some((p) => p.includes('{brew}'))) {
      throw new Error('路径里的 {brew} 占位符没有被后端展开：' + JSON.stringify(listed));
    }
    if (listed.some((p) => !p.startsWith('/'))) {
      throw new Error('配置文件路径必须是绝对路径（相对路径会被文件接口的越界校验拒绝）：' + JSON.stringify(listed));
    }
    if (!bodyText.includes('保存不等于生效')) {
      throw new Error('清单里没有说明"保存后需要重载/重启才生效"（宝塔那类会给重载入口）');
    }
    await shot('20c-sites-config-files');
    await closeAnyModal(page);
  });

  await step('一键 LNMP 入口与真实环境一致（本机三层实测，不桩）', async () => {
    await reloadSites();
    const facts = await page.evaluate(async () => {
      const j = async (p) => {
        const r = await fetch(new URL(p, document.baseURI),
          { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
        return r.json();
      };
      const [b, l] = await Promise.all([
        j('api/v1/system/base-env'), j('api/v1/market/lnmp-options'),
      ]);
      return { base: b.data, lnmp: l.data };
    });
    const miss = [...(((facts.base || {}).missing) || [])];
    const groups = ((facts.lnmp || {}).groups) || [];
    ['nginx', 'php', 'mysql'].forEach((k) => {
      const g = groups.find((x) => x.key === k);
      if (!g || !g.options || !g.options.length || !g.options.some((o) => o.installed)) {
        miss.push(g ? g.label : k);
      }
    });
    const wantShown = miss.length > 0;
    const has = (await page.locator('.content button:has-text("一键 LNMP")').count()) > 0;
    if (has !== wantShown) {
      throw new Error('「一键 LNMP」入口与真实环境不一致：真实缺失=' + JSON.stringify(miss)
        + '（应' + (wantShown ? '显示' : '不显示') + '），实际' + (has ? '显示' : '不显示'));
    }
    if (wantShown) {
      const title = await page.locator('.content button:has-text("一键 LNMP")').first().getAttribute('title');
      for (const m of miss) {
        if (!title.includes(m)) throw new Error('按钮 title 没列出真实缺失项「' + m + '」：' + title);
      }
    }
    await shot('20c-lnmp-entry-real');
  });

  await step('nginx 状态按钮：桩造"运行 / 停止 / 无证据"三态（判据：运行体优先、服务记录兜底）', async () => {
    // 判据在 sites.js 的 nginxState()：运行体优先、服务记录兜底（只桩 /services 测不到真判据）。
    const rtRoute = /\/api\/v1\/sites\/runtime/;
    const svcRoute = /\/api\/v1\/services\?health=0/;
    // 这段所有响应都是自己桩的（含刻意的 500），整段开着 expectHTTPError，finally 还原。
    expectHTTPError = true;
    // lnmp-options 实测约 30s，桩掉它，给一份"都装好了"的桩。
    const lnmpRoute = '**/api/v1/market/lnmp-options';
    const stubRT = (nginx) => (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { nginx, php: { running: true }, mysql: { running: true } } }),
    });
    const stubSvc = (body) => (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { list: body } }),
    });
    await page.route(lnmpRoute, (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { groups: [
        { key: 'nginx', label: 'Nginx', selected: 'nginx',
          options: [{ formula: 'nginx', name: 'Nginx', installed: true, recommended: true }] },
        { key: 'php', label: 'PHP', selected: 'php@8.2',
          options: [{ formula: 'php@8.2', name: 'PHP 8.2', installed: true, recommended: true }] },
        { key: 'mysql', label: 'MySQL', selected: 'mysql@8.4',
          options: [{ formula: 'mysql@8.4', name: 'MySQL 8.4', installed: true, recommended: true }] },
      ], default: { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' } } }),
    }));
    try {
      await page.route(rtRoute, stubRT({ running: true, evidence: '桩：nginx master 进程在跑' }));
      await page.unroute(svcRoute).catch(() => {});
      await page.route(svcRoute, stubSvc([]));
      await reloadSites();
      let t = await page.locator('.card-head').first().innerText();
      if (!t.includes('nginx 运行')) throw new Error('运行体说在跑时按钮没写「nginx 运行」：\n' + t);
      await shot('20d-nginx-running');

      await page.unroute(rtRoute);
      await page.route(rtRoute, stubRT({ running: false, evidence: '桩：没有 nginx 进程' }));
      await reloadSites();
      t = await page.locator('.card-head').first().innerText();
      if (t.includes('nginx 运行')) throw new Error('运行体说没进程时却出现「nginx 运行」：\n' + t);
      if (!t.includes('nginx 停止')) throw new Error('运行体说没进程时按钮没写「nginx 停止」：\n' + t);
      await shot('20e-nginx-stopped');

      // ③ 运行体读不到且服务记录里也没有 nginx → 「nginx 状态未知」
      await page.unroute(rtRoute);
      await page.route(rtRoute, (route) => route.fulfill({
        status: 500, contentType: 'application/json',
        body: JSON.stringify({ ok: false, msg: '桩：运行体探测失败' }),
      }));
      await page.unroute(svcRoute);
      await page.route(svcRoute, stubSvc([
        { name: 'php', display_name: 'PHP 8.2 (FPM)', state: { running: true, status: 'running' } },
      ]));
      await reloadSites();
      t = await page.locator('.card-head').first().innerText();
      if (t.includes('nginx 运行') || t.includes('nginx 停止')) {
        throw new Error('运行体读不到、服务记录里也没有 nginx 时却写了运行/停止（没有证据）：\n' + t);
      }
      if (!t.includes('nginx 状态未知')) throw new Error('没有证据时按钮没写「nginx 状态未知」：\n' + t);
      await shot('20e2-nginx-unknown');

      await page.unroute(svcRoute);
      await page.route(svcRoute, stubSvc([
        { name: 'nginx', display_name: 'Nginx', state: { running: true, status: 'running' } },
      ]));
      await reloadSites();
      t = await page.locator('.card-head').first().innerText();
      if (!t.includes('nginx 运行')) {
        throw new Error('运行体读不到时应回落到服务记录（state.running=true → 「nginx 运行」）：\n' + t);
      }
      await shot('20e3-nginx-fallback-services');
    } finally {
      expectHTTPError = false;
      await page.unroute(rtRoute).catch(() => {});
      await page.unroute(svcRoute).catch(() => {});
      await page.unroute(lnmpRoute).catch(() => {});
    }
  });

  await step('一键 LNMP：完整→不显示；缺 Homebrew+PHP→显示并如实列出，且未点「开始安装」不发请求（桩数据）', async () => {
    const requests = [];
    const stub = (data) => (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data }),
    });
    const completeBase = { clt_ok: true, brew_ok: true, deps_ok: true, ready: true, missing: [] };
    const optionsAllInstalled = {
      groups: [
        { key: 'nginx', label: 'Nginx', selected: 'nginx',
          options: [{ formula: 'nginx', name: 'Nginx', installed: true, recommended: true }] },
        { key: 'php', label: 'PHP', selected: 'php@8.2',
          options: [{ formula: 'php@8.2', name: 'PHP 8.2', installed: true, recommended: true }] },
        { key: 'mysql', label: 'MySQL', selected: 'mysql@8.4',
          options: [{ formula: 'mysql@8.4', name: 'MySQL 8.4', installed: true, recommended: true }] },
      ],
      default: { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' },
    };
    const missingBase = { clt_ok: true, brew_ok: false, deps_ok: false, ready: false, missing: ['Homebrew'] };
    const optionsMissingPHP = {
      groups: [
        { key: 'nginx', label: 'Nginx', selected: 'nginx',
          options: [{ formula: 'nginx', name: 'Nginx', installed: true, recommended: true }] },
        { key: 'php', label: 'PHP', selected: 'php@8.2',
          options: [
            { formula: 'php@8.2', name: 'PHP 8.2', installed: false, recommended: true },
            { formula: 'php@8.4', name: 'PHP 8.4', installed: false },
          ] },
        { key: 'mysql', label: 'MySQL', selected: 'mysql@8.4',
          options: [{ formula: 'mysql@8.4', name: 'MySQL 8.4', installed: true, recommended: true }] },
      ],
      default: { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' },
    };

    await page.route('**/api/v1/market/install-lnmp', (route) => {
      requests.push(route.request().postData() || '');
      return route.fulfill({
        status: 202, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-lnmp-gate', title: '一键 LNMP' } }),
      });
    });
    // 任务进度流：回一条"成功"并结束，否则任务窗会重连不存在的流刷 404。
    await page.route('**/api/v1/tasks**', (route) => {
      const m = route.request().url().match(/\/api\/v1\/tasks\/([^/?]+)\/stream/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200,
          headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '一键 LNMP', status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '一键 LNMP', status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
      });
    });

    try {
      // ① 环境完整（底座就绪 + 三件套都装了任意版本）→ 入口**不出现**
      await page.route('**/api/v1/system/base-env', stub(completeBase));
      await page.route('**/api/v1/market/lnmp-options', stub(optionsAllInstalled));
      await reloadSites();
      if (await page.locator('.content button:has-text("一键 LNMP")').count()) {
        throw new Error('环境完整时仍显示「一键 LNMP」（用户要求：不该显示）');
      }
      await shot('20f-lnmp-complete-hidden');

      // ② 缺 Homebrew（底座）+ 整组 PHP 都没装 → 入口出现，title 如实列出
      await page.unroute('**/api/v1/system/base-env');
      await page.unroute('**/api/v1/market/lnmp-options');
      await page.route('**/api/v1/system/base-env', stub(missingBase));
      await page.route('**/api/v1/market/lnmp-options', stub(optionsMissingPHP));
      await reloadSites();
      const btn = page.locator('.content button:has-text("一键 LNMP")');
      if (!(await btn.count())) throw new Error('缺失（Homebrew + PHP）时却没有「一键 LNMP」入口');
      const title = await btn.first().getAttribute('title');
      if (!title.includes('当前缺失"Homebrew、PHP"')) {
        throw new Error('按钮 title 没有逐字写「当前缺失"Homebrew、PHP"」：' + title);
      }
      if (!title.includes('已安装的环境不会重复安装')) {
        throw new Error('按钮 title 缺少"已安装的环境不会重复安装"：' + title);
      }
      await shot('20g-lnmp-missing-shown');

      // ③ 打开弹窗：顶部显著提醒缺失项；**仍未发出**任何安装请求
      await btn.first().click();
      await page.waitForSelector('.modal-mask button:has-text("开始安装")', { timeout: 10000 });
      await page.waitForTimeout(500);
      const modalText = await page.locator('.modal-body').last().innerText();
      if (!/当前缺失：Homebrew、PHP/.test(modalText)) {
        throw new Error('弹窗顶部没有显著写出「当前缺失：Homebrew、PHP」：\n' + modalText.slice(0, 400));
      }
      if (!/已安装的环境不会重复安装/.test(modalText)) {
        throw new Error('弹窗缺少"已安装的环境不会重复安装，本次会跳过"');
      }
      if (requests.length !== 0) {
        throw new Error('只打开弹窗就发了安装请求（用户没有二次点击「开始安装」的机会）：'
          + JSON.stringify(requests));
      }
      await shot('20h-lnmp-dialog-missing');

      // ④ 用户再点一次「开始安装」→ 才发请求（桩回 202 假任务，不真装）
      await page.locator('.modal-mask button:has-text("开始安装")').last().click();
      await page.waitForTimeout(1200);
      if (requests.length !== 1) {
        throw new Error('点「开始安装」后应恰好发一次 install-lnmp，实际 ' + requests.length + ' 次');
      }
      await closeAnyModal(page);
    } finally {
      await page.unroute('**/api/v1/system/base-env');
      await page.unroute('**/api/v1/market/lnmp-options');
      await page.unroute('**/api/v1/market/install-lnmp');
      await page.unroute('**/api/v1/tasks**');
    }
  });

  await step('PHP 环境（调整配置 → PHP 环境）与真实 /api/v1/php 一致（本机实测：悬空别名不再算成一个版本）', async () => {
    await reloadSites();
    const real = await page.evaluate(async () => {
      const r = await fetch(new URL('api/v1/php', document.baseURI),
        { headers: { Accept: 'application/json' }, credentials: 'same-origin' });
      return r.json();
    });
    const want = ((real && real.data && real.data.list) || []).map((p) => 'PHP ' + p.version);
    await page.click('.card-head button:has-text("⚙️ 调整配置")');
    await page.waitForSelector('.modal-mask', { timeout: 10000 });
    // PHP 环境现在是「调整配置」里的一页（用户要求删掉工具条上那颗独立按钮）
    await page.locator('.modal button.zp-seg-btn').filter({ hasText: 'PHP 环境' }).first().click();
    await page.waitForTimeout(800);
    const mask = page.locator('.modal-mask').last();
    if (!want.length) {
      // 版本列表为空时必须给"去应用市场装"的引导，而不是空白弹窗。
      await mask.locator('.empty').waitFor({ timeout: 8000 });
      const t = await mask.innerText();
      if (!/应用市场/.test(t)) throw new Error('没有 PHP 时弹窗没有引导去「应用市场」：\n' + t);
      await shot('20k-php-env-empty');
      await closeAnyModal(page);
      return;
    }
    await mask.locator('table.table tbody tr').first().waitFor({ timeout: 8000 });
    const rows = mask.locator('table.table tbody tr');
    const got = [];
    for (let i = 0; i < await rows.count(); i += 1) {
      got.push((await rows.nth(i).locator('td').first().innerText()).replace(/\s+/g, ' ').trim());
    }
    for (const v of want) {
      if (!got.some((g) => g.includes(v))) {
        throw new Error('弹窗缺少真实存在的版本 ' + v + '：UI=' + JSON.stringify(got) + ' API=' + JSON.stringify(want));
      }
    }
    if (got.length !== want.length) {
      throw new Error('弹窗行数与真实 /api/v1/php 不一致：UI=' + JSON.stringify(got) + ' API=' + JSON.stringify(want));
    }
    for (let i = 0; i < got.length; i += 1) {
      if (!(await rows.nth(i).locator('button:has-text("卸载")').count())) {
        throw new Error(got[i] + ' 行没有「🗑 卸载」按钮');
      }
    }
    await shot('20k-php-env-real');
    await closeAnyModal(page);
  });

  await step('PHP 环境页：每个版本都有「🗑 卸载」，点击先出确认框，确认后才发卸载请求（桩数据）', async () => {
    const dels = [];
    const phpStub = (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: {
        list: [
          { version: '8.2', service: 'php@8.2', pass: '/tmp/zp-php82.sock', running: true,
            is_default: true, listen_ok: true, preferred_pass: '/tmp/zp-php82.sock' },
          // 8.4 在真机上是**无版本别名** `php`（service='php'），但目录 id 必须是 php84。
          { version: '8.4', service: 'php', pass: '/tmp/zp-php84.sock', running: false,
            is_default: false, listen_ok: false, listen_err: '桩：未配置端点',
            preferred_pass: '/tmp/zp-php84.sock' },
        ],
        socket_dir: '/tmp', note: 'UI 测试桩：两个 PHP 版本',
      } }),
    });
    await page.route('**/api/v1/php', phpStub);
    await page.route(/\/api\/v1\/market\/[^/?]+/, (route) => {
      if (route.request().method() !== 'DELETE') return route.continue();
      dels.push(route.request().url());
      return route.fulfill({
        status: 202, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-php-uninstall', title: '卸载 PHP' } }),
      });
    });
    await page.route('**/api/v1/tasks**', (route) => {
      const m = route.request().url().match(/\/api\/v1\/tasks\/([^/?]+)\/stream/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200, headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '卸载 PHP', status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '卸载 PHP', status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
      });
    });
    try {
      await reloadSites();
      await page.click('.card-head button:has-text("⚙️ 调整配置")');
      await page.waitForSelector('.modal-mask', { timeout: 10000 });
      await page.locator('.modal button.zp-seg-btn').filter({ hasText: 'PHP 环境' }).first().click();
      await page.waitForSelector('.modal-mask table.table tbody tr', { timeout: 10000 });
      await page.waitForTimeout(400);
      const rows = page.locator('.modal-mask').last().locator('table.table tbody tr');
      const n = await rows.count();
      if (n !== 2) throw new Error('PHP 桩数据应有 2 行，实际 ' + n);
      for (let i = 0; i < n; i++) {
        if (!(await rows.nth(i).locator('button:has-text("卸载")').count())) {
          throw new Error('PHP 环境弹窗第 ' + (i + 1) + ' 行没有「🗑 卸载」按钮（用户找不到卸载入口）');
        }
      }
      await shot('20i-php-env-uninstall');

      await rows.nth(0).locator('button:has-text("卸载")').click();
      await page.waitForSelector('.modal-mask:has(button:has-text("卸载 PHP 8.2"))', { timeout: 8000 });
      const c1 = await page.locator('.modal-mask').last().locator('.modal-body').innerText();
      if (!c1.includes('502')) throw new Error('默认版本的卸载确认框没有警告"站点会 502"：\n' + c1);
      if (!c1.includes('brew uninstall php@8.2')) {
        throw new Error('确认框没写清会执行 brew uninstall php@8.2：\n' + c1);
      }
      if (dels.length !== 0) throw new Error('只弹出确认框就发了卸载请求：' + JSON.stringify(dels));
      await shot('20j-php-uninstall-confirm');
      await page.locator('.modal-mask').last().locator('button:has-text("取消")').click();
      await page.waitForTimeout(400);
      if (dels.length !== 0) throw new Error('取消卸载却发了请求：' + JSON.stringify(dels));

      // ② 非默认版本：确认后才**恰好发一次** DELETE（应用 id 由 service 推导：php@8.4 → php84）
      const rows2 = page.locator('.modal-mask').last().locator('table.table tbody tr');
      await rows2.nth(1).locator('button:has-text("卸载")').click();
      await page.waitForSelector('.modal-mask:has(button:has-text("卸载 PHP 8.4"))', { timeout: 8000 });
      await page.locator('.modal-mask').last().locator('button:has-text("卸载 PHP 8.4")').click();
      await page.waitForTimeout(1200);
      if (dels.length !== 1) {
        throw new Error('确认后应恰好发一次卸载请求，实际 ' + dels.length + ' 次：' + JSON.stringify(dels));
      }
      if (!/\/api\/v1\/market\/php84\?remove_data=0/.test(dels[0])) {
        throw new Error('卸载请求的应用 id 应是目录 id php84（由 service=php@8.4 推导）：' + dels[0]);
      }
      await closeAnyModal(page);
    } finally {
      await page.unroute('**/api/v1/php');
      await page.unroute(/\/api\/v1\/market\/[^/?]+/);
      await page.unroute('**/api/v1/tasks**');
    }
  });

  await privStep('新建一个 PHP 站点', async () => {
    await page.click('button:has-text("新建站点")');
    const domainInput = page.locator('input[placeholder="例如：demo.test"]');
    await domainInput.waitFor({ timeout: 8000 });
    await domainInput.fill(TEST_SITE);
    await page.locator('input[placeholder="可选，便于自己识别"]').fill('UI 自动化测试站点');
    await page.selectOption('select.select >> nth=0', 'generic');
    await shot('21-site-new');
    await page.click('button:has-text("创建站点")');
    await page.waitForTimeout(4000);
    await shot('22-site-created');
    // 必须断言"真的建成了"：原来只截图不断言，建站失败也打印 OK，错误被推给下一步。
    const body = await page.locator('.content').innerText();
    const okToast = await page.locator('.toast').count();
    if (!body.includes(TEST_SITE) && !okToast) {
      throw new Error('点了「创建站点」但既没建出站点、也没弹出任何提示（建站步骤静默失败了）');
    }
  });

  await privStep('站点列表出现新站点且配置存在', async () => {
    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
    await page.click('button:has-text("⟳ 刷新")');
    await page.waitForTimeout(1500);
    const txt = await page.locator('.content').innerText();
    if (!txt.includes(TEST_SITE)) throw new Error('站点未出现在列表中');
    if (!txt.includes('运行中')) throw new Error('站点状态不是运行中（nginx 配置可能缺失）');
    await shot('23-site-listed');
  });

  await privStep('诊断站点（真实 HTTP 探测）', async () => {
    const row = page.locator('tr', { hasText: TEST_SITE });
    await row.locator('button:has-text("诊断")').click();
    await page.waitForSelector('text=访问地址', { timeout: 15000 });
    await page.waitForTimeout(1500);
    await shot('24-site-check');
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('HTTP 状态码')) throw new Error('诊断结果缺少状态码');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await step('网站管理：列表第一行是默认站点（打开/重建/查看状态），域名文本本身是外链', async () => {
    // 用户改口径：**只显示域名文本本身**，且这段文字本身就是链接。
    await reloadSites();
    const rows = page.locator('.zp-table-wrap table.table tbody tr');
    if (!(await rows.count())) throw new Error('站点列表没有渲染出表格（默认站点这一行必须永远在）');
    const first = rows.first();
    if (!(await first.locator('text=默认站点').count())) {
      throw new Error('列表第一行不是默认站点：\n' + (await first.innerText()));
    }
    for (const label of ['打开', '查看状态']) {
      if (!(await first.locator(`button:has-text("${label}")`).count())) {
        throw new Error('默认站点行缺少「' + label + '」按钮：\n' + (await first.innerText()));
      }
    }
    if (!(await first.locator('button:has-text("重建"), button:has-text("创建")').count())) {
      throw new Error('默认站点行缺少「重建 / 创建」按钮：\n' + (await first.innerText()));
    }
    await shot('23b-sites-default-row');

    const siteRows = page.locator('.zp-table-wrap table.table tbody tr:not(.zp-default-row)');
    const n = await siteRows.count();
    if (!n) return;
    const cell = siteRows.first().locator('td').first();
    const links = await cell.locator('a').evaluateAll(
      (as) => as.map((a) => ({
        href: a.getAttribute('href'),
        target: a.getAttribute('target'),
        rel: a.getAttribute('rel'),
        text: (a.textContent || '').trim(),
      })));
    if (!links.length) throw new Error('站点域名处没有链接（用户要求域名文本本身可点）');
    if (links.length !== 1) {
      throw new Error('域名处只应有 1 个链接（不再并列 http:// 与 https://）：' + JSON.stringify(links));
    }
    const l = links[0];
    if (!/^https?:\/\//.test(String(l.href || ''))) throw new Error('域名链接不是 http(s)：' + JSON.stringify(l));
    if (l.target !== '_blank') throw new Error('域名链接没有 target=_blank：' + JSON.stringify(l));
    if (!String(l.rel || '').includes('noopener')) throw new Error('域名链接没有 rel=noopener：' + JSON.stringify(l));
    if (/^https?:\/\//i.test(l.text)) {
      throw new Error('域名链接的文本带协议前缀（用户明确不要）：' + JSON.stringify(l));
    }
    const cellText = await cell.innerText();
    if (cellText.includes('http://') || cellText.includes('https://')) {
      throw new Error('域名列里仍出现了 http:// 或 https:// 文本：\n' + cellText);
    }
    if (!/(https?|🔒)/.test(cellText)) {
      throw new Error('域名列丢失了协议信息（协议要用小字/title/图标表达，不能丢）：\n' + cellText);
    }
    await shot('23c-sites-domain-links');
  });


  await privStep('校验 nginx', async () => {
    // 「校验 nginx」挂在 nginx 状态按钮的展开菜单里，先展开再点。
    await page.click('.card-head button.zp-run-btn');
    await page.waitForSelector('.card-head .zp-menu button:has-text("校验 nginx")',
      { state: 'visible', timeout: 8000 });
    await page.click('.card-head .zp-menu button:has-text("校验 nginx")');
    await page.waitForSelector('.toast', { timeout: 15000 });
    await page.waitForTimeout(800);
    const toastText = await page.locator('.toast').first().innerText();
    if (!toastText.includes('通过')) throw new Error('nginx 配置校验未通过: ' + toastText);
    await shot('25-nginx-test');
  });

  await privStep('删除测试站点（保留文件）', async () => {
    const row = page.locator('tr', { hasText: TEST_SITE });
    await row.locator('button:has-text("删除")').click();
    await page.waitForSelector('button:has-text("确认删除")', { timeout: 8000 });
    await shot('26-site-delete');
    await page.click('button:has-text("确认删除")');
    await page.waitForTimeout(3000);
    await page.click('button:has-text("⟳ 刷新")');
    await page.waitForTimeout(1200);
    const txt = await page.locator('.content').innerText();
    if (txt.includes(TEST_SITE)) throw new Error('站点删除后仍在列表中');
    await shot('27-site-deleted');
  });

  // ---------- 「我的应用」（P3；「服务管理」与「应用市场」合并成一个「应用」版块，
  // 两个 Tab；老 hash #/services 会落到「已安装」）----------
  await step('打开我的应用', async () => {
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);
    await shot('30-services');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('/')) throw new Error('服务页未渲染统计信息');
  });

  await step('服务卡片显示真实状态', async () => {
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('运行中')) throw new Error('未显示任何运行中的服务');
  });

  // ---------- 卸载：点下去必须有可见反馈（回归：坑 154）----------
  // 用户报障"点「🗑 卸载」没有任何反馈，也没真的卸载"。全部走桩：必须出现确认框；确认（202）→ 进度窗
  // 且 DELETE 真的发出；500 → 带原因的 toast；blocked 时按钮**不能 disabled**；请求挂住也要说话。
  await step('卸载：点按钮必须有确认框 / 任务窗 / 明确错误（桩数据，不碰真实服务）', async () => {
    const FAKES = [
      { name: 'uitest-svc-ok', display_name: 'UITEST 卸载·正常', kind: 'native', managed: true, state: { running: true, status: 'running' } },
      { name: 'uitest-svc-fail', display_name: 'UITEST 卸载·失败', kind: 'native', managed: true, state: { running: true, status: 'running' } },
      { name: 'uitest-svc-slow', display_name: 'UITEST 卸载·被挂住', kind: 'native', managed: true, state: { running: true, status: 'running' } },
      // 记录「仅纳管」+ 目录说面板装的 → 给市场式「卸载」，其 uninstall.blocked 非空 = 不能卸。
      { name: 'uitest-svc-blocked', display_name: 'UITEST 卸载·被阻止', kind: 'native', managed: false, state: { running: true, status: 'running' } },
    ];
    const BLOCKED_REASON = 'UITEST 桩：还有别的应用在用它';
    const uninstallCalls = [];

    expectHTTPError = true;

    const clearToasts = () => page.evaluate(() => {
      document.querySelectorAll('.toasts .toast').forEach((n) => n.remove());
    });

    const closeAllModals = async () => {
      for (let i = 0; i < 5; i++) {
        const masks = page.locator('.modal-mask');
        if (!(await masks.count())) break;
        const x = masks.last().locator('button.modal-close').first();
        if (await x.count()) await x.click().catch(() => {});
        await page.waitForTimeout(250);
      }
    };

    const openUninstallBtn = async (displayName) => {
      const card = page.locator('#installed-grid > div', { hasText: displayName }).first();
      await card.waitFor({ timeout: 15000 });
      await card.locator('button:has-text("管理")').click();
      const btn = page.locator('.modal-mask button:has-text("🗑 卸载")').last();
      await btn.waitFor({ timeout: 8000 });
      return btn;
    };

    await page.route('**/api/v1/services**', async (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      const un = path.match(/^services\/([^/]+)\/uninstall$/);
      if (req.method() === 'DELETE' && un) {
        const name = decodeURIComponent(un[1]);
        uninstallCalls.push(name);
        if (name === 'uitest-svc-fail') {
          return route.fulfill({
            status: 500, contentType: 'application/json',
            body: JSON.stringify({ ok: false, msg: 'UITEST 桩：服务不存在' }),
          });
        }
        const body = JSON.stringify({ ok: true, data: { task_id: 'uitest-task-' + name, title: '卸载 ' + name } });
        if (name === 'uitest-svc-slow') await new Promise((r) => setTimeout(r, 4000));
        return route.fulfill({ status: 202, contentType: 'application/json', body });
      }
      if (req.method() === 'GET' && (path === 'services' || path === 'services/health')) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { list: FAKES } }) });
      }
      if (req.method() === 'GET' && /^services\/[^/]+$/.test(path)) {
        const f = FAKES.find((x) => path === 'services/' + x.name) || FAKES[0];
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: f }) });
      }
      if (req.method() === 'GET' && /credentials$/.test(path)) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { credentials: [] } }) });
      }
      return route.continue();
    });
    // 市场目录只放那条"有卸载计划但当前被阻止"的条目。
    await page.route('**/api/v1/market**', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({
        ok: true,
        data: {
          list: [{
            id: 'uitest-svc-blocked',
            name: 'UITEST 卸载·被阻止',
            installed: true,
            uninstall: { kind: 'installer', steps: ['停止服务', '删除残留'], blocked: BLOCKED_REASON },
          }],
        },
      }),
    }));
    // 假任务的进度流必须立刻回"成功"并结束，否则假任务会一直重连不存在的流刷 404。
    await page.route('**/api/v1/tasks**', (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      const m = path.match(/^tasks\/([^/]+)\/stream$/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200,
          headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '卸载 ' + id, status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '卸载 ' + id, status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({
        status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
      });
    });

    await closeAllModals();
    // **必须制造一次 hash 变化**，否则视图不会用刚注册的桩重新拉服务列表（直接点同一导航项是空操作）。
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(500);
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);

    await (await openUninstallBtn('UITEST 卸载·正常')).click();
    const confirm = page.locator('.modal-mask', { hasText: '卸载服务' }).last();
    await confirm.locator('button:has-text("确认卸载")').waitFor({ timeout: 8000 });
    await shot('30a-uninstall-confirm');

    await confirm.locator('button:has-text("确认卸载")').click();
    await page.locator('.modal-mask', { hasText: '关闭窗口（后台继续）' }).last().waitFor({ timeout: 10000 });
    if (!uninstallCalls.includes('uitest-svc-ok')) {
      throw new Error('点了确认卸载，但 DELETE 没有发出去：' + JSON.stringify(uninstallCalls));
    }
    await shot('30b-uninstall-task-window');
    await closeAllModals();
    await clearToasts();

    // ④ 后端失败（桩 500）→ 必须出现带原因的 toast，且**不能**弹进度窗
    await (await openUninstallBtn('UITEST 卸载·失败')).click();
    await page.locator('.modal-mask button:has-text("确认卸载")').last().click();
    const errToast = page.locator('.toasts .toast.err').first();
    await errToast.waitFor({ timeout: 8000 });
    const errText = await errToast.innerText();
    if (!/UITEST 桩：服务不存在/.test(errText)) {
      throw new Error('卸载失败没有把原因显示出来：' + errText);
    }
    if (await page.locator('.modal-mask', { hasText: '关闭窗口（后台继续）' }).count()) {
      throw new Error('卸载提交失败了，却弹出了任务进度窗（谎报成功）');
    }
    await page.waitForTimeout(400); // 等 toast 淡入动画走完，截图里要看得见它
    await shot('30c-uninstall-fail-visible');
    await closeAllModals();
    await clearToasts();

    const blockedCard = page.locator('#installed-grid > div', { hasText: 'UITEST 卸载·被阻止' }).first();
    await blockedCard.waitFor({ timeout: 15000 });
    await blockedCard.locator('button:has-text("管理")').click();
    const blockedBtn = page.locator('.modal-mask button:text-is("卸载")').last();
    await blockedBtn.waitFor({ timeout: 8000 });
    if (await blockedBtn.isDisabled()) {
      // 禁用的按钮点下去什么都不发生（原因还只在悬浮提示里）= 用户眼里的"点了没反应"
      throw new Error('卸载计划被 blocked 时按钮是 disabled 的：点击不会有任何反馈');
    }
    const masksBefore = await page.locator('.modal-mask').count();
    await blockedBtn.click();
    const blockedToast = page.locator('.toasts .toast.warn').first();
    await blockedToast.waitFor({ timeout: 8000 });
    const blockedText = await blockedToast.innerText();
    if (!blockedText.includes(BLOCKED_REASON)) {
      throw new Error('被阻止的卸载没有把原因显示出来：' + blockedText);
    }
    if (await page.locator('.modal-mask').count() !== masksBefore) {
      throw new Error('被阻止的卸载不该继续弹确认框');
    }
    if (uninstallCalls.includes('uitest-svc-blocked')) {
      throw new Error('被阻止的卸载居然把 DELETE 发出去了（不该动后端）');
    }
    await page.waitForTimeout(400); // 等 toast 淡入动画走完，截图里要看得见它
    await shot('30d-uninstall-blocked-visible');
    await closeAllModals();
    await clearToasts();

    // ⑥ 把提交超时收紧到 1.2s 让断言几秒内完成；钩子防御式调用，避免退化成 TypeError。
    await page.evaluate(async () => {
      const { taskCenter } = await import('./js/tasks.js');
      if (typeof taskCenter.setSubmitTimeoutMs === 'function') taskCenter.setSubmitTimeoutMs(1200);
    });
    await (await openUninstallBtn('UITEST 卸载·被挂住')).click();
    await page.locator('.modal-mask button:has-text("确认卸载")').last().click();
    const slowToast = page.locator('.toasts .toast.err').first();
    await slowToast.waitFor({ timeout: 8000 });
    const slowText = await slowToast.innerText();
    if (!/没有收到面板确认|请求被挂住/.test(slowText)) {
      throw new Error('提交被挂住时没有给出超时提示（界面静默了）：' + slowText);
    }
    await page.waitForTimeout(400); // 等 toast 淡入动画走完，截图里要看得见它
    await shot('30e-uninstall-timeout-visible');
    // 迟到 4 秒的 202 到达后，任务必须被接管（补开进度窗），不能成为"没人管的任务"
    await page.locator('.modal-mask', { hasText: '关闭窗口（后台继续）' }).last().waitFor({ timeout: 12000 });
    if (!uninstallCalls.includes('uitest-svc-slow')) {
      throw new Error('超时场景里 DELETE 没有被发出：' + JSON.stringify(uninstallCalls));
    }
    await shot('30f-uninstall-late-adopted');

    await page.evaluate(async () => {
      const { taskCenter } = await import('./js/tasks.js');
      if (typeof taskCenter.setSubmitTimeoutMs === 'function') taskCenter.setSubmitTimeoutMs();
    });
    await closeAllModals();
    await clearToasts();
    await page.unroute('**/api/v1/services**');
    await page.unroute('**/api/v1/market**');
    await page.unroute('**/api/v1/tasks**');
    expectHTTPError = false;
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(800);
    await page.click('.nav-item:has-text("应用")');
    await page.waitForTimeout(2500);
  });

  // ---------- brew 依赖拦下卸载：必须给「强制卸载 / 取消」两个选择（桩数据）----------
  // 真机卸载 python@3.13 被 brew 以 "required by llvm and rust" 拒绝，面板以前只贴英文原文。锁住：
  // 说明对话框逐字写明后果与 `--ignore-dependencies`、**恰好**两个选择、取消不发请求、强制卸载带 `force=1`。
  await step('brew 依赖拦下卸载：出「强制卸载 / 取消」说明框，取消不发请求、强制带 force=1（桩数据）', async () => {
    const NAME = 'uitest-brew-blocked';
    const LABEL = 'UITEST brew·被依赖拦下';
    const FORCE_NOTE = '强制卸载会破坏这些包：llvm、rust（它们会缺依赖、可能无法运行）。'
      + '命令：brew uninstall --ignore-dependencies python@3.13';
    const deletes = [];

    expectHTTPError = true;

    const closeAllModals = async () => {
      for (let i = 0; i < 5; i++) {
        const masks = page.locator('.modal-mask');
        if (!(await masks.count())) break;
        const x = masks.last().locator('button.modal-close').first();
        if (await x.count()) await x.click().catch(() => {});
        await page.waitForTimeout(250);
      }
    };

    await page.route('**/api/v1/services**', (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      if (req.method() === 'GET' && (path === 'services' || path === 'services/health')) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
          ok: true, data: { list: [{ name: NAME, display_name: LABEL, kind: 'native',
            managed: false, state: { running: false, status: 'stopped' } }] },
        }) });
      }
      if (req.method() === 'GET' && /^services\/[^/]+$/.test(path)) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
          ok: true, data: { name: NAME, display_name: LABEL, kind: 'native', managed: false,
            state: { running: false, status: 'stopped' } },
        }) });
      }
      if (req.method() === 'GET' && /credentials$/.test(path)) {
        return route.fulfill({ status: 200, contentType: 'application/json',
          body: JSON.stringify({ ok: true, data: { credentials: [] } }) });
      }
      return route.continue();
    });
    await page.route('**/api/v1/market**', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: {
        list: [{
          id: NAME, name: LABEL, installed: true,
          uninstall: {
            kind: 'brew', formula: 'python@3.13',
            steps: ['brew uninstall python@3.13', '⚠️ llvm、rust 依赖它'],
            blocked: 'brew 拒绝卸载 python@3.13：llvm、rust 依赖它。可以先卸载它们，'
              + '或选择强制卸载（brew uninstall --ignore-dependencies python@3.13，'
              + '会破坏 llvm、rust：它们会缺依赖、可能无法运行）',
            force_allowed: true,
            force_note: FORCE_NOTE,
            dependents: [
              { kind: 'brew', name: 'llvm', detail: 'Homebrew 包 llvm 依赖 python@3.13' },
              { kind: 'brew', name: 'rust', detail: 'Homebrew 包 rust 依赖 python@3.13' },
            ],
          },
        }],
      } }),
    }));
    await page.route('**/api/v1/market/*', (route) => {
      if (route.request().method() !== 'DELETE') return route.continue();
      deletes.push(route.request().url());
      return route.fulfill({ status: 202, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { task_id: 'uitest-brew-force', title: '卸载 ' + LABEL } }) });
    });
    // 假任务进度流：立刻回"成功"并结束，避免假任务一直重连不存在的流刷 404。
    await page.route('**/api/v1/tasks**', (route) => {
      const m = route.request().url().match(/\/api\/v1\/tasks\/([^/?]+)\/stream/);
      if (m) {
        const id = decodeURIComponent(m[1]);
        const mk = (ev, obj) => `event: ${ev}\ndata: ${JSON.stringify(obj)}\n\n`;
        return route.fulfill({
          status: 200, headers: { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' },
          body: mk('meta', { task: { id, title: '卸载', status: 'running' }, oldest_seq: 1 })
            + mk('status', { id, title: '卸载', status: 'succeeded', line_count: 0 }),
        });
      }
      return route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }) });
    });

    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(500);
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);

    const card = page.locator('#installed-grid > div', { hasText: LABEL }).first();
    await card.waitFor({ timeout: 15000 });
    await card.locator('button:has-text("管理")').click();
    const unBtn = page.locator('.modal-mask button:text-is("卸载")').last();
    await unBtn.waitFor({ timeout: 8000 });

    // ① 点「卸载」→ 说明对话框（不是 toast、不是直接确认框）
    const masksBefore = await page.locator('.modal-mask').count();
    await unBtn.click();
    const dlg = page.locator('.modal-mask', { hasText: '还不能卸载' }).last();
    await dlg.waitFor({ timeout: 8000 });
    const dlgText = await dlg.locator('.modal-body').innerText();
    for (const need of ['llvm', 'rust', '强制卸载', '--ignore-dependencies']) {
      if (!dlgText.includes(need)) {
        throw new Error('说明对话框没有写清「' + need + '」（用户必须看到会破坏什么、执行什么命令）：\n' + dlgText);
      }
    }
    if (await dlg.locator('button:text-is("取消")').count() !== 1
      || await dlg.locator('button:text-is("强制卸载")').count() !== 1) {
      throw new Error('说明对话框必须恰好给「取消」与「强制卸载」两个选择');
    }
    await shot('30g-brew-force-dialog');

    await dlg.locator('button:text-is("取消")').click();
    await page.waitForTimeout(500);
    if (deletes.length !== 0) {
      throw new Error('点了取消却发了卸载请求：' + JSON.stringify(deletes));
    }
    if (await page.locator('.modal-mask', { hasText: '将执行：' }).count()) {
      throw new Error('点了取消反而弹出了卸载确认框（应当什么都不做）');
    }
    if (await page.locator('.modal-mask').count() > masksBefore) {
      throw new Error('取消之后留下了多余的弹窗');
    }

    // ③ 再次点「卸载」→ 强制卸载 → 确认框（勾选已预置）→ 确认 → DELETE 带 force=1
    const unBtn2 = page.locator('.modal-mask button:text-is("卸载")').last();
    await unBtn2.click();
    const dlg2 = page.locator('.modal-mask', { hasText: '还不能卸载' }).last();
    await dlg2.waitFor({ timeout: 8000 });
    await dlg2.locator('button:text-is("强制卸载")').click();
    await page.waitForTimeout(700);
    await shot('30h0-after-force-click');
    {
      const masks = page.locator('.modal-mask');
      const n = await masks.count();
      const texts = [];
      for (let i = 0; i < n; i++) texts.push((await masks.nth(i).innerText()).slice(0, 200));
      console.log('  [debug] force 之后弹窗数=' + n + ' 删除请求=' + JSON.stringify(deletes)
        + '\n' + texts.map((t, i) => '   #' + i + ': ' + t.replace(/\n/g, ' | ')).join('\n'));
    }
    const confirm = page.locator('.modal-mask:has([data-confirm-uninstall])').last();
    await confirm.waitFor({ timeout: 8000 });
    const confirmText = await confirm.locator('.modal-body').innerText();
    if (!confirmText.includes('llvm') || !confirmText.includes('--ignore-dependencies')) {
      throw new Error('强制卸载确认框没有写清会破坏哪些包/执行什么命令：\n' + confirmText);
    }
    const forceBox = confirm.locator('input[type="checkbox"]').first();
    if (!(await forceBox.isChecked())) {
      throw new Error('从「强制卸载」进来时，确认框里的强制开关应当是已勾选的');
    }
    await shot('30h-brew-force-confirm');
    const okBtn = confirm.locator('button.btn-danger').last();
    const okText = (await okBtn.innerText()).trim();
    if (!okText.includes('强制卸载')) {
      throw new Error('强制勾选后确认按钮应显示「强制卸载」，实际「' + okText + '」');
    }
    await okBtn.click();
    await page.waitForTimeout(800);
    if (deletes.length !== 1) {
      throw new Error('确认强制卸载后应恰好发一次 DELETE，实际 ' + deletes.length + ' 次：' + JSON.stringify(deletes));
    }
    if (!/[?&]force=1\b/.test(deletes[0])) {
      throw new Error('强制卸载的请求 URL 必须带 force=1（否则后端会按普通卸载拒绝）：' + deletes[0]);
    }
    await shot('30i-brew-force-task');

    await closeAllModals();
    await page.evaluate(() => {
      document.querySelectorAll('.toasts .toast').forEach((n) => n.remove());
    });
    await page.unroute('**/api/v1/services**');
    await page.unroute('**/api/v1/market**');
    await page.unroute('**/api/v1/market/*');
    await page.unroute('**/api/v1/tasks**');
    expectHTTPError = false;
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(800);
    await page.click('.nav-item:has-text("应用")');
    await page.waitForTimeout(2500);
  });

  // ---------- 没有守护进程 / 装了但无面板记录 → 绝不写「已停止」----------
  // 报障：Colima/Nginx/phpMyAdmin/Python 都被归到"已停止"。真机：ffmpeg 这类是 no_daemon，nginx 正在
  // :80 上跑、只是没记录。走桩：前者写"无常驻进程"、后者写"已安装（面板里暂无记录）"、筛选都不出现。
  await step('状态命名：no_daemon / 装了但无面板记录都不许写「已停止」（桩数据）', async () => {
    const NOD = { id: 'uitest-nodaemon', name: 'UITEST 命令行工具（无常驻进程）', installed: true, adopted: false,
      no_daemon: true, kind: 'native', port: 0,
      uninstall: { kind: 'installer', steps: ['brew uninstall uitest-nodaemon'] } };
    const NOREC = { id: 'uitest-norecord', name: 'UITEST 装了但无面板记录', installed: true, adopted: false,
      no_daemon: false, kind: 'native', port: 80, service_in_launchd: false,
      uninstall: { kind: 'brew', formula: 'uitest-norecord', steps: ['brew uninstall uitest-norecord'] } };

    await page.route('**/api/v1/services**', (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      if (req.method() === 'GET' && (path === 'services' || path === 'services/health')) {
        // 一条**真的** Stopped 的服务记录：它才该出现在「已停止」里（对照组）。
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
          ok: true, data: { list: [{ name: 'uitest-really-stopped', display_name: 'UITEST 真停了',
            kind: 'native', managed: true, state: { running: false, status: 'stopped' } }] },
        }) });
      }
      return route.continue();
    });
    await page.route('**/api/v1/market**', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { list: [NOD, NOREC] } }),
    }));
    await page.route('**/api/v1/market/*', (route) => route.continue());

    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(500);
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);

    const cardOf = (name) => page.locator('#installed-grid > div', { hasText: name }).first();
    const nod = cardOf(NOD.name);
    const norec = cardOf(NOREC.name);
    await nod.waitFor({ timeout: 15000 });
    await norec.waitFor({ timeout: 15000 });

    const nodText = await nod.innerText();
    const norecText = await norec.innerText();
    if (nodText.includes('已停止')) {
      throw new Error('no_daemon 的卡片写了「已停止」（它根本没有常驻进程，这是假信息）：\n' + nodText);
    }
    if (!/无常驻进程/.test(nodText)) {
      throw new Error('no_daemon 的卡片没有如实写"无常驻进程"：\n' + nodText);
    }
    if (norecText.includes('已停止')) {
      throw new Error('"装了但面板里没有记录"的卡片写了「已停止」（面板根本不知道它停没停）：\n' + norecText);
    }
    if (!/已安装（面板里暂无记录）/.test(norecText)) {
      throw new Error('"装了但无面板记录"的卡片没有如实写「已安装（面板里暂无记录）」：\n' + norecText);
    }
    if (!/端口/.test(norecText)) {
      throw new Error('"装了但无面板记录"的卡片没有给出端口监听结论（查不到也要明写"未检测"）：\n' + norecText);
    }
    await shot('30j-status-honest-labels');

    // 点「已停止」筛选：只有**真的有记录且状态是停止**的那张该出现。
    await page.locator('#installed-toolbar button', { hasText: '已停止' }).first().click();
    await page.waitForTimeout(600);
    const filtered = await page.locator('#installed-grid > div').allInnerTexts();
    const joined = filtered.join('\n');
    if (joined.includes(NOD.name)) {
      throw new Error('「已停止」筛选里出现了 no_daemon 的应用（它没有可停止的东西）：\n' + joined);
    }
    if (joined.includes(NOREC.name)) {
      throw new Error('「已停止」筛选里出现了"面板无记录"的应用（面板不知道它停没停）：\n' + joined);
    }
    if (!joined.includes('UITEST 真停了')) {
      throw new Error('「已停止」筛选没有命中**真的** Stopped 的那条服务记录：\n' + joined);
    }
    await shot('30k-stopped-filter-honest');

    await page.locator('#installed-toolbar button', { hasText: '全部' }).first().click();
    await page.waitForTimeout(400);
    await page.unroute('**/api/v1/services**');
    await page.unroute('**/api/v1/market**');
    await page.unroute('**/api/v1/market/*');
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(800);
    await page.click('.nav-item:has-text("应用")');
    await page.waitForTimeout(2500);
  });

  // ---------- compose 记录 + 运行时不可用：必须有"只删记录"的出口（2026-09 真机缺陷）----------
  // 真机：删掉 Colima/Docker 后记录只剩「卸载」，点卸载报"未找到 docker compose 命令" → 记录删不掉。
  // 走桩：桩 500 → 弹带「从*移除」的对话框并说明"未停止容器"；点它真的调 DELETE /services/{name}。
  await step('compose 记录运行时不可用：卸载失败可一键只删记录', async () => {
    const NAME = 'uitest-compose-gone';
    const LABEL = 'UITEST compose·运行时没了';
    const FAKE = {
      name: NAME, display_name: LABEL,
      kind: 'compose', managed: true,
      driver_error: 'Docker 不可用，无法管理 compose 项目',
      state: { status: 'unavailable', running: false },
      compose_file: '/tmp/uitest-compose/docker-compose.yml',
      work_dir: '/tmp/uitest-compose', port: 0,
    };
    const uninstallCalls = [];
    const forgetCalls = [];
    expectHTTPError = true; // 桩的 500 是断言对象，不是前端故障

    await page.route('**/api/v1/services**', async (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      if (req.method() === 'DELETE' && path === `services/${NAME}/uninstall`) {
        uninstallCalls.push(path);
        return route.fulfill({
          status: 500, contentType: 'application/json',
          body: JSON.stringify({ ok: false, msg: '未找到 docker compose 命令。请先安装 Docker…' }),
        });
      }
      if (req.method() === 'DELETE' && path === `services/${NAME}`) {
        forgetCalls.push(path);
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: {} }) });
      }
      if (req.method() === 'GET' && (path === 'services' || path === 'services/health')) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { list: [FAKE] } }) });
      }
      if (req.method() === 'GET' && path === `services/${NAME}`) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: FAKE }) });
      }
      if (req.method() === 'GET' && /credentials$/.test(path)) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { credentials: [] } }) });
      }
      return route.continue();
    });
    await page.route('**/api/v1/market**', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { list: [] } }),
    }));
    await page.route('**/api/v1/tasks**', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
    }));

    const closeModals = async () => {
      for (let i = 0; i < 5; i++) {
        const masks = page.locator('.modal-mask');
        if (!(await masks.count())) break;
        const x = masks.last().locator('button.modal-close').first();
        if (await x.count()) await x.click().catch(() => {});
        await page.waitForTimeout(250);
      }
    };

    try {
      // **必须制造一次 hash 变化**，否则视图不会重新挂载并用刚注册的桩重拉列表。
      await page.click('.nav-item:has-text("仪表盘")');
      await page.waitForTimeout(600);
      await page.click('.nav-item:has-text("应用")');
      await page.waitForTimeout(2000);

      // 打开这条 compose 记录的「⚙️ 管理」面板。
      // 起（用户要求）：目录应用的收尾**只有**「🗑 卸载」，
      // 「只删记录」不再是并排按钮，只在"卸载失败"的兜底对话框里出现。
      const card = page.locator('#installed-grid > div', { hasText: LABEL }).first();
      await card.waitFor({ timeout: 15000 });
      await card.locator('button:has-text("管理")').click();
      const panel = page.locator('.modal-mask').last();
      await panel.locator('button:has-text("🗑 卸载")').waitFor({ timeout: 8000 });
      const panelText = await panel.innerText();
      if (panelText.includes('从列表移除')) {
        throw new Error('面板里仍有旧文案「从列表移除」（目录应用的收尾现在只有「🗑 卸载」）');
      }
      await shot('31a-compose-uninstall-entry');

      // 点「🗑 卸载」→ 必须先出现确认框；确认后桩 500（模拟后端真卸载失败）
      await panel.locator('button:has-text("🗑 卸载")').last().click();
      const confirm = page.locator('.modal-mask', { hasText: '卸载服务' }).last();
      await confirm.locator('button:has-text("确认卸载")').waitFor({ timeout: 8000 });
      await confirm.locator('button:has-text("确认卸载")').click();

      // 按**标题**定位对话框：面板里可能也有同名按钮，hasText 会先匹配到面板本身（实测踩到）。
      const fallback = page.locator('.modal-mask')
        .filter({ has: page.locator('.modal-head h3', { hasText: /^从面板移除 · / }) }).last();
      await fallback.waitFor({ timeout: 10000 });
      const ftext = await fallback.innerText();
      if (!ftext.includes('容器与磁盘数据不会被删') && !ftext.includes('没有容器在跑')) {
        throw new Error('卸载失败对话框没有如实说明"不会动容器/数据"：\n' + ftext.slice(0, 300));
      }
      if (!ftext.includes('从面板移除该服务')) throw new Error('兜底对话框没有「从面板移除该服务」出口');
      if (ftext.includes('从列表移除')) throw new Error('兜底对话框仍有旧文案「从列表移除」');
      await shot('31b-compose-uninstall-failed-fallback');

      // 点「从面板移除该服务」→ 必须真的调用 DELETE /services/{name}
      await fallback.locator('button:has-text("从面板移除该服务")').last().click();
      await page.waitForTimeout(1200);
      if (!forgetCalls.includes(`services/${NAME}`)) {
        throw new Error('点了「从面板移除该服务」但 DELETE /services/{name} 没有发出去：' + JSON.stringify(forgetCalls));
      }
      if (!uninstallCalls.includes(`services/${NAME}/uninstall`)) {
        throw new Error('点「卸载」时 DELETE .../uninstall 没有发出去：' + JSON.stringify(uninstallCalls));
      }
      const okToast = page.locator('.toasts .toast.ok').last();
      await okToast.waitFor({ timeout: 8000 });
      const okText = await okToast.innerText();
      if (!okText.includes('已从面板移除') && !okText.includes('已停止这个服务并从面板移除')) {
        throw new Error('删除记录成功的提示没有如实说清结果：' + okText);
      }
      if (okText.includes('从列表移除')) throw new Error('成功提示仍有旧文案「从列表移除」：' + okText);
      await shot('31c-compose-record-deleted');
    } finally {
      await closeModals();
      await page.unroute('**/api/v1/services**');
      await page.unroute('**/api/v1/market**');
      await page.unroute('**/api/v1/tasks**');
      expectHTTPError = false;
      // 让页面重新拉真实数据，后面步骤不要停在这张桩卡片上
      await page.click('.nav-item:has-text("仪表盘")');
      await page.waitForTimeout(700);
      await page.click('.nav-item:has-text("应用")');
      await page.waitForTimeout(2200);
    }
  });

  // ---------- 本机已有的服务：移除动作必须如实说"不卸载软件"（用户）----------
  // managed=false 的语义是"只删记录"，绝不能假装能卸载：按钮写「从*移除」，点下去弹确认框并
  // **逐字**说明不会卸载软件本身，点「取消」不发任何 DELETE。全部走桩。
  await step('本机已有的服务：移除按钮如实写「不卸载软件」，取消不发请求', async () => {
    const NAME = 'uitest-adopted-svc';
    const LABEL = 'UITEST 本机已有·只移除记录';
    const FAKE = {
      name: NAME, display_name: LABEL, kind: 'native', managed: false,
      state: { running: true, status: 'running', detail: 'pid 999' },
      launch_label: 'com.uitest.adopted', port: 0,
    };
    const deletes = [];
    expectHTTPError = true; // 桩的 404（凭据）是断言对象，不是前端故障

    await page.route('**/api/v1/services**', async (route) => {
      const req = route.request();
      const path = req.url().split('/api/v1/')[1].split('?')[0];
      if (req.method() === 'DELETE' && path === `services/${NAME}`) {
        deletes.push(path);
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: {} }) });
      }
      if (req.method() === 'GET' && (path === 'services' || path === 'services/health')) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { list: [FAKE] } }) });
      }
      if (req.method() === 'GET' && path === `services/${NAME}`) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: FAKE }) });
      }
      if (req.method() === 'GET' && /credentials$/.test(path)) {
        return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { credentials: [] } }) });
      }
      return route.continue();
    });
    await page.route('**/api/v1/market**', (route) => route.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true, data: { list: [] } }),
    }));
    await page.route('**/api/v1/tasks**', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ ok: true, data: { tasks: [], lines: [], has_more: false } }),
    }));

    try {
      // 制造一次 hash 变化，否则视图不会重新挂载、也就不会用刚注册的桩重拉列表
      await page.click('.nav-item:has-text("仪表盘")');
      await page.waitForTimeout(600);
      await page.click('.nav-item:has-text("应用")');
      await page.waitForTimeout(2000);

      const card = page.locator('#installed-grid > div', { hasText: LABEL }).first();
      await card.waitFor({ timeout: 15000 });
      await card.locator('button:has-text("管理")').click();
      // 面板不认识的服务（managed=false）：唯一收尾是「从面板移除该服务」，先停服务再删记录。
      const removeBtn = page.locator('.modal-mask button:has-text("从面板移除该服务")').last();
      await removeBtn.waitFor({ timeout: 8000 });
      const panelText = await page.locator('.modal-mask').last().innerText();
      if (panelText.includes('从列表移除')) {
        throw new Error('面板里仍有旧文案「从列表移除（不卸载软件）」（新文案是「从面板移除该服务」）');
      }
      await shot('31d-remove-from-list-entry');

      await removeBtn.click();
      // 同样按**标题**定位确认框：面板里也有一颗同名按钮，
      // 用 hasText 会先匹配到面板本身（见上面 compose 那一步的注释）。
      const confirm = page.locator('.modal-mask')
        .filter({ has: page.locator('.modal-head h3', { hasText: /^从面板移除该服务$/ }) }).last();
      await confirm.waitFor({ timeout: 8000 });
      const ctext = await confirm.innerText();
      if (!ctext.includes('不会卸载软件本身') && !ctext.includes('软件本身仍在磁盘上')) {
        throw new Error('确认框没有逐字说明"不会卸载软件本身"：\n' + ctext.slice(0, 300));
      }
      if (!ctext.includes('先停止')) {
        throw new Error('确认框没有说明会"先停服务再删记录"（否则会留下隐身运行的服务）：\n' + ctext.slice(0, 300));
      }
      if (ctext.includes('从列表移除')) throw new Error('确认框仍有旧文案「从列表移除」：\n' + ctext.slice(0, 200));
      if (/纳管/.test(ctext)) throw new Error('确认框里仍有内部词"纳管"：\n' + ctext.slice(0, 300));
      await shot('31e-remove-from-list-confirm');

      // 取消 → 一个请求都不该发出去
      await confirm.locator('button:has-text("取消")').last().click();
      await page.waitForTimeout(800);
      if (deletes.length) {
        throw new Error('点了「取消」却发出了 DELETE：' + JSON.stringify(deletes));
      }
    } finally {
      await closeAnyModal(page);
      await page.unroute('**/api/v1/services**');
      await page.unroute('**/api/v1/market**');
      await page.unroute('**/api/v1/tasks**');
      expectHTTPError = false;
      // 让页面重新拉真实数据，后面步骤不要停在这张桩卡片上
      await page.click('.nav-item:has-text("仪表盘")');
      await page.waitForTimeout(700);
      await page.click('.nav-item:has-text("应用")');
      await page.waitForTimeout(2200);
    }
  });

  await step('查看服务日志（SSE 实时流）', async () => {
    // 2026-09-16 起「日志」按钮搬进「应用管理」面板，必须先开面板再点日志，
    // 否则这一步会找不到按钮而**静默跳过**。
    const detailBtn = page.locator('.content button:has-text("⚙️ 管理")').first();
    if (!(await detailBtn.count())) {
      return;
    }
    await detailBtn.click();
    await page.waitForSelector('.modal', { timeout: 8000 });
    await page.waitForTimeout(600);
    const logBtn = page.locator('.modal button:has-text("日志")').first();
    if (!(await logBtn.count())) throw new Error('应用管理面板里没有「日志」按钮');
    // 有些服务没有可跟踪的日志文件，后端对日志流返回 400 属**预期**；要断言界面没撒谎
    // （不能一边被拒一边说"正在重连…"）。
    expectHTTPError = true;
    try {
      await logBtn.click();
      await page.waitForSelector('.modal', { timeout: 8000 });
      await page.waitForTimeout(2500);
      await shot('31-service-logs');
      // 必须按 testid 取日志流自己的状态 pill：取第一个会拿到"运行中"而误判（实测踩到）。
      const pill = await page.locator('[data-testid="zp-logs-status"]').first().innerText();
      // 日志弹窗叠在「应用管理」面板上，页面里同时有 2 个 .modal-body：直接 innerText() 会
      // strict mode 违规 → catch 吞掉 → 变成假失败。取全部弹窗正文拼起来。
      const bodies = await page.locator('.modal-body').allInnerTexts().catch(() => []);
      const body = bodies.join('\n');
      const okStates = ['实时', '中断', '结束', '无法读取日志'];
      if (!okStates.some((s) => pill.includes(s))) {
        throw new Error('日志流状态既不是实时/中断/结束，也不是明确的失败: ' + pill);
      }
      if (pill.includes('无法读取日志') && !/无法建立/.test(body)) {
        throw new Error('日志流建立失败时没有说明原因: ' + body.slice(0, 200));
      }
      if (pill.includes('重连') && body.includes('无法建立')) {
        throw new Error('服务端已拒绝连接，界面却说正在重连（浏览器不会重连）');
      }
      await page.keyboard.press('Escape');
      await page.waitForTimeout(400);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(500);
    } finally {
      expectHTTPError = false;
    }
  });

  await step('服务详情显示完整信息', async () => {
    const detailBtn = page.locator('.content button:has-text("⚙️ 管理")').first();
    if (!(await detailBtn.count())) return;
    await detailBtn.click();
    await page.waitForSelector('.modal .kv', { timeout: 8000 });
    await page.waitForTimeout(700);
    await shot('32-service-detail');
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('运行状态')) throw new Error('详情缺少运行状态');
    // 管理面板就是市场卡片点开的同一个面板（servicePanel.openServicePanel）：
    // 这里顺带断言它的"操作"区有启停按钮，防止"面板退化成只读信息页"。
    if (!/启动|停止/.test(body)) throw new Error('管理面板里没有启停按钮');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await step('打开应用市场', async () => {
    await page.click('.nav-item:has-text("应用")');
    await page.waitForTimeout(800);
    await page.click('[data-tab="market"]');
    await page.waitForTimeout(2000);
    await shot('33-market');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('Ollama')) throw new Error('应用市场未列出预期应用');
  });

  await step('应用市场安装前检查', async () => {
    // 找一个可用的"安装"按钮。`:text-is()` 是精确匹配，不能写成 `:has-text("安装")`：
    // 宽松匹配会点到页面顶部的「⚡ 一键 LNMP」。
    const btn = page.locator('.content button:text-is("安装")').first();
    if (!(await btn.count())) return;
    await btn.click();
    await page.waitForSelector('.modal', { timeout: 10000 });
    await page.waitForTimeout(2000); // 等安装前检查异步返回
    await shot('34-market-preflight');
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('条件') && !body.includes('端口')) {
      throw new Error('安装前检查未显示检查项: ' + body.slice(0, 160));
    }
    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
  });

  await step('「扫描可纳管服务」入口已不存在（纳管是面板自己的事）', async () => {
    // 用户："用户不需要知道什么是纳管。" 断言这个入口不存在、页面上也不再出现内部词；
    // 面板启动时自己登记已知服务，第三方服务用「+ 注册服务」加。用 goto 而非点侧栏，路由改造不会走错页。
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(1500);
    await shot('35-no-adopt-entry');

    if (await page.locator('button:has-text("扫描可纳管服务")').count()) {
      throw new Error('工具栏上仍然有「扫描可纳管服务」按钮（用户要求删掉这个入口）');
    }
    if (await page.locator('button:has-text("注册服务")').count() === 0) {
      throw new Error('「+ 注册服务」不见了 —— 删的是"纳管扫描"，不是手工登记已有服务的能力');
    }
    // 用户可见处一个内部词都不许留（注释里可以解释，界面上不行）。
    // 只查"纳管"与两个整词，不查"托管"：应用摘要里有"自托管"这种正常说法。
    const txt = await page.locator('.content').innerText();
    for (const bad of ['纳管', '面板托管', '接入管理']) {
      if (txt.includes(bad)) {
        throw new Error(`「应用」页面上仍出现内部词「${bad}」：` + txt.slice(0, 240));
      }
    }
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);
  });

  await step('注册服务弹窗表单完整', async () => {
    await page.click('button:has-text("注册服务")');
    await page.waitForSelector('.modal', { timeout: 8000 });
    await page.waitForTimeout(800);
    await shot('36-service-new');
    const inputs = await page.locator('.modal input, .modal select').count();
    if (inputs < 8) throw new Error('注册表单字段过少: ' + inputs);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  // 健康检查两种判定（都是自造服务）：① 401/403 = 活着但要登录 → 必须算健康（用户报过误报）；
  // ② 真连不上 = 失败，且必须给出"检查了什么、为什么、点哪里"。用面板自己的接口当"要登录"的目标，
  // 9 号端口当"连不上"的目标；zapQuiet 的清理调用预期 404，用 expectHTTPError 排除。
  const zapQuiet = async (name) => {
    expectHTTPError = true;
    try { await zapSvc(name); } finally { expectHTTPError = false; }
  };
  const zapSvc = (name) => page.evaluate(async ([b, n]) => {
    const csrf = document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/)?.[1] || '';
    await fetch(b + '/api/v1/services/' + encodeURIComponent(n), {
      method: 'DELETE', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
    });
  }, [base, name]);
  const mkSvc = (payload) => page.evaluate(async ([b, p]) => {
    const csrf = document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/)?.[1] || '';
    const r = await fetch(b + '/api/v1/services', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: JSON.stringify(p),
    });
    const j = await r.json();
    if (r.status !== 200) throw new Error('造服务失败 ' + r.status + ' ' + JSON.stringify(j));
    return true;
  }, [base, payload]);

  await step('要登录的服务算健康（不再误报失败）', async () => {
    const name = 'zp-ui-health-auth';
    await zapQuiet(name);
    await mkSvc({
      name, display_name: 'UI 测试：需要登录的服务', kind: 'native',
      start_cmd: 'sleep 3600', health_url: apiURL('/api/v1/services'),
    });
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(600);
    // 「服务管理」侧栏项在 2026-09-17 已并入「应用」版块；旧的 #/services 会被
    // 别名落到「已安装」Tab。用 goto 而不是点侧栏，是为了任何路由改造都不会
    // 让这一步静默走错页面（原来的选择器等不到元素，报的却是"超时"）。
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);

    // 服务卡片是内联样式的 div、没有类名，直接取会拿到最内层不可见元素（innerText 超时）。
    const txt = await page.evaluate((name) => {
      const hits = Array.from(document.querySelectorAll('div')).filter((el) => {
        const t = el.innerText || '';
        return t.includes(name) && t.includes('健康');
      });
      return hits.length ? hits[hits.length - 1].innerText : null;
    }, 'UI 测试：需要登录的服务');
    if (!txt) throw new Error('「应用 → 已安装」页上没有找到刚造的测试服务卡片');
    if (/健康检查失败/.test(txt)) {
      throw new Error('401 不该再显示"健康检查失败"（这就是用户报的误报）: ' + txt.slice(0, 200));
    }
    if (!/健康/.test(txt)) {
      throw new Error('401 的服务应被判为健康，实际卡片内容：' + txt.slice(0, 200));
    }
    if (!/身份验证/.test(txt)) {
      throw new Error('健康但需要登录时，要说明原因（"需要身份验证，服务本身正常"）: ' + txt.slice(0, 200));
    }
    await shot('37a-health-auth-ok');
    await zapSvc(name);
  });

  await step('健康检查失败有可操作入口', async () => {
    const name = 'zp-ui-health-down';
    await zapQuiet(name);
    await mkSvc({
      name, display_name: 'UI 测试：连不上的服务', kind: 'native',
      start_cmd: 'sleep 3600', health_url: 'http://127.0.0.1:9/',
    });
    // 注意：**再次点击当前所在的导航项不会重新渲染**（hash 没变 → 不触发 hashchange），
    // 所以这里先绕到仪表盘再回来，强制重新拉取服务列表（含健康检查）。
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(600);
    // 「服务管理」侧栏项在 2026-09-17 已并入「应用」版块；旧的 #/services 会被
    // 别名落到「已安装」Tab。用 goto 而不是点侧栏，是为了任何路由改造都不会
    // 让这一步静默走错页面（原来的选择器等不到元素，报的却是"超时"）。
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2000);

    // 健康检查是异步的（每个服务最长 8s），轮询等待而不是固定 sleep。
    // 2026-09-19 工具栏按钮改成「⚠ N 个需要处理」、筛选合并成一项；断言按钮出现且筛得到**这张卡片**、
    // 工具栏只剩 4 项。
    const pill = page.locator('button:has-text("个需要处理")');
    let appeared = false;
    for (let i = 0; i < 20; i++) {
      if (await pill.count()) { appeared = true; break; }
      await page.waitForTimeout(500);
    }
    if (!appeared) {
      throw new Error('造了一个连不上的服务，页头应当出现"N 个需要处理"');
    }
    const filterLabels = await page.locator('#installed-toolbar button').allInnerTexts();
    for (const need of ['全部', '运行中', '已停止', '需要处理']) {
      if (!filterLabels.some((t) => t.includes(need))) {
        throw new Error(`工具栏缺少筛选「${need}」：` + JSON.stringify(filterLabels));
      }
    }
    for (const bad of ['异常', '仅健康检查失败', '健康检查失败', '纳管']) {
      if (filterLabels.some((t) => t.includes(bad))) {
        throw new Error(`工具栏仍有半术语/内部词「${bad}」：` + JSON.stringify(filterLabels));
      }
    }
    await pill.first().click();
    await page.waitForTimeout(1200);
    await shot('37b-health-filtered');

    const body = await page.locator('.content').innerText();
    if (!body.includes('UI 测试：连不上的服务')) {
      throw new Error('「需要处理」筛掉了健康检查失败的服务（合并筛选时藏起了问题）: ' + body.slice(0, 300));
    }
    // 不能只给红标签，必须告诉用户"检查了什么、为什么、下一步点哪"；按**当前真实入口**断言。
    for (const need of ['健康检查失败', '检查地址', '改检查地址', '⟳ 刷新']) {
      if (!body.includes(need)) {
        throw new Error(`筛选后的页面缺少「${need}」，用户看完还是不知道怎么办: ` + body.slice(0, 220));
      }
    }
    if (!/连不上|监听|超时/.test(body)) {
      throw new Error('连不上时应给出人话（别把 curl 原始报错丢给用户）: ' + body.slice(0, 220));
    }
    await page.click('button:has-text("全部")');
    await page.waitForTimeout(800);
    await zapSvc(name);
  });

  // ---------- 任务中心（安装/卸载的实时进度）----------
  // 用户要求：装东西要看得见过程，窗口能关掉也能随时重开。这里绝不真装：只用一个不存在的
  // 服务名去调卸载接口（任务会因"服务不存在"而失败）。
  await step('任务中心：任务可在关窗后重新打开', async () => {
    const taskId = await page.evaluate(async (b) => {
      const csrf = document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/)?.[1] || '';
      const r = await fetch(b + '/api/v1/services/uitest-任务中心-不存在/uninstall', {
        method: 'DELETE',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      });
      const j = await r.json();
      // 长任务必须**立刻**返回 202，而不是傻等（老实现是同步请求）
      if (r.status !== 202) {
        throw new Error('卸载应立刻返回 202（长任务），实际 ' + r.status + ' ' + JSON.stringify(j));
      }
      const id = j && j.data && j.data.task_id;
      if (!id) throw new Error('响应里没有 task_id：' + JSON.stringify(j));
      return id;
    }, base);
    if (!taskId) throw new Error('没拿到 task_id');

    const btn = page.locator('#zp-task-btn');
    if (!(await btn.count())) throw new Error('顶栏缺少任务中心入口 #zp-task-btn');

    await btn.click();
    await page.waitForTimeout(800);
    let txt = await page.locator('.modal').last().innerText();
    if (!/任务中心/.test(txt)) throw new Error('任务中心弹窗没打开：' + txt.slice(0, 120));
    // 列表内容是异步拉回来的，**轮询等待**而不是固定 sleep：
    // 固定 sleep 会让这条断言随机器快慢时通时不通（真发生过）。
    let listed = false;
    for (let i = 0; i < 20; i++) {
      txt = await page.locator('.modal').last().innerText();
      if (/uitest-任务中心-不存在|卸载服务/.test(txt)) { listed = true; break; }
      await page.waitForTimeout(400);
    }
    if (!listed) {
      throw new Error('任务列表里看不到刚提交的任务：' + txt.slice(0, 200));
    }
    await shot('45b-tasks-list');

    await page.locator('.modal').last().locator('text=/卸载服务/').first().click();
    await page.waitForTimeout(1500);
    txt = await page.locator('.modal').last().innerText();
    if (!/关闭窗口（后台继续）/.test(txt)) {
      throw new Error('进度窗缺少「关闭窗口（后台继续）」：' + txt.slice(0, 200));
    }
    if (!/失败|不存在/.test(txt)) {
      throw new Error('进度窗没有显示失败原因：' + txt.slice(0, 200));
    }
    await shot('45c-task-progress');

    await page.locator('.modal').last().locator('button:has-text("关闭窗口（后台继续）")').click();
    await page.waitForTimeout(600);
    await btn.click();
    let back = false;
    for (let i = 0; i < 20; i++) {
      await page.waitForTimeout(400);
      txt = await page.locator('.modal').last().innerText();
      if (/卸载服务/.test(txt)) { back = true; break; }
    }
    if (!back) {
      throw new Error('关窗后重新打开，任务不该消失：' + txt.slice(0, 200));
    }
    await page.locator('.modal').last().locator('button:has-text("关闭")').first().click();
    await page.waitForTimeout(400);
  });

  await step('清理上次测试残留', async () => {
    // 通过页面上下文调用 API：保证可重复运行（上次留下的文件会让"新建"返回 400）。
    const r = await page.evaluate(async (base) => {
      // 这段跑在**浏览器**里：不能用 Node 侧的 apiURL，只能拿传进来的 base 自己拼。
      // base 带尾斜杠，直接拼 '/api/…' 会变双斜杠 → ServeMux 301 清洗。
      const api = base.replace(/\/+$/, '') + '/api/v1';
      const csrf = document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/)?.[1] || '';
      const res = await fetch(api + '/files?path=' + encodeURIComponent('/Users/zizdog/www'), {
        credentials: 'same-origin',
      });
      const j = await res.json();
      const names = (j?.data?.entries || [])
        .map((e) => e.path)
        .filter((p) => /zp-ui-test/.test(p));
      if (names.length) {
        await fetch(api + '/files/delete', {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
          body: JSON.stringify({ paths: names, recursive: true }),
        });
      }
      return names.length;
    }, base);
    if (r) console.log('       （清理了 ' + r + ' 项上次残留）');
  });

  await step('打开文件管理', async () => {
    await page.click('.nav-item:has-text("文件管理")');
    await page.waitForSelector('table.table', { timeout: 15000 });
    await page.waitForTimeout(1200);
    await shot('40-files');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('网站') && !txt.includes('路径')) throw new Error('文件页未渲染路径信息');
    const junk = await page.evaluate(() => {
      let n = 0;
      const walk = (el) => {
        for (const c of el.childNodes) {
          if (c.nodeType === 3 && /^(null|undefined)$/.test(c.textContent.trim())) n++;
          if (c.nodeType === 1) walk(c);
        }
      };
      walk(document.querySelector('.content'));
      return n;
    });
    if (junk > 0) throw new Error('文件管理页出现了 ' + junk + ' 处字面量 null');
  });

  await step('网站管理：状态行不许出现字面量 null，全就绪时不该念 LNMP 说明', async () => {
    // 2026-09-18 报障：状态行出现字面量 "null" 且全都跑着还在念"一键 LNMP"。根因是原生
    // Element.append 把条件渲染的 null 当文本（老坑），修法是一律走 appendAll / if 分支。
    // ⚠️ 必须先回到网站管理页，否则 .card-body 也在、断言是**空过**的。
    await reloadSites();
    const box = page.locator('.card-body').first();
    const text = (await box.innerText()) || '';
    if (/\bnull\b/.test(text)) {
      throw new Error('网站管理页出现字面量 null：' + text.slice(0, 200));
    }
    if (text.includes('已就绪') && text.includes('一键 LNMP」会先确保运行依赖')) {
      throw new Error('环境已就绪时不该再念「一键 LNMP 会先确保运行依赖…」：' + text.slice(0, 200));
    }
    await shot('52c-sites-env-line');
  });

  await step('调整配置 → 上传与执行上限 必须有内容（点开空白 = 用户报障的那一类）', async () => {
    // 2026-09-18 报障：「上传大小 / 执行时间」点开是**空的**（函数定义在另一个作用域里）。
    // 断言"正文里有实质内容"而不是"弹窗出现了"；2026-09-19 起并入「⚙️ 调整配置」。
    await reloadSites();
    const btn = page.getByRole('button', { name: '⚙️ 调整配置', exact: true }).first();
    if (!(await btn.count())) throw new Error('网站管理工具条里没有「⚙️ 调整配置」入口');
    await btn.click();
    await page.waitForSelector('.modal-mask', { timeout: 10000 });
    const tab = page.locator('.modal button.zp-seg-btn').filter({ hasText: '上传与执行上限' }).first();
    if (!(await tab.count())) throw new Error('「调整配置」弹窗里没有「上传与执行上限」页签');
    await tab.click();
    await page.waitForTimeout(2500);
    const text = (await page.locator('.modal-mask .modal-body').allInnerTexts()).join('\n');
    for (const want of ['client_max_body_size', 'upload_max_filesize', '保存并应用']) {
      if (!text.includes(want)) {
        throw new Error('「上传与执行上限」页里缺少 ' + want + ' —— 正文实际是：' + text.slice(0, 200));
      }
    }
    await shot('40c-limits-modal');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
  });

  await step('上传：进度提示 + 落盘 + 上传文件夹入口', async () => {
    // 锁的报障：点上传"没反应、没成功也没提示"。三条缺一不可：① 真的发出请求并落盘；
    // ② 有可见进度；③ 有「上传文件夹」入口。⚠️ 显式导航，不依赖上一步停在文件管理页。
    await page.goto(page.url().split('#')[0] + '#/files', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('input[type="file"]', { state: 'attached', timeout: 20000 });
    await page.waitForTimeout(1200);
    const plain = page.locator('input[type="file"]:not([webkitdirectory])');
    if (!(await plain.count())) throw new Error('文件管理页没有普通上传的 input');
    if (!(await page.locator('input[webkitdirectory]').count())) {
      throw new Error('文件管理页没有 webkitdirectory input（用户报障点名要「上传文件夹」）');
    }
    if (!(await page.getByRole('button', { name: '⬆ 上传文件夹', exact: true }).count())) {
      throw new Error('工具栏缺少「⬆ 上传文件夹」按钮');
    }
    // 上传前**必须先站进临时沙箱**（血泪教训：默认目录曾漂到 /opt/homebrew/etc，测试文件写进真机）。
    // 不能只断言"当前目录在 /tmp 下"——调试实例默认目录是真实 ~/www，那样门禁永远红；
    // 这里主动切到面板临时根（多根时面包屑上有根选择下拉）。
    const roots = await page.evaluate(async () => {
      const r = await fetch('api/v1/files', { credentials: 'same-origin' });
      const j = await r.json();
      return (j.data && j.data.roots) || [];
    });
    const isTmp = (x) => x.startsWith('/tmp/') || x.startsWith('/private/tmp/');
    const sandbox = roots.find((x) => isTmp(x) && /\/work$/.test(x)) || roots.find(isTmp);
    if (!sandbox) {
      throw new Error('这个实例没有任何临时目录根（/tmp 或 /private/tmp），拒绝上传：' + JSON.stringify(roots));
    }
    const rootSel = page.locator('select.select').first();
    const rootOpts = await rootSel.locator('option').evaluateAll((os) => os.map((o) => o.value));
    if (!rootOpts.includes(sandbox)) {
      throw new Error('根选择下拉里没有临时沙箱根 ' + sandbox + '：' + JSON.stringify(rootOpts));
    }
    await rootSel.selectOption(sandbox);
    await page.waitForTimeout(1500);
    // 判据取根下拉当前选中的值（列表正在展示的根），而不是 GET /files 的默认目录（恒为 WWWRoot）。
    const here = await rootSel.inputValue();
    if (!isTmp(here)) {
      throw new Error('切到临时沙箱失败，当前列表的根是（' + here + '）—— 拒绝上传');
    }
    const uploadHits = [];
    const watch = (req) => { if (req.url().includes('/files/upload')) uploadHits.push(req.url()); };
    page.on('request', watch);
    const name = 'zp-uitest-upload.txt';
    const tmp = join(tmpdir(), name);
    try {
      writeFileSync(tmp, 'zizpanel uitest upload\n');
      await plain.setInputFiles(tmp);
      await page.waitForSelector('.modal-mask', { timeout: 10000 });
      const progressText = (await page.locator('.modal-mask .modal-body').textContent()) || '';
      if (!/[已正]上传|正在准备|%/.test(progressText)) {
        throw new Error('上传时没有出现进度提示，弹窗里是：' + progressText);
      }
      // ⚠️ waitForFunction 第 2 个参数是 arg，options 放第 3 个 —— 写错会得到"假超时"。
      await page.waitForFunction(() => {
        const el = document.querySelector('.modal-mask .modal-body');
        return el && /已上传 \d+ 个文件/.test(el.textContent);
      }, null, { timeout: 30000 });
      await shot('40b-files-upload-progress');
      await page.getByRole('button', { name: '关闭', exact: true }).click();
      await page.waitForTimeout(1200);
      const rows = await page.locator('tbody tr').allTextContents();
      if (!rows.some((r) => r.includes(name))) throw new Error('上传完成但列表里看不到 ' + name);
      const abs = await page.evaluate(async (n) => {
        const r = await fetch('api/v1/files', { credentials: 'same-origin' });
        const j = await r.json();
        const e = (j.data.entries || []).find((x) => x.name === n);
        return e ? e.path : '';
      }, name);
      if (abs) {
        await page.evaluate(async (p) => {
          const csrf = decodeURIComponent((document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/) || [])[1] || '');
          await fetch('api/v1/files/delete', {
            method: 'POST', credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
            body: JSON.stringify({ paths: [p], recursive: false }),
          });
        }, abs);
        await page.getByRole('button', { name: '⟳ 刷新', exact: true }).click();
        await page.waitForTimeout(1200);
        const after = await page.locator('tbody tr').allTextContents();
        if (after.some((r) => r.includes(name))) throw new Error('清理失败：' + name + ' 还在列表里');
      }
      if (uploadHits.length !== 1) throw new Error('期望恰好 1 次上传请求，实际 ' + uploadHits.length);
    } finally {
      page.off('request', watch);
      rmSync(tmp, { force: true });
    }
  });

  await step('进入子目录并返回上级', async () => {
    const dirLink = page.locator('tbody tr td a').first();
    if (!(await dirLink.count())) return;
    await dirLink.click();
    await page.waitForTimeout(1500);
    const up = page.locator('button:has-text("上一级")');
    if (await up.count()) {
      await up.click();
      await page.waitForTimeout(1200);
    }
    await shot('41-files-nav');
  });

  await step('新建文件并在线编辑保存', async () => {
    // 上一步的双击可能留下了编辑弹窗：先关干净，否则新按钮被遮罩挡住点不到。
    await closeAnyModal(page);
    // 确保在网站根目录（上一步可能停留在子目录）
    const crumb = page.locator('a:has-text("www")').first();
    if (await crumb.count()) {
      await crumb.click();
      await page.waitForTimeout(1500);
    }
    await page.getByRole('button', { name: '＋ 新建文件', exact: true }).click();
    await page.waitForSelector('.modal input', { timeout: 8000 });
    await page.fill('.modal input', 'zp-ui-test.txt');
    await page.click('.modal button:has-text("确定")');
    await page.waitForTimeout(2500);
    console.log('       [诊断] 新建后文件名链接数:', await page.locator('a', { hasText: 'zp-ui-test.txt' }).count());

    const link = page.locator('a', { hasText: 'zp-ui-test.txt' }).first();
    await link.waitFor({ timeout: 10000 });
    await link.click();
    // 编辑器是内嵌 CodeMirror + .zpf-win；不能用 page.fill(textarea)：CM 的输入层也是隐藏
    // textarea，直接塞值它读不到（表现为"填了没反应"）。
    await page.waitForSelector('.zpf-win .CodeMirror', { timeout: 20000 });
    await page.waitForTimeout(600);
    await shot('42a-files-editor');
    await page.locator('.zpf-win .CodeMirror').click();
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.type('hello from ui test');
    const typed = await page.evaluate(() => {
      const el = document.querySelector('.zpf-win .CodeMirror');
      return el && el.CodeMirror ? el.CodeMirror.getValue() : '';
    });
    if (!typed.includes('hello from ui test')) {
      throw new Error('CodeMirror 里没有出现输入的内容，实际：' + JSON.stringify(typed));
    }
    await page.keyboard.press('ControlOrMeta+s');
    await page.waitForTimeout(2500);
    await shot('42-files-edited');
  });

  await step('删除测试文件', async () => {
    await closeAnyModal(page);
    const row = page.locator('tr', { hasText: 'zp-ui-test.txt' }).first();
    await row.locator('button:has-text("⋯")').click();
    await page.waitForSelector('.modal button:has-text("删除")', { timeout: 8000 });
    await page.click('.modal button:has-text("删除")');
    await page.waitForSelector('button:has-text("删除")', { timeout: 8000 });
    const confirmBtns = page.locator('.modal button:has-text("删除")');
    if (await confirmBtns.count()) {
      await confirmBtns.last().click();
    }
    await page.waitForTimeout(2000);
    await shot('43-files-deleted');
  });

  await step('打开搜索面板', async () => {
    await page.click('button:has-text("搜索")');
    await page.waitForSelector('.modal input', { timeout: 8000 });
    await page.fill('.modal input', 'zizdog');
    await page.click('.modal button:has-text("开始搜索")');
    await page.waitForTimeout(3000);
    await shot('44-files-search');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await step('打开计划任务', async () => {
    await page.click('.nav-item:has-text("计划任务")');
    await page.waitForTimeout(2000);
    await shot('50-cron');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('计划任务') && !txt.includes('新建任务')) {
      throw new Error('计划任务页未渲染: ' + txt.slice(0, 100));
    }
  });

  await step('计划表达式实时预览', async () => {
    await page.click('button:has-text("新建任务")');
    await page.waitForSelector('.modal input', { timeout: 8000 });
    await page.waitForTimeout(500);
    await page.waitForTimeout(1500);
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('每天') && !body.includes('执行')) {
      throw new Error('计划预览未显示描述: ' + body.slice(0, 150));
    }
    const schedInput = page.locator('.modal input').nth(1);
    await schedInput.fill('bad cron');
    await page.waitForTimeout(1500);
    const body2 = await page.locator('.modal-body').innerText();
    if (!body2.includes('✗') && !body2.includes('非法') && !body2.includes('不合法')) {
      throw new Error('非法表达式未提示错误');
    }
    await shot('51-cron-preview');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await privStep('创建并删除计划任务', async () => {
    await page.click('button:has-text("新建任务")');
    await page.waitForSelector('.modal input', { timeout: 8000 });
    await page.locator('.modal input').first().fill('zp-ui-cron');
    const ta = page.locator('.modal textarea');
    if (await ta.count()) await ta.fill('echo ui-test');
    await page.click('.modal button:has-text("创建任务")');
    await page.waitForTimeout(3500);
    await shot('52-cron-created');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('zp-ui-cron')) throw new Error('任务未出现在列表中');

    const row = page.locator('tr', { hasText: 'zp-ui-cron' }).first();
    await row.locator('button:has-text("删除")').click();
    await page.waitForSelector('button:has-text("删除")', { timeout: 8000 });
    const confirms = page.locator('.modal button:has-text("删除")');
    await confirms.last().click();
    await page.waitForTimeout(3000);
    const txt2 = await page.locator('.content').innerText();
    if (txt2.includes('zp-ui-cron')) throw new Error('任务删除后仍在列表中');
    await shot('53-cron-deleted');
  });

  await step('打开数据库管理', async () => {
    await page.click('.nav-item:has-text("数据库")');
    await page.waitForTimeout(3000);
    await shot('70-database');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('MySQL') && !txt.includes('无法连接')) {
      throw new Error('数据库页未渲染: ' + txt.slice(0, 150));
    }
    const junk = await page.evaluate(() => {
      let n = 0;
      const walk = (el) => {
        for (const c of el.childNodes) {
          if (c.nodeType === 3 && /^(null|undefined)$/.test(c.textContent.trim())) n++;
          if (c.nodeType === 1) walk(c);
        }
      };
      walk(document.querySelector('.content'));
      return n;
    });
    if (junk > 0) throw new Error('数据库页出现了 ' + junk + ' 处字面量 null');
  });

  await step('查看某个库的表', async () => {
    const btn = page.locator('button:has-text("查看表")').first();
    if (!(await btn.count())) return;
    await btn.click();
    await page.waitForSelector('.modal', { timeout: 10000 });
    await page.waitForTimeout(2500);
    await shot('71-database-tables');
    const body = await page.locator('.modal-body').innerText();
    if (body.includes('读取失败')) throw new Error('读取表列表失败: ' + body.slice(0, 150));
    await page.keyboard.press('Escape');
    await page.waitForTimeout(600);
  });

  await step('切换到账号与权限', async () => {
    await page.click('button:has-text("账号与权限")');
    await page.waitForTimeout(2000);
    await shot('72-database-users');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('账号') && !txt.includes('@')) {
      throw new Error('账号列表未渲染');
    }
  });

  await step('SQL 执行页可用', async () => {
    await page.click('button:has-text("SQL 执行")');
    await page.waitForTimeout(1500);
    const ta = page.locator('textarea').first();
    if (!(await ta.count())) throw new Error('SQL 输入区未渲染');
    await ta.fill('SELECT 1 AS ok');
    await page.click('button:has-text("▶ 执行")');
    await page.waitForTimeout(2500);
    await shot('73-database-sql');
    const txt = await page.locator('.content').innerText();
    if (txt.includes('执行失败')) throw new Error('SQL 执行失败: ' + txt.slice(-200));
  });

  await step('SQL 安全限制生效（多语句被拒）', async () => {
    // 这一步会故意触发后端 400（多语句被拒），属于预期行为
    expectHTTPError = true;
    const ta = page.locator('textarea').first();
    await ta.fill('SELECT 1; DROP TABLE x');
    await page.click('button:has-text("▶ 执行")');
    await page.waitForTimeout(2500);
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('单条')) throw new Error('多语句未被拒绝: ' + txt.slice(-200));
    await shot('74-database-sql-blocked');
    expectHTTPError = false;
  });

  await step('打开日志页（默认落在「日志」Tab）', async () => {
    await page.click('.nav-item:has-text("日志")');
    await page.waitForTimeout(2500);
    await shot('60-logs');
    if (await page.locator('button[data-tab="logs"].btn-primary').count() !== 1) {
      throw new Error('日志页默认没有落在「日志」Tab');
    }
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('日志文件')) throw new Error('日志页未渲染: ' + txt.slice(0, 120));
    const junk = await page.evaluate(() => {
      let n = 0;
      const walk = (el) => {
        for (const c of el.childNodes) {
          if (c.nodeType === 3 && /^(null|undefined)$/.test(c.textContent.trim())) n++;
          if (c.nodeType === 1) walk(c);
        }
      };
      walk(document.querySelector('.content'));
      return n;
    });
    if (junk > 0) throw new Error('日志页出现了 ' + junk + ' 处字面量 null');
  });

  await step('选择日志并实时尾随', async () => {
    const item = page.locator('.nav-item', { hasText: '访问日志' }).first();
    if (!(await item.count())) return;
    await item.click();
    await page.waitForTimeout(3000);
    await shot('61-log-viewer');
    const box = page.locator('.logbox');
    if (!(await box.count())) throw new Error('日志查看区未渲染');
    const content = await box.innerText();
    if (content.includes('读取失败')) throw new Error('日志读取失败: ' + content.slice(0, 120));
    if (content.length < 5) throw new Error('日志内容为空');
  });

  await step('日志关键字过滤', async () => {
    const input = page.locator('input[placeholder*="关键字过滤"]').first();
    if (!(await input.count())) return;
    await input.fill('GET');
    await page.click('button:has-text("应用过滤")');
    await page.waitForTimeout(2000);
    await shot('62-log-filter');
    const content = await page.locator('.logbox').first().innerText();
    if (content.includes('读取失败')) throw new Error('过滤后读取失败');
  });

  await step('终端默认关闭时给出明确提示', async () => {
    await page.click('.nav-item:has-text("Web 终端")');
    await page.waitForTimeout(2000);
    await shot('45-terminal-disabled');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('未启用') && !txt.includes('已连接') && !txt.includes('会话')) {
      throw new Error('终端页未正确渲染: ' + txt.slice(0, 120));
    }
  });

  // 终端会话回收：离开页面后 PTY 读协程若还阻塞，会话就一直挂在列表里；
  // max_sessions=3，挂满后用户再点终端只会看到"会话上限"。
  await step('终端会话在离开页面后被回收', async () => {
    const termList = () => page.evaluate(async (b) => {
      const r = await fetch(b + '/api/v1/terminal', { credentials: 'same-origin' });
      const j = await r.json();
      return (j && j.data && j.data.list) || [];
    }, base);

    await page.click('.nav-item:has-text("Web 终端")');
    await page.waitForTimeout(3000);
    const pageTxt = await page.locator('.content').innerText();
    if (/未启用/.test(pageTxt)) {
      console.log('        [跳过] 这台机器的 Web 终端没有开启');
      return;
    }
    const opened = await termList();
    if (!opened.length) throw new Error('终端页已连接，但服务端没有会话记录');
    await shot('45d-terminal-session');

    // 离开页面（切路由会触发页面级 cleanup → closeWS → 服务端应回收会话）
    await page.click('.nav-item:has-text("仪表盘")');
    let released = false;
    let now = opened;
    for (let i = 0; i < 20; i++) {
      await page.waitForTimeout(500);
      now = await termList();
      if (now.length === 0) { released = true; break; }
    }
    if (!released) {
      throw new Error(`离开终端页后仍有 ${now.length} 个会话挂着（会占满会话上限，` +
        '最终让终端打不开）：' + JSON.stringify(now.map((x) => x.id)));
    }
  });

  // ---------- 侧栏信息架构 + 「日志」合并页（回归：2026-09 三条调整）----------
  // ① 「系统设置」改名「mac设置」（路由 id 仍是 system）；② 「日志中心」+「操作审计」合并成唯一
  // 「日志」项、页内两个 Tab；③ 它在「系统」分组里。分组在 DOM 里是扁平兄弟节点，只能靠相对位置判断。
  await step('侧栏：mac设置改名 / 唯一「日志」项紧跟面板设置 / 检查更新在设置里', async () => {
    if (await page.locator('.nav-item:has-text("系统设置")').count()) {
      throw new Error('侧栏仍有「系统设置」，应已改名「mac设置」');
    }
    if (await page.locator('.nav-item:has-text("mac设置")').count() !== 1) {
      throw new Error('侧栏「mac设置」入口应恰好 1 个');
    }
    if (await page.locator('.nav-item:has-text("日志")').count() !== 1) {
      throw new Error('侧栏「日志」入口应恰好 1 个');
    }
    if (await page.locator('.nav-item:has-text("日志中心")').count()) {
      throw new Error('侧栏不应再有「日志中心」独立项');
    }
    if (await page.locator('.nav-item:has-text("操作审计")').count()) {
      throw new Error('侧栏不应再有「操作审计」独立项（已并入「日志」页）');
    }

    const readGroup = (title) => page.evaluate((t) => {
      const nav = document.querySelector('nav.nav');
      if (!nav) return null;
      const nodes = Array.from(nav.children);
      const gi = nodes.findIndex((n) => n.classList.contains('nav-group') && n.textContent.trim() === t);
      if (gi < 0) return { found: false, items: [] };
      const items = [];
      for (let i = gi + 1; i < nodes.length; i++) {
        if (nodes[i].classList.contains('nav-group')) break;
        if (nodes[i].classList.contains('nav-item')) items.push(nodes[i].textContent.trim());
      }
      return { found: true, items };
    }, title);

    // ③「系统」分组里只剩 面板设置 → 日志（紧邻）。
    // 用户要求把「检查更新」收回「面板设置」第 3 个 Tab：同时断言侧栏没有它、设置里有它。
    const sys = await readGroup('系统');
    if (!sys || !sys.found) throw new Error('侧栏里找不到「系统」分组标题');
    const si = sys.items.findIndex((t) => t.includes('面板设置'));
    const li = sys.items.findIndex((t) => t.includes('日志'));
    if (si < 0) throw new Error('「系统」分组里没有「面板设置」：' + JSON.stringify(sys.items));
    if (li < 0) throw new Error('「系统」分组里没有「日志」：' + JSON.stringify(sys.items));
    if (li !== si + 1) {
      throw new Error('「日志」应紧跟在「面板设置」之后：' + JSON.stringify(sys.items));
    }
    if (sys.items.some((t) => t.includes('检查更新'))) {
      throw new Error('侧栏不该再有「检查更新」（已收回面板设置，重复入口会让用户困惑）：'
        + JSON.stringify(sys.items));
    }
    await shot('52b-nav-system-group');

    // 面板设置 3 个 Tab 顺序：访问与安全 → 账号与两步验证 → 检查更新。
    await page.click('.nav-item:has-text("面板设置")');
    await page.waitForTimeout(900);
    const tabTitles = (await page.locator('.content button.btn-sm').allInnerTexts()).map((x) => x.trim());
    const wantTabs = ['访问与安全', '账号与两步验证', '检查更新'];
    const gotTabs = tabTitles.filter((t) => wantTabs.includes(t));
    if (gotTabs.join('|') !== wantTabs.join('|')) {
      throw new Error(`设置页 Tab 顺序不对：期望 ${wantTabs.join(' → ')}，实际 ${gotTabs.join(' → ')}`);
    }

    await page.goto(base.replace(/\/+$/, '') + '/#/logs', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('button[data-tab="logs"]', { timeout: 15000 });
    await page.waitForTimeout(800);
    if (await page.locator('button[data-tab="logs"].btn-primary').count() !== 1) {
      throw new Error('#/logs 直开没有落在「日志」Tab');
    }
    if (!(await page.locator('.content').innerText()).includes('日志文件')) {
      throw new Error('#/logs 直开后日志块没有渲染');
    }
    await shot('52c-logs-hash');

    await page.goto(base.replace(/\/+$/, '') + '/#/audit', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.card-head h3:has-text("操作审计")', { timeout: 15000 });
    await page.waitForTimeout(800);
    if (await page.locator('button[data-tab="audit"].btn-primary').count() !== 1) {
      throw new Error('#/audit 直开没有切到「操作审计」Tab');
    }
    // ⑤ 全站只有一个「操作审计」入口：侧栏没有独立项（上面已断言），
    //    唯一的入口是合并页里这个 Tab 按钮。
    if (await page.locator('button[data-tab="audit"]').count() !== 1) {
      throw new Error('「操作审计」入口（Tab 按钮）应恰好 1 个');
    }
    await shot('52d-audit-hash');
  });

  await step('操作审计页：筛选、加载更多、导出入口', async () => {
    // 合并后没有独立的导航项：先进「日志」页，再切到「操作审计」Tab。
    await page.click('.nav-item:has-text("日志")');
    await page.waitForSelector('button[data-tab="audit"]', { timeout: 15000 });
    await page.click('button[data-tab="audit"]');
    await page.waitForSelector('.card-head h3:has-text("操作审计")', { timeout: 15000 });
    await page.waitForTimeout(2000);
    await shot('53-audit');

    const body = await page.locator('.content').innerText();
    if (!body.includes('共 ') && !body.includes('没有符合条件')) {
      throw new Error('审计页没有渲染出结果概要: ' + body.slice(0, 160));
    }
    if (/(^|\s)(null|undefined)(\s|$)/.test(body)) {
      throw new Error('审计页出现字面量 null/undefined: ' + body.slice(0, 200));
    }

    // 关键词筛选：用页面动作下拉里**真实出现过**的动作名。以前写死搜 "login"，在全新实例上必然
    // 失败（第一次进走的是「初始化」，一条 login 都没有），会在正确行为上报错。
    if (body.includes('没有符合条件') === false) {
      const kw = await page.evaluate(() => {
        const sels = Array.from(document.querySelectorAll('.content select.select'));
        const actionSel = sels.find((s) => Array.from(s.options).some((o) => o.textContent.includes('全部动作')));
        const opt = actionSel && Array.from(actionSel.options).find((o) => o.value);
        return opt ? opt.value : '';
      });
      if (!kw) throw new Error('审计页的动作下拉里没有任何真实动作可筛');
      await page.fill('input[placeholder^="关键词"]', kw);
      await page.click('button:has-text("查询")');
      await page.waitForTimeout(1800);
      const filtered = await page.locator('.content').innerText();
      if (!filtered.includes(kw)) {
        throw new Error(`关键词筛选「${kw}」后结果里没有它: ` + filtered.slice(0, 200));
      }
      await shot('54-audit-filtered');

      // 重置应恢复（不残留筛选）
      await page.click('button:has-text("重置")');
      await page.waitForTimeout(1500);
      const reset = await page.locator('.content').innerText();
      if (!reset.includes('共 ')) {
        throw new Error('重置后应仍有结果概要: ' + reset.slice(0, 160));
      }
    }

    for (const label of ['导出 CSV', '导出 JSON']) {
      if (!(await page.locator(`button:has-text("${label}")`).count())) {
        throw new Error('缺少导出按钮: ' + label);
      }
    }
  });

  // ---------- Docker（P3）----------
  // 价值在"页面真的渲染出来了"：逐个点过分区并断言有内容。允许未安装（macOS 常态），
  // 此时必须显示引导卡片而不是报错。
  await step('Docker 页可用（含各分区切换）', async () => {
    await page.click('.nav-item:has-text("Docker")');
    await page.waitForSelector('.card-head h3:has-text("Docker")', { timeout: 15000 });
    await page.waitForTimeout(2500); // 等首屏探测 Docker 环境
    await shot('46-docker-containers');

    const body = await page.locator('.content').innerText();
    // 回归：原生 append 把 null 渲染成字面量。必须匹配"独立成词"的 null/undefined，不能用 \b。
    if (/(^|\s)(null|undefined)(\s|$)/.test(body)) {
      throw new Error('Docker 页出现了字面量 null/undefined: ' + body.slice(0, 200));
    }

    const unavailable = body.includes('Docker 环境不可用');
    if (unavailable) {
      // 用户 2026-09-19：没装运行时**直接给一键安装**；不再有「去服务管理」入口；
      // 文案要如实说清代价（需要 Linux 虚拟机、下载 1–3 GB、首次启动约 40 秒）。
      if (!body.includes('一键安装 Docker（Colima）')) {
        throw new Error('Docker 不可用时应直接给「一键安装 Docker（Colima）」，实际: ' + body.slice(0, 240));
      }
      for (const bad of ['去应用市场', '去服务管理', '已纳管服务', '纳管']) {
        if (body.includes(bad)) {
          throw new Error(`Docker 页不该再出现「${bad}」入口（用户要求弱化纳管、去掉无效跳转）`);
        }
      }
      if (!/虚拟机/.test(body) || !/1[–-]3\s*GB/.test(body)) {
        throw new Error('一键安装必须如实写清代价（Linux 虚拟机 + 约 1–3 GB 下载）: ' + body.slice(0, 240));
      }
      return;
    }

    // 环境可用：剩下的分区都要能切过去且不报错。
    // 循环变量不能叫 shot（会遮蔽截图函数 shot()，报错很误导）。
    // 2026-09-19 删掉了 '已纳管服务' 分区；本机没有 Docker，这条在本机**未被执行到**。
    for (const [tab, shotName] of [
      ['镜像', '47-docker-images'],
      ['数据卷', '48-docker-volumes'],
      ['网络', '49-docker-networks'],
      ['Compose', '50-docker-compose'],
    ]) {
      await page.click(`button:has-text("${tab}")`);
      await page.waitForTimeout(1200);
      await shot(shotName);
      const t = await page.locator('.content').innerText();
      if (!t.includes('Docker')) {
        throw new Error(`切到「${tab}」分区后页面异常: ` + t.slice(0, 150));
      }
      if (/(^|\s)(null|undefined)(\s|$)/.test(t)) {
        throw new Error(`「${tab}」分区出现字面量 null/undefined: ` + t.slice(0, 200));
      }
    }

    await page.click('button:has-text("容器")');
    await page.waitForTimeout(1200);
    await shot('52-docker-containers-back');
  });

  // ---------- 容器运行时的"已安装"必须看现实（用户 2026-09-19 报的假"已安装"）----------
  // 现场：只剩一份旧版 /Library/LaunchDaemons/com.zizdog.colima.plist，市场却回 installed=true。
  // 断言以**本机真实探测**为准：读 /docker/info 的 runtime.binary_installed，再要求市场条目与它一致。
  await step('容器运行时的「已安装」跟真实探测一致（僵尸 plist 不算已安装）', async () => {
    const probe = await page.evaluate(async (b) => {
      const api = b.replace(/\/+$/, '') + '/api/v1';
      const info = await (await fetch(api + '/docker/info', { credentials: 'same-origin' })).json();
      const market = await (await fetch(api + '/market', { credentials: 'same-origin' })).json();
      const item = (market?.data?.list || []).find((x) => x.id === 'docker-runtime');
      return {
        binary: info?.data?.runtime?.binary_installed,
        state: info?.data?.runtime?.state,
        installed: item ? item.installed : null,
        artifacts: item ? item.artifacts : null,
        note: item ? item.note : '',
        plistExists: item?.docker_runtime?.plist_exists,
      };
    }, base);
    if (probe.installed === null) throw new Error('应用市场里找不到 docker-runtime 条目');
    if (probe.binary !== probe.installed) {
      throw new Error('docker-runtime 的 installed 与真实探测不一致（假"已安装"回归）：'
        + JSON.stringify(probe));
    }
    if (probe.binary === false) {
      if (probe.state !== 'not-installed') {
        throw new Error('没有二进制时 state 应为 not-installed，实际 ' + probe.state);
      }
      if (probe.plistExists === true && probe.artifacts !== true) {
        throw new Error('只剩僵尸 plist 时应如实报 artifacts=true（用户要能清理/重装），实际 '
          + JSON.stringify(probe));
      }
      await page.goto(page.url().split('#')[0] + '#/apps/market', { waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(2500);
      const card = page.locator('.grid.grid-3 > div', { hasText: 'Docker 运行时' }).first();
      const cardText = (await card.count()) ? await card.innerText() : '';
      if (/已安装/.test(cardText)) {
        throw new Error('应用市场把 docker-runtime 显示成「已安装」，而真实探测是未安装（僵尸 plist）：'
          + cardText.slice(0, 200));
      }
    }
  });

  await step('回到仪表盘', async () => {
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(1500);
  });

  await step('重新打开面板后会话保持', async () => {
    // 用"先离开再回来"而不是 page.reload()：面板有 SSE 长连接时 reload 会一直等导航事件（超时）。
    await page.goto('about:blank', { waitUntil: 'commit', timeout: 10000 });
    await page.goto(base + '/', { waitUntil: 'commit', timeout: 15000 });
    await page.waitForSelector('.layout', { timeout: 15000 });
    await shot('09-reload-session-kept');
    const stillIn = await page.locator('.nav-item:has-text("仪表盘")').count();
    if (!stillIn) throw new Error('重新打开后未保持登录状态');
  });

  await step('退出登录', async () => {
    expectAuthFailure = true;
    await page.click('button[title="退出登录"]');
    await page.waitForSelector('.auth-card', { timeout: 8000 });
    await shot('10-logged-out');
  });

  await step('错误密码被拒绝', async () => {
    await page.fill('input[autocomplete="username"]', 'admin');
    await page.fill('input[autocomplete="current-password"]', 'definitely-wrong');
    await page.click('button:has-text("登 录")');
    await page.waitForSelector('.toast.err', { timeout: 8000 });
    await shot('11-login-rejected');
  });

  await step('正确密码登录成功', async () => {
    await page.fill('input[autocomplete="current-password"]', PASSWORD);
    await page.click('button:has-text("登 录")');
    await page.waitForSelector('.layout', { timeout: 10000 });
    expectAuthFailure = false; // 恢复正常登出态不再产生预期 401
  });

  await step('移动端视图（窄屏抽屉导航）', async () => {
    await page.setViewportSize({ width: 430, height: 900 });
    await page.waitForTimeout(800);
    await shot('12-mobile');
    await page.click('.hamburger');
    await page.waitForTimeout(600);
    await shot('13-mobile-nav');
    await page.setViewportSize({ width: 1440, height: 900 });
  });

  if (errors.length) {
    console.log('\n⚠️  浏览器控制台错误:');
    errors.forEach((e) => console.log('   - ' + e));
    throw new Error(`前端有 ${errors.length} 条控制台错误`);
  }

  console.log('\n✅ UI 端到端验证通过');
  if (skipped.length) {
    // 必须显式说出来：跳过不是通过。
    // 要看完整的特权链路（建站 / nginx 校验），对真实安装实例跑 make uitest-live。
    console.log(`\n⚠️  跳过 ${skipped.length} 步（ZP_SKIP_PRIV=1，本地非 root 实例无提权能力）:`);
    skipped.forEach((s) => console.log('   - ' + s));
    console.log('   完整覆盖请执行：make uitest-live');
  }
  console.log('截图:');
  shots.forEach((s) => console.log('   ' + s));
  await browser.close();
  process.exit(0);
} catch (e) {
  await shot('99-failure').catch(() => {});
  console.error('\n❌ UI 验证失败:', e.message);
  if (errors.length) {
    console.error('浏览器控制台错误:');
    errors.forEach((x) => console.error('   - ' + x));
  }
  await browser.close();
  process.exit(1);
}
