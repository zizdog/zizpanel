// cron.js —— 计划任务页面。
//
// 交互重点：
//   - 计划表达式输入框实时预览（中文描述 + 接下来 5 次执行时间），
//     避免用户写完不知道什么时候会跑
//   - 常用计划做成下拉预设，减少手写 cron
//   - 任务状态以系统（launchd）为准：显示"已注册/未注册"，
//     并提供"重新注册全部任务"用于修复不同步
//   - 备份类任务有专门的表单（选范围、保留天数），不用手写命令

import { api } from './api.js';
import { h, clear, toast, modal, confirmBox, promptBox, appendAll } from './ui.js';
import { registerCleanup } from './app.js';

let cache = null;

export function CronView(content, ctx = {}) {
  clear(content);

  const head = h('div.card-head', [
    h('h3', { text: '计划任务' }),
    h('div.spacer'),
    h('div', { id: 'cron-head', style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }),
  ]);
  const body = h('div.card-body.tight');
  const backupBox = h('div');

  content.append(
    h('div.card', [head, body]),
    backupBox,
  );
  const headBox = head.querySelector('#cron-head');

  async function load() {
    clear(body);
    body.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取计划任务…' })]));
    try {
      cache = await api.cron();
    } catch (e) {
      clear(body);
      body.append(h('div.empty', [h('div.big', { text: '⚠️' }), h('h4', { text: '读取失败' }), h('p', { text: e.message })]));
      return;
    }
    renderHead();
    renderList();
    renderBackups();
  }

  function renderHead() {
    clear(headBox);
    const list = cache?.list || [];
    const enabled = list.filter((j) => j.enabled).length;
    const unloaded = list.filter((j) => j.enabled && !j.loaded).length;
    appendAll(headBox,
      h('span.pill', { text: `共 ${list.length} 个任务` }),
      h('span.pill' + (enabled ? '.ok' : ''), { text: `${enabled} 个启用` }),
      unloaded > 0 ? h('span.pill.danger', {
        text: `${unloaded} 个未注册到系统`,
        title: '数据库里是启用状态，但系统里没有对应的定时任务。点右侧「重新注册」修复。',
      }) : null,
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      h('button.btn.btn-sm', {
        text: '🔧 重新注册全部',
        title: '把数据库里的任务重新写入系统定时器（用于修复不同步）',
        onclick: async () => {
          try {
            const r = await api.cronSync();
            if (r.failed?.length) toast(`注册 ${r.applied} 个，失败 ${r.failed.length} 个：${r.failed[0]}`, 'warn', 12000);
            else toast(`已注册 ${r.applied} 个任务`, 'ok');
            load();
          } catch (e) { toast(e.message, 'err', 9000); }
        },
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 新建任务', onclick: () => jobModal(null, load) }),
    );
  }

  function renderList() {
    clear(body);
    const list = cache?.list || [];
    if (!list.length) {
      body.append(h('div.empty', [
        h('div.big', { text: '⏰' }),
        h('h4', { text: '还没有计划任务' }),
        h('p', { text: '可以创建定时备份、定时清理缓存，或定期调用某个 URL。' }),
        h('div', { style: { marginTop: '16px', display: 'flex', gap: '8px', justifyContent: 'center' } }, [
          h('button.btn.btn-primary', { text: '创建定时备份', onclick: () => quickBackup() }),
          h('button.btn', { text: '新建任务', onclick: () => jobModal(null, load) }),
        ]),
      ]));
      return;
    }

    body.append(h('div', { style: { overflowX: 'auto' } }, [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '任务' }), h('th', { text: '类型' }), h('th', { text: '计划' }),
          h('th', { text: '状态' }), h('th', { text: '上次执行' }), h('th', { text: '操作' }),
        ])]),
        h('tbody', list.map((j) => h('tr', [
          h('td', [
            h('div', { style: { fontWeight: '550' }, text: j.name }),
            h('div', { style: { fontSize: '11px', color: 'var(--text-mute)' }, text: j.label }),
          ]),
          h('td', h('span.pill', { text: kindLabel(j.kind) })),
          h('td', [
            h('code.code', { style: { fontSize: '11.5px' }, text: j.schedule }),
            j.next_run_hint ? h('div', { style: { fontSize: '11px', color: 'var(--text-mute)', marginTop: '3px' }, text: '下次：' + j.next_run_hint }) : null,
          ]),
          h('td', [
            h('span.pill' + (j.enabled ? (j.loaded ? '.ok' : '.danger') : ''),
              { text: j.enabled ? (j.loaded ? '已启用' : '未注册') : '已停用' }),
          ]),
          h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: j.last_run || '从未执行' }),
          h('td', [
            h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, [
              h('button.btn.btn-sm', {
                text: '立即执行',
                title: '用与定时触发完全相同的环境执行一次',
                onclick: async () => {
                  try {
                    const r = await api.cronRun(j.id);
                    toast(r.msg || '已触发', 'ok', 8000);
                  } catch (e) { toast(e.message, 'err', 9000); }
                },
              }),
              h('button.btn.btn-sm', { text: '日志', onclick: () => openLog(j) }),
              h('button.btn.btn-sm', { text: '编辑', onclick: () => jobModal(j, load) }),
              h('button.btn.btn-sm', {
                text: j.enabled ? '停用' : '启用',
                onclick: async () => {
                  try {
                    await api.cronToggle(j.id, !j.enabled);
                    toast(j.enabled ? '已停用' : '已启用', 'ok');
                    load();
                  } catch (e) { toast(e.message, 'err', 9000); }
                },
              }),
              h('button.btn.btn-danger.btn-sm', {
                text: '删除',
                onclick: async () => {
                  if (!await confirmBox(`删除任务「${j.name}」？\n\n系统里的定时任务会一并移除。`, { title: '删除任务', danger: true, okText: '删除' })) return;
                  try {
                    await api.cronDelete(j.id);
                    toast('已删除', 'ok');
                    load();
                  } catch (e) { toast(e.message, 'err', 9000); }
                },
              }),
            ]),
          ]),
        ]))),
      ]),
    ]));
  }

  function kindLabel(kind) {
    return { shell: '脚本命令', backup: '备份', url: 'URL 调用' }[kind] || kind;
  }

  // ---------- 备份文件列表 ----------
  async function renderBackups() {
    clear(backupBox);
    let data = { list: [], dir: '' };
    try { data = await api.backups(); } catch { /* 忽略 */ }
    if (!data.list.length) return;

    appendAll(backupBox, h('div.card', [
      h('div.card-head', [
        h('h3', { text: '已有备份' }),
        h('div.spacer'),
        h('span.sub', { text: data.dir }),
      ]),
      h('div.card-body.tight', [
        h('table.table', [
          h('thead', [h('tr', [h('th', { text: '文件' }), h('th', { text: '大小' }), h('th', { text: '时间' }), h('th', { text: '操作' })])]),
          h('tbody', data.list.map((b) => h('tr', [
            h('td.mono', { style: { fontSize: '11.5px' }, text: b.name }),
            h('td.num', { text: humanSize(b.size) }),
            h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: b.mod_time }),
            h('td', [
              h('button.btn.btn-sm', {
                text: '下载',
                onclick: () => { window.location.href = api.fileDownloadURL(b.path); },
              }),
            ]),
          ]))),
        ]),
      ]),
    ]));
  }

  function quickBackup() {
    jobModal({
      name: 'daily-backup', kind: 'backup', schedule: '0 3 * * *',
      backup_targets: ['sites', 'nginx', 'panel'], keep_days: 7,
      backup_dir: cache?.backup_dir || '/opt/zizpanel/work/backup',
      enabled: true,
    }, load);
  }

  function openLog(j) {
    const box = h('pre.logbox', { style: { maxHeight: '420px' }, text: '加载中…' });
    const pathHint = h('div.hint', { style: { marginTop: '8px' } });
    const loadLog = async () => {
      try {
        const r = await api.cronLog(j.id, 300);
        box.textContent = r.content || (r.msg || '（暂无输出）');
        box.scrollTop = box.scrollHeight;
        pathHint.textContent = '日志文件：' + r.path;
      } catch (e) { box.textContent = '读取失败：' + e.message; }
    };
    loadLog();
    const m = modal({
      title: `运行日志：${j.name}`,
      wide: true,
      body: h('div', [
        h('div', { style: { display: 'flex', gap: '8px', marginBottom: '10px' } }, [
          h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: loadLog }),
          h('button.btn.btn-sm', {
            text: '复制',
            onclick: async () => {
              try { await navigator.clipboard.writeText(box.textContent); toast('已复制', 'ok'); }
              catch { toast('复制失败', 'warn'); }
            },
          }),
        ]),
        box,
        pathHint,
      ]),
    });
  }

  registerCleanup(() => { });
  load();
}

