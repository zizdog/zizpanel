// app.js —— 应用入口：状态、路由、登录/初始化、布局外壳。
//
// 路由用 hash（#/xxx）而非 History API：面板可能被放在子路径或反代后面，
// hash 路由不需要服务端配合改写，也避免了刷新 404。

import { api, ApiError } from './api.js';
import { h, clear, toast, $, modal, bytes, duration, esc } from './ui.js';
import { DashboardView, SettingsView } from './views.js';
import { SystemSettingsView } from './systemsettings.js';
import { SitesView } from './sites.js';
import { ReverseProxyView } from './reverseproxy.js';
import { CertsView } from './certs.js';
import { AppsView } from './apps.js';
import { FilesView } from './files.js';
import { TerminalView } from './terminal.js';
import { CronView } from './cron.js';
import { LogsHubView } from './logshub.js';
import { DatabaseView } from './database.js';
import { DockerView } from './docker.js';
import { UpdateView, startUpgradeWatcher, hasUpdate } from './update.js';
import { taskCenter } from './tasks.js';

// ---------------- 全局状态 ----------------
export const state = {
  session: null, // { user, version, config, counts }
  metrics: null, // 最近一次系统指标
  route: '',
};

// ---------------- 导航注册表 ----------------
// phase 表示该模块计划在哪个阶段交付，占位页会显示出来。
export const NAV = [
  { group: '总览' },
  { id: 'dashboard', title: '仪表盘', icon: '📊', view: DashboardView },
  // 「系统监控」已改成「mac设置」（2026-09 用户要求把显示名从「系统设置」改成
  // 「mac设置」：原名与「面板设置」并列时歧义太大）。**只改显示名**，
  // 路由 id 仍是 'system'，`#/system` 与所有旧链接照旧可用。
  { id: 'system', title: 'mac设置', icon: '🛠️', view: SystemSettingsView },
  { group: '网站' },
  { id: 'sites', title: '网站管理', icon: '🌐', view: SitesView },
  { id: 'proxy', title: '反向代理', icon: '🔀', view: ReverseProxyView },
  // 证书与站点同级而不是塞进站点详情：一张泛域名证书常被多个站点共用，
  // 而且"先申请证书、后建站"也是常见顺序，挂在站点下面会找不到入口。
  { id: 'certs', title: 'SSL 证书', icon: '🔐', view: CertsView },
  { id: 'database', title: '数据库', icon: '🗄️', view: DatabaseView },
  { group: '服务器' },
  // 2026-09-17 信息架构：原来的「服务管理」与「应用市场」两个导航项合成
  // **一个「应用」版块**（侧栏只有这一项），页内四个一级 Tab：
  // **已安装（默认）/ 应用市场 / docker / 一键建站**（用户第二版要求）。
  //   · 真机量化（mini）：服务管理 31 行，31 行全都也出现在应用市场，独有内容 0 行；
  //     而且服务管理内部有 4 对重复（php81/…/php84 各有两条记录指向同一 launchd 标签）。
  //   · `#/services` 这类老书签/文档里的链接**必须继续可用**：见 ROUTE_TARGET
  //     的别名表 —— 它落到同一个 AppsView，并自动切到「已安装」Tab；
  //     `#/apps/docker` 这类带 Tab 的 hash 也能直接指定页内 Tab（见 routeFor）。
  { id: 'apps', title: '应用', icon: '🧩', view: AppsView },
  { id: 'docker', title: 'Docker', icon: '🐳', view: DockerView },
  { group: '运维' },
  { id: 'files', title: '文件管理', icon: '📁', view: FilesView },
  { id: 'terminal', title: 'Web 终端', icon: '🖥️', view: TerminalView },
  { id: 'cron', title: '计划任务', icon: '⏰', view: CronView },
  { group: '系统' },
  // 2026-09 用户要求：顺序上「面板设置 → 日志 → 检查更新」。
  // 「日志」把原「日志中心」与「操作审计」合并成一页两个 Tab（见 logshub.js）：
  // 侧栏只剩一个入口，旧的 #/logs 与 #/audit 仍分别落到对应 Tab（见 ROUTE_TARGET）。
  { id: 'settings', title: '面板设置', icon: '🔧', view: SettingsView },
  { id: 'logs', title: '日志', icon: '📜', view: LogsHubView },
  // 2026-09-21：原来这里是「面板设置 → 关于与运维」的第 4 个 Tab。用户要求把它
  // 整块提到侧边栏并改名「检查更新」——"有没有新版本"是随时想知道的状态，
  // 埋在两级点击之后没人看得到，也就没人升级。
  { id: 'update', title: '检查更新', icon: '⬆️', view: UpdateView },
];

