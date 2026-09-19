// reverseproxy.js —— 独立的「反向代理」功能页。
//
// 为什么单独一页（而不是塞在「网站管理」里）：
//   网站管理的模型是"一个站点 = 域名 + 根目录 + PHP"，而这里要建的反代规则
//   里**大半根本没有站点目录** —— 只是"把这个端口/域名的请求转到另一台机器"。
//   硬塞进站点模型会出现"必须填一个并不存在的根目录"这种别扭。
//
// 界面上刻意把三件事说清楚，因为反代最容易在这三处出问题：
//   1. **监听端口**：规则写好了但 nginx 没在听（端口被占/nginx 没起）→ 如实标红；
//   2. **目标是否可达**：保存前就能点「测试连通」，不用先保存再猜；
//   3. **WebSocket**：默认开。关掉它，带界面的服务会"能打开但用不了"。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';
// 证书列表的归一化与"按域名挑证书"直接复用「SSL 证书」页的纯函数：
// 反代与站点两侧必须用同一套匹配规则，否则同一个域名在两个页面会选到不同证书。
import { normalizeCerts, pickCertForDomain } from './certs.js';

let cache = null;
// probeErr 是最近一次异步状态探测的错误（列表徽标据此显示"检测失败"，不谎报不可达）。
let probeErr = '';

// SSL_PROVIDERS 是证书来源选项，取值与站点侧完全一致（self/mkcert/manual/acme）。
// ACME 放第一位并标注"推荐"：它是唯一能被浏览器直接信任的来源，
// 而另外三种（自签/mkcert/手工）都依赖用户自己的信任链。
const SSL_PROVIDERS = [
  { value: 'acme', label: "面板证书库（ACME / Let's Encrypt，推荐）" },
  { value: 'mkcert', label: 'mkcert 本地 CA（本机信任时不提示）' },
  { value: 'self', label: '自签证书（浏览器会提示不受信任）' },
  { value: 'manual', label: '粘贴自有证书' },
];

// authSummary 把后端的「回读生效值」拼成一行文字。
//
// 绝不只说"已保存"：verified=true 才说生效；读不到就明确写"未复核"并带上原因
//（用户 2026-09-25 明确要求"保存后要回读生效值，读不到就如实说未复核"）。
function authSummary(it) {
  const a = (it && it.auth) || {};
  if (!a.enabled) return '';
  const base = '当前：用户名 ' + (a.user || '（未设置）');
  if (a.verified) return base + '；回读：✅ 密码文件与 nginx 配置都已生效。';
  return base + '；回读：⚠️ 未复核 —— ' + (a.note || '读不到生效值，请重新保存并检查 nginx。');
}

// cacheSummary 把后端给的缓存"回读生效值"拼成一行文字（同样绝不说"已保存"了事）。
function cacheSummary(it) {
  const c = (it && it.cache) || {};
  if (!c.enabled) return '';
  const base = '当前：上限 ' + (c.size || '?') + '，有效期 ' + (c.valid || '?') + '，目录 ' + (c.dir || '?');
  if (c.verified) return base + '；回读：✅ conf.d 声明与该规则的 nginx 配置都已生效。';
  return base + '；回读：⚠️ 未复核 —— ' + (c.note || '读不到生效值，请重新保存并检查 nginx。');
}

// sslSummary 把后端给的证书摘要拼成一行可读文字（来源 / 覆盖域名 / 到期 / 剩余）。
// 字段缺失时逐项跳过，不编造——没有的信息宁可不说。
function sslSummary(it) {  const s = (it && it.ssl) || {};
  const parts = ['HTTPS 已启用 · ' + (s.provider_label || s.provider || '证书')];
  const domains = Array.isArray(s.domains) ? s.domains.filter(Boolean) : [];
  if (domains.length) parts.push('覆盖 ' + domains.join(', '));
  if (s.expires) parts.push('到期 ' + s.expires);
  if (s.days_left != null && s.days_left >= 0) parts.push('剩余 ' + s.days_left + ' 天');
  else if (s.days_left != null && s.days_left < 0) parts.push('已过期');
  if (s.renew_hint) parts.push('⚠️ ' + s.renew_hint);
  return parts.join(' · ');
}

// sslTitle 是锁标志的悬浮说明（换行分隔，便于看清路径）。
function sslTitle(it) {
  const s = (it && it.ssl) || {};
  const lines = [sslSummary(it)];
  if (s.cert_path) lines.push('证书：' + s.cert_path);
  if (s.key_path) lines.push('私钥：' + s.key_path);
  return lines.join('\n');
}

// fmtBytes 把字节数变成人读的单位（只用于列表展示，不参与任何判定）。
function fmtBytes(n) {
  const v = Number(n) || 0;
  if (v < 1024) return v + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let x = v / 1024;
  let i = 0;
  while (x >= 1024 && i < units.length - 1) { x /= 1024; i += 1; }
  return x.toFixed(x >= 100 ? 0 : 1) + ' ' + units[i];
}

// lanForwardPill 显示这条规则的「局域网出口」状态。
//
// 三种情况必须能一眼分开：真的在转发（回环端口在听）、明确选了 nginx 直连、
// 以及"自动判定结果显示目标是局域网却不在转发"（= 有 502 风险，标红）。
function lanForwardPill(it) {
  // 停用的规则不写 nginx 配置、转发器也已停掉：不拿"未在转发"去吓用户。
  if (!it.enabled) return null;
  if (it.forward_active) {
    return h('span.pill.ok', {
      text: '经面板转发 :' + (it.forward_port || '?'),
      title: '面板在回环上转发到 ' + (it.forward_upstream || it.target) + '；nginx 只连 127.0.0.1，不受 macOS 本地网络授权影响',
    });
  }
  if (it.lan_forward === 'off') {
    return h('span.pill' + (it.target_scope === 'private' ? '.danger' : ''), {
      text: it.target_scope === 'private' ? 'nginx 直连（局域网，有风险）' : 'nginx 直连',
      title: it.target_scope === 'private'
        ? '目标是局域网地址，nginx 直连可能被 macOS 15「本地网络」授权拦成 502'
        : 'nginx 直接连目标（旧行为）',
    });
  }
  if (it.target_scope === 'private') {
    return h('span.pill.danger', {
      text: '局域网目标未在转发',
      title: '这条规则的目标是局域网地址，但转发器没有在听：查看下面的转发错误或重新保存规则',
    });
  }
  return null;
}

