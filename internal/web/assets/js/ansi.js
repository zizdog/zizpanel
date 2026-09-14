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
//   - 回滚历史（滚出屏幕顶部的行保留下来，见 MAX_HISTORY）
//   - 块状光标（DECTCEM：ESC[?25h/l 可见性）
//   - 备用屏幕（ESC[?1049h/l，别名 ?47/?1047）
// 未支持（可接受）：滚动区域（DECSTBM）、鼠标上报。

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

// 回滚历史上限。2000 行是"够翻"和"不把浏览器内存吃光"的折中：
// 每行是 cols 个 cell 对象，而且每行在 DOM 里至少一个节点，
// 设成无上限的话，跑一晚上日志式输出的命令就会把标签页拖死。
const MAX_HISTORY = 2000;

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
    this.grid = this.freshGrid();
    this.x = 0;
    this.y = 0;
    this.fg = null;
    this.bg = null;
    this.attrs = 0;
    this.savedX = 0;
    this.savedY = 0;
    this.cursorVisible = true;
    // 回滚历史：元素是 { seq, row }。
    // seq 单调递增，render() 用它判断"哪些历史行已经进了 DOM"，
    // 这样既不会重复渲染，上限裁剪时也知道该删掉哪些 DOM 节点。
    this.history = [];
    this.histSeq = 0;
    this.histDirty = true;
    // 备用屏幕状态（见 applyMode 里的 ?1049 处理）
    this.altScreen = false;
    this.mainSaved = null;
    this.altGrid = null;
    // 解析状态：用于跨 write() 调用拼接不完整的转义序列
    this.pending = '';
  }

  freshGrid() {
    const g = [];
    for (let r = 0; r < this.rows; r++) g.push(this.blankRow());
    return g;
  }

  blankRow(cols = this.cols) {
    const row = [];
    for (let c = 0; c < cols; c++) row.push(newCell());
    return row;
  }

  /** fitRow 把一行按给定列宽裁剪/补空。 */
  fitRow(row, cols) {
    while (row.length < cols) row.push(newCell());
    row.length = cols;
    return row;
  }

  /**
   * fitGrid 把一整块网格调成 cols×rows。
   * 备用屏切换期间窗口可能改过尺寸，被切走的那块屏会停在旧列宽/旧行数上；
   * 不归一化的话渲染时 row[c-1] 会是 undefined（真的抛过 TypeError）。
   */
  fitGrid(grid, cols, rows) {
    while (grid.length < rows) grid.push(this.blankRow(cols));
    grid.length = rows;
    for (const row of grid) this.fitRow(row, cols);
    return grid;
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
    if (cols !== this.cols) {
      // 历史行是按旧列宽渲染的。这里直接截断/补空，而不是重排换行 ——
      // 只要有一行比容器宽，整块屏幕就会挤出**横向**滚动条，
      // 而横向滚动会让"定宽 18px 行 + 等宽字体"的列对齐彻底失效。
      for (const entry of this.history) this.fitRow(entry.row, cols);
      this.histDirty = true; // 历史行要按新宽度重画
    }
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
    const dropped = this.grid.shift();
    // 备用屏里的滚动**绝不能**进回滚历史：vim/less 每重画一帧就可能滚一次，
    // 若记进历史，用户退出后往上滚看到的全是重画帧垃圾，真正的历史反被冲掉。
    if (dropped && !this.altScreen) this.pushHistory(dropped);
    this.grid.push(this.blankRow());
    this.y = this.rows - 1;
  }

  pushHistory(row) {
    this.history.push({ seq: ++this.histSeq, row });
    // 超上限丢最老的；对应 DOM 由 renderHistory() 按 seq 补齐/裁剪
    if (this.history.length > MAX_HISTORY) this.history.shift();
  }

  clearHistory() {
    this.history = [];
    this.histDirty = true; // 让 render() 把历史 DOM 也清掉
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
      case 'h': this.applyMode(params, priv, true); return;
      case 'l': this.applyMode(params, priv, false); return;
      default: return;
    }
  }

  /**
   * applyMode 处理 DECSET/DECRST（ESC[?N h/l）。只实现真正影响渲染的模式。
   */
  applyMode(params, priv, on) {
    // 不带 '?' 的是 ANSI 模式（插入模式、行回绕等），与渲染无关，忽略
    if (priv !== '?') return;
    for (const p of params) {
      if (p === 25) {
        // DECTCEM：程序可以隐藏光标（vim/less 会自己画）
        this.cursorVisible = on;
      } else if (p === 47 || p === 1047 || p === 1049) {
        // 47/1047/1049 都是"备用屏幕"，差别只在清屏时机：
        //   47   ：切换，但保留备用屏上一次留下的内容
        //   1047 ：进入时清空，退出时也清空
        //   1049 ：进入时清空 + 保存光标位置，退出时恢复光标（vim/less 用的就是它）
        if (on) this.enterAltScreen(p !== 47);
        else this.exitAltScreen(p === 1047);
      }
    }
  }

  enterAltScreen(clear) {
    if (this.altScreen) return;
    // 保存主屏的全部可恢复状态：网格、光标、回滚历史、DECSC 保存位。
    // 网格是引用保存 —— 进入备用屏后主屏网格不会再被写，所以退出时原样就是"原样"。
    this.mainSaved = {
      grid: this.grid,
      x: this.x,
      y: this.y,
      history: this.history,
      savedX: this.savedX,
      savedY: this.savedY,
    };
    this.altScreen = true;
    if (clear || !this.altGrid) this.altGrid = this.freshGrid();
    // 复用上一次的备用屏内容时也要归一化：中间可能改过窗口宽度
    else this.fitGrid(this.altGrid, this.cols, this.rows);
    this.grid = this.altGrid;
    // 备用屏自己的历史恒为空（scrollUp 在备用屏下不入历史）
    this.history = [];
    this.x = 0;
    this.y = 0;
    this.histDirty = true; // 让 render() 清掉屏幕上主屏的历史行
  }

  exitAltScreen(clearAlt) {
    if (!this.altScreen || !this.mainSaved) return;
    // 47 的语义要求备用屏内容留到下次进入；1047l 要求退出前清空
    this.altGrid = clearAlt ? null : this.grid;
    // 主屏在备用屏期间不会被 resize 动到，所以要按**当前**尺寸归一化后再还原
    this.grid = this.fitGrid(this.mainSaved.grid, this.cols, this.rows);
    this.x = Math.min(this.mainSaved.x, this.cols - 1);
    this.y = Math.min(this.mainSaved.y, this.rows - 1);
    this.savedX = this.mainSaved.savedX;
    this.savedY = this.mainSaved.savedY;
    this.history = this.mainSaved.history;
    for (const entry of this.history) this.fitRow(entry.row, this.cols);
    this.mainSaved = null;
    this.altScreen = false;
    this.histDirty = true;
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
      // 终端惯例：3J（erase saved lines）清回滚历史，2J 只清屏、不动历史。
      if (mode === 3) this.clearHistory();
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
   * DOM 结构（都由本函数保证存在）：
   *   .term-screen
   *     .term-history   <- 回滚历史，行数 ≤ MAX_HISTORY
   *     .term-rows      <- 恰好 rows 个 .term-line，与 grid 行按下标一一对应
   *
   * 分成两层的原因：滚出屏幕的行只要往 .term-history 里追加即可，
   * "当前屏 32 行"的下标关系仍然只由 .term-rows 维护，
   * 不会因为历史变长而算错。
   *
   * 性能策略：逐行比较上一次的行内容，只有变化的行才重建 DOM。
   * 终端每秒可能刷新多次，全量重建会在长会话里明显卡顿。
   */
  render(container) {
    const { histEl, rowsEl } = this.ensureStructure(container);
    this.renderHistory(histEl);

    while (rowsEl.childElementCount < this.rows) rowsEl.appendChild(this.newLineEl());
    while (rowsEl.childElementCount > this.rows) rowsEl.removeChild(rowsEl.lastElementChild);

    for (let r = 0; r < this.rows; r++) {
      const row = this.grid[r];
      const line = rowsEl.children[r];
      // 生成该行的"签名"：字符 + 样式，用于判断是否需要重绘
      let sig = '';
      for (let c = 0; c < this.cols; c++) {
        const cell = row[c];
        sig += cell.ch + (cell.fg || '') + (cell.bg || '') + cell.attrs;
      }
      // 光标也进签名：光标移动时单元格内容没变，但这一行必须重画，
      // 否则块状光标会停在原地不动。
      if (this.cursorVisible && r === this.y) sig += '|C' + this.x;
      if (line.dataset.sig === sig) continue;
      line.dataset.sig = sig;
      this.paintLine(line, row, r);
    }
  }

  /**
   * ensureStructure 保证容器里有 .term-history + .term-rows 两层骨架。
   * 容器可能被外部 clear() 过（重连时会重建终端的 DOM），所以每次都核对。
   */
  ensureStructure(container) {
    let histEl = container.firstElementChild;
    let rowsEl = histEl ? histEl.nextElementSibling : null;
    if (!histEl || !histEl.classList.contains('term-history') ||
        !rowsEl || !rowsEl.classList.contains('term-rows')) {
      while (container.firstChild) container.removeChild(container.firstChild);
      histEl = document.createElement('div');
      histEl.className = 'term-history';
      rowsEl = document.createElement('div');
      rowsEl.className = 'term-rows';
      container.appendChild(histEl);
      container.appendChild(rowsEl);
      this.histDirty = true; // 骨架是新的，历史行得整体重画
    }
    return { histEl, rowsEl };
  }

  newLineEl() {
    const line = document.createElement('div');
    line.className = 'term-line';
    return line;
  }

  /**
   * renderHistory 让 .term-history 的 DOM 与 this.history 对齐。
   *
   * 正常情况每次只补 1 行（刚滚出去的那行），所以是 O(1)；
   * 只有切换备用屏/改宽度/清屏这类整体换屏才会全量重画。
   */
  renderHistory(histEl) {
    if (this.histDirty) {
      while (histEl.firstChild) histEl.removeChild(histEl.firstChild);
      this.histDirty = false;
    }
    // 超出上限被丢掉的最老行，DOM 里也要删掉
    const firstSeq = this.history.length ? this.history[0].seq : 0;
    while (histEl.firstElementChild && Number(histEl.firstElementChild.dataset.seq) < firstSeq) {
      histEl.removeChild(histEl.firstElementChild);
    }
    while (histEl.childElementCount > this.history.length) {
      histEl.removeChild(histEl.lastElementChild);
    }
    // 快路径：DOM 已经和历史一一对应
    if (histEl.childElementCount === this.history.length && this.history.length) {
      const lastDomSeq = Number(histEl.lastElementChild.dataset.seq);
      if (lastDomSeq === this.history[this.history.length - 1].seq) return;
    }

    let lastSeq = histEl.lastElementChild ? Number(histEl.lastElementChild.dataset.seq) : 0;
    const frag = document.createDocumentFragment();
    for (const entry of this.history) {
      if (entry.seq <= lastSeq) continue;
      const line = this.newLineEl();
      line.dataset.seq = String(entry.seq);
      this.paintLine(line, entry.row, -1); // 历史行不画光标
      frag.appendChild(line);
    }
    histEl.appendChild(frag);
  }

  /**
   * paintLine 把一行 cell 画进 .term-line。
   * cursorRowIndex 是这一行在网格里的行号；历史行传 -1（光标只在当前屏）。
   */
  paintLine(line, row, cursorRowIndex) {
    const cx = this.cursorVisible && cursorRowIndex === this.y ? this.x : -1;

    // 按连续相同样式分组，减少 span 数量
    const frag = document.createDocumentFragment();
    let runStart = 0;
    for (let c = 1; c <= this.cols; c++) {
      const prev = row[c - 1];
      const cur = c < this.cols ? row[c] : null;
      // 光标所在的那一格必须单独成段，否则它会被并进普通 span，反色块画不出来
      const same = cur && cur.fg === prev.fg && cur.bg === prev.bg && cur.attrs === prev.attrs
        && c !== cx && c !== cx + 1;
      if (!same) {
        const text = this.sliceText(row, runStart, c);
        const isCursor = runStart === cx;
        if (text.trim() === '' && !prev.fg && !prev.bg && !prev.attrs && !isCursor) {
          // 纯空白的默认样式：直接放文本节点，省去 span
          frag.appendChild(document.createTextNode(text));
        } else {
          const span = document.createElement('span');
          span.textContent = text;
          let fg = prev.fg;
          let bg = prev.bg;
          if (prev.attrs & ATTR_REVERSE) {
            const swap = fg;
            fg = bg || '#1c1f26';
            bg = swap || '#e6e9f0';
          }
          if (fg) span.style.color = fg;
          if (bg) span.style.background = bg;
          if (prev.attrs & ATTR_BOLD) span.style.fontWeight = '700';
          if (prev.attrs & ATTR_DIM) span.style.opacity = '0.65';
          if (prev.attrs & ATTR_ITALIC) span.style.fontStyle = 'italic';
          if (prev.attrs & ATTR_UNDERLINE) span.style.textDecoration = 'underline';
          if (prev.attrs & ATTR_STRIKE) span.style.textDecoration = 'line-through';
          if (isCursor) {
            // 块状光标 = 反色块：拿该格的前景色当底、背景色当字色。
            // 空格单元格（ch 是 ' '）也走这条路，所以空白处同样看得见。
            span.classList.add('term-cursor');
            span.style.color = bg || '#0b0d12';
            span.style.background = fg || '#d8dee9';
          }
          frag.appendChild(span);
        }
        runStart = c;
      }
    }
    line.replaceChildren(frag);
  }

  sliceText(row, from, to) {
    let s = '';
    for (let i = from; i < to; i++) s += row[i].ch;
    return s;
  }

  /** plainText 返回纯文本内容（含回滚历史，供"复制全部"使用）。 */
  plainText() {
    const lines = [];
    const push = (row) => {
      let s = '';
      const n = Math.min(this.cols, row.length);
      for (let c = 0; c < n; c++) s += row[c].ch;
      lines.push(s.replace(/\s+$/, ''));
    };
    for (const entry of this.history) push(entry.row);
    for (let r = 0; r < this.rows; r++) push(this.grid[r]);
    while (lines.length && lines[lines.length - 1] === '') lines.pop();
    return lines.join('\n');
  }
}
