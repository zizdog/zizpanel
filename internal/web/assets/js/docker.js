// docker.js —— Docker 管理页。
//
// 与「应用 → 我的应用」的分工（刻意如此，避免两个页面互相打架）：
//   · 我的应用 管**服务生命周期**：把某个容器/compose 项目当作一条服务来启停、看日志、健康检查。
//   · 本页 管**Docker 本身**：这台机器上有哪些容器/镜像/卷/网络、镜像拉取与清理、
//     compose 文件的编辑与部署、以及从零创建一个容器。
//
// 本页**不再有「已纳管服务」分区**（2026-09-19 删掉）：服务管理已合并进
// 「应用 → 我的应用」，在这里再放一个跳转入口等于让用户在两处找同一个东西；
// 容器/项目的状态在「容器」「Compose」分区里本来就看得见。同理，环境不可用时
// 不再有「去服务管理」这种指路按钮（那个页面已经不存在了）。
//
// 环境不可用（macOS 上没装 Docker 是常态）时首屏显示的是**一键安装**，
// 而不是"去哪里装"的说明：用户实测的困难正是"找不到 Docker"（原话：
// "不应该显示『去应用市场』……应该直接显示『一键安装 docker』"）。
// 装的是面板已有的 Docker 运行时安装任务（Colima），走任务中心、有进度、
// 失败会如实报错；装完本页自己刷新到可用状态，用户不用去别的页面找。

import { api } from './api.js';
import { h, clear, toast } from './ui.js';
import { registerCleanup } from './app.js';
import { taskCenter } from './tasks.js';
import { renderContainers } from './docker-containers.js';
import { renderImages } from './docker-images.js';
import { renderVolumes } from './docker-volumes.js';
import { renderNetworks } from './docker-networks.js';
import { renderCompose } from './docker-compose.js';
import { renderMirrors } from './docker-mirrors.js';

const TABS = [
  { id: 'containers', title: '容器', icon: '📦', render: renderContainers, count: 'containers' },
  { id: 'images', title: '镜像', icon: '💿', render: renderImages, count: 'images' },
  { id: 'volumes', title: '数据卷', icon: '🗃️', render: renderVolumes, count: 'volumes' },
  { id: 'networks', title: '网络', icon: '🔌', render: renderNetworks, count: 'networks' },
  { id: 'compose', title: 'Compose', icon: '🧩', render: renderCompose, count: 'compose_projects' },
  // 加速源：用户抱怨「拉取太慢」的直接对策（给 docker 守护进程配 registry-mirrors）
  { id: 'mirrors', title: '加速源', icon: '🚀', render: renderMirrors, count: null },
];

export function DockerView(content, ctx = {}) {
  // 本轮打开过的 EventSource / 定时器，切页时统一收掉，避免后台连接泄漏。
  const cleanups = [];
  registerCleanup(() => {
    cleanups.forEach((fn) => { try { fn(); } catch { /* 清理失败不影响切页 */ } });
  });
  const trackCleanup = (fn) => cleanups.push(fn);

  clear(content);

  const headBox = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
  const tabBar = h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap', marginBottom: '12px' } });
  const pane = h('div');

  const tabButtons = new Map();
  TABS.forEach((t) => {
    const btn = h('button.btn.btn-ghost.btn-sm', {
      text: `${t.icon} ${t.title}`,
      onclick: () => { active = t.id; render(); },
    });
    tabButtons.set(t.id, btn);
    tabBar.append(btn);
  });

  content.append(
    h('div.card', [
      h('div.card-head', [h('h3', { text: 'Docker' }), h('div.spacer'), headBox]),
      h('div.card-body.tight', [tabBar, pane]),
    ]),
  );

  let active = 'containers';

  async function render() {
    clear(headBox);
    clear(pane);
    tabButtons.forEach((btn, id) => {
      btn.className = id === active ? 'btn btn-primary btn-sm' : 'btn btn-ghost btn-sm';
    });

    const tab = TABS.find((t) => t.id === active);
    if (!tab) return;

    // 每次切换都重新探一次环境：用户很可能刚去装好了 Docker 再回来。
    let info;
    try {
      info = await api.dockerInfo();
    } catch (e) {
      pane.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取 Docker 环境失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }

    if (!info.available) {
      renderUnavailable(pane, info, render);
      return;
    }

    const counts = info.counts || {};
    headBox.append(h('span.pill.ok', {
      text: `Docker ${info.version || '已就绪'}`,
      title: 'socket: ' + info.socket,
    }));
    if (tab.count && counts[tab.count] != null) {
      const n = tab.id === 'containers'
        ? `${counts.containers_running || 0}/${counts.containers}`
        : String(counts[tab.count]);
      headBox.append(h('span.pill', { text: n, title: '当前分区的条目数' }));
    }
    headBox.append(h('button.btn.btn-ghost.btn-sm', { text: '刷新', onclick: render }));

    try {
      await tab.render(pane, {
        info,
        refresh: render,
        trackCleanup,
        goTab: (id) => { active = id; render(); },
      });
    } catch (e) {
      clear(pane);
      pane.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '加载失败' }),
        h('p', { text: e.message }),
      ]));
    }
  }

  render();
}

