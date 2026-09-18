// 语音转文字（whisper.cpp）Web UI 的真机验证脚本。
//
// 用法（需要一个跑起来的 stt-serve；见下面"怎么起服务"）：
//
//   node tools/stt-ui-verify.mjs <base-url> <3秒音频> <长音频> [证据目录]
//
//   例：
//     node tools/stt-ui-verify.mjs http://127.0.0.1:18892 /tmp/stt/test-3s.wav /tmp/stt/test-134s.wav /tmp/stt
//
// 怎么起服务（不需要 root、不碰 launchd、用临时模型根目录）：
//
//   # 1. 引擎（原生 arm64 瓶，实测 8.6 秒）
//   brew install whisper.cpp
//   # 2. 默认档模型（466 MiB；走 hf-mirror，实测约 10 分钟）
//   mkdir -p /tmp/stt/models && printf 'small\n' > /tmp/stt/current_model
//   curl -L -C - -o /tmp/stt/models/ggml-small.bin \
//     https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
//   # 3. 中文测试音频（系统自带 say + ffmpeg）
//   say -v Tingting -o /tmp/stt/say.aiff "今天天气不错，测试语音转文字。"
//   ffmpeg -y -i /tmp/stt/say.aiff -ar 16000 -ac 1 -c:a pcm_s16le /tmp/stt/test-3s.wav
//   ffmpeg -y -stream_loop 39 -i /tmp/stt/test-3s.wav -c copy /tmp/stt/test-134s.wav
//   # 4. 起服务（用真机二进制；`go build -o /tmp/zizpanel ./cmd/zizpanel`）
//   /tmp/zizpanel stt-serve --listen 127.0.0.1:18892 --brew-prefix /opt/homebrew --root /tmp/stt
//
// 它检查三件事：
//   ① 短音频同步路径：真的转出中文（断言文本含「今天天气不错」）+ 截图；
//   ② 134 秒长音频 → 202 + 任务中心 → 抓**跑到一半**的真实分片进度 + 截图；
//   ③ 按 tools/ui-audit.mjs 的**同两条硬规则**审计新页面（横向溢出 / 无名按钮）。
//
// 为什么需要这个脚本、而不是直接跑 tools/ui-audit.mjs：
//   那个脚本的 ROUTES 是**面板自己的路由**（仪表盘/文件/网站/…），而 `/stt/` 是
//   应用页面（独立进程提供，直连端口与面板别名共用同一份 HTML）。它审计不到，
//   所以这里用**同一套判据**（de.scrollWidth 溢出 + 按钮有无文字/title/aria-label）
//   审计新页面。面板路由那一份仍然由 tools/ui-audit.mjs 负责，两者互补。
//
// 需要 playwright（本仓库 node_modules 里已有）。
import { chromium } from 'playwright';

const BASE = process.argv[2] || 'http://127.0.0.1:18892';
const SHORT = process.argv[3];
const LONG = process.argv[4];
const OUT = process.argv[5] || '/tmp/stt';

if (!SHORT || !LONG) {
  console.error('用法：node tools/stt-ui-verify.mjs <base-url> <3秒音频> <长音频> [证据目录]');
  process.exit(2);
}

const hard = [];
const soft = [];
const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
const page = await ctx.newPage();
const consoleErrors = [];
page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
page.on('pageerror', (e) => consoleErrors.push('pageerror: ' + e.message));

const ready = async () => {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => {
    const t = document.getElementById('engine-text');
    return t && !t.textContent.includes('正在探测');
  }, { timeout: 90000 });
};

console.log('=== 语音转文字 UI 真机验证 ===');
console.log(`入口 ${BASE}/  证据目录 ${OUT}\n`);

// ---------- ① 短音频（同步路径） ----------
await ready();
const engineText = (await page.textContent('#engine-text')).trim();
console.log('① 引擎徽章：', engineText);
if (!engineText.includes('就绪')) hard.push(`① 引擎徽章不是「就绪」：${engineText}`);

await page.selectOption('#language', 'zh');
await page.selectOption('#model', 'small');
await page.selectOption('#format', 'srt');
await page.setInputFiles('#file', SHORT);
await page.waitForFunction(() => !document.getElementById('fileinfo').hidden, { timeout: 20000 });
console.log('   已选文件：', ((await page.textContent('#fi-name')) || '').trim());

await page.click('#btn-run');
await page.waitForFunction(() => !document.getElementById('result-wrap').hidden, { timeout: 180000 });
const text = (await page.textContent('#out-text')) || '';
const segCount = await page.locator('#out-seg .seg').count();
const meta = (await page.textContent('#result-meta')) || '';
console.log('   转出文本：', JSON.stringify(text.trim()));
console.log('   元信息：', meta.trim());
console.log('   进度条已收起：', await page.isHidden('#progress-wrap'));
await page.screenshot({ path: `${OUT}/ui-1-sync-result.png`, fullPage: true });

if (!text.includes('今天天气不错')) hard.push(`① 转出的文本里没有「今天天气不错」：${JSON.stringify(text)}`);
else soft.push('① 同步路径转出中文成功（含「今天天气不错」）');
if (segCount < 1) hard.push('① 带时间戳的分段区是空的');
if (!(await page.isHidden('#progress-wrap'))) hard.push('① 同步返回后进度条没有收起（会让人以为后台还在跑）');

