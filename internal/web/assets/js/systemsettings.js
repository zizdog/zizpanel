// systemsettings.js —— 「系统设置」页：把 macOS 本身配成一台服务器。
//
// 这一页取代了原来的「系统监控」页（用户判断：监控页和仪表盘内容高度重复）。
// 重复的指标已经并进仪表盘，这里只做**监控页做不到的事**：改 macOS 的设置。
//
// 三条纪律（对应后端 sysconfig 包）：
//   1. 先显示**当前真实状态**，再提供动作 —— 不用"上次点过按钮"这种记忆。
//   2. 动作全部走**任务中心**：用户能看见每条命令与输出，关掉窗口也不中断。
//   3. 机器不支持的项（例如笔记本 pmset 没有 autorestart）**明确标出来**，
//      不提供一个点了会静默无效的按钮 —— 那比没有更糟。

import { api } from './api.js';
import { h, clear, toast, confirmBox, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

// 动作目录：id 要与后端 handleSystemSettingsAction 里的 key 一一对应。
// dangerous 的动作在执行前要用户确认（会在系统里留下可见变化）。
const ACTIONS = {
  'server-mode': { title: '一键设为服务器模式', confirm: true },
  power: { title: '关闭睡眠与节能策略' },
  'block-updates': { title: '彻底阻止系统更新' },
  'restore-updates': { title: '恢复系统更新（撤销阻断）', confirm: true, danger: true },
  'verify-updates': { title: '验证更新阻断' },
  'silence-diagnostics': { title: '关闭崩溃报告弹窗与诊断上报' },
  'spotlight-off': { title: '关闭 Spotlight 索引' },
  'spotlight-on': { title: '开启 Spotlight 索引' },
  'enable-ssh': { title: '开启远程登录（SSH）' },
  'disable-autologin': { title: '关闭自动登录', confirm: true, danger: true },
};

// 更新相关偏好的中文名。判断依据是"安装类键是否为 0"，这里如实展示原始值。
const PREF_LABELS = {
  AutomaticCheckEnabled: '自动检查更新',
  AutomaticDownload: '自动下载更新',
  AutomaticallyInstallMacOSUpdates: '自动安装 macOS 更新',
  CriticalUpdateInstall: '安装关键/安全更新',
  ConfigDataInstall: '安装配置数据更新',
  AutomaticRestartAfterUpdate: '更新后自动重启',
  AppStoreAutoUpdate: 'App Store 自动更新',
};
// 这几个键必须为 0 才算"不会自己动手装东西"。
const PREF_CRITICAL = new Set([
  'AutomaticDownload', 'AutomaticallyInstallMacOSUpdates', 'CriticalUpdateInstall', 'ConfigDataInstall',
]);

export function SystemSettingsView(content, ctx = {}) {
  clear(content);

  const warnings = h('div');
  const summary = h('dl.kv');
  const powerBody = h('tbody');
  const updateBody = h('tbody');
  const updateStatus = h('div');
  const diagBody = h('dl.kv');
  const remoteBody = h('dl.kv');

  const refreshBtn = h('button.btn.btn-sm', { text: '⟳ 刷新状态', onclick: () => load(true) });

  appendAll(content,
    warnings,

    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '主机状态' }),
        h('div.spacer'),
        refreshBtn,
      ]),
      h('div.card-body', [summary]),
    ]),

    // ---------- 一键服务器模式 ----------
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '一键设为服务器模式' }),
        h('div.spacer'),
        h('span.sub', { text: '推荐：新机器或重装后先点这个' }),
      ]),
      h('div.card-body', [
        h('p.hint', {
          text: '按顺序执行下面这些设置。每一步的命令与输出都会显示在任务中心的进度窗里，' +
            '执行完会重新读一遍真实值复核 —— 不看退出码，只看系统有没有真的变。',
        }),
        h('ol', { style: { margin: '8px 0 12px 20px', lineHeight: '1.9' } }, [
          h('li', { text: '关闭系统睡眠与硬盘睡眠（服务器睡了就等于离线）' }),
          h('li', { text: '打开断电自恢复（若本机 pmset 支持；不支持的机型会跳过并说明）' }),
          h('li', { text: '彻底阻止 macOS 系统更新（偏好 + hosts 双重阻断）' }),
          h('li', { text: '关闭崩溃报告弹窗与自动上报（不再弹窗打扰）' }),
          h('li', { text: '关闭 Spotlight 索引（省 CPU/IO）' }),
          h('li', { text: '开启远程登录 SSH（否则重启后可能再也连不上）' }),
        ]),
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
          actionBtn('server-mode', 'btn-primary'),
        ]),
      ]),
    ]),

    h('div.grid.grid-2', [
      // ---------- 电源与睡眠 ----------
      h('div.card', [
        h('div.card-head', [
          h('h3', { text: '电源与睡眠' }),
          h('div.spacer'),
          h('span.sub', { text: 'pmset' }),
        ]),
        h('div.card-body.tight', [
          h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '项目' }), h('th', { text: '当前' }),
              h('th', { text: '服务器应为' }), h('th', { text: '状态' }),
            ])]),
            powerBody,
          ]),
        ]),
        h('div.card-body', [
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            actionBtn('power', 'btn-primary'),
          ]),
        ]),
      ]),

      // ---------- 系统更新 ----------
      h('div.card', [
        h('div.card-head', [
          h('h3', { text: '系统更新阻断' }),
          h('div.spacer'),
          h('span.sub', { text: '偏好 + hosts' }),
        ]),
        h('div.card-body', [updateStatus]),
        h('div.card-body.tight', [
          h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '设置项' }), h('th', { text: '当前值' }), h('th', { text: '判定' }),
            ])]),
            updateBody,
          ]),
        ]),
        h('div.card-body', [
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            actionBtn('block-updates', 'btn-primary'),
            actionBtn('verify-updates'),
            actionBtn('restore-updates'),
          ]),
        ]),
      ]),
    ]),

    h('div.grid.grid-2', [
      // ---------- 诊断与索引 ----------
      h('div.card', [
        h('div.card-head', [h('h3', { text: '崩溃报告与索引' })]),
        h('div.card-body', [diagBody]),
        h('div.card-body', [
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            actionBtn('silence-diagnostics', 'btn-primary'),
            actionBtn('spotlight-off'),
            actionBtn('spotlight-on'),
          ]),
        ]),
      ]),

      // ---------- 远程访问与登录 ----------
      h('div.card', [
        h('div.card-head', [h('h3', { text: '远程访问与登录' })]),
        h('div.card-body', [remoteBody]),
        h('div.card-body', [
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            actionBtn('enable-ssh', 'btn-primary'),
            actionBtn('disable-autologin'),
          ]),
        ]),
      ]),
    ]),
  );

  // actionBtn 造一个"跑一个设置动作"的按钮（带确认与任务中心提交）。
  function actionBtn(action, cls = '') {
    const meta = ACTIONS[action] || { title: action };
    return h(`button.btn.btn-sm${cls ? '.' + cls : ''}`, {
      text: meta.title,
      onclick: () => runAction(action),
    });
  }

  async function runAction(action) {
    const meta = ACTIONS[action] || { title: action };
    if (meta.confirm) {
      const msg = action === 'restore-updates'
        ? '恢复系统更新会撤掉 hosts 阻断段，并把自动更新偏好打开 —— 这台机器之后可能自己下载更新。确定？'
        : action === 'disable-autologin'
          ? '关闭自动登录后，重启需要手动输入密码才能进桌面。如果这是无人值守的机器，确定？'
          : '会修改系统设置（pmset / hosts / 偏好文件）。确定继续？';
      const yes = await confirmBox(msg, {
        title: meta.title,
        danger: !!meta.danger,
        okText: '执行',
      });
      if (!yes) return;
    }
    // 交给任务中心：立刻返回 task_id，进度窗由它打开；关掉窗口不会中断任务。
    taskCenter.start({
      kind: 'sysconfig',
      target: action,
      title: meta.title,
      start: () => api.systemSettingsAction(action),
      // 任务结束（成功或失败）都重新探一次真实状态：界面必须反映机器现状，
      // 而不是"我们请求过要改"。
      onDone: () => load(false),
    });
  }

  async function load(showToast) {
    clear(summary);
    summary.append(h('dt', { text: '读取中…' }), h('dd', { text: '' }));
    let st;
    try {
      st = await api.systemSettings();
    } catch (e) {
      clear(summary);
      summary.append(h('dt', { text: '读取失败' }), h('dd', { text: e.message }));
      return;
    }
    clear(summary);
    if (showToast) toast('已刷新系统状态', 'ok');

    // ---------- 警告 ----------
    clear(warnings);
    (st.warnings || []).forEach((w) => {
      warnings.append(h('div.card', [
        h('div.card-body', [
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start' } }, [
            h('span', { text: '⚠️' }),
            h('div', { text: w }),
          ]),
        ]),
      ]));
    });

    // ---------- 主机状态 ----------
    const rows = [
      ['机型', st.model || '—'],
      ['面板权限', st.is_root ? 'root（可以改系统设置）' : '非 root —— 系统设置将无法生效'],
      ['远程登录 SSH', st.ssh_listening ? `正在监听 ${st.ssh_port} 端口` : `未监听（${st.ssh_port} 端口没人听）`],
      ['自动登录', st.auto_login ? st.auto_login : '未开启'],
      ['Tailscale', st.tailscale ? '已安装' : '未安装'],
    ];
    rows.forEach(([k, v]) => summary.append(h('dt', { text: k }), h('dd', { text: v })));

    // ---------- 电源 ----------
    clear(powerBody);
    (st.power || []).forEach((p) => {
      powerBody.append(h('tr', [
        h('td', [
          h('div', { text: p.label }),
          p.key ? h('div.hint.mono', { text: p.key }) : null,
          p.note ? h('div.hint', { text: p.note }) : null,
        ]),
        h('td.mono', { text: p.current || '—' }),
        h('td.mono', { text: p.desired || '—' }),
        h('td', [
          !p.supported
            ? h('span.pill.warn', { text: '本机不支持' })
            : (p.ok ? h('span.pill.ok', { text: '已是服务器值' }) : h('span.pill.danger', { text: '待调整' })),
        ]),
      ]));
    });

    // ---------- 更新 ----------
    clear(updateStatus);
    const up = st.updates || {};
    updateStatus.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      up.blocked ? h('span.pill.ok', { text: '已彻底阻断' }) : h('span.pill.danger', { text: '未阻断' }),
      h('span.sub', { text: up.note || '' }),
    ]));
    if (up.last_recommended) {
      updateStatus.append(h('div.hint', {
        text: '系统里还挂着一条推荐更新：' + up.last_recommended + '（再执行一次阻断会清掉它）',
      }));
    }

    clear(updateBody);
    const prefs = up.prefs || {};
    Object.keys(PREF_LABELS).forEach((k) => {
      if (!(k in prefs)) return;
      const v = prefs[k];
      let verdict;
      if (PREF_CRITICAL.has(k)) {
        verdict = v === '0'
          ? h('span.pill.ok', { text: '已关' })
          : h('span.pill.warn', { text: '开启（会自己装更新）' });
      } else if (k === 'AutomaticCheckEnabled') {
        // 这个键 macOS 有时会自己删掉；删掉不影响阻断（还有 hosts 兜底），
        // 所以这里不去吓用户，只说"不依赖它"。
        verdict = v === '0' ? h('span.pill.ok', { text: '已关' }) : h('span.hint', { text: '不依赖此项' });
      } else {
        verdict = h('span.hint', { text: '—' });
      }
      updateBody.append(h('tr', [
        h('td', { text: PREF_LABELS[k] }),
        h('td.mono', { text: v }),
        h('td', [verdict]),
      ]));
    });
    updateBody.append(h('tr', [
      h('td', [
        h('div', { text: 'hosts 阻断段' }),
        h('div.hint', { text: '把更新目录域名解析到 0.0.0.0' }),
      ]),
      h('td.mono', { text: up.hosts_blocked ? '存在' : '不存在' }),
      h('td', [up.hosts_blocked ? h('span.pill.ok', { text: '已挡' }) : h('span.pill.danger', { text: '未挡' })]),
    ]));

    // ---------- 诊断与索引 ----------
    clear(diagBody);
    [
      ['崩溃报告弹窗', st.crash_dialog || '—'],
      ['诊断上报', st.diagnostics_silenced ? '已静默' : '未静默（可能仍会弹窗/上报）'],
      ['Spotlight 索引', st.spotlight || '—'],
    ].forEach(([k, v]) => diagBody.append(h('dt', { text: k }), h('dd', { text: v })));

    // ---------- 远程访问与登录 ----------
    clear(remoteBody);
    [
      ['SSH 端口', String(st.ssh_port || 22)],
      ['SSH 状态', st.ssh_listening ? '正在监听' : '未监听'],
      ['自动登录用户', st.auto_login ? st.auto_login : '未开启（安全）'],
    ].forEach(([k, v]) => remoteBody.append(h('dt', { text: k }), h('dd', { text: v })));
  }

  // 注意：**不**订阅任务中心的进度变化来刷新状态。
  // onChange 每来一行日志都会触发，而 load() 要真去跑 pmset/defaults/lsof
  // 十几条命令 —— 订阅它会让页面在任务运行时疯狂探测。
  // 复核只发生在动作结束（onDone）与用户点「刷新状态」时。
  load(false);
}
