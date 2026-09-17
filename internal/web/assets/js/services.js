// services.js —— 「应用」页「已安装」Tab + 服务侧的共享组件。
//
// 2026-09-17 信息架构（第二版，用户："服务管理/apps 合并"之后又要四个一级菜单）：
// 原来的「服务管理」整页（ServicesView）现在是「应用 → 已安装」Tab
// （renderInstalledApps，见下），导航里不再有单独的服务管理页。
// 这个文件现在负责：
//   - 已安装清单（市场已安装条目 + 服务记录，合并去重，**一张卡片一个应用**）
//   - 共享组件：日志弹窗 / 注册服务表单 / 凭据 / 配置文件编辑器 / 应用专属功能入口
//
// 交互设计要点：
//   - 每张卡片一眼看清"这个应用在不在跑、健康不健康"
//   - 状态灯的颜色直接反映真实状态（面板每次都实时查询系统）
//   - 纳管服务与托管服务在界面上有明确区分：前者不能卸载
//   - 卡片上只给一颗「打开」（判定见 servicePanel.openTargetOf）；「直链」在管理面板里
//   - 日志用 SSE 实时推送，不靠前端轮询

import { api, sseServiceLogs } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll, bytes, promptBox } from './ui.js';
// 依赖方向：apps.js → servicePanel.js ↔ services.js（两模块互取函数，但都只在
// 渲染/点击时才调用，不在模块初始化时求值，所以没有初始化顺序问题）。
// 卡片外壳、唯一那颗「打开」、「启停 / 重启 / 刷新」「状态措辞」全部来自
// servicePanel.js —— 与市场 / docker / 建站卡片、管理面板**同一份实现**。
// mergeAppEntries（去重）与 appCardShell（卡片 DOM）逻辑都只此一份。
import {
  openServicePanel, openOnlyAction, portAccessWarning, appCardShell,
  mergeAppEntries, statusLine, marketQuickActions,
} from './servicePanel.js';

// 「已安装」的状态筛选（全部 / 运行中 / 已停止 / 异常 / 仅健康检查失败）。
// 放模块级：Tab 切换、动作后重画都不该把用户的筛选选择丢掉。
let stateFilter = 'all';

