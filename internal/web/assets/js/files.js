// files.js —— 文件管理页面。
//
// 交互设计：
//   - 面包屑 + 目录树式导航；每行显示权限/属主/大小/时间
//   - 双击目录进入；双击文件按类型打开：图片→查看器、音视频→播放器、其余→编辑器
//   - 多选 + 批量操作（删除/压缩）；上传支持拖拽与多选
//   - 所有破坏性操作都要二次确认，删除目录必须显式勾选"递归"

import { api, apiURL } from './api.js';
import { taskCenter } from './tasks.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll, esc, rate, duration } from './ui.js';
import { registerCleanup, gotoSMBDisks } from './app.js';
// 上传分批的**纯函数**放单独模块：它可以被 node 直接跑门禁
// （见 internal/web/upload_batch_gate_test.go），而本文件依赖 DOM 无法单测。
import { oversizeAdvice } from './uploadlimit.js';
// 上传队列的**唯一**数据结构（入队/逐文件进度/总计）。本文件只负责把它画出来。
import { createUploadQueue, UPLOAD_STATUS } from './uploadqueue.js';
// 列表排序的**唯一**比较实现：纯函数放在单独模块，node 门禁能直接断言它
// （见 tools/check-files-pure.mjs）；这里只负责把结果与用户选择接上去。
import { sortEntries, normalizeSortKey, normalizeSortDir, extOf } from './sortfiles.js';
// 「改上限」小窗复用设置页同一份实现（panellimit.js）：同一处校验、同一处回读，
// 绝不在这里再写一套保存逻辑。
import { openPanelLimitEditor } from './panellimit.js';
// 「约剩」的速度/剩余口径**只有一份**（transfereta.js）：样本不足或总数未知一律
// 不显示（宁可不给，也不编一个假时间）。视频计划进度条复用它，不另写一套。
import { pushSample, rateBps, formatEta } from './transfereta.js';

// 面板单次上传上限**从前端写死改成回读**：GET /api/v1/files/upload-limit。
//
// 为什么不能写死（用户报障的根因之一）：提示里的数字必须来自真实生效的上限，
// 而这个上限是可配的（config.json → panel_upload_limit）。写死的 4 GiB 一旦
// 与配置漂移，提示就在说谎；旧实现还拿它去比**整批总大小**，于是 4.16 GB 的
// 文件夹被拒、却建议用户用同一个功能分流 —— 自相矛盾。现在：
//   · 只有单个文件 > 上限才拒绝（点名文件）；
//   · 总大小任意大，都按"每次请求 ≤ 上限"自动分批。

// 显示隐藏文件的状态记在 localStorage（刷新/切换板块后保持）。默认关闭。
const SHOW_HIDDEN_KEY = 'zp-files-show-hidden';

// 列表排序偏好也记 localStorage（"name:1" = 名字升序；刷新后保持）。
const SORT_PREF_KEY = 'zp-files-sort';

// readSortPref 把存的偏好收敛成合法值（坏了就回落名字升序，绝不报错）。
function readSortPref() {
  const parts = String(readLS(SORT_PREF_KEY) || '').split(':');
  return { key: normalizeSortKey(parts[0]), dir: normalizeSortDir(Number(parts[1])) };
}

let cwd = '';
let showHidden = readLS(SHOW_HIDDEN_KEY) === '1';
let selection = new Set();
let lastList = null;
// selAnchor 是 Shift 范围选择在**当前可见顺序**里的锚点索引。
let selAnchor = -1;
// visibleEntries 是最近一次渲染的可见条目顺序（键盘/范围选择都用它）。
let visibleEntries = [];
// rowEls / headCheckbox 是最近一次渲染的 DOM 引用：选中态靠它们**原地**改，
// 不整表重绘 —— 重绘会让双击的第二次点击落到新节点上，dblclick 永不触发（坑：双击打不开）。
let rowEls = [];
let headCheckbox = null;
// clipboard = { mode: 'copy' | 'cut', paths: string[] }
// 只在内存里（不写 localStorage）：剪贴板内容是易失的，刷新页面后失效最不意外。
let clipboard = null;
// 目录收藏：**存在服务端**（settings KV），所以手机上打开面板也是同一份。
// null = 还没拉到（拉之前工具栏不显示"已收藏"状态，绝不猜）。
let favorites = null;
// 当前打开的编辑器窗口（同一时刻最多一个）。**全局存活**：切换路由时不清除，
// 只有用户点 ✕ / 菜单「关闭编辑器」才会被 dispose（见 createEditorWindow 的 onClosed）。
let activeEditor = null;

// 目录大小（宝塔式按需计算）的结果缓存：key = 目录路径。
// **只活在当前页面会话**（模块级 Map，刷新即失效）：重绘/切目录/切路由都复用，
// 绝不因为一次重绘就重算；点已显示的结果 = 强制重算（覆盖缓存）。
// 运行中的条目用它挡住重复请求。目录内容被改后缓存**不自动失效**，靠再点一次。
const dirSizeCache = new Map();

// ============================================================================
//  上传（唯一实现）
// ============================================================================
//
// 用户要求（宝塔式上传面板）：选中/拖入后**立刻**逐文件成行 —— 文件名（长名截断、
// title 给全名与相对路径）/大小/每文件进度与百分比/实时速度/状态（等待/上传中/
// 已完成/失败：原因/已取消），以及该行的取消/重试/移除；总计区给
// 「共 N 个文件 · 已完成 x · 失败 y」+ 总进度 + 总速度 + 约剩余时间。
//
// 规矩（用户两次报障后定的，继续有效）：
//   1. 判据只能是"单个文件 vs 上限"：总大小任意大都不拒绝（每文件一个请求）；
//   2. 绝不静默：超限先本地标失败并**点名文件**，一个字节都不发；
//   3. 必须能看出"在动"：XHR 的 upload.onprogress 给逐文件字节；
//   4. 失败必须给出**服务端返回的原因**。
//
// 所有状态只在 uploadqueue.js 的队列里（模块级单例）：本文件不自己算进度、不自己
// 维护"第几个文件"，只把队列画出来（见 mountUploadProgress / uploadRow）。
// 面板也因此是**模块级浮动面板**，切页面/切目录都不销毁（用户要求）。

let uploadLimitInfo = null;

// getUploadLimit 回读面板真实生效的单次上传上限。
//
// 读不到就**如实标"未复核"**，绝不写死一个数字（旧实现写死 4 GiB，
// 一旦配置漂移提示就在说谎）。未复核时不拦，交给服务端判。
async function getUploadLimit() {
  // 每次调用都**重新回读**：上限能改（设置页/小窗），也可能改了 config.json 后
  // 重启面板才生效 —— 缓存过的数字会让老页面拿旧值把文件误判成超限（真机踩过）。
  // 只有回读失败时才退回上一次**已复核**的值，避免一次网络抖动就丢掉拦超限的能力。
  let info;
  try {
    const r = await api.fileUploadLimit();
    const bytes = Number(r && r.limit_bytes) || 0;
    const sizeText = bytes > 0 ? humanSize(bytes) : '';
    info = {
      bytes,
      limit_bytes: bytes,
      text: sizeText,
      sizeText,
      source: (r && r.source) || '',
      verified: !!(r && r.verified),
      note: (r && r.note) || '',
    };
  } catch (e) {
    if (uploadLimitInfo && uploadLimitInfo.verified) return uploadLimitInfo;
    info = {
      bytes: 0, limit_bytes: 0, text: '', sizeText: '', source: '', verified: false,
      note: '读不到上限接口：' + ((e && e.message) || e),
    };
  }
  uploadLimitInfo = info;
  return info;
}

// applyPanelLimitView 把一次**服务端回读**的上限结果同时喂给上传面板那行与队列
// （队列的超限预检用同一份值），不整页刷新。
function applyPanelLimitView(p) {
  const n = Number(p && (p.limit_bytes != null ? p.limit_bytes : p.bytes)) || 0;
  const sizeText = n > 0 ? humanSize(n) : '';
  uploadLimitInfo = {
    bytes: n,
    limit_bytes: n,
    text: sizeText,
    sizeText,
    source: (p && p.source) || '',
    verified: !!(p && p.verified),
    note: (p && p.note) || '',
  };
  const q = ensureUploadQueue();
  q.setLimit(uploadLimitInfo);
  if (uploadPanelState && uploadPanelState.refresh) uploadPanelState.refresh();
}

// openPanelLimitFromFiles 是上传面板那行「改上限」的入口：先回读当前生效值，
// 再用两处共用的 panellimit.js 小窗改；保存后立刻把这一行刷成新的回读值。
async function openPanelLimitFromFiles() {
  let cur;
  try {
    cur = await api.fileUploadLimit();
  } catch (e) {
    toast('读不到当前上限：' + ((e && e.message) || e), 'err', 10000);
    return;
  }
  openPanelLimitEditor(cur, (fresh) => applyPanelLimitView(fresh));
}

// ---------- 队列单例 ----------

let uploadQueue = null;
// uploadTargetDir 是面板当前的目标目录（入队时逐个记进条目；面板标题显示"上传到 …"）。
let uploadTargetDir = '';
// uploadEnterDir 是"文件夹上传完成后的唯一顶层目录"（面板给一颗「进入」）。
let uploadEnterDir = '';
// fileListRefresh 由 FilesView 每次重建时重新绑定，让队列完成后刷新当前列表。
let fileListRefresh = () => {};

// ensureUploadQueue 是队列的**唯一**创建点（模块级单例：切页/切目录不丢）。
function ensureUploadQueue() {
  if (uploadQueue) return uploadQueue;
  uploadQueue = createUploadQueue({ send: sendUploadRequest });
  uploadQueue.onIdle(() => {
    const t = uploadQueue.totals();
    fileListRefresh();
    // 文件夹上传完成后，若所有相对路径同属一个顶层目录，给一颗「进入 xxx」。
    const rels = uploadQueue.items.filter((it) => it.rel.includes('/')).map((it) => it.rel);
    const top = singleTopDir(rels);
    const base = uploadQueue.items.length ? uploadQueue.items[0].dir : cwd;
    uploadEnterDir = top ? `${base}/${top}` : '';
    if (t.busy || !t.count) return;
    if (t.failed) toast(`已上传 ${t.done} 个，${t.failed} 个失败（列表里可逐条重试）`, 'warn', 15000);
    else if (t.done) toast(`已上传 ${t.done} 个文件`, 'ok');
  });
  return uploadQueue;
}

// singleTopDir 返回一组相对路径共同的唯一顶层目录（没有则返回 ''）。
function singleTopDir(rels) {
  if (!rels.length) return '';
  const tops = new Set(rels.map((r) => r.split('/')[0]));
  return tops.size === 1 ? [...tops][0] : '';
}

// sendUploadRequest 是队列唯一的传输实现（**每个文件一个请求**，才有逐文件进度）。
//
// 上传必须用 XHR：fetch 完全没有"上传进度"能力（只有下载流的 reader），
// 这正是旧版只剩一句干等 toast 的原因。XHR 的 upload.onprogress 才有已传字节。
function sendUploadRequest(item, { onProgress, registerAbort }) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    registerAbort(() => { try { xhr.abort(); } catch { /* 已经结束 */ } });
    xhr.open('POST', apiURL('files/upload'), true);
    xhr.withCredentials = true;
    xhr.setRequestHeader('X-CSRF-Token', readCookie('zp_csrf'));
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded); };
    xhr.onload = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch { data = null; }
      const okBody = xhr.status >= 200 && xhr.status < 300 && data && data.ok !== false;
      if (!okBody) {
        resolve({ ok: false, error: `HTTP ${xhr.status}${xhr.statusText ? ' ' + xhr.statusText : ''}：${serverReason(xhr, data)}` });
        return;
      }
      const payload = (data && data.data) || {};
      const up = (payload.uploaded || [])[0];
      if (!up) {
        const why = (payload.failed || [])[0];
        resolve({ ok: false, error: why ? String(why) : (payload.msg || '服务端没有确认这个文件已落盘') });
        return;
      }
      resolve({ ok: true, savedName: up.name || '', overwritten: !!up.overwritten, savedPath: up.path || '' });
    };
    xhr.onerror = () => resolve({ ok: false, error: '连接被中断（网络断开、面板重启，或被服务端在读请求时中断）' });
    xhr.ontimeout = () => resolve({ ok: false, error: '服务端在限定时间内没有收完数据' });
    xhr.onabort = () => resolve({ ok: false, aborted: true });
    let fd;
    try {
      fd = new FormData();
      fd.append('dir', item.dir);
      // 「上传文件夹」：带 relpaths（**一个文件也要带**，后端据此进入目录树模式）。
      if (item.folder) fd.append('relpaths', JSON.stringify([item.rel]));
      if (item.onConflict) fd.append('on_conflict', item.onConflict);
      fd.append('files', item.file, item.file.name);
    } catch (err) {
      resolve({ ok: false, error: '无法准备上传数据：' + ((err && err.message) || err) });
      return;
    }
    xhr.send(fd);
  });
}

// ---------- 隐藏的选择框（模块级：面板跨页面存活，选择框不能挂在某次视图上） ----------

let uploadFileInput = null;
let uploadFolderInput = null;

function ensureUploadInputs() {
  if (uploadFileInput && uploadFolderInput && uploadFileInput.isConnected) return;
  uploadFileInput = h('input', {
    type: 'file', multiple: true, style: { display: 'none' },
    onchange: async (e) => {
      const files = Array.from(e.target.files || []);
      e.target.value = '';
      if (files.length) await uploadEntries(files.map((f) => ({ file: f, rel: f.name })));
    },
  });
  // 「上传文件夹」入口：webkitdirectory 让浏览器把整棵目录树交出来，
  // 每个文件带 webkitRelativePath（如 mysite/assets/app.css）。
  // 用 setAttribute 而不是 h() 的属性赋值：webkitdirectory 是**布尔** IDL 属性，
  // 赋空字符串会被浏览器当成 false（"设了但没生效"），setAttribute 才稳。
  uploadFolderInput = h('input', {
    type: 'file', multiple: true, style: { display: 'none' },
    onchange: async (e) => {
      const files = Array.from(e.target.files || []);
      e.target.value = '';
      // 空文件夹浏览器不会给任何文件：这里必须**说出来**，否则"选了文件夹没反应"
      // 又是一次静默（取消选择不会触发 change，所以走到这里就是真的空）。
      if (!files.length) { toast('这个文件夹里没有文件（空文件夹不会有任何东西被上传）', 'warn', 8000); return; }
      await uploadEntries(files.map((f) => ({ file: f, rel: f.webkitRelativePath || f.name })), { folder: true });
    },
  });
  uploadFolderInput.setAttribute('webkitdirectory', '');
  uploadFolderInput.setAttribute('directory', '');
  appendAll(document.body, uploadFileInput, uploadFolderInput);
}

function pickUploadFiles() { ensureUploadInputs(); uploadFileInput.click(); }
function pickUploadFolder() { ensureUploadInputs(); uploadFolderInput.click(); }

// ---------- 拖拽（唯一入口） ----------

// wireDropZone 让"拖文件/拖文件夹进来"走与按钮完全相同的上传路径。
//
// 为什么要走同一条路：拖拽只是另一个入口，校验、进度、错误提示、相对路径
// 重建目录树的行为必须与「上传文件夹」按钮一模一样，否则会出现
// "按钮传没问题、拖进来就静默失败"这种最难查的差异。
// zone.show(on) 由调用方决定怎么表现：列表上的提示条是显隐，上传面板里是整体高亮；
// zone.dir() 给出这次拖拽的目标目录（切页后面板仍在，目标目录必须当场取）。
function wireDropZone(el, zone) {
  if (!zone) zone = {};
  const hasFiles = (e) => {
    const dt = e.dataTransfer;
    if (!dt) return false;
    return Array.from(dt.types || []).includes('Files');
  };
  let depth = 0; // dragenter/dragleave 会随子元素反复触发，用计数避免提示闪烁
  const show = (on) => { if (zone.show) zone.show(on); };
  const dir = () => (zone.dir ? zone.dir() : cwd);
  el.addEventListener('dragenter', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault(); depth++; show(true);
  });
  el.addEventListener('dragover', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });
  el.addEventListener('dragleave', (e) => {
    if (!hasFiles(e)) return;
    depth = Math.max(0, depth - 1);
    if (!depth) show(false);
  });
  el.addEventListener('drop', async (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault(); depth = 0; show(false);
    try {
      await handleDrop(e.dataTransfer, dir());
    } catch (err) {
      toast('处理拖入的内容失败：' + (err && err.message ? err.message : err), 'err', 12000);
    }
  });
}

// handleDrop 把 DataTransfer 展开成 {file, rel} 列表。
//
// 优先用 webkitGetAsEntry：只有它能拿到**目录**与相对路径。
// 它不可用时退化到 dataTransfer.files（拿不到目录结构，也不能拖文件夹），
// 这时只能按文件名上传，并且要如实告诉用户为什么没有目录。
async function handleDrop(dt, dir) {
  const items = dt.items ? Array.from(dt.items).filter((it) => it.kind === 'file') : [];
  const roots = items.map((it) => (it.webkitGetAsEntry ? it.webkitGetAsEntry() : null)).filter(Boolean);
  if (!roots.length) {
    const plain = Array.from(dt.files || []);
    if (!plain.length) { toast('没有识别到可上传的文件', 'warn'); return; }
    toast('当前浏览器不支持读取拖入的文件夹，已按普通文件上传（目录结构会丢失）', 'warn', 12000);
    await uploadEntries(plain.map((f) => ({ file: f, rel: f.name })), { dir });
    return;
  }
  const entries = [];
  let hasDir = false;
  for (const root of roots) {
    if (root.isDirectory) hasDir = true;
    await walkEntry(root, '', entries);
  }
  if (!entries.length) { toast('拖入的文件夹是空的，没有可上传的文件', 'warn'); return; }
  // 只有文件夹（或混着文件夹）时才带相对路径；纯文件拖拽与「选择文件」等价。
  await uploadEntries(entries, { folder: hasDir, dir });
}

// ---------- 浮动面板 + 逐文件列表 ----------

let uploadPanelState = null;   // { root, refresh, setMinimized, destroy, sync }

// uploadRow 是**唯一**的逐文件行渲染器（一行 = 一个文件）。
//
// 行内容：文件名 / 大小 / 操作 / 每文件进度条 / 百分比 · 速度 · 状态。
// 它只读队列条目，自己不存任何进度。
function uploadRow(item) {
  const nameEl = h('div.zp-up-name', { text: item.name, title: item.rel || item.name });
  const sizeEl = h('div.zp-up-size', { text: humanSize(item.size) });
  const actEl = h('div.zp-up-act');
  const fill = h('i', { style: { width: '0%' } });
  const bar = h('div.bar.zp-up-bar', [fill]);
  const pctEl = h('span.zp-up-pct');
  const speedEl = h('span.zp-up-speed');
  const statusEl = h('span.zp-up-status');
  const metaEl = h('div.zp-up-meta', [pctEl, speedEl, statusEl]);
  const row = h('div.zp-up-row', { dataset: { uploadId: String(item.id) } }, [nameEl, sizeEl, actEl, bar, metaEl]);

  function update(it) {
    nameEl.textContent = it.name;
    nameEl.title = it.rel || it.name;
    sizeEl.textContent = humanSize(it.size);
    const p = it.size > 0 ? Math.min(100, (it.sent / it.size) * 100) : (it.status === UPLOAD_STATUS.DONE ? 100 : 0);
    fill.style.width = p.toFixed(1) + '%';
    fill.style.background = it.status === UPLOAD_STATUS.FAILED ? 'var(--danger)'
      : it.status === UPLOAD_STATUS.CANCELED ? 'var(--text-mute)' : '';
    pctEl.textContent = p.toFixed(0) + '%';
    speedEl.textContent = it.status === UPLOAD_STATUS.UPLOADING && it.bps > 0 ? rate(it.bps) : '';
    statusEl.textContent = uploadStatusText(it);
    statusEl.className = 'zp-up-status is-' + it.status;
    // 失败行的完整原因进 title；超限项再补上可执行的出路（细节不进一句话文案）。
    statusEl.title = it.oversize ? `${it.error}。出路：${oversizeHint()}` : (it.error || '');
    row.classList.toggle('is-failed', it.status === UPLOAD_STATUS.FAILED);
    // 行操作随状态变：等待/上传中 = 取消；失败/已取消 = 重试（超限项不出现）；都能移除。
    clear(actEl);
    const q = ensureUploadQueue();
    if (it.status === UPLOAD_STATUS.WAITING || it.status === UPLOAD_STATUS.UPLOADING) {
      appendAll(actEl, h('button.btn.btn-sm', { text: '取消', title: '取消这个文件（不会在目标目录留下半截文件）', onclick: () => q.cancel(it.id) }));
    } else {
      if (!it.oversize && (it.status === UPLOAD_STATUS.FAILED || it.status === UPLOAD_STATUS.CANCELED)) {
        appendAll(actEl, h('button.btn.btn-sm', { text: '重试', title: '只重试这个文件', onclick: () => q.retry(it.id) }));
      }
      appendAll(actEl, h('button.btn.btn-sm', { text: '移除', onclick: () => q.remove(it.id) }));
    }
  }
  update(item);
  return { row, update };
}

// uploadStatusText 把队列状态翻成用户能看懂的一行字；失败必须带**原因**。
function uploadStatusText(it) {
  switch (it.status) {
    case UPLOAD_STATUS.WAITING: return '等待';
    case UPLOAD_STATUS.UPLOADING: return '上传中';
    case UPLOAD_STATUS.DONE:
      if (it.overwritten) return '已完成（已覆盖同名文件）';
      if (it.savedName && it.savedName !== it.name) return `已完成（重名，已存为 ${it.savedName}）`;
      return '已完成';
    case UPLOAD_STATUS.FAILED: {
      const why = String(it.error || '未知原因');
      return '失败：' + (why.length > 120 ? why.slice(0, 120) + '…' : why);
    }
    case UPLOAD_STATUS.CANCELED: return '已取消';
    default: return it.status;
  }
}

// mountUploadProgress 是**唯一**的进度渲染器：总计区与逐文件行都从队列读，
// 不自己算任何进度（算进度只有 uploadqueue.js 一处）。
function mountUploadProgress(host) {
  const q = ensureUploadQueue();
  const totalLine = h('div.zp-up-total');
  const totalFill = h('i', { style: { width: '0%' } });
  const totalBar = h('div.bar.zp-up-totalbar', [totalFill]);
  const rateLine = h('div.zp-up-rate');
  // 上限那一行从"只读提示"变成入口：点「改上限」就地弹小窗（复用设置页同一套
  // 保存 + 回读实现）。文本节点与按钮分开，刷新时只改文本、不重建按钮。
  const limitText = h('span.zp-up-limit-text');
  const limitEditBtn = h('button.btn.btn-sm', {
    text: '改上限',
    style: { marginLeft: '6px' },
    dataset: { testid: 'zp-up-limit-edit' },
    title: '修改面板单次上传上限（只写面板配置，不碰站点 nginx/PHP）',
    onclick: openPanelLimitFromFiles,
  });
  const limitLine = h('div.hint.zp-up-limit', { style: { display: 'none' } }, [limitText, limitEditBtn]);
  const enterBox = h('div.zp-up-enter');
  const empty = h('div.hint.zp-up-empty', {
    text: '队列是空的：点上面的「选择文件 / 选择文件夹」，或把文件拖进这个面板的任意位置。',
  });
  const listEl = h('div.zp-upload-list');
  const rows = new Map();
  appendAll(host, h('div.files-upload-progress', [totalLine, totalBar, rateLine, limitLine, enterBox, empty, listEl]));

  function refresh() {
    const t = q.totals();
    totalLine.textContent = `共 ${t.count} 个文件 · 已完成 ${t.done} · 失败 ${t.failed}`;
    totalFill.style.width = t.percent.toFixed(1) + '%';
    totalFill.style.background = t.failed ? 'var(--danger)' : '';
    const bits = [];
    if (t.uploading) bits.push(`总速度 ${rate(t.bps)}`);
    if (t.eta > 0) bits.push(`预计剩余 约 ${duration(t.eta)}`);
    if (t.canceled) bits.push(`已取消 ${t.canceled}`);
    rateLine.textContent = bits.join(' · ');
    const li = uploadLimitInfo;
    if (li && !li.verified) {
      limitLine.style.display = '';
      limitText.textContent = `上传上限未复核：${li.note || '读不到面板配置'}（超限文件由服务端拒收）`;
    } else if (li && li.sizeText) {
      limitLine.style.display = '';
      limitText.textContent = `单文件上限 ${li.sizeText} · 来源：${li.source || '面板配置'}`;
    } else {
      limitLine.style.display = 'none';
    }
    // 「进入」只在上传结束且所有相对路径同属一个顶层目录时出现（文件夹上传）。
    clear(enterBox);
    if (uploadEnterDir && !t.busy) {
      appendAll(enterBox, h('button.btn.btn-sm', {
        text: `进入 ${basename(uploadEnterDir)}`,
        onclick: () => { const d = uploadEnterDir; uploadEnterDir = ''; fileListRefresh(d); refresh(); },
      }));
    }
    empty.style.display = t.count ? 'none' : '';
    listEl.style.display = t.count ? '' : 'none';
    // 逐文件行：按队列条目增删，已有行原地更新（不整表重绘，不闪）。
    const live = new Set();
    for (const it of q.items) {
      live.add(it.id);
      let rc = rows.get(it.id);
      if (!rc) { rc = uploadRow(it); rows.set(it.id, rc); listEl.appendChild(rc.row); }
      rc.update(it);
    }
    for (const [id, rc] of [...rows]) {
      if (!live.has(id)) { rc.row.remove(); rows.delete(id); }
    }
    if (uploadPanelState && uploadPanelState.sync) uploadPanelState.sync(t);
  }

  const unsub = q.subscribe(refresh);
  refresh();
  return { refresh, destroy() { unsub(); clear(host); } };
}

// openUploadPanel 打开（或展开）浮动面板。面板挂在 document.body 上，路由切换只重建
// #app 里的内容，所以它不会因为切页面/切目录而消失（用户要求）。
function openUploadPanel(dir) {
  if (dir) uploadTargetDir = dir;
  if (uploadPanelState) { uploadPanelState.setMinimized(false); uploadPanelState.refresh(); return uploadPanelState; }

  const titleEl = h('div.zp-upload-title');
  const minBtn = h('button.zp-upload-iconbtn', { text: '—', title: '收起（上传继续，点右边这条小窗可以再展开）' });
  const closeBtn = h('button.zp-upload-iconbtn', { text: '×' });
  const head = h('div.zp-upload-head', [h('span.zp-upload-ico', { text: '⬆' }), titleEl, h('div', { style: { flex: '1' } }), minBtn, closeBtn]);
  const body = h('div.zp-upload-body');
  const host = h('div.files-upload-progress-host');
  const drop = h('div.files-drop.zp-upload-drop', {
    text: '把文件或整个文件夹拖到这里（拖到面板任意位置也行）；点这里选文件',
    title: '点一下 = 选择文件；也可以把文件/文件夹拖到面板任意位置',
    onclick: pickUploadFiles,
  });
  const retryBtn = h('button.btn.btn-sm', { text: '重试失败项', onclick: () => ensureUploadQueue().retryAllFailed() });
  const actions = h('div.zp-upload-actions', [
    h('button.btn.btn-sm.btn-primary', { text: '选择文件', onclick: pickUploadFiles }),
    h('button.btn.btn-sm', { text: '选择文件夹', onclick: pickUploadFolder }),
    h('button.btn.btn-sm', { text: '开始上传', title: '开始/继续上传队列里「等待」的文件', onclick: () => ensureUploadQueue().start() }),
    h('button.btn.btn-sm', { text: '取消全部', onclick: () => ensureUploadQueue().cancelAll() }),
    retryBtn,
    h('button.btn.btn-sm', { text: '清空已完成', title: '清掉已完成/失败/已取消的行（上传中的不受影响）', onclick: () => ensureUploadQueue().clearFinished() }),
  ]);
  appendAll(body, actions, drop, host);
  const root = h('div.zp-upload-float', [head, body]);
  document.body.appendChild(root);

  const progress = mountUploadProgress(host);
  let minimized = false;

  function dirsOfQueue() {
    const q = ensureUploadQueue();
    return [...new Set(q.items.map((it) => it.dir).filter(Boolean))];
  }

  function sync(t) {
    const dirs = dirsOfQueue();
    const dirText = dirs.length === 1 ? dirs[0] : (dirs.length > 1 ? `${dirs.length} 个目录` : (uploadTargetDir || cwd || '/'));
    titleEl.textContent = minimized
      ? `上传中 ${t.done}/${t.count} · ${t.percent.toFixed(0)}%`
      : `上传到 ${dirText}`;
    titleEl.title = `上传到 ${dirText}`;
    retryBtn.textContent = t.failed ? `重试失败项（${t.failed}）` : '重试失败项';
    retryBtn.disabled = !t.failed;
  }

  function setMinimized(on) {
    minimized = !!on;
    root.classList.toggle('zp-min', minimized);
    body.style.display = minimized ? 'none' : '';
    minBtn.textContent = minimized ? '▢' : '—';
    minBtn.title = minimized ? '展开上传面板' : '收起（上传继续，点右边这条小窗可以再展开）';
    progress.refresh();
  }

  minBtn.onclick = () => setMinimized(!minimized);
  head.addEventListener('click', (e) => { if (minimized && e.target !== minBtn && e.target !== closeBtn) setMinimized(false); });
  closeBtn.title = '关闭面板';
  closeBtn.onclick = () => {
    const t = ensureUploadQueue().totals();
    if (t.busy || t.count) {
      // 绝不因为一次误点就把"正在上传"藏掉：收起成小窗，上传继续。
      setMinimized(true);
      toast('上传在后台继续，点右下角小窗可以再展开', 'info', 9000);
      return;
    }
    destroy();
  };

  function destroy() {
    progress.destroy();
    root.remove();
    uploadPanelState = null;
  }

  // 整个面板都能接拖拽（用户要求），拖进来时整体高亮。
  wireDropZone(root, {
    show: (on) => { root.classList.toggle('zp-drop-active', on); drop.classList.toggle('active', on); },
    dir: () => uploadTargetDir || cwd,
  });

  uploadPanelState = { root, refresh: progress.refresh, setMinimized, destroy, sync };
  progress.refresh();
  // 面板一打开就回读上限并显示那一行（不能等用户真的传过一次才显示 ——
  // 否则"当前上限是多少、能不能改"在他上传前根本看不到）。
  getUploadLimit().then((info) => applyPanelLimitView(info));
  return uploadPanelState;
}

