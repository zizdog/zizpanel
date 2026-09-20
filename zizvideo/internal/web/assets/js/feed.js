// 竖向全屏短视频流：transform 位移 + 手势锁 + seed 随机游标 + 拖动进度

import { api, patchProgressKeepalive } from "./api.js";
import { el, fmtDuration } from "./dom.js";
import { mountNav } from "./nav.js";

const WHEEL_STEP = 40;
const TOUCH_STEP = 50;
const GESTURE_QUIET = 400;   // 一次手势的静默阈值（400ms 内不第二次推进）
const SLIDE_MS = 280;        // 与 .track 的 transition 保持一致
const PROGRESS_EVERY_MS = 5000;
const COMPLETE_TAIL_MS = 1500;
const BATCH = 10;
const WINDOW = 1;
const SOUND_KEY = "zv_sound";

// 手势锁：一次手势只允许前进一条（短内容一次跳两条是回归 bug）
export function createGestureGate({ quietMs = GESTURE_QUIET } = {}) {
  let armed = false;
  let lastInput = -Infinity;
  let animating = false;
  return {
    get animating() { return animating; },
    // 原始输入：距上次输入超过 quietMs 才算一次新手势
    input(now) {
      if (now - lastInput > quietMs) armed = true;
      lastInput = now;
    },
    // 触摸开始：明确开始一次新手势
    begin(now) { armed = true; lastInput = now; },
    // 动画结束（transitionend 或兜底计时器）
    settle() { animating = false; },
    // 取这次手势的推进额度；返回 0 表示本手势已经推进过了
    take(now, direction) {
      if (animating || !armed) return 0;
      armed = false;
      animating = true;
      lastInput = now;
      return direction > 0 ? 1 : -1;
    },
  };
}

function isPlayable(item) {
  if (item.compatibility && item.compatibility.direct === false) return false;
  if (item.status && item.status !== "ready") return false;
  return true;
}

function readSoundPref() {
  try { return localStorage.getItem(SOUND_KEY) === "on"; } catch (err) { return false; }
}

function writeSoundPref(on) {
  try { localStorage.setItem(SOUND_KEY, on ? "on" : "off"); } catch (err) { /* 隐私模式忽略 */ }
}

function isInteractive(target) {
  if (!target || typeof target.closest !== "function") return false;
  return !!target.closest("button, input, .bar-wrap, .set-panel, .lib-chip");
}

