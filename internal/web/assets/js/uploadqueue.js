// uploadqueue.js —— 上传队列的**唯一**数据结构（入队 / 逐文件进度 / 完成 / 失败 /
// 取消 / 总计与速度、剩余时间）。
//
// 为什么单独一个模块：面板要**逐文件**成行显示进度，每个文件的状态就只能有一处；
// 分散在渲染分支与请求回调里，必然出现"列表和总计打架"。而且面板要跨页面/跨目录
// 存活（用户要求），队列也必须是模块级单例，不能挂在某一次页面重建的闭包里。
//
// 这里不碰 DOM、不发请求：传输由调用方注入的 send(item, hooks) 决定，
// 所以同一份队列被按钮/拖拽/面板共用（见 files.js 的 uploadEntries）。

// UPLOAD_CONCURRENCY 是同时上传的文件数，取 2 的理由：
//   · 面板是单进程 Go，每个请求要完整收进 multipart 临时文件，并发越高内存与
//     读超时风险越大；单个大文件本来就吃满带宽。
//   · 1 个并发时，一个慢文件会让整条队列干等；2 个在"大文件 + 小文件"混合时
//     既有前进感，又不会让所有进度条一起慢慢爬（宝塔也是 1~2 路）。
export const UPLOAD_CONCURRENCY = 2;

// 一个文件在队列里的状态。文案不在这里（渲染层负责翻译成中文）。
export const UPLOAD_STATUS = {
  WAITING: 'waiting',
  UPLOADING: 'uploading',
  DONE: 'done',
  FAILED: 'failed',
  CANCELED: 'canceled',
};

// SPEED_WINDOW_MS：实时速度只取最近这段窗口 —— 用全程平均会让"已经卡住"显示成
// 仍在高速，剩余时间跟着骗人。NOTIFY_MS：进度事件很密，渲染节流到约 6~7 次/秒。
const SPEED_WINDOW_MS = 3000;
const NOTIFY_MS = 150;

let nextId = 1;

