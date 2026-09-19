// files.js —— 文件管理页面。
//
// 交互设计：
//   - 面包屑 + 目录树式导航；每行显示权限/属主/大小/时间
//   - 双击目录进入，双击文件打开编辑器（带语法高亮的简易编辑器）
//   - 多选 + 批量操作（删除/压缩）；上传支持拖拽与多选
//   - 所有破坏性操作都要二次确认，删除目录必须显式勾选"递归"

import { api, apiURL } from './api.js';
import { taskCenter } from './tasks.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll, esc, rate, duration } from './ui.js';
import { registerCleanup } from './app.js';

// MAX_UPLOAD 必须与后端 internal/web/api_files.go 的 maxUpload 一致（4 GiB）。
//
// 为什么前端要单独知道这个数：用户选中文件后**先本地判一次**，超限就直接给
// 明确提示、一个字节都不发。旧版是在浏览器把 512MB 传完之后才被服务端拒绝，
// 用户白等半天只看到一句失败 —— 这正是"点了没反应"的一半原因。
// 两边漂移会让"本地通过、服务端拒收"重现，所以有测试锁死这两个数字相等。
const MAX_UPLOAD = 4 * 1024 * 1024 * 1024;

// 显示隐藏文件的状态记在 localStorage（刷新/切换板块后保持）。默认关闭。
const SHOW_HIDDEN_KEY = 'zp-files-show-hidden';

let cwd = '';
let showHidden = readLS(SHOW_HIDDEN_KEY) === '1';
let selection = new Set();
let lastList = null;
// selAnchor 是 Shift 范围选择在**当前可见顺序**里的锚点索引。
let selAnchor = -1;
// visibleEntries 是最近一次渲染的可见条目顺序（键盘/范围选择都用它）。
let visibleEntries = [];
// clipboard = { mode: 'copy' | 'cut', paths: string[] }
// 只在内存里（不写 localStorage）：剪贴板内容是易失的，刷新页面后失效最不意外。
let clipboard = null;
// 当前打开的编辑器窗口（同一时刻最多一个）。**全局存活**：切换路由时不清除，
// 只有用户点 ✕ / 菜单「关闭编辑器」才会被 dispose（见 createEditorWindow 的 onClosed）。
let activeEditor = null;

