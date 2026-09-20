// permissions.js —— 「权限」页：逐项申请 macOS 授权。
//
// 后端 GET 只下发**不碰受保护路径**的判据；真正读一次只发生在用户点「申请」+
// 二次确认之后（坑 191）。未安装的应用后端根本不返回它的条目，这一页因此不会给
// 它渲染任何灰色占位块 —— 页面对条目是完全数据驱动的。

import { api, apiURL } from './api.js';
import { taskCenter } from './tasks.js';
import { h, clear, toast, appendAll, confirmBox } from './ui.js';

const STATUS = {
  granted: { text: '已授权', cls: 'ok' },
  denied: { text: '被拒绝', cls: 'danger' },
  needs_console: { text: '需要你在这台机器前', cls: 'warn' },
  unknown: { text: '未确认', cls: '' },
};

function fmtTime(s) {
  const t = Date.parse(s || '');
  return Number.isNaN(t) ? '' : new Date(t).toLocaleString();
}

export function PermissionsView(content, ctx = {}) {
  clear(content);
  const statusBox = h('div.card-body');
  const listBox = h('div');
  const resultBox = h('div');
  const refreshBtn = h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(true) });
  let snapshot = null;

  appendAll(content,
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '权限' }),
        h('div.spacer'),
        h('span.sub', { text: '逐项申请；系统弹窗需要你在这台机器前点「允许」' }),
        refreshBtn,
      ]),
      statusBox,
      resultBox,
    ]),
    listBox,
  );

  let firstLoad = true;
  load(false);

  async function load(showToast) {
    refreshBtn.disabled = true;
    refreshBtn.textContent = '读取中…';
    if (firstLoad) {
      clear(statusBox);
      statusBox.append(h('div.hint', { text: '正在读取权限状态…' }));
    }
    let data;
    try {
      data = await api.get(apiURL('permissions'));
    } catch (e) {
      firstLoad = false;
      refreshBtn.disabled = false;
      refreshBtn.textContent = '⟳ 刷新';
      clear(statusBox);
      statusBox.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.danger', { text: '权限状态读取失败' }),
        h('span.sub', { text: (e && e.message) || String(e) }),
      ]));
      return;
    }
    snapshot = data || {};
    firstLoad = false;
    refreshBtn.disabled = false;
    refreshBtn.textContent = '⟳ 刷新';
    renderStatus(snapshot);
    renderList(snapshot);
    if (showToast) toast('已刷新权限状态', 'ok');
  }

  function renderStatus(data) {
    clear(statusBox);
    if (!data.has_console_session) {
      statusBox.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.warn', { text: '现在没人在机器前' }),
        h('span.hint', { text: ' 系统弹窗没人点会被记成拒绝；请到这台机器的屏幕前再申请。' }),
      ]));
    } else {
      statusBox.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.ok', { text: '有人在机器前' }),
        h('span.hint', { text: ' 控制台用户：' + (data.console_user || '-') + '；申请后请在这台机器的屏幕上点「允许」。' }),
      ]));
    }
    if (data.is_remote) {
      statusBox.append(h('div.hint', { text: '你正在远程操作面板：只要这台机器前有人能点弹窗，申请就有效。' }));
    }
  }

  function renderList(data) {
    clear(listBox);
    const items = Array.isArray(data.items) ? data.items : [];
    items.forEach((it) => listBox.append(itemCard(it)));
  }

  function itemCard(it) {
    const st = STATUS[it.status] || STATUS.unknown;
    const body = h('div.card-body');

    const marks = [h('span.pill' + (st.cls ? '.' + st.cls : ''), { text: st.text })];
    if (it.accepts_path) marks.push(h('span.pill', { text: '由它自己的进程弹窗' }));
    body.append(h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center' } }, marks));
    body.append(h('div.hint', { text: it.why || '' }));

    // 判据/历史：读不到就如实说"需要你在机器前点一次才能确认"。
    const hist = it.last_checked_at
      ? ('上次申请 ' + fmtTime(it.last_checked_at) + '：' + (it.last_result || '-'))
      : (it.status_hint || '还没有申请记录');
    body.append(h('div.hint', { text: hist }));

    const facts = [];
    if (it.version) facts.push(['版本', it.version]);
    if (it.exec_path) facts.push(['可执行文件', it.exec_path]);
    if (it.signing_id) facts.push(['签名身份', it.signing_id]);
    if (facts.length) {
      const ul = h('ul.zp-notes-list');
      facts.forEach(([k, v]) => ul.append(h('li', [h('strong', { text: k + '：' }), h('span.mono', { text: v })])));
      body.append(ul);
    }

    const targets = Array.isArray(it.targets) ? it.targets : [];
    if (targets.length) {
      const ul = h('ul.zp-notes-list');
      targets.forEach((t) => ul.append(h('li.mono', { text: t })));
      body.append(h('div', { style: { marginTop: '6px' } }, [h('strong', { text: '申请时会读的目标' }), ul]));
    }

    body.append(applyRow(it));
    if (it.manual_path) body.append(manualDetails(it.manual_path));

    return h('div.card', [
      h('div.card-head', [h('h3', { text: it.title }), h('div.spacer'), h('span.sub', { text: it.id })]),
      body,
    ]);
  }

  // applyRow 是「申请」按钮 + 目录选择（只有需要路径的条目才有）。
  //
  // 按钮可用性贴着后端预检：没人在机器前就禁用并写明原因（后端也会拒绝，不靠前端自觉）。
  function applyRow(it) {
    const disabled = !it.can_apply;
    const why = it.status === 'needs_console'
      ? '现在没人在机器前，弹窗没人点；请到真机操作，或按下面的路径手动授权'
      : (it.status_hint || '');
    const btn = h('button.btn.btn-sm', {
      text: '🔓 申请',
      disabled,
      title: disabled ? why : '先二次确认，然后由对应的身份读一次并让系统弹窗',
      onclick: () => applyPermission(it, btn),
    });

    let pathInput = null;
    if (it.accepts_path) {
      const roots = Array.isArray(it.roots) ? it.roots : [];
      if (roots.length) {
        pathInput = h('select.input', { title: '选择要申请的媒体目录' },
          roots.map((r) => h('option', { value: r, text: r })));
      } else {
        pathInput = h('input.input', {
          type: 'text', placeholder: '填写要申请的媒体目录绝对路径（面板不猜目录）', title: '要申请的媒体目录',
        });
      }
    }

    const row = h('div', { style: { marginTop: '8px', display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      btn,
      pathInput,
      h('span.hint', { text: disabled ? why : '点一下会先二次确认：会读一次、系统会弹窗，需要你在机器前点「允许」。' }),
    ]);
    // 把输入框挂到按钮上，applyPermission 才能取到用户选的目录。
    btn._zpPathInput = pathInput;
    return row;
  }

  // applyPermission 先二次确认再提交任务（202 + task_id，进度走 SSE）。
  // 取消 = 什么都不做（不发请求、不建任务）；后端也要求 body.confirm=true。
  async function applyPermission(it, btn) {
    const needPath = !!it.accepts_path;
    const pathValue = btn._zpPathInput ? String(btn._zpPathInput.value || '').trim() : '';
    if (needPath && !pathValue) {
      toast('请先选择或填写要申请的目录', 'warn');
      return;
    }
    const ok = await confirmBox(
      '接下来会由' + (needPath ? '该应用自己的进程' : '面板') + '读一次目标目录，系统可能弹出授权询问——'
      + '请在这台机器的屏幕上点「允许」。\n'
      + '你现在不在那台机器前的话，先别继续：没人点，系统会把它记成拒绝。',
      { title: '申请权限：' + it.title, okText: '我准备好了' });
    if (!ok) return;
    btn.disabled = true;
    btn.textContent = '申请中…';
    const body = { confirm: true };
    if (needPath) body.path = pathValue;
    await taskCenter.start({
      kind: 'permission_apply',
      target: 'permission-' + it.id,
      title: '申请权限：' + it.title,
      start: () => api.post(apiURL('permissions/' + encodeURIComponent(it.id) + '/apply'), body),
      onDone: afterApply,
    });
    // 提交失败 / 已有同名任务在跑（start 直接返回它的 id，不回调 onDone）都要能再点。
    btn.disabled = false;
    btn.textContent = '🔓 申请';
  }

  function afterApply(m) {
    load(false);
    clear(resultBox);
    if (!m) return;
    const done = m.status === 'succeeded';
    const r = m.result || {};
    const msg = r.message || m.error || '';
    const lines = String(msg).split('\n').map((s) => s.trim()).filter(Boolean);
    const steps = Array.isArray(r.steps) ? r.steps : [];
    const wrap = h('div', [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill' + (done ? '.ok' : '.danger'), { text: done ? '申请完成' : '申请未完成' }),
        h('span.sub', { text: m.title || '' }),
        h('button.btn.btn-sm', { text: '查看任务日志', onclick: () => taskCenter.openTask(m.id) }),
      ]),
      ...lines.map((ln) => h('div', { style: { marginTop: '6px' } }, [h('strong', { text: ln })])),
      steps.length ? (() => {
        const ul = h('ul.zp-notes-list');
        steps.forEach((s) => ul.append(h('li.mono', { text: s })));
        return ul;
      })() : null,
    ]);
    resultBox.append(wrap);
    if (done && msg) toast(msg, 'ok', 10000);
    else if (!done) toast('权限申请失败：' + (m.error || '未知原因'), 'err', 12000);
  }

  // manualDetails 把"去系统设置哪里手动授权"收进折叠项（文案纪律：首屏只留一句）。
  function manualDetails(text) {
    const det = h('details', { style: { marginTop: '8px' } });
    det.append(h('summary', { text: '还是被拒？手动授权路径' }));
    det.append(h('div.hint', { style: { marginTop: '6px' }, text }));
    return det;
  }
}
