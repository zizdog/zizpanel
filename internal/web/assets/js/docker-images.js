// docker-images.js —— Docker 管理页的「镜像」分区。
//
// 这个分区只负责镜像的四个动作：拉取、列出、删除、清理悬空层。
// 刻意**不做**镜像构建入口：构建需要 Dockerfile 与上下文目录，属于
// Compose/CI 的职责，面板里硬塞一个会变成半个 IDE（也因此这里没有
// 「导入 tar」「导出 tar」这类会让二进制膨胀的旁路功能）。
//
// 与主视图的分工：Docker 可用性由 docker.js 统一判断（不可用时根本不会
// 走到这里），所以本文件不做任何环境探测，直接假设 socket 可用。

import { api } from './api.js';
import { h, clear, toast, confirmBox, bytes } from './ui.js';
import { taskCenter } from './tasks.js';

// 错误提示统一用 toast(msg, 'err')，而不是字面的 'error'。
//
// 原因：ui.js 的图标映射与 app.css 的 .toast.* 只认 ok/err/warn 三种，
// 传 'error' 会退化成"无样式 + ℹ️ 图标"，用户看到的错误提示不像错误。
// 这条约定是踩过的坑，别的模块也都用 'err'。

// Docker 在镜像被容器占用 / 被其它镜像依赖 / 被多个 tag 引用时，
// 返回的错误里稳定带有这些关键词。命中后**不能**直接强删：强删会绕过
// Docker 的保护，让正在运行的容器底层文件系统出现空洞，是"面板把生产搞坏了"
// 的典型来源。所以统一走"讲清后果 + 二次确认"的路径。
const IN_USE_RE = /(being used|in use|is used by|conflict|referenced in multiple|dependent child)/i;

// 网络失败提示：判据与 internal/services/netfail.go 同源，NET_HINT_MARKER 必须逐字一致。
// 文案纪律：标题一句话 ≤40 字，改镜像/代理的入口收进折叠项。
const NET_HINT_MARKER = '面板不会替你翻墙';
const NET_HINT_ONE_LINE = '🌐 网络问题：面板不会替你翻墙，请自备代理/梯子后重试';
const NET_HINT_ENTRIES = 'Docker 页 →「加速源」（换 docker.io 镜像源）；面板设置 → 访问与安全 →「应用包镜像基址」。';
const NET_EVIDENCE = [
  'could not resolve host', 'temporary failure in name resolution',
  'name or service not known', 'no such host', 'server misbehaving',
  'connection refused', 'connection timed out', 'connection timeout',
  'connection reset by peer', 'no route to host', 'network is unreachable',
  'network is down', 'host is down', 'i/o timeout', 'operation timed out',
  'failed to connect to', "couldn't connect to server", 'could not connect to server',
  'dial tcp', 'dial udp',
  'tls handshake timeout', 'tls: failed to verify certificate',
  'tls: bad certificate', 'remote error: tls:', 'x509:',
  'certificate signed by unknown authority', 'certificate is not valid for',
  'client.timeout exceeded', 'request canceled while waiting for connection',
  'curl: (6)', 'curl: (7)', 'curl: (28)', 'curl: (35)', 'curl: (52)',
  'curl: (56)', 'curl: (60)', 'ssl connect error',
];
const NET_LOCAL_ENDPOINT = ['unix://', 'dial unix', 'docker.sock', '127.0.0.1', 'localhost', '[::1]'];
const NET_LOCAL_OVERRIDE = ['proxyconnect'];

// 只认有真实证据的网络失败，拿不准 false：误报比漏报更糟。
function isNetworkFailureText(text) {
  const msg = String(text == null ? '' : text).toLowerCase();
  if (!msg.trim()) return false;
  if (msg.includes(NET_HINT_MARKER)) return true;
  if (NET_LOCAL_OVERRIDE.some((s) => msg.includes(s))) return true; // 走代理失败优先
  if (NET_LOCAL_ENDPOINT.some((s) => msg.includes(s))) return false; // 本地 socket 没起来 ≠ 要翻墙
  if (NET_EVIDENCE.some((s) => msg.includes(s))) return true;
  if (msg.includes('context deadline exceeded')
    && (msg.includes('http://') || msg.includes('https://'))) return true;
  return false;
}

