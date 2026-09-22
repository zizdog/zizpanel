// 剧场（短剧）：列表 + 按序连播的独立播放器（条目 9）
// 顺序完全由后端的 position 决定；播放端自己按数组顺序推进，不吃随机游标。

import { api, patchProgressKeepalive } from "./api.js";
import { el, clear, banner, setBanner, field, fmtDuration, asArray } from "./dom.js";
import { session } from "./auth.js";
import { confirmDialog } from "./confirm.js";
import { mountSeriesAdmin } from "./series-admin.js";

const PROGRESS_EVERY_MS = 5000;
const COMPLETE_TAIL_MS = 1500;

function isAdmin() { return !!(session.user && session.user.role === "admin"); }

/* ---------- 剧场列表 ---------- */

export function mountSeries(view) {
  const note = banner();
  const detectNote = el("div", { class: "banner", hidden: true, dataset: { role: "series-detect-note" } });
  let detectTimer = null;
  const box = el("div", { class: "series-list", dataset: { role: "series-list" } });
  const titleInput = el("input", { class: "input", placeholder: "剧场标题", required: true });
  const descInput = el("input", { class: "input", placeholder: "简介（可选）" });
  const submit = el("button", { class: "btn primary", type: "submit", text: "新建" });
  const form = el("form", { class: "panel", hidden: true, dataset: { role: "series-create" } },
    field("标题", titleInput), field("简介", descInput), el("div", { class: "actions" }, submit));
  const head = el("div", { class: "page-head" }, el("h2", { class: "page-title", text: "剧场" }));
  if (isAdmin()) {
    head.append(el("button", {
      class: "btn small", type: "button", text: "一键识别全部", dataset: { role: "series-detect-all" },
      onclick: (event) => runDetectAll(event.currentTarget),
    }));
    head.append(el("button", {
      class: "btn small", type: "button", text: "＋ 新建剧场", dataset: { role: "series-create-toggle" },
      onclick: () => { form.hidden = !form.hidden; if (!form.hidden) titleInput.focus(); },
    }));
  }
  view.append(el("div", { class: "page" }, head,
    el("div", { class: "muted small-note", text: "剧场成员在剧场「管理」里；媒体库/允许根/用户在后台" }),
    form, note, detectNote, box));

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    submit.disabled = true;
    try {
      const created = await api.request("POST", "/api/v1/admin/series", {
        title: titleInput.value.trim(), description: descInput.value.trim(),
      });
      titleInput.value = "";
      descInput.value = "";
      form.hidden = true;
      // 新建后直接进剧场页看"下一步做什么"，不把用户丢回空列表。
      if (created && created.id) openSeries(created.id);
      else load();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "新建失败");
    } finally {
      submit.disabled = false;
    }
  });

  function openSeries(id) {
    const hash = "#/series/" + encodeURIComponent(id);
    if (location.hash === hash) load();
    else location.hash = hash;
  }

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
      box.append(el("div", { class: "muted",
        text: isAdmin() ? "还没有剧场，点右上角新建；文件放在允许根目录的子文件夹里" : "还没有剧场" }));
      return;
    }
    for (const item of list) box.append(seriesCard(item, load));
  }

  // 一键识别：先预览变化（不落库）→ 确认 → 任务进度 → 结果（文案 ≤40 字）。
  async function runDetectAll(button) {
    setBanner(detectNote, "正在统计变化…");
    button.disabled = true;
    let preview;
    try {
      preview = await api.detectAll({ confirm: false });
    } catch (err) {
      setBanner(detectNote, err && err.message ? err.message : "预览失败");
      button.disabled = false;
      return;
    }
    button.disabled = false;
    const failed = Number(preview && preview.failed_total) || 0;
    if (failed > 0) {
      setBanner(detectNote, "预览失败 " + failed + " 个剧场：" + firstDetectError(preview));
      return;
    }
    const changes = Number(preview && preview.changes_total) || 0;
    const manual = Number(preview && preview.manual_skipped_total) || 0;
    if (changes === 0) {
      setBanner(detectNote, "没有需要识别的新集");
      return;
    }
    const ok = await confirmDialog({
      title: "一键识别全部", danger: false, confirmText: "开始识别",
      message: "将更新 " + changes + " 集，跳过 " + manual + " 集手动",
    });
    if (!ok) { setBanner(detectNote, ""); return; }
    let task;
    try {
      task = await api.detectAll({ confirm: true });
    } catch (err) {
      setBanner(detectNote, err && err.message ? err.message : "提交失败");
      return;
    }
    const taskId = task && task.task_id;
    if (!taskId) {
      setBanner(detectNote, (task && task.note) || "没有需要识别的剧场");
      return;
    }
    pollDetect(taskId);
  }

  function pollDetect(taskId) {
    if (detectTimer) clearInterval(detectTimer);
    setBanner(detectNote, "识别中…");
    detectTimer = setInterval(async () => {
      let task;
      try {
        task = await api.jobTask(taskId);
      } catch (err) {
        clearInterval(detectTimer);
        detectTimer = null;
        setBanner(detectNote, err && err.message ? err.message : "查询失败");
        return;
      }
      if (task.status === "pending" || task.status === "running") {
        setBanner(detectNote, "识别中 " + (Number(task.processed) || 0) + "/" + (Number(task.total) || 0) + "…");
        return;
      }
      clearInterval(detectTimer);
      detectTimer = null;
      const updated = Number(task.updated) || 0;
      const skipped = Number(task.manual_skipped) || 0;
      const label = task.status === "success" ? "已更新 " : task.status === "failed" ? "识别失败：" : "识别已中断：";
      let text = label + updated + " 集，跳过 " + skipped + " 集手动";
      if (task.error) text += "；" + task.error;
      if (task.degraded && task.degrade_reason) text += "；" + task.degrade_reason;
      setBanner(detectNote, text);
      load();
    }, 1000);
  }

  function firstDetectError(preview) {
    const rows = Array.isArray(preview && preview.per_series) ? preview.per_series : [];
    for (const row of rows) { if (row && row.error) return row.error; }
    return "请重试";
  }

  load();
  return function cleanup() {
    if (detectTimer) clearInterval(detectTimer);
    detectTimer = null;
  };
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
  // 管理入口也放在剧场页：entries 只在后台会让用户找不到（B.2）。
  const manageBtn = isAdmin()
    ? el("button", {
        class: "btn small", type: "button", text: "管理这个剧场", dataset: { role: "series-manage-play" },
        onclick: () => {
          if (!state.series) { showToast("还在加载，请稍候"); return; }
          openDrawer(state.series, () => loadSeries());
        },
      })
    : null;

  const playBox = el("div", { class: "series-play" },
    el("div", { class: "sp-stage" }, video),
    el("div", { class: "sp-top" },
      el("a", { class: "btn small", href: "#/series", text: "← 剧场", dataset: { role: "back-to-series" } }),
      titleEl, manageBtn, countEl, orderNote),
    center, centerBtn,
    el("div", { class: "sp-bottom" },
      el("div", { class: "row sp-controls" }, prev, epSelect, next, listToggle, sound),
      el("div", { class: "bar" }, barFill),
      el("div", { class: "times" }, elapsed, total)),
    listBox, toast);
  // 新剧场没有剧集时不留空页面：给"下一步做什么"的卡片（放哪/怎么命名/两条路）。
  const guideBox = el("div", { class: "series-guide", hidden: true, dataset: { role: "series-empty-guide" } });
  view.append(playBox, guideBox);

  const STEP_LABELS = { scan: "扫描", detect: "识别", add_existing: "加入已有媒体" };

  function renderGuide(guide) {
    const data = guide || {};
    const usable = asArray(data.usable_roots);
    const all = asArray(data.roots);
    const steps = asArray(data.steps);
    const card = el("div", { class: "panel guide-box" },
      el("div", { class: "panel-title", text: data.title || "这个剧场还没有剧集" }),
      el("div", { class: "muted small-note", text: data.where || "文件放在某个允许根目录下的子文件夹里" }),
      usable.length
        ? el("div", { class: "muted small-note", text: "可用允许根：" + usable.join("、") })
        : el("div", { class: "row" },
            el("span", { class: "muted small-note", text: all.length ? "已配置的允许根都不可用" : "还没有允许根" }),
            el("a", { class: "btn small primary", href: data.roots_url || "#/admin/roots", text: "去加允许根" })),
      el("div", { class: "muted small-note", text: data.naming || "" }),
      el("div", { class: "muted small-note", text: data.naming_note || "" }),
      ...steps.map((step) => el("div", { class: "guide-step" },
        el("span", { class: "guide-key", text: STEP_LABELS[step.key] || step.key || "步骤" }),
        el("span", { class: "ep-title", text: step.text || "" }))),
      el("div", { class: "actions" },
        isAdmin() ? el("button", {
          class: "btn primary", type: "button", text: "管理这个剧场（加入已有媒体）",
          dataset: { role: "series-manage-empty" },
          onclick: () => { if (state.series) openDrawer(state.series, () => loadSeries()); },
        }) : null,
        isAdmin() ? el("a", { class: "btn small", href: "#/admin/libraries", text: "去后台建库扫描" }) : null,
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
