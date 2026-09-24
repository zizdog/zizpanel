// ==UserScript==
// @name         aria2 下载助手（ZizPanel）
// @namespace    zizpanel/aria2-helper
// @version      2.0.0
// @description  把网页里的下载链接/磁力一键推送到自建 aria2（Alt+点击 / 右键菜单 / 批量 / 任务管理 / 连接自检）
// @author       ZizPanel
// @match        *://*/*
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_xmlhttpRequest
// @grant        GM_registerMenuCommand
// @connect      aria2.zizdog.com
// @connect      *
// @run-at       document-idle
// @noframes
// ==/UserScript==
//
// 只需要一个值（填进「设置」，点「测试连接」会告诉你对不对）：
//   RPC 密钥（rpc-secret）：aria2 自己的凭据，body 里 token:<rpc-secret>。
//   在跑 aria2 的那台机器上：grep rpc-secret ~/aria/aria2.conf
//
// 面板**不再**要求额外的 X-Aria2-Token：默认（兼容）模式下它不按浏览器来源拦，
// 保护由 rpc-secret 承担；只有你在「面板设置 → 访问与安全」里打开严格模式后，
// 浏览器里**别的网站**才打不进来（扩展仍能连 —— 它不带 Sec-Fetch-Site 头）。
//
// 刻意不做的事（上一版在这些地方翻车）：WebSocket 传输、伪造 Origin/Referer、
// 自动去面板抓凭证（跨站带 cookie 读响应会被 CORS 挡住）、跳转页二次解析。
// 只保留一条可预测的路径：GM_xmlhttpRequest POST JSON-RPC。