export function FilesView(content, ctx = {}) {
  clear(content);
  selection = new Set();
  selAnchor = -1;
  visibleEntries = [];

  const crumbs = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap', fontSize: '13px' } });
  const toolbar = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' } });
  const tableBox = h('div', { style: { overflowX: 'auto', minHeight: '260px' } });
  const statusBar = h('div.files-status', { style: { fontSize: '12px', color: 'var(--text-mute)', padding: '8px 14px', borderTop: '1px solid var(--border-soft)' } });

  const fileInput = h('input', {
    type: 'file', multiple: true, style: { display: 'none' },
    onchange: async (e) => {
      if (e.target.files.length) {
        await uploadEntries(Array.from(e.target.files).map((f) => ({ file: f, rel: f.name })));
      }
      e.target.value = '';
    },
  });

  // 「上传文件夹」入口：webkitdirectory 让浏览器把整棵目录树交出来，
  // 每个文件带 webkitRelativePath（如 mysite/assets/app.css）。
  // 用 setAttribute 而不是 h() 的属性赋值：webkitdirectory 是**布尔** IDL 属性，
  // 赋空字符串会被浏览器当成 false（"设了但没生效"），setAttribute 才稳。
  const folderInput = h('input', {
    type: 'file', multiple: true, style: { display: 'none' },
    onchange: async (e) => {
      const files = Array.from(e.target.files || []);
      // 空文件夹浏览器不会给任何文件：这里必须**说出来**，否则"选了文件夹没反应"
      // 又是一次静默（取消选择不会触发 change，所以走到这里就是真的空）。
      if (!files.length) {
        toast('这个文件夹里没有文件（空文件夹不会有任何东西被上传）', 'warn', 8000);
        e.target.value = '';
        return;
      }
      await uploadEntries(files.map((f) => ({ file: f, rel: f.webkitRelativePath || f.name })), { folder: true });
      e.target.value = '';
    },
  });
  folderInput.setAttribute('webkitdirectory', '');
  folderInput.setAttribute('directory', '');

  // 拖拽提示区：只有拖动时才显示（平时不占版面）。
  const dropZone = h('div.files-drop', {
    style: { display: 'none', margin: '0 14px 10px' },
    text: '把文件或整个文件夹拖到这里上传（文件夹会保留目录结构）',
  });

  const card = h('div.card', [
    h('div.card-head', [crumbs]),
    h('div', { style: { padding: '10px 14px', borderBottom: '1px solid var(--border-soft)', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [toolbar]),
    dropZone,
    h('div.card-body.tight', [tableBox]),
    statusBar,
    fileInput,
    folderInput,
  ]);

  appendAll(content, card);
  wireDropZone(card);

  // wireDropZone 让"拖文件/拖文件夹进来"走与按钮完全相同的上传路径。
  //
  // 为什么要走同一条路：拖拽只是另一个入口，校验、进度、错误提示、相对路径
  // 重建目录树的行为必须与「上传文件夹」按钮一模一样，否则会出现
  // "按钮传没问题、拖进来就静默失败"这种最难查的差异。
  function wireDropZone(el) {
    const hasFiles = (e) => {
      const dt = e.dataTransfer;
      if (!dt) return false;
      const types = Array.from(dt.types || []);
      return types.includes('Files');
    };
    let depth = 0; // dragenter/dragleave 会随子元素反复触发，用计数避免提示闪烁
    const show = (on) => { dropZone.style.display = on ? '' : 'none'; };
    el.addEventListener('dragenter', (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault(); depth++; show(true);
    });
    el.addEventListener('dragover', (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'copy';
    });
    el.addEventListener('dragleave', (e) => {
      if (!hasFiles(e)) return;
      depth = Math.max(0, depth - 1);
      if (!depth) show(false);
    });
    el.addEventListener('drop', async (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault(); depth = 0; show(false);
      try {
        await handleDrop(e.dataTransfer);
      } catch (err) {
        toast('处理拖入的内容失败：' + (err && err.message ? err.message : err), 'err', 12000);
      }
    });
  }

  // handleDrop 把 DataTransfer 展开成 {file, rel} 列表。
  //
  // 优先用 webkitGetAsEntry：只有它能拿到**目录**与相对路径。
  // 它不可用时退化到 dataTransfer.files（拿不到目录结构，也不能拖文件夹），
  // 这时只能按文件名上传，并且要如实告诉用户为什么没有目录。
  async function handleDrop(dt) {
    const items = dt.items ? Array.from(dt.items).filter((it) => it.kind === 'file') : [];
    const roots = items.map((it) => (it.webkitGetAsEntry ? it.webkitGetAsEntry() : null)).filter(Boolean);
    if (!roots.length) {
      const plain = Array.from(dt.files || []);
      if (!plain.length) { toast('没有识别到可上传的文件', 'warn'); return; }
      toast('当前浏览器不支持读取拖入的文件夹，已按普通文件上传（目录结构会丢失）', 'warn', 12000);
      await uploadEntries(plain.map((f) => ({ file: f, rel: f.name })));
      return;
    }
    const entries = [];
    let hasDir = false;
    for (const root of roots) {
      if (root.isDirectory) hasDir = true;
      await walkEntry(root, '', entries);
    }
    if (!entries.length) { toast('拖入的文件夹是空的，没有可上传的文件', 'warn'); return; }
    // 只有文件夹（或混着文件夹）时才带相对路径；纯文件拖拽与「⬆ 上传」等价。
    await uploadEntries(entries, { folder: hasDir });
  }

  async function load(path) {
    if (path) cwd = path;
    selection = new Set();
    selAnchor = -1;
    clear(tableBox);
    appendAll(tableBox, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取目录…' })]));
    try {
      lastList = await api.files(cwd, showHidden);
    } catch (e) {
      clear(tableBox);
      renderOpenError(e);
      return;
    }
    cwd = lastList.path;
    renderCrumbs();
    renderToolbar();
    renderTable();
  }

  // renderOpenError 把后端的错误按行显示出来。
  //
  // 为什么不是一行 <p>：外接卷被 macOS 隐私保护拒绝时，后端（api_files.go）
  // 返回的是 **403 + 多行可操作指引**（改用「挂载到自定义挂载点」）。挤进一行
  // 文本会把换行吃掉，用户只看到 "operation not permitted" —— 那正是报障的原文。
  function renderOpenError(e) {
    const msg = String((e && e.message) || e);
    // 403 有两种：越界（面板白名单拒绝）与"外接卷被 macOS 隐私保护拒绝"。
    // 只有后者才说"被系统拒绝"，并给磁盘页入口 —— 别把面板自己的拒绝也说成系统问题。
    const isTCC = !!(e && e.status === 403) && msg.includes('隐私保护');
    const box = h('div.files-denied');
    msg.split('\n').map((s) => s.trim()).filter(Boolean)
      .forEach((ln, i) => box.append(h('div', { class: i === 0 ? 'files-denied-head' : 'files-denied-line', text: ln })));
    // 指引里提到的「磁盘 → 挂载到自定义挂载点…」直接给一个入口。
    if (isTCC) {
      box.append(h('button.btn.btn-sm', {
        text: '🖥 打开磁盘工具…',
        style: { marginTop: '8px' },
        onclick: () => { location.hash = '#/disks'; },
      }));
    }
    appendAll(tableBox, h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: isTCC ? '无法打开该目录（被系统拒绝）' : '无法打开该目录' }),
      box,
    ]));
  }

  // ---------- 位置下拉：常用 / 磁盘与卷 ----------
  //
  // 根目录集合由后端下发（lastList.roots），并带 root_kinds 标出用途。
  // 下拉只列"没有被别的根包含"的根（/opt/zizpanel/data 在 /opt/zizpanel 之下，
  // 就不重复列），末尾固定一条「🖥 管理磁盘…」跳到已存在的 #/disks 页。
  function rootKind(r) {
    return (lastList && lastList.root_kinds && lastList.root_kinds[r]) || 'other';
  }

  function rootLabel(r) {
    switch (rootKind(r)) {
      case 'www': return 'www 目录';
      case 'home': return '用户目录';
      case 'panel': return '/opt/zizpanel（面板安装根）';
      case 'homebrew': return '/opt/homebrew/etc';
      case 'volume': return r.replace(/^\/Volumes\//, '') + '（' + r + '）';
      default: return r;
    }
  }

  // dropdownRoots 去掉被其它根包含的子根，但**命名根**（www/用户/安装根/homebrew/卷）
  // 即使嵌在别的根里也保留 —— 网站根目录就在用户目录之下，不能因为它被折叠掉。
  function dropdownRoots() {
    const roots = lastList?.roots || [];
    const named = new Set(['www', 'home', 'panel', 'homebrew', 'volume']);
    return roots.filter((r) => {
      const kind = rootKind(r);
      if (kind === 'data') return false; // 面板数据目录从安装根进去即可，不重复列
      if (named.has(kind)) return true;
      return !roots.some((o) => o !== r && r.startsWith(o + '/'));
    });
  }

  // bestRoot 取"最长匹配"的根（面包屑与下拉定位都用它，避免子根被父根盖住）。
  function bestRoot(p) {
    const roots = lastList?.roots || [];
    let best = '';
    for (const r of roots) {
      if ((p === r || p.startsWith(r + '/')) && r.length > best.length) best = r;
    }
    return best;
  }

  function locationSelect() {
    const roots = lastList?.roots || [];
    const listed = dropdownRoots();
    const cur = bestRoot(cwd);
    // 当前目录若落在"被折叠的子根"里，用它的祖先根作为下拉的选中值。
    const curListed = listed.includes(cur) ? cur : listed.filter((r) => cur.startsWith(r + '/')).sort((a, b) => b.length - a.length)[0] || cur;

    const groups = { common: [], volume: [], other: [] };
    for (const r of listed) {
      const k = rootKind(r);
      if (k === 'volume') groups.volume.push(r);
      else if (k === 'www' || k === 'home' || k === 'panel' || k === 'homebrew') groups.common.push(r);
      else groups.other.push(r);
    }
    const order = { www: 0, home: 1, panel: 2, homebrew: 3, other: 9 };
    groups.common.sort((a, b) => (order[rootKind(a)] ?? 9) - (order[rootKind(b)] ?? 9));

    const opt = (r) => h('option', { value: r, text: rootLabel(r) });
    const optgroup = (label, rs) => (rs.length
      ? h('optgroup', { label }, rs.map(opt))
      : null);

    const sel = h('select.select', {
      title: '位置：常用目录 / 磁盘与卷',
      style: { width: 'auto', maxWidth: '260px', padding: '3px 26px 3px 8px', fontSize: '12.5px' },
      onchange: (e) => {
        if (e.target.value === '__disks__') {
          e.target.value = curListed;
          location.hash = '#/disks';
          return;
        }
        if (e.target.value) load(e.target.value);
      },
    }, [
      optgroup('常用', groups.common),
      optgroup('磁盘与卷', groups.volume),
      optgroup('其它位置', groups.other),
      h('option', { value: '__disks__', text: '🖥 管理磁盘…' }),
    ]);
    sel.value = curListed;
    return sel;
  }

  function renderCrumbs() {
    clear(crumbs);
    const roots = lastList?.roots || [];
    const root = bestRoot(cwd);
    const parts = [];

    // 位置下拉：始终显示（哪怕只有一个根，也要能一键跳到磁盘页）
    parts.push(locationSelect());
    parts.push(h('span', { style: { color: 'var(--text-mute)' }, text: '/' }));

    if (root) {
      const rel = cwd.slice(root.length).replace(/^\//, '');
      const segs = rel ? rel.split('/') : [];
      parts.push(h('a', {
        href: 'javascript:void(0)', text: basename(root) || root,
        title: root, onclick: () => load(root),
      }));
      let acc = root;
      segs.forEach((s) => {
        acc += '/' + s;
        const target = acc;
        parts.push(h('span', { style: { color: 'var(--text-mute)' }, text: '/' }));
        parts.push(h('a', { href: 'javascript:void(0)', text: s, onclick: () => load(target) }));
      });
    } else {
      parts.push(h('span', { text: cwd }));
    }
    // 可访问范围：悬停可见（roots 少时直接列出来也占地方）
    if (roots.length) {
      parts.push(h('span.pill', {
        text: `可访问 ${roots.length} 个位置`,
        title: '允许访问的范围：\n' + roots.join('\n'),
        style: { cursor: 'help' },
      }));
    }
    appendAll(crumbs, ...parts);
    // 父目录按钮
    if (lastList?.parent) {
      appendAll(crumbs, h('div', { style: { flex: 1 } }),
        h('button.btn.btn-sm', { text: '↑ 上一级', onclick: () => load(lastList.parent) }));
    }
  }

  // ---------- 列表：排序 / 图片预览 / 键盘 ----------
  //
  // 用户 2026-09-22："让文件管理器和编辑器变成可用的现代的工具。"
  // 编辑器换成了 CodeMirror（见下），文件列表这边补三件最常用的：
  //   · 点表头排序（名称/大小/时间），目录永远在前；
  //   · 图片点开是**预览**（把图片塞进文本编辑器是最糟的默认行为）；
  //   · 键盘：↑↓ 之外 —— Enter 打开选中项、Delete 删除选中项、Esc 取消选择，
  //     双击行直接打开（这是所有人的肌肉记忆，之前只有"编辑"按钮能点）。
  const IMAGE_EXT = /\.(png|jpe?g|gif|webp|svg|bmp|ico|avif)$/i;
  let sortKey = 'name';   // name | size | time
  let sortDir = 1;        // 1 升序 / -1 降序

  function isImage(entry) { return !entry.is_dir && IMAGE_EXT.test(entry.name || ''); }

  function sortedEntries(list) {
    const arr = list.slice();
    arr.sort((a, b) => {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1; // 目录永远排在文件前面
      let r = 0;
      if (sortKey === 'size') r = (a.size || 0) - (b.size || 0);
      else if (sortKey === 'time') r = String(a.mod_time || '').localeCompare(String(b.mod_time || ''));
      else r = String(a.name || '').localeCompare(String(b.name || ''), 'zh');
      if (r === 0) r = String(a.name || '').localeCompare(String(b.name || ''), 'zh');
      return r * sortDir;
    });
    return arr;
  }

  function sortHeader(key, text) {
    const on = sortKey === key;
    return h('th', [
      h('button.btn.btn-sm', {
        text: text + (on ? (sortDir > 0 ? ' ▲' : ' ▼') : ''),
        title: '按' + text + '排序（再点一次反向）',
        style: { padding: '2px 6px', fontSize: '12px' },
        onclick: () => {
          if (sortKey === key) sortDir = -sortDir; else { sortKey = key; sortDir = 1; }
          renderTable();
        },
      }),
    ]);
  }

  // openAny 是按文件类型选动作的唯一入口：图片 → 预览，其余 → 文本编辑器。
  function openAny(entry) {
    if (isImage(entry)) previewImage(entry);
    else openEditor(entry);
  }

  // ---------- 图片查看器（原图 + 上一张/下一张 + 缩放） ----------
  //
  // 用户要求"点开图片走内置查看器"。看图的真实需求是**在同目录的一组图之间翻页**
  // 与放大看细节，所以这里不是单图弹窗：它把当前目录里所有图片组成一个列表，
  // 支持 ←/→ 翻页、+/- 缩放、0 复位。非图片仍然走文本编辑器（见 openAny）。
  function previewImage(entry) {
    const images = (lastList?.entries || []).filter(isImage);
    let idx = images.findIndex((x) => x.path === entry.path);
    if (idx < 0) { images.unshift(entry); idx = 0; }
    let scale = 1;

    const img = h('img', {
      alt: '',
      style: { display: 'block', margin: '0 auto', maxWidth: '100%', borderRadius: '6px', background: 'var(--panel-2)', transformOrigin: 'center center' },
      onerror: () => toast('图片加载失败（可能不是浏览器支持的格式）', 'warn', 8000),
    });
    const caption = h('div.hint', { style: { marginTop: '8px', textAlign: 'center' } });
    const zoomLabel = h('span.pill', { text: '100%' });
    const stage = h('div.zp-imgview-stage', [img]);
    const nav = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', justifyContent: 'center', marginTop: '8px' } });

    function applyZoom() {
      img.style.transform = `scale(${scale})`;
      img.style.cursor = scale > 1 ? 'grab' : 'default';
      zoomLabel.textContent = Math.round(scale * 100) + '%';
    }
    function show(i) {
      idx = (i + images.length) % images.length;
      const e = images[idx];
      scale = 1; applyZoom();
      img.style.width = '';
      img.src = api.fileDownloadURL(e.path);
      img.alt = e.name;
      caption.textContent = `${e.name} · ${humanSize(e.size)} · 第 ${idx + 1}/${images.length} 张`;
    }

    function onKey(ev) {
      if (ev.key === 'ArrowLeft') { ev.preventDefault(); show(idx - 1); }
      else if (ev.key === 'ArrowRight') { ev.preventDefault(); show(idx + 1); }
      else if (ev.key === '+' || ev.key === '=') { ev.preventDefault(); scale = Math.min(6, scale + 0.25); applyZoom(); }
      else if (ev.key === '-') { ev.preventDefault(); scale = Math.max(0.25, scale - 0.25); applyZoom(); }
      else if (ev.key === '0') { ev.preventDefault(); scale = 1; applyZoom(); }
    }
    document.addEventListener('keydown', onKey);

    appendAll(nav,
      h('button.btn.btn-sm', { text: '◀ 上一张', disabled: images.length < 2, onclick: () => show(idx - 1) }),
      h('button.btn.btn-sm', { text: '下一张 ▶', disabled: images.length < 2, onclick: () => show(idx + 1) }),
      h('span', { style: { width: '10px' } }),
      h('button.btn.btn-sm', { text: '－ 缩小', onclick: () => { scale = Math.max(0.25, scale - 0.25); applyZoom(); } }),
      zoomLabel,
      h('button.btn.btn-sm', { text: '＋ 放大', onclick: () => { scale = Math.min(6, scale + 0.25); applyZoom(); } }),
      h('button.btn.btn-sm', { text: '复位', onclick: () => { scale = 1; applyZoom(); } }),
    );

    const m = modal({
      title: '🖼️ 图片查看器',
      wide: true,
      body: h('div', [
        stage,
        caption,
        nav,
        h('div.hint', { style: { textAlign: 'center', marginTop: '4px' },
          text: '快捷键：← / → 切换上一张下一张，+ / - 缩放，0 复位' }),
      ]),
      footer: () => [
        h('button.btn', { text: '下载原图', onclick: () => { window.location.href = api.fileDownloadURL(images[idx].path); } }),
        h('button.btn', { text: '用文本编辑器打开', onclick: () => { m.close(); openEditor(images[idx]); } }),
        h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() }),
      ],
      onClose: () => document.removeEventListener('keydown', onKey),
    });
    show(idx);
  }

  // wwwRoot 返回网站根目录（kind=www 的根），找不到就退回第一个根。
  function wwwRoot() {
    const roots = lastList?.roots || [];
    return roots.find((r) => rootKind(r) === 'www') || roots[0] || '';
  }

  function renderToolbar() {
    clear(toolbar);
    const selCount = selection.size;
    const clip = clipboard && clipboard.paths.length ? clipboard : null;
    appendAll(toolbar, 
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(cwd) }),
      h('button.btn.btn-sm', {
        text: '🌐 www 目录',
        title: '一键回到网站根目录',
        onclick: () => { const w = wwwRoot(); if (w) load(w); },
      }),
      h('button.btn.btn-sm', { text: '⬆ 上传', onclick: () => fileInput.click() }),
      h('button.btn.btn-sm', { text: '⬆ 上传文件夹', onclick: () => folderInput.click() }),
      h('button.btn.btn-sm', {
        text: '＋ 新建文件夹',
        onclick: async () => {
          const name = await promptBox({ title: '新建文件夹', label: '文件夹名称', placeholder: '例如 assets' });
          if (!name) return;
          try {
            await api.fileMkdir(`${cwd}/${name}`);
            toast('已创建', 'ok'); load(cwd);
          } catch (e) { toast(e.message, 'err'); }
        },
      }),
      h('button.btn.btn-sm', {
        text: '＋ 新建文件',
        onclick: async () => {
          const name = await promptBox({ title: '新建文件', label: '文件名', placeholder: '例如 index.php' });
          if (!name) return;
          try {
            await api.fileTouch(`${cwd}/${name}`);
            toast('已创建', 'ok'); load(cwd);
          } catch (e) { toast(e.message, 'err'); }
        },
      }),
      h('label', {
        style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px', cursor: 'pointer' },
        title: '只影响列表显示，不是安全边界：知道完整路径仍可直接访问（服务端按可访问范围校验）',
      }, [
        h('input', {
          type: 'checkbox', checked: showHidden,
          onchange: (e) => {
            showHidden = e.target.checked;
            try { localStorage.setItem(SHOW_HIDDEN_KEY, showHidden ? '1' : '0'); } catch { /* 存不了就本次生效 */ }
            load(cwd);
          },
        }),
        h('span', { text: '显示隐藏文件' }),
      ]),
      h('div', { style: { flex: 1 } }),
      selCount > 0 ? h('span.pill.brand', { text: `已选 ${selCount} 项` }) : null,
      // 剪贴板状态与粘贴入口：用户按了 Ctrl+C/X 之后要能一眼看到"剪贴板里有什么"。
      clip ? h('button.btn.btn-sm', {
        text: `📋 粘贴 ${clip.paths.length} 项${clip.mode === 'cut' ? '（剪切）' : '（复制）'}`,
        title: '粘贴到当前目录（Ctrl+V）',
        onclick: pasteClipboard,
      }) : null,
      // 两个"压缩"必须一眼分得清：这里是**打包**（zip/tar 归档），
      // 「🖼️ 图片压缩」是图片体积优化（另一件事，用 libvips）。
      h('button.btn.btn-sm', {
        text: '🖼️ 图片压缩',
        title: '把当前目录里的图片压小（质量 / 最长边 / 输出格式可选；默认另存为 xxx.min.<ext>，不动原文件）',
        onclick: imageCompressModal,
      }),
      selCount > 0 ? h('button.btn.btn-sm', { text: '打包压缩', onclick: compressSelected }) : null,
      selCount > 0 ? h('button.btn.btn-sm.btn-danger', { text: '删除', onclick: deleteSelected }) : null,
      h('button.btn.btn-sm', { text: '🔍 搜索', onclick: searchModal }),
    );
  }

  function renderTable() {
    clear(tableBox);
    const list = lastList?.entries || [];
    if (!list.length) {
      visibleEntries = [];
      appendAll(tableBox, h('div.empty', [
        h('div.big', { text: '📂' }),
        h('h4', { text: '这个目录是空的' }),
        h('p', { text: '可以用上方按钮上传文件或新建内容。' }),
      ]));
      updateStatus();
      return;
    }

    const ordered = sortedEntries(list);
    visibleEntries = ordered;
    const allChecked = ordered.length > 0 && ordered.every((e) => selection.has(e.path));
    const head = h('tr', [
      h('th', { style: { width: '34px' } }, [
        h('input', {
          type: 'checkbox', checked: allChecked,
          title: '全选 / 取消全选（Ctrl+A）',
          onchange: (e) => {
            selection.clear();
            if (e.target.checked) ordered.forEach((x) => selection.add(x.path));
            selAnchor = -1;
            renderToolbar(); renderTable();
          },
        }),
      ]),
      sortHeader('name', '名称'),
      sortHeader('size', '大小'),
      h('th', { text: '权限' }),
      h('th', { text: '属主' }),
      sortHeader('time', '修改时间'),
      h('th', { text: '操作' }),
    ]);

    const rows = ordered.map((e, idx) => {
      const selected = selection.has(e.path);
      return h('tr', {
        class: selected ? 'zp-row-selected' : '',
        style: selected ? { background: 'var(--brand-soft)' } : {},
        // 单击 = 选择（Ctrl/⌘ 切换、Shift 范围），双击 = 打开。
        // 这是所有文件管理器的通用肌肉记忆，之前只有"编辑"按钮能点。
        onclick: (ev) => {
          if (ev.target && (ev.target.tagName === 'INPUT' || ev.target.closest('button, a, input'))) return;
          if (ev.shiftKey && selAnchor >= 0) {
            selectRange(selAnchor, idx);
          } else if (ev.ctrlKey || ev.metaKey) {
            if (selection.has(e.path)) selection.delete(e.path); else selection.add(e.path);
            selAnchor = idx;
          } else {
            selection = new Set([e.path]);
            selAnchor = idx;
          }
          renderToolbar(); renderTable();
        },
        // 右键：文件/文件夹行的上下文菜单（按类型禁用不适用项）
        oncontextmenu: (ev) => {
          ev.preventDefault();
          if (!selection.has(e.path)) { selection = new Set([e.path]); selAnchor = idx; renderToolbar(); renderTable(); }
          rowContextMenu(ev.clientX, ev.clientY, e);
        },
        ondblclick: (ev) => {
          if (ev.target && ev.target.closest('input')) return; // 别抢勾选框
          if (e.is_dir) load(e.path); else openAny(e);
        },
      }, [
        h('td', [
          h('input', {
            type: 'checkbox', checked: selected,
            onchange: (ev) => {
              if (ev.target.checked) selection.add(e.path); else selection.delete(e.path);
              selAnchor = idx;
              renderToolbar(); renderTable();
            },
          }),
        ]),
        h('td', [
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '7px' } }, [
            h('span', { text: e.is_dir ? '📁' : fileIcon(e.name), style: { fontSize: '15px' } }),
            e.is_dir
              ? h('a', {
                href: 'javascript:void(0)', style: { fontWeight: '550' }, text: e.name,
                onclick: () => load(e.path),
              })
              : h('a', {
                href: 'javascript:void(0)', text: e.name,
                title: isImage(e) ? '点击查看' : '点击编辑（双击也可以）',
                onclick: () => openAny(e),
              }),
            e.symlink ? h('span.pill', { text: '链接', title: '指向 ' + (e.symlink_target || '?') }) : null,
            e.read_only ? h('span.pill.warn', { text: '只读' }) : null,
            e.sensitive ? h('span.pill.danger', {
              text: '敏感',
              title: '面板数据目录（数据库与凭据）。仍可读写，但覆盖/删除前会二次确认。',
            }) : null,
          ]),
        ]),
        h('td.num', { text: e.is_dir ? '—' : humanSize(e.size) }),
        h('td.mono', { style: { fontSize: '11.5px' }, text: String(e.mode_num.toString(8)).padStart(3, '0') }),
        h('td', { style: { fontSize: '11.5px' }, text: e.owner || '—' }),
        h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: e.mod_time }),
        h('td', [
          h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, [
            e.is_dir
              ? h('button.btn.btn-sm', { text: '打开', onclick: () => load(e.path) })
              : h('button.btn.btn-sm', {
                text: isImage(e) ? '查看' : '编辑',
                onclick: () => openAny(e),
              }),
            h('button.btn.btn-sm', {
              text: '下载',
              disabled: e.is_dir,
              onclick: () => { window.location.href = api.fileDownloadURL(e.path); },
            }),
            h('button.btn.btn-sm', { text: '⋯', title: '更多操作（右键也可以）', onclick: () => moreMenu(e) }),
          ]),
        ]),
      ]);
    });

    appendAll(tableBox, h('table.table', [h('thead', [head]), h('tbody', rows)]));
    // 点空白处取消选择 / 右键空白处出菜单：挂在 tableBox 的**外层**（.card-body），
    // 因为表格本身会把 tableBox 撑满，表格下方/右侧的留白属于外层容器。
    const blankHost = tableBox.parentElement || tableBox;
    blankHost.onclick = (ev) => {
      if (ev.target && ev.target.closest('tr')) return;
      if (!selection.size) return;
      selection = new Set();
      selAnchor = -1;
      renderToolbar(); renderTable();
    };
    blankHost.oncontextmenu = (ev) => {
      if (ev.target && ev.target.closest('tr')) return; // 行菜单优先
      ev.preventDefault();
      blankContextMenu(ev.clientX, ev.clientY);
    };
    updateStatus();
  }

  // selectRange 把 [a,b] 区间加入选择（按当前可见顺序）。
  function selectRange(a, b) {
    const lo = Math.min(a, b), hi = Math.max(a, b);
    const next = new Set();
    for (let i = lo; i <= hi && i < visibleEntries.length; i++) next.add(visibleEntries[i].path);
    selection = next;
  }

  // ---------- 右键菜单 ----------
  //
  // 自己起一层 .zp-ctx-menu（而不是 modal）：右键菜单要贴着鼠标、点别处就消失。
  let openCtxMenu = null;

  function closeContextMenu() {
    if (openCtxMenu) { openCtxMenu.remove(); openCtxMenu = null; }
  }

  function showContextMenu(x, y, items) {
    closeContextMenu();
    const menu = h('div.zp-ctx-menu');
    for (const it of items) {
      if (!it) continue;
      if (it.sep) { menu.appendChild(h('div.zp-ctx-sep')); continue; }
      if (it.disabled) {
        menu.appendChild(h('div.zp-ctx-item.zp-ctx-disabled', { text: it.label }));
        continue;
      }
      menu.appendChild(h('button.zp-ctx-item', {
        type: 'button', text: it.label,
        onclick: () => { closeContextMenu(); try { it.run(); } catch (e) { toast('操作失败：' + ((e && e.message) || e), 'err'); } },
      }));
    }
    menu.style.visibility = 'hidden';
    document.body.appendChild(menu);
    const r = menu.getBoundingClientRect();
    menu.style.left = Math.max(6, Math.min(x, window.innerWidth - r.width - 6)) + 'px';
    menu.style.top = Math.max(6, Math.min(y, window.innerHeight - r.height - 6)) + 'px';
    menu.style.visibility = '';
    openCtxMenu = menu;
  }

  // rowContextMenu 是文件/文件夹行的右键菜单，按条目类型禁用不适用项。
  function rowContextMenu(x, y, e) {
    const paths = selection.has(e.path) && selection.size > 1 ? [...selection] : [e.path];
    const multi = paths.length > 1;
    const target = multi ? paths : e.path;
    const archive = !e.is_dir && /\.(zip|tar\.gz|tgz|tar)$/i.test(e.name);
    const hasDirs = (lastList?.entries || []).some((x2) => selection.has(x2.path) && x2.is_dir);

    const items = [
      {
        label: e.is_dir ? '打开' : (isImage(e) ? '查看' : '编辑'),
        run: () => (e.is_dir ? load(e.path) : openAny(e)),
      },
      !e.is_dir && !isImage(e) ? { label: '用编辑器打开', run: () => openEditor(e) } : null,
      { label: '下载', disabled: e.is_dir || multi, run: () => { window.location.href = api.fileDownloadURL(e.path); } },
      archive ? { label: '解压到当前目录', run: () => extractEntry(e) } : null,
      { sep: true },
      { label: '重命名' + (multi ? '（仅单项）' : ''), disabled: multi, run: () => renameEntry(e) },
      { label: '复制（Ctrl+C）', run: () => copySelection(paths) },
      { label: '剪切（Ctrl+X）', run: () => cutSelection(paths) },
      {
        label: '粘贴（Ctrl+V）',
        disabled: !(clipboard && clipboard.paths.length),
        run: pasteClipboard,
      },
      { label: '压缩…', run: () => { if (!selection.has(e.path)) selection = new Set([e.path]); compressSelected(); } },
      { label: '权限…', disabled: multi, run: () => chmodModal(e) },
      { sep: true },
      { label: '复制完整路径', run: () => copyFullPath(e.path) },
      { label: '删除' + (multi ? ` ${paths.length} 项` : ''), run: () => (multi || hasDirs || e.is_dir ? deleteSelectionOrOne(e, paths) : deleteOne(e)) },
    ];
    showContextMenu(x, y, items);
  }

  // blankContextMenu 是空白处的右键菜单。
  function blankContextMenu(x, y) {
    showContextMenu(x, y, [
      { label: '刷新', run: () => load(cwd) },
      { sep: true },
      { label: '新建文件夹…', run: () => toolbarNew('dir') },
      { label: '新建文件…', run: () => toolbarNew('file') },
      { label: '上传文件…', run: () => fileInput.click() },
      { label: '上传文件夹…', run: () => folderInput.click() },
      { sep: true },
      { label: '粘贴（Ctrl+V）', disabled: !(clipboard && clipboard.paths.length), run: pasteClipboard },
      { label: `全选（${visibleEntries.length} 项）`, disabled: !visibleEntries.length, run: selectAll },
      { label: showHidden ? '隐藏点文件' : '显示隐藏文件', run: () => toggleHidden() },
    ]);
  }

  function updateStatus() {
    const l = lastList;
    if (!l) { statusBar.textContent = ''; return; }
    const dirs = l.entries.filter((e) => e.is_dir).length;
    const files = l.entries.length - dirs;
    statusBar.textContent = `共 ${l.total} 项（${dirs} 个目录，${files} 个文件）` +
      (l.truncated ? ' — 条目过多已截断显示' : '') +
      `　路径：${l.path}${l.writable ? '' : '（当前不可写）'}`;
  }

  function moreMenu(e) {
    const rename = h('button.btn.btn-block', {
      text: '重命名',
      onclick: async () => {
        m.close();
        const name = await promptBox({ title: '重命名', label: '新名称', value: e.name });
        if (!name || name === e.name) return;
        try {
          await api.fileRename(e.path, `${dirname(e.path)}/${name}`);
          toast('已重命名', 'ok'); load(cwd);
        } catch (err) { toast(err.message, 'err'); }
      },
    });
    const copy = h('button.btn.btn-block', {
      text: '复制到…',
      onclick: async () => {
        m.close();
        const dest = await promptBox({
          title: '复制到', label: '目标完整路径', value: e.path + '-copy',
          hint: '必须是允许访问的目录内的绝对路径',
        });
        if (!dest) return;
        try {
          await api.fileCopy(e.path, dest);
          toast('已复制', 'ok'); load(cwd);
        } catch (err) { toast(err.message, 'err'); }
      },
    });
    const move = h('button.btn.btn-block', {
      text: '移动到…',
      onclick: async () => {
        m.close();
        const dest = await promptBox({ title: '移动到', label: '目标完整路径', value: e.path, hint: '会重命名或移动到该位置' });
        if (!dest || dest === e.path) return;
        try {
          await api.fileRename(e.path, dest);
          toast('已移动', 'ok'); load(cwd);
        } catch (err) { toast(err.message, 'err'); }
      },
    });
    const chmod = h('button.btn.btn-block', {
      text: '权限…',
      title: '修改该文件/目录的读/写/执行权限（宝塔式勾选界面）',
      onclick: () => {
        m.close();
        chmodModal(e);
      },
    });
    const del = h('button.btn.btn-danger.btn-block', {
      text: '删除',
      onclick: async () => {
        m.close();
        await deleteOne(e);
      },
    });

    const buttons = [rename, copy, move, chmod];
    // 剪贴板入口（与 Ctrl+C / Ctrl+X 等价）
    buttons.splice(1, 0,
      h('button.btn.btn-block', { text: '复制', onclick: () => { m.close(); copySelection([e.path]); } }),
      h('button.btn.btn-block', { text: '剪切', onclick: () => { m.close(); cutSelection([e.path]); } }));
    // 归档文件才有解压入口
    if (/\.(zip|tar\.gz|tgz|tar)$/i.test(e.name)) {
      buttons.splice(3, 0, h('button.btn.btn-block', {
        text: '解压到当前目录',
        title: '大压缩包会在任务中心里跑，并逐条显示"已解压 N/M：文件名"',
        onclick: async () => {
          m.close();
          // 走任务中心：解压 900MB 是分钟级动作，关掉窗口也要能找回进度
          //（用户 2026-09-18 报障："一点反应都没有！没有任何进度"）。
          try {
            await taskCenter.start({
              kind: 'file_extract', target: e.path,
              title: '解压 ' + e.name,
              start: () => api.fileExtract(e.path, cwd),
              onDone: (task) => {
                if (task && task.status && task.status !== 'succeeded') {
                  toast('解压失败：' + (task.error || task.status), 'err', 14000);
                  return;
                }
                const r = (task && task.result) || {};
                toast(r.msg || '已解压到当前目录', 'ok', 9000);
                load(cwd);
              },
            });
          } catch (err) { toast('解压失败：' + ((err && err.message) || err), 'err', 12000); }
        },
      }));
    }
    buttons.push(del);

    const m = modal({
      title: e.name,
      body: h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } }, [
        h('dl.kv', [
          h('dt', { text: '完整路径' }), h('dd', { text: e.path }),
          h('dt', { text: '类型' }), h('dd', { text: e.is_dir ? '目录' : '文件' }),
          h('dt', { text: '权限' }), h('dd', { text: e.mode + '（' + String(e.mode_num.toString(8)).padStart(3, '0') + '）' }),
          h('dt', { text: '属主' }), h('dd', { text: e.owner }),
          h('dt', { text: '大小' }), h('dd', { text: e.is_dir ? '—' : `${humanSize(e.size)}（${e.size} 字节）` }),
          h('dt', { text: '修改时间' }), h('dd', { text: e.mod_time }),
          e.symlink ? h('dt', { text: '指向' }) : null,
          e.symlink ? h('dd', { text: e.symlink_target }) : null,
        ]),
        h('div', { style: { display: 'flex', flexDirection: 'column', gap: '6px', marginTop: '10px' } }, buttons),
      ]),
    });
  }

  // ---------- 单项/批量动作（右键菜单、快捷键、工具条共用） ----------
  //
  // 这些函数都定义在 FilesView 内部（闭包），所以它们始终读写**当前**这个视图的
  // cwd / lastList / selection；右键菜单与 Ctrl+C/X/V 调用的就是同一套实现。

  async function renameEntry(e) {
    const name = await promptBox({ title: '重命名', label: '新名称', value: e.name });
    if (!name || name === e.name) return;
    try {
      await api.fileRename(e.path, `${dirname(e.path)}/${name}`);
      toast('已重命名', 'ok'); load(cwd);
    } catch (err) { toast(err.message, 'err'); }
  }

  function copySelection(paths) {
    if (!paths || !paths.length) return;
    clipboard = { mode: 'copy', paths: [...paths] };
    toast(`已复制 ${paths.length} 项，到目标目录按 Ctrl+V 粘贴`, 'info', 6000);
    renderToolbar();
  }

  function cutSelection(paths) {
    if (!paths || !paths.length) return;
    clipboard = { mode: 'cut', paths: [...paths] };
    toast(`已剪切 ${paths.length} 项，到目标目录按 Ctrl+V 粘贴`, 'info', 6000);
    renderToolbar();
  }

  function selectAll() {
    selection = new Set(visibleEntries.map((e) => e.path));
    selAnchor = visibleEntries.length ? 0 : -1;
    renderToolbar(); renderTable();
  }

  function toggleHidden() {
    showHidden = !showHidden;
    try { localStorage.setItem(SHOW_HIDDEN_KEY, showHidden ? '1' : '0'); } catch { /* 本次生效 */ }
    load(cwd);
  }

  async function toolbarNew(kind) {
    const name = await promptBox(kind === 'dir'
      ? { title: '新建文件夹', label: '文件夹名称', placeholder: '例如 assets' }
      : { title: '新建文件', label: '文件名', placeholder: '例如 index.php' });
    if (!name) return;
    try {
      if (kind === 'dir') await api.fileMkdir(`${cwd}/${name}`);
      else await api.fileTouch(`${cwd}/${name}`);
      toast('已创建', 'ok'); load(cwd);
    } catch (e) { toast(e.message, 'err'); }
  }

  function copyFullPath(p) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(p)
        .then(() => toast('已复制路径：' + p, 'ok', 5000))
        .catch(() => toast(p, 'warn', 8000));
    } else {
      toast(p, 'ok', 8000);
    }
  }

  // 解压（走任务中心：大压缩包是分钟级动作，关掉窗口也要能找回进度）
  async function extractEntry(e) {
    try {
      await taskCenter.start({
        kind: 'file_extract', target: e.path,
        title: '解压 ' + e.name,
        start: () => api.fileExtract(e.path, cwd),
        onDone: (task) => {
          if (task && task.status && task.status !== 'succeeded') {
            toast('解压失败：' + (task.error || task.status), 'err', 14000);
            return;
          }
          const r = (task && task.result) || {};
          toast(r.msg || '已解压到当前目录', 'ok', 9000);
          load(cwd);
        },
      });
    } catch (err) { toast('解压失败：' + ((err && err.message) || err), 'err', 12000); }
  }

  // confirmSensitive 对"敏感目录"里的条目追加一道确认。
  //
  // 面板数据目录（SQLite 库、凭据）仍然可读写（用户明确要求），但覆盖/删除它
  // 可能直接让面板起不来 —— 所以单独再问一次，而不是混在普通确认里。
  async function confirmSensitive(entries, action) {
    const hit = (entries || []).filter((e) => e && e.sensitive);
    if (!hit.length) return true;
    return confirmBox(
      `⚠️ 涉及 ${hit.length} 项位于**面板数据目录**（数据库与凭据）。\n\n` +
      `${action}可能让面板无法登录或启动。确定继续？`,
      { title: '敏感目录确认', danger: true, okText: '我已了解，继续' });
  }

  // ---------- 粘贴（复制 / 剪切） ----------
  //
  // 目标已存在同名项时**必须先问**（保留两者 / 覆盖 / 跳过），绝不静默覆盖。
  // 剪切走后端 /files/move：同卷是 rename，跨卷回退 copy+delete，结果里带 way。
  function askPasteConflict(names, total, isCut) {
    return new Promise((resolve) => {
      let picked = 'rename';
      const radios = [
        { v: 'rename', label: '保留两者（推荐）', desc: '自动改名（如 index-1.php），两个都留着，什么都不丢。' },
        { v: 'overwrite', label: '覆盖同名项', desc: `用${isCut ? '被剪切的内容' : '复制出来的内容'}替换目标 —— 目标原有内容不可恢复。` },
        { v: 'skip', label: '跳过同名项', desc: '目标已存在的项不动，只处理不重名的。' },
      ];
      const inputs = radios.map((r) => h('input', {
        type: 'radio', name: 'zp-paste-conflict', value: r.v, checked: r.v === picked,
        onchange: () => { picked = r.v; },
      }));
      const body = h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, [
        h('p', { text: `目标目录「${cwd}」里已有 ${names.length} 个同名项（本次共 ${total} 项）：` }),
        h('ul', { style: { margin: '6px 0 10px 18px', maxHeight: '160px', overflow: 'auto' } },
          names.slice(0, 50).map((n) => h('li', { text: n }))),
        ...radios.map((r, i) => h('label', {
          style: { display: 'flex', gap: '8px', alignItems: 'flex-start', padding: '8px 10px',
            border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', marginBottom: '8px', cursor: 'pointer' },
        }, [inputs[i], h('div', [
          h('div', { style: { fontWeight: '600' }, text: r.label }),
          h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: r.desc }),
        ])])),
      ]);
      const m = modal({
        title: '目标已存在同名项',
        body,
        footer: [
          h('button.btn', { text: '取消', onclick: () => { m.close(); resolve(null); } }),
          h('button.btn.btn-primary', { text: '继续粘贴', onclick: () => { m.close(); resolve(picked); } }),
        ],
      });
    });
  }

  // uniqueTarget 在目标目录里找一个不冲突的名字。
  function uniqueTarget(dir, name, taken) {
    if (!taken.has(name)) return `${dir}/${name}`;
    const dot = name.lastIndexOf('.');
    const base = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : '';
    for (let i = 1; i < 1000; i++) {
      const cand = `${base}-${i}${ext}`;
      if (!taken.has(cand)) return `${dir}/${cand}`;
    }
    return `${dir}/${base}-${Date.now()}${ext}`;
  }

  async function pasteClipboard() {
    if (!clipboard || !clipboard.paths.length) { toast('剪贴板是空的', 'warn'); return; }
    const isCut = clipboard.mode === 'cut';
    // 剪切后粘贴回**同一个目录**是无操作：静默略过，别弹"同名冲突"骚扰用户。
    const items = clipboard.paths.slice().filter((p) => !(isCut && p === `${cwd}/${basename(p)}`));
    if (!items.length) {
      toast(isCut ? '这些项已经在当前目录里' : '没有可粘贴的内容', 'warn');
      return;
    }
    const existing = new Set((lastList?.entries || []).map((e) => e.name));
    const conflicts = [...new Set(items.map((p) => basename(p)).filter((n) => existing.has(n)))];
    let strategy = 'rename';
    if (conflicts.length) {
      const choice = await askPasteConflict(conflicts, items.length, isCut);
      if (!choice) return;
      strategy = choice;
    }

    const taken = new Set(existing);
    const okList = [];
    const renamed = [];
    const failed = [];
    let skipped = 0;

    for (const src of items) {
      const name = basename(src);
      let dest = `${cwd}/${name}`;
      // 剪切后粘贴回原目录是无操作；复制到原目录则是"制造一份副本"（走下面的改名分支）。
      if (isCut && src === dest) { skipped++; continue; }
      if (taken.has(name) || src === dest) {
        if (strategy === 'skip') { skipped++; continue; }
        if (strategy === 'rename') {
          dest = uniqueTarget(cwd, name, taken);
          renamed.push(`${name} → ${basename(dest)}`);
        } else if (strategy === 'overwrite' && src === dest) {
          // 不能"先删掉自己再复制自己"：那会把唯一一份数据删没。
          skipped++; continue;
        }
      }
      try {
        if (isCut) {
          if (strategy === 'overwrite' && existing.has(name) && entrySensitive(dest)) {
            if (!await confirmSensitive([{ path: dest, sensitive: true }], `覆盖「${name}」`)) { skipped++; continue; }
          }
          const r = await api.fileMove(src, dest, strategy === 'overwrite' ? 'overwrite' : 'rename');
          if (r && r.skipped) { skipped++; continue; }
          okList.push({ name, dest: (r && r.to) || dest, way: (r && r.way) || '' });
        } else {
          if (strategy === 'overwrite' && existing.has(name)) {
            if (!await confirmSensitive([{ path: dest, sensitive: entrySensitive(dest) }],
              `覆盖「${name}」`)) { skipped++; continue; }
            await api.fileDelete([dest], true);
          }
          await api.fileCopy(src, dest);
          okList.push({ name, dest, way: 'copy' });
        }
      } catch (e) { failed.push(`${name}: ${e.message}`); }
      taken.add(basename(dest));
    }

    if (isCut && !failed.length) clipboard = null; // 全部成功才清空剪贴板
    const ways = new Set(okList.map((o) => o.way));
    let msg = `${isCut ? '移动' : '复制'}完成：成功 ${okList.length} 项`;
    if (renamed.length) msg += `，自动改名 ${renamed.length} 项`;
    if (skipped) msg += `，跳过 ${skipped} 项`;
    if (failed.length) msg += `，失败 ${failed.length} 项`;
    if (isCut && ways.has('copy+delete')) msg += '（含跨卷：复制后删除源）';
    toast(msg, failed.length ? 'warn' : 'ok', failed.length ? 15000 : 6000);
    if (renamed.length) toast('自动改名：' + renamed.slice(0, 5).join('，') + (renamed.length > 5 ? ' …' : ''), 'info', 8000);
    if (failed.length) {
      modal({
        title: '粘贴失败明细',
        body: h('div', [
          h('p', { text: `成功 ${okList.length} 项，失败 ${failed.length} 项。失败的文件没有被改动。` }),
          h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.8' } }, failed.map((f) => h('li', { text: f }))),
        ]),
      });
    }
    renderToolbar();
    load(cwd);
  }

  // entrySensitive 判断某个路径是否落在敏感根里（用于粘贴覆盖前的二次确认）。
  function entrySensitive(p) {
    const roots = lastList?.sensitive_roots || [];
    return roots.some((r) => p === r || p.startsWith(r + '/'));
  }

  // ---------- 权限修改（勾选式 UI，宝塔式入口） ----------
  function chmodModal(e) {
    const cur = Number(e.mode_num) & 0o777;
    const bits = [
      { bit: 0o400, who: '属主 u', label: '读' }, { bit: 0o200, who: '属主 u', label: '写' }, { bit: 0o100, who: '属主 u', label: '执行' },
      { bit: 0o040, who: '同组 g', label: '读' }, { bit: 0o020, who: '同组 g', label: '写' }, { bit: 0o010, who: '同组 g', label: '执行' },
      { bit: 0o004, who: '其他 o', label: '读' }, { bit: 0o002, who: '其他 o', label: '写' }, { bit: 0o001, who: '其他 o', label: '执行' },
    ];
    const boxes = bits.map((b) => h('input', { type: 'checkbox', checked: (cur & b.bit) !== 0, dataset: { bit: String(b.bit) } }));
    const modeText = h('span.mono', { style: { fontWeight: '600' }, text: cur.toString(8).padStart(3, '0') });
    const recursive = h('input', { type: 'checkbox' });

    const currentMode = () => boxes.reduce((v, b) => v | (b.checked ? Number(b.dataset.bit) : 0), 0);
    const sync = () => { modeText.textContent = currentMode().toString(8).padStart(3, '0'); };
    boxes.forEach((b) => b.addEventListener('change', sync));

    const rows = ['属主 u', '同组 g', '其他 o'].map((who, gi) => h('tr', [
      h('td', { text: who }),
      ...boxes.slice(gi * 3, gi * 3 + 3).map((b) => h('td', [b])),
    ]));

    const m = modal({
      title: '权限：' + e.name,
      body: h('div', { style: { fontSize: '13px', lineHeight: '1.7' } }, [
        h('div.hint', { text: '完整路径：' + e.path + (e.is_dir ? '（目录）' : '（文件）') }),
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '对象' }), h('th', { text: '读 (4)' }), h('th', { text: '写 (2)' }), h('th', { text: '执行 (1)' })])]),
          h('tbody', rows),
        ]),
        h('div', { style: { marginTop: '10px' } }, [h('span', { text: '八进制：' }), modeText]),
        e.is_dir ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '10px', cursor: 'pointer' } },
          [recursive, h('span', { text: '同时递归应用到该目录下的所有内容' })]) : null,
        h('div.hint', { style: { marginTop: '8px' },
          text: '常用：644 = 普通文件；755 = 目录或可执行脚本；600 = 仅属主可读写。' }),
        e.sensitive ? h('div.hint', { style: { color: 'var(--warn)' },
          text: '⚠️ 这是面板数据目录里的内容，改错权限可能让面板无法读取数据库或凭据。' }) : null,
      ]),
      footer: [
        h('button.btn', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-primary', {
          text: '应用',
          onclick: async () => {
            const mode = currentMode().toString(8).padStart(3, '0');
            if (!await confirmSensitive([e], `修改「${e.name}」的权限`)) return;
            if (e.is_dir && recursive.checked && !await confirmBox(
              `将递归修改「${e.name}」下所有内容的权限为 ${mode}。继续？`,
              { title: '递归修改权限', okText: '递归修改' })) return;
            try {
              const r = await api.fileChmod(e.path, mode, e.is_dir && recursive.checked);
              toast(r.msg || '权限已修改', 'ok');
              m.close(); load(cwd);
            } catch (err) { toast(err.message, 'err'); }
          },
        }),
      ],
    });
  }

  // ---------- 删除 ----------
  async function deleteOne(e) {
    if (!await confirmSensitive([e], `删除「${e.name}」`)) return;
    if (e.is_dir) {
      const recursive = await confirmBox(
        `确认删除目录「${e.name}」及其中的全部内容？\n\n此操作不可撤销。`,
        { title: '删除目录', danger: true, okText: '递归删除' });
      if (!recursive) return;
      try {
        await api.fileDelete([e.path], true);
        toast('已删除', 'ok'); load(cwd);
      } catch (err) { toast(err.message, 'err', 10000); }
      return;
    }
    if (!await confirmBox(`确认删除文件「${e.name}」？`, { title: '删除文件', danger: true, okText: '删除' })) return;
    try {
      await api.fileDelete([e.path], false);
      toast('已删除', 'ok'); load(cwd);
    } catch (err) { toast(err.message, 'err', 10000); }
  }

  // deleteSelectionOrOne 供右键菜单调用：多选时批量删，单选时走单项删除。
  async function deleteSelectionOrOne(e, paths) {
    if (paths && paths.length > 1) return deleteSelected();
    return deleteOne(e);
  }

  async function deleteSelected() {
    const paths = [...selection];
    if (!paths.length) return;
    const entries = (lastList?.entries || []).filter((e) => selection.has(e.path));
    if (!await confirmSensitive(entries, '删除这些内容')) return;
    const hasDir = entries.some((e) => e.is_dir);
    const msg = `将删除 ${paths.length} 项${hasDir ? '（包含目录，其中的内容会一并删除）' : ''}。\n\n此操作不可撤销。`;
    if (!await confirmBox(msg, { title: '批量删除', danger: true, okText: '确认删除' })) return;
    try {
      const r = await api.fileDelete(paths, true);
      if (r.error) toast(r.msg, 'warn', 12000);
      else toast(r.msg, 'ok');
      load(cwd);
    } catch (e) { toast(e.message, 'err', 10000); }
  }

  // imageCompressModal 是「图片压缩」弹窗：引擎状态 + 选项 + 走任务中心。
  //
  // 设计要点（都是用户提过的要求）：
  //   · 先给**事实**：这个目录里有几张图、共多大（后端扫描给出），值不值得跑用户自己判断；
  //   · 引擎没装时**不让点**，并直接给「一键安装」（不把用户打发去别的页面找）；
  //   · 长任务走任务中心（关掉窗口也能在任务中心看进度），结果逐条列出"省了多少"；
  //   · 默认**不动原文件**（另存 .min），覆盖是显式选项且会先确认。
  async function imageCompressModal() {
    const body = h('div');
    const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' } });
    const m = modal({ title: '🖼️ 图片压缩（libvips）', body, footer: [foot], wide: false });
    body.append(h('div.hint', { text: '正在读取引擎与目录…' }));

    let st = null;
    try {
      st = await api.imageEngine(cwd, false);
    } catch (e) {
      body.textContent = '读取引擎状态失败：' + ((e && e.message) || e);
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
      return;
    }

    // 选项（把上次的选择留在本页会话里，省得每次都重设）
    const quality = h('input', { type: 'range', min: '40', max: '100', step: '1', value: String(st.default_quality || 82), style: { flex: '1' } });
    const qualityText = h('span', { text: String(st.default_quality || 82), style: { width: '34px', textAlign: 'right' } });
    quality.addEventListener('input', () => { qualityText.textContent = quality.value; });
    const format = h('select.select', [
      h('option', { value: 'keep', text: '保持原格式（只重新编码）' }),
      h('option', { value: 'webp', text: 'WebP（通常最小，兼容性好）' }),
      h('option', { value: 'avif', text: 'AVIF（更小，但编码慢、老浏览器不支持）' }),
      h('option', { value: 'jpeg', text: 'JPEG' }),
      h('option', { value: 'png', text: 'PNG（无损，压不动多少）' }),
    ]);
    const maxEdge = h('select.select', (st.max_edge_choices || [0, 1920, 2560, 3840]).map((v) => h('option', {
      value: String(v), text: v === 0 ? '不缩放（只重压）' : '最长边 ' + v + 'px（只缩小，不放大）',
    })));
    maxEdge.value = '0';
    const strip = h('input', { type: 'checkbox' });
    const recursive = h('input', { type: 'checkbox' });
    const overwrite = h('input', { type: 'checkbox' });

    function draw() {
      clear(body);
      clear(foot);

      if (!st.available) {
        body.append(h('div', { style: { lineHeight: '1.8' } }, [
          h('div', { style: { fontWeight: '620', color: 'var(--warn)' }, text: '引擎还没装（缺 libvips）' }),
          h('div', { style: { marginTop: '4px' }, text: st.reason || '' }),
          h('div.hint', { style: { marginTop: '8px' },
            text: '「图片压缩（libvips）」是原生 arm64 包，不需要 Docker / Node / PHP；装好后这个弹窗就能用了。' }),
        ]));
        foot.append(h('button.btn.btn-primary', {
          text: '一键安装（libvips）',
          onclick: async () => {
            try {
              await taskCenter.start({
                kind: 'install', target: st.market_app_id, title: '安装 图片压缩（libvips）',
                start: () => api.marketInstall(st.market_app_id),
                onDone: async (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('安装失败：' + (task.error || task.status), 'err', 12000);
                    return;
                  }
                  toast('引擎已装好，正在刷新…', 'ok', 8000);
                  try { st = await api.imageEngine(cwd, recursive.checked); } catch (e) { /* 下面 draw 会如实显示 */ }
                  draw();
                },
              });
            } catch (e) { toast('安装失败：' + ((e && e.message) || e), 'err', 12000); }
          },
        }));
        foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
        return;
      }

      const scan = st.scan_error
        ? h('div.hint', { style: { color: 'var(--danger)' }, text: '扫描目录失败：' + st.scan_error })
        : h('div.hint', {
          text: '目录 ' + (st.dir || cwd) + '：找到 ' + st.image_count + ' 张图片，共 ' + humanSize(st.total_bytes)
            + (st.image_count === 0 ? '（这个目录里没有可压缩的图片）' : ''),
        });

      body.append(h('div', { style: { lineHeight: '1.7' } }, [
        h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)', marginBottom: '6px' },
          text: '引擎：' + (st.version || '') + '（' + (st.bin || '') + '）' }),
        scan,
        h('div.field', { style: { marginTop: '10px' } }, [
          h('label', { text: '质量（越小越省体积；PNG 是无损格式，这一项对它无效）' }),
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [quality, qualityText]),
        ]),
        h('div.field', [h('label', { text: '输出格式' }), format]),
        h('div.field', [h('label', { text: '尺寸' }), maxEdge]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '8px' } },
          [strip, h('span', { text: '去掉元数据（EXIF/ICC 等，体积更小；照片的拍摄信息会丢）' })]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } },
          [recursive, h('span', { text: '包含子目录' })]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } },
          [overwrite, h('span', { text: '覆盖原文件（默认不勾：另存为 xxx.min.<ext>）' })]),
        h('div.hint', { style: { marginTop: '8px' },
          text: '失败或"压完反而更大"的文件一律保留原样并逐条说明；压缩在任务中心执行，关掉这个窗口也能看进度。' }),
      ]));

      const start = h('button.btn.btn-primary', { text: '开始压缩' + (st.image_count ? '（' + st.image_count + ' 张）' : '') });
      start.addEventListener('click', async () => {
        const opts = {
          dir: st.dir || cwd,
          recursive: recursive.checked,
          quality: Number(quality.value),
          format: format.value,
          max_edge: Number(maxEdge.value),
          strip_metadata: strip.checked,
          overwrite: overwrite.checked,
        };
        if (opts.overwrite && !await confirmBox(
          '将**覆盖** ' + (st.dir || cwd) + ' 里的原图（共 ' + st.image_count + ' 张）。\n\n'
          + '压完更大的文件会自动保留原样，但覆盖不可撤销 —— 重要图片建议先备份。\n\n继续？',
          { title: '覆盖原文件', okText: '覆盖压缩' })) return;
        m.close();
        try {
          await taskCenter.start({
            kind: 'image_compress', target: opts.dir,
            title: '压缩图片（' + st.image_count + ' 张）',
            start: () => api.imageCompress(opts),
            onDone: (task) => {
              if (task && task.status && task.status !== 'succeeded') {
                toast('图片压缩失败：' + (task.error || task.status), 'err', 14000);
                return;
              }
              const r = (task && task.result) || {};
              toast('图片压缩完成：成功 ' + (r.done || 0) + ' 张，跳过 ' + (r.skipped || 0)
                + ' 张（压完更大），失败 ' + (r.failed || 0) + ' 张；共省 ' + humanSize(r.saved_bytes || 0),
              r.failed ? 'warn' : 'ok', 14000);
              load(cwd);
            },
          });
        } catch (e) { toast('图片压缩失败：' + ((e && e.message) || e), 'err', 14000); }
      });
      foot.append(h('button.btn', { text: '重新扫描', onclick: async () => {
        try { st = await api.imageEngine(cwd, recursive.checked); draw(); }
        catch (e) { toast('扫描失败：' + ((e && e.message) || e), 'err'); }
      } }));
      foot.append(start);
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
      recursive.addEventListener('change', () => { /* 需要重新扫描才准 */ });
    }
    draw();
  }

  async function compressSelected() {
    const paths = [...selection];
    const names = paths.map((p) => basename(p));
    const format = h('select.select', [
      h('option', { value: 'zip', text: 'ZIP（通用）' }),
      h('option', { value: 'tar.gz', text: 'TAR.GZ（保留权限与软链接）' }),
      h('option', { value: 'tar', text: 'TAR（不压缩）' }),
    ]);
    const output = h('input.input', { value: (names.length === 1 ? names[0].replace(/\.[^.]+$/, '') : 'archive') + '.zip' });
    format.addEventListener('change', () => {
      const base = output.value.replace(/\.(zip|tar\.gz|tar)$/i, '');
      output.value = base + '.' + format.value;
    });
    const m = modal({
      title: `压缩 ${paths.length} 项`,
      body: h('div', [
        h('div.field', [h('label', { text: '压缩格式' }), format]),
        h('div.field', [h('label', { text: '输出文件名' }), output]),
        h('div.hint', { text: '归档会生成在当前目录下。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始打包',
          title: '大目录会在任务中心里跑，并逐条显示"已处理 N/M：路径"',
          onclick: async () => {
            close();
            try {
              await taskCenter.start({
                kind: 'file_compress', target: cwd,
                title: '打包 ' + names.length + ' 项',
                start: () => api.fileCompress(cwd, names, format.value, output.value),
                onDone: (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('打包失败：' + (task.error || task.status), 'err', 14000);
                    return;
                  }
                  const r = (task && task.result) || {};
                  toast(r.msg || '打包完成', 'ok', 9000);
                  load(cwd);
                },
              });
            } catch (e) { toast('打包失败：' + ((e && e.message) || e), 'err', 12000); }
          },
        }),
      ],
    });
  }

  // ---------- 上传 ----------
  //
  // 三条不可退让的规矩（用户报障后定的）：
  //   1. 绝不静默：选中文件后先本地判大小，超限立刻弹明确提示并且**不发请求**；
  //   2. 必须能看出"在动"：用 XHR（不是 fetch）拿 upload.onprogress，
  //      显示百分比 + 已传/总大小 + 速度 + 预计剩余；
  //   3. 失败必须给出**服务端返回的原因**（HTTP status + body 里的 msg），
  //      网络中断明说"连接中断"，不许只剩一句干等的 toast。

  // uploadEntries 是所有上传入口（按钮/文件夹按钮/拖拽）的唯一实现。
  //
  // 整体套 try/catch：**任何**没预料到的异常都必须变成用户看得见的提示。
  // 2026-09-20 那次报障的形态就是"异常抛在任何提示之前 → 彻底无声"，
  // 所以这里不允许再出现没有出口的异常路径。
  async function uploadEntries(entries, opts = {}) {
    try {
      if (!entries.length) return;
      const problem = precheckUpload(entries);
      if (problem) { showUploadProblem(problem); return; }
      // 同名文件先问清楚：覆盖还是共存（用户 2026-09-22 明确要求）。
      // 上传文件夹不走这个询问 —— 它的语义本来就是"按原结构覆盖整站"，
      // 进度窗里也明说了；把 index.php 问成 index-1.php 会让站点直接跑不起来。
      let onConflict = '';
      if (!opts.folder) {
        const dup = conflictNames(entries);
        if (dup.length) {
          const choice = await askUploadConflict(dup, entries.length);
          if (!choice) return; // 用户取消：一个字节都没上传
          onConflict = choice;
        }
      }
      await runUpload(entries, { folder: !!opts.folder, onConflict });
    } catch (err) {
      const msg = err && err.message ? err.message : String(err);
      toast('上传未能开始：' + msg, 'err', 15000);
    }
  }

  // conflictNames 返回这一批里"目标目录已存在同名条目"的文件名（去重）。
  //
  // 判据用**当前目录的列表**（lastList）：用户看到的就是它，所以弹窗里说的
  // 名字与他屏幕上的一致。列表可能已经过期（别人刚传过），那种情况由服务端兜底
  // （on_conflict 传到后端，真正落盘时再判一次）。
  function conflictNames(entries) {
    const have = new Set(((lastList && lastList.entries) || []).map((e) => e.name));
    const out = [];
    for (const e of entries) {
      const base = e.rel.split('/').pop();
      if (have.has(base) && !out.includes(base)) out.push(base);
    }
    return out;
  }

  // askUploadConflict 问用户"同名文件怎么办"，返回 'rename' / 'overwrite' / null（取消）。
  //
  // 默认选中「保留两者」：不覆盖是最安全的默认值（覆盖别人的文件不可逆）。
  function askUploadConflict(names, total) {
    return new Promise((resolve) => {
      let picked = 'rename';
      const radios = [
        { v: 'rename', label: '保留两者（推荐）', desc: '同名文件自动改名（如 index-1.php），两个都留着，什么都不丢。' },
        { v: 'overwrite', label: '覆盖同名文件', desc: '用新上传的文件替换旧文件 —— 旧内容不可恢复，请确认。' },
      ];
      const inputs = radios.map((r) => h('input', {
        type: 'radio', name: 'zp-upload-conflict', value: r.v, checked: r.v === picked,
        onchange: () => { picked = r.v; },
      }));
      const body = h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, [
        h('p', { text: `目标目录里已经有 ${names.length} 个同名文件（本次共 ${total} 个文件）：` }),
        h('ul', { style: { margin: '6px 0 10px 18px', maxHeight: '160px', overflow: 'auto' } },
          names.slice(0, 50).map((n) => h('li', { text: n }))),
        ...radios.map((r, i) => h('label', {
          style: { display: 'flex', gap: '8px', alignItems: 'flex-start', padding: '8px 10px',
            border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', marginBottom: '8px', cursor: 'pointer' },
        }, [inputs[i], h('div', [
          h('div', { style: { fontWeight: '600' }, text: r.label }),
          h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: r.desc }),
        ])])),
        h('div.hint', { text: '这个选择对本次上传的所有同名文件都生效。' }),
      ]);
      const m = modal({
        title: '同名文件已存在',
        body,
        footer: [
          h('button.btn', { text: '取消上传', onclick: () => { m.close(); resolve(null); } }),
          h('button.btn.btn-primary', {
            text: '继续上传',
            onclick: () => { m.close(); resolve(picked); },
          }),
        ],
      });
    });
  }

  // precheckUpload 在**发请求之前**做本地校验，返回 null 表示可以上传。
  //
  // 为什么总大小也要判：整个 multipart 请求体只受后端 maxUpload 约束，
  // "每个文件都没超但加起来超了"同样会被服务端拒收 —— 那又是一次白传。
  function precheckUpload(entries) {
    const total = entries.reduce((s, e) => s + (e.file.size || 0), 0);
    const oversize = entries.filter((e) => (e.file.size || 0) > MAX_UPLOAD);
    if (!oversize.length && total <= MAX_UPLOAD) return null;
    return { total, oversize };
  }

  // showUploadProblem 把"超限"讲清楚：文件名 + 实际大小 + 上限 + 建议。
  function showUploadProblem({ total, oversize }) {
    const rows = [];
    if (oversize.length) {
      rows.push(h('p', { text: `有 ${oversize.length} 个文件超过单个文件上限：` }));
      const list = h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.8' } },
        oversize.slice(0, 20).map((e) => h('li', { text: `${e.rel} — ${humanSize(e.file.size)}` })));
      rows.push(list);
      if (oversize.length > 20) rows.push(h('p', { text: `…另有 ${oversize.length - 20} 个同样超限的文件` }));
    } else {
      rows.push(h('p', { text: `这一次要传的总大小是 ${humanSize(total)}，超过单次上传上限。` }));
    }
    rows.push(h('p', {
      style: { marginTop: '10px' },
      text: `面板单次上传上限：${humanSize(MAX_UPLOAD)}；你这次总共 ${humanSize(total)}。`,
    }));
    rows.push(h('div', {
      style: { marginTop: '8px', lineHeight: '1.8' },
      text: '建议：① 用「⬆ 上传文件夹」把网站按子目录分批传上去（超限的那个文件仍然要单独处理）；' +
        '② 先在本地分卷压缩（如 site.part1.zip、site.part2.zip）再逐个上传；' +
        '③ 也可以再把压缩包解压。文件一个字节都没有上传，不需要清理。',
    }));
    const m = modal({
      title: '⛔ 超过上传上限，已在上传前拦下',
      body: h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, rows),
      footer: [h('button.btn.btn-primary', { text: '知道了', onclick: () => m.close() })],
    });
  }

  // runUpload 真正发请求：XHR + 进度窗。
  async function runUpload(entries, { folder, onConflict }) {
    const total = entries.reduce((s, e) => s + (e.file.size || 0), 0);

    // ---- 进度窗（先建窗口，再拼请求体）----
    //
    // 顺序很重要：拼 FormData 也可能抛（历史上 appendAll 就抛在这里，
    // 而窗口还没建 → 用户什么都看不到）。先把窗口立起来，任何后续异常
    // 都能显示在窗口里。
    const barFill = h('i', { style: { width: '0%' } });
    const lineMain = h('div', { style: { fontWeight: '600' } , text: '正在准备…' });
    const lineRate = h('div', { style: { color: 'var(--text-mute)', marginTop: '4px' }, text: ' ' });
    const lineNote = h('div', { style: { marginTop: '8px', fontSize: '12.5px', color: 'var(--text-mute)' },
      text: folder ? `将在 ${cwd} 下按原目录结构重建（同名文件会被覆盖）` : `目标目录：${cwd}` });

    // 状态与 XHR 先声明再接线：按钮回调（取消上传）会引用 xhr。
    const started = Date.now();
    let lastDraw = 0;
    let loaded = 0;
    let finished = false;

    // 上传必须用 XHR：fetch 完全没有"上传进度"能力（只有下载流的 reader），
    // 这正是旧版只剩一句干等 toast 的原因。XHR 的 upload.onprogress 才有已传字节。
    const xhr = new XMLHttpRequest();
    xhr.open('POST', apiURL('files/upload'), true);
    xhr.withCredentials = true;
    xhr.setRequestHeader('X-CSRF-Token', readCookie('zp_csrf'));

    let modalRef = null;
    const cancelBtn = h('button.btn', { text: '取消上传', onclick: () => xhr.abort() });
    const closeBtn = h('button.btn.btn-primary', { text: '关闭', style: { display: 'none' }, onclick: () => modalRef && modalRef.close() });
    modalRef = modal({
      title: folder ? `⬆ 上传文件夹（${entries.length} 个文件）` : `⬆ 上传 ${entries.length} 个文件`,
      body: h('div', { style: { fontSize: '13.5px', lineHeight: '1.7' } }, [
        lineMain,
        h('div.bar', [barFill]),
        lineRate,
        lineNote,
      ]),
      footer: [cancelBtn, closeBtn],
      closeOnBackdrop: false, // 上传中误点遮罩不该让窗口消失（那看起来又像"没反应"）
      closeOnEsc: false,
    });

    // 每 200ms~1s 刷新一次界面：**即使一个进度事件都没有**也要能看出"在动" ——
    // 大文件在浏览器决定何时发第一个 progress 事件前可能安静好几秒，
    // 那几秒里用户看到的不能是静止的窗口。
    const ticker = setInterval(() => {
      if (!finished) draw(loaded);
    }, 1000);

    function draw(sent) {
      const now = Date.now();
      const elapsed = Math.max(0.001, (now - started) / 1000);
      const p = total > 0 ? Math.min(100, (sent / total) * 100) : 0;
      barFill.style.width = p.toFixed(1) + '%';
      lineMain.textContent = total > 0
        ? `已上传 ${humanSize(sent)} / ${humanSize(total)}（${p.toFixed(1)}%）`
        : `已上传 ${humanSize(sent)}`;
      const bps = sent / elapsed;
      let rest = '—';
      if (sent > 0 && total > sent) rest = duration((total - sent) / Math.max(1, bps));
      lineRate.textContent = `速度 ${rate(bps)} · ${sent > 0 && total > sent ? `预计剩余 ${rest}` : '等待服务端确认'} · 已用 ${duration(elapsed)}`;
    }

    function finish() {
      finished = true;
      clearInterval(ticker);
      cancelBtn.style.display = 'none';
      closeBtn.style.display = '';
    }

    // fail 把失败原因写进同一个窗口（用户不用去找别的地方），再补一条 toast。
    function fail(title, detail) {
      finish();
      barFill.style.background = 'var(--danger)';
      lineMain.textContent = title;
      lineMain.style.color = 'var(--danger)';
      lineRate.textContent = '';
      clear(lineNote);
      appendAll(lineNote, h('div', { text: detail }));
      toast(`${title}：${detail}`, 'err', 15000);
    }

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) loaded = e.loaded;
      const now = Date.now();
      if (now - lastDraw < 200) return; // 进度事件很密，限制重绘频率
      lastDraw = now;
      draw(loaded);
    };

    // 拼请求体。FormData 必须用 fd.append(two args) —— appendAll 是 DOM 助手，
    // 拿它塞 FormData 会抛（见 ui.js 的注释）。这里的异常会显示在进度窗里。
    let fd;
    try {
      fd = new FormData();
      fd.append('dir', cwd);
      // on_conflict：用户在上面那个弹窗里的选择（普通上传才有）。
      // 传空时后端按形态取默认值（普通=rename 保留两者，文件夹=overwrite）。
      if (onConflict) fd.append('on_conflict', onConflict);
      if (folder) {
        // 相对路径按顺序与 files 一一对应；后端 Go 的 multipart 解析对同一字段名保序。
        fd.append('relpaths', JSON.stringify(entries.map((e) => e.rel)));
      }
      for (const e of entries) fd.append('files', e.file, e.file.name);
    } catch (err) {
      fail('无法准备上传数据', (err && err.message ? err.message : String(err)));
      return;
    }
    xhr.onload = () => {
      let data = null;
      try { data = JSON.parse(xhr.responseText); } catch (_) { data = null; }
      const okBody = xhr.status >= 200 && xhr.status < 300 && data && data.ok !== false;
      if (!okBody) {
        fail(`上传失败（HTTP ${xhr.status}${xhr.statusText ? ' ' + xhr.statusText : ''}）`, serverReason(xhr, data));
        return;
      }
      finish();
      const payload = data.data || {};
      const uploaded = payload.uploaded || [];
      const failed = payload.failed || [];
      const overwritten = uploaded.filter((u) => u.overwritten).length;
      barFill.style.width = '100%';
      lineMain.textContent = `✅ ${payload.msg || `已上传 ${uploaded.length} 个文件`}`;
      lineMain.style.color = 'var(--ok)';
      lineRate.textContent = `共 ${humanSize(uploaded.reduce((s, u) => s + (u.size || 0), 0))} · 用时 ${duration((Date.now() - started) / 1000)}`;
      clear(lineNote);
      if (overwritten) appendAll(lineNote, h('div', { text: `其中 ${overwritten} 个覆盖了同名文件。` }));
      else if (payload.on_conflict === 'rename') {
        const renamed = uploaded.filter((u) => basename(u.rel_path || u.name || '') !== (u.name || ''));
        if (renamed.length) {
          appendAll(lineNote, h('div', { text: `其中 ${renamed.length} 个与已有文件重名，已自动改名（两个都保留）：` }),
            h('ul', { style: { margin: '4px 0 0 18px', lineHeight: '1.7' } },
              renamed.slice(0, 10).map((u) => h('li', { text: `${basename(u.rel_path || '')} → ${u.name}` }))));
        }
      }
      if (failed.length) {
        appendAll(lineNote,
          h('div', { style: { color: 'var(--warn)', marginTop: '6px' }, text: `${failed.length} 个文件失败：` }),
          h('ul', { style: { margin: '4px 0 0 18px', lineHeight: '1.7' } }, failed.map((f) => h('li', { text: String(f) }))));
        toast(`已上传 ${uploaded.length} 个，${failed.length} 个失败（详见窗口）`, 'warn', 15000);
      } else {
        toast(payload.msg || '上传完成', 'ok');
      }
      // 文件夹上传后，若所有相对路径都在同一个顶层目录下，给一个"进入"按钮
      const top = singleTopDir(entries.filter((e) => e.rel.includes('/')).map((e) => e.rel));
      if (folder && top) {
        appendAll(lineNote, h('button.btn.btn-sm', {
          text: `进入 ${top}`, style: { marginTop: '8px' },
          onclick: () => { modalRef && modalRef.close(); load(`${cwd}/${top}`); },
        }));
      }
      load(cwd);
    };
    xhr.onerror = () => fail('连接中断，上传未完成',
      '与服务端的连接被中断（网络断开、面板重启、或服务端在读请求时中断）。已上传的部分文件可能留在目标目录里，重传即可。');
    xhr.ontimeout = () => fail('上传超时', '服务端在限定时间内没有收完数据。');
    xhr.onabort = () => fail('已取消上传', '你点了「取消上传」，请求已中止。已传完的文件会留在目标目录里。');
    xhr.send(fd);
  }

  // serverReason 把服务端返回的原因原样取出来（这是用户唯一能据此自救的信息）。
  //
  // 优先 JSON 里的 msg/error（面板自己的失败响应）；拿不到就把原始 body 截一段
  // 显示出来（例如反向代理返回的 413 HTML 页面 —— 那也必须让用户看见）。
  function serverReason(xhr, data) {
    let msg = '';
    if (data && typeof data === 'object') msg = String(data.msg || data.error || '');
    if (!msg) {
      const txt = String(xhr.responseText || '').trim();
      if (txt) msg = txt.length > 400 ? txt.slice(0, 400) + '…' : txt;
    }
    return msg || '（服务端没有返回任何说明）';
  }

  // singleTopDir 返回一组相对路径共同的唯一顶层目录（没有则返回 ''）。
  function singleTopDir(rels) {
    if (!rels.length) return '';
    const tops = new Set(rels.map((r) => r.split('/')[0]));
    return tops.size === 1 ? [...tops][0] : '';
  }

  // ---------- 编辑器 ----------
  //
  // 编辑器是**窗口化 + 全局存活**的（见文件末尾 createEditorWindow）：同一时刻只存在
  // 一个窗口，再次打开别的文件时在窗口里新开一个标签（不叠一层新窗口、也不顶掉旧标签）。
  //
  // 关键：编辑器层挂在 document.body 上，而路由切换只重建 #app 里的内容，所以窗口
  // 天然跨板块存活；离开文件页时**不再销毁它**（见本函数末尾的 cleanup）。这样
  // 最小化后的胶囊切到仪表盘/网站再切回来仍在，点开内容不丢。
  //
  // 编辑器由旧版 FilesView 创建，但它跨路由存活后，回调里捕获的旧 DOM 已经脱离。
  // 所以每次 FilesView 重建都把 refreshList / onDir 重新绑到**当前**这个视图上。
  function editorOptions() {
    return {
      roots: (lastList && lastList.roots) || [],
      refreshList: () => { if (content.isConnected) load(cwd); },
      onDir: (p) => { if (content.isConnected) load(p); },
    };
  }

  function openInEditorWindow(entry, res) {
    if (activeEditor) {
      activeEditor.setOptions(editorOptions());
      activeEditor.openFile(entry, res);
      return;
    }
    activeEditor = createEditorWindow(entry, res, Object.assign(editorOptions(), {
      onClosed: () => { activeEditor = null; },
    }));
  }

  async function openEditor(entry) {
    let res;
    try {
      res = await api.fileRead(entry.path);
    } catch (e) {
      toast(e.message, 'err');
      return;
    }
    if (res.binary) {
      modal({
        title: entry.name,
        body: h('div.empty', [
          h('div.big', { text: '🔒' }),
          h('h4', { text: '这是二进制文件' }),
          h('p', { text: '为避免破坏文件，不提供在线编辑。可以下载后用本地工具处理。' }),
        ]),
      });
      return;
    }
    if (res.too_large) {
      modal({
        title: entry.name,
        body: h('div.empty', [
          h('div.big', { text: '📦' }),
          h('h4', { text: '文件过大' }),
          h('p', { text: `该文件 ${humanSize(res.size)}，超过在线编辑上限（2MB）。请下载后编辑，或用 Web 终端处理。` }),
        ]),
      });
      return;
    }

    openInEditorWindow(entry, res);
  }

  // ---------- 搜索 ----------
  function searchModal() {
    const query = h('input.input', { placeholder: '文件名或内容关键词' });
    const mode = h('select.select', [
      h('option', { value: 'name', text: '按文件名搜索' }),
      h('option', { value: 'content', text: '按文件内容搜索' }),
    ]);
    const results = h('div', { style: { marginTop: '12px', maxHeight: '380px', overflow: 'auto' } });

    const doSearch = async () => {
      if (!query.value.trim()) { toast('请输入关键词', 'warn'); return; }
      clear(results);
      appendAll(results, h('div.empty', [h('p', { text: '搜索中…' })]));
      try {
        const r = await api.fileSearch(cwd, query.value.trim(), mode.value, 200);
        clear(results);
        if (!r.hits.length) {
          appendAll(results, h('div.empty', [h('p', { text: `没有找到匹配项（已扫描 ${r.scanned} 项，耗时 ${r.elapsed_ms}ms）` })]));
          return;
        }
        appendAll(results, 
          h('div.hint', { text: `找到 ${r.hits.length} 项，扫描 ${r.scanned} 项，耗时 ${r.elapsed_ms}ms` + (r.truncated ? '（结果已截断）' : '') }),
          h('table.table', [
            h('thead', [h('tr', [h('th', { text: '名称' }), h('th', { text: '路径' }), h('th', { text: '操作' })])]),
            h('tbody', r.hits.map((hit) => h('tr', [
              h('td', [
                h('div', { text: hit.name }),
                hit.match_line ? h('div', {
                  style: { fontSize: '11px', color: 'var(--text-mute)', fontFamily: 'var(--mono)' },
                  text: `第 ${hit.line_no} 行：${hit.match_line}`,
                }) : null,
              ]),
              h('td.mono', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: hit.path }),
              h('td', [
                h('button.btn.btn-sm', {
                  text: hit.is_dir ? '打开' : '编辑',
                  onclick: () => {
                    m.close();
                    if (hit.is_dir) load(hit.path);
                    else openEditor({ path: hit.path, name: hit.name });
                  },
                }),
                h('button.btn.btn-sm', {
                  text: '定位',
                  onclick: () => { m.close(); load(dirname(hit.path)); },
                }),
              ]),
            ]))),
          ]),
        );
      } catch (e) {
        clear(results);
        appendAll(results, h('div.empty', [h('p', { text: '搜索失败：' + e.message })]));
      }
    };
    query.addEventListener('keydown', (e) => { if (e.key === 'Enter') doSearch(); });

    const m = modal({
      title: `搜索：${cwd}`,
      wide: true,
      body: h('div', [
        h('div.row', [
          h('div.field', [h('label', { text: '关键词' }), query]),
          h('div.field', [h('label', { text: '搜索方式' }), mode]),
        ]),
        h('button.btn.btn-primary', { text: '开始搜索', onclick: doSearch }),
        results,
      ]),
    });
    setTimeout(() => query.focus(), 60);
  }

  // 键盘：Ctrl+A 全选、Ctrl+C/X/V 复制/剪切/粘贴、Delete 删除、F2 重命名、Enter 打开、
  // Esc 取消选择。输入框/弹窗里有焦点时一律不接管（否则在搜索框里按 Delete 会删文件）。
  function onKeyDown(e) {
    const tag = (document.activeElement && document.activeElement.tagName) || '';
    if (/^(INPUT|TEXTAREA|SELECT)$/.test(tag)) return;
    if (document.querySelector('.modal-mask')) return;
    // 编辑器窗口展开时不要抢它的快捷键（最小化成胶囊时文件列表照常可操作）。
    const edWin = document.querySelector('.zpf-layer .zpf-win');
    if (edWin && !edWin.classList.contains('zpf-win-min')) return;
    const mod = e.ctrlKey || e.metaKey;
    const key = e.key;

    if (mod && (key === 'a' || key === 'A')) {
      if (!visibleEntries.length) return;
      e.preventDefault(); selectAll(); return;
    }
    if (mod && (key === 'c' || key === 'C')) {
      if (!selection.size) return;
      e.preventDefault(); copySelection([...selection]); return;
    }
    if (mod && (key === 'x' || key === 'X')) {
      if (!selection.size) return;
      e.preventDefault(); cutSelection([...selection]); return;
    }
    if (mod && (key === 'v' || key === 'V')) {
      if (!(clipboard && clipboard.paths.length)) return;
      e.preventDefault(); pasteClipboard(); return;
    }
    if (key === 'Escape' && selection.size) {
      selection.clear(); selAnchor = -1; renderToolbar(); renderTable(); return;
    }
    if ((key === 'Delete' || key === 'Backspace') && selection.size) {
      e.preventDefault(); deleteSelected(); return;
    }
    if (key === 'F2' && selection.size) {
      const first = visibleEntries.find((x) => selection.has(x.path));
      if (!first) return;
      e.preventDefault(); renameEntry(first); return;
    }
    if (key === 'Enter' && selection.size) {
      const first = visibleEntries.find((x) => selection.has(x.path));
      if (!first) return;
      e.preventDefault();
      if (first.is_dir) load(first.path); else openAny(first);
    }
  }
  document.addEventListener('keydown', onKeyDown);
  // 右键菜单：点别处 / 滚动 / Esc 就关掉（与系统菜单一致）。
  const onDocPointer = (ev) => {
    if (openCtxMenu && !openCtxMenu.contains(ev.target)) closeContextMenu();
  };
  const onDocScroll = () => closeContextMenu();
  document.addEventListener('mousedown', onDocPointer);
  window.addEventListener('scroll', onDocScroll, true);
  registerCleanup(() => {
    document.removeEventListener('keydown', onKeyDown);
    document.removeEventListener('mousedown', onDocPointer);
    window.removeEventListener('scroll', onDocScroll, true);
    closeContextMenu();
    // 编辑器**故意不在这里销毁**：它是全局存活的窗口（用户要求 1 —— 最小化后切成
    // 胶囊，切到别的板块再切回来仍要在、内容不能丢）。模块级的 activeEditor 引用
    // 也不会因为路由切换而失效。关掉它只有两条路：窗口右上角 ✕，或菜单「文件 →
    // 关闭编辑器」。未保存的内容由 beforeunload 守卫 + 关闭前确认保护。
  });
  // 编辑器可能由**上一次**的 FilesView 创建并一直存活：这里把它的 refreshList / onDir
  // 重新绑到当前这个视图，否则保存后的文件列表刷新会打在已经脱离文档的旧 DOM 上。
  if (activeEditor) activeEditor.setOptions(editorOptions());
  load(cwd || undefined);
}

