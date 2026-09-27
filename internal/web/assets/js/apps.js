// apps.js —— 「应用」页：四个一级 Tab（已安装 / 应用市场 / docker / 一键建站）。
//
// 2026-09-17 用户第二版信息架构（原话："做 4 个一级菜单：已安装，应用市场，
// docker，一键建站；默认显示已安装；应用市场里的显示始终显示全部……恢复之前的
// 卡片展示样式！重要！"）：
//   · 「已安装」（默认）—— 市场已安装条目 + 服务记录按归一化 key 合并去重后，
//     **一张卡片一个应用**（实现住在 services.js 的 renderInstalledApps）。
//     卡片上只留一颗「打开」（规则见下），启停/重启/刷新/管理保留。
//   · 「应用市场」—— **一个列表显示全部**可安装的原生应用（brew / 官方二进制 /
//     面板安装器），**不再**按已安装/未安装分成两部分，也不再默认只显示未安装。
//     docker 类与站点应用不在这里（各有自己的 Tab）。
//   · 「docker」—— 纯展示面板建议的 docker 项目：默认端口 + 需要的镜像 +
//     预配置 compose 文件（含随机密钥与端口说明）。**不提供任何安装动作**。
//   · 「一键建站」—— site_app 类条目（typecho / wordpress / freshrss），
//     表单与流程与上一轮完全一致。
//   侧栏仍然只有「应用」一项；hash 能直接指定 Tab（`#/apps/docker`），
//   老 hash `#/services` 落到「已安装」（见 app.js 的 ROUTE_TARGET / routeFor）。
//
// 卡片样式：四个 Tab 全部走 `.grid.grid-3` + servicePanel.appCardShell ——
// 就是改动前应用市场那套卡片（图标 + 名称 + summary + pill + 一排按钮）。
// 合并页面那一轮换成的"行"（services.js 的 appRow）已按用户要求删除。
//
// 市场这一侧的核心思路不变：把"能不能装"和"装什么"分开。
//   - 用户点安装时，先跑一次安装前检查（依赖、端口），把问题一次说清
//   - 检查通过才真正安装；安装是**异步任务**，提交后进度交给「任务中心」，
//     用户可以关窗口、切页面，随时从顶栏重新打开看进度（见 tasks.js）
//   - 已经装过的应用显示「已安装」pill + 「打开」，常用动作（打开/启停/重启/刷新/
//     管理）直接摆在卡片上；安装生命周期动作（重装 / 卸载 / 文档 / 直链）收进
//     「⚙️ 管理」面板

import { api } from './api.js';
import { h, clear, toast, modal, appendAll, confirmBox } from './ui.js';
import { registerCleanup } from './app.js';
import { taskCenter } from './tasks.js';
// 「已安装」Tab 的实现（合并去重清单、一张卡片一个应用）住在 services.js ——
// 它是原服务管理页的继承者，数据字段（服务记录）也主要来自那一侧。
import { renderInstalledApps } from './services.js';
// 「应用管理」面板、卡片外壳、唯一那颗「打开」与启停/重启的唯一实现都在
// servicePanel.js —— 四个 Tab 的卡片与「已安装」卡片点开的是**同一个**面板
// （用户 2026-09-16 的核心要求：同一个应用的能力不分散在两个页面）。
//
// appCardShell  四个 Tab 共用的卡片 DOM（用户要求"卡片样式统一"）
// openOrRepairActions / openTargetOf
//               卡片上「打开 / 重新部署」的**唯一**判定（入口只有一颗「打开」）
// dedupeMarketEntries 市场/docker 列表按归一化 key 去重（与「已安装」的去重同一套规则）
// hasPanelUI    与面板同一条"有没有面板托管的界面"判据
import {
  openServicePanel, marketQuickActions, hasPanelUI, appCardShell,
  openOrRepairActions, dedupeMarketEntries, appKeyOf,
  // 卸载确认只有一份实现（servicePanel.confirmUninstallPlan）：这里曾经抄过
  // 一份，而那份的确认按钮 `close(); resolve(true)` 会被 modal 的 onClose
  // 里的 resolve(false) 抢先定稿 —— 用户点「确认卸载」后什么都不发生。
  confirmUninstallPlan,
} from './servicePanel.js';
// 网络失败判据与统一文案（唯一前端真源，见 netfail.js；后端同名判据见 services/netfail.go）。
import { isNetworkFailureText, networkHintText, networkHintBlock } from './netfail.js';

// NET_HINT_ENTRIES 是本页"去哪改镜像/代理"的入口（各页入口不同）。
const NET_HINT_ENTRIES = '面板设置 → 访问与安全 →「应用包镜像基址」；Docker →「加速源」；'
  + '面板设置 →「检查更新」→「升级源地址」。';
const netHintBlock = (errText) => networkHintBlock(errText, NET_HINT_ENTRIES);

let cache = null;
// svcList 是这轮市场数据对应**完整服务记录**（api.services(true)，带健康检查）。
// 「已安装」Tab 用它做合并去重、状态/端口/健康，以及管理面板的 svc 入参。
let svcList = [];
// proxyState 是 /api/v1/market/proxies 的探测结果（slug → {proxy_ok, reason}）。
// 它只服务于「应用市场」顶部「检测可用性 / 生成 nginx 入口」两个工具，**不再**
// 决定「打开」的地址：那个探测不带面板会话，子路径在面板端口上返回 401，
// proxy_ok 几乎恒为 false。用它决定地址会让 it-tools 这类应用的「打开」错变成端口直连。
let proxyState = null;
// svcState 是"这轮市场数据对应的服务状态"（服务名 → state），由 svcList 派生。
// 市场卡片的首颗动作按钮要在「启动」与「停止」之间选一个；拿不到状态时退化成
// 「启动」（后端对一个已经在跑的服务执行 start 是幂等的，不会因此谎报状态）。
let svcState = {};

// ---------------------------------------------------------------------------
//  市场数据缓存（用户 2026-09-22：打开这一页直接读缓存，只有点「⟳ 更新」才刷新）
// ---------------------------------------------------------------------------
//
// 为什么落 sessionStorage：内存里的 cache 能覆盖面板内切页，但**整页刷新/重开浏览器**
// 就没了 —— 那正是"应用市场打开慢"的场景。缓存只存上一轮探测的结论，界面必须如实
// 标出它有多旧（见 renderTabBar 的「缓存 · N 分钟前」），绝不假装实时。
const MARKET_CACHE_STORE = 'zp.market.dataCache';

// loadDataCache 读回上一次的 {market, services, at}；形状不对一律当没有（不猜）。
function loadDataCache() {
  try {
    const raw = sessionStorage.getItem(MARKET_CACHE_STORE);
    const o = raw ? JSON.parse(raw) : null;
    if (!o || typeof o !== 'object' || !o.market || !Array.isArray(o.market.list)) return null;
    return { market: o.market, services: Array.isArray(o.services) ? o.services : [], at: String(o.at || '') };
  } catch (e) {
    return null;
  }
}

// saveDataCache 把这一轮结论落盘；存不下（隐私模式/配额满）只在本次会话生效，不影响结论。
function saveDataCache() {
  if (!cache) return;
  try {
    sessionStorage.setItem(MARKET_CACHE_STORE, JSON.stringify({
      market: cache, services: svcList, at: cacheAt || new Date().toISOString(),
    }));
  } catch (e) { /* 存不了就算了 */ }
}

// cacheAt 是这一轮结论的取得时刻（缓存年龄照它算）。
let cacheAt = '';
// preheat 是登录预热正在进行的那次"拉齐数据"（见 bootstrapAppUpdates）。
// 进度：预热与用户进页面撞在一起时，页面**等它**而不是再发一遍相同的两个请求。
let preheat = null;
// 进页面时先恢复上一次的缓存：有它就直接渲染（不发请求）。
const restoredMarketCache = loadDataCache();
if (restoredMarketCache) {
  cache = restoredMarketCache.market;
  svcList = restoredMarketCache.services;
  cacheAt = restoredMarketCache.at;
}

// ---------------------------------------------------------------------------
//  「有可更新应用」的常驻提醒 + 后台定时检查（用户 2026-09-22 要求）
// ---------------------------------------------------------------------------
//
// 用户原话："后台就在固定时间主动检查一次，如有更新，显著提醒。如 1h 一次。"
// 所以：① 计数写 localStorage（刷新页面后侧栏徽标仍在）；② 每小时**真查**一次（fresh=1，
// 不是读缓存 —— 读缓存的后台检查没有意义）；③ 有更新就派发事件让外壳显示侧栏徽标，
// 应用页顶部再常驻一条横幅；④ 新发现时补一条 toast。
const APP_UPDATE_STORE = 'zp.appUpdates';      // {count, checked_at}
const APP_UPDATE_INTERVAL_MS = 60 * 60 * 1000; // 1 小时
const APP_UPDATE_FIRST_DELAY_MS = 3000;        // 进面板先查一次（延迟 3s，别跟首屏抢）

function loadAppUpdateCount() {
  try {
    const o = JSON.parse(localStorage.getItem(APP_UPDATE_STORE) || 'null');
    return o && typeof o.count === 'number' ? o.count : 0;
  } catch (e) {
    return 0;
  }
}

function saveAppUpdateCount(count) {
  try {
    localStorage.setItem(APP_UPDATE_STORE, JSON.stringify({ count, checked_at: Date.now() }));
  } catch (e) { /* 存不下：本次会话的事件仍然生效 */ }
  try {
    // 外壳（app.js）监听这个事件更新侧栏徽标 —— 两个模块不互相 import。
    window.dispatchEvent(new CustomEvent('zp:app-updates', { detail: { count } }));
  } catch (e) { /* 事件不可用就等下次渲染 */ }
}

// applyUpdatesResponse 把批量结论并进 updateChecks（两个子 Tab 共用同一份）。
// 目标里**没有出现在 items** 的条目 = 面板拿不到可比版本 ⇒ 如实记 unknown，
// 绝不写成"已是最新"。
function applyUpdatesResponse(resp, ids) {
  const items = (resp && resp.items) || {};
  const at = (resp && resp.checked_at) || new Date().toISOString();
  for (const id of ids) {
    if (items[id] && typeof items[id] === 'object') {
      updateChecks[id] = items[id];
      continue;
    }
    updateChecks[id] = {
      installed: '', latest: '', update_available: false, unknown: true, checked_at: at,
      error: '面板现在拿不到这个应用的可比版本（它没有版本真源）',
    };
  }
}

let appUpdateTimer = null;
let appUpdateInflight = false;
let appUpdateAnnounced = 0;

// runBackgroundUpdateCheck 后台主动查一次（定时器用，与任何视图无关）。
//
// 数据源是**缓存里的市场列表**（模块级 loadDataCache），所以即使当前不在「应用」页、
// 或者用户已经切走，它照样能工作。
async function runBackgroundUpdateCheck() {
  // 与页面自己的那一轮探测互斥：两轮同时打后端只会重复跑一次 brew outdated。
  if (appUpdateInflight || updateInflight) return;
  const data = loadDataCache();
  const list = (data && data.market && Array.isArray(data.market.list)) ? data.market.list : null;
  if (!list) return; // 还没拉过市场列表：等下一轮（面板刚启动时可能还没人打开过应用页）
  const targets = list.filter((a) => a && a.id && a.supports_update_check && isInstalled(a));
  if (!targets.length) {
    saveAppUpdateCount(0);
    return;
  }
  appUpdateInflight = true;
  try {
    const resp = await api.marketUpdates(true);
    applyUpdatesResponse(resp, targets.map((a) => a.id));
    saveUpdateCheckStore();
    const pending = targets.filter(updatePendingOf);
    saveAppUpdateCount(pending.length);
    if (pending.length > appUpdateAnnounced) {
      appUpdateAnnounced = pending.length;
      const names = pending.slice(0, 3).map((a) => a.name).join('、');
      toast('发现 ' + pending.length + ' 个应用有新版本：' + names + (pending.length > 3 ? ' 等' : ''),
        'info', 12000);
    }
    try { window.dispatchEvent(new CustomEvent('zp:app-update-checked', { detail: { count: pending.length } })); } catch (e) { /* 忽略 */ }
  } catch (e) {
    // 查不成**不动计数**：把"没查成"显示成"没有更新"就是谎报。
  } finally {
    appUpdateInflight = false;
  }
}

// lastBackgroundCheckAt 读上一次"真的查成了"的时刻（0 = 还没成功过）。
function lastBackgroundCheckAt() {
  try {
    const o = JSON.parse(localStorage.getItem(APP_UPDATE_STORE) || 'null');
    return (o && typeof o.checked_at === 'number') ? o.checked_at : 0;
  } catch (e) {
    return 0;
  }
}

