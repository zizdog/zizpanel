// nav.js —— 「导航页」面板内页面：sun-panel 风格的图标网格首页。
//
// 为什么自研而不是部署现成的 sun-panel / home-dash（结论写在代码里备查）：
// 现成项目要么需要 Node/Vue 构建链（违背本项目「内嵌前端 + 无构建步骤」的路线），
// 要么自带一套运行时/数据库/鉴权/端口 —— 等于在面板之外再养一套要备份、要升级、
// 要单独记口令的系统。导航页的数据量极小（几个组、几十条链接），做进面板可以直接
// 复用面板已有的鉴权、风格、备份/恢复与审计，升级时只需替换一个二进制。
//
// 页面职责（数据在 /api/v1/nav/*，见 internal/web/api_nav.go）：
//   · 只读浏览：分组区段 + 图标卡片，点卡片新标签打开（target=_blank rel=noopener）；
//   · 顶部搜索：按名称/描述/URL 输入即筛（纯前端过滤，不发请求）；
//   · 编辑模式（登录后可开）：增删改分组与站点、按钮上下排序、导入/导出；
//   · 独立别名页 `/nav/`：给浏览器首页用，未登录也能看，见 assets/nav/。
//
// 交互刻意不做拖拽排序（按钮排序即可）、不做多用户/分享/天气/RSS —— 划界见任务说明。

import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { api } from './api.js';

// faviconOf 用目标站点自己的 /favicon.ico 当图标。
//
// 刻意**不接第三方 favicon CDN**（如 Google s2）：那会把"这台机器收藏了哪些站点"
// 泄露给第三方，而且离线环境下图标全裂。这里只是把图标字段填成目标站点的
// `/favicon.ico`，浏览器打开导航页时直接向该站点取 —— 不经过任何外部图标服务。
//
// TODO（如实标注）：没有做「面板服务端抓取 favicon 再缓存」的代理。
// 原因是那等于让服务端去请求任意用户填的 URL（SSRF：内网地址、重定向、
// 超大文件、私有存储），收益不抵风险。需要离线/稳定图标时，手工把图片地址
// 填进图标字段即可（外链图片同样只在浏览器侧加载）。
function faviconOf(rawURL) {
  try {
    const u = new URL(String(rawURL || '').trim());
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
    return u.origin + '/favicon.ico';
  } catch {
    return '';
  }
}

// uploadNavImage 让用户挑一张本地图片，上传成面板自己托管的图片。
//
// 用户 2026-09-18 要求："导航页要求可以上传本地图标！"以及"要可以自定义背景图！"
// 两者共用这套逻辑，只是接口/上限/格式不同：
//   · 图标 —— /api/v1/nav/icons，≤512 KiB，png/jpg/gif/webp/svg/ico；
//   · 背景图 —— /api/v1/nav/background，≤8 MiB，png/jpg/webp（不收 SVG）。
// 文件都落在面板数据目录的 nav-icons/ 里（文件名是内容哈希），
// 读取走公开的 /nav/icons/<name>（导航页是匿名首页，图片必须匿名可读）。
// 上传成功后把地址填进输入框 —— 用户点「保存」才真正生效。
async function uploadNavImage(input, btn, what, isBackground) {
  const picker = h('input', {
    type: 'file',
    accept: isBackground
      ? '.png,.jpg,.jpeg,.webp,image/png,image/jpeg,image/webp'
      : '.png,.jpg,.jpeg,.gif,.webp,.svg,.ico,image/png,image/jpeg,image/gif,image/webp,image/svg+xml,image/x-icon',
    style: { display: 'none' },
  });
  picker.onchange = async () => {
    const f = picker.files && picker.files[0];
    picker.value = '';
    if (!f) return;
    const oldText = btn.textContent;
    btn.disabled = true;
    btn.textContent = '上传中…';
    try {
      const info = isBackground ? await api.navBackgroundUpload(f) : await api.navIconUpload(f);
      input.value = (info && info.url) || '';
      toast(what + '已上传：' + f.name + '。点「保存」后生效', 'ok', 6000);
    } catch (e) {
      toast(what + '上传失败：' + e.message, 'err', 9000);
    } finally {
      btn.disabled = false;
      btn.textContent = oldText;
    }
  };
  // 追加到 body 再点：Safari 要求 input 在文档里才能触发选择框。
  document.body.appendChild(picker);
  picker.click();
  setTimeout(() => picker.remove(), 120000);
}

