// 语音转文字界面（原生 ESM，无构建步骤、无外部依赖、离线可用）。
//
// 所有请求都用**相对路径**：页面既可以由直连端口（http://127.0.0.1:8892/）
// 打开，也可以由面板别名（http://<主机>/stt/）打开。绝对路径（/v1/models）
// 在别名下会打到站点根，整个页面就废了 —— 所以这里一律不带前导斜杠。

const $ = (id) => document.getElementById(id);

const state = {
  file: null,
  fileName: '',
  result: null,      // { text, segments, language, model, durationMs }
  models: [],        // /v1/models 的 data
  currentModel: '',
  syncMaxSeconds: 60,
  chunkSeconds: 60,
  recorder: null,
  recChunks: [],
  recTimer: null,
  recStart: 0,
  es: null,
  busy: false,
};

// ---------------------------------------------------------------------------
//  小工具
// ---------------------------------------------------------------------------

function humanSize(n) {
  if (!n || n <= 0) return '0 B';
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let f = n;
  if (f < 1024) return `${f} B`;
  let i = -1;
  while (f >= 1024 && i < units.length - 1) { f /= 1024; i += 1; }
  return `${f.toFixed(1)} ${units[i]}`;
}

function fmtClock(sec) {
  const s = Math.max(0, Math.round(sec));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = s % 60;
  const p = (n) => String(n).padStart(2, '0');
  return h > 0 ? `${p(h)}:${p(m)}:${p(ss)}` : `${p(m)}:${p(ss)}`;
}

function srtTime(sec) {
  const total = Math.max(0, Math.round((sec || 0) * 1000));
  const h = Math.floor(total / 3600000);
  const m = Math.floor((total % 3600000) / 60000);
  const s = Math.floor((total % 60000) / 1000);
  const ms = total % 1000;
  const p = (n, w = 2) => String(n).padStart(w, '0');
  return `${p(h)}:${p(m)}:${p(s)},${p(ms, 3)}`;
}

function vttTime(sec) {
  return srtTime(sec).replace(',', '.');
}

function show(id, on) {
  const el = $(id);
  if (el) el.hidden = !on;
}

function setError(msg) {
  const box = $('error-box');
  if (!msg) { box.hidden = true; box.textContent = ''; return; }
  box.hidden = false;
  box.textContent = msg;
}

