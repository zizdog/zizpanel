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
const LEVEL_STYLE = {
  step: { fontWeight: '650', color: '#e6ebf5' },
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

/** upsertMeta 只处理"实时"来源（SSE status / POST 返回值），失败会弹一次 toast。 */
function upsertMeta(m) {
  if (!m || !m.id) return;
  STATE.metas.set(m.id, m);
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
      STATE.metas.set(id, Object.assign({}, prev, d.task));
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
      .map((t) => [t.id, t.status, t.line_count, t.last, t.elapsed_ms].join('|')).join(';');
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

  const body = h('div', [head, errBox, resultBox, trimHint, logWrap, hint]);

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

// ---------------- 结果区块（token / address）----------------

function resultBlock(r) {
  const rows = [];
  if (r.token) rows.push({ label: '共享密钥', value: String(r.token), copy: '📋 复制密钥', ok: '密钥已复制' });
  if (r.address) rows.push({ label: '访问地址', value: String(r.address), copy: '📋 复制地址', ok: '地址已复制' });
  if (!rows.length) return null;
  return h('div', {
    style: {
      marginBottom: '12px', padding: '12px', background: 'var(--warn-soft)',
      borderRadius: '6px', border: '1px solid var(--border)',
    },
  }, [
    h('div', { style: { fontWeight: '600', marginBottom: '8px' }, text: '⚠️ 请记录以下信息（只在这里显示）' }),
    ...rows.map((row) => h('div', { style: { marginBottom: '8px' } }, [
      h('div', { style: { fontSize: '11.5px', color: 'var(--text-dim)', marginBottom: '3px' }, text: row.label }),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('code', { style: { fontSize: '13px', userSelect: 'all', wordBreak: 'break-all' }, text: row.value }),
        h('button.btn.btn-sm', { text: row.copy, onclick: () => copyText(row.value, row.ok) }),
      ]),
    ])),
    r.warning ? h('div', { style: { color: 'var(--warn)', fontSize: '12px' }, text: '⚠️ ' + r.warning }) : null,
  ]);
}

function copyText(text, okMsg) {
  const s = String(text == null ? '' : text);
  if (!s) { toast('没有可复制的内容', 'warn'); return; }
  if (!navigator.clipboard || !navigator.clipboard.writeText) {
    // 非 HTTPS / 非 localhost 下 navigator.clipboard 是 undefined，
    // 这时候要如实告诉用户"手动选中复制"，而不是静默失败。
    toast('当前环境不支持自动复制，请手动选中复制', 'warn');
    return;
  }
  navigator.clipboard.writeText(s)
    .then(() => toast(okMsg || '已复制', 'ok'))
    .catch(() => toast('复制失败，请手动选中复制', 'warn'));
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

/**
 * start({kind, target, title, start}) 提交一个任务。
 *
 * start 是调用方给的 Promise（`() => api.installXxx()`）：后端现在立刻返回
 * `{ok:true, data:{task_id, title}}`，进度靠任务中心看。失败（4xx）时给出 toast，
 * 不抛异常 —— 调用方不需要写 try/catch。
 */
async function start({ kind, target, title, start: run, onDone } = {}) {
  if (typeof run !== 'function') { toast('内部错误：缺少任务执行函数', 'err'); return null; }

  // 同一个 target 已经在跑：后端也会拒绝（并发安装各自独立，仅禁止重复启动同一个），
  // 这里直接把用户带到那个任务的进度窗，而不是让他看到一个 409 错误。
  const running = findByTarget(target);
  if (running) {
    toast(`「${running.title || title || target}」正在进行中`, 'warn');
    openTask(running.id);
    return running.id;
  }

  let res = null;
  try {
    res = await run();
  } catch (e) {
    toast((title ? title + '：' : '') + ((e && e.message) || String(e)), 'err', 9000);
    return null;
  }

  const id = res && (res.task_id || (res.task && res.task.id) || res.id);
  if (!id) {
    // 后端还没切到异步版本：如实说，绝不假装任务已经启动（那会让用户以为在装，
    // 实际什么都没发生）。
    toast((title || '任务') + '：服务端没有返回任务编号（后端可能尚未启用任务中心）', 'warn', 10000);
    return null;
  }

  const meta = Object.assign({
    id,
    kind: kind || 'install',
    target: target || '',
    title: (res.task && res.task.title) || res.title || title || '任务',
    status: 'running',
    started_at: new Date().toISOString(),
    line_count: 0,
  }, res.task || {}, { _localAt: Date.now() });

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

export const taskCenter = { button, init, openList, openTask, start, findByTarget, onChange };
