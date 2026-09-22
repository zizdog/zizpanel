// 剧场管理（仅管理员）：编辑 / 加入已有媒体 / 排序 / 移除 / 删除（条目 9）
// 只写引用关系，不复制、不上传、不删除任何媒体文件。

import { api } from "./api.js";
import { el, clear, banner, setBanner, field, input, fmtDuration } from "./dom.js";
import { confirmDialog } from "./confirm.js";

export function mountSeriesAdmin(box, series, options) {
  const opts = options || {};
  const note = banner();
  const titleInput = input({ value: series.title || "" });
  const descInput = input({ value: series.description || "" });
  const save = el("button", { class: "btn primary", type: "submit", text: "保存" });
  const editForm = el("form", { class: "panel" },
    field("标题", titleInput), field("简介", descInput),
    el("div", { class: "actions" }, save), note);

  const searchInput = input({ placeholder: "搜索媒体标题" });
  const searchBtn = el("button", { class: "btn small", type: "button", text: "搜索" });
  const results = el("div", { class: "pick-list", dataset: { role: "media-picker" } });
  const addBtn = el("button", { class: "btn primary", type: "button", text: "加入所选", disabled: true });
  const picker = el("div", { class: "panel" },
    el("div", { class: "muted small-note", text: "只能引用已扫描到的媒体，不会复制文件。" }),
    el("div", { class: "muted small-note", text: "文件名带 S01E01/EP03/第3集 才会认成集号" }),
    el("div", { class: "row" }, searchInput, searchBtn),
    results, el("div", { class: "actions" }, addBtn));

  const episodes = el("div", { class: "ep-admin-list", dataset: { role: "series-episodes" } });
  const epOrderNote = el("div", { class: "muted small-note", hidden: true, dataset: { role: "episode-order-note" } });
  const epPanel = el("div", { class: "panel" },
    el("div", { class: "muted small-note", text: "剧集顺序（↑↓ 调整，✕ 移出）" }),
    epOrderNote, episodes);
  // 补丁 R1：按文件名识别季/集号；先给变化清单，确认后才落库。
  const detectBtn = el("button", {
    class: "btn small", type: "button", text: "自动识别剧集", dataset: { role: "series-detect" },
  });
  const detectBox = el("div", { class: "detect-box", hidden: true, dataset: { role: "series-detect-changes" } });
  const detectPanel = el("div", { class: "panel" },
    el("div", { class: "row" }, detectBtn),
    el("div", { class: "muted small-note", text: "按文件名识别，识别不到的不猜；手动排过的不会被覆盖。" }),
    detectBox);
  const delBtn = el("button", { class: "btn danger", type: "button", text: "删除剧场", dataset: { role: "series-delete" } });
  box.append(editForm, picker, detectPanel, epPanel, el("div", { class: "actions" }, delBtn));

  let ids = [];
  const mediaByID = {};
  const entryByID = {};

  editForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    save.disabled = true;
    try {
      await api.request("PATCH", "/api/v1/admin/series/" + encodeURIComponent(series.id),
        { title: titleInput.value.trim(), description: descInput.value.trim() });
      setBanner(note, "已保存");
      if (opts.onChanged) opts.onChanged();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "保存失败");
    } finally {
      save.disabled = false;
    }
  });

  delBtn.addEventListener("click", async () => {
    const ok = await confirmDialog({
      title: "删除剧场",
      message: "只会删除剧场的分组，不会删除任何媒体文件或媒体记录。",
      confirmText: "删除",
    });
    if (!ok) return;
    try {
      await api.request("DELETE", "/api/v1/admin/series/" + encodeURIComponent(series.id));
      if (opts.onDeleted) opts.onDeleted();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "删除失败");
    }
  });

  async function loadPicker() {
    clear(results);
    results.append(el("div", { class: "muted small-note", text: "加载中…" }));
    try {
      const res = await api.mediaList({ per_page: 100, status: "ready", q: searchInput.value.trim() });
      const list = res && res.data && Array.isArray(res.data.list) ? res.data.list : [];
      clear(results);
      if (!list.length) {
        results.append(el("div", { class: "muted small-note", text: "没有匹配的媒体" }));
        return;
      }
      for (const item of list) {
        const box2 = el("input", { type: "checkbox", value: String(item.id) });
        box2.addEventListener("change", () => { addBtn.disabled = !results.querySelector("input:checked"); });
        results.append(el("label", { class: "pick-row" },
          box2,
          el("span", { class: "pick-title", text: item.title || ("#" + item.id) }),
          el("span", { class: "muted small-note", text: fmtDuration(item.duration_ms) })));
      }
    } catch (err) {
      clear(results);
      results.append(el("div", { class: "muted small-note", text: err && err.message ? err.message : "加载失败" }));
    }
  }

  searchBtn.addEventListener("click", loadPicker);
  searchInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") { event.preventDefault(); loadPicker(); }
  });

  addBtn.addEventListener("click", async () => {
    const picked = Array.from(results.querySelectorAll("input:checked")).map((node) => node.value);
    if (!picked.length) return;
    addBtn.disabled = true;
    try {
      const data = await api.request("POST", "/api/v1/admin/series/" + encodeURIComponent(series.id) + "/media",
        { media_ids: picked });
      const added = data && typeof data.added === "number" ? data.added : 0;
      const detected = data && typeof data.detected === "number" ? data.detected : 0;
      const skipped = picked.length - added;
      setBanner(note, "已加入 " + added + " 集（识别到 " + detected + " 集）"
        + (skipped > 0 ? "，跳过 " + skipped + " 集已在剧场中" : ""));
      await load();
      await loadPicker();
      if (opts.onChanged) opts.onChanged();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "加入失败");
    }
  });

  function paintEpisodes() {
    clear(episodes);
    if (!ids.length) {
      episodes.append(el("div", { class: "muted small-note", text: "还没有剧集" }));
      return;
    }
    let unrecognized = 0;
    ids.forEach((mediaID, index) => {
      const item = mediaByID[mediaID] || {};
      const entry = entryByID[mediaID] || {};
      const label = entry.episode_label || ("第 " + (index + 1) + " 集");
      if (label === "未识别") unrecognized++;
      episodes.append(el("div", { class: "ep-admin-row", dataset: { media: mediaID } },
        el("span", { class: "ep-no", text: label, dataset: { role: "ep-label" } }),
        el("span", { class: "ep-title", text: item.title || mediaID }),
        entry.episode_source === "manual"
          ? el("span", { class: "muted small-note", text: "手动" })
          : null,
        el("span", { class: "row" },
          moveButton("↑", index, -1), moveButton("↓", index, 1),
          el("button", {
            class: "btn small danger", type: "button", text: "✕", dataset: { role: "ep-remove", media: mediaID },
            onclick: () => removeEpisode(mediaID),
          }))));
    });
    epOrderNote.hidden = unrecognized === 0;
    epOrderNote.textContent = unrecognized > 0 ? "未识别（按文件名排）共 " + unrecognized + " 集" : "";
  }

  function moveButton(label, index, delta) {
    const target = index + delta;
    return el("button", {
      class: "btn small", type: "button", text: label, disabled: target < 0 || target >= ids.length,
      dataset: { role: delta < 0 ? "ep-up" : "ep-down", media: ids[index] },
      onclick: () => move(index, delta),
    });
  }

  async function move(index, delta) {
    const target = index + delta;
    if (target < 0 || target >= ids.length) return;
    const next = ids.slice();
    const tmp = next[index];
    next[index] = next[target];
    next[target] = tmp;
    try {
      await api.request("PUT", "/api/v1/admin/series/" + encodeURIComponent(series.id) + "/order",
        { media_ids: next });
      await load();
      if (opts.onChanged) opts.onChanged();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "排序失败");
      await load();
    }
  }

  async function removeEpisode(mediaID) {
    const ok = await confirmDialog({
      title: "移出剧场",
      message: "只从剧场移除这一集，媒体记录与文件都保留。",
      confirmText: "移出",
    });
    if (!ok) return;
    try {
      await api.request("DELETE",
        "/api/v1/admin/series/" + encodeURIComponent(series.id) + "/media/" + encodeURIComponent(mediaID));
      await load();
      if (opts.onChanged) opts.onChanged();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "移除失败");
    }
  }

  function renderChanges(data) {
    clear(detectBox);
    const changes = data && Array.isArray(data.changes) ? data.changes : [];
    const manualSkipped = data && typeof data.manual_skipped === "number" ? data.manual_skipped : 0;
    if (!changes.length) {
      detectBox.hidden = false;
      detectBox.append(el("div", { class: "muted small-note",
        text: "没有需要变更的剧集" + (manualSkipped > 0 ? "（" + manualSkipped + " 集手动排过，已跳过）" : "") }));
      return;
    }
    const applyBtn = el("button", {
      class: "btn small primary", type: "button", text: "确认应用（" + changes.length + " 集）",
      dataset: { role: "series-detect-confirm" },
      onclick: () => applyChanges(changes),
    });
    const cancelBtn = el("button", {
      class: "btn small", type: "button", text: "取消", dataset: { role: "series-detect-cancel" },
      onclick: () => { detectBox.hidden = true; },
    });
    detectBox.hidden = false;
    detectBox.append(
      el("div", { class: "muted small-note", text: "变化清单（确认前不会写库）" }),
      ...changes.map((c) => el("div", { class: "detect-row", dataset: { role: "detect-row", media: c.media_id } },
        el("span", { class: "ep-no", text: (c.old_label || "未识别") + " → " + (c.new_label || "未识别") }),
        el("span", { class: "ep-title", text: c.filename || c.title || c.media_id }))),
      el("div", { class: "row" }, applyBtn, cancelBtn));
  }

  async function applyChanges(changes) {
    try {
      const data = await api.request("POST", detectURL(), { confirm: true });
      const updated = data && typeof data.updated === "number" ? data.updated : changes.length;
      setBanner(note, "已识别 " + updated + " 集");
      detectBox.hidden = true;
      await load();
      if (opts.onChanged) opts.onChanged();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "识别失败");
    }
  }

  function detectURL() {
    return "/api/v1/admin/series/" + encodeURIComponent(series.id) + "/detect";
  }

  detectBtn.addEventListener("click", async () => {
    setBanner(note, "");
    detectBtn.disabled = true;
    try {
      const data = await api.request("POST", detectURL(), { confirm: false });
      renderChanges(data);
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "识别失败");
    } finally {
      detectBtn.disabled = false;
    }
  });

  async function load() {
    const data = await api.request("GET", "/api/v1/series/" + encodeURIComponent(series.id));
    const list = data && Array.isArray(data.list) ? data.list : [];
    ids = list.map((entry) => entry.media && entry.media.id).filter(Boolean);
    for (const key of Object.keys(mediaByID)) delete mediaByID[key];
    for (const key of Object.keys(entryByID)) delete entryByID[key];
    for (const entry of list) {
      if (entry.media) mediaByID[entry.media.id] = entry.media;
      if (entry.media) entryByID[entry.media.id] = entry;
    }
    if (data && data.series) {
      if (document.activeElement !== titleInput) titleInput.value = data.series.title || "";
      if (document.activeElement !== descInput) descInput.value = data.series.description || "";
    }
    paintEpisodes();
  }

  load().catch((err) => setBanner(note, err && err.message ? err.message : "加载失败"));
  loadPicker();
  return null;
}
