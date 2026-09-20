// 剧场（短剧）：列表 + 按序连播的独立播放器（条目 9）
// 顺序完全由后端的 position 决定；播放端自己按数组顺序推进，不吃随机游标。

import { api, patchProgressKeepalive } from "./api.js";
import { el, clear, banner, setBanner, field, fmtDuration } from "./dom.js";
import { session } from "./auth.js";
import { confirmDialog } from "./confirm.js";
import { mountSeriesAdmin } from "./series-admin.js";

const PROGRESS_EVERY_MS = 5000;
const COMPLETE_TAIL_MS = 1500;

function isAdmin() { return !!(session.user && session.user.role === "admin"); }

/* ---------- 剧场列表 ---------- */

export function mountSeries(view) {
  const note = banner();
  const box = el("div", { class: "series-list", dataset: { role: "series-list" } });
  const titleInput = el("input", { class: "input", placeholder: "剧场标题", required: true });
  const descInput = el("input", { class: "input", placeholder: "简介（可选）" });
  const submit = el("button", { class: "btn primary", type: "submit", text: "新建" });
  const form = el("form", { class: "panel", hidden: true, dataset: { role: "series-create" } },
    field("标题", titleInput), field("简介", descInput), el("div", { class: "actions" }, submit));
  const head = el("div", { class: "page-head" }, el("h2", { class: "page-title", text: "剧场" }));
  if (isAdmin()) {
    head.append(el("button", {
      class: "btn small", type: "button", text: "＋ 新建剧场", dataset: { role: "series-create-toggle" },
      onclick: () => { form.hidden = !form.hidden; if (!form.hidden) titleInput.focus(); },
    }));
  }
  view.append(el("div", { class: "page" }, head, form, note, box));

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    submit.disabled = true;
    try {
      await api.request("POST", "/api/v1/admin/series", {
        title: titleInput.value.trim(), description: descInput.value.trim(),
      });
      titleInput.value = "";
      descInput.value = "";
      form.hidden = true;
      load();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "新建失败");
    } finally {
      submit.disabled = false;
    }
  });

  async function load() {
    clear(box);
    box.append(el("div", { class: "muted", text: "加载中…" }));
    let list = [];
    try {
      const data = await api.request("GET", "/api/v1/series");
      list = data && Array.isArray(data.list) ? data.list : [];
    } catch (err) {
      clear(box);
      setBanner(note, err && err.message ? err.message : "加载失败");
      return;
    }
    clear(box);
    if (!list.length) {
      box.append(el("div", { class: "muted", text: isAdmin() ? "还没有剧场，点右上角新建" : "还没有剧场" }));
      return;
    }
    for (const item of list) box.append(seriesCard(item, load));
  }

  load();
  return null;
}

function seriesCard(item, reload) {
  const cover = item.cover_url
    ? el("img", { class: "series-cover", src: item.cover_url, alt: "", loading: "lazy" })
    : el("div", { class: "series-cover no-cover", text: "无封面" });
  const card = el("a", {
    class: "series-card", href: "#/series/" + encodeURIComponent(item.id),
    dataset: { role: "series-card", id: String(item.id) },
  }, cover, el("div", { class: "series-meta" },
    el("div", { class: "series-title", text: item.title || "-" }),
    el("div", { class: "muted small-note", text: "共 " + (item.episode_count || 0) + " 集" }),
    item.description ? el("div", { class: "series-desc muted small-note", text: item.description }) : null));
  if (!isAdmin()) return card;

  const wrap = el("div", { class: "series-row" }, card,
    el("button", {
      class: "btn small", type: "button", text: "管理", dataset: { role: "series-manage", id: String(item.id) },
      onclick: (event) => { event.preventDefault(); openDrawer(item, reload); },
    }));
  return wrap;
}

function openDrawer(item, reload) {
  const overlay = el("div", { class: "modal-overlay", dataset: { role: "series-admin-drawer" } });
  const body = el("div", { class: "panel series-admin-body" });
  const box = el("div", { class: "modal wide" },
    el("div", { class: "picker-head" },
      el("span", { text: "管理剧场：" + (item.title || "") }),
      el("button", {
        class: "btn small", type: "button", text: "关闭", dataset: { role: "drawer-close" },
        onclick: () => overlay.remove(),
      })),
    body);
  overlay.append(box);
  overlay.addEventListener("click", (event) => { if (event.target === overlay) overlay.remove(); });
  document.body.append(overlay);
  mountSeriesAdmin(body, item, {
    onChanged: () => reload(),
    onDeleted: () => { overlay.remove(); reload(); },
  });
}

/* ---------- 剧场播放页（按序连播） ---------- */

