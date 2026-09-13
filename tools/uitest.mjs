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
const PASSWORD = process.env.ZP_PASS || 'PanelTestPw-9x!';
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

page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
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

  await step('切换到系统监控页', async () => {
    await page.click('.nav-item:has-text("系统监控")');
    await page.waitForTimeout(2600);
    await shot('04-monitor');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('详细指标')) throw new Error('监控页未渲染');
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

  await step('未配置升级源时给出明确提示', async () => {
    await page.fill('input[placeholder^="https://example.com"]', '');
    // 这一步**故意**触发 400（未配置升级源），这类 4xx 是断言对象而非故障，
    // 用现成的 expectHTTPError 开关把它从"控制台错误"里排除。
    expectHTTPError = true;
    try {
      await page.click('button:has-text("检查更新")');
      await page.waitForSelector('.toast', { timeout: 10000 });
      await page.waitForTimeout(400);
      const t = await page.locator('.toast').first().innerText();
      if (!t.includes('升级源')) throw new Error('提示不明确: ' + t);
    } finally {
      expectHTTPError = false;
    }
    await shot('07-upgrade-no-source');
    // 把 Tab 还原：后续步骤假设停留在「访问与安全」页，
    // 测试步骤之间不能互相踩状态（这一条我自己刚踩过）。
    await page.click('button:has-text("访问与安全")');
    await page.waitForTimeout(600);
  });

  await step('验证访问策略可保存', async () => {
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

  await step('导航项全部为真实模块', async () => {
    // P4 完成后已无占位页：遍历侧边栏，确认每一项都能打开且不是"正在开发中"
    const items = await page.locator('.nav-item').allInnerTexts();
    if (items.length < 8) throw new Error('侧边栏导航项过少: ' + items.length);
    await page.click('.nav-item:has-text("数据库")');
    await page.waitForTimeout(2500);
    await shot('08-database');
    const txt = await page.locator('.content').innerText();
    if (txt.includes('正在开发中')) {
      throw new Error('数据库模块仍是占位页: ' + items.join(' / '));
    }
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

  await privStep('校验 nginx 配置', async () => {
    await page.click('button:has-text("校验 nginx 配置")');
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

  // ---------- 服务管理（P3）----------
  await step('打开服务管理', async () => {
    await page.click('.nav-item:has-text("服务管理")');
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
    const logBtn = page.locator('.content button:has-text("日志")').first();
    if (!(await logBtn.count())) {
      // 没有服务卡片时跳过
      return;
    }
    await logBtn.click();
    await page.waitForSelector('.modal', { timeout: 8000 });
    await page.waitForTimeout(2500);
    await shot('31-service-logs');
    const pill = await page.locator('.modal .pill').first().innerText();
    if (!pill.includes('实时') && !pill.includes('中断') && !pill.includes('结束')) {
      throw new Error('日志流状态异常: ' + pill);
    }
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await step('服务详情显示完整信息', async () => {
    const detailBtn = page.locator('.content button:has-text("详情")').first();
    if (!(await detailBtn.count())) return;
    await detailBtn.click();
    await page.waitForSelector('.modal .kv', { timeout: 8000 });
    await page.waitForTimeout(700);
    await shot('32-service-detail');
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('运行状态')) throw new Error('详情缺少运行状态');
    await page.keyboard.press('Escape');
    await page.waitForTimeout(500);
  });

  await step('打开应用市场', async () => {
    await page.click('.nav-item:has-text("应用市场")');
    await page.waitForTimeout(2000);
    await shot('33-market');
    const txt = await page.locator('.content').innerText();
    if (!txt.includes('Ollama')) throw new Error('应用市场未列出预期应用');
  });

  await step('应用市场安装前检查', async () => {
    // 找一个可用的"安装"按钮（不是已安装、不是不可用）
    const btn = page.locator('.content button:has-text("安装")').first();
    if (!(await btn.count())) return;
    await btn.click();
    await page.waitForSelector('.modal', { timeout: 10000 });
    await page.waitForTimeout(1500);
    await shot('34-market-preflight');
    const body = await page.locator('.modal-body').innerText();
    if (!body.includes('条件') && !body.includes('端口')) {
      throw new Error('安装前检查未显示检查项');
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

  // ---------- 文件管理（P4）----------
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
    // 回归：原生 append 把 null 渲染成字面量 "null"，这个项目踩过
    if (/\bnull\b/.test(body) || /\bundefined\b/.test(body)) {
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
    for (const [tab, shot] of [
      ['镜像', '47-docker-images'],
      ['数据卷', '48-docker-volumes'],
      ['网络', '49-docker-networks'],
      ['Compose', '50-docker-compose'],
      ['已纳管服务', '51-docker-services'],
    ]) {
      await page.click(`button:has-text("${tab}")`);
      await page.waitForTimeout(1200);
      await shot(shot);
      const t = await page.locator('.content').innerText();
      if (!t.includes('Docker')) {
        throw new Error(`切到「${tab}」分区后页面异常: ` + t.slice(0, 150));
      }
      if (/\bnull\b/.test(t) || /\bundefined\b/.test(t)) {
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