// ============================================================================
//  在线编辑器（内嵌 CodeMirror 5，MIT）
//
//  为什么不再自己写（用户 2026-09-22 授权："让文件管理器和编辑器变成可用的现代
//  的工具，写不好可以直接引入开源项目。"）：
//  这里原来是一套自研的"透明 textarea + 高亮层"。字体、行高、内边距、Tab 宽度
//  任何一处不一致就会错位，而它真的错了两次且都是用户先发现的：
//    · 光标与文字逐列错开 —— 浏览器 UA 给 `<code>` 写死了 font-family: monospace，
//      直接作用在元素上的规则压过继承，两层字宽不一致；
//    · 第一次点进编辑区光标被拉到开头 —— focus 处理器里改了选区。
//  这类"自己维护一个文本编辑器"的账越滚越大，而 CodeMirror 5 是成熟稳定的选择：
//  光标/选区/撤销重做、查找替换、括号匹配、自动缩进、代码折叠、大文件视口渲染
//  全部现成，且是**单文件 UMD + 按需模式**，不需要任何构建步骤就能嵌进面板。
//
//  按需加载：CodeMirror 本体 + 语言模式约 470KB，只有真的打开编辑器时才加载，
//  不让它拖慢面板首屏（仪表盘/文件列表根本用不到）。
//  许可证与来源见 assets/vendor/codemirror/README.md（文件未做任何修改）。
// ============================================================================

