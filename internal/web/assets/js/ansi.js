// ansi.js —— 最小 ANSI 终端渲染器。
//
// 为什么自己写而不是引入 xterm.js：
//   面板要长期稳定、零构建、零外部依赖。xterm.js 体积大且需要打包链路。
//   终端功能的核心需求是"能跑命令、看清输出、颜色正确"，
//   用字符网格 + 常用 ANSI 指令解析即可满足，约 400 行可控代码。
//
// 支持的指令（覆盖 shell / git / ls / brew 等日常命令的输出）：
//   - SGR 颜色与样式（30-37/90-97 前景，40-47/100-107 背景，1/2/3/4/7/9 样式，38;5;N 与 38;2;R;G;B）
//   - 光标移动：CUU/CUD/CUF/CUB（A/B/C/D）、CHA（G）、CUP（H/f）、CNL/CPL（E/F）
//   - 擦除：ED（J：0/1/2/3）、EL（K：0/1/2）
//   - 保存/恢复光标（s/u）、插入行（L）、删除行（M）、删除字符（P）
//   - 回车、换行、退格、制表符
// 未支持（可接受）：滚动区域（DECSTBM）、备用屏幕的完整语义、鼠标上报。

// 默认调色板（与主流终端接近，深色背景）
const PALETTE = [
  '#1c1f26', '#e06c75', '#98c379', '#e5c07b',
  '#61afef', '#c678dd', '#56b6c2', '#abb2bf',
];
const PALETTE_BRIGHT = [
  '#5c6370', '#ff7b86', '#b4e391', '#ffd68a',
  '#82c7ff', '#e5a0ff', '#7fdfe8', '#ffffff',
];

function colorFor(idx, bright) {
  if (idx < 0) return null;
  if (idx < 8) return bright ? PALETTE_BRIGHT[idx] : PALETTE[idx];
  if (idx >= 8 && idx < 16) return PALETTE[idx - 8 + 0] && bright ? PALETTE_BRIGHT[idx - 8] : PALETTE[idx - 8];
  if (idx >= 16 && idx <= 231) {
    // 216 色立方
    const n = idx - 16;
    const r = Math.floor(n / 36) * 51;
    const g = Math.floor((n % 36) / 6) * 51;
    const b = (n % 6) * 51;
    return `rgb(${r},${g},${b})`;
  }
  if (idx >= 232 && idx <= 255) {
    const v = (idx - 232) * 10 + 8;
    return `rgb(${v},${v},${v})`;
  }
  return null;
}

/** Cell 是一个字符及其样式。 */
function newCell(ch = ' ', fg = null, bg = null, attrs = 0) {
  return { ch, fg, bg, attrs };
}

// 样式位
export const ATTR_BOLD = 1;
export const ATTR_DIM = 2;
export const ATTR_ITALIC = 4;
export const ATTR_UNDERLINE = 8;
export const ATTR_REVERSE = 16;
export const ATTR_STRIKE = 32;

/**
 * Terminal 维护一个 cols×rows 的字符网格，并提供 ANSI 解析与增删改查。
 *
 * 使用方式：把 shell 输出喂给 write()，然后调用 render() 拿到 DOM。
 */
export class Terminal {
  constructor(cols = 120, rows = 32) {
    this.cols = cols;
    this.rows = rows;
    this.reset();
  }

  reset() {
    this.grid = [];
    for (let r = 0; r < this.rows; r++) this.grid.push(this.blankRow());
    this.x = 0;
    this.y = 0;
    this.fg = null;
    this.bg = null;
    this.attrs = 0;
    this.savedX = 0;
    this.savedY = 0;
    // 解析状态：用于跨 write() 调用拼接不完整的转义序列
    this.pending = '';
  }

  blankRow() {
    const row = [];
    for (let c = 0; c < this.cols; c++) row.push(newCell());
    return row;
  }

  resize(cols, rows) {
    cols = Math.max(20, Math.min(400, cols | 0));
    rows = Math.max(5, Math.min(200, rows | 0));
    if (cols === this.cols && rows === this.rows) return;
    const next = [];
    for (let r = 0; r < rows; r++) {
      const row = this.blankRow();
      const rowLen = next.length;
      if (r < this.grid.length) {
        for (let c = 0; c < Math.min(cols, this.cols); c++) row[c] = this.grid[r][c];
      }
      next.push(row);
      void rowLen;
    }
    this.grid = next;
    this.cols = cols;
    this.rows = rows;
    // 修正光标位置
    this.x = Math.min(this.x, cols - 1);
    this.y = Math.min(this.y, rows - 1);
    // 重新按新宽度分配空行（上面循环里 blankRow 用的是旧 cols）
    for (let r = 0; r < rows; r++) {
      while (this.grid[r].length < cols) this.grid[r].push(newCell());
      this.grid[r].length = cols;
    }
  }

