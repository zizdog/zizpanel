// docker-services.js —— Docker 页的「已纳管服务」分区。
//
// 这一区存在的理由：Docker 页管的是"引擎里的东西"（容器/镜像/卷/网络），
// 而「服务管理」管的是"被登记为服务的生命周期"（开机自启、健康检查、统一日志）。
// 两者会在同一批对象上重叠 —— 一个 compose 项目既是一个 Docker 项目，
// 也是一条 compose 服务。
//
// 与其把服务列表在这里再抄一遍（两份状态迟早会不一致），这里只做**索引与跳转**：
// 显示已纳管的 docker/compose 服务、给出状态与常用动作、并链到服务管理页看完整信息。
// 这样"哪些容器是被面板纳管的"一眼可见，而详情只有一处权威。

import { api } from './api.js';
import { h, clear, toast, confirmBox, appendAll } from './ui.js';

export async function renderDockerServices(container, ctx) {
  const note = h('div.hint', {
    style: { marginBottom: '10px' },
    text: '这里只显示 kind 为 docker / compose 的已纳管服务。完整信息（健康检查、日志流、可纳管扫描）在「服务管理」页。',
  });
  const tableBox = h('div');
  container.append(note, tableBox);

  async function load() {
    clear(tableBox);
    tableBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取服务…' })]));

    let all = [];
    try {
      const res = await api.services(false);
      all = (res && res.list) || [];
    } catch (e) {
      clear(tableBox);
      tableBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取服务列表失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }

    const list = all.filter((s) => s.kind === 'docker' || s.kind === 'compose');

    clear(tableBox);
    if (list.length === 0) {
      tableBox.append(h('div.empty', [
        h('div.big', { text: '⚙️' }),
        h('h4', { text: '还没有纳管的 Docker 服务' }),
        h('p', { text: '在「Compose」分区保存并部署一个项目，它会自动登记到这里；也可以在「服务管理」里手工纳管已有容器。' }),
      ]));
      return;
    }

    const rows = list.map((s) => {
      const st = s.state || {};
      const running = !!st.running;
      const kindText = s.kind === 'compose' ? 'Compose 项目' : 'Docker 容器';

      const actions = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } });
      if (running) {
        actions.append(
          h('button.btn.btn-ghost.btn-sm', { text: '停止', onclick: () => act(s.name, 'stop') }),
          h('button.btn.btn-ghost.btn-sm', { text: '重启', onclick: () => act(s.name, 'restart') }),
        );
      } else {
        actions.append(h('button.btn.btn-primary.btn-sm', { text: '启动', onclick: () => act(s.name, 'start') }));
      }
      actions.append(
        h('button.btn.btn-ghost.btn-sm', {
          text: '查看日志',
          title: '跳到「服务管理」，那里有实时日志流',
          onclick: () => { location.hash = '#/services'; },
        }),
      );

      return h('tr', [
        h('td', [
          h('div', { style: { fontWeight: '600' }, text: s.display_name || s.name }),
          h('div.hint', { text: s.name }),
        ]),
        h('td', [h('span.pill', { text: kindText })]),
        h('td', [
          h(running ? 'span.pill.ok' : 'span.pill.warn', { text: st.status || 'unknown' }),
          h('div.hint', { text: st.detail || '' }),
        ]),
        h('td', [h('code', { text: s.port ? String(s.port) : '—' })]),
        h('td', [actions]),
      ]);
    });

    tableBox.append(h('table.table', [
      h('thead', [h('tr', [
        h('th', { text: '服务' }), h('th', { text: '类型' }), h('th', { text: '状态' }),
        h('th', { text: '端口' }), h('th', { text: '操作' }),
      ])]),
      h('tbody', rows),
    ]));

    appendAll(tableBox, h('div', { style: { marginTop: '12px' } }, [
      h('button.btn.btn-ghost.btn-sm', {
        text: '打开服务管理页', onclick: () => { location.hash = '#/services'; },
      }),
    ]));
  }

  async function act(name, action) {
    // 停止 compose 项目会停掉它下面所有容器 —— 这个后果要说清楚
    if (action === 'stop') {
      const okGo = await confirmBox(`停止服务 ${name}？\n\ncompose 项目会停止它下面的所有容器。`,
        { title: '停止服务', danger: true, okText: '停止' });
      if (!okGo) return;
    }
    try {
      await api.serviceAction(name, action);
      toast(`${name}：${action} 完成`, 'ok');
      await load();
      ctx.refresh && ctx.refresh();
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  await load();
}
