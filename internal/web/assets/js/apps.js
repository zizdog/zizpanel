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
// openOnlyAction / openTargetOf / portAccessWarning
//               卡片上那颗「打开」的**唯一**判定与"不支持子路径"的逐字提示
//               （用户 2026-09-17 第六条：只显示打开、不显示直链；不支持子路径的
//                用 ip:端口打开并提示"该应用不支持子路径，请用端口访问，或自行配置反代。"）
// dedupeMarketEntries 市场/docker 列表按归一化 key 去重（与「已安装」的去重同一套规则）
// hasPanelUI    与面板同一条"有没有面板托管的界面"判据
import {
  openServicePanel, marketQuickActions, hasPanelUI, appCardShell,
  openOnlyAction, portAccessWarning, dedupeMarketEntries, appKeyOf,
  // 卸载确认只有一份实现（servicePanel.confirmUninstallPlan）：这里曾经抄过
  // 一份，而那份的确认按钮 `close(); resolve(true)` 会被 modal 的 onClose
  // 里的 resolve(false) 抢先定稿 —— 用户点「确认卸载」后什么都不发生。
  confirmUninstallPlan,
} from './servicePanel.js';

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

export function AppsView(content, ctx = {}) {
  clear(content);

  // 默认落在「已安装」；`#/services`（app.js 的 ROUTE_TARGET）显式给 tab='installed'，
  // `#/apps/docker` 这类 hash 给 tab='docker'。未知值一律落回「已安装」。
  let active = (ctx && APP_TAB_IDS.has(ctx.tab)) ? ctx.tab : 'installed';
  let loadError = null;
  let marketGrid = null;
  let marketHead = null;

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
    // 后端这次**没能复核**「本机装了哪些 Homebrew 包」（brew_probe_ok=false）时，
    // 如实说出来：列表里那些「安装」只代表"没查成"，不代表东西真的没装
    //（铁律 11：不许把"不知道"显示成"没有"）。没有这条提示，一次 brew 超时
    // 就会让用户以为自己的软件全被卸了 —— 那正是 2026-09-23 那一类报障。
    if (cache && cache.brew_probe_ok === false) {
      const why = String(cache.brew_probe_error || '').trim();
      tabBar.appendChild(h('span.pill.warn', {
        text: '⚠ 未能复核已装软件',
        title: '这次没能读到 Homebrew 的已装清单：'
          + '下面标着「安装」的应用可能其实已经装着。点「⟳ 刷新」重试。'
          + (why ? '\n\n真实原因：' + why : ''),
      }));
      // 把真实原因**显示出来**（不只塞进 title）：2026-09-19 用户报障时界面上
      // 只有一句"brew 不可用或超时"，谁也不知道到底是 brew 坏了、权限问题还是超时。
      if (why) {
        tabBar.appendChild(h('span.hint', {
          text: '原因：' + (why.length > 140 ? why.slice(0, 140) + '…' : why),
          title: why,
        }));
      }
    }
  }

  function renderBody() {
    clear(body);
    if (active === 'installed') renderInstalledTab();
    else if (active === 'docker') renderDockerTab();
    else if (active === 'sites') renderSitesTab();
    else renderMarketTab();
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
    renderInstalledApps(body, { market: cache, list: svcList, onReload: load });
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
  async function fetchAll() {
    const [mkt, svc] = await Promise.allSettled([api.market(), api.services(true)]);
    if (mkt.status === 'fulfilled') { cache = mkt.value; loadError = null; }
    else if (!cache) loadError = mkt.reason;
    if (svc.status === 'fulfilled') svcList = (svc.value && svc.value.list) || [];
    rebuildSvcState();
  }

  async function load() {
    clear(body);
    appendAll(body, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取应用目录…' })]));
    await fetchAll();
    // 探测是"能不能打开"的依据，但它要跑十几条网络请求（含 8 秒超时）。
    // **不在打开页面时自动跑** —— 用户反馈"应用市场打开较慢，其它页面都是秒开"。
    // 改为：结果缓存在内存里；点「检测可用性」时才真跑；生成入口后刷新一次。
    if (!proxyState) proxyState = { enabled: true, items: [], _stale: true };
    renderTabBar();
    renderBody();
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

  // svcOfApp 找这个市场条目对应的服务记录（卡片上的「打开」在没给 port_url 时
  // 要靠它拿服务端口）。按归一化 key 对齐 —— 与合并去重同一套规则。
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
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
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
      // 这个按钮管第二段 —— 用户要的 `http://192.168.1.4/iopaint/` 就是它。
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
  // 就是纯卡片展示。这些推荐项目在 NAS 镜像里提供现成内容供拉取，compose 里给出
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
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
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
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
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
      toast('探测失败：' + e.message, 'err', 8000);
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

  // adoptApp（把本机已有的服务接进面板）的用户入口在 2026-09-21 之后只剩一处：
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

  // renderGrid 重画「应用市场」的卡片网格。
  //
  // 数据是 marketApps()：**全部**可安装的原生应用（已安装的与未安装的**同时**
  // 显示在一个列表里，不再分两块、也不再默认只显示未安装）。docker 类与站点
  // 应用各有自己的 Tab，不在这里。
  function renderGrid() {
    if (!marketGrid) return;
    clear(marketGrid);
    const list = marketApps();
    if (!list.length) {
      appendAll(marketGrid, h('div.empty', [
        h('div.big', { text: '🧩' }),
        h('h4', { text: '应用目录为空' }),
        h('p', { text: 'docker 项目在「docker」Tab，一键建站在「一键建站」Tab。' }),
      ]));
      return;
    }

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
      appendAll(marketGrid,
        h('div.section-title', { text: '全部应用' }),
        h('div.grid.grid-3', list.map((a) => appCard(a))),
      );
      return;
    }

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
  // 2026-09-21 用户："弱化管纳这个概念 … 只要知道自己可以在应用里执行安装、
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
      // 「打开」的地址判定在 servicePanel.openTargetOf：支持子路径走子路径，
      // 不支持走端口直连（port_url，或 location.hostname + 端口拼）。
      // 不支持子路径时，卡片下方会有一行逐字提示（portAccessWarning）。
      actions.push(...openOnlyAction(a, { svc: svcOfApp(a) }));
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
        a.kind === 'native' ? h('span.pill.brand', { text: '原生' }) : null,
        a.kind === 'compose' ? h('span.pill.brand', { text: 'Docker' }) : null,
        // 原生 brew 服务：记录在、但 plist 不在 = 服务其实没注册（ollama 就是这样，
        // 用户看到"已安装"却在服务里启动失败）。这一条要显式说出来。
        (a.kind === 'native' && a.service_label && a.installed && !a.service_in_launchd)
          ? h('span.pill.warn', {
            text: '已安装·服务未注册',
            title: '这个 brew 服务还没在 launchd 里注册（plist 不存在）。' +
              '到「已安装」Tab 点一次启动即可自动注册（面板会用 brew services start 补上）',
          })
          : (a.adopted
            // 面板里有这条记录（内部叫"纳管"）。对用户来说就是**已安装**：
            // 不再单给一个"已纳管"pill（那个词是内部概念，用户 2026-09-21 要求弱化）。
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
          title: '这个应用当前没有安装，但磁盘上还有上次卸载保留的产物/数据；' +
            '点「安装」会复用它们，只想清干净就点「删除残留数据」',
        }) : null,
        !a.available && !a.installed ? h('span.pill.warn', { text: a.note || '暂不可用' }) : null,
      ],
      // 卡片上只放**一句话**摘要 + 一段描述（两段不重复）。
      // 完整说明仍然在 title 与「⚙️ 管理」面板里，不会丢。
      text: (a.description && a.description !== subtitle) ? a.description : '',
      textTitle: a.description || '',
      actions,
      // 不支持子路径时，卡片上始终显示那句逐字提示（用户 2026-09-17 第六条）。
      warning: installed ? portAccessWarning(a, { svc: svcOfApp(a) }) : null,
    });
  }

  // openDirectActions（旧的"打开 / 直链"两颗按钮）在卡片上已不再使用：
  //   用户 2026-09-17 第六条要求卡片**只显示「打开」**，不支持子路径时改用
  //   ip:端口打开并显示那句逐字提示 —— 判定收敛到 servicePanel.openTargetOf /
  //   openOnlyAction / portAccessWarning。管理面板里仍然给「打开 + 直链」两颗
  //   （这是上一轮用户明确要的），实现仍在 openDirectActions，一行未改。

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
  // 哪颗按钮出现仍然**全部由数据决定**（config_path / managed / ui.slug /
  // ui.console_only / 凭据接口是否为空），这里不再按应用 ID 写任何分支。
  //
  // hasPanelUI 从 servicePanel.js 导入（与面板、「已安装」卡片**同一条判据**）；
  // openAppDetail 不传 proxyState ——卡片上那颗「打开」的地址由数据
  // （ui.slug / ui.prefer_direct / port_url）决定，与探测结果无关；
  // 管理面板里的「打开 / 直链」仍由 openDirectActions 给（两颗都给）。

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
    return openServicePanel({
      market: a,
      onDone: refreshSilently,
      onReinstall: () => openInstaller(a),
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
                  toast('建站失败：' + (m.error || m.status), 'err', 12000);
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
    return [h('button.btn.btn-sm.btn-danger', {
      text: '删除残留数据',
      title: '这个应用当前没有安装；只删除磁盘上的残留产物/数据',
      onclick: () => doUninstall(a, plan, true),
    })];
  }

  // doUninstall 先弹一个"会做什么"的确认框，再交给任务中心。
  //
  // 确认框里逐条列出步骤与可选删除的路径 —— 卸载不可逆，
  // 一句"确定卸载吗"是不够的（用户有权知道模型/样本/任务会不会一起没）。
  // 确认框本体只有 servicePanel.confirmUninstallPlan 一份实现（见那里的说明：
  // 这里原来抄的那份会让「确认卸载」永远解析成 false，用户看到的就是"点了没反应"）。
  async function doUninstall(a, plan, residual = false) {
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
      title: (residual ? '删除残留数据 ' : (force ? '强制卸载 ' : '卸载 ')) + a.name,
      start: () => api.marketUninstall(a.id, wipe, force),
      // 结果必须显式说出来：用户反馈过"卸载完没有任何提示，卡片还停在旧状态，
      // 看起来像什么都没发生"。任务中心的进度窗给过程，这里给结论。
      onDone: (m) => {
        if (m && m.status && m.status !== 'succeeded') {
          toast((residual ? '删除残留数据失败：' : '卸载失败：') + (m.error || m.status), 'err', 12000);
        } else {
          toast(
            residual
              ? '已删除「' + a.name + '」的残留数据'
              : '已卸载「' + a.name + '」' + (wipe ? '（含数据/产物）' : '（数据/产物已保留）'),
            'ok', 9000);
        }
        load();
      },
    });
  }

  // doForget（只删除面板记录）已删除（2026-09-17）：卡片上不再直接给那颗按钮，
  // 它住在「⚙️ 管理」面板里（servicePanel.js 的 forgetButton，按 s.managed===false
  // 决定出现），实现只有那一份。卡片上唯一保留的残留态动作是「删除残留数据」。
  // 2026-09-21 起它的文案是「从列表移除（不卸载软件）」，不再出现"纳管"。

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
  // 存在**的服务登记进面板，不重新安装、不动用户的软件）。用户 2026-09-21 要求
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
      appendAll(box, h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
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
        if (m && m.status && m.status !== 'succeeded') {
          toast((adopt ? '添加到面板失败：' : '安装失败：') + (m.error || m.status), 'err', 12000);
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