// createUploadQueue 是队列的**唯一**定义（函数级门禁会数它只出现一次）。
//
// send(item, { onProgress, registerAbort }) 必须返回 Promise：
//   · 成功 → { ok: true, savedName?, overwritten? }
//   · 失败 → { ok: false, error }
//   · 取消 → { ok: false, aborted: true }
export function createUploadQueue({ send, concurrency = UPLOAD_CONCURRENCY } = {}) {
  if (typeof send !== 'function') throw new TypeError('createUploadQueue 需要 send(item, hooks)');
  const limit = Math.max(1, Number(concurrency) || UPLOAD_CONCURRENCY);

  const items = [];
  const listeners = new Set();
  const idleHandlers = new Set();

  let limitBytes = 0;   // ≤0 = 上限未复核：不拦，交给服务端判
  let limitText = '';
  let activeCount = 0;
  let wasBusy = false;
  let lastNotify = 0;
  let notifyTimer = null;

  function find(id) { return items.find((it) => it.id === id) || null; }

  function fireIdle() {
    for (const fn of [...idleHandlers]) {
      try { fn(); } catch (e) { console.warn('upload queue idle handler failed', e); }
    }
  }

  // maybeIdle 只在"忙 → 闲"的那一次跳变上报一次，避免每个文件结束都弹提示。
  function maybeIdle() {
    const busy = activeCount > 0 || items.some((it) => it.status === UPLOAD_STATUS.WAITING);
    if (busy) { wasBusy = true; return; }
    if (wasBusy) { wasBusy = false; fireIdle(); }
  }

  function notify(force) {
    const now = Date.now();
    if (force || now - lastNotify >= NOTIFY_MS) {
      lastNotify = now;
      if (notifyTimer) { clearTimeout(notifyTimer); notifyTimer = null; }
      for (const fn of [...listeners]) {
        try { fn(); } catch (e) { console.warn('upload queue listener failed', e); }
      }
      return;
    }
    if (!notifyTimer) {
      notifyTimer = setTimeout(() => { notifyTimer = null; notify(true); }, NOTIFY_MS - (now - lastNotify));
    }
  }

  // noteProgress 用滑动窗口算实时速度：只统计最近 SPEED_WINDOW_MS 内到达的字节。
  function noteProgress(it, sent) {
    const now = Date.now();
    if (sent > it.sent) it.sent = sent;
    it.samples.push([now, it.sent]);
    while (it.samples.length > 2 && now - it.samples[0][0] > SPEED_WINDOW_MS) it.samples.shift();
    const [t0, s0] = it.samples[0];
    const dt = (now - t0) / 1000;
    if (dt >= 0.25) {
      it.bps = Math.max(0, (it.sent - s0) / dt);
    } else if (it.startedAt) {
      it.bps = it.sent / Math.max(0.25, (now - it.startedAt) / 1000);
    }
  }

  function oversizeError(size) {
    const cap = limitText || (limitBytes + ' 字节');
    // 出路写在 title 里（细节不进一句话文案），由渲染层读出 oversizeAdvice。
    return `超过单文件上限 ${cap}（这个文件 ${size} 字节）`;
  }

  // markOversize：超限的文件**一个字节都不发**，就地标失败（用户不用白等一次 413）。
  // oversize=true 让"重试"不出现 —— 不改面板上限的话重试必然再失败一次。
  function markOversize(it) {
    it.status = UPLOAD_STATUS.FAILED;
    it.oversize = true;
    it.error = oversizeError(it.size);
  }

  // add 入队：**立刻**返回条目（渲染层马上就能成行），此时还没发任何请求。
  function add(entries, { dir = '', folder = false, onConflict = '' } = {}) {
    const added = [];
    for (const e of entries || []) {
      const file = (e && e.file) || e;
      if (!file) continue;
      const rel = (e && e.rel) || file.name || '';
      const size = Number(file.size) || 0;
      const it = {
        id: nextId++, file, rel, name: rel.split('/').pop() || file.name || '', size,
        dir, folder, onConflict,
        status: UPLOAD_STATUS.WAITING, sent: 0, bps: 0, error: '',
        savedName: '', overwritten: false, oversize: false,
        startedAt: 0, samples: [], token: null, abort: null,
      };
      if (limitBytes > 0 && size > limitBytes) markOversize(it);
      items.push(it);
      added.push(it);
    }
    notify(true);
    return added;
  }

  // setLimit 回读上限后调用：对**还没发出去**的超限项补标失败；上限变大时把本地
  // 预检误拦的项放回队列（真机踩过：页面缓存旧上限，服务端其实允许）。
  function setLimit(info) {
    const next = Math.max(0, Number(info && info.bytes) || 0);
    const text = (info && (info.text || info.sizeText || info.limit_text)) || '';
    const grew = next > limitBytes;
    limitBytes = next;
    limitText = text;
    if (limitBytes > 0) {
      for (const it of items) {
        if (it.status === UPLOAD_STATUS.WAITING && it.size > limitBytes) {
          markOversize(it);
        } else if (grew && it.status === UPLOAD_STATUS.FAILED && it.oversize && it.size <= limitBytes) {
          // 上限已放宽：之前的"超限"结论不再成立，按未发送处理，等服务端判。
          it.oversize = false;
          it.error = '';
          it.status = UPLOAD_STATUS.WAITING;
        }
      }
    }
    notify(true);
  }

  // setConflict 把"同名怎么办"的选择应用到所有还没发出去的条目。
  function setConflict(choice) {
    for (const it of items) {
      if (it.status === UPLOAD_STATUS.WAITING && !it.onConflict) it.onConflict = choice || '';
    }
    notify(true);
  }

  function settle(it, token, res) {
    if (it.token !== token) return; // 已被取消或重试用新 token 接管，旧结果作废
    it.token = null;
    it.abort = null;
    activeCount = Math.max(0, activeCount - 1);
    it.bps = 0;
    if (res && res.ok) {
      it.status = UPLOAD_STATUS.DONE;
      it.sent = it.size;
      it.savedName = res.savedName || it.name;
      it.overwritten = !!res.overwritten;
      it.error = '';
    } else if (res && res.aborted) {
      it.status = UPLOAD_STATUS.CANCELED;
      it.error = '';
    } else {
      it.status = UPLOAD_STATUS.FAILED;
      it.error = (res && res.error) || '未知错误';
    }
    notify(true);
    pump();
    maybeIdle();
  }

  function startItem(it) {
    const token = {};
    it.token = token;
    it.status = UPLOAD_STATUS.UPLOADING;
    it.startedAt = Date.now();
    it.sent = 0;
    it.bps = 0;
    it.error = '';
    it.samples = [];
    activeCount++;
    notify(true);
    Promise.resolve()
      .then(() => send(it, {
        onProgress: (sent) => {
          if (it.token !== token) return;
          noteProgress(it, Number(sent) || 0);
          notify(false);
        },
        registerAbort: (fn) => { if (it.token === token) it.abort = fn; },
      }))
      .then((res) => settle(it, token, res))
      .catch((err) => settle(it, token, { ok: false, error: (err && err.message) || String(err) }));
  }

  // pump 按并发上限补位。**只在这里决定"下一个发谁"**，别处不得自己发请求。
  function pump() {
    while (activeCount < limit) {
      const next = items.find((it) => it.status === UPLOAD_STATUS.WAITING);
      if (!next) break;
      startItem(next);
    }
  }

  function start() { pump(); maybeIdle(); notify(true); }

  function cancel(id) {
    const it = find(id);
    if (!it) return false;
    if (it.status === UPLOAD_STATUS.WAITING) {
      it.status = UPLOAD_STATUS.CANCELED;
      it.bps = 0;
      notify(true);
      maybeIdle();
      return true;
    }
    if (it.status === UPLOAD_STATUS.UPLOADING) {
      it.token = null; // 旧 send 的 settle 作废
      const abort = it.abort;
      it.abort = null;
      activeCount = Math.max(0, activeCount - 1);
      it.status = UPLOAD_STATUS.CANCELED;
      it.bps = 0;
      if (abort) { try { abort(); } catch { /* 已经结束 */ } }
      notify(true);
      pump();
      maybeIdle();
      return true;
    }
    return false;
  }

  function cancelAll() {
    let n = 0;
    for (const it of [...items]) { if (cancel(it.id)) n++; }
    return n;
  }

  function retry(id) {
    const it = find(id);
    if (!it || it.oversize) return false;
    if (it.status !== UPLOAD_STATUS.FAILED && it.status !== UPLOAD_STATUS.CANCELED) return false;
    it.status = UPLOAD_STATUS.WAITING;
    it.sent = 0;
    it.bps = 0;
    it.error = '';
    it.samples = [];
    notify(true);
    pump();
    maybeIdle();
    return true;
  }

  // retryAllFailed 一键重试全部失败项（超限项跳过：重试必然再失败）。
  function retryAllFailed() {
    let n = 0;
    for (const it of items) {
      if (it.status === UPLOAD_STATUS.FAILED && !it.oversize) {
        it.status = UPLOAD_STATUS.WAITING; it.sent = 0; it.bps = 0; it.error = ''; it.samples = []; n++;
      }
    }
    notify(true);
    pump();
    maybeIdle();
    return n;
  }

  function remove(id) {
    const it = find(id);
    if (!it || it.status === UPLOAD_STATUS.UPLOADING) return false;
    const i = items.indexOf(it);
    if (i >= 0) items.splice(i, 1);
    notify(true);
    return true;
  }

  // clearFinished 清掉已完成/失败/已取消/已移除的行，上传中的一个都不动。
  function clearFinished() {
    let n = 0;
    for (let i = items.length - 1; i >= 0; i--) {
      const st = items[i].status;
      if (st === UPLOAD_STATUS.DONE || st === UPLOAD_STATUS.FAILED || st === UPLOAD_STATUS.CANCELED) {
        items.splice(i, 1);
        n++;
      }
    }
    notify(true);
    return n;
  }

  // totals 是"总计区"的唯一数据来源（渲染层不许自己累加）。
  function totals() {
    let count = items.length, done = 0, failed = 0, canceled = 0, waiting = 0, uploading = 0;
    let sent = 0, total = 0, bps = 0;
    for (const it of items) {
      sent += it.sent;
      total += it.size;
      if (it.status === UPLOAD_STATUS.DONE) done++;
      else if (it.status === UPLOAD_STATUS.FAILED) failed++;
      else if (it.status === UPLOAD_STATUS.CANCELED) canceled++;
      else if (it.status === UPLOAD_STATUS.UPLOADING) { uploading++; bps += it.bps || 0; }
      else waiting++;
    }
    const remaining = Math.max(0, total - sent);
    const percent = total > 0
      ? Math.min(100, (sent / total) * 100)
      : (count > 0 && done === count ? 100 : 0);
    return {
      count, done, failed, canceled, waiting, uploading, sent, total, bps, remaining, percent,
      eta: bps > 0 && remaining > 0 ? remaining / bps : 0,
      busy: uploading > 0 || waiting > 0,
    };
  }

  function subscribe(fn) {
    if (typeof fn !== 'function') return () => {};
    listeners.add(fn);
    return () => listeners.delete(fn);
  }

  function onIdle(fn) {
    if (typeof fn !== 'function') return () => {};
    idleHandlers.add(fn);
    return () => idleHandlers.delete(fn);
  }

  return {
    items, add, setLimit, setConflict, start, cancel, cancelAll, retry, retryAllFailed,
    remove, clearFinished, totals, subscribe, onIdle,
    concurrency: limit,
  };
}
