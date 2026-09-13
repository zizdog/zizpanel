// ui.js —— 轻量 DOM 构建与通用组件。
//
// 为什么不用框架：面板要长期稳定运行且要能嵌入单个 Go 二进制，
// 无构建步骤意味着升级面板不需要保证 Node 版本、不需要 CI。
// 这里用一个 h() 构建器替代 JSX，写法足够简洁，且零依赖。

/** h('div.card', {onclick}, [children]) —— 极简 DOM 构建器。 */
export function h(spec, props, children) {
  if (props === undefined || props === null) { props = {}; }
  // 第二个参数还可以是"子节点"而不是 props。
  // 关键：DOM Node 也必须被识别为子节点 —— 否则 h('div', someElement)
  // 会把元素当成属性对象遍历，结果是内容被静默丢弃（这个 bug 实际发生过：
  // modal({body: h(...)}) 的弹窗内容整体消失，排查成本很高）。
  if (props instanceof Node || Array.isArray(props) ||
      typeof props === 'string' || typeof props === 'number') {
    children = props; props = {};
  }
  const [tagPart, ...classParts] = String(spec).split('.');
  const [tag, id] = tagPart.split('#');
  const el = document.createElement(tag || 'div');
  if (id) el.id = id;
  if (classParts.length) el.className = classParts.join(' ');

  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class' || k === 'className') { el.className = [el.className, v].filter(Boolean).join(' '); }
    else if (k === 'style' && typeof v === 'object') { Object.assign(el.style, v); }
    else if (k === 'html') { el.innerHTML = v; }
    else if (k === 'text') { el.textContent = v; }
    else if (k === 'dataset') { Object.assign(el.dataset, v); }
    else if (k.startsWith('on') && typeof v === 'function') { el.addEventListener(k.slice(2).toLowerCase(), v); }
    else if (k in el && k !== 'list' && typeof v !== 'object') { try { el[k] = v; } catch { el.setAttribute(k, v); } }
    else { el.setAttribute(k, v); }
  }
  append(el, children);
  return el;
}

export function append(parent, children) {
  // 跳过"没有内容"的值：null / undefined / false。
  //
  // 这个判断必须在数组展开的每一层都生效 —— 它同时也是数组元素的分支入口，
  // 所以写成"提前返回"即可覆盖两种情况。
  // 曾经这里只判断了顶层参数，导致 h('div', [a, null, b]) 会把 null
  // 渲染成字面量文本 "null"（工具栏上真的显示过一个 null）。
  //
  // 注意：0 和空字符串是合法内容，必须保留。
  if (children === null || children === undefined || children === false) return parent;
  if (Array.isArray(children)) { children.forEach((c) => append(parent, c)); return parent; }
  if (children instanceof Node) { parent.appendChild(children); return parent; }
  parent.appendChild(document.createTextNode(String(children)));
  return parent;
}

/** clear(el) 清空子节点。 */
export function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }

// appendAll(parent, a, b, c, ...) —— 安全版的 Element.append。
//
// 为什么需要它：浏览器原生的 Element.append() 会把 null / undefined
// 转成文本节点 "null" 插进 DOM。而"条件渲染"（`cond ? el : null`）
// 是最常用的写法，于是页面上会莫名出现字面量 null。
// 这个坑真实发生过：服务管理页与应用市场页的工具栏都显示了一个 null。
//
// 用法：需要追加"可能为 null 的节点"时，用 appendAll 而不是原生 append。
export function appendAll(parent, ...items) {
  for (const it of items) {
    if (it === null || it === undefined || it === false || it === true) continue;
    if (Array.isArray(it)) { appendAll(parent, ...it); continue; }
    if (it instanceof Node) { parent.appendChild(it); continue; }
    parent.appendChild(document.createTextNode(String(it)));
  }
  return parent;
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

// ---------------- Toast ----------------

export function toast(message, type = 'info', timeout = 4200) {
  const box = document.getElementById('toasts');
  const icon = { ok: '✅', err: '⛔', warn: '⚠️', info: 'ℹ️' }[type] || 'ℹ️';
  const node = h(`div.toast.${type === 'info' ? '' : type}`, [
    h('span', { text: icon }),
    h('div.msg', { text: String(message) }),
    h('span.close', { text: '×', onclick: () => node.remove() }),
  ]);
  box.appendChild(node);
  if (timeout > 0) setTimeout(() => node.remove(), timeout);
  return node;
}

// ---------------- 弹窗 ----------------

/**
 * modal({title, body, footer, wide}) -> {close, el}
 * body 可以是 Node 或返回 Node 的函数。
 */
export function modal({ title, body, footer, wide = false, onClose } = {}) {
  const mask = h('div.modal-mask');
  const close = () => { mask.remove(); document.removeEventListener('keydown', onKey); if (onClose) onClose(); };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', onKey);
  mask.addEventListener('mousedown', (e) => { if (e.target === mask) close(); });

  const box = h(`div.modal${wide ? '.wide' : ''}`, [
    h('div.modal-head', [
      h('h3', { text: title || '' }),
      h('div.spacer'),
      h('button.modal-close', { text: '×', title: '关闭 (Esc)', onclick: close }),
    ]),
    h('div.modal-body', typeof body === 'function' ? body() : body),
    footer ? h('div.modal-foot', typeof footer === 'function' ? footer(close) : footer) : null,
  ]);
  mask.appendChild(box);
  document.body.appendChild(mask);
  const firstInput = box.querySelector('input, textarea, select');
  if (firstInput) setTimeout(() => firstInput.focus(), 40);
  return { close, el: box };
}

/** confirmBox(msg, {title, danger}) -> Promise<boolean> */
export function confirmBox(message, { title = '确认操作', danger = false, okText = '确定' } = {}) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (done) return; done = true; m.close(); resolve(v); };
    const m = modal({
      title,
      body: h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' }, text: message }),
      footer: [
        h('button.btn', { text: '取消', onclick: () => finish(false) }),
        h(`button.btn.${danger ? 'btn-danger' : 'btn-primary'}`, { text: okText, onclick: () => finish(true) }),
      ],
      onClose: () => finish(false),
    });
  });
}

