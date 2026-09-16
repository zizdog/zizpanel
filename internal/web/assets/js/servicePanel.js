// servicePanel.js —— 一个应用 / 一个服务，**唯一**的详情面板与其动作清单。
//
// ---------------------------------------------------------------------------
//  为什么要有这个文件（用户 2026-09-16 的原话）
//
//  "你的服务管理页面真的废物！我在软件处点击查看服务跳转过去！能作的也就是：
//   停止重启这些，直接放在软件页面不就行了？折腾什么？再说 frp 的使用配置在
//   软件市场页面，tts 的在服务页面，不割裂呈，不愚蠢吗？"
//
//  翻译成两个设计约束：
//    ① 市场卡片上的"查看服务"不该只是跳去服务管理页 —— 跳过去能做的事
//       （启停/重启/改配置/看日志）必须在卡片上直接能做；
//    ② 同一个应用的配置入口不能一半在市场、一半在服务管理 ——
//       用户不该记住"哪个应用在哪个页面配"。
//
//  所以：市场卡片与服务管理**两处都通向这同一个面板**（openServicePanel），
//  面板内部按同一个结构给全部能力：
//      状态 / 操作 / 配置 / 日志 / 凭据 / 界面 / 收尾
//
// ---------------------------------------------------------------------------
//  三条实现纪律（改这个文件前先读完）
//
//  1. **不出现任何应用 ID 的 if/else。** 哪颗按钮出现，全部由数据决定：
//       · config_path 有没有            → 有没有「📝 编辑配置文件」
//       · managed 是 true 还是 false    → 「卸载」还是「取消纳管」
//       · ui.slug 且 !ui.console_only   → 有没有「打开界面」（见 hasPanelUI）
//       · 凭据接口返回空                → 不给「🔑 查看凭据」
//       · 目录条目的 app_widgets        → 有没有这个应用自己的功能入口
//     这个项目正在消灭"一个应用一个补丁"，任何 `if (m.id === 'frpc')` 都是回退。
//
//  2. **动作只有一份实现。** 启停/重启、配置文件编辑器（configFileModal）、
//     凭据弹窗（credentialsModal）、日志弹窗（openLogs）都从 services.js 复用；
//     卡片上的按钮与面板里的按钮出自同一个 serviceActions()，
//     不存在"卡片一套、面板另一套"的第二份实现。
//
//  3. **市场与我们自己的数据形状不一样，在这里归一化。**
//     市场条目（api.market）有 id / installed / config_path（只有文件名）/ uninstall 计划；
//     服务记录（api.service）有 name / managed / config_path（**绝对路径**）。
//     面板两种都能开：只给市场条目时自己补查服务记录（面板打开慢一点点没关系，
//     而市场列表一次渲染十几张卡片，绝不能在渲染时逐张发请求查服务）。
// ---------------------------------------------------------------------------

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { panelPath } from './app.js';
import { taskCenter } from './tasks.js';
import {
  configFileModal, credentialsModal, openLogs, healthHint, loginCredsOf, appWidgets,
} from './services.js';

// PANEL_QUERY_TIMEOUT_MS 是面板首屏查询的**硬上限**。
//
// 为什么需要它（用户 2026-09-16 反馈："ffmpeg 一直显示读取中…、卡住"）：
// 面板首屏要查服务记录与凭据，每一步内部都 try/catch —— 失败会落定成
// "没有这项数据"。但如果请求本身**永远不回来**（后端假死 / 连接被挂住），
// 界面就会一直停在「正在读取服务状态…」这个中间态上。
// 加上这个上限后，"成功 / 失败 / 超时"三种情况都会走到 render()：
// 要么拿到真实状态，要么按"没有服务记录"如实渲染。**不存在不落定的读取中**。
const PANEL_QUERY_TIMEOUT_MS = 10000;

// ---------------------------------------------------------------------------
//  归一化：把市场条目与服务记录收敛成面板需要的那几个字段
// ---------------------------------------------------------------------------

// serviceNameOf 取"面板接口认的服务名"。
//
// **顺序很重要，2026-09-16 踩过一次**：市场条目里的 `name` 是**展示名**
// （目录条目的 App.Name，例如 "frpc（frp 客户端）"），不是服务记录名。
// 拿它去 GET /services/<名> 会 404，面板就显示成"未在服务管理里"、
// 只给「启动」，配置文件路径与日志也全拿不到（假数据端到端验证抓到的就是这一条）。
// 所以标识类字段必须先试：
//   · uninstall.service —— 后端填的就是 rec.Name（见 services.PlanUninstallFor），**权威**；
//   · service_label     —— 目录里声明的 launchd 标签（frpc 是 com.zizdog.frpc）；
//   · name              —— 服务记录自己的标识字段；对市场条目是展示名（兜底）；
//   · id                —— 目录 ID，后端 FindAppByService 也认这种写法。
export function serviceNameOf(m) {
  if (!m) return '';
  return m.uninstall?.service || m.service_label || m.name || m.id || '';
}