const NAV_BY_ID = Object.fromEntries(NAV.filter((n) => n.id).map((n) => [n.id, n]));

// ROUTE_TARGET 是"旧路由 → 合并后的落点"的别名表。
//
// 为什么必须有它：`#/services` 写在文档、书签、以及别的页面（数据库页的
// "去已安装启动 MySQL"、Docker 页的"去服务管理"、仪表盘的快捷卡）里。
// 合并导航后如果它变成 404/仪表盘，用户会以为功能没了。
//   services → apps 版块，并**自动切到「已安装」Tab**（那里就是原服务管理的清单）。
//
// 这个函数导出只有一个原因：tools/appdetail-verify.mjs 用它断言
// "#/services 落到已安装 Tab"，而不是靠人肉点。运行时没有别的调用方。
const ROUTE_TARGET = {
  services: { id: 'apps', tab: 'installed' },
  // 2026-09：「日志中心」与「操作审计」合并成侧栏「日志」一页两个 Tab。
  // 旧 hash `#/audit` 必须继续可用 → 落到 logs 版块并直接切到「操作审计」Tab；
  // `#/logs` 不需要别名（id 本来就是 logs），默认落第一个 Tab。
  audit: { id: 'logs', tab: 'audit' },
  // 「关于与运维」已从「面板设置」的页内 Tab 提成独立页（侧栏「检查更新」）。
  // 页内 Tab 时代的链接有两种写法，一个都不能 404：
  //   #/settings/about  —— 旧写法（版块 + Tab），见下面的 SUB_ROUTE_TARGET
  //   #/about / #/upgrade —— 文档/书签里可能直接引用的单段写法
  about: { id: 'update' },
  upgrade: { id: 'update' },
};

// 两段式 hash（#/<版块>/<Tab>）的别名表，键是 "版块/Tab"。
const SUB_ROUTE_TARGET = {
  'settings/about': { id: 'update' },
};

