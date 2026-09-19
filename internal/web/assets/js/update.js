// update.js —— 「面板设置 → 检查更新」这一个页内 Tab 的内容。
//
// 位置变过两次，最终形态是 2026-09-20 用户定的：
//   原来是「面板设置」的第 4 个 Tab → 提到侧边栏做成独立页「检查更新」
//   → 用户发现"设置里一个入口、侧栏又一个入口"是重复的，要求**收回设置页**，
//     顺序排在「访问与安全 / 文件与终端 / 账号与两步验证」之后（见 views.js 的 tabs）。
// 所以这里导出的 UpdateView 是**页内组件**，由 SettingsView 挂到那个 Tab 里，
// 不再有独立的侧栏入口（旧 hash `#/update`、`#/about`、`#/settings/about`
// 仍会被路由别名落到这个 Tab 上，见 app.js 的 ROUTE_TARGET）。
//
// 这一页除了原来的升级卡片，还负责三件主动的事（同一轮需求）：
//   1. 进面板 / 打开本页时自动 `POST /system/upgrade/check`；
//   2. 每 6 小时周期检测一次（只在页面可见时跑，避免后台标签页白耗流量）；
//   3. 发现新版本时，页面内横幅 + 侧边栏「检查更新」上的小红点，
//      而且**刷新后仍然在**（检测结果落 localStorage，见 updateInfo()）。
//
// 「一键更新」= stage → apply 一口气走完：用户只点一次，中间不等他做决定。
// apply 会重启面板，所以这里不把"请求成功"当成功 —— 而是轮询状态/健康接口，
// 等新版真的起来（看门狗写下 success）再 location.reload()，让浏览器换上新前端。
//
// 这个卡片最特殊的地方（沿用原注释）：升级过程中面板会**重启**，所有请求都会
// 失败一段时间。任何"发起后弹个成功提示就完事"的写法都是错的：那时新版都还没开始跑。
//
// 2026-09-20 用户要求："升级过程要有详细的内容展示！"
// 于是升级期间不再只有一句"升级中"（那样用户只能干等，也不知道卡在哪）：
//   · 阶段条：检查更新 → 下载安装包 → 校验并暂存 → 替换程序 → 重启并验证 → 完成；
//   · 后端 `state.steps` 里记的每一步（时间 + 阶段 + 结论）逐条摊开；
//   · 已用时长每秒刷新，下载/重启期间持续轮询状态（面板重启时的失败是预期，保持安静）。

import { api, apiURL } from './api.js';
import { h, clear, toast, modal } from './ui.js';
import { state } from './app.js';

// ---------------- 主动检测：状态、周期、持久化 ----------------

const LS_KEY = 'zp-upgrade-check'; // {has_update, latest, current, checked_at, effective_source}
const CHECK_INTERVAL = 6 * 60 * 60 * 1000; // 周期检测：6 小时
const BOOT_COOLDOWN = 10 * 60 * 1000;      // 进面板时：距上次检测超过 10 分钟就重测一次
const SCAN_INTERVAL = 5 * 60 * 1000;       // 每 5 分钟看一次"到点没"，只有到点且页面可见才真发请求
const FRESH_SUCCESS_MS = 2 * 60 * 1000;    // 2 分钟内完成的成功态 = "刚刚发生"，才值得弹横幅
const AUTO_DISMISS_MS = 7000;              // 成功横幅自动消失的时间
const RELOAD_DEADLINE_MS = 4 * 60 * 1000;  // 等面板重启回来的上限
const HEALTH_TIMEOUT_MS = 2500;

let started = false;
let scanTimer = null;
let inflight = null;

function readSaved() {
  try { return JSON.parse(localStorage.getItem(LS_KEY) || 'null'); } catch { return null; }
}
function saveSaved(info) {
  // 隐私模式 / 存储被禁用时 localStorage 会抛异常：检测本身不能因此失败。
  try { localStorage.setItem(LS_KEY, JSON.stringify(info)); } catch { /* 本次会话仍然生效 */ }
}
function forgetSaved() {
  try { localStorage.removeItem(LS_KEY); } catch { /* 同上 */ }
}

// updateInfo 返回"当前仍然有效"的检测结果（点侧栏徽标就是读它）。
//
// 关键的一条：面板升上去之后，磁盘里那条 latest 会**等于**当前版本 ——
// 这时候必须把徽标清掉，否则小红点会跟着仓库一起进坟墓。
export function updateInfo() {
  const saved = readSaved();
  if (!saved || !saved.latest) return null;
  const cur = state.session?.version || '';
  if (cur && saved.latest === cur) { forgetSaved(); return null; }
  return saved;
}
export function hasUpdate() {
  const info = updateInfo();
  return !!(info && info.has_update);
}
export function latestVersion() {
  const info = updateInfo();
  return info ? info.latest : '';
}
export function lastCheckedAt() {
  const info = readSaved();
  return info ? (info.checked_at || 0) : 0;
}

function emit() {
  // 侧边栏是每次路由重建的，徽标要能"当场"亮/灭，所以用事件通知 app.js 原地同步。
  try { window.dispatchEvent(new Event('zp:update-state')); } catch { /* 老浏览器 */ }
}

// checkUpgrades 调一次 POST /system/upgrade/check。
//
// source 故意不传：空源时后端会走"配置里的源 → NAS → 公网 → 镜像 → GitHub"
// 候选链，而这里**不能**顺手把用户的 upgrade_source 写空（那是设置页的语义）。
export async function checkUpgrades({ force = false, silent = true } = {}) {
  if (inflight) return inflight;
  const saved = readSaved();
  if (!force && saved && Date.now() - (saved.checked_at || 0) < CHECK_INTERVAL) return saved;

  inflight = api.upgradeCheck()
    .then((res) => {
      const info = {
        has_update: !!res.has_update,
        latest: res.latest || res.current || '',
        current: res.current || state.session?.version || '',
        checked_at: Date.now(),
        effective_source: res.effective_source || res.source || '',
        asset_error: res.asset_error || '',
        // notes 是远端清单里的**外部输入**，原样存起来（渲染时才净化）。
        // 存进 localStorage 是为了刷新页面后，顶部横幅仍能在 status 回来之前
        // 就把"这次更新了什么"显示出来（status 也会带一份，见 api_upgrade.go）。
        notes: typeof res.notes === 'string' ? res.notes : '',
      };
      saveSaved(info);
      emit();
      return info;
    })
    .catch((e) => {
      if (!silent) throw e;
      // 自动检测失败不打扰用户：保留上一次结果，等下一轮。
      return null;
    })
    .finally(() => { inflight = null; });
  return inflight;
}

function scan() {
  // 后台标签页不跑周期检测（用户看不见的时候没必要占带宽）。
  if (document.visibilityState !== 'visible') return;
  const saved = readSaved();
  if (saved && Date.now() - (saved.checked_at || 0) < CHECK_INTERVAL) return;
  checkUpgrades({ force: false, silent: true });
}

// startUpgradeWatcher 在登录后的外壳里调一次（幂等）。
export function startUpgradeWatcher() {
  if (started) return;
  started = true;
  // 进入面板时自动检测一次。用 10 分钟冷却而不是"每次刷新都打"：
  // 频繁刷新不该把发布清单当心跳接口压。
  const saved = readSaved();
  if (!saved || Date.now() - (saved.checked_at || 0) > BOOT_COOLDOWN) {
    checkUpgrades({ force: true, silent: true });
  }
  scanTimer = setInterval(scan, SCAN_INTERVAL);
  window.addEventListener('visibilitychange', scan);
  // 页面从 bfcache 里恢复时也补一次，否则"回到面板"可能还是几天前的结论。
  window.addEventListener('pageshow', scan);
  void scanTimer;
}

// ---------------- 页面 ----------------

const mutedStyle = { color: 'var(--text-mute)', fontSize: '11.5px', marginTop: '6px', lineHeight: '1.6' };
const actionsStyle = { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginTop: '10px' };

