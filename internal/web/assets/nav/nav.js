// nav.js —— 独立别名页（浏览器首页）的渲染逻辑。只读。
//
// 页面地址：GET /nav/（由 internal/web/api_nav.go 从内嵌资源直接服务）
// 数据来源：GET /nav/data   —— 公开只读，只有名称/地址/图标/描述/外观设置，
//                              无任何凭据。`themes` 是 8 套色系的颜色表（服务端是唯一真相源）。
// 登录探测：GET /nav/whoami —— 未登录时**绝不回显面板安全后缀**，
//                             登录后才给出「回面板编辑」的链接。
//
// 安全：所有数据都通过 textContent / createElement 落到 DOM，绝不用 innerHTML
// 拼用户内容（站名、描述、图标地址都来自数据库，属于用户可控输入）。
//
// 这份文件必须自包含：它不能 import 面板的 js/ui.js —— 那些资源在安全后缀之后，
// 匿名访问者取不到（详情见 index.html 顶部注释）。
//
// 观感（2026-09-18 用户要求照抄参考首页）：
//   · 外观模式 auto/light/dark、8 套主题色系、卡片尺寸 s/m/l；
//   · 渐变背景双缓冲交叉淡入 + 两个漂移光晕；
//   · 毛玻璃卡片（样式在 index.html 的 <style> 里，这里只负责开关与变量）。
//
// 设置来源与优先级：
//   服务端 nav_settings（面板「🎨 外观」里保存的默认）
//     ↑ 被 localStorage 的本机覆盖（访客在右上角 ⚙ 里选的，只影响这一台浏览器）
//   「↺ 跟随面板默认」会清掉本机覆盖，回到服务端值。

const LS = { mode: 'sp.mode', theme: 'sp.theme', size: 'sp.size' };
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');
const root = document.documentElement;

// 本机覆盖（localStorage）。隐私模式/禁用存储时静默降级为空 —— 不因为读不到存储就白屏。
function localPref(key) {
  try { return localStorage.getItem(key) || ''; } catch { return ''; }
}
function setLocalPref(key, val) {
  try { if (val) localStorage.setItem(key, val); else localStorage.removeItem(key); } catch { /* 忽略 */ }
}

const DEFAULTS = { mode: 'auto', theme: 'neon', size: 'm' };

// S 是"当前生效"的外观：服务端默认 ← 本机覆盖。
const S = { mode: DEFAULTS.mode, theme: DEFAULTS.theme, size: DEFAULTS.size, accent: '' };
// serverSettings 保留服务端原值，供「跟随面板默认」回退使用。
let serverSettings = {};
let themes = [];
let themeByID = {};
let themesLoaded = false;

const app = document.getElementById('app');
const search = document.getElementById('search');
const subtitle = document.getElementById('subtitle');
const navTitle = document.getElementById('nav-title');
const userbg = document.getElementById('userbg');

let groups = [];
let items = [];
let query = '';

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
};

// ---------------- 外观：模式 / 色系 / 尺寸 ----------------

function isDark() {
  return S.mode === 'dark' || (S.mode === 'auto' && darkQuery.matches);
}

function normMode(v) { return ['auto', 'light', 'dark'].includes(v) ? v : DEFAULTS.mode; }
function normSize(v) { return ['s', 'm', 'l'].includes(v) ? v : DEFAULTS.size; }
function normTheme(v) { return themeByID[v] ? v : (themeByID[DEFAULTS.theme] ? DEFAULTS.theme : Object.keys(themeByID)[0] || ''); }

// syncFromServer 把服务端默认 + 本机覆盖算进 S。
function syncFromServer() {
  const ss = serverSettings || {};
  S.mode = normMode(localPref(LS.mode) || String(ss.mode || ''));
  S.theme = normTheme(localPref(LS.theme) || String(ss.theme || ''));
  S.size = normSize(localPref(LS.size) || String(ss.size || ''));
  S.accent = /^#[0-9a-f]{6}$/i.test(String(ss.accent || '')) ? String(ss.accent) : '';
}

