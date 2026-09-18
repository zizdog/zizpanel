// nginxpanel.js —— 「⚙️ 调整配置」（宝塔式的统一配置面板）。
//
// 为什么是"一个"面板：用户 2026-09-22 明确要求
//   「合并「⚙️ Nginx 管理」和「⚡ 上传大小 / 执行时间」2 个按钮到「配置文件」中，
//     合并后改名「调整配置」，重复的内容要整合」
// 于是原来三个各自独立、内容互相重复的弹窗收敛成这一个：
//   · nginx        —— 服务 / 性能调整 / 配置修改 / 错误日志（原 nginxpanel.js）
//   · 上传与执行上限 —— 原 views.js 的 renderLimitsInto（与「面板设置」同一份实现）
//   · 配置文件      —— 原 sites.js 的配置文件清单（调用方通过 opts.extra 注入）
//   · PHP 环境      —— 原 sites.js 的 PHP 多版本表格（同上，调用方注入）
//
// 关键是**只有一份实现**：
//   · `client_max_body_size` 只在「nginx → 性能调整」里可编辑；「上传与执行上限」页
//     只做只读展示 + 一颗直达按钮（绝不出现第二个可编辑控件）；
//   · 配置文件的编辑器复用 services.js 的 configFileModal（同一套文件接口与保存语义）；
//   · 注入（opts.extra）而不是反向 import sites.js，避免两个模块互相依赖。
//
// 设计立场（用户反复强调过）：
//   · **常用更改必须是功能**，不能只让用户去编辑配置原文件 —— 性能调整是表单；
//   · 表单的每一项都要说明"这是什么、现在多少、改了会怎样"（宝塔那套提示）；
//   · 保存后**回读生效值**：不是"已保存"，而是"这一项现在是 X / 被别处覆盖了"。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox } from './ui.js';
import { configFileModal } from './services.js';
// 「上传与执行上限」这份实现住在 views.js（面板设置里也用它）——直接复用，不复制第二份。
//
// 依赖方向是单向的：views.js 不 import 本文件（它需要"跳到性能调整"时由调用方注入
// 回调，见 renderLimitsInto 的 opts.openNginxTuning）。所以这里 import views.js 不会
// 形成 views.js ↔ nginxpanel.js 的双向依赖。
import { renderLimitsInto } from './views.js';

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

// nginx 这一组内部的四个页签（沿用原来「Nginx 管理」的四页）。
const NGINX_TABS = [
  { id: 'service', text: '服务' },
  { id: 'tuning', text: '性能调整' },
  { id: 'config', text: '配置修改' },
  { id: 'logs', text: '错误日志' },
];