// uploadPanel 是工具栏「⬆ 上传」那颗按钮的入口（打开/展开浮动面板）。
function uploadPanel() { openUploadPanel(cwd); }

// ---------- 入队（按钮 / 拖拽 / 选择框共用的唯一入口） ----------

// uploadEntries 是所有上传入口（选择文件/选择文件夹/拖拽）的唯一实现。
//
// 整体套 try/catch：**任何**没预料到的异常都必须变成用户看得见的提示。
// 那次报障的形态就是"异常抛在任何提示之前 → 彻底无声"。
async function uploadEntries(entries, opts = {}) {
  try {
    if (!entries.length) return;
    const dir = opts.dir || cwd;
    openUploadPanel(dir);
    const q = ensureUploadQueue();
    // 先入队：立刻逐文件成行（用户要求），上限到达后再把超限项标失败。
    uploadEnterDir = '';
    q.add(entries.map((e) => ({ file: e.file, rel: e.rel })), { dir, folder: !!opts.folder });
    try {
      q.setLimit(await getUploadLimit());
    } catch { /* 读不到上限就不拦，交给服务端判 */ }
    // 同名文件先问清楚：覆盖还是共存（用户明确要求）。
    // 上传文件夹不走这个询问 —— 它的语义本来就是"按原结构覆盖整站"，
    // 把 index.php 问成 index-1.php 会让站点直接跑不起来。
    if (!opts.folder) {
      const dup = conflictNames(entries);
      if (dup.length) {
        const choice = await askUploadConflict(dup, entries.length);
        if (!choice) { q.cancelWaiting(); return; } // 用户取消：一个字节都没上传
        q.setConflict(choice);
      }
    }
    q.start();
  } catch (err) {
    const msg = err && err.message ? err.message : String(err);
    toast('上传未能开始：' + msg, 'err', 15000);
  }
}

// conflictNames 返回这一批里"目标目录已存在同名条目"的文件名（去重）。
//
// 判据用**当前目录的列表**（lastList）：用户看到的就是它，所以弹窗里说的
// 名字与他屏幕上的一致。列表可能已经过期（别人刚传过），那种情况由服务端兜底
// （on_conflict 传到后端，真正落盘时再判一次）。
function conflictNames(entries) {
  const have = new Set(((lastList && lastList.entries) || []).map((e) => e.name));
  const out = [];
  for (const e of entries) {
    const base = e.rel.split('/').pop();
    if (have.has(base) && !out.includes(base)) out.push(base);
  }
  return out;
}

// askUploadConflict 问用户"同名文件怎么办"，返回 'rename' / 'overwrite' / null（取消）。
//
// 默认选中「保留两者」：不覆盖是最安全的默认值（覆盖别人的文件不可逆）。
function askUploadConflict(names, total) {
  return new Promise((resolve) => {
    let picked = 'rename';
    const radios = [
      { v: 'rename', label: '保留两者（推荐）', desc: '同名文件自动改名（如 index-1.php），两个都留着，什么都不丢。' },
      { v: 'overwrite', label: '覆盖同名文件', desc: '用新上传的文件替换旧文件 —— 旧内容不可恢复，请确认。' },
    ];
    const inputs = radios.map((r) => h('input', {
      type: 'radio', name: 'zp-upload-conflict', value: r.v, checked: r.v === picked,
      onchange: () => { picked = r.v; },
    }));
    const body = h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, [
      h('p', { text: `目标目录里已经有 ${names.length} 个同名文件（本次共 ${total} 个文件）：` }),
      h('ul', { style: { margin: '6px 0 10px 18px', maxHeight: '160px', overflow: 'auto' } },
        names.slice(0, 50).map((n) => h('li', { text: n }))),
      ...radios.map((r, i) => h('label', {
        style: { display: 'flex', gap: '8px', alignItems: 'flex-start', padding: '8px 10px',
          border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', marginBottom: '8px', cursor: 'pointer' },
      }, [inputs[i], h('div', [
        h('div', { style: { fontWeight: '600' }, text: r.label }),
        h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: r.desc }),
      ])])),
      h('div.hint', { text: '这个选择对本次上传的所有同名文件都生效。' }),
    ]);
    const m = modal({
      title: '同名文件已存在',
      body,
      footer: [
        h('button.btn', { text: '取消上传', onclick: () => { m.close(); resolve(null); } }),
        h('button.btn.btn-primary', {
          text: '继续上传',
          onclick: () => { m.close(); resolve(picked); },
        }),
      ],
    });
  });
}

// serverReason 把服务端返回的原因原样取出来（这是用户唯一能据此自救的信息）。
//
// 优先 JSON 里的 msg/error（面板自己的失败响应）；拿不到就把原始 body 截一段
// 显示出来（例如反向代理返回的 413 HTML 页面 —— 那也必须让用户看见）。
function serverReason(xhr, data) {
  let msg = '';
  if (data && typeof data === 'object') msg = String(data.msg || data.error || '');
  if (!msg) {
    const txt = String(xhr.responseText || '').trim();
    if (txt) msg = txt.length > 400 ? txt.slice(0, 400) + '…' : txt;
  }
  return msg || '（服务端没有返回任何说明）';
}

// oversizeHint 把"单文件太大"的出路接到失败行的 title 上（细节不进一句话文案）。
function oversizeHint() {
  return oversizeAdvice().join('；');
}

// 退出登录时（hash 变空）销毁面板：绝不能把上一位登录者的上传队列留在登录页上。
window.addEventListener('hashchange', () => {
  if (location.hash === '' && uploadPanelState) uploadPanelState.destroy();
});