// lanDirectWarning 是"局域网目标 + nginx 直连"时的显眼提示。
//
// 这是本功能存在的理由：macOS 15 会拦 Homebrew 的 nginx 访问局域网，而无头
// 服务器上没人点弹窗 —— 症状是反代全部 502，而用户完全看不出原因。
function lanDirectWarning(it) {
  if (!it.lan_direct_warning) return null;
  return h('div.banner.banner-warn', [
    h('strong', { text: '⚠️ 可能因 macOS 授权失效而 502：' }),
    h('span', {
      text: '目标 ' + it.target + ' 是局域网地址，但这条规则选择了 nginx 直连。'
        + 'macOS 15 的「本地网络」隐私门会拦 Homebrew 的 nginx（无头服务器没人点弹窗授权）。'
        + '建议把「局域网出口」改成「自动」或「经面板转发」。',
    }),
  ]);
}

export function ReverseProxyView(content, ctx = {}) {
  clear(content);

  const headBox = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
  const grid = h('div', { style: { display: 'grid', gap: '12px' } });
  const wrap = h('div.card', [
    h('div.card-head', [
      h('h3', { text: '反向代理' }),
      h('div.spacer'),
      headBox,
    ]),
    h('div.card-body.tight', [h('div.hint', {
      text: '一条规则 = 监听端口 + 域名（可留空）+ 路径前缀（可留空）→ 目标地址。' +
        '不需要站点目录；配置写进 nginx 的 vhosts 目录，停用即移除。',
    })]),
    h('div.card-body.tight', [grid]),
  ]);
  content.append(wrap);

  async function load() {
    clear(grid);
    appendAll(grid, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取规则…' })]));
    try {
      cache = await api.proxies();
    } catch (e) {
      clear(grid);
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
      ]));
      return;
    }
    renderHead();
    renderGrid();
    // 首屏渲染完**立刻**异步探测状态：列表接口本身不再跑任何真实探测，
    // 所以目标不可达也不会拖慢打开页面（2026-09-19 用户报"明显需要等待"）。
    void probeStatuses();
  }

  // probeStatuses 渲染完/点「检测」时异步探测规则状态。
  //
  // 后端并发探测、每条 ≤500ms、结果进 60s TTL 缓存；这里把结果写回当前列表
  // 并重绘徽标。探测失败**不**把状态改成"不可达"（那会是谎报），只提示失败。
  async function probeStatuses(ids) {
    let r;
    try {
      r = await api.proxyProbeStatus(ids && ids.length ? { ids } : { all: true });
      probeErr = '';
    } catch (e) {
      probeErr = (e && e.message) || String(e);
      renderGrid();
      return;
    }
    const byId = new Map((r.list || []).map((x) => [x.id, x]));
    (cache?.list || []).forEach((it) => {
      const x = byId.get(it.id);
      if (!x) return;
      it.port_listening = !!x.port_listening;
      it.target_ok = !!x.target_ok;
      it.target_detail = x.target_detail;
      it.status_probed = true;
      it.status_stale = false;
      it.status_probe_at = x.probed_at;
      it.status_probe_age_ms = 0;
    });
    renderGrid();
  }

  function renderHead() {
    clear(headBox);
    const ng = cache?.nginx || {};
    // 反向代理由 nginx 执行；没装时只给按钮加 title 太隐蔽（用户点不动却不知道为什么），
    // 所以在页面上直接给一条横幅。
    if (!ng.installed) {
      appendAll(headBox, h('div.banner.banner-warn', [
        h('strong', { text: '反向代理需要 nginx：' }),
        h('span', { text: '请先到「应用市场 → 网站环境」安装 nginx，装完刷新本页即可新建规则。' }),
      ]));
    }
    appendAll(headBox,
      ng.installed
        ? h('span.pill.ok', { text: 'nginx 已安装' })
        : h('span.pill.danger', { text: '需要先安装 nginx' }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      // 「修复 nginx 环境」从「网站管理」搬到这里（用户要求）：它补的正是**反向代理**
      // 依赖的两样东西（conf.d 的加载 + WebSocket 升级用的 $connection_upgrade map），
      // 放在站点列表里既找不着、名字也说不清它干什么。名字改成直白的
      // 「补齐 nginx 全局配置」，title 里写清具体补什么、且只补缺失部分。
      ng.installed
        ? h('button.btn.btn-sm', {
          text: '🧩 补齐 nginx 全局配置',
          title: '确保 nginx.conf 里加载 conf.d/*.conf，并定义 WebSocket 升级所需的 $connection_upgrade map。\n'
            + '反向代理（尤其是带 WebSocket 的界面）依赖它；只补齐缺失的部分，不改你已有的配置。',
          onclick: async () => {
            try {
              const r = await api.nginxRepair();
              toast(r.msg || '已补齐', 'ok');
              load();
            } catch (e) { toast(e.message, 'err', 9000); }
          },
        })
        : null,
      h('button.btn.btn-sm.btn-primary', {
        text: '➕ 新建规则',
        disabled: !ng.installed,
        title: ng.installed ? '' : '反向代理由 nginx 执行，请先到「应用市场 → 网站环境」装 nginx',
        onclick: () => editRule(null),
      }));
  }

  function renderGrid() {
    clear(grid);
    const list = cache?.list || [];
    if (list.length === 0) {
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '🔀' }),
        h('p', { text: '还没有反向代理规则' }),
        h('p.hint', { text: '例如：监听 8090，目标 http://127.0.0.1:8090 —— 就能从这台机器访问镜像站上的站点' }),
      ]));
      return;
    }
    appendAll(grid, ...list.map(ruleCard));
  }

  function ruleCard(it) {
    const domains = (it.domains || '').split(',').map((s) => s.trim()).filter(Boolean);
    const title = it.name || ('规则 #' + it.id);
    // 状态徽标的"新鲜度"：probed=false 表示还没探测过（显示"检测中…"）；
    // 有检测时间就显示"约 N 秒前检测"，过期要明确标出来（不当实时状态展示）。
    const probed = !!it.status_probed;
    const ageS = it.status_probe_at
      ? Math.max(0, Math.round((Date.now() - Date.parse(it.status_probe_at)) / 1000))
      : null;
    return h('div', {
      style: {
        border: '1px solid var(--border)', borderRadius: 'var(--radius)',
        padding: '12px 14px', display: 'flex', flexDirection: 'column', gap: '8px',
        opacity: it.enabled ? 1 : 0.65,
      },
    }, [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('div', { style: { fontWeight: '620', fontSize: '13.5px' }, text: title }),
        it.enabled ? h('span.pill.ok', { text: '已启用' }) : h('span.pill', { text: '已停用' }),
        it.enabled
          ? (!probed
            ? h('span.pill', { text: '端口检测中…', title: '正在异步检测 ' + it.listen + ' 端口是否在监听' })
            : (it.port_listening
              ? h('span.pill.ok', { text: '端口 ' + it.listen + ' 在听' })
              : h('span.pill.danger', {
                text: '端口 ' + it.listen + ' 没有监听',
                title: 'nginx 没在这个端口上监听：可能 nginx 没启动、或端口被别的进程占用。看「日志中心」或终端里 nginx -t',
              })))
          : null,
        !probed
          ? h('span.pill', { text: '目标检测中…', title: '正在异步检测目标是否可达（每条 ≤500ms）' })
          : (it.target_ok
            ? h('span.pill.ok', { text: '目标可达', title: it.target_detail })
            : h('span.pill.warn', { text: '目标不可达', title: it.target_detail })),
        it.websocket ? h('span.pill', { text: 'WS' }) : null,
        it.ssl_enabled
          ? h('span.pill.ok', { text: '🔒 HTTPS', title: sslTitle(it) })
          : null,
        it.auth_enabled
          ? h('span.pill' + (it.auth_verified ? '.ok' : '.warn'), {
            text: it.auth_verified ? '🔑 鉴权' : '🔑 鉴权（未复核）',
            title: authSummary(it),
          })
          : null,
        it.cache_enabled
          ? h('span.pill' + (it.cache_verified ? '.ok' : '.warn'), {
            text: it.cache_verified
              ? ('⚡ 缓存 ' + (it.cache_size || '') + '/' + (it.cache_valid || ''))
              : '⚡ 缓存（未复核）',
            title: cacheSummary(it),
          })
          : null,
        lanForwardPill(it),
        h('div.spacer'),
      ]),
      h('div.mono', {
        style: { fontSize: '12.5px', color: 'var(--text-dim)', lineHeight: '1.7' },
      }, [
        h('div', { text: '监听 :' + it.listen + (domains.length ? '  ' + domains.join(', ') : '  (所有域名)') + (it.path ? '  ' + it.path + '*' : '') }),
        h('div', { text: '→  ' + it.target }),
        it.forward_active
          ? h('div', {
            text: '→  经面板转发 127.0.0.1:' + it.forward_port
              + '（活跃 ' + (it.forward_active_conns || 0) + ' / 累计 ' + (it.forward_total_conns || 0) + ' 条连接'
              + '，出 ' + fmtBytes(it.forward_bytes_in) + ' / 入 ' + fmtBytes(it.forward_bytes_out) + '）',
          })
          : null,
      ]),
      lanDirectWarning(it),
      it.forward_last_error
        ? h('div.hint', {
          style: { color: 'var(--danger)' },
          text: '最近一次转发错误（累计 ' + (it.forward_error_count || 0) + ' 次）：' + it.forward_last_error,
        })
        : null,
      it.ssl_enabled && it.ssl ? h('div.hint', { text: sslSummary(it) }) : null,
      it.remark ? h('div.hint', { text: it.remark }) : null,
      probed
        ? h('div.hint', {
          style: it.status_stale ? { color: 'var(--warn)' } : {},
          text: '状态检测时间：' + (ageS != null ? '约 ' + ageS + ' 秒前' : '刚检测')
            + (it.status_stale ? '（缓存已过期，点「检测」重测）' : ''),
        })
        : h('div.hint', {
          text: probeErr
            ? ('状态检测失败：' + probeErr + '（点「检测」重试）')
            : '状态：检测中…（列表不再同步探测，探测在后台跑）',
        }),
      probed && it.target_detail && !it.target_ok
        ? h('div.hint', { style: { color: 'var(--danger)' }, text: it.target_detail })
        : null,
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm', { text: '✏️ 编辑', onclick: () => editRule(it) }),
        h('button.btn.btn-sm', {
          text: it.enabled ? '⏸ 停用' : '▶ 启用',
          onclick: () => toggle(it),
        }),
        h('button.btn.btn-sm', {
          text: '🔍 测试目标',
          onclick: async () => {
            try {
              const r = await api.proxyTest(it.target);
              toast(r.detail || (r.ok ? '可达' : '不可达'), r.ok ? 'ok' : 'err', 8000);
            } catch (e) { toast('测试失败：' + e.message, 'err', 8000); }
          },
        }),
        // 「检测」= 重测这条规则的端口/目标状态（走异步批量接口的单条分支）。
        h('button.btn.btn-sm', {
          text: '检测',
          title: '重新检测这条规则的端口监听与目标可达性（每条 ≤500ms）',
          onclick: () => { void probeStatuses([it.id]); },
        }),
        // 「测大请求体」——**按需触发**（真的发 64KB），刻意不放进 load()/列表渲染：
        // "端口在听、目标可达"都不等于能收大请求体。2026-09-18 事故里，大请求体先被
        // 缓冲到 client_body_temp，目录不可写时 nginx 在转发前直接回它自己的 500 页，
        // 而所有常规状态都是绿的。坏的结果会写清卡在哪一步。
        h('button.btn.btn-sm', {
          text: '📦 测大请求体',
          title: '真的向这条规则的监听端口 POST 一个 64KB 请求体，确认 nginx 能收下并转发。'
            + '只在点这一下时跑，不会自动探测；失败会写清是哪一步'
            + '（nginx 请求体上限 / nginx 自身的 500 页 / 转发到上游）。',
          onclick: async () => {
            const pending = toast('正在向 :' + it.listen + ' 发送 64KB 请求体…', 'info', 0);
            try {
              const r = await api.probeProxyBody(it.id);
              pending.remove();
              const step = r.step ? '（' + r.step + '）' : '';
              const head = r.status === 'ok'
                ? '✅ 大请求体探测通过：'
                : (r.status === 'bad' ? '⛔ 大请求体探测失败' + step + '：' : '⚠️ 未能探测' + step + '：');
              toast(head + (r.detail || ''), r.status === 'ok' ? 'ok' : (r.status === 'bad' ? 'err' : 'warn'), 14000);
            } catch (e) {
              pending.remove();
              toast('探测请求本身失败：' + e.message, 'err', 10000);
            }
          },
        }),
        // 「清空缓存」只在开过缓存（配置在，或磁盘上还有缓存文件）时出现。
        (it.cache_enabled || it.cache_dir_exists)
          ? h('button.btn.btn-sm', {
            text: '🧹 清空缓存',
            title: '删除这条规则的本地缓存文件' + (it.cache_zone ? '（缓存区 ' + it.cache_zone + '）' : '')
              + '；不改 nginx 配置，也不影响目标机器。',
            onclick: () => clearCache(it),
          })
          : null,
        h('button.btn.btn-sm.btn-danger', { text: '删除', onclick: () => remove(it) }),
      ]),
    ]);
  }
  async function toggle(it) {
    try {
      await api.proxyToggle(it.id);
      toast(it.enabled ? '已停用（nginx 配置已移除）' : '已启用', 'ok');
      load();
    } catch (e) {
      toast('操作失败：' + e.message, 'err', 10000);
    }
  }

  async function remove(it) {
    const okGo = await confirmBox(
      '删除规则「' + (it.name || it.id) + '」？\n\n' +
      '· 会移除它的 nginx 配置并重载\n' +
      '· 它的本地缓存文件也会一起删除\n' +
      '· 目标机器上的服务**不受影响**',
      { title: '删除反向代理规则', okText: '删除' });
    if (!okGo) return;
    try {
      const r = await api.proxyDelete(it.id);
      // 后端清缓存失败时会带回 cache_clear_error：如实转述，不谎报"已删除干净"。
      if (r && r.cache_clear_error) {
        toast('已删除，但缓存没清掉：' + r.cache_clear_error, 'warn', 14000);
      } else {
        toast('已删除', 'ok');
      }
      load();
    } catch (e) {
      toast('删除失败：' + e.message, 'err', 10000);
    }
  }

  // clearCache 清空一条规则的本地缓存（只删文件：走主接口的 cache_clear 动作，
  // 不新增路由，也不重写 nginx 配置）。
  async function clearCache(it) {
    const okGo = await confirmBox(
      '清空规则「' + (it.name || it.id) + '」的缓存？\n\n' +
      '· 只删除本机缓存文件，下次访问会重新回源\n' +
      '· 不改 nginx 配置，也不影响目标机器',
      { title: '清空反向代理缓存', okText: '清空' });
    if (!okGo) return;
    try {
      const r = await api.proxyUpdate(it.id, { cache_clear: true });
      toast((r && r.msg) || '已清空缓存', 'ok');
      load();
    } catch (e) {
      toast('清空失败：' + e.message, 'err', 12000);
    }
  }

  // editRule 新建/编辑对话框。
  //
  // 校验顺序刻意做成"先测连通再保存"：反代最常见的失败就是目标写错，
  // 而保存成功、nginx 也 reload 成功、访问却 502 —— 那时候用户要自己去猜。
  //
  // HTTPS 区块与「网站管理 → SSL 证书」是同一套体验，但出于两点差异单独实现：
  //   · 反代规则只是**引用**证书，acme 证书的申请仍走「SSL 证书」页的任务中心；
  //   · 保存分两步：先保存规则本身，再调 /proxies/{id}/ssl 绑定证书。
  //     任何一步失败都按后端原文提示，不吞掉，也不谎报"HTTPS 已启用"。
  function editRule(it) {
    const isNew = !it;
    const cur = (it && it.ssl) || {};
    const hadSSL = !!cur.enabled;
    const f = {
      name: h('input.input', { value: it?.name || '', placeholder: '例如：镜像站' }),
      listen: h('input.input', { type: 'number', value: it?.listen ?? 8090, min: '1', max: '65535' }),
      domains: h('input.input', { value: it?.domains || '', placeholder: '留空 = 该端口上所有域名；多个用逗号分隔' }),
      path: h('input.input', { value: it?.path || '', placeholder: '留空 = 所有路径；或填前缀，例如 /api' }),
      target: h('input.input', { value: it?.target || '', placeholder: 'http://192.168.1.8:8090' }),
      preserve: h('input', { type: 'checkbox', checked: !!it?.preserve_host }),
      ws: h('input', { type: 'checkbox', checked: it ? !!it.websocket : true }),
      // 一键补齐常用请求头（Lucky 风格预设）。老规则没这个字段 → 默认关，
      // 生成结果与升级前逐字一致；新建规则默认勾上。
      stdHeaders: h('input', { type: 'checkbox', checked: it ? !!it.standard_headers : true }),
      // HTTPS 上游的 SNI（proxy_ssl_name）。留空自动推导：目标是域名用它本身，
      // 目标是 IP 用本条规则的第一个域名。
      tlsName: h('input.input', { value: it?.tls_name || '', placeholder: '留空自动；目标是 https://<IP> 时建议填上游域名' }),
      // 「局域网出口」三态。默认自动：目标是局域网时由面板回环转发，
      // 其余直连 —— 这样老规则升级后配置不用动，也不会因为授权门 502。
      lanForward: h('select.select', { id: 'zp-proxy-lan-forward' }, [
        h('option', { value: 'auto', text: '自动（推荐）：目标是局域网时经面板转发，其余 nginx 直连' }),
        h('option', { value: 'on', text: '经面板转发：把局域网出口收回面板（修 macOS 本地网络授权）' }),
        h('option', { value: 'off', text: 'nginx 直连：保持旧行为（局域网目标可能 502）' }),
      ]),
      enabled: h('input', { type: 'checkbox', checked: it ? !!it.enabled : true }),
      remark: h('input.input', { value: it?.remark || '', placeholder: '备注（可留空）' }),
      // ---- HTTPS ----
      sslOn: h('input', { type: 'checkbox', checked: hadSSL, id: 'zp-proxy-ssl-on' }),
      sslProvider: h('select.select', { id: 'zp-proxy-ssl-provider' },
        SSL_PROVIDERS.map((p) => h('option', { value: p.value, text: p.label }))),
      // 已有证书时才有意义：勾上就重新签发/重新应用（例如 mkcert 重签、换加密方式）。
      sslReissue: h('input', { type: 'checkbox', checked: false, id: 'zp-proxy-ssl-reissue' }),
      // 明文 HTTP 打到本端口时 301 跳 https（Lucky 同款行为），新建规则默认开。
      redirectHTTP: h('input', { type: 'checkbox', checked: it ? !!it.redirect_http : true }),
      // ---- 访问鉴权（HTTP Basic Auth，2026-09-25 用户要求）----
      // 开启后访问该规则地址必须通过 Basic Auth，否则 401（nginx 直接回）。
      authOn: h('input', { type: 'checkbox', checked: it ? !!it.auth_enabled : false }),
      authUser: h('input.input', { value: it?.auth_user || '', placeholder: '例如：admin' }),
      // 密码框永远不回显：留空 = 沿用已保存的密码（编辑已有规则时不会被迫重设）。
      authPass: h('input.input', { type: 'password', placeholder: it?.auth_enabled ? '留空 = 保持原密码不变' : '设置访问密码' }),
      // ---- 反向代理缓存（可选，默认关）----
      cacheOn: h('input', { type: 'checkbox', checked: it ? !!it.cache_enabled : false, id: 'zp-proxy-cache-on' }),
      cacheSize: h('select.select', { id: 'zp-proxy-cache-size' },
        [['256m', '256 MB'], ['1g', '1 GB（推荐）'], ['5g', '5 GB'], ['10g', '10 GB'], ['50g', '50 GB']]
          .map(([v, label]) => h('option', { value: v, text: label }))),
      cacheValid: h('select.select', { id: 'zp-proxy-cache-valid' },
        [['10m', '10 分钟'], ['1h', '1 小时（推荐）'], ['1d', '1 天'], ['7d', '7 天'], ['30d', '30 天']]
          .map(([v, label]) => h('option', { value: v, text: label }))),
    };
    f.sslProvider.value = SSL_PROVIDERS.some((p) => p.value === cur.provider) ? cur.provider : 'acme';
    f.lanForward.value = ['auto', 'on', 'off'].includes(it?.lan_forward) ? it.lan_forward : 'auto';
    const sizeOpts = Array.from(f.cacheSize.options).map((o) => o.value);
    const validOpts = Array.from(f.cacheValid.options).map((o) => o.value);
    f.cacheSize.value = sizeOpts.includes(it?.cache_size) ? it.cache_size : '1g';
    f.cacheValid.value = validOpts.includes(it?.cache_valid) ? it.cache_valid : '1h';

    const certSel = h('select.select', { id: 'zp-proxy-ssl-cert' });
    const certHint = h('div.hint', { text: '正在读取证书…' });
    const acmeRow = h('div.field', { id: 'zp-proxy-ssl-acme' }, [
      h('label', { text: '选择证书（面板证书库）' }),
      certSel,
      certHint,
    ]);
    const manualCert = h('textarea.textarea', {
      placeholder: '-----BEGIN CERTIFICATE-----\n…', style: { minHeight: '96px' },
    });
    const manualKey = h('textarea.textarea', {
      placeholder: '-----BEGIN PRIVATE KEY-----\n…', style: { minHeight: '96px' },
    });
    const manualRow = h('div', { id: 'zp-proxy-ssl-manual', style: { display: 'none' } }, [
      h('div.field', [h('label', { text: '证书（含链）' }), manualCert]),
      h('div.field', [h('label', { text: '私钥' }), manualKey,
        hadSSL && cur.provider === 'manual'
          ? h('div.hint', { text: '已经保存过一份手工证书；不改的话留空即可沿用。' })
          : null]),
    ]);
    const reissueRow = h('label', {
      style: { display: hadSSL ? 'flex' : 'none', gap: '8px', alignItems: 'center', marginBottom: '8px' },
    }, [f.sslReissue, h('span', { text: '重新签发 / 重新应用证书（默认复用已绑定的那张，不重签）' })]);
    const sslDetail = h('div', { id: 'zp-proxy-ssl-detail', style: { marginTop: '8px' } }, [
      h('div.field', [h('label', { text: '证书来源' }), f.sslProvider,
        h('div.hint', {
          text: 'ACME 证书请在「SSL 证书」页申请（申请/续期是后台任务），这里只负责引用；'
            + '引用的是证书库路径，续期后同路径覆盖，规则不用改。',
        })]),
      acmeRow, manualRow, reissueRow,
      hadSSL
        ? h('div.hint', { text: '当前已启用：' + sslSummary(it) })
        : null,
    ]);

    // ---- 证书列表：懒加载 + 按规则域名预选 ----
    let certsCache = null;
    let certsErr = '';
    async function loadCerts() {
      if (certsCache) return certsCache;
      try {
        const all = normalizeCerts(await api.certs());
        // 申请失败的条目没有证书文件，绝不能出现在"可绑定"列表里。
        certsCache = all.filter((c) => c.status !== 'failed');
      } catch (e) {
        certsErr = e.message;
        certsCache = [];
      }
      return certsCache;
    }
    function renderCertHint() {
      if (certsErr) {
        certHint.textContent = '读取证书失败：' + certsErr;
        certHint.style.color = 'var(--danger)';
        return;
      }
      certHint.style.color = '';
      const c = (certsCache || []).find((x) => x.primary === certSel.value);
      if (!c) { certHint.textContent = ''; return; }
      const parts = ['覆盖：' + c.domains.join(', ')];
      if (c.notAfter) parts.push('到期：' + c.notAfter);
      if (c.daysLeft != null) parts.push('剩余 ' + c.daysLeft + ' 天');
      if (c.issuer) parts.push('签发机构：' + c.issuer);
      certHint.textContent = parts.join(' · ');
    }
    certSel.addEventListener('change', renderCertHint);

    async function renderCerts() {
      certHint.style.color = '';
      certHint.textContent = '正在读取证书…';
      const certs = await loadCerts();
      // clear 必须放在 await 之后：开关/来源被快速切换时会有两次并发渲染，
      // 两次都"先清后填"会叠加成 6 个选项（真被验证脚本抓到过）。
      clear(certSel);
      if (certsErr) { renderCertHint(); return; }
      if (!certs.length) {
        certHint.style.color = 'var(--warn)';
        certHint.textContent = '面板里还没有可用证书：请先到「SSL 证书」页申请一张（http-01 或 dns-01），再回来选。';
        return;
      }
      const firstDomain = (f.domains.value.split(/[\s,;]+/).map((s) => s.trim().toLowerCase()).filter(Boolean)[0]) || '';
      const byDomain = pickCertForDomain(certs, firstDomain);
      // 编辑已有 SSL 时优先按现有证书路径预选，避免"什么都没改却换了证书"。
      const byPath = cur.cert_path ? certs.find((c) => c.certPath === cur.cert_path) : null;
      const picked = byPath || byDomain;
      certs.forEach((c) => certSel.append(h('option', {
        value: c.primary,
        text: `${c.primary}（${c.domains.join(', ')}）` + (c.daysLeft != null ? ` · 剩余 ${c.daysLeft} 天` : ''),
        selected: !!picked && c.primary === picked.primary,
      })));
      renderCertHint();
    }

    function renderSSL() {
      const on = f.sslOn.checked;
      sslDetail.style.display = on ? '' : 'none';
      if (!on) return;
      const p = f.sslProvider.value;
      acmeRow.style.display = p === 'acme' ? '' : 'none';
      manualRow.style.display = p === 'manual' ? '' : 'none';
      if (p === 'acme') renderCerts();
    }
    f.sslOn.addEventListener('change', renderSSL);
    f.sslProvider.addEventListener('change', renderSSL);

    const testOut = h('div.hint', { text: '' });
    const row = (label, node, hint) => h('div', { style: { marginBottom: '10px' } }, [
      h('div', { style: { fontSize: '12px', color: 'var(--text-dim)', marginBottom: '4px' }, text: label }),
      node,
      hint ? h('div.hint', { text: hint }) : null,
    ]);

    // ---- 访问鉴权详情（开关关掉时整块隐藏）----
    const authReadback = it && it.auth_enabled
      ? h('div.hint', { style: { color: it.auth_verified ? 'var(--ok)' : 'var(--warn)' },
        text: authSummary(it) })
      : null;
    const authDetail = h('div', { id: 'zp-proxy-auth-detail', style: { marginTop: '8px' } }, [
      row('用户名', f.authUser, '存在 htpasswd 文件里的用户名（建议只用字母数字与 . _ -）'),
      row('密码', f.authPass, it?.auth_enabled
        ? '留空 = 保持已保存的密码不变；填了就换成新的。'
        : '开启鉴权时必须设置；密码只保存哈希。'),
      authReadback,
    ]);
    const renderAuth = () => { authDetail.style.display = f.authOn.checked ? '' : 'none'; };
    f.authOn.addEventListener('change', renderAuth);

    // ---- 反向代理缓存详情（开关关掉时整块隐藏）----
    const cacheReadback = it && it.cache_enabled
      ? h('div.hint', { style: { color: it.cache_verified ? 'var(--ok)' : 'var(--warn)' },
        text: cacheSummary(it) })
      : null;
    const cacheDetail = h('div', { id: 'zp-proxy-cache-detail', style: { marginTop: '8px' } }, [
      row('缓存上限', f.cacheSize, '超过上限时 nginx 淘汰最久没用的缓存'),
      row('有效期', f.cacheValid, '超过有效期后回源重新校验'),
      cacheReadback,
      it && it.cache_dir_exists
        ? h('div.hint', { text: '缓存目录：' + (it.cache_dir || '') + '（已有缓存文件，可用列表上的「清空缓存」删掉）' })
        : null,
    ]);
    const renderCache = () => { cacheDetail.style.display = f.cacheOn.checked ? '' : 'none'; };
    f.cacheOn.addEventListener('change', renderCache);
    const body = h('div', [
      row('规则名称', f.name, '只用于你自己识别'),
      row('监听端口', f.listen, 'nginx 在这个端口上接收请求；80 需要 root，面板已具备'),
      row('域名（可选）', f.domains, '留空表示这个端口上任何域名都走这条规则'),
      row('路径前缀（可选）', f.path, '只代理某个前缀，例如 /api；留空代理全部'),
      row('目标地址', f.target, '例：http://127.0.0.1:8090、https://<面板地址>:8443'),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px' } },
        [f.preserve, h('span', { text: '把原始 Host 透传给目标（默认关：多数后端按目标 Host 分站，透传会 404）' })]),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px' } },
        [f.ws, h('span', { text: '代理 WebSocket（带界面的服务建议开，关掉会"能打开但用不了"）' })]),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '6px' } },
        [f.stdHeaders, h('span', { text: '一键补齐常用请求头（X-Forwarded-Host / X-Forwarded-Port / REMOTE-HOST）' })]),
      h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginBottom: '10px' } },
        [f.enabled, h('span', { text: '启用' })]),
      row('HTTPS 上游 SNI（可选）', f.tlsName, '目标是 https:// 时，nginx 默认不发 SNI，可能落到上游的默认 server'),
      row('局域网出口', f.lanForward,
        'macOS 15 的「本地网络」隐私门会拦 Homebrew 的 nginx 访问局域网（无头服务器没人点弹窗）→ 反代 502。'
        + '「自动」会在目标是局域网时改由面板从回环转发；「nginx 直连」保留旧行为。'),
      // 回环端口是面板分配的结果，只读展示（表单里不能改）。
      it && (it.forward_port || it.forward_last_error)
        ? h('div.hint', {
          text: '当前：' + (it.forward_active
            ? '正在经面板转发 127.0.0.1:' + it.forward_port
              + '（活跃 ' + (it.forward_active_conns || 0) + ' / 累计 ' + (it.forward_total_conns || 0) + ' 条连接）'
            : (it.forward_port ? '已分配回环端口 ' + it.forward_port + '，但当前没有在监听' : '没有使用面板转发'))
            + (it.forward_last_error ? '；最近一次转发错误：' + it.forward_last_error : ''),
        })
        : null,
      row('备注', f.remark),
      // ---- 访问鉴权（HTTP Basic Auth）----
      h('div', { style: { borderTop: '1px solid var(--border-soft)', paddingTop: '12px', marginTop: '4px' } }, [
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          f.authOn, h('span', { style: { fontWeight: '600' }, text: '需要用户名密码（HTTP Basic Auth）' }),
        ]),
        h('div.hint', {
          text: '开启后，访问这条规则的地址必须先通过 Basic Auth，否则 nginx 直接回 401'
            + '（响应头带 WWW-Authenticate: Basic realm=…）。密码只保存 apr1 哈希：'
            + '不回显、不写日志、不进审计。',
        }),
        authDetail,
      ]),
      // ---- 反向代理缓存（可选，默认关）----
      h('div', { style: { borderTop: '1px solid var(--border-soft)', paddingTop: '12px', marginTop: '4px' } }, [
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' },
          title: 'nginx 只在开启响应缓冲（proxy_buffering on）时才写缓存；'
            + '该规则下的 SSE / 流式输出会从"边收边发"变成"先攒后发"，要流式就别开缓存。' }, [
          f.cacheOn, h('span', { style: { fontWeight: '600' }, text: '启用缓存（回源一次，之后走本地缓存）' }),
        ]),
        h('div.hint', { text: '镜像站 / 图床 / CDN 前置适用；关闭时行为与现在完全一致。' }),
        cacheDetail,
      ]),
      h('div', { style: { borderTop: '1px solid var(--border-soft)', paddingTop: '12px', marginTop: '4px' } }, [
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          f.sslOn, h('span', { style: { fontWeight: '600' }, text: '启用 HTTPS（由 nginx 直接终止 TLS）' }),
        ]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } },
          [f.redirectHTTP, h('span', { text: '明文 HTTP 自动 301 跳 HTTPS（同端口，例如 http://域名:8889 → https://域名:8889）' })]),
        h('div.hint', {
          text: '同一端口的规则不能 HTTP/HTTPS 混用（nginx 一个端口只有一种协议）；'
            + '80 端口不能开 HTTPS（面板默认站点占着它）。',
        }),
        sslDetail,
      ]),
      testOut,
    ]);

    const m = modal({
      title: (isNew ? '新建' : '编辑') + '反向代理规则',
      wide: true,
      body,
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn', {
          text: '🔍 测试目标',
          onclick: async () => {
            const t = f.target.value.trim();
            if (!t) { toast('请先填目标地址', 'warn'); return; }
            testOut.textContent = '正在测试…';
            try {
              const r = await api.proxyTest(t);
              testOut.textContent = r.detail || '';
              testOut.style.color = r.ok ? 'var(--ok)' : 'var(--danger)';
            } catch (e) {
              testOut.textContent = '测试失败：' + e.message;
              testOut.style.color = 'var(--danger)';
            }
          },
        }),
        h('button.btn.btn-primary', {
          text: isNew ? '创建' : '保存',
          onclick: async () => {
            const sslOn = f.sslOn.checked;
            const provider = f.sslProvider.value;
            const manualC = manualCert.value.trim();
            const manualK = manualKey.value.trim();
            // 开 HTTPS 且需要新取证书时，先在前端做"缺什么"的拦截，
            // 避免规则先建好、证书却没绑上（那样用户会以为 HTTPS 生效了）。
            const needNew = sslOn && (!hadSSL || provider !== cur.provider || f.sslReissue.checked);
            if (needNew && provider === 'acme' && !certSel.value) {
              toast('请选择一张证书；如果列表为空，请先到「SSL 证书」页申请', 'warn', 12000);
              return;
            }
            if (needNew && provider === 'manual' && (!manualC || !manualK)) {
              toast('手工模式需要同时粘贴证书与私钥', 'warn', 12000);
              return;
            }

            const payload = {
              name: f.name.value.trim(),
              listen: Number(f.listen.value) || 0,
              domains: f.domains.value.trim(),
              path: f.path.value.trim(),
              target: f.target.value.trim(),
              preserve_host: f.preserve.checked,
              websocket: f.ws.checked,
              enabled: f.enabled.checked,
              remark: f.remark.value.trim(),
              standard_headers: f.stdHeaders.checked,
              tls_name: f.tlsName.value.trim(),
              redirect_http: f.redirectHTTP.checked,
              lan_forward: f.lanForward.value,
            };
            // 访问鉴权：开关 + 用户名 + 密码。空密码**不放进 payload**，
            // 后端据此判定"沿用原密码"（密码框永不回显）。
            payload.auth_enabled = f.authOn.checked;
            payload.auth_user = f.authUser.value.trim();
            if (f.authPass.value !== '') payload.auth_password = f.authPass.value;
            // 缓存：开关 + 上限 + 有效期（后端会归一化并回读真实配置）。
            payload.cache_enabled = f.cacheOn.checked;
            payload.cache_size = f.cacheSize.value;
            payload.cache_valid = f.cacheValid.value;
            if (f.authOn.checked) {
              if (!payload.auth_user) {
                toast('开启了「需要用户名密码」，请填用户名', 'warn', 9000);
                return;
              }
              const hasStored = !!(it && it.auth && it.auth.password_set);
              if (f.authPass.value === '' && !hasStored) {
                toast('开启了「需要用户名密码」，请设置密码', 'warn', 9000);
                return;
              }
            }
            // 关闭 HTTPS：走主接口把 ssl_enabled=false 落库并重生成非 SSL 配置。
            if (hadSSL && !sslOn) payload.ssl_enabled = false;

            // 这次要**启用/更换 HTTPS**：必须把证书信息一起放进主接口的 payload。
            //
            // 真机报障（2026-09-17 用户）：启用 HTTPS 原本是"先建规则 → 再调
            // /proxies/{id}/ssl 绑证书"两步，而第一步的 payload 里没有任何 ssl 字段，
            // 后端看来它是一条 **HTTP** 规则 —— 当同一端口上已有 HTTPS 规则时
            // （mini 的 8889 上 wp/site2/panel2 都是 HTTPS），会被"同端口不能混用
            // HTTP/HTTPS"的保护直接 409，第二步永远没机会跑，用户怎么点都保存不了。
            //
            // 面板证书库里的 cert_path/key_path 就是 /ssl 会写进库的那一份，所以这里
            // 提前带上，主接口写出的就是一条**合法的 HTTPS 规则**（nginx -t 也才过得去）。
            if (sslOn && needNew) {
              if (provider === 'acme') {
                const c = (certsCache || []).find((x) => x.primary === certSel.value);
                if (c && c.certPath && c.keyPath) {
                  payload.ssl_enabled = true;
                  payload.ssl_provider = 'acme';
                  payload.ssl_cert = c.certPath;
                  payload.ssl_key = c.keyPath;
                }
              } else if (provider === 'manual' && manualC && manualK) {
                payload.ssl_enabled = true;
                payload.ssl_provider = 'manual';
                payload.ssl_cert = manualC;
                payload.ssl_key = manualK;
              }
              // self / mkcert 的证书是 /ssl 那一步现生成的，这里没有可用路径 ——
              // 那种情况后端会拦下并给出"先建成停用 → 绑好证书 → 再启用"的出路。
            }

            let saved = null;
            try {
              saved = isNew ? await api.proxyCreate(payload) : await api.proxyUpdate(it.id, payload);
            } catch (e) {
              // 后端的校验/冲突信息是给用户看的，原样弹出来
              toast(e.message, 'err', 12000);
              return;
            }

            // 第二步：绑定证书。provider 与证书都没变时跳过，避免每次改备注都重签。
            let needSSL = sslOn && (
              !hadSSL
              || provider !== cur.provider
              || f.sslReissue.checked
              || (provider === 'manual' && (manualC || manualK))
            );
            if (sslOn && !needSSL && provider === 'acme') {
              const c = (certsCache || []).find((x) => x.primary === certSel.value);
              if (c && c.certPath && cur.cert_path && c.certPath !== cur.cert_path) needSSL = true;
            }
            if (needSSL) {
              const savedId = (saved && saved.id) || it?.id;
              const sslPayload = { provider };
              if (provider === 'acme') sslPayload.cert_primary = certSel.value;
              if (provider === 'manual') { sslPayload.cert = manualC; sslPayload.key = manualK; }
              try {
                await api.proxySSL(savedId, sslPayload);
                toast('HTTPS 已启用', 'ok');
              } catch (e) {
                // 规则本身已保存，但证书没绑定成功 —— 必须如实说清楚，
                // 否则用户会以为已经在用 HTTPS 了。
                toast('规则已保存，但 HTTPS 未生效：' + e.message, 'err', 16000);
                close();
                load();
                return;
              }
            }
            // 保存结果要如实说清"鉴权有没有真的生效"：后端做了磁盘回读，
            // verified=true 才敢说已生效；读不到就显示"未复核"（用户明确要求）。
            const a = (saved && saved.auth) || {};
            let msg = isNew ? '规则已创建' : '规则已保存';
            let level = 'ok';
            let hold = 0;
            if (saved && saved.auth_enabled) {
              if (a.verified) {
                msg += '；访问鉴权已生效（用户名 ' + (a.user || '') + '）';
              } else {
                msg += '；访问鉴权未复核：' + (a.note || '读不到生效值，请检查 nginx 是否已重载');
                level = 'warn';
                hold = 16000;
              }
            }
            // 缓存同样要回读生效值（conf.d 的声明 + vhost 的 location 指令）。
            const cv = (saved && saved.cache) || {};
            if (saved && saved.cache_enabled) {
              if (cv.verified) {
                msg += '；缓存已生效（' + (cv.size || '') + ' / ' + (cv.valid || '') + '）';
              } else {
                msg += '；缓存未复核：' + (cv.note || '读不到生效值，请检查 nginx 是否已重载');
                level = 'warn';
                hold = 16000;
              }
            }
            toast(msg, level, hold);
            close();
            load();
          },
        }),
      ],
    });
    void m;
    renderSSL();
    renderAuth();
    renderCache();
    setTimeout(() => f.name.focus(), 60);
  }

  registerCleanup(() => { cache = null; });
  load();
}
