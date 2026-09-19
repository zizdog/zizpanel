// tasks.js —— 「任务中心」：安装 / 卸载这类**几分钟到十几分钟**的操作的实时进度。
//
// 为什么是这个形状（契约见 SPEC-任务中心.md）：
//   - 状态与 SSE 都是**模块级单例**。app.js 的 renderApp() 每次切路由都会重建整个
//     外壳并执行 runCleanup() 清掉"页面级"资源；如果把这些注册进页面 cleanup，
//     用户切一次页面安装进度就断了 —— 而任务本身在后台还在跑，界面却再也找不回来。
//   - 关窗 ≠ 取消。关掉进度窗只是不再渲染；后台的 EventSource 继续把状态收敛进
//     st.metas，顶栏徽标因此始终能如实显示"还有几个任务在跑"。
//   - 断线交给 EventSource 自己重连。它会带上 Last-Event-ID，服务端据此续发；
//     我们只在 onerror 里如实显示"连接中断，重连中…"，**绝不自己写重连循环** ——
//     自己重连会创建新的连接，与服务端的历史补发叠加成重复日志。
//   - 所有日志行按 seq 去重（见 addLines）：EventSource 重连、REST 兜底、SSE 补历史
//     三条路径都会重复送来同一批行，没有 seq 去重就会满屏重复。

import { api } from './api.js';
import { h, clear, appendAll, toast, modal, confirmBox, duration } from './ui.js';

// 客户端缓存的行数上限。服务端每任务最多 4000 行，这里留点余量；
// 真正的"历史"在服务端，客户端这份只用于渲染。
const MAX_LINES = 5000;

// 进度窗里保留的 DOM 行数上限：4000 个 <div> 会让滚动和选中变卡，
// 而用户几乎不会往回翻几百行。超出就丢最老的节点（数据仍在 st.lines 里）。
const MAX_DOM_LINES = 2500;

const STATE = {
  metas: new Map(),      // id -> TaskMeta
  lines: new Map(),      // id -> Line[]
  // id -> InputRequest：任务**此刻**在等用户输入（见 SPEC「任务输入」）。
  //
  // 为什么不直接读 metas[id].input_required：列表轮询（GET /tasks）返回的是
  // 请求发起那一刻的快照，可能比 SSE 的"开始等待"事件更旧；直接覆盖会把刚弹出来的
  // 输入框抹掉。这里只接受"权威来源"（SSE 的 input_required 事件、status/meta
  // 快照、REST 详情）的写入，并且只在**落定**（input_result 非空）时删除。
  inputs: new Map(),
  lastSeq: new Map(),    // id -> 已收到的最大 seq（去重用）
  oldest: new Map(),     // id -> 服务端缓冲里最老的一行 seq（>1 = 更早的行已滚出）
  fallback: new Set(),   // 已经用 REST 兜底拉过历史的任务（每个任务只兜一次）
  streams: new Map(),    // id -> { es, state }  state: live | reconnecting
  listeners: new Set(),  // (kind, payload) => void；kind ∈ meta|lines|stream
  notified: new Set(),   // 已弹过完成提示的任务 id（避免重复 toast）
  inited: false,
  listErr: '',           // GET /api/v1/tasks 的失败原因（后端还没上线时为非空）
};

// 顶栏按钮只有一个（每次 renderApp 都会重建外壳，旧的随外壳一起被丢弃）。
// 这里只记住"当前那一个"，状态变化时原地更新它，而不是给每个按钮都挂订阅。
let badgeEl = null;

const STATUS_LABEL = { running: '运行中', succeeded: '成功', failed: '失败', canceled: '已中断' };
const STATUS_PILL = { running: 'brand', succeeded: 'ok', failed: 'danger', canceled: 'warn' };

// 日志级别配色。step 加粗、out 常规、err 红、ok 绿、warn 黄、cmd 灰。
// input = 后端给"任务在等输入"配的那条提示文案（结构信息走 input_required 事件，
// 绝不解析这行文字）。
const LEVEL_STYLE = {
  step: { fontWeight: '650', color: '#e6ebf5' },
  input: { fontWeight: '650', color: '#fbbf24' },
  out: { color: '#c8d0e0' },
  err: { color: '#f87171' },
  ok: { color: '#4ade80' },
  warn: { color: '#fbbf24' },
  cmd: { color: '#7f8a9d' },
};

const isRunning = (m) => !!m && (m.status || 'running') === 'running';

function statusLabel(s) { return STATUS_LABEL[s] || s || '未知'; }

function statusPillCls(s) { return 'pill' + (STATUS_PILL[s] ? ' ' + STATUS_PILL[s] : ''); }

/** 实时耗时（秒）。运行中的任务按 started_at 现算，结束的用 elapsed_ms。 */
function elapsedSec(m) {
  if (!m) return 0;
  if (isRunning(m)) {
    const t0 = Date.parse(m.started_at || '');
    if (!Number.isNaN(t0)) return Math.max(0, (Date.now() - t0) / 1000);
  }
  if (m.elapsed_ms) return Number(m.elapsed_ms) / 1000;
  const t0 = Date.parse(m.started_at || '');
  const t1 = Date.parse(m.finished_at || '');
  if (!Number.isNaN(t0) && !Number.isNaN(t1) && t1 > t0) return (t1 - t0) / 1000;
  return 0;
}

function runningCount() {
  let n = 0;
  for (const m of STATE.metas.values()) if (isRunning(m)) n++;
  return n;
}

// ---------------- 订阅 / 通知 ----------------

function emit(kind, payload) {
  for (const fn of Array.from(STATE.listeners)) {
    try { fn(kind, payload); } catch (e) { console.warn('task listener failed', e); }
  }
}

/**
 * onChange(fn) 订阅**任务元信息**变化（不含日志行），返回取消订阅函数。
 * 应用市场的卡片按钮用它把「安装」换成「查看进度」，只在状态变化时重画。
 */
function onChange(fn) {
  if (typeof fn !== 'function') return () => {};
  STATE.listeners.add(fn);
  return () => STATE.listeners.delete(fn);
}

// 顶栏徽标是模块级的常驻订阅：它必须在任何路由下都活着。
onChange((kind) => { if (kind !== 'lines') paintBadge(); });

function paintBadge() {
  if (!badgeEl) return;
  const n = runningCount();
  if (n > 0) {
    badgeEl.textContent = '⟳ ' + n;
    badgeEl.title = `${n} 个任务正在运行，点击查看进度`;
    badgeEl.style.color = 'var(--brand)';
  } else {
    // 没有任务时**也要有入口**：用户要能随时重开看历史，
    // 而不是"徽标消失了就再也找不到任务中心"。
    badgeEl.textContent = '🗂';
    badgeEl.title = '任务中心';
    badgeEl.style.color = '';
  }
}

