// servicePanel.js —— 一个应用 / 一个服务，**唯一**的详情面板与其动作清单。
// 三条纪律：① **不出现任何应用 ID 的 if/else**（哪颗按钮出现全由数据决定，任何 `m.id === 'frpc'` 都是回退）；
// ② **动作只有一份实现**（从 services.js 复用）；③ **市场条目与服务记录形状不同，在这里归一化**。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { panelPath } from './app.js';
import { taskCenter } from './tasks.js';
import {
  configFileModal, credentialsModal, openLogs, healthHint, loginCredsOf, appWidgets,
} from './services.js';
// 「上传与执行上限」的渲染实现只有 views.js 的 renderLimitsInto 一份；
// 2026-09-22 起它与 nginx 四页 / 配置文件 / PHP 环境一起挂在「⚙️ 调整配置」弹窗里。
import { adjustConfigModal } from './nginxpanel.js';

// PANEL_QUERY_TIMEOUT_MS 是面板首屏查询的**硬上限**。
// 没有它时请求假死会让首屏永远停在「正在读取服务状态…」（用户 2026-09-16 反馈 ffmpeg 卡住）；
// 加上后成功/失败/超时三种情况都会走到 render()，**不存在不落定的读取中**。
const PANEL_QUERY_TIMEOUT_MS = 10000;

// ---------------------------------------------------------------------------
//  归一化：把市场条目与服务记录收敛成面板需要的那几个字段
// ---------------------------------------------------------------------------

// serviceNameOf 取"面板接口认的服务名"。**顺序很重要（2026-09-16 踩过）**：市场条目的 `name` 是
// 展示名，拿它去 GET /services/<名> 会 404。所以先试 uninstall.service（**权威**）→ service_label → id。
export function serviceNameOf(m) {
  if (!m) return '';
  return m.uninstall?.service || m.service_label || m.name || m.id || '';
}

// serviceNamesOf 返回"可能要试的几个服务名"：目录的 ID/label 与真实记录名并非总是一致，
// 逐个试比在前端猜一个更便宜（猜错的代价是"面板看起来什么都不能做"）。
function serviceNamesOf(m) {
  if (!m) return [];
  const out = [];
  for (const k of [m.uninstall?.service, m.service_label, m.name, m.id]) {
    if (k && !out.includes(k)) out.push(k);
  }
  return out;
}

// svcNameCache 记住"市场条目对应哪条服务记录"；只活在内存里，失败不缓存。
const svcNameCache = new Map();

// hasPanelUI 与 apps.js 的同一判据（**必须一致**）：有 slug 且不是应用自带控制台。
export function hasPanelUI(m) {
  return !!(m && m.ui && m.ui.slug && !m.ui.console_only);
}

// ---------------------------------------------------------------------------
//  合并「我的应用」：市场条目 + 服务记录 → 一行一条（去重）
// ---------------------------------------------------------------------------

// KNOWN_LABEL_PREFIXES 是已知的 launchd 标签前缀。真机（mini，2026-09-17）上同一套 PHP
// 有两套命名（homebrew.mxcl.php@8.2 / sh.brew.php@8.2），归一化必须同时吃掉前缀与分隔符差异，
// 否则同一个服务渲染成两行，用户点哪行都不知道对不对。
const KNOWN_LABEL_PREFIXES = [
  'homebrew.mxcl.', 'homebrew-mxcl-', 'sh.brew.', 'sh-brew-',
  'cn.zizdog.', 'cn-zizdog-', 'com.zizdog.', 'com-zizdog-',
];

// appKeyOf 把一个应用/服务归一化成一个去重 key（顺序固定）：标识类字段（launch_label →
// service_label → id → uninstall.service → name）→ 小写 → 去掉一个已知前缀（只去一次）→
// 把 @ . - _ 与空白全部删掉。**绝不能只看 name**（市场条目的 name 是展示名，同一条会出现两行）。
export function appKeyOf(x) {
  if (!x) return '';
  const raw = x.launch_label || x.service_label
    || (typeof x.id === 'string' ? x.id : '')
    || (x.uninstall && x.uninstall.service)
    || x.name || '';
  let k = String(raw).trim().toLowerCase();
  for (const p of KNOWN_LABEL_PREFIXES) {
    if (k.startsWith(p)) { k = k.slice(p.length); break; }
  }
  return k.replace(/[@._\s-]/g, '');
}

// SVC_MERGE_FIELDS：只补保留那条**为空**的字段，绝不覆盖已有真实值。
const SVC_MERGE_FIELDS = [
  'port', 'launch_label', 'plist_path', 'config_path', 'log_path', 'work_dir',
  'start_cmd', 'compose_file', 'container', 'image', 'category', 'health_url',
  'display_name', 'icon', 'description',
];

function blank(v) { return v === undefined || v === null || v === '' || v === 0; }

// svcInfoScore：managed=true 最优先，其次"确实在跑"的，再看路径/命令更全的。
function svcInfoScore(s) {
  let n = 0;
  if (s.managed) n += 100;
  if (s.state && s.state.running) n += 50;
  for (const k of ['config_path', 'log_path', 'plist_path', 'work_dir', 'start_cmd', 'compose_file', 'container']) {
    if (!blank(s[k])) n += 3;
  }
  if (!blank(s.port)) n += 1;
  return n;
}

// pickAndMergeSvc：挑一条，其余记录的字段按"非空者优先、运行中优先"补进来。
function pickAndMergeSvc(pool) {
  if (!pool || !pool.length) return null;
  let best = pool[0];
  for (const s of pool.slice(1)) if (svcInfoScore(s) > svcInfoScore(best)) best = s;
  const out = { ...best };
  for (const other of pool) {
    if (other === best) continue;
    for (const k of SVC_MERGE_FIELDS) if (blank(out[k]) && !blank(other[k])) out[k] = other[k];
    const os = other.state || {};
    if (!out.state || (out.state.running !== true && os.running === true)) {
      if (other.state) out.state = other.state;
    }
    const oh = other.health || {};
    if (!out.health || (out.health.checked !== true && oh.checked === true)) {
      if (other.health) out.health = other.health;
    }
    if (other.managed && !out.managed) out.managed = true;
  }
  // 记下被合并掉的记录名：出问题时一眼能看出"另一条去哪了"（只活在内存里）。
  out.merged_from = pool.filter((s) => s !== best).map((s) => s.name);
  return out;
}

// marketRank：优先带界面/直链/文档元数据的，再优先已安装的。
function marketRank(a) {
  return (hasPanelUI(a) ? 4 : 0) + (a.port_url ? 2 : 0) + (a.docs_url ? 1 : 0) + (a.installed ? 8 : 0);
}

function entryNameOf(e) {
  return (e.svc && (e.svc.display_name || e.svc.name))
    || (e.market && e.market.name) || e.key;
}

function compareEntries(a, b) {
  const rank = (e) => {
    const st = e.svc && e.svc.state;
    if (st && st.running) return 0;
    if (st && (st.status === 'error' || st.status === 'unavailable')) return 1;
    if (e.svc) return 2;
    return 3; // 只有市场条目（无常驻进程 / 没有服务记录）
  };
  const d = rank(a) - rank(b);
  if (d) return d;
  return String(entryNameOf(a)).localeCompare(String(entryNameOf(b)), 'zh-Hans-CN');
}