// 扩展名 → 语言键。语言键再映射到 CodeMirror 的模式与依赖文件。
const EXT_LANG = {
  php: 'php', phtml: 'php',
  js: 'js', mjs: 'js', cjs: 'js', jsx: 'js',
  ts: 'ts', tsx: 'ts',
  json: 'json',
  go: 'go',
  py: 'py',
  sh: 'sh', bash: 'sh', zsh: 'sh',
  yaml: 'yaml', yml: 'yaml',
  html: 'html', htm: 'html',
  css: 'css', scss: 'css', less: 'css',
  sql: 'sql',
  ini: 'ini', conf: 'ini', cnf: 'ini', properties: 'ini', env: 'ini',
  md: 'md', markdown: 'md',
  xml: 'xml', svg: 'xml',
  dockerfile: 'docker',
};

// 每种语言：给用户看的名字 + CodeMirror 的 mode + 需要按顺序加载的模式文件。
//
// 依赖关系不能省：php 模式依赖 xml + javascript + css + htmlmixed + clike，
// htmlmixed 又依赖 xml + javascript + css —— 少加载一个，CodeMirror 会**静默**
// 退化成纯文本（控制台只留一行 undefined 模式名），用户看到的是"高亮没了"。
const CM_LANGS = {
  php: { label: 'PHP', mode: 'application/x-httpd-php', deps: ['xml', 'javascript', 'css', 'htmlmixed', 'clike', 'php'] },
  js: { label: 'JavaScript', mode: 'javascript', deps: ['javascript'] },
  ts: { label: 'TypeScript', mode: 'javascript', deps: ['javascript'] },
  json: { label: 'JSON', mode: { name: 'javascript', json: true }, deps: ['javascript'] },
  go: { label: 'Go', mode: 'go', deps: ['go'] },
  py: { label: 'Python', mode: 'python', deps: ['python'] },
  sh: { label: 'Shell', mode: 'shell', deps: ['shell'] },
  yaml: { label: 'YAML', mode: 'yaml', deps: ['yaml'] },
  html: { label: 'HTML', mode: 'htmlmixed', deps: ['xml', 'javascript', 'css', 'htmlmixed'] },
  xml: { label: 'XML', mode: 'xml', deps: ['xml'] },
  css: { label: 'CSS', mode: 'css', deps: ['css'] },
  sql: { label: 'SQL', mode: 'sql', deps: ['sql'] },
  ini: { label: 'INI / 配置', mode: 'properties', deps: ['properties'] },
  md: { label: 'Markdown', mode: 'markdown', deps: ['markdown'] },
  nginx: { label: 'Nginx', mode: 'text/x-nginx-conf', deps: ['nginx'] },
  docker: { label: 'Dockerfile', mode: 'text/x-dockerfile', deps: ['dockerfile'] },
};