  /** write 解析并写入一段数据（可能含 ANSI 转义）。 */
  write(data) {
    const text = this.pending + data;
    this.pending = '';
    let i = 0;
    while (i < text.length) {
      const ch = text[i];
      if (ch === '\x1b') {
        const consumed = this.parseEscape(text, i);
        if (consumed === -1) {
          // 序列不完整，留到下次
          this.pending = text.slice(i);
          return;
        }
        i += consumed;
        continue;
      }
      if (ch === '\n') { this.newline(); i++; continue; }
      if (ch === '\r') { this.x = 0; i++; continue; }
      if (ch === '\b') { if (this.x > 0) this.x--; i++; continue; }
      if (ch === '\t') {
        const next = Math.min(this.cols - 1, (Math.floor(this.x / 8) + 1) * 8);
        this.x = next;
        i++;
        continue;
      }
      if (ch === '\x07') { i++; continue; } // 响铃：忽略
      if (ch === '\x00') { i++; continue; }
      this.putChar(ch);
      i++;
    }
  }

  /** putChar 写入一个可见字符。 */
  putChar(ch) {
    if (ch === '\u0000') return;
    if (this.x >= this.cols) {
      // 自动换行
      this.x = 0;
      this.newline();
    }
    const row = this.grid[this.y];
    if (row && this.x < row.length) {
      row[this.x] = newCell(ch, this.fg, this.bg, this.attrs);
    }
    this.x++;
  }

  newline() {
    this.y++;
    if (this.y >= this.rows) this.scrollUp();
  }

  scrollUp() {
    this.grid.shift();
    this.grid.push(this.blankRow());
    this.y = this.rows - 1;
  }

  /**
   * parseEscape 解析从 index 开始的转义序列。
   * 返回消费的字符数；-1 表示序列不完整（需要等更多数据）。
   */
  parseEscape(text, index) {
    const next = text[index + 1];
    if (next === undefined) return -1;

    // CSI：ESC [ params letter
    if (next === '[') {
      let j = index + 2;
      while (j < text.length) {
        const c = text[j];
        if (c >= '\x40' && c <= '\x7e') {
          this.applyCSI(text.slice(index + 2, j), c);
          return j - index + 1;
        }
        j++;
      }
      return -1;
    }
    // OSC：ESC ] ... BEL 或 ESC \
    if (next === ']') {
      let j = index + 2;
      while (j < text.length) {
        if (text[j] === '\x07') return j - index + 1;
        if (text[j] === '\x1b' && text[j + 1] === '\\') return j - index + 2;
        j++;
      }
      return -1;
    }
    // 单字符序列
    switch (next) {
      case '7': this.savedX = this.x; this.savedY = this.y; return 2;
      case '8': this.x = this.savedX; this.y = this.savedY; return 2;
      case 'D': this.newline(); return 2;         // IND
      case 'M': this.reverseIndex(); return 2;    // RI
      case 'E': this.x = 0; this.newline(); return 2;
      case 'c': this.reset(); return 2;           // RIS
      case '(': case ')':
        // 字符集选择：跳过两个字符
        return text[index + 2] === undefined ? -1 : 3;
      default:
        return 2; // 其它单字符序列直接忽略
    }
  }

  reverseIndex() {
    if (this.y > 0) {
      this.y--;
    } else {
      this.grid.pop();
      this.grid.unshift(this.blankRow());
    }
  }

