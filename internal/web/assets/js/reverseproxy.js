// reverseproxy.js —— 独立的「反向代理」功能页。
//
// 为什么单独一页（而不是塞在「网站管理」里）：
//   网站管理的模型是"一个站点 = 域名 + 根目录 + PHP"，而这里要建的反代规则
//   里**大半根本没有站点目录** —— 只是"把这个端口/域名的请求转到另一台机器"。
//   硬塞进站点模型会出现"必须填一个并不存在的根目录"这种别扭。
//
// 界面上刻意把三件事说清楚，因为反代最容易在这三处出问题：
//   1. **监听端口**：规则写好了但 nginx 没在听（端口被占/nginx 没起）→ 如实标红；
//   2. **目标是否可达**：保存前就能点「测试连通」，不用先保存再猜；
//   3. **WebSocket**：默认开。关掉它，带界面的服务会"能打开但用不了"。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';

let cache = null;

export function ReverseProxyView(content, ctx = {}) {
  clear(content);

  const headBox = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
  const grid = h('div', { style: { display: 'grid', gap: '12px' } });
  const wrap = h('div.card', [
    h('div.card-head', [
      h('h3', { text: '反向代理' }),
      h('div.spacer'),
      headBox,
    ]),
    h('div.card-body.tight', [h('div.hint', {
      text: '一条规则 = 监听端口 + 域名（可留空）+ 路径前缀（可留空）→ 目标地址。' +
        '不需要站点目录；配置写进 nginx 的 vhosts 目录，停用即移除。',
    })]),
    h('div.card-body.tight', [grid]),
  ]);
  content.append(wrap);

  async function load() {
    clear(grid);
    appendAll(grid, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取规则…' })]));
    try {
      cache = await api.proxies();
    } catch (e) {
      clear(grid);
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
      ]));
      return;
    }
    renderHead();
    renderGrid();
  }

  function renderHead() {
    clear(headBox);
    const ng = cache?.nginx || {};
    appendAll(headBox,
      ng.installed
        ? h('span.pill.ok', { text: 'nginx 已安装' })
        : h('span.pill.danger', { text: '需要先安装 nginx' }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      h('button.btn.btn-sm.btn-primary', {
        text: '➕ 新建规则',
        disabled: !ng.installed,
        title: ng.installed ? '' : '反向代理由 nginx 执行，请先到「应用市场 → 网站环境」装 nginx',
        onclick: () => editRule(null),
      }));
  }

  function renderGrid() {
    clear(grid);
    const list = cache?.list || [];
    if (list.length === 0) {
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '🔀' }),
        h('p', { text: '还没有反向代理规则' }),
        h('p.hint', { text: '例如：监听 8090，目标 http://192.168.1.8:8090 —— 就能从这台机器访问 NAS 上的站点' }),
      ]));
      return;
    }
    appendAll(grid, ...list.map(ruleCard));
  }

  function ruleCard(it) {
    const domains = (it.domains || '').split(',').map((s) => s.trim()).filter(Boolean);
    const title = it.name || ('规则 #' + it.id);
    return h('div', {
      style: {
        border: '1px solid var(--border)', borderRadius: 'var(--radius)',
        padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: '8px',
        opacity: it.enabled ? 1 : 0.65,
      },
    }, [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('div', { style: { fontWeight: '620', fontSize: '13.5px' }, text: title }),
        it.enabled ? h('span.pill.ok', { text: '已启用' }) : h('span.pill', { text: '已停用' }),
        it.enabled && it.port_listening
          ? h('span.pill.ok', { text: '端口 ' + it.listen + ' 在听' })
          : (it.enabled ? h('span.pill.danger', {
            text: '端口 ' + it.listen + ' 没有监听',
            title: 'nginx 没在这个端口上监听：可能 nginx 没启动、或端口被别的进程占用。看「日志中心」或终端里 nginx -t',
          }) : null),
        it.target_ok
          ? h('span.pill.ok', { text: '目标可达', title: it.target_detail })
          : h('span.pill.warn', { text: '目标不可达', title: it.target_detail }),
        it.websocket ? h('span.pill', { text: 'WS' }) : null,
        h('div.spacer'),
      ]),
      h('div.mono', {
        style: { fontSize: '12.5px', color: 'var(--text-dim)', lineHeight: '1.7' },
      }, [
        h('div', { text: '监听 :' + it.listen + (domains.length ? '  ' + domains.join(', ') : '  (所有域名)') + (it.path ? '  ' + it.path + '*' : '') }),
        h('div', { text: '→  ' + it.target }),
      ]),
      it.remark ? h('div.hint', { text: it.remark }) : null,
      it.target_detail && !it.target_ok ? h('div.hint', { style: { color: 'var(--danger)' }, text: it.target_detail }) : null,
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm', { text: '✏️ 编辑', onclick: () => editRule(it) }),
        h('button.btn.btn-sm', {
          text: it.enabled ? '⏸ 停用' : '▶ 启用',
          onclick: () => toggle(it),
        }),
        h('button.btn.btn-sm', {
          text: '🔍 测试目标',
          onclick: async () => {
            try {
              const r = await api.proxyTest(it.target);
              toast(r.detail || (r.ok ? '可达' : '不可达'), r.ok ? 'ok' : 'err', 8000);
            } catch (e) { toast('测试失败：' + e.message, 'err', 8000); }
          },
        }),
        h('button.btn.btn-sm.btn-danger', { text: '删除', onclick: () => remove(it) }),
      ]),
    ]);
  }

  async function toggle(it) {
    try {
      await api.proxyToggle(it.id);
      toast(it.enabled ? '已停用（nginx 配置已移除）' : '已启用', 'ok');
      load();
    } catch (e) {
      toast('操作失败：' + e.message, 'err', 10000);
    }
  }

  async function remove(it) {
    const okGo = await confirmBox(
      '删除规则「' + (it.name || it.id) + '」？\n\n' +
      '· 会移除它的 nginx 配置并重载\n' +
      '· 目标机器上的服务**不受影响**',
      { title: '删除反向代理规则', okText: '删除' });
    if (!okGo) return;
    try {
      await api.proxyDelete(it.id);
      toast('已删除', 'ok');
      load();
    } catch (e) {
      toast('删除失败：' + e.message, 'err', 10000);
    }
  }

  // editRule 新建/编辑对话框。
  //
  // 校验顺序刻意做成"先测连通再保存"：反代最常见的失败就是目标写错，
  // 而保存成功、nginx 也 reload 成功、访问却 502 —— 那时候用户要自己去猜。
  function editRule(it) {
    const isNew = !it;
    const f = {
      name: h('input.input', { value: it?.name || '', placeholder: '例如：NAS 镜像站' }),
      listen: h('input.input', { type: 'number', value: it?.listen ?? 8090, min: '1', max: '65535' }),
      domains: h('input.input', { value: it?.domains || '', placeholder: '留空 = 该端口上所有域名；多个用逗号分隔' }),
      path: h('input.input', { value: it?.path || '', placeholder: '留空 = 所有路径；或填前缀，例如 /api' }),
      target: h('input.input', { value: it?.target || '', placeholder: 'http://192.168.1.8:8090' }),
      preserve: h('input', { type: 'checkbox', checked: !!it?.preserve_host }),
      ws: h('input', { type: 'checkbox', checked: it ? !!it.websocket : true }),
      enabled: h('input', { type: 'checkbox', checked: it ? !!it.enabled : true }),
      remark: h('input.input', { value: it?.remark || '', placeholder: '备注（可留空）' }),
    };
    const testOut = h('div.hint', { text: '' });
    const row = (label, node, hint) => h('div', { style: { marginBottom: '10px' } }, [
      h('div', { style: { fontSize: '12px', color: 'var(--text-dim)', marginBottom: '4px' }, text: label }),
      node,
      hint ? h('div.hint', { text: hint }) : null,
    ]);
    const body = h('div', [
      row('规则名称', f.name, '只用于你自己识别'),
      row('监听端口', f.listen, 'nginx 在这个端口上接收请求；80 需要 root，面板已具备'),
      row('域名（可选）', f.domains, '留空表示这个端口上任何域名都走这条规则'),
      row('路径前缀（可选）', f.path, '只代理某个前缀，例如 /api；留空代理全部'),
      row('目标地址', f.target, '例：http://192.168.1.8:8090、https://127.0.0.1:8443'),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px' } },
        [f.preserve, h('span', { text: '把原始 Host 透传给目标（默认关：多数后端按目标 Host 分站，透传会 404）' })]),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px' } },
        [f.ws, h('span', { text: '代理 WebSocket（带界面的服务建议开，关掉会"能打开但用不了"）' })]),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '10px' } },
        [f.enabled, h('span', { text: '启用' })]),
      row('备注', f.remark),
      testOut,
    ]);

    const m = modal({
      title: (isNew ? '新建' : '编辑') + '反向代理规则',
      wide: true,
      body,
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn', {
          text: '🔍 测试目标',
          onclick: async () => {
            const t = f.target.value.trim();
            if (!t) { toast('请先填目标地址', 'warn'); return; }
            testOut.textContent = '正在测试…';
            try {
              const r = await api.proxyTest(t);
              testOut.textContent = r.detail || '';
              testOut.style.color = r.ok ? 'var(--ok)' : 'var(--danger)';
            } catch (e) {
              testOut.textContent = '测试失败：' + e.message;
              testOut.style.color = 'var(--danger)';
            }
          },
        }),
        h('button.btn.btn-primary', {
          text: isNew ? '创建' : '保存',
          onclick: async () => {
            const payload = {
              name: f.name.value.trim(),
              listen: Number(f.listen.value) || 0,
              domains: f.domains.value.trim(),
              path: f.path.value.trim(),
              target: f.target.value.trim(),
              preserve_host: f.preserve.checked,
              websocket: f.ws.checked,
              enabled: f.enabled.checked,
              remark: f.remark.value.trim(),
            };
            try {
              if (isNew) await api.proxyCreate(payload);
              else await api.proxyUpdate(it.id, payload);
              toast(isNew ? '规则已创建' : '规则已保存', 'ok');
              close();
              load();
            } catch (e) {
              // 后端的校验/冲突信息是给用户看的，原样弹出来
              toast(e.message, 'err', 12000);
            }
          },
        }),
      ],
    });
    void m;
    setTimeout(() => f.name.focus(), 60);
  }

  registerCleanup(() => { cache = null; });
  load();
}
