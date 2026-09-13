// files.js —— 文件管理页面。
//
// 交互设计：
//   - 面包屑 + 目录树式导航；每行显示权限/属主/大小/时间
//   - 双击目录进入，双击文件打开编辑器（带语法高亮的简易编辑器）
//   - 多选 + 批量操作（删除/压缩）；上传支持拖拽与多选
//   - 所有破坏性操作都要二次确认，删除目录必须显式勾选"递归"

import { api, apiURL } from './api.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll, esc } from './ui.js';
import { registerCleanup } from './app.js';

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
      if (e.target.files.length) await uploadFiles(e.target.files);
      e.target.value = '';
    },
  });

  appendAll(content, 
    h('div.card', [
      h('div.card-head', [crumbs]),
      h('div', { style: { padding: '10px 14px', borderBottom: '1px solid var(--border-soft)', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [toolbar]),
      h('div.card-body.tight', [tableBox]),
      statusBar,
      fileInput,
    ]),
  );

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
      selCount > 0 ? h('button.btn.btn-sm', { text: '压缩', onclick: compressSelected }) : null,
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
  async function uploadFiles(files) {
    const fd = new FormData();
    appendAll(fd, 'dir', cwd);
    for (const f of files) appendAll(fd, 'files', f, f.name);
    const t = toast(`正在上传 ${files.length} 个文件…`, 'info', 0);
    try {
      // 上传用原生 fetch：FormData 不能走 JSON 封装的 request()
      const res = await fetch(apiURL('files/upload'), {
        method: 'POST', body: fd, credentials: 'same-origin',
        headers: { 'X-CSRF-Token': readCookie('zp_csrf') },
      });
      const data = await res.json();
      t.remove();
      if (!res.ok || data.ok === false) throw new Error(data.msg || `HTTP ${res.status}`);
      toast(data.data.msg || '上传完成', 'ok');
      if (data.data.failed?.length) toast('部分失败：' + data.data.failed.join('；'), 'warn', 12000);
      load(cwd);
    } catch (e) {
      t.remove();
      toast('上传失败：' + e.message, 'err', 12000);
    }
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

    const editor = h('textarea.textarea', {
      value: res.content,
      style: { minHeight: '420px', width: '100%', fontSize: '12.5px', lineHeight: '1.6' },
      spellcheck: false,
    });
    const stat = h('span.sub', { text: `${res.content.length} 字符` });
    editor.addEventListener('input', () => { stat.textContent = `${editor.value.length} 字符（已修改）`; });

    let dirty = false;
    editor.addEventListener('input', () => { dirty = true; });

    const m = modal({
      title: `编辑：${entry.name}`,
      wide: true,
      body: h('div', [
        h('div', { style: { marginBottom: '8px', display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('code.code', { text: entry.path }),
          stat,
          h('button.btn.btn-sm', {
            text: '查找替换',
            onclick: () => replaceInEditor(editor, entry),
          }),
        ]),
        editor,
        h('div.hint', { style: { marginTop: '8px' }, text: '保存采用"先写临时文件再替换"，写入中断不会破坏原文件。' }),
      ]),
      footer: (close) => [
        h('button.btn', {
          text: '取消',
          onclick: () => { if (!dirty || confirm('有未保存的修改，确定关闭？')) close(); },
        }),
        h('button.btn.btn-primary', {
          text: '保存',
          onclick: async () => {
            try {
              await api.fileWrite(entry.path, editor.value);
              dirty = false;
              toast('已保存', 'ok');
              close();
            } catch (e) { toast(e.message, 'err', 10000); }
          },
        }),
      ],
      onClose: () => { },
    });

    // Ctrl/Cmd+S 保存
    const onKey = (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault();
        api.fileWrite(entry.path, editor.value)
          .then(() => { dirty = false; toast('已保存', 'ok'); })
          .catch((err) => toast(err.message, 'err'));
      }
    };
    editor.addEventListener('keydown', onKey);
    registerCleanup(() => editor.removeEventListener('keydown', onKey));
  }

  function replaceInEditor(editor, entry) {
    const find = h('input.input', { placeholder: '查找内容' });
    const repl = h('input.input', { placeholder: '替换为' });
    const m = modal({
      title: '查找替换',
      body: h('div', [
        h('div.field', [h('label', { text: '查找' }), find]),
        h('div.field', [h('label', { text: '替换为' }), repl]),
        h('div.hint', { text: '仅作用于当前编辑框内容，保存后才写入文件。查找按纯文本处理，不会把特殊字符当正则。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn', {
          text: '全部替换',
          onclick: () => {
            if (!find.value) return;
            const parts = editor.value.split(find.value);
            const n = parts.length - 1;
            editor.value = parts.join(repl.value);
            editor.dispatchEvent(new Event('input'));
            toast(`已替换 ${n} 处（尚未保存）`, 'ok');
            close();
          },
        }),
      ],
    });
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

// ---------- 小工具 ----------
function basename(p) { return String(p).split('/').filter(Boolean).pop() || '/'; }
function dirname(p) { const parts = String(p).split('/'); parts.pop(); return parts.join('/') || '/'; }

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