  applyCSI(paramStr, final) {
    // 参数可能形如 "1;31" 或 "?25" 或空
    const priv = paramStr.startsWith('?') ? '?' : '';
    const body = priv ? paramStr.slice(1) : paramStr;
    const params = body === '' ? [] : body.split(';').map((s) => (s === '' ? 0 : parseInt(s, 10) || 0));
    const p0 = params.length ? params[0] : 0;

    switch (final) {
      case 'm': this.applySGR(params); return;
      case 'A': this.y = Math.max(0, this.y - (p0 || 1)); return;
      case 'B': this.y = Math.min(this.rows - 1, this.y + (p0 || 1)); return;
      case 'C': this.x = Math.min(this.cols - 1, this.x + (p0 || 1)); return;
      case 'D': this.x = Math.max(0, this.x - (p0 || 1)); return;
      case 'E': this.y = Math.min(this.rows - 1, this.y + (p0 || 1)); this.x = 0; return;
      case 'F': this.y = Math.max(0, this.y - (p0 || 1)); this.x = 0; return;
      case 'G': this.x = Math.max(0, Math.min(this.cols - 1, (p0 || 1) - 1)); return;
      case 'd': this.y = Math.max(0, Math.min(this.rows - 1, (p0 || 1) - 1)); return;
      case 'H': case 'f': {
        const row = (params[0] || 1) - 1;
        const col = (params[1] || 1) - 1;
        this.y = Math.max(0, Math.min(this.rows - 1, row));
        this.x = Math.max(0, Math.min(this.cols - 1, col));
        return;
      }
      case 'J': this.eraseDisplay(p0); return;
      case 'K': this.eraseLine(p0); return;
      case 'L': this.insertLines(p0 || 1); return;
      case 'M': this.deleteLines(p0 || 1); return;
      case 'P': this.deleteChars(p0 || 1); return;
      case 'X': this.eraseChars(p0 || 1); return;
      case '@': this.insertChars(p0 || 1); return;
      case 's': this.savedX = this.x; this.savedY = this.y; return;
      case 'u': this.x = this.savedX; this.y = this.savedY; return;
      case 'h': case 'l': return; // 模式设置（光标可见性等）：忽略
      default: return;
    }
  }

  applySGR(params) {
    if (!params.length) { this.fg = null; this.bg = null; this.attrs = 0; return; }
    for (let i = 0; i < params.length; i++) {
      const p = params[i];
      if (p === 0) { this.fg = null; this.bg = null; this.attrs = 0; }
      else if (p === 1) this.attrs |= ATTR_BOLD;
      else if (p === 2) this.attrs |= ATTR_DIM;
      else if (p === 3) this.attrs |= ATTR_ITALIC;
      else if (p === 4) this.attrs |= ATTR_UNDERLINE;
      else if (p === 7) this.attrs |= ATTR_REVERSE;
      else if (p === 9) this.attrs |= ATTR_STRIKE;
      else if (p === 22) this.attrs &= ~(ATTR_BOLD | ATTR_DIM);
      else if (p === 23) this.attrs &= ~ATTR_ITALIC;
      else if (p === 24) this.attrs &= ~ATTR_UNDERLINE;
      else if (p === 27) this.attrs &= ~ATTR_REVERSE;
      else if (p === 29) this.attrs &= ~ATTR_STRIKE;
      else if (p >= 30 && p <= 37) this.fg = colorFor(p - 30, false);
      else if (p === 38) {
        // 扩展前景色：38;5;N 或 38;2;R;G;B
        const mode = params[i + 1];
        if (mode === 5) { this.fg = colorFor(params[i + 2], true); i += 2; }
        else if (mode === 2) {
          this.fg = `rgb(${params[i + 2] || 0},${params[i + 3] || 0},${params[i + 4] || 0})`;
          i += 4;
        }
      } else if (p === 39) this.fg = null;
      else if (p >= 40 && p <= 47) this.bg = colorFor(p - 40, false);
      else if (p === 48) {
        const mode = params[i + 1];
        if (mode === 5) { this.bg = colorFor(params[i + 2], true); i += 2; }
        else if (mode === 2) {
          this.bg = `rgb(${params[i + 2] || 0},${params[i + 3] || 0},${params[i + 4] || 0})`;
          i += 4;
        }
      } else if (p === 49) this.bg = null;
      else if (p >= 90 && p <= 97) this.fg = colorFor(p - 90, true);
      else if (p >= 100 && p <= 107) this.bg = colorFor(p - 100, true);
    }
  }

  eraseDisplay(mode) {
    if (mode === 2 || mode === 3) {
      for (let r = 0; r < this.rows; r++) this.grid[r] = this.blankRow();
      if (mode === 2) { this.x = 0; this.y = 0; }
      return;
    }
    if (mode === 1) {
      for (let r = 0; r < this.y; r++) this.grid[r] = this.blankRow();
      for (let c = 0; c <= this.x && c < this.cols; c++) this.grid[this.y][c] = newCell();
      return;
    }
    // mode 0：光标到行尾
    for (let c = this.x; c < this.cols; c++) this.grid[this.y][c] = newCell();
    for (let r = this.y + 1; r < this.rows; r++) this.grid[r] = this.blankRow();
  }