// mergeAppEntries 把市场目录（只取已安装/已纳管的）与服务记录合并去重，返回 [{ key, market, svc }]；
// **两个都留着**（市场给 ui/port_url/安装器语义，服务记录给状态/绝对路径/日志），各取 rank 最高的。
export function mergeAppEntries(marketList, svcList) {
  const entries = new Map();
  const ensure = (key) => {
    if (!key) return null;
    if (!entries.has(key)) entries.set(key, { key, market: null, svc: null, _svcPool: [] });
    return entries.get(key);
  };
  for (const a of marketList || []) {
    // 未安装的条目留在「应用市场」Tab；「我的应用」只收已安装 / 已纳管的。
    if (!a || (!a.installed && !a.adopted)) continue;
    const e = ensure(appKeyOf(a));
    if (!e) continue;
    if (!e.market || marketRank(a) > marketRank(e.market)) e.market = a;
  }
  for (const s of svcList || []) {
    if (!s || !s.name) continue;
    const e = ensure(appKeyOf(s));
    if (e) e._svcPool.push(s);
  }
  for (const e of entries.values()) {
    e.svc = pickAndMergeSvc(e._svcPool);
    delete e._svcPool;
  }
  return [...entries.values()].sort(compareEntries);
}

// dedupeMarketEntries 按归一化 key 去重（保留 marketRank 最高的）：市场列表本身也可能
// 出现同一应用的两条目录条目，渲染前统一收敛。
export function dedupeMarketEntries(list) {
  const byKey = new Map();
  for (const a of list || []) {
    if (!a) continue;
    const k = appKeyOf(a);
    if (!k) continue;
    const cur = byKey.get(k);
    if (!cur || marketRank(a) > marketRank(cur)) byKey.set(k, a);
  }
  return [...byKey.values()];
}

// portDirectURL 用**当前访问面板的主机名** + 服务端口拼直链（后端不返回局域网 IP）。
// 拼出来的通常可用，但服务可能只监听 127.0.0.1 或根本不是 HTTP 界面 —— 调用方必须在 title
// 里如实写上这层不确定性。协议固定 http://（面板自己可能是 https，应用端口不是）。
export function portDirectURL(port) {
  const p = Number(port) || 0;
  if (!p) return '';
  const host = (typeof location !== 'undefined' && location.hostname) || '';
  if (!host) return '';
  return 'http://' + host + ':' + p + '/';
}

// subpathWarning 把"子路径实测不可用"渲染成按钮下方一行**始终可见**的小字：
// 只在按钮上加 title 不够（不悬浮就看不到，用户以为按钮坏了，2026-09-17 第四条抱怨）。
export function subpathWarning(m) {
  if (!hasPanelUI(m)) return null;
  const ui = m.ui || {};
  if (!ui.prefer_direct) return null;
  const note = String(ui.note || '').trim() || '该应用实测不能挂在子路径下';
  return h('div', {
    style: { flexBasis: '100%', fontSize: '11.5px', lineHeight: '1.6', color: 'var(--warn, #fbbf24)' },
    title: '子路径入口 /' + (ui.slug || '') + '/ —— ' + note,
    text: '⚠️ 该应用不支持子路径：' + note + '；请用「直链」',
  });
}

// ---------------------------------------------------------------------------
//  卡片外壳：四个 Tab（已安装 / 应用市场 / docker / 一键建站）共用同一套 DOM
// ---------------------------------------------------------------------------
// 用户 2026-09-17 要求恢复卡片网格样式；四个 Tab 共用这一个函数（结构/pill/按钮排布只有一份），
// 调用方只负责折算 props。
export function appCardShell({
  icon, name, subtitle, subtitleTitle = '', pills = [], text = '', textTitle = '',
  extra = [], actions = [], warning = null, dataset = null,
}) {
  return h('div', {
    dataset: dataset || undefined,
    style: {
      background: 'var(--panel-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius)',
      padding: '15px 16px', display: 'flex', flexDirection: 'column', gap: '10px',
    },
  }, [
    h('div', { style: { display: 'flex', gap: '10px', alignItems: 'flex-start' } }, [
      h('div', {
        style: {
          width: '38px', height: '38px', borderRadius: '10px', display: 'grid', placeItems: 'center',
          background: 'var(--panel)', fontSize: '20px', flex: '0 0 auto',
        },
        text: icon || '🧩',
      }),
      h('div', { style: { flex: 1, minWidth: 0 } }, [
        h('div', { style: { fontWeight: '620', fontSize: '14px' }, text: name }),
        subtitle ? h('div', {
          style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '2px' },
          title: subtitleTitle || '', text: subtitle,
        }) : null,
      ]),
    ]),
    pills.length ? h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, pills) : null,
    // 卡片只放一段说明；完整描述仍在 title 与「⚙️ 管理」里。
    text ? h('div', {
      style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.55' },
      title: textTitle || '', text,
    }) : null,
    ...extra,
    h('div', { style: { display: 'flex', gap: '6px', marginTop: 'auto', paddingTop: '4px', flexWrap: 'wrap' } },
      actions.filter(Boolean)),
    warning,
  ]);
}

// openTargetOf 计算卡片上的「打开」按钮指向哪里（用户 2026-09-17 第六条的唯一判定）：有 ui.slug →
// "/<slug>/"；否则端口直连（优先 port_url）；两者都没有 → 没有入口。console_only **不给**任何入口；
// self_conf（phpMyAdmin）必须走 panelPath（相对路径会打到 SPA 回落页）。返回 { href, subpath, port } 或 null。
export function openTargetOf(m, opts = {}) {
  const svc = opts.svc || null;
  const ui = (m && m.ui) || null;
  const slug = (ui && ui.slug) || '';
  if (ui && ui.console_only) return null;
  const port = (svc && svc.port) || (m && m.port) || 0;
  const explicit = (m && m.port_url) || '';
  const direct = explicit || portDirectURL(port);
  if (ui && ui.self_conf && slug) {
    const href = panelPath(slug + '/');
    return { href, subpath: true, port, direct, title: '经面板打开（需先登录面板）：' + href };
  }
  if (hasPanelUI(m) && !(ui && ui.prefer_direct)) {
    return { href: '/' + slug + '/', subpath: true, port, direct, title: '经面板的 /' + slug + '/ 打开' };
  }
  if (!direct) return null;
  return {
    href: direct, subpath: false, port, direct,
    title: '该应用不支持子路径，直接访问它的端口：' + direct,
  };
}

// openOnlyAction 渲染卡片上**唯一**的「打开」按钮（用户 2026-09-17："只显示打开，不显示直链"）。
// 没有可用入口时返回空数组，绝不给出点开必然打不开的按钮。
export function openOnlyAction(m, opts = {}) {
  const t = openTargetOf(m, opts);
  if (!t) return [];
  return [h('a.btn.btn-sm.btn-primary', {
    href: t.href, target: '_blank', rel: 'noopener', text: '打开', title: t.title,
  })];
}

// PORT_ACCESS_WARNING 是用户要求的**逐字**提示（不要改写、不要加前后缀）。
export const PORT_ACCESS_WARNING = '该应用不支持子路径，请用端口访问，或自行配置反代。';

// portAccessWarning：只在「打开」真的指向端口直连时给一行**始终可见**的小字；
// 支持子路径或没有入口的卡片不给。管理面板里用 subpathWarning（带 ui.note 原文）。
export function portAccessWarning(m, opts = {}) {
  const t = openTargetOf(m, opts);
  if (!t || t.subpath) return null;
  return h('div', {
    style: { flexBasis: '100%', fontSize: '11.5px', lineHeight: '1.6', color: 'var(--warn, #fbbf24)' },
    title: '打开地址是 ' + t.href,
    text: PORT_ACCESS_WARNING,
  });
}

// vendorImage 取 Docker 目录条目里的镜像名（在面板里显示"官方镜像是什么"）。
function vendorImage(m) {
  const yaml = (m && m.compose_yaml) || '';
  const mm = yaml.match(/^\s*image:\s*(\S+)/m);
  return mm ? mm[1] : '';
}

