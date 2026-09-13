// database.js —— 数据库管理页面。
//
// 结构：库 / 账号 / SQL 执行 / 备份 四个 Tab。
//
// 安全相关的前端约束（后端同样会校验，前端只是提前拦住明显的错误）：
//   - 库名、用户名、主机名的输入框都带格式提示
//   - 删库要求输入库名确认（防手滑点到隔壁库）
//   - 系统库与 root 账号标注出来，操作按钮对应禁用/提示
//   - SQL 执行区明确说明"只允许单条语句、不允许注释"

import { api, apiURL } from './api.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';

let cache = null;
let tab = 'databases';

const PRIVILEGES = [
  'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'INDEX',
  'CREATE TEMPORARY TABLES', 'LOCK TABLES', 'EXECUTE', 'CREATE VIEW', 'SHOW VIEW',
  'CREATE ROUTINE', 'ALTER ROUTINE', 'EVENT', 'TRIGGER', 'REFERENCES',
];

export function DatabaseView(content, ctx = {}) {
  clear(content);
  tab = 'databases';

  const head = h('div.card-head', [
    h('h3', { text: '数据库管理' }),
    h('div.spacer'),
    h('div', { id: 'db-head', style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);
  const tabBar = h('div', { style: { display: 'flex', gap: '6px', padding: '10px 14px', borderBottom: '1px solid var(--border-soft)', flexWrap: 'wrap' } });
  const body = h('div');

  content.append(h('div.card', [head, tabBar, h('div.card-body', [body])]));
  const headBox = head.querySelector('#db-head');

  async function load() {
    clear(body);
    body.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在连接数据库…' })]));
    try {
      cache = await api.database();
    } catch (e) {
      clear(body);
      // 连不上时**必须给出可操作的东西**，而不是只报一句"无法连接"。
      // 面板早先只认某个项目约定文件里的密码，换台机器就永远连不上，
      // 用户完全不知道该改哪里。这里直接让他填，填完就能连。
      body.append(credentialsForm(e.message));
      return;
    }
    renderHead();
    renderTabs();
    renderBody();
  }

  // pmaURL 返回 phpMyAdmin 入口地址。
  //
  // **必须用绝对地址指向 nginx 的端口（默认 80），不能用相对路径。**
  // 踩过的坑：写成相对路径 '/phpmyadmin/' 时，浏览器会解析到面板自己的
  // 地址（https://host:8443/phpmyadmin/）—— 那是面板的 SPA，
  // 它不认识这个路径，于是卡在"正在连接面板…"永远出不来。
  // phpMyAdmin 是 nginx 上的一个 location，跟面板不是同一个服务。
  function pmaURL() {
    const host = location.hostname || '127.0.0.1';
    // nginx 若不在 80 端口，可用 ?pma_port=8080 覆盖
    const q = new URLSearchParams(location.search).get('pma_port');
    const port = q || '80';
    const suffix = port === '80' ? '' : ':' + port;
    return `http://${host}${suffix}/phpmyadmin/`;
  }

  // credentialsForm 连接失败时显示的凭据表单。
  //
  // 为什么把"密码"也回显：面板是登录后才能访问的后台，用户需要看到
  // 自己填过什么才能改。真正的保护在登录鉴权，不在这里。
  function credentialsForm(msg) {
    const cfg = state.session?.config || {};
    const host = h('input.input', { value: cfg.mysql_host || '127.0.0.1' });
    const port = h('input.input', { value: String(cfg.mysql_port || 3306) });
    const socket = h('input.input', { value: cfg.mysql_socket || '/tmp/mysql.sock' });
    const user = h('input.input', { value: cfg.mysql_user || 'root' });
    const pass = h('input.input', { type: 'password', value: cfg.mysql_password || '' });
    const save = h('button.btn.btn-primary', {
      text: '保存并重试',
      onclick: async () => {
        save.disabled = true;
        save.textContent = '保存中…';
        try {
          await api.saveSettings({
            mysql_host: host.value.trim(),
            mysql_port: parseInt(port.value, 10) || 3306,
            mysql_socket: socket.value.trim(),
            mysql_user: user.value.trim(),
            mysql_password: pass.value,
          });
          toast('已保存，正在重试连接…', 'ok');
          await load();
        } catch (err) {
          toast('保存失败：' + err.message, 'err', 9000);
        } finally {
          save.disabled = false;
          save.textContent = '保存并重试';
        }
      },
    });
    return h('div', { style: { maxWidth: '560px', margin: '0 auto' } }, [
      h('div.empty', [
        h('div.big', { text: '🔌' }),
        h('h4', { text: '无法连接 MySQL' }),
        h('p', { style: { color: 'var(--text-mute)', fontSize: '12px' }, text: msg }),
        h('p', {
          style: { fontSize: '12.5px', lineHeight: '1.7', textAlign: 'left', marginTop: '10px' },
          text: '面板需要一组能连上 MySQL 的账号密码。brew 全新安装的 MySQL ' +
                'root 是空密码（直接点保存即可）；如果你之前给 root 设过密码，' +
                '请在下面填写。',
        }),
      ]),
      h('div.field', [h('label', { text: '主机' }), host]),
      h('div.field', [h('label', { text: '端口' }), port]),
      h('div.field', [h('label', { text: 'Socket' }), socket,
        h('div.hint', { text: 'TCP 连不上时会用 socket，一般不用改' })]),
      h('div.field', [h('label', { text: '用户名' }), user]),
      h('div.field', [h('label', { text: '密码' }), pass,
        h('div.hint', { text: '空密码就留空' })]),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '12px' } }, [
        save,
        h('button.btn', { text: '⟳ 仅重试', onclick: load }),
      ]),
    ]);
  }

  function renderHead() {
    clear(headBox);
    // 主入口是 phpMyAdmin：库表、权限、导入导出这些它比面板做得全。
    // 面板自研那几个 Tab 保留为**应急入口**（phpMyAdmin 起不来时还能改密码）。
    const pmaBtn = h('button.btn.btn-sm.btn-primary', {
      text: '↗ 打开 phpMyAdmin',
      title: '库表与权限管理的推荐入口；面板内置工具作为应急备用',
      onclick: () => window.open(pmaURL(), '_blank', 'noopener'),
    });
    if (!cache?.connected) {
      appendAll(headBox,
        h('span.pill.danger', { text: '未连接' }),
        h('button.btn.btn-sm', { text: '⟳ 重试', onclick: load }),
        pmaBtn,
      );
      return;
    }
    appendAll(headBox,
      pmaBtn,
      h('span.pill.ok', { text: 'MySQL ' + (cache.version || '') }),
      h('span.pill', { text: `${(cache.databases || []).length} 个库` }),
      h('span.pill', { text: `${(cache.users || []).length} 个账号` }),
      cache.has_password ? null : h('span.pill.warn', {
        text: '未读到 root 密码',
        title: '面板从 ~/www/.env.local 读取数据库密码，未读到可能无法执行管理操作',
      }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
    );
  }

  function renderTabs() {
    clear(tabBar);
    const tabs = [
      { id: 'databases', title: '数据库' },
      { id: 'users', title: '账号与权限' },
      { id: 'sql', title: 'SQL 执行' },
      { id: 'backup', title: '导入导出' },
    ];
    for (const t of tabs) {
      appendAll(tabBar, h(`button.btn.btn-sm${tab === t.id ? '.btn-primary' : ''}`, {
        text: t.title,
        onclick: () => { tab = t.id; renderTabs(); renderBody(); },
      }));
    }
  }

  function renderBody() {
    clear(body);
    if (!cache?.connected) {
      appendAll(body, h('div.empty', [
        h('div.big', { text: '🔌' }),
        h('h4', { text: '无法连接 MySQL' }),
        h('p', { text: cache?.error || '未知错误' }),
        cache?.hint ? h('div', {
          style: { marginTop: '12px', padding: '10px 12px', background: 'var(--panel-2)', borderRadius: '6px', fontSize: '12.5px', maxWidth: '560px', textAlign: 'left' },
          text: cache.hint,
        }) : null,
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center' } }, [
          h('button.btn', { text: '去服务管理启动 MySQL', onclick: () => { location.hash = '#/services'; } }),
          h('button.btn.btn-primary', { text: '重试连接', onclick: load }),
        ]),
      ]));
      return;
    }
    if (tab === 'databases') renderDatabases();
    else if (tab === 'users') renderUsers();
    else if (tab === 'sql') renderSQL();
    else renderBackup();
  }

  // ---------- 数据库 ----------
  function renderDatabases() {
    const list = cache.databases || [];
    appendAll(body,
      h('div', { style: { display: 'flex', gap: '8px', marginBottom: '12px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-primary.btn-sm', { text: '+ 新建数据库', onclick: createDBModal }),
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      ]),
      list.length
        ? h('div', { style: { overflowX: 'auto' } }, [
          h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '数据库' }), h('th', { text: '表数量' }), h('th', { text: '大小' }),
              h('th', { text: '字符集' }), h('th', { text: '操作' }),
            ])]),
            h('tbody', list.map((d) => h('tr', [
              h('td', [
                h('span', { style: { fontWeight: '550' }, text: d.name }),
                d.system ? h('span.pill.warn', { style: { marginLeft: '6px' }, text: '系统库' }) : null,
              ]),
              h('td.num', { text: d.tables }),
              h('td.num', { text: humanSize(d.size) }),
              h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: d.charset + (d.collation ? ' · ' + d.collation : '') }),
              h('td', [
                h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, [
                  h('button.btn.btn-sm', {
                    text: '查看表',
                    onclick: () => showTables(d.name),
                  }),
                  h('button.btn.btn-sm', {
                    text: '导出',
                    title: '导出为 .sql 文件',
                    onclick: () => dumpDB(d.name),
                  }),
                  h('button.btn.btn-sm', {
                    text: 'SQL',
                    title: '在该库上执行 SQL',
                    onclick: () => { tab = 'sql'; renderTabs(); renderBody(); setTimeout(() => {
                      const sel = body.querySelector('select[data-role="sql-db"]');
                      if (sel) sel.value = d.name;
                    }, 100); },
                  }),
                  d.system ? h('button.btn.btn-sm', {
                    text: '不可删除',
                    disabled: true,
                    title: '系统库不允许删除',
                  }) : h('button.btn.btn-danger.btn-sm', {
                    text: '删除',
                    onclick: () => dropDB(d.name),
                  }),
                ]),
              ]),
            ]))),
          ]),
        ])
        : h('div.empty', [h('p', { text: '没有数据库' })]),
    );
  }

  function createDBModal() {
    const name = h('input.input', { placeholder: '例如 myapp' });
    const charset = h('select.select', [
      h('option', { value: 'utf8mb4', text: 'utf8mb4（推荐，支持 emoji 与全部 Unicode）' }),
      h('option', { value: 'utf8', text: 'utf8' }),
      h('option', { value: 'latin1', text: 'latin1' }),
      h('option', { value: 'gbk', text: 'gbk' }),
    ]);
    const m = modal({
      title: '新建数据库',
      body: h('div', [
        h('div.field', [h('label', { text: '数据库名 *' }), name,
          h('div.hint', { text: '只允许字母、数字、下划线与 $，最多 64 字符' })]),
        h('div.field', [h('label', { text: '字符集' }), charset]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '创建',
          onclick: async () => {
            try {
              const r = await api.databaseCreate({ name: name.value.trim(), charset: charset.value });
              toast(r.msg, 'ok');
              close(); load();
            } catch (e) { toast(e.message, 'err', 10000); }
          },
        }),
      ],
    });
    setTimeout(() => name.focus(), 60);
  }

  async function dropDB(name) {
    // 要求输入库名确认：删库不可恢复，仅靠点"确定"太容易手滑
    const typed = await promptBox({
      title: '删除数据库 ' + name,
      label: `请输入数据库名「${name}」以确认删除`,
      placeholder: name,
      hint: '这会永久删除该库及其全部数据，无法恢复。建议先「导出」一份备份。',
    });
    if (typed !== name) {
      if (typed !== null) toast('输入的库名不匹配，已取消删除', 'warn');
      return;
    }
    try {
      const r = await api.databaseDrop(name);
      toast(r.msg, 'ok');
      load();
    } catch (e) { toast(e.message, 'err', 10000); }
  }

  async function showTables(name) {
    const box = h('div', [h('div.empty', [h('p', { text: '读取中…' })])]);
    const m = modal({ title: `表：${name}`, wide: true, body: box });
    try {
      const r = await api.databaseTables(name);
      clear(box);
      const tables = r.tables || [];
      if (!tables.length) {
        appendAll(box, h('div.empty', [h('p', { text: '这个库里还没有表' })]));
        return;
      }
      appendAll(box, h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '表名' }), h('th', { text: '引擎' }), h('th', { text: '行数（估算）' }),
          h('th', { text: '大小' }), h('th', { text: '排序规则' }), h('th', { text: '更新时间' }),
        ])]),
        h('tbody', tables.map((t) => h('tr', [
          h('td', [
            h('span', { style: { fontWeight: '550' }, text: t.name }),
            t.comment ? h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: t.comment }) : null,
          ]),
          h('td', { text: t.engine || '—' }),
          h('td.num', { text: String(t.rows) }),
          h('td.num', { text: humanSize(t.size) }),
          h('td', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: t.collation || '—' }),
          h('td', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: t.updated_at || '—' }),
        ]))),
      ]));
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [h('p', { text: '读取失败：' + e.message })]));
    }
  }

  // ---------- 账号 ----------
  function renderUsers() {
    const users = cache.users || [];
    const dbNames = (cache.databases || []).map((d) => d.name);
    appendAll(body,
      h('div', { style: { display: 'flex', gap: '8px', marginBottom: '12px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-primary.btn-sm', { text: '+ 新建账号', onclick: () => createUserModal(dbNames) }),
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      ]),
      users.length
        ? h('div', { style: { overflowX: 'auto' } }, [
          h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '账号' }), h('th', { text: '可访问的库' }), h('th', { text: '状态' }), h('th', { text: '操作' }),
            ])]),
            h('tbody', users.map((u) => h('tr', [
              h('td', [
                h('span', { style: { fontWeight: '550' }, text: u.user }),
                h('span', { style: { color: 'var(--text-mute)' }, text: '@' + u.host }),
                u.system ? h('span.pill.warn', { style: { marginLeft: '6px' }, text: '内置' }) : null,
              ]),
              h('td', {
                style: { fontSize: '11.5px', color: 'var(--text-mute)' },
                text: (u.databases || []).join(', ') || '—',
              }),
              h('td', [
                u.is_locked ? h('span.pill.danger', { text: '已锁定' }) : h('span.pill.ok', { text: '正常' }),
                u.has_password ? null : h('span.pill.warn', { style: { marginLeft: '4px' }, text: '无密码' }),
              ]),
              h('td', [
                h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, [
                  h('button.btn.btn-sm', { text: '授权', onclick: () => viewGrants(u) }),
                  h('button.btn.btn-sm', { text: '改密码', onclick: () => changePassword(u) }),
                  u.user === 'root' ? h('button.btn.btn-sm', {
                    text: '不可删除', disabled: true, title: '不允许删除 root 账号',
                  }) : h('button.btn.btn-danger.btn-sm', {
                    text: '删除',
                    onclick: () => dropUser(u),
                  }),
                ]),
              ]),
            ]))),
          ]),
        ])
        : h('div.empty', [h('p', { text: '没有数据库账号' })]),
    );
  }

  function createUserModal(dbNames) {
    const name = h('input.input', { placeholder: '例如 appuser' });
    const host = h('input.input', { value: 'localhost', placeholder: 'localhost' });
    const pwd = h('input.input', { type: 'text', placeholder: '至少 8 位', value: randomPassword() });
    const dbSelect = h('select.select', { multiple: true, size: Math.min(6, Math.max(3, dbNames.length)), style: { minHeight: '90px' } },
      dbNames.filter((n) => !['information_schema', 'performance_schema'].includes(n))
        .map((n) => h('option', { value: n, text: n })));
    const allDBs = h('input', { type: 'checkbox', onchange: (e) => { dbSelect.disabled = e.target.checked; } });
    const privBoxes = {};
    const privList = ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'INDEX'];
    const privRow = h('div', { style: { display: 'flex', gap: '12px', flexWrap: 'wrap' } },
      privList.map((p) => {
        const cb = h('input', { type: 'checkbox', checked: ['SELECT', 'INSERT', 'UPDATE', 'DELETE'].includes(p) });
        privBoxes[p] = cb;
        return h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px' } }, [cb, h('span', { text: p })]);
      }));

    const m = modal({
      title: '新建数据库账号',
      wide: true,
      body: h('div', [
        h('div.row', [
          h('div.field', [h('label', { text: '用户名 *' }), name, h('div.hint', { text: '字母、数字、下划线、点、连字符' })]),
          h('div.field', [h('label', { text: '允许来源主机' }), host,
            h('div.hint', { text: 'localhost 只能本机连接；% 允许任意来源（不推荐，除非有防火墙）' })]),
        ]),
        h('div.field', [h('label', { text: '密码 *' }), pwd,
          h('div.hint', { text: '已自动生成随机密码，可直接使用或自行修改' })]),
        h('div.field', [
          h('label', { text: '授权数据库 *' }),
          h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '12.5px', marginBottom: '6px' } }, [
            allDBs, h('span', { text: '全部数据库（*.*，权限较大，请谨慎）' }),
          ]),
          dbSelect,
        ]),
        h('div.field', [h('label', { text: '权限' }), privRow]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '创建账号',
          onClick: null,
          onclick: async () => {
            const selected = allDBs.checked ? ['*'] : Array.from(dbSelect.selectedOptions).map((o) => o.value);
            const privs = Object.entries(privBoxes).filter(([, cb]) => cb.checked).map(([k]) => k);
            if (!name.value.trim()) { toast('请填写用户名', 'warn'); return; }
            if (!selected.length) { toast('请至少选择一个数据库', 'warn'); return; }
            if (!privs.length) { toast('请至少选择一个权限', 'warn'); return; }
            try {
              const r = await api.databaseUserCreate({
                user: name.value.trim(), host: host.value.trim() || 'localhost',
                password: pwd.value, privileges: privs, databases: selected,
              });
              toast(r.msg, 'ok', 10000);
              close(); load();
            } catch (e) { toast(e.message, 'err', 12000); }
          },
        }),
      ],
    });
    setTimeout(() => name.focus(), 60);
  }

  async function viewGrants(u) {
    const box = h('pre.logbox', { style: { maxHeight: '340px' }, text: '加载中…' });
    const m = modal({
      title: `授权：${u.user}@${u.host}`,
      wide: true,
      body: h('div', [
        h('div.hint', { style: { marginBottom: '10px' }, text: '以下是该账号在 MySQL 里实际拥有的授权（SHOW GRANTS 的真实输出）。' }),
        box,
        h('div', { style: { marginTop: '12px', display: 'flex', gap: '8px' } }, [
          h('button.btn.btn-sm', {
            text: '追加授权',
            onclick: () => { m.close(); grantMore(u); },
          }),
        ]),
      ]),
    });
    try {
      const r = await api.databaseGrants(u.user, u.host);
      box.textContent = (r.grants || []).join('\n');
    } catch (e) { box.textContent = '读取失败：' + e.message; }
  }

  function grantMore(u) {
    const db = h('input.input', { placeholder: '数据库名，或 * 表示全部' });
    const privBoxes = {};
    const row = h('div', { style: { display: 'flex', gap: '12px', flexWrap: 'wrap' } },
      PRIVILEGES.slice(0, 12).map((p) => {
        const cb = h('input', { type: 'checkbox' });
        privBoxes[p] = cb;
        return h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px' } }, [cb, h('span', { text: p })]);
      }));
    const m = modal({
      title: `追加授权：${u.user}@${u.host}`,
      wide: true,
      body: h('div', [
        h('div.field', [h('label', { text: '数据库' }), db, h('div.hint', { text: '填 * 表示全部数据库（权限较大）' })]),
        h('div.field', [h('label', { text: '要追加的权限' }), row]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '授权',
          onclick: async () => {
            const privs = Object.entries(privBoxes).filter(([, cb]) => cb.checked).map(([k]) => k);
            if (!privs.length) { toast('请选择权限', 'warn'); return; }
            try {
              const r = await api.databaseGrant({
                user: u.user, host: u.host,
                database: db.value.trim() || '*', privileges: privs,
              });
              toast(r.msg, 'ok');
              close(); load();
            } catch (e) { toast(e.message, 'err', 10000); }
          },
        }),
      ],
    });
  }

  async function changePassword(u) {
    const pwd = await promptBox({
      title: `重置密码：${u.user}@${u.host}`,
      label: '新密码（至少 8 位）',
      value: randomPassword(),
      hint: '重置后使用该账号的程序需要同步更新配置，否则会连接失败。',
    });
    if (!pwd) return;
    try {
      const r = await api.databaseUserPassword(u.user, u.host, pwd);
      toast(r.msg + '，新密码：' + pwd, 'ok', 15000);
    } catch (e) { toast(e.message, 'err', 10000); }
  }

  async function dropUser(u) {
    if (!await confirmBox(
      `删除账号 ${u.user}@${u.host}？\n\n该账号当前可访问：${(u.databases || []).join(', ') || '（无库级权限）'}\n` +
      '删除后使用该账号的程序会立即无法连接。',
      { title: '删除数据库账号', danger: true, okText: '删除' })) return;
    try {
      const r = await api.databaseUserDrop(u.user, u.host);
      toast(r.msg, 'ok');
      load();
    } catch (e) { toast(e.message, 'err', 10000); }
  }

  // ---------- SQL 执行 ----------
  function renderSQL() {
    const dbSel = h('select.select', { 'data-role': 'sql-db', style: { width: 'auto' } }, [
      h('option', { value: '', text: '不指定数据库' }),
      ...(cache.databases || []).map((d) => h('option', { value: d.name, text: d.name })),
    ]);
    const sqlBox = h('textarea.textarea', {
      placeholder: 'SELECT * FROM wp_posts LIMIT 10;\n\n注意：只允许单条语句，且不允许包含 SQL 注释。',
      style: { minHeight: '150px' },
    });
    const resultBox = h('div', { style: { marginTop: '14px' } });

    const run = async () => {
      clear(resultBox);
      appendAll(resultBox, h('div.empty', [h('p', { text: '执行中…' })]));
      try {
        const r = await api.databaseQuery(dbSel.value, sqlBox.value);
        clear(resultBox);
        if (r.is_select) {
          if (!r.rows.length) {
            appendAll(resultBox, h('div.empty', [h('p', { text: '查询成功，但没有返回数据行' })]));
            return;
          }
          appendAll(resultBox,
            h('div.hint', { style: { marginBottom: '8px' }, text: `返回 ${r.rows.length} 行，耗时 ${r.elapsed_ms}ms` }),
            h('div', { style: { overflowX: 'auto', maxHeight: '420px' } }, [
              h('table.table', [
                h('thead', [h('tr', (r.columns || []).map((c) => h('th', { text: c })))]),
                h('tbody', r.rows.map((row) => h('tr', row.map((cell) => h('td.mono', {
                  style: { fontSize: '11.5px', maxWidth: '300px', overflow: 'hidden', textOverflow: 'ellipsis' },
                  text: cell === null || cell === undefined ? 'NULL' : String(cell),
                  title: String(cell ?? ''),
                }))))),
              ]),
            ]),
          );
        } else {
          appendAll(resultBox, h('div', {
            style: { padding: '12px', background: 'var(--ok-soft)', borderRadius: '6px', fontSize: '13px' },
            text: `✓ ${r.affected || '执行成功'}（耗时 ${r.elapsed_ms}ms）`,
          }));
        }
      } catch (e) {
        clear(resultBox);
        appendAll(resultBox, h('div', {
          style: { padding: '12px', background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12.5px', whiteSpace: 'pre-wrap' },
          text: '执行失败：' + e.message,
        }));
      }
    };

    appendAll(body,
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '10px', flexWrap: 'wrap' } }, [
        h('span', { style: { fontSize: '12.5px', color: 'var(--text-dim)' }, text: '在数据库上执行：' }),
        dbSel,
        h('button.btn.btn-primary.btn-sm', { text: '▶ 执行', onclick: run }),
        h('button.btn.btn-sm', { text: '清空', onclick: () => { sqlBox.value = ''; clear(resultBox); } }),
      ]),
      sqlBox,
      h('div.hint', { style: { marginTop: '8px' }, text: '安全限制：只允许单条语句；不允许 SQL 注释（-- 与 /* */）；结果最多显示 2000 行。所有执行都会写入操作审计。' }),
      resultBox,
    );

    sqlBox.addEventListener('keydown', (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); run(); }
    });
  }

  // ---------- 导入导出 ----------
  async function renderBackup() {
    const listBox = h('div', { style: { marginTop: '14px' } });
    const loadBackups = async () => {
      clear(listBox);
      try {
        const r = await api.databaseBackups();
        const list = r.list || [];
        appendAll(listBox,
          h('div.hint', { style: { marginBottom: '8px' }, text: '备份目录：' + r.dir }),
          list.length
            ? h('table.table', [
              h('thead', [h('tr', [
                h('th', { text: '文件' }), h('th', { text: '大小' }), h('th', { text: '时间' }), h('th', { text: '操作' }),
              ])]),
              h('tbody', list.map((b) => h('tr', [
                h('td.mono', { style: { fontSize: '11.5px' }, text: b.name }),
                h('td.num', { text: humanSize(b.size) }),
                h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: b.mod_time }),
                h('td', [
                  h('div', { style: { display: 'flex', gap: '4px' } }, [
                    h('button.btn.btn-sm', {
                      text: '下载',
                      onclick: () => { window.location.href = api.fileDownloadURL(b.path); },
                    }),
                    h('button.btn.btn-sm', {
                      text: '导入',
                      title: '把这个文件导入到指定的库',
                      onclick: () => importModal(b.path),
                    }),
                  ]),
                ]),
              ]))),
            ])
            : h('div.empty', [h('p', { text: '还没有导出过备份' })]),
        );
      } catch (e) {
        appendAll(listBox, h('div.empty', [h('p', { text: '读取失败：' + e.message })]));
      }
    };

    appendAll(body,
      h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', marginBottom: '12px' } }, [
        h('button.btn.btn-sm', { text: '⟳ 刷新备份列表', onclick: loadBackups }),
        h('button.btn.btn-sm', { text: '导入 SQL 文件', onclick: () => importModal('') }),
      ]),
      h('div', {
        style: { padding: '12px', background: 'var(--panel-2)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.8' },
      }, [
        h('div', { text: '导出：在「数据库」标签页点某个库的「导出」按钮，会生成 .sql 文件到上方的备份目录。' }),
        h('div', { text: '导入：选择 .sql 文件并指定目标库。文件必须位于面板允许访问的目录内。' }),
        h('div', { text: '导出的 .sql 含完整表结构与数据，请妥善保管（可能含敏感数据）。' }),
      ]),
      listBox,
    );
    loadBackups();
  }

  function importModal(file) {
    const path = h('input.input', { value: file, placeholder: '/opt/zizpanel/work/db-backup/xxx.sql' });
    const dbSel = h('select.select', [
      h('option', { value: '', text: '请选择目标数据库' }),
      ...(cache.databases || []).filter((d) => !d.system).map((d) => h('option', { value: d.name, text: d.name })),
    ]);
    const m = modal({
      title: '导入 SQL 文件',
      wide: true,
      body: h('div', [
        h('div.field', [h('label', { text: 'SQL 文件路径 *' }), path,
          h('div.hint', { text: '必须是面板允许访问的目录内的绝对路径' })]),
        h('div.field', [h('label', { text: '导入到数据库 *' }), dbSel,
          h('div.hint', { text: '文件里的建表语句会在这个库里执行；导入前请确认该库是空的或你想覆盖的对象' })]),
        h('div', {
          style: { padding: '10px', background: 'var(--warn-soft)', borderRadius: '6px', fontSize: '12.5px' },
          text: '⚠️ 导入是不可逆操作。如果目标库里已有同名表，可能会报错或产生冲突。建议先导出目标库做备份。',
        }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始导入',
          onclick: async () => {
            if (!dbSel.value) { toast('请选择目标数据库', 'warn'); return; }
            if (!path.value.trim()) { toast('请填写文件路径', 'warn'); return; }
            try {
              const r = await api.databaseImport(dbSel.value, path.value.trim());
              toast(r.msg, 'ok', 12000);
              close(); load();
            } catch (e) { toast(e.message, 'err', 15000); }
          },
        }),
      ],
    });
  }

  async function dumpDB(name) {
    const t = toast(`正在导出 ${name}…`, 'info', 0);
    try {
      const r = await api.databaseDump(name);
      t.remove();
      toast(`已导出 ${r.tables} 张表（${humanSize(r.size)}），耗时 ${r.elapsed_ms}ms`, 'ok', 12000);
      if (tab === 'backup') renderBody();
    } catch (e) {
      t.remove();
      toast('导出失败：' + e.message, 'err', 15000);
    }
  }

  registerCleanup(() => { });
  load();
}

// ---------- 工具 ----------

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 1 : 2) + ' ' + units[i];
}

// randomPassword 生成一个足够强的随机密码。
//
// 用 crypto.getRandomValues：Math.random 不是密码学安全的，
// 用它生成数据库密码是不合适的。
function randomPassword(len = 18) {
  const chars = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#%^&*-_';
  const buf = new Uint32Array(len);
  crypto.getRandomValues(buf);
  let out = '';
  for (let i = 0; i < len; i++) out += chars[buf[i] % chars.length];
  return out;
}

void apiURL;
