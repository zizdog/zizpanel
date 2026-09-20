// API 客户端：统一信封解析 + CSRF 双提交（坑 3：写操作必须带 X-CSRF-Token）

const CSRF_COOKIE = "zv_csrf";

export class ApiError extends Error {
  constructor(message, code, status) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

function readCookie(name) {
  const raw = String(document.cookie || "");
  for (const part of raw.split(";")) {
    const at = part.indexOf("=");
    if (at < 0) continue;
    if (part.slice(0, at).trim() === name) return decodeURIComponent(part.slice(at + 1).trim());
  }
  return "";
}

function queryString(params) {
  if (!params) return "";
  const parts = [];
  for (const key of Object.keys(params)) {
    const value = params[key];
    if (value === null || value === undefined || value === "") continue;
    parts.push(encodeURIComponent(key) + "=" + encodeURIComponent(String(value)));
  }
  return parts.length ? "?" + parts.join("&") : "";
}

function buildInit(method, body, keepalive) {
  const headers = {};
  const init = { method, headers, credentials: "same-origin", cache: "no-store" };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  if (method !== "GET" && method !== "HEAD") {
    const token = readCookie(CSRF_COOKIE);
    if (token) headers["X-CSRF-Token"] = token;
  }
  if (keepalive) init.keepalive = true;
  return init;
}

async function envelope(method, path, body, keepalive) {
  let response;
  try {
    response = await fetch(path, buildInit(method, body, keepalive));
  } catch (err) {
    throw new ApiError("网络错误，请重试", "NETWORK", 0);
  }
  const text = await response.text();
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch (err) { payload = null; }
  }
  const error = payload && payload.error;
  if (!response.ok || error) {
    const message = error && error.message ? String(error.message) : "请求失败（HTTP " + response.status + "）";
    throw new ApiError(message, (error && error.code) || "HTTP_" + response.status, response.status);
  }
  const data = payload && Object.prototype.hasOwnProperty.call(payload, "data") ? payload.data : payload;
  const meta = (payload && payload.meta) || {};
  return { data: data === undefined ? null : data, meta };
}

async function request(method, path, body) {
  const result = await envelope(method, path, body, false);
  return result.data;
}

// PATCH 无法走 sendBeacon，只能 fetch + keepalive 在页面卸载时补一次（坑 2）
export function patchProgressKeepalive(mediaId, body) {
  const path = "/api/v1/me/progress/" + encodeURIComponent(mediaId);
  try {
    const done = fetch(path, buildInit("PATCH", body, true));
    if (done && typeof done.catch === "function") done.catch(() => {});
  } catch (err) {
    // 页面正在卸载，忽略
  }
}

export const api = {
  request,
  envelope,
  setupStatus: () => request("GET", "/api/v1/setup/status"),
  setup: (body) => request("POST", "/api/v1/setup", body),
  login: (body) => request("POST", "/api/v1/auth/login", body),
  logout: () => request("POST", "/api/v1/auth/logout", {}),
  me: () => request("GET", "/api/v1/auth/me"),

  users: () => request("GET", "/api/v1/users"),
  createUser: (body) => request("POST", "/api/v1/users", body),
  updateUser: (id, body) => request("PATCH", "/api/v1/users/" + encodeURIComponent(id), body),
  userLibraries: (id) => request("GET", "/api/v1/admin/users/" + encodeURIComponent(id) + "/libraries"),
  setUserLibraries: (id, libraryIds) =>
    request("PUT", "/api/v1/admin/users/" + encodeURIComponent(id) + "/libraries", { library_ids: libraryIds }),
  defaultLibraries: () => request("GET", "/api/v1/admin/libraries/defaults"),
  backfillDefaults: () => request("POST", "/api/v1/admin/libraries/defaults/backfill", { confirm: true }),

  libraries: () => request("GET", "/api/v1/libraries"),
  library: (id) => request("GET", "/api/v1/libraries/" + encodeURIComponent(id)),
  createLibrary: (body) => request("POST", "/api/v1/libraries", body),
  updateLibrary: (id, body) => request("PATCH", "/api/v1/libraries/" + encodeURIComponent(id), body),
  deleteLibrary: (id) => request("DELETE", "/api/v1/libraries/" + encodeURIComponent(id)),
  scanLibrary: (id) => request("POST", "/api/v1/libraries/" + encodeURIComponent(id) + "/scan", { kind: "incremental" }),
  scanTask: (id) => request("GET", "/api/v1/scan-tasks/" + encodeURIComponent(id)),
  detectAll: (body) => request("POST", "/api/v1/admin/series/detect-all", body),
  jobTask: (id) => request("GET", "/api/v1/admin/tasks/" + encodeURIComponent(id)),

  mediaList: (params) => envelope("GET", "/api/v1/media" + queryString(params), undefined, false),
  media: (id) => request("GET", "/api/v1/media/" + encodeURIComponent(id)),
  myLibraries: () => request("GET", "/api/v1/me/libraries"),
  feedNext: (params) => envelope("GET", "/api/v1/feed/next" + queryString(params), undefined, false),
  feedSettings: () => request("GET", "/api/v1/feed/settings"),
  patchFeedSettings: (body) => request("PATCH", "/api/v1/feed/settings", body),

  patchProgress: (mediaId, body) => request("PATCH", "/api/v1/me/progress/" + encodeURIComponent(mediaId), body),
  myProgress: () => request("GET", "/api/v1/me/progress"),

  addFavorite: (id) => request("POST", "/api/v1/me/favorites/" + encodeURIComponent(id), {}),
  removeFavorite: (id) => request("DELETE", "/api/v1/me/favorites/" + encodeURIComponent(id)),

  addReaction: (id, kind) => request("POST", "/api/v1/media/" + encodeURIComponent(id) + "/reactions", { kind }),
  patchReaction: (id, kind) => request("PATCH", "/api/v1/media/" + encodeURIComponent(id) + "/reactions", { kind }),
  removeReaction: (id) => request("DELETE", "/api/v1/media/" + encodeURIComponent(id) + "/reactions"),

  systemInfo: () => request("GET", "/api/v1/admin/system/info"),

  // 自动扫描：读写设置 + 立即触发一轮（202 + task_ids，扫描仍走任务中心）。
  autoScan: () => request("GET", "/api/v1/admin/autoscan"),
  saveAutoScan: (body) => request("PATCH", "/api/v1/admin/autoscan", body),
  runAutoScan: () => request("POST", "/api/v1/admin/autoscan/run", {}),

  mediaRoots: () => request("GET", "/api/v1/media/roots"),
  addMediaRoot: (path) => request("POST", "/api/v1/media/roots", { path }),
  removeMediaRoot: (path) =>
    request("DELETE", "/api/v1/media/roots" + queryString({ path })),
  browse: (path, offset) =>
    request("GET", "/api/v1/fs/browse" + queryString({ path, offset, limit: 200 })),
};
