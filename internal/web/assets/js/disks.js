// disks.js —— 「磁盘」页：看磁盘信息 + 挂载/卸载 + 开机自动挂载 + 危险操作。
//
// 页面结构刻意分成**两区**，视觉上就让人分清"安全"与"会毁数据"：
//   · 安全操作：挂载 / 卸载 / 开机自动挂载（每行三个按钮，二次确认）；
//   · 危险操作：抹盘 / 格式化 / 建卷 / 删卷 / 重命名 —— 单独放进**默认折叠**的
//     <details> 里，每个都要"手输设备标识 + 勾选警告 + 看清受影响清单"才允许点。
//
// 纪律：
//   1. 只显示运行体的真实结论；读不到写"未复核 + 原因"，不显示空白、不假装成功。
//   2. **系统盘/受保护设备所有写操作按钮禁用并写明原因**（后端还会再 403）。
//   3. 危险操作必须强确认；后端执行前还会重新枚举校验（TOCTOU，见 api_disks_ops.go）。
//
// 文件系统取舍（与后端 diskFilesystems 同一份文案，来自 GET /system/disks）：
//   exFAT 全平台读写、适合插拔备份，但无日志；APFS macOS 原生最快、别家读不了；
//   HFS+ 旧；FAT32 单文件 ≤ 4 GiB，装不下镜像包。

import { api, apiURL } from './api.js';
import { taskCenter } from './tasks.js';
import { h, clear, toast, confirmBox, modal, bytes, appendAll } from './ui.js';

