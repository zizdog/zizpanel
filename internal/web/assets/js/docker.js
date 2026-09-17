// docker.js —— Docker 管理页。
//
// 与「应用 → 我的应用」的分工（刻意如此，避免两个页面互相打架）：
//   · 我的应用 管**服务生命周期**：把某个容器/compose 项目当作一条服务来启停、看日志、健康检查。
//   · 本页 管**Docker 本身**：这台机器上有哪些容器/镜像/卷/网络、镜像拉取与清理、
//     compose 文件的编辑与部署、以及从零创建一个容器。
//   两个页面会在同一批对象上出现（已纳管的 docker/compose 服务），所以这里
//   专门有一个「已纳管服务」分区做跳转，而不是把服务列表再抄一遍。
//
// 环境不可用（macOS 上没装 Docker 是常态）时首屏显示引导卡片而不是报错：
// 后端为此专门返回 409 + 一句人能看懂的话（含"去哪里装"）。

import { api } from './api.js';
import { h, clear } from './ui.js';
import { registerCleanup } from './app.js';
import { renderContainers } from './docker-containers.js';
import { renderImages } from './docker-images.js';
import { renderVolumes } from './docker-volumes.js';
import { renderNetworks } from './docker-networks.js';
import { renderCompose } from './docker-compose.js';
import { renderDockerServices } from './docker-services.js';
import { renderMirrors } from './docker-mirrors.js';

const TABS = [
  { id: 'containers', title: '容器', icon: '📦', render: renderContainers, count: 'containers' },
  { id: 'images', title: '镜像', icon: '💿', render: renderImages, count: 'images' },
  { id: 'volumes', title: '数据卷', icon: '🗃️', render: renderVolumes, count: 'volumes' },
  { id: 'networks', title: '网络', icon: '🔌', render: renderNetworks, count: 'networks' },
  { id: 'compose', title: 'Compose', icon: '🧩', render: renderCompose, count: 'compose_projects' },
  // 加速源：用户抱怨「拉取太慢」的直接对策（给 docker 守护进程配 registry-mirrors）
  { id: 'mirrors', title: '加速源', icon: '🚀', render: renderMirrors, count: null },
  { id: 'services', title: '已纳管服务', icon: '⚙️', render: renderDockerServices, count: null },
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
      renderUnavailable(pane, info);
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

// renderUnavailable 显示"环境不可用"的引导卡片。
//
// 这是 macOS 上的**常态**（Docker 不是系统自带），所以文案必须给出下一步动作，
// 而不是只说"失败了" —— 后者会让用户以为面板坏了。
function renderUnavailable(container, info) {
  const msg = info.error || '未检测到可用的 Docker 环境';
  clear(container);

  container.append(
    h('div.empty', [
      h('div.big', { text: '🐳' }),
      h('h4', { text: 'Docker 环境不可用' }),
      h('p', { text: msg }),
    ]),
    h('div', { style: { maxWidth: '640px', margin: '0 auto 18px' } }, [
      h('div.hint', { text: 'macOS 上 Docker 引擎不是系统自带的，需要一个 Linux 虚拟机来承载。推荐按这个顺序处理：' }),
      h('ol', { style: { paddingLeft: '20px', lineHeight: '2', color: 'var(--text-dim)', fontSize: '13px' } }, [
        h('li', { text: '打开「应用市场」，安装「Docker 运行时（Colima）」，装完会自动开机自启。' }),
        h('li', { text: '回到「应用 → 我的应用」，确认 docker-runtime 处于运行中（首次启动约需 40 秒）。' }),
        h('li', { text: '回到本页点「刷新」。' }),
      ]),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '14px' } }, [
        h('button.btn.btn-primary', { text: '去应用市场', onclick: () => location.hash = '#/apps' }),
        h('button.btn.btn-ghost', { text: '去服务管理', onclick: () => location.hash = '#/services' }),
      ]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '当前探测结果' })]),
      h('div.card-body.tight', [
        h('table.table', [h('tbody', [
          h('tr', [h('td', { text: 'socket 路径' }), h('td', [h('code', { text: info.socket || '（未探测到）' })])]),
          h('tr', [h('td', { text: '引擎版本' }), h('td', { text: info.version || '—' })]),
          h('tr', [h('td', { text: '错误' }), h('td', { text: msg })]),
        ])]),
      ]),
    ]),
  );
}
