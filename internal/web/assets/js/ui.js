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
//
// ⚠️ 它是 **DOM** 助手，不是万能 append。历史上有人拿它往 FormData 里塞字段：
//
//	appendAll(fd, 'dir', cwd)   // fd 是 FormData
//
// DOM 的 append() 可以只传一个参数，而 FormData.append() 至少要两个
// （name、value）—— 形状根本不同。旧实现在这里 `parent.appendChild(...)`，
// 于是抛 `parent.appendChild is not a function`；第一版"修好它"的实现改成
// `parent.append(it)`，又抛 `Failed to execute 'append' on 'FormData':
// 2 arguments required`。两次都抛在**任何 toast 之前**，而 async 事件处理器里
// 的 rejection 不会显示任何东西 —— 用户看到的就是那次报障：
// 「点上传没反应、没成功也没提示」。
//
// 所以这里的规矩是：**不认识的目标要大声报错**，并且上传路径由调用方
// 用 try/catch 把这个错误变成用户看得见的提示（见 files.js::uploadEntries）。
// 上传请直接用 `fd.append(name, value)` / `fd.append(name, blob, filename)`。
export function appendAll(parent, ...items) {
  if (parent && typeof parent.appendChild !== 'function') {
    if (typeof FormData !== 'undefined' && parent instanceof FormData) {
      throw new TypeError(
        'appendAll 用于 DOM，不能追加到 FormData（FormData.append 需要 name/value 两个参数）。' +
        '请改用 fd.append(name, value) 或 fd.append(name, blob, filename)。',
      );
    }
    // 其它"单参数 append"的容器（URLSearchParams 之类）可以照常追加
    if (typeof parent.append === 'function') {
      for (const it of items) {
        if (it === null || it === undefined || it === false || it === true) continue;
        if (Array.isArray(it)) { appendAll(parent, ...it); continue; }
        parent.append(it);
      }
      return parent;
    }
    throw new TypeError('appendAll 的目标既不是 DOM 节点，也没有 append() 方法');
  }
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
  // 反馈通道本身不能失败：所有写操作（启停/卸载/升级…）的失败提示都走这里。
  // 一旦它抛异常（例如 #toasts 不在 DOM 里），调用方 catch 里的那句提示就跟着
  // 一起没了 —— 用户看到的正是"点了没反应"（见 DEVELOPMENT.md 坑 154）。
  // 缺容器就现场补一个，保证提示一定看得见。
  let box = document.getElementById('toasts');
  if (!box) {
    box = h('div#toasts.toasts');
    (document.body || document.documentElement).appendChild(box);
  }
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
 * modal({title, body, footer, wide, onClose, onRequestClose, closeOnEsc, closeOnBackdrop,
 *        minimizable, statusText, onMinimize}) -> {close, el, minimize, setStatus, isMinimized}
 * body 可以是 Node 或返回 Node 的函数。
 *
 * 下面这组参数是**加法式**扩展，默认值等于旧行为，所以现有调用方（确认框、各页面的
 * 表单弹窗……）一个都不会变：Esc 关、点遮罩关、标题栏只有一颗 ×。只有显式传了
 * false / true 的调用方（目前只有文件编辑器 editorModal）才会看到新行为。
 *
 *   closeOnEsc      默认 true。false → 不监听 Esc（文件编辑器要求"只能点关闭按钮"）。
 *   closeOnBackdrop 默认 true。false → 点遮罩不关（同上）。
 *   onRequestClose  返回 false 时**拦下这次关闭**（关闭按钮、遮罩、Esc 都走这条路径）。
 *                   文件编辑器用它做"有未保存修改"的确认；旧调用方不传即无影响。
 *   minimizable     默认 false。true → 标题栏多一颗「—」，点它把弹窗收成右下角一条
 *                   标题栏（正文和页脚一起隐藏，DOM 不销毁，所以编辑内容/光标/滚动
 *                   位置都在）。再点一次（或点那条标题栏）还原。
 *   statusText      收起/展开都显示在标题栏上的状态提示（编辑器用"● 未保存"）。
 *   onMinimize      最小化状态变化时回调 (on:boolean)，调用方借此同步自己的视图。
 *
 * 为什么"只能关闭按钮关闭"和最小化只给文件编辑器：其它弹窗都是短表单，
 * 点遮罩就能放弃正好符合预期；而编辑器里可能有几分钟的编辑成果，误触遮罩/Esc
 * 就丢掉代价太大，所以它的关闭路径必须收敛到唯一的按钮上并经过未保存确认。
 */
export function modal({
  title, body, footer, wide = false, onClose,
  onRequestClose, closeOnEsc = true, closeOnBackdrop = true,
  minimizable = false, statusText = '', onMinimize,
} = {}) {
  const mask = h('div.modal-mask');
  const close = () => {
    mask.remove();
    document.removeEventListener('keydown', onKey);
    if (onClose) onClose();
  };
  const onKey = (e) => { if (e.key === 'Escape') requestClose(); };
  // requestClose 是"用户要求关闭"的唯一入口（关闭按钮、遮罩、Esc 都走它），
  // 这样未保存确认之类的拦截逻辑对三条路径一视同仁。
  const requestClose = () => {
    if (onRequestClose && onRequestClose() === false) return;
    close();
  };
  if (closeOnEsc) document.addEventListener('keydown', onKey);
  mask.addEventListener('mousedown', (e) => { if (closeOnBackdrop && e.target === mask) requestClose(); });

  const head = h('div.modal-head', [
    h('h3', { text: title || '', title: title || '' }),
    h('div.spacer'),
  ]);

  const bodyNode = h('div.modal-body', typeof body === 'function' ? body() : body);
  const footNode = footer ? h('div.modal-foot', typeof footer === 'function' ? footer(close) : footer) : null;

  const box = h(`div.modal${wide ? '.wide' : ''}`, [head, bodyNode, footNode]);
  mask.appendChild(box);

  let minimized = false;
  let minBtn = null;
  let statusNode = null;

  function syncMinButtons() {
    if (minBtn) {
      minBtn.textContent = minimized ? '▢' : '—';
      minBtn.title = minimized ? '还原窗口' : '最小化为标题栏（内容与光标保留）';
    }
    box.classList.toggle('zp-min', minimized);
    if (statusNode) statusNode.style.display = minimizable ? '' : 'none';
  }

  function minimize(on) {
    minimized = !!on;
    // 正文/页脚只是 display:none，DOM 一个节点都不删 —— 所以编辑内容、光标位置、
    // 滚动位置全都由浏览器继续保着，还原后接着写就行。
    //
    // 顺序很重要：必须先让正文重新可见，再通知调用方（onMinimize）。文件编辑器在
    // 回调里要 editor.focus() —— 对 display:none 的元素调 focus() 会被浏览器忽略，
    // 先通知后显示的话，还原后焦点就丢了。
    bodyNode.style.display = minimized ? 'none' : '';
    if (footNode) footNode.style.display = minimized ? 'none' : '';
    mask.classList.toggle('zp-min', minimized);
    syncMinButtons();
    if (onMinimize) onMinimize(minimized);
  }

  if (minimizable) {
    // 最小化状态必须有明显提示：收起后留下的就是这条标题栏（带标题 + 未保存状态），
    // 不会让人以为窗口已经关掉了。
    statusNode = h('span.zp-min-status', { text: statusText });
    minBtn = h('button.modal-close.zp-min-btn', {
      text: '—',
      title: '最小化为标题栏（内容与光标保留）',
      onclick: () => minimize(!minimized),
    });
    head.appendChild(statusNode);
    head.appendChild(minBtn);
    // 收起时整条标题栏都是"还原"热区（宝塔就是这个手感）。
    // 必须排除最小化按钮自己：点它已经切换过状态了，如果不排除，这次点击会继续冒泡
    // 到这里，把刚收起的窗口立刻又还原（表现为"最小化按钮点了没反应"）。
    head.addEventListener('click', (e) => {
      if (!minimized) return;
      if (minBtn && minBtn.contains(e.target)) return;
      minimize(false);
    });
  }

  head.appendChild(h('button.modal-close', {
    text: '×',
    title: closeOnEsc ? '关闭 (Esc)' : '关闭',
    onclick: requestClose,
  }));

  document.body.appendChild(mask);
  const firstInput = box.querySelector('input, textarea, select');
  if (firstInput) setTimeout(() => firstInput.focus(), 40);

  /** setStatus(text) —— 更新标题栏上的状态提示（文件编辑器用它显示"● 未保存"）。 */
  const setStatus = (text) => {
    if (!statusNode) return;
    statusNode.textContent = String(text || '');
    statusNode.classList.toggle('zpf-dirty', !!text);
  };

  return { close, el: box, minimize, setStatus, isMinimized: () => minimized };
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