export function FilesView(content, ctx = {}) {
  clear(content);
  selection = new Set();
  selAnchor = -1;
  visibleEntries = [];
  rowEls = [];
  headCheckbox = null;

  const crumbs = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap', fontSize: '13px' } });
  const toolbar = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' } });
  const tableBox = h('div', { style: { overflowX: 'auto', minHeight: '260px' } });
  const statusBar = h('div.files-status', { style: { fontSize: '12px', color: 'var(--text-mute)', padding: '8px 14px', borderTop: '1px solid var(--border-soft)' } });

  // 拖拽提示区：只有拖动时才显示（平时不占版面）。
  const dropZone = h('div.files-drop', {
    style: { display: 'none', margin: '0 14px 10px' },
    text: '把文件或整个文件夹拖到这里上传（文件夹会保留目录结构）',
  });

  // opBar 是复制/移动/删除的**就地进度条**：用户一般盯着文件管理器，
  // 不该逼他跑去任务中心看进度（任务窗也可以看，数据是同一份）。
  const opBar = h('div.files-opbar', { style: { display: 'none', padding: '9px 14px', borderBottom: '1px solid var(--border-soft)' } });

  const card = h('div.card', [
    // 「网络盘」入口放卡片头（面包屑那一行）而不是工具栏：工具栏在 390px 上已经很挤，
    // 再塞一颗会把行高顶开、列表整体下移（工具栏注释里记过这个坑）。
    // flexWrap：360px 上面包屑 + 这颗按钮放不进一行（实测溢出 25px），允许换行。
    h('div.card-head', { style: { flexWrap: 'wrap' } }, [crumbs, h('div.spacer'), h('button.btn.btn-sm', {
      text: '🖧 网络盘',
      title: '挂载 NAS 共享（SMB）给 Jellyfin 当媒体库 —— 跳到「磁盘管理 → 网络磁盘（SMB）」',
      dataset: { testid: 'zp-smb-entry' },
      onclick: () => gotoSMBDisks(),
    })]),
    h('div', { style: { padding: '10px 14px', borderBottom: '1px solid var(--border-soft)', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [toolbar]),
    opBar,
    dropZone,
    h('div.card-body.tight', [tableBox]),
    statusBar,
  ]);

  appendAll(content, card);
  // 页面上的拖拽区与上传面板共用同一个 wireDropZone/handleDrop（模块级唯一入口）。
  wireDropZone(card, {
    show: (on) => { dropZone.style.display = on ? '' : 'none'; },
    dir: () => cwd,
  });

  // fileListRefresh 让上传队列（模块级、跨页面存活）在完成后刷新**当前**这个视图；
  // 视图已经脱离文档（切走了）就什么都不做。
  fileListRefresh = (p) => { if (content.isConnected) load(p || cwd); };

  async function load(path) {
    if (path) cwd = path;
    selection = new Set();
    selAnchor = -1;
    clear(tableBox);
    appendAll(tableBox, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取目录…' })]));
    try {
      lastList = await api.files(cwd, showHidden);
    } catch (e) {
      clear(tableBox);
      renderOpenError(e);
      return;
    }
    cwd = lastList.path;
    renderCrumbs();
    renderToolbar();
    renderTable();
    // 回到这个目录时，如果还有文件操作在后台跑，把就地进度条接回来。
    resumeOpBar();
  }

  // ---------- 复制 / 移动 / 删除：走任务中心 + 就地进度条 ----------
  //
  // 为什么不再是同步请求：用户报障"从一个盘向另一个盘剪切粘贴 80 个文件、
  // 每个 100 多 MB，过程中没有任何进度提示" —— 同步请求会干等几分钟，
  // 刷新页面还会掐断它。现在一次粘贴 = 一个任务（202 + task_id），
  // 进度是后端的**结构化字节进度**（tasks 的 progress），关掉页面也照跑。
  //
  // 订阅只有一个来源：tasks.js 的 onChange（复用它的 SSE），这里不另写一套。
  let opTaskId = '';

  const OP_KINDS = /^file_(copy|move|delete|media_clean)$/;

  // 进度条内部节点**只建一次**，之后原地更新：
  // 重建 DOM 会让节点的位置/尺寸每 200ms 变一次（点击「中断」时按钮正被替换，
  // 浏览器/自动化都会判成"不稳"），原地更新还顺带没有闪烁。
  const opFill = h('div', { style: { height: '100%', width: '0%', borderRadius: '999px', background: 'var(--brand)' } });
  const opMsg = h('div', { style: { fontSize: '12px', color: 'var(--text-dim)', marginTop: '5px', wordBreak: 'break-word' }, text: '' });
  const opCancel = h('button.btn.btn-sm.btn-danger', {
    text: '中断',
    title: '中断后：已完成的保持不变，没写完的半成品会删掉',
    onclick: async () => {
      if (!opTaskId) return;
      try {
        await api.taskCancel(opTaskId);
        toast('已请求中断…', 'warn', 5000);
      } catch (e) { toast('中断失败：' + ((e && e.message) || e), 'err', 10000); }
    },
  });
  appendAll(opBar, h('div', { style: { display: 'flex', alignItems: 'center', gap: '10px' } }, [
    h('div', { style: { flex: '1', minWidth: '0' } }, [
      h('div', { style: { height: '6px', borderRadius: '999px', background: 'var(--border)', overflow: 'hidden' } }, [opFill]),
      opMsg,
    ]),
    opCancel,
  ]));

  function renderOpBar() {
    const meta = opTaskId ? taskCenter.meta(opTaskId) : null;
    if (!meta || String(meta.status || 'running') !== 'running') {
      opBar.style.display = 'none';
      return;
    }
    const p = meta.progress || null;
    const pct = taskCenter.progressPercent(p);
    opFill.style.width = pct == null ? '100%' : pct + '%';
    opFill.style.opacity = pct == null ? '.35' : '1';
    // 速度/ETA 走任务中心同一份采样（progressRateText）：跨盘/网络盘复制时
    // 用户最需要"还要多久"。数据不够时它返回"计算中…"，不编数字。
    const rate = taskCenter.progressRateText(opTaskId, p);
    opMsg.textContent = ((p && p.message) || meta.title || '正在处理…') + (rate ? ' · ' + rate : '');
    opBar.style.display = '';
  }

  // resumeOpBar 在进入/回到这个目录时把仍在跑的文件操作进度条接回来
  // （切走再切回来不该"看不见进度"，任务本身一直在后台跑）。
  function resumeOpBar() {
    if (opTaskId && taskCenter.meta(opTaskId)) return;
    const t = taskCenter.findByTarget(cwd);
    if (t && OP_KINDS.test(String(t.kind || ''))) {
      opTaskId = t.id;
      renderOpBar();
    }
  }

  // startFileOp 是复制/移动/删除的唯一入口：走任务中心、就地显示进度、如实收尾。
  async function startFileOp({ kind, title, run, target, onSettled }) {
    const id = await taskCenter.start({
      kind, title, target: target || cwd,
      start: run,
      // 就地有进度条了，不再自动弹任务窗；任务照常登记，任务中心随时能查。
      openWindow: false,
      onDone: (task) => {
        opTaskId = '';
        renderOpBar();
        load(cwd);
        if (onSettled) { try { onSettled(task); } catch { /* 收尾失败不影响结果展示 */ } }
        showFileOpResult(task);
      },
    });
    if (id) { opTaskId = id; renderOpBar(); }
    return id;
  }

  // showFileOpResult 如实收尾：成功/跳过/失败逐条说清，失败项列出来。
  function showFileOpResult(task) {
    if (!task) return;
    const canceled = String(task.status || '') === 'canceled';
    const r = task.result || {};
    const msg = r.msg || (canceled ? '已中断' : '已完成');
    toast(msg, (canceled || Number(r.failed) > 0) ? 'warn' : 'ok', (canceled || Number(r.failed) > 0) ? 15000 : 7000);
    // 去广告是"成功/跳过/失败"三态明细（每个文件一行 + 省下的体积），单独画一张表。
    if (String(task.kind || '') === 'file_media_clean') { showCleanResult(r, canceled); return; }
    const bad = (r.items || []).filter((it) => it && it.error);
    if (!bad.length) return;
    modal({
      title: canceled ? '已中断：未完成的项' : '失败明细',
      body: h('div', [
        h('p', { text: `成功 ${r.done || 0} 项，失败 ${bad.length} 项`
          + (r.pending ? `，未处理 ${r.pending} 项` : '') + '。' }),
        h('div.hint', { text: '失败的文件没有被改动；跨卷移动中断时源文件一定保留。' }),
        h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.8', maxHeight: '260px', overflow: 'auto' } },
          bad.slice(0, 200).map((it) => h('li', { text: (it.name || it.from) + '：' + it.error }))),
      ]),
    });
  }

  // showCleanResult 是「🧹 去广告（无损）」的结果明细：三态逐条 + 省下的体积 +
  // "这一趟去掉了什么" + 备份文件名（只有保留备份的条目才有），失败原因原样显示。
  function showCleanResult(r, canceled) {
    const items = r.items || [];
    const stateOf = (it) => (it.error ? '失败' : (it.skipped ? '跳过' : '成功'));
    const savedText = (it) => {
      if (it.error || it.skipped) return '—';
      const n = Number(it.saved_bytes || 0);
      return n > 0 ? '省 ' + humanSize(n) : '没有变小';
    };
    // 不备份时绝不显示"已备份"：只有后端回报了 backup 名才写出来。
    const backupText = (it) => (it.error || it.skipped ? '—' : (it.backup || '未保留'));
    const rows = items.map((it) => h('tr', [
      h('td.zp-plan-name', { text: it.name || it.from || '' }),
      h('td', { text: stateOf(it) }),
      h('td', { text: savedText(it) }),
      h('td', { text: backupText(it) }),
      h('td', {
        text: it.error || it.skipped_reason || it.note || '',
        style: { color: it.error ? 'var(--danger)' : (it.skipped ? 'var(--warn)' : 'var(--text-mute)') },
      }),
    ]));
    const keepBackup = !!r.keep_backup;
    modal({
      title: '🧹 去广告（无损）结果',
      wide: true,
      body: h('div', [
        h('p', { text: (canceled ? '已中断：' : '') + (r.msg || '') }),
        items.length
          ? h('div.zp-plan-scroll', [h('table.table', { style: { fontSize: '12px' } }, [
            h('thead', [h('tr', ['文件', '状态', '体积', '备份', '原因'].map((t) => h('th', { text: t })))]),
            h('tbody', rows),
          ])])
          : h('div.hint', { text: '没有可显示的条目' }),
        h('div.hint', {
          text: keepBackup
            ? '原文件已保留为 .bak（上表「备份」列）；失败/取消时临时文件已删除，源文件未改动。'
            : '原文件已直接替换（未保留备份）；失败/取消时临时文件已删除，源文件未改动。',
        }),
      ]),
    });
  }

  const unsubOpBar = taskCenter.onChange((kind, payload) => {
    if (kind === 'lines') return;
    if (payload && payload.id && opTaskId && payload.id !== opTaskId) return;
    renderOpBar();
  });

  // renderOpenError 把后端的错误按行显示出来。
  //
  // 为什么不是一行 <p>：外接卷被 macOS 隐私保护拒绝时，后端（api_files.go）
  // 返回的是 **403 + 多行可操作指引**（改用「挂载到自定义挂载点」）。挤进一行
  // 文本会把换行吃掉，用户只看到 "operation not permitted" —— 那正是报障的原文。
  function renderOpenError(e) {
    const msg = String((e && e.message) || e);
    // 403 有两种：越界（面板白名单拒绝）与"外接卷被 macOS 隐私保护拒绝"。
    // 只有后者才说"被系统拒绝"，并给磁盘页入口 —— 别把面板自己的拒绝也说成系统问题。
    const isTCC = !!(e && e.status === 403) && msg.includes('隐私保护');
    const box = h('div.files-denied');
    msg.split('\n').map((s) => s.trim()).filter(Boolean)
      .forEach((ln, i) => box.append(h('div', { class: i === 0 ? 'files-denied-head' : 'files-denied-line', text: ln })));
    // 指引里提到的「磁盘 → 挂载到自定义挂载点…」直接给一个入口。
    if (isTCC) {
      box.append(h('button.btn.btn-sm', {
        text: '🖥 打开磁盘管理…',
        style: { marginTop: '8px' },
        onclick: () => { location.hash = '#/disks'; },
      }));
    }
    appendAll(tableBox, h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: isTCC ? '无法打开该目录（被系统拒绝）' : '无法打开该目录' }),
      box,
    ]));
  }

  // ---------- 位置下拉：常用 / 磁盘与卷 ----------
  // 标签由后端下发（root_labels，已去重/剪枝/唯一），前端只按 root_kinds 分组渲染。
  function rootKind(r) {
    return (lastList && lastList.root_kinds && lastList.root_kinds[r]) || 'other';
  }

  // root_labels 是后端的"路径 → 唯一标签"表；没有它时（旧响应）退回本地启发式。
  function rootLabels() {
    return (lastList && lastList.root_labels) || null;
  }

  function rootLabel(r) {
    const labels = rootLabels();
    if (labels && labels[r]) return labels[r];
    // 回退也带上位置，免得两个根显示成同一行。
    switch (rootKind(r)) {
      case 'www': return 'www 目录';
      case 'home': return '用户目录';
      case 'panel': return r + '（面板安装根）';
      case 'homebrew': return r + '（Homebrew 配置）';
      case 'app': return r + '（应用配置目录）';
      case 'volume': return r.replace(/^\/Volumes\//, '') + '（' + r + '）';
      default: return r;
    }
  }

  // 只列后端给了标签的根；没有 root_labels 时退回旧启发式。
  function dropdownRoots() {
    const roots = lastList?.roots || [];
    const labels = rootLabels();
    if (labels) return roots.filter((r) => !!labels[r]);
    const named = new Set(['www', 'home', 'panel', 'homebrew', 'app', 'volume']);
    return roots.filter((r) => {
      const kind = rootKind(r);
      if (kind === 'data') return false; // 面板数据目录从安装根进去即可，不重复列
      if (named.has(kind)) return true;
      return !roots.some((o) => o !== r && r.startsWith(o + '/'));
    });
  }

  // bestRoot 取"最长匹配"的根（面包屑与下拉定位都用它，避免子根被父根盖住）。
  function bestRoot(p) {
    const roots = lastList?.roots || [];
    let best = '';
    for (const r of roots) {
      if ((p === r || p.startsWith(r + '/')) && r.length > best.length) best = r;
    }
    return best;
  }

  function locationSelect() {
    const roots = lastList?.roots || [];
    const listed = dropdownRoots();
    const cur = bestRoot(cwd);
    // 当前目录若落在"被折叠的子根"里，用它的祖先根作为下拉的选中值。
    const curListed = listed.includes(cur) ? cur : listed.filter((r) => cur.startsWith(r + '/')).sort((a, b) => b.length - a.length)[0] || cur;

    const groups = { common: [], volume: [], other: [] };
    for (const r of listed) {
      const k = rootKind(r);
      if (k === 'volume') groups.volume.push(r);
      else if (k === 'www' || k === 'home' || k === 'panel' || k === 'homebrew' || k === 'app') groups.common.push(r);
      else groups.other.push(r);
    }
    const order = { www: 0, home: 1, panel: 2, homebrew: 3, app: 4, other: 9 };
    groups.common.sort((a, b) => (order[rootKind(a)] ?? 9) - (order[rootKind(b)] ?? 9));

    const opt = (r) => h('option', { value: r, text: rootLabel(r) });
    const optgroup = (label, rs) => (rs.length
      ? h('optgroup', { label }, rs.map(opt))
      : null);

    const sel = h('select.select', {
      title: '位置：常用目录 / 磁盘与卷',
      style: { width: 'auto', maxWidth: '260px', padding: '3px 26px 3px 8px', fontSize: '12.5px' },
      onchange: (e) => {
        if (e.target.value === '__disks__') {
          e.target.value = curListed;
          location.hash = '#/disks';
          return;
        }
        if (e.target.value) load(e.target.value);
      },
    }, [
      optgroup('常用', groups.common),
      optgroup('磁盘与卷', groups.volume),
      optgroup('其它位置', groups.other),
      h('option', { value: '__disks__', text: '🖥 管理磁盘…' }),
    ]);
    sel.value = curListed;
    return sel;
  }

  function renderCrumbs() {
    clear(crumbs);
    const roots = lastList?.roots || [];
    const root = bestRoot(cwd);
    const parts = [];

    // 位置下拉：始终显示（哪怕只有一个根，也要能一键跳到磁盘页）
    parts.push(locationSelect());
    parts.push(h('span', { style: { color: 'var(--text-mute)' }, text: '/' }));

    if (root) {
      const rel = cwd.slice(root.length).replace(/^\//, '');
      const segs = rel ? rel.split('/') : [];
      parts.push(h('a', {
        href: 'javascript:void(0)', text: basename(root) || root,
        title: root, onclick: () => load(root),
      }));
      let acc = root;
      segs.forEach((s) => {
        acc += '/' + s;
        const target = acc;
        parts.push(h('span', { style: { color: 'var(--text-mute)' }, text: '/' }));
        parts.push(h('a', { href: 'javascript:void(0)', text: s, onclick: () => load(target) }));
      });
    } else {
      parts.push(h('span', { text: cwd }));
    }
    // 可访问范围：悬停可见（roots 少时直接列出来也占地方）
    if (roots.length) {
      parts.push(h('span.pill', {
        text: `可访问 ${roots.length} 个位置`,
        title: '允许访问的范围：\n' + roots.join('\n'),
        style: { cursor: 'help' },
      }));
    }
    appendAll(crumbs, ...parts);
    // 父目录按钮
    if (lastList?.parent) {
      appendAll(crumbs, h('div', { style: { flex: 1 } }),
        h('button.btn.btn-sm', { text: '↑ 上一级', onclick: () => load(lastList.parent) }));
    }
  }

  // ---------- 列表：排序 / 图片预览 / 键盘 ----------
  //
  // 用户："让文件管理器和编辑器变成可用的现代的工具。"
  // 编辑器换成了 CodeMirror（见下），文件列表这边补三件最常用的：
  //   · 点表头排序（名称/大小/时间），目录永远在前；
  //   · 图片点开是**预览**（把图片塞进文本编辑器是最糟的默认行为）；
  //   · 键盘：↑↓ 之外 —— Enter 打开选中项、Delete 删除选中项、Esc 取消选择，
  //     双击行直接打开（这是所有人的肌肉记忆，之前只有"编辑"按钮能点）。
  const IMAGE_EXT = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i;
  // 浏览器能原生解码的媒体（其余一律不假装能播，见 noPlayBody）：
  //   视频 mp4/m4v/mov(H.264+AAC)、webm/ogv；音频 mp3/m4a/aac/wav/ogg/flac。
  const VIDEO_EXT = /\.(mp4|m4v|mov|webm|ogv)$/i;
  const AUDIO_EXT = /\.(mp3|m4a|aac|wav|ogg|oga|opus|flac)$/i;
  // 已知放不了的容器/编码：mkv/avi/wmv/flv/rmvb 等，浏览器基本解不了（坑 188）。
  const NO_PLAY_EXT = /\.(mkv|avi|wmv|flv|rmvb|rm|mpg|mpeg|ts|m2ts|3gp|asf|wma|mka|ape|vob|f4v)$/i;
  // 视频压缩的**候选**扩展名：与后端 internal/videoopt 的 videoExts 同一口径
  // （门禁会比对两边有没有走样）。注意它比 VIDEO_EXT 宽 —— 后者只是"浏览器能播的"。
  const VIDEO_CANDIDATE_EXT = /\.(mp4|m4v|mov|mkv|avi|webm|flv|wmv|mpg|mpeg|ts|m2ts|mts|3gp|rmvb|rm|vob|ogv|asf|f4v)$/i;
  // 排序状态：默认名字升序（= 旧行为）；用户的选择记 localStorage，刷新后保持。
  // 目录永远排在文件前面这条由 sortfiles.js 的 compareEntries 保证（唯一的比较器）。
  // 排序入口只有**表头点击**这一种（用户点名：不要工具栏的排序菜单）。
  const sortPref0 = readSortPref();
  let sortKey = sortPref0.key; // name | type | time | size
  let sortDir = sortPref0.dir; // 1 升序 / -1 降序

  function persistSort() { writeLS(SORT_PREF_KEY, sortKey + ':' + sortDir); }

  // setSort 是排序状态的**唯一**写入口（表头点击走它）：写偏好 → 重画表格。
  function setSort(key, dir) {
    sortKey = normalizeSortKey(key);
    sortDir = normalizeSortDir(dir);
    persistSort();
    renderTable();
  }

  function isImage(entry) { return !entry.is_dir && IMAGE_EXT.test(entry.name || ''); }

  // mediaKind 判一个条目该走播放器还是"放不了"提示：video | audio | unsupported | ''。
  function mediaKind(entry) {
    if (entry.is_dir) return '';
    const n = entry.name || '';
    if (VIDEO_EXT.test(n)) return 'video';
    if (AUDIO_EXT.test(n)) return 'audio';
    if (NO_PLAY_EXT.test(n)) return 'unsupported';
    return '';
  }

  // openLabel 是列表按钮与右键菜单的动词：媒体不能写"编辑"（以前点开是乱码）。
  function openLabel(entry) {
    if (entry.is_dir) return '打开';
    if (isImage(entry)) return '查看';
    return mediaKind(entry) ? '播放' : '编辑';
  }

  // sortedEntries 只做接线：比较逻辑全在 sortfiles.js 的纯函数里（门禁直接断言它）。
  function sortedEntries(list) {
    return sortEntries(list, sortKey, sortDir);
  }

  // sortHeader 是表头那颗排序按钮（唯一的排序入口：点同列切升/降序）。
  function sortHeader(key, text) {
    const on = sortKey === key;
    return h('th', [
      h('button.btn.btn-sm', {
        text: text + (on ? (sortDir > 0 ? ' ▲' : ' ▼') : ''),
        title: '按' + text + '排序（再点一次反向）',
        style: { padding: '2px 6px', fontSize: '12px' },
        onclick: () => setSort(key, on ? -sortDir : 1),
      }),
    ]);
  }

  // typeLabel 是「类型」列的单元格文本：目录=「目录」，文件=小写扩展名（不带点），
  // 无扩展名=「—」（排序时它永远垫底，见 sortfiles.js）。
  function typeLabel(e) {
    if (e.is_dir) return '目录';
    const ext = extOf(e.name);
    return ext ? ext.slice(1) : '—';
  }

  // openAny 是按文件类型选动作的唯一入口：图片 → 预览，音视频 → 播放器，其余 → 文本编辑器。
  function openAny(entry) {
    const kind = mediaKind(entry);
    if (isImage(entry)) previewImage(entry);
    else if (kind) previewMedia(entry, kind);
    else openEditor(entry);
  }

  // ---------- 图片查看器（原图 + 上一张/下一张 + 缩放） ----------
  //
  // 用户要求"点开图片走内置查看器"。看图的真实需求是**在同目录的一组图之间翻页**
  // 与放大看细节，所以这里不是单图弹窗：它把当前目录里所有图片组成一个列表，
  // 支持 ←/→ 翻页、+/- 缩放、0 复位。非图片仍然走文本编辑器（见 openAny）。
  function previewImage(entry) {
    const images = (lastList?.entries || []).filter(isImage);
    let idx = images.findIndex((x) => x.path === entry.path);
    if (idx < 0) { images.unshift(entry); idx = 0; }
    let scale = 1;

    const img = h('img', {
      alt: '',
      style: { display: 'block', margin: '0 auto', maxWidth: '100%', borderRadius: '6px', background: 'var(--panel-2)', transformOrigin: 'center center' },
      onerror: () => toast('图片加载失败（可能不是浏览器支持的格式）', 'warn', 8000),
    });
    const caption = h('div.hint', { style: { marginTop: '8px', textAlign: 'center' } });
    const zoomLabel = h('span.pill', { text: '100%' });
    const stage = h('div.zp-imgview-stage', [img]);
    const nav = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', justifyContent: 'center', marginTop: '8px' } });

    function applyZoom() {
      img.style.transform = `scale(${scale})`;
      img.style.cursor = scale > 1 ? 'grab' : 'default';
      zoomLabel.textContent = Math.round(scale * 100) + '%';
    }
    function show(i) {
      idx = (i + images.length) % images.length;
      const e = images[idx];
      scale = 1; applyZoom();
      img.style.width = '';
      img.src = api.fileDownloadURL(e.path);
      img.alt = e.name;
      caption.textContent = `${e.name} · ${humanSize(e.size)} · 第 ${idx + 1}/${images.length} 张`;
    }

    function onKey(ev) {
      if (ev.key === 'ArrowLeft') { ev.preventDefault(); show(idx - 1); }
      else if (ev.key === 'ArrowRight') { ev.preventDefault(); show(idx + 1); }
      else if (ev.key === '+' || ev.key === '=') { ev.preventDefault(); scale = Math.min(6, scale + 0.25); applyZoom(); }
      else if (ev.key === '-') { ev.preventDefault(); scale = Math.max(0.25, scale - 0.25); applyZoom(); }
      else if (ev.key === '0') { ev.preventDefault(); scale = 1; applyZoom(); }
    }
    document.addEventListener('keydown', onKey);

    appendAll(nav,
      h('button.btn.btn-sm', { text: '◀ 上一张', disabled: images.length < 2, onclick: () => show(idx - 1) }),
      h('button.btn.btn-sm', { text: '下一张 ▶', disabled: images.length < 2, onclick: () => show(idx + 1) }),
      h('span', { style: { width: '10px' } }),
      h('button.btn.btn-sm', { text: '－ 缩小', onclick: () => { scale = Math.max(0.25, scale - 0.25); applyZoom(); } }),
      zoomLabel,
      h('button.btn.btn-sm', { text: '＋ 放大', onclick: () => { scale = Math.min(6, scale + 0.25); applyZoom(); } }),
      h('button.btn.btn-sm', { text: '复位', onclick: () => { scale = 1; applyZoom(); } }),
    );

    const m = modal({
      title: '🖼️ 图片查看器',
      wide: true,
      body: h('div', [
        stage,
        caption,
        nav,
        h('div.hint', { style: { textAlign: 'center', marginTop: '4px' },
          text: '快捷键：← / → 切换上一张下一张，+ / - 缩放，0 复位' }),
      ]),
      footer: () => [
        h('button.btn', { text: '下载原图', onclick: () => { window.location.href = api.fileDownloadURL(images[idx].path); } }),
        h('button.btn', { text: '用文本编辑器打开', onclick: () => { m.close(); openEditor(images[idx]); } }),
        h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() }),
      ],
      onClose: () => document.removeEventListener('keydown', onKey),
    });
    show(idx);
  }

  // ---------- 音视频播放器（Plyr，内嵌 vendor） ----------
  //
  // 用户要"文件管理要能打开视频和音频文件" + "最好引入现有项目"。
  // Plyr 见 assets/vendor/plyr/README.md；解码交给浏览器，面板不做转码。
  function previewMedia(entry, kind) {
    if (kind === 'unsupported') { noPlayModal(entry); return; }

    // preload=metadata：只取元数据，长视频/大文件不会整份进内存。
    const media = h(kind === 'video' ? 'video' : 'audio', {
      class: 'zp-media-el', controls: true, playsinline: true, preload: 'metadata',
    });
    media.src = api.fileDownloadURL(entry.path);

    const status = h('div.hint', { style: { textAlign: 'center' }, text: '正在加载播放器…' });
    const stage = h('div.zp-media-stage', [media]);
    let player = null;
    let closed = false;
    let failed = false;

    // 元素报错（容器/编码浏览器解不了）→ 换成明确提示，绝不假装能播。
    media.addEventListener('error', () => {
      if (closed || failed) return;
      failed = true;
      showNoPlay(stage, status, entry, player);
    });

    const m = modal({
      title: (kind === 'video' ? '🎬 ' : '🎵 ') + entry.name,
      wide: true,
      body: h('div', [stage, status]),
      footer: () => [downloadBtn(entry), h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() })],
      onClose: () => {
        closed = true;
        try { if (player) player.destroy(); } catch { /* 销毁失败不该挡住关闭 */ }
      },
    });

    ensurePlyr().then((Plyr) => {
      if (closed || failed) return;
      player = new Plyr(media, PLYR_OPTIONS);
      player.on('ready', () => { if (!closed) status.style.display = 'none'; });
    }).catch(() => {
      if (closed) return;
      // Plyr 没起来也不装死：元素自带 controls，原生控件仍可播放。
      status.textContent = '播放器皮肤没加载上，已用浏览器自带控件';
    });
  }

  // showNoPlay 把播放器换成"放不了"提示（loaded→error 与已知不支持的格式共用）。
  function showNoPlay(stage, status, entry, player) {
    try { if (player) player.destroy(); } catch { /* 同上 */ }
    clear(stage);
    stage.appendChild(noPlayBody());
    if (status) status.style.display = 'none';
  }

  function noPlayModal(entry) {
    const m = modal({
      title: entry.name,
      body: noPlayBody(),
      footer: () => [downloadBtn(entry), h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() })],
    });
  }

  // noPlayBody：一句话给结论，为什么/后续能力收进折叠项（文案纪律：可见文案 ≤40 字）。
  function noPlayBody() {
    return h('div', [
      h('div.hint', { style: { textAlign: 'center', padding: '18px 0' },
        text: '浏览器不支持这个格式，请下载后用本地播放器' }),
      h('details', { style: { maxWidth: '560px', margin: '0 auto' } }, [
        h('summary', { text: '为什么放不了？', style: { cursor: 'pointer' } }),
        h('div.hint', { style: { marginTop: '6px' },
          text: '浏览器只直接解 mp4/m4v/mov/webm/ogv 与 mp3/m4a/aac/wav/ogg/flac；' +
            'mkv/avi/wmv/flv/rmvb 等容器或 HEVC 等编码要先转码，后续可做 ffmpeg 边转边播。' }),
      ]),
    ]);
  }

  function downloadBtn(entry) {
    return h('button.btn', {
      text: '下载',
      onclick: () => { window.location.href = api.fileDownloadURL(entry.path); },
    });
  }

  // wwwRoot 返回网站根目录（kind=www 的根），找不到就退回第一个根。
  function wwwRoot() {
    const roots = lastList?.roots || [];
    return roots.find((r) => rootKind(r) === 'www') || roots[0] || '';
  }

  // 新建/魔法箱的二级菜单项：都是"工具栏入口"的菜单，与行操作菜单无关
  // （行操作菜单只能来自 rowMenuItems，见那里）。
  function newMenuItems() {
    return [
      { label: '新建文件夹', run: () => toolbarNew('dir') },
      { label: '新建文件', run: () => toolbarNew('file') },
    ];
  }

  // 魔法箱：原来右侧那三颗（图片压缩 / 视频压缩 / 打包压缩）合并成一个菜单，
  // 位置仍在工具栏右侧；各项的弹窗与功能一字未改（另加「去广告（无损）」）。
  function compressMenuItems() {
    return [
      { label: '🖼️ 图片压缩', title: '把当前目录里的图片压小（默认另存为 xxx.min.<ext>，不动原文件）', run: imageCompressModal },
      { label: '🎬 视频压缩', title: '把当前目录里的视频压小（产物写进 output/；绝不越压越大）', run: videoCompressModal },
      { label: '🧹 去广告（无损）', title: '彻底清理：清空全局标签与轨道标题、删非字体附件与多余视频流；不重编码，默认直接替换原文件', run: mediaCleanModal },
      { label: '📦 打包压缩' + (selection.size ? `（${selection.size} 项）` : ''), title: '把选中的项打成 zip/tar 归档', disabled: selection.size === 0, run: compressSelected },
    ];
  }

  function renderToolbar() {
    clear(toolbar);
    const selCount = selection.size;
    const clip = clipboard && clipboard.paths.length ? clipboard : null;
    const favCount = favList().length;
    appendAll(toolbar, 
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(cwd) }),
      h('button.btn.btn-sm', {
        text: '🌐 www',
        title: '一键回到网站根目录',
        onclick: () => { const w = wwwRoot(); if (w) load(w); },
      }),
      // 收藏只有这一颗：点它打开收藏列表。加入/取消收藏在行右键与「更多」菜单里。
      h('button.btn.btn-sm', {
        text: (isFavorite(cwd) ? '★ ' : '') + '收藏' + (favCount ? '（' + favCount + '）' : ''),
        title: '打开收藏列表（加入/取消收藏在右键或「更多」菜单）',
        onclick: favoritesModal,
      }),
      // 上传只有一颗（用户要求去掉那个 ▾）：点它直接打开上传面板，
      // 面板里本来就有「选择文件 / 选择文件夹」，再挂一个二级菜单是多余的。
      h('button.btn.btn-sm', {
        text: '⬆ 上传',
        title: '打开上传面板（可选文件/文件夹，也可把文件拖进面板）',
        onclick: uploadPanel,
      }),
      h('button.btn.btn-sm', {
        text: '＋ 新建',
        title: '新建文件夹 / 新建文件',
        onclick: (ev) => toggleDropdown(ev.currentTarget, newMenuItems()),
      }),
      h('label', {
        style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px', cursor: 'pointer' },
        title: '只影响列表显示，不是安全边界：知道完整路径仍可直接访问（服务端按可访问范围校验）',
      }, [
        h('input', {
          type: 'checkbox', checked: showHidden,
          onchange: (e) => {
            showHidden = e.target.checked;
            try { localStorage.setItem(SHOW_HIDDEN_KEY, showHidden ? '1' : '0'); } catch { /* 存不了就本次生效 */ }
            load(cwd);
          },
        }),
        h('span', { text: '显示隐藏' }),
      ]),
      h('div', { style: { flex: 1 } }),
      // 选中相关控件**始终占位**（空选时隐藏/禁用）：一旦让工具栏因选中而换行，
      // 列表整体下移，双击的第二下会落到别的行上（1280 宽实测打开了错误的文件）。
      h('span.pill.brand', {
        text: selCount ? `已选 ${selCount} 项` : '已选 0 项',
        style: { minWidth: '78px', justifyContent: 'center', visibility: selCount ? '' : 'hidden' },
      }),
      // 剪贴板状态与粘贴入口：用户按了 Ctrl+C/X 之后要能一眼看到"剪贴板里有什么"。
      clip ? h('button.btn.btn-sm', {
        text: `📋 粘贴 ${clip.paths.length} 项${clip.mode === 'cut' ? '（剪切）' : '（复制）'}`,
        title: '粘贴到当前目录（Ctrl+V）',
        onclick: pasteClipboard,
      }) : null,
      // 魔法箱在右侧（原来三颗压缩按钮的位置）。
      h('button.btn.btn-sm', {
        text: '魔法箱 ▾',
        title: '图片压缩 / 视频压缩 / 去广告（无损）/ 打包压缩',
        onclick: (ev) => toggleDropdown(ev.currentTarget, compressMenuItems()),
      }),
      h('button.btn.btn-sm.btn-danger', { text: '删除', disabled: selCount === 0, onclick: deleteSelected }),
      h('button.btn.btn-sm', { text: '🔍 搜索', onclick: searchModal }),
    );
  }

  // ---------- 目录收藏 ----------
  //
  // 存服务端（settings KV）而不是 localStorage：手机上打开面板也要看得到同一份。
  // "已收藏"标记只信服务端列表（没拉到就不显示 ★，绝不猜）。
  function favList() {
    return (favorites && favorites.items) || [];
  }

  function isFavorite(p) {
    return !!p && favList().some((it) => it.path === p);
  }

  async function refreshFavorites() {
    try {
      favorites = await api.fileFavorites();
    } catch {
      // 拉不到就保留上一次的结论（没有就当作空），绝不让工具栏卡住不渲染。
      if (!favorites) favorites = { items: [], missing: 0 };
    }
    renderToolbar();
  }

  // setFavorite 是**唯一**的收藏写入口（加/移除都走它，成功后用服务端返回的
  // 最新列表覆盖本地状态并重画工具栏）。
  async function setFavorite(action, path) {
    try {
      favorites = await api.fileFavoriteSet(action, path);
      renderToolbar();
      return true;
    } catch (e) {
      toast((action === 'add' ? '收藏失败：' : '移除失败：') + ((e && e.message) || e), 'err', 10000);
      return false;
    }
  }

  // toggleFavorite 是收藏写入口，收/放传路径（工具栏那颗只负责打开列表）。
  async function toggleFavorite(p) {
    if (!p) return;
    const on = isFavorite(p);
    if (await setFavorite(on ? 'remove' : 'add', p)) {
      toast(on ? '已取消收藏' : '已收藏（手机上也能看到）', 'ok');
    }
  }

  // favoritesModal 是收藏夹：点「跳转」直接进目录；目录不在了就**如实**标出来
  // （"目录不存在"），并且照样允许移除 —— 绝不静默消失、也绝不假装能跳。
  function favoritesModal() {
    const body = h('div');
    const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end' } });
    const m = modal({ title: '⭐ 收藏夹', body, footer: [foot] });

    function draw() {
      clear(body);
      clear(foot);
      const items = favList();
      if (!items.length) {
        body.append(h('div.hint', { text: '还没有收藏：在文件夹上右键或用「更多」菜单 →「加入收藏」。' }));
        foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
        return;
      }
      for (const it of items) {
        body.append(h('div', {
          style: {
            display: 'flex', gap: '8px', alignItems: 'center', padding: '7px 0',
            borderBottom: '1px solid var(--border-soft)',
          },
        }, [
          h('div', { style: { flex: '1', minWidth: '0' } }, [
            h('div', { text: (it.exists ? '📁 ' : '⚠️ ') + it.name, style: { fontWeight: '550' } }),
            h('div.hint', { text: it.path, style: { wordBreak: 'break-all' } }),
            it.exists ? null : h('div.hint', {
              text: it.reason || '目录不存在',
              style: { color: 'var(--warn)' },
            }),
          ]),
          it.exists
            ? h('button.btn.btn-sm', {
              text: '跳转',
              onclick: () => { m.close(); load(it.path); },
            })
            : h('button.btn.btn-sm', {
              text: '无法跳转', disabled: true,
              title: it.reason || '目录不存在',
            }),
          h('button.btn.btn-sm', {
            text: '移除',
            onclick: async () => { if (await setFavorite('remove', it.path)) draw(); },
          }),
        ]));
      }
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
    }
    draw();
  }

  // 文件名不是"打开"按钮：单击只选中，双击才打开（handleRowOpen）。
  // 文件分支绝不能调 openAny —— 否则第一次点击就弹窗，第二次点击落在遮罩上把它关掉，
  // 用户看到的就是"双击没反应"。目录保留"单击进入"这条既有路径。
  function nameCell(e) {
    return h('div', { style: { display: 'flex', alignItems: 'center', gap: '7px' } }, [
      h('span', { text: e.is_dir ? '📁' : fileIcon(e.name), style: { fontSize: '15px' } }),
      e.is_dir
        ? h('a', {
          href: 'javascript:void(0)', style: { fontWeight: '550' }, text: e.name,
          title: '单击进入目录',
          onclick: (ev) => { ev.stopPropagation(); load(e.path); },
        })
        : h('a', {
          href: 'javascript:void(0)', text: e.name,
          title: '双击' + openLabel(e) + '（单击只选中）',
        }),
      e.symlink ? h('span.pill' + (e.symlink_broken ? '.warn' : ''), {
        text: e.symlink_broken ? '链接失效' : '链接',
        title: e.symlink_broken
          ? '链接已失效（目标不存在）：' + (e.symlink_target || '?')
          : '符号链接 → ' + (e.symlink_target || '?'),
      }) : null,
      e.read_only ? h('span.pill.warn', { text: '只读' }) : null,
      e.sensitive ? h('span.pill.danger', {
        text: '敏感',
        title: '面板数据目录（数据库与凭据）。仍可读写，但覆盖/删除前会二次确认。',
      }) : null,
    ]);
  }

  // 行单击：只改选中态，**不整表重绘**。重绘会让 dblclick 的第二次点击落到新节点上，
  // 浏览器就不发 dblclick 了 —— 这是"双击打不开"的直接原因。
  function handleRowClick(e, idx, ev) {
    if (ev.target && ev.target.closest('button, input')) return;
    if (ev.shiftKey && selAnchor >= 0) {
      selectRange(selAnchor, idx);
    } else if (ev.ctrlKey || ev.metaKey) {
      if (selection.has(e.path)) selection.delete(e.path); else selection.add(e.path);
      selAnchor = idx;
    } else {
      selection = new Set([e.path]);
      selAnchor = idx;
    }
    syncSelectionUI();
    renderToolbar();
  }

  // 行双击：文件走 openAny（图片查看器 / 播放器 / 编辑器同一入口），目录进入。
  function handleRowOpen(e) {
    if (e.is_dir) load(e.path); else openAny(e);
  }

  // 选中态原地同步（行 class/底色 + 复选框），替代整表重绘。
  function syncSelectionUI() {
    rowEls.forEach((tr, i) => {
      const e = visibleEntries[i];
      if (!tr || !e) return;
      const on = selection.has(e.path);
      tr.classList.toggle('zp-row-selected', on);
      tr.style.background = on ? 'var(--brand-soft)' : '';
      const cb = tr.querySelector('input[type=checkbox]');
      if (cb) cb.checked = on;
    });
    if (headCheckbox) {
      headCheckbox.checked = visibleEntries.length > 0 && visibleEntries.every((e) => selection.has(e.path));
    }
  }

  // sizeCell 生成「大小」单元格：文件行原样显示，目录行是可点的按需计算。
  function sizeCell(e) {
    const td = h('td.num');
    fillSizeCell(td, e);
    return td;
  }

  // ---------- 目录大小：按需计算 ----------
  //
  // 文件行的大小原样显示；目录行不再显示"—"，而是可点的「计算」
  // （宝塔同款：递归统计很贵，绝不进列表请求）。缓存策略见 dirSizeCache。
  function fillSizeCell(td, e) {
    if (!e.is_dir) { td.textContent = humanSize(e.size); return; }
    clear(td);
    const st = dirSizeCache.get(e.path);
    const btn = (cls, text, title) => h('button.zpf-dirsize' + (cls ? '.' + cls : ''), {
      type: 'button', text, title,
      onclick: (ev) => { ev.stopPropagation(); startDirSize(e, td); },
      ondblclick: (ev) => ev.stopPropagation(), // 双击不该顺手把目录打开
    });
    if (!st || st.state === 'idle') {
      td.appendChild(btn('', '计算', '统计这个文件夹的递归大小（不跟随符号链接）'));
      return;
    }
    if (st.state === 'running') {
      td.appendChild(h('span.zpf-dirsize.is-run', { text: '计算中…' }));
      return;
    }
    if (st.state === 'error') {
      td.appendChild(btn('is-err', '计算失败', st.message || '计算失败'));
      return;
    }
    // 没统计完必须带标记：一个看起来精确的数字会骗人
    const text = humanSize(st.bytes) + (st.truncated ? '+（未统计完）' : '');
    const bits = [st.files + ' 个文件', st.dirs + ' 个目录'];
    if (st.truncated) bits.push(st.reason || '超出预算，没统计完');
    if (st.skipped) bits.push('跳过 ' + st.skipped + ' 项（读不到）');
    if (st.symlinks) bits.push('跳过 ' + st.symlinks + ' 个符号链接');
    bits.push('用时 ' + (st.ms / 1000).toFixed(2) + 's', '点击重新计算');
    td.appendChild(btn('is-done', text, bits.join(' · ')));
  }

  async function startDirSize(e, td) {
    if ((dirSizeCache.get(e.path) || {}).state === 'running') return;
    dirSizeCache.set(e.path, { state: 'running' });
    fillSizeCell(td, e);
    try {
      const r = await api.fileDirSize(e.path);
      dirSizeCache.set(e.path, {
        state: 'done',
        bytes: (r && r.bytes) || 0, files: (r && r.files) || 0, dirs: (r && r.dirs) || 0,
        skipped: (r && r.skipped) || 0, symlinks: (r && r.symlinks) || 0,
        truncated: !!(r && r.truncated), reason: (r && r.reason) || '', ms: (r && r.ms) || 0,
      });
    } catch (err) {
      dirSizeCache.set(e.path, { state: 'error', message: (err && err.message) || String(err) });
    }
    // 等待期间表格可能已重绘（td 已脱离文档）：缓存已写好，下次渲染自然显示。
    if (td.isConnected) fillSizeCell(td, e);
  }

  function renderTable() {
    clear(tableBox);
    const list = lastList?.entries || [];
    if (!list.length) {
      visibleEntries = [];
      rowEls = [];
      headCheckbox = null;
      appendAll(tableBox, h('div.empty', [
        h('div.big', { text: '📂' }),
        h('h4', { text: '这个目录是空的' }),
        h('p', { text: '可以用上方按钮上传文件或新建内容。' }),
      ]));
      updateStatus();
      return;
    }

    const ordered = sortedEntries(list);
    visibleEntries = ordered;
    rowEls = [];
    const allChecked = ordered.length > 0 && ordered.every((e) => selection.has(e.path));
    headCheckbox = h('input', {
      type: 'checkbox', checked: allChecked,
      title: '全选 / 取消全选（Ctrl+A）',
      onchange: (e) => {
        selection.clear();
        if (e.target.checked) ordered.forEach((x) => selection.add(x.path));
        selAnchor = -1;
        renderToolbar(); renderTable();
      },
    });
    const head = h('tr', [
      h('th', { style: { width: '34px' } }, [headCheckbox]),
      sortHeader('name', '名称'),
      sortHeader('size', '大小'),
      sortHeader('type', '类型'),
      h('th', { text: '权限' }),
      h('th', { text: '属主' }),
      sortHeader('time', '修改时间'),
      h('th', { text: '操作' }),
    ]);

    const rows = ordered.map((e, idx) => {
      const selected = selection.has(e.path);
      const tr = h('tr', {
        class: selected ? 'zp-row-selected' : '',
        style: selected ? { background: 'var(--brand-soft)' } : {},
        // 单击 = 选择（Ctrl/⌘ 切换、Shift 范围），双击 = 打开（handleRowOpen）。
        // 单击处理器只原地切选中态：整表重绘会让第二次点击落到新节点上，dblclick 永不触发。
        onclick: (ev) => handleRowClick(e, idx, ev),
        // 右键：文件/文件夹行的上下文菜单（按类型禁用不适用项）
        oncontextmenu: (ev) => {
          ev.preventDefault();
          if (!selection.has(e.path)) { selection = new Set([e.path]); selAnchor = idx; renderToolbar(); syncSelectionUI(); }
          rowContextMenu(ev.clientX, ev.clientY, e);
        },
        ondblclick: (ev) => {
          if (ev.target && ev.target.closest('input')) return; // 别抢勾选框
          handleRowOpen(e);
        },
      }, [
        h('td', [
          h('input', {
            type: 'checkbox', checked: selected,
            onchange: (ev) => {
              if (ev.target.checked) selection.add(e.path); else selection.delete(e.path);
              selAnchor = idx;
              renderToolbar(); renderTable();
            },
          }),
        ]),
        h('td', [nameCell(e)]),
        sizeCell(e),
        h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: typeLabel(e) }),
        h('td.mono', { style: { fontSize: '11.5px' }, text: String(e.mode_num.toString(8)).padStart(3, '0') }),
        h('td', { style: { fontSize: '11.5px' }, text: e.owner || '—' }),
        h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: e.mod_time }),
        h('td', [
          // 「操作」列只有这一颗「更多」：打开/下载/重命名/… 全在菜单里（与行右键同一份内容）。
          h('button.btn.btn-sm', { text: '更多', title: '更多操作（与行右键菜单同一份内容）', onclick: () => rowMoreMenu(e) }),
        ]),
      ]);
      rowEls.push(tr);
      return tr;
    });

    appendAll(tableBox, h('table.table', [h('thead', [head]), h('tbody', rows)]));
    // 点空白处取消选择 / 右键空白处出菜单：挂在 tableBox 的**外层**（.card-body），
    // 因为表格本身会把 tableBox 撑满，表格下方/右侧的留白属于外层容器。
    const blankHost = tableBox.parentElement || tableBox;
    blankHost.onclick = (ev) => {
      if (ev.target && ev.target.closest('tr')) return;
      if (!selection.size) return;
      selection = new Set();
      selAnchor = -1;
      renderToolbar(); renderTable();
    };
    blankHost.oncontextmenu = (ev) => {
      if (ev.target && ev.target.closest('tr')) return; // 行菜单优先
      ev.preventDefault();
      blankContextMenu(ev.clientX, ev.clientY);
    };
    updateStatus();
  }

  // selectRange 把 [a,b] 区间加入选择（按当前可见顺序）。
  function selectRange(a, b) {
    const lo = Math.min(a, b), hi = Math.max(a, b);
    const next = new Set();
    for (let i = lo; i <= hi && i < visibleEntries.length; i++) next.add(visibleEntries[i].path);
    selection = next;
  }

  // ---------- 右键菜单 ----------
  //
  // 自己起一层 .zp-ctx-menu（而不是 modal）：右键菜单要贴着鼠标、点别处就消失。
  // 工具栏的「上传 ▾ / 新建 / 魔法箱」下拉也复用它（见 toggleDropdown）。
  let openCtxMenu = null;
  let dropdownAnchor = null;

  function closeContextMenu() {
    if (openCtxMenu) { openCtxMenu.remove(); openCtxMenu = null; }
    dropdownAnchor = null;
  }

  function showContextMenu(x, y, items) {
    closeContextMenu();
    const menu = h('div.zp-ctx-menu');
    for (const it of items) {
      if (!it) continue;
      if (it.sep) { menu.appendChild(h('div.zp-ctx-sep')); continue; }
      if (it.disabled) {
        menu.appendChild(h('div.zp-ctx-item.zp-ctx-disabled', { text: it.label, title: it.title || '当前选择不适用' }));
        continue;
      }
      menu.appendChild(h('button.zp-ctx-item' + (it.danger ? '.zp-ctx-danger' : ''), {
        type: 'button', text: it.label, title: it.title || '',
        onclick: () => { closeContextMenu(); try { it.run(); } catch (e) { toast('操作失败：' + ((e && e.message) || e), 'err'); } },
      }));
    }
    menu.style.visibility = 'hidden';
    document.body.appendChild(menu);
    const r = menu.getBoundingClientRect();
    menu.style.left = Math.max(6, Math.min(x, window.innerWidth - r.width - 6)) + 'px';
    menu.style.top = Math.max(6, Math.min(y, window.innerHeight - r.height - 6)) + 'px';
    menu.style.visibility = '';
    openCtxMenu = menu;
  }

  // toggleDropdown 把工具栏按钮当锚点：菜单贴在按钮下方，内容与右键菜单同一个渲染器。
  function toggleDropdown(anchor, items) {
    if (openCtxMenu && dropdownAnchor === anchor) { closeContextMenu(); return; }
    const r = anchor.getBoundingClientRect();
    showContextMenu(r.left, r.bottom + 4, items);
    dropdownAnchor = anchor;
  }

  // menuPaths 是行操作的选中集合：右键的那一行若在多选里就整批操作，否则只操作它。
  function menuPaths(e) {
    return selection.has(e.path) && selection.size > 1 ? [...selection] : [e.path];
  }

  // rowMenuItems 是**行操作菜单的唯一来源**：行右键与「操作」列的「更多」都调它。
  // 只允许存在这一处定义（门禁 TestFilesRowMenuSingleSourceGate 盯着）。
  function rowMenuItems(e, paths) {
    const multi = paths.length > 1;
    const archive = !e.is_dir && /\.(zip|tar\.gz|tgz|tar)$/i.test(e.name);
    const hasDirs = (lastList?.entries || []).some((x) => selection.has(x.path) && x.is_dir);
    return [
      { label: openLabel(e), run: () => (e.is_dir ? load(e.path) : openAny(e)) },
      // 媒体不走文本编辑器（点开就是乱码/二进制提示），只留播放与下载。
      !e.is_dir && !isImage(e) && !mediaKind(e) ? { label: '用编辑器打开', run: () => openEditor(e) } : null,
      { label: '下载', title: '下载到本机（多选时请逐个下载）', disabled: e.is_dir || multi, run: () => { window.location.href = api.fileDownloadURL(e.path); } },
      archive ? { label: '解压到当前目录', title: '大压缩包会在任务中心里跑，并逐条显示进度', run: () => extractEntry(e) } : null,
      { sep: true },
      { label: '重命名' + (multi ? '（仅单项）' : ''), disabled: multi, run: () => renameEntry(e) },
      { label: '批量改名' + (multi ? `（${paths.length} 项）` : ''), title: `对选中的 ${paths.length} 项批量改名`, run: () => batchRename(paths) },
      { label: '复制到…', disabled: multi, run: () => copyEntryTo(e) },
      { label: '移动到…', disabled: multi, run: () => moveEntryTo(e) },
      { label: '复制（Ctrl+C）', run: () => copySelection(paths) },
      { label: '剪切（Ctrl+X）', run: () => cutSelection(paths) },
      {
        label: '粘贴（Ctrl+V）',
        disabled: !(clipboard && clipboard.paths.length),
        run: pasteClipboard,
      },
      { label: '压缩…', run: () => { if (!selection.has(e.path)) selection = new Set([e.path]); compressSelected(); } },
      { label: '权限…', title: '修改该文件/目录的读/写/执行权限（宝塔式勾选界面）', disabled: multi, run: () => chmodModal(e) },
      { label: isFavorite(e.path) ? '取消收藏' : '加入收藏', title: '收藏存服务端，手机上也能看到', run: () => toggleFavorite(e.path) },
      { sep: true },
      { label: '复制完整路径', run: () => copyFullPath(e.path) },
      { label: '删除' + (multi ? ` ${paths.length} 项` : ''), danger: true, run: () => (multi || hasDirs || e.is_dir ? deleteSelectionOrOne(e, paths) : deleteOne(e)) },
    ];
  }

  // rowContextMenu 是文件/文件夹行的右键菜单（内容来自 rowMenuItems）。
  function rowContextMenu(x, y, e) {
    showContextMenu(x, y, rowMenuItems(e, menuPaths(e)));
  }

  // blankContextMenu 是空白处的右键菜单。
  function blankContextMenu(x, y) {
    showContextMenu(x, y, [
      { label: '刷新', run: () => load(cwd) },
      { sep: true },
      { label: '新建文件夹…', run: () => toolbarNew('dir') },
      { label: '新建文件…', run: () => toolbarNew('file') },
      { label: '上传文件…', run: () => pickUploadFiles() },
      { label: '上传文件夹…', run: () => pickUploadFolder() },
      { sep: true },
      { label: '粘贴（Ctrl+V）', disabled: !(clipboard && clipboard.paths.length), run: pasteClipboard },
      { label: `全选（${visibleEntries.length} 项）`, disabled: !visibleEntries.length, run: selectAll },
      { label: showHidden ? '隐藏点文件' : '显示隐藏文件', run: () => toggleHidden() },
    ]);
  }

  function updateStatus() {
    const l = lastList;
    if (!l) { statusBar.textContent = ''; return; }
    const dirs = l.entries.filter((e) => e.is_dir).length;
    const files = l.entries.length - dirs;
    statusBar.textContent = `共 ${l.total} 项（${dirs} 个目录，${files} 个文件）` +
      (l.truncated ? ' — 条目过多已截断显示' : '') +
      `　路径：${l.path}${l.writable ? '' : '（当前不可写）'}`;
  }

  // copyEntryTo / moveEntryTo 是「复制到…」「移动到…」的唯一实现（右键与「更多」共用）。
  async function copyEntryTo(e) {
    const dest = await promptBox({
      title: '复制到', label: '目标完整路径', value: e.path + '-copy',
      hint: '必须是允许访问的目录内的绝对路径',
    });
    if (!dest) return;
    // 复制是长任务（可能几 GB）；同目录内复制也要有进度可见。
    await startFileOp({
      kind: 'file_copy', title: `复制到 ${basename(dest)}`,
      run: () => api.fileCopy([{ from: e.path, to: dest }]),
    });
  }

  async function moveEntryTo(e) {
    const dest = await promptBox({ title: '移动到', label: '目标完整路径', value: e.path, hint: '会重命名或移动到该位置' });
    if (!dest || dest === e.path) return;
    try {
      await api.fileRename(e.path, dest);
      toast('已移动', 'ok'); load(cwd);
    } catch (err) { toast(err.message, 'err'); }
  }

  // rowMoreMenu 是「操作」列那颗「更多」：同一份 rowMenuItems，外加这个条目的元信息。
  // （两个入口一份内容 —— 改菜单项只需改 rowMenuItems 一处。）
  function rowMoreMenu(e) {
    const paths = menuPaths(e);
    const buttons = rowMenuItems(e, paths).filter(Boolean).map((it) => {
      if (it.sep) return h('div', { style: { height: '1px', margin: '5px 2px', background: 'var(--border-soft)' } });
      if (it.disabled) {
        return h('button.btn.btn-block', { text: it.label, disabled: true, title: it.title || '当前选择不适用' });
      }
      return h('button.btn.btn-block' + (it.danger ? '.btn-danger' : ''), {
        text: it.label, title: it.title || '',
        onclick: () => {
          m.close();
          try { it.run(); } catch (err) { toast('操作失败：' + ((err && err.message) || err), 'err'); }
        },
      });
    });

    const m = modal({
      title: e.name,
      body: h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } }, [
        h('dl.kv', [
          h('dt', { text: '完整路径' }), h('dd', { text: e.path }),
          h('dt', { text: '类型' }), h('dd', {
            text: e.symlink ? (e.symlink_broken ? '符号链接（已失效）' : '符号链接') : (e.is_dir ? '目录' : '文件'),
          }),
          h('dt', { text: '权限' }), h('dd', { text: e.mode + '（' + String(e.mode_num.toString(8)).padStart(3, '0') + '）' }),
          h('dt', { text: '属主' }), h('dd', { text: e.owner }),
          h('dt', { text: '大小' }), h('dd', { text: e.is_dir ? '—' : `${humanSize(e.size)}（${e.size} 字节）` }),
          h('dt', { text: '修改时间' }), h('dd', { text: e.mod_time }),
          e.symlink ? h('dt', { text: '指向' }) : null,
          e.symlink ? h('dd', {
            text: (e.symlink_target || '?') + (e.symlink_broken ? '（目标不存在）' : ''),
            style: e.symlink_broken ? { color: 'var(--warn)' } : null,
          }) : null,
        ]),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '6px', marginTop: '10px' } }, buttons),
      ]),
    });
  }

  // ---------- 单项/批量动作（右键菜单、快捷键、工具条共用） ----------
  //
  // 这些函数都定义在 FilesView 内部（闭包），所以它们始终读写**当前**这个视图的
  // cwd / lastList / selection；右键菜单与 Ctrl+C/X/V 调用的就是同一套实现。

  async function renameEntry(e) {
    const name = await promptBox({ title: '重命名', label: '新名称', value: e.name });
    if (!name || name === e.name) return;
    try {
      await api.fileRename(e.path, `${dirname(e.path)}/${name}`);
      toast('已重命名', 'ok'); load(cwd);
    } catch (err) { toast(err.message, 'err'); }
  }

  // ---------- 批量改名 ----------
  //
  // 规则引擎只有后端一份（internal/rename）：预览与执行都调它，前端**不自己算新名**。
  // 入口只有 rowMenuItems 里的「批量改名」（右键与「更多」共用同一份）。

  const RENAME_PRESETS = [
    { label: '第二十集 → 第20集', rules: [{ type: 'cnnum', scope: 'episode' }] },
    { label: '第二十集 → EP20', rules: [{ type: 'episode', prefix: 'EP', width: 0 }] },
    { label: 'EP20 → EP020（补零 3 位）', rules: [{ type: 'episode', prefix: 'EP', width: 3 }] },
    { label: '删除 [组名] 标签', rules: [{ type: 'replace', mode: 'regex', find: '\\[[^\\]]*\\]\\s*', replace: '' }] },
    { label: '空格转下划线', rules: [{ type: 'replace', mode: 'literal', find: ' ', replace: '_' }] },
  ];
  const RENAME_TYPES = [
    ['replace', '替换 / 删除文本'], ['insert', '插入文本'], ['delete', '删除匹配'],
    ['cnnum', '中文数字 → 数字'], ['episode', '集数 → EP'],
  ];
  const RENAME_MODES = [['literal', '字面'], ['wildcard', '通配符 * ?'], ['regex', '正则']];
  const RENAME_POSITIONS = [['prefix', '最前'], ['suffix', '最后'], ['after', '第 N 个字符后']];
  const RENAME_HINT = { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '4px', lineHeight: '1.5' };
  const RENAME_DIFF = { background: 'rgba(245,158,11,.30)', borderRadius: '3px' };

  // renameDefaultRule 返回某类型的初始参数（切换类型时重置）。
  function renameDefaultRule(type) {
    if (type === 'insert') return { type: 'insert', position: 'prefix', at: 0, text: '' };
    if (type === 'delete') return { type: 'delete', mode: 'literal', find: '' };
    if (type === 'cnnum') return { type: 'cnnum', scope: 'episode' };
    if (type === 'episode') return { type: 'episode', prefix: 'EP', width: 0 };
    return { type: 'replace', mode: 'literal', find: '', replace: '' };
  }

  // renameDiff 返回 [公共前缀, 原名中段, 新名中段, 公共后缀]，只标真正改动的部分。
  function renameDiff(a, b) {
    if (a === b) return [a, '', '', ''];
    let i = 0;
    while (i < a.length && i < b.length && a[i] === b[i]) i += 1;
    let j = 0;
    while (j < a.length - i && j < b.length - i && a[a.length - 1 - j] === b[b.length - 1 - j]) j += 1;
    return [a.slice(0, i), a.slice(i, a.length - j), b.slice(i, b.length - j), a.slice(a.length - j)];
  }

  function batchRename(paths) {
    const names = [...paths].map((p) => basename(p));
    if (!names.length) return;

    let rules = [];
    let plan = null;
    let timer = null;
    let seq = 0;
    let applying = false;

    const presetSel = h('select.select', [h('option', { value: '', text: '选择预设…' })].concat(
      RENAME_PRESETS.map((p, i) => h('option', { value: String(i), text: p.label }))));
    const includeExt = h('input', { type: 'checkbox' });
    const rulesBox = h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } });
    const previewBox = h('div');
    const statusHint = h('div', { style: { ...RENAME_HINT, marginTop: '8px' } });
    const applyBtn = h('button.btn.btn-primary', { text: '应用', disabled: true });

    function payload() {
      return { dir: cwd, names, rules, options: { include_ext: !!includeExt.checked } };
    }

    function field(labelText, control) {
      return h('div.field', [h('label', { text: labelText }), control]);
    }

    function renderRules() {
      clear(rulesBox);
      if (!rules.length) {
        rulesBox.appendChild(h('div', { style: RENAME_HINT, text: '还没有规则：选一个预设，或点下面「加一条规则」。' }));
      }
      rules.forEach((r, i) => rulesBox.appendChild(ruleCard(r, i)));
      schedulePreview();
    }

    function ruleCard(r, i) {
      const typeSel = h('select.select', { style: { maxWidth: '180px' } },
        RENAME_TYPES.map(([v, t]) => h('option', { value: v, text: t, selected: r.type === v })));
      typeSel.addEventListener('change', () => { rules[i] = renameDefaultRule(typeSel.value); renderRules(); });
      const move = (d) => {
        const j = i + d;
        if (j < 0 || j >= rules.length) return;
        const tmp = rules[i]; rules[i] = rules[j]; rules[j] = tmp;
        renderRules();
      };
      const head = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span', { text: '规则 ' + (i + 1), style: { fontSize: '12px', color: 'var(--text-mute)' } }),
        typeSel,
        h('div', { style: { flex: '1' } }),
        h('button.btn.btn-sm', { text: '↑', title: '上移', onclick: () => move(-1) }),
        h('button.btn.btn-sm', { text: '↓', title: '下移', onclick: () => move(1) }),
        h('button.btn.btn-sm.btn-danger', { text: '✕', title: '删除这条规则', onclick: () => { rules.splice(i, 1); renderRules(); } }),
      ]);

      const textInput = (value, placeholder, onChange) => {
        const inp = h('input.input', { value, placeholder });
        inp.addEventListener('input', () => { onChange(inp.value); schedulePreview(); });
        return inp;
      };
      const cell = (labelText, control) =>
        h('div', { style: { flex: '1 1 160px', minWidth: '0' } }, [h('label', { style: RENAME_HINT, text: labelText }), control]);
      const body = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', marginTop: '8px', minWidth: '0' } });

      if (r.type === 'replace' || r.type === 'delete') {
        const modeSel = h('select.select', RENAME_MODES.map(([v, t]) =>
          h('option', { value: v, text: t, selected: (r.mode || 'literal') === v })));
        modeSel.addEventListener('change', () => { r.mode = modeSel.value; schedulePreview(); });
        body.appendChild(cell('匹配方式', modeSel));
        body.appendChild(cell('查找', textInput(r.find || '', r.mode === 'regex' ? '如 第([0-9]+)集' : '如 第二十集', (v) => { r.find = v; })));
        if (r.type === 'replace') {
          body.appendChild(cell('替换为（留空 = 删除；$1 引用捕获）', textInput(r.replace || '', '留空即删除', (v) => { r.replace = v; })));
        }
      } else if (r.type === 'insert') {
        const posSel = h('select.select', RENAME_POSITIONS.map(([v, t]) =>
          h('option', { value: v, text: t, selected: (r.position || 'prefix') === v })));
        posSel.addEventListener('change', () => { r.position = posSel.value; renderRules(); });
        body.appendChild(cell('位置', posSel));
        if ((r.position || 'prefix') === 'after') {
          const atInput = h('input.input', { type: 'number', value: String(r.at || 0), min: '0' });
          atInput.addEventListener('input', () => { r.at = Number(atInput.value) || 0; schedulePreview(); });
          body.appendChild(cell('第几个字符后（中文按字算）', atInput));
        }
        body.appendChild(cell('插入的文本', textInput(r.text || '', '如 _', (v) => { r.text = v; })));
      } else if (r.type === 'cnnum') {
        const scopeSel = h('select.select', [
          h('option', { value: 'episode', text: '只在集数语境（推荐）', selected: (r.scope || 'episode') === 'episode' }),
          h('option', { value: 'all', text: '整个名字', selected: r.scope === 'all' }),
        ]);
        scopeSel.addEventListener('change', () => { r.scope = scopeSel.value; schedulePreview(); });
        body.appendChild(cell('替换范围', scopeSel));
        body.appendChild(h('div', { style: { ...RENAME_HINT, flex: '2 1 220px' }, text: '集数语境 = 第X集/话/期/回/部/季/卷/章、EPx、Ex。' }));
      } else if (r.type === 'episode') {
        const widthSel = h('select.select', [0, 1, 2, 3].map((w) =>
          h('option', { value: String(w), text: w <= 1 ? '不补零' : '补零到 ' + w + ' 位', selected: Number(r.width || 0) === w })));
        widthSel.addEventListener('change', () => { r.width = Number(widthSel.value); schedulePreview(); });
        body.appendChild(cell('前缀', textInput(r.prefix || '', 'EP', (v) => { r.prefix = v; })));
        body.appendChild(cell('数字宽度', widthSel));
      }
      return h('div', {
        style: { border: '1px solid var(--border-soft)', borderRadius: '8px', padding: '8px 10px', minWidth: '0' },
      }, [head, body]);
    }

    function schedulePreview() {
      if (timer) clearTimeout(timer);
      timer = setTimeout(refreshPlan, 220);
    }

    async function refreshPlan() {
      timer = null;
      if (!rules.length) {
        plan = null;
        renderPreview('还没有规则。');
        return;
      }
      const mine = ++seq;
      let res;
      try {
        res = await api.fileRenamePlan(payload());
      } catch (e) {
        if (mine !== seq) return;
        plan = null;
        renderPreview('预览失败：' + ((e && e.message) || e));
        return;
      }
      if (mine !== seq) return;
      plan = res;
      renderPreview('');
    }

    function previewCell(it) {
      const bad = it.status === 'conflict' || it.status === 'invalid';
      const [, , newMid, post] = renameDiff(it.name, it.new_name);
      const newPre = it.new_name.slice(0, it.new_name.length - newMid.length - post.length);
      const label = it.status === 'ok' ? '将改名'
        : it.status === 'unchanged' ? '无变化'
          : (it.status === 'conflict' ? '冲突：' : '非法：') + (it.reason || '');
      const style = bad ? { color: 'var(--danger)' } : (it.status === 'unchanged' ? { color: 'var(--text-mute)' } : {});
      return h('tr', { style: bad ? { background: 'rgba(239,68,68,.10)' } : {} }, [
        h('td.zp-plan-name', { text: it.name }),
        h('td.zp-plan-name', [
          h('span', { text: newPre }),
          newMid ? h('span', { style: RENAME_DIFF, text: newMid }) : null,
          h('span', { text: post }),
        ]),
        h('td', { text: label, style }),
      ]);
    }

    function renderPreview(errText) {
      clear(previewBox);
      if (errText) {
        previewBox.appendChild(h('div', { style: RENAME_HINT, text: errText }));
        statusHint.style.color = '';
        statusHint.textContent = errText;
        applyBtn.disabled = true;
        return;
      }
      if (!plan) { applyBtn.disabled = true; return; }
      const rows = (plan.items || []).map(previewCell);
      previewBox.appendChild(h('div.zp-plan-scroll', [
        h('table.table', { style: { fontSize: '12px' } }, [
          h('thead', [h('tr', ['原名', '新名', '状态'].map((t) => h('th', { text: t })))]),
          h('tbody', rows.length ? rows : [h('tr', [h('td', { colspan: '3', text: '没有条目' })])]),
        ]),
      ]));
      const blocked = (plan.conflict || 0) + (plan.invalid || 0);
      applyBtn.disabled = applying || blocked > 0 || !plan.ok;
      if (blocked > 0) {
        statusHint.style.color = 'var(--danger)';
        statusHint.textContent = `有 ${plan.conflict} 项冲突、${plan.invalid} 项不合法：先改规则再应用`;
      } else if (!plan.ok) {
        statusHint.style.color = '';
        statusHint.textContent = '没有需要改名的项。';
      } else {
        statusHint.style.color = '';
        statusHint.textContent = `将改名 ${plan.ok} 项，${plan.unchanged} 项无变化。`;
      }
    }

    function showResult(result) {
      clear(body);
      const rows = ((result && result.items) || []).map((it) => {
        const bad = it.status === 'failed';
        const skipped = it.status === 'skipped';
        return h('tr', { style: bad ? { background: 'rgba(239,68,68,.10)' } : {} }, [
          h('td.zp-plan-name', { text: it.name }),
          h('td.zp-plan-name', { text: it.new_name }),
          h('td', { text: it.status === 'ok' ? '成功' : (skipped ? '跳过' : '失败'), style: bad ? { color: 'var(--danger)' } : (skipped ? { color: 'var(--text-mute)' } : {}) }),
          h('td', { text: it.reason || '—' }),
        ]);
      });
      body.appendChild(h('div', [
        h('div', { style: { fontWeight: '600', marginBottom: '8px' }, text: (result && result.summary) || '完成' }),
        h('div.zp-plan-scroll', [
          h('table.table', { style: { fontSize: '12px' } }, [
            h('thead', [h('tr', ['原名', '新名', '结果', '原因'].map((t) => h('th', { text: t })))]),
            h('tbody', rows.length ? rows : [h('tr', [h('td', { colspan: '4', text: '没有条目' })])]),
          ]),
        ]),
      ]));
    }

    presetSel.addEventListener('change', () => {
      const preset = RENAME_PRESETS[Number(presetSel.value)];
      if (!preset) return;
      rules = preset.rules.map((r) => ({ ...r }));
      renderRules();
    });
    includeExt.addEventListener('change', schedulePreview);

    const body = h('div', { style: { minWidth: '0' } }, [
      h('div', { style: RENAME_HINT, text: `目录：${cwd}` }),
      field('预设', presetSel),
      h('label', { style: { display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '12px', fontSize: '12.5px' } }, [
        includeExt, h('span', { text: '规则也作用于扩展名（默认只改主名）' }),
      ]),
      rulesBox,
      h('button.btn.btn-sm', { text: '＋ 加一条规则', onclick: () => { rules.push(renameDefaultRule('replace')); renderRules(); } }),
      h('div', { style: { margin: '14px 0 6px', fontWeight: '600', fontSize: '13px' }, text: '预览' }),
      previewBox,
      statusHint,
    ]);

    modal({
      title: `对选中的 ${names.length} 项批量改名`,
      body,
      wide: true,
      footer: (close) => [applyBtn, h('button.btn', { text: '关闭', onclick: close })],
    });

    applyBtn.addEventListener('click', async () => {
      if (applying || !plan) return;
      applying = true;
      applyBtn.disabled = true;
      statusHint.style.color = '';
      statusHint.textContent = '正在改名…';
      try {
        const res = await api.fileRenameApply({ ...payload(), fingerprint: plan.fingerprint });
        const result = res && res.result;
        showResult(result);
        applyBtn.textContent = '已完成';
        toast((result && result.summary) || '批量改名完成', (result && result.failed) ? 'warn' : 'ok', 12000);
        load(cwd);
      } catch (e) {
        applying = false;
        statusHint.style.color = 'var(--danger)';
        statusHint.textContent = '应用失败：' + ((e && e.message) || e);
        refreshPlan();
      }
    });

    renderRules();
  }

  function copySelection(paths) {
    if (!paths || !paths.length) return;
    clipboard = { mode: 'copy', paths: [...paths] };
    toast(`已复制 ${paths.length} 项，到目标目录按 Ctrl+V 粘贴`, 'info', 6000);
    renderToolbar();
  }

  function cutSelection(paths) {
    if (!paths || !paths.length) return;
    clipboard = { mode: 'cut', paths: [...paths] };
    toast(`已剪切 ${paths.length} 项，到目标目录按 Ctrl+V 粘贴`, 'info', 6000);
    renderToolbar();
  }

  function selectAll() {
    selection = new Set(visibleEntries.map((e) => e.path));
    selAnchor = visibleEntries.length ? 0 : -1;
    renderToolbar(); renderTable();
  }

  function toggleHidden() {
    showHidden = !showHidden;
    try { localStorage.setItem(SHOW_HIDDEN_KEY, showHidden ? '1' : '0'); } catch { /* 本次生效 */ }
    load(cwd);
  }

  async function toolbarNew(kind) {
    const name = await promptBox(kind === 'dir'
      ? { title: '新建文件夹', label: '文件夹名称', placeholder: '例如 assets' }
      : { title: '新建文件', label: '文件名', placeholder: '例如 index.php' });
    if (!name) return;
    try {
      if (kind === 'dir') await api.fileMkdir(`${cwd}/${name}`);
      else await api.fileTouch(`${cwd}/${name}`);
      toast('已创建', 'ok'); load(cwd);
    } catch (e) { toast(e.message, 'err'); }
  }

  function copyFullPath(p) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(p)
        .then(() => toast('已复制路径：' + p, 'ok', 5000))
        .catch(() => toast(p, 'warn', 8000));
    } else {
      toast(p, 'ok', 8000);
    }
  }

  // 解压（走任务中心：大压缩包是分钟级动作，关掉窗口也要能找回进度）
  async function extractEntry(e) {
    try {
      await taskCenter.start({
        kind: 'file_extract', target: e.path,
        title: '解压 ' + e.name,
        start: () => api.fileExtract(e.path, cwd),
        onDone: (task) => {
          if (task && task.status && task.status !== 'succeeded') {
            toast('解压失败：' + (task.error || task.status), 'err', 14000);
            return;
          }
          const r = (task && task.result) || {};
          toast(r.msg || '已解压到当前目录', 'ok', 9000);
          load(cwd);
        },
      });
    } catch (err) { toast('解压失败：' + ((err && err.message) || err), 'err', 12000); }
  }

  // confirmSensitive 对"敏感目录"里的条目追加一道确认。
  //
  // 面板数据目录（SQLite 库、凭据）仍然可读写（用户明确要求），但覆盖/删除它
  // 可能直接让面板起不来 —— 所以单独再问一次，而不是混在普通确认里。
  async function confirmSensitive(entries, action) {
    const hit = (entries || []).filter((e) => e && e.sensitive);
    if (!hit.length) return true;
    return confirmBox(
      `⚠️ 涉及 ${hit.length} 项位于**面板数据目录**（数据库与凭据）。\n\n` +
      `${action}可能让面板无法登录或启动。确定继续？`,
      { title: '敏感目录确认', danger: true, okText: '我已了解，继续' });
  }

  // ---------- 粘贴（复制 / 剪切） ----------
  //
  // 目标已存在同名项时**必须先问**（保留两者 / 覆盖 / 跳过），绝不静默覆盖。
  // 剪切走后端 /files/move：同卷是 rename，跨卷回退 copy+delete，结果里带 way。
  function askPasteConflict(names, total, isCut) {
    return new Promise((resolve) => {
      let picked = 'rename';
      const radios = [
        { v: 'rename', label: '保留两者（推荐）', desc: '自动改名（如 index-1.php），两个都留着，什么都不丢。' },
        { v: 'overwrite', label: '覆盖同名项', desc: `用${isCut ? '被剪切的内容' : '复制出来的内容'}替换目标 —— 目标原有内容不可恢复。` },
        { v: 'skip', label: '跳过同名项', desc: '目标已存在的项不动，只处理不重名的。' },
      ];
      const inputs = radios.map((r) => h('input', {
        type: 'radio', name: 'zp-paste-conflict', value: r.v, checked: r.v === picked,
        onchange: () => { picked = r.v; },
      }));
      const body = h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, [
        h('p', { text: `目标目录「${cwd}」里已有 ${names.length} 个同名项（本次共 ${total} 项）：` }),
        h('ul', { style: { margin: '6px 0 10px 18px', maxHeight: '160px', overflow: 'auto' } },
          names.slice(0, 50).map((n) => h('li', { text: n }))),
        ...radios.map((r, i) => h('label', {
          style: { display: 'flex', gap: '8px', alignItems: 'flex-start', padding: '8px 10px',
            border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', marginBottom: '8px', cursor: 'pointer' },
        }, [inputs[i], h('div', [
          h('div', { style: { fontWeight: '600' }, text: r.label }),
          h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: r.desc }),
        ])])),
      ]);
      const m = modal({
        title: '目标已存在同名项',
        body,
        footer: [
          h('button.btn', { text: '取消', onclick: () => { m.close(); resolve(null); } }),
          h('button.btn.btn-primary', { text: '继续粘贴', onclick: () => { m.close(); resolve(picked); } }),
        ],
      });
    });
  }

  // uniqueTarget 在目标目录里找一个不冲突的名字。
  function uniqueTarget(dir, name, taken) {
    if (!taken.has(name)) return `${dir}/${name}`;
    const dot = name.lastIndexOf('.');
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : '';
    for (let i = 1; i < 1000; i++) {
      const cand = `${base}-${i}${ext}`;
      if (!taken.has(cand)) return `${dir}/${cand}`;
    }
    return `${dir}/${base}-${Date.now()}${ext}`;
  }

  async function pasteClipboard() {
    if (!clipboard || !clipboard.paths.length) { toast('剪贴板是空的', 'warn'); return; }
    const isCut = clipboard.mode === 'cut';
    // 剪切后粘贴回**同一个目录**是无操作：静默略过，别弹"同名冲突"骚扰用户。
    const items = clipboard.paths.slice().filter((p) => !(isCut && p === `${cwd}/${basename(p)}`));
    if (!items.length) {
      toast(isCut ? '这些项已经在当前目录里' : '没有可粘贴的内容', 'warn');
      return;
    }
    const existing = new Set((lastList?.entries || []).map((e) => e.name));
    const conflicts = [...new Set(items.map((p) => basename(p)).filter((n) => existing.has(n)))];
    let strategy = 'rename';
    if (conflicts.length) {
      const choice = await askPasteConflict(conflicts, items.length, isCut);
      if (!choice) return;
      strategy = choice;
    }

    // 目标路径在这里定下来（一次请求带上全部条目），冲突策略同时交给后端复核：
    // rename 时前端先给一个不冲突的名字；skip/overwrite 由后端按策略处理。
    const taken = new Set(existing);
    const pairs = [];
    const skippedNames = [];
    for (const src of items) {
      const name = basename(src);
      let dest = `${cwd}/${name}`;
      // 剪切后粘贴回原目录是无操作；复制到原目录则是"制造一份副本"（走改名分支）。
      if (isCut && src === dest) { skippedNames.push(`${name}（已在当前目录）`); continue; }
      if (taken.has(name) || src === dest) {
        if (strategy === 'skip') { skippedNames.push(`${name}（同名已存在）`); continue; }
        if (strategy === 'rename') {
          dest = uniqueTarget(cwd, name, taken);
        } else if (strategy === 'overwrite' && src === dest) {
          // 不能"先删掉自己再复制自己"：那会把唯一一份数据删没。
          skippedNames.push(`${name}（源与目标相同）`); continue;
        }
      }
      if (strategy === 'overwrite' && existing.has(name) && entrySensitive(dest)) {
        if (!await confirmSensitive([{ path: dest, sensitive: true }], `覆盖「${name}」`)) {
          skippedNames.push(`${name}（未确认覆盖）`); continue;
        }
      }
      pairs.push({ from: src, to: dest });
      taken.add(basename(dest));
    }
    if (!pairs.length) {
      toast(skippedNames.length ? `没有可处理的内容（跳过 ${skippedNames.length} 项）` : '没有可粘贴的内容', 'warn', 9000);
      return;
    }

    const verb = isCut ? '移动' : '复制';
    await startFileOp({
      kind: isCut ? 'file_move' : 'file_copy',
      title: `${verb} ${pairs.length} 项到 ${basename(cwd)}`,
      run: () => (isCut ? api.fileMove(pairs, strategy) : api.fileCopy(pairs, strategy)),
      onSettled: (task) => {
        // 剪切**全部成功**才清空剪贴板：中断/失败时保留，用户能重试。
        if (isCut && String(task.status || '') === 'succeeded') clipboard = null;
        if (skippedNames.length) {
          toast(`另有 ${skippedNames.length} 项按你的选择跳过：` + skippedNames.slice(0, 3).join('，')
            + (skippedNames.length > 3 ? ' …' : ''), 'info', 9000);
        }
        renderToolbar();
      },
    });
  }

  // entrySensitive 判断某个路径是否落在敏感根里（用于粘贴覆盖前的二次确认）。
  function entrySensitive(p) {
    const roots = lastList?.sensitive_roots || [];
    return roots.some((r) => p === r || p.startsWith(r + '/'));
  }

  // ---------- 权限修改（勾选式 UI，宝塔式入口） ----------
  function chmodModal(e) {
    const cur = Number(e.mode_num) & 0o777;
    const bits = [
      { bit: 0o400, who: '属主 u', label: '读' }, { bit: 0o200, who: '属主 u', label: '写' }, { bit: 0o100, who: '属主 u', label: '执行' },
      { bit: 0o040, who: '同组 g', label: '读' }, { bit: 0o020, who: '同组 g', label: '写' }, { bit: 0o010, who: '同组 g', label: '执行' },
      { bit: 0o004, who: '其他 o', label: '读' }, { bit: 0o002, who: '其他 o', label: '写' }, { bit: 0o001, who: '其他 o', label: '执行' },
    ];
    const boxes = bits.map((b) => h('input', { type: 'checkbox', checked: (cur & b.bit) !== 0, dataset: { bit: String(b.bit) } }));
    const modeText = h('span.mono', { style: { fontWeight: '600' }, text: cur.toString(8).padStart(3, '0') });
    const recursive = h('input', { type: 'checkbox' });

    const currentMode = () => boxes.reduce((v, b) => v | (b.checked ? Number(b.dataset.bit) : 0), 0);
    const sync = () => { modeText.textContent = currentMode().toString(8).padStart(3, '0'); };
    boxes.forEach((b) => b.addEventListener('change', sync));

    const rows = ['属主 u', '同组 g', '其他 o'].map((who, gi) => h('tr', [
      h('td', { text: who }),
      ...boxes.slice(gi * 3, gi * 3 + 3).map((b) => h('td', [b])),
    ]));

    const m = modal({
      title: '权限：' + e.name,
      body: h('div', { style: { fontSize: '13px', lineHeight: '1.7' } }, [
        h('div.hint', { text: '完整路径：' + e.path + (e.is_dir ? '（目录）' : '（文件）') }),
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '对象' }), h('th', { text: '读 (4)' }), h('th', { text: '写 (2)' }), h('th', { text: '执行 (1)' })])]),
          h('tbody', rows),
        ]),
        h('div', { style: { marginTop: '10px' } }, [h('span', { text: '八进制：' }), modeText]),
        e.is_dir ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '10px', cursor: 'pointer' } },
          [recursive, h('span', { text: '同时递归应用到该目录下的所有内容' })]) : null,
        h('div.hint', { style: { marginTop: '8px' },
          text: '常用：644 = 普通文件；755 = 目录或可执行脚本；600 = 仅属主可读写。' }),
        e.sensitive ? h('div.hint', { style: { color: 'var(--warn)' },
          text: '⚠️ 这是面板数据目录里的内容，改错权限可能让面板无法读取数据库或凭据。' }) : null,
      ]),
      footer: [
        h('button.btn', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-primary', {
          text: '应用',
          onclick: async () => {
            const mode = currentMode().toString(8).padStart(3, '0');
            if (!await confirmSensitive([e], `修改「${e.name}」的权限`)) return;
            if (e.is_dir && recursive.checked && !await confirmBox(
              `将递归修改「${e.name}」下所有内容的权限为 ${mode}。继续？`,
              { title: '递归修改权限', okText: '递归修改' })) return;
            try {
              const r = await api.fileChmod(e.path, mode, e.is_dir && recursive.checked);
              toast(r.msg || '权限已修改', 'ok');
              m.close(); load(cwd);
            } catch (err) { toast(err.message, 'err'); }
          },
        }),
      ],
    });
  }

  // ---------- 删除（走任务中心：大目录/海量小文件的删除也是分钟级动作） ----------
  async function deleteOne(e) {
    if (!await confirmSensitive([e], `删除「${e.name}」`)) return;
    // 符号链接：只删链接本身，绝不跟随去删目标（后端 Delete 也保证这一点）。
    // 按 is_dir 走"递归删目录"的措辞会骗人——链接里的内容一个都不会动。
    if (e.symlink) {
      if (!await confirmBox(
        `确认删除符号链接「${e.name}」？\n\n只删除链接本身，不会删除它指向的内容。`,
        { title: '删除链接', danger: true, okText: '删除链接' })) return;
      await startFileOp({
        kind: 'file_delete', title: `删除 ${e.name}`,
        run: () => api.fileDelete([e.path], false),
      });
      return;
    }
    if (e.is_dir) {
      const recursive = await confirmBox(
        `确认删除目录「${e.name}」及其中的全部内容？\n\n此操作不可撤销。`,
        { title: '删除目录', danger: true, okText: '递归删除' });
      if (!recursive) return;
      await startFileOp({
        kind: 'file_delete', title: `删除 ${e.name}`,
        run: () => api.fileDelete([e.path], true),
      });
      return;
    }
    if (!await confirmBox(`确认删除文件「${e.name}」？`, { title: '删除文件', danger: true, okText: '删除' })) return;
    await startFileOp({
      kind: 'file_delete', title: `删除 ${e.name}`,
      run: () => api.fileDelete([e.path], false),
    });
  }

  // deleteSelectionOrOne 供右键菜单调用：多选时批量删，单选时走单项删除。
  async function deleteSelectionOrOne(e, paths) {
    if (paths && paths.length > 1) return deleteSelected();
    return deleteOne(e);
  }

  async function deleteSelected() {
    const paths = [...selection];
    if (!paths.length) return;
    const entries = (lastList?.entries || []).filter((e) => selection.has(e.path));
    if (!await confirmSensitive(entries, '删除这些内容')) return;
    const hasDir = entries.some((e) => e.is_dir && !e.symlink);
    const msg = `将删除 ${paths.length} 项${hasDir ? '（包含目录，其中的内容会一并删除）' : ''}。\n\n此操作不可撤销。`;
    if (!await confirmBox(msg, { title: '批量删除', danger: true, okText: '确认删除' })) return;
    await startFileOp({
      kind: 'file_delete', title: `删除 ${paths.length} 项`,
      run: () => api.fileDelete(paths, true),
    });
  }

  // imageCompressModal 是「图片压缩」弹窗：引擎状态 + 选项 + 走任务中心。
  //
  // 设计要点（都是用户提过的要求）：
  //   · 先给**事实**：这个目录里有几张图、共多大（后端扫描给出），值不值得跑用户自己判断；
  //   · 引擎没装时**不让点**，并直接给「一键安装」（不把用户打发去别的页面找）；
  //   · 长任务走任务中心（关掉窗口也能在任务中心看进度），结果逐条列出"省了多少"；
  //   · 默认**不动原文件**（另存 .min），覆盖是显式选项且会先确认。
  async function imageCompressModal() {
    const body = h('div');
    const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' } });
    const m = modal({ title: '🖼️ 图片压缩（libvips）', body, footer: [foot], wide: false });
    body.append(h('div.hint', { text: '正在读取引擎与目录…' }));

    let st = null;
    try {
      st = await api.imageEngine(cwd, false);
    } catch (e) {
      body.textContent = '读取引擎状态失败：' + ((e && e.message) || e);
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
      return;
    }

    // 选项（把上次的选择留在本页会话里，省得每次都重设）
    const quality = h('input', { type: 'range', min: '40', max: '100', step: '1', value: String(st.default_quality || 82), style: { flex: '1' } });
    const qualityText = h('span', { text: String(st.default_quality || 82), style: { width: '34px', textAlign: 'right' } });
    quality.addEventListener('input', () => { qualityText.textContent = quality.value; });
    const format = h('select.select', [
      h('option', { value: 'keep', text: '保持原格式（只重新编码）' }),
      h('option', { value: 'webp', text: 'WebP（通常最小，兼容性好）' }),
      h('option', { value: 'avif', text: 'AVIF（更小，但编码慢、老浏览器不支持）' }),
      h('option', { value: 'jpeg', text: 'JPEG' }),
      h('option', { value: 'png', text: 'PNG（无损，压不动多少）' }),
    ]);
    const maxEdge = h('select.select', (st.max_edge_choices || [0, 1920, 2560, 3840]).map((v) => h('option', {
      value: String(v), text: v === 0 ? '不缩放（只重压）' : '最长边 ' + v + 'px（只缩小，不放大）',
    })));
    maxEdge.value = '0';
    const strip = h('input', { type: 'checkbox' });
    const recursive = h('input', { type: 'checkbox' });
    const overwrite = h('input', { type: 'checkbox' });

    function draw() {
      clear(body);
      clear(foot);

      if (!st.available) {
        body.append(h('div', { style: { lineHeight: '1.8' } }, [
          h('div', { style: { fontWeight: '620', color: 'var(--warn)' }, text: '引擎还没装（缺 libvips）' }),
          h('div', { style: { marginTop: '4px' }, text: st.reason || '' }),
          h('div.hint', { style: { marginTop: '8px' },
            text: '「图片压缩（libvips）」是原生 arm64 包，不需要 Docker / Node / PHP；装好后这个弹窗就能用了。' }),
        ]));
        foot.append(h('button.btn.btn-primary', {
          text: '一键安装（libvips）',
          onclick: async () => {
            try {
              await taskCenter.start({
                kind: 'install', target: st.market_app_id, title: '安装 图片压缩（libvips）',
                start: () => api.marketInstall(st.market_app_id),
                onDone: async (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('安装失败：' + (task.error || task.status), 'err', 12000);
                    return;
                  }
                  toast('引擎已装好，正在刷新…', 'ok', 8000);
                  try { st = await api.imageEngine(cwd, recursive.checked); } catch (e) { /* 下面 draw 会如实显示 */ }
                  draw();
                },
              });
            } catch (e) { toast('安装失败：' + ((e && e.message) || e), 'err', 12000); }
          },
        }));
        foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
        return;
      }

      const scan = st.scan_error
        ? h('div.hint', { style: { color: 'var(--danger)' }, text: '扫描目录失败：' + st.scan_error })
        : h('div.hint', {
          text: '目录 ' + (st.dir || cwd) + '：找到 ' + st.image_count + ' 张图片，共 ' + humanSize(st.total_bytes)
            + (st.image_count === 0 ? '（这个目录里没有可压缩的图片）' : ''),
        });

      body.append(h('div', { style: { lineHeight: '1.7' } }, [
        h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)', marginBottom: '6px' },
          text: '引擎：' + (st.version || '') + '（' + (st.bin || '') + '）' }),
        scan,
        h('div.field', { style: { marginTop: '10px' } }, [
          h('label', { text: '质量（越小越省体积；PNG 是无损格式，这一项对它无效）' }),
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [quality, qualityText]),
        ]),
        h('div.field', [h('label', { text: '输出格式' }), format]),
        h('div.field', [h('label', { text: '尺寸' }), maxEdge]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '8px' } },
          [strip, h('span', { text: '去掉元数据（EXIF/ICC 等，体积更小；照片的拍摄信息会丢）' })]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } },
          [recursive, h('span', { text: '包含子目录' })]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } },
          [overwrite, h('span', { text: '覆盖原文件（默认不勾：另存为 xxx.min.<ext>）' })]),
        h('div.hint', { style: { marginTop: '8px' },
          text: '失败或"压完反而更大"的文件一律保留原样并逐条说明；压缩在任务中心执行，关掉这个窗口也能看进度。' }),
      ]));

      const start = h('button.btn.btn-primary', { text: '开始压缩' + (st.image_count ? '（' + st.image_count + ' 张）' : '') });
      start.addEventListener('click', async () => {
        const opts = {
          dir: st.dir || cwd,
          recursive: recursive.checked,
          quality: Number(quality.value),
          format: format.value,
          max_edge: Number(maxEdge.value),
          strip_metadata: strip.checked,
          overwrite: overwrite.checked,
        };
        if (opts.overwrite && !await confirmBox(
          '将**覆盖** ' + (st.dir || cwd) + ' 里的原图（共 ' + st.image_count + ' 张）。\n\n'
          + '压完更大的文件会自动保留原样，但覆盖不可撤销 —— 重要图片建议先备份。\n\n继续？',
          { title: '覆盖原文件', okText: '覆盖压缩' })) return;
        m.close();
        try {
          await taskCenter.start({
            kind: 'image_compress', target: opts.dir,
            title: '压缩图片（' + st.image_count + ' 张）',
            start: () => api.imageCompress(opts),
            onDone: (task) => {
              if (task && task.status && task.status !== 'succeeded') {
                toast('图片压缩失败：' + (task.error || task.status), 'err', 14000);
                return;
              }
              const r = (task && task.result) || {};
              toast('图片压缩完成：成功 ' + (r.done || 0) + ' 张，跳过 ' + (r.skipped || 0)
                + ' 张（压完更大），失败 ' + (r.failed || 0) + ' 张；共省 ' + humanSize(r.saved_bytes || 0),
              r.failed ? 'warn' : 'ok', 14000);
              load(cwd);
            },
          });
        } catch (e) { toast('图片压缩失败：' + ((e && e.message) || e), 'err', 14000); }
      });
      foot.append(h('button.btn', { text: '重新扫描', onclick: async () => {
        try { st = await api.imageEngine(cwd, recursive.checked); draw(); }
        catch (e) { toast('扫描失败：' + ((e && e.message) || e), 'err'); }
      } }));
      foot.append(start);
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
      recursive.addEventListener('change', () => { /* 需要重新扫描才准 */ });
    }
    draw();
  }

  // mediaCleanModal 是「🧹 去广告（无损）」入口：没选中先说明；有选中就先只读统计
  // （后端按扩展名递归数，不跑 ffmpeg），再弹确认窗，最后走任务中心。
  //
  // 确认窗必须写清五件事（用户点名）：不重编码 / 彻底清理哪些东西 /
  // 默认不保留 .bak（勾选才留）/ 先写临时文件核对时长才替换 / 本次处理 N 个
  // （数字来自后端统计，前端不猜、不谎报）。
  async function mediaCleanModal() {
    if (!selection.size) {
      modal({
        title: '🧹 去广告（无损）',
        body: h('div', [
          h('p', { text: '先在列表里选中要处理的视频或文件夹。' }),
          h('div.hint', { text: '文件夹会递归找里面的视频；支持 .mkv .mp4 .m4v .mov .avi .ts .webm。' }),
        ]),
        footer: (close) => [h('button.btn', { text: '知道了', onclick: close })],
      });
      return;
    }
    const paths = [...selection];
    const loading = modal({
      title: '🧹 去广告（无损）',
      body: h('div.hint', { text: '正在统计要处理的视频…' }),
      footer: [],
    });
    let plan;
    try {
      plan = await api.fileMediaCleanPlan(paths);
    } catch (e) {
      loading.close();
      toast('统计失败：' + ((e && e.message) || e), 'err', 12000);
      return;
    }
    loading.close();
    const total = Number(plan.total || 0);
    const ignored = Number(plan.ignored || 0) + Number(plan.skipped || 0);
    const ignoredNote = ignored ? '（另有 ' + ignored + ' 个不是支持的视频类型，会跳过）' : '';
    if (!total) {
      modal({
        title: '🧹 去广告（无损）',
        body: h('div', [
          h('p', { text: '选中的内容里没有可处理的视频。' }),
          ignored ? h('div.hint', { text: '另有 ' + ignored + ' 个不是支持的视频类型。' }) : null,
          h('div.hint', { text: '支持 ' + ((plan.exts || []).join(' / ')) + '。' }),
        ]),
        footer: (close) => [h('button.btn', { text: '知道了', onclick: close })],
      });
      return;
    }
    // 默认不勾：用户报障 .bak 极难清理。勾了才有备份（可回滚）。
    const keepBackup = h('input', { type: 'checkbox' });
    modal({
      title: '🧹 去广告（无损）',
      body: h('div', [
        h('p', { text: '将处理 ' + total + ' 个视频' + ignoredNote + '。' }),
        h('div.hint', { text: '彻底清理：清空全部全局标签与各轨道标题，删掉非字体附件（保留字体）与多余视频流（保留第一条视频）；内嵌封面一并删掉。' }),
        h('ol', { style: { margin: '6px 0 0 18px', lineHeight: '1.9' } }, [
          h('li', { text: '不重编码：画质与音轨不变。' }),
          h('li', { text: '默认直接替换原文件（不留备份）；勾选下方选项才保留 .bak。' }),
          h('li', { text: '先写临时文件，核对时长通过（差 <1 秒）才替换。' }),
          h('li', { text: '本次会处理 ' + total + ' 个文件。' }),
        ]),
        h('label', {
          style: { display: 'flex', gap: '6px', alignItems: 'center', marginTop: '8px', cursor: 'pointer' },
          title: '默认不勾：查过时长的产物直接盖回原文件名。勾了才有回滚余地，但要自己清理 .bak。',
        }, [keepBackup, h('span', { text: '保留原文件为 .bak（默认不勾；勾了才备份）' })]),
        h('div.hint', { text: '彻底清理在未勾选备份时不可逆（但会先核对时长）；失败或取消时临时文件会删掉，源文件不动。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始处理',
          title: '在任务中心后台执行；关掉页面也在跑',
          onclick: () => {
            const backup = !!keepBackup.checked;
            close();
            startFileOp({
              kind: 'file_media_clean',
              title: '去广告（无损）' + total + ' 个',
              run: () => api.fileMediaClean(paths, backup),
            });
          },
        }),
      ],
    });
  }

  // videoPipelineTip 是「按源类型给提示」的唯一渲染处：判据**全部来自后端**
  // （plan.pipeline / pipeline_reason / 三个计数），前端绝不自己拿分辨率/位深重算
  // （前端接线门禁盯着）。四类文案：快路 / 软解 / 混合计数 / 判据不足。
  function videoPipelineTip(plan) {
    const fast = plan.pipeline_fast || 0;
    const soft = plan.pipeline_software || 0;
    const unknown = plan.pipeline_unknown || 0;
    const kind = plan.pipeline || '';
    const reason = plan.pipeline_reason || '';
    if (!fast && !soft && !unknown) return null;
    let text;
    if (kind === 'mixed') {
      const parts = [];
      if (fast) parts.push('⚡ 硬件管线 ' + fast + ' 个');
      if (soft) parts.push('🐢 软件解码 ' + soft + ' 个');
      if (unknown) parts.push('❔ 判据不足 ' + unknown + ' 个');
      text = parts.join(' · ');
    } else if (kind === 'fast') {
      text = '⚡ 此片源适合硬件管线（4K·10bit·高码率），预计快很多';
    } else if (kind === 'software' && reason !== 'unknown') {
      text = '🐢 此片源软件解码更快（8bit/低码率），已避开硬件管线';
    } else {
      text = '❔ 源信息不足，按默认管线处理（不保证提速）';
    }
    return h('div.zp-pipeline-tip', {
      'data-testid': 'zp-video-pipeline-tip',
      style: { margin: '0 0 8px', fontWeight: '620' },
      text,
      title: videoPipelineTitle(plan),
    });
  }

  // videoPipelineTitle 是提示的细节（判据 + 回退），不占主句。
  function videoPipelineTitle(plan) {
    const reasons = {
      '4k_10bit_highbitrate': '4K 级 + 10bit 及以上 + 码率 ≥6 Mbps',
      'low_bitrate': '4K 级 10bit 但码率不足 6 Mbps',
      'bit_depth_8': '4K 级但只有 8bit',
      'not_4k': '分辨率不到 4K 级',
      'unknown': '位深/码率读不到',
      'mixed': '目录里不止一类片源',
    };
    const reason = plan.pipeline_reason || 'unknown';
    // 逐文件事实（后端字段）：只在**所有行一致**时写进 title，混合时不挑一个代表。
    const facts = [...new Set((plan.rows || []).map((r) => {
      const bits = [];
      if (r.src_bit_depth) bits.push(r.src_bit_depth + 'bit');
      if (r.src_bitrate_kbps) bits.push(r.src_bitrate_kbps + ' kbps');
      if (r.src_fps) bits.push(r.src_fps + 'fps' + (r.cap_fps30 ? '→30fps' : ''));
      return bits.join(' · ');
    }).filter(Boolean))];
    return '快路判据：' + (reasons[reason] || reason)
      + (facts.length === 1 ? '（源：' + facts[0] + '）' : '')
      + '；当前编码器 ' + (plan.encoder_codec || plan.encoder || '—')
      + '；硬件管线失败会自动回退软件解码并写日志';
  }

  // videoCompressModal 是「🎬 压缩视频」弹窗：档位 + 编码器 + 模式 + 计划表 + 走任务中心。
  //
  // 用户点名的硬要求都在这里：
  //   · 绝不放大（档位只是"封顶"，目标分辨率不会超过原尺寸）；
  //   · 绝不越压越大（计划表白纸黑字写体积对比，执行时回读产物大小）；
  //   · 后台跑（关掉这个窗口/整个页面都不影响，进度与中断都在任务中心）。
  // 档位/码率/编码器/模式/质量档的选项与默认值**全部来自后端**，前端不重复写数字。
  //
  // 性能（用户报障）：改配置不再重新探测。后端按「路径+大小+时间」缓存 ffprobe 结论，
  // 所以改档位/码率/编码器/模式只做纯函数重算。刷新期间保留已画好的计划表与用户刚点的
  // 选项，只显示一行"正在刷新计划…"；连点多项时只认最后一次响应。
  async function videoCompressModal() {
    const body = h('div');
    // 进度区与计划表分开：刷新时不动已画好的表，也不把用户刚点的选项弹回去。
    // 主行是后端给的唯一文案（递归时含真实计数：已扫描目录数 / 发现视频数 / 当前目录）；
    // 第二行是「约剩」，只在总数已知且样本足够时才有内容（数据不足就不显示）。
    const progressText = h('div.hint', { text: '正在刷新计划…' });
    const progressEta = h('div.hint', { style: { display: 'none' } });
    const cancelBtn = h('button.btn.btn-sm', {
      text: '取消',
      title: '停止这次扫描：不会产生计划；已画好的计划保持原样',
      onclick: () => {
        // 只置标志 + abort：请求被中断后由 reload 的收场统一显示"已取消"，
        // 避免"取消与响应擦肩"时两边都去改界面。
        canceledByUser = true;
        stopProgress();
        if (abortPlan) abortPlan.abort();
      },
    });
    // 不确定态动画（CSS 只新增）：只表示"还在跑"，不写任何假百分比。
    const progressBar = h('div.zp-plan-prog', [h('i')]);
    const progressBox = h('div', { style: { display: 'none' } }, [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [progressText, cancelBtn]),
      progressEta,
      progressBar,
    ]);
    // scopeHint 是"这次处理哪些视频"的唯一说明 + 切换入口（用户点名：别让人以为漏压了）。
    const scopeHint = h('div.zp-video-scope', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '8px' } });
    const shell = h('div', [progressBox, scopeHint, body]);
    const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' } });
    // 关掉弹窗要能收场：中断进行中的计划请求（服务端规划循环会看 ctx），并停掉进度轮询。
    let abortPlan = null;
    let progressTimer = null;
    // canceledByUser 区分"用户点取消"与"被下一次请求顶掉/关窗"：只有前者要显示已取消。
    let canceledByUser = false;
    // progressSamples 是「约剩」的采样（进度单位/秒），复用 transfereta 的窗口算法。
    let progressSamples = [];
    const m = modal({
      title: '🎬 压缩视频', body: shell, footer: [foot], wide: true,
      onClose: () => {
        if (abortPlan) abortPlan.abort();
        stopProgress();
      },
    });

    let preset = ''; // 空 = 用后端默认（首次响应回填 = 原始档）
    let kbps = 0; // 0 = 用档位下限；原始档 = 按该文件的源分辨率建议（再受原码率×0.95 封顶）
    let encoder = ''; // 空 = 用后端默认（首次响应回填）
    let mode = '';
    let quality = 0; // 0 = 用该编码器的默认质量档
    let twoPass = false;
    // recursive 默认 **不勾**：不勾 = 只看当前这一层（与既有行为逐字节一致）。
    // 勾上后扫描/规划/执行都带 recursive: true，产物按原目录结构放进 output/。
    let recursive = false;
    let plan = null;
    // reqSeq：连点 3 个选项会有 3 个请求，只让最后一次的响应落地（旧的直接丢弃）。
    let reqSeq = 0;
    let startBtn = null;
    // 提交期的原地反馈：用户实测的空档是"弹窗先消失、进度窗还没来"。
    // 拿到 202 之前不关弹窗，一直显示"正在提交压缩任务…"。
    const submitHint = h('div.hint', { style: { display: 'none' } });

    // 打开弹窗那一刻的选择集：**只把视频候选带进请求**（扩展名口径与后端 videoExts 一致）；
    // 选择集里没有视频 ⇒ 不带 names（= 处理整个目录），与用户点名的语义一致。
    const selDirs = new Set(((lastList && lastList.entries) || []).filter((e) => e.is_dir).map((e) => e.name));
    const selNames = [...selection].map((p) => basename(p))
      .filter((n) => n && !selDirs.has(n) && VIDEO_CANDIDATE_EXT.test(n));
    const selOthers = [...selection].map((p) => basename(p))
      .filter((n) => n && !selDirs.has(n) && !VIDEO_CANDIDATE_EXT.test(n));
    let onlySelected = selNames.length > 0;

    // renderScope 画"只处理选中的 N 个 / 处理全部 M 个"+ 一颗切换按钮（状态一目了然）。
    function renderScope() {
      clear(scopeHint);
      // 勾了「包含子目录」就没有"只处理选中"这回事：选中的是当前这层的文件，
      // 勾子目录表示要连子目录一起处理（切换时前端已把 onlySelected 关掉）。
      if (recursive) {
        scopeHint.append(h('span', { text: '处理当前目录及子目录的全部'
          + (plan && plan.total ? ' ' + plan.total + ' 个' : '') + '视频' }));
        return;
      }
      if (onlySelected) {
        scopeHint.append(h('span', { text: '只处理选中的 ' + selNames.length + ' 个视频'
          + (selOthers.length ? '（另有 ' + selOthers.length + ' 项不是视频，已忽略）' : '') }));
        scopeHint.append(h('button.btn.btn-sm', {
          text: '改为处理全部', title: '忽略当前选择，处理这个目录里的全部视频',
          onclick: () => { onlySelected = false; reload(); },
        }));
      } else {
        scopeHint.append(h('span', { text: '处理当前目录的全部'
          + (plan && plan.total ? ' ' + plan.total + ' 个' : '') + '视频' }));
        if (selNames.length) {
          scopeHint.append(h('button.btn.btn-sm', {
            text: '只处理选中', title: '只处理你在文件列表里选中的那些视频',
            onclick: () => { onlySelected = true; reload(); },
          }));
        }
      }
    }

    renderScope();

    // 后端归一化后的值回填本地状态（默认值只由后端定义一次）。
    // recursive 也以后端回显为准：取消刷新时就靠它把勾选状态滚回"已画出的那份计划"。
    function syncFromPlan() {
      preset = plan.preset || preset;
      kbps = plan.kbps || 0;
      encoder = plan.encoder || encoder;
      mode = plan.mode || mode;
      quality = plan.quality || 0;
      twoPass = !!plan.two_pass;
      recursive = !!plan.recursive;
    }

    // stopProgress 停掉进度轮询。
    function stopProgress() {
      if (progressTimer) { clearInterval(progressTimer); progressTimer = null; }
    }

    // applyProgress 把服务端的进度快照画出来：主行直接用后端文案（唯一措辞来源，
    // 递归时是「已扫描 N 个目录 · 发现 M 个视频 · 已用 T 秒 · 当前：…」，三个数都是真的）。
    // 「约剩」只在**总数已知**时才算：递归扫描阶段总目录数不可知 ⇒ 数据不足就不显示；
    // 探测阶段总数已知，按"已读取个数/秒"外推（口径与文件操作那套 transfereta 完全一致：
    // 样本 <2 或跨度 <0.5s 一律不编数字）。
    function applyProgress(p) {
      if (p.message) progressText.textContent = p.message;
      let eta = '';
      if (p.phase === 'probe' && p.total > 0) {
        progressSamples = pushSample(progressSamples, Date.now(), p.done || 0, 60000);
        const perSec = rateBps(progressSamples, 0.5);
        const rem = p.total - (p.done || 0);
        if (perSec > 0 && rem > 0) eta = '约剩 ' + formatEta(rem / perSec);
      }
      progressEta.textContent = eta;
      progressEta.style.display = eta ? '' : 'none';
    }

    // startProgress 在请求飞行期间轮询服务端的实时进度（只读内存快照，不重新扫描）。
    // 冷路径与"刷新路径"都轮询：勾「包含子目录」是在已有计划上刷新，过去只显示一句
    // 「正在刷新计划…」、看不到任何进度（用户报障的正是这里）。
    function startProgress(my) {
      stopProgress();
      progressSamples = [];
      progressEta.textContent = '';
      progressEta.style.display = 'none';
      const tick = async () => {
        if (my !== reqSeq) return;
        try {
          const p = await api.fileVideoPlanProgress(cwd);
          if (my !== reqSeq) return;
          if (p && p.active) applyProgress(p);
        } catch { /* 轮询失败不影响主请求：保持已有文案 */ }
      };
      tick();
      progressTimer = setInterval(tick, 150);
    }

    // showCanceled 是取消后的如实收场：显示"已取消"，绝不显示假计划、绝不误报失败。
    // 有上一份完整计划就原样退回它（连勾选/选项一起滚回去），没有就显示可重试的空页。
    function showCanceled() {
      stopProgress();
      progressBox.style.display = '';
      cancelBtn.style.display = 'none';
      progressBar.style.display = 'none'; // 已停下就别再转（不然"已取消"配一个转圈=自相矛盾）
      progressText.textContent = '已取消';
      progressEta.textContent = '';
      progressEta.style.display = 'none';
      if (plan) {
        syncFromPlan();
        draw();
        return;
      }
      clear(body); clear(foot);
      body.append(h('div.hint', { text: '已取消，没有生成计划。' }));
      foot.append(h('button.btn', { text: '重新规划', onclick: () => reload() }));
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
    }

    async function reload(opts) {
      const o = opts || {};
      const my = ++reqSeq;
      canceledByUser = false;
      // 上一次还没回来就再点（连点选项/关窗）：中断它，绝不让旧响应覆盖新状态。
      if (abortPlan) abortPlan.abort();
      abortPlan = new AbortController();
      stopProgress();
      // 进度区在两种路径都显示；文案保留既有的两句（冷路径/刷新路径各一句）。
      progressBox.style.display = '';
      cancelBtn.style.display = '';
      progressBar.style.display = '';
      progressText.textContent = plan ? '正在刷新计划…' : '正在读取目录与视频信息…';
      progressEta.textContent = '';
      progressEta.style.display = 'none';
      if (!plan) {
        // 冷路径（第一次打开）：只有这里才真的在等服务端逐文件探测。
        clear(body); clear(foot);
      } else if (startBtn) {
        startBtn.disabled = true; // 刷新期间不许提交，避免提交到上一份计划
      }
      startProgress(my);
      let next;
      try {
        next = await api.fileVideoPlan({
          dir: cwd, preset, kbps, encoder, mode, quality, two_pass: twoPass, rescan: !!o.rescan,
          // 勾了「包含子目录」才带 recursive：不勾时请求与既有行为逐字节一致。
          ...(recursive ? { recursive: true } : {}),
          // 只处理选中的视频时才带 names；不带 = 处理整个目录（后端语义）。
          ...(onlySelected && !recursive ? { names: selNames } : {}),
        }, { signal: abortPlan.signal });
      } catch (e) {
        // 自己中断的：用户点了「取消」就如实显示已取消；被下一次请求顶掉/关窗则静默收场。
        if (e && e.name === 'AbortError') {
          if (canceledByUser && my === reqSeq) showCanceled();
          return;
        }
        if (my !== reqSeq) return; // 过期响应：只留最后一次
        stopProgress();
        progressBox.style.display = 'none';
        if (!plan) {
          clear(body); clear(foot);
          body.append(h('div.hint', { style: { color: 'var(--danger)' }, text: '读取失败：' + ((e && e.message) || e) }));
          foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
        } else {
          // 刷新失败：退回到上一份可用计划（选项显示与本地状态一起退回，绝不错位）。
          preset = plan.preset || preset;
          kbps = plan.kbps || 0;
          syncFromPlan();
          draw();
          toast('刷新计划失败：' + ((e && e.message) || e), 'err', 10000);
        }
        return;
      }
      if (my !== reqSeq) return; // 过期响应：只留最后一次
      if (canceledByUser) { showCanceled(); return; } // 取消与响应擦肩：按用户意图收场
      stopProgress();
      progressBox.style.display = 'none';
      plan = next;
      syncFromPlan();
      draw();
    }

    // 单选一行（选项与说明都来自后端；选中项的 hint 显示在下面）。
    function radioRow(name, choices, current, onPick) {
      const row = h('div', { style: { display: 'flex', gap: '14px', alignItems: 'center', flexWrap: 'wrap' } },
        (choices || []).map((c) => h('label', {
          style: { display: 'flex', gap: '5px', alignItems: 'center', cursor: 'pointer' },
        }, [
          h('input', {
            type: 'radio', name, value: c.value, checked: c.value === current,
            onchange: () => onPick(c.value),
          }),
          h('span', { text: c.label }),
        ])));
      const cur = (choices || []).find((c) => c.value === current);
      return [row, cur && cur.hint ? h('div.hint', { text: cur.hint }) : null];
    }

    // 一行体积对比：「原 → 预计（-XX%）」；质量优先如实说不可预估，不编数字。
    function sizeText(before, after, pct, unknown) {
      const b = humanSize(before || 0);
      if (unknown) return b + ' → 不可预估';
      if (!after) return b + ' → —';
      return b + ' → ' + humanSize(after) + '（-' + (pct || 0) + '%）';
    }

    function draw() {
      clear(body); clear(foot);
      renderScope();
      startBtn = null;

      if (!plan.available) {
        body.append(h('div', { style: { lineHeight: '1.8' } }, [
          h('div', { style: { fontWeight: '620', color: 'var(--warn)' }, text: '引擎还没装（缺 FFmpeg）' }),
          h('div', { text: plan.reason || '' }),
          h('div.hint', { text: '装好后这个弹窗就能用（应用市场 → FFmpeg）' }),
        ]));
        foot.append(h('button.btn.btn-primary', {
          text: '一键安装 FFmpeg',
          onclick: async () => {
            await taskCenter.start({
              kind: 'install', target: plan.market_app_id, title: '安装 FFmpeg（音视频工具）',
              start: () => api.marketInstall(plan.market_app_id),
              onDone: (task) => {
                if (task && task.status && task.status !== 'succeeded') {
                  toast('安装失败：' + (task.error || task.status), 'err', 12000);
                  return;
                }
                toast('FFmpeg 已装好，正在重新规划…', 'ok', 8000);
                reload();
              },
            });
          },
        }));
        foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
        return;
      }

      // 第一项 = 分辨率（5 档，默认「原始」；选项与文案都来自后端）。
      const [presetRow, presetHint] = radioRow('zp-video-preset',
        (plan.presets || []).map((p) => ({ value: p.id, label: p.label, hint: p.hint })),
        plan.preset, (v) => { preset = v; kbps = 0; reload(); });

      const [encoderRow, encoderHint] = radioRow('zp-video-encoder', plan.encoders, plan.encoder, (v) => {
        encoder = v; quality = 0; // 质量档数值在两种编码器里含义不同，切了就回默认
        if (v !== 'cpu') twoPass = false; // 2-pass 只支持 CPU
        reload(); // 只换编码器，码率/档位一个字都不动（码率参数与 CPU 时逐字相同）
      });
      const [modeRow, modeHint] = radioRow('zp-video-mode', plan.modes, plan.mode, (v) => {
        mode = v; quality = 0;
        if (v !== 'bitrate') twoPass = false; // 质量优先没有 2-pass
        reload();
      });

      // 目标码率 / 质量档：二选一（由模式决定）。
      // 两者的刻度**方向相反**（CRF 越小越好 / -q:v 越大越好），标签必须写清。
      let tuneField;
      if (plan.mode === 'quality') {
        const q = h('select.select', (plan.quality_choices || []).map((c) => h('option', {
          value: String(c.value), text: c.label,
        })));
        q.value = String(plan.quality);
        q.addEventListener('change', () => { quality = Number(q.value) || 0; reload(); });
        tuneField = h('div.field', [h('label', {
          text: plan.encoder === 'cpu'
            ? '质量档 CRF（1~51，越小画质越好、文件越大）'
            : '质量档 -q:v（1~100，越大画质越好、文件越大，与 CRF 相反）',
        }), q]);
      } else {
        const bitrate = h('select.select', (plan.bitrate_choices || []).map((c) => h('option', {
          value: String(c.kbps), text: c.label, title: c.hint || '',
        })));
        bitrate.value = String(plan.kbps);
        bitrate.addEventListener('change', () => { kbps = Number(bitrate.value) || 0; reload(); });
        tuneField = h('div.field', [h('label', { text: '目标码率' }), bitrate]);
      }

      // 2-pass：勾上即**自动锁定** CPU + 目标码率（硬件不支持 2-pass；CRF 本来就是单遍）。
      const passBox = h('input', { type: 'checkbox', checked: plan.two_pass });
      const passAllowed = plan.encoder === 'cpu' && plan.mode === 'bitrate';
      passBox.addEventListener('change', () => {
        twoPass = passBox.checked;
        if (twoPass) { encoder = 'cpu'; mode = 'bitrate'; }
        reload();
      });

      // 「包含子目录」：默认不勾（不勾时请求与既有行为逐字节一致）。
      // 勾上后扫描/规划/执行都带 recursive: true，产物按原目录结构放进 output/。
      const recBox = h('input', { type: 'checkbox', id: 'zp-video-recursive', checked: recursive });
      recBox.addEventListener('change', () => {
        recursive = recBox.checked;
        // 勾了子目录就不能再"只处理选中的这些"（选中的是当前这层的文件）。
        if (recursive) onlySelected = false;
        reload();
      });

      const rows = (plan.rows || []).map((r) => {
        let tune = '—';
        if (r.video_kbps) {
          const codec = r.encoder_codec ? r.encoder_codec + ' · ' : '';
          if (r.mode === 'quality') {
            tune = codec + (r.encoder === 'cpu' ? 'CRF ' + r.quality : '质量档 ' + r.quality)
              + '（上限 ' + r.maxrate_kbps + ' kbps）';
          } else {
            // 原始档的码率是"按这个文件的源分辨率建议"来的；被原片封顶时要写清楚。
            const basis = r.suggested_from === 'rate70' ? '原片码率 70%' : '源分辨率';
            const bySource = !r.suggested_kbps ? ''
              : (r.video_kbps !== r.suggested_kbps
                ? '（建议 ' + r.suggested_kbps + ' kbps，已被原片封顶）'
                : '（按' + basis + '建议）');
            tune = codec + r.video_kbps + ' kbps' + bySource + (r.audio_disabled ? '（无音轨）' : '');
          }
          if (r.two_pass) tune += ' · 2-pass';
        }
        const src = (r.source_codec_label || r.source_codec || '')
          + (r.source_video_kbps ? ' · ' + r.source_video_kbps + ' kbps' + (r.source_estimated ? '（估算）' : '') : '');
        // 码率已到极限 ⇒ 不转码、原样放进 output（体积不变小）；原因细节收进 title。
        const cappedSkip = !!(r.capped && r.skip_reason);
        return h('tr', [
          h('td.zp-plan-name', { text: r.rel_path || r.name, title: r.rel_path ? r.name : '' }),
          h('td', { text: src || '—' }),
          h('td', { text: r.source_width ? r.source_width + 'x' + r.source_height : '—' }),
          h('td', { text: r.target_width ? r.target_width + 'x' + r.target_height : '—' }),
          h('td', { text: tune }),
          h('td', { text: cappedSkip ? '原样（不变小）' : sizeText(r.source_bytes, r.est_bytes, r.est_percent, r.estimate_unknown) }),
          h('td', {
            style: { color: r.skip_reason ? 'var(--warn)' : (r.capped ? 'var(--warn)' : 'var(--text-mute)') },
            title: cappedSkip ? r.skip_reason : '',
            text: cappedSkip ? '已跳过（码率已到极限）→ 会原样放入 output' : (r.skip_reason || r.note || '—'),
          }),
        ]);
      });

      const totalRow = h('tr', [
        h('td', { text: '合计' }),
        h('td', { colspan: '4', text: plan.runnable + ' 个可压' }),
        h('td', { text: plan.total_source_bytes ? sizeText(plan.total_source_bytes, plan.est_bytes, plan.est_percent, plan.estimate_unknown) : '—' }),
        h('td', { text: '' }),
      ]);

      body.append(h('div', { style: { lineHeight: '1.7' } }, [
        // 「按源类型给提示」放在计划区最上面：点「开始压缩」之前就能看到。
        videoPipelineTip(plan),
        // 码率已到极限的提醒必须是第一眼看到的（判据来自后端 capped）；
        // 细节（硬链接 / 清单文件名）收进 title，不占主句。
        plan.warning ? h('div.banner-warn', [
          h('span', { text: '⚠ ' + plan.warning, title: plan.warning_detail || '' }),
        ]) : null,
        h('div.field', [h('label', { text: '分辨率' }), presetRow, presetHint]),
        h('div.field', [h('label', { text: '编码器' }), encoderRow, encoderHint]),
        h('div.field', [h('label', { text: '模式' }), modeRow, modeHint]),
        tuneField,
        h('div.field', [
          h('label', { text: '高级：2-pass 编码（更慢约 2 倍，同体积更清晰）' }),
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
            passBox,
            h('span', { text: passAllowed ? '开启 2-pass（自动锁定 CPU + 目标码率）' : '仅 CPU + 目标码率可用' }),
          ]),
        ]),
        h('div.hint', {
          text: '目录 ' + (plan.dir || cwd) + '：' + plan.total + ' 个视频，可压 '
            + plan.runnable + ' 个，跳过 ' + plan.skipped + ' 个'
            + (plan.encoder_codec ? ' · 编码器 ' + plan.encoder_codec : ''),
        }),
        // 勾上后连子目录一起处理，产物按原目录结构放进 output/（默认不勾）。
        h('div.field', { style: { marginTop: '6px' } }, [
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', cursor: 'pointer' } }, [
            recBox,
            h('span', { text: '包含子目录（保持目录结构）' }),
          ]),
          h('div.hint', { text: '含子目录：连子目录里的视频一起处理，产物按原目录结构放进 output/' }),
        ]),
        // 递归扫描的如实说明：跳过的子目录 / 深度或数量上限。
        ...(plan.scan_notes || []).map((t) => h('div.hint', {
          style: { color: 'var(--warn)' }, text: '⚠ ' + t,
        })),
        ...(plan.scan_skipped || []).map((s) => h('div.hint', {
          text: '已跳过子目录 ' + s.rel_path + '：' + s.reason,
        })),
        h('div.hint', { text: plan.note || '' }),
        h('div.zp-plan-scroll', [
          h('table.table', { style: { fontSize: '12px' } }, [
            h('thead', [h('tr', ['文件', '源格式', '原分辨率', '目标', '目标码率/质量', '体积（原→预计）', '说明']
              .map((t) => h('th', { text: t })))]),
            h('tbody', rows.length ? rows : [h('tr', [h('td', { colspan: '7', text: '这个目录里没有视频' })])]),
            h('tfoot', rows.length ? [totalRow] : []),
          ]),
        ]),
        h('div.hint', { style: { marginTop: '8px' }, text: '产物写进 output/；关掉窗口也在后台跑' }),
        submitHint,
      ]));

      // 可压为 0 但"跳过的文件要原样放进 output"时也要能开始：这一趟产出完整一套。
      const canStart = !!plan.runnable || !!plan.place_count;
      const start = h('button.btn.btn-primary', {
        text: '开始压缩' + (plan.runnable ? '（' + plan.runnable + ' 个）' : ''),
        disabled: !canStart,
        title: canStart ? '在任务中心后台执行；关掉页面不受影响' : '没有可处理的内容',
      });
      startBtn = start;
      // 提交提示每次重画都归零（上一次的"提交失败"不该留在新计划上）。
      submitHint.style.display = 'none';
      submitHint.style.color = '';
      submitHint.textContent = '';
      start.addEventListener('click', async () => {
        const opts = {
          dir: cwd, preset: plan.preset, kbps: plan.kbps,
          encoder: plan.encoder, mode: plan.mode, quality: plan.quality, two_pass: plan.two_pass,
          // 勾了子目录才带 recursive（与计划表同口径）。
          ...(recursive ? { recursive: true } : {}),
          // 与计划表同口径：只处理选中的就必须把 names 一起交给任务（任务会重新规划）。
          ...(onlySelected && !recursive ? { names: selNames } : {}),
          // 计划表指纹（可压行的名字/相对路径+字节数）：任务重新探测后逐条核对，
          // 文件变了/不见了就如实跳过 —— 绝不静默按旧计划压。
          sources: (plan.rows || []).filter((r) => !r.skip_reason)
            .map((r) => ({
              name: r.name, bytes: r.source_bytes || 0,
              ...(r.rel_path ? { rel_path: r.rel_path } : {}),
            })),
        };
        start.disabled = true;
        submitHint.style.color = '';
        submitHint.style.display = '';
        submitHint.textContent = '正在提交压缩任务…';
        const id = await taskCenter.start({
          kind: 'video_compress', target: opts.dir,
          title: '压缩视频（' + plan.runnable + ' 个 · ' + plan.preset + (recursive ? ' · 含子目录' : '') + '）',
          start: () => api.fileVideoCompress(opts),
          onError: (msg) => {
            // 提交失败：弹窗留在原地显示**后端原话**（toast 会消失，不足以让用户看清原因）。
            // 409 的原因是"已有压缩任务在跑"，原样显示，绝不吞成"创建失败"。
            submitHint.style.color = 'var(--danger)';
            submitHint.textContent = '提交失败：' + msg;
            // 顺手指到那个正在跑的任务（同一时间只允许一个，两个一起跑都会变慢）。
            const run = taskCenter.findByKind('video_compress');
            if (run) {
              submitHint.append(' ', h('button.btn.btn-sm', {
                text: '打开进行中的压缩任务',
                onclick: () => taskCenter.openTask(run.id),
              }));
            }
            start.disabled = false;
          },
          onDone: (task) => {
            if (task && task.status && task.status !== 'succeeded') {
              toast('视频压缩失败：' + (task.error || task.status), 'err', 14000);
              load(cwd);
              return;
            }
            const r = (task && task.result) || {};
            // 汇总文案由后端分开计数（压缩 / 原样放入 output / 其它跳过），前端不重算；
            // 末尾附上本次转码用时（后端给整句，细节在进度窗的日志里）。
            toast('视频压缩完成：' + (r.summary_text || ((r.done || 0) + ' 个完成 / ' + (r.skipped || 0) + ' 个跳过'))
              + (r.duration_text ? ' · ' + r.duration_text : ''),
            r.failed ? 'warn' : 'ok', 14000);
            load(cwd);
          },
        });
        // 拿到 202（有任务编号）才关弹窗、开进度窗；失败留在弹窗里显示原因。
        if (id) m.close();
      });
      foot.append(start);
      // 「重新扫描」= 显式让服务端丢掉本目录的探测缓存并重探（正常改配置走缓存、不重探）。
      // title 必须写清它是干什么的，否则用户会以为它是普通的"刷新"。
      foot.append(h('button.btn', {
        text: '⟳ 重新扫描',
        title: '丢掉本目录的缓存并重新读取视频信息（文件已换但大小/时间没变时用它）',
        onclick: () => reload({ rescan: true }),
      }));
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
    }

    reload();
  }

  async function compressSelected() {
    const paths = [...selection];
    const names = paths.map((p) => basename(p));
    const format = h('select.select', [
      h('option', { value: 'zip', text: 'ZIP（通用）' }),
      h('option', { value: 'tar.gz', text: 'TAR.GZ（保留权限与软链接）' }),
      h('option', { value: 'tar', text: 'TAR（不压缩）' }),
    ]);
    const output = h('input.input', { value: (names.length === 1 ? names[0].replace(/\.[^.]+$/, '') : 'archive') + '.zip' });
    format.addEventListener('change', () => {
      const base = output.value.replace(/\.(zip|tar\.gz|tar)$/i, '');
      output.value = base + '.' + format.value;
    });
    const m = modal({
      title: `压缩 ${paths.length} 项`,
      body: h('div', [
        h('div.field', [h('label', { text: '压缩格式' }), format]),
        h('div.field', [h('label', { text: '输出文件名' }), output]),
        h('div.hint', { text: '归档会生成在当前目录下。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始打包',
          title: '大目录会在任务中心里跑，并逐条显示"已处理 N/M：路径"',
          onclick: async () => {
            close();
            try {
              await taskCenter.start({
                kind: 'file_compress', target: cwd,
                title: '打包 ' + names.length + ' 项',
                start: () => api.fileCompress(cwd, names, format.value, output.value),
                onDone: (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('打包失败：' + (task.error || task.status), 'err', 14000);
                    return;
                  }
                  const r = (task && task.result) || {};
                  toast(r.msg || '打包完成', 'ok', 9000);
                  load(cwd);
                },
              });
            } catch (e) { toast('打包失败：' + ((e && e.message) || e), 'err', 12000); }
          },
        }),
      ],
    });
  }

  // ---------- 编辑器 ----------
  //
  // 编辑器是**窗口化 + 全局存活**的（见文件末尾 createEditorWindow）：同一时刻只存在
  // 一个窗口，再次打开别的文件时在窗口里新开一个标签（不叠一层新窗口、也不顶掉旧标签）。
  //
  // 关键：编辑器层挂在 document.body 上，而路由切换只重建 #app 里的内容，所以窗口
  // 天然跨板块存活；离开文件页时**不再销毁它**（见本函数末尾的 cleanup）。这样
  // 最小化后的胶囊切到仪表盘/网站再切回来仍在，点开内容不丢。
  //
  // 编辑器由旧版 FilesView 创建，但它跨路由存活后，回调里捕获的旧 DOM 已经脱离。
  // 所以每次 FilesView 重建都把 refreshList / onDir 重新绑到**当前**这个视图上。
  function editorOptions() {
    return {
      roots: (lastList && lastList.roots) || [],
      refreshList: () => { if (content.isConnected) load(cwd); },
      onDir: (p) => { if (content.isConnected) load(p); },
    };
  }

  function openInEditorWindow(entry, res) {
    if (activeEditor) {
      activeEditor.setOptions(editorOptions());
      activeEditor.openFile(entry, res);
      return;
    }
    activeEditor = createEditorWindow(entry, res, Object.assign(editorOptions(), {
      onClosed: () => { activeEditor = null; },
    }));
  }

  async function openEditor(entry) {
    let res;
    try {
      res = await api.fileRead(entry.path);
    } catch (e) {
      toast(e.message, 'err');
      return;
    }
    if (res.binary) {
      modal({
        title: entry.name,
        body: h('div.empty', [
          h('div.big', { text: '🔒' }),
          h('h4', { text: '这是二进制文件' }),
          h('p', { text: '为避免破坏文件，不提供在线编辑。可以下载后用本地工具处理。' }),
        ]),
      });
      return;
    }
    if (res.too_large) {
      modal({
        title: entry.name,
        body: h('div.empty', [
          h('div.big', { text: '📦' }),
          h('h4', { text: '文件过大' }),
          h('p', { text: `该文件 ${humanSize(res.size)}，超过在线编辑上限（2MB）。请下载后编辑，或用 Web 终端处理。` }),
        ]),
      });
      return;
    }

    openInEditorWindow(entry, res);
  }

  // ---------- 搜索 ----------
  function searchModal() {
    const query = h('input.input', { placeholder: '文件名或内容关键词' });
    const mode = h('select.select', [
      h('option', { value: 'name', text: '按文件名搜索' }),
      h('option', { value: 'content', text: '按文件内容搜索' }),
    ]);
    const results = h('div', { style: { marginTop: '12px', maxHeight: '380px', overflow: 'auto' } });

    const doSearch = async () => {
      if (!query.value.trim()) { toast('请输入关键词', 'warn'); return; }
      clear(results);
      appendAll(results, h('div.empty', [h('p', { text: '搜索中…' })]));
      try {
        const r = await api.fileSearch(cwd, query.value.trim(), mode.value, 200);
        clear(results);
        if (!r.hits.length) {
          appendAll(results, h('div.empty', [h('p', { text: `没有找到匹配项（已扫描 ${r.scanned} 项，耗时 ${r.elapsed_ms}ms）` })]));
          return;
        }
        appendAll(results, 
          h('div.hint', { text: `找到 ${r.hits.length} 项，扫描 ${r.scanned} 项，耗时 ${r.elapsed_ms}ms` + (r.truncated ? '（结果已截断）' : '') }),
          h('table.table', [
            h('thead', [h('tr', [h('th', { text: '名称' }), h('th', { text: '路径' }), h('th', { text: '操作' })])]),
            h('tbody', r.hits.map((hit) => h('tr', [
              h('td', [
                h('div', { text: hit.name }),
                hit.match_line ? h('div', {
                  style: { fontSize: '11px', color: 'var(--text-mute)', fontFamily: 'var(--mono)' },
                  text: `第 ${hit.line_no} 行：${hit.match_line}`,
                }) : null,
              ]),
              h('td.mono', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: hit.path }),
              h('td', [
                h('button.btn.btn-sm', {
                  text: hit.is_dir ? '打开' : '编辑',
                  onclick: () => {
                    m.close();
                    if (hit.is_dir) load(hit.path);
                    else openEditor({ path: hit.path, name: hit.name });
                  },
                }),
                h('button.btn.btn-sm', {
                  text: '定位',
                  onclick: () => { m.close(); load(dirname(hit.path)); },
                }),
              ]),
            ]))),
          ]),
        );
      } catch (e) {
        clear(results);
        appendAll(results, h('div.empty', [h('p', { text: '搜索失败：' + e.message })]));
      }
    };
    query.addEventListener('keydown', (e) => { if (e.key === 'Enter') doSearch(); });

    const m = modal({
      title: `搜索：${cwd}`,
      wide: true,
      body: h('div', [
        h('div.row', [
          h('div.field', [h('label', { text: '关键词' }), query]),
          h('div.field', [h('label', { text: '搜索方式' }), mode]),
        ]),
        h('button.btn.btn-primary', { text: '开始搜索', onclick: doSearch }),
        results,
      ]),
    });
    setTimeout(() => query.focus(), 60);
  }

  // 键盘：Ctrl+A 全选、Ctrl+C/X/V 复制/剪切/粘贴、Delete 删除、F2 重命名、Enter 打开、
  // Esc 取消选择。输入框/弹窗里有焦点时一律不接管（否则在搜索框里按 Delete 会删文件）。
  function onKeyDown(e) {
    const tag = (document.activeElement && document.activeElement.tagName) || '';
    if (/^(INPUT|TEXTAREA|SELECT)$/.test(tag)) return;
    if (document.querySelector('.modal-mask')) return;
    // 编辑器窗口展开时不要抢它的快捷键（最小化成胶囊时文件列表照常可操作）。
    const edWin = document.querySelector('.zpf-layer .zpf-win');
    if (edWin && !edWin.classList.contains('zpf-win-min')) return;
    const mod = e.ctrlKey || e.metaKey;
    const key = e.key;

    if (mod && (key === 'a' || key === 'A')) {
      if (!visibleEntries.length) return;
      e.preventDefault(); selectAll(); return;
    }
    if (mod && (key === 'c' || key === 'C')) {
      if (!selection.size) return;
      e.preventDefault(); copySelection([...selection]); return;
    }
    if (mod && (key === 'x' || key === 'X')) {
      if (!selection.size) return;
      e.preventDefault(); cutSelection([...selection]); return;
    }
    if (mod && (key === 'v' || key === 'V')) {
      if (!(clipboard && clipboard.paths.length)) return;
      e.preventDefault(); pasteClipboard(); return;
    }
    if (key === 'Escape' && selection.size) {
      selection.clear(); selAnchor = -1; renderToolbar(); renderTable(); return;
    }
    if ((key === 'Delete' || key === 'Backspace') && selection.size) {
      e.preventDefault(); deleteSelected(); return;
    }
    if (key === 'F2' && selection.size) {
      const first = visibleEntries.find((x) => selection.has(x.path));
      if (!first) return;
      e.preventDefault(); renameEntry(first); return;
    }
    if (key === 'Enter' && selection.size) {
      const first = visibleEntries.find((x) => selection.has(x.path));
      if (!first) return;
      e.preventDefault();
      if (first.is_dir) load(first.path); else openAny(first);
    }
  }
  document.addEventListener('keydown', onKeyDown);
  // 右键菜单：点别处 / 滚动 / Esc 就关掉（与系统菜单一致）。
  // 锚点按钮自己例外 —— mousedown 先关、click 再开会让"再点一次收起"失效。
  const onDocPointer = (ev) => {
    if (!openCtxMenu || openCtxMenu.contains(ev.target)) return;
    if (dropdownAnchor && dropdownAnchor.contains(ev.target)) return;
    closeContextMenu();
  };
  const onDocScroll = () => closeContextMenu();
  document.addEventListener('mousedown', onDocPointer);
  window.addEventListener('scroll', onDocScroll, true);
  registerCleanup(() => {
    unsubOpBar();
    document.removeEventListener('keydown', onKeyDown);
    document.removeEventListener('mousedown', onDocPointer);
    window.removeEventListener('scroll', onDocScroll, true);
    closeContextMenu();
    // 编辑器**故意不在这里销毁**：它是全局存活的窗口（用户要求 1 —— 最小化后切成
    // 胶囊，切到别的板块再切回来仍要在、内容不能丢）。模块级的 activeEditor 引用
    // 也不会因为路由切换而失效。关掉它只有两条路：窗口右上角 ✕，或菜单「文件 →
    // 关闭编辑器」。未保存的内容由 beforeunload 守卫 + 关闭前确认保护。
  });
  // 编辑器可能由**上一次**的 FilesView 创建并一直存活：这里把它的 refreshList / onDir
  // 重新绑到当前这个视图，否则保存后的文件列表刷新会打在已经脱离文档的旧 DOM 上。
  if (activeEditor) activeEditor.setOptions(editorOptions());
  load(cwd || undefined);
  // 收藏存在服务端：进页面时拉一次（拿到之前工具栏不显示"已收藏"，不猜）。
  refreshFavorites();
}

