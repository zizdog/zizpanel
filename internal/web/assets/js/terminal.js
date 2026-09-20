// terminal.js —— Web 终端页面。
//
// 几个关键交互：
//   - 键盘输入直接透传给 PTY；不拦截任何组合键（Ctrl+C / Ctrl+Z / Tab 都要生效）
//   - 粘贴走 bracketed paste 语义（直接写入，交给 shell 处理）
//   - 窗口尺寸变化时同步 PTY 大小，否则 vim/top 之类会画错
//   - 输出在 .term-screen 内部滚动，滚出去的行进回滚历史；默认自动贴底，
//     用户手动上滚后暂停贴底并给出「↓ 回到底部」
//   - 断线后显示原因并提供"重连"，而不是静默失效
//   - 顶部明确标注当前身份（默认是普通用户，不是 root）
//
// ---------------------------------------------------------------------------
//  保活（用户要求，与文件编辑器最小化同等要求）
// ---------------------------------------------------------------------------
//  面板是 hash 路由 SPA：切板块时视图 DOM 会整个重建。终端**不能**因此被切断 ——
//  只要没登出面板，切走再回来，屏幕内容、回滚历史、正在跑的命令都必须还在，
//  而且还能继续输入执行。
//
//  做法：把终端实例（term）、WebSocket（ws）与整块终端 DOM（dom）缓存在
//  **模块级**；TerminalView 每次挂载只是把这些**同一个节点重新挂到新容器**里，
//  连接全程不动。只有两种情况才真正销毁：
//    · 用户点「关闭会话」（会话本身就该结束）；
//    · 登出面板（app.js 的 doLogout 调 destroyTerminal()）。
//
//  ⚠️ 路由切换时 app.js 会执行 onLeave 清理，所以这里的清理**绝不能** closeWS ——
//  它只摘掉 window resize 监听，连接与 DOM 全部保留。

import { api } from './api.js';
import { h, clear, toast, confirmBox, appendAll } from './ui.js';
import { Terminal } from './ansi.js';

// ---------------- 模块级持久状态（跨路由复用的那部分） ----------------
let ws = null;
let term = null;
let heartbeat = null;
// follow：是否自动贴底。用户手动往上滚看历史时置 false，
// 否则每来一行输出就把他拽回底部，等于没法回看。
let follow = true;
// 心跳：后端给 WebSocket 读了 75 秒的超时，用来发现"对端被强杀、没有 FIN"的情况。
// 真实空闲的会话必须靠它不断刷新期限，否则用户走开一会儿终端就被判死了。
const HEARTBEAT_MS = 25000;

// dom 是整块终端 UI 的持久节点（screen / statusBar / infoEl / toBottomBtn）。
// 连 statusBar 与 infoEl 也持久化：renderStatus() 是模块级函数，
// 若每次挂载都换新节点，切走后连接上收到的输出就会画进已经脱离文档的旧节点。
let dom = null;

// gen 用来作废"挂起的异步启动"：登出/重连会让上一次 start() 的 await 变成过期操作，
// 否则用户登出后，一个尚未返回的 terminalInfo() 仍会建立一条新的 shell 连接。
let gen = 0;

// sessionClosed：是否已显式关闭（点「关闭会话」或登出）。关闭后不再自动重连，
// 直到用户点状态栏里的「重连」。
let sessionClosed = false;

// lastFailure：最近一次"连不上"的结论（advice/detail）。切走板块再回来时
// screen 会被 term.render 重画，用它把结论补回来，细节不丢。
let lastFailure = null;