// ---------------- 数据更新 ----------------

function addLines(id, lines) {
  if (!Array.isArray(lines) || !lines.length) return;
  let arr = STATE.lines.get(id);
  if (!arr) { arr = []; STATE.lines.set(id, arr); }
  let last = STATE.lastSeq.get(id) || 0;
  let added = false;
  for (const ln of lines) {
    if (!ln) continue;
    const seq = Number(ln.seq) || 0;
    if (seq && seq <= last) continue; // 重复帧（重连补历史 / REST 兜底 / SSE 与 REST 重叠）
    if (seq) last = seq;
    arr.push(ln);
    added = true;
  }
  if (last) STATE.lastSeq.set(id, last);
  if (arr.length > MAX_LINES) arr.splice(0, arr.length - MAX_LINES);
  if (added) emit('lines', { id });
}

/**
 * syncInput 把一份任务快照里的输入状态并进 STATE.inputs。
 *
 * 规则（与 SPEC 的契约一一对应）：
 *   - `input_required` 非空 ⇒ 任务此刻在等这个 key；
 *   - `input_required` 缺席且 `input_result` 非空 ⇒ 这次等待已落定
 *     （submitted / timeout / canceled），收框；
 *   - 两个都没有 ⇒ **不动**（可能只是快照比"开始等待"更旧）。
 */
function syncInput(m) {
  if (!m || !m.id) return;
  if (m.input_required) STATE.inputs.set(m.id, m.input_required);
  else if (m.input_result) STATE.inputs.delete(m.id);
}

/**
 * mergeInputState 处理 SSE 的 input_required 事件。
 *
 * 只改"输入相关"的两个字段，其余（result/status/line_count…）保持原样 ——
 * 状态事件与输入事件是两条独立通道，顺序不保证，整体覆盖会互相踩掉。
 */
function mergeInputState(id, payload) {
  if (!id || !payload) return;
  const cur = STATE.metas.get(id);
  if (!cur) {
    // 极少数情况下"开始等待"事件先于 meta/status 到达：只记输入状态，
    // 不凭一条输入事件造出半个任务；meta 到了自然会渲染出来。
    if (payload.input_required) STATE.inputs.set(id, payload.input_required);
    return;
  }
  const next = Object.assign({}, cur);
  if (payload.input_required) next.input_required = payload.input_required;
  else delete next.input_required;
  if (payload.input_result) next.input_result = payload.input_result;
  STATE.metas.set(id, next);
  syncInput(next);
  emit('meta', { id });
}

/** upsertMeta 只处理"实时"来源（SSE status / POST 返回值），失败会弹一次 toast。 */
function upsertMeta(m) {
  if (!m || !m.id) return;
  STATE.metas.set(m.id, m);
  syncInput(m);
  if (!isRunning(m)) {
    // 结束的任务没有后续数据了：缓一下收完尾巴日志再关连接，
    // 否则 EventSource 会对着一个已经结束的流无限重连。
    scheduleCloseStream(m.id);
    notifyFinished(m);
  }
  emit('meta', { id: m.id });
}

/** notifyFinished 弹一次完成提示。只在实时路径调用 —— 刷新列表时不该刷屏。 */
function notifyFinished(m) {
  if (!m || STATE.notified.has(m.id)) return;
  STATE.notified.add(m.id);
  const title = m.title || '任务';
  if (m.status === 'succeeded') toast(`✅ ${title} 已完成`, 'ok', 6000);
  else if (m.status === 'canceled') toast(`⚠️ ${title} 已中断`, 'warn', 8000);
  else if (m.status === 'failed') toast(`⛔ ${title} 失败：${m.error || '详情见任务日志'}`, 'err', 12000);
}

function applyList(list) {
  const seen = new Set();
  for (const m of list) {
    if (!m || !m.id) continue;
    seen.add(m.id);
    const prev = STATE.metas.get(m.id);
    STATE.metas.set(m.id, m);
    syncInput(m);
    // 列表也可能第一次告诉我们"某个任务已经结束了"（例如任务跑得比一次刷新还快），
    // 这时同样要给完成提示，否则用户永远等不到结果。
    if (prev && isRunning(prev) && !isRunning(m)) { notifyFinished(m); scheduleCloseStream(m.id); }
  }
  const now = Date.now();
  for (const id of Array.from(STATE.metas.keys())) {
    if (seen.has(id)) continue;
    // 服务端列表是权威的（进程重启后残留的 running 就该消失），
    // 唯一例外是刚 POST 出去、列表还没刷到的竞态窗口 —— 用 _localAt 标记兜 60 秒。
    const local = STATE.metas.get(id);
    if (local && local._localAt && now - local._localAt < 60000) continue;
    STATE.metas.delete(id);
    STATE.lines.delete(id);
    STATE.lastSeq.delete(id);
    STATE.oldest.delete(id);
    STATE.fallback.delete(id);
    STATE.inputs.delete(id);
    closeStream(id);
  }
  // 页面加载 / 切回时，把仍在跑的任务的流接上（徽标与进度靠它保持实时）。
  for (const m of STATE.metas.values()) if (isRunning(m)) ensureStream(m.id);
}

/** refresh 拉一次任务列表。失败**不抛异常**：后端没上线时页面必须照常能用。 */
async function refresh() {
  try {
    const data = await api.tasks();
    STATE.listErr = '';
    applyList((data && data.tasks) || []);
  } catch (e) {
    STATE.listErr = (e && e.message) || '暂时取不到任务列表';
  }
  emit('meta', {});
}

// ---------------- SSE ----------------

function closeStream(id) {
  const rec = STATE.streams.get(id);
  if (!rec) return;
  STATE.streams.delete(id);
  try { rec.es.close(); } catch { /* 已经关了 */ }
  emit('stream', { id });
}

/**
 * scheduleCloseStream 在任务结束后**缓一下**再关流。
 *
 * 服务端在处理 status/done 时要先把订阅通道里剩下的行 drain 完，紧接着关闭
 * 连接；如果我们收到"结束"就立刻 close，最后几行日志就永远看不到了。
 * 1.5 秒足够 drain（缓冲 512 行，是纯内存操作）。
 */
function scheduleCloseStream(id) {
  setTimeout(() => closeStream(id), 1500);
}

/**
 * ensureStream 为**运行中**的任务建立 SSE 连接（每个任务只有一条）。
 *
 * 已结束的任务不开流：它没有增量，只会让 EventSource 对着一个关闭的连接反复重连。
 * 那种情况下用 fetchHistory() 分页读历史。
 */
