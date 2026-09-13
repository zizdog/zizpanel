// app.js —— 应用入口：状态、路由、登录/初始化、布局外壳。
//
// 路由用 hash（#/xxx）而非 History API：面板可能被放在子路径或反代后面，
// hash 路由不需要服务端配合改写，也避免了刷新 404。

import { api, ApiError } from './api.js';
import { h, clear, toast, $, modal, bytes, duration, esc } from './ui.js';
import { DashboardView, MonitorView, SettingsView, ComingSoonView } from './views.js';
import { SitesView } from './sites.js';
import { ServicesView } from './services.js';
import { AppsView } from './apps.js';
import { FilesView } from './files.js';
import { TerminalView } from './terminal.js';
import { CronView } from './cron.js';
import { LogsView } from './logs.js';
import { DatabaseView } from './database.js';
import { DockerView } from './docker.js';

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
  { id: 'monitor', title: '系统监控', icon: '📈', view: MonitorView },
  { group: '网站' },
  { id: 'sites', title: '网站管理', icon: '🌐', view: SitesView },
  { id: 'database', title: '数据库', icon: '🗄️', view: DatabaseView },
  { group: '服务器' },
  { id: 'services', title: '服务管理', icon: '⚙️', view: ServicesView },
  { id: 'apps', title: '应用市场', icon: '🧩', view: AppsView },
  { id: 'docker', title: 'Docker', icon: '🐳', view: DockerView },
  { group: '运维' },
  { id: 'files', title: '文件管理', icon: '📁', view: FilesView },
  { id: 'terminal', title: 'Web 终端', icon: '🖥️', view: TerminalView },
  { id: 'cron', title: '计划任务', icon: '⏰', view: CronView },
  { id: 'logs', title: '日志中心', icon: '📜', view: LogsView },
  { group: '系统' },
  { id: 'audit', title: '操作审计', icon: '🧾', view: ComingSoonView, phase: 'P1' },
  { id: 'settings', title: '面板设置', icon: '🔧', view: SettingsView },
];

const NAV_BY_ID = Object.fromEntries(NAV.filter((n) => n.id).map((n) => [n.id, n]));

// ---------------- 主题 ----------------
function initTheme() {
  const saved = localStorage.getItem('zp-theme');
  const theme = saved || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
  document.documentElement.dataset.theme = theme;
}

function toggleTheme() {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = next;
  localStorage.setItem('zp-theme', next);
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
  const route = currentRoute();
  const item = NAV_BY_ID[route] || NAV_BY_ID.dashboard;

  const nav = h('nav.nav');
  NAV.forEach((n) => {
    if (n.group) { nav.appendChild(h('div.nav-group', { text: n.group })); return; }
    const active = n.id === item.id;
    nav.appendChild(h(`div.nav-item${active ? '.active' : ''}`, {
      onclick: () => { location.hash = '#/' + n.id; document.body.classList.remove('nav-open'); },
    }, [
      h('span.ico', { text: n.icon }),
      h('span', { text: n.title }),
      n.phase ? h('span.badge', { text: n.phase }) : null,
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
      h('div', { text: '登录账号：' + (user.username || '-') }),
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
    h('span.pill.' + (user.totp_enabled ? 'ok' : 'warn'), {
      text: user.totp_enabled ? '2FA 已开启' : '2FA 未开启',
      title: '两步验证状态，可在「面板设置」中调整',
    }),
    h('button.btn.btn-ghost.btn-icon', { text: document.documentElement.dataset.theme === 'light' ? '🌙' : '☀️', title: '切换主题', onclick: () => { toggleTheme(); renderApp(); } }),
    h('button.btn.btn-ghost.btn-icon', { text: '⟳', title: '刷新', onclick: () => render() }),
    h('button.btn.btn-ghost.btn-icon', { text: '⏻', title: '退出登录', onclick: doLogout }),
  ]);

  const main = h('main.main', [topbar, content]);
  const layout = h('div.layout', [sidebar, main, h('div.overlay', { onclick: () => document.body.classList.remove('nav-open') })]);

  mount(layout);
  document.body.classList.remove('nav-open');

  // 渲染前先清理上一个页面的长连接（SSE / 定时器），避免叠加泄漏
  runCleanup();

  // 渲染当前页面
  const view = item.view;
  if (typeof view === 'function' && view.length >= 2) {
    view(content, { item, topbar, pageTitle, onLeave: registerCleanup });
  } else {
    view(content, { item });
  }
  document.title = `${item.title} · ZizPanel`;
}

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

function currentRoute() {
  const m = location.hash.match(/^#\/([a-z0-9_-]+)/i);
  return m ? m[1] : 'dashboard';
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