await page.click('#tab-seg');
const segVisible = await page.isVisible('#out-seg');
await page.click('#tab-text');
if (!segVisible || !(await page.isVisible('#out-text'))) hard.push('① 结果页签切换不生效');

const [dl] = await Promise.all([
  page.waitForEvent('download', { timeout: 20000 }).catch(() => null),
  page.click('#btn-dl-txt'),
]);
if (!dl) hard.push('① 点「下载 .txt」没有触发下载');
else console.log('   下载文件名：', dl.suggestedFilename());

// ---------- ② 长音频（任务 + 真实进度） ----------
console.log('\n② 长音频走任务中心（按 60 秒切片）');
await ready();
await page.selectOption('#language', 'zh');
await page.selectOption('#model', 'small');
await page.setInputFiles('#file', LONG);
await page.waitForFunction(() => !document.getElementById('fileinfo').hidden, { timeout: 30000 });
await page.click('#btn-run');
await page.waitForFunction(() => !document.getElementById('progress-wrap').hidden, { timeout: 60000 });

let midPct = 0;
let midLog = '';
for (let i = 0; i < 300; i++) {
  const pct = parseInt((await page.textContent('#progress-pct')) || '0', 10);
  if (pct > 0 && pct < 100) {
    midPct = pct;
    midLog = (await page.textContent('#progress-log')) || '';
    await page.screenshot({ path: `${OUT}/ui-2-task-progress.png`, fullPage: false });
    break;
  }
  await page.waitForTimeout(200);
}
console.log(`   中途进度：${midPct}%`);
console.log('   真实进度日志：', midLog.split('\n').filter((l) => l.includes('片')).slice(-2).join(' || '));
if (!midPct) hard.push('② 没抓到中间进度（进度可能是假的）');
if (!/已完成\s*\d+\/\d+\s*片/.test(midLog)) hard.push(`② 进度日志里没有真实的分片计数：${midLog}`);

await page.waitForFunction(() => !document.getElementById('result-wrap').hidden, { timeout: 600000 });
const longText = (await page.textContent('#out-text')) || '';
const longMeta = (await page.textContent('#result-meta')) || '';
console.log('   结果元信息：', longMeta.trim());
console.log('   结果前 48 字：', JSON.stringify(longText.trim().slice(0, 48)));
await page.screenshot({ path: `${OUT}/ui-3-task-result.png`, fullPage: true });
if (!longText.includes('今天天气不错')) hard.push('② 长音频结果里没有「今天天气不错」');
if (!/切片\s*\d+/.test(longMeta)) hard.push(`② 结果元信息里应显示切片数：${longMeta}`);

// ---------- ③ ui-audit 同规则 ----------
console.log('\n③ ui-audit 同规则检查（横向溢出 / 无名按钮）');
await ready();
for (const w of [1440, 1280, 1024]) {
  await page.setViewportSize({ width: w, height: 1000 });
  await page.waitForTimeout(200);
  const m = await page.evaluate(() => {
    const de = document.documentElement;
    const wide = [];
    for (const el of document.querySelectorAll('body *')) {
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.right > de.clientWidth + 2) {
        wide.push(`${el.tagName.toLowerCase()}${el.id ? '#' + el.id : ''} right=${Math.round(r.right)}`);
      }
    }
    return { over: de.scrollWidth - de.clientWidth, wide: wide.slice(0, 5) };
  });
  if (m.over > 2) {
    hard.push(`③ ${w}px 横向溢出 ${m.over}px：${m.wide.join(', ')}`);
    await page.screenshot({ path: `${OUT}/ui-4-overflow-${w}.png`, fullPage: true });
  } else console.log(`   ${w}px 无横向溢出 ✅`);
}
const unnamed = await page.evaluate(() => {
  const out = [];
  for (const b of document.querySelectorAll('button, a.button, a.btn')) {
    const cs = getComputedStyle(b);
    if (cs.display === 'none' || cs.visibility === 'hidden') continue;
    if (!((b.textContent || '').trim() || b.getAttribute('title') || b.getAttribute('aria-label'))) {
      out.push(b.outerHTML.slice(0, 90));
    }
  }
  return out;
});
if (unnamed.length) hard.push(`③ 有 ${unnamed.length} 个无名按钮：${unnamed.join(' | ')}`);
else console.log('   无名按钮：0 个 ✅');
if (consoleErrors.length) soft.push(`③ 控制台错误 ${consoleErrors.length} 条：${consoleErrors.slice(0, 3).join(' | ')}`);

await browser.close();
if (soft.length) { console.log('\n--- 提示 ---'); soft.forEach((s) => console.log('  ' + s)); }
if (hard.length) {
  console.log(`\n❌ ${hard.length} 个硬问题：`);
  hard.forEach((h) => console.log('  ' + h));
  process.exit(1);
}
console.log('\n✅ 同步路径、长音频真实进度、ui-audit 同规则检查全部通过。');
