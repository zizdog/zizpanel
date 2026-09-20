// app.js —— 应用入口：状态、路由、登录/初始化、布局外壳。
//
// 路由用 hash（#/xxx）而非 History API：面板可能被放在子路径或反代后面，
// hash 路由不需要服务端配合改写，也避免了刷新 404。

import { api, ApiError } from './api.js';
import { h, clear, toast, $ } from './ui.js';
import { DashboardView, SettingsView } from './views.js';
import { SystemSettingsView } from './systemsettings.js';
import { SitesView } from './sites.js';
import { ReverseProxyView } from './reverseproxy.js';
import { CertsView } from './certs.js';
import { AppsView } from './apps.js';
import { FilesView } from './files.js';
import { TerminalView, destroyTerminal } from './terminal.js';
import { CronView } from './cron.js';
import { LogsHubView } from './logshub.js';
import { NavView } from './nav.js';
import { DisksView } from './disks.js';
import { DatabaseView } from './database.js';
import { DockerView } from './docker.js';
import { startUpgradeWatcher, hasUpdate } from './update.js';
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
  // 「导航页」＝ sun-panel 风格的图标网格首页（点卡片新标签打开）。
  // 放在「总览」组：它是用户每天第一眼看的页面，不是运维工具。
  // 数据量极小，直接做进面板（复用鉴权/备份/审计），不引入第二套运行时。
  { id: 'nav', title: '导航页', icon: '🧭', view: NavView },
  // 「系统监控」已改成「mac设置」（2026-09 用户要求把显示名从「系统设置」改成
  // 「mac设置」：原名与「面板设置」并列时歧义太大）。**只改显示名**，
  // 路由 id 仍是 'system'，`#/system` 与所有旧链接照旧可用。
  { id: 'system', title: 'mac设置', icon: '⚙️', view: SystemSettingsView },
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
  // 「磁盘管理」：看磁盘信息 + 挂载/卸载 + 开机自动挂载 + 受控初始化镜像盘。
  // 放在「系统」组（与「面板设置 / 日志」同组）：它是 OS 级的存储工具，
  // 不是"面板自己的设置"，也不是「mac设置」那种把 macOS 配成服务器的动作集。
  { id: 'disks', title: '磁盘管理', icon: '💾', view: DisksView },
  // 「权限」（逐项申请 macOS 授权）2026-09-20 从侧栏搬进「面板设置」的 Tab；
  // 旧 hash `#/permissions` 由下面 ROUTE_TARGET 的别名兜住，书签不会白屏。
  // 「日志」把原「日志中心」与「操作审计」合并成一页两个 Tab（见 logshub.js）：
  // 侧栏只剩一个入口，旧的 #/logs 与 #/audit 仍分别落到对应 Tab（见 ROUTE_TARGET）。
  //
  // 「检查更新」在被用户要求**收回**「面板设置」（第 4 个 Tab，
  // 排在「访问与安全 / 文件与终端 / 账号与两步验证」之后）——它曾在侧栏独立存在过，
  // 而设置里也留了一份入口，两处重复让用户困惑。现在侧栏只有「面板设置」，
  // 升级入口在它里面；旧 hash（#/update、#/about、#/settings/about）走下面的别名表。
  { id: 'settings', title: '面板设置', icon: '🛠️', view: SettingsView },
  { id: 'logs', title: '日志', icon: '📜', view: LogsHubView },
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
  // 「检查更新」的 hash 变过两次，**历史写法一个都不能 404**（文档、书签、
  // 侧栏徽标点进来的地址都在用）：
  //   #/update            —— 侧栏独立页时代的地址
  //   #/about / #/upgrade —— 更早的"关于与运维"单段写法
  //   #/settings/about    —— 页内 Tab 时代的两段写法，见下面的 SUB_ROUTE_TARGET
  // 一律落到「面板设置」的 update Tab（renderApp 把 tab 传给 SettingsView）。
  update: { id: 'settings', tab: 'update' },
  about: { id: 'settings', tab: 'update' },
  upgrade: { id: 'settings', tab: 'update' },
  // 「权限」2026-09-20 从侧栏搬进「面板设置」的 Tab，旧 hash 必须继续可用。
  permissions: { id: 'settings', tab: 'permissions' },
};