// langKeyFor 判断某个文件名该用哪种语言（认不出来的返回空 = 纯文本）。
//
// 文件名判据优先于扩展名：`nginx.conf` / `Dockerfile` 都没有可用的扩展名，
// 只看后缀会把它们当纯文本 —— 而这两种恰恰是面板里最常改的文件。
function langKeyFor(name) {
  const base = String(name || '').split('/').pop().toLowerCase();
  if (base === 'dockerfile' || base.startsWith('dockerfile.')) return 'docker';
  if (base === 'nginx.conf' || /\.nginx$/.test(base)) return 'nginx';
  const ext = base.includes('.') ? base.slice(base.lastIndexOf('.') + 1) : '';
  return EXT_LANG[ext] || '';
}

// ---------------- 资源按需加载 ----------------
//
// 为什么自己写 <script> 注入而不是 `import()`：CodeMirror 5 是 UMD 包，
// 它把 CodeMirror 挂到 window 上；`import()` 一个 UMD 文件拿到的是它的
// module.exports，而模式/插件文件之间靠**全局 CodeMirror**互相注册 ——
// 混用两条路径会让插件注册到另一个实例上，表现为"模式加载了但没生效"。
const CM_ASSET_BASE = new URL('../vendor/codemirror/', import.meta.url);
const CM_CORE_CSS = [
  'codemirror.min.css',
  'addon/dialog/dialog.min.css',
  'addon/fold/foldgutter.min.css',
  'addon/scroll/simplescrollbars.min.css',
];
const CM_CORE_JS = [
  'codemirror.min.js',
  'addon/search/searchcursor.min.js',
  'addon/search/search.min.js',
  'addon/search/jump-to-line.min.js',
  'addon/dialog/dialog.min.js',
  'addon/edit/matchbrackets.min.js',
  'addon/edit/closebrackets.min.js',
  'addon/edit/continuelist.min.js',
  'addon/edit/matchtags.min.js',
  'addon/fold/foldcode.min.js',
  'addon/fold/foldgutter.min.js',
  'addon/fold/brace-fold.min.js',
  'addon/fold/xml-fold.min.js',
  'addon/fold/comment-fold.min.js',
  'addon/fold/indent-fold.min.js',
  'addon/selection/active-line.min.js',
  'addon/scroll/simplescrollbars.min.js',
  'addon/comment/comment.min.js',
];

