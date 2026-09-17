// docker-containers.js —— Docker 页的「容器」分区。
//
// 这是 Docker 页最重要的分区：日常要做的就是"看看现在跑着什么、起停一下、翻日志"。
//
// 四个刻意的设计：
//
//  1. **默认显示全部容器**（含已停止的）。只看运行中的会让人以为"我的容器不见了"，
//     而实际最常见的困惑正是"我创建的那个容器怎么没了"（其实是退出了）。
//
//  2. **日志用抽屉而不是新页面**：排查问题时需要在列表和日志之间反复看，
//     弹一个盖住全屏的 modal 会让人没法对照容器列表。
//
//  3. **日志默认读"完整日志"**，并且能自动刷新。很多镜像的初始登录信息
//     （File Browser 的 admin 随机口令就是典型）**只在首次启动时打进日志一次**，
//     只取尾部几百行会把它翻没，用户就永远登不进去。openContainerLogs 因此
//     导出给 docker-compose.js 的"部署完成"结果复用，不另写一套弹窗。
//
//  4. **创建容器走任务中心**：本地没有镜像时后端会先拉镜像，
//     这一步可能几分钟，层进度必须逐行可见，也不能被"用户刷新页面"杀掉。

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, appendAll } from './ui.js';
import { taskCenter } from './tasks.js';

// statePill 把容器状态映射成一个带颜色的 pill。
//
// Docker 的 State 取值：created / running / paused / restarting / removing / exited / dead。
function statePill(state) {
  const s = String(state || '').toLowerCase();
  // 用 h 的 "tag.class" 语法，类名直接写全，避免运行时拼字符串拼出双点
  const spec = {
    running: 'span.pill.ok',
    created: 'span.pill.brand',
    restarting: 'span.pill.warn',
    paused: 'span.pill.warn',
    exited: 'span.pill',
    dead: 'span.pill.warn',
  }[s] || 'span.pill';
  return h(spec, { text: s || 'unknown' });
}

// portsText 把端口数组拼成 "0.0.0.0:8080→80/tcp" 这样的可读文本。
function portsText(ports) {
  if (!Array.isArray(ports) || ports.length === 0) return '—';
  const parts = [];
  for (const p of ports) {
    const proto = p.Type || 'tcp';
    if (p.PublicPort) {
      parts.push(`${p.IP || '0.0.0.0'}:${p.PublicPort}→${p.PrivatePort}/${proto}`);
    } else if (p.PrivatePort) {
      parts.push(`${p.PrivatePort}/${proto}`);
    }
  }
  return parts.length ? parts.join('  ') : '—';
}