// serviceNamesOf 返回"可能要试的几个服务名"。
// 目录里的 ID / label 与真实记录名并非总是一致（例如 uptime-kuma 的记录名就是
// 目录 ID 本身），与其在前端猜一个，不如把候选逐个试一遍 —— 查服务详情很便宜，
// 而猜错一次的代价是"面板看起来什么都不能做"。
function serviceNamesOf(m) {
  if (!m) return [];
  const out = [];
  for (const k of [m.uninstall?.service, m.service_label, m.name, m.id]) {
    if (k && !out.includes(k)) out.push(k);
  }
  return out;
}

// svcNameCache 记住"某个市场条目对应哪条服务记录"，避免每次开面板都试 2-3 次。
// 只活在内存里；失败不缓存（下次可能就好了，例如用户刚点了「纳管」）。
const svcNameCache = new Map();

// hasPanelUI 与 apps.js 的 hasPanelUI 同一判据（**必须一致**：卡片上有「打开」
// 而面板里没有，或反过来，用户会以为功能丢了）：有 slug 且不是应用自带的控制台。
export function hasPanelUI(m) {
  return !!(m && m.ui && m.ui.slug && !m.ui.console_only);
}

// vendorImage 取 Docker 目录条目里的镜像名（在面板里显示"官方镜像是什么"）。
function vendorImage(m) {
  const yaml = (m && m.compose_yaml) || '';
  const mm = yaml.match(/^\s*image:\s*(\S+)/m);
  return mm ? mm[1] : '';
}

// resolvePanelData 把调用方给的东西补全成"面板能完整渲染"的数据。
//
// market —— 市场条目（可能没有）；svc —— 服务记录（可能没有，服务管理页总是有）。
async function resolvePanelData(market, svc) {
  const m = market || null;
  let s = svc || null;
  let name = serviceNameOf(s) || serviceNameOf(m);

  // 只给了市场条目（从应用市场点开）时必须补查一次服务记录：
  // 配置文件的**绝对路径**、managed、driver_ready、日志路径都只有服务记录才有，
  // 而市场条目里的 config_path 只是文件名（frpc.toml），拿它去读文件必然失败。
  //
  // 候选名逐个试：命中一个就用它（启停/日志/配置接口都只认这个名字）。
  // 全都不中说明这个应用还没有服务记录（没纳管/没装），按"无记录"如实渲染。
  //
  // 例外（用户 2026-09-16 反馈 "ffmpeg 未在服务管理里"）：`no_daemon` 的应用
  // **本来就没有守护进程**，目录里也没有 service_label —— 它不可能有服务记录。
  // 这种情况**不去查**（省掉注定 404 的请求，也不会把"没有记录"渲染成异常态）。
  // 判据全部来自数据：no_daemon 且没有任何服务标识。
  // 注意 m 可能为 null（服务管理页点开面板时只给 svc）—— 必须先判 m，
  // 否则这里会抛 TypeError，把整次渲染推进兜底分支（凭据/界面按钮就没了）。
  const noServiceByIdentity = !!(m && m.no_daemon)
    && !m.service_label
    && !(m.uninstall && m.uninstall.service);
  if (!s && m && !noServiceByIdentity) {
    const cached = svcNameCache.get(name);
    const cands = cached ? [cached, ...serviceNamesOf(m)] : serviceNamesOf(m);
    for (const cand of cands) {
      if (!cand) continue;
      try {
        const got = await api.service(cand);
        if (got && got.name) { s = got; name = got.name; svcNameCache.set(serviceNameOf(m), got.name); break; }
      } catch { /* 试下一个候选 */ }
    }
  }
  // 凭据接口返回**登录用**凭据才给「🔑 查看凭据」（空则不给，见 loginCredsOf）。
  // 这里先查一次：让"按钮存在"本身就等于"点开有东西看"。
  // 为什么不做成"先给按钮、点开再查"：给一颗点开是空弹窗的按钮，
  // 正是这个项目反复被投诉的那类误导（点了没用的按钮比没有按钮更糟）。
  // 服务详情本来就便宜（本机接口），多这一次查询换来的是诚实的按钮清单。
  let cred = null;
  const credName = s ? serviceNameOf(s) : '';
  if (credName) {
    try {
      const res = await api.serviceCredentials(credName);
      const list = loginCredsOf(res && res.credentials);
      if (list.length) cred = { list, ui: (res && res.ui) || '', config_path: (res && res.config_path) || '' };
    } catch { /* 没有凭据接口/读不到 → 不给按钮 */ }
  }
  return { market: m, svc: s, name: s ? serviceNameOf(s) : name, cred };
}

// ---------------------------------------------------------------------------
//  动作：按钮只有这一份实现，卡片与面板都从这里取
// ---------------------------------------------------------------------------

