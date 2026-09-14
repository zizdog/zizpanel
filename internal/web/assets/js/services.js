// services.js —— 服务管理页面。
//
// 交互设计要点：
//   - 卡片视图按分类分组，一眼看清"哪些在跑、哪些挂了"
//   - 状态灯的颜色直接反映真实状态（面板每次都实时查询系统）
//   - 纳管服务与托管服务在界面上有明确区分：前者不能卸载
//   - 日志用 SSE 实时推送，不靠前端轮询
//   - 应用市场先做"安装前检查"，把缺依赖/端口冲突一次说清

import { api, sseServiceLogs } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';
import { taskCenter } from './tasks.js';

// 页面上缓存的列表数据
let cache = null;
let filter = 'all';

export function ServicesView(content, ctx = {}) {
  clear(content);

  const cards = h('div');
  const toolbar = h('div.card-head', [
    h('h3', { text: '服务' }),
    h('div.spacer'),
    h('div', { id: 'svc-toolbar', style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);

  appendAll(content, 
    h('div.card', [toolbar, h('div.card-body', [cards])]),
  );

  const toolbarBox = toolbar.querySelector('#svc-toolbar');

  function renderToolbar() {
    clear(toolbarBox);
    const list = (cache?.list || []);
    const running = list.filter((s) => s.state?.running).length;
    const unhealthy = list.filter((s) => s.health?.checked && !s.health.ok).length;

    const filters = [
      { id: 'all', label: '全部' },
      { id: 'running', label: '运行中' },
      { id: 'stopped', label: '已停止' },
      { id: 'problem', label: '异常' },
      { id: 'unhealthy', label: '仅健康检查失败' },
    ];
    appendAll(toolbarBox, 
      h('span.pill' + (running > 0 ? '.ok' : ''), { text: `${running}/${list.length} 运行中` }),
      // 做成可点的：用户看到"有失败"的第一反应是"哪些？怎么办？"，
      // 所以点它直接筛出失败的服务（卡片里还有具体原因与下一步）。
      unhealthy > 0 ? h('button.btn.btn-sm.btn-danger', {
        text: `⚠ ${unhealthy} 个健康检查失败`,
        title: '点这里只看失败的服务',
        onclick: () => { filter = 'unhealthy'; renderToolbar(); renderCards(); },
      }) : null,
      h('div', { style: { display: 'flex', gap: '4px' } }, filters.map((f) =>
        h(`button.btn.btn-sm${filter === f.id ? '.btn-primary' : ''}`, {
          text: f.label,
          onclick: () => { filter = f.id; renderToolbar(); renderCards(); },
        }))),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      h('button.btn.btn-sm', {
        text: '🔍 扫描可纳管服务',
        title: '找出本机上已经在运行的 launchd 服务，接入面板管理',
        onclick: scanAdoptable,
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 注册服务', onclick: () => newServiceModal(load) }),
    );
  }

  async function load() {
    clear(cards);
    appendAll(cards, h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在查询服务状态…' })]));
    try {
      cache = await api.services(true);
    } catch (e) {
      clear(cards);
      appendAll(cards, h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取服务失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }
    renderToolbar();
    renderCards();
  }

  function stateOf(s) {
    const st = s.state || {};
    if (st.running) return { cls: 'ok', text: '运行中' };
    if (st.status === 'error') return { cls: 'danger', text: '异常' };
    if (st.status === 'unavailable') return { cls: 'warn', text: '环境不可用' };
    if (st.status === 'not-installed') return { cls: 'warn', text: '未安装' };
    if (st.status === 'unknown') return { cls: '', text: '未知' };
    return { cls: '', text: '已停止' };
  }

  /**
   * healthHint 把"健康检查失败"翻译成"大概是什么问题、该怎么办"。
   *
   * 用户的原话是：看到"1 个健康检查失败"这个提示，不知道接下来该做什么。
   * 光给一个红标签等于把排查工作全丢给用户。这里按**失败的种类**给出最可能的
   * 原因与下一步动作。
   *
   * 注意 401/403 这条分支现在很少走到：后端已经把"需要身份验证"判为**健康**
   * （Stirling PDF 这类应用装完要设自己的账号密码，401 说明它活得好好的）。
   * 只有当用户又填了「期望包含内容」时，才会因为内容对不上而失败 ——
   * 所以这里的话术是"校验方式选错了"，而不是"服务有问题"。
   */
  function healthHint(health) {
    const code = Number(health.code || 0);
    const msg = String(health.message || '');
    if (code === 401 || code === 403) {
      return '该地址要求身份验证，服务本身是正常的（所以默认按状态码判断时它算健康）。'
        + '但登录页里不会有你填的「期望包含内容」，建议把它清空，'
        + '或把「健康检查地址」换成不需要登录的路径（常见：/healthz、/api/health、/ping）。';
    }
    if (code === 404) {
      return '地址存在但路径不对（404）。确认健康检查地址写的是这个服务真实提供的路径。';
    }
    if (code >= 500) {
      return '服务自己返回了错误（5xx）。先看它的日志，通常是配置或依赖问题。';
    }
    if (/timeout|超时/i.test(msg)) {
      return '响应超时：服务可能很忙或卡住了。看日志确认它是否在正常处理请求；'
        + '如果是大应用，也可以把检查地址换成更轻量的路径。';
    }
    if (/refused|拒绝|无法连接|connect/i.test(msg)) {
      return '连不上：服务没在监听这个地址。确认它已启动、端口写对了，'
        + '以及它监听的是 127.0.0.1 还是 0.0.0.0（面板与服务的网络位置不同会影响）。';
    }
    if (/期望|包含|expect/i.test(msg)) {
      return '响应里没有「期望包含内容」。可能是被重定向到了登录页，或该路径返回的是别的页面；'
        + '建议把期望内容清空，只校验状态码。';
    }
    return '检查未通过。可以先看服务日志；若服务其实正常，多半是检查地址或期望内容选得不合适。';
  }

  function matchesFilter(s) {
    if (filter === 'all') return true;
    const st = s.state || {};
    if (filter === 'running') return !!st.running;
    if (filter === 'stopped') return !st.running && st.status !== 'error' && st.status !== 'unavailable';
    if (filter === 'problem') {
      return st.status === 'error' || st.status === 'unavailable' ||
        (s.health && s.health.checked && !s.health.ok);
    }
    if (filter === 'unhealthy') {
      return !!(s.health && s.health.checked && !s.health.ok);
    }
    return true;
  }

  function renderCards() {
    clear(cards);
    const list = (cache?.list || []).filter(matchesFilter);
    if (!list.length) {
      appendAll(cards, h('div.empty', [
        h('div.big', { text: '⚙️' }),
        h('h4', { text: (cache?.list || []).length ? '没有符合筛选条件的服务' : '还没有纳管任何服务' }),
        h('p', { text: '可以从「应用市场」安装新服务，或扫描本机已在运行的服务接入管理。' }),
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center' } }, [
          h('button.btn.btn-primary', { text: '打开应用市场', onclick: () => { location.hash = '#/apps'; } }),
          h('button.btn', { text: '扫描可纳管服务', onclick: scanAdoptable }),
        ]),
      ]));
      return;
    }

    const grid = h('div.grid.grid-3', list.map((s) => serviceCard(s)));
    appendAll(cards, grid);
  }

  function serviceCard(s) {
    const st = stateOf(s);
    const state = s.state || {};
    const health = s.health || {};

    const healthPill = (() => {
      if (!health.checked) return null;
      if (health.ok) {
        return h('span.pill.ok', { text: '健康', title: health.message + `（${health.latency_ms}ms）` });
      }
      return h('span.pill.danger', { text: '健康检查失败', title: health.message });
    })();

    const canControl = s.driver_ready !== false;

    return h('div', {
      style: {
        background: 'var(--panel-2)', border: '1px solid var(--border)',
        borderRadius: 'var(--radius)', padding: '15px 16px', display: 'flex', flexDirection: 'column', gap: '10px',
      },
    }, [
      // 标题行
      h('div', { style: { display: 'flex', alignItems: 'flex-start', gap: '10px' } }, [
        h('div', {
          style: {
            width: '38px', height: '38px', borderRadius: '10px', display: 'grid', placeItems: 'center',
            background: 'var(--panel)', fontSize: '20px', flex: '0 0 auto',
          },
          text: s.icon || '⚙️',
        }),
        h('div', { style: { flex: 1, minWidth: 0 } }, [
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '7px' } }, [
            h('span.dot' + (st.cls ? '.' + st.cls : ''), { title: st.text }),
            h('span', { style: { fontWeight: '620', fontSize: '14px' }, text: s.display_name || s.name }),
          ]),
          h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', marginTop: '2px' }, text: s.description || s.name }),
        ]),
      ]),

      // 状态标签行
      h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        h('span.pill' + (st.cls ? '.' + st.cls : ''), { text: st.text, title: state.detail || '' }),
        s.port > 0 ? h('span.pill', { text: ':' + s.port }) : null,
        h('span.pill' + (s.managed ? '.brand' : ''), {
          text: s.managed ? '面板托管' : '仅纳管',
          title: s.managed ? '面板负责完整生命周期，可卸载' : '由你自己安装，面板只做启停与查看，不会卸载',
        }),
        healthPill,
        s.driver_error ? h('span.pill.warn', { text: '驱动不可用', title: s.driver_error }) : null,
      ]),

      // 需要身份验证（401/403）时，卡片上给一行**说明**：
      // 后端已经把它判为健康（Stirling PDF 这类应用装完就是要设账号密码，
      // 401 说明它活得好好的），但用户会疑惑"明明要登录，怎么显示健康"，
      // 所以把原因摆出来，而不是只藏在标签的 title 里。
      (health.checked && health.ok && (health.code === 401 || health.code === 403))
        ? h('div', {
          style: { fontSize: '11.5px', color: 'var(--text-mute)' },
          text: '健康检查：' + health.message + '（' + (health.latency_ms || 0) + 'ms）',
        }) : null,

      // 健康检查失败：把"是什么、为什么、怎么办"都摆出来
      (health.checked && !health.ok) ? h('div', {
        style: {
          background: 'var(--panel)', border: '1px solid var(--danger, #d9534f)',
          borderRadius: '8px', padding: '10px 12px', display: 'grid', gap: '6px',
        },
      }, [
        h('div', { style: { fontSize: '12.5px', fontWeight: '620' }, text: '健康检查失败：' + (health.message || '未知原因') }),
        h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', wordBreak: 'break-all' }, text: '检查地址：' + (health.url || '（未配置）') }),
        h('div', { style: { fontSize: '11.5px', color: 'var(--text-dim)', lineHeight: '1.6' }, text: healthHint(health) }),
        h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
          h('button.btn.btn-sm.btn-primary', { text: '重新检查', onclick: () => recheckHealth(s) }),
          h('button.btn.btn-sm', { text: '查看日志', onclick: () => openLogs(s) }),
          h('button.btn.btn-sm', { text: '改检查地址', onclick: () => newServiceModal(load, s) }),
        ]),
      ]) : null,

      // 状态详情
      state.detail ? h('div', {
        style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.5' },
        text: state.detail,
      }) : null,

      // 操作
      h('div', { style: { display: 'flex', gap: '5px', flexWrap: 'wrap', marginTop: 'auto', paddingTop: '4px' } }, [
        state.running
          ? h('button.btn.btn-sm', { text: '停止', disabled: !canControl, onclick: () => act(s, 'stop') })
          : h('button.btn.btn-sm.btn-ok', { text: '启动', disabled: !canControl, onclick: () => act(s, 'start') }),
        h('button.btn.btn-sm', { text: '重启', disabled: !canControl, onclick: () => act(s, 'restart') }),
        isQwenService(s) ? h('button.btn.btn-sm', { text: '模型', title: '切换 Base / CustomVoice', onclick: () => openQwenModels(s) }) : null,
        h('button.btn.btn-sm', { text: '日志', onclick: () => openLogs(s) }),
        h('button.btn.btn-sm', { text: '详情', onclick: () => openDetail(s) }),
      ]),
    ]);
  }

  // recheckHealth 重查单个服务的健康状态。
  //
  // 只请求这一个服务（GET /services/{name} 会带上最新的 health），
  // 然后原地更新列表里的那一条并重画 —— 不整页重载，因为整页要重新
  // 查询所有服务（含 colima/compose 这类偏慢的），而用户此刻只关心这一个。
  async function recheckHealth(s) {
    const t = toast(`正在重新检查「${s.display_name || s.name}」…`, 'info', 0);
    try {
      const fresh = await api.service(s.name);
      const list = (cache && cache.list) || [];
      const i = list.findIndex((x) => x.name === s.name);
      if (i >= 0) list[i] = fresh;
      t.remove();
      const hl = fresh.health || {};
      if (!hl.checked) {
        toast('该服务没有配置健康检查地址', 'warn', 8000);
      } else if (hl.ok) {
        toast(`「${fresh.display_name || s.name}」健康检查已通过`, 'ok');
      } else {
        toast(`仍然失败：${hl.message || '未知原因'}`, 'warn', 9000);
      }
      renderToolbar();
      renderCards();
    } catch (e) {
      t.remove();
      toast('重新检查失败：' + e.message, 'err', 9000);
    }
  }

  async function act(s, action) {
    const labels = { start: '启动', stop: '停止', restart: '重启' };
    const t = toast(`${labels[action]}「${s.display_name}」中…`, 'info', 0);
    try {
      const r = await api.serviceAction(s.name, action);
      t.remove();
      const okMsg = r.state?.running ? '已启动' : '已停止';
      toast(`「${s.display_name}」${okMsg}（${r.cost_ms}ms）`, 'ok');
    } catch (e) {
      t.remove();
      toast(`${labels[action]}失败：${e.message}`, 'err', 12000);
    }
    load();
  }

  // ---------- 实时日志 ----------
  function openLogs(s) {
    const box = h('pre.logbox', { style: { maxHeight: '440px', minHeight: '200px' }, text: '正在连接日志流…\n' });
    let es = null;
    let paused = false;

    const follow = h('input', { type: 'checkbox', checked: true });
    const status = h('span.pill', { text: '连接中…' });

    const start = () => {
      if (es) es.close();
      status.className = 'pill';
      status.textContent = '连接中…';
      es = sseServiceLogs(s.name, {
        onData: (chunk) => {
          status.className = 'pill ok';
          status.textContent = '实时';
          if (paused) return;
          box.textContent += chunk;
          // 限制内存：超过 200KB 时只保留后一半
          if (box.textContent.length > 200 * 1024) {
            box.textContent = box.textContent.slice(-100 * 1024);
          }
          box.scrollTop = box.scrollHeight;
        },
        onError: async (_ev, src) => {
          // readyState === CLOSED 说明这次连接是**被服务端拒绝**的（非 200 或不是
          // event-stream），浏览器不会再重连。此时还说"正在重连…"，
          // 用户就在等一个永远不会发生的事 —— 这是误导。
          // 真机上最常见的触发点：这个服务没有任何可跟踪的日志文件（后端返回 400）。
          if (src && src.readyState === EventSource.CLOSED) {
            status.className = 'pill warn';
            status.textContent = '无法读取日志';
            // 去非流式接口问一句真正的原因，别把原因硬编码在文案里。
            let why = '';
            try {
              await api.serviceLogs(s.name, 1);
            } catch (e) {
              why = e.message;
            }
            box.textContent = '（日志流无法建立' + (why ? '：' + why : '') + '）\n' + box.textContent;
            return;
          }
          status.className = 'pill warn';
          status.textContent = '连接中断，正在重连…';
        },
        onClose: () => {
          status.className = 'pill';
          status.textContent = '日志流已结束';
        },
      });
    };

    const stop = () => { if (es) { es.close(); es = null; } };

    const m = modal({
      title: `日志：${s.display_name}`,
      wide: true,
      body: h('div', [
        h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', marginBottom: '10px', flexWrap: 'wrap' } }, [
          status,
          h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '12.5px' } }, [
            follow, h('span', { text: '自动滚动' }),
          ]),
          h('button.btn.btn-sm', {
            text: '清空显示',
            onclick: () => { box.textContent = ''; },
          }),
          h('button.btn.btn-sm', {
            text: '复制全部',
            onclick: async () => {
              try {
                await navigator.clipboard.writeText(box.textContent);
                toast('已复制到剪贴板', 'ok');
              } catch { toast('复制失败（浏览器限制）', 'warn'); }
            },
          }),
          h('span.sub', { text: s.log_path ? '日志文件：' + s.log_path : '' }),
        ]),
        box,
        h('div.hint', { style: { marginTop: '10px' }, text: '日志由服务端实时推送（SSE）。面板重启或服务重启后会自动重连。' }),
      ]),
      onClose: stop,
    });
    follow.addEventListener('change', () => { paused = !follow.checked; });
    start();
  }

  // ---------- 服务详情 ----------
  // ---------------------------------------------------------------------
  //  音色接收端：更换共享密钥
  //
  //  为什么单独做入口：这个密钥是**网站与接收端之间的共享凭据**，会泄露、要轮换。
  //  以前只能"重新部署接收端"才能换（那会连 receiver.py 一起重写、重新登记服务），
  //  而用户想要的只是换这一样东西 —— 换密钥不该有别的副作用。
  //
  //  改完**网站那边必须同步改**（openaiKey，接收端密钥由它推导），
  //  否则上传音色与合成会立刻 403 —— 所以弹窗里要把这句话说在最前面。
  // ---------------------------------------------------------------------
  const RECEIVER_LABEL = 'com.zizdog.voicereceiver';

  function randomReceiverToken() {
    const b = new Uint8Array(16);
    crypto.getRandomValues(b);
    return 'ttsv-' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  }

  // 与后端 ValidateReceiverToken 同一套规则（字符集会写进 plist XML，脏字符会让服务起不来）
  function validReceiverToken(t) {
    return /^[A-Za-z0-9._-]{8,128}$/.test(t);
  }

  function changeReceiverToken(parentModal) {
    const input = h('input.input', { value: randomReceiverToken(), spellcheck: 'false' });
    const hint = h('div.hint', {});
    const sync = () => {
      hint.textContent = validReceiverToken(input.value.trim())
        ? '只能包含字母、数字、- _ .（8–128 个字符）。'
        : '⚠️ 密钥只能包含字母、数字、- _ .，长度 8–128（不能有空格或 & < > 等字符）。';
      hint.style.color = validReceiverToken(input.value.trim()) ? 'var(--text-mute)' : 'var(--danger)';
    };
    input.addEventListener('input', sync);
    sync();

    const m = modal({
      title: '更改音色接收端共享密钥',
      body: h('div', [
        h('div', {
          style: {
            background: 'var(--warn-soft)', border: '1px solid var(--border)',
            borderRadius: '8px', padding: '10px 12px', marginBottom: '12px', fontSize: '12.5px', lineHeight: '1.7',
          },
          text: '改完密钥后，网站 TtsVoice 插件的 openaiKey 必须同步改成新值'
            + '（接收端地址与 refUploadToken 都由它推导）。改完之前，上传音色与合成立刻会 403。',
        }),
        h('div.field', [h('label', { text: '新密钥' }), input, hint]),
        h('div', { style: { display: 'flex', gap: '8px', marginTop: '4px' } }, [
          h('button.btn.btn-sm', {
            text: '🎲 重新生成',
            onclick: () => { input.value = randomReceiverToken(); sync(); },
          }),
        ]),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-primary', {
          text: '确认更换',
          onclick: () => {
            const token = input.value.trim();
            if (!validReceiverToken(token)) { toast('密钥不合法：只能字母数字 - _ .，长度 8–128', 'warn'); return; }
            close();
            if (parentModal) parentModal.close();
            // 走任务中心：要重写 plist、重载守护进程、再用新密钥验证一次
            taskCenter.start({
              kind: 'receiver-token',
              target: 'voicereceiver',
              title: '更换音色接收端共享密钥',
              start: () => api.changeReceiverToken(token),
            });
          },
        }),
      ],
    });
  }

  async function openDetail(s) {
    let data;
    try {
      data = await api.service(s.name);
    } catch (e) {
      toast(e.message, 'err');
      return;
    }
    const cur = data;
    const state = cur.state || {};
    const health = cur.health || {};

    const rows = [
      ['服务标识', cur.name],
      ['类型', cur.kind === 'native' ? '原生（launchd / 命令）' : cur.kind],
      ['分类', cur.category],
      ['端口', cur.port || '—'],
      ['运行状态', state.status + (state.detail ? '（' + state.detail + '）' : '')],
      ['进程 PID', state.pid || '—'],
      ['健康检查', health.checked ? (health.ok ? '正常' : '失败') + '：' + health.message : '未配置'],
      ['检查地址', health.url || '—'],
      // 详情里同样给出"怎么办"，否则用户点进详情还是只有一个"失败"
      ...(health.checked && !health.ok ? [['建议', healthHint(health)]] : []),
      ['访问地址', state.endpoint || '—'],
    ];
    if (cur.launch_label) rows.push(['launchd 标签', cur.launch_label]);
    if (cur.plist_path) rows.push(['plist 路径', cur.plist_path]);
    if (cur.start_cmd) rows.push(['启动命令', cur.start_cmd]);
    if (cur.work_dir) rows.push(['工作目录', cur.work_dir]);
    if (cur.log_path) rows.push(['日志文件', cur.log_path]);
    if (cur.compose_file) rows.push(['compose 文件', cur.compose_file]);
    if (cur.container) rows.push(['容器名', cur.container]);

    const body = h('div', [
      h('dl.kv', rows.flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { text: String(v) })])),
      h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
        h('button.btn.btn-sm', {
          text: '✏️ 编辑配置',
          onclick: (ev) => { m.close(); newServiceModal(load, cur); },
        }),
        // 音色接收端专有：换共享密钥
        cur.launch_label === RECEIVER_LABEL ? h('button.btn.btn-sm', {
          text: '🔑 更改共享密钥',
          title: '重新生成或指定「网站 ↔ 接收端」之间的共享密钥',
          onclick: () => changeReceiverToken(m),
        }) : null,
        h('button.btn.btn-sm', {
          text: '🚫 从面板移除',
          title: '只移除面板里的记录，系统上的服务不受影响',
          onclick: async () => {
            if (!await confirmBox('从面板移除该服务？\n\n这只删除面板里的记录，系统上的服务/容器不会被停止或删除。', { title: '移除服务' })) return;
            try {
              await api.serviceForget(cur.name);
              toast('已从面板移除', 'ok');
              m.close(); load();
            } catch (e) { toast(e.message, 'err'); }
          },
        }),
        cur.managed ? h('button.btn.btn-danger.btn-sm', {
          text: '🗑 卸载服务',
          title: '停止并删除由面板安装的服务',
          onclick: async () => {
            if (!await confirmBox(
              `将卸载「${cur.display_name}」。\n\n` +
              (cur.kind === 'compose'
                ? '这会停止并删除容器与网络（具名数据卷会保留）。'
                : '这会卸载软件包并删除其后台服务配置。'),
              { title: '卸载服务', danger: true, okText: '确认卸载' })) return;
            // 卸载是异步任务（可能跑几分钟）：请求立刻返回 task_id，
            // 进度与结果都在任务中心的进度窗里看，关窗口不会中断它。
            m.close();
            taskCenter.start({
              kind: 'uninstall',
              target: cur.name,
              title: `卸载 ${cur.display_name}`,
              start: () => api.serviceUninstall(cur.name),
            });
            load();
          },
        }) : h('button.btn.btn-sm', {
          text: 'ℹ️ 这是纳管服务',
          disabled: true,
          title: '该服务由你自己安装，面板不会卸载它',
        }),
      ]),
    ]);

    const m = modal({ title: `服务：${cur.display_name}`, wide: true, body });
  }

  // ---------- 扫描可纳管服务 ----------
  async function scanAdoptable() {
    const box = h('div', [h('div.empty', [h('div.big', { text: '🔍' }), h('p', { text: '正在扫描本机的 launchd 服务…' })])]);
    const m = modal({ title: '扫描可纳管服务', wide: true, body: box });

    let list = [];
    try {
      const r = await api.adoptable();
      list = r.list || [];
    } catch (e) {
      clear(box);
      appendAll(box, h('div.empty', [h('div.big', { text: '⚠️' }), h('p', { text: e.message })]));
      return;
    }

    clear(box);
    if (!list.length) {
      appendAll(box, h('div.empty', [
        h('div.big', { text: '✅' }),
        h('h4', { text: '没有发现可纳管的新服务' }),
        h('p', { text: '本机上已运行的第三方 launchd 服务都已经在面板里了。' }),
      ]));
      return;
    }

    appendAll(box, 
      h('div.hint', { style: { marginBottom: '12px' }, text: '以下是本机正在使用的 launchd 服务。纳管后可以在面板里查看状态、启停、看日志；面板不会卸载它们。' }),
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '名称' }), h('th', { text: '状态' }), h('th', { text: '可执行文件' }), h('th', { text: '操作' }),
        ])]),
        h('tbody', list.map((c) => h('tr', [
          h('td', [
            h('div', { style: { fontWeight: '550' }, text: c.label }),
            h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: c.plist_path }),
          ]),
          h('td', h('span.pill' + (c.running ? '.ok' : ''), { text: c.running ? '运行中' : '已停止' })),
          h('td.mono', { style: { fontSize: '11px' }, text: c.program || '—' }),
          h('td', h('button.btn.btn-sm.btn-primary', {
            text: '纳管',
            onclick: async (ev) => {
              const btn = ev.target;
              btn.disabled = true;
              btn.textContent = '纳管中…';
              try {
                await api.adopt({ label: c.label, display_name: c.label, icon: '🧩' });
                toast(`已纳管 ${c.label}`, 'ok');
                ev.target.closest('tr').style.opacity = '0.4';
                btn.textContent = '已纳管';
                load();
              } catch (e) {
                toast(e.message, 'err', 9000);
                btn.disabled = false;
                btn.textContent = '纳管';
              }
            },
          })),
        ]))),
      ]),
    );
  }

  registerCleanup(() => { });
  async function doUnload(mdl) {
    const okGo = await confirmBox(
      '释放「' + mdl.label + '」占用的内存？\n\n' +
      '释放后该能力将不可立即使用；下次有请求时会自动重新加载（约 22-26 秒）。',
      { title: '释放内存', okText: '释放', danger: true }
    );
    if (!okGo) return;
    try {
      await api.qwenUnloadModel(mdl.name);
      toast('已释放 ' + mdl.label, 'ok');
      await load();
    } catch (e) {
      toast('释放失败：' + e.message, 'danger');
    }
  }

  load();
}

