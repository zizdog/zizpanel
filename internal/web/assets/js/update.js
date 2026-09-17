// update.js —— 「检查更新」页（原「面板设置 → 关于与运维」被整块搬到这里）。
//
// 为什么单独成页：用户 2026-09-21 要求把"在线升级"从设置页的第四个 Tab 提到
// 侧边栏。设置页是**低频表单**，而"有没有新版本"是**随时想知道**的状态；
// 埋在两层点击之后必然没人看。
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

import { api, apiURL } from './api.js';
import { h, clear, toast } from './ui.js';
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

function fmtTime(ts) {
  if (!ts) return '尚未检测';
  try { return new Date(ts).toLocaleString('zh-CN'); } catch { return '—'; }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

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

export function UpdateView(content, ctx = {}) {
  clear(content);

  // 整个页面共用一份状态：横幅、卡片、按钮都读它。
  let info = null;
  let busy = false;
  let dismissTimer = null;
  let pollTimer = null;
  let pollDeadline = 0;
  let checkInfo = null; // 最近一次"检查更新"的结论（has_update / latest）

  const srcInput = h('input.input', {
    placeholder: 'https://example.com/zizpanel/releases（放着 manifest.json 的目录）',
    value: '',
  });
  const notice = h('div');
  const versionTag = h('span#zp-up-ver', { dataset: { testid: 'zp-up-ver' }, style: { color: 'var(--text-mute)', fontSize: '12px' }, text: '读取中…' });
  const bodyEl = h('div.card-body');
  const card = h('div.card', [
    h('div.card-head', [h('h3', { text: '在线升级' }), h('div.spacer'), versionTag]),
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
  function renderNotice() {
    clear(notice);
    if (checkInfo && checkInfo.has_update) {
      const cur = checkInfo.current || state.session?.version || '?';
      notice.append(banner('warn', [
        h('div', { style: { display: 'flex', gap: '9px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('span', { style: { fontSize: '18px' }, text: '🎉' }),
          h('strong', { text: `发现新版本 v${checkInfo.latest}` }),
          h('span', { text: `（当前 v${cur}）` }),
        ]),
        checkInfo.asset_error ? h('div', { style: mutedStyle, text: checkInfo.asset_error }) : null,
        h('div', { style: mutedStyle, text: '最后检测：' + fmtTime(checkInfo.checked_at) + ' · 每 6 小时自动检测一次' }),
        h('div', { style: actionsStyle }, [
          h('button.btn.btn-sm.btn-primary', { text: '一键更新', onclick: () => oneClickUpdate() }),
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

  // ---------- 升级卡片 ----------
  function stopPoll() {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
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
      scheduleSuccessDismiss(fresh);
    } else if (status === 'rolled_back') {
      bodyEl.append(banner('err', [
        h('strong', { text: '升级失败，已自动回滚：' }),
        h('span', { text: st.message || '' }),
        st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null,
        h('div', { style: mutedStyle, text: '面板已恢复到升级前的版本，可以正常使用。请把上面这条原因反馈给开发者。' }),
      ]));
    } else if (status === 'failed') {
      bodyEl.append(banner('err', [
        h('strong', { text: '升级未执行：' }),
        h('span', { text: st.message || '' }),
        st.error ? h('div', { style: mutedStyle, text: '原因：' + st.error }) : null,
      ]));
    }

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
    const isBusy = busy || status === 'applying' || status === 'restarting';
    const btnCheck = h('button.btn', {
      text: '立即检测',
      disabled: isBusy || undefined,
      onclick: () => runCheck(true, btnCheck),
    });

    // 「一键更新」：一次点击走完 stage → apply，中间不需要用户接管。
    const target = data.staged ? data.staged_version : (checkInfo?.has_update ? checkInfo.latest : latestVersion());
    const btnUpdate = h('button.btn.btn-primary', {
      dataset: { testid: 'zp-oneclick-update' },
      text: target ? `一键更新到 v${target}` : '一键更新',
      disabled: isBusy || !data.can_remote || data.can_apply === false || undefined,
      onclick: () => oneClickUpdate(),
    });

    bodyEl.append(h('div', { style: actionsStyle }, [btnCheck, btnUpdate]));

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
      bodyEl.append(banner('ok', [
        h('strong', { text: `v${data.staged_version} 已准备就绪。` }),
        (data.notes || data.state?.notes) ? h('pre.logbox', { text: data.notes || data.state.notes }) : null,
        h('div', { style: actionsStyle }, [
          h('button.btn.btn-primary', {
            text: `一键更新到 v${data.staged_version}`,
            disabled: isBusy || undefined,
            onclick: () => oneClickUpdate(),
          }),
          h('button.btn.btn-sm', {
            text: '放弃这个包',
            onclick: async () => {
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
      render(await api.upgradeStatus());
    } catch (e) {
      clear(bodyEl);
      bodyEl.append(banner('err', [h('span', { text: '读取升级信息失败：' + e.message })]));
      if (showError) toast('读取升级信息失败：' + e.message, 'err', 9000);
    }
  }

  // runCheck：手动「立即检测」走这条；自动检测由下方的 checkUpgrades 负责。
  async function runCheck(manual, btn) {
    if (btn) { btn.disabled = true; btn.textContent = '检测中…'; }
    try {
      checkInfo = await checkUpgrades({ force: true, silent: !manual });
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
      toast(e.message, 'err', 9000);
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = '立即检测'; }
    }
  }

  // 升级期间轮询。面板重启时请求会失败 —— 这是**预期行为**，
  // 所以失败不报错，只继续等，直到面板回来或超时。
  function startPoll() {
    stopPoll();
    pollDeadline = Date.now() + 5 * 60 * 1000;
    const tick = async () => {
      try {
        const data = await api.upgradeStatus();
        render(data);
        const s = data.state?.status;
        if (s !== 'applying' && s !== 'restarting') { stopPoll(); return; }
      } catch {
        // 面板正在重启：保持安静，继续尝试
      }
      if (Date.now() > pollDeadline) { stopPoll(); return; }
      pollTimer = setTimeout(tick, 2000);
    };
    pollTimer = setTimeout(tick, 2000);
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
    busy = true;
    try {
      let staged = !!(info && info.staged);
      let ver = info?.staged_version || '';

      if (!staged) {
        toast('正在下载并校验升级包…', 'info', 6000);
        const res = await api.upgradeStage(srcInput.value.trim());
        if (!res.staged) {
          toast(res.message || '无需升级', 'ok');
          busy = false;
          render(await api.upgradeStatus());
          return;
        }
        staged = true;
        ver = res.version || ver;
      }

      toast(`${ver ? 'v' + ver + ' ' : ''}已就绪，正在升级，面板会短暂重启…`, 'info', 8000);
      render({ ...(info || {}), state: { status: 'applying', message: `正在升级到 v${ver}…` } });
      startPoll();

      // apply 会重启面板：请求本身可能中断，这属于预期，不能当失败处理。
      try { await api.upgradeApply(); } catch { /* 连接被重启切断 */ }

      await watchRestartAndReload();
    } catch (e) {
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
  refresh(false);

  // 打开本页时**总是**检测一次（不受 6 小时周期限制）——用户点进来就是想看结果。
  checkInfo = readSaved();
  renderNotice();
  checkUpgrades({ force: true, silent: true }).then((res) => {
    if (res) { checkInfo = res; renderNotice(); if (res.has_update) refresh(false); }
  }).catch(() => { /* 自动检测失败不打扰 */ });

  if (ctx.onLeave) {
    ctx.onLeave(() => {
      stopPoll();
      clearDismissTimer();
      // 离开页面时清掉这次检测的 in-flight 引用：它已经在模块级去重了，
      // 这里只需保证定时器不泄漏（SSE 式的长连接本页没有）。
    });
  }
}
