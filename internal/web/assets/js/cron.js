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
import { taskCenter } from './tasks.js';

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
  //
  // 这块是「备份与恢复」的入口：立即备份 / 上传备份恢复 / 逐行 恢复·下载·删除。
  // 恢复是长任务，统一交给任务中心（taskCenter.start），进度在顶栏「任务中心」里看。
  async function renderBackups() {
    clear(backupBox);
    let data = { list: [], dir: '', targets: [] };
    let loadErr = '';
    try { data = await api.backups() || data; } catch (e) { loadErr = e.message || String(e); }

    const head = h('div.card-head', [
      h('h3', { text: '已有备份' }),
      h('div.spacer'),
      h('span.sub', { text: data.dir || '' }),
      h('button.btn.btn-sm', {
        text: '⬆ 上传备份恢复',
        title: '上传本机或其他机器导出的 .tar.gz 备份归档（上传后立即校验 sha256，坏包会被拒绝）',
        onclick: () => uploadBackup(data),
      }),
      h('button.btn.btn-primary.btn-sm', {
        text: '立即备份',
        title: '马上生成一份新的备份归档（数据库用一致性快照，带 sha256 清单）',
        onclick: () => instantBackupModal(data),
      }),
    ]);

    const list = data.list || [];
    let inner;
    if (loadErr) {
      inner = h('div.empty', [h('div.big', { text: '⚠️' }), h('h4', { text: '读取备份列表失败' }), h('p', { text: loadErr })]);
    } else if (!list.length) {
      inner = h('div.empty', [
        h('div.big', { text: '🗄️' }),
        h('h4', { text: '还没有备份' }),
        h('p', { text: '点右上角「立即备份」马上生成一份；也可以创建定时备份任务让它每天自动跑。' }),
      ]);
    } else {
      inner = h('div', { style: { overflowX: 'auto' } }, [
        h('table.table', [
          h('thead', [h('tr', [
            h('th', { text: '文件' }), h('th', { text: '来源 / 时间' }), h('th', { text: '内容' }),
            h('th', { text: '大小' }), h('th', { text: '操作' }),
          ])]),
          h('tbody', list.map((b) => h('tr', [
            h('td', [
              h('div.mono', { style: { fontSize: '11.5px' }, text: b.name }),
              b.is_snapshot ? h('span.pill', {
                text: '恢复前快照',
                title: '恢复操作自动生成的回滚凭据：恢复后如果发现问题，可以用它再恢复一次回到原状',
              }) : null,
            ]),
            h('td', { style: { fontSize: '11.5px', color: 'var(--text-mute)' } }, [
              h('div', { text: b.manifest_ok ? (b.hostname || '未知来源') : '⚠️ 清单不可读' }),
              h('div', { text: b.created_at || b.mod_time || '' }),
            ]),
            h('td', { style: { fontSize: '11.5px' } }, [
              b.manifest_ok
                ? h('div', { text: (b.targets || []).join(' · ') || '（未记录范围）' })
                : h('div', { style: { color: 'var(--danger, #d33)' }, text: b.manifest_error || '清单不可读' }),
              b.manifest_ok && b.contains_secrets
                ? h('span.pill.danger', {
                    text: `含明文口令（${b.secret_count} 个文件）`,
                    title: '归档里有面板配置、ACME 账号私钥或站点私钥。请当机密文件保管，不要外发。',
                  })
                : null,
            ]),
            h('td.num', { text: humanSize(b.size) }),
            h('td', [
              h('div', { style: { display: 'flex', gap: '4px', flexWrap: 'wrap' } }, [
                h('button.btn.btn-sm', {
                  text: '恢复',
                  title: '用这份归档恢复站点配置 / 证书 / 反向代理（会先生成恢复前快照）',
                  onclick: () => restoreModal(b),
                }),
                h('button.btn.btn-sm', {
                  text: '下载',
                  onclick: () => { window.location.href = api.backupDownloadURL(b.name); },
                }),
                h('button.btn.btn-danger.btn-sm', {
                  text: '删除',
                  onclick: async () => {
                    const extra = b.contains_secrets ? '\n\n⚠️ 这个归档含明文口令，删除后无法找回。' : '';
                    if (!await confirmBox(`删除备份「${b.name}」？${extra}`, { title: '删除备份', danger: true, okText: '删除' })) return;
                    try {
                      await api.backupDelete(b.name);
                      toast('已删除', 'ok');
                      renderBackups();
                    } catch (e) { toast(e.message, 'err', 9000); }
                  },
                }),
              ]),
            ]),
          ]))),
        ]),
      ]);
    }

    appendAll(backupBox, h('div.card', [head, h('div.card-body.tight', [inner])]));
  }

  // 「立即备份」：勾选范围（含密项默认不勾，勾了会明确提示"含凭据"）。
  function instantBackupModal(data) {
    const targets = data.targets || [];
    const boxes = targets.map((t) => {
      const cb = h('input', { type: 'checkbox' });
      // 默认勾选 panel + nginx（站点的配置/证书/反代都在里面）；
      // 含第三方 token 的应用配置与体积巨大的网站文件默认**不勾**。
      cb.checked = ['panel', 'nginx'].includes(t.id);
      return { t, cb };
    });
    const keepInput = h('input.input', { type: 'number', min: '0', value: '7', style: { width: '90px' } });
    const m = modal({
      title: '立即备份',
      body: h('div', [
        h('p.hint', { text: '数据库会用一致性快照（VACUUM INTO）导出，归档里带每个文件的 sha256 清单。' }),
        h('div', { style: { display: 'grid', gap: '6px', margin: '10px 0' } }, boxes.map(({ t, cb }) =>
          h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start' } }, [
            cb,
            h('span', [
              h('span', { text: t.label }),
              t.opt_in ? h('span.pill', { style: { marginLeft: '6px' }, text: '含凭据' }) : null,
            ]),
          ]))),
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
          h('span', { text: '保留最近' }),
          keepInput,
          h('span', { text: '天（0 = 不自动清理旧备份）' }),
        ]),
      ]),
      footer: [
        h('button.btn', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-primary', {
          text: '开始备份',
          onclick: () => {
            const sel = boxes.filter(({ cb }) => cb.checked).map(({ t }) => t.id);
            if (!sel.length) { toast('请至少选择一个备份范围', 'warn'); return; }
            m.close();
            taskCenter.start({
              kind: 'backup',
              target: 'backup:manual',
              title: '立即备份（' + sel.join(',') + '）',
              start: () => api.backupCreate({ targets: sel, keep_days: Number(keepInput.value) || 0 }),
              onDone: () => { toast('备份任务已结束', 'ok'); renderBackups(); },
            });
          },
        }),
      ],
    });
  }

  // 「恢复」：先展示归档来源（哪台机器/什么时候/含不含明文口令），再二次确认。
  async function restoreModal(b) {
    if (!b.manifest_ok) {
      toast('这份归档的清单不可读，无法恢复：' + (b.manifest_error || ''), 'err', 12000);
      return;
    }
    let info = null;
    try { info = await api.backupInfo(b.name); } catch (e) { toast('读取归档信息失败：' + e.message, 'err', 9000); return; }
    if (!info.compatible) {
      toast('不能恢复：' + info.compat_reason, 'err', 14000);
      return;
    }

    const secretList = (info.secret_files || []).slice(0, 8);
    const secretMore = (info.secret_files || []).length - secretList.length;
    const cfgBox = h('input', { type: 'checkbox' });
    const body = h('div', [
      h('table.table', [
        h('tbody', [
          h('tr', [h('th', { text: '来源机器' }), h('td', { text: info.hostname || '未知' })]),
          h('tr', [h('th', { text: '备份时间' }), h('td', { text: info.created_at || '未知' })]),
          h('tr', [h('th', { text: '面板版本' }), h('td', { text: info.panel_version || '未知' })]),
          h('tr', [h('th', { text: '备份范围' }), h('td', { text: (info.targets || []).join(' · ') })]),
          h('tr', [h('th', { text: '文件数' }), h('td', { text: String(info.file_count || 0) })]),
        ]),
      ]),
      info.older ? h('p.hint', {
        style: { marginTop: '8px' },
        text: '⚠️ 这份备份比当前面板旧，恢复后会自动重放数据库迁移补齐新增字段。',
      }) : null,
      info.contains_secrets ? h('div', {
        style: { marginTop: '10px', padding: '8px', border: '1px solid var(--danger, #d33)', borderRadius: '6px' },
      }, [
        h('div', { style: { fontWeight: '600' }, text: `⚠️ 归档含 ${(info.secret_files || []).length} 个明文口令/私钥文件` }),
        h('div', { style: { fontSize: '12px', color: 'var(--text-mute)', marginTop: '4px' }, text: secretList.join('、') + (secretMore > 0 ? ` 等 ${secretList.length + secretMore} 个` : '') }),
      ]) : null,
      h('div', { style: { marginTop: '12px' } }, [
        h('p', { text: '恢复会做这些事：先把当前状态自动快照一份（回滚凭据），替换面板数据库，写回证书与 ACME 状态，按数据库重建站点与反向代理配置，并重新注册计划任务。恢复后需要重新登录。' }),
        h('label', { style: { display: 'flex', gap: '8px', alignItems: 'flex-start', marginTop: '8px' } }, [
          cfgBox,
          h('span', [
            h('b', { text: '同时恢复面板配置（config.json）' }),
            h('div', { style: { fontSize: '12px', color: 'var(--text-mute)' } ,
              text: '默认不恢复，保持本机身份。勾选它会把面板后缀、监听端口、升级源一起换成备份里那套，' +
                '需要重启面板才生效 —— 网页会断开几秒。只有在「把面板搬到新机器」时才需要勾。' }),
          ]),
        ]),
      ]),
    ]);

    const m = modal({
      title: '恢复：' + b.name,
      body,
      footer: [
        h('button.btn', { text: '取消', onclick: () => m.close() }),
        h('button.btn.btn-danger', {
          text: '开始恢复',
          onclick: () => {
            const wantCfg = cfgBox.checked;
            if (wantCfg && !window.confirm('再次确认：恢复面板配置会把面板后缀/监听端口/升级源一起换掉，并重启面板，网页会断开几秒。继续？')) {
              return;
            }
            m.close();
            taskCenter.start({
              kind: 'restore',
              target: 'backup:restore',
              title: '恢复备份 ' + b.name,
              start: () => api.backupRestore(b.name, wantCfg),
              onDone: (meta) => {
                if (meta && meta.status && meta.status !== 'succeeded') {
                  toast('恢复未全部成功：' + (meta.error || meta.status) + '（详见任务日志里的「未恢复」清单）', 'err', 15000);
                } else {
                  toast('恢复完成', 'ok', 9000);
                }
                renderBackups();
              },
            });
          },
        }),
      ],
    });
  }

  // 「上传备份恢复」：上传后立刻整包校验，坏包当场拒绝。
  function uploadBackup(data) {
    const input = h('input', { type: 'file', accept: '.tar.gz,application/gzip', style: { display: 'none' } });
    input.addEventListener('change', async () => {
      const file = input.files && input.files[0];
      if (!file) { input.remove(); return; }
      if (!/\.tar\.gz$/i.test(file.name)) {
        toast('请选择 .tar.gz 备份归档', 'warn', 9000);
        input.remove();
        return;
      }
      toast('正在上传并校验 ' + file.name + ' …', 'info', 8000);
      try {
        const r = await api.backupUpload(file);
        toast(`已上传：来源 ${r.hostname || '未知'}，${r.created_at || ''}` +
          (r.contains_secrets ? `（含 ${r.secret_count} 个明文口令文件）` : ''), 'ok', 12000);
        await renderBackups();
      } catch (e) {
        toast('上传失败：' + e.message, 'err', 14000);
      }
      input.remove();
    });
    document.body.appendChild(input);
    input.click();
  }

  function quickBackup() {
    jobModal({
      name: 'daily-backup', kind: 'backup', schedule: '0 3 * * *',
      backup_targets: ['sites', 'nginx', 'panel'], keep_days: 7,
      // 目录从后端配置来，不再写死 /opt/zizpanel（重定位安装下会写错地方）。
      backup_dir: cache?.backup_dir || '',
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

  // 备份专属字段。可选范围从后端来（含按应用注册表派生的 apps:* 含密项），
  // 不再在前端抄一份写死的四项目录。
  const targetOpts = (cache?.targets && cache.targets.length)
    ? cache.targets
    : [
      { id: 'sites', label: '网站文件' }, { id: 'mysql', label: '数据库' },
      { id: 'nginx', label: 'nginx 配置' }, { id: 'panel', label: '面板数据' },
    ];
  const targetBoxes = {};
  const selectedTargets = j.backup_targets || ['sites', 'nginx', 'panel'];
  const backupRow = h('div', { style: { display: 'flex', gap: '14px', flexWrap: 'wrap' } },
    targetOpts.map((t) => {
      const cb = h('input', { type: 'checkbox', checked: selectedTargets.includes(t.id) });
      targetBoxes[t.id] = cb;
      return h('label', {
        style: { display: 'flex', gap: '6px', alignItems: 'center', fontSize: '13px' },
        title: t.opt_in ? '这一项含第三方服务凭据（token / 密码），默认不勾；勾了归档里就会有它' : '',
      }, [
        cb,
        h('span', { text: t.label + (t.opt_in ? '（含凭据）' : '') }),
      ]);
    }));
  const keepDays = h('input.input', { type: 'number', value: j.keep_days || 7, min: 1, max: 365 });
  const backupDir = h('input.input', { value: j.backup_dir || cache?.backup_dir || '' });
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
