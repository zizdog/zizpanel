// docker-volumes.js —— Docker「数据卷」分区。
//
// 数据卷是 Docker 对象里唯一"删了就真没了"的东西：镜像可以重新拉，容器可以重建，
// 但卷里装的是用户数据。所以本分区所有删除路径都必须二次确认，并且把
// "只删没被使用的卷"（prune）和"强制删除正在使用的卷"（force remove）明确分开 ——
// 后者几乎一定意味着用户会丢数据，不能顺手替他决定。

import { api } from './api.js';
import { h, clear, toast, confirmBox, bytes } from './ui.js';

// 后端用的是 Go 的 time.Time：没有创建时间的卷会被序列化成零值
// "0001-01-01T00:00:00Z"。直接 new Date().toLocaleString() 会渲染出
// "公元 1 年"，看起来像乱码而不是"没有"，所以显式识别成占位符。
function fmtTime(s) {
  if (!s) return '—';
  const t = Date.parse(s);
  if (Number.isNaN(t)) return '—';
  const d = new Date(t);
  if (d.getFullYear() <= 1) return '—';
  return d.toLocaleString();
}

// 卷被容器占用时 Docker 的报错文案不完全固定（常见 "volume is in use"），
// 但都会带 "in use"，这里做宽松匹配，避免因为措辞差异错过"正在使用"这个关键分支。
function isInUse(msg) {
  return /in use/i.test(String(msg || ''));
}

export async function renderVolumes(container, ctx) {
  ctx = ctx || {};
  const reload = () => { if (typeof ctx.refresh === 'function') ctx.refresh(); };

  clear(container);
  // 先占位再请求：docker.js 会 await 这个函数，没有占位的话用户会盯着空白卡片。
  container.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取数据卷…' })]));

  let list = [];
  try {
    const r = await api.dockerVolumes();
    list = (r && r.list) || [];
  } catch (e) {
    // 这里不把异常抛回 docker.js：它的统一 catch 只显示一句"加载失败"，
    // 看不到原因也无法就地重试。错误必须同时进 toast（用户可能在别的分区操作过）
    // 和容器（当前视野里要有落点）。
    clear(container);
    toast(e.message, 'err', 9000);
    container.append(h('div.empty', [
      h('div.big', { text: '⚠️' }),
      h('h4', { text: '读取数据卷失败' }),
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
    h('span.pill', { text: `共 ${list.length} 个数据卷` }),
    h('div.spacer'),
    h('button.btn.btn-danger.btn-sm', {
      text: '🧹 清理未使用卷',
      title: '删除所有没有被任何容器使用的卷',
      onclick: () => doPrune(reload),
    }),
  ]);
}

function buildTable(list, ctx, reload) {
  return h('table.table', [
    h('thead', [h('tr', [
      h('th', { text: '名称' }),
      h('th', { text: '驱动' }),
      h('th', { text: '挂载点' }),
      h('th', { text: '创建时间' }),
      h('th', { text: '操作' }),
    ])]),
    h('tbody', list.map((v) => h('tr', [
      h('td', { style: { fontWeight: '550' }, text: v.Name || '—' }),
      h('td', { text: v.Driver || '—' }),
      h('td', [
        // 挂载点很长（/var/lib/docker/volumes/<名字>/_data），整段铺开会把表格撑爆，
        // 所以截断显示、完整值放进 title 供悬停查看。
        h('code', {
          text: v.Mountpoint || '—',
          title: v.Mountpoint || '',
          style: {
            display: 'inline-block', maxWidth: '300px', overflow: 'hidden',
            textOverflow: 'ellipsis', whiteSpace: 'nowrap', verticalAlign: 'bottom',
          },
        }),
      ]),
      h('td', { text: fmtTime(v.CreatedAt) }),
      h('td', [
        h('button.btn.btn-danger.btn-sm', {
          text: '删除',
          onclick: () => doRemove(v.Name, ctx, reload),
        }),
      ]),
    ]))),
  ]);
}

function buildEmpty() {
  return h('div.empty', [
    h('div.big', { text: '🗃️' }),
    h('h4', { text: '还没有数据卷' }),
    h('p', { text: '运行容器时用 -v 挂载具名卷，卷就会出现在这里。' }),
  ]);
}

async function doRemove(name, ctx, reload) {
  if (!name) return;
  const ok = await confirmBox(
    `删除数据卷「${name}」？\n\n` +
    '卷里的数据会永久丢失，且无法恢复。如果还有其他容器需要这份数据，请先备份。',
    { title: '删除数据卷', danger: true, okText: '删除' }
  );
  if (!ok) return;

  try {
    await api.dockerVolumeRemove(name, false);
    toast(`已删除数据卷 ${name}`, 'ok');
    reload();
  } catch (e) {
    if (!isInUse(e.message)) { toast(e.message, 'err', 9000); return; }

    // 被容器占用时普通删除永远不会成功，所以这里不能停在报错上：
    // 要么用户先去停/删容器，要么由用户明确选择强制删除（可能让占用它的容器之后起不来）。
    const force = await confirmBox(
      `数据卷「${name}」正被容器使用，无法直接删除。\n\n` +
      '请先停止并删除使用它的容器，然后重试；' +
      '如果确认要强制删除，卷里的数据会永久丢失，占用它的容器之后可能无法正常启动。',
      { title: '卷正在使用中', danger: true, okText: '强制删除' }
    );
    if (!force) return;

    try {
      await api.dockerVolumeRemove(name, true);
      toast(`已强制删除数据卷 ${name}`, 'ok');
      reload();
    } catch (e2) {
      toast(e2.message, 'err', 9000);
    }
  }
}

async function doPrune(reload) {
  const ok = await confirmBox(
    '清理所有未被使用的数据卷？\n\n' +
    '只删除没有任何容器在使用的卷；正在使用的卷不会被删。' +
    '被清理卷里的数据会永久丢失，且无法恢复。',
    { title: '清理未使用卷', danger: true, okText: '清理' }
  );
  if (!ok) return;

  try {
    const r = await api.dockerVolumePrune();
    const n = (r && r.reclaimed) || 0;
    // reclaimed 为 0 时说"释放了 0 B"很怪，直接换成"没有可清理的"更好懂。
    toast(n > 0 ? `清理完成，释放了 ${bytes(n)} 空间` : '没有可清理的未使用卷', 'ok');
    reload();
  } catch (e) {
    toast(e.message, 'err', 9000);
  }
}
