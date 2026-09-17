// docker-mirrors.js —— Docker 镜像加速源（换源）。
//
// 用户的原话是"现在的拉取太慢了，没法用"。这台机器上 registry-1.docker.io
// 直连会超时，所以界面第一件事就是把**官方源现在到底通不通**摆出来，
// 而不是让用户继续怀疑是自己网络的问题。
//
// 交互（2026-09-17 按用户反馈重做："点击后要等很久，应该立即加载，
// 加一个点击检测的按钮，让用户主动选择操作触发源的检测"）：
//   1. **进入页面不再自动探测**。探测 14 个源最多要 8 秒，过去点进来就是干等。
//      现在页面用**上次检测的缓存**（后端只读接口）立即渲染；没缓存就如实显示
//      "尚未检测"，绝不假装已检测。
//   2. 探测只由用户点「检测可用性」触发：按钮进入 loading/禁用态，文案带
//      "已用 Ns"；完成后把结果填回每个源，并显示"上次检测：HH:MM:SS"，
//      让用户知道数据有多旧。
//   3. 缓存由后端持有（带时间戳的内存缓存，面板重启后回到"尚未检测"）。
//
// 其它两条设计（保留）：
//   · 判定不看"能不能连上"：registry 的 /v2/ 返回 401 才是正常的；
//   · 保存会重启 Docker 运行时（约 30-60 秒，容器会短暂中断），
//     所以走任务中心，进度里能看到 colima restart 与一次真实拉取测试。