function ensureStream(id) {
  const rec = STATE.streams.get(id);
  if (rec) return rec;
  const meta = STATE.metas.get(id);
  if (meta && !isRunning(meta)) return null;

  const es = new EventSource(api.taskStreamURL(id), { withCredentials: true });
  const r = { es, state: 'live' };
  STATE.streams.set(id, r);

  es.addEventListener('lines', (e) => {
    let payload = null;
    try { payload = JSON.parse(e.data); } catch { return; }
    addLines(id, payload && payload.lines);
  });
  // 后端在流的开头额外发一条 meta（{task, oldest_seq}）。SPEC 只要求 lines/status，
  // 但这条能让进度窗在拿到第一行日志之前就有标题/状态，也顺手告诉我们
  // "更早的若干行已经滚出服务端缓冲"（oldest_seq），不至于假装日志是完整的。
  es.addEventListener('meta', (e) => {
    let payload = null;
    try { payload = JSON.parse(e.data); } catch { return; }
    if (payload && payload.task) upsertMeta(payload.task);
    noteOldest(id, payload && payload.oldest_seq);
  });
  es.addEventListener('status', (e) => {
    let meta2 = null;
    try { meta2 = JSON.parse(e.data); } catch { return; }
    upsertMeta(meta2);
  });
  // 限时输入的开始 / 落定。**结构以这条事件为准**，绝不去解析 level:"input" 的日志行
  // （那行只是给用户看的提示文案，改一次字就崩）。
  //   data: {"input_required": {…}, "input_result": ""}        开始等待
  //   data: {"input_required": null, "input_result": "timeout"} 落定 → 收框
  es.addEventListener('input_required', (e) => {
    let payload = null;
    try { payload = JSON.parse(e.data); } catch { return; }
    mergeInputState(id, payload);
  });
  es.addEventListener('open', () => { r.state = 'live'; emit('stream', { id }); });
  es.onerror = () => {
    // EventSource 会自动重连并带上 Last-Event-ID（服务端据此续发），
    // 所以这里**只改状态显示**，绝不 close、也不自己写重连循环。
    r.state = 'reconnecting';
    emit('stream', { id });
    // 兜底：连 SSE 都被中间层掐掉的环境里，至少让进度窗有历史可看。
    // 每个任务只兜一次 —— 否则一条永远连不上的流会每 3 秒重连一次、
    // 每次都把整份历史再拉一遍（8 页），纯属白烧流量。
    if (!STATE.fallback.has(id)) {
      STATE.fallback.add(id);
      fetchHistory(id);
    }
  };
  return r;
}

/** noteOldest 记下服务端缓冲里最老的一行 seq（>1 说明更早的行已被丢弃）。 */
function noteOldest(id, oldest) {
  const n = Number(oldest) || 0;
  if (n > 1 && STATE.oldest.get(id) !== n) {
    STATE.oldest.set(id, n);
    emit('meta', { id });
  }
}

/**
 * fetchHistory 走 REST 拉历史（`GET /api/v1/tasks/{id}`）。失败静默 —— 它只是兜底：
 * SSE 被中间层掐掉、或任务已经结束（不再开流）时用它把日志补出来。
 *
 * 分页拉：服务端单次最多给 limit 行，has_more 为真时继续。上限 8 页
 * （800×8=6400）覆盖服务端"每任务最多 4000 行"的缓冲，不会无限循环。
 */
async function fetchHistory(id) {
  let after = 0;
  for (let page = 0; page < 8; page++) {
    let d = null;
    try {
      d = await api.task(id, after, 800);
    } catch {
      return; // 后端还没上线 / 任务已被挤出列表：保持现状，不打断界面
    }
    if (!d) return;
    if (Array.isArray(d.lines)) addLines(id, d.lines);
    if (d.task) {
      const prev = STATE.metas.get(id) || {};
      const merged = Object.assign({}, prev, d.task);
      // 详情是**权威快照**：`input_required` 缺席就是"此刻没在等"，必须把 prev 里的
      // 旧值删掉（Object.assign 只覆盖不删除）。是否清 STATE.inputs 由 input_result
      // 决定：没有 input_result 的旧快照不会把已经弹出的输入框抹掉。
      if (!d.task.input_required) delete merged.input_required;
      syncInput(d.task);
      STATE.metas.set(id, merged);
      if (prev && isRunning(prev) && !isRunning(d.task)) notifyFinished(d.task);
      emit('meta', { id });
    }
    noteOldest(id, d.oldest_seq);
    if (!d.has_more) return;
    const next = Number(d.next_after) || 0;
    if (next <= after) return; // 服务端没推进游标，避免死循环
    after = next;
  }
}

// ---------------- 顶栏入口 ----------------

function button() {
  // 固定 id：按钮的 title 会随状态变化（"任务中心" / "N 个任务正在运行…"），
  // 而 UI 测试需要一个**稳定**的选择器（同理见 app.js 的 #sidebar-account）。
  const el = h('button.btn.btn-ghost.btn-icon', {
    id: 'zp-task-btn',
    title: '任务中心',
    onclick: () => openList(),
  });
  badgeEl = el;
  paintBadge();
  return el;
}

// ---------------- 任务列表 ----------------

function sortedMetas() {
  const arr = Array.from(STATE.metas.values());
  arr.sort((a, b) => {
    const ra = isRunning(a) ? 0 : 1;
    const rb = isRunning(b) ? 0 : 1;
    if (ra !== rb) return ra - rb; // 运行中在前
    const ta = Date.parse(a.started_at || '') || 0;
    const tb = Date.parse(b.started_at || '') || 0;
    return tb - ta; // 新的在前
  });
  return arr;
}

let listWindow = null;

