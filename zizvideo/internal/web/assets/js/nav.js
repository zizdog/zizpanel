// 底栏四入口：首页（播放页）/ 剧场 / 收藏 / 我的（条目 10）

import { el } from "./dom.js";

const ITEMS = [
  { key: "feed", label: "首页", icon: "🏠", hash: "#/feed" },
  { key: "series", label: "剧场", icon: "🎬", hash: "#/series" },
  { key: "favorites", label: "收藏", icon: "♥", hash: "#/favorites" },
  { key: "me", label: "我的", icon: "👤", hash: "#/me" },
];

// mountNav 只返回节点：路由切换时整个 view 被清空，无需额外清理。
export function mountNav(active) {
  const nav = el("nav", { class: "bottom-nav", dataset: { role: "bottom-nav" } });
  for (const item of ITEMS) {
    nav.append(el("a", {
      class: "nav-item" + (item.key === active ? " on" : ""),
      href: item.hash, dataset: { key: item.key },
    },
      el("span", { class: "nav-icon", text: item.icon }),
      el("span", { class: "nav-label", text: item.label })));
  }
  return nav;
}
