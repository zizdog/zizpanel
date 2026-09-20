// 我的：账号、设置入口、退出登录（条目 10）

import { el, fmtDate } from "./dom.js";
import { session } from "./auth.js";

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

  const settings = el("div", { class: "panel", dataset: { role: "me-settings" } },
    el("div", { class: "panel-title", text: "设置" }),
    el("a", { class: "btn small", href: "#/feed", text: "播放设置" }),
    el("div", { class: "muted small-note", text: "循环播放、自动播下一集在播放页右上角 ⚙ 里设置。" }));
  if (user.role === "admin") {
    settings.append(el("a", { class: "btn small", href: "#/admin", text: "管理后台" }));
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