// ============================================================================
//  在线编辑器（内嵌 CodeMirror 5，MIT）
//
//  为什么不再自己写（用户授权："让文件管理器和编辑器变成可用的现代
//  的工具，写不好可以直接引入开源项目。"）：
//  这里原来是一套自研的"透明 textarea + 高亮层"。字体、行高、内边距、Tab 宽度
//  任何一处不一致就会错位，而它真的错了两次且都是用户先发现的：
//    · 光标与文字逐列错开 —— 浏览器 UA 给 `<code>` 写死了 font-family: monospace，
//      直接作用在元素上的规则压过继承，两层字宽不一致；
//    · 第一次点进编辑区光标被拉到开头 —— focus 处理器里改了选区。
//  这类"自己维护一个文本编辑器"的账越滚越大，而 CodeMirror 5 是成熟稳定的选择：
//  光标/选区/撤销重做、查找替换、括号匹配、自动缩进、代码折叠、大文件视口渲染
//  全部现成，且是**单文件 UMD + 按需模式**，不需要任何构建步骤就能嵌进面板。
//
//  按需加载：CodeMirror 本体 + 语言模式约 470KB，只有真的打开编辑器时才加载，
//  不让它拖慢面板首屏（仪表盘/文件列表根本用不到）。
//  许可证与来源见 assets/vendor/codemirror/README.md（文件未做任何修改）。
// ============================================================================

