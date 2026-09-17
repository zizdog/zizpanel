// certs.js —— 「SSL 证书」页：ACME（Let's Encrypt / ZeroSSL）自动申请、续期、删除，
// 以及站点侧「使用已有证书」的证书选择器。
//
// 为什么单独一页（而不是塞进「网站管理」）：
//   证书的生命周期比站点长 —— 一张 `*.example.com` 可以被多个站点共用、也可能先申请
//   后建站；而站点侧的 SSL Tab 只需要"引用哪一张"。两者分开后，"申请失败"与
//   "站点没配上证书"是两件各自可诊断的事，不会混在一个弹窗里。
//
// 三条硬约束（都来自这轮需求）：
//   1. **数据驱动**：DNS 服务商清单与它需要的字段全部来自
//      `GET /api/v1/certs/dns-providers`，前端不写任何服务商/域名分支。
//   2. **不碰私钥**：列表接口本身不含私钥；申请时的 DNS 凭据只在提交那一次发送，
//      输入框是 type=password、不回显、不缓存。
//   3. **长任务走任务中心**：申请/续期是异步任务（返回 task_id），进度复用
//      taskCenter.start()，这里绝不自己写进度轮询 —— 关掉窗口任务也要继续跑。
//
// 容错：后端接口可能还没落地。所有来自接口的数据都先过 normalize* 归一化，
// 任何形状不认识时最坏只表现为"列表为空 / 没有服务商"，**绝不让整页抛异常白屏**。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

// ---------------- 常量（每项只在这里出现一次） ----------------

// CA 选项。value 会**原样**作为 POST /api/v1/certs 的 ca 字段。
// staging 不是"次要选项"：ACME 正式环境对同一域名有每周签发上限，域名解析/80 端口
// 没验证通过前反复试会直接撞限流，所以界面上明确建议先用它验证。
const CA_OPTIONS = [
  { value: 'letsencrypt', label: "Let's Encrypt（正式）" },
  { value: 'letsencrypt-staging', label: "Let's Encrypt 测试（staging，建议先用它验证）" },
  { value: 'zerossl', label: 'ZeroSSL' },
];

// 验证方式。hint 直接写进表单，因为这两条前置条件是"申请失败"最常见的两个原因。
const CHALLENGE_OPTIONS = [
  {
    value: 'http-01',
    label: 'http-01（HTTP 文件校验）',
    hint: '需要域名的 80 端口从公网可达，且解析到本机（面板已放行 /.well-known/acme-challenge/）。不支持泛域名。',
  },
  {
    value: 'dns-01',
    label: 'dns-01（DNS TXT 校验）',
    hint: '在 DNS 服务商处加一条 TXT 记录即可，不需要 80 端口；支持 *.example.com 泛域名。填了凭据后由面板自动添加与清理。',
  },
];

// ---------------- 纯函数工具（导出：站点侧与验证脚本都用同一份逻辑） ----------------

function str(v) { return v == null ? '' : String(v).trim(); }

/** splitDomains 把"多行 + 逗号/空格分隔"的输入拆成规范化域名数组。 */
export function splitDomains(value) {
  return String(value == null ? '' : value)
    .split(/[\s,;]+/)
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);
}

/** isWildcard 判断是否泛域名（*.example.com）。 */
export function isWildcard(domain) { return String(domain || '').startsWith('*.'); }

/**
 * daysLeftFrom 从到期时间字符串算剩余天数（后端给了 days_left 就用后端的）。
 * 用 Date.parse 而不是直接比较字符串：后端可能给 RFC3339 也可能给 "2006-01-02"。
 */
export function daysLeftFrom(notAfter, now = Date.now()) {
  const t = Date.parse(notAfter);
  if (Number.isNaN(t)) return null;
  return Math.floor((t - now) / 86400000);
}

/**
 * normalizeCerts 把列表接口的返回归一成固定形状。
 *
 * 为什么要容忍多种字段名：接口契约只冻结了"包含哪些信息"，没冻结外层包装
 * （`{list:[...]}` / `{certs:[...]}` / 裸数组都可能）。归一化后即使后端换了包装，
 * 页面最坏也只是显示"暂无证书"，而不是 TypeError 白屏。
 */
