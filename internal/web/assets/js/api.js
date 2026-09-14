// api.js —— 与后端 /api/v1 通信的薄封装。
//
// 关键点：
//   - 会话走 HttpOnly Cookie，前端不需要管理 token；
//   - 写操作要带上 X-CSRF-Token（双提交模式，值从可读的 zp_csrf Cookie 取）；
//   - 统一错误处理：抛出带 message 的 Error，由调用方决定怎么提示。

function readCookie(name) {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
  return m ? decodeURIComponent(m[1]) : '';
}

// API_BASE 用相对路径而不是 "/api/v1"。
//
// 原因：面板有两种访问方式 —— 直连 https://<IP>:8443/ 和经 nginx 的
// 子路径 http://localhost/_panel/。绝对路径 "/api/v1" 在后一种情况下会
// 打到 nginx 根路径而非面板，导致接口 404。
// 相对路径 "./api/v1" 在两种部署下都能正确解析。
// 注意：(function(){...})() 形式，避免依赖 DOM 就绪时机。
const API_BASE = new URL('api/v1/', document.baseURI).pathname.replace(/\/$/, '');

// apiURL 拼出接口地址（相对当前部署路径）。
export function apiURL(path) {
  return `${API_BASE}/${String(path).replace(/^\/+/, '')}`;
}

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function request(method, path, body, opts = {}) {
  const headers = { Accept: 'application/json' };
  const init = { method, headers, credentials: 'same-origin' };

  if (body !== undefined && body !== null) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  if (method !== 'GET' && method !== 'HEAD') {
    const csrf = readCookie('zp_csrf');
    if (csrf) headers['X-CSRF-Token'] = csrf;
  }

  let res;
  try {
    res = await fetch(path, init);
  } catch (e) {
    throw new ApiError('无法连接面板服务，请检查网络或面板是否在运行', 0);
  }

  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { /* 非 JSON 响应 */ }
  }

  if (!res.ok) {
    const msg = (data && data.msg) || `${res.status} ${res.statusText}`;
    throw new ApiError(msg, res.status);
  }
  if (data && data.ok === false) {
    throw new ApiError(data.msg || '请求失败', res.status);
  }
  return data ? data.data : null;
}