function openList() {
  if (listWindow) return; // 已经开着，不叠第二个

  const body = h('div');
  const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', width: '100%' } });
  let signature = '';
  let poll = null;

  const m = modal({
    title: '任务中心',
    wide: true,
    body,
    footer: () => [foot],
    onClose: () => {
      if (poll) clearInterval(poll);
      unsub();
      listWindow = null;
    },
  });

  const row = (t) => h('div.tc-row', {
    style: {
      padding: '10px 12px', borderBottom: '1px solid var(--border-soft)', cursor: 'pointer',
      display: 'flex', gap: '10px', alignItems: 'flex-start',
    },
    onclick: () => { m.close(); openTask(t.id); },
    onmouseenter: (e) => { e.currentTarget.style.background = 'var(--panel-2)'; },
    onmouseleave: (e) => { e.currentTarget.style.background = ''; },
  }, [
    h('div', { style: { flex: '1', minWidth: 0 } }, [
      h('div', { style: { fontWeight: '550', fontSize: '13px' }, text: t.title || t.id }),
      h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '2px' },
        text: `${statusLabel(t.status)} · 耗时 ${duration(elapsedSec(t))} · ${t.line_count || 0} 行`,
      }),
      t.last ? h('div', {
        style: {
          fontFamily: 'var(--mono)', fontSize: '11.5px', color: 'var(--text-dim)', marginTop: '3px',
          whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis',
        },
        text: t.last,
      }) : null,
    ]),
    h('span', { class: statusPillCls(t.status), text: statusLabel(t.status) }),
    // 在等输入的任务要一眼能认出来：徽标只说"有几个在跑"，用户关掉进度窗后
    // 若不知道有人在等他，限时输入就会静默超时。
    STATE.inputs.has(t.id) ? h('span.pill.warn', { text: '⏳ 等待输入' }) : null,
  ]);

  function render() {
    clear(body); clear(foot);
    if (STATE.listErr && STATE.metas.size === 0) {
      appendAll(body, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '暂时取不到任务列表' }),
        h('p', { text: STATE.listErr }),
      ]));
    } else {
      const list = sortedMetas();
      if (!list.length) {
        appendAll(body, h('div.empty', [
          h('div.big', { text: '🗂' }),
          h('h4', { text: '还没有任务记录' }),
          h('p', { text: '安装、卸载这类耗时操作会自动出现在这里。' }),
        ]));
      } else {
        appendAll(body, h('div', list.map(row)));
        if (STATE.listErr) {
          appendAll(body, h('div.hint', {
            style: { marginTop: '10px', color: 'var(--warn)' },
            text: '列表刷新失败：' + STATE.listErr,
          }));
        }
      }
    }
    appendAll(foot,
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => { refresh(); } }),
      h('button.btn.btn-sm', { text: '关闭', onclick: () => m.close() }),
    );
  }

  // 只在内容真的变了时重画，避免轮询把正在选中的文字/滚动位置冲掉。
  const update = () => {
    const sig = STATE.listErr + '#' + sortedMetas()
      .map((t) => [t.id, t.status, t.line_count, t.last, t.elapsed_ms,
        STATE.inputs.has(t.id) ? 'in' : ''].join('|')).join(';');
    if (sig === signature) return;
    signature = sig;
    render();
  };

  const unsub = onChange((kind) => { if (kind !== 'lines') update(); });
  render();
  // 打开就**立刻**拉一次：只靠 3 秒轮询的话，用户点开"任务中心"可能先看到空列表，
  // 而他要找的任务明明已经在跑（UI 测试就是这么抓到这个问题的）。
  refresh();
  // 运行中的耗时/行数每隔几秒自然地往前走；SSE 只在"本页已知的任务"上活着，
  // 所以列表还要一条慢轮询来发现别的标签页/别的页面启动的任务。
  poll = setInterval(() => { refresh(); }, 3000);
  listWindow = m;
}

// ---------------- 进度窗 ----------------

const openWindows = new Map(); // id -> modal 返回值，防止同一个任务开两个窗

