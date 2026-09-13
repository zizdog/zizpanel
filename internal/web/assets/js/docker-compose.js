// docker-compose.js —— Docker 页的「Compose」分区。
//
// 这是开发文档 P3 里明确列的能力：贴一份 docker-compose.yml → 一键部署。
//
// 三个刻意的设计：
//
//  1. **项目名由用户显式填写，且是唯一的外部输入**。它是唯一会参与文件路径拼接
//     的东西，所以后端做了两道校验（字符白名单 + 路径前缀复核）。前端这里也
//     先拦一道，避免白跑一趟后端才报错。
//
//  2. **保存与部署分开**。只保存不会碰容器（可以先把 yml 存草稿），
//     部署才执行 up -d。把两者合成一个按钮会让人不敢按 —— "我只是想改个注释"。
//
//  3. **部署输出原样显示**。compose 的报错（端口占用、镜像拉不到、yml 语法错）
//     都在这段文本里，替用户"翻译"成一句话反而会丢信息。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll } from './ui.js';

// 与后端 validComposeName 保持一致：字母数字与 . _ -，不以点开头。
// 前端先拦一道只是为了让用户更早看到问题；权威校验在后端。
function validName(name) {
  return /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(name) && !name.includes('..');
}

// 新项目的模板：给一个能直接跑起来的最小例子，比空白编辑器友好得多。
const TEMPLATE = `# 面板会用 docker compose up -d 部署这份文件。
# 项目名（目录名）就是上面填的那个，所有文件都会落在 <工作目录>/compose/<项目名>/ 下。
services:
  web:
    image: nginx:alpine
    container_name: my-web
    restart: unless-stopped
    ports:
      - "8080:80"
`;