// 双缓冲交叉淡入：新渐变写到"另一张" .bg 上，再淡出旧的，避免切换时闪白。
let flip = false;
function paintBG() {
  const t = themeByID[S.theme];
  const a = document.getElementById('bgA');
  const b = document.getElementById('bgB');
  if (!a || !b) return;
  if (!t) { // 没有色系表（旧服务端/读取失败）：退回一层中性渐变，仍可用。
    a.style.backgroundImage = 'linear-gradient(135deg,#1b1b2f,#2b2b45)';
    a.classList.add('on');
    b.classList.remove('on');
    return;
  }
  const c = isDark() ? t.dark : t.light;
  const next = flip ? a : b;
  const cur = flip ? b : a;
  next.style.backgroundImage = 'linear-gradient(135deg, ' + (c.grad || []).join(',') + ')';
  next.classList.add('on');
  cur.classList.remove('on');
  flip = !flip;

  root.style.setProperty('--orb1', (c.orb && c.orb[0]) || '#8b5cf6');
  root.style.setProperty('--orb2', (c.orb && c.orb[1]) || '#22d3ee');

  // accent 与 theme 的共存：accent（面板里手填的主题色）优先；留空则跟随色系自带的强调色。
  const accent = S.accent || c.accent || (c.orb && c.orb[0]) || '#8b5cf6';
  root.style.setProperty('--accent', accent);
  root.style.setProperty('--accent-soft', 'color-mix(in srgb, ' + accent + ' 16%, transparent)');
  root.style.setProperty('--brand', accent);
  root.style.setProperty('--nav-accent', accent);
}

// buildSwatches 渲染 8 张色卡（预览色随模式联动，与参考页一致）。
//
// 关键：**主题/模式没变时不重建 DOM**。重建会把"正在被点击的那张色卡"从文档里摘掉，
// 事件冒泡到 document 时 `panel.contains(e.target)` 就变成 false，面板会被误判成
// "点了外面"而关掉（实测：选完一个色系面板就自己关了）。
let swatchDark = null;
function buildSwatches(force) {
  const box = document.getElementById('themes');
  if (!box) return;
  const dark = isDark();
  const unchanged = !force && box.childElementCount === themes.length
    && box.childElementCount > 0 && swatchDark === dark;
  if (!unchanged) {
    box.replaceChildren();
    for (const t of themes) {
      const c = (dark ? t.dark : t.light) || {};
      const btn = el('button', 'sw');
      btn.type = 'button';
      btn.dataset.k = t.id;
      btn.title = t.name || t.id;
      const colorBox = el('span', 'box');
      colorBox.style.background = 'linear-gradient(135deg,' + (c.grad || []).join(',') + ')';
      btn.appendChild(colorBox);
      btn.appendChild(el('span', 'nm', t.name || t.id));
      box.appendChild(btn);
    }
    swatchDark = dark;
    if (themes.length === 0) {
      box.appendChild(el('div', 'nm', themesLoaded ? '色系读取失败，刷新重试' : '正在读取色系…'));
    }
  }
  // 选中态单独同步：不碰 DOM 结构，只切 class。
  box.querySelectorAll('.sw').forEach((b) => b.classList.toggle('on', b.dataset.k === S.theme));
}

function paintControls() {
  document.querySelectorAll('[data-group] button').forEach((b) => {
    const g = b.parentElement.dataset.group;
    b.classList.toggle('on', b.dataset.v === S[g]);
  });
  buildSwatches();
}

function applyAppearance() {
  root.setAttribute('data-theme', S.mode);
  root.setAttribute('data-size', S.size);
  paintBG();
  paintControls();
}

// ---------------- 服务端设置（标题 / 副标题 / 背景图） ----------------

