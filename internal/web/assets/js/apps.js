// apps.js —— 「应用」页：我的应用（合并去重后的清单）+ 应用市场（可安装目录）。
//
// 2026-09-17 信息架构合并（用户："服务管理/apps 合并，执行你的建议（推荐 A）"）：
//   原来的「服务管理」与「应用市场」两个导航项合成**一个「应用」版块**，页内两个 Tab：
//     · 我的应用 —— 市场里已安装的条目 + 面板服务记录，按归一化 key 合并去重，
//                   一行一条（实现住在 services.js 的 renderMyApps）；
//     · 应用市场 —— 可安装 / 可更新的目录条目（本文件的卡片网格）。
//   老链接 `#/services` 由 app.js 的 ROUTE_TARGET 落到本页并切到「我的应用」Tab。
//
// 市场这一侧的核心思路：把"能不能装"和"装什么"分开。
//   - 用户点安装时，先跑一次安装前检查（依赖、端口），把问题一次说清
//   - 检查通过才真正安装；安装是**异步任务**，提交后进度交给「任务中心」，
//     用户可以关窗口、切页面，随时从顶栏重新打开看进度（见 tasks.js）
//   - 已经装过的应用显示"已安装"，常用动作（打开/直链/启停/重启/刷新/管理）
//     直接摆在卡片上；安装生命周期动作（重装 / 卸载 / 文档）收进「⚙️ 管理」面板

import { api } from './api.js';
import { h, clear, toast, modal, appendAll } from './ui.js';
import { registerCleanup } from './app.js';
import { taskCenter } from './tasks.js';
// 「我的应用」Tab 的实现（合并去重清单）住在 services.js —— 它是原服务管理页
// 的继承者，数据字段（服务记录）也主要来自那一侧。
import { renderMyApps } from './services.js';
// 「应用管理」面板与启停/重启的唯一实现在 servicePanel.js —— 「我的应用」行点开的
// 是**同一个**面板（这是用户 2026-09-16 的核心要求：同一个应用的能力不分散在
// 两个页面）。卡片上通往它的入口**只有一个**「⚙️ 管理」：用户明确说原来的
// 「详情」与「查看服务」内容一样，"统一保留一个管理就行了"，所以那两个按钮都删了。
// 配置文件编辑器（configFileModal）住在 services.js，由面板内部复用，这里不再直接用。
//
// openDirectActions / subpathWarning / hasPanelUI 也来自 servicePanel.js：卡片上的
// 「打开 / 直链」与服务行、管理面板**必须**是同一份实现（用户 2026-09-17："打开/
// 直链/刷新/重启/停止/管理"这组固定语义，两处不能各写一套）。subpathWarning 是
// 按钮下方那行"为什么不支持子路径"的小字（用户第四条抱怨：只有 ⚠️ 没有解释）。
import { openServicePanel, marketQuickActions, openDirectActions, hasPanelUI, subpathWarning } from './servicePanel.js';

let cache = null;
// svcList 是这轮市场数据对应**完整服务记录**（api.services(true)，带健康检查）。
// 「我的应用」Tab 用它做合并去重、状态/端口/健康、以及管理面板的 svc 入参。
let svcList = [];
// proxyState 是 /api/v1/market/proxies 的探测结果（slug → {proxy_ok, reason}）。
// 它只服务于顶部「检测可用性 / 生成 nginx 入口」两个工具，**不再**决定「打开 /
// 直链」的归属：那个探测不带面板会话，子路径在面板端口上返回 401，proxy_ok
// 几乎恒为 false —— 用它决定归属会让 it-tools 这类应用的「打开」错变成端口直连。
let proxyState = null;
// svcState 是"这轮市场数据对应的服务状态"（服务名 → state），由 svcList 派生。
// 市场卡片的首颗动作按钮要在「启动」与「停止」之间选一个；拿不到状态时退化成
// 「启动」（后端对一个已经在跑的服务执行 start 是幂等的，不会因此谎报状态）。
let svcState = {};

