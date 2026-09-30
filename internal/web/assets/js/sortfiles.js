// sortfiles.js —— 文件列表排序的**纯函数**（唯一比较实现）。
//
// 为什么单独一个模块：排序以前写死在 files.js 的 sortedEntries 里，
// 而 files.js 依赖 DOM，node 跑不起来 ⇒ 比较逻辑没有任何能失败的门禁。
// 这里只做数据进/数据出（不碰 DOM、不读 localStorage、不发请求），
// 浏览器 import 它，tools/check-files-pure.mjs 也用同一份源码跑断言。
//
// 规则（用户点名）：
//   · 目录永远排在文件前面（与升降序无关）；
//   · 键：name / type / time / size —— time 用 mod_time，type 用扩展名，size 用 size；
//   · type 键下"无扩展名"永远垫底（也与升降序无关）；
//   · 同键相等时按名字升序兜底，保证顺序稳定；
//   · 默认 name 升序（与旧行为一致）。

// SORT_KEYS 是允许的键（顺序 = 工具栏菜单顺序）。
export const SORT_KEYS = ['name', 'type', 'time', 'size'];

// normalizeSortKey 把外部输入（localStorage/URL）收敛到合法键，非法就回落 name。
export function normalizeSortKey(key) {
  return SORT_KEYS.includes(key) ? key : 'name';
}

// normalizeSortDir 只认 -1（降序）；其余（含 undefined）都是升序 1。
export function normalizeSortDir(dir) {
  return Number(dir) < 0 ? -1 : 1;
}

// extOf 返回小写扩展名（含点）；没有扩展名（或只有开头的点）返回 ''。
export function extOf(name) {
  const n = String(name || '');
  const i = n.lastIndexOf('.');
  if (i <= 0 || i === n.length - 1) return '';
  return n.slice(i).toLowerCase();
}

// nameCompare 用中文排序（与旧行为一致），保证同键时顺序稳定。
function nameCompare(a, b) {
  return String(a.name || '').localeCompare(String(b.name || ''), 'zh');
}

// compareEntries 是**唯一**的比较器：返回 <0 / 0 / >0（sort 语义）。
export function compareEntries(a, b, key, dir) {
  // 目录永远在前面：升降序都不翻转这条。
  if (!!a.is_dir !== !!b.is_dir) return a.is_dir ? -1 : 1;

  const k = normalizeSortKey(key);
  const d = normalizeSortDir(dir);

  if (k === 'type') {
    const ae = extOf(a.name);
    const be = extOf(b.name);
    // 无扩展名永远垫底（不受方向影响）。
    if (!ae !== !be) return ae ? -1 : 1;
    let r = 0;
    if (ae < be) r = -1;
    else if (ae > be) r = 1;
    if (r !== 0) return r * d;
    return nameCompare(a, b);
  }

  let r = 0;
  if (k === 'size') r = (Number(a.size) || 0) - (Number(b.size) || 0);
  else if (k === 'time') r = String(a.mod_time || '').localeCompare(String(b.mod_time || ''));
  else r = nameCompare(a, b);
  // 主键相等时按名字升序兜底（不受方向影响），保证顺序稳定、可复现。
  if (r !== 0) return r * d;
  return nameCompare(a, b);
}

// sortEntries 返回排好序的**新数组**（不改调用方的 list）。
export function sortEntries(list, key, dir) {
  return (Array.isArray(list) ? list.slice() : []).sort((a, b) => compareEntries(a, b, key, dir));
}
