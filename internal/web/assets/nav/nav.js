// nav.js —— 独立别名页（浏览器首页）的渲染逻辑。只读。
//
// 页面地址：GET /nav/（由 internal/web/api_nav.go 从内嵌资源直接服务）
// 数据来源：GET /nav/data   —— 公开只读，只有名称/地址/图标/描述，无任何凭据
// 登录探测：GET /nav/whoami —— 未登录时**绝不回显面板安全后缀**，
//                             登录后才给出「回面板编辑」的链接。
//
// 安全：所有数据都通过 textContent / createElement 落到 DOM，绝不用 innerHTML
// 拼用户内容（站名、描述、图标地址都来自数据库，属于用户可控输入）。
//
// 这份文件必须自包含：它不能 import 面板的 js/ui.js —— 那些资源在安全后缀之后，
// 匿名访问者取不到（详情见 index.html 顶部注释）。

const THEME_KEY = 'zp-theme';
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');

function themePref() {
  try {
    const v = localStorage.getItem(THEME_KEY);
    return v === 'light' || v === 'dark' ? v : 'auto';
  } catch {
    return 'auto'; // 隐私模式/禁用存储：按系统主题
  }
}

function applyTheme() {
  const p = themePref();
  document.documentElement.dataset.theme = p === 'auto' ? (darkQuery.matches ? 'dark' : 'light') : p;
}

applyTheme();
darkQuery.addEventListener('change', () => { if (themePref() === 'auto') applyTheme(); });
// 同一 origin 下面板改了主题（localStorage 变化），这个页面也跟着变。
window.addEventListener('storage', (e) => { if (e.key === THEME_KEY) applyTheme(); });

const app = document.getElementById('app');
const search = document.getElementById('search');
const subtitle = document.getElementById('subtitle');

let groups = [];
let items = [];
let query = '';

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
};

function iconNode(it) {
  const icon = String(it.icon || '').trim();
  if (/^https?:\/\//i.test(icon)) {
    const img = el('img', 'icon');
    img.src = icon;
    img.alt = '';
    img.loading = 'lazy';
    // 图标裂了不显示破图：退回默认 emoji（不改数据，只影响展示）。
    img.addEventListener('error', () => {
      const span = el('span', 'icon', '🔗');
      img.replaceWith(span);
    });
    return img;
  }
  return el('span', 'icon', icon || '🔗');
}

function cardNode(it) {
  const a = el('a', 'card');
  a.href = String(it.url || '#');
  const openNew = !(it.open_new_tab === false || it.open_new_tab === 0);
  a.target = openNew ? '_blank' : '_self';
  a.rel = 'noopener';
  a.title = String(it.url || '') + (it.description ? '\n' + it.description : '');
  a.appendChild(iconNode(it));
  const meta = el('div', 'meta');
  meta.appendChild(el('div', 'name', it.name));
  if (it.description) meta.appendChild(el('div', 'desc', it.description));
  a.appendChild(meta);
  return a;
}

function visibleGroups() {
  const q = query.trim().toLowerCase();
  const byGroup = (gid) => items.filter((it) => Number(it.group_id) === Number(gid));
  if (!q) return groups.map((g) => ({ g, list: byGroup(g.id) }));
  return groups
    .map((g) => {
      const nameHit = String(g.name).toLowerCase().includes(q);
      const list = byGroup(g.id).filter((it) => nameHit
        || String(it.name).toLowerCase().includes(q)
        || String(it.description || '').toLowerCase().includes(q)
        || String(it.url).toLowerCase().includes(q));
      return { g, list };
    })
    .filter((x) => x.list.length > 0);
}

function render() {
  while (app.firstChild) app.removeChild(app.firstChild);

  if (groups.length === 0) {
    const box = el('div', 'empty');
    box.appendChild(el('div', 'big', '🧭'));
    box.appendChild(el('h3', null, '还没有导航内容'));
    box.appendChild(el('p', null, '请登录面板，在「导航页 → 编辑」里添加分组和常用站点。'));
    app.appendChild(box);
    return;
  }
  const shown = visibleGroups();
  if (shown.length === 0) {
    const box = el('div', 'empty');
    box.appendChild(el('div', 'big', '🔍'));
    box.appendChild(el('h3', null, '没有匹配的站点'));
    box.appendChild(el('p', null, '换个关键词，或清空搜索框。'));
    app.appendChild(box);
    return;
  }
  for (const { g, list } of shown) {
    const sec = el('section', 'group');
    const head = el('div', 'group-head');
    head.appendChild(el('h2', null, g.name));
    head.appendChild(el('span', 'count', String(list.length) + ' 个站点'));
    sec.appendChild(head);
    const grid = el('div', 'grid');
    list.forEach((it) => grid.appendChild(cardNode(it)));
    sec.appendChild(grid);
    app.appendChild(sec);
  }
}

search.addEventListener('input', (e) => { query = e.target.value; render(); });

async function load() {
  try {
    const res = await fetch('./data', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
    const body = await res.json().catch(() => null);
    if (!res.ok || !body || body.ok === false) {
      throw new Error((body && body.msg) || `HTTP ${res.status}`);
    }
    groups = (body.data && body.data.groups) || [];
    items = (body.data && body.data.items) || [];
  } catch (e) {
    groups = [];
    items = [];
    while (app.firstChild) app.removeChild(app.firstChild);
    const box = el('div', 'empty');
    box.appendChild(el('div', 'big', '⚠️'));
    box.appendChild(el('h3', null, '读取导航数据失败'));
    box.appendChild(el('p', null, e.message || String(e)));
    app.appendChild(box);
    return;
  }
  render();
}

// whoami：登录了才给「回面板编辑」的入口；未登录只显示一句说明，
// **不回显面板安全后缀**（这是别名页公开可访问时的硬要求）。
async function loadWhoami() {
  let data = null;
  try {
    const res = await fetch('./whoami', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
    const body = await res.json().catch(() => null);
    if (res.ok && body && body.ok !== false) data = body.data;
  } catch {
    return; // 探测失败就不显示编辑入口（宁可少一个链接，也不谎报"已登录"）
  }
  if (!data || !data.authenticated || !data.panel_entry) return;
  const slot = document.getElementById('edit-slot');
  if (!slot) return;
  const link = el('a', 'edit-link', '✎ 回面板编辑');
  link.href = String(data.panel_entry) + '#/nav';
  link.title = '在 ZizPanel 面板里编辑导航页';
  slot.appendChild(link);
  if (subtitle) subtitle.textContent = 'ZizPanel · 已登录，可编辑';
}

load();
loadWhoami();