export function TerminalView(content, ctx = {}) {
  clear(content);
  const d = ensureDom();

  // 每次挂载都用**同一批持久节点**重建卡片：DOM 被移动而非重建，
  // 所以 screen 里的回滚历史、滚动位置、statusBar 的当前状态都原样保留。
  content.append(
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: 'Web 终端' }),
        h('div.spacer'),
        d.statusBar,
      ]),
      h('div.card-body.tight', { style: { padding: '0' } }, [d.screenWrap]),
      h('div', { style: { padding: '10px 14px', borderTop: '1px solid var(--border-soft)' } }, [d.infoEl]),
    ]),
  );

  if (!term) {
    // 还没有会话（首次进入 / 上次读到"未启用"后重进）→ 建立。
    start();
  } else {
    // 已有会话：**不重连、不清屏**，只把持久 DOM 重新挂上、按新尺寸 fit。
    reattach();
  }

  // 屏幕尺寸变化时同步给后端。
  // 监听是"每次挂载一份、离开时摘掉"的：connection 保活，但监听不能叠加。
  const onResize = () => fitSize();
  window.addEventListener('resize', onResize);
  if (ctx.onLeave) {
    ctx.onLeave(() => {
      window.removeEventListener('resize', onResize);
      // 刻意**不** closeWS()、不销毁 term：这就是保活的关键。
    });
  }
}

// ensureDom 只构造一次持久节点，并把与"会话无关"的事件监听挂一次。
function ensureDom() {
  if (dom) return dom;

  // .term-screen 自己就是滚动容器（overflow: auto）：滚出屏幕顶部的行
  // 会被保留在它内部的 .term-history 里，所以滚轮/滚动条在这里生效，
  // 而不是去撑高整个页面。
  const screen = h('div.term-screen', { tabindex: '0' });
  const toBottomBtn = h('button.btn.btn-sm', {
    text: '↓ 回到底部',
    style: { display: 'none', position: 'absolute', right: '14px', bottom: '12px', boxShadow: 'var(--shadow)' },
    onclick: () => {
      follow = true;
      screen.scrollTop = screen.scrollHeight;
      toBottomBtn.style.display = 'none';
      screen.focus();
    },
  });
  const screenWrap = h('div', { style: { position: 'relative' } }, [screen, toBottomBtn]);
  const statusBar = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } });
  const infoEl = h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.6' } });

  // 用户手动往上滚 → 暂停自动贴底并给出「回到底部」；滚回底部 → 恢复自动贴底。
  screen.addEventListener('scroll', () => {
    const atBottom = screen.scrollHeight - screen.scrollTop - screen.clientHeight < 24;
    follow = atBottom;
    toBottomBtn.style.display = atBottom ? 'none' : '';
  });

  // 键盘输入 -> PTY（监听只挂一次，重连不会叠加）。
  screen.addEventListener('keydown', onKeyDown);
  screen.addEventListener('paste', onPaste);
  screen.addEventListener('click', () => screen.focus());

  dom = { screen, screenWrap, toBottomBtn, statusBar, infoEl };
  return dom;
}

function renderStatus(state, text) {
  if (!dom) return;
  clear(dom.statusBar);
  appendAll(dom.statusBar,
    h('span.pill' + (state === 'ok' ? '.ok' : state === 'err' ? '.danger' : '.warn'), { text }),
    h('button.btn.btn-sm', {
      text: '清屏',
      onclick: () => {
        // reset() 同时清网格与回滚历史：清屏按钮的语义就是"把屏幕清干净"，
        // 只清可见区而把历史留着，用户会以为清屏没生效。
        if (term) { term.reset(); term.render(dom.screen); }
        follow = true;
        if (dom.toBottomBtn) dom.toBottomBtn.style.display = 'none';
        dom.screen.scrollTop = 0;
        dom.screen.focus();
      },
    }),
    h('button.btn.btn-sm', {
      text: '复制全部',
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(term ? term.plainText() : '');
          toast('已复制终端内容', 'ok');
        } catch { toast('复制失败（浏览器限制）', 'warn'); }
      },
    }),
    h('button.btn.btn-sm', {
      text: '重连',
      onclick: () => { closeWS(); start(); },
    }),
    h('button.btn.btn-sm.btn-danger', {
      text: '关闭会话',
      onclick: async () => {
        if (!await confirmBox('关闭当前终端会话？正在运行的任务会被终止。', { title: '关闭终端', danger: true, okText: '关闭' })) return;
        // 这是"主动关闭"：连接真的断掉，并标记为已关闭（切页回来不会偷偷重连）。
        closeWS();
        sessionClosed = true;
        renderStatus('', '会话已关闭');
      },
    }),
  );
}

// stopHeartbeat 幂等：重复调用不会出错。
function stopHeartbeat() {
  if (heartbeat) {
    clearInterval(heartbeat);
    heartbeat = null;
  }
}