// serviceActions 生成"启停/重启/刷新"三颗按钮（首颗是启动还是停止看状态）。
//
// 状态从哪来：服务管理页把它已经加载好的 state 塞进 `m.state`；
// 市场卡片**没有** state（不能在渲染时逐张请求），于是退化成"启动" ——
// 后端对一个已在跑的服务执行 start 是幂等的，所以不会出现"在跑却让你启动"
// 那种谎报；只是首屏少一次状态同步，点「⟳ 刷新」即可确认。
export function serviceActions(m, { onDone } = {}) {
  const name = serviceNameOf(m);
  const st = (m && m.state) || {};
  const canControl = !m || m.driver_ready !== false;
  const label = (m && (m.display_name || m.name)) || name || '这个服务';
  const btn = (text, action, cls = '', title = '') => h('button.btn.btn-sm' + cls, {
    text,
    // driver 没就绪（例如 Docker 没装却要点 compose 应用）时禁用：
    // 给一颗点下去必然报错的按钮等于误导（这是之前服务页已有的一致性）。
    disabled: !canControl || !name,
    title: title || `${text}「${label}」`,
    onclick: () => doServiceAction(name, action, m, onDone),
  });
  return [
    st.running === true
      ? btn('停止', 'stop')
      : btn('启动', 'start', '.btn-ok'),
    btn('重启', 'restart'),
    // 状态刷新不是"动作"：只重新查一次，不改动服务。市场卡片上手动修正首屏状态用它。
    // 文案按用户要求从「刷新状态」缩短为「刷新」（行为完全不变，只是标签）。
    btn('⟳ 刷新', 'status', '', '重新查询它在系统里的真实状态（不改动服务）'),
  ];
}

// doServiceAction 执行一次服务动作并如实报告结果。
//
// 这是**唯一**的启停实现：应用市场卡片、服务管理卡片、详情面板都走它。
// 以前市场卡片与面板各写一份，一处改了另一处就烂 ——
// 用户看到的"有的地方能重启、有的地方只有跳转"就是这么来的。
export async function doServiceAction(name, action, m = {}, onDone) {
  if (!name) { toast('不知道这条服务在面板里的名字，无法操作', 'err', 9000); return; }
  const label = m.display_name || m.name || name;

  if (action === 'status') {
    try {
      const fresh = await api.service(name);
      const st = (fresh && fresh.state) || {};
      toast(`「${label}」当前：${st.running ? '运行中' : (st.status || '已停止')}`, st.running ? 'ok' : 'info', 6000);
      if (typeof onDone === 'function') onDone(fresh);
    } catch (e) {
      toast('查询失败：' + e.message, 'err', 9000);
    }
    return;
  }

  const labels = { start: '启动', stop: '停止', restart: '重启' };
  const t = toast(`${labels[action] || action}「${label}」中…`, 'info', 0);
  let failed = false;
  let warning = '';
  try {
    const resp = await api.serviceAction(name, action);
    warning = (resp && resp.warning) || '';
  } catch (e) {
    failed = true;
    toast(`${labels[action] || action}失败：${e.message}。可打开「⚙️ 管理 → 📜 日志」看原因`, 'err', 14000);
  }
  t.remove();
  // 后端说"操作完成但结果没被确认"时（例如启动后 8 秒还没看到它跑起来），
  // 必须原样告诉用户，不能报一个干净的"已启动" —— 那正是谎报成功。
  if (!failed && warning) toast(`「${label}」已请求${labels[action] || action}：${warning}`, 'warn', 14000);
  else if (!failed) toast(`「${label}」已${labels[action] || action}`, 'ok');
  // 不论成败都刷新一次：后端可能已经改了状态却返回了错误（历史上 launchctl
  // 的退出码就谎报过成功），界面必须显示的是**系统里的真实状态**而不是我们的预期。
  if (typeof onDone === 'function') onDone();
}

// marketQuickActions 给**应用市场卡片**的那一排常用动作。
//
// 用户要求"已安装的应用，卡片上直接给出常用动作，不需要先跳到服务管理页"。
// 这里放卡片上真正好用的几个：启停/重启/刷新 + 管理。
// 「📝 编辑配置文件」「📜 日志」「🔑 凭据」都在管理面板里 ——
// 它们要么需要服务记录（市场条目只有配置**文件名**，绝对路径只能从服务记录拿），
// 要么需要一个更宽的弹窗才看得清；卡片宽度不适合摊开一个编辑器。
// 关键点：点「管理」打开的就是**与服务管理里同一个面板**，用户没有被搬走。
//
// 2026-09-16 起卡片上**只有这一颗**"进面板"的按钮：以前同时有卡片主按钮
// 「查看服务」（apps.js 的 primaryButton）与这里的「⚙️ 详情」，两者打开的是
// 同一个面板 —— 用户原话"原来的详情和查看服务内容一样，统一保留一个管理就行了"。
// 现在主按钮是「重装」/「安装」，进面板只剩这颗「⚙️ 管理」。
//
// opts.state：市场页顺手拉到的服务状态（见 apps.js 的 svcState）。
// 有它卡片上的首颗按钮才是对的（在跑→「停止」，没跑→「启动」）；
// 没有就退化成「启动」，后端对已在跑的服务执行 start 是幂等的。
// opts.onManage：由调用方接管"开管理面板"（应用市场传的是 apps.js 的
// openAppDetail）。为什么要让调用方接管：市场页手里有界面探测结果 proxyState，
// 面板里的「打开界面」要用同一个结论（子路径还是端口直连）——否则卡片上写
// 「直连端口」、点进面板却给一个已知打不开的子路径，两个入口自相矛盾。
// 服务管理页不开这个按钮，所以给不给 onManage 都不影响它。
export function marketQuickActions(m, { state, onDone, onManage } = {}) {
  const installed = !!(m && (m.installed || m.adopted));
  if (!installed) return [];
  // 卡片上的启停按钮只在"**服务确实可管**"时给：有面板记录（adopted）或
  // 服务已在 launchd 里。装了但服务没注册的孤儿态（plist 丢了 / 装到一半）点启停
  // 只会报"找不到服务"；而 CLI 工具（ffmpeg 这类 no_daemon）**本来就没有服务**，
  // 给它启停按钮同样是点了必然报错。这两种状态该用的按钮是卡片上的
  // 「重装」，所以这里只留「⚙️ 管理」，让用户先看清状态。
  const canControl = !!(m.adopted || m.service_in_launchd);
  return [
    ...(canControl ? serviceActions(state ? { ...m, state } : m, { onDone }) : []),
    h('button.btn.btn-sm', {
      text: '⚙️ 管理',
      title: '状态 / 启停 / 配置 / 日志 / 凭据 / 卸载 —— 都在这里，不用跳去服务管理页',
      onclick: () => (typeof onManage === 'function' ? onManage() : openServicePanel({ market: m, onDone })),
    }),
  ];
}