// 两段式 hash（#/<版块>/<Tab>）的别名表，键是 "版块/Tab"。
const SUB_ROUTE_TARGET = {
  'settings/about': { id: 'settings', tab: 'update' },
  // 2026-09-19：面板设置的两个 Tab 被删/合并，但旧 hash（书签、文档、别的页面
  // 里的链接）一个都不能白屏：
  //   #/settings/limits   —— 「上传与执行限制」Tab（已删，入口只在「调整配置」里）
  //   #/settings/terminal —— 「文件与终端」Tab（已并入「访问与安全」）
  // 两者都落到合并后的「访问与安全」。
  'settings/limits': { id: 'settings', tab: 'access' },
  'settings/terminal': { id: 'settings', tab: 'access' },
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

// ---------------- 侧栏折叠（只显示图标） ----------------
//
// 为什么记在 localStorage：用户折叠侧栏是"我习惯这样用"的长期偏好，
// 刷新一次就弹回来会让他每次都要再折一遍。
const SIDEBAR_KEY = 'zp-sidebar-collapsed';

function sidebarCollapsed() {
  try { return localStorage.getItem(SIDEBAR_KEY) === '1'; } catch { return false; }
}

// applySidebarState 把折叠状态落到 body 类名与那颗开关按钮上。
// 单独抽出来是因为 renderApp() 每次路由切换都会重建侧栏 DOM，重建后必须**原地**补状态。
function applySidebarState() {
  const on = sidebarCollapsed();
  document.body.classList.toggle('zp-sidebar-collapsed', on);
  const btn = document.querySelector('.sidebar-toggle');
  if (btn) {
    btn.textContent = on ? '»' : '«';
    btn.title = on ? '展开侧栏（显示文字）' : '折叠侧栏（只显示图标）';
    btn.setAttribute('aria-label', on ? '展开侧栏' : '折叠侧栏');
  }
}

function toggleSidebar() {
  try { localStorage.setItem(SIDEBAR_KEY, sidebarCollapsed() ? '0' : '1'); } catch { /* 存不了就本次会话生效 */ }
  applySidebarState();
}

// footerStatus 给底部 footer 的一句状态（版本号在 footer 里单独一列）。
function footerStatus() {
  const c = state.session?.config || {};
  const scheme = c.tls ? 'HTTPS' : 'HTTP';
  return c.listen ? `面板运行正常 · 监听 ${c.listen} · ${scheme}` : '面板运行正常';
}

// ---------------- 顶栏「2FA」状态按钮 ----------------
//
// 用户要求：内容改成只写「2FA」，状态**只用颜色**表达，
// 点击跳到「面板设置 → 账号与两步验证」里的 2FA 那一节。
// 三态各一种颜色，title 说明到底是什么意思：
//   ok     已开启（绿）
//   warn   未开启（橙；面板建议开启）
//   danger 需注意 / 未复核（红）—— 会话里读不到 totp_enabled 字段时如实说
//          "无法复核"，**绝不猜成"未开启"**（第三节：读不到就拒绝，不猜默认值）
function totpBadge(user) {
  const v = user ? user.totp_enabled : undefined;
  if (v === true) {
    return { tone: 'ok', title: '两步验证（2FA）已开启。点击前往「面板设置 → 账号与两步验证」调整' };
  }
  if (v === false) {
    return { tone: 'warn', title: '两步验证（2FA）未开启 —— 建议开启。点击前往「面板设置 → 账号与两步验证」' };
  }
  return { tone: 'danger', title: '两步验证（2FA）需注意：会话数据里读不到该字段，无法复核（不猜状态）。点击前往查看' };
}

// ---------------- 「跳到设置页的某一节」 ----------------
//
// 设置页的 Tab 内容是**异步**渲染的（如 renderAccount 里 await 了登录会话），
// 点击那一刻目标元素还不存在。所以这里只记下"渲染完成后要滚到哪个锚点"，
// 由视图渲染完成后消费（views.js 的 renderAccount → consumePendingAnchor）。
let pendingAnchor = '';

export function consumePendingAnchor() {
  const a = pendingAnchor;
  pendingAnchor = '';
  return a;
}

// goto2FASettings 跳到「面板设置 → 账号与两步验证」，并把 2FA 卡片滚进视野。
function goto2FASettings() {
  pendingAnchor = 'zp-2fa-section';
  const wanted = '#/settings/account';
  if ((location.hash || '#/dashboard') === wanted) {
    // hash 没变不会触发 hashchange —— 手动重渲染一次，让新视图消费锚点。
    render();
    return;
  }
  location.hash = wanted;
}

function renderApp() {
  // 路由先过别名表：`#/services` 会落到 apps 版块的「已安装」Tab（见 ROUTE_TARGET）；
  // `#/apps/docker` 这类带 Tab 的 hash 由 routeFor 解析出 tab 传进页面。
  const target = routeFor();
  const item = NAV_BY_ID[target.id] || NAV_BY_ID.dashboard;

  const nav = h('nav.nav');
  NAV.forEach((n) => {
    if (n.group) { nav.appendChild(h('div.nav-group', { text: n.group })); return; }
    const active = n.id === item.id;
    // 有新版本时的小红点：挂在「面板设置」上（升级入口就在它的「检查更新」Tab 里）。
    // **刷新后仍然在**：检测结果落在 localStorage，见 update.js 的 updateInfo()。
    // 为什么不干脆去掉：删掉侧栏入口后，这是"有新版本"唯一常驻可见的信号 ——
    // 没了它，用户不点进设置就永远不知道能升级。
    const dot = n.id === 'settings'
      ? h('span.badge.zp-update-dot', {
        dataset: { testid: 'zp-update-badge' },
        text: '新',
        title: '面板设置 → 检查更新：发现新版本',
        style: { background: 'var(--danger)', color: '#fff', border: '1px solid transparent' },
        hidden: !hasUpdate(),
      })
      : null;
    nav.appendChild(h(`div.nav-item${active ? '.active' : ''}`, {
      // 折叠后只剩图标，所以 title 是用户唯一能看到的名称提示。
      title: n.title,
      onclick: () => { location.hash = '#/' + n.id; document.body.classList.remove('nav-open'); },
    }, [
      h('span.ico', { text: n.icon }),
      h('span', { text: n.title }),
      n.phase ? h('span.badge', { text: n.phase }) : null,
      dot,
    ]));
  });

  const user = state.session?.user || {};
  const sidebarToggle = h('button.btn.btn-ghost.btn-icon.sidebar-toggle', {
    text: sidebarCollapsed() ? '»' : '«',
    title: sidebarCollapsed() ? '展开侧栏（显示文字）' : '折叠侧栏（只显示图标）',
    onclick: (e) => { e.stopPropagation(); toggleSidebar(); },
  });
  const sidebar = h('aside.sidebar', [
    h('div.sidebar-head', [
      h('div.mark', { text: 'Z' }),
      h('div.sidebar-brand', [
        h('div.title', { text: 'ZizPanel' }),
        h('div.ver', { text: 'v' + (state.session?.version || '') }),
      ]),
      sidebarToggle,
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
    // 2FA 状态：内容只有「2FA」，状态靠颜色（见 totpBadge），点击去设置里那一节。
    (() => {
      const t = totpBadge(user);
      return h('button.pill.' + t.tone + '.zp-2fa-pill', {
        text: '2FA',
        title: t.title,
        'aria-label': t.title,
        dataset: { testid: 'zp-2fa-pill', state: t.tone },
        onclick: goto2FASettings,
      });
    })(),
    (() => { const f = themeButtonFace(); return h('button.btn.btn-ghost.btn-icon', { text: f.icon, title: f.title, onclick: () => { toggleTheme(); renderApp(); } }); })(),
    // 刷新：原来是 '⟳'（U+27F3），在顶栏里比其它图标明显偏小、发虚。
    // 换成与主题按钮同一套的 emoji 字符，并保留 title / 说明快捷键。
    h('button.btn.btn-ghost.btn-icon.zp-refresh-btn', {
      text: '🔄',
      title: '刷新当前页面（浏览器快捷键：⌘/Ctrl + R）',
      'aria-label': '刷新',
      onclick: () => render(),
    }),
    h('button.btn.btn-ghost.btn-icon', { text: '⏻', title: '退出登录', onclick: doLogout }),
  ]);

  // 底部 footer：版本号 + 一句状态。放在 main 的**正常流**里（不是 fixed），
  // 所以它永远不会盖住 content 的内容 —— 内容短时它在视口底部，内容长时它在最下面。
  const footer = h('footer.footer', [
    h('span.footer-strong', { text: 'ZizPanel' }),
    h('span', { text: 'v' + (state.session?.version || '未知') }),
    h('span.footer-sep', { text: '·' }),
    h('span', { text: footerStatus() }),
  ]);

  const main = h('main.main', [topbar, content, footer]);
  const layout = h('div.layout', [sidebar, main, h('div.overlay', { onclick: () => document.body.classList.remove('nav-open') })]);

  mount(layout);
  document.body.classList.remove('nav-open');
  // 侧栏折叠状态：renderApp() 每次都重建侧栏 DOM，重建后必须原地补回折叠类与按钮外观。
  applySidebarState();

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
  // 终端是**跨路由保活**的（见 terminal.js）：登出是"必须销毁"的时刻之一，
  // 否则会把一条能执行本机 shell 的连接留给下一位登录者。
  destroyTerminal();
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
