// docker-networks.js —— Docker「网络」分区。
//
// 和卷相比，网络大多是"可重建的配置"：删错了重新 docker network create 就行，
// 所以这里只有删除/清理两个动作，不做更复杂的编辑。
// 但有两个坑必须处理：
//   1. bridge / host / none 是 Docker 内置网络，永远删不掉，按钮要直接禁用而不是
//      等用户点了再报错；
//   2. 有容器连接的网络删不掉（Docker 报 "active endpoints"），要把这句话翻译成人话。

import { api } from './api.js';
import { h, clear, toast, confirmBox } from './ui.js';

// Docker 内置网络，删不掉。按名字判断即可：内置网络的 Name 是固定的。
const BUILTIN = new Set(['bridge', 'host', 'none']);

// 后端返回的 Created 同样是 Go time.Time 字符串，可能是零值 0001-01-01。

// textOr 把"看起来是空"的值统一成占位符。
//
// 为什么不直接写 `v || '—'`：Docker 在某些内置网络上把 Driver 返回成**字符串 "null"**
// （不是 JSON null），例如 none 网络的 /networks 响应里就是 "Driver":"null"。
// 那样 `"null" || '—'` 得到的是字符串 "null"，表格里就真的渲染出一个 null ——
// UI 测试的"不允许出现字面量 null"断言正是这么抓出来的。
function textOr(v, placeholder = '—') {
  const s = v == null ? '' : String(v).trim();
  if (s === '' || s === 'null' || s === 'undefined') return placeholder;
  return s;
}

function fmtTime(s) {
  if (!s) return '—';
  const t = Date.parse(s);
  if (Number.isNaN(t)) return '—';
  const d = new Date(t);
  if (d.getFullYear() <= 1) return '—';
  return d.toLocaleString();
}

// IPAM.Config 在部分驱动下是 null，在自定义网络里也可能是空数组，
// 直接写 [0].Subnet 会在这些网络上报错，于是整个列表都渲染不出来。
function subnetOf(n) {
  const cfg = n && n.IPAM && Array.isArray(n.IPAM.Config) ? n.IPAM.Config : [];
  return (cfg[0] && cfg[0].Subnet) || '—';
}

// Containers 可能是 null / 缺失 / 空对象，统计连接数时统一兜底。
function connectedCount(n) {
  return Object.keys((n && n.Containers) || {}).length;
}

function isInUse(msg) {
  return /in use|active endpoints/i.test(String(msg || ''));
}

export async function renderNetworks(container, ctx) {
  ctx = ctx || {};
  const reload = () => { if (typeof ctx.refresh === 'function') ctx.refresh(); };

  clear(container);
  container.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取网络…' })]));

  let list = [];
  try {
    const r = await api.dockerNetworks();
    list = (r && r.list) || [];
  } catch (e) {
    // 与卷分区一致：错误既进 toast 也留在当前容器里，避免"点了没反应"。
    clear(container);
    toast(e.message, 'err', 9000);
    container.append(h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: '读取网络失败' }),
      h('p', { text: e.message }),
      h('div', { style: { marginTop: '14px' } }, [
        h('button.btn.btn-ghost.btn-sm', { text: '重试', onclick: reload }),
      ]),
    ]));
    return;
  }

  clear(container);
  container.append(buildToolbar(list, reload));
  container.append(list.length ? buildTable(list, ctx, reload) : buildEmpty());
}

function buildToolbar(list, reload) {
  return h('div', {
    style: {
      display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap',
      padding: '10px 14px', borderBottom: '1px solid var(--border-soft)',
    },
  }, [
    h('span.pill', { text: `共 ${list.length} 个网络` }),
    h('div.spacer'),
    h('button.btn.btn-danger.btn-sm', {
      text: '🧹 清理未使用网络',
      title: '清理没有容器连接、且非内置的网络',
      onclick: () => doPrune(reload),
    }),
  ]);
}

