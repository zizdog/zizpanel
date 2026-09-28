// panellimit.js —— 「面板单次上传上限」的**唯一**前端编辑/保存/回读实现。
//
// 两处入口共用这一份：
//   · 设置页「上传与执行上限」里的输入框（views.js → renderLimitsInto）；
//   · 文件管理器上传面板那行的「改上限」小窗（files.js）。
// 保存一律走 api.saveUploadLimits（后端同一处校验 + 落盘 + 回读），界面上的数字
// 一律来自**服务端回读**，绝不把用户输入当成已生效值。
//
// 与站点限制（nginx client_max_body_size / PHP upload_max_filesize）不是一回事：
// 这一项只写 config.json，不碰 nginx/PHP，也不重启任何服务。

import { api } from './api.js';
import { h, toast, modal, bytes } from './ui.js';

// 一句话说明（≤40 字）；三条细节全在 PANEL_LIMIT_TITLE（title 属性里）。
export const PANEL_LIMIT_HINT = '服务端按它拒收超限文件；几 GB 大文件建议直接放磁盘。';
export const PANEL_LIMIT_TITLE = '服务端按这个值拒收：超限文件一个字节都不传。'
  + '若面板在反向代理/隧道后面，代理自身也有上限（同一块设置里的 nginx client_max_body_size；'
  + '隧道服务商可能另有更小的硬限）—— 面板改不了它们。'
  + '超大文件（几 GB）用浏览器上传很脆弱、断线要重来；媒体文件更稳的做法是直接放到磁盘'
  + '（文件管理里的复制/移动，或 Finder/SMB/NAS 同步），Jellyfin 只负责扫描媒体库。';

// panelLimitText 把服务端回读视图写成人话（输入框旁边那行"当前生效值"）。
export function panelLimitText(p) {
  p = p || {};
  const b = bytes(p.limit_bytes || 0);
  if (!p.verified) {
    return '当前未复核：' + (p.note || '配置里读不到有效值') + '（暂按 ' + b + ' 拒收）';
  }
  return '当前生效：' + b + '（来源：' + (p.source || '面板配置') + '）';
}

// buildPanelLimitField 造「输入框 + 当前生效值 + 说明」那一组 DOM（两处入口共用）。
export function buildPanelLimitField(current) {
  const input = h('input.input', {
    value: (current && current.limit_text) || '',
    placeholder: '例如 4g / 8g / 12288m',
    style: { maxWidth: '220px' },
    title: '面板自身单次 HTTP 请求的读入上限；与站点 nginx/PHP 的上限不是一回事（写进 config.json）',
    dataset: { testid: 'zp-panel-upload-limit-input' },
  });
  const eff = h('div.hint', {
    dataset: { testid: 'zp-panel-upload-limit-effective' },
    text: panelLimitText(current),
  });
  const field = h('div.field', [
    h('label', { text: '面板单次上传上限' }),
    h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [input, eff]),
    h('div.hint', { text: PANEL_LIMIT_HINT, title: PANEL_LIMIT_TITLE }),
  ]);
  return {
    input,
    eff,
    field,
    // set 用**服务端回读结果**就地刷新输入框与生效值（保存后调它，不是本地赋值）。
    set: (p) => {
      input.value = (p && p.limit_text) || '';
      eff.textContent = panelLimitText(p);
    },
  };
}

// savePanelLimitOnly 是两处入口共用的保存 + 回读路径。
//
// 只提交面板上限：后端判定站点限制没变时会同步回 200 + 回读视图（不建任务、
// 不 reload nginx）。返回值是**服务端回读的真实生效值**（非法值会在这里抛 400 人话）。
export async function savePanelLimitOnly(value) {
  const res = await api.saveUploadLimits({ panel_upload_limit: String(value == null ? '' : value).trim() });
  if (res && res.panel) return res.panel;
  // 没有同步回读（老后端）：退回到回读接口，绝不把请求值当成生效值。
  return await api.fileUploadLimit();
}

// openPanelLimitEditor 弹小窗改上限（文件管理器入口）。
//
// current 是打开时的回读视图；onSaved(回读视图) 由调用方刷新它自己那行。
export function openPanelLimitEditor(current, onSaved) {
  const f = buildPanelLimitField(current);
  modal({
    title: '修改面板单次上传上限',
    body: f.field,
    footer: (close) => {
      const saveBtn = h('button.btn.btn-primary', {
        text: '保存',
        dataset: { testid: 'zp-panel-upload-limit-save' },
        onclick: async () => {
          saveBtn.disabled = true;
          try {
            const fresh = await savePanelLimitOnly(f.input.value);
            f.set(fresh); // 就地显示服务端回读值
            if (typeof onSaved === 'function') onSaved(fresh);
            toast('面板单次上传上限已保存：' + bytes(fresh.limit_bytes || 0), 'ok', 6000);
            close();
          } catch (e) {
            // 非法值就地报错；值不变（后端未落盘，界面也不会改数字）。
            toast((e && e.message) || String(e), 'err', 12000);
          } finally {
            saveBtn.disabled = false;
          }
        },
      });
      return h('div', { style: { display: 'flex', gap: '8px', justifyContent: 'flex-end', width: '100%' } }, [
        h('button.btn.btn-sm', { text: '取消', onclick: close }),
        saveBtn,
      ]);
    },
  });
}