const cmAssetLoaded = new Set();
let cmCorePromise = null;

function cmLoadOne(url, isCss) {
  if (cmAssetLoaded.has(url)) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const el = isCss
      ? Object.assign(document.createElement('link'), { rel: 'stylesheet', href: url })
      : Object.assign(document.createElement('script'), { src: url, async: false });
    el.onload = () => { cmAssetLoaded.add(url); resolve(); };
    el.onerror = () => reject(new Error('加载编辑器资源失败：' + url));
    document.head.appendChild(el);
  });
}

// ensureCodeMirror 保证本体与插件就绪（同一个 Promise 只加载一次；失败不缓存，
// 下次打开还能重试 —— 缓存失败的 Promise 会让编辑器"永远打不开"）。
function ensureCodeMirror() {
  if (!cmCorePromise) {
    cmCorePromise = (async () => {
      for (const f of CM_CORE_CSS) await cmLoadOne(new URL(f, CM_ASSET_BASE).href, true);
      // 顺序加载 JS：插件依赖全局 CodeMirror，异步并行会让插件先于本体执行
      for (const f of CM_CORE_JS) await cmLoadOne(new URL(f, CM_ASSET_BASE).href, false);
      if (!window.CodeMirror) throw new Error('CodeMirror 已加载但没有挂上 window.CodeMirror');
      ensureEditorStyle();
    })().catch((e) => { cmCorePromise = null; throw e; });
  }
  return cmCorePromise;
}

// ensureLang 按依赖顺序加载该语言需要的模式文件（模式之间也有依赖，见 CM_LANGS）。
async function ensureLang(langKey) {
  const def = CM_LANGS[langKey];
  if (!def) return;
  for (const m of def.deps) {
    await cmLoadOne(new URL(`mode/${m}.min.js`, CM_ASSET_BASE).href, false);
  }
}

// ---------------- 配色 ----------------
//
// 语义只有两档（键名沿用旧版：用户已经选过的 panel / monokai 都还认）：
//   · panel   → 跟随面板：浅色面板用官方浅色主题 eclipse、深色面板用官方深色主题
//               material-darker（`:root[data-theme]` 一变就实时跟着换）；
//   · monokai → 官方 monokai。
//
// 语法着色**全部**来自 CodeMirror 官方主题（`vendor/codemirror/theme/*.min.css`，
// 从 cdnjs `codemirror@5.65.16` 原样下载、未改动一个字）。
//
// 为什么删掉自研主题：旧版手写了一个 `cm-s-<自研名>` 主题的 token 颜色，又在弹窗
// 根节点上加一个类去覆盖面板 CSS 变量（--panel/--text/--bg-soft…），后者把弹窗里的
// 按钮与输入框一起染成 Monokai 色 —— 用户 2026-09-22 报的"按钮颜色有问题"。
// 现在的原则：编辑器配色只用官方主题，面板自己的控件永远用面板变量。
const ZPF_THEME_KEY = 'zp-file-editor-theme';
const ZPF_THEME_PANEL = 'panel';
const ZPF_THEME_MONOKAI = 'monokai';
const ZPF_CSS_ID = 'zpf-editor-style';

// CodeMirror 主题名 == theme/<name>.min.css 的文件名 == `.cm-s-<name>` 类名。
const CM_THEME_LIGHT = 'eclipse';
const CM_THEME_DARK = 'material-darker';
const CM_THEME_MONOKAI = 'monokai';

function readEditorTheme() {
  try {
    return localStorage.getItem(ZPF_THEME_KEY) === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
  } catch { return ZPF_THEME_PANEL; }
}

function saveEditorTheme(v) {
  try { localStorage.setItem(ZPF_THEME_KEY, v); } catch { /* 存不了就本次会话生效 */ }
}

// panelIsLight 读面板**真正生效**的主题：app.js 把 light/dark/auto 三态折算后
// 写到 documentElement 的 data-theme 上。读不到时按深色算 —— app.css 的
// `:root` 默认就是深色，两者一致。
function panelIsLight() {
  return document.documentElement.dataset.theme === 'light';
}

function cmThemeFor(setting) {
  if (setting === ZPF_THEME_MONOKAI) return CM_THEME_MONOKAI;
  return panelIsLight() ? CM_THEME_LIGHT : CM_THEME_DARK;
}

// ensureCmTheme 按需加载官方主题 CSS（与本体一样：用到哪个才加载哪个）。
function ensureCmTheme(name) {
  return cmLoadOne(new URL(`theme/${name}.min.css`, CM_ASSET_BASE).href, true);
}

// ensureEditorStyle 注入**最小布局**样式（只注入一次）。
//
// 只负责"让 CodeMirror 撑满容器"和"查找框跟随面板"；背景/前景/语法色一律交给
// 官方主题，所以这里**不许**再出现任何 token 颜色或面板变量覆盖。
function ensureEditorStyle() {
  if (document.getElementById(ZPF_CSS_ID)) return;
  const st = document.createElement('style');
  st.id = ZPF_CSS_ID;
  st.textContent = [
    // 编辑器高度铺满外层容器（CodeMirror 需要一个有高度的父元素）
    '.zpf-cm { position: relative; display: flex; flex-direction: column; min-height: 0; }',
    '.zpf-cm .CodeMirror { flex: 1 1 auto; height: auto; min-height: 0; font-family: var(--mono); font-size: 13px; line-height: 1.6; }',
    // 查找/替换对话框：CodeMirror 自带的是浅色浮层，这里让它跟随面板
    '.zpf-cm .CodeMirror-dialog { background: var(--panel); color: var(--text); border-top: 1px solid var(--border); padding: 6px 10px; font-size: 12.5px; }',
    '.zpf-cm .CodeMirror-dialog input { background: var(--bg-soft); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 3px 6px; font-family: var(--mono); }',
    '.zpf-cm .CodeMirror-dialog button { background: var(--panel-2); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 2px 8px; cursor: pointer; }',
  ].join('\n');
  document.head.appendChild(st);
}

// ---------------- 编辑器窗口（窗口化：图标按钮 + 菜单栏 + 目录树 + 状态栏） ----------------
//
// 用户 2026-09-23 原话："文件编辑器还是太差了，能不能照抄宝塔的吗？"
// 宝塔的文件编辑器是"窗口 + 目录树 + 菜单栏"的形态，所以这里按那套重做：
//   · 右上角是**图标按钮**（最小化 / 最大化 / 关闭），不是文字按钮，每个都有中文 title；
//   · 左侧目录树：展开/折叠、点目录切换浏览目录、点文件切换编辑文件、当前文件高亮、
//     宽度可拖动（记在 localStorage）；
//   · 菜单栏把已有能力（保存/另存为/查找/跳行/刷新/下载/关闭…）收进「文件/编辑/视图/帮助」；
//   · 窗口可拖动，位置记在 localStorage；
//   · 「最大化」= 铺满**面板 content 区域**（不是浏览器全屏、不是系统全屏）；
//   · 「最小化」= 右下角胶囊，**不加任何遮罩**。
//
// 为什么不再用 modal()：modal 的遮罩是 `position:fixed; inset:0`，最小化时若忘了
// 写 `pointer-events:none`，收起后整块透明遮罩仍然吞掉所有点击 —— 这正是用户报的
// "最小化后点不了面板别处"。这里自己起一层 `.zpf-layer`（`pointer-events:none`）
// + 窗口本身（`pointer-events:auto`），从结构上杜绝"全屏遮罩挡住点击"。

const ZPF_TREE_W_KEY = 'zp-file-editor-tree-w';
const ZPF_TREE_HIDDEN_KEY = 'zp-file-editor-tree-hidden';
const ZPF_POS_KEY = 'zp-file-editor-pos';
// 还原态相对 content 区域的内缩：让它看起来是一个"窗口"，同时仍然充满内容区。
const ZPF_WIN_INSET = 14;

function clamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }

function readLS(key) { try { return localStorage.getItem(key); } catch { return null; } }
function writeLS(key, v) { try { localStorage.setItem(key, String(v)); } catch { /* 存不了就本次会话生效 */ } }

// contentRect 返回面板内容区（`.content`）的视口坐标。编辑器的所有几何都以它为界。
function contentRect() {
  const el = document.querySelector('.content');
  if (el) {
    const r = el.getBoundingClientRect();
    if (r.width > 80 && r.height > 80) return r;
  }
  return { left: 0, top: 0, right: window.innerWidth, bottom: window.innerHeight, width: window.innerWidth, height: window.innerHeight };
}

// pickTreeRoot 选目录树的根：优先用白名单根目录里**包含该文件**的那个（最长匹配），
// 否则退到文件所在目录 —— 这样树不会从一个莫名其妙的祖先开始。
function pickTreeRoot(filePath, roots) {
  const p = String(filePath || '');
  let best = '';
  for (const raw of roots || []) {
    const r = String(raw || '').replace(/\/+$/, '');
    if (!r) continue;
    if ((p === r || p.startsWith(r + '/')) && r.length > best.length) best = r;
  }
  if (best) return best;
  return dirname(p) || '/';
}

function readStoredPos() {
  try {
    const v = JSON.parse(readLS(ZPF_POS_KEY) || 'null');
    if (v && Number.isFinite(v.x) && Number.isFinite(v.y)) return { x: v.x, y: v.y };
  } catch { /* 坏值就按默认位置 */ }
  return { x: 0, y: 0 };
}

