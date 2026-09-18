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
//       · managed 是 true 还是 false    → 「卸载」还是「从列表移除（不卸载软件）」
//       · ui.slug 且 !ui.console_only   → 有没有「打开 / 直链」（见 openDirectActions）
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
// 「上传与执行上限」的渲染实现只有 views.js 的 renderLimitsInto 一份
//（用户 2026-09-18 要求这类常用更改必须是功能，而不是让用户去编辑配置原文件）；
// 2026-09-22 起它与 nginx 四页 / 配置文件 / PHP 环境一起挂在同一个
// 「⚙️ 调整配置」弹窗里（nginxpanel.js），所以这里只 import 那一个入口。
import { adjustConfigModal } from './nginxpanel.js';

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

// ---------------------------------------------------------------------------
//  合并「我的应用」：市场条目 + 服务记录 → 一行一条（去重）
// ---------------------------------------------------------------------------

// KNOWN_LABEL_PREFIXES 是已知的 launchd 标签前缀。
//
// 真机（mini，2026-09-17）上同一套 PHP 有两套命名：
//   · 目录里声明的   homebrew.mxcl.php@8.2
//   · 面板记录里的   sh.brew.php@8.2
// 记录**名**还会被压成 homebrew-mxcl-php8-2 / sh-brew-php8-2 这类写法。
// 归一化必须同时吃掉"前缀"与"分隔符"两种差异，否则同一个服务会渲染成两行 ——
// 而两行各带一组按钮，用户点哪一行都不知道对不对。
const KNOWN_LABEL_PREFIXES = [
  'homebrew.mxcl.', 'homebrew-mxcl-', 'sh.brew.', 'sh-brew-',
  'cn.zizdog.', 'cn-zizdog-', 'com.zizdog.', 'com-zizdog-',
];

// appKeyOf 把一个应用 / 服务归一化成一个去重 key。
//
// 规则（顺序固定，改动前先读上面那段真机结论）：
//   ① 取"标识类字段"：launch_label（服务记录）→ service_label（市场条目）→
//      id（市场条目的 slug）→ uninstall.service → name；
//      绝不能只看 name —— 市场条目的 name 是**展示名**（"Stirling PDF"、带空格），
//      而服务记录的名字是 slug（stirling-pdf）。拿展示名做 key 会让同一个 compose
//      应用在「我的应用」里出现两行（一行来自市场、一行来自服务记录）。
//   ② 统一小写；
//   ③ 去掉一个已知前缀（只去一次，避免把服务名里本来就有的一段吃掉）；
//   ④ 把 @ . - _ 与空白全部删掉 —— 于是
//        homebrew.mxcl.php@8.2 / sh.brew.php8-2 / php82 / php8.2 / php8-2
//      全部收敛成 php82；"Stirling PDF" 与 stirling-pdf 也收敛成 stirlingpdf。
//
// 为什么是"删掉"而不是"统一成某一个字符"：真机上点号、连字符、@ 和"什么都没有"
// 四种写法都出现过；只把 `-` 映射成 `.` 仍然会漏掉 `php82` 这种无分隔符的写法。
// 过合并的风险（foo-bar 与 foobar 被并成一条）在本项目的服务命名里不存在；
// 万一将来出现，由 tools/appdetail-verify.mjs 的归一化断言暴露，而不是悄悄并掉。
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

// SVC_MERGE_FIELDS：被丢弃那条服务记录的"有效信息"要补进保留的那条。
// 只补**保留那条为空**的字段，绝不覆盖已有的真实值。
const SVC_MERGE_FIELDS = [
  'port', 'launch_label', 'plist_path', 'config_path', 'log_path', 'work_dir',
  'start_cmd', 'compose_file', 'container', 'image', 'category', 'health_url',
  'display_name', 'icon', 'description',
];

function blank(v) { return v === undefined || v === null || v === '' || v === 0; }

// svcInfoScore 决定同 key 的多条服务记录里保留哪条：
// managed=true 最优先（它才有完整生命周期），其次"确实在跑"的那条，
// 再看谁带的路径 / 命令更全。分数相同则保留先出现的那条（API 顺序稳定）。
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

// pickAndMergeSvc 从同 key 的服务记录里挑一条，并把其余记录的字段补进来。
// 状态 / 端口 / 日志路径 / plist 路径按"非空者优先、运行中优先"合并。
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

// marketRank 决定同 key 的市场条目里保留哪条：优先带界面 / 直链 / 文档元数据的，
// 再优先已安装的 —— 那些字段是安装器与「打开 / 文档」按钮的依据。
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

// mergeAppEntries 把"市场目录（只取已安装 / 已纳管的）"与"服务记录"合并去重。
//
// 返回 [{ key, market, svc }]：两个字段都可能为 null，但不会同时为 null。
// 关键点：**两个都留着** —— openServicePanel({market, svc}) 正是要这两份数据
// （市场给 ui / port_url / docs_url / 安装器语义，服务记录给状态 / 绝对配置路径 / 日志）。
//
// 保留哪条：
//   · market 侧 —— marketRank 最高的（优先市场目录里能对上的那条记录，用户要求）；
//   · svc   侧 —— svcInfoScore 最高的（managed=true → 在跑 → 信息更全）。
// 被丢弃那条的有效信息由 pickAndMergeSvc 合并进来。
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

// dedupeMarketEntries 把市场目录按归一化 key 去重（同 key 保留 marketRank 最高的那条）。
//
// 用户要求"两部分都按去重后的应用来渲染"：市场列表本身也可能出现同一个应用的
// 两条目录条目（不同写法），渲染前统一收敛 —— 否则同一张能力会被画成两张卡片。
// 「已安装」Tab 的去重仍然走 mergeAppEntries（它要同时握着 market 与 svc 两份数据）。
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