// ============================================================================
//  新建 / 编辑任务弹窗
// ============================================================================

export function jobModal(existing, onDone) {
  const isEdit = !!existing && !!existing.id;
  const j = existing || {};

  const name = h('input.input', { value: j.name || '', placeholder: '例如 daily-backup（需含字母或数字）' });
  const kind = h('select.select', [
    h('option', { value: 'shell', text: '执行命令 / 脚本', selected: (j.kind || 'shell') === 'shell' }),
    h('option', { value: 'backup', text: '备份（网站 / 数据库 / 配置）', selected: j.kind === 'backup' }),
    h('option', { value: 'url', text: '调用 URL', selected: j.kind === 'url' }),
  ]);
  const schedule = h('input.input', { value: j.schedule || '0 3 * * *', placeholder: '分 时 日 月 周，例如 0 3 * * *' });
  const preset = h('select.select', { style: { width: 'auto' } }, [
    h('option', { value: '', text: '常用计划…' }),
    ...(cache?.presets || []).map((p) => h('option', { value: p.schedule, text: p.label })),
  ]);
  const command = h('textarea.textarea', {
    value: j.command || '',
    placeholder: '例如：\n/opt/homebrew/bin/brew cleanup -s\n或\ncd ~/www/mysite && /usr/bin/php think cache:clear',
    style: { minHeight: '110px' },
  });
  const workDir = h('input.input', { value: j.work_dir || '', placeholder: '可选，命令的工作目录' });
  const enabled = h('input', { type: 'checkbox', checked: j.enabled !== false });

  // 备份专属字段
  const targets = ['sites', 'mysql', 'nginx', 'panel'];
  const targetLabels = { sites: '网站文件', mysql: '数据库', nginx: 'nginx 配置', panel: '面板数据' };
  const targetBoxes = {};
  const selectedTargets = j.backup_targets || ['sites', 'nginx', 'panel'];
  const backupRow = h('div', { style: { display: 'flex', gap: '14px', flexWrap: 'wrap' } },
    targets.map((t) => {
      const cb = h('input', { type: 'checkbox', checked: selectedTargets.includes(t) });
      targetBoxes[t] = cb;
      return h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '13px' } }, [
        cb, h('span', { text: targetLabels[t] }),
      ]);
    }));
  const keepDays = h('input.input', { type: 'number', value: j.keep_days || 7, min: 1, max: 365 });
  const backupDir = h('input.input', { value: j.backup_dir || cache?.backup_dir || '/opt/zizpanel/work/backup' });
  const backupFields = h('div', [
    h('div.field', [h('label', { text: '备份范围' }), backupRow]),
    h('div.row', [
      h('div.field', [h('label', { text: '保留天数' }), keepDays, h('div.hint', { text: '超过该天数的旧备份会被自动删除' })]),
      h('div.field', [h('label', { text: '备份目录' }), backupDir]),
    ]),
  ]);

  const commandFields = h('div', [
    h('div.field', [h('label', { text: '要执行的命令' }), command,
      h('div.hint', { text: '以 bash -lc 执行，支持管道与重定向。数据库密码等敏感信息请从 ~/www/.env.local 读取，不要写在这里。' })]),
    h('div.field', [h('label', { text: '工作目录' }), workDir]),
  ]);

  // 实时预览
  const preview = h('div', {
    style: { padding: '10px 12px', background: 'var(--panel-2)', borderRadius: '6px', fontSize: '12.5px', lineHeight: '1.7' },
  });
  let previewTimer = null;
  const updatePreview = async () => {
    clear(preview);
    const expr = schedule.value.trim();
    if (!expr) {
      preview.textContent = '请输入计划表达式';
      return;
    }
    try {
      const r = await api.cronPreview(expr);
      if (!r.valid) {
        preview.innerHTML = `<span style="color:var(--danger)">✗ ${escapeHtml(r.error || '表达式不合法')}</span>`;
        return;
      }
      appendAll(preview,
        h('div', [h('strong', { text: r.describe })]),
        h('div', { style: { color: 'var(--text-mute)', marginTop: '4px' }, text: '接下来执行：' + (r.next_runs || []).join('　') }),
      );
    } catch (e) {
      preview.textContent = '预览失败：' + e.message;
    }
  };
  schedule.addEventListener('input', () => {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(updatePreview, 400);
  });
  preset.addEventListener('change', () => {
    if (preset.value) { schedule.value = preset.value; updatePreview(); }
  });

  const updateKind = () => {
    const isBackup = kind.value === 'backup';
    commandFields.hidden = isBackup;
    backupFields.hidden = !isBackup;
    updatePreview();
  };
  kind.addEventListener('change', updateKind);
  updateKind();

  const submit = async (close) => {
    const payload = {
      name: name.value.trim(),
      kind: kind.value,
      schedule: schedule.value.trim(),
      command: kind.value === 'backup' ? '' : command.value,
      work_dir: workDir.value.trim(),
      enabled: enabled.checked,
      backup_targets: Object.entries(targetBoxes).filter(([, cb]) => cb.checked).map(([k]) => k),
      backup_dir: backupDir.value.trim(),
      keep_days: Number(keepDays.value) || 7,
    };
    if (!payload.name) { toast('请填写任务名称', 'warn'); return; }
    try {
      if (isEdit) await api.cronUpdate(j.id, payload);
      else await api.cronCreate(payload);
      toast(isEdit ? '已保存' : '任务已创建', 'ok');
      close();
      if (onDone) onDone();
    } catch (e) { toast(e.message, 'err', 11000); }
  };

  const m = modal({
    title: isEdit ? '编辑任务：' + j.name : '新建计划任务',
    wide: true,
    body: h('div', [
      h('div.row', [
        h('div.field', [h('label', { text: '任务名称 *' }), name,
          h('div.hint', { text: '用于生成系统标识，需包含字母或数字' })]),
        h('div.field', [h('label', { text: '类型' }), kind]),
      ]),
      h('div.field', [
        h('label', { text: '执行计划 *' }),
        h('div', { style: { display: 'flex', gap: '8px' } }, [schedule, preset]),
        h('div.hint', { text: '5 段格式：分 时 日 月 周。支持 * , - / 与英文周几（mon-fri）' }),
      ]),
      preview,
      h('div', { style: { marginTop: '14px' } }, [commandFields, backupFields]),
      h('div.field', [
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          enabled, h('span', { text: '启用该任务' }),
        ]),
      ]),
    ]),
    footer: (close) => [
      h('button.btn', { text: '取消', onclick: close }),
      h('button.btn.btn-primary', { text: isEdit ? '保存' : '创建任务', onclick: () => submit(close) }),
    ],
  });
  setTimeout(() => { name.focus(); updatePreview(); }, 60);
}

function humanSize(n) {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  const units = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(n >= 10 ? 1 : 2) + ' ' + units[i];
}

function escapeHtml(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

void promptBox;