function buildTable(list, ctx, reload) {
  return h('table.table', [
    h('thead', [h('tr', [
      h('th', { text: '名称' }),
      h('th', { text: '驱动' }),
      h('th', { text: '范围' }),
      h('th', { text: '子网' }),
      h('th', { text: '连接容器' }),
      h('th', { text: '操作' }),
    ])]),
    h('tbody', list.map((n) => {
      const builtin = BUILTIN.has(n.Name);
      const connected = connectedCount(n);
      return h('tr', [
        h('td', [
          h('div', { style: { fontWeight: '550' }, text: n.Name || n.Id || '—' }),
          builtin
            ? h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: 'Docker 内置网络' })
            : null,
          n.Created ? h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: fmtTime(n.Created) }) : null,
        ]),
        h('td', { text: textOr(n.Driver) }),
        h('td', [h('span.pill' + (n.Scope === 'local' ? '' : '.brand'), { text: textOr(n.Scope) })]),
        h('td', [h('code', { text: subnetOf(n) })]),
        h('td', [
          h('span.pill' + (connected > 0 ? '.ok' : ''), {
            text: String(connected),
            title: connected > 0 ? '有容器连接时无法删除该网络' : '当前没有容器连接',
          }),
        ]),
        h('td', [
          // 内置网络不给可点的删除按钮：点了必然失败，禁用它比让用户撞一次报错更省事。
          builtin
            ? h('button.btn.btn-danger.btn-sm', {
              text: '删除',
              disabled: true,
              title: 'Docker 内置网络，不能删除',
              style: { opacity: '0.4', cursor: 'not-allowed' },
            })
            : h('button.btn.btn-danger.btn-sm', {
              text: '删除',
              onclick: () => doRemove(n, ctx, reload),
            }),
        ]),
      ]);
    })),
  ]);
}

function buildEmpty() {
  return h('div.empty', [
    h('div.big', { text: '🔌' }),
    h('h4', { text: '没有可显示的网络' }),
    h('p', { text: '正常情况下这里至少会有 bridge / host / none 三个内置网络；如果为空，可能是 Docker 引擎刚启动或权限不足。' }),
  ]);
}

async function doRemove(net, ctx, reload) {
  const name = net.Name || net.Id;
  if (!name) return;
  const connected = connectedCount(net);
  const extra = connected > 0
    ? `当前有 ${connected} 个容器连接在这个网络上，删除会失败；请先断开或删除这些容器。`
    : '这个网络当前没有容器连接。';
  const ok = await confirmBox(
    `删除网络「${name}」？\n\n${extra}\n` +
    '删除后需要重新创建才能再次使用同名网络，此操作不可撤销。',
    { title: '删除网络', danger: true, okText: '删除' }
  );
  if (!ok) return;

  try {
    // Id 是网络的唯一标识；Name 只作兜底（后端两者都接受）。
    await api.dockerNetworkRemove(net.Id || net.Name);
    toast(`已删除网络 ${name}`, 'ok');
    reload();
  } catch (e) {
    // Docker 的原话是 "network ... has active endpoints"，用户看不懂，
    // 这里统一翻译成"还有容器连在这个网络上"。
    if (isInUse(e.message)) {
      toast('还有容器连在这个网络上，请先断开或删除这些容器再删除网络', 'err', 9000);
      return;
    }
    toast(e.message, 'err', 9000);
  }
}

async function doPrune(reload) {
  const ok = await confirmBox(
    '清理所有未使用的网络？\n\n' +
    '只清理没有容器连接、且非内置的网络；bridge / host / none 会保留。此操作不可撤销。',
    { title: '清理未使用网络', danger: true, okText: '清理' }
  );
  if (!ok) return;

  try {
    const r = await api.dockerNetworkPrune();
    const n = (r && r.removed) || 0;
    toast(n > 0 ? `已清理 ${n} 个未使用网络` : '没有可清理的未使用网络', 'ok');
    reload();
  } catch (e) {
    toast(e.message, 'err', 9000);
  }
}
