// 后台「剧场」页签：新建 / 批量建 / 一键识别 / 上传 / 逐剧场管理都收在这里。
// 观看面（#/series）只负责看与播，不放任何管理入口（用户 2026-09-22 要求）。

import { api } from "./api.js";
import { el, clear, banner, setBanner, field, input, asArray } from "./dom.js";
import { confirmDialog } from "./confirm.js";
import { batchImportSeries, importDirIntoSeries } from "./series-import.js";
import { uploadNewSeries, uploadToSeries } from "./uploads.js";
import { mountSeriesAdmin } from "./series-admin.js";

function btn(label, onclick, extra) {
  return el("button", {
    class: "btn small" + (extra ? " " + extra : ""), type: "button", text: label, onclick,
  });
}

function gridOf(headers) {
  const headRow = el("tr");
  for (const label of headers) headRow.append(el("th", { text: label }));
  const body = el("tbody");
  const table = el("table", { class: "grid" }, el("thead", null, headRow), body);
  return { table: el("div", { class: "table-scroll" }, table), body };
}

function textCell(value) {
  return el("td", { text: value === null || value === undefined || value === "" ? "-" : String(value) });
}

export function mountSeriesTab(root) {
  const note = banner();
  const detectNote = el("div", { class: "banner", hidden: true, dataset: { role: "series-detect-note" } });
  const { table, body } = gridOf(["标题", "集数", "简介", "操作"]);

  const titleInput = input({ placeholder: "剧场标题", required: true });
  const descInput = input({ placeholder: "简介（可选）" });
  const submit = el("button", { class: "btn primary", type: "submit", text: "新建" });
  const form = el("form", { class: "panel", hidden: true, dataset: { role: "series-create" } },
    field("标题", titleInput), field("简介", descInput), el("div", { class: "actions" }, submit));
  const createToggle = btn("＋ 新建剧场", () => {
    form.hidden = !form.hidden;
    if (!form.hidden) titleInput.focus();
  }, "primary");

  root.append(note, detectNote, form,
    el("div", { class: "row" },
      createToggle,
      btn("新建剧场并上传", () => uploadNewSeries(load)),
      btn("按子目录批量建剧场", () => batchImportSeries(load)),
      btn("一键识别全部", (event) => runDetectAll(event.currentTarget))),
    table);

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
      await load();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "新建失败");
    } finally {
      submit.disabled = false;
    }
  });

  function seriesRow(item) {
    const actions = el("div", { class: "actions" },
      btn("管理", () => openDrawer(item), "primary"),
      btn("上传", () => uploadToSeries(item, load)),
      btn("从目录导入", () => importDirIntoSeries(item, load)));
    return el("tr", { dataset: { role: "series-row", id: String(item.id) } },
      textCell(item.title), textCell((item.episode_count || 0) + " 集"), textCell(item.description),
      el("td", null, actions));
  }

  function openDrawer(item) {
    const overlay = el("div", { class: "modal-overlay", dataset: { role: "series-admin-drawer" } });
    const panelBody = el("div", { class: "panel series-admin-body" });
    const box = el("div", { class: "modal wide" },
      el("div", { class: "picker-head" },
        el("span", { text: "管理剧场：" + (item.title || "") }),
        el("button", {
          class: "btn small", type: "button", text: "关闭", dataset: { role: "drawer-close" },
          onclick: () => overlay.remove(),
        })),
      panelBody);
    overlay.append(box);
    overlay.addEventListener("click", (event) => { if (event.target === overlay) overlay.remove(); });
    document.body.append(overlay);
    mountSeriesAdmin(panelBody, item, {
      onChanged: load,
      onDeleted: () => { overlay.remove(); load(); },
    });
  }

  async function load() {
    clear(body);
    body.append(el("tr", null, el("td", { class: "empty", colspan: "4", text: "加载中…" })));
    let list = [];
    try {
      const data = await api.request("GET", "/api/v1/series");
      list = asArray(data && data.list);
    } catch (err) {
      clear(body);
      setBanner(note, err && err.message ? err.message : "加载失败");
      return;
    }
    clear(body);
    if (!list.length) {
      body.append(el("tr", null, el("td", { class: "empty", colspan: "4", text: "还没有剧场" })));
      return;
    }
    for (const item of list) body.append(seriesRow(item));
  }

  // 一键识别：先预览变化（不落库）→ 确认 → 任务进度 → 结果（文案 ≤40 字）。
  let detectTimer = null;

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
    for (const row of asArray(preview && preview.per_series)) {
      if (row && row.error) return row.error;
    }
    return "请重试";
  }

  load();
  return () => { if (detectTimer) clearInterval(detectTimer); detectTimer = null; };
}
