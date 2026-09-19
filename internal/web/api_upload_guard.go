package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
)

// ============================================================================
//  「大 body」上传路由的两道保护
//
//  用户报障：新建站点后要传 **978MB** 的网站包，点上传
//  **没反应、没成功也没提示**。根因有两条，而且都是"服务端先动手、用户看不到"：
//
//   1. cmd/zizpanel/http.go 给整个面板设了 `ReadTimeout: 30 * time.Second`。
//      Go 的 ReadTimeout 是"从连接建立到**读完整个请求（含 body）**"的**绝对**
//      截止时间 —— 也就是说**任何超过 30 秒的上传都会被服务端掐断**。
//      978MB 要走浏览器上传，30 秒内传完需要约 32MB/s，公网/普通家宽根本不可能。
//      连接被中途掐断，浏览器只看到网络错误，面板一句提示都没有。
//      （旧的 512MB 上限只是第二道坎：即使传得够快，也要先白传 512MB 才被拒。）
//   2. 前端只有一句常驻 toast，没有进度、没有服务端原因 —— 于是"没反应"。
//
//  ReadTimeout **不能整体去掉**：它防的是 Slowloris / 悬挂连接。
//  所以做法是：全局保留 30 秒，只有"大 body"路由自己把读截止时间推后，
//  并且仍然保留一个总时长上限（见 longUploadWindow）。
// ============================================================================

// longUploadWindow 是"大 body"上传路由给自己设的读超时（从现在起算）。
//
// 为什么不干脆 `SetReadDeadline(time.Time{})`（清零）：清零就等于**没有上限**，
// 一条极慢的连接可以永远占着服务端。这里给 2 小时 —— 4GB 要在 2 小时内传完
// 只需要约 0.6MB/s，已经宽松到不会误伤正常用户，同时仍然是一个有限值。
//
// ⚠️ 后来人请注意：这个函数存在的唯一原因是全局 ReadTimeout=30s。
// 如果你看到这里觉得"30 秒够用了"想把它删掉，请先去读上面的报障经过。
const longUploadWindow = 2 * time.Hour

// allowLongUpload 把当前请求的读截止时间推后到 longUploadWindow 之后。
//
// 实现走 Go 1.20+ 的 http.ResponseController（不要再去抓 net.Conn —— 那样在
// TLS/HTTP2 下会拿错对象）。返回 error 只是"是否成功延长"，而不是"上传失败"：
// 延长失败时小文件仍然传得动，所以调用方只应把它记进日志，不要拦下整个请求
// （拦下反而会把一个本来能用的功能变成"永远传不了"）。
func allowLongUpload(w http.ResponseWriter, r *http.Request) error {
	rc := http.NewResponseController(w)
	return rc.SetReadDeadline(time.Now().Add(longUploadWindow))
}

// uploadLimitMessage 生成"超过上限"的人话：实际大小、上限、可执行的建议。
//
// got<=0 表示长度未知（分块传输，服务端边收边算），此时不编造数字。
func uploadLimitMessage(got, limit int64, what string) string {
	gotStr := "未知（分块传输，服务端边收边算）"
	if got > 0 {
		gotStr = files.FormatSize(got)
	}
	return fmt.Sprintf("%s：你传了 %s，上限是 %s。建议：① 用「⬆ 上传文件夹」把网站按子目录分批传上去；"+
		"② 先在本地分卷压缩（例如 site.part1.zip）再逐个上传；③ 也可以先把压缩包传上来，再用「解压」。",
		what, gotStr, files.FormatSize(limit))
}

// isBodyTooLarge 判断一个 multipart 解析错误是不是"超过了 MaxBytesReader 的上限"。
//
// 用 errors.As 抓 *http.MaxBytesError（Go 1.19+ 的正规做法）；旧版本只会给出
// 一句字符串错误，所以再兜一层文本判断 —— 否则用户看到的会是
// "解析上传内容失败: http: request body too large"，等于没说，而且状态码还是 400
// 而不是 413（前端也就没法按"超限"给出建议）。
func isBodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}

// errUploadTooLarge 表示上传请求体超过给定上限。
var errUploadTooLarge = errors.New("上传内容超过上限")

// readUploadForm 解析 multipart 表单，并把"超过上限"与"其它解析失败"分开。
//
// 两道判据，缺一不可：
//  1. ContentLength 在**接收 body 之前**就能判 —— 用户不必先白传几百 MB；
//  2. MaxBytesReader 边收边算 —— 分块传输（ContentLength=-1）或前端谎报时兜底。
//
// 为什么抽成一个带 limit 参数的函数：单测没法真造一个 >4GB 的请求体，
// 但可以用一个小 limit 走完"MaxBytesReader 截断 → 判成超限"这条路径。
// 直接把 maxUpload 写死在函数体里，这条路就永远不会被测到。
func readUploadForm(r *http.Request, limit int64) error {
	if r.ContentLength > limit {
		return errUploadTooLarge
	}
	// 传 nil 作为 ResponseWriter 是安全的：MaxBytesReader 只是在溢出时用它
	// 通知服务端关闭连接，nil 时那次类型断言不成立、直接跳过。
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if isBodyTooLarge(err) {
			return errUploadTooLarge
		}
		return err
	}
	return nil
}
