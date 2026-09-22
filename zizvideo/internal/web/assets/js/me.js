// 我的：账号、设置入口、退出登录（条目 10）

import { el, fmtDate } from "./dom.js";
import { api } from "./api.js";
import { session } from "./auth.js";
import { createFeedSettingsForm, normalizeFeedSettings } from "./play-settings.js";

function kv(label, value) {
  return el("div", { class: "kv" },
    el("span", { class: "k", text: label }),
    el("span", { class: "v", text: value === null || value === undefined || value === "" ? "-" : String(value) }));
}

export function mountMe(view, options) {
  const opts = options || {};
  const user = session.user || {};
  const account = el("div", { class: "panel", dataset: { role: "me-account" } },
    el("div", { class: "panel-title", text: "账号" }),
    kv("用户名", user.username),
    kv("显示名", user.display_name),
    kv("角色", user.role === "admin" ? "管理员" : "普通用户"),
    kv("上次登录", user.last_login_at ? fmtDate(user.last_login_at) : "首次"));
  // 可访问媒体库：数值来自唯一判据的 /me/libraries（P3 / B.6）
  const libraryLine = kv("可访问媒体库", "-");
  account.append(libraryLine);
  api.myLibraries().then((list) => {
    const n = Array.isArray(list) ? list.length : 0;
    libraryLine.lastChild.textContent = n ? (n + " 个") : "未授权任何媒体库";
  }).catch(() => { libraryLine.lastChild.textContent = "未复核"; });

  // 「我的」里不再放播放设置（用户 2026-09-22 要求去掉）：入口只留在首页右上 ⚙。
  const settings = el("div", { class: "panel", dataset: { role: "me-settings" } },
    el("div", { class: "panel-title", text: "设置" }));
  if (user.role === "admin") {
    settings.append(el("a", { class: "btn small", href: "#/admin", text: "管理后台" }));
  }

  async function togglePlaySettings() {
    if (!playBox.classList.contains("hidden")) { playBox.classList.add("hidden"); return; }
    playBox.classList.remove("hidden");
    playNote.textContent = "读取设置…";
    try {
      playSettings = normalizeFeedSettings(await api.feedSettings());
      playForm.paint(playSettings);
      playNote.textContent = "";
    } catch (err) {
      playNote.textContent = err && err.message ? err.message : "读取设置失败";
    }
  }

  async function savePlaySetting(partial) {
    const before = playSettings;
    playSettings = normalizeFeedSettings(Object.assign({}, playSettings, partial));
    playForm.paint(playSettings);
    playNote.textContent = "保存中…";
    try {
      const result = await api.patchFeedSettings(partial);
      playSettings = normalizeFeedSettings(result || playSettings);
      playForm.paint(playSettings);
      playNote.textContent = "已保存";
    } catch (err) {
      playSettings = before;
      playForm.paint(playSettings);
      playNote.textContent = err && err.message ? err.message : "保存失败";
    }
  }

  const logout = el("button", {
    class: "btn danger", type: "button", text: "退出登录", dataset: { role: "logout" },
    onclick: () => { if (opts.onLogout) opts.onLogout(); },
  });
  view.append(el("div", { class: "page" },
    el("div", { class: "page-head" }, el("h2", { class: "page-title", text: "我的" })),
    account, settings, el("div", { class: "actions" }, logout)));
  return null;
}