// resolvePanelData 把调用方给的东西补全成"面板能完整渲染"的数据。
async function resolvePanelData(market, svc) {
  const m = market || null;
  let s = svc || null;
  let name = serviceNameOf(s) || serviceNameOf(m);

  // 只给市场条目时必须补查服务记录（绝对路径/managed/日志路径只有它才有）；no_daemon 的应用
  // **不去查**（省掉注定 404 的请求）。必须先判 m，否则 TypeError 会把整次渲染推进兜底分支。
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
  // 凭据接口返回**登录用**凭据才给「🔑 查看凭据」：先查一次，让"按钮存在"等于"点开有东西看"
  // （给一颗点开是空弹窗的按钮正是本项目反复被投诉的误导）。
  let cred = null;
  const credName = s ? serviceNameOf(s) : '';
  // 只在**可能真有凭据**时才问（应用声明了配置文件路径）；否则每次开无配置文件的应用都打一条
  // 404，浏览器控制台留错误，UI 门禁把控制台错误当失败（2026-09-18 smoke 因此变红）。
  const mayHaveCreds = !!(m && m.config_path);
  if (credName && mayHaveCreds) {
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
// 市场卡片**没有** state（不能在渲染时逐张请求），于是退化成"启动"——后端对已在跑的服务
// 执行 start 是幂等的，不会谎报；点「⟳ 刷新」即可确认。
export function serviceActions(m, { onDone } = {}) {
  const name = serviceNameOf(m);
  const st = (m && m.state) || {};
  const canControl = !m || m.driver_ready !== false;
  const label = (m && (m.display_name || m.name)) || name || '这个服务';
  const btn = (text, action, cls = '', title = '') => h('button.btn.btn-sm' + cls, {
    text,
    // driver 没就绪时禁用：点下去必然报错的按钮等于误导。
    disabled: !canControl || !name,
    title: title || `${text}「${label}」`,
    onclick: () => doServiceAction(name, action, m, onDone),
  });
  return [
    st.running === true
      ? btn('停止', 'stop')
      : btn('启动', 'start', '.btn-ok'),
    btn('重启', 'restart'),
    // 状态刷新不是"动作"：只重新查一次，不改动服务。
    btn('⟳ 刷新', 'status', '', '重新查询它在系统里的真实状态（不改动服务）'),
  ];
}

// doServiceAction 执行一次服务动作并如实报告结果。
// **唯一**的启停实现（市场卡片、服务管理卡片、详情面板都走它）。
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
  // 后端说"操作完成但结果未被确认"时必须原样告诉用户，不能报一个干净的"已启动"。
  if (!failed && warning) toast(`「${label}」已请求${labels[action] || action}：${warning}`, 'warn', 14000);
  else if (!failed) toast(`「${label}」已${labels[action] || action}`, 'ok');
  // 不论成败都刷新：界面必须显示**系统里的真实状态**而不是我们的预期（launchctl 谎报过）。
  if (typeof onDone === 'function') onDone();
}

// marketQuickActions 给应用市场卡片 / 「我的应用」行的那一排常用动作（启停/重启/刷新 + 管理）；
// 编辑配置/日志/凭据/重装/卸载都在管理面板里，卡片上**只有这一颗**"进面板"的按钮。
// opts.state 有值首颗按钮才对；opts.onManage 由调用方接管开面板；opts.svc 有记录时以记录为准。
export function marketQuickActions(m, { state, onDone, onManage, svc = null } = {}) {
  const s = svc || null;
  const installed = !!(m && (m.installed || m.adopted)) || !!s;
  if (!installed) return [];
  // 启停按钮只在"**服务确实可管**"时给：有服务记录 / adopted / 服务已在 launchd 里。
  // 孤儿态（plist 丢了）与 no_daemon 命令行工具点启停只会报"找不到服务"，它们该用的按钮在
  // 「⚙️ 管理」里。
  const canControl = !!s || !!(m && (m.adopted || m.service_in_launchd));
  const target = s || (state ? { ...m, state } : m);
  return [
    ...(canControl ? serviceActions(target, { onDone }) : []),
    h('button.btn.btn-sm', {
      text: '⚙️ 管理',
      title: '状态 / 启停 / 配置 / 日志 / 凭据 / 重装 / 文档 / 卸载 —— 都在这里，不用跳到别的页面',
      onclick: () => (typeof onManage === 'function' ? onManage() : openServicePanel({ market: m, svc: s, onDone })),
    }),
  ];
}

// ---------------------------------------------------------------------------
//  详情面板
// ---------------------------------------------------------------------------

function pill(cls, text, title) {
  return h('span.pill' + (cls ? '.' + cls : ''), { text, title: title || '' });
}

// raceDeadline 给 promise 加硬上限；定时器在落定后清掉，不留悬挂 timer。
function raceDeadline(p, ms, label) {
  let timer;
  const deadline = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(label + '超过 ' + ms + 'ms 没有返回')), ms);
  });
  return Promise.race([p, deadline]).finally(() => clearTimeout(timer));
}

// statusLine 把服务状态翻译成"一眼能看懂"的一行。
// ⚠️ 铁律（2026-09-21 用户点名）：**只有真的有服务记录/launchd 作业且它报 stopped 时才许写「已停止」**；
// no_daemon 与"装了但无记录"的应用都**不是**"已停止"。st 为空不是"还在读"，而是"根本没有服务记录"。
export function statusLine(st, m, s = null) {
  if (st) {
    if (st.running) return { cls: 'ok', text: '运行中', title: st.detail || '' };
    if (st.status === 'error') return { cls: 'danger', text: '异常', title: st.detail || '' };
    if (st.status === 'unavailable') return { cls: 'warn', text: '环境不可用', title: st.detail || '' };
    if (st.status === 'not-installed') return { cls: 'warn', text: '未安装', title: st.detail || '' };
    if (st.status === 'unknown') return { cls: '', text: '未知', title: st.detail || '' };
    // 有记录、没在跑、也没报错 → 这才是真的「已停止」；no_daemon 万一有记录也要按实说。
    if (m && m.no_daemon) return noDaemonLine(m);
    return { cls: '', text: '已停止', title: st.detail || '服务记录报的是停止状态，可以在这里启动它' };
  }
  if (m && m.no_daemon) return noDaemonLine(m);
  if (m && isInstalledMarketItem(m)) {
    // 装了但面板里没有记录（例如本机 nginx 在 :80 上跑着只是没登记）：**绝不能**写「已停止」。
    return {
      cls: '',
      text: '已安装（面板里暂无记录）',
      title: '这个应用装在机器上，但面板里还没有它的服务记录 —— 所以这里既不能说它在跑、'
        + '也不能说它停了。要能看到状态、启停、改配置，用工具栏的「+ 注册服务」把它加进来。'
        + portCheckNote(m, s),
    };
  }
  return {
    cls: '',
    text: '面板里暂无记录',
    title: '面板里还没有这条服务的记录，所以拿不到配置文件路径与日志。'
      + '可以在「已安装」工具栏用「+ 注册服务」把本机已有的服务加进来',
  };
}

// noDaemonLine 是"没有常驻进程"的应用的终态文案。no_daemon 只说明没有常驻进程，**不等于**
// "是网页入口"：phpMyAdmin 是网页入口、ffmpeg 是命令行工具，按**有没有界面**分开说，
// 并且一律不出现「已停止」。
function noDaemonLine(m) {
  const webUI = hasPanelUI(m) || !!(m.ui && m.ui.self_conf);
  return webUI
    ? { cls: '', text: '已安装（网页入口，无常驻进程）',
      title: '这个应用没有守护进程：装完就是一个网页入口，面板里不会有常驻服务记录，也就没有"运行/停止"这回事' }
    : { cls: '', text: '已安装（命令行工具，无常驻进程）',
      title: '这个应用是命令行工具：没有守护进程、也没有网页界面，供面板或其它应用在后台调用，没有"运行/停止"这回事' };
}

