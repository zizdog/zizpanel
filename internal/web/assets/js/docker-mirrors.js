// docker-mirrors.js —— Docker 镜像加速源（换源）。
//
// 用户的原话是"现在的拉取太慢了，没法用"。这台机器上 registry-1.docker.io
// 直连会超时，所以界面第一件事就是把**官方源现在到底通不通**摆出来，
// 而不是让用户继续怀疑是自己网络的问题。
//
// 三条设计：
//   · 内置候选列表里每条都带**实测结论**（哪些站已经死了），
//     并且提供"检测可用性"用**用户自己的网络**再验一遍；
//   · 判定不看"能不能连上"：registry 的 /v2/ 返回 401 才是正常的；
//   · 保存会重启 Docker 运行时（约 30-60 秒，容器会短暂中断），
//     所以走任务中心，进度里能看到 colima restart 与一次真实拉取测试。

import { api } from './api.js';
import { h, clear, toast, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

let probeCache = null; // 探测结果（url → probe），避免每次切回都重探

export async function renderMirrors(pane, ctx) {
  clear(pane);
  appendAll(pane, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取加速源配置…' })]));

  let data;
  try {
    data = await api.dockerMirrors();
  } catch (e) {
    clear(pane);
    appendAll(pane, h('div.empty', [
      h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
    ]));
    return;
  }
  const st = data.state || {};
  const candidates = data.candidates || [];

  // 已选中的集合：以配置文件里的为准（生效中的可能是重启前的旧值）
  let selected = new Set((st.configured || []).map((u) => u.replace(/\/$/, '')));

  const hubBox = h('div');
  const candBox = h('div', { style: { display: 'flex', flexDirection: 'column', gap: '8px' } });
  const statusBox = h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } });
  const customInput = h('input.input', { placeholder: '自定义加速源地址，如 https://docker.m.daocloud.io', style: { flex: '1 1 260px' } });

  const probeBtn = h('button.btn.btn-sm', { text: '🔍 检测可用性', onclick: () => doProbe() });
  const saveBtn = h('button.btn.btn-sm.btn-primary', {
    text: '保存并重启 Docker',
    onclick: () => doSave(),
  });

  function syncProbeLabels() {
    // 把探测结果贴到候选行上（有结果才贴）
    if (!probeCache) return;
    candBox.querySelectorAll('[data-mirror]').forEach((row) => {
      const url = row.dataset.mirror;
      const slot = row.querySelector('.probe-slot');
      if (!slot) return;
      clear(slot);
      const p = probeCache[url];
      if (!p) {
        slot.append(h('span.pill', { text: '未检测' }));
        return;
      }
      const cls = p.ok ? '.ok' : '.warn';
      const ms = p.latency_ms ? `${p.latency_ms}ms` : '—';
      slot.append(h('span.pill' + cls, { text: p.ok ? `可用 ${ms}` : '不可用', title: p.detail || '' }));
    });
    // 官方源单独特判（它超时是最常见的情况，要说清）
    clear(hubBox);
    const hub = probeCache['https://registry-1.docker.io'];
    if (hub) {
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
            ? `现在能连上（${hub.latency_ms}ms）。加速源仍可提升拉取速度。`
            : `现在连不上（${hub.detail}）—— 这就是"拉取太慢/拉不动"的直接原因，必须配加速源。` }),
        ]),
      ]));
    }
  }

  async function doProbe() {
    probeBtn.disabled = true;
    probeBtn.textContent = '检测中…';
    try {
      const res = await api.dockerMirrorProbe();
      probeCache = {};
      (res.probes || []).forEach((p) => { probeCache[p.url] = p; });
      syncProbeLabels();
      const okCount = (res.probes || []).filter((p) => p.ok && p.url !== 'https://registry-1.docker.io').length;
      toast(`检测完成：${okCount} 个可用`, okCount > 0 ? 'ok' : 'warn');
    } catch (e) {
      toast('检测失败：' + e.message, 'err', 9000);
    } finally {
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
      probeBtn, saveBtn,
      h('span.sub', { text: '保存会重启 Docker 运行时（colima restart，约 30-60 秒），期间容器会短暂中断' }),
    ]),
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

  syncProbeLabels();
  // 首次进入自动探一次：用户来这一页就是想知道"现在到底能不能用"
  if (!probeCache) doProbe();
}