// portDirectURL 用**当前访问面板的主机名** + 服务端口拼一个直链。
//
// 后端不返回局域网 IP，所以只能用 location.hostname：从 127.0.0.1 / 局域网 IP /
// 隧道域名访问时，拼出来的是各自那条路，通常可用。但服务可能只监听 127.0.0.1，
// 或这个端口根本不是 HTTP 界面 —— 所以调用方必须在 title 里如实写上这层不确定性
// （见 openDirectActions 的 synthesized 分支）。协议固定 http://：面板自己可能是
// https，而应用端口不是。
export function portDirectURL(port) {
  const p = Number(port) || 0;
  if (!p) return '';
  const host = (typeof location !== 'undefined' && location.hostname) || '';
  if (!host) return '';
  return 'http://' + host + ':' + p + '/';
}

// subpathWarning 把"这个应用的子路径实测不可用"渲染成按钮下方的一行小字。
//
// 用户 2026-09-17 的第四条抱怨：Miniflux / Syncthing / Alist / ddns-go 显示
// 「⚠️ 打开」却没有任何解释。只在按钮上加 title 不够 —— 不悬浮就看不到，
// 用户只会以为按钮坏了。所以给一行**始终可见**的说明，管理面板里同样渲染。
// ui.note 为空时用一句兜底，绝不把"为什么"留空。
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
//
// 用户 2026-09-17 明确要求"恢复之前的卡片展示样式"：改动前的应用市场用的是
// `.grid.grid-3` 下的卡片网格（图标 + 名称 + summary + pill + 一排按钮）。
// 「服务管理 + 应用市场合并」那一轮把它换成了"行"，用户不接受 —— 这一轮四个 Tab
// 全部回到卡片，而且**共用这一个函数**：结构、间距、pill、按钮排布只有一份，
// 不会出现"已安装的卡片和市场里的卡片长得不一样"。
//
// 调用方（apps.js 的市场/docker/建站卡片、services.js 的已安装卡片）只负责
// 把数据折算成 props，不自己拼 DOM。
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
    // 一句话说明（summary 之外的长描述）。以前把整段 description 铺在卡片里，
    // 文字比按钮还多；现在卡片只放一段，完整说明仍然在 title 与「⚙️ 管理」里。
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

// openTargetOf 计算卡片上的「打开」按钮该指向哪里 —— 用户 2026-09-17 第六条的
// **唯一**判定（改这里之前先读完用户原话）：
//
//   支持子路径（有 ui.slug、不是自带控制台、且不是人工实测 prefer_direct）
//       → "/<slug>/"（相对路径，127.0.0.1 / 局域网 IP / 隧道域名下都通）；
//   不支持子路径 → 端口直连：优先接口给的 port_url，没有就用
//       http://<当前访问面板的主机名>:<服务端口>/ 拼；
//   两者都没有（纯纳管服务没有端口、no_daemon 命令行工具）→ 没有打开入口。
//
// 例外一：console_only（frpc 这类应用自带的控制台）**不给**任何打开入口 ——
//   用户 2026-09-16 明确不要在面板里跳过去（与 openDirectActions 同一判据）。
// 例外二：self_conf（phpMyAdmin 的 nginx location 只挂在面板那条路上）必须走
//   panelPath；用相对路径会打到 SPA 的回落页（200，但内容是面板首页）。
//
// 返回 { href, subpath, port, direct, title } 或 null。
// 管理面板里的「打开 / 直链」仍然用 openDirectActions（两颗都给、prefer_direct
// 只加警示）—— 这条规则只服务于"卡片上只留一颗「打开」"的那两个 Tab。
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

// openOnlyAction 渲染卡片上那**唯一**的「打开」按钮 ——
// 用户 2026-09-17："对于已经安装的软件，只显示打开，不显示直链"。
// 没有可用入口（console_only / 没有界面也没有端口）时返回空数组，
// 绝不给出一个点开必然打不开的按钮。
export function openOnlyAction(m, opts = {}) {
  const t = openTargetOf(m, opts);
  if (!t) return [];
  return [h('a.btn.btn-sm.btn-primary', {
    href: t.href, target: '_blank', rel: 'noopener', text: '打开', title: t.title,
  })];
}

// PORT_ACCESS_WARNING 是用户要求的**逐字**提示（不要改写、不要加前后缀）。
export const PORT_ACCESS_WARNING = '该应用不支持子路径，请用端口访问，或自行配置反代。';

// portAccessWarning 把上面那句话渲染成卡片上**始终可见**的一行小字。
// 只在「打开」真的指向端口直连（openTargetOf 的 subpath=false）时给；
// 支持子路径的卡片不给，没有打开入口的卡片也不给（那时问题不是"子路径"，而是
// "这个应用根本没有网页界面"）。管理面板里仍然用 subpathWarning（它带 ui.note 原文）。
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
//
// market —— 市场条目（可能没有）；svc —— 服务记录（可能没有，「我的应用」行总是有）。
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
  // 注意 m 可能为 null（从「我的应用」行点开面板时可能只给 svc）—— 必须先判 m，
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
  // 只在**可能真有凭据**时才去问（应用声明了配置文件路径；后端正是从那个文件里解析凭据）。
  // 否则每次打开没有配置文件的应用（如 Docker 运行时）都会打一条 404 ——
  // 按钮确实不会出现（下面 catch 掉），但浏览器控制台会留一条错误，
  // 而我们自己的 UI 门禁把控制台错误当失败（2026-09-18 smoke 因此变红）。
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
//
// 状态从哪来：「我的应用」行把服务记录（含 state）直接递进来（opts.svc）；
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