// fmtTime 把 Unix 秒格式化成本地时间。
function fmtTime(sec) {
  if (!sec) return '—';
  const d = new Date(sec * 1000);
  if (Number.isNaN(d.getTime())) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export async function renderContainers(container, ctx) {
  let showAll = true;

  const toolbar = h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '10px' } });
  const tableBox = h('div');

  container.append(toolbar, tableBox);

  async function load() {
    clear(tableBox);
    tableBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取容器…' })]));

    let list = [];
    try {
      const res = await api.dockerContainers(showAll);
      list = (res && res.list) || [];
    } catch (e) {
      clear(tableBox);
      tableBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取容器列表失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }

    clear(tableBox);
    if (list.length === 0) {
      tableBox.append(h('div.empty', [
        h('div.big', { text: '📦' }),
        h('h4', { text: showAll ? '还没有任何容器' : '没有运行中的容器' }),
        h('p', { text: showAll ? '点右上角「创建容器」从镜像起一个，或到 Compose 分区部署一个项目。' : '勾选「显示已停止」可以看到已退出的容器。' }),
      ]));
      return;
    }

    const rows = list.map((c) => {
      const name = primaryName(c);
      const running = String(c.State).toLowerCase() === 'running';

      const actions = h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } });
      if (running) {
        actions.append(
          h('button.btn.btn-ghost.btn-sm', { text: '停止', onclick: () => act(name, 'stop') }),
          h('button.btn.btn-ghost.btn-sm', { text: '重启', onclick: () => act(name, 'restart') }),
        );
      } else {
        actions.append(h('button.btn.btn-primary.btn-sm', { text: '启动', onclick: () => act(name, 'start') }));
      }
      actions.append(
        h('button.btn.btn-ghost.btn-sm', { text: '日志', onclick: () => openContainerLogs(name) }),
        h('button.btn.btn-ghost.btn-sm', { text: '详情', onclick: () => showDetail(name) }),
        h('button.btn.btn-danger.btn-sm', { text: '删除', onclick: () => remove(name, running) }),
      );

      return h('tr', [
        h('td', [
          h('div', { style: { fontWeight: '600' }, text: name }),
          h('div.hint', { text: shortID(c.ID) + ' · ' + fmtTime(c.Created) }),
        ]),
        h('td', [h('span', { text: c.Image || '—', title: c.Image })]),
        h('td', [statePill(c.State), h('div.hint', { text: c.Status || '' })]),
        h('td', [h('code', { text: portsText(c.Ports) })]),
        h('td', [actions]),
      ]);
    });

    tableBox.append(h('table.table', [
      h('thead', [h('tr', [
        h('th', { text: '容器' }), h('th', { text: '镜像' }), h('th', { text: '状态' }),
        h('th', { text: '端口' }), h('th', { text: '操作' }),
      ])]),
      h('tbody', rows),
    ]));
  }

  function primaryName(c) {
    const names = c.Names || [];
    if (!names.length) return shortID(c.ID);
    return String(names[0]).replace(/^\//, '');
  }

  function shortID(id) {
    const s = String(id || '');
    return s.startsWith('sha256:') ? s.slice(7, 19) : s.slice(0, 12);
  }

  async function act(name, action) {
    try {
      await api.dockerContainerAction(name, action);
      toast(`${name}：${action} 完成`, 'ok');
      await load();
      ctx.refresh && ctx.refresh();
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  async function remove(name, running) {
    const extra = running
      ? '\n\n它正在运行：确认后会先强杀再删除，容器内未落盘的数据会丢失。'
      : '';
    if (!await confirmBox(`删除容器 ${name}？${extra}\n\n匿名数据卷会一并删除；具名卷与镜像保留。`,
      { title: '删除容器', danger: true, okText: running ? '强杀并删除' : '删除' })) return;
    try {
      await api.dockerContainerRemove(name, running);
      toast(`已删除 ${name}`, 'ok');
      await load();
      ctx.refresh && ctx.refresh();
    } catch (e) {
      toast(e.message, 'err');
    }
  }

  async function showDetail(name) {
    const box = h('div', { text: '正在读取…' });
    const m = modal({
      title: `容器详情 · ${name}`,
      wide: true,
      body: box,
      footer: [h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() })],
    });

    let d;
    try {
      d = await api.dockerContainerInspect(name);
    } catch (e) {
      clear(box);
      box.append(h('div.empty', [h('p', { text: '读取失败：' + e.message })]));
      return;
    }

    const cfg = d.Config || {};
    const st = d.State || {};
    const host = d.HostConfig || {};
    const net = d.NetworkSettings || {};

    const rows = [
      ['容器 ID', shortID(d.Id || '')],
      ['镜像', cfg.Image || '—'],
      ['状态', `${st.Status || '—'}${st.ExitCode ? '（退出码 ' + st.ExitCode + '）' : ''}`],
      ['创建时间', d.Created ? new Date(d.Created).toLocaleString() : '—'],
      ['启动时间', st.StartedAt && !String(st.StartedAt).startsWith('0001') ? new Date(st.StartedAt).toLocaleString() : '—'],
      ['重启策略', (host.RestartPolicy && host.RestartPolicy.Name) || 'no'],
      ['工作目录', cfg.WorkingDir || '—'],
      ['入口点', Array.isArray(cfg.Entrypoint) ? cfg.Entrypoint.join(' ') : (cfg.Entrypoint || '—')],
      ['命令', Array.isArray(cfg.Cmd) ? cfg.Cmd.join(' ') : (cfg.Cmd || '—')],
      ['环境变量', (cfg.Env || []).length ? (cfg.Env || []).join('\n') : '—'],
      ['挂载', (d.Mounts || []).map((mnt) => `${mnt.Source} → ${mnt.Destination}${mnt.RW ? '' : ' (ro)'}`).join('\n') || '—'],
      ['网络', Object.keys(net.Networks || {}).join(', ') || '—'],
    ];

    clear(box);
    box.append(h('table.table', [
      h('tbody', rows.map(([k, v]) => h('tr', [
        h('td', { style: { width: '130px', color: 'var(--text-dim)' }, text: k }),
        h('td', [h('span', { style: { whiteSpace: 'pre-wrap', wordBreak: 'break-all' }, text: String(v) })]),
      ]))),
    ]));
  }

  /** 创建容器表单。 */
  function openCreate() {
    const image = h('input.input', { placeholder: 'nginx:1.27', autofocus: true });
    const name = h('input.input', { placeholder: '留空自动生成，例如 my-nginx' });
    const ports = h('textarea.textarea', { rows: 3, placeholder: '一行一个，例如\n8080:80\n127.0.0.1:3306:3306' });
    const envs = h('textarea.textarea', { rows: 3, placeholder: '一行一个，KEY=VALUE，例如\nTZ=Asia/Shanghai\nMYSQL_ROOT_PASSWORD=secret' });
    const binds = h('textarea.textarea', { rows: 3, placeholder: '一行一个，/宿主绝对路径:/容器路径，例如\n/Users/zizdog/www:/usr/share/nginx/html:ro' });
    const command = h('input.input', { placeholder: '留空用镜像默认，例如 sleep 1000' });
    const restart = h('select.select', {}, [
      h('option', { value: 'no', text: 'no（不自动重启）' }),
      h('option', { value: 'unless-stopped', text: 'unless-stopped（推荐）', selected: true }),
      h('option', { value: 'always', text: 'always' }),
      h('option', { value: 'on-failure', text: 'on-failure' }),
    ]);
    const network = h('input.input', { placeholder: '留空用默认 bridge，例如 my-net' });
    const autoStart = h('input', { type: 'checkbox', checked: true });
    const privileged = h('input', { type: 'checkbox' });

    const lines = (el) => String(el.value || '').split('\n').map((s) => s.trim()).filter(Boolean);

    const m = modal({
      title: '创建容器',
      wide: true,
      body: h('div', { style: { display: 'grid', gap: '12px' } }, [
        h('div.field', [h('label', { text: '镜像 *（必须已存在，或先在「镜像」分区拉取）' }), image]),
        h('div.field', [h('label', { text: '容器名' }), name]),
        h('div.field', [h('label', { text: '端口映射' }), ports,
          h('div.hint', { text: '格式：宿主端口:容器端口（可加 /udp）。只写容器端口则由 Docker 随机分配宿主端口。' })]),
        h('div.field', [h('label', { text: '环境变量' }), envs]),
        h('div.field', [h('label', { text: '目录挂载' }), binds,
          h('div.hint', { text: '宿主路径必须是这台机器上的绝对路径；末尾加 :ro 表示只读。' })]),
        h('div.field', [h('label', { text: '启动命令' }), command,
          h('div.hint', { text: '按空白切分成参数数组交给 Docker，不经过 shell，也不解析引号 —— 所以“sh -c echo a; sleep 1”这类写法在这里不成立（会被切成多段）。需要复杂命令请用 Compose，或先写一个脚本文件再执行它。' })]),
        h('div', { style: { display: 'flex', gap: '16px', flexWrap: 'wrap' } }, [
          h('div.field', { style: { minWidth: '220px' } }, [h('label', { text: '重启策略' }), restart]),
          h('div.field', { style: { minWidth: '220px' } }, [h('label', { text: '网络' }), network]),
        ]),
        h('div', { style: { display: 'flex', gap: '18px', flexWrap: 'wrap' } }, [
          h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [autoStart, h('span', { text: '创建后立即启动' })]),
          h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [privileged, h('span', { text: '特权模式（--privileged，仅在确实需要时勾选）' })]),
        ]),
      ]),
      footer: [
        h('button.btn.btn-ghost', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-primary', { text: '创建', onclick: () => submit() }),
      ],
    });

    async function submit() {
      const spec = {
        image: image.value.trim(),
        name: name.value.trim(),
        command: command.value.trim(),
        ports: lines(ports),
        env: lines(envs),
        binds: lines(binds),
        restart_policy: restart.value,
        network: network.value.trim(),
        auto_start: autoStart.checked,
        privileged: privileged.checked,
      };
      if (!spec.image) {
        toast('必须填写镜像名', 'err');
        image.focus();
        return;
      }
      // 创建走任务中心：本地没有这个镜像时后端会先拉取（可能几分钟），
      // 拉取的层进度要逐行可见，也不能因为用户刷新页面被杀掉。
      // start() 只等"任务提交"这一步（后端立刻回 202），不等任务跑完；
      // 拿不到 task_id（例如参数被 400 拦下）就**不关表单**，用户还能改完重试。
      const id = await taskCenter.start({
        kind: 'docker-container-create',
        target: 'docker:container:' + (spec.name || spec.image),
        title: '创建容器 ' + (spec.name || spec.image),
        start: () => api.dockerContainerCreate(spec),
        onDone: async (m) => {
          if (m && m.status === 'succeeded') {
            toast(`已创建 ${spec.name || (m.result && m.result.id) || spec.image}${spec.auto_start ? ' 并已启动' : ''}`, 'ok');
          }
          await load();
          ctx.refresh && ctx.refresh();
        },
      });
      if (id) m.close();
    }
  }

  // ---- 工具栏 ----
  const allToggle = h('input', { type: 'checkbox', checked: showAll });
  allToggle.addEventListener('change', () => { showAll = allToggle.checked; load(); });

  appendAll(toolbar,
    h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [
      allToggle, h('span', { text: '显示已停止的容器' }),
    ]),
    h('div.spacer', { style: { flex: '1' } }),
    h('button.btn.btn-primary.btn-sm', { text: '＋ 创建容器', onclick: openCreate }),
  );

  await load();
}