export function normalizeCerts(data) {
  const raw = Array.isArray(data) ? data
    : (data && (data.list || data.certs || data.items)) || [];
  if (!Array.isArray(raw)) return [];
  return raw.map((c) => {
    if (!c || typeof c !== 'object') return null;
    const primary = str(c.primary || c.domain || c.main || c.name);
    if (!primary) return null;
    let domains = Array.isArray(c.domains) ? c.domains.map(str).filter(Boolean)
      : splitDomains(c.domains);
    if (!domains.length) domains = [primary];
    // status 缺省视为 issued：老后端（或只回证书的兼容接口）不该被当成失败条目。
    // 只认 "failed" 这一个明确值 —— 其它任何写法都按已签发处理，避免误报。
    const status = str(c.status || c.state).toLowerCase() === 'failed' ? 'failed' : 'issued';
    const notAfter = str(c.not_after || c.notAfter || c.expires || c.expiry);
    const given = c.days_left != null ? Number(c.days_left)
      : (c.daysLeft != null ? Number(c.daysLeft) : NaN);
    // 失败条目没有到期时间：days_left 必须视为"未知"而不是 0，
    // 否则会被当成"今天到期/已过期"，还会被顶部统计算进"30 天内到期"。
    const daysLeft = status === 'failed'
      ? null
      : (Number.isFinite(given) ? given : daysLeftFrom(notAfter));
    return {
      primary,
      domains,
      status,
      issuer: str(c.issuer || c.ca_name),
      notAfter,
      daysLeft,
      challenge: str(c.challenge),
      ca: str(c.ca),
      needsRenewal: !!(c.needs_renewal || c.needsRenewal),
      // 失败条目用于展示与「预填申请表单」；lastError 已由后端脱敏。
      // 注意 dnsProvider 只有服务商**名字**，凭据值从不出现在这里。
      lastError: str(c.last_error || c.lastError),
      lastErrorAt: str(c.last_error_at || c.lastErrorAt),
      failures: Number(c.failures) || 0,
      email: str(c.email),
      dnsProvider: str(c.dns_provider || c.dnsProvider),
      createdAt: str(c.created_at || c.createdAt),
      // cert_path / key_path 只是磁盘路径（契约里就没有私钥正文）。
      // 界面目前不展示它们，留着是为了排查问题时能对得上后端日志；
      // 任何情况下前端都不请求、不缓存、不回显私钥内容。
      certPath: str(c.cert_path),
      keyPath: str(c.key_path),
    };
  }).filter(Boolean);
}

/**
 * normalizeFields 归一化"一个服务商需要哪些环境变量"。
 *
 * 后端可能给 `env: ["CF_Token"]`、`env: {CF_Token: "说明"}`、
 * `fields: [{key,label,required}]`……契约只约定到"键名"，所以这里全部接住。
 * 无法识别的项直接丢弃：少一个输入框远好过整页崩掉。
 */
export function normalizeFields(item) {
  let src = item.fields || item.env || item.env_keys || item.environment
    || item.keys || item.variables || item.params || [];
  if (src && typeof src === 'object' && !Array.isArray(src)) {
    // {CF_Token: "API 令牌"} 这种"键 → 说明"的映射
    src = Object.entries(src).map(([key, v]) => (
      typeof v === 'string' ? { key, label: v } : Object.assign({ key }, v || {})));
  }
  if (!Array.isArray(src)) return [];
  return src.map((f) => {
    if (typeof f === 'string') return { key: f, label: f, required: true };
    if (!f || typeof f !== 'object') return null;
    const key = str(f.key || f.name || f.env || f.id || f.variable);
    if (!key) return null;
    return {
      key,
      label: str(f.label || f.title || f.description || f.desc || key),
      required: f.required !== false,
    };
  }).filter(Boolean);
}

/** normalizeProviders 归一化 DNS 服务商清单（数据驱动的核心）。 */
export function normalizeProviders(data) {
  const raw = Array.isArray(data) ? data
    : (data && (data.providers || data.list || data.items)) || [];
  if (!Array.isArray(raw)) return [];
  return raw.map((p) => {
    // 最简形态：后端只给一个名字数组
    if (typeof p === 'string') return { name: p, label: p, fields: [] };
    if (!p || typeof p !== 'object') return null;
    const name = str(p.name || p.id || p.code || p.key || p.provider);
    if (!name) return null;
    return {
      name,
      label: str(p.label || p.display_name || p.title || name),
      fields: normalizeFields(p),
    };
  }).filter(Boolean);
}

/**
 * wildcardMatch 判断证书里的一个域名模式是否能覆盖目标域名。
 * 只覆盖一层（*.example.com 能覆盖 a.example.com，不覆盖 a.b.example.com），
 * 与 TLS 证书的通配规则一致 —— 多覆盖一层是错的，会让用户以为配好了其实浏览器报错。
 */
export function wildcardMatch(pattern, domain) {
  const p = str(pattern).toLowerCase();
  const d = str(domain).toLowerCase();
  if (!p.startsWith('*.')) return false;
  const suffix = p.slice(2);
  if (!d.endsWith('.' + suffix)) return false;
  return d.slice(0, d.length - suffix.length - 1).indexOf('.') === -1;
}

/**
 * pickCertForDomain 为一个站点域名挑出最合适的证书（站点侧按域名预选）。
 * 顺序：精确命中 > 泛域名命中。都不中返回 null（界面据此给"去申请"引导）。
 */
export function pickCertForDomain(certs, domain) {
  const d = str(domain).toLowerCase();
  if (!d) return null;
  const exact = (certs || []).find((c) => c.domains.some((x) => str(x).toLowerCase() === d));
  if (exact) return exact;
  return (certs || []).find((c) => c.domains.some((x) => wildcardMatch(x, d))) || null;
}

