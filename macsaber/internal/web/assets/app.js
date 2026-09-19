// app.js —— mac军刀 前端入口：登录/初始化 + 分类 → 工具 → 通用表单 → 结果/任务进度。
// 无构建步骤：原生 ESM，直接由 Go 内嵌下发。
import { api, ApiError, pollTask } from './js/api.js';
import { el, clear, toast } from './js/ui.js';
import { renderForm } from './js/form.js';
import { renderResult, renderTask } from './js/result.js';
import { renderCategories, renderToolCards } from './js/cards.js';

const state = { categories: [], active: '', tool: null };

const screenAuth = document.getElementById('screen-auth');
const screenApp = document.getElementById('screen-app');
const catsBox = document.getElementById('cats');
const toolPanel = document.getElementById('tool-panel');
const resultPanel = document.getElementById('result-panel');
const whoami = document.getElementById('whoami');
const logoutBtn = document.getElementById('btn-logout');
const authTitle = document.getElementById('auth-title');
const authHint = document.getElementById('auth-hint');
const authForm = document.getElementById('form-auth');
const authBtn = document.getElementById('btn-auth');
const confirmField = document.getElementById('fld-confirm');

async function boot() {
  try {
    const st = await api.setupStatus();
    document.getElementById('brand').textContent = st.name || 'mac军刀';
    // 先试会话：已登录就直接进主界面，避免登录页闪一下。
    try {
      await enterApp();
      return;
    } catch {
      showAuth(st.inited);
    }
  } catch (e) {
    showAuth(false);
    toast('读取服务状态失败：' + e.message);
  }
}

function showAuth(inited) {
  screenAuth.classList.remove('hidden');
  screenApp.classList.add('hidden');
  authTitle.textContent = inited ? '登录' : '初始化';
  authHint.textContent = inited ? '输入本机口令进入。' : '首次使用请设置本机用户名与口令。';
  authBtn.textContent = inited ? '登录' : '建立';
  confirmField.classList.toggle('hidden', inited);
  authForm.querySelector('input[name=confirm]').required = !inited;
}

authForm.addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = new FormData(authForm);
  authBtn.disabled = true;
  try {
    if (authTitle.textContent === '初始化') {
      await api.setup(f.get('user'), f.get('password'), f.get('confirm'));
    } else {
      await api.login(f.get('user'), f.get('password'));
    }
    authForm.reset();
    await enterApp();
  } catch (e) {
    toast(e.message || '操作失败');
  } finally {
    authBtn.disabled = false;
  }
});

logoutBtn.addEventListener('click', async () => {
  try { await api.logout(); } catch { /* 会话已失效也算退出成功 */ }
  location.reload();
});

async function enterApp() {
  const sess = await api.session();
  whoami.textContent = sess.user || '';
  logoutBtn.classList.remove('hidden');
  screenAuth.classList.add('hidden');
  screenApp.classList.remove('hidden');
  const data = await api.tools();
  state.categories = data.categories || [];
  if (state.categories.length === 0) {
    clear(toolPanel);
    toolPanel.appendChild(el('p', { class: 'hint', text: '还没有注册任何工具。' }));
    return;
  }
  // hash 路由：刷新后仍停在同一个分类上。
  const want = decodeURIComponent(location.hash.replace(/^#\/?/, ''));
  const pick = state.categories.find((c) => c.id === want) || state.categories[0];
  state.active = pick.id;
  drawCats();
  drawTools(pick);
}

function drawCats() {
  renderCategories(catsBox, state.categories, state.active, (id) => {
    state.active = id;
    location.hash = '/' + id;
    drawCats();
    drawTools(state.categories.find((c) => c.id === id));
  });
}

function drawTools(category) {
  state.tool = null;
  clear(resultPanel);
  renderToolCards(toolPanel, category, (t) => openTool(t));
}

function openTool(t) {
  state.tool = t;
  clear(resultPanel);
  renderForm(t, toolPanel, (tool, body, ctx) => runTool(tool, body, ctx));
}

async function runTool(tool, body, ctx) {
  ctx.button.disabled = true;
  ctx.button.textContent = tool.async ? '已提交…' : '执行中…';
  clear(resultPanel);
  try {
    const resp = await api.run(tool.id, body);
    if (resp.accepted && resp.task_id) {
      await followTask(resp.task_id);
    } else {
      renderResult(resultPanel, resp.result || { ok: false, msg: '没有返回结果' });
    }
  } catch (e) {
    renderResult(resultPanel, {
      ok: false,
      msg: e instanceof ApiError ? e.message : String(e.message || e),
    });
    toast(e.message || '执行失败');
  } finally {
    ctx.button.disabled = !tool.available;
    ctx.button.textContent = tool.async ? '执行（后台任务）' : '执行';
  }
}

async function followTask(id) {
  const task = await pollTask(id, (t) => {
    renderTask(resultPanel, t, { onCancel: cancelTask });
  });
  renderTask(resultPanel, task, { onCancel: cancelTask });
  if (task.status === 'succeeded') toast('后台任务完成');
}

async function cancelTask(id) {
  try {
    await api.cancel(id);
    toast('已请求取消');
  } catch (e) {
    toast(e.message || '取消失败');
  }
}

boot();
