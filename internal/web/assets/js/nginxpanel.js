// nginxpanel.js —— 宝塔式的「Nginx 管理」面板（用户 2026-09-18：参考宝塔的样子）。
//
// 为什么单独一个模块：这块 UI 有四个页签（服务 / 配置修改 / 性能调整 / 错误日志），
// 塞进 sites.js 会让那个文件更难读；而它的三处入口（网站管理工具条、nginx 应用
// 管理面板、以及需要时从提示点进来）只需要 import 一个函数。
//
// 设计立场（用户反复强调过）：
//   · **常用更改必须是功能**，不能只让用户去编辑配置原文件 —— 性能调整是表单；
//   · 表单的每一项都要说明"这是什么、现在多少、改了会怎样"（宝塔那套提示）；
//   · 保存后**回读生效值**：不是"已保存"，而是"这一项现在是 X / 被别处覆盖了"。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox } from './ui.js';
import { configFileModal } from './services.js';

// TUNING_FIELDS 是表单字段表（顺序与宝塔那张表一致）。
//
// unit 只用于展示；真正的单位换算在后端（priv/nginxtuning.go 的 spec.Format/Parse）——
// 前端**不**做换算，避免两边各算一次、算岔了。
const TUNING_FIELDS = [
  {
    key: 'worker_processes', label: 'worker_processes', kind: 'text',
    hint: '处理进程，auto 表示自动，数字表示进程数',
  },
  {
    key: 'worker_connections', label: 'worker_connections', kind: 'number', min: 128, max: 65535,
    hint: '最大并发连接数',
  },
  {
    key: 'keepalive_timeout', label: 'keepalive_timeout', kind: 'number', min: 1, max: 3600,
    hint: '连接超时时间（秒）',
  },
  {
    key: 'gzip', label: 'gzip', kind: 'select',
    options: [{ value: 'on', text: '开启' }, { value: 'off', text: '关闭' }],
    hint: '是否开启压缩传输',
  },
  {
    key: 'gzip_min_length_kb', label: 'gzip_min_length', kind: 'number', min: 0, max: 1048576,
    hint: 'KB，最小压缩文件',
  },
  {
    key: 'gzip_comp_level', label: 'gzip_comp_level', kind: 'number', min: 1, max: 9,
    hint: '压缩率（1-9，越大越费 CPU）',
  },
  {
    key: 'client_max_body_size_mb', label: 'client_max_body_size', kind: 'number', min: 1, max: 102400,
    hint: 'MB，最大上传文件',
  },
  {
    key: 'server_names_hash_bucket_size', label: 'server_names_hash_bucket_size', kind: 'number',
    min: 32, max: 1024, hint: '服务器名字的 hash 表大小',
  },
  {
    key: 'client_header_buffer_size_kb', label: 'client_header_buffer_size', kind: 'number',
    min: 1, max: 1024, hint: 'KB，客户端请求头 buffer 大小',
  },
  {
    key: 'client_body_buffer_size_kb', label: 'client_body_buffer_size', kind: 'number',
    min: 8, max: 4096, hint: 'KB，请求主体缓冲区',
  },
];