import { api } from './api.js';
import { h, clear, toast, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

// 官方源单独特判（它超时是最常见的情况，要说清）
const HUB_URL = 'https://registry-1.docker.io';

// 进程内兜底缓存：后端有权威缓存（GET .../mirrors/cached）。
// 这里留一份是为了"缓存接口偶发失败"时不至于把刚测到的结果丢掉。
let probeCache = null; // url → probe
let probeCheckedAt = null; // RFC3339 字符串（检测时间）

// fmtCheckedAt 把后端给的检测时间格式化成"HH:MM:SS"（不是今天则带上月-日）。
// 返回 null 表示"没有检测时间" —— 调用方据此显示"尚未检测"。
function fmtCheckedAt(iso) {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  const pad = (n) => String(n).padStart(2, '0');
  const hms = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  const now = new Date();
  return d.toDateString() === now.toDateString() ? hms : `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hms}`;
}

function fmtSeconds(ms) {
  return `${(ms / 1000).toFixed(1)}s`;
}

export async function renderMirrors(pane, ctx) {
  clear(pane);
  appendAll(pane, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取加速源配置…' })]));

  // 两个请求并行：现状（很快，不探测）与"上次检测"的缓存（只读）。
  // 缓存接口失败不致命 —— 如实显示"尚未检测"即可，不要因此整页报错。
  let data;
  let cached = null;
  try {
    [data, cached] = await Promise.all([
      api.dockerMirrors(),
      api.dockerMirrorCached().catch(() => null),
    ]);
  } catch (e) {
    clear(pane);
    appendAll(pane, h('div.empty', [
      h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
    ]));
    return;
  }
  if (cached && cached.cached) {
    // 后端缓存是权威的：它一定不比前端手里的旧（检测接口写缓存后才回响应）
    probeCache = {};
    (cached.probes || []).forEach((p) => { probeCache[p.url] = p; });
    probeCheckedAt = cached.checked_at || null;
  }
  const st = data.state || {};
  const candidates = data.candidates || [];

  // 已选中的集合：以配置文件里的为准（生效中的可能是重启前的旧值）
  let selected = new Set((st.configured || []).map((u) => u.replace(/\/$/, '')));

  const hubBox = h('div');
  const candBox = h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } });
  const statusBox = h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } });
  const checkInfo = h('span'); // "上次检测：HH:MM:SS" / "尚未检测"
  const customInput = h('input.input', { placeholder: '自定义加速源地址，如 https://docker.m.daocloud.io', style: { flex: '1 1 260px' } });

  const probeBtn = h('button.btn.btn-sm', { text: '🔍 检测可用性', onclick: () => doProbe() });
  const saveBtn = h('button.btn.btn-sm.btn-primary', {
    text: '保存并重启 Docker',
    onclick: () => doSave(),
  });

  // renderCheckInfo 显示"这批数据有多旧"（用户要求的检测时间）。
  function renderCheckInfo() {
    clear(checkInfo);
    const t = fmtCheckedAt(probeCheckedAt);
    if (t === null) {
      checkInfo.append(h('span.sub', { text: '尚未检测 —— 点「检测可用性」用你这台机器的网络实测每个源' }));
    } else {
      checkInfo.append(h('span.pill', { text: '上次检测：' + t, title: probeCheckedAt }));
    }
  }

  function syncProbeLabels() {
    // 把探测结果贴到候选行上（有结果才贴；没 result 的行保持"未检测"）
    candBox.querySelectorAll('[data-mirror]').forEach((row) => {
      const url = row.dataset.mirror;
      const slot = row.querySelector('.probe-slot');
      if (!slot) return;
      clear(slot);
      const p = probeCache ? probeCache[url] : null;
      if (!p) {
        slot.append(h('span.pill', { text: '未检测' }));
        return;
      }
      const cls = p.ok ? '.ok' : '.warn';
      const ms = p.latency_ms ? `${p.latency_ms}ms` : '—';
      slot.append(h('span.pill' + cls, { text: p.ok ? `可用 ${ms}` : '不可用', title: p.detail || '' }));
    });
    syncHubBox();
  }

  // syncHubBox 渲染官方源结论：**没测过就如实说没测过**，
  // 不能把"尚未检测"显示成"连不上"（那会让用户白折腾自己的网络）。
  function syncHubBox() {
    clear(hubBox);
    const hub = probeCache ? probeCache[HUB_URL] : null;
    if (!hub) {
      hubBox.append(h('div', {
        style: {
          padding: '10px 12px', borderRadius: 'var(--radius)',
          border: '1px solid var(--border)', background: 'var(--bg-soft, transparent)',
          display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap',
        },
      }, [
        h('span', { text: '❔' }),
        h('div', { style: { flex: 1, minWidth: '200px' } }, [
          h('div', { style: { fontWeight: '620' }, text: 'Docker Hub 官方源（registry-1.docker.io）' }),
          h('div', { style: { fontSize: '12px', opacity: 0.85 }, text: '尚未检测。点上面的「检测可用性」会用你自己这台机器的网络实测一次（最多约 8 秒）。' }),
        ]),
      ]));
      return;
    }
    hubBox.append(h('div', {
      style: {
        padding: '10px 12px', borderRadius: 'var(--radius)',
        border: '1px solid ' + (hub.ok ? 'var(--ok)' : 'var(--danger)'),
        background: hub.ok ? 'var(--ok-soft)' : 'var(--danger-soft)',
        display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap',
      },
    }, [
      h('span', { text: hub.ok ? '✅' : '⛔️' }),
      h('div', { style: { flex: 1, minWidth: '200px' } }, [
        h('div', { style: { fontWeight: '620' }, text: 'Docker Hub 官方源（registry-1.docker.io）' }),
        h('div', { style: { fontSize: '12px', opacity: 0.85 }, text: hub.ok
          ? `上次检测时能连上（${hub.latency_ms}ms）。加速源仍可提升拉取速度。`
          : `上次检测时连不上（${hub.detail}）—— 这就是"拉取太慢/拉不动"的直接原因，必须配加速源。` }),
      ]),
    ]));
  }

  async function doProbe() {
    const t0 = Date.now();
    probeBtn.disabled = true;
    const tick = () => {
      probeBtn.textContent = `检测中…（已用 ${Math.round((Date.now() - t0) / 1000)}s）`;
    };
    tick();
    const timer = setInterval(tick, 1000);
    // 切页/离开时必须收掉计时器，否则它会一直跑（哪怕是往已经脱离文档的按钮上写字）
    if (ctx && typeof ctx.trackCleanup === 'function') {
      ctx.trackCleanup(() => clearInterval(timer));
    }
    try {
      const res = await api.dockerMirrorProbe();
      probeCache = {};
      (res.probes || []).forEach((p) => { probeCache[p.url] = p; });
      probeCheckedAt = res.checked_at || new Date().toISOString();
      // 后端是"一次性返回整批"，所以这里整批回填；总耗时如实写在提示里。
      syncProbeLabels();
      renderCheckInfo();
      const elapsed = fmtSeconds(Date.now() - t0);
      const okCount = (res.probes || []).filter((p) => p.ok && p.url !== HUB_URL).length;
      toast(`检测完成（用时 ${elapsed}）：${okCount} 个加速源可用`, okCount > 0 ? 'ok' : 'warn', 9000);
    } catch (e) {
      // 失败**不清空**旧结果：上次的检测时间仍然是有用的信息，别把它抹掉
      toast('检测失败：' + e.message, 'err', 9000);
    } finally {
      clearInterval(timer);
      probeBtn.disabled = false;
      probeBtn.textContent = '🔍 检测可用性';
    }
  }

  async function doSave() {
    const mirrors = Array.from(selected);
    taskCenter.start({
      kind: 'docker-mirror',
      target: 'docker-mirrors',
      title: '更换 Docker 镜像加速源',
      start: () => api.dockerMirrorSave(mirrors),
      onDone: () => renderMirrors(pane, ctx),
    });
  }

  function candidateRow(c) {
    const box = h('input', { type: 'checkbox', checked: selected.has(c.url) });
    box.addEventListener('change', () => {
      if (box.checked) selected.add(c.url);
      else selected.delete(c.url);
    });
    return h('div', {
      'data-mirror': c.url,
      style: {
        display: 'flex', gap: '10px', alignItems: 'flex-start',
        border: '1px solid var(--border)', borderRadius: 'var(--radius)', padding: '10px 12px',
      },
    }, [
      h('div', { style: { paddingTop: '2px' } }, [box]),
      h('div', { style: { flex: 1, minWidth: 0 } }, [
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
          h('span', { style: { fontWeight: '620' }, text: c.name || c.url }),
          h('span.pill', { text: c.url.replace(/^https?:\/\//, '') }),
        ]),
        c.note ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '4px' }, text: c.note }) : null,
      ]),
      h('div.probe-slot', { style: { flex: '0 0 auto' } }, [h('span.pill', { text: '未检测' })]),
    ]);
  }

  appendAll(pane,
    hubBox,
    h('div.hint', {
      style: { marginTop: '10px' },
      text: '加速源只影响 Docker Hub 的镜像（docker.io / library/*）；ghcr.io 与私有仓库不走这里。' +
        '建议只选 1-3 个。顺序有意义：docker 只用**第一个能应答**的源，' +
        '所以保存时面板会先实测一遍，按延迟从小到大写进去（可用性优先）。',
    }),
    h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', margin: '12px 0' } }, [
      probeBtn, saveBtn, checkInfo,
    ]),
    h('div.hint', { text: '保存会重启 Docker 运行时（colima restart，约 30-60 秒），期间容器会短暂中断。' }),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '当前状态' }), h('div.spacer'), h('span.sub', { text: st.runtime || '' })]),
      h('div.card-body', [statusBox]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '候选加速源（内置）' })]),
      h('div.card-body', [candBox]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '自定义' })]),
      h('div.card-body', [
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
          customInput,
          h('button.btn.btn-sm', {
            text: '加入列表',
            onclick: () => {
              const v = customInput.value.trim().replace(/\/$/, '');
              if (!v) return;
              if (!/^https?:\/\//.test(v)) { toast('地址必须以 http:// 或 https:// 开头', 'warn'); return; }
              selected.add(v);
              // 自定义项也进候选列表（否则勾选了却看不到）
              if (!candidates.some((c) => c.url === v)) {
                candidates.push({ url: v, name: '自定义', note: '' });
                candBox.append(candidateRow({ url: v, name: '自定义', note: '' }));
              }
              customInput.value = '';
              toast('已加入待保存列表', 'ok');
            },
          }),
        ]),
        h('div.hint', { style: { marginTop: '8px' }, text: '保存前建议先点「检测可用性」确认这条地址真的能用。' }),
      ]),
    ]),
  );

  // 状态行
  appendAll(statusBox,
    h('span.pill' + (st.supported ? '.ok' : '.warn'), {
      text: st.supported ? '面板可自动配置' : '面板不自动改这台机器的配置',
      title: st.note || '',
    }),
    h('span.pill', { text: '配置文件：' + (st.config_path || '未知'), title: st.config_path || '' }),
    h('span.pill' + (st.need_restart ? '.warn' : ''), {
      text: st.need_restart ? '配置与生效不一致（需要重启运行时）' : '配置已生效',
    }),
    (st.effective || []).length
      ? h('span.pill.ok', { text: '生效中：' + st.effective.join('、') })
      : h('span.pill.warn', { text: '当前没有任何加速源' }),
  );
  if (st.note) {
    statusBox.append(h('div.hint', { style: { flexBasis: '100%' }, text: st.note }));
  }

  // 候选列表：内置 + 已配置但不在内置里的（用户自己加过的）
  const all = candidates.slice();
  (st.configured || []).forEach((u) => {
    if (!all.some((c) => c.url === u)) all.push({ url: u, name: '自定义', note: '配置文件里已有的条目' });
  });
  all.forEach((c) => candBox.append(candidateRow(c)));

  // 用缓存（或"尚未检测"）渲染；**不自动探测** —— 探测由用户点按钮触发
  syncProbeLabels();
  renderCheckInfo();
}
