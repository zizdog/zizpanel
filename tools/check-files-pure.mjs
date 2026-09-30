#!/usr/bin/env node
// ============================================================================
//  check-files-pure.mjs —— 文件管理两个纯函数的**真逻辑**门禁。
//
//  为什么不是 acorn：acorn 只证明"能被 ES 解析器解析"，不证明逻辑对。
//  check-js-syntax.mjs / check-js-undeclared.mjs 都放过"排序方向反了""速度窗口
//  没裁剪""数据不够也算一个假速度"这类问题 —— 界面全绿、用户看到的顺序/速度却是错的。
//
//  做法：直接把浏览器要 import 的那两个模块源码当 ES 模块导入（data: URL，绕开
//  package.json 的 CommonJS 默认），用同一份实现跑断言 —— 不存在"Go 侧重写一份、
//  浏览器跑另一份"的漂移可能。
//
//  用法：node tools/check-files-pure.mjs
//  退出码 0 通过，1 有断言失败，2 环境不对。
// ============================================================================
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const JS_DIR = join(here, '..', 'internal', 'web', 'assets', 'js');

async function importAsset(name) {
  const src = readFileSync(join(JS_DIR, name), 'utf8');
  const url = 'data:text/javascript;base64,' + Buffer.from(src, 'utf8').toString('base64');
  return import(url);
}

let bad = 0;
let total = 0;
function check(name, fn) {
  total++;
  try {
    fn();
    console.log('  ✓ ' + name);
  } catch (e) {
    bad++;
    console.error('  ✗ ' + name + '\n      ' + ((e && e.message) || e));
  }
}
function eq(got, want, what) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error((what || '值') + ' 应为 ' + w + '，实际 ' + g);
}
const names = (list) => list.map((x) => x.name);

const sortMod = await importAsset('sortfiles.js');
const rateMod = await importAsset('transfereta.js');
const { sortEntries, compareEntries, extOf, normalizeSortKey, normalizeSortDir } = sortMod;
const { pushSample, rateBps, remainingBytes, formatRate, formatEta, rateEtaText, createRateSampler } = rateMod;
if (typeof sortEntries !== 'function' || typeof rateEtaText !== 'function') {
  console.error('导出的纯函数缺失（改名了？门禁判据必须同步）');
  process.exit(2);
}

// ---------------------------------------------------------------- 排序 ----
console.log('\n== 排序（sortfiles.js）==');

const f = (name, extra = {}) => Object.assign({ name, is_dir: false, size: 0, mod_time: '' }, extra);
const d = (name) => ({ name, is_dir: true, size: 0, mod_time: '' });

check('extOf：小写扩展名；无扩展名/末尾点返回空', () => {
  eq(extOf('Movie.MKV'), '.mkv');
  eq(extOf('noext'), '');
  eq(extOf('.hidden'), '');
  eq(extOf('trailing.'), '');
});

check('默认名字升序（与旧行为一致）', () => {
  eq(names(sortEntries([f('b.txt'), f('a.txt'), f('c.txt')], 'name', 1)), ['a.txt', 'b.txt', 'c.txt']);
});

check('名字降序', () => {
  eq(names(sortEntries([f('a.txt'), f('c.txt'), f('b.txt')], 'name', -1)), ['c.txt', 'b.txt', 'a.txt']);
});

check('目录永远排在文件前面（升序与降序都不变）', () => {
  const list = [f('a.txt'), d('zzz'), f('m.txt'), d('aaa')];
  for (const dir of [1, -1]) {
    const got = sortEntries(list, 'name', dir);
    const firstFile = got.findIndex((x) => !x.is_dir);
    const lastDir = got.map((x) => x.is_dir).lastIndexOf(true);
    if (lastDir > firstFile) throw new Error('dir=' + dir + ' 时目录没有全部排在文件前：' + names(got));
  }
});

check('类型升序：按扩展名，无扩展名永远垫底', () => {
  const list = [f('noext'), f('b.zip'), f('a.mkv'), f('c.avi')];
  eq(names(sortEntries(list, 'type', 1)), ['c.avi', 'a.mkv', 'b.zip', 'noext']);
});

check('类型降序：扩展名反向，但无扩展名仍垫底', () => {
  const list = [f('noext'), f('b.zip'), f('a.mkv'), f('c.avi')];
  eq(names(sortEntries(list, 'type', -1)), ['b.zip', 'a.mkv', 'c.avi', 'noext']);
});

check('时间：按 mod_time，降序 = 新的在前', () => {
  const list = [
    f('old.mkv', { mod_time: '2024-01-01 00:00:00' }),
    f('new.mkv', { mod_time: '2026-01-01 00:00:00' }),
    f('mid.mkv', { mod_time: '2025-01-01 00:00:00' }),
  ];
  eq(names(sortEntries(list, 'time', -1)), ['new.mkv', 'mid.mkv', 'old.mkv']);
  eq(names(sortEntries(list, 'time', 1)), ['old.mkv', 'mid.mkv', 'new.mkv']);
});