// routeFor 解析 hash：#/<版块> 或 #/<版块>/<页内 Tab>（如 #/apps/docker）。
//
// 第二段只对"有页内 Tab 的页面"有意义（目前只有「应用」的
// installed / market / docker / sites），其它页面拿到 tab 也不会用。
// 老 hash `#/apps`（无第二段）tab 为空串，由 AppsView 落回默认的「已安装」。
export function routeFor(hash = location.hash) {
  const m = String(hash || '').match(/^#\/([a-z0-9_-]+)(?:\/([a-z0-9_-]+))?/i);
  const id = m ? m[1] : 'dashboard';
  const sub = (m && m[2]) ? m[2] : '';
  const t = ROUTE_TARGET[id] || (sub ? SUB_ROUTE_TARGET[`${id}/${sub}`] : null);
  return t ? { ...t } : { id, tab: sub };
}

// panelPath 拼"面板内的路径"，例如 panelPath('phpmyadmin/') → "/jab5c63/phpmyadmin/"。
//
// 为什么所有入口都要走它（用户明确要求"不同入口地址应该一致"）：
// 面板可能从 127.0.0.1:8443、局域网 IP、或隧道域名访问。以前各处硬拼
// `http://<局域网IP>/xxx/`，于是同一个应用在不同页面给出不同地址，
// 有的还指向已经被收紧的 nginx 端口（403）。统一用**当前访问的面板地址** +
// 安全后缀，入口就永远是同一个，且天然支持隧道访问。
export function panelPath(sub = '') {
  const entry = state.session?.config?.panel_entry || '/';
  return entry.replace(/\/+$/, '/') + String(sub).replace(/^\/+/, '');
}

// ---------------- 主题（三态：light / dark / auto） ----------------
//
// 用户 2026-09-17 反馈："之前的夜间模式 3 态，现在没有自动模式了"。
// 以前这里把"跟随系统"当成了**首次访问时的一次性推导**（把系统偏好落成一个具体值
// 存进 localStorage），于是存下来之后就跟系统脱钩了 —— 系统换了主题，面板不跟。
// 现在把 auto 当成**真正的第三态**存起来：
//   · 'light' / 'dark'：用户手动固定；
//   · 'auto'（或没有存过）：跟随系统，且系统主题变化时**实时**跟随。
const THEME_KEY = 'zp-theme';
const darkQuery = window.matchMedia('(prefers-color-scheme: dark)');

function themePref() {
  const v = localStorage.getItem(THEME_KEY);
  return v === 'light' || v === 'dark' ? v : 'auto';
}

// resolvedTheme 把偏好折算成真正生效的主题（auto → 当前系统主题）。
function resolvedTheme() {
  const p = themePref();
  return p === 'auto' ? (darkQuery.matches ? 'dark' : 'light') : p;
}

function applyTheme() {
  document.documentElement.dataset.theme = resolvedTheme();
}

function initTheme() {
  applyTheme();
  // 只有 auto 需要监听：固定主题时系统变化不该影响面板。
  darkQuery.addEventListener('change', () => { if (themePref() === 'auto') applyTheme(); });
}

// 主题按钮的图标与提示按**偏好**（而不是折算后的主题）显示 —— 否则"跟随系统"
// 会看起来像手动固定成了当前那个主题，用户就再也找不到 auto 了。
function themeButtonFace() {
  const p = themePref();
  if (p === 'light') return { icon: '☀️', title: '主题：浅色（点击切换：深色）' };
  if (p === 'dark') return { icon: '🌙', title: '主题：深色（点击切换：跟随系统）' };
  return { icon: '🌗', title: `主题：跟随系统（当前 ${resolvedTheme() === 'dark' ? '深色' : '浅色'}，点击切换：浅色）` };
}

function toggleTheme() {
  const order = ['light', 'dark', 'auto'];
  const next = order[(order.indexOf(themePref()) + 1) % order.length];
  localStorage.setItem(THEME_KEY, next);
  applyTheme();
}

// ---------------- 启动 ----------------
async function boot() {
  initTheme();
  window.addEventListener('hashchange', render);
  try {
    const st = await api.setupStatus();
    if (st.needs_setup) { renderSetup(); return; }
  } catch (e) {
    showBootError(e);
    return;
  }
  try {
    state.session = await api.session();
  } catch (e) {
    if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
      renderLogin();
      if (e.status === 403) toast(e.message, 'err', 8000);
      return;
    }
    showBootError(e);
    return;
  }
  // 已登录。如果用户开了 2FA 而面板策略要求强制 2FA，提示一次
  if (state.session.user && state.session.user.is_admin === false) {
    toast('当前为普通账号，部分操作受限', 'warn');
  }
  renderApp();
}

function showBootError(err) {
  const app = $('app') || document.getElementById('app');
  const boot = document.getElementById('boot');
  const msg = err instanceof Error ? err.message : String(err);
  const box = h('div.auth-wrap', [
    h('div.auth-card', [
      h('div.auth-brand', [
        h('div.mark', { text: 'Z' }),
        h('div', [h('h1', { text: 'ZizPanel' }), h('p', { text: '无法连接面板服务' })]),
      ]),
      h('p.sub', { text: msg }),
      h('div.card', [
        h('div.card-body', [
          h('div.section-title', { text: '排查步骤' }),
          h('div', { style: { fontSize: '12.5px', lineHeight: '1.9', color: 'var(--text-dim)' } }, [
            h('div', { text: '1. 确认服务在运行：' }),
            h('pre.logbox', { style: { maxHeight: '90px' }, text: 'sudo launchctl list | grep zizpanel' }),
            h('div', { text: '2. 查看日志：' }),
            h('pre.logbox', { style: { maxHeight: '90px' }, text: 'zizpanel status\ntail -50 /opt/zizpanel/logs/panel-$(date +%Y%m%d).log' }),
          ]),
        ]),
      ]),
      h('button.btn.btn-primary.btn-block', { text: '重新加载', onclick: () => location.reload() }),
    ]),
  ]);
  if (boot) boot.remove();
  clear(app).appendChild(box);
  app.hidden = false;
}

