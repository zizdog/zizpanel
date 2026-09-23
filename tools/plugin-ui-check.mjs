// plugin-ui-check.mjs —— 本地插件「前端分区」的端到端自检（本机自用）。
//
// 它干的事：起一个调试实例（make run-local），往它的插件目录放两份声明（一份合法、一份坏 JSON），
// 然后用真浏览器走真实登录表单进「应用 → 应用市场」，断言：
//   ① 出现「本地插件」分区；② 坏文件被列出来并说明原因；③ 默认未启用；
//   ④ 「看计划」能展开干跑输出；⑤ 启用后市场里出现这张卡；⑥ 停用后卡片消失（插件仍在列表里）。
//
// 依赖：先 `make run-local`（脚本会自己往调试实例的插件目录写文件）。
// 用法：node tools/plugin-ui-check.mjs http://127.0.0.1:18443/dev
// B2b 前端验收：真浏览器 + 真接口（调试实例）—— 本地插件分区可见、启用后卡片出现、停用后消失。
import { chromium } from 'playwright';
import { mkdirSync, writeFileSync } from 'node:fs';
// 调试实例每次 make run-local 都会清空自己的根目录 ⇒ 插件文件必须在它起来之后再写。
const PLUGIN_DIR = '/tmp/zizpanel-dev/plugins';
mkdirSync(PLUGIN_DIR, { recursive: true });
writeFileSync(PLUGIN_DIR + '/myapp.json', JSON.stringify({
  schema: 'zizpanel.app/v1', id: 'myapp', name: '我的本地插件', icon: '🧩',
  summary: 'UI 验证用（不真的安装）',
  source: { kind: 'brew', formula: 'mosquitto', checksum: 'sha256' },
  run: { mode: 'brew-service' },
  expose: { port: 18883, bind: '0.0.0.0', ui: 'app' },
  verify: { any_of: [{ kind: 'http', path: '/', expect: '', timeout: '60s' }] },
  health: { kind: 'http', path: '/', expect: '', interval: '30s' },
  uninstall: { always: ['/Library/LaunchDaemons/homebrew.mxcl.mosquitto.plist'],
    optional_data: ['~/mosquitto'], formula: 'mosquitto' },
  update: { kind: 'brew' },
  requires: { system_daemon: true, ports: [18883] },
}, null, 2));
writeFileSync(PLUGIN_DIR + '/broken.json', '{not json\n');
console.log('已放好两份本地插件：', PLUGIN_DIR);
const BASE = (process.argv[2] || 'http://127.0.0.1:18443/dev').replace(/\/+$/, '');
const PASS = 'zizpanel-test-fixture-pass';
const browser = await chromium.launch({ args: ['--disable-dev-shm-usage', '--disable-gpu'] });
browser.on('disconnected', () => console.log('!! 浏览器断开（很可能被系统杀掉）'));
const page = await (await browser.newContext()).newPage();
page.on('pageerror', (e) => console.log('PAGEERROR', e.message));
// 走**真实登录/初始化表单**：调试实例每次都是全新的，首次会出「初始化」表单
// （用户名 + 密码 + 确认密码），所以要把**所有**密码框都填上。
await page.goto(BASE + '/');
await page.waitForTimeout(1000);
const pws = page.locator('input[type=password]');
if (await pws.count()) {
  const userBox = page.locator('input[type=text]').first();
  if (await userBox.count()) await userBox.fill('admin').catch(() => {});
  for (let i = 0; i < await pws.count(); i++) await pws.nth(i).fill(PASS);
  await pws.last().press('Enter');
  await page.waitForTimeout(3000);
}
console.log('登录后是否已进面板：', /登录账号/.test(await page.locator('body').innerText()));
// 直接以 #/apps/market 整页加载：应用页默认落在「已安装」，插件分区在「应用市场」里。
await page.goto(BASE + '/#/apps/market');
await page.waitForTimeout(1500);
// 首屏要等 brew 探测（冷启动可能十几秒），等市场卡片出现再继续
await page.waitForSelector('.grid .card', { timeout: 40000 }).catch(() => {});
await page.waitForTimeout(1500);
console.log('URL:', page.url());
console.log('可见文本片段:', (await page.locator('body').innerText()).replace(/\n+/g, ' | ').slice(0, 300));

const txt = () => page.locator('body').innerText();
async function pluginBlockText() {
  return page.evaluate(() => {
    const titles = [...document.querySelectorAll('.section-title')];
    const t = titles.find((x) => (x.textContent || '').includes('本地插件'));
    if (!t) return '(没有本地插件分区)';
    let node = t.nextElementSibling, out = t.textContent + '\n';
    while (node && !node.classList.contains('section-title')) { out += node.innerText + '\n'; node = node.nextElementSibling; }
    return out;
  });
}
const out = [];
const check = (n, ok, extra) => { out.push(ok); console.log((ok ? '✅ ' : '❌ ') + n + (extra ? '  → ' + extra : '')); };

// ① 分区与说明出现
await page.getByText('本地插件', { exact: false }).first().waitFor({ timeout: 30000 });
let t = await txt();
check('① 出现「本地插件」分区', /本地插件/.test(t));
console.log('--- 插件分区实际渲染 ---\n' + (await pluginBlockText()));
check('② 坏文件被列出来并说明问题', /声明有问题/.test(t), (t.match(/声明有问题[\s\S]{0,80}/) || [''])[0].replace(/\n/g, ' '));
check('③ 插件默认未启用', /未启用/.test(t));

// ④ 看计划（展开干跑输出）
const planBtn = page.getByRole('button', { name: '看计划' }).first();
if (await planBtn.count()) {
  await planBtn.click();
  await page.waitForTimeout(400);
  t = await txt();
  check('④ 计划里能看到"会做什么"', /干跑/.test(t) && /Homebrew formula mosquitto/.test(t));
} else {
  check('④ 计划按钮存在', false);
}

// ⑤ 启用 → 市场里出现这张卡
const enableBtn = page.getByRole('button', { name: '启用' }).first();
await enableBtn.click();
await page.waitForTimeout(2500);
t = await txt();
check('⑤ 启用后市场里出现插件卡片', /我的本地插件/.test(t) && /已启用/.test(t));

// ⑥ 停用 → 卡片消失
const disableBtn = page.getByRole('button', { name: '停用' }).first();
await disableBtn.click();
await page.waitForTimeout(2500);
t = await txt();
check('⑥ 停用后卡片不再出现在市场（但插件仍在列表里）', /本地插件/.test(t) && !/已启用/.test(t));

await page.screenshot({ path: '/tmp/zp-plugin-ui.png', fullPage: false });
await browser.close();
console.log(out.every(Boolean) ? '\n全部通过' : '\n有失败项');
process.exit(out.every(Boolean) ? 0 : 1);