// pickUploadedIcon 打开「已上传的图标」选择器：**看着图挑**。
//
// 为什么必须有个看图的选择器（用户："也可以在已经上传的图标中选择！"）：
// 落盘文件名是内容哈希（防穿越、天然去重），人根本认不出来是哪张 ——
// 只给一个文件名列表等于让用户猜。
async function pickUploadedIcon(iconInput, what = '图标') {
  const grid = h('div.zp-icon-grid');
  const status = h('div.hint', { text: '正在读取已上传的图标…' });
  const m = modal({
    title: '从已上传的' + what + '中选择',
    wide: true,
    body: h('div', [status, grid]),
    footer: (close) => [h('button.btn', { text: '关闭', onclick: close })],
  });
  let icons = [];
  try {
    const data = await api.navIcons();
    icons = (data && data.icons) || [];
  } catch (e) {
    status.textContent = '读取已上传图标失败：' + e.message;
    return m;
  }
  if (icons.length === 0) {
    status.textContent = '还没有上传过' + what + ' —— 用上面的「⬆ 上传」按钮先传一张。';
    return m;
  }
  status.textContent = '共 ' + icons.length + ' 张：点一张就选中（🗑 删除该图，已引用的条目会显示默认图标）';
  for (const ic of icons) {
    const card = h('div.zp-icon-cell', [
      h('img', {
        src: ic.url,
        alt: '',
        title: ic.name + '（' + Math.round((ic.bytes || 0) / 1024) + ' KB）',
        loading: 'lazy',
        onclick: () => {
          iconInput.value = ic.url;
          m.close();
          toast('已选择' + what + '，点「保存」后生效', 'ok');
        },
      }),
      h('button.btn.btn-sm.btn-icon.btn-danger.zp-icon-del', {
        text: '🗑',
        title: '删除这张图标（磁盘上真的删掉）',
        onclick: async (ev) => {
          ev.stopPropagation();
          const yes = await confirmBox(
            '这张图标会从磁盘上真的删掉。已经引用它的站点会显示默认图标，需要重新选一张。',
            { title: '删除图标？', danger: true, okText: '删除' });
          if (!yes) return;
          try {
            await api.navIconDelete(ic.name);
            card.remove();
            toast('已删除该图标', 'ok');
          } catch (e) {
            toast('删除失败：' + e.message, 'err');
          }
        },
      }),
    ]);
    grid.appendChild(card);
  }
  return m;
}

// NAV_APPEARANCE_CSS —— 面板内导航页**只保留卡片尺寸**的样式。
//
// 用户要求：背景图 / 主题背景**只用于独立页 /nav/**，
// 面板内 #/nav 的观感必须与面板一致（不铺背景）。
// 所以原来那套"毛玻璃 + 渐变 + 漂移光晕"的运行时样式整体删掉：
// 面板内直接吃 app.css 里既有的 .zp-nav-* 规则 —— 那是面板自己的色板，
// 深浅主题跟随 panel 的 data-theme，视觉上就是普通面板页。
// 这里只留"卡片尺寸（小/中/大）"这一档与背景无关的密度设置。
const NAV_APPEARANCE_CSS = `
.zp-nav-shell[data-nav-size="s"] .zp-nav-grid{grid-template-columns:repeat(auto-fill,minmax(210px,1fr));gap:12px}
.zp-nav-shell[data-nav-size="s"] .zp-nav-card{padding:11px 14px;border-radius:14px;gap:11px;min-height:56px}
.zp-nav-shell[data-nav-size="s"] .zp-nav-icon{width:38px;height:38px;font-size:19px;border-radius:11px}
.zp-nav-shell[data-nav-size="s"] .zp-nav-name{font-size:.90em}
.zp-nav-shell[data-nav-size="s"] .zp-nav-desc{font-size:.70em}
.zp-nav-shell[data-nav-size="m"] .zp-nav-grid{grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:18px}
.zp-nav-shell[data-nav-size="m"] .zp-nav-card{padding:16px 18px;border-radius:18px;gap:14px;min-height:70px}
.zp-nav-shell[data-nav-size="m"] .zp-nav-icon{width:52px;height:52px;font-size:26px;border-radius:15px}
.zp-nav-shell[data-nav-size="m"] .zp-nav-name{font-size:1.02em}
.zp-nav-shell[data-nav-size="m"] .zp-nav-desc{font-size:.78em}
.zp-nav-shell[data-nav-size="l"] .zp-nav-grid{grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:24px}
.zp-nav-shell[data-nav-size="l"] .zp-nav-card{padding:22px 24px;border-radius:24px;gap:18px;min-height:86px}
.zp-nav-shell[data-nav-size="l"] .zp-nav-icon{width:68px;height:68px;font-size:34px;border-radius:20px}
.zp-nav-shell[data-nav-size="l"] .zp-nav-name{font-size:1.20em}
.zp-nav-shell[data-nav-size="l"] .zp-nav-desc{font-size:.86em}
`;

// ensureNavAppearanceStyle 把上面的样式注入 <head>，只注入一次（切页重进不叠加）。
function ensureNavAppearanceStyle() {
  if (document.getElementById('zp-nav-appearance-css')) return;
  const st = document.createElement('style');
  st.id = 'zp-nav-appearance-css';
  st.textContent = NAV_APPEARANCE_CSS;
  document.head.appendChild(st);
}

// 外观三件套的白名单（与服务端同一条判据；服务端是权威，这里只是渲染兜底）。
const NAV_MODES = ['auto', 'light', 'dark'];
const NAV_SIZES = ['s', 'm', 'l'];

