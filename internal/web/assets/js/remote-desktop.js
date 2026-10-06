// remote-desktop.js —— 「远程桌面」：面板里直接看/操作本机桌面（内嵌 noVNC）。
//
// 为什么要它（用户 2026-10-06）：面板跑在无头 Mac 上，macOS 的 GUI 授权弹窗只能在
// **控制台会话**上点。现在面板自己当 VNC 中继（浏览器 ←WS→ 面板 ←TCP→ 127.0.0.1:5900），
// 所以只要面板入口可达就行，**不需要把 5900 暴露出去**。
//
// 保活（用户明确要求：和「文件管理 / Web 终端」一样**切页不刷新、不断线**）：
//   · RFB 连接、canvas、状态都是**模块级单例**；
//   · 每次进这一页只把**同一批持久 DOM 节点**移动进新视图（move 不是重建），
//     所以画面、会话语义、滚动位置都原样保留；
//   · onLeave 里**刻意不断开**连接。
//
// 认证：不用 macOS 的"VNC 旧版密码"（那会把机器认证降级成一个可爆破的 8 位密码），
// 而是走 Apple 的 ARD 认证（安全类型 30，noVNC 原生支持）—— 直接用 **Mac 账号 + 密码**，
// 面板既不存密码、也不改系统认证设置。

import { api, apiURL } from './api.js';
import { h, clear, toast } from './ui.js';
import RFB from '../novnc/core/rfb.js';

// ---------- 模块级单例（保活的关键） ----------
let session = null;      // { rfb, canvas, host }
let dom = null;          // 持久 DOM
let status = null;       // 最近一次读到的后端状态
let pendingCreds = null; // 上次输入过的用户名（只记用户名，不记密码）

// ensureDom 只构造一次持久节点。
function ensureDom() {
  if (dom) return dom;
  // 两行**刻意分开**：上面一行是"屏幕共享服务状态"（每次进页面从后端读），
  // 下面一行是"连接状态"（由 RFB 事件写）。合成一行会出现"回到这页时
  // 刚显示的「已连接」被状态刷新覆盖掉"（2026-10-06 实测）。
  const info = h('div.hint', { dataset: { testid: 'zp-rd-status' }, text: '正在读取屏幕共享状态…' });
  const state = h('div', {
    dataset: { testid: 'zp-rd-state' }, text: '未连接',
    style: { marginTop: '6px', fontSize: '13px' },
  });
  const connectBtn = h('button.btn.btn-primary', {
    text: '连接', dataset: { testid: 'zp-rd-connect' },
    title: '用这台 Mac 的账号密码登录（ARD 认证，密码不经过面板存储）',
    onclick: () => openCreds(false),
  });
  const disconnectBtn = h('button.btn', {
    text: '断开', dataset: { testid: 'zp-rd-disconnect' },
    onclick: () => { teardown(true); toast('已断开远程桌面', 'ok'); renderButtons(); },
  });
  const creds = h('div', { dataset: { testid: 'zp-rd-creds' }, style: { display: 'none', marginTop: '10px' } });
  const canvas = h('canvas', {
    dataset: { testid: 'zp-rd-canvas' },
    style: { width: '100%', height: '100%', display: 'block', background: '#000' },
  });
  const stage = h('div', {
    dataset: { testid: 'zp-rd-stage' },
    style: {
      position: 'relative', width: '100%', height: 'min(66vh, 720px)', minHeight: '320px',
      background: '#000', borderRadius: '8px', overflow: 'hidden', display: 'none',
    },
  }, [canvas]);
  dom = { info, state, connectBtn, disconnectBtn, creds, canvas, stage };
  return dom;
}

function renderButtons() {
  if (!dom) return;
  const live = !!(session && session.rfb);
  dom.connectBtn.disabled = live;
  dom.disconnectBtn.disabled = !live;
  dom.creds.style.display = 'none';
}