// ============================================================================
//  注册 / 编辑服务弹窗（供服务页与市场页复用）
// ============================================================================

export function newServiceModal(onDone, existing = null) {
  const isEdit = !!existing;
  const c = existing || {};

  const name = h('input.input', { value: c.name || '', placeholder: '英文标识，如 my-api', disabled: isEdit });
  const display = h('input.input', { value: c.display_name || '', placeholder: '展示名称，如 我的接口服务' });
  const kind = h('select.select', [
    h('option', { value: 'native', text: '原生服务（launchd 或启动命令）', selected: (c.kind || 'native') === 'native' }),
    h('option', { value: 'docker', text: 'Docker 容器', selected: c.kind === 'docker' }),
    h('option', { value: 'compose', text: 'Docker Compose 项目', selected: c.kind === 'compose' }),
  ]);
  const category = h('select.select', [
    h('option', { value: 'custom', text: '自定义', selected: (c.category || 'custom') === 'custom' }),
    h('option', { value: 'ai', text: 'AI 服务', selected: c.category === 'ai' }),
    h('option', { value: 'tool', text: '运维工具', selected: c.category === 'tool' }),
    h('option', { value: 'lnmp', text: '网站环境', selected: c.category === 'lnmp' }),
  ]);
  const port = h('input.input', { type: 'number', value: c.port || '', placeholder: '服务监听的端口' });
  const launchLabel = h('input.input', { value: c.launch_label || '', placeholder: '如 com.example.myapi' });
  const plistPath = h('input.input', { value: c.plist_path || '', placeholder: '留空则自动在 LaunchDaemons / LaunchAgents 中查找' });
  const startCmd = h('input.input', { value: c.start_cmd || '', placeholder: '如 /usr/bin/python3 -m http.server 9000' });
  const workDir = h('input.input', { value: c.work_dir || '', placeholder: '可选，服务的工作目录' });
  const healthURL = h('input.input', { value: c.health_url || '', placeholder: '如 http://127.0.0.1:9000/health' });
  const healthExpect = h('input.input', { value: c.health_expect || '', placeholder: '可选，响应里必须包含的内容' });
  const logPath = h('input.input', { value: c.log_path || '', placeholder: '留空则从 plist 读取；命令服务默认写到面板目录' });
  const container = h('input.input', { value: c.container || '', placeholder: '容器名（默认与服务标识相同）' });
  const composeFile = h('input.input', { value: c.compose_file || '', placeholder: 'docker-compose.yml 的绝对路径' });

  // 按类型显示不同字段
  const nativeBox = h('div', [
    h('div.field', [h('label', { text: 'launchd 标签' }), launchLabel,
      h('div.hint', { text: '如果服务由 launchd 托管，填标签（如 com.example.api）；留空并填下面的启动命令则由面板作为子进程运行。' })]),
    h('div.field', [h('label', { text: 'plist 路径（可选）' }), plistPath]),
    h('div.field', [h('label', { text: '启动命令（无 launchd 时使用）' }), startCmd,
      h('div.hint', { text: '⚠️ 用启动命令的服务是面板的子进程：面板重启后它们会停止。需要长期稳定运行请改用 launchd。' })]),
    h('div.field', [h('label', { text: '工作目录' }), workDir]),
  ]);
  const dockerBox = h('div', [
    h('div.field', [h('label', { text: '容器名' }), container,
      h('div.hint', { text: '面板直接通过 Docker API 管理容器，不需要 docker CLI。' })]),
  ]);
  const composeBox = h('div', [
    h('div.field', [h('label', { text: 'compose 文件路径' }), composeFile,
      h('div.hint', { text: '指向一个已存在的 docker-compose.yml；面板用 docker compose 管理它。' })]),
  ]);

  const updateKind = () => {
    nativeBox.hidden = kind.value !== 'native';
    dockerBox.hidden = kind.value !== 'docker';
    composeBox.hidden = kind.value !== 'compose';
  };
  kind.addEventListener('change', updateKind);
  updateKind();

  const submit = async (close) => {
    const payload = {
      name: name.value.trim(),
      display_name: display.value.trim(),
      kind: kind.value,
      category: category.value,
      port: Number(port.value) || 0,
      launch_label: launchLabel.value.trim(),
      plist_path: plistPath.value.trim(),
      start_cmd: startCmd.value.trim(),
      work_dir: workDir.value.trim(),
      health_url: healthURL.value.trim(),
      health_expect: healthExpect.value.trim(),
      log_path: logPath.value.trim(),
      container: container.value.trim(),
      compose_file: composeFile.value.trim(),
    };
    if (!payload.name && !payload.display_name) {
      toast('请填写服务标识或展示名称', 'warn');
      return;
    }
    if (payload.kind === 'native' && !payload.launch_label && !payload.start_cmd) {
      toast('原生服务需要填 launchd 标签或启动命令', 'warn');
      return;
    }
    try {
      if (isEdit) await api.serviceUpdate(c.name, payload);
      else await api.serviceCreate(payload);
      toast(isEdit ? '已保存' : '服务已注册', 'ok');
      close();
      if (onDone) onDone();
    } catch (e) {
      toast(e.message, 'err', 10000);
    }
  };

  const m = modal({
    title: isEdit ? '编辑服务：' + c.display_name : '注册服务',
    wide: true,
    body: h('div', [
      h('div.row', [
        h('div.field', [h('label', { text: '服务标识 *' }), name,
          h('div.hint', { text: isEdit ? '标识创建后不可修改' : '小写英文/数字/连字符，用于内部引用' })]),
        h('div.field', [h('label', { text: '展示名称' }), display]),
      ]),
      h('div.row', [
        h('div.field', [h('label', { text: '类型' }), kind]),
        h('div.field', [h('label', { text: '分类' }), category]),
        h('div.field', [h('label', { text: '端口' }), port]),
      ]),
      nativeBox, dockerBox, composeBox,
      h('div.section-title', { style: { marginTop: '8px' }, text: '健康检查（推荐填写）' }),
      h('div.row', [
        h('div.field', [h('label', { text: '健康检查地址' }), healthURL]),
        h('div.field', [h('label', { text: '期望包含内容' }), healthExpect]),
      ]),
      h('div.field', [h('label', { text: '日志文件路径' }), logPath]),
    ]),
    footer: (close) => [
      h('button.btn', { text: '取消', onclick: close }),
      h('button.btn.btn-primary', { text: isEdit ? '保存' : '注册服务', onclick: () => submit(close) }),
    ],
  });
  setTimeout(() => (isEdit ? display : name).focus(), 60);
}