// banner 画一条状态条：项目里没有 .notice 这类现成类名，统一用内联样式 + CSS 变量。
function banner(kind, nodes, extra = {}) {
  const bg = { ok: 'var(--ok-soft)', err: 'var(--danger-soft)', warn: 'var(--warn-soft)' }[kind] || 'var(--panel-2)';
  const color = { ok: 'var(--ok)', err: 'var(--danger)', warn: 'var(--warn)' }[kind] || 'var(--text)';
  return h('div', {
    style: Object.assign({
      padding: '11px 13px', background: bg, color, borderRadius: 'var(--radius)',
      fontSize: '12.5px', lineHeight: '1.7', marginBottom: '13px',
    }, extra),
  }, nodes);
}

// 网络失败提示：判据与 internal/services/netfail.go 同源，NET_HINT_MARKER 必须逐字一致。
// 文案纪律：标题一句话 ≤40 字，入口名收进折叠项。apps.js 有一份同样的实现。
const NET_HINT_MARKER = '面板不会替你翻墙';
const NET_HINT_ONE_LINE = '🌐 网络问题：面板不会替你翻墙，请自备代理/梯子后重试';
const NET_HINT_ENTRIES = '面板设置 → 访问与安全 →「应用包镜像基址」；Docker →「加速源」；'
  + '面板设置 →「检查更新」→「升级源地址」。';
const NET_EVIDENCE = [
  'could not resolve host', 'temporary failure in name resolution',
  'name or service not known', 'no such host', 'server misbehaving',
  'connection refused', 'connection timed out', 'connection timeout',
  'connection reset by peer', 'no route to host', 'network is unreachable',
  'network is down', 'host is down', 'i/o timeout', 'operation timed out',
  'failed to connect to', "couldn't connect to server", 'could not connect to server',
  'dial tcp', 'dial udp',
  'tls handshake timeout', 'tls: failed to verify certificate',
  'tls: bad certificate', 'remote error: tls:', 'x509:',
  'certificate signed by unknown authority', 'certificate is not valid for',
  'client.timeout exceeded', 'request canceled while waiting for connection',
  // curl 特定退出码（刻意排除 22：HTTP 错误码，是"镜像上没有文件"不是网络不通）
  'curl: (6)', 'curl: (7)', 'curl: (28)', 'curl: (35)', 'curl: (52)',
  'curl: (56)', 'curl: (60)', 'ssl connect error',
];
const NET_LOCAL_ENDPOINT = ['unix://', 'dial unix', 'docker.sock', '127.0.0.1', 'localhost', '[::1]'];
const NET_LOCAL_OVERRIDE = ['proxyconnect'];

// 只认有真实证据的网络失败，拿不准 false：误报比漏报更糟。
function isNetworkFailureText(text) {
  const msg = String(text == null ? '' : text).toLowerCase();
  if (!msg.trim()) return false;
  if (msg.includes(NET_HINT_MARKER)) return true; // 后端已附提示
  if (NET_LOCAL_OVERRIDE.some((s) => msg.includes(s))) return true; // 走代理失败优先
  if (NET_LOCAL_ENDPOINT.some((s) => msg.includes(s))) return false; // 本地服务没起来 ≠ 要翻墙
  if (NET_EVIDENCE.some((s) => msg.includes(s))) return true;
  if (msg.includes('context deadline exceeded')
    && (msg.includes('http://') || msg.includes('https://'))) return true;
  return false;
}

// networkHintText 是可直接 toast 的一句话（≤40 字）。
function networkHintText() {
  return NET_HINT_ONE_LINE;
}

// networkHintEntries 渲染"改镜像/代理的入口"折叠项（细节收起来，标题只一句话）。
function networkHintEntries() {
  return h('details', { style: { marginTop: '6px' } }, [
    h('summary', { style: { cursor: 'pointer', color: 'var(--text-dim)' }, text: '面板里改镜像/代理的入口' }),
    h('div', { style: { marginTop: '4px', color: 'var(--text-dim)' }, text: NET_HINT_ENTRIES }),
  ]);
}

// networkPanel 是醒目展示块：danger 底 + pill + 一句话 + 折叠入口 + 原文。
function networkPanel(errText) {
  const raw = String(errText == null ? '' : errText).trim();
  const nodes = [
    h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('span.pill.danger', { style: { fontSize: '12px', fontWeight: '700' }, text: '🌐 网络问题' }),
      h('strong', { text: NET_HINT_ONE_LINE }),
    ]),
    networkHintEntries(),
  ];
  if (raw) {
    nodes.push(h('div', {
      style: {
        marginTop: '6px', fontFamily: 'var(--mono)', fontSize: '11.5px',
        color: 'var(--text-dim)', wordBreak: 'break-all',
      },
      text: '原始报错：' + raw,
    }));
  }
  return h('div', {
    dataset: { testid: 'zp-network-failure' },
    style: {
      padding: '11px 13px', background: 'var(--danger-soft)',
      border: '1px solid var(--danger)', borderRadius: 'var(--radius)',
      fontSize: '12.5px', lineHeight: '1.7', marginBottom: '13px',
    },
  }, nodes);
}

function fmtTime(ts) {
  if (!ts) return '尚未检测';
  try { return new Date(ts).toLocaleString('zh-CN'); } catch { return '—'; }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

// ---------------- 更新说明（Markdown 基础渲染） ----------------
//
// 用户 2026-09-24 的要求：「面板在线更新要提示更新内容。」
//
// 说明文字来自远端清单的 `notes`（RELEASE_NOTES.md 原文），是**外部输入** ——
// 发布源一旦被投毒，notes 里放一段 <script> 就能在面板里执行脚本（面板是 root 权限，
// 后果不需要解释）。所以这里的铁律：
//
//   1. **绝不 innerHTML**。所有节点都用 createElement / textContent 逐节点构造，
//      原始 HTML 标签只会变成字面文本（`<script>alert(1)</script>` 显示为一行字）。
//   2. **链接白名单**：只有 `http(s)://` 才生成 <a>，`javascript:` / `data:` / 相对路径
//      一律退回纯文本，连 href 都不碰。
//   3. 属性值只来自我们自己写死的字符串；用户内容永远走 textContent。
//
// 支持的语法（够 RELEASE_NOTES.md 用就行）：#/##/### 标题、-/* 无序列表、
// 1. 有序列表、段落、**粗体**、`行内代码`、[文字](http链接)、``` 代码块。

// notesInline 把一行里的行内标记渲染成一组节点（返回 DocumentFragment）。
function notesInline(text) {
  const frag = document.createDocumentFragment();
  const src = String(text == null ? '' : text);
  // 一条正则一次扫描：粗体 / 行内代码 / 链接。不递归、不解析嵌套，
  // 这样不存在"转义绕过"的路径 —— 每个分支产出的都是纯文本或白名单链接。
  const re = /\*\*([^*]+)\*\*|`([^`]+)`|\[([^\]]*)\]\(([^)\s]*)\)/g;
  let last = 0;
  let m;
  while ((m = re.exec(src)) !== null) {
    if (m.index > last) frag.append(document.createTextNode(src.slice(last, m.index)));
    if (m[1] !== undefined) {
      frag.append(h('strong', { text: m[1] }));
    } else if (m[2] !== undefined) {
      frag.append(h('code', { text: m[2] }));
    } else {
      const label = m[3] || '';
      const url = m[4] || '';
      if (/^https?:\/\//i.test(url)) {
        // 只有明确 http(s) 的链接才成为 <a>。rel/target 写死，防 window.opener 反向控制。
        frag.append(h('a', {
          href: url, target: '_blank', rel: 'noopener noreferrer',
          text: label || url,
        }));
      } else {
        // javascript: / data: / file: 等：**整段原样当文本**，绝不进 href。
        frag.append(document.createTextNode(src.slice(m.index, re.lastIndex)));
      }
    }
    last = re.lastIndex;
  }
  if (last < src.length) frag.append(document.createTextNode(src.slice(last)));
  return frag;
}

function notesIsListStart(line) { return /^\s*[-*]\s+/.test(line); }
function notesIsOrderedStart(line) { return /^\s*\d+[.)]\s+/.test(line); }
function notesIsBlockStart(line) {
  return /^\s*```/.test(line) || /^#{1,3}\s+/.test(line)
    || notesIsListStart(line) || notesIsOrderedStart(line);
}

