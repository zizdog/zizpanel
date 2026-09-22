// 上传：三步（start → PUT → finish）。PUT 走 XHR 只为拿 upload.onprogress，
// 一次一个文件、流式落到目标目录；不把整批文件塞进一次请求。
import { api, csrfToken } from "./api.js";
import { el, clear, banner, setBanner, fmtBytes } from "./dom.js";

const PART_HINT = "文件夹上传会把子目录里的视频一并带进来（落点仍在目标目录下）";

function xhrError(xhr) {
  try {
    const payload = JSON.parse(xhr.responseText || "");
    if (payload && payload.error && payload.error.message) return String(payload.error.message);
  } catch (err) { /* 非 JSON 就退回状态码 */ }
  return "上传失败（HTTP " + xhr.status + "）";
}

// putFile 流式上传一个文件，返回 upload.onprogress 的字节数回调。
function putFile(url, file, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url, true);
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");
    const token = csrfToken();
    if (token) xhr.setRequestHeader("X-CSRF-Token", token);
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(event.loaded, event.total);
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error(xhrError(xhr)));
    };
    xhr.onerror = () => reject(new Error("网络中断，上传失败"));
    xhr.onabort = () => reject(new Error("已取消"));
    xhr.send(file);
  });
}

export async function uploadFiles(target, files, opts) {
  const options = opts || {};
  const list = Array.from(files || []);
  if (!list.length) throw new Error("请先选择文件");
  const start = await api.uploadStart(Object.assign({}, target, {
    overwrite: !!options.overwrite,
    files: list.map((file) => ({ name: file.name, size: file.size })),
  }));
  const totalBytes = list.reduce((sum, file) => sum + (file.size || 0), 0) || 1;
  let doneBytes = 0;
  for (let i = 0; i < list.length; i += 1) {
    const entry = start.files[i];
    if (!entry) continue;
    if (options.onProgress) options.onProgress(doneBytes / totalBytes, "上传 " + (i + 1) + "/" + list.length + "：" + file.name);
    await putFile(entry.url, list[i], (loaded) => {
      if (options.onProgress) {
        options.onProgress(Math.min(1, (doneBytes + loaded) / totalBytes),
          "上传 " + (i + 1) + "/" + list.length + "：" + list[i].name);
      }
    });
    doneBytes += (list[i].size || 0);
  }
  const result = await api.uploadFinish(start.upload_id);
  return Object.assign({ seriesId: start.series_id, fileCount: list.length }, result);
}

