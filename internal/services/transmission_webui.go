package services

// ============================================================================
//  Transmission 的中文 Web UI（第三方 Transmission Next UI v0.3.3，MIT）
//
//  官方 4.x 网页版**只有英文**（4.0 重写前端时丢了 i18n；本机把 4.1.3 的
//  transmission-app.js 扒开数过：0 个汉字、0 张语言表）。用户要求中文界面。
//
//  做法：把内嵌在面板里的那一份界面（assets/transmission-next，来源与 sha256 见
//  那里的 PROVENANCE.md）写进 daemon 的 **web 根目录**
//  （<brew>/share/transmission/public_html）。daemon 每个请求都从磁盘读，
//  所以**不需要重启**，也不用碰 settings.json / plist / RPC 口令。
//
//  为什么不用 TRANSMISSION_WEB_HOME：那要给 plist 加环境变量，而 brew services
//  每次 start 都会重写 plist，系统级迁移脚本又是所有应用共用的一份。
//  写 web 根目录一样"贴运行体"（判据是 /transmission/web/ 真的回了中文），代价更小。
//
//  `brew upgrade transmission-cli` 会换掉整个 keg（连带 web 根目录里我们写的东西）
//  ⇒ 面板启动时自愈一次（EnsureTransmissionWebUIQuiet）；失败只写日志，绝不挡启动。
// ============================================================================

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/logx"
)

//go:embed assets/transmission-next
var transmissionWebUIFS embed.FS

const (
	// transmissionWebUIAssetsRoot 是内嵌界面在 embed.FS 里的根（与 PROVENANCE.md 同级）。
	transmissionWebUIAssetsRoot = "assets/transmission-next"
	// transmissionWebUIVersion 必须与 PROVENANCE.md 里的上游版本一致。
	transmissionWebUIVersion = "0.3.3"
	// transmissionWebUIMarker 写在 web 根目录里，内容是版本号（判断"要不要重写"）。
	transmissionWebUIMarker = ".zizpanel-webui"
	// transmissionWebUIStockBak 是官方英文那份 index.html 的备份（只留一次，便于回退）。
	transmissionWebUIStockBak = "index.html.zizpanel-stock"
	// transmissionWebUIProvenanceFile 只属于仓库，不进 web 根目录。
	transmissionWebUIProvenanceFile = "PROVENANCE.md"
)

var transmissionWebUILog = logx.New("transmission-webui")

// transmissionWebUIJSRefRe 取 index.html 里引用的脚本 —— 中文界面唯一的入口文件。
var transmissionWebUIJSRefRe = regexp.MustCompile(`(?i)src="([^"]+\.js)"`)

// transmissionWebUIServeDir 是 daemon 真正读的那个 web 根目录（由 brew 前缀推导）。
func (m *Manager) transmissionWebUIServeDir() string {
	return filepath.Join(m.brewPrefix(), "share", "transmission", "public_html")
}

// transmissionWebUIServeProblem 检查一个 web 根目录里是不是中文那一份界面
// （空串 = 是）。判据是**磁盘上真正被服务的那份文件**：index.html 引用了 assets/*.js，
// 且那个 js 里确实有中文 —— 只看标记文件会把"文件被换回去了"当成已生效。
func transmissionWebUIServeProblem(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return "读不到 index.html（" + err.Error() + "）"
	}
	m := transmissionWebUIJSRefRe.FindSubmatch(raw)
	if m == nil {
		return "index.html 里没有指向 assets/*.js 的引用 —— 还是官方英文界面那一份"
	}
	ref := string(m[1])
	if strings.HasPrefix(ref, "/") || strings.Contains(ref, "://") {
		return "index.html 引用的是绝对地址（" + ref + "），不是面板内嵌的那一份"
	}
	jsPath := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(ref, "./")))
	js, err := os.ReadFile(jsPath)
	if err != nil {
		return "中文界面的脚本 " + ref + " 读不到（" + err.Error() + "）"
	}
	if !containsCJK(js) {
		return "界面脚本 " + ref + " 里没有任何中文，不是中文那一份"
	}
	return ""
}

