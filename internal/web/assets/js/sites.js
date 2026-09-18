// sites.js —— 网站管理页面。
//
// 交互设计参考宝塔但做了取舍：
//   - 列表页直接给出"配置是否存在""PHP 是否在跑""证书到期"这些真实状态，
//     而不是只显示数据库里的记录 —— 面板与 nginx 状态不一致是最常见的求助原因。
//   - 新建向导把「伪静态模板」和「PHP 版本」放在一起，因为它们互相影响
//     （Laravel/ThinkPHP 的运行目录要落到 public）。
//   - 每个站点提供"诊断"入口，一键做完 HTTP 探测、PHP 探针、证书检查、错误日志。

import { api } from './api.js';
import {
  h, clear, toast, modal, confirmBox, $,
} from './ui.js';
import { state, registerCleanup } from './app.js';
// 任务中心：LNMP 是长任务（十几分钟），提交后立刻返回 task_id，进度走 SSE。
// 这里**不自己写进度轮询**，也绝不改成同步请求 —— 用户一刷新就把 brew 杀了。
import { taskCenter } from './tasks.js';
// 站点 SSL Tab 里的「使用 Let's Encrypt 证书」整块 UI 放在 certs.js：
// 证书库与站点是两套生命周期，把它做成一个"自包含 + 自己异步填充"的组件，
// sites.js 只负责挂上去，避免两个页面对证书数据各写一份渲染逻辑。
import { acmeSslSection } from './certs.js';

let cache = null; // 站点列表数据（含预设与 PHP 版本）

// 站点详情面板当前所在的 Tab
let detailTab = 'basic';

// ---------------- 一键 LNMP 入口 ----------------
//
// 为什么这个入口在「网站管理」而不在「应用市场」（用户明确要求）：
// LNMP 是网站功能的地基（新建站点、伪静态、SSL、数据库都依赖 nginx / PHP / MySQL），
// 用户往往是在这一页才发现"环境还没装"；应用市场里放的是**单个应用**，
// 而 LNMP 是组合动作（三件套 + 默认站点 / vhosts / 系统级守护进程等收尾），
// 不是一张可安装的卡片（目录里也没有 ID=lnmp 的条目，见 services/catalog.go）。
//
// LNMP_HINT 是按钮上的 tooltip（鼠标悬停就能看完的那段话）。
//
// 2026-09-19 起版本由用户在弹窗里选，所以这里不再写死 "PHP 8.2 + MySQL 8.4" ——
// 写死的话，用户选了 8.4 再看提示就会发现"提示与实际不符"。
const LNMP_HINT = '一键 LNMP 会先确保运行依赖（命令行开发者工具 CLT / Homebrew），'
  + '再安装 nginx + PHP + MySQL + phpMyAdmin（数据库管理界面），'
  + '并完成默认站点、vhosts 目录、MySQL 初始化、系统级守护进程等收尾工作。'
  + '点开会先让你选 nginx / PHP / MySQL 各自的版本。'
  + '全程约十几分钟（取决于网络与 Homebrew 下载/编译速度）。'
  + '提交后立刻返回任务号，进度在「任务中心」实时显示 —— 关掉窗口、切换页面都不会中断安装。';

// LNMP_MYSQL_HINT 是 MySQL root 口令的说明（用户明确要求"在这个界面上显著说明"）。
//
// 任务中心里还有一条同义的常驻提示条（tasks.js，按标题 /LNMP/i 匹配），
// 两处都在：用户决定点确认之前就要知道"装 MySQL 会问口令、可以不干预"，
// 而不是等任务开起来才看到。
const LNMP_MYSQL_HINT = '装 MySQL 时会问一次 root 口令（在任务中心里，60 秒不回答就自动生成强随机口令）'
  + ' —— **可以不干预**：装完后到「数据库 → 账号与权限」里直接点一下就能改成你想要的口令。';

// lnmpGroupOrder 是弹窗里组件分组的展示顺序（与后端 groups 的 key 对应）。
//
// 写在这里而不是直接用后端顺序：后端保证顺序（有测试锁死），但前端自己也该有
// 一个明确的顺序概念 —— 万一后端漏了一组，下面的"缺组"检查会如实报出来，
// 而不是悄悄少渲染一行。
const LNMP_GROUP_ORDER = [
  { key: 'nginx', label: 'Nginx', desc: 'Web 服务器（网站入口，只有一个版本）' },
  { key: 'php', label: 'PHP', desc: '站点解析 PHP 用；面板让每个版本监听自己的专属 socket，可以多版本共存' },
  { key: 'mysql', label: 'MySQL', desc: '数据库（面板的「数据库」页与 phpMyAdmin 依赖它）' },
];

/**
 * openLNMPDialog 打开"选版本"弹窗；只有用户点确认之后才会创建安装任务。
 *
 * 流程（用户 2026-09-19 的要求："弹窗出来先让用户选择各服务版本，
 * 而不是直接执行安装"）：
 *   1. 点按钮 → **先发 GET /market/lnmp-options**（只读，不装任何东西）；
 *   2. 三组（nginx / PHP / MySQL）各列出候选，默认选中推荐项，已安装的标出来；
 *   3. 用户确认 → 才 POST /market/install-lnmp（body 里是本次选择）→ 任务中心。
 *
 * 接口失败时**不弹空弹窗、也不偷偷用默认值安装**：明确报错，并给一颗
 * "用默认版本安装"的显式按钮（用户点了才算他的选择）。
 */
