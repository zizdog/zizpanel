// uploadplan.js —— 上传分批的**纯函数**（不碰 DOM、不发请求）。
//
// 判据只有两条（用户报障后定的）：
//   1. 单个文件 > 上限 → 拒绝（必须点名文件）；
//   2. 整批总大小**不参与拒绝**：超过上限只是"多分几批"。
//
// 放在单独模块里是为了能被 node 直接跑门禁
// （见 internal/web/upload_batch_gate_test.go）—— 依赖 DOM 的 files.js 无法单测。

// MULTIPART_RESERVE_MAX 是每批给 multipart 边界/头预留的固定余量上限（1 MiB）。
const MULTIPART_RESERVE_MAX = 1 << 20;
// MULTIPART_RESERVE_RATIO 是按比例预留的余量（上限的 1/20）。
const MULTIPART_RESERVE_RATIO = 20;
// PER_PART_OVERHEAD 是每个文件在 multipart 里的头/边界开销（保守取 1 KiB）。
const PER_PART_OVERHEAD = 1024;

// uploadReserve 返回一批要预留的 multipart 开销余量。
export function uploadReserve(limitBytes) {
  const l = Number(limitBytes) || 0;
  if (l <= 0) return 0;
  return Math.min(Math.floor(l / MULTIPART_RESERVE_RATIO), MULTIPART_RESERVE_MAX);
}

// planUploadBatches 把一批文件按"每次请求 ≤ 上限"装批。
//
// sizes 是每个文件的字节数；分批必须**连续**（不打乱顺序）：
// 「上传文件夹」的相对路径与文件一一对应，顺序错了会把文件写进错目录。
//
// 返回 { batches: [[i,...], ...], oversize: [i,...], budget }：
//   · oversize = 单个文件就超过上限的索引（调用方必须点名拒绝、不做任何上传）；
//   · 其余文件全部进 batches，**总大小多大都不拒绝**；
//   · 每批 sum(size) ≤ 上限（多文件批留余量；单个文件贴近上限时独占一批）。
export function planUploadBatches(sizes, limitBytes) {
  const limit = Number(limitBytes) || 0;
  const list = Array.isArray(sizes) ? sizes : [];
  // 上限未知（≤0）时不假装能分批：全部进一批，交给服务端判（前端如实说"未复核"）。
  if (limit <= 0) {
    return { batches: list.length ? [list.map((_, i) => i)] : [], oversize: [], budget: 0 };
  }
  const budget = Math.max(1, limit - uploadReserve(limit));
  const batches = [];
  const oversize = [];
  let cur = [];
  let curBytes = 0;
  const flush = () => {
    if (cur.length) { batches.push(cur); cur = []; curBytes = 0; }
  };
  for (let i = 0; i < list.length; i++) {
    const s = Number(list[i]) || 0;
    if (s > limit) { oversize.push(i); continue; }
    // 已经装了东西，再加就超预算 → 先把这一批发出去（保证每批 ≤ 上限）。
    if (cur.length && curBytes + s + PER_PART_OVERHEAD > budget) flush();
    cur.push(i);
    curBytes += s + PER_PART_OVERHEAD;
  }
  flush();
  return { batches, oversize, budget };
}

// oversizeAdvice 是"单个文件太大"时给用户的出路。
//
// ⚠️ 绝不许再提"再用一次上传功能"（例如"用上传文件夹分批"）：用户刚用的就是它，
// 那种建议自相矛盾且必然失败 —— 正是这次报障的形态。
export function oversizeAdvice() {
  return [
    '在本机分卷压缩后分次上传',
    '用命令行（终端）直接放进站点目录',
  ];
}