export async function renderCompose(container, ctx) {
  const toolbar = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '10px' } });
  const listBox = h('div');
  const rootHint = h('div.hint', { style: { marginBottom: '10px' } });
  container.append(toolbar, rootHint, listBox);

  let projects = [];

  async function load() {
    clear(listBox);
    listBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取 compose 项目…' })]));

    try {
      const res = await api.dockerComposeProjects();
      projects = (res && res.list) || [];
      if (res && res.root) {
        rootHint.textContent = `项目目录：${res.root}（每个项目一个子目录，內含 docker-compose.yml）`;
      }
    } catch (e) {
      clear(listBox);
      listBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取 compose 项目失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }

    clear(listBox);
    if (projects.length === 0) {
      listBox.append(h('div.empty', [
        h('div.big', { text: '🧩' }),
        h('h4', { text: '还没有 compose 项目' }),
        h('p', { text: '点右上角「新建项目」，贴一份 docker-compose.yml 即可部署。' }),
      ]));
      return;
    }

    const rows = projects.map((p) => {
      const actions = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } });
      if (p.exists) {
        actions.append(
          h('button.btn.btn-primary.btn-sm', { text: '部署', title: 'docker compose up -d', onclick: () => act(p.name, 'up') }),
          h('button.btn.btn-ghost.btn-sm', { text: '重启', onclick: () => act(p.name, 'restart') }),
          h('button.btn.btn-ghost.btn-sm', { text: '停止', onclick: () => stop(p.name) }),
        );
      }
      actions.append(
        h('button.btn.btn-ghost.btn-sm', { text: p.exists ? '编辑' : '创建文件', onclick: () => edit(p) }),
        h('button.btn.btn-ghost.btn-sm', {
          text: '日志',
          title: '跳到「服务管理」看这个项目的实时日志',
          onclick: () => { location.hash = '#/services'; },
        }),
        h('button.btn.btn-danger.btn-sm', { text: '删除', onclick: () => del(p) }),
      );

      return h('tr', [
        h('td', [
          h('div', { style: { fontWeight: '600' }, text: p.name }),
          h('div.hint', { text: p.file }),
        ]),
        h('td', [
          p.exists
            ? h('span.pill.ok', { text: '已就绪' })
            : h('span.pill.warn', { text: '缺 yml' }),
          p.registered ? h('span.pill.brand', { text: '已纳管', style: { marginLeft: '6px' } }) : null,
        ]),
        h('td', [h('span.hint', { text: p.updated_at || '—' })]),
        h('td', [actions]),
      ]);
    });

    listBox.append(h('table.table', [
      h('thead', [h('tr', [
        h('th', { text: '项目' }), h('th', { text: '状态' }), h('th', { text: '最后修改' }), h('th', { text: '操作' }),
      ])]),
      h('tbody', rows),
    ]));

    appendAll(listBox, h('div.hint', {
      style: { marginTop: '12px' },
      text: '「部署」= docker compose up -d --remove-orphans，并自动登记到「服务管理」；「停止」= docker compose down。',
    }));
  }

  async function edit(p) {
    const isNew = !p.exists;
    const nameBox = h('input.input', { value: p.name, placeholder: '项目名，例如 my-blog（只能用字母数字与 . _ -）' });
    // 项目名与目录绑定，改名等于换目录，老项目会留在原地 —— 所以已有项目不允许改名
    if (!isNew) nameBox.disabled = true;

    const editor = h('textarea.textarea', {
      style: { minHeight: '46vh', fontFamily: 'var(--mono)' },
      value: p.exists ? '正在读取…' : TEMPLATE,
    });
    const register = h('input', { type: 'checkbox', checked: true });

    const m = modal({
      title: isNew ? '新建 compose 项目' : `编辑 · ${p.name}`,
      wide: true,
      body: h('div', { style: { display: 'grid', gap: '12px' } }, [
        h('div.field', [
          h('label', { text: isNew ? '项目名 *' : '项目名（已创建的项目不能改名，改名等于换目录）' }),
          nameBox,
          h('div.hint', { text: `文件会写到 <工作目录>/compose/${isNew ? '<项目名>' : p.name}/docker-compose.yml` }),
        ]),
        h('div.field', [h('label', { text: 'docker-compose.yml' }), editor]),
        h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [
          register, h('span', { text: '保存后登记到「服务管理」（推荐，部署后可在那里启停与看实时日志）' }),
        ]),
      ]),
      footer: [
        h('button.btn.btn-ghost', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-ghost', { text: '仅保存', onclick: () => save(false) }),
        h('button.btn.btn-primary', { text: '保存并部署', onclick: () => save(true) }),
      ],
    });

    let loaded = isNew;
    if (p.exists) {
      try {
        const res = await api.dockerComposeRead(p.name);
        editor.value = (res && res.content) || '';
        loaded = true;
      } catch (e) {
        editor.value = '';
        toast('读取 yml 失败：' + e.message, 'err');
      }
    }
    if (isNew) nameBox.focus();

    async function save(deploy) {
      const name = String(nameBox.value || '').trim();
      if (!validName(name)) {
        toast('项目名只能用字母、数字与 . _ -，且不能以点开头', 'err');
        nameBox.focus();
        return;
      }
      if (!editor.value.trim()) {
        toast('compose 内容不能为空', 'err');
        return;
      }
      if (!loaded) {
        toast('内容还没读出来，稍等一下再保存', 'err');
        return;
      }
      try {
        const res = await api.dockerComposeSave(name, editor.value, register.checked);
        toast((res && res.message) || '已保存', 'ok');
      } catch (e) {
        toast(e.message, 'err');
        return;
      }
      m.close();
      await load();
      if (deploy) await act(name, 'up');
      ctx.refresh && ctx.refresh();
    }
  }

  async function act(name, action) {
    if (action === 'up') {
      toast(`正在部署 ${name}（拉镜像可能需要几分钟）…`, 'info');
    }
    try {
      const res = await api.dockerComposeAction(name, action);
      showOutput(name, action, (res && res.output) || '');
      toast(`${name}：${action} 成功`, 'ok');
      await load();
      ctx.refresh && ctx.refresh();
    } catch (e) {
      // 失败时后端也会把 compose 的输出带在错误里，展示出来比只报"失败"有用得多
      showOutput(name, action, e.message);
      toast(`${name}：${action} 失败`, 'err');
    }
  }

  /** showOutput 把 compose 的原始输出显示出来。 */
  function showOutput(name, action, output) {
    const m = modal({
      title: `compose ${action} · ${name}`,
      wide: true,
      body: h('pre', {
        style: {
          maxHeight: '60vh', overflow: 'auto', margin: '0', padding: '10px',
          background: 'var(--bg-soft, rgba(127,127,127,.08))', borderRadius: '6px',
          fontSize: '12px', lineHeight: '1.55', whiteSpace: 'pre-wrap', wordBreak: 'break-word',
        },
        text: output || '（没有输出）',
      }),
      footer: [h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() })],
    });
  }

  async function stop(name) {
    const yes = await confirmBox(
      `停止项目 ${name}？\n\n这会执行 docker compose down，停掉并删除该项目下的容器与网络。\n（具名数据卷默认保留。）`,
      { title: '停止并移除容器', danger: true, okText: '停止' });
    if (!yes) return;
    await act(name, 'down');
  }

  async function del(p) {
    const yes = await confirmBox(
      `删除项目 ${p.name}？\n\n会先执行 compose down，再删除目录 ${p.dir}。\n此操作不可撤销。`,
      { title: '删除 compose 项目', danger: true, okText: '删除目录' });
    if (!yes) return;
    try {
      await api.dockerComposeDelete(p.name, false);
      toast(`已删除项目 ${p.name}`, 'ok');
      await load();
      ctx.refresh && ctx.refresh();
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  async function createNew() {
    // 让用户先填项目名，再打开编辑器（名称决定了目录）
    const name = await promptBox({
      title: '新建 compose 项目',
      label: '项目名（只能用字母、数字与 . _ -）',
      placeholder: 'my-blog',
      hint: '会在 <工作目录>/compose/<项目名>/docker-compose.yml 创建文件',
    });
    if (!name) return;
    const n = String(name).trim();
    if (!validName(n)) {
      toast('项目名不合法', 'err');
      return;
    }
    await edit({ name: n, exists: false, dir: '', file: '' });
  }

  appendAll(toolbar,
    h('button.btn.btn-primary.btn-sm', { text: '＋ 新建项目', onclick: createNew }),
  );

  await load();
}