export const api = {
  get: (p, o) => request('GET', p, null, o),
  post: (p, b, o) => request('POST', p, b ?? {}, o),
  del: (p, b, o) => request('DELETE', p, b, o),

  // 上传文件走 multipart，不能走 request()（它会强制 JSON Content-Type，
  // 而 multipart 的 boundary 必须由浏览器自己生成）。
  upload: async (path, file, fieldName = 'package') => {
    const fd = new FormData();
    fd.append(fieldName, file, file.name);
    const headers = { Accept: 'application/json' };
    const csrf = readCookie('zp_csrf');
    if (csrf) headers['X-CSRF-Token'] = csrf;
    let res;
    try {
      res = await fetch(path, { method: 'POST', headers, body: fd, credentials: 'same-origin' });
    } catch {
      throw new ApiError('上传失败：无法连接面板服务', 0);
    }
    const text = await res.text();
    let data = null;
    if (text) { try { data = JSON.parse(text); } catch { /* 非 JSON */ } }
    if (!res.ok) throw new ApiError((data && data.msg) || `${res.status} ${res.statusText}`, res.status);
    if (data && data.ok === false) throw new ApiError(data.msg || '上传失败', res.status);
    return data ? data.data : null;
  },

  // ---- 具体接口 ----
  ping: () => request('GET', `${API_BASE}/ping`),
  setupStatus: () => request('GET', `${API_BASE}/setup/status`),
  setup: (username, password) => request('POST', `${API_BASE}/setup`, { username, password }),
  login: (username, password) => request('POST', `${API_BASE}/login`, { username, password }),
  loginTOTP: (challenge, code) => request('POST', `${API_BASE}/login/totp`, { challenge, code }),
  logout: () => request('POST', `${API_BASE}/logout`, {}),
  session: () => request('GET', `${API_BASE}/session`),
  systemInfo: () => request('GET', `${API_BASE}/system/info`),
  processes: (sort = 'cpu', limit = 12) =>
    request('GET', `${API_BASE}/system/processes?sort=${sort}&limit=${limit}`),
  // 审计：列表（带检索与游标分页）、筛选项、导出。
  // audit() 保留原样给仪表盘用（它只关心最近的记录）。
  audit: (limit = 60) => request('GET', `${API_BASE}/audit?limit=${limit}`),
  auditQuery: (params = {}) => {
    const p = new URLSearchParams();
    Object.entries(params).forEach(([k, v]) => {
      if (v !== undefined && v !== null && v !== '') p.set(k, String(v));
    });
    return request('GET', `${API_BASE}/audit?${p.toString()}`);
  },
  auditFacets: () => request('GET', `${API_BASE}/audit/facets`),
  getSettings: () => request('GET', `${API_BASE}/settings`),

  // ---- 在线升级 ----
  upgradeStatus: () => request('GET', `${API_BASE}/system/upgrade`),
  upgradeCheck: (source) => request('POST', `${API_BASE}/system/upgrade/check`, { source }),
  upgradeStage: (source) => request('POST', `${API_BASE}/system/upgrade/stage`, { source }),
  upgradeApply: () => request('POST', `${API_BASE}/system/upgrade/apply`, {}),
  upgradeDismiss: () => request('POST', `${API_BASE}/system/upgrade/dismiss`, {}),
  upgradeUpload: (file) => api.upload(`${API_BASE}/system/upgrade/upload`, file),
  saveSettings: (patch) => request('POST', `${API_BASE}/settings`, patch),
  changePassword: (oldPwd, newPwd) =>
    request('POST', `${API_BASE}/account/password`, { old: oldPwd, new: newPwd }),
  // 改用户名：要当前密码确认；成功后会话仍然有效（会话按 user_id 关联）
  renameUser: (username, password) =>
    request('POST', `${API_BASE}/account/username`, { username, password }),
  totpSetup: () => request('POST', `${API_BASE}/account/totp/setup`, {}),
  totpEnable: (secret, code) => request('POST', `${API_BASE}/account/totp/enable`, { secret, code }),
  totpDisable: (password) => request('POST', `${API_BASE}/account/totp/disable`, { password }),
  sessions: () => request('GET', `${API_BASE}/account/sessions`),

  // ---- 站点管理 ----
  sites: () => request('GET', `${API_BASE}/sites`),
  siteCreate: (payload) => request('POST', `${API_BASE}/sites`, payload),
  siteReloadAll: () => request('POST', `${API_BASE}/sites/reload`, {}),
  site: (domain) => request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}`),
  siteUpdate: (domain, patch) => request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}`, patch),
  siteDelete: (domain, removeFiles) =>
    request('DELETE', `${API_BASE}/sites/${encodeURIComponent(domain)}?remove_files=${removeFiles ? 1 : 0}`),
  siteSSL: (domain, payload) => request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}/ssl`, payload),
  siteSSLDisable: (domain) => request('DELETE', `${API_BASE}/sites/${encodeURIComponent(domain)}/ssl`),
  siteCheck: (domain) => request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}/check`),
  siteLog: (domain, kind = 'access', lines = 200) =>
    request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}/log?kind=${kind}&lines=${lines}`),

  // ---- 服务管理 ----
  services: (health = true) => request('GET', `${API_BASE}/services?health=${health ? 1 : 0}`),
  service: (name) => request('GET', `${API_BASE}/services/${encodeURIComponent(name)}`),
  serviceCreate: (payload) => request('POST', `${API_BASE}/services`, payload),
  serviceUpdate: (name, payload) => request('POST', `${API_BASE}/services/${encodeURIComponent(name)}`, payload),
  serviceForget: (name) => request('DELETE', `${API_BASE}/services/${encodeURIComponent(name)}`),
  serviceUninstall: (name) => request('DELETE', `${API_BASE}/services/${encodeURIComponent(name)}/uninstall`),
  serviceAction: (name, action) =>
    request('POST', `${API_BASE}/services/${encodeURIComponent(name)}/${action}`, {}),
  serviceLogs: (name, lines = 300) =>
    request('GET', `${API_BASE}/services/${encodeURIComponent(name)}/logs?lines=${lines}`),

  // ---- 任务中心（安装/卸载的实时进度）----
  //
  // 任务只活在面板进程内存里，进程一重启就清空 —— 所以这里没有任何"持久化"接口，
  // 前端也不该把任务 id 存进 localStorage 之类的地方。
  tasks: () => request('GET', `${API_BASE}/tasks`),
  task: (id, after = 0, limit = 800) =>
    request('GET', `${API_BASE}/tasks/${encodeURIComponent(id)}?after=${after}&limit=${limit}`),
  taskCancel: (id) => request('POST', `${API_BASE}/tasks/${encodeURIComponent(id)}/cancel`, {}),
  // 进度用 SSE。URL 必须走 apiURL 拼相对路径：面板可能挂在 /_panel 子路径下。
  taskStreamURL: (id) => apiURL(`tasks/${encodeURIComponent(id)}/stream`),

  // ---- 应用市场 ----
  market: () => request('GET', `${API_BASE}/market`),
  marketPreflight: (id) => request('GET', `${API_BASE}/market/${encodeURIComponent(id)}/preflight`),
  marketInstall: (id) => request('POST', `${API_BASE}/market/${encodeURIComponent(id)}/install`, {}),
  installLNMP: () => request('POST', `${API_BASE}/market/install-lnmp`, {}),
  installPhpMyAdmin: () => request('POST', `${API_BASE}/market/install-phpmyadmin`, {}),
  installQwenTTS: (opts) => request('POST', `${API_BASE}/market/install-qwentts`, opts || {}),
  installVoiceReceiver: (opts) => request('POST', `${API_BASE}/market/install-voicereceiver`, opts || {}),
  installIOPaint: () => request('POST', `${API_BASE}/market/install-iopaint`, {}),
  // 更换音色接收端共享密钥。token 留空 = 让面板生成一个新的随机密钥。
  changeReceiverToken: (token) => request('POST', `${API_BASE}/voice/receiver/token`, { token: token || '' }),

  // Qwen3 TTS 的双模型切换。
  // Base 与 CustomVoice 能力互斥（克隆 vs 预置音色），两个都要装、按需切换。
  qwenModels: () => request('GET', `${API_BASE}/qwen/models`),
  qwenSwitchModel: (name) => request('POST', `${API_BASE}/qwen/model`, { name }),
  qwenUnloadModel: (name) => request('DELETE', `${API_BASE}/qwen/model?name=${encodeURIComponent(name)}`),
  adoptable: () => request('GET', `${API_BASE}/adoptable`),
  adopt: (payload) => request('POST', `${API_BASE}/adopt`, payload),
  freePorts: (from = 9000) => request('GET', `${API_BASE}/ports/free?from=${from}`),

  // ---- 数据库管理 ----
  database: () => request('GET', `${API_BASE}/database`),
  databaseTables: (name) => request('GET', `${API_BASE}/database/tables?name=${encodeURIComponent(name)}`),
  databaseCreate: (payload) => request('POST', `${API_BASE}/database`, payload),
  databaseDrop: (name) => request('DELETE', `${API_BASE}/database/${encodeURIComponent(name)}`),
  databaseUserCreate: (payload) => request('POST', `${API_BASE}/database/user`, payload),
  databaseUserDrop: (user, host) =>
    request('DELETE', `${API_BASE}/database/user?user=${encodeURIComponent(user)}&host=${encodeURIComponent(host)}`),
  databaseUserPassword: (user, host, password) =>
    request('POST', `${API_BASE}/database/user/password`, { user, host, password }),
  databaseGrant: (payload) => request('POST', `${API_BASE}/database/grant`, payload),
  databaseGrants: (user, host) =>
    request('GET', `${API_BASE}/database/grants?user=${encodeURIComponent(user)}&host=${encodeURIComponent(host)}`),
  databaseQuery: (database, sql) => request('POST', `${API_BASE}/database/query`, { database, sql }),
  databaseDump: (name) => request('POST', `${API_BASE}/database/dump`, { name }),
  databaseImport: (name, file) => request('POST', `${API_BASE}/database/import`, { name, file }),
  databaseBackups: () => request('GET', `${API_BASE}/database/backups`),

  // ---- 日志中心 ----
  logs: () => request('GET', `${API_BASE}/logs`),
  logRead: (query) => request('GET', `${API_BASE}/logs/read?${query}`),
  logTruncate: (key) => request('POST', `${API_BASE}/logs/truncate`, { key }),
  logDelete: (key) => request('DELETE', `${API_BASE}/logs?key=${encodeURIComponent(key)}`),

  // ---- 计划任务 ----
  cron: () => request('GET', `${API_BASE}/cron`),
  cronGet: (id) => request('GET', `${API_BASE}/cron/${id}`),
  cronCreate: (payload) => request('POST', `${API_BASE}/cron`, payload),
  cronUpdate: (id, payload) => request('POST', `${API_BASE}/cron/${id}`, payload),
  cronDelete: (id) => request('DELETE', `${API_BASE}/cron/${id}`),
  cronToggle: (id, enabled) => request('POST', `${API_BASE}/cron/${id}/toggle`, { enabled }),
  cronRun: (id) => request('POST', `${API_BASE}/cron/${id}/run`, {}),
  cronLog: (id, lines = 300) => request('GET', `${API_BASE}/cron/${id}/log?lines=${lines}`),
  cronSync: () => request('POST', `${API_BASE}/cron/sync`, {}),
  cronPreview: (schedule) =>
    request('GET', `${API_BASE}/cron/preview?schedule=${encodeURIComponent(schedule)}`),
  backups: () => request('GET', `${API_BASE}/backups`),

  // ---- 文件管理 ----
  files: (path, hidden = false) =>
    request('GET', `${API_BASE}/files?path=${encodeURIComponent(path)}&hidden=${hidden ? 1 : 0}`),
  fileRead: (path) => request('GET', `${API_BASE}/files/read?path=${encodeURIComponent(path)}`),
  fileWrite: (path, content) => request('POST', `${API_BASE}/files/write`, { path, content }),
  fileMkdir: (path) => request('POST', `${API_BASE}/files/mkdir`, { path }),
  fileTouch: (path) => request('POST', `${API_BASE}/files/touch`, { path }),
  fileRename: (from, to) => request('POST', `${API_BASE}/files/rename`, { from, to }),
  fileCopy: (from, to) => request('POST', `${API_BASE}/files/copy`, { from, to }),
  fileChmod: (path, mode, recursive = false) =>
    request('POST', `${API_BASE}/files/chmod`, { path, mode, recursive }),
  fileDelete: (paths, recursive) => request('POST', `${API_BASE}/files/delete`, { paths, recursive }),
  fileCompress: (dir, names, format, output) =>
    request('POST', `${API_BASE}/files/compress`, { dir, names, format, output }),
  fileExtract: (archive, dest) => request('POST', `${API_BASE}/files/extract`, { archive, dest }),
  fileSearch: (path, query, mode = 'name', limit = 200) =>
    request('POST', `${API_BASE}/files/search`, { path, query, mode, limit }),
  fileReplace: (path, find, replace, all = true) =>
    request('POST', `${API_BASE}/files/replace`, { path, find, replace, all }),
  // 下载与上传走原生表单/URL，不经过 JSON 封装
  fileDownloadURL: (path) => apiURL(`files/download?path=${encodeURIComponent(path)}`),

  // ---- Web 终端 ----
  terminalInfo: () => request('GET', `${API_BASE}/terminal`),
  terminalKill: (id) => request('DELETE', `${API_BASE}/terminal/${encodeURIComponent(id)}`),
  terminalWSURL: () => {
    const u = new URL(apiURL('terminal/ws'), location.href);
    u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
    return u.toString();
  },

  // ---- 系统设置（把 macOS 配成服务器）----
  // 探测是只读的，随时可刷新；动作一律走任务中心（返回 task_id），
  // 因为其中「一键设为服务器模式」要跑十几条命令、动 hosts 与 pmset。
  systemSettings: () => request('GET', `${API_BASE}/system/settings`),
  systemSettingsAction: (action) =>
    request('POST', `${API_BASE}/system/settings/${enc(action)}`, {}),

  // ---- nginx 环境 ----
  nginxTest: () => request('POST', `${API_BASE}/system/nginx/test`, {}),
  nginxStatus: () => request('GET', `${API_BASE}/system/nginx/status`),
  nginxRepair: () => request('POST', `${API_BASE}/system/nginx/repair`, {}),

  // ---- Docker ----
  //
  // 所有标识符都过 enc()：容器名/镜像名/卷名可能含 `/`（nginx:1.27、
  // library/redis、my_proj_db），不编码会把路径拆断。后端用的是 `{path...}`
  // 通配参数，所以编码后的 %2F 会被正确还原。
  dockerInfo: () => request('GET', `${API_BASE}/docker/info`),
  dockerContainers: (all = true) => request('GET', `${API_BASE}/docker/containers?all=${all ? 1 : 0}`),
  // 容器名走查询参数而不是路径段：Go 1.22 的 `{path...}` 通配只能出现在
  // 模式末尾，而这三个接口后面还要跟 /logs、/inspect 或动作名。
  dockerContainerAction: (name, action) =>
    request('POST', `${API_BASE}/docker/containers/action?name=${enc(name)}&action=${enc(action)}`, {}),
  dockerContainerLogs: (name, lines = 300) =>
    request('GET', `${API_BASE}/docker/containers/logs?name=${enc(name)}&lines=${lines}`),
  dockerContainerInspect: (name) =>
    request('GET', `${API_BASE}/docker/containers/inspect?name=${enc(name)}`),
  dockerContainerRemove: (name, force = false) =>
    request('DELETE', `${API_BASE}/docker/containers/${enc(name)}?force=${force ? 1 : 0}`),
  dockerContainerCreate: (spec) => request('POST', `${API_BASE}/docker/containers`, spec),
  dockerImages: (all = false) => request('GET', `${API_BASE}/docker/images?all=${all ? 1 : 0}`),
  dockerImagePull: (image) => request('POST', `${API_BASE}/docker/images/pull`, { image }),
  dockerImageRemove: (ref, force = false) =>
    request('DELETE', `${API_BASE}/docker/images/${enc(ref)}?force=${force ? 1 : 0}`),
  dockerImagePrune: () => request('POST', `${API_BASE}/docker/images/prune`, {}),
  dockerVolumes: () => request('GET', `${API_BASE}/docker/volumes`),
  dockerVolumeRemove: (name, force = false) =>
    request('DELETE', `${API_BASE}/docker/volumes/${enc(name)}?force=${force ? 1 : 0}`),
  dockerVolumePrune: () => request('POST', `${API_BASE}/docker/volumes/prune`, {}),
  dockerNetworks: () => request('GET', `${API_BASE}/docker/networks`),
  dockerNetworkRemove: (id) => request('DELETE', `${API_BASE}/docker/networks/${enc(id)}`),
  dockerNetworkPrune: () => request('POST', `${API_BASE}/docker/networks/prune`, {}),
  dockerComposeProjects: () => request('GET', `${API_BASE}/docker/compose`),
  dockerComposeRead: (name) => request('GET', `${API_BASE}/docker/compose/${enc(name)}`),
  dockerComposeSave: (name, content, register = true) =>
    request('POST', `${API_BASE}/docker/compose`, { name, content, register }),
  dockerComposeAction: (name, action, removeVolumes = false) =>
    request('POST',
      `${API_BASE}/docker/compose/${enc(name)}/actions?action=${enc(action)}&remove_volumes=${removeVolumes ? 1 : 0}`,
      {}),
  dockerComposeDelete: (name, removeVolumes = false) =>
    request('DELETE', `${API_BASE}/docker/compose/${enc(name)}?remove_volumes=${removeVolumes ? 1 : 0}`),
};

// enc 把标识符编码进路径段。
//
// 与 encodeURIComponent 的区别：它会把 `/` 也编码成 %2F。
// 这是必需的 —— 镜像名 `library/redis` 若原样放进路径，
// 就会被路由当成两级路径而 404（后端用 `{path...}` 接收，能还原 %2F）。
function enc(v) {
  return encodeURIComponent(String(v == null ? '' : v));
}

// sseServiceLogs 订阅服务日志流。
//
// 服务日志用"按行推送"的 SSE：服务端把新增日志按行拆成多个 data 行，
// EventSource 收到一整条 message 就是一段新增日志。
//
// onError 会带上 EventSource 本身（第二个参数）：调用方必须能区分
// 「暂时断了、浏览器会自动重连」与「服务端明确拒绝（例如该服务没有日志文件，
// 返回 400）、永远不会重连」—— 这两种情况的提示语不能是同一句（见 services.js）。
export function sseServiceLogs(name, { onData, onError, onClose } = {}) {
  const es = new EventSource(apiURL(`services/${encodeURIComponent(name)}/logs/stream`),
    { withCredentials: true });
  if (onData) es.onmessage = (e) => onData(e.data + '\n');
  es.addEventListener('close', () => { if (onClose) onClose(); es.close(); });
  if (onError) es.onerror = (e) => onError(e, es);
  return es;
}

// sse 建立指标推送连接，返回 EventSource（调用方负责 close）。
export function sse(url, { onSample, onError } = {}) {
  const es = new EventSource(url, { withCredentials: true });
  if (onSample) es.addEventListener('sample', (e) => {
    try { onSample(JSON.parse(e.data)); } catch { /* 忽略坏帧 */ }
  });
  if (onError) es.onerror = onError;
  return es;
}