// isInstalledMarketItem 判断"这条市场条目确实装在机器上"（后端给了 installed 或纳管记录）。
function isInstalledMarketItem(m) {
  return !!(m && (m.installed || m.adopted));
}

// portCheckNote 只在**后端真的做过检查**时才给结论（通过/没通过+地址/没查过写"未检测"）：
// 前端自己探测会撞混合内容/跨域，失败不能证明端口没在听。导出给 services.js 共用。
export function portCheckNote(m, s = null) {
  const h = (s && s.health) || (m && m.health) || null;
  const port = Number((m && (m.entry_port || m.port)) || (s && s.port) || 0);
  if (h && h.checked) {
    const at = h.url ? '（' + h.url + '）' : '';
    if (h.ok) return '端口在监听' + at + '：面板刚检查过，能连上';
    return '端口没在监听' + at + '：面板刚检查过，连不上' + (port ? '（端口 ' + port + '）' : '');
  }
  return port > 0
    ? '端口 ' + port + ' 是否在监听：未检测（面板还没有这条应用的检查地址）'
    : '没有可用端口，无从检测它是否在运行';
}

// openDirectActions 渲染「打开 / 直链」这一对入口 —— **唯一的一份实现**（三处调用方必须一致）。
// 语义固定（2026-09-17）：「打开」**永远**是子路径、「直链」**永远**是端口地址；prefer_direct 时
// 「打开」**不换成**直连，只加 ⚠️ 与 title；不看 /market/proxies 探测（不带会话，几乎恒 false）。
export function openDirectActions(m, opts = {}) {
  const svc = opts.svc || null;
  const explain = !!opts.explain;
  const ui = (m && m.ui) || null;
  const slug = (ui && ui.slug) || '';
  const panelUI = !!(ui && ui.slug && !ui.console_only);
  const port = (svc && svc.port) || (m && m.port) || 0;
  const explicit = (m && m.port_url) || '';
  const direct = explicit || portDirectURL(port);
  const synthesized = !explicit && !!direct;
  const note = (ui && ui.note) || '';

  const directButton = () => h('a.btn.btn-sm', {
    href: direct, target: '_blank', rel: 'noopener', text: '直链',
    title: synthesized
      ? '按当前访问地址与端口拼出来的：' + direct +
        '（面板拿不到局域网 IP，也不保证这个端口就是网页界面，可能不适用）'
      : '绕过面板、直接访问应用自己的端口：' + direct,
  });

  // console_only（frpc 自带控制台）：两个入口都不给；只有市场条目才带 ui。
  if (m && ui && ui.console_only) return [];

  // 没有面板界面：见函数头的说明。
  if (!panelUI) {
    if (svc && direct) return [directButton()];
    if (explain) {
      return [h('span', {
        style: { fontSize: '11.5px', color: 'var(--text-mute)' },
        title: m
          ? '这个应用在目录里没有界面（没有 ui.slug），也没有可用的端口，拼不出打开地址'
          : '这条服务记录没有端口，也没有面板界面，拼不出打开地址',
        text: '没有可用的打开入口',
      })];
    }
    return [];
  }

  // SelfConf（phpMyAdmin）：nginx location 只允许本机，唯一入口是**面板自己**那条；
  // 相对路径会打到面板 SPA 的回落页（200 却是面板首页），所以走 panelPath。
  if (ui.self_conf) {
    const out = [h('a.btn.btn-sm.btn-primary', {
      href: panelPath((slug || 'phpmyadmin') + '/'), target: '_blank', rel: 'noopener', text: '打开',
      title: '经面板打开（需先登录面板）' +
        (direct ? '；也可以直连：' + direct
          : '；这个应用只能从面板打开，没有可直连的端口，所以没有「直链」'),
    })];
    if (direct) out.push(directButton());
    return out;
  }

  const path = '/' + slug + '/';
  // prefer_direct 是**人工实测**结论（自动探测发现不了"资源全 200、前端路由不认前缀"）：
  // 只加警示，不改按钮归属。
  const warn = !!ui.prefer_direct;
  const why = warn ? (note || '这个应用实测不支持子路径') : note;
  const out = [h('a.btn.btn-sm.btn-primary', {
    href: path, target: '_blank', rel: 'noopener',
    text: warn ? '⚠️ 打开' : '打开',
    title: '经面板的 /' + slug + '/ 打开' +
      (why ? '。' + why : '') +
      (warn ? '。点开可能是空白页，请用旁边的「直链」' : '') +
      (direct ? '' : '。没有可用的端口直连地址，所以没有「直链」'),
  })];
  if (direct) out.push(directButton());
  return out;
}

// reinstallButton 把「重装」放进**管理面板**（用户 2026-09-17）。安装器住在 apps.js，调用方通过
// `onReinstall` 把现有重装动作递进来；只在**真的可重装**时给（卸载计划是 installer/service 两类）。
export function reinstallButton(m, { onReinstall, onDone } = {}) {
  const kind = m && m.uninstall && m.uninstall.kind;
  if (!m || !m.id || (kind !== 'installer' && kind !== 'service')) return null;
  const label = m.name || m.id;
  return h('button.btn.btn-sm', {
    text: '重装',
    title: '重新跑一遍安装（会复用已下载的产物、保留数据，不会重复下载）',
    onclick: async () => {
      if (!await confirmBox(
        '重装「' + label + '」？\n\n' +
        '· 会重新跑一遍安装流程（下载 / 解压 / 重建服务定义）\n' +
        '· 已下载的产物会复用，不会重复下载\n' +
        '· 已存在的配置与数据保留\n' +
        '· 服务会重启一次',
        { title: '重装 ' + label, okText: '开始重装' })) return;
      if (typeof onReinstall === 'function') { onReinstall(); return; }
      taskCenter.start({
        kind: 'install', target: m.id, title: '重装 ' + label,
        start: () => api.marketInstall(m.id),
        onDone: () => { if (typeof onDone === 'function') onDone(); },
      });
    },
  });
}

/**
 * openServicePanel 打开「应用管理」面板 —— 市场卡片与服务管理**共用的唯一入口**。
 *
 * @param {object}  o
 * @param {object} [o.market]  市场条目（api.market 的一项）
 * @param {object} [o.svc]     服务记录（api.service / services 列表的一项）
 * @param {function}[o.onDone]  动作完成后刷新调用方（市场卡片 / 服务卡片）
 * @param {function}[o.onReinstall] 调用方那套"现有重装动作"（apps.js 的安装器）；
 *                                  不传时面板退回通用后端安装接口（见 reinstallButton）
 * @param {string} [o.primaryText] 面板里的第一颗按钮（"添加到面板"/"安装"这类**状态相关**动作）；
 *                                 文案与行为由调用方决定，面板只负责把它放在同一屏里。
 * @param {function}[o.primaryRun]
 * @returns {Promise<object>} modal 句柄
 */