function openTask(id) {
  if (!id) return null;
  const opened = openWindows.get(id);
  if (opened) return opened;

  const initial = STATE.metas.get(id) || { id, title: '任务详情', status: 'running' };
  if (STATE.metas.has(id) && !isRunning(initial)) fetchHistory(id); // 已结束：一次性读历史
  else ensureStream(id);

  // ---- 状态行：状态徽标 + 实时耗时 + 行数 ----
  const pill = h('span.tc-status', { class: statusPillCls(initial.status), text: statusLabel(initial.status) });
  const metaLine = h('span', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: '' });
  const connHint = h('span', {
    style: { display: 'none', fontSize: '11.5px', color: 'var(--warn)' },
    text: '连接中断，重连中…',
  });
  const head = h('div', {
    style: { display: 'flex', alignItems: 'center', gap: '10px', flexWrap: 'wrap', marginBottom: '10px' },
  }, [pill, metaLine, connHint]);

  const errBox = h('div', {
    style: {
      display: 'none', marginBottom: '10px', padding: '9px 11px', background: 'var(--danger-soft)',
      borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.7', color: 'var(--danger)',
    },
  });

  const resultBox = h('div.tc-result');

  // ---- 限时输入（如 MySQL root 口令）的挂载点 ----
  const inputBox = h('div.tc-input');

  // 服务端真的有行被环形缓冲丢掉时，如实说一句 —— 假装日志是完整的最误导人。
  const trimHint = h('div.hint', { style: { display: 'none', marginBottom: '8px', color: 'var(--warn)' } });

  // ---- 日志区 ----
  const logEl = h('div.tc-log', {
    style: {
      background: '#0b0d12', border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
      fontFamily: 'var(--mono)', fontSize: '12px', lineHeight: '1.6', padding: '10px 12px',
      height: '340px', overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-word', color: '#c8d0e0',
    },
  });
  let follow = true;
  const toBottom = h('button.btn.btn-sm.tc-tobottom', {
    text: '↓ 回到底部',
    style: { display: 'none', position: 'absolute', right: '14px', bottom: '12px', boxShadow: 'var(--shadow)' },
    onclick: () => { follow = true; logEl.scrollTop = logEl.scrollHeight; toBottom.style.display = 'none'; },
  });
  const logWrap = h('div', { style: { position: 'relative', marginBottom: '10px' } }, [logEl, toBottom]);

  // 用户手动往上滚 → 暂停自动滚动并给出「回到底部」；
  // 否则每来一行就把视图拽到底，用户根本没法回头看。
  logEl.addEventListener('scroll', () => {
    const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 24;
    follow = atBottom;
    toBottom.style.display = atBottom ? 'none' : '';
  });

  const hint = h('div.hint', {
    text: '关闭窗口不会中断任务：它在后台继续跑，随时可以点顶栏的「任务中心」重新打开。',
  });

  // ---- 一键 LNMP 的固定提示条（用户明确要求"在这个界面上显著说明"）----
  //
  // MySQL root 口令是在**这个窗口里**弹输入框问的（60 秒不答自动生成），
  // 所以"可以不干预"这句话必须出现在同一个窗口里，而不是只写在按钮的悬浮说明里。
  const lnmpHint = h('div', {
    style: {
      display: 'none', marginBottom: '10px', padding: '9px 12px',
      borderLeft: '3px solid var(--warn)', background: 'rgba(230,162,60,.12)',
      borderRadius: 'var(--radius-sm)', fontSize: '12.5px', lineHeight: '1.7',
    },
    text: 'MySQL root 口令：可以不干预 —— 稍后弹出的输入框 60 秒不回答会自动生成一个强随机口令；'
      + '装完后到「数据库 → 账号与权限」里直接点一下就能改成你想要的口令。',
  });

  const body = h('div', [head, errBox, inputBox, resultBox, trimHint, lnmpHint, logWrap, hint]);

  // 只在「一键 LNMP」这个任务上显示口令提示（标题由后端给出，形如
  // 「一键 LNMP（nginx / PHP 8.2 / MySQL 8.4）」；有的快照只带 target）。
  if (/LNMP/i.test(String(initial.title || '')) || String(initial.target || '') === 'lnmp') {
    lnmpHint.style.display = 'block';
  }

  // ---- 限时输入（如 MySQL root 口令）：倒计时、提交、落定后收框 ----
  //
  // 关键取舍（都是"看起来能用、实际会误导人"的坑）：
  //   · 倒计时以服务端 deadline 为准，不是"收到事件后从 0 开始数"；
  //   · 归零后**停止提交**（再提交必然 409），并明确说"任务会自动生成并继续"；
  //   · 提交成功后**不立刻收框**，等 input_required→null 的落定事件 —— 否则一旦
  //     任务已经用默认值继续，用户会以为是自己填的生效了；
  //   · 提交失败原样显示后端的 msg（409 的原因各不相同），绝不统一写成"提交失败"。
  let inputUi = null; // 当前挂载的输入控件；null = 没在显示

  const INPUT_RESULT_TEXT = {
    submitted: '✓ 已提交，任务已继续。',
    timeout: '⏱ 已超时，任务会自动生成并继续（不需要刷新页面）。生成的凭据会显示在本任务的安装结果里。',
    canceled: '已取消：任务被中断，你输入的内容没有被使用。',
  };

  // inputDeadlineMs 以服务端的 deadline 为准；只有它缺失/不可解析时才用
  // timeout_seconds 兜底（正常路径不该走到这里）。
  function inputDeadlineMs(req, now) {
    const t = Date.parse((req && req.deadline) || '');
    if (!Number.isNaN(t)) return t;
    return now + (Number(req && req.timeout_seconds) || 0) * 1000;
  }

  function remainingText(ms) {
    const s = Math.max(0, Math.ceil(ms / 1000));
    if (s >= 60) return `${Math.floor(s / 60)} 分 ${String(s % 60).padStart(2, '0')} 秒`;
    return `${s} 秒`;
  }

  // tickInput 由 updateStatus 每秒调用一次（耗时本来就在走字，不额外起定时器）。
  function tickInput() {
    const ui = inputUi;
    if (!ui) return;
    const left = ui.deadline - Date.now();
    if (!ui.expired && left <= 0) {
      ui.expired = true;
      ui.field.disabled = true;
      ui.submit.disabled = true;
      ui.count.textContent = '已超时';
      ui.note.textContent = '已超时，任务会自动生成并继续（不需要刷新页面）。';
      ui.note.style.display = '';
      return;
    }
    if (!ui.expired) ui.count.textContent = '剩余 ' + remainingText(left);
  }

  function mountInput(req) {
    clear(inputBox);
    const isSecret = !!req.secret;
    const field = h('input.input', {
      // secret:true → 密码框（type=password）；不回显任何"当前值"（后端也不会给）。
      type: isSecret ? 'password' : 'text',
      autocomplete: 'off',
      spellcheck: 'false',
      placeholder: isSecret ? '留空＝自动生成强随机口令' : '',
      style: { flex: '1', minWidth: '200px' },
    });
    const count = h('span.tc-input-count', {
      style: { fontSize: '12px', color: 'var(--text-dim)', whiteSpace: 'nowrap' },
      text: '',
    });
    const err = h('div.tc-input-err', {
      style: {
        display: 'none', marginTop: '8px', color: 'var(--danger)',
        fontSize: '12px', lineHeight: '1.6', whiteSpace: 'pre-wrap',
      },
    });
    const note = h('div.tc-input-note', {
      style: { display: 'none', marginTop: '8px', color: 'var(--warn)', fontSize: '12px' },
    });
    const submit = h('button.btn.btn-primary', { text: '提交', onclick: () => submitInput() });
    field.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); submitInput(); }
    });
    const reqKey = req.key || 'input';
    inputUi = {
      key: reqKey,
      deadline: inputDeadlineMs(req, Date.now()),
      expired: false,
      busy: false,
      field,
      submit,
      count,
      note,
      err,
    };
    inputBox.appendChild(h('div.tc-input-card', {
      style: {
        marginBottom: '12px', padding: '12px', background: 'var(--brand-soft)',
        border: '1px solid var(--brand)', borderRadius: '6px',
      },
    }, [
      h('div', { style: { fontWeight: '600', marginBottom: '7px' }, text: '⌨️ ' + (req.label || reqKey) }),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } },
        [field, submit, count]),
      req.hint ? h('div.hint', { style: { marginTop: '6px' }, text: req.hint }) : null,
      // 这是"限时输入"而不是"必须回答"：不填也会继续，必须让人一眼看见。
      h('div.hint', {
        style: { marginTop: '4px' },
        text: '限时输入：不填也没关系 —— 倒计时结束后任务会自动生成并继续，不需要刷新页面。',
      }),
      note, err,
    ]));
    tickInput();
  }

  function unmountInput(meta) {
    const ui = inputUi;
    inputUi = null;
    clear(inputBox);
    if (!ui) return;
    // 落定 / 任务结束：收框，但留一句结局说明（超时要明确说"会自动继续"）。
    const result = (meta && meta.input_result) || (ui.expired ? 'timeout' : '');
    const text = INPUT_RESULT_TEXT[result];
    if (text) {
      inputBox.appendChild(h('div.hint.tc-input-result', {
        style: { marginBottom: '10px', color: 'var(--warn)' },
        text,
      }));
    }
  }

  async function submitInput() {
    const ui = inputUi;
    if (!ui || ui.busy || ui.expired) return;
    ui.busy = true;
    ui.err.style.display = 'none';
    ui.submit.disabled = true;
    ui.submit.textContent = '提交中…';
    const value = ui.field.value; // 只在内存里交给后端：不写 URL / DOM 数据属性 / localStorage，也不打印
    try {
      await api.taskInput(id, ui.key, value);
      // 成功也**不收框**：等落定事件（input_required → null）再收。
      // 顺手清掉输入框里的值，避免口令在 DOM 里长时间停留。
      ui.field.value = '';
      ui.submit.textContent = '已提交，等待任务继续…';
      ui.note.textContent = '已提交，等待任务确认…（口令不会出现在日志或审计里）';
      ui.note.style.display = '';
    } catch (e) {
      // 后端 msg **原样**显示：409 可能是"已超时""已提交过""不在等这个 key"，
      // 统一改写成"提交失败"会把真正原因吞掉。
      const msg = (e && e.message) || String(e);
      ui.err.textContent = msg;
      ui.err.style.display = '';
      ui.submit.textContent = '提交';
      ui.busy = false;
      ui.submit.disabled = ui.expired;
      toast(msg, 'err', 12000);
    }
  }

  function renderInput(meta) {
    const req = STATE.inputs.get(id);
    if (req && isRunning(meta)) {
      // 刷新页面后重建窗口时，STATE.inputs 已经由 GET /tasks（或 SSE meta）填好，
      // 所以这里能直接把待输入状态恢复出来。
      if (!inputUi || inputUi.key !== (req.key || 'input')) mountInput(req);
      else { inputUi.deadline = inputDeadlineMs(req, Date.now()); tickInput(); }
      return;
    }
    // 任务结束 / 输入已落定 → 收起输入框
    if (inputUi) unmountInput(meta);
  }

  // 复制的是**这个窗口**的日志：从 STATE 里现取，而不是读 DOM ——
  // 超出 MAX_DOM_LINES 的老节点已经被丢掉，而用户期望复制到完整历史。
  function logText() {
    const arr = STATE.lines.get(id) || [];
    return arr.map((l) => (l && l.text != null ? String(l.text) : '')).join('\n');
  }

  const copyBtn = h('button.btn', {
    text: '📋 复制日志',
    onclick: () => copyText(logText(), '日志已复制'),
  });

  const cancelBtn = h('button.btn.btn-danger', {
    text: '⛔ 中断',
    title: '强制结束这个任务正在运行的命令',
    onclick: async () => {
      const meta = STATE.metas.get(id) || initial;
      const ok = await confirmBox(
        `中断「${meta.title || id}」会强制结束它正在运行的命令，可能留下装到一半的状态。\n\n` +
        '确定要中断吗？（已经写下去的文件/配置不会自动回滚）',
        { title: '中断任务', danger: true, okText: '确认中断' });
      if (!ok) return;
      cancelBtn.disabled = true;
      cancelBtn.textContent = '正在中断…';
      try {
        await api.taskCancel(id);
        toast('已发送中断请求，正在结束子进程…', 'warn');
      } catch (e) {
        toast(e.message, 'err', 9000);
      } finally {
        cancelBtn.textContent = '⛔ 中断';
        updateStatus();
      }
    },
  });

  const m = modal({
    title: initial.title || '任务详情',
    wide: true,
    body,
    footer: (close) => [
      copyBtn,
      cancelBtn,
      h('button.btn', { text: '关闭窗口（后台继续）', onclick: close }),
    ],
    onClose: () => {
      clearInterval(timer);
      unsub();
      // 关窗时清掉输入框里可能还留着的口令：它只该活在这一次提交里。
      if (inputUi) { inputUi.field.value = ''; inputUi = null; }
      clear(inputBox);
      openWindows.delete(id);
    },
  });
  openWindows.set(id, m);

  // 标题可能比窗口晚到（POST 只回 {task_id,title}，详情由 SSE/REST 补），
  // 所以状态更新时同步刷新 modal 头部的 h3。
  const h3 = m.el.querySelector('.modal-head h3');
  function setTitle(t) {
    if (h3 && t && h3.textContent !== t) h3.textContent = t;
  }

  // ---- 从 STATE 增量渲染日志（按 seq 去重，兼容"先订阅后补历史"）----
  let lastRenderedSeq = 0;
  function renderNewLines() {
    const arr = STATE.lines.get(id) || [];
    const fresh = [];
    for (const ln of arr) {
      const seq = Number(ln && ln.seq) || 0;
      if (seq && seq <= lastRenderedSeq) continue;
      if (seq) lastRenderedSeq = seq;
      fresh.push(ln);
    }
    if (!fresh.length) return;
    const frag = document.createDocumentFragment();
    for (const ln of fresh) frag.appendChild(lineEl(ln));
    logEl.appendChild(frag);
    while (logEl.childElementCount > MAX_DOM_LINES) logEl.removeChild(logEl.firstChild);
    if (follow) logEl.scrollTop = logEl.scrollHeight;
  }

  let resultSig = '';
  function renderResult(meta) {
    const r = meta && meta.result;
    const sig = r ? JSON.stringify(r) : '';
    if (sig === resultSig) return; // 每秒一次的耗时刷新不该重建可复制的区块
    resultSig = sig;
    clear(resultBox);
    if (!r) return;
    appendAll(resultBox, resultBlock(r));
  }

  function updateStatus() {
    const meta = STATE.metas.get(id) || initial;
    const status = meta.status || 'running';
    // 注意保留 .tc-status：它只是给自动化测试/后续样式用的定位标记，
    // 直接赋值 className 会把它抹掉。
    pill.className = statusPillCls(status) + ' tc-status';
    pill.textContent = statusLabel(status);

    const rec = STATE.streams.get(id);
    connHint.style.display = rec && rec.state === 'reconnecting' ? '' : 'none';

    const count = meta.line_count != null ? meta.line_count : (STATE.lines.get(id) || []).length;
    metaLine.textContent = `耗时 ${duration(elapsedSec(meta))} · ${count} 行`;

    if (meta.error) {
      errBox.style.display = '';
      errBox.textContent = '失败原因：' + meta.error;
    } else {
      errBox.style.display = 'none';
    }
    cancelBtn.disabled = !isRunning(meta);
    setTitle(meta.title);

    const oldest = STATE.oldest.get(id) || 0;
    if (oldest > 1) {
      trimHint.style.display = '';
      trimHint.textContent = `更早的 ${oldest - 1} 行已滚出服务端缓冲（每个任务只保留最近 4000 行日志）`;
    } else {
      trimHint.style.display = 'none';
    }
    renderResult(meta);
    renderInput(meta);
  }

  const unsub = onChange((kind, payload) => {
    if (payload && payload.id && payload.id !== id) return;
    if (kind === 'lines') renderNewLines();
    else updateStatus();
  });
  // 先补历史再订阅的顺序：订阅在前，历史渲染在后，
  // 中间到达的行既在 STATE 里（会被这次渲染带上），也会触发器回调（seq 去重挡住重复）。
  renderNewLines();
  updateStatus();
  // 运行中每秒走字（耗时是现算的，必须自己跳动）。
  const timer = setInterval(updateStatus, 1000);

  return m;
}