// ---------------- 分类筛选（全部 / 原生 / Docker） ----------------
//
// 用户要求："应用市场加「全部 / 原生 / Docker」下拉筛选，用于快速只看某一类。"
// 只在前端过滤：市场数据本来就在手里（一次 GET /api/v1/market），
// 再加一个接口只会多一次往返、切换筛选还更慢。
//
// 选择要能保留（刷新浏览器、切页面再回来都不丢），所以存 localStorage。
// localStorage 在隐私模式 / 被禁用时会直接抛异常，因此读写都包 try：
// 存不了就退化成"仅本次会话生效"，功能降级但**不报错**（不谎报"已记住"）。
const KIND_FILTER_KEY = 'zp-market-kind-filter';
const KIND_FILTERS = ['', 'native', 'docker'];

function readKindFilter() {
  try {
    const v = localStorage.getItem(KIND_FILTER_KEY);
    return KIND_FILTERS.includes(v) ? v : '';
  } catch { return ''; }
}

function saveKindFilter(v) {
  try { localStorage.setItem(KIND_FILTER_KEY, v); } catch { /* 存不了就算了 */ }
}

let kindFilter = readKindFilter();

// kindGroupOf 把一个应用归到筛选档。
//
// KindColima（Colima 容器运行时）归到 **Docker** 档：它本身就是 Docker 引擎，
// 用户想看"Docker 类"时一定也想看到它；单独给它一档会让下拉变成四项，
// 而"原生 / Docker"问的是**怎么装**，Colima 显然是 Docker 那一侧。
// 未知 Kind 归原生档（当前目录里不存在这种条目）。
function kindGroupOf(a) {
  if (a.kind === 'compose' || a.kind === 'docker' || a.kind === 'colima') return 'docker';
  return 'native';
}

// kindFilterSelect 生成筛选下拉。放在函数里是因为头部每次刷新都会重画。
//
// onChange 由 AppsView 传进来（`renderGrid`）—— 这个函数在模块作用域，
// 拿不到视图闭包里的 renderGrid；直接在里面调用会抛 ReferenceError
// （下拉能改、localStorage 也写了，但列表纹丝不动）。
function kindFilterSelect(onChange) {
  const sel = h('select#apps-kind-filter.select', {
    style: { width: 'auto' },
    title: '只看某一类应用：原生 = Homebrew / 官方 darwin 二进制；'
      + 'Docker = 容器应用与 Colima 容器运行时',
  }, [
    h('option', { value: '', text: '全部', selected: kindFilter === '' }),
    h('option', { value: 'native', text: '原生', selected: kindFilter === 'native' }),
    h('option', { value: 'docker', text: 'Docker', selected: kindFilter === 'docker' }),
  ]);
  sel.addEventListener('change', () => {
    kindFilter = KIND_FILTERS.includes(sel.value) ? sel.value : '';
    saveKindFilter(kindFilter);
    if (typeof onChange === 'function') onChange();
  });
  return sel;
}

// ---------------------------------------------------------------------------
//  页内 Tab（用户 2026-09-17：服务管理 + 应用市场 → 一个「应用」版块）
// ---------------------------------------------------------------------------

// INSTALL_FILTERS 是应用市场的安装状态筛选，默认「未安装」——
// 市场这一栏的用途是"还能装什么"；已安装应用的常用动作在「我的应用」里。
const INSTALL_FILTERS = [
  { id: '', label: '全部' },
  { id: 'missing', label: '未安装' },
  { id: 'installed', label: '已安装' },
];
let installFilter = 'missing';

function installFilterSelect(onChange) {
  const sel = h('select#apps-install-filter.select', {
    style: { width: 'auto' },
    title: '按安装状态筛选：未安装 = 还能装的；已安装 = 装过的（常用动作在「我的应用」里）',
  }, INSTALL_FILTERS.map((f) => h('option', { value: f.id, text: f.label, selected: installFilter === f.id })));
  sel.addEventListener('change', () => {
    installFilter = INSTALL_FILTERS.some((f) => f.id === sel.value) ? sel.value : 'missing';
    if (typeof onChange === 'function') onChange();
  });
  return sel;
}

// isInstalled 是"已安装 / 已纳管"的唯一判据（市场卡片按钮、安装筛选共用）。
function isInstalled(a) { return !!(a && (a.installed || a.adopted)); }