async function openLNMPDialog() {
  const state = {
    groups: null, // 后端给的分组
    err: '',
    busy: true,
    picked: {}, // group.key → 选中的 formula
  };
  let m = null;

  const box = h('div', { style: { minWidth: '520px', maxWidth: '620px' } });
  const body = h('div');
  const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end' } });
  body.append(box);
  foot.append(h('div', { style: { flex: '1' } }));
  foot.append(h('button.btn', { text: '取消', onclick: () => (m ? m.close() : null) }));

  // install 按钮是**唯一**发安装请求的入口。
  const installBtn = h('button.btn.btn-primary', {
    text: '开始安装',
    onclick: () => {
      const sel = currentSelection();
      if (!sel) return; // currentSelection 已经 toast 过原因
      if (m) m.close();
      // 提交交给任务中心：立刻返回 task_id、进度走 SSE、失败有 toast。
      // 绝不在这里同步 await 安装过程（那会让用户刷新就杀掉 brew）。
      taskCenter.start({
        kind: 'install',
        target: 'lnmp',
        title: '一键 LNMP',
        start: () => api.installLNMP(sel),
      });
    },
  });
  installBtn.disabled = true;
  foot.append(installBtn);

  // currentSelection 把界面上的选择读成 {nginx, php, mysql}；不完整时 toast 并返回 null。
  function currentSelection() {
    const sel = { nginx: state.picked.nginx, php: state.picked.php, mysql: state.picked.mysql };
    const missing = LNMP_GROUP_ORDER.filter((g) => !sel[g.key]).map((g) => g.label);
    if (missing.length) {
      toast('至少要选：' + missing.join('、') + '（三件套必须各选一个版本）', 'warn');
      return null;
    }
    return sel;
  }

  function render() {
    clear(box);
    clear(foot);
    foot.append(h('div', { style: { flex: '1' } }));
    foot.append(h('button.btn', { text: '取消', onclick: () => (m ? m.close() : null) }));
    foot.append(installBtn);

    if (state.busy) {
      box.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取各组件可选版本…' })]));
      installBtn.disabled = true;
      return;
    }
    if (state.err) {
      // 明确报错 + 显式默认入口。**不**自动用默认值装：那是替用户做决定，
      // 用户以为面板"弹窗没出来就装好了"，实际装了他没选的版本。
      box.append(h('div', [
        h('p', { style: { color: 'var(--danger, #d33)', marginBottom: '8px' },
          text: '读取可选版本失败：' + state.err }),
        h('p.hint', { text: '没有拿到候选版本，所以面板**不会**擅自开始安装。' }),
        h('p.hint', { text: '你可以点「重试」再取一次；或者明确选择"用默认版本安装"' +
          '（nginx + PHP 8.2 + MySQL 8.4）。' }),
        h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px' } }, [
          h('button.btn', { text: '重试', onclick: () => load() }),
          h('button.btn.btn-primary', {
            text: '用默认版本安装（nginx + PHP 8.2 + MySQL 8.4）',
            onclick: () => {
              // 显式默认：body 里仍然带上这三个 formula（不是"留空让后端决定"），
              // 这样日志与审计里能看出"用户就是选的默认"。
              const def = { nginx: 'nginx', php: 'php@8.2', mysql: 'mysql@8.4' };
              if (m) m.close();
              taskCenter.start({
                kind: 'install',
                target: 'lnmp',
                title: '一键 LNMP',
                start: () => api.installLNMP(def),
              });
            },
          }),
        ]),
      ]));
      installBtn.disabled = true;
      return;
    }

    box.append(
      h('p.hint', { style: { marginBottom: '8px' },
        text: '三件套各选一个版本。取消或直接关闭弹窗都**不会**安装任何东西，' +
          '安装任务只有点「开始安装」之后才会创建。' }),
      h('p.hint', { style: { marginBottom: '12px' }, text: LNMP_MYSQL_HINT }),
    );

    const byKey = {};
    (state.groups || []).forEach((g) => { byKey[g.key] = g; });

    LNMP_GROUP_ORDER.forEach((meta) => {
      const g = byKey[meta.key];
      const row = h('div', { style: { marginBottom: '14px' } });
      row.append(h('div', { style: { fontWeight: '600', marginBottom: '4px' },
        text: meta.label + (g && g.label ? '（' + g.label + '）' : '') }));
      row.append(h('div.hint', { style: { marginBottom: '6px' }, text: meta.desc }));

      if (!g || !Array.isArray(g.options) || !g.options.length) {
        // 后端没给这一组（或一个候选都没有）：如实说，并**禁用**安装按钮。
        // 静默跳过会让用户以为"这个组件不用选"，最后 400 的错在提交后才出现。
        row.append(h('div.hint', { style: { color: 'var(--danger, #d33)' },
          text: '面板没有取到「' + meta.label + '」的可选版本（应用目录里可能没有这一项）。' +
            '请到「应用市场 → 网站环境」里确认，或先点「重试」。' }));
        box.append(row);
        return;
      }
      if (!state.picked[meta.key]) {
        // 默认选中后端给的 recommended/selected（PHP 8.2 为推荐）
        const def = g.options.find((o) => o.recommended) || g.options.find((o) => o.formula === g.selected) || g.options[0];
        state.picked[meta.key] = def.formula;
      }

      const name = 'zp-lnmp-' + meta.key;
      g.options.forEach((o) => {
        const input = h('input', {
          type: 'radio',
          name,
          value: o.formula,
          checked: state.picked[meta.key] === o.formula,
        });
        input.addEventListener('change', () => {
          if (input.checked) {
            state.picked[meta.key] = o.formula;
            syncInstallLabel();
          }
        });
        // 已安装的组件必须一眼看得出来：用户重跑一键 LNMP 时最关心
        // "会不会动我已经装好的东西"（幂等会跳过重装，但界面得先说清楚）。
        const badges = [];
        if (o.recommended) badges.push(h('span.pill', { text: '推荐' }));
        if (o.installed) badges.push(h('span.pill.ok', { text: '已安装（本次会跳过重装）' }));
        row.append(h('label', {
          style: { display: 'flex', gap: '8px', alignItems: 'flex-start', margin: '4px 0', cursor: 'pointer' },
        }, [
          input,
          h('div', { style: { flex: '1' } }, [
            h('div', [
              h('span', { text: o.name || o.formula }),
              h('span.hint', { text: '（' + o.formula + '）', style: { marginLeft: '6px' } }),
              ...badges,
            ]),
            o.summary ? h('div.hint', { text: o.summary }) : null,
            o.note ? h('div.hint', { text: o.note }) : null,
          ]),
        ]));
      });
      box.append(row);
    });

    // 三组都齐了才允许提交（后端也会校验一次，返回 400 + 原因）。
    const complete = LNMP_GROUP_ORDER.every((g) => state.picked[g.key] && byKey[g.key]);
    installBtn.disabled = !complete;
    syncInstallLabel();
  }

  // syncInstallLabel 让按钮上写着"这次会装什么" —— 用户在点之前就能核对。
  function syncInstallLabel() {
    const labels = LNMP_GROUP_ORDER
      .map((g) => {
        const f = state.picked[g.key];
        if (!f) return '';
        return f.includes('@') ? g.label + ' ' + f.split('@')[1] : f;
      })
      .filter(Boolean);
    installBtn.textContent = labels.length ? '开始安装（' + labels.join(' + ') + '）' : '开始安装';
  }

  async function load() {
    state.busy = true;
    state.err = '';
    render();
    try {
      const res = await api.lnmpOptions();
      const groups = (res && res.groups) || [];
      if (!groups.length) throw new Error('接口没有返回任何候选组件');
      state.groups = groups;
      state.busy = false;
    } catch (e) {
      state.busy = false;
      state.err = (e && e.message) || String(e);
    }
    render();
  }

  m = modal({
    title: '一键 LNMP：选择要安装的版本',
    body,
    footer: foot,
    // 弹窗宽度：三组选项要并排看清楚
    wide: true,
  });
  render();
  load();
}

// ---------------- 网站环境状态（现实判据）----------------
//
// 2026-09 产品把"运行依赖"（CLT / Homebrew / ffmpeg，跨应用）与"网站环境"
// （nginx + PHP + MySQL + phpMyAdmin，只服务本站）拆成两层。本页只管后者。
//
// 判据是**现实**，不是"面板数据库里有没有服务/站点记录"：
//   · 服务列表 GET /api/v1/services 里同名条目 state.running 为真；
//   · PHP 额外认 sites 接口的真实探测（php_versions[].running）——
//     真机实测：php-fpm 明明在跑，服务列表里却可能没有 php 条目，只认服务列表会误报。
// 读不到状态时**如实说读不到**，绝不把"读不到"当成"已就绪"。
const WEB_ENV_PARTS = ['nginx', 'PHP', 'MySQL'];