// networkHintBlock 是醒目展示块：danger 底 + pill + 一句话 + 折叠入口 + 原文。
function networkHintBlock(errText) {
  const raw = String(errText == null ? '' : errText).trim();
  const nodes = [
    h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('span.pill.danger', { style: { fontSize: '12px', fontWeight: '700' }, text: '🌐 网络问题' }),
      h('strong', { text: NET_HINT_ONE_LINE }),
    ]),
    h('details', { style: { marginTop: '6px' } }, [
      h('summary', { style: { cursor: 'pointer', color: 'var(--text-dim)' }, text: '面板里改镜像/代理的入口' }),
      h('div', { style: { marginTop: '4px', color: 'var(--text-dim)' }, text: NET_HINT_ENTRIES }),
    ]),
  ];
  if (raw) {
    nodes.push(h('div', {
      style: {
        marginTop: '6px', fontFamily: 'var(--mono)', fontSize: '11.5px',
        color: 'var(--text-dim)', wordBreak: 'break-all',
      },
      text: '原始报错：' + raw,
    }));
  }
  return h('div', {
    dataset: { testid: 'zp-network-failure' },
    style: {
      padding: '11px 13px', background: 'var(--danger-soft)',
      border: '1px solid var(--danger)', borderRadius: 'var(--radius)',
      fontSize: '12.5px', lineHeight: '1.7', marginBottom: '12px',
    },
  }, nodes);
}

// netFailure 保存本分区最近一次"网络类"失败（拉取任务在任务中心结束时写入）。
// 它必须在模块级：ctx.refresh() 会重建整个分区，函数局部状态会被冲掉。
let netFailure = '';

/** tagsOf 把 RepoTags 规范化成数组。 */
// 坑（真实发生过）：悬空镜像 <none>:<none> 的 RepoTags 在 JSON 里是 null
// 而不是 []，直接 .map/.length 会抛 TypeError，把整个列表打成空白页 ——
// 而"列表空白"看起来像后端挂了，排查方向会被完全带偏。
function tagsOf(img) {
  const t = img && img.RepoTags;
  return Array.isArray(t) ? t.filter(Boolean) : [];
}

/** shortId 把 "sha256:abcd…" 截成 12 位短 ID（与 docker images 输出一致）。 */
function shortId(id) {
  return String(id || '').replace(/^sha256:/, '').slice(0, 12) || '—';
}

/** createdText 把 Unix 秒转成本地时间；字段缺失时给占位而不是 "Invalid Date"。 */
function createdText(sec) {
  const n = Number(sec);
  if (!Number.isFinite(n) || n <= 0) return '—';
  return new Date(n * 1000).toLocaleString();
}

// 是否包含中间层镜像（all=1）。放在模块级而不是 renderImages 内部，
// 是因为 ctx.refresh() 会重新执行 renderImages：若状态是函数局部的，
// 用户打开"显示中间层"后每删一个悬空层，列表就会跳回不含中间层的视图，
// 想连着清理几个就得反复拨开关。默认 false（悬空层对日常操作是噪音，
// 排查"磁盘被谁吃了"时才需要看）。
let showAll = false;

