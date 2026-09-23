// mobile.js —— 手机一屏（C4）：状态 + 常用操作。
//
// 为什么单独一页而不是"把桌面版自适应"：手机上真正要看的是"有没有事"和
// "点一下能不能恢复"，桌面版的信息密度在 6 寸屏上等于什么都看不见。
// 这一页只放三块：**状态**（CPU/内存/磁盘/服务异常）、**问题清单**、**常用操作**。
//
// 判据全部来自后端真实探测（/system/info 与 /services），不做任何本地猜测；
// 写操作（重启服务）走同一个 serviceAction 接口，失败如实弹错误。

import { api } from './api.js';
import { state } from './app.js';
import { h, clear, toast, confirmBox, appendAll, bytes, pct, duration, levelOf } from './ui.js';
import { isStandalone } from './pwa.js';

export function MobileView(content, ctx = {}) {
  clear(content);

  const statusBox = h('div.card');
  const problemBox = h('div.card');
  const actionBox = h('div.card');
  const refreshBtn = h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(true) });

  // 主屏打开时给一次安装说明（没装过的用户不知道还能这么做）。
  const installHint = isStandalone()
    ? null
    : h('div.card', [
      h('div.card-body', [
        h('div.hint', {
          text: '手机上点「分享 → 添加到主屏幕」，就能像 App 一样全屏打开（无需再输网址）。',
        }),
      ]),
    ]);

  appendAll(content,
    h('div.card', [
      h('div.card-head', [h('h3', { text: '手机一屏' }), h('div.spacer'), refreshBtn]),
      h('div.card-body', [
        h('div.hint', { text: '只为手机准备：一眼看状态，一键恢复。完整功能仍在左侧菜单里。' }),
      ]),
    ]),
    statusBox,
    problemBox,
    actionBox,
    installHint,
  );

  function metricRow(label, percent, extra) {
    const level = levelOf(percent);
    const bar = h('div.bar', [h('i', { style: { width: Math.max(0, Math.min(100, percent)) + '%' } })]);
    return h('div', { style: { marginBottom: '10px' } }, [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'baseline' } }, [
        h('span', { text: label }),
        h('span.hint', { text: extra || '' }),
        h('div.spacer', { style: { flex: '1' } }),
        h(level === 'danger' ? 'span.pill.danger' : level === 'warn' ? 'span.pill.warn' : 'span.pill.ok',
          { text: pct(percent) }),
      ]),
      bar,
    ]);
  }

  function renderStatus(s, svc) {
    clear(statusBox);
    const problems = svc.filter(isProblem);
    const memPct = s.mem_total ? ((s.mem_total - s.mem_free) / s.mem_total) * 100 : 0;
    const diskPct = s.disk_total ? (s.disk_used / s.disk_total) * 100 : 0;
    statusBox.append(
      h('div.card-head', [
        h('h3', { text: '状态' }),
        h('div.spacer'),
        problems.length
          ? h('span.pill.danger', { text: `${problems.length} 个问题` })
          : h('span.pill.ok', { text: '一切正常' }),
      ]),
      h('div.card-body', [
        h('div.hint', {
          text: `${s.hostname || '本机'} · 已运行 ${duration(s.uptime)} · 面板 ${state.session?.version || ''}`,
        }),
        h('div', { style: { marginTop: '10px' } }, [
          metricRow('CPU', s.cpu_used || 0, `${s.cpu_cores || '?'} 核 · 负载 ${(s.load_1 || 0).toFixed(2)}`),
          metricRow('内存', memPct, `${bytes(s.mem_used || 0)} / ${bytes(s.mem_total || 0)}`),
          metricRow('磁盘', diskPct, `${bytes(s.disk_used || 0)} / ${bytes(s.disk_total || 0)}`),
        ]),
      ]),
    );
  }

  // isProblem 与「服务」页「需要处理」同一判据：启动失败 / 运行时不可用 /
  // 健康检查没过；用户主动停掉的不算。
  function isProblem(s) {
    const st = s.state || {};
    const health = s.health || {};
    if (s.stopped_by_user === true && !st.running) return false;
    return st.status === 'error' || st.status === 'unavailable' || (health.checked && !health.ok);
  }

  async function restart(name, display) {
    const yes = await confirmBox(`重启「${display}」？`, { title: '重启服务', okText: '重启' });
    if (!yes) return;
    try {
      await api.serviceAction(name, 'restart');
      toast(`已请求重启「${display}」`, 'ok');
    } catch (e) {
      toast('重启失败：' + (e && e.message ? e.message : e), 'err', 8000);
    }
    load(false);
  }

  function renderProblems(svc) {
    clear(problemBox);
    const problems = svc.filter(isProblem);
    problemBox.append(
      h('div.card-head', [h('h3', { text: '需要处理' })]),
    );
    if (!problems.length) {
      problemBox.append(h('div.card-body', [
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          h('span.pill.ok', { text: '没有异常服务' }),
          h('span.hint', { text: `共 ${svc.length} 个服务` }),
        ]),
      ]));
      return;
    }
    problemBox.append(h('div.card-body', problems.slice(0, 6).map((s) => {
      const st = s.state || {};
      const health = s.health || {};
      const why = st.detail || (health.checked && !health.ok ? health.message : '') || st.status || '';
      return h('div', {
        style: { display: 'flex', gap: '8px', alignItems: 'center', padding: '6px 0', flexWrap: 'wrap' },
      }, [
        h('span', { text: (s.icon || '') + ' ' + (s.display_name || s.name) }),
        h('span.pill.danger', { text: st.running ? '不健康' : (st.status || '异常') }),
        h('button.btn.btn-sm', {
          text: '重启', onclick: () => restart(s.name, s.display_name || s.name),
        }),
        why ? h('div.hint', { style: { width: '100%' }, text: String(why).split('\n')[0] }) : null,
      ]);
    })));
  }

  function renderActions() {
    clear(actionBox);
    const entry = state.session?.config?.panel_entry || '/';
    const open = (path) => window.open(entry.replace(/\/+$/, '/') + path, '_blank', 'noopener');
    actionBox.append(
      h('div.card-head', [h('h3', { text: '常用操作' })]),
      h('div.card-body', [
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
          h('button.btn.btn-sm.btn-primary', { text: '重启 Nginx', onclick: () => restart('nginx', 'Nginx') }),
          h('button.btn.btn-sm', { text: '导航页', onclick: () => open('nav/') }),
          h('button.btn.btn-sm', { text: '文件管理', onclick: () => { location.hash = '#/files'; } }),
          h('button.btn.btn-sm', { text: '计划任务', onclick: () => { location.hash = '#/cron'; } }),
          h('button.btn.btn-sm', { text: '面板设置', onclick: () => { location.hash = '#/settings'; } }),
        ]),
        h('div.hint', { style: { marginTop: '8px' }, text: '重启是真实操作，会短暂中断对应服务；失败会如实报错。' }),
      ]),
    );
  }

  async function load(showToast) {
    clear(statusBox);
    statusBox.append(h('div.card-body', [h('div.hint', { text: '读取中…' })]));
    let s;
    try {
      s = await api.systemInfo();
    } catch (e) {
      clear(statusBox);
      statusBox.append(h('div.card-body', [h('div.hint', { text: '读取系统状态失败：' + e.message })]));
      return;
    }
    let svc = [];
    let svcLoaded = false;
    try {
      const res = await api.services(false);
      svc = (res && res.list) || [];
      svcLoaded = true;
    } catch (e) {
      // 服务列表读不到就如实说，不把它当成"没有问题"。
      clear(problemBox);
      problemBox.append(
        h('div.card-head', [h('h3', { text: '需要处理' })]),
        h('div.card-body', [h('div.hint', { text: '读取服务列表失败：' + e.message })]),
      );
    }
    renderStatus(s, svc);
    if (svcLoaded) renderProblems(svc);
    renderActions();
    if (showToast) toast('已刷新', 'ok');
  }

  load(false);
  void ctx;
}