// ---------------- 登录 / 初始化 ----------------

function authShell(title, subtitle, body) {
  return h('div.auth-wrap', [
    h('div.auth-card', [
      h('div.auth-brand', [
        h('div.mark', { text: 'Z' }),
        h('div', [h('h1', { text: 'ZizPanel' }), h('p', { text: 'macOS 网站与服务管理面板' })]),
      ]),
      h('h2', { text: title }),
      h('p.sub', { text: subtitle }),
      body,
    ]),
  ]);
}

function mount(node) {
  const boot = document.getElementById('boot');
  if (boot) boot.remove();
  const app = document.getElementById('app');
  clear(app).appendChild(node);
  app.hidden = false;
}

function renderLogin() {
  const username = h('input.input', { type: 'text', autocomplete: 'username', placeholder: '用户名', value: 'admin' });
  const password = h('input.input', { type: 'password', autocomplete: 'current-password', placeholder: '登录密码' });
  const btn = h('button.btn.btn-primary.btn-block', { text: '登 录' });

  const doLogin = async () => {
    if (!username.value.trim() || !password.value) { toast('请输入用户名和密码', 'warn'); return; }
    btn.disabled = true; btn.textContent = '正在登录…';
    try {
      const res = await api.login(username.value.trim(), password.value);
      if (res && res.need_totp) { renderTOTPLogin(res.challenge); return; }
      state.session = await api.session();
      toast('登录成功', 'ok');
      location.hash = '#/dashboard';
      renderApp();
    } catch (e) {
      toast(e.message, 'err');
      btn.disabled = false; btn.textContent = '登 录';
    }
  };
  password.addEventListener('keydown', (e) => { if (e.key === 'Enter') doLogin(); });
  btn.addEventListener('click', doLogin);

  mount(authShell('登录面板', '请输入账号密码', h('div', [
    h('div.field', [h('label', { text: '用户名' }), username]),
    h('div.field', [h('label', { text: '密码' }), password]),
    btn,
    h('div.auth-foot', { text: 'ZizPanel · 自托管面板' }),
  ])));
  setTimeout(() => password.focus(), 60);
}

function renderTOTPLogin(challenge) {
  const code = h('input.input', {
    type: 'text', inputmode: 'numeric', maxlength: 6, autocomplete: 'one-time-code',
    placeholder: '6 位动态验证码', style: { letterSpacing: '6px', textAlign: 'center', fontSize: '18px' },
  });
  const btn = h('button.btn.btn-primary.btn-block', { text: '验 证' });
  let submitted = false;
  const doVerify = async () => {
    if (submitted) return;
    submitted = true;
    btn.disabled = true; btn.textContent = '验证中…';
    try {
      await api.loginTOTP(challenge, code.value.trim());
      state.session = await api.session();
      toast('登录成功', 'ok');
      location.hash = '#/dashboard';
      renderApp();
    } catch (e) {
      toast(e.message, 'err');
      submitted = false; btn.disabled = false; btn.textContent = '验 证';
      code.select();
    }
  };
  code.addEventListener('keydown', (e) => { if (e.key === 'Enter') doVerify(); });
  btn.addEventListener('click', doVerify);

  mount(authShell('两步验证', '请输入验证器 App 中的 6 位动态码', h('div', [
    h('div.field', [h('label', { text: '动态验证码' }), code]),
    btn,
    h('button.btn.btn-ghost.btn-block', { text: '返回重新登录', style: { marginTop: '8px' }, onclick: renderLogin }),
  ])));
  setTimeout(() => code.focus(), 60);
}