(function () {
  'use strict';

  const DEFAULTS = {
    rpc: 'https://aria2.zizdog.com:8888/jsonrpc',
    rpcSecret: '',
    dir: '',
    altClick: true,
    exts: 'zip,rar,7z,tar,gz,tgz,bz2,xz,zst,iso,img,dmg,pkg,msi,deb,rpm,apk,exe,mp4,mkv,avi,mov,flv,webm,wmv,mp3,flac,wav,ape,pdf,epub,mobi,azw3,torrent,bin',
    referer: false,
    headers: '',
    ballPos: null,
  };
  let cfg = Object.assign({}, DEFAULTS);
  const store = {
    read(k) { try { return typeof GM_getValue === 'function' ? GM_getValue(k) : localStorage.getItem('a2h:' + k); } catch (e) { return null; } },
    write(k, v) { try { if (typeof GM_setValue === 'function') GM_setValue(k, v); else localStorage.setItem('a2h:' + k, v); } catch (e) {} },
  };
  try { const s = store.read('cfg'); if (s) cfg = Object.assign(cfg, typeof s === 'string' ? JSON.parse(s) : s); } catch (e) {}
  const saveCfg = () => store.write('cfg', JSON.stringify(cfg));

  /* ---------------- 与 RPC 说话 ---------------- */

  // 头值必须干净：CR/LF 会让个别浏览器/管理器直接抛 Type error。
  const clean = (v) => String(v == null ? '' : v).replace(/[\r\n\u0000-\u001f\u007f]/g, ' ').trim();

  function gmPost(url, headers, body, timeout) {
    return new Promise((resolve, reject) => {
      const doReq = (fn) => fn({
        method: 'POST', url, headers, data: body, timeout: timeout || 15000,
        onload: (r) => resolve(r),
        onerror: (e) => reject(new Error('网络层失败：' + ((e && (e.error || e.statusText)) || '无法连接 ' + url))),
        ontimeout: () => reject(new Error('请求超时（' + (timeout || 15000) + 'ms）')),
        onabort: () => reject(new Error('请求被中断')),
      });
      const native = typeof GM_xmlhttpRequest === 'function' ? GM_xmlhttpRequest
        : (typeof GM !== 'undefined' && GM && typeof GM.xmlHttpRequest === 'function' ? GM.xmlHttpRequest : null);
      if (!native) return reject(new Error('这个脚本管理器没有 GM_xmlhttpRequest（需要 Tampermonkey / Violentmonkey）'));
      doReq(native);
    });
  }

  async function rpc(method, params, timeout) {
    const url = clean(cfg.rpc);
    if (!/^https?:\/\/.+/i.test(url)) throw new Error('RPC 地址没填对：' + (cfg.rpc || '(空)'));
    const headers = { 'Content-Type': 'application/json' };
    const all = clean(cfg.rpcSecret) ? ['token:' + clean(cfg.rpcSecret)].concat(params || []) : (params || []);
    const body = JSON.stringify({ jsonrpc: '2.0', id: 'a2h-' + Date.now(), method, params: all });

    const res = await gmPost(url, headers, body, timeout);
    const raw = String(res.responseText != null ? res.responseText : (res.response || ''));
    if (res.status === 403) {
      throw new Error('被面板拒了（403）：' + (raw.slice(0, 160) || '来源校验未通过') +
        '\n→ 面板开了「aria2 严格模式」：到「面板设置 → 访问与安全」把它关掉（默认就是关的）');
    }
    if (res.status !== 200) throw new Error('HTTP ' + res.status + '：' + (raw.slice(0, 200) || '(空响应)'));
    let data;
    try { data = JSON.parse(raw); } catch (e) { throw new Error('响应不是 JSON：' + raw.slice(0, 160)); }
    if (data.error) {
      const m = (data.error.message || '') + ' (code ' + data.error.code + ')';
      throw new Error(/unauthor/i.test(m) ? 'aria2 说没授权（Unauthorized）：rpc-secret 不对 → ' + m : 'aria2 返回错误：' + m);
    }
    return data.result;
  }

  // 推送一条链接（http/ftp/磁力/thunder）。filename 为空时用 URL 末段。
  async function push(rawUrl, filename, opts) {
    const url = toHttp(rawUrl);
    if (!url) throw new Error('不是可推送的链接');
    const o = { split: '16', 'max-connection-per-server': '16' };
    if (clean(cfg.dir)) o.dir = clean(cfg.dir);
    if (filename) o.out = filename;
    if (cfg.referer && opts && opts.referer) { o.referer = opts.referer; }
    const extra = parseExtraHeaders();
    if (extra.length) o.header = extra;
    const gid = await rpc('aria2.addUri', [[url], o]);
    return gid;
  }

  function parseExtraHeaders() {
    return String(cfg.headers || '').split('\n').map((l) => {
      const i = l.indexOf(':');
      return i > 0 ? clean(l.slice(0, i)) + ': ' + clean(l.slice(i + 1)) : '';
    }).filter(Boolean);
  }

  // thunder:// → http(s)（base64 段），其余原样；磁力原样。
  function toHttp(u) {
    const s = clean(u);
    if (!s) return '';
    if (/^(https?|ftp|magnet):/i.test(s)) return s;
    const m = /^thunder:\/\/([A-Za-z0-9+/=_-]+)/i.exec(s);
    if (m) {
      try {
        const b = atob(m[1].replace(/-/g, '+').replace(/_/g, '/'));
        const real = /^(https?|ftp):\/\/.+/.exec(b);
        if (real) return real[0];
      } catch (e) {}
    }
    return '';
  }

  /* ---------------- 页面上的动作 ---------------- */

  const extSet = () => new Set(String(cfg.exts || '').toLowerCase().split(',').map((s) => s.trim()).filter(Boolean));

  function isPushed(url) {
    const s = String(url || '');
    if (/^magnet:/i.test(s) || /^thunder:\/\//i.test(s)) return true;
    if (!/^https?:/i.test(s)) return false;
    let path = s;
    try { path = new URL(s, location.href).pathname; } catch (e) {}
    const m = /\.([a-z0-9]{1,8})(?:$|[?#])/i.exec(path);
    return !!m && extSet().has(m[1].toLowerCase());
  }

  function fileNameOf(url) {
    try {
      const u = new URL(url, location.href);
      const last = decodeURIComponent(u.pathname.split('/').filter(Boolean).pop() || '');
      return /\./.test(last) ? last : '';
    } catch (e) { return ''; }
  }

  function collectLinks() {
    const out = [];
    const seen = new Set();
    document.querySelectorAll('a[href]').forEach((a) => {
      const href = a.href || a.getAttribute('href') || '';
      if (!href || seen.has(href) || !isPushed(href)) return;
      seen.add(href);
      out.push({ url: href, name: (a.textContent || '').trim().slice(0, 80) || fileNameOf(href) });
    });
    return out;
  }

  document.addEventListener('click', async (e) => {
    if (!cfg.altClick || !e.altKey || e.button !== 0) return;
    const a = e.target && e.target.closest ? e.target.closest('a[href]') : null;
    const url = a ? a.href : '';
    if (!url || !isPushed(url)) return;
    e.preventDefault(); e.stopPropagation();
    await pushOne(url, fileNameOf(url), url);
  }, true);

  async function pushOne(url, name, referer) {
    toast('正在推送到 aria2…', 'info');
    try {
      const gid = await push(url, name, { referer: referer || location.href });
      toast('已推送（gid ' + String(gid).slice(0, 8) + '）', 'ok');
      if (state.panelOpen) renderTasks();
    } catch (err) {
      toast(String(err.message || err).split('\n')[0], 'err');
      throw err;
    }
  }

  /* ---------------- 界面（shadow DOM，绝不污染宿主页面） ---------------- */

  const state = { root: null, panelEl: null, bodyEl: null, panelOpen: false, tab: 'tasks', timer: null, lastLog: '' };
  const CSS = `
  :host{all:initial}
  .wrap{position:fixed;z-index:2147483647;right:16px;bottom:16px;font:13px/1.5 -apple-system,"PingFang SC",sans-serif;color:#e6edf3}
  .ball{width:44px;height:44px;border-radius:50%;background:#1f6feb;color:#fff;display:flex;align-items:center;justify-content:center;
        box-shadow:0 4px 14px rgba(0,0,0,.35);cursor:grab;user-select:none;font-size:18px}
  .ball.busy{background:#9e6a03}
  .panel{position:absolute;right:0;bottom:56px;width:min(420px,92vw);max-height:70vh;background:#0d1117;border:1px solid #30363d;
         border-radius:10px;box-shadow:0 10px 30px rgba(0,0,0,.5);display:none;flex-direction:column;overflow:hidden}
  .panel.open{display:flex}
  .tabs{display:flex;border-bottom:1px solid #30363d}
  .tab{flex:1;text-align:center;padding:9px 0;cursor:pointer;opacity:.65}
  .tab.active{opacity:1;color:#58a6ff;box-shadow:inset 0 -2px 0 #58a6ff}
  .body{padding:10px;overflow:auto}
  .row{display:flex;gap:6px;align-items:center;margin:6px 0}
  .row label{width:104px;flex:none;opacity:.8}
  input[type=text],input[type=password],textarea{flex:1;min-width:0;background:#010409;border:1px solid #30363d;color:#e6edf3;
         border-radius:6px;padding:5px 7px;font:12px ui-monospace,monospace}
  textarea{height:52px}
  button{background:#21262d;border:1px solid #30363d;color:#e6edf3;border-radius:6px;padding:4px 9px;cursor:pointer;font-size:12px}
  button.p{background:#238636;border-color:#2ea043}
  button:hover{filter:brightness(1.15)}
  .task{border:1px solid #21262d;border-radius:8px;padding:7px 8px;margin:6px 0;background:#010409}
  .task .n{white-space:nowrap;overflow:hidden;text-overflow:ellipsis;font-size:12px}
  .task .m{display:flex;gap:8px;align-items:center;margin-top:4px;font-size:11.5px;opacity:.85}
  .bar{height:4px;background:#21262d;border-radius:3px;overflow:hidden;margin-top:5px}
  .bar i{display:block;height:100%;background:#2ea043}
  .hint{font-size:11.5px;opacity:.7;margin:4px 0}
  .diag{white-space:pre-wrap;font:11.5px ui-monospace,monospace;background:#010409;border:1px solid #21262d;border-radius:6px;
        padding:6px;max-height:120px;overflow:auto}
  .toast{position:fixed;left:50%;transform:translateX(-50%);bottom:24px;background:#161b22;border:1px solid #30363d;color:#e6edf3;
         padding:8px 12px;border-radius:8px;z-index:2147483647;box-shadow:0 6px 18px rgba(0,0,0,.5);max-width:80vw}
  .toast.ok{border-color:#2ea043}.toast.err{border-color:#f85149}
  `;

  function h(tag, cls, text) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text != null) el.textContent = text;
    return el;
  }

  function toast(msg, kind) {
    if (!state.root) buildUI();
    const box = h('div', 'toast ' + (kind === 'ok' ? 'ok' : kind === 'err' ? 'err' : ''), msg);
    state.root.appendChild(box);
    setTimeout(() => box.remove(), kind === 'err' ? 6000 : 2600);
  }

  function buildUI() {
    if (state.root) return;
    const host = document.createElement('div');
    host.style.cssText = 'all:initial';
    const root = host.attachShadow({ mode: 'open' });
    const style = document.createElement('style'); style.textContent = CSS; root.appendChild(style);
    const wrap = h('div', 'wrap');
    const panel = h('div', 'panel');
    const tabs = h('div', 'tabs');
    [['tasks', '任务'], ['settings', '设置']].forEach(([k, label]) => {
      const t = h('div', 'tab' + (state.tab === k ? ' active' : ''), label);
      t.onclick = () => { state.tab = k; panel.querySelectorAll('.tab').forEach((x) => x.classList.remove('active')); t.classList.add('active'); render(); };
      tabs.appendChild(t);
    });
    const body = h('div', 'body');
    const ball = h('div', 'ball', '⬇');
    ball.title = 'aria2 下载助手（点开面板；Alt+点击下载链接直接推送）';
    ball.onclick = () => togglePanel(!state.panelOpen);
    panel.appendChild(tabs); panel.appendChild(body);
    wrap.appendChild(panel); wrap.appendChild(ball);
    root.appendChild(wrap);
    (document.body || document.documentElement).appendChild(host);
    state.root = root; state.panelEl = panel; state.bodyEl = body;

    if (cfg.ballPos) { wrap.style.left = cfg.ballPos.x + 'px'; wrap.style.top = cfg.ballPos.y + 'px'; wrap.style.right = 'auto'; wrap.style.bottom = 'auto'; }
    let drag = null;
    ball.addEventListener('mousedown', (e) => { drag = { x: e.clientX, y: e.clientY, moved: false }; ball.style.cursor = 'grabbing'; });
    window.addEventListener('mousemove', (e) => {
      if (!drag) return;
      if (Math.abs(e.clientX - drag.x) + Math.abs(e.clientY - drag.y) > 4) drag.moved = true;
      if (!drag.moved) return;
      wrap.style.left = Math.max(0, e.clientX - 22) + 'px';
      wrap.style.top = Math.max(0, e.clientY - 22) + 'px';
      wrap.style.right = 'auto'; wrap.style.bottom = 'auto';
    });
    window.addEventListener('mouseup', () => {
      if (!drag) return;
      ball.style.cursor = 'grab';
      if (drag.moved) {
        const r = wrap.getBoundingClientRect();
        cfg.ballPos = { x: Math.round(r.left), y: Math.round(r.top) }; saveCfg();
      }
      drag = null;
    });
    render();
  }

  function togglePanel(open) {
    buildUI();
    state.panelOpen = open;
    state.panelEl.classList.toggle('open', open);
    if (open) { render(); scheduleRefresh(); } else { stopRefresh(); }
  }

  function scheduleRefresh() { stopRefresh(); state.timer = setInterval(() => { if (state.panelOpen && state.tab === 'tasks') renderTasks(); }, 3000); }
  function stopRefresh() { if (state.timer) clearInterval(state.timer); state.timer = null; }

  function render() { if (state.tab === 'settings') renderSettings(); else renderTasks(); }

  async function renderTasks() {
    const b = state.bodyEl;
    b.textContent = '';
    const bar = h('div', 'row');
    const btnAll = h('button', 'p', '推送本页全部');
    btnAll.onclick = pushAll;
    const btnLink = h('button', null, '推送链接…');
    btnLink.onclick = async () => {
      const u = prompt('要推送的链接（http/https/ftp/磁力）：', '');
      if (u) await pushOne(u.trim(), '', location.href).catch(() => {});
    };
    const btnTest = h('button', null, '测试连接');
    btnTest.onclick = testConn;
    bar.appendChild(btnAll); bar.appendChild(btnLink); bar.appendChild(btnTest);
    b.appendChild(bar);
    const stat = h('div', 'hint', '正在读取…');
    b.appendChild(stat);
    const list = h('div', null);
    b.appendChild(list);
    try {
      const g = await rpc('aria2.getGlobalStat', []);
      stat.textContent = '下载 ' + g.numActive + ' · 等待 ' + g.numWaiting + ' · 已停 ' + g.numStoppedTotal +
        ' · ↓' + fmtSpeed(g.downloadSpeed) + ' ↑' + fmtSpeed(g.uploadSpeed);
      const act = await rpc('aria2.tellActive', []);
      const wait = await rpc('aria2.tellWaiting', [0, 20]);
      const items = (act || []).concat(wait || []);
      if (!items.length) { list.appendChild(h('div', 'hint', '当前没有任务。Alt+点击页面上任意下载链接即可推送。')); return; }
      items.forEach((t) => list.appendChild(taskRow(t)));
    } catch (err) {
      stat.textContent = '读不到任务：' + String(err.message || err).split('\n')[0];
      stat.style.color = '#f85149';
    }
  }

  function taskRow(t) {
    const box = h('div', 'task');
    const name = (t.bittorrent && t.bittorrent.info && t.bittorrent.info.name) ||
      (t.files && t.files[0] && t.files[0].path ? t.files[0].path.split('/').pop() : '') || t.gid;
    box.appendChild(h('div', 'n', name));
    const total = Number(t.totalLength || 0), done = Number(t.completedLength || 0);
    const pct = total ? Math.round(done / total * 100) : 0;
    const bar = h('div', 'bar'); const fill = h('i'); fill.style.width = pct + '%'; bar.appendChild(fill);
    box.appendChild(bar);
    const meta = h('div', 'm', pct + '% · ' + fmtSize(done) + '/' + (total ? fmtSize(total) : '?') + ' · ' + fmtSpeed(t.downloadSpeed) +
      (t.errorMessage ? ' · ⛔' + t.errorMessage : ''));
    const row = h('div', 'm');
    const toggle = h('button', null, t.status === 'active' ? '暂停' : '继续');
    toggle.onclick = async () => {
      try { await rpc(t.status === 'active' ? 'aria2.pause' : 'aria2.unpause', [t.gid]); renderTasks(); }
      catch (e) { toast(String(e.message || e).split('\n')[0], 'err'); }
    };
    const del = h('button', null, '删除');
    del.onclick = async () => {
      try {
        await rpc('aria2.forceRemove', [t.gid]);
        await rpc('aria2.removeDownloadResult', [t.gid]).catch(() => {});
        renderTasks();
      } catch (e) { toast(String(e.message || e).split('\n')[0], 'err'); }
    };
    row.appendChild(toggle); row.appendChild(del); row.appendChild(h('span', null, t.status));
    box.appendChild(meta); box.appendChild(row);
    return box;
  }

  function renderSettings() {
    const b = state.bodyEl;
    b.textContent = '';
    const fields = [
      ['rpc', 'RPC 地址', 'text'],
      ['rpcSecret', 'aria2 密钥', 'password'],
      ['dir', '下载目录(可空)', 'text'],
      ['exts', '可推送扩展名', 'text'],
      ['headers', '额外请求头(每行一条)', 'textarea'],
    ];
    const inputs = {};
    fields.forEach(([key, label, type]) => {
      const row = h('div', 'row');
      row.appendChild(h('label', null, label));
      const inp = type === 'textarea' ? h('textarea') : document.createElement('input');
      if (type !== 'textarea') inp.type = type;
      inp.value = cfg[key] || '';
      row.appendChild(inp); inputs[key] = inp; b.appendChild(row);
    });
    const rowAlt = h('div', 'row');
    rowAlt.appendChild(h('label', null, 'Alt+点击拦截'));
    const chk = document.createElement('input'); chk.type = 'checkbox'; chk.checked = !!cfg.altClick;
    rowAlt.appendChild(chk); b.appendChild(rowAlt);
    const rowRef = h('div', 'row');
    rowRef.appendChild(h('label', null, '带上页面 Referer'));
    const chkRef = document.createElement('input'); chkRef.type = 'checkbox'; chkRef.checked = !!cfg.referer;
    rowRef.appendChild(chkRef); b.appendChild(rowRef);

    const btns = h('div', 'row');
    const save = h('button', 'p', '保存');
    save.onclick = () => {
      Object.keys(inputs).forEach((k) => { cfg[k] = inputs[k].value; });
      cfg.altClick = chk.checked; cfg.referer = chkRef.checked;
      saveCfg(); toast('已保存', 'ok');
    };
    const test = h('button', null, '测试连接');
    test.onclick = () => { save.onclick(); testConn(); };
    const aria = h('button', null, '打开 AriaNg');
    aria.onclick = () => window.open(clean(cfg.rpc).replace(/\/jsonrpc\/?$/i, '/'), '_blank', 'noopener');
    btns.appendChild(save); btns.appendChild(test); btns.appendChild(aria);
    b.appendChild(btns);
    b.appendChild(h('div', 'hint', 'aria2 密钥在跑 aria2 的机器上取：`grep rpc-secret ~/aria/aria2.conf`。面板默认不按来源拦，所以不需要额外凭证；若报 403，去「面板设置 → 访问与安全」关掉「aria2 严格模式」。'));
    const diag = h('div', 'diag', state.lastLog || '（点「测试连接」看结果）');
    b.appendChild(diag);
  }

  async function testConn() {
    const log = [];
    try {
      const url = clean(cfg.rpc).replace(/\/jsonrpc\/?$/i, '');
      const v = await rpc('aria2.getVersion', []);
      log.push('✅ 连接正常：aria2 ' + v.version);
      log.push('   RPC ' + url + '/jsonrpc');
      try { const g = await rpc('aria2.getGlobalStat', []); log.push('   活动 ' + g.numActive + ' / 等待 ' + g.numWaiting); } catch (e) {}
    } catch (err) {
      log.push('⛔ ' + String(err.message || err));
      if (/403/.test(String(err.message || err))) log.push('   （403 = 面板开了「aria2 严格模式」：到「面板设置 → 访问与安全」关掉它）');
    }
    state.lastLog = log.join('\n');
    toast(log[0], log[0][0] === '✅' ? 'ok' : 'err');
    if (state.tab === 'settings' && state.panelOpen) renderSettings();
  }

  async function pushAll() {
    const links = collectLinks();
    if (!links.length) return toast('本页没有匹配的下载链接', 'err');
    if (!confirm('推送 ' + links.length + ' 条到 aria2？')) return;
    let ok = 0, fail = 0;
    for (const l of links) {
      try { await push(l.url, fileNameOf(l.url), location.href); ok++; }
      catch (e) { fail++; toast(String(e.message || e).split('\n')[0], 'err'); break; }
    }
    toast('已推送 ' + ok + ' 条' + (fail ? '，第 ' + (ok + 1) + ' 条起失败' : ''), fail ? 'err' : 'ok');
    renderTasks();
  }

  const fmtSize = (n) => {
    n = Number(n) || 0; const u = ['B', 'K', 'M', 'G', 'T']; let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + u[i];
  };
  const fmtSpeed = (n) => fmtSize(n) + '/s';

  /* ---------------- 菜单命令 ---------------- */
  function menu(label, fn) { try { if (typeof GM_registerMenuCommand === 'function') GM_registerMenuCommand(label, fn); } catch (e) {} }
  menu('⬇ aria2 助手面板', () => togglePanel(!state.panelOpen));
  menu('📤 推送本页全部下载链接', () => { buildUI(); pushAll(); });
  menu('⚙️ 设置', () => { buildUI(); togglePanel(true); state.tab = 'settings'; render(); });
  menu('🔍 测试连接', () => { buildUI(); testConn(); });

  // 给油猴控制台/自动化留一个入口，方便自检（不影响正常使用）
  window.__A2H = { cfg, saveCfg, rpc, push, pushOne, testConn, collectLinks, togglePanel, _state: state };
})();
