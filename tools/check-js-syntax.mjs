// check-js-syntax.mjs —— 用**真正的 ES 解析器**校验内嵌前端的语法。
//
// 为什么不能只用 `node --check`：
// 实测踩过。两个文件里各少了一个 `}`（对象字面量写成
// `{ style: { fontWeight: '600', text: p.name })`），
// `node --check` **返回 0 说没问题**，而 Chrome 的模块解析器直接拒绝执行，
// 表现是整个面板白屏、`pageerror: Unexpected token ')'`、控制台连行号都不给。
// 定位花了很久 —— 因为报错信息完全指不到出问题的文件。
//
// acorn 会在毫秒级给出**精确到行列**的位置，所以把它接进 `make check`：
// 这样"前端语法坏了"永远不会再以"白屏 + 无行号"的形式跑到真机上。
//
// 用法：node tools/check-js-syntax.mjs [目录...]
// 未安装 acorn 时**跳过并返回 0**：不因为一个可选工具缺失就阻断检查，
// 但会明确打印提示（CI/开发机上装了就有门禁）。

import fs from 'fs';
import path from 'path';

const dirs = process.argv.slice(2);
if (dirs.length === 0) dirs.push('internal/web/assets/js');

let acorn;
try {
  acorn = await import('acorn');
} catch {
  console.log('（未安装 acorn，跳过 JS 语法校验：npm install）');
  process.exit(0);
}

function collect(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) collect(p, out);
    else if (e.name.endsWith('.js')) out.push(p);
  }
  return out;
}

const files = [];
for (const d of dirs) {
  if (!fs.existsSync(d)) continue;
  const st = fs.statSync(d);
  if (st.isDirectory()) collect(d, files);
  else files.push(d);
}

let bad = 0;
for (const f of files.sort()) {
  const src = fs.readFileSync(f, 'utf8');
  try {
    acorn.parse(src, { ecmaVersion: 'latest', sourceType: 'module', locations: true });
  } catch (e) {
    bad++;
    console.error('✗ %s\n    %s', f, e.message);
    if (e.loc) {
      const lines = src.split('\n');
      const w = String(e.loc.line + 2).length;
      for (let k = Math.max(0, e.loc.line - 3); k < Math.min(lines.length, e.loc.line + 2); k++) {
        console.error('    %s%s| %s', k + 1 === e.loc.line ? '>>' : '  ',
          String(k + 1).padStart(w), lines[k]);
      }
    }
  }
}

if (bad > 0) {
  console.error('\n%d 个文件有语法错误（这些文件会让整个面板白屏，必须修）', bad);
  process.exit(1);
}
console.log('✓ JS 语法 OK（%d 个文件，acorn 校验）', files.length);
