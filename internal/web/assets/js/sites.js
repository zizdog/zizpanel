// sites.js —— 网站管理页面。
//
// 交互设计参考宝塔但做了取舍：
//   - 列表页直接给出"配置是否存在""PHP 是否在跑""证书到期"这些真实状态，
//     而不是只显示数据库里的记录 —— 面板与 nginx 状态不一致是最常见的求助原因。
//   - 新建向导把「伪静态模板」和「PHP 版本」放在一起，因为它们互相影响
//     （Laravel/ThinkPHP 的运行目录要落到 public）。
//   - 每个站点提供"诊断"入口，一键做完 HTTP 探测、PHP 探针、证书检查、错误日志。

import { api } from './api.js';
import {
  h, clear, toast, modal, confirmBox, $,
} from './ui.js';
import { state, registerCleanup } from './app.js';

let cache = null; // 站点列表数据（含预设与 PHP 版本）

// 站点详情面板当前所在的 Tab
let detailTab = 'basic';

export function SitesView(content, ctx = {}) {
  clear(content);

  const listBox = h('div');
  const statusBar = h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } });

  const toolbar = h('div.card-head', [
    h('h3', { text: '站点列表' }),
    h('div.spacer'),
    statusBar,
  ]);

  content.append(
    h('div.card', [
      toolbar,
      h('div.card-body.tight', [listBox]),
    ]),
  );

  async function load() {
    clear(listBox);
    listBox.append(h('div.empty', [h('div.big', { text: '⏳' }), h('p', { text: '正在读取站点…' })]));
    try {
      cache = await api.sites();
    } catch (e) {
      clear(listBox);
      listBox.append(h('div.empty', [
        h('div.big', { text: '⚠️' }),
        h('h4', { text: '读取站点失败' }),
        h('p', { text: e.message }),
      ]));
      return;
    }
    renderStatus();
    renderList();
  }

  function renderStatus() {
    clear(statusBar);
    const c = cache || {};
    const phpRunning = (c.php_versions || []).filter((p) => p.running).length;
    statusBar.append(
      h('span.pill', { text: `共 ${(c.list || []).length} 个站点` }),
      h('span.pill' + (phpRunning > 0 ? '.ok' : '.warn'), {
        text: `PHP-FPM 运行中 ${phpRunning} 个`,
        title: (c.php_versions || []).map((p) => `${p.version} (${p.pass}) ${p.running ? '运行中' : '未运行'}`).join('\n'),
      }),
      h('button.btn.btn-sm', { text: '⟳ 刷新', onclick: load }),
      h('button.btn.btn-sm', {
        text: '🧪 校验 nginx 配置',
        onclick: async () => {
          try {
            const r = await api.nginxTest();
            if (r.ok) toast('nginx 配置校验通过', 'ok');
            else toast('配置有问题：' + r.output, 'err', 12000);
          } catch (e) { toast(e.message, 'err'); }
        },
      }),
      h('button.btn.btn-sm', {
        text: '🔧 修复 nginx 环境',
        title: '补齐反向代理所需的 WebSocket 升级 map 与 conf.d 加载',
        onclick: async () => {
          try {
            const r = await api.nginxRepair();
            toast(r.msg || '已修复', 'ok');
          } catch (e) { toast(e.message, 'err', 9000); }
        },
      }),
      h('button.btn.btn-sm', {
        text: '♻️ 重建全部配置',
        title: '按当前数据库状态重新生成所有站点的 nginx 配置（用于修复被手工改坏的配置）',
        onclick: async () => {
          if (!await confirmBox('将按模板重新生成所有站点的 nginx 配置并重载。\n\n你自己手工加在 vhost 里的内容会被覆盖（面板只保留数据库中的设置）。\n\n继续？', { title: '重建全部配置' })) return;
          try {
            const r = await api.siteReloadAll();
            const n = (r.rebuilt || []).length;
            if ((r.failed || []).length) toast(`重建 ${n} 个，失败 ${r.failed.length} 个：${r.failed[0]}`, 'warn', 12000);
            else toast(`已重建 ${n} 个站点配置`, 'ok');
            load();
          } catch (e) { toast(e.message, 'err', 9000); }
        },
      }),
      h('button.btn.btn-primary.btn-sm', { text: '+ 新建站点', onclick: newSiteModal }),
    );
  }

  function renderList() {
    clear(listBox);
    const list = (cache && cache.list) || [];
    if (!list.length) {
      listBox.append(h('div.empty', [
        h('div.big', { text: '🌐' }),
        h('h4', { text: '还没有站点' }),
        h('p', { text: '点击右上角「新建站点」，输入一个域名即可开始。' }),
        h('div', { style: { marginTop: '16px' } }, [
          h('button.btn.btn-primary', { text: '新建第一个站点', onclick: newSiteModal }),
        ]),
      ]));
      return;
    }

    const tbody = h('tbody', list.map((s) => h('tr', [
      h('td', [
        h('div', { style: { fontWeight: '600' }, text: s.domain }),
        s.aliases ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.aliases }) : null,
        s.remark ? h('div', { style: { fontSize: '11.5px', color: 'var(--text-mute)' }, text: s.remark }) : null,
      ]),
      h('td', [
        h('span.pill' + (s.conf_exists ? '.ok' : '.danger'), {
          text: s.conf_exists ? (s.enabled ? '运行中' : '已停用') : '配置缺失',
          title: s.conf_exists ? 'nginx 配置文件存在' : '数据库有这个站点，但 nginx 配置文件不存在 —— 请点「重建全部配置」',
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
          h('button.btn.btn-sm', { text: '管理', onclick: () => openDetail(s.domain) }),
          h('button.btn.btn-sm', { text: '诊断', onclick: () => runCheck(s.domain) }),
          h('button.btn.btn-danger.btn-sm', { text: '删除', onclick: () => delSite(s) }),
        ]),
      ]),
    ])));

    listBox.append(h('div', { style: { overflowX: 'auto' } }, [
      h('table.table', [
        h('thead', [h('tr', [
          h('th', { text: '域名' }), h('th', { text: '状态' }), h('th', { text: 'PHP' }),
          h('th', { text: '路由' }), h('th', { text: 'SSL' }), h('th', { text: '运行目录' }), h('th', { text: '操作' }),
        ])]),
        tbody,
      ]),
    ]));
  }

  function presetLabel(name) {
    const p = (cache?.presets || []).find((x) => x.name === name);
    return p ? p.label : (name || '无');
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
    const php = h('select.select', [
      h('option', { value: '', text: '纯静态（不解析 PHP）' }),
      ...phps.map((p) => h('option', {
        value: p.version,
        text: `PHP ${p.version}${p.running ? '' : '（未运行）'}${p.is_default ? ' ← 当前默认' : ''}`,
        selected: p.is_default,
      })),
    ]);
    const proxy = h('input.input', { placeholder: '可选，如 http://127.0.0.1:3000（填了就是反向代理站点）' });
    const presetHint = h('div.hint', { text: '' });
    const rootPreview = h('code.code', { text: '' });

    const updatePreview = () => {
      const d = domain.value.trim().toLowerCase();
      const p = presets.find((x) => x.name === preset.value);
      const dir = p && p.public_dir ? `${wwwRoot}/${d}/${p.public_dir}` : `${wwwRoot}/${d}`;
      rootPreview.textContent = d ? dir : '（请输入域名）';
      if (p && p.public_dir) {
        presetHint.textContent = `${p.label}：运行目录会自动设为 public 子目录。${p.description}`;
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
    updatePreview();

    const submit = async (close) => {
      const d = domain.value.trim().toLowerCase();
      if (!d) { toast('请输入域名', 'warn'); return; }
      try {
        const r = await api.siteCreate({
          domain: d,
          aliases: aliases.value.trim(),
          php_version: proxy.value.trim() ? '' : php.value,
          rewrite: proxy.value.trim() ? 'none' : preset.value,
          proxy_pass: proxy.value.trim(),
          remark: remark.value.trim(),
        });
        toast(`站点 ${d} 创建成功`, 'ok');
        close();
        load();
        if (r && r.root) setTimeout(() => openDetail(d), 300);
      } catch (e) {
        toast(e.message, 'err', 9000);
      }
    };

    const m = modal({
      title: '新建站点',
      body: h('div', [
        h('div.field', [h('label', { text: '域名 *' }), domain, h('div.hint', { text: '不需要输入 www，附加域名写在下一个字段' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases]),
        h('div.field', [h('label', { text: '运行目录' }), rootPreview, h('div.hint', { text: '目录会自动创建（已在 ~/www 下）' })]),
        h('div.field', [h('label', { text: '路由 / 伪静态' }), preset, presetHint]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php, h('div.hint', { text: '选择"纯静态"时，nginx 会拒绝执行该站点下的 PHP 文件' })]),
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
      const aliases = h('input.input', { value: site.aliases || '' });
      const remark = h('input.input', { value: site.remark || '' });
      const enabled = h('input', { type: 'checkbox', checked: site.enabled });
      const save = h('button.btn.btn-primary', {
        text: '保存',
        onclick: async () => {
          save.disabled = true;
          try {
            await api.siteUpdate(domain, {
              aliases: aliases.value.trim(), remark: remark.value.trim(), enabled: enabled.checked,
            });
            toast('已保存', 'ok');
            load();
          } catch (e) { toast(e.message, 'err', 9000); }
          finally { save.disabled = false; }
        },
      });
      return h('div', [
        h('div.field', [h('label', { text: '主域名' }), h('div', [h('code.code', { text: site.domain })]),
          h('div.hint', { text: '域名创建后不可修改（改域名等于新建站点，涉及目录与证书）' })]),
        h('div.field', [h('label', { text: '附加域名' }), aliases, h('div.hint', { text: '多个用英文逗号分隔，例如 www.demo.test, m.demo.test' })]),
        h('div.field', [h('label', { text: '运行目录' }), h('div', [h('code.code', { text: site.root })]),
          h('div.hint', { text: '由路由模板决定（Laravel/ThinkPHP 会指向 public 子目录）' })]),
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
      const php = h('select.select', [
        h('option', { value: '', text: '纯静态（不解析 PHP）', selected: !site.php_version }),
        ...phps.map((p) => h('option', {
          value: p.version,
          text: `PHP ${p.version}${p.running ? '' : '（未运行）'} · ${p.pass}`,
          selected: p.version === site.php_version,
        })),
      ]);
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
          } catch (e) { toast(e.message, 'err', 12000); }
          finally { save.disabled = false; }
        },
      });

      return h('div', [
        h('div.field', [h('label', { text: '路由 / 伪静态模板' }), preset, hint]),
        h('div.field', [h('label', { text: 'PHP 版本' }), php,
          h('div.hint', { text: phps.some((p) => p.running) ? '只解析该版本 FPM 监听的地址；版本未运行会返回 502' : '未检测到运行中的 PHP-FPM，请先启动相应服务' })]),
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
                  if (!await confirmBox('关闭后站点只监听 80 端口。继续？', { danger: true })) return;
                  try {
                    const r = await api.siteSSLDisable(domain);
                    Object.assign(site, r.site);
                    toast('已关闭 HTTPS', 'ok');
                    render(); load();
                  } catch (e) { toast(e.message, 'err', 9000); }
                },
              }),
            ]),
          );
          return;
        }
        box.append(
          h('div.hint', { style: { marginBottom: '14px' }, text: '开启 HTTPS 后，80 端口的请求会自动 301 跳转到 443。' }),
          h('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } }, [
            h('button.btn.btn-primary', { text: '用 mkcert 签发（推荐）', onclick: () => issue('mkcert') }),
            h('button.btn', { text: '自签证书', onclick: () => issue('self') }),
            h('button.btn', { text: '粘贴自有证书', onclick: () => manual() }),
          ]),
          h('div.hint', { style: { marginTop: '12px' } }, [
            h('div', { text: '• mkcert：使用本机 mkcert CA 签发。若已在系统信任该 CA，浏览器不会提示。' }),
            h('div', { text: '• 自签证书：无需任何依赖，浏览器会提示不受信任（点"继续访问"即可）。' }),
            h('div', { text: '• Let\'s Encrypt 自动签发需要公网域名且 80 端口可达，将在后续版本提供。' }),
          ]),
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
          toast(e.message, 'err', 14000);
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
                } catch (e) { toast(e.message, 'err', 12000); }
              },
            }),
          ],
        });
      }

      render();
      return box;
    }

    function tabConf() {
      const showGenerated = h('button.btn.btn-sm.btn-primary', { text: '面板生成的配置' });
      const showActual = h('button.btn.btn-sm', { text: '磁盘上的实际配置' });
      const box = h('pre.logbox', { style: { maxHeight: '420px' }, text: '' });
      const setMode = (gen) => {
        showGenerated.className = 'btn btn-sm' + (gen ? ' btn-primary' : '');
        showActual.className = 'btn btn-sm' + (gen ? '' : ' btn-primary');
        box.textContent = gen
          ? (data.generated || '（生成失败：' + (data.generate_err || '未知原因') + '）')
          : (data.conf || '（配置文件不存在，请点「重建全部配置」）');
      };
      showGenerated.addEventListener('click', () => setMode(true));
      showActual.addEventListener('click', () => setMode(false));
      setMode(true);

      return h('div', [
        h('div', { style: { display: 'flex', gap: '8px', marginBottom: '10px', flexWrap: 'wrap' } }, [
          showGenerated, showActual,
          h('button.btn.btn-sm', {
            text: '🔄 重新生成并应用',
            onclick: async () => {
              try {
                await api.siteUpdate(domain, {});
                toast('已重新生成并重载', 'ok');
                const fresh = await api.site(domain);
                data.conf = fresh.conf; data.generated = fresh.generated;
                setMode(false);
                load();
              } catch (e) { toast(e.message, 'err', 12000); }
            },
          }),
        ]),
        box,
        h('div.hint', { style: { marginTop: '10px' }, text: `配置文件路径：${data.conf_path}` }),
        h('div.hint', { text: '注意：直接在服务器上手工修改该文件，会在面板下次保存时被覆盖。持久化的改动用「自定义配置」字段。' }),
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
    const removeFiles = h('input', { type: 'checkbox' });
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
        h('div.hint', { style: { marginTop: '10px' }, text: '不勾选则只删除配置，文件保留，方便之后重新绑定。' }),
      ]),
      footer: (close) => [
        h('button.btn', { text: '取消', onclick: close }),
        h('button.btn.btn-danger', {
          text: '确认删除',
          onclick: async () => {
            try {
              const r = await api.siteDelete(s.domain, removeFiles.checked);
              toast(r.msg + (r.files ? '（' + r.files + '）' : ''), 'ok');
              close();
              load();
            } catch (e) { toast(e.message, 'err', 9000); }
          },
        }),
      ],
    });
  }

  registerCleanup(() => { });
  load();
}
