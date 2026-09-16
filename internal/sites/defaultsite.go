package sites

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
//  默认站点（localhost）的占位页
//
//  为什么这份内容放在 sites 包、而不是各自写一份：
//  "建默认站点"这件事有两个入口，且都必须产出**同一个标记文本**：
//
//   1. 面板设置里的「整理默认站点」（internal/web）—— 建目录 + 占位页 +
//      整份重写 000-default.conf，并**复核首页真的返回 200 且含这段标记**；
//   2. 一键 LNMP 的收尾（internal/services，phpMyAdmin 需要默认站点来接住
//      `/phpmyadmin` 入口）—— 只在默认站点缺失时补一份最小的。
//
//  两份占用页文本一旦走样，第 1 条的"复核"就会误判成失败（页面 200 但标记对不上
//  → 面板报"nginx 没有真正生效"）。所以文本与"缺了才建"的逻辑都收敛到这里。
//
//  真机背景（2026-09-17 mini）：一键 LNMP 装完 phpMyAdmin 打不开，原因是
//  `vhosts/000-default.conf` 根本不存在 —— phpMyAdmin 安装器只会往"已有的默认
//  站点"里插 location。这里补的就是那个缺口。
// ============================================================================

// LocalhostIndexHTML 是默认站点的占位首页。
//
// 它同时是"默认站点真的生效了"的判据：面板会取 http://127.0.0.1/ 并检查
// 页面里是否含 LocalhostIndexMarker。
const LocalhostIndexMarker = "这是本机 Web 服务的默认站点"

// LocalhostIndexHTML 见上。
const LocalhostIndexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>localhost</title>
  <style>
    body { font-family: -apple-system, "PingFang SC", sans-serif; margin: 15vh auto; max-width: 32rem;
           padding: 0 1.5rem; color: #222; line-height: 1.8; }
    code { background: #f2f2f4; padding: .1rem .35rem; border-radius: .25rem; }
    .muted { color: #888; font-size: .9rem; }
  </style>
</head>
<body>
  <h1>localhost</h1>
  <p>` + LocalhostIndexMarker + `。它只放这一张占位页，用来接住没匹配到具体域名的请求。</p>
  <p class="muted">面板不在这个端口上：请用面板自己的地址（HTTPS + 安全后缀）访问。<br>
  需要管理数据库？在面板里打开 phpMyAdmin（需先登录面板）。</p>
</body>
</html>
`

// EnsureLocalhostPlaceholder 确保 <wwwRoot>/localhost/index.html 存在。
//
// **已存在就一定不改**：用户可能把这张占位页换成了自己的内容，覆盖它属于
// "动了用户的东西"。返回是否创建了文件，便于日志如实说明。
func EnsureLocalhostPlaceholder(wwwRoot string) (path string, created bool, err error) {
	wwwRoot = strings.TrimSpace(wwwRoot)
	if wwwRoot == "" {
		return "", false, fmt.Errorf("网站根目录为空，无法创建默认站点占位页")
	}
	dir := filepath.Join(wwwRoot, "localhost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("创建默认站点目录 %s 失败: %w", dir, err)
	}
	index := filepath.Join(dir, "index.html")
	if _, serr := os.Stat(index); serr == nil {
		return index, false, nil // 已经有了：一个字都不动
	}
	if err := os.WriteFile(index, []byte(LocalhostIndexHTML), 0o644); err != nil {
		return "", false, fmt.Errorf("写入默认站点占位页 %s 失败: %w", index, err)
	}
	return index, true, nil
}

// ============================================================================
//  默认站点的 index.php（用户 2026-09-17 明确要求）
//
//  "一键安装脚本安装完成后要默认建一个站，localhost/ip 访问，静态网站，
//   有一个默认 index.php 就行。"
//
//  所以默认站点不只是"接住请求的占位页"，还要**真的能跑 PHP**：
//  面板会写一个 index.php，并让默认站点的 vhost 用该机器当前 PHP 版本的
//  **专属 FastCGI 端点**去执行它（不是写死 9000 —— 多版本下 9000 没人听）。
//
//  与 index.html 的关系：**不删、不改**用户/面板之前留下的 index.html；
//  只是把 index.php 放在 nginx `index` 指令的**前面**，于是 `/` 走 PHP。
//  用户删掉 index.php，原来的 index.html 就又回来了 —— 不覆盖任何人的内容。
// ============================================================================

// DefaultIndexPHPMarker 是"这个 index.php 是面板建的"的标记。
//
// 用来实现"缺了才建、用户改过就不覆盖"：文件存在（无论内容）就一个字都不改。
const DefaultIndexPHPMarker = "ZP-DEFAULT-SITE"

// DefaultIndexPHP 是默认站点的占位首页（PHP）。
//
// 刻意保持简单，但**自证 PHP 可用**：版本 + 服务器时间 + 运行方式。
const DefaultIndexPHP = `<?php
// ` + DefaultIndexPHPMarker + `：由 ZizPanel 创建（默认站点占位页）。
// 面板只在文件不存在时创建它；你改过之后不会再被覆盖。
header('Content-Type: text/html; charset=utf-8');
?>
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>ZizPanel 默认站点</title>
  <style>
    body { font-family: -apple-system, "PingFang SC", sans-serif; margin: 12vh auto; max-width: 34rem;
           padding: 0 1.5rem; color: #222; line-height: 1.8; }
    code { background: #f2f2f4; padding: .1rem .35rem; border-radius: .25rem; }
    .ok { color: #0a7a33; font-weight: 600; }
    .muted { color: #888; font-size: .9rem; }
  </style>
</head>
<body>
  <h1>ZizPanel 默认站点</h1>
  <p class="ok">PHP 正常工作</p>
  <ul>
    <li>PHP 版本：<code><?= htmlspecialchars(PHP_VERSION) ?></code></li>
    <li>运行方式：<code><?= htmlspecialchars(PHP_SAPI) ?></code></li>
    <li>服务器时间：<code><?= date('Y-m-d H:i:s') ?></code></li>
  </ul>
  <p class="muted">这个页面由 <code>www/localhost/index.php</code> 生成（面板的默认站点）。
  换成你自己的站点：在「网站管理」里新建站点，或直接替换这个目录里的文件。</p>
</body>
</html>
`

// EnsureDefaultSitePHPIndex 确保 <wwwRoot>/localhost/index.php 存在。
//
// **已存在就一定不改**（用户可能已经把它换成自己的站点入口）。
// 返回是否创建了文件，便于日志如实说明。
func EnsureDefaultSitePHPIndex(wwwRoot string) (path string, created bool, err error) {
	wwwRoot = strings.TrimSpace(wwwRoot)
	if wwwRoot == "" {
		return "", false, fmt.Errorf("网站根目录为空，无法创建默认站点 index.php")
	}
	dir := filepath.Join(wwwRoot, "localhost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("创建默认站点目录 %s 失败: %w", dir, err)
	}
	index := filepath.Join(dir, "index.php")
	if _, serr := os.Stat(index); serr == nil {
		return index, false, nil // 已经有了：一个字都不动
	}
	if err := os.WriteFile(index, []byte(DefaultIndexPHP), 0o644); err != nil {
		return "", false, fmt.Errorf("写入默认站点 index.php %s 失败: %w", index, err)
	}
	return index, true, nil
}