export function AppsView(content, ctx = {}) {
  clear(content);

  // 默认落在「我的应用」；`#/services`（app.js 的 ROUTE_TARGET）显式给 tab='mine'，
  // 老书签因此仍然落在原服务管理的那份清单上。
  let active = ctx && ctx.tab === 'market' ? 'market' : 'mine';
  let loadError = null;
  let marketGrid = null;
  let marketHead = null;

  const tabBar = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '14px', flexWrap: 'wrap' } });
  const body = h('div');
  appendAll(content, tabBar, body);

  function renderTabBar() {
    clear(tabBar);
    for (const t of [{ id: 'mine', title: '我的应用' }, { id: 'market', title: '应用市场' }]) {
      tabBar.appendChild(h(`button.btn.btn-sm${active === t.id ? '.btn-primary' : ''}`, {
        dataset: { tab: t.id },
        text: t.title,
        onclick: () => { if (active === t.id) return; active = t.id; renderTabBar(); renderBody(); },
      }));
    }
  }

  function renderBody() {
    clear(body);
    if (active === 'mine') renderMineTab();
    else renderMarketTab();
  }

  function renderMineTab() {
    if (loadError && !cache) {
      appendAll(body, h('div.card', [h('div.card-body', [h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: loadError.message || String(loadError) }),
      ])])]));
      return;
    }
    // 合并去重、每行按钮、可纳管扫描都在 services.js 的 renderMyApps 里
    // （它同时握着市场条目与服务记录，见那边文件头的说明）。
    renderMyApps(body, { market: cache, list: svcList, onReload: load });
  }

  // rebuildSvcState 从完整服务记录派生"服务名 → state"（市场卡片首颗按钮用）。
  function rebuildSvcState() {
    const next = {};
    for (const s of svcList) if (s && s.name) next[s.name] = s.state || null;
    svcState = next;
  }

  // fetchAll 一次拉齐两边的数据：市场目录 + 服务记录（带健康检查）。
  // 两者**并行**，且一个失败不影响另一个（服务记录拉不到时「我的应用」只显示
  // 市场里的已安装条目，如实降级，不谎报）。
  //
  // 注意：这里用的是 api.services(true)（**带健康检查**，比市场自己以前用的
  // false 慢一点），因为「我的应用」每行要给出健康检查结果。两个请求并行发出，
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
    const docker = cache?.docker || {};
    const installed = (cache?.list || []).filter(isInstalled).length;
    appendAll(marketHead,
      h('span.pill', { text: `共 ${(cache?.list || []).length} 个应用` }),
      installed > 0 ? h('span.pill.ok', { text: `已安装 ${installed}` }) : null,
      h('span.pill' + (docker.available ? '.ok' : '.warn'), {
        text: docker.available ? `Docker ${docker.version || '已就绪'}` : 'Docker 未安装',
        title: docker.available ? 'Docker socket: ' + docker.socket : 'Docker 类应用需要先安装 Docker（推荐 OrbStack）',
      }),
      // 安装状态筛选（默认「未安装」）+ 「全部 / 原生 / Docker」筛选（按 Kind，
      // 选择记在 localStorage）。两个下拉都只做前端过滤，切换不重新请求。
      installFilterSelect(renderGrid),
      kindFilterSelect(renderGrid),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      // 「一键安装 LNMP 环境」已从这里**搬到「网站管理」页**（用户要求：它属于网站板块）。
      // 为什么市场里不再保留这个入口：LNMP 是**组合动作**（nginx + PHP + MySQL
      // + 默认站点 / vhosts / 系统级守护进程等收尾工作），不是单个可安装条目；
      // 同一件事在两处给入口只会让用户不知道该点哪。
      // 后端能力（POST /api/v1/market/install-lnmp）与目录条目都保留；目录里
      // 本来也没有 ID=lnmp 的 App（见 catalog.go「网站环境」段的说明），所以市场
      // 不会渲染出它的卡片；万一将来有人加进去，web 层的 marketHiddenApps 会挡下。
      // 原来的「⚙️ 服务管理」按钮已删：那一页就是本页的「我的应用」Tab，
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

  // adoptApp（纳管一个已在跑的服务）已删除（2026-09-17 合并时）：
  // 唯一的纳管入口是「我的应用 → 🔍 扫描可纳管服务」，实现住在 services.js 的
  // renderMyApps（openAdoptable）。市场卡片本来就不给「纳管」按钮 —— 卡片的
  // 按钮集合是固定那六颗（打开/直链/启停/重启/刷新/管理），而纳管是另一个语义
  // （登记一个已经存在的服务），不该混进应用的生命周期动作里。
  // 保留两份"确认纳管"弹窗只会在文案与行为上慢慢分叉，所以这里整段删掉。

  function installPhpMyAdmin(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'phpmyadmin',
      title: '部署 phpMyAdmin',
      start: () => api.installPhpMyAdmin(),
    });
  }

  function renderGrid() {
    if (!marketGrid) return;
    clear(marketGrid);
    const all = cache?.list || [];
    if (!all.length) {
      appendAll(marketGrid, h('div.empty', [h('div.big', { text: '🧩' }), h('h4', { text: '应用目录为空' })]));
      return;
    }

    // 先按安装状态（默认「未安装」）与「全部 / 原生 / Docker」筛选
    // （纯前端，见 isInstalled / kindGroupOf）。
    let list = all;
    if (installFilter === 'missing') list = list.filter((a) => !isInstalled(a));
    else if (installFilter === 'installed') list = list.filter(isInstalled);
    if (kindFilter) list = list.filter((a) => kindGroupOf(a) === kindFilter);

    if (!list.length) {
      appendAll(marketGrid, h('div.empty', [
        h('div.big', { text: '🔍' }),
        h('h4', { text: installFilter === 'missing' ? '没有可安装的新应用了' : '这一类暂时没有应用' }),
        h('p', {
          text: installFilter === 'missing'
            ? '目录里的应用都已经安装。把筛选切到「已安装」可以看它们的常用动作，或切到「全部」。'
            : '把上方的筛选切回「全部」就能看到全部应用。',
        }),
      ]));
      return;
    }

    // 分类板块的**顺序与中文名全部来自后端**（GET /api/v1/market 的 sections，
    // 单一来源在 internal/services/catalog.go 的 MarketSections）。
    // 前端不再自带一份分类中文名 —— 用户要求把最后一个板块「其它」改名为
    // 「基础环境」，改名只改 catalog.go 一处，这里自动跟着变，不会再漂。
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
        // 兜底板块收纳所有没有专属板块的分类（当前的 lnmp / runtime 都落在这里，
        // 也就是 nginx / PHP / MySQL / Colima 容器运行时 —— 它们正是「基础环境」）。
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
  //   残留态（产物还在、没装） → **安装**（安装器幂等，会复用残留产物）
  //   一键建站类              → 空（入口是卡片上的「一键建站」）
  //   已安装                  → **空**（卡片只留常用动作；安装生命周期收进「⚙️ 管理」）
  //   其它                    → 安装
  //
  // 2026-09-16 收敛（用户原话："截止 0.11.0 软件市场的显示有问题……Qwen3 TTS、
  // frpc、Orbien 客户端这几个不显示重装，查看服务 应改为 重装"）：
  // 改之前同一件"重装"被拆成三种文案 —— 已纳管给「查看服务」、已装且在 launchd
  // 给「纳管 + 重装」、已装但没服务才给「重装」。偏偏用户最常重装的那几个
  // （Qwen3 TTS / 接收端 / frpc / Orbien 都是"已纳管"）显示的是「查看服务」，
  // 于是重装入口等于不存在。当时主按钮只由 **a.installed** 决定：装了就给「重装」。
  //
  // 2026-09-17 再收敛（用户原话："每个应用只保留，打开、直链、刷新、重启、停止、
  // 管理……重装、卸载、文档等放进管理的弹出页面里"）：卡片上**不再**直接摆「重装」，
  // 它和「卸载 / 取消纳管 / 文档」一起收进「⚙️ 管理」面板（见 servicePanel.js 的
  // reinstallButton / uninstallButton / docLink）。所以已安装应用这里返回空，
  // 卡片剩下的就是那组固定语义的常用动作。安装入口（未安装 / 残留态）不受影响。
  function primaryButton(a) {
    const running = taskCenter.findByTarget(a.id);
    if (running) {
      return h('button.btn.btn-sm.btn-primary', {
        text: '⟳ 查看进度',
        title: '这个应用有正在进行的任务，点开看实时进度',
        onclick: () => taskCenter.openTask(running.id),
      });
    }
    if (residualOf(a)) {
      return h('button.btn.btn-sm.btn-primary', {
        text: '安装',
        disabled: !a.available,
        title: '磁盘上还有上次卸载保留的数据/产物；安装会复用它们，不会重复下载',
        onclick: () => openInstaller(a),
      });
    }
    // 一键建站类应用装出来是**网站**（目录 + 数据库 + vhost），入口在卡片上的
    // 「一键建站」；重装要走建站流程而不是安装器，所以主按钮留空。
    if (a.site_app) return null;
    // 已安装 → 卡片上不再有主按钮（安装生命周期动作都在「⚙️ 管理」面板里）。
    if (a.installed) return null;
    return h('button.btn.btn-sm.btn-primary', {
      text: '安装',
      disabled: !a.available,
      onclick: () => openInstaller(a),
    });
  }

  function appCard(a) {
    return h('div', {
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
          text: a.icon || '🧩',
        }),
        h('div', { style: { flex: 1, minWidth: 0 } }, [
          h('div', { style: { fontWeight: '620', fontSize: '14px' }, text: a.name }),
          h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '2px' }, text: a.summary }),
        ]),
      ]),
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        a.port > 0 ? h('span.pill', { text: ':' + a.port }) : null,
        a.kind === 'native' ? h('span.pill.brand', { text: '原生' }) : null,
        a.kind === 'compose' ? h('span.pill.brand', { text: 'Docker' }) : null,
        // 原生 brew 服务：记录在、但 plist 不在 = 服务其实没注册（ollama 就是这样，
        // 用户看到"已安装"却在服务管理里启动失败）。这一条要显式说出来。
        (a.kind === 'native' && a.service_label && a.installed && !a.service_in_launchd)
          ? h('span.pill.warn', {
            text: '已安装·服务未注册',
            title: '这个 brew 服务还没在 launchd 里注册（plist 不存在）。' +
              '到「服务管理」点一次启动即可自动注册（面板会用 brew services start 补上）',
          })
          : (a.adopted ? h('span.pill.ok', { text: '已纳管' })
          : (a.installed
            // 装了但服务没在 launchd 里（plist 丢了/没注册成功）是一种**孤儿态**：
            // 说"已安装·未纳管"会让人以为点一下纳管就行，而那个按钮必然报错。
            // 所以这里如实说"服务未注册"。
            //
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
                text: a.service_in_launchd ? '已安装·未纳管' : '已安装·服务未注册',
                title: a.service_in_launchd ? '' : '安装产物还在，但 launchd 里找不到这个服务；点「⚙️ 管理」里的「重装」可修复（会重建服务定义）',
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
      ]),
      // 卡片上只放**一句话**（summary）。以前把整段 description 铺在卡片里，
      // 一个条目的文字比按钮还多，用户要滚动半天才能看完一个应用（2026-09-16 反馈）。
      // 完整说明不丢：鼠标悬停给 title；已安装应用点「⚙️ 管理」里有「文档」，
      // 未安装应用卡片上直接给「文档」（它们没有管理入口）。
      (a.summary || a.description) ? h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.55' },
        title: a.description || '',
        text: a.summary || a.description,
      }) : null,
      h('div', { style: { display: 'flex', gap: '6px', marginTop: 'auto', paddingTop: '4px', flexWrap: 'wrap' } }, [
        primaryButton(a),
        // 界面入口：打开 / 直链 —— **同一份实现**在 servicePanel.js 的
        // openDirectActions（「我的应用」行与管理面板也用它）。只有已安装、
        // 且确实有面板托管界面的应用才有（frpc 这类自带控制台不算）。
        ...(a.installed ? openDirectActions(a) : []),
        // 已安装应用的常用动作：启动/停止、重启、刷新、⚙️ 管理。
        // 用户要求（2026-09-16）：卡片上直接给常用动作，**不需要先跳到别的页面**；
        // 而「⚙️ 管理」打开的是与「我的应用」行**同一个**面板 ——
        // 配置文件的编辑、凭据、日志、重装、文档、卸载都在那里面，不是两套按钮。
        // 2026-09-17 起卡片上只有这一组固定语义的按钮（打开/直链/启停/重启/刷新/管理），
        // 「重装」「文档」也从卡片收进了这个面板。
        ...marketQuickActions(a, {
          state: stateOfApp(a),
          onDone: refreshSilently,
          // 让「⚙️ 管理」走 openAppDetail：面板里的「重装」要用本页的安装器
          // 上下文（onReinstall，保留 Qwen 那类应用自己的选项框）。
          onManage: () => openAppDetail(a),
        }),
        ...siteInstallButtons(a),
        ...uninstallButtons(a),
        // 文档：**已安装**应用不再直接摆在卡片上（收进「⚙️ 管理」面板，见
        // servicePanel.js 的 docLink）；未安装应用没有「管理」入口，文档
        // 必须留在卡片上，否则用户就找不到这个应用的官方文档了。
        !a.installed && a.docs_url
          ? h('a.btn.btn-sm', { href: a.docs_url, target: '_blank', rel: 'noopener', text: '文档' })
          : null,
      ]),
      // prefer_direct（人工实测子路径不可用）时，按钮下方给一行**始终可见**的
      // 说明 —— 用户 2026-09-17 第四条抱怨：Miniflux / Syncthing / Alist / ddns-go
      // 显示「⚠️ 打开」却没有任何解释。只有 ⚠️ 与 title 不够，不悬浮就看不到。
      // 与服务行、管理面板同一份实现（servicePanel.subpathWarning）。
      subpathWarning(a),
    ]);
  }

  // openButtons 已删除（2026-09-17）：卡片上的「打开 / 直链」现在直接调用
  // servicePanel.js 的 openDirectActions —— 与「我的应用」行、管理面板同一份实现。
  // 旧的这份按 /api/v1/market/proxies 的探测结果决定"打开=子路径还是端口直连"，
  // 而那个探测不带面板会话（子路径返回 401 → proxy_ok 几乎恒为 false），
  // 结果把 it-tools 这类应用的「打开」错变成了端口直连、子路径降级成「试试子路径」。
  // 新规则见 openDirectActions 的说明：两颗按钮的语义固定，prefer_direct 只加警示。

  // ---------- 详情面板（卡片上的"全部动作"都收在这里）----------
  //
  // 2026-09-16 改版：以前卡片上按应用类型分两套按钮 ——
  // 有面板界面的给「打开」，没有的给「📝 编辑配置文件 + 🔄 重启服务」，
  // 而「停止/启动」当时只在服务管理页有。用户的原话是"能作的也就是：停止重启这些，
  // 直接放在软件页面不就行了？折腾什么？"，再加上 frpc 的配置入口在市场、
  // TTS 接收端的在服务管理 —— 同一个应用的能力被拆到了两个页面。
  //
  // 现在只有一个入口：**应用管理面板**（servicePanel.js）。卡片上给常用动作
  // （打开/直链/启停/重启/刷新）+「⚙️ 管理」，「我的应用」行点开的也是同一个面板。
  // 哪颗按钮出现仍然**全部由数据决定**（config_path / managed / ui.slug /
  // ui.console_only / 凭据接口是否为空），这里不再按应用 ID 写任何分支。
  //
  // hasPanelUI 从 servicePanel.js 导入（与面板、「我的应用」行**同一条判据**）；
  // openButtons 已删、openAppDetail 不再传 proxyState ——「打开 / 直链」的归属
  // 由数据（ui.slug / ui.prefer_direct / port_url）决定，与探测结果无关。

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
  //   · forget    —— **纳管的第三方服务**（nginx / php / mysql / 用户自己注册的）：
  //                  面板绝不卸载它们（删掉用户自己的 MySQL 等于删掉他的数据），
  //                  只给「取消纳管」，并在确认框里说清楚"只移除记录，不动系统"。
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
  // 重装、卸载、文档等放进管理的弹出页面里"）：已安装应用的「卸载 / 取消纳管」
  // 不再摆在卡片上 —— 它们已经（并继续）住在「⚙️ 管理」面板里：
  //   · managed=true          → 面板里的「🗑 卸载」（uninstallButton）
  //   · managed=false         → 面板里的「取消纳管」（forgetButton）
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
  async function doUninstall(a, plan, residual = false) {
    const remove = h('input', { type: 'checkbox' });
    const lines = (plan.steps || []).map((s) => h('li', { text: s }));
    const paths = plan.data_paths || [];
    const body = h('div', [
      residual
        ? h('div', {
          style: { marginBottom: '8px' },
          text: '「' + a.name + '」当前没有安装（服务和面板记录都不在），这一步只删除磁盘上的残留产物/数据，不可恢复。',
        })
        : h('div', { style: { marginBottom: '8px' }, text: '将执行：' }),
      residual ? null : h('ul', { style: { margin: '0 0 10px 18px', lineHeight: '1.7' } }, lines),
      (residual || !plan.keep_note) ? null : h('div.hint', { text: '会保留：' + plan.keep_note }),
      (!residual && paths.length)
        ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '10px' } }, [
          remove,
          h('span', { text: '同时删除数据/产物（不可恢复）：' }),
        ])
        : null,
      paths.length
        ? h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
          paths.map((p) => h('li.mono', { text: p })))
        : null,
    ]);
    const okGo = await new Promise((resolve) => {
      const m = modal({
        title: (residual ? '删除残留数据 · ' : '卸载 ') + a.name,
        body,
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
    // 残留清理的语义就是"删掉产物"，所以直接 remove_data=1，不再让用户勾选。
    const wipe = residual ? true : remove.checked;
    taskCenter.start({
      kind: 'uninstall',
      target: a.id,
      title: (residual ? '删除残留数据 ' : '卸载 ') + a.name,
      start: () => api.marketUninstall(a.id, wipe),
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

  // doForget（取消纳管）已删除（2026-09-17）：卡片上不再直接给「取消纳管」，
  // 它住在「⚙️ 管理」面板里（servicePanel.js 的 forgetButton，按 s.managed===false
  // 决定出现），实现只有那一份。卡片上唯一保留的残留态动作是「删除残留数据」。

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
  // 这样"重装"这个动作在两个入口（市场卡片 → 管理、「我的应用」行 → 管理）只有一份
  // 文案与一份确认语义，而应用自己的选项框仍然保留。

  // ---------- 安装前检查 ----------
  async function preflight(a) {
    const box = h('div', [h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: '正在检查安装条件…' })])]);
    const footer = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', width: '100%' } });

    let pf = null;
    const m = modal({
      title: `安装检查：${a.name}`,
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
          ? h('span.pill.ok', { text: '✅ 条件满足，可以安装' })
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
      a.post_install_hint ? h('div.hint', { style: { marginTop: '12px' }, text: '安装后：' + a.post_install_hint }) : null,
      a.manual_hint ? h('div.hint', { style: { marginTop: '8px' }, text: a.manual_hint }) : null,
    );

    clear(footer);
    appendAll(footer, 
      h('button.btn', { text: '关闭', onclick: () => m.close() }),
      pf.ready || a.adopt_label
        ? h('button.btn.btn-primary', {
          text: a.adopt_label ? '接入管理' : '开始安装',
          onclick: () => { m.close(); doInstall(a); },
        })
        : h('button.btn.btn-primary', {
          text: '仍然尝试安装',
          title: '条件不满足时安装可能失败，但你可以继续',
          onclick: () => { m.close(); doInstall(a); },
        }),
    );
  }

  // ---------- 执行安装 ----------
  //
  // 旧实现是"同步等请求 + 事后打印 steps"，用户在整个过程中看不到任何真实输出，
  // 关掉窗口也找不回来。现在改成：POST 立刻返回 task_id，进度交给任务中心。
  // 失败（4xx/5xx）由 taskCenter.start 统一 toast，这里不需要再兜一层。
  function doInstall(a) {
    taskCenter.start({
      kind: 'install',
      target: a.id,
      title: `安装 ${a.name}`,
      start: () => api.marketInstall(a.id),
      // 装完必须**立刻**把卡片状态刷新过来（用户 2026-09-16 要求：
      // "安装、卸载后面板中的软件状态要及时更新"）。以前只靠任务状态变化重画，
      // 而任务结束时市场数据还是旧的，卡片就停留在"可安装"。
      onDone: (m) => {
        if (m && m.status && m.status !== 'succeeded') {
          toast('安装失败：' + (m.error || m.status), 'err', 12000);
        } else {
          // 装完把"下一步"直接说出来。用户最常问的就是"装完我该干嘛"。
          // 2026-09-16 起配置入口只有一个：卡片上的「⚙️ 管理」→ 面板里的
          // 「📝 编辑配置文件」+ 启停按钮在同一屏，不再需要两个页面来回找。
          const next = a.config_path
            ? '：点卡片上的「⚙️ 管理」，在面板里改「📝 编辑配置文件」并重启服务生效'
            : '';
          toast('「' + a.name + '」已安装' + next, 'ok', 10000);
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
  // 只刷新市场会让「我的应用」停在旧状态（安装后不进清单）。
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
