// terminal.js —— Web 终端页面。
//
// 几个关键交互：
//   - 键盘输入直接透传给 PTY；不拦截任何组合键（Ctrl+C / Ctrl+Z / Tab 都要生效）
//   - 粘贴走 bracketed paste 语义（直接写入，交给 shell 处理）
//   - 窗口尺寸变化时同步 PTY 大小，否则 vim/top 之类会画错
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
// statusBar 必须和 containerEl / infoEl 一样放在模块级：
// renderStatus() 是模块级函数，却要往这个元素里塞按钮。之前它是
// TerminalView 里的 const，于是 renderStatus 一调用就抛
// "statusBar is not defined"（终端状态栏永远不更新，而且只在浏览器
// 控制台留下一行无文件名的 ReferenceError）。
let statusBar = null;

export function TerminalView(content, ctx = {}) {
  clear(content);

  const screen = h('div.term-screen', { tabindex: '0' });
  statusBar = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } });
  infoEl = h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.6' } });
  containerEl = screen;

  content.append(
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: 'Web 终端' }),
        h('div.spacer'),
        statusBar,
      ]),
      h('div.card-body.tight', { style: { padding: '0' } }, [screen]),
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
      onclick: () => { if (term) { term.reset(); term.render(containerEl); } containerEl.focus(); },
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

function closeWS() {
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
    h('div', { text: `Shell：${info.shell}　起始目录：${info.home || '~'}　空闲超时：${info.idle_timeout_mins} 分钟　会话上限：${info.max_sessions}` }),
  );

  // 初始化网格：按容器实际尺寸推算列数行数
  const cols = estimateCols(containerEl);
  const rows = estimateRows(containerEl);
  term = new Terminal(cols, rows);
  clear(containerEl);
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
    containerEl.focus();
  };

  ws.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.type === 'output') {
      term.write(msg.data);
      term.render(containerEl);
      scrollToBottom();
    } else if (msg.type === 'close') {
      if (msg.data) { term.write(msg.data); term.render(containerEl); }
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

function estimateCols(el) {
  const w = el.clientWidth || 1000;
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
  send({ type: 'resize', cols, rows });
}

function scrollToBottom() {
  if (!containerEl) return;
  // 容器本身不滚动（网格固定行数），这里保证父级可见
  const scroller = containerEl.parentElement;
  if (scroller) scroller.scrollTop = scroller.scrollHeight;
}