// renderNotesMarkdown 把说明文本渲染成一个 DOM 子树（绝不 innerHTML）。
function renderNotesMarkdown(md) {
  const root = h('div.zp-notes-md');
  const lines = String(md == null ? '' : md).replace(/\r\n?/g, '\n').split('\n');
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    // ``` 代码块：内容整体进 textContent（Fence 里的标签同样只是文本）。
    if (/^\s*```/.test(line)) {
      const buf = [];
      i += 1;
      while (i < lines.length && !/^\s*```/.test(lines[i])) { buf.push(lines[i]); i += 1; }
      i += 1; // 跳过收尾的 ```
      root.append(h('pre.zp-notes-code', { text: buf.join('\n') }));
      continue;
    }

    // #/##/### 标题（h4/h5/h6，避免抢走页面 <h3> 的层级）。
    const heading = /^(#{1,3})\s+(.*)$/.exec(line);
    if (heading) {
      const tag = 'h' + (heading[1].length + 3);
      root.append(h(tag + '.zp-notes-h', [notesInline(heading[2])]));
      i += 1;
      continue;
    }

    // - / * 无序列表：连续同类行合成一个 <ul>。
    if (notesIsListStart(line)) {
      const ul = h('ul.zp-notes-list');
      while (i < lines.length && notesIsListStart(lines[i])) {
        ul.append(h('li', [notesInline(lines[i].replace(/^\s*[-*]\s+/, ''))]));
        i += 1;
      }
      root.append(ul);
      continue;
    }

    // 1. / 1) 有序列表：连续同类行合成一个 <ol>。
    if (notesIsOrderedStart(line)) {
      const ol = h('ol.zp-notes-list');
      while (i < lines.length && notesIsOrderedStart(lines[i])) {
        ol.append(h('li', [notesInline(lines[i].replace(/^\s*\d+[.)]\s+/, ''))]));
        i += 1;
      }
      root.append(ol);
      continue;
    }

    if (line.trim() === '') { i += 1; continue; }

    // 段落：一直收行到空行 / 下一个块级语法为止，行间按 Markdown 约定用空格连接。
    const buf = [line];
    i += 1;
    while (i < lines.length && lines[i].trim() !== '' && !notesIsBlockStart(lines[i])) {
      buf.push(lines[i]);
      i += 1;
    }
    root.append(h('p.zp-notes-p', [notesInline(buf.join(' '))]));
  }
  return root;
}

// notesSummary 从说明里抽一句纯文本摘要（确认框先给"这次更新了什么"的概览）。
// 纯标题块会被跳过 —— 摘要只显示一句版本号标题等于没给。
function notesSummary(md, max = 200) {
  const raw = String(md == null ? '' : md).trim();
  if (!raw) return '';
  const clean = (s) => s
    .replace(/^#{1,6}\s+/gm, '')
    .replace(/^\s*[-*]\s+/gm, '')
    .replace(/^\s*\d+[.)]\s+/gm, '')
    .replace(/```[\s\S]*?```/g, '')
    .replace(/\*\*([^*]+)\*\*/g, '$1')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\s+/g, ' ')
    .trim();
  let chosen = '';
  for (const block of raw.split(/\n\s*\n/)) {
    const t = block.trim();
    if (!t) continue;
    const lines = t.split('\n');
    // 整块都是标题 → 跳过，继续找真正的正文。
    if (/^#{1,6}\s+/.test(lines[0])
      && lines.every((l) => l.trim() === '' || /^#{1,6}\s+/.test(l))) continue;
    chosen = clean(t);
    if (chosen) break;
  }
  if (!chosen) chosen = clean(raw);
  return chosen.length > max ? chosen.slice(0, max) + '…' : chosen;
}

// notesBlock 是"更新说明"的标准展示块：标题 + 默认展开、可折叠的说明体。
// open=false 时收起，但 summary 文案仍是用户能看懂的一句话。
// 没有说明时**如实写出来**，不留空白、也不编内容。
function notesBlock(notes, { title = '本次更新内容', open = true, testid = 'zp-update-notes', summary } = {}) {
  const raw = typeof notes === 'string' ? notes.trim() : '';
  const box = h('div.zp-notes', { dataset: { testid } });
  if (title) box.append(h('div.zp-notes-title', { text: title }));
  if (!raw) {
    box.append(h('div.zp-notes-empty', {
      dataset: { testid: testid + '-empty' },
      text: '这个版本没有提供更新说明。',
    }));
    return box;
  }
  const details = h('details.zp-notes-details');
  if (open) details.open = true;
  details.append(h('summary.zp-notes-sum', { text: summary || (open ? '收起更新说明' : '展开查看完整更新说明') }));
  details.append(h('div.zp-notes-body', [renderNotesMarkdown(raw)]));
  box.append(details);
  return box;
}

// confirmUpgrade 是「一键更新 / 立即升级」前的确认框。
//
// 用户 2026-09-24 的要求：动手之前要能看到"这个版本更新了什么" ——
// 先给一句**摘要**，再给一个可展开的**全文**，而不是只有一个"确定/取消"。
// notes 同样来自远端清单，走和页面上一样的净化渲染路径。
function confirmUpgrade(version, notes) {
  const raw = typeof notes === 'string' ? notes.trim() : '';
  const full = h('details.zp-notes-details');
  full.append(h('summary.zp-notes-sum', { text: '展开查看完整更新说明' }));
  full.append(h('div.zp-notes-body', [renderNotesMarkdown(raw)]));

  const notesEl = h('div.zp-notes', { dataset: { testid: 'zp-update-notes-confirm' } }, [
    h('div.zp-notes-title', { text: '这个版本更新了什么' }),
    raw
      ? h('div', {}, [
        h('p.zp-notes-p', { text: notesSummary(raw) }),
        full,
      ])
      : h('div.zp-notes-empty', {
        dataset: { testid: 'zp-update-notes-confirm-empty' },
        text: '这个版本没有提供更新说明。',
      }),
  ]);

  return new Promise((resolve) => {
    let done = false;
    const finish = (v) => { if (done) return; done = true; m.close(); resolve(v); };
    const m = modal({
      title: `确认升级到 v${version || '新版本'}`,
      body: h('div.zp-notes-confirm', [
        h('div', {
          style: { fontSize: '13.5px', lineHeight: '1.7', marginBottom: '11px' },
          text: '面板会先下载并校验升级包，确认无误后替换程序并自动重启；重启期间页面短暂断开，请不要关闭本页。',
        }),
        notesEl,
      ]),
      footer: [
        h('button.btn', {
          dataset: { testid: 'zp-update-confirm-cancel' },
          text: '取消',
          onclick: () => finish(false),
        }),
        h('button.btn.btn-primary', {
          dataset: { testid: 'zp-update-confirm-ok' },
          text: '立即升级',
          onclick: () => finish(true),
        }),
      ],
      onClose: () => finish(false),
    });
  });
}

// pingHealth 探一次面板是否已经活过来。升级/重启期间连接会断，失败是预期。
async function pingHealth() {
  try {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), HEALTH_TIMEOUT_MS);
    const res = await fetch(apiURL('health'), {
      cache: 'no-store', credentials: 'same-origin', signal: ctl.signal,
    });
    clearTimeout(timer);
    return res.ok;
  } catch {
    return false;
  }
}

// ---------------- 升级过程的可视化 ----------------
//
// 用户 2026-09-23 的要求（原话意译）：
//   「下载升级包要很久，页面上没有明显的升级进度，用户不知道发生了什么就去刷新重试。
//     要在检查更新页面最显眼的位置实时显示进度，并提示『请勿退出或刷新页面』，
//     用类似 logbox 的样式。」
//
// 所以这里的重点从"摊开 state.steps"升级成"一块真正的进度面板"：
//   · 大号百分比 + 进度条 + 已下载 X / Y + 速度 + 已用时间 + 当前阶段；
//   · 顶部固定一行醒目警示（只在升级进行中出现）；
//   · 等宽字体的 logbox，逐行显示后端写的**真实**阶段与日志，自动滚到底；
//   · 结束（完成/失败）后结果留在页面上，失败原因不会一闪而过。
//
// 数据全部来自后端 `state.progress` / `state.logs`（见 internal/upgrade/progress.go）：
// 字节数来自下载回调的真实写入计数，阶段来自真实的过程切换 —— 前端不编造任何数字。
const UP_PHASES = [
  { id: 'manifest', label: '获取清单' },
  { id: 'download', label: '下载' },
  { id: 'verify', label: '校验' },
  { id: 'extract', label: '解包' },
  { id: 'smoke', label: '试运行' },
  { id: 'apply', label: '替换程序' },
  { id: 'restart', label: '重启并验证' },
];