// ============================================================================
//  「已安装」Tab —— 市场已安装条目 + 服务记录，合并去重后一张卡片一个应用
// ============================================================================
//
// 这一节原来是「服务管理」整页（ServicesView），合并那一轮变成页内 Tab（一行一条）。
// 2026-09-17 用户要求恢复卡片展示、并把 Tab 改名为「已安装」：
//   ① 把**市场里已安装的条目**与**面板服务记录**按归一化 key 合并去重
//      （真机上 php81/82/83/84 各显示两行，同一个服务两组按钮）；
//   ② 每张卡片给出状态（复用 servicePanel.statusLine 的**同一套措辞**）、端口、
//      健康检查结果，以及常用动作
//      （打开 / 停止(启动) / 重启 / ⟳ 刷新 / ⚙️ 管理）；
//   ③ 卡片上**只给「打开」**（不支持子路径时指向端口，并显示那句逐字提示）；
//      「直链」仍然在「⚙️ 管理」面板里 —— 这是上一轮用户明确要的，不能删。
//
// 数据由调用方（apps.js 的 AppsView）一次拉好递进来，本函数自己不发起请求：
//   opts.market    GET /api/v1/market 的**整个响应**（取 .list 里已安装/已纳管的）
//   opts.list      GET /api/v1/services?health=1 的 list（服务记录）
//   opts.onReload  需要整体重拉时的回调（注册服务后、纳管/取消纳管后等）
export function renderInstalledApps(container, opts = {}) {
  const marketList = () => (opts.market && opts.market.list) || [];
  const svcList = () => (Array.isArray(opts.list) ? opts.list : []);
  const reload = () => {
    if (typeof opts.onReload === 'function') opts.onReload();
    else renderAll();
  };

  // afterAction 是启停/重启/刷新动作完成后的回调。
  // 只拿到一条新记录时就地替换、重画这一屏（不整页重拉 —— 整页要重查所有服务，
  // 含 colima/compose 这类偏慢的，而用户此刻只关心这一个）；
  // 拿不到具体记录（纳管 / 取消纳管）才让调用方整体重拉。
  function afterAction(fresh) {
    if (fresh && fresh.name && Array.isArray(opts.list)) {
      const i = opts.list.findIndex((x) => x && x.name === fresh.name);
      if (i >= 0) opts.list[i] = fresh; else opts.list.push(fresh);
      renderAll();
      return;
    }
    reload();
  }

  const toolbar = h('div.card-head', [
    h('h3', { text: '已安装' }),
    h('div.spacer'),
    h('div#installed-toolbar', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);
  const listBox = h('div');
  appendAll(container, h('div.card', [toolbar, h('div.card-body', [listBox])]));
  const toolbarBox = toolbar.querySelector('#installed-toolbar');

  function renderAll() { renderToolbar(); renderRows(); }

  // entriesNow 每次都重新合并：动作更新的是服务记录数组，合并结果必须跟着变。
  function entriesNow() { return mergeAppEntries(marketList(), svcList()); }

  function matchesFilter(e) {
    if (stateFilter === 'all') return true;
    const st = (e.svc && e.svc.state) || {};
    const health = (e.svc && e.svc.health) || {};
    if (stateFilter === 'running') return !!st.running;
    if (stateFilter === 'stopped') {
      return !st.running && st.status !== 'error' && st.status !== 'unavailable';
    }
    if (stateFilter === 'problem') {
      return st.status === 'error' || st.status === 'unavailable' || (health.checked && !health.ok);
    }
    if (stateFilter === 'unhealthy') return !!(health.checked && !health.ok);
    return true;
  }

  function renderToolbar() {
    const rows = entriesNow();
    const svcs = svcList();
    const running = svcs.filter((s) => s.state && s.state.running).length;
    const unhealthy = svcs.filter((s) => s.health && s.health.checked && !s.health.ok).length;
    const filters = [
      { id: 'all', label: '全部' },
      { id: 'running', label: '运行中' },
      { id: 'stopped', label: '已停止' },
      { id: 'problem', label: '异常' },
      { id: 'unhealthy', label: '仅健康检查失败' },
    ];
    clear(toolbarBox);
    appendAll(toolbarBox,
      h('span.pill' + (running > 0 ? '.ok' : ''), {
        text: `${running}/${svcs.length} 服务运行中`,
        title: '面板服务记录里正在运行的条数（没有常驻进程的应用不计入）',
      }),
      h('span.pill', {
        text: `共 ${rows.length} 个应用`,
        title: '市场已安装条目 + 服务记录合并去重后的卡片数（一个应用一张卡片）',
      }),
      // 可点的：用户看到"有失败"的第一反应是"哪些？怎么办？"，
      // 所以点它直接筛出失败的服务（卡片里还有具体原因与下一步）。
      unhealthy > 0 ? h('button.btn.btn-sm.btn-danger', {
        text: `⚠ ${unhealthy} 个健康检查失败`,
        title: '点这里只看失败的服务',
        onclick: () => { stateFilter = 'unhealthy'; renderAll(); },
      }) : null,
      h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, filters.map((f) =>
        h(`button.btn.btn-sm${stateFilter === f.id ? '.btn-primary' : ''}`, {
          text: f.label,
          onclick: () => { stateFilter = f.id; renderAll(); },
        }))),
      h('button.btn.btn-sm', {
        text: '⟳ 刷新',
        title: '重新读取市场目录与服务状态',
        onclick: reload,
      }),
      h('button.btn.btn-sm', {
        text: '🔍 扫描可纳管服务',
        title: '找出本机上已经在运行的 launchd 服务，接入面板管理',
        onclick: () => openAdoptable(afterAction),
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 注册服务', onclick: () => newServiceModal(reload) }),
    );
  }

  function renderRows() {
    clear(listBox);
    const all = entriesNow();
    const rows = all.filter(matchesFilter);
    if (!rows.length) {
      appendAll(listBox, h('div.empty', [
        h('div.big', { text: '🧩' }),
        h('h4', { text: all.length ? '没有符合筛选条件的应用' : '还没有已安装的应用' }),
        h('p', { text: '到「应用市场」Tab 安装新应用，或扫描本机已在运行的服务接入管理。' }),
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center' } }, [
          h('button.btn', { text: '扫描可纳管服务', onclick: () => openAdoptable(afterAction) }),
        ]),
      ]));
      return;
    }
    // 卡片网格（与市场 / docker / 建站 Tab 同一套：.grid.grid-3 + appCardShell）。
    appendAll(listBox, h('div#installed-grid.grid.grid-3', rows.map(installedCard)));
  }

  // installedCard 渲染**一张卡片** —— 这张卡片同时握着市场条目（m）与服务记录
  // （s），所以「打开 / 安装器语义」与「状态 / 配置路径 / 日志 / plist」不会再
  // 分散在两个页面、两组按钮里。
  //
  // 卡片结构走 servicePanel.appCardShell（与市场 / docker / 建站 Tab 同一套 DOM，
  // 用户要求"恢复之前的卡片展示样式"）。
  function installedCard(e) {
    const m = e.market;
    const s = e.svc;
    const line = statusLine(s && s.state, m);
    const health = (s && s.health) || {};
    const name = (s && (s.display_name || s.name)) || (m && m.name) || e.key;
    const icon = (s && s.icon) || (m && m.icon) || '🧩';
    const port = (s && s.port) || (m && m.port) || 0;
    const subtitle = (m && m.summary) || (s && s.description) || '';
    // 卡片上的长描述只在与 summary 不同时才渲染（否则同一句话出现两遍）。
    const longText = (m && m.description) || '';
    const text = longText && longText !== subtitle ? longText : '';

    // 卡片上的动作组：**只有一颗「打开」**（用户 2026-09-17："只显示打开，
    // 不显示直链"）+ 停止(启动)/重启/⟳ 刷新（serviceActions，经 marketQuickActions
    // 统一给）+ ⚙️ 管理。
    // 「打开」的判定在 servicePanel.openTargetOf：支持子路径走子路径，不支持走
    // 端口直连；「直链」仍然住在「⚙️ 管理」面板里，卡片上不再出现。
    // 启停判据由 marketQuickActions 决定：有服务记录就一定给，否则看
    // adopted / service_in_launchd（跟市场卡片完全同一条规则）。
    const actions = [
      ...openOnlyAction(m, { svc: s }),
      ...marketQuickActions(m, {
        svc: s,
        onDone: afterAction,
        onManage: () => openServicePanel({ market: m, svc: s, onDone: () => afterAction() }),
      }),
    ];

    const pills = [
      h('span.pill' + (line.cls ? '.' + line.cls : ''), { text: line.text, title: line.title || '' }),
      port > 0 ? h('span.pill', { text: ':' + port }) : null,
      s ? h('span.pill' + (s.managed ? '.brand' : ''), {
        text: s.managed ? '面板托管' : '仅纳管',
        title: s.managed ? '面板负责完整生命周期，可卸载' : '由你自己安装，面板只做启停与查看，不会卸载',
      }) : null,
      health.checked
        ? (health.ok
          ? h('span.pill.ok', { text: '健康', title: (health.message || '') + '（' + (health.latency_ms || 0) + 'ms）' })
          : h('span.pill.danger', { text: '健康检查失败', title: health.message || '' }))
        : null,
      s && s.driver_error ? h('span.pill.warn', { text: '驱动不可用', title: s.driver_error }) : null,
      // 装了但面板里没有服务记录（孤儿态）时 statusLine 已经如实说「未纳管」，
      // 这里不再补第二颗同义 pill —— 卡片本来就要短，重复说两遍只会更乱。
      // 去重的证据：同一 launchd 服务在面板里有多条记录时，这里如实说出合并了几条，
      // 免得用户以后在数据库里看到两条记录却不知道为什么界面只有一张卡片。
      s && Array.isArray(s.merged_from) && s.merged_from.length
        ? h('span.pill', {
          text: '已合并 ' + s.merged_from.length + ' 条记录',
          title: '同一个服务在面板记录里有多条（launchd 标签相同）：' + s.merged_from.join('、'),
        })
        : null,
    ];

    // 健康检查失败：把"是什么、为什么、怎么办"摆出来（与旧服务卡片同一套话术）。
    // 「重新检查」不再单独给一颗按钮 —— 卡片上的「⟳ 刷新」就是重新查这一条。
    const extra = (health.checked && !health.ok)
      ? [h('div', { style: { fontSize: '11.5px', color: 'var(--danger)' } }, [
        h('div', { text: '检查地址：' + (health.url || '（未配置）') + ' —— ' + healthHint(health) }),
        h('button.btn.btn-sm', {
          style: { marginTop: '4px' },
          text: '改检查地址',
          title: '改完再点这张卡片上的「⟳ 刷新」重新检查',
          onclick: () => newServiceModal(reload, s),
        }),
      ])]
      : [];

    return appCardShell({
      icon,
      name,
      subtitle,
      pills,
      text,
      extra,
      actions,
      // 不支持子路径时，卡片上始终显示那句逐字提示（用户 2026-09-17 第六条）。
      warning: portAccessWarning(m, { svc: s }),
      dataset: { appKey: e.key, appName: name },
    });
  }

  // ---------- 扫描可纳管服务（原服务管理页的扫描弹窗，功能一字未减） ----------
  //
  // 2026-09-17：合并后它挂在「已安装」Tab 的工具条上（可纳管属于"本机已有的服务"）。
  // 纳管成功后 onDone(afterAction) 会重画这一屏。
  async function openAdoptable(onDone) {
    const box = h('div', [
      h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: '正在扫描本机的 launchd 服务…' })]),
    ]);
    modal({ title: '扫描可纳管服务', wide: true, body: box });

    let list = [];
    try {
      const r = await api.adoptable();
      list = r.list || [];
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
      return;
    }

    clear(box);
    if (!list.length) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '✅' }),
        h('h4', { text: '没有发现可纳管的新服务' }),
        h('p', { text: '本机上已运行的第三方 launchd 服务都已经在面板里了。' }),
      ]));
      return;
    }

    appendAll(box,
      h('div.hint', {
        style: { marginBottom: '12px' },
        text: '以下是本机正在使用的 launchd 服务。纳管后可以在面板里查看状态、启停、看日志；面板不会卸载它们。',
      }),
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '名称' }), h('th', { text: '状态' }), h('th', { text: '可执行文件' }), h('th', { text: '操作' }),
        ])]),
        h('tbody', list.map((c) => h('tr', [
          h('td', [
            h('div', { style: { fontWeight: '550' }, text: c.label }),
            h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: c.plist_path }),
          ]),
          // 状态措辞分三种：正在跑 / 已加载但按需启动（没有进程是正常的）/
          // 根本没加载。macOS 上很多作业是按需触发的（系统 cron 就是），
          // 一律写成"已停止"会让人以为服务坏了。
          h('td', [
            c.running
              ? h('span.pill.ok', { text: '运行中' })
              : (c.loaded
                ? h('span.pill', {
                  text: '待触发（按需运行）',
                  title: '已加载到 launchd，但没有常驻进程 —— 这类作业在需要时才被拉起，属正常状态',
                })
                : h('span.pill.warn', { text: '未加载' })),
          ]),
          h('td.mono', { style: { fontSize: '11px' }, text: c.program || '—' }),
          h('td', h('button.btn.btn-sm.btn-primary', {
            text: '纳管',
            onclick: async (ev) => {
              const btn = ev.target;
              btn.disabled = true;
              btn.textContent = '纳管中…';
              try {
                await api.adopt({ label: c.label, display_name: c.label, icon: '🧩' });
                toast(`已纳管 ${c.label}`, 'ok');
                ev.target.closest('tr').style.opacity = '0.4';
                btn.textContent = '已纳管';
                if (typeof onDone === 'function') onDone();
              } catch (e) {
                toast(e.message, 'err', 9000);
                btn.disabled = false;
                btn.textContent = '纳管';
              }
            },
          })),
        ]))),
      ]),
    );
  }

  renderAll();
}

// ============================================================================
//  健康检查失败 → "怎么办"（服务页、应用市场卡片、应用详情面板共用）
// ============================================================================

/**
 * healthHint 把"健康检查失败"翻译成"大概是什么问题、该怎么办"。
 *
 * 用户的原话是：看到"1 个健康检查失败"这个提示，不知道接下来该做什么。
 * 光给一个红标签等于把排查工作全丢给用户。这里按**失败的种类**给出最可能的
 * 原因与下一步动作。
 *
 * 注意 401/403 这条分支现在很少走到：后端已经把"需要身份验证"判为**健康**
 * （Stirling PDF 这类应用装完要设自己的账号密码，401 说明它活得好好的）。
 * 只有当用户又填了「期望包含内容」时，才会因为内容对不上而失败 ——
 * 所以这里的话术是"校验方式选错了"，而不是"服务有问题"。
 *
 * 2026-09-16 提到模块级并导出：应用详情面板（servicePanel.js）用同一套话术，
 * 各写一份的话用户在不同页面会看到两种解释，改了一处另一处就烂。
 */
