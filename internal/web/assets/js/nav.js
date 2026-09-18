// nav.js —— 「导航页」面板内页面：sun-panel 风格的图标网格首页。
//
// 为什么自研而不是部署现成的 sun-panel / home-dash（结论写在代码里备查）：
// 现成项目要么需要 Node/Vue 构建链（违背本项目「内嵌前端 + 无构建步骤」的路线），
// 要么自带一套运行时/数据库/鉴权/端口 —— 等于在面板之外再养一套要备份、要升级、
// 要单独记口令的系统。导航页的数据量极小（几个组、几十条链接），做进面板可以直接
// 复用面板已有的鉴权、风格、备份/恢复与审计，升级时只需替换一个二进制。
//
// 页面职责（数据在 /api/v1/nav/*，见 internal/web/api_nav.go）：
//   · 只读浏览：分组区段 + 图标卡片，点卡片新标签打开（target=_blank rel=noopener）；
//   · 顶部搜索：按名称/描述/URL 输入即筛（纯前端过滤，不发请求）；
//   · 编辑模式（登录后可开）：增删改分组与站点、按钮上下排序、导入/导出；
//   · 独立别名页 `/nav/`：给浏览器首页用，未登录也能看，见 assets/nav/。
//
// 交互刻意不做拖拽排序（按钮排序即可）、不做多用户/分享/天气/RSS —— 划界见任务说明。

import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { api } from './api.js';

// faviconOf 用目标站点自己的 /favicon.ico 当图标。
//
// 刻意**不接第三方 favicon CDN**（如 Google s2）：那会把"这台机器收藏了哪些站点"
// 泄露给第三方，而且离线环境下图标全裂。这里只是把图标字段填成目标站点的
// `/favicon.ico`，浏览器打开导航页时直接向该站点取 —— 不经过任何外部图标服务。
//
// TODO（如实标注）：没有做「面板服务端抓取 favicon 再缓存」的代理。
// 原因是那等于让服务端去请求任意用户填的 URL（SSRF：内网地址、重定向、
// 超大文件、私有存储），收益不抵风险。需要离线/稳定图标时，手工把图片地址
// 填进图标字段即可（外链图片同样只在浏览器侧加载）。
function faviconOf(rawURL) {
  try {
    const u = new URL(String(rawURL || '').trim());
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return '';
    return u.origin + '/favicon.ico';
  } catch {
    return '';
  }
}

// uploadNavIcon 让用户挑一张本地图片，上传成面板自己托管的图标。
//
// 用户 2026-09-18 要求："导航页要求可以上传本地图标！"
// 图标落在面板数据目录的 nav-icons/ 里（文件名是内容哈希），
// 读取走公开的 /nav/icons/<name>（导航页是匿名首页，图标必须匿名可读）。
// 上传成功后把地址直接填进图标输入框 —— 用户点「保存」才真正生效。
async function uploadNavIcon(iconInput, btn) {
  const picker = h('input', {
    type: 'file',
    accept: '.png,.jpg,.jpeg,.gif,.webp,.svg,.ico,image/png,image/jpeg,image/gif,image/webp,image/svg+xml,image/x-icon',
    style: { display: 'none' },
  });
  picker.onchange = async () => {
    const f = picker.files && picker.files[0];
    picker.value = '';
    if (!f) return;
    const oldText = btn.textContent;
    btn.disabled = true;
    btn.textContent = '上传中…';
    try {
      const info = await api.navIconUpload(f);
      iconInput.value = (info && info.url) || '';
      toast('图标已上传：' + f.name + '。点「保存」后这条才生效', 'ok', 6000);
    } catch (e) {
      toast('图标上传失败：' + e.message, 'err', 9000);
    } finally {
      btn.disabled = false;
      btn.textContent = oldText;
    }
  };
  // 追加到 body 再点：Safari 要求 input 在文档里才能触发选择框。
  document.body.appendChild(picker);
  picker.click();
  setTimeout(() => picker.remove(), 120000);
}