export async function openServicePanel(o = {}) {
  const { market, svc, onDone, onReinstall, primaryText, primaryRun } = o;
  const m0 = market || null;
  const title = (svc && svc.display_name) || (m0 && m0.name) || '应用管理';

  const statusBox = h('div', [h('div.hint', { text: '正在读取服务状态…' })]);
  const actionBox = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } });
  const detailBox = h('div', [h('div.hint', { text: '正在读取服务详情…' })]);

  const m = modal({
    // 标题 2026-09-16 改成「应用管理」：市场上通往它的按钮只剩「⚙️ 管理」，标题跟着按钮走。
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

  // renderActions 生成面板里的全部按钮，顺序按用户的使用顺序：
  // ① 状态动作（调用方给）② 启停 ③ 界面 ④ 配置 ⑤ 凭据 ⑥ 日志 ⑦ 应用功能 ⑧ 收尾。
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
    // 只在"服务确实可管"时才给：装了但没有服务的应用点启停只会报"找不到服务"，
    // 反复被投诉的正是"点了没用的按钮比没有按钮更糟"。
    const canControl = !!s || !!(mi && (mi.adopted || mi.service_in_launchd));
    if (canControl) out.push(...serviceActions(s || mi, { onDone: afterAction }));

    // ③ 界面：与市场卡片同一份实现（openDirectActions）；console_only 不渲染。
    // 纯纳管服务没有 port_url，用 location.hostname + 端口拼一颗直链并在 title 里说明；
    // 两者都没有时 explain 会渲染一行"为什么没有"。
    out.push(...openDirectActions(mi, { svc: s, explain: true }));

    // ③b 重装：卡片上不再直接给，统一收进管理面板。
    const rb = reinstallButton(mi, { onReinstall, onDone: afterAction });
    if (rb) out.push(rb);

    // ④ 配置：判据是"面板知不知道配置文件的**绝对路径**"，不是"有没有服务记录"。
    // 2026-09-18 报障："php 和 nginx 的编辑配置文件都是灰色的" —— nginx 可能装着、在跑、
    // 但面板里没有记录。配置路径是**目录的静态属性**，与归不归面板管无关，所以一律可点。
    const cfgPath = (s && s.config_path) || (mi && mi.config_path_abs) || '';
    if (cfgPath) {
      // 没有服务记录时给最小上下文：编辑器只要路径与显示名。
      const cfgCtx = s || { config_path: cfgPath, display_name: (mi && mi.name) || cfgPath };
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        title: (mi && mi.post_install_hint ? '要改什么：' + mi.post_install_hint + ' —— ' : '') +
          '直接编辑 ' + cfgPath + '（保存后需重启服务才生效）',
        onclick: () => configFileModal(cfgCtx, afterAction),
      }));
    } else if (mi && mi.config_path) {
      // 只写了文件名、算不出绝对路径时如实说"面板不知道它在哪"，不灰掉入口。
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        title: '面板不知道这个应用的配置文件在哪（目录里写的是 ' + mi.config_path +
          '，解析不出绝对路径）。可以到「文件管理」里按路径找到它直接编辑。',
        onclick: () => toast('这个应用的配置文件路径面板解析不出来（目录里写的是 ' + mi.config_path +
          '）；可以到「文件管理」里手动找到它', 'warn', 12000),
      }));
    }

    // ④b 常用设置（用户 2026-09-18："常用更改应该做成功能，而不是让用户编辑配置原文件"）：
    // nginx/PHP 最常改的两件事（一次能传多大、脚本能跑多久）给一颗直达按钮，打开的是与面板设置
    // **同一份**编辑器。nginx 专属：宝塔式「⚙️ 调整配置」已把原来两颗重复按钮合并成一颗。
    if (isNginxApp(mi)) {
      out.push(h('button.btn.btn-sm', {
        text: '⚙️ 调整配置',
        title: '打开统一配置面板：nginx（服务 / 性能调整（含最大上传大小）/ 配置修改 / 错误日志）、'
          + '上传与执行上限、配置文件清单、PHP 环境',
        onclick: () => adjustConfigModal(),
      }));
    } else if (isLimitTunable(mi)) {
      // PHP 条目：同一份实现（views.js 的 renderLimitsInto），直接落到那一页。
      out.push(h('button.btn.btn-sm', {
        text: '⚙️ 调整配置',
        title: '直接打开「上传与执行上限」：改「一次能传多大」「脚本能跑多久」'
          + '（改完自动重载 nginx、重启 php-fpm，并回读生效值）—— 不需要编辑配置文件',
        onclick: () => adjustConfigModal({ page: 'limits' }),
      }));
    }

    // ⑤ 凭据：接口返回了**登录用**凭据才给按钮；按钮存在 = 点开一定有东西看。
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

    // ⑦ 应用自己的功能入口（调用密钥 / 各来源 / 模型…）：哪几颗住在 services.js 的
    // appWidgets 注册表里，这里只负责摆在同一排。
    if (s) out.push(...appWidgets(s, m));

    // ⑧ 收尾：managed=false 只给「从列表移除（不卸载软件）」，managed=true 才给「卸载」。
    // 2026-09 真机缺陷：compose 运行时被删后记录只剩「卸载」而卸载必然失败 → 记录删不掉；
    // 现在任何托管记录都额外给"只删记录"出口。文案全部是用户语言，但**判据与行为一个字没改**。
    if (s) {
      const rt = runtimeDownOf(s);
      const pk = (mi && mi.uninstall && mi.uninstall.kind) || '';
      if (pk === 'brew' || pk === 'installer' || pk === 'service') {
        // 计划说得清"怎么真卸载" → 收尾只有这一颗主动作。2026-09-21 用户要求不要再并排
        // 摆一颗"只删记录"（"移除却不卸载是什么意思 …… 让用户看不到却持续运行"）。
        out.push(marketUninstallButton(mi, afterAction, s));
      } else if (s.managed) {
        // 面板托管但目录里没有卸载计划（下架条目等）：仍走通用卸载，不摆"只删记录"。
        out.push(uninstallButton(s, afterAction));
      } else {
        // 面板**不认识**这条服务：唯一诚实的收尾是"停掉 + 从记录移除"，并逐字说明软件仍在磁盘上。
        out.push(forgetButton(s, afterAction, { runtimeDown: rt.down, reason: rt.reason }));
      }
    } else if (mi) {
      if (mi.uninstall?.kind === 'forget') {
        out.push(forgetButton(mi, afterAction));
      } else if (mi.uninstall?.kind === 'installer' || mi.uninstall?.kind === 'service'
        || mi.uninstall?.kind === 'brew') {
        out.push(marketUninstallButton(mi, afterAction));
      }
    }
    if (mi && mi.docs_url) out.push(docLink(mi.docs_url));
    return out;
  }

  function docLink(url) {
    return h('a.btn.btn-sm', { href: url, target: '_blank', rel: 'noopener', text: '文档' });
  }

  // afterAction 重新拉数据并原地重画：用户不必关掉面板再点开。
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
    // 把市场条目也传进去：没有服务记录时要靠它区分 no_daemon 与"装了但还没登记"。
    const line = statusLine(st, mi, s);
    clear(statusBox);
    appendAll(statusBox,
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center', marginBottom: '8px' } }, [
        h('div', { style: { fontWeight: '620', fontSize: '14px', marginRight: '4px' }, text: displayName }),
        pill(line.cls, line.text, line.title),
        ((s && s.port) || (mi && mi.port)) > 0 ? pill('', ':' + ((s && s.port) || mi.port)) : null,
        // 「面板托管 / 仅纳管」pill 已删除（用户 2026-09-21："弱化管纳这个概念"）；不能换成
        // "由面板安装/本机已有" —— managed=false 并不等于"软件是用户装的"，照记录写就是说假话。
        // 「面板里没有服务记录」只对"本该有服务却查不到"的应用说，no_daemon 报这句是误导。
        (!s && mi && (mi.installed || mi.adopted) && !mi.no_daemon)
          ? pill('warn', '面板里没有服务记录',
            '这个应用装在机器上，但面板里还没有对应的服务记录，所以拿不到配置文件路径与日志')
          : null,
        health.checked
          ? (health.ok
            ? pill('ok', '健康', health.message + '（' + (health.latency_ms || 0) + 'ms）')
            : pill('danger', '健康检查失败', health.message))
          : null,
        s && s.driver_error ? pill('warn', '驱动不可用', s.driver_error) : null,
        // 容器类应用：标出目录里的官方镜像，给用户一个"我这版是不是旧的"的参照。
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
    // prefer_direct 时按钮下方补一行**始终可见**的说明（与「我的应用」行、市场卡片同一份）。
    appendAll(actionBox, subpathWarning(mi));

    // ---- 详情 ----
    // 只 push 有值的行：以前写死一串字段并打印可能为空的项，界面上出现一堆空白行。
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
    // 没有服务记录时给一句**终态**说明（不是"读取中"）：no_daemon 说清它是命令行工具/网页入口；
    // 其余说清加进来之后才会有配置/日志/启停（入口：工具栏「+ 注册服务」）。
    const noSvcHint = (!s && mi)
      ? (mi.no_daemon
        ? '这个应用是' + (hasPanelUI(mi) || (mi.ui && mi.ui.self_conf) ? '网页入口' : '命令行工具') +
          '，本来就没有常驻进程，所以不会出现在「已安装」的服务清单里。'
        : '面板里还没有这条服务的记录，所以看不到配置路径与日志。'
          + '可以在「已安装」工具栏用「+ 注册服务」把本机已有的服务加进来，之后这里就会有配置、日志与启停入口。')
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

  // 首屏渲染必须**一定**落定：resolvePanelData 内部每步都 try/catch，这里再兜一层 ——
  // 一旦抛出，界面就永远停在「正在读取服务状态…」。兜底渲染用"无服务记录"的形态。
  try {
    render(await raceDeadline(resolvePanelData(m0, svc), PANEL_QUERY_TIMEOUT_MS, '读取服务状态'));
  } catch (e) {
    render({ market: m0, svc: svc || null, name: serviceNameOf(svc || m0), cred: null });
    toast('读取服务详情失败：' + (e && e.message ? e.message : e) +
      '。面板已按"没有服务记录"渲染，可重开面板再试', 'err', 12000);
  }
  return m;
}