function cssEsc(s) {
  if (window.CSS && CSS.escape) return CSS.escape(String(s));
  return String(s).replace(/["\\]/g, '\\$&');
}

// createEditorWindow 打开一个编辑器窗口（同一时刻只应存在一个）。
// entry/res 是初始文件；opts.roots 是文件白名单根目录；opts.refreshList 刷新背后的文件列表；
// opts.onDir 是"树里点了目录"时要切换的浏览目录；opts.onClosed 用于让调用方清掉引用。
//
// 多标签（用户要求 2）：窗口里可以有多个文件标签。做法是**一个 CodeMirror 实例 +
// 每个标签一份 CodeMirror.Doc**，切换标签用 cm.swapDoc()。为什么不是每个标签造一个
// 编辑器实例：Doc 会自带**各自的撤销历史、光标与滚动位置**，这正好是标签切换要的语义，
// 而一个实例只维护一套 DOM/事件，代价最小。查找框、配色、缩进等实例级选项全局共用。
function createEditorWindow(entry0, res0, opts = {}) {
  let roots = opts.roots || [];
  let refreshList = typeof opts.refreshList === 'function' ? opts.refreshList : () => {};
  let onDir = typeof opts.onDir === 'function' ? opts.onDir : () => {};
  const onClosed = typeof opts.onClosed === 'function' ? opts.onClosed : () => {};

  // 活动标签的"镜像"变量：下面大量既有函数直接读写 entry/res/dirty 等，切标签时
  // 由 activateTab() 先 commit 回标签对象、再把这些变量换成新标签的值，改动面最小。
  let entry = entry0;
  let res = res0;
  let cm = null;
  let dirty = false;
  let theme = readEditorTheme();
  let themeSeq = 0;
  let maximized = false;
  let minimized = false;
  let disposed = false;
  let langKey = langKeyFor(entry.name);
  let langDef = CM_LANGS[langKey];

  // ---- 标签状态 ----
  // tab: { id, entry, res, doc, dirty, langKey, langDef, cleanGen }
  const tabs = [];
  let tabSeq = 0;
  let activeTabId = null;

  function currentTab() { return tabs.find((t) => t.id === activeTabId) || null; }
  function tabForPath(p) { return tabs.find((t) => t.entry.path === p) || null; }
  function mkTab(e, r) {
    const key = langKeyFor(e.name);
    return { id: 'zpf-filetab-' + (++tabSeq), entry: e, res: r, doc: null, dirty: false, langKey: key, langDef: CM_LANGS[key], cleanGen: 0 };
  }
  // snapshotActive 把镜像变量写回活动标签对象（切换/保存前调用）。
  function snapshotActive() {
    const t = currentTab();
    if (!t) return;
    t.entry = entry; t.res = res; t.dirty = dirty; t.langKey = langKey; t.langDef = langDef;
  }

  const firstTab = mkTab(entry0, res0);
  tabs.push(firstTab);
  activeTabId = firstTab.id;

  // ---- 目录树状态 ----
  let treeRoot = pickTreeRoot(entry.path, roots);
  let treeWidth = clamp(Number(readLS(ZPF_TREE_W_KEY)) || 230, 150, 460);
  let treeHidden = readLS(ZPF_TREE_HIDDEN_KEY) === '1';
  const treeChildren = new Map(); // dir -> entries[]
  const treeExpanded = new Set([treeRoot]);
  const treeLoading = new Set();
  let curDir = dirname(entry.path);

  // ---- 位置记忆（还原态默认位置之上的偏移） ----
  let pos = readStoredPos();

  // ===================== DOM =====================
  const titleText = h('span.zpf-title');
  const titlePath = h('span.zpf-title-path');
  const dirtyDot = h('span.zpf-dirty-dot', { style: { display: 'none' } });

  const minBtn = h('button.zpf-iconbtn', { text: '–', title: '最小化（收成右下角胶囊，面板仍可操作）', 'aria-label': '最小化' });
  const maxBtn = h('button.zpf-iconbtn', { text: '⛶', title: '最大化（铺满面板内容区）', 'aria-label': '最大化' });
  const closeBtn = h('button.zpf-iconbtn.zpf-close', { text: '✕', title: '关闭编辑器（有未保存修改会先确认；Esc 不会关闭编辑器）', 'aria-label': '关闭' });

  const titlebar = h('div.zpf-titlebar', [
    h('span', { text: '📝', style: { fontSize: '13px' } }),
    titleText,
    titlePath,
    h('div.spacer'),
    dirtyDot,
    minBtn,
    maxBtn,
    closeBtn,
  ]);

  const menubar = h('div.zpf-menubar');
  const tabStrip = h('div.zpf-filetabs');

  const treeHead = h('div.zpf-tree-head');
  const treeScroll = h('div.zpf-tree-scroll');
  const treeEl = h('aside.zpf-tree', [treeHead, treeScroll]);
  const treeResizer = h('div.zpf-tree-resizer', { title: '拖动调整目录树宽度（双击还原默认宽度）' });

  const host = h('div.zpf-cm');
  const statusInfo = h('span');
  const statusbar = h('div.zpf-statusbar', [statusInfo]);
  const editEl = h('section.zpf-edit', [host, statusbar]);
  const bodyEl = h('div.zpf-body', [treeEl, treeResizer, editEl]);

  const win = h('div.zpf-win', [titlebar, menubar, tabStrip, bodyEl]);
  const layer = h('div.zpf-layer', [win]);
  document.body.appendChild(layer);

  // ===================== 几何：窗口 / 最大化 / 最小化 =====================
  function applyGeometry() {
    if (disposed || minimized) return; // 胶囊的位置由 CSS 固定
    ensureContentObserver(); // 路由切换会换掉 .content 元素，几何/观察都要贴着**当前**那个
    const r = contentRect();
    let left;
    let top;
    let width;
    let height;
    if (maximized) {
      // 「最大化」= 与 content 区域逐像素重合（不是浏览器全屏）。
      left = r.left; top = r.top; width = r.width; height = r.height;
    } else {
      const inset = Math.min(ZPF_WIN_INSET, Math.max(4, Math.min(r.width, r.height) / 10));
      width = Math.max(320, r.width - inset * 2);
      height = Math.max(200, r.height - inset * 2);
      width = Math.min(width, r.width);
      height = Math.min(height, r.height);
      // 拖动范围：至少留 160px 横向、标题栏纵向留在 content 内，别把窗口拖到看不见。
      left = clamp(r.left + inset + pos.x, r.left - width + 160, r.right - 160);
      top = clamp(r.top + inset + pos.y, r.top, r.bottom - 46);
    }
    win.style.left = Math.round(left) + 'px';
    win.style.top = Math.round(top) + 'px';
    win.style.width = Math.round(width) + 'px';
    win.style.height = Math.round(height) + 'px';
  }

  function refreshCM() { requestAnimationFrame(() => { if (cm) { cm.setSize(null, '100%'); cm.refresh(); } }); }

  function setMaximized(on) {
    maximized = !!on;
    if (maximized && minimized) setMinimizedState(false);
    win.classList.toggle('zpf-win-max', maximized);
    applyGeometry();
    syncChrome();
    refreshCM();
  }

  function setMinimizedState(on) {
    minimized = !!on;
    if (minimized) maximized = false;
    win.classList.toggle('zpf-win-min', minimized);
    win.classList.toggle('zpf-win-max', maximized);
    applyGeometry();
    syncChrome();
    setDirty(dirty);
    if (!minimized) { refreshCM(); if (cm) cm.focus(); }
  }

  function syncChrome() {
    maxBtn.textContent = maximized ? '🗗' : '⛶';
    maxBtn.title = maximized ? '还原窗口（回到面板内容区里可拖动的位置）' : '最大化（铺满面板内容区）';
    maxBtn.setAttribute('aria-label', maximized ? '还原' : '最大化');
    minBtn.title = minimized ? '还原窗口' : '最小化（收成右下角胶囊，面板仍可操作）';
    minBtn.setAttribute('aria-label', minimized ? '还原' : '最小化');
    treeEl.style.display = treeHidden ? 'none' : '';
    treeResizer.style.display = treeHidden ? 'none' : '';
    if (!treeHidden) treeEl.style.width = treeWidth + 'px';
  }

  // ===================== 标题 / 状态 =====================
  function updateTitle() {
    titleText.textContent = '编辑：' + entry.name;
    titlePath.textContent = entry.path;
    win.title = entry.path;
  }

  function updateStatus() {
    if (!cm) { statusInfo.textContent = '正在加载编辑器…'; return; }
    const c = cm.getCursor();
    statusInfo.textContent = (langDef ? langDef.label : '纯文本') + ' · ' + cm.lineCount() + ' 行 · '
      + humanSize(res.size) + ' · 第 ' + (c.line + 1) + ' 行，第 ' + (c.ch + 1) + ' 列';
  }

  let unloadGuardOn = false;
  // 只要**任意一个标签**有未保存修改，关闭/刷新浏览器就要拦一次（不只是活动标签）。
  function onBeforeUnload(e) { if (!tabs.some((t) => t.dirty)) return; e.preventDefault(); e.returnValue = ''; }
  function installUnloadGuard() { if (!unloadGuardOn) { unloadGuardOn = true; window.addEventListener('beforeunload', onBeforeUnload); } }
  function removeUnloadGuard() { if (unloadGuardOn) { unloadGuardOn = false; window.removeEventListener('beforeunload', onBeforeUnload); } }
  function syncUnloadGuard() { if (tabs.some((t) => t.dirty)) installUnloadGuard(); else removeUnloadGuard(); }

  // setDirty 只作用于**活动标签**（镜像变量 dirty 也同步）。后台标签的状态由
  // saveAll / closeTab 直接改标签对象，改完调 renderTabs()。
  function setDirty(v) {
    const t = currentTab();
    const val = !!v;
    const changed = !t || t.dirty !== val;
    if (t) t.dirty = val;
    dirty = val;
    dirtyDot.textContent = '● 未保存';
    dirtyDot.style.display = val ? '' : 'none';
    syncUnloadGuard();
    if (changed) renderTabs();
  }

  // ===================== 标签栏 =====================
  // 每次重画整条标签栏（标签数量是个位数，成本可忽略）；标签过多时靠 CSS
  // overflow-x:auto 横向滚动，并把活动标签滚进可视区。
  function renderTabs() {
    if (disposed) return;
    clear(tabStrip);
    for (const t of tabs) {
      const active = t.id === activeTabId;
      const dot = h('span.zpf-filetab-dot', { text: '●', title: '有未保存的修改', style: { display: t.dirty ? '' : 'none' } });
      const nameEl = h('span.zpf-filetab-name', { text: t.entry.name, title: t.entry.path });
      const x = h('button.zpf-filetab-close', { type: 'button', text: '✕', title: '关闭这个标签（未保存会先确认）', 'aria-label': '关闭标签' });
      x.addEventListener('click', (e) => { e.stopPropagation(); closeTab(t); });
      const el = h('div.zpf-filetab' + (active ? '.active' : ''), [dot, nameEl, x]);
      el.title = t.entry.path;
      el.addEventListener('click', () => { if (t.id !== activeTabId) activateTab(t); });
      tabStrip.appendChild(el);
    }
    const act = tabStrip.querySelector('.zpf-filetab.active');
    if (act && act.scrollIntoView) act.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }

  // ===================== 菜单栏 =====================
  let openMenuIdx = -1;
  // 鼠标划过菜单标题会切换打开的菜单；此时紧接着的点击**不应该把它关掉**
  // （否则"从「文件」划到「编辑」再点一下"会把编辑菜单关了 —— 真实用户会以为点坏了）。
  let menuOpenedByHover = false;

  function closeMenus() {
    openMenuIdx = -1;
    menuOpenedByHover = false;
    menubar.querySelectorAll('.zpf-menubar-item').forEach((el) => el.classList.remove('open'));
  }

  function renderMenuPop(pop, def) {
    clear(pop);
    for (const it of def.items()) {
      if (it.sep) { pop.appendChild(h('div.zpf-menu-sep')); continue; }
      const mark = it.checked === true ? '✓ ' : (it.checked === false ? '　' : '');
      pop.appendChild(h('button.zpf-menu-row', {
        disabled: !!it.disabled,
        type: 'button',
        onclick: () => {
          closeMenus();
          try { it.run(); } catch (e) { toast('操作失败：' + ((e && e.message) || e), 'err'); }
        },
      }, [
        h('span', { text: mark + it.label }),
        it.hint ? h('span.zpf-menu-hint', { text: it.hint }) : null,
      ]));
    }
  }

  function buildMenus() {
    clear(menubar);
    menuDefs().forEach((def, idx) => {
      const title = h('button.zpf-menu-title', { text: def.label, type: 'button' });
      const pop = h('div.zpf-menu-pop');
      const item = h('div.zpf-menubar-item', [title, pop]);
      const openIt = () => { closeMenus(); item.classList.add('open'); openMenuIdx = idx; renderMenuPop(pop, def); };
      title.addEventListener('click', (e) => {
        e.stopPropagation();
        if (openMenuIdx === idx) {
          if (menuOpenedByHover) { menuOpenedByHover = false; return; } // 刚被 hover 打开，这一击不关
          closeMenus();
        } else openIt();
      });
      title.addEventListener('mouseenter', () => {
        if (openMenuIdx >= 0 && openMenuIdx !== idx) { openIt(); menuOpenedByHover = true; }
      });
      item.addEventListener('click', (e) => e.stopPropagation());
      menubar.appendChild(item);
    });
  }

  function menuDefs() {
    return [
      {
        label: '文件',
        items: () => [
          { label: '保存', hint: '⌘S', disabled: !cm, run: () => saveFile() },
          { label: '全部保存', hint: '⌘⌥S', disabled: !cm || !tabs.some((t) => t.dirty), run: saveAll },
          { label: '另存为…', hint: '⌘⇧S', disabled: !cm, run: saveAs },
          { label: '重新载入（放弃未保存修改）', disabled: !cm, run: reloadFile },
          { sep: true },
          { label: '下载文件', run: downloadFile },
          { label: '复制文件路径', run: () => copyText(entry.path) },
          { sep: true },
          { label: '关闭编辑器', run: requestClose },
        ],
      },
      {
        label: '编辑',
        items: () => [
          { label: '撤销', hint: '⌘Z', disabled: !cm, run: () => cm.execCommand('undo') },
          { label: '重做', hint: '⌘⇧Z', disabled: !cm, run: () => cm.execCommand('redo') },
          { sep: true },
          { label: '查找替换', hint: '⌘F', disabled: !cm, run: () => cm.execCommand('findPersistent') },
          { label: '跳转到行…', hint: '⌥G', disabled: !cm, run: () => cm.execCommand('jumpToLine') },
          { sep: true },
          { label: '切换注释', hint: '⌘/', disabled: !cm, run: () => cm.execCommand('toggleComment') },
          { label: '全选', hint: '⌘A', disabled: !cm, run: () => { cm.focus(); cm.execCommand('selectAll'); } },
        ],
      },
      {
        label: '视图',
        items: () => [
          { label: maximized ? '还原窗口' : '最大化', run: () => setMaximized(!maximized) },
          { label: treeHidden ? '显示目录树' : '隐藏目录树', run: toggleTree },
          { label: '刷新目录树', run: refreshTree },
          { sep: true },
          { label: '自动换行', checked: !!(cm && cm.getOption('lineWrapping')), disabled: !cm, run: toggleWrap },
          { label: '折叠全部', disabled: !cm, run: () => cm.execCommand('foldAll') },
          { label: '展开全部', disabled: !cm, run: () => cm.execCommand('unfoldAll') },
          { sep: true },
          { label: '配色：跟随面板', checked: theme === ZPF_THEME_PANEL, run: () => applyTheme(ZPF_THEME_PANEL) },
          { label: '配色：Monokai', checked: theme === ZPF_THEME_MONOKAI, run: () => applyTheme(ZPF_THEME_MONOKAI) },
          { sep: true },
          { label: fsElement() ? '退出浏览器全屏' : '浏览器全屏', run: toggleFullscreen },
        ],
      },
      {
        label: '帮助',
        items: () => [
          { label: '快捷键说明', run: showShortcuts },
          { label: '关于在线编辑器', run: showAbout },
        ],
      },
    ];
  }

  function showShortcuts() {
    const rows = [
      ['⌘ / Ctrl + S', '保存当前标签'],
      ['⌘ / Ctrl + ⌥ / Alt + S', '全部保存（依次写盘，汇总成功/失败）'],
      ['⌘ / Ctrl + ⇧ + S', '另存为'],
      ['⌘ / Ctrl + F', '查找 / 替换'],
      ['⌥ / Alt + G', '跳转到行'],
      ['⌘ / Ctrl + /', '切换注释'],
      ['Tab / ⇧ + Tab', '缩进 / 反缩进'],
      ['Esc', '关闭菜单 / 查找框（**不会**关闭编辑器）'],
      ['✕ / 菜单「关闭编辑器」', '关闭编辑器（未保存会先确认）'],
    ];
    modal({
      title: '编辑器快捷键',
      body: h('table.table', [
        h('thead', [h('tr', [h('th', { text: '按键' }), h('th', { text: '作用' })])]),
        h('tbody', rows.map((r) => h('tr', [h('td.mono', { text: r[0] }), h('td', { text: r[1] })]))),
      ]),
    });
  }

  function showAbout() {
    modal({
      title: '关于在线编辑器',
      body: h('div', { style: { fontSize: '13px', lineHeight: '1.8' } }, [
        h('p', { text: '内嵌 CodeMirror 5（MIT 许可），随面板一起分发，不依赖任何外部 CDN，也没有构建步骤。' }),
        h('p', { text: '目录树、菜单栏与窗口行为（拖动 / 最小化 / 最大化）由 ZizPanel 自己实现。' }),
      ]),
    });
  }

  // ===================== 目录树 =====================
  async function treeLoad(dir) {
    if (treeChildren.has(dir)) return treeChildren.get(dir);
    if (treeLoading.has(dir)) return [];
    treeLoading.add(dir);
    try {
      const l = await api.files(dir, false);
      treeChildren.set(dir, (l.entries || []).slice());
      return treeChildren.get(dir);
    } catch (e) {
      treeChildren.set(dir, []);
      if (!disposed) toast('目录树读取失败：' + dir + '（' + ((e && e.message) || e) + '）', 'warn', 10000);
      return [];
    } finally {
      treeLoading.delete(dir);
      if (!disposed) treeRender();
    }
  }

  function sortedKids(list) {
    const arr = list.slice();
    arr.sort((a, b) => {
      if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
      return String(a.name || '').localeCompare(String(b.name || ''), 'zh');
    });
    return arr;
  }

  function treeRow(e, depth) {
    const active = !e.is_dir && e.path === entry.path;
    const isCurDir = e.is_dir && e.path === curDir;
    const row = h('div.zpf-tree-row' + (active ? '.active' : '') + (isCurDir ? '.current-dir' : ''), {
      style: { paddingLeft: (6 + depth * 12) + 'px' },
      dataset: { path: e.path },
      title: e.path,
    });
    row.appendChild(h('span.zpf-tree-caret', {
      text: e.is_dir ? (treeExpanded.has(e.path) ? '▾' : '▸') : '',
      onclick: (ev) => { if (!e.is_dir) return; ev.stopPropagation(); toggleDir(e.path); },
    }));
    row.appendChild(h('span', { text: e.is_dir ? (treeExpanded.has(e.path) ? '📂' : '📁') : fileIcon(e.name) }));
    row.appendChild(h('span.zpf-tree-name', { text: e.name }));
    row.addEventListener('click', () => {
      if (e.is_dir) {
        treeExpanded.add(e.path);
        curDir = e.path;
        treeRender();
        treeLoad(e.path);
        onDir(e.path); // 切换当前浏览目录（背后的文件列表跟着走）
      } else {
        openFileByTree(e.path, e.name);
      }
    });
    return row;
  }

  function treeRender() {
    if (disposed) return;
    clear(treeScroll);
    const nodes = [h('div.zpf-tree-row.zpf-tree-root', { title: treeRoot }, [
      h('span.zpf-tree-caret', {
        text: treeExpanded.has(treeRoot) ? '▾' : '▸',
        onclick: (ev) => { ev.stopPropagation(); toggleDir(treeRoot); },
      }),
      h('span', { text: '🗂️' }),
      h('span.zpf-tree-name', { text: basename(treeRoot) || treeRoot, style: { fontWeight: '600' } }),
    ])];
    const walk = (dir, depth) => {
      for (const e of sortedKids(treeChildren.get(dir) || [])) {
        nodes.push(treeRow(e, depth));
        if (e.is_dir && treeExpanded.has(e.path)) walk(e.path, depth + 1);
      }
    };
    if (treeExpanded.has(treeRoot)) walk(treeRoot, 1);
    appendAll(treeScroll, nodes);
    locateCurrent();
  }

  function toggleDir(path) {
    if (treeExpanded.has(path)) treeExpanded.delete(path);
    else { treeExpanded.add(path); if (!treeChildren.has(path)) treeLoad(path); }
    treeRender();
  }

  function locateCurrent() {
    const el = treeScroll.querySelector('.zpf-tree-row[data-path="' + cssEsc(entry.path) + '"]');
    if (el) el.scrollIntoView({ block: 'nearest' });
  }

  // revealFile 把当前文件在树里定位出来：展开祖先目录 → 高亮 → 滚到可见处。
  async function revealFile(file) {
    const d = dirname(file);
    const rootPrefix = treeRoot.replace(/\/+$/, '');
    const chain = [];
    let cur = d;
    while (cur && (cur === treeRoot || cur.startsWith(rootPrefix + '/'))) {
      chain.unshift(cur);
      if (cur === treeRoot) break;
      const parent = dirname(cur);
      if (parent === cur) break;
      cur = parent;
    }
    for (const dir of chain) {
      treeExpanded.add(dir);
      if (!treeChildren.has(dir)) await treeLoad(dir);
    }
    treeRender();
  }

  function refreshTree() {
    treeChildren.clear();
    treeLoad(treeRoot).then(() => revealFile(entry.path));
  }

  function toggleTree() {
    treeHidden = !treeHidden;
    writeLS(ZPF_TREE_HIDDEN_KEY, treeHidden ? '1' : '0');
    syncChrome();
    refreshCM();
  }

  // ===================== 打开 / 切换文件（多标签） =====================
  // activateTab 把活动标签切到 t：先把镜像变量写回旧标签，再把镜像换成 t 的值，
  // 最后让 CodeMirror 显示 t 的 Doc（swapDoc 会保留该标签自己的撤销历史/光标/滚动）。
  async function activateTab(t) {
    if (!t || disposed) return;
    snapshotActive();
    activeTabId = t.id;
    entry = t.entry;
    res = t.res;
    dirty = t.dirty;
    langKey = t.langKey;
    langDef = t.langDef;
    curDir = dirname(entry.path);
    renderTabs();
    // 语言模式必须先就绪：模式文件没加载完就建 Doc，CodeMirror 会**静默**退化成纯文本。
    try { await ensureLang(langKey); } catch (e) { toast('语言模式加载失败：' + ((e && e.message) || e), 'warn', 8000); }
    if (disposed) return;
    langDef = CM_LANGS[langKey];
    if (cm) {
      if (!t.doc) t.doc = window.CodeMirror.Doc(res.content, langDef ? langDef.mode : null);
      cm.swapDoc(t.doc);
      setDirty(!!t.dirty);
      refreshCM();
      if (!minimized) requestAnimationFrame(() => { if (cm) cm.focus(); });
    }
    updateTitle();
    updateStatus();
    revealFile(entry.path);
  }

  // addTab 打开一个新标签（已经打开过的路径就切过去，不重复开）。
  async function addTab(e, r) {
    const exist = tabForPath(e.path);
    if (exist) { await activateTab(exist); return; }
    const t = mkTab(e, r);
    tabs.push(t);
    await activateTab(t);
  }

  async function openFileByTree(path, name) {
    const exist = tabForPath(path);
    if (exist) { await activateTab(exist); return; }
    let r;
    try { r = await api.fileRead(path); }
    catch (e) { toast('打开失败：' + ((e && e.message) || e), 'err', 10000); return; }
    if (r.binary) { toast('这是二进制文件，不能用文本编辑器打开', 'warn', 8000); return; }
    if (r.too_large) { toast('文件过大（' + humanSize(r.size) + '），超过在线编辑上限', 'warn', 10000); return; }
    await addTab({ path, name: name || basename(path) }, r);
  }

  // applyFile 把**当前标签**换成另一个文件内容（重新载入 / 另存为后的路径更新）。
  async function applyFile(newEntry, newRes) {
    const t = currentTab();
    entry = newEntry;
    res = newRes;
    const key = langKeyFor(entry.name);
    const langChanged = key !== langKey;
    langKey = key;
    langDef = CM_LANGS[key];
    if (langChanged) {
      try { await ensureLang(langKey); } catch (e) { toast('语言模式加载失败：' + ((e && e.message) || e), 'warn', 8000); }
    }
    curDir = dirname(entry.path);
    if (t) { t.entry = entry; t.res = res; t.langKey = langKey; t.langDef = langDef; }
    if (cm) {
      cm.setValue(res.content);
      cm.clearHistory();
      cm.setCursor(0, 0);
      if (langChanged) cm.setOption('mode', langDef ? langDef.mode : null);
      if (t) t.cleanGen = cm.changeGeneration();
      setDirty(false);
      renderTabs();
      refreshCM();
      requestAnimationFrame(() => { if (cm) cm.focus(); });
    }
    updateTitle();
    updateStatus();
    revealFile(entry.path);
  }

  // ===================== 保存 / 下载 =====================
  async function saveFile() {
    const t = currentTab();
    if (!cm || !t) return false;
    try {
      await api.fileWrite(t.entry.path, t.doc.getValue());
      if (t.id === activeTabId) { t.cleanGen = cm.changeGeneration(); setDirty(false); }
      else t.dirty = false;
      t.res = Object.assign({}, t.res, { size: t.doc.getValue().length });
      if (t.id === activeTabId) updateStatus();
      renderTabs();
      toast('已保存 ' + basename(t.entry.path), 'ok');
      refreshList();
      return true;
    } catch (e) {
      toast('保存失败：' + ((e && e.message) || e), 'err', 12000);
      return false;
    }
  }

  // saveAll 依次把**所有未保存**的标签写盘，最后汇总：成功几个、哪个失败、原因是什么。
  // 失败不中断（一个文件权限不对不该让其余文件也存不了），但必须逐个说清楚。
  async function saveAll() {
    if (!cm) return;
    const pending = tabs.filter((t) => t.dirty);
    if (!pending.length) { toast('没有未保存的修改', 'warn', 4000); return; }
    let ok = 0;
    const fails = [];
    for (const t of pending) {
      try {
        await api.fileWrite(t.entry.path, t.doc.getValue());
        if (t.id === activeTabId) t.cleanGen = cm.changeGeneration();
        t.dirty = false;
        t.res = Object.assign({}, t.res, { size: t.doc.getValue().length });
        ok++;
      } catch (e) {
        fails.push({ path: t.entry.path, msg: (e && e.message) || String(e) });
      }
    }
    if (tabs.length && currentTab()) setDirty(!!currentTab().dirty);
    syncUnloadGuard();
    renderTabs();
    updateStatus();
    refreshList();
    const skipped = tabs.length - pending.length;
    if (!fails.length) {
      toast(`全部保存完成：成功写入 ${ok} 个文件` + (skipped ? `（另有 ${skipped} 个没有改动）` : ''), 'ok', 6000);
      return;
    }
    modal({
      title: '全部保存结果',
      body: h('div', [
        h('p', { text: `成功 ${ok} 个，失败 ${fails.length} 个` + (skipped ? `（另有 ${skipped} 个没有改动）` : '') + '。' }),
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '文件' }), h('th', { text: '失败原因' })])]),
          h('tbody', fails.map((f) => h('tr', [
            h('td.mono', { style: { fontSize: '11.5px' }, text: f.path }),
            h('td', { style: { color: 'var(--danger)' }, text: f.msg }),
          ]))),
        ]),
      ]),
    });
  }

  async function saveAs() {
    if (!cm) return;
    const cur = currentTab();
    const dest = await promptBox({
      title: '另存为', label: '目标完整路径', value: entry.path,
      hint: '必须在文件管理允许的目录内（绝对路径）',
    });
    if (!dest || dest === entry.path) return;
    const content = cm.getValue();
    try {
      // 后端 /files/write 只写**已存在**的文件（不新建），所以"另存为新路径"要先 touch。
      // touch 报"已存在同名文件"时问一句是否覆盖 —— 覆盖不可逆。
      let exists = false;
      try {
        await api.fileTouch(dest);
      } catch (e) {
        exists = /已存在/.test((e && e.message) || '');
      }
      if (exists) {
        const yes = await confirmBox(`「${dest}」已存在，要用当前内容覆盖它吗？`, {
          title: '覆盖已存在的文件', danger: true, okText: '覆盖',
        });
        if (!yes) return;
      }
      await api.fileWrite(dest, content);
      entry = { path: dest, name: basename(dest) };
      res = Object.assign({}, res, { size: content.length });
      if (cur) {
        cur.entry = entry;
        cur.res = res;
        cur.cleanGen = cm.changeGeneration();
      }
      setDirty(false);
      renderTabs();
      updateTitle();
      updateStatus();
      toast('已另存为 ' + dest, 'ok');
      revealFile(entry.path);
      refreshList();
    } catch (e) {
      toast('另存为失败：' + ((e && e.message) || e), 'err', 12000);
    }
  }

  async function reloadFile() {
    if (!confirmDiscard()) return;
    try {
      const r = await api.fileRead(entry.path);
      if (r.binary || r.too_large) { toast('该文件已不能在线编辑（二进制或过大）', 'warn', 8000); return; }
      await applyFile(entry, r);
      toast('已重新载入', 'ok');
    } catch (e) { toast('重新载入失败：' + ((e && e.message) || e), 'err', 10000); }
  }

  function downloadFile() { window.location.href = api.fileDownloadURL(entry.path); }

  function copyText(t) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(t)
        .then(() => toast('已复制：' + t, 'ok', 5000))
        .catch(() => toast('复制失败，请手动选择：' + t, 'warn', 8000));
    } else {
      toast(t, 'ok', 8000);
    }
  }

  function toggleWrap() {
    if (!cm) return;
    cm.setOption('lineWrapping', !cm.getOption('lineWrapping'));
    refreshCM();
  }

  // ===================== 关闭 =====================
  function confirmDiscard() {
    if (!dirty) return true;
    return confirm('「' + entry.name + '」有未保存的修改，确定放弃这些修改？');
  }

  // closeTab 关闭单个标签：有未保存修改先确认；关掉最后一个标签 = 关闭整个编辑器。
  function closeTab(t) {
    const idx = tabs.indexOf(t);
    if (idx < 0 || disposed) return;
    if (t.dirty && !confirm('「' + t.entry.name + '」有未保存的修改，确定关闭这个标签？')) return;
    const wasActive = t.id === activeTabId;
    tabs.splice(idx, 1);
    if (!tabs.length) { dispose(); return; }
    if (wasActive) {
      activateTab(tabs[Math.min(idx, tabs.length - 1)]);
    } else {
      syncUnloadGuard();
      renderTabs();
    }
  }

  // requestClose 关闭整个编辑器（✕ 或菜单「文件 → 关闭编辑器」）。只要**任一**标签
  // 有未保存修改就先确认，并把是哪些文件说清楚。
  function requestClose() {
    const dirtyTabs = tabs.filter((t) => t.dirty);
    if (dirtyTabs.length) {
      const names = dirtyTabs.map((t) => t.entry.name).join('、');
      if (!confirm('有未保存的修改（' + names + '），确定关闭编辑器？')) return;
    }
    dispose();
  }

  // Esc 只关菜单；**不关编辑器**（用户要求 3）。查找/跳行框由 CodeMirror 的
  // dialog 插件自己处理（它收到 Esc 会 close 并 stopPropagation），所以这里只补菜单。
  function onDocKeyDown(e) {
    if (disposed || e.key !== 'Escape') return;
    if (openMenuIdx >= 0) closeMenus();
  }
  document.addEventListener('keydown', onDocKeyDown);

  function dispose() {
    if (disposed) return;
    disposed = true;
    removeUnloadGuard();
    document.removeEventListener('mousedown', onDocDown);
    document.removeEventListener('keydown', onDocKeyDown);
    document.removeEventListener('fullscreenchange', onFsChange);
    document.removeEventListener('webkitfullscreenchange', onFsChange);
    window.removeEventListener('resize', applyGeometry);
    window.removeEventListener('hashchange', onRouteChange);
    if (resizeObs) { try { resizeObs.disconnect(); } catch { /* 忽略 */ } }
    themeObserver.disconnect();
    layer.remove();
    onClosed();
  }

  // ===================== 主题 =====================
  async function applyTheme(v) {
    theme = v === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
    saveEditorTheme(theme);
    const seq = ++themeSeq;
    const name = cmThemeFor(theme);
    try {
      await ensureCmTheme(name);
    } catch (e) {
      toast('编辑器主题加载失败：' + ((e && e.message) || e), 'err');
      return;
    }
    if (seq !== themeSeq || disposed) return;
    if (cm) {
      cm.setOption('theme', name);
      requestAnimationFrame(() => { if (cm) cm.refresh(); });
    }
  }

  const themeObserver = new MutationObserver(() => {
    if (theme === ZPF_THEME_PANEL) applyTheme(ZPF_THEME_PANEL);
  });
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  // ===================== 拖动 / 缩放 =====================
  function startDrag(e) {
    if (e.button !== 0 || minimized) return;
    const t = e.target;
    if (t && t.closest && t.closest('button, a, select, input, textarea, .zpf-menu-pop')) return;
    closeMenus();
    if (maximized) setMaximized(false); // 拖最大化窗口 = 先还原再拖（与系统窗口一致）
    e.preventDefault();
    const rect = win.getBoundingClientRect();
    const sx = e.clientX;
    const sy = e.clientY;
    const ol = rect.left;
    const ot = rect.top;
    const move = (ev) => {
      const r = contentRect();
      const w = win.offsetWidth;
      const h = win.offsetHeight;
      let left = ol + (ev.clientX - sx);
      let top = ot + (ev.clientY - sy);
      // 关键约束：窗口**始终留在 content 区域内**（用户要求 f：任何状态下都充满内容区、
      // 不留大片空白）。还原态只比 content 内缩 14px，所以可拖范围不大，但位置会被记住。
      left = clamp(left, r.left, Math.max(r.left, r.right - w));
      top = clamp(top, r.top, Math.max(r.top, r.bottom - h));
      win.style.left = Math.round(left) + 'px';
      win.style.top = Math.round(top) + 'px';
      pos = { x: Math.round(left - (r.left + ZPF_WIN_INSET)), y: Math.round(top - (r.top + ZPF_WIN_INSET)) };
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      document.body.classList.remove('zpf-dragging');
      writeLS(ZPF_POS_KEY, JSON.stringify(pos));
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
    document.body.classList.add('zpf-dragging');
  }

  titlebar.addEventListener('mousedown', startDrag);
  menubar.addEventListener('mousedown', startDrag);

  // 最小化后整条标题栏就是"还原"热区（宝塔手感）；按钮自己已处理，别重复触发。
  titlebar.addEventListener('click', (e) => {
    if (!minimized) return;
    if (e.target && e.target.closest && e.target.closest('button')) return;
    setMinimizedState(false);
  });

  treeResizer.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const sx = e.clientX;
    const w0 = treeWidth;
    const move = (ev) => {
      treeWidth = clamp(w0 + (ev.clientX - sx), 150, 460);
      treeEl.style.width = treeWidth + 'px';
      if (cm) cm.refresh();
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      document.body.classList.remove('zpf-resizing');
      writeLS(ZPF_TREE_W_KEY, treeWidth);
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
    document.body.classList.add('zpf-resizing');
  });
  treeResizer.addEventListener('dblclick', () => {
    treeWidth = 230;
    treeEl.style.width = '230px';
    writeLS(ZPF_TREE_W_KEY, 230);
    if (cm) cm.refresh();
  });

  minBtn.addEventListener('click', (e) => { e.stopPropagation(); setMinimizedState(!minimized); });
  maxBtn.addEventListener('click', (e) => { e.stopPropagation(); setMaximized(!maximized); });
  closeBtn.addEventListener('click', (e) => { e.stopPropagation(); requestClose(); });

  function onDocDown(e) { if (!win.contains(e.target)) closeMenus(); }
  document.addEventListener('mousedown', onDocDown);

  // ===================== 浏览器全屏（既有能力，收进「视图」菜单） =====================
  function fsElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }

  function toggleFullscreen() {
    if (fsElement()) {
      const exit = document.exitFullscreen || document.webkitExitFullscreen;
      if (exit) { const p = exit.call(document); if (p && p.catch) p.catch(() => {}); }
      return;
    }
    const req = win.requestFullscreen || win.webkitRequestFullscreen;
    if (!req) { toast('当前浏览器不支持屏幕全屏', 'warn'); return; }
    const p = req.call(win);
    if (p && p.catch) p.catch((e) => toast('进入屏幕全屏失败：' + ((e && e.message) || e), 'err'));
  }

  function onFsChange() { refreshCM(); }
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('webkitfullscreenchange', onFsChange);

  // ===================== 自适应 =====================
  // 侧栏折叠 / 展开只改变 content 的宽度，**不触发 window.resize**，所以必须观察 content 本身。
  //
  // 编辑器是全局存活的，而每次路由切换都会重建 `.content` 元素 —— 旧观察目标会脱离
  // 文档树（resize 再也不触发）。ensureContentObserver 负责在目标失效时改观察新的那个，
  // applyGeometry 每次都会调它，所以切换板块后点胶囊还原 / 最大化都能拿到正确矩形。
  let resizeObs = null;
  let contentObsTarget = null;
  function ensureContentObserver() {
    if (!window.ResizeObserver) return;
    const c = document.querySelector('.content');
    if (!c || (contentObsTarget === c && c.isConnected)) return;
    if (!resizeObs) resizeObs = new ResizeObserver(() => applyGeometry());
    else { try { resizeObs.disconnect(); } catch { /* 忽略 */ } }
    contentObsTarget = c;
    resizeObs.observe(c);
  }
  ensureContentObserver();
  window.addEventListener('resize', applyGeometry);
  function onRouteChange() { applyGeometry(); }
  window.addEventListener('hashchange', onRouteChange);

  // ===================== 初始化 =====================
  updateTitle();
  syncChrome();
  setDirty(false);
  renderTabs();
  buildMenus();
  applyGeometry();

  (async () => {
    try { await treeLoad(treeRoot); } catch { /* 已在 treeLoad 里提示 */ }
    await revealFile(entry.path);
  })();

  (async () => {
    try {
      await ensureCodeMirror();
      await ensureLang(langKey);
      await ensureCmTheme(cmThemeFor(theme));
      const themeName = cmThemeFor(theme);
      cm = window.CodeMirror(host, {
        value: '',
        mode: null,
        theme: themeName,
        lineNumbers: true,
        lineWrapping: false,
        indentUnit: 4,
        tabSize: 4,
        indentWithTabs: false,
        smartIndent: true,
        electricChars: true,
        autoCloseBrackets: true,
        matchBrackets: true,
        styleActiveLine: true,
        foldGutter: true,
        gutters: ['CodeMirror-linenumbers', 'CodeMirror-foldgutter'],
        scrollbarStyle: 'simple',
        viewportMargin: 30,
        extraKeys: {
          'Ctrl-S': () => { saveFile(); },
          'Cmd-S': () => { saveFile(); },
          'Ctrl-Alt-S': () => { saveAll(); },
          'Cmd-Alt-S': () => { saveAll(); },
          'Shift-Ctrl-S': () => { saveAs(); },
          'Shift-Cmd-S': () => { saveAs(); },
          'Ctrl-F': 'findPersistent',
          'Cmd-F': 'findPersistent',
          'Shift-Ctrl-F': 'replace',
          'Shift-Cmd-F': 'replace',
          'Alt-G': 'jumpToLine',
          'Ctrl-/': 'toggleComment',
          'Cmd-/': 'toggleComment',
          // 这里**故意没有 Esc**（用户要求 3）：按 Esc 不许关编辑器。Esc 只用于关菜单
          // （见 onDocKeyDown）和关 CodeMirror 的查找/跳行框（dialog 插件自带，且它会
          // stopPropagation）。关编辑器只能点 ✕ 或菜单「文件 → 关闭编辑器」。
          // Tab / Shift+Tab 必须显式接管：CodeMirror 默认的 Tab 在空行上会插入
          // **制表符**，而面板其它地方（以及面板自己生成的配置）统一用 4 空格 ——
          // 混用会在保存后的文件里留下看不见的差异。有选区时整块缩进/反缩进。
          'Tab': (ed) => {
            if (ed.somethingSelected()) ed.indentSelection('add');
            else ed.replaceSelection('    ', 'end');
          },
          'Shift-Tab': (ed) => ed.indentSelection('subtract'),
        },
        // 查找/替换对话框的文案（CodeMirror 自带的是英文；面板是中文界面）
        // 键名必须与 CodeMirror 插件的 phrase() 调用逐字一致（含冒号），
        // 否则查表失败会**静默**退回英文 —— 第一版就踩了（写的是 'Search'）。
        phrases: {
          'Search:': '查找：',
          'Replace:': '替换：',
          'Replace with:': '替换为：',
          'Replace all:': '全部替换：',
          'Replace?': '要替换吗？',
          'With:': '替换为：',
          'Jump to line:': '跳到行：',
          'All': '全部',
          'Stop': '停止',
          'Yes': '是',
          'No': '否',
          '(Use /re/ syntax for regexp search)': '（支持 /正则/ 语法）',
          '(Use line:column or scroll% syntax)': '（可用 行:列 或 百分比）',
        },
      });
      // 初始标签的 Doc（每个标签一份 Doc => 各自独立的撤销历史/光标/滚动）
      const t0 = currentTab();
      if (t0 && !t0.doc) t0.doc = window.CodeMirror.Doc(res.content, langDef ? langDef.mode : null);
      if (t0) cm.swapDoc(t0.doc);
      cm.setCursor(0, 0);
      cm.clearHistory();
      if (t0) t0.cleanGen = cm.changeGeneration();
      // 脏标记用"与上次干净时的变更代数比较"，而不是收到 change 就置脏：
      // 程序化的 setValue / swapDoc / clearHistory 也会触发 change，靠事件置脏会假报未保存。
      cm.on('change', () => {
        const t = currentTab();
        if (!t) return;
        setDirty(!cm.isClean(t.cleanGen));
      });
      cm.on('cursorActivity', updateStatus);
      setDirty(false);
      updateStatus();
      renderTabs();
      cm.focus();
      buildMenus(); // 编辑器就绪后菜单项从禁用变可用
      refreshCM();
      // 挂载是异步的：如果这期间用户切了配色（或面板深浅色变了），按最新选择纠正一次
      if (cmThemeFor(theme) !== themeName) applyTheme(theme);
    } catch (e) {
      // 加载失败要给出**原因**（网络/镜像/文件缺失），并且仍然给一条能走通的路
      clear(host);
      appendAll(host, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '编辑器加载失败' }),
        h('p', { text: (e && e.message) || String(e) }),
        h('p', { text: '可以刷新页面重试；也可以用 Web 终端或下载后本地编辑。文件内容没有被改动。' }),
      ]));
      statusInfo.textContent = '';
    }
  })();

  return {
    // openFile 打开文件：已打开则切到那个标签，否则新开一个标签。
    openFile: (e, r) => addTab(e, r),
    // setFile 保留旧语义：把**当前标签**换成另一个文件（内部重新载入等场景）。
    setFile: (e, r) => applyFile(e, r),
    // setOptions 让重新创建的 FilesView 把回调重新绑到当前 DOM 上（编辑器跨路由存活）。
    setOptions: (o = {}) => {
      if (Array.isArray(o.roots)) roots = o.roots;
      if (typeof o.refreshList === 'function') refreshList = o.refreshList;
      if (typeof o.onDir === 'function') onDir = o.onDir;
    },
    focus: () => { if (cm) cm.focus(); },
    dispose,
  };
}

