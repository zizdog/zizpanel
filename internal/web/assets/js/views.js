// views.js —— 各功能页面。
//
// 约定：每个 View(content, ctx) 负责把内容渲染进 content 元素。
// 需要长连接的页面（如仪表盘）在离开时通过 ctx.onLeave 注册清理函数。

import { api, sse, apiURL } from './api.js';
import {
  h, clear, toast, modal, confirmBox, bytes, rate, pct, duration,
  levelOf, Sparkline, $,
} from './ui.js';
import { state, NAV, panelPath } from './app.js';
import { taskCenter } from './tasks.js';
// 「检查更新」在 2026-09-20 被用户要求**收回**「面板设置」（原来是侧栏独立页），
// 作为本页第 4 个 Tab。它的实现仍在 update.js 里，这里只是把它挂进来 ——
// 不复制一份代码，避免"两处升级界面各改一半"（本项目反复踩过的坑）。
import { UpdateView } from './update.js';

// 用于在渲染器内部切换路由的小工具（app.js 的 render 无法被 import 循环引用）
function go(id) { location.hash = '#/' + id; }

// ============================================================================
//  仪表盘
// ============================================================================

export function DashboardView(content, ctx = {}) {
  clear(content);

  const metrics = {}; // 保存需要实时更新的 DOM 引用

  // ---------- 概览卡片 ----------
  function metricCard(key, label, icon, opts = {}) {
    const value = h('div.value', { text: '—' });
    const meta = h('div.meta', { text: '' });
    const bar = h('div.bar', [h('i', { style: { width: '0%' } })]);
    const card = h(`div.metric${opts.span ? '.span2' : ''}`, [
      h('div.label', [h('span', { text: icon }), h('span', { text: label })]),
      value, meta,
      opts.bar === false ? null : bar,
    ]);
    metrics[key] = { value, meta, bar: bar.querySelector('i'), card };
    return card;
  }

  const topGrid = h('div.grid.grid-4', [
    metricCard('cpu', 'CPU 使用率', '💻'),
    metricCard('mem', '内存使用', '💾'),
    metricCard('disk', '磁盘占用', '🗄️'),
    metricCard('net', '网络吞吐', '🌊', { bar: false }),
  ]);

  // ---------- 系统信息 ----------
  const sysList = h('dl.kv');
  const sysCard = h('div.card', [
    h('div.card-head', [h('h3', { text: '系统信息' }), h('div.spacer'), h('span.sub', { id: 'live-dot', text: '实时' })]),
    h('div.card-body', [sysList]),
  ]);

  // ---------- CPU 曲线 ----------
  const cpuSpark = new Sparkline({ color: 'var(--brand)' });
  const memSpark = new Sparkline({ color: 'var(--ok)' });
  const netSpark = new Sparkline({ color: 'var(--info)' });
  const charts = h('div.grid.grid-3', [
    h('div.card', [
      h('div.card-head', [h('h3', { text: 'CPU' }), h('div.spacer'), h('span.sub', { text: '近 2 分钟' })]),
      h('div.card-body', [cpuSpark.svg]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '内存' }), h('div.spacer'), h('span.sub', { text: '近 2 分钟' })]),
      h('div.card-body', [memSpark.svg]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '网络速率' }), h('div.spacer'), h('span.sub', { text: '近 2 分钟' })]),
      h('div.card-body', [netSpark.svg]),
    ]),
  ]);

  // ---------- 进程表 ----------
  const procBody = h('tbody');
  const procCard = h('div.card', [
    h('div.card-head', [
      h('h3', { text: '资源占用 Top 进程' }),
      h('div.spacer'),
      h('div', { style: { display: 'flex', gap: '6px' } }, [
        h('button.btn.btn-sm', { text: '按 CPU', onclick: () => loadProcs('cpu') }),
        h('button.btn.btn-sm', { text: '按内存', onclick: () => loadProcs('mem') }),
      ]),
    ]),
    h('div.card-body.tight', [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: 'PID' }), h('th', { text: '程序' }),
          h('th', { text: 'CPU' }), h('th', { text: '内存' }),
          h('th', { text: '常驻内存' }), h('th', { text: '运行时长' }),
        ])]),
        procBody,
      ]),
    ]),
  ]);

  // ---------- 服务状态 ----------
  //
  // 这里原先是一张"此模块正在开发中"的占位卡（P3 之前留下的）。
  // 服务管理早已交付，那张卡就变成了**误导**：仪表盘告诉用户功能没做，
  // 而侧边栏里它就在那儿可用。现在改成真实的服务概览。
  const svcBody = h('div.card-body.tight');
  const servicesCard = h('div.card', [
    h('div.card-head', [
      h('h3', { text: '服务状态' }),
      h('div.spacer'),
      h('button.btn.btn-ghost.btn-sm', {
        text: '我的应用', onclick: () => go('services'),
      }),
    ]),
    svcBody,
  ]);

  async function loadServices() {
    clear(svcBody);
    svcBody.append(h('div.empty', [h('p', { text: '加载中…' })]));
    let list = [];
    try {
      const res = await api.services(false);
      list = (res && res.list) || [];
    } catch (e) {
      clear(svcBody);
      svcBody.append(h('div.empty', [h('p', { text: '读取服务列表失败：' + e.message })]));
      return;
    }
    clear(svcBody);
    if (!list.length) {
      svcBody.append(h('div.empty', [
        h('div.big', { text: '⚙️' }),
        h('p', { text: '还没有服务。到「应用」的「应用市场」安装一个即可。' }),
      ]));
      return;
    }
    const running = list.filter((s) => (s.state || {}).running).length;
    const rows = list.slice(0, 6).map((s) => {
      const st = s.state || {};
      return h('tr', [
        h('td', [
          h('span', { text: (s.icon || '') + ' ' }),
          h('span', { text: s.display_name || s.name }),
        ]),
        h('td', [h(st.running ? 'span.pill.ok' : 'span.pill.warn', { text: st.status || 'unknown' })]),
        h('td.mono', { text: s.port ? String(s.port) : '—' }),
      ]);
    });
    if (list.length > rows.length) {
      rows.push(h('tr', [h('td', { colspan: 3 }, [
        h('span.hint', { text: `另有 ${list.length - rows.length} 个服务，见「应用 → 我的应用」` }),
      ])]));
    }
    svcBody.append(
      h('div.hint', { style: { marginBottom: '6px' }, text: `${running}/${list.length} 个服务运行中` }),
      h('table.table', [h('tbody', rows)]),
    );
  }

  // ---------- 运行依赖（基础环境）未就绪的引导 ----------
  //
  // 2026-09 产品拆分：这条横幅只管**跨应用运行依赖** ——
  // 命令行开发者工具(CLT) → Homebrew → ffmpeg。它跟"网站环境"
  // （nginx + PHP + MySQL + phpMyAdmin）是**两层**，老文案把这一步叫"一键 LNMP"
  // 是标签在骗人：点「安装基础环境」不会装 nginx/PHP/MySQL。
  //
  // 判据**只认后端** GET /api/v1/system/base-env（data 形如
  // {clt_ok, brew_ok, deps_ok, ready, missing[]}），不再用服务列表去猜。
  // 旧面板没有这个接口时 request() 抛 404 —— 必须**如实说"读不到"**，
  // 既不静默也不假装已就绪（铁律：能谎报成功比没做更糟）。
  //
  // 为什么还放在仪表盘最上面、且保留：安装器**什么都不问**，用户第一次进来
  // 如果缺依赖会看到"什么都装不了"的面板，所以必须主动说一次。
  // 刻意**不做硬门禁**：镜像不可用时不让人进门，比不提示更糟。所以是"提示 + 一键装 + 稍后"。
  const baseEnvNotice = h('div');

  // baseEnvDismissed 只认"用户点了稍后"这一个持久标记。
  // 刻意**不缓存"已就绪"**：缓存会把"这次读不到"当成"已就绪"永久跳过提示。
  function baseEnvDismissed() {
    return localStorage.getItem('zp-baseenv-dismissed') === '1';
  }

  // missingList 取出后端 missing 里缺的项（文案用）。
  // 后端漏给 missing 时按三个布尔量兜底 —— 仍然是"如实列出缺什么"，
  // 绝不因为字段缺失就写"已就绪"。
  function missingList(data) {
    const arr = Array.isArray(data && data.missing) ? data.missing.filter(Boolean) : [];
    if (arr.length) return arr.map((x) => String(x));
    const out = [];
    if (data && data.clt_ok === false) out.push('命令行开发者工具');
    if (data && data.brew_ok === false) out.push('Homebrew');
    if (data && data.deps_ok === false) out.push('ffmpeg');
    return out;
  }

  function startBaseEnv() {
    taskCenter.start({
      kind: 'install',
      // target 与后端任务保持一致（POST /api/v1/system/base-env/install → 202 + task_id），
      // 这样「任务中心」能按同一个值找到运行中的任务（见 sites.js 的同款用法）。
      target: 'base-env',
      title: '安装基础环境',
      start: () => api.installBaseEnv(),
    });
    baseEnvNotice.replaceChildren();
  }

  function dismissBaseEnv() {
    localStorage.setItem('zp-baseenv-dismissed', '1');
    baseEnvNotice.replaceChildren();
  }

  // 运行依赖有缺项：逐项列出缺什么。
  function renderBaseEnvMissing(data) {
    const missing = missingList(data);
    const known = missing.length > 0;
    // 只有后端明确说 CLT 已就绪时才加这句（否则就是在替后端编状态）。
    const cltNote = (data && data.clt_ok === true) ? '（命令行开发者工具已就绪）' : '';
    // 后端 ready=false 却没给 missing（契约异常）：如实说"未就绪但不知道缺哪项"，
    // **不要**替它列成"三个都缺"（那是在编状态）。
    const head = known
      ? '⚠ 缺少运行依赖：' + missing.join('、') + cltNote
      : '⚠ 运行依赖未就绪（后端未返回具体缺项）';
    baseEnvNotice.append(h('div.card', {
      style: { borderLeft: '4px solid #e6a23c', marginBottom: '14px' },
    }, [
      h('div.card-body', [
        h('div', { style: { fontWeight: '600', marginBottom: '6px' }, text: head }),
        h('div.hint', {
          text: '这里说的是**跨应用的运行依赖**：命令行开发者工具（CLT）→ Homebrew → ffmpeg。'
            + '凡是走 Homebrew 安装的应用都依赖它们（python3 随命令行开发者工具一起装好，不需要单独安装）。'
            + '**这一步不含网站环境**（nginx / PHP / MySQL / phpMyAdmin）——'
            + '需要网站环境请到「网站管理」点「⚡ 一键 LNMP」，它会先确保这里的运行依赖再装那套。'
            + '这一步不装 MySQL，所以**不会询问 root 口令**。'
            + '全程约几分钟，进度在「任务中心」实时可见、关掉页面也不中断。',
        }),
        h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('button.btn.btn-primary', { text: '⚡ 安装基础环境', onclick: startBaseEnv }),
          h('button.btn.btn-sm', {
            text: '稍后',
            title: '暂时不装：刷新页面后若依赖仍缺失会再次提示',
            onclick: dismissBaseEnv,
          }),
        ]),
      ]),
    ]));
  }

  // 读不到状态（接口 404 / 网络错误）：如实说读不到，并给「重试」。
  // 此时**不显示安装按钮** —— 连缺什么都不知道，给一个"安装"入口就是在暗示结论。
  function renderBaseEnvUnknown(message) {
    baseEnvNotice.append(h('div.card', {
      style: { borderLeft: '4px solid #d9534f', marginBottom: '14px' },
    }, [
      h('div.card-body', [
        h('div', { style: { fontWeight: '600', marginBottom: '6px' }, text: '⚠ 无法读取运行依赖状态（接口不可用）' }),
        h('div.hint', {
          text: '面板没能从后端拿到「命令行开发者工具 / Homebrew / ffmpeg」的就绪状态：'
            + String(message || '未知错误')
            + '。在读到真实状态之前，这里不会显示"已就绪"，也不会替你决定要不要安装。',
        }),
        h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('button.btn.btn-sm', { text: '⟳ 重试', onclick: () => { void checkBaseEnv(); } }),
          h('button.btn.btn-sm', {
            text: '稍后',
            title: '暂时忽略：刷新页面后会重试读取',
            onclick: dismissBaseEnv,
          }),
        ]),
      ]),
    ]));
  }

  // checkBaseEnv 拉一次运行依赖状态并据此渲染（或按 ready===true 保持不显示）。
  // 与 loadServices 解耦：服务列表接口挂了也不该让这条提示变成"沉默的空白"。
  async function checkBaseEnv() {
    baseEnvNotice.replaceChildren();
    if (baseEnvDismissed()) return;
    let data = null;
    try {
      data = await api.baseEnv();
    } catch (e) {
      renderBaseEnvUnknown((e && e.message) || String(e));
      return;
    }
    // 只认 ready === true 才是"不显示"。undefined / 字段缺失一律当"没就绪"处理，
    // 免得后端换契约时界面默默变回"假装一切正常"。
    if (data && data.ready === true) return;
    renderBaseEnvMissing(data || {});
  }

  content.append(
    baseEnvNotice,
    topGrid,
    h('div.grid.grid-2', [sysCard, charts]),
    procCard,
    servicesCard,
  );



  // ---------- 服务状态与数据更新 ----------
  loadServices();
  // 运行依赖横幅独立于服务列表拉取（见 checkBaseEnv 的注释）。
  void checkBaseEnv();

  function update(s) {
    state.metrics = s;

    // CPU
    const cpuLevel = levelOf(s.cpu_used, 80, 92);
    metrics.cpu.card.className = 'metric' + (cpuLevel ? ' ' + cpuLevel : '');
    metrics.cpu.value.textContent = pct(s.cpu_used);
    metrics.cpu.meta.textContent = `${s.cpu_cores} 核 · 负载 ${s.load_1.toFixed(2)} / ${s.load_5.toFixed(2)} / ${s.load_15.toFixed(2)}`;
    metrics.cpu.bar.style.width = Math.min(100, s.cpu_used) + '%';
    metrics.cpu.bar.parentElement.className = 'bar' + (cpuLevel ? ' ' + cpuLevel : '');
    cpuSpark.push(s.cpu_used);

    // 内存
    const memPct = s.mem_total ? (s.mem_used / s.mem_total) * 100 : 0;
    const memLevel = levelOf(memPct, 80, 92);
    metrics.mem.card.className = 'metric' + (memLevel ? ' ' + memLevel : '');
    metrics.mem.value.innerHTML = `${pct(memPct)}<small>${bytes(s.mem_used)} / ${bytes(s.mem_total)}</small>`;
    metrics.mem.meta.textContent = `可用 ${bytes(s.mem_free + s.mem_cached)}（含可回收 ${bytes(s.mem_cached)}）` +
      (s.swap_total ? ` · Swap ${bytes(s.swap_used)}/${bytes(s.swap_total)}` : '');
    metrics.mem.bar.style.width = Math.min(100, memPct) + '%';
    metrics.mem.bar.parentElement.className = 'bar' + (memLevel ? ' ' + memLevel : '');
    memSpark.push(memPct);

    // 磁盘
    const diskPct = s.disk_total ? (s.disk_used / s.disk_total) * 100 : 0;
    const diskLevel = levelOf(diskPct, 80, 90);
    metrics.disk.card.className = 'metric' + (diskLevel ? ' ' + diskLevel : '');
    metrics.disk.value.innerHTML = `${pct(diskPct)}<small>${bytes(s.disk_used)} / ${bytes(s.disk_total)}</small>`;
    metrics.disk.meta.textContent = `剩余 ${bytes(s.disk_free)}`;
    metrics.disk.bar.style.width = Math.min(100, diskPct) + '%';
    metrics.disk.bar.parentElement.className = 'bar' + (diskLevel ? ' ' + diskLevel : '');

    // 网络
    const netTop = Math.max(s.net_rx_rate, s.net_tx_rate);
    metrics.net.value.innerHTML =
      `<span style="color:var(--ok)">↓</span> ${rate(s.net_rx_rate)}<small></small>`;
    metrics.net.meta.innerHTML =
      `<span style="color:var(--brand)">↑</span> ${rate(s.net_tx_rate)} · 累计 ↓${bytes(s.net_rx)} ↑${bytes(s.net_tx)}`;
    netSpark.push(netTop > 0 ? Math.min(100, (netTop / (10 * 1024 * 1024)) * 100) : 0);

    // 系统信息
    clear(sysList);
    const rows = [
      ['主机名', s.hostname || '-'],
      ['操作系统', `macOS ${s.os || '?'} (${s.arch})`],
      ['处理器', s.cpu_model || '-'],
      ['运行时长', duration(s.uptime)],
      // 系统负载与 Swap 原先只出现在「系统监控」页；那一页已改造成「mac设置」，
      // 于是把这两项搬进仪表盘，避免页面改造反而丢掉信息。
      ['系统负载 (1/5/15)', `${s.load_1.toFixed(2)} / ${s.load_5.toFixed(2)} / ${s.load_15.toFixed(2)}`],
      ['Swap 使用', s.swap_total ? `${bytes(s.swap_used)} / ${bytes(s.swap_total)}` : '未启用'],
      ['进程数', String(s.procs)],
      // 取不到时**如实说原因**：以前这里写死"不可读取（需 root）"，
      // 而 Apple Silicon 上的真实原因是 powermetrics 没有 smc 采样器（不是权限）。
      // 现在后端会把来源/原因放在 cpu_temp_note 里（例如 "IOHID tdie×24"）。
      ['CPU 温度', s.cpu_temp > 0
        ? s.cpu_temp + ' °C' + (s.cpu_temp_note ? '（' + s.cpu_temp_note + '）' : '')
        : '取不到' + (s.cpu_temp_note ? '：' + s.cpu_temp_note : '')],
      ['面板地址', location.origin],
      ['服务端时间', new Date().toLocaleString('zh-CN')],
    ];
    rows.forEach(([k, v]) => {
      sysList.append(h('dt', { text: k }), h('dd', { text: v }));
    });
  }

  async function loadProcs(sort) {
    try {
      const res = await api.processes(sort, 12);
      clear(procBody);
      if (!res.list || !res.list.length) {
        procBody.append(h('tr', [h('td', { colspan: 6 }, [h('div.empty', { text: '没有读取到进程' })])]));
        return;
      }
      res.list.forEach((p) => {
        procBody.append(h('tr', [
          h('td.num.mono', { text: p.pid }),
          h('td.mono', { text: p.command, title: p.command }),
          h('td.num', { text: p.cpu != null ? p.cpu.toFixed(1) + '%' : '—' }),
          h('td.num', { text: (p.mem || 0).toFixed(1) + '%' }),
          h('td.num', { text: bytes((p.rss || 0) * 1024) }),
          h('td.num', { text: p.etime || '-' }),
        ]));
      });
    } catch (e) {
      toast('读取进程失败: ' + e.message, 'err');
    }
  }

  loadProcs('cpu');

  // ---------- SSE 实时推送 ----------
  // 路由切换时由 app.js 统一执行 onLeave 清理，避免留下悬空的 EventSource
  const es = sse(apiURL('system/stream?interval=2'), {
    onSample: update,
    onError: () => {
      const dot = document.getElementById('live-dot');
      if (dot) { dot.textContent = '连接中断，正在重连…'; dot.className = 'sub'; }
    },
  });
  if (ctx.onLeave) ctx.onLeave(() => es.close());
}

