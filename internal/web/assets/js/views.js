// views.js —— 各功能页面。
//
// 约定：每个 View(content, ctx) 负责把内容渲染进 content 元素。
// 需要长连接的页面（如仪表盘）在离开时通过 ctx.onLeave 注册清理函数。

import { api, sse, apiURL } from './api.js';
import {
  h, clear, toast, modal, confirmBox, bytes, rate, pct, duration,
  levelOf, Sparkline, $,
} from './ui.js';
import { state, NAV } from './app.js';

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
    metricCard('cpu', 'CPU 使用率', '🧠'),
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

  // ---------- 待接入服务（服务管理在 P3 交付） ----------
  const servicesCard = h('div.card', [
    h('div.card-head', [
      h('h3', { text: '服务状态' }),
      h('div.spacer'),
      h('span.sub', { text: '服务管理模块将在 P3 提供完整启停与日志' }),
    ]),
    h('div.card-body', [
      h('div.empty', [
        h('div.big', { text: '⚙️' }),
        h('h4', { text: '此模块正在开发中' }),
        h('p', { text: '当前可先用系统监控查看进程与资源占用。' }),
      ]),
    ]),
  ]);

  content.append(
    topGrid,
    h('div.grid.grid-2', [sysCard, charts]),
    procCard,
    servicesCard,
  );

  // ---------- 数据更新 ----------
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
      ['进程数', String(s.procs)],
      ['CPU 温度', s.cpu_temp > 0 ? s.cpu_temp + ' °C' : '不可读取（需 root）'],
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
//  系统监控
// ============================================================================

export function MonitorView(content, ctx = {}) {
  clear(content);

  const spark = new Sparkline({ max: 120, height: 120, color: 'var(--brand)' });
  const memSpark = new Sparkline({ max: 120, height: 120, color: 'var(--ok)' });
  const statsList = h('dl.kv');
  const procBody = h('tbody');

  content.append(
    h('div.grid.grid-2', [
      h('div.card', [
        h('div.card-head', [h('h3', { text: 'CPU 使用率' }), h('div.spacer'), h('span.sub', { text: '每 2 秒采样' })]),
        h('div.card-body', [spark.svg]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '内存使用率' }), h('div.spacer'), h('span.sub', { text: '每 2 秒采样' })]),
        h('div.card-body', [memSpark.svg]),
      ]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '详细指标' })]),
      h('div.card-body', [statsList]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '进程 Top 20' })]),
      h('div.card-body.tight', [
        h('table.table', [
          h('thead', [h('tr', [
            h('th', { text: 'PID' }), h('th', { text: '程序' }),
            h('th', { text: 'CPU' }), h('th', { text: '内存' }), h('th', { text: '常驻内存' }),
          ])]),
          procBody,
        ]),
      ]),
    ]),
  );

  const update = (s) => {
    spark.push(s.cpu_used);
    memSpark.push(s.mem_total ? (s.mem_used / s.mem_total) * 100 : 0);
    clear(statsList);
    const rows = [
      ['CPU 使用率', pct(s.cpu_used)],
      ['系统负载 (1/5/15)', `${s.load_1.toFixed(2)} / ${s.load_5.toFixed(2)} / ${s.load_15.toFixed(2)}`],
      ['物理内存', `${bytes(s.mem_used)} / ${bytes(s.mem_total)}`],
      ['空闲内存', bytes(s.mem_free)],
      ['可回收内存', bytes(s.mem_cached)],
      ['Swap 使用', s.swap_total ? `${bytes(s.swap_used)} / ${bytes(s.swap_total)}` : '未启用'],
      ['磁盘空间', `${bytes(s.disk_used)} / ${bytes(s.disk_total)}（剩余 ${bytes(s.disk_free)}）`],
      ['网络累计接收', bytes(s.net_rx)],
      ['网络累计发送', bytes(s.net_tx)],
      ['实时下行', rate(s.net_rx_rate)],
      ['实时上行', rate(s.net_tx_rate)],
      ['进程总数', String(s.procs)],
      ['系统运行时长', duration(s.uptime)],
    ];
    rows.forEach(([k, v]) => statsList.append(h('dt', { text: k }), h('dd', { text: v })));
  };

  const procTimer = setInterval(async () => {
    try {
      const res = await api.processes('cpu', 20);
      clear(procBody);
      (res.list || []).forEach((p) => procBody.append(h('tr', [
        h('td.num.mono', { text: p.pid }),
        h('td.mono', { text: p.command, title: p.command }),
        h('td.num', { text: p.cpu != null ? p.cpu.toFixed(1) + '%' : '—' }),
        h('td.num', { text: (p.mem || 0).toFixed(1) + '%' }),
        h('td.num', { text: bytes((p.rss || 0) * 1024) }),
      ])));
    } catch { /* 静默 */ }
  }, 4000);

  const es = sse(apiURL('system/stream?interval=2'), { onSample: update });
  if (ctx.onLeave) {
    ctx.onLeave(() => { es.close(); clearInterval(procTimer); });
  }
}

