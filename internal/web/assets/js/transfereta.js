// transfereta.js —— 传输速度 / 预计剩余的**纯函数**（唯一实现）。
//
// 为什么单独一个模块：上传队列的 noteProgress 里曾有一套滑窗算法，而复制/移动/
// 删除/去广告这些**文件操作**的进度从来没有速度显示（用户报障：跨盘、尤其和 SMB
// 网络盘之间耗时很久，看不到速度）。两处各写一套必然漂移，所以抽到这里：
// 浏览器 import 它（files.js / tasks.js），tools/check-files-pure.mjs 也用同一份
// 源码跑断言 —— 把窗口逻辑改坏，门禁必须变红。
//
// 这里不碰 DOM、不发请求、不读时钟（now 由调用方传入），所以可被判据完全复现。

// RATE_WINDOW_MS：实时速度只取最近这段窗口。用全程平均会让"已经卡住"显示成
// 仍在高速，剩余时间跟着骗人。
export const RATE_WINDOW_MS = 3000;

// MIN_SPAN_SEC：窗口跨度小于它就算"数据不够"，返回 0（宁可不显示，也不瞎猜）。
export const MIN_SPAN_SEC = 0.25;

/**
 * pushSample 往采样序列追加一条 [时间ms, 字节]，只保留窗口内的点。
 * 纯函数：返回新数组，不改入参。
 *   · 字节单调不减（SSE 重放/回退时不让速度变成负数）；
 *   · 至少保留 2 个点（算速度需要首尾跨度）。
 */
export function pushSample(samples, now, bytes, windowMs = RATE_WINDOW_MS) {
  const arr = (Array.isArray(samples) ? samples : []).slice();
  const t = Number(now) || 0;
  const b = Math.max(0, Number(bytes) || 0);
  const last = arr.length ? arr[arr.length - 1] : null;
  arr.push([t, last && b < last[1] ? last[1] : b]);
  const win = Number(windowMs) > 0 ? Number(windowMs) : RATE_WINDOW_MS;
  while (arr.length > 2 && t - arr[0][0] > win) arr.shift();
  return arr;
}

/**
 * rateBps 用窗口内首尾两个采样算实时速度（字节/秒）。
 * 跨度不足 MIN_SPAN_SEC 返回 0 —— "计算中"比一个假数字安全。
 */
export function rateBps(samples, minSpanSec = MIN_SPAN_SEC) {
  if (!Array.isArray(samples) || samples.length < 2) return 0;
  const [t0, s0] = samples[0];
  const [t1, s1] = samples[samples.length - 1];
  const dt = (t1 - t0) / 1000;
  if (!(dt >= minSpanSec)) return 0;
  return Math.max(0, (s1 - s0) / dt);
}

/** remainingBytes 返回剩余字节；总数未知（<=0）时返回 -1（= 算不出 ETA）。 */
export function remainingBytes(done, total) {
  const t = Number(total) || 0;
  if (!(t > 0)) return -1;
  return Math.max(0, t - (Number(done) || 0));
}

/** formatRate 把字节/秒写成 "12.3 MB/s" 这类人话。 */
export function formatRate(bps) {
  const v = Number(bps) || 0;
  if (v < 1024) return v.toFixed(0) + ' B/s';
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + ' KB/s';
  if (v < 1024 * 1024 * 1024) return (v / 1048576).toFixed(1) + ' MB/s';
  return (v / 1073741824).toFixed(2) + ' GB/s';
}

/** formatEta 把秒数写成 "2 分 10 秒" / "1 小时 5 分" / "12 秒"。 */
export function formatEta(sec) {
  const s = Math.max(0, Math.floor(Number(sec) || 0));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return m > 0 ? `${h} 小时 ${m} 分` : `${h} 小时`;
  if (m > 0) return `${m} 分 ${s % 60} 秒`;
  return `${s} 秒`;
}

/**
 * rateEtaText 出"速度 · 剩余"这一句。
 *   · 速度算不出（数据不够 / 停住）⇒ "计算中…"，绝不编数字；
 *   · 总数未知 ⇒ 只给速度，不给剩余时间。
 */
export function rateEtaText(bps, remaining) {
  if (!(Number(bps) > 0)) return '计算中…';
  const speed = formatRate(bps);
  const rem = Number(remaining);
  if (!(rem > 0)) return speed;
  return speed + ' · 约剩 ' + formatEta(rem / Number(bps));
}

/**
 * createRateSampler 是给"每个任务一个采样器"用的小工厂（状态封装在闭包里）。
 * note() 返回当前速度；text() 返回可直接显示的文案。
 */
export function createRateSampler(opts = {}) {
  const windowMs = Number(opts.windowMs) > 0 ? Number(opts.windowMs) : RATE_WINDOW_MS;
  const minSpanSec = Number(opts.minSpanSec) > 0 ? Number(opts.minSpanSec) : MIN_SPAN_SEC;
  let samples = [];
  return {
    note(bytes, now = Date.now()) {
      samples = pushSample(samples, now, bytes, windowMs);
      return rateBps(samples, minSpanSec);
    },
    bps() { return rateBps(samples, minSpanSec); },
    text(remaining) { return rateEtaText(rateBps(samples, minSpanSec), remaining); },
    reset() { samples = []; },
    samples() { return samples.slice(); },
  };
}
