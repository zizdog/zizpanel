// 按目录建剧场：选目录 → 只遍历这一个目录 → 预览 → 确认。
// 不扫全库、不搜全库；集号一律由后端用 internal/detect 识别。
import { api } from "./api.js";
import { el, clear, banner, setBanner } from "./dom.js";
import { openDirectoryPicker } from "./roots.js";

const PREVIEW_ROWS = 100;

function baseName(path) {
  const parts = String(path || "").split("/");
  return parts[parts.length - 1] || path || "";
}

// modal 建一个统一的抽屉/弹层；onClose 用来清理轮询定时器。
function modal(title, onClose) {
  const note = banner();
  const body = el("div", { class: "panel" });
  const overlay = el("div", { class: "modal-overlay" });
  const close = () => {
    overlay.remove();
    if (onClose) onClose();
  };
  const box = el("div", { class: "modal wide" },
    el("div", { class: "picker-head" },
      el("span", { text: title }),
      el("button", { class: "btn small", type: "button", text: "关闭", onclick: close })),
    note, body);
  overlay.append(box);
  overlay.addEventListener("click", (event) => { if (event.target === overlay) close(); });
  document.body.append(overlay);
  return { overlay, body, note, close };
}

// pickImportDir 把目录选择器起点放在第一个可用的允许根上（越界仍由后端拒绝）。
export async function pickImportDir(onPicked) {
  let start = "/";
  try {
    const data = await api.mediaRoots();
    const usable = (data && Array.isArray(data.roots) ? data.roots : [])
      .find((root) => root && root.status === "ok" && root.path);
    if (usable) start = usable.path;
  } catch (err) { /* 拉不到就退回文件系统根，仍由后端校验 */ }
  openDirectoryPicker({ start, onPicked });
}

function depthControls() {
  const recursive = el("input", { type: "checkbox", dataset: { role: "dir-recursive" } });
  const depthInput = el("input", {
    class: "input tiny", type: "number", min: "1", max: "5", value: "1",
    dataset: { role: "dir-depth" },
  });
  const depthRow = el("label", { class: "row", hidden: true },
    el("span", { class: "muted small-note", text: "只下钻" }), depthInput,
    el("span", { class: "muted small-note", text: "层" }));
  recursive.addEventListener("change", () => { depthRow.hidden = !recursive.checked; });
  const row = el("div", { class: "row" },
    el("label", { class: "row" }, recursive,
      el("span", { class: "muted small-note", text: "包含子目录" })), depthRow);
  return { row, body: () => ({ recursive: recursive.checked, depth: Number(depthInput.value) || 1 }) };
}

/* ---------- A：剧场内「从目录导入剧集」 ---------- */

export function importDirIntoSeries(series, onDone) {
  pickImportDir((path) => openDirPanel(series, path, onDone)).catch(() => {});
}

function openDirPanel(series, path, onDone) {
  const panel = modal("从目录导入剧集");
  const controls = depthControls();
  const previewBtn = el("button", { class: "btn small", type: "button", text: "预览", dataset: { role: "dir-preview" } });
  const importBtn = el("button", { class: "btn primary", type: "button", text: "确认导入", disabled: true, dataset: { role: "dir-confirm" } });
  const result = el("div", { class: "pick-list", dataset: { role: "dir-preview-result" } });
  panel.body.append(
    el("div", { class: "muted small-note", text: path }),
    el("div", { class: "muted small-note", text: "只导入这个目录，不会扫描媒体库" }),
    controls.row,
    el("div", { class: "actions" }, previewBtn, importBtn),
    result);

  const requestBody = () => Object.assign({ path }, controls.body());

  async function preview() {
    setBanner(panel.note, "正在统计…");
    previewBtn.disabled = true;
    importBtn.disabled = true;
    clear(result);
    try {
      const data = await api.seriesDirPreview(series.id, requestBody());
      renderPreview(data, result);
      importBtn.disabled = Number(data && data.total_files) === 0;
      setBanner(panel.note, "共 " + (Number(data.total_files) || 0) + " 个文件：识别 "
        + (Number(data.recognized) || 0) + " 集 / 未识别 " + (Number(data.unidentified) || 0) + " 个");
    } catch (err) {
      setBanner(panel.note, err && err.message ? err.message : "预览失败");
    } finally {
      previewBtn.disabled = false;
    }
  }

  previewBtn.addEventListener("click", preview);

  importBtn.addEventListener("click", async () => {
    importBtn.disabled = true;
    setBanner(panel.note, "正在导入…");
    try {
      const data = await api.seriesDirImport(series.id, requestBody());
      const added = Number(data && data.added) || 0;
      const skipped = Number(data && data.skipped) || 0;
      setBanner(panel.note, "已加入 " + added + " 集（新登记 "
        + (Number(data && data.registered) || 0) + " 条媒体）"
        + (skipped > 0 ? "，跳过 " + skipped + " 集已在剧场中" : ""));
      importBtn.disabled = false;
      if (onDone) await onDone();
      await preview();
    } catch (err) {
      setBanner(panel.note, err && err.message ? err.message : "导入失败");
      importBtn.disabled = false;
    }
  });

  preview();
}