// ============================================================================
//  面板设置
// ============================================================================

export function SettingsView(content) {
  clear(content);
  const user = state.session?.user || {};
  const cfg = state.session?.config || {};

  const tabs = [
    { id: 'access', title: '访问与安全' },
    { id: 'terminal', title: '文件与终端' },
    { id: 'account', title: '账号与两步验证' },
    { id: 'about', title: '关于与运维' },
  ];
  let active = 'access';

  const body = h('div');
  const tabBar = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '16px', flexWrap: 'wrap' } });

  function renderTabs() {
    clear(tabBar);
    tabs.forEach((t) => tabBar.append(h(`button.btn.btn-sm${active === t.id ? '.btn-primary' : ''}`, {
      text: t.title,
      onclick: () => { active = t.id; renderTabs(); renderBody(); },
    })));
  }

  async function renderBody() {
    clear(body);
    if (active === 'access') await renderAccess();
    else if (active === 'terminal') await renderTerminal();
    else if (active === 'account') await renderAccount();
    else renderAbout();
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
    const sessionHours = h('input.input', { type: 'number', value: s.session_hours, min: 1, max: 720 });
    const maxFail = h('input.input', { type: 'number', value: s.login_max_fail, min: 1, max: 50 });
    const lockMins = h('input.input', { type: 'number', value: s.login_lock_mins, min: 1, max: 1440 });

    const save = h('button.btn.btn-primary', {
      text: '保存设置',
      onclick: async () => {
        save.disabled = true;
        try {
          const patch = {
            access_mode: mode.value,
            ip_whitelist: whitelist.value.split('\n').map((x) => x.trim()).filter(Boolean),
            trust_proxy: trustProxy.checked,
            session_hours: Number(sessionHours.value),
            login_max_fail: Number(maxFail.value),
            login_lock_mins: Number(lockMins.value),
          };
          await api.saveSettings(patch);
          state.session.config = Object.assign({}, state.session.config, { access_mode: patch.access_mode });
          toast('设置已保存并立即生效', 'ok');
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

  // ---------- 关于 ----------
  // ---------- 在线升级 ----------
  //
  // 这个卡片最特殊的地方：升级过程中面板会**重启**，所有请求都会失败一段时间。
  // 因此它的行为不是"调一次接口拿结果"，而是：
  //   发起升级 → 持续轮询 → 容忍连接失败 → 面板回来后读到看门狗写下的最终结论。
  // 任何"发起后弹个成功提示就完事"的写法都是错的：那时新版都还没开始跑。
  // 状态条：项目里没有 .notice 这类现成类名，统一用内联样式 + CSS 变量。
  // 单独抽出来是因为升级卡片有 5 种状态要展示，散落 5 处内联样式必然走形。
  function banner(kind, nodes) {
    const bg = { ok: 'var(--ok-soft)', err: 'var(--danger-soft)', warn: 'var(--warn-soft)' }[kind] || 'var(--panel-2)';
    const color = { ok: 'var(--ok)', err: 'var(--danger)', warn: 'var(--warn)' }[kind] || 'var(--text)';
    return h('div', {
      style: {
        padding: '11px 13px', background: bg, color, borderRadius: 'var(--radius)',
        fontSize: '12.5px', lineHeight: '1.7', marginBottom: '13px',
      },
    }, nodes);
  }
  const mutedStyle = { color: 'var(--text-mute)', fontSize: '11.5px', marginTop: '6px', lineHeight: '1.6' };
  const actionsStyle = {
    display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginTop: '10px',
  };

  function renderUpgradeCard() {
    const card = h('div.card', [
      h('div.card-head', [
        h('h3', { text: '在线升级' }),
        h('div.spacer'),
        h('span', { id: 'zp-up-ver', style: { color: 'var(--text-mute)', fontSize: '12px' }, text: '读取中…' }),
      ]),
    ]);
    const bodyEl = h('div.card-body');
    card.append(bodyEl);

    let pollTimer = null;
    let pollDeadline = 0;

    function stopPoll() {
      if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
    }

    function render(info) {
      clear(bodyEl);
      const st = info.state || {};
      const status = st.status || 'idle';

      // ---- 顶部状态条：把"正在发生什么"讲清楚 ----
      if (status === 'applying' || status === 'restarting') {
        bodyEl.append(banner('warn', [
          h('strong', { text: '升级进行中：' }),
          h('span', { text: st.message || st.stage || '正在替换程序并重启面板…' }),
          h('div', { style: mutedStyle, text: '面板即将短暂断开，页面会自动重连。请不要关闭这个页面。' }),
        ]));
      } else if (status === 'success') {
        bodyEl.append(banner('ok', [
          h('strong', { text: '升级成功：' }),
          h('span', { text: `${st.message || ''}（当前 v${st.to || '?'}）` }),
        ]));
      } else if (status === 'rolled_back') {
        bodyEl.append(banner('err', [
          h('strong', { text: '升级失败，已自动回滚：' }),
          h('span', { text: st.message || '' }),
          st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null,
          h('div', { style: mutedStyle, text: '面板已恢复到升级前的版本，可以正常使用。请把上面这条原因反馈给开发者。' }),
        ]));
      } else if (status === 'failed') {
        bodyEl.append(banner('err', [
          h('strong', { text: '升级未执行：' }),
          h('span', { text: st.message || '' }),
          st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null,
        ]));
      }

      // ---- 版本与来源 ----
      const kv = h('dl.kv', [
        h('dt', { text: '当前版本' }), h('dd', { text: `v${info.current_version}（${info.arch}）` }),
        h('dt', { text: '构建时间' }), h('dd', { text: info.build_time || '-' }),
        h('dt', { text: '内嵌发布公钥' }), h('dd', {
          text: info.can_remote ? info.pubkey + '…' : '未配置（无法从网络升级）',
        }),
      ]);
      bodyEl.append(kv);

      if (!info.can_apply) {
        bodyEl.append(banner('warn', [
          h('strong', { text: '当前面板不是以 root 运行，无法自我升级。' }),
          h('div', { style: mutedStyle, text: '正式安装的面板由 LaunchDaemon 以 root 运行。本地调试实例请改用 install.sh 升级。' }),
        ]));
      }

      // ---- 升级源 ----
      const srcInput = h('input.input', {
        placeholder: 'https://example.com/zizpanel/releases（放着 manifest.json 的目录）',
        value: info.source || '',
      });
      bodyEl.append(h('div.field', [
        h('label', { text: '升级源地址' }),
        srcInput,
        h('div.hint', {
          text: info.can_remote
            ? '该目录需同时提供 manifest.json 与 manifest.json.sig，面板会用内嵌公钥验签后才会安装。'
            : '面板没有内嵌发布公钥，出于安全考虑会拒绝从网络升级；请使用下面的「上传升级包」。',
        }),
      ]));
      if (info.plain_http) {
        bodyEl.append(banner('warn', [
          h('span', { text: '注意：升级源使用明文 HTTP。签名验证仍然有效（攻击者拿不出私钥），但仍建议改用 HTTPS。' }),
        ]));
      }

      // ---- 操作按钮 ----
      const busy = status === 'applying' || status === 'restarting';
      const btnCheck = h('button.btn', {
        text: '检查更新',
        disabled: busy || undefined,
        onclick: async () => {
          btnCheck.disabled = true;
          btnCheck.textContent = '检查中…';
          try {
            const src = srcInput.value.trim();
            // 先把输入框的内容存进设置，再发起检查。
            //
            // 为什么必须这样：检查更新接口在收到空源时会**回退到配置里的旧值**，
            // 所以"把输入框清空再点检查"根本清不掉源 —— 用户会看到一个
            // 清不掉的地址，而面板还坚称"已是最新版本"。
            // 保存一次（空串即清空）之后，下面的检查才会真正走到"未配置"分支。
            await api.saveSettings({ upgrade_source: src });
            const res = await api.upgradeCheck(src);
            if (res.has_update) {
              toast(`发现新版本 v${res.latest}`, 'ok');
              if (res.asset_error) toast(res.asset_error, 'warn', 9000);
            } else {
              toast(`已是最新版本 v${res.current}`, 'ok');
            }
            render(await api.upgradeStatus());
          } catch (e) {
            toast(e.message, 'err', 9000);
          } finally {
            btnCheck.disabled = false;
            btnCheck.textContent = '检查更新';
          }
        },
      });

      const btnStage = h('button.btn.btn-primary', {
        text: '下载并准备升级',
        disabled: (busy || !info.can_remote) || undefined,
        onclick: async () => {
          btnStage.disabled = true;
          btnStage.textContent = '下载校验中…';
          try {
            const res = await api.upgradeStage(srcInput.value.trim());
            if (res.staged) toast(`v${res.version} 已就绪，可以升级`, 'ok', 8000);
            else toast(res.message || '无需升级', 'ok');
            render(await api.upgradeStatus());
          } catch (e) {
            toast(e.message, 'err', 12000);
            render(await api.upgradeStatus());
          } finally {
            btnStage.disabled = false;
            btnStage.textContent = '下载并准备升级';
          }
        },
      });

      bodyEl.append(h('div', { style: actionsStyle }, [btnCheck, btnStage]));

      // ---- 上传（离线路径）----
      const fileInput = h('input', { type: 'file', accept: '.tar.gz,.tgz' });
      const btnUpload = h('button.btn', {
        text: '上传升级包',
        disabled: busy || undefined,
        onclick: async () => {
          const f = fileInput.files && fileInput.files[0];
          if (!f) { toast('请先选择 .tar.gz 升级包', 'warn'); return; }
          btnUpload.disabled = true;
          btnUpload.textContent = '上传校验中…';
          try {
            const res = await api.upgradeUpload(f);
            toast(`已验证：包内版本 v${res.version}`, 'ok', 8000);
            render(await api.upgradeStatus());
          } catch (e) {
            toast(e.message, 'err', 12000);
          } finally {
            btnUpload.disabled = false;
            btnUpload.textContent = '上传升级包';
          }
        },
      });
      bodyEl.append(h('div.field', [
        h('label', { text: '离线升级（上传发布包）' }),
        h('div', { style: actionsStyle }, [fileInput, btnUpload]),
        h('div.hint', { text: '适合没有外网、或升级源不可达的情况。上传后同样会先解包试运行，确认无误才允许升级。' }),
      ]));

      // ---- 已就绪 → 立即升级 ----
      if (info.staged && status === 'staged') {
        const notes = info.notes || '';
        bodyEl.append(banner('ok', [
          h('strong', { text: `v${info.staged_version} 已准备就绪。` }),
          notes ? h('pre.logbox', { text: notes }) : null,
          h('div', { style: actionsStyle }, [
            h('button.btn.btn-primary', {
              text: `立即升级到 v${info.staged_version}`,
              onclick: () => {
                modal({
                  title: '确认升级',
                  body: h('div', [
                    h('p', { text: `即将把面板从 v${info.current_version} 升级到 v${info.staged_version}。` }),
                    h('p.muted', { text: '升级过程中面板会重启，页面会短暂断开；如果新版本启动失败，会自动回滚到当前版本。' }),
                  ]),
                  footer: (close) => [
                    h('button.btn', { text: '取消', onclick: close }),
                    h('button.btn.btn-primary', {
                      text: '开始升级',
                      onclick: async () => {
                        close();
                        try {
                          await api.upgradeApply();
                          toast('升级已开始，请等待面板重启', 'ok', 8000);
                          startPoll();
                          render(await api.upgradeStatus().catch(() => ({ state: { status: 'applying' } })));
                        } catch (e) {
                          toast(e.message, 'err', 12000);
                        }
                      },
                    }),
                  ],
                });
              },
            }),
            h('button.btn.btn-sm', {
              text: '放弃这个包',
              onclick: async () => {
                try { await api.upgradeDismiss(); toast('已清除', 'ok'); render(await api.upgradeStatus()); }
                catch (e) { toast(e.message, 'err'); }
              },
            }),
          ]),
        ]));
      }

      // ---- 结束态 → 清除 ----
      if (status === 'success' || status === 'rolled_back' || status === 'failed') {
        bodyEl.append(h('div', { style: actionsStyle }, [
          h('button.btn.btn-sm', {
            text: '知道了（清除提示）',
            onclick: async () => {
              try { await api.upgradeDismiss(); render(await api.upgradeStatus()); }
              catch (e) { toast(e.message, 'err'); }
            },
          }),
        ]));
      }

      const verEl = card.querySelector('#zp-up-ver');
      if (verEl) verEl.textContent = `v${info.current_version}`;
    }

    // 升级期间轮询。面板重启时请求会失败 —— 这是**预期行为**，
    // 所以失败不报错，只继续等，直到面板回来或超时。
    function startPoll() {
      stopPoll();
      pollDeadline = Date.now() + 5 * 60 * 1000;
      const tick = async () => {
        try {
          const info = await api.upgradeStatus();
          render(info);
          const s = info.state?.status;
          if (s === 'applying' || s === 'restarting') {
            // 面板还活着但仍在升级（还没重启），继续等
          } else {
            stopPoll();
            return;
          }
        } catch {
          // 面板正在重启：保持安静，继续尝试
        }
        if (Date.now() > pollDeadline) {
          stopPoll();
          return;
        }
        pollTimer = setTimeout(tick, 2000);
      };
      pollTimer = setTimeout(tick, 2000);
    }

    api.upgradeStatus()
      .then((info) => {
        render(info);
        const s = info.state?.status;
        if (s === 'applying' || s === 'restarting') startPoll();
      })
      .catch((e) => {
        clear(bodyEl);
        bodyEl.append(banner('err', [h('span', { text: '读取升级信息失败：' + e.message })]));
      });

    return card;
  }

  function renderAbout() {
    body.append(
      renderUpgradeCard(),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '面板信息' })]),
        h('div.card-body', [
          h('dl.kv', [
            h('dt', { text: '面板版本' }), h('dd', { text: state.session?.version || '-' }),
            h('dt', { text: 'Go 运行时' }), h('dd', { text: cfg.go_version || '-' }),
            h('dt', { text: '启动时间' }), h('dd', { text: cfg.started_at || '-' }),
            h('dt', { text: '安装标识' }), h('dd', { text: cfg.install_id || '-' }),
          ]),
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '常用运维命令' })]),
        h('div.card-body', [
          h('pre.logbox', {
            text: [
              '# 查看状态与访问地址',
              'zizpanel status',
              '',
              '# 忘记密码时重置（在终端执行，无需登录面板）',
              'sudo zizpanel reset-password <用户名>',
              '',
              '# 重启面板',
              'sudo launchctl kickstart -k system/cn.zizpanel.panel',
              '',
              '# 查看面板日志',
              'tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log',
              '',
              '# 重新生成 HTTPS 自签证书',
              'sudo zizpanel gen-cert',
              '',
              '# 卸载面板（保留网站数据）',
              'sudo /opt/zizpanel/uninstall.sh',
            ].join('\n'),
          }),
        ]),
      ]),
      h('div.card', [
        h('div.card-head', [h('h3', { text: '操作审计' }), h('div.spacer')]),
        h('div.card-body.tight', [auditTable()]),
      ]),
    );
  }

  function auditTable() {
    const tbody = h('tbody', [h('tr', [h('td', { colspan: 5 }, [h('div.empty', { text: '加载中…' })])])]);
    api.audit(40).then((res) => {
      clear(tbody);
      const list = res.list || [];
      if (!list.length) {
        tbody.append(h('tr', [h('td', { colspan: 5 }, [h('div.empty', [h('p', { text: '暂无审计记录' })])])]));
        return;
      }
      list.forEach((e) => tbody.append(h('tr', [
        h('td.num.mono', { text: e.ts }),
        h('td', { text: e.actor || '-' }),
        h('td.mono', { text: e.ip || '-' }),
        h('td', [h('span.pill.' + (e.ok ? 'ok' : 'danger'), { text: e.action })]),
        h('td', { text: e.detail || e.message || '' }),
      ])));
    }).catch(() => {});
    return h('table.table', [
      h('thead', [h('tr', [
        h('th', { text: '时间' }), h('th', { text: '操作者' }), h('th', { text: '来源 IP' }),
        h('th', { text: '动作' }), h('th', { text: '说明' }),
      ])]),
      tbody,
    ]);
  }

  renderTabs();
  renderBody();
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
          h('span.pill.ok', { text: '实时系统监控' }),
          h('span.pill.ok', { text: '操作审计' }),
          h('span.pill.ok', { text: '内存级登录限流' }),
        ]),
      ]),
    ]),
  );
}
