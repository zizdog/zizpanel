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

let cwd = '';
let showHidden = false;
let selection = new Set();
let lastList = null;

export function FilesView(content, ctx = {}) {
  clear(content);
  selection = new Set();

  const crumbs = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap', fontSize: '13px' } });
  const toolbar = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' } });
  const tableBox = h('div', { style: { overflowX: 'auto', minHeight: '260px' } });
  const statusBar = h('div', { style: { fontSize: '12px', color: 'var(--text-mute)', padding: '8px 14px', borderTop: '1px solid var(--border-soft)' } });

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
    clear(tableBox);
    appendAll(tableBox, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取目录…' })]));
    try {
      lastList = await api.files(cwd, showHidden);
    } catch (e) {
      clear(tableBox);
      appendAll(tableBox, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '无法打开该目录' }),
        h('p', { text: e.message }),
      ]));
      return;
    }
    cwd = lastList.path;
    renderCrumbs();
    renderToolbar();
    renderTable();
  }

  function renderCrumbs() {
    clear(crumbs);
    const roots = lastList?.roots || [];
    const root = roots.find((r) => cwd === r || cwd.startsWith(r + '/'));
    const parts = [];

    // 根目录选择器（有多个白名单根时显示下拉）
    if (roots.length > 1) {
      const sel = h('select.select', {
        style: { width: 'auto', padding: '3px 26px 3px 8px', fontSize: '12.5px' },
        onchange: (e) => load(e.target.value),
      }, roots.map((r) => h('option', { value: r, text: r, selected: r === root })));
      parts.push(sel, h('span', { style: { color: 'var(--text-mute)' }, text: '/' }));
    }
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

  function previewImage(entry) {
    const url = api.fileDownloadURL(entry.path);
    const m = modal({
      title: '🖼️ ' + entry.name,
      wide: true,
      body: h('div', { style: { textAlign: 'center' } }, [
        h('img', {
          src: url, alt: entry.name,
          style: { maxWidth: '100%', maxHeight: '68vh', borderRadius: '6px', background: 'var(--panel-2)' },
          onerror: () => toast('图片加载失败（可能不是浏览器支持的格式）', 'warn', 8000),
        }),
        h('div.hint', { style: { marginTop: '8px' }, text: `${humanSize(entry.size)} · ${entry.path}` }),
      ]),
      footer: () => [
        h('button.btn', { text: '下载', onclick: () => { window.location.href = url; } }),
        h('button.btn', { text: '仍然用文本编辑器打开', onclick: () => { m.close(); openEditor(entry); } }),
        h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() }),
      ],
    });
  }

  function renderToolbar() {
    clear(toolbar);
    const selCount = selection.size;
    appendAll(toolbar, 
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(cwd) }),
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
      h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px', cursor: 'pointer' } }, [
        h('input', {
          type: 'checkbox', checked: showHidden,
          onchange: (e) => { showHidden = e.target.checked; load(cwd); },
        }),
        h('span', { text: '显示隐藏文件' }),
      ]),
      selCount > 0 ? h('div', { style: { flex: 1 } }) : h('div', { style: { flex: 1 } }),
      selCount > 0 ? h('span.pill.brand', { text: `已选 ${selCount} 项` }) : null,
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
      appendAll(tableBox, h('div.empty', [
        h('div.big', { text: '📂' }),
        h('h4', { text: '这个目录是空的' }),
        h('p', { text: '可以用上方按钮上传文件或新建内容。' }),
      ]));
      updateStatus();
      return;
    }

    const allChecked = list.every((e) => selection.has(e.path));
    const head = h('tr', [
      h('th', { style: { width: '34px' } }, [
        h('input', {
          type: 'checkbox', checked: allChecked,
          onchange: (e) => {
            selection.clear();
            if (e.target.checked) list.forEach((x) => selection.add(x.path));
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

    const rows = sortedEntries(list).map((e) => {
      const selected = selection.has(e.path);
      return h('tr', {
        style: selected ? { background: 'var(--brand-soft)' } : {},
        // 双击行 = 打开（目录进入 / 文件编辑或预览）；单击仍然是勾选/点链接。
        // 这是所有文件管理器的通用肌肉记忆，之前只有"编辑"按钮能点。
        ondblclick: (ev) => {
          if (ev.target && ev.target.tagName === 'INPUT') return; // 别抢勾选框
          if (e.is_dir) load(e.path); else openAny(e);
        },
      }, [
        h('td', [
          h('input', {
            type: 'checkbox', checked: selected,
            onchange: (ev) => {
              if (ev.target.checked) selection.add(e.path); else selection.delete(e.path);
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
                title: isImage(e) ? '点击预览' : '点击编辑（双击也可以）',
                onclick: () => openAny(e),
              }),
            e.symlink ? h('span.pill', { text: '链接', title: '指向 ' + (e.symlink_target || '?') }) : null,
            e.read_only ? h('span.pill.warn', { text: '只读' }) : null,
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
                text: isImage(e) ? '预览' : '编辑',
                onclick: () => openAny(e),
              }),
            h('button.btn.btn-sm', {
              text: '下载',
              disabled: e.is_dir,
              onclick: () => { window.location.href = api.fileDownloadURL(e.path); },
            }),
            h('button.btn.btn-sm', { text: '⋯', title: '更多操作', onclick: () => moreMenu(e) }),
          ]),
        ]),
      ]);
    });

    appendAll(tableBox, h('table.table', [h('thead', [head]), h('tbody', rows)]));
    updateStatus();
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
      text: '修改权限',
      onclick: async () => {
        m.close();
        const mode = await promptBox({
          title: '修改权限', label: '八进制权限', value: String(e.mode_num.toString(8)).padStart(3, '0'),
          hint: '例如 644（文件）、755（目录/可执行）',
        });
        if (!mode) return;
        const recursive = (await confirmBox('是否同时递归修改该目录下的所有内容？', {
          title: '递归修改权限', okText: '递归修改',
        }));
        try {
          const r = await api.fileChmod(e.path, mode, e.is_dir && recursive);
          toast(r.msg || '权限已修改', 'ok'); load(cwd);
        } catch (err) { toast(err.message, 'err'); }
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

  // ---------- 删除 ----------
  async function deleteOne(e) {
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

  async function deleteSelected() {
    const paths = [...selection];
    const hasDir = (lastList?.entries || []).some((e) => selection.has(e.path) && e.is_dir);
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

    editorModal(entry, res);
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

  // 键盘：Enter 打开选中项、Delete 删除选中项、Esc 取消选择。
  // 输入框/弹窗里有焦点时一律不接管（否则在搜索框里按 Delete 会删文件）。
  function onKeyDown(e) {
    const tag = (document.activeElement && document.activeElement.tagName) || '';
    if (/^(INPUT|TEXTAREA|SELECT)$/.test(tag)) return;
    if (document.querySelector('.modal-mask')) return;
    if (e.key === 'Escape' && selection.size) {
      selection.clear(); renderToolbar(); renderTable(); return;
    }
    if ((e.key === 'Delete' || e.key === 'Backspace') && selection.size) {
      e.preventDefault(); deleteSelected(); return;
    }
    if (e.key === 'Enter' && selection.size) {
      const list = lastList?.entries || [];
      const first = list.find((x) => selection.has(x.path));
      if (!first) return;
      e.preventDefault();
      if (first.is_dir) load(first.path); else openAny(first);
    }
  }
  document.addEventListener('keydown', onKeyDown);
  registerCleanup(() => { document.removeEventListener('keydown', onKeyDown); });
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
// 两档：跟随面板（默认）/ Monokai。键名沿用旧版：用户已经选过的偏好不该丢。
const ZPF_THEME_KEY = 'zp-file-editor-theme';
const ZPF_THEME_PANEL = 'panel';
const ZPF_THEME_MONOKAI = 'monokai';
const ZPF_CSS_ID = 'zpf-editor-style';

function readEditorTheme() {
  try {
    return localStorage.getItem(ZPF_THEME_KEY) === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
  } catch { return ZPF_THEME_PANEL; }
}

function saveEditorTheme(v) {
  try { localStorage.setItem(ZPF_THEME_KEY, v); } catch { /* 存不了就本次会话生效 */ }
}

function applyEditorThemeTo(el, v) {
  if (el) el.classList.toggle('zpf-monokai', v === ZPF_THEME_MONOKAI);
}

// ensureEditorStyle 注入编辑器自己的样式（只注入一次）。
//
// 面板主题变量（--text / --panel-2 / …）两套主题都定义好了，这里只把 CodeMirror
// 的类名映射过去，所以浅色/深色/Monokai 三种外观都不需要各写一份。
function ensureEditorStyle() {
  if (document.getElementById(ZPF_CSS_ID)) return;
  const st = document.createElement('style');
  st.id = ZPF_CSS_ID;
  st.textContent = [
    // 编辑器高度铺满外层容器（CodeMirror 需要一个有高度的父元素）
    '.zpf-cm { position: relative; display: flex; flex-direction: column; min-height: 0; }',
    '.zpf-cm .CodeMirror { flex: 1 1 auto; height: auto; min-height: 0; font-family: var(--mono); font-size: 13px; line-height: 1.6; background: var(--bg-soft); color: var(--text); }',
    '.zpf-cm .CodeMirror-gutters { background: var(--panel-2); border-right: 1px solid var(--border-soft); }',
    '.zpf-cm .CodeMirror-linenumber { color: var(--text-mute); }',
    '.zpf-cm .CodeMirror-cursor { border-left: 2px solid var(--text); }',
    '.zpf-cm .CodeMirror-selected { background: rgba(96,165,250,.30) !important; }',
    '.zpf-cm .CodeMirror-activeline-background { background: rgba(127,127,127,.08); }',
    '.zpf-cm .CodeMirror-matchingbracket { color: #16a34a !important; font-weight: 700; }',
    '.zpf-cm .CodeMirror-nonmatchingbracket { color: #dc2626 !important; }',
    '.zpf-cm .CodeMirror-foldmarker { color: var(--brand); }',
    // 面板主题下的语法着色（沿用旧版配色，视觉上与升级前一致）
    '.cm-s-zp-panel .cm-comment { color: #6b7688; font-style: italic; }',
    '.cm-s-zp-panel .cm-string, .cm-s-zp-panel .cm-string-2 { color: #86d99a; }',
    '.cm-s-zp-panel .cm-number { color: #f0a868; }',
    '.cm-s-zp-panel .cm-keyword, .cm-s-zp-panel .cm-atom, .cm-s-zp-panel .cm-def { color: #c792ea; }',
    '.cm-s-zp-panel .cm-variable-2, .cm-s-zp-panel .cm-attribute { color: #e2b96b; }',
    '.cm-s-zp-panel .cm-variable-3, .cm-s-zp-panel .cm-type, .cm-s-zp-panel .cm-builtin { color: #4ec9b0; }',
    '.cm-s-zp-panel .cm-tag, .cm-s-zp-panel .cm-meta { color: #f07178; }',
    '.cm-s-zp-panel .cm-link, .cm-s-zp-panel .cm-qualifier { color: #6fb3f2; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-comment { color: #8a93a3; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-string, :root[data-theme="light"] .cm-s-zp-panel .cm-string-2 { color: #15803d; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-number { color: #b45309; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-keyword, :root[data-theme="light"] .cm-s-zp-panel .cm-atom, :root[data-theme="light"] .cm-s-zp-panel .cm-def { color: #7c3aed; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-variable-2, :root[data-theme="light"] .cm-s-zp-panel .cm-attribute { color: #a16207; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-variable-3, :root[data-theme="light"] .cm-s-zp-panel .cm-type, :root[data-theme="light"] .cm-s-zp-panel .cm-builtin { color: #0f766e; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-tag, :root[data-theme="light"] .cm-s-zp-panel .cm-meta { color: #b91c1c; }',
    ':root[data-theme="light"] .cm-s-zp-panel .cm-link, :root[data-theme="light"] .cm-s-zp-panel .cm-qualifier { color: #1d4ed8; }',
    // 查找/替换对话框：CodeMirror 自带的是浅色浮层，这里让它跟随面板
    '.zpf-cm .CodeMirror-dialog { background: var(--panel); color: var(--text); border-top: 1px solid var(--border); padding: 6px 10px; font-size: 12.5px; }',
    '.zpf-cm .CodeMirror-dialog input { background: var(--bg-soft); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 3px 6px; font-family: var(--mono); }',
    '.zpf-cm .CodeMirror-dialog button { background: var(--panel-2); color: var(--text); border: 1px solid var(--border); border-radius: 4px; padding: 2px 8px; cursor: pointer; }',
    // Monokai：只作用于编辑器弹窗根节点，面板其它部分不受影响
    '.zpf-monokai { --bg-soft: #272822; --panel: #272822; --panel-2: #34352c;',
    '  --border: #49483e; --border-soft: #3b3c33; --text: #f8f8f2; --text-mute: #b9b9ae;',
    '  background: #272822; border-color: #49483e; }',
    '.zpf-monokai .modal-head, .zpf-monokai .modal-foot { border-color: #3b3c33; }',
    '.zpf-monokai .modal-head h3, .zpf-monokai .hint { color: #f8f8f2; }',
    '.zpf-monokai .modal-close { color: #b9b9ae; }',
    '.zpf-monokai .modal-close:hover { color: #f8f8f2; }',
    '.zpf-monokai .select, .zpf-monokai .input { color: #f8f8f2; }',
    '.zpf-monokai .select option { background: #272822; color: #f8f8f2; }',
    '.zpf-monokai .zpf-cm .CodeMirror { background: #272822; color: #f8f8f2; }',
  ].join('\n');
  document.head.appendChild(st);
}

// ---------------- 编辑器弹窗 ----------------

// editorModal 打开在线编辑器（entry 是文件条目，res 是 /files/read 的结果）。
//
// 结构：工具条（配色 / 路径 / 语言 / 查找 / 最大化 / 全屏）+ CodeMirror + 底部保存。
// 先把窗口立起来再异步加载 CodeMirror：加载慢或失败时用户看到的是**原因**，
// 而不是一个"点了没反应"的空白弹窗。
function editorModal(entry, res) {
  const langKey = langKeyFor(entry.name);
  const langDef = CM_LANGS[langKey];
  const host = h('div.zpf-cm', { style: { flex: '1 1 auto', minHeight: '0', height: '58vh' } });
  const hint = h('div.hint', { style: { marginTop: '6px' } });
  const editorBox = h('div', {
    style: {
      display: 'flex', flexDirection: 'column', minHeight: '0',
      border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)', overflow: 'hidden',
      background: 'var(--bg-soft)',
    },
  }, [host, hint]);

  let cm = null;
  let dirty = false;
  let theme = readEditorTheme();
  let boxWide = false;

  const themeSel = h('select.select', { style: { width: 'auto' } }, [
    h('option', { value: ZPF_THEME_PANEL, text: '配色：跟随面板', selected: theme === ZPF_THEME_PANEL }),
    h('option', { value: ZPF_THEME_MONOKAI, text: '配色：Monokai', selected: theme === ZPF_THEME_MONOKAI }),
  ]);
  themeSel.addEventListener('change', () => applyTheme(themeSel.value));

  const findBtn = h('button.btn.btn-sm', {
    text: '🔍 查找替换',
    title: '查找 Ctrl+F · 替换 Shift+Ctrl+F · 跳行 Alt+G',
    onclick: () => cm && cm.execCommand('findPersistent'),
  });
  const maxBtn = h('button.btn.btn-sm', { text: '⛶ 最大化', title: '撑满窗口（不进入系统全屏）', onclick: () => setMaximized(!boxWide) });
  const fsBtn = h('button.btn.btn-sm', { text: '⛶ 屏幕全屏', title: '进入浏览器全屏（Esc 退出）', onclick: toggleFullscreen });
  const saveBtn = h('button.btn.btn-primary', { text: '保存', title: '保存（Ctrl+S）' });

  const toolbar = h('div', {
    style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', padding: '8px 10px', borderBottom: '1px solid var(--border-soft)' },
  }, [
    themeSel, findBtn, maxBtn, fsBtn,
    h('div', { style: { flex: '1 1 auto' } }),
    h('span.hint', { text: entry.path }),
  ]);

  const bodyEl = h('div', { style: { display: 'flex', flexDirection: 'column', minHeight: '0', flex: '1 1 auto' } }, [toolbar, editorBox]);

  function setDirty(v) {
    dirty = v;
    m?.setStatus(v ? '● 未保存' : '');
  }

  function applyTheme(v) {
    theme = v === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
    saveEditorTheme(theme);
    themeSel.value = theme;
    applyEditorThemeTo(m?.el, theme);
    if (cm) {
      cm.setOption('theme', theme === ZPF_THEME_MONOKAI ? 'monokai' : 'zp-panel');
      // 换主题会重建行高/尺寸相关样式，必须 refresh，否则光标与行会短暂错位
      requestAnimationFrame(() => cm.refresh());
    }
  }

  function setMaximized(on) {
    boxWide = on;
    const bodyWrap = m.el.querySelector('.modal-body');
    if (on) {
      m.el.style.width = 'min(96vw, 1600px)';
      m.el.style.maxWidth = '96vw';
      m.el.style.height = '92vh';
      m.el.style.maxHeight = '92vh';
      bodyWrap.style.display = 'flex';
      bodyWrap.style.flexDirection = 'column';
      bodyWrap.style.flex = '1 1 auto';
      bodyWrap.style.minHeight = '0';
      bodyWrap.style.overflow = 'hidden';
      host.style.height = '100%';
    } else {
      m.el.style.width = '';
      m.el.style.maxWidth = '';
      m.el.style.height = '';
      m.el.style.maxHeight = '';
      bodyWrap.style.display = '';
      bodyWrap.style.flexDirection = '';
      bodyWrap.style.flex = '';
      bodyWrap.style.minHeight = '';
      bodyWrap.style.overflow = '';
      host.style.height = '58vh';
    }
    maxBtn.textContent = on ? '🗗 还原' : '⛶ 最大化';
    requestAnimationFrame(() => { if (cm) { cm.setSize(null, '100%'); cm.refresh(); } });
  }

  function fsElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }

  function toggleFullscreen() {
    if (fsElement()) {
      const exit = document.exitFullscreen || document.webkitExitFullscreen;
      if (exit) { const p = exit.call(document); if (p && p.catch) p.catch(() => {}); }
      return;
    }
    const req = editorBox.requestFullscreen || editorBox.webkitRequestFullscreen;
    if (!req) { toast('当前浏览器不支持屏幕全屏', 'warn'); return; }
    const p = req.call(editorBox);
    if (p && p.catch) p.catch((e) => toast('进入屏幕全屏失败：' + ((e && e.message) || e), 'err'));
  }

  function onFsChange() {
    const on = !!fsElement();
    fsBtn.textContent = on ? '🗗 退出全屏' : '⛶ 屏幕全屏';
    requestAnimationFrame(() => { if (cm) cm.refresh(); });
  }
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('webkitfullscreenchange', onFsChange);

  async function writeBack() {
    if (!cm) return false;
    saveBtn.disabled = true;
    try {
      await api.fileWrite(entry.path, cm.getValue());
      setDirty(false);
      toast('已保存', 'ok');
      return true;
    } catch (e) {
      toast(e.message, 'err', 10000);
      return false;
    } finally {
      saveBtn.disabled = false;
    }
  }

  function confirmDiscard() {
    return !dirty || confirm('有未保存的修改，确定关闭？');
  }

  saveBtn.addEventListener('click', async () => { if (await writeBack()) m.close(); });

  const m = modal({
    title: `编辑：${entry.name}`,
    wide: true,
    minimizable: true,
    body: bodyEl,
    footer: () => [
      h('button.btn', { text: '取消', onclick: () => { if (confirmDiscard()) m.close(); } }),
      saveBtn,
    ],
    // 编辑器里可能是十几分钟的改动：点遮罩不关（误触代价太大）。
    // Esc 允许关闭，但**必须走同一道确认**（有未保存改动时先问）——
    // 完全禁用 Esc 反直觉，而"能关但不丢数据"才是用户真正要的。
    closeOnBackdrop: false,
    closeOnEsc: true,
    onRequestClose: () => confirmDiscard(),
    onMinimize: (min) => { if (!min && cm) requestAnimationFrame(() => cm.refresh()); },
    onClose: () => {
      document.removeEventListener('fullscreenchange', onFsChange);
      document.removeEventListener('webkitfullscreenchange', onFsChange);
    },
  });

  applyEditorThemeTo(m.el, theme);

  // ---- 异步挂载 CodeMirror ----
  hint.textContent = '正在加载编辑器…';
  (async () => {
    await ensureCodeMirror();
    await ensureLang(langKey);
    cm = window.CodeMirror(host, {
      value: res.content,
      mode: langDef ? langDef.mode : null,
      theme: theme === ZPF_THEME_MONOKAI ? 'monokai' : 'zp-panel',
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
        'Ctrl-S': () => { writeBack(); },
        'Cmd-S': () => { writeBack(); },
        'Ctrl-F': 'findPersistent',
        'Cmd-F': 'findPersistent',
        'Shift-Ctrl-F': 'replace',
        'Shift-Cmd-F': 'replace',
        'Alt-G': 'jumpToLine',
        'Ctrl-/': 'toggleComment',
        'Cmd-/': 'toggleComment',
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
    // 打开时把光标放在开头，并且**不让这次初始内容算作"未保存的改动"**
    cm.setCursor(0, 0);
    cm.clearHistory();
    cm.on('change', () => setDirty(true));
    setDirty(false);
    cm.focus();
    const lines = cm.lineCount();
    hint.textContent = `${langDef ? langDef.label : '纯文本'} · ${lines} 行 · ${humanSize(res.size)}`
      + '　Tab 缩进 · Ctrl+S 保存 · Ctrl+F 查找' + (langDef && langKey === 'ts' ? '（TypeScript 按 JavaScript 高亮）' : '');
  })().catch((e) => {
    // 加载失败要给出**原因**（网络/镜像/文件缺失），并且仍然给一条能走通的路
    clear(host);
    appendAll(host, h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: '编辑器加载失败' }),
      h('p', { text: (e && e.message) || String(e) }),
      h('p', { text: '可以刷新页面重试；也可以用 Web 终端或下载后本地编辑。文件内容没有被改动。' }),
    ]));
    hint.textContent = '';
  });
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
