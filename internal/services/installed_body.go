package services

import (
	"os"
	"path/filepath"
	"strings"
)

// ============================================================================
//  「安装体」（真实产物）判定
//
//  为什么需要它 —— 这是「装了却显示未安装 / 不在已安装列表 / 还显示安装按钮」
//  这一族缺陷的根因（用户报障两条：图片压缩（libvips）安装成功后
//  仍显示「安装」；phpMyAdmin 明明是装好的目录却显示未安装）。
//
//  目录里有一批条目**没有常驻服务**（App.NoDaemon）：
//    · 纯 CLI 引擎：vips / ffmpeg / python@x.y —— 装完就是 brew 前缀下的一个可执行文件；
//    · 网页入口：phpMyAdmin —— 装完就是 <brew>/share/phpmyadmin 这个目录 + index.php。
//  面板不会给它们建服务记录（建了就是"永远没有状态的假记录"，ffmpeg 的历史坑），
//  于是它们的「已安装」过去**只**来自 `brew list --versions` 的一句话。
//  那句话一旦缺席（探测失败/超时、市场缓存还没刷新、用户是手工装的），判据就整个
//  落空 —— 界面把明明装着的东西显示成「安装」。
//
//  这里把判据拉回**磁盘上的真实产物**（AGENTS 第三节：判据要贴着运行体）：
//    · RuntimePath 指向可执行文件 → 文件存在且带可执行位；
//    · RuntimePath 指向目录       → 目录存在，且 RuntimeEntry（入口文件）也在。
//
//  ⚠️ 只做 stat，**不执行**任何命令：市场列表是首屏渲染路径，昂贵的真实探测
//  （`vips --version` / brew / docker）不许放在这里（AGENTS 第三节）。
//  "能不能跑"由按需路径负责：internal/imgopt.DetectEngine、安装时的
//  imgCompressVersion 复核、以及文件管理里点「图片压缩」时的引擎探测。
// ============================================================================

// RuntimeBody 是一次"安装体"探测的结论。
type RuntimeBody struct {
	// Path 是磁盘上真实存在、且满足条件的产物路径；空 = 没有产物证据。
	Path string
	// IsDir 表示这个产物是目录（网页入口型）；false = 可执行文件（CLI 型）。
	IsDir bool
	// Declared 表示目录条目声明过 RuntimePath（用于区分"没声明"与"声明了但不在"）。
	Declared bool
}

// Exists 报告"安装体真的在磁盘上"（这就是 installed 判据要的那条真实证据）。
func (b RuntimeBody) Exists() bool { return b.Path != "" }

// ResolveRuntimePath 展开 RuntimePath 里的 `{brew}` 前缀与 `~/` 家目录前缀。
//
// 与 ConfigFilePath 用**同一套**约定（少一套写法就少一处漂移）。展开不出来
// （没有 brew 前缀 / 没有家目录）时返回空串 = 这条证据这次拿不到，调用方不得
// 据此下"没装"的结论。
func ResolveRuntimePath(declared, brewPrefix, userHome string) string {
	p := strings.TrimSpace(declared)
	if p == "" {
		return ""
	}
	if p == "{brew}" {
		return brewPrefix
	}
	if rest, ok := strings.CutPrefix(p, "{brew}/"); ok {
		if brewPrefix == "" {
			return ""
		}
		return filepath.Join(brewPrefix, rest)
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if userHome == "" {
			return ""
		}
		return filepath.Join(userHome, rest)
	}
	return p
}

// DetectRuntimeBody 探测某个目录条目声明的安装体（只 stat，不执行）。
//
// 三种结果严格区分：
//   - 没声明 RuntimePath          → Declared=false，Exists()=false（这条证据不适用）；
//   - 声明了、但磁盘上没有/不合格 → Declared=true，Exists()=false；
//   - 声明了、真实产物在          → Path 非空，Exists()=true。
func DetectRuntimeBody(app App, brewPrefix, userHome string) RuntimeBody {
	if strings.TrimSpace(app.RuntimePath) == "" {
		return RuntimeBody{}
	}
	body := RuntimeBody{Declared: true}
	path := ResolveRuntimePath(app.RuntimePath, brewPrefix, userHome)
	if path == "" {
		return body
	}
	// os.Stat 会跟随软链接：brew 的 bin 全部是软链，**悬空软链**（指向已删除的
	// Cellar）会返回错误 → 不算已安装。这与 BrewStateFor 的口径一致
	// （2026-09-18 的教训：悬空软链把已卸载的版本列成"已安装"）。
	st, err := os.Stat(path)
	if err != nil {
		return body
	}
	if entry := strings.TrimSpace(app.RuntimeEntry); entry != "" {
		// 目录型（网页入口）：目录必须在，入口文件也必须真的在。
		// 只看目录会把"web 根被清空/只剩空壳"算成已安装，那是谎报。
		if !st.IsDir() {
			return body
		}
		est, err := os.Stat(filepath.Join(path, entry))
		if err != nil || est.IsDir() {
			return body
		}
		body.Path, body.IsDir = path, true
		return body
	}
	// 文件型（命令行工具）：必须是**普通文件且带可执行位**。
	// 目录在这里一律不算 —— 目录形态必须在 RuntimeEntry 里写清入口文件。
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return body
	}
	body.Path = path
	return body
}