// pickUploadedIcon 打开「已上传的图标」选择器：**看着图挑**。
//
// 为什么必须有个看图的选择器（用户："也可以在已经上传的图标中选择！"）：
// 落盘文件名是内容哈希（防穿越、天然去重），人根本认不出来是哪张 ——
// 只给一个文件名列表等于让用户猜。
async function pickUploadedIcon(iconInput) {
  const grid = h('div.zp-icon-grid');
  const status = h('div.hint', { text: '正在读取已上传的图标…' });
  const m = modal({
    title: '从已上传的图标中选择',
    wide: true,
    body: h('div', [status, grid]),
    footer: (close) => [h('button.btn', { text: '关闭', onclick: close })],
  });
  let icons = [];
  try {
    const data = await api.navIcons();
    icons = (data && data.icons) || [];
  } catch (e) {
    status.textContent = '读取已上传图标失败：' + e.message;
    return m;
  }
  if (icons.length === 0) {
    status.textContent = '还没有上传过图标 —— 到「编辑站点 → 图标」那一行点「⬆ 上传本地图标」。';
    return m;
  }
  status.textContent = '共 ' + icons.length + ' 张：点一张就选中（🗑 删除该图，已引用的条目会显示默认图标）';
  for (const ic of icons) {
    const card = h('div.zp-icon-cell', [
      h('img', {
        src: ic.url,
        alt: '',
        title: ic.name + '（' + Math.round((ic.bytes || 0) / 1024) + ' KB）',
        loading: 'lazy',
        onclick: () => {
          iconInput.value = ic.url;
          m.close();
          toast('已选择图标，点「保存」后生效', 'ok');
        },
      }),
      h('button.btn.btn-sm.btn-icon.btn-danger.zp-icon-del', {
        text: '🗑',
        title: '删除这张图标（磁盘上真的删掉）',
        onclick: async (ev) => {
          ev.stopPropagation();
          const yes = await confirmBox(
            '这张图标会从磁盘上真的删掉。已经引用它的站点会显示默认图标，需要重新选一张。',
            { title: '删除图标？', danger: true, okText: '删除' });
          if (!yes) return;
          try {
            await api.navIconDelete(ic.name);
            card.remove();
            toast('已删除该图标', 'ok');
          } catch (e) {
            toast('删除失败：' + e.message, 'err');
          }
        },
      }),
    ]);
    grid.appendChild(card);
  }
  return m;
}

