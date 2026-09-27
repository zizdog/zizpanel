// offsite.js —— 「异地备份」弹窗（唯一实现）。
//
// 入口有两处（「已有备份」卡片顶部 + 「计划任务」卡片顶部），都调用同一个
// openOffsiteModal()，避免出现两份走样的 UI。
//
// 口令只写进服务端 config.json：这里只看到 password_set（"已配置"），
// 输入框留空 = 不改。

import { api } from './api.js';
import { h, clear, toast, modal, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

// PROTOCOLS 是协议与加密方式的唯一映射（界面选项与默认端口都从这里来）。
const PROTOCOLS = {
  smtp: {
    label: 'SMTP（邮件附件）',
    port: { none: 25, starttls: 587, ssl: 465 },
    enc: [
      { v: 'none', t: '不加密（仅内网 / 25）' },
      { v: 'starttls', t: 'STARTTLS（587）' },
      { v: 'ssl', t: 'SSL 隐式加密（465）' },
    ],
  },
  ftp: {
    label: 'FTP',
    port: { none: 21, explicit_tls: 21 },
    enc: [
      { v: 'none', t: '不加密（明文，仅内网）' },
      { v: 'explicit_tls', t: '显式 TLS（AUTH TLS）' },
    ],
  },
  ftps: {
    label: 'FTPS（显式 TLS）',
    port: { explicit_tls: 21 },
    enc: [{ v: 'explicit_tls', t: '显式 TLS（AUTH TLS）' }],
  },
};

const STD_PORTS = [21, 25, 465, 587];

function field(label, node, hint) {
  return h('div.field', [h('label', { text: label }), node, hint ? h('div.hint', { text: hint }) : null]);
}

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 1 : 2) + ' ' + units[i];
}

function statusPill(e) {
  if (e.status === 'sent') return h('span.pill.ok', { text: '已发送' });
  if (e.status === 'skipped') return h('span.pill.warn', { text: '未发送', title: e.error || '' });
  return h('span.pill.danger', { text: '失败', title: e.error || '' });
}

