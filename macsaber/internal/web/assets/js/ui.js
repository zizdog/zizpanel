// ui.js —— 极小的 DOM 助手与提示条。不用框架，一个渲染器画所有工具。
export function el(tag, attrs = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k === 'html') node.innerHTML = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (k === 'value') node.value = v;
    else if (v === true) node.setAttribute(k, '');
    else node.setAttribute(k, String(v));
  }
  for (const c of [].concat(children)) {
    if (c === undefined || c === null || c === false) continue;
    node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

let toastTimer = 0;
export function toast(msg, ms = 2600) {
  const box = document.getElementById('toast');
  box.textContent = msg;
  box.classList.remove('hidden');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => box.classList.add('hidden'), ms);
}

export function bytes(n) {
  if (typeof n !== 'number' || !isFinite(n)) return '';
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let f = n;
  for (const u of units) {
    f /= 1024;
    if (f < 1024) return f.toFixed(1) + ' ' + u;
  }
  return f.toFixed(1) + ' PB';
}

export function human(value) {
  if (value === null || value === undefined) return '';
  if (typeof value === 'number' && value > 1024 * 1024) return bytes(value) + '（' + value + '）';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}