// ---------------- 列表行的展示工具 ----------------

/**
 * daysPill 剩余天数徽标。<30 天必须"醒目"（黄色），已过期/濒临过期用红色。
 * 这个阈值不是拍脑袋：Let's Encrypt 证书 90 天有效，续期窗口就是到期前 30 天。
 */
function daysPill(days) {
  if (days == null) return h('span', { style: { color: 'var(--text-mute)' }, text: '未知' });
  if (days < 0) return h('span.pill.danger', { text: '已过期 ' + (-days) + ' 天' });
  if (days < 15) return h('span.pill.danger', { text: '⚠️ 仅剩 ' + days + ' 天' });
  if (days < 30) return h('span.pill.warn', { text: '⚠️ 剩余 ' + days + ' 天' });
  return h('span.pill.ok', { text: '剩余 ' + days + ' 天' });
}

// DNS 服务商清单在模块级缓存：同一张页面上反复打开申请窗不必重复请求。
// 它只用于"渲染输入框"，**不含任何用户填过的值**，所以缓存是安全的。
let providersCache = [];
let providersLoaded = false;
let providersError = '';
let providersLoading = null; // 进行中的请求：防止同时打开两个申请窗时重复拉取

// ---------------- 页面 ----------------

export function CertsView(content) {
  clear(content);

  const headBox = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } });
  const listBox = h('div');
  // 自动续期是在**面板进程内**的每日任务（不是 launchd 计划任务），
  // 因此"几点检查、提前多少天"必须让用户看得见 —— 否则用户无法判断
  // 它到底有没有在工作（后端一直在返回这两个值，以前前端没显示）。
  const renewBox = h('div.hint', { text: '⏰ 自动续期：读取中…' });

  content.append(h('div.card', [
    h('div.card-head', [
      h('h3', { text: 'SSL 证书' }),
      h('div.spacer'),
      headBox,
    ]),
    h('div.card-body.tight', [h('div.hint', {
      text: '在面板里自动申请并续期 HTTPS 证书。申请与续期都是后台任务，'
        + '进度看右上角的「任务中心」；申请好后到「网站管理 → 站点 → SSL 证书」里选用。',
    }), renewBox]),
    h('div.card-body.tight', [listBox]),
  ]));

  async function load() {
    clear(listBox);
    appendAll(listBox, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取证书…' })]));
    let certs;
    try {
      const raw = await api.certs();
      certs = normalizeCerts(raw);
      // 这两项来自后端 /api/v1/certs 的元信息（renew_daily_at / renew_threshold_days）。
      // 老后端没这两个字段时退化成一句通用说明，不显示"未知"这种废话。
      const daily = str(raw && raw.renew_daily_at);
      const days = Number(raw && raw.renew_threshold_days);
      renewBox.textContent = daily
        ? '⏰ 自动续期：' + daily + (days > 0 ? '；剩余有效期不足 ' + days + ' 天时自动重签' : '')
          + '（失败会写进任务中心与操作审计；也可以在下面每一行点「🔄 续期」手动触发）'
        : '⏰ 自动续期：面板内置每日任务会在证书到期前自动重签，'
          + '失败会写进任务中心与操作审计。';
    } catch (e) {
      clear(listBox);
      appendAll(listBox, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取证书失败' }),
        h('p', { text: e.message }),
        h('p.hint', { text: '如果后端接口还没上线，这里会一直失败；这不影响其它页面。' }),
      ]));
      renderHead([]);
      return;
    }
    renderHead(certs);
    renderList(certs);
  }

  function renderHead(certs) {
    clear(headBox);
    const issued = certs.filter((c) => c.status !== 'failed');
    const failed = certs.filter((c) => c.status === 'failed');
    const expiring = issued.filter((c) => c.daysLeft != null && c.daysLeft < 30).length;
    const running = taskCenter.findByTarget
      ? certs.filter((c) => taskCenter.findByTarget('cert:' + c.primary)).length
      : 0;
    appendAll(headBox,
      h('span.pill', { text: `共 ${issued.length} 张证书` }),
      failed.length ? h('span.pill.danger', {
        text: `⚠️ ${failed.length} 个申请失败（可重试）`,
        title: '失败条目已保留域名 / 校验方式 / DNS 服务商；点该行的「重试」即可，不需要重新填写',
      }) : null,
      expiring ? h('span.pill.warn', { text: `⚠️ ${expiring} 张 30 天内到期`, title: '续期窗口是到期前 30 天，点该行的「续期」即可' }) : null,
      running ? h('span.pill.brand', { text: `⟳ ${running} 个任务进行中` }) : null,
      h('button.btn.btn-sm', { id: 'zp-cert-refresh', text: '⟳ 刷新', onclick: load }),
      h('button.btn.btn-sm.btn-primary', { id: 'zp-cert-apply', text: '➕ 申请证书', onclick: () => applyModal() }),
    );
  }

  function renderList(certs) {
    clear(listBox);
    if (!certs.length) {
      appendAll(listBox, h('div.empty', [
        h('div.big', { text: '🔐' }),
        h('h4', { text: '还没有证书' }),
        h('p', { text: '还没有证书，点右上角申请。' }),
        h('p.hint', { text: '域名已经解析到本机、80 端口可达时选 http-01；要申请 *.example.com 泛域名则选 dns-01 并填 DNS 服务商凭据。' }),
        h('div', { style: { marginTop: '16px' } }, [
          h('button.btn.btn-primary', { text: '申请第一张证书', onclick: () => applyModal() }),
        ]),
      ]));
      return;
    }

    const tbody = h('tbody', certs.map((c) => (c.status === 'failed' ? failedRow(c) : issuedRow(c))));

    appendAll(listBox, h('div', { style: { overflowX: 'auto' } }, [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '主域名' }), h('th', { text: '覆盖域名' }), h('th', { text: '签发机构' }),
          h('th', { text: '到期时间' }), h('th', { text: '验证方式' }), h('th', { text: 'CA' }), h('th', { text: '操作' }),
        ])]),
        tbody,
      ]),
    ]));
  }

  // issuedRow 是已签发证书的行；failedRow 是「申请失败、可重试」的行。
  // 两行共用同一套表头（多了 status 字段区分），前端只需要一套列布局。
  function issuedRow(c) {
    return h('tr', [
      h('td', [
        h('div', { style: { fontWeight: '600' }, text: c.primary }),
        c.needsRenewal ? h('div', { style: { marginTop: '3px' } }, [h('span.pill.warn', { text: '需要续期' })]) : null,
        // 已签发的证书 + 上次重签失败：旧证书仍在服役，只加一个可解释的小徽标。
        c.lastError ? h('div', { style: { marginTop: '3px' } }, [
          h('span.pill.danger', {
            text: '上次申请失败',
            title: c.lastError + (c.lastErrorAt ? '（' + c.lastErrorAt + '）' : ''),
          }),
        ]) : null,
      ]),
      h('td', [
        h('div', {
          style: { fontSize: '12px', color: 'var(--text-dim)', maxWidth: '260px', wordBreak: 'break-all' },
          text: c.domains.join(', '),
        }),
      ]),
      h('td', { text: c.issuer || '—' }),
      h('td', [
        h('div', { style: { fontSize: '12px' }, text: c.notAfter || '—' }),
        h('div', { style: { marginTop: '3px' } }, [daysPill(c.daysLeft)]),
      ]),
      h('td', c.challenge
        ? h('span.pill', { text: c.challenge, title: c.challenge === 'dns-01' ? 'DNS TXT 校验，支持泛域名' : 'HTTP 文件校验，需要 80 端口可达' })
        : h('span', { text: '—' })),
      h('td', { text: c.ca || '—' }),
      h('td', h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm', { text: '🔄 续期', onclick: () => renew(c) }),
        h('button.btn.btn-sm.btn-danger', { text: '删除', onclick: () => remove(c) }),
      ])),
    ]);
  }

  function failedRow(c) {
    const err = c.lastError || '（没有记录具体的错误信息，详情见任务中心）';
    return h('tr', [
      h('td', [
        h('div', { style: { fontWeight: '600' }, text: c.primary }),
        h('div', { style: { marginTop: '3px', display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
          h('span.pill.danger', { text: '申请失败，可重试' }),
          c.failures > 1 ? h('span.pill.warn', { text: `已失败 ${c.failures} 次` }) : null,
        ]),
      ]),
      h('td', [
        h('div', {
          style: { fontSize: '12px', color: 'var(--text-dim)', maxWidth: '260px', wordBreak: 'break-all' },
          text: c.domains.join(', '),
        }),
      ]),
      h('td', { text: '—' }),
      h('td', [
        h('div', { style: { fontSize: '12px' }, text: c.lastErrorAt || c.updatedAt || '—' }),
        h('div', {
          style: {
            fontSize: '11.5px', color: 'var(--danger)', maxWidth: '320px', marginTop: '3px', wordBreak: 'break-word',
          },
          title: err,
          text: err,
        }),
      ]),
      h('td', c.challenge
        ? h('span.pill', { text: c.challenge, title: c.challenge === 'dns-01' ? 'DNS TXT 校验，支持泛域名' : 'HTTP 文件校验，需要 80 端口可达' })
        : h('span', { text: '—' })),
      h('td', { text: c.ca || '—' }),
      h('td', h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm.btn-primary', {
          text: '🔁 重试',
          title: '用保存的域名 / 校验方式 / DNS 服务商 + 服务端已存凭据直接重签，不需要重新填写',
          onclick: () => retry(c),
        }),
        h('button.btn.btn-sm', {
          text: '✏️ 修改后重试',
          title: '打开申请表单并预填上次的内容，改完再提交',
          onclick: () => applyModal(prefillFrom(c)),
        }),
        h('button.btn.btn-sm.btn-danger', { text: '删除', onclick: () => remove(c) }),
      ])),
    ]);
  }

  // prefillFrom 把失败条目转成申请表单的预填值。
  // 只带域名 / 邮箱 / CA / 校验方式 / DNS 服务商**名字**（没有凭据值）。
  function prefillFrom(c) {
    return {
      prefilled: true,
      domains: c.domains.join('\n'),
      email: c.email,
      ca: c.ca,
      challenge: c.challenge,
      dnsProvider: c.dnsProvider,
    };
  }

  function renew(c) {
    taskCenter.start({
      kind: 'cert',
      // target 与申请用同一套前缀，避免"申请"和"续期"同一个域名时并发跑两个任务
      target: 'cert:' + c.primary,
      title: `续期证书 ${c.primary}`,
      start: () => api.certRenew(c.primary),
      onDone: (m) => { if (m.status === 'succeeded') load(); },
    });
  }

  // retry 用保存的失败条目直接重签：请求体为空，凭据全部在服务端。
  function retry(c) {
    taskCenter.start({
      kind: 'cert',
      target: 'cert:' + c.primary,
      title: `重试申请证书 ${c.primary}`,
      start: () => api.certRetry(c.primary),
      onDone: (m) => { if (m.status === 'succeeded') load(); },
    });
  }

  async function remove(c) {
    const failed = c.status === 'failed';
    const ok = await confirmBox(
      failed
        ? `删除失败的申请记录「${c.primary}」？\n\n`
          + `覆盖域名：${c.domains.join(', ')}\n`
          + '删除后这条记录不再出现在列表里；需要时可以在「申请证书」里重新申请。'
        : `删除证书「${c.primary}」？\n\n`
          + `覆盖域名：${c.domains.join(', ')}\n`
          + '删除后引用它的站点会失去 HTTPS（这些站点会回落到只监听 80 端口）。\n'
          + '证书与私钥文件会从磁盘移除；需要时可以重新申请。',
      { title: failed ? '删除申请记录' : '删除证书', danger: true, okText: failed ? '删除记录' : '删除证书' },
    );
    if (!ok) return;
    try {
      await api.certDelete(c.primary);
      toast(failed ? '失败申请记录已删除' : '证书已删除', 'ok');
      load();
    } catch (e) {
      toast('删除失败：' + e.message, 'err', 10000);
    }
  }

  // ---------------- 申请表单 ----------------

  function applyModal(preset = null) {
    // preset 兼容两种调用：字符串（只预填域名）或对象（失败条目「修改后重试」）。
    // 对象里**没有凭据**：只有域名/邮箱/CA/校验方式/DNS 服务商名字。
    const pre = typeof preset === 'string' ? { domains: preset } : (preset || {});
    const domains = h('textarea.textarea', {
      id: 'zp-cert-domains',
      placeholder: 'example.com\nwww.example.com\n*.example.com',
      value: pre.domains || '',
      style: { minHeight: '92px' },
    });
    const email = h('input.input', {
      id: 'zp-cert-email', type: 'email', placeholder: 'you@example.com', autocomplete: 'email', value: pre.email || '',
    });
    const caSel = h('select.select', { id: 'zp-cert-ca' }, CA_OPTIONS.map((o) => h('option', { value: o.value, text: o.label })));
    const chalSel = h('select.select', { id: 'zp-cert-challenge' }, CHALLENGE_OPTIONS.map((o) => h('option', { value: o.value, text: o.label })));
    // 预填 CA / 校验方式：只在该值确实在选项里时才选，否则保持默认（避免空选择）。
    if (pre.ca && CA_OPTIONS.some((o) => o.value === pre.ca)) caSel.value = pre.ca;
    if (pre.challenge && CHALLENGE_OPTIONS.some((o) => o.value === pre.challenge)) chalSel.value = pre.challenge;
    const chalHint = h('div.hint');
    const emailHint = h('div.hint', { text: '用于证书到期提醒与 CA 账号注册，建议填真实邮箱。' });

    // ---- dns-01 才出现的服务商区域 ----
    const provSel = h('select.select', { id: 'zp-dns-provider' });
    const provHint = h('div.hint');
    const fieldsBox = h('div', { id: 'zp-dns-fields' });
    const dnsRow = h('div.field', { id: 'zp-dns-row', style: { display: 'none' } }, [
      h('label', { text: 'DNS 服务商' }),
      provSel,
      provHint,
      fieldsBox,
    ]);

    const summary = h('div.hint');

    function currentChallenge() { return CHALLENGE_OPTIONS.find((o) => o.value === chalSel.value); }

    function renderChallenge() {
      const opt = currentChallenge() || {};
      chalHint.textContent = opt.hint || '';
      const isDns = chalSel.value === 'dns-01';
      dnsRow.style.display = isDns ? '' : 'none';
      if (isDns) loadProviders();
    }

    function renderProviderFields() {
      clear(fieldsBox);
      const p = providersCache.find((x) => x.name === provSel.value);
      if (!p) return;
      // 从失败条目预填时：凭据已经在服务端，说明"留空即沿用"，避免用户以为必须重填。
      if (pre.dnsProvider && pre.dnsProvider === provSel.value) {
        appendAll(fieldsBox, h('div.hint', {
          text: `「${p.label || provSel.value}」的凭据已保存在服务端（接口不回显）；这次不改凭据的话，下面留空即可沿用。`,
        }));
      }
      if (!p.fields.length) {
        appendAll(fieldsBox, h('div.hint', {
          text: `${p.label} 没有声明额外环境变量；若申请失败，请查看任务日志里 CA 的报错。`,
        }));
        return;
      }
      appendAll(fieldsBox, h('div.hint', {
        text: '下面是这个服务商需要的凭据（字段由后端接口给出，面板不写死）。'
          + '凭据只用于本次提交，不会回显、也不会保存到浏览器。',
      }));
      p.fields.forEach((f) => {
        appendAll(fieldsBox, h('div.field', [
          h('label', { text: f.label + (f.required ? '' : '（可选）') }),
          // type=password：DNS API Token 等同于账号权限，不能在屏幕上明文停留。
          h('input.input', {
            type: 'password',
            autocomplete: 'off',
            'data-env-key': f.key,
            placeholder: f.key,
          }),
          h('div.hint', { text: '环境变量名：' + f.key }),
        ]));
      });
    }

    function loadProviders() {
      // 幂等：清单只在第一次真正请求，之后直接复用（它只是"输入框的模板"，
      // 不含任何用户输入）。in-flight 也要挡住，否则打开两个窗会重复请求。
      if (providersLoaded) { renderProviders(); return Promise.resolve(); }
      if (providersLoading) return providersLoading;
      provHint.textContent = '正在读取 DNS 服务商清单…';
      provHint.style.color = '';
      providersLoading = (async () => {
        try {
          providersCache = normalizeProviders(await api.certDnsProviders());
          providersError = '';
        } catch (e) {
          providersCache = [];
          providersError = e.message;
        }
        providersLoaded = true;
        renderProviders();
      })();
      return providersLoading;
    }

    function renderProviders() {
      clear(provSel);
      if (providersError) {
        provHint.textContent = '读取 DNS 服务商失败：' + providersError
          + '（可以用 http-01 验证，或先在 DNS 服务商处手动加 TXT 记录）';
        provHint.style.color = 'var(--danger)';
        return;
      }
      provHint.style.color = '';
      if (!providersCache.length) {
        provHint.textContent = '后端没有返回任何 DNS 服务商。可以改用 http-01（不支持泛域名），或在服务商处手动加 TXT 记录。';
        return;
      }
      provHint.textContent = '选择服务商后，下面会按它需要的环境变量动态生成输入框。';
      providersCache.forEach((p) => provSel.append(h('option', { value: p.name, text: p.label })));
      // 预填失败条目里的服务商（只有后端清单里确实有它时才选，否则保持默认第一个）。
      if (pre.dnsProvider && providersCache.some((p) => p.name === pre.dnsProvider)) {
        provSel.value = pre.dnsProvider;
      }
      renderProviderFields();
    }

    chalSel.addEventListener('change', renderChallenge);
    provSel.addEventListener('change', renderProviderFields);
    renderChallenge();
    // 预选清单里第一个服务商（数据一到就渲染字段，不需要用户多点一次）
    loadProviders();

    function collectEnv() {
      const env = {};
      fieldsBox.querySelectorAll('input[data-env-key]').forEach((inp) => {
        const k = inp.getAttribute('data-env-key');
        if (k) env[k] = inp.value;
      });
      return env;
    }

    function updateSummary() {
      const list = splitDomains(domains.value);
      const wild = list.filter(isWildcard);
      if (!list.length) { summary.textContent = ''; return; }
      const parts = [`将申请 ${list.length} 个域名：${list.join(', ')}`];
      if (wild.length) parts.push(`其中 ${wild.length} 个泛域名必须用 dns-01`);
      parts.push(`验证方式：${chalSel.value}，CA：${(CA_OPTIONS.find((o) => o.value === caSel.value) || {}).label || caSel.value}`);
      summary.textContent = parts.join(' · ');
    }
    domains.addEventListener('input', updateSummary);
    caSel.addEventListener('change', updateSummary);
    chalSel.addEventListener('change', updateSummary);
    updateSummary();

    const submit = h('button.btn.btn-primary', { id: 'zp-cert-submit', text: '提交申请（后台任务）' });

    const body = h('div', [
      pre.prefilled ? h('div.hint', {
        style: { color: 'var(--warn)', marginBottom: '8px' },
        text: '已按上次失败的申请预填域名 / 邮箱 / 校验方式 / DNS 服务商；'
          + 'DNS 凭据保存在服务端，不改的话留空即可沿用。改完点下面的按钮重新提交。',
      }) : null,
      h('div.field', [h('label', { text: '域名' }), domains,
        h('div.hint', { text: '多个域名用换行或逗号分隔；第一个非泛域名会作为这张证书的主域名（也是列表里的标识）。' })]),
      h('div.field', [h('label', { text: '邮箱' }), email, emailHint]),
      h('div.field', [h('label', { text: 'CA（证书颁发机构）' }), caSel,
        h('div.hint', { text: '先用 staging 验证域名/端口是否真的配好，通过了再换成正式 CA；正式环境对同一域名有签发频率限制。' })]),
      h('div.field', [h('label', { text: '验证方式' }), chalSel, chalHint]),
      dnsRow,
      summary,
    ]);

    const m = modal({
      title: pre.prefilled ? '修改后重新申请证书' : '申请 SSL 证书',
      wide: true,
      body,
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        submit,
      ],
    });

    submit.addEventListener('click', async () => {
      const list = splitDomains(domains.value);
      if (!list.length) { toast('请至少填一个域名', 'warn'); return; }
      const mail = email.value.trim();
      if (!mail || mail.indexOf('@') < 1) { toast('请填一个有效的邮箱（CA 注册与到期提醒要用）', 'warn'); return; }

      const isDns = chalSel.value === 'dns-01';
      if (!isDns && list.some(isWildcard)) {
        // 这是 ACME 协议本身的限制，不是后端的偏好：http-01 无法验证泛域名。
        // 在提交前拦住，比让用户等几十秒再看任务失败要好。
        toast('泛域名（*.example.com）只能用 dns-01 验证，请切换验证方式', 'warn', 9000);
        return;
      }
      const provider = isDns ? providersCache.find((x) => x.name === provSel.value) : null;
      if (isDns && !provider) {
        toast('请选择一个 DNS 服务商（http-01 不需要）', 'warn', 8000);
        return;
      }

      const payload = {
        domains: list,
        email: mail,
        challenge: chalSel.value,
        ca: caSel.value,
      };
      if (isDns) payload.dns = { name: provider.name, env: collectEnv() };

      submit.disabled = true;
      submit.textContent = '正在提交…';
      // 长任务走任务中心：它内部会处理"同 target 已在跑"的冲突、失败 toast，
      // 并把进度窗打开（进度窗关掉也不影响后台任务）。
      const id = await taskCenter.start({
        kind: 'cert',
        target: 'cert:' + list[0],
        title: `申请证书 ${list[0]}`,
        start: () => api.certApply(payload),
        onDone: (meta) => { if (meta.status === 'succeeded') load(); },
      });
      // 注意用 m.close()：footer 里的 close 只是 footer 回调的形参，
      // 在提交处理器这个作用域里根本不存在（写 close() 会 ReferenceError，
      // 而且是异步异常 —— 表现为"点了提交没反应、窗口不关"，非常难查）。
      if (id) m.close();
      else { submit.disabled = false; submit.textContent = '提交申请（后台任务）'; }
    });

    setTimeout(() => domains.focus(), 60);
    return m;
  }

  load();
}

