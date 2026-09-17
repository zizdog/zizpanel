// logshub.js —— 「日志」页：把原「日志中心」与「操作审计」合成一页两个 Tab。
//
// 为什么合并（2026-09 用户要求）：审计本质就是"谁在什么时候做了什么"的历史日志，
// 和运行日志分居两个侧栏项（还各自占一个分组）没有道理；合并后侧栏只有一个
// 「日志」入口、页面里只有一块「操作审计」内容。
//
// **旧 hash 一个都不能 404**（老书签/文档/docker.js 里的提示都引用过）：
//   #/logs   → 默认落在第一个 Tab（日志文件）
//   #/audit  → 直接切到「操作审计」Tab（app.js 的 ROUTE_TARGET 把 audit 映射到本页）
//
// 两个子视图是现成的整页组件（logs.js 的 LogsView / audit.js 的 AuditView）：
// 这里只做"外壳 + 切换"，绝不复制它们的渲染逻辑，audit.js 因此一字未改。
// 子视图仍可被单独 import 使用（App detail 测试等）。

import { h, clear } from './ui.js';
import { LogsView, stopLogsFollow } from './logs.js';
import { AuditView } from './audit.js';

// HUB_TABS 的 id 同时是 dataset.tab 的值（回归断言靠它定位），第一个是默认块。
const HUB_TABS = [
  { id: 'logs', title: '日志', render: LogsView },
  { id: 'audit', title: '操作审计', render: AuditView },
];
const HUB_TAB_IDS = new Set(HUB_TABS.map((t) => t.id));

export function LogsHubView(content, ctx = {}) {
  clear(content);

  // `#/audit`（ROUTE_TARGET 给 tab='audit'）直接切到审计块；
  // `#/logs` 或未知值一律落第一个 Tab（日志文件）。
  let active = (ctx && HUB_TAB_IDS.has(ctx.tab)) ? ctx.tab : 'logs';

  const tabBar = h('div', {
    style: { display: 'flex', gap: '6px', marginBottom: '14px', flexWrap: 'wrap' },
  });
  const body = h('div');
  content.append(tabBar, body);

  function renderTabBar() {
    clear(tabBar);
    for (const t of HUB_TABS) {
      tabBar.appendChild(h(`button.btn.btn-sm${active === t.id ? '.btn-primary' : ''}`, {
        // dataset.tab 是测试与"当前块"的稳定锚点（文案会变，锚点不该变）。
        dataset: { tab: t.id },
        text: t.title,
        onclick: () => {
          if (active === t.id) return;
          // 离开"日志"块时先停掉实时尾随 SSE：否则它会留在后台一直连着
          //（LogsView 只在重新渲染或被路由清理时才停）。
          if (active === 'logs') stopLogsFollow();
          active = t.id;
          renderTabBar();
          renderBody();
        },
      }));
    }
  }

  function renderBody() {
    clear(body);
    const t = HUB_TABS.find((x) => x.id === active) || HUB_TABS[0];
    // 子视图拿同一份 ctx（含 onLeave / item）：Tab 内部切换不会触发路由级 cleanup，
    // 所以上面显式停了日志的 SSE，其余按子视图自己的生命周期处理。
    t.render(body, ctx);
  }

  renderTabBar();
  renderBody();
}
