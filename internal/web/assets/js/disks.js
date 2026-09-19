// disks.js —— 「外接磁盘」页：只显示外置磁盘，只有危险操作（抹盘/格式化/建卷/删卷/重命名）。
//
// 2026-09-19 按用户要求重做（原版被批"看着就头痛"）：
//   · **只显示外置磁盘**（后端 `GET /system/disks` 默认就是 `diskutil list -plist external`）；
//   · **不再有**挂载/卸载/开机自动挂载按钮 —— 外接盘 macOS 本来就会自动挂载，
//     用户在这页只做四件事：抹盘 / 格式化 / 建卷 / 删卷 / 重命名；
//   · 危险操作区**默认展开**（原来要点开），页面上只剩一句警告；
//   · 其它说明（隐私保护授权、文件系统取舍）收进折叠项，不再堆在首屏。
//
// 纪律（不变）：
//   1. 只显示运行体的真实结论；读不到写"未复核 + 原因"，不显示空白、不假装成功。
//   2. 危险操作必须强确认（手输设备标识 + 勾选警告）；后端执行前后都会重新枚举校验（TOCTOU）。
//   3. 系统盘不在这个页面出现（后端已经过滤），但后端写操作仍按全量枚举做系统盘保护。

import { api, apiURL } from './api.js';
import { taskCenter } from './tasks.js';
import { h, clear, toast, modal, bytes, appendAll } from './ui.js';

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
        h('h3', { text: '外接磁盘' }),
        h('div.spacer'),
        h('span.sub', { text: '只显示外置磁盘；只有危险操作' }),
        refreshBtn,
      ]),
      status,
      resultBox,
    ]),
    listBox,
  );

  // loading 状态放在卡片头旁边，不在正文里塞一大段"读取中…"——
  // 刷新时**保留旧内容**（避免整页闪白），只有第一次才显示骨架。
  let firstLoad = true;
  load(false);

  async function load(showToast) {
    refreshBtn.disabled = true;
    refreshBtn.textContent = '读取中…';
    if (firstLoad) {
      clear(status);
      status.append(h('div.hint', { text: '正在读取外置磁盘…（首次约 0.5 秒；15 秒内再打开直接用缓存）' }));
    }
    let data;
    try {
      // 刷新走 ?fresh=1：绕过 15 秒缓存，拿真实状态（面板纪律：看真实状态）。
      data = await api.get(apiURL('system/disks' + (showToast ? '?fresh=1' : '')));
    } catch (e) {
      firstLoad = false;
      refreshBtn.disabled = false;
      refreshBtn.textContent = '⟳ 刷新';
      clear(status);
      status.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('span.pill.danger', { text: '磁盘列表读取失败' }),
        h('span.sub', { text: e && e.message ? e.message : String(e) }),
      ]));
      return;
    }
    snapshot = data || {};
    firstLoad = false;
    refreshBtn.disabled = false;
    refreshBtn.textContent = '⟳ 刷新';
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
    // 首屏只留一句：用户明确要求"只留 危险操作 抹盘/格式化/删卷会永久销毁数据"。
    status.append(h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
      h('span.pill.danger', { text: '危险操作' }),
      h('span.hint', { text: ' 抹盘 / 格式化 / 删卷会永久销毁数据。' }),
    ]));

    // 其余说明收进折叠项（默认收起）：想看的人点开，不占首屏。
    const det = h('details', { style: { marginTop: '8px' } });
    det.append(h('summary', { text: '说明（外接盘读不到 / 文件系统怎么选 / 面板权限）' }));
    const inner = h('div', { style: { marginTop: '6px' } });

    // ① 读不到外接盘 = macOS 隐私保护，两条授权路径。
    inner.append(h('div', [
      h('strong', { text: '文件管理读不到这块盘？' }),
      h('div.hint', { text: 'macOS 隐私保护（可移除宗卷）拦住了面板。放行两条路：'
        + '① 系统弹窗问「想要访问可移除宗卷上的文件」时点「允许」；'
        + '② 系统设置 → 隐私与安全性 → 完全磁盘访问权限 → 把 ' + panelBin()
        + ' 打开（这块盘还要给站点/镜像站用，就把 nginx 也打开）。'
        + '面板已用固定证书签名，所以授权一次之后升级不用再授。' }),
    ]));

    // ② 文件系统取舍（做镜像盘/备份盘时才有用）。
    const fss = data.filesystems || [];
    if (fss.length) {
      const ul = h('ul.zp-notes-list');
      fss.forEach((f) => ul.append(h('li', [
        h('strong', { text: f.label }),
        h('span.hint', { text: ' — ' + (f.note || '') }),
      ])));
      inner.append(h('div', { style: { marginTop: '6px' } }, [h('strong', { text: '格式化时文件系统怎么选' }), ul]));
    }

    // ③ 面板权限：非 root 时写操作会如实失败（不谎报）。
    if (!data.is_root) {
      inner.append(h('div', { style: { marginTop: '6px' } }, [
        h('span.pill.warn', { text: '面板非 root' }),
        h('span.hint', { text: ' 抹盘/格式化/建卷/删卷会因权限失败并如实报错；正式面板以 root 运行时可执行。' }),
      ]));
    }
    det.append(inner);
    status.append(det);
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
      h('h3', { text: `💾 ${d.id}${d.virtual ? ' · APFS 容器' : (d.volume_name ? ' · ' + d.volume_name : '')}` }),
      h('div.spacer'),
      h('span.sub', { text: [d.model, d.bus_protocol, bytes(d.size_bytes)].filter(Boolean).join(' · ') }),
    ];

    const body = h('div.card-body');
    // 标记只留必要的：原来把"整盘/可移除/加密未复核/SMART未复核"全糊上去，用户说看着头痛。
    const marks = [h('span.pill', { text: '可移除/外接' })];
    if (d.encrypted === true) marks.push(h('span.pill.warn', { text: '已加密' }));
    if (d.smart_status === 'failing') marks.push(h('span.pill.danger', { text: 'SMART 异常' }));
    body.append(h('div', { style: { display: 'flex', gap: '6px', flexWrap: 'wrap', alignItems: 'center' } }, marks));
    if (d.info_error) {
      body.append(h('div', { style: { marginTop: '8px' } }, [
        h('span.pill.danger', { text: '未复核' }), h('span.hint', { text: ' ' + d.info_error }),
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
            h('th', { text: '容量' }), h('th', { text: '挂载点' }), h('th', { text: '说明' }),
          ])]),
          tbody,
        ]),
      ]));
    } else {
      body.append(h('div.hint', { text: '这台设备没有分区/卷。' }));
    }

    // 容器提示：APFS 容器**不直接挂载**，卷建在它里面 —— 用户被"没有可挂载的文件系统"绕晕过，
    // 这里直接用一句话说清楚它是什么、以及为什么不需要对它做任何挂载操作。
    const containers = parts.filter((p) => p.content === 'Apple_APFS' && !p.has_filesystem);
    containers.forEach((c) => {
      const names = (g.init && g.init.container_ref === c.id && g.init.volume_names) ? g.init.volume_names : [];
      body.append(h('div.hint', { style: { marginTop: '8px' } }, [
        h('span.pill', { text: 'APFS 容器' }),
        h('span', { text: ' ' + c.id + ' 是 APFS 容器（不是卷，不直接挂载）：卷建在它里面'
          + (names.length ? '，目前有 ' + names.join('、') : '') + ' —— 文件管理访问的是里面的卷，不是它本身。' }),
      ]));
    });

    body.append(dangerZone(g, d));
    return h('div.card.disk-card', [h('div.card-head', head), body]);
  }

  function partRow(p) {
    const isContainer = p.content === 'Apple_APFS' && !p.has_filesystem;
    const mountCell = p.mounted && p.mount_point ? h('span.mono', { text: p.mount_point }) : h('span.hint', { text: '未挂载' });
    const note = isContainer
      ? h('span.hint', { text: 'APFS 容器（不直接挂载）' })
      : (p.apfs_volume ? h('span.hint', { text: 'APFS 卷' }) : h('span.hint', { text: '' }));
    return h('tr', [
      h('td', [h('div.mono', { text: p.id })]),
      h('td', { text: p.volume_name || '—' }),
      h('td', { text: p.filesystem || p.content || '—' }),
      h('td.mono', { text: bytes(p.size_bytes) }),
      h('td', [mountCell]),
      h('td', [note]),
    ]);
  }

  // dangerZone：危险操作区 —— **默认展开**（用户要求：不要再点一下才看到）。
  // 这一页只做这些事，所以默认展开才是正常的；警告靠一句文字 + 按钮本身的红色。
  function dangerZone(g, d) {
    const sys = !!d.system_disk;
    const reason = d.protect_reason || '系统盘';
    const wrap = h('div.disk-danger-area');
    wrap.append(h('div.disk-danger-head', { text: '危险操作（会永久销毁数据）' }));
    const body = h('div.disk-danger-body');

    body.append(h('div', { style: { marginTop: '4px', display: 'flex', gap: '8px', flexWrap: 'wrap', alignItems: 'center' } }, [
      h('button.btn.btn-sm.btn-danger', {
        text: d.whole_disk ? '格式化 / 抹盘这块盘…' : '格式化这个分区/卷…',
        title: sys ? '系统盘，禁用：' + reason : '会永久抹掉这块设备上的一切数据',
        disabled: sys,
        onclick: () => openFormatDialog(g, d),
      }),
      h('span.hint', { text: '整盘抹掉会重建分区表；格式化某个卷只重建该卷。' }),
    ]));

    // 只在**容器自己**那一组显示"新建 APFS 卷"：
    // 容器的卷是建在容器里的，物理盘那一组（disk9）不该再出现同一个按钮（用户看到两个"新建 APFS 卷"）。
    if (g.init && g.init.container_ref === d.id) {
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

    // 只列"用户真正会操作的卷"：
    //   · 排除系统盘设备（后端已过滤，这里再保险一次）；
    //   · 排除 EFI 分区（外接盘的 EFI 分区格式化只会毁掉可引导性，磁盘工具里做更合适）；
    //   · 排除 APFS 容器本身（它不是卷；对它要做的"新建卷"在上一行的按钮里）。
    const rows = (g.partitions || []).filter((p) => !p.system_disk
      && p.content !== 'EFI'
      && !(p.content === 'Apple_APFS' && !p.has_filesystem));
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
