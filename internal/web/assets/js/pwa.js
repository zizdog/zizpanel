// pwa.js —— 让面板能"加到手机主屏"（C4）。
//
// 为什么 manifest / 图标链接由 JS 注入而不是写死在 index.html：
// 面板可能挂在 `/<安全后缀>/` 下，界面又是 hash 路由 —— 相对路径在
// `#/xxx` 这类地址下容易算错；用 session 里的 panel_entry 拼绝对路径最稳。
//
// Service Worker 注册失败**不影响任何功能**（只是没有离线壳），所以只 console.warn：
// 用 toast 吓用户反而像"面板坏了"。

function headLink(rel, href, extra = {}) {
  const el = document.createElement('link');
  el.rel = rel;
  el.href = href;
  Object.entries(extra).forEach(([k, v]) => el.setAttribute(k, v));
  document.head.appendChild(el);
  return el;
}

function headMeta(name, content) {
  const el = document.createElement('meta');
  el.name = name;
  el.content = content;
  document.head.appendChild(el);
  return el;
}

export function setupPWA(entry, version) {
  const base = entry && entry !== '/' ? String(entry).replace(/\/+$/, '/') : '/';
  try {
    headLink('manifest', base + 'manifest.webmanifest');
    headLink('apple-touch-icon', base + 'pwa/icon-192.png');
    const theme = document.querySelector('meta[name="theme-color"]');
    if (theme) theme.content = '#2563eb';
    else headMeta('theme-color', '#2563eb');
    headMeta('apple-mobile-web-app-capable', 'yes');
    headMeta('apple-mobile-web-app-title', 'ZizPanel');
  } catch { /* DOM 不可用（测试环境）：忽略 */ }

  if (!('serviceWorker' in navigator) || location.protocol === 'file:') return;
  const url = base + 'sw.js?v=' + encodeURIComponent(String(version || 'dev'));
  navigator.serviceWorker.register(url, { scope: base }).catch((e) => {
    console.warn('Service Worker 注册失败（离线壳不可用，其它功能不受影响）：', e);
  });
}

// standalone 表示"从主屏图标打开"，用于界面上的小提示（只在手机页用）。
export function isStandalone() {
  try {
    return window.matchMedia('(display-mode: standalone)').matches || window.navigator.standalone === true;
  } catch { return false; }
}