/** promptBox({title, label, value, placeholder, type}) -> Promise<string|null> */
export function promptBox({ title, label, value = '', placeholder = '', type = 'text', hint = '' } = {}) {
  return new Promise((resolve) => {
    const input = h('input.input', { type, value, placeholder });
    let done = false;
    const finish = (v) => { if (done) return; done = true; m.close(); resolve(v); };
    const submit = () => finish(input.value.trim());
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
    const m = modal({
      title,
      body: h('div.field', [
        label ? h('label', { text: label }) : null,
        input,
        hint ? h('div.hint', { text: hint }) : null,
      ]),
      footer: [
        h('button.btn', { text: '取消', onclick: () => finish(null) }),
        h('button.btn.btn-primary', { text: '确定', onclick: submit }),
      ],
      onClose: () => finish(null),
    });
  });
}

// ---------------- 格式化 ----------------

export function bytes(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2) + ' ' + units[i];
}

export function rate(bps) {
  const v = Number(bps) || 0;
  if (v < 1024) return v.toFixed(0) + ' B/s';
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + ' KB/s';
  if (v < 1024 * 1024 * 1024) return (v / 1048576).toFixed(2) + ' MB/s';
  return (v / 1073741824).toFixed(2) + ' GB/s';
}

export function pct(v) { return (Number(v) || 0).toFixed(1) + '%'; }

export function duration(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  const d = Math.floor(sec / 86400);
  const hr = Math.floor((sec % 86400) / 3600);
  const mi = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d} 天 ${hr} 小时`;
  if (hr > 0) return `${hr} 小时 ${mi} 分`;
  if (mi > 0) return `${mi} 分 ${sec % 60} 秒`;
  return `${sec} 秒`;
}

export function levelOf(percent, warn = 75, danger = 90) {
  if (percent >= danger) return 'danger';
  if (percent >= warn) return 'warn';
  return '';
}

/** escapeHtml 用于必须用 innerHTML 的场景。 */
export function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

/** 把带 ANSI 色彩/进度的终端输出简化成纯文本。 */
export function stripAnsi(s) {
  return String(s ?? '').replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, '');
}

// ---------------- 迷你折线图 ----------------

/**
 * Sparkline 维护固定长度的数据点，用 SVG 画折线。
 * 纯手写是因为面板的图表需求极简单（一条曲线），不值得引入图表库。
 */
export class Sparkline {
  constructor({ max = 60, height = 46, color = 'var(--brand)', fill = true } = {}) {
    this.data = [];
    this.max = max;
    this.height = height;
    this.color = color;
    this.fill = fill;
    this.svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    this.svg.setAttribute('class', 'spark');
    this.svg.setAttribute('preserveAspectRatio', 'none');
    this.svg.setAttribute('viewBox', `0 0 100 ${height}`);
  }

  push(v) {
    this.data.push(Number(v) || 0);
    if (this.data.length > this.max) this.data.shift();
    this.render();
  }

  render() {
    const n = this.data.length;
    const H = this.height;
    if (n < 2) { this.svg.innerHTML = ''; return; }
    const peak = Math.max(100, ...this.data); // 百分比固定 0-100 基准，曲线更稳定
    const pts = this.data.map((v, i) => {
      const x = (i / (n - 1)) * 100;
      const y = H - (Math.min(v, peak) / peak) * (H - 4) - 2;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    });
    const line = `M${pts.join(' L')}`;
    const area = `${line} L100,${H} L0,${H} Z`;
    this.svg.innerHTML =
      (this.fill ? `<path d="${area}" fill="${this.color}" opacity="0.12"/>` : '') +
      `<path d="${line}" fill="none" stroke="${this.color}" stroke-width="1.6" ` +
      `stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/>`;
  }
}

/** 简单的表单读取：readForm(root) -> {name: value} */
export function readForm(root) {
  const out = {};
  root.querySelectorAll('[data-field]').forEach((el) => {
    const name = el.dataset.field;
    if (el.type === 'checkbox') out[name] = el.checked;
    else if (el.type === 'number') out[name] = el.value === '' ? null : Number(el.value);
    else out[name] = el.value;
  });
  return out;
}
