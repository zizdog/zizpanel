// 管理后台：媒体库 / 媒体 / 用户 / 系统 四个页签

import { api } from "./api.js";
import { el, clear, banner, setBanner, field, input, fmtDuration, fmtBytes, fmtDate } from "./dom.js";

function button(label, onclick, extraClass) {
  return el("button", {
    class: "btn small" + (extraClass ? " " + extraClass : ""),
    type: "button", text: label, onclick,
  });
}

function cell(value) {
  const td = el("td");
  if (value instanceof Node) td.append(value);
  else td.textContent = value === null || value === undefined || value === "" ? "-" : String(value);
  return td;
}

function rowOf(values) {
  const row = el("tr");
  for (const value of values) row.append(cell(value));
  return row;
}

function emptyRow(cols, text) {
  return el("tr", null, el("td", { class: "empty", colspan: String(cols), text }));
}

function gridOf(headers) {
  const table = el("table", { class: "grid" });
  const headRow = el("tr");
  for (const label of headers) headRow.append(el("th", { text: label }));
  const body = el("tbody");
  table.append(el("thead", null, headRow), body);
  return { table: el("div", { class: "table-scroll" }, table), body };
}

function selectFrom(options) {
  const select = el("select", { class: "input" });
  for (const option of options) select.append(el("option", { value: option.value, text: option.label }));
  return select;
}

function kv(label, value) {
  return el("div", { class: "kv" },
    el("span", { class: "k", text: label }),
    el("span", { class: "v", text: value === null || value === undefined || value === "" ? "-" : String(value) }));
}

/* ---------- 媒体库 ---------- */

function mountLibraries(root) {
  const note = banner();
  const timers = [];
  const { table, body } = gridOf(["名称", "根目录", "递归", "启用", "创建时间", "操作"]);
  const nameInput = input({ placeholder: "名称", required: true });
  const pathInput = input({ placeholder: "根目录，例如 /Users/me/Videos", required: true });
  const recursive = el("input", { type: "checkbox", checked: true });
  const enabled = el("input", { type: "checkbox", checked: true });
  const ignoreInput = input({ placeholder: "忽略规则，逗号分隔" });
  const submit = el("button", { class: "btn primary", type: "submit", text: "新建" });
  const cancel = el("button", { class: "btn", type: "button", text: "取消", hidden: true });
  let editing = null;

  const form = el("form", { class: "panel" },
    el("div", { class: "row" }, field("名称", nameInput), field("根目录", pathInput)),
    el("div", { class: "row" },
      el("label", { class: "check" }, recursive, el("span", { text: "递归扫描" })),
      el("label", { class: "check" }, enabled, el("span", { text: "启用" }))),
    field("忽略规则", ignoreInput),
    el("div", { class: "actions" }, submit, cancel),
    note);

  function setEditing(library) {
    editing = library;
    if (library) {
      nameInput.value = library.name || "";
      pathInput.value = library.root_path || "";
      recursive.checked = !!library.recursive;
      enabled.checked = !!library.enabled;
      ignoreInput.value = Array.isArray(library.ignore_rules) ? library.ignore_rules.join(",") : "";
      submit.textContent = "保存";
      cancel.hidden = false;
    } else {
      nameInput.value = "";
      pathInput.value = "";
      recursive.checked = true;
      enabled.checked = true;
      ignoreInput.value = "";
      submit.textContent = "新建";
      cancel.hidden = true;
    }
  }

  async function refresh() {
    setBanner(note, "");
    try {
      const result = await api.libraries();
      const list = result && Array.isArray(result.list) ? result.list : [];
      clear(body);
      if (!list.length) body.append(emptyRow(6, "暂无媒体库"));
      for (const library of list) body.append(libraryRow(library));
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "加载失败");
    }
  }

  function pollScan(taskId, line) {
    const timer = setInterval(async () => {
      try {
        const task = await api.scanTask(taskId);
        const scanned = Number(task.scanned) || 0;
        const total = Number(task.total) || 0;
        const failed = Number(task.failed) || 0;
        line.textContent = "已扫描 " + scanned + "/" + total + "，失败 " + failed;
        if (task.status === "success" || task.status === "failed" || task.status === "interrupted") {
          clearInterval(timer);
          const label = task.status === "success" ? "扫描完成" : task.status === "failed" ? "扫描失败" : "扫描已中断";
          line.textContent = label + "，已扫描 " + scanned + "/" + total + "，失败 " + failed +
            (task.error ? "：" + task.error : "");
        }
      } catch (err) {
        clearInterval(timer);
        line.textContent = err && err.message ? err.message : "查询失败";
      }
    }, 1000);
    timers.push(timer);
  }

  async function startScan(library, line) {
    line.textContent = "提交扫描…";
    try {
      const result = await api.scanLibrary(library.id);
      const taskId = result && (result.task_id || result.id);
      if (!taskId) { line.textContent = "未返回任务号"; return; }
      pollScan(taskId, line);
    } catch (err) {
      line.textContent = err && err.message ? err.message : "扫描失败";
    }
  }

  function libraryRow(library) {
    const line = el("div", { class: "scan-line" });
    const actions = el("div", { class: "actions" },
      button("扫描", () => startScan(library, line)),
      button("编辑", () => { setEditing(library); nameInput.focus(); }),
      button("删除", async () => {
        if (!window.confirm("删除媒体库「" + (library.name || library.id) + "」？")) return;
        try { await api.deleteLibrary(library.id); await refresh(); }
        catch (err) { setBanner(note, err && err.message ? err.message : "删除失败"); }
      }, "danger"));
    const holder = el("td", null, actions, line);
    return rowOf([library.name, library.root_path, library.recursive ? "是" : "否",
      library.enabled ? "是" : "否", fmtDate(library.created_at), holder]);
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    submit.disabled = true;
    const payload = {
      name: nameInput.value.trim(),
      root_path: pathInput.value.trim(),
      recursive: recursive.checked,
      enabled: enabled.checked,
      ignore_rules: ignoreInput.value.split(",").map((part) => part.trim()).filter(Boolean),
    };
    try {
      if (editing) await api.updateLibrary(editing.id, payload);
      else await api.createLibrary(payload);
      setEditing(null);
      await refresh();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "保存失败");
    } finally {
      submit.disabled = false;
    }
  });

  cancel.addEventListener("click", () => setEditing(null));
  root.append(form, table);
  refresh();

  return () => {
    for (const timer of timers) clearInterval(timer);
  };
}