export function NavView(content, ctx = {}) {
  clear(content);

  let groups = [];
  let items = [];
  let edit = false;
  let query = '';
  let loaded = false;

  const toolbar = h('div.zp-nav-toolbar');
  const body = h('div', { id: 'zp-nav-body' });
  content.append(toolbar, body);

  const idOf = (x) => Number(x.id);
  const itemsOf = (gid) => items.filter((it) => Number(it.group_id) === Number(gid));

  // ---------------- 数据 ----------------

  async function load() {
    try {
      const tree = await api.navTree();
      groups = (tree && tree.groups) || [];
      items = (tree && tree.items) || [];
      loaded = true;
      renderBody();
    } catch (e) {
      loaded = true;
      clear(body);
      body.appendChild(h('div.card', h('div.card-body', h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取导航数据失败' }),
        h('p', { text: e.message || String(e) }),
        h('button.btn.btn-sm', { text: '重试', style: { marginTop: '10px' }, onclick: load }),
      ]))));
    }
  }

  // 每次写操作后整棵重拉：数据量极小（几十条），换来的是「界面 == 数据库」，
  // 不会出现本地乐观更新算错顺序、刷新后又变回去的问题。
  async function reload(msg) {
    await load();
    if (msg) toast(msg, 'ok');
  }

  // ---------------- 搜索过滤 ----------------

  // filtering 表示搜索框非空。此时列表是「过滤后的子集」，
  // 上下移动按钮会按错误的下标算顺序 —— 所以搜索时禁用排序（按钮 title 说明原因）。
  const filtering = () => query.trim() !== '';

  function visibleGroups() {
    const q = query.trim().toLowerCase();
    if (!q) return groups.map((g) => ({ g, list: itemsOf(idOf(g)) }));
    return groups
      .map((g) => {
        const nameHit = String(g.name).toLowerCase().includes(q);
        const list = itemsOf(idOf(g)).filter((it) => nameHit
          || String(it.name).toLowerCase().includes(q)
          || String(it.description || '').toLowerCase().includes(q)
          || String(it.url).toLowerCase().includes(q));
        return { g, list };
      })
      .filter((x) => x.list.length > 0);
  }

  // ---------------- 工具栏 ----------------

  function renderToolbar() {
    clear(toolbar);
    const search = h('input.input.zp-nav-search', {
      type: 'search',
      placeholder: '搜索名称 / 描述 / 网址…',
      value: query,
      dataset: { testid: 'nav-search' },
      oninput: (e) => { query = e.target.value; renderBody(); },
    });
    appendAll(toolbar,
      search,
      h('div.spacer'),
      h('a.btn.btn-sm', {
        href: api.navStandaloneURL(), target: '_blank', rel: 'noopener',
        title: '打开可在浏览器里当首页的独立导航页（未登录也能看）',
        text: '↗ 独立页',
      }),
      edit ? h('button.btn.btn-sm', {
        text: '＋ 站点', dataset: { testid: 'nav-add-item' },
        onclick: () => itemForm(null, groups.length ? idOf(groups[0]) : 0),
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '＋ 分组', dataset: { testid: 'nav-add-group' }, onclick: () => groupForm(null),
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '⬆ 导入', dataset: { testid: 'nav-import' }, onclick: importDialog,
      }) : null,
      edit ? h('button.btn.btn-sm', {
        text: '⬇ 导出', dataset: { testid: 'nav-export' }, onclick: exportFile,
      }) : null,
      h(`button.btn.btn-sm${edit ? '.btn-primary' : ''}`, {
        text: edit ? '✓ 完成' : '✎ 编辑',
        dataset: { testid: 'nav-edit-toggle' },
        title: edit ? '退出编辑模式' : '进入编辑模式（增删改分组与站点、排序、导入导出）',
        onclick: () => { edit = !edit; renderToolbar(); renderBody(); },
      }),
    );
  }

  // ---------------- 卡片 ----------------

  function iconNode(it) {
    const icon = String(it.icon || '').trim();
    // http(s) 直链，或**面板自己托管的本地图标**（/nav/icons/<内容哈希>.<扩展名>，
    // 就是用户在编辑弹窗里点「⬆ 上传本地图标」传上来的那张）。两者都用 <img> 渲染。
    if (/^https?:\/\//i.test(icon)
      || /^\/nav\/icons\/[0-9a-f]{16}\.(png|jpg|jpeg|gif|webp|svg|ico)$/i.test(icon)) {
      return h('img.zp-nav-icon', { src: icon, alt: '', loading: 'lazy' });
    }
    return h('span.zp-nav-icon.zp-nav-emoji', { text: icon || '🔗' });
  }

  // cardNode 返回「链接 + 编辑操作」两层结构。
  //
  // 为什么操作按钮不放进 <a> 里：嵌套可点击元素既是无效 HTML，也会让点删除时
  // 连带触发导航。卡片本体是真正的 <a>（target=_blank rel=noopener），
  // 编辑操作绝对定位在右上角、是它的兄弟节点。
  function cardNode(it, gid, idx) {
    const list = itemsOf(gid);
    const openNew = !(it.open_new_tab === false || it.open_new_tab === 0);
    const link = h('a.zp-nav-card', {
      href: it.url,
      target: openNew ? '_blank' : '_self',
      rel: 'noopener',
      title: it.url + (it.description ? '\n' + it.description : ''),
      dataset: { testid: 'nav-card', url: it.url, name: it.name },
    }, [
      h('div.zp-nav-card-top', [iconNode(it)]),
      h('div.zp-nav-card-main', [
        h('div.zp-nav-name', { text: it.name }),
        it.description ? h('div.zp-nav-desc', { text: it.description }) : null,
      ]),
    ]);
    if (!edit) return link;
    const ops = h('div.zp-nav-card-ops', [
      h('button.btn.btn-sm.btn-icon', {
        text: '✎', title: '编辑这个站点', dataset: { testid: 'nav-item-edit' },
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); itemForm(it); },
      }),
      h('button.btn.btn-sm.btn-icon', {
        text: '↑', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '上移',
        disabled: filtering() || idx === 0,
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); moveItem(gid, idx, -1); },
      }),
      h('button.btn.btn-sm.btn-icon', {
        text: '↓', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '下移',
        disabled: filtering() || idx === list.length - 1,
        onclick: (e) => { e.preventDefault(); e.stopPropagation(); moveItem(gid, idx, 1); },
      }),
      h('button.btn.btn-sm.btn-icon.btn-danger', {
        text: '🗑', title: '删除这个站点', dataset: { testid: 'nav-item-delete' },
        onclick: async (e) => {
          e.preventDefault(); e.stopPropagation();
          if (!await confirmBox(`确定删除站点「${it.name}」？（不可撤销）`,
            { title: '删除站点', danger: true, okText: '删除' })) return;
          try {
            await api.navItemDelete(idOf(it));
            await reload('站点已删除');
          } catch (err) { toast(err.message, 'err'); }
        },
      }),
    ]);
    return h('div.zp-nav-card-wrap', [link, ops]);
  }

  // ---------------- 主体 ----------------

  function renderBody() {
    clear(body);
    if (!loaded) {
      body.appendChild(h('div.card', h('div.card-body', [
        h('div.skeleton'), h('div.skeleton'), h('div.skeleton'),
      ])));
      return;
    }
    if (groups.length === 0) {
      body.appendChild(emptyState());
      return;
    }
    const shown = visibleGroups();
    if (shown.length === 0) {
      body.appendChild(h('div.empty', [
        h('div.big', { text: '🔍' }),
        h('h4', { text: '没有匹配的站点' }),
        h('p', { text: '换个关键词，或清空搜索框。' }),
      ]));
      return;
    }
    shown.forEach(({ g, list }, gi) => body.appendChild(groupSection(g, list, gi, shown.length)));
  }

  function emptyState() {
    return h('div.card', h('div.card-body', h('div.empty', [
      h('div.big', { text: '🧭' }),
      h('h4', { text: '还没有导航内容' }),
      h('p', { text: '先加一个组，再加几个常用站点，这里就会变成你的浏览器首页。' }),
      h('div', { style: { marginTop: '14px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
        h('button.btn.btn-primary.btn-sm', {
          text: '＋ 新建第一个分组', dataset: { testid: 'nav-empty-add-group' },
          onclick: () => groupForm(null),
        }),
        edit ? null : h('button.btn.btn-sm', {
          text: '✎ 进入编辑模式',
          onclick: () => { edit = true; renderToolbar(); renderBody(); },
        }),
      ]),
    ])));
  }

  function groupSection(g, list, gi, total) {
    const gid = idOf(g);
    const head = h('div.zp-nav-group-head', [
      h('h3', { text: g.name }),
      h('span.pill', { text: String(list.length) + ' 个站点' }),
      h('div.spacer'),
      edit ? h('div.zp-nav-group-ops', [
        h('button.btn.btn-sm', {
          text: '＋ 站点', title: '在这个分组里加站点', dataset: { testid: 'nav-group-add-item' },
          onclick: () => itemForm(null, gid),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '✎', title: '重命名分组', dataset: { testid: 'nav-group-edit' },
          onclick: () => groupForm(g),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '↑', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '分组上移',
          disabled: filtering() || gi === 0, onclick: () => moveGroup(gi, -1),
        }),
        h('button.btn.btn-sm.btn-icon', {
          text: '↓', title: filtering() ? '搜索状态下不能排序（先清空搜索框）' : '分组下移',
          disabled: filtering() || gi === total - 1, onclick: () => moveGroup(gi, 1),
        }),
        h('button.btn.btn-sm.btn-icon.btn-danger', {
          text: '🗑', title: '删除分组（连同其中的站点）', dataset: { testid: 'nav-group-delete' },
          onclick: async () => {
            if (!await confirmBox(`确定删除分组「${g.name}」？其中的 ${list.length} 个站点会一起删除，且不可撤销。`,
              { title: '删除分组', danger: true, okText: '删除' })) return;
            try {
              await api.navGroupDelete(gid);
              await reload('分组已删除');
            } catch (e) { toast(e.message, 'err'); }
          },
        }),
      ]) : null,
    ]);
    const grid = h('div.zp-nav-grid');
    if (list.length === 0) {
      grid.appendChild(h('div.zp-nav-empty-inline', {
        text: edit ? '这个分组还是空的，点上方「＋ 站点」加一个。' : '这个分组还没有站点。',
      }));
    } else {
      list.forEach((it, idx) => grid.appendChild(cardNode(it, gid, idx)));
    }
    return h('section.card.zp-nav-group', { dataset: { testid: 'nav-group', name: g.name } }, [
      h('div.card-head', [head]),
      h('div.card-body', [grid]),
    ]);
  }

  // ---------------- 排序（按钮版，不做拖拽） ----------------

  async function moveGroup(gi, delta) {
    const order = groups.map(idOf);
    const target = gi + delta;
    if (target < 0 || target >= order.length) return;
    [order[gi], order[target]] = [order[target], order[gi]];
    try {
      await api.navGroupReorder(order);
      await load();
    } catch (e) { toast(e.message, 'err'); }
  }

  async function moveItem(gid, idx, delta) {
    const order = itemsOf(gid).map(idOf);
    const target = idx + delta;
    if (target < 0 || target >= order.length) return;
    [order[idx], order[target]] = [order[target], order[idx]];
    try {
      await api.navItemReorder(order);
      await load();
    } catch (e) { toast(e.message, 'err'); }
  }

  // ---------------- 表单 ----------------

  function groupForm(g) {
    const isEdit = !!g;
    const name = h('input.input', {
      type: 'text', value: g ? g.name : '',
      placeholder: '例如：常用工具 / 影音 / 自建服务',
    });
    const okBtn = h('button.btn.btn-primary', { text: isEdit ? '保存' : '创建' });
    const m = modal({
      title: isEdit ? '重命名分组' : '新建分组',
      body: h('div.field', [h('label', { text: '分组名称' }), name]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      const v = name.value.trim();
      if (!v) { toast('分组名称不能为空', 'warn'); return; }
      okBtn.disabled = true;
      try {
        if (isEdit) await api.navGroupUpdate(idOf(g), { name: v });
        else await api.navGroupCreate({ name: v });
        m.close();
        await reload(isEdit ? '分组已重命名' : '分组已创建');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') okBtn.click(); });
  }

  function itemForm(it, presetGid) {
    const isEdit = !!it;
    if (groups.length === 0) { toast('请先创建一个分组', 'warn'); return; }
    const gid = it ? Number(it.group_id) : Number(presetGid || idOf(groups[0]));

    const name = h('input.input', { type: 'text', value: it ? it.name : '', placeholder: '例如：ZizPanel 面板' });
    const url = h('input.input', { type: 'text', value: it ? it.url : '', placeholder: 'https://example.com' });
    const icon = h('input.input', { type: 'text', value: it ? (it.icon || '') : '', placeholder: '一个 emoji（🧭）或图片地址，可留空' });
    const desc = h('input.input', { type: 'text', value: it ? (it.description || '') : '', placeholder: '一句话说明（可留空）' });
    const groupSel = h('select.select', groups.map((g) => h('option', {
      value: String(idOf(g)), text: g.name, selected: idOf(g) === gid,
    })));
    const openNew = h('input', {
      type: 'checkbox', checked: it ? !(it.open_new_tab === false || it.open_new_tab === 0) : true,
    });

    const favi = h('button.btn.btn-sm', {
      text: '用站点 favicon',
      title: '把图标填成该站点自己的 /favicon.ico（不经过任何第三方图标服务）',
      onclick: () => {
        const v = faviconOf(url.value);
        if (!v) { toast('请先填写 http(s) 站点地址', 'warn'); return; }
        icon.value = v;
      },
    });
    // 上传本地图标 / 从已上传的里面挑（用户 2026-09-18 要求）。
    const iconUp = h('button.btn.btn-sm', {
      text: '⬆ 上传本地图标',
      title: '选一张本地图片（png / jpg / gif / webp / svg / ico，≤512 KiB）上传到面板，当作这个站点的图标',
    });
    const iconPick = h('button.btn.btn-sm', {
      text: '🖼 已上传',
      title: '从你上传过的图标里挑一张（看图选择，可删除）',
    });
    iconUp.onclick = () => uploadNavIcon(icon, iconUp);
    iconPick.onclick = () => pickUploadedIcon(icon);

    const okBtn = h('button.btn.btn-primary', { text: isEdit ? '保存' : '创建' });
    const m = modal({
      title: isEdit ? '编辑站点' : '新建站点',
      wide: true,
      body: h('div', [
        h('div.field', [h('label', { text: '名称' }), name]),
        h('div.field', [h('label', { text: '网址（只支持 http / https）' }), url]),
        h('div.field', [
          h('label', { text: '图标' }),
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
            [icon, favi, iconUp, iconPick]),
          h('div.hint', {
            text: '填 emoji、图片直链，或上传一张本地图片（png / jpg / gif / webp / svg / ico，≤512 KiB）。'
              + '上传后可点「🖼 已上传」在已传过的里面挑。留空则显示默认图标。',
          }),
        ]),
        h('div.field', [h('label', { text: '描述' }), desc]),
        h('div.field', [h('label', { text: '所属分组' }), groupSel]),
        h('div.field', [
          h('label', { text: '打开方式' }),
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', fontSize: '13px' } }, [openNew, '在新标签页打开']),
        ]),
      ]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      const payload = {
        group_id: Number(groupSel.value),
        name: name.value.trim(),
        url: url.value.trim(),
        icon: icon.value.trim(),
        description: desc.value.trim(),
        open_new_tab: openNew.checked,
      };
      if (!payload.name) { toast('站点名称不能为空', 'warn'); return; }
      if (!payload.url) { toast('站点地址不能为空', 'warn'); return; }
      okBtn.disabled = true;
      try {
        if (isEdit) await api.navItemUpdate(idOf(it), payload);
        else await api.navItemCreate(payload);
        m.close();
        await reload(isEdit ? '站点已保存' : '站点已创建');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
  }

  // ---------------- 导入 / 导出 ----------------

  async function exportFile() {
    try {
      const res = await fetch(api.navExportURL(), { credentials: 'same-origin' });
      if (!res.ok) throw new Error(`导出失败（HTTP ${res.status}）`);
      const text = await res.text();
      const blob = new Blob([text], { type: 'application/json' });
      const a = h('a', { href: URL.createObjectURL(blob) });
      a.download = `zizpanel-nav-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(a.href), 4000);
      toast('已导出导航数据（可重新导入）', 'ok');
    } catch (e) {
      toast(e.message || String(e), 'err');
    }
  }

  function importDialog() {
    const ta = h('textarea.textarea', { placeholder: '把导出的 JSON 粘贴到这里，或选择文件…', rows: '10' });
    const file = h('input', { type: 'file', accept: '.json,application/json' });
    file.addEventListener('change', async () => {
      const f = file.files && file.files[0];
      if (!f) return;
      ta.value = await f.text();
    });
    const okBtn = h('button.btn.btn-danger', { text: '导入并替换' });
    const m = modal({
      title: '导入导航数据（整份替换）',
      wide: true,
      body: h('div', [
        h('div', {
          style: { fontSize: '13px', lineHeight: '1.7', color: 'var(--text-dim)', marginBottom: '10px' },
          text: '导入会用文件内容整份替换当前所有分组与站点。请先导出一份备份。',
        }),
        h('div.field', [h('label', { text: '选择 JSON 文件' }), file]),
        h('div.field', [h('label', { text: '或直接粘贴 JSON' }), ta]),
      ]),
      footer: (close) => [h('button.btn', { text: '取消', onclick: close }), okBtn],
    });
    okBtn.onclick = async () => {
      let doc;
      try {
        doc = JSON.parse(ta.value);
      } catch {
        toast('内容不是合法 JSON', 'err');
        return;
      }
      const gN = Array.isArray(doc.groups) ? doc.groups.length : 0;
      const iN = Array.isArray(doc.items) ? doc.items.length : 0;
      const yes = await confirmBox(
        `导入会整份替换当前所有导航分组与站点（本次将写入 ${gN} 个分组、${iN} 个站点），且不可撤销。确定继续吗？`,
        { title: '确认导入（整份替换）', danger: true, okText: '确定替换' },
      );
      if (!yes) return;
      okBtn.disabled = true;
      try {
        const res = await api.navImport(doc);
        m.close();
        await reload((res && res.msg) || '导入完成');
      } catch (e) {
        toast(e.message, 'err');
        okBtn.disabled = false;
      }
    };
  }

  renderToolbar();
  renderBody();
  load();
}
