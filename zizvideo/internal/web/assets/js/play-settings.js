// 播放设置：首页 ⚙ 与「我的」共用同一份控件语义 + 同一个 PATCH /api/v1/feed/settings。
// 三项：自动播放下一个 / 循环播放 / 左右键跳转秒数。

import { el } from "./dom.js";

export const FEED_SETTINGS_DEFAULTS = { loop_play: false, autoplay_next: true, seek_seconds: 10 };

// normalizeFeedSettings 把任意响应收敛成三项合法值：非法/缺省一律用默认（10 秒、上限 120）。
export function normalizeFeedSettings(data) {
  const src = data && typeof data === "object" ? data : {};
  const raw = Number(src.seek_seconds);
  const seek = Number.isFinite(raw) && raw >= 1 ? Math.min(120, Math.round(raw)) : FEED_SETTINGS_DEFAULTS.seek_seconds;
  return {
    loop_play: !!src.loop_play,
    autoplay_next: src.autoplay_next === undefined ? FEED_SETTINGS_DEFAULTS.autoplay_next : !!src.autoplay_next,
    seek_seconds: seek,
  };
}

export function seekSecondsOf(settings) {
  return normalizeFeedSettings(settings).seek_seconds;
}

// 自动播下一个开着时循环不生效（与后端 loop_effective 同义）。
export function loopEffective(settings) {
  const s = normalizeFeedSettings(settings);
  return !!s.loop_play && !s.autoplay_next;
}

// createFeedSettingsForm 只画控件并回调 onChange(partial)；状态与请求由调用方持有。
export function createFeedSettingsForm(options) {
  const opts = options || {};
  const auto = el("input", { type: "checkbox" });
  const loop = el("input", { type: "checkbox" });
  const seek = el("input", { type: "number", min: "1", max: "120", step: "1" });
  const note = el("div", { class: "set-note", text: "连播开启时循环不生效" });
  const autoRow = el("label", { class: "set-row" }, auto, el("span", { text: "自动播放下一个" }));
  const loopRow = el("label", { class: "set-row" }, loop, el("span", { text: "循环播放" }));
  const seekRow = el("label", { class: "set-row" }, el("span", { text: "左右键跳转" }), seek, el("span", { text: "秒" }));
  const form = el("div", { class: "set-form" }, autoRow, loopRow, seekRow, note);

  function emit(partial) {
    if (opts.onChange) opts.onChange(partial);
  }

  let last = normalizeFeedSettings(opts.settings);

  auto.addEventListener("change", () => emit({ autoplay_next: auto.checked }));
  loop.addEventListener("change", () => emit({ loop_play: loop.checked }));
  seek.addEventListener("change", () => {
    const raw = Number(seek.value);
    const next = Number.isFinite(raw) ? Math.max(1, Math.min(120, Math.round(raw))) : last.seek_seconds;
    seek.value = String(next);
    emit({ seek_seconds: next });
  });

  function paint(settings) {
    last = normalizeFeedSettings(settings || last);
    auto.checked = !!last.autoplay_next;
    loop.checked = !!last.loop_play;
    loop.disabled = !!last.autoplay_next;
    seek.value = String(last.seek_seconds);
    note.classList.toggle("hidden", !last.autoplay_next);
  }

  paint();
  return { node: form, paint };
}