function closeWS() {
  stopHeartbeat();
  if (ws) {
    try { ws.close(); } catch { /* 忽略 */ }
    ws = null;
  }
}

// destroyTerminal 是**唯一**的销毁入口：登出面板时由 app.js 调用。
// 主动点「关闭会话」只断连接（term 留着让用户还能看到关闭前的内容）。
export function destroyTerminal() {
  gen++; // 作废所有挂起的 start()
  closeWS();
  term = null;
  sessionClosed = false;
  lastFailure = null;
  follow = true;
  if (dom) {
    clear(dom.screen);
    clear(dom.statusBar);
    clear(dom.infoEl);
    dom.toBottomBtn.style.display = 'none';
  }
}

// reattach 把一个**仍然活着的**会话重新挂到刚重建的视图里。
function reattach() {
  if (!dom || !term) return;
  const el = dom.screen;
  // 先按当前 DOM 重画一遍（脱离文档期间的输出会在这时补齐结构），
  // 再按新容器尺寸 fit —— 否则 vim/top 之类按旧列数画的界面会错位。
  term.render(el);
  fitSize();
  if (follow) el.scrollTop = el.scrollHeight;
  el.focus();
  // 上次是"连不上"：结论要补回屏幕上（statusBar 的 pill 还在，细节不能丢）
  if (lastFailure) showConnectFailure(lastFailure.advice, lastFailure.detail);
}

// showUnavailable 在终端区域里画一个"读不到状态"的说明（绝不猜一个默认状态）。
function showUnavailable(title, detail, withRetry) {
  if (!dom) return;
  clear(dom.screen);
  appendAll(dom.screen, h('div.empty', [
    h('div.big', { text: '⚠️' }),
    h('h4', { text: title }),
    h('p', { text: detail }),
    withRetry ? h('div', { style: { marginTop: '16px' } }, [
      h('button.btn.btn-sm', { text: '↻ 重试', onclick: () => start() }),
    ]) : null,
  ]));
}

// showConnectFailure 显示"连不上"的可行动结论：首行 ≤40 字（服务端给），
// 细节收进折叠项。**绝不只显示错误码** —— 用户要知道去哪儿开 WebSocket 透传。
function showConnectFailure(advice, detail) {
  if (!dom) return;
  lastFailure = { advice, detail };
  clear(dom.screen);
  appendAll(dom.screen, h('div.empty', [
    h('div.big', { text: '⚠️' }),
    h('h4', { text: advice }),
    h('details', {
      style: { marginTop: '8px', textAlign: 'left', maxWidth: '760px' },
    }, [
      h('summary', { style: { cursor: 'pointer', color: 'var(--text-dim)' }, text: '详情' }),
      h('div', { style: { marginTop: '6px', color: 'var(--text-dim)', lineHeight: '1.7' }, text: detail }),
    ]),
    h('div', { style: { marginTop: '14px' } }, [
      h('button.btn.btn-sm', { text: '↻ 重连', onclick: () => start() }),
    ]),
  ]));
}

// diagnoseAndShow：WS 从未连上时浏览器只给 1006、读不到失败原因，
// 用普通 HTTP 请求向面板要结论（判据与文案都在服务端，前端不改写）。
async function diagnoseAndShow(my) {
  let v = null;
  try { v = await api.terminalWSDiagnose(); } catch { /* 面板都读不到，走兜底 */ }
  if (my !== gen) return; // 已被登出/重连取代
  const advice = v && v.advice ? v.advice : '连不上终端：请在反向代理那层开启 WebSocket 透传';
  const detail = v && v.detail ? v.detail : '浏览器没拿到握手失败的原因，可换直连地址再试。';
  renderStatus('err', advice);
  showConnectFailure(advice, detail);
}