// upStageIndex 把后端的阶段 id（优先）或状态映射到阶段下标；
// -1 = 没有正在进行的升级，UP_PHASES.length = 全部走完。
function upStageIndex(st) {
  const stage = st && st.progress && st.progress.stage;
  if (stage) {
    const i = UP_PHASES.findIndex((p) => p.id === stage);
    if (i >= 0) return i;
    if (stage === 'done') return UP_PHASES.length;
    if (stage === 'failed') return -1;
  }
  // 老状态没有 progress：按 status 退化推断，保证面板仍然可用。
  switch (st && st.status) {
    case 'checking': return 0;
    case 'downloading': return 1;
    case 'staged': return UP_PHASES.length;
    case 'applying': return 5;
    case 'restarting': return 6;
    case 'success': return UP_PHASES.length;
    default: return -1;
  }
}

function upPhaseBar(st) {
  const cur = upStageIndex(st);
  const bar = h('div', {
    dataset: { testid: 'zp-upgrade-phases' },
    style: { display: 'flex', gap: '6px', flexWrap: 'wrap', margin: '10px 0 0' },
  });
  UP_PHASES.forEach((p, i) => {
    const done = cur > i;
    const active = cur === i;
    bar.append(h('span', {
      style: {
        padding: '2px 8px', borderRadius: '10px', fontSize: '11.5px',
        border: '1px solid ' + (active ? 'var(--brand)' : 'var(--border)'),
        background: active ? 'var(--brand)' : 'transparent',
        color: active ? '#fff' : (done ? 'var(--text)' : 'var(--text-mute)'),
        opacity: done || active ? '1' : '0.55',
      },
      text: (done ? '✓ ' : (active ? '▶ ' : '')) + p.label,
    }));
  });
  return bar;
}

function upElapsedText(startedAt) {
  const started = startedAt ? Date.parse(startedAt) : 0;
  if (!started) return '';
  const secs = Math.max(0, Math.round((Date.now() - started) / 1000));
  if (secs < 60) return `已用 ${secs} 秒`;
  return `已用 ${Math.floor(secs / 60)} 分 ${secs % 60} 秒`;
}

