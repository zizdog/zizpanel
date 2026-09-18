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
// 手动编辑底层配置文件（nginx.conf / php.ini / my.cnf）的入口不复用第二套编辑器：
// 「⚙️ 配置文件」弹窗里的每一条都调 services.js 的 configFileModal ——
// 它读写走既有的文件接口（GET/POST /api/v1/files/read|write，含白名单与越界校验），
// 保存/重启的语义也只有那一份实现。
import { configFileModal } from './services.js';
// 「上传大小 / 执行时间」：与「面板设置」共用同一份实现（用户要求这类常用更改
// 必须是功能，而不是让用户去改配置原文件）。
import { uploadLimitsModal } from './views.js';
// 宝塔式「Nginx 管理」（服务 / 配置修改 / 性能调整 / 错误日志）。
import { nginxPanelModal } from './nginxpanel.js';

let cache = null; // 站点列表数据（含预设与 PHP 版本）

// defSite 是「默认站点」的真实状态（GET /api/v1/system/default-site）。
//
// 用户要求"面板安装完成后要建立一个默认静态站点"：面板启动时会自动建一次，
// 这个状态就是"到底建成了没有"的唯一显示来源 —— null = 还没读到（既不说成
// 建好了，也不说成失败）。
let defSite = null;

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

// LNMP_TITLE_TAIL 是「⚡ 一键 LNMP」按钮 tooltip 的后半句（用户 2026-09 指定的
// 逐字文案）。前半句由 lnmpHintTitle 按**真实探测**填：
//   · 有缺失 → 当前缺失"xxx"（多项用「、」连接，底座项用后端 missing 里的名字）；
//   · 读取中 → 正在读取环境状态…；
//   · 读不到 → 环境状态读取失败，可点开确认。
// 缺失项一变，整句跟着变（不写死任何一项）。
const LNMP_TITLE_TAIL = '：「⚡ 一键 LNMP」会确保环境完整，包括mac系统必要的底座'
  + '（如CLT / Homebrew），已安装的环境不会重复安装。';

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
  // noticeBox 是弹窗**顶部**那块"当前缺失"的显著提醒（用户要求：不是灰字小字）。
  // 它在 render() 里按最新事实重画：内容来自 refreshWebEnv 已探到的底座缺失
  // 与弹窗这次自己拉到的 groups —— 两处都是真实探测，不是猜的。
  const noticeBox = h('div');
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

  // renderNotice 画顶部"当前缺失"块。三种情况分开说，绝不含糊：
  //   · 有缺失（底座 + 整组未装的组件）→ 醒目列出，并说明已装的会跳过；
  //   · 底座层没读到 → 如实说"无法确认"，不假装完整；
  //   · 真的都齐了 → 写"环境已完整，无需安装"（用户手工从工具条点进来时兜住）。
  function renderNotice() {
    clear(noticeBox);
    const frame = {
      border: '1px solid var(--warn)', background: 'var(--warn-soft)',
      borderRadius: '8px', padding: '10px 12px', marginBottom: '12px',
    };
    const detail = h('div', { style: { marginTop: '4px' },
      text: '已安装的环境不会重复安装，本次会跳过。' });
    if (state.busy) {
      noticeBox.append(h('div', { style: frame }, [
        h('div', { style: { fontWeight: '700' }, text: '⏳ 正在确认环境是否完整…' }),
      ]));
      return;
    }
    if (state.err) {
      noticeBox.append(h('div', { style: frame }, [
        h('div', { style: { fontWeight: '700' },
          text: '⚠️ 环境状态读取失败，暂时无法确认缺失项 —— 请先点「重试」' }),
        detail,
      ]));
      return;
    }
    const parts = [];
    // 底座项用后端 missing 里的名字（命令行开发者工具 / Homebrew / ffmpeg）
    if (webEnv.basePhase === 'ready') parts.push(...webEnv.baseMissing);
    // 组件项用弹窗这次刚拉到的最新 groups（比页面初载时更可靠）
    parts.push(...missingComponents(state.groups));
    if (parts.length) {
      noticeBox.append(h('div', { style: frame }, [
        h('div', { style: { fontWeight: '700', fontSize: '14px' },
          text: '⚠️ 当前缺失：' + parts.join('、') }),
        detail,
      ]));
      return;
    }
    if (webEnv.basePhase === 'loading') {
      noticeBox.append(h('div', { style: frame }, [
        h('div', { style: { fontWeight: '700' },
          text: '⏳ 正在读取运行依赖（CLT / Homebrew / ffmpeg）状态…' }),
        detail,
      ]));
      return;
    }
    if (webEnv.basePhase !== 'ready') {
      noticeBox.append(h('div', { style: frame }, [
        h('div', { style: { fontWeight: '700' },
          text: '⚠️ 运行依赖（CLT / Homebrew / ffmpeg）状态读取失败，暂时无法确认缺失项' }),
        detail,
      ]));
      return;
    }
    noticeBox.append(h('div', { style: frame }, [
      h('div', { style: { fontWeight: '700', fontSize: '14px' }, text: '✅ 环境已完整，无需安装' }),
      detail,
    ]));
  }

  function render() {
    clear(box);
    clear(foot);
    box.append(noticeBox);
    renderNotice();
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
      h('p.hint', { style: { marginBottom: '8px' }, text: LNMP_HINT }),
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

// ---------------- 一键 LNMP 入口的判据（三层真实探测）----------------
//
// 入口要不要出现、"缺什么"怎么写，都必须有证据。三层各自记状态：
//   · 服务层  GET /services            → nginx 条目 state.running（nginx 运行/停止）
//   · 底座层  GET /system/base-env     → missing（命令行开发者工具 / Homebrew / ffmpeg）
//   · 组件层  GET /market/lnmp-options → 每组 options[].installed（真实 brew+记录+plist）
//
// **没读到 ≠ 已就绪**：任何一层没读到都不隐藏入口，而是显示入口并如实说"读不到"
//（隐藏入口等于对用户谎报"环境完整"）。三层都读到且都没有缺失时才隐藏。
function freshWebEnv() {
  return {
    servicesPhase: 'loading', // loading | ready | error
    servicesErr: '',
    // nginxState 只有四种取值，**没有证据绝不写"运行"、也不写"停止"**：
    //   loading → 还没读到；running / stopped → 服务列表里真有 nginx 条目；
    //   unknown → 接口失败，或服务列表里根本没有 nginx 条目。
    // 「没有条目」为什么算未知而不是停止：真机实测 nginx 在 :80 上跑着，
    // 面板的服务列表里却没有它（不归面板登记/不是面板装的）—— 写"停止"就是谎报。
    nginxState: 'loading',
    // runtime 是**运行体证据**（GET /api/v1/sites/runtime）：
    //   nginx / PHP / MySQL 各自 {running, evidence, registered, probe_error}。
    // 这是"网站在不在跑"的唯一权威来源 —— 服务列表只说明"归不归面板管"。
    runtimePhase: 'loading', // loading | ready | error
    runtime: null,
    runtimeErr: '',
    basePhase: 'loading', // loading | ready | error
    baseMissing: [],
    lnmpPhase: 'loading', // loading | ready | error
    lnmpGroups: [],
  };
}
let webEnv = freshWebEnv();
// envToken 是"页面代次"：切换页面后旧视图还在飞的探测结果必须作废，
// 否则上一页的结论会写进新页面的状态条（用户会看到状态闪成上一次的结论）。
let envToken = 0;

// missingComponents 从 lnmpOptions 的分组算出"整组一个版本都没装"的组件。
// 判据：整组 options 的 installed 全为 false 才算这个组件缺失；装了任意一个版本
// 就算已有（重跑一键 LNMP 时后端会跳过已装的部分，面板不重复安装）。
function missingComponents(groups) {
  const list = Array.isArray(groups) ? groups : [];
  const out = [];
  LNMP_GROUP_ORDER.forEach((meta) => {
    const g = list.find((x) => x && x.key === meta.key);
    if (!g || !Array.isArray(g.options) || !g.options.length) {
      // 目录里没有这个组件（面板拿不到任何可装版本）—— 按缺失列出，
      // 绝不因为"没数据"就当成已安装（那会让入口消失、用户以为环境齐了）。
      out.push(meta.label);
      return;
    }
    if (!g.options.some((o) => o && o.installed)) out.push(g.label || meta.label);
  });
  return out;
}

// envMissing 返回当前**有证据**的缺失项（底座缺失 + 整组未装的组件）。
function envMissing() {
  return [
    ...(webEnv.basePhase === 'ready' ? webEnv.baseMissing : []),
    ...(webEnv.lnmpPhase === 'ready' ? missingComponents(webEnv.lnmpGroups) : []),
  ];
}

// envUnread 表示还有一层没读到。读不到时不许把"没读到"当成"环境完整"。
function envUnread() {
  return webEnv.basePhase !== 'ready' || webEnv.lnmpPhase !== 'ready';
}

// lnmpNeeded：只有"两层都读到、且都没有缺失"才隐藏入口；其余一律显示。
function lnmpNeeded() {
  return envUnread() || envMissing().length > 0;
}

// lnmpHintTitle 生成按钮 tooltip：缺失项一变，文案跟着变。
function lnmpHintTitle() {
  if (webEnv.basePhase === 'error' || webEnv.lnmpPhase === 'error') {
    return '环境状态读取失败，可点开确认' + LNMP_TITLE_TAIL;
  }
  if (envUnread()) return '正在读取环境状态…' + LNMP_TITLE_TAIL;
  const miss = envMissing();
  if (!miss.length) return '环境已完整，无需安装' + LNMP_TITLE_TAIL;
  return '当前缺失"' + miss.join('、') + '"' + LNMP_TITLE_TAIL;
}

// phpAppID 把 PHP 版本映射成**应用市场目录里的 id**：php@8.2 → php82
// （与 catalog.go 的 ID 命名一致：php82 / php84）。
//
// 为什么不能只去掉 service 里的 @ 和 .：真机上 8.4 可能是**无版本别名** `php`
//（opt/php → Cellar/php/8.4.7），它的 PHPVersion.service 就叫 "php" ——
// 去掉 @/. 会得到市场里根本不存在的 id "php"，点卸载只会拿到 400。
// 所以 service 带版本号时用它，否则回落到 version（8.4 → php84）。
// 目录里确实没有对应 id 时，后端会如实返回 400，界面把原因显示出来。
function phpAppID(p) {
  const svc = String((p && p.service) || '');
  const m = svc.match(/^php@(\d+)\.(\d+)/)
    || String((p && p.version) || '').match(/^(\d+)\.(\d+)/);
  return m ? 'php' + m[1] + m[2] : '';
}


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
// 调用方必须先判 lnmpNeeded()：环境完整时这个入口不该出现。
function lnmpButton(big) {
  return h('button.btn' + (big ? '.btn-primary' : '.btn-sm'), {
    text: '⚡ 一键 LNMP',
    title: lnmpHintTitle(),
    onclick: startLNMP,
  });
}

export function SitesView(content, ctx = {}) {
  clear(content);

  // 新一页 = 新代次：旧视图还在飞的探测结果作废；事实先回到"读取中"，
  // 不沿用上一页的"运行 / 环境完整"结论。
  envToken += 1;
  webEnv = freshWebEnv();

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

  // renderWebEnvLine 画"网站环境"那一行（服务层就绪与否）。
  // 失败时**如实说读不到**，不沿用上一次的结论、更不假装就绪。
  function renderWebEnvLine(list) {
    clear(webEnvLine);
    if (webEnv.servicesPhase === 'error') {
      webEnvLine.textContent = '网站环境：无法读取服务状态（' + webEnv.servicesErr + '）';
      return;
    }
    // svcRunning 只用来说明"归不归面板管"（不再用来判断"在不在跑"）。
    const svcRunning = (re) => list.some((s) => (re.test(String(s.name || ''))
      || re.test(String(s.display_name || ''))) && !!((s.state || {}).running));
    // PHP 的真实探测（比"服务记录"准）：sites 接口的 php_versions 直接给出 fpm 是否在跑。
    const phpLive = ((cache && cache.php_versions) || []).some((p) => p && p.running);
    const rt = webEnv.runtime;
    // 判定顺序（2026-09-18 用户报障"nginx 明明在跑却显示未就绪"之后定的规矩）：
    //   ① 有运行体证据 → 以它为准；
    //   ② 探测本身没做成（runtime 接口挂了）→ **未知**（不写"未就绪"，也绝不写"已就绪"）；
    //   ③ 兜底才看服务记录（老版本行为，仅当运行体层完全没数据时）。
    const checks = {
      nginx: () => (rt && rt.nginx ? !!rt.nginx.running : svcRunning(/^nginx/i)),
      PHP: () => (rt && rt.php ? !!rt.php.running : (phpLive || svcRunning(/^php/i))),
      MySQL: () => (rt && rt.mysql ? !!rt.mysql.running : svcRunning(/^(mysql|mariadb|percona)/i)),
    };
    // unknown：运行体层没读到、服务层也没读到 → 面板**没有证据**，如实说"未知"。
    const unknown = (label) => {
      if (rt) return false; // 有运行体层数据就不存在"未知"
      const svcHas = svcRunning(new RegExp('^' + (label === 'PHP' ? 'php' : label), 'i'));
      return !svcHas && !(label === 'PHP' && phpLive);
    };
    const missing = WEB_ENV_PARTS.filter((label) => !checks[label]());
    const unknownParts = missing.filter((label) => unknown(label));
    const reallyMissing = missing.filter((label) => !unknown(label));
    webEnvLine.append(
      h('span', { text: '网站环境：' }),
      reallyMissing.length
        ? h('span.pill.warn', { text: '未就绪' })
        : (unknownParts.length
          ? h('span.pill.warn', { text: '状态未知' })
          : h('span.pill.ok', { text: '已就绪' })),
      h('span', {
        // 用"未检测到运行中的"而不是"未运行"：面板没看见 ≠ 一定没在跑
        //（例如本机有一份不归面板管的 nginx）。只报"检测到了什么"，不替现实下结论。
        text: '（' + (missing.length
          ? '未检测到运行中的：' + missing.join('、')
          : 'nginx / PHP / MySQL 均在运行') + '）',
      }),
    );
    // 在跑、但面板里没有它的记录 → 说清楚，并给出"接入"的路（不是"未就绪"）。
    //
    // ⚠️ 这里**必须**用 appendAll：原生 Element.append 会把 `null` 当文本渲染成
    // 字符串 "null"（2026-09-18 用户报障：状态行末尾直接显示了一个 null）。
    // appendAll（ui.js）会跳过 null/undefined —— 条件渲染一律走它。
    if (runningUnregistered().length) {
      webEnvLine.append(h('div', {
        style: { marginTop: '3px' },
        text: '注：' + runningUnregistered().map((x) => x.label).join('、')
          + ' 正在运行，但面板里没有它的服务记录（不影响网站运行）。'
          + '需要面板管它（重启/停止/看状态）时，到「应用」里对同名条目点「接入」。',
      }));
    }
    // "一键 LNMP 会做什么"这段只在**环境不完整或状态未知**时才说 ——
    // 全都跑着的时候还念一遍，用户会以为面板在劝他重装（用户原话：
    // "既然都在运行，提示这个干什么？"）。
    const needExplain = lnmpNeeded() || missing.length > 0 || unknownParts.length > 0
      || webEnv.lnmpPhase !== 'ready' || webEnv.basePhase !== 'ready';
    if (needExplain) {
      webEnvLine.append(h('div', {
        style: { marginTop: '3px' },
        text: '「⚡ 一键 LNMP」会先确保运行依赖（命令行开发者工具 CLT / Homebrew）'
          + '再装 nginx + PHP + MySQL + phpMyAdmin。运行依赖（含 ffmpeg）是所有 brew 应用的公共底座，'
          + '与网站无关，缺了会在首页提示。',
      }));
    }
  }

  // refreshWebEnv 并行拉三层事实（服务状态 / 运行依赖 / LNMP 组件已装情况），
  // 拿到后重画状态条与空列表大按钮。
  //
  // 为什么要在这里重画：renderStatus() 在 load() 里**早于**本函数完成就调用过一次，
  // 那时三层都还是"读取中"。本函数只重画 DOM，不再触发探测 —— 不会形成循环。
  async function refreshWebEnv() {
    const myToken = envToken;
    webEnv.servicesPhase = 'loading';
    webEnv.nginxState = 'loading';
    clear(webEnvLine);
    webEnvLine.textContent = '网站环境：读取中…';

    const [svcRes, baseRes, lnmpRes, rtRes] = await Promise.allSettled([
      api.services(false), // 服务记录（回答"归不归面板管"，**不回答**"在不在跑"）
      api.baseEnv(), // 底座：CLT / Homebrew / ffmpeg
      api.lnmpOptions(), // 三件套各自装了哪个版本（installed 是后端真实探测）
      api.sitesRuntime(), // 运行体证据：进程 / 端口 / socket（nginx / PHP / MySQL）
    ]);
    if (myToken !== envToken) return; // 页面已切换：这次结果作废，不写进新页面

    // ① 服务层：nginx 到底在不在跑
    let list = [];
    if (svcRes.status === 'fulfilled') {
      list = (svcRes.value && svcRes.value.list) || [];
      webEnv.servicesPhase = 'ready';
      const nginx = list.find((s) => nginxEntry(s));
      // 有条目才谈运行/停止；**没有条目一律算未知**（见 freshWebEnv 的说明）。
      webEnv.nginxState = !nginx ? 'unknown'
        : (((nginx.state || {}).running) ? 'running' : 'stopped');
    } else {
      webEnv.servicesPhase = 'error';
      webEnv.servicesErr = (svcRes.reason && svcRes.reason.message) || String(svcRes.reason);
      webEnv.nginxState = 'unknown';
    }

    // ①b 运行体层：nginx / PHP / MySQL 到底在不在跑（与"有没有记录"分开）
    if (rtRes.status === 'fulfilled' && rtRes.value) {
      webEnv.runtimePhase = 'ready';
      webEnv.runtime = rtRes.value;
    } else {
      webEnv.runtimePhase = 'error';
      webEnv.runtime = null;
      webEnv.runtimeErr = (rtRes.reason && rtRes.reason.message) || String(rtRes.reason);
    }

    // ② 底座层：missing 是后端给的人读名（命令行开发者工具 / Homebrew / ffmpeg）
    if (baseRes.status === 'fulfilled' && baseRes.value && Array.isArray(baseRes.value.missing)) {
      webEnv.basePhase = 'ready';
      webEnv.baseMissing = baseRes.value.missing.slice();
    } else {
      webEnv.basePhase = 'error';
      webEnv.baseMissing = [];
    }

    // ③ 组件层：groups 为空 = 后端没给出候选（读不到），不能当成"都装了"
    if (lnmpRes.status === 'fulfilled' && lnmpRes.value
      && Array.isArray(lnmpRes.value.groups) && lnmpRes.value.groups.length) {
      webEnv.lnmpPhase = 'ready';
      webEnv.lnmpGroups = lnmpRes.value.groups;
    } else {
      webEnv.lnmpPhase = 'error';
      webEnv.lnmpGroups = [];
    }

    renderWebEnvLine(list);
    // 状态条上"一键 LNMP 入口"与 nginx 状态都依赖上面三层事实，重画一次；
    // 空列表的中间大按钮同样要跟着出现/消失。
    renderStatus();
    renderList();
  }

  async function load() {
    clear(listBox);
    listBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取站点…' })]));
    // 默认站点状态与站点列表并行拉（只读文件探测，没有 brew / 网络开销）。
    // 失败不阻断列表：状态退回 null，药丸显示"未知"，绝不假装"已创建"。
    void api.defaultSite().then((v) => { defSite = v || null; renderStatus(); })
      .catch(() => { defSite = null; renderStatus(); });
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

  // runningUnregistered 返回"在跑、但面板服务记录里没有"的组件。
  //
  // 这是 2026-09-18 那次报障的另一半：nginx 在跑、面板没记录 → 以前显示"未就绪"，
  // 现在显示"在跑"并说明"不归面板管"。用户据此可以决定要不要接入（而不是被误导去重装）。
  function runningUnregistered() {
    const rt = webEnv.runtime;
    if (!rt) return [];
    const out = [];
    const pairs = [['nginx', 'Nginx'], ['php', 'PHP'], ['mysql', 'MySQL']];
    for (const [key, label] of pairs) {
      const p = rt[key];
      if (p && p.running && !p.registered) out.push({ label, probe: p });
    }
    return out;
  }

  // nginxEntry 判断服务条目是不是 nginx（与 renderWebEnvLine 的 svcRunning 同一套口径）。
  function nginxEntry(s) {
    return /^nginx/i.test(String((s && s.name) || ''))
      || /^nginx/i.test(String((s && s.display_name) || ''));
  }

  // nginxPill 是「🐘 PHP 环境」右侧那颗**只读**状态药丸。
  //
  // 判据优先用**运行体证据**（GET /api/v1/sites/runtime → priv.NginxStatus() 的
  // pgrep 结果），服务记录只在运行体层读不到时兜底：
  //   运行体说在跑      → "nginx 运行"
  //   运行体说没进程    → "nginx 停止"
  //   运行体层读不到    → 退回服务记录；两者都没有 → "状态未知"
  //
  // 2026-09-18 的报障就是"只看服务记录"造成的：nginx 在 :80 上正常服务、面板里
  // 却没有它的记录 → 药丸写"未知"、环境行写"未就绪"。**没看见 ≠ 没在跑。**
  function nginxPill() {
    const rt = webEnv.runtime && webEnv.runtime.nginx;
    if (webEnv.runtimePhase === 'loading' && webEnv.servicesPhase === 'loading') {
      return h('span.pill', { text: 'nginx 状态读取中…', title: '正在探测 nginx 进程与服务记录' });
    }
    if (rt) {
      if (rt.running) {
        return h('span.pill.ok', {
          text: 'nginx 运行',
          title: (rt.evidence || '运行体探测说它在跑')
            + (rt.registered ? '' : '\n（面板里没有它的服务记录：能看状态，但重启/停止要在「应用」里先接入）'),
        });
      }
      return h('span.pill.warn', {
        text: 'nginx 停止',
        title: (rt.evidence || '运行体探测没找到 nginx 进程')
          + (rt.probe_error ? '\n探测错误：' + rt.probe_error : ''),
      });
    }
    if (webEnv.nginxState === 'running') {
      return h('span.pill.ok', { text: 'nginx 运行', title: '服务列表里 nginx 条目 state.running = true' });
    }
    if (webEnv.nginxState === 'stopped') {
      return h('span.pill.warn', {
        text: 'nginx 停止',
        title: '服务列表里 nginx 条目 state.running = false',
      });
    }
    return h('span.pill.warn', {
      text: 'nginx 状态未知',
      title: webEnv.runtimePhase === 'error'
        ? '运行体探测失败，服务列表里也没有可用结论，无法判断 nginx 是否在跑：' + webEnv.runtimeErr
        : '服务列表里没有 nginx 条目（它可能不归面板登记）—— 面板没有证据说它在跑，也没有证据说它停了',
    });
  }

  // defSitePill 是工具条上「默认站点」的状态药丸（只读）。
  // 判据全部来自后端 defaultSiteStatusNow()：已创建 / 还没建 / 缺 nginx / 读不到。
  // **没有证据时既不说"已创建"也不说"失败"**。
  function defSitePill() {
    if (defSite === null) {
      return h('span.pill', { text: '默认站点未知', title: '还没读到默认站点状态（或读取失败）' });
    }
    if (defSite.applied) {
      return h('span.pill.ok', {
        text: '默认站点已就绪',
        title: 'http://' + (defSite.url || '').replace(/^https?:\/\//, '') +
          ' 由面板的默认站点接住（root ' + (defSite.index_path || '') + '）',
      });
    }
    if (defSite.nginx_present === false) {
      return h('span.pill.warn', {
        text: '默认站点待创建（缺 nginx）',
        title: defSite.needs_action || '这台机器还没有 nginx',
      });
    }
    if (defSite.foreign_vhost) {
      return h('span.pill.warn', {
        text: '默认站点（非面板生成）',
        title: defSite.needs_action || '80 端口上已有一份不是面板生成的配置；面板不会自动覆盖它',
      });
    }
    return h('span.pill.warn', {
      text: '默认站点未创建',
      title: defSite.needs_action || '点「🏠 默认站点」创建',
    });
  }

  // defaultSiteModal 是「默认站点」弹窗：说清状态、给一条能走通的路。
  //
  // 用户 2026-09-21 的要求是"装完就该有一个默认静态站点"。所以这里的按钮
  // 顺序刻意按"缺什么先补什么"排：没有 nginx → 先「只安装 Nginx」（不拉 PHP/MySQL）；
  // 有 nginx 但没建 → 「创建默认站点」；已建 → 显示地址与「重建」。
  function defaultSiteModal() {
    const body = h('div');
    const foot = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', flexWrap: 'wrap' } });
    const m = modal({ title: '🏠 默认站点', body, footer: [foot], wide: false });

    function draw(st) {
      clear(body);
      clear(foot);
      if (!st) {
        body.append(h('div.hint', { text: '正在读取默认站点状态…' }));
        return;
      }
      const ip = (st.url || '').replace(/^https?:\/\//, '');
      body.append(h('div', { style: { lineHeight: '1.8' } }, [
        h('div', { style: { marginBottom: '6px' },
          text: st.applied ? '✅ 默认站点已就绪' : '⚠️ 默认站点还没建好' }),
        h('div', { style: { fontSize: '12.5px', color: 'var(--text-dim)' },
          text: '默认站点是 80 端口上接住"没匹配到具体域名"的请求的兜底站点，' +
            'root 指向 ' + (st.index_path ? st.index_path.replace(/\/index\.html$/, '') : '（未创建）') +
            '。它**只依赖 nginx**：没有 PHP / MySQL 也是一份可用的静态站点。' }),
        st.applied ? h('ul', { style: { margin: '10px 0 0 18px', lineHeight: '1.8' } }, [
          h('li', { text: '访问地址：http://' + ip + '/' }),
          h('li', { text: 'vhost：' + (st.vhost_path || '') + (st.vhost_marked ? '（面板生成）' : '（不是面板生成的，重建会覆盖）') }),
        ]) : null,
        st.nginx_present === false
          ? h('div', { style: { marginTop: '10px', padding: '9px 11px', border: '1px solid var(--warn)',
            background: 'var(--warn-soft)', borderRadius: '6px', lineHeight: '1.7' },
          }, [
            h('div', { style: { fontWeight: '620' }, text: '这台机器还没有 Nginx' }),
            h('div', { style: { marginTop: '4px', fontSize: '12.5px' },
              text: '默认站点要靠 nginx 监听 80 端口，所以先装它。面板可以**只装 Nginx**（不装 PHP 与 MySQL），' +
                '装好后面板会自动把默认站点建起来，你也可以回来点「创建默认站点」。' }),
          ])
          : null,
        st.foreign_vhost ? h('div', { style: { marginTop: '10px', padding: '9px 11px', border: '1px solid var(--warn)',
          background: 'var(--warn-soft)', borderRadius: '6px', lineHeight: '1.7' } }, [
          h('div', { style: { fontWeight: '620' }, text: '80 端口上已有一份不是面板生成的配置' }),
          h('div', { style: { marginTop: '4px', fontSize: '12.5px' },
            text: '面板不会自动覆盖它（那可能是你自己写的站点）。要换成面板的默认站点，' +
              '点下面那颗按钮 —— 原文件会先备份成 ' + ((st.vhost_path || '000-default.conf') + '.zizpanel.bak') + '。' }),
        ]) : null,
        st.error ? h('div.hint', { style: { marginTop: '8px', color: 'var(--danger)' },
          text: '上一次的结果：' + st.error }) : null,
        h('div', { style: { marginTop: '10px', fontSize: '12px', color: 'var(--text-dim)' },
          text: '重建会按模板重写 ' + (st.vhost_path || '000-default.conf') +
            '（面板自己的那份配置）：手工加在这份文件里的内容会丢；别的站点文件不受影响。' }),
      ]));

      if (st.nginx_present === false) {
        foot.append(h('button.btn.btn-primary', {
          text: '只安装 Nginx（不装 PHP/MySQL）',
          onclick: async () => {
            try {
              await taskCenter.start({
                kind: 'install', target: 'nginx', title: '安装 Nginx',
                start: () => api.marketInstall('nginx'),
                onDone: async (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    toast('安装 Nginx 失败：' + (task.error || task.status), 'err', 12000);
                    return;
                  }
                  toast('Nginx 已安装，正在创建默认站点…', 'ok', 8000);
                  // 装完立刻接着建默认站点：用户点这一颗按钮的意图就是"给我一个能访问的站点"，
                  // 不该再让他回来点第二次。失败时如实报错（不静默）。
                  try {
                    const r = await api.defaultSiteApply();
                    toast((r && r.message) || '默认站点已创建', 'ok', 9000);
                  } catch (e) {
                    toast('Nginx 装好了，但默认站点创建失败：' + ((e && e.message) || e), 'err', 14000);
                  }
                  await refreshDefaultSite();
                  load();
                },
              });
            } catch (e) { toast('安装 Nginx 失败：' + ((e && e.message) || e), 'err', 12000); }
          },
        }));
      } else {
        // foreign_vhost：80 端口上已经有一份**不是面板生成的**配置（用户自己写的）。
        // 面板不会自动覆盖它，只有用户在这一屏明确点下去才覆盖 —— 而且**会先备份**。
        const foreign = !!st.foreign_vhost;
        foot.append(h('button.btn' + (st.applied ? '' : '.btn-primary'), {
          text: st.applied ? '重建默认站点' : (foreign ? '覆盖为面板默认站点' : '创建默认站点'),
          onclick: async () => {
            if (foreign && !await confirmBox(
              (st.vhost_path || '000-default.conf') + ' 不是面板生成的（可能是你自己写的 80 端口站点）。\n\n'
              + '继续会用面板的默认站点覆盖它，原文件会先备份成 .zizpanel.bak（不会丢）。\n\n继续？',
              { title: '覆盖为面板默认站点', okText: '覆盖（先备份）' })) return;
            try {
              const r = await api.defaultSiteApply();
              toast((r && r.message) || '默认站点已创建', 'ok', 9000);
              await refreshDefaultSite();
              load();
            } catch (e) { toast('创建默认站点失败：' + ((e && e.message) || e), 'err', 14000); }
          },
        }));
      }
      if (st.applied) {
        foot.append(h('button.btn', {
          text: '打开看看', onclick: () => window.open('http://' + ip + '/', '_blank', 'noopener'),
        }));
      }
      foot.append(h('button.btn', { text: '关闭', onclick: () => m.close() }));
    }

    async function refreshDefaultSite() {
      try { defSite = await api.defaultSite(); } catch (e) { defSite = null; }
      draw(defSite);
      renderStatus();
    }
    draw(defSite);
    void refreshDefaultSite();
  }

  // moreMenu 是工具条上的二级菜单（弹窗 + 竖排 .btn-block，沿用文件管理的既有写法）。
  // 「校验 nginx」与「重建全部配置」都是低频动作，收进这里，行为与原来完全一致。
  function moreMenu() {
    const validate = h('button.btn.btn-block', {
      text: '🧪 校验 nginx',
      onclick: async () => {
        m.close();
        try {
          const r = await api.nginxTest();
          if (r.ok) { toast('nginx 配置校验通过', 'ok'); return; }
          // 校验失败：**把 nginx 的原始输出完整显示出来**（含文件名与行号）。
          // 用弹窗而不是 toast —— 报错常常好几行，toast 会被截断，
          // 用户只看到"配置有问题"就等于没给信息（2026-09-18 用户报障）。
          modal({
            title: '❌ nginx 配置校验未通过',
            wide: true,
            body: h('div', [
              h('div.hint', { text: '下面是 nginx -t 的原始输出（含出错文件与行号）：' }),
              h('pre', {
                style: { whiteSpace: 'pre-wrap', fontFamily: 'var(--mono)', fontSize: '12.5px',
                  background: 'var(--danger-soft)', padding: '10px 12px', borderRadius: '6px',
                  maxHeight: '340px', overflow: 'auto', userSelect: 'text' },
                text: r.output || '（nginx 没有输出任何内容，请看「日志中心 → nginx 主错误日志」）',
              }),
              h('div.hint', { style: { marginTop: '8px' },
                text: '常见修法：按行号改那一行；或到「⚙️ 配置文件」里打开对应文件修正。' +
                  '面板改 nginx.conf 前会备份成 nginx.conf.zizpanel.bak，可直接用它还原。' }),
            ]),
            footer: (close) => [h('button.btn', { text: '关闭', onclick: close })],
          });
        } catch (e) { toast(e.message, 'err', 12000); }
      },
    });
    const rebuild = h('button.btn.btn-block', {
      text: '♻️ 重建全部配置',
      onclick: async () => {
        m.close();
        if (!await confirmBox('将按模板重新生成所有站点的 nginx 配置并重载。\n\n你自己手工加在 vhost 里的内容会被覆盖（面板只保留数据库中的设置）。\n\n继续？', { title: '重建全部配置' })) return;
        try {
          const r = await api.siteReloadAll();
          const n = (r.rebuilt || []).length;
          if ((r.failed || []).length) toast(`重建 ${n} 个，失败 ${r.failed.length} 个：${r.failed[0]}`, 'warn', 12000);
          else toast(`已重建 ${n} 个站点配置`, 'ok');
          load();
        } catch (e) { toast(e.message, 'err', 9000); }
      },
    });
    const m = modal({
      title: '更多操作',
      body: h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } }, [
        h('div', [
          validate,
          h('div.hint', { text: '对 nginx 配置跑一次语法校验（nginx -t）' }),
        ]),
        h('div', [
          rebuild,
          h('div.hint', { text: '按当前数据库状态重新生成所有站点的 nginx 配置（用于修复被手工改坏的配置）' }),
        ]),
      ]),
    });
  }

  // ---------- ⚙️ 配置文件（手动改 nginx / php.ini / my.cnf 的入口）----------
  //
  // 用户 2026-09-20："面板也要有手动改 php 和 nginx 的入口啊！这是基本操作。"
  //
  // 清单由后端给（api.sites() 的 config_files 字段）：brew 前缀在 Apple Silicon
  // 是 /opt/homebrew、Intel 是 /usr/local，前端拼字符串一定会在另一种机器上错。
  // 每一条的「📝 编辑」都复用 services.js 的 configFileModal（同一套文件接口与
  // 保存/重启语义）；文件不存在时按钮照点，编辑器会如实报原因。
  function configFilesModal() {
    const box = h('div', [h('div.empty', [h('p', { text: '正在读取配置文件清单…' })])]);
    const m = modal({ title: '网站环境配置文件', wide: true, body: box });

    async function load() {
      clear(box);
      box.append(h('div.empty', [h('p', { text: '正在读取配置文件清单…' })]));
      let files = (cache && cache.config_files) || null;
      if (!files) {
        try {
          const res = await api.sites();
          cache = res;
          files = res.config_files || [];
        } catch (e) {
          clear(box);
          box.append(h('div.empty', [
            h('div.big', { text: '⚠️' }),
            h('h4', { text: '读取配置文件清单失败' }),
            h('p', { text: e.message }),
          ]));
          return;
        }
      }
      if (!files.length) {
        clear(box);
        box.append(h('div.empty', [
          h('div.big', { text: '📄' }),
          h('h4', { text: '没有可列出的配置文件' }),
          h('p', { text: '后端没有返回清单（面板版本可能较旧）；也可以先在「⋯ 更多 → 重建全部配置」之后再试。' }),
        ]));
        return;
      }
      const groups = new Map();
      for (const f of files) {
        if (!groups.has(f.group)) groups.set(f.group, []);
        groups.get(f.group).push(f);
      }
      const rows = [];
      for (const [group, items] of groups) {
        rows.push(h('div', { style: { fontWeight: '600', margin: '12px 0 4px' }, text: group }));
        for (const it of items) {
          rows.push(h('div', {
            style: {
              display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap',
              padding: '7px 0', borderBottom: '1px solid var(--line,#e5e7eb)',
            },
          }, [
            h('div', { style: { flex: '1 1 320px', minWidth: '260px' } }, [
              h('div', { text: it.label }),
              h('code.code', { style: { fontSize: '11.5px', wordBreak: 'break-all' }, text: it.path }),
              it.exists
                ? null
                : h('div.hint', { text: '文件还不存在（服务未启动过或尚未生成）—— 编辑器会如实说明' }),
            ]),
            h('button.btn.btn-sm', {
              text: '📝 编辑',
              title: it.service
                ? '打开编辑器；保存后可用编辑器里的「🔄 重启服务」重启 ' + it.service
                : '打开编辑器；面板没有这条服务的记录，保存后请手工重启对应服务',
              onclick: () => configFileModal({
                display_name: it.label,
                config_path: it.path,
                name: it.service || '',
              }, () => { load(); }),
            }),
          ]));
        }
      }
      clear(box);
      box.append(
        h('div.hint', {
          style: { marginBottom: '6px' },
          text: '这些文件是底层服务的真实配置。面板生成的站点 vhost 会在下次保存站点时被覆盖；'
            + 'nginx.conf / php.ini / my.cnf 属于手工维护，面板不会动它们。',
        }),
        ...rows,
        h('div', {
          style: { marginTop: '14px', padding: '10px 12px', background: 'var(--warn-soft)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.8' },
        }, [
          h('div', { text: '⚠️ 保存不等于生效：' }),
          h('div', { text: '· nginx 配置改完要重新加载 nginx（本条目的「🔄 重启服务」；只想重载可用「⋯ 更多 → 校验 nginx」先确认语法，再保存任意站点触发自动重载）' }),
          h('div', { text: '· php.ini / php-fpm.conf / www.conf 改完要重启对应版本的 php-fpm' }),
          h('div', { text: '· my.cnf 改完要重启 MySQL' }),
          h('div', { text: '重启按钮点下去如果失败，会给出失败原因（面板没登记该服务 / 权限不足等），不会假装成功。' }),
        ]),
      );
    }
    load();
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
      //
      // 只有"环境完整"（两层都读到且都不缺）时才不给入口 —— 读不到时照常给，
      // 并在 title 里说明读不到（见 lnmpNeeded / lnmpHintTitle）。
      ...(list.length && lnmpNeeded() ? [lnmpButton(false)] : []),
      // 「PHP-FPM 运行中 x/y 个」原来是一个独立药丸，与这个按钮说的是同一件事 ——
      // 用户要求合并：状态直接写进按钮文案，点开就是 PHP 环境面板。
      h('button.btn.btn-sm', {
        text: phps.length ? `🐘 PHP 环境 · ${phpRunning}/${phps.length} 运行中` : '🐘 PHP 环境',
        title: '查看已安装的 PHP 版本、各自的 FastCGI 端点与运行状态；一键修复端点\n'
          + phps.map((p) => `${p.version} ${p.running ? '运行中' : '未运行'} · ${p.pass || p.listen_err || '端点未解析'}`).join('\n'),
        onclick: phpEnvModal,
      }),
      // 紧挨「PHP 环境」右侧：nginx 的真实运行状态（只读药丸，不是可点的假按钮）。
      nginxPill(),
      // ⚙️ Nginx 管理：宝塔式的四页签（服务 / 配置修改 / 性能调整 / 错误日志）。
      h('button.btn.btn-sm', {
        text: '⚙️ Nginx 管理',
        title: 'nginx 的运行状态、性能参数（worker_processes / 连接数 / gzip / 最大上传大小…）、配置文件与错误日志',
        onclick: () => nginxPanelModal(),
      }),
      // ⚡ 常用设置：一次能传多大 / 脚本能跑多久。这是用户最常改的东西，
      // 所以放在「配置文件」**前面**并且是功能按钮（不用去改配置文件）。
      h('button.btn.btn-sm', {
        text: '⚡ 上传大小 / 执行时间',
        title: '在面板里改 nginx 请求体上限与 PHP 上传/执行上限（改完自动重载 nginx、重启 php-fpm，并回读生效值）',
        onclick: () => uploadLimitsModal(),
      }),
      // ⚙️ 配置文件：手动改 nginx.conf / 各站点 vhost / php.ini / my.cnf 的入口
      // （宝塔式"基本操作"）。文件清单与路径由后端给，编辑复用既有配置编辑器。
      h('button.btn.btn-sm', {
        text: '⚙️ 配置文件',
        title: '手动编辑 nginx.conf、各站点 vhost、每个 PHP 版本的 php.ini / php-fpm 配置、MySQL 的 my.cnf（保存后需要重载/重启才生效）',
        onclick: configFilesModal,
      }),
      // 🏠 默认站点：装完面板就该有的那个静态站点。状态如实显示：
      //   · 已创建 → 绿色药丸（点开可重建）
      //   · 还没建 → 橙色药丸（点开一键建；缺 nginx 时先只装 nginx）
      //   · 读不到 → 灰色药丸"未知"（不假装已创建）
      defSitePill(),
      h('button.btn.btn-sm', {
        text: '🏠 默认站点',
        title: '查看/创建面板的默认静态站点（www/localhost，监听 80 端口的兜底站点；不需要 PHP 与 MySQL）',
        onclick: defaultSiteModal,
      }),
      // 「校验 nginx」/「重建全部配置」收进这个二级菜单（用户要求）。
      h('button.btn.btn-sm', { text: '⋯ 更多', title: '更多操作', onclick: moreMenu }),
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
        //
        // 与工具条同一判据：环境完整（两层都读到且都不缺）时**不出现**；
        // 读不到时照常出现（不拿"没读到"当真完整）。
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
          ...(lnmpNeeded() ? [lnmpButton(true)] : []),
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
              h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
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
                // 卸载入口：**每个真实存在的版本**都有一颗（用户报障：php8.4 根本
                // 找不到卸载按钮）。点了先出确认框，确认后才走任务中心。
                h('button.btn.btn-sm.btn-danger', {
                  text: '🗑 卸载',
                  title: '卸载 PHP ' + p.version + '（停止该版本 php-fpm 并 brew uninstall ' + (p.service || ('php@' + p.version)) + '）',
                  onclick: () => uninstallPHP(p, refreshAfterUninstall),
                }),
              ]),
            ]),
          ]))),
        ]),
        h('div.hint', { style: { marginTop: '12px' }, text: `套接字目录：${data.socket_dir || ''}` }),
        h('div.hint', { text: '注意：改写配置后必须重启对应的 php-fpm 才会生效；面板会在重启后确认端点真的有人在监听，否则如实报错。' }),
      );
    };
    // refreshAfterUninstall 卸载成功后重拉列表并重绘。render/state2 是本函数的作用域，
    // 而 uninstallPHP 定义在 SitesView 层（拿不到它们），所以用回调传进去。
    const refreshAfterUninstall = async () => {
      try { state2.data = await api.phpList(); } catch (e) { /* 拉取失败就保留旧数据 */ }
      render();
    };
    render();
  }

  // uninstallPHP 卸载一个**真实存在的** PHP 版本（用户报障：php8.4 找不到卸载入口）。
  //
  // 分工：这里只负责"说清会做什么 + 显著警告 + 走任务中心"；真的停服务、删服务
  // 定义、`brew uninstall` 由后端（DELETE /api/v1/market/{id}）执行，本文件不碰后端。
  //
  // 四道门（缺一不可）：
  //   ① 确认框逐条列出本次动作（卸载不可逆）；
  //   ② 默认版本 / 正被站点使用的版本 → **显著警告**：卸载后这些站点会 502；
  //   ③ **brew 依赖**：如果别的已安装包依赖这个 PHP（真机：python 8.4 被 llvm/rust
  //      依赖时 brew 会拒绝卸载），先弹说明对话框，给「取消 / 强制卸载」两个明确选择，
  //      正文逐字写清会破坏哪些包 —— 而不是让用户点了确认再吃一整段 brew 英文报错；
  //   ④ 失败必须有可见反馈 —— taskCenter 提交失败会 toast，任务失败走 onDone 的
  //      toast；绝不出现"点了没反应"。
  //
  // 计划来自后端依赖引擎（按需接口，见下面 uninstallPHP 里的说明），
  // 不在前端重算依赖 —— 依赖判定只有后端那一处实现。
  async function uninstallPHP(p, onDone) {
    const appId = phpAppID(p);
    const formula = p.service || ('php@' + p.version);
    // 计划来自后端**按需**接口（GET /api/v1/market/{id}/uninstall-plan）：
    // 市场列表里的计划**故意不查依赖**（每个条目一次真实 brew 调用，36 条串起来
    // 就是 15 秒冷启动 —— 用户报的"正在读取应用目录…"）。依赖判定只有后端一处实现，
    // 前端只负责把结论讲给用户听。
    // 取不到就按"未检查"继续（照旧弹确认框），但不假装没有依赖：真被 brew 拒绝时
    // 任务里会如实报错。
    let plan = null;
    try {
      const res = await api.marketUninstallPlan(appId);
      plan = (res && res.uninstall) || null;
    } catch (e) { plan = null; }
    const usedBy = ((cache && cache.list) || [])
      .filter((s) => String(s.php_version || '') === String(p.version))
      .map((s) => s.domain);
    const risky = !!p.is_default || usedBy.length > 0;
    let wipe = false;
    let force = false;

    const wipeBox = h('input', { type: 'checkbox' });
    wipeBox.addEventListener('change', () => { wipe = wipeBox.checked; });
    // 强制卸载：只在后端说"被 brew 依赖拦下"时出现，默认不勾。
    const forceBox = h('input', { type: 'checkbox' });
    forceBox.addEventListener('change', () => { force = forceBox.checked; });
    const forceDeps = ((plan && plan.dependents) || [])
      .filter((d) => d.kind === 'brew').map((d) => d.name);
    const forceList = forceDeps.length ? forceDeps.join('、') : '依赖它的包';

    const warning = h('div', {
      style: {
        border: '1px solid ' + (risky ? 'var(--danger)' : 'var(--warn)'),
        background: risky ? 'var(--danger-soft)' : 'var(--warn-soft)',
        borderRadius: '8px', padding: '10px 12px', marginBottom: '12px',
      },
    }, risky
      ? [
        h('div', { style: { fontWeight: '700', color: '#f87171', fontSize: '14px' },
          text: '⚠️ PHP ' + p.version
            + (p.is_default ? ' 是面板的默认版本' : ' 正被站点使用')
            + (usedBy.length ? '：' + usedBy.join('、') : '') }),
        h('div', { style: { marginTop: '4px' },
          text: '卸载后，使用 PHP ' + p.version + ' 的站点会 502 —— 需要到站点设置里改选其它 PHP 版本。' }),
      ]
      : [
        h('div', { style: { fontWeight: '700', fontSize: '14px' }, text: '卸载 PHP ' + p.version }),
        h('div', { style: { marginTop: '4px' }, text: '卸载不可恢复；其它 PHP 版本与站点文件不受影响。' }),
      ]);

    // brew 依赖拦下：**先**弹"还不能卸载"对话框（逐条列出谁依赖它），
    // 用户在对话框里明确点「强制卸载」才继续到确认框 —— 也就是默认取消。
    if (plan && plan.blocked && plan.force_allowed === true) {
      const deps = (plan.dependents || []).map((d) => h('div', {
        style: { marginBottom: '8px' },
      }, [
        h('div', { style: { fontWeight: '620' }, text: 'Homebrew 包：' + (d.name || '') }),
        d.detail ? h('div', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: d.detail }) : null,
      ]));
      let dm = null;
      dm = modal({
        title: '还不能卸载 PHP ' + p.version,
        wide: true,
        body: h('div', [
          h('div', {
            style: { marginBottom: '10px', padding: '9px 11px', background: 'var(--warn-soft)',
              borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
            text: plan.blocked,
          }),
          ...deps,
          h('div', {
            style: { marginTop: '4px', padding: '9px 11px', border: '1px solid var(--danger)',
              background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
            text: '强制卸载会破坏这些包：' + forceList + '（它们会缺依赖、可能无法运行）。'
              + '执行的命令：brew uninstall --ignore-dependencies ' + (plan.formula || formula),
          }),
        ]),
        footer: [
          h('button.btn', { text: '取消', onclick: () => dm.close() }),
          h('button.btn.btn-danger', {
            text: '强制卸载', title: '忽略依赖强行卸载（会破坏 ' + forceList + '）',
            onclick: () => { dm.close(); force = true; void confirmThenRun(true); },
          }),
        ],
      });
      return; // 等用户在对话框里选；取消 = 什么都不做（不发请求）
    }

    // confirmThenRun 真正提交卸载任务。force 只在用户明确选择时为 true。
    async function confirmThenRun(useForce) {
      await taskCenter.start({
        kind: 'uninstall',
        target: formula,
        title: (useForce ? '强制卸载 PHP ' : '卸载 PHP ') + p.version,
        start: () => api.marketUninstall(appId, wipe, useForce),
        onDone: async (task) => {
          if (task && task.status && task.status !== 'succeeded') {
            // 失败绝不沉默：把后端的原话显示出来（它已经是人话，会点名依赖方）。
            toast('卸载 PHP ' + p.version + ' 失败：' + (task.error || task.status), 'err', 12000);
            return;
          }
          toast('已卸载 PHP ' + p.version, 'ok', 9000);
          // 重拉 PHP 列表并重绘（回调由 phpEnvModal 提供：它持有 state2/render）。
          if (typeof onDone === 'function') await onDone();
          // 环境事实变了：重新探测，让「一键 LNMP」入口/nginx 状态跟着更新。
          void refreshWebEnv();
        },
      });
    }

    // openConfirm 弹"本次会做什么"确认框。forceDefault=true 时强制勾选已预置
    // （用户在上一屏明确点了「强制卸载」，这一屏再让他确认一次命令与后果）。
    function openConfirm(forceDefault) {
      force = !!forceDefault;
      forceBox.checked = !!forceDefault;
      const okBtn = h('button.btn.btn-danger', {
        text: force ? '强制卸载 PHP ' + p.version : '卸载 PHP ' + p.version,
      });
      forceBox.addEventListener('change', () => {
        okBtn.textContent = forceBox.checked ? '强制卸载 PHP ' + p.version : '卸载 PHP ' + p.version;
      });
      let m = null;
      m = modal({
        title: '卸载 PHP ' + p.version,
        body: h('div', { dataset: { confirmUninstall: '1' } }, [
          // 与 servicePanel.confirmUninstallPlan 用同一句开场白：卸载确认框的
          // 形状一致（也让自动化测试能用同一选择器稳定选中它）。
          h('div', { style: { marginBottom: '8px' }, text: '将执行：' }),
          warning,
          h('div', [
            h('div', { style: { fontWeight: '600' }, text: '本次会做：' }),
            h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.8' } }, [
              h('li', { text: '停止 PHP ' + p.version + ' 的 php-fpm（若在运行）' }),
              h('li', { text: '删除面板里这个版本的服务定义与开机自启项' }),
              h('li', { text: '执行 brew uninstall ' + formula + '（只动这一个版本）' }),
              h('li', { text: '保留其它 PHP 版本与站点文件；不会自动删除其它 brew 包' }),
            ]),
          ]),
          (plan && plan.force_allowed === true) ? h('div', {
            style: { marginTop: '10px', padding: '9px 11px', border: '1px solid var(--danger)',
              background: 'var(--danger-soft)', borderRadius: '6px', lineHeight: '1.7' },
          }, [
            h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', cursor: 'pointer' } },
              [forceBox, h('span', { text: '强制卸载（忽略依赖，brew --ignore-dependencies）' })]),
            h('div', { style: { marginTop: '4px', fontSize: '12.5px' },
              text: '会破坏这些包：' + forceList + '（它们会缺依赖、可能无法运行）。'
                + '不勾选时若 brew 因依赖拒绝卸载，任务会失败并如实报出依赖方。' }),
          ]) : null,
          h('label', {
            style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '12px', cursor: 'pointer' },
          }, [
            wipeBox,
            h('div', [
              h('div', { text: '同时删除该版本的配置目录（不可恢复）' }),
              h('div.hint', { text: 'php.ini / php-fpm.conf 等；只删除 ' + p.version
                + ' 自己的 etc/php/' + p.version + '，其它版本与 phpmyadmin 的配置不会碰' }),
            ]),
          ]),
        ]),
        footer: [
          h('button.btn', { text: '取消', onclick: () => m.close() }),
          okBtn,
        ],
      });
      okBtn.addEventListener('click', () => { m.close(); void confirmThenRun(forceBox.checked); });
    }

    // brew 依赖拦下：**先**弹"还不能卸载"对话框（逐条列出谁依赖它），
    // 用户在对话框里明确点「强制卸载」才继续到确认框 —— 默认就是取消。
    if (plan && plan.blocked && plan.force_allowed === true) {
      const deps = (plan.dependents || []).map((d) => h('div', {
        style: { marginBottom: '8px' },
      }, [
        h('div', { style: { fontWeight: '620' }, text: 'Homebrew 包：' + (d.name || '') }),
        d.detail ? h('div', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: d.detail }) : null,
      ]));
      let dm = null;
      dm = modal({
        title: '还不能卸载 PHP ' + p.version,
        wide: true,
        body: h('div', [
          h('div', {
            style: { marginBottom: '10px', padding: '9px 11px', background: 'var(--warn-soft)',
              borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
            text: plan.blocked,
          }),
          ...deps,
          h('div', {
            style: { marginTop: '4px', padding: '9px 11px', border: '1px solid var(--danger)',
              background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '13px', lineHeight: '1.7' },
            text: '强制卸载会破坏这些包：' + forceList + '（它们会缺依赖、可能无法运行）。'
              + '执行的命令：brew uninstall --ignore-dependencies ' + (plan.formula || formula),
          }),
        ]),
        footer: [
          h('button.btn', { text: '取消', onclick: () => dm.close() }),
          h('button.btn.btn-danger', {
            text: '强制卸载', title: '忽略依赖强行卸载（会破坏 ' + forceList + '）',
            onclick: () => { dm.close(); openConfirm(true); },
          }),
        ],
      });
      return; // 等用户在对话框里选；取消 = 什么都不做（不发请求）
    }

    openConfirm(false);
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