export function healthHint(health) {
  const code = Number(health.code || 0);
  const msg = String(health.message || '');
  if (code === 401 || code === 403) {
    return '该地址要求身份验证，服务本身是正常的（所以默认按状态码判断时它算健康）。'
      + '但登录页里不会有你填的「期望包含内容」，建议把它清空，'
      + '或把「健康检查地址」换成不需要登录的路径（常见：/healthz、/api/health、/ping）。';
  }
  if (code === 404) {
    return '地址存在但路径不对（404）。确认健康检查地址写的是这个服务真实提供的路径。';
  }
  if (code >= 500) {
    return '服务自己返回了错误（5xx）。先看它的日志，通常是配置或依赖问题。';
  }
  if (/timeout|超时/i.test(msg)) {
    return '响应超时：服务可能很忙或卡住了。看日志确认它是否在正常处理请求；'
      + '如果是大应用，也可以把检查地址换成更轻量的路径。';
  }
  if (/refused|拒绝|无法连接|connect/i.test(msg)) {
    return '连不上：服务没在监听这个地址。确认它已启动、端口写对了，'
      + '以及它监听的是 127.0.0.1 还是 0.0.0.0（面板与服务的网络位置不同会影响）。';
  }
  if (/期望|包含|expect/i.test(msg)) {
    return '响应里没有「期望包含内容」。可能是被重定向到了登录页，或该路径返回的是别的页面；'
      + '建议把期望内容清空，只校验状态码。';
  }
  return '检查未通过。可以先看服务日志；若服务其实正常，多半是检查地址或期望内容选得不合适。';
}

// ============================================================================
//  实时日志（服务页卡片、应用详情面板共用）
//
//  2026-09-16 提到模块级并导出：应用详情面板里的「📜 日志」必须与这里**是同一个**
//  日志窗（SSE、自动滚动、断线重连的文案判定都只有这一份）。
// ============================================================================

export function openLogs(s) {
  const box = h('pre.logbox', { style: { maxHeight: '440px', minHeight: '200px' }, text: '正在连接日志流…\n' });
  let es = null;
  let paused = false;

  const follow = h('input', { type: 'checkbox', checked: true });
  const status = h('span.pill', { text: '连接中…' });

  const start = () => {
    if (es) es.close();
    status.className = 'pill';
    status.textContent = '连接中…';
    es = sseServiceLogs(s.name, {
      onData: (chunk) => {
        status.className = 'pill ok';
        status.textContent = '实时';
        if (paused) return;
        box.textContent += chunk;
        // 限制内存：超过 200KB 时只保留后一半
        if (box.textContent.length > 200 * 1024) {
          box.textContent = box.textContent.slice(-100 * 1024);
        }
        box.scrollTop = box.scrollHeight;
      },
      onError: async (_ev, src) => {
        // readyState === CLOSED 说明这次连接是**被服务端拒绝**的（非 200 或不是
        // event-stream），浏览器不会再重连。此时还说"正在重连…"，
        // 用户就在等一个永远不会发生的事 —— 这是误导。
        // 真机上最常见的触发点：这个服务没有任何可跟踪的日志文件（后端返回 400）。
        if (src && src.readyState === EventSource.CLOSED) {
          status.className = 'pill warn';
          status.textContent = '无法读取日志';
          // 去非流式接口问一句真正的原因，别把原因硬编码在文案里。
          let why = '';
          try {
            await api.serviceLogs(s.name, 1);
          } catch (e) {
            why = e.message;
          }
          box.textContent = '（日志流无法建立' + (why ? '：' + why : '') + '）\n' + box.textContent;
          return;
        }
        status.className = 'pill warn';
        status.textContent = '连接中断，正在重连…';
      },
      onClose: () => {
        status.className = 'pill';
        status.textContent = '日志流已结束';
      },
    });
  };

  const stop = () => { if (es) { es.close(); es = null; } };

  const m = modal({
    title: `日志：${s.display_name}`,
    wide: true,
    body: h('div', [
      h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', marginBottom: '10px', flexWrap: 'wrap' } }, [
        status,
        h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '12.5px' } }, [
          follow, h('span', { text: '自动滚动' }),
        ]),
        h('button.btn.btn-sm', {
          text: '清空显示',
          onclick: () => { box.textContent = ''; },
        }),
        h('button.btn.btn-sm', {
          text: '复制全部',
          onclick: async () => {
            try {
              await navigator.clipboard.writeText(box.textContent);
              toast('已复制到剪贴板', 'ok');
            } catch { toast('复制失败（浏览器限制）', 'warn'); }
          },
        }),
        h('span.sub', { text: s.log_path ? '日志文件：' + s.log_path : '' }),
      ]),
      box,
      h('div.hint', { style: { marginTop: '10px' }, text: '日志由服务端实时推送（SSE）。面板重启或服务重启后会自动重连。' }),
    ]),
    onClose: stop,
  });
  follow.addEventListener('change', () => { paused = !follow.checked; });
  start();
}

// ============================================================================
//  注册 / 编辑服务弹窗（供服务页与市场页复用）
// ============================================================================

export function newServiceModal(onDone, existing = null) {
  const isEdit = !!existing;
  const c = existing || {};

  const name = h('input.input', { value: c.name || '', placeholder: '英文标识，如 my-api', disabled: isEdit });
  const display = h('input.input', { value: c.display_name || '', placeholder: '展示名称，如 我的接口服务' });
  const kind = h('select.select', [
    h('option', { value: 'native', text: '原生服务（launchd 或启动命令）', selected: (c.kind || 'native') === 'native' }),
    h('option', { value: 'docker', text: 'Docker 容器', selected: c.kind === 'docker' }),
    h('option', { value: 'compose', text: 'Docker Compose 项目', selected: c.kind === 'compose' }),
  ]);
  const category = h('select.select', [
    h('option', { value: 'custom', text: '自定义', selected: (c.category || 'custom') === 'custom' }),
    h('option', { value: 'ai', text: 'AI 服务', selected: c.category === 'ai' }),
    h('option', { value: 'tool', text: '运维工具', selected: c.category === 'tool' }),
    h('option', { value: 'lnmp', text: '网站环境', selected: c.category === 'lnmp' }),
  ]);
  const port = h('input.input', { type: 'number', value: c.port || '', placeholder: '服务监听的端口' });
  const launchLabel = h('input.input', { value: c.launch_label || '', placeholder: '如 com.example.myapi' });
  const plistPath = h('input.input', { value: c.plist_path || '', placeholder: '留空则自动在 LaunchDaemons / LaunchAgents 中查找' });
  const startCmd = h('input.input', { value: c.start_cmd || '', placeholder: '如 /usr/bin/python3 -m http.server 9000' });
  const workDir = h('input.input', { value: c.work_dir || '', placeholder: '可选，服务的工作目录' });
  const healthURL = h('input.input', { value: c.health_url || '', placeholder: '如 http://127.0.0.1:9000/health' });
  const healthExpect = h('input.input', { value: c.health_expect || '', placeholder: '可选，响应里必须包含的内容' });
  const logPath = h('input.input', { value: c.log_path || '', placeholder: '留空则从 plist 读取；命令服务默认写到面板目录' });
  const container = h('input.input', { value: c.container || '', placeholder: '容器名（默认与服务标识相同）' });
  const composeFile = h('input.input', { value: c.compose_file || '', placeholder: 'docker-compose.yml 的绝对路径' });

  // 按类型显示不同字段
  const nativeBox = h('div', [
    h('div.field', [h('label', { text: 'launchd 标签' }), launchLabel,
      h('div.hint', { text: '如果服务由 launchd 托管，填标签（如 com.example.api）；留空并填下面的启动命令则由面板作为子进程运行。' })]),
    h('div.field', [h('label', { text: 'plist 路径（可选）' }), plistPath]),
    h('div.field', [h('label', { text: '启动命令（无 launchd 时使用）' }), startCmd,
      h('div.hint', { text: '⚠️ 用启动命令的服务是面板的子进程：面板重启后它们会停止。需要长期稳定运行请改用 launchd。' })]),
    h('div.field', [h('label', { text: '工作目录' }), workDir]),
  ]);
  const dockerBox = h('div', [
    h('div.field', [h('label', { text: '容器名' }), container,
      h('div.hint', { text: '面板直接通过 Docker API 管理容器，不需要 docker CLI。' })]),
  ]);
  const composeBox = h('div', [
    h('div.field', [h('label', { text: 'compose 文件路径' }), composeFile,
      h('div.hint', { text: '指向一个已存在的 docker-compose.yml；面板用 docker compose 管理它。' })]),
  ]);

  const updateKind = () => {
    nativeBox.hidden = kind.value !== 'native';
    dockerBox.hidden = kind.value !== 'docker';
    composeBox.hidden = kind.value !== 'compose';
  };
  kind.addEventListener('change', updateKind);
  updateKind();

  const submit = async (close) => {
    const payload = {
      name: name.value.trim(),
      display_name: display.value.trim(),
      kind: kind.value,
      category: category.value,
      port: Number(port.value) || 0,
      launch_label: launchLabel.value.trim(),
      plist_path: plistPath.value.trim(),
      start_cmd: startCmd.value.trim(),
      work_dir: workDir.value.trim(),
      health_url: healthURL.value.trim(),
      health_expect: healthExpect.value.trim(),
      log_path: logPath.value.trim(),
      container: container.value.trim(),
      compose_file: composeFile.value.trim(),
    };
    if (!payload.name && !payload.display_name) {
      toast('请填写服务标识或展示名称', 'warn');
      return;
    }
    if (payload.kind === 'native' && !payload.launch_label && !payload.start_cmd) {
      toast('原生服务需要填 launchd 标签或启动命令', 'warn');
      return;
    }
    try {
      if (isEdit) await api.serviceUpdate(c.name, payload);
      else await api.serviceCreate(payload);
      toast(isEdit ? '已保存' : '服务已注册', 'ok');
      close();
      if (onDone) onDone();
    } catch (e) {
      toast(e.message, 'err', 10000);
    }
  };

  const m = modal({
    title: isEdit ? '编辑服务：' + c.display_name : '注册服务',
    wide: true,
    body: h('div', [
      h('div.row', [
        h('div.field', [h('label', { text: '服务标识 *' }), name,
          h('div.hint', { text: isEdit ? '标识创建后不可修改' : '小写英文/数字/连字符，用于内部引用' })]),
        h('div.field', [h('label', { text: '展示名称' }), display]),
      ]),
      h('div.row', [
        h('div.field', [h('label', { text: '类型' }), kind]),
        h('div.field', [h('label', { text: '分类' }), category]),
        h('div.field', [h('label', { text: '端口' }), port]),
      ]),
      nativeBox, dockerBox, composeBox,
      h('div.section-title', { style: { marginTop: '8px' }, text: '健康检查（推荐填写）' }),
      h('div.row', [
        h('div.field', [h('label', { text: '健康检查地址' }), healthURL]),
        h('div.field', [h('label', { text: '期望包含内容' }), healthExpect]),
      ]),
      h('div.field', [h('label', { text: '日志文件路径' }), logPath]),
    ]),
    footer: (close) => [
      h('button.btn', { text: '取消', onclick: close }),
      h('button.btn.btn-primary', { text: isEdit ? '保存' : '注册服务', onclick: () => submit(close) }),
    ],
  });
  setTimeout(() => (isEdit ? display : name).focus(), 60);
}