export function NavView(content, ctx = {}) {
  clear(content);
  ensureNavAppearanceStyle();

  let groups = [];
  let items = [];
  let edit = false;
  let query = '';
  let loaded = false;
  // 外观设置（标题 / 副标题 / 主题色 / 背景图 / 模式 / 色系 / 尺寸），随 navTree 一起返回，
  // 面板内页面与独立别名页共用同一份（存 nav_settings 表）。
  let settings = {};
  // 8 套色系表：由服务端下发（navTree.themes），前端不硬编码颜色。
  let themes = [];
  // draft 是「🎨 外观」弹窗里未保存的即时预览（只含 mode/theme/size），关掉弹窗就丢弃。
  let draft = null;

  const toolbar = h('div.zp-nav-toolbar');
  const body = h('div', { id: 'zp-nav-body' });
  // 面板内**不再有背景层/光晕层**（用户要求：不铺背景，观感与面板一致）。
  // 背景图与色系渐变只在独立页 /nav/（assets/nav/）里渲染。
  const shell = h('div.zp-nav-shell', { dataset: { testid: 'nav-shell' } }, [toolbar, body]);
  content.append(shell);

  const idOf = (x) => Number(x.id);
  const itemsOf = (gid) => items.filter((it) => Number(it.group_id) === Number(gid));

  // ---------------- 数据 ----------------

  async function load() {
    try {
      const tree = await api.navTree();
      groups = (tree && tree.groups) || [];
      items = (tree && tree.items) || [];
      settings = (tree && tree.settings) || {};
      themes = (tree && tree.themes) || [];
      loaded = true;
      applyAppearance();
      renderBody();
    } catch (e) {
      loaded = true;
      clear(body);
      body.appendChild(h('div.card', h('div.card-body', h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取导航数据失败' }),
        h('p', { text: e.message || String(e) }),
        h('button.btn.btn-sm', { text: '重试', style: { marginTop: '10px' }, onclick: load }),
      ]))));
    }
  }

  // 每次写操作后整棵重拉：数据量极小（几十条），换来的是「界面 == 数据库」，
  // 不会出现本地乐观更新算错顺序、刷新后又变回去的问题。
  async function reload(msg) {
    await load();
    if (msg) toast(msg, 'ok');
  }

  // ---------------- 搜索过滤 ----------------

  // filtering 表示搜索框非空。此时列表是「过滤后的子集」，
  // 上下移动按钮会按错误的下标算顺序 —— 所以搜索时禁用排序（按钮 title 说明原因）。
  const filtering = () => query.trim() !== '';

  function visibleGroups() {
    const q = query.trim().toLowerCase();
    if (!q) return groups.map((g) => ({ g, list: itemsOf(idOf(g)) }));
    return groups
      .map((g) => {
        const nameHit = String(g.name).toLowerCase().includes(q);
        const list = itemsOf(idOf(g)).filter((it) => nameHit
          || String(it.name).toLowerCase().includes(q)
          || String(it.description || '').toLowerCase().includes(q)
          || String(it.url).toLowerCase().includes(q));
        return { g, list };
      })
      .filter((x) => x.list.length > 0);
  }

  // ---------------- 外观（标题 / 主题色 / 背景图）----------------
  //
  // 用户 2026-09-18 要求：「标题要可以改！要可以自定义背景图！要可以指定主题色！」
  // 设置存在服务端（nav_settings 表），面板内页面与独立别名页共用同一份 ——
  // 所以在面板里改完，访客打开的 /nav/ 也会是同一套外观。

  // safeAccent 只认 #rrggbb（与服务端同一条判据）：拼进 CSS 之前**再校验一次**，
  // 免得坏数据（手工改库、旧版本残留）把样式注入点打开。
  const safeAccentOf = (raw) => (/^#[0-9a-f]{6}$/i.test(String(raw || '')) ? String(raw) : '');
  const safeAccent = () => safeAccentOf(settings.accent);

  // effSettings：服务端已保存的设置 ← 弹窗里未保存的即时预览（draft）。
  function effSettings() {
    const base = settings || {};
    const d = draft || {};
    const mode = NAV_MODES.includes(String(d.mode || base.mode)) ? String(d.mode || base.mode) : 'auto';
    const size = NAV_SIZES.includes(String(d.size || base.size)) ? String(d.size || base.size) : 'm';
    let theme = String(d.theme || base.theme || '');
    if (!themes.some((t) => t.id === theme)) theme = themes.length ? themes[0].id : '';
    return { ...base, mode, size, theme };
  }

  const modeIsDark = (mode) => mode === 'dark'
    || (mode === 'auto' && window.matchMedia('(prefers-color-scheme: dark)').matches);

  // applyAppearance 把外观应用到导航页容器（标题/副标题在 renderToolbar 里走 DOM）。
  //
  // 2026-09-19：面板内**只应用卡片尺寸与自定义主题色** —— 背景图 / 色系渐变 /
  // 漂移光晕 / 外观模式都**只作用于独立页 /nav/**（用户明确要求面板内不铺背景、
  // 观感与面板一致）。设置本身仍然保存到服务端，独立页读的是同一份。
  // 因此这里不再设置 data-nav-mode，也不再画任何背景图层。
  function applyAppearance() {
    const s = effSettings();
    shell.setAttribute('data-nav-size', s.size);
    // 自定义主题色只影响卡片悬停边框（app.css 的 --nav-accent）；
    // 留空就删掉，回落到**面板品牌色**，而不是某个色系自带的颜色。
    const accent = safeAccentOf(s.accent);
    if (accent) content.style.setProperty('--nav-accent', accent);
    else content.style.removeProperty('--nav-accent');
  }

  // appearanceForm 是「🎨 外观」弹窗：外观模式 / 主题色系 / 卡片尺寸 / 标题 / 副标题 / 主题色 / 背景图。
  //
  // 前四项（模式/色系/尺寸）改的是**导航页自己的观感**（服务端默认，别名页同样生效）；
  // 弹窗里的选择即时预览，点「保存外观」才写库。
  function appearanceForm() {
    const title = h('input.input', {
      type: 'text', value: settings.title || '', placeholder: '导航页（默认）', maxlength: '40',
    });
    const subtitle = h('input.input', {
      type: 'text', value: settings.subtitle || '', placeholder: 'ZizPanel · 自托管首页（默认）', maxlength: '60',
    });
    const accent = h('input.input', {
      type: 'text', value: settings.accent || '', placeholder: '留空 = 跟随色系', maxlength: '7',
    });
    const colorPick = h('input', { type: 'color', value: safeAccent() || '#8b5cf6', title: '用取色器选一个主题色' });
    colorPick.oninput = () => { accent.value = colorPick.value; };
    const presets = ['#3b82f6', '#22c55e', '#f59e0b', '#ef4444', '#a855f7', '#06b6d4'];
    const swatches = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center' } },
      presets.map((c) => h('button.btn.btn-sm', {
        text: '●', title: '用 ' + c, style: { color: c, fontWeight: '700' },
        onclick: () => { accent.value = c; colorPick.value = c; },
      })).concat([
        h('button.btn.btn-sm', { text: '跟随色系', title: '清空自定义主题色，改用所选色系自带的强调色', onclick: () => { accent.value = ''; } }),
      ]));

    // ---- 外观模式 ----
    const modeWrap = h('div', { style: { display: 'flex', gap: '6px' } });
    NAV_MODES.forEach((v) => {
      const label = { auto: '自动', light: '亮色', dark: '暗色' }[v];
      const b = h('button.btn.btn-sm', { text: label, type: 'button', dataset: { testid: 'nav-mode-' + v } });
      b.onclick = () => { draft = { ...(draft || {}), mode: v }; applyAppearance(); sync(); };
      modeWrap.appendChild(b);
    });

    // ---- 卡片尺寸 ----
    const sizeWrap = h('div', { style: { display: 'flex', gap: '6px' } });
    NAV_SIZES.forEach((v) => {
      const label = { s: '小', m: '中', l: '大' }[v];
      const b = h('button.btn.btn-sm', { text: label, type: 'button', dataset: { testid: 'nav-size-' + v } });
      b.onclick = () => { draft = { ...(draft || {}), size: v }; applyAppearance(); sync(); };
      sizeWrap.appendChild(b);
    });

    // ---- 主题色系（8 套，预览色随模式联动）----
    const themeGrid = h('div', { style: { display: 'grid', gridTemplateColumns: 'repeat(4,1fr)', gap: '9px' } });
    function renderThemes() {
      clear(themeGrid);
      if (themes.length === 0) {
        themeGrid.appendChild(h('div.hint', { text: '色系表未加载 —— 关闭弹窗、刷新页面后重试' }));
        return;
      }
      const dark = modeIsDark(effSettings().mode);
      const cur = effSettings().theme;
      themes.forEach((t) => {
        const c = (dark ? t.dark : t.light) || {};
        const on = cur === t.id;
        const b = h('button', {
          type: 'button', title: t.name, dataset: { testid: 'nav-theme-' + t.id },
          style: {
            cursor: 'pointer', background: 'none', padding: '3px', borderRadius: '12px',
            border: on ? '2px solid var(--text)' : '2px solid transparent',
          },
        });
        b.appendChild(h('span', {
          style: {
            display: 'block', height: '34px', borderRadius: '9px',
            boxShadow: 'inset 0 0 0 1px rgba(255,255,255,.18)',
            background: 'linear-gradient(135deg,' + (c.grad || []).join(',') + ')',
          },
        }));
        b.appendChild(h('span', {
          style: { display: 'block', textAlign: 'center', fontSize: '11px', marginTop: '5px', color: 'var(--text-dim)' },
          text: t.name,
        }));
        b.onclick = () => { draft = { ...(draft || {}), theme: t.id }; applyAppearance(); sync(); };
        themeGrid.appendChild(b);
      });
    }

    function sync() {
      const ef = effSettings();
      NAV_MODES.forEach((v, i) => modeWrap.children[i].classList.toggle('btn-primary', v === ef.mode));
      NAV_SIZES.forEach((v, i) => sizeWrap.children[i].classList.toggle('btn-primary', v === ef.size));
      renderThemes();
    }
    sync();

    const bgInput = h('input', { type: 'hidden', value: settings.background || '' });
    const bgPreview = h('img', {
      alt: '', style: { maxWidth: '100%', maxHeight: '150px', borderRadius: '8px', display: 'none', marginTop: '8px' },
    });
    const refreshBg = () => {
      const v = String(bgInput.value || '').trim();
      if (v) { bgPreview.src = v; bgPreview.style.display = 'block'; } else { bgPreview.style.display = 'none'; }
    };
    refreshBg();
    const bgUp = h('button.btn.btn-sm', { text: '⬆ 上传背景图', title: 'png / jpg / webp，≤8 MiB' });
    bgUp.onclick = async () => { await uploadNavImage(bgInput, bgUp, '背景图', true); refreshBg(); };
    const bgPick = h('button.btn.btn-sm', { text: '🖼 已上传', title: '从已经上传过的图片里挑一张' });
    bgPick.onclick = () => pickUploadedIcon(bgInput, '背景图');
    const bgClear = h('button.btn.btn-sm', { text: '清除背景', title: '恢复无色系渐变（改用当前色系背景）', onclick: () => { bgInput.value = ''; refreshBg(); } });

    const okBtn = h('button.btn.btn-primary', { text: '保存外观' });
    const m = modal({
      title: '导航页外观',
      wide: true,
      // 关掉弹窗（含取消/遮罩/Esc）就丢弃未保存的即时预览，恢复服务端值。
      onClose: () => { draft = null; applyAppearance(); },
      body: h('div', [
        h('div.hint', {
          style: { marginBottom: '12px' },
          html: '<b>面板内导航页</b>固定跟随面板观感、<b>不铺背景</b>；下面的外观模式 / 主题色系 / 背景图'
            + '<b>只作用于独立页 <code class="code">/nav/</code></b>（点工具栏「↗ 独立页」查看效果）。'
            + '卡片尺寸与自定义主题色在面板内同样生效。',
        }),
        h('div.field', [h('label', { text: '外观模式（仅独立页）' }), modeWrap,
          h('div.hint', { text: '自动 = 跟随操作系统；亮色 / 暗色为固定外观。只作用于独立导航页 /nav/。' })]),
        h('div.field', [h('label', { text: '主题色系（仅独立页）' }), themeGrid,
          h('div.hint', { text: '8 套色系决定独立页的背景渐变与两个光晕的颜色；每套都含亮/暗两版。' })]),
        h('div.field', [h('label', { text: '卡片尺寸（面板内 + 独立页）' }), sizeWrap,
          h('div.hint', { text: '小 / 中 / 大三档，改变卡片密度与图标大小。两处都生效。' })]),
        h('div.field', [h('label', { text: '标题' }), title,
          h('div.hint', { text: '显示在导航页左上角。留空 = 「导航页」。' })]),
        h('div.field', [h('label', { text: '副标题' }), subtitle,
          h('div.hint', { text: '标题下面那行小字。留空 = 默认。' })]),
        h('div.field', [h('label', { text: '主题色（面板内 + 独立页）' }),
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } },
            [accent, colorPick, swatches]),
          h('div.hint', {
            text: '自定义强调色（卡片悬停边框、搜索框聚焦…）。面板内以它为准；留空 = 跟随面板品牌色'
              + '（独立页留空 = 跟随所选色系）。不影响面板其它页面的配色。',
          })]),
        h('div.field', [h('label', { text: '背景图（仅独立页）' }),
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [bgUp, bgPick, bgClear]),
          h('div.hint', { text: 'png / jpg / webp，≤8 MiB。只铺在独立导航页 /nav/ 上（cover + 暗色遮罩保证文字可读）；面板内不显示背景图。' }),
          bgPreview]),
      ]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      okBtn.disabled = true;
      try {
        const ef = effSettings();
        const saved = await api.navSaveSettings({
          title: title.value.trim(),
          subtitle: subtitle.value.trim(),
          accent: accent.value.trim(),
          background: bgInput.value.trim(),
          mode: ef.mode,
          theme: ef.theme,
          size: ef.size,
        });
        draft = null;
        if (saved) settings = saved;
        applyAppearance();
        renderToolbar();
        m.close();
        toast('外观已保存 —— 独立导航页刷新后同样生效', 'ok', 5000);
      } catch (e) {
        toast('保存外观失败：' + e.message, 'err', 9000);
        okBtn.disabled = false;
      }
    };
  }

  // ---------------- 工具栏 ----------------

  function renderToolbar() {
    clear(toolbar);
    const search = h('input.input.zp-nav-search', {
      type: 'search',
      placeholder: '搜索名称 / 描述 / 网址…',
      value: query,
      dataset: { testid: 'nav-search' },
      oninput: (e) => { query = e.target.value; renderBody(); },
    });
    appendAll(toolbar,
      h('b.zp-nav-title', { text: String(settings.title || '').trim() || '导航页' }),
      String(settings.subtitle || '').trim()
        ? h('span.zp-nav-subtitle', { text: String(settings.subtitle).trim() }) : null,
      search,
      h('div.spacer'),
      h('a.btn.btn-sm', {
        href: api.navStandaloneURL(), target: '_blank', rel: 'noopener',
        title: '打开可在浏览器里当首页的独立导航页（未登录也能看）',
        text: '↗ 独立页',
      }),
      edit ? h('button.btn.btn-sm', {
        text: '🎨 外观', dataset: { testid: 'nav-appearance' },
        title: '改导航页外观：外观模式 / 主题色系 / 背景图只作用于独立页 /nav/；卡片尺寸与主题色两处都生效',
        onclick: appearanceForm,
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '＋ 站点', dataset: { testid: 'nav-add-item' },
        onclick: () => itemForm(null, groups.length ? idOf(groups[0]) : 0),
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '＋ 分组', dataset: { testid: 'nav-add-group' }, onclick: () => groupForm(null),
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '⬆ 导入', dataset: { testid: 'nav-import' }, onclick: importDialog,
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '⬇ 导出', dataset: { testid: 'nav-export' }, onclick: exportFile,
      }) : null,
      h(`button.btn.btn-sm${edit ? '.btn-primary' : ''}`, {
        text: edit ? '✓ 完成' : '✎ 编辑',
        dataset: { testid: 'nav-edit-toggle' },
        title: edit ? '退出编辑模式' : '进入编辑模式（增删改分组与站点、排序、导入导出）',
        onclick: () => { edit = !edit; renderToolbar(); renderBody(); },
      }),
    );
  }

  // ---------------- 卡片 ----------------

  function iconNode(it) {
    const icon = String(it.icon || '').trim();
    // http(s) 直链，或**面板自己托管的本地图标**（/nav/icons/<内容哈希>.<扩展名>，
    // 就是用户在编辑弹窗里点「⬆ 上传本地图标」传上来的那张）。两者都用 <img> 渲染。
    if (/^https?:\/\//i.test(icon)
      || /^\/nav\/icons\/[0-9a-f]{16}\.(png|jpg|jpeg|gif|webp|svg|ico)$/i.test(icon)) {
      return h('img.zp-nav-icon', { src: icon, alt: '', loading: 'lazy' });
    }
    return h('span.zp-nav-icon.zp-nav-emoji', { text: icon || '🔗' });
  }

  // cardNode 返回「链接 + 编辑操作」两层结构。
  //
  // 为什么操作按钮不放进 <a> 里：嵌套可点击元素既是无效 HTML，也会让点删除时
  // 连带触发导航。卡片本体是真正的 <a>（target=_blank rel=noopener），
  // 编辑操作绝对定位在右上角、是它的兄弟节点。
  function cardNode(it, gid, idx) {
    const list = itemsOf(gid);
    const openNew = !(it.open_new_tab === false || it.open_new_tab === 0);
    const link = h('a.zp-nav-card', {
      href: it.url,
      target: openNew ? '_blank' : '_self',
      rel: 'noopener',
      title: it.url + (it.description ? '\n' + it.description : ''),
      dataset: { testid: 'nav-card', url: it.url, name: it.name },
    }, [
      h('div.zp-nav-card-top', [iconNode(it)]),
      h('div.zp-nav-card-main', [
        h('div.zp-nav-name', { text: it.name }),
        it.description ? h('div.zp-nav-desc', { text: it.description }) : null,
      ]),
    ]);
    if (!edit) return link;
    const ops = h('div.zp-nav-card-ops', [
      h('button.btn.btn-sm.btn-icon', {
        text: '✎', title: '编辑这个站点', dataset: { testid: 'nav-item-edit' },
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); itemForm(it); },
      }),
      h('button.btn.btn-sm.btn-icon', {
        text: '↑', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '上移',
        disabled: filtering() || idx === 0,
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); moveItem(gid, idx, -1); },
      }),
      h('button.btn.btn-sm.btn-icon', {
        text: '↓', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '下移',
        disabled: filtering() || idx === list.length - 1,
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); moveItem(gid, idx, 1); },
      }),
      h('button.btn.btn-sm.btn-icon.btn-danger', {
        text: '🗑', title: '删除这个站点', dataset: { testid: 'nav-item-delete' },
        onclick: async (e) => {
          e.preventDefault(); e.stopPropagation();
          if (!await confirmBox(`确定删除站点「${it.name}」？（不可撤销）`,
            { title: '删除站点', danger: true, okText: '删除' })) return;
          try {
            await api.navItemDelete(idOf(it));
            await reload('站点已删除');
          } catch (err) { toast(err.message, 'err'); }
        },
      }),
    ]);
    return h('div.zp-nav-card-wrap', [link, ops]);
  }

  // ---------------- 主体 ----------------

  function renderBody() {
    clear(body);
    if (!loaded) {
      body.appendChild(h('div.card', h('div.card-body', [
        h('div.skeleton'), h('div.skeleton'), h('div.skeleton'),
      ])));
      return;
    }
    if (groups.length === 0) {
      body.appendChild(emptyState());
      return;
    }
    const shown = visibleGroups();
    if (shown.length === 0) {
      body.appendChild(h('div.empty', [
        h('div.big', { text: '🔍' }),
        h('h4', { text: '没有匹配的站点' }),
        h('p', { text: '换个关键词，或清空搜索框。' }),
      ]));
      return;
    }
    shown.forEach(({ g, list }, gi) => body.appendChild(groupSection(g, list, gi, shown.length)));
  }

  function emptyState() {
    return h('div.card', h('div.card-body', h('div.empty', [
      h('div.big', { text: '🧭' }),
      h('h4', { text: '还没有导航内容' }),
      h('p', { text: '先加一个组，再加几个常用站点，这里就会变成你的浏览器首页。' }),
      h('div', { style: { marginTop: '14px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
        h('button.btn.btn-primary.btn-sm', {
          text: '＋ 新建第一个分组', dataset: { testid: 'nav-empty-add-group' },
          onclick: () => groupForm(null),
        }),
        edit ? null : h('button.btn.btn-sm', {
          text: '✎ 进入编辑模式',
          onclick: () => { edit = true; renderToolbar(); renderBody(); },
        }),
      ]),
    ])));
  }

  function groupSection(g, list, gi, total) {
    const gid = idOf(g);
    const head = h('div.zp-nav-group-head', [
      h('h3', { text: g.name }),
      h('span.pill', { text: String(list.length) + ' 个站点' }),
      h('div.spacer'),
      edit ? h('div.zp-nav-group-ops', [
        h('button.btn.btn-sm', {
          text: '＋ 站点', title: '在这个分组里加站点', dataset: { testid: 'nav-group-add-item' },
          onclick: () => itemForm(null, gid),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '✎', title: '重命名分组', dataset: { testid: 'nav-group-edit' },
          onclick: () => groupForm(g),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '↑', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '分组上移',
          disabled: filtering() || gi === 0, onclick: () => moveGroup(gi, -1),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '↓', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '分组下移',
          disabled: filtering() || gi === total - 1, onclick: () => moveGroup(gi, 1),
        }),
        h('button.btn.btn-sm.btn-icon.btn-danger', {
          text: '🗑', title: '删除分组（连同其中的站点）', dataset: { testid: 'nav-group-delete' },
          onclick: async () => {
            if (!await confirmBox(`确定删除分组「${g.name}」？其中的 ${list.length} 个站点会一起删除，且不可撤销。`,
              { title: '删除分组', danger: true, okText: '删除' })) return;
            try {
              await api.navGroupDelete(gid);
              await reload('分组已删除');
            } catch (e) { toast(e.message, 'err'); }
          },
        }),
      ]) : null,
    ]);
    const grid = h('div.zp-nav-grid');
    if (list.length === 0) {
      grid.appendChild(h('div.zp-nav-empty-inline', {
        text: edit ? '这个分组还是空的，点上方「＋ 站点」加一个。' : '这个分组还没有站点。',
      }));
    } else {
      list.forEach((it, idx) => grid.appendChild(cardNode(it, gid, idx)));
    }
    return h('section.card.zp-nav-group', { dataset: { testid: 'nav-group', name: g.name } }, [
      h('div.card-head', [head]),
      h('div.card-body', [grid]),
    ]);
  }

  // ---------------- 排序（按钮版，不做拖拽） ----------------

  async function moveGroup(gi, delta) {
    const order = groups.map(idOf);
    const target = gi + delta;
    if (target < 0 || target >= order.length) return;
    [order[gi], order[target]] = [order[target], order[gi]];
    try {
      await api.navGroupReorder(order);
      await load();
    } catch (e) { toast(e.message, 'err'); }
  }

  async function moveItem(gid, idx, delta) {
    const order = itemsOf(gid).map(idOf);
    const target = idx + delta;
    if (target < 0 || target >= order.length) return;
    [order[idx], order[target]] = [order[target], order[idx]];
    try {
      await api.navItemReorder(order);
      await load();
    } catch (e) { toast(e.message, 'err'); }
  }

  // ---------------- 表单 ----------------

  function groupForm(g) {
    const isEdit = !!g;
    const name = h('input.input', {
      type: 'text', value: g ? g.name : '',
      placeholder: '例如：常用工具 / 影音 / 自建服务',
    });
    const okBtn = h('button.btn.btn-primary', { text: isEdit ? '保存' : '创建' });
    const m = modal({
      title: isEdit ? '重命名分组' : '新建分组',
      body: h('div.field', [h('label', { text: '分组名称' }), name]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      const v = name.value.trim();
      if (!v) { toast('分组名称不能为空', 'warn'); return; }
      okBtn.disabled = true;
      try {
        if (isEdit) await api.navGroupUpdate(idOf(g), { name: v });
        else await api.navGroupCreate({ name: v });
        m.close();
        await reload(isEdit ? '分组已重命名' : '分组已创建');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') okBtn.click(); });
  }

  function itemForm(it, presetGid) {
    const isEdit = !!it;
    if (groups.length === 0) { toast('请先创建一个分组', 'warn'); return; }
    const gid = it ? Number(it.group_id) : Number(presetGid || idOf(groups[0]));

    const name = h('input.input', { type: 'text', value: it ? it.name : '', placeholder: '例如：ZizPanel 面板' });
    const url = h('input.input', { type: 'text', value: it ? it.url : '', placeholder: 'https://example.com' });
    const icon = h('input.input', { type: 'text', value: it ? (it.icon || '') : '', placeholder: '一个 emoji（🧭）或图片地址，可留空' });
    const desc = h('input.input', { type: 'text', value: it ? (it.description || '') : '', placeholder: '一句话说明（可留空）' });
    const groupSel = h('select.select', groups.map((g) => h('option', {
      value: String(idOf(g)), text: g.name, selected: idOf(g) === gid,
    })));
    const openNew = h('input', {
      type: 'checkbox', checked: it ? !(it.open_new_tab === false || it.open_new_tab === 0) : true,
    });

    const favi = h('button.btn.btn-sm', {
      text: '用站点 favicon',
      title: '把图标填成该站点自己的 /favicon.ico（不经过任何第三方图标服务）',
      onclick: () => {
        const v = faviconOf(url.value);
        if (!v) { toast('请先填写 http(s) 站点地址', 'warn'); return; }
        icon.value = v;
      },
    });
    // 上传本地图标 / 从已上传的里面挑（用户 2026-09-18 要求）。
    const iconUp = h('button.btn.btn-sm', {
      text: '⬆ 上传本地图标',
      title: '选一张本地图片（png / jpg / gif / webp / svg / ico，≤512 KiB）上传到面板，当作这个站点的图标',
    });
    const iconPick = h('button.btn.btn-sm', {
      text: '🖼 已上传',
      title: '从你上传过的图标里挑一张（看图选择，可删除）',
    });
    iconUp.onclick = () => uploadNavImage(icon, iconUp, '图标', false);
    iconPick.onclick = () => pickUploadedIcon(icon);

    const okBtn = h('button.btn.btn-primary', { text: isEdit ? '保存' : '创建' });
    const m = modal({
      title: isEdit ? '编辑站点' : '新建站点',
      wide: true,
      body: h('div', [
        h('div.field', [h('label', { text: '名称' }), name]),
        h('div.field', [h('label', { text: '网址（只支持 http / https）' }), url]),
        h('div.field', [
          h('label', { text: '图标' }),
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
            [icon, favi, iconUp, iconPick]),
          h('div.hint', {
            text: '填 emoji、图片直链，或上传一张本地图片（png / jpg / gif / webp / svg / ico，≤512 KiB）。'
              + '上传后可点「🖼 已上传」在已传过的里面挑。留空则显示默认图标。',
          }),
        ]),
        h('div.field', [h('label', { text: '描述' }), desc]),
        h('div.field', [h('label', { text: '所属分组' }), groupSel]),
        h('div.field', [
          h('label', { text: '打开方式' }),
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', fontSize: '13px' } }, [openNew, '在新标签页打开']),
        ]),
      ]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      const payload = {
        group_id: Number(groupSel.value),
        name: name.value.trim(),
        url: url.value.trim(),
        icon: icon.value.trim(),
        description: desc.value.trim(),
        open_new_tab: openNew.checked,
      };
      if (!payload.name) { toast('站点名称不能为空', 'warn'); return; }
      if (!payload.url) { toast('站点地址不能为空', 'warn'); return; }
      okBtn.disabled = true;
      try {
        if (isEdit) await api.navItemUpdate(idOf(it), payload);
        else await api.navItemCreate(payload);
        m.close();
        await reload(isEdit ? '站点已保存' : '站点已创建');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
  }

  // ---------------- 导入 / 导出 ----------------

  async function exportFile() {
    try {
      const res = await fetch(api.navExportURL(), { credentials: 'same-origin' });
      if (!res.ok) throw new Error(`导出失败（HTTP ${res.status}）`);
      const text = await res.text();
      const blob = new Blob([text], { type: 'application/json' });
      const a = h('a', { href: URL.createObjectURL(blob) });
      a.download = `zizpanel-nav-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 4000);
      toast('已导出导航数据（可重新导入）', 'ok');
    } catch (e) {
      toast(e.message || String(e), 'err');
    }
  }

  function importDialog() {
    const ta = h('textarea.textarea', { placeholder: '把导出的 JSON 粘贴到这里，或选择文件…', rows: '10' });
    const file = h('input', { type: 'file', accept: '.json,application/json' });
    file.addEventListener('change', async () => {
      const f = file.files && file.files[0];
      if (!f) return;
      ta.value = await f.text();
    });
    const okBtn = h('button.btn.btn-danger', { text: '导入并替换' });
    const m = modal({
      title: '导入导航数据（整份替换）',
      wide: true,
      body: h('div', [
        h('div', {
          style: { fontSize: '13px', lineHeight: '1.7', color: 'var(--text-dim)', marginBottom: '10px' },
          text: '导入会用文件内容整份替换当前所有分组与站点。请先导出一份备份。',
        }),
        h('div.field', [h('label', { text: '选择 JSON 文件' }), file]),
        h('div.field', [h('label', { text: '或直接粘贴 JSON' }), ta]),
      ]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      let doc;
      try {
        doc = JSON.parse(ta.value);
      } catch {
        toast('内容不是合法 JSON', 'err');
        return;
      }
      const gN = Array.isArray(doc.groups) ? doc.groups.length : 0;
      const iN = Array.isArray(doc.items) ? doc.items.length : 0;
      const yes = await confirmBox(
        `导入会整份替换当前所有导航分组与站点（本次将写入 ${gN} 个分组、${iN} 个站点），且不可撤销。确定继续吗？`,
        { title: '确认导入（整份替换）', danger: true, okText: '确定替换' },
      );
      if (!yes) return;
      okBtn.disabled = true;
      try {
        const res = await api.navImport(doc);
        m.close();
        await reload((res && res.msg) || '导入完成');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
  }

  // 面板内只看卡片尺寸与自定义强调色，与系统主题无关（背景/色系只在独立页），
  // 所以这里不再监听 prefers-color-scheme。

  renderToolbar();
  renderBody();
  load();
}