// openOffsiteModal 打开「异地备份」弹窗（两处入口共用）。
export async function openOffsiteModal(onDone) {
  let data;
  try {
    data = await api.offsite();
  } catch (e) {
    toast('读取异地备份配置失败：' + e.message, 'err', 9000);
    return;
  }
  const st = data.settings || {};
  data.ledger = data.ledger || [];

  // ---- 表单元素 ----
  const enabled = h('input', { type: 'checkbox' });
  enabled.checked = !!st.enabled;
  const protocol = h('select.select', Object.entries(PROTOCOLS).map(([v, p]) =>
    h('option', { value: v, text: p.label, selected: (st.protocol || 'smtp') === v })));
  const host = h('input.input', { value: st.host || '', placeholder: '例如 smtp.example.com 或 FTP 服务器域名' });
  const port = h('input.input', { type: 'number', min: '1', max: '65535', value: String(st.port || '') });
  const encryption = h('select.select');
  const username = h('input.input', { value: st.username || '', placeholder: '可选（内网匿名 FTP 可留空）' });
  const password = h('input.input', {
    type: 'password',
    placeholder: st.password_set ? '已配置（留空 = 不改）' : '未配置',
    autocomplete: 'new-password',
  });
  const from = h('input.input', { value: st.from || '', placeholder: '发件地址，例如 panel@example.com' });
  const to = h('input.input', { value: st.to || '', placeholder: '收件地址，多个用逗号分隔' });
  const subjectPrefix = h('input.input', { value: st.subject_prefix || '', placeholder: '[ZizPanel 备份] ' });
  const remoteDir = h('input.input', { value: st.remote_dir || '', placeholder: '例如 /backups/zizpanel（留空 = 登录后的默认目录）' });
  const maxFile = h('input.input', { type: 'number', min: '1', value: String(st.max_file_mb || 20) });
  const insecure = h('input', { type: 'checkbox' });
  insecure.checked = !!st.insecure_skip_verify;

  const smtpBox = h('div', [
    h('div.row', [
      field('发件人', from),
      field('收件人', to, '多个收件人用逗号分隔'),
    ]),
    field('主题前缀', subjectPrefix),
  ]);
  const ftpBox = h('div', [
    field('远程目录', remoteDir, '留空 = 登录后的默认目录'),
  ]);

  function syncEncryption() {
    const p = PROTOCOLS[protocol.value] || PROTOCOLS.smtp;
    const want = st.encryption && p.enc.some((o) => o.v === st.encryption) ? st.encryption : p.enc[0].v;
    clear(encryption);
    appendAll(encryption, p.enc.map((o) =>
      h('option', { value: o.v, text: o.t, selected: o.v === want })));
    smtpBox.hidden = protocol.value !== 'smtp';
    ftpBox.hidden = protocol.value === 'smtp';
  }
  protocol.addEventListener('change', () => {
    // 端口跟着协议走，但不覆盖用户自己填过的端口。
    const cur = Number(port.value) || 0;
    const p = PROTOCOLS[protocol.value];
    if (!cur || STD_PORTS.includes(cur)) {
      const def = p.port[p.enc[0].v];
      if (def) port.value = String(def);
    }
    st.encryption = '';
    syncEncryption();
  });
  encryption.addEventListener('change', () => {
    const p = PROTOCOLS[protocol.value];
    const cur = Number(port.value) || 0;
    if (!cur || STD_PORTS.includes(cur)) {
      const def = p.port[encryption.value];
      if (def) port.value = String(def);
    }
  });
  syncEncryption();

  const form = h('div', [
    h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '12px' } },
      [enabled, h('span', { text: '启用异地备份（备份完成后自动发送新增归档）' })]),
    h('div.row', [
      field('协议', protocol),
      field('主机', host),
      field('端口', port),
    ]),
    h('div.row', [
      field('加密', encryption),
      field('用户名', username),
      field('口令', password),
    ]),
    smtpBox,
    ftpBox,
    h('div.row', [
      field('单文件大小上限（MB）', maxFile, '超过上限的文件不会发送（建议改用 FTP）；绝不截断'),
      field('证书', h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } },
        [insecure, h('span', { text: '允许自签名证书' })])),
    ]),
  ]);

  // ---- 状态与账本（可单独刷新）----
  const statusBox = h('div');
  const ledgerBox = h('div');

  function payload() {
    return {
      enabled: enabled.checked,
      protocol: protocol.value,
      host: host.value.trim(),
      port: Number(port.value) || 0,
      encryption: encryption.value,
      username: username.value.trim(),
      password: password.value,
      from: from.value.trim(),
      to: to.value.trim(),
      subject_prefix: subjectPrefix.value,
      remote_dir: remoteDir.value.trim(),
      max_file_mb: Number(maxFile.value) || 0,
      insecure_skip_verify: insecure.checked,
    };
  }

  function renderStatus() {
    clear(statusBox);
    const ready = !!(data.settings && data.settings.ready);
    const n = Number(data.files_total) || 0;
    const pending = Number(data.pending) || 0;
    if (!ready) {
      appendAll(statusBox, h('div', {
        style: { padding: '8px 10px', border: '1px solid var(--warn, #f59e0b)', borderRadius: '6px', fontSize: '12.5px', marginBottom: '10px' },
        text: '还没配置异地备份：填好上面的服务器信息并启用后，「立即发送」才可用。',
      }));
    } else if (n === 0) {
      appendAll(statusBox, h('p.hint', { text: '备份目录里还没有归档（先去「已有备份」生成一份）。' }));
    } else {
      appendAll(statusBox, h('p.hint', {
        text: `备份目录共 ${n} 个归档，其中 ${pending} 个还没发送。`,
      }));
    }
    appendAll(statusBox, h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', marginBottom: '10px' } }, [
      h('button.btn.btn-primary.btn-sm', {
        text: '立即发送未发送的备份',
        title: '把账本里没有成功记录的归档逐个发出去（没有新增时会如实说"没有新增备份"）',
        onclick: () => send({}, '异地备份'),
      }),
      h('button.btn.btn-sm', {
        text: '全部重发',
        title: '忽略账本，把备份目录里的归档全部再发一遍',
        onclick: () => send({ force: true }, '异地备份（全部重发）'),
      }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: refresh }),
    ]));
  }

  function renderLedger() {
    clear(ledgerBox);
    const list = data.ledger || [];
    if (!list.length) {
      appendAll(ledgerBox, h('p.hint', { text: '还没有发送记录。' }));
      return;
    }
    appendAll(ledgerBox,
      h('div.section-title', { style: { marginTop: '12px' }, text: '已发送账本' }),
      h('div.zp-plan-scroll', [
        h('table.table', [
          h('thead', [h('tr', [
            h('th', { text: '文件' }), h('th', { text: '时间' }), h('th', { text: '大小' }),
            h('th', { text: '协议' }), h('th', { text: '结果' }), h('th', { text: '操作' }),
          ])]),
          h('tbody', list.map((e) => h('tr', [
            h('td', h('div.zp-plan-name.mono', { style: { fontSize: '11.5px' }, text: e.name, title: e.error || e.name })),
            h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: (e.sent_at || '').replace('T', ' ').slice(0, 19) }),
            h('td.num', { text: humanSize(e.bytes || e.size) }),
            h('td', { style: { fontSize: '11.5px' }, text: e.protocol || '' }),
            h('td', [statusPill(e)]),
            h('td', [
              h('button.btn.btn-sm', {
                text: '重发',
                title: e.status === 'sent' ? '这份已经发过，仍要再发一次' : '重新发送这一个文件',
                onclick: () => send({ name: e.name, force: true }, '异地备份（重发 ' + e.name + '）'),
              }),
            ]),
          ]))),
        ]),
      ]));
  }

  async function refresh() {
    try {
      data = await api.offsite();
      data.ledger = data.ledger || [];
    } catch (e) {
      toast('刷新失败：' + e.message, 'err', 9000);
      return;
    }
    renderStatus();
    renderLedger();
  }

  async function doSave(silent) {
    try {
      const r = await api.offsiteSave(payload());
      data.settings = r.settings;
      password.value = '';
      password.placeholder = r.settings.password_set ? '已配置（留空 = 不改）' : '未配置';
      if (!silent) toast('已保存异地备份配置', 'ok');
      renderStatus();
      if (onDone) onDone();
      return true;
    } catch (e) {
      toast('保存失败：' + e.message, 'err', 11000);
      return false;
    }
  }

  async function doTest() {
    // 先保存再测：保证测的就是存下来的那一份（否则会出现"测通了但存的是别的"）。
    if (!await doSave(true)) return;
    toast('正在测试连接…', 'info', 6000);
    try {
      const r = await api.offsiteTest();
      toast(r.msg || '测试连接成功', 'ok', 12000);
    } catch (e) {
      toast('测试连接失败：' + e.message, 'err', 14000);
    }
  }

  function send(opts, title) {
    taskCenter.start({
      kind: 'offsite_backup',
      target: 'offsite:send',
      title,
      start: () => api.offsiteSend(opts),
      onDone: (meta) => {
        if (meta && meta.status && meta.status !== 'succeeded') {
          toast('异地备份未全部成功：' + (meta.error || meta.status) + '（详见任务日志）', 'err', 14000);
        } else {
          toast('异地备份任务已结束', 'ok', 9000);
        }
        refresh();
        if (onDone) onDone();
      },
    });
  }

  const m = modal({
    title: '异地备份',
    wide: true,
    body: h('div', [
      h('p.hint', { text: '把新增的备份自动发到异地：SMTP 作为邮件附件，或 FTP / FTPS 上传。口令只存在面板配置里，不写日志。' }),
      form,
      h('div.section-title', { style: { marginTop: '16px' }, text: '发送' }),
      statusBox,
      ledgerBox,
    ]),
    footer: (close) => [
      h('button.btn', { text: '关闭', onclick: close }),
      h('button.btn', { text: '测试连接', onclick: doTest }),
      h('button.btn.btn-primary', { text: '保存', onclick: () => doSave(false) }),
    ],
  });

  renderStatus();
  renderLedger();
  void m;
}
