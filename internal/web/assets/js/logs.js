// logs.js —— 日志中心页面。
//
// 设计思路：日志排障时最常见的动作是"找最近的错误"，
// 所以页面默认按分类分组、按修改时间倒序，并提供：
//   - 关键字 / 正则 / 级别过滤
//   - 实时尾随（SSE，自动重连）
//   - 一键跳到最新、下载、清空
//
// 日志 key 由服务端映射到真实路径，前端不做路径拼接。

import { api, apiURL } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';

let cache = null;
let currentKey = '';
let filter = { text: '', level: '', regex: false };
let followES = null;

// stopFollow 必须在模块级：LogsView 开头与页面清理都要调用它，
// 而它原先被定义在 openLog 内部，外层调用会直接抛 ReferenceError。
function stopFollow() {
  if (followES) {
    try { followES.close(); } catch { /* 忽略 */ }
    followES = null;
  }
}

export function LogsView(content, ctx = {}) {
  clear(content);
  currentKey = '';
  stopFollow();

  const listBox = h('div', { style: { maxHeight: '620px', overflow: 'auto' } });
  const statBar = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
  const viewerTitle = h('h3', { text: '请选择左侧日志' });
  const viewerBody = h('div', { style: { padding: '16px' } }, [
    h('div.empty', [
      h('div.big', { text: '📜' }),
      h('h4', { text: '从左侧选择一份日志' }),
      h('p', { text: '支持关键字过滤、级别过滤与实时尾随。' }),
    ]),
  ]);
  const viewerTools = h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center', flexWrap: 'wrap' } });

  content.append(
    h('div', { style: { display: 'grid', gridTemplateColumns: 'minmax(240px, 300px) 1fr', gap: 'var(--gap)', alignItems: 'start' } }, [
      h('div.card', { style: { marginBottom: 0 } }, [
        h('div.card-head', [h('h3', { text: '日志文件' }), h('div.spacer')]),
        h('div', { style: { padding: '10px 14px', borderBottom: '1px solid var(--border-soft)' } }, [statBar]),
        h('div.card-body.tight', [listBox]),
      ]),
      h('div.card', { style: { marginBottom: 0 } }, [
        h('div.card-head', [viewerTitle, h('div.spacer'), viewerTools]),
        viewerBody,
      ]),
    ]),
  );

  async function load() {
    clear(listBox);
    listBox.append(h('div.empty', [h('p', { text: '加载中…' })]));
    try {
      cache = await api.logs();
    } catch (e) {
      clear(listBox);
      listBox.append(h('div.empty', [h('p', { text: '读取失败：' + e.message })]));
      return;
    }
    renderStats();
    renderList();
  }

  function renderStats() {
    clear(statBar);
    const s = cache?.stats || {};
    appendAll(statBar,
      h('span.pill', { text: `${s.total_files || 0} 个日志` }),
      h('span.pill', { text: humanSize(s.total_size || 0) }),
      s.largest ? h('span.pill.warn', {
        text: '最大：' + humanSize(s.largest_size),
        title: s.largest,
      }) : null,
      h('button.btn.btn-sm', { text: '⟳', title: '刷新列表', onclick: load }),
    );
  }

  function renderList() {
    clear(listBox);
    const list = cache?.list || [];
    const cats = cache?.categories || [];
    if (!list.length) {
      listBox.append(h('div.empty', [h('p', { text: '没有发现日志文件' })]));
      return;
    }
    // 按分类分组，保持服务端给的顺序
    const groups = new Map();
    for (const e of list) {
      if (!groups.has(e.category)) groups.set(e.category, []);
      groups.get(e.category).push(e);
    }
    for (const cat of cats) {
      const items = groups.get(cat.key);
      if (!items || !items.length) continue;
      listBox.append(h('div.nav-group', { text: cat.icon + ' ' + cat.label }));
      for (const e of items) {
        const active = e.key === currentKey;
        appendAll(listBox, h('div.nav-item' + (active ? '.active' : ''), {
          style: { display: 'flex', gap: '8px', alignItems: 'flex-start', cursor: 'pointer' },
          onclick: () => openLog(e),
        }, [
          h('div', { style: { flex: 1, minWidth: 0 } }, [
            h('div', {
              style: { fontSize: '12.5px', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' },
              text: e.name,
              title: e.path,
            }),
            h('div', {
              style: { fontSize: '10.5px', color: 'var(--text-mute)', marginTop: '2px' },
              text: e.exists ? `${humanSize(e.size)} · ${e.mod_time}` : '尚未生成',
            }),
          ]),
          !e.available && e.exists ? h('span.pill.danger', { text: '无权限' }) : null,
        ]));
      }
    }
  }

  async function openLog(e) {
    currentKey = e.key;
    stopFollow();
    renderList();

    clear(viewerTitle);
    appendAll(viewerTitle, e.name);
    clear(viewerTools);
    appendAll(viewerTools,
      h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px' } }, [
        h('input', {
          type: 'checkbox', checked: true,
          onchange: (ev) => { if (ev.target.checked) startFollow(); else stopFollow(); },
        }),
        h('span', { text: '实时尾随' }),
      ]),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => loadContent() }),
      h('button.btn.btn-sm', {
        text: '⤓ 下载',
        onclick: () => { window.location.href = apiURL(`logs/download?key=${encodeURIComponent(e.key)}`); },
      }),
      e.rotatable ? h('button.btn.btn-sm.btn-danger', {
        text: '🗑 清空',
        title: '清空日志内容（会自动保留尾部备份）',
        onclick: async () => {
          if (!await confirmBox(
            `清空「${e.name}」？\n\n清空前会自动把文件末尾 512KB 备份为同名 .1 文件，` +
            '以便回溯刚发生的问题。', { title: '清空日志', danger: true, okText: '清空' })) return;
          try {
            const r = await api.logTruncate(e.key);
            toast(r.msg, 'ok', 8000);
            load();
          } catch (err) { toast(err.message, 'err', 9000); }
        },
      }) : null,
      h('button.btn.btn-sm', {
        text: '关于该日志',
        onclick: () => showInfo(e),
      }),
    );

    // 过滤工具栏
    const filterBar = h('div', {
      style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '10px' },
    });
    renderFilterBar(filterBar);

    const box = h('pre.logbox', { style: { maxHeight: '520px', minHeight: '260px' }, text: '加载中…' });
    clear(viewerBody);
    appendAll(viewerBody, filterBar, box);

    async function loadContent() {
      stopFollow();
      box.textContent = '加载中…';
      try {
        const q = new URLSearchParams({ key: e.key, lines: '500' });
        if (filter.text) { q.set('filter', filter.text); if (filter.regex) q.set('regex', '1'); }
        if (filter.level) q.set('level', filter.level);
        const r = await api.logRead(q.toString());
        const lines = r.lines || [];
        box.textContent = lines.length ? lines.join('\n') : '（没有符合条件的日志行）';
        box.scrollTop = box.scrollHeight;
      } catch (err) {
        box.textContent = '读取失败：' + err.message;
      }
    }

    function renderFilterBar(bar) {
      clear(bar);
      const text = h('input.input', {
        value: filter.text, placeholder: '关键字过滤（按纯文本匹配）',
        style: { width: '220px', padding: '5px 10px', fontSize: '12.5px' },
        onkeydown: (ev) => { if (ev.key === 'Enter') { filter.text = ev.target.value; loadContent(); } },
      });
      const level = h('select.select', {
        style: { width: 'auto', padding: '5px 26px 5px 8px', fontSize: '12.5px' },
        onchange: (ev) => { filter.level = ev.target.value; loadContent(); },
      }, [
        h('option', { value: '', text: '全部级别' }),
        h('option', { value: 'error', text: '仅错误', selected: filter.level === 'error' }),
        h('option', { value: 'warn', text: '仅警告', selected: filter.level === 'warn' }),
      ]);
      appendAll(bar,
        text,
        level,
        h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12px' } }, [
          h('input', {
            type: 'checkbox', checked: filter.regex,
            onchange: (ev) => { filter.regex = ev.target.checked; },
          }),
          h('span', { text: '正则' }),
        ]),
        h('button.btn.btn-sm', { text: '应用过滤', onclick: () => { filter.text = text.value; loadContent(); } }),
        h('button.btn.btn-sm', {
          text: '清除过滤',
          onclick: () => { filter = { text: '', level: '', regex: false }; renderFilterBar(bar); loadContent(); },
        }),
      );
    }

    async function startFollow() {
      if (followES) return;
      if (!e.available && e.exists) {
        toast('该日志当前无法读取（可能是权限不足）', 'warn');
        return;
      }
      clear(viewerBody);
      appendAll(viewerBody, filterBar, box);
      box.textContent = '正在建立实时连接…\n';
      followES = new EventSource(apiURL(`logs/stream?key=${encodeURIComponent(e.key)}&tail=80`),
        { withCredentials: true });
      followES.onmessage = (ev) => {
        if (!ev.data) return;
        box.textContent += ev.data + '\n';
        // 限制内存：超过 512KB 只保留后半段
        if (box.textContent.length > 512 * 1024) {
          box.textContent = box.textContent.slice(-256 * 1024);
        }
        box.scrollTop = box.scrollHeight;
      };
      followES.addEventListener('close', () => { stopFollow(); box.textContent += '\n（日志流已结束）\n'; });
      followES.onerror = () => { /* 浏览器会自动重连 */ };
    }

    function showInfo(entry) {
      modal({
        title: entry.name,
        body: h('dl.kv', [
          h('dt', { text: '路径' }), h('dd', { text: entry.path }),
          h('dt', { text: '分类' }), h('dd', { text: entry.category }),
          h('dt', { text: '大小' }), h('dd', { text: entry.exists ? humanSize(entry.size) : '尚未生成' }),
          h('dt', { text: '最后修改' }), h('dd', { text: entry.mod_time || '—' }),
          h('dt', { text: '可读取' }), h('dd', { text: entry.available ? '是' : '否' }),
          h('dt', { text: '可清空' }), h('dd', { text: entry.rotatable ? '是（面板产生）' : '否（系统/第三方产生）' }),
        ].concat(entry.description ? [h('dt', { text: '说明' }), h('dd', { text: entry.description })] : [])),
      });
    }

    await loadContent();
    // 默认开启实时尾随
    startFollow();
    registerCleanup(stopFollow);

    viewerBody._cleanup = stopFollow;
  }

  registerCleanup(() => { stopFollow(); });
  load();
}

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 1 : 2) + ' ' + units[i];
}