// upFmtBytes 把字节数变成人读形式（与后端 upgradeHumanBytes 同规则）。
function upFmtBytes(n) {
  const v0 = Number(n);
  if (!isFinite(v0) || v0 <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = v0;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
  return (i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
}

function upFmtSpeed(bps) {
  const v = Number(bps);
  if (!isFinite(v) || v <= 0) return '—';
  return upFmtBytes(v) + '/s';
}

// upPct 只在后端**确实**给出了百分比时才显示数字。
// -1 表示"总大小未知"（既没有清单 size 也没有 Content-Length）——
// 这时宁可不显示百分比、画一条滚动的不确定进度条，也不猜一个假数字。
function upPct(st, status) {
  const p = (st && st.progress) || {};
  if (typeof p.percent === 'number' && p.percent >= 0) return Math.min(100, p.percent);
  if (status === 'staged' || status === 'success') return 100;
  return -1;
}

function upStageText(st, status) {
  const p = (st && st.progress) || {};
  if (p.stage_label) return p.stage_label;
  if (status === 'staged') return '已完成，等待应用';
  if (status === 'success') return '完成';
  if (status === 'rolled_back') return '失败（已回滚）';
  if (status === 'failed') return '失败';
  return st.stage || '准备中';
}

// upLogLines 把后端的结构化日志（或老状态的 steps）拍成等宽文本行。
function upLogLines(st) {
  const lines = [];
  if (Array.isArray(st.logs)) {
    for (const l of st.logs) {
      const at = l.at ? String(l.at).replace('T', ' ').slice(11, 19) : '--:--:--';
      lines.push(`${at}  ${l.text || ''}`);
    }
  }
  if (!lines.length && Array.isArray(st.steps)) {
    // 向后兼容：老状态只有 steps（没有 logs）时照旧能显示。
    for (const s of st.steps) {
      const at = s.at ? String(s.at).replace('T', ' ').slice(11, 19) : '--:--:--';
      const mark = s.ok === false ? '✗' : '✓';
      lines.push(`${at}  ${mark} ${s.stage || ''}${s.message ? '：' + s.message : ''}`);
    }
  }
  return lines;
}

function upLogBox(st) {
  const lines = upLogLines(st);
  if (!lines.length) return null;
  const box = h('pre.logbox', {
    dataset: { testid: 'zp-upgrade-log' },
    style: { maxHeight: '240px', marginTop: '10px' },
    text: lines.join('\n'),
  });
  // 追加式日志：永远滚到底部，用户一眼看到最新一步。
  // 用即时赋值而不是 behavior:'smooth' —— 后者对 prefers-reduced-motion 用户是动画。
  queueMicrotask(() => { box.scrollTop = box.scrollHeight; });
  return box;
}

// upProgressPanel 画出"最显眼"的那块升级进度面板。
//
// running：正在升级（顶部出警示条 + 不确定/确定进度条）
// 结束态：顶部换成结果行（含失败原因），下面照样保留日志与阶段。
function upProgressPanel(st, status) {
  const running = status === 'checking' || status === 'downloading'
    || status === 'applying' || status === 'restarting';
  const finished = status === 'staged' || status === 'success'
    || status === 'rolled_back' || status === 'failed';
  const p = st.progress || {};
  const pct = upPct(st, status);
  const unknown = pct < 0;
  const done = status === 'staged' || status === 'success';
  const failed = status === 'rolled_back' || status === 'failed';

  const panel = h('div', {
    class: 'zp-upg-panel' + (running ? ' running' : '') + (finished ? ' finished' : ''),
    dataset: { testid: 'zp-upgrade-panel' },
  });

  // ---- 顶部：警示条（运行中）或结果条（已结束）----
  if (running) {
    panel.append(h('div', {
      class: 'zp-upg-warn',
      dataset: { testid: 'zp-upgrade-warning' },
    }, [
      h('span', { class: 'zp-upg-warn-ico', text: '⚠️' }),
      h('div', {}, [
        h('strong', { text: '升级进行中，请勿退出或刷新页面' }),
        h('div', { class: 'zp-upg-warn-sub', text: '进度由面板实时写入，刷新/关闭会让你看不到进展；面板重启期间页面会自动重连。' }),
      ]),
    ]));
  } else {
    panel.append(h('div', {
      class: 'zp-upg-result ' + (done ? 'ok' : (failed ? 'err' : '')),
      dataset: { testid: 'zp-upgrade-result' },
    }, [
      h('span', { class: 'zp-upg-result-ico', text: done ? '✅' : (failed ? '⛔' : 'ℹ️') }),
      h('div', {}, [
        h('strong', {
          text: done
            ? (status === 'staged' ? `安装包已就绪，可以升级到 v${(st.to || '')}` : '升级完成')
            : (st.message || '升级未完成'),
        }),
        // 失败原因必须留在页面上（用户明确要求"不要一闪而过"）。
        // 网络失败额外挂一颗醒目的 pill + 可照做的入口，别让用户以为功能坏了。
        st.error ? h('div', { class: 'zp-upg-err', dataset: { testid: 'zp-upgrade-error' } }, [
          isNetworkFailureText(st.error)
            ? h('span.pill.danger', { style: { fontWeight: '700', marginRight: '6px' }, text: '🌐 网络问题' })
            : null,
          h('span', { text: '原因：' + st.error }),
          isNetworkFailureText(st.error)
            ? h('div', { style: { marginTop: '4px' } }, [
              h('strong', { text: NET_HINT_MARKER + ' —— 请自备代理/梯子后重试' }),
              h('div', { text: '镜像入口：面板设置 → 访问与安全 →「应用包镜像基址」；在线升级源在「面板设置 → 检查更新」。' }),
            ])
            : null,
        ]) : null,
      ]),
    ]));
  }

  // ---- 主体：大号百分比 + 进度条 + 字节/速度/时间 ----
  const barFill = h('i', unknown ? { class: 'indet' } : { style: { width: Math.max(0, Math.min(100, pct)) + '%' } });
  const total = Number(p.total_bytes) || 0;
  const got = Number(p.downloaded_bytes) || 0;
  const bytesText = total > 0
    ? `已下载 ${upFmtBytes(got)} / ${upFmtBytes(total)}`
    : `已下载 ${upFmtBytes(got)}`;

  panel.append(h('div', { class: 'zp-upg-main' }, [
    h('div', {
      class: 'zp-upg-pct',
      dataset: { testid: 'zp-upgrade-percent' },
      text: unknown ? '…' : Math.round(pct) + '%',
    }),
    h('div', { class: 'zp-upg-detail' }, [
      h('div', { class: 'zp-upg-stage' }, [
        h('span', { class: 'zp-upg-stage-dot' }),
        h('span', { dataset: { testid: 'zp-upgrade-stage-label' }, text: upStageText(st, status) }),
        st.message ? h('span', { class: 'zp-upg-msg', dataset: { testid: 'zp-upgrade-message' }, text: st.message }) : null,
      ]),
      h('div', { class: 'zp-upg-bar' + (unknown ? ' unknown' : '') }, [barFill]),
      h('div', { class: 'zp-upg-stats' }, [
        h('span', { dataset: { testid: 'zp-upgrade-bytes' }, text: bytesText }),
        h('span', { class: 'zp-upg-sep', text: '·' }),
        h('span', { text: '速度 ' + upFmtSpeed(p.bytes_per_second) }),
        h('span', { class: 'zp-upg-sep', text: '·' }),
        h('span', { dataset: { testid: 'zp-upgrade-elapsed' }, text: upElapsedText(st.started_at) }),
      ]),
    ]),
  ]));

  panel.append(upPhaseBar(st));
  const log = upLogBox(st);
  if (log) panel.append(log);
  return panel;
}

export function UpdateView(content, ctx = {}) {
  clear(content);

  // 整个页面共用一份状态：横幅、卡片、按钮都读它。
  let info = null;
  let busy = false;
  let dismissTimer = null;
  let pollTimer = null;
  let pollDeadline = 0;
  let checkInfo = null; // 最近一次"检查更新"的结论（has_update / latest）
  let stageInflight = false; // stage 请求是否还在飞（决定轮询什么时候停）
  let elapsedTimer = null;   // 每秒刷新"已用时长"
  // upRunning：升级是否正在进行。顶部醒目提示读它决定显示"发现新版本"还是
  // "请勿退出或刷新页面"—— 升级途中还在劝用户"一键更新"是自相矛盾的。
  let upRunning = false;
  // sessionResult：本次会话里最后一次**结束**的进度面板快照。
  // 用户明确要求"结束（成功/失败）后结果要留在页面上，不要一闪而过"；
  // 而 success 的绿色横幅仍按既有约定自动消失（历史回归），所以面板自己留一份，
  // 直到用户点「知道了 / 放弃这个包」主动清除。刷新页面不再保留（磁盘状态说了算）。
  let sessionResult = null;
  // watching：是否已经在等面板重启，避免刷新恢复轮询时挂出第二条等待链。
  let watching = false;
  // statusNotes：后端 status 顶层带回来的更新说明（配置里的缓存值）。
  // 它是刷新页面后（check 还没回来时）也能显示说明的兜底来源。
  let statusNotes = '';
  // netFailure：最近一次"检查更新 / 升级"失败的原文，**仅当判定为网络问题时**才存。
  // 存下来是为了在页面最上方常驻一条醒目的网络提示（见 renderNotice）。
  let netFailure = '';

  const srcInput = h('input.input', {
    placeholder: 'https://example.com/zizpanel/releases（放着 manifest.json 的目录）',
    value: '',
  });
  const notice = h('div');
  const versionTag = h('span#zp-up-ver', { dataset: { testid: 'zp-up-ver' }, style: { color: 'var(--text-mute)', fontSize: '12px' }, text: '读取中…' });
  const bodyEl = h('div.card-body');
  // 「检查更新」只留**一颗**按钮，放在卡片标题栏右侧。
  //
  // 为什么（用户 2026-09-20 反馈）：原来卡片正文里并排两颗按钮
  // （「立即检测」+「一键更新到 vX」），而上方"发现新版本"的醒目横幅里**已经有**
  // 一颗「一键更新」—— 同一个动作出现两次，用户不知道该点哪个，只觉得重复。
  // 现在：横幅里那颗负责"升级"（只在真的有新版本时出现），标题栏这颗负责"再检测一次"。
  const btnCheck = h('button.btn.btn-sm', {
    dataset: { testid: 'zp-update-check-btn' },
    text: '检查更新',
    onclick: () => runCheck(true, btnCheck),
  });
  const card = h('div.card', [
    h('div.card-head', [h('h3', { text: '在线升级' }), h('div.spacer'), btnCheck, versionTag]),
    bodyEl,
  ]);

  content.append(
    notice,
    card,
    h('div.card', [
      h('div.card-head', [h('h3', { text: '面板信息' })]),
      h('div.card-body', [
        h('dl.kv', [
          h('dt', { text: '面板版本' }), h('dd', { text: 'v' + (state.session?.version || '-') }),
          h('dt', { text: 'Go 运行时' }), h('dd', { text: state.session?.config?.go_version || '-' }),
          h('dt', { text: '启动时间' }), h('dd', { text: state.session?.config?.started_at || '-' }),
          h('dt', { text: '安装标识' }), h('dd', { text: state.session?.config?.install_id || '-' }),
        ]),
      ]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '常用运维命令' })]),
      h('div.card-body', [
        h('pre.logbox', {
          text: [
            '# 查看状态与访问地址',
            'zizpanel status',
            '',
            '# 忘记密码时重置（在终端执行，无需登录面板）',
            'sudo zizpanel reset-password <用户名>',
            '',
            '# 重启面板',
            'sudo launchctl kickstart -k system/cn.zizpanel.panel',
            '',
            '# 查看面板日志',
            'tail -f /opt/zizpanel/logs/panel-$(date +%Y%m%d).log',
            '',
            '# 重新生成 HTTPS 自签证书',
            'sudo zizpanel gen-cert',
            '',
            '# 卸载面板（保留网站数据）',
            'sudo /opt/zizpanel/uninstall.sh',
          ].join('\n'),
        }),
      ]),
    ]),
  );

  // ---------- 顶部醒目提示：有没有新版本 ----------
  function renderNoticeInner() {
    clear(notice);
    // 升级进行中：顶部不再劝用户"一键更新"（那样自相矛盾），
    // 改成最醒目的"请勿退出或刷新页面"。进度面板里也有一条同样的警示，
    // 这里是页面最顶上的那一份，滚动到任何位置都能看到。
    if (upRunning) {
      const warnTop = banner('warn', [
        h('div', { style: { display: 'flex', gap: '9px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('span', { style: { fontSize: '18px' }, text: '⚠️' }),
          h('strong', { text: '升级进行中，请勿退出或刷新页面' }),
        ]),
        h('div', { style: mutedStyle, text: '下载 / 校验 / 安装都在面板后台继续；刷新虽然不会中断下载，但你会看不到实时进度。' }),
      ], { border: '1px solid var(--warn)' });
      warnTop.dataset.testid = 'zp-upgrade-warning-top';
      notice.append(warnTop);
      return;
    }
    if (checkInfo && checkInfo.has_update) {
      const cur = checkInfo.current || state.session?.version || '?';
      // 说明有两份来源：这次 check 的响应（checkInfo.notes）与后端缓存的
      // 配置值（status 顶层的 notes，落盘后刷新页面也在）。谁有就用谁，
      // 两份都没有时**如实说"没有提供说明"**，不编造。
      const notes = (checkInfo.notes && String(checkInfo.notes).trim())
        || (statusNotes && String(statusNotes).trim()) || '';
      notice.append(banner('warn', [
        h('div', { style: { display: 'flex', gap: '9px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('span', { style: { fontSize: '18px' }, text: '🎉' }),
          h('strong', { text: `发现新版本 v${checkInfo.latest}` }),
          h('span', { text: `（当前 v${cur}）` }),
        ]),
        checkInfo.asset_error ? h('div', { style: mutedStyle, text: checkInfo.asset_error }) : null,
        // 更新内容放在横幅里最显眼的位置：默认展开，可折叠。
        notesBlock(notes, {
          title: `本次更新内容（v${checkInfo.latest}）`,
          open: true,
          testid: 'zp-update-notes-banner',
        }),
        h('div', { style: mutedStyle, text: '最后检测：' + fmtTime(checkInfo.checked_at) + ' · 每 6 小时自动检测一次' }),
        h('div', { style: actionsStyle }, [
          // 升级入口只留这一颗（新版本存在时出现）。testid 沿用原来的
          // zp-oneclick-update —— 自动化测试按它找"一键更新"，换了名字等于偷偷改契约。
          h('button.btn.btn-sm.btn-primary', {
            dataset: { testid: 'zp-oneclick-update' },
            text: `一键更新到 v${checkInfo.latest}`,
            onclick: () => oneClickUpdate(),
          }),
        ]),
      ], { border: '1px solid var(--warn)' }));
    } else if (checkInfo) {
      const cur = checkInfo.current || state.session?.version || '?';
      notice.append(banner('ok', [
        h('span', { text: `已是最新版本 v${cur}。` }),
        h('span', { style: { color: 'var(--text-mute)' }, text: ` 最后检测：${fmtTime(checkInfo.checked_at)}` }),
      ]));
    } else {
      notice.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '13px' } }, [
        h('span.sub', { text: '还没有检测结果。' }),
        h('span.sub', { text: '最后检测：' + fmtTime(lastCheckedAt()) }),
      ]));
    }
  }

  // renderNotice 在"有没有新版本"的基础上，再挂一条**网络失败**提示：
  // 只要最近一次检查更新/升级失败被判定为网络问题，就常驻在页面最上方
  // （醒目 pill + 可照做的入口 + 原始报错），直到下一次成功为止。
  function renderNotice() {
    renderNoticeInner();
    if (netFailure) notice.append(networkPanel(netFailure));
  }

  // ---------- 升级卡片 ----------
  function stopPoll() {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
  }

  function stopElapsedTimer() {
    if (elapsedTimer) { clearInterval(elapsedTimer); elapsedTimer = null; }
  }

  // startElapsedTimer 每秒刷新"已用时长"。为什么不用轮询兼做：轮询 2 秒一次、
  // 且面板重启期间会失败 —— 那时秒表必须继续走（用户最需要知道"已经等了多久"）。
  function startElapsedTimer(startedAt) {
    const tick = () => {
      const text = upElapsedText(startedAt);
      document.querySelectorAll('[data-testid="zp-upgrade-elapsed"]').forEach((el) => {
        el.textContent = text;
      });
    };
    tick();
    elapsedTimer = setInterval(tick, 1000);
  }

  function clearDismissTimer() {
    if (dismissTimer) { clearTimeout(dismissTimer); dismissTimer = null; }
  }

  // 成功横幅不能一直挂着（用户报的 bug：升级成功后那条绿条永不消失）。
  //
  // 真实原因：success 是**看门狗写在磁盘上的终态**，前端每次进页面都把它读出来
  // 重新画一条 banner；只有用户主动点「知道了」才会清除，于是几天后打开面板
  // 还挂着一条"升级成功"。修法分两档：
  //   · 刚刚完成（2 分钟内）→ 显示横幅，7 秒后自动调 dismiss 并重渲染；
  //   · 更早的历史成功态 → 不弹横幅，只留一行灰色说明，并顺手把磁盘终态清掉。
  function scheduleSuccessDismiss(isFresh) {
    clearDismissTimer();
    if (isFresh) {
      dismissTimer = setTimeout(async () => {
        dismissTimer = null;
        try { await api.upgradeDismiss(); } catch { /* 面板可能正在重启 */ }
        try { render(await api.upgradeStatus()); } catch { /* 保持现状 */ }
      }, AUTO_DISMISS_MS);
    } else {
      // 历史成功态：只留一行灰色说明，并顺手把磁盘上的终态清掉 ——
      // 否则下次进页面又会被当成"刚发生的结果"读出来。
      api.upgradeDismiss().catch(() => {});
    }
  }

  function render(next) {
    if (next) info = next;
    const data = info || { state: { status: 'idle' } };
    clear(bodyEl);
    const st = data.state || {};
    const status = st.status || 'idle';

    // 说明的两份来源：status 顶层（配置缓存）与 state 里（若有）。
    const prevStatusNotes = statusNotes;
    statusNotes = (typeof data.notes === 'string' && data.notes)
      || (typeof st.notes === 'string' && st.notes) || '';

    // 每次重渲染先作废旧的成功自动消失定时器；只有 success 会重新排一个。
    clearDismissTimer();

    // ---- 顶部状态条：把"正在发生什么"讲清楚 ----
    if (status === 'applying' || status === 'restarting') {
      bodyEl.append(banner('warn', [
        h('strong', { text: '升级进行中：' }),
        h('span', { text: st.message || st.stage || '正在替换程序并重启面板…' }),
        h('div', { style: mutedStyle, text: '面板即将短暂断开，页面会自动重连；成功后会自动刷新整个网页。请不要关闭这个页面。' }),
      ]));
    } else if (status === 'success') {
      const finished = st.finished_at ? Date.parse(st.finished_at) : 0;
      const fresh = finished && (Date.now() - finished) < FRESH_SUCCESS_MS;
      if (fresh) {
        bodyEl.append(banner('ok', [
          h('strong', { text: '升级成功：' }),
          h('span', { text: `${st.message || ''}（当前 v${st.to || '?'}）` }),
          h('div', { style: mutedStyle, text: `这条提示会在 ${Math.round(AUTO_DISMISS_MS / 1000)} 秒后自动消失。` }),
        ]));
      } else {
        // 历史成功态：绝不再弹横幅（这正是"提示永远不消失"的来源），只留一行。
        bodyEl.append(h('div', {
          style: { color: 'var(--text-mute)', fontSize: '12px', marginBottom: '13px' },
          text: `上次升级：v${st.to || '?'} 成功${st.finished_at ? '（' + st.finished_at.replace('T', ' ').slice(0, 19) + '）' : ''}`,
        }));
      }
      // apply 成功后告诉用户"本次升级包含什么"（说明来自发布清单，可能为空）。
      bodyEl.append(notesBlock(statusNotes, {
        title: `本次升级包含（v${st.to || '?'}）`,
        open: true,
        testid: 'zp-update-notes-success',
      }));
      scheduleSuccessDismiss(fresh);
    } else if (status === 'rolled_back') {
      bodyEl.append(banner('err', [
        h('strong', { text: '升级失败，已自动回滚：' }),
        h('span', { text: st.message || '' }),
        // 网络原因就用**醒目**的网络提示替代那行灰色小字；非网络原因保持原文。
        st.error && isNetworkFailureText(st.error)
          ? networkPanel(st.error)
          : (st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null),
        h('div', { style: mutedStyle, text: '面板已恢复到升级前的版本，可以正常使用。请把上面这条原因反馈给开发者。' }),
      ]));
    } else if (status === 'failed') {
      bodyEl.append(banner('err', [
        h('strong', { text: '升级未执行：' }),
        h('span', { text: st.message || '' }),
        st.error && isNetworkFailureText(st.error)
          ? networkPanel(st.error)
          : (st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null),
      ]));
    }

    // ---- 升级进度面板（最显眼的位置）----
    //
    // 只有在"真的有一次升级过程"时才显示：idle 时挂一块面板会让用户以为在升级。
    // 判据是后端写了结构化进度 / 日志 / 步骤（老状态只有 steps 也能显示）。
    const inProgress = status === 'checking' || status === 'downloading'
      || status === 'applying' || status === 'restarting';
    const ended = status === 'staged' || status === 'success'
      || status === 'rolled_back' || status === 'failed';
    const hasUpgradeTrace = !!(st.progress
      || (Array.isArray(st.logs) && st.logs.length)
      || (Array.isArray(st.steps) && st.steps.length));
    // 顶部提示要在"升级开始/结束"的那一刻跟着切换：
    // 升级中显示"请勿退出或刷新页面"，结束/开始时各自恢复。
    const wasRunning = upRunning;
    upRunning = inProgress;
    // notes 变了也要重画顶部横幅：刷新页面时 check 还没回来、status 先带着
    // 说明到达，这时横幅必须立刻从"没有说明"变成真正的说明。
    if (upRunning !== wasRunning || statusNotes !== prevStatusNotes) renderNotice();

    if (hasUpgradeTrace && ended) {
      // 记下结束态的快照：即使随后绿色横幅按约定自动消失（磁盘状态被清成 idle），
      // 进度面板与日志仍留在页面上，失败原因不会一闪而过。
      sessionResult = { state: st, data };
    }
    let panelState = null;
    if (hasUpgradeTrace && (inProgress || ended)) {
      panelState = st;
    } else if (sessionResult && status === 'idle') {
      panelState = sessionResult.state;
    }
    if (panelState) {
      // 结束态用快照里的 status 渲染（此时 info 可能已经是 idle 了）。
      const panelStatus = (panelState === st) ? status : (panelState.status || 'idle');
      bodyEl.append(upProgressPanel(panelState, panelStatus));
    }
    // 已用时长每秒刷新：只在升级进行中挂计时器，结束就停（避免页面一直空转）。
    stopElapsedTimer();
    if (inProgress && st.started_at) startElapsedTimer(st.started_at);

    // ---- 版本与来源 ----
    bodyEl.append(h('dl.kv', [
      h('dt', { text: '当前版本' }), h('dd', { text: `v${data.current_version || '—'}（${data.arch || '—'}）` }),
      h('dt', { text: '构建时间' }), h('dd', { text: data.build_time || '-' }),
      h('dt', { text: '内嵌发布公钥' }), h('dd', {
        text: data.can_remote ? data.pubkey + '…' : '未配置（无法从网络升级）',
      }),
    ]));

    // 只有后端**明确说**不是 root 才提示：首帧的骨架数据没有 can_apply，
    // 用 `!data.can_apply` 会闪一条假警告。
    if (data.can_apply === false) {
      bodyEl.append(banner('warn', [
        h('strong', { text: '当前面板不是以 root 运行，无法自我升级。' }),
        h('div', { style: mutedStyle, text: '正式安装的面板由 LaunchDaemon 以 root 运行。本地调试实例请改用 install.sh 升级。' }),
      ]));
    }

    // ---- 升级源 ----
    if (!srcInput.value) srcInput.value = data.source || '';
    bodyEl.append(h('div.field', [
      h('label', { text: '升级源地址' }),
      srcInput,
      h('div.hint', {
        text: data.can_remote
          ? '该目录需同时提供 manifest.json 与 manifest.json.sig，面板会用内嵌公钥验签后才会安装。留空则按候选源自动选择。'
          : '面板没有内嵌发布公钥，出于安全考虑会拒绝从网络升级；请使用下面的「上传升级包」。',
      }),
    ]));
    if (data.plain_http) {
      bodyEl.append(banner('warn', [
        h('span', { text: '注意：升级源使用明文 HTTP。签名验证仍然有效（攻击者拿不出私钥），但仍建议改用 HTTPS。' }),
      ]));
    }

    // ---- 操作按钮 ----
    //
    // 这里**不再**放"立即检测 + 一键更新"两颗按钮（用户 2026-09-20 明确要求删掉那一段：
    // 与横幅里的「一键更新」重复）。只剩标题栏那颗「检查更新」；升级入口在
    // 上方横幅（有新版本时）与下面「已就绪」区块（已暂存时），各出现一次。
    const isBusy = busy || status === 'applying' || status === 'restarting'
      || status === 'checking' || status === 'downloading';
    btnCheck.disabled = !!isBusy;
    btnCheck.textContent = (status === 'checking' || status === 'downloading') ? '检测中…' : '检查更新';

    // ---- 上传（离线路径）----
    const fileInput = h('input', { type: 'file', accept: '.tar.gz,.tgz' });
    const btnUpload = h('button.btn', {
      text: '上传升级包',
      disabled: isBusy || undefined,
      onclick: async () => {
        const f = fileInput.files && fileInput.files[0];
        if (!f) { toast('请先选择 .tar.gz 升级包', 'warn'); return; }
        btnUpload.disabled = true;
        btnUpload.textContent = '上传校验中…';
        try {
          const res = await api.upgradeUpload(f);
          toast(`已验证：包内版本 v${res.version}`, 'ok', 8000);
          render(await api.upgradeStatus());
        } catch (e) {
          toast(e.message, 'err', 12000);
        } finally {
          btnUpload.disabled = false;
          btnUpload.textContent = '上传升级包';
        }
      },
    });
    bodyEl.append(h('div.field', [
      h('label', { text: '离线升级（上传发布包）' }),
      h('div', { style: actionsStyle }, [fileInput, btnUpload]),
      h('div.hint', { text: '适合没有外网、或升级源不可达的情况。上传后同样会先解包试运行，确认无误才允许升级。' }),
    ]));

    // ---- 已就绪 → 一键更新 ----
    if (data.staged && status === 'staged') {
      // stage 之后（包已下好、尚未替换程序）：说明同样要展示，
      // 用户在这里做"要不要真的换"的决定。data.notes 优先，退回 statusNotes。
      const stagedNotes = (typeof data.notes === 'string' && data.notes)
        || (data.state && typeof data.state.notes === 'string' && data.state.notes)
        || statusNotes;
      bodyEl.append(banner('ok', [
        h('strong', { text: `v${data.staged_version} 已准备就绪。` }),
        notesBlock(stagedNotes, {
          title: `本次更新内容（v${data.staged_version}）`,
          open: true,
          testid: 'zp-update-notes-staged',
        }),
        h('div', { style: actionsStyle }, [
          h('button.btn.btn-primary', {
            text: `一键更新到 v${data.staged_version}`,
            disabled: isBusy || undefined,
            onclick: () => oneClickUpdate(),
          }),
          h('button.btn.btn-sm', {
            text: '放弃这个包',
            onclick: async () => {
              sessionResult = null;
              try { await api.upgradeDismiss(); toast('已清除', 'ok'); render(await api.upgradeStatus()); }
              catch (e) { toast(e.message, 'err'); }
            },
          }),
        ]),
      ]));
    }

    // ---- 结束态 → 明确关闭（成功态另有自动消失，见 scheduleSuccessDismiss） ----
    if (status === 'success' || status === 'rolled_back' || status === 'failed') {
      bodyEl.append(h('div', { style: actionsStyle }, [
        h('button.btn.btn-sm', {
          text: '知道了（清除提示）',
          onclick: async () => {
            clearDismissTimer();
            sessionResult = null;
            try { await api.upgradeDismiss(); render(await api.upgradeStatus()); }
            catch (e) { toast(e.message, 'err'); }
          },
        }),
      ]));
    }

    versionTag.textContent = `v${data.current_version}`;
  }

  async function refresh(showError) {
    try {
      const data = await api.upgradeStatus();
      render(data);
      return data;
    } catch (e) {
      clear(bodyEl);
      bodyEl.append(banner('err', [h('span', { text: '读取升级信息失败：' + e.message })]));
      if (showError) toast('读取升级信息失败：' + e.message, 'err', 9000);
      return null;
    }
  }

  // runCheck：用户点标题栏「检查更新」走这条；自动检测由下方的 checkUpgrades 负责。
  async function runCheck(manual, btn) {
    if (btn) { btn.disabled = true; btn.textContent = '检测中…'; }
    try {
      checkInfo = await checkUpgrades({ force: true, silent: !manual });
      // 这次真的拿到了结论 → 上一次的网络失败提示该撤掉了（只在 manual 时会抛错，
      // 自动检测失败是静默的，不该覆盖用户刚刚看到的提示）。
      if (checkInfo) netFailure = '';
      if (manual) {
        if (checkInfo?.has_update) {
          toast(`发现新版本 v${checkInfo.latest}`, 'ok');
          if (checkInfo.asset_error) toast(checkInfo.asset_error, 'warn', 9000);
        } else if (checkInfo) {
          toast(`已是最新版本 v${checkInfo.current}`, 'ok');
        }
      }
      renderNotice();
      render();
      if (checkInfo?.has_update) render(await api.upgradeStatus());
    } catch (e) {
      if (isNetworkFailureText(e.message)) {
        // 醒目、且能照做：常驻面板 + 一条能看全的错误 toast。
        netFailure = e.message;
        renderNotice();
        toast(networkHintText() + '\n原始报错：' + e.message, 'err', 20000);
      } else {
        toast(e.message, 'err', 9000);
      }
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = '检查更新'; }
    }
  }

  // 升级期间轮询。面板重启时请求会失败 —— 这是**预期行为**，
  // 所以失败不报错，只继续等，直到面板回来或超时。
  // startPoll：升级期间轮询状态。
  //
  // keepGoing 由调用方给：下载阶段与替换阶段要继续的条件完全不同
  // （下载阶段面板没重启，状态是 checking/downloading；替换阶段会重启，
  // 状态是 applying/restarting）。budgetMs 是这次轮询的总预算 ——
  // 下载可能十几分钟（公网源），不能再用写死的 5 分钟。
  function startPoll(keepGoing, budgetMs) {
    stopPoll();
    pollDeadline = Date.now() + (budgetMs || 6 * 60 * 1000);
    const tick = async () => {
      try {
        const data = await api.upgradeStatus();
        render(data);
        if (keepGoing && !keepGoing(data.state || {})) { stopPoll(); return; }
      } catch {
        // 面板正在重启：保持安静，继续尝试
      }
      if (Date.now() > pollDeadline) { stopPoll(); return; }
      pollTimer = setTimeout(tick, 2000);
    };
    pollTimer = setTimeout(tick, 2000);
  }

  // resumeUpgradeIfRunning：用户在升级途中刷新/重新进入本页时，把进度轮询
  // （以及"等面板重启"）接回去。
  //
  // 这正是用户报的场景 —— "看不到进度就刷新重试"。刷新本身不该中断下载
  // （后端已用 WithoutCancel），页面也必须自己恢复到"正在升级"的显示，
  // 而不是让用户看到一句 idle 以为白等了。
  function resumeUpgradeIfRunning(data) {
    const st = (data && data.state) || {};
    const status = st.status || 'idle';
    if (status === 'checking' || status === 'downloading') {
      startPoll((s) => s.status === 'checking' || s.status === 'downloading', 20 * 60 * 1000);
      return;
    }
    if (status === 'applying' || status === 'restarting') {
      startPoll((s) => s.status === 'applying' || s.status === 'restarting', 10 * 60 * 1000);
      if (!watching) {
        watching = true;
        watchRestartAndReload().finally(() => { watching = false; });
      }
    }
  }

  // oneClickUpdate：stage → apply → 等面板重启 → location.reload()。
  //
  // 这是"一键"的核心：用户不再需要先点「下载并准备升级」、再点「立即升级」。
  // 每个阶段都如实显示；apply 之后请求会断，所以成功与否**只看面板回来后的状态**。
  async function oneClickUpdate() {
    if (busy) return;
    // 非 root 时 stage（下载）能成功但 apply 一定 403 —— 先拦住，别白白下几百 MB。
    if (info && info.can_apply === false) {
      toast('当前面板不是以 root 运行，无法自我升级；请用「上传升级包」或 install.sh', 'warn', 9000);
      return;
    }
    // 动手之前先把"这个版本更新了什么"摆出来（摘要 + 可展开全文）。
    // 取消就什么都不做 —— 确认框是用户唯一能看到说明后再做决定的地方。
    const targetVer = (info && info.staged_version) || (checkInfo && checkInfo.latest) || '';
    const targetNotes = (info && typeof info.notes === 'string' && info.notes)
      || (info && info.state && typeof info.state.notes === 'string' && info.state.notes)
      || (checkInfo && typeof checkInfo.notes === 'string' && checkInfo.notes)
      || statusNotes;
    if (!(await confirmUpgrade(targetVer, targetNotes))) return;
    busy = true;
    try {
      let staged = !!(info && info.staged);
      let ver = info?.staged_version || '';

      if (!staged) {
        toast('正在下载并校验升级包…', 'info', 6000);
        // 下载阶段就开轮询：后端会把"正在获取清单 / 正在下载 / 已暂存"逐步写盘，
        // 用户因此能看到进度与已用时长，而不是盯着一句 toast 干等（用户 2026-09-20 要求）。
        stageInflight = true;
        startPoll((st) => stageInflight || st.status === 'checking' || st.status === 'downloading',
          20 * 60 * 1000);
        render({
          ...(info || {}),
          state: {
            status: 'checking',
            stage: '正在获取发布清单…',
            started_at: new Date().toISOString(),
            // 先摆一个"未知百分比"的进度骨架：点下按钮到第一次轮询之间（约 2 秒）
            // 也得有那块醒目的警示与面板，否则用户会以为按钮没反应。
            progress: { stage: 'manifest', stage_label: '获取发布清单', percent: -1 },
          },
        });
        let res;
        try {
          res = await api.upgradeStage(srcInput.value.trim());
        } finally {
          stageInflight = false;
        }
        if (!res.staged) {
          stopPoll();
          toast(res.message || '无需升级', 'ok');
          busy = false;
          render(await api.upgradeStatus());
          return;
        }
        staged = true;
        ver = res.version || ver;
        // 这一轮真的下到了包 → 网络是通的，撤掉之前那条网络提示。
        netFailure = '';
      }

      toast(`${ver ? 'v' + ver + ' ' : ''}已就绪，正在升级，面板会短暂重启…`, 'info', 8000);
      render({
        ...(info || {}),
        state: {
          status: 'applying',
          stage: `正在升级到 v${ver}…`,
          started_at: new Date().toISOString(),
          progress: { stage: 'apply', stage_label: '替换程序', percent: -1 },
        },
      });
      startPoll((st) => st.status === 'applying' || st.status === 'restarting', 10 * 60 * 1000);

      // apply 会重启面板：请求本身可能中断，这属于预期，不能当失败处理。
      try { await api.upgradeApply(); } catch { /* 连接被重启切断 */ }

      await watchRestartAndReload();
    } catch (e) {
      // 下载/暂存失败：网络原因就把提示摆到页面最上方（醒目 pill），
      // 而不是只弹一条会消失的 toast。
      if (isNetworkFailureText(e.message)) {
        netFailure = e.message;
        renderNotice();
      }
      toast('更新失败：' + e.message, 'err', 12000);
      try { render(await api.upgradeStatus()); } catch { /* 保持现状 */ }
    } finally {
      busy = false;
    }
  }

  // watchRestartAndReload 等新版真的站起来：轮询状态 → health 200 → 整页刷新。
  async function watchRestartAndReload() {
    const startedAt = Date.now();
    while (Date.now() - startedAt < RELOAD_DEADLINE_MS) {
      await sleep(2000);
      let data = null;
      try { data = await api.upgradeStatus(); } catch { /* 面板正在重启 */ }
      if (data) {
        const st = data.state || {};
        if (st.status === 'success' && await pingHealth()) {
          stopPoll();
          clearDismissTimer();
          toast('升级完成，正在刷新页面…', 'ok', 6000);
          location.reload();
          return;
        }
        if (st.status === 'rolled_back' || st.status === 'failed') {
          stopPoll();
          render(data);
          toast('升级没有成功，详见页面提示', 'err', 12000);
          return;
        }
      }
    }
    stopPoll();
    toast('等待面板重启超时：请手动刷新本页查看结果', 'err', 0);
    try { render(await api.upgradeStatus()); } catch { /* 面板仍不可达 */ }
  }

  // ---------- 挂载 ----------
  // 先给一个"读取中"占位，等真状态回来再画卡片：用假骨架数据渲染会闪一条
  // "vundefined" 和一条假的能力警告（首帧没有 can_apply / current_version）。
  renderNotice();
  bodyEl.append(h('div.empty', { text: '读取升级信息中…' }));
  // 读一次真状态；如果后端说"正在升级"（用户刷新过页面），就把轮询接回去。
  refresh(false).then((data) => { resumeUpgradeIfRunning(data); });

  // 打开本页时**总是**检测一次（不受 6 小时周期限制）——用户点进来就是想看结果。
  checkInfo = readSaved();
  renderNotice();
  checkUpgrades({ force: true, silent: true }).then((res) => {
    if (res) { checkInfo = res; renderNotice(); if (res.has_update) refresh(false); }
  }).catch(() => { /* 自动检测失败不打扰 */ });

  if (ctx.onLeave) {
    ctx.onLeave(() => {
      stopPoll();
      stopElapsedTimer(); // 秒表也要停：升级中的页面被切走时它没有任何可见去处
      clearDismissTimer();
      // 离开页面时清掉这次检测的 in-flight 引用：它已经在模块级去重了，
      // 这里只需保证定时器不泄漏（SSE 式的长连接本页没有）。
    });
  }
}
