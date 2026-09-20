// 竖向全屏短视频流：懒建卡片 + 单一播放 + 进度上报

import { api, patchProgressKeepalive } from "./api.js";
import { el, fmtDuration } from "./dom.js";

const SNAP_RATIO = 0.6;
const WHEEL_STEP = 40;
const GESTURE_COOLDOWN = 400;
const TOUCH_STEP = 50;
const PROGRESS_EVERY_MS = 5000;
const COMPLETE_TAIL_MS = 1500;
const BATCH = 10;
const WINDOW = 1;

function isPlayable(item) {
  if (item.compatibility && item.compatibility.direct === false) return false;
  if (item.status && item.status !== "ready") return false;
  return true;
}

export function mountFeed(view) {
  const feed = el("div", { class: "feed" });
  const toast = el("div", { class: "toast hidden" });
  view.append(feed, toast);

  const state = {
    items: [], shells: [], built: new Map(),
    active: -1, cursor: 0, hasMore: true, loading: false, soundOn: false,
    infoCard: null, endCard: null, emptyCard: null,
  };
  let toastTimer = 0;

  const observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting || entry.intersectionRatio < SNAP_RATIO) continue;
      const index = Number(entry.target.dataset.index);
      if (Number.isInteger(index) && index !== state.active) setActive(index);
    }
  }, { root: feed, threshold: [SNAP_RATIO] });

  /* ---------- 卡片与视频 ---------- */

  function ensureEntry(index) {
    if (state.built.has(index)) return state.built.get(index);
    return buildEntry(index);
  }

  function buildEntry(index) {
    const item = state.items[index];
    const shell = state.shells[index];
    if (!item || !shell) return null;
    // 每次物化都重建整个 layer，销毁时整块移除，避免重复叠加（坑 8）
    const layer = el("div", { class: "layer" });
    const stage = el("div", { class: "stage" });
    layer.append(stage);
    shell.append(layer);
    const entry = {
      index, item, layer, video: null, fill: null, elapsed: null, total: null,
      fav: null, like: null, hint: null, soundHint: null, playBtn: null,
      resumeDone: false, playRejected: false, broken: false, destroyed: false,
    };
    state.built.set(index, entry);

    entry.fav = el("button", { class: "icon-btn", type: "button", title: "收藏", text: "♥" });
    entry.like = el("button", { class: "icon-btn", type: "button", title: "喜欢", text: "👍" });
    entry.fav.classList.toggle("on", !!item.favorite);
    entry.like.classList.toggle("on", item.reaction === "like");
    entry.fav.addEventListener("click", () => toggleFavorite(entry));
    entry.like.addEventListener("click", () => toggleLike(entry));

    entry.fill = el("span", { class: "bar-fill" });
    entry.elapsed = el("span", { text: "0:00" });
    entry.total = el("span", { text: fmtDuration(item.duration_ms) });
    layer.append(el("div", { class: "overlay" },
      el("div", { class: "ov-top" },
        el("div", { class: "ov-title", text: item.title || ("#" + item.id) }),
        el("div", { class: "ov-actions" }, entry.fav, entry.like)),
      el("div", { class: "bar" }, entry.fill),
      el("div", { class: "times" }, entry.elapsed, entry.total)
    ));

    if (!isPlayable(item)) {
      const reason = item.compatibility && item.compatibility.reason
        ? item.compatibility.reason
        : ("这个视频放不了（" + (item.status || "unknown") + "）");
      layer.append(centerMessage(item.compatibility && item.compatibility.direct === false ? "无法直接播放" : "这个视频放不了", reason));
      return entry;
    }

    const video = el("video", {
      class: "video", playsinline: "", "webkit-playsinline": "", preload: "metadata",
    });
    video.controls = false;
    video.muted = !state.soundOn;
    video.loop = index === 0;
    video.src = item.stream_url;
    entry.video = video;
    video.addEventListener("loadedmetadata", () => onMetadata(entry));
    video.addEventListener("loadeddata", () => checkFrames(entry));
    video.addEventListener("timeupdate", () => paintTime(entry));
    video.addEventListener("play", () => {
      hidePlayButton(entry);
      if (!state.soundOn) showSoundHint(entry);
    });
    video.addEventListener("error", () => showBroken(entry));
    video.addEventListener("click", () => toggleSound(entry));
    stage.append(video);
    return entry;
  }

  function centerMessage(title, sub) {
    return el("div", { class: "center-msg" },
      el("div", { class: "center-inner" },
        el("div", { class: "center-title", text: title }),
        el("div", { class: "center-sub", text: sub })));
  }

  function destroyEntry(index) {
    const entry = state.built.get(index);
    if (!entry) return;
    entry.destroyed = true;
    if (entry.hintTimer) clearTimeout(entry.hintTimer);
    if (entry.video) {
      try { entry.video.pause(); } catch (err) { /* ignore */ }
      // 只在这里清 src；活跃卡播放期间绝不 removeAttribute("src")（坑 4）
      entry.video.removeAttribute("src");
      try { entry.video.load(); } catch (err) { /* ignore */ }
    }
    if (entry.layer && entry.layer.parentNode) entry.layer.remove();
    state.built.delete(index);
  }

  function syncWindow(index) {
    for (const builtIndex of Array.from(state.built.keys())) {
      if (Math.abs(builtIndex - index) > WINDOW) destroyEntry(builtIndex);
    }
    for (let offset = -WINDOW; offset <= WINDOW; offset += 1) {
      const target = index + offset;
      if (target >= 0 && target < state.items.length) ensureEntry(target);
    }
    for (const [builtIndex, entry] of state.built) {
      if (!entry.video) continue;
      if (builtIndex === index) { entry.video.preload = "metadata"; continue; }
      entry.video.preload = builtIndex === index + 1 ? "metadata" : "none";
      if (!entry.video.paused) entry.video.pause();
    }
  }

  function setActive(index) {
    if (index < 0 || index === state.active) return;
    const previous = state.active;
    if (previous >= 0) flushProgress(previous, false);
    state.active = index;
    state.cursor = index;
    syncWindow(index);
    const entry = state.built.get(index);
    if (entry && entry.video && !entry.broken) tryPlay(entry);
    maybeLoadMore(index);
  }

  function tryPlay(entry) {
    const result = entry.video.play();
    if (result && typeof result.catch === "function") {
      result.catch(() => showPlayButton(entry));
    }
  }

  function showPlayButton(entry) {
    if (entry.playRejected || entry.destroyed || !entry.video || !entry.layer) return;
    entry.playRejected = true;
    entry.playBtn = el("button", {
      class: "center-btn", type: "button", text: "点击播放",
      onclick: () => {
        hidePlayButton(entry);
        entry.video.muted = false;
        state.soundOn = true;
        hideSoundHint(entry);
        const result = entry.video.play();
        if (result && typeof result.catch === "function") result.catch(() => {});
      },
    });
    entry.layer.append(entry.playBtn);
  }

  function hidePlayButton(entry) {
    if (entry.playBtn && entry.playBtn.parentNode) entry.playBtn.remove();
    entry.playBtn = null;
  }

  function toggleSound(entry) {
    if (!entry.video) return;
    if (entry.video.muted) {
      entry.video.muted = false;
      state.soundOn = true;
      hideSoundHint(entry);
    } else {
      entry.video.muted = true;
    }
  }

  function showSoundHint(entry) {
    if (entry.destroyed || !entry.layer) return;
    if (!entry.soundHint) {
      entry.soundHint = el("div", { class: "hint low", text: "点击开启声音" });
      entry.layer.append(entry.soundHint);
    }
    entry.soundHint.classList.remove("hidden");
  }

  function hideSoundHint(entry) {
    if (entry.soundHint) entry.soundHint.classList.add("hidden");
  }

  function showHint(entry, text) {
    if (entry.destroyed || !entry.layer) return;
    if (!entry.hint) {
      entry.hint = el("div", { class: "hint" });
      entry.layer.append(entry.hint);
    }
    entry.hint.textContent = text;
    entry.hint.classList.remove("hidden");
    if (entry.hintTimer) clearTimeout(entry.hintTimer);
    entry.hintTimer = setTimeout(() => { if (entry.hint) entry.hint.classList.add("hidden"); }, 2500);
  }

  function onMetadata(entry) {
    if (!entry.resumeDone) {
      entry.resumeDone = true;
      const progress = entry.item.progress || {};
      const position = Number(progress.position_ms) || 0;
      if (position > 0 && !progress.completed) {
        try { entry.video.currentTime = position / 1000; } catch (err) { /* 元数据未就绪则跳过 */ }
        showHint(entry, "已续播");
      }
    }
    paintTime(entry);
  }

  function durationMs(entry) {
    const video = entry.video;
    if (video && Number.isFinite(video.duration) && video.duration > 0) return video.duration * 1000;
    return Number(entry.item.duration_ms) || 0;
  }

  function paintTime(entry) {
    const video = entry.video;
    if (!video) return;
    const duration = durationMs(entry);
    const position = Math.max(0, (video.currentTime || 0) * 1000);
    if (entry.fill) entry.fill.style.width = (duration > 0 ? Math.min(100, (position / duration) * 100) : 0) + "%";
    if (entry.elapsed) entry.elapsed.textContent = fmtDuration(position);
    if (entry.total && duration > 0) entry.total.textContent = fmtDuration(duration);
  }

  function showBroken(entry, sub) {
    if (entry.broken || entry.destroyed || !entry.layer) return;
    entry.broken = true;
    if (entry.video) { try { entry.video.pause(); } catch (err) { /* ignore */ } }
    entry.layer.append(centerMessage("这个视频放不了", sub || ("状态：" + (entry.item.status || "unknown"))));
  }

  // 服务端 direct 会高估（无 HEVC 硬件解码的 Chrome 只出声不出画且不报 error），
  // 首帧解不出来时如实提示，别让用户对着黑屏（坑 11）
  function checkFrames(entry) {
    if (entry.destroyed || entry.broken || !entry.video) return;
    if (entry.video.videoWidth > 0) return;
    const codec = (entry.item.codecs && entry.item.codecs.video) || "该编码";
    showBroken(entry, "这台浏览器解不出 " + String(codec).toUpperCase() + " 画面");
  }

  /* ---------- 反应 / 收藏 ---------- */

  function showToast(message) {
    toast.textContent = String(message);
    toast.classList.remove("hidden");
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toast.classList.add("hidden"), 2500);
  }

  async function toggleFavorite(entry) {
    const item = entry.item;
    const next = !item.favorite;
    item.favorite = next;
    entry.fav.classList.toggle("on", next);
    try {
      const result = next ? await api.addFavorite(item.id) : await api.removeFavorite(item.id);
      if (result && typeof result.favorite === "boolean" && result.favorite !== next) {
        item.favorite = result.favorite;
        entry.fav.classList.toggle("on", result.favorite);
      }
    } catch (err) {
      item.favorite = !next;
      entry.fav.classList.toggle("on", !next);
      showToast(err && err.message ? err.message : "操作失败");
    }
  }

  async function toggleLike(entry) {
    const item = entry.item;
    const liked = item.reaction === "like";
    item.reaction = liked ? null : "like";
    entry.like.classList.toggle("on", !liked);
    try {
      if (liked) await api.removeReaction(item.id);
      else await api.addReaction(item.id, "like");
    } catch (err) {
      item.reaction = liked ? "like" : null;
      entry.like.classList.toggle("on", liked);
      showToast(err && err.message ? err.message : "操作失败");
    }
  }

  /* ---------- 进度上报 ---------- */

  function progressPayload(entry) {
    const duration = Math.round(durationMs(entry));
    const position = Math.round(Math.max(0, (entry.video.currentTime || 0) * 1000));
    return {
      position_ms: position,
      duration_ms: duration,
      completed: duration > 0 && duration - position <= COMPLETE_TAIL_MS,
    };
  }

  function flushProgress(index, keepalive) {
    const entry = state.built.get(index);
    if (!entry || !entry.video || entry.broken) return;
    const payload = progressPayload(entry);
    if (keepalive) patchProgressKeepalive(entry.item.id, payload);
    else api.patchProgress(entry.item.id, payload).catch(() => {});
  }

  const progressTimer = setInterval(() => {
    const entry = state.built.get(state.active);
    if (!entry || !entry.video || entry.broken) return;
    if (entry.video.paused || entry.video.ended) return; // 暂停中不上报（坑 5）
    api.patchProgress(entry.item.id, progressPayload(entry)).catch(() => {});
  }, PROGRESS_EVERY_MS);

  function onPageHide() { flushProgress(state.active, true); }
  function onVisibility() { if (document.visibilityState === "hidden") flushProgress(state.active, true); }

  /* ---------- 列表加载 ---------- */

  function setInfoCard(on) {
    if (on) {
      if (!state.infoCard) state.infoCard = el("div", { class: "card info", text: "加载中…" });
      feed.append(state.infoCard);
    } else if (state.infoCard && state.infoCard.parentNode) {
      state.infoCard.remove();
    }
  }

  function updateEndCard() {
    if (state.hasMore || !state.items.length) return;
    if (!state.endCard) state.endCard = el("div", { class: "card info", text: "没有更多了" });
    feed.append(state.endCard);
  }

  function appendItems(list, meta) {
    for (const item of list) {
      const index = state.items.length;
      const shell = el("article", { class: "card", dataset: { index: String(index), id: String(item.id) } });
      state.items.push(item);
      state.shells.push(shell);
      feed.append(shell);
      observer.observe(shell);
    }
    state.hasMore = !!(meta && meta.has_more);
    if (!state.items.length && !state.emptyCard) {
      state.emptyCard = el("div", { class: "card info", text: "还没有视频" });
      feed.append(state.emptyCard);
    }
    updateEndCard();
    if (state.items.length && state.active < 0) setActive(0);
  }

  async function loadMore() {
    if (state.loading || !state.hasMore) return false;
    state.loading = true;
    setInfoCard(true);
    try {
      const last = state.items.length ? state.items[state.items.length - 1] : null;
      const result = await api.feedNext({ last_id: last ? last.id : undefined, limit: BATCH });
      const list = result && result.data && Array.isArray(result.data.list) ? result.data.list : [];
      const meta = (result && result.meta) || {};
      // 空批次必须停，否则 has_more 说谎时会无限翻页（坑 9）
      appendItems(list, { has_more: !!meta.has_more && list.length > 0 });
      return true;
    } catch (err) {
      showToast(err && err.message ? err.message : "加载失败");
      return false;
    } finally {
      state.loading = false;
      setInfoCard(false);
    }
  }

  function maybeLoadMore(index) {
    if (state.hasMore && !state.loading && index >= state.items.length - 3) loadMore();
  }

  /* ---------- 导航 ---------- */

  function goTo(index) {
    if (!state.items.length) return;
    const target = Math.max(0, Math.min(state.items.length - 1, index));
    const shell = state.shells[target];
    if (!shell) return;
    state.cursor = target;
    feed.scrollTo({ top: shell.offsetTop, behavior: "smooth" });
  }

  async function goToEnd() {
    let guard = 0;
    while (state.hasMore && guard < 50) {
      guard += 1;
      const ok = await loadMore();
      if (!ok) break;
    }
    goTo(state.items.length - 1);
  }

  let wheelAcc = 0;
  let wheelLock = 0;
  let touchY = 0;
  let touchLock = 0;

  function onWheel(event) {
    event.preventDefault();
    const now = Date.now();
    if (now < wheelLock) { wheelAcc = 0; return; }
    wheelAcc += event.deltaY;
    if (Math.abs(wheelAcc) < WHEEL_STEP) return;
    const direction = wheelAcc > 0 ? 1 : -1;
    wheelAcc = 0;
    wheelLock = now + GESTURE_COOLDOWN;
    goTo(state.cursor + direction);
  }

  function onTouchStart(event) {
    if (event.touches.length !== 1) return;
    touchY = event.touches[0].clientY;
  }

  function onTouchEnd(event) {
    const now = Date.now();
    if (now < touchLock) return;
    const touch = event.changedTouches && event.changedTouches[0];
    if (!touch) return;
    const delta = touchY - touch.clientY;
    if (Math.abs(delta) < TOUCH_STEP) return;
    touchLock = now + GESTURE_COOLDOWN;
    goTo(state.cursor + (delta > 0 ? 1 : -1));
  }

  function onKeyDown(event) {
    const target = event.target;
    const tag = target && target.tagName ? target.tagName : "";
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (target && target.isContentEditable)) return;
    if (event.key === " " && tag === "BUTTON") return;
    if (event.key === "ArrowDown" || event.key === "PageDown" || event.key === " " || event.key === "Spacebar") {
      event.preventDefault();
      goTo(state.cursor + 1);
    } else if (event.key === "ArrowUp" || event.key === "PageUp") {
      event.preventDefault();
      goTo(state.cursor - 1);
    } else if (event.key === "Home") {
      event.preventDefault();
      goTo(0);
    } else if (event.key === "End") {
      event.preventDefault();
      goToEnd();
    }
  }

  feed.addEventListener("wheel", onWheel, { passive: false });
  feed.addEventListener("touchstart", onTouchStart, { passive: true });
  feed.addEventListener("touchend", onTouchEnd, { passive: true });
  document.addEventListener("keydown", onKeyDown);
  window.addEventListener("pagehide", onPageHide);
  document.addEventListener("visibilitychange", onVisibility);

  loadMore();

  return function cleanup() {
    clearInterval(progressTimer);
    if (toastTimer) clearTimeout(toastTimer);
    observer.disconnect();
    feed.removeEventListener("wheel", onWheel);
    feed.removeEventListener("touchstart", onTouchStart);
    feed.removeEventListener("touchend", onTouchEnd);
    document.removeEventListener("keydown", onKeyDown);
    window.removeEventListener("pagehide", onPageHide);
    document.removeEventListener("visibilitychange", onVisibility);
    flushProgress(state.active, false);
    for (const index of Array.from(state.built.keys())) destroyEntry(index);
  };
}