// ---------------------------------------------------------------------------
//  容器日志弹窗
//
//  导出是因为 docker-compose.js 的"部署完成"结果也要用同一套弹窗：
//  部署完直接点容器看日志，不用先切到「容器」分区再找。
// ---------------------------------------------------------------------------

// LOG_TAIL_LINES 是"普通模式"一次读的行数：够一次排障看，又不会把整份日志塞进 DOM。
const LOG_TAIL_LINES = 2000;

// LOG_FOLLOW_MS 是自动刷新的间隔：docker 日志的常见节奏（秒级）够用。
const LOG_FOLLOW_MS = 2000;

/** diffTail 返回 newTail 相对 oldTail 新增的行；对不齐时返回 null。 */
function diffTail(oldTail, newTail) {
  const oldLines = String(oldTail == null ? '' : oldTail).replace(/\n+$/, '').split('\n');
  const newLines = String(newTail == null ? '' : newTail).replace(/\n+$/, '').split('\n');
  if (!oldLines[0] && oldLines.length === 1) return newLines.join('\n');
  if (!newLines[0] && newLines.length === 1) return '';
  const anchor = oldLines[oldLines.length - 1];
  if (!anchor) return null;
  // 从后往前找锚点：日志里重复行很常见，取最后一次出现的位置才不会漏行。
  for (let i = newLines.length - 1; i >= 0; i--) {
    if (newLines[i] === anchor) return newLines.slice(i + 1).join('\n');
  }
  return null;
}

