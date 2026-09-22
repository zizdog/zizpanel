#!/usr/bin/env node
// ============================================================================
//  check-js-undeclared.mjs —— 抓"赋值了但全文件没有声明"的标识符。
//
//  为什么需要它：acorn 只查语法，不查名字有没有定义。历史上清理"死代码"时
//  删掉过 `let cmCorePromise = null;`，而下面 4 处引用还在 —— 语法完全合法、
//  门禁全绿，浏览器里却是 `Can't find variable: cmCorePromise`，编辑器直接打不开。
//
//  判据（保守，宁少报不误报）：
//    · 收集文件内所有声明：var/let/const、function/class、函数参数、catch 参数、import；
//    · 收集所有"裸标识符赋值"：`x = …`、`x++`、`for (x of …)`、`for (x in …)`；
//    · 赋值了但文件内没有任何声明、也不在已知全局白名单里 → 报错。
//  仅扫第一方资源；vendor/ 下的第三方库不扫（它们的全局风格不适用这条判据）。
//
//  用法：node tools/check-js-undeclared.mjs <目录或文件> [更多…]
//  退出码 0 通过，1 有发现，2 用法/环境不对。
// ============================================================================
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { join, basename } from 'node:path';

let acorn;
try {
  acorn = await import('acorn');
} catch {
  console.error('（未安装 acorn，跳过：npm install）');
  process.exit(0);
}

// 浏览器/宿主提供的全局名（赋值给它们不算"没声明"）。
const GLOBALS = new Set([
  'window', 'document', 'location', 'navigator', 'history', 'screen', 'console',
  'localStorage', 'sessionStorage', 'indexedDB', 'crypto', 'performance',
  'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'queueMicrotask',
  'requestAnimationFrame', 'cancelAnimationFrame', 'fetch', 'URL', 'URLSearchParams',
  'Blob', 'File', 'FileReader', 'FormData', 'Headers', 'Request', 'Response',
  'EventSource', 'WebSocket', 'AbortController', 'TextEncoder', 'TextDecoder',
  'alert', 'confirm', 'prompt', 'structuredClone', 'CustomEvent', 'Event',
  'MutationObserver', 'IntersectionObserver', 'ResizeObserver', 'Notification',
  'matchMedia', 'getComputedStyle', 'scrollTo', 'open', 'close', 'postMessage',
  // vendor 库挂到 window 上的名字（由 assets/vendor 提供）
  'CodeMirror', 'Plyr', 'hljs',
]);

const args = process.argv.slice(2);
if (args.length === 0) {
  console.error('用法：node tools/check-js-undeclared.mjs <目录或文件> [更多…]');
  process.exit(2);
}

function walk(p, out) {
  // 目录不存在就跳过：调用方会把"可能没有这个模块"的路径一起传进来
  // （模块下线后 Makefile 没同步，整个 make check 会死在这里）。
  if (!existsSync(p)) return;
  const st = statSync(p);
  if (st.isDirectory()) {
    if (basename(p) === 'vendor' || basename(p) === 'node_modules') return;
    for (const e of readdirSync(p)) walk(join(p, e), out);
  } else if (p.endsWith('.js') || p.endsWith('.mjs')) {
    out.push(p);
  }
}

// 只取模式里的标识符名（解构、默认值都要覆盖到）。
function patternNames(node, into) {
  if (!node) return;
  switch (node.type) {
    case 'Identifier': into.add(node.name); break;
    case 'ObjectPattern':
      for (const p of node.properties) {
        if (p.type === 'RestElement') patternNames(p.argument, into);
        else patternNames(p.value, into);
      }
      break;
    case 'ArrayPattern':
      for (const el of node.elements) patternNames(el, into);
      break;
    case 'AssignmentPattern': patternNames(node.left, into); break;
    case 'RestElement': patternNames(node.argument, into); break;
  }
}

function collect(ast) {
  const declared = new Set();
  const assigned = new Map(); // name -> 行号

  const visit = (node, parent) => {
    if (!node || typeof node.type !== 'string') return;
    switch (node.type) {
      case 'VariableDeclarator': patternNames(node.id, declared); break;
      case 'FunctionDeclaration':
      case 'FunctionExpression':
      case 'ArrowFunctionExpression':
        if (node.id) declared.add(node.id.name);
        for (const p of node.params) patternNames(p, declared);
        break;
      case 'ClassDeclaration':
      case 'ClassExpression':
        if (node.id) declared.add(node.id.name);
        break;
      case 'CatchClause': patternNames(node.param, declared); break;
      case 'ImportDeclaration':
        for (const s of node.specifiers) declared.add(s.local.name);
        break;
      case 'AssignmentExpression':
        if (node.left.type === 'Identifier') {
          if (!assigned.has(node.left.name)) assigned.set(node.left.name, node.loc.start.line);
        }
        break;
      case 'UpdateExpression':
        if (node.argument.type === 'Identifier') {
          if (!assigned.has(node.argument.name)) assigned.set(node.argument.name, node.loc.start.line);
        }
        break;
      case 'ForOfStatement':
      case 'ForInStatement':
        if (node.left.type === 'Identifier') {
          if (!assigned.has(node.left.name)) assigned.set(node.left.name, node.loc.start.line);
        }
        break;
    }
    for (const k of Object.keys(node)) {
      if (k === 'loc' || k === 'start' || k === 'end') continue;
      const v = node[k];
      if (Array.isArray(v)) for (const c of v) visit(c, node);
      else if (v && typeof v.type === 'string') visit(v, node);
    }
  };
  visit(ast, null);
  return { declared, assigned };
}

const files = [];
for (const a of args) walk(a, files);

let bad = 0;
for (const f of files) {
  let ast;
  try {
    ast = acorn.parse(readFileSync(f, 'utf8'), { ecmaVersion: 'latest', sourceType: 'module', locations: true });
  } catch (e) {
    console.error(`✗ ${f}: 语法错误（先跑 check-js-syntax.mjs）: ${e.message}`);
    bad++;
    continue;
  }
  const { declared, assigned } = collect(ast);
  for (const [name, line] of assigned) {
    if (declared.has(name) || GLOBALS.has(name)) continue;
    console.error(`✗ ${f}:${line}  ${name} 被赋值但全文件没有声明（浏览器会报 Can't find variable）`);
    bad++;
  }
}
if (bad === 0) {
  console.log(`✓ JS 未声明赋值检查通过（${files.length} 个文件）`);
  process.exit(0);
}
console.error(`\n共 ${bad} 处。补声明，或把该名字加进 GLOBALS 白名单（要在注释里写清它从哪来）。`);
process.exit(1);