export function mountSeriesPlay(view, seriesID) {
  const state = { series: null, items: [], index: -1, soundOn: false, ended: false, autoNext: false };
  const toast = el("div", { class: "toast hidden", dataset: { role: "toast" } });
  const video = el("video", { class: "video", playsinline: "", "webkit-playsinline": "", preload: "metadata" });
  video.controls = false;
  video.muted = true;

  const titleEl = el("div", { class: "sp-title", text: "" });
  const countEl = el("div", { class: "sp-count", text: "加载中…", dataset: { role: "episode-label" } });
  const epSelect = el("select", { class: "input tiny", dataset: { role: "episode-select" } });
  const prev = el("button", { class: "btn small", type: "button", text: "上一集", dataset: { role: "prev-episode" } });
  const next = el("button", { class: "btn small", type: "button", text: "下一集", dataset: { role: "next-episode" } });
  const sound = el("button", { class: "icon-btn", type: "button", text: "🔇", title: "静音开关", dataset: { role: "sound" } });
  const listToggle = el("button", { class: "btn small", type: "button", text: "选集", dataset: { role: "episode-toggle" } });
  const listBox = el("div", { class: "ep-list", hidden: true, dataset: { role: "episode-list" } });
  const barFill = el("span", { class: "bar-fill" });
  const elapsed = el("span", { text: "0:00" });
  const total = el("span", { text: "0:00" });
  const center = el("div", { class: "center-msg", hidden: true });
  const centerBtn = el("button", {
    class: "center-btn", type: "button", text: "点击播放", hidden: true, dataset: { role: "play" },
    onclick: () => { centerBtn.hidden = true; tryPlay(); },
  });

  view.append(el("div", { class: "series-play" },
    el("div", { class: "sp-stage" }, video),
    el("div", { class: "sp-top" },
      el("a", { class: "btn small", href: "#/series", text: "← 剧场", dataset: { role: "back-to-series" } }),
      titleEl, countEl),
    center, centerBtn,
    el("div", { class: "sp-bottom" },
      el("div", { class: "row sp-controls" }, prev, epSelect, next, listToggle, sound),
      el("div", { class: "bar" }, barFill),
      el("div", { class: "times" }, elapsed, total)),
    listBox, toast));

  let toastTimer = 0;
  function showToast(message) {
    toast.textContent = String(message);
    toast.classList.remove("hidden");
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toast.classList.add("hidden"), 2500);
  }

  function current() { return state.items[state.index]; }

  function durationMs() {
    if (Number.isFinite(video.duration) && video.duration > 0) return video.duration * 1000;
    return Number((current() || {}).duration_ms) || 0;
  }

  function paintTime() {
    const duration = durationMs();
    const position = Math.max(0, (video.currentTime || 0) * 1000);
    barFill.style.width = (duration > 0 ? Math.min(100, (position / duration) * 100) : 0) + "%";
    elapsed.textContent = fmtDuration(position);
    if (duration > 0) total.textContent = fmtDuration(duration);
  }

  function paint() {
    const item = current();
    if (!item) return;
    titleEl.textContent = item.title || ("#" + item.id);
    countEl.textContent = state.ended
      ? "已播完最后一集"
      : ("第 " + (state.index + 1) + " 集 / 共 " + state.items.length + " 集");
    epSelect.value = String(state.index);
    prev.disabled = state.index <= 0;
    next.disabled = state.index >= state.items.length - 1;
    sound.textContent = state.soundOn ? "🔊" : "🔇";
    paintEpList();
  }

  function paintEpList() {
    clear(listBox);
    state.items.forEach((item, i) => {
      const progress = item.progress || {};
      const pct = item.duration_ms > 0
        ? Math.max(0, Math.min(100, Math.round(((Number(progress.position_ms) || 0) / item.duration_ms) * 100)))
        : 0;
      listBox.append(el("button", {
        class: "ep-item" + (i === state.index ? " on" : ""), type: "button",
        dataset: { role: "ep-item", index: String(i) },
        onclick: () => { listBox.hidden = true; state.autoNext = false; playIndex(i, true); },
      },
        el("span", { class: "ep-no", text: "第 " + (i + 1) + " 集" }),
        el("span", { class: "ep-title", text: item.title || ("#" + item.id) }),
        el("span", { class: "ep-pct muted", text: pct > 0 ? pct + "%" : fmtDuration(item.duration_ms) })));
    });
  }

  function flush(keepalive) {
    const item = current();
    if (!item || video.readyState === 0 || !video.src) return;
    const duration = Math.round(durationMs());
    const position = Math.round(Math.max(0, (video.currentTime || 0) * 1000));
    const payload = {
      position_ms: position, duration_ms: duration,
      completed: duration > 0 && duration - position <= COMPLETE_TAIL_MS,
    };
    if (keepalive) patchProgressKeepalive(item.id, payload);
    else api.patchProgress(item.id, payload).catch(() => {});
  }

  function tryPlay() {
    center.hidden = true;
    const result = video.play();
    if (result && typeof result.catch === "function") {
      result.catch(() => { centerBtn.hidden = false; });
    }
  }

  function playIndex(index, autoplay) {
    if (index < 0 || index >= state.items.length) return;
    if (state.index === index && video.getAttribute("src")) {
      if (autoplay) tryPlay();
      return;
    }
    if (state.index >= 0) flush(false);
    state.index = index;
    state.ended = false;
    const item = current();
    video.muted = !state.soundOn;
    video.src = item.stream_url;
    try { video.load(); } catch (err) { /* 直接设 src 也能播 */ }
    paint();
    if (autoplay) tryPlay();
  }

  function onEnded() {
    if (state.index + 1 < state.items.length) {
      const target = state.index + 1;
      state.autoNext = true;
      playIndex(target, true);
      // 自动下一集：只前进一集，绝不跳两集
      showToast("自动播放 第 " + (target + 1) + " 集");
      return;
    }
    state.ended = true;
    flush(false);
    paint();
  }

  function onMetadata() {
    const progress = (current() || {}).progress || {};
    const position = Number(progress.position_ms) || 0;
    if (position > 0 && !progress.completed) {
      try { video.currentTime = position / 1000; } catch (err) { /* 跳过 */ }
      // 自动下一集的提示不能被续播提示顶掉，直接合并成一条。
      showToast(state.autoNext
        ? ("自动播放 第 " + (state.index + 1) + " 集 · 已续播")
        : ("已续播 第 " + (state.index + 1) + " 集"));
      state.autoNext = false;
    }
    paintTime();
  }

  video.addEventListener("loadedmetadata", onMetadata);
  video.addEventListener("timeupdate", paintTime);
  video.addEventListener("ended", onEnded);
  video.addEventListener("error", () => {
    if (!video.getAttribute("src")) return;
    center.hidden = false;
    clear(center);
    center.append(el("div", { class: "center-inner" },
      el("div", { class: "center-title", text: "这一集放不了" }),
      el("div", { class: "center-sub", text: (current() || {}).title || "" })));
  });

  prev.addEventListener("click", () => { state.autoNext = false; playIndex(state.index - 1, true); });
  next.addEventListener("click", () => { state.autoNext = false; playIndex(state.index + 1, true); });
  epSelect.addEventListener("change", () => {
    state.autoNext = false;
    playIndex(Number(epSelect.value), true);
    listBox.hidden = true;
  });
  listToggle.addEventListener("click", () => { listBox.hidden = !listBox.hidden; });
  sound.addEventListener("click", () => {
    state.soundOn = !state.soundOn;
    video.muted = !state.soundOn;
    sound.textContent = state.soundOn ? "🔊" : "🔇";
  });

  const timer = setInterval(() => {
    if (!current() || video.paused || video.ended) return;
    flush(false);
  }, PROGRESS_EVERY_MS);

  const onPageHide = () => flush(true);
  const onVisibility = () => { if (document.visibilityState === "hidden") flush(true); };
  window.addEventListener("pagehide", onPageHide);
  document.addEventListener("visibilitychange", onVisibility);

  (async () => {
    try {
      const data = await api.request("GET", "/api/v1/series/" + encodeURIComponent(seriesID));
      const raw = data && Array.isArray(data.list) ? data.list : [];
      state.series = data && data.series ? data.series : null;
      state.items = raw.map((entry) => entry.media).filter(Boolean);
      titleEl.textContent = (state.series && state.series.title) || "剧场";
      clear(epSelect);
      state.items.forEach((item, i) => {
        epSelect.append(el("option", { value: String(i), text: "第 " + (i + 1) + " 集" }));
      });
      if (!state.items.length) {
        countEl.textContent = "这个剧场还没有剧集";
        return;
      }
      playIndex(0, true);
    } catch (err) {
      countEl.textContent = "加载失败";
      showToast(err && err.message ? err.message : "加载失败");
    }
  })();

  return function cleanup() {
    clearInterval(timer);
    if (toastTimer) clearTimeout(toastTimer);
    window.removeEventListener("pagehide", onPageHide);
    document.removeEventListener("visibilitychange", onVisibility);
    flush(false);
    try { video.pause(); } catch (err) { /* ignore */ }
    video.removeAttribute("src");
  };
}