// runtimeDownOf 判断"这条记录的运行时现在还可用吗"。用户删掉 Colima/Docker 后，compose 记录的
// 驱动直接不可用（后端把原因放在 driver_error、状态 unavailable），此时「卸载」永远失败；
// 判据只认后端给的真实字段，不猜。
function runtimeDownOf(s) {
  const reason = (s && s.driver_error) || '';
  const status = String((s && s.state && s.state.status) || '');
  return { down: !!reason || status === 'unavailable', reason: reason || '运行时不可用' };
}

// forgetRecord 走"只删记录"接口（后端不触碰系统）。**404 必须如实降级**：旧面板没有这个能力时
// 明说"该版本面板不支持，请升级"。提示语一律说用户能观察到的后果，不说"已删除面板记录"。
async function forgetRecord(name, label, opts = {}) {
  try {
    const res = await api.serviceForget(name);
    const stopped = !!(res && res.stopped);
    toast(stopped
      ? '已停止这个服务并从面板移除。软件仍在磁盘上，需要你自己卸载。'
      : '已从面板移除（它当时不在运行，没有留下后台进程）。软件仍在磁盘上，需要你自己卸载。',
    'ok', 14000);
    if (typeof opts.onDone === 'function') opts.onDone();
    return true;
  } catch (e) {
    const status = e && e.status;
    if (status === 404 || status === 405) {
      toast('该版本面板不支持"从面板移除"（' + ((e && e.message) || status) + '），请升级面板后再试', 'err', 15000);
    } else {
      // 409 = 停不掉且仍在运行：面板**拒绝**移除，否则就成了"隐身运行"。
      toast('没有从面板移除：' + ((e && e.message) || e), 'err', 15000);
    }
    return false;
  }
}

// recordOnlyModal 是"卸载失败"时的兜底对话框：说清原因，并给一个**可直接点**的
// 「从面板移除该服务」出口（不能只丢一句 toast 就完了）。这条出口会先停服务，
// 停不掉且仍在运行时会拒绝移除。
function recordOnlyModal(opts) {
  const { label, name, error, runtimeDown, reason, onDone } = opts;
  const bodyLines = [
    h('div', { style: { marginBottom: '8px' }, text: '卸载「' + label + '」没有成功：' + error }),
  ];
  if (runtimeDown) {
    bodyLines.push(h('div', { text: '它的运行时不可用（' + (reason || '运行时不可用') + '），所以卸载做不到。' }));
    bodyLines.push(h('div.hint', { text: '这一步只把这条记录从面板移除；容器与磁盘数据不会被删（运行时不可用时也没有容器在跑）。' }));
  } else {
    bodyLines.push(h('div', { text: '可以把这条服务从面板移除 —— 面板不认识它、也不知道该怎么卸载它。' }));
    bodyLines.push(h('div.hint', { text: '面板会先停止这个服务，然后只删掉记录；软件仍留在磁盘上，需要你自己卸载。' }));
  }
  modal({
    title: '从面板移除 · ' + label,
    body: h('div', bodyLines),
    footer: (close) => [
      h('button.btn', { text: '关闭', onclick: close }),
      h('button.btn.btn-primary', {
        text: '从面板移除该服务',
        onclick: async () => { close(); await forgetRecord(name, label, { runtimeDown, reason, onDone }); },
      }),
    ],
  });
}

// forgetButton 「从面板移除该服务」：面板不认识这条服务时的唯一收尾动作（用户 2026-09-21：
// "让用户看不到却持续运行"）。目录里的应用**不再**出现它；只删记录也**必须先停服务**（会 409 拒绝）。
export function forgetButton(m, onDone, opts = {}) {
  const name = serviceNameOf(m);
  const label = m.display_name || m.name || name;
  const runtimeDown = !!opts.runtimeDown;
  const reason = opts.reason || '运行时不可用';
  return h('button.btn.btn-sm', {
    text: '从面板移除该服务',
    title: runtimeDown
      ? '这个服务面板不认识、也不知道怎么卸载。会把这条记录从面板移除（软件仍在磁盘上，需要你自己卸载）'
      : '这个服务面板不认识、也不知道怎么卸载。会先停止它，再把记录从面板移除（软件仍在磁盘上，需要你自己卸载）',
    onclick: async () => {
      const msg = '把「' + label + '」从面板移除？\n\n'
        + (runtimeDown
          ? '运行时不可用（' + reason + '），现在没有容器在跑。\n'
          : '面板会**先停止**这个服务（不会留下还在后台运行的隐身服务），\n')
        + '然后只删掉面板里的这条记录 —— 软件本身仍在磁盘上、需要你自己卸载。\n'
        + (runtimeDown ? '' : '想再加回来，可以用「已安装」工具栏的「+ 注册服务」。');
      if (!await confirmBox(msg, {
        title: '从面板移除该服务',
        okText: '确认移除',
      })) return;
      await forgetRecord(name, label, { runtimeDown, reason, onDone });
    },
  });
}