function renderSetup() {
  const username = h('input.input', { type: 'text', value: 'admin', autocomplete: 'username' });
  const pwd1 = h('input.input', { type: 'password', autocomplete: 'new-password', placeholder: '至少 8 位' });
  const pwd2 = h('input.input', { type: 'password', autocomplete: 'new-password', placeholder: '再次输入' });
  const btn = h('button.btn.btn-primary.btn-block', { text: '创建管理员并进入面板' });

  const submit = async () => {
    const u = username.value.trim();
    if (u.length < 2) { toast('用户名至少 2 个字符', 'warn'); return; }
    if (pwd1.value.length < 8) { toast('密码至少 8 位', 'warn'); return; }
    if (pwd1.value !== pwd2.value) { toast('两次输入的密码不一致', 'warn'); return; }
    btn.disabled = true; btn.textContent = '正在初始化…';
    try {
      await api.setup(u, pwd1.value);
      state.session = await api.session();
      toast('初始化完成，欢迎使用 ZizPanel', 'ok');
      location.hash = '#/dashboard';
      renderApp();
    } catch (e) {
      toast(e.message, 'err', 7000);
      btn.disabled = false; btn.textContent = '创建管理员并进入面板';
    }
  };
  pwd2.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
  btn.addEventListener('click', submit);

  mount(authShell('首次初始化', '面板尚未创建账号，请设置管理员账号', h('div', [
    h('div.field', [h('label', { text: '管理员用户名' }), username, h('div.hint', { text: '用于登录面板，建议不要使用 admin 以外过于简单的名字' })]),
    h('div.field', [h('label', { text: '登录密码' }), pwd1, h('div.hint', { text: '至少 8 位，建议包含大小写字母与数字' })]),
    h('div.field', [h('label', { text: '确认密码' }), pwd2]),
    btn,
    h('div.auth-foot', { text: '创建后可在「面板设置」中开启两步验证' }),
  ])));
  setTimeout(() => pwd1.focus(), 60);
}

// ---------------- 主界面 ----------------

function renderApp() {
  // 路由先过别名表：`#/services` 会落到 apps 版块的「已安装」Tab（见 ROUTE_TARGET）；
  // `#/apps/docker` 这类带 Tab 的 hash 由 routeFor 解析出 tab 传进页面。
  const target = routeFor();
  const item = NAV_BY_ID[target.id] || NAV_BY_ID.dashboard;

  const nav = h('nav.nav');
  NAV.forEach((n) => {
    if (n.group) { nav.appendChild(h('div.nav-group', { text: n.group })); return; }
    const active = n.id === item.id;
    // 「检查更新」上的小红点：发现新版本时挂在侧栏上，**刷新后仍然在**
    // （检测结果落在 localStorage，见 update.js 的 updateInfo()）。
    const dot = n.id === 'update'
      ? h('span.badge.zp-update-dot', {
        dataset: { testid: 'zp-update-badge' },
        text: '新',
        title: '发现新版本，点击查看',
        style: { background: 'var(--danger)', color: '#fff', border: '1px solid transparent' },
        hidden: !hasUpdate(),
      })
      : null;
    nav.appendChild(h(`div.nav-item${active ? '.active' : ''}`, {
      onclick: () => { location.hash = '#/' + n.id; document.body.classList.remove('nav-open'); },
    }, [
      h('span.ico', { text: n.icon }),
      h('span', { text: n.title }),
      n.phase ? h('span.badge', { text: n.phase }) : null,
      dot,
    ]));
  });

  const user = state.session?.user || {};
  const sidebar = h('aside.sidebar', [
    h('div.sidebar-head', [
      h('div.mark', { text: 'Z' }),
      h('div', [
        h('div.title', { text: 'ZizPanel' }),
        h('div.ver', { text: 'v' + (state.session?.version || '') }),
      ]),
    ]),
    nav,
    h('div.sidebar-foot', [
      // 给个 id：改用户名后可以原地更新这一行，不必重渲染整个外壳
      // （重渲染会把设置页打回第一个 Tab，用户会觉得"改完跳走了"）
      h('div', { id: 'sidebar-account', text: '登录账号：' + (user.username || '-') }),
      h('div', { text: (state.session?.config?.listen || '') + (state.session?.config?.tls ? ' · HTTPS' : ' · HTTP') }),
    ]),
  ]);

  const content = h('div.content');
  const pageTitle = h('h2', { text: item.title });
  const topbar = h('div.topbar', [
    h('button.btn.btn-ghost.btn-icon.hamburger', { text: '☰', title: '菜单', onclick: () => document.body.classList.toggle('nav-open') }),
    pageTitle,
    h('span.crumb', { text: item.group || '' }),
    h('div.spacer'),
    // 任务中心入口：任务状态活在 tasks.js 的模块级单例里，不随路由重建，
    // 所以任何页面都能重新打开正在安装的任务（关掉窗口 ≠ 取消任务）。
    taskCenter.button(),
    h('span.pill.' + (user.totp_enabled ? 'ok' : 'warn'), {
      text: user.totp_enabled ? '2FA 已开启' : '2FA 未开启',
      title: '两步验证状态，可在「面板设置」中调整',
    }),
    (() => { const f = themeButtonFace(); return h('button.btn.btn-ghost.btn-icon', { text: f.icon, title: f.title, onclick: () => { toggleTheme(); renderApp(); } }); })(),
    h('button.btn.btn-ghost.btn-icon', { text: '⟳', title: '刷新', onclick: () => render() }),
    h('button.btn.btn-ghost.btn-icon', { text: '⏻', title: '退出登录', onclick: doLogout }),
  ]);

  const main = h('main.main', [topbar, content]);
  const layout = h('div.layout', [sidebar, main, h('div.overlay', { onclick: () => document.body.classList.remove('nav-open') })]);

  mount(layout);
  document.body.classList.remove('nav-open');

  // 任务中心初始化：内部幂等（只会拉一次 GET /api/v1/tasks，然后按运行中的任务建 SSE）。
  // 之所以放在这里而不是 boot()，是因为 renderApp() 会重建顶栏按钮 ——
  // 初始化完成后徽标要画在"当前这个"按钮上。
  taskCenter.init();

  // 新版本主动检测：进面板时自动测一次，之后每 6 小时一次（幂等，页面可见时才跑）。
  // 结果变化会派发 zp:update-state，由下面的监听原地同步侧栏小红点。
  startUpgradeWatcher();
  syncUpdateBadge();

  // 渲染前先清理上一个页面的长连接（SSE / 定时器），避免叠加泄漏
  runCleanup();

  // 渲染当前页面：所有页面统一拿到同一份 ctx。
  //
  // 这里以前写的是 `if (view.length >= 2)` —— 只有"声明了两个形参"的页面才拿得到
  // onLeave。而 `export function DashboardView(content, ctx = {})` 因为有**默认值**，
  // `Function.length` 是 1，于是仪表盘注册清理函数的那个分支从来没走到过：
  // 每重渲染一次仪表盘就漏一条 /api/v1/system/stream 长连接（切主题、切路由都算），
  // 攒够 6 条就把浏览器的"同一主机最多 6 个连接"占满 ——
  // 之后面板所有接口都挂住不返回，**服务端其实一切正常**（curl 200/56ms），
  // 只有浏览器里一片"正在读取…"。这是 2026-09-14 修掉的真坑。
  // 教训：形参个数不是"能力声明"，别拿它做路由判断。
  const view = item.view;
  view(content, { item, topbar, pageTitle, onLeave: registerCleanup, tab: target.tab });
  document.title = `${item.title} · ZizPanel`;
}

