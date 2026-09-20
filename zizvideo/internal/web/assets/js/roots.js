// 目录选择器：只显示目录名（接口也只回目录），绝不读取文件内容。

import { api } from "./api.js";
import { el, clear, asArray } from "./dom.js";

// norm 归一化尾斜杠，用于把选中的目录和允许根对齐比较。
function norm(path) {
  if (!path || path === "/") return "/";
  return String(path).replace(/\/+$/, "");
}

// isUnder 判断 path 是否落在 root 内（或就是 root）。
function isUnder(path, root) {
  const p = norm(path);
  const r = norm(root);
  return p === r || p.startsWith(r + "/");
}

function crumbButton(label, path, onPick) {
  return el("button", { class: "btn small", type: "button", text: label, onclick: () => onPick(path) });
}

function breadcrumb(path, onPick) {
  const box = el("div", { class: "crumbs" });
  if (!path) return box;
  box.append(crumbButton("根", "/", onPick));
  const parts = String(path).split("/").filter(Boolean);
  let acc = "";
  for (const part of parts) {
    acc += "/" + part;
    box.append(el("span", { class: "crumb-sep", text: "/" }),
      crumbButton(part, acc, onPick));
  }
  return box;
}

// openDirectoryPicker 打开目录浏览器；选中后回调 onPicked(path)。
export function openDirectoryPicker(options) {
  const onPicked = options && options.onPicked ? options.onPicked : () => {};
  const roots = () => (options && options.roots ? options.roots() : []);

  const note = el("div", { class: "banner", hidden: true });
  const crumbs = el("div", { class: "crumbs" });
  const list = el("div", { class: "dir-list" });
  const more = el("button", { class: "btn small", type: "button", text: "加载更多", hidden: true });
  const selectButton = el("button", { class: "btn primary", type: "button", text: "选这个目录" });
  const addButton = el("button", { class: "btn", type: "button", text: "加为允许根", hidden: true });
  const upButton = el("button", { class: "btn small", type: "button", text: "上级" });

  const modal = el("div", { class: "picker" },
    el("div", { class: "picker-card" },
      el("div", { class: "picker-head" },
        el("span", { text: "选择媒体目录" }),
        el("button", { class: "btn small", type: "button", text: "关闭", onclick: close })),
      note,
      crumbs,
      list,
      more,
      el("div", { class: "actions" }, upButton, addButton, selectButton)));
  const overlay = el("div", { class: "picker-overlay" }, modal);

  let current = "";
  let startsShown = false;

  function close() {
    document.removeEventListener("keydown", onKey);
    overlay.remove();
  }

  function onKey(event) {
    if (event.key === "Escape") close();
  }

  function say(message) {
    if (!message) { note.textContent = ""; note.hidden = true; return; }
    note.textContent = message;
    note.hidden = false;
  }

  function dirRow(name, path) {
    return el("button", {
      class: "dir-row", type: "button", title: path,
      onclick: () => load(path).catch(report),
    }, el("span", { class: "dir-name", text: name }));
  }

  // entryRows 永远返回数组：空目录给一行提示，绝不能回单个节点让调用方展开（坑 13）。
  function entryRows(entries) {
    const rows = asArray(entries);
    if (!rows.length) return [el("div", { class: "muted", text: "没有子目录" })];
    return rows.map((entry) => {
      const row = dirRow(entry && entry.name, entry && entry.path);
      if (!entry || entry.readable === false) {
        row.disabled = true;
        row.title = ((entry && entry.path) || "") + "（不可读）";
      }
      row.append(el("span", { class: "dir-meta", text: ((entry && entry.subdirs) || 0) + " 个子目录" }));
      return row;
    });
  }

  function startChips(starts) {
    const list2 = asArray(starts);
    if (!list2.length) return null;
    const box = el("div", { class: "chips" });
    for (const start of list2) box.append(crumbButton(start, start, (p) => load(p).catch(report)));
    return box;
  }

  async function render(result, append) {
    const data = result || {};
    current = data.path;
    clear(crumbs);
    crumbs.append(breadcrumb(data.path, (p) => load(p).catch(report)));
    const rows = entryRows(data.entries);
    if (append) {
      for (const row of rows) list.append(row);
    } else {
      clear(list);
      if (!startsShown) {
        const chips = startChips(data.starts);
        if (chips) { list.append(chips); startsShown = true; }
      }
      for (const row of rows) list.append(row);
    }
    const loaded = list.querySelectorAll(".dir-row").length;
    more.hidden = !data.has_more;
    more.onclick = () => load(data.path, loaded).catch(report);
    upButton.disabled = !data.parent;
    upButton.onclick = () => { if (data.parent) load(data.parent).catch(report); };
    const covered = roots().some((root) => isUnder(data.path, root));
    addButton.hidden = covered;
    addButton.textContent = "加为允许根";
    selectButton.textContent = "选这个目录";
  }

  async function load(path, offset) {
    say("");
    try {
      const result = await api.browse(path, offset || 0);
      await render(result, (offset || 0) > 0);
      if (options && options.onVisited) options.onVisited(result.path);
    } catch (err) {
      say(err && err.message ? err.message : "打开目录失败");
    }
  }

  function report(err) {
    say(err && err.message ? err.message : "操作失败");
  }

  selectButton.addEventListener("click", () => {
    if (!current) { say("请先选一个目录"); return; }
    onPicked(current);
    close();
  });

  addButton.addEventListener("click", async () => {
    if (!current) return;
    say("");
    try {
      await api.addMediaRoot(current);
      if (options && options.onRootsChanged) await options.onRootsChanged();
      addButton.hidden = true;
      selectButton.textContent = "选这个目录";
      say("已加为允许根：" + current);
    } catch (err) {
      report(err);
    }
  });

  document.addEventListener("keydown", onKey);
  document.body.append(overlay);
  load((options && options.start) || "/Volumes").catch(report);
  return { close };
}