/* ---------- 媒体 ---------- */

function mountMedia(root) {
  const note = banner();
  const librarySelect = selectFrom([{ value: "", label: "全部媒体库" }]);
  const query = input({ placeholder: "标题关键词" });
  const statusSelect = selectFrom([
    { value: "", label: "全部状态" },
    { value: "ready", label: "ready" },
    { value: "probe_failed", label: "probe_failed" },
    { value: "missing", label: "missing" },
  ]);
  const info = el("div", { class: "muted" });
  const { table, body } = gridOf(["标题", "媒体库", "时长", "分辨率", "视频编码", "状态", "大小"]);
  const prev = button("上一页", () => { page = Math.max(1, page - 1); refresh(); });
  const next = button("下一页", () => { page += 1; refresh(); });
  const names = new Map();
  let page = 1;

  async function loadLibraries() {
    try {
      const result = await api.libraries();
      const list = result && Array.isArray(result.list) ? result.list : [];
      for (const library of list) {
        names.set(String(library.id), library.name || String(library.id));
        librarySelect.append(el("option", { value: String(library.id), text: library.name || String(library.id) }));
      }
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "媒体库加载失败");
    }
  }

  async function refresh() {
    setBanner(note, "");
    try {
      const result = await api.mediaList({
        page,
        per_page: 20,
        library_id: librarySelect.value,
        q: query.value.trim(),
        status: statusSelect.value,
      });
      const list = result && result.data && Array.isArray(result.data.list) ? result.data.list : [];
      const meta = (result && result.meta) || {};
      clear(body);
      if (!list.length) body.append(emptyRow(7, "没有数据"));
      for (const item of list) {
        const resolution = item.width && item.height ? item.width + "×" + item.height : "-";
        body.append(rowOf([item.title, names.get(String(item.library_id)) || item.library_id,
          fmtDuration(item.duration_ms), resolution, item.codecs ? item.codecs.video : "-",
          item.status, fmtBytes(item.size)]));
      }
      const current = Number(meta.page) || page;
      info.textContent = "第 " + current + " 页 / 共 " + (Number(meta.total) || 0) + " 条";
      prev.disabled = current <= 1;
      next.disabled = !meta.has_more;
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "加载失败");
    }
  }

  const filters = el("div", { class: "row" }, librarySelect, query, statusSelect);
  function onFilter() { page = 1; refresh(); }
  librarySelect.addEventListener("change", onFilter);
  statusSelect.addEventListener("change", onFilter);
  query.addEventListener("change", onFilter);
  root.append(note, el("div", { class: "panel" }, filters), el("div", { class: "actions" }, prev, next, info), table);
  loadLibraries().then(refresh);
}

/* ---------- 用户 ---------- */

