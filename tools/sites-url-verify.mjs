// tools/sites-url-verify.mjs —— 用真实浏览器验证「站点列表域名列」的新口径。
//
// 用户 2026-09-25 明确要求：不要 blog.x / http://blog.x / https://blog.x 三种都列，
// 只显示**域名文本本身**，并且这段文字本身就是链接（target=_blank + rel=noopener）；
// 协议等附加信息用别的方式表达（小字/title/图标）。
//
// 用法：
//   node tools/sites-url-verify.mjs http://127.0.0.1:18450/dev/ /tmp/zp-sites-shots
//
// 退出码非 0 = 断言失败。需要在沙箱 DB 里先放一条站点记录（测试脚本自己插入，
// 不经过需要 root 的建站链路）。

import { chromium } from 'playwright';
import { mkdirSync } from 'node:fs';

const base = (process.argv[2] || 'http://127.0.0.1:18450/dev/').replace(/\/+$/, '') + '/';
const outDir = process.argv[3] || '/tmp/zp-sites-shots';
const PASSWORD = process.env.ZP_PASS || 'zizpanel-test-fixture-pass';
mkdirSync(outDir, { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  ignoreHTTPSErrors: true,
  locale: 'zh-CN',
});
const page = await ctx.newPage();
const errors = [];
page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));

try {
  // ---- 登录 / 首次初始化 ----
  await page.goto(base, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('input[autocomplete="username"]', { timeout: 20000 });
  if (await page.locator('text=首次初始化').count()) {
    await page.fill('input[autocomplete="username"]', 'admin');
    await page.fill('input[placeholder="至少 8 位"]', PASSWORD);
    await page.fill('input[placeholder="再次输入"]', PASSWORD);
    await page.click('button:has-text("创建管理员并进入面板")');
  } else {
    await page.fill('input[autocomplete="username"]', 'admin');
    await page.fill('input[autocomplete="current-password"]', PASSWORD);
    await page.click('button:has-text("登 录")');
  }
  await page.waitForSelector('.layout', { timeout: 25000 });

  // ---- 网站管理 ----
  await page.click('.nav-item:has-text("网站管理")');
  await page.waitForSelector('.zp-table-wrap table.table tbody tr', { timeout: 20000 });
  await page.waitForTimeout(500);

  const siteRows = page.locator('.zp-table-wrap table.table tbody tr:not(.zp-default-row)');
  const n = await siteRows.count();
  if (!n) throw new Error('站点列表里没有已注册站点（沙箱 DB 里应先插入一条测试站点）');
  const cell = siteRows.first().locator('td').first();
  const links = await cell.locator('a').evaluateAll((as) => as.map((a) => ({
    href: a.getAttribute('href'),
    target: a.getAttribute('target'),
    rel: a.getAttribute('rel'),
    text: (a.textContent || '').trim(),
  })));
  const cellText = (await cell.innerText()).trim();
  console.log('域名列文本:\n' + cellText);
  console.log('链接:', JSON.stringify(links));
  if (links.length !== 1) throw new Error('域名处应恰好 1 个链接，实际 ' + links.length + '：' + JSON.stringify(links));
  const l = links[0];
  if (!/^https?:\/\//.test(String(l.href || ''))) throw new Error('域名链接不是 http(s)：' + JSON.stringify(l));
  if (l.target !== '_blank') throw new Error('域名链接没有 target=_blank：' + JSON.stringify(l));
  if (!String(l.rel || '').includes('noopener')) throw new Error('域名链接没有 rel=noopener：' + JSON.stringify(l));
  if (/^https?:\/\//i.test(l.text)) throw new Error('域名链接文本带协议前缀：' + JSON.stringify(l));
  if (cellText.includes('http://') || cellText.includes('https://')) {
    throw new Error('域名列里仍出现 http:// 或 https:// 文本：\n' + cellText);
  }
  if (!/(https?|🔒)/.test(cellText)) throw new Error('域名列丢了协议信息：\n' + cellText);
  await page.screenshot({ path: outDir + '/sites-domain-link.png' });

  // ---- 反向代理首屏耗时（异步探测不应拖慢首屏）----
  let proxyApiMs = -1;
  let navAt = 0;
  page.on('response', (r) => {
    if (r.url().includes('/api/v1/proxies') && r.request().method() === 'GET') {
      if (proxyApiMs < 0 && navAt) proxyApiMs = Date.now() - navAt;
    }
  });
  const t0 = Date.now();
  navAt = t0;
  await page.click('.nav-item:has-text("反向代理")');
  await page.waitForFunction(() => {
    const t = document.body.innerText || '';
    return t.includes('监听 :') || t.includes('还没有反向代理规则');
  }, null, { timeout: 20000 });
  const firstPaintMs = Date.now() - t0;
  // 等异步探测把徽标从"检测中…"更新出来（最多 6s）。
  await page.waitForTimeout(2500);
  const bodyText = await page.locator('.card-body').last().innerText();
  await page.screenshot({ path: outDir + '/reverseproxy-firstscreen.png' });
  console.log('反向代理首屏可交互 = ' + firstPaintMs + ' ms；GET /api/v1/proxies = ' + proxyApiMs + ' ms');
  const statusLines = bodyText.split('\n').filter((s) => s.includes('状态检测时间') || s.includes('检测中') || s.includes('目标不可达'));
  console.log('状态行（异步探测后）:\n' + statusLines.slice(0, 8).join('\n'));
  if (bodyText.includes('目标检测中…')) {
    console.log('提示：仍有"目标检测中…"（异步探测还没回来或失败）');
  }

  if (errors.length) throw new Error('页面有 JS 异常：\n' + errors.join('\n'));

  // ---- 反代鉴权 UI：开关 + 用户名 + 密码字段，开关联动显隐 ----
  await page.click('button:has-text("新建规则")');
  await page.waitForSelector('#zp-proxy-auth-detail', { state: 'attached', timeout: 10000 });
  const authBox = page.locator('#zp-proxy-auth-detail');
  if (await authBox.isVisible()) throw new Error('「需要用户名密码」未勾选时不应显示用户名/密码字段');
  const authLabel = page.locator('label:has-text("需要用户名密码") input[type="checkbox"]');
  if (!(await authLabel.count())) throw new Error('反代表单缺少「需要用户名密码」开关');
  await authLabel.check();
  if (!(await authBox.isVisible())) throw new Error('勾选后应显示用户名/密码字段');
  const userInput = authBox.locator('input.input').nth(0);
  const passInput = authBox.locator('input[type="password"]');
  if (!(await userInput.count()) || !(await passInput.count())) {
    throw new Error('鉴权字段缺少用户名或密码输入框');
  }
  await userInput.fill('tester');
  await passInput.fill('s3cret');
  await page.screenshot({ path: outDir + '/reverseproxy-auth-form.png' });
  console.log('✅ 反代鉴权表单：开关 + 用户名 + 密码字段都在，且开关联动显隐');

  console.log('✅ 站点域名列断言通过；截图在 ' + outDir);
} finally {
  await browser.close();
}
