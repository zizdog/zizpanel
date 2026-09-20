// sites.js —— 网站管理页面。
//
// 交互设计参考宝塔但做了取舍：
//   - 列表页直接给出"配置是否存在""PHP 是否在跑""证书到期"这些真实状态，
//     而不是只显示数据库里的记录 —— 面板与 nginx 状态不一致是最常见的求助原因。
//   - 新建向导把「伪静态模板」和「PHP 版本」放在一起，因为它们互相影响
//     （Laravel/ThinkPHP 的运行目录要落到 public）。
//   - 每个站点提供"诊断"入口，一键做完 HTTP 探测、PHP 探针、证书检查、错误日志。

import { api, apiURL } from './api.js';
import {
  h, clear, toast, modal, confirmBox, failureToast,
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
// 「上传与执行上限」的实现不再单独开弹窗：它现在是「⚙️ 调整配置」里的一个页签
// （用户要求把重复入口整合成一个）。渲染实现仍在 views.js 的
// renderLimitsInto 里，由 nginxpanel.js 直接复用。
// 宝塔式「⚙️ 调整配置」（nginx 四页 + 上传与执行上限 + 配置文件 + PHP 环境）。
import { adjustConfigModal } from './nginxpanel.js';
// 网络失败判据与统一文案（唯一前端真源，见 netfail.js；后端同名判据见 services/netfail.go）。
import { isNetworkFailureText, networkHintBlock } from './netfail.js';

let cache = null; // 站点列表数据（含预设与 PHP 版本）

// defSite 是「默认站点」的真实状态（GET /api/v1/system/default-site）。
//
// 用户要求"面板安装完成后要建立一个默认静态站点"：面板启动时会自动建一次，
// 这个状态就是"到底建成了没有"的唯一显示来源 —— null = 还没读到（既不说成
// 建好了，也不说成失败）。
let defSite = null;

// 站点详情面板当前所在的 Tab
let detailTab = 'basic';

// togglePending 是"启停在飞"的域名集合：请求/确认期间重复点击直接忽略（防连点）。
// 只做防重入，**不改按钮外观** —— 状态一律以回执为准，不做乐观更新。
const togglePending = new Set();

// NET_HINT_ENTRIES 是本页"去哪改镜像/代理"的入口（各页入口不同）。
const NET_HINT_ENTRIES = '面板设置 → 访问与安全 →「应用包镜像基址」；Docker 页 →「加速源」（Docker 镜像）。';

// netFailureHost 是本页的提示容器（SitesView 挂载）。LNMP / Nginx 安装任务在没有
// 网络时失败，要在这里说清"是网络问题"，而不是让用户以为建站功能坏了。
let netFailureHost = null;

// showNetFailure 只在判据命中时画醒目块（非网络失败一个字都不加）。
function showNetFailure(errText) {
  if (!netFailureHost || !isNetworkFailureText(errText)) return;
  clear(netFailureHost);
  netFailureHost.append(networkHintBlock(errText, NET_HINT_ENTRIES));
}

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
  + '再安装 nginx + PHP + 数据库 + phpMyAdmin（数据库管理界面），'
  + '并完成默认站点、vhosts 目录、数据库初始化、系统级守护进程等收尾工作。'
  + '点开会先让你选 nginx / PHP / 数据库的版本。'
  + '全程约十几分钟（取决于网络与 Homebrew 下载/编译速度）。'
  + '提交后立刻返回任务号，进度在「任务中心」实时显示 —— 关掉窗口、切换页面都不会中断安装。';

// LNMP_MYSQL_HINT 是 MySQL root 口令的说明（用户明确要求"在这个界面上显著说明"）。
//
// 任务中心里还有一条同义的常驻提示条（tasks.js，按标题 /LNMP/i 匹配），
// 两处都在：用户决定点确认之前就要知道"装 MySQL 会问口令、可以不干预"，
// 而不是等任务开起来才看到。
const LNMP_MYSQL_HINT = '装数据库时会问一次 root 口令（在任务中心里，60 秒不回答就自动生成强随机口令）'
  + ' —— **可以不干预**：装完后到「数据库 → 账号与权限」里直接点一下就能改成你想要的口令。';

// lnmpGroupOrder 是弹窗里组件分组的展示顺序（与后端 groups 的 key 对应）。
//
// 写在这里而不是直接用后端顺序：后端保证顺序（有测试锁死），但前端自己也该有
// 一个明确的顺序概念 —— 万一后端漏了一组，下面的"缺组"检查会如实报出来，
// 而不是悄悄少渲染一行。
const LNMP_GROUP_ORDER = [
  { key: 'nginx', label: 'Nginx', desc: 'Web 服务器（网站入口，只有一个版本）' },
  { key: 'php', label: 'PHP', desc: '站点解析 PHP 用；面板让每个版本监听自己的专属 socket，可以多版本共存' },
  { key: 'mysql', label: '数据库', desc: '默认 MariaDB；可与 MySQL 二选一，两者只装一个。默认共用数据目录 /opt/homebrew/var/mysql。' },
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
        // 三件套要联网下载：网络类失败在页面上给醒目块（非网络失败一个字都不加）。
        onDone: (task) => {
          if (task && task.status && task.status !== 'succeeded') {
            showNetFailure(task.error || task.status);
          }
        },
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
    // 数据库引擎维度：后端用它决定装 mysql@8.4 还是 mariadb（与 mysql 字段必须一致）。
    sel.db_engine = sel.mysql === 'mariadb' ? 'mariadb' : 'mysql';
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
          '（nginx + PHP 8.2 + MariaDB）。' }),
        h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px' } }, [
          h('button.btn', { text: '重试', onclick: () => load() }),
          h('button.btn.btn-primary', {
            text: '用默认版本安装（nginx + PHP 8.2 + MariaDB）',
            onclick: () => {
              // 显式默认：body 里仍然带上这三个 formula（不是"留空让后端决定"），
              // 这样日志与审计里能看出"用户就是选的默认"。
              const def = { nginx: 'nginx', php: 'php@8.2', mysql: 'mariadb', db_engine: 'mariadb' };
              if (m) m.close();
              taskCenter.start({
                kind: 'install',
                target: 'lnmp',
                title: '一键 LNMP',
                start: () => api.installLNMP(def),
                onDone: (task) => {
                  if (task && task.status && task.status !== 'succeeded') {
                    showNetFailure(task.error || task.status);
                  }
                },
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
      // 数据库位只可能装一个（用户 2026-09-20）：这台机器已装另一个引擎时，
      // 提交必然被后端拒绝，所以在这里就先说清"装另一个前要先卸载它"。
      if (meta.key === 'mysql') {
        const installedOther = g.options.find((o) => o.installed && o.formula !== state.picked[meta.key]);
        if (installedOther) {
          row.append(h('div.hint', { style: { color: 'var(--warn, #b8860b)' },
            text: '这台机器已装 ' + (installedOther.name || installedOther.formula) +
              '：两个引擎只能装一个，装另一个前请先在终端卸载它。' }));
        }
      }
      box.append(row);
    });

    // 三组都齐了才允许提交（后端也会校验一次，返回 400 + 原因）。
    const complete = LNMP_GROUP_ORDER.every((g) => state.picked[g.key] && byKey[g.key]);
    installBtn.disabled = !complete;
    syncInstallLabel();
  }

  // syncInstallLabel 让按钮上写着"这次会装什么" —— 用户在点之前就能核对。
  function syncInstallLabel() {
    const byKey = {};
    (state.groups || []).forEach((g) => { byKey[g.key] = g; });
    const labels = LNMP_GROUP_ORDER
      .map((g) => {
        const f = state.picked[g.key];
        if (!f) return '';
        if (f.includes('@')) return g.label + ' ' + f.split('@')[1];
        // 没有 @版本 的（nginx / mariadb）用目录展示名，不要露出裸 formula。
        const opts = (byKey[g.key] && byKey[g.key].options) || [];
        const hit = opts.find((o) => o.formula === f);
        return (hit && hit.name) || f;
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

  // defSiteNotice：默认站点没建好时的**醒目提示条**。
  //
  // 为什么必须有它（用户报障"全新安装的面板，没有默认网站！"）：
  // 以前只有工具条上一颗小药丸，而空列表那句提示说的是"新建站点需要
  // nginx+PHP+MySQL" —— 默认站点的事一个字都没提。用户在新机器上看到的
  // 就是"这里什么都没有"，根本不知道该有一个默认站点。
  // 这里如实说清状态，并且给一颗**能走通**的按钮（弹窗里缺 nginx 就先只装 nginx）。
  const defSiteNotice = h('div');
  // netNotice：LNMP / Nginx 安装任务的网络类失败在页面顶部给醒目块。
  const netNotice = h('div');
  netFailureHost = netNotice;

  const toolbar = h('div.card-head', [
    h('h3', { text: '站点列表' }),
    h('div.spacer'),
    statusBar,
  ]);

  content.append(
    h('div.card', [
      toolbar,
      h('div.card-body.tight', [netNotice, defSiteNotice, webEnvLine, listBox]),
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
    // 加载骨架：等待期间给出"内容要来了"的形状（配合 app.css 的 .skeleton 微光），
    // 而不是让用户干等一行字。
    listBox.append(h('div', { style: { padding: '8px 2px' } }, [
      h('div.skeleton', { style: { width: '58%' } }),
      h('div.skeleton', { style: { width: '42%' } }),
      h('div.skeleton', { style: { width: '70%' } }),
      h('p.hint', { style: { marginTop: '10px' }, text: '正在读取站点…' }),
    ]));
    // 默认站点状态与站点列表**并行**拉（只读文件探测，没有 brew / 网络开销），
    // 但两个都要等：列表第一行就是默认站点，先画表格再补状态会让这一行闪一下
    //（甚至短暂显示"状态未知"）。失败不阻断列表：状态退回 null，
    // 那一行如实写"状态未知"，绝不假装"已创建"。
    const [sitesRes, defRes] = await Promise.allSettled([api.sites(), api.defaultSite()]);
    defSite = defRes.status === 'fulfilled' ? (defRes.value || null) : null;
    if (sitesRes.status === 'rejected') {
      clear(listBox);
      listBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取站点失败' }),
        h('p', { text: sitesRes.reason.message }),
      ]));
      // 站点接口挂了也要给网站环境状态：PHP 那条只能退回服务列表（cache 里没有 php_versions）。
      renderStatus();
      void refreshWebEnv();
      return;
    }
    cache = sitesRes.value;
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

  // nginxState 给出工具条上那颗 nginx 按钮的**文字与判据**。
  //
  // 判据优先用**运行体证据**（GET /api/v1/sites/runtime → priv.NginxStatus() 的
  // pgrep 结果），服务记录只在运行体层读不到时兜底：
  //   运行体说在跑      → "nginx 运行"
  //   运行体说没进程    → "nginx 停止"
  //   运行体层读不到    → 退回服务记录；两者都没有 → "nginx 状态未知"
  //
  // 2026-09-18 的报障就是"只看服务记录"造成的：nginx 在 :80 上正常服务、面板里
  // 却没有它的记录 → 药丸写"未知"、环境行写"未就绪"。**没看见 ≠ 没在跑。**
  function nginxState() {
    const rt = webEnv.runtime && webEnv.runtime.nginx;
    if (webEnv.runtimePhase === 'loading' && webEnv.servicesPhase === 'loading') {
      return { text: 'nginx 状态读取中…', cls: '', title: '正在探测 nginx 进程与服务记录' };
    }
    if (rt) {
      if (rt.running) {
        return {
          text: 'nginx 运行',
          cls: '.zp-on',
          title: (rt.evidence || '运行体探测说它在跑')
            + (rt.registered ? '' : '\n（面板里没有它的服务记录：能看状态，但重启/停止要在「应用」里先接入）')
            + '\n\n悬停/点击这里可以展开：校验配置 / 修复 Nginx 环境 / 重建全部配置',
        };
      }
      return {
        text: 'nginx 停止',
        cls: '.zp-off',
        title: (rt.evidence || '运行体探测没找到 nginx 进程')
          + (rt.probe_error ? '\n探测错误：' + rt.probe_error : '')
          + '\n\n悬停/点击这里可以展开：校验配置 / 修复 Nginx 环境 / 重建全部配置',
      };
    }
    if (webEnv.nginxState === 'running') {
      return { text: 'nginx 运行', cls: '.zp-on', title: '服务列表里 nginx 条目 state.running = true' };
    }
    if (webEnv.nginxState === 'stopped') {
      return {
        text: 'nginx 停止',
        cls: '.zp-off',
        title: '服务列表里 nginx 条目 state.running = false',
      };
    }
    return {
      text: 'nginx 状态未知',
      cls: '',
      title: webEnv.runtimePhase === 'error'
        ? '运行体探测失败，服务列表里也没有可用结论，无法判断 nginx 是否在跑：' + webEnv.runtimeErr
        : '服务列表里没有 nginx 条目（它可能不归面板登记）—— 面板没有证据说它在跑，也没有证据说它停了',
    };
  }

  // nginxRunButton 是工具条上的「nginx 运行 / nginx 停止」按钮（用户要求
  // 用它替换「⋯ 更多」）。它 hover / 聚焦 / 点击时下拉显示原来的二级操作：
  //   🧪 校验 nginx / 🔧 修复 Nginx 环境 / ♻️ 重建全部配置
  //
  // 三条打开路径缺一不可：hover 是鼠标用户的第一直觉；focus-within 让键盘用户
  // 也能展开（不只靠 :hover）；点击则兼容触屏（没有 hover）。菜单项本身是真按钮，
  // 所以 Tab 能走到、Enter 能触发。
  function nginxRunButton() {
    const st = nginxState();
    const wrap = h('div.zp-menu-wrap');
    const menu = h('div.zp-menu', { role: 'menu' });
    const btn = h('button.btn.btn-sm.zp-run-btn' + st.cls, {
      text: st.text, title: st.title, 'aria-haspopup': 'true', 'aria-expanded': 'false',
    });

    const onDocDown = (e) => { if (!wrap.contains(e.target)) closeMenu(); };
    const onKey = (e) => { if (e.key === 'Escape') closeMenu(); };
    function closeMenu() {
      wrap.classList.remove('open');
      btn.setAttribute('aria-expanded', 'false');
      document.removeEventListener('mousedown', onDocDown);
      document.removeEventListener('keydown', onKey);
    }
    function openMenu() {
      wrap.classList.add('open');
      btn.setAttribute('aria-expanded', 'true');
      document.addEventListener('mousedown', onDocDown);
      document.addEventListener('keydown', onKey);
    }
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      if (wrap.classList.contains('open')) closeMenu(); else openMenu();
    });
    btn.addEventListener('keydown', (e) => {
      if (e.key !== 'ArrowDown') return;
      e.preventDefault();
      openMenu();
      const first = menu.querySelector('button');
      if (first) first.focus();
    });

    // menuItem 的 onclick 一定先收起菜单再执行 —— 否则动作里打开的弹窗会被
    // 还开着的下拉菜单压住/抢焦点。
    const menuItem = (text, hint, act) => h('button.zp-menu-item', {
      type: 'button', title: hint,
      onclick: async () => { closeMenu(); await act(); },
    }, [
      h('span.zp-menu-item-text', { text }),
      h('span.zp-menu-item-hint', { text: hint }),
    ]);

    // 🧪 校验 nginx
    const validate = menuItem('🧪 校验 nginx', '对 nginx 配置跑一次语法校验（nginx -t）', async () => {
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
              text: '常见修法：按行号改那一行；或到「⚙️ 调整配置 → 配置文件」里打开对应文件修正。' +
                '面板改 nginx.conf 前会备份成 nginx.conf.zizpanel.bak，可直接用它还原。' }),
          ]),
          footer: (close) => [h('button.btn', { text: '关闭', onclick: close })],
        });
      } catch (e) { toast(e.message, 'err', 12000); }
    });

    // 🔧 修复 nginx 环境：把"面板自己的片段没被加载"这类故障变成一键可修。
    // 用户报障："反向代理用不了了！…unknown "connection_upgrade" variable"
    // —— 根因是 brew 重装/升级 nginx 把 nginx.conf 还原成出厂版，conf.d 的 include
    // 与 upgrade map 一起没了。这里补齐并复核 nginx -t，不再让用户去重装 nginx。
    const fixEnv = menuItem('🔧 修复 Nginx 环境',
      '反向代理报 unknown "connection_upgrade" variable、或站点配置保存后不生效时点它：' +
        '补齐面板的 nginx 片段加载与 WebSocket map，并复核 nginx -t', async () => {
        try {
          const r = await api.nginxEnsureEnv();
          const fixed = r.fixed || [];
          if (r.nginx_test_ok === false) {
            modal({
              title: 'nginx 配置校验仍未通过',
              wide: true,
              body: h('div', [
                h('div.hint', { text: fixed.length ? '已修复：\n' + fixed.join('\n') : '环境没有需要修的地方。' }),
                h('pre', {
                  style: { whiteSpace: 'pre-wrap', fontFamily: 'var(--mono)', fontSize: '12.5px',
                    background: 'var(--danger-soft)', padding: '10px 12px', borderRadius: '6px',
                    maxHeight: '320px', overflow: 'auto', userSelect: 'text' },
                  text: r.nginx_test || '（nginx 没有输出）',
                }),
              ]),
              footer: (close) => [h('button.btn', { text: '关闭', onclick: close })],
            });
            return;
          }
          toast(fixed.length ? `已修复 ${fixed.length} 项：${fixed[0]}` : 'nginx 环境本来就是好的，校验通过', 'ok', 9000);
          load();
        } catch (e) { toast('修复失败：' + e.message, 'err', 14000); }
      });

    // ♻️ 重建全部配置
    const rebuild = menuItem('♻️ 重建全部配置',
      '按当前数据库状态重新生成所有站点的 nginx 配置（用于修复被手工改坏的配置）', async () => {
        if (!await confirmBox('将按模板重新生成所有站点的 nginx 配置并重载。\n\n你自己手工加在 vhost 里的内容会被覆盖（面板只保留数据库中的设置）。\n\n继续？', { title: '重建全部配置' })) return;
        try {
          const r = await api.siteReloadAll();
          const n = (r.rebuilt || []).length;
          if ((r.failed || []).length) toast(`重建 ${n} 个，失败 ${r.failed.length} 个：${r.failed[0]}`, 'warn', 12000);
          else toast(`已重建 ${n} 个站点配置`, 'ok');
          load();
        } catch (e) { toast(e.message, 'err', 9000); }
      });

    menu.append(validate, fixEnv, rebuild);
    wrap.append(btn, menu);
    return wrap;
  }

  // renderDefSiteNotice 画"默认站点没建好"的提示条（见 defSiteNotice 的说明）。
  //
  // 判据只有一条：defSite.applied === false。null（还没读到）不出提示 ——
  // 不拿"没读到"当"没建"；已就绪时这一段是空的，不占地方。
  function renderDefSiteNotice() {
    clear(defSiteNotice);
    if (!defSite || defSite.applied) return;
    const noNginx = defSite.nginx_present === false;
    const foreign = !!defSite.foreign_vhost;
    const title = noNginx ? '🏠 默认站点还没有创建：这台机器上还没有 Nginx'
      : foreign ? '🏠 80 端口上是别人写的站点，面板没有自动覆盖它'
        : '🏠 默认站点还没有创建';
    const detail = defSite.needs_action
      || (noNginx
        ? '默认站点要靠 nginx 监听 80 端口。可以只装 Nginx（不装 PHP 与 MySQL），装好后面板会自动建一个纯静态默认站点。'
        : '点右边按钮即可创建（只写面板自己的那份 vhost + 一张占位页，不需要 PHP 与 MySQL）。');
    defSiteNotice.append(h('div', {
      style: {
        display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap',
        border: '1px solid var(--warn)', borderRadius: '8px',
        padding: '10px 12px', marginBottom: '10px',
        background: 'color-mix(in srgb, var(--warn) 8%, transparent)',
      },
    }, [
      h('div', { style: { flex: '1 1 320px', minWidth: '260px' } }, [
        h('div', { style: { fontWeight: '600' }, text: title }),
        h('div', { style: { fontSize: '12.5px', lineHeight: '1.7', color: 'var(--text-dim)', marginTop: '3px' }, text: detail }),
        defSite.error ? h('div', { style: { fontSize: '12px', color: 'var(--text-mute)', marginTop: '3px' }, text: '上次尝试：' + defSite.error }) : null,
      ]),
      // 按钮复用**同一个**弹窗（缺 nginx → 先只装 nginx，然后立刻建站点）：
      // 另写一条快捷路径就会多出第二套实现，早晚走样。
      h('button.btn.btn-sm.btn-primary', {
        text: noNginx ? '只安装 Nginx' : (foreign ? '覆盖为面板默认站点' : '立即创建默认站点'),
        onclick: defaultSiteModal,
      }),
    ]));
  }

  // applyDefaultSite 执行"创建 / 重建默认站点"（列表第一行与「🏠 默认站点」弹窗
  // **共用同一条路径**：两处各写一遍必然走样，尤其是 foreign_vhost 的确认与失败提示）。
  //
  // 返回 true 表示接口调用成功；失败会自己 toast（含后端原话），调用方据此决定要不要重画。
  async function applyDefaultSite() {
    const st = defSite || {};
    // 80 端口上已经有一份**不是面板生成的**配置（用户自己写的）：面板不会自动覆盖，
    // 只有用户明确点下去才覆盖 —— 而且会先备份。这条确认只在这里写一次。
    if (st.foreign_vhost && !await confirmBox(
      (st.vhost_path || '000-default.conf') + ' 不是面板生成的（可能是你自己写的 80 端口站点）。\n\n'
      + '继续会用面板的默认站点覆盖它，原文件会先备份成 .zizpanel.bak（不会丢）。\n\n继续？',
      { title: '覆盖为面板默认站点', okText: '覆盖（先备份）' })) return false;
    try {
      const r = await api.defaultSiteApply();
      toast((r && r.message) || '默认站点已创建', 'ok', 9000);
      load();
      return true;
    } catch (e) {
      toast('创建默认站点失败：' + ((e && e.message) || e), 'err', 14000);
      return false;
    }
  }

  // defaultSiteModal 是「默认站点」弹窗：说清状态、给一条能走通的路。
  //
  // 用户的要求是"装完就该有一个默认静态站点"。所以这里的按钮
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
                    showNetFailure(task.error || task.status);
                    toast('安装 Nginx 失败：' + (task.error || task.status), 'err', 12000);
                    return;
                  }
                  toast('Nginx 已安装，正在创建默认站点…', 'ok', 8000);
                  // 装完立刻接着建默认站点：用户点这一颗按钮的意图就是"给我一个能访问的站点"，
                  // 不该再让他回来点第二次。失败时如实报错（不静默）。
                  // 走与列表行同一份 applyDefaultSite —— 不写第二套创建逻辑。
                  await applyDefaultSite();
                  await refreshDefaultSite();
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
          // 创建/重建的确认与失败提示只有 applyDefaultSite 一份实现
          //（列表第一行那颗「重建」按钮走的是同一条路径）。
          onclick: async () => {
            if (await applyDefaultSite()) await refreshDefaultSite();
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


  // ---------- ⚙️ 配置文件（手动改 nginx / php.ini / my.cnf 的入口）----------
  //
  // 用户 2026-09-20："面板也要有手动改 php 和 nginx 的入口啊！这是基本操作。"
  //
  // 清单由后端给（api.sites() 的 config_files 字段）：brew 前缀在 Apple Silicon
  // 是 /opt/homebrew、Intel 是 /usr/local，前端拼字符串一定会在另一种机器上错。
  // 每一条的「📝 编辑」都复用 services.js 的 configFileModal（同一套文件接口与
  // 保存/重启语义）；文件不存在时按钮照点，编辑器会如实报原因。
  // renderConfigFilesInto 把清单画进「⚙️ 调整配置 → 配置文件」页（原来是独立弹窗
  // 「网站环境配置文件」，用户要求合并成一个入口）。
  async function renderConfigFilesInto(box) {
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
          h('p', { text: '后端没有返回清单（面板版本可能较旧）；也可以先在「nginx 运行 → ♻️ 重建全部配置」之后再试。' }),
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
          h('div', { text: '· nginx 配置改完要重新加载 nginx（本条目的「🔄 重启服务」；只想重载可用工具条的「nginx 运行 → 🧪 校验 nginx」先确认语法，再保存任意站点触发自动重载）' }),
          h('div', { text: '· php.ini / php-fpm.conf / www.conf 改完要重启对应版本的 php-fpm' }),
          h('div', { text: '· my.cnf 改完要重启 MySQL' }),
          h('div', { text: '重启按钮点下去如果失败，会给出失败原因（面板没登记该服务 / 权限不足等），不会假装成功。' }),
        ]),
      );
    }
    await load();
  }

  function renderStatus() {
    clear(statusBar);
    renderDefSiteNotice();
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
      h('span.pill', { text: `共 ${list.length} 个站点` }),
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
      ...(list.length && lnmpNeeded() ? [lnmpButton(false)] : []),
      // nginx 状态与三个低频操作合成一颗按钮：hover / 聚焦 / 点击都会展开二级菜单
      // （用户要求用它替换「⋯ 更多」，状态本身仍然只来自真实探测）。
      nginxRunButton(),
      // 🏠 默认站点：装完面板就该有的那个静态站点。它同时是列表里的第一行；
      // 这里保留工具条入口，是为了"还没建 / 缺 nginx"时能一键补上。
      h('button.btn.btn-sm', {
        text: '🏠 默认站点',
        title: '查看/创建面板的默认静态站点（www/localhost，监听 80 端口的兜底站点；不需要 PHP 与 MySQL）',
        onclick: defaultSiteModal,
      }),
      // ⚙️ 调整配置：原来三颗按钮（Nginx 管理 / 上传大小 / 执行时间 / 配置文件）合并成
      // 这一颗（用户要求）。PHP 版本与端点也在里面的一页 ——
      // 原来那颗「🐘 PHP 环境 · x/y 运行中」按钮已删除（用户明确要求去掉）。
      h('button.btn.btn-sm', {
        text: '⚙️ 调整配置',
        title: 'nginx（服务 / 性能调整 / 配置修改 / 错误日志）、上传与执行上限、配置文件清单、PHP 环境'
          + (phps.length
            ? `\n当前 PHP：${phpRunning}/${phps.length} 运行中`
              + phps.map((p) => `\n· ${p.version} ${p.running ? '运行中' : '未运行'} ${p.pass || p.listen_err || '端点未解析'}`).join('')
            : ''),
        onclick: () => openAdjustConfig(),
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 新建站点', onclick: newSiteModal }),
      // 🖼️ Piwigo 一键建站：站点 + PHP + 数据库 + 官方发行包一次做完（见 api_sites_piwigo.go）。
      h('button.btn.btn-sm', {
        text: '🖼️ Piwigo 一键建站',
        title: '一次动作建好站点、专用数据库并解压官方 Piwigo 发行包；装完打开安装向导收尾（需要 PHP 8.2+）',
        onclick: piwigoModal,
      }),
    );
    statusBar.append(...bar);
  }

  // openAdjustConfig 打开合并后的「⚙️ 调整配置」弹窗。
  //
  // 页面结构与数据来源（用户的合并要求）：
  //   nginx 组（服务 / 性能调整 / 配置修改 / 错误日志）—— nginxpanel.js 自带
  //   上传与执行上限 —— views.js 的 renderLimitsInto（唯一一份实现）
  //   配置文件 / PHP 环境 —— 通过 opts.extra 注入（它们要用到本作用域的站点缓存
  //   与卸载回调，搬进 nginxpanel.js 反而会绕成互相 import）
  function openAdjustConfig(page) {
    adjustConfigModal({
      page,
      paths: {
        nginx_conf: nginxConfPath(),
        site_error_log: (cache && cache.nginx_error_log) || '',
        brew_error_log: (cache && cache.nginx_brew_error_log) || '',
      },
      extra: [
        {
          id: 'files',
          label: '配置文件',
          desc: 'nginx.conf / 各站点 vhost / php.ini / my.cnf 的手工编辑入口（路径全部由后端给，前端不拼字符串）',
          render: renderConfigFilesInto,
        },
        {
          id: 'php',
          label: 'PHP 环境',
          desc: '已安装的 PHP 版本、各自的 FastCGI 端点与运行状态',
          render: renderPHPEnvInto,
        },
      ],
    });
  }

  // nginxConfPath 从后端给的配置文件清单里找 nginx 主配置的真实路径。
  // 找不到就返回空串（界面显示"路径未知"）—— 前端绝不自己拼 /opt/homebrew。
  function nginxConfPath() {
    const files = (cache && cache.config_files) || [];
    const hit = files.find((f) => f && f.service === 'nginx' && /nginx\.conf$/.test(String(f.path || '')));
    return (hit && hit.path) || '';
  }

  function renderList() {
    clear(listBox);
    const list = (cache && cache.list) || [];

    // 表格**永远**画出来，第一行是默认站点。
    //
    // 用户："默认站点要和宝塔完全一样，安装完用户就可以在网站管理里面
    // 看到有这样一个默认站点" —— 所以哪怕 sites 表里一条记录都没有，首页也要有这一行。
    //
    // ⚠️ **不往 sites 表里插记录**：站点模板生成器会按站点记录整份重写 vhost，
    // 而默认站点用的是它专用模板（000-default.conf）—— 插记录等于让「重建全部配置」
    // 把默认站点覆盖成普通站点模板。这一行只是 GET /api/v1/system/default-site 的
    // **只读投影**，动作（打开/重建/查看状态）全部走既有的默认站点接口。
    const tbody = h('tbody', [
      defaultSiteRow(),
      ...list.map((s) => siteRow(s)),
    ]);
    listBox.append(h('div.zp-table-wrap', [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '域名' }), h('th', { text: '状态' }), h('th', { text: 'PHP' }),
          h('th', { text: '路由' }), h('th', { text: 'SSL' }), h('th', { text: '运行目录' }), h('th', { text: '操作' }),
        ])]),
        tbody,
      ]),
    ]));

    if (list.length) return;

    // 还没有**自己的**站点：在表格下面保留原来的空态说明与入口。
    // 默认站点那一行已经在上面，所以这里不再重复"本来就该有一个默认站点"的口径。
    listBox.append(h('div.empty', [
      h('div.big', { text: '🌐' }),
      h('h4', { text: '还没有自己的站点' }),
      h('p', { text: '表格第一行是面板装好后自带的**默认站点**（纯静态，不需要 PHP 与 MySQL）；' +
        '它建好了没有由那一行的状态与上面的提示条如实反映。' }),
      h('p', { text: '新建自己的站点才需要 nginx + PHP + MySQL：环境还没装的话，用「一键 LNMP」一次装好；' +
        '环境已就绪的话，直接新建站点即可。' }),
      // 空列表时 LNMP 是**大按钮**（用户要求：醒目但不过度）：
      // 没有环境的话，先建站点也跑不起来，所以它排在最前面。
      //
      // 与工具条同一判据：环境完整（两层都读到且都不缺）时**不出现**；
      // 读不到时照常出现（不拿"没读到"当真完整）。
      h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center', flexWrap: 'wrap' } }, [
        // 默认站点还没建 → 这颗排最前（它就是"新机器上第一件该做的事"）。
        ...(defSite && !defSite.applied
          ? [h('button.btn.btn-primary', {
            text: defSite.nginx_present === false ? '🏠 创建默认站点（先装 Nginx）' : '🏠 创建默认站点',
            onclick: defaultSiteModal,
          })]
          : []),
        ...(lnmpNeeded() ? [lnmpButton(true)] : []),
        h('button.btn', { text: '新建第一个站点', onclick: newSiteModal }),
      ]),
    ]));
  }

  // siteRow 画一个**已注册站点**的行。
  function siteRow(s) {
    return h('tr', [
      h('td', [
        h('div', { style: { fontWeight: '600', display: 'flex', alignItems: 'center', gap: '6px' } }, [
          domainLink(s),
          (Number(s.listen_port) || 80) !== 80
            ? h('span.pill.brand', { text: ':' + s.listen_port, title: '该站点监听端口 ' + s.listen_port })
            : null,
        ]),
        s.aliases ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.aliases }) : null,
        s.remark ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.remark }) : null,
        // 一键建站建的站点（有 install_db）：把「安装向导」这条收尾入口直接摆在行里。
        s.install_db
          ? h('div', { style: { fontSize: '11.5px' } }, [
            h('a', {
              href: wizardURL(s), target: '_blank', rel: 'noopener', text: '🧭 安装向导',
              title: '打开 ' + wizardURL(s) + ' 完成 Piwigo 安装（库名/账号/口令见建站任务结果）',
            }),
          ])
          : null,
        addressInfo(s),
      ]),
      h('td', [
        h('span.pill' + (s.conf_exists ? '.ok' : '.danger'), {
          text: s.conf_exists ? (s.enabled ? '运行中' : '已停用') : '配置缺失',
          title: s.conf_exists ? 'nginx 配置文件存在' : '数据库有这个站点，但 nginx 配置文件不存在 —— 请用「nginx 运行 → ♻️ 重建全部配置」',
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
          siteToggleButton(s),
          h('button.btn.btn-sm', { text: '管理', onclick: () => openDetail(s.domain) }),
          h('button.btn.btn-sm', { text: '诊断', onclick: () => runCheck(s.domain) }),
          h('button.btn.btn-danger.btn-sm', { text: '删除', onclick: () => delSite(s) }),
        ]),
      ]),
    ]);
  }

  // siteToggleButton 是列表行的一键启停：文案只看 s.enabled，与编辑弹窗同一语义。
  // 停止 = enabled:false（从 nginx 移除 vhost 并 reload），启动 = enabled:true（重建 + reload）。
  function siteToggleButton(s) {
    const stopping = !!s.enabled;
    const btn = h(`button.btn.${stopping ? 'btn-ghost' : 'btn-primary'}.btn-sm`, {
      text: stopping ? '停止' : '启动',
      title: stopping ? '停止该站点：从 nginx 移除配置并重载' : '启动该站点：重新生成配置并重载',
      onclick: () => toggleSite(s),
    });
    return btn;
  }

  // toggleSite 二次确认后切 enabled，成功/失败都重拉列表；失败贴后端原文（不谎报）。
  async function toggleSite(s) {
    if (togglePending.has(s.domain)) return; // 防连点：确认框/请求在飞时忽略重复提交
    const enable = !s.enabled;
    togglePending.add(s.domain);
    try {
      const okBox = await confirmBox(
        enable
          ? '启动会重新生成 nginx 配置并重载，站点恢复对外服务。继续？'
          : '停止会从 nginx 移除配置并重载，站点停止对外服务。继续？',
        { title: (enable ? '启动站点：' : '停止站点：') + s.domain, danger: !enable });
      if (!okBox) return;
      await api.siteUpdate(s.domain, { enabled: enable });
      toast(enable ? '已启动 ' + s.domain : '已停止 ' + s.domain, 'ok');
      await load();
    } catch (e) {
      toast(e.message, 'err', 9000);
    } finally {
      togglePending.delete(s.domain);
    }
  }

  // siteOpenTarget 计算站点「打开」地址：被反向代理规则前置时走公网入口（坑 214）。
  // 打开地址只在这里算一份 —— 别处再拼一遍就会漏掉规则、退回 80。
  function siteOpenTarget(s) {
    const pe = s && s.public_entry;
    if (pe && pe.available && pe.public_url) {
      return { href: pe.public_url, viaRule: pe.rule_name || '', note: pe.note || '' };
    }
    const host = String(s.domain || '').replace(/:\d+$/, '');
    if (!host) return { href: '', viaRule: '', note: (pe && pe.note) || '' };
    const scheme = s.ssl_enabled ? 'https' : 'http';
    const port = Number(s.listen_port) || 80;
    const dial = !s.ssl_enabled && port !== 80 ? ':' + port : '';
    return { href: scheme + '://' + host + dial + '/', viaRule: '', note: (pe && pe.note) || '' };
  }

  // wizardURL 拼一键建站站点的安装向导地址：基址与「打开」同源（不另拼 80 端口）。
  function wizardURL(s) {
    const base = siteOpenTarget(s).href || ('http://' + String(s.domain || '') + '/');
    return base.replace(/\/+$/, '') + '/install.php';
  }

  // domainLink 把**域名文本本身**做成链接（用户明确要求）。
  //
  // 用户原话：不要 `blog.zizdog.com` / `http://blog.zizdog.com` / `https://blog.zizdog.com`
  // 三种形式都列出来 —— 只显示域名文本本身，并且这段文字本身可点、新标签打开。
  // 链接地址必须走 siteOpenTarget：有反代规则时用公网入口，而不是站点自己的 80。
  function domainLink(s) {
    const host = String(s.domain || '').replace(/:\d+$/, '');
    if (!host) return h('span', { text: s.domain || '' });
    const target = siteOpenTarget(s);
    const notes = ['在新窗口打开 ' + target.href];
    if (target.viaRule) {
      notes.push('该域名由反向代理规则「' + target.viaRule + '」前置，按公网入口打开');
    } else {
      notes.push(s.ssl_enabled
        ? '该站点已开启 SSL：https 可用'
        : '该站点未开启 SSL：这里用 http 打开；直接访问 https://' + host + '/ 会提示证书不受信任');
    }
    if (target.note) notes.push(target.note);
    if (!s.conf_exists) {
      notes.push('nginx 配置文件不存在（数据库有记录、nginx 没在服务）');
    } else if (!s.enabled) {
      notes.push('站点已停用（nginx 配置文件在，但不服务）');
    }
    return h('a', {
      href: target.href, target: '_blank', rel: 'noopener',
      text: s.domain, title: notes.join('；'),
    });
  }

  // addressInfo 用小字如实标出协议与可达状态。
  //
  // 这是"不丢信息"的那一半：不再列出三种网址形态，但协议（http/https）、
  // 自定义端口与"配置在不在、启没启用"仍然看得见；有反代规则时标明公网入口。
  function addressInfo(s) {
    const host = String(s.domain || '').replace(/:\d+$/, '');
    if (!host) return null;
    const target = siteOpenTarget(s);
    const state = !s.conf_exists
      ? '⚠ nginx 配置缺失'
      : (s.enabled ? '配置存在、已启用' : '配置存在、已停用');
    let proto;
    if (target.viaRule) {
      proto = (target.href.indexOf('https://') === 0 ? '🔒 https' : 'http') +
        ' · 公网入口（规则「' + target.viaRule + '」）';
    } else {
      const port = Number(s.listen_port) || 80;
      proto = s.ssl_enabled ? '🔒 https' : (port === 80 ? 'http' : 'http:' + port);
    }
    return h('div.zp-addr', [
      h('span', {
        style: { fontSize: '11.5px', color: 'var(--text-mute)' },
        text: proto + ' · ' + state,
      }),
    ]);
  }

  // defaultSiteRow 把默认站点画成列表里的第一行（用户要求与宝塔一致）。
  //
  // 状态只有三种：已就绪 / 还没建好（含缺 nginx、80 端口是别人写的） / 读不到。
  // **读不到就写"状态未知"**，绝不假装已创建。
  function defaultSiteRow() {
    const st = defSite || {};
    const applied = !!(defSite && st.applied);
    const host = String(st.url || '').replace(/^https?:\/\//, '').replace(/\/+$/, '');
    const root = String(st.index_path || '').replace(/\/index\.html$/, '');
    const pill = defSite === null
      ? h('span.pill', { text: '状态未知', title: '还没读到默认站点状态（或读取失败）—— 面板没有证据，不假装已创建' })
      : (applied
        ? h('span.pill.ok', { text: '运行中', title: '默认站点已就绪：' + (st.vhost_path || '') })
        : (st.nginx_present === false
          ? h('span.pill.warn', { text: '待创建（缺 nginx）', title: st.needs_action || '这台机器还没有 nginx' })
          : (st.foreign_vhost
            ? h('span.pill.warn', { text: '未接管（非面板生成）', title: st.needs_action || '80 端口上已有一份不是面板生成的配置；面板不会自动覆盖它' })
            : h('span.pill.warn', { text: '未创建', title: st.needs_action || '点「创建」即可（只写面板自己的那份 vhost + 一张占位页）' }))));
    return h('tr.zp-default-row', [
      h('td', [
        h('div', { style: { fontWeight: '600' } }, [
          h('span', { text: '默认站点' }),
          h('span.pill.brand', { style: { marginLeft: '6px' }, text: '默认' }),
        ]),
        host
          ? h('div', { style: { fontWeight: '600' } }, [
            h('a', {
              href: 'http://' + host + '/', target: '_blank', rel: 'noopener',
              text: host, title: '在新窗口打开 http://' + host + '/（默认站点只监听 80 端口、没有 HTTPS）',
            }),
          ])
          : null,
        h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' },
          text: '80 端口兜底站点（面板自带，不占用「站点」表的记录）' }),
      ]),
      h('td', [pill]),
      h('td', h('span.pill', { text: '静态' })),
      h('td', h('span', { style: { fontSize: '12px', color: 'var(--text-dim)' }, text: '未匹配域名时接住' })),
      h('td', h('span.pill.warn', { text: 'http' })),
      h('td.mono', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: root || '（未创建）' }),
      h('td', [
        h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap' } }, [
          h('button.btn.btn-sm', {
            text: '打开',
            title: applied ? '在新窗口打开 http://' + host + '/' : '默认站点还没创建 —— 先点「创建」，或点「查看状态」看原因',
            onclick: () => {
              if (!applied || !host) {
                toast('默认站点还没创建，暂时打不开 —— 点「查看状态」看原因', 'warn', 9000);
                return;
              }
              window.open('http://' + host + '/', '_blank', 'noopener');
            },
          }),
          h('button.btn.btn-sm', {
            text: applied ? '重建' : '创建',
            title: '按面板模板重写 ' + (st.vhost_path || '000-default.conf') + '（只影响默认站点这一份配置，别的站点不受影响）',
            onclick: () => applyDefaultSite(),
          }),
          h('button.btn.btn-sm', {
            text: '查看状态',
            title: '打开「🏠 默认站点」：状态、vhost 路径、创建/重建与排障入口',
            onclick: defaultSiteModal,
          }),
        ]),
      ]),
    ]);
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

  // ---------- PHP 环境 ----------
  // 把"装了哪些版本、各自听在哪、跑没跑、要不要修"一次说清，
  // 并且**每个版本一个修复按钮** —— 多版本共存的排障全靠这一屏。
  //
  // 它现在是「⚙️ 调整配置 → PHP 环境」那一页：用户要求删掉工具条上那颗
  // 「🐘 PHP 环境 · 1/1 运行中」按钮，把版本/端点信息并进调整配置弹窗。
  // 依旧画进调用方给的容器 —— 同一份渲染既能被弹窗用，也能被将来的别处用。
  async function renderPHPEnvInto(box) {
    clear(box);
    box.append(h('div.empty', [h('div.big', { text: '🐘' }), h('p', { text: '正在读取 PHP 版本…' })]));

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
          // 重拉 PHP 列表并重绘（回调由 renderPHPEnvInto 提供：它持有 state2/render）。
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
    const root = h('input.input', {
      placeholder: wwwRoot ? `留空用 ${wwwRoot}/<域名>` : '留空用 网站根目录/<域名>',
    });
    const port = h('input.input', { type: 'number', min: '1', max: '65535', value: '80' });
    const autoindex = h('input', { type: 'checkbox' });
    const proxyCache = h('input', { type: 'checkbox' });
    const rootPreview = h('div.hint', { text: '' });

    const updatePreview = () => {
      const d = domain.value.trim().toLowerCase();
      const p = presets.find((x) => x.name === preset.value);
      const base = root.value.trim() || (d ? `${wwwRoot}/${d}` : '');
      const dir = base && p && p.public_dir && !base.endsWith('/' + p.public_dir)
        ? `${base}/${p.public_dir}` : base;
      rootPreview.textContent = dir ? `运行目录：${dir}` : '运行目录：（请输入域名或根目录）';
      if (p && p.public_dir) {
        presetHint.textContent = `${p.label}：运行目录会自动设为 ${p.public_dir}/ 子目录。${p.description}`;
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
    root.addEventListener('input', updatePreview);
    updatePreview();

    const submit = async (close) => {
      const d = domain.value.trim().toLowerCase();
      if (!d) { toast('请输入域名', 'warn'); return; }
      const portNum = Number(port.value.trim() === '' ? 80 : port.value.trim());
      if (!Number.isInteger(portNum) || portNum < 1 || portNum > 65535) {
        toast('监听端口必须是 1-65535 的整数', 'warn');
        return;
      }
      try {
        const r = await api.siteCreate({
          domain: d,
          aliases: aliases.value.trim(),
          php_version: proxy.value.trim() ? '' : php.value,
          rewrite: proxy.value.trim() ? 'none' : preset.value,
          proxy_pass: proxy.value.trim(),
          remark: remark.value.trim(),
          root: root.value.trim(),
          listen_port: portNum,
          autoindex: autoindex.checked,
          proxy_cache: proxyCache.checked,
        });
        toast(`站点 ${d} 创建成功`, 'ok');
        if (r && (r.root_verified === false || r.listen_port_verified === false)) {
          const why = r.root_verify_error || r.listen_port_verify_error || '';
          toast(`有改动未复核${why ? '：' + why : ''}`, 'warn', 9000);
        }
        close();
        load();
        if (r && r.root) setTimeout(() => openDetail(d), 300);
      } catch (e) {
        failureToast(e, 16000);
      }
    };

    const m = modal({
      title: '新建站点',
      body: h('div', [
        h('div.field', [h('label', { text: '域名 *' }), domain, h('div.hint', { text: '不需要输入 www，附加域名写在下一个字段' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases]),
        h('div.field', [h('label', { text: '根目录' }), root, rootPreview,
          h('div.hint', {
            text: '留空用默认目录；可填外接盘里的目录',
            title: '支持 /Volumes 下的外接盘与家目录下的目录；系统目录会被拒绝。' +
              '选了 Laravel/ThinkPHP 时会自动接上 public 子目录。',
          })]),
        h('div.field', [h('label', { text: '监听端口' }), port,
          h('div.hint', {
            text: '默认 80；同一端口可放多个不同域名的站点',
            title: '例如 8090。面板/内置服务已占用、或同端口下有站点/反代规则用同一个域名时拒绝；' +
              '自定义端口只作用于 HTTP，开了 HTTPS 时 HTTPS 仍在 443。',
          })]),
        h('div.field', [h('label', { text: '路由 / 伪静态' }), preset, presetHint]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php, phpHint,
          h('div.hint', { text: '选择"纯静态"时，nginx 会拒绝执行该站点下的 PHP 文件' })]),
        h('div.field', [
          h('label', { text: '目录索引' }),
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            autoindex, h('span', { style: { fontSize: '13px' }, text: '开启目录索引（列出目录里的文件）' }),
          ]),
          h('div.hint', {
            text: '默认关闭；目录里没有首页文件时才会列出文件',
            title: '生成的 vhost 是 server 级 autoindex on，对本站所有目录生效。' +
              '有 index.php/index.html 的目录仍优先显示首页，不受影响。',
          }),
        ]),
        h('div.field', [
          h('label', { text: '回源缓存' }),
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            proxyCache, h('span', { style: { fontSize: '13px' }, text: '把上游文件缓存到本地，省流量、提速' }),
          ]),
          h('div.hint', {
            text: '改上游内容后可能要等缓存过期才更新',
            title: '开启后由面板声明 nginx 缓存区（zp_site_<id>），server 级 proxy_cache 会继承到本站所有回源 location。' +
              '200/301/302 缓存 7 天、404 缓存 1 分钟；上游出错时先给旧副本。关闭后配置与开启前逐字一致。',
          }),
        ]),
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

  // ---------- Piwigo 一键建站 ----------
  //
  // 一次动作：站点 + PHP + 专用库/账号 + 官方发行包。装完必须打开安装向导收尾
  // （管理员由向导创建；Piwigo 的向导只认表单输入，面板预写的配置不会自动带上）。
  function piwigoModal() {
    const phps = cache?.php_versions || [];
    const wwwRoot = cache?.www_root || '';
    // PHP 8.2+ 才放行（后端也会拒一次，前端先如实说清，不让用户白等一个任务）。
    const eligible = phps.filter((p) => phpAtLeast82(p.version));
    const domain = h('input.input', { placeholder: '例如：piwigo.test' });
    const php = h('select.select', eligible.map((p) => h('option', {
      value: p.version,
      text: phpOptionLabel(p),
      selected: p.is_default,
    })));
    const rootHint = h('div.hint', {
      text: wwwRoot ? `站点目录：${wwwRoot}/<域名>` : '站点目录：网站根目录/<域名>',
    });
    const phpHint = h('div.hint', {
      text: eligible.length
        ? 'Piwigo 需要 PHP 8.2+（官方系统要求）'
        : '本机没有 PHP 8.2+：请先到「应用市场」装 PHP 8.2/8.4，再回来建站',
    });
    const wizardBox = h('div');
    if (!eligible.length) {
      php.disabled = true;
      php.append(h('option', { value: '', text: '（本机没有 8.2+ 的 PHP）' }));
    }

    const submit = async (close) => {
      const d = domain.value.trim().toLowerCase();
      if (!d) { toast('请输入域名', 'warn'); return; }
      if (!eligible.length) { toast('本机没有 PHP 8.2+，无法建站', 'err'); return; }
      const chosenPHP = php.value || eligible[0].version;
      close();
      await taskCenter.start({
        kind: 'site-install',
        target: d,
        title: '一键建站 Piwigo（' + d + '）',
        start: () => api.marketInstallSite('piwigo', { domain: d, php: chosenPHP }),
        onDone: (task) => {
          load();
          if (!task || task.status !== 'succeeded') return;
          // 装完把"打开安装向导"这条唯一剩下的路直接摆出来（用站点域名）。
          showWizard(d);
        },
      });
    };

    const m = modal({
      title: 'Piwigo 一键建站',
      body: h('div', [
        h('div.field', [h('label', { text: '域名 *' }), domain,
          h('div.hint', { text: '校验沿用站点规则；不要填已在用的域名' })]),
        h('div.field', [h('label', { text: 'PHP 版本 *' }), php, phpHint, rootHint]),
        h('div.hint', {
          text: '面板会新建专用数据库与账号（口令随机生成，只显示在任务结果里）',
        }),
        h('div.hint', {
          text: '装完打开安装向导填写库信息并设置管理员；时间较长，进度在任务中心',
        }),
        wizardBox,
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', { text: '开始建站', onclick: () => submit(close) }),
      ],
    });
    setTimeout(() => domain.focus(), 60);
  }

  // phpAtLeast82 只看主次版本（Piwigo 官方要求 PHP 8.2+）。
  function phpAtLeast82(v) {
    const m = String(v || '').match(/^(\d+)\.(\d+)/);
    if (!m) return false;
    const major = Number(m[1]);
    const minor = Number(m[2]);
    return major > 8 || (major === 8 && minor >= 2);
  }

  // showWizard 给出"打开安装向导"的链接（地址走 siteOpenTarget，带站点域名）。
  function showWizard(domain) {
    const target = 'http://' + domain + '/install.php';
    const a = h('a', { href: target, target: '_blank', rel: 'noopener', text: target });
    modal({
      title: '安装向导：' + domain,
      body: h('div', [
        h('div', { style: { marginBottom: '10px' }, text: '站点已就绪，请打开安装向导完成最后一步：' }),
        h('div', { style: { marginBottom: '10px' } }, [a]),
        h('div.hint', {
          text: '向导里填安装结果中的库名/用户名/密码，地址选 localhost，管理员账号自己设',
        }),
      ]),
      footer: (close) => [h('button.btn.btn-primary', { text: '知道了', onclick: close })],
    });
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
      const wwwRoot = cache?.www_root || '';
      const aliases = h('input.input', { value: site.aliases || '' });
      const remark = h('input.input', { value: site.remark || '' });
      const enabled = h('input', { type: 'checkbox', checked: site.enabled });
      const root = h('input.input', {
        value: site.base_root || '',
        placeholder: wwwRoot ? `留空用 ${wwwRoot}/${site.domain}` : '留空用 网站根目录/<域名>',
      });
      const autoindex = h('input', { type: 'checkbox', checked: !!site.autoindex });
      const proxyCache = h('input', { type: 'checkbox', checked: !!site.proxy_cache });
      const port = h('input.input', {
        type: 'number', min: '1', max: '65535',
        value: String(site.listen_port || 80),
      });
      const rootState = h('div.hint', { text: `当前生效：${site.root}` });
      const portState = h('div.hint', { text: `当前监听：${site.listen_port || 80}` });
      const save = h('button.btn.btn-primary', {
        text: '保存',
        onclick: async () => {
          const portNum = Number(port.value.trim() === '' ? 80 : port.value.trim());
          if (!Number.isInteger(portNum) || portNum < 1 || portNum > 65535) {
            toast('监听端口必须是 1-65535 的整数', 'warn');
            return;
          }
          save.disabled = true;
          try {
            const r = await api.siteUpdate(domain, {
              aliases: aliases.value.trim(), remark: remark.value.trim(), enabled: enabled.checked,
              root: root.value.trim(), listen_port: portNum, autoindex: autoindex.checked,
              proxy_cache: proxyCache.checked,
            });
            if (r && r.site) {
              Object.assign(site, r.site);
              rootState.textContent = `当前生效：${r.root_applied || r.site.root}`;
              portState.textContent = `当前监听：${r.listen_port_applied || r.site.listen_port || 80}`;
            }
            toast('已保存', 'ok');
            if (r && (r.root_verified === false || r.listen_port_verified === false)) {
              const why = r.root_verify_error || r.listen_port_verify_error || '';
              toast(`有改动未复核${why ? '：' + why : ''}`, 'warn', 9000);
            }
            if (r && r.cache_enabled && r.cache_verified === false) {
              toast(`回源缓存未复核${r.cache_verify_error ? '：' + r.cache_verify_error : ''}`, 'warn', 9000);
            }
            load();
          } catch (e) { failureToast(e, 16000); }
          finally { save.disabled = false; }
        },
      });
      return h('div', [
        h('div.field', [h('label', { text: '主域名' }), h('div', [h('code.code', { text: site.domain })]),
          h('div.hint', { text: '域名创建后不可修改（改域名等于新建站点，涉及目录与证书）' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases, h('div.hint', { text: '多个用英文逗号分隔，例如 www.demo.test, m.demo.test' })]),
        h('div.field', [h('label', { text: '根目录' }), root, rootState,
          h('div.hint', {
            text: '留空用默认目录；可填外接盘里的目录',
            title: '支持 /Volumes 下的外接盘与家目录下的目录；系统目录会被拒绝。' +
              '改了会重新生成 nginx 配置并回读生效值。Laravel/ThinkPHP 会自动接上 public 子目录。',
          })]),
        h('div.field', [h('label', { text: '监听端口' }), port, portState,
          h('div.hint', {
            text: '默认 80；同一端口可放多个不同域名的站点',
            title: '例如 8090。面板/内置服务已占用、或同端口下有站点/反代规则用同一个域名时直接报错；' +
              '自定义端口只作用于 HTTP，开了 HTTPS 时 HTTPS 仍在 443。',
          })]),
        h('div.field', [
          h('label', { text: '目录索引' }),
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            autoindex, h('span', { style: { fontSize: '13px' }, text: '开启目录索引（列出目录里的文件）' }),
          ]),
          h('div.hint', {
            text: '默认关闭；目录里没有首页文件时才会列出文件',
            title: '生成的 vhost 是 server 级 autoindex on，对本站所有目录生效。' +
              '有 index.php/index.html 的目录仍优先显示首页，不受影响。',
          }),
        ]),
        h('div.field', [
          h('label', { text: '回源缓存' }),
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            proxyCache, h('span', { style: { fontSize: '13px' }, text: '把上游文件缓存到本地，省流量、提速' }),
          ]),
          h('div.hint', {
            text: '改上游内容后可能要等缓存过期才更新',
            title: '开启后由面板声明 nginx 缓存区（zp_site_<id>），server 级 proxy_cache 会继承到本站所有回源 location。' +
              '200/301/302 缓存 7 天、404 缓存 1 分钟；上游出错时先给旧副本。关闭后配置与开启前逐字一致。',
          }),
        ]),
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
          } catch (e) { failureToast(e, 16000); }
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
                  if (!await confirmBox('关闭后站点只监听自己的端口（默认 80）。继续？', { danger: true })) return;
                  try {
                    const r = await api.siteSSLDisable(domain);
                    Object.assign(site, r.site);
                    toast('已关闭 HTTPS', 'ok');
                    render(); load();
                  } catch (e) { failureToast(e, 14000); }
                },
              }),
            ]),
          );
          box.append(acmeSection());
          return;
        }
        box.append(
          h('div.hint', { style: { marginBottom: '14px' }, text: '开启 HTTPS 后，站点自己端口上的请求会 301 跳转到 443。' }),
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
          failureToast(e, 16000);
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
                } catch (e) { failureToast(e, 16000); }
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
            failureToast(e, 20000);
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
              } catch (e) { failureToast(e, 16000); }
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
    // 一键建站的站点才给"同时删除数据库"：默认保留（勾了才删，且后端只删面板建的那个库）。
    const installDB = s.install_db || '';
    const removeFiles = h('input', { type: 'checkbox' });
    const removeDB = h('input', { type: 'checkbox' });
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
        installDB ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', fontSize: '13px', marginTop: '8px' } }, [
          removeDB,
          h('span', { text: '同时删除数据库 ' + installDB + ' 及专用账号（不可恢复！）' }),
        ]) : null,
        h('div.hint', {
          style: { marginTop: '10px' },
          text: installDB
            ? '不勾选则只删除配置，文件与数据库都保留。'
            : '不勾选则只删除配置，文件保留，方便之后重新绑定。',
        }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-danger', {
          text: '确认删除',
          onclick: async () => {
            try {
              const q = new URLSearchParams();
              if (removeFiles.checked) q.set('remove_files', '1');
              if (installDB && removeDB.checked) q.set('remove_db', '1');
              const qs = q.toString();
              const r = await api.del(apiURL('sites/' + encodeURIComponent(s.domain) + (qs ? '?' + qs : '')));
              toast(r.msg + (r.files ? '（' + r.files + '）' : '') + (r.db ? '（' + r.db + '）' : ''), 'ok', 12000);
              close();
              load();
            } catch (e) { failureToast(e, 16000); }
          },
        }),
      ],
    });
  }

  registerCleanup(() => { });
  load();
}