// ---------------------------------------------------------------------------
//  详情面板
// ---------------------------------------------------------------------------

function pill(cls, text, title) {
  return h('span.pill' + (cls ? '.' + cls : ''), { text, title: title || '' });
}

// raceDeadline 给一个 promise 加硬上限：到点还没落定就 reject（调用方按"读不到"处理）。
// 定时器在 promise 落定后清掉，不会留下悬挂的 timer。
function raceDeadline(p, ms, label) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + '超过 ' + ms + 'ms 没有返回')), ms);
  });
  return Promise.race([p, deadline]).finally(() => clearTimeout(timer));
}

// statusLine 把服务状态翻译成"一眼能看懂"的一行。
//
// 2026-09-16 修 bug（用户反馈 "ffmpeg 显示读取中…未在服务管理里"、而且**卡在**
// 读取中上）：以前 st 为空就一律返回「读取中…」。但 render() 只在数据**已经查完**
// 之后才被调用（见文件末尾的 `render(await resolvePanelData(...))`），所以 st
// 为空**不是"还在读"，而是"这个应用根本没有服务记录"**。两种情况混成一个文案，
// 没有守护进程的应用（ffmpeg / phpMyAdmin 这类 no_daemon）就永远停在一句
// 不落定的话上，看上去像界面卡死。
//
// 现在按**目录数据**给终态，不再有"读取中"这种中间态从 render 里冒出来：
//   · no_daemon 且面板托管界面 → 网页入口（本来就没有常驻进程）
//   · no_daemon 且没有界面     → 命令行工具（本来就没有常驻进程）
//   · 其余（已安装但没纳管）    → 未纳管（有服务可纳管，只是还没登记）
function statusLine(st, m) {
  if (st) {
    if (st.running) return { cls: 'ok', text: '运行中', title: st.detail || '' };
    if (st.status === 'error') return { cls: 'danger', text: '异常', title: st.detail || '' };
    if (st.status === 'unavailable') return { cls: 'warn', text: '环境不可用', title: st.detail || '' };
    if (st.status === 'not-installed') return { cls: 'warn', text: '未安装', title: st.detail || '' };
    if (st.status === 'unknown') return { cls: '', text: '未知', title: st.detail || '' };
    return { cls: '', text: '已停止', title: st.detail || '' };
  }
  if (m && m.no_daemon) {
    const webUI = hasPanelUI(m) || !!(m.ui && m.ui.self_conf);
    return webUI
      ? { cls: '', text: '网页入口（无常驻进程）', title: '这个应用没有守护进程，装完就是一个网页入口，服务管理里不会有它' }
      : { cls: '', text: '命令行工具（无常驻进程）', title: '这个应用是命令行工具：没有守护进程、也没有网页界面，供面板或其它应用在后台调用' };
  }
  return { cls: '', text: '未纳管', title: '面板里还没有这条服务的记录，所以拿不到配置文件路径与日志' };
}

// uiButtonFor 给出「打开界面」入口（仅当这个应用有**面板托管的**网页界面）。
//
// 这不是"第二份按钮逻辑"，而是同一份判据在面板里的再现：面板是唯一入口，
// 若这里不判断，就会出现"市场卡片上有「打开」、点进详情反而没有"的割裂。
// 判据依然是数据：ui.slug / ui.console_only / ui.prefer_direct / ui.self_conf / 探测结果。
// （apps.js 的 openButtons 还多一层"主入口/次要入口"的排序，那是卡片才需要的。）
function uiButtonFor(m, proxyState) {
  if (!hasPanelUI(m)) return null;
  const path = '/' + m.ui.slug + '/';
  const direct = m.port_url || '';
  if (m.ui.self_conf) {
    // 这类应用的 nginx location 由安装器自己写、且只允许本机（phpMyAdmin），
    // 唯一能用的入口是**面板自己**那条（要求先登录面板）。
    return h('a.btn.btn-sm.btn-primary', {
      href: panelPath('phpmyadmin/'), target: '_blank', rel: 'noopener', text: '🌐 打开界面',
      title: '经面板打开（需先登录面板）',
    });
  }
  const probed = !!(proxyState && !proxyState._stale);
  const st = ((proxyState && proxyState.items) || []).find((x) => x.slug === m.ui.slug) || {};
  // 尚未探测（_stale）时按"能用"对待 —— 否则没点过「检测可用性」的用户会看到
  // 所有应用都被降级成端口直连（那不是我们想给的默认结论）。
  const proxyOK = !m.ui.prefer_direct && (probed ? !!st.proxy_ok : true);
  const why = m.ui.prefer_direct ? (m.ui.note || '这个应用不支持子路径') : (st.reason || m.ui.note || '');
  if (proxyOK) {
    return h('a.btn.btn-sm.btn-primary', {
      href: path, target: '_blank', rel: 'noopener', text: '🌐 打开界面',
      title: '经面板的 /' + m.ui.slug + '/ 打开（所有入口都通）',
    });
  }
  if (direct) {
    // 子路径已知不可用（PreferDirect 或探测失败）：主入口必须是端口直连，
    // 否则用户点开拿到的是一个已知打不开的地址。
    return h('a.btn.btn-sm.btn-primary', {
      href: direct, target: '_blank', rel: 'noopener', text: '🌐 打开界面',
      title: '这个应用挂子路径实测不可用，直接给端口入口：' + direct + (why ? '（' + why + '）' : ''),
    });
  }
  return h('a.btn.btn-sm', {
    href: path, target: '_blank', rel: 'noopener', text: '🌐 打开界面',
    title: why || '应用可能没有启动',
  });
}

