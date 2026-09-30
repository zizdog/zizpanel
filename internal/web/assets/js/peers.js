// peers.js —— 「多机」页（C5）：把另外几台 Mac 的状态集中在一屏。
//
// 这一版刻意**只读**：主面板定期拉子机的摘要（版本/负载/服务计数），不代它执行任何写操作。
// 写操作的信任模型没定清楚之前不做 —— 半成品的"远程控制"比没有更危险。
//
// 两条界面纪律：
//   · 每台子机必须能看出"最后一次成功是什么时候 / 上次为什么没连上"（拿旧摘要冒充最新
//     是这类功能最容易犯的错）；
//   · 本机凭证默认不显示，点「显示」才展开（旁边有风险提示：拿到它就能读本机摘要）。

import { api } from './api.js';
import { h, clear, toast, confirmBox, bytes, pct, duration, levelOf } from './ui.js';

export function PeersView(content) {
  clear(content);

  const selfBox = h('div.card');
  const listBox = h('div.card');
  const formBox = h('div.card');

  const nameInput = h('input.input', { placeholder: '例如 m4-mini', style: { flex: '1 1 160px' } });
  const urlInput = h('input.input', {
    placeholder: 'https://m4.lan:8443/ab12cd/（面板入口，含安全后缀）', style: { flex: '2 1 280px' },
  });
  const tokenInput = h('input.input', { placeholder: '子机的访问凭证', style: { flex: '1 1 200px' } });
  const fpInput = h('input.input', {
    placeholder: '证书指纹（https 必填，64 位十六进制）', style: { flex: '2 1 240px' },
  });
  const formMsg = h('div');

  async function load(showToast) {
    let st;
    try {
      st = await api.peers();
    } catch (e) {
      clear(listBox);
      listBox.append(h('div.card-body', [h('div.hint', { text: '读取子机列表失败：' + e.message })]));
      return;
    }
    renderSelf(st.self || {});
    renderList(st.peers || []);
    if (showToast) toast('已刷新', 'ok');
  }

  function renderSelf(self) {
    clear(selfBox);
    const tokenRow = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
    if (self.enabled && self.token) {
      const code = h('code.mono', { text: '••••••••' });
      let shown = false;
      tokenRow.append(
        code,
        h('button.btn.btn-sm', {
          text: '显示',
          onclick: (e) => {
            shown = !shown;
            code.textContent = shown ? self.token : '••••••••';
            e.target.textContent = shown ? '隐藏' : '显示';
          },
        }),
        h('button.btn.btn-sm', {
          text: '复制',
          onclick: async () => {
            try { await navigator.clipboard.writeText(self.token); toast('凭证已复制', 'ok'); }
            catch { toast('复制失败，请手动选中复制', 'warn'); }
          },
        }),
        h('button.btn.btn-sm', { text: '重新生成', onclick: () => setToken(true) }),
        h('button.btn.btn-sm', { text: '关闭', onclick: () => setToken(false) }),
      );
    } else {
      tokenRow.append(
        h('span.pill.warn', { text: '未开启' }),
        h('span.hint', { text: '开启后会生成一个只读凭证，把它填到主面板的「添加子机」里' }),
        h('button.btn.btn-sm.btn-primary', { text: '开启', onclick: () => setToken(true) }),
      );
    }
    selfBox.append(
      h('div.card-head', [h('h3', { text: '本机接入信息' }), h('div.spacer'),
        self.enabled ? h('span.pill.ok', { text: '允许被主面板聚合' }) : h('span.pill.warn', { text: '已关闭（默认）' })]),
      h('div.card-body', [
        h('p.hint', {
          text: '把下面两项填到另一台面板的「多机 → 添加子机」里，它就能只读地看到本机的状态。' +
            '凭证只读、可随时重新生成（旧的立即失效）；面板不会代本机执行任何操作。',
        }),
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', margin: '8px 0' } }, [
          h('span.hint', { text: '面板入口' }),
          h('code.mono', { text: self.url_hint || '' }),
        ]),
        tokenRow,
      ]),
    );
  }

  async function setToken(enabled) {
    if (!enabled) {
      const yes = await confirmBox('关闭后，所有主面板立即读不到本机状态。确定？', { title: '关闭被管理', danger: true, okText: '关闭' });
      if (!yes) return;
    }
    try {
      await api.agentToken(enabled);
      toast(enabled ? '已生成新凭证（旧凭证立即失效）' : '已关闭', 'ok');
      load(false);
    } catch (e) {
      toast('操作失败：' + (e && e.message ? e.message : e), 'err', 8000);
    }
  }

  function renderList(peers) {
    clear(listBox);
    listBox.append(h('div.card-head', [
      h('h3', { text: '子机' }),
      h('div.spacer'),
      h('button.btn.btn-sm', { text: '⟳ 全部刷新', onclick: () => refreshAll() }),
    ]));
    if (!peers.length) {
      listBox.append(h('div.card-body', [h('div.hint', { text: '还没有子机。在下面添加一台，或把本机的接入信息填到另一台面板上。' })]));
      return;
    }
    listBox.append(h('div.card-body', peers.map((p) => peerCard(p))));
  }

  // 摘要"多久没更新了"必须显示出来：一台子机三天前失联、界面却只写"已连接"，
  // 那是最容易骗到人的假状态（它看起来和刚拉过一模一样）。
  const STALE_AFTER_MS = 15 * 60 * 1000;
  function peerFreshness(p) {
    if (!p.last_at) return { stale: false, text: '' };
    const at = Date.parse(p.last_at);
    if (Number.isNaN(at)) return { stale: false, text: '' };
    const mins = Math.floor((Date.now() - at) / 60000);
    if (mins < 1) return { stale: false, text: '' };
    const text = mins < 60 ? mins + ' 分钟前' : Math.floor(mins / 60) + ' 小时前';
    return { stale: Date.now() - at > STALE_AFTER_MS, text };
  }

  function peerCard(p) {
    const s = p.summary || null;
    const fresh = peerFreshness(p);
    const stale = fresh.stale;
    const ageText = fresh.text;
    const head = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('strong', { text: p.name }),
      h('span.hint.mono', { text: p.url }),
      // 三态都要如实：失败 / 从没拉到过 / 数据可能过期 —— 只有"刚拉到"才显示「已连接」。
      p.last_error
        ? h('span.pill.danger', { text: '没连上', title: p.last_error })
        : (!p.last_at
          ? h('span.pill.warn', { text: '还没拉到数据', title: '添加之后还没成功拉到过它的状态' })
          : (stale
            ? h('span.pill.warn', { text: '数据可能过期', title: '很久没有成功拉到过它的状态了' })
            : h('span.pill.ok', { text: '已连接' }))),
      h('span.hint', { text: p.last_at ? '最后成功：' + p.last_at + (stale ? '（' + ageText + '）' : '') : '还没有成功过' }),
      h('div', { style: { flex: '1' } }),
      h('button.btn.btn-sm', { text: '刷新', onclick: () => refreshOne(p.id) }),
      h('button.btn.btn-sm', { text: '移除', onclick: () => removePeer(p) }),
    ]);
    const rows = [];
    if (p.last_error) {
      // 失败原因原样显示（事实），再给一句面板的推断（怎么修）—— 两者分开写，别混成一句。
      rows.push(h('div.hint', { text: '上次失败：' + p.last_error }));
      if (p.last_advice) rows.push(h('div.hint', { text: '→ ' + p.last_advice }));
    }
    if (s) {
      // 用 mem_used/mem_total（活动监视器口径），别拿 total−free 当已用：
      // 那样会把文件缓存算进去，与主面板本机显示不一致（见 internal/sysinfo/macos.go）。
      const memPct = s.mem_total ? (s.mem_used / s.mem_total) * 100 : 0;
      const diskPct = s.disk_total ? (s.disk_used / s.disk_total) * 100 : 0;
      rows.push(h('div', { style: { display: 'flex', gap: '14px', flexWrap: 'wrap', marginTop: '6px' } }, [
        metric('版本', s.version || '—'),
        metric('运行时长', duration(s.uptime || 0)),
        metric('CPU', pct(s.cpu_used || 0), levelOf(s.cpu_used || 0)),
        metric('内存', s.mem_total ? pct(memPct) + `（${bytes(s.mem_used || 0)}/${bytes(s.mem_total)}）` : '—', levelOf(memPct)),
        metric('磁盘', s.disk_total ? pct(diskPct) + `（${bytes(s.disk_used || 0)}/${bytes(s.disk_total)}）` : '—', levelOf(diskPct)),
        metric('服务', s.services ? `${s.services.running}/${s.services.total} 运行` : '—',
          s.services && s.services.problems ? 'danger' : 'ok'),
        metric('证书', s.certs ? `${s.certs.total} 张${s.certs.expiring ? `（${s.certs.expiring} 张快到期）` : ''}` : '—',
          s.certs && s.certs.expiring ? 'warn' : 'ok'),
      ]));
    }
    return h('div', { style: { borderTop: '1px solid var(--line, #2a2f3a)', padding: '10px 0' } }, [head, ...rows]);
  }

  function metric(label, value, level) {
    const cls = level === 'danger' ? 'span.pill.danger' : level === 'warn' ? 'span.pill.warn' : 'span.pill.ok';
    return h('div', [
      h('div.hint', { text: label }),
      h('div', [h(cls, { text: value })]),
    ]);
  }

  async function refreshOne(id) {
    try { await api.peerRefresh(id); await load(false); }
    catch (e) { toast('刷新失败：' + (e && e.message ? e.message : e), 'err', 8000); }
  }

  async function refreshAll() {
    try { await api.peerRefreshAll(); toast('已刷新', 'ok'); await load(false); }
    catch (e) { toast('刷新失败：' + (e && e.message ? e.message : e), 'err', 8000); }
  }

  async function removePeer(p) {
    const yes = await confirmBox(`移除子机「${p.name}」？（只是本面板不再聚合它，对那台机器没有任何影响）`,
      { title: '移除子机', okText: '移除' });
    if (!yes) return;
    try { await api.peerDelete(p.id); await load(false); }
    catch (e) { toast('移除失败：' + (e && e.message ? e.message : e), 'err', 8000); }
  }

  async function addPeer() {
    clear(formMsg);
    const body = {
      name: nameInput.value.trim(),
      url: urlInput.value.trim(),
      token: tokenInput.value.trim(),
      fingerprint: fpInput.value.trim(),
    };
    if (!body.name || !body.url || !body.token) {
      formMsg.append(h('div.hint', { text: '名字、面板入口、凭证都要填。' }));
      return;
    }
    try {
      await api.peerAdd(body);
      nameInput.value = ''; urlInput.value = ''; tokenInput.value = ''; fpInput.value = '';
      toast('已添加，正在拉取它的状态', 'ok');
      await load(false);
    } catch (e) {
      formMsg.append(h('div.hint', { text: '添加失败：' + (e && e.message ? e.message : e) }));
    }
  }

  formBox.append(
    h('div.card-head', [h('h3', { text: '添加子机' })]),
    h('div.card-body', [
      h('p.hint', {
        text: '在另一台机器上面板的「多机」页开启「被主面板管理」，把它给出的凭证与地址填到这里。' +
          'https 地址必须同时填证书指纹（面板之间多是自签证书，不固定指纹就等于信任中间人）。',
      }),
      h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', margin: '8px 0' } }, [
        nameInput, urlInput, tokenInput, fpInput,
        h('button.btn.btn-sm.btn-primary', { text: '添加', onclick: () => addPeer() }),
      ]),
      formMsg,
    ]),
  );

  content.append(selfBox, listBox, formBox);
  load(false);
}
