// DOM 助手：只用 createElement / textContent，API 文本一律不进 innerHTML（坑 1）

export function add(parent, children) {
  const list = Array.isArray(children) ? children.flat(Infinity) : [children];
  for (const child of list) {
    if (child === null || child === undefined || child === false || child === true) continue;
    if (child instanceof Node) parent.append(child);
    else parent.append(document.createTextNode(String(child)));
  }
  return parent;
}

export function el(tag, props, ...children) {
  const node = document.createElement(tag);
  if (props) {
    for (const key of Object.keys(props)) {
      const value = props[key];
      if (value === null || value === undefined || value === false) continue;
      if (key === "class") node.className = String(value);
      else if (key === "text") node.textContent = String(value);
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key === "style") Object.assign(node.style, value);
      else if (key.startsWith("on") && typeof value === "function") {
        node.addEventListener(key.slice(2).toLowerCase(), value);
      } else if (value === true) node.setAttribute(key, "");
      else node.setAttribute(key, String(value));
    }
  }
  return add(node, children);
}

// asArray：接口字段不保证是数组时统一收敛，别把非数组喂给遍历或 append 展开（坑 13）
export function asArray(value) {
  return Array.isArray(value) ? value : [];
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function fmtDuration(ms) {
  const total = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
  const two = (n) => (n < 10 ? "0" + n : String(n));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return h > 0 ? h + ":" + two(m) + ":" + two(s) : m + ":" + two(s);
}

export function fmtBytes(bytes) {
  const value = Number(bytes);
  if (!Number.isFinite(value) || value < 0) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let index = 0;
  let size = value;
  while (size >= 1024 && index < units.length - 1) { size /= 1024; index += 1; }
  const text = index === 0 ? String(Math.round(size)) : size.toFixed(size >= 100 ? 0 : 1);
  return text + " " + units[index];
}

export function fmtDate(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  const two = (n) => (n < 10 ? "0" + n : String(n));
  return date.getFullYear() + "-" + two(date.getMonth() + 1) + "-" + two(date.getDate()) +
    " " + two(date.getHours()) + ":" + two(date.getMinutes());
}

export function banner() {
  return el("div", { class: "banner", hidden: true });
}

export function setBanner(node, message) {
  if (!message) { node.textContent = ""; node.hidden = true; return; }
  node.textContent = String(message);
  node.hidden = false;
}

export function field(labelText, inputNode) {
  return el("label", { class: "field" },
    el("span", { class: "field-label", text: labelText }), inputNode);
}

export function input(props) {
  return el("input", Object.assign({ class: "input", type: "text" }, props || {}));
}