// containsCJK 判断一段文本里有没有汉字（只看 CJK 统一表意文字基本区，够用了）。
func containsCJK(b []byte) bool {
	for _, r := range string(b) {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// transmissionWebUIApplied 报告"服务目录里现在就是这一版中文界面"，以及不是的原因。
func (m *Manager) transmissionWebUIApplied() (bool, string) {
	dir := m.transmissionWebUIServeDir()
	raw, err := os.ReadFile(filepath.Join(dir, transmissionWebUIMarker))
	if err != nil {
		return false, "还没有面板写入的标记文件"
	}
	if got := strings.TrimSpace(string(raw)); got != transmissionWebUIVersion {
		return false, "标记版本是 " + got + "，要装的是 " + transmissionWebUIVersion
	}
	if why := transmissionWebUIServeProblem(dir); why != "" {
		return false, why
	}
	return true, ""
}

// EnsureTransmissionWebUI 把内嵌的中文界面写进 daemon 的 web 根目录（幂等）。
//
// 已经是最新就只读一个标记文件；写之前先给官方那份 index.html 留一次备份
// （回退：删掉 index.html、把 index.html.zizpanel-stock 改回来即可）。
func (m *Manager) EnsureTransmissionWebUI(ctx context.Context, res *InstallResult) error {
	dir := m.transmissionWebUIServeDir()
	if !fileExists(dir) {
		return fmt.Errorf("找不到 Transmission 的 web 目录 %s"+
			"（brew 装的是不是 transmission-cli？）", dir)
	}
	if ok, _ := m.transmissionWebUIApplied(); ok {
		if res != nil {
			res.step(ctx, "Web UI 已是中文界面（v"+transmissionWebUIVersion+"，无需改动）")
		}
		return nil
	}
	stock := filepath.Join(dir, "index.html")
	bak := filepath.Join(dir, transmissionWebUIStockBak)
	if fileExists(stock) && !fileExists(bak) {
		if err := copyRegularFile(stock, bak); err != nil {
			return fmt.Errorf("备份官方界面 index.html 失败（没备份就不覆盖它）：%w", err)
		}
	}
	if err := writeEmbeddedTransmissionWebUI(dir); err != nil {
		return err
	}
	// 写完立刻按**服务目录里的文件**自检一次：复制不完整就在这一步如实失败，
	// 而不是留一个"标记说中文、页面是坏的"的假象。
	if why := transmissionWebUIServeProblem(dir); why != "" {
		return fmt.Errorf("写入后自检没通过：%s", why)
	}
	if err := os.WriteFile(filepath.Join(dir, transmissionWebUIMarker),
		[]byte(transmissionWebUIVersion+"\n"), 0o644); err != nil {
		return fmt.Errorf("写标记文件失败: %w", err)
	}
	if res != nil {
		res.step(ctx, "已写入中文界到 "+dir+"（Transmission Next UI v"+transmissionWebUIVersion+
			"，第三方 MIT；官方英文那份备份为 "+transmissionWebUIStockBak+"）")
	}
	return nil
}

// writeEmbeddedTransmissionWebUI 把内嵌的界面文件铺到目标目录（覆盖同名文件，
// **不删**官方留下来的其它文件 —— 不完整写入也还能回退到官方那一份）。
func writeEmbeddedTransmissionWebUI(dst string) error {
	root := transmissionWebUIAssetsRoot
	return fs.WalkDir(transmissionWebUIFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." || rel == transmissionWebUIProvenanceFile {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, rerr := transmissionWebUIFS.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

// EnsureTransmissionWebUIQuiet 是面板启动时的自愈入口：只处理"装过 Transmission、
// 但 web 根目录被换回官方英文"这一种情况（brew upgrade 之后就是它）。
// 没装就立刻返回；失败只写日志 —— 中文界面不该拦住面板启动。
func (m *Manager) EnsureTransmissionWebUIQuiet(ctx context.Context) {
	if m == nil {
		return
	}
	if !fileExists(m.transmissionWebUIServeDir()) {
		return
	}
	if ok, _ := m.transmissionWebUIApplied(); ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := m.EnsureTransmissionWebUI(ctx, nil); err != nil {
		transmissionWebUILog.Warn("Transmission 中文界面自愈失败（现在打开是官方英文界面）：%v", err)
		return
	}
	transmissionWebUILog.Info("Transmission 中文界面已写回 %s", m.transmissionWebUIServeDir())
}

// transmissionHTTPBody 是"取回正文"的 HTTP 探测（单测替换它，绝不连真机 9091）。
var transmissionHTTPBody = func(m *Manager, ctx context.Context, rawURL, user, password string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// 中文界面的 bundle 约 650 KiB，留 8 MiB 余量；超了按读到的部分判断。
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, body, nil
}

func (m *Manager) transmissionHTTPGetBody(ctx context.Context, rawURL, user, password string) (int, []byte, error) {
	return transmissionHTTPBody(m, ctx, rawURL, user, password)
}

// verifyTransmissionWebUIServed 是**端到端**判据：从 9091 上把首页取回来，
// 按首页自己引用的脚本再取一次，脚本里必须有中文。首页/脚本任何一个拿不到、
// 或脚本里没有中文，都如实报错（绝不"文件写过了就算成功"）。
func (m *Manager) verifyTransmissionWebUIServed(ctx context.Context, user, password string) error {
	webURL := "http://127.0.0.1:" + strconv.Itoa(transmissionPort) + "/transmission/web/"
	code, body, err := m.transmissionHTTPGetBody(ctx, webURL, user, password)
	if err != nil {
		return fmt.Errorf("取不到 %s：%v", webURL, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("%s 返回 %d（带凭据应为 200）", webURL, code)
	}
	ref := transmissionWebUIJSRefRe.FindSubmatch(body)
	if ref == nil {
		return fmt.Errorf("%s 的首页里没有 assets/*.js 引用 —— 现在还是官方英文界面", webURL)
	}
	base, err := url.Parse(webURL)
	if err != nil {
		return fmt.Errorf("解析 %s 失败：%v", webURL, err)
	}
	rel, err := url.Parse(string(ref[1]))
	if err != nil {
		return fmt.Errorf("解析首页里的脚本地址失败：%v", err)
	}
	jsURL := base.ResolveReference(rel).String()
	jsCode, js, jerr := m.transmissionHTTPGetBody(ctx, jsURL, user, password)
	if jerr != nil {
		return fmt.Errorf("取不到界面脚本 %s：%v", jsURL, jerr)
	}
	if jsCode != http.StatusOK {
		return fmt.Errorf("界面脚本 %s 返回 %d", jsURL, jsCode)
	}
	if !containsCJK(js) {
		return fmt.Errorf("界面脚本 %s 里没有中文 —— 现在还是官方英文界面", jsURL)
	}
	return nil
}

// copyRegularFile 复制一个普通文件（0600/0644 由调用方决定目标权限语义，这里保持 0644）。
func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
