// 会话状态 + 登录/初始化视图 + 顶栏

import { api } from "./api.js";
import { el, clear, banner, setBanner, field, input } from "./dom.js";

export const session = { user: null };

export async function loadMe() {
  try {
    session.user = await api.me();
  } catch (err) {
    if (err && (err.status === 401 || err.code === "AUTH_UNAUTHORIZED")) session.user = null;
    else throw err;
  }
  return session.user;
}

export async function doLogout() {
  try { await api.logout(); } catch (err) { /* 退出失败也要回到登录页 */ }
  session.user = null;
}

export function renderHeader(headerEl, onLogout, hideHeader) {
  clear(headerEl);
  const user = session.user;
  if (hideHeader || !user) { headerEl.hidden = true; return; }
  headerEl.hidden = false;
  const nodes = [
    el("div", { class: "brand", text: "Zizvideo" }),
    el("div", { class: "spacer" }),
    el("span", { class: "who", text: user.display_name || user.username || "" }),
  ];
  // append() 会把 null 变成 "null" 文本节点，非管理员必须走条件分支（坑 10）
  if (user.role === "admin") nodes.push(el("a", { class: "link", href: "#/admin", text: "管理" }));
  nodes.push(el("button", { class: "btn small", type: "button", text: "退出", onclick: onLogout }));
  for (const node of nodes) headerEl.append(node);
}

export function mountLogin(view, onSuccess) {
  const username = input({ type: "text", autocomplete: "username", placeholder: "用户名", required: true });
  const password = input({ type: "password", autocomplete: "current-password", placeholder: "口令", required: true });
  const note = banner();
  const submit = el("button", { class: "btn primary", type: "submit", text: "登录" });
  const form = el("form", { class: "panel narrow" },
    el("h1", { class: "title", text: "Zizvideo" }),
    field("用户名", username),
    field("口令", password),
    note,
    submit
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    submit.disabled = true;
    try {
      session.user = await api.login({ username: username.value.trim(), password: password.value });
      onSuccess();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "登录失败");
    } finally {
      submit.disabled = false;
    }
  });
  view.append(form);
  username.focus();
}

export function mountSetup(view, onSuccess) {
  const username = input({ type: "text", autocomplete: "username", placeholder: "用户名", required: true });
  const password = input({ type: "password", autocomplete: "new-password", placeholder: "口令", required: true });
  const confirm = input({ type: "password", autocomplete: "new-password", placeholder: "再输一次口令", required: true });
  const note = banner();
  const submit = el("button", { class: "btn primary", type: "submit", text: "创建管理员" });
  const form = el("form", { class: "panel narrow" },
    el("h1", { class: "title", text: "初始化 Zizvideo" }),
    field("用户名", username),
    field("口令", password),
    field("确认口令", confirm),
    note,
    submit
  );
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    if (password.value !== confirm.value) { setBanner(note, "两次输入的口令不一致"); return; }
    submit.disabled = true;
    try {
      const name = username.value.trim();
      // 契约要求 display_name，初始化界面未单列，用用户名兜底（坑 7）
      session.user = await api.setup({ username: name, password: password.value, display_name: name });
      onSuccess();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "初始化失败");
    } finally {
      submit.disabled = false;
    }
  });
  view.append(form);
  username.focus();
}
