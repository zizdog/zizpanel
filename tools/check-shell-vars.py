#!/usr/bin/env python3
"""检查 shell 脚本中的多字节变量名 bug。

问题：bash 的变量名只允许 [A-Za-z0-9_]。当 `$VAR` 后面紧跟一个非 ASCII 字符
（中文标点、中文文字）时，bash 会把该字符的所有字节并入变量名。

  实际踩到的例子（install.sh）：
      echo "进程未运行（$st）"
  多字节右括号 `）` 的 UTF-8 是 ef bc 89，bash 认为变量名是 `st\xef\xbc\x89`，
  于是报 `st\xef\xbc\x89: unbound variable`（开启 set -u 时直接退出脚本）。
  报错信息里的变量名带乱码，非常难定位。

正确写法是加花括号明确边界：
      echo "进程未运行（${st}）"

这个检查必须用字节级扫描：把文件当 UTF-8 字符串读取再匹配，
正则的字符类在多字节边界上并不可靠（第一版扫描器就是这样漏掉了这一处）。

用法：python3 tools/check-shell-vars.py install.sh tools/*.sh
退出码 0 表示通过，1 表示发现问题。
"""

import re
import sys

# 字节级模式：$ + 变量名 + 紧随的非 ASCII 字节
PATTERN = re.compile(rb"\$([A-Za-z_][A-Za-z0-9_]*)([\x80-\xff])")


def scan(path: str) -> list[tuple[int, str, str]]:
    """返回 [(行号, 变量名, 该行内容), ...]"""
    try:
        data = open(path, "rb").read()
    except FileNotFoundError:
        return []
    hits = []
    for m in PATTERN.finditer(data):
        line_no = data.count(b"\n", 0, m.start()) + 1
        line_start = data.rfind(b"\n", 0, m.start()) + 1
        line_end = data.find(b"\n", m.start())
        if line_end == -1:
            line_end = len(data)
        line = data[line_start:line_end].decode("utf-8", errors="replace").strip()
        hits.append((line_no, m.group(1).decode(), line))
    return hits


def main() -> int:
    paths = sys.argv[1:]
    if not paths:
        print("用法: check-shell-vars.py <脚本文件...>", file=sys.stderr)
        return 2

    total = 0
    for path in paths:
        hits = scan(path)
        if not hits:
            continue
        print(f"\n\033[31m✗ {path}\033[0m：$VAR 后紧跟非 ASCII 字符，"
              f"bash 会把它并入变量名（set -u 下报 unbound variable）")
        for line_no, var, line in hits:
            print(f"    行 {line_no}: ${{{var}}}  ← 应写成 ${{{var}}}")
            print(f"        {line}")
        total += len(hits)

    if total:
        print(f"\n\033[31m共发现 {total} 处，请改为 ${{VAR}} 形式\033[0m")
        return 1
    print("\033[32m✓ shell 变量引用检查通过\033[0m")
    return 0


if __name__ == "__main__":
    sys.exit(main())