// openUploadPanel 是三个入口共用的弹层：库 / 剧场 / 新建剧场 + 上传。
export function openUploadPanel(config) {
  const cfg = config || {};
  const note = banner();
  const overlay = el("div", { class: "modal-overlay", dataset: { role: "upload-panel" } });
  const body = el("div", { class: "panel" });
  const box = el("div", { class: "modal wide" },
    el("div", { class: "picker-head" },
      el("span", { text: cfg.title || "上传视频" }),
      el("button", { class: "btn small", type: "button", text: "关闭", onclick: () => overlay.remove() })),
    note, body);
  overlay.append(box);
  overlay.addEventListener("click", (event) => { if (event.target === overlay) overlay.remove(); });
  document.body.append(overlay);

  const fileInput = el("input", { type: "file", multiple: true, dataset: { role: "upload-files" } });
  const folderInput = el("input", {
    type: "file", multiple: true, webkitdirectory: true, dataset: { role: "upload-folder" },
  });
  const overwrite = el("input", { type: "checkbox", dataset: { role: "upload-overwrite" } });
  const barFill = el("span", { class: "bar-fill" });
  const bar = el("div", { class: "bar", hidden: true }, barFill);
  const action = el("button", { class: "btn primary", type: "button", text: "开始上传" });
  const result = el("div", { class: "pick-list", dataset: { role: "upload-result" } });
  let picked = [];

  // 目标选择：库下拉（新建剧场时另加名字输入）。
  const librarySelect = el("select", { class: "input", dataset: { role: "upload-library" } });
  const nameInput = el("input", { class: "input", placeholder: "新剧场名字", dataset: { role: "upload-new-series" } });

  async function loadLibraries() {
    try {
      const data = await api.libraries();
      const list = Array.isArray(data && data.list) ? data.list : [];
      clear(librarySelect);
      for (const lib of list) {
        librarySelect.append(el("option", { value: String(lib.id), text: lib.name || lib.id }));
      }
      if (cfg.libraryId) librarySelect.value = cfg.libraryId;
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "媒体库加载失败");
    }
  }

  if (cfg.mode === "series") {
    body.append(el("div", { class: "muted small-note", text: "目标剧场：" + (cfg.seriesTitle || cfg.seriesId) }));
    body.append(el("div", { class: "muted small-note", text: "落点：<媒体库>/<剧场名>/" }));
    // 老剧场没记住库时要让用户选，否则后端只能回"请选择媒体库"而无处可选。
    if (!cfg.libraryId) {
      body.append(el("label", { class: "field" },
        el("span", { class: "field-label", text: "媒体库" }), librarySelect));
    }
  } else {
    if (cfg.mode === "new") {
      body.append(el("label", { class: "field" },
        el("span", { class: "field-label", text: "剧场名字" }), nameInput));
    }
    body.append(el("label", { class: "field" },
      el("span", { class: "field-label", text: "媒体库" }), librarySelect));
  }
  body.append(
    el("div", { class: "muted small-note", text: PART_HINT }),
    el("div", { class: "row" },
      el("label", { class: "field" }, el("span", { class: "field-label", text: "选文件" }), fileInput),
      el("label", { class: "field" }, el("span", { class: "field-label", text: "选文件夹" }), folderInput)),
    el("label", { class: "row" }, overwrite,
      el("span", { class: "muted small-note", text: "同名覆盖（默认改名保留两者）" })),
    bar,
    el("div", { class: "actions" }, action),
    result);

  function pick(list, source) {
    const other = source === fileInput ? folderInput : fileInput;
    other.value = "";
    picked = Array.from(list || []);
    setBanner(note, picked.length ? ("已选 " + picked.length + " 个文件") : "");
    clear(result);
    if (picked.length) {
      for (const file of picked.slice(0, 20)) {
        result.append(el("div", { class: "detect-row" },
          el("span", { class: "ep-title", text: file.name }),
          el("span", { class: "muted small-note", text: fmtBytes(file.size) })));
      }
      if (picked.length > 20) {
        result.append(el("div", { class: "muted small-note", text: "只显示前 20 个，共 " + picked.length + " 个" }));
      }
    }
  }
  fileInput.addEventListener("change", () => pick(fileInput.files, fileInput));
  folderInput.addEventListener("change", () => pick(folderInput.files, folderInput));

  function target() {
    if (cfg.mode === "series") return { series_id: cfg.seriesId, library_id: librarySelect.value || cfg.libraryId || "" };
    if (cfg.mode === "new") {
      const name = nameInput.value.trim();
      if (!name) throw new Error("请先填剧场名字");
      return { new_series_name: name, library_id: librarySelect.value };
    }
    if (!librarySelect.value) throw new Error("请先选媒体库");
    return { library_id: librarySelect.value };
  }

  action.addEventListener("click", async () => {
    let where;
    try {
      where = target();
    } catch (err) {
      setBanner(note, err.message);
      return;
    }
    if (!picked.length) {
      setBanner(note, "请先选择文件或文件夹");
      return;
    }
    action.disabled = true;
    bar.hidden = false;
    setBanner(note, "开始上传…");
    try {
      const out = await uploadFiles(where, picked, {
        overwrite: overwrite.checked,
        onProgress: (ratio, text) => {
          barFill.style.width = Math.round(ratio * 100) + "%";
          setBanner(note, text);
        },
      });
      renderUploadResult(out, result, cfg);
      setBanner(note, "上传完成：识别 " + out.recognized + " 集 / 未识别 " + out.unidentified + " 个"
        + (out.added ? "，新增 " + out.added + " 集" : ""));
      if (cfg.onDone) cfg.onDone(out);
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "上传失败");
    } finally {
      action.disabled = false;
      bar.hidden = true;
      barFill.style.width = "0%";
    }
  });

  loadLibraries();
  if (cfg.mode === "new" && cfg.seriesTitle) nameInput.value = cfg.seriesTitle;
  return { overlay };
}

function renderUploadResult(out, box, cfg) {
  clear(box);
  box.append(el("div", { class: "detect-row" },
    el("span", { class: "ep-no", text: "完成" }),
    el("span", { class: "ep-title", text: "登记 " + (out.registered || 0) + " 条，识别 "
      + (out.recognized || 0) + " 集，未识别 " + (out.unidentified || 0) + " 个" })));
  const seriesId = out.seriesId || (cfg.mode === "series" ? cfg.seriesId : "");
  if (seriesId) {
    box.append(el("div", { class: "actions" },
      el("a", { class: "btn small primary", href: "#/series/" + encodeURIComponent(seriesId), text: "去手动排序" })));
  } else {
    box.append(el("div", { class: "actions" },
      el("a", { class: "btn small", href: "#/admin/media", text: "去看媒体列表" })));
  }
}

// 入口 1：库页上传（落到库根，不建集）。
export function uploadToLibrary(onDone, libraryId) {
  openUploadPanel({ mode: "library", libraryId, title: "上传视频到媒体库", onDone });
}

// 入口 2：剧场页 / 管理抽屉「上传到本剧场」。
export function uploadToSeries(series, onDone) {
  openUploadPanel({
    mode: "series", seriesId: series.id, seriesTitle: series.title,
    libraryId: series.library_id || "", title: "上传到本剧场", onDone,
  });
}

// 入口 3：剧场列表「新建剧场并上传」。
export function uploadNewSeries(onDone) {
  openUploadPanel({ mode: "new", title: "新建剧场并上传", onDone });
}