// ============================================================================
//  面板设置
// ============================================================================

// uploadLimitsModal 是"上传大小 / 执行时间"的**独立弹窗**（nginx + PHP 一起改）。
//
// 用户 2026-09-18 明确要求：这类常用更改必须是**功能**，不能只让用户去编辑配置原文件。
// 所以 nginx / PHP 的管理面板与网站管理工具条都直接开这个弹窗，改完自动
// 重载 nginx + 重启 php-fpm，并**回读生效值**（不是只报"已保存"）。
export function uploadLimitsModal() {
  const box = h('div');
  const m = modal({ title: '⚡ 上传大小 / 执行时间（nginx + PHP）', wide: true, body: box });
  const refresh = async () => { clear(box); await renderLimitsInto(box, refresh); };
  void refresh();
  return m;
}

export function SettingsView(content, ctx = {}) {
  clear(content);
  const user = state.session?.user || {};
  const cfg = state.session?.config || {};

  // 「检查更新」排在三项之后（用户 2026-09-20 明确要求的顺序）。
  // 它是**低频但重要**的动作：放在设置里，但保留直达 hash（#/settings/update），
  // 以及旧 hash（#/update、#/about、#/settings/about）的别名，见 app.js 的别名表。
  const tabs = [
    { id: 'access', title: '访问与安全' },
    // 上传与执行限制紧跟访问设置：用户报障的 413（phpMyAdmin 导入失败）就是
    // 在这里改的 —— 放太深等于"又找不到入口"。
    { id: 'limits', title: '上传与执行限制' },
    { id: 'terminal', title: '文件与终端' },
    { id: 'account', title: '账号与两步验证' },
    { id: 'update', title: '检查更新' },
  ];
  let active = tabs.some((t) => t.id === ctx.tab) ? ctx.tab : 'access';

  const body = h('div');
  const tabBar = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '16px', flexWrap: 'wrap' } });

  function renderTabs() {
    clear(tabBar);
    tabs.forEach((t) => tabBar.append(h(`button.btn.btn-sm${active === t.id ? '.btn-primary' : ''}`, {
      text: t.title,
      onclick: () => {
        active = t.id;
        // 把当前 Tab 写进 URL（用 replaceState：不触发 hashchange，因此不会整页重挂载）。
        // 为什么值得做：刷新后仍停在同一页（否则"检查更新"这种要看结果的页刷新就跳回
        // 第一个 Tab），而且可以把 #/settings/update 直接收藏/发给别人。
        try { history.replaceState(null, '', '#/settings/' + t.id); } catch { /* 隐私模式等 */ }
        renderTabs();
        renderBody();
      },
    })));
  }

  async function renderBody() {
    clear(body);
    if (active === 'access') await renderAccess();
    else if (active === 'limits') await renderLimits();
    else if (active === 'terminal') await renderTerminal();
    else if (active === 'update') await renderUpdate();
    else await renderAccount();
  }

  // ---------- 检查更新 ----------
  // 直接复用「检查更新」页的实现（update.js）。它渲染自己的卡片（在线升级 /
  // 面板信息 / 常用运维命令），所以这里只给一个容器。
  async function renderUpdate() {
    const box = h('div', { dataset: { testid: 'zp-settings-update' } });
    body.append(box);
    UpdateView(box, ctx);
  }

  // ---------- 上传与执行限制（用户报障 413 的入口）----------
  async function renderLimits() { clear(body); await renderLimitsInto(body, renderLimits); }

  //
  // 背景：phpMyAdmin 导入几十 MB 的 SQL 报 **413 Request Entity Too Large**。
  // nginx 出厂 client_max_body_size 只有 1m、PHP 出厂 upload 2M/post 8M，
  // 而面板此前**没有任何界面**能改这两组上限（用户原话："面板找不到更改的入口"）。
  //
  // 交互约定（与任务中心的规矩一致）：
  //   保存 → POST 校验（非法输入当场 400 + 人话）→ 202 + task_id →
  //   任务里写 vhost / conf.d → reload nginx → 重启 php-fpm → **回读生效值**。
  //   这里把回读结果渲染成"生效值"卡片，绝不只显示"已保存"。
  // renderLimitsInto 把"上传与执行限制"面板画进**任意容器**。
  //
  // 为什么参数化容器：这两组上限（nginx client_max_body_size / PHP upload+post+memory+
  // 执行时间）不只是设置页里的一个版块，它是用户**最常改**的东西 ——
  // 2026-09-18 用户报障："php 和 nginx 的编辑配置文件都是灰色的，用户没法更改文件大小限制"，
  // 并明确要求"这些常用更改应该同时做成功能，而不是让用户只能编辑配置原文件"。
  // 所以同一份实现要能同时出现在：面板设置版块、nginx/PHP 的管理面板、网站管理工具条
  //（三处共用，绝不复制第二份 —— 复制出来的那份迟早会与后端校验漂移）。
  async function renderLimitsInto(container, refresh) {
    let v;
    try { v = await api.getUploadLimits(); }
    catch (e) {
      container.append(h('div.card', [h('div.card-body', { text: '读取上传/执行限制失败: ' + e.message })]));
      return;
    }
    const lim = v.limits || {};
    const def = v.defaults || {};

    const sizeField = (key, label, hint) => {
      const input = h('input.input', {
        value: lim[key] ?? '', placeholder: def[key] || '',
        style: { maxWidth: '220px' },
      });
      return {
        input,
        field: h('div.field', [h('label', { text: label }), input, h('div.hint', { text: hint })]),
      };
    };
    const nginxField = sizeField('client_max_body_size', 'nginx 请求体上限（client_max_body_size）',
      '例：512m / 1g。这是 nginx 的硬上限：请求超过它会被直接返回 413，PHP 根本收不到数据 —— '
      + 'phpMyAdmin 导入大 SQL 报 413 就是这里太小。');
    const uploadField = sizeField('upload_max_filesize', 'PHP upload_max_filesize',
      '单个上传文件的上限（例：512M）。');
    const postField = sizeField('post_max_size', 'PHP post_max_size',
      '整个请求体的上限（例：512M）。不能小于 upload_max_filesize，否则上传一定失败。');
    const memField = sizeField('memory_limit', 'PHP memory_limit',
      'PHP 进程内存上限（例：512M）。导入大 SQL 时解析要占用内存。');
    const execInput = h('input.input', {
      type: 'number', value: lim.max_execution_time ?? 300, min: 1, max: 86400,
      style: { maxWidth: '220px' },
    });
    const execField = h('div.field', [
      h('label', { text: 'PHP max_execution_time（秒）' }),
      execInput,
      h('div.hint', { text: '导入大 SQL 会跑很久；出厂的 30 秒会让大文件导入中途失败。改这个值会同时对齐 phpMyAdmin 的 $cfg[\'ExecTimeLimit\']。' }),
    ]);

    // mismatch 横幅：磁盘上的生效值还不是配置值时，明确告诉用户"还没生效"。
    const banner = h('div');
    function renderBanner() {
      clear(banner);
      if (!v.mismatch) return;
      banner.append(h('div', {
        style: {
          margin: '0 0 14px', padding: '10px 12px', background: 'var(--warn-soft)',
          borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.8',
        },
      }, [
        h('div', { text: '⚠️ 有配置项还没有生效（见下面的「当前生效值」）。' }),
        h('div', { text: '点「保存并应用」会把目标值写进面板生成的 vhost 与 PHP conf.d，重载 nginx 并重启对应的 php-fpm。' }),
      ]));
    }
    renderBanner();

    const save = h('button.btn.btn-primary', {
      text: '保存并应用',
      onclick: async () => {
        save.disabled = true;
        try {
          const patch = {
            client_max_body_size: nginxField.input.value.trim(),
            upload_max_filesize: uploadField.input.value.trim(),
            post_max_size: postField.input.value.trim(),
            memory_limit: memField.input.value.trim(),
            max_execution_time: Number(execInput.value),
          };
          // 后端会先校验（非法 400 + 人话）；通过则 202 + task_id，进度在任务中心。
          const id = await taskCenter.start({
            kind: 'settings',
            target: 'upload-limits',
            title: '应用上传与执行限制',
            start: () => api.saveUploadLimits(patch),
            // 任务结束后重新回读：生效值卡片必须跟着变（不能停在旧值）。
            onDone: () => { toast('上传与执行限制已应用，正在回读生效值…', 'ok', 6000); refresh(); },
          });
          if (id) toast('已提交，进度与生效值回读在任务中心里', 'ok', 8000);
        } catch (e) {
          toast(e.message, 'err', 12000);
        } finally {
          save.disabled = false;
        }
      },
    });
    const reread = h('button.btn.btn-sm', { text: '↻ 重新回读', onclick: refresh });

    // ---- 生效值：nginx 侧 ----
    const nginxRows = (v.nginx || []).map((f) => h('tr', [
      h('td.mono', { style: { fontSize: '12px', wordBreak: 'break-all' }, text: f.file }),
      h('td.mono', { text: f.value || '（未设置 → nginx 默认 1m）' }),
      h('td', [f.ok
        ? h('span.pill.ok', { text: '已生效' })
        : h('span.pill.warn', { text: '未生效' })]),
    ]));

    // ---- 生效值：PHP 侧（每个版本真的跑一次 php-cgi 回读 ini）----
    const phpRows = (v.php || []).map((p) => {
      const vals = p.values || {};
      const valueText = p.error
        ? '未复核：' + p.error
        : `upload ${vals.upload_max_filesize} · post ${vals.post_max_size} · memory ${vals.memory_limit} · max_execution_time ${vals.max_execution_time}s`;
      return h('tr', [
        h('td', { text: 'PHP ' + p.version }),
        h('td.mono', { style: { fontSize: '12px', wordBreak: 'break-all' }, text: p.fragment || '-' }),
        h('td.mono', { style: { fontSize: '12px', wordBreak: 'break-all' } }, [
          h('div', { text: valueText }),
          // 回读用的 SAPI 与"查不到"的说明都如实显示（CLI 会把
          // max_execution_time 强制成 0，那时标"未复核"而不是报失败）。
          p.sapi ? h('div.hint', { text: '（' + p.sapi + ' 回读）' }) : null,
          p.note ? h('div.hint', { style: { color: '#d97706' }, text: p.note }) : null,
        ]),
        h('td', [p.error
          ? h('span.pill.warn', { text: '未复核' })
          : (p.ok ? h('span.pill.ok', { text: '已生效' }) : h('span.pill.warn', { text: '未生效' }))]),
      ]);
    });

    container.append(
      h('div.card', [
        h('div.card-head', [
          h('h3', { text: '上传与执行限制' }),
          h('div.spacer'),
          h('span.sub', { text: '改完会自动重载 nginx 并重启 php-fpm（走任务中心）' }),
        ]),
        h('div.card-body', [
          banner,
          h('div.hint', {
            style: { marginBottom: '12px' },
            html: '这里控制"一次能传多大"：nginx 的请求体上限与 PHP 的上传/执行上限必须<b>同时</b>放大，'
              + '否则请求要么被 nginx 用 413 挡在门外，要么进来后被 PHP 拒绝。'
              + '默认值 <code class="code">' + (def.client_max_body_size || '512m')
              + '</code> / <code class="code">' + (def.upload_max_filesize || '512M')
              + '</code> 就是按"能导入大 SQL"选的。',
          }),
          h('div.row', [nginxField.field, uploadField.field]),
          h('div.row', [postField.field, memField.field, execField]),
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginTop: '4px' } }, [
            save, reread,
            h('span.hint', { text: '保存会写：面板生成的各站点 vhost、默认站点、phpMyAdmin 入口，以及 PHP conf.d 的 99-zizpanel-limits.ini' }),
          ]),
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [
          h('h3', { text: '当前生效值（回读）' }),
          h('div.spacer'),
          h('span.sub', { text: v.mismatch ? '有未生效项' : '全部已生效' }),
        ]),
        h('div.card-body.tight', [
          h('div', { style: { padding: '0 0 8px', fontSize: '12.5px' } }, [
            h('div', { text: 'nginx（' + (v.nginx_dir || '') + '）：' }),
          ]),
          nginxRows.length
            ? h('table.table', [
              h('thead', [h('tr', [h('th', { text: '配置文件' }), h('th', { text: 'client_max_body_size' }), h('th', { text: '状态' })])]),
              h('tbody', nginxRows),
            ])
            : h('div.empty', [h('p', { text: 'vhost 目录里还没有 .conf（先建站点或点「整理默认站点」）' })]),
          h('div', { style: { padding: '12px 0 8px', fontSize: '12.5px' }, text: 'PHP（每个已安装版本真的跑一次 php 回读 ini_get；CLI 与 php-fpm 读同一份 conf.d）：' }),
          phpRows.length
            ? h('table.table', [
              h('thead', [h('tr', [h('th', { text: '版本' }), h('th', { text: '面板片段' }), h('th', { text: '回读值' }), h('th', { text: '状态' })])]),
              h('tbody', phpRows),
            ])
            : h('div.empty', [h('p', { text: '本机没有发现已安装的 PHP 版本（先装 PHP 或用一键 LNMP）' })]),
        ]),
      ]),
    );
  }

  // ---------- 访问与安全 ----------
  async function renderAccess() {
    let s;
    try { s = await api.getSettings(); }
    catch (e) { body.append(h('div.card', [h('div.card-body', { text: '读取设置失败: ' + e.message })])); return; }

    const mode = h('select.select', {}, [
      h('option', { value: 'any', text: '任意来源（配合 HTTPS 与强密码，适合 Tailscale/内网）', selected: s.access_mode === 'any' }),
      h('option', { value: 'local', text: '仅本机（最安全，无法远程访问）', selected: s.access_mode === 'local' }),
      h('option', { value: 'whitelist', text: '仅白名单网段（推荐配合 Tailscale）', selected: s.access_mode === 'whitelist' }),
    ]);
    const whitelist = h('textarea.textarea', {
      value: (s.ip_whitelist || []).join('\n'),
      placeholder: '每行一个网段或 IP，例如：\n100.64.0.0/10\n192.168.1.0/24\n10.0.0.5',
      style: { minHeight: '110px' },
    });
    const trustProxy = h('input', { type: 'checkbox', checked: !!s.trust_proxy });
    // 安全后缀（安全入口）：可以随时改。改完**当前页面立刻失效**（新地址才有效），
    // 所以界面上要说清楚并把新地址显示出来，别让用户改完自己找不到面板。
    const suffixInput = h('input.input', { value: s.panel_suffix || '', placeholder: '留空 = 不启用安全入口', style: { flex: '1 1 200px' } });
    const suffixNow = h('span.hint', { text: '当前入口：' + (s.panel_entry || '/') });
    const randSuffix = h('button.btn.btn-sm', {
      text: '🎲 随机生成',
      onclick: () => {
        const abc = 'abcdefghjkmnpqrstuvwxyz23456789';
        let out = '';
        const b = new Uint8Array(8);
        crypto.getRandomValues(b);
        for (const x of b) out += abc[x % abc.length];
        suffixInput.value = out;
      },
    });
    const proxyAuth = h('input', { type: 'checkbox', checked: s.app_proxy_auth !== false });
    const sessionHours = h('input.input', { type: 'number', value: s.session_hours, min: 1, max: 720 });
    const maxFail = h('input.input', { type: 'number', value: s.login_max_fail, min: 1, max: 50 });
    const lockMins = h('input.input', { type: 'number', value: s.login_lock_mins, min: 1, max: 1440 });
    // 应用包镜像（自建 NAS）：面板里所有安装过程都先检查它。
    // 留空 = 关闭镜像、各来源回到内置的公网/国内镜像（仅用于镜像站故障时应急）。
    const mirrorInput = h('input.input', {
      value: s.mirror_base || '',
      placeholder: '例如 https://mirror.zizdog.com:8888（留空 = 关闭镜像）',
      style: { flex: '1 1 320px' },
    });
    const mirrorProbe = h('input.input', { type: 'number', value: s.mirror_probe_seconds || 4, min: 1, max: 60 });
    // 仅走 NAS（离线）模式：整机断外网 / 隔离网络 / 迁移到新 Mac 时打开。
    // 打开后各安装器**禁止回落外网**，缺资源就明确失败并列出缺哪个文件。
    // 刻意与 mirror_base 放在同一张卡片里：两者一起看才不会被误解成
    // "配了镜像就离线了" —— 默认是"镜像优先 + 缺件回落公网"，只有这个勾才是硬离线。
    const offlineOnly = h('input', { type: 'checkbox', checked: !!s.offline_only });
    const offlineEmptyMirrorHint =
      '⚠️ 现在开着离线模式，但镜像基址是空的 —— 这个组合下任何安装都会明确失败，请先填上面的镜像基址。';
    const offlineWarn = h('div.hint', { style: { color: '#d97706' }, text: '' });
    const syncOfflineWarn = () => {
      offlineWarn.textContent = (offlineOnly.checked && !mirrorInput.value.trim()) ? offlineEmptyMirrorHint : '';
    };
    offlineOnly.addEventListener('change', syncOfflineWarn);
    mirrorInput.addEventListener('input', syncOfflineWarn);
    syncOfflineWarn();

    const save = h('button.btn.btn-primary', {
      text: '保存设置',
      onclick: async () => {
        save.disabled = true;
        try {
          const patch = {
            access_mode: mode.value,
            ip_whitelist: whitelist.value.split('\n').map((x) => x.trim()).filter(Boolean),
            trust_proxy: trustProxy.checked,
            panel_suffix: suffixInput.value.trim(),
            app_proxy_auth: proxyAuth.checked,
            session_hours: Number(sessionHours.value),
            login_max_fail: Number(maxFail.value),
            login_lock_mins: Number(lockMins.value),
            mirror_base: mirrorInput.value.trim(),
            mirror_probe_seconds: Number(mirrorProbe.value) || 4,
            offline_only: offlineOnly.checked,
          };
          const saved = await api.saveSettings(patch);
          const entry = (saved && saved.panel_entry) || patch.panel_suffix;
          state.session.config = Object.assign({}, state.session.config, {
            access_mode: patch.access_mode,
            panel_entry: entry,
            panel_suffix: patch.panel_suffix,
          });
          if (patch.panel_suffix !== (s.panel_suffix || '')) {
            // 后缀变了：当前地址已经失效，必须把新地址摆到用户眼前
            modal({
              title: '安全入口已修改',
              body: h('div', [
                h('p', { text: '新的面板入口是：' }),
                h('div.mono', { style: { padding: '8px', background: 'var(--panel-2)', borderRadius: '6px' }, text: location.origin + entry }),
                h('div.hint', { style: { marginTop: '8px' }, text: '当前页面马上就会失效（刷新会 404），请改用上面的地址。' }),
              ]),
              footer: (close) => [h('button.btn.btn-primary', { text: '知道了', onclick: () => { close(); location.href = entry; } })],
            });
          } else {
            toast('设置已保存并立即生效', 'ok');
          }
        } catch (e) { toast(e.message, 'err', 7000); }
        finally { save.disabled = false; }
      },
    });

    const localIPs = [...new Set([location.hostname])];

    body.append(
      h('div.card', [
        h('div.card-head', [h('h3', { text: '远程访问策略' }), h('div.spacer'), h('span.sub', { text: '决定哪些来源能打开面板' })]),
        h('div.card-body', [
          h('div.field', [h('label', { text: '访问模式' }), mode]),
          h('div.field', [
            h('label', { text: 'IP 白名单' }),
            whitelist,
            h('div.hint', { html: '仅「白名单」模式生效。本机回环地址始终允许，不会被自己锁在门外。<br>Tailscale 网段：<code class="code">100.64.0.0/10</code>' }),
          ]),
          h('div.field', [
            h('label', [h('span', { text: '安全入口后缀（仿宝塔：面板只在 /<后缀>/ 下提供服务）' })]),
            h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [suffixInput, randSuffix]),
            suffixNow,
            h('div.hint', { text: '留空 = 关闭安全入口（面板回到根路径，安全性下降，不建议）。改完当前页面会失效，新地址会弹窗告诉你。' }),
          ]),
          h('div.row', [
            h('label', [h('span', { text: '应用界面（/iopaint/、/squoosh/ 等）要求先登录面板' })]),
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
              proxyAuth, h('span', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: '默认要求登录：这些界面挂在面板端口上，而 Squoosh 这类应用本身没有鉴权。' }),
            ]),
          ]),
          h('div.row', [
            h('label', [h('span', { text: '信任反向代理头（X-Forwarded-For）' })]),
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
              trustProxy, h('span', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: '仅当面板挂在 nginx 之后时开启；直连时开启会导致 IP 白名单可被伪造绕过。' }),
            ]),
          ]),
          h('div.row', [
            h('div.field', [h('label', { text: '登录会话有效期（小时）' }), sessionHours]),
            h('div.field', [h('label', { text: '连续失败几次锁定账号' }), maxFail]),
            h('div.field', [h('label', { text: '锁定时长（分钟）' }), lockMins]),
          ]),
          // 应用包镜像：面板里所有安装过程都先检查它（见 services/mirror.go）。
          // 语义是"镜像**优先**、缺件回落公网"；要"禁止回落"必须再勾下面的
          // 「仅走 NAS（离线）」—— 两件事分开，文案必须写清，否则用户会以为
          // 配了镜像就等于离线（那是这个页面最容易被误读的地方）。
          h('div.field', [
            h('label', { text: '应用包镜像基址' }),
            mirrorInput,
            h('div.hint', {
              html: '安装 frpc / DDNS-Go 这类应用时，面板先检查 ' +
                '<code class="code">&lt;基址&gt;/apps/&lt;应用&gt;/&lt;版本&gt;/&lt;文件名&gt;</code> 在不在；' +
                '在就从镜像下（并在任务日志里写明来源）。' +
                '镜像上缺这个包、或镜像站暂时不可达时，<b>默认</b>会回落到内置的公网/国内源 —— ' +
                '要禁止回落请勾下面的「仅走 NAS（离线）」。<br>' +
                '留空 = 关闭镜像（各来源回到内置的公网/国内镜像，仅用于镜像站故障时应急）。',
            }),
          ]),
          h('div.row', [
            h('label', [h('span', { text: '仅走 NAS（离线）：禁止任何外网回落，缺资源即明确失败' })]),
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
              offlineOnly,
              h('span', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: '用于整机断外网 / 隔离网络 / 迁移到新 Mac。打开后装不了的应用会明确报出缺哪个文件，而不是悄悄去连外网（那样只会"装到一半卡死"）。' }),
            ]),
            offlineWarn,
          ]),
          h('div.row', [
            h('div.field', [h('label', { text: '镜像资源探测超时（秒）' }), mirrorProbe]),
            h('div.hint', { style: { alignSelf: 'center' }, text: '镜像不可达时不让安装白等；局域网/同城镜像正常在 100ms 内应答。' }),
          ]),
          save,
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '访问入口' })]),
        h('div.card-body', [
          h('dl.kv', [
            h('dt', { text: '当前访问地址' }), h('dd', { text: location.origin }),
            h('dt', { text: '监听地址' }), h('dd', { text: cfg.listen || '-' }),
            h('dt', { text: 'HTTPS' }), h('dd', { text: cfg.tls ? '已启用（自签证书）' : '未启用' }),
            h('dt', { text: '网站根目录' }), h('dd', { text: cfg.www_root || '-' }),
            h('dt', { text: '数据目录' }), h('dd', { text: cfg.data_dir || '-' }),
          ]),
          h('div.hint', { style: { marginTop: '12px' }, html: '修改监听端口或 HTTPS 需要改配置文件后重启面板：<br><code class="code">sudo launchctl kickstart -k system/cn.zizpanel.panel</code>' }),
        ]),
      ]),
    );
  }

  // ---------- 文件与终端 ----------
  async function renderTerminal() {
    let st;
    try { st = await api.getSettings(); } catch (e) {
      body.append(h('div.card', [h('div.card-body', { text: '读取设置失败: ' + e.message })]));
      return;
    }
    let info = null;
    try { info = await api.terminalInfo(); } catch { /* 忽略 */ }

    const enabled = h('input', { type: 'checkbox', checked: !!st.terminal_enabled });
    const shell = h('input.input', { value: st.terminal_shell || '', placeholder: '留空自动选择 /bin/zsh 或 /bin/bash' });
    const idle = h('input.input', { type: 'number', value: st.terminal_idle_mins, min: 0, max: 1440 });
    const maxSess = h('input.input', { type: 'number', value: st.terminal_max_sessions, min: 1, max: 20 });

    const warnBox = h('div', { style: { marginTop: '12px' } });
    const updateWarn = () => {
      clear(warnBox);
      if (enabled.checked) {
        appendAll(warnBox, h('div', {
          style: { padding: '10px 12px', background: 'var(--warn-soft)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.7' },
        }, [
          h('div', { text: '⚠️ 开启后，任何能登录面板的人都能在这台 Mac 上执行任意命令。' }),
          h('div', { text: `当前将以「${st.terminal_user || 'root'}」身份运行 shell。` }),
          h('div', { text: '请确保：面板已开启两步验证、访问来源受限（如 Tailscale），并使用强密码。' }),
        ]));
      }
    };
    enabled.addEventListener('change', updateWarn);
    updateWarn();

    const save = h('button.btn.btn-primary', {
      text: '保存设置',
      onclick: async () => {
        save.disabled = true;
        try {
          await api.saveSettings({
            terminal_enabled: enabled.checked,
            terminal_shell: shell.value.trim(),
            terminal_idle_mins: Number(idle.value),
            terminal_max_sessions: Number(maxSess.value),
          });
          toast('已保存', 'ok');
          renderTerminal();
        } catch (e) { toast(e.message, 'err', 9000); }
        finally { save.disabled = false; }
      },
    });

    const sessions = (info?.list || []).map((s0) => h('tr', [
      h('td.mono', { style: { fontSize: '11.5px' }, text: s0.id.slice(0, 8) }),
      h('td', { text: s0.user || '-' }),
      h('td.mono', { text: s0.ip || '-' }),
      h('td', { text: s0.started_at }),
      h('td.num', { text: s0.idle_sec + ' 秒前活动' }),
      h('td', h('button.btn.btn-sm.btn-danger', {
        text: '强制关闭',
        onclick: async () => {
          try { await api.terminalKill(s0.id); toast('已关闭', 'ok'); renderTerminal(); }
          catch (e) { toast(e.message, 'err'); }
        },
      })),
    ]));

    body.append(
      h('div.card', [
        h('div.card-head', [h('h3', { text: 'Web 终端' }), h('div.spacer'),
          h('span.sub', { text: '高权限功能，默认关闭' })]),
        h('div.card-body', [
          h('div.field', [
            h('div', { style: { display: 'flex', alignItems: 'center', gap: '9px' } }, [
              enabled, h('span', { style: { fontSize: '13.5px', fontWeight: '550' }, text: '启用 Web 终端（浏览器里操作本机 shell）' }),
            ]),
          ]),
          warnBox,
          h('div.row', { style: { marginTop: '16px' } }, [
            h('div.field', [h('label', { text: 'Shell 路径' }), shell,
              h('div.hint', { text: '留空则自动选择 /bin/zsh（macOS 默认）或 /bin/bash' })]),
            h('div.field', [h('label', { text: '空闲超时（分钟）' }), idle,
              h('div.hint', { text: '0 = 不自动回收（会话一直留着，直到你点「关闭会话」或面板重启）' }),
              h('div.hint', { text: '填 0 表示不限制。超时后会话自动断开。' })]),
            h('div.field', [h('label', { text: '最大并发会话数' }), maxSess]),
          ]),
          save,
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '当前终端会话' }), h('div.spacer'),
          h('span.sub', { text: `${(info?.list || []).length} 个活动会话` })]),
        h('div.card-body.tight', [
          sessions.length
            ? h('table.table', [
              h('thead', [h('tr', [
                h('th', { text: '会话' }), h('th', { text: '运行用户' }), h('th', { text: '来源 IP' }),
                h('th', { text: '开始时间' }), h('th', { text: '活动' }), h('th', { text: '操作' }),
              ])]),
              h('tbody', sessions),
            ])
            : h('div.empty', [h('p', { text: '当前没有活动的终端会话' })]),
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '文件管理可访问范围' })]),
        h('div.card-body', [
          h('div.hint', { text: '文件管理器只能访问以下目录（由安装时的配置决定）。所有路径都会做软链接解析，无法越界。' }),
          h('div', { style: { marginTop: '10px', display: 'flex', flexDirection: 'column', gap: '5px' } },
            (st.www_root ? [
              h('code.code', { text: st.www_root + '　（网站目录）' }),
              h('code.code', { text: '/opt/zizpanel/data　（面板数据）' }),
              h('code.code', { text: '/opt/zizpanel/logs　（日志）' }),
              h('code.code', { text: '/opt/zizpanel/work　（工作目录）' }),
            ] : [h('span', { text: '—' })])),
        ]),
      ]),
    );
  }

  // ---------- 账号 ----------
  async function renderAccount() {
    const oldPwd = h('input.input', { type: 'password', placeholder: '当前密码' });
    const newPwd = h('input.input', { type: 'password', placeholder: '新密码（至少 8 位）' });
    const newPwd2 = h('input.input', { type: 'password', placeholder: '再次输入新密码' });
    const pwdBtn = h('button.btn.btn-primary', {
      text: '修改密码',
      onclick: async () => {
        if (newPwd.value.length < 8) { toast('新密码至少 8 位', 'warn'); return; }
        if (newPwd.value !== newPwd2.value) { toast('两次输入不一致', 'warn'); return; }
        pwdBtn.disabled = true;
        try {
          await api.changePassword(oldPwd.value, newPwd.value);
          toast('密码已修改，请重新登录', 'ok');
          setTimeout(() => { state.session = null; location.hash = ''; location.reload(); }, 1200);
        } catch (e) { toast(e.message, 'err'); pwdBtn.disabled = false; }
      },
    });

    // ---- 改用户名 ----
    //
    // 要当前密码：用户名是登录凭据的一半，审计里"谁做的"也记的是它。
    // 成功后**原地更新侧边栏那一行**，不重渲染外壳 —— 重渲染会把设置页打回
    // 第一个 Tab，用户会以为"改完把我踢走了"。
    const newName = h('input.input', { value: user.username || '', placeholder: '新用户名（3-32 位字母数字 . _ - @）' });
    const namePwd = h('input.input', { type: 'password', placeholder: '当前密码（确认身份）' });
    const nameBtn = h('button.btn.btn-primary', {
      text: '修改用户名',
      onclick: async () => {
        const v = newName.value.trim();
        if (v === (user.username || '')) { toast('新用户名与当前相同', 'warn'); return; }
        if (!namePwd.value) { toast('请输入当前密码以确认', 'warn'); namePwd.focus(); return; }
        nameBtn.disabled = true;
        try {
          const res = await api.renameUser(v, namePwd.value);
          const got = (res && res.username) || v;
          if (state.session && state.session.user) state.session.user.username = got;
          if (user) user.username = got;              // 同一对象，后续渲染也跟着变
          const line = document.getElementById('sidebar-account');
          if (line) line.textContent = '登录账号：' + got;
          namePwd.value = '';
          toast('用户名已改为 ' + got + '（登录时请用新名字）', 'ok', 8000);
        } catch (e) {
          toast(e.message, 'err', 9000);
        } finally {
          nameBtn.disabled = false;
        }
      },
    });

    const totpBox = h('div');
    function renderTOTP() {
      clear(totpBox);
      if (user.totp_enabled) {
        const pwd = h('input.input', { type: 'password', placeholder: '输入当前密码以关闭' });
        const btn = h('button.btn.btn-danger', {
          text: '关闭两步验证',
          onclick: async () => {
            try {
              await api.totpDisable(pwd.value);
              toast('两步验证已关闭', 'ok');
              state.session.user.totp_enabled = false;
              renderTOTP();
            } catch (e) { toast(e.message, 'err'); }
          },
        });
        totpBox.append(
          h('div.pill.ok', { text: '✅ 两步验证已开启' }),
          h('div.field', { style: { marginTop: '14px' } }, [h('label', { text: '关闭两步验证' }), pwd]),
          btn,
        );
        return;
      }
      const secretBox = h('code.code', { text: '点击下方按钮生成密钥', style: { display: 'block', padding: '10px', wordBreak: 'break-all' } });
      const codeInput = h('input.input', { type: 'text', inputmode: 'numeric', maxlength: 6, placeholder: '6 位验证码' });
      const genBtn = h('button.btn', { text: '① 生成密钥' });
      const enBtn = h('button.btn.btn-primary', { text: '② 验证并开启', disabled: true });
      let secret = '';

      genBtn.addEventListener('click', async () => {
        genBtn.disabled = true;
        try {
          const res = await api.totpSetup();
          secret = res.secret;
          secretBox.textContent = res.secret;
          enBtn.disabled = false;
          toast('已生成密钥，请在验证器 App 中手工添加', 'ok', 6000);
        } catch (e) { toast(e.message, 'err'); genBtn.disabled = false; }
      });
      enBtn.addEventListener('click', async () => {
        try {
          await api.totpEnable(secret, codeInput.value.trim());
          toast('两步验证已开启，下次登录需要输入动态码', 'ok', 6000);
          state.session.user.totp_enabled = true;
          renderTOTP();
        } catch (e) { toast(e.message, 'err'); }
      });

      totpBox.append(
        h('div.pill.warn', { text: '⚠️ 两步验证未开启' }),
        h('div.hint', { style: { margin: '12px 0' }, html: '在 Google Authenticator / 1Password / Microsoft Authenticator 中「手工输入密钥」添加账号，然后输入 App 显示的 6 位动态码完成绑定。' }),
        h('div.field', [h('label', { text: '密钥（手工输入到验证器）' }), secretBox]),
        h('div', { style: { display: 'flex', gap: '8px', marginBottom: '14px' } }, [genBtn]),
        h('div.field', [h('label', { text: '动态验证码' }), codeInput]),
        enBtn,
      );
    }
    renderTOTP();

    let sessions = [];
    try { sessions = (await api.sessions()).list || []; } catch { /* 忽略 */ }

    body.append(
      h('div.card', [
        h('div.card-head', [
          h('h3', { text: '用户名' }),
          h('div.spacer'),
          h('span.sub', { text: '改名后登录用新用户名，当前会话不受影响' }),
        ]),
        h('div.card-body', [
          h('div.row', [
            h('div.field', [h('label', { text: '用户名' }), newName]),
            h('div.field', [h('label', { text: '当前密码' }), namePwd]),
          ]),
          nameBtn,
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '修改密码' }), h('div.spacer'), h('span.sub', { text: '修改后所有登录会话立即失效' })]),
        h('div.card-body', [
          h('div.row', [
            h('div.field', [h('label', { text: '当前密码' }), oldPwd]),
            h('div.field', [h('label', { text: '新密码' }), newPwd]),
            h('div.field', [h('label', { text: '确认新密码' }), newPwd2]),
          ]),
          pwdBtn,
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '两步验证（2FA）' }), h('div.spacer'), h('span.sub', { text: '强烈建议开启' })]),
        h('div.card-body', [totpBox]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '登录会话' }), h('div.spacer'), h('span.sub', { text: `共 ${sessions.length} 个活跃会话` })]),
        h('div.card-body.tight', [
          h('table.table', [
            h('thead', [h('tr', [h('th', { text: 'IP' }), h('th', { text: '客户端' }), h('th', { text: '最近活跃' }), h('th', { text: '到期时间' })])]),
            h('tbody', sessions.map((s) => h('tr', [
              h('td.mono', { text: s.ip || '-' }),
              h('td', { text: (s.user_agent || '-').slice(0, 60) }),
              h('td.num', { text: s.last_seen }),
              h('td.num', { text: s.expires_at }),
            ]))),
          ]),
        ]),
      ]),
    );
  }


  renderTabs();
  renderBody();
  // 这里**不再**有「已迁移的功能」指路卡片：2026-09-20 用户要求把「检查更新」
  // 收回设置页（重复入口让人困惑），功能就在上面第 4 个 Tab 里，再放一个
  // "去别处"的按钮只会把用户又支到别的地方。
  content.append(tabBar, body);
}