// runtimeOf 取出后端给的环境探测结果（GET /api/v1/docker/info 的 data.runtime）。
//
// 它是**真实探测**的结论（二进制在不在 / socket 在不在且能不能连），
// 所以界面能区分三态：没装（一键安装）/ 装了但引擎没跑（启动）/
// 在跑（正常显示内容）。老后端没有这个字段时返回空对象，下面的判定会安全退化。
function runtimeOf(info) {
  const r = info && info.runtime;
  return (r && typeof r === 'object') ? r : {};
}

// renderUnavailable 显示"环境不可用"的引导卡片。
//
// 这是 macOS 上的**常态**（Docker 不是系统自带），所以第一屏必须给出
// **一个能立刻做完的动作**，而不是只说"失败了"、更不是指路到别的页面。
//
// 三态文案的差别很重要（2026-09-19 用户实测）：
//   · 没装 → 主按钮「🐳 一键安装 Docker（Colima）」；
//   · 装了但引擎没跑 → 主按钮「▶ 启动 Docker 运行时」（装是装了的，
//     再让他"安装"一遍就是把用户绕回同一个地方）；
//   · 状态探测不到（老后端） → 仍给一键安装（幂等，不会重复装）。
function renderUnavailable(container, info, refresh) {
  const msg = info.error || '未检测到可用的 Docker 环境';
  const rt = runtimeOf(info);
  const state = rt.state || info.runtime_state || '';
  // 只有"明确知道二进制已经在了"才显示启动；其余一律给安装（安装是幂等的）。
  const installed = rt.binary_installed === true;
  const appId = info.runtime_app_id || 'docker-runtime';

  clear(container);

  const actions = h('div', { style: { display: 'flex', gap: '8px', marginTop: '14px', flexWrap: 'wrap' } });
  if (installed) {
    actions.append(h('button.btn.btn-primary', {
      text: '▶ 启动 Docker 运行时',
      title: '启动 Colima 虚拟机（首次启动约需 40 秒）',
      onclick: () => startRuntime(appId, refresh),
    }));
  } else {
    actions.append(h('button.btn.btn-primary', {
      text: '🐳 一键安装 Docker（Colima）',
      title: '下载 Colima + Lima 虚拟机并启动引擎，装完自动开机自启',
      onclick: () => installRuntime(appId, refresh),
    }));
  }
  // 「刷新」必须留在同一个页面上：装完（或在别处启动完）用户不该被要求
  // 去别的页面找状态，点一下这里就能重新探一次。
  actions.append(h('button.btn.btn-ghost', { text: '刷新状态', onclick: () => refresh() }));

  // 如实写清代价：这几条都是用户装机时真实会遇到的（下载体积、首次启动耗时、
  // 装完会自动开机自启）。写清楚比事后解释"为什么这么久"要好。
  const steps = installed
    ? [
      '「启动 Docker 运行时」会拉起 Colima 的 Linux 虚拟机（首次启动约需 40 秒）。',
      '启动过程中进度与结果都在顶栏「任务中心」，可以关掉本页去做别的。',
      '起来之后回到本页点「刷新状态」，容器/镜像/Compose 分区就都能用了。',
    ]
    : [
      '「一键安装 Docker（Colima）」会装好 Colima + Lima + Docker 命令行 + docker-compose（下载约 1–3 GB，视网络而定）。',
      '随后自动创建并启动 Linux 虚拟机：首次启动约需 40 秒（若需从公网拉虚拟机镜像会更久，进度在任务中心里）。',
      '装完自动配置开机自启（系统级，无需登录桌面），本页会自己刷新到可用状态。',
      '失败不会假装成功：任务中心会给出具体失败原因，可以修好网络后重试（安装是幂等的）。',
    ];

  container.append(
    h('div.empty', [
      h('div.big', { text: '🐳' }),
      h('h4', { text: state === 'stopped' ? 'Docker 运行时没在运行' : 'Docker 环境不可用' }),
      h('p', { text: msg }),
    ]),
    h('div', { style: { maxWidth: '680px', margin: '0 auto 18px' } }, [
      h('div.hint', {
        text: installed
          ? 'Colima 已经装在这台机器上，只是 Docker 引擎没起来。直接启动它即可：'
          : 'macOS 上 Docker 引擎不是系统自带的，需要一个 Linux 虚拟机来承载。这里可以直接装好：',
      }),
      h('ol', { style: { paddingLeft: '20px', lineHeight: '2', color: 'var(--text-dim)', fontSize: '13px' } },
        steps.map((t) => h('li', { text: t }))),
      actions,
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '当前探测结果' })]),
      h('div.card-body.tight', [
        h('table.table', [h('tbody', [
          h('tr', [h('td', { text: 'colima 命令' }), h('td', { text: rt.bin_path || (installed ? '在' : '未安装') })]),
          h('tr', [h('td', { text: 'socket 路径' }), h('td', [h('code', { text: info.socket || rt.socket_path || '（未探测到）' })])]),
          h('tr', [h('td', { text: '引擎版本' }), h('td', { text: info.version || rt.engine_version || '—（未知，需引擎在跑才能读到）' })]),
          h('tr', [h('td', { text: '虚拟机' }), h('td', { text: rt.vm_state || '未知' })]),
          h('tr', [h('td', { text: '错误' }), h('td', { text: msg })]),
        ])]),
      ]),
    ]),
  );
}

