// macOS 语音合成界面的前端（原生 ESM，无构建步骤、无外部依赖）。
//
// 接口都在同一个服务里，且**全部用相对路径**（v1/voices 而不是 /v1/voices）：
// 页面既可能出现在直连端口（http://127.0.0.1:8891/），也可能出现在面板别名
// （http://<主机>/speech/）下 —— 用绝对路径的话别名下会打到站点根，直接白屏。
//
// 这个文件的原则与后端一致：**错误如实显示**。不吞异常、不把失败画成成功，
// 后端的错误正文（say 失败 / 音色不存在 / 超长 / 格式不可用）原样显示出来。

const $ = (id) => document.getElementById(id);

const el = {
  engineBadge: $('engine-badge'),
  engineDot: $('engine-dot'),
  engineText: $('engine-text'),
  globalError: $('global-error'),
  text: $('text'),
  textStat: $('text-stat'),
  textHint: $('text-hint'),
  voice: $('voice'),
  voiceNote: $('voice-note'),
  format: $('format'),
  formatNote: $('format-note'),
  speed: $('speed'),
  speedVal: $('speed-val'),
  btnGenerate: $('btn-generate'),
  btnReset: $('btn-reset'),
  btnSample: $('btn-sample'),
  progressWrap: $('progress-wrap'),
  progress: $('progress'),
  progressLabel: $('progress-label'),
  progressPct: $('progress-pct'),
  progressLog: $('progress-log'),
  errorBox: $('error-box'),
  resultWrap: $('result-wrap'),
  player: $('player'),
  meta: $('meta'),
  download: $('btn-download'),
  copyCurl: $('btn-copy-curl'),
  emptyHint: $('empty-hint'),
  footEngine: $('foot-engine'),
};

const state = {
  health: null,
  voices: [],
  maxChars: 20000,
  syncMaxChars: 600,
  busy: false,
  objectUrl: '',
  source: null,
  lastRequest: null,
};

const SAMPLE = '你好，这是 ZizPanel 的语音合成测试。\n第二句用来验证多段拼接是否正常。';

// ---------- 通用小工具 ----------

function showError(msg) {
  el.errorBox.hidden = !msg;
  el.errorBox.textContent = msg || '';
}

function showGlobalError(msg) {
  el.globalError.hidden = !msg;
  el.globalError.textContent = msg || '';
}

function fmtBytes(n) {
  if (!n && n !== 0) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(2) + ' MB';
}

function setBusy(busy) {
  state.busy = busy;
  el.btnGenerate.disabled = busy;
  el.btnGenerate.textContent = busy ? '⏳ 正在合成…' : '▶ 生成语音';
}

// 后端错误体形状：{ok:false, error:{message}, msg}（OpenAI 风格 + 面板的 msg）。
async function readError(res) {
  let text = '';
  try {
    text = await res.text();
  } catch {
    return 'HTTP ' + res.status;
  }
  try {
    const j = JSON.parse(text);
    return (j.error && j.error.message) || j.msg || text || ('HTTP ' + res.status);
  } catch {
    return text || ('HTTP ' + res.status);
  }
}

// ---------- 引擎状态 / 音色 / 格式 ----------

async function loadHealth() {
  try {
    const res = await fetch('healthz', { headers: { Accept: 'application/json' } });
    const h = await res.json();
    state.health = h;
    if (h.max_chars) state.maxChars = h.max_chars;
    if (h.sync_max_chars) state.syncMaxChars = h.sync_max_chars;
    renderEngine(h, res.ok);
    renderFormats(h);
  } catch (e) {
    renderEngine(null, false);
    showGlobalError('探测引擎失败：' + e.message + '（界面本身可用，但生成一定会失败）');
  }
}

function renderEngine(h, httpOk) {
  const ok = httpOk && h && h.ok;
  el.engineDot.className = 'dot ' + (ok ? 'ok' : 'bad');
  if (!h) {
    el.engineText.textContent = '引擎状态未知';
    el.engineBadge.title = '探测 /healthz 失败';
    el.footEngine.textContent = '引擎状态未知';
    return;
  }
  if (ok) {
    el.engineText.textContent = `引擎可用 · ${h.voices} 个音色（中文 ${h.chinese_voices}）`;
    el.engineBadge.title = `${h.say_bin} · 采样率 ${h.sample_rate}Hz`;
    el.footEngine.textContent =
      `${h.say_bin} · ${h.voices} 个音色（中文 ${h.chinese_voices}）· ` +
      (h.ffmpeg ? 'mp3 可用' : 'mp3 不可用（没有 ffmpeg）');
  } else {
    el.engineText.textContent = '引擎不可用';
    el.engineBadge.title = h.reason || '未知原因';
    el.footEngine.textContent = '引擎不可用：' + (h.reason || '未知原因');
    showGlobalError('语音合成引擎当前不可用：' + (h.reason || '未知原因'));
  }
}

