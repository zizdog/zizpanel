// tools/uitest.mjs —— 端到端 UI 验证脚本。
//
// 用途：每次改动前端后自动跑一遍真实浏览器流程，并留下截图。
// 这比"接口返回 200"有意义得多：接口通了不代表页面渲染正常、
// 不代表按钮能点、不代表会话能保持。
//
// 用法：
//   node tools/uitest.mjs http://127.0.0.1:18443 /tmp/zp-shots
//
// 退出码非 0 表示流程失败，可直接接入 CI。

import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const base = process.argv[2] || 'http://127.0.0.1:18443';
// 测试站点域名：用 .test 顶级域（RFC 6761 保留），不会与真实域名冲突
const TEST_SITE = process.env.ZP_TEST_SITE || 'zptest-demo.test';
// 管理员口令**不写进仓库**。
//   make smoke（临时实例）会用它现场创建管理员，任意值都行 —— 所以有默认的假口令；
//   make uitest-live 要登录**真实**面板，必须由 Makefile 通过 ZP_PASS 传入真实口令
//   （真实口令存在被 gitignore 的 .panel-credential.local 里）。
// 之前这里硬编码着真实口令，等于把生产口令提交进版本库，已改掉。
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

// 需要 root 提权的步骤（建站/nginx 校验）在**本地非 root 实例**上必然失败：
// 面板调用的是受限提权助手，而助手硬性要求以 root 运行。
// 所以本地 make smoke 用 ZP_SKIP_PRIV=1 显式跳过这几步，并如实打印"跳过"，
// 而不是把失败当成通过。完整的特权链路请对真实安装实例跑：
//     make uitest-live
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
// expectAuthFailure 为 true 时，401/403 属于流程预期（如探活未登录、密码错误），
// 不算故障。真正的故障是 JS 运行时异常、资源 404/500、以及非预期的鉴权失败。
//
// 注意：这里监听的是 response 而不是 console —— 浏览器对 401 只会打印
// "Failed to load resource"，文本里既没有状态码也没有 URL，无法据此判断。
let expectAuthFailure = false;
// expectHTTPError：某些步骤会故意触发 4xx（例如验证"多语句被拒绝"），
// 这类错误是测试的断言对象，不应计为故障。
let expectHTTPError = false;