function lineEl(ln) {
  const lv = String((ln && ln.level) || 'out');
  const style = Object.assign(
    { whiteSpace: 'pre-wrap', wordBreak: 'break-word' },
    LEVEL_STYLE[lv] || LEVEL_STYLE.out,
  );
  const text = ln && ln.text != null ? String(ln.text) : '';
  // 命令行的 `$ ` 前缀由**后端**写进文本（见 services.streamCmd），
  // 前端不再补一次 —— 否则会显示成 "$ $ docker-compose ..."。
  // 放在后端还有个好处：「复制日志」粘出去自带命令标识。
  return h('div', { style, text });
}

// ---------------- 结果区块（token / address / 凭据）----------------

// resultBlock 渲染任务成功后的结果。
//
// 为什么凭据必须在这里渲染（缺陷 D11）：services/install.go 的约定是
// `result.credentials` 是**唯一允许出现口令的地方**；而 LNMP 在"写回面板配置失败"
// 时的提示是"请立刻复制本次任务结果里的一次性凭据，并在「数据库 → 连接设置」手工填入"。
// 以前这里只认 token/address、两者都没有就 return null —— 用户根本看不到那块内容，
// 那条指引就成了死路。一键建站的 db_pass 同理（它不在 credentials[] 里）。
//
// 安全红线：凭据只渲染在任务结果里（文本节点），不进 URL、不 console.log、
// 不写 DOM 的 data 属性、不进 localStorage。
function resultBlock(r) {
  const rows = [];
  const push = (label, value, copyLabel, okMsg) => {
    if (value === null || value === undefined || value === '') return;
    rows.push({ label, value: String(value), copyLabel, okMsg });
  };

  push('共享密钥', r.token, '📋 复制密钥', '密钥已复制');
  push('访问地址', r.address, '📋 复制地址', '地址已复制');

  // credentials[]：后端定义的"装完必须让用户看一眼"的一次性凭据。
  const creds = Array.isArray(r.credentials) ? r.credentials : [];
  for (const c of creds) {
    if (!c) continue;
    // label 是给人看的说明；没有 label 就退回键名（两种后端都给过）。
    push(c.label || c.key || '凭据', c.value, '📋 复制', '已复制');
  }

  // 一键建站（WordPress / Typecho）的库口令走 siteInstallResult.db_pass。
  const dbPass = r.db_pass;
  push('数据库口令', dbPass, '📋 复制口令', '口令已复制');

  if (!rows.length) return null;

  const hasSecret = !!(dbPass || creds.some((c) => c && c.value));
  return h('div.tc-result-card', {
    style: {
      marginBottom: '12px', padding: '12px', background: 'var(--warn-soft)',
      borderRadius: '6px', border: '1px solid var(--border)',
    },
  }, [
    h('div', {
      style: { fontWeight: '600', marginBottom: '8px' },
      text: hasSecret
        ? '🔑 请立刻复制保存（凭据只出现在任务结果里：不写日志、不进审计）'
        : '⚠️ 请记录以下信息（只在这里显示）',
    }),
    ...rows.map((row) => {
      // 值只进文本节点；复制按钮把 <code> 传下去，剪贴板不可用/被拒时降级为"选中文本"。
      const code = h('code.tc-cred-value', {
        style: { fontSize: '13px', userSelect: 'all', wordBreak: 'break-all' },
        text: row.value,
      });
      return h('div', { style: { marginBottom: '8px' } }, [
        h('div', { style: { fontSize: '11.5px', color: 'var(--text-dim)', marginBottom: '3px' }, text: row.label }),
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
          code,
          h('button.btn.btn-sm.tc-copy', { text: row.copyLabel, onclick: () => copyText(row.value, row.okMsg, code) }),
        ]),
      ]);
    }),
    dbPass ? h('div.hint', {
      style: { marginTop: '2px' },
      text: '数据库口令：一键建站生成的库账号口令，同时写在站点目录里的配置文件'
        + '（WordPress：wp-config.php；Typecho：config.inc.php），随时可以查。',
    }) : null,
    r.warning ? h('div', { style: { color: 'var(--warn)', fontSize: '12px' }, text: '⚠️ ' + r.warning }) : null,
  ]);
}

