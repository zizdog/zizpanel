// e2e-check.mjs —— 油猴脚本的端到端自检（本机自用）。
//
// 它干的事：把 aria2-helper.user.js 注入一个真浏览器页面，用一个忠实的 GM shim
// 把 GM_xmlhttpRequest 搬到 Node（Node 没有同源策略，正好像扩展通道），并且**故意**
// 给每个请求打上 `Sec-Fetch-Site: cross-site`（浏览器一定会打，扩展也躲不掉）——
// 然后拿真的面板 8898 + 真的 aria2 跑：链接识别 / 带凭证的 RPC / 真推送落盘 /
// 凭证写错时报错是否可照做。
//
// 依赖本机环境：面板口令在 .panel-credential.local、rpc-secret 在 ~/aria/aria2.conf。
// 用法：node tools/userscript/e2e-check.mjs
// 端到端验证油猴脚本：真浏览器页面 + GM shim（GM_xmlhttpRequest → Node fetch，
// 等价于扩展通道：不受页面同源策略约束）+ 真的面板 8898 + 真的 aria2。
import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
import { execSync } from 'node:child_process';

const SRC = readFileSync('tools/userscript/aria2-helper.user.js', 'utf8');
const RPC = 'http://127.0.0.1:8898/jsonrpc';

// 只需要 aria2 自己的 rpc-secret —— 面板默认不按浏览器来源拦（2026-09-24 起），
// 所以脚本这条路只用这一个凭据就够（X-Aria2-Token 机制已按用户要求删除）。
const secret = execSync(`sed -n 's/^rpc-secret=//p' ${process.env.HOME}/aria/aria2.conf`).toString().trim();
console.log('（凭据已取：rpc-secret ' + secret.length + ' 位）');

const browser = await chromium.launch();
const page = await browser.newContext({ ignoreHTTPSErrors: true }).then((c) => c.newPage());

// GM shim：把扩展通道的请求搬到 Node（Node 无同源策略，正好模拟 GM_xmlhttpRequest）
await page.exposeFunction('__gmFetch', async ({ url, headers, data }) => {
  try {
    const r = await fetch(url, { method: 'POST', headers, body: data });
    const text = await r.text();
    const hdrs = [...r.headers.entries()].map(([k, v]) => k + ': ' + v).join('\r\n');
    return { status: r.status, statusText: r.statusText, responseText: text, responseHeaders: hdrs };
  } catch (e) {
    return { status: 0, statusText: String(e.message), responseText: '' };
  }
});
const shim = `
  window.__store = {};
  function GM_getValue(k, d){ return k in window.__store ? window.__store[k] : d; }
  function GM_setValue(k, v){ window.__store[k] = v; }
  function GM_registerMenuCommand(){}
  function GM_xmlhttpRequest(o){
    // 浏览器一定会给跨站请求打上这个头，扩展通道也躲不掉 —— 正是它触发面板的 403
    var hs = Object.assign({}, o.headers, { 'Sec-Fetch-Site': 'cross-site' });
    window.__gmFetch({ url: o.url, headers: hs, data: o.data }).then(function(r){
      if (o.onload) o.onload(r);
    }).catch(function(e){ if (o.onerror) o.onerror({ error: String(e && e.message || e) }); });
  }
`;

await page.setContent(`<html><body>
  <a href="https://example.com/a.zip">zip 包</a>
  <a href="https://example.com/index.html">页面</a>
  <a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567">磁力</a>
</body></html>`);
await page.addScriptTag({ content: shim + '\n' + SRC });

const results = [];
const check = (name, okv, extra) => { results.push([name, okv, extra]); console.log((okv ? '✅ ' : '❌ ') + name + (extra ? '  → ' + extra : '')); };

// ① 页面链接识别
const links = await page.evaluate(() => window.__A2H.collectLinks().map((l) => l.url));
check('① 页面可推送链接=zip+磁力（html 被过滤）', links.length === 2 && links[0].endsWith('a.zip') && links[1].startsWith('magnet:'), JSON.stringify(links));

// ② 连接测试（真 RPC，带密钥 + 凭证）
const v = await page.evaluate(async ({ rpc, secret, token }) => {
  window.__A2H.cfg.rpc = rpc; window.__A2H.cfg.rpcSecret = secret;
  try { return { ok: true, version: (await window.__A2H.rpc('aria2.getVersion', [])).version }; }
  catch (e) { return { ok: false, err: String(e.message || e) }; }
}, { rpc: RPC, secret });
check('② RPC 通（只用 rpc-secret，不需要额外头）', v.ok && !!v.version, JSON.stringify(v));

// ③ 真推送一条，并确认落到下载目录
const gid = await page.evaluate(async () => {
  try { return { ok: true, gid: await window.__A2H.push('http://127.0.0.1:8898/js/aria-ng-a5324ae04a.min.js') }; }
  catch (e) { return { ok: false, err: String(e.message || e) }; }
});
check('③ 推送链接拿到 gid', gid.ok && /^[0-9a-f]{8,}$/.test(gid.gid || ''), JSON.stringify(gid));

// ③b 任务真的完成、且落到 aria2 的下载目录（默认 ~/Downloads）
if (gid.ok) {
  let st = null;
  for (let i = 0; i < 12; i++) {
    st = await page.evaluate(async ({ rpc, secret, gid }) => {
      window.__A2H.cfg.rpc = rpc; window.__A2H.cfg.rpcSecret = secret;
      try { return await window.__A2H.rpc('aria2.tellStatus', [gid, ['status', 'files', 'completedLength', 'errorMessage']]); }
      catch (e) { return { err: String(e.message || e) }; }
    }, { rpc: RPC, secret, gid: gid.gid });
    if (st && (st.status === 'complete' || st.status === 'error')) break;
    await new Promise((r) => setTimeout(r, 1000));
  }
  const path = st && st.files && st.files[0] && st.files[0].path || '';
  check('③b 任务完成且落在下载目录', st && st.status === 'complete' && path.includes('/Downloads/'), st && (st.status + ' ' + path + ' ' + (st.errorMessage || '')));
}

// ④ 负向：**密钥**写错必须给出能照做的错误（而不是静默失败）
const bad = await page.evaluate(async () => {
  window.__A2H.cfg.rpcSecret = 'definitely-wrong';
  try { await window.__A2H.rpc('aria2.getVersion', []); return { ok: true }; }
  catch (e) { return { ok: false, err: String(e.message || e) }; }
});
check('④ 密钥写错时如实报错', !bad.ok && /Unauthorized|error/i.test(bad.err || ''), JSON.stringify(bad));

await browser.close();
console.log(results.every((r) => r[1]) ? '\n全部通过' : '\n有失败项');
process.exit(results.every((r) => r[1]) ? 0 : 1);
