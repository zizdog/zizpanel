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
  // ---- 运行依赖（基础环境）----
  //
  // 与"网站环境"（nginx + PHP + MySQL + phpMyAdmin）是**两层**，别再混用：
  // 这里只报"跨应用运行依赖"——命令行开发者工具(CLT) → Homebrew → ffmpeg。
  // 判据完全来自后端（不拿服务列表猜）：data = {clt_ok, brew_ok, deps_ok, ready, missing[]}。
  // 旧面板没有这两个接口 → request() 会抛 status=404 的 ApiError，调用方必须如实提示。
  baseEnv: () => request('GET', `${API_BASE}/system/base-env`),
  installBaseEnv: () => request('POST', `${API_BASE}/system/base-env/install`, {}),
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
  // ---- 上传与执行限制（nginx client_max_body_size + PHP 上传/执行上限）----
  // GET 返回配置值 + **回读的生效值**（界面据此区分"已保存"与"已生效"）；
  // POST 校验后返回 202 + task_id，真正的应用（写 vhost + conf.d → reload nginx
  // → 重启 php-fpm）在任务中心里跑，进度与回读都在任务日志里。
  getUploadLimits: () => request('GET', `${API_BASE}/settings/upload-limits`),
  saveUploadLimits: (patch) => request('POST', `${API_BASE}/settings/upload-limits`, patch),
  changePassword: (oldPwd, newPwd) =>
    request('POST', `${API_BASE}/account/password`, { old: oldPwd, new: newPwd }),
  // 改用户名：要当前密码确认；成功后会话仍然有效（会话按 user_id 关联）
  renameUser: (username, password) =>
    request('POST', `${API_BASE}/account/username`, { username, password }),
  totpSetup: () => request('POST', `${API_BASE}/account/totp/setup`, {}),
  totpEnable: (secret, code) => request('POST', `${API_BASE}/account/totp/enable`, { secret, code }),
  totpDisable: (password) => request('POST', `${API_BASE}/account/totp/disable`, { password }),
  sessions: () => request('GET', `${API_BASE}/account/sessions`),

  // ---- 反向代理（独立功能）----
  proxies: () => request('GET', `${API_BASE}/proxies`),
  proxyCreate: (payload) => request('POST', `${API_BASE}/proxies`, payload),
  proxyUpdate: (id, payload) => request('POST', `${API_BASE}/proxies/${id}`, payload),
  proxyToggle: (id) => request('POST', `${API_BASE}/proxies/${id}/toggle`, {}),
  proxyDelete: (id) => request('DELETE', `${API_BASE}/proxies/${id}`),
  proxyTest: (target) => request('POST', `${API_BASE}/proxies/test`, { target }),
  // HTTPS：与站点侧 siteSSL 对称（同一套 provider 取值：
  // self / mkcert / manual / acme）。acme 只是"引用证书库里那张"，
  // 申请仍走「SSL 证书」页的任务中心。
  proxySSL: (id, payload) => request('POST', `${API_BASE}/proxies/${id}/ssl`, payload),
  proxySSLDisable: (id) => request('DELETE', `${API_BASE}/proxies/${id}/ssl`),

  // ---- 站点管理 ----
  sites: () => request('GET', `${API_BASE}/sites`),
  siteCreate: (payload) => request('POST', `${API_BASE}/sites`, payload),
  siteReloadAll: () => request('POST', `${API_BASE}/sites/reload`, {}),
  // 网站环境的**真实运行时**（nginx / PHP / MySQL）：进程 / 端口 / socket 证据。
  // 服务记录只回答"归不归面板管"，绝不能用来回答"在不在跑"。
  sitesRuntime: () => request('GET', `${API_BASE}/sites/runtime`),
  site: (domain) => request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}`),
  siteUpdate: (domain, patch) => request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}`, patch),
  siteDelete: (domain, removeFiles) =>
    request('DELETE', `${API_BASE}/sites/${encodeURIComponent(domain)}?remove_files=${removeFiles ? 1 : 0}`),
  siteSSL: (domain, payload) => request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}/ssl`, payload),
  siteSSLDisable: (domain) => request('DELETE', `${API_BASE}/sites/${encodeURIComponent(domain)}/ssl`),
  siteCheck: (domain) => request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}/check`),
  // 直接保存磁盘上的 vhost（站点「配置」页的编辑器用）。后端写完会跑 nginx -t、
  // 重载、并复核新配置里的监听端口真的在应答；任何一步失败都会回滚并返回原因。
  siteConfSave: (domain, content) =>
    request('POST', `${API_BASE}/sites/${encodeURIComponent(domain)}/conf`, { content }),
  siteLog: (domain, kind = 'access', lines = 200) =>
    request('GET', `${API_BASE}/sites/${encodeURIComponent(domain)}/log?kind=${kind}&lines=${lines}`),

  // ---- SSL 证书（ACME 自动申请）----
  //
  // 为什么这些接口要单独一组（而不是塞进 sites）：
  // 证书是**独立于站点**的资源 —— 一张 `*.example.com` 可以被多个站点复用，
  // 也可能先申请、后建站。站点侧只是"引用"它（见 siteSSL 的 provider=acme）。
  //
  // 列表接口按契约**不含私钥内容**，面板也从不提供读取私钥的接口；
  // 申请时提交的 DNS 凭据只在这一次 POST 里发送，前端不保存、不回显。
  //
  // 申请/续期都是**异步任务**：后端立刻返回 {task_id}，进度走任务中心
  // （见 tasks.js 的 taskCenter.start），前端不再自己轮询。
  certs: () => request('GET', `${API_BASE}/certs`),
  certApply: (payload) => request('POST', `${API_BASE}/certs`, payload),
  certRenew: (primary) => request('POST', `${API_BASE}/certs/${enc(primary)}/renew`, {}),
  // certRetry：用服务端保存的失败条目 + 已存凭据重签，**请求体为空**——
  // 用户不需要重新填域名/校验方式/DNS 服务商，浏览器也不参与任何凭据。
  certRetry: (primary) => request('POST', `${API_BASE}/certs/${enc(primary)}/retry`, {}),
  certDelete: (primary) => request('DELETE', `${API_BASE}/certs/${enc(primary)}`),
  // DNS 服务商清单：只返回名称与所需环境变量键名，绝不回显用户填过的值。
  certDnsProviders: () => request('GET', `${API_BASE}/certs/dns-providers`),

  // ---- PHP 多版本 ----
  // phpList：列出本机已安装的 PHP 版本（含各自端点与运行状态）。
  // 版本下拉框的数据源，不再写死、也不是自由文本。
  phpList: () => request('GET', `${API_BASE}/php`),
  // phpFixListen：让某个版本监听在它独有的端点上（改 www.conf + 重启 fpm）。
  phpFixListen: (version, restart = true) =>
    request('POST', `${API_BASE}/php/fix-listen`, { version, restart }),

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
  // 面板给应用随机生成的登录凭据（从它自己的配置文件里解析）。
  // 没有这个接口时，用户装完 frpc/Orbien 根本不知道 admin UI 的账号口令。
  serviceCredentials: (name) =>
    request('GET', `${API_BASE}/services/${encodeURIComponent(name)}/credentials`),

  // ---- 任务中心（安装/卸载的实时进度）----
  //
  // 任务只活在面板进程内存里，进程一重启就清空 —— 所以这里没有任何"持久化"接口，
  // 前端也不该把任务 id 存进 localStorage 之类的地方。
  tasks: () => request('GET', `${API_BASE}/tasks`),
  task: (id, after = 0, limit = 800) =>
    request('GET', `${API_BASE}/tasks/${encodeURIComponent(id)}?after=${after}&limit=${limit}`),
  taskCancel: (id) => request('POST', `${API_BASE}/tasks/${encodeURIComponent(id)}/cancel`, {}),
  // 提交任务的限时输入（如 MySQL root 口令）。
  // 值只在这一个请求里发送：后端不回显、不写日志、不进审计，前端也不保存它
  // （不写 localStorage、不写 URL、不 console.log）。
  taskInput: (id, key, value) =>
    request('POST', `${API_BASE}/tasks/${encodeURIComponent(id)}/input`, { key, value }),
  // 进度用 SSE。URL 必须走 apiURL 拼相对路径：面板可能挂在 /_panel 子路径下。
  taskStreamURL: (id) => apiURL(`tasks/${encodeURIComponent(id)}/stream`),

  // ---- 应用市场 ----
  market: () => request('GET', `${API_BASE}/market`),
  // 应用界面子路径：探测（每个应用"现在能不能真的打开"）+ 生成 nginx 入口。
  // 探测不是看 HTTP 200：它还会取页面里引用的 js/css，代理没改写对时
  // 页面照样 200、资源全 404（白屏），只看状态码会得出错误结论。
  appProxies: () => request('GET', `${API_BASE}/market/proxies`),
  appProxyApply: () => request('POST', `${API_BASE}/market/proxies/apply`, {}),
  // 从市场卸载：面板装的走这里（service/installer 两类）。
  // 纳管的第三方服务面板不卸载，前端改用「取消纳管」（serviceForget）。
  // 一键建站：domain/admin_user/admin_pass/site_name
  marketInstallSite: (id, payload) => request('POST', `${API_BASE}/market/${encodeURIComponent(id)}/install-site`, payload),
  // force 只能在用户**明确选择**「强制卸载」时才传 true：后端会把它翻成
  // `brew uninstall --ignore-dependencies`（会破坏依赖它的包）。默认 false。
  marketUninstall: (id, removeData = false, force = false) =>
    request('DELETE', `${API_BASE}/market/${encodeURIComponent(id)}?remove_data=${removeData ? 1 : 0}`
      + (force ? '&force=1' : '')),
  marketPreflight: (id) => request('GET', `${API_BASE}/market/${encodeURIComponent(id)}/preflight`),
  // 卸载前的完整计划（含依赖检测）。市场列表**故意不查依赖** —— 每个条目
  // 一次 `brew uses --installed`，36 条就是 15 秒冷启动（列表卡在"正在读取应用目录…"）。
  // 所以前端在用户点「卸载」时才调这个接口，拿到真正带 blocked/dependents 的计划。
  marketUninstallPlan: (id) => request('GET', `${API_BASE}/market/${encodeURIComponent(id)}/uninstall-plan`),
  marketInstall: (id) => request('POST', `${API_BASE}/market/${encodeURIComponent(id)}/install`, {}),
  // 一键 LNMP 分两步：先取**版本候选**（弹窗让用户选 nginx/PHP/MySQL 各自的版本），
  // 用户确认后才把选择提交出去。options 里的 installed 是后端真实探测
  //（brew / 面板服务记录 / launchd plist），不是猜的 —— 用户重跑一键 LNMP 时
  // 最关心的就是"会不会动我已经装好的东西"。
  lnmpOptions: () => request('GET', `${API_BASE}/market/lnmp-options`),
  // selection 形如 {nginx:'nginx', php:'php@8.2', mysql:'mysql@8.4'}。
  // 不传也可以（后端按默认三件套处理，保证向后兼容），但前端**必须**把
  // 用户在这次弹窗里的选择传出去 —— 不传就等于替用户做了决定。
  installLNMP: (selection) => request('POST', `${API_BASE}/market/install-lnmp`, selection || {}),
  installPhpMyAdmin: () => request('POST', `${API_BASE}/market/install-phpmyadmin`, {}),
  installQwenTTS: (opts) => request('POST', `${API_BASE}/market/install-qwentts`, opts || {}),
  installVoiceReceiver: (opts) => request('POST', `${API_BASE}/market/install-voicereceiver`, opts || {}),
  installIOPaint: () => request('POST', `${API_BASE}/market/install-iopaint`, {}),
  // ---- 调用密钥（receiver v1.6.0）：多密钥 + 每密钥额度 + 用量 ----
  // 入口是"添加密钥"而不是"改共享密钥"：一把泄露不必全员更换，
  // 而且每把可以单独设额度（单位：字）。
  voiceKeys: () => request('GET', `${API_BASE}/voice/receiver/keys`),
  voiceKeyAdd: (payload) => request('POST', `${API_BASE}/voice/receiver/keys`, payload || {}),
  voiceKeyUpdate: (id, payload) =>
    request('PATCH', `${API_BASE}/voice/receiver/keys/${encodeURIComponent(id)}`, payload || {}),
  voiceKeyDelete: (id) =>
    request('DELETE', `${API_BASE}/voice/receiver/keys/${encodeURIComponent(id)}`),
  voiceUsage: () => request('GET', `${API_BASE}/voice/receiver/usage`),
  // 清零用量（管理动作：接收端要求管理密钥，网站手里的调用密钥清不了自己的额度）
  voiceUsageReset: (keyId) => request('POST', `${API_BASE}/voice/receiver/usage/reset`,
    keyId ? { key_id: keyId } : { all: true }),

  // ---- 音色来源（receiver v1.5.0）----
  // 每个网站一份自己的参考音频，避免多站点互相覆盖（这正是"合成返回 0 字节"的根因之一）。
  voiceSources: () => request('GET', `${API_BASE}/voice/receiver/sources`),
  deleteVoiceSource: (source) =>
    request('DELETE', `${API_BASE}/voice/receiver/sources?source=${encodeURIComponent(source)}`),
  // 上传必须走 multipart（文件 + 来源），request() 会把 body 当 JSON 编码，所以单独实现。
  uploadVoiceSource: async (source, file) => {
    const fd = new FormData();
    fd.append('source', source);
    fd.append('file', file);
    const headers = { Accept: 'application/json' };
    const csrf = readCookie('zp_csrf');
    if (csrf) headers['X-CSRF-Token'] = csrf;
    let res;
    try {
      res = await fetch(apiURL('voice/receiver/sources'), {
        method: 'POST', headers, body: fd, credentials: 'same-origin',
      });
    } catch {
      throw new ApiError('无法连接面板服务，请检查网络或面板是否在运行', 0);
    }
    const text = await res.text();
    let data = null;
    if (text) { try { data = JSON.parse(text); } catch { /* 非 JSON */ } }
    if (!res.ok) throw new ApiError((data && data.msg) || `${res.status} ${res.statusText}`, res.status);
    if (data && data.ok === false) throw new ApiError(data.msg || '上传失败', res.status);
    return data ? data.data : null;
  },

  // Qwen3 TTS 的模型状态。
  // 2026-09-14 起只有一个模型（1.7B-Base-8bit，音色克隆）：网站侧预置音色下线。
  // switch 现在等于"加载到内存"，unload 是"释放内存"（下次请求会自动重新加载）。
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

  // ---- mac 设置（把 macOS 配成服务器；页面显示名从「系统设置」改来）----
  // 探测是只读的，随时可刷新；动作一律走任务中心（返回 task_id），
  // 因为其中「一键设为服务器模式」要跑十几条命令、动 hosts 与 pmset。
  systemSettings: () => request('GET', `${API_BASE}/system/settings`),
  systemSettingsAction: (action) =>
    request('POST', `${API_BASE}/system/settings/${enc(action)}`, {}),
  // 内网段预授权：秒级同步接口（写 defaults + 读回复核），
  // payload = { enabled, cidrs }；成功返回的是**重新探测到**的状态。
  systemSettingsLANPreauth: (payload) =>
    request('POST', `${API_BASE}/system/settings/lan-preauth`, payload),

  // ---- nginx 环境 ----
  nginxTest: () => request('POST', `${API_BASE}/system/nginx/test`, {}),
  nginxStatus: () => request('GET', `${API_BASE}/system/nginx/status`),
  nginxRepair: () => request('POST', `${API_BASE}/system/nginx/repair`, {}),
  // nginx 性能参数（宝塔式「性能调整」表单）：GET 含**真实生效值回读**（nginx -T），
  // POST 会备份 → 改写 nginx.conf → nginx -t → 失败自动回滚 → 重载 → 回读生效值。
  nginxTuning: () => request('GET', `${API_BASE}/system/nginx/tuning`),
  nginxTuningSave: (values) => request('POST', `${API_BASE}/system/nginx/tuning`, values),

  // ---- 图片压缩（应用「图片压缩（libvips）」）----
  // engine：引擎在不在 + 目标目录里有多少张图（只读，不改任何文件）。
  // compress：批量压缩，走任务中心（202 + task_id，进度走 SSE）。
  imageEngine: (dir, recursive = false) =>
    request('GET', `${API_BASE}/images/engine?dir=${encodeURIComponent(dir || '')}`
      + (recursive ? '&recursive=1' : '')),
  imageCompress: (payload) => request('POST', `${API_BASE}/images/compress`, payload),

  // ---- 默认站点（80 端口上的兜底静态站点）----
  // 面板启动时会自动建一次；这两个接口用于"看状态"与"手动创建/修复"。
  // 没有 nginx 时 apply 会返回 409 + 人话（界面据此给「只安装 Nginx」）。
  defaultSite: () => request('GET', `${API_BASE}/system/default-site`),
  defaultSiteApply: () => request('POST', `${API_BASE}/system/default-site/apply`, {}),

  // ---- Docker ----
  //
  // 所有标识符都过 enc()：容器名/镜像名/卷名可能含 `/`（nginx:1.27、
  // library/redis、my_proj_db），不编码会把路径拆断。后端用的是 `{path...}`
  // 通配参数，所以编码后的 %2F 会被正确还原。
  dockerInfo: () => request('GET', `${API_BASE}/docker/info`),
  // Docker 镜像加速源（换源）：现状 / 上次检测缓存 / 检测可用性 / 保存并重启运行时
  //
  // dockerMirrorCached 是**只读**接口：返回上次检测的结果与检测时间，
  // 绝不触发探测 —— 页面靠它立即渲染（点进「加速源」不再自动等一轮探测）。
  dockerMirrors: () => request('GET', `${API_BASE}/docker/mirrors`),
  dockerMirrorCached: () => request('GET', `${API_BASE}/docker/mirrors/cached`),
  dockerMirrorProbe: (urls) => request('POST', `${API_BASE}/docker/mirrors/probe`, urls ? { urls } : {}),
  dockerMirrorSave: (mirrors) => request('POST', `${API_BASE}/docker/mirrors`, { mirrors }),
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