function renderPreview(data, box) {
  clear(box);
  const rows = Array.isArray(data && data.entries) ? data.entries : [];
  if (!rows.length) {
    box.append(el("div", { class: "muted small-note", text: "这个目录下没有可导入的视频" }));
    return;
  }
  for (const entry of rows.slice(0, PREVIEW_ROWS)) {
    box.append(el("div", { class: "detect-row" },
      el("span", { class: "ep-no", text: entry.episode_label || "未识别" }),
      el("span", { class: "ep-title", text: baseName(entry.path) }),
      entry.in_series ? el("span", { class: "muted small-note", text: "已在剧场" }) : null,
      entry.other_series_title
        ? el("span", { class: "muted small-note", text: "已在《" + entry.other_series_title + "》" })
        : null));
  }
  if (rows.length > PREVIEW_ROWS) {
    box.append(el("div", { class: "muted small-note", text: "只显示前 " + PREVIEW_ROWS + " 条，共 " + rows.length + " 条" }));
  }
}

/* ---------- B：剧场列表「按子目录批量建剧场」 ---------- */

export function batchImportSeries(onDone) {
  pickImportDir((path) => openBatchPanel(path, onDone)).catch(() => {});
}

function openBatchPanel(path, onDone) {
  let timer = 0;
  const panel = modal("按子目录批量建剧场", () => { if (timer) clearInterval(timer); timer = 0; });
  const previewBtn = el("button", { class: "btn small", type: "button", text: "预览", dataset: { role: "dirs-preview" } });
  const runBtn = el("button", { class: "btn primary", type: "button", text: "确认建剧场", disabled: true, dataset: { role: "dirs-confirm" } });
  const result = el("div", { class: "pick-list", dataset: { role: "dirs-preview-result" } });
  panel.body.append(
    el("div", { class: "muted small-note", text: path }),
    el("div", { class: "muted small-note", text: "每个一级子目录 = 一个剧场（子目录内递归识别）" }),
    el("div", { class: "actions" }, previewBtn, runBtn),
    result);

  async function preview() {
    setBanner(panel.note, "正在统计…");
    previewBtn.disabled = true;
    runBtn.disabled = true;
    clear(result);
    try {
      const data = await api.seriesDirsPreview({ path });
      renderBatchPreview(data, result);
      runBtn.disabled = !(Number(data && data.subdirs) > 0);
      setBanner(panel.note, (Number(data && data.subdirs) || 0) + " 个子目录，共 "
        + (Number(data && data.file_count) || 0) + " 个视频");
    } catch (err) {
      setBanner(panel.note, err && err.message ? err.message : "预览失败");
    } finally {
      previewBtn.disabled = false;
    }
  }

  previewBtn.addEventListener("click", preview);

  runBtn.addEventListener("click", async () => {
    runBtn.disabled = true;
    try {
      const data = await api.seriesDirsImport({ path });
      const taskId = data && data.task_id;
      if (!taskId) {
        setBanner(panel.note, (data && data.note) || "没有可建的剧场");
        return;
      }
      setBanner(panel.note, "已提交，共 " + (Number(data.total) || 0) + " 个目录");
      pollJob(taskId);
    } catch (err) {
      setBanner(panel.note, err && err.message ? err.message : "提交失败");
      runBtn.disabled = false;
    }
  });

  function pollJob(taskId) {
    if (timer) clearInterval(timer);
    timer = setInterval(async () => {
      let task;
      try {
        task = await api.jobTask(taskId);
      } catch (err) {
        clearInterval(timer);
        timer = 0;
        setBanner(panel.note, err && err.message ? err.message : "查询失败");
        return;
      }
      if (task.status === "pending" || task.status === "running") {
        setBanner(panel.note, "已处理 " + (Number(task.processed) || 0) + "/"
          + (Number(task.total) || 0) + " 个目录 · 新增 " + (Number(task.updated) || 0) + " 集…");
        return;
      }
      clearInterval(timer);
      timer = 0;
      const summary = task.summary || {};
      const label = task.status === "success" ? "完成：" : task.status === "failed" ? "部分失败：" : "已中断：";
      let text = label + "新建 " + (Number(summary.series_created) || 0) + " 个、补 "
        + (Number(summary.series_reused) || 0) + " 个，新增 " + (Number(task.updated) || 0) + " 集";
      if (task.error) text += "；" + task.error;
      if (task.degraded && task.degrade_reason) text += "；" + task.degrade_reason;
      setBanner(panel.note, text);
      runBtn.disabled = false;
      if (onDone) onDone();
    }, 1000);
  }

  preview();
}

function renderBatchPreview(data, box) {
  clear(box);
  const rows = Array.isArray(data && data.entries) ? data.entries : [];
  if (!rows.length) {
    box.append(el("div", { class: "muted small-note", text: "这个目录下没有子目录" }));
    return;
  }
  for (const entry of rows.slice(0, PREVIEW_ROWS)) {
    box.append(el("div", { class: "detect-row" },
      el("span", { class: "ep-no", text: entry.name || "-" }),
      el("span", { class: "ep-title", text: "识别 " + (entry.recognized || 0) + " 集 / 未识别 "
        + (entry.unidentified || 0) + " 个" }),
      entry.will_reuse ? el("span", { class: "muted small-note", text: "已有剧场，补集" }) : null,
      entry.over_limit ? el("span", { class: "muted small-note", text: "文件超限，将跳过" }) : null));
  }
  if (rows.length > PREVIEW_ROWS) {
    box.append(el("div", { class: "muted small-note", text: "只显示前 " + PREVIEW_ROWS + " 个，共 " + rows.length + " 个" }));
  }
}