export function mountFeed(view) {
  const feed = el("div", { class: "feed" });
  const track = el("div", { class: "track" });
  const chip = el("div", { class: "feed-chip hidden" });
  const toast = el("div", { class: "toast hidden" });
  feed.append(track, chip);
  view.append(feed, toast);
  // 底栏挂载点（条目 10）：只加容器与入口，不改播放/进度逻辑
  view.append(mountNav("feed"));

  const gate = createGestureGate();
  const state = {
    items: [], shells: [], built: new Map(),
    active: -1, hasMore: true, loading: false,
    soundOn: readSoundPref(),
    scope: "", scopeName: "", nextCursor: "",
    settings: { loop_play: false, loop_effective: false, autoplay_next: true },
    infoCard: null, emptyCard: null,
  };
  let toastTimer = 0;
  let settleTimer = 0;

  /* ---------- 位移与激活 ---------- */

  function paintTrack(animate) {
    const offset = state.active < 0 ? 0 : -state.active * 100;
    if (animate) {
      track.style.transform = "translate3d(0," + offset + "%,0)";
      if (settleTimer) clearTimeout(settleTimer);
      settleTimer = setTimeout(() => { settleTimer = 0; gate.settle(); }, SLIDE_MS + 80);
    } else {
      track.style.transition = "none";
      track.style.transform = "translate3d(0," + offset + "%,0)";
      void track.offsetHeight; // 强制回流，避免位移要下一帧才生效
      track.style.transition = "";
    }
  }

  function onTransitionEnd(event) {
    if (event.target !== track) return;
    if (settleTimer) { clearTimeout(settleTimer); settleTimer = 0; }
    gate.settle();
  }

  function setActive(index, animate) {
    if (index < 0 || index === state.active) return;
    const previous = state.active;
    if (previous >= 0) flushProgress(previous, false);
    state.active = index;
    paintTrack(animate !== false);
    syncWindow(index);
    const entry = state.built.get(index);
    if (entry && entry.video && !entry.broken) tryPlay(entry);
    maybeLoadMore(index);
  }

  // 一次手势只前进一条：goTo 只按最终目标走一步，不会叠加
  function goTo(index, animate) {
    if (!state.items.length) return;
    if (index > state.items.length - 1 && state.hasMore) {
      const before = state.items.length;
      loadMore().then(() => { if (state.items.length > before) goTo(index, animate); });
      return;
    }
    const target = Math.max(0, Math.min(state.items.length - 1, index));
    if (target === state.active) return;
    setActive(target, animate);
  }

  function goToEnd() {
    if (!state.items.length) return;
    goTo(state.items.length - 1);
  }

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
      fav: null, like: null, hint: null, soundHint: null, playBtn: null, flash: null,
      bubble: null, bar: null, barWrap: null, sound: null, gear: null, panel: null,
      setLoop: null, setAuto: null, setNote: null, seekRatio: 0,
      seeking: false, flashTimer: 0, hintTimer: 0,
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
    entry.bubble = el("div", { class: "seek-bubble hidden", text: "0:00" });
    entry.bar = el("div", { class: "bar" }, entry.fill);
    entry.barWrap = el("div", { class: "bar-wrap" }, entry.bar, entry.bubble);
    wireSeek(entry);

    layer.append(el("div", { class: "overlay" },
      el("div", { class: "ov-top" },
        el("div", { class: "ov-title", text: item.title || ("#" + item.id) }),
        el("div", { class: "ov-actions" }, entry.fav, entry.like)),
      entry.barWrap,
      el("div", { class: "times" }, entry.elapsed, entry.total)
    ));

    // 左上角"来自 <库名>"（点击切范围），右上角声音与设置
    layer.append(libraryCorner(item),
      el("div", { class: "ov-corner right" }, soundButton(entry), gearButton(entry)));

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
    video.loop = loopEnabled();
    video.src = item.stream_url;
    entry.video = video;
    video.addEventListener("loadedmetadata", () => onMetadata(entry));
    video.addEventListener("loadeddata", () => checkFrames(entry));
    video.addEventListener("timeupdate", () => paintTime(entry));
    video.addEventListener("play", () => {
      hidePlayButton(entry);
      if (!state.soundOn) showSoundHint(entry);
    });
    video.addEventListener("ended", () => onEnded(entry));
    video.addEventListener("error", () => showBroken(entry));
    video.addEventListener("click", () => togglePlay(entry));
    stage.append(video);
    return entry;
  }

  function centerMessage(title, sub) {
    return el("div", { class: "center-msg" },
      el("div", { class: "center-inner" },
        el("div", { class: "center-title", text: title }),
        el("div", { class: "center-sub", text: sub })));
  }

  function libraryCorner(item) {
    const name = item.library_name || "未知库";
    const here = el("button", { class: "lib-chip", type: "button", title: "只看这个库", text: "来自 " + name });
    here.addEventListener("click", (event) => {
      event.stopPropagation();
      switchScope(item.library_id, name);
    });
    const corner = el("div", { class: "ov-corner left" }, here);
    if (state.scope) {
      const back = el("button", { class: "lib-chip all", type: "button", text: "全部库" });
      back.addEventListener("click", (event) => {
        event.stopPropagation();
        switchScope("", "");
      });
      corner.append(back);
    }
    return corner;
  }

  function destroyEntry(index) {
    const entry = state.built.get(index);
    if (!entry) return;
    entry.destroyed = true;
    if (entry.hintTimer) clearTimeout(entry.hintTimer);
    if (entry.flashTimer) clearTimeout(entry.flashTimer);
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

  function tryPlay(entry) {
    const result = entry.video.play();
    if (result && typeof result.catch === "function") {
      result.catch(() => showPlayButton(entry));
    }
  }

  /* ---------- 声音（角落按钮，不再是单击画面） ---------- */

  function paintSound(entry) {
    if (entry && entry.sound) entry.sound.textContent = state.soundOn ? "🔊" : "🔇";
  }

  function setSound(on) {
    state.soundOn = !!on;
    writeSoundPref(state.soundOn);
    for (const entry of state.built.values()) {
      if (entry.video) entry.video.muted = !state.soundOn;
      paintSound(entry);
      if (state.soundOn) hideSoundHint(entry);
    }
  }

  function soundButton(entry) {
    entry.sound = el("button", { class: "icon-btn", type: "button", title: "声音" });
    paintSound(entry);
    entry.sound.addEventListener("click", (event) => {
      event.stopPropagation();
      setSound(!state.soundOn);
    });
    return entry.sound;
  }

  function showSoundHint(entry) {
    if (entry.destroyed || !entry.layer) return;
    if (!entry.soundHint) {
      entry.soundHint = el("div", { class: "hint low", text: "点右上角喇叭开启声音" });
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

  /* ---------- 单击播放/暂停（中央 300ms 提示） ---------- */

  function flash(entry, text) {
    if (entry.destroyed || !entry.layer) return;
    if (!entry.flash) {
      entry.flash = el("div", { class: "center-flash" });
      entry.layer.append(entry.flash);
    }
    entry.flash.textContent = text;
    entry.flash.classList.remove("fade");
    if (entry.flashTimer) clearTimeout(entry.flashTimer);
    entry.flashTimer = setTimeout(() => {
      if (entry.flash) entry.flash.classList.add("fade");
    }, 300);
  }

  function togglePlay(entry) {
    if (entry.destroyed || entry.broken || !entry.video) return;
    if (entry.video.paused) {
      const result = entry.video.play();
      if (result && typeof result.catch === "function") result.catch(() => showPlayButton(entry));
      flash(entry, "▶");
    } else {
      entry.video.pause();
      flash(entry, "⏸");
    }
  }

  function showPlayButton(entry) {
    if (entry.playRejected || entry.destroyed || !entry.video || !entry.layer) return;
    entry.playRejected = true;
    entry.playBtn = el("button", {
      class: "center-btn", type: "button", text: "点击播放",
      onclick: () => {
        hidePlayButton(entry);
        setSound(true);
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

  function onEnded(entry) {
    flushProgress(entry.index, false);
    if (state.settings.autoplay_next && (entry.index + 1 < state.items.length || state.hasMore)) {
      goTo(entry.index + 1); // 连播：自动下一个
      return;
    }
    if (!state.settings.loop_play) showPlayButton(entry); // 连播关且循环关：停在末尾等重播
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
    if (!video || entry.seeking) return; // 拖动中由手指决定显示
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

  /* ---------- 进度：点击 / 拖动 seek ---------- */

  function wireSeek(entry) {
    const ratioAt = (clientX) => {
      const rect = entry.bar.getBoundingClientRect();
      if (!rect.width) return 0;
      return Math.max(0, Math.min(1, (clientX - rect.left) / rect.width));
    };
    const paintRatio = (ratio) => {
      const duration = durationMs(entry);
      const text = fmtDuration(ratio * duration);
      entry.fill.style.width = (ratio * 100) + "%";
      entry.elapsed.textContent = text;
      entry.bubble.textContent = text;
      entry.bubble.style.left = (ratio * 100) + "%";
    };
    entry.barWrap.addEventListener("pointerdown", (event) => {
      if (!entry.video) return;
      event.preventDefault();
      event.stopPropagation();
      entry.seeking = true;
      entry.seekRatio = ratioAt(event.clientX);
      try { entry.barWrap.setPointerCapture(event.pointerId); } catch (err) { /* 忽略 */ }
      entry.bubble.classList.remove("hidden");
      paintRatio(entry.seekRatio);
    });
    entry.barWrap.addEventListener("pointermove", (event) => {
      if (!entry.seeking) return;
      event.preventDefault();
      entry.seekRatio = ratioAt(event.clientX);
      paintRatio(entry.seekRatio);
    });
    const finish = (event) => {
      if (!entry.seeking) return;
      entry.seeking = false;
      if (event && event.pointerId !== undefined) {
        try { entry.barWrap.releasePointerCapture(event.pointerId); } catch (err) { /* 忽略 */ }
      }
      entry.bubble.classList.add("hidden");
      const duration = durationMs(entry);
      if (entry.video && duration > 0) {
        try { entry.video.currentTime = (entry.seekRatio * duration) / 1000; } catch (err) { /* 元数据未就绪 */ }
      }
      paintTime(entry);
      // 松手立刻上报，不等 5s 定时器（item 3）
      if (!entry.broken) api.patchProgress(entry.item.id, progressPayload(entry)).catch(() => {});
    };
    entry.barWrap.addEventListener("pointerup", finish);
    entry.barWrap.addEventListener("pointercancel", finish);
  }

  /* ---------- 播放设置（右上 ⚙ 二级面板） ---------- */

  function loopEnabled() {
    return !!state.settings.loop_play && !state.settings.autoplay_next;
  }

  function paintPanel(entry) {
    if (!entry.panel) return;
    entry.setAuto.checked = !!state.settings.autoplay_next;
    entry.setLoop.checked = !!state.settings.loop_play;
    entry.setLoop.disabled = !!state.settings.autoplay_next;
    entry.setNote.classList.toggle("hidden", !state.settings.autoplay_next);
  }

  function buildPanel(entry) {
    entry.setAuto = el("input", { type: "checkbox" });
    entry.setAuto.addEventListener("change", () => saveSettings({ autoplay_next: entry.setAuto.checked }));
    entry.setLoop = el("input", { type: "checkbox" });
    entry.setLoop.addEventListener("change", () => saveSettings({ loop_play: entry.setLoop.checked }));
    entry.setNote = el("div", { class: "set-note", text: "连播开启时循环不生效" });
    entry.panel = el("div", { class: "set-panel hidden" },
      el("label", { class: "set-row" }, entry.setAuto, el("span", { text: "自动播放下一个" })),
      el("label", { class: "set-row" }, entry.setLoop, el("span", { text: "循环播放" })),
      entry.setNote);
    entry.panel.addEventListener("click", (event) => event.stopPropagation());
    return entry.panel;
  }

  function closePanels() {
    for (const entry of state.built.values()) {
      if (entry.panel) entry.panel.classList.add("hidden");
    }
  }

  function gearButton(entry) {
    entry.gear = el("button", { class: "icon-btn", type: "button", title: "播放设置", text: "⚙" });
    entry.gear.addEventListener("click", (event) => {
      event.stopPropagation();
      if (!entry.panel) entry.layer.append(buildPanel(entry));
      const open = entry.panel.classList.contains("hidden");
      closePanels();
      paintPanel(entry);
      if (open) entry.panel.classList.remove("hidden");
    });
    return entry.gear;
  }

  function applySettings() {
    const loop = loopEnabled();
    for (const entry of state.built.values()) {
      if (entry.video) entry.video.loop = loop;
      paintPanel(entry);
    }
  }

  async function saveSettings(partial) {
    const before = state.settings;
    state.settings = Object.assign({}, state.settings, partial);
    applySettings();
    try {
      const result = await api.patchFeedSettings(partial);
      if (result) state.settings = result;
    } catch (err) {
      state.settings = before;
      showToast(err && err.message ? err.message : "设置保存失败");
    }
    applySettings();
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

  function setLoading(on) {
    if (on) {
      if (!state.items.length) {
        if (!state.infoCard) state.infoCard = el("div", { class: "card info", text: "加载中…" });
        if (!state.infoCard.parentNode) feed.append(state.infoCard);
        return;
      }
      chip.textContent = "加载中…";
      chip.classList.remove("hidden");
      return;
    }
    if (state.infoCard && state.infoCard.parentNode) state.infoCard.remove();
    chip.classList.add("hidden");
  }

  function appendItems(list, meta) {
    if (meta && meta.settings) {
      state.settings = meta.settings;
      applySettings();
    }
    for (const item of list) {
      const index = state.items.length;
      const shell = el("article", { class: "card", dataset: { index: String(index), id: String(item.id) } });
      shell.style.top = (index * 100) + "%";
      state.items.push(item);
      state.shells.push(shell);
      track.append(shell);
    }
    state.hasMore = !!(meta && meta.has_more);
    if (!state.items.length && !state.hasMore && !state.emptyCard) {
      state.emptyCard = el("div", { class: "card info", text: "还没有视频" });
      feed.append(state.emptyCard);
    }
    if (state.items.length && state.active < 0) setActive(0, false);
  }

  async function loadMore() {
    if (state.loading || !state.hasMore) return false;
    state.loading = true;
    setLoading(true);
    try {
      const result = await api.feedNext({
        library_id: state.scope || undefined,
        cursor: state.nextCursor || undefined,
        limit: BATCH,
      });
      const list = result && result.data && Array.isArray(result.data.list) ? result.data.list : [];
      const meta = (result && result.meta) || {};
      state.nextCursor = meta.next_cursor || "";
      // 空批次必须停，否则 has_more 说谎时会无限翻页（坑 9）
      appendItems(list, { has_more: !!meta.has_more && list.length > 0, settings: meta.settings });
      return true;
    } catch (err) {
      showToast(err && err.message ? err.message : "加载失败");
      return false;
    } finally {
      state.loading = false;
      setLoading(false);
    }
  }

  function maybeLoadMore(index) {
    if (state.hasMore && !state.loading && index >= state.items.length - 3) loadMore();
  }

  /* ---------- 范围切换（来自 <库名>） ---------- */

  function switchScope(libraryID, libraryName) {
    const next = libraryID || "";
    if (state.scope === next) return;
    resetFeed(next, libraryName || "");
  }

  function resetFeed(libraryID, libraryName) {
    for (const index of Array.from(state.built.keys())) destroyEntry(index);
    for (const shell of state.shells) shell.remove();
    state.items = [];
    state.shells = [];
    state.active = -1;
    state.hasMore = true;
    state.loading = false;
    state.nextCursor = "";
    state.scope = libraryID;
    state.scopeName = libraryName;
    if (state.emptyCard && state.emptyCard.parentNode) state.emptyCard.remove();
    state.emptyCard = null;
    closePanels();
    gate.settle();
    paintTrack(false);
    showToast(libraryID ? ("只看 " + libraryName) : "已切回全部库");
    loadMore();
  }

  /* ---------- 手势与键盘 ---------- */

  let wheelAcc = 0;
  let touchY = null;

  function onWheel(event) {
    event.preventDefault();
    const now = Date.now();
    wheelAcc += event.deltaY;
    gate.input(now);
    if (Math.abs(wheelAcc) < WHEEL_STEP) return;
    const direction = wheelAcc > 0 ? 1 : -1;
    wheelAcc = 0;
    const step = gate.take(now, direction);
    if (step) goTo(state.active + step);
  }

  function onTouchStart(event) {
    if (event.touches.length !== 1) return;
    if (isInteractive(event.target)) { touchY = null; return; }
    touchY = event.touches[0].clientY;
    gate.begin(Date.now());
  }

  function onTouchEnd(event) {
    if (touchY === null) return;
    const touch = event.changedTouches && event.changedTouches[0];
    if (!touch) { touchY = null; return; }
    const delta = touchY - touch.clientY;
    touchY = null;
    if (Math.abs(delta) < TOUCH_STEP) return;
    const step = gate.take(Date.now(), delta > 0 ? 1 : -1);
    if (step) goTo(state.active + step);
  }

  function onKeyDown(event) {
    const target = event.target;
    const tag = target && target.tagName ? target.tagName : "";
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (target && target.isContentEditable)) return;
    if (event.key === " " && tag === "BUTTON") return;
    if (event.key === "Escape") { closePanels(); return; }
    if (event.key === "ArrowDown" || event.key === "PageDown" || event.key === " " || event.key === "Spacebar") {
      event.preventDefault();
      goTo(state.active + 1);
    } else if (event.key === "ArrowUp" || event.key === "PageUp") {
      event.preventDefault();
      goTo(state.active - 1);
    } else if (event.key === "Home") {
      event.preventDefault();
      goTo(0);
    } else if (event.key === "End") {
      event.preventDefault();
      goToEnd();
    }
  }

  track.addEventListener("transitionend", onTransitionEnd);
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
    if (settleTimer) clearTimeout(settleTimer);
    track.removeEventListener("transitionend", onTransitionEnd);
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