// syncUpdateBadge 原地亮/灭侧栏「检查更新」的小红点。
//
// 为什么不靠重渲染外壳：renderApp() 会把页面打回默认状态（正在填的表单会丢）。
// update.js 检测完只派发一个事件，这里负责把徽标改掉。
function syncUpdateBadge() {
  const show = hasUpdate();
  document.querySelectorAll('.zp-update-dot').forEach((el) => { el.hidden = !show; });
}
window.addEventListener('zp:update-state', syncUpdateBadge);

// ---------------- 页面级资源清理 ----------------
// View 通过 ctx.onLeave(fn) 注册清理函数；切换路由时统一执行。
let cleanups = [];

export function registerCleanup(fn) {
  if (typeof fn === 'function') cleanups.push(fn);
}

function runCleanup() {
  cleanups.forEach((fn) => {
    try { fn(); } catch (e) { console.warn('cleanup failed', e); }
  });
  cleanups = [];
}

async function doLogout() {
  try { await api.logout(); } catch { /* 网络失败也要让用户退出，本地状态必须清空 */ }
  state.session = null;
  runCleanup();
  location.hash = '';
  renderLogin();
}

function render() {
  // 未登录时直接展示登录页，不要重新走 boot()：
  // boot 会再探测一次会话，而刚登出时这次探测必然 401，属于无意义的请求。
  if (!state.session) { renderLogin(); return; }
  runCleanup();
  renderApp();
}

// ---------------- 启动 ----------------
window.addEventListener('DOMContentLoaded', boot);