// adjustConfigModal 打开「⚙️ 调整配置」。
//
// opts.page    初始页面：'service'|'tuning'|'config'|'logs'（nginx 组内）或
//              'limits'，或 opts.extra 里注册的 id。默认 'tuning'（最常用）。
// opts.paths   后端给的真实路径（前端一律不拼路径）：
//              { nginx_conf, site_error_log, brew_error_log }。缺省时按需向
//              /api/v1/sites 取一次；仍然读不到就显示"路径未知"，绝不猜。
// opts.extra   追加页面：{ id, label, desc, render(container) }。调用方（sites.js）
//              用它把「配置文件」「PHP 环境」挂进来 —— 这两块的数据在 SitesView 的
//              作用域里（站点缓存、卸载回调），不适合搬进本模块。
export function adjustConfigModal(opts = {}) {
  const extras = Array.isArray(opts.extra) ? opts.extra : [];
  const groups = [
    { id: 'nginx', text: 'nginx', desc: '服务 / 性能调整 / 配置修改 / 错误日志' },
    { id: 'limits', text: '上传与执行上限', desc: 'PHP 上传 / 执行上限（nginx 请求体上限在「nginx → 性能调整」改）' },
    ...extras.map((e) => ({ id: e.id, text: e.label, desc: e.desc || '' })),
  ];
  let paths = Object.assign({ nginx_conf: '', site_error_log: '', brew_error_log: '' }, opts.paths || {});

  const navBox = h('div', { style: { marginBottom: '14px' } });
  const body = h('div');
  const m = modal({ title: '⚙️ 调整配置', wide: true, body: h('div', [navBox, body]) });

  let group = 'nginx';
  let sub = 'tuning';
  const start = String(opts.page || '');
  if (NGINX_TABS.some((t) => t.id === start)) { group = 'nginx'; sub = start; }
  else if (groups.some((g) => g.id === start)) { group = start; }
  let tuning = null; // 上一次读到的参数（保存后刷新）

  // segRow 画一排页签（与宝塔一样：当前页高亮，点其它页即切换）。
  // 两级共用同一套渲染：外层是"哪一块"，内层只在 nginx 下出现的四个页签。
  function segRow(items, current, onPick, cls) {
    const row = h('div.zp-seg' + (cls ? '.' + cls : ''));
    for (const it of items) {
      row.append(h('button.zp-seg-btn' + (current === it.id ? '.active' : ''), {
        text: it.text,
        title: it.desc || it.text,
        'data-seg': it.id,
        onclick: () => onPick(it.id),
      }));
    }
    return row;
  }

  function drawNav() {
    clear(navBox);
    navBox.append(segRow(groups, group, (id) => {
      group = id;
      if (id === 'nginx' && !NGINX_TABS.some((t) => t.id === sub)) sub = 'tuning';
      drawNav();
      drawSafely();
    }, 'zp-seg-main'));
    if (group === 'nginx') {
      navBox.append(segRow(NGINX_TABS, sub, (id) => { sub = id; drawNav(); drawSafely(); }, 'zp-sub'));
    }
  }

  // jumpTo 切到指定页面（供页内按钮用：例如「上传与执行上限」页那颗"去改
  // client_max_body_size"，或「配置修改」页那颗"去配置文件清单"）。
  function jumpTo(id, subId) {
    if (id === 'nginx' && subId) sub = subId;
    group = id;
    drawNav();
    drawSafely();
  }

  // target 是**当前这一页**自己的容器。
  //
  // 为什么每次 draw 都新建一个容器（而不是各页共用 body）：这些页都是"先画读取中，
  // await 后再填内容"，共用容器时若用户中途切页，晚到的结果会写进**新的那一页**
  //（表现成"配置文件页里冒出 PHP 版本表"）。独立容器让过期结果只写进已经不在 DOM 里
  // 的那个节点，天然失效。
  let target = body;

  // draw() 是异步的（每页都要 await 后端），而所有调用点都是事件处理器 ——
  // 必须把 rejection 变成用户看得见的错误：async 事件处理器里抛出的异常不会显示任何东西
  //（本项目真实踩过：点按钮"没反应、没成功也没提示"）。
  function drawSafely() {
    Promise.resolve(draw()).catch((e) => {
      clear(target);
      target.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '这一页渲染失败' }),
        h('p', { text: (e && e.message) || String(e) }),
      ]));
    });
  }

  async function draw() {
    clear(body);
    target = h('div');
    body.append(target);
    if (group === 'nginx') {
      if (sub === 'service') return drawService();
      if (sub === 'config') return drawConfig();
      if (sub === 'logs') return drawLogs();
      return drawTuning();
    }
    if (group === 'limits') {
      await renderLimitsInto(target, () => drawSafely(),
        { openNginxTuning: () => jumpTo('nginx', 'tuning') });
      return;
    }
    const ex = extras.find((e) => e.id === group);
    if (!ex) {
      target.append(h('div.empty', [
        h('div.big', { text: '❓' }),
        h('p', { text: '这一页不存在（界面与面板版本不一致？刷新页面即可）' }),
      ]));
      return;
    }
    return ex.render(target);
  }

  // ---------------- 性能调整（宝塔那张表） ----------------
  async function drawTuning() {
    let res;
    try {
      res = tuning || await api.nginxTuning();
    } catch (e) {
      clear(target);
      target.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取 nginx 参数失败' }),
        h('p', { text: (e && e.message) || String(e) }),
      ]));
      return;
    }
    tuning = res;
    const vals = res.values || {};
    const found = res.found || {};
    clear(target);

    if (res.helper_unavailable) {
      // 提权助手不可用时**不许**把出厂默认值当现状展示（否则用户照着保存
      // 会把真实配置冲掉）。这里直接说清原因。
      target.append(h('div', { style: { padding: '10px 12px', background: 'var(--warn-soft)',
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
        drawNav();
        drawSafely();
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

    target.append(
      h('div.hint', { text: '文件：' + (res.file || '') + (res.version ? '（' + res.version + '）' : '') }),
      h('table.table', [h('tbody', rows)]),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '10px', flexWrap: 'wrap' } }, [
        save,
        h('button.btn.btn-sm', {
          text: '↻ 重新读取', onclick: async () => { tuning = null; drawSafely(); },
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
    clear(target);
    if (err) {
      target.append(h('div.empty', [h('p', { text: '读取 nginx 状态失败：' + err })]));
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
          drawSafely();
        } catch (e) { toast(label + '失败：' + ((e && e.message) || e), 'err', 12000); }
      },
    });
    target.append(h('div', { style: { lineHeight: '2' } }, [
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
    clear(target);
    // nginx.conf 的路径**由后端给**（config_files 里那一项），前端不拼 /opt/homebrew ——
    // Intel 的 Homebrew 在 /usr/local，写死就是"点开报文件不存在"。
    if (!paths.nginx_conf) {
      target.append(h('div.hint', { text: '读取中…' }));
      await loadPaths();
      clear(target);
    }
    const path = paths.nginx_conf;
    target.append(h('div', { style: { lineHeight: '1.9' } }, [
      h('div', { text: '直接编辑 nginx.conf 原文（低频、危险操作；常用参数请用「性能调整」）' }),
      path
        ? h('div.hint', { text: '文件：' + path + '。保存后会校验语法；请在改完点「服务 → 重载」让它生效。' })
        : h('div.hint', { text: '路径未知：面板没有从后端拿到 nginx.conf 的真实路径（既没有站点数据、'
          + '后端也没给出配置文件清单）。可以到「配置文件」页看后端给出的清单，或用命令行编辑。' }),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '10px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm.btn-primary', {
          text: '📝 打开配置文件',
          title: path ? '打开 ' + path : '面板不知道 nginx.conf 在哪，点开只会如实说明原因',
          onclick: () => configFileModal({ config_path: path, display_name: 'nginx.conf' }),
        }),
        h('button.btn.btn-sm', {
          text: '⚙️ 各站点 vhost / PHP 配置',
          title: '切到本弹窗的「配置文件」页：各站点 vhost、每个 PHP 版本的 php.ini / www.conf、my.cnf…',
          onclick: () => {
            // 配置文件清单就在同一个弹窗里（原来这里要关窗再让用户去工具条找，已经合并）。
            if (extras.some((e) => e.id === 'files')) jumpTo('files');
            else toast('这一处没有挂「配置文件」页：请到「网站管理 → ⚙️ 调整配置 → 配置文件」里选文件', 'info', 8000);
          },
        }),
      ]),
    ]));
  }

  // ---------------- 错误日志 ----------------
  //
  // 路径**全部来自后端**（api_sites.go 的 nginx_error_log / nginx_brew_error_log，
  // 由 Cfg.LogRoot 与 Cfg.BrewPrefix 推导并核对文件真的存在）。
  // 这里以前写着 /Users/zizdog/www/_logs/nginx-error.log 与 /opt/homebrew/...，
  // 换一台机器（另一个用户名 / Intel 前缀）两行就都是错的 —— 用户 2026-09-22 报障。
  async function drawLogs() {
    clear(target);
    if (!paths.site_error_log && !paths.brew_error_log) {
      target.append(h('div.hint', { text: '读取中…' }));
      await loadPaths();
      clear(target);
    }
    const mono = (p) => h('li.mono', { style: { fontSize: '12px', wordBreak: 'break-all' }, text: p });
    target.append(h('div', { style: { lineHeight: '1.9' } }, [
      h('div', { text: 'nginx 的错误日志有两个常见位置，具体以 nginx.conf 里的 error_log 指令为准：' }),
      h('ul', { style: { margin: '6px 0 0 18px' } }, [
        paths.site_error_log
          ? mono(paths.site_error_log + '（面板的站点日志目录，默认站点与站点 vhost 写在这里）')
          : h('li', { style: { fontSize: '12.5px', color: 'var(--warn)' }, text: '路径未知：站点日志目录里读不到 nginx-error.log' }),
        paths.brew_error_log
          ? mono(paths.brew_error_log + '（Homebrew nginx 的出厂位置）')
          : h('li', { style: { fontSize: '12.5px', color: 'var(--warn)' }, text: '路径未知：Homebrew 的 var/log/nginx/error.log 读不到' }),
      ]),
      h('div.hint', { style: { marginTop: '8px' },
        text: '面板不猜路径：上面两条是后端核对过**文件真的存在**才显示的；读不到就写"路径未知"，'
          + '你可以按 nginx.conf 里的 error_log 去找。' }),
      h('div.hint', { style: { marginTop: '8px' },
        text: '面板的「日志中心 → nginx」里有实时日志流（带搜索与下载）。这里只做指引，不复制第二套日志查看器。' }),
      h('div', { style: { marginTop: '10px' } }, [
        h('button.btn.btn-sm', {
          text: '📜 去日志中心', onclick: () => { m.close(); location.hash = '#/logs'; },
        }),
      ]),
    ]));
  }

  // loadPaths 在后端字段缺失时按需取一次 /api/v1/sites（老版本面板 / 调用方没传 paths）。
  // 仍然取不到就保持空串 —— 界面显示"路径未知"，绝不猜。
  async function loadPaths() {
    try {
      const r = await api.sites();
      paths = {
        nginx_conf: paths.nginx_conf || nginxConfFrom(r),
        site_error_log: paths.site_error_log || r.nginx_error_log || '',
        brew_error_log: paths.brew_error_log || r.nginx_brew_error_log || '',
      };
    } catch (e) { /* 读不到就保持"路径未知" */ }
  }

  drawNav();
  drawSafely();
  return m;
}

// nginxConfFrom 从后端给的配置文件清单里找 nginx 主配置的真实路径。
// 找不到就返回空串（界面显示"路径未知"），不拿 brew 前缀去猜。
function nginxConfFrom(sitesRes) {
  const files = (sitesRes && sitesRes.config_files) || [];
  const hit = files.find((f) => f && f.service === 'nginx' && /nginx\.conf$/.test(String(f.path || '')));
  return (hit && hit.path) || '';
}