function copyText(text, okMsg, el) {
  const s = String(text == null ? '' : text);
  if (!s) { toast('没有可复制的内容', 'warn'); return; }
  if (!navigator.clipboard || !navigator.clipboard.writeText) {
    // 非 HTTPS / 非 localhost 下 navigator.clipboard 是 undefined：
    // 降级为"选中这段文本"让用户按 ⌘/Ctrl+C，而不是只说一句"不支持"就结束。
    selectText(el);
    toast('当前环境不支持自动复制，已选中文本，请按 ⌘/Ctrl+C 复制', 'warn', 8000);
    return;
  }
  navigator.clipboard.writeText(s)
    .then(() => toast(okMsg || '已复制', 'ok'))
    .catch(() => {
      selectText(el);
      toast('复制失败，已选中文本，请按 ⌘/Ctrl+C 复制', 'warn', 8000);
    });
}

/** selectText 是复制按钮的降级路径：把节点里的文本框选起来。 */
function selectText(el) {
  if (!el || typeof document.createRange !== 'function' || typeof window.getSelection !== 'function') return;
  try {
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  } catch { /* 选中失败也不阻断：用户还能手动三击选中 */ }
}

// ---------------- 对外入口 ----------------

function findByTarget(target) {
  if (!target) return null;
  for (const m of STATE.metas.values()) {
    if (m.target === target && isRunning(m)) return m;
  }
  return null;
}

/** init 只在第一次渲染外壳时拉一次列表（app.js 每次 renderApp 都会调，内部幂等）。 */
let initPromise = null;
function init() {
  if (STATE.inited) return initPromise;
  STATE.inited = true;
  initPromise = (async () => {
    // 有 running 就点亮徽标；**不自动弹窗**，避免在用户干别的事时打断他。
    await refresh();
  })();
  return initPromise;
}

// SUBMIT_TIMEOUT_MS 是"提交任务"这一步的硬上限。
//
// 为什么必须有（本 bug 的根因之一，见 坑清单 坑 154）：后端对这个接口是
// **立刻回 202 + task_id**（正常毫秒级），可一旦请求迟迟不落定（面板进程假死 /
// 连接被中间层挂住 / 半开连接），原来那句 `await run()` 会一直等下去 —— 确认框
// 已经关掉、进度窗又永远不会出现，用户看到的就是**点了没反应**：没有 toast、
// 没有报错、也没有真的卸载。写操作的失败必须可见，绝不允许静默。
// 同样的上限在面板首屏查询里早就有（servicePanel.js 的 PANEL_QUERY_TIMEOUT_MS）。
const SUBMIT_TIMEOUT_MS = 20000;

