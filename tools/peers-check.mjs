// peers-check.mjs —— 「多机」页（C5）的真浏览器自检（本机自用）。
//
// 为什么需要它：接口级验收（两个实例 + curl）证明不了**页面接线**对不对 ——
// api.js 的方法名写错、NAV 少一项、表单字段名对不上，acorn 语法门禁一个都抓不到，
// 而用户看到的就是"点了没反应"。
//
// 用法（先起两个独立调试实例）：
//   make run-local LOCAL_ROOT=/tmp/zizpanel-c5p LOCAL_PORT=18446 LOCAL_SUFFIX=dev
//   make run-local LOCAL_ROOT=/tmp/zizpanel-c5c LOCAL_PORT=18447 LOCAL_SUFFIX=dev
//   node tools/peers-check.mjs http://127.0.0.1:18446/dev http://127.0.0.1:18447/dev
//
// 断言 8 件事：①「多机」在侧栏；② 本机接入信息默认关闭；③ 子机侧点开启拿到凭证；
// ④ 面板入口提示带后缀；⑤ 父面板填表添加子机；⑥ 卡片显示「已连接」与真实摘要；
// ⑦ 摘要里有版本/服务等聚合数字；⑧ 页面不出现报错文案。
import { chromium } from 'playwright';

const PARENT = (process.argv[2] || 'http://127.0.0.1:18446/dev').replace(/\/+$/, '');
const CHILD = (process.argv[3] || 'http://127.0.0.1:18447/dev').replace(/\/+$/, '');
const PASS = 'zizpanel-test-fixture-pass';

const results = [];
function check(name, ok, detail = '') {
  results.push({ name, ok });
  console.log(`${ok ? '✅' : '❌'} ${name}${detail ? ' — ' + detail : ''}`);
}

const browser = await chromium.launch({ args: ['--disable-dev-shm-usage', '--disable-gpu'] });

// loginPanel 打开一个实例并走真实登录/初始化表单（调试实例每次都可能是全新的）。
async function loginPanel(entry) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.log('PAGEERROR', e.message));
  await page.goto(entry + '/');
  await page.waitForTimeout(1200);
  const pws = page.locator('input[type=password]');
  if (await pws.count()) {
    const user = page.locator('input[type=text]').first();
    if (await user.count()) await user.fill('admin').catch(() => {});
    for (let i = 0; i < await pws.count(); i++) await pws.nth(i).fill(PASS);
    await pws.last().press('Enter');
    await page.waitForTimeout(3000);
  }
  const text = await page.locator('body').innerText();
  if (/创建管理员并进入面板|设置管理员/.test(text)) throw new Error(entry + ' 登录失败');
  return { ctx, page };
}

const child = await loginPanel(CHILD);
const parent = await loginPanel(PARENT);

// ① 侧栏有「多机」
await parent.page.goto(PARENT + '/#/peers');
await parent.page.waitForTimeout(1500);
let body = await parent.page.locator('body').innerText();
check('侧栏有「多机」入口', /多机/.test(body), body.replace(/\n+/g, ' | ').slice(0, 120));
check('页面渲染出「本机接入信息」与「添加子机」', /本机接入信息/.test(body) && /添加子机/.test(body),
  body.replace(/\n+/g, ' | ').slice(0, 200));
check('默认关闭（不是一上来就允许别人读本机）', /未开启|已关闭/.test(body));

// ③ 子机侧：点「开启」拿只读凭证
await child.page.goto(CHILD + '/#/peers');
await child.page.waitForTimeout(1500);
const enableBtn = child.page.locator('button', { hasText: '开启' }).first();
if (await enableBtn.count()) {
  await enableBtn.click();
  await child.page.waitForTimeout(1500);
}
const childBody = await child.page.locator('body').innerText();
check('子机侧开启后显示凭证区', /允许被主面板聚合|重新生成/.test(childBody),
  childBody.replace(/\n+/g, ' | ').slice(0, 200));

// 读出凭证（点「显示」再读 code 文本）
const showBtn = child.page.locator('button', { hasText: '显示' }).first();
if (await showBtn.count()) await showBtn.click();
await child.page.waitForTimeout(300);
const token = await child.page.evaluate(() => {
  const codes = [...document.querySelectorAll('code')];
  const t = codes.map((c) => (c.textContent || '').trim()).find((x) => /^[0-9a-f]{32,}$/.test(x));
  return t || '';
});
check('拿到 64 位只读凭证', /^[0-9a-f]{64}$/.test(token), token ? token.slice(0, 8) + '…' : '(空)');

// ④ 面板入口提示（给用户复制到另一台）
check('本机接入信息里有带后缀的面板入口',
  new RegExp(new URL(CHILD + '/').pathname.replace(/\//g, '\\/')).test(childBody) || /https?:\/\/[^\s]+/.test(childBody),
  childBody.replace(/\n+/g, ' | ').slice(0, 160));

// ⑤ 父面板填表添加子机
const inputs = parent.page.locator('#app input.input');
await inputs.nth(0).fill('child-panel');
await inputs.nth(1).fill(CHILD + '/');
await inputs.nth(2).fill(token);
await parent.page.locator('button', { hasText: '添加' }).first().click();
await parent.page.waitForTimeout(3000);
body = await parent.page.locator('body').innerText();
check('添加后出现子机卡片', /child-panel/.test(body), body.replace(/\n+/g, ' | ').slice(0, 200));
check('卡片显示「已连接」与真实摘要', /已连接/.test(body) && /版本/.test(body) && /服务/.test(body),
  body.replace(/\n+/g, ' | ').slice(0, 240));
check('摘要里有聚合数字（X/Y 运行、磁盘百分比）', /\d+\/\d+ 运行/.test(body) && /%/.test(body));
check('页面没有报错文案', !/读取子机列表失败|添加失败/.test(body));

// ⑨ 诚实性：故意填错凭证，界面必须显示「没连上」+ 原因，而不是继续显示"已连接"
const inputs2 = parent.page.locator('#app input.input');
await inputs2.nth(0).fill('bad-token');
await inputs2.nth(1).fill(CHILD + '/');
await inputs2.nth(2).fill('definitely-wrong-token');
await parent.page.locator('button', { hasText: '添加' }).first().click();
await parent.page.waitForTimeout(3000);
const afterBad = await parent.page.locator('body').innerText();
check('凭证错时显示「没连上」', /没连上/.test(afterBad), afterBad.replace(/\n+/g, ' | ').slice(0, 240));
check('失败原因如实显示在卡片上（403 + 凭证不对）', /403/.test(afterBad) && /凭证不对/.test(afterBad));
check('失败的那台没有摘要数字（不拿旧数据冒充）', !/没连上[\s\S]{0,80}\d+\/\d+ 运行/.test(afterBad));
check('给出可操作的修法（不是只有一句报错）', /重新生成/.test(afterBad), afterBad.replace(/\n+/g, ' | ').slice(-260));

await browser.close();
const failed = results.filter((r) => !r.ok);
console.log(`\n${results.length - failed.length}/${results.length} 项通过`);
if (failed.length) {
  console.log('失败项：' + failed.map((f) => f.name).join('；'));
  process.exit(1);
}