// openCreds 显示"用户名 + 密码"输入（用户名默认沿用上次 / 面板账号）。
function openCreds(reconnect) {
  const d = ensureDom();
  clear(d.creds);
  const userI = h('input.input', {
    type: 'text', placeholder: 'Mac 用户名', value: pendingCreds || '', dataset: { testid: 'zp-rd-user' },
  });
  const passI = h('input.input', {
    type: 'password', placeholder: 'Mac 登录密码', dataset: { testid: 'zp-rd-pass' },
  });
  const go = h('button.btn.btn-primary', {
    text: reconnect ? '重新连接' : '登录并连接', dataset: { testid: 'zp-rd-login' },
    onclick: () => {
      const username = userI.value.trim();
      const password = passI.value;
      if (!username || !password) { toast('用户名和密码都要填', 'err'); return; }
      pendingCreds = username;
      if (session && session.rfb) {
        // 已经有连接：把凭据交回给 noVNC（它自己重试认证）。
        session.rfb.sendCredentials({ username, password });
        passI.value = '';
        return;
      }
      connect(username, password);
      passI.value = '';
    },
  });
  d.creds.append(
    h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [userI, passI, go]),
    h('div.hint', { style: { marginTop: '6px' }, text: '用那台 Mac 的账号与登录密码（与面板账号无关）；密码只在浏览器到屏幕共享之间用，面板不保存。' }),
  );
  d.creds.style.display = '';
  passI.focus();
}

// ---------- 连接 / 断开 ----------

function connect(username, password) {
  const d = ensureDom();
  teardown(false);
  // 先把画面区显示出来再创建 RFB：noVNC 连接时就会量容器尺寸，
  // 容器还在 display:none（0×0）时它算出的缩放是坏的（2026-10-06 实测：canvas 停在 300×150 不渲染）。
  d.stage.style.display = '';
  const wsURL = new URL(apiURL('system/remote-desktop/ws'), document.baseURI);
  wsURL.protocol = wsURL.protocol === 'https:' ? 'wss:' : 'ws:';
  let rfb;
  try {
    // 刻意**不传** wsProtocols：面板的 WebSocket 握手不协商子协议，
    // 浏览器如果请求了 'binary' 而服务端没回同名的 Sec-WebSocket-Protocol，连接会被浏览器判失败
    //（2026-10-06 端到端实测踩到：连接一闪就断）。
    rfb = new RFB(d.canvas, wsURL.toString(), { credentials: { username, password }, shared: true });
  } catch (e) {
    toast('创建远程桌面连接失败：' + ((e && e.message) || e), 'err', 15000);
    return;
  }
  rfb.scaleViewport = true;
  rfb.resizeSession = false;
  rfb.viewOnly = false;
  session = { rfb, canvas: d.canvas, host: '' };
  d.state.textContent = '正在连接…';

  rfb.addEventListener('connect', () => {
    d.state.textContent = '已连接：' + (session && session.host ? session.host : '本机桌面');
    renderButtons();
  });
  rfb.addEventListener('desktopname', (ev) => {
    if (session) session.host = (ev.detail && ev.detail.name) || '';
    d.state.textContent = '已连接：' + (session.host || '本机桌面');
  });
  rfb.addEventListener('credentialsrequired', () => {
    d.state.textContent = '需要登录这台 Mac（输入账号密码后继续）';
    openCreds(true);
  });
  rfb.addEventListener('securityfailure', (ev) => {
    const reason = (ev.detail && ev.detail.reason) || '认证失败';
    d.state.textContent = '登录失败：' + reason;
    toast('远程桌面登录失败：' + reason, 'err', 15000);
    openCreds(true);
  });
  rfb.addEventListener('disconnect', (ev) => {
    const clean = ev.detail && ev.detail.clean;
    teardown(false);
    d.state.textContent = clean ? '已断开（可重新连接）' : '连接已断开 —— 屏幕共享没在跑，或面板重启过；点「连接」重试';
    renderButtons();
  });
  renderButtons();
}

