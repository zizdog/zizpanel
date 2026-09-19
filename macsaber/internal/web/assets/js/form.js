// form.js —— 通用参数渲染器：Go 下发的 params 决定表单长什么样。
// 新增工具只要给元数据，这里一行都不用改。
import { el } from './ui.js';

function numAttr(p) {
  const a = {};
  if (typeof p.min === 'number') a.min = p.min;
  if (typeof p.max === 'number') a.max = p.max;
  return a;
}

function field(p) {
  const id = 'p-' + p.name;
  let input;
  switch (p.type) {
    case 'textarea': {
      input = el('textarea', {
        id, name: p.name, placeholder: p.placeholder || '',
        class: p.multiline ? 'multi' : '',
      });
      break;
    }
    case 'select': {
      const opts = (p.options || []).map((o) =>
        el('option', { value: o.value, text: o.label || o.value }));
      input = el('select', { id, name: p.name }, opts);
      if (p.default !== undefined) input.value = String(p.default);
      break;
    }
    case 'bool': {
      const box = el('input', { id, name: p.name, type: 'checkbox' });
      if (p.default === true) box.checked = true;
      input = box;
      break;
    }
    case 'number': {
      input = el('input', Object.assign({
        id, name: p.name, type: 'number', placeholder: p.placeholder || '',
      }, numAttr(p)));
      if (p.default !== undefined) input.value = String(p.default);
      break;
    }
    case 'password': {
      input = el('input', { id, name: p.name, type: 'password', autocomplete: 'off', placeholder: p.placeholder || '' });
      break;
    }
    case 'path':
    case 'outpath': {
      input = el('input', {
        id, name: p.name, type: 'text', list: 'ms-paths',
        placeholder: p.placeholder || '/绝对/路径',
        autocomplete: 'off', spellcheck: 'false',
      });
      break;
    }
    default: {
      input = el('input', { id, name: p.name, type: 'text', placeholder: p.placeholder || '' });
      if (p.default !== undefined) input.value = String(p.default);
    }
  }
  const label = el('label', { class: 'fld', for: id }, [
    el('span', {}, [p.label + (p.required ? ' ' : ''), p.required ? el('em', { text: '*' }) : null]),
    input,
    p.help ? el('p', { class: 'hint', text: p.help }) : null,
  ]);
  if (p.type === 'bool') {
    return el('label', { class: 'check' }, [input, el('span', { text: p.label })]);
  }
  return label;
}

// ackInput 给危险确认框一个稳定的选择器（页面上可能有别的 checkbox 参数）。
function ackInput() {
  return el('input', { type: 'checkbox', class: 'check-ack' });
}

export function renderForm(tool, mount, onSubmit) {
  const form = el('form', { class: 'card', autocomplete: 'off' });
  form.appendChild(el('h3', { text: tool.name }));
  form.appendChild(el('p', { class: 'hint', text: tool.summary }));

  if (!tool.available) {
    form.appendChild(el('p', { class: 'note danger', text: '本机不可用：' + (tool.unavailable_reason || '未知原因') }));
  }
  if (tool.danger) {
    form.appendChild(el('p', { class: 'note danger', text: '危险操作：' + (tool.danger_note || '请确认后执行') }));
  }
  const probe = (tool.params || []).find((p) => p.probe);
  if (probe) form.appendChild(el('p', { class: 'note', text: '该工具需要一次真实探测，点执行后才给结论。' }));

  const inputs = {};
  for (const p of tool.params || []) {
    const f = field(p);
    form.appendChild(f);
    inputs[p.name] = f.querySelector('input, textarea, select');
  }

  let ack = null;
  if (tool.danger) {
    ack = ackInput();
    form.appendChild(el('label', { class: 'check' }, [ack,
      el('span', { text: '我已知晓风险，确认执行' })]));
  }

  const btn = el('button', { class: 'primary', type: 'submit', text: tool.async ? '执行（后台任务）' : '执行' });
  form.appendChild(btn);

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const body = {};
    for (const p of tool.params || []) {
      const node = inputs[p.name];
      if (!node) continue;
      if (p.type === 'bool') {
        body[p.name] = !!node.checked;
        continue;
      }
      const raw = node.value;
      if (p.type === 'number') {
        if (raw === '') continue;
        body[p.name] = Number(raw);
        continue;
      }
      if (raw === '') continue;
      body[p.name] = raw;
    }
    if (tool.danger) body._confirm = ack.checked ? tool.danger_floor : '';
    onSubmit(tool, body, { button: btn });
  });
  return mount.appendChild(form);
}