function mountUsers(root) {
  const note = banner();
  const { table, body } = gridOf(["用户名", "显示名", "角色", "状态", "最后登录", "操作"]);
  const username = input({ placeholder: "用户名", required: true });
  const password = input({ type: "password", placeholder: "口令", required: true });
  const display = input({ placeholder: "显示名" });
  const role = selectFrom([{ value: "user", label: "user" }, { value: "admin", label: "admin" }]);
  const submit = el("button", { class: "btn primary", type: "submit", text: "新建用户" });

  const form = el("form", { class: "panel" },
    el("div", { class: "row" }, field("用户名", username), field("口令", password),
      field("显示名", display), field("角色", role)),
    el("div", { class: "actions" }, submit),
    note);

  async function refresh() {
    setBanner(note, "");
    try {
      const result = await api.users();
      const list = result && Array.isArray(result.list) ? result.list : [];
      clear(body);
      if (!list.length) body.append(emptyRow(6, "暂无用户"));
      for (const user of list) body.append(userRow(user));
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "加载失败");
    }
  }

  function userRow(user) {
    async function patch(payload) {
      setBanner(note, "");
      try { await api.updateUser(user.id, payload); await refresh(); }
      catch (err) { setBanner(note, err && err.message ? err.message : "操作失败"); }
    }
    const newPassword = el("input", { class: "input tiny", type: "password", placeholder: "新口令" });
    const confirm = button("确定", async () => {
      if (!newPassword.value) return;
      await patch({ password: newPassword.value });
      newPassword.value = "";
      resetBox.classList.add("hidden");
    });
    const resetBox = el("div", { class: "actions hidden" }, newPassword, confirm);
    const actions = el("div", { class: "actions" },
      button("改角色", () => patch({ role: user.role === "admin" ? "user" : "admin" })),
      button(user.status === "active" ? "禁用" : "启用", () => patch({ status: user.status === "active" ? "disabled" : "active" })),
      button("重置口令", () => resetBox.classList.remove("hidden")));
    const holder = el("td", null, actions, resetBox);
    return rowOf([user.username, user.display_name, user.role, user.status, fmtDate(user.last_login_at), holder]);
  }

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    setBanner(note, "");
    submit.disabled = true;
    try {
      await api.createUser({
        username: username.value.trim(),
        password: password.value,
        display_name: display.value.trim() || username.value.trim(),
        role: role.value,
      });
      username.value = "";
      password.value = "";
      display.value = "";
      await refresh();
    } catch (err) {
      setBanner(note, err && err.message ? err.message : "创建失败");
    } finally {
      submit.disabled = false;
    }
  });

  root.append(form, table);
  refresh();
}

/* ---------- 系统 ---------- */

function mountSystem(root) {
  const note = banner();
  const box = el("div", { class: "panel" });
  root.append(note, box);

  api.systemInfo().then((info) => {
    const ffmpeg = info.ffmpeg || {};
    const ffprobe = info.ffprobe || {};
    const disk = info.disk || {};
    const counts = info.counts || {};
    clear(box);
    box.append(
      kv("版本", info.version),
      kv("Go 版本", info.go_version),
      kv("监听", info.listen),
      kv("数据目录", info.data_dir || disk.data_dir),
      kv("ffmpeg", ffmpeg.version),
      kv("ffmpeg 路径", ffmpeg.path),
      kv("VideoToolbox H.264", ffmpeg.videotoolbox_h264 ? "支持" : "不支持"),
      kv("ffprobe", ffprobe.version),
      kv("ffprobe 路径", ffprobe.path),
      kv("磁盘可用", fmtBytes(disk.free_bytes)),
      kv("磁盘总量", fmtBytes(disk.total_bytes)),
      kv("媒体库 / 媒体 / 失败", (counts.libraries || 0) + " / " + (counts.media || 0) + " / " + (counts.failed || 0)));
  }).catch((err) => {
    setBanner(note, err && err.message ? err.message : "加载失败");
  });
}

/* ---------- 容器 ---------- */

export function mountAdmin(view) {
  const note = banner();
  const tabs = el("nav", { class: "tabs" });
  const panel = el("div", { class: "admin-body" });
  const definitions = [
    { key: "libraries", label: "媒体库", mount: mountLibraries },
    { key: "media", label: "媒体", mount: mountMedia },
    { key: "users", label: "用户", mount: mountUsers },
    { key: "system", label: "系统", mount: mountSystem },
  ];
  const tabButtons = new Map();
  let cleanup = null;
  let activeKey = "";

  function select(key) {
    if (key === activeKey) return;
    activeKey = key;
    if (cleanup) { try { cleanup(); } catch (err) { /* ignore */ } cleanup = null; }
    clear(panel);
    for (const [tabKey, node] of tabButtons) node.classList.toggle("on", tabKey === key);
    const definition = definitions.filter((item) => item.key === key)[0];
    if (definition) cleanup = definition.mount(panel) || null;
  }

  for (const definition of definitions) {
    const tabButton = el("button", { class: "tab", type: "button", text: definition.label, onclick: () => select(definition.key) });
    tabButtons.set(definition.key, tabButton);
    tabs.append(tabButton);
  }

  view.append(el("div", { class: "admin" }, note, tabs, panel));
  select("libraries");

  return () => { if (cleanup) cleanup(); };
}
