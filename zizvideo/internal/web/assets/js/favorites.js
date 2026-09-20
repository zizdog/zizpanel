// 收藏页：点赞 / 收藏 / 历史 三个 Tab，每个都可清除记录（条目 10）

import { api } from "./api.js";
import { el, clear, banner, setBanner, fmtDuration } from "./dom.js";
import { confirmDialog } from "./confirm.js";

const TABS = [
  {
    key: "likes", label: "点赞", empty: "还没有点赞过的视频",
    list: () => api.request("GET", "/api/v1/me/likes"),
    clear: () => api.request("DELETE", "/api/v1/me/likes"),
    hint: "只清除点赞记录，不删除视频文件。",
  },
  {
    key: "favorites", label: "收藏", empty: "还没有收藏的视频",
    list: () => api.request("GET", "/api/v1/me/favorites"),
    clear: () => api.request("DELETE", "/api/v1/me/favorites"),
    hint: "只清除收藏记录，不删除视频文件。",
  },
  {
    key: "history", label: "历史", empty: "还没有观看记录",
    list: () => api.myProgress(),
    clear: () => api.request("DELETE", "/api/v1/me/progress"),
    hint: "只清除观看历史，不删除视频文件。",
  },
];

function normalize(tab, data) {
  const raw = data && Array.isArray(data.list) ? data.list : [];
  return raw.map((entry) => {
    if (tab.key === "history") return { media: entry.media || {}, progress: entry };
    return { media: entry, progress: entry.progress || {} };
  });
}

export function mountFavorites(view) {
  const note = banner();
  const tabsNav = el("nav", { class: "tabs", dataset: { role: "fav-tabs" } });
  const listBox = el("div", { class: "fav-list", dataset: { role: "fav-list" } });
  const status = el("div", { class: "muted small-note", dataset: { role: "clear-status" } });
  const clearBtn = el("button", {
    class: "btn danger small", type: "button", text: "清除记录", dataset: { role: "clear-records" },
  });
  const buttons = new Map();
  let current = TABS[0];

  view.append(el("div", { class: "page" },
    el("div", { class: "page-head" },
      el("h2", { class: "page-title", text: "收藏" }), clearBtn),
    tabsNav, note, status, listBox));

  for (const tab of TABS) {
    const button = el("button", {
      class: "tab", type: "button", text: tab.label, dataset: { role: "fav-tab", tab: tab.key },
      onclick: () => select(tab),
    });
    buttons.set(tab.key, button);
    tabsNav.append(button);
  }

  function select(tab) {
    current = tab;
    for (const [key, node] of buttons) node.classList.toggle("on", key === tab.key);
    clearBtn.dataset.tab = tab.key;
    setBanner(note, "");
    status.textContent = "";
    load();
  }

  async function load() {
    clear(listBox);
    listBox.append(el("div", { class: "muted", text: "加载中…" }));
    let items = [];
    try {
      items = normalize(current, await current.list());
    } catch (err) {
      clear(listBox);
      setBanner(note, err && err.message ? err.message : "加载失败");
      return;
    }
    clear(listBox);
    if (!items.length) {
      listBox.append(el("div", { class: "muted", text: current.empty }));
      return;
    }
    for (const item of items) listBox.append(favRow(current, item));
  }

  clearBtn.addEventListener("click", async () => {
    const items = listBox.querySelectorAll(".fav-row");
    const ok = await confirmDialog({
      title: "清除" + current.label + "记录",
      message: "将清除全部 " + items.length + " 条" + current.label + "记录。" + current.hint,
      confirmText: "确认清除",
    });
    if (!ok) return;
    clearBtn.disabled = true;
    try {
      const data = await current.clear();
      // 条数以后端返回为准，不拿本地列表凑数。
      const cleared = data && typeof data.cleared === "number" ? data.cleared : 0;
      status.textContent = cleared > 0
        ? ("已清除 " + cleared + " 条" + current.label + "记录")
        : ("没有可清除的" + current.label + "记录");
      setBanner(note, "");
      await load();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "清除失败");
    } finally {
      clearBtn.disabled = false;
    }
  });

  select(TABS[0]);
  return null;
}

function favRow(tab, item) {
  const media = item.media || {};
  const thumb = media.cover_url
    ? el("img", { class: "fav-thumb", src: media.cover_url, alt: "", loading: "lazy" })
    : el("div", { class: "fav-thumb no-cover", text: "无封面" });
  const meta = tab.key === "history"
    ? ("看到 " + fmtDuration(item.progress.position_ms) + " / " + fmtDuration(media.duration_ms))
    : fmtDuration(media.duration_ms);
  return el("div", { class: "fav-row", dataset: { media: String(media.id || "") } },
    thumb,
    el("div", { class: "fav-meta" },
      el("div", { class: "fav-title", text: media.title || ("#" + media.id) }),
      el("div", { class: "muted small-note", text: meta }),
      item.progress && item.progress.completed ? el("div", { class: "muted small-note", text: "已看完" }) : null));
}