// installRuntime 一键安装 Docker 运行时（Colima）。
//
// 走已有的安装任务（POST /api/v1/market/docker-runtime/install → 任务中心），
// 所以进度、日志、失败原因、并发去重都由任务中心那一套统一负责 ——
// 这里不自己写轮询、也不自己编成功（铁律：功能不许谎报成功）。
function installRuntime(appId, refresh) {
  toast('已提交安装任务：Colima + Lima + Docker 命令行，下载约 1–3 GB；' +
        '进度与结果都在顶栏「任务中心」。', 'info', 9000);
  taskCenter.start({
    kind: 'install',
    target: appId || 'docker-runtime',
    title: '安装 Docker 运行时（Colima）',
    start: () => api.marketInstall(appId || 'docker-runtime'),
    // 只在**任务真的成功**时刷新：失败时如实停在引导页（原因在任务中心里），
    // 绝不假装已经可用。
    onDone: (meta) => {
      if (meta && meta.status === 'succeeded') {
        toast('Docker 运行时已就绪，正在刷新本页…', 'ok', 5000);
        // 等引擎的 socket 就绪再探：安装任务的最后一步已经验证过 API 可用，
        // 这里多给一秒是为了让探测不是"刚好卡在边界上"。
        setTimeout(() => { refresh(); }, 1000);
      }
    },
  });
}

// startRuntime 启动一个**已经装好**的 Docker 运行时（Colima 虚拟机）。
//
// 与"安装"分开：装了但引擎没起来时，用户要的是启动，不是再下载一遍。
//
// 刻意**不走任务中心**：服务动作接口（POST /api/v1/services/{name}/start）是
// 同步返回的（它直接给 state/warning），hardcode 成 202+task_id 的 taskCenter
// 契约会拿到"服务端没有返回任务编号"的假告警。这里如实做到哪一步说到哪一步：
//   · 有 warning（后端请求成功但没确认真的起来了）→ 原样转述，不报干净的"已启动"；
//   · 成功 → 重新探一次环境（探测是权威的，不看我们的预期）；
//   · 失败 → 如实报错并停在这个页面。
async function startRuntime(appId, refresh) {
  const name = appId || 'docker-runtime';
  const t = toast('正在启动 Docker 运行时（首次启动约需 40 秒）…', 'info', 0);
  try {
    const resp = await api.serviceAction(name, 'start');
    t.remove();
    const warning = (resp && resp.warning) || '';
    if (warning) {
      // 后端说"操作完成但结果没被确认"时必须原样告诉用户 —— 那正是谎报成功的反面。
      toast('已请求启动 Docker 运行时：' + warning, 'warn', 14000);
    } else {
      toast('已请求启动 Docker 运行时，正在刷新本页…', 'ok', 6000);
    }
    setTimeout(() => { refresh(); }, 1200);
  } catch (e) {
    t.remove();
    toast('启动 Docker 运行时失败：' + e.message + '（可到「任务中心」重试一键安装）', 'err', 14000);
  }
}