// uninstallButton 卸载面板托管的应用（managed=true 的收尾动作）。
export function uninstallButton(s, onDone) {
  const label = (s && (s.display_name || s.name)) || '这个服务';
  return h('button.btn.btn-danger.btn-sm', {
    text: '🗑 卸载',
    title: '停止并删除由面板安装的服务（会先说明会做什么并要求确认）',
    onclick: async () => {
      try {
        if (!await confirmBox(
          `将卸载「${label}」。\n\n` +
          (s.kind === 'compose'
            ? '这会停止并删除容器与网络（具名数据卷会保留）。'
            : '这会卸载软件包并删除其后台服务配置。'),
          { title: '卸载服务', danger: true, okText: '确认卸载' })) return;
        // 卸载是异步任务，进度在任务中心的进度窗里。必须 await 这次提交：提交没成功时
        // taskCenter.start 会弹**带原因**的 toast 并返回 null，那种情况下不能触发 onDone、更不能沉默
        // （写操作失败必须可见，这是坑 154 的教训）。卸载失败必须给出下一步。
        const rt = runtimeDownOf(s);
        const taskId = await taskCenter.start({
          kind: 'uninstall', target: s.name, title: `卸载 ${label}`,
          start: () => api.serviceUninstall(s.name),
          onDone: (task) => {
            if (task && task.status && task.status !== 'succeeded') {
              const r2 = runtimeDownOf(s);
              recordOnlyModal({
                label, name: s.name, error: task.error || task.status,
                runtimeDown: r2.down, reason: r2.reason, onDone,
              });
              return; // 刷新交给对话框里的"只删除记录"成功后自己做
            }
            if (typeof onDone === 'function') onDone();
          },
        });
        if (!taskId) {
          // 提交就没成功：taskCenter 已弹带原因的 toast，这里补上可直接点的出口。
          recordOnlyModal({
            label, name: s.name, error: '卸载任务没能提交（原因见上方提示）',
            runtimeDown: rt.down, reason: rt.reason, onDone,
          });
          return;
        }
      } catch (e) {
        // 兜底：确认框/渲染层抛异常也必须说话（async 处理器里的异常会变成"点了没反应"）。
        toast('卸载「' + label + '」失败：' + ((e && e.message) || e), 'err', 12000);
      }
    },
  });
}

// confirmUninstallPlan 弹出"这次卸载会做什么"的确认框，返回 Promise<{wipe, force}|null>。
// ⚠️ 从真机事故里长出来（用户："frpc 点击卸载没有任何反应"）：原来两处各写一份确认框，而 modal 的
// close() 会**同步**调用 onClose → resolve(false) 先执行、Promise 被定死。现在只有这一份实现。
export function confirmUninstallPlan({ name, plan = {}, residual = false, forceDefault = false }) {
  return new Promise((resolve) => {
    let done = false;
    // 勾选框放在**这一份**实现里，调用方不再自己渲染第二个确认框（那正是原来出错的形态）。
    const remove = h('input', { type: 'checkbox' });
    // 强制卸载：只在计划明确允许时出现（默认不勾）。
    const forceAllowed = !residual && plan.force_allowed === true;
    // forceDefault：用户从「强制卸载」按钮进来 → 预勾选；多这一道确认是刻意的
    // （强制卸载会破坏别的包，必须让用户在**看得见后果**的那一屏再点一次）。
    const forceBox = h('input', { type: 'checkbox', checked: forceAllowed && forceDefault });
    const forceDeps = (plan.dependents || []).filter((d) => d.kind === 'brew').map((d) => d.name);
    const forceList = forceDeps.length ? forceDeps.join('、') : '依赖它的包';
    // data-confirm-uninstall 是自动化测试的稳定锚点：没有它测试只能靠文案猜，改一次文案就假失败。
    const okBtn = h('button.btn.btn-danger', {
      dataset: { confirmUninstall: '' },
      text: residual ? '删除残留数据' : (forceAllowed && forceDefault ? '强制卸载' : '确认卸载'),
    });
    // 勾上/取消强制时按钮文字跟着变：用户按下去之前必须一眼看见自己在做什么。
    forceBox.addEventListener('change', () => {
      okBtn.textContent = residual ? '删除残留数据' : (forceBox.checked ? '强制卸载' : '确认卸载');
    });
    const m = modal({
      title: (residual ? '删除残留数据 · ' : '卸载 ') + name,
      wide: false,
      body: h('div', { dataset: { confirmUninstall: '1' } }, [
        residual
          ? h('div', {
            style: { marginBottom: '8px' },
            text: '这个应用当前没有安装，这一步只删除磁盘上的残留产物/数据，不可恢复。',
          })
          : h('div', { style: { marginBottom: '8px' }, text: '将执行：' }),
        residual ? null : h('ul', { style: { margin: '0 0 10px 18px', lineHeight: '1.7' } },
          (plan.steps || []).map((x) => h('li', { text: x }))),
        (!residual && plan.keep_note) ? h('div.hint', { text: '会保留：' + plan.keep_note }) : null,
        // 强制卸载：按钮上/正文里**逐字**写清会破坏哪些包，以及真正执行的命令。
        forceAllowed ? h('div', {
          style: { marginTop: '10px', padding: '9px 11px', border: '1px solid var(--danger)',
            background: 'var(--danger-soft)', borderRadius: '6px', lineHeight: '1.7' },
        }, [
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', cursor: 'pointer' } },
            [forceBox, h('span', { text: '强制卸载（忽略依赖）' })]),
          h('div', { style: { marginTop: '4px', fontSize: '12.5px' },
            text: '会破坏这些包：' + forceList + '（它们会缺依赖、可能无法运行）。'
              + '执行的命令：brew uninstall --ignore-dependencies ' + (plan.formula || name) }),
          plan.force_note ? h('div', { style: { marginTop: '4px', fontSize: '12px', color: 'var(--text-dim)' },
            text: plan.force_note }) : null,
        ]) : null,
        (!residual && (plan.data_paths || []).length)
          ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '10px' } },
            [remove, h('span', { text: '同时删除数据/产物（不可恢复）：' })])
          : null,
        (plan.data_paths || []).length
          ? h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
            (plan.data_paths || []).map((x) => h('li.mono', { text: x })))
          : null,
      ]),
      footer: [
        h('button.btn', { text: '取消', onclick: () => finish(null) }),
        okBtn,
      ],
      onClose: () => finish(null),
    });
    okBtn.addEventListener('click', () => finish({
      wipe: residual ? true : !!remove.checked,
      force: forceAllowed && !!forceBox.checked,
    }));
    // finish 在 m 赋值之后才被调用；done 闸门保证 close() 触发的 onClose 不会覆盖用户点的结果。
    function finish(v) {
      if (done) return;
      done = true;
      m.close();
      resolve(v);
    }
  });
}