// 扩展名 → 语言键。语言键再映射到 CodeMirror 的模式与依赖文件。
const EXT_LANG = {
  php: 'php', phtml: 'php',
  js: 'js', mjs: 'js', cjs: 'js', jsx: 'js',
  ts: 'ts', tsx: 'ts',
  json: 'json',
  go: 'go',
  py: 'py',
  sh: 'sh', bash: 'sh', zsh: 'sh',
  yaml: 'yaml', yml: 'yaml',
  html: 'html', htm: 'html',
  css: 'css', scss: 'css', less: 'css',
  sql: 'sql',
  ini: 'ini', conf: 'ini', cnf: 'ini', properties: 'ini', env: 'ini',
  md: 'md', markdown: 'md',
  xml: 'xml', svg: 'xml',
  dockerfile: 'docker',
};

// 每种语言：给用户看的名字 + CodeMirror 的 mode + 需要按顺序加载的模式文件。
//
// 依赖关系不能省：php 模式依赖 xml + javascript + css + htmlmixed + clike，
// htmlmixed 又依赖 xml + javascript + css —— 少加载一个，CodeMirror 会**静默**
// 退化成纯文本（控制台只留一行 undefined 模式名），用户看到的是"高亮没了"。
const CM_LANGS = {
  php: { label: 'PHP', mode: 'application/x-httpd-php', deps: ['xml', 'javascript', 'css', 'htmlmixed', 'clike', 'php'] },
  js: { label: 'JavaScript', mode: 'javascript', deps: ['javascript'] },
  ts: { label: 'TypeScript', mode: 'javascript', deps: ['javascript'] },
  json: { label: 'JSON', mode: { name: 'javascript', json: true }, deps: ['javascript'] },
  go: { label: 'Go', mode: 'go', deps: ['go'] },
  py: { label: 'Python', mode: 'python', deps: ['python'] },
  sh: { label: 'Shell', mode: 'shell', deps: ['shell'] },
  yaml: { label: 'YAML', mode: 'yaml', deps: ['yaml'] },
  html: { label: 'HTML', mode: 'htmlmixed', deps: ['xml', 'javascript', 'css', 'htmlmixed'] },
  xml: { label: 'XML', mode: 'xml', deps: ['xml'] },
  css: { label: 'CSS', mode: 'css', deps: ['css'] },
  sql: { label: 'SQL', mode: 'sql', deps: ['sql'] },
  ini: { label: 'INI / 配置', mode: 'properties', deps: ['properties'] },
  md: { label: 'Markdown', mode: 'markdown', deps: ['markdown'] },
  nginx: { label: 'Nginx', mode: 'text/x-nginx-conf', deps: ['nginx'] },
  docker: { label: 'Dockerfile', mode: 'text/x-dockerfile', deps: ['dockerfile'] },
};