// nginxPanelModal 打开「Nginx 管理」。
export function nginxPanelModal() {
  const body = h('div');
  const tabsBox = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '12px', flexWrap: 'wrap' } });
  const m = modal({ title: '⚙️ Nginx 管理', wide: true, body: h('div', [tabsBox, body]) });

  let tab = 'tuning';
  let tuning = null; // 上一次读到的参数（保存后刷新）

  function drawTabs() {
    clear(tabsBox);
    const items = [
      { id: 'service', text: '服务' },
      { id: 'config', text: '配置修改' },
      { id: 'tuning', text: '性能调整' },
      { id: 'logs', text: '错误日志' },
    ];
    for (const it of items) {
      tabsBox.append(h('button.btn.btn-sm' + (tab === it.id ? '.btn-primary' : ''), {
        text: it.text,
        onclick: () => { tab = it.id; drawTabs(); draw(); },
      }));
    }
  }

  async function draw() {
    clear(body);
    body.append(h('div.hint', { text: '读取中…' }));
    if (tab === 'tuning') return drawTuning();
    if (tab === 'service') return drawService();
    if (tab === 'config') return drawConfig();
    return drawLogs();
  }

  // ---------------- 性能调整（宝塔那张表） ----------------
  async function drawTuning() {
    let res;
    try {
      res = tuning || await api.nginxTuning();
    } catch (e) {
      clear(body);
      body.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取 nginx 参数失败' }),
        h('p', { text: (e && e.message) || String(e) }),
      ]));
      return;
    }
    tuning = res;
    const vals = res.values || {};
    const found = res.found || {};
    clear(body);

    if (res.helper_unavailable) {
      // 提权助手不可用时**不许**把出厂默认值当现状展示（否则用户照着保存
      // 会把真实配置冲掉）。这里直接说清原因。
      body.append(h('div', { style: { padding: '10px 12px', background: 'var(--warn-soft)',
        borderRadius: '6px', lineHeight: '1.8' } }, [
        h('div', { style: { fontWeight: '620' }, text: '读不到 nginx 的真实配置' }),
        h('div', { text: res.error || '提权助手不可用' }),
      ]));
      return;
    }

    const inputs = new Map();
    const rows = TUNING_FIELDS.map((f) => {
      let input;
      if (f.kind === 'select') {
        input = h('select.select', (f.options || []).map((o) => h('option', {
          value: o.value, text: o.text,
          selected: (f.key === 'gzip' ? (vals[f.key] ? 'on' : 'off') : vals[f.key]) === o.value,
        })));
      } else {
        input = h('input.input', {
          type: f.kind === 'number' ? 'number' : 'text',
          value: vals[f.key] ?? '',
          min: f.min, max: f.max,
          style: { maxWidth: '160px' },
        });
      }
      inputs.set(f.key, input);
      const notes = [];
      if (found[f.key] === false) {
        notes.push('nginx.conf 里没有这一行，显示的是 nginx 出厂默认值；保存后会写入。');
      }
      return h('tr', [
        h('td', { style: { padding: '8px 10px', whiteSpace: 'nowrap', fontWeight: '560' }, text: f.label }),
        h('td', { style: { padding: '8px 10px' } }, [input]),
        h('td', { style: { padding: '8px 10px', fontSize: '12px', color: 'var(--text-dim)', lineHeight: '1.6' } },
          [h('div', { text: f.hint }), ...notes.map((t) => h('div', { style: { color: 'var(--warn)' }, text: t }))]),
      ]);
    });

    const save = h('button.btn.btn-primary', { text: '保存' });
    save.addEventListener('click', async () => {
      const payload = {};
      for (const f of TUNING_FIELDS) {
        const el = inputs.get(f.key);
        payload[f.key] = f.key === 'gzip' ? el.value === 'on'
          : (f.kind === 'number' ? Number(el.value) : String(el.value).trim());
      }
      if (payload.client_max_body_size_mb < 1) {
        toast('最大上传文件至少 1 MB', 'warn');
        return;
      }
      save.disabled = true;
      save.textContent = '保存中…';
      try {
        const out = await api.nginxTuningSave(payload);
        tuning = out;
        toast('已保存并重载 nginx；下面是回读到的**真实生效值**', 'ok', 9000);
        drawTabs();
        draw();
      } catch (e) {
        // 后端在 nginx -t 失败时会自动回滚，并把原因带回来 —— 原样显示。
        toast('保存失败：' + ((e && e.message) || e), 'err', 16000);
      } finally {
        save.disabled = false;
        save.textContent = '保存';
      }
    });

    // 生效值回读表：只有**真实加载**（nginx -T）里读到的值才敢叫"生效"
    const effRows = TUNING_FIELDS.map((f) => h('tr', [
      h('td.mono', { style: { fontSize: '12px' }, text: f.key }),
      h('td.mono', { style: { fontSize: '12px' } }, [h('div', { text: (res.effective || {})[f.key] || '（未读到）' })]),
    ]));

    body.append(
      h('div.hint', { text: '文件：' + (res.file || '') + (res.version ? '（' + res.version + '）' : '') }),
      h('table.table', [h('tbody', rows)]),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '10px', flexWrap: 'wrap' } }, [
        save,
        h('button.btn.btn-sm', {
          text: '↻ 重新读取', onclick: async () => { tuning = null; draw(); },
        }),
        h('span.hint', { text: '保存会先备份 nginx.conf（.zizpanel.bak），再写盘 → nginx -t → 重载；校验不过会自动回滚' }),
      ]),
      (res.conflicts || []).length
        ? h('div', { style: { marginTop: '12px', padding: '10px 12px', background: 'var(--warn-soft)',
          borderRadius: '6px', lineHeight: '1.8', fontSize: '12.5px' } }, [
          h('div', { style: { fontWeight: '620' }, text: '⚠️ 有几项的真实生效值与 nginx.conf 里写的不一致' }),
          ...(res.conflicts || []).map((c) => h('div', { text: '· ' + c })),
        ])
        : null,
      h('div.card', [
        h('div.card-head', [h('h3', { text: '真实生效值（nginx -T 回读）' })]),
        h('div.card-body.tight', [h('table.table', [h('tbody', effRows)])]),
      ]),
      res.test_output
        ? h('div.hint', { style: { whiteSpace: 'pre-wrap', marginTop: '6px' }, text: 'nginx -t：' + res.test_output })
        : null,
    );
  }

  // ---------------- 服务 ----------------
  async function drawService() {
    let st = null, err = '';
    try { st = await api.nginxStatus(); } catch (e) { err = (e && e.message) || String(e); }
    clear(body);
    if (err) {
      body.append(h('div.empty', [h('p', { text: '读取 nginx 状态失败：' + err })]));
      return;
    }
    const running = (st && st.status) === 'running';
    const act = (what, label) => h('button.btn.btn-sm', {
      text: label,
      onclick: async () => {
        if (what === 'stop' && !await confirmBox('停止 nginx 会让这个机器上所有站点立刻无法访问。\n\n继续？',
          { title: '停止 nginx', okText: '停止' })) return;
        try {
          await api.serviceAction('nginx', what === 'reload' ? 'reload' : what);
          toast('已执行：' + label, 'ok');
          draw();
        } catch (e) { toast(label + '失败：' + ((e && e.message) || e), 'err', 12000); }
      },
    });
    body.append(h('div', { style: { lineHeight: '2' } }, [
      h('div', [h('span', { text: '运行状态：' }), running
        ? h('span.pill.ok', { text: '运行中' })
        : h('span.pill.warn', { text: '未运行' })]),
      h('div', { text: '站点：共 ' + (st.total || 0) + ' 个，启用 ' + (st.enabled || 0) + ' 个' }),
      h('div.hint', { text: '面板里没有 nginx 的服务记录时，"重载/停止"会失败 —— ' +
        '可以在「应用 → 已安装」里对 Nginx 点「接入」，或直接用下面的「配置修改」。' }),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '10px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm', {
          text: '🧪 校验配置', onclick: async () => {
            try {
              const r = await api.nginxTest();
              toast(r.ok ? 'nginx 配置校验通过' : '配置有问题：' + r.output, r.ok ? 'ok' : 'err', r.ok ? 4000 : 14000);
            } catch (e) { toast('校验失败：' + ((e && e.message) || e), 'err'); }
          },
        }),
        act('start', '▶ 启动'),
        act('reload', '🔄 重载'),
        act('stop', '⏹ 停止'),
      ]),
    ]));
  }

  // ---------------- 配置修改 ----------------
  async function drawConfig() {
    clear(body);
    const path = '/opt/homebrew/etc/nginx/nginx.conf';
    body.append(h('div', { style: { lineHeight: '1.9' } }, [
      h('div', { text: '直接编辑 nginx.conf 原文（低频、危险操作；常用参数请用「性能调整」）' }),
      h('div.hint', { text: '文件：' + path + '。保存后会校验语法；请在改完点「服务 → 重载」让它生效。' }),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '10px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm.btn-primary', {
          text: '📝 打开配置文件',
          onclick: () => configFileModal({ config_path: path, display_name: 'nginx.conf' }),
        }),
        h('button.btn.btn-sm', {
          text: '⚙️ 各站点 vhost / PHP 配置',
          title: '到「网站管理 → ⚙️ 配置文件」里选具体文件（各站点 vhost、php.ini、my.cnf…）',
          onclick: () => {
            m.close();
            toast('请在「网站管理 → ⚙️ 配置文件」里选择要编辑的文件', 'info', 8000);
          },
        }),
      ]),
    ]));
  }

  // ---------------- 错误日志 ----------------
  function drawLogs() {
    clear(body);
    const errLog = '/opt/homebrew/var/log/nginx/error.log';
    const siteErr = '/Users/zizdog/www/_logs/nginx-error.log';
    body.append(h('div', { style: { lineHeight: '1.9' } }, [
      h('div', { text: 'nginx 的错误日志通常在这两个位置（这台机器的配置用的是后者）：' }),
      h('ul', { style: { margin: '6px 0 0 18px' } }, [
        h('li.mono', { style: { fontSize: '12px' }, text: siteErr + '（nginx.conf 里 error_log 指向的）' }),
        h('li.mono', { style: { fontSize: '12px' }, text: errLog + '（Homebrew 默认位置）' }),
      ]),
      h('div.hint', { style: { marginTop: '8px' },
        text: '面板的「日志中心 → nginx」里有实时日志流（带搜索与下载）。这里只做指引，不复制第二套日志查看器。' }),
      h('div', { style: { marginTop: '10px' } }, [
        h('button.btn.btn-sm', {
          text: '📜 去日志中心', onclick: () => { m.close(); location.hash = '#/logs'; },
        }),
      ]),
    ]));
  }

  drawTabs();
  draw();
  return m;
}