function renderFormats(h) {
  const formats = (h && h.formats) || [];
  el.format.innerHTML = '';
  for (const f of formats) {
    const opt = document.createElement('option');
    opt.value = f.format;
    opt.textContent = f.format + (f.available ? '' : '（不可用）');
    opt.disabled = !f.available;
    if (!f.available) opt.title = f.reason || '不可用';
    el.format.appendChild(opt);
  }
  const first = formats.find((f) => f.available);
  if (first) el.format.value = first.format;
  updateFormatNote();
}

function updateFormatNote() {
  const h = state.health;
  const chosen = el.format.value;
  const f = ((h && h.formats) || []).find((x) => x.format === chosen);
  const parts = [];
  if (chosen === 'aiff') parts.push('say 的原生产物（链路最短）');
  if (chosen === 'wav') parts.push('未压缩 PCM 16bit / 22050Hz 单声道');
  if (chosen === 'm4a') parts.push('AAC 编码，体积小，适合试听/分享');
  if (chosen === 'mp3') parts.push('需要 ffmpeg 编码（系统自带的 afconvert 不能编码 mp3）');
  if (f && !f.available) parts.push('当前不可用：' + (f.reason || ''));
  el.formatNote.textContent = parts.join('；');
}

async function loadVoices() {
  try {
    const res = await fetch('v1/voices', { headers: { Accept: 'application/json' } });
    if (!res.ok) {
      showGlobalError('取音色清单失败：' + (await readError(res)));
      return;
    }
    const j = await res.json();
    state.voices = Array.isArray(j.data) ? j.data : [];
    renderVoices(j);
  } catch (e) {
    showGlobalError('取音色清单失败：' + e.message);
  }
}

function renderVoices(meta) {
  const zh = state.voices.filter((v) => v.chinese);
  const other = state.voices.filter((v) => !v.chinese);
  const groups = [
    ['中文音色（zh / yue）', zh],
    ['其它音色', other],
  ];
  el.voice.innerHTML = '';
  const auto = document.createElement('option');
  auto.value = '';
  auto.textContent = '（自动：中文文本优先用中文音色）';
  el.voice.appendChild(auto);
  for (const [label, list] of groups) {
    if (!list.length) continue;
    const g = document.createElement('optgroup');
    g.label = `${label} · ${list.length}`;
    for (const v of list) {
      const opt = document.createElement('option');
      opt.value = v.name;
      opt.textContent = `${v.chinese ? '🈶 ' : ''}${v.name}（${v.lang}）`;
      if (v.example) opt.title = v.example;
      g.appendChild(opt);
    }
    el.voice.appendChild(g);
  }
  // 默认选中后端"中文文本会自动挑"的那个，让界面显示与实际行为一致。
  const preferred = state.voices.find((v) => v.name === 'Tingting');
  if (preferred) el.voice.value = preferred.name;
  const zhCount = zh.length;
  el.voiceNote.textContent = zhCount
    ? `共 ${state.voices.length} 个音色（中文 ${zhCount} 个）。中文文本建议直接用中文音色。`
    : `共 ${state.voices.length} 个音色；这台机器没有中文音色（可在「系统设置 → 辅助功能 → 朗读内容 → 系统声音」里添加）。`;
  if (meta && meta.source) el.voiceNote.title = '来源：' + meta.source;
}

// ---------- 文本统计 ----------

function updateTextStat() {
  const n = [...el.text.value.trim()].length;
  el.textStat.textContent = `${n} 字`;
  if (n === 0) {
    el.textHint.textContent = '';
    return;
  }
  if (n > state.maxChars) {
    el.textHint.textContent = `超过单次上限 ${state.maxChars} 字（请拆成多次）`;
  } else if (n > state.syncMaxChars) {
    el.textHint.textContent = `超过 ${state.syncMaxChars} 字：会转入任务中心，带真实进度`;
  } else {
    el.textHint.textContent = '同步返回（秒级）';
  }
}

// ---------- 生成 ----------