// pageerror 一定要带堆栈：只记 message 的话，像 "statusBar is not defined"
// 这种错误不告诉你是哪个文件哪一行，排查只能靠猜（真踩过）。
page.on('pageerror', (e) => {
  const at = (e.stack || '').split('\n').filter((l) => l.includes('assets/js/'))[0];
  errors.push('pageerror: ' + e.message + (at ? ' @ ' + at.trim() : ''));
});
page.on('requestfailed', (r) => {
  const url = r.url();
  const why = r.failure()?.errorText || '';
  // SSE 长连接被前端主动关闭时（关弹窗、切页面）会报 ERR_ABORTED，
  // 这是预期行为，不是故障。
  if (why.includes('ERR_ABORTED') && (url.includes('/logs/stream') || url.includes('/system/stream'))) {
    return;
  }
  errors.push(`requestfailed: ${url} ${why}`);
});
page.on('response', (res) => {
  const code = res.status();
  if (code < 400) return;
  // 未登录时的 /session 探活必然 401，属于设计内的行为
  if (code === 401 && /\/api\/v1\/(session|login)/.test(res.url())) return;
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
    // 回归测试：h() 的第二个参数是 DOM Node 时必须被当作子节点。
    // 曾经这里被误判为 props 对象，导致 h('div', someElement) 的内容被静默丢弃
    // （弹窗内容整体消失，且不报任何错，排查成本极高）。
    const r = await page.evaluate(async () => {
      const { h } = await import('./js/ui.js');
      const inner = h('div', [h('span', { text: 'x' })]);
      // 数组里的 null/false/undefined 必须被跳过，不能渲染成文本
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
    // CPU 卡片必须出现真实百分比，而不是初始的 "—"
    await page.waitForFunction(() => {
      const el = document.querySelector('.metric .value');
      return el && /%/.test(el.textContent);
    }, { timeout: 15000 });
    // 已进入面板，之后的 401 都算异常
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
    // 主机名必须真实存在
    const host = await page.evaluate(() => document.body.innerText);
    if (!host.includes('macOS')) throw new Error('未显示操作系统信息');
  });

  await step('深色主题截图', async () => {
    await page.click('button[title="切换主题"]');
    await page.waitForTimeout(600);
    await shot('03-dashboard-dark');
    await page.click('button[title="切换主题"]');
  });

  await step('切换到系统设置页', async () => {
    await page.click('.nav-item:has-text("系统设置")');
    await page.waitForTimeout(2600);
    await shot('04-system-settings');
    const txt = await page.locator('.content').innerText();
    // 这一页取代了原来的「系统监控」（与仪表盘重复）。内容必须真的渲染出来：
    // 服务器模式、电源策略、更新阻断 —— 少一个都说明接口或页面挂了。
    for (const need of ['一键设为服务器模式', '电源与睡眠', '系统更新阻断', '远程访问与登录']) {
      if (!txt.includes(need)) throw new Error(`系统设置页缺少「${need}」`);
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
  });

  await step('切换设置内的三个 Tab', async () => {
    for (const t of ['账号与两步验证', '关于与运维', '访问与安全']) {
      await page.click(`button:has-text("${t}")`);
      await page.waitForTimeout(700);
      await shot('06-settings-' + t);
    }
  });

  // ---------- 在线升级 ----------
  await step('在线升级卡片显示真实状态', async () => {
    await page.click('button:has-text("关于与运维")');
    await page.waitForTimeout(1200);
    const txt = await page.locator('.content').innerText();
    for (const need of ['在线升级', '当前版本', '内嵌发布公钥', '检查更新']) {
      if (!txt.includes(need)) throw new Error(`在线升级卡片缺少「${need}」`);
    }
    // 当前版本必须和 session 里报告的一致，避免卡片显示了一个假的版本
    const ver = await page.locator('#zp-up-ver').innerText();
    if (!/^v\d+\.\d+\.\d+/.test(ver)) throw new Error('版本号显示异常: ' + ver);
    await shot('07-upgrade-card');
  });

  await step('升级源留空时自动走候选源（不再报“尚未配置升级源地址”）', async () => {
    // 2026-09-17 起语义变了：升级源为空时**不再 400**，而是按候选链逐个试
    // （用户显式源 → 同网段 NAS → https://zizdog.com/zizpanel → mirror → GitHub），
    // 每个候选内部都要通过验签才算命中。所以这里断言的是"给出了结论"，
    // 而不是某个具体状态码 —— 有更新/已是最新/全部候选不可达都是正常结果。
    await page.fill('input[placeholder^="https://example.com"]', '');
    expectHTTPError = true;
    try {
      await page.click('button:has-text("检查更新")');
      await page.waitForTimeout(2000);
      const body = await page.locator('.content').innerText();
      if (body.includes('尚未配置升级源地址')) {
        throw new Error('仍在报「尚未配置升级源地址」——候选源没有生效');
      }
      if (!/候选|升级源|已是最新|新版本|失败/.test(body)) {
        throw new Error('检查更新没有给出任何结论：' + body.slice(0, 200));
      }
    } finally {
      expectHTTPError = false;
    }
    await shot('07-upgrade-no-source');
    // 把 Tab 还原：后续步骤假设停留在「访问与安全」页，
    // 测试步骤之间不能互相踩状态（这一条我自己刚踩过）。
    await page.click('button:has-text("访问与安全")');
    await page.waitForTimeout(600);
  });

  await step('用户名表单可用（只验证校验，不改真实账号）', async () => {
    // 刻意**不提交合法的新用户名**：那会真的改名，而本测试后面还要用当前账号
    // 登录（下一轮运行也一样）。成功路径由 Go 测试覆盖，那里有硬证据：
    // 改名后旧用户名登录失败、新用户名登录成功、会话不受影响。
    //
    // 用户名卡片在「账号与两步验证」Tab 里，而上一步骤停在「关于与运维」，
    // 所以要先切回来（曾漏这一步，测试报"缺少入口"，其实是找错了 Tab）。
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

    // (a)(b) 两次点击都**故意**打回 400（用户名不合法 / 当前密码错），
    // 这正是要断言的行为，所以用 expectHTTPError 开关把它们从
    // "浏览器控制台错误"里排除 —— 否则整轮测试会因为这两条预期内的 400 判失败。
    expectHTTPError = true;
    try {
      // (a) 非法用户名 → 明确提示
      await nameInput.fill('a');
      await pwdInput.fill('whatever-wrong');
      await btn.click();
      await page.waitForTimeout(1200);
      let t = await page.locator('.toast').last().innerText().catch(() => '');
      if (!/不合法|相同|密码/.test(t)) {
        throw new Error('非法用户名没有给出明确提示，实际: ' + t);
      }

      // (b) 当前密码错 → 凭据错误（会打到接口，但不会改动账号）
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
    // 切到白名单再切回来，验证接口与提示都正常
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
    // 这一段以前只点了「数据库」一项，注释却写着"遍历侧边栏" —— 名不副实。
    // 现在真的逐项点开：占位页全部替换完了，这条断言才真正有保障。
    const items = await page.locator('.nav-item').allInnerTexts();
    if (items.length < 8) throw new Error('侧边栏导航项过少: ' + items.length);

    const navItems = page.locator('.nav-item');
    const n = await navItems.count();
    const placeholders = [];
    for (let i = 0; i < n; i++) {
      const label = (await navItems.nth(i).innerText()).trim();
      if (!label) continue;
      await navItems.nth(i).click();
      await page.waitForTimeout(1100);
      const txt = await page.locator('.content').innerText();
      if (txt.includes('正在开发中')) placeholders.push(label);
    }
    if (placeholders.length) {
      throw new Error('仍有占位页: ' + placeholders.join(' / '));
    }
    await shot('08-nav-all-real');
  });

  // ---------- 网站管理（P2）----------
  await step('打开网站管理', async () => {
    await page.click('.nav-item:has-text("网站管理")');
    await page.waitForSelector('button:has-text("新建站点")', { timeout: 10000 });
    await page.waitForTimeout(800);
    await shot('20-sites');
  });

  await privStep('新建一个 PHP 站点', async () => {
    await page.click('button:has-text("新建站点")');
    const domainInput = page.locator('input[placeholder="例如：demo.test"]');
    await domainInput.waitFor({ timeout: 8000 });
    await domainInput.fill(TEST_SITE);
    await page.locator('input[placeholder="可选，便于自己识别"]').fill('UI 自动化测试站点');
    // 伪静态选通用，PHP 用下拉里的默认值
    await page.selectOption('select.select >> nth=0', 'generic');
    await shot('21-site-new');
    await page.click('button:has-text("创建站点")');
    // 等创建成功提示或详情弹窗
    await page.waitForTimeout(4000);
    await shot('22-site-created');
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

  await privStep('校验 nginx', async () => {
    await page.click('button:has-text("校验 nginx")');
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

  // ---------- 「我的应用」（P3；2026-09-17 起「服务管理」与「应用市场」合并成
  // 一个「应用」版块，两个 Tab；老 hash #/services 会自动落到「我的应用」）----------
  await step('打开我的应用', async () => {
    await page.goto(page.url().split('#')[0] + '#/services');
    await page.waitForTimeout(2500);
    await shot('30-services');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('/')) throw new Error('服务页未渲染统计信息');
  });

  await step('服务卡片显示真实状态', async () => {
    const txt = await page.locator('.content').innerText();
    // 已纳管的 qwen3tts 应显示为运行中
    if (!txt.includes('运行中')) throw new Error('未显示任何运行中的服务');
  });

  await step('查看服务日志（SSE 实时流）', async () => {
    // 2026-09-16：「日志」按钮从服务卡片搬进了「应用管理」面板（市场卡片与服务管理
    // 点开的是同一个面板；市场上的两个入口「详情」「查看服务」已合并成「⚙️ 管理」）。
    // 所以先开面板再点日志 —— 这里必须跟着走，
    // 否则这一步会因为找不到按钮而**静默跳过**，SSE 那条防线就名存实亡了。
    const detailBtn = page.locator('.content button:has-text("⚙️ 管理")').first();
    if (!(await detailBtn.count())) {
      // 没有服务卡片时跳过
      return;
    }
    await detailBtn.click();
    await page.waitForSelector('.modal', { timeout: 8000 });
    await page.waitForTimeout(600);
    const logBtn = page.locator('.modal button:has-text("日志")').first();
    if (!(await logBtn.count())) throw new Error('应用管理面板里没有「日志」按钮');
    // 有些服务本来就没有可跟踪的日志文件（例如 brew 的 mysql8.4 把日志写在别处），
    // 此时后端对日志流返回 400，浏览器会如实记一条控制台错误 —— 这是**预期内**的。
    // 要断言的是界面没有因此撒谎：不能一边被服务端拒绝、一边说"正在重连…"
    // （EventSource 对非 200 响应不会重连）。
    expectHTTPError = true;
    try {
      await logBtn.click();
      await page.waitForSelector('.modal', { timeout: 8000 });
      await page.waitForTimeout(2500);
      await shot('31-service-logs');
      const pill = await page.locator('.modal .pill').first().innerText();
      const body = await page.locator('.modal-body').innerText().catch(() => '');
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
      // 先关日志弹窗，再关它下面那层「应用管理」面板（Esc 一次只关最上面一层）。
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
    // 导航里已经没有「应用市场」这个版块了：点「应用」再切到市场 Tab
    await page.click('.nav-item:has-text("应用")');
    await page.waitForTimeout(800);
    await page.click('[data-tab="market"]');
    await page.waitForTimeout(2000);
    await shot('33-market');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('Ollama')) throw new Error('应用市场未列出预期应用');
  });

  await step('应用市场安装前检查', async () => {
    // 找一个可用的"安装"按钮（不是已安装、不是不可用）。
    //
    // `:text-is()` 是**精确**匹配，不能写成 `:has-text("安装")`：
    // 市场页顶部还有一颗「⚡ 一键 LNMP」，但这里的 `安装` 精确匹配本就排除它；
    // 而且是页面里第一个匹配项 —— 用宽松匹配会点开 LNMP 向导，
    // 于是"未显示检查项"失败，看起来像市场坏了，其实只是点错了按钮。
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

  await step('扫描可纳管服务', async () => {
    await page.click('.nav-item:has-text("服务管理")');
    await page.waitForTimeout(1500);
    await page.click('button:has-text("扫描可纳管服务")');
    await page.waitForSelector('.modal', { timeout: 10000 });
    await page.waitForTimeout(2000);
    await shot('35-adoptable');
    const body = await page.locator('.modal-body').innerText();
    // 面板自身与 nginx 必须被排除
    if (body.includes('cn.zizpanel.panel')) throw new Error('扫描结果不应包含面板自身');
    if (body.includes('cn.zizdog.nginx')) throw new Error('扫描结果不应包含 nginx');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
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

  // 健康检查的两种判定（都是**自造服务**，不依赖这台机器上装了什么）：
  //   1) 401/403 = 服务活着但要登录 → 必须算健康（用户报过这个误报：
  //      Stirling PDF 设完自己的账号密码后一直被标成"健康检查失败"）
  //   2) 真连不上 = 失败，且必须给出"检查了什么、为什么、点哪里"
  //
  // 用面板自己的接口当"要登录"的目标：健康检查不带会话 cookie，
  // 必然拿到 401；用 9 号端口当"连不上"的目标（保留端口，不会有人监听）。
  // zapQuiet：清理"上一次运行可能留下的"测试服务。
  // 这条调用**预期会失败**（服务本来就不存在，接口返回 404），
  // 用 expectHTTPError 开关把它排除在"浏览器控制台错误"之外 ——
  // 否则每轮测试都会因为一条早就不存在的记录而失败。
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
    // 面板自己的 API 在不带 cookie 时返回 401 —— 正是 Stirling PDF 那种情形
    await mkSvc({
      name, display_name: 'UI 测试：需要登录的服务', kind: 'native',
      start_cmd: 'sleep 3600', health_url: base + '/api/v1/services',
    });
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(600);
    await page.click('.nav-item:has-text("服务管理")');
    await page.waitForTimeout(2500);

    // 服务卡片是**内联样式的 div**、没有类名，用 `.card div` 之类的选择器会取到
    // 最内层那个不可见的元素（innerText 直接超时）。这里在浏览器里找
    // "同时包含服务名与健康信息的最小 div" —— 精确且不依赖类名。
    const txt = await page.evaluate((name) => {
      const hits = Array.from(document.querySelectorAll('div')).filter((el) => {
        const t = el.innerText || '';
        return t.includes(name) && t.includes('健康');
      });
      return hits.length ? hits[hits.length - 1].innerText : null;
    }, 'UI 测试：需要登录的服务');
    if (!txt) throw new Error('服务管理页上没有找到刚造的测试服务卡片');
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
    // 9 号端口不会有人监听：稳定复现"连不上"
    await mkSvc({
      name, display_name: 'UI 测试：连不上的服务', kind: 'native',
      start_cmd: 'sleep 3600', health_url: 'http://127.0.0.1:9/',
    });
    // 注意：**再次点击当前所在的导航项不会重新渲染**（hash 没变 → 不触发 hashchange），
    // 所以这里先绕到仪表盘再回来，强制重新拉取服务列表（含健康检查）。
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(600);
    await page.click('.nav-item:has-text("服务管理")');
    await page.waitForTimeout(2000);

    // 健康检查是异步的（每个服务最长 8s），轮询等待而不是固定 sleep
    const pill = page.locator('button:has-text("个健康检查失败")');
    let appeared = false;
    for (let i = 0; i < 20; i++) {
      if (await pill.count()) { appeared = true; break; }
      await page.waitForTimeout(500);
    }
    if (!appeared) {
      throw new Error('造了一个连不上的服务，页头应当出现"N 个健康检查失败"');
    }
    await pill.first().click();
    await page.waitForTimeout(1200);
    await shot('37b-health-filtered');

    const body = await page.locator('.content').innerText();
    // 关键：不能只给一个红标签，必须告诉用户"检查了什么、大概为什么、下一步点哪"
    for (const need of ['健康检查失败', '检查地址', '重新检查']) {
      if (!body.includes(need)) {
        throw new Error(`筛选后的页面缺少「${need}」，用户看完还是不知道怎么办: ` + body.slice(0, 220));
      }
    }
    if (!/连不上|监听|超时/.test(body)) {
      throw new Error('连不上时应给出人话（别把 curl 原始报错丢给用户）: ' + body.slice(0, 220));
    }
    await page.click('button:has-text("全部")');
    await page.waitForTimeout(800);
    // 收拾干净：测试造的服务不能留在用户面板里
    await zapSvc(name);
  });

  // ---------- 文件管理（P4）----------
  // ---------- 任务中心（安装/卸载的实时进度）----------
  //
  // 用户的明确要求：装东西要看得见过程，窗口能关掉、也能随时重新打开。
  //
  // 这里**不会真的安装或卸载任何东西**：只用一个不存在的服务名去调卸载接口
  // （任务会因为"服务不存在"而失败）。真实安装会动用户机器上的
  // brew / launchd / docker，UI 测试绝不能碰。
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

    // 顶栏入口必须**任何页面都在**（这是"随时能重开"的前提）
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

    // 打开进度窗：应当看到日志与"关闭窗口（后台继续）"
    await page.locator('.modal').last().locator('text=/卸载服务/').first().click();
    await page.waitForTimeout(1500);
    txt = await page.locator('.modal').last().innerText();
    if (!/关闭窗口（后台继续）/.test(txt)) {
      throw new Error('进度窗缺少「关闭窗口（后台继续）」：' + txt.slice(0, 200));
    }
    // 任务会因为服务不存在而失败 —— 失败原因必须**看得见**，不能只显示一个红点
    if (!/失败|不存在/.test(txt)) {
      throw new Error('进度窗没有显示失败原因：' + txt.slice(0, 200));
    }
    await shot('45c-task-progress');

    // 关掉窗口 ≠ 取消任务：关窗后还能从顶栏重新打开，任务记录仍在
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
    // 通过页面上下文调用 API：保证测试可重复运行
    // （上一次失败时留下的文件会让这次的"新建"返回 400）
    const r = await page.evaluate(async (base) => {
      const csrf = document.cookie.match(/(?:^|; )zp_csrf=([^;]*)/)?.[1] || '';
      const res = await fetch(base + '/api/v1/files?path=' + encodeURIComponent('/Users/zizdog/www'), {
        credentials: 'same-origin',
      });
      const j = await res.json();
      const names = (j?.data?.entries || [])
        .map((e) => e.path)
        .filter((p) => /zp-ui-test/.test(p));
      if (names.length) {
        await fetch(base + '/api/v1/files/delete', {
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
    // 回归：页面上不允许出现字面量 null / undefined（原生 append 会把它渲染成文本）
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

  await step('进入子目录并返回上级', async () => {
    // 找到第一个目录链接
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

    // 找到刚建的文件并点击编辑
    const link = page.locator('a', { hasText: 'zp-ui-test.txt' }).first();
    await link.waitFor({ timeout: 10000 });
    await link.click();
    await page.waitForSelector('.modal textarea', { timeout: 10000 });
    await page.waitForTimeout(600);
    await shot('42a-files-editor');
    await page.fill('.modal textarea', 'hello from ui test');
    await page.click('.modal button:has-text("保存")');
    await page.waitForTimeout(2500);
    await shot('42-files-edited');
  });

  await step('删除测试文件', async () => {
    const row = page.locator('tr', { hasText: 'zp-ui-test.txt' }).first();
    await row.locator('button:has-text("⋯")').click();
    await page.waitForSelector('.modal button:has-text("删除")', { timeout: 8000 });
    await page.click('.modal button:has-text("删除")');
    await page.waitForSelector('button:has-text("删除")', { timeout: 8000 });
    // 二次确认弹窗
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

  // ---------- 计划任务（P4）----------
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
    // 预览区应显示中文描述
    await page.waitForTimeout(1500);
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('每天') && !body.includes('执行')) {
      throw new Error('计划预览未显示描述: ' + body.slice(0, 150));
    }
    // 改一个非法表达式，应提示错误
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
    // 命令类型任务需要填命令
    const ta = page.locator('.modal textarea');
    if (await ta.count()) await ta.fill('echo ui-test');
    await page.click('.modal button:has-text("创建任务")');
    await page.waitForTimeout(3500);
    await shot('52-cron-created');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('zp-ui-cron')) throw new Error('任务未出现在列表中');

    // 删除它
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

  // ---------- 数据库管理（P4）----------
  await step('打开数据库管理', async () => {
    await page.click('.nav-item:has-text("数据库")');
    await page.waitForTimeout(3000);
    await shot('70-database');
    const txt = await page.locator('.content').innerText();
    // 要么显示已连接（有库列表），要么给出明确的连接失败提示
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

  // ---------- 日志中心（P4）----------
  await step('打开日志中心', async () => {
    await page.click('.nav-item:has-text("日志中心")');
    await page.waitForTimeout(2500);
    await shot('60-logs');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('日志文件')) throw new Error('日志中心未渲染: ' + txt.slice(0, 120));
    // 不允许出现字面量 null
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
    if (junk > 0) throw new Error('日志中心出现了 ' + junk + ' 处字面量 null');
  });

  await step('选择日志并实时尾随', async () => {
    // 选一个一定有内容的日志（站点访问日志）
    const item = page.locator('.nav-item', { hasText: '访问日志' }).first();
    if (!(await item.count())) return;
    await item.click();
    await page.waitForTimeout(3000);
    await shot('61-log-viewer');
    const box = page.locator('.logbox');
    if (!(await box.count())) throw new Error('日志查看区未渲染');
    const content = await box.innerText();
    if (content.includes('读取失败')) throw new Error('日志读取失败: ' + content.slice(0, 120));
    // 实时尾随应连上（内容里应出现日志行或提示）
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

  // ---------- Web 终端（P4）----------
  await step('终端默认关闭时给出明确提示', async () => {
    await page.click('.nav-item:has-text("Web 终端")');
    await page.waitForTimeout(2000);
    await shot('45-terminal-disabled');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('未启用') && !txt.includes('已连接') && !txt.includes('会话')) {
      throw new Error('终端页未正确渲染: ' + txt.slice(0, 120));
    }
  });

  // 终端会话的回收：这条锁住一个会让"终端用不了"的泄漏 ——
  // 离开页面后 PTY 读协程如果还阻塞着，会话就一直挂在列表里；
  // 本机 max_sessions=3，挂满之后用户再点终端就只会看到"会话上限"。
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

  // ---------- 操作审计（P1）----------
  await step('操作审计页：筛选、加载更多、导出入口', async () => {
    await page.click('.nav-item:has-text("操作审计")');
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

    // 关键词筛选：用一个**真实出现过**的动作名去筛。
    //
    // 这里以前写死搜 "login"，在一个全新的实例上必然失败：第一次进面板走的是
    // 「初始化」而不是「登录」，一条 login 记录都没有 —— 于是测试会在**正确行为**
    // 上报错（这个坑只有在全新实例上才暴露，对着用过的真机跑一直是绿的）。
    // 现在从页面的动作下拉里取一个真实动作名，测的是"筛选"这件事本身。
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

    // 导出入口存在（不实际下载，避免测试里产生文件）
    for (const label of ['导出 CSV', '导出 JSON']) {
      if (!(await page.locator(`button:has-text("${label}")`).count())) {
        throw new Error('缺少导出按钮: ' + label);
      }
    }
  });

  // ---------- Docker（P3）----------
  //
  // 这一段的价值在于"页面真的渲染出来了"：Docker 页有六个分区与十几个接口，
  // 单测只能证明接口对，证明不了前端不会在渲染时抛异常（历史上前端 bug 多数
  // 是页面白屏，接口全是 200）。所以这里逐个点过分区，并断言页面有内容。
  //
  // 允许 Docker 未安装：那是 macOS 上的常态，此时必须显示引导卡片而不是报错。
  await step('Docker 页可用（含各分区切换）', async () => {
    await page.click('.nav-item:has-text("Docker")');
    await page.waitForSelector('.card-head h3:has-text("Docker")', { timeout: 15000 });
    await page.waitForTimeout(2500); // 等首屏探测 Docker 环境
    await shot('46-docker-containers');

    const body = await page.locator('.content').innerText();
    // 回归：原生 append 把 null 渲染成字面量 "null"，这个项目踩过。
    // 必须匹配"独立成词"的 null/undefined（前后为空白或首尾），
    // 不能用 \b —— 那样会误伤 "Docker 内置网络" 这类正常文案。
    if (/(^|\s)(null|undefined)(\s|$)/.test(body)) {
      throw new Error('Docker 页出现了字面量 null/undefined: ' + body.slice(0, 200));
    }

    const unavailable = body.includes('Docker 环境不可用');
    if (unavailable) {
      if (!body.includes('Colima') && !body.includes('应用市场')) {
        throw new Error('Docker 不可用时必须给出安装引导，实际: ' + body.slice(0, 200));
      }
      return;
    }

    // 环境可用：六个分区都要能切过去且不报错
    //
    // 注意循环变量不能叫 shot —— 那会遮蔽上面的截图函数 shot()，
    // 于是 await shot(shot) 变成"拿字符串当函数调"，报错还很误导（shot is not a function）。
    for (const [tab, shotName] of [
      ['镜像', '47-docker-images'],
      ['数据卷', '48-docker-volumes'],
      ['网络', '49-docker-networks'],
      ['Compose', '50-docker-compose'],
      ['已纳管服务', '51-docker-services'],
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

    // 容器分区要能列出真实容器（沙箱里没有容器，只为覆盖渲染路径）
    await page.click('button:has-text("容器")');
    await page.waitForTimeout(1200);
    await shot('52-docker-containers-back');
  });

  await step('回到仪表盘', async () => {
    await page.click('.nav-item:has-text("仪表盘")');
    await page.waitForTimeout(1500);
  });

  await step('重新打开面板后会话保持', async () => {
    // 说明：这里用"先离开再回来"而不是 page.reload()。
    // 面板有 SSE 长连接，Playwright 的 reload 在长连接存在时
    // 会一直等待导航事件（表现为超时），这是测试工具层面的现象，
    // 与页面行为无关。goto 到同一 URL 能完整验证"刷新后仍免登录"。
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