async function start() {
  const d = ensureDom();
  const my = ++gen;
  sessionClosed = false;
  lastFailure = null;

  let info;
  try {
    info = await api.terminalInfo();
  } catch (e) {
    if (my !== gen) return; // 已被登出/重连取代
    showUnavailable('无法读取终端状态', e.message || String(e), true);
    return;
  }
  if (my !== gen) return;

  if (!info.enabled) {
    clear(d.screen);
    appendAll(d.screen, h('div.empty', [
      h('div.big', { text: '🔒' }),
      h('h4', { text: 'Web 终端未启用' }),
      h('p', { text: '这是一个高权限功能（等于把本机 shell 交给浏览器），因此默认关闭。' }),
      h('div', { style: { marginTop: '16px' } }, [
        h('button.btn.btn-primary', {
          // 终端的开关已并入「面板设置 → 访问与安全」（2026-09-19 信息架构调整）。
          text: '前往面板设置开启',
          onclick: () => { location.hash = '#/settings/access'; },
        }),
      ]),
    ]));
    renderStatus('', '未启用');
    clear(d.infoEl);
    return;
  }

  // 状态信息（明确告知当前身份，避免误以为一定是 root）
  clear(d.infoEl);
  appendAll(d.infoEl,
    h('div', { text: info.note }),
    // 空闲超时 0 的含义是"不限制"（见 internal/term 的 IdleTimeout），
    // 直接写"0 分钟"会让人以为会话立刻过期 —— 这是用户真正会误读的一句话。
    h('div', {
      text: `Shell：${info.shell}　起始目录：${info.home || '~'}　空闲超时：`
        + `${info.idle_timeout_mins > 0 ? info.idle_timeout_mins + ' 分钟' : '不限制'}`
        + `　会话上限：${info.max_sessions}`,
    }),
  );

  // 初始化网格：按容器实际尺寸推算列数行数
  const cols = estimateCols(d.screen);
  const rows = estimateRows(d.screen);
  term = new Terminal(cols, rows);
  clear(d.screen);
  // 新建会话都回到"自动贴底"的初始状态
  follow = true;
  if (d.toBottomBtn) d.toBottomBtn.style.display = 'none';
  renderStatus('connecting', '正在连接…');

  // 建立 WebSocket
  let url;
  try {
    url = api.terminalWSURL();
  } catch (e) {
    renderStatus('err', '无法构造连接地址');
    return;
  }
  const sock = new WebSocket(url);
  ws = sock;
  // opened：本次连接是否真的升级成功过。没成功过才可能是反代吃掉了
  // Connection/Upgrade；成功过再断是链路中断，不能给同一句结论。
  let opened = false;

  sock.onopen = () => {
    if (sock !== ws) return; // 已被重连替换
    opened = true;
    lastFailure = null;
    renderStatus('ok', '已连接');
    send({ type: 'resize', cols: term.cols, rows: term.rows });
    // 心跳：让服务端知道"对端还活着"（服务端读超时 75s，这里 25s 一次）。
    stopHeartbeat();
    heartbeat = setInterval(() => send({ type: 'ping' }), HEARTBEAT_MS);
    d.screen.focus();
  };

  sock.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.type === 'output') {
      paintOutput(msg.data);
    } else if (msg.type === 'close') {
      if (msg.data) paintOutput(msg.data);
      renderStatus('', '会话已结束');
    } else if (msg.type === 'error') {
      renderStatus('err', msg.error || '终端错误');
      toast(msg.error || '终端错误', 'err', 10000);
    }
  };

  sock.onerror = () => {
    if (sock !== ws) return;
    renderStatus('err', '连接出错');
  };

  sock.onclose = (ev) => {
    if (sock !== ws) return; // 旧 socket（已被重连替换）的关闭事件，忽略
    stopHeartbeat();
    ws = null;
    if (ev.code === 1000) return; // 正常关闭（用户点「关闭会话」）
    if (opened) {
      // 连上过再断：是链路中断，不能推给 WebSocket 透传。
      renderStatus('err', `连接中断（代码 ${ev.code}）`);
      showConnectFailure(`连接中断（代码 ${ev.code}）`,
        '链路被中间层掐断（常见于反向代理的 WebSocket 超时）。可点「重连」；'
        + '若反复发生，请检查反向代理那一层的 WebSocket 透传与超时设置。');
      return;
    }
    // 从未连上：浏览器只给 1006、读不到原因，去向面板要结论。
    diagnoseAndShow(my);
  };
}

function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(obj));
  }
}