// langKeyFor 判断某个文件名该用哪种语言（认不出来的返回空 = 纯文本）。
//
// 文件名判据优先于扩展名：`nginx.conf` / `Dockerfile` 都没有可用的扩展名，
// 只看后缀会把它们当纯文本 —— 而这两种恰恰是面板里最常改的文件。
function langKeyFor(name) {
  const base = String(name || '').split('/').pop().toLowerCase();
  if (base === 'dockerfile' || base.startsWith('dockerfile.')) return 'docker';
  if (base === 'nginx.conf' || /\.nginx$/.test(base)) return 'nginx';
  const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : '';
  return EXT_LANG[ext] || '';
}

// ---------------- 资源按需加载 ----------------
//
// 为什么自己写 <script> 注入而不是 `import()`：CodeMirror 5 是 UMD 包，
// 它把 CodeMirror 挂到 window 上；`import()` 一个 UMD 文件拿到的是它的
// module.exports，而模式/插件文件之间靠**全局 CodeMirror**互相注册 ——
// 混用两条路径会让插件注册到另一个实例上，表现为"模式加载了但没生效"。
const CM_ASSET_BASE = new URL('../vendor/codemirror/', import.meta.url);
const CM_CORE_CSS = [
  'codemirror.min.css',
  'addon/dialog/dialog.min.css',
  'addon/fold/foldgutter.min.css',
  'addon/scroll/simplescrollbars.min.css',
];
const CM_CORE_JS = [
  'codemirror.min.js',
  'addon/search/searchcursor.min.js',
  'addon/search/search.min.js',
  'addon/search/jump-to-line.min.js',
  'addon/dialog/dialog.min.js',
  'addon/edit/matchbrackets.min.js',
  'addon/edit/closebrackets.min.js',
  'addon/edit/continuelist.min.js',
  'addon/edit/matchtags.min.js',
  'addon/fold/foldcode.min.js',
  'addon/fold/foldgutter.min.js',
  'addon/fold/brace-fold.min.js',
  'addon/fold/xml-fold.min.js',
  'addon/fold/comment-fold.min.js',
  'addon/fold/indent-fold.min.js',
  'addon/selection/active-line.min.js',
  'addon/scroll/simplescrollbars.min.js',
  'addon/comment/comment.min.js',
];

