// 二次确认弹窗：危险操作（清除记录/删除剧场）必须先确认再执行

import { el } from "./dom.js";

export function confirmDialog(options) {
  const opts = options || {};
  return new Promise((resolve) => {
    const overlay = el("div", { class: "modal-overlay", dataset: { role: "confirm-modal" } });
    let done = false;

    function close(ok) {
      if (done) return;
      done = true;
      document.removeEventListener("keydown", onKey);
      overlay.remove();
      resolve(!!ok);
    }

    function onKey(event) {
      if (event.key === "Escape") { event.preventDefault(); close(false); }
    }

    const cancel = el("button", {
      class: "btn", type: "button", text: opts.cancelText || "取消",
      dataset: { role: "cancel" }, onclick: () => close(false),
    });
    const ok = el("button", {
      class: "btn " + (opts.danger === false ? "primary" : "danger"), type: "button",
      text: opts.confirmText || "确认", dataset: { role: "confirm" }, onclick: () => close(true),
    });
    overlay.append(el("div", { class: "modal", role: "dialog", "aria-modal": "true" },
      el("div", { class: "modal-title", text: opts.title || "确认操作" }),
      el("div", { class: "modal-text", text: opts.message || "" }),
      el("div", { class: "actions modal-actions" }, cancel, ok)));
    overlay.addEventListener("click", (event) => { if (event.target === overlay) close(false); });
    document.addEventListener("keydown", onKey);
    document.body.append(overlay);
    ok.focus();
  });
}

// choiceDialog 给"两个都算确认"的危险操作（前台删除：只删记录 / 连文件一起删）。
// 返回所选 choice.value；取消或 Esc 返回 null。
export function choiceDialog(options) {
  const opts = options || {};
  return new Promise((resolve) => {
    const overlay = el("div", { class: "modal-overlay", dataset: { role: opts.role || "choice-modal" } });
    let done = false;

    function close(value) {
      if (done) return;
      done = true;
      document.removeEventListener("keydown", onKey);
      overlay.remove();
      resolve(value === undefined ? null : value);
    }

    function onKey(event) {
      if (event.key === "Escape") { event.preventDefault(); close(null); }
    }

    const actions = el("div", { class: "actions modal-actions" },
      el("button", {
        class: "btn", type: "button", text: opts.cancelText || "取消",
        dataset: { role: "cancel" }, onclick: () => close(null),
      }));
    for (const choice of opts.choices || []) {
      actions.append(el("button", {
        class: "btn " + (choice.danger ? "danger" : "primary"), type: "button", text: choice.label,
        dataset: { role: choice.value }, onclick: () => close(choice.value),
      }));
    }
    overlay.append(el("div", { class: "modal", role: "dialog", "aria-modal": "true" },
      el("div", { class: "modal-title", text: opts.title || "确认操作" }),
      el("div", { class: "modal-text", text: opts.message || "" }),
      actions));
    overlay.addEventListener("click", (event) => { if (event.target === overlay) close(null); });
    document.addEventListener("keydown", onKey);
    document.body.append(overlay);
  });
}
