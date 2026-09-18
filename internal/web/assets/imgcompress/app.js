// 图片压缩 Web UI 的前端（原生 ESM，无构建步骤、无外部依赖）。
//
// 路径约定：所有请求都用**相对路径**（`api/v1/...`），这样同一份代码在
//   直连端口  http://127.0.0.1:8890/           → /api/v1/...
//   面板别名  http://<主机>/imgcompress/        → /imgcompress/api/v1/...
// 两种入口下都对，不需要任何路径改写规则。

const API = 'api/v1/';

const $ = (id) => document.getElementById(id);

const state = {
  files: [],
  taskId: '',
  total: 0,
  completed: 0,
  lastSeq: 0,
  source: null,
  engine: null,
};

// ---------- 小工具 ----------

function formatBytes(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '—';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n;
  let i = -1;
  do {
    v /= 1024;
    i += 1;
  } while (v >= 1024 && i < units.length - 1);
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

function savedPercent(before, after) {
  if (!before || before <= 0) return '—';
  const pct = ((before - after) / before) * 100;
  if (pct <= 0) return `+${Math.abs(pct).toFixed(1)}%`;
  return `-${pct.toFixed(1)}%`;
}

function showBanner(text, kind) {
  const el = $('banner');
  el.textContent = text;
  el.className = `banner${kind ? ' ' + kind : ''}`;
}

function hideBanner() {
  $('banner').className = 'banner hidden';
}

function addLog(level, text) {
  const ul = $('logs');
  const li = document.createElement('li');
  li.className = level || '';
  li.textContent = text;
  ul.appendChild(li);
  ul.scrollTop = ul.scrollHeight;
}

// ---------- 引擎状态 ----------

async function loadEngine() {
  try {
    const r = await fetch(`${API}engine`, { headers: { Accept: 'application/json' } });
    const d = await r.json();
    state.engine = d;
    const pill = $('engine-pill');
    if (d.available) {
      pill.className = 'engine ok';
      $('engine-text').textContent = `引擎就绪：${d.version || d.bin || 'vips'}`;
      $('engine-text').title = d.bin || '';
      hideBanner();
      updateStart();
    } else {
      pill.className = 'engine bad';
      $('engine-text').textContent = '引擎不可用';
      showBanner(
        `图片压缩引擎不可用：${d.reason || '未知原因'}\n\n` +
          '到面板的「应用市场 → 图片压缩（libvips）」重新安装即可。',
        'warn',
      );
      updateStart();
    }
  } catch (e) {
    $('engine-pill').className = 'engine bad';
    $('engine-text').textContent = '引擎状态未知';
    showBanner(`无法读取引擎状态：${e.message}`, 'warn');
    updateStart();
  }
}

// ---------- 选择文件 ----------

function updateStart() {
  const ready = state.engine && state.engine.available;
  $('start').disabled = !ready || state.files.length === 0;
}

function totalPickedBytes() {
  return state.files.reduce((a, f) => a + f.size, 0);
}

function renderPicked() {
  const ul = $('picked');
  ul.textContent = '';
  state.files.forEach((f, i) => {
    const li = document.createElement('li');
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = f.name;
    name.title = f.name;
    const right = document.createElement('span');
    right.style.display = 'flex';
    right.style.gap = '10px';
    right.style.alignItems = 'center';
    const size = document.createElement('span');
    size.className = 'size';
    size.textContent = formatBytes(f.size);
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.className = 'rm';
    rm.textContent = '移除';
    rm.title = `从列表移除 ${f.name}`;
    rm.addEventListener('click', () => {
      state.files.splice(i, 1);
      renderPicked();
    });
    right.append(size, rm);
    li.append(name, right);
    ul.appendChild(li);
  });
  $('drop-sub').textContent = state.files.length
    ? `已选 ${state.files.length} 个文件，共 ${formatBytes(totalPickedBytes())}`
    : '支持 jpg / jpeg / png / webp / avif / heic / tif / gif / bmp，可多选';
  updateStart();
}

function addFiles(list) {
  const incoming = Array.from(list || []);
  if (!incoming.length) return;
  const limit = (state.engine && state.engine.max_files) || 100;
  const perFile = (state.engine && state.engine.max_file_bytes) || 0;
  const skipped = [];
  for (const f of incoming) {
    if (perFile && f.size > perFile) {
      skipped.push(`${f.name}（${formatBytes(f.size)}，超过单文件上限 ${formatBytes(perFile)}）`);
      continue;
    }
    if (state.files.length >= limit) {
      skipped.push(`${f.name}（超过一次最多 ${limit} 个）`);
      continue;
    }
    state.files.push(f);
  }
  renderPicked();
  if (skipped.length) {
    showBanner(`这些文件没有加入列表：\n${skipped.join('\n')}`, 'warn');
  } else {
    hideBanner();
  }
}

// ---------- 压缩 ----------

function optionsForm() {
  const fd = new FormData();
  fd.append('quality', $('quality').value);
  fd.append('max_edge', $('max-edge').value);
  fd.append('format', $('format').value);
  fd.append('strip_metadata', $('strip').checked ? '1' : '0');
  return fd;
}

function resetProgress() {
  $('logs').textContent = '';
  $('bar-fill').style.width = '0%';
  $('progress-count').textContent = '';
  state.completed = 0;
  state.lastSeq = 0;
}

function noteCompleted(text) {
  if (/^[✓↷✗]/.test(text)) {
    state.completed += 1;
    const total = state.total || state.completed;
    const pct = Math.min(100, Math.round((state.completed / total) * 100));
    $('bar-fill').style.width = `${pct}%`;
    $('progress-count').textContent = `已完成 ${state.completed}/${total}`;
  }
}

async function startCompress() {
  if (!state.files.length) return;
  hideBanner();
  $('result-card').classList.add('hidden');
  $('progress-card').classList.remove('hidden');
  resetProgress();
  $('start').disabled = true;
  $('cancel').classList.remove('hidden');
  state.total = state.files.length;

  const fd = optionsForm();
  for (const f of state.files) fd.append('files', f, f.name);

  try {
    const r = await fetch(`${API}compress`, { method: 'POST', body: fd });
    const d = await r.json().catch(() => ({ ok: false, msg: `服务返回了非 JSON 内容（HTTP ${r.status}）` }));
    if (!r.ok || !d.ok) {
      showBanner(`压缩失败：${d.msg || r.status}`, '');
      $('cancel').classList.add('hidden');
      $('start').disabled = false;
      return;
    }
    state.taskId = d.data.task_id;
    if (d.data.sync) {
      addLog('step', '单文件同步压缩完成');
      $('bar-fill').style.width = '100%';
      finishWith(d.data.result);
      return;
    }
    addLog('step', `任务已创建 ${state.taskId}（${d.data.title || ''}），正在压缩…`);
    subscribe(state.taskId);
  } catch (e) {
    showBanner(`上传失败：${e.message}`, '');
    $('cancel').classList.add('hidden');
    $('start').disabled = false;
  }
}

function subscribe(taskId) {
  if (state.source) state.source.close();
  const es = new EventSource(`${API}tasks/${taskId}/stream`);
  state.source = es;

  es.addEventListener('lines', (ev) => {
    let payload;
    try {
      payload = JSON.parse(ev.data);
    } catch {
      return;
    }
    for (const line of payload.lines || []) {
      if (line.seq <= state.lastSeq) continue;
      state.lastSeq = line.seq;
      addLog(line.level, line.text);
      noteCompleted(line.text || '');
    }
  });

  es.addEventListener('done', (ev) => {
    es.close();
    state.source = null;
    $('bar-fill').style.width = '100%';
    $('cancel').classList.add('hidden');
    let meta = null;
    try {
      meta = JSON.parse(ev.data).task;
    } catch {
      meta = null;
    }
    if (meta && meta.result) {
      finishWith(meta.result, meta);
    } else {
      fetchResult(taskId);
    }
  });

  es.onerror = () => {
    // EventSource 会自动带 Last-Event-ID 重连；这里只在页面上如实说一句。
    addLog('warn', '进度流断开，正在重连…（长任务不受影响，仍可在服务端继续）');
  };
}

async function fetchResult(taskId) {
  try {
    const r = await fetch(`${API}tasks/${taskId}`);
    const d = await r.json();
    if (d.ok && d.data && d.data.task) {
      finishWith(d.data.task.result, d.data.task);
      return;
    }
    showBanner(`读取任务结果失败：${d.msg || r.status}`, '');
  } catch (e) {
    showBanner(`读取任务结果失败：${e.message}`, '');
  } finally {
    $('cancel').classList.add('hidden');
  }
}

function finishWith(result, meta) {
  if (state.source) {
    state.source.close();
    state.source = null;
  }
  $('cancel').classList.add('hidden');
  $('start').disabled = false;
  if (!result) {
    showBanner('任务结束了，但服务端没有返回结果（可能被中断）', 'warn');
    return;
  }
  renderResult(result, meta);
  if (meta && meta.status === 'failed') {
    showBanner(`任务失败：${meta.error || '原因见结果表与日志'}`, '');
  } else if (result.failed > 0) {
    showBanner(`有 ${result.failed} 个文件失败（其余已处理），失败原因见结果表与日志`, 'warn');
  }
}

function renderResult(result, meta) {
  const card = $('result-card');
  card.classList.remove('hidden');
  const savedPct = result.before_bytes > 0
    ? (((result.before_bytes - result.after_bytes) / result.before_bytes) * 100).toFixed(1)
    : '0.0';
  $('result-summary').textContent =
    `成功 ${result.done} · 跳过 ${result.skipped}（压完更大）· 失败 ${result.failed} · ` +
    `${formatBytes(result.before_bytes)} → ${formatBytes(result.after_bytes)}（省 ${savedPct}%）`;

  const body = $('result-body');
  body.textContent = '';
  for (const it of result.items || []) {
    const tr = document.createElement('tr');

    const tdName = document.createElement('td');
    tdName.className = 'name';
    tdName.textContent = it.name;

    const tdBefore = document.createElement('td');
    tdBefore.className = 'num';
    tdBefore.textContent = formatBytes(it.before);

    const tdAfter = document.createElement('td');
    tdAfter.className = 'num';
    tdAfter.textContent = it.after ? formatBytes(it.after) : '—';

    const tdSaved = document.createElement('td');
    tdSaved.className = 'num';
    if (it.error) {
      tdSaved.textContent = '—';
    } else if (it.skipped) {
      tdSaved.textContent = '保留原图';
    } else {
      tdSaved.textContent = savedPercent(it.before, it.after);
    }

    const tdStatus = document.createElement('td');
    if (it.error) {
      const s = document.createElement('span');
      s.className = 'bad';
      s.textContent = `失败：${it.error}`;
      tdStatus.appendChild(s);
    } else if (it.skipped) {
      const s = document.createElement('span');
      s.className = 'skip';
      s.textContent = '已跳过（压完反而更大，保留原文件）';
      tdStatus.appendChild(s);
    } else {
      const s = document.createElement('span');
      s.className = 'good';
      s.textContent = '成功';
      tdStatus.appendChild(s);
    }

    const tdDl = document.createElement('td');
    if (!it.error && !it.skipped && state.taskId) {
      const a = document.createElement('a');
      a.className = 'dl';
      a.href = `${API}tasks/${state.taskId}/files/${it.index}`;
      a.textContent = '下载';
      a.setAttribute('download', it.dst || it.name);
      a.title = `下载 ${it.dst || it.name}`;
      tdDl.appendChild(a);
    } else {
      tdDl.textContent = '—';
    }

    tr.append(tdName, tdBefore, tdAfter, tdSaved, tdStatus, tdDl);
    body.appendChild(tr);
  }

  const hasOutput = (result.items || []).some((it) => !it.error && !it.skipped);
  $('zip').disabled = !hasOutput || !state.taskId;
  if (meta && meta.status === 'canceled') {
    showBanner('任务已被中断（已完成的部分结果仍然可以下载）', 'warn');
  }
}

async function cancelTask() {
  if (!state.taskId) return;
  try {
    await fetch(`${API}tasks/${state.taskId}/cancel`, { method: 'POST' });
    addLog('warn', '已请求中断任务…');
  } catch (e) {
    showBanner(`中断失败：${e.message}`, 'warn');
  }
}

// ---------- 事件接线 ----------

function init() {
  const drop = $('drop');
  const input = $('file-input');

  drop.addEventListener('click', () => input.click());
  drop.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      input.click();
    }
  });
  input.addEventListener('change', () => {
    addFiles(input.files);
    input.value = '';
  });
  ['dragenter', 'dragover'].forEach((ev) =>
    drop.addEventListener(ev, (e) => {
      e.preventDefault();
      drop.classList.add('over');
    }),
  );
  ['dragleave', 'drop'].forEach((ev) =>
    drop.addEventListener(ev, (e) => {
      e.preventDefault();
      drop.classList.remove('over');
    }),
  );
  drop.addEventListener('drop', (e) => {
    if (e.dataTransfer && e.dataTransfer.files) addFiles(e.dataTransfer.files);
  });
  // 拖到页面其它地方也不让浏览器直接打开图片（否则用户会以为"没反应"）
  ['dragover', 'drop'].forEach((ev) =>
    window.addEventListener(ev, (e) => e.preventDefault()),
  );

  $('quality').addEventListener('input', () => {
    $('quality-val').textContent = $('quality').value;
  });
  $('start').addEventListener('click', startCompress);
  $('clear').addEventListener('click', () => {
    state.files = [];
    renderPicked();
    hideBanner();
    $('progress-card').classList.add('hidden');
    $('result-card').classList.add('hidden');
  });
  $('cancel').addEventListener('click', cancelTask);
  $('zip').addEventListener('click', () => {
    if (state.taskId) window.location.href = `${API}tasks/${state.taskId}/zip`;
  });

  renderPicked();
  loadEngine();
}

init();