export function DisksView(content, ctx = {}) {
  clear(content);
  const status = h('div.card-body');
  const listBox = h('div');
  // 危险操作走任务中心：完成后在这里留一块**持久**的结果摘要（含回读），
  // 而不是一个转瞬即逝的 toast。progress modal 里还能看到完整日志。
  const resultBox = h('div');
  const refreshBtn = h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: () => load(true) });
  let snapshot = null;

  appendAll(content,
    h('div.card', [
      h('div.card-head', [
        h('h3', { text: '磁盘工具' }),
        h('div.spacer'),
        h('span.sub', { text: '查看 / 挂载 / 卸载 / 开机自动挂载 / 抹盘 / 格式化 / 建卷 / 删卷 / 重命名' }),
        refreshBtn,
      ]),
      status,
      resultBox,
    ]),
    listBox,
  );

  load(false);

  async function load(showToast) {
    clear(status);
    status.append(h('div.hint', { text: '读取中…（diskutil list + info + apfs list）' }));
    clear(listBox);
    let data;
    try {
      data = await api.get(apiURL('system/disks'));
    } catch (e) {
      clear(status);
      status.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.danger', { text: '磁盘列表读取失败' }),
        h('span.sub', { text: e && e.message ? e.message : String(e) }),
      ]));
      return;
    }
    snapshot = data || {};
    renderStatus(snapshot);
    renderList(snapshot);
    if (showToast) toast('已刷新磁盘状态', 'ok');
  }

  // ---------- 危险操作：任务中心 ----------
  //
  // 后端把这些动作改成**任务中心任务**（202 + task_id）：
  //   · 提交后立刻能在任务中心看到进度/日志/命令，关掉窗口任务照样跑；
  //   · 任务结束回调 onDone：刷新磁盘列表 + 展示回读结果摘要。
  // taskCenter.start 已经负责"提交失败要说话"（4xx/5xx 会 toast 原因，返回 null），
  // 所以这里**绝不**把提交失败当成功。
  function submitDiskTask(kind, title, deviceID, pathSuffix, body) {
    return taskCenter.start({
      kind,
      target: deviceID, // 同一设备同时只允许一个危险任务（后端也会 409）
      title,
      start: () => api.post(apiURL(`system/disks/${encodeURIComponent(deviceID)}/${pathSuffix}`), body),
      onDone: afterDiskTask,
    });
  }

  function afterDiskTask(m) {
    load(false); // 任务里已经回读并落进结果；列表再按运行体真实状态刷一次
    renderTaskResult(m);
  }

  // renderTaskResult 展示"完成后回读"的结果摘要（卷名/容量/挂载/容器剩余空间）。
  // 结果来自任务 result（后端 diskTaskResult 的 message + steps）。
  function renderTaskResult(m) {
    clear(resultBox);
    if (!m) return;
    const done = m.status === 'succeeded';
    const r = m.result || {};
    const steps = Array.isArray(r.steps) ? r.steps : [];
    const msg = r.message || m.error || '';
    const wrap = h('div.disk-task-result', [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill' + (done ? '.ok' : '.danger'), { text: done ? '磁盘任务完成' : '磁盘任务未完成' }),
        h('span.sub', { text: m.title || '' }),
        h('button.btn.btn-sm', { text: '查看任务日志', onclick: () => taskCenter.openTask(m.id) }),
      ]),
      msg ? h('div', { style: { marginTop: '6px' } }, [h('strong', { text: msg })]) : null,
      steps.length ? (() => {
        const ul = h('ul.zp-notes-list');
        steps.forEach((s) => ul.append(h('li.mono', { text: s })));
        return ul;
      })() : null,
    ]);
    resultBox.append(wrap);
    if (done && msg) toast(msg, 'ok', 10000);
    else if (!done) toast('磁盘任务失败：' + (m.error || '未知原因'), 'err', 12000);
  }

  function renderStatus(data) {
    clear(status);
    status.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('span', { class: data.is_root ? 'pill ok' : 'pill warn', text: data.is_root ? '面板以 root 运行' : '面板非 root' }),
      h('span.sub', { text: data.is_root
        ? '磁盘操作直接执行（root 下 diskutil 不需要任何 GUI 授权弹窗）。'
        : '挂载/卸载可能可用；抹盘/格式化/建卷/删卷/写 fstab 通常会因权限失败并如实报错 —— 正式面板以 root 运行时可执行。' }),
    ]));
    status.append(h('div.hint.mono', { text: 'fstab：' + (data.fstab_path || '/etc/fstab')
      + (data.fstab_readable ? '' : '（读不到：' + (data.fstab_error || '未知原因') + '）') }));
    (data.notes || []).forEach((n) => status.append(h('div.hint', { text: '· ' + n })));

    // 外接盘/卷的静态提示：macOS 隐私保护会挡住外接盘，**只能人工授权**（实测过），
    // 静态写在页面上，不让用户先在文件管理里撞一次 EPERM 才知道。
    status.append(h('div', { style: { marginTop: '8px' } }, [
      h('span.pill.warn', { text: '外接盘/卷' }),
      h('span.hint', { text: ' 文件管理读写外接盘（可移除宗卷）会被 macOS 隐私保护拒绝（operation not permitted）：'
        + '面板是 root 后台进程、没有用户会话，系统连询问窗口都不会弹。放行只能人工授权一次 —— '
        + '系统设置 → 隐私与安全性 → 完全磁盘访问权限 → 点「+」选中 ' + panelBin()
        + '（这块盘还要给镜像站/站点用，就把 nginx 二进制也加上）→ 打开开关 → 重启面板。'
        + '换挂载点不能绕过（已实测：挂载本身成功，读它仍被拒）；升级面板后可能要再授权一次。' }),
    ]));
    status.append(h('div', { style: { marginTop: '8px' } }, [
      h('span.pill.warn', { text: '危险操作' }),
      h('span.hint', { text: ' 抹盘/格式化/删卷会永久销毁数据；系统盘相关设备已禁用全部写操作。' }),
    ]));

    // 文件系统取舍直接列在页面上（不只藏在格式化弹窗里）——
    // 用户做镜像盘之前就该看到"exFAT 全平台/FAT32 单文件 4GiB"这类代价。
    const fss = data.filesystems || [];
    if (fss.length) {
      const ul = h('ul.zp-notes-list');
      fss.forEach((f) => ul.append(h('li', [
        h('strong', { text: f.label }),
        h('span.hint', { text: ' — ' + (f.note || '') }),
      ])));
      const det = h('details');
      det.append(h('summary', { text: '文件系统取舍（抹盘/格式化时怎么选）' }));
      det.append(ul);
      status.append(det);
    }
  }

  function renderList(data) {
    clear(listBox);
    const groups = data.list || [];
    if (!groups.length) {
      listBox.append(h('div.card', [h('div.card-body', [h('div.hint', { text: '没有读到任何磁盘。' })])]));
      return;
    }
    groups.forEach((g) => listBox.append(diskCard(g)));
  }

  function diskCard(g) {
    const d = g.disk || {};
    const head = [
      h('h3', { text: `💾 ${d.id}${d.volume_name ? ' · ' + d.volume_name : ''}` }),
      h('div.spacer'),
      h('span.sub', { text: [d.model, d.bus_protocol, bytes(d.size_bytes)].filter(Boolean).join(' · ') }),
    ];

    const body = h('div.card-body');
    const marks = [h('span.pill', { text: d.whole_disk ? '整盘' : '分区/卷' })];
    if (d.internal) marks.push(h('span.pill.warn', { text: '内置' }));
    if (d.removable || d.external) marks.push(h('span.pill', { text: '可移除/外接' }));
    marks.push(encMark(d), smartMark(d));
    if (d.system_disk) marks.push(h('span.pill.danger', { text: '系统盘' }));
    body.append(h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center' } }, marks));
    if (d.info_error) {
      body.append(h('div', { style: { marginTop: '8px' } }, [
        h('span.pill.danger', { text: '未复核' }), h('span.hint', { text: ' ' + d.info_error }),
      ]));
    }
    if (d.system_disk) {
      body.append(h('div', { style: { marginTop: '8px' } }, [
        h('span.pill.danger', { text: '⛔ 系统盘，已禁用全部写操作' }),
        h('span.hint', { text: ' ' + (d.protect_reason || '') }),
      ]));
    }

    const parts = g.partitions || [];
    if (parts.length) {
      const tbody = h('tbody');
      parts.forEach((p) => tbody.append(partRow(p)));
      body.append(h('div', { style: { overflowX: 'auto' } }, [
        h('table.table', [
          h('thead', [h('tr', [
            h('th', { text: '设备' }), h('th', { text: '卷名' }), h('th', { text: '文件系统' }),
            h('th', { text: '容量' }), h('th', { text: '挂载点' }), h('th', { text: '标记' }), h('th', { text: '安全操作' }),
          ])]),
          tbody,
        ]),
      ]));
    } else {
      body.append(h('div.hint', { text: '这台设备没有分区/卷。' }));
    }

    if (d.whole_disk && (d.mountable || d.mounted)) {
      body.append(h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px', flexWrap: 'wrap' } }, safeActions(d)));
    }

    body.append(dangerZone(g, d));
    return h('div.card.disk-card', [h('div.card-head', head), body]);
  }

  function partRow(p) {
    const marks = [encMark(p)];
    if (p.external || p.removable) marks.push(h('span.pill', { text: '可移除/外接' }));
    marks.push(h('span.pill', { text: p.filesystem || p.content || '未知' }));
    if (p.auto_mount) marks.push(h('span.pill.ok', { text: '开机自动挂载' }));
    if (p.system_disk) marks.push(h('span.pill.danger', { text: '系统盘' }));
    const mountCell = p.mounted && p.mount_point ? h('span.mono', { text: p.mount_point }) : h('span.hint', { text: '未挂载' });
    return h('tr', [
      h('td', [h('div.mono', { text: p.id })]),
      h('td', { text: p.volume_name || '—' }),
      h('td', { text: p.filesystem || p.content || '—' }),
      h('td.mono', { text: bytes(p.size_bytes) }),
      h('td', [mountCell]),
      h('td', [h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, marks)]),
      h('td', [h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap' } }, safeActions(p))]),
    ]);
  }

  // safeActions 是挂载/卸载/开机自动挂载；系统盘一律 disabled 并写明原因。
  function safeActions(p) {
    const sys = !!p.system_disk;
    const reason = p.protect_reason || '系统盘';
    const out = [];
    out.push(h('button.btn.btn-sm', {
      text: '挂载',
      title: sys ? '系统盘，禁用：' + reason
        : (p.mounted ? '已经挂载了' : (p.locked ? '加密卷锁定，需人工解锁（无头环境不可用）' : '挂载这台设备')),
      disabled: sys || p.mounted || p.locked,
      onclick: () => doAction(p, 'mount'),
    }));
    out.push(h('button.btn.btn-sm', {
      text: '卸载',
      title: sys ? '系统盘，禁用：' + reason : (p.mounted ? '卸载（有程序占用时会失败并如实报错）' : '当前未挂载'),
      disabled: sys || !p.mounted,
      onclick: () => doAction(p, 'unmount'),
    }));
    let amReason = '';
    if (sys) amReason = '系统盘，禁用：' + reason;
    else if (p.whole_disk) amReason = '整盘不能设置开机自动挂载，请选某个卷';
    else if (!p.uuid) amReason = '没有卷 UUID，无法用 UUID 稳定引用';
    else if (!p.has_filesystem) amReason = '没有可挂载的文件系统（例如 APFS 容器）';
    else if (p.encrypted) amReason = '加密卷开机需人工解锁（无头环境不可用），本版拒绝为它写自动挂载';
    out.push(h('button.btn.btn-sm' + (p.auto_mount ? '.btn-primary' : ''), {
      text: p.auto_mount ? '关闭自动挂载' : '开机自动挂载',
      title: amReason || (p.auto_mount ? '删除 /etc/fstab 条目' : '写入 /etc/fstab'),
      disabled: !!amReason,
      onclick: () => doAutoMount(p, !p.auto_mount),
    }));
    if (amReason && !sys) out.push(h('span.hint', { text: amReason }));
    return out;
  }

  // dangerZone：默认折叠的危险操作区。系统盘 → 全部禁用并写明原因。
  function dangerZone(g, d) {
    const sys = !!d.system_disk;
    const reason = d.protect_reason || '系统盘';
    const wrap = h('details.disk-danger');
    const body = h('div.disk-danger-body');
    wrap.append(h('summary', { text: '⚠️ 危险操作：抹盘 / 格式化 / 建卷 / 删卷 / 重命名（点击展开；系统盘已禁用）' }));

    if (sys) {
      body.append(h('div', { style: { marginTop: '8px' } }, [
        h('span.pill.danger', { text: '⛔ 系统盘，危险操作全部禁用' }),
        h('span.hint', { text: ' ' + reason }),
      ]));
    }

    body.append(h('div', { style: { marginTop: '10px', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
      h('button.btn.btn-sm.btn-danger', {
        text: d.whole_disk ? '格式化 / 抹盘这块盘…' : '格式化这个分区/卷…',
        title: sys ? '系统盘，禁用：' + reason : '会永久抹掉这块设备上的一切数据',
        disabled: sys,
        onclick: () => openFormatDialog(g, d),
      }),
      sys ? h('span.hint', { text: '禁用：' + reason })
        : h('span.hint', { text: '整盘抹掉会重建分区表；分区/卷抹掉只重建该卷。' }),
    ]));

    if (g.init) {
      const init = g.init;
      body.append(h('div', { style: { marginTop: '8px', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
        h('button.btn.btn-sm.btn-danger', {
          text: '新建 APFS 卷…',
          title: init.eligible ? '在容器里再建一个卷（不动已有卷）' : '不可用：' + init.reason,
          disabled: !init.eligible,
          onclick: () => openCreateVolumeDialog(d, init),
        }),
        init.container_ref ? h('span.hint.mono', { text: '容器 ' + init.container_ref
          + '　已有卷 ' + init.volume_count + ' 个' + (init.volume_names && init.volume_names.length ? '（' + init.volume_names.join('、') + '）' : '')
          + '　可用 ' + bytes(init.container_free) }) : null,
        init.eligible ? null : h('span.hint', { text: '不可用：' + (init.reason || '未知原因') }),
      ]));
    }

    const rows = (g.partitions || []).filter((p) => !p.system_disk);
    if (rows.length) {
      const tbody = h('tbody');
      rows.forEach((p) => {
        const canDel = !!p.apfs_volume;
        tbody.append(h('tr', [
          h('td', [h('div.mono', { text: p.id }), h('div.hint', { text: p.volume_name || '—' })]),
          h('td', { text: p.filesystem || p.content || '—' }),
          h('td', [
            h('button.btn.btn-sm', {
              text: '格式化…', title: sys ? '系统盘，禁用' : '会永久抹掉这个卷的数据',
              disabled: sys, onclick: () => openFormatDialog(g, p),
            }),
            ' ',
            h('button.btn.btn-sm', {
              text: '重命名…', title: sys ? '系统盘，禁用' : '只改卷名，不动数据',
              disabled: sys || !p.volume_name, onclick: () => openRenameVolumeDialog(p),
            }),
            ' ',
            h('button.btn.btn-sm.btn-danger', {
              text: '删除卷…', title: sys ? '系统盘，禁用' : (canDel ? '删除这个 APFS 卷及其数据' : '只有 APFS 卷可删'),
              disabled: sys || !canDel, onclick: () => openDeleteVolumeDialog(p),
            }),
          ]),
        ]));
      });
      body.append(h('div', { style: { marginTop: '10px', overflowX: 'auto' } }, [
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '设备/卷' }), h('th', { text: '文件系统' }), h('th', { text: '危险操作' })])]),
          tbody,
        ]),
      ]));
    }
    wrap.append(body);
    return wrap;
  }

  function encMark(d) {
    if (!d.encrypted_known) return h('span.pill', { text: '加密：未复核', title: 'diskutil 未返回加密字段' });
    return d.encrypted ? h('span.pill.warn', { text: '已加密' }) : h('span.pill.ok', { text: '未加密' });
  }

  function smartMark(d) {
    const map = {
      verified: ['ok', 'SMART：正常'], failing: ['danger', 'SMART：异常'],
      not_supported: ['warn', 'SMART：不支持'], unknown: ['warn', 'SMART：未复核'],
    };
    const [cls, label] = map[d.smart_status] || ['warn', 'SMART：未复核'];
    return h('span.pill.' + cls, { text: label, title: d.smart_note || '' });
  }

  // ---------- 安全动作 ----------

  // panelBin 是面板自己的可执行文件路径（由 /system/disks 下发，非默认安装也对）。
  //
  // 用途只有一个：外接盘被 macOS 隐私保护拒绝时，告诉用户**去系统设置里给谁授权**。
  function panelBin() {
    return (snapshot && snapshot.panel_binary) || '/opt/zizpanel/bin/zizpanel';
  }

  // 注：「挂载到自定义挂载点…」入口已删除（2026-09-19）。
  // 它当初是作为"绕开 macOS 对外接盘的隐私保护"的解法加进来的，实测证伪：
  // 挂到 /Volumes 之外后挂载成功，但面板读那个路径仍然 operation not permitted
  // （TCC 按**卷**判定，与挂载点无关）。留着一个做不到的按钮比没有更糟，
  // 所以入口去掉；后端 `mount_point` 能力保留（API 可用，见 api_disks.go）。

  // renderMountResult 把结果（含**回读**挂载点）留在页面上，不只是一闪而过的 toast。
  function renderMountResult(p, r) {
    if (!r) return;
    clear(resultBox);
    resultBox.append(h('div.disk-task-result', [
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill' + (r.verified && r.mounted ? '.ok' : '.danger'),
          { text: r.verified && r.mounted ? '已挂载（回读确认）' : '未确认' }),
        h('span.sub', { text: p.id + (p.volume_name ? '（' + p.volume_name + '）' : '') }),
      ]),
      h('div.mono', { style: { marginTop: '6px' }, text: r.mount_point ? ('挂载点：' + r.mount_point) : (r.message || '回读没有给出挂载点') }),
      h('div.hint', { text: '文件管理里的「位置」下拉现在应能看到它；读不到多半是 macOS 隐私保护（见上方「外接盘/卷」那行）。' }),
    ]));
  }

  async function doAction(p, action) {
    const verb = action === 'mount' ? '挂载' : '卸载';
    const yes = await confirmBox(`${verb} ${p.id}${p.volume_name ? '（' + p.volume_name + '）' : ''}？\n\n` +
      (action === 'mount' ? '会调用 diskutil mount 并回读挂载点确认。' : '会调用 diskutil unmount；被占用会失败并如实报错（不强卸）。'),
      { title: verb + '磁盘', okText: verb });
    if (!yes) return;
    try {
      const r = await api.post(apiURL(`system/disks/${encodeURIComponent(p.id)}/${action}`), {});
      toast(r && r.message ? r.message : verb + '完成', 'ok');
      // 挂载后把**回读**到的挂载点留在页面上（不只是一闪而过的 toast）：
      // 挂载点是否真的生效，只能看回读结果。
      if (action === 'mount') renderMountResult(p, r);
    } catch (e) {
      toast(e && e.message ? e.message : String(e), 'err', 9000);
    } finally { load(false); }
  }

  async function doAutoMount(p, enable) {
    const yes = await confirmBox(
      (enable
        ? `把 ${p.id}（${p.volume_name || ''}）写进 /etc/fstab，开机自动挂载？\n\n条目形如：UUID=${p.uuid} ${p.mount_point || '/Volumes/…'} ${p.fs_type || 'apfs'} rw 0 2`
        : `从 /etc/fstab 删除 ${p.id}（${p.volume_name || ''}）的自动挂载条目？`) +
      '\n\n注意：开机自动挂载是否真正生效必须重启后才能确认；本面板不做重启，也不会标成已验证。',
      { title: enable ? '设置开机自动挂载' : '关闭开机自动挂载', okText: enable ? '写入' : '删除' });
    if (!yes) return;
    try {
      const r = await api.post(apiURL(`system/disks/${encodeURIComponent(p.id)}/auto-mount`), { enabled: enable });
      toast(r && r.message ? r.message : '已保存', 'ok', 8000);
    } catch (e) {
      toast(e && e.message ? e.message : String(e), 'err', 9000);
    } finally { load(false); }
  }

  // ---------- 危险动作：共用的强确认弹窗 ----------
  //
  // 必须同时满足：手输设备标识一致 + 勾选警告 + 业务校验通过，才允许点"执行"。
  // submit 返回任务编号（提交给任务中心）或 null（提交失败，原因已 toast）。
  function strongDialog({ title, device, warning, body, command, submitLabel, validateExtra, submit }) {
    const confirmInput = h('input.input', { type: 'text', placeholder: '原样输入 ' + device.id });
    const ack = h('input', { type: 'checkbox' });
    const errBox = h('div');
    const goBtn = h('button.btn.btn-danger', { text: submitLabel, disabled: true });
    const sync = () => {
      const extra = validateExtra ? validateExtra() : true;
      goBtn.disabled = !(confirmInput.value.trim() === device.id && ack.checked && extra);
    };
    confirmInput.addEventListener('input', sync);
    ack.addEventListener('change', sync);

    const m = modal({
      title,
      body: h('div', [
        h('div.disk-warn', [h('strong', { text: '⚠️ ' }), h('span', { text: warning })]),
        command ? h('div.hint.mono', { text: '将执行：' + command }) : null,
        body,
        h('div.field', [h('label', { text: `二次确认：手动输入设备标识 「${device.id}」` }), confirmInput]),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center', marginTop: '6px' } }, [
          ack, h('span', { text: '我已知晓：这会永久销毁上述设备上的数据，且不可恢复' }),
        ]),
        errBox,
      ]),
      footer: [h('button.btn', { text: '取消', onclick: () => m.close() }), goBtn],
    });

    goBtn.addEventListener('click', async () => {
      errBox.textContent = '';
      goBtn.disabled = true; goBtn.textContent = '提交中…';
      let taskID = null;
      try {
        taskID = await submit();
      } catch (e) {
        taskID = null;
        errBox.append(h('div', { style: { marginTop: '8px' } }, [
          h('span.pill.danger', { text: '提交失败' }),
          h('span.hint', { text: ' ' + (e && e.message ? e.message : String(e)) }),
        ]));
      }
      if (taskID) {
        // 任务已在任务中心跑（进度窗自动打开）。关掉确认框，结果由 onDone 展示。
        m.close();
      } else {
        // 提交失败（参数被 400/409 拦下等）：保留输入让用户改完重试。
        goBtn.textContent = submitLabel; sync();
      }
    });
    sync();
    return m;
  }

  // affectedList 列出"这次会波及哪些卷/分区"，让用户在点之前看见。
  function affectedList(g, d) {
    const items = [];
    if (g.disk && g.disk.id === d.id) {
      (g.partitions || []).forEach((p) => items.push(`${p.id} ${p.volume_name || '(无卷名)'} ${p.filesystem || p.content || ''}`));
      if (g.init && g.init.volume_names) g.init.volume_names.forEach((n) => items.push(`容器 ${g.init.container_ref} 的卷 ${n}`));
    } else {
      items.push(`${d.id} ${d.volume_name || '(无卷名)'} ${d.filesystem || d.content || ''}`);
    }
    return items;
  }

  function openFormatDialog(g, d) {
    const fss = (snapshot && snapshot.filesystems) || [];
    const sel = h('select.input');
    fss.forEach((f) => sel.append(h('option', { value: f.key, text: f.label, selected: f.key === 'apfs' })));
    const noteBox = h('div.hint');
    const nameInput = h('input.input', { type: 'text', placeholder: '新卷名（格式化后使用）', maxlength: 32, value: d.volume_name || 'Untitled' });
    const list = h('ul.zp-notes-list');
    affectedList(g, d).forEach((x) => list.append(h('li', { text: x })));
    const cmdBox = h('div.hint.mono');

    const fsOf = () => fss.find((f) => f.key === sel.value) || { key: '', note: '', diskutil_format: '' };
    const whole = g.disk && g.disk.id === d.id;
    const syncNote = () => {
      const f = fsOf();
      clear(noteBox); noteBox.append(document.createTextNode(f.note || ''));
      const cmd = whole ? `diskutil eraseDisk ${f.diskutil_format || '<格式>'} <卷名> ${d.id}`
        : `diskutil eraseVolume ${f.diskutil_format || '<格式>'} <卷名> ${d.id}`;
      clear(cmdBox); cmdBox.append(document.createTextNode('将执行：' + cmd));
    };
    sel.addEventListener('change', syncNote);
    syncNote();

    const body = h('div', [
      h('p.hint', { text: '文件系统取舍：' }),
      h('div.field', [h('label', { text: '格式化为' }), sel, noteBox]),
      h('p.hint', { text: '将会丢失的数据（当前清单）：' }),
      list,
      h('div.field', [h('label', { text: '新卷名' }), nameInput]),
      cmdBox,
    ]);

    const m = strongDialog({
      title: '格式化 / 抹盘 · ' + d.id,
      device: d,
      warning: '这会永久抹掉 ' + d.id + ' 上的一切数据（不可恢复）。选择「保持现状」则不会执行任何格式化。',
      body,
      submitLabel: '格式化',
      validateExtra: () => fsOf().key !== 'keep' && nameInput.value.trim() !== '',
      submit: () => submitDiskTask('disk_erase', '抹盘/格式化 ' + d.id, d.id, 'erase', {
        filesystem: sel.value,
        name: nameInput.value.trim(),
        confirm: d.id,
        expect_uuid: d.uuid || '',
        expect_size: Number(d.size_bytes) || 0,
      }),
    });
    // 选择文件系统或改名都要刷新按钮可用性。
    sel.addEventListener('change', () => { const ack = m.el.querySelector('input[type=checkbox]'); if (ack) ack.dispatchEvent(new Event('change')); });
    nameInput.addEventListener('input', () => { const ack = m.el.querySelector('input[type=checkbox]'); if (ack) ack.dispatchEvent(new Event('change')); });
  }

  // refreshDialogBtn 让"业务字段变化"也能刷新强确认按钮（勾选框是 sync 的触发器）。
  function bindDialogRefresh(m, inputs) {
    inputs.forEach((el) => el.addEventListener('input', () => {
      const ack = m.el.querySelector('input[type=checkbox]');
      if (ack) ack.dispatchEvent(new Event('change'));
    }));
  }

  function openCreateVolumeDialog(d, init) {
    const nameInput = h('input.input', { type: 'text', placeholder: '例如 ZPMirror', maxlength: 32 });
    const body = h('div', [
      h('p.hint', { text: '在容器 ' + (init.container_ref || '') + ' 里新建一个 APFS 卷；不会删除分区表，也不会动已有卷（已有：'
        + ((init.volume_names || []).join('、') || '无') + '）。' }),
      h('div.field', [h('label', { text: '新卷名' }), nameInput]),
    ]);
    const m = strongDialog({
      title: '新建 APFS 卷 · ' + d.id,
      device: d,
      warning: '会在 ' + d.id + ' 上创建新的文件系统卷。请确认这块盘就是你要用的盘。',
      body,
      command: init.backend_command || '',
      submitLabel: '新建',
      validateExtra: () => nameInput.value.trim() !== '',
      submit: () => submitDiskTask('disk_volume_create', '新建 APFS 卷 ' + d.id, d.id, 'volume-create', {
        name: nameInput.value.trim(), confirm: d.id,
        expect_uuid: d.uuid || '', expect_size: Number(d.size_bytes) || 0,
      }),
    });
    bindDialogRefresh(m, [nameInput]);
  }

  function openDeleteVolumeDialog(p) {
    const body = h('div', [
      h('p.hint', { text: '将删除 APFS 卷 ' + p.id + '（' + (p.volume_name || '') + '）及其全部数据；容器的其它卷不受影响。' }),
    ]);
    strongDialog({
      title: '删除 APFS 卷 · ' + p.id,
      device: p,
      warning: '这会永久删除卷 ' + (p.volume_name || p.id) + ' 及其中的数据，不可恢复。',
      body,
      command: 'diskutil apfs deleteVolume ' + p.id,
      submitLabel: '删除卷',
      submit: () => submitDiskTask('disk_volume_delete', '删除 APFS 卷 ' + p.id, p.id, 'volume-delete', {
        confirm: p.id, expect_uuid: p.uuid || '', expect_size: Number(p.size_bytes) || 0,
      }),
    });
  }

  function openRenameVolumeDialog(p) {
    const nameInput = h('input.input', { type: 'text', placeholder: '新卷名', maxlength: 32, value: p.volume_name || '' });
    const body = h('div', [
      h('p.hint', { text: '只改卷名，不动数据。当前卷名：' + (p.volume_name || '（无）') }),
      h('div.field', [h('label', { text: '新卷名' }), nameInput]),
    ]);
    const m = strongDialog({
      title: '重命名卷 · ' + p.id,
      device: p,
      warning: '会修改卷 ' + p.id + ' 的名称（不影响数据）。',
      body,
      command: 'diskutil rename ' + p.id + ' <新卷名>',
      submitLabel: '重命名',
      validateExtra: () => nameInput.value.trim() !== '',
      submit: () => submitDiskTask('disk_volume_rename', '重命名卷 ' + p.id, p.id, 'volume-rename', {
        name: nameInput.value.trim(), confirm: p.id,
        expect_uuid: p.uuid || '', expect_size: Number(p.size_bytes) || 0,
      }),
    });
    bindDialogRefresh(m, [nameInput]);
  }
}
