// sw.js —— 面板的 PWA Service Worker（无构建步骤：浏览器直接加载这个文件）。
//
// 缓存策略（三条，都是有意的）：
//   1. 只缓存**面板自己的静态外壳**（入口页 / CSS / JS / 图标）：离线时至少能打开界面，
//      用过的页面能开。
//   2. `/api/` **一律走网络、绝不缓存**：接口数据必须新鲜 —— 拿旧数据糊弄用户
//      比打不开更糟（前端会如实显示"无法连接面板服务"）。
//   3. 缓存名带版本号（注册时用 `?v=<面板版本>`）：面板就地升级后注册地址变了，
//      自动换一个 worker 与空缓存，不会把旧版 JS 一直喂给浏览器。
//
// 另外：**代理出去的应用界面与导航页一律不碰**（/iopaint/、/aria/、/nav/ …）。
// 缓存别人的界面会变成"改了配置不生效"这种最难查的问题。

const SW_VERSION = new URL(self.location.href).searchParams.get('v') || 'dev';
const CACHE = 'zpanel-shell-' + SW_VERSION;
const SCOPE = self.registration.scope; // 面板入口（含安全后缀），如 https://host:8443/abc/

// isShellAsset 判"这是不是面板自己的静态资源"。
//
// 判据用**相对面板入口的路径**，所以带后缀与不带后缀两种部署都对；
// 落在入口之外的（应用代理、导航页别名）一律返回 false。
function isShellAsset(url) {
  if (url.origin !== self.location.origin) return false;
  if (!url.pathname.startsWith(SCOPE)) return false;
  const rel = url.pathname.slice(new URL(SCOPE).pathname.length);
  if (rel === '' || rel === 'index.html' || rel === 'app.css') return true;
  return rel.startsWith('js/') || rel.startsWith('vendor/') || rel.startsWith('pwa/');
}

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    // 只预热入口页：本项目没有构建步骤，列不出全部模块清单；
    // 其余文件在第一次真正用到时按需缓存（见 fetch）。
    await cache.addAll([SCOPE, SCOPE + 'index.html']).catch(() => { /* 预热失败不影响安装 */ });
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(
      names
        .filter((n) => n.startsWith('zpanel-shell-') && n !== CACHE)
        .map((n) => caches.delete(n)),
    );
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.pathname.indexOf('/api/') >= 0) return; // 接口：只走网络，绝不缓存
  if (!isShellAsset(url)) return;                 // 别人的界面：完全不插手

  event.respondWith((async () => {
    const cache = await caches.open(CACHE);
    const hit = await cache.match(req);
    // 后台更新（stale-while-revalidate）：先用缓存保证快与离线可用，
    // 同时把新版本写回缓存，下次打开就是新的。
    const fresh = fetch(req).then(async (res) => {
      if (res && res.ok && res.type === 'basic') {
        // await：put 完成前不让事件生命周期结束（否则写入可能被丢弃）
        await cache.put(req, res.clone()).catch(() => { /* 配额满/私有模式：忽略 */ });
      }
      return res;
    }).catch(() => null);
    if (hit) return hit;
    const res = await fresh;
    if (res) return res;
    // 真的拿不到就如实报错，**不伪造**一个看起来正常的响应。
    return new Response('离线，且这个文件还没有被缓存。', {
      status: 503,
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    });
  })());
});
