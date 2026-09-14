// apps.js —— 应用市场页面。
//
// 核心思路：把"能不能装"和"装什么"分开。
//   - 用户点安装时，先跑一次安装前检查（依赖、端口），把问题一次说清
//   - 检查通过才真正安装，并在弹窗里逐步展示安装进度
//   - 已经装过的应用显示"已安装"，可以一键跳到服务管理页

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';

let cache = null;

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
      h('button.btn.btn-sm.btn-primary', { text: '⚡ 一键安装 LNMP 环境', onclick: installLNMP }),
      h('button.btn.btn-sm', { text: '⚙️ 服务管理', onclick: () => { location.hash = '#/services'; } }),
    );
  }

  // installLNMP 一键装 nginx + PHP + MySQL，并做四件包管理管不到的收尾工作。
  //
  // 这一步可能跑十几分钟，且**不会**重启面板，所以用模态框同步展示步骤流，
  // 让用户能一直看到"现在到哪了"。完成后提示他回到这里刷新（届时
  // 这三个应用的按钮会变成「已安装」—— 因为市场现在会查 brew 了）。
  async function installLNMP() {
    const steps = h('div', { style: { maxHeight: '320px', overflow: 'auto', fontSize: '12.5px', lineHeight: '1.8' } });
    const tip = h('div', { style: { color: 'var(--text-mute)', fontSize: '11.5px', marginBottom: '8px' },
      text: '国内镜像下通常 10-40 分钟。中途请不要关闭页面 —— 面板不会重启，但关闭后就看进度了。' });
    let closed = false;
    modal({
      title: '一键安装 LNMP 环境',
      body: h('div', [
        tip,
        h('div', { text: '将安装：nginx（改 listen 80）、PHP 8.3 FPM、MySQL 8.4（初始化数据目录），' +
                       '并注册为系统级后台服务（开机自启、不依赖登录）。',
          style: { fontSize: '12.5px', marginBottom: '10px' } }),
        steps,
      ]),
      footer: (close) => [h('button.btn', { text: '后台继续（关闭窗口）', onclick: () => { closed = true; close(); } })],
      onClose: () => { closed = true; },
    });
    const push = (t) => {
      if (closed) return;
      appendAll(steps, h('div', { text: '· ' + t }));
      steps.parentElement && (steps.parentElement.scrollTop = steps.parentElement.scrollHeight);
    };
    push('正在提交安装任务…（brew 安装阶段没有逐步输出，请耐心等待）');
    try {
      const res = await api.installLNMP();
      (res.steps || []).forEach((t) => steps.append(h('div', { text: '· ' + t })));
      if (res.ok === false) {
        appendAll(steps, h('div', { style: { color: 'var(--danger)', marginTop: '8px' }, text: '失败：' + res.error }));
        toast('LNMP 安装失败，详情见窗口', 'err', 12000);
        return;
      }
      appendAll(steps, h('div', { style: { color: 'var(--ok)', marginTop: '8px' }, text: '✅ 安装完成' }));
      if (res.warning) appendAll(steps, h('div', { style: { color: 'var(--warn)' }, text: '⚠️ ' + res.warning }));
      toast('LNMP 环境已就绪', 'ok', 8000);
      load();
    } catch (e) {
      appendAll(steps, h('div', { style: { color: 'var(--danger)', marginTop: '8px' }, text: '失败：' + e.message }));
      toast(e.message, 'err', 12000);
    }
  }

  // runTask 是"一键装某个组合"的通用展示器：
  // 同步接口 + 步骤流。这类任务动辄十几分钟，必须让用户看到"现在到哪了"。
  async function runTask(title, intro, fn) {
    const steps = h('div', { style: { maxHeight: '320px', overflow: 'auto', fontSize: '12.5px', lineHeight: '1.8' } });
    let closed = false;
    modal({
      title,
      body: h('div', [
        h('div', { style: { color: 'var(--text-mute)', fontSize: '11.5px', marginBottom: '8px' }, text: intro }),
        steps,
      ]),
      footer: (close) => [h('button.btn', { text: '后台继续（关闭窗口）', onclick: () => { closed = true; close(); } })],
      onClose: () => { closed = true; },
    });
    const push = (t) => { if (!closed) { appendAll(steps, h('div', { text: '· ' + t })); } };
    push('已提交，正在执行…');
    try {
      const res = await fn();
      (res.steps || []).forEach((t) => push(t));
      if (res.ok === false) {
        appendAll(steps, h('div', { style: { color: 'var(--danger)', marginTop: '8px' }, text: '失败：' + res.error }));
        toast(title + '失败，详情见窗口', 'err', 12000);
        return;
      }
      // 需要用户**记下来**的信息单独做成可复制区块。
      // 埋在步骤日志里会被忽略，而密钥丢了就得重新部署。
      if (res.token) {
        appendAll(steps, h('div', {
          style: {
            marginTop: '12px', padding: '12px', background: 'var(--warn-soft)',
            borderRadius: '6px', border: '1px solid var(--border)',
          },
        }, [
          h('div', { style: { fontWeight: '600', marginBottom: '6px' }, text: '⚠️ 请记录共享密钥（只在这里显示）' }),
          h('code', { style: { fontSize: '13px', userSelect: 'all', wordBreak: 'break-all' }, text: res.token }),
          h('div', { style: { marginTop: '8px' } }, [
            h('button.btn.btn-sm', {
              text: '📋 复制密钥',
              onclick: () => {
                navigator.clipboard?.writeText(res.token)
                  .then(() => toast('密钥已复制', 'ok'))
                  .catch(() => toast('复制失败，请手动选中复制', 'warn'));
              },
            }),
          ]),
        ]));
      }
      appendAll(steps, h('div', { style: { color: 'var(--ok)', marginTop: '8px' }, text: '✅ 完成' }));
      if (res.warning) appendAll(steps, h('div', { style: { color: 'var(--warn)' }, text: '⚠️ ' + res.warning }));
      toast(title + ' 已完成', 'ok', 8000);
      load();
    } catch (e) {
      appendAll(steps, h('div', { style: { color: 'var(--danger)', marginTop: '8px' }, text: '失败：' + e.message }));
      toast(e.message, 'err', 12000);
    }
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
  function installQwenTTS() {
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
            runTask('部署 Qwen3 TTS',
              opts.auth ? '已选择加鉴权：装完会自动部署带密钥的反向代理入口。'
                        : '未加鉴权：8880 将直接对外。',
              () => api.installQwenTTS(opts));
          },
        }),
      ],
    });
  }

  function installVoiceReceiver() {
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
            runTask('部署音色接收端', '正在部署…', () => api.installVoiceReceiver(opts));
          },
        }),
      ],
    });
  }

  function installIOPaint() {
    runTask('部署 IOPaint（图片去水印）',
      '安装 IOPaint：上传图片 → 涂抹水印/杂物 → 擦除，支持批量与视频。' +
      '使用 LaMa 模型并启用 Apple Silicon MPS 加速。' +
      '会拉取 torch（约 1~2GB），通常需要十几分钟。',
      () => api.installIOPaint());
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

  function installPhpMyAdmin() {
    runTask('部署 phpMyAdmin',
      '安装数据库管理界面并接入 nginx 默认站点，装完访问 http://<本机地址>/phpmyadmin/。',
      () => api.installPhpMyAdmin());
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
    const rest = list.filter((a) => !['ai', 'tool', 'other'].includes(a.category || 'other'));
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
        a.adopted ? h('span.pill.ok', { text: '已纳管' })
          : (a.installed
            // 装了但服务没在 launchd 里（plist 丢了/没注册成功）是一种**孤儿态**：
            // 说"已安装·未纳管"会让人以为点一下纳管就行，而那个按钮必然报错。
            // 所以这里如实说"服务未注册"。
            ? h('span.pill' + (a.service_in_launchd ? '' : '.warn'), {
              text: a.service_in_launchd ? '已安装·未纳管' : '已安装·服务未注册',
              title: a.service_in_launchd ? '' : '安装产物还在，但 launchd 里找不到这个服务；用「重新部署」可修复',
            })
            : null),
        !a.available && !a.installed ? h('span.pill.warn', { text: a.note || '暂不可用' }) : null,
      ]),
      a.description ? h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.55' },
        text: a.description,
      }) : null,
      h('div', { style: { display: 'flex', gap: '6px', marginTop: 'auto', paddingTop: '4px', flexWrap: 'wrap' } }, [
        // 三种状态的按钮各不相同：
        //   已纳管        → 查看服务
        //   已装但未纳管   → **纳管**（这是兜底入口，之前缺失，用户找不到已装的应用）
        //   未安装        → 安装
        a.adopted
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
              }))),
        a.docs_url ? h('a.btn.btn-sm', { href: a.docs_url, target: '_blank', rel: 'noopener', text: '文档' }) : null,
      ]),
    ]);
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
    switch (a.panel_installer) {
      case 'qwentts': installQwenTTS(); return;
      case 'voicereceiver': installVoiceReceiver(); return;
      case 'iopaint': installIOPaint(); return;
      case 'phpmyadmin': installPhpMyAdmin(); return;
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
  async function doInstall(a) {
    const stepList = h('div', { style: { fontFamily: 'var(--mono)', fontSize: '12px', lineHeight: '1.8' } });
    const box = h('div', [
      h('div', { style: { marginBottom: '12px' } }, [
        h('span.pill.brand', { text: '安装中，请勿关闭页面' }),
      ]),
      stepList,
      h('div.hint', { style: { marginTop: '12px' }, text: '首次安装需要下载依赖或镜像，可能需要几分钟。' }),
    ]);
    const m = modal({ title: `正在安装：${a.name}`, wide: true, body: box });

    const log = (text) => {
      appendAll(stepList, h('div', { text: '▸ ' + text }));
    };
    log('开始安装…');

    try {
      const res = await api.marketInstall(a.id);
      m.close();
      toast(res.message || '安装完成', 'ok', 12000);
      if (res.warning) toast(res.warning, 'warn', 15000);
      // 装完直接跳到服务管理，用户能立刻确认状态
      location.hash = '#/services';
    } catch (e) {
      m.close();
      modal({
        title: `安装失败：${a.name}`,
        wide: true,
        body: h('div', [
          h('div', { style: { padding: '11px', background: 'var(--danger-soft)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.7' } }, [
            h('div', { text: e.message }),
          ]),
          h('div.hint', { style: { marginTop: '12px' }, text: '如果是依赖缺失，先按提示安装依赖再重试；如果是端口冲突，可以换一个端口后重试。' }),
        ]),
      });
    }
  }

  registerCleanup(() => { });
  load();
}
