// apps.js —— 应用市场页面。
//
// 核心思路：把"能不能装"和"装什么"分开。
//   - 用户点安装时，先跑一次安装前检查（依赖、端口），把问题一次说清
//   - 检查通过才真正安装；安装是**异步任务**，提交后进度交给「任务中心」，
//     用户可以关窗口、切页面，随时从顶栏重新打开看进度（见 tasks.js）
//   - 已经装过的应用显示"已安装"，可以一键跳到服务管理页

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup, panelPath } from './app.js';
import { taskCenter } from './tasks.js';
// 「应用管理」面板与启停/重启的唯一实现在 servicePanel.js —— 服务管理页点开的
// 是**同一个**面板（这是用户 2026-09-16 的核心要求：同一个应用的能力不分散在
// 两个页面）。卡片上通往它的入口**只有一个**「⚙️ 管理」：用户明确说原来的
// 「详情」与「查看服务」内容一样，"统一保留一个管理就行了"，所以那两个按钮都删了。
// 配置文件编辑器（configFileModal）住在 services.js，由面板内部复用，这里不再直接用。
import { openServicePanel, marketQuickActions } from './servicePanel.js';

let cache = null;
// proxyState 是 /api/v1/market/proxies 的探测结果（slug → {proxy_ok, reason}）。
// 有界面的应用给两个入口：子路径 /<slug>/ 与直连端口；哪个能用由探测说了算，
// 而不是"我们配了就假设它能打开"。
let proxyState = null;
// svcState 是"这轮市场数据对应的服务状态"（服务名 → state），来自一次
// api.services(false)（**不带健康检查**，所以很快）。
//
// 为什么市场页也要拉它：卡片上的第一颗动作按钮要在「启动」与「停止」之间选一个。
// 以前市场卡片只有"编辑配置 + 重启"，服务管理页才有启停 —— 用户的原话是
// "能作的也就是：停止重启这些，直接放在软件页面不就行了？"。
// 拿不到状态时（还没加载完 / 这个应用没有服务记录）退化成「启动」：
// 后端对一个已经在跑的服务执行 start 是幂等的，不会因此谎报状态。
let svcState = {};
let svcStateLoaded = false;

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
  const sel = h('select.select', {
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

// loadServiceStates 拉一次全部服务的状态（不带健康检查），并重画卡片。
// 失败就保持空表 —— 宁可按"未知"渲染，也不让整个市场页打不开。
async function loadServiceStates() {
  try {
    const res = await api.services(false);
    const next = {};
    for (const s of (res && res.list) || []) {
      if (s && s.name) next[s.name] = s.state || null;
    }
    svcState = next;
    svcStateLoaded = true;
  } catch {
    svcStateLoaded = false;
  }
}

export function AppsView(content, ctx = {}) {
  clear(content);

  const grid = h('div');
  const head = h('div.card-head', [
    h('h3', { text: '应用市场' }),
    h('div.spacer'),
    h('div', { id: 'apps-head', style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);

  appendAll(content, 
    h('div.card', [head, h('div.card-body', [grid])]),
  );
  const headBox = head.querySelector('#apps-head');

  async function load() {
    clear(grid);
    appendAll(grid, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取应用目录…' })]));
    try {
      cache = await api.market();
    } catch (e) {
      clear(grid);
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
      ]));
      return;
    }
    // 服务状态**后台补**（用来决定卡片上首颗按钮是「启动」还是「停止」）：
    // 市场数据一到就先画，不让状态查询挡住页面 ——
    // 以前市场页"打开较慢"的教训就是别在首屏串行等慢接口。
    // 拿不到状态时按钮退化成「启动」（后端 start 幂等，不会谎报）。
    loadServiceStates().then(() => { if (cache) renderGrid(); });
    // 探测是"能不能打开"的依据，但它要跑十几条网络请求（含 8 秒超时）。
    // **不在打开页面时自动跑** —— 用户反馈"应用市场打开较慢，其它页面都是秒开"。
    // 改为：结果缓存在内存里；点「检测可用性」时才真跑；生成入口后刷新一次。
    if (!proxyState) {
      proxyState = { enabled: true, items: [], _stale: true };
    }
    renderHead();
    renderGrid();
  }

  // stateOfApp 取这个应用在服务记录里的状态（给卡片上的启停按钮用）。
  //
  // 键要把三种写法都试一遍：面板记录名不一定是目录 ID（frpc 的记录名是
  // com.zizdog.frpc），与后端 FindAppByService 认的写法保持一致。
  function stateOfApp(a) {
    for (const k of [(a.uninstall && a.uninstall.service) || '', a.service_label || '', a.id || '']) {
      if (k && svcState[k]) return svcState[k];
    }
    return null;
  }

  function renderHead() {
    clear(headBox);
    const docker = cache?.docker || {};
    const installed = (cache?.list || []).filter((a) => a.installed).length;
    appendAll(headBox, 
      h('span.pill', { text: `共 ${(cache?.list || []).length} 个应用` }),
      installed > 0 ? h('span.pill.ok', { text: `已安装 ${installed}` }) : null,
      h('span.pill' + (docker.available ? '.ok' : '.warn'), {
        text: docker.available ? `Docker ${docker.version || '已就绪'}` : 'Docker 未安装',
        title: docker.available ? 'Docker socket: ' + docker.socket : 'Docker 类应用需要先安装 Docker（推荐 OrbStack）',
      }),
      // 「全部 / 原生 / Docker」筛选（按 Kind 在前端过滤，选择记在 localStorage）。
      kindFilterSelect(renderGrid),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      // 「一键安装 LNMP 环境」已从这里**搬到「网站管理」页**（用户要求：它属于网站板块）。
      // 为什么市场里不再保留这个入口：LNMP 是**组合动作**（nginx + PHP + MySQL
      // + 默认站点 / vhosts / 系统级守护进程等收尾工作），不是单个可安装条目；
      // 同一件事在两处给入口只会让用户不知道该点哪。
      // 后端能力（POST /api/v1/market/install-lnmp）与目录条目都保留；目录里
      // 本来也没有 ID=lnmp 的 App（见 catalog.go「网站环境」段的说明），所以市场
      // 不会渲染出它的卡片；万一将来有人加进去，web 层的 marketHiddenApps 会挡下。
      h('button.btn.btn-sm', { text: '⚙️ 服务管理', onclick: () => { location.hash = '#/services'; } }),
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

  // adoptApp 把一个已安装但未登记的服务纳管进来。
  //
  // 2026-09-16 起市场卡片**不再**直接给「纳管」按钮：用户要求已安装应用的
  // 主按钮统一成「重装」，而「纳管」是另一个语义（登记一个已经在跑的服务）。
  // 这个实现保留在这里，因为 api.adopt 仍被「服务管理 → 扫描可纳管服务」使用，
  // 两处的行为必须一致；以后若要把纳管放回面板，直接用这个函数即可。
  //
  // 这是"自动纳管的兜底"：面板启动时会自动登记目录里已知的服务，
  // 但如果服务是后装的、或标签不在目录里，就得靠它。
  function adoptApp(a) {
    const label = a.service_label || a.adopt_label;
    if (!label) { toast('这个应用没有可纳管的服务标签', 'warn'); return; }
    modal({
      title: `纳管「${a.name}」`,
      body: h('div', { style: { fontSize: '12.5px', lineHeight: '1.8' } }, [
        h('p', { text: `将把本机正在运行的 ${label} 登记到「服务管理」。` }),
        h('p', { style: { color: 'var(--text-mute)' },
          text: '面板只做启停与查看，不会卸载它、也不会改动它的启动方式。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '确认纳管',
          onclick: async () => {
            close();
            try {
              await api.adopt({ label, display_name: a.name, icon: a.icon, port: a.port, category: a.category });
              toast(`已纳管「${a.name}」`, 'ok');
              load();
            } catch (e) {
              toast(e.message, 'err', 12000);
            }
          },
        }),
      ],
    });
  }

  function installPhpMyAdmin(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'phpmyadmin',
      title: '部署 phpMyAdmin',
      start: () => api.installPhpMyAdmin(),
    });
  }

  function renderGrid() {
    clear(grid);
    const all = cache?.list || [];
    if (!all.length) {
      appendAll(grid, h('div.empty', [h('div.big', { text: '🧩' }), h('h4', { text: '应用目录为空' })]));
      return;
    }

    // 先按「全部 / 原生 / Docker」筛选（纯前端，见 kindGroupOf）。
    const list = kindFilter ? all.filter((a) => kindGroupOf(a) === kindFilter) : all;
    if (!list.length) {
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '🔍' }),
        h('h4', { text: '这一类暂时没有应用' }),
        h('p', { text: '把上方的筛选切回「全部」就能看到全部应用。' }),
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
      appendAll(grid,
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
      appendAll(grid,
        h('div.section-title', { style: { marginTop: grid.childElementCount ? '20px' : '0' }, text: s.label }),
        h('div.grid.grid-3', items.map((a) => appCard(a))),
      );
    }
    // 保险：后端万一没给兜底板块，把剩下的条目并进最后一个已有板块，
    // 保证任何应用都不会"因为板块定义缺失而从页面上消失"。
    const rest = list.filter((a) => !covered.has(a));
    if (rest.length) {
      const title = fallback ? fallback.label : (sections[sections.length - 1].label || '');
      appendAll(grid,
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
  //   已安装                  → **重装**（安装器幂等，保留数据）
  //   其它                    → 安装
  //
  // 2026-09-16 收敛（用户原话："截止 0.11.0 软件市场的显示有问题……Qwen3 TTS、
  // frpc、Orbien 客户端这几个不显示重装，查看服务 应改为 重装"）：
  // 改之前同一件"重装"被拆成三种文案 —— 已纳管给「查看服务」、已装且在 launchd
  // 给「纳管 + 重装」、已装但没服务才给「重装」。偏偏用户最常重装的那几个
  // （Qwen3 TTS / 接收端 / frpc / Orbien 都是"已纳管"）显示的是「查看服务」，
  // 于是重装入口等于不存在。现在主按钮只由 **a.installed** 决定：装了就给「重装」。
  //
  // 为什么删掉「查看服务」：它和「⚙️ 管理」打开的是**同一个**面板
  // （servicePanel.js 的 openServicePanel）。用户明确说"原来的详情和查看服务
  // 内容一样，统一保留一个管理就行了"——两个通向同一面板的按钮只会让人
  // 不知道该点哪个。
  //
  // 为什么删掉「纳管」兜底：它只在"服务确实在 launchd 里、但面板没有记录"时出现，
  // 属于**另一个语义**（把已运行的服务登记进来），不是重装。用户要求主按钮统一，
  // 而登记能力并没有丢：「服务管理 → 扫描可纳管服务」走的是同一个 api.adopt。
  // （adoptApp 的实现保留在本文件里，两处语义必须一致。）
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
    // 已安装 → 主按钮统一「重装」。
    //
    // 判据只看 a.installed（后端给的是：面板服务记录 / launchd 里的作业 /
    // brew formula 三者之一），**不看** adopted / service_in_launchd / no_daemon ——
    // 命令行工具（ffmpeg）与纳管服务（qwen3tts）在这颗按钮上的语义完全一样：
    // 再跑一遍幂等的安装器（已下载的产物会复用、数据保留）。
    // 以前按这三种状态给三种文案，正是用户看到的"有的显示查看服务、有的没有重装"。
    if (a.installed) {
      return h('button.btn.btn-sm', {
        text: '重装',
        title: '重新跑一遍安装（会复用已下载的产物、保留数据，不会重复下载）',
        onclick: () => reinstallApp(a),
      });
    }
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
                title: a.service_in_launchd ? '' : '安装产物还在，但 launchd 里找不到这个服务；用卡片上的「重装」可修复（会重建服务定义）',
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
      // 完整说明不丢：鼠标悬停给 title，想细看可以点「文档」。
      (a.summary || a.description) ? h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.55' },
        title: a.description || '',
        text: a.summary || a.description,
      }) : null,
      h('div', { style: { display: 'flex', gap: '6px', marginTop: 'auto', paddingTop: '4px', flexWrap: 'wrap' } }, [
        primaryButton(a),
        // 界面入口（有面板托管界面的应用才有）。
        ...openButtons(a),
        // 已安装应用的常用动作：启动/停止、重启、刷新、⚙️ 管理。
        // 用户要求（2026-09-16）：卡片上直接给常用动作，**不需要先跳到服务管理页**；
        // 而「⚙️ 管理」打开的是与服务管理页**同一个**面板 ——
        // 配置文件的编辑、凭据、日志、卸载都在那里面，不是两套按钮。
        // 2026-09-16 起卡片上只有这一颗"进面板"的按钮（原来的「详情」「查看服务」
        // 合并成它），主按钮则统一是「重装」/「安装」。
        ...marketQuickActions(a, {
          state: stateOfApp(a),
          onDone: refreshSilently,
          // 让「⚙️ 管理」走 openAppDetail：它会把市场页的界面探测结果
          // （proxyState）一起带进面板，面板里的「打开界面」才能和卡片上的
          // 「打开」给同一个结论（子路径还是端口直连）。
          onManage: () => openAppDetail(a),
        }),
        ...siteInstallButtons(a),
        ...uninstallButtons(a),
        a.docs_url ? h('a.btn.btn-sm', { href: a.docs_url, target: '_blank', rel: 'noopener', text: '文档' }) : null,
      ]),
    ]);
  }

  // openButtons 给"有界面且已装"的应用两个入口：子路径与直连端口。
  //
  // 为什么主按钮可能指向直连：有些应用必须自己设 base path 才能挂子路径
  // （n8n / Gitea / MinIO / Stirling 都是），硬挂会白屏。
  // 探测（/api/v1/market/proxies）说不行，就把直连作为首选入口，
  // 并在 title 里说清原因 —— 而不是给一个点开是白屏的按钮。
  function openButtons(a) {
    if (!hasPanelUI(a) || !a.installed) return [];
    const st = (proxyState?.items || []).find((x) => x.slug === a.ui.slug) || {};
    const path = '/' + a.ui.slug + '/';
    const direct = a.port_url || '';
    // SelfConf（phpMyAdmin）：它的 location 由安装器直接写进 nginx，
    // 面板不反代，所以只能用 nginx 的**绝对地址** —— 用相对路径会打到
    // 面板自己的 SPA 回落上（返回 200 却是面板首页，极具误导性）。
    if (a.ui.self_conf) {
      // SelfConf 的应用（phpMyAdmin）：它的 nginx location 只允许本机，
      // 所以唯一能用的入口是**面板自己**那条（要求先登录面板）。
      // 用户反馈过这里给出的是局域网地址 http://192.168.1.4/phpmyadmin/ → 403。
      return [h('a.btn.btn-sm.btn-primary', {
        href: panelPath('phpmyadmin/'), target: '_blank', rel: 'noopener', text: '打开',
        title: '经面板打开（需先登录面板；面板会反代到本机的 phpMyAdmin）',
      })];
    }
    // prefer_direct 是**人工实测**的结论（自动探测发现不了"资源全 200 但
    // 前端路由不认这个前缀"的情况），所以它的优先级高于探测结果。
    // 尚未探测过（_stale）时按"能用"对待 —— 否则没点过检测的用户会看到所有应用
    // 都被降级成"直连端口"（这不是我们想给的默认结论）。
    const probed = !proxyState?._stale;
    const proxyOK = !a.ui.prefer_direct && (probed ? !!st.proxy_ok : true);
    const why = a.ui.prefer_direct ? (a.ui.note || '这个应用不支持子路径') : (st.reason || a.ui.note || '');
    const out = [];
    if (proxyOK) {
      out.push(h('a.btn.btn-sm.btn-primary', {
        href: path, target: '_blank', rel: 'noopener', text: '打开',
        title: '经面板的 /' + a.ui.slug + '/ 打开（所有入口都通：80 端口、面板端口、隧道）',
      }));
      if (direct) {
        out.push(h('a.btn.btn-sm', { href: direct, target: '_blank', rel: 'noopener', text: '直连端口', title: '绕过面板直接访问：' + direct }));
      }
    } else if (direct) {
      // 子路径不可用（应用需要自己设 base path，或前端路由不认这个前缀）。
      //
      // 既然"子路径不可用"是**人工实测/探测**得出的结论（PreferDirect 或探测失败），
      // 主入口就必须是**端口直连** —— 否则用户点「打开」拿到的是一个已知打不开的
      // 子路径，与 AppUI.PreferDirect 的字段文档（"「打开」直接给端口直连，
      // 子路径降级成次要入口"）自相矛盾。子路径保留成"试试"按钮，
      // 万一以后上游支持了或探测结论变了，仍有一条入口。
      out.push(h('a.btn.btn-sm.btn-primary', {
        href: direct, target: '_blank', rel: 'noopener', text: '打开',
        title: '直连应用端口：' + direct + (why ? '（' + why + '）' : ''),
      }));
      out.push(h('a.btn.btn-sm', {
        href: path, target: '_blank', rel: 'noopener', text: '试试子路径',
        title: why || '子路径可能不可用',
      }));
    } else {
      out.push(h('a.btn.btn-sm', {
        href: path, target: '_blank', rel: 'noopener', text: '打开',
        title: why || '应用可能没有启动',
      }));
    }
    return out;
  }

  // ---------- 详情面板（卡片上的"全部动作"都收在这里）----------
  //
  // 2026-09-16 改版：以前卡片上按应用类型分两套按钮 ——
  // 有面板界面的给「打开」，没有的给「📝 编辑配置文件 + 🔄 重启服务」，
  // 而「停止/启动」只在服务管理页有。用户的原话是"能作的也就是：停止重启这些，
  // 直接放在软件页面不就行了？折腾什么？"，再加上 frpc 的配置入口在市场、
  // TTS 接收端的在服务管理 —— 同一个应用的能力被拆到了两个页面。
  //
  // 现在只有一个入口：**应用管理面板**（servicePanel.js）。卡片上给常用动作
  // （启停/重启/刷新）+「⚙️ 管理」，服务管理页点开的也是同一个面板。
  // 哪颗按钮出现仍然**全部由数据决定**（config_path / managed / ui.slug /
  // ui.console_only / 凭据接口是否为空），这里不再按应用 ID 写任何分支。

  // hasPanelUI 判断"这个应用有没有**本面板提供**的网页使用入口"。
  //
  // 与 a.ui 的区别：frpc 有 UI（它自己的 7400 控制台，目录里标了
  // console_only），但那不是面板的使用入口 —— 用户明确不要在面板里跳过去。
  // 只有 IOPaint / Uptime Kuma / phpMyAdmin 这类"面板自己托管/代理"的界面
  // 才保留「打开」。servicePanel.js 的 uiButtonFor 用的是**同一条判据**。
  function hasPanelUI(a) {
    return !!(a.ui && a.ui.slug && !a.ui.console_only);
  }

  // openAppDetail 打开「应用管理」面板 —— 市场卡片上唯一的"进面板"入口。
  //
  // 面板是**唯一的应用操作入口**，这里只负责把市场这份数据递进去，并告诉它
  // 两件事：① 打开界面时用探测结果（proxyState，决定子路径还是端口直连）；
  // ② 动作完成后刷新卡片。服务记录由面板自己按名字去查
  // （市场条目里的 config_path 只是文件名，绝对路径只有服务记录才有）。
  //
  // 2026-09-16：卡片上的「⚙️ 管理」按钮通过 marketQuickActions 的 onManage
  // 回调走到这里 —— 保留这条路径而不是让按钮直接 openServicePanel，
  // 就是为了 proxyState 不丢（否则面板里的「打开界面」会和卡片上的「打开」不一致）。
  function openAppDetail(a, opts = {}) {
    return openServicePanel({
      market: a,
      proxyState,
      onDone: refreshSilently,
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

  function uninstallButtons(a) {
    const plan = a.uninstall || {};
    const residual = residualOf(a);
    // 残留态也要有删除入口：按新语义它显示为"未安装"，「卸载」按钮就没了，
    // 那样用户永远清不掉上次卸载保留的产物/数据（2026-09-16 用户反馈）。
    if (!a.installed && !residual) return [];
    if (plan.kind === 'service' || plan.kind === 'installer') {
      return [h('button.btn.btn-sm.btn-danger', {
        text: residual ? '删除残留数据' : '卸载',
        title: residual
          ? '这个应用当前没有安装；只删除磁盘上的残留产物/数据'
          : (plan.blocked || '卸载「' + a.name + '」（会列出具体删除内容并要求确认）'),
        disabled: !!plan.blocked && !residual,
        onclick: () => doUninstall(a, plan, residual),
      })];
    }
    if (plan.kind === 'forget') {
      return [h('button.btn.btn-sm', {
        text: '取消纳管',
        title: '只把这个服务从面板记录里移除，不动系统上的任何东西',
        onclick: () => doForget(a, plan),
      })];
    }
    return [];
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

  // doForget 取消纳管：只删面板记录，不动系统。
  async function doForget(a, plan) {
    const okGo = await confirmBox(
      '把「' + a.name + '」从面板记录里移除？\n\n' +
      '面板不会卸载你自己安装的软件（不跑 brew uninstall、不删文件），' +
      '只是不再管它。要真正删除请在终端里自行处理。',
      { title: '取消纳管', okText: '取消纳管' });
    if (!okGo) return;
    const name = plan.service || a.id;
    try {
      await api.serviceForget(name);
      toast('已取消纳管', 'ok');
      refreshSilently();
    } catch (e) {
      toast('取消失败：' + e.message, 'err', 9000);
    }
  }

  // openInstaller 按应用打开对应的部署对话框。
  //
  // 抽出来是因为有**两个入口**要用它：
  //   · 「安装」——没装过的应用，以及"残留数据"（artifacts && !installed）；
  //   · 「重装」——已安装的应用（见 reinstallApp；它确认完也走这里）。
  // 安装器本身是幂等的：重跑会重建 venv/服务定义/plist 并登记到服务管理，
  // 所以"重装"就是最合理的修复动作，不需要另写一套修复逻辑。
  // （2026-09-16 起不再有单独的「重新部署」按钮：它与「重装」是同一个安装器，
  // 用户要求已安装应用的主按钮统一成「重装」，所以那个文案已删。）
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
  // 重装与安装走**同一条安装器**（它是幂等的：已下载的产物会复用、
  // 已存在的配置与数据不动），所以这里做的只是"把后果说清楚再跑一次"。
  // 用户明确要求有这个入口（2026-09-16）：装了但想修、或想更新配置时，
  // 以前只能先卸载再装，中间那段时间服务是停的。
  async function reinstallApp(a) {
    const okGo = await confirmBox(
      '重装「' + a.name + '」？\n\n' +
      '· 会重新跑一遍安装流程（下载/解压/重建服务定义）\n' +
      '· **已下载的产物会复用**，不会重复下载\n' +
      '· 已存在的配置与数据**保留**\n' +
      '· 服务会重启一次',
      { title: '重装 ' + a.name, okText: '开始重装' });
    if (!okGo) return;
    doInstall(a);
  }

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

  // refreshSilently 重新拉一次市场数据并重画卡片，**不显示"正在读取"占位**。
  //
  // 为什么要单独一个：load() 会先 clear(grid) 再显示 loading 占位，
  // 任务刚结束时调用它，用户会看到整页闪一下白 —— 而他要的只是"状态更新"。
  async function refreshSilently() {
    try {
      cache = await api.market();
    } catch {
      return; // 拉不到就保持旧画面，不要把一个空网格拍给用户
    }
    renderHead();
    renderGrid();
  }

  // 任务状态变化（开始/结束）时重画卡片：正在安装的应用，按钮要变成「查看进度」。
  // 只订阅元信息变化，**不订阅日志行** —— 否则 brew 每输出一行都会重建整个网格。
  // 这是页面级订阅，跟着页面一起清理：任务状态本身活在 tasks.js 的单例里，
  // 清理掉的只是"这个页面要不要重画"。
  registerCleanup(taskCenter.onChange((kind) => {
    if (kind === 'lines' || !cache) return;
    renderGrid();
  }));
  load();
}
