# panel-entry.awk —— 在 nginx vhost 中插入/替换面板入口 location 块。
#
# 为什么单独成文件：这段逻辑需要在嵌套引号里嵌入 awk 程序，
# 内联在 shell 里转义极其脆弱（开发中确实反复踩坑）。
# 独立文件后可用 awk -f 调用，也能单独测试。
#
# 输入变量（-v 传入）：
#   path  面板访问路径，如 /_panel
#   mb/me 标记块起止注释
#   body  入口块内容文件（由 shell 生成，含正确缩进与 nginx 变量）
#
# 处理规则：
#   1. 丢弃旧的标记块（幂等的基础）
#   2. 丢弃无标记的历史 /_panel location 块（按花括号配对跳过）
#   3. 把入口块插到 `location / {` 之前；没有则插到 server 块最后一个 } 之前
#
# 注意：花括号计数用 open_minus_close() 而不是 gsub(/\{/,"{")。
# 虽然本例中两者结果相同，但 gsub 会修改 $0，
# 在需要同时判断"原始行内容"的场景下容易出错，因此统一用不修改输入的方式。

function open_minus_close(s,   a, b) {
  a = s; b = s
  gsub(/\{/, "", a)
  gsub(/\}/, "", b)
  return length(b) - length(a)
}

# 1) 跳过旧的标记块
skipMarked {
  if (index($0, me) > 0) skipMarked = 0
  next
}
index($0, mb) > 0 { skipMarked = 1; next }

# 2) 跳过无标记的旧入口块（花括号配对，直到配对闭合）
depth > 0 {
  depth += open_minus_close($0)
  next
}
# 匹配旧入口块：location ^~ /path  或 location ^~ /path/  或 location /path
$0 ~ ("^[ \t]*location[ \t]+\\^~[ \t]+" path "/?[ \t]*\\{") ||
$0 ~ ("^[ \t]*location[ \t]+" path "/?[ \t]*\\{") ||
$0 ~ ("^[ \t]*location[ \t]+=+[ \t]+" path "[ \t]*\\{") {
  depth = open_minus_close($0)
  if (depth <= 0) depth = 1   # 单行闭合的极端情况
  next
}

# 3) 记录插入锚点：`location / {` 那一行
!anchor && $0 ~ /^[ \t]*location[ \t]+\/[ \t]*\{/ {
  anchor = total + 1
}

{ lines[++total] = $0 }

END {
  at = anchor
  if (at <= 0) {
    # 没有 location / ：插在 server 块最后一个 `}` 之前
    for (i = total; i >= 1; i--) {
      if (lines[i] ~ /^[ \t]*\}[ \t]*$/) { at = i; break }
    }
  }
  if (at <= 0) at = total + 1

  for (i = 1; i <= total; i++) {
    if (i == at) emit_body()
    print lines[i]
  }
  if (at > total) emit_body()
}

function emit_body(   line) {
  while ((getline line < body) > 0) print line
  close(body)
}