function applySettings(st) {
  const s = st || {};
  serverSettings = s;
  const title = String(s.title || '').trim();
  if (title) {
    if (navTitle) navTitle.textContent = title;
    document.title = title + ' · ZizPanel';
  } else if (navTitle) {
    navTitle.textContent = '导航页';
    document.title = '导航页 · ZizPanel';
  }
  const sub = String(s.subtitle || '').trim();
  if (subtitle) subtitle.textContent = sub || 'ZizPanel · 自托管首页';

  // 背景图：铺满整页并压一层暗色遮罩，保证卡片文字可读。
  const bg = String(s.background || '').trim();
  if (bg) {
    const safe = bg.replace(/["'()\\\s]/g,
      (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase());
    userbg.style.backgroundImage =
      'linear-gradient(rgba(0,0,0,.35), rgba(0,0,0,.35)), url("' + safe + '")';
  } else {
    userbg.style.backgroundImage = '';
  }
  syncFromServer();
  applyAppearance();
}

// ---------------- 卡片渲染 ----------------

function iconNode(it) {
  const icon = String(it.icon || '').trim();
  // http(s) 直链，或**面板自己托管的本地图标**（/nav/icons/<内容哈希>.<扩展名>）：
  // 用户在面板的「编辑站点」里点「⬆ 上传本地图标」传上来的那张。
  // 这条路径是公开可读的（导航页本身匿名可访问），所以别名页也能显示。
  if (/^https?:\/\//i.test(icon)
    || /^\/nav\/icons\/[0-9a-f]{16}\.(png|jpg|jpeg|gif|webp|svg|ico)$/i.test(icon)) {
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
  a.dataset.k = (String(it.name || '') + ' ' + String(it.description || '') + ' ' + String(it.url || '')).toLowerCase();
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

function emptyBox(big, title, msg) {
  const box = el('div', 'empty');
  box.appendChild(el('div', 'big', big));
  box.appendChild(el('h3', null, title));
  box.appendChild(el('p', null, msg));
  return box;
}

function render() {
  app.replaceChildren();

  if (groups.length === 0) {
    app.appendChild(emptyBox('🧭', '还没有导航内容',
      '请登录面板，在「导航页 → 编辑」里添加分组和常用站点。'));
    return;
  }
  const shown = visibleGroups();
  if (shown.length === 0) {
    app.appendChild(emptyBox('🔍', '没有匹配的站点', '换个关键词，或清空搜索框。'));
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

// ---------------- 时钟 ----------------

function tick() {
  const n = new Date();
  const h = n.getHours();
  const hi = h < 6 ? '夜深了' : h < 11 ? '早上好' : h < 14 ? '中午好' : h < 18 ? '下午好' : '晚上好';
  const p = (x) => String(x).padStart(2, '0');
  const clock = document.getElementById('clock');
  if (clock) clock.textContent = `${hi} · ${p(h)}:${p(n.getMinutes())}:${p(n.getSeconds())}`;
}

// ---------------- 右上角 ⚙：本机外观覆盖 ----------------

function bindAppearanceControls() {
  const fab = document.getElementById('fab');
  const panel = document.getElementById('panel');
  if (fab && panel) {
    fab.addEventListener('click', () => panel.classList.toggle('open'));
    document.addEventListener('click', (e) => {
      // composedPath 在事件派发时就固定了：即使某个监听器把被点的节点从 DOM 里摘掉，
      // 这里仍能认出"这次点击来自面板内部"，不会误关。
      const path = e.composedPath ? e.composedPath() : [e.target];
      if (!path.includes(panel) && !path.includes(fab)) panel.classList.remove('open');
    });
    panel.addEventListener('click', (e) => {
      const sw = e.target.closest('.sw');
      if (sw) {
        S.theme = sw.dataset.k;
        setLocalPref(LS.theme, S.theme);
        applyAppearance();
        return;
      }
      const btn = e.target.closest('[data-group] button');
      if (!btn) return;
      const g = btn.parentElement.dataset.group;
      S[g] = btn.dataset.v;
      setLocalPref(LS[g], S[g]);
      applyAppearance();
    });
  }
  const reset = document.getElementById('reset');
  if (reset) {
    reset.addEventListener('click', () => {
      setLocalPref(LS.mode, '');
      setLocalPref(LS.theme, '');
      setLocalPref(LS.size, '');
      syncFromServer();
      applyAppearance();
    });
  }
}

// 系统主题变化时（自动模式）实时联动。
darkQuery.addEventListener('change', () => { if (S.mode === 'auto') applyAppearance(); });

// ---------------- 数据加载 ----------------

async function load() {
  try {
    const res = await fetch('./data', { credentials: 'same-origin', headers: { Accept: 'application/json' } });
    const body = await res.json().catch(() => null);
    if (!res.ok || !body || body.ok === false) {
      throw new Error((body && body.msg) || `HTTP ${res.status}`);
    }
    groups = (body.data && body.data.groups) || [];
    items = (body.data && body.data.items) || [];
    themes = (body.data && body.data.themes) || [];
    themeByID = {};
    for (const t of themes) if (t && t.id) themeByID[t.id] = t;
    themesLoaded = true;
    applySettings(body.data && body.data.settings);
  } catch (e) {
    groups = [];
    items = [];
    app.replaceChildren();
    app.appendChild(emptyBox('⚠️', '读取导航数据失败', e.message || String(e)));
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
  slot.replaceChildren(link);
}

// 先按默认外观渲染一次（避免数据到达前是一片没有背景的空白）。
syncFromServer();
applyAppearance();
bindAppearanceControls();
tick();
setInterval(tick, 1000);

load();
loadWhoami();