/**
 * paintOutput 写入一段 PTY 输出并处理滚动位置。
 *
 * 自动贴底的判定必须放在 render 之后：只有渲染完 scrollHeight 才是新的。
 */
function paintOutput(data) {
  if (!term || !dom) return;
  const el = dom.screen;
  const prevHeight = el.scrollHeight;
  term.write(data);
  term.render(el);
  if (follow) {
    el.scrollTop = el.scrollHeight;
  } else {
    // 用户正在翻历史：新行是从上方（.term-history）插入的，
    // 把 scrollTop 加上同样的高度增量，让他盯着的那几行留在原地，
    // 而不是被新输出向上顶走。
    el.scrollTop += el.scrollHeight - prevHeight;
  }
}

function onKeyDown(e) {
  if (!ws || ws.readyState !== WebSocket.OPEN) return;
  // Ctrl/Cmd+C 在没有选中文本时代表中断信号，需要透传
  const hasSelection = window.getSelection() && String(window.getSelection());
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'c' && hasSelection) {
    return; // 让浏览器执行复制
  }
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'v') {
    return; // 交给 paste 事件
  }

  let data = null;
  if (e.key === 'Enter') data = '\r';
  else if (e.key === 'Backspace') data = '\x7f';
  else if (e.key === 'Tab') data = '\t';
  else if (e.key === 'Escape') data = '\x1b';
  else if (e.key === 'ArrowUp') data = '\x1b[A';
  else if (e.key === 'ArrowDown') data = '\x1b[B';
  else if (e.key === 'ArrowRight') data = '\x1b[C';
  else if (e.key === 'ArrowLeft') data = '\x1b[D';
  else if (e.key === 'Home') data = '\x1b[H';
  else if (e.key === 'End') data = '\x1b[F';
  else if (e.key === 'PageUp') data = '\x1b[5~';
  else if (e.key === 'PageDown') data = '\x1b[6~';
  else if (e.key === 'Delete') data = '\x1b[3~';
  else if (e.ctrlKey && e.key.length === 1) {
    // Ctrl+A..Z -> 0x01..0x1a
    const code = e.key.toUpperCase().charCodeAt(0) - 64;
    if (code >= 1 && code <= 26) data = String.fromCharCode(code);
  } else if (e.key.length === 1) {
    data = e.key;
  }

  if (data !== null) {
    e.preventDefault();
    send({ type: 'input', data });
    // 本地回显由 PTY 的 echo 负责，这里不做
  }
}

function onPaste(e) {
  e.preventDefault();
  const text = (e.clipboardData || window.clipboardData).getData('text');
  if (text) send({ type: 'input', data: text });
}

// ---------- 尺寸推算 ----------

const CHAR_W = 8.4;   // 与 CSS 中的等宽字体字号对应
const CHAR_H = 18;

// 滚动条的占位宽度。终端是定宽等宽网格：一旦可用宽度变小挤出**横向**滚动条，
// 字符对齐、选区、光标位置都会整体错位。所以列数必须按"扣掉滚动条"的宽度算。
// overlay 滚动条（macOS 默认）不占布局宽度，量出来是 0，也就没有这层风险。
let scrollbarPx = -1;

function measureScrollbar(el) {
  if (scrollbarPx >= 0) return scrollbarPx;
  const prev = el.style.overflowY;
  el.style.overflowY = 'scroll'; // 强制占位滚动条出现，量一次真实宽度
  scrollbarPx = Math.max(0, el.offsetWidth - el.clientWidth);
  el.style.overflowY = prev;
  return scrollbarPx;
}

function estimateCols(el) {
  const w = (el.clientWidth || 1000) - measureScrollbar(el);
  return Math.max(40, Math.floor((w - 24) / CHAR_W));
}

function estimateRows(el) {
  const hgt = el.clientHeight || 520;
  return Math.max(12, Math.floor((hgt - 24) / CHAR_H));
}

function fitSize() {
  if (!term || !dom) return;
  const el = dom.screen;
  const cols = estimateCols(el);
  const rows = estimateRows(el);
  if (cols === term.cols && rows === term.rows) return;
  term.resize(cols, rows);
  term.render(el);
  if (follow) el.scrollTop = el.scrollHeight;
  send({ type: 'resize', cols, rows });
}