export async function renderImages(container, ctx) {
  // 拉取可能持续数分钟，用户很可能中途切到别的分区甚至离开 Docker 页。
  // 切页时 docker.js 会执行这里登记的清理函数：迟到的回调看到 disposed
  // 就不该再往"已经换掉的页面"上弹 toast、触发整页刷新 —— 否则会出现
  // 一个和当前页面完全无关的提示，或让已被丢弃的 DOM 树反复重建。
  let disposed = false;
  ctx.trackCleanup(() => { disposed = true; });

  clear(container);

  const pullInput = h('input.input', {
    placeholder: 'nginx:1.27',
    style: { flex: '1', minWidth: '240px' },
    // 回车即拉取：这个框里只会填镜像名，逼用户再去点一次按钮没有意义。
    onkeydown: (e) => { if (e.key === 'Enter') doPull(); },
  });
  const pullBtn = h('button.btn.btn-primary', { text: '拉取', onclick: doPull });
  const pruneBtn = h('button.btn.btn-sm', {
    text: '🧹 清理悬空镜像',
    title: '只删除没有标签、也没有被引用的中间层',
    onclick: doPrune,
  });
  // 用 onchange（而不是 addEventListener）：h() 的 on* 约定在别处也这么用，
  // 统一写法能避免"这个元素后来被替换了但监听器还挂着"的排查成本。
  const allToggle = h('input', {
    type: 'checkbox',
    checked: showAll, // 与模块级状态保持一致，刷新后开关不会"自己弹回去"
    onchange: () => { showAll = allToggle.checked; load(); },
  });

  const listCount = h('span.pill', { text: '—' });
  const listBox = h('div');
  // netNotice：拉取失败且判据是网络问题时，在这里显示醒目块（原文收在块里）。
  const netNotice = h('div');
  if (netFailure) netNotice.append(networkHintBlock(netFailure));

  container.append(
    netNotice,
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '拉取 / 清理' }),
        h('div.spacer'),
        pruneBtn,
      ]),
      h('div.card-body', [
        h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
          pullInput,
          pullBtn,
          h('label', {
            style: {
              display: 'flex', gap: '6px', alignItems: 'center',
              fontSize: '13px', color: 'var(--text-dim)', cursor: 'pointer', paddingLeft: '4px',
            },
          }, [allToggle, h('span', { text: '显示中间层镜像' })]),
        ]),
        h('div.hint', {
          text: '拉取过程（每个镜像层的下载/解压进度）在「任务中心」里逐行实时可见，'
            + '关掉窗口不会中断；完成后列表会自动刷新。'
            + '中间层镜像即 <none>:<none> 的悬空层，默认隐藏。',
        }),
      ]),
    ]),
    h('div.card', [
      h('div.card-head', [h('h3', { text: '镜像列表' }), h('div.spacer'), listCount]),
      h('div.card-body.tight', [listBox]),
    ]),
  );

  /** load 重新拉取列表。每次都按当前 showAll 查询，避免开关与数据不一致。 */
  async function load() {
    clear(listBox);
    listCount.textContent = '读取中…';
    listBox.append(h('div.empty', [
      h('div.big', { text: '⏳' }),
      h('p', { text: '正在读取镜像…' }),
    ]));

    let data;
    try {
      data = await api.dockerImages(showAll);
    } catch (e) {
      if (disposed) return;
      listCount.textContent = '读取失败';
      clear(listBox);
      listBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取镜像列表失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }
    if (disposed) return;
    renderList((data && data.list) || []);
  }

  function renderList(list) {
    clear(listBox);
    listCount.textContent = `${list.length} 个`;
    listCount.title = showAll ? '包含中间层镜像' : '仅顶层镜像（不含悬空中间层）';

    if (!list.length) {
      listBox.append(h('div.empty', [
        h('div.big', { text: '💿' }),
        h('h4', { text: showAll ? '这台机器上没有任何镜像' : '还没有镜像' }),
        h('p', {
          text: showAll
            ? '连中间层都没有，说明 Docker 还没用过。'
            : '在上方输入镜像名后点「拉取」，例如 nginx:1.27、redis:7-alpine。',
        }),
      ]));
      return;
    }

    listBox.append(h('table.table', [
      h('thead', [h('tr', [
        h('th', { text: '镜像' }),
        h('th', { text: 'ID' }),
        h('th', { text: '大小' }),
        h('th', { text: '创建时间' }),
        h('th', { text: '操作' }),
      ])]),
      h('tbody', list.map(imageRow)),
    ]));
  }

  function imageRow(img) {
    const tags = tagsOf(img);
    const dangling = tags.length === 0;
    const used = Math.max(0, Number(img.Containers) || 0);

    // 镜像列：一个镜像可能挂多个 tag，横向排开且允许换行。
    // 悬空层必须显式标成 <none>，否则用户会以为面板把名字显示丢了。
    const tagCell = dangling
      ? h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, [
        h('span.pill.warn', {
          text: '<none>',
          title: '悬空镜像：没有任何标签，通常是构建/拉取留下的中间层，可用「清理悬空镜像」释放空间',
        }),
      ])
      : h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } },
        tags.map((t) => h('span.pill.brand', { text: t, title: t })));

    // 已经被容器使用的镜像提示出来：用户就不会疑惑"为什么删除失败了"。
    if (used > 0) {
      tagCell.append(h('span.pill.ok', {
        text: `${used} 个容器在用`,
        title: '有容器基于该镜像创建，直接删除会被 Docker 拒绝',
      }));
    }

    const size = Math.max(0, Number(img.Size) || 0);
    const shared = Math.max(0, Number(img.SharedSize) || 0);
    const sizeCell = h('td', [h('span', { text: bytes(size) })]);
    // SharedSize > 0 时单看 Size 会高估真实占用，把共享量放进 title 解释清楚。
    if (shared > 0) sizeCell.title = `其中 ${bytes(shared)} 与其它镜像共享层，磁盘实际新增占用更少`;

    // delBtn 在自身的 onclick 里被引用，但回调要等点击时才执行（那时已初始化），
    // 所以这里不存在时序问题，也不需要额外的查询选择器。
    const delBtn = h('button.btn.btn-danger.btn-sm', {
      text: '删除',
      onclick: () => removeImage(img, delBtn),
    });

    return h('tr', [
      h('td', [tagCell]),
      h('td', [h('code', { text: shortId(img.Id), title: img.Id })]),
      sizeCell,
      h('td', { text: createdText(img.Created) }),
      h('td', [delBtn]),
    ]);
  }

  async function doPull() {
    const image = pullInput.value.trim();
    if (!image) {
      toast('请先输入镜像名，例如 nginx:1.27', 'warn');
      pullInput.focus();
      return;
    }
    // 拉取走任务中心：后端立刻返回 202 + task_id，镜像层的下载/解压进度逐行进任务日志，
    // 关掉窗口也不会中断（任务不挂在 HTTP 请求的 ctx 上）。
    // 这里**不能** await 完再弹"成功"——那样整个拉取期间界面没有任何真实进展可看。
    taskCenter.start({
      kind: 'docker-image-pull',
      target: 'docker:image:' + image,
      title: '拉取镜像 ' + image,
      start: () => api.dockerImagePull(image),
      onDone: (m) => {
        if (disposed) return;
        if (m && m.status === 'succeeded') {
          netFailure = ''; // 这次成功了，把上一次的网络提示收掉
          const msg = (m.result && m.result.message) ? `已拉取 ${image} · ${m.result.message}` : `已拉取 ${image}`;
          toast(msg, 'ok', 8000);
          pullInput.value = '';
        } else if (m && m.status && m.status !== 'succeeded') {
          // 拉取是联网动作：网络类失败在本分区顶部给醒目块（非网络失败一个字都不加）。
          const msg = m.error || m.status;
          if (isNetworkFailureText(msg)) netFailure = msg;
        }
        ctx.refresh();
      },
    });
  }

  // pruning 是 confirmBox 期间的重入锁：确认框是异步的，没有它会弹出多个框。
  let pruning = false;

  async function doPrune() {
    if (pruning) return;
    pruning = true;
    try {
      const ok = await confirmBox(
        '将清理「悬空镜像」（dangling）：没有任何标签、也没有被其它镜像引用的中间层。\n\n'
        + '带标签的镜像和正在被容器使用的镜像不会被删，所以这一步不会影响线上容器，'
        + '清理后这些层占用的磁盘空间会被释放。\n\n继续？',
        { title: '清理悬空镜像', danger: true, okText: '清理' },
      );
      if (!ok) return;

      pruneBtn.disabled = true;
      const r = await api.dockerImagePrune();
      if (disposed) return;
      // reclaimed 是字节数，直接用 bytes() 转人类可读；为 0 不是错误，
      // 只是没有可清理的东西，用 info 而不是"成功"来提示更准确。
      const n = Math.max(0, Number((r && r.reclaimed) || 0));
      if (n > 0) toast(`已清理悬空镜像，释放 ${bytes(n)}`, 'ok');
      else toast('没有可清理的悬空镜像', 'info');
      ctx.refresh();
    } catch (e) {
      if (!disposed) toast(e.message, 'err', 9000);
    } finally {
      pruning = false;
      pruneBtn.disabled = false;
    }
  }

  async function removeImage(img, btn) {
    const tags = tagsOf(img);
    const dangling = tags.length === 0;
    // 有标签时用第一个 tag 作为 ref：docker rmi <tag> 只摘掉这个标签，
    // 比按 ID 删更保守（按 ID 删多标签镜像会被 Docker 直接拒绝，需要 force，
    // 而 force 是我们要尽量避开的）。悬空层没有 tag，只能按 ID 删。
    const ref = dangling ? img.Id : tags[0];

    const consequence = dangling
      ? '这是没有标签的悬空层，删除后无法按名称找回，只能重新构建或拉取。'
      : (tags.length > 1
        ? `本次只移除标签 ${ref}；该镜像还有另外 ${tags.length - 1} 个标签，镜像层会被保留。`
        : `镜像 ${ref} 的本地层会被删除，之后要用需要重新拉取。`);

    const ok = await confirmBox(
      `确定删除镜像 ${ref} 吗？\n\n${consequence}\n\n`
      + '如果有容器正在使用它，Docker 会拒绝这次删除（不会误删），届时你可以再决定要不要强制删除。',
      { title: '删除镜像', danger: true, okText: '删除' },
    );
    if (!ok) return;

    btn.disabled = true;
    try {
      // 安全默认：始终先 force=false。只有用户在下一个确认框里明确选了
      // 「强制删除」才会传 force=true（见下面的分支）。
      const r = await api.dockerImageRemove(ref, false);
      if (disposed) return;
      const msg = (r && r.message) ? `已删除 ${ref} · ${r.message}` : `已删除 ${ref}`;
      toast(msg, 'ok');
      ctx.refresh();
    } catch (e) {
      if (disposed) return;
      btn.disabled = false;

      if (!IN_USE_RE.test(String(e.message || ''))) {
        toast(e.message, 'err', 12000);
        return;
      }

      // 命中"被占用 / 有子镜像"：这是 Docker 的保护，不是面板的 bug。
      // 绝不在这里自动加 force —— 必须让用户看懂后果后自己选，才可能强删。
      const force = await confirmBox(
        `删除失败：${e.message}\n\n`
        + '这通常说明还有容器在使用这个镜像，或者它被其它镜像依赖。\n\n'
        + '强制删除会绕过 Docker 的保护：使用它的容器底层文件系统层会缺失，'
        + '正在运行的容器可能出现读写异常、启动失败。\n\n'
        + '更稳妥的做法是先到「容器」分区停掉并删除相关容器，再回来删除镜像。\n\n'
        + `仍要强制删除 ${ref} 吗？`,
        { title: '强制删除镜像', danger: true, okText: '强制删除' },
      );
      if (!force) {
        toast('已取消，镜像未被删除', 'info');
        return;
      }

      btn.disabled = true;
      try {
        const r2 = await api.dockerImageRemove(ref, true);
        if (disposed) return;
        const msg2 = (r2 && r2.message) ? `已强制删除 ${ref} · ${r2.message}` : `已强制删除 ${ref}`;
        toast(msg2, 'ok');
        ctx.refresh();
      } catch (e2) {
        btn.disabled = false;
        // 强删都失败的信息（依赖链、只读层）很长且需要用户细看，给足时间。
        if (!disposed) toast(e2.message, 'err', 15000);
      }
    }
  }

  await load();
}