/**
 * openServicePanel 打开「应用详情」面板 —— 市场卡片与服务管理**共用的唯一入口**。
 *
 * @param {object}  o
 * @param {object} [o.market]  市场条目（api.market 的一项）
 * @param {object} [o.svc]     服务记录（api.service / services 列表的一项）
 * @param {object} [o.proxyState] 市场页的界面探测结果（决定「打开界面」走子路径还是端口）
 * @param {function}[o.onDone]  动作完成后刷新调用方（市场卡片 / 服务卡片）
 * @param {string} [o.primaryText] 面板里的第一颗按钮（"纳管"/"安装"这类**状态相关**动作）；
 *                                 文案与行为由调用方决定，面板只负责把它放在同一屏里。
 * @param {function}[o.primaryRun]
 * @returns {Promise<object>} modal 句柄
 */
export async function openServicePanel(o = {}) {
  const { market, svc, proxyState, onDone, primaryText, primaryRun } = o;
  const m0 = market || null;
  const title = (svc && svc.display_name) || (m0 && m0.name) || '应用管理';

  const statusBox = h('div', [h('div.hint', { text: '正在读取服务状态…' })]);
  const actionBox = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } });
  const detailBox = h('div', [h('div.hint', { text: '正在读取服务详情…' })]);

  const m = modal({
    // 标题 2026-09-16 从「应用详情」改成「应用管理」：市场上通往它的按钮只剩
    // 一颗「⚙️ 管理」（原来的「详情」「查看服务」合并），标题跟着按钮走，
    // 用户不会以为点「管理」开出来的是另一个"详情"页。
    title: '应用管理：' + title,
    wide: true,
    body: h('div', [
      statusBox,
      h('div.section-title', { style: { marginTop: '4px' }, text: '操作' }),
      actionBox,
      h('div.section-title', { style: { marginTop: '14px' }, text: '信息' }),
      detailBox,
    ]),
  });

  let resolved = null; // 最近一次的数据，动作完成后原地重画（不关窗、不重开）

  // renderActions 生成面板里的全部按钮 —— 这就是"不该分散在两个页面"的那份清单。
  // 顺序按用户的使用顺序：
  //   ① 状态相关动作（纳管/安装，由调用方给） ② 启停 ③ 界面 ④ 配置 ⑤ 凭据
  //   ⑥ 日志 ⑦ 应用自己的功能（目录条目的 app_widgets） ⑧ 收尾（取消纳管 / 卸载）
  function renderActions(res) {
    const s = res.svc;
    const mi = res.market || m0;
    const out = [];

    if (primaryText) {
      out.push(h('button.btn.btn-sm.btn-primary', {
        text: primaryText,
        title: '这个应用当前状态下该做的第一步',
        onclick: () => { m.close(); if (typeof primaryRun === 'function') primaryRun(); },
      }));
    }

    const installed = !!s || !!(mi && (mi.installed || mi.adopted));
    if (!installed) {
      // 没装：给一句话说清去哪儿装，而不是给一排点了必然报错的按钮。
      out.push(h('button.btn.btn-sm.btn-primary', {
        text: '安装',
        title: '先安装；装完这里就有启停、配置文件与日志入口',
        onclick: () => {
          m.close();
          if (typeof primaryRun === 'function') primaryRun();
          else toast('请在上一个页面点「安装」', 'info', 6000);
        },
      }));
      if (mi && mi.docs_url) out.push(docLink(mi.docs_url));
      return out;
    }

    // ② 启停/重启 —— 与服务管理卡片**同一份实现**，状态也共用 s.state。
    //
    // 只在"**服务确实可管**"时才给：有服务记录，或市场数据说这条服务已纳管 /
    // 已在 launchd 里。装了但没有服务的应用（ffmpeg 这类 no_daemon 命令行工具、
    // 或者 plist 丢了的孤儿态）给启停按钮，点下去只会报"找不到服务" ——
    // 这个项目反复被投诉的就是"点了没用的按钮比没有按钮更糟"。
    // 判据全部是数据（s / adopted / service_in_launchd），没有按应用 ID 的分支。
    const canControl = !!s || !!(mi && (mi.adopted || mi.service_in_launchd));
    if (canControl) out.push(...serviceActions(s || mi, { onDone: afterAction }));

    // ③ 界面：只有"面板托管的网页界面"才有（frpc 的 7400 控制台不算，用户明确不要）。
    const ui = mi ? uiButtonFor(mi, proxyState) : null;
    if (ui) out.push(ui);

    // ④ 配置：判据是 config_path 有没有（目录数据），不是应用 ID。
    //    路径必须用**服务记录里的绝对路径**：市场条目里的 config_path 只是文件名。
    const cfgPath = (s && s.config_path) || '';
    if (cfgPath) {
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        title: (mi && mi.post_install_hint ? '要改什么：' + mi.post_install_hint + ' —— ' : '') +
          '直接编辑 ' + cfgPath + '（保存后需重启服务才生效）',
        onclick: () => configFileModal(s, afterAction),
      }));
    } else if (mi && mi.config_path) {
      // 有配置文件名、但查不到服务记录（还没纳管）：如实说明原因，
      // 绝不给一个"点了报 400：文件不存在"的编辑器。
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        disabled: true,
        title: '配置文件是 ' + mi.config_path + '，但面板里还没有这条服务的记录，' +
          '拿不到它的绝对路径。先点「纳管」（或在服务管理里启动一次）再来编辑。',
      }));
    }

    // ⑤ 凭据：接口返回了**登录用**的凭据才给按钮（空则不给，见 loginCredsOf）。
    //    按钮存在 = 点开一定有东西看。
    if (s && res.cred && res.cred.list.length) {
      out.push(h('button.btn.btn-sm', {
        text: '🔑 查看凭据',
        title: '显示面板为这个应用生成的管理界面用户名/口令' + (res.cred.ui ? '（' + res.cred.ui + '）' : ''),
        onclick: () => credentialsModal(s),
      }));
    }

    // ⑥ 日志：只要有服务记录就能看（日志路径由服务记录提供）。
    if (s) {
      out.push(h('button.btn.btn-sm', {
        text: '📜 日志',
        title: '实时查看这个服务的日志' + (s.log_path ? '：' + s.log_path : ''),
        onclick: () => openLogs(s),
      }));
    }

    // ⑦ 应用自己的功能入口（调用密钥 / 各来源 / 模型 …）。
    //    实现与"哪个应用有哪几颗"住在 services.js 的 appWidgets 注册表里，
    //    这里只负责把它们放在同一排；没有专属入口的应用得到空数组。
    if (s) out.push(...appWidgets(s, m));

    // ⑧ 收尾：managed=false 只给「取消纳管」，managed=true 才给「卸载」。
    //    这条判据来自**服务记录**，不来自应用 ID —— 非面板管理的服务不出现卸载。
    if (s) {
      if (s.managed === false) out.push(forgetButton(s, afterAction));
      else if (s.managed) out.push(uninstallButton(s, afterAction));
    } else if (mi) {
      if (mi.uninstall?.kind === 'forget') {
        out.push(forgetButton(mi, afterAction));
      } else if (mi.uninstall?.kind === 'installer' || mi.uninstall?.kind === 'service') {
        out.push(marketUninstallButton(mi, afterAction));
      }
    }
    if (mi && mi.docs_url) out.push(docLink(mi.docs_url));
    return out;
  }

  function docLink(url) {
    return h('a.btn.btn-sm', { href: url, target: '_blank', rel: 'noopener', text: '文档' });
  }

  // afterAction 动作完成后重新拉一次数据并原地重画：
  // 用户不必关掉面板再点开（"点一次重启，面板里的状态还是旧的"就是这种坑）。
  async function afterAction(fresh) {
    if (typeof onDone === 'function') onDone();
    const keepSvc = (fresh && fresh.name) ? fresh : (resolved && resolved.svc);
    const res = await resolvePanelData((resolved && resolved.market) || m0, keepSvc);
    render(res);
  }

  function render(res) {
    resolved = res;
    const s = res.svc;
    const mi = res.market || m0;
    const st = (s && s.state) || null;
    const health = (s && s.health) || {};
    const displayName = (s && s.display_name) || (mi && mi.name) || title;

    // ---- 状态 ----
    // 把市场条目也传进去：没有服务记录时要靠它区分"本来就没有守护进程"
    // （no_daemon，终态是"命令行工具/网页入口"）与"装了但还没纳管"。
    const line = statusLine(st, mi);
    clear(statusBox);
    appendAll(statusBox,
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center', marginBottom: '8px' } }, [
        h('div', { style: { fontWeight: '620', fontSize: '14px', marginRight: '4px' }, text: displayName }),
        pill(line.cls, line.text, line.title),
        ((s && s.port) || (mi && mi.port)) > 0 ? pill('', ':' + ((s && s.port) || mi.port)) : null,
        s ? pill(s.managed ? 'brand' : '', s.managed ? '面板托管' : '仅纳管',
          s.managed ? '面板负责完整生命周期，可卸载' : '由你自己安装，面板只做启停与查看，不会卸载') : null,
        // 「未在服务管理里」只对"**本该有服务**却查不到记录"的应用说。
        // no_daemon 的应用（ffmpeg / phpMyAdmin）本来就没有守护进程，报这一句
        // 是纯粹的误导 —— 用户 2026-09-16 反馈的正是 ffmpeg 上这句。
        // 它们的状态由上面的 statusLine 如实说成"命令行工具/网页入口（无常驻进程）"。
        (!s && mi && (mi.installed || mi.adopted) && !mi.no_daemon)
          ? pill('warn', '未在服务管理里',
            '这个应用装在机器上，但面板里还没有对应的服务记录，所以拿不到配置文件路径与日志')
          : null,
        health.checked
          ? (health.ok
            ? pill('ok', '健康', health.message + '（' + (health.latency_ms || 0) + 'ms）')
            : pill('danger', '健康检查失败', health.message))
          : null,
        s && s.driver_error ? pill('warn', '驱动不可用', s.driver_error) : null,
        // 容器类应用：把目录里的官方镜像标出来，用户有个"我这版是不是旧的"的参照
        // （面板不做自动升级，也不假装能做）。
        mi && mi.kind === 'compose' && vendorImage(mi) ? pill('', '官方镜像 ' + vendorImage(mi)) : null,
      ]),
      st && st.detail ? h('div.hint', { text: st.detail }) : null,
      // 健康检查失败时把"是什么、为什么、怎么办"摆出来 —— 与市场/服务卡片同一套话术。
      health.checked && !health.ok
        ? h('div.hint', { style: { color: 'var(--danger)' }, text: '检查地址：' + (health.url || '（未配置）') + ' —— ' + healthHint(health) })
        : null,
    );

    // ---- 操作 ----
    clear(actionBox);
    appendAll(actionBox, ...renderActions(res));

    // ---- 详情 ----
    // 只 push 有值的行：以前写死一串字段并打印 cur.category 这类可能为空的项，
    // 界面上就出现一堆空白/undefined 的行。
    const rows = [];
    const push = (k, v) => { if (v !== undefined && v !== null && v !== '') rows.push([k, String(v)]); };
    push('服务标识', s && s.name);
    push('类型', s && (s.kind === 'native' ? '原生（launchd / 命令）' : s.kind));
    push('分类', (s && s.category) || (mi && mi.category));
    push('端口', (s && s.port) || (mi && mi.port) || '');
    push('运行状态', st && (st.status + (st.detail ? '（' + st.detail + '）' : '')));
    push('进程 PID', st && st.pid);
    push('健康检查', health.checked ? (health.ok ? '正常' : '失败') + '：' + health.message : '');
    push('检查地址', health.url);
    push('访问地址', st && st.endpoint);
    push('配置文件', s && s.config_path);
    push('日志文件', s && s.log_path);
    push('工作目录', s && s.work_dir);
    push('启动命令', s && s.start_cmd);
    push('launchd 标签', s && s.launch_label);
    push('plist 路径', s && s.plist_path);
    push('compose 文件', s && s.compose_file);
    push('容器名', s && s.container);
    push('守护方式', mi && mi.kind);
    clear(detailBox);
    // 没有服务记录时给一句**终态**说明（不是"读取中"）：
    //   · no_daemon 的应用本来就没有守护进程 —— 说清它是命令行工具/网页入口，
    //     免得用户以为"面板没管到它"（用户 2026-09-16 反馈 ffmpeg 的那一条）；
    //   · 其余是"装了但没纳管"—— 说清纳管之后才会出现配置/日志/启停。
    const noSvcHint = (!s && mi)
      ? (mi.no_daemon
        ? '这个应用是' + (hasPanelUI(mi) || (mi.ui && mi.ui.self_conf) ? '网页入口' : '命令行工具') +
          '，本来就没有常驻进程，所以不会出现在「服务管理」里。'
        : '面板里还没有这条服务的记录（可能还没纳管）：纳管之后这里才会有配置路径、日志与启停入口。')
      : (!rows.length ? '读不到这条服务的详情。' : '');
    appendAll(detailBox,
      rows.length
        ? h('dl.kv', rows.flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })]))
        : null,
      noSvcHint ? h('div.hint', { text: noSvcHint }) : null,
      mi && mi.description
        ? h('div.hint', { style: { marginTop: '10px' }, text: mi.description })
        : null,
      mi && mi.post_install_hint
        ? h('div.hint', { style: { marginTop: '6px' }, text: '装完之后：' + mi.post_install_hint })
        : null,
    );
  }

  // 首屏渲染必须**一定**落定：resolvePanelData 内部每一步都已经自己 try/catch
  // （服务查询、凭据查询失败都只是"没有那项数据"），正常不会抛。这里再兜一层，
  // 是因为面板一旦抛出，界面上就永远停在「正在读取服务状态…」这个中间态 ——
  // 用户反馈的"卡在读取中"无论如何不能再出现。
  // 兜底渲染用"无服务记录"的形态：它会走 statusLine 的终态分支（未纳管 /
  // 命令行工具），而不是中间态。
  try {
    render(await raceDeadline(resolvePanelData(m0, svc), PANEL_QUERY_TIMEOUT_MS, '读取服务状态'));
  } catch (e) {
    render({ market: m0, svc: svc || null, name: serviceNameOf(svc || m0), cred: null });
    toast('读取服务详情失败：' + (e && e.message ? e.message : e) +
      '。面板已按"没有服务记录"渲染，可重开面板再试', 'err', 12000);
  }
  return m;
}