// ---------------------------------------------------------------------------
//  Qwen3 TTS 的模型状态（常驻 / 已下载 / 未下载）
//
//  2026-09-14 起网站侧只支持「自定义音色」（克隆），预置音色整体下线，
//  所以服务端只有一个模型：1.7B-Base-8bit。这个入口保留的价值是两件事：
//    · 看得见"模型到底在不在、驻没驻留"（服务挂掉时最需要看到它）
//    · 手动**加载**（冷启动后不用等网站第一个请求）与**释放内存**（腾地方）
//  它不再是"两个模型二选一"——清单里只有一个，界面按列表渲染，不做切换语义。
// ---------------------------------------------------------------------------

// qwenLaunchLabel 与服务端登记时用的 launchd 标签一致。
const qwenLaunchLabel = 'com.zizdog.qwen3tts';

// isQwenService 判断"这条服务是不是 Qwen3 TTS"。
// 判据是**目录/记录里的标签**，不是应用 ID —— 与 appWidgets 注册表同一套思路。
export function isQwenService(s) {
  return !!(s && (s.launch_label === qwenLaunchLabel || s.name === 'com-zizdog-qwen3tts'));
}

// openQwenModels 打开模型状态弹窗（面板里的「🧠 模型」按钮）。
// 参数既可以是服务记录，也可以是市场条目 —— 只有 display_name/name 会被用到，
// 所以应用详情面板可以直接把手头那份数据传进来。
export function openQwenModels(svc) {
  const label = (svc && (svc.display_name || svc.name)) || 'Qwen3 TTS';
  const body = h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } }, [
    h('div', {
      style: { fontSize: '12px', color: 'var(--text-mute)', lineHeight: '1.6' },
      text: '网站侧插件现在只支持「自定义音色」（克隆），预置音色已下线，' +
        '所以只需要 1.7B-Base 这一个模型（约 2.9GB）。常驻内存后第一次合成不用等冷加载；' +
        '内存吃紧时可以释放，下次请求会自动重新加载（约 25 秒，不影响正确性）。',
    }),
    h('div', { id: 'qwen-model-list', style: { display: 'flex', flexDirection: 'column', gap: '8px' } }, [
      h('div', { style: { fontSize: '12px', color: 'var(--text-mute)' }, text: '读取中…' }),
    ]),
  ]);

  const m = modal({ title: 'Qwen 模型（' + label + '）', body });

  async function load() {
    const box = document.getElementById('qwen-model-list');
    if (!box) return;
    clear(box);
    let data;
    try {
      data = await api.qwenModels();
    } catch (e) {
      appendAll(box, [h('div', { style: { fontSize: '12px', color: 'var(--danger)' }, text: '读取失败：' + e.message })]);
      return;
    }
    const list = (data && data.list) || [];
    const active = (data && data.active) || '';

    for (const mdl of list) {
      const isActive = mdl.name === active || (mdl.loaded && !active);
      const pills = [];
      pills.push(h('span.pill' + (mdl.loaded ? '.ok' : ''), {
        text: mdl.loaded ? '已驻留内存' : (mdl.downloaded ? '已下载·未加载' : '未下载'),
        title: mdl.loaded ? '可立即推理' : (mdl.downloaded ? '首次使用或切换时会加载' : '需要先安装'),
      }));
      if (mdl.default) pills.push(h('span.pill.brand', { text: '插件默认', title: '插件里 openaiModel 默认填这个' }));

      appendAll(box, [
        h('div', {
          style: {
            border: '1px solid ' + (isActive ? 'var(--brand)' : 'var(--border)'),
            borderRadius: 'var(--radius)', padding: '11px 12px',
            display: 'flex', flexDirection: 'column', gap: '7px',
          },
        }, [
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            h('span', { style: { fontWeight: '620', fontSize: '13px' }, text: mdl.label }),
            ...pills,
          ]),
          h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.5' }, text: mdl.note || '' }),
          h('div', { style: { fontSize: '11px', color: 'var(--text-mute)', fontFamily: 'var(--mono, monospace)', wordBreak: 'break-all' }, text: mdl.name }),
          h('div', { style: { display: 'flex', gap: '6px', marginTop: '2px' } }, [
            mdl.loaded
              ? h('button.btn.btn-sm', { text: '释放内存', title: '下次用到会自动重新加载', onclick: () => doUnload(mdl) })
              : h('button.btn.btn-sm' + (mdl.downloaded ? '.btn-ok' : ''), {
                  text: mdl.downloaded ? '加载到内存' : '未下载，需重新安装 Qwen 服务',
                  disabled: !mdl.downloaded,
                  onclick: (ev) => doSwitch(mdl, ev.target),
                }),
          ]),
        ]),
      ]);
    }
    if (!list.length) {
      appendAll(box, [h('div', { style: { fontSize: '12px', color: 'var(--text-mute)' }, text: '没有可用模型。' })]);
    }
  }

  async function doSwitch(mdl, btn) {
    // 首次加载要读约 2.9GB 权重，实测约 25 秒 —— 必须告知用户，否则会以为卡死。
    const okGo = await confirmBox(
      '把「' + mdl.label + '」加载到内存？\n\n首次加载需读取约 2.9GB 权重，实测约 25 秒；' +
      '完成前该服务无法推理。加载后它会一直常驻，直到你显式释放。',
      { title: '加载模型', okText: '开始加载' }
    );
    if (!okGo) return;
    const old = btn.textContent;
    btn.disabled = true;
    btn.textContent = '加载中，请稍候…';
    try {
      await api.qwenSwitchModel(mdl.name);
      toast('已加载 ' + mdl.label, 'ok');
      await load();
    } catch (e) {
      toast('加载失败：' + e.message, 'danger');
      btn.disabled = false;
      btn.textContent = old;
    }
  }

  load();
}

