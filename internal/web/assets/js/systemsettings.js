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

  // ---------- 内网段预授权（可选、可一键撤销；见后端 lan_preauth.go）----------
  const lanStatus = h('div');
  const lanToggle = h('input', { type: 'checkbox' });
  const lanInput = h('input.input', {
    type: 'text',
    placeholder: '例如 192.168.1.0/24（多个用逗号分隔）',
  });
  const lanMsg = h('div');
  const lanSaveBtn = h('button.btn.btn-sm.btn-primary', { text: '保存', onclick: () => submitLAN(lanToggle.checked) });
  const lanRollbackBtn = h('button.btn.btn-sm', { text: '撤销', onclick: () => submitLAN(false) });
  // 用户手动改过输入框之后，刷新状态不再覆盖它（否则会把正在编辑的内容冲掉）。
  let lanTouched = false;
  lanInput.addEventListener('input', () => { lanTouched = true; });

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

    // ---------- 允许免授权访问内网段（可选、可一键撤销）----------
    //
    // 为什么单独一张卡：这是**降低隐私门强度**的可选项，必须在界面上把代价
    // 写清楚（对所有程序生效、需重启），并且状态要显示磁盘上的真实值。
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '允许免授权访问内网段' }),
        h('div.spacer'),
        h('span.sub', { text: '可选 · 会削弱 macOS「本地网络」隐私门 · 需重启' }),
      ]),
      h('div.card-body', [
        h('p.hint', {
          text: 'macOS 15 的「本地网络」隐私门会拦住没拿到授权的程序访问局域网。' +
            'Homebrew 的 nginx 是 ad-hoc 签名、标识随升级变化，无头服务器上没人点授权弹窗，' +
            '所以它的局域网反代会 502。开启这一项会写入 Apple 官方的预授权键' +
            '（com.apple.network.local-network 的两个 CIDR 数组，系统域与真实用户域各一份），' +
            '让系统把这个网段当成"不是本地网络"。',
        }),
        h('p.hint', [
          '代价：这个网段在本机上对',
          h('strong', { text: '所有程序' }),
          '都不再受这道隐私门限制（不是只对 nginx 或面板）。改动必须重启后才生效；' +
            '撤销同样是重启后才不再豁免，但不会清除系统里已登记或授权过的程序（例如手动点过「允许」的）。',
        ]),
        lanStatus,
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', margin: '12px 0 6px' } }, [
          lanToggle,
          h('span', { text: '写入预授权（勾选后点「保存」写入；取消勾选后点「保存」= 删除）' }),
        ]),
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '10px' } }, [
          h('span.hint', { text: '网段（CIDR，逗号分隔）' }),
          lanInput,
        ]),
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
          lanSaveBtn,
          lanRollbackBtn,
        ]),
        lanMsg,
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
    // 受 macOS 隐私保护（TCC）的目录：读不到就直说，并给出授权路径。
    // 用户反馈过"把 ~/Documents 挂进 File Browser 却看不到文件、终端里报
    // Operation not permitted" —— 那不是 Docker 的问题，是这里。
    (st.protected_dirs || []).forEach((d) => {
      summary.append(
        h('dt', { text: d.name }),
        h('dd', [
          d.readable
            ? h('span.pill.ok', { text: '可读' })
            : h('span.pill.danger', { text: '读不到（需完全磁盘访问权限）', title: d.err || '' }),
          h('span.hint', { text: ' ' + d.path }),
        ]),
      );
    });

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

    // ---------- 内网段预授权 ----------
    renderLAN(st);
  }

  // renderLAN 把后端探测到的真实状态画成状态行（不靠"上次点过按钮"的记忆）。
  function renderLAN(st) {
    const lp = st.lan_preauth || {};
    clear(lanStatus);
    clear(lanMsg);
    // 默认值：已设置就显示当前网段，否则显示自动推导出来的默认网段。
    if (!lanTouched && !lanInput.value) {
      lanInput.value = (lp.cidrs && lp.cidrs.length)
        ? lp.cidrs.join(', ')
        : (lp.detected_cidr || '');
    }
    if (!lp.supported) {
      lanToggle.disabled = true;
      lanStatus.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.warn', { text: '本机不支持' }),
        h('span.sub', { text: '找不到 /usr/bin/defaults，无法读写这个偏好域。' }),
      ]));
      return;
    }
    lanToggle.disabled = false;
    if (!lp.readable) {
      // 读不到就直说读不到 —— 不猜一个状态给用户。
      lanStatus.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.danger', { text: '状态读取失败' }),
        h('span.sub', { text: lp.read_error || '读不到当前状态。' }),
      ]));
      return;
    }
    let pill;
    if (lp.enabled) {
      lanToggle.checked = true;
      pill = lp.reboot_required
        ? h('span.pill.warn', { text: '已写入 · 重启后生效' })
        : h('span.pill.danger', { text: '已生效 · 该网段对所有程序放行' });
    } else if (lp.partial) {
      lanToggle.checked = true;
      pill = h('span.pill.warn', { text: '不完整 · 只写进了一个域' });
    } else if (lp.reboot_required) {
      // 磁盘上已经没有设置，但文件是本次开机后改的 → 要重启才恢复隐私门。
      lanToggle.checked = false;
      pill = h('span.pill.warn', { text: '已撤销 · 重启后恢复隐私门' });
    } else {
      lanToggle.checked = false;
      pill = h('span.pill.ok', { text: '未开启 · 仍受隐私门保护' });
    }
    const row = [pill];
    if (lp.cidrs && lp.cidrs.length) row.push(h('span.mono', { text: lp.cidrs.join(', ') }));
    lanStatus.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, row));
    if (lp.reboot_note) lanStatus.append(h('div.hint', { text: lp.reboot_note }));
    else if (lp.note) lanStatus.append(h('div.hint', { text: lp.note }));
    if (lp.warning) lanStatus.append(h('div.hint', { text: '⚠️ ' + lp.warning }));
  }

  // submitLAN 写入（enabled=true）或撤销（enabled=false）。
  //
  // 纪律：只有后端真的返回成功才提示"已写入/已撤销"；失败时如实显示错误，
  // 并且**不**去刷新状态（避免把错误信息冲掉）。
  async function submitLAN(enable) {
    if (enable) {
      const cidrs = (lanInput.value || '').trim();
      if (!cidrs) {
        showLANMsg('请先填一个网段（CIDR），例如 192.168.1.0/24。');
        lanInput.focus();
        return;
      }
    }
    const yes = await confirmBox(
      enable
        ? '会写入 Apple 官方预授权键（系统域 + 真实用户域），让该网段对所有程序都不再受' +
          '「本地网络」隐私门限制，而且必须重启后才生效。确定写入？'
        : '会删除两个域的预授权键；重启后该网段不再豁免。注意：已登记/授权过的程序（例如手动点过' +
          '「允许」的）不会被清除。确定撤销？',
      {
        title: enable ? '允许免授权访问内网段' : '撤销内网段预授权',
        danger: true,
        okText: enable ? '写入' : '撤销',
      },
    );
    if (!yes) return;
    setLANBusy(true);
    try {
      const st = await api.systemSettingsLANPreauth({
        enabled: enable,
        cidrs: enable ? lanInput.value.trim() : '',
      });
      toast(enable ? '已写入预授权，重启后生效' : '已撤销预授权，重启后恢复隐私门', 'ok');
      // 直接渲染后端返回的**真实状态**：它带着"刚写入 → 必须重启"的强制标记。
      // 这里不再重新探测一次，免得 cfprefsd 还没落盘时把"需重启"读丢。
      renderLAN({ lan_preauth: st });
    } catch (e) {
      showLANMsg((enable ? '写入失败：' : '撤销失败：') + (e && e.message ? e.message : e));
    } finally {
      setLANBusy(false);
    }
  }

  function showLANMsg(text) {
    clear(lanMsg);
    lanMsg.append(h('div', { style: { marginTop: '10px' } }, [
      h('span.pill.danger', { text: '⚠️ ' + text }),
    ]));
  }

  function setLANBusy(busy) {
    lanSaveBtn.disabled = busy;
    lanRollbackBtn.disabled = busy;
  }

  // 注意：**不**订阅任务中心的进度变化来刷新状态。
  // onChange 每来一行日志都会触发，而 load() 要真去跑 pmset/defaults/lsof
  // 十几条命令 —— 订阅它会让页面在任务运行时疯狂探测。
  // 复核只发生在动作结束（onDone）与用户点「刷新状态」时。
  load(false);
}