// ---------------- 站点侧：选用已有证书（给 sites.js 的 SSL Tab 用） ----------------

/**
 * acmeSslSection 生成"使用 Let's Encrypt 证书"这一块 UI。
 *
 * 为什么做成"返回节点 + 自己异步填充"：站点详情是同步 render 出来的
 * （sites.js 的 tabSSL），不能 await 一个弹窗。这里先把骨架放进 DOM，
 * 证书列表回来后再原地更新。
 *
 * site 只需要 `domain`；onApplied 在证书成功应用后回调（让站点详情刷新）。
 */
export function acmeSslSection(site = {}, { onApplied } = {}) {
  const domain = str(site.domain);

  const statusBox = h('div', { style: { marginBottom: '10px' } });
  const body = h('div');

  const box = h('div', {
    style: {
      marginTop: '16px', paddingTop: '14px', borderTop: '1px solid var(--border-soft)',
    },
  }, [
    h('div.section-title', { text: "使用 Let's Encrypt 证书（面板内申请）" }),
    statusBox,
    body,
  ]);

  function renderLoading() {
    clear(statusBox); clear(body);
    appendAll(body, h('div.hint', { text: '正在读取已申请的证书…' }));
  }

  function renderError(msg) {
    clear(statusBox); clear(body);
    appendAll(statusBox, h('span.pill.danger', { text: '读取证书失败' }));
    appendAll(body,
      h('div.hint', { style: { color: 'var(--danger)' }, text: msg }),
      h('div', { style: { marginTop: '8px' } }, [
        h('button.btn.btn-sm', { text: '🔐 去证书页处理', onclick: () => { location.hash = '#/certs'; } }),
      ]));
  }

  function renderEmpty(hadFailed) {
    clear(statusBox); clear(body);
    appendAll(statusBox, h('span.pill.warn', { text: '还没有可用于该站点的证书' }));
    appendAll(body,
      h('div.hint', {
        text: hadFailed
          // 有失败条目时不能说"还没申请过"——那是谎报，也会让用户找不到重试入口。
          ? `面板里有申请失败的记录，但还没有可用的证书。到证书页点「重试」即可（域名 / 校验方式 / DNS 服务商已保存），`
            + `或重新申请一张覆盖 ${domain || '该域名'} 的证书。`
          : `面板里还没有申请过任何证书。点下面的「去申请」到证书页申请一张覆盖 ${domain || '该域名'} 的证书，`
            + '申请完成后回到这里选用即可。',
      }),
      h('div', { style: { marginTop: '8px', display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm.btn-primary', { text: hadFailed ? '🔁 去证书页重试' : '➕ 去申请证书', onclick: () => { location.hash = '#/certs'; } }),
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      ]));
  }

  function renderCerts(certs) {
    clear(statusBox); clear(body);
    const picked = pickCertForDomain(certs, domain);
    if (picked) {
      appendAll(statusBox, h('span.pill.ok', {
        text: `已按域名自动匹配：${picked.primary}`,
        title: picked.domains.join(', '),
      }));
    } else {
      appendAll(statusBox, h('span.pill.warn', {
        text: '没有与该域名匹配的证书',
        title: '下面仍可手动选一张，但浏览器可能因域名不匹配而报警告',
      }));
    }

    const sel = h('select.select', { id: 'zp-acme-cert-select' },
      certs.map((c) => h('option', {
        value: c.primary,
        text: `${c.primary}（${c.domains.join(', ')}）` + (c.daysLeft != null ? ` · 剩余 ${c.daysLeft} 天` : ''),
        selected: !!picked && c.primary === picked.primary,
      })));

    const info = h('div.hint');
    const renderInfo = () => {
      const c = certs.find((x) => x.primary === sel.value);
      if (!c) { info.textContent = ''; return; }
      const parts = [`覆盖：${c.domains.join(', ')}`];
      if (c.notAfter) parts.push(`到期：${c.notAfter}`);
      if (c.daysLeft != null) parts.push(`剩余 ${c.daysLeft} 天`);
      if (c.issuer) parts.push(`签发机构：${c.issuer}`);
      info.textContent = parts.join(' · ');
    };
    sel.addEventListener('change', renderInfo);
    renderInfo();

    appendAll(body,
      h('div.field', [h('label', { text: '选择证书' }), sel, info]),
      h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
        // 没有匹配证书时，"去申请"才是主操作：把"应用一张不匹配的证书"做成主按钮，
        // 等于把用户往一个浏览器必然报警告的结果上推（用户点了一定会疑惑）。
        h(picked ? 'button.btn.btn-sm.btn-primary' : 'button.btn.btn-sm', {
          text: '应用选中的证书',
          disabled: !picked,
          title: picked ? '' : '当前没有覆盖该域名的证书，请先申请',
          onclick: async () => {
            const c = certs.find((x) => x.primary === sel.value);
            if (!c) { toast('请先选择一张证书', 'warn'); return; }
            if (!await confirmBox(
              `把证书「${c.primary}」应用到站点 ${domain}？\n\n`
              + `覆盖域名：${c.domains.join(', ')}\n`
              + '如果这些域名不包含该站点域名，浏览器会提示证书不匹配。',
              { title: '应用证书' },
            )) return;
            try {
              // cert_primary 告诉后端"复用证书库里这一张"，而不是重新签发。
              await api.siteSSL(domain, { provider: 'acme', cert_primary: c.primary });
              toast('已应用证书并重载 nginx', 'ok');
              if (typeof onApplied === 'function') onApplied();
            } catch (e) {
              toast(e.message, 'err', 12000);
            }
          },
        }),
        h(picked ? 'button.btn.btn-sm' : 'button.btn.btn-sm.btn-primary', {
          text: '➕ 去申请新证书',
          onclick: () => { location.hash = '#/certs'; },
        }),
        h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      ]),
      !picked ? h('div.hint', {
        style: { color: 'var(--warn)' },
        text: '该域名还没有匹配的证书，建议先去证书页申请一张（http-01 或 dns-01）。',
      }) : null);
  }

  async function load() {
    renderLoading();
    let all;
    try {
      all = normalizeCerts(await api.certs());
    } catch (e) {
      renderError(e.message);
      return;
    }
    // 失败条目不能出现在"可应用的证书"里：它根本没有证书文件。
    const certs = all.filter((c) => c.status !== 'failed');
    if (!certs.length) { renderEmpty(all.length > 0); return; }
    renderCerts(certs);
  }

  load();
  return box;
}
