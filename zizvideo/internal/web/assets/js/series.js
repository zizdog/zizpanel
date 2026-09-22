// 剧场（短剧）：列表 + 按序连播的独立播放器（条目 9）
// 顺序完全由后端的 position 决定；播放端自己按数组顺序推进，不吃随机游标。
// 观看面只负责看与播：剧场的新建/导入/识别/上传/管理都在后台（#/admin/series）。

import { api, patchProgressKeepalive } from "./api.js";
import { el, clear, banner, setBanner, fmtDuration, asArray } from "./dom.js";

const PROGRESS_EVERY_MS = 5000;
const COMPLETE_TAIL_MS = 1500;

/* ---------- 剧场列表 ---------- */

export function mountSeries(view) {
  const note = banner();
  const box = el("div", { class: "series-list", dataset: { role: "series-list" } });
  const head = el("div", { class: "page-head" }, el("h2", { class: "page-title", text: "剧场" }));
  view.append(el("div", { class: "page" }, head, note, box));

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
      box.append(el("div", { class: "muted", text: "还没有剧场" }));
      return;
    }
    for (const item of list) box.append(seriesCard(item));
  }

  load();
}

function seriesCard(item) {
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
  return card;
}

/* ---------- 剧场播放页（按序连播） ---------- */

export function mountSeriesPlay(view, seriesID) {
  const state = { series: null, items: [], index: -1, soundOn: false, ended: false, autoNext: false, seekSeconds: 10 };
  const toast = el("div", { class: "toast hidden", dataset: { role: "toast" } });
  const video = el("video", { class: "video", playsinline: "", "webkit-playsinline": "", preload: "metadata" });
  video.controls = false;
  video.muted = true;

  const titleEl = el("div", { class: "sp-title", text: "" });
  const countEl = el("div", { class: "sp-count", text: "加载中…", dataset: { role: "episode-label" } });
  const orderNote = el("div", { class: "muted small-note", hidden: true, dataset: { role: "episode-order-note" } });
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
  // 观看面不放管理入口：剧场管理在后台「剧场」页签（#/admin/series）。
  const playBox = el("div", { class: "series-play" },
    el("div", { class: "sp-stage" }, video),
    el("div", { class: "sp-top" },
      el("a", { class: "btn small", href: "#/series", text: "← 剧场", dataset: { role: "back-to-series" } }),
      titleEl, countEl, orderNote),
    center, centerBtn,
    el("div", { class: "sp-bottom" },
      el("div", { class: "row sp-controls" }, prev, epSelect, next, listToggle, sound),
      el("div", { class: "bar" }, barFill),
      el("div", { class: "times" }, elapsed, total)),
    listBox, toast);
  // 新剧场没有剧集时不留空页面：给"下一步做什么"的卡片（放哪/怎么命名/两条路）。
  const guideBox = el("div", { class: "series-guide", hidden: true, dataset: { role: "series-empty-guide" } });
  view.append(playBox, guideBox);

  const STEP_LABELS = { import_dir: "从目录导入", upload: "上传", batch: "批量建" };

  function renderGuide(guide) {
    const data = guide || {};
    const usable = asArray(data.usable_roots);
    const all = asArray(data.roots);
    const steps = asArray(data.steps);
    const card = el("div", { class: "panel guide-box" },
      el("div", { class: "panel-title", text: data.title || "这个剧场还没有剧集" }),
      el("div", { class: "muted small-note", text: data.where || "文件放在某个媒体库目录的子文件夹里" }),
      usable.length
        ? el("div", { class: "muted small-note", text: "可用允许根：" + usable.join("、") })
        : el("div", { class: "row" },
            el("span", { class: "muted small-note", text: all.length ? "已配置的允许根都不可用" : "还没有允许根" })),
      el("div", { class: "muted small-note", text: data.naming || "" }),
      el("div", { class: "muted small-note", text: data.naming_note || "" }),
      ...steps.map((step) => el("div", { class: "guide-step" },
        el("span", { class: "guide-key", text: STEP_LABELS[step.key] || step.key || "步骤" }),
        el("span", { class: "ep-title", text: step.text || "" }))),
      el("div", { class: "actions" },
        el("a", { class: "btn small", href: "#/series", text: "← 返回剧场列表" })));
    clear(guideBox);
    guideBox.append(card);
  }

  let toastTimer = 0;
  function showToast(message) {
    toast.textContent = String(message);
    toast.classList.remove("hidden");
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toast.classList.add("hidden"), 2500);
  }

  function current() { return state.items[state.index]; }

  // 补丁 R1：优先用后端给的 episode_label（S1E3 / 第 3 集 / 未识别）。
  function labelOf(item, index) {
    if (item && item.episode_label) return item.episode_label;
    const i = index == null ? state.index : index;
    return "第 " + (i + 1) + " 集";
  }

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

  // 左右键跳转：clamp 到 [0, duration]，不打断播放状态。
  function seekBy(direction) {
    if (!current() || !video.getAttribute("src")) return;
    const seconds = state.seekSeconds;
    const max = durationMs() / 1000;
    let target = (Number(video.currentTime) || 0) + direction * seconds;
    if (target < 0) target = 0;
    if (max > 0 && target > max) target = max;
    try { video.currentTime = target; } catch (err) { return; }
    paintTime();
    showToast((direction > 0 ? "前进 " : "后退 ") + seconds + " 秒");
  }

  function onKeyDown(event) {
    const target = event.target;
    const tag = target && target.tagName ? target.tagName : "";
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (target && target.isContentEditable)) return;
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    event.preventDefault();
    seekBy(event.key === "ArrowRight" ? 1 : -1);
  }

  function paint() {
    const item = current();
    if (!item) return;
    titleEl.textContent = item.title || ("#" + item.id);
    countEl.textContent = state.ended
      ? "已播完最后一集"
      : (labelOf(item) + " · 第 " + (state.index + 1) + " / " + state.items.length);
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
        el("span", { class: "ep-no", text: labelOf(item, i) }),
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
      showToast("自动播放 " + labelOf(state.items[target], target));
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
        ? ("自动播放 " + labelOf(current()) + " · 已续播")
        : ("已续播 " + labelOf(current())));
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
  document.addEventListener("keydown", onKeyDown);

  // 左右键秒数跟随播放页设置；拉取失败就用 state 里的默认 10。
  api.feedSettings().then((data) => {
    const n = Number(data && data.seek_seconds);
    if (Number.isFinite(n) && n >= 1) state.seekSeconds = Math.min(120, Math.round(n));
  }).catch(() => {});

  async function loadSeries() {
    try {
      const data = await api.request("GET", "/api/v1/series/" + encodeURIComponent(seriesID));
      const raw = data && Array.isArray(data.list) ? data.list : [];
      state.series = data && data.series ? data.series : null;
      state.items = raw.map((entry) => {
        const item = entry.media || {};
        item.episode_label = entry.episode_label || "";
        item.episode_source = entry.episode_source || "";
        return item;
      }).filter(Boolean);
      titleEl.textContent = (state.series && state.series.title) || "剧场";
      const unrecognized = state.items.filter((item) => item.episode_label === "未识别").length;
      clear(epSelect);
      state.items.forEach((item, i) => {
        epSelect.append(el("option", { value: String(i), text: labelOf(item, i) }));
      });
      if (!state.items.length) {
        countEl.textContent = "这个剧场还没有剧集";
        orderNote.hidden = true;
        renderGuide(data && data.guide);
        playBox.hidden = true;
        guideBox.hidden = false;
        return;
      }
      guideBox.hidden = true;
      playBox.hidden = false;
      orderNote.hidden = unrecognized === 0;
      orderNote.textContent = unrecognized > 0 ? "未识别（按文件名排）共 " + unrecognized + " 集" : "";
      if (state.index < 0 || state.index >= state.items.length) playIndex(0, true);
      else paint();
    } catch (err) {
      countEl.textContent = "加载失败";
      showToast(err && err.message ? err.message : "加载失败");
    }
  }
  loadSeries();

  return function cleanup() {
    clearInterval(timer);
    if (toastTimer) clearTimeout(toastTimer);
    window.removeEventListener("pagehide", onPageHide);
    document.removeEventListener("visibilitychange", onVisibility);
    document.removeEventListener("keydown", onKeyDown);
    flush(false);
    try { video.pause(); } catch (err) { /* ignore */ }
    video.removeAttribute("src");
  };
}