// submitTimeoutMs 允许测试收紧（tools/uitest.mjs 要在几秒内确定性地验证"超时要说话"）。
// setSubmitTimeoutMs(ms)：ms 为正数时生效；不传 / 非正数 = 恢复默认。
let submitTimeoutMs = SUBMIT_TIMEOUT_MS;
function setSubmitTimeoutMs(ms) {
  const n = Number(ms);
  submitTimeoutMs = (Number.isFinite(n) && n > 0) ? n : SUBMIT_TIMEOUT_MS;
  return submitTimeoutMs;
}

/** idOf 从后端各种返回形状里取出任务编号（历史上有过 task_id / task.id / id 三种）。 */
function idOf(res) {
  if (!res) return '';
  return res.task_id || (res.task && res.task.id) || res.id || '';
}

/**
 * adopt 把"已经确认创建成功"的任务接进任务中心：登记 meta、接流、刷新列表、开进度窗。
 * 正常提交与"迟到但最终成功"的提交共用这一份（后者见 start 的超时分支）——
 * 同一个动作只有一条实现，不会出现"补开的那条路少做了一步"。
 */
function adopt(id, res, { kind, target, title, onDone } = {}) {
  const meta = Object.assign({
    id,
    kind: kind || 'install',
    target: target || '',
    title: (res && res.task && res.task.title) || (res && res.title) || title || '任务',
    status: 'running',
    started_at: new Date().toISOString(),
    line_count: 0,
  }, (res && res.task) || {}, { _localAt: Date.now() });

  STATE.metas.set(id, meta);
  ensureStream(id);
  emit('meta', { id });
  refresh();          // 后台刷新列表；不 await，免得挡住进度窗
  openTask(id);

  // onDone：任务结束时回调一次，给调用方"刷列表/提示结果"用。
  // 用 onChange 订阅而不是轮询：状态变化本来就会广播（徽标就是靠它更新的）。
  // 注意只回调一次 —— 进度窗随后还会反复推 meta（耗时在走字），不能重复触发。
  if (typeof onDone === 'function') {
    const off = onChange(() => {
      const m = STATE.metas.get(id);
      if (!m || isRunning(m)) return;
      off();
      try { onDone(m); } catch (e) { console.warn('task onDone failed', e); }
    });
  }
  return id;
}

/**
 * start({kind, target, title, start, onDone, timeoutMs}) 提交一个任务。
 *
 * start 是调用方给的 Promise（`() => api.installXxx()`）：后端立刻返回
 * `{ok:true, data:{task_id, title}}`，进度靠任务中心看。返回任务编号；
 * **提交失败一律不在界面上沉默**：给出带原因的 toast 并返回 null。不抛异常
 * （调用方不需要写 try/catch），但应当检查返回值再决定要不要刷新界面。
 *
 * timeoutMs 只给测试用（默认 SUBMIT_TIMEOUT_MS）：提交超过这个时间还没落定，
 * 就如实说"没收到确认"，绝不假装成功。
 */
async function start({ kind, target, title, start: run, onDone, timeoutMs } = {}) {
  if (typeof run !== 'function') { toast('内部错误：缺少任务执行函数', 'err'); return null; }

  // 同一个 target 已经在跑：后端也会拒绝（并发安装各自独立，仅禁止重复启动同一个），
  // 这里直接把用户带到那个任务的进度窗，而不是让他看到一个 409 错误。
  const running = findByTarget(target);
  if (running) {
    toast(`「${running.title || title || target}」正在进行中`, 'warn');
    openTask(running.id);
    return running.id;
  }

  // 先把请求真的发出去；同步抛错也要落定成"可见的失败"，不能悬着。
  let submit;
  try {
    submit = Promise.resolve(run());
  } catch (e) {
    toast((title ? title + '：' : '') + ((e && e.message) || String(e)), 'err', 9000);
    return null;
  }

  const budget = (Number.isFinite(Number(timeoutMs)) && Number(timeoutMs) > 0)
    ? Number(timeoutMs) : submitTimeoutMs;
  const TIMEOUT = Symbol('submit-timeout');
  let timer = null;
  const guard = new Promise((resolve) => { timer = setTimeout(() => resolve(TIMEOUT), budget); });

  let res = null;
  let fail = null;
  try {
    res = await Promise.race([submit, guard]);
  } catch (e) {
    fail = e;
  } finally {
    clearTimeout(timer);
  }

  if (fail) {
    toast((title ? title + '：' : '') + ((fail && fail.message) || String(fail)), 'err', 9000);
    return null;
  }

  if (res === TIMEOUT) {
    // 请求没落定：**绝不能**当成成功（那是谎报），也绝不能沉默（那正是本 bug）。
    toast((title || '任务') +
      `：提交后 ${Math.max(1, Math.round(budget / 1000))} 秒内没有收到面板确认（请求被挂住了）。` +
      '请点顶栏「任务中心」确认它有没有真的开始；没有就再试一次。', 'err', 15000);
    refresh(); // 后端可能其实已经建好了任务：列表是权威的，让它有机会显示出来
    // 迟到的结果也必须有归宿：真的建了任务就补开进度窗并如实说明 ——
    // 否则这个任务永远"没人管"（用户以为没开始，其实它在后台跑）。
    submit.then((late) => {
      const lateId = idOf(late);
      if (!lateId) return;
      toast((title || '任务') + '：它其实已经创建（提交只是回得慢），正在打开进度窗', 'warn', 9000);
      adopt(lateId, late, { kind, target, title, onDone });
    }).catch((e) => {
      toast((title ? title + '：' : '') + '提交最终失败：' + ((e && e.message) || String(e)), 'err', 12000);
    });
    return null;
  }

  const id = idOf(res);
  if (!id) {
    // 后端还没切到异步版本：如实说，绝不假装任务已经启动（那会让用户以为在装，
    // 实际什么都没发生）。
    toast((title || '任务') + '：服务端没有返回任务编号（后端可能尚未启用任务中心）', 'warn', 10000);
    return null;
  }

  return adopt(id, res, { kind, target, title, onDone });
}

export const taskCenter = {
  button, init, openList, openTask, start, findByTarget, onChange, setSubmitTimeoutMs,
};