// forgetButton 取消纳管：只删面板记录，不动系统上的服务（managed=false 的收尾动作）。
export function forgetButton(m, onDone) {
  const name = serviceNameOf(m);
  const label = m.display_name || m.name || name;
  return h('button.btn.btn-sm', {
    text: '取消纳管',
    title: '只把这个服务从面板记录里移除，不动系统上的任何东西',
    onclick: async () => {
      if (!await confirmBox(
        '把「' + label + '」从面板记录里移除？\n\n' +
        '面板不会卸载你自己安装的软件（不跑 brew uninstall、不删文件），只是不再管它。' +
        '以后想再管它，可以用「扫描可纳管服务」重新纳管。',
        { title: '取消纳管', okText: '取消纳管' })) return;
      try {
        await api.serviceForget(name);
        toast('已取消纳管', 'ok');
        if (typeof onDone === 'function') onDone();
      } catch (e) {
        toast('取消失败：' + e.message, 'err', 9000);
      }
    },
  });
}

// uninstallButton 卸载面板托管的应用（managed=true 的收尾动作）。
export function uninstallButton(s, onDone) {
  return h('button.btn.btn-danger.btn-sm', {
    text: '🗑 卸载',
    title: '停止并删除由面板安装的服务（会先说明会做什么并要求确认）',
    onclick: async () => {
      if (!await confirmBox(
        `将卸载「${s.display_name || s.name}」。\n\n` +
        (s.kind === 'compose'
          ? '这会停止并删除容器与网络（具名数据卷会保留）。'
          : '这会卸载软件包并删除其后台服务配置。'),
        { title: '卸载服务', danger: true, okText: '确认卸载' })) return;
      // 卸载是异步任务（可能跑几分钟）：进度与结果在任务中心的进度窗里看。
      taskCenter.start({
        kind: 'uninstall', target: s.name, title: `卸载 ${s.display_name || s.name}`,
        start: () => api.serviceUninstall(s.name),
      });
      if (typeof onDone === 'function') onDone();
    },
  });
}