// teardown 关掉当前连接（noVNC 的 disconnect() 会自己发关闭帧；失败也当断开了，
// 因为 session 已经清空，界面不会留下一个"看起来还连着"的假象）。
function teardown(_sendClose) {
  if (!session) return;
  const { rfb } = session;
  session = null;
  try { rfb.disconnect(); } catch { /* 忽略 */ }
}

// ---------- 后端状态 / 开关 ----------

async function refreshStatus() {
  const d = ensureDom();
  try {
    status = await api.get(apiURL('system/remote-desktop'));
  } catch (e) {
    d.info.textContent = '读不到屏幕共享状态：' + ((e && e.message) || e);
    return;
  }
  const parts = [];
  parts.push(status.listening ? `屏幕共享：正在监听 ${status.port}` : `屏幕共享：未运行（${status.port} 没人听）`);
  if (status.enabled_known) parts.push(status.enabled ? '（开机自启已开）' : '（开机自启未开）');
  if (status.auth_types && status.auth_types.length) parts.push('认证方式 ' + status.auth_types.join('/'));
  if (status.check_error) parts.push('⚠️ ' + status.check_error);
  const acc = status.access || {};
  if (acc.verified) {
    parts.push(acc.exists
      ? `允许访问：仅 ${acc.group} 的成员` + (acc.user ? (acc.user_member ? '（当前用户已在组里）' : '（当前用户不在组里）') : '')
      : '允许访问：未设服务 ACL（本机任意用户都能连）');
  } else {
    parts.push('允许访问：未复核' + (acc.error ? '（' + acc.error + '）' : ''));
  }
  d.info.textContent = parts.join(' · ');
}

async function action(name) {
  try {
    status = await api.post(apiURL('system/remote-desktop/' + name), {});
    toast(name === 'enable' ? '已开启屏幕共享' : '已关闭屏幕共享', 'ok');
  } catch (e) {
    toast((name === 'enable' ? '开启失败：' : '关闭失败：') + ((e && e.message) || e), 'err', 20000);
  }
  await refreshStatus();
}

// ---------- 视图 ----------

export function RemoteDesktopView(content, ctx = {}) {
  clear(content);
  const d = ensureDom();

  const enableBtn = h('button.btn.btn-sm', {
    text: '开启屏幕共享', dataset: { testid: 'zp-rd-enable' },
    onclick: () => action('enable'),
  });
  const disableBtn = h('button.btn.btn-sm', {
    text: '关闭屏幕共享', dataset: { testid: 'zp-rd-disable' },
    onclick: () => action('disable'),
  });
  const refreshBtn = h('button.btn.btn-sm', {
    text: '⟳ 刷新状态', dataset: { testid: 'zp-rd-refresh' }, onclick: () => refreshStatus(),
  });

  content.append(
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '远程桌面' }),
        h('div.spacer'),
        enableBtn, disableBtn, refreshBtn, d.connectBtn, d.disconnectBtn,
      ]),
      h('div.card-body', [
        h('p.hint', {
          text: '在面板里直接操作这台 Mac 的桌面（GUI 授权弹窗、只能用界面的软件都在这里点）。'
            + '连接走面板自己的入口，不需要把 5900 暴露到公网。',
        }),
        d.info,
        d.state,
        d.creds,
        h('div', { style: { marginTop: '10px' } }, [d.stage]),
      ]),
    ]),
  );

  // 保活：切页**不**断开、不销毁 canvas；只摘掉本次挂载加的监听。
  if (ctx.onLeave) ctx.onLeave(() => { /* 刻意什么都不做：这就是保活的关键 */ });

  renderButtons();
  refreshStatus();
  if (session && session.rfb) d.stage.style.display = '';
}

// resetSessionForTest 只给自动化测试用：把模块级单例清干净（不影响生产路径）。
export function __resetRemoteDesktopForTest() {
  teardown(false);
  dom = null;
  status = null;
}
