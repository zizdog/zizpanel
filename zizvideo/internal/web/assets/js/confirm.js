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