// ---------------------------------------------------------------------------
//  Qwen3 TTS 的双模型切换
//
//  为什么要这个入口：Base 与 CustomVoice 能力**互斥** ——
//  Base 能克隆音色但会静默忽略预置音色参数，CustomVoice 有 9 个预置音色
//  但不会克隆。网站上「上传过音色就用克隆、没上传就用默认音色」要两全，
//  就得两个模型都能用。而它们各自加载后都常驻内存（服务端没有淘汰机制），
//  所以这里做成显式切换：加载目标、卸载其余，避免同时占约 10GB。
// ---------------------------------------------------------------------------

// qwenLaunchLabel 与服务端登记时用的 launchd 标签一致。
const qwenLaunchLabel = 'com.zizdog.qwen3tts';

function isQwenService(s) {
  return s && (s.launch_label === qwenLaunchLabel || s.name === 'com-zizdog-qwen3tts');
}

function openQwenModels(svc) {
  const body = h('div', { style: { display: 'flex', flexDirection: 'column', gap: '10px' } }, [
    h('div', {
      style: { fontSize: '12px', color: 'var(--text-mute)', lineHeight: '1.6' },
      text: '两个模型能力互斥：Base 负责音色克隆，CustomVoice 负责 9 个预置音色。' +
        '实测两个都常驻内存约 4GB（16GB 机器上可用内存仍有 92%、无交换），' +
        '所以默认两个都加载好，插件发来哪种请求都能立刻响应；需要腾内存时可单独释放。',
    }),
    h('div', { id: 'qwen-model-list', style: { display: 'flex', flexDirection: 'column', gap: '8px' } }, [
      h('div', { style: { fontSize: '12px', color: 'var(--text-mute)' }, text: '读取中…' }),
    ]),
  ]);

  const m = modal({ title: 'Qwen 模型（' + (svc.display_name || svc.name) + '）', body });

  async function load() {
    const box = document.getElementById('qwen-model-list');
    if (!box) return;
    clear(box);
    let data;
    try {
      data = await api.qwenModels();
    } catch (e) {
      appendAll(box, [h('div', { style: { fontSize: '12px', color: 'var(--danger)' }, text: '读取失败：' + e.message })]);
      return;
    }
    const list = (data && data.list) || [];
    const active = (data && data.active) || '';

    for (const mdl of list) {
      const isActive = mdl.name === active || (mdl.loaded && !active);
      const pills = [];
      pills.push(h('span.pill' + (mdl.loaded ? '.ok' : ''), {
        text: mdl.loaded ? '已驻留内存' : (mdl.downloaded ? '已下载·未加载' : '未下载'),
        title: mdl.loaded ? '可立即推理' : (mdl.downloaded ? '首次使用或切换时会加载' : '需要先安装'),
      }));
      if (mdl.default) pills.push(h('span.pill.brand', { text: '插件默认', title: '插件里 openaiModel 默认填这个' }));

      appendAll(box, [
        h('div', {
          style: {
            border: '1px solid ' + (isActive ? 'var(--brand)' : 'var(--border)'),
            borderRadius: 'var(--radius)', padding: '11px 12px',
            display: 'flex', flexDirection: 'column', gap: '7px',
          },
        }, [
          h('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } }, [
            h('span', { style: { fontWeight: '620', fontSize: '13px' }, text: mdl.label }),
            ...pills,
          ]),
          h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)', lineHeight: '1.5' }, text: mdl.note || '' }),
          h('div', { style: { fontSize: '11px', color: 'var(--text-mute)', fontFamily: 'var(--mono, monospace)', wordBreak: 'break-all' }, text: mdl.name }),
          h('div', { style: { display: 'flex', gap: '6px', marginTop: '2px' } }, [
            mdl.loaded
              ? h('button.btn.btn-sm', { text: '释放内存', title: '下次用到会自动重新加载', onclick: () => doUnload(mdl) })
              : h('button.btn.btn-sm' + (mdl.downloaded ? '.btn-ok' : ''), {
                  text: mdl.downloaded ? '加载到内存' : '未下载，需重新安装 Qwen 服务',
                  disabled: !mdl.downloaded,
                  onclick: (ev) => doSwitch(mdl, ev.target),
                }),
          ]),
        ]),
      ]);
    }
    if (!list.length) {
      appendAll(box, [h('div', { style: { fontSize: '12px', color: 'var(--text-mute)' }, text: '没有可用模型。' })]);
    }
  }

  async function doSwitch(mdl, btn) {
    // 首次加载要读约 2GB 权重，实测约 26 秒 —— 必须告知用户，否则会以为卡死。
    const okGo = await confirmBox(
      '把「' + mdl.label + '」加载到内存？\n\n首次加载需读取约 2GB 权重，实测约 22-26 秒；' +
      '完成前该服务无法推理。加载后另一个模型会保留，两者可同时使用。',
      { title: '加载模型', okText: '开始加载' }
    );
    if (!okGo) return;
    const old = btn.textContent;
    btn.disabled = true;
    btn.textContent = '切换中，请稍候…';
    try {
      await api.qwenSwitchModel(mdl.name);
      toast('已加载 ' + mdl.label, 'ok');
      await load();
    } catch (e) {
      toast('切换失败：' + e.message, 'danger');
      btn.disabled = false;
      btn.textContent = old;
    }
  }

  load();
}
