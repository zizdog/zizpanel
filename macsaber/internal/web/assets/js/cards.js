// cards.js —— 工具卡片：按分类列工具，元数据里没写死的都不猜。
import { el } from './ui.js';

export function renderCategories(mount, categories, activeId, onPick) {
  mount.innerHTML = '';
  for (const c of categories) {
    mount.appendChild(el('button', {
      type: 'button',
      class: c.id === activeId ? 'active' : '',
      text: c.title + '（' + c.tools.length + '）',
      onclick: () => onPick(c.id),
    }));
  }
}

export function renderToolCards(mount, category, onOpen) {
  mount.innerHTML = '';
  const grid = el('div', { class: 'toolgrid' });
  for (const t of category.tools) {
    const card = el('div', { class: 'tcard' });
    card.appendChild(el('h4', { text: t.name }));
    card.appendChild(el('p', { text: t.summary || '' }));
    const row = el('div', { class: 'row' });
    row.appendChild(el('button', { type: 'button', class: 'primary', text: '打开', onclick: () => onOpen(t) }));
    if (t.async) row.appendChild(el('span', { class: 'badge async', text: '后台任务' }));
    if (t.danger) row.appendChild(el('span', { class: 'badge danger', text: '危险' }));
    if (!t.available) row.appendChild(el('span', { class: 'badge no', text: '不可用' }));
    card.appendChild(row);
    if (!t.available) card.appendChild(el('p', { class: 'note danger', text: t.unavailable_reason || '' }));
    grid.appendChild(card);
  }
  mount.appendChild(grid);
}