// marketUninstallButton 从市场卸载（面板自研安装器 / compose 应用走这里）。
// 确认框逐条列出会做什么、以及可选的"同时删除数据"路径 —— 卸载不可逆。
export function marketUninstallButton(mi, onDone) {
  const plan = mi.uninstall || {};
  const residual = !!mi.artifacts && !mi.installed;
  return h('button.btn.btn-sm.btn-danger', {
    text: residual ? '删除残留数据' : '卸载',
    disabled: !!plan.blocked && !residual,
    title: residual ? '只删除磁盘上的残留产物/数据' : (plan.blocked || '卸载「' + mi.name + '」'),
    onclick: async () => {
      const remove = h('input', { type: 'checkbox' });
      const paths = plan.data_paths || [];
      const okGo = await new Promise((resolve) => {
        modal({
          title: (residual ? '删除残留数据 · ' : '卸载 ') + mi.name,
          body: h('div', [
            residual
              ? h('div', {
                style: { marginBottom: '8px' },
                text: '这个应用当前没有安装，这一步只删除磁盘上的残留产物/数据，不可恢复。',
              })
              : h('div', { style: { marginBottom: '8px' }, text: '将执行：' }),
            residual ? null : h('ul', { style: { margin: '0 0 10px 18px', lineHeight: '1.7' } },
              (plan.steps || []).map((x) => h('li', { text: x }))),
            (!residual && plan.keep_note) ? h('div.hint', { text: '会保留：' + plan.keep_note }) : null,
            (!residual && paths.length)
              ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '10px' } },
                [remove, h('span', { text: '同时删除数据/产物（不可恢复）：' })])
              : null,
            paths.length
              ? h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
                paths.map((x) => h('li.mono', { text: x })))
              : null,
          ]),
          footer: (close) => [
            h('button.btn', { text: '取消', onclick: () => { close(); resolve(false); } }),
            h('button.btn.btn-danger', {
              text: residual ? '删除残留数据' : '确认卸载',
              onclick: () => { close(); resolve(true); },
            }),
          ],
          onClose: () => resolve(false),
        });
      });
      if (!okGo) return;
      const wipe = residual ? true : remove.checked;
      taskCenter.start({
        kind: 'uninstall', target: mi.id,
        title: (residual ? '删除残留数据 ' : '卸载 ') + mi.name,
        start: () => api.marketUninstall(mi.id, wipe),
        onDone: (task) => {
          if (task && task.status && task.status !== 'succeeded') {
            toast((residual ? '删除残留数据失败：' : '卸载失败：') + (task.error || task.status), 'err', 12000);
          } else {
            toast(residual ? '已删除「' + mi.name + '」的残留数据'
              : '已卸载「' + mi.name + '」' + (wipe ? '（含数据/产物）' : '（数据/产物已保留）'), 'ok', 9000);
          }
          if (typeof onDone === 'function') onDone();
        },
      });
    },
  });
}