/** tailOf 取一段日志的尾部窗口（自动刷新时按行对齐用）。 */
function tailOf(text) {
  const lines = String(text == null ? '' : text).replace(/\n+$/, '').split('\n');
  return lines.slice(-400).join('\n');
}

/**
 * openContainerLogs(name) 打开某个容器的日志。
 *
 * 三个刻意的取舍：
 *   · **打开就是完整日志**（后端 lines=0 → Docker 的 tail=all）。初始口令这类
 *     "只在首次启动出现一次"的信息在最早的几行里，只取尾部会把它翻没。
 *     日志特别多的容器可以勾「仅看最近 2000 行」少传一些。
 *   · **自动刷新是显式开关**（默认关）：每 2 秒拉一次尾部并**追加**新增行，
 *     用户往上翻时不会被拽回底部。
 *   · 对不齐（日志刷得太快、尾部窗口滚过去了）时**如实说**并回退成"显示最近 300 行"，
 *     绝不拼出一份看起来连续、其实缺行的日志。
 */
export function openContainerLogs(name) {
  let full = true;     // true = 完整日志（Docker tail=all）
  let follow = false;  // 自动刷新
  let timer = null;
  let lastTail = '';   // 上一次的尾部文本，用于算"新增了哪几行"
  let disposed = false;

  const box = h('pre', {
    style: {
      maxHeight: '60vh', overflow: 'auto', margin: '0', padding: '10px',
      background: 'var(--bg-soft, rgba(127,127,127,.08))', borderRadius: '6px',
      fontSize: '12px', lineHeight: '1.55', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
    },
    text: '正在读取日志…',
  });
  const status = h('span.hint', { text: '' });
  const followBox = h('input', { type: 'checkbox' });
  const tailOnlyBox = h('input', { type: 'checkbox' });

  const m = modal({
    title: `日志 · ${name}`,
    wide: true,
    body: h('div', [
      h('div', { style: { display: 'flex', gap: '12px', alignItems: 'center', flexWrap: 'wrap', marginBottom: '8px' } }, [
        h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '13px' } }, [
          followBox, h('span', { text: '自动刷新（每 2 秒追加新行）' }),
        ]),
        h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '13px' } }, [
          tailOnlyBox, h('span', { text: `仅看最近 ${LOG_TAIL_LINES} 行` }),
        ]),
        status,
      ]),
      h('div.hint', {
        style: { marginBottom: '8px' },
        text: '默认读完整日志：首次启动生成的初始登录信息（例如 File Browser 的 admin 随机口令）就在最早的几行里。'
          + '日志是纯文本、可能含口令，截图或转发前请留意；完整日志最多显示 8MB，超出部分会被截断。',
      }),
      box,
    ]),
    footer: [h('button.btn.btn-primary', { text: '关闭', onclick: () => m.close() })],
    onClose: () => { disposed = true; stopFollow(); },
  });

  function stickToBottom() {
    box.scrollTop = box.scrollHeight;
  }

  function stopFollow() {
    follow = false;
    followBox.checked = false;
    if (timer) { clearInterval(timer); timer = null; }
  }

  async function load({ keepFollow } = {}) {
    status.textContent = '正在读取…';
    try {
      // lines=0 是后端约定的"要完整日志"（Docker 的 tail=all）。
      const res = await api.dockerContainerLogs(name, full ? 0 : LOG_TAIL_LINES);
      if (disposed) return;
      const text = (res && res.logs) ? res.logs : '（没有日志输出）';
      box.textContent = text;
      lastTail = tailOf(text);
      const n = String(text).split('\n').length;
      status.textContent = full
        ? `完整日志 · ${n} 行（最多 8MB）`
        : `最近 ${LOG_TAIL_LINES} 行 · 共 ${n} 行`;
      stickToBottom();
    } catch (e) {
      if (disposed) return;
      box.textContent = '读取失败：' + e.message;
      status.textContent = '读取失败';
      stopFollow();
      return;
    }
    // 首屏/手动切换时不自动开启跟随：用户没点开关就不该有定时器在跑。
    if (!keepFollow) stopFollow();
  }

  async function poll() {
    if (disposed || !follow) return;
    let res;
    try {
      res = await api.dockerContainerLogs(name, 300);
    } catch {
      status.textContent = '自动刷新失败（连接不上面板？）';
      return;
    }
    if (disposed || !follow) return;
    const text = (res && res.logs) ? res.logs : '';
    const added = diffTail(lastTail, text);
    if (added === null) {
      box.textContent = text || '（没有日志输出）';
      lastTail = tailOf(text);
      status.textContent = '日志刷得太快，尾部对不上；已改为显示最近 300 行';
      stickToBottom();
      return;
    }
    lastTail = tailOf(text);
    if (added) {
      const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
      box.textContent += (box.textContent && !box.textContent.endsWith('\n') ? '\n' : '') + added;
      if (atBottom) stickToBottom();
    }
    status.textContent = `自动刷新中 · ${box.textContent.split('\n').length} 行`;
  }

  followBox.addEventListener('change', () => {
    follow = followBox.checked;
    if (follow) {
      if (!timer) timer = setInterval(poll, LOG_FOLLOW_MS);
      status.textContent = '自动刷新中…';
      poll();
    } else {
      stopFollow();
    }
  });

  // 「仅看最近 N 行」：大日志不想一次性传完时的退路（勾上就重读）。
  tailOnlyBox.addEventListener('change', () => {
    full = !tailOnlyBox.checked;
    load({ keepFollow: follow });
  });

  load();
}