// api 发一个相对路径请求并解析 JSON（错误也如实带出来）。
async function api(path, opts) {
  const res = await fetch(path, opts);
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch (_) { body = null; }
  if (!res.ok) {
    const msg = (body && (body.msg || (body.error && body.error.message))) || text || `HTTP ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    throw err;
  }
  return body;
}

// ---------------------------------------------------------------------------
//  健康 / 档位
// ---------------------------------------------------------------------------

function setEngineBadge(kind, text) {
  $('engine-dot').className = 'dot ' + kind;
  $('engine-text').textContent = text;
}

async function loadHealth() {
  try {
    const res = await fetch('healthz', { cache: 'no-store' });
    const h = await res.json();
    if (h.ok) {
      const missing = h.missing > 0 ? `，还有 ${h.missing} 档没下` : '';
      setEngineBadge('ok', `就绪 · 当前 ${h.current}${missing}`);
    } else {
      setEngineBadge('bad', '不可用');
      // 如实把原因摆出来：这就是"为什么不能转写"。
      setError(h.reason || '引擎不可用（/healthz 返回 ok:false）');
    }
    if (h.models && h.models.length) renderModelList(h.models, h.current);
    if (h.sync_max_seconds) state.syncMaxSeconds = h.sync_max_seconds;
    return h;
  } catch (e) {
    setEngineBadge('bad', '连不上服务');
    setError(`连不上本机服务：${e.message}`);
    return null;
  }
}

async function loadModels() {
  try {
    const body = await api('v1/models');
    state.models = body.data || [];
    state.currentModel = body.current || '';
    state.syncMaxSeconds = body.sync_max_seconds || state.syncMaxSeconds;
    state.chunkSeconds = body.chunk_seconds || state.chunkSeconds;
    renderLanguageChoices(body.languages || []);
    renderModelSelect();
    renderModelList(state.models, state.currentModel);
    $('sync-max').textContent = String(state.syncMaxSeconds);
    $('chunk-sec').textContent = String(state.chunkSeconds);
    $('limit-note').textContent =
      `单次上限 ${(body.max_audio_seconds / 3600).toFixed(1)} 小时；超过 ${state.syncMaxSeconds} 秒的音频自动转入任务并给出真实进度。`;
    const dir = body.models_dir || '';
    $('foot-models').textContent = dir ? `模型目录：${dir}` : '';
  } catch (e) {
    setError(`拉取档位清单失败：${e.message}`);
  }
}

function renderLanguageChoices(list) {
  const sel = $('language');
  const want = sel.value;
  sel.innerHTML = '';
  const items = list.length ? list : [
    { code: 'auto', label: '自动检测' }, { code: 'zh', label: '中文' },
    { code: 'en', label: '英语' }, { code: 'ja', label: '日语' },
  ];
  for (const l of items) {
    const o = document.createElement('option');
    o.value = l.code;
    o.textContent = l.label;
    sel.appendChild(o);
  }
  sel.value = want || 'zh';
}

function renderModelSelect() {
  const sel = $('model');
  const want = sel.value || state.currentModel;
  sel.innerHTML = '';
  for (const m of state.models) {
    const o = document.createElement('option');
    o.value = m.id;
    o.textContent = m.installed ? `${m.name}（已装）` : `${m.name} — 未下载`;
    sel.appendChild(o);
  }
  const installed = state.models.filter((m) => m.installed);
  if (want && state.models.some((m) => m.id === want)) sel.value = want;
  else if (installed.length) sel.value = installed[0].id;
  updateModelNote();
}

function updateModelNote() {
  const m = state.models.find((x) => x.id === $('model').value);
  if (!m) { $('model-note').textContent = ''; return; }
  const ram = m.ram_mb ? `内存建议约 ${m.ram_mb} MB` : (m.ram_note || '内存占用未实测');
  $('model-note').textContent = `${humanSize(m.bytes)} · ${ram}${m.installed ? '' : ' · 还没下载'}`;
}

function renderModelList(models, current) {
  const box = $('model-list');
  box.innerHTML = '';
  for (const m of models) {
    const row = document.createElement('div');
    row.className = 'model-row';

    const who = document.createElement('div');
    who.className = 'who';
    const nm = document.createElement('div');
    nm.className = 'nm';
    nm.textContent = m.name;
    const b = document.createElement('span');
    b.className = 'badge ' + (m.installed ? 'ok' : 'miss');
    b.textContent = m.installed ? '已装' : '未下载';
    nm.appendChild(b);
    if (m.current) {
      const c = document.createElement('span');
      c.className = 'badge ok';
      c.textContent = '当前档';
      nm.appendChild(c);
    }
    const dd = document.createElement('div');
    dd.className = 'dd';
    const ram = m.ram_mb ? `内存建议约 ${m.ram_mb} MB` : (m.ram_note || '内存未实测');
    dd.textContent = `${humanSize(m.bytes)} · ${ram}${m.reason ? ' · ' + m.reason : ''}`;
    who.appendChild(nm);
    who.appendChild(dd);

    const ops = document.createElement('div');
    ops.className = 'ops';
    if (!m.installed) {
      const dl = document.createElement('button');
      dl.type = 'button';
      dl.className = 'tiny';
      dl.textContent = `下载 ${humanSize(m.bytes)}`;
      dl.addEventListener('click', () => downloadModel(m));
      ops.appendChild(dl);
    } else if (!m.current) {
      const use = document.createElement('button');
      use.type = 'button';
      use.className = 'tiny';
      use.textContent = '设为当前档';
      use.addEventListener('click', () => selectModel(m));
      ops.appendChild(use);
      const del = document.createElement('button');
      del.type = 'button';
      del.className = 'tiny danger';
      del.textContent = '删除';
      del.addEventListener('click', () => deleteModel(m));
      ops.appendChild(del);
    } else {
      const del = document.createElement('button');
      del.type = 'button';
      del.className = 'tiny danger';
      del.textContent = '删除';
      del.addEventListener('click', () => deleteModel(m));
      ops.appendChild(del);
    }

    row.appendChild(who);
    row.appendChild(ops);
    box.appendChild(row);
  }
  if (current && state.currentModel !== current) state.currentModel = current;
}

// ---------------------------------------------------------------------------
//  模型管理（下载走任务中心：进度是真的字节进度）
// ---------------------------------------------------------------------------

async function downloadModel(m) {
  setError('');
  try {
    const body = await api(`v1/models/${encodeURIComponent(m.id)}/download`, { method: 'POST' });
    if (body && body.already) {
      await loadModels();
      await loadHealth();
      return;
    }
    const taskId = body && body.data && body.data.task_id;
    if (!taskId) throw new Error('服务没有返回 task_id');
    startProgress(`正在下载模型 ${m.id}（${humanSize(m.bytes)}）`);
    followTask(taskId, () => {
      finishProgress();
      loadModels().then(loadHealth);
    });
  } catch (e) {
    setError(`下载模型失败：${e.message}`);
  }
}

async function selectModel(m) {
  setError('');
  try {
    await api(`v1/models/${encodeURIComponent(m.id)}/select`, { method: 'POST' });
    await loadModels();
    await loadHealth();
  } catch (e) {
    setError(`切换档位失败：${e.message}`);
  }
}

async function deleteModel(m) {
  const ok = window.confirm(
    `确定删除模型「${m.name}」吗？\n\n路径：${m.path}\n体积：${humanSize(m.bytes)}\n\n` +
    '删除后这一档要重新下载（几百 MB ~ 1.5 GB），不可撤销。');
  if (!ok) return;
  setError('');
  try {
    const r = await api(`v1/models/${encodeURIComponent(m.id)}/delete`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true }),
    });
    setError('');
    window.alert(`已删除 ${m.id}（释放 ${humanSize(r.freed_bytes)}）。当前档：${r.current}`);
    await loadModels();
    await loadHealth();
  } catch (e) {
    setError(`删除模型失败：${e.message}`);
  }
}

// ---------------------------------------------------------------------------
//  选文件 / 录音
// ---------------------------------------------------------------------------

function setFile(file, name) {
  state.file = file;
  state.fileName = name || (file && file.name) || 'audio';
  show('fileinfo', true);
  $('fi-name').textContent = state.fileName;
  $('fi-meta').textContent = `大小 ${humanSize(file ? file.size : 0)} · 类型 ${(file && file.type) || '未知'}`;
  const url = URL.createObjectURL(file);
  const player = $('player');
  player.src = url;
  player.hidden = false;
  $('btn-run').disabled = false;
  setError('');
}

function clearAll() {
  state.file = null;
  state.fileName = '';
  state.result = null;
  $('file').value = '';
  show('fileinfo', false);
  const player = $('player');
  player.removeAttribute('src');
  player.hidden = true;
  $('btn-run').disabled = true;
  $('result-wrap').hidden = true;
  $('empty-hint').hidden = false;
  setError('');
  show('progress-wrap', false);
  if (state.es) { state.es.close(); state.es = null; }
}

function pickRecorderMime() {
  const cands = [
    'audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/ogg;codecs=opus', 'audio/mpeg',
  ];
  if (typeof MediaRecorder === 'undefined') return '';
  for (const c of cands) {
    if (MediaRecorder.isTypeSupported && MediaRecorder.isTypeSupported(c)) return c;
  }
  return '';
}

function extForMime(mime) {
  if (!mime) return 'webm';
  if (mime.includes('mp4')) return 'm4a';
  if (mime.includes('ogg')) return 'ogg';
  if (mime.includes('mpeg')) return 'mp3';
  return 'webm';
}

async function toggleRecord() {
  const btn = $('btn-record');
  if (state.recorder && state.recorder.state === 'recording') {
    state.recorder.stop();
    return;
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    setError('这个浏览器不支持麦克风录音（navigator.mediaDevices 不可用）；请改用「选择文件」。');
    return;
  }
  try {
    // getUserMedia 在"权限弹窗还没被回答"时既不 resolve 也不 reject，
    // 界面会看起来像"点了没反应"。加一个超时并如实报出来 ——
    // 用户至少知道该去看权限设置，而不是反复点按钮。
    const stream = await Promise.race([
      navigator.mediaDevices.getUserMedia({ audio: true }),
      new Promise((_, reject) => setTimeout(
        () => reject(new Error('等待麦克风授权超过 15 秒')), 15000)),
    ]);
    const mime = pickRecorderMime();
    const rec = mime ? new MediaRecorder(stream, { mimeType: mime }) : new MediaRecorder(stream);
    state.recorder = rec;
    state.recChunks = [];
    rec.addEventListener('dataavailable', (ev) => { if (ev.data && ev.data.size) state.recChunks.push(ev.data); });
    rec.addEventListener('stop', () => {
      stream.getTracks().forEach((t) => t.stop());
      const type = rec.mimeType || mime || 'audio/webm';
      const blob = new Blob(state.recChunks, { type });
      setFile(blob, `录音-${new Date().toISOString().slice(11, 19).replace(/:/g, '')}.${extForMime(type)}`);
      btn.textContent = '● 开始录音';
      show('rec-time', false);
      if (state.recTimer) { clearInterval(state.recTimer); state.recTimer = null; }
    });
    rec.start();
    btn.textContent = '■ 停止录音';
    show('rec-time', true);
    state.recStart = Date.now();
    state.recTimer = setInterval(() => {
      $('rec-time').textContent = fmtClock((Date.now() - state.recStart) / 1000);
    }, 250);
  } catch (e) {
    setError(`打开麦克风失败：${e.message}。`+
      '请检查浏览器的麦克风权限（地址栏左侧的图标 → 允许），或直接改用「选择文件」；'+
      '经面板别名 http://<主机>/stt/ 访问时，浏览器要求 HTTPS 或 localhost 才允许录音 —— '+
      'http 访问下请改用直连端口 http://127.0.0.1:8892/。');
  }
}

// ---------------------------------------------------------------------------
//  进度（任务中心 SSE）
// ---------------------------------------------------------------------------

function startProgress(label) {
  show('progress-wrap', true);
  $('progress-label').textContent = label;
  $('progress-pct').textContent = '0%';
  $('progress').value = 0;
  $('progress-log').textContent = '';
}

function appendLog(text) {
  const box = $('progress-log');
  box.textContent += (box.textContent ? '\n' : '') + text;
  box.scrollTop = box.scrollHeight;
}

function setProgressPct(p) {
  const v = Math.max(0, Math.min(100, Math.round(p)));
  $('progress').value = v;
  $('progress-pct').textContent = `${v}%`;
}

function finishProgress() {
  setProgressPct(100);
  if (state.es) { state.es.close(); state.es = null; }
  setTimeout(() => show('progress-wrap', false), 800);
}

// followTask 订阅 SSE 进度（契约与面板任务中心一致：meta / lines / done）。
// 进度是**真的**：转写按 60 秒切片报"已完成 N/M 片"，下载报真实字节数。
function followTask(taskId, onDone) {
  if (state.es) { state.es.close(); state.es = null; }
  const src = new EventSource(`v1/tasks/${encodeURIComponent(taskId)}/stream`);
  state.es = src;
  let lastPct = 0;

  const handleLines = (payload) => {
    for (const line of payload.lines || []) {
      appendLog(line.text);
      // 把日志里的"已完成 3/10 片"或百分比翻译成进度条（只要有明确数字）。
      let m = /已完成\s*(\d+)\/(\d+)/.exec(line.text);
      if (m) { setProgressPct((Number(m[1]) / Number(m[2])) * 100); continue; }
      m = /正在转写第\s*(\d+)\/(\d+)\s*片/.exec(line.text);
      if (m) { setProgressPct(((Number(m[1]) - 1) / Number(m[2])) * 100); continue; }
      m = /(\d{1,3})%/.exec(line.text);
      if (m) {
        const p = Number(m[1]);
        if (p >= lastPct) { lastPct = p; setProgressPct(p); }
      }
    }
  };

  src.addEventListener('meta', (ev) => {
    try {
      const payload = JSON.parse(ev.data);
      if (payload.task) $('progress-label').textContent = payload.task.title || '正在处理…';
      if (payload.lines) { /* 元信息里没有行 */ }
    } catch (_) { /* 元信息解析不了不影响进度 */ }
  });
  src.addEventListener('lines', (ev) => {
    try { handleLines(JSON.parse(ev.data)); } catch (_) { /* 忽略坏行 */ }
  });
  src.addEventListener('done', (ev) => {
    let failed = false;
    try {
      const payload = JSON.parse(ev.data);
      failed = payload.task && payload.task.status === 'failed';
      if (failed) setError(`任务失败：${(payload.task && payload.task.error) || '见任务日志'}`);
    } catch (_) { /* 忽略 */ }
    src.close();
    state.es = null;
    if (onDone) onDone(!failed);
  });
  src.addEventListener('error', () => {
    // EventSource 会自动重连；真失败时 done 事件已经给过结论。
  });
}

// ---------------------------------------------------------------------------
//  转写
// ---------------------------------------------------------------------------

async function run() {
  if (!state.file || state.busy) return;
  state.busy = true;
  $('btn-run').disabled = true;
  setError('');
  $('result-wrap').hidden = true;
  $('empty-hint').hidden = true;
  startProgress('正在上传并转写…');
  appendLog(`文件：${state.fileName}（${humanSize(state.file.size)}）`);

  const fd = new FormData();
  fd.append('file', state.file, state.fileName);
  fd.append('model', $('model').value);
  fd.append('language', $('language').value);
  fd.append('translate', $('translate').value);
  // 界面固定用 verbose_json 取回**分段**（纯文本与字幕都由它渲染）；
  // 导出格式见上面的下拉框。直接调接口时 response_format 完全按 OpenAI 语义生效。
  fd.append('response_format', 'verbose_json');

  try {
    const res = await fetch('v1/audio/transcriptions', { method: 'POST', body: fd });
    const text = await res.text();
    let body = null;
    try { body = text ? JSON.parse(text) : null; } catch (_) { body = null; }

    if (res.status === 202 && body && body.data && body.data.task_id) {
      appendLog(body.data.note || '已转入任务');
      setProgressPct(0);
      followTask(body.data.task_id, (ok) => {
        state.busy = false;
        $('btn-run').disabled = false;
        finishProgress();
        if (ok) loadTaskResult(body.data.task_id);
      });
      return;
    }
    if (!res.ok) {
      const msg = (body && (body.msg || (body.error && body.error.message))) || text || `HTTP ${res.status}`;
      throw new Error(msg);
    }
    // 同步路径的结果是**当场**给出的：立刻收起进度条，
    // 不要留一条停在 100% 的进度条让人以为后台还在跑。
    finishProgress();
    show('progress-wrap', false);
    state.busy = false;
    $('btn-run').disabled = false;
    renderResult(body || {});
  } catch (e) {
    state.busy = false;
    $('btn-run').disabled = false;
    finishProgress();
    setError(`转写失败：${e.message}`);
    $('empty-hint').hidden = false;
  }
}

async function loadTaskResult(taskId) {
  try {
    const body = await api(`v1/tasks/${encodeURIComponent(taskId)}/result?meta=1`);
    renderResult(body.result || {});
  } catch (e) {
    setError(`取任务结果失败：${e.message}`);
  }
}

function renderResult(r) {
  const segs = Array.isArray(r.segments) ? r.segments : [];
  state.result = {
    text: r.text || '',
    segments: segs,
    language: r.language || '',
    model: r.model || '',
    durationMs: r.duration_ms || 0,
    chunks: r.chunks || 0,
    elapsedMs: r.elapsed_ms || 0,
  };
  $('out-text').textContent = state.result.text || '（没有识别到文字：可能是静音，或语言选错了）';
  const box = $('out-seg');
  box.innerHTML = '';
  if (!segs.length) {
    const p = document.createElement('div');
    p.className = 'muted small';
    p.textContent = '没有分段（音频里没有可识别的语音）。';
    box.appendChild(p);
  }
  for (const s of segs) {
    const d = document.createElement('div');
    d.className = 'seg';
    const t = document.createElement('div');
    t.className = 't';
    t.textContent = `${srtTime(s.start)} → ${srtTime(s.end)}`;
    const x = document.createElement('div');
    x.className = 'x';
    x.textContent = s.text;
    d.appendChild(t);
    d.appendChild(x);
    box.appendChild(d);
  }
  const meta = $('result-meta');
  meta.textContent = [
    `档位 ${state.result.model || '-'}`,
    `识别语言 ${state.result.language || '未知'}`,
    `时长 ${fmtClock(state.result.durationMs / 1000)}`,
    `分段 ${segs.length}`,
    state.result.chunks ? `切片 ${state.result.chunks}` : '',
    state.result.elapsedMs ? `引擎耗时 ${(state.result.elapsedMs / 1000).toFixed(1)} 秒` : '',
  ].filter(Boolean).join(' · ');

  show('result-wrap', true);
  $('empty-hint').hidden = true;
  selectTab('text');
}

function selectTab(which) {
  const isText = which === 'text';
  $('tab-text').setAttribute('aria-selected', String(isText));
  $('tab-seg').setAttribute('aria-selected', String(!isText));
  $('out-text').hidden = !isText;
  $('out-seg').hidden = isText;
}

// ---------------------------------------------------------------------------
//  复制 / 下载
// ---------------------------------------------------------------------------

async function copyText(s) {
  try {
    await navigator.clipboard.writeText(s);
    setError('');
  } catch (_) {
    // 剪贴板 API 在非安全上下文/无权限时会失败：退回选中文本让用户自己复制。
    const ta = document.createElement('textarea');
    ta.value = s;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (_) { window.alert('复制失败，请手工选中文本'); }
    document.body.removeChild(ta);
  }
}

function segmentsAsText() {
  if (!state.result) return '';
  return state.result.segments.map((s) => `[${srtTime(s.start)} → ${srtTime(s.end)}] ${s.text}`).join('\n');
}

function download(name, content, mime) {
  const blob = new Blob([content], { type: mime || 'text/plain;charset=utf-8' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = name;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  setTimeout(() => URL.revokeObjectURL(a.href), 4000);
}

function buildSrt() {
  if (!state.result) return '';
  return state.result.segments
    .map((s, i) => `${i + 1}\n${srtTime(s.start)} --> ${srtTime(s.end)}\n${s.text}\n`)
    .join('\n');
}

function buildVtt() {
  if (!state.result) return '';
  return 'WEBVTT\n\n' + state.result.segments
    .map((s, i) => `${i + 1}\n${vttTime(s.start)} --> ${vttTime(s.end)}\n${s.text}\n`)
    .join('\n');
}

function buildJSON() {
  if (!state.result) return '{}';
  return JSON.stringify({
    task: 'transcribe',
    language: state.result.language || 'unknown',
    duration: state.result.durationMs / 1000,
    text: state.result.text,
    segments: state.result.segments.map((s) => ({
      id: s.id, start: s.start, end: s.end, text: s.text,
    })),
    model: state.result.model,
    chunks: state.result.chunks,
    elapsed_ms: state.result.elapsedMs,
  }, null, 2);
}

function downloadSelectedFormat() {
  const f = $('format').value;
  const base = (state.fileName || 'transcript').replace(/\.[^.]+$/, '') || 'transcript';
  if (f === 'srt') return download(`${base}.srt`, buildSrt(), 'application/x-subrip;charset=utf-8');
  if (f === 'vtt') return download(`${base}.vtt`, buildVtt(), 'text/vtt;charset=utf-8');
  if (f === 'json') return download(`${base}.json`, buildJSON(), 'application/json;charset=utf-8');
  return download(`${base}.txt`, (state.result && state.result.text) || '', 'text/plain;charset=utf-8');
}

// ---------------------------------------------------------------------------
//  接线
// ---------------------------------------------------------------------------

const drop = $('drop');
drop.addEventListener('click', () => $('file').click());
drop.addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); $('file').click(); }
});
for (const evName of ['dragenter', 'dragover']) {
  drop.addEventListener(evName, (ev) => { ev.preventDefault(); drop.classList.add('over'); });
}
for (const evName of ['dragleave', 'drop']) {
  drop.addEventListener(evName, (ev) => { ev.preventDefault(); drop.classList.remove('over'); });
}
drop.addEventListener('drop', (ev) => {
  const f = ev.dataTransfer && ev.dataTransfer.files && ev.dataTransfer.files[0];
  if (f) setFile(f, f.name);
});

$('file').addEventListener('change', (ev) => {
  const f = ev.target.files && ev.target.files[0];
  if (f) setFile(f, f.name);
});

$('btn-record').addEventListener('click', toggleRecord);
$('btn-run').addEventListener('click', run);
$('btn-reset').addEventListener('click', clearAll);
$('model').addEventListener('change', updateModelNote);
$('tab-text').addEventListener('click', () => selectTab('text'));
$('tab-seg').addEventListener('click', () => selectTab('seg'));
$('btn-copy').addEventListener('click', () => copyText($('out-text').textContent || ''));
$('btn-copy-seg').addEventListener('click', () => copyText(segmentsAsText()));
$('btn-dl-txt').addEventListener('click', () => download(
  ((state.fileName || 'transcript').replace(/\.[^.]+$/, '') || 'transcript') + '.txt',
  (state.result && state.result.text) || '', 'text/plain;charset=utf-8'));
$('btn-dl-srt').addEventListener('click', () => download(
  ((state.fileName || 'transcript').replace(/\.[^.]+$/, '') || 'transcript') + '.srt',
  buildSrt(), 'application/x-subrip;charset=utf-8'));
$('btn-dl-fmt').addEventListener('click', downloadSelectedFormat);

loadHealth().then(loadModels);