// bootstrapAppUpdates 是**外壳在登录后（含刷新页面）调用的一次性预热**。
//
// 用户 2026-09-22 的三条报障都落在这里：
//   ① "不访问应用版块就永远不提示" —— 检查原来挂在应用页的挂载里；
//   ② "登入后后台就该检测一次，并生成缓存，点击软件市场秒进" —— 没有缓存时先拉一次
//      市场列表（缓存 = 秒进；同时它也是更新检查判断"装了哪些应用"的输入，原来没缓存
//      就直接跳过检查，于是不打开应用页就永远没有提醒）；
//   ③ 检查完立刻把"有 N 个可更新"写进 localStorage + 派发事件，侧栏红点**不用等访问应用页**。
export async function bootstrapAppUpdates() {
  if (!loadDataCache()) {
    // ⚠️ 市场与服务记录必须**一起**就位再写进 cache（2026-09-22 真机撞到的竞态）：
    // 以前先 `cache = await api.market()`、再单独等服务记录，中间那段窗口里
    // `cache` 已非空而 `svcList` 还是空的 —— 用户这时（登录后立刻）进「应用 → 已安装」
    // 会看到**所有**应用的卡片都写「已安装（面板里暂无记录）」、按钮全是「启动」，
    // 而且不会自己变回来（页面只在 cache 为空时拉数据，预热完成不重画）。
    // 现在两者都拿到才赋值，页面要么读到完整数据、要么走自己的 fetchAll。
    preheat = (async () => {
      const [mkt, svc] = await Promise.allSettled([api.market(), api.services(true)]);
      if (mkt.status !== 'fulfilled') throw mkt.reason;
      cache = mkt.value;
      if (svc.status === 'fulfilled') svcList = (svc.value && svc.value.list) || [];
      cacheAt = new Date().toISOString();
      saveDataCache();
    })();
    try {
      await preheat;
    } catch (e) {
      // 市场列表都拿不到就不猜"有没有新版"：等心跳或用户自己打开应用页。
      return;
    } finally {
      preheat = null;
    }
  }
  startAppUpdateWatcher();
  await runBackgroundUpdateCheck();
}

// startAppUpdateWatcher 只起一次定时器（模块级，与当前在哪个页面无关）。
//
// 心跳 30 秒、但**只在距上次成功检查超过 1 小时时才真查**。为什么不用
// `setInterval(run, 1h)`：那一轮可能正好撞上页面自己的探测而被互斥保护跳过，
// 于是这次后台检查被推迟整整一小时 —— 用户会以为"后台检查没生效"。
function startAppUpdateWatcher() {
  if (appUpdateTimer) return;
  // 首次：3s 后试；若正好撞上页面自己那轮探测（互斥保护会跳过），5s 后再试，
  // 最多 6 次 —— 不让"有新版"的提醒因为一次撞车而拖到 30s 心跳。
  let tries = 0;
  const first = () => {
    if (appUpdateInflight || updateInflight) {
      if (++tries < 6) setTimeout(first, 5000);
      return;
    }
    runBackgroundUpdateCheck();
  };
  setTimeout(first, APP_UPDATE_FIRST_DELAY_MS);
  appUpdateTimer = setInterval(() => {
    if (Date.now() - lastBackgroundCheckAt() < APP_UPDATE_INTERVAL_MS) return;
    runBackgroundUpdateCheck();
  }, 30 * 1000);
}

// ---------------------------------------------------------------------------
//  一级 Tab（用户 2026-09-17：已安装 / 应用市场 / docker / 一键建站）
// ---------------------------------------------------------------------------
//
// 数组顺序就是默认顺序：`已安装` 在最前，AppsView 在 ctx.tab 没给或给了未知值时
// 落回它。每个 Tab 有稳定的 data-tab 值（也是 hash 里的第二段：`#/apps/docker`）。
const APP_TABS = [
  { id: 'installed', title: '已安装' },
  { id: 'market', title: '应用市场' },
  { id: 'docker', title: 'docker' },
  { id: 'sites', title: '一键建站' },
];
const APP_TAB_IDS = new Set(APP_TABS.map((t) => t.id));

// ---------------------------------------------------------------------------
//  docker 推荐项目：字段判定（字段名以后端接口为准，见下）
// ---------------------------------------------------------------------------
//
// 后端（internal/web/api_services.go 的 market item + services.App）给的稳定契约：
//   · docker_reference（App 上的字段）/ docker_recommended（market item 的别名）为 true
//     → 这是"面板推荐的 Docker 项目"：卡片进 docker Tab，**不给安装动作**；
//   · compose_yaml           → 预配置 compose 文件的内容（卡片上「复制 compose 配置」）；
//   · compose_url            → 镜像站上同一份文件的下载/查看地址（卡片上「打开 compose 文件」）；
//   · compose_env_url        → 变量样例 .env.example 的地址（卡片上「变量样例文件 ↗」）；
//   · compose_readme_url     → 全部推荐项目的总索引（顶部提示里的链接）。
// 这里仍然多认几个历史/备用写法（docker_rec / recommended_docker / compose /
// compose_file_content …），是为了**接口字段变更时不至于把条目漏出 docker Tab**；
// 兜底再按 kind=compose/docker 归类（当前目录里所有 compose 条目都是推荐项目，
// 有 Go 侧测试锁住）。**不含 colima** —— 那是 Docker 运行时本身，要留在应用市场
// 让人能安装，不是"推荐项目"。
function isDockerRec(a) {
  if (!a) return false;
  for (const k of ['docker_reference', 'docker_recommended', 'docker_rec', 'recommended_docker']) {
    if (typeof a[k] === 'boolean') return a[k];
  }
  if (a.docker === true) return true;
  return a.kind === 'compose' || a.kind === 'docker';
}

function composeTextOf(a) {
  return (a && (a.compose_yaml || a.compose || a.compose_content || a.compose_file_content)) || '';
}

function composeURLOf(a) {
  return (a && (a.compose_url || a.compose_file_url || a.compose_download_url || a.compose_yaml_url)) || '';
}

function composeEnvURLOf(a) {
  return (a && (a.compose_env_url || a.env_url)) || '';
}

// composeReadmeURLOf 取"全部推荐项目的总索引"地址：后端在每条推荐项目上都带同一个值，
// 取第一条非空的即可（docker Tab 顶部提示里给一个总入口）。
function composeReadmeURLOf(list) {
  for (const a of list || []) {
    const u = a && (a.compose_readme_url || a.compose_index_url);
    if (u) return u;
  }
  return '';
}