// dependentsModal 是"还有东西在用它"的说明对话框（用户 2026-09-21：不要只弹一句 toast，要逐条列出
// "谁在用它、该怎么办"并给可处理的入口：站点 → 网站管理切版本；应用/容器 → 去「应用」；Homebrew
// 包依赖 → 额外给「强制卸载」）。onForce 非空 = 允许强制，forceText 逐字写清会破坏哪些包。
function dependentsModal({ name, plan, onDone, onForce = null, forceText = '' }) {
  const deps = plan.dependents || [];
  const KIND_LABEL = { site: '站点', container: '运行中的容器', app: '应用', panel: '面板自身', brew: 'Homebrew 包' };
  const rows = deps.map((d) => h('div', {
    style: { marginBottom: '10px', paddingBottom: '8px', borderBottom: '1px solid var(--border-soft)' },
  }, [
    h('div', { style: { fontWeight: '620' }, text: (KIND_LABEL[d.kind] || d.kind || '对象') + '：' + (d.name || '') }),
    d.detail ? h('div', { style: { fontSize: '12px', color: 'var(--text-dim)', marginTop: '2px' }, text: d.detail }) : null,
    d.action ? h('div', { style: { fontSize: '12.5px', marginTop: '4px' }, text: '→ ' + d.action }) : null,
  ]));
  const wantsSites = deps.some((d) => d.kind === 'site');
  const wantsApps = deps.some((d) => d.kind === 'app' || d.kind === 'container');
  let handle = null;
  handle = modal({
    title: '还不能卸载 ' + name,
    wide: true,
    body: h('div', [
      h('div', {
        style: { marginBottom: '10px', padding: '9px 11px', background: 'var(--warn-soft)',
          borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
        text: plan.blocked || '还有对象在使用它，需要先处理。',
      }),
      ...rows,
      // 强制卸载的后果**逐字**写在正文里（按钮文字也一致），让用户按下去之前就知道。
      (onForce && forceText) ? h('div', {
        style: { marginTop: '4px', padding: '9px 11px', border: '1px solid var(--danger)',
          background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
        text: forceText,
      }) : null,
    ]),
    footer: (close) => [
      wantsSites ? h('button.btn.btn-sm', {
        text: '去「网站管理」处理', title: '切 PHP 版本 / 停用站点',
        onclick: () => { close(); if (typeof onDone === 'function') onDone(); location.hash = '#/sites'; },
      }) : null,
      wantsApps ? h('button.btn.btn-sm', {
        text: '去「应用」处理', title: '卸载或停止依赖它的应用/容器',
        onclick: () => { close(); if (typeof onDone === 'function') onDone(); location.hash = '#/services'; },
      }) : null,
      onForce
        ? h('button.btn.btn-sm', { text: '取消', onclick: close })
        : h('button.btn.btn-primary', { text: '知道了', onclick: close }),
      onForce
        ? h('button.btn.btn-sm.btn-danger', {
          text: '强制卸载', title: forceText || '忽略依赖强行卸载（会破坏依赖它的包）',
          onclick: () => { close(); onForce(); },
        })
        : null,
    ].filter(Boolean),
  });
  return handle;
}

// marketUninstallButton 从市场卸载（brew 原生 / 自研安装器 / compose）；确认框逐条列出会做什么。
// svc 是可选的**服务记录**：卸载失败时用它做"从面板移除记录"的兜底出口。
// isLimitTunable：判据来自**目录数据**（不写死条目名），只对与上传/执行上限有关的应用给按钮。
export function isNginxApp(mi) {
  if (!mi) return false;
  const hay = [mi.id, mi.brew_formula, mi.service_label, mi.name]
    .map((x) => String(x || '')).join(' ').toLowerCase();
  return /(^|[^a-z])nginx([^a-z]|$)/.test(hay);
}

export function isLimitTunable(mi) {
  if (!mi) return false;
  const hay = [mi.id, mi.brew_formula, mi.service_label, mi.name]
    .map((x) => String(x || '')).join(' ').toLowerCase();
  return /(^|[^a-z])nginx([^a-z]|$)/.test(hay) || /php/.test(hay);
}

export function marketUninstallButton(mi, onDone, svc = null) {
  // listPlan 是**列表里的**计划：刻意**没有查过依赖**（查依赖要跑真的 brew，每个条目约 0.4s，
// 36 条就是 15 秒冷启动）。真正的依赖判定在用户点「卸载」时按需查。
  const listPlan = mi.uninstall || {};
  const residual = !!mi.artifacts && !mi.installed;
  const recordName = (svc && svc.name) || mi.id;
  const recordLabel = (svc && (svc.display_name || svc.name)) || mi.name;
  const label = residual ? '删除残留数据' : '卸载';

  // submit 把一次卸载交给任务中心；force 只在用户明确选择时才为 true。
  const submit = async (wipe, force) => {
    await taskCenter.start({
      kind: 'uninstall', target: mi.id,
      title: (residual ? '删除残留数据 ' : (force ? '强制卸载 ' : '卸载 ')) + mi.name,
      start: () => api.marketUninstall(mi.id, wipe, force),
      onDone: (task) => {
        if (task && task.status && task.status !== 'succeeded') {
          const err = task.error || task.status;
          toast((residual ? '删除残留数据失败：' : '卸载失败：') + err, 'err', 12000);
          // 卸载失败时给出下一步：只有确实存在服务记录时才提供。
          if (svc && svc.name) {
            const rt = runtimeDownOf(svc);
            recordOnlyModal({
              label: recordLabel, name: recordName, error: err,
              runtimeDown: rt.down, reason: rt.reason, onDone,
            });
            return;
          }
        } else {
          toast(residual ? '已删除「' + mi.name + '」的残留数据'
            : '已卸载「' + mi.name + '」' + (wipe ? '（含数据/产物）' : '（数据/产物已保留）'), 'ok', 9000);
        }
        if (typeof onDone === 'function') onDone();
      },
    });
  };

  // 按钮**刻意不设 disabled**（也不设 aria-disabled）：disabled 的按钮点下去什么都不发生，
  // 原因还只在悬浮提示里 —— 用户看到的就是"点了没反应"。点下去用 toast 说明为什么不能卸载。
  const btn = h('button.btn.btn-sm.btn-danger', {
    text: label,
    title: residual ? '只删除磁盘上的残留产物/数据' : (listPlan.blocked || '卸载「' + mi.name + '」'),
  });
  btn.onclick = async () => {
    // 先取**完整**计划（含依赖检测）；这一步失败**不阻断**卸载，但如实说明"没能检查依赖"。
    let plan = listPlan;
    let depCheckErr = '';
    if (!residual) {
      btn.disabled = true;
      btn.textContent = '检查依赖…';
      try {
        const res = await api.marketUninstallPlan(mi.id);
        if (res && res.uninstall) plan = res.uninstall;
      } catch (e) {
        depCheckErr = (e && e.message) || String(e);
      } finally {
        btn.disabled = false;
        btn.textContent = label;
      }
      if (depCheckErr && !plan.blocked) {
        toast('没能检查「' + mi.name + '」的依赖关系（' + depCheckErr + '）：'
          + '按"没有已知依赖"继续；如果别的包依赖它，Homebrew 会在执行时拒绝（那时会有明确报错）',
        'warn', 12000);
      }
    }
    // 计划被 blocked 时按钮保持可点，点下去把原因说出来，而不是静默。
    const blocked = !!plan.blocked && !residual;
    // 被 **Homebrew 依赖**拦下且计划允许强制时（force_allowed），用户有第二条路
    // （--ignore-dependencies），只在用户明确选择时才走。
    const canForce = blocked && plan.force_allowed === true;
    const forceDeps = (plan.dependents || []).filter((d) => d.kind === 'brew').map((d) => d.name);
    const forceText = plan.force_note
      || ('强制卸载会破坏这些包：' + (forceDeps.join('、') || '依赖它的包')
        + '（它们会缺依赖、可能无法运行）。命令：brew uninstall --ignore-dependencies '
        + (plan.formula || mi.id));
    try {
      if (blocked) {
        // 有结构化依赖 → 弹说明对话框；允许强制时同时给出「强制卸载 / 取消」。
        if ((plan.dependents || []).length || canForce) {
          dependentsModal({
            name: mi.name, plan, onDone,
            // 强制卸载**不直接提交**：先弹"将执行…+强制开关已勾选"的确认框（两道门）。
            onForce: canForce
              ? () => {
                void (async () => {
                  const ans = await confirmUninstallPlan({
                    name: mi.name, plan, residual: false, forceDefault: true,
                  });
                  if (!ans) return;
                  await submit(ans.wipe, true);
                })();
              }
              : null,
            forceText: canForce ? forceText : '',
          });
          return;
        }
        toast('现在不能卸载「' + mi.name + '」：' + plan.blocked, 'warn', 14000);
        return;
      }
      const answer = await confirmUninstallPlan({ name: mi.name, plan, residual });
      if (!answer) return; // 取消 / 直接关掉确认框：什么都不做（也不静默——本来就没提交）
      await submit(answer.wipe, answer.force);
    } catch (e) {
      // 同上：任何一步失败都要有一句带原因的话，绝不静默。
      toast((residual ? '删除残留数据' : '卸载') + '「' + mi.name + '」失败：' +
        ((e && e.message) || e), 'err', 12000);
    }
  };
  return btn;
}