async function generate() {
  if (state.busy) return;
  const input = el.text.value.trim();
  showError('');
  showGlobalError('');
  if (!input) {
    showError('要合成的文本为空：请先输入内容（引擎对空文本会"成功"地产出一段静音，面板在请求前就拒绝）。');
    return;
  }
  if ([...input].length > state.maxChars) {
    showError(`文本 ${[...input].length} 字，超过单次上限 ${state.maxChars} 字，请拆成多次。`);
    return;
  }
  resetResult();
  setBusy(true);
  const req = {
    input,
    voice: el.voice.value,
    format: el.format.value,
    speed: Number(el.speed.value),
  };
  state.lastRequest = req;
  try {
    const res = await fetch('v1/audio/speech', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'audio/*, application/json' },
      body: JSON.stringify(req),
    });
    const ctype = (res.headers.get('Content-Type') || '').toLowerCase();
    if (res.status === 202) {
      const j = await res.json();
      await runTask(j.data, req);
      return;
    }
    if (!res.ok) {
      showError(await readError(res));
      return;
    }
    if (!ctype.startsWith('audio/')) {
      // 既不是音频也不是 202：如实报错，不把意外响应当成功。
      showError('服务返回了非音频响应（Content-Type: ' + ctype + '）：' + (await readError(res)));
      return;
    }
    const blob = await res.blob();
    presentAudio(blob, {
      format: res.headers.get('X-Zizpanel-Format') || req.format,
      voice: res.headers.get('X-Zizpanel-Voice') || req.voice || '（系统默认）',
      lang: res.headers.get('X-Zizpanel-Lang') || '',
      rate: res.headers.get('X-Zizpanel-Rate') || '',
      speed: res.headers.get('X-Zizpanel-Effective-Speed') || '',
      chars: res.headers.get('X-Zizpanel-Chars') || '',
      segments: res.headers.get('X-Zizpanel-Segments') || '',
      sync: true,
    });
  } catch (e) {
    showError('请求失败：' + e.message);
  } finally {
    setBusy(false);
  }
}

// runTask 走长文本任务：SSE 进度 → 完成后取音频。
function runTask(data, req) {
  return new Promise((resolve) => {
    if (!data || !data.task_id) {
      showError('服务返回了 202 但没有 task_id，无法跟踪进度');
      resolve();
      return;
    }
    const total = Number(data.segments) || 0;
    // **不用服务端给的 stream_url / audio_url 去请求**：那是"根绝对路径"
    // （/v1/tasks/…），对直连端口是对的，但在面板别名 /speech/ 下会解析到
    // 站点根 → 404（真机验证时就是这样失败的）。这里一律用**相对当前目录**
    // 的路径自己拼：直连 http://127.0.0.1:8891/ 与别名 http://<主机>/speech/
    // 两种入口都成立。
    const taskBase = 'v1/tasks/' + encodeURIComponent(data.task_id);
    el.progressWrap.hidden = false;
    el.progress.value = 0;
    el.progressPct.textContent = '0%';
    el.progressLabel.textContent = `已转入任务 ${data.task_id}（${data.chars} 字 / ${total} 段）`;
    el.progressLog.textContent = '';

    const finish = async (errMsg) => {
      if (state.source) {
        state.source.close();
        state.source = null;
      }
      if (errMsg) {
        el.progressLabel.textContent = '任务失败';
        showError(errMsg);
        resolve();
        return;
      }
      el.progress.value = 100;
      el.progressPct.textContent = '100%';
      el.progressLabel.textContent = '任务完成，正在取回音频…';
      try {
        const res = await fetch(taskBase + '/audio');
        if (!res.ok) {
          showError('取音频失败：' + (await readError(res)));
          resolve();
          return;
        }
        const blob = await res.blob();
        presentAudio(blob, {
          format: res.headers.get('X-Zizpanel-Format') || req.format,
          voice: res.headers.get('X-Zizpanel-Voice') || req.voice || '（系统默认）',
          lang: res.headers.get('X-Zizpanel-Lang') || '',
          rate: res.headers.get('X-Zizpanel-Rate') || '',
          speed: res.headers.get('X-Zizpanel-Effective-Speed') || '',
          chars: res.headers.get('X-Zizpanel-Chars') || '',
          segments: res.headers.get('X-Zizpanel-Segments') || '',
          sync: false,
        });
      } catch (e) {
        showError('取音频失败：' + e.message);
      }
      resolve();
    };

    const src = new EventSource(taskBase + '/stream');
    state.source = src;
    src.addEventListener('lines', (ev) => {
      let payload;
      try {
        payload = JSON.parse(ev.data);
      } catch {
        return;
      }
      for (const line of payload.lines || []) {
        const msg = line.msg || line.text || '';
        if (!msg) continue;
        el.progressLog.textContent += msg + '\n';
        el.progressLog.scrollTop = el.progressLog.scrollHeight;
        // 真实进度：后端每合成完一段就报一次"已合成 N/M 段"。
        const m = /已合成\s+(\d+)\s*\/\s*(\d+)\s*段/.exec(msg);
        if (m) {
          const done = Number(m[1]);
          const all = Number(m[2]) || total || 1;
          const pct = Math.min(99, Math.round((done / all) * 100));
          el.progress.value = pct;
          el.progressPct.textContent = pct + '%';
        }
      }
    });
    src.addEventListener('done', (ev) => {
      let meta = null;
      try {
        meta = JSON.parse(ev.data).task;
      } catch {
        meta = null;
      }
      if (meta && meta.status === 'failed') {
        finish(meta.error || '合成失败（任务日志里有原因）');
        return;
      }
      if (meta && meta.status === 'canceled') {
        finish('任务已取消，没有音频产出');
        return;
      }
      finish(null);
    });
    src.onerror = () => {
      // EventSource 会自动重连；真失败时 done 事件已经给过结论。
      // 这里不直接判失败（否则网络抖动会被误报成合成失败）。
      el.progressLabel.textContent = '进度流中断，正在重连…（音频仍可在任务完成后取回）';
    };
  });
}

