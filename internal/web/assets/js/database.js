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
import { state, registerCleanup, panelPath } from './app.js';
import { taskCenter } from './tasks.js';

let cache = null;
let tab = 'databases';

const PRIVILEGES = [
  'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE', 'DROP', 'ALTER', 'INDEX',
  'CREATE TEMPORARY TABLES', 'LOCK TABLES', 'EXECUTE', 'CREATE VIEW', 'SHOW VIEW',
  'CREATE ROUTINE', 'ALTER ROUTINE', 'EVENT', 'TRIGGER', 'REFERENCES',
];

// MASK 是口令列的默认遮罩：口令只有用户点「👁」时才写进 DOM，
// 隐藏状态下节点里只有这几个圆点（不是把口令藏在 data-* 属性里）。
const MASK = '••••••••';

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
      body.append(connectionForm({ msg: e.message }));
      return;
    }
    renderHead();
    renderTabs();
    renderBody();
  }

  // pmaURL 返回 phpMyAdmin 入口地址。
  //
  // 走**面板自己的入口**（`<当前地址>/<安全后缀>/phpmyadmin/`）：
  //   · nginx 上那个 /phpmyadmin/ 已经收紧成"只允许本机"（外部直连 403，
  //     这是用户要求的安全设定）；
  //   · 面板这条路要求先登录面板，正是用户要的"必须登录才能进"；
  //   · 用当前地址拼，所以从 127.0.0.1、局域网 IP、隧道域名访问都一致。
  function pmaURL() {
    return panelPath('phpmyadmin/');
  }

  // connectionForm 是「MySQL 连接设置」表单（保存后重新加载页面数据）。
  //
  // 为什么做成**常驻入口**（而不是只在连不上时才出现）：一键 LNMP / MySQL 安装的
  // root 口令是限时询问的（不填/超时＝自动生成）；万一"写回面板配置"失败，
  // 那一步的提示会让用户"复制任务结果里的一次性凭据，并在「数据库 → 连接设置」
  // 手工填入"。若这个表单只在连接失败时才渲染，面板连得上时用户根本找不到地方
  // 核对/更正口令 —— 那条指引就没有落点。
  //
  // 为什么把"密码"也回显：面板是登录后才能访问的后台，用户需要看到自己填过什么
  // 才能改。真正的保护在登录鉴权，不在这里。
  function connectionForm({ msg = '', onSaved = null } = {}) {
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
          if (onSaved) onSaved(); else await load();
        } catch (err) {
          toast('保存失败：' + err.message, 'err', 9000);
        } finally {
          save.disabled = false;
          save.textContent = '保存并重试';
        }
      },
    });
    const parts = [];
    if (msg) {
      parts.push(h('div.empty', [
        h('div.big', { text: '🔌' }),
        h('h4', { text: '无法连接 MySQL' }),
        h('p', { style: { color: 'var(--text-mute)', fontSize: '12px' }, text: msg }),
        h('p', {
          style: { fontSize: '12.5px', lineHeight: '1.7', textAlign: 'left', marginTop: '10px' },
          text: '面板需要一组能连上 MySQL 的账号密码。brew 全新安装的 MySQL ' +
                'root 是空密码（直接点保存即可）；如果你之前给 root 设过密码，' +
                '请在下面填写。',
        }),
      ]));
    }
    parts.push(
      h('div.hint', {
        style: { marginBottom: '10px', lineHeight: '1.7' },
        text: '一键安装 MySQL 时面板会限时询问 root 口令：不填或超时会自动生成一个强随机口令，' +
              '正常情况下已经写在这里。如果那次安装提示"写入面板配置失败"，' +
              '请从那次任务结果里复制一次性口令填到下面。',
      }),
      h('div.field', [h('label', { text: '主机' }), host]),
      h('div.field', [h('label', { text: '端口' }), port]),
      h('div.field', [h('label', { text: 'Socket' }), socket,
        h('div.hint', { text: 'TCP 连不上时会用 socket，一般不用改' })]),
      h('div.field', [h('label', { text: '用户名' }), user]),
      h('div.field', [h('label', { text: '密码' }), pass,
        h('div.hint', { text: '空密码就留空' })]),
      h('div', { style: { display: 'flex', gap: '8px', marginTop: '12px' } }, [
        save,
        h('button.btn', { text: '⟳ 仅重试', onclick: () => load() }),
      ]),
    );
    return h('div', { style: { maxWidth: '560px', margin: msg ? '0 auto' : '' } }, parts);
  }

  // connectionModal 打开常驻的「连接设置」弹窗。保存成功后关窗并重载数据库页，
  // 让"能不能连上"立刻有结论（而不是让用户自己再点一次重试）。
  function connectionModal() {
    const m = modal({
      title: 'MySQL 连接设置',
      body: connectionForm({
        onSaved: () => { m.close(); load(); },
      }),
    });
  }

  function renderHead() {
    clear(headBox);
    // 主入口是 phpMyAdmin：库表、权限、导入导出这些它比面板做得全。
    // 面板自研那几个 Tab 保留为**应急入口**（phpMyAdmin 起不来时还能改密码）。
    // 用**真链接**（<a>）而不是 button + window.open：
    //   · 用户能在浏览器状态栏 / 右键菜单里看到真实地址（不会被弹窗拦截器吞掉）；
    //   · 可以中键新标签打开、可以复制链接。
    // 之前是 button + window.open，被拦截时"点了没反应"，用户只能把当前页地址
    // 复制出来（还带着浏览器的 #:~:text= 片段），看起来就像"链接拼错了"。
    const pmaBtn = h('a.btn.btn-sm.btn-primary', {
      text: '↗ 打开 phpMyAdmin',
      href: pmaURL(),
      target: '_blank',
      rel: 'noopener',
      title: '库表与权限管理的推荐入口（需先登录面板）；面板内置工具作为应急备用',
    });
    // 常驻「连接设置」：安装时生成/失败回填口令的唯一落点（见 connectionForm 注释）。
    const connBtn = h('button.btn.btn-sm', {
      id: 'db-conn-settings',
      text: '🔌 连接设置',
      title: '查看/修改面板连接 MySQL 用的主机、账号与密码；安装时的一次性 root 口令填在这里',
      onclick: connectionModal,
    });
    if (!cache?.connected) {
      appendAll(headBox,
        h('span.pill.danger', { text: '未连接' }),
        h('button.btn.btn-sm', { text: '⟳ 重试', onclick: load }),
        connBtn,
        pmaBtn,
      );
      return;
    }
    // 引擎名由后端解析（MySQL 8.4 / MariaDB）；版本串里已经带引擎名时不重复。
    // 读不到就写"数据库"，不替后端猜一个引擎名。
    const engName = (cache.engine && cache.engine.name) || '数据库';
    const verStr = String(cache.version || '');
    const verPill = verStr.toLowerCase().includes(engName.toLowerCase())
      ? verStr : (engName + ' ' + verStr).trim();
    appendAll(headBox,
      pmaBtn,
      connBtn,
      h('span.pill.ok', { text: verPill }),
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
        // 引擎解析读不到就如实说"未复核"，绝不假装知道该连哪个引擎。
        cache?.engine && cache.engine.verified === false ? h('div', {
          style: { marginTop: '10px', padding: '8px 12px', background: 'var(--warn-soft)', borderRadius: '6px', fontSize: '12.5px', maxWidth: '560px', textAlign: 'left' },
          text: '数据库引擎状态未复核：' + (cache.engine.note || '读不到当前生效的引擎，面板不猜默认值'),
        }) : null,
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
          h('button.btn', { text: '⚙️ 连接设置', onclick: connectionModal }),
          h('button.btn', { text: '去「应用 → 已安装」启动 MySQL', onclick: () => { location.hash = '#/services'; } }),
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
      cache.credentials_error
        ? h('div', {
          style: { marginBottom: '12px', padding: '10px 12px', background: 'var(--warn-soft)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.7' },
          text: '⚠️ ' + cache.credentials_error,
        })
        : null,
      h('div.hint', {
        style: { marginBottom: '10px', lineHeight: '1.7' },
        text: '说明：MySQL 里只存口令的哈希，无法还原原文。面板能显示口令，是因为它在创建/改口令时把明文自己存了一份；'
          + '不是面板创建、也没有被面板改过口令的账号会显示「外部设置，不可回显」，请用「重设密码」设一个新口令。',
      }),
      users.length
        ? h('div', { style: { overflowX: 'auto' } }, [
          h('table.table', [
            h('thead', [h('tr', [
              h('th', { text: '账号' }), h('th', { text: '可访问的库' }), h('th', { text: '状态' }),
              h('th', { text: '密码' }), h('th', { text: '操作' }),
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
                // MariaDB 的 mysql.user 没有 account_locked 列，读不到锁定状态：
                // 如实标"不支持"，绝不把读不到的 false 显示成"正常"。
                u.lock_state_known === false
                  ? h('span.pill.warn', {
                    text: '锁定状态不支持',
                    title: '这个服务端（MariaDB）的 mysql.user 没有 account_locked 列，面板读不到账号锁定状态',
                  })
                  : (u.is_locked ? h('span.pill.danger', { text: '已锁定' }) : h('span.pill.ok', { text: '正常' })),
                u.has_password ? null : h('span.pill.warn', { style: { marginLeft: '4px' }, text: '无密码' }),
              ]),
              h('td', passwordCell(u)),
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

  // passwordCell 渲染「密码」列。
  //
  // 诚实原则（本功能的红线）：
  //   MySQL 里只存口令的哈希，任何面板都无法从数据库反推原文。
  //   面板之所以能显示口令，只因为它**自己在创建/改口令时把明文存了下来**。
  //   · password_known=true  → 默认打码，一键显示/复制；
  //   · password_known=false → 明确显示「外部设置，不可回显」+「重设密码」，
  //     **绝不**显示空串或编一个值冒充。
  //
  // 安全：口令只在闭包里，隐藏时 DOM 里只有圆点；
  // 不写进 URL / localStorage / console / data-* 属性。
  function passwordCell(u) {
    const wrap = h('div', { style: { display: 'flex', gap: '4px', alignItems: 'center', flexWrap: 'wrap' } });
    if (!u.password_known) {
      appendAll(wrap,
        h('span', {
          style: { fontSize: '11.5px', color: 'var(--text-mute)' },
          text: '外部设置，不可回显',
          title: '这个账号不是面板创建的（或由面板旧版本创建）。MySQL 里只有口令哈希，无法还原原文。要改口令请点「重设密码」。',
        }),
        h('button.btn.btn-sm', { text: '重设密码', onclick: () => changePassword(u) }),
      );
      return wrap;
    }
    // 已知但为空：面板创建时就没设口令。这是"已知为空"，不是"未知"。
    if (!u.password) {
      appendAll(wrap, h('span.pill.warn', {
        text: '空密码（无口令）',
        title: '这个账号没有口令，任何能连到 MySQL 的程序都能用它登录。建议点「改密码」设一个。',
      }));
      return wrap;
    }
    let revealed = false;
    const value = h('span.mono', {
      style: { fontSize: '12px', letterSpacing: '1px' },
      text: MASK,
    });
    const toggle = h('button.btn.btn-sm', {
      text: '👁',
      title: '显示 / 隐藏口令',
      onclick: () => {
        revealed = !revealed;
        value.textContent = revealed ? u.password : MASK;
        value.style.letterSpacing = revealed ? '0' : '1px';
        toggle.textContent = revealed ? '🙈' : '👁';
      },
    });
    const copy = h('button.btn.btn-sm', {
      text: '📋',
      title: '复制口令到剪贴板',
      // 复制走闭包里的口令；即使处于打码状态也能复制。
      // 剪贴板不可用时先显示再选中（降级路径，照抄 tasks.js）。
      onclick: () => copySecret(u.password, () => {
        if (!revealed) { revealed = true; value.textContent = u.password; value.style.letterSpacing = '0'; toggle.textContent = '🙈'; }
        return value;
      }),
    });
    appendAll(wrap, value, toggle, copy,
      u.panel_account ? h('span.pill', {
        style: { marginLeft: '2px' },
        text: '面板连接用',
        title: '这是面板连接 MySQL 自己使用的账号；改它的口令会同步写回面板配置（见「🔌 连接设置」）。',
      }) : null);
    return wrap;
  }

  // copySecret 复制口令。剪贴板 API 不可用（非 HTTPS / 非 localhost）或写入失败时，
  // 降级为"把口令显示出来并选中"，让用户按 ⌘/Ctrl+C —— 与 tasks.js 的处理一致。
  function copySecret(secret, reveal) {
    if (!secret) { toast('没有可复制的内容', 'warn'); return; }
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      selectText(reveal());
      toast('当前环境不支持自动复制，已显示并选中，请按 ⌘/Ctrl+C 复制', 'warn', 8000);
      return;
    }
    navigator.clipboard.writeText(secret)
      .then(() => toast('口令已复制到剪贴板', 'ok'))
      .catch(() => {
        selectText(reveal());
        toast('复制失败，已显示并选中，请按 ⌘/Ctrl+C 复制', 'warn', 8000);
      });
  }

  // selectText 是复制按钮的降级路径：把节点里的文本选中。
  function selectText(el) {
    if (!el || typeof document.createRange !== 'function' || typeof window.getSelection !== 'function') return;
    try {
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
    } catch { /* 选中失败不是致命问题，用户还可以手动选中 */ }
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
    // 默认**全选**：以前默认只勾 SELECT/INSERT/UPDATE/DELETE，于是按默认建出来的账号
    // 能读写却不能建表 —— 用它导入任何 `.sql` 备份都会得到
    // `#1142 CREATE command denied`（2026-09-18 用户报障："新建用户选择数据库，
    // 结果没有权限"）。导入备份是账号最常见的用途，默认值必须能用。
    const privRow = h('div', { style: { display: 'flex', gap: '12px', flexWrap: 'wrap' } },
      privList.map((p) => {
        const cb = h('input', { type: 'checkbox', checked: true });
        privBoxes[p] = cb;
        return h('label', { style: { display: 'flex', gap: '5px', alignItems: 'center', fontSize: '12.5px' } }, [cb, h('span', { text: p })]);
      }));
    // 取消勾选「建表相关」权限时，**当场**把后果说清楚（不要等用户导入时才撞 #1142）。
    const privWarn = h('div.hint', { style: { color: 'var(--warn)' }, text: '' });
    const refreshPrivWarn = () => {
      const missing = ['CREATE', 'ALTER', 'DROP', 'INDEX'].filter((k) => privBoxes[k] && !privBoxes[k].checked);
      privWarn.textContent = missing.length
        ? '⚠️ 不含 ' + missing.join('/') + '：这个账号**无法导入 .sql 备份**（备份里有建表/改表语句），也建不了表'
        : '';
    };
    Object.values(privBoxes).forEach((cb) => cb.addEventListener('change', refreshPrivWarn));

    // 创建按钮先建出来：失败/部分失败时要能禁用它，避免用户重复点导致重复建号。
    const createBtn = h('button.btn.btn-primary', {
      text: '创建账号',
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
          if (r.warning) {
            // 账号在 MySQL 里已经建好了，只是面板没能保存口令用于回显。
            // **不关窗**：这个口令此后在列表里看不到，必须让用户当场复制走。
            createBtn.disabled = true;
            createBtn.textContent = '已创建（口令未能保存）';
            toast('账号已创建，但面板未能保存口令供日后回显：' + r.warning
              + '｜请立即复制这个密码：' + pwd.value, 'warn', 0);
            load();
            return;
          }
          toast(r.msg + '，新密码：' + pwd.value + '（已保存，之后可在「账号与权限」列表里点 👁 查看）', 'ok', 15000);
          close(); load();
        } catch (e) { toast(e.message, 'err', 12000); }
      },
    });

    refreshPrivWarn();
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
        createBtn,
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
      hint: u.panel_account
        ? '这是面板连接 MySQL 自己用的账号：新口令会同时写回面板配置（见「🔌 连接设置」），否则面板会连不上。'
        : '重置后使用该账号的程序需要同步更新配置，否则会连接失败。',
    });
    if (!pwd) return;
    try {
      const r = await api.databaseUserPassword(u.user, u.host, pwd);
      if (r.warning) {
        // 口令已在 MySQL 生效，但面板没能保存它用于回显 —— 此后列表里看不到，
        // 必须让用户当场记下来。
        toast(r.msg + '。⚠️ ' + r.warning + '｜新密码：' + pwd, 'warn', 0);
      } else {
        toast(r.msg + '，新密码：' + pwd + '（已保存，之后可在列表里点 👁 查看）', 'ok', 15000);
      }
      // 重新拉一次 GET /api/v1/database：让列表里的密码列反映**面板实际保存的**值，
      // 而不是只改内存里那一行（否则刷新页面又会变回去）。
      await load();
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
            // 走任务中心：导入大 SQL 是分钟级动作，任务里按**真实字节数**报进度，
            // 关掉窗口也能找回，还能中断（2026-09-18 用户报障：导入时页面毫无输出，
            // 只能看着像"卡死"）。
            close();
            try {
              await taskCenter.start({
                kind: 'db_import', target: dbSel.value,
                title: '导入 SQL → ' + dbSel.value,
                start: () => api.databaseImport(dbSel.value, path.value.trim()),
                onDone: (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('导入失败：' + (task.error || task.status), 'err', 16000);
                    return;
                  }
                  const r = (task && task.result) || {};
                  toast(r.msg || '导入完成', 'ok', 12000);
                  load();
                },
              });
            } catch (e) { toast('导入失败：' + ((e && e.message) || e), 'err', 15000); }
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