// marketQuickActions 给**应用市场卡片 / 「我的应用」行**的那一排常用动作。
//
// 用户要求"已安装的应用，卡片上直接给出常用动作，不需要先跳到别的页面"。
// 这里放真正好用的几个：启停/重启/刷新 + 管理。
// 「📝 编辑配置文件」「📜 日志」「🔑 凭据」「重装」「文档」「卸载」都在管理面板里 ——
// 它们要么需要服务记录（市场条目只有配置**文件名**，绝对路径只能从服务记录拿），
// 要么需要一个更宽的弹窗才看得清；卡片宽度不适合摊开一个编辑器。
// 关键点：点「管理」打开的就是**与「我的应用」行同一个面板**，用户没有被搬走。
//
// 2026-09-16 起卡片上**只有这一颗**"进面板"的按钮：以前同时有卡片主按钮
// 「查看服务」（apps.js 的 primaryButton）与这里的「⚙️ 详情」，两者打开的是
// 同一个面板 —— 用户原话"原来的详情和查看服务内容一样，统一保留一个管理就行了"。
// 2026-09-17 起卡片是「打开 / 直链 / 启停 / 重启 / 刷新 / 管理」这一组固定语义
// （见 openDirectActions），安装生命周期动作（重装 / 卸载）只在「⚙️ 管理」里。
//
// opts.state：市场页顺手拉到的服务状态（见 apps.js 的 svcState）。
// 有它卡片上的首颗按钮才是对的（在跑→「停止」，没跑→「启动」）；
// 没有就退化成「启动」，后端对已在跑的服务执行 start 是幂等的。
// opts.onManage：由调用方接管"开管理面板"（应用市场传的是 apps.js 的
// openAppDetail，「我的应用」行传的是带 svc 的 openServicePanel）。为什么要让
// 调用方接管：市场页手里有完整的目录条目，面板里的「重装」要用调用方那套安装器
// 上下文（`onReinstall`）——否则重装就会退化成不计应用选项的通用安装。
// 不传 onManage 时退回 `openServicePanel({market, svc})`，行为一致。
//
// 「打开 / 直链」**不在这里**：它们由 openDirectActions 单独给，卡片渲染时
// 与这一排动作并排（apps.js / services.js 都调用同一份）。
//
// opts.svc：合并后的「我的应用」行会同时递进服务记录。有它时以服务记录为准：
//   · 启停按钮一定给（有真实记录就能管，不再依赖 adopted / service_in_launchd 推断）；
//   · 目标对象用服务记录（面板接口只认它的 name），状态直接用 s.state。
// 市场卡片不传 svc，行为与以前完全一样。
export function marketQuickActions(m, { state, onDone, onManage, svc = null } = {}) {
  const s = svc || null;
  const installed = !!(m && (m.installed || m.adopted)) || !!s;
  if (!installed) return [];
  // 卡片上的启停按钮只在"**服务确实可管**"时给：有服务记录（svc）、有面板记录
  // （adopted）或服务已在 launchd 里。装了但服务没注册的孤儿态（plist 丢了 /
  // 装到一半）点启停只会报"找不到服务"；而 CLI 工具（ffmpeg 这类 no_daemon）
  // **本来就没有服务**，给它启停按钮同样是点了必然报错。这两种状态该用的按钮在
  // 「⚙️ 管理」里（面板里有「重装」），所以这里只留「⚙️ 管理」。
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
// ⚠️ 一条铁律（2026-09-21 用户发火点名的）：**只有真的有服务记录/launchd 作业
// 而且它报 stopped 时，才允许写「已停止」。** 没有任何守护进程的应用
// （ffmpeg / python@x.y / phpMyAdmin 这类 no_daemon）与"装了但面板里没有记录"
// 的应用都**不是**"已停止" —— 它们本来就没有可停止的进程，写「已停止」是假信息。
// 用户原话："以下应用都被归类到了：已停止分类中：Docker 运行时（Colima）、FFmpeg、
// Nginx、phpMyAdmin、Python 3.10/3.11/3.13。它们真的不运行吗？！"
// （本机 nginx 事实在 :80 上跑着，只是面板里没有它的记录。）
//
// 判据顺序（按"用户能观察到的事实"排）：
//   ① 有服务记录 → 运行中 / 异常 / 环境不可用 / 未安装 / 未知 / **已停止**；
//   ② no_daemon  → 网页入口 / 命令行工具（本来就没有常驻进程）；
//   ③ 其余（装了但面板里没有记录）→ **已安装（面板里暂无记录）** +
//      端口是否监听（有健康检查结论就照实说；没有就明写"未检测"，绝不猜）。
//
// 2026-09-16 修 bug（用户反馈 "ffmpeg 显示读取中…未在服务管理里"、而且**卡在**
// 读取中上）：以前 st 为空就一律返回「读取中…」。但 render() 只在数据**已经查完**
// 之后才被调用（见文件末尾的 `render(await resolvePanelData(...))`），所以 st
// 为空**不是"还在读"，而是"这个应用根本没有服务记录"**。两种情况混成一个文案，
// 没有守护进程的应用（ffmpeg / phpMyAdmin 这类 no_daemon）就永远停在一句
// 不落定的话上，看上去像界面卡死。
//
// 2026-09-21 用户："弱化管纳这个概念" —— 原来这里写的是「未纳管」，那是个内部词。
// 改成"面板里暂无记录"：说的是一件用户能观察到的**事实**。
//
// 2026-09-17 导出：合并后的「我的应用」每一行的状态 pill 也走这一份措辞 ——
// 服务行、市场卡片、管理面板三处的状态文案必须同一套（用户要求"复用现有
// statusLine 那套措辞"），所以它不再是本文件的私有函数。
// @param {object} [s] 服务记录（可选）：只为转述它上面**真实做过**的健康检查结论。
export function statusLine(st, m, s = null) {
  if (st) {
    if (st.running) return { cls: 'ok', text: '运行中', title: st.detail || '' };
    if (st.status === 'error') return { cls: 'danger', text: '异常', title: st.detail || '' };
    if (st.status === 'unavailable') return { cls: 'warn', text: '环境不可用', title: st.detail || '' };
    if (st.status === 'not-installed') return { cls: 'warn', text: '未安装', title: st.detail || '' };
    if (st.status === 'unknown') return { cls: '', text: '未知', title: st.detail || '' };
    // 走到这里 = 有记录、服务没在跑、也没有报错 → 这才是真的「已停止」。
    // no_daemon 的应用**不该**有服务记录；万一有（历史遗留）也要按"没有常驻
    // 进程"如实说，而不是说它"已停止"。
    if (m && m.no_daemon) return noDaemonLine(m);
    return { cls: '', text: '已停止', title: st.detail || '服务记录报的是停止状态，可以在这里启动它' };
  }
  if (m && m.no_daemon) return noDaemonLine(m);
  if (m && isInstalledMarketItem(m)) {
    // 装了、但面板里没有它的服务记录（例如本机 nginx 在 :80 上跑着，只是没登记）。
    // **绝不能**写「已停止」—— 面板根本不知道它停没停。
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

// noDaemonLine 是"没有常驻进程"的应用的终态文案。
//
// no_daemon 只说明"没有常驻进程"，**不等于**"是网页入口"：phpMyAdmin（nginx
// alias）是网页入口，而 ffmpeg 是命令行工具。以前一律写「网页入口」，ffmpeg
// 就被标成"网页入口"（用户 2026-09-16 截图反馈）。所以按**有没有界面**分开说，
// 并且一律不出现「已停止」（它们本来就没有可停止的东西）。
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

// portCheckNote 是"这个端口到底通不通"的如实说明。
//
// 只在**后端真的做过检查**时才给结论（服务记录里的 health.checked）：
//   · 通过 → 端口在监听（来自面板自己的健康检查，不是猜的）；
//   · 没通过 → 端口没在监听（或检查地址不对），并给出那个地址；
//   · 没查过 → 明写"未检测"，绝不把"不知道"说成"在跑"或"停了"。
//
// 为什么用健康检查结论而不是自己探测：前端在浏览器里发请求会撞混合内容/跨域，
// 拿到的失败不能证明端口没在听 —— 那种"看起来不对"的结论必须先怀疑探测方式
// （2026-09-13 的教训）。所以这里只转述后端已经查过的结论。
//
// 导出给 services.js 的卡片正文用（同一份措辞，不写第二遍）。
//
// @param {object} m 市场条目（给端口）
// @param {object} [s] 服务记录（给**真实做过**的健康检查结论；没有记录时为 null）
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

// openDirectActions 渲染「打开 / 直链」这一对入口 —— **唯一的一份实现**。
//
// 用户 2026-09-17 的明确要求（两颗按钮的语义固定，不再按探测结果调换归属）：
//   · 「打开」**永远**是面板反代的子路径 `/<slug>/`（相对路径即可：
//     127.0.0.1 / 局域网 IP / 隧道域名下都指向同一个入口）；
//   · 「直链」**永远**是应用自己的端口地址（接口给的 `port_url`，形如
//     http://192.168.1.4:3000/）；
//   · 子路径实测不可用（`ui.prefer_direct`，人工真机结论）时，「打开」**不换成**
//     直连 —— 只在文案上加 ⚠️ 与 title 里的实话，建议用户改用旁边的「直链」。
//
// 为什么不再看 GET /api/v1/market/proxies 的探测结果：那个探测是**不带面板
// 会话**发出的，应用子路径在面板端口上会返回 401（要登录），proxy_ok 几乎恒为
// false。用它决定归属就会把「打开」错判成端口直连 —— 用户看到的
// "IT-Tools 反了"（打开变成 192.168.1.4:8083、子路径降级成"试试"）就是它。
// 探测结果只服务于面板顶部的「检测可用性 / 生成 nginx 入口」工具，不再参与这里。
//
// 调用方（三处必须一致）：apps.js 的市场卡片、services.js 的「我的应用」行、
// 本文件的「应用管理」面板。console_only（frpc 这类应用自带的控制台）不渲染：
// 用户明确不要在面板里跳过去（2026-09-16）。没有界面（没有 slug）时：
//   · 只有在**真有服务记录 + 有端口**时给一颗「直链」（用 location.hostname + 端口拼，
//     title 里如实写明这是拼出来的、可能不适用）；
//   · 市场条目自己声明了 port（Ollama 的 11434 这类）但目录里没有界面 —— 不给，
//     因为那多半是 API 端口，点了必然打不开；这就是"不给点开必然打不开的按钮"。
//
// @param {object} m   市场条目（可以为 null —— 纯纳管的第三方服务就没有市场条目）
// @param {object} [opts]
// @param {object} [opts.svc]     服务记录（补齐端口，并在没有 port_url 时拼直链）
// @param {boolean}[opts.explain] true → 连"为什么没有打开入口"也渲染成一行小字
//                                （管理弹窗里用；卡片上不摆这句，免得占地方）
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

  // 应用自带的控制台（console_only，例如 frpc 的 admin UI）：用户明确不要在面板里
  // 跳过去，所以两个入口都不给。只有市场条目才会带 ui，这条判据不影响纯纳管服务。
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

  // SelfConf（phpMyAdmin）：它的 nginx location 由安装器自己写、且只允许本机，
  // 所以唯一能用的入口是**面板自己**那条（要求先登录面板）。
  // 用相对路径会打到面板 SPA 的回落上（返回 200 却是面板首页，极具误导性），
  // 所以这里走 panelPath（面板入口 + /<slug>/）。
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
  // prefer_direct 是**人工实测**的结论（自动探测发现不了"资源全 200、但前端路由
  // 不认这个前缀"）：只加警示，不改按钮归属。除了 ⚠️ 与 title，调用方还会用
  // subpathWarning() 在按钮下方渲染一行始终可见的说明（用户第四条要求）。
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

// reinstallButton 把「重装」放进**管理面板** —— 卡片上不再直接给这颗按钮
// （用户 2026-09-17："重装、卸载、文档等放进管理的弹出页面里"）。
//
// 为什么不在这里自己实现安装：安装器住在 apps.js（面板自研安装器的应用要先弹
// 选项框，例如 Qwen 的"要不要鉴权"），所以调用方通过 `onReinstall` 把**现有的
// 重装动作**（apps.js 的 openInstaller）递进来，行为与卡片上原来那颗「重装」
// 完全一致，后端接口一行没改。
// 没有安装器上下文时（例如从市场卡片点开）退回同一个后端安装接口：
// 安装器本身是幂等的（已下载的产物复用、配置与数据保留），语义相同。
//
// 只在**真的可重装**时给这颗按钮：卸载计划是 installer / service 两类
// （面板自研安装器、compose / 托管服务）才摆 —— 纳管类第三方服务（nginx /
// php / mysql，kind=forget）没有安装器，点「重装」只会拿到后端的
// "纳管类应用请用纳管而不是安装"，那不叫重装；kind=none（brew 核心组件、
// 找不到可卸载对象）同理。判据全部来自数据，没有按应用 ID 的分支。
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
  //   ① 状态相关动作（添加到面板/安装，由调用方给） ② 启停 ③ 界面 ④ 配置 ⑤ 凭据
  //   ⑥ 日志 ⑦ 应用自己的功能（目录条目的 app_widgets） ⑧ 收尾（从列表移除 / 卸载）
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

    // ③ 界面：打开 / 直链 —— 与市场卡片、「我的应用」行**同一份实现**（openDirectActions）。
    //    frpc 这类应用自带的控制台（console_only）不在这里渲染，用户明确不要。
    //    这里把服务记录一起递进去（svc）：纯纳管的第三方服务没有市场条目、
    //    也没有 port_url，只有端口 —— 那就用 location.hostname + 端口拼一颗「直链」，
    //    并在 title 里说明这是拼出来的。两者都没有时 explain 会渲染一行"为什么没有"。
    out.push(...openDirectActions(mi, { svc: s, explain: true }));

    // ③b 重装：卡片上不再直接给这颗按钮，统一收进管理面板（用户 2026-09-17 要求）。
    //     有安装器上下文时走调用方那套（保留应用自己的选项框），否则退回通用安装接口。
    const rb = reinstallButton(mi, { onReinstall, onDone: afterAction });
    if (rb) out.push(rb);

    // ④ 配置：判据是"面板知不知道配置文件的**绝对路径**"，不是"有没有服务记录"。
    //
    // 2026-09-18 用户报障："php 和 nginx 的编辑配置文件都是灰色的" —— 旧逻辑只在
    // 服务记录里带 config_path 时才给按钮，而 nginx 完全可以"装着、在跑、面板里
    // 没有记录"，于是最常用的那颗按钮变成灰色，用户连 client_max_body_size /
    // upload_max_filesize 都改不了，只能手工改文件。
    //
    // 配置路径是**目录的静态属性**（后端 config_path_abs / 服务记录的 config_path
    // 都已经是绝对路径），与"归不归面板管"无关 —— 所以这里一律给可点的按钮。
    const cfgPath = (s && s.config_path) || (mi && mi.config_path_abs) || '';
    if (cfgPath) {
      // 没有服务记录时给一个最小的上下文：编辑器只需要路径与显示名，
      // 「重启服务」那颗按钮在没有记录时会如实报错（而不是假装重启成功）。
      const cfgCtx = s || { config_path: cfgPath, display_name: (mi && mi.name) || cfgPath };
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        title: (mi && mi.post_install_hint ? '要改什么：' + mi.post_install_hint + ' —— ' : '') +
          '直接编辑 ' + cfgPath + '（保存后需重启服务才生效）',
        onclick: () => configFileModal(cfgCtx, afterAction),
      }));
    } else if (mi && mi.config_path) {
      // 目录里只写了文件名、也算不出绝对路径（罕见）：如实说"面板不知道它在哪"，
      // 并给出用户能自己做的事 —— 不再把入口灰掉（灰按钮 = 点了没反应）。
      out.push(h('button.btn.btn-sm', {
        text: '📝 编辑配置文件',
        title: '面板不知道这个应用的配置文件在哪（目录里写的是 ' + mi.config_path +
          '，解析不出绝对路径）。可以到「文件管理」里按路径找到它直接编辑。',
        onclick: () => toast('这个应用的配置文件路径面板解析不出来（目录里写的是 ' + mi.config_path +
          '）；可以到「文件管理」里手动找到它', 'warn', 12000),
      }));
    }

    // ④b **常用设置**（用户 2026-09-18 明确要求："这些常用更改应该同时做成功能，
    //     而不应该是让用户只能编辑配置原文件"）。
    //
    //     nginx / PHP 上最常改的两件事：
    //       · 一次能传多大（client_max_body_size / upload_max_filesize / post_max_size）
    //       · 脚本能跑多久（max_execution_time）
    //     这里给一颗直达按钮，打开的是**与面板设置同一份**的编辑器（不复制实现）。
    // nginx 专属：宝塔式的「⚙️ 调整配置」（原来这里有两颗按钮
    // 「Nginx 管理」与「上传大小 / 执行时间」，内容重复 —— 用户 2026-09-22 要求
    // 合并成一颗：nginx 四页 + 上传与执行上限 + 配置文件清单 + PHP 环境）。
    if (isNginxApp(mi)) {
      out.push(h('button.btn.btn-sm', {
        text: '⚙️ 调整配置',
        title: '打开统一配置面板：nginx（服务 / 性能调整（含最大上传大小）/ 配置修改 / 错误日志）、'
          + '上传与执行上限、配置文件清单、PHP 环境',
        onclick: () => adjustConfigModal(),
      }));
    } else if (isLimitTunable(mi)) {
      // PHP 这类条目：同一份实现（views.js 的 renderLimitsInto），只是直接落到那一页 ——
      // 不再有第二个"上传大小 / 执行时间"弹窗实现。
      out.push(h('button.btn.btn-sm', {
        text: '⚙️ 调整配置',
        title: '直接打开「上传与执行上限」：改「一次能传多大」「脚本能跑多久」'
          + '（改完自动重载 nginx、重启 php-fpm，并回读生效值）—— 不需要编辑配置文件',
        onclick: () => adjustConfigModal({ page: 'limits' }),
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

    // ⑧ 收尾：managed=false 只给「从列表移除（不卸载软件）」，managed=true 才给「卸载」。
    //    这条判据来自**服务记录**，不来自应用 ID —— 非面板管理的服务不出现卸载。
    //
    // 2026-09 真机缺陷：用户删掉 Colima/Docker 后，一条 compose 记录只剩「卸载」，
    // 而卸载必然失败（"未找到 docker compose 命令"）→ 记录永远删不掉、被卡死。
    // 现在**任何托管记录都额外给一个"只删记录"出口**；运行时不可用时它就是唯一
    // 能真正成功的收尾动作，文案如实写「从列表移除（不停止容器）」并说明未停止容器。
    //
    // 2026-09-21 用户："用户不需要知道什么是纳管 … 只要知道自己可以在应用里执行
    // 安装、卸载、重装这些动作。" 所以这里的文案全部改成用户语言（删掉"纳管/托管"），
    // 但**判据与行为一个字没改**：能不能真卸载仍然只看服务记录/目录卸载计划，
    // 面板永远不会假装能卸载用户自己装的软件（那条边界是靠按钮文案如实表达，
    // 不是靠让用户理解"纳管"）。
    if (s) {
      const rt = runtimeDownOf(s);
      const pk = (mi && mi.uninstall && mi.uninstall.kind) || '';
      if (pk === 'brew' || pk === 'installer' || pk === 'service') {
        // 计划说得清"怎么真卸载" → 收尾只有这一颗主动作。
        //
        // 2026-09-21 用户明确要求：不要再并排摆一颗"只删记录"（原话："移除却不
        // 卸载是什么意思 …… 让用户看不到却持续运行"）。目录里的应用
        // （brew 原生 / 面板安装器 / compose）都走这里，点下去是真卸载。
        out.push(marketUninstallButton(mi, afterAction, s));
      } else if (s.managed) {
        // 面板托管的服务，但目录里没有它的卸载计划（下架条目等）：
        // 仍然走通用卸载（停服务 + 删记录），不摆"只删记录"。
        out.push(uninstallButton(s, afterAction));
      } else {
        // 面板**不认识**这条服务（没有目录条目、也不知道怎么卸载它）：
        // 唯一诚实的收尾是"停掉 + 从记录移除"，并逐字说明软件仍在磁盘上。
        // 见 forgetButton：它现在会先停服务，绝不留下"隐身运行"。
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
    const line = statusLine(st, mi, s);
    clear(statusBox);
    appendAll(statusBox,
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center', marginBottom: '8px' } }, [
        h('div', { style: { fontWeight: '620', fontSize: '14px', marginRight: '4px' }, text: displayName }),
        pill(line.cls, line.text, line.title),
        ((s && s.port) || (mi && mi.port)) > 0 ? pill('', ':' + ((s && s.port) || mi.port)) : null,
        // 「面板托管 / 仅纳管」pill 已删除（2026-09-21 用户："弱化管纳这个概念"）。
        // 不能换成"由面板安装 / 本机已有"：记录里的 managed=false 并不等于
        // "软件是用户装的" —— 面板自研安装器装出来的应用走的也是
        // RegisterInstalledService，记录同样是 managed=false（见 install.go:1945）。
        // 照记录写就会对用户说假话（铁律 11），而"谁装的"也不是用户能做的动作。
        // 面板到底是「卸载」还是只「从列表移除（不卸载软件）」，由下面那颗
        // 收尾按钮自己如实说明 —— 那才是用户需要知道的。
        // 「面板里没有服务记录」只对"**本该有服务**却查不到记录"的应用说。
        // no_daemon 的应用（ffmpeg / phpMyAdmin）本来就没有守护进程，报这一句
        // 是纯粹的误导 —— 用户 2026-09-16 反馈的正是 ffmpeg 上这句。
        // 它们的状态由上面的 statusLine 如实说成"命令行工具/网页入口（无常驻进程）"。
        // 2026-09-17 合并后不再写"未在服务管理里"：那个页面已经不存在了（它就是
        // 「我的应用」Tab），对一个不存在的页面报"不在里面"只会让人困惑。
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
    // 子路径实测不可用（prefer_direct）时，按钮下方补一行**始终可见**的说明 ——
    // 用户第四条抱怨就是"⚠️ 打开没有任何提示"。与「我的应用」行、市场卡片同一份。
    appendAll(actionBox, subpathWarning(mi));

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
    //   · 其余是"装了、但面板里还没有它的记录"—— 说清加进来之后才会有
    //     配置/日志/启停（入口：工具栏「+ 注册服务」；不再说"纳管"这个内部词）。
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

  // 首屏渲染必须**一定**落定：resolvePanelData 内部每一步都已经自己 try/catch
  // （服务查询、凭据查询失败都只是"没有那项数据"），正常不会抛。这里再兜一层，
  // 是因为面板一旦抛出，界面上就永远停在「正在读取服务状态…」这个中间态 ——
  // 用户反馈的"卡在读取中"无论如何不能再出现。
  // 兜底渲染用"无服务记录"的形态：它会走 statusLine 的终态分支（面板里暂无记录 /
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

// runtimeDownOf 判断"这条记录的运行时现在还可用吗"。
//
// 为什么必须有（2026-09 真机缺陷）：用户把 Colima/Docker 删掉后，compose 记录
// 的驱动直接不可用 —— 后端把原因放在 driver_error、状态是 unavailable。这时
// 「卸载」永远失败（"未找到 docker compose 命令"），如果界面上只有这一个出口，
// 记录就永远删不掉、用户被卡死。判据只认后端给的真实字段，不猜。
function runtimeDownOf(s) {
  const reason = (s && s.driver_error) || '';
  const status = String((s && s.state && s.state.status) || '');
  return { down: !!reason || status === 'unavailable', reason: reason || '运行时不可用' };
}

// forgetRecord 走"只删记录"接口（DELETE /api/v1/services/{name}，后端只从面板
// 移除记录、不触碰系统）。**404 必须如实降级**：旧面板没有这个能力时明确说
// "该版本面板不支持，请升级"，绝不假装已经删掉。
//
// 提示语一律说**用户能观察到的后果**（记录没了、容器/文件没动），
// 不再说"已删除面板记录"这种内部说法（2026-09-21）。
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

// recordOnlyModal 是"卸载失败"时的兜底对话框：把原因说清，并给一个**可直接点**的
// 「从面板移除该服务」出口。卸载失败不能只丢一句 toast 就完了（用户没有下一步）。
//
// 注意（2026-09-21）：这条出口现在也会**先停服务**（DELETE /services/{name} 的
// 语义已改），停不掉且仍在运行时会拒绝移除 —— 绝不再制造"用户看不到却还在跑"。
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

// forgetButton 「从面板移除该服务」：面板不认识这条服务时的唯一收尾动作。
//
// 2026-09-21 用户明确要求（原话："移除却不卸载是什么意思 …… 让用户看不到却持续
// 运行"）：
//   · 目录里的应用（brew 原生 / 面板安装器 / compose）**不再**出现这颗按钮 ——
//     它们只有「🗑 卸载」，而且是真的卸载；
//   · 只有"面板不认识、也不知道怎么卸载"的服务才允许只删记录；
//   · 只删记录也**必须先停服务**（后端 DELETE /api/v1/services/{name} 已改成
//     先停再删，停不掉且仍在运行会 409 拒绝），绝不留下一个还在运行、
//     面板里却看不到的服务；
//   · 文案逐字说明"软件仍在磁盘上，需要你自己卸载"。
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
        // 卸载是异步任务（可能跑几分钟）：进度与结果在任务中心的进度窗里看。
        //
        // 必须 await 这次提交：提交本身没成功（后端拒绝 / 网络不通 / 请求被挂住）
        // 时 taskCenter.start 会弹出**带原因**的 toast 并返回 null ——
        // 那种情况下既不能再触发 onDone（会刷出一个"什么都没发生"的界面），
        // 更不能沉默。写操作的失败必须可见，这条是本 bug（坑 154）的教训。
        //
        // 2026-09 追加：卸载失败（尤其 compose 运行时被删掉）**必须给出下一步** ——
        // 弹一个带「只删除记录」按钮的对话框，而不是只留一句错误让用户卡死。
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
        // 兜底：确认框/渲染层自己抛异常时也必须说话 —— async 点击处理器里的异常
        // 会变成 unhandled rejection，用户那边就是"点了没反应"。
        toast('卸载「' + label + '」失败：' + ((e && e.message) || e), 'err', 12000);
      }
    },
  });
}

// confirmUninstallPlan 弹出"这次卸载会做什么"的确认框。
// 返回 Promise<{wipe:boolean, force:boolean}|null>：null = 取消/关掉（什么都没做），
// 否则 wipe 表示用户有没有勾"同时删除数据/产物"，force 表示有没有选"强制卸载"。
//
// 关于 force（2026-09-21 用户真机）：卸载 python@3.13 时 brew 以
// "because it is required by llvm and rust" 拒绝卸载，而面板以前只把这段英文
// 原文贴回来 —— 用户既看不懂，也不知道还能怎么办。现在计划阶段就把依赖方列出来，
// 这里给出**两个明确选项**：取消（默认什么都不做）与
// 「强制卸载（brew uninstall --ignore-dependencies …，会破坏 llvm、rust）」。
// force 只能在计划里 force_allowed=true 时出现；它**默认不勾选**，
// 必须用户主动勾上（或点强制按钮）才会带出去。
//
// ⚠️ 这个函数是从一个真机事故里长出来的（2026-09-21 用户原话："frpc 点击卸载
// 没有任何反应，没有进度，没有提示，什么都没有"）。根因不是后端：原来这两处
// （apps.js 的残留清理、本文件的 marketUninstallButton）各写了一份一模一样的
// 确认框，确认按钮都是：
//
//     onclick: () => { close(); resolve(true); }
//     ...
//     onClose: () => resolve(false)
//
// 而 modal 的 close() 会**同步**调用 onClose —— 于是 resolve(false) 先执行，
// Promise 被定死为 false，`if (!okGo) return;` 直接返回：对话框关掉了，
// 却什么都没发生（没有请求、没有进度、没有错误提示）。这正是用户报的形态。
//
// 现在只有这一份实现，并且用 done 闸门保证"谁先定稿谁算数"（与 ui.confirmBox
// 同一套做法）。卸载确认只有一处，不会再出现第二份写错的拷贝。
export function confirmUninstallPlan({ name, plan = {}, residual = false, forceDefault = false }) {
  return new Promise((resolve) => {
    let done = false;
    // 勾选框放在**这一份**实现里，并由 finish 把结果带回调用方 ——
    // 调用方不再自己渲染第二个确认框（那正是原来出错的形态）。
    const remove = h('input', { type: 'checkbox' });
    // 强制卸载：只在计划明确允许时出现（默认不勾）。
    const forceAllowed = !residual && plan.force_allowed === true;
    // forceDefault：用户是**从「强制卸载」那颗按钮进来的** → 预勾选，按钮文字也直接
    // 显示成「强制卸载」。多这一道确认是刻意的：强制卸载会破坏别的包（例如 llvm/rust），
    // 必须让用户在**看得见后果**的那一屏再点一次。
    const forceBox = h('input', { type: 'checkbox', checked: forceAllowed && forceDefault });
    const forceDeps = (plan.dependents || []).filter((d) => d.kind === 'brew').map((d) => d.name);
    const forceList = forceDeps.length ? forceDeps.join('、') : '依赖它的包';
    // data-confirm-uninstall 是自动化测试的稳定锚点（uitest 用它确认"真的弹了确认框、
    // 且确认之后真的会发请求"）。没有它，测试只能靠文案猜，改一次文案就假失败。
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
    // finish 在 m 赋值之后才会被调用（按钮/关闭都发生在用户交互时），
    // 所以这里引用 m 是安全的；done 闸门保证 close() 触发的 onClose 不会
    // 覆盖用户真正点下的那个结果。
    function finish(v) {
      if (done) return;
      done = true;
      m.close();
      resolve(v);
    }
  });
}

// dependentsModal 是"还有东西在用它"的说明对话框。
//
// 用户 2026-09-21 明确要求：卸载有依赖时**不要只弹一句 toast**，要逐条列出
// "谁在用它、该怎么办"，并给一个能直接去处理的入口：
//   · 站点正在用这个 PHP 版本      → 去「网站管理」切版本
//   · 有应用/容器在用（TTS、容器） → 去「应用」卸载/停止
//   · 面板自身的依赖               → 说明后果
//   · **Homebrew 包依赖它**        → 额外给「强制卸载」这颗按钮（见 onForce）
//
// onForce 非空 = 这个计划允许强制卸载（brew --ignore-dependencies）。此时页脚是
// 两个明确选项：取消（默认）/ 强制卸载；forceText 逐字写清会破坏哪些包。
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
      // 强制卸载的后果：**逐字**写在正文里（按钮文字也要一致），
      // 让用户在按下去之前就知道会破坏哪些包。
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

// marketUninstallButton 从市场卸载（brew 原生 / 面板自研安装器 / compose 应用走这里）。
// 确认框逐条列出会做什么、以及可选的"同时删除数据"路径 —— 卸载不可逆。
//
// svc 是可选的**服务记录**：卸载失败时用它做"从面板移除记录"的兜底出口
// （记录名/展示名以记录为准；没有记录时没有可删的记录，就只如实报错）。
// isLimitTunable 判断这个应用是不是"与上传/执行上限有关"（nginx / PHP）。
//
// 判据来自**目录数据**（id / brew_formula / service_label / name），不写死某一台机器
// 上的条目名：nginx 的请求体上限、PHP 的上传与执行上限都在它们的配置里。
// 其它应用（frpc / miniflux…）没有这两组上限，给了按钮只会让人困惑。
// isNginxApp 判断是不是 nginx 条目（判据来自目录数据，不写死名字）。
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
  // listPlan 是**列表里的**计划：它刻意**没有查过依赖**（查依赖要跑真的 brew，
  // 每个条目约 0.4s，36 条就是 15 秒冷启动 —— 用户看到的是"正在读取应用目录…"
  // 卡住）。真正的依赖判定在用户点「卸载」时按需查，见下面的 onclick。
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
          // 卸载失败时给出下一步。只有确实存在服务记录时才提供 ——
          // 没有记录就没有可删的东西。
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

  // 按钮**刻意不设 disabled**（也刻意不设 aria-disabled：那会让辅助技术与自动化
  // 把它当成不可用，点都点不到）。按钮保持可用，点下去用 toast 说明为什么
  // 现在不能卸载 —— disabled 的按钮点下去什么都不发生，原因还只在悬浮提示里，
  // 用户看到的就是"点了没反应"。唯一短暂禁用是下面"检查依赖…"的那半秒，
  // 而且按钮上**看得见**正在做什么。
  const btn = h('button.btn.btn-sm.btn-danger', {
    text: label,
    title: residual ? '只删除磁盘上的残留产物/数据' : (listPlan.blocked || '卸载「' + mi.name + '」'),
  });
  btn.onclick = async () => {
    // 1) 先把**完整**计划取回来（含依赖检测）。这一步失败**不阻断**卸载：
    //    按列表里那份（未查依赖）继续，但如实说明"没能检查依赖"。
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
    // 2) 计划被 blocked（例如"还有 1 个 Docker 应用在用这个运行时"）时保持可点：
    //    点下去把原因说出来，而不是静默。
    const blocked = !!plan.blocked && !residual;
    // 被 **Homebrew 依赖**拦下、且计划允许强制时（force_allowed），用户有第二条路：
    // brew uninstall --ignore-dependencies。它只在用户明确选择时才走。
    const canForce = blocked && plan.force_allowed === true;
    const forceDeps = (plan.dependents || []).filter((d) => d.kind === 'brew').map((d) => d.name);
    const forceText = plan.force_note
      || ('强制卸载会破坏这些包：' + (forceDeps.join('、') || '依赖它的包')
        + '（它们会缺依赖、可能无法运行）。命令：brew uninstall --ignore-dependencies '
        + (plan.formula || mi.id));
    try {
      if (blocked) {
        // 有结构化依赖 → 弹说明对话框（逐条列出"谁在用它、该怎么办"）；
        // 允许强制时（brew 依赖）同时给出「强制卸载 / 取消」两个明确选择。
        if ((plan.dependents || []).length || canForce) {
          dependentsModal({
            name: mi.name, plan, onDone,
            // 强制卸载**不直接提交**：先弹"将执行…+强制开关已勾选"的确认框，
            // 用户在那屏上再点一次才真的发请求（极端破坏性动作要两道门）。
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