function presentAudio(blob, info) {
  if (state.objectUrl) URL.revokeObjectURL(state.objectUrl);
  state.objectUrl = URL.createObjectURL(blob);
  el.player.src = state.objectUrl;
  const name = 'speech.' + (info.format || 'aiff');
  el.download.href = state.objectUrl;
  el.download.download = name;
  el.download.textContent = `⬇ 下载 ${name}（${fmtBytes(blob.size)}）`;
  el.resultWrap.hidden = false;
  el.emptyHint.hidden = true;

  const rows = [
    ['音色', info.voice + (info.lang ? `（${info.lang}）` : '')],
    ['格式', info.format],
    ['大小', fmtBytes(blob.size)],
    ['实际语速', info.rate ? `${info.rate} 词/分钟（≈ ${info.speed}×）` : '—'],
    ['字符数', info.chars || '—'],
    ['分段数', info.segments || '1'],
    ['返回路径', info.sync ? '同步（200 + 音频）' : '任务（202 + task_id）'],
  ];
  el.meta.innerHTML = '';
  for (const [k, v] of rows) {
    const dt = document.createElement('dt');
    dt.textContent = k;
    const dd = document.createElement('dd');
    dd.textContent = String(v);
    el.meta.append(dt, dd);
  }
}

function resetResult() {
  showError('');
  el.progressWrap.hidden = true;
  el.progressLog.textContent = '';
  el.progress.value = 0;
}

function copyCurl() {
  const req = state.lastRequest || {
    input: el.text.value.trim(),
    voice: el.voice.value,
    format: el.format.value,
    speed: Number(el.speed.value),
  };
  const base = location.origin + location.pathname.replace(/\/$/, '');
  const cmd =
    `curl -sS -X POST '${base}/v1/audio/speech' \\\n` +
    `  -H 'Content-Type: application/json' \\\n` +
    `  -d '${JSON.stringify(req)}' \\\n` +
    `  -o /tmp/speech.${req.format || 'aiff'}`;
  navigator.clipboard?.writeText(cmd).then(
    () => {
      el.copyCurl.textContent = '✓ 已复制';
      setTimeout(() => (el.copyCurl.textContent = '复制 curl 命令'), 1500);
    },
    () => showError('复制失败（浏览器未授权剪贴板）：\n' + cmd),
  );
}

// ---------- 接线 ----------

el.text.addEventListener('input', updateTextStat);
el.speed.addEventListener('input', () => {
  el.speedVal.textContent = Number(el.speed.value).toFixed(2) + '×';
});
el.format.addEventListener('change', updateFormatNote);
el.btnGenerate.addEventListener('click', generate);
el.btnReset.addEventListener('click', () => {
  el.text.value = '';
  updateTextStat();
  resetResult();
  el.resultWrap.hidden = true;
  el.emptyHint.hidden = false;
});
el.btnSample.addEventListener('click', () => {
  el.text.value = SAMPLE;
  updateTextStat();
});
el.copyCurl.addEventListener('click', copyCurl);
// ⌘/Ctrl+Enter 生成
el.text.addEventListener('keydown', (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
    e.preventDefault();
    generate();
  }
});

updateTextStat();
loadHealth().then(loadVoices);
