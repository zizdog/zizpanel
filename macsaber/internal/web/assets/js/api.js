// api.js —— 后端接口封装。所有写操作自动带 CSRF 双提交头。
export const CSRF_COOKIE = 'ms_csrf';

export function cookie(name) {
  const parts = document.cookie ? document.cookie.split('; ') : [];
  for (const p of parts) {
    const i = p.indexOf('=');
    if (i > 0 && p.slice(0, i) === name) return decodeURIComponent(p.slice(i + 1));
  }
  return '';
}

export class ApiError extends Error {
  constructor(status, msg) {
    super(msg || ('请求失败 ' + status));
    this.status = status;
  }
}

async function req(method, path, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  if (method !== 'GET' && method !== 'HEAD') {
    opt.headers['X-CSRF-Token'] = cookie(CSRF_COOKIE);
  }
  const res = await fetch(path, opt);
  let data = null;
  const text = await res.text();
  if (text) {
    try { data = JSON.parse(text); } catch { data = { ok: false, msg: text.slice(0, 300) }; }
  }
  if (!res.ok) throw new ApiError(res.status, (data && data.msg) || '');
  return data;
}

export const api = {
  version: () => req('GET', 'api/version'),
  setupStatus: () => req('GET', 'api/setup/status'),
  setup: (user, password, confirm) => req('POST', 'api/setup', { user, password, confirm }),
  login: (user, password) => req('POST', 'api/login', { user, password }),
  logout: () => req('POST', 'api/logout', {}),
  session: () => req('GET', 'api/session'),
  tools: () => req('GET', 'api/tools'),
  run: (id, params) => req('POST', 'api/tools/' + encodeURIComponent(id) + '/run', params),
  task: (id) => req('GET', 'api/tasks/' + encodeURIComponent(id)),
  tasks: () => req('GET', 'api/tasks'),
  cancel: (id) => req('POST', 'api/tasks/' + encodeURIComponent(id) + '/cancel', {}),
};

// pollTask 轮询任务直到结束；onTick 用来刷新进度条。
export async function pollTask(id, onTick, intervalMs = 700) {
  for (;;) {
    const data = await api.task(id);
    const task = data.task;
    onTick(task);
    if (task.status !== 'running') return task;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
}