// ============================================================================
//  开发中的模块占位
// ============================================================================

export function ComingSoonView(content, ctx = {}) {
  clear(content);
  const item = ctx.item || {};
  const phase = item.phase || '后续阶段';

  const roadmap = {
    P2: ['站点增删改查与 nginx 配置生成', '伪静态规则模板（Typecho / WordPress / Laravel 等）',
      '多版本 PHP 绑定与切换', 'Let\'s Encrypt 一键签发与自动续期', '反向代理可视化配置'],
    P3: ['服务注册表（裸装 + Docker 统一抽象）', '服务启停、实时日志、健康检查',
      'Compose 编辑器与一键部署', '应用市场：STT/TTS/IOPaint/Ollama/Uptime Kuma 等'],
    P4: ['文件管理器与在线代码编辑', 'Web 终端（浏览器内 shell）', '计划任务（定时备份/脚本）',
      '日志中心（聚合 + 实时尾随）', '数据库管理（库/用户/导入导出）'],
    P1: ['两步验证强制策略', '操作审计检索与导出', '系统告警通知'],
  }[phase] || [];

  content.append(
    h('div.card', [
      h('div.card-body', [
        h('div.empty', [
          h('div.big', { text: item.icon || '🚧' }),
          h('h4', { text: `${item.title || '该模块'}正在开发中` }),
          h('p', { text: `计划在 ${phase} 阶段交付。以下是该模块将包含的能力：` }),
        ]),
        h('div', { style: { maxWidth: '620px', margin: '0 auto' } },
          roadmap.map((t) => h('div', {
            style: {
              display: 'flex', gap: '9px', padding: '8px 0',
              borderBottom: '1px solid var(--border-soft)', fontSize: '13px', color: 'var(--text-dim)',
            },
          }, [h('span', { text: '•', style: { color: 'var(--brand)' } }), h('span', { text: t })]))),
        h('div', { style: { textAlign: 'center', marginTop: '22px' } }, [
          h('button.btn.btn-primary', { text: '返回仪表盘', onclick: () => go('dashboard') }),
        ]),
      ]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '当前已交付' })]),
      h('div.card-body', [
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
          h('span.pill.ok', { text: '远程访问 + 自签 HTTPS' }),
          h('span.pill.ok', { text: '登录鉴权 + 会话管理' }),
          h('span.pill.ok', { text: '两步验证 TOTP' }),
          h('span.pill.ok', { text: '访问策略（IP 白名单）' }),
          h('span.pill.ok', { text: '实时系统监控（仪表盘）' }),
          h('span.pill.ok', { text: 'mac 设置' }),
          h('span.pill.ok', { text: '操作审计' }),
          h('span.pill.ok', { text: '内存级登录限流' }),
        ]),
      ]),
    ]),
  );
}