// startLNMP 是按钮的处理入口。
//
// 它**只打开弹窗**，绝不在这里发安装请求：用户必须先看到并确认版本选择。
// 真正的 POST /market/install-lnmp 在弹窗的「开始安装」按钮里发出
//（唯一出口），见 openLNMPDialog 的注释。
function startLNMP() {
  openLNMPDialog();
}

// lnmpButton 生成 LNMP 入口按钮。big=true 用于站点列表为空时的**大按钮**，
// 其余情况（列表非空）在工具条上给一个**次级按钮** —— 同一页不同时给两个大入口。
function lnmpButton(big) {
  return h('button.btn' + (big ? '.btn-primary' : '.btn-sm'), {
    text: '⚡ 一键 LNMP',
    title: LNMP_HINT,
    onclick: startLNMP,
  });
}

export function SitesView(content, ctx = {}) {
  clear(content);

  const listBox = h('div');
  // 网站环境状态行：本页的定位只跟 nginx/PHP/MySQL/phpMyAdmin 有关，
  // 与首页横幅的"运行依赖（CLT/Homebrew/ffmpeg）"是两层，必须分开说清。
  const webEnvLine = h('div.hint', {
    style: { marginBottom: '8px', fontSize: '12px', lineHeight: '1.7' },
  });
  const statusBar = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } });

  const toolbar = h('div.card-head', [
    h('h3', { text: '站点列表' }),
    h('div.spacer'),
    statusBar,
  ]);

  content.append(
    h('div.card', [
      toolbar,
      h('div.card-body.tight', [webEnvLine, listBox]),
    ]),
  );

  // refreshWebEnv 拉服务列表（health=0，只要状态不要逐条探测）判断网站环境。
  // 失败时**如实说读不到**，不沿用上一次的结论、更不假装就绪。
  async function refreshWebEnv() {
    clear(webEnvLine);
    webEnvLine.textContent = '网站环境：读取中…';
    let list = [];
    try {
      const res = await api.services(false);
      list = (res && res.list) || [];
    } catch (e) {
      webEnvLine.textContent = '网站环境：无法读取服务状态（' + e.message + '）';
      return;
    }
    // svcRunning 只在同名条目命中且 state.running 为真时算就绪
    //（PHP 允许多版本共存，任意一个版本在跑即可）。
    const svcRunning = (re) => list.some((s) => (re.test(String(s.name || ''))
      || re.test(String(s.display_name || ''))) && !!((s.state || {}).running));
    // PHP 的真实探测（比"服务记录"准）：sites 接口的 php_versions 直接给出 fpm 是否在跑。
    const phpLive = ((cache && cache.php_versions) || []).some((p) => p && p.running);
    const checks = {
      nginx: () => svcRunning(/^nginx/i),
      PHP: () => phpLive || svcRunning(/^php/i),
      MySQL: () => svcRunning(/^(mysql|mariadb|percona)/i),
    };
    const missing = WEB_ENV_PARTS.filter((label) => !checks[label]());
    clear(webEnvLine);
    webEnvLine.append(
      h('span', { text: '网站环境：' }),
      missing.length
        ? h('span.pill.warn', { text: '未就绪' })
        : h('span.pill.ok', { text: '已就绪' }),
      h('span', {
        // 用"未检测到运行中的"而不是"未运行"：面板没看见 ≠ 一定没在跑
        //（例如本机有一份不归面板管的 nginx）。只报"检测到了什么"，不替现实下结论。
        text: '（' + (missing.length ? '未检测到运行中的：' + missing.join('、') : 'nginx / PHP / MySQL 均在运行') + '）',
      }),
      h('div', {
        style: { marginTop: '3px' },
        text: '「⚡ 一键 LNMP」会先确保运行依赖（命令行开发者工具 CLT / Homebrew）'
          + '再装 nginx + PHP + MySQL + phpMyAdmin。运行依赖（含 ffmpeg）是所有 brew 应用的公共底座，'
          + '与网站无关，缺了会在首页提示。',
      }),
    );
  }

  async function load() {
    clear(listBox);
    listBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取站点…' })]));
    try {
      cache = await api.sites();
    } catch (e) {
      clear(listBox);
      listBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取站点失败' }),
        h('p', { text: e.message }),
      ]));
      // 站点接口挂了也要给网站环境状态：PHP 那条只能退回服务列表（cache 里没有 php_versions）。
      void refreshWebEnv();
      return;
    }
    renderStatus();
    renderList();
    // 放在 sites 之后：refreshWebEnv 会读 cache.php_versions 做 PHP 的真实判据。
    void refreshWebEnv();
  }

  function renderStatus() {
    clear(statusBar);
    const c = cache || {};
    const list = c.list || [];
    const phps = c.php_versions || [];
    const phpRunning = phps.filter((p) => p.running).length;
    // "需要修复"= 该版本还没被面板配置成独立端点（仍写着 Homebrew 出厂的 9000）。
    // 多版本共用 9000 正是"第二个版本起不来"的原因，必须在列表页就能看见。
    const phpNeedFix = phps.filter((p) => !p.listen_ok).length;
    // 用数组拼再展开：原生 Element.append 会把 null 渲染成文本 "null"
    // （工具栏上真的显示过一个 null，见 ui.js 的注释）。
    const bar = [
      h('span.pill', { text: `共 ${(c.list || []).length} 个站点` }),
    ];
    if (phpNeedFix) {
      bar.push(h('span.pill.warn', {
        text: `⚠️ ${phpNeedFix} 个 PHP 版本未配置独立端点`,
        title: phps.filter((p) => !p.listen_ok)
          .map((p) => `PHP ${p.version}：${p.conflict || `当前 ${p.pass || '未解析'}，应为 ${p.preferred_pass}`}`)
          .join('\n'),
      }));
    }
    bar.push(
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      // LNMP 入口：站点非空时放工具条上的**次级按钮**；空列表时改用中间的大按钮
      // （见 renderList 的空态），两处不同时出现，避免重复入口。
      ...(list.length ? [lnmpButton(false)] : []),
      // 「PHP-FPM 运行中 x/y 个」原来是一个独立药丸，与这个按钮说的是同一件事 ——
      // 用户要求合并：状态直接写进按钮文案，点开就是 PHP 环境面板。
      h('button.btn.btn-sm', {
        text: phps.length ? `🐘 PHP 环境 · ${phpRunning}/${phps.length} 运行中` : '🐘 PHP 环境',
        title: '查看已安装的 PHP 版本、各自的 FastCGI 端点与运行状态；一键修复端点\n'
          + phps.map((p) => `${p.version} ${p.running ? '运行中' : '未运行'} · ${p.pass || p.listen_err || '端点未解析'}`).join('\n'),
        onclick: phpEnvModal,
      }),
      h('button.btn.btn-sm', {
        text: '🧪 校验 nginx',
        title: '对 nginx 配置跑一次语法校验（nginx -t）',
        onclick: async () => {
          try {
            const r = await api.nginxTest();
            if (r.ok) toast('nginx 配置校验通过', 'ok');
            else toast('配置有问题：' + r.output, 'err', 12000);
          } catch (e) { toast(e.message, 'err'); }
        },
      }),
      h('button.btn.btn-sm', {
        text: '♻️ 重建全部配置',
        title: '按当前数据库状态重新生成所有站点的 nginx 配置（用于修复被手工改坏的配置）',
        onclick: async () => {
          if (!await confirmBox('将按模板重新生成所有站点的 nginx 配置并重载。\n\n你自己手工加在 vhost 里的内容会被覆盖（面板只保留数据库中的设置）。\n\n继续？', { title: '重建全部配置' })) return;
          try {
            const r = await api.siteReloadAll();
            const n = (r.rebuilt || []).length;
            if ((r.failed || []).length) toast(`重建 ${n} 个，失败 ${r.failed.length} 个：${r.failed[0]}`, 'warn', 12000);
            else toast(`已重建 ${n} 个站点配置`, 'ok');
            load();
          } catch (e) { toast(e.message, 'err', 9000); }
        },
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 新建站点', onclick: newSiteModal }),
    );
    statusBar.append(...bar);
  }

  function renderList() {
    clear(listBox);
    const list = (cache && cache.list) || [];
    if (!list.length) {
      listBox.append(h('div.empty', [
        h('div.big', { text: '🌐' }),
        h('h4', { text: '还没有站点' }),
        h('p', { text: '新建站点需要 nginx + PHP + MySQL。环境还没装的话，用「一键 LNMP」一次装好；' +
          '环境已就绪的话，直接新建站点即可。' }),
        // 空列表时 LNMP 是**大按钮**（用户要求：醒目但不过度）：
        // 没有环境的话，先建站点也跑不起来，所以它排在最前面。
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
          lnmpButton(true),
          h('button.btn', { text: '新建第一个站点', onclick: newSiteModal }),
        ]),
      ]));
      return;
    }

    const tbody = h('tbody', list.map((s) => h('tr', [
      h('td', [
        h('div', { style: { fontWeight: '600' }, text: s.domain }),
        s.aliases ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.aliases }) : null,
        s.remark ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.remark }) : null,
      ]),
      h('td', [
        h('span.pill' + (s.conf_exists ? '.ok' : '.danger'), {
          text: s.conf_exists ? (s.enabled ? '运行中' : '已停用') : '配置缺失',
          title: s.conf_exists ? 'nginx 配置文件存在' : '数据库有这个站点，但 nginx 配置文件不存在 —— 请点「重建全部配置」',
        }),
      ]),
      h('td', s.php_version ? h('span.pill.brand', { text: 'PHP ' + s.php_version }) : h('span.pill', { text: '静态' })),
      h('td', s.proxy_pass
        ? h('span.pill.brand', { text: '反代', title: s.proxy_pass })
        : h('span', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: presetLabel(s.rewrite) })),
      h('td', s.ssl_enabled
        ? h('span.pill.ok', { text: '🔒 ' + (s.ssl_provider || 'ssl'), title: s.ssl_expires ? '到期 ' + s.ssl_expires : '' })
        : h('span.pill.warn', { text: '未开启' })),
      h('td.mono', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.root }),
      h('td', [
        h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
          h('button.btn.btn-sm', { text: '管理', onclick: () => openDetail(s.domain) }),
          h('button.btn.btn-sm', { text: '诊断', onclick: () => runCheck(s.domain) }),
          h('button.btn.btn-danger.btn-sm', { text: '删除', onclick: () => delSite(s) }),
        ]),
      ]),
    ])));

    listBox.append(h('div', { style: { overflowX: 'auto' } }, [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '域名' }), h('th', { text: '状态' }), h('th', { text: 'PHP' }),
          h('th', { text: '路由' }), h('th', { text: 'SSL' }), h('th', { text: '运行目录' }), h('th', { text: '操作' }),
        ])]),
        tbody,
      ]),
    ]));
  }

  function presetLabel(name) {
    const p = (cache?.presets || []).find((x) => x.name === name);
    return p ? p.label : (name || '无');
  }

  // ---------- PHP 版本选项 ----------
  //
  // 版本下拉框的数据源是后端按 Homebrew 实际安装情况推导出来的列表
  // （GET /api/v1/php → cache.php_versions），既不是写死的，也不是自由文本。
  // 标签里必须把"没配好 / 没在跑"直接写出来：这两种状态都会让站点 502，
  // 而用户在"我明明选了 8.4"的时候根本想不到是端点没配或 fpm 没起来。
  function phpOptionLabel(p) {
    const tags = [];
    if (!p.listen_ok) tags.push('⚠️端点未配置');
    if (!p.running) tags.push('未运行');
    if (p.conflict) tags.push('⚠️端点冲突');
    const suffix = tags.length ? `（${tags.join('，')}）` : '';
    const pass = p.pass || p.preferred_pass || '';
    return `PHP ${p.version}${suffix}${p.is_default ? ' ← 默认' : ''}${pass ? ' · ' + pass : ''}`;
  }

  // phpFixButton 生成"把该版本改成独立端点并重启"的按钮。
  async function fixPHPEndpoint(version, reload) {
    if (!await confirmBox(
      `将把 PHP ${version} 的 php-fpm 配置改成它专属的 Unix socket 端点，并重启该服务。\n\n`
      + '为什么要改：Homebrew 的每个 PHP 版本出厂都监听 127.0.0.1:9000，'
      + '两个版本同时跑必然抢端口，后起的那个起不来。\n\n'
      + '面板会先备份原配置（www.conf.zizpanel.bak）再改写。继续？',
      { title: `修复 PHP ${version} 监听端点` },
    )) return;
    try {
      const r = await api.phpFixListen(version, true);
      toast(r.msg || `PHP ${version} 已就绪`, 'ok');
      if (reload) reload();
      load();
    } catch (e) {
      toast(e.message, 'err', 16000);
    }
  }

  // ---------- PHP 环境面板 ----------
  // 把"装了哪些版本、各自听在哪、跑没跑、要不要修"一次说清，
  // 并且**每个版本一个修复按钮** —— 多版本共存的排障全靠这一屏。
  async function phpEnvModal() {
    const box = h('div', [h('div.empty', [h('div.big', { text: '🐘' }), h('p', { text: '正在读取 PHP 版本…' })])]);
    modal({ title: 'PHP 多版本环境', wide: true, body: box });

    // data 用可变对象包一层：修复后要重新拉一次列表再重绘，
    // 直接给参数赋值在闭包里容易看漏（阅读时以为还是最初那份数据）。
    const state2 = {};
    try {
      state2.data = await api.phpList();
    } catch (e) {
      clear(box);
      box.append(h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
      return;
    }
    const render = () => {
      clear(box);
      const data = state2.data || {};
      const list = data.list || [];
      if (!list.length) {
        box.append(h('div.empty', [
          h('div.big', { text: '📦' }),
          h('h4', { text: '没有检测到 PHP' }),
          h('p', { text: '请先到「应用市场」安装 PHP（默认装 8.2；也可选 8.3 / 8.4）。' }),
        ]));
        return;
      }
      box.append(
        h('div.hint', {
          style: { marginBottom: '12px' },
          text: data.note || '每个 PHP 版本使用独立的 Unix socket 端点。',
        }),
        h('table.table', [
          h('thead', [h('tr', [
            h('th', { text: '版本' }), h('th', { text: 'FastCGI 端点' }),
            h('th', { text: '运行状态' }), h('th', { text: '配置' }), h('th', { text: '操作' }),
          ])]),
          h('tbody', list.map((p) => h('tr', [
            h('td', [h('strong', { text: 'PHP ' + p.version }),
              p.is_default ? h('span.pill.brand', { style: { marginLeft: '6px' }, text: '默认' }) : null]),
            h('td.mono', { style: { fontSize: '11.5px' }, text: p.pass || '（未解析）' }),
            h('td', p.running
              ? h('span.pill.ok', { text: '运行中' })
              : h('span.pill.danger', { text: '未运行', title: '该端点上没有进程监听；选了这个版本的站点会 502' })),
            h('td', p.listen_ok
              ? h('span.pill.ok', { text: '已是独立端点' })
              : h('span.pill.warn', {
                text: '需要修复',
                title: p.listen_err || p.conflict || `当前 ${p.pass}，应为 ${p.preferred_pass}`,
              })),
            h('td', [
              p.listen_ok && p.running ? h('span', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: '无需处理' })
                : h('button.btn.btn-sm.btn-primary', {
                  text: p.listen_ok ? '↻ 重启服务' : '🔧 修复端点并重启',
                  onclick: async () => {
                    await fixPHPEndpoint(p.version, async () => {
                      try { state2.data = await api.phpList(); } catch (e) { /* 拉取失败就保留旧数据 */ }
                      render();
                    });
                  },
                }),
            ]),
          ]))),
        ]),
        h('div.hint', { style: { marginTop: '12px' }, text: `套接字目录：${data.socket_dir || ''}` }),
        h('div.hint', { text: '注意：改写配置后必须重启对应的 php-fpm 才会生效；面板会在重启后确认端点真的有人在监听，否则如实报错。' }),
      );
    };
    render();
  }

  // ---------- 新建站点 ----------
  function newSiteModal() {
    const presets = cache?.presets || [];
    const phps = cache?.php_versions || [];
    const wwwRoot = cache?.www_root || '';

    const domain = h('input.input', { placeholder: '例如：demo.test' });
    const aliases = h('input.input', { placeholder: '可选，多个用英文逗号分隔' });
    const remark = h('input.input', { placeholder: '可选，便于自己识别' });
    const preset = h('select.select', presets.map((p) =>
      h('option', { value: p.name, text: p.label, selected: p.name === 'generic' })));
    // PHP 版本：只能从**已安装的版本**里选（后端按 Homebrew 实际安装情况列出），
    // 加一个"纯静态"选项。没有已安装版本时给出引导，而不是让人以为可以手填。
    const php = h('select.select', [
      h('option', { value: '', text: '纯静态（不解析 PHP）' }),
      ...phps.map((p) => h('option', {
        value: p.version,
        text: phpOptionLabel(p),
        selected: p.is_default,
      })),
    ]);
    if (!phps.length) {
      php.append(h('option', { value: '', text: '（本机还没有安装任何 PHP，请先到「应用市场」安装）' }));
    }
    const phpHint = h('div.hint', {
      text: phps.length
        ? phps.map((p) => `PHP ${p.version} ${p.running ? '运行中' : '未运行'} · ${p.pass || '端点未解析'}`).join('；')
        : '本机未检测到已安装的 PHP 版本。',
    });
    const proxy = h('input.input', { placeholder: '可选，如 http://127.0.0.1:3000（填了就是反向代理站点）' });
    const presetHint = h('div.hint', { text: '' });
    const rootPreview = h('code.code', { text: '' });

    const updatePreview = () => {
      const d = domain.value.trim().toLowerCase();
      const p = presets.find((x) => x.name === preset.value);
      const dir = p && p.public_dir ? `${wwwRoot}/${d}/${p.public_dir}` : `${wwwRoot}/${d}`;
      rootPreview.textContent = d ? dir : '（请输入域名）';
      if (p && p.public_dir) {
        presetHint.textContent = `${p.label}：运行目录会自动设为 public 子目录。${p.description}`;
      } else {
        presetHint.textContent = p ? p.description : '';
      }
      // 选了反代就隐藏 PHP 与伪静态的意义
      const isProxy = proxy.value.trim() !== '';
      php.disabled = isProxy;
      preset.disabled = isProxy;
    };
    domain.addEventListener('input', updatePreview);
    preset.addEventListener('change', updatePreview);
    proxy.addEventListener('input', updatePreview);
    updatePreview();

    const submit = async (close) => {
      const d = domain.value.trim().toLowerCase();
      if (!d) { toast('请输入域名', 'warn'); return; }
      try {
        const r = await api.siteCreate({
          domain: d,
          aliases: aliases.value.trim(),
          php_version: proxy.value.trim() ? '' : php.value,
          rewrite: proxy.value.trim() ? 'none' : preset.value,
          proxy_pass: proxy.value.trim(),
          remark: remark.value.trim(),
        });
        toast(`站点 ${d} 创建成功`, 'ok');
        close();
        load();
        if (r && r.root) setTimeout(() => openDetail(d), 300);
      } catch (e) {
        toast(e.message, 'err', 9000);
      }
    };

    const m = modal({
      title: '新建站点',
      body: h('div', [
        h('div.field', [h('label', { text: '域名 *' }), domain, h('div.hint', { text: '不需要输入 www，附加域名写在下一个字段' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases]),
        h('div.field', [h('label', { text: '运行目录' }), rootPreview, h('div.hint', { text: '目录会自动创建（已在 ~/www 下）' })]),
        h('div.field', [h('label', { text: '路由 / 伪静态' }), preset, presetHint]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php, phpHint,
          h('div.hint', { text: '选择"纯静态"时，nginx 会拒绝执行该站点下的 PHP 文件' })]),
        h('div.field', [
          h('label', { text: '反向代理（可选）' }),
          proxy,
          h('div.hint', { text: '填了之后整站转发到该地址，并自动带上 WebSocket 升级头。适合代理 Docker 服务。' }),
        ]),
        h('div.field', [h('label', { text: '备注' }), remark]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', { text: '创建站点', onclick: () => submit(close) }),
      ],
    });
    setTimeout(() => domain.focus(), 60);
  }

  // ---------- 站点详情 ----------
  async function openDetail(domain) {
    let data;
    try {
      data = await api.site(domain);
    } catch (e) {
      toast(e.message, 'err');
      return;
    }
    const site = data.site;
    const presets = data.presets || [];
    const phps = data.php_versions || [];

    const body = h('div');
    const tabBar = h('div', { style: { display: 'flex', gap: '6px', marginBottom: '14px', flexWrap: 'wrap' } });
    const tabs = [
      { id: 'basic', title: '基本设置' },
      { id: 'router', title: '路由与 PHP' },
      { id: 'ssl', title: 'SSL 证书' },
      { id: 'conf', title: '配置查看' },
      { id: 'log', title: '日志' },
    ];

    const renderTab = () => {
      clear(tabBar);
      tabs.forEach((t) => tabBar.append(h(`button.btn.btn-sm${detailTab === t.id ? '.btn-primary' : ''}`, {
        text: t.title,
        onclick: () => { detailTab = t.id; renderTab(); renderBody(); },
      })));
    };

    const renderBody = () => {
      clear(body);
      if (detailTab === 'basic') body.append(tabBasic());
      else if (detailTab === 'router') body.append(tabRouter());
      else if (detailTab === 'ssl') body.append(tabSSL());
      else if (detailTab === 'conf') body.append(tabConf());
      else body.append(tabLog());
    };

    function tabBasic() {
      const aliases = h('input.input', { value: site.aliases || '' });
      const remark = h('input.input', { value: site.remark || '' });
      const enabled = h('input', { type: 'checkbox', checked: site.enabled });
      const save = h('button.btn.btn-primary', {
        text: '保存',
        onclick: async () => {
          save.disabled = true;
          try {
            await api.siteUpdate(domain, {
              aliases: aliases.value.trim(), remark: remark.value.trim(), enabled: enabled.checked,
            });
            toast('已保存', 'ok');
            load();
          } catch (e) { toast(e.message, 'err', 9000); }
          finally { save.disabled = false; }
        },
      });
      return h('div', [
        h('div.field', [h('label', { text: '主域名' }), h('div', [h('code.code', { text: site.domain })]),
          h('div.hint', { text: '域名创建后不可修改（改域名等于新建站点，涉及目录与证书）' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases, h('div.hint', { text: '多个用英文逗号分隔，例如 www.demo.test, m.demo.test' })]),
        h('div.field', [h('label', { text: '运行目录' }), h('div', [h('code.code', { text: site.root })]),
          h('div.hint', { text: '由路由模板决定（Laravel/ThinkPHP 会指向 public 子目录）' })]),
        h('div.field', [h('label', { text: '备注' }), remark]),
        h('div.field', [
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            enabled, h('span', { style: { fontSize: '13px' }, text: '启用该站点（关闭后会从 nginx 移除配置）' }),
          ]),
        ]),
        save,
      ]);
    }

    function tabRouter() {
      const preset = h('select.select', presets.map((p) =>
        h('option', { value: p.name, text: p.label, selected: p.name === site.rewrite })));
      // 已安装版本 + 纯静态。若站点现存版本已经不在列表里（例如被卸载了），
      // 仍然把它显示出来，否则用户一保存就把它悄悄改成了别的版本。
      const curPHP = phps.find((p) => p.version === site.php_version);
      const phpOptions = [
        h('option', { value: '', text: '纯静态（不解析 PHP）', selected: !site.php_version }),
        ...phps.map((p) => h('option', {
          value: p.version,
          text: phpOptionLabel(p),
          selected: p.version === site.php_version,
        })),
      ];
      if (site.php_version && !curPHP) {
        phpOptions.push(h('option', {
          value: site.php_version,
          text: `PHP ${site.php_version}（已不在本机已安装列表中）`,
          selected: true,
        }));
      }
      const php = h('select.select', phpOptions);

      // 该站点当前 PHP 端点的健康提示：没配好 / 没在跑 / 端点冲突。
      // 这三种状态都会让站点 502，必须在用户保存前就说清楚。
      const phpc = phps.find((p) => p.version === site.php_version);
      const phpWarn = h('div');
      if (site.php_version && !site.proxy_pass) {
        const problems = [];
        if (data.fastcgi_err) problems.push(data.fastcgi_err);
        if (phpc && phpc.conflict) problems.push(phpc.conflict);
        if (phpc && !phpc.listen_ok) {
          problems.push(`PHP ${site.php_version} 还没被面板配置成独立端点（当前 ${phpc.pass || '未解析'}，应为 ${phpc.preferred_pass}）`);
        }
        if (phpc && !phpc.running) {
          problems.push(`PHP ${site.php_version} 的 php-fpm 没有在监听（端点 ${phpc.pass || '未解析'}），本站会返回 502`);
        }
        if (problems.length) {
          phpWarn.append(...[
            ...problems.map((t) => h('div', {
              style: {
                padding: '7px 10px', marginBottom: '6px', background: 'var(--danger-soft)',
                borderRadius: '6px', fontSize: '12.5px',
              },
              text: '• ' + t,
            })),
            h('button.btn.btn-sm.btn-primary', {
              text: `🔧 修复 PHP ${site.php_version} 端点（会重启该版本 fpm）`,
              onclick: () => fixPHPEndpoint(site.php_version),
            }),
          ]);
        }
      }
      const proxy = h('input.input', { value: site.proxy_pass || '', placeholder: '留空为普通站点；填写则整站反代' });
      const extra = h('textarea.textarea', {
        value: site.extra_conf || '',
        placeholder: '# 追加到 server 块内的 nginx 指令\n# 例如：\n# location = /health { return 200 "ok"; }',
        style: { minHeight: '130px' },
      });
      const hint = h('div.hint', { text: '' });
      const updateHint = () => {
        const p = presets.find((x) => x.name === preset.value);
        hint.textContent = p ? p.description + (p.public_dir ? `（运行目录会自动切到 ${p.public_dir}/）` : '') : '';
      };
      preset.addEventListener('change', updateHint);
      updateHint();

      const save = h('button.btn.btn-primary', {
        text: '保存并应用',
        onclick: async () => {
          save.disabled = true;
          try {
            await api.siteUpdate(domain, {
              rewrite: preset.value,
              php_version: proxy.value.trim() ? '' : php.value,
              proxy_pass: proxy.value.trim(),
              extra_conf: extra.value,
            });
            toast('已保存，nginx 已重载', 'ok');
            const fresh = await api.site(domain);
            Object.assign(site, fresh.site);
            load();
          } catch (e) { toast(e.message, 'err', 12000); }
          finally { save.disabled = false; }
        },
      });

      return h('div', [
        h('div.field', [h('label', { text: '路由 / 伪静态模板' }), preset, hint]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php,
          h('div.hint', {
            text: phps.length
              ? '每个版本监听各自的 FastCGI 端点（Unix socket），因此多版本可以共存。版本未运行或端点未配置都会返回 502。'
              : '本机未检测到已安装的 PHP 版本，请先到「应用市场」安装。',
          }),
          phpWarn]),
        h('div.field', [
          h('label', { text: '反向代理目标' }),
          proxy,
          h('div.hint', { text: '填写后本页的 PHP 与伪静态设置不再生效，整站转发到该地址。' }),
        ]),
        h('div.field', [h('label', { text: '自定义配置（高级）' }), extra,
          h('div.hint', { text: '内容会原样追加到 server 块内。写错会导致 nginx 校验失败并自动回滚，不会影响其它站点。' })]),
        save,
      ]);
    }

    function tabSSL() {
      const box = h('div');
      // 证书申请成功后要整块重画（状态行、按钮都会变），所以把"挂载点"包一层。
      const acmeSection = () => acmeSslSection(site, { onApplied: () => { render(); load(); } });
      const render = () => {
        clear(box);
        const cur = site.ssl_enabled;
        box.append(
          h('div', { style: { marginBottom: '14px' } }, [
            cur
              ? h('span.pill.ok', { text: `已开启 · ${site.ssl_provider || ''} · 到期 ${site.ssl_expires || '未知'}` })
              : h('span.pill.warn', { text: '未开启 HTTPS' }),
          ]),
        );
        if (cur) {
          box.append(
            h('dl.kv', [
              h('dt', { text: '证书路径' }), h('dd', { text: site.ssl_cert }),
              h('dt', { text: '私钥路径' }), h('dd', { text: site.ssl_key }),
              h('dt', { text: '签发方式' }), h('dd', { text: site.ssl_provider }),
              h('dt', { text: '到期时间' }), h('dd', { text: site.ssl_expires || '未知' }),
            ]),
            h('div', { style: { marginTop: '14px', display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
              h('button.btn.btn-primary', { text: '重新签发', onclick: () => issue() }),
              h('button.btn.btn-danger', {
                text: '关闭 HTTPS',
                onclick: async () => {
                  if (!await confirmBox('关闭后站点只监听 80 端口。继续？', { danger: true })) return;
                  try {
                    const r = await api.siteSSLDisable(domain);
                    Object.assign(site, r.site);
                    toast('已关闭 HTTPS', 'ok');
                    render(); load();
                  } catch (e) { toast(e.message, 'err', 9000); }
                },
              }),
            ]),
          );
          box.append(acmeSection());
          return;
        }
        box.append(
          h('div.hint', { style: { marginBottom: '14px' }, text: '开启 HTTPS 后，80 端口的请求会自动 301 跳转到 443。' }),
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            h('button.btn.btn-primary', { text: '用 mkcert 签发（推荐）', onclick: () => issue('mkcert') }),
            h('button.btn', { text: '自签证书', onclick: () => issue('self') }),
            h('button.btn', { text: '粘贴自有证书', onclick: () => manual() }),
          ]),
          h('div.hint', { style: { marginTop: '12px' } }, [
            h('div', { text: '• mkcert：使用本机 mkcert CA 签发。若已在系统信任该 CA，浏览器不会提示。' }),
            h('div', { text: '• 自签证书：无需任何依赖，浏览器会提示不受信任（点"继续访问"即可）。' }),
            h('div', { text: "• 需要浏览器信任的正式证书：用下面的「使用 Let's Encrypt 证书」——先在「SSL 证书」页申请，再回来选。" }),
          ]),
          acmeSection(),
        );
      };

      async function issue(provider = 'mkcert') {
        const btnText = provider === 'mkcert' ? 'mkcert' : '自签';
        try {
          const r = await api.siteSSL(domain, { provider });
          Object.assign(site, r.site);
          toast(`${btnText}证书已签发并应用`, 'ok');
          render(); load();
        } catch (e) {
          toast(e.message, 'err', 14000);
        }
      }

      function manual() {
        const cert = h('textarea.textarea', { placeholder: '-----BEGIN CERTIFICATE-----\n…', style: { minHeight: '110px' } });
        const key = h('textarea.textarea', { placeholder: '-----BEGIN PRIVATE KEY-----\n…', style: { minHeight: '110px' } });
        const m = modal({
          title: '粘贴自有证书',
          body: h('div', [
            h('div.field', [h('label', { text: '证书（含链）' }), cert]),
            h('div.field', [h('label', { text: '私钥' }), key]),
          ]),
          footer: (close) => [
            h('button.btn', { text: '取消', onclick: close }),
            h('button.btn.btn-primary', {
              text: '保存并应用',
              onclick: async () => {
                try {
                  const r = await api.siteSSL(domain, { provider: 'manual', cert: cert.value, key: key.value });
                  Object.assign(site, r.site);
                  toast('证书已应用', 'ok');
                  close(); render(); load();
                } catch (e) { toast(e.message, 'err', 12000); }
              },
            }),
          ],
        });
      }

      render();
      return box;
    }

    function tabConf() {
      // 用户明确要求（2026-09-17）：
      //   ① 站点管理里要能**直接编辑**配置文件（他要改默认端口，原来只能看）；
      //   ② 「面板生成的配置」与「磁盘上的实际配置」两个按钮是重复的，去掉；
      //   ③ 保存后**自动重载生效**。
      // 所以这里只剩一个编辑器：编辑的就是磁盘上那份（nginx 真正加载的），
      // 保存走 siteConfSave（后端 nginx -t → reload → 复核端口 → 失败回滚）。
      const editor = h('textarea.input', {
        spellcheck: 'false',
        style: {
          width: '100%', minHeight: '420px', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
          fontSize: '12.5px', lineHeight: '1.5', whiteSpace: 'pre', resize: 'vertical',
        },
      });
      let original = data.conf || '';
      editor.value = original;
      const dirty = () => editor.value !== original;

      const status = h('span.hint', { text: '' });
      const saveBtn = h('button.btn.btn-sm.btn-primary', {
        text: '💾 保存并重载',
        onclick: async () => {
          saveBtn.disabled = true;
          status.textContent = '正在写入并校验（nginx -t）…';
          try {
            await api.siteConfSave(domain, editor.value);
            original = editor.value;
            status.textContent = '';
            toast('配置已保存并重载生效', 'ok');
            const fresh = await api.site(domain);
            data.conf = fresh.conf; data.generated = fresh.generated;
            if (!dirty()) editor.value = data.conf || '';
            load();
          } catch (e) {
            // 失败时**保留用户写的内容**（不要用磁盘上的旧内容盖掉他的编辑），
            // 并如实说明后端已回滚、站点仍是旧配置。
            status.textContent = '保存失败（已回滚，站点仍在用修改前的配置）：' + e.message;
            toast(e.message, 'err', 20000);
          } finally {
            saveBtn.disabled = false;
          }
        },
      });

      return h('div', [
        h('div', { style: { display: 'flex', gap: '8px', marginBottom: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
          saveBtn,
          h('button.btn.btn-sm', {
            text: '↩ 撤销未保存的修改',
            onclick: () => { editor.value = original; status.textContent = ''; },
          }),
          h('button.btn.btn-sm', {
            // 把编辑器内容换成"面板按当前站点设置生成的内容"（只填充、不保存）：
            // 想看模板长什么样 / 想从模板改起时用，不改变"直接编辑磁盘文件"的语义。
            text: '🔄 载入面板生成的配置',
            title: '只填进编辑器，不保存；想恢复成面板模板时用',
            onclick: () => {
              editor.value = data.generated || '';
              status.textContent = '已载入面板生成的配置（尚未保存，点「保存并重载」才生效）';
            },
          }),
          h('button.btn.btn-sm', {
            text: '⚙️ 重新生成并应用',
            title: '按面板里的站点设置（别名 / PHP 版本 / 伪静态 / 自定义配置）重新生成整份配置并重载',
            onclick: async () => {
              try {
                await api.siteUpdate(domain, {});
                toast('已重新生成并重载', 'ok');
                const fresh = await api.site(domain);
                data.conf = fresh.conf; data.generated = fresh.generated;
                original = data.conf || '';
                editor.value = original;
                status.textContent = '';
                load();
              } catch (e) { toast(e.message, 'err', 12000); }
            },
          }),
          status,
        ]),
        editor,
        h('div.hint', { style: { marginTop: '10px' }, text: `配置文件路径：${data.conf_path}（就是上面编辑的这一份，nginx 加载的也是它）` }),
        h('div.hint', {
          text: '直接改这一份并保存 → 后端先跑 `nginx -t`，通过后重载，并复核新配置里的监听端口真的在应答；' +
            '任何一步失败都会自动回滚，站点不会变成打不开。' +
            '注意：点站点其它页签的「保存」（面板按模板重新生成）会覆盖这里的手工改动 —— ' +
            '想让改动在面板里持久化，请把要保留的片段写进站点设置里的「自定义配置」。',
        }),
      ]);
    }

    function tabLog() {
      const box = h('pre.logbox', { style: { maxHeight: '420px' }, text: '加载中…' });
      const kindSel = h('select.select', { style: { width: 'auto' } }, [
        h('option', { value: 'access', text: '访问日志' }),
        h('option', { value: 'error', text: '错误日志' }),
      ]);
      let timer = null;
      const loadLog = async () => {
        try {
          const r = await api.siteLog(domain, kindSel.value, 300);
          box.textContent = r.content || (r.msg || '（暂无日志）');
          box.scrollTop = box.scrollHeight;
        } catch (e) { box.textContent = '读取失败：' + e.message; }
      };
      kindSel.addEventListener('change', loadLog);
      loadLog();
      const auto = h('input', { type: 'checkbox' });
      auto.addEventListener('change', () => {
        if (auto.checked) timer = setInterval(loadLog, 3000);
        else if (timer) { clearInterval(timer); timer = null; }
      });
      registerCleanup(() => { if (timer) clearInterval(timer); });

      return h('div', [
        h('div', { style: { display: 'flex', gap: '8px', marginBottom: '10px', alignItems: 'center', flexWrap: 'wrap' } }, [
          kindSel,
          h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: loadLog }),
          h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '12.5px' } }, [
            auto, h('span', { text: '自动刷新（3 秒）' }),
          ]),
        ]),
        box,
        h('div.hint', { style: { marginTop: '10px' }, text: `日志目录：${data.log_dir}` }),
      ]);
    }

    renderTab();
    renderBody();

    modal({
      title: `站点：${domain}`,
      wide: true,
      body: h('div', [tabBar, body]),
    });
  }

  // ---------- 诊断 ----------
  async function runCheck(domain) {
    const box = h('div', [h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: '正在访问站点并检查…' })])]);
    const m = modal({ title: `诊断：${domain}`, wide: true, body: box });
    let r;
    try {
      r = await api.siteCheck(domain);
    } catch (e) {
      clear(box);
      box.append(h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
      return;
    }
    clear(box);
    const rows = [
      ['访问地址', r.url],
      ['HTTP 状态码', r.http_code || '无响应'],
      ['PHP 解析', r.php_works ? '正常' : (r.php_raw ? '❌ 源码被直接输出' : '未检测/不适用')],
      ['HTTPS', r.https ? '已开启' : '未开启'],
    ];
    if (r.cert_issuer) rows.push(['证书签发者', r.cert_issuer]);
    if (r.cert_expiry) rows.push(['证书到期', r.cert_expiry]);

    box.append(
      h('div', { style: { marginBottom: '14px' } }, [
        r.ok && !r.php_raw
          ? h('span.pill.ok', { text: '✅ 站点访问正常' })
          : h('span.pill.danger', { text: '⚠️ 发现问题' }),
      ]),
      h('dl.kv', rows.flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { text: v })])),
    );

    if (r.issues && r.issues.length) {
      box.append(
        h('div.section-title', { style: { marginTop: '18px' }, text: '发现的问题' }),
        h('div', r.issues.map((i) => h('div', {
          style: { padding: '7px 10px', marginBottom: '6px', background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12.5px' },
          text: '• ' + i,
        }))),
      );
    }
    if (r.hints && r.hints.length) {
      box.append(
        h('div.section-title', { style: { marginTop: '18px' }, text: '建议' }),
        h('div', r.hints.map((i) => h('div', {
          style: { padding: '7px 10px', marginBottom: '6px', background: 'var(--panel-2)', borderRadius: '6px', fontSize: '12.5px', color: 'var(--text-dim)' },
          text: '• ' + i,
        }))),
      );
    }
    if (r.error_log) {
      box.append(
        h('div.section-title', { style: { marginTop: '18px' }, text: '最近的错误日志' }),
        h('pre.logbox', { style: { maxHeight: '220px' }, text: r.error_log }),
      );
    }
  }

  async function delSite(s) {
    const hasFiles = true;
    const removeFiles = h('input', { type: 'checkbox' });
    const m = modal({
      title: '删除站点：' + s.domain,
      body: h('div', [
        h('div', { style: { marginBottom: '14px', fontSize: '13.5px', lineHeight: '1.8' } }, [
          h('div', { text: '将删除该站点的 nginx 配置，并从面板记录中移除。' }),
          h('div', { style: { color: 'var(--text-mute)', fontSize: '12.5px', marginTop: '4px' }, text: '站点根目录：' + s.root }),
        ]),
        hasFiles ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', fontSize: '13px' } }, [
          removeFiles,
          h('span', { text: '同时删除站点目录及其中所有文件（不可恢复！）' }),
        ]) : null,
        h('div.hint', { style: { marginTop: '10px' }, text: '不勾选则只删除配置，文件保留，方便之后重新绑定。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-danger', {
          text: '确认删除',
          onclick: async () => {
            try {
              const r = await api.siteDelete(s.domain, removeFiles.checked);
              toast(r.msg + (r.files ? '（' + r.files + '）' : ''), 'ok');
              close();
              load();
            } catch (e) { toast(e.message, 'err', 9000); }
          },
        }),
      ],
    });
  }

  registerCleanup(() => { });
  load();
}