// ---------- 服务详情 ----------
// ---------------------------------------------------------------------
//  音色接收端：调用密钥（receiver v1.6.0）
//
//  改版原因（用户要求）：
//    · 入口应该是**添加密钥**，而不是"更改共享密钥"——
//      一把共享密钥意味着任何一处泄露都要全员换，而且分不清是谁在用；
//    · 每把密钥可以单独设额度（单位：字），一个站点用超了不影响其它站点；
//    · 要能看到用量（今天/近 7 天/累计），否则"额度"只是个数。
//
//  密钥与额度存在接收端的 keys.json 里，**按 mtime 热加载** ——
//  加/停/删/改额度都不需要重启，而它可能正在替用户合成。
// ---------------------------------------------------------------------
export const RECEIVER_LABEL = 'com.zizdog.voicereceiver';

function randomReceiverToken() {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return 'ttsv-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

// 与后端 ValidateReceiverToken 同一套规则
function validReceiverToken(t) {
  return /^[A-Za-z0-9._-]{8,128}$/.test(t);
}

// 字符数千分位：额度动辄几十万字，不分组很难一眼读出来
function fmtChars(n) {
  return (Number(n) || 0).toLocaleString('zh-CN');
}

function maskKey(k) {
  const v = String(k || '');
  if (v.length <= 10) return v;
  return v.slice(0, 6) + '…' + v.slice(-4);
}

// 14 天字数趋势：用最朴素的 div 条形（Sparkline 是百分比基准，画字数会误导）。
//
// 每天一个**固定宽度**的柱子 + 日期标签。第一版用了 flex:1，结果只有一天数据时
// 那根柱子铺满整条 64px 高的横带，看起来像一条蓝色横幅，根本读不出"这是一天"。
function dayBars(days) {
  const list = Object.entries(days || {}).sort().slice(-14);
  if (!list.length) return h('div.hint', { text: '还没有按天的用量数据。' });
  const peak = Math.max(1, ...list.map(([, v]) => Number(v) || 0));
  return h('div', [
    h('div', { style: { display: 'flex', alignItems: 'flex-end', gap: '6px', height: '64px' } },
      list.map(([d, v]) => h('div', {
        title: `${d}：${fmtChars(v)} 字`,
        style: {
          width: '26px', flex: '0 0 auto', borderRadius: '3px 3px 0 0',
          background: 'var(--brand)', opacity: '0.75',
          height: Math.max(3, Math.round((Number(v) || 0) / peak * 56)) + 'px',
        },
      }))),
    h('div', { style: { display: 'flex', gap: '6px', marginTop: '4px' } },
      list.map(([d]) => h('div', {
        style: { width: '26px', flex: '0 0 auto', fontSize: '10px', color: 'var(--text-mute)', textAlign: 'center' },
        text: d.slice(5),
      }))),
    h('div.hint', {
      style: { marginTop: '4px' },
      text: list.length === 1
        ? `目前只有 ${list[0][0]} 一天的数据（按天统计从 v1.6.0 开始记）。`
        : `近 ${list.length} 天每天的字数（按天统计从 v1.6.0 开始记，历史累计见上面「累计」）。`,
    }),
  ]);
}

// ============================================================================
//  应用专有界面：音色接收端（调用密钥 / 各来源）
//
//  2026-09-16 从 ServicesView 内部提到模块级并导出：应用详情面板
//  （servicePanel.js）里的「🔑 调用密钥」「🎙 各来源」必须与这里**是同一个弹窗**。
//  它们仍然住在 services.js —— 这是"某个应用自己的界面"的集中地，
//  面板只负责决定"哪条服务该给这几颗按钮"（见本文件底部的 appWidgets）。
// ============================================================================

// openVoiceKeysModal 打开"调用密钥与用量"。
//
// 第一个参数是服务对象，只用于接口报错时的提示语；接口本身是按接收端来的
// （不区分哪条服务记录），所以按钮可以直接调 openVoiceKeysModal()。
export async function openVoiceKeysModal(svc, parentModal) {
  const box = h('div', [
    h('div.empty', [h('div.big', { text: '🔑' }), h('p', { text: '正在读取密钥与用量…' })]),
  ]);
  void svc;
  const m = modal({ title: '调用密钥与用量（每个网站一把，可设额度）', wide: true, body: box });

  function showSecret(key) {
    modal({
      title: '这把密钥的完整值（只显示这一次）',
      body: h('div', [
        h('div', {
          style: {
            background: 'var(--warn-soft)', border: '1px solid var(--border)',
            borderRadius: '8px', padding: '10px 12px', marginBottom: '12px', fontSize: '12.5px', lineHeight: '1.7',
          },
          text: '把它填进该网站 TtsVoice 插件的 openaiKey（接收端地址与 refUploadToken 都由它推导）。'
            + '列表里之后只显示掩码，复制请趁现在。',
        }),
        h('input.input', { value: key, readOnly: true, spellcheck: 'false', onclick: (e) => e.target.select() }),
      ]),
      footer: (close) => [
        h('button.btn.btn-primary', {
          text: '复制并关闭',
          onclick: async () => {
            try {
              await navigator.clipboard.writeText(key);
              toast('已复制到剪贴板', 'ok');
            } catch { toast('复制失败（浏览器限制），请手动选中复制', 'warn'); }
            close();
          },
        }),
      ],
    });
  }

  async function addKey() {
    const nameInput = h('input.input', { placeholder: '例如：zizdog.cn 网站', spellcheck: 'false' });
    const quotaInput = h('input.input', { type: 'number', min: '0', value: '0', placeholder: '0 = 不限' });
    const valueInput = h('input.input', { placeholder: '留空自动生成', spellcheck: 'false' });
    const hint = h('div.hint', { text: '额度单位是「字」：按提交文本的字符数计（中文 1 字算 1，含标点）。0 = 不限量。' });
    const vhint = h('div.hint', { text: '留空则由面板生成一个随机密钥。也可以填你已有的密钥值。' });

    modal({
      title: '添加调用密钥',
      body: h('div', [
        h('div.field', [h('label', { text: '名称（给谁用）' }), nameInput]),
        h('div.field', [h('label', { text: '额度（字，0 = 不限）' }), quotaInput, hint]),
        h('div.field', [h('label', { text: '密钥值（可留空）' }), valueInput, vhint]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '添加',
          onclick: async () => {
            const value = valueInput.value.trim();
            if (value && !validReceiverToken(value)) {
              toast('密钥值不合法：只能字母数字 - _ .，长度 8–128', 'warn');
              return;
            }
            const quota = Number(quotaInput.value || 0);
            if (!Number.isFinite(quota) || quota < 0) { toast('额度不能是负数', 'warn'); return; }
            try {
              const r = await api.voiceKeyAdd({
                name: nameInput.value.trim(), value, quota_chars: Math.floor(quota),
              });
              close();
              toast('已添加密钥', 'ok');
              showSecret((r && r.key && r.key.key) || '');
              await load();
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }),
      ],
    });
  }

  function editKey(row) {
    const nameInput = h('input.input', { value: row.name || '', spellcheck: 'false' });
    const quotaInput = h('input.input', { type: 'number', min: '0', value: String(row.quota_chars || 0) });
    const resetValue = h('input', { type: 'checkbox' });
    modal({
      title: `编辑密钥：${row.name || row.id}`,
      body: h('div', [
        h('div.field', [h('label', { text: '名称' }), nameInput]),
        h('div.field', [
          h('label', { text: '额度（字，0 = 不限）' }), quotaInput,
          h('div.hint', { text: `已用 ${fmtChars(row.used_chars)} 字。把额度调到低于已用，会让该站点立刻调不动（返回 429）。` }),
        ]),
        h('div.field', [
          h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center' } }, [
            resetValue, h('span', { text: '同时重置密钥值（网站那边要同步改）' }),
          ]),
        ]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '保存',
          onclick: async () => {
            const quota = Number(quotaInput.value || 0);
            if (!Number.isFinite(quota) || quota < 0) { toast('额度不能是负数', 'warn'); return; }
            const payload = {
              name: nameInput.value.trim(),
              quota_chars: Math.floor(quota),
            };
            if (resetValue.checked) payload.value = '';
            try {
              const r = await api.voiceKeyUpdate(row.id, payload);
              close();
              toast('已保存', 'ok');
              if (resetValue.checked) {
                await load();
                if (r && r.key && r.key.key) showSecret(r.key.key);
              } else {
                await load();
              }
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }),
      ],
    });
  }

  async function load() {
    clear(box);
    let data = null;
    let err = null;
    try {
      data = await api.voiceKeys();
    } catch (e) { err = e; }

    if (err) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('p', { text: err.message }),
        h('div.hint', { text: '这个页面需要接收端 v1.6.0。请在「应用市场 → 音色样本接收端」重新部署一次。' }),
      ]));
      return;
    }

    const list = data.keys || [];
    const total = data.total || {};

    const head = h('div', { style: { display: 'flex', gap: '18px', flexWrap: 'wrap', marginBottom: '4px' } }, [
      h('div', [h('div.sub', { text: '今日' }), h('div', { style: { fontSize: '18px', fontWeight: '600' }, text: fmtChars(total.today_chars) + ' 字' })]),
      h('div', [h('div.sub', { text: '近 7 天' }), h('div', { style: { fontSize: '18px', fontWeight: '600' }, text: fmtChars(total.week_chars) + ' 字' })]),
      h('div', [h('div.sub', { text: '累计' }), h('div', { style: { fontSize: '18px', fontWeight: '600' }, text: fmtChars(total.chars) + ' 字' })]),
      h('div', [h('div.sub', { text: '调用次数' }), h('div', { style: { fontSize: '18px', fontWeight: '600' }, text: fmtChars(total.requests) })]),
      h('div', [h('div.sub', { text: '产出音频' }), h('div', { style: { fontSize: '18px', fontWeight: '600' }, text: bytes(total.audio_bytes) })]),
    ]);

    const rows = list.map((row) => {
      const quotaText = row.deleted ? '—'
        : (row.unlimited ? '不限' : fmtChars(row.quota_chars) + ' 字');
      const remaining = row.deleted ? '—'
        : (row.unlimited ? '—' : fmtChars(row.remaining_chars) + ' 字');
      // 额度进度条：一眼看出哪把快用完了
      const pct = (!row.unlimited && row.quota_chars > 0)
        ? Math.min(100, Math.round(row.used_chars / row.quota_chars * 100)) : 0;
      const barColor = pct >= 100 ? 'var(--danger)' : (pct >= 80 ? 'var(--warn)' : 'var(--brand)');

      const actions = [];
      if (row.legacy) {
        actions.push(h('button.btn.btn-sm', {
          text: '迁移',
          title: '重新部署接收端后，这把来自服务配置的密钥会变成可管理的密钥',
          onclick: () => { if (parentModal) parentModal.close(); m.close(); toast('请在应用市场重新部署「音色样本接收端」以完成迁移', 'info', 8000); },
        }));
      } else if (!row.deleted) {
        actions.push(h('button.btn.btn-sm', { text: '编辑', onclick: () => editKey(row) }));
        actions.push(h('button.btn.btn-sm', {
          text: row.enabled ? '停用' : '启用',
          title: row.enabled ? '停用后该密钥立刻失效（网站会 403）' : '重新启用这把密钥',
          onclick: async () => {
            try {
              await api.voiceKeyUpdate(row.id, { enabled: !row.enabled });
              toast(row.enabled ? '已停用' : '已启用', 'ok');
              await load();
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }));
      }

      // 清零：所有行都能清（含"服务配置里的兼容密钥"和已删除密钥留下的历史）——
      // 它只动统计，不动密钥、额度与样本。
      if (row.used_chars > 0 || row.requests > 0) {
        actions.push(h('button.btn.btn-sm', {
          text: '清零',
          title: '把这把密钥的已用字数清零（从 0 重新计），密钥与额度都不变',
          onclick: async () => {
            if (!await confirmBox(
              `把「${row.name || row.id}」的用量清零？\n\n`
              + `当前已用 ${fmtChars(row.used_chars)} 字、${fmtChars(row.requests)} 次调用，`
              + '清零后从 0 重新计（额度不变）。\n'
              + '按天曲线也会一并清空；**正在跑的作业**完成后仍会按实际完成的字数记进来。',
              { title: '清零用量', okText: '确认清零' })) return;
            try {
              await api.voiceUsageReset(row.id);
              toast('已清零', 'ok');
              await load();
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }));
      }

      if (!row.legacy && !row.deleted) {
        actions.push(h('button.btn.btn-sm.btn-danger', {
          text: '删除',
          onclick: async () => {
            if (!await confirmBox(
              `删除密钥「${row.name || row.id}」？\n\n`
              + '该密钥会**立刻失效**（接收端热加载，不用重启），用它调用的网站会开始 403。\n'
              + '历史用量会保留在统计里。',
              { title: '删除密钥', danger: true, okText: '确认删除' })) return;
            try {
              await api.voiceKeyDelete(row.id);
              toast('已删除', 'ok');
              await load();
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }));
      }

      return h('tr', { style: row.deleted ? { opacity: '0.6' } : {} }, [
        h('td', [
          h('strong', { text: row.name || row.id }),
          row.deleted ? h('span.sub', { text: '  （已删除）' }) : null,
          row.legacy ? h('span.sub', { text: '  （来自服务配置，未迁移）' }) : null,
          !row.enabled && !row.deleted ? h('span.sub', { text: '  （已停用）' }) : null,
        ]),
        h('td', [
          h('code', { text: maskKey(row.key) }),
          row.key ? h('button.btn.btn-sm', {
            text: '显示',
            title: '查看完整密钥（用于填进网站插件）',
            onclick: () => showSecret(row.key),
          }) : null,
          row.key ? h('button.btn.btn-sm', {
            text: '复制',
            onclick: async () => {
              try { await navigator.clipboard.writeText(row.key); toast('已复制', 'ok'); }
              catch { toast('复制失败（浏览器限制）', 'warn'); }
            },
          }) : null,
        ]),
        h('td', { text: quotaText }),
        h('td', { text: fmtChars(row.used_chars) + ' 字' }),
        h('td', [
          h('div', { text: remaining }),
          (row.reserved_chars > 0)
            ? h('div.sub', { title: '已提交、还在合成的作业占用的字数', text: `（占用中 ${fmtChars(row.reserved_chars)} 字）` })
            : null,
          (!row.unlimited && !row.deleted)
            ? h('div', {
                style: {
                  height: '4px', borderRadius: '2px', marginTop: '3px',
                  background: 'var(--border)', overflow: 'hidden', minWidth: '70px',
                },
              }, [h('div', { style: { width: pct + '%', height: '100%', background: barColor } })])
            : null,
        ]),
        h('td', { text: fmtChars(row.today_chars) + ' 字' }),
        h('td', { text: row.last_used ? new Date(row.last_used * 1000).toLocaleString('zh-CN') : '未使用' }),
        h('td', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, actions),
      ]);
    });

    appendAll(box,
      head,
      h('div', { style: { margin: '10px 0 14px' } }, [dayBars(total.days)]),
      list.length
        ? h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '名称' }), h('th', { text: '密钥' }), h('th', { text: '额度' }),
              h('th', { text: '已用' }), h('th', { text: '剩余' }), h('th', { text: '今日' }),
              h('th', { text: '最后使用' }), h('th', { text: '操作' }),
            ])]),
            h('tbody', rows),
          ])
        : h('div.empty', [
            h('div.big', { text: '🔑' }),
            h('h4', { text: '还没有任何调用密钥' }),
            h('p', { text: '添加一把给网站用；每个网站建议各用一把，并设置额度。' }),
          ]),
      h('div.hint', {
        style: { marginTop: '12px' },
        text: '额度按**实际合成完成**的字数计（中文 1 字算 1，含标点）：提交时先占用额度'
          + '（防止同一把密钥并发提交把总额度撑爆），作业完成/失败/取消时按已完成的块结算，'
          + '没跑到的部分自动退回 —— 生成失败或中途取消不会按整篇收费。'
          + '额度用完时接收端返回 429（quota exceeded），与该密钥无效的 403 区分开。'
          + '需要重新开始计时，用行内的「清零」（密钥与额度不变）。'
          + '密钥、额度、清零都立即生效，不用重启接收端。',
      }),
      h('div', { style: { marginTop: '14px', display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm.btn-primary', { text: '➕ 添加密钥', onclick: addKey }),
        h('button.btn.btn-sm', {
          text: '🧹 清零全部用量',
          title: '把所有密钥的已用字数/调用次数一起清零（密钥与额度不变）',
          onclick: async () => {
            if (!await confirmBox(
              '把所有密钥的用量统计清零？\n\n'
              + `当前累计 ${fmtChars(total.chars)} 字、${fmtChars(total.requests)} 次调用。\n`
              + '密钥、额度、音色样本都不受影响；按天曲线会一并清空。',
              { title: '清零全部用量', okText: '确认清零' })) return;
            try {
              await api.voiceUsageReset('');
              toast('已清零全部用量', 'ok');
              await load();
            } catch (e) { toast(e.message, 'err', 8000); }
          },
        }),
        h('button.btn.btn-sm', { text: '🔄 刷新', onclick: load }),
      ]),
      data.usage_error
        ? h('div.hint', { style: { color: 'var(--danger)', marginTop: '8px' }, text: '用量读取失败：' + data.usage_error })
        : null,
    );
  }

  await load();
  return m;
}