// ---------- 拖拽：把 DataTransfer 展开成 {file, rel} ----------
//
// 只有 webkitGetAsEntry 能拿到目录与相对路径；Entry.file() / readEntries() 都是
// 回调式 API，这里包成 Promise 好按顺序走完。
// readEntries 一次**最多只返回一批**（Chrome 约 100 个），必须反复调用到返回空数组
// —— 只调一次会静默丢掉后面的文件（"文件夹传了一半"就是这么来的）。

function readAllEntries(reader) {
  return new Promise((resolve, reject) => {
    const all = [];
    const next = () => {
      reader.readEntries((batch) => {
        if (!batch.length) { resolve(all); return; }
        all.push(...batch);
        next();
      }, reject);
    };
    next();
  });
}

function entryFile(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject));
}

async function walkEntry(entry, prefix, out) {
  if (!entry) return;
  if (entry.isFile) {
    const file = await entryFile(entry);
    out.push({ file, rel: prefix + entry.name });
    return;
  }
  if (entry.isDirectory) {
    const kids = await readAllEntries(entry.createReader());
    for (const k of kids) await walkEntry(k, prefix + entry.name + '/', out);
  }
}

// ---------- 小工具 ----------
function basename(p) { return String(p).split('/').filter(Boolean).pop() || '/'; }function dirname(p) { const parts = String(p).split('/'); parts.pop(); return parts.join('/') || '/'; }

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2) + ' ' + units[i];
}

function fileIcon(name) {
  const ext = String(name).split('.').pop().toLowerCase();
  const map = {
    php: '🐘', js: '📜', ts: '📘', json: '🧾', css: '🎨', html: '🌐', htm: '🌐',
    md: '📝', txt: '📄', log: '📋', sql: '🗃️', sh: '⚙️', yml: '⚙️', yaml: '⚙️',
    png: '🖼️', jpg: '🖼️', jpeg: '🖼️', gif: '🖼️', svg: '🖼️', webp: '🖼️',
    zip: '📦', gz: '📦', tar: '📦', pdf: '📕', mp4: '🎬', mp3: '🎵',
  };
  return map[ext] || '📄';
}

function readCookie(name) {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}

void esc;