// dockerImagesOf 取"这个项目需要哪些镜像"：接口给的清单优先，
// 没有就从 compose 内容的 image: 行解析（面板只展示，不替用户判断镜像版本）。
function dockerImagesOf(a) {
  const direct = a && (a.images || a.docker_images || a.required_images);
  if (Array.isArray(direct) && direct.length) return direct.map((x) => String(x));
  const out = [];
  const re = /^\s*image:\s*["']?([^"'\s#]+)/gm;
  let m;
  while ((m = re.exec(composeTextOf(a))) !== null) {
    if (!out.includes(m[1])) out.push(m[1]);
  }
  return out;
}

// isInstalled 是"已安装"的唯一判据（市场卡片 pill、安装按钮共用）。
// installed=true 是"磁盘上装了/launchd 里有"；adopted=true 是"面板里还有它的
// 服务记录"（内部概念）。两者对用户都是"已经装好了"，所以合并成同一个判断。
function isInstalled(a) { return !!(a && (a.installed || a.adopted)); }

// versionPillOf 给"本机装着的版本"一枚 pill（拿不到就不给）。
//
// 数据来自后端 installed_version：brew 类现查 `brew list --versions`，其余轨来自
// 安装时记录（见 services.Manager.RecordInstalledVersion）。**空就是空** ——
// 面板没有版本真源时如实不显示，绝不编一个版本号（目录声明版本不是本机事实）。
function versionPillOf(a) {
  const v = String((a && a.installed_version) || '').trim();
  if (!v) return null;
  return h('span.pill', { text: v, title: '本机装着的版本（brew 类现查 brew list --versions；其余来自安装时记录）' });
}

// ---------------------------------------------------------------------------
//  市场卡片的「更新检查」（按需探测 + sessionStorage 缓存）
// ---------------------------------------------------------------------------
//
// 只在 supports_update_check 且**已安装**的条目上按需打 update-check：昂贵探测绝不进
// 列表/首屏（AGENTS 第三节 7）。sessionStorage 让同一次会话刷新页面不重复探测、不抖顺序。
// TTL 10 分钟 > 后端缓存的 9 分钟：到期再问时后端一定已过期，不会把旧 checked_at 发回来。
const UPDATE_CHECK_TTL_MS = 10 * 60 * 1000;
const UPDATE_CHECK_STORE = 'zp.market.updateCheck';

// updateChecks: 应用 ID → {installed, latest, update_available, unknown, checked_at, error}
// updateChecking: 应用 ID → true（这个条目正在探测，卡片上显示"检查更新中…"）
// updateInflight: 正在跑的那一轮探测（Promise）；页面切走再回来时接在它后面重画，
// 不会因为"上一轮还没回来"而永远看不到徽标。
let updateChecks = loadUpdateCheckStore();
let updateChecking = {};
let updateInflight = null;

function loadUpdateCheckStore() {
  try {
    const raw = sessionStorage.getItem(UPDATE_CHECK_STORE);
    const obj = raw ? JSON.parse(raw) : null;
    return (obj && typeof obj === 'object') ? obj : {};
  } catch (e) {
    return {};
  }
}

function saveUpdateCheckStore() {
  try {
    sessionStorage.setItem(UPDATE_CHECK_STORE, JSON.stringify(updateChecks));
  } catch (e) { /* 隐私模式/配额满：只是不缓存，不影响本次结论 */ }
}

// updateCheckFresh 判断一条结论是否在 TTL 内（判据是后端给的 checked_at）。
function updateCheckFresh(c) {
  if (!c || !c.checked_at) return false;
  const at = Date.parse(c.checked_at);
  return Number.isFinite(at) && (Date.now() - at) < UPDATE_CHECK_TTL_MS;
}

// updateCheckOf 取这个条目**可信且未过期**的检查结论（没有/过期 → null）。
function updateCheckOf(a) {
  if (!a || !a.id) return null;
  const c = updateChecks[a.id];
  return updateCheckFresh(c) ? c : null;
}

// updatePendingOf 是"徽标 + 置顶"的唯一判据：只有**确定**有更新才算。
// unknown（索引不可达/拿不到期望值）一律不算 —— 绝不能把"不知道"说成"有新版"。
function updatePendingOf(a) {
  const c = updateCheckOf(a);
  return !!(c && c.update_available === true && !c.unknown);
}

// invalidateUpdateCheck 让某个条目的结论立刻失效（安装/更新完成后调用）。
function invalidateUpdateCheck(id) {
  if (!id) return;
  delete updateChecks[id];
  saveUpdateCheckStore();
}

export function AppsView(content, ctx = {}) {
  clear(content);

  // 默认落在「已安装」；`#/services`（app.js 的 ROUTE_TARGET）显式给 tab='installed'，
  // `#/apps/docker` 这类 hash 给 tab='docker'。未知值一律落回「已安装」。
  let active = (ctx && APP_TAB_IDS.has(ctx.tab)) ? ctx.tab : 'installed';
  let loadError = null;
  let marketGrid = null;
  let marketHead = null;
  // netFailure：最近一次安装/探测失败的原文，**仅当判定为网络问题时**才存。
  // 存下来是为了在本页内容最上方常驻一条醒目的网络提示（见 renderBody）。
  let netFailure = '';
  // alive：本页还挂着吗 —— 按需的更新检查是异步的，回来时页面可能已经切走，
  // 不许再往卸掉的 DOM 上写东西。
  let alive = true;
  registerCleanup(() => { alive = false; });
  // 后台定时检查完成（可能是别的视图起的定时器）→ 本视图跟着重画；切走就退订。
  const onAppUpdateChecked = () => { if (alive) { renderTabBar(); renderBody(); } };
  window.addEventListener('zp:app-update-checked', onAppUpdateChecked);
  registerCleanup(() => window.removeEventListener('zp:app-update-checked', onAppUpdateChecked));

  const tabBar = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '14px', flexWrap: 'wrap' } });
  const body = h('div');
  appendAll(content, tabBar, body);

  function renderTabBar() {
    clear(tabBar);
    for (const t of APP_TABS) {
      tabBar.appendChild(h(`button.btn.btn-sm${active === t.id ? '.btn-primary' : ''}`, {
        dataset: { tab: t.id },
        text: t.title,
        onclick: () => { if (active === t.id) return; active = t.id; renderTabBar(); renderBody(); },
      }));
    }
    // 缓存年龄：如实说明这一屏是缓存（打开即显示、不发请求）以及它有多旧。
    // 没有这条提示，用户会把"缓存里的已安装/健康状态"当成此刻的真实状态。
    if (cache) {
      const age = cacheAgeText();
      tabBar.appendChild(h('span.pill', {
        text: age ? '缓存 · ' + age : '缓存',
        title: '打开这一页直接读本地缓存，不发请求；只有点「⟳ 更新」才重新探测。'
          + (age ? '\n\n这份数据是 ' + age + '的。' : ''),
      }));
    }
    if (refreshing) {
      tabBar.appendChild(h('span.pill.warn', {
        text: refreshPhase || '更新中…',
        title: '正在重跑本机探测（brew/docker）并检查有没有新版',
      }));
    }
    // 后端这次**没能复核**「本机装了哪些 Homebrew 包」（brew_probe_ok=false）时，
    // 如实说出来：列表里那些「安装」只代表"没查成"，不代表东西真的没装
    //（铁律 11：不许把"不知道"显示成"没有"）。没有这条提示，一次 brew 超时
    // 就会让用户以为自己的软件全被卸了 —— 那正是那一类报障。
    if (cache && cache.brew_probe_ok === false) {
      const why = String(cache.brew_probe_error || '').trim();
      tabBar.appendChild(h('span.pill.warn', {
        text: '⚠ 未能复核已装软件',
        title: '这次没能读到 Homebrew 的已装清单：'
          + '下面标着「安装」的应用可能其实已经装着。点「⟳ 更新」重试。'
          + (why ? '\n\n真实原因：' + why : ''),
      }));
      // 把真实原因**显示出来**（不只塞进 title）：2026-09-19 用户报障时界面上
      // 只有一句"brew 不可用或超时"，谁也不知道到底是 brew 坏了、权限问题还是超时。
      // 原因是网络时额外给醒目 pill（这条路径既可能是 brew 坏了，也可能是网络失败）。
      if (why) {
        if (isNetworkFailureText(why)) {
          tabBar.appendChild(h('span.pill.danger', {
            text: '🌐 网络问题',
            title: why,
          }));
        }
        tabBar.appendChild(h('span.hint', {
          text: '原因：' + (why.length > 140 ? why.slice(0, 140) + '…' : why),
          title: why,
        }));
      }
    }
  }

  function renderBody() {
    clear(body);
    // 有网络失败时，先在最上方摆出醒目提示（pill + 能照做的镜像入口 + 原文）。
    // 放在内容之前：用户打开这一页第一眼就知道"是网络，不是功能坏了"。
    if (netFailure) body.append(netHintBlock(netFailure));
    // 「有可更新应用」的常驻横幅（用户 2026-09-22：后台每小时检查，有更新要**显著提醒**）。
    // 放在内容之前、与网络提示同级；计数来自后台检查 + 手动更新的结论。
    const pending = pendingUpdates();
    if (pending.length) body.append(appUpdatesBanner(pending));
    if (active === 'installed') renderInstalledTab();
    else if (active === 'docker') renderDockerTab();
    else if (active === 'sites') renderSitesTab();
    else renderMarketTab();
  }

  // appUpdatesBanner 是"有新版"的显著提醒：常驻到更新做完为止，并给一个直达入口。
  function appUpdatesBanner(list) {
    const names = list.slice(0, 4).map((a) => a.name).join('、');
    return h('div#app-updates-banner', {
      style: {
        background: 'var(--warn-soft)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius)', padding: '12px 14px', marginBottom: '14px',
        fontSize: '12.5px', lineHeight: '1.8',
      },
    }, [
      h('div', { text: '🔔 有 ' + list.length + ' 个应用可以更新：' + names + (list.length > 4 ? ' 等' : '') }),
      h('div.hint', { text: '点右上角「⟳ 更新」重新检查；已安装的卡片上有「更新」按钮，点一下即可升级。' }),
      active === 'market'
        ? null
        : h('button.btn.btn-sm', {
          style: { marginTop: '6px' },
          text: '去「应用市场」看',
          onclick: () => { active = 'market'; renderTabBar(); renderBody(); },
        }),
    ]);
  }

  function renderInstalledTab() {
    if (loadError && !cache) {
      appendAll(body, h('div.card', [h('div.card-body', [h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: loadError.message || String(loadError) }),
      ])])]));
      return;
    }
    // 合并去重、卡片动作、状态筛选都在 services.js 的 renderInstalledApps 里
    // （它同时握着市场条目与服务记录，见那边文件头的说明）。
    renderInstalledApps(body, {
      market: cache,
      list: svcList,
      onReload: refreshSilently,
      // 「已安装」卡片复用**同一份**更新结论与同一套渲染（别新造一套）：
      // 用户默认落地的就是这个 Tab（2026-09-22 zizvideo 报障：有新版却看不见入口）。
      updateBadgeOf,
      updateAction,
      versionPillOf,
    });
  }


  // rebuildSvcState 从完整服务记录派生"服务名 → state"（市场卡片首颗按钮用）。
  function rebuildSvcState() {
    const next = {};
    for (const s of svcList) if (s && s.name) next[s.name] = s.state || null;
    svcState = next;
  }

  // fetchAll 一次拉齐两边的数据：市场目录 + 服务记录（带健康检查）。
  // 两者**并行**，且一个失败不影响另一个（服务记录拉不到时「已安装」只显示
  // 市场里的已安装条目，如实降级，不谎报）。
  //
  // 注意：这里用的是 api.services(true)（**带健康检查**，比市场自己以前用的
  // false 慢一点），因为「已安装」每张卡片要给出健康检查结果。两个请求并行发出，
  // 首屏等待没有叠加。
  async function fetchAll(opts = {}) {
    const [mkt, svc] = await Promise.allSettled([api.market(!!opts.fresh), api.services(true)]);
    if (mkt.status === 'fulfilled') {
      cache = mkt.value;
      loadError = null;
      cacheAt = new Date().toISOString();
      saveDataCache();
    } else if (!cache) {
      loadError = mkt.reason;
    }
    if (svc.status === 'fulfilled') svcList = (svc.value && svc.value.list) || [];
    rebuildSvcState();
  }

  // cacheAgeText 把缓存年龄说成人话（拿不到时间就返回空串，不编）。
  function cacheAgeText() {
    const t = Date.parse(cacheAt || '');
    if (!Number.isFinite(t)) return '';
    const mins = Math.max(0, Math.floor((Date.now() - t) / 60000));
    if (mins < 1) return '刚刚';
    if (mins < 60) return mins + ' 分钟前';
    const hours = Math.floor(mins / 60);
    return hours < 24 ? hours + ' 小时前' : Math.floor(hours / 24) + ' 天前';
  }

  // updateCheckTargets 是"这一轮该探测更新的条目"：市场列表全集（含 docker / 建站条目），
  // 再由 eligibleForUpdateCheck 筛（supports_update_check + 已安装 + 结论已过期）。
  // 关键：**与当前是哪个子 Tab 无关** —— 用户默认落在「已安装」，探测也必须照跑，
  // 否则那个 Tab 上的卡片永远等不到徽标（2026-09-22 的真实报障）。
  function updateCheckTargets() {
    return (cache && Array.isArray(cache.list)) ? cache.list : [];
  }

  // load 是进页面的入口：**有缓存就直接渲染，一个请求都不发**（缓存优先，2026-09-22 用户要求）。
  // 只有"从来没有缓存"（首次用面板/换了浏览器）才拉一次；此后一律等用户点「⟳ 更新」。
  async function load() {
    if (!proxyState) proxyState = { enabled: true, items: [], _stale: true };
    // 后台检查由外壳在登录后预热（bootstrapAppUpdates）；这里只挂"查完重画"的订阅。
    if (cache) {
      // 恢复出来的服务记录也要派生一次"服务名 → state"（卡片首颗按钮要它）。
      rebuildSvcState();
      renderTabBar();
      renderBody();
      // 缓存只省"列表数据"这一次请求；更新结论**每次进页面都问一次后端**
      //（后端自己 9 分钟缓存 + 安装/升级后失效）—— 与子 Tab 无关地跑，两个 Tab 共用结果。
      ensureUpdateChecks(updateCheckTargets(), afterUpdateChecks, false);
      return;
    }
    clear(body);
    appendAll(body, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取应用目录…（首次）' })]));
    // 登录预热可能正在拉同一份数据（见 bootstrapAppUpdates）：等它，别再发一遍
    // 完全相同的两个请求；预热失败（preheat 已清空）就自己拉。
    const inFlight = preheat;
    if (inFlight) { await inFlight.catch(() => {}); }
    if (!cache) await fetchAll();
    // 本地插件与目录一起预取：renderGrid 里那块插件是**同步**渲染的，
    // 不再"先画再补"（那样在慢机器上会看到分区里写着 0 条，过一会儿才变）。
    await loadPlugins();
    rebuildSvcState();
    renderTabBar();
    renderBody();
    // 首次加载没得读缓存，这一次探测顺带把更新检查也做了。
    ensureUpdateChecks(updateCheckTargets(), afterUpdateChecks, false);
  }

  // refreshing/refreshPhase 是「⟳ 更新」的进度状态：按钮与顶部都要**看得见**在做事
  // （用户 2026-09-22 报障："刷新点击后没有反应"—— 数据没变时尤其像没反应）。
  let refreshing = false;
  let refreshPhase = '';

  // refresh 是「⟳ 更新」的唯一入口（用户 2026-09-22：把"检查更新"合并进刷新、就叫「更新」）：
  // ① fresh=1 重拉列表（后端重跑 brew/docker 真实复核）
  // ② 强制重查一次更新（fresh=1）
  // ③ 一条汇总 toast，告诉用户到底做了什么、有没有新版。
  // 装/卸/启停之后的自动更新仍走 refreshSilently（后端在那条路径上已经失效过缓存）。
  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    refreshPhase = '正在刷新应用列表…';
    renderTabBar();
    if (active === 'market') renderHead();
    try {
      await fetchAll({ fresh: true });
      refreshPhase = '正在检查更新…';
      renderTabBar();
      if (active === 'market') renderHead();
      await ensureUpdateChecks(updateCheckTargets(), null, true);
    } finally {
      refreshing = false;
      refreshPhase = '';
    }
    renderTabBar();
    renderBody();
    const pending = pendingUpdates();
    saveAppUpdateCount(pending.length);
    toast('已更新应用列表：共 ' + ((cache && cache.list) ? cache.list.length : 0) + ' 个应用' +
      (pending.length ? '，其中 ' + pending.length + ' 个有新版' : '，这次没有检测到新版'),
      pending.length ? 'warn' : 'ok', 9000);
  }

  // pendingUpdates 是当前"确定有新版"的条目（唯一判据 updatePendingOf）。
  function pendingUpdates() {
    return updateCheckTargets().filter(updatePendingOf);
  }

  // stateOfApp 取这个应用在服务记录里的状态（给市场卡片上的启停按钮用）。
  //
  // 键要把三种写法都试一遍：面板记录名不一定是目录 ID（frpc 的记录名是
  // com.zizdog.frpc），与后端 FindAppByService 认的写法保持一致。
  function stateOfApp(a) {
    for (const k of [(a.uninstall && a.uninstall.service) || '', a.service_label || '', a.id || '']) {
      if (k && svcState[k]) return svcState[k];
    }
    return null;
  }

  // svcOfApp 找这个市场条目对应的服务记录（「打开」的地址兜底读它、
  // 服务缺失时的「重新部署」判据也读它）。按归一化 key 对齐 —— 与合并去重同一套规则。
  function svcOfApp(a) {
    const key = appKeyOf(a);
    if (!key) return null;
    for (const s of svcList) if (s && s.name && appKeyOf(s) === key) return s;
    return null;
  }

  // marketApps 是「应用市场」Tab 的条目：**全部**可安装的原生应用。
  //
  //   · 不再按已安装/未安装切成两块（用户："始终显示全部"）；
  //   · docker 推荐项目去「docker」Tab，站点应用去「一键建站」Tab（各自有 Tab）。
  //   · 按归一化 key 去重后渲染（与「已安装」Tab 同一套去重规则）。
  function marketApps() {
    return dedupeMarketEntries((cache?.list || []).filter((a) => a && !isDockerRec(a) && !a.site_app));
  }

  // ---------- 更新检查（每次进页面问后端一次；后端自己 9 分钟缓存 + 安装/升级后失效）----------

  // eligibleForUpdateCheck 是"这个条目该不该探测"的唯一判据：
  // supports_update_check（静态声明）+ 已安装 + 当前没有在查。
  //
  // ⚠️ **刻意不看前端的 10 分钟 TTL**（2026-09-22 zizvideo 报障）：后端批量接口自己
  // 有 9 分钟缓存、而且安装/升级完会主动失效它；前端再压一层 TTL 的后果是
  // "镜像上刚发布的新版在界面上消失"——用户看到的旧结论要等 10 分钟才刷新。
  // 所以每次进页面都向后端问一次（后端命中缓存时几乎零成本）；force 只决定要不要
  // 带 fresh=1 让后端重跑真实探测。
  function eligibleForUpdateCheck(a) {
    return !!(a && a.id && a.supports_update_check && isInstalled(a) && !updateChecking[a.id]);
  }

  // ensureUpdateChecks 一次批量拿**全部**结论（后端一次 `brew outdated` + 动态索引），
  // 写进同一份 updateChecks —— 「已安装」与「应用市场」两个子 Tab 共用它。
  // 按卡片逐个探测会跑 N 次联网比对（13 个 brew 应用 = 13 次），所以这里必须批量。
  // force=true（用户点「⟳ 更新」）时带 fresh=1 让后端重跑真实探测。
  // 返回这一轮的 Promise：调用方（刷新）要等它落定才能如实汇报"更新完了"。
  function ensureUpdateChecks(list, onDone, force) {
    if (updateInflight) {
      if (typeof onDone === 'function') updateInflight.then(() => onDone());
      return updateInflight;
    }
    const targets = (list || []).filter(eligibleForUpdateCheck);
    if (!targets.length) return Promise.resolve();
    const ids = targets.map((a) => a.id);
    for (const id of ids) updateChecking[id] = true;
    const round = api.marketUpdates(!!force)
      .then((resp) => { applyUpdatesResponse(resp, ids); })
      .catch((e) => { for (const id of ids) updateChecks[id] = updateCheckFailure(e); })
      .then(() => {
        for (const id of ids) delete updateChecking[id];
        saveUpdateCheckStore();
      });
    updateInflight = round;
    round.then(() => {
      if (updateInflight === round) updateInflight = null;
      if (typeof onDone === 'function') onDone();
    });
    return round;
  }

  // updateCheckFailure 把接口/网络失败折叠成一条**如实的 unknown**（不猜结论）。
  function updateCheckFailure(e) {
    const msg = (e && e.message) ? e.message : String(e);
    return {
      installed: '', latest: '', update_available: false, unknown: true,
      checked_at: new Date().toISOString(), error: '检查失败：' + msg,
    };
  }

  // startUpgrade 是「更新」按钮的动作：统一打 POST /market/{id}/upgrade，
  // 后端按轨分流（brew → `brew upgrade`；动态索引条目 → 复用安装流程）。
  // 刻意不复用 preflight：应用已经装着在跑，更新不需要端口/依赖预检；
  // 而 `brew install` 对已装包是**幂等跳过**，拿它当更新就是"点了没反应还报成功"。
  // syncAfterAppUpgrade 在"某个应用升级完成"后同步一次（用户 2026-09-22："执行软件更新后
  // 就应该同步一次，然后调整显示"）：重拉市场列表（已装版本变了）→ 强制重查 → 重算红点，
  // 避免"更新完了提示还挂着"或"红点与页面状态对不上"。
  async function syncAfterAppUpgrade() {
    try {
      cache = await api.market();
      cacheAt = new Date().toISOString();
      saveDataCache();
    } catch (e) { /* 保留旧缓存，等下一轮 */ }
    await runBackgroundUpdateCheck();
    try { window.dispatchEvent(new CustomEvent('zp:app-update-checked', { detail: {} })); } catch (e) { /* 忽略 */ }
  }

  function startUpgrade(a) {
    if (!a || !a.id) return;
    // 升级前后都让结论失效：任务结束时后端已经把缓存清掉了，前端再**强制重探一次**，
    // 这样"更新"按钮点完徽标立刻按新版本刷新（不用等前台缓存过期）。
    invalidateUpdateCheck(a.id);
    taskCenter.start({
      kind: 'upgrade',
      target: a.id,
      title: '更新 ' + (a.name || a.id),
      start: () => api.marketUpgrade(a.id),
      onDone: () => {
        invalidateUpdateCheck(a.id);
        // 后端在任务收尾时已经失效过缓存；这里"重拉列表 + 强制重查 + 重算红点"一次做完，
        // 页面上的徽标、顶部横幅与侧栏红点就会同时切到新状态。
        syncAfterAppUpgrade().then(() => refreshSilently());
      },
    });
  }

  // afterUpdateChecks 是探测落定后的重画（页面已切走就不再动 DOM）。
  // **当前在哪个子 Tab 就重画哪个**：探测本身与子 Tab 无关，结论两个 Tab 共用。
  function afterUpdateChecks() {
    if (!alive) return;
    if (active === 'market' && marketGrid) {
      renderHead();
      renderGrid();
      return;
    }
    // 「已安装」等其它子 Tab：整块重画（徽标 + 「更新」按钮就在 installedCard 上）。
    renderBody();
  }

  // updateBadgeOf 只给**确定有更新**的条目一枚徽标；unknown/已最新/没查 一律不给。
  function updateBadgeOf(a) {
    if (!updatePendingOf(a)) return null;
    const latest = String((updateCheckOf(a) || {}).latest || '');
    return h('span.pill.warn', {
      text: '有新版' + (latest ? ' v' + latest : ''),
      title: '镜像站上已经有更新的版本' + (latest ? ' v' + latest : '') + '；点「更新」按安装同一套流程升级',
    });
  }

  // updateCheckNoteOf 是被动位置上的一行小字：只在 unknown 时说明失败原因，
  // **绝不显示"已是最新"**（用户明确要求）。
  function updateCheckNoteOf(a) {
    if (!a || !a.supports_update_check) return null;
    const c = updateCheckOf(a);
    if (!c || !c.unknown) return null;
    const why = String(c.error || '检查没有返回结论');
    return h('div', {
      style: { fontSize: '11.5px', lineHeight: '1.6', color: 'var(--text-mute)' },
      title: why,
      text: '更新检查失败：' + (why.length > 80 ? why.slice(0, 80) + '…' : why),
    });
  }

  // updateAction 给已安装的 supports_update_check 卡片那颗按钮：
  // 探测中 → 可见的「检查更新中…」；确定有更新 → 主按钮「更新」（走现有安装任务流）；
  // 其余 → 「检查更新」重试入口。
  function updateAction(a) {
    if (!a || !a.supports_update_check || !isInstalled(a)) return [];
    if (updateChecking[a.id]) {
      return [h('button.btn.btn-sm', {
        text: '检查更新中…', disabled: true,
        title: '正在读镜像索引并与已装模块的版本比对',
      })];
    }
    if (updatePendingOf(a)) {
      return [h('button.btn.btn-sm.btn-primary', {
        text: '更新',
        title: '更新到最新版：brew 条目走 `brew upgrade`；独立产物条目按最新版重装并复核 sha256 与版本',
        onclick: () => startUpgrade(a),
      })];
    }
    // 「检查更新」按钮已按用户要求**合并进右上角的「⟳ 更新」**（一次查全部，
    // 不再每张卡片各给一个按钮）。这里没有新版就什么都不给，只留徽标/备注。
    return [];
  }

  // ---------- 应用市场 Tab ----------
  function renderMarketTab() {
    const grid = h('div');
    const head = h('div.card-head', [
      h('h3', { text: '应用市场' }),
      h('div.spacer'),
      h('div#apps-head', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
    ]);
    appendAll(body, h('div.card', [head, h('div.card-body', [grid])]));
    marketGrid = grid;
    marketHead = head.querySelector('#apps-head');
    if (loadError && !cache) {
      appendAll(marketGrid, h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: loadError.message || String(loadError) }),
      ]));
      return;
    }
    renderHead();
    renderGrid();
  }

  function renderHead() {
    if (!marketHead) return;
    clear(marketHead);
    const all = marketApps();
    const installed = all.filter(isInstalled).length;
    appendAll(marketHead,
      h('span.pill', { text: `共 ${all.length} 个应用` }),
      installed > 0 ? h('span.pill.ok', { text: `已安装 ${installed}` }) : null,
      // 安装状态筛选（全部/未安装/已安装，默认未安装）与「原生/Docker」分类筛选
      // 都已删除：用户明确要求市场**始终显示全部**，docker 类也搬去了 docker Tab。
      // 平时打开这一页直接读缓存（缓存优先，见 load 的说明）；这颗按钮是唯一的刷新入口。
      h('button.btn.btn-sm', {
        text: refreshing ? (refreshPhase || '更新中…') : '⟳ 更新',
        disabled: refreshing,
        title: '重新探测本机装了哪些软件、有没有新版（要跑一次 brew / docker 复核，约几秒）。'
          + '平时打开应用市场直接读缓存、不发请求；装好软件后点这里更新状态。',
        onclick: () => refresh(),
      }),
      // 「一键安装 LNMP 环境」已从这里**搬到「网站管理」页**（用户要求：它属于网站板块）。
      // 为什么市场里不再保留这个入口：LNMP 是**组合动作**（nginx + PHP + MySQL
      // + 默认站点 / vhosts / 系统级守护进程等收尾工作），不是单个可安装条目；
      // 同一件事在两处给入口只会让用户不知道该点哪。
      // 后端能力（POST /api/v1/market/install-lnmp）与目录条目都保留；目录里
      // 本来也没有 ID=lnmp 的 App（见 catalog.go「网站环境」段的说明），所以市场
      // 不会渲染出它的卡片；万一将来有人加进去，web 层的 marketHiddenApps 会挡下。
      // 原来的「⚙️ 服务管理」按钮已删：那一页就是本页的「已安装」Tab，
      // 页内 Tab 已经给了入口，再放一颗按钮只会让人以为是两个页面。
      // 子路径入口有两段：面板自己反代（自动生效）+ nginx 的 80 端口（要写配置）。
      // 这个按钮管第二段 —— 用户要的 `http://<本机 IP>/iopaint/` 就是它。
      proxyState && proxyState.enabled
        ? h('button.btn.btn-sm', {
          text: '🔍 检测可用性',
          title: '逐个探测应用界面能不能打开（要跑十几条请求，约 5-15 秒）',
          onclick: () => doProbe(),
        })
        : null,
      proxyState && proxyState.enabled
        ? h('button.btn.btn-sm', {
          text: '🔗 生成 nginx 入口',
          title: '把每个有界面的应用挂到 http://<主机>/<应用>/（写入 nginx 并重载）',
          onclick: applyProxies,
        })
        : null,
    );
  }

  // ---------- docker Tab：纯展示面板建议的 docker 项目 ----------
  //
  // 用户 2026-09-17 的原始要求（"一，关于 docker 应用"）：docker 容器不该放在
  // 应用中心、不该写得那么"重"；这里只列出**面板建议的项目**，不添加任何功能，
  // 就是纯卡片展示。这些推荐项目在镜像站里提供现成内容供拉取，compose 里给出
  // **预配置文件**供用户参考，用户简单编辑后即可自己运行 —— 所以这个 Tab 里
  // **没有任何安装/部署/启停按钮**（"但不提供安装"）。
  function renderDockerTab() {
    const docker = cache?.docker || {};
    const list = dockerRecApps();
    const grid = h('div#docker-grid.grid.grid-3', list.map(dockerCard));
    const head = h('div.card-head', [
      h('h3', { text: 'docker 推荐项目' }),
      h('div.spacer'),
      h('div#docker-head', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill', { text: `共 ${list.length} 个项目` }),
        h('span.pill' + (docker.available ? '.ok' : '.warn'), {
          text: docker.available ? `Docker ${docker.version || '已就绪'}` : 'Docker 运行时未就绪',
          title: docker.available
            ? 'Docker socket: ' + (docker.socket || '')
            : '这些 compose 文件可以直接取用；要真正跑起来需要一个 Docker 运行时（可用「应用市场」里的 Colima / OrbStack）',
        }),
        h('button.btn.btn-sm', {
          text: refreshing ? '刷新中…' : '⟳ 刷新',
          disabled: refreshing,
          title: '重新探测一次 Docker 运行时与已装软件（约几秒）',
          onclick: () => refresh(),
        }),
      ]),
    ]);
    const box = h('div.card-body');
    appendAll(box, dockerCallout());
    if (!list.length) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '🐳' }),
        h('h4', { text: '暂时没有推荐项目' }),
        h('p', { text: '面板目录里还没有标记为 docker 推荐项目的条目。' }),
      ]));
    } else {
      appendAll(box, grid);
    }
    appendAll(body, h('div.card', [head, box]));
  }

  // dockerCallout 是"已在醒目处提醒提供预配置 compose 文件"的那一条（用户要求）。
  // 用 warn-soft 底 + 边框，放在列表最上方；文案是纯文本，不使用 Markdown 记号。
  function dockerCallout() {
    const docker = cache?.docker || {};
    // 镜像站上"全部推荐项目"的总索引（后端 compose_readme_url，每条同一个值）。
    const readme = composeReadmeURLOf(cache?.list || []);
    return h('div#docker-callout', {
      style: {
        background: 'var(--warn-soft)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius)', padding: '12px 14px', marginBottom: '14px',
        fontSize: '12.5px', lineHeight: '1.8',
      },
    }, [
      h('div', { text: '📦 面板已为下面每个项目提供预配置的 docker compose 文件（含随机密钥与端口说明），可直接取用/改完自己跑。' }),
      h('div.hint', {
        text: docker.available
          ? '这些只是推荐项目清单：面板不代为安装、不管启停。取走 compose 文件后由你自己执行。'
          : '这些只是推荐项目清单：面板不代为安装、不管启停。当前还没有可用的 Docker 运行时，'
            + '取走 compose 文件后请先准备好运行时再执行。',
      }),
      readme
        ? h('div', { style: { marginTop: '4px' } }, [
          h('a', {
            href: readme, target: '_blank', rel: 'noopener',
            text: '查看全部推荐项目的 compose 说明 ↗',
            title: '从面板镜像站打开 compose/README.md：一页列出全部推荐项目、各自端口与网络方式，先看这里再挑项目（新标签页打开）',
          }),
        ])
        : null,
    ]);
  }

  function dockerRecApps() {
    return dedupeMarketEntries((cache?.list || []).filter(isDockerRec));
  }

  // dockerCard 是一张纯展示卡片：图标 / 名称 / 描述 / 默认端口 / 需要的镜像 /
  // compose 文件的操作。**没有安装、部署、启停、卸载按钮**。
  function dockerCard(a) {
    const images = dockerImagesOf(a);
    const url = composeURLOf(a);
    const envURL = composeEnvURLOf(a);
    const text = composeTextOf(a);
    const pills = [
      a.port > 0 ? h('span.pill', { text: '默认端口 :' + a.port }) : null,
      images.length ? h('span.pill.brand', { text: `${images.length} 个镜像` }) : null,
    ];
    const extra = [];
    // 需要哪些镜像：接口给了就用接口的，没给就从 compose 的 image: 行解析。
    // 两者都拿不到时如实说明"以 compose 文件为准"，不编造清单。
    extra.push(h('div', { style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.6' } }, [
      h('span', { text: '需要镜像：' }),
      images.length
        ? h('span.mono', { style: { wordBreak: 'break-all' }, text: images.join('、') })
        : h('span', { text: '（接口未提供，以 compose 文件里的 image: 为准）' }),
    ]));
    const actions = [];
    if (text) {
      actions.push(h('button.btn.btn-sm.btn-primary', {
        text: '复制 compose 配置',
        title: '复制这个项目的预配置 docker-compose.yml 内容，自己改完即可运行',
        onclick: async () => {
          try {
            await navigator.clipboard.writeText(text);
            toast(`已复制「${a.name}」的 compose 配置`, 'ok');
          } catch { toast('复制失败（浏览器限制），请从文件 URL 打开后手动复制', 'warn', 9000); }
        },
      }));
    }
    if (url) {
      actions.push(h('a.btn.btn-sm', {
        href: url, target: '_blank', rel: 'noopener', text: '打开 compose 文件',
        title: '打开/下载面板为这个项目准备好的 compose 文件：' + url,
      }));
    }
    if (envURL) {
      actions.push(h('a.btn.btn-sm', {
        href: envURL, target: '_blank', rel: 'noopener', text: '变量样例文件 ↗',
        title: '从面板镜像站取这个项目需要的环境变量样例（.env.example）：照着里面的说明改，'
          + '再复制成 .env 就行（新标签页打开）',
      }));
    }
    if (!actions.length) {
      // 接口没给内容/地址时把位置留出来，并如实说明（绝不摆一颗点了没用的按钮）。
      actions.push(h('span', {
        style: { fontSize: '11.5px', color: 'var(--text-mute)' },
        text: '接口还没有提供这个项目的 compose 文件内容/地址',
      }));
    }
    return appCardShell({
      icon: a.icon, name: a.name, subtitle: a.summary,
      pills, text: a.description || '', extra, actions,
    });
  }

  // ---------- 一键建站 Tab ----------
  //
  // site_app 类条目（typecho / wordpress / freshrss）单独一栏：它们的"安装"
  // 是一整套建站流程（下载源码 → 建库 → 写配置 → 建 vhost + 伪静态），
  // 表单与流程住在下面的 openSiteInstall，与上一轮一字未改。
  function renderSitesTab() {
    const list = dedupeMarketEntries((cache?.list || []).filter((a) => a && a.site_app));
    const head = h('div.card-head', [
      h('h3', { text: '一键建站' }),
      h('div.spacer'),
      h('div#sites-head', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill', { text: `共 ${list.length} 个建站程序` }),
        h('button.btn.btn-sm', {
          text: refreshing ? '刷新中…' : '⟳ 刷新',
          disabled: refreshing,
          title: '重新探测一次 Docker 运行时与已装软件（约几秒）',
          onclick: () => refresh(),
        }),
      ]),
    ]);
    const box = h('div.card-body');
    if (!list.length) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '🌐' }),
        h('h4', { text: '暂时没有可一键建站的程序' }),
        h('p', { text: '面板目录里还没有 site_app 类型的条目。' }),
      ]));
    } else {
      // 卡片外壳与市场 / docker / 已安装同一套；按钮由 appCard 的 site_app 分支给
      // （「一键建站」+ 有 docs_url 时的「文档」）。
      appendAll(box, h('div.hint', {
        style: { marginBottom: '14px' },
        text: '填一个域名即可：面板会自动下载官方源码、建库建用户、写配置文件、'
          + '建站点并套用伪静态。域名创建后不可修改。',
      }), h('div#sites-grid.grid.grid-3', list.map(appCard)));
    }
    appendAll(body, h('div.card', [head, box]));
  }

  // doProbe 手动触发一次可用性探测（打开页面时不再自动跑，见 load() 的说明）。
  async function doProbe() {
    try {
      toast('正在探测应用界面…', 'info', 3000);
      proxyState = await api.appProxies();
      renderGrid();
    } catch (e) {
      if (isNetworkFailureText(e.message)) {
        // 探测失败也可能是网络：摆出常驻醒目提示，而不是只闪一条 toast。
        netFailure = e.message;
        renderBody();
        toast(networkHintText() + '\n原始报错：' + e.message, 'err', 20000);
      } else {
        toast('探测失败：' + e.message, 'err', 8000);
      }
    }
  }

  // applyProxies 生成/更新 nginx 里的子路径入口。
  // 秒级动作（写文件 + nginx -t + reload），后端同步返回，失败会原样带回 nginx 的报错。
  async function applyProxies() {
    try {
      const r = await api.appProxyApply();
      toast(`已写入 ${r.count} 个入口（${(r.slugs || []).join('、')}）并重载 nginx`, 'ok', 8000);
    } catch (e) {
      toast('生成失败：' + e.message, 'err', 12000);
      return;
    }
    // 等 nginx 把新配置真正切上去再探测：reload 是异步的（旧 worker 要收尾），
    // 立刻探测会拿到旧配置的结论，界面就会显示成"子路径不可用"。
    await new Promise((r) => setTimeout(r, 900));
    try { proxyState = await api.appProxies(); } catch { /* 忽略 */ }
    renderGrid();
  }

  // installLNMP 已搬到「网站管理」页（sites.js）：它是网站板块的环境准备动作。
  // 这里只留一行注释说明去向，免得下次有人以为市场漏了这个入口。

  // randomToken 生成与后端同格式的密钥：ttsv- + 32 位十六进制
  function randomToken() {
    const b = new Uint8Array(16);
    crypto.getRandomValues(b);
    return 'ttsv-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  }

  // installQwenTTS 让用户先选"要不要鉴权"和"密钥从哪来"，再开始装。
  //
  // 为什么把鉴权做成选项：mlx-audio 上游完全没有鉴权，对外暴露与否是
  // 一个真实取舍。默认勾选"加鉴权"，并且不加鉴权时明确写出后果 ——
  // 默认值要安全，危险选项要让用户看见代价。
  function installQwenTTS(appId) {
    const authOn = h('input', { type: 'checkbox', checked: true });
    const keyInput = h('input.input', { placeholder: '留空则自动生成', value: '' });
    const risk = h('div', {
      style: {
        display: 'none', marginTop: '8px', padding: '9px 11px',
        background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12px', lineHeight: '1.7',
      },
      text: '⚠️ 不加鉴权：Qwen 会监听 0.0.0.0 且没有任何鉴权，' +
            '同一内网里任何人都能白用这块 GPU 合成语音。只在你完全可控的网络里这样做。',
    });
    const keyRow = h('div.field', [
      h('label', { text: '共享密钥（网站插件里要填同一个值）' }),
      h('div', { style: { display: 'flex', gap: '8px' } }, [
        keyInput,
        h('button.btn.btn-sm', {
          text: '🎲 生成',
          onclick: () => { keyInput.value = randomToken(); },
        }),
      ]),
      h('div.hint', { text: '留空则自动生成一个随机密钥 —— 部署完成后会单独显示出来，请务必记录。' }),
    ]);
    authOn.addEventListener('change', () => {
      risk.style.display = authOn.checked ? 'none' : 'block';
      keyRow.style.display = authOn.checked ? '' : 'none';
    });

    modal({
      title: '部署 Qwen3 TTS',
      body: h('div', [
        h('div', { style: { fontSize: '12.5px', lineHeight: '1.8', marginBottom: '12px' } },
          '将安装：Python 3.11 虚拟环境 → mlx-audio[server] → 模型（约 2GB）→ ' +
          '注册为系统级后台服务（端口 8880）。需要 16GB 内存与 10GB 可用磁盘。'),
        h('div.field', [
          h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, [
            authOn, h('span', { text: '加鉴权（推荐）' }),
          ]),
          h('div.hint', {
            text: 'Qwen 只监听本机，对外由一个带共享密钥的反向代理（8899）提供 —— ' +
                  '网站插件通过 8899 + 密钥访问。取消勾选则 8880 直接对外且无鉴权。',
          }),
          risk,
        ]),
        keyRow,
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始部署',
          onclick: () => {
            const opts = { auth: authOn.checked, token: keyInput.value.trim() };
            close();
            toast(opts.auth ? '已选择加鉴权：装完会自动部署带密钥的反向代理入口。'
                            : '未加鉴权：8880 将直接对外。', opts.auth ? 'info' : 'warn', 9000);
            taskCenter.start({
              kind: 'install',
              target: appId || 'qwen3tts',
              title: '部署 Qwen3 TTS',
              start: () => api.installQwenTTS(opts),
            });
          },
        }),
      ],
    });
  }

  function installVoiceReceiver(appId) {
    const keyInput = h('input.input', { placeholder: '留空则自动生成', value: '' });
    const noAuth = h('input', { type: 'checkbox' });
    const risk = h('div', {
      style: {
        display: 'none', marginTop: '8px', padding: '9px 11px',
        background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12px', lineHeight: '1.7',
      },
      text: '⚠️ 留空且勾选"不鉴权"：任何人都能往这台机器投放文件，也能调用合成。仅限完全可控的内网。',
    });
    noAuth.addEventListener('change', () => { risk.style.display = noAuth.checked ? 'block' : 'none'; });

    modal({
      title: '部署音色接收端',
      body: h('div', [
        h('div', { style: { fontSize: '12.5px', lineHeight: '1.8', marginBottom: '12px' } },
          '部署音色样本接收端 + 带鉴权的反向代理（端口 8899）：接收上传的音色样本，' +
          '并把 /v1/* 转发给只监听本机的 8880。'),
        h('div.field', [
          h('label', { text: '共享密钥（网站插件里要填同一个值）' }),
          h('div', { style: { display: 'flex', gap: '8px' } }, [
            keyInput,
            h('button.btn.btn-sm', { text: '🎲 生成', onclick: () => { keyInput.value = randomToken(); } }),
          ]),
          h('div.hint', { text: '留空则自动生成；已部署过时会复用旧密钥（换掉会让网站失联）。' }),
        ]),
        h('div.field', [
          h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, [
            noAuth, h('span', { text: '不启用鉴权（密钥留空）' }),
          ]),
          risk,
        ]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始部署',
          onclick: () => {
            const opts = { token: keyInput.value.trim(), no_auth: noAuth.checked };
            close();
            taskCenter.start({
              kind: 'install',
              target: appId || 'voicereceiver',
              title: '部署音色接收端',
              start: () => api.installVoiceReceiver(opts),
            });
          },
        }),
      ],
    });
  }

  // installIOPaint / installPhpMyAdmin 都是"提交即返回"的异步任务，
  // 真正的进度与结果（IOPaint 的访问地址等）在任务中心的进度窗里。
  function installIOPaint(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'iopaint',
      title: '部署 IOPaint（图片去水印）',
      start: () => api.installIOPaint(),
    });
  }

  // adoptApp（把本机已有的服务接进面板）的用户入口在之后只剩一处：
  // 这类应用（目录里有 adopt_label 的，例如 Ollama 的官方二进制安装）在卡片上
  // 显示「添加到面板」，点了走下面的 preflight → doInstall → 后端 POST
  // /api/v1/market/install（后端对 AdoptLabel 非空的应用走 AdoptApp，不重新安装、
  // 也不动用户的软件）。**不再有"扫描本机可纳管服务"的批量入口**（见 services.js）。
  // 面板启动时会自己登记本机已有的已知服务，用户不需要理解"纳管"。
  // 曾经有过两份"确认纳管"弹窗，那份历史实现在 2026-09-17 合并时就删掉了。

  function installPhpMyAdmin(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'phpmyadmin',
      title: '部署 phpMyAdmin',
      start: () => api.installPhpMyAdmin(),
    });
  }

  // ---------------- 本地插件（P2） ----------------
  //
  // 为什么单独一块：插件是"用户自己放进来的应用"，与面板自带目录的信任级别不同 ——
  // 默认关闭、要显式启用；坏文件也要列出来并说明原因（"放进去了什么都不发生"最难查）。
  // 启用/停用会改变市场目录（后端 Catalog 会并进已启用的插件），所以切换后必须重载。
  let pluginsCache = null; // { dir, items[] } | null（null = 还没拉过）
  let pluginPlanId = '';   // 正在展开「计划」的那一条

  async function loadPlugins(force) {
    if (pluginsCache && !force) return pluginsCache;
    try {
      // api.request 已经把 {ok,data} 解包成 data 了 —— 别再取 .data（会拿到 undefined，
      // 现象是"分区在、但永远显示 0 条"）。
      const r = await api.plugins();
      pluginsCache = (r && Array.isArray(r.items)) ? r : { dir: '', items: [] };
    } catch (e) {
      pluginsCache = { dir: '', items: [], error: String(e.message || e) };
    }
    return pluginsCache;
  }

  function pluginRow(it) {
    const id = it.id || '';
    const meta = [];
    if (it.source_kind) meta.push('来源 ' + it.source_kind);
    if (it.run_mode) meta.push('运行 ' + it.run_mode);
    if (it.file) meta.push(it.file);
    const row = h('div.card', { style: { padding: '10px 12px' } });
    const head = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } });
    head.appendChild(h('span', { text: it.icon || '🧩' }));
    head.appendChild(h('strong', { text: it.name || it.file || '(未命名)' }));
    head.appendChild(h('span.sub', { text: id ? '(' + id + ')' : '' }));
    head.appendChild(h('div.spacer', { style: { flex: '1' } }));
    if (!it.valid) {
      head.appendChild(h('span.badge.badge-warn', { text: '声明有问题' }));
    } else if (it.enabled) {
      head.appendChild(h('span.badge.badge-ok', { text: '已启用' }));
    } else {
      head.appendChild(h('span.badge', { text: '未启用' }));
    }
    row.appendChild(head);
    if (meta.length) row.appendChild(h('div.sub', { text: meta.join(' · ') }));
    if (!it.valid) {
      row.appendChild(h('div.sub', { style: { color: 'var(--danger, #d9534f)' }, text: it.error || '声明不合法' }));
    } else if (!it.installable) {
      row.appendChild(h('div.sub', { text: it.reason || '暂不支持安装' }));
    }
    const btns = h('div', { style: { display: 'flex', gap: '6px', marginTop: '8px', flexWrap: 'wrap' } });
    if (it.valid && it.installable) {
      btns.appendChild(h('button.btn.btn-sm' + (it.enabled ? '' : '.btn-primary'), {
        text: it.enabled ? '停用' : '启用',
        onclick: async () => {
          try {
            await api.pluginToggle(id, !it.enabled);
            toast(it.enabled ? '已停用（已装的东西不会被删）' : '已启用，正在刷新应用目录…', 'ok');
            pluginsCache = null;
            await load();
          } catch (e) { toast(e.message, 'err'); }
        },
      }));
    }
    if (it.plan) {
      btns.appendChild(h('button.btn.btn-sm', {
        text: pluginPlanId === id ? '收起计划' : '看计划',
        onclick: () => { pluginPlanId = pluginPlanId === id ? '' : id; renderGrid(); },
      }));
    }
    row.appendChild(btns);
    if (it.plan && pluginPlanId === id) {
      row.appendChild(h('pre', {
        text: it.plan,
        style: { whiteSpace: 'pre-wrap', fontSize: '11.5px', maxHeight: '220px', overflow: 'auto', margin: '8px 0 0' },
      }));
    }
    return row;
  }

  function renderPluginsBlock(grid) {
    const info = pluginsCache;
    if (!info) {
      // 正常情况下 load() 已经预取过；万一没有（例如从缓存直接画），这里补一次拉取并重画。
      loadPlugins().then(() => { if (marketGrid) renderGrid(); });
      return;
    }
    const items = info.items || [];
    appendAll(grid, h('div.section-title', {
      style: { marginTop: grid.childElementCount ? '20px' : '0' },
      text: '本地插件（' + items.length + '）',
    }));
    if (info.error) {
      appendAll(grid, h('div.empty', [h('p', { text: '读本地插件失败：' + info.error })]));
      return;
    }
    if (!items.length) {
      appendAll(grid, h('div.empty', [h('p', {
        text: '把一份 JSON 放进 ' + (info.dir || '<安装根>/plugins') + ' 就能新增应用；' +
          '格式见 docs/插件规范.md，或跑 `zizpanel plugin init` 生成模板。',
      })]));
      return;
    }
    appendAll(grid, h('div.grid.grid-3', items.map(pluginRow)));
  }

  // renderGrid 重画「应用市场」的卡片网格。
  //
  // 数据是 marketApps()：**全部**可安装的原生应用（已安装的与未安装的**同时**
  // 显示在一个列表里，不再分两块、也不再默认只显示未安装）。docker 类与站点
  // 应用各有自己的 Tab，不在这里。
  function renderGrid() {
    if (!marketGrid) return;
    clear(marketGrid);
    const all = marketApps();
    if (!all.length) {
      appendAll(marketGrid, h('div.empty', [
        h('div.big', { text: '🧩' }),
        h('h4', { text: '应用目录为空' }),
        h('p', { text: 'docker 项目在「docker」Tab，一键建站在「一键建站」Tab。' }),
      ]));
      return;
    }

    // 更新探测**不在这里触发**：它很贵（读镜像索引 / 起 `--version` 子进程），
    // 只在首次加载、点「⟳ 更新」或后台定时检查时各跑一次（见 load / refresh / runBackgroundUpdateCheck）。
    // 挂在每次重画上会让"切个页签"就重新探测一轮。

    // 置顶：只有**确定**有更新（update_available=true）的条目提前到一个显式分组；
    // unknown/没查/已最新保持原顺序 —— 顺序不因为"还没查完"抖一下。
    // 只在这一屏可见列表内生效，不动 Tab 分流、去重与筛选语义。
    const pinned = all.filter(updatePendingOf);
    const list = pinned.length ? all.filter((a) => !updatePendingOf(a)) : all;
    const pinnedBlock = pinned.length
      ? h('div', [
        h('div.section-title', { text: `可更新（${pinned.length}）` }),
        h('div.grid.grid-3', pinned.map((a) => appCard(a))),
      ])
      : null;

    // 分类板块的**顺序与中文名全部来自后端**（GET /api/v1/market 的 sections，
    // 单一来源在 internal/services/catalog.go 的 MarketSections）。
    // 前端**不写死任何分类中文名** —— 2026-09 起后端把 nginx/PHP/MySQL 归到
    // 「网站环境」、把命令行工具/容器运行时归到兜底板块：这类改名的字面量只存在于
    // 后端一处，这里自动跟着变，不会再漂。
    // 空板块会被跳过（例如站点应用搬去「一键建站」后 site 板块在这里为空）。
    const sections = cache?.sections || [];
    if (!sections.length) {
      // 后端没给 sections（旧版本 / 接口异常）时的降级：不按分类分板块，
      // 但**必须把应用都显示出来**，不能因为拿不到板块定义就留一片空白。
      appendAll(marketGrid, pinnedBlock,
        h('div.section-title', {
          style: { marginTop: pinnedBlock ? '20px' : '0' }, text: '全部应用',
        }),
        h('div.grid.grid-3', list.map((a) => appCard(a))),
      );
      return;
    }

    if (pinnedBlock) marketGrid.appendChild(pinnedBlock);
    const namedKeys = sections.filter((s) => !s.fallback).map((s) => s.key);
    const fallback = sections.find((s) => s.fallback);
    const covered = new Set();
    for (const s of sections) {
      const items = s.fallback
        // 兜底板块收纳所有没有专属板块的分类（后端当前给的是命令行工具/容器运行时等
        // 「跨应用运行依赖」；nginx/PHP/MySQL 已有专属的「网站环境」板块）。
        // 前端不假设兜底里一定是什么 —— 一律照后端给出的 label 渲染。
        ? list.filter((a) => !namedKeys.includes(a.category || 'other'))
        : list.filter((a) => (a.category || 'other') === s.key);
      if (!items.length) continue;
      items.forEach((a) => covered.add(a));
      appendAll(marketGrid,
        h('div.section-title', { style: { marginTop: marketGrid.childElementCount ? '20px' : '0' }, text: s.label }),
        h('div.grid.grid-3', items.map((a) => appCard(a))),
      );
    }
    // 保险：后端万一没给兜底板块，把剩下的条目并进最后一个已有板块，
    // 保证任何应用都不会"因为板块定义缺失而从页面上消失"。
    const rest = list.filter((a) => !covered.has(a));
    if (rest.length) {
      const title = fallback ? fallback.label : (sections[sections.length - 1].label || '');
      appendAll(marketGrid,
        h('div.section-title', { style: { marginTop: '20px' }, text: title }),
        h('div.grid.grid-3', rest.map((a) => appCard(a))),
      );
    }
    // 本地插件单列一块（默认关闭、要显式启用；坏文件也列出来说明原因）。
    renderPluginsBlock(marketGrid);
  }

  // residualOf 判断"没装、但磁盘上还留着上次卸载保留下来的产物/数据"。
  //
  // 为什么必须与"已安装"分开（2026-09-16 用户反馈的原始需求）：
  // 以前后端把"磁盘上有产物"直接算成已安装，于是卸载（保留数据）之后卡片永远停在
  // "已安装·服务未注册"：既没有「安装」入口、也没法重装。用户的原话是
  // "卸载完成后连安装的入口都没有，用户怎么重装"。
  // 现在后端只把产物报成 artifacts，"已安装"只认服务记录/plist/formula，
  // 于是这种状态会如实落到下面的「残留数据」+「安装」。
  function residualOf(a) { return !!a.artifacts && !a.installed; }

  // serviceRepairOf 读后端给的"服务缺失该怎么修"（唯一判据在后端，见
  // services.ServiceRepairFor）：修法与应用怎么部署有关，前端不猜（坑 231）。
  function serviceRepairOf(a) {
    const r = a && a.service_repair;
    return r && r.needed ? r : null;
  }

  // primaryButton 按"这个应用此刻处于什么状态"给出唯一正确的下一步。
  //
  // 判定顺序（改之前先读完这段）：
  //   有任务在跑              → 查看进度（点回任务中心，而不是再点一次）
  //   本机已有这个服务、面板还没记录 → **添加到面板**（不重新安装；见下）
  //   残留态（产物还在、没装） → **安装**（安装器幂等，会复用残留产物）
  //   其它（未安装）          → 安装
  //
  // 2026-09-17 收敛：这个函数现在**只服务未安装/残留态/未登记的卡片**。
  //   · 已安装的卡片给「打开」+ 启停/重启/刷新/管理（见下面的 installed 分支，
  //     「已安装」Tab 里则由 services.js 的 installedCard 给同一组）；
  //   · 站点应用的入口是卡片上的「一键建站」（siteInstallButtons）；
  //   · 「重装 / 卸载 / 文档」全部收进「⚙️ 管理」面板（servicePanel.js 的
  //     reinstallButton / marketUninstallButton / docLink），卡片上不再出现。
  //
  // 用户："弱化管纳这个概念 … 只要知道自己可以在应用里执行安装、
  // 卸载、重装这些动作就可以了。" 所以：
  //   · adopt_label 非空（后端会走 AdoptApp，只登记已存在的服务）时，按钮文案
  //     写成用户能懂的「添加到面板」，而不是「安装」或「纳管」；
  //   · 点完之后的下一步与其它应用完全一样：卡片变成"已安装"，出现
  //     启动 / 停止 / 重启 / ⚙️ 管理（marketQuickActions 由数据决定，不按应用分叉）。
  function primaryButton(a) {
    const running = taskCenter.findByTarget(a.id);
    if (running) {
      return h('button.btn.btn-sm.btn-primary', {
        text: '⟳ 查看进度',
        title: '这个应用有正在进行的任务，点开看实时进度',
        onclick: () => taskCenter.openTask(running.id),
      });
    }
    const adopt = !!a.adopt_label;
    return h('button.btn.btn-sm.btn-primary', {
      text: adopt ? '添加到面板' : '安装',
      disabled: !a.available,
      title: adopt
        ? (a.available
          ? '本机已经有这个服务，把它加到面板里就能查看状态、启动 / 停止 / 重启'
          : (a.note || '本机没有检测到这个服务'))
        : (residualOf(a)
          ? '磁盘上还有上次卸载保留的数据/产物；安装会复用它们，不会重复下载'
          : ''),
      onclick: () => openInstaller(a),
    });
  }

  // docLink 官方文档入口（未安装应用与站点应用留在卡片上 —— 它们没有「⚙️ 管理」
  // 入口可去；已安装应用的文档收在管理面板里，见 servicePanel.js 的 docLink）。
  function docLink(url) {
    return h('a.btn.btn-sm', { href: url, target: '_blank', rel: 'noopener', text: '文档' });
  }

  // appCard 渲染「应用市场」与「一键建站」的条目卡片 ——
  // 卡片外壳走 servicePanel.appCardShell（四个 Tab 同一套 DOM）。
  //
  // 三种形态（全部由数据决定，没有按应用 ID 的分支）：
  //   · 站点应用       → 「一键建站」+（有文档时）「文档」
  //   · 已安装         → 「已安装」pill + **一颗「打开」**（用户第六条：
  //                       "只显示打开，不显示直链"）+ 启停/重启/刷新/⚙️ 管理
  //   · 未安装         → 「安装」（本机已有这个服务时是「添加到面板」）
  //                       +（残留态时）「删除残留数据」+（有文档时）「文档」
  function appCard(a) {
    const installed = isInstalled(a);
    const actions = [];
    if (a.site_app) {
      actions.push(...siteInstallButtons(a));
      if (a.docs_url) actions.push(docLink(a.docs_url));
    } else if (installed) {
      // 「打开」的地址判定在 servicePanel.openTargetOf：**只认用户配的访问地址**
      // （access_url，反代/局域网），没配就不给「打开」（用户 2026-09-27）。
      // 更新检查那一组按钮放在最前：确定有更新时「更新」是这张卡的主按钮。
      actions.push(...updateAction(a));
      // 「打开」or「重新部署」：服务缺失（plist 丢了/没注册成功）时打开指向的服务
      // 并不存在（点了必然 502），这一格改成真的能修的那一步（见 openOrRepairActions）。
      // 没配访问地址时这一格给「设置访问地址」，点了直接进本卡的管理面板并聚焦输入框。
      actions.push(...openOrRepairActions(a, {
        svc: svcOfApp(a),
        onReinstall: () => openInstaller(a),
        onConfigure: () => openAppDetail(a, { focusAccess: true }),
      }));
      // 常用动作：启动/停止、重启、⟳ 刷新、⚙️ 管理（**同一份** serviceActions
      // 实现）。「⚙️ 管理」打开的是与「已安装」卡片**同一个**面板 ——
      // 配置文件编辑、凭据、日志、重装、文档、直链、卸载都在那里面。
      actions.push(...marketQuickActions(a, {
        state: stateOfApp(a),
        onDone: refreshSilently,
        // 让「⚙️ 管理」走 openAppDetail：面板里的「重装」要用本页的安装器
        // 上下文（onReinstall，保留 Qwen 那类应用自己的选项框）。
        onManage: () => openAppDetail(a),
      }));
    } else {
      actions.push(primaryButton(a));
      actions.push(...uninstallButtons(a));
      if (a.docs_url) actions.push(docLink(a.docs_url));
    }
    const subtitle = a.summary || '';
    return appCardShell({
      icon: a.icon,
      name: a.name,
      subtitle,
      subtitleTitle: a.description || '',
      pills: [
        a.port > 0 ? h('span.pill', { text: ':' + a.port }) : null,
        versionPillOf(a),
        a.kind === 'native' ? h('span.pill.brand', { text: '原生' }) : null,
        a.kind === 'compose' ? h('span.pill.brand', { text: 'Docker' }) : null,
        // 装了运行体、但服务没注册（plist 丢了/从没注册成功）：说清缺什么 + **真的**
        // 下一步点哪里。修法与提示都来自后端（services.ServiceRepairFor）—— brew 服务
        // 能靠 `brew services start` 补出 plist，面板安装器托管的（com.zizdog.stt）
        // 只能重跑安装。前端过去一律说"面板会用 brew services start 补上"，
        // whisper.cpp 用户照着点什么也不会发生（坑 231）。
        serviceRepairOf(a)
          ? h('span.pill.warn', {
            text: '已安装·服务未注册',
            title: serviceRepairOf(a).hint || '安装产物在，但 launchd 里找不到这个服务',
          })
          : (a.adopted
            // 面板里有这条记录（内部叫"纳管"）。对用户来说就是**已安装**：
            // 不再单给一个"已纳管"pill（那个词是内部概念，用户要求弱化）。
            // 用户能做什么由卡片上的按钮给（启停 / 重启 / ⚙️ 管理），不需要看懂记录来源。
            ? h('span.pill.ok', {
              text: '已安装',
              title: '面板已经认得这个服务：可以启动 / 停止 / 重启，也能在「⚙️ 管理」里改配置、看日志',
            })
          : (a.installed
            // 装了但服务没在 launchd 里（plist 丢了/没注册成功）是一种**孤儿态**。
            // 例外：no_daemon 的应用**本来就没有守护进程**（phpMyAdmin 是
            // nginx alias + php-fpm，装完就是一个网页入口）。对它报"服务未注册"
            // 是纯粹的误导 —— 用户反馈过这个。（2026-09-14）
            ? (a.no_daemon
              // no_daemon 只说明"没有常驻进程"，**不等于**"是网页入口"：
              // phpMyAdmin（nginx alias）是网页入口，而 ffmpeg 是命令行工具。
              // 以前一律写「网页入口」，ffmpeg 就会被标成"已安装·网页入口"
              // （用户 2026-09-16 截图反馈）。所以按**有没有界面**分开说。
              ? (hasPanelUI(a) || (a.ui && a.ui.self_conf)
                ? h('span.pill.ok', { text: '已安装·网页入口', title: '这个应用没有常驻进程，装完就是一个网页入口' })
                : h('span.pill.ok', { text: '已安装·命令行', title: '这个应用是命令行工具（没有常驻进程，也没有网页界面）' }))
              : h('span.pill' + (a.service_in_launchd ? '' : '.warn'), {
                // "未纳管"改成用户语言：服务在系统里，面板里还没有它的记录。
                // 怎么加进来写在 title 里（工具栏的「+ 注册服务」是真实入口）。
                text: a.service_in_launchd ? '已安装·未加入面板' : '已安装·服务未注册',
                title: a.service_in_launchd
                  ? '这个服务在系统里是好的，只是面板里还没有它的记录。' +
                    '可以在「已安装」工具栏用「+ 注册服务」把它加进来（就能改配置、看日志、启停）'
                  : '安装产物还在，但 launchd 里找不到这个服务；点「⚙️ 管理」里的「重装」可修复（会重建服务定义）',
              }))
            : null)),
        // 残留数据：没装、但磁盘上还有上次卸载保留的产物/数据。
        // 必须与"已安装"分开显示 —— 以前这种状态被判成已安装，卡片停在旧状态、
        // 连安装入口都没有（2026-09-16 用户反馈）。
        residualOf(a) ? h('span.pill.warn', {
          text: '残留数据',
          title: '这个应用当前没有安装：磁盘上还有上次卸载保留的产物/数据，' +
            '或者只剩面板服务记录（运行体已经不在）。' +
            '点「安装」会复用残留产物，只想清干净就点「删除残留数据」',
        }) : null,
        !a.available && !a.installed ? h('span.pill.warn', { text: a.note || '暂不可用' }) : null,
        // 数据库引擎互斥（用户 2026-09-20）：另一个引擎已装时，这张卡点安装必然被拒。
        // 后端是唯一判据，这里只是提前把话说清（详情在 title 里）。
        a.engine_conflict ? h('span.pill.warn', {
          text: '与已装引擎冲突',
          title: a.engine_conflict,
        }) : null,
        // 更新徽标：只有**确定**有更新才给（unknown/已最新/没查都不给）。
        installed ? updateBadgeOf(a) : null,
      ],
      // 卡片上只放**一句话**摘要 + 一段描述（两段不重复）。
      // 完整说明仍然在 title 与「⚙️ 管理」面板里，不会丢。
      text: (a.description && a.description !== subtitle) ? a.description : '',
      textTitle: a.description || '',
      // 次要位置只放"更新检查失败：<原因>"这一行；成功/最新都不在这里说话。
      extra: [updateCheckNoteOf(a)],
      actions,
    });
  }

  // 入口只有一颗「打开」（判定收敛到 servicePanel.openTargetOf / openOrRepairActions）：
  //   · 2026-09-17 起卡片只显示「打开」；
  //   · 2026-09-27 起「打开」只认用户配的访问地址（反代/局域网）；没配就改显示
  //     「设置访问地址」，面板不再自己拼端口当打开目标。

  // ---------- 详情面板（卡片上的"全部动作"都收在这里）----------
  //
  // 2026-09-16 改版：以前卡片上按应用类型分两套按钮 ——
  // 有面板界面的给「打开」，没有的给「📝 编辑配置文件 + 🔄 重启服务」，
  // 而「停止/启动」当时只在服务管理页有。用户的原话是"能作的也就是：停止重启这些，
  // 直接放在软件页面不就行了？折腾什么？"，再加上 frpc 的配置入口在市场、
  // TTS 接收端的在服务管理 —— 同一个应用的能力被拆到了两个页面。
  //
  // 现在只有一个入口：**应用管理面板**（servicePanel.js）。卡片上给常用动作
  // （打开/启停/重启/刷新）+「⚙️ 管理」，「已安装」卡片点开的也是同一个面板。
  // 哪颗按钮出现仍然**全部由数据决定**（config_path / managed / access_url /
  // 凭据接口是否为空），这里不再按应用 ID 写任何分支。
  //
  // openAppDetail 不传 proxyState ——卡片上那颗「打开」的地址只看
  // access_url（用户配的反代/局域网地址），与探测结果无关；
  // 管理面板里也只给一颗「打开」（没配地址时它就不出现）。

  // openAppDetail 打开「应用管理」面板 —— 市场卡片上唯一的"进面板"入口。
  //
  // 面板是**唯一的应用操作入口**，这里只负责把市场这份数据递进去，并告诉它
  // 两件事：① 重装走本页的安装器（onReinstall，保留应用自己的选项框）；
  // ② 动作完成后刷新卡片。服务记录由面板自己按名字去查
  // （市场条目里的 config_path 只是文件名，绝对路径只有服务记录才有）。
  //
  // 2026-09-16：卡片上的「⚙️ 管理」按钮通过 marketQuickActions 的 onManage
  // 回调走到这里 —— 保留这条路径而不是让按钮直接 openServicePanel，
  // 就是为了把本页的安装器上下文（onReinstall）带进面板。
  function openAppDetail(a, opts = {}) {
    const r = serviceRepairOf(a);
    return openServicePanel({
      market: a,
      onDone: refreshSilently,
      onReinstall: () => openInstaller(a),
      // 服务缺失且本页能修时，把「重新部署」放在管理面板的第一颗按钮上 ——
      // 卡片与面板给的是**同一个**下一步，不让用户在两处看到不同说法。
      ...(r && r.action === 'reinstall'
        ? { primaryText: '重新部署', primaryRun: () => openInstaller(a) }
        : {}),
      ...opts,
    });
  }

  // uninstallButtons 给"面板装的"应用一个卸载入口。
  //
  // 为什么必须分三类（见 services.UninstallPlan）：
  //   · service   —— 托管服务（compose 应用等），可以真卸载；
  //   · installer —— 面板自研安装器装的（IOPaint / Qwen / 接收端 / phpMyAdmin /
  //                  Docker 运行时），走安装器自己的卸载；
  //   · forget    —— **本机已有的第三方服务**（内部叫"纳管"：nginx / php / mysql /
  //                  用户自己注册的）：面板绝不卸载它们（删掉用户自己的 MySQL
  //                  等于删掉他的数据），只给「从列表移除（不卸载软件）」，
  //                  并在确认框里逐字说清"不会卸载软件、不动系统上的任何东西"。
  // 没有这三类之一的（brew 核心组件、未安装）就不给按钮。
  // siteInstallButtons 给「一键建站」类应用一个建站入口。
  //
  // 这类应用装出来是一个**网站**（目录 + 数据库 + 伪静态 + vhost），
  // 不是服务也不是容器，所以按钮文案与流程都不同：先弹一个表单问域名与管理员，
  // 再由后端一气做完（见 api_site_apps.go）。
  function siteInstallButtons(a) {
    if (!a.site_app) return [];
    return [h('button.btn.btn-sm.btn-primary', {
      text: '一键建站',
      disabled: !a.available,
      title: '自动下载源码、建库、建站点并套用伪静态',
      onclick: () => openSiteInstall(a),
    })];
  }

  async function openSiteInstall(a) {
    const domain = h('input.input', { placeholder: '例如：blog.test', value: '' });
    const php = h('input.input', { value: '8.2' });
    const note = h('div.hint', {
      text: '面板会自动：下载官方源码 → 解压到 ~/www/<域名> → 建库建用户 → 写配置文件 → ' +
        '建站点并套用「' + (a.site_app.rewrite || '') + '」伪静态。',
    });
    const m = modal({
      title: '一键建站 · ' + a.name,
      body: h('div', [
        h('div.field', [h('label', { text: '域名' }), domain,
          h('div.hint', { text: '先用一个测试域名（如 blog.test）即可；域名创建后不可修改' })]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php]),
        note,
        (a.site_app.notes || []).length
          ? h('ul', { style: { margin: '8px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
            (a.site_app.notes || []).map((n) => h('li', { text: n })))
          : null,
        a.site_app.finish_path
          ? h('div.hint', { style: { marginTop: '8px' }, text: '装完请打开 http://<域名>' + a.site_app.finish_path + ' 走完最后一步。' })
          : null,
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始建站',
          onclick: async () => {
            const d = domain.value.trim();
            if (!d) { toast('请填域名', 'warn'); return; }
            close();
            taskCenter.start({
              kind: 'site-install',
              target: d,
              title: '一键建站 ' + a.name + '（' + d + '）',
              start: () => api.marketInstallSite(a.id, { domain: d, php: php.value.trim() || '8.2' }),
              onDone: (m) => {
                if (m && m.status && m.status !== 'succeeded') {
                  const msg = m.error || m.status;
                  // 站点源码包要从镜像/GitHub 下：网络失败必须显著，别让用户以为建站功能坏了。
                  if (isNetworkFailureText(msg)) {
                    netFailure = msg;
                    toast(networkHintText() + '\n原始报错：' + msg, 'err', 20000);
                  } else {
                    toast('建站失败：' + msg, 'err', 12000);
                  }
                } else {
                  toast('「' + a.name + '」已建站完成', 'ok', 9000);
                }
                refreshSilently();
              },
            });
          },
        }),
      ],
    });
    setTimeout(() => domain.focus(), 60);
    return m;
  }

  // uninstallButtons 只保留**残留态**的「删除残留数据」入口。
  //
  // 2026-09-17（用户原话："每个应用只保留，打开、直链、刷新、重启、停止、管理……
  // 重装、卸载、文档等放进管理的弹出页面里"）：已安装应用的收尾动作
  // 不再摆在卡片上 —— 它们已经（并继续）住在「⚙️ 管理」面板里：
  //   · managed=true          → 面板里的「🗑 卸载」（uninstallButton）
  //   · managed=false         → 面板里的「从列表移除（不卸载软件）」（forgetButton）
  //   · 市场安装器 / compose  → 面板里的「卸载」（marketUninstallButton，逐条列出会删什么）
  // 三类语义都在 servicePanel.js 里由数据决定，一处都不会丢。
  //
  // 残留态（artifacts && !installed）**必须留在卡片上**：这时应用没装、
  // 面板里也不是"已安装"形态，如果连这张卡都没有删除入口，用户就永远清不掉
  // 上次卸载保留下来的产物/数据（2026-09-16 用户反馈过）。
  function uninstallButtons(a) {
    if (!residualOf(a)) return [];
    const plan = a.uninstall || {};
    // 没有可删的磁盘产物/数据（只剩面板记录，或记录 + 无 data_paths）→ 走
    // **只删面板记录**的专用入口：不碰任何在跑的引擎（真机 2026-09-20：
    // mysql84 的死记录被 3306 上 MariaDB 的监听与站点依赖挡住，永远删不掉）。
    // 有 data_paths 时仍然走真正的卸载（remove_data 语义不变）。
    const hasData = Array.isArray(plan.data_paths) && plan.data_paths.length > 0;
    const forgetOnly = !hasData;
    return [h('button.btn.btn-sm.btn-danger', {
      text: forgetOnly ? '删除残留记录' : '删除残留数据',
      title: forgetOnly
        ? '这个应用当前没有安装、也没有磁盘产物；只把这条面板记录删掉（不会停止任何正在运行的服务）'
        : '这个应用当前没有安装；只删除磁盘上的残留产物/数据',
      onclick: () => doUninstall(a, plan, true, forgetOnly),
    })];
  }

  // doUninstall 先弹一个"会做什么"的确认框，再交给任务中心。
  //
  // 确认框里逐条列出步骤与可选删除的路径 —— 卸载不可逆，
  // 一句"确定卸载吗"是不够的（用户有权知道模型/样本/任务会不会一起没）。
  // 确认框本体只有 servicePanel.confirmUninstallPlan 一份实现（见那里的说明：
  // 这里原来抄的那份会让「确认卸载」永远解析成 false，用户看到的就是"点了没反应"）。
  async function doUninstall(a, plan, residual = false, forgetOnly = false) {
    const answer = await confirmUninstallPlan({ name: a.name, plan, residual });
    if (!answer) return;
    // 残留清理的语义就是"删掉产物"，所以直接 remove_data=1，不再让用户勾选。
    const wipe = residual ? true : answer.wipe;
    // force 只可能来自确认框里那个默认不勾的「强制卸载」勾选框
    //（brew --ignore-dependencies，会破坏依赖它的包）。
    const force = residual ? false : !!answer.force;
    taskCenter.start({
      kind: 'uninstall',
      target: a.id,
      title: ((forgetOnly ? '删除残留记录 ' : (residual ? '删除残留数据 ' : (force ? '强制卸载 ' : '卸载 '))) + a.name),
      start: () => (forgetOnly ? api.marketUninstallForget(a.id) : api.marketUninstall(a.id, wipe, force)),
      // 结果必须显式说出来：用户反馈过"卸载完没有任何提示，卡片还停在旧状态，
      // 看起来像什么都没发生"。任务中心的进度窗给过程，这里给结论。
      onDone: (m) => {
        if (m && m.status && m.status !== 'succeeded') {
          toast((forgetOnly ? '删除残留记录失败：' : (residual ? '删除残留数据失败：' : '卸载失败：')) + (m.error || m.status), 'err', 12000);
        } else {
          toast(
            forgetOnly
              ? '已删除「' + a.name + '」的残留记录（没有停止任何服务）'
              : (residual
                ? '已删除「' + a.name + '」的残留数据'
                : '已卸载「' + a.name + '」' + (wipe ? '（含数据/产物）' : '（数据/产物已保留）')),
            'ok', 9000);
        }
        // 卸载改的是本机真实状态：必须重拉（不能走"有缓存就直接渲染"的 load()）。
        refreshSilently();
      },
    });
  }

  // doForget（只删除面板记录）已删除（2026-09-17）：卡片上不再直接给那颗按钮，
  // 它住在「⚙️ 管理」面板里（servicePanel.js 的 forgetButton，按 s.managed===false
  // 决定出现），实现只有那一份。卡片上唯一保留的残留态动作是「删除残留数据」。
  // 2026-09-18 起它的文案是「从列表移除（不卸载软件）」，不再出现"纳管"。

  // openInstaller 按应用打开对应的部署对话框。
  //
  // 抽出来是因为有**两个入口**要用它：
  //   · 「安装」——没装过的应用，以及"残留数据"（artifacts && !installed）；
  //   · 「重装」——已安装的应用，从「⚙️ 管理」面板里的「重装」进来
  //     （servicePanel.js 的 reinstallButton 负责确认框，确认后回调到这里）。
  // 安装器本身是幂等的：重跑会重建 venv/服务定义/plist 并登记到服务管理，
  // 所以"重装"就是最合理的修复动作，不需要另写一套修复逻辑。
  function openInstaller(a) {
    // 用面板自研安装器的项目要收集选项（例如 Qwen 的"要不要鉴权"、
    // 密钥从哪来），所以直接打开对应对话框，而不是走通用安装流程。
    // 传 a.id 作为任务 target：市场卡片的「查看进度」就是按这个值找运行中的任务的。
    switch (a.panel_installer) {
      case 'qwentts': installQwenTTS(a.id); return;
      case 'voicereceiver': installVoiceReceiver(a.id); return;
      case 'iopaint': installIOPaint(a.id); return;
      case 'phpmyadmin': installPhpMyAdmin(a.id); return;
    }
    preflight(a);
  }

  // ---------- 重装 ----------
  //
  // 重装的确认框与调度都收进了 servicePanel.js 的 reinstallButton（管理面板里
  // 那颗「重装」）；本文件只提供"用哪套安装器"这一步（上面的 openInstaller）。
  // 这样"重装"这个动作在两个入口（市场卡片 → 管理、「已安装」卡片 → 管理）只有一份
  // 文案与一份确认语义，而应用自己的选项框仍然保留。

  // ---------- 安装前检查 ----------
  //
  // adopt = 目录里声明了 adopt_label（后端对这类应用走 AdoptApp：只把本机**已经
  // 存在**的服务登记进面板，不重新安装、不动用户的软件）。用户要求
  // 弱化"纳管"这个概念，所以这类应用在这里一律说「添加到面板」，
  // 不说"安装"（它没装任何东西）也不说"纳管"（内部词）。
  async function preflight(a) {
    const adopt = !!a.adopt_label;
    const box = h('div', [h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: adopt ? '正在检查这个服务…' : '正在检查安装条件…' })])]);
    const footer = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', width: '100%' } });

    let pf = null;
    const m = modal({
      title: adopt ? `添加到面板：${a.name}` : `安装检查：${a.name}`,
      wide: true,
      body: box,
      footer: () => [footer],
    });

    try {
      pf = await api.marketPreflight(a.id);
    } catch (e) {
      clear(box);
      const network = isNetworkFailureText(e.message);
      if (network) netFailure = e.message;
      appendAll(box,
        h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]),
        // 安装前检查本身是本地动作；但一旦失败原因是网络（例如去探测镜像/依赖），
        // 必须让用户当场看出是网络，而不是以为这个应用不能装。
        network ? netHintBlock(e.message) : null);
      return;
    }

    clear(box);
    appendAll(box, 
      h('div', { style: { marginBottom: '14px' } }, [
        pf.ready
          ? h('span.pill.ok', { text: adopt ? '✅ 本机已经有这个服务，可以添加到面板' : '✅ 条件满足，可以安装' })
          : h('span.pill.danger', { text: '⚠️ 有未满足的条件' }),
      ]),
      // 端口
      h('div', {
        style: {
          padding: '9px 11px', marginBottom: '8px', borderRadius: '6px', fontSize: '12.5px',
          background: pf.port_free ? 'var(--ok-soft)' : 'var(--danger-soft)',
        },
      }, [
        h('div', { text: (pf.port_free ? '✓ ' : '✗ ') + (pf.port_note || '端口检查未执行') }),
      ]),
      // 依赖
      ...(pf.checks || []).map((c) => h('div', {
        style: {
          padding: '9px 11px', marginBottom: '8px', borderRadius: '6px', fontSize: '12.5px',
          background: c.ok ? 'var(--ok-soft)' : 'var(--warn-soft)',
        },
      }, [
        h('div', { text: (c.ok ? '✓ ' : '! ') + c.name + '：' + c.detail }),
        !c.ok && c.fix_cmd ? h('div', {
          style: { marginTop: '5px', fontFamily: 'var(--mono)', fontSize: '11.5px', color: 'var(--text-dim)' },
          text: '$ ' + c.fix_cmd,
        }) : null,
      ])),
      // 依赖清单（目录里的 requires）：**安装前**就告诉用户"会一并装哪些东西"。
      //
      // 用户 2026-09-18 报障："我安装 tts 成功了，但是它依赖 ffmpeg，却没有安装 ffmpeg，
      // 并且应该给出提示，知道会一并安装" + "Python 3.11 也是 tts 等的依赖，也没有一并安装"。
      // 依赖声明在目录里（requires），但界面从来没渲染过 —— 用户当然不知道。
      reqList(a).length ? h('div', {
        style: {
          padding: '9px 11px', marginTop: '10px', borderRadius: '6px', fontSize: '12.5px',
          background: 'var(--brand-soft, var(--ok-soft))',
        },
      }, [
        h('div', { style: { fontWeight: '620' }, text: '依赖：安装时会一并装好' }),
        h('ul', { style: { margin: '4px 0 0 18px', lineHeight: '1.8' } },
          reqList(a).map((r) => h('li', { text: r.text }))),
      ]) : null,
      a.post_install_hint ? h('div.hint', { style: { marginTop: '12px' }, text: (adopt ? '加到面板后：' : '安装后：') + a.post_install_hint }) : null,
      a.manual_hint ? h('div.hint', { style: { marginTop: '8px' }, text: a.manual_hint }) : null,
    );

    clear(footer);
    appendAll(footer, 
      h('button.btn', { text: '关闭', onclick: () => m.close() }),
      pf.ready || a.adopt_label
        ? h('button.btn.btn-primary', {
          text: adopt ? '添加到面板' : '开始安装',
          onclick: () => { m.close(); doInstall(a); },
        })
        : h('button.btn.btn-primary', {
          text: '仍然尝试安装',
          title: '条件不满足时安装可能失败，但你可以继续',
          onclick: () => { m.close(); doInstall(a); },
        }),
    );
  }

  // reqList 把目录里的 requires 渲染成人话（安装检查对话框与安装确认共用）。
  //
  // 依赖声明的**唯一来源**是目录（internal/services/catalog.go 的 Requires）：
  // 前端不写死任何应用名与依赖名，加应用时不需要改前端。
  function reqList(a) {
    const out = [];
    for (const r of (a && a.requires) || []) {
      const v = String((r && r.value) || '');
      let text = '';
      if (r && r.type === 'brew_formula') {
        text = v + (r.hint ? '（' + r.hint.replace(/^brew install [^（(]*[（(]?/, '').replace(/）?$/, '') + '）' : '');
        if (!r.hint) text = v;
      } else if (r && r.type === 'docker') {
        text = 'Docker 运行时' + (r.hint ? '（' + r.hint + '）' : '');
      } else {
        text = (v || r.type || '未知依赖') + (r && r.hint ? '（' + r.hint + '）' : '');
      }
      out.push({ raw: v, text });
    }
    return out;
  }

  // ---------- 执行安装 ----------
  //
  // 旧实现是"同步等请求 + 事后打印 steps"，用户在整个过程中看不到任何真实输出，
  // 关掉窗口也找不回来。现在改成：POST 立刻返回 task_id，进度交给任务中心。
  // 失败（4xx/5xx）由 taskCenter.start 统一 toast，这里不需要再兜一层。
  async function doInstall(a) {
    // 安装前把"会一并安装的依赖"再说一遍并要求确认（用户明确要求"应该给出提示，
    // 知道会一并安装"）——装 TTS 会顺带装上 ffmpeg 与 Python 3.11 这种事实，
    // 不该只在任务日志里出现。
    const reqs = reqList(a);
    if (reqs.length && !await confirmBox(
      '安装「' + a.name + '」时会**一并安装**这些依赖：\n\n· ' +
      reqs.map((r) => r.text).join('\n· ') +
      '\n\n依赖由 Homebrew 安装（原生 arm64 包）。继续安装？',
      { title: '安装 ' + a.name, okText: '开始安装' })) return;
    // adopt 类应用（目录有 adopt_label）：后端只把本机已有的服务登记进来，
    // 所以任务名与成功提示都**不能**说"安装"（没装任何东西，也不该让用户以为
    // 面板重新装了一遍他已有的软件）。
    const adopt = !!a.adopt_label;
    taskCenter.start({
      kind: 'install',
      target: a.id,
      title: adopt ? `添加到面板 ${a.name}` : `安装 ${a.name}`,
      start: () => api.marketInstall(a.id),
      // 装完必须**立刻**把卡片状态刷新过来（用户 2026-09-16 要求：
      // "安装、卸载后面板中的软件状态要及时更新"）。以前只靠任务状态变化重画，
      // 而任务结束时市场数据还是旧的，卡片就停留在"可安装"。
      onDone: (m) => {
        // 装/更新结束（无论成败）都让本条的更新结论失效：成功时徽标与置顶必须
        // 一起消失；失败时也不能把旧结论当成本次结果继续用。
        invalidateUpdateCheck(a.id);
        if (m && m.status && m.status !== 'succeeded') {
          const msg = m.error || m.status;
          // 安装失败里最常见的真实原因是网络（brew 瓶 / GitHub / docker 镜像）。
          // 后端已尽量在任务错误里附上网络提示；这里再判一次是为了兜住没附上的路径。
          if (isNetworkFailureText(msg)) {
            netFailure = msg;
            toast(networkHintText() + '\n原始报错：' + msg, 'err', 20000);
          } else {
            toast((adopt ? '添加到面板失败：' : '安装失败：') + msg, 'err', 12000);
          }
        } else {
          // 装完把"下一步"直接说出来。用户最常问的就是"装完我该干嘛"。
          // 2026-09-16 起配置入口只有一个：卡片上的「⚙️ 管理」→ 面板里的
          // 「📝 编辑配置文件」+ 启停按钮在同一屏，不再需要两个页面来回找。
          const next = a.config_path
            ? '：点卡片上的「⚙️ 管理」，在面板里改「📝 编辑配置文件」并重启服务生效'
            : '';
          toast(adopt
            ? '「' + a.name + '」已添加到面板：现在可以启动 / 停止 / 重启它了' + next
            : '「' + a.name + '」已安装' + next, 'ok', 10000);
        }
        refreshSilently();
      },
    });
  }

  // refreshSilently 重新拉一次市场数据 + 服务记录并原地重画当前 Tab，
  // **不显示"正在读取"占位**。
  //
  // 为什么要单独一个：load() 会先 clear(body) 再显示 loading 占位，
  // 任务刚结束时调用它，用户会看到整页闪一下白 —— 而他要的只是"状态更新"。
  // 注意两边都要重拉：装的/卸的东西同时改变市场条目的 installed 与服务记录，
  // 只刷新市场会让「已安装」停在旧状态（安装后不进清单）。
  async function refreshSilently() {
    await fetchAll();
    renderBody();
  }

  // 任务状态变化（开始/结束）时重画卡片：正在安装的应用，按钮要变成「查看进度」。
  // 只订阅元信息变化，**不订阅日志行** —— 否则 brew 每输出一行都会重建整个网格。
  // 这是页面级订阅，跟着页面一起清理：任务状态本身活在 tasks.js 的单例里，
  // 清理掉的只是"这个页面要不要重画"。
  registerCleanup(taskCenter.onChange((kind) => {
    if (kind === 'lines' || !cache) return;
    renderBody();
  }));
  load();
}
