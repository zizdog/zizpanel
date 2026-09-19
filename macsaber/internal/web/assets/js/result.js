// result.js —— 通用结果渲染器：把 Result{ok,msg,data,files} 画成卡片/表格/文件。
import { el, clear, human, bytes } from './ui.js';

const PREFERRED_ORDER = ['hardware', 'load', 'memory', 'disks', 'text', 'file', 'paths', 'unavailable'];

function rowsFromObject(obj) {
  const dl = el('dl', { class: 'kv' });
  for (const [k, v] of Object.entries(obj)) {
    dl.appendChild(el('dt', { text: k }));
    dl.appendChild(el('dd', { text: human(v) }));
  }
  return dl;
}

function rowsFromArray(arr) {
  if (arr.length === 0) return el('p', { class: 'hint', text: '（空）' });
  if (typeof arr[0] !== 'object') {
    return el('p', { class: 'kv', text: arr.map((x) => human(x)).join('、') });
  }
  const keys = [];
  for (const item of arr) for (const k of Object.keys(item)) if (!keys.includes(k)) keys.push(k);
  const table = el('table', { class: 'grid' });
  table.appendChild(el('tr', {}, keys.map((k) => el('th', { text: k }))));
  for (const item of arr) {
    table.appendChild(el('tr', {}, keys.map((k) => el('td', { text: human(item[k]) }))));
  }
  return table;
}

export function renderResult(mount, result) {
  clear(mount);
  const card = el('section', { class: 'card' });
  const ok = !!result.ok;
  card.appendChild(el('h3', { text: ok ? '结果' : '执行失败' }));
  card.appendChild(el('p', { class: ok ? 'msg ok' : 'msg err', text: result.msg || (ok ? '完成' : '未知错误') }));

  const data = result.data;
  if (data && typeof data === 'object' && !Array.isArray(data)) {
    const keys = Object.keys(data).sort((a, b) => {
      const ia = PREFERRED_ORDER.indexOf(a), ib = PREFERRED_ORDER.indexOf(b);
      return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
    });
    for (const k of keys) {
      const v = data[k];
      card.appendChild(el('h4', { text: k }));
      if (Array.isArray(v)) card.appendChild(rowsFromArray(v));
      else if (v && typeof v === 'object') card.appendChild(rowsFromObject(v));
      else card.appendChild(el('p', { text: human(v) }));
    }
  } else if (Array.isArray(data)) {
    card.appendChild(rowsFromArray(data));
  } else if (data !== undefined && data !== null) {
    card.appendChild(el('pre', { class: 'out', text: human(data) }));
  }

  for (const f of result.files || []) {
    const link = el('a', { href: f.download_url, text: '下载 ' + f.name + '（' + bytes(f.size) + '）' });
    card.appendChild(el('p', {}, [link]));
  }
  mount.appendChild(card);
}

export function renderTask(mount, task, { onCancel } = {}) {
  clear(mount);
  const card = el('section', { class: 'card' });
  card.appendChild(el('h3', { text: task.title || '后台任务' }));
  card.appendChild(el('p', { class: 'hint', text: '状态：' + task.status + ' · ' + (task.elapsed_ms / 1000).toFixed(1) + 's' }));

  const bar = el('div', { class: 'bar' }, [el('i')]);
  card.appendChild(bar);
  const percent = guessPercent(task);
  bar.firstChild.style.width = percent + '%';

  if (onCancel && task.status === 'running') {
    card.appendChild(el('button', { type: 'button', text: '取消任务', onclick: () => onCancel(task.id) }));
  }
  const pre = el('pre', { class: 'out', text: (task.logs || []).map((l) => l.text).join('\n') });
  card.appendChild(pre);

  if (task.status === 'failed') {
    card.appendChild(el('p', { class: 'msg err', text: '失败：' + (task.error || '未给出原因') }));
  }
  if (task.result) {
    const inner = el('div');
    renderResult(inner, task.result);
    card.appendChild(inner);
  }
  mount.appendChild(card);
  pre.scrollTop = pre.scrollHeight;
}

function guessPercent(task) {
  if (task.status === 'succeeded') return 100;
  const logs = task.logs || [];
  for (let i = logs.length - 1; i >= 0; i--) {
    const m = /\[(\d{1,3})%\]/.exec(logs[i].text || '');
    if (m) return Math.max(0, Math.min(100, Number(m[1])));
  }
  return task.status === 'running' ? 8 : 100;
}
