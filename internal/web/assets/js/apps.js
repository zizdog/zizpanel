// apps.js —— 应用市场页面。
//
// 核心思路：把"能不能装"和"装什么"分开。
//   - 用户点安装时，先跑一次安装前检查（依赖、端口），把问题一次说清
//   - 检查通过才真正安装；安装是**异步任务**，提交后进度交给「任务中心」，
//     用户可以关窗口、切页面，随时从顶栏重新打开看进度（见 tasks.js）
//   - 已经装过的应用显示"已安装"，可以一键跳到服务管理页

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup, panelPath } from './app.js';
import { taskCenter } from './tasks.js';

let cache = null;
// proxyState 是 /api/v1/market/proxies 的探测结果（slug → {proxy_ok, reason}）。
// 有界面的应用给两个入口：子路径 /<slug>/ 与直连端口；哪个能用由探测说了算，
// 而不是"我们配了就假设它能打开"。
let proxyState = null;

export function AppsView(content, ctx = {}) {
  clear(content);

  const grid = h('div');
  const head = h('div.card-head', [
    h('h3', { text: '应用市场' }),
    h('div.spacer'),
    h('div', { id: 'apps-head', style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);

  appendAll(content, 
    h('div.card', [head, h('div.card-body', [grid])]),
  );
  const headBox = head.querySelector('#apps-head');

  async function load() {
    clear(grid);
    appendAll(grid, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取应用目录…' })]));
    try {
      cache = await api.market();
    } catch (e) {
      clear(grid);
      appendAll(grid, h('div.empty', [
        h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message }),
      ]));
      return;
    }
    // 探测是"能不能打开"的依据，但它要跑十几条网络请求（含 8 秒超时）。
    // **不在打开页面时自动跑** —— 用户反馈"应用市场打开较慢，其它页面都是秒开"。
    // 改为：结果缓存在内存里；点「检测可用性」时才真跑；生成入口后刷新一次。
    if (!proxyState) {
      proxyState = { enabled: true, items: [], _stale: true };
    }
    renderHead();
    renderGrid();
  }

  function renderHead() {
    clear(headBox);
    const docker = cache?.docker || {};
    const installed = (cache?.list || []).filter((a) => a.installed).length;
    appendAll(headBox, 
      h('span.pill', { text: `共 ${(cache?.list || []).length} 个应用` }),
      installed > 0 ? h('span.pill.ok', { text: `已安装 ${installed}` }) : null,
      h('span.pill' + (docker.available ? '.ok' : '.warn'), {
        text: docker.available ? `Docker ${docker.version || '已就绪'}` : 'Docker 未安装',
        title: docker.available ? 'Docker socket: ' + docker.socket : 'Docker 类应用需要先安装 Docker（推荐 OrbStack）',
      }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      // 顶部只保留"一键 LNMP"：它是个**组合动作**（装三个包 + 四处收尾工作），
      // 在列表里没有对应的单个条目。其余项目都已进应用目录，
      // 各自卡片上的按钮就是入口 —— 顶部重复放一遍只会让人不知道该点哪。
      h('button.btn.btn-sm.btn-primary', {
        text: '⚡ 一键安装 LNMP 环境',
        title: '没装 Homebrew 时会先在面板里把 Homebrew 与命令行开发者工具装上（全程在面板内完成）',
        onclick: installLNMP,
      }),
      h('button.btn.btn-sm', { text: '⚙️ 服务管理', onclick: () => { location.hash = '#/services'; } }),
      // 子路径入口有两段：面板自己反代（自动生效）+ nginx 的 80 端口（要写配置）。
      // 这个按钮管第二段 —— 用户要的 `http://192.168.1.4/iopaint/` 就是它。
      proxyState && proxyState.enabled
        ? h('button.btn.btn-sm', {
          text: '🔍 检测可用性',
          title: '逐个探测应用界面能不能打开（要跑十几条请求，约 5-15 秒）',
          onclick: () => doProbe(),
        })
        : null,
      proxyState && proxyState.enabled
        ? h('button.btn.btn-sm', {
          text: '🔗 生成 nginx 入口',
          title: '把每个有界面的应用挂到 http://<主机>/<应用>/（写入 nginx 并重载）',
          onclick: applyProxies,
        })
        : null,
    );
  }

  // doProbe 手动触发一次可用性探测（打开页面时不再自动跑，见 load() 的说明）。
  async function doProbe() {
    try {
      toast('正在探测应用界面…', 'info', 3000);
      proxyState = await api.appProxies();
      renderGrid();
    } catch (e) {
      toast('探测失败：' + e.message, 'err', 8000);
    }
  }

  // applyProxies 生成/更新 nginx 里的子路径入口。
  // 秒级动作（写文件 + nginx -t + reload），后端同步返回，失败会原样带回 nginx 的报错。
  async function applyProxies() {
    try {
      const r = await api.appProxyApply();
      toast(`已写入 ${r.count} 个入口（${(r.slugs || []).join('、')}）并重载 nginx`, 'ok', 8000);
    } catch (e) {
      toast('生成失败：' + e.message, 'err', 12000);
      return;
    }
    // 等 nginx 把新配置真正切上去再探测：reload 是异步的（旧 worker 要收尾），
    // 立刻探测会拿到旧配置的结论，界面就会显示成"子路径不可用"。
    await new Promise((r) => setTimeout(r, 900));
    try { proxyState = await api.appProxies(); } catch { /* 忽略 */ }
    renderGrid();
  }

  // installLNMP 一键装 nginx + PHP + MySQL，并做四件包管理管不到的收尾工作。
  //
  // 这一步可能跑十几分钟，所以它**不再**是"同步请求 + 事后打印步骤"：
  // 后端立刻返回 task_id，进度由任务中心（tasks.js）用 SSE 实时展示。
  // 关掉进度窗、切页面都不会中断安装 —— 任务跑在 context.Background() 上。
  function installLNMP() {
    taskCenter.start({
      kind: 'install',
      target: 'lnmp',
      title: '一键安装 LNMP 环境',
      start: () => api.installLNMP(),
    });
  }

  // randomToken 生成与后端同格式的密钥：ttsv- + 32 位十六进制
  function randomToken() {
    const b = new Uint8Array(16);
    crypto.getRandomValues(b);
    return 'ttsv-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  }

  // installQwenTTS 让用户先选"要不要鉴权"和"密钥从哪来"，再开始装。
  //
  // 为什么把鉴权做成选项：mlx-audio 上游完全没有鉴权，对外暴露与否是
  // 一个真实取舍。默认勾选"加鉴权"，并且不加鉴权时明确写出后果 ——
  // 默认值要安全，危险选项要让用户看见代价。
  function installQwenTTS(appId) {
    const authOn = h('input', { type: 'checkbox', checked: true });
    const keyInput = h('input.input', { placeholder: '留空则自动生成', value: '' });
    const risk = h('div', {
      style: {
        display: 'none', marginTop: '8px', padding: '9px 11px',
        background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12px', lineHeight: '1.7',
      },
      text: '⚠️ 不加鉴权：Qwen 会监听 0.0.0.0 且没有任何鉴权，' +
            '同一内网里任何人都能白用这块 GPU 合成语音。只在你完全可控的网络里这样做。',
    });
    const keyRow = h('div.field', [
      h('label', { text: '共享密钥（网站插件里要填同一个值）' }),
      h('div', { style: { display: 'flex', gap: '8px' } }, [
        keyInput,
        h('button.btn.btn-sm', {
          text: '🎲 生成',
          onclick: () => { keyInput.value = randomToken(); },
        }),
      ]),
      h('div.hint', { text: '留空则自动生成一个随机密钥 —— 部署完成后会单独显示出来，请务必记录。' }),
    ]);
    authOn.addEventListener('change', () => {
      risk.style.display = authOn.checked ? 'none' : 'block';
      keyRow.style.display = authOn.checked ? '' : 'none';
    });

    modal({
      title: '部署 Qwen3 TTS',
      body: h('div', [
        h('div', { style: { fontSize: '12.5px', lineHeight: '1.8', marginBottom: '12px' } },
          '将安装：Python 3.11 虚拟环境 → mlx-audio[server] → 模型（约 2GB）→ ' +
          '注册为系统级后台服务（端口 8880）。需要 16GB 内存与 10GB 可用磁盘。'),
        h('div.field', [
          h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, [
            authOn, h('span', { text: '加鉴权（推荐）' }),
          ]),
          h('div.hint', {
            text: 'Qwen 只监听本机，对外由一个带共享密钥的反向代理（8899）提供 —— ' +
                  '网站插件通过 8899 + 密钥访问。取消勾选则 8880 直接对外且无鉴权。',
          }),
          risk,
        ]),
        keyRow,
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始部署',
          onclick: () => {
            const opts = { auth: authOn.checked, token: keyInput.value.trim() };
            close();
            toast(opts.auth ? '已选择加鉴权：装完会自动部署带密钥的反向代理入口。'
                            : '未加鉴权：8880 将直接对外。', opts.auth ? 'info' : 'warn', 9000);
            taskCenter.start({
              kind: 'install',
              target: appId || 'qwen3tts',
              title: '部署 Qwen3 TTS',
              start: () => api.installQwenTTS(opts),
            });
          },
        }),
      ],
    });
  }

  function installVoiceReceiver(appId) {
    const keyInput = h('input.input', { placeholder: '留空则自动生成', value: '' });
    const noAuth = h('input', { type: 'checkbox' });
    const risk = h('div', {
      style: {
        display: 'none', marginTop: '8px', padding: '9px 11px',
        background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12px', lineHeight: '1.7',
      },
      text: '⚠️ 留空且勾选"不鉴权"：任何人都能往这台机器投放文件，也能调用合成。仅限完全可控的内网。',
    });
    noAuth.addEventListener('change', () => { risk.style.display = noAuth.checked ? 'block' : 'none'; });

    modal({
      title: '部署音色接收端',
      body: h('div', [
        h('div', { style: { fontSize: '12.5px', lineHeight: '1.8', marginBottom: '12px' } },
          '部署音色样本接收端 + 带鉴权的反向代理（端口 8899）：接收上传的音色样本，' +
          '并把 /v1/* 转发给只监听本机的 8880。'),
        h('div.field', [
          h('label', { text: '共享密钥（网站插件里要填同一个值）' }),
          h('div', { style: { display: 'flex', gap: '8px' } }, [
            keyInput,
            h('button.btn.btn-sm', { text: '🎲 生成', onclick: () => { keyInput.value = randomToken(); } }),
          ]),
          h('div.hint', { text: '留空则自动生成；已部署过时会复用旧密钥（换掉会让网站失联）。' }),
        ]),
        h('div.field', [
          h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, [
            noAuth, h('span', { text: '不启用鉴权（密钥留空）' }),
          ]),
          risk,
        ]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始部署',
          onclick: () => {
            const opts = { token: keyInput.value.trim(), no_auth: noAuth.checked };
            close();
            taskCenter.start({
              kind: 'install',
              target: appId || 'voicereceiver',
              title: '部署音色接收端',
              start: () => api.installVoiceReceiver(opts),
            });
          },
        }),
      ],
    });
  }

  // installIOPaint / installPhpMyAdmin 都是"提交即返回"的异步任务，
  // 真正的进度与结果（IOPaint 的访问地址等）在任务中心的进度窗里。
  function installIOPaint(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'iopaint',
      title: '部署 IOPaint（图片去水印）',
      start: () => api.installIOPaint(),
    });
  }

  // adoptApp 把一个已安装但未登记的服务纳管进来。
  //
  // 这是"自动纳管的兜底"：面板启动时会自动登记目录里已知的服务，
  // 但如果服务是后装的、或标签不在目录里，就得靠这个按钮。
  function adoptApp(a) {
    const label = a.service_label || a.adopt_label;
    if (!label) { toast('这个应用没有可纳管的服务标签', 'warn'); return; }
    modal({
      title: `纳管「${a.name}」`,
      body: h('div', { style: { fontSize: '12.5px', lineHeight: '1.8' } }, [
        h('p', { text: `将把本机正在运行的 ${label} 登记到「服务管理」。` }),
        h('p', { style: { color: 'var(--text-mute)' },
          text: '面板只做启停与查看，不会卸载它、也不会改动它的启动方式。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '确认纳管',
          onclick: async () => {
            close();
            try {
              await api.adopt({ label, display_name: a.name, icon: a.icon, port: a.port, category: a.category });
              toast(`已纳管「${a.name}」`, 'ok');
              load();
            } catch (e) {
              toast(e.message, 'err', 12000);
            }
          },
        }),
      ],
    });
  }

  function installPhpMyAdmin(appId) {
    taskCenter.start({
      kind: 'install',
      target: appId || 'phpmyadmin',
      title: '部署 phpMyAdmin',
      start: () => api.installPhpMyAdmin(),
    });
  }

  function renderGrid() {
    clear(grid);
    const list = cache?.list || [];
    if (!list.length) {
      appendAll(grid, h('div.empty', [h('div.big', { text: '🧩' }), h('h4', { text: '应用目录为空' })]));
      return;
    }

    // 按分类分组展示
    const groups = [
      { key: 'site', label: '一键建站' },
      { key: 'ai', label: 'AI 服务' },
      { key: 'tool', label: '运维工具' },
      { key: 'other', label: '其它' },
    ];
    for (const g of groups) {
      const items = list.filter((a) => (a.category || 'other') === g.key);
      if (!items.length) continue;
      appendAll(grid, 
        h('div.section-title', { style: { marginTop: grid.childElementCount ? '20px' : '0' }, text: g.label }),
        h('div.grid.grid-3', items.map((a) => appCard(a))),
      );
    }
    // 兜底：没有分类的也显示出来
    const rest = list.filter((a) => !['site', 'ai', 'tool', 'other'].includes(a.category || 'other'));
    if (rest.length) {
      appendAll(grid, 
        h('div.section-title', { style: { marginTop: '20px' }, text: '其它' }),
        h('div.grid.grid-3', rest.map((a) => appCard(a))),
      );
    }
  }

  function appCard(a) {
    return h('div', {
      style: {
        background: 'var(--panel-2)', border: '1px solid var(--border)', borderRadius: 'var(--radius)',
        padding: '15px 16px', display: 'flex', flexDirection: 'column', gap: '10px',
      },
    }, [
      h('div', { style: { display: 'flex', gap: '10px', alignItems: 'flex-start' } }, [
        h('div', {
          style: {
            width: '38px', height: '38px', borderRadius: '10px', display: 'grid', placeItems: 'center',
            background: 'var(--panel)', fontSize: '20px', flex: '0 0 auto',
          },
          text: a.icon || '🧩',
        }),
        h('div', { style: { flex: 1, minWidth: 0 } }, [
          h('div', { style: { fontWeight: '620', fontSize: '14px' }, text: a.name }),
          h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '2px' }, text: a.summary }),
        ]),
      ]),
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        a.port > 0 ? h('span.pill', { text: ':' + a.port }) : null,
        a.kind === 'native' ? h('span.pill.brand', { text: '原生' }) : null,
        a.kind === 'compose' ? h('span.pill.brand', { text: 'Docker' }) : null,
        // 原生 brew 服务：记录在、但 plist 不在 = 服务其实没注册（ollama 就是这样，
        // 用户看到"已安装"却在服务管理里启动失败）。这一条要显式说出来。
        (a.kind === 'native' && a.service_label && a.installed && !a.service_in_launchd)
          ? h('span.pill.warn', {
            text: '已安装·服务未注册',
            title: '这个 brew 服务还没在 launchd 里注册（plist 不存在）。' +
              '到「服务管理」点一次启动即可自动注册（面板会用 brew services start 补上）',
          })
          : (a.adopted ? h('span.pill.ok', { text: '已纳管' })
          : (a.installed
            // 装了但服务没在 launchd 里（plist 丢了/没注册成功）是一种**孤儿态**：
            // 说"已安装·未纳管"会让人以为点一下纳管就行，而那个按钮必然报错。
            // 所以这里如实说"服务未注册"。
            //
            // 例外：no_daemon 的应用**本来就没有守护进程**（phpMyAdmin 是
            // nginx alias + php-fpm，装完就是一个网页入口）。对它报"服务未注册"
            // 是纯粹的误导 —— 用户反馈过这个。（2026-09-14）
            ? (a.no_daemon
              ? h('span.pill.ok', { text: '已安装·网页入口', title: '这个应用没有常驻进程，装完就是一个网页入口' })
              : h('span.pill' + (a.service_in_launchd ? '' : '.warn'), {
                text: a.service_in_launchd ? '已安装·未纳管' : '已安装·服务未注册',
                title: a.service_in_launchd ? '' : '安装产物还在，但 launchd 里找不到这个服务；用「重新部署」可修复',
              }))
            : null)),
        !a.available && !a.installed ? h('span.pill.warn', { text: a.note || '暂不可用' }) : null,
      ]),
      a.description ? h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.55' },
        text: a.description,
      }) : null,
      h('div', { style: { display: 'flex', gap: '6px', marginTop: 'auto', paddingTop: '4px', flexWrap: 'wrap' } }, [
        // 按钮状态：
        //   有任务在跑     → 查看进度（点回任务中心的进度窗，而不是再点一次安装）
        //   已纳管        → 查看服务
        //   已装但未纳管   → **纳管**（这是兜底入口，之前缺失，用户找不到已装的应用）
        //   未安装        → 安装
        taskCenter.findByTarget(a.id)
          ? h('button.btn.btn-sm.btn-primary', {
            text: '⟳ 查看进度',
            title: '这个应用有正在进行的任务，点开看实时进度',
            onclick: () => taskCenter.openTask(taskCenter.findByTarget(a.id).id),
          })
          : (a.adopted
            ? h('button.btn.btn-sm', { text: '查看服务', onclick: () => { location.hash = '#/services'; } })
            : (a.installed && a.service_label && a.service_in_launchd
              // 「纳管」只在**服务确实在 launchd 里**时才给 —— 否则点下去必然报
              // "找不到 xxx 的 plist，且该服务未在 launchd 中加载"，
              // 用户看到的就是一个点了没用的按钮（这正是用户反馈的问题之一）。
              ? h('button.btn.btn-sm.btn-primary', {
                text: '纳管',
                title: '把这个已在运行的服务登记到「服务管理」',
                onclick: () => adoptApp(a),
              })
              : (a.site_app
                ? null // 建站类的入口由 siteInstallButtons 提供
                : (a.panel_installer && a.artifacts && !a.service_in_launchd
                // 孤儿态：产物还在、服务没了 → 重新部署（安装器是幂等的，会重建 plist）
                ? h('button.btn.btn-sm.btn-primary', {
                  text: '重新部署',
                  title: '安装产物还在，但服务没在 launchd 里；重新部署会重建服务定义并登记到服务管理',
                  onclick: () => openInstaller(a),
                })
                : h('button.btn.btn-sm.btn-primary', {
                  text: '安装',
                  disabled: !a.available,
                  onclick: () => openInstaller(a),
                }))))),
        ...openButtons(a),
        ...siteInstallButtons(a),
        ...uninstallButtons(a),
        a.docs_url ? h('a.btn.btn-sm', { href: a.docs_url, target: '_blank', rel: 'noopener', text: '文档' }) : null,
      ]),
    ]);
  }

  // openButtons 给"有界面且已装"的应用两个入口：子路径与直连端口。
  //
  // 为什么主按钮可能指向直连：有些应用必须自己设 base path 才能挂子路径
  // （n8n / Gitea / MinIO / Stirling 都是），硬挂会白屏。
  // 探测（/api/v1/market/proxies）说不行，就把直连作为首选入口，
  // 并在 title 里说清原因 —— 而不是给一个点开是白屏的按钮。
  function openButtons(a) {
    if (!a.ui || !a.ui.slug || !a.installed) return [];
    const st = (proxyState?.items || []).find((x) => x.slug === a.ui.slug) || {};
    const path = '/' + a.ui.slug + '/';
    const direct = a.port_url || '';
    // SelfConf（phpMyAdmin）：它的 location 由安装器直接写进 nginx，
    // 面板不反代，所以只能用 nginx 的**绝对地址** —— 用相对路径会打到
    // 面板自己的 SPA 回落上（返回 200 却是面板首页，极具误导性）。
    if (a.ui.self_conf) {
      // SelfConf 的应用（phpMyAdmin）：它的 nginx location 只允许本机，
      // 所以唯一能用的入口是**面板自己**那条（要求先登录面板）。
      // 用户反馈过这里给出的是局域网地址 http://192.168.1.4/phpmyadmin/ → 403。
      return [h('a.btn.btn-sm.btn-primary', {
        href: panelPath('phpmyadmin/'), target: '_blank', rel: 'noopener', text: '打开',
        title: '经面板打开（需先登录面板；面板会反代到本机的 phpMyAdmin）',
      })];
    }
    // prefer_direct 是**人工实测**的结论（自动探测发现不了"资源全 200 但
    // 前端路由不认这个前缀"的情况），所以它的优先级高于探测结果。
    // 尚未探测过（_stale）时按"能用"对待 —— 否则没点过检测的用户会看到所有应用
    // 都被降级成"直连端口"（这不是我们想给的默认结论）。
    const probed = !proxyState?._stale;
    const proxyOK = !a.ui.prefer_direct && (probed ? !!st.proxy_ok : true);
    const why = a.ui.prefer_direct ? (a.ui.note || '这个应用不支持子路径') : (st.reason || a.ui.note || '');
    const out = [];
    if (proxyOK) {
      out.push(h('a.btn.btn-sm.btn-primary', {
        href: path, target: '_blank', rel: 'noopener', text: '打开',
        title: '经面板的 /' + a.ui.slug + '/ 打开（所有入口都通：80 端口、面板端口、隧道）',
      }));
      if (direct) {
        out.push(h('a.btn.btn-sm', { href: direct, target: '_blank', rel: 'noopener', text: '直连端口', title: '绕过面板直接访问：' + direct }));
      }
    } else if (direct) {
      // 子路径不可用（应用需要自己设 base path，或前端路由不认这个前缀）。
      //
      // 既然"子路径不可用"是**人工实测/探测**得出的结论（PreferDirect 或探测失败），
      // 主入口就必须是**端口直连** —— 否则用户点「打开」拿到的是一个已知打不开的
      // 子路径，与 AppUI.PreferDirect 的字段文档（"「打开」直接给端口直连，
      // 子路径降级成次要入口"）自相矛盾。子路径保留成"试试"按钮，
      // 万一以后上游支持了或探测结论变了，仍有一条入口。
      out.push(h('a.btn.btn-sm.btn-primary', {
        href: direct, target: '_blank', rel: 'noopener', text: '打开',
        title: '直连应用端口：' + direct + (why ? '（' + why + '）' : ''),
      }));
      out.push(h('a.btn.btn-sm', {
        href: path, target: '_blank', rel: 'noopener', text: '试试子路径',
        title: why || '子路径可能不可用',
      }));
    } else {
      out.push(h('a.btn.btn-sm', {
        href: path, target: '_blank', rel: 'noopener', text: '打开',
        title: why || '应用可能没有启动',
      }));
    }
    return out;
  }

  // uninstallButtons 给"面板装的"应用一个卸载入口。
  //
  // 为什么必须分三类（见 services.UninstallPlan）：
  //   · service   —— 托管服务（compose 应用等），可以真卸载；
  //   · installer —— 面板自研安装器装的（IOPaint / Qwen / 接收端 / phpMyAdmin /
  //                  Docker 运行时），走安装器自己的卸载；
  //   · forget    —— **纳管的第三方服务**（nginx / php / mysql / 用户自己注册的）：
  //                  面板绝不卸载它们（删掉用户自己的 MySQL 等于删掉他的数据），
  //                  只给「取消纳管」，并在确认框里说清楚"只移除记录，不动系统"。
  // 没有这三类之一的（brew 核心组件、未安装）就不给按钮。
  // siteInstallButtons 给「一键建站」类应用一个建站入口。
  //
  // 这类应用装出来是一个**网站**（目录 + 数据库 + 伪静态 + vhost），
  // 不是服务也不是容器，所以按钮文案与流程都不同：先弹一个表单问域名与管理员，
  // 再由后端一气做完（见 api_site_apps.go）。
  function siteInstallButtons(a) {
    if (!a.site_app) return [];
    return [h('button.btn.btn-sm.btn-primary', {
      text: '一键建站',
      disabled: !a.available,
      title: '自动下载源码、建库、建站点并套用伪静态',
      onclick: () => openSiteInstall(a),
    })];
  }

  async function openSiteInstall(a) {
    const domain = h('input.input', { placeholder: '例如：blog.test', value: '' });
    const php = h('input.input', { value: '8.3' });
    const note = h('div.hint', {
      text: '面板会自动：下载官方源码 → 解压到 ~/www/<域名> → 建库建用户 → 写配置文件 → ' +
        '建站点并套用「' + (a.site_app.rewrite || '') + '」伪静态。',
    });
    const m = modal({
      title: '一键建站 · ' + a.name,
      body: h('div', [
        h('div.field', [h('label', { text: '域名' }), domain,
          h('div.hint', { text: '先用一个测试域名（如 blog.test）即可；域名创建后不可修改' })]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php]),
        note,
        (a.site_app.notes || []).length
          ? h('ul', { style: { margin: '8px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
            (a.site_app.notes || []).map((n) => h('li', { text: n })))
          : null,
        a.site_app.finish_path
          ? h('div.hint', { style: { marginTop: '8px' }, text: '装完请打开 http://<域名>' + a.site_app.finish_path + ' 走完最后一步。' })
          : null,
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '开始建站',
          onclick: async () => {
            const d = domain.value.trim();
            if (!d) { toast('请填域名', 'warn'); return; }
            close();
            taskCenter.start({
              kind: 'site-install',
              target: d,
              title: '一键建站 ' + a.name + '（' + d + '）',
              start: () => api.marketInstallSite(a.id, { domain: d, php: php.value.trim() || '8.3' }),
              onDone: () => load(),
            });
          },
        }),
      ],
    });
    setTimeout(() => domain.focus(), 60);
    return m;
  }

  function uninstallButtons(a) {
    const plan = a.uninstall || {};
    if (!a.installed) return [];
    if (plan.kind === 'service' || plan.kind === 'installer') {
      return [h('button.btn.btn-sm.btn-danger', {
        text: '卸载',
        title: plan.blocked || '卸载「' + a.name + '」（会列出具体删除内容并要求确认）',
        disabled: !!plan.blocked,
        onclick: () => doUninstall(a, plan),
      })];
    }
    if (plan.kind === 'forget') {
      return [h('button.btn.btn-sm', {
        text: '取消纳管',
        title: '只把这个服务从面板记录里移除，不动系统上的任何东西',
        onclick: () => doForget(a, plan),
      })];
    }
    return [];
  }

  // doUninstall 先弹一个"会做什么"的确认框，再交给任务中心。
  //
  // 确认框里逐条列出步骤与可选删除的路径 —— 卸载不可逆，
  // 一句"确定卸载吗"是不够的（用户有权知道模型/样本/任务会不会一起没）。
  async function doUninstall(a, plan) {
    const remove = h('input', { type: 'checkbox' });
    const lines = (plan.steps || []).map((s) => h('li', { text: s }));
    const body = h('div', [
      h('div', { style: { marginBottom: '8px' }, text: '将执行：' }),
      h('ul', { style: { margin: '0 0 10px 18px', lineHeight: '1.7' } }, lines),
      plan.keep_note ? h('div.hint', { text: '会保留：' + plan.keep_note }) : null,
      (plan.data_paths || []).length
        ? h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '10px' } }, [
          remove,
          h('span', { text: '同时删除数据/产物（不可恢复）：' }),
        ])
        : null,
      (plan.data_paths || []).length
        ? h('ul', { style: { margin: '6px 0 0 18px', lineHeight: '1.7', fontSize: '12px' } },
          (plan.data_paths || []).map((p) => h('li.mono', { text: p })))
        : null,
    ]);
    const okGo = await new Promise((resolve) => {
      const m = modal({
        title: '卸载 ' + a.name,
        body,
        footer: (close) => [
          h('button.btn', { text: '取消', onclick: () => { close(); resolve(false); } }),
          h('button.btn.btn-danger', {
            text: '确认卸载',
            onclick: () => { close(); resolve(true); },
          }),
        ],
        onClose: () => resolve(false),
      });
    });
    if (!okGo) return;
    taskCenter.start({
      kind: 'uninstall',
      target: a.id,
      title: '卸载 ' + a.name,
      start: () => api.marketUninstall(a.id, remove.checked),
      onDone: () => load(),
    });
  }

  // doForget 取消纳管：只删面板记录，不动系统。
  async function doForget(a, plan) {
    const okGo = await confirmBox(
      '把「' + a.name + '」从面板记录里移除？\n\n' +
      '面板不会卸载你自己安装的软件（不跑 brew uninstall、不删文件），' +
      '只是不再管它。要真正删除请在终端里自行处理。',
      { title: '取消纳管', okText: '取消纳管' });
    if (!okGo) return;
    const name = plan.service || a.id;
    try {
      await api.serviceForget(name);
      toast('已取消纳管', 'ok');
      load();
    } catch (e) {
      toast('取消失败：' + e.message, 'err', 9000);
    }
  }

  // openInstaller 按应用打开对应的部署对话框。
  //
  // 抽出来是因为有**两个入口**要用它：
  //   · 「安装」——没装过的应用；
  //   · 「重新部署」——装了但服务没注册的孤儿态（plist 丢了等）。
  // 安装器本身是幂等的：重跑会重建 venv/服务定义/plist 并登记到服务管理，
  // 所以"重新部署"就是最合理的修复动作，不需要另写一套修复逻辑。
  function openInstaller(a) {
    // 用面板自研安装器的项目要收集选项（例如 Qwen 的"要不要鉴权"、
    // 密钥从哪来），所以直接打开对应对话框，而不是走通用安装流程。
    // 传 a.id 作为任务 target：市场卡片的「查看进度」就是按这个值找运行中的任务的。
    switch (a.panel_installer) {
      case 'qwentts': installQwenTTS(a.id); return;
      case 'voicereceiver': installVoiceReceiver(a.id); return;
      case 'iopaint': installIOPaint(a.id); return;
      case 'phpmyadmin': installPhpMyAdmin(a.id); return;
    }
    preflight(a);
  }

  // ---------- 安装前检查 ----------
  async function preflight(a) {
    const box = h('div', [h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: '正在检查安装条件…' })])]);
    const footer = h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', width: '100%' } });

    let pf = null;
    const m = modal({
      title: `安装检查：${a.name}`,
      wide: true,
      body: box,
      footer: () => [footer],
    });

    try {
      pf = await api.marketPreflight(a.id);
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
      return;
    }

    clear(box);
    appendAll(box, 
      h('div', { style: { marginBottom: '14px' } }, [
        pf.ready
          ? h('span.pill.ok', { text: '✅ 条件满足，可以安装' })
          : h('span.pill.danger', { text: '⚠️ 有未满足的条件' }),
      ]),
      // 端口
      h('div', {
        style: {
          padding: '9px 11px', marginBottom: '8px', borderRadius: '6px', fontSize: '12.5px',
          background: pf.port_free ? 'var(--ok-soft)' : 'var(--danger-soft)',
        },
      }, [
        h('div', { text: (pf.port_free ? '✓ ' : '✗ ') + (pf.port_note || '端口检查未执行') }),
      ]),
      // 依赖
      ...(pf.checks || []).map((c) => h('div', {
        style: {
          padding: '9px 11px', marginBottom: '8px', borderRadius: '6px', fontSize: '12.5px',
          background: c.ok ? 'var(--ok-soft)' : 'var(--warn-soft)',
        },
      }, [
        h('div', { text: (c.ok ? '✓ ' : '! ') + c.name + '：' + c.detail }),
        !c.ok && c.fix_cmd ? h('div', {
          style: { marginTop: '5px', fontFamily: 'var(--mono)', fontSize: '11.5px', color: 'var(--text-dim)' },
          text: '$ ' + c.fix_cmd,
        }) : null,
      ])),
      a.post_install_hint ? h('div.hint', { style: { marginTop: '12px' }, text: '安装后：' + a.post_install_hint }) : null,
      a.manual_hint ? h('div.hint', { style: { marginTop: '8px' }, text: a.manual_hint }) : null,
    );

    clear(footer);
    appendAll(footer, 
      h('button.btn', { text: '关闭', onclick: () => m.close() }),
      pf.ready || a.adopt_label
        ? h('button.btn.btn-primary', {
          text: a.adopt_label ? '接入管理' : '开始安装',
          onclick: () => { m.close(); doInstall(a); },
        })
        : h('button.btn.btn-primary', {
          text: '仍然尝试安装',
          title: '条件不满足时安装可能失败，但你可以继续',
          onclick: () => { m.close(); doInstall(a); },
        }),
    );
  }

  // ---------- 执行安装 ----------
  //
  // 旧实现是"同步等请求 + 事后打印 steps"，用户在整个过程中看不到任何真实输出，
  // 关掉窗口也找不回来。现在改成：POST 立刻返回 task_id，进度交给任务中心。
  // 失败（4xx/5xx）由 taskCenter.start 统一 toast，这里不需要再兜一层。
  function doInstall(a) {
    taskCenter.start({
      kind: 'install',
      target: a.id,
      title: `安装 ${a.name}`,
      start: () => api.marketInstall(a.id),
    });
  }

  // 任务状态变化（开始/结束）时重画卡片：正在安装的应用，按钮要变成「查看进度」。
  // 只订阅元信息变化，**不订阅日志行** —— 否则 brew 每输出一行都会重建整个网格。
  // 这是页面级订阅，跟着页面一起清理：任务状态本身活在 tasks.js 的单例里，
  // 清理掉的只是"这个页面要不要重画"。
  registerCleanup(taskCenter.onChange((kind) => {
    if (kind === 'lines' || !cache) return;
    renderGrid();
  }));
  load();
}