// ---------------------------------------------------------------------
//  音色接收端：各来源（receiver v1.5.0）
//
//  每个网站一份参考音频，落在 <dir>/<source>/ref.wav。
//  以前所有站点共用 <dir>/ref.wav，后传的覆盖先传的 —— 用户听到的是
//  别的站的音色，界面上完全看不出来（而且非 wav 字节被改名成 .wav 后
//  上游会返回 0 字节，就是那次"合成失败"的根因）。
//
//  这份列表直接来自接收端的 /voice/sources：解析/转码只在接收端做一次，
//  面板不重复实现一套"什么算合法音频"的判断。
// ---------------------------------------------------------------------

export async function openVoiceSourcesModal(parentModal) {
  const box = h('div', [
    h('div.empty', [h('div.big', { text: '🎙' }), h('p', { text: '正在读取各来源…' })]),
  ]);
  const m = modal({ title: '音色来源（每个网站一份参考音频）', wide: true, body: box });

  function fmtTime(sec) {
    if (!sec) return '未使用';
    const d = new Date(sec * 1000);
    const p = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
  }

  // 选文件 → 上传（同一个入口用于"替换"与"新增"）
  function pickAndUpload(source, label) {
    const input = h('input', { type: 'file', accept: 'audio/*', style: { display: 'none' } });
    input.addEventListener('change', async () => {
      const file = input.files && input.files[0];
      if (!file) return;
      const t = toast(`正在上传「${label}」…`, 'info', 60000);
      try {
        const res = await api.uploadVoiceSource(source, file);
        const warn = res && res.warning;
        toast(warn ? `已保存，但要注意：${warn}` : `已保存「${label}」的音色（${Number(res?.duration || 0).toFixed(1)} 秒）`,
          warn ? 'warn' : 'ok', warn ? 8000 : 4200);
        if (res && res.truncated) toast('原音频超过 12 秒，已自动截断', 'warn', 6000);
        await load();
      } catch (e) {
        toast(e.message, 'err', 9000);
      } finally {
        t.remove?.();
        input.value = '';
      }
    });
    document.body.appendChild(input);
    input.click();
    setTimeout(() => input.remove(), 60000);
  }

  async function addSource() {
    const src = await promptBox({
      title: '新增音色来源',
      label: '来源标识（建议用网站域名或站点 slug，只能用字母数字 - _）',
      placeholder: '例如 zizdog-cn',
      hint: '每个来源各自保存一份 ref.wav；网站调用合成时带上同一个标识即可。',
    });
    if (!src) return;
    const clean = String(src).trim();
    if (!clean) return;
    pickAndUpload(clean, clean);
  }

  async function load() {
    clear(box);
    let list = [];
    let err = null;
    try {
      const r = await api.voiceSources();
      list = r.sources || [];
    } catch (e) {
      err = e;
    }

    if (err) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('p', { text: err.message }),
        h('div.hint', { text: '这个页面需要接收端 v1.5.0。旧版本请在「应用市场 → 音色样本接收端」重新部署一次。' }),
      ]));
      return;
    }

    const rows = list.map((s) => h('tr', [
      h('td', [
        h('strong', { text: s.source }),
        s.builtin
          ? h('span.sub', {
              title: '面板自带的默认音色：没指定音色的调用（插件、脚本、其它程序）会用它',
              text: '  （内置默认音色' + (s.has_ref_text ? '，含参考文字' : '') + '）',
            })
          : null,
        (!s.builtin && s.has_ref_text)
          ? h('span.sub', { text: '  （含参考文字）' })
          : null,
        s.legacy
          ? h('span.sub', { text: '  （v1.4.0 残留，已不再使用，可删除）' })
          : null,
      ]),
      h('td', [
        s.duration > 0
          ? h('span', { text: `${Number(s.duration).toFixed(1)} 秒 / ${bytes(s.size)}` })
          : h('span', {
              style: { color: 'var(--danger)' },
              title: '读不出时长：这份文件多半不是合法 wav（例如把 mp3 改名成 .wav 传上来的）',
              text: `⚠️ 无法解析 / ${bytes(s.size)}`,
            }),
      ]),
      h('td', { text: (s.sha256 || '').slice(0, 8) + '…', title: s.sha256 || '' }),
      h('td', { text: fmtTime(s.last_used) }),
      h('td', { style: { display: 'flex', gap: '6px' } }, [
        h('button.btn.btn-sm', {
          text: '替换',
          title: '上传新的参考音频（会做校验并转成 24kHz 单声道 wav）',
          onclick: () => pickAndUpload(s.source, s.source),
        }),
        h('button.btn.btn-sm.btn-danger', {
          text: '删除',
          onclick: async () => {
            const extra = s.legacy
              ? '\n\n这是 v1.4.0 留在根目录的残留文件，v1.5.0 已经不再读它 —— 删掉不影响任何网站。'
              : '';
            if (!await confirmBox(
              `删除来源「${s.source}」的音色样本${s.legacy ? '（v1.4.0 残留）' : ''}？\n\n`
              + '之后该来源的合成请求会返回 400（voice sample not found），直到重新上传。\n'
              + '如果网站还在用这个来源，合成会立刻失败。' + extra,
              { title: '删除音色来源', danger: true, okText: '确认删除' })) return;
            try {
              await api.deleteVoiceSource(s.source);
              toast(`已删除「${s.source}」`, 'ok');
              await load();
            } catch (e) { toast(e.message, 'err'); }
          },
        }),
      ]),
    ]));

    appendAll(box,
      h('div.hint', {
        style: { marginBottom: '10px' },
        text: '每个来源一份独立的参考音频（<样本目录>/<来源>/ref.wav）。'
          + '上传时会校验是不是真音频，并统一转成 ≤12 秒、24kHz、单声道的 wav —— '
          + '上游按扩展名选解码器，改名的假 wav 会让合成返回空音频。',
      }),
      list.length
        ? h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '来源标识' }), h('th', { text: '时长 / 大小' }),
              h('th', { text: 'sha256' }), h('th', { text: '最后使用' }), h('th', { text: '操作' }),
            ])]),
            h('tbody', rows),
          ])
        : h('div.empty', [
            h('div.big', { text: '📭' }),
            h('h4', { text: '还没有任何来源的音色样本' }),
            h('p', { text: '可以点下面的按钮先上传一份，也可以等网站自己上传。' }),
          ]),
      h('div', { style: { marginTop: '14px', display: 'flex', gap: '8px' } }, [
        h('button.btn.btn-sm.btn-primary', { text: '➕ 上传新来源', onclick: addSource }),
        h('button.btn.btn-sm', { text: '🔄 刷新', onclick: load }),
      ]),
    );
  }

  await load();
  return m;
}

