// audit.js —— 操作审计页。
//
// 这个页面存在的唯一理由是**检索**：面板里每个敏感动作都会写一条审计
// （站点增删、升级、文件删除、数据库执行…共 129 处写入点），出了问题时
// 要能回答"谁、什么时候、动了什么、成没成"。所以页面的重心是筛选器，
// 而不是把日志铺一屏。
//
// 三个刻意的设计：
//
//  1. **游标分页（"加载更多"）而不是页码。** 审计表只增不减，翻页期间新记录
//     插进来会让 OFFSET 分页出现重复/遗漏；游标（id < before_id）不会漂移。
//     代价是不能跳页——而看审计本来就是从最新往回翻。
//
//  2. **筛选项来自真实数据**（/audit/facets）。动作名是代码里的字符串
//     （site_create、upgrade_stage…），让用户手敲等于没有筛选。
//
//  3. **导出沿用当前筛选**。页面上看到什么，导出的就是什么。

import { api, apiURL } from './api.js';
import { h, clear, toast, appendAll } from './ui.js';

// 筛选状态放在模块级：切走再回来时保留上次的筛选（审计常常要来回对照）。
let filters = {
  q: '', action: '', actor: '', ok: '', from: '', to: '', limit: 50,
};
let facets = null;

export function AuditView(content, ctx = {}) {
  clear(content);

  const headBox = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } });
  const filterBox = h('div', { style: { marginBottom: '10px' } });
  const summary = h('div.hint', { style: { margin: '4px 0 10px' } });
  const tableBox = h('div');

  content.append(
    h('div.card', [
      h('div.card-head', [h('h3', { text: '操作审计' }), h('div.spacer'), headBox]),
      h('div.card-body.tight', [filterBox, summary, tableBox]),
    ]),
  );

  // ---------------- 筛选器 ----------------

  const qInput = h('input.input', {
    placeholder: '关键词（动作/目标/说明/操作者/IP）', value: filters.q,
  });
  const actionSel = h('select.select');
  const actorSel = h('select.select');
  const okSel = h('select.select', {}, [
    h('option', { value: '', text: '全部结果', selected: filters.ok === '' }),
    h('option', { value: '1', text: '仅成功', selected: filters.ok === '1' }),
    h('option', { value: '0', text: '仅失败', selected: filters.ok === '0' }),
  ]);
  const fromInput = h('input.input', { type: 'date', value: filters.from });
  const toInput = h('input.input', { type: 'date', value: filters.to });
  const limitSel = h('select.select', {}, [20, 50, 100, 200].map((n) =>
    h('option', { value: String(n), text: `每页 ${n} 条`, selected: filters.limit === n })));

  function fillSelects() {
    clear(actionSel);
    actionSel.append(h('option', { value: '', text: '全部动作' }));
    ((facets && facets.actions) || []).forEach((a) => {
      actionSel.append(h('option', {
        value: a.value, text: `${a.value}（${a.count}）`, selected: filters.action === a.value,
      }));
    });
    clear(actorSel);
    actorSel.append(h('option', { value: '', text: '全部操作者' }));
    ((facets && facets.actors) || []).forEach((a) => {
      actorSel.append(h('option', {
        value: a.value, text: `${a.value}（${a.count}）`, selected: filters.actor === a.value,
      }));
    });
  }

  function readFilters() {
    filters = {
      q: qInput.value.trim(),
      action: actionSel.value,
      actor: actorSel.value,
      ok: okSel.value,
      from: fromInput.value,
      to: toInput.value,
      limit: Number(limitSel.value) || 50,
    };
  }

  /** exportURL 拼出导出地址（沿用当前筛选）。 */
  function exportURL(format) {
    readFilters();
    const p = new URLSearchParams();
    Object.entries(filters).forEach(([k, v]) => { if (v !== '' && k !== 'limit') p.set(k, v); });
    p.set('format', format);
    return apiURL('audit/export?' + p.toString());
  }

  /** download 用临时 <a> 触发下载：导出接口带 attachment 头，不会离开当前页。 */
  function download(format) {
    const a = h('a', { href: exportURL(format), download: '', style: { display: 'none' } });
    document.body.appendChild(a);
    a.click();
    setTimeout(() => a.remove(), 0);
    toast('已开始导出 ' + format.toUpperCase(), 'ok');
  }

  const btnQuery = h('button.btn.btn-primary.btn-sm', { text: '查询', onclick: () => load(true) });
  const btnReset = h('button.btn.btn-ghost.btn-sm', {
    text: '重置',
    onclick: () => {
      qInput.value = ''; actionSel.value = ''; actorSel.value = '';
      okSel.value = ''; fromInput.value = ''; toInput.value = '';
      filters = { q: '', action: '', actor: '', ok: '', from: '', to: '', limit: filters.limit };
      load(true);
    },
  });

  // 回车即查询（搜关键词是最高频的操作）
  qInput.addEventListener('keydown', (e) => { if (e.key === 'Enter') load(true); });

  filterBox.append(
    h('div', { style: { display: 'grid', gap: '8px', gridTemplateColumns: 'repeat(auto-fit, minmax(170px, 1fr))' } }, [
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '关键词' }), qInput]),
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '动作' }), actionSel]),
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '操作者' }), actorSel]),
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '结果' }), okSel]),
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '起始日期' }), fromInput]),
      h('div.field', { style: { marginBottom: '0' } }, [h('label', { text: '结束日期' }), toInput]),
    ]),
    h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', marginTop: '10px' } }, [
      btnQuery, btnReset, h('div.spacer', { style: { flex: '1' } }), limitSel,
      h('button.btn.btn-ghost.btn-sm', { text: '导出 CSV', onclick: () => download('csv') }),
      h('button.btn.btn-ghost.btn-sm', { text: '导出 JSON', onclick: () => download('json') }),
    ]),
  );

  // ---------------- 列表 ----------------

  let cursor = 0;
  let total = 0;

  function rowFor(e) {
    const detail = [e.detail, e.message].filter(Boolean).join(' | ');
    return h('tr', [
      h('td.num.mono', { style: { whiteSpace: 'nowrap' }, text: e.ts }),
      h('td', { text: e.actor || '—' }),
      h('td.mono', { text: e.ip || '—' }),
      h('td', [h('span.pill' + (e.ok ? '.ok' : '.warn'), { text: e.action })]),
      h('td.mono', { text: e.target || '—' }),
      h('td', [h(e.ok ? 'span.pill.ok' : 'span.pill.warn', { text: e.ok ? '成功' : '失败' })]),
      h('td', {
        style: { whiteSpace: 'pre-wrap', wordBreak: 'break-word', maxWidth: '420px' },
        title: detail,
        text: detail || '—',
      }),
    ]);
  }

  async function load(reset) {
    if (reset) cursor = 0;
    btnQuery.disabled = true;
    if (reset) {
      clear(tableBox);
      tableBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在检索…' })]));
    }
    readFilters();

    const params = { limit: filters.limit };
    if (cursor > 0) params.before_id = cursor;
    ['q', 'action', 'actor', 'ok', 'from', 'to'].forEach((k) => {
      if (filters[k] !== '') params[k] = filters[k];
    });

    let res;
    try {
      res = await api.auditQuery(params);
    } catch (e) {
      clear(tableBox);
      tableBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '检索失败' }),
        h('p', { text: e.message }),
      ]));
      btnQuery.disabled = false;
      return;
    }
    btnQuery.disabled = false;

    total = res.total || 0;
    summary.textContent = total === 0
      ? '没有符合条件的记录'
      : `共 ${total} 条，其中失败 ${res.failed || 0} 条`;

    const list = res.list || [];
    if (cursor === 0) {
      clear(tableBox);
      if (list.length === 0) {
        tableBox.append(h('div.empty', [
          h('div.big', { text: '🧾' }),
          h('h4', { text: '没有符合条件的记录' }),
          h('p', { text: '可以放宽筛选条件，或清空关键词再试。' }),
        ]));
        headBox.textContent = '';
        return;
      }
      tableBox.append(h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '时间' }), h('th', { text: '操作者' }), h('th', { text: '来源 IP' }),
          h('th', { text: '动作' }), h('th', { text: '目标' }), h('th', { text: '结果' }),
          h('th', { text: '说明' }),
        ])]),
        h('tbody', { id: 'audit-rows' }, list.map(rowFor)),
      ]));
    } else {
      const tbody = tableBox.querySelector('#audit-rows');
      list.forEach((e) => tbody && tbody.append(rowFor(e)));
    }

    const shown = tableBox.querySelectorAll('#audit-rows tr').length;
    cursor = res.next_before_id || 0;

    // 底部：还能翻就显示"加载更多"，否则说明已到底
    const foot = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', marginTop: '12px' } });
    if (res.has_more && cursor > 0) {
      foot.append(h('button.btn.btn-ghost.btn-sm', {
        text: `加载更多（已显示 ${shown} / ${total}）`,
        onclick: () => load(false),
      }));
    } else {
      foot.append(h('span.hint', { text: `已显示全部 ${shown} 条` }));
    }
    const old = tableBox.querySelector('.audit-foot');
    if (old) old.remove();
    foot.className = 'audit-foot';
    tableBox.append(foot);

    headBox.textContent = '';
    headBox.append(h('span.pill', { text: `${total} 条`, title: '当前筛选命中数' }));
    if (res.failed > 0) {
      headBox.append(h('span.pill.warn', { text: `失败 ${res.failed}`, title: '当前筛选中的失败数' }));
    }
  }

  // ---------------- 启动 ----------------

  (async () => {
    try {
      facets = await api.auditFacets();
    } catch (e) {
      facets = { actions: [], actors: [] };
      toast('读取筛选项失败：' + e.message, 'err');
    }
    fillSelects();
    await load(true);
  })();

  headBox.append(h('button.btn.btn-ghost.btn-sm', { text: '刷新', onclick: () => load(true) }));
}