  eraseLine(mode) {
    const row = this.grid[this.y];
    if (!row) return;
    if (mode === 2) {
      this.grid[this.y] = this.blankRow();
    } else if (mode === 1) {
      for (let c = 0; c <= this.x && c < this.cols; c++) row[c] = newCell();
    } else {
      for (let c = this.x; c < this.cols; c++) row[c] = newCell();
    }
  }

  insertLines(n) {
    for (let i = 0; i < n; i++) {
      this.grid.splice(this.y, 0, this.blankRow());
      this.grid.pop();
    }
  }

  deleteLines(n) {
    for (let i = 0; i < n; i++) {
      this.grid.splice(this.y, 1);
      this.grid.push(this.blankRow());
    }
  }

  deleteChars(n) {
    const row = this.grid[this.y];
    if (!row) return;
    row.splice(this.x, n);
    while (row.length < this.cols) row.push(newCell());
  }

  eraseChars(n) {
    const row = this.grid[this.y];
    if (!row) return;
    for (let i = 0; i < n && this.x + i < this.cols; i++) row[this.x + i] = newCell();
  }

  insertChars(n) {
    const row = this.grid[this.y];
    if (!row) return;
    for (let i = 0; i < n; i++) row.splice(this.x, 0, newCell());
    row.length = this.cols;
  }

  /**
   * render 把网格渲染成 DOM。
   *
   * 性能策略：逐行比较上一次的行内容，只有变化的行才重建 DOM。
   * 终端每秒可能刷新多次，全量重建会在长会话里明显卡顿。
   */
  render(container) {
    while (container.childElementCount < this.rows) {
      const line = document.createElement('div');
      line.className = 'term-line';
      container.appendChild(line);
    }
    while (container.childElementCount > this.rows) {
      container.removeChild(container.lastChild);
    }

    for (let r = 0; r < this.rows; r++) {
      const row = this.grid[r];
      const line = container.children[r];
      // 生成该行的"签名"：字符 + 样式，用于判断是否需要重绘
      let sig = '';
      for (let c = 0; c < this.cols; c++) {
        const cell = row[c];
        sig += cell.ch + (cell.fg || '') + (cell.bg || '') + cell.attrs;
      }
      if (line.dataset.sig === sig) continue;
      line.dataset.sig = sig;

      // 按连续相同样式分组，减少 span 数量
      const frag = document.createDocumentFragment();
      let runStart = 0;
      for (let c = 1; c <= this.cols; c++) {
        const prev = row[c - 1];
        const cur = c < this.cols ? row[c] : null;
        const same = cur && cur.fg === prev.fg && cur.bg === prev.bg && cur.attrs === prev.attrs;
        if (!same) {
          const text = this.sliceText(row, runStart, c);
          if (text.trim() === '' && !prev.fg && !prev.bg && !prev.attrs) {
            // 纯空白的默认样式：直接放文本节点，省去 span
            frag.appendChild(document.createTextNode(text));
          } else {
            const span = document.createElement('span');
            span.textContent = text;
            if (prev.fg) span.style.color = prev.fg;
            if (prev.bg) span.style.background = prev.bg;
            if (prev.attrs & ATTR_BOLD) span.style.fontWeight = '700';
            if (prev.attrs & ATTR_DIM) span.style.opacity = '0.65';
            if (prev.attrs & ATTR_ITALIC) span.style.fontStyle = 'italic';
            if (prev.attrs & ATTR_UNDERLINE) span.style.textDecoration = 'underline';
            if (prev.attrs & ATTR_STRIKE) span.style.textDecoration = 'line-through';
            if (prev.attrs & ATTR_REVERSE) {
              span.style.color = prev.bg || '#1c1f26';
              span.style.background = prev.fg || '#e6e9f0';
            }
            frag.appendChild(span);
          }
          runStart = c;
        }
      }
      line.replaceChildren(frag);
    }
  }

  sliceText(row, from, to) {
    let s = '';
    for (let i = from; i < to; i++) s += row[i].ch;
    return s;
  }

  /** plainText 返回纯文本内容（用于复制）。 */
  plainText() {
    const lines = [];
    for (let r = 0; r < this.rows; r++) {
      let s = '';
      for (let c = 0; c < this.cols; c++) s += this.grid[r][c].ch;
      lines.push(s.replace(/\s+$/, ''));
    }
    while (lines.length && lines[lines.length - 1] === '') lines.pop();
    return lines.join('\n');
  }
}