// ============================================================================
//  应用自己的功能入口（调用密钥 / 各来源 / 模型 …）
//
//  这是"某个应用多出来的那几颗按钮"的**唯一登记处**：servicePanel.js 只调用
//  appWidgets(s, parentModal)，不自己按应用写分支。以后要给某个应用加专属入口，
//  实现与按钮都加在这个文件里，然后在下面的注册表里多写一行 ——
//  应用市场与服务管理两处会**同时**出现，不会再一次出现"一个入口只在一个页面有"
//  的割裂（用户 2026-09-16 反馈的正是这个）。
// ============================================================================

// WIDGET_BUTTONS：launchd 标签 → 造按钮的函数。
//
// 用 label 而不是"应用 ID"当键：服务记录里可靠的就是 launch_label
// （Qwen 那条还有 name 的兼容写法），目录 ID 在服务记录里并不存在。
const WIDGET_BUTTONS = {
  // 音色接收端：调用密钥（多密钥 + 每把额度 + 用量）与各来源（每站一份参考音频）
  [RECEIVER_LABEL]: (parentModal) => [
    h('button.btn.btn-sm', {
      text: '🔑 调用密钥',
      title: '添加/停用/删除各网站的调用密钥，设置每把的额度（单位：字），查看用量',
      onclick: () => openVoiceKeysModal(parentModal),
    }),
    h('button.btn.btn-sm', {
      text: '🎙 各来源',
      title: '查看 / 替换 / 删除各网站的音色样本',
      onclick: () => openVoiceSourcesModal(parentModal),
    }),
  ],
  // Qwen3 TTS：模型是否驻留内存、手动加载/释放
  [qwenLaunchLabel]: (parentModal, s) => [
    h('button.btn.btn-sm', {
      text: '🧠 模型',
      title: '查看模型是否驻留内存、手动加载或释放（释放后下次请求会自动重新加载）',
      onclick: () => openQwenModels(s),
    }),
  ],
};