// 内嵌资源通用加载器（CodeMirror 与 Plyr 共用）：注入 <script>/<link>，同一 URL 只加载一次。
const assetLoaded = new Set();

// CodeMirror 本体加载的进行中 Promise（成功后复用；失败置空以便重试）。
// ⚠️ 这一行曾被清理"死代码"时误删 —— 语法照样合法、门禁全绿，但浏览器报
// `Can't find variable: cmCorePromise`，编辑器直接打不开。门禁：tools/check-js-undeclared.mjs。
let cmCorePromise = null;

function loadAssetOnce(url, isCss) {
  if (assetLoaded.has(url)) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const el = isCss
      ? Object.assign(document.createElement('link'), { rel: 'stylesheet', href: url })
      : Object.assign(document.createElement('script'), { src: url, async: false });
    el.onload = () => { assetLoaded.add(url); resolve(); };
    el.onerror = () => reject(new Error('加载内嵌资源失败：' + url));
    document.head.appendChild(el);
  });
}

// ensureCodeMirror 保证本体与插件就绪（同一个 Promise 只加载一次；失败不缓存，
// 下次打开还能重试 —— 缓存失败的 Promise 会让编辑器"永远打不开"）。
function ensureCodeMirror() {
  if (!cmCorePromise) {
    cmCorePromise = (async () => {
      for (const f of CM_CORE_CSS) await loadAssetOnce(new URL(f, CM_ASSET_BASE).href, true);
      // 顺序加载 JS：插件依赖全局 CodeMirror，异步并行会让插件先于本体执行
      for (const f of CM_CORE_JS) await loadAssetOnce(new URL(f, CM_ASSET_BASE).href, false);
      if (!window.CodeMirror) throw new Error('CodeMirror 已加载但没有挂上 window.CodeMirror');
      ensureEditorStyle();
    })().catch((e) => { cmCorePromise = null; throw e; });
  }
  return cmCorePromise;
}

// ensureLang 按依赖顺序加载该语言需要的模式文件（模式之间也有依赖，见 CM_LANGS）。
async function ensureLang(langKey) {
  const def = CM_LANGS[langKey];
  if (!def) return;
  for (const m of def.deps) {
    await loadAssetOnce(new URL(`mode/${m}.min.js`, CM_ASSET_BASE).href, false);
  }
}

// ---------------- 播放器按需加载（Plyr） ----------------
//
// 与 CodeMirror 同一套做法：真点开音视频才注入 css/js（坑 188）。
// 不能用 `import()`：面板把 .mjs/.js 按 MIME 直发，UMD 在 module 里拿不到 root 会崩。
const PLYR_ASSET_BASE = new URL('../vendor/plyr/', import.meta.url);
let plyrPromise = null;

const PLYR_OPTIONS = {
  // 图标必须指到内嵌 svg：Plyr 默认指官方 CDN，运行时绝不联网（坑 188）。
  iconUrl: new URL('plyr.svg', PLYR_ASSET_BASE).href,
  preload: 'metadata',
  i18n: {
    restart: '重播', play: '播放', pause: '暂停', seek: '跳转', volume: '音量',
    mute: '静音', unmute: '取消静音', settings: '设置', speed: '速度',
    normal: '正常', quality: '画质', loop: '循环', enterFullscreen: '全屏',
    exitFullscreen: '退出全屏', pip: '画中画', played: '已播放', buffered: '已缓冲',
  },
};

function ensurePlyr() {
  if (!plyrPromise) {
    plyrPromise = (async () => {
      await loadAssetOnce(new URL('plyr.css', PLYR_ASSET_BASE).href, true);
      await loadAssetOnce(new URL('plyr.min.js', PLYR_ASSET_BASE).href, false);
      if (!window.Plyr) throw new Error('Plyr 已加载但没有挂上 window.Plyr');
      return window.Plyr;
    })().catch((e) => { plyrPromise = null; throw e; });
  }
  return plyrPromise;
}

// ---------------- 配色 ----------------
//
// 语义只有两档（键名沿用旧版：用户已经选过的 panel / monokai 都还认）：
//   · panel   → 跟随面板：浅色面板用官方浅色主题 eclipse、深色面板用官方深色主题
//               material-darker（`:root[data-theme]` 一变就实时跟着换）；
//   · monokai → 官方 monokai。
//
// 语法着色**全部**来自 CodeMirror 官方主题（`vendor/codemirror/theme/*.min.css`，
// 从 cdnjs `codemirror@5.65.16` 原样下载、未改动一个字）。
//
// 为什么删掉自研主题：旧版手写了一个 `cm-s-<自研名>` 主题的 token 颜色，又在弹窗
// 根节点上加一个类去覆盖面板 CSS 变量（--panel/--text/--bg-soft…），后者把弹窗里的
// 按钮与输入框一起染成 Monokai 色 —— 用户报的"按钮颜色有问题"。
// 现在的原则：编辑器配色只用官方主题，面板自己的控件永远用面板变量。
const ZPF_THEME_KEY = 'zp-file-editor-theme';
const ZPF_THEME_PANEL = 'panel';
const ZPF_THEME_MONOKAI = 'monokai';
const ZPF_CSS_ID = 'zpf-editor-style';

// CodeMirror 主题名 == theme/<name>.min.css 的文件名 == `.cm-s-<name>` 类名。
const CM_THEME_LIGHT = 'eclipse';
const CM_THEME_DARK = 'material-darker';
const CM_THEME_MONOKAI = 'monokai';

function readEditorTheme() {
  try {
    return localStorage.getItem(ZPF_THEME_KEY) === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
  } catch { return ZPF_THEME_PANEL; }
}

function saveEditorTheme(v) {
  try { localStorage.setItem(ZPF_THEME_KEY, v); } catch { /* 存不了就本次会话生效 */ }
}

// panelIsLight 读面板**真正生效**的主题：app.js 把 light/dark/auto 三态折算后
// 写到 documentElement 的 data-theme 上。读不到时按深色算 —— app.css 的
// `:root` 默认就是深色，两者一致。
function panelIsLight() {
  return document.documentElement.dataset.theme === 'light';
}

function cmThemeFor(setting) {
  if (setting === ZPF_THEME_MONOKAI) return CM_THEME_MONOKAI;
  return panelIsLight() ? CM_THEME_LIGHT : CM_THEME_DARK;
}

// ensureCmTheme 按需加载官方主题 CSS（与本体一样：用到哪个才加载哪个）。
function ensureCmTheme(name) {
  return loadAssetOnce(new URL(`theme/${name}.min.css`, CM_ASSET_BASE).href, true);
}

// ensureEditorStyle 注入**最小布局**样式（只注入一次）。
//
// 只负责"让 CodeMirror 撑满容器"和"查找框跟随面板"；背景/前景/语法色一律交给
// 官方主题，所以这里**不许**再出现任何 token 颜色或面板变量覆盖。
function ensureEditorStyle() {
  if (document.getElementById(ZPF_CSS_ID)) return;
  const st = document.createElement('style');
  st.id = ZPF_CSS_ID;
  st.textContent = [
    // 编辑器高度铺满外层容器（CodeMirror 需要一个有高度的父元素）
    '.zpf-cm { position: relative; display: flex; flex-direction: column; min-height: 0; }',
    '.zpf-cm .CodeMirror { flex: 1 1 auto; height: auto; min-height: 0; font-family: var(--mono); font-size: 13px; line-height: 1.6; }',
    // 查找/替换对话框：CodeMirror 自带的是浅色浮层，这里让它跟随面板
    '.zpf-cm .CodeMirror-dialog { background: var(--panel); color: var(--text); border-top: 1px solid var(--border); padding: 6px 10px; font-size: 12.5px; }',
    '.zpf-cm .CodeMirror-dialog input { background: var(--bg-soft); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 3px 6px; font-family: var(--mono); }',
    '.zpf-cm .CodeMirror-dialog button { background: var(--panel-2); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 2px 8px; cursor: pointer; }',
  ].join('\n');
  document.head.appendChild(st);
}

// ---------------- 编辑器窗口（窗口化：图标按钮 + 菜单栏 + 目录树 + 状态栏） ----------------
//
// 用户原话："文件编辑器还是太差了，能不能照抄宝塔的吗？"
// 宝塔的文件编辑器是"窗口 + 目录树 + 菜单栏"的形态，所以这里按那套重做：
//   · 右上角是**图标按钮**（最小化 / 最大化 / 关闭），不是文字按钮，每个都有中文 title；
//   · 左侧目录树：展开/折叠、点目录切换浏览目录、点文件切换编辑文件、当前文件高亮、
//     宽度可拖动（记在 localStorage）；
//   · 菜单栏把已有能力（保存/另存为/查找/跳行/刷新/下载/关闭…）收进「文件/编辑/视图/帮助」；
//   · 窗口可拖动，位置/大小记在 localStorage；打开与 resize 都先夹进视口（坑 220）；
//   · 「最大化」= 铺满**面板 content 区域**（不是浏览器全屏、不是系统全屏）；
//   · 「最小化」= 右下角胶囊，**不加任何遮罩**。
//
// 为什么不再用 modal()：modal 的遮罩是 `position:fixed; inset:0`，最小化时若忘了
// 写 `pointer-events:none`，收起后整块透明遮罩仍然吞掉所有点击 —— 这正是用户报的
// "最小化后点不了面板别处"。这里自己起一层 `.zpf-layer`（`pointer-events:none`）
// + 窗口本身（`pointer-events:auto`），从结构上杜绝"全屏遮罩挡住点击"。

const ZPF_TREE_W_KEY = 'zp-file-editor-tree-w';
const ZPF_TREE_HIDDEN_KEY = 'zp-file-editor-tree-hidden';
const ZPF_POS_KEY = 'zp-file-editor-pos';

function clamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }

function readLS(key) { try { return localStorage.getItem(key); } catch { return null; } }
function writeLS(key, v) { try { localStorage.setItem(key, String(v)); } catch { /* 存不了就本次会话生效 */ } }

// contentRect 返回面板内容区（`.content`）的视口坐标。最大化语义以它为界。
function contentRect() {
  const el = document.querySelector('.content');
  if (el) {
    const r = el.getBoundingClientRect();
    if (r.width > 80 && r.height > 80) return r;
  }
  return { left: 0, top: 0, right: window.innerWidth, bottom: window.innerHeight, width: window.innerWidth, height: window.innerHeight };
}

// clampEditorGeom 把窗口几何夹进可用区（纯函数，可单测，坑 220）。
// 越界 → 平移进边界；尺寸非法/装不下 → 回退到居中的默认几何（占可用区 84%），绝不沿用坏几何。
// g/view 都是 layer 内坐标；返回 {left, top, width, height, fellBack}。
function clampEditorGeom(g, view) {
  const MIN_W = 320;   // 与 CSS .zpf-win 的 min-width 一致
  const MIN_H = 200;   // 与 CSS .zpf-win 的 min-height 一致
  const RATIO = 0.84;  // 默认几何占可用区的比例（80%~90%）
  const MARGIN = 8;    // 与视口边缘留的边距
  const vw = Math.max(1, Math.round(Number(view && view.width) || 0));
  const vh = Math.max(1, Math.round(Number(view && view.height) || 0));
  const vx = Math.round(Number(view && view.left) || 0);
  const vy = Math.round(Number(view && view.top) || 0);
  // 可用区先按边距收缩；太小就不缩，否则连最小尺寸都放不下。
  const mx = Math.min(MARGIN, Math.max(0, Math.floor((vw - MIN_W) / 2)));
  const my = Math.min(MARGIN, Math.max(0, Math.floor((vh - MIN_H) / 2)));
  const aw = Math.max(1, vw - mx * 2);
  const ah = Math.max(1, vh - my * 2);
  const x0 = vx + mx;
  const y0 = vy + my;
  const minW = Math.min(MIN_W, aw);
  const minH = Math.min(MIN_H, ah);
  const isNum = (v) => typeof v === 'number' && Number.isFinite(v);
  const has = !!(g && isNum(g.left) && isNum(g.top) && isNum(g.width) && isNum(g.height));
  const w = has ? Math.round(g.width) : 0;
  const h = has ? Math.round(g.height) : 0;
  const badSize = !has || w < minW || h < minH || w > aw || h > ah;
  if (!badSize) {
    return {
      left: Math.round(Math.min(x0 + aw - w, Math.max(x0, g.left))),
      top: Math.round(Math.min(y0 + ah - h, Math.max(y0, g.top))),
      width: w,
      height: h,
      fellBack: false,
    };
  }
  const dw = Math.min(aw, Math.max(minW, Math.round(aw * RATIO)));
  const dh = Math.min(ah, Math.max(minH, Math.round(ah * RATIO)));
  return {
    left: Math.round(x0 + (aw - dw) / 2),
    top: Math.round(y0 + (ah - dh) / 2),
    width: dw,
    height: dh,
    fellBack: true,
  };
}

// readStoredGeom 读持久化几何：`{left,top,width,height}`；旧格式 `{x,y}`（相对默认位的偏移）标记成 legacy。
function readStoredGeom() {
  try {
    const v = JSON.parse(readLS(ZPF_POS_KEY) || 'null');
    if (v && ['left', 'top', 'width', 'height'].every((k) => typeof v[k] === 'number' && Number.isFinite(v[k]))) {
      return { left: v.left, top: v.top, width: v.width, height: v.height };
    }
    if (v && typeof v.x === 'number' && Number.isFinite(v.x) && typeof v.y === 'number' && Number.isFinite(v.y)) {
      return { legacy: { x: v.x, y: v.y } };
    }
  } catch { /* 坏值走默认几何 */ }
  return null;
}

// pickTreeRoot 选目录树的根：优先用白名单根目录里**包含该文件**的那个（最长匹配），
// 否则退到文件所在目录 —— 这样树不会从一个莫名其妙的祖先开始。
function pickTreeRoot(filePath, roots) {
  const p = String(filePath || '');
  let best = '';
  for (const raw of roots || []) {
    const r = String(raw || '').replace(/\/+$/, '');
    if (!r) continue;
    if ((p === r || p.startsWith(r + '/')) && r.length > best.length) best = r;
  }
  if (best) return best;
  return dirname(p) || '/';
}

