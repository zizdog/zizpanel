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

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';
import { Terminal } from './ansi.js';

let ws = null;
let term = null;
let containerEl = null;
let infoEl = null;
// follow：是否自动贴底。用户手动往上滚看历史时置 false，
// 否则每来一行输出就把他拽回底部，等于没法回看。
let follow = true;
// 「↓ 回到底部」按钮（做法与任务中心进度窗的 tc-tobottom 一致）
let toBottomBtn = null;
// statusBar 必须和 containerEl / infoEl 一样放在模块级：
// renderStatus() 是模块级函数，却要往这个元素里塞按钮。之前它是
// TerminalView 里的 const，于是 renderStatus 一调用就抛
// "statusBar is not defined"（终端状态栏永远不更新，而且只在浏览器
// 控制台留下一行无文件名的 ReferenceError）。
let statusBar = null;
// 心跳：后端给 WebSocket 读了 75 秒的超时，用来发现"对端被强杀、没有 FIN"的情况。
// 真实空闲的会话必须靠它不断刷新期限，否则用户走开一会儿终端就被判死了。
let heartbeat = null;
const HEARTBEAT_MS = 25000;

export function TerminalView(content, ctx = {}) {
  clear(content);

  // .term-screen 自己就是滚动容器（overflow: auto）：滚出屏幕顶部的行
  // 会被保留在它内部的 .term-history 里，所以滚轮/滚动条在这里生效，
  // 而不是去撑高整个页面。
  const screen = h('div.term-screen', { tabindex: '0' });
  statusBar = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } });
  infoEl = h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.6' } });
  containerEl = screen;
  follow = true;

  toBottomBtn = h('button.btn.btn-sm', {
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

  // 用户手动往上滚 → 暂停自动贴底并给出「回到底部」；滚回底部 → 恢复自动贴底。
  screen.addEventListener('scroll', () => {
    const atBottom = screen.scrollHeight - screen.scrollTop - screen.clientHeight < 24;
    follow = atBottom;
    toBottomBtn.style.display = atBottom ? 'none' : '';
  });

  content.append(
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: 'Web 终端' }),
        h('div.spacer'),
        statusBar,
      ]),
      h('div.card-body.tight', { style: { padding: '0' } }, [screenWrap]),
      h('div', { style: { padding: '10px 14px', borderTop: '1px solid var(--border-soft)' } }, [infoEl]),
    ]),
  );

  start();

  // 屏幕尺寸变化时同步给后端
  const onResize = () => fitSize();
  window.addEventListener('resize', onResize);
  registerCleanup(() => {
    window.removeEventListener('resize', onResize);
    closeWS();
  });
}

function renderStatus(state, text) {
  clear(statusBar);
  appendAll(statusBar,
    h('span.pill' + (state === 'ok' ? '.ok' : state === 'err' ? '.danger' : '.warn'), { text }),
    h('button.btn.btn-sm', {
      text: '清屏',
      onclick: () => {
        // reset() 同时清网格与回滚历史：清屏按钮的语义就是"把屏幕清干净"，
        // 只清可见区而把历史留着，用户会以为清屏没生效。
        if (term) { term.reset(); term.render(containerEl); }
        follow = true;
        if (toBottomBtn) toBottomBtn.style.display = 'none';
        containerEl.scrollTop = 0;
        containerEl.focus();
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
        closeWS();
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

async function start() {
  let info;
  try {
    info = await api.terminalInfo();
  } catch (e) {
    clear(containerEl);
    appendAll(containerEl, h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: '无法读取终端状态' }),
      h('p', { text: e.message }),
    ]));
    return;
  }

  if (!info.enabled) {
    clear(containerEl);
    appendAll(containerEl, h('div.empty', [
      h('div.big', { text: '🔒' }),
      h('h4', { text: 'Web 终端未启用' }),
      h('p', { text: '这是一个高权限功能（等于把本机 shell 交给浏览器），因此默认关闭。' }),
      h('div', { style: { marginTop: '16px' } }, [
        h('button.btn.btn-primary', {
          text: '前往面板设置开启',
          onclick: () => { location.hash = '#/settings'; },
        }),
      ]),
    ]));
    return;
  }

  // 状态信息（明确告知当前身份，避免误以为一定是 root）
  clear(infoEl);
  appendAll(infoEl,
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
  const cols = estimateCols(containerEl);
  const rows = estimateRows(containerEl);
  term = new Terminal(cols, rows);
  clear(containerEl);
  // 重连/新会话都回到"自动贴底"的初始状态
  follow = true;
  if (toBottomBtn) toBottomBtn.style.display = 'none';
  renderStatus('connecting', '正在连接…');

  // 建立 WebSocket
  let url;
  try {
    url = api.terminalWSURL();
  } catch (e) {
    renderStatus('err', '无法构造连接地址');
    return;
  }
  ws = new WebSocket(url);

  ws.onopen = () => {
    renderStatus('ok', '已连接');
    send({ type: 'resize', cols: term.cols, rows: term.rows });
    // 心跳：让服务端知道"对端还活着"（服务端读超时 75s，这里 25s 一次）。
    stopHeartbeat();
    heartbeat = setInterval(() => send({ type: 'ping' }), HEARTBEAT_MS);
    containerEl.focus();
  };

  ws.onmessage = (ev) => {
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

  ws.onerror = () => {
    renderStatus('err', '连接出错');
  };

  ws.onclose = (ev) => {
    stopHeartbeat();
    if (ev.code !== 1000) {
      renderStatus('err', `连接已断开（代码 ${ev.code}）。可点"重连"。`);
    }
  };

  // 键盘输入 -> PTY
  containerEl.addEventListener('keydown', onKeyDown);
  containerEl.addEventListener('paste', onPaste);
  containerEl.addEventListener('click', () => containerEl.focus());
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
  if (!term || !containerEl) return;
  const prevHeight = containerEl.scrollHeight;
  term.write(data);
  term.render(containerEl);
  if (follow) {
    containerEl.scrollTop = containerEl.scrollHeight;
  } else {
    // 用户正在翻历史：新行是从上方（.term-history）插入的，
    // 把 scrollTop 加上同样的高度增量，让他盯着的那几行留在原地，
    // 而不是被新输出向上顶走。
    containerEl.scrollTop += containerEl.scrollHeight - prevHeight;
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
  if (!term || !containerEl) return;
  const cols = estimateCols(containerEl);
  const rows = estimateRows(containerEl);
  if (cols === term.cols && rows === term.rows) return;
  term.resize(cols, rows);
  term.render(containerEl);
  if (follow) containerEl.scrollTop = containerEl.scrollHeight;
  send({ type: 'resize', cols, rows });
}
