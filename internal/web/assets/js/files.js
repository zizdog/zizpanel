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
      h('th', { text: '名称' }),
      h('th', { text: '大小' }),
      h('th', { text: '权限' }),
      h('th', { text: '属主' }),
      h('th', { text: '修改时间' }),
      h('th', { text: '操作' }),
    ]);

    const rows = list.map((e) => {
      const selected = selection.has(e.path);
      return h('tr', { style: selected ? { background: 'var(--brand-soft)' } : {} }, [
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
                title: '点击编辑',
                onclick: () => openEditor(e),
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
              : h('button.btn.btn-sm', { text: '编辑', onclick: () => openEditor(e) }),
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
        onclick: async () => {
          m.close();
          try {
            const r = await api.fileExtract(e.path, cwd);
            toast(r.msg || '已解压', 'ok'); load(cwd);
          } catch (err) { toast(err.message, 'err', 10000); }
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
          text: '开始压缩',
          onclick: async () => {
            try {
              const r = await api.fileCompress(cwd, names, format.value, output.value);
              toast(r.msg, 'ok'); close(); load(cwd);
            } catch (e) { toast(e.message, 'err', 10000); }
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
      await runUpload(entries, { folder: !!opts.folder });
    } catch (err) {
      const msg = err && err.message ? err.message : String(err);
      toast('上传未能开始：' + msg, 'err', 15000);
    }
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
  async function runUpload(entries, { folder }) {
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

  registerCleanup(() => { });
  load(cwd || undefined);
}

// ============================================================================
//  在线编辑器：透明 textarea + 高亮层
// ============================================================================
//
// 做法：一个 <pre> 放彩色高亮结果，一个 <textarea> 叠在它上面负责真正的输入；
// textarea 的文字设成透明、只留光标，看起来就像在"直接编辑彩色代码"。
//
// 为什么不用 contenteditable：输入法、选区、撤销、光标位置全都要自己实现，
// textarea 是浏览器白送的、行为最稳的输入控件。代价是两层必须逐像素对齐 ——
// 字体、字号、行高、内边距、边框宽度、tab 宽度任何一项不一致都会错位，
// 所以这些属性统一由 zpfTextStyle() 提供，两层共用同一份。

const HL_MAX_CHARS = 200 * 1024; // 超过 200KB 直接跳过高亮：整篇重新分词的开销随体积线性增长，几 MB 的日志会把页面冻住
const HL_MAX_TOKENS = 20000;     // 片段上限：压缩过的单行代码能在一屏里产生几万个 token，超了同样退回纯文本
const LN_MAX = 20000;            // 行号上限：全换行的 2MB 文件能有上百万行，拼行号字符串会拖死输入
const ZPF_FONT_SIZE = 13;
const ZPF_LINE_HEIGHT = 20;
const ZPF_PAD = '10px 12px';
const ZPF_CSS_ID = 'zpf-editor-style';

// ---------------- 编辑器配色（跟随面板 / Monokai） ----------------
//
// 编辑器是面板里唯一的"长时间盯着的代码界面"，所以给它一套**独立于面板主题**的配色。
// 两档：跟随面板（默认，行为与以前完全一致）/ Monokai（经典取值，深色）。
// 选择要持久化 —— 每次打开文件都要重选一次的话这个开关就等于没有。
// 键名刻意与面板主题 'zp-theme'、应用市场筛选 'zp-market-kind-filter' 区分开，
// 互不覆盖：改编辑器配色不该把面板切到深色，反之亦然。
const ZPF_THEME_KEY = 'zp-file-editor-theme';
const ZPF_THEME_PANEL = 'panel';     // 跟随面板（浅色/深色都走原来的规则）
const ZPF_THEME_MONOKAI = 'monokai';

function readEditorTheme() {
  // localStorage 在隐私模式/被禁用时会抛异常，读不到就退回默认值
  try {
    return localStorage.getItem(ZPF_THEME_KEY) === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
  } catch { return ZPF_THEME_PANEL; }
}

function saveEditorTheme(v) {
  try { localStorage.setItem(ZPF_THEME_KEY, v); } catch { /* 存不了就算了，本次会话仍然生效 */ }
}

/**
 * applyEditorThemeTo(el, v) —— 把配色作用到弹窗根节点。
 * 只加/去一个类，样式全在 ensureEditorStyle() 注入的 CSS 里。
 * 不动 :root，所以面板主题不受影响，同页面其它弹窗也不会被带成黑底。
 */
function applyEditorThemeTo(el, v) {
  if (el) el.classList.toggle('zpf-monokai', v === ZPF_THEME_MONOKAI);
}

function zpfTextStyle() {
  return {
    fontFamily: 'var(--mono)',
    fontSize: ZPF_FONT_SIZE + 'px',
    lineHeight: ZPF_LINE_HEIGHT + 'px',
    fontWeight: '400',
    letterSpacing: 'normal',
    tabSize: '4',
    whiteSpace: 'pre',               // 必须 pre：pre-wrap 会折行，折行后行号槽和高亮层必然错位
    padding: ZPF_PAD,
    border: '1px solid transparent', // 透明边框占位：textarea 自带 1px 边框，高亮层不补就会差 1px
    margin: '0',
  };
}

// 扩展名 → 语言。任务里要求的语言集，不认识的一律纯文本。
const EXT_LANG = {
  php: 'php',
  js: 'js', mjs: 'js', cjs: 'js',
  ts: 'ts',
  json: 'json',
  go: 'go',
  py: 'py',
  sh: 'sh', bash: 'sh', zsh: 'sh',
  yaml: 'yaml', yml: 'yaml',
  html: 'html', htm: 'html',
  css: 'css',
  sql: 'sql',
  ini: 'ini', conf: 'ini', cnf: 'ini',
  md: 'md',
};

const LANG_LABEL = {
  php: 'PHP', js: 'JavaScript', ts: 'TypeScript', json: 'JSON', go: 'Go',
  py: 'Python', sh: 'Shell', yaml: 'YAML', html: 'HTML', css: 'CSS',
  sql: 'SQL', ini: 'INI', md: 'Markdown',
};

// 语法规则：每种语言是一组 [组名, 正则源]，运行时拼成一条带命名分组的 alternation。
// 顺序 = 优先级：注释/字符串必须排在关键字前面。整篇只跑一次 exec 循环，
// 不能对每个 token 各来一遍 replace —— 那样既慢，又会让后一遍把前一遍的结果套娃。
const RE_C_COM = String.raw`\/\/[^\n]*|\/\*[\s\S]*?\*\/`;
const RE_HASH_COM = String.raw`#[^\n]*`;
const RE_STR_DQ = String.raw`"(?:\\.|[^"\\\n])*"`;
const RE_STR_SQ = String.raw`'(?:\\.|[^'\\\n])*'`;
const RE_STR_JS = String.raw`\x60(?:\\.|[^\x60\\])*\x60|` + RE_STR_DQ + '|' + RE_STR_SQ;
const RE_NUM = String.raw`\b(?:0[xX][0-9a-fA-F]+|0[bB][01]+|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)`;
const RE_FN_CALL = String.raw`[A-Za-z_$][\w$]*(?=\s*\()`;
const RE_TYPE_NAME = String.raw`\b[A-Z][A-Za-z0-9_$]*\b`;

function kwRe(words) { return String.raw`\b(?:` + words + String.raw`)\b`; }

const JS_KW = 'break|case|catch|class|const|continue|debugger|default|delete|do|else|export|extends|finally|for|function|if|import|in|instanceof|let|new|of|return|static|super|switch|this|throw|try|typeof|var|void|while|with|yield|async|await|true|false|null|undefined|get|set';
const TS_KW = JS_KW + '|interface|type|enum|implements|declare|namespace|abstract|public|private|protected|readonly|as|satisfies|keyof|infer|is|asserts|never|unknown|any|string|number|boolean|object|symbol|bigint|override';
const GO_KW = 'break|case|chan|const|continue|default|defer|else|fallthrough|for|func|go|goto|if|import|interface|map|package|range|return|select|struct|switch|type|var|nil|true|false|iota|make|new|len|cap|append|copy|delete|panic|recover';
const PY_KW = 'and|as|assert|async|await|break|class|continue|def|del|elif|else|except|finally|for|from|global|if|import|in|is|lambda|nonlocal|not|or|pass|raise|return|try|while|with|yield|True|False|None|self';
const SH_KW = 'if|then|else|elif|fi|for|while|until|do|done|case|esac|function|return|in|local|export|readonly|declare|source|alias|unset|shift|exit|eval|exec|trap|set|echo|printf|test|break|continue|true|false';
const SQL_KW = 'select|from|where|insert|into|values|update|set|delete|create|table|drop|alter|add|index|view|join|left|right|inner|outer|on|group|by|order|having|limit|offset|union|all|distinct|as|and|or|not|null|is|like|between|exists|count|sum|avg|min|max|case|when|then|else|end|primary|key|foreign|references|default|unique|begin|commit|rollback|with|desc|asc';
const PHP_KW = 'abstract|and|array|as|break|callable|case|catch|class|clone|const|continue|declare|default|do|echo|else|elseif|empty|enddeclare|endfor|endforeach|endif|endswitch|endwhile|enum|eval|exit|extends|final|finally|fn|for|foreach|function|global|goto|if|implements|include|include_once|instanceof|insteadof|interface|isset|list|match|namespace|new|or|print|private|protected|public|readonly|require|require_once|return|static|switch|throw|trait|try|unset|use|var|while|xor|yield|true|false|null|this|parent|self';

function jsLike(kw) {
  return {
    flags: 'g',
    rules: [
      ['com', RE_C_COM],
      ['str', RE_STR_JS],
      ['num', RE_NUM],
      ['kw', kwRe(kw)],
      ['fn', RE_FN_CALL],
      ['typ', RE_TYPE_NAME],
    ],
  };
}

const LANG_DEFS = {
  js: jsLike(JS_KW),
  ts: jsLike(TS_KW),
  json: {
    flags: 'g',
    rules: [
      // 键名单独一色：JSON 里 "key": 和普通字符串靠这个区分
      ['key', String.raw`"(?:\\.|[^"\\\n])*"(?=\s*:)`],
      ['str', RE_STR_DQ],
      ['num', RE_NUM],
      ['kw', kwRe('true|false|null')],
    ],
  },
  php: {
    flags: 'g',
    rules: [
      // `#(?!\[)`：PHP 8 的属性语法 `#[Attr]` 不是注释，不能吃掉整行
      ['com', String.raw`\/\/[^\n]*|\/\*[\s\S]*?\*\/|#(?!\[)[^\n]*`],
      ['str', RE_STR_DQ + '|' + RE_STR_SQ],
      ['vr', String.raw`\$[A-Za-z_]\w*`],
      ['num', RE_NUM],
      ['kw', kwRe(PHP_KW)],
      ['fn', String.raw`[A-Za-z_]\w*(?=\s*\()`],
      ['typ', RE_TYPE_NAME],
    ],
  },
  go: {
    flags: 'g',
    rules: [
      ['com', RE_C_COM],
      ['str', String.raw`\x60[^\x60]*\x60` + '|' + RE_STR_DQ + '|' + RE_STR_SQ],
      ['num', RE_NUM],
      ['kw', kwRe(GO_KW)],
      ['fn', String.raw`[A-Za-z_]\w*(?=\s*\()`],
      ['typ', RE_TYPE_NAME],
    ],
  },
  py: {
    flags: 'g',
    rules: [
      ['com', RE_HASH_COM],
      ['str', String.raw`[rRbBuUfF]{0,2}(?:"""[\s\S]*?"""|'''[\s\S]*?'''|"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*')`],
      ['num', RE_NUM],
      ['kw', kwRe(PY_KW)],
      ['fn', String.raw`[A-Za-z_]\w*(?=\s*\()`],
      ['typ', RE_TYPE_NAME],
    ],
  },
  sh: {
    flags: 'g',
    rules: [
      ['com', RE_HASH_COM],
      ['str', RE_STR_DQ + '|' + String.raw`'[^'\n]*'`],
      ['vr', String.raw`\$\{[A-Za-z_]\w*\}|\$[A-Za-z_]\w*|\$[0-9@*#?$!-]`],
      ['num', String.raw`\b\d+\b`],
      ['kw', kwRe(SH_KW)],
      ['fn', String.raw`[A-Za-z_][\w-]*(?=\s*\(\s*\))`],
    ],
  },
  yaml: {
    flags: 'gm',
    rules: [
      ['com', RE_HASH_COM],
      ['str', RE_STR_DQ + '|' + String.raw`'[^'\n]*'`],
      ['key', String.raw`^[ \t]*-?[ \t]*[A-Za-z_][\w .-]*(?=:)`],
      ['num', String.raw`\b\d+(?:\.\d+)?\b`],
      ['kw', kwRe('true|false|null|yes|no|on|off|True|False|Null|Yes|No|On|Off')],
    ],
  },
  html: {
    flags: 'gi',
    rules: [
      ['com', String.raw`<!--[\s\S]*?-->`],
      ['imp', String.raw`<!doctype[^>]*>`],
      ['tag', String.raw`<\/?[a-z][\w:-]*`],
      ['attr', String.raw`[a-z_:][\w:.-]*(?=\s*=)`],
      ['str', String.raw`"[^"\n]*"|'[^'\n]*'`],
    ],
  },
  css: {
    flags: 'g',
    rules: [
      ['com', String.raw`\/\*[\s\S]*?\*\/`],
      ['str', RE_STR_DQ + '|' + String.raw`'[^'\n]*'`],
      ['at', String.raw`@[a-zA-Z-]+`],
      ['imp', String.raw`!important\b`],
      ['num', String.raw`#[0-9a-fA-F]{3,8}\b|\b\d+(?:\.\d+)?(?:px|em|rem|vh|vw|vmin|vmax|%|s|ms|deg|fr|ch)?`],
      ['fn', String.raw`[a-zA-Z-]+(?=\()`],
    ],
  },
  sql: {
    flags: 'gi',
    rules: [
      ['com', String.raw`--[^\n]*|\/\*[\s\S]*?\*\/`],
      ['str', String.raw`'(?:''|[^'])*'|"(?:""|[^"])*"`],
      ['num', String.raw`\b\d+(?:\.\d+)?\b`],
      ['kw', kwRe(SQL_KW)],
      ['fn', String.raw`[a-z_]\w*(?=\s*\()`],
    ],
  },
  ini: {
    flags: 'gim',
    rules: [
      ['com', String.raw`[;#][^\n]*`],
      ['sec', String.raw`^[ \t]*\[[^\]\n]*\]`],
      ['str', RE_STR_DQ + '|' + String.raw`'[^'\n]*'`],
      ['key', String.raw`^[ \t]*[A-Za-z_][\w.-]*(?=\s*[=:])`],
      ['num', String.raw`\b\d+(?:\.\d+)?\b`],
      ['kw', kwRe('true|false|yes|no|on|off|null')],
    ],
  },
  md: {
    flags: 'gm',
    rules: [
      ['com', String.raw`<!--[\s\S]*?-->`],
      ['hd', String.raw`^#{1,6}[^\n]*`],
      ['delim', String.raw`^[ \t]*(?:\x60{3,}|~{3,})[^\n]*`],
      ['code', String.raw`\x60[^\x60\n]+\x60`],
      ['bold', String.raw`\*\*[^*\n]+\*\*|__[^_\n]+__`],
      ['link', String.raw`!?\[[^\]\n]*\]\([^)\n]*\)`],
      ['kw', String.raw`^[ \t]{0,3}(?:[-*+]|\d+\.)[ \t]|^[ \t]{0,3}>[ \t]?`],
    ],
  },
};

// 组名 → 注入样式里的类名
const TOKEN_CLASS = {
  com: 'zpf-com', str: 'zpf-str', num: 'zpf-num', kw: 'zpf-kw', fn: 'zpf-fn',
  typ: 'zpf-typ', vr: 'zpf-vr', key: 'zpf-key', tag: 'zpf-tag', attr: 'zpf-attr',
  at: 'zpf-at', imp: 'zpf-imp', hd: 'zpf-hd', link: 'zpf-link', sec: 'zpf-sec',
  code: 'zpf-str', bold: 'zpf-b', delim: 'zpf-com',
};

const ZPF_REGEX = new Map();

function langRegex(lang) {
  let re = ZPF_REGEX.get(lang);
  if (re) return re;
  const def = LANG_DEFS[lang];
  if (!def) return null;
  re = new RegExp(def.rules.map(([name, src]) => `(?<${name}>${src})`).join('|'), def.flags || 'g');
  ZPF_REGEX.set(lang, re);
  return re;
}

/** paintHighlight(frag, text, lang) -> token 数：把文本切成"纯文本 + 上色 span"塞进 frag。 */
function paintHighlight(frag, text, lang) {
  const re = langRegex(lang);
  if (!re) return 0;
  re.lastIndex = 0;
  let last = 0;
  let count = 0;
  let m;
  while ((m = re.exec(text)) !== null) {
    const tok = m[0];
    // 零宽匹配会让 exec 原地踏步、死循环；正则都要求至少一个字符，这里只是兜底
    if (!tok.length) { re.lastIndex++; continue; }
    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    const g = m.groups || {};
    let cls = '';
    for (const k in g) { if (g[k] !== undefined) { cls = TOKEN_CLASS[k] || ''; break; } }
    frag.appendChild(cls ? h('span', { class: cls, text: tok }) : document.createTextNode(tok));
    last = m.index + tok.length;
    count++;
  }
  if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
  return count;
}

/** langOf(name) -> 语言 id，不认识的返回 ''（纯文本）。 */
function langOf(name) {
  const m = /\.([A-Za-z0-9]+)$/.exec(String(name));
  return m ? (EXT_LANG[m[1].toLowerCase()] || '') : '';
}

function countLines(text) {
  let n = 1;
  for (let i = text.indexOf('\n'); i >= 0; i = text.indexOf('\n', i + 1)) n++;
  return n;
}

// 行号文本按"行数"缓存：输入时行数通常没变，没必要每次重新拼几万个数
let zpfLnKey = -1;
let zpfLnText = '';
function lineNumbers(n) {
  if (n === zpfLnKey) return zpfLnText;
  const arr = new Array(n);
  for (let i = 0; i < n; i++) arr[i] = i + 1;
  zpfLnKey = n;
  zpfLnText = arr.join('\n');
  return zpfLnText;
}

// 面板禁止改 CSS 文件，所以编辑器自己的样式在这里注入一次。
// 只能用 textContent 赋值：面板禁止一切 HTML 字符串注入，样式表也不例外。
function ensureEditorStyle() {
  if (document.getElementById(ZPF_CSS_ID)) return;
  const st = document.createElement('style');
  st.id = ZPF_CSS_ID;
  st.textContent = [
    '.zpf-code, .zpf-lines { margin: 0; transform-origin: 0 0; will-change: transform; }',
    // <code> 默认是 inline，而 transform 对 inline 元素无效 —— 不改成 block 滚动同步会静默失效
    '.zpf-code { display: block; }',
    // 输入层的文字永远不可见（高亮层负责显示），否则会出现"双层文字"；
    // ::selection 也要一起处理：某些浏览器选中时会用默认高亮前景色把文字显出来
    '.zpf-ta { color: transparent !important; -webkit-text-fill-color: transparent; caret-color: var(--text); background: transparent !important; }',
    '.zpf-ta::selection { background: rgba(96,165,250,.35); color: transparent; -webkit-text-fill-color: transparent; }',
    // 纯文本模式（大文件 / 不认识的语言 / 代码过密）让 textarea 自己显示文字
    '.zpf-ta.zpf-plain { color: var(--text) !important; -webkit-text-fill-color: var(--text); }',
    // 查找命中层：整层文字透明，只留 <mark> 的背景色块，所以它既可以垫在
    // 高亮模式（彩色文字）下面，也可以垫在纯文本模式（textarea 自己显示文字）下面。
    // color 必须显式写：mark 的 UA 样式会带上 MarkText 前景色，不覆盖就会把
    // 下面那层的字盖成不透明色块后的另一种颜色。
    '.zpf-hitbox { color: transparent; pointer-events: none; }',
    '.zpf-hit, .zpf-hit-cur { color: transparent; padding: 0; margin: 0; border-radius: 2px; }',
    '.zpf-hit { background: rgba(250, 204, 21, .30); }',
    '.zpf-hit-cur { background: rgba(249, 115, 22, .70); box-shadow: 0 0 0 1px rgba(249, 115, 22, .85); }',
    ':root[data-theme="light"] .zpf-hit { background: rgba(234, 179, 8, .40); }',
    ':root[data-theme="light"] .zpf-hit-cur { background: rgba(249, 115, 22, .55); }',
    '.zpf-fcount { display: inline-block; min-width: 54px; text-align: center; font-size: 12px; font-variant-numeric: tabular-nums; }',
    '.zpf-editor:fullscreen, .zpf-editor:-webkit-full-screen { border: none; border-radius: 0; }',
    '.zpf-com { color: #6b7688; font-style: italic; }',
    // 注意别把 .zpf-code（高亮层容器）写进着色规则：它一上色，所有未被 token 命中的
    // 普通文本（标识符、运算符、括号）都会跟着变绿，看起来整篇都是字符串
    '.zpf-str { color: #86d99a; }',
    '.zpf-num { color: #f0a868; }',
    '.zpf-kw, .zpf-at, .zpf-sec { color: #c792ea; }',
    '.zpf-fn, .zpf-key { color: #6fb3f2; }',
    '.zpf-typ { color: #4ec9b0; }',
    '.zpf-vr, .zpf-attr { color: #e2b96b; }',
    '.zpf-tag, .zpf-imp { color: #f07178; }',
    '.zpf-hd { color: #6fb3f2; font-weight: 600; }',
    '.zpf-link { color: #6fb3f2; text-decoration: underline; }',
    '.zpf-b { color: #c792ea; font-weight: 600; }',
    ':root[data-theme="light"] .zpf-com { color: #8a93a3; }',
    ':root[data-theme="light"] .zpf-str { color: #15803d; }',
    ':root[data-theme="light"] .zpf-num { color: #b45309; }',
    ':root[data-theme="light"] .zpf-kw, :root[data-theme="light"] .zpf-at, :root[data-theme="light"] .zpf-sec, :root[data-theme="light"] .zpf-b { color: #7c3aed; }',
    ':root[data-theme="light"] .zpf-fn, :root[data-theme="light"] .zpf-key, :root[data-theme="light"] .zpf-hd, :root[data-theme="light"] .zpf-link { color: #1d4ed8; }',
    ':root[data-theme="light"] .zpf-typ { color: #0f766e; }',
    ':root[data-theme="light"] .zpf-vr, :root[data-theme="light"] .zpf-attr { color: #a16207; }',
    ':root[data-theme="light"] .zpf-tag, :root[data-theme="light"] .zpf-imp { color: #b91c1c; }',

    // ---------------- Monokai（只作用于编辑器弹窗） ----------------
    //
    // 面板全局只有浅色/深色两套，编辑器要的是第三套**互不干扰**的配色，所以不去动
    // :root，而是挂在编辑器自己的弹窗根节点 .zpf-monokai 上。用户没选 Monokai 时
    // 这条规则完全不参与匹配，浅色/深色下的观感与以前逐像素一致。
    //
    // 重定义面板变量而不是逐个覆盖内联样式：编辑器的背景、行号槽、查找条背景的内联
    // 样式里写的都是 var(--xxx)（见 editorModal），变量在这里被重定义后它们自动跟着变；
    // 面板对这些元素没有 !important，且正文文字在编辑器里本来就由高亮层画，
    // 所以只用 .zpf-monokai 这一个类就能整套换色。
    '.zpf-monokai {',
    '  --bg-soft: #272822; --panel: #272822; --panel-2: #34352c;',
    '  --border: #49483e; --border-soft: #3b3c33; --text: #f8f8f2; --text-mute: #b9b9ae;',
    '  background: #272822; border-color: #49483e;',
    '}',
    '.zpf-monokai .modal-head, .zpf-monokai .modal-foot { border-color: #3b3c33; }',
    '.zpf-monokai .modal-head h3, .zpf-monokai .hint { color: #f8f8f2; }',
    '.zpf-monokai .modal-close { color: #b9b9ae; }',
    '.zpf-monokai .modal-close:hover { color: #f8f8f2; }',
    // 工具栏/查找条里的 input、下拉都是 var(--bg-soft) 底色 + 面板默认的深色文字，
    // Monokai 下 --bg-soft 变成 #272822，文字却还是深色 —— 实测"配色"下拉成了
    // 近黑底上的近黑字，基本看不见。这里把文字/占位符一起提亮。
    '.zpf-monokai .select, .zpf-monokai .input { color: #f8f8f2; }',
    '.zpf-monokai .input::placeholder { color: #9a9a90; }',
    '.zpf-monokai .select option { background: #272822; color: #f8f8f2; }',
    '.zpf-monokai .zpf-editor { background: #272822; border-color: #49483e; }',
    '.zpf-monokai .zpf-code, .zpf-monokai .zpf-hitbox, .zpf-monokai .zpf-lines { color: #f8f8f2; }',
    '.zpf-monokai .zpf-ta { caret-color: #f8f8f2; }',
    '.zpf-monokai .zpf-ta.zpf-plain { color: #f8f8f2 !important; -webkit-text-fill-color: #f8f8f2; }',
    '.zpf-monokai .zpf-ta::selection { background: rgba(73, 72, 62, .85); }',
    '.zpf-monokai .zpf-hit { background: rgba(230, 219, 116, .35); }',
    '.zpf-monokai .zpf-hit-cur { background: rgba(249, 38, 114, .55); box-shadow: 0 0 0 1px rgba(249, 38, 114, .9); }',
    // 经典 Monokai 取值：注释 #75715e / 字符串 #e6db74 / 关键字 #f92672 /
    // 数字 #ae81ff / 函数名 #a6e22e / 类型·类名 #66d9ef。
    //
    // 这里必须带 `:root`：上面的浅色规则是 `:root[data-theme="light"] .zpf-kw`，
    // 选择器权重 (0,3,0) 比 `.zpf-monokai .zpf-kw` 的 (0,2,0) 高，浅色面板下会直接
    // 把 Monokai 的配色顶掉（真机验证时就是这样：字全是浅色主题的颜色）。
    // 补一个 :root 让权重相同，靠"后写的那条赢"取胜。
    ':root .zpf-monokai .zpf-com { color: #75715e; }',
    ':root .zpf-monokai .zpf-str { color: #e6db74; }',
    ':root .zpf-monokai .zpf-num { color: #ae81ff; }',
    ':root .zpf-monokai .zpf-kw, :root .zpf-monokai .zpf-at, :root .zpf-monokai .zpf-sec { color: #f92672; }',
    ':root .zpf-monokai .zpf-fn, :root .zpf-monokai .zpf-key { color: #a6e22e; }',
    ':root .zpf-monokai .zpf-typ { color: #66d9ef; }',
    ':root .zpf-monokai .zpf-vr, :root .zpf-monokai .zpf-attr { color: #fd971f; }',
    ':root .zpf-monokai .zpf-tag, :root .zpf-monokai .zpf-imp { color: #f92672; }',
    ':root .zpf-monokai .zpf-hd { color: #a6e22e; }',
    ':root .zpf-monokai .zpf-link { color: #66d9ef; }',
    ':root .zpf-monokai .zpf-b { color: #ae81ff; }',

    // ---------------- 弹窗最小化（ui.js modal({minimizable:true})） ----------------
    //
    // 正文/页脚由 ui.js 用 display:none 隐藏（DOM 不销毁，编辑内容与光标都在），
    // 这里只负责"看起来收成了右下角一条标题栏"。
    // 遮罩整层藏掉：不然一条细窗口底下压着一层半透明全屏黑幕，页面上什么都看不清。
    '.modal-mask.zp-min { background: transparent; backdrop-filter: none; pointer-events: none; }',
    // 收起时必须固定定位并指定宽度：弹窗本来是 grid 居中项，宽度由 max-width 决定，
    // 不给宽度就会撑成整行。position:fixed 会脱离 grid 容器，right/bottom 才生效。
    '.modal.zp-min { position: fixed; right: 20px; bottom: 20px; width: min(420px, calc(100vw - 40px)); height: auto !important; max-height: none !important; z-index: 300; pointer-events: auto; cursor: pointer; }',
    '.zp-min-status { font-size: 12px; color: var(--text-mute); margin-right: 10px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 46%; }',
    '.zp-min-status.zpf-dirty { color: var(--warn); font-weight: 600; }',
    '.zp-min-btn { font-size: 15px; padding: 0 6px; }',
  ].join('\n');
  document.head.appendChild(st);
}

/**
 * editorModal(entry, res) —— 打开在线编辑弹窗。
 * 只在这里组装 UI；读文件/二进制判断仍由调用方负责。
 * 查找/替换是弹窗内部的一条可折叠查找条（默认隐藏），不再走另一个小弹窗。
 */
function editorModal(entry, res) {
  ensureEditorStyle();
  const lang = langOf(entry.name);

  // 高亮层：用 <code> 包一层，滚动同步靠 transform 平移它（见 syncScroll）
  const hlInner = h('code.zpf-code');
  const hlLayer = h('pre.zpf-pre', {
    style: Object.assign(zpfTextStyle(), {
      position: 'absolute', top: '0', left: '0', right: '0', bottom: '0',
      overflow: 'hidden', color: 'var(--text)',
    }),
  }, [hlInner]);

  const linesInner = h('pre.zpf-lines', {
    style: Object.assign(zpfTextStyle(), {
      padding: '0', border: 'none', textAlign: 'right',
      color: 'var(--text-mute)', userSelect: 'none',
    }),
  });
  const gutter = h('div', {
    style: {
      flexShrink: '0', overflow: 'hidden', background: 'var(--panel-2)',
      border: '1px solid transparent', borderRightColor: 'var(--border-soft)',
      padding: '10px 10px 10px 12px', width: 'calc(3ch + 26px)',
      // 宽度用 ch 计算，所以 gutter 自己的字体也必须是等宽体，否则 ch 是正文字的宽度
      fontFamily: 'var(--mono)', fontSize: ZPF_FONT_SIZE + 'px',
    },
    // 点行号栏会把焦点从 textarea 抢走，接着敲字就敲不进去
    onmousedown: (e) => e.preventDefault(),
  }, [linesInner]);

  const editor = h('textarea.textarea.zpf-ta', {
    value: res.content,
    wrap: 'off',
    autocapitalize: 'off',
    autocomplete: 'off',
    autocorrect: 'off',
    style: Object.assign(zpfTextStyle(), {
      position: 'absolute', top: '0', left: '0', right: '0', bottom: '0',
      width: '100%', height: '100%', resize: 'none', outline: 'none', minHeight: '0',
      color: 'transparent', caretColor: 'var(--text)', background: 'transparent',
      overflow: 'auto', zIndex: '2',
    }),
  });
  // h() 会跳过值为 false 的属性，spellcheck 只能创建后补 —— 否则代码里满屏红波浪线
  editor.setAttribute('spellcheck', 'false');

  // 查找命中层：和高亮层同一套 zpfTextStyle、同一个 transform，所以逐像素对齐。
  //
  // 为什么不用 textarea 的 ::selection 来显示当前匹配：查找时焦点必须留在查找框里
  // （否则"输入即查找"就断了），而浏览器只在输入控件**获得焦点**时才绘制它的选区 ——
  // 失焦的 textarea 选区是看不见的。所以命中色块必须由外面这层画。
  // 它垫在 textarea 下面（textarea 的背景是透明的），因此两行文字都能透出来：
  // 高亮模式下透出彩色文字，纯文本模式下透出 textarea 自己渲染的文字。
  const hitInner = h('code.zpf-code');
  const hitLayer = h('pre.zpf-hitbox', {
    style: Object.assign(zpfTextStyle(), {
      position: 'absolute', top: '0', left: '0', right: '0', bottom: '0',
      overflow: 'hidden', zIndex: '1', display: 'none',
    }),
  }, [hitInner]);

  const codeWrap = h('div', { style: { position: 'relative', flex: '1 1 auto', minWidth: '0' } }, [hlLayer, hitLayer, editor]);

  const editorBox = h('div.zpf-editor', {
    style: {
      position: 'relative', display: 'flex', flex: '1 1 auto',
      height: '58vh', minHeight: '300px', overflow: 'hidden',
      background: 'var(--bg-soft)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius-sm)', transition: 'border-color .16s',
    },
  }, [gutter, codeWrap]);

  const stat = h('span', { style: { fontSize: '12px', color: 'var(--text-mute)' }, text: `${res.content.length} 字符` });
  const bigHint = h('div.hint', { style: { display: 'none' } });
  const langPill = h('span.pill', { text: lang ? LANG_LABEL[lang] : '纯文本' });

  let dirty = false;
  let dense = false;   // 代码过密（token 超预算）→ 永久退回纯文本，避免每次输入都要重绘几万个 span
  let maximized = false;
  let collapsed = false;  // 最小化（收成右下角一条标题栏），由下面的 restoreView() 维护
  let firstFocus = true;
  let rafId = 0;
  let lastHint = '';
  let m = null;

  // 最小化时编辑器整体 display:none。高亮层/命中层的滚动同步依赖 clientHeight，
  // 隐藏状态下算出来的都是 0，所以这类计算要先问一句"看得见吗"。
  function boxHidden() { return collapsed; }

  function setHint(msg) {
    if (msg === lastHint) return;
    lastHint = msg;
    if (msg) { bigHint.textContent = msg; bigHint.style.display = ''; }
    else bigHint.style.display = 'none';
  }

  function renderGutter(text) {
    const n = countLines(text);
    if (n > LN_MAX) { gutter.style.display = 'none'; linesInner.textContent = ''; return; }
    gutter.style.display = '';
    gutter.style.width = `calc(${String(n).length}ch + 26px)`;
    linesInner.textContent = lineNumbers(n);
  }

  // 高亮层与输入层的滚动同步。
  //
  // 为什么用 transform 而不是把 pre.scrollTop 设成和 textarea 一样：
  // pre 必须隐藏自己的滚动条（否则会和 textarea 的滚动条叠成两条），
  // 一旦隐藏，pre 的 clientHeight 就比 textarea 大，"滚到底"的位置对不上，
  // 最后几行会错位。transform 没有可滚动范围，直接用 textarea 的值平移，
  // 顶部和底部都严格对齐。
  function syncScroll() {
    const x = editor.scrollLeft;
    const y = editor.scrollTop;
    hlInner.style.transform = `translate(${-x}px, ${-y}px)`;
    linesInner.style.transform = `translateY(${-y}px)`;
    if (hitLayer.style.display !== 'none') {
      syncHits();
      // 命中层只画可视区上下各 HIT_MARGIN 行；滚出这个范围才需要重画。
      // 缓冲留 10 行：滚动事件里直接重画会让大文件每次滚动都扫一遍文本，很浪费。
      const top = y / ZPF_LINE_HEIGHT;
      const bottom = (y + editor.clientHeight) / ZPF_LINE_HEIGHT;
      if (top < hitWinFirst + 10 || bottom > hitWinLast - 10) scheduleHits();
    }
  }

  function render() {
    // 最小化期间不重绘，也不做滚动同步：编辑器是 display:none，clientHeight 全是 0，
    // 这时候算出来的命中窗口和 transform 都是错的，只是在浪费 CPU。
    // 内容本来就在 textarea 的 value 里，还原时 restoreView() 会重新走一遍完整版。
    if (boxHidden()) return;
    const text = editor.value;
    const tooBig = text.length > HL_MAX_CHARS;
    const overlay = !!lang && !tooBig && !dense;

    if (overlay) {
      editor.classList.remove('zpf-plain');
      hlLayer.style.display = '';
      const frag = document.createDocumentFragment();
      let n = 0;
      try {
        n = paintHighlight(frag, text, lang);
      } catch (e) {
        dense = true;
        toast('语法高亮出错，已切换为纯文本：' + e.message, 'warn', 8000);
      }
      if (n > HL_MAX_TOKENS) dense = true;        // 片段太多，丢弃这次结果
      else if (!dense) { clear(hlInner); hlInner.appendChild(frag); }
    }

    // 纯文本模式：让 textarea 自己显示文字（原生渲染比我们重绘一份快得多），
    // 高亮层必须同时隐藏，否则两层文字会叠在一起
    if (!overlay || dense) {
      editor.classList.add('zpf-plain');
      hlLayer.style.display = 'none';
    }

    renderGutter(text);
    if (tooBig) setHint(`文件较大（${humanSize(text.length)}），已改用纯文本模式以保证输入流畅；编辑与保存不受影响。`);
    else if (dense) setHint('代码片段过于密集，已改用纯文本模式以保证输入流畅。');
    else setHint('');
    // 查找条开着时，文本一变命中位置也跟着变（用户可能直接在编辑区改代码）。
    // 这里只重算计数、不滚动：用户正在编辑的地方不能被"跳到当前匹配"拽走。
    if (findOpen) { recount(); paintHits(); }
    syncScroll();
  }

  // 合并同一帧内的多次输入。不用 debounce：textarea 的文字是透明的，
  // 高亮层必须紧跟输入，延迟重绘会让刚敲的字先"消失"再出现。
  function scheduleRender() {
    if (rafId) return;
    rafId = requestAnimationFrame(() => { rafId = 0; render(); });
  }

  // ---------- 两种全屏 ----------
  function fsElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }

  function applyHeight() {
    // 屏幕全屏时由 UA 决定尺寸，这里显式给 100% 更稳（各浏览器 UA 规则强度不一致）
    //
    // 非全屏时要把查找条让出来的高度扣掉：编辑区是定高的，而 .modal 有 88vh 上限，
    // 查找条一展开，正文就超出上限被 .modal-body 裁掉 —— 表现为编辑器最后几行
    // （连同当前匹配的色块）看不见，点"下一处"像是没反应。
    const shrink = findOpen && !fsElement() ? Math.min(160, findBar.offsetHeight + 8) : 0;
    editorBox.style.height = fsElement() ? '100%' : `calc(58vh - ${shrink}px)`;
  }

  function setMaximized(on) {
    maximized = on;
    // 注意：modal() 会把我传进去的 body 再包一层 .modal-body，
    // 所以编辑器区外面其实有两层。要让编辑器撑满，必须让外层也变成
    // "定高的纵向 flex"，否则里层 flex:1 没有可分配的空闲空间（表现为底部留一大块空白）。
    const bodyWrap = m.el.querySelector('.modal-body');
    if (on) {
      // 网页全屏：只把弹窗撑满视口，不碰系统全屏
      m.el.style.width = 'min(96vw, 1600px)';
      m.el.style.maxWidth = '96vw';
      m.el.style.height = '92vh';
      m.el.style.maxHeight = '92vh';
      bodyWrap.style.display = 'flex';
      bodyWrap.style.flexDirection = 'column';
      bodyWrap.style.flex = '1 1 auto';
      bodyWrap.style.minHeight = '0';
      bodyWrap.style.overflow = 'hidden';
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
    }
    maxBtn.textContent = on ? '🗗 还原' : '⛶ 最大化';
    // 尺寸变了，textarea 的可滚动范围也变了，下一帧把高亮层对回去
    requestAnimationFrame(syncScroll);
  }

  function toggleFullscreen() {
    if (fsElement()) {
      const exit = document.exitFullscreen || document.webkitExitFullscreen;
      if (exit) {
        const p = exit.call(document);
        if (p && p.catch) p.catch(() => {});
      }
      return;
    }
    const req = editorBox.requestFullscreen || editorBox.webkitRequestFullscreen;
    if (!req) { toast('当前浏览器不支持屏幕全屏', 'warn'); return; }
    // 必须在用户手势里调用，浏览器可能拒绝（权限/手势失效）——不能静默失败
    const p = req.call(editorBox);
    if (p && p.catch) p.catch((e) => toast('进入屏幕全屏失败：' + (e && e.message ? e.message : e), 'err'));
  }

  function onFsChange() {
    const on = !!fsElement();
    fsBtn.textContent = on ? '🗗 退出全屏' : '⛶ 屏幕全屏';
    applyHeight();
    requestAnimationFrame(syncScroll);
  }
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('webkitfullscreenchange', onFsChange);

  // 全屏下 Esc 是浏览器"退出全屏"的快捷键，但它也会冒泡到 modal 的 Esc 监听上 ——
  // 不拦住的话，用户按一次 Esc 会连编辑器弹窗一起关掉。捕获阶段截住即可。
  const onEscCapture = (e) => {
    if (e.key === 'Escape' && fsElement()) e.stopPropagation();
  };
  document.addEventListener('keydown', onEscCapture, true);

  // ---------- 查找 / 替换条 ----------
  //
  // 为什么默认隐藏、而且在 DOM 顺序里排在编辑区之后：
  // ui.js 的 modal() 打开弹窗后会自动 focus 里面第一个 input/textarea/select。
  // 查找条里全是输入框 —— 一旦它先出现在 DOM 里（或者不隐藏），打开文件时焦点就会被
  // 查找框抢走：用户一敲键盘是在搜索，而且编辑器根本收不到输入。
  // 所以两道保险：① 默认 display:none；② 在 bodyEl 里排在 editorBox 之后，
  // querySelector 找到的第一个控件永远是编辑用的 textarea。
  // 视觉上它仍在编辑区上方，靠 flex 的 order:-1（只影响绘制顺序，不影响 DOM 顺序）。

  const FIND_MAX = 20000;   // 命中上限：2MB 文件里搜单个字母能有几十万处，全存下来只会拖慢计数
  const HIT_MARGIN = 40;    // 命中层窗口在可视区上下各多画 40 行，避免滚动时每帧重画
  let findOpen = false;
  let matches = [];
  let truncated = false;
  let cur = -1;
  let hitWinFirst = 0;
  let hitWinLast = 0;
  let hitsRaf = 0;
  let measureCtx = null;

  const findInput = h('input.input', { placeholder: '查找内容', style: { width: '160px' } });
  const replaceInput = h('input.input', { placeholder: '替换为', style: { width: '160px' } });
  const caseCb = h('input', { type: 'checkbox', style: { margin: '0' } });
  const findCount = h('span.zpf-fcount', { text: '0/0', title: '当前匹配 / 匹配总数' });
  // onmousedown 里 preventDefault：点按钮不该把焦点从查找框抢走，
  // 否则"输入关键词 → 点下一处 → 继续敲字"会变成敲进编辑区
  const keepFocus = (e) => e.preventDefault();
  const prevBtn = h('button.btn.btn-sm', { text: '↑ 上一处', title: '上一处（Shift+Enter）', onmousedown: keepFocus, onclick: () => stepMatch(-1) });
  const nextBtn = h('button.btn.btn-sm', { text: '↓ 下一处', title: '下一处（Enter）', onmousedown: keepFocus, onclick: () => stepMatch(1) });
  const findBar = h('div.zpf-findbar', {
    style: {
      display: 'none', flexDirection: 'column', gap: '6px',
      marginBottom: '8px', padding: '8px 10px', order: '-1',
      background: 'var(--panel-2)', border: '1px solid var(--border-soft)',
      borderRadius: 'var(--radius-sm)',
    },
  }, [
    // 分两行是刻意的：弹窗默认宽度下"查找 + 替换"挤一行会折行，把关闭按钮甩到
    // 第二行去，看起来像坏了。两组各占一行，任何宽度下都整齐。
    h('div', { style: { display: 'flex', alignItems: 'center', gap: '6px', flexWrap: 'wrap' } }, [
      h('span', { style: { fontSize: '12.5px', color: 'var(--text-mute)' }, text: '查找' }),
      findInput,
      findCount,
      prevBtn,
      nextBtn,
      h('label', {
        style: { display: 'flex', alignItems: 'center', gap: '3px', fontSize: '12px', cursor: 'pointer' },
        title: '区分大小写',
      }, [caseCb, h('span', { text: 'Aa' })]),
      h('div', { style: { flex: '1' } }),
      h('button.btn.btn-sm', { text: '×', title: '关闭查找（Esc）', onclick: closeFind }),
    ]),
    h('div', { style: { display: 'flex', alignItems: 'center', gap: '6px', flexWrap: 'wrap' } }, [
      h('span', { style: { fontSize: '12.5px', color: 'var(--text-mute)' }, text: '替换为' }),
      replaceInput,
      h('button.btn.btn-sm', { text: '替换当前', title: '只替换当前匹配（尚未保存）', onmousedown: keepFocus, onclick: replaceCurrent }),
      h('button.btn.btn-sm', { text: '全部替换', title: '替换全部匹配（尚未保存）', onmousedown: keepFocus, onclick: replaceAll }),
    ]),
  ]);

  /**
   * haystack(q) —— 按当前"区分大小写"开关返回用于 indexOf 的文本与关键词。
   * 查找一律按**纯文本**处理，不把用户输入当正则（正则会让 `.` `*` `(` 这类
   * 输入变成语法错误或意外命中，普通用户只会觉得"搜不到"）。
   */
  function haystack(q) {
    const text = editor.value;
    if (caseCb.checked) return { src: text, hay: text, needle: q };
    const lo = text.toLowerCase();
    const nlo = q.toLowerCase();
    // 个别 Unicode 字符（如 'İ'）小写化后长度会变，长度一旦不同，命中下标就和原文
    // 对不上（会画错位置、替换错字符）。这种极少数情况退回区分大小写：宁可少命中，不能错位。
    if (lo.length !== text.length || nlo.length !== q.length) return { src: text, hay: text, needle: q };
    return { src: text, hay: lo, needle: nlo };
  }

  function computeMatches() {
    const q = findInput.value;
    matches = [];
    truncated = false;
    if (!q) return;
    const { hay, needle } = haystack(q);
    let from = 0;
    for (;;) {
      const at = hay.indexOf(needle, from);
      if (at < 0) break;
      matches.push({ start: at, end: at + q.length });
      if (matches.length >= FIND_MAX) { truncated = true; break; }
      from = at + q.length;
    }
  }

  function lineAt(text, pos) {
    let line = 0;
    for (let i = text.indexOf('\n'); i >= 0 && i < pos; i = text.indexOf('\n', i + 1)) line++;
    return line;
  }

  function updateFindUI() {
    const q = findInput.value;
    if (!q) findCount.textContent = '0/0';
    else if (!matches.length) findCount.textContent = '无匹配';
    else findCount.textContent = `${cur + 1}/${matches.length}${truncated ? '+' : ''}`;
    findCount.style.color = q && !matches.length ? '#f87171' : 'var(--text-mute)';
    findCount.title = truncated ? `命中超过 ${FIND_MAX} 处，只统计并跳到前 ${FIND_MAX} 处` : '当前匹配 / 匹配总数';
    prevBtn.disabled = !matches.length;
    nextBtn.disabled = !matches.length;
  }

  function syncHits() {
    // 窗口首行在文档里的 y 是 hitWinFirst*行高，所以整体上移这么多再减掉 scrollTop
    hitInner.style.transform = `translate(${-editor.scrollLeft}px, ${hitWinFirst * ZPF_LINE_HEIGHT - editor.scrollTop}px)`;
  }

  function scheduleHits() {
    if (hitsRaf) return;
    hitsRaf = requestAnimationFrame(() => { hitsRaf = 0; paintHits(); });
  }

  /**
   * paintHits() —— 把命中画进命中层（当前匹配换一个更醒目的颜色）。
   * 只画可视区附近的窗口：大文件退回纯文本模式后 textarea 自己已经在渲染整篇文本，
   * 再整篇镜像一份、每次滚动都重排，就是白白的双倍开销。
   */
  function paintHits() {
    // 最小化时编辑器是 display:none，clientHeight 为 0：这时候画命中只会画出一个
    // 一行的窗口（错误的色块位置）。直接跳过，还原时由 restoreView() 重画。
    if (boxHidden()) { hitLayer.style.display = 'none'; return; }
    if (!findOpen || !findInput.value || !matches.length) {
      hitLayer.style.display = 'none';
      clear(hitInner);
      return;
    }
    const text = editor.value;
    const viewH = editor.clientHeight || 0;
    const firstView = Math.floor(editor.scrollTop / ZPF_LINE_HEIGHT);
    const lastView = firstView + Math.ceil(viewH / ZPF_LINE_HEIGHT) + 1;
    const firstLine = Math.max(0, firstView - HIT_MARGIN);
    const lastLine = lastView + HIT_MARGIN;

    // 行号 → 字符下标（数换行）。纯文本模式的大文件下这是 O(n)，但只在重画时做一次，
    // 比把整篇文本镜像进 DOM 便宜得多。
    let line = 0;
    let pos = 0;
    while (line < firstLine && pos < text.length) {
      const nl = text.indexOf('\n', pos);
      if (nl < 0) break;
      pos = nl + 1; line++;
    }
    const winStart = pos;
    const winFirst = line;   // 窗口首行在文件里的真实行号（文件比 firstLine 短时会更小）
    while (line <= lastLine && pos < text.length) {
      const nl = text.indexOf('\n', pos);
      if (nl < 0) { pos = text.length; break; }
      pos = nl + 1; line++;
    }
    const winEnd = pos;

    const curMatch = cur >= 0 ? matches[cur] : null;
    const frag = document.createDocumentFragment();
    let p = winStart;
    for (const mt of matches) {
      if (mt.end <= winStart) continue;
      if (mt.start >= winEnd) break;
      const s = Math.max(mt.start, winStart);
      const e = Math.min(mt.end, winEnd);
      if (s > p) frag.appendChild(document.createTextNode(text.slice(p, s)));
      frag.appendChild(h('mark', {
        class: mt === curMatch ? 'zpf-hit zpf-hit-cur' : 'zpf-hit',
        text: text.slice(s, e),
      }));
      p = e;
    }
    if (p < winEnd) frag.appendChild(document.createTextNode(text.slice(p, winEnd)));

    clear(hitInner);
    hitInner.appendChild(frag);
    hitWinFirst = winFirst;
    hitWinLast = line;
    hitLayer.style.display = '';
    syncHits();
  }

  /** 用 canvas 量同一字体下的文字宽度：中文这类宽字符也能量准（按字符数×单宽算会偏）。 */
  function textWidth(s) {
    if (!measureCtx) measureCtx = document.createElement('canvas').getContext('2d');
    measureCtx.font = `${ZPF_FONT_SIZE}px ${getComputedStyle(editor).fontFamily}`;
    return measureCtx.measureText(s).width;
  }

  /** 把当前匹配滚进可视区（只动编辑器内部滚动，绝不 scrollIntoView，否则整个页面会被顶跑）。 */
  function revealMatch(i) {
    const mt = matches[i];
    if (!mt) return;
    const text = editor.value;
    const lineTop = lineAt(text, mt.start) * ZPF_LINE_HEIGHT;
    const viewH = editor.clientHeight;
    if (lineTop - ZPF_LINE_HEIGHT < editor.scrollTop) {
      editor.scrollTop = Math.max(0, lineTop - ZPF_LINE_HEIGHT);
    } else if (lineTop + 2 * ZPF_LINE_HEIGHT > editor.scrollTop + viewH) {
      editor.scrollTop = Math.max(0, lineTop + 2 * ZPF_LINE_HEIGHT - viewH);
    }
    // 横向：只有在匹配列真的滚出视野时才动，两侧各留 24px 余量
    const lineStart = text.lastIndexOf('\n', mt.start - 1) + 1;
    const before = text.slice(lineStart, mt.start).replace(/\t/g, '    ');
    const self = text.slice(mt.start, mt.end).replace(/\t/g, '    ');
    const x0 = textWidth(before);
    const x1 = x0 + textWidth(self);
    if (x0 - 24 < editor.scrollLeft) editor.scrollLeft = Math.max(0, x0 - 24);
    else if (x1 + 24 > editor.scrollLeft + editor.clientWidth) {
      editor.scrollLeft = Math.max(0, x1 + 24 - editor.clientWidth);
    }
    // 选区也设到匹配上：一是让"替换当前"有明确目标，二是用户点回编辑区时光标正好在匹配处。
    // 注意不能 focus 编辑器 —— 焦点必须留在查找框里，否则"输入即查找"就断了；
    // 失焦的 textarea 不绘制选区，所以可见的命中由 hitLayer 负责画（见上面的说明）。
    editor.setSelectionRange(mt.start, mt.end);
    syncScroll();
  }

  /** 重算命中并把"当前匹配"钉在 anchorStart 之后第一个（anchorStart 为空则从光标处往后找）。 */
  function recount(anchorStart) {
    const prev = cur >= 0 && matches[cur] ? matches[cur].start : null;
    computeMatches();
    const anchor = anchorStart != null ? anchorStart : prev;
    if (!matches.length) cur = -1;
    else if (anchor != null) {
      const i = matches.findIndex((m) => m.start >= anchor);
      cur = i < 0 ? matches.length - 1 : i;
    } else {
      // 在文件中间按 Ctrl+F 时不应该跳回文件头，从光标处往后找第一个
      const i = matches.findIndex((m) => m.end > editor.selectionStart);
      cur = i < 0 ? 0 : i;
    }
    updateFindUI();
  }

  /** 改文本、更新计数、把匹配重算一遍并跳到 anchorStart 之后的下一个匹配。 */
  function findRefresh(anchorStart) {
    recount(anchorStart);
    if (cur >= 0) revealMatch(cur);
    paintHits();
  }

  /** 下一处 / 上一处：到头循环回绕。 */
  function stepMatch(delta) {
    if (!matches.length) return;
    cur = (cur + delta + matches.length) % matches.length;
    updateFindUI();
    revealMatch(cur);
    paintHits();
  }

  /** 替换/直接改文本后的统一收口：滚动位置要显式放回去。 */
  function applyEdited(next, anchorStart) {
    const y = editor.scrollTop;
    const x = editor.scrollLeft;
    editor.value = next;
    // 直接给 value 赋值在部分浏览器会把滚动位置弹回开头，存一下再放回去
    editor.scrollTop = y;
    editor.scrollLeft = x;
    dirty = true;
    stat.textContent = `${next.length} 字符（已修改）`;
    scheduleRender();
    findRefresh(anchorStart);
  }

  function replaceCurrent() {
    if (cur < 0 || !matches.length) { toast('没有可替换的匹配', 'warn'); return; }
    const mt = matches[cur];
    const rep = replaceInput.value;
    const next = editor.value.slice(0, mt.start) + rep + editor.value.slice(mt.end);
    // 锚在插入内容之后：连续点"替换当前"会顺着往下替换，和编辑器里的替换流程一致
    applyEdited(next, mt.start + rep.length);
  }

  function replaceAll() {
    const q = findInput.value;
    if (!q) { toast('请先输入查找内容', 'warn'); return; }
    // 这里单独扫一遍而**不复用 matches**：matches 有 FIND_MAX 上限（大文件里搜单个
    // 字母会撞上），拿它来"全部替换"会只替换前 2 万处却不告诉用户。
    const { src, hay, needle } = haystack(q);
    const rep = replaceInput.value;
    let out = '';
    let pos = 0;
    let n = 0;
    for (;;) {
      const at = hay.indexOf(needle, pos);
      if (at < 0) break;
      out += src.slice(pos, at) + rep;
      pos = at + q.length;
      n++;
    }
    if (!n) { toast('没有可替换的匹配', 'warn'); return; }
    out += src.slice(pos);
    applyEdited(out, 0);
    toast(`已替换 ${n} 处（尚未保存）`, 'ok');
  }

  function openFind() {
    if (!findOpen) {
      findOpen = true;
      findBar.style.display = 'flex';
      // 让出查找条占的高度，别把弹窗顶过 88vh 上限（见 applyHeight 的说明）
      applyHeight();
      requestAnimationFrame(syncScroll);
    }
    // 再按一次 Ctrl+F 或点按钮时把已有关键词选中，方便直接覆盖重输
    findInput.focus();
    findInput.select();
    findRefresh();
  }

  function closeFind() {
    if (!findOpen) return;
    findOpen = false;
    // 保留关键词和命中不删（matches 留着），但要清掉画出来的色块
    findBar.style.display = 'none';
    clear(hitInner);
    hitLayer.style.display = 'none';
    applyHeight();                 // 把高度还给编辑区
    requestAnimationFrame(syncScroll);
    // 焦点还给编辑器：按 Esc 关掉查找条之后，用户通常就是要接着改代码
    editor.focus();
  }

  findInput.addEventListener('input', () => findRefresh());
  findInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); stepMatch(e.shiftKey ? -1 : 1); }
  });
  replaceInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); replaceCurrent(); }
  });
  caseCb.addEventListener('change', () => findRefresh());

  // 为什么快捷键挂在 document 的**捕获阶段**：
  // ① 焦点在查找框里时，Ctrl+F / Esc 不会冒泡到 textarea 的监听器上；
  // ② ui.js 的 modal() 也是在 document 上（冒泡阶段）监听 Esc 的，捕获阶段先跑，
  //    这样"查找条开着 → Esc 只关查找条；查找条关着 → Esc 才关弹窗"才成立。
  const onEditorKey = (e) => {
    const key = e.key || '';
    const mod = e.ctrlKey || e.metaKey;
    if (key === 'Escape') {
      // 屏幕全屏时 Esc 归浏览器"退出全屏"用（见上面的 onEscCapture），
      // 这时不能抢：否则 preventDefault 会让用户退不出全屏。
      if (findOpen && !fsElement()) {
        e.preventDefault();
        e.stopPropagation();   // 拦住 modal 的 Esc，别把整个编辑弹窗一起关掉
        closeFind();
      }
      return;
    }
    if (mod && !e.altKey && key.toLowerCase() === 'f') {
      e.preventDefault();      // 拦掉浏览器自带的查找框，否则它会盖在面板上
      openFind();
      return;
    }
    // 焦点在查找框里时 Ctrl+S 也要保存。焦点在编辑区里时由 textarea 自己的监听处理，
    // 这里不重复触发（否则会写两次文件、弹两个 toast）。
    if (mod && findOpen && findBar.contains(e.target) && key.toLowerCase() === 's') {
      e.preventDefault();
      writeBack();
    }
  };
  document.addEventListener('keydown', onEditorKey, true);

  // ---------- 工具栏 ----------
  const maxBtn = h('button.btn.btn-sm', {
    text: '⛶ 最大化', title: '撑满浏览器窗口（不进入系统全屏）',
    onclick: () => setMaximized(!maximized),
  });
  const fsBtn = h('button.btn.btn-sm', {
    text: '⛶ 屏幕全屏', title: '调用系统全屏，Esc 退出',
    onclick: toggleFullscreen,
  });
  // 配色选择：两档（跟随面板 / Monokai），选择存 localStorage（见 ZPF_THEME_KEY）。
  // 排在工具栏最前面，避免被右侧的查找/全屏按钮挤到换行之外看不见。
  const themeSel = h('select.select', {
    title: '编辑器配色（选择会记住）',
    style: { width: 'auto', fontSize: '12.5px', padding: '5px 26px 5px 8px' },
    onchange: () => applyEditorTheme(themeSel.value),
  }, [
    h('option', { value: ZPF_THEME_PANEL, text: '配色：跟随面板' }),
    h('option', { value: ZPF_THEME_MONOKAI, text: '配色：Monokai' }),
  ]);
  themeSel.value = readEditorTheme();

  const toolbar = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '8px' } }, [
    themeSel,
    h('code.code', { text: entry.path }),
    stat,
    langPill,
    h('div', { style: { flex: '1' } }),
    h('button.btn.btn-sm', { text: '🔍 查找替换', title: '查找 / 替换（Ctrl+F）', onclick: openFind }),
    maxBtn,
    fsBtn,
  ]);

  const bodyEl = h('div', { style: { display: 'flex', flexDirection: 'column', flex: '1 1 auto', minHeight: '0' } }, [
    toolbar,
    editorBox,
    // 查找条排在编辑区之后是有意的，见上面"为什么默认隐藏"的说明；
    // 视觉位置由它自己的 order:-1 决定（显示在编辑区上方）
    findBar,
    bigHint,
    h('div.hint', { style: { marginTop: '8px' }, text: '保存采用"先写临时文件再替换"，写入中断不会破坏原文件。' }),
  ]);

  async function writeBack() {
    try {
      await api.fileWrite(entry.path, editor.value);
      dirty = false;
      syncStatus();
      toast('已保存', 'ok');
      return true;
    } catch (e) { toast(e.message, 'err', 10000); return false; }
  }

  // 未保存时才拦一道。ui.js 的 onRequestClose 对"关闭按钮 / 遮罩 / Esc"三条路径一视同仁，
  // 而我们下面把后两条关掉了，所以实际只有关闭按钮会走到这里。
  function confirmDiscard() {
    return !dirty || confirm('有未保存的修改，确定关闭？');
  }

  // 未保存提示写进标题栏（最小化后它是唯一还看得见的区域），否则收起来之后
  // 用户完全不知道里面还有没保存的改动。
  function syncStatus() {
    // m 可能还没赋值（本函数只在 modal() 之后被调用，这里只是兜底）
    m?.setStatus(dirty ? '● 未保存' : '');
  }

  m = modal({
    title: `编辑：${entry.name}`,
    wide: true,
    body: bodyEl,
    footer: () => [
      h('button.btn', {
        text: '取消',
        // 已保存（或没改过）时直接关，走不到确认框；有改动才问
        onclick: () => { if (confirmDiscard()) m.close(); },
      }),
      h('button.btn.btn-primary', {
        text: '保存',
        onclick: async () => { if (await writeBack()) m.close(); },
      }),
    ],
    // ① 只能通过关闭按钮关闭：Esc 与点遮罩都不关。
    //    为什么只给文件编辑器：其它弹窗都是"填错就重来"的短表单，点遮罩放弃是符合
    //    直觉的；编辑器里可能是十几分钟的改动，误触一下就没了的代价太大。
    closeOnEsc: false,
    closeOnBackdrop: false,
    onRequestClose: confirmDiscard,
    // ② 最小化：点标题栏的「—」收成右下角一条标题栏，再点还原。
    minimizable: true,
    onMinimize: (on) => { collapsed = on; if (!on) restoreView(); },
    onClose: () => {
      document.removeEventListener('fullscreenchange', onFsChange);
      document.removeEventListener('webkitfullscreenchange', onFsChange);
      document.removeEventListener('keydown', onEscCapture, true);
      document.removeEventListener('keydown', onEditorKey, true);
      // 关弹窗时如果还在屏幕全屏，必须主动退出，否则会留在一块空白全屏层上
      if (fsElement()) {
        const exit = document.exitFullscreen || document.webkitExitFullscreen;
        if (exit) { const p = exit.call(document); if (p && p.catch) p.catch(() => {}); }
      }
    },
  });

  /**
   * applyEditorTheme(v) —— 切编辑器配色并把选择记下来。
   * 用户没选 Monokai 时（默认）面板原来的浅色/深色规则原样生效。
   */
  function applyEditorTheme(v) {
    const val = v === ZPF_THEME_MONOKAI ? ZPF_THEME_MONOKAI : ZPF_THEME_PANEL;
    applyEditorThemeTo(m.el, val);
    saveEditorTheme(val);
    themeSel.value = val;
  }

  /**
   * restoreView() —— 从"最小化"还原之后把视图对齐回来。
   *
   * 最小化只是给编辑区 display:none，textarea 的 value / 光标 / 滚动位置都是浏览器
   * 自己保着的，所以这里不需要（也不能）重建 DOM。但有两件事必须补：
   *   ① 还原后编辑器重新获得焦点，用户接着敲字不用再点一下；
   *   ② 高亮层与行号槽是 transform 平移的，隐藏期间没同步过，要重画一次。
   */
  function restoreView() {
    // 之前 render() 在最小化期间被跳过，这里补一次完整重绘（含行号、命中层、滚动同步）
    render();
    editor.focus();
    // 还原后可见高度变了，命中层的可视窗口要按新尺寸重画
    if (findOpen) requestAnimationFrame(() => { paintHits(); syncScroll(); });
  }

  // 初始配色：读 localStorage，打开文件就应用，不需要用户每次重选
  applyEditorTheme(themeSel.value);
  syncStatus();

  editor.addEventListener('input', () => {
    dirty = true;
    stat.textContent = `${editor.value.length} 字符（已修改）`;
    syncStatus(); // 标题栏的"● 未保存"提示（最小化后这是唯一看得见的地方）
    scheduleRender();
  });
  editor.addEventListener('scroll', syncScroll);
  editor.addEventListener('focus', () => {
    editorBox.style.borderColor = 'var(--brand)';
    // 首次聚焦时把光标和滚动拉回开头。
    // 坑：modal() 会自动 focus 第一个输入框，而给 textarea 赋 value 会把光标放在文末，
    // focus 又会把光标滚进视野 —— 结果是打开文件直接停在最后一行，很莫名其妙。
    if (firstFocus) {
      firstFocus = false;
      editor.setSelectionRange(0, 0);
      editor.scrollTop = 0;
      editor.scrollLeft = 0;
      syncScroll();
    }
  });
  editor.addEventListener('blur', () => { editorBox.style.borderColor = 'var(--border)'; });

  // Ctrl/Cmd+S 保存（不关闭弹窗，和原来一致）
  editor.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      writeBack();
    }
  });

  render(); // 先画好再让用户看到，避免弹窗刚出现时高亮层是空的
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