function cssEsc(s) {
  if (window.CSS && CSS.escape) return CSS.escape(String(s));
  return String(s).replace(/["\\]/g, '\\$&');
}

// createEditorWindow 打开一个编辑器窗口（同一时刻只应存在一个）。
// entry/res 是初始文件；opts.roots 是文件白名单根目录；opts.refreshList 刷新背后的文件列表；
// opts.onDir 是"树里点了目录"时要切换的浏览目录；opts.onClosed 用于让调用方清掉引用。
//
// 多标签（用户要求 2）：窗口里可以有多个文件标签。做法是**一个 CodeMirror 实例 +
// 每个标签一份 CodeMirror.Doc**，切换标签用 cm.swapDoc()。为什么不是每个标签造一个
// 编辑器实例：Doc 会自带**各自的撤销历史、光标与滚动位置**，这正好是标签切换要的语义，
// 而一个实例只维护一套 DOM/事件，代价最小。查找框、配色、缩进等实例级选项全局共用。
function createEditorWindow(entry0, res0, opts = {}) {
  let roots = opts.roots || [];
  let refreshList = typeof opts.refreshList === 'function' ? opts.refreshList : () => {};
  let onDir = typeof opts.onDir === 'function' ? opts.onDir : () => {};
  const onClosed = typeof opts.onClosed === 'function' ? opts.onClosed : () => {};

  // 活动标签的"镜像"变量：下面大量既有函数直接读写 entry/res/dirty 等，切标签时
  // 由 activateTab() 先 commit 回标签对象、再把这些变量换成新标签的值，改动面最小。
  let entry = entry0;
  let res = res0;
  let cm = null;
  let dirty = false;
  let theme = readEditorTheme();
  let themeSeq = 0;
  let maximized = false;
  let minimized = false;
  let disposed = false;
  let langKey = langKeyFor(entry.name);
  let langDef = CM_LANGS[langKey];

  // ---- 标签状态 ----
  // tab: { id, entry, res, doc, dirty, langKey, langDef, cleanGen }
  const tabs = [];
  let tabSeq = 0;
  let activeTabId = null;

  function currentTab() { return tabs.find((t) => t.id === activeTabId) || null; }
  function tabForPath(p) { return tabs.find((t) => t.entry.path === p) || null; }
  function mkTab(e, r) {
    const key = langKeyFor(e.name);
    return { id: 'zpf-filetab-' + (++tabSeq), entry: e, res: r, doc: null, dirty: false, langKey: key, langDef: CM_LANGS[key], cleanGen: 0 };
  }
  // snapshotActive 把镜像变量写回活动标签对象（切换/保存前调用）。
  function snapshotActive() {
    const t = currentTab();
    if (!t) return;
    t.entry = entry; t.res = res; t.dirty = dirty; t.langKey = langKey; t.langDef = langDef;
  }

  const firstTab = mkTab(entry0, res0);
  tabs.push(firstTab);
  activeTabId = firstTab.id;

  // ---- 目录树状态 ----
  let treeRoot = pickTreeRoot(entry.path, roots);
  let treeWidth = clamp(Number(readLS(ZPF_TREE_W_KEY)) || 230, 150, 460);
  let treeHidden = readLS(ZPF_TREE_HIDDEN_KEY) === '1';
  const treeChildren = new Map(); // dir -> entries[]
  const treeExpanded = new Set([treeRoot]);
  const treeLoading = new Set();
  let curDir = dirname(entry.path);

  // ---- 窗口几何记忆（相对 .zpf-layer 的 left/top/width/height） ----
  let geom = null;

  // ===================== DOM =====================
  const titleText = h('span.zpf-title');
  const titlePath = h('span.zpf-title-path');
  const dirtyDot = h('span.zpf-dirty-dot', { style: { display: 'none' } });

  const minBtn = h('button.zpf-iconbtn', { text: '–', title: '最小化（收成右下角胶囊，面板仍可操作）', 'aria-label': '最小化' });
  const maxBtn = h('button.zpf-iconbtn', { text: '⛶', title: '最大化（铺满面板内容区）', 'aria-label': '最大化' });
  const closeBtn = h('button.zpf-iconbtn.zpf-close', { text: '✕', title: '关闭编辑器（有未保存修改会先确认；Esc 不会关闭编辑器）', 'aria-label': '关闭' });

  const titlebar = h('div.zpf-titlebar', [
    h('span', { text: '📝', style: { fontSize: '13px' } }),
    titleText,
    titlePath,
    h('div.spacer'),
    dirtyDot,
    minBtn,
    maxBtn,
    closeBtn,
  ]);

  const menubar = h('div.zpf-menubar');
  const tabStrip = h('div.zpf-filetabs');

  const treeHead = h('div.zpf-tree-head');
  const treeScroll = h('div.zpf-tree-scroll');
  const treeEl = h('aside.zpf-tree', [treeHead, treeScroll]);
  const treeResizer = h('div.zpf-tree-resizer', { title: '拖动调整目录树宽度（双击还原默认宽度）' });

  const host = h('div.zpf-cm');
  const statusInfo = h('span');
  const statusbar = h('div.zpf-statusbar', [statusInfo]);
  const editEl = h('section.zpf-edit', [host, statusbar]);
  const bodyEl = h('div.zpf-body', [treeEl, treeResizer, editEl]);

  const win = h('div.zpf-win', [titlebar, menubar, tabStrip, bodyEl]);
  const layer = h('div.zpf-layer', [win]);
  document.body.appendChild(layer);

  // ===================== 几何：窗口 / 最大化 / 最小化 =====================

  // layerViewRect 返回窗口可用区：`.zpf-layer` 的实际矩形（layer 内坐标，layer 是 fixed inset:0）。
  // 不用 window.innerWidth 猜 —— layer 才是窗口绝对定位的容器（坑 220）。
  function layerViewRect() {
    const r = layer.getBoundingClientRect();
    if (r.width > 80 && r.height > 80) return { left: 0, top: 0, width: r.width, height: r.height };
    return { left: 0, top: 0, width: window.innerWidth, height: window.innerHeight };
  }

  // persistGeom 把**夹过**的几何写回 localStorage（下次打开不再复发）。最大化/最小化不写。
  function persistGeom() {
    if (!geom || maximized || minimized) return;
    writeLS(ZPF_POS_KEY, JSON.stringify(geom));
  }

  function applyGeometry() {
    if (disposed || minimized) return; // 胶囊位置由 CSS 固定（zpf-win-min 带 !important）
    ensureContentObserver(); // 路由切换会换掉 .content 元素，最大化几何要贴着**当前**那个
    if (maximized) {
      // 「最大化」= 与 content 区域逐像素重合（不是浏览器全屏），语义不变。
      const r = contentRect();
      win.style.left = Math.round(r.left) + 'px';
      win.style.top = Math.round(r.top) + 'px';
      win.style.width = Math.round(r.width) + 'px';
      win.style.height = Math.round(r.height) + 'px';
      return;
    }
    const view = layerViewRect();
    const stored = geom || readStoredGeom();
    // 旧格式 {x,y} 是相对默认位的偏移：贴到默认几何上再夹，一次迁移。
    let g = stored;
    if (stored && stored.legacy) {
      const d = clampEditorGeom(null, view);
      g = { left: d.left + stored.legacy.x, top: d.top + stored.legacy.y, width: d.width, height: d.height };
    }
    const next = clampEditorGeom(g, view);
    geom = { left: next.left, top: next.top, width: next.width, height: next.height };
    win.style.left = next.left + 'px';
    win.style.top = next.top + 'px';
    win.style.width = next.width + 'px';
    win.style.height = next.height + 'px';
    persistGeom();
  }

  function refreshCM() { requestAnimationFrame(() => { if (cm) { cm.setSize(null, '100%'); cm.refresh(); } }); }

  function setMaximized(on) {
    maximized = !!on;
    if (maximized && minimized) setMinimizedState(false);
    win.classList.toggle('zpf-win-max', maximized);
    applyGeometry();
    syncChrome();
    refreshCM();
  }

  function setMinimizedState(on) {
    minimized = !!on;
    if (minimized) maximized = false;
    win.classList.toggle('zpf-win-min', minimized);
    win.classList.toggle('zpf-win-max', maximized);
    applyGeometry();
    syncChrome();
    setDirty(dirty);
    if (!minimized) { refreshCM(); if (cm) cm.focus(); }
  }

  function syncChrome() {
    maxBtn.textContent = maximized ? '🗗' : '⛶';
    maxBtn.title = maximized ? '还原窗口（回到面板内容区里可拖动的位置）' : '最大化（铺满面板内容区）';
    maxBtn.setAttribute('aria-label', maximized ? '还原' : '最大化');
    minBtn.title = minimized ? '还原窗口' : '最小化（收成右下角胶囊，面板仍可操作）';
    minBtn.setAttribute('aria-label', minimized ? '还原' : '最小化');
    treeEl.style.display = treeHidden ? 'none' : '';
    treeResizer.style.display = treeHidden ? 'none' : '';
    if (!treeHidden) treeEl.style.width = treeWidth + 'px';
  }

  // ===================== 标题 / 状态 =====================
  function updateTitle() {
    titleText.textContent = '编辑：' + entry.name;
    titlePath.textContent = entry.path;
    win.title = entry.path;
  }

  function updateStatus() {
    if (!cm) { statusInfo.textContent = '正在加载编辑器…'; return; }
    const c = cm.getCursor();
    statusInfo.textContent = (langDef ? langDef.label : '纯文本') + ' · ' + cm.lineCount() + ' 行 · '
      + humanSize(res.size) + ' · 第 ' + (c.line + 1) + ' 行，第 ' + (c.ch + 1) + ' 列';
  }

  let unloadGuardOn = false;
  // 只要**任意一个标签**有未保存修改，关闭/刷新浏览器就要拦一次（不只是活动标签）。
  function onBeforeUnload(e) { if (!tabs.some((t) => t.dirty)) return; e.preventDefault(); e.returnValue = ''; }
  function installUnloadGuard() { if (!unloadGuardOn) { unloadGuardOn = true; window.addEventListener('beforeunload', onBeforeUnload); } }
  function removeUnloadGuard() { if (unloadGuardOn) { unloadGuardOn = false; window.removeEventListener('beforeunload', onBeforeUnload); } }
  function syncUnloadGuard() { if (tabs.some((t) => t.dirty)) installUnloadGuard(); else removeUnloadGuard(); }

  // setDirty 只作用于**活动标签**（镜像变量 dirty 也同步）。后台标签的状态由
  // saveAll / closeTab 直接改标签对象，改完调 renderTabs()。
  function setDirty(v) {
    const t = currentTab();
    const val = !!v;
    const changed = !t || t.dirty !== val;
    if (t) t.dirty = val;
    dirty = val;
    dirtyDot.textContent = '● 未保存';
    dirtyDot.style.display = val ? '' : 'none';
    syncUnloadGuard();
    if (changed) renderTabs();
  }

  // ===================== 标签栏 =====================
  // 每次重画整条标签栏（标签数量是个位数，成本可忽略）；标签过多时靠 CSS
  // overflow-x:auto 横向滚动，并把活动标签滚进可视区。
  function renderTabs() {
    if (disposed) return;
    clear(tabStrip);
    for (const t of tabs) {
      const active = t.id === activeTabId;
      const dot = h('span.zpf-filetab-dot', { text: '●', title: '有未保存的修改', style: { display: t.dirty ? '' : 'none' } });
      const nameEl = h('span.zpf-filetab-name', { text: t.entry.name, title: t.entry.path });
      const x = h('button.zpf-filetab-close', { type: 'button', text: '✕', title: '关闭这个标签（未保存会先确认）', 'aria-label': '关闭标签' });
      x.addEventListener('click', (e) => { e.stopPropagation(); closeTab(t); });
      const el = h('div.zpf-filetab' + (active ? '.active' : ''), [dot, nameEl, x]);
      el.title = t.entry.path;
      el.addEventListener('click', () => { if (t.id !== activeTabId) activateTab(t); });
      tabStrip.appendChild(el);
    }
    const act = tabStrip.querySelector('.zpf-filetab.active');
    if (act && act.scrollIntoView) act.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }

  // ===================== 菜单栏 =====================
  let openMenuIdx = -1;
  // 鼠标划过菜单标题会切换打开的菜单；此时紧接着的点击**不应该把它关掉**
  // （否则"从「文件」划到「编辑」再点一下"会把编辑菜单关了 —— 真实用户会以为点坏了）。
  let menuOpenedByHover = false;

  function closeMenus() {
    openMenuIdx = -1;
    menuOpenedByHover = false;
    menubar.querySelectorAll('.zpf-menubar-item').forEach((el) => el.classList.remove('open'));
  }

  function renderMenuPop(pop, def) {
    clear(pop);
    for (const it of def.items()) {
      if (it.sep) { pop.appendChild(h('div.zpf-menu-sep')); continue; }
      const mark = it.checked === true ? '✓ ' : (it.checked === false ? '　' : '');
      pop.appendChild(h('button.zpf-menu-row', {
        disabled: !!it.disabled,
        type: 'button',
        onclick: () => {
          closeMenus();
          try { it.run(); } catch (e) { toast('操作失败：' + ((e && e.message) || e), 'err'); }
        },
      }, [
        h('span', { text: mark + it.label }),
        it.hint ? h('span.zpf-menu-hint', { text: it.hint }) : null,
      ]));
    }
  }

  function buildMenus() {
    clear(menubar);
    menuDefs().forEach((def, idx) => {
      const title = h('button.zpf-menu-title', { text: def.label, type: 'button' });
      const pop = h('div.zpf-menu-pop');
      const item = h('div.zpf-menubar-item', [title, pop]);
      const openIt = () => { closeMenus(); item.classList.add('open'); openMenuIdx = idx; renderMenuPop(pop, def); };
      title.addEventListener('click', (e) => {
        e.stopPropagation();
        if (openMenuIdx === idx) {
          if (menuOpenedByHover) { menuOpenedByHover = false; return; } // 刚被 hover 打开，这一击不关
          closeMenus();
        } else openIt();
      });
      title.addEventListener('mouseenter', () => {
        if (openMenuIdx >= 0 && openMenuIdx !== idx) { openIt(); menuOpenedByHover = true; }
      });
      item.addEventListener('click', (e) => e.stopPropagation());
      menubar.appendChild(item);
    });
  }

  function menuDefs() {
    return [
      {
        label: '文件',
        items: () => [
          { label: '保存', hint: '⌘S', disabled: !cm, run: () => saveFile() },
          { label: '全部保存', hint: '⌘⌥S', disabled: !cm || !tabs.some((t) => t.dirty), run: saveAll },
          { label: '另存为…', hint: '⌘⇧S', disabled: !cm, run: saveAs },
          { label: '重新载入（放弃未保存修改）', disabled: !cm, run: reloadFile },
          { sep: true },
          { label: '下载文件', run: downloadFile },
          { label: '复制文件路径', run: () => copyText(entry.path) },
          { sep: true },
          { label: '关闭编辑器', run: requestClose },
        ],
      },
      {
        label: '编辑',
        items: () => [
          { label: '撤销', hint: '⌘Z', disabled: !cm, run: () => cm.execCommand('undo') },
          { label: '重做', hint: '⌘⇧Z', disabled: !cm, run: () => cm.execCommand('redo') },
          { sep: true },
          { label: '查找替换', hint: '⌘F', disabled: !cm, run: () => cm.execCommand('findPersistent') },
          { label: '跳转到行…', hint: '⌥G', disabled: !cm, run: () => cm.execCommand('jumpToLine') },
          { sep: true },
          { label: '切换注释', hint: '⌘/', disabled: !cm, run: () => cm.execCommand('toggleComment') },
          { label: '全选', hint: '⌘A', disabled: !cm, run: () => { cm.focus(); cm.execCommand('selectAll'); } },
        ],
      },
      {
        label: '视图',
        items: () => [
          { label: maximized ? '还原窗口' : '最大化', run: () => setMaximized(!maximized) },
          { label: treeHidden ? '显示目录树' : '隐藏目录树', run: toggleTree },
          { label: '刷新目录树', run: refreshTree },
          { sep: true },
          { label: '自动换行', checked: !!(cm && cm.getOption('lineWrapping')), disabled: !cm, run: toggleWrap },
          { label: '折叠全部', disabled: !cm, run: () => cm.execCommand('foldAll') },
          { label: '展开全部', disabled: !cm, run: () => cm.execCommand('unfoldAll') },
          { sep: true },
          { label: '配色：跟随面板', checked: theme === ZPF_THEME_PANEL, run: () => applyTheme(ZPF_THEME_PANEL) },
          { label: '配色：Monokai', checked: theme === ZPF_THEME_MONOKAI, run: () => applyTheme(ZPF_THEME_MONOKAI) },
          { sep: true },
          { label: fsElement() ? '退出浏览器全屏' : '浏览器全屏', run: toggleFullscreen },
        ],
      },
      {
        label: '帮助',
        items: () => [
          { label: '快捷键说明', run: showShortcuts },
          { label: '关于在线编辑器', run: showAbout },
        ],
      },
    ];
  }

  function showShortcuts() {
    const rows = [
      ['⌘ / Ctrl + S', '保存当前标签'],
      ['⌘ / Ctrl + ⌥ / Alt + S', '全部保存（依次写盘，汇总成功/失败）'],
      ['⌘ / Ctrl + ⇧ + S', '另存为'],
      ['⌘ / Ctrl + F', '查找 / 替换'],
      ['⌥ / Alt + G', '跳转到行'],
      ['⌘ / Ctrl + /', '切换注释'],
      ['Tab / ⇧ + Tab', '缩进 / 反缩进'],
      ['Esc', '关闭菜单 / 查找框（**不会**关闭编辑器）'],
      ['✕ / 菜单「关闭编辑器」', '关闭编辑器（未保存会先确认）'],
    ];
    modal({
      title: '编辑器快捷键',
      body: h('table.table', [
        h('thead', [h('tr', [h('th', { text: '按键' }), h('th', { text: '作用' })])]),
        h('tbody', rows.map((r) => h('tr', [h('td.mono', { text: r[0] }), h('td', { text: r[1] })]))),
      ]),
    });
  }

  function showAbout() {
    modal({
      title: '关于在线编辑器',
      body: h('div', { style: { fontSize: '13px', lineHeight: '1.8' } }, [
        h('p', { text: '内嵌 CodeMirror 5（MIT 许可），随面板一起分发，不依赖任何外部 CDN，也没有构建步骤。' }),
        h('p', { text: '目录树、菜单栏与窗口行为（拖动 / 最小化 / 最大化）由 ZizPanel 自己实现。' }),
      ]),
    });
  }

  // ===================== 目录树 =====================
  async function treeLoad(dir) {
    if (treeChildren.has(dir)) return treeChildren.get(dir);
    if (treeLoading.has(dir)) return [];
    treeLoading.add(dir);
    try {
      const l = await api.files(dir, false);
      treeChildren.set(dir, (l.entries || []).slice());
      return treeChildren.get(dir);
    } catch (e) {
      treeChildren.set(dir, []);
      if (!disposed) toast('目录树读取失败：' + dir + '（' + ((e && e.message) || e) + '）', 'warn', 10000);
      return [];
    } finally {
      treeLoading.delete(dir);
      if (!disposed) treeRender();
    }
  }

  function sortedKids(list) {
    // 目录树同样走唯一的比较器（名字升序 + 目录在前），不再留第二份实现。
    return sortEntries(list, 'name', 1);
  }

  function treeRow(e, depth) {
    const active = !e.is_dir && e.path === entry.path;
    const isCurDir = e.is_dir && e.path === curDir;
    const row = h('div.zpf-tree-row' + (active ? '.active' : '') + (isCurDir ? '.current-dir' : ''), {
      style: { paddingLeft: (6 + depth * 12) + 'px' },
      dataset: { path: e.path },
      title: e.path,
    });
    row.appendChild(h('span.zpf-tree-caret', {
      text: e.is_dir ? (treeExpanded.has(e.path) ? '▾' : '▸') : '',
      onclick: (ev) => { if (!e.is_dir) return; ev.stopPropagation(); toggleDir(e.path); },
    }));
    row.appendChild(h('span', { text: e.is_dir ? (treeExpanded.has(e.path) ? '📂' : '📁') : fileIcon(e.name) }));
    row.appendChild(h('span.zpf-tree-name', { text: e.name }));
    row.addEventListener('click', () => {
      if (e.is_dir) {
        treeExpanded.add(e.path);
        curDir = e.path;
        treeRender();
        treeLoad(e.path);
        onDir(e.path); // 切换当前浏览目录（背后的文件列表跟着走）
      } else {
        openFileByTree(e.path, e.name);
      }
    });
    return row;
  }

  function treeRender() {
    if (disposed) return;
    clear(treeScroll);
    const nodes = [h('div.zpf-tree-row.zpf-tree-root', { title: treeRoot }, [
      h('span.zpf-tree-caret', {
        text: treeExpanded.has(treeRoot) ? '▾' : '▸',
        onclick: (ev) => { ev.stopPropagation(); toggleDir(treeRoot); },
      }),
      h('span', { text: '🗂️' }),
      h('span.zpf-tree-name', { text: basename(treeRoot) || treeRoot, style: { fontWeight: '600' } }),
    ])];
    const walk = (dir, depth) => {
      for (const e of sortedKids(treeChildren.get(dir) || [])) {
        nodes.push(treeRow(e, depth));
        if (e.is_dir && treeExpanded.has(e.path)) walk(e.path, depth + 1);
      }
    };
    if (treeExpanded.has(treeRoot)) walk(treeRoot, 1);
    appendAll(treeScroll, nodes);
    locateCurrent();
  }

  function toggleDir(path) {
    if (treeExpanded.has(path)) treeExpanded.delete(path);
    else { treeExpanded.add(path); if (!treeChildren.has(path)) treeLoad(path); }
    treeRender();
  }

  function locateCurrent() {
    const el = treeScroll.querySelector('.zpf-tree-row[data-path="' + cssEsc(entry.path) + '"]');
    if (el) el.scrollIntoView({ block: 'nearest' });
  }

  // revealFile 把当前文件在树里定位出来：展开祖先目录 → 高亮 → 滚到可见处。
  async function revealFile(file) {
    const d = dirname(file);
    const rootPrefix = treeRoot.replace(/\/+$/, '');
    const chain = [];
    let cur = d;
    while (cur && (cur === treeRoot || cur.startsWith(rootPrefix + '/'))) {
      chain.unshift(cur);
      if (cur === treeRoot) break;
      const parent = dirname(cur);
      if (parent === cur) break;
      cur = parent;
    }
    for (const dir of chain) {
      treeExpanded.add(dir);
      if (!treeChildren.has(dir)) await treeLoad(dir);
    }
    treeRender();
  }

  function refreshTree() {
    treeChildren.clear();
    treeLoad(treeRoot).then(() => revealFile(entry.path));
  }

  function toggleTree() {
    treeHidden = !treeHidden;
    writeLS(ZPF_TREE_HIDDEN_KEY, treeHidden ? '1' : '0');
    syncChrome();
    refreshCM();
  }

  // ===================== 打开 / 切换文件（多标签） =====================
  // activateTab 把活动标签切到 t：先把镜像变量写回旧标签，再把镜像换成 t 的值，
  // 最后让 CodeMirror 显示 t 的 Doc（swapDoc 会保留该标签自己的撤销历史/光标/滚动）。
  async function activateTab(t) {
    if (!t || disposed) return;
    snapshotActive();
    activeTabId = t.id;
    entry = t.entry;
    res = t.res;
    dirty = t.dirty;
    langKey = t.langKey;
    langDef = t.langDef;
    curDir = dirname(entry.path);
    renderTabs();
    // 语言模式必须先就绪：模式文件没加载完就建 Doc，CodeMirror 会**静默**退化成纯文本。
    try { await ensureLang(langKey); } catch (e) { toast('语言模式加载失败：' + ((e && e.message) || e), 'warn', 8000); }
    if (disposed) return;
    langDef = CM_LANGS[langKey];
    if (cm) {
      if (!t.doc) t.doc = window.CodeMirror.Doc(res.content, langDef ? langDef.mode : null);
      cm.swapDoc(t.doc);
      setDirty(!!t.dirty);
      refreshCM();
      if (!minimized) requestAnimationFrame(() => { if (cm) cm.focus(); });
    }
    updateTitle();
    updateStatus();
    revealFile(entry.path);
  }

  // addTab 打开一个新标签（已经打开过的路径就切过去，不重复开）。
  async function addTab(e, r) {
    const exist = tabForPath(e.path);
    if (exist) { await activateTab(exist); return; }
    const t = mkTab(e, r);
    tabs.push(t);
    await activateTab(t);
  }

  async function openFileByTree(path, name) {
    const exist = tabForPath(path);
    if (exist) { await activateTab(exist); return; }
    let r;
    try { r = await api.fileRead(path); }
    catch (e) { toast('打开失败：' + ((e && e.message) || e), 'err', 10000); return; }
    if (r.binary) { toast('这是二进制文件，不能用文本编辑器打开', 'warn', 8000); return; }
    if (r.too_large) { toast('文件过大（' + humanSize(r.size) + '），超过在线编辑上限', 'warn', 10000); return; }
    await addTab({ path, name: name || basename(path) }, r);
  }

  // applyFile 把**当前标签**换成另一个文件内容（重新载入 / 另存为后的路径更新）。
  async function applyFile(newEntry, newRes) {
    const t = currentTab();
    entry = newEntry;
    res = newRes;
    const key = langKeyFor(entry.name);
    const langChanged = key !== langKey;
    langKey = key;
    langDef = CM_LANGS[key];
    if (langChanged) {
      try { await ensureLang(langKey); } catch (e) { toast('语言模式加载失败：' + ((e && e.message) || e), 'warn', 8000); }
    }
    curDir = dirname(entry.path);
    if (t) { t.entry = entry; t.res = res; t.langKey = langKey; t.langDef = langDef; }
    if (cm) {
      cm.setValue(res.content);
      cm.clearHistory();
      cm.setCursor(0, 0);
      if (langChanged) cm.setOption('mode', langDef ? langDef.mode : null);
      if (t) t.cleanGen = cm.changeGeneration();
      setDirty(false);
      renderTabs();
      refreshCM();
      requestAnimationFrame(() => { if (cm) cm.focus(); });
    }
    updateTitle();
    updateStatus();
    revealFile(entry.path);
  }

  // ===================== 保存 / 下载 =====================
  async function saveFile() {
    const t = currentTab();
    if (!cm || !t) return false;
    try {
      await api.fileWrite(t.entry.path, t.doc.getValue());
      if (t.id === activeTabId) { t.cleanGen = cm.changeGeneration(); setDirty(false); }
      else t.dirty = false;
      t.res = Object.assign({}, t.res, { size: t.doc.getValue().length });
      if (t.id === activeTabId) updateStatus();
      renderTabs();
      toast('已保存 ' + basename(t.entry.path), 'ok');
      refreshList();
      return true;
    } catch (e) {
      toast('保存失败：' + ((e && e.message) || e), 'err', 12000);
      return false;
    }
  }

  // saveAll 依次把**所有未保存**的标签写盘，最后汇总：成功几个、哪个失败、原因是什么。
  // 失败不中断（一个文件权限不对不该让其余文件也存不了），但必须逐个说清楚。
  async function saveAll() {
    if (!cm) return;
    const pending = tabs.filter((t) => t.dirty);
    if (!pending.length) { toast('没有未保存的修改', 'warn', 4000); return; }
    let ok = 0;
    const fails = [];
    for (const t of pending) {
      try {
        await api.fileWrite(t.entry.path, t.doc.getValue());
        if (t.id === activeTabId) t.cleanGen = cm.changeGeneration();
        t.dirty = false;
        t.res = Object.assign({}, t.res, { size: t.doc.getValue().length });
        ok++;
      } catch (e) {
        fails.push({ path: t.entry.path, msg: (e && e.message) || String(e) });
      }
    }
    if (tabs.length && currentTab()) setDirty(!!currentTab().dirty);
    syncUnloadGuard();
    renderTabs();
    updateStatus();
    refreshList();
    const skipped = tabs.length - pending.length;
    if (!fails.length) {
      toast(`全部保存完成：成功写入 ${ok} 个文件` + (skipped ? `（另有 ${skipped} 个没有改动）` : ''), 'ok', 6000);
      return;
    }
    modal({
      title: '全部保存结果',
      body: h('div', [
        h('p', { text: `成功 ${ok} 个，失败 ${fails.length} 个` + (skipped ? `（另有 ${skipped} 个没有改动）` : '') + '。' }),
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '文件' }), h('th', { text: '失败原因' })])]),
          h('tbody', fails.map((f) => h('tr', [
            h('td.mono', { style: { fontSize: '11.5px' }, text: f.path }),
            h('td', { style: { color: 'var(--danger)' }, text: f.msg }),
          ]))),
        ]),
      ]),
    });
  }

  async function saveAs() {
    if (!cm) return;
    const cur = currentTab();
    const dest = await promptBox({
      title: '另存为', label: '目标完整路径', value: entry.path,
      hint: '必须在文件管理允许的目录内（绝对路径）',
    });
    if (!dest || dest === entry.path) return;
    const content = cm.getValue();
    try {
      // 后端 /files/write 只写**已存在**的文件（不新建），所以"另存为新路径"要先 touch。
      // touch 报"已存在同名文件"时问一句是否覆盖 —— 覆盖不可逆。
      let exists = false;
      try {
        await api.fileTouch(dest);
      } catch (e) {
        exists = /已存在/.test((e && e.message) || '');
      }
      if (exists) {
        const yes = await confirmBox(`「${dest}」已存在，要用当前内容覆盖它吗？`, {
          title: '覆盖已存在的文件', danger: true, okText: '覆盖',
        });
        if (!yes) return;
      }
      await api.fileWrite(dest, content);
      entry = { path: dest, name: basename(dest) };
      res = Object.assign({}, res, { size: content.length });
      if (cur) {
        cur.entry = entry;
        cur.res = res;
        cur.cleanGen = cm.changeGeneration();
      }
      setDirty(false);
      renderTabs();
      updateTitle();
      updateStatus();
      toast('已另存为 ' + dest, 'ok');
      revealFile(entry.path);
      refreshList();
    } catch (e) {
      toast('另存为失败：' + ((e && e.message) || e), 'err', 12000);
    }
  }

  async function reloadFile() {
    if (!confirmDiscard()) return;
    try {
      const r = await api.fileRead(entry.path);
      if (r.binary || r.too_large) { toast('该文件已不能在线编辑（二进制或过大）', 'warn', 8000); return; }
      await applyFile(entry, r);
      toast('已重新载入', 'ok');
    } catch (e) { toast('重新载入失败：' + ((e && e.message) || e), 'err', 10000); }
  }

  function downloadFile() { window.location.href = api.fileDownloadURL(entry.path); }

  function copyText(t) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(t)
        .then(() => toast('已复制：' + t, 'ok', 5000))
        .catch(() => toast('复制失败，请手动选择：' + t, 'warn', 8000));
    } else {
      toast(t, 'ok', 8000);
    }
  }

  function toggleWrap() {
    if (!cm) return;
    cm.setOption('lineWrapping', !cm.getOption('lineWrapping'));
    refreshCM();
  }

  // ===================== 关闭 =====================
  function confirmDiscard() {
    if (!dirty) return true;
    return confirm('「' + entry.name + '」有未保存的修改，确定放弃这些修改？');
  }

  // closeTab 关闭单个标签：有未保存修改先确认；关掉最后一个标签 = 关闭整个编辑器。
  function closeTab(t) {
    const idx = tabs.indexOf(t);
    if (idx < 0 || disposed) return;
    if (t.dirty && !confirm('「' + t.entry.name + '」有未保存的修改，确定关闭这个标签？')) return;
    const wasActive = t.id === activeTabId;
    tabs.splice(idx, 1);
    if (!tabs.length) { dispose(); return; }
    if (wasActive) {
      activateTab(tabs[Math.min(idx, tabs.length - 1)]);
    } else {
      syncUnloadGuard();
      renderTabs();
    }
  }

  // requestClose 关闭整个编辑器（✕ 或菜单「文件 → 关闭编辑器」）。只要**任一**标签
  // 有未保存修改就先确认，并把是哪些文件说清楚。
  function requestClose() {
    const dirtyTabs = tabs.filter((t) => t.dirty);
    if (dirtyTabs.length) {
      const names = dirtyTabs.map((t) => t.entry.name).join('、');
      if (!confirm('有未保存的修改（' + names + '），确定关闭编辑器？')) return;
    }
    dispose();
  }

  // Esc 只关菜单；**不关编辑器**（用户要求 3）。查找/跳行框由 CodeMirror 的
  // dialog 插件自己处理（它收到 Esc 会 close 并 stopPropagation），所以这里只补菜单。
  function onDocKeyDown(e) {
    if (disposed || e.key !== 'Escape') return;
    if (openMenuIdx >= 0) closeMenus();
  }
  document.addEventListener('keydown', onDocKeyDown);

  function dispose() {
    if (disposed) return;
    disposed = true;
    removeUnloadGuard();
    document.removeEventListener('mousedown', onDocDown);
    document.removeEventListener('keydown', onDocKeyDown);
    document.removeEventListener('fullscreenchange', onFsChange);
    document.removeEventListener('webkitfullscreenchange', onFsChange);
    window.removeEventListener('resize', applyGeometry);
    window.removeEventListener('hashchange', onRouteChange);
    if (resizeObs) { try { resizeObs.disconnect(); } catch { /* 忽略 */ } }
    themeObserver.disconnect();
    layer.remove();
    onClosed();
  }

  // ===================== 主题 =====================
  async function applyTheme(v) {
    theme = v === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
    saveEditorTheme(theme);
    const seq = ++themeSeq;
    const name = cmThemeFor(theme);
    try {
      await ensureCmTheme(name);
    } catch (e) {
      toast('编辑器主题加载失败：' + ((e && e.message) || e), 'err');
      return;
    }
    if (seq !== themeSeq || disposed) return;
    if (cm) {
      cm.setOption('theme', name);
      requestAnimationFrame(() => { if (cm) cm.refresh(); });
    }
  }

  const themeObserver = new MutationObserver(() => {
    if (theme === ZPF_THEME_PANEL) applyTheme(ZPF_THEME_PANEL);
  });
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  // ===================== 拖动 / 缩放 =====================
  function startDrag(e) {
    if (e.button !== 0 || minimized) return;
    const t = e.target;
    if (t && t.closest && t.closest('button, a, select, input, textarea, .zpf-menu-pop')) return;
    closeMenus();
    if (maximized) setMaximized(false); // 拖最大化窗口 = 先还原再拖（与系统窗口一致）
    e.preventDefault();
    const sx = e.clientX;
    const sy = e.clientY;
    const ol = win.offsetLeft; // offsetParent 就是 .zpf-layer
    const ot = win.offsetTop;
    const w = win.offsetWidth;
    const h = win.offsetHeight;
    const move = (ev) => {
      // 拖动只在可用区里平移（尺寸不变）；越界由这里挡住，落盘前还会再夹一次（坑 220）。
      const view = layerViewRect();
      const left = clamp(ol + (ev.clientX - sx), 0, Math.max(0, view.width - w));
      const top = clamp(ot + (ev.clientY - sy), 0, Math.max(0, view.height - h));
      win.style.left = Math.round(left) + 'px';
      win.style.top = Math.round(top) + 'px';
      geom = { left: Math.round(left), top: Math.round(top), width: w, height: h };
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      document.body.classList.remove('zpf-dragging');
      persistGeom();
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
    document.body.classList.add('zpf-dragging');
  }

  titlebar.addEventListener('mousedown', startDrag);
  menubar.addEventListener('mousedown', startDrag);

  // 最小化后整条标题栏就是"还原"热区（宝塔手感）；按钮自己已处理，别重复触发。
  titlebar.addEventListener('click', (e) => {
    if (!minimized) return;
    if (e.target && e.target.closest && e.target.closest('button')) return;
    setMinimizedState(false);
  });

  treeResizer.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const sx = e.clientX;
    const w0 = treeWidth;
    const move = (ev) => {
      treeWidth = clamp(w0 + (ev.clientX - sx), 150, 460);
      treeEl.style.width = treeWidth + 'px';
      if (cm) cm.refresh();
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      document.body.classList.remove('zpf-resizing');
      writeLS(ZPF_TREE_W_KEY, treeWidth);
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
    document.body.classList.add('zpf-resizing');
  });
  treeResizer.addEventListener('dblclick', () => {
    treeWidth = 230;
    treeEl.style.width = '230px';
    writeLS(ZPF_TREE_W_KEY, 230);
    if (cm) cm.refresh();
  });

  minBtn.addEventListener('click', (e) => { e.stopPropagation(); setMinimizedState(!minimized); });
  maxBtn.addEventListener('click', (e) => { e.stopPropagation(); setMaximized(!maximized); });
  closeBtn.addEventListener('click', (e) => { e.stopPropagation(); requestClose(); });

  function onDocDown(e) { if (!win.contains(e.target)) closeMenus(); }
  document.addEventListener('mousedown', onDocDown);

  // ===================== 浏览器全屏（既有能力，收进「视图」菜单） =====================
  function fsElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }

  function toggleFullscreen() {
    if (fsElement()) {
      const exit = document.exitFullscreen || document.webkitExitFullscreen;
      if (exit) { const p = exit.call(document); if (p && p.catch) p.catch(() => {}); }
      return;
    }
    const req = win.requestFullscreen || win.webkitRequestFullscreen;
    if (!req) { toast('当前浏览器不支持屏幕全屏', 'warn'); return; }
    const p = req.call(win);
    if (p && p.catch) p.catch((e) => toast('进入屏幕全屏失败：' + ((e && e.message) || e), 'err'));
  }

  function onFsChange() { refreshCM(); }
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('webkitfullscreenchange', onFsChange);

  // ===================== 自适应 =====================
  // 侧栏折叠 / 展开只改变 content 的宽度，**不触发 window.resize**，所以必须观察 content 本身。
  //
  // 编辑器是全局存活的，而每次路由切换都会重建 `.content` 元素 —— 旧观察目标会脱离
  // 文档树（resize 再也不触发）。ensureContentObserver 负责在目标失效时改观察新的那个，
  // applyGeometry 每次都会调它，所以切换板块后点胶囊还原 / 最大化都能拿到正确矩形。
  let resizeObs = null;
  let contentObsTarget = null;
  function ensureContentObserver() {
    if (!window.ResizeObserver) return;
    const c = document.querySelector('.content');
    if (!c || (contentObsTarget === c && c.isConnected)) return;
    if (!resizeObs) resizeObs = new ResizeObserver(() => applyGeometry());
    else { try { resizeObs.disconnect(); } catch { /* 忽略 */ } }
    contentObsTarget = c;
    resizeObs.observe(c);
  }
  ensureContentObserver();
  window.addEventListener('resize', applyGeometry);
  function onRouteChange() { applyGeometry(); }
  window.addEventListener('hashchange', onRouteChange);

  // ===================== 初始化 =====================
  updateTitle();
  syncChrome();
  setDirty(false);
  renderTabs();
  buildMenus();
  applyGeometry();

  (async () => {
    try { await treeLoad(treeRoot); } catch { /* 已在 treeLoad 里提示 */ }
    await revealFile(entry.path);
  })();

  (async () => {
    try {
      await ensureCodeMirror();
      await ensureLang(langKey);
      await ensureCmTheme(cmThemeFor(theme));
      const themeName = cmThemeFor(theme);
      cm = window.CodeMirror(host, {
        value: '',
        mode: null,
        theme: themeName,
        lineNumbers: true,
        lineWrapping: false,
        indentUnit: 4,
        tabSize: 4,
        indentWithTabs: false,
        smartIndent: true,
        electricChars: true,
        autoCloseBrackets: true,
        matchBrackets: true,
        styleActiveLine: true,
        foldGutter: true,
        gutters: ['CodeMirror-linenumbers', 'CodeMirror-foldgutter'],
        scrollbarStyle: 'simple',
        viewportMargin: 30,
        extraKeys: {
          'Ctrl-S': () => { saveFile(); },
          'Cmd-S': () => { saveFile(); },
          'Ctrl-Alt-S': () => { saveAll(); },
          'Cmd-Alt-S': () => { saveAll(); },
          'Shift-Ctrl-S': () => { saveAs(); },
          'Shift-Cmd-S': () => { saveAs(); },
          'Ctrl-F': 'findPersistent',
          'Cmd-F': 'findPersistent',
          'Shift-Ctrl-F': 'replace',
          'Shift-Cmd-F': 'replace',
          'Alt-G': 'jumpToLine',
          'Ctrl-/': 'toggleComment',
          'Cmd-/': 'toggleComment',
          // 这里**故意没有 Esc**（用户要求 3）：按 Esc 不许关编辑器。Esc 只用于关菜单
          // （见 onDocKeyDown）和关 CodeMirror 的查找/跳行框（dialog 插件自带，且它会
          // stopPropagation）。关编辑器只能点 ✕ 或菜单「文件 → 关闭编辑器」。
          // Tab / Shift+Tab 必须显式接管：CodeMirror 默认的 Tab 在空行上会插入
          // **制表符**，而面板其它地方（以及面板自己生成的配置）统一用 4 空格 ——
          // 混用会在保存后的文件里留下看不见的差异。有选区时整块缩进/反缩进。
          'Tab': (ed) => {
            if (ed.somethingSelected()) ed.indentSelection('add');
            else ed.replaceSelection('    ', 'end');
          },
          'Shift-Tab': (ed) => ed.indentSelection('subtract'),
        },
        // 查找/替换对话框的文案（CodeMirror 自带的是英文；面板是中文界面）
        // 键名必须与 CodeMirror 插件的 phrase() 调用逐字一致（含冒号），
        // 否则查表失败会**静默**退回英文 —— 第一版就踩了（写的是 'Search'）。
        phrases: {
          'Search:': '查找：',
          'Replace:': '替换：',
          'Replace with:': '替换为：',
          'Replace all:': '全部替换：',
          'Replace?': '要替换吗？',
          'With:': '替换为：',
          'Jump to line:': '跳到行：',
          'All': '全部',
          'Stop': '停止',
          'Yes': '是',
          'No': '否',
          '(Use /re/ syntax for regexp search)': '（支持 /正则/ 语法）',
          '(Use line:column or scroll% syntax)': '（可用 行:列 或 百分比）',
        },
      });
      // 初始标签的 Doc（每个标签一份 Doc => 各自独立的撤销历史/光标/滚动）
      const t0 = currentTab();
      if (t0 && !t0.doc) t0.doc = window.CodeMirror.Doc(res.content, langDef ? langDef.mode : null);
      if (t0) cm.swapDoc(t0.doc);
      cm.setCursor(0, 0);
      cm.clearHistory();
      if (t0) t0.cleanGen = cm.changeGeneration();
      // 脏标记用"与上次干净时的变更代数比较"，而不是收到 change 就置脏：
      // 程序化的 setValue / swapDoc / clearHistory 也会触发 change，靠事件置脏会假报未保存。
      cm.on('change', () => {
        const t = currentTab();
        if (!t) return;
        setDirty(!cm.isClean(t.cleanGen));
      });
      cm.on('cursorActivity', updateStatus);
      setDirty(false);
      updateStatus();
      renderTabs();
      cm.focus();
      buildMenus(); // 编辑器就绪后菜单项从禁用变可用
      refreshCM();
      // 挂载是异步的：如果这期间用户切了配色（或面板深浅色变了），按最新选择纠正一次
      if (cmThemeFor(theme) !== themeName) applyTheme(theme);
    } catch (e) {
      // 加载失败要给出**原因**（网络/镜像/文件缺失），并且仍然给一条能走通的路
      clear(host);
      appendAll(host, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '编辑器加载失败' }),
        h('p', { text: (e && e.message) || String(e) }),
        h('p', { text: '可以刷新页面重试；也可以用 Web 终端或下载后本地编辑。文件内容没有被改动。' }),
      ]));
      statusInfo.textContent = '';
    }
  })();

  return {
    // openFile 打开文件：已打开则切到那个标签，否则新开一个标签。
    openFile: (e, r) => addTab(e, r),
    // setFile 保留旧语义：把**当前标签**换成另一个文件（内部重新载入等场景）。
    setFile: (e, r) => applyFile(e, r),
    // setOptions 让重新创建的 FilesView 把回调重新绑到当前 DOM 上（编辑器跨路由存活）。
    setOptions: (o = {}) => {
      if (Array.isArray(o.roots)) roots = o.roots;
      if (typeof o.refreshList === 'function') refreshList = o.refreshList;
      if (typeof o.onDir === 'function') onDir = o.onDir;
    },
    focus: () => { if (cm) cm.focus(); },
    dispose,
  };
}

// ---------- 拖拽：把 DataTransfer 展开成 {file, rel} ----------
//
// 只有 webkitGetAsEntry 能拿到目录与相对路径；Entry.file() / readEntries() 都是
// 回调式 API，这里包成 Promise 好按顺序走完。
// readEntries 一次**最多只返回一批**（Chrome 约 100 个），必须反复调用到返回空数组
// —— 只调一次会静默丢掉后面的文件（"文件夹传了一半"就是这么来的）。

function readAllEntries(reader) {
  return new Promise((resolve, reject) => {
    const all = [];
    const next = () => {
      reader.readEntries((batch) => {
        if (!batch.length) { resolve(all); return; }
        all.push(...batch);
        next();
      }, reject);
    };
    next();
  });
}

function entryFile(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject));
}

async function walkEntry(entry, prefix, out) {
  if (!entry) return;
  if (entry.isFile) {
    const file = await entryFile(entry);
    out.push({ file, rel: prefix + entry.name });
    return;
  }
  if (entry.isDirectory) {
    const kids = await readAllEntries(entry.createReader());
    for (const k of kids) await walkEntry(k, prefix + entry.name + '/', out);
  }
}

// ---------- 小工具 ----------
function basename(p) { return String(p).split('/').filter(Boolean).pop() || '/'; }function dirname(p) { const parts = String(p).split('/'); parts.pop(); return parts.join('/') || '/'; }

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2) + ' ' + units[i];
}

function fileIcon(name) {
  const ext = String(name).split('.').pop().toLowerCase();
  const map = {
    php: '🐘', js: '📜', ts: '📘', json: '🧾', css: '🎨', html: '🌐', htm: '🌐',
    md: '📝', txt: '📄', log: '📋', sql: '🗃️', sh: '⚙️', yml: '⚙️', yaml: '⚙️',
    png: '🖼️', jpg: '🖼️', jpeg: '🖼️', gif: '🖼️', svg: '🖼️', webp: '🖼️',
    zip: '📦', gz: '📦', tar: '📦', pdf: '📕', mp4: '🎬', mp3: '🎵',
  };
  return map[ext] || '📄';
}

function readCookie(name) {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}

void esc;