check('大小：降序 = 大的在前（缺失 size 当 0）', () => {
  const list = [f('s.mkv', { size: 10 }), f('l.mkv', { size: 1000 }), f('z.mkv')];
  eq(names(sortEntries(list, 'size', -1)), ['l.mkv', 's.mkv', 'z.mkv']);
});

check('同键相等时按名字升序兜底（顺序稳定）', () => {
  const list = [f('b', { size: 5 }), f('a', { size: 5 }), f('c', { size: 5 })];
  eq(names(sortEntries(list, 'size', -1)), ['a', 'b', 'c']);
});

check('sortEntries 不改动入参', () => {
  const list = [f('b.txt'), f('a.txt')];
  sortEntries(list, 'name', 1);
  eq(names(list), ['b.txt', 'a.txt']);
});

check('normalizeSortKey/Dir 收敛非法输入', () => {
  eq(normalizeSortKey('bogus'), 'name');
  eq(normalizeSortKey('time'), 'time');
  eq(normalizeSortDir(-3), -1);
  eq(normalizeSortDir(undefined), 1);
  eq(normalizeSortDir('1'), 1);
});

check('compareEntries 是唯一比较器：符号与 sort 语义一致', () => {
  if (!(compareEntries(f('a'), f('b'), 'name', 1) < 0)) throw new Error('a<b 应为负');
  if (!(compareEntries(f('b'), f('a'), 'name', 1) > 0)) throw new Error('b>a 应为正');
  eq(compareEntries(f('a'), f('a'), 'name', 1), 0);
});

// ------------------------------------------------------- 速度 / ETA ----
console.log('\n== 速度 / 预计剩余（transfereta.js）==');

check('rateBps：1 秒推进 1000 万字节 ⇒ 10 MB/s', () => {
  const s = pushSample(pushSample([], 1000, 0), 2000, 10000000);
  eq(rateBps(s), 10000000);
});

check('数据不够：单点 / 跨度 < 0.25s ⇒ 0（绝不瞎猜）', () => {
  eq(rateBps(pushSample([], 0, 0)), 0);
  eq(rateBps(pushSample(pushSample([], 0, 0), 100, 5000000)), 0);
});

check('滑动窗口：只取最近 3 秒，旧样本被丢掉（不能拿全程平均冒充实时）', () => {
  let s = [];
  s = pushSample(s, 0, 0, 3000);
  s = pushSample(s, 1000, 10000000, 3000); // 头 1 秒很快（10 MB/s）
  s = pushSample(s, 4000, 13000000, 3000); // 最后 3 秒只走 3MB（1 MB/s）
  eq(s.length, 2, '窗口内应只剩窗口内的 2 个点');
  eq(rateBps(s), 1000000, '窗口速度（全程平均是 3.25 MB/s，那是骗人的）');
});

check('停住（字节不再涨）⇒ 速度归零，不显示假速度', () => {
  let s = [];
  s = pushSample(s, 0, 5000000);
  s = pushSample(s, 1000, 10000000);
  s = pushSample(s, 3000, 10000000); // 2 秒没动
  s = pushSample(s, 4000, 10000000);
  eq(rateBps(s), 0);
});

check('字节回退（SSE 重放）不让速度变负', () => {
  const s = pushSample(pushSample([], 0, 10000000), 1000, 4000000);
  if (rateBps(s) < 0) throw new Error('速度不能为负');
});

check('formatRate / formatEta 文案', () => {
  eq(formatRate(500), '500 B/s');
  eq(formatRate(1024), '1.0 KB/s');
  eq(formatRate(12.3 * 1048576), '12.3 MB/s');
  eq(formatEta(12), '12 秒');
  eq(formatEta(130), '2 分 10 秒');
  eq(formatEta(3900), '1 小时 5 分');
});

check('rateEtaText：12.3 MB/s · 约剩 2 分 10 秒（用户点名的样子）', () => {
  const bps = 12.3 * 1048576;
  eq(rateEtaText(bps, Math.ceil(bps * 130)), '12.3 MB/s · 约剩 2 分 10 秒');
});

check('rateEtaText：速度未知 ⇒ 计算中…；总数未知 ⇒ 只给速度', () => {
  eq(rateEtaText(0, 1000), '计算中…');
  eq(rateEtaText(1048576, -1), '1.0 MB/s');
  eq(rateEtaText(1048576, 0), '1.0 MB/s');
});

check('remainingBytes：总数未知返回 -1（算不出 ETA）', () => {
  eq(remainingBytes(10, 0), -1);
  eq(remainingBytes(10, 100), 90);
  eq(remainingBytes(120, 100), 0);
});

check('createRateSampler：note/text 与纯函数一致', () => {
  const m = createRateSampler();
  m.note(0, 1000);
  const bps = m.note(10000000, 2000);
  eq(bps, 10000000);
  eq(m.text(20000000), '9.5 MB/s · 约剩 2 秒');
  m.reset();
  eq(m.text(1000), '计算中…');
});

if (bad > 0) {
  console.error(`\n❌ ${bad}/${total} 条纯函数断言失败（排序或速度/ETA 逻辑被改坏了）`);
  process.exit(1);
}
console.log(`\n✅ 纯函数门禁通过（${total} 条断言：排序 + 速度/ETA）`);