/**
 * appWidgets 返回这条服务自己的功能按钮（没有专属入口时返回空数组）。
 *
 * 为什么参数里既有 s 又有 parentModal：按钮的 onclick 里要用它们，
 * 但**不能在渲染时就知道要开哪个弹窗**（那要等用户点），所以这里只是把值闭包进去。
 * 传进来的 s 必须是完整服务记录（面板里就是查好的那条）。
 */
export function appWidgets(s, parentModal) {
  if (!s) return [];
  const make = WIDGET_BUTTONS[s.launch_label] || (isQwenService(s) ? WIDGET_BUTTONS[qwenLaunchLabel] : null);
  return make ? make(parentModal, s) : [];
}

// ============================================================================
//  凭据 / 配置文件弹窗（供服务页与应用市场复用）
//
// 这段原本写在 ServicesView 内部，2026-09-16 提到模块级并导出：
// 应用市场的卡片也要给同一颗「📝 编辑配置文件」按钮（frpc / Orbien 客户端
// 这类应用的用户流程就是"改配置 → 重启"，两个页面必须复用同一个弹窗 ——
// 各写一份的话，改了一处另一处就会烂掉）。
// 「查看凭据」目前只在服务详情里用，但顺手一起导出，方便以后需要时复用。
// ============================================================================

// 哪些凭据算"登录凭据"。
//
// 为什么需要这份名单：接收端的凭据接口会把它自己配置文件里的 token 一并报出来
// （ttsv_token / admin_token 等），那不是给人登录用的账号口令。
// 以前不看这个，应用市场/服务详情里每个接收端条目都会多出一颗「查看凭据」，
// 用户点开看到的是一堆自己不认识的内部密钥。
//
// 一处定义、两处使用：credentialsModal（弹窗里也过滤）与 servicePanel
// （决定"要不要给这颗按钮"）—— 两边判据必须一致，否则会出现
// "有按钮但点开是空的"。
const NOT_LOGIN_CREDS = new Set(['ttsv_token', 'admin_token', 'token', 'shared_token', 'secret']);
export function loginCredsOf(list) {
  return (list || []).filter((c) => !NOT_LOGIN_CREDS.has(String((c && c.key) || '').toLowerCase()));
}

// ---------- 查看凭据 ----------
//
// 为什么需要：frpc / Orbien 这类应用的 admin UI 用的是**面板随机生成**的
// 用户名与口令。安装时它们只出现在任务日志的"凭据区块"里，事后再找就只能
// 翻配置文件（真机反馈："安装的时候也没让设置啊！我用你妈逼登陆吗"）。
// 所以这里把它做成常驻入口：随时能看、能复制。
export function credentialsModal(cur) {
  const box = h('div');
  const m = modal({ title: `登录凭据：${cur.display_name}`, body: box });

  async function loadCreds() {
    clear(box);
    appendAll(box, h('div.empty', [h('p', { text: '正在读取…' })]));
    let res;
    try {
      res = await api.serviceCredentials(cur.name);
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读不到凭据' }),
        h('p', { text: e.message }),
      ]));
      return;
    }
    // 只看"登录凭据"：接收端那些内部 token 不算（见 loginCredsOf）。
    const list = loginCredsOf(res && res.credentials);
    clear(box);
    if (!list.length) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '🔓' }),
        h('h4', { text: '这个应用没有配置登录凭据' }),
        h('p.hint', { text: '它的管理界面不需要密码，或者凭据不在配置文件里。' }),
      ]));
      return;
    }
    const rows = list.map((c) => h('div', {
      style: { display: 'flex', gap: '10px', alignItems: 'center', padding: '8px 0', borderBottom: '1px solid var(--line,#e5e7eb)' },
    }, [
      h('div', { style: { minWidth: '160px' } }, [
        h('div', { text: c.label }),
        h('div.sub', { style: { fontSize: '12px', opacity: 0.7 }, text: c.key }),
      ]),
      h('code.code', { style: { flex: '1', wordBreak: 'break-all' }, text: c.value }),
      h('button.btn.btn-sm', {
        text: '复制',
        onclick: async () => {
          try { await navigator.clipboard.writeText(c.value); toast('已复制', 'ok'); }
          catch (_) { toast('复制失败，请手动选中', 'err'); }
        },
      }),
    ]));
    appendAll(box, h('div', [
      res.ui ? h('div.hint', { html: '管理界面：<code class="code">' + res.ui + '</code>（用下面的账号口令登录）' }) : null,
      h('div.hint', { text: '文件：' + res.config_path }),
      ...rows,
      h('div', { style: { marginTop: '14px' } }, [
        h('button.btn.btn-sm', { text: '↻ 重新读取', onclick: loadCreds }),
      ]),
    ]));
  }
  loadCreds();
}

// ---------- 编辑配置文件 ----------
//
// 为什么需要：frpc / Orbien 客户端这类应用确实"在面板里跑"，
// 但它们的配置不是几张表单能覆盖的（token、隧道、反代规则、证书…）。
// 面板只做两件事：给出文件路径、提供一个文本框；读写走**既有的**文件接口
// （GET /api/v1/files/read、POST /api/v1/files/write），
// 白名单与越界校验完全复用，不新造一套文件读写。
//
// 保存后**不自动重启**：配置里往往有端口/token，改错了要能先看一眼再重启，
// 所以保存只提示"重启后生效"，重启交给旁边那颗显式按钮。
export function configFileModal(s, reload) {
  const path = s.config_path;
  const box = h('div', [h('div.empty', [h('p', { text: '正在读取配置文件…' })])]);
  const m = modal({ title: `配置文件：${s.display_name}`, wide: true, body: box });
  let editor = null;
  let saved = '';

  const body = () => h('div', [
    h('div.hint', { text: '文件：' + path }),
    editor,
    h('div', { style: { marginTop: '12px', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
      h('button.btn.btn-sm.btn-primary', { text: '💾 保存', onclick: save }),
      h('button.btn.btn-sm', { text: '🔄 重启服务', title: '保存后必须重启才生效', onclick: restart }),
      h('button.btn.btn-sm', { text: '↻ 重新读取', onclick: loadFile }),
      h('span.sub', { style: { marginLeft: 'auto' }, text: '保存后需重启服务生效' }),
    ]),
  ]);

  async function loadFile() {
    clear(box);
    appendAll(box, h('div.empty', [h('p', { text: '正在读取配置文件…' })]));
    let res;
    try {
      res = await api.fileRead(path);
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取失败' }),
        h('p', { text: e.message }),
        h('p.hint', {
          text: '文件：' + path + '。若服务还没启动过、配置尚未生成，' +
            '先启动一次服务再回来编辑。',
        }),
      ]));
      return;
    }
    if (res.binary || res.too_large) {
      clear(box);
      appendAll(box, h('div.empty', [
        h('div.big', { text: '🔒' }),
        h('h4', { text: res.binary ? '这是二进制文件' : '文件过大' }),
        h('p', {
          text: res.binary
            ? '为避免破坏文件，不提供在线编辑。'
            : '超过在线编辑上限（2MB），请下载后用本地工具处理。',
        }),
      ]));
      return;
    }
    editor = h('textarea.input', {
      value: res.content ?? '',
      spellcheck: 'false',
      style: {
        width: '100%', minHeight: '360px', fontFamily: 'var(--mono)',
        fontSize: '13px', lineHeight: '20px', whiteSpace: 'pre', overflow: 'auto',
      },
    });
    saved = editor.value;
    clear(box);
    appendAll(box, body());
  }

  async function save() {
    if (!editor) return;
    if (editor.value === saved) { toast('内容没有变化', 'warn'); return; }
    const t = toast('保存中…', 'info', 0);
    try {
      await api.fileWrite(path, editor.value);
      saved = editor.value;
      t.remove();
      toast('已保存。请点「🔄 重启服务」让它生效', 'ok', 9000);
    } catch (e) {
      t.remove();
      toast('保存失败：' + e.message, 'err', 12000);
    }
  }

  async function restart() {
    const t = toast(`重启「${s.display_name}」中…`, 'info', 0);
    try {
      await api.serviceAction(s.name, 'restart');
      t.remove();
      toast(`「${s.display_name}」已重启`, 'ok');
      if (typeof reload === 'function') reload();
    } catch (e) {
      t.remove();
      toast('重启失败：' + e.message + '。请到「日志」里看原因', 'err', 14000);
    }
  }

  loadFile();
}
