package sites

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
//  上传与执行限制（nginx 请求体上限 + PHP 上传/执行上限）
//
//  用户报障（2026-09-20）：phpMyAdmin 导入几十 MB 的 SQL 报
//  **413 Request Entity Too Large**。根因是 nginx 的默认 client_max_body_size
//  只有 **1m** —— 请求体根本进不到 PHP，所以先 413 而不是 PHP 报错；
//  即使放进来，PHP 出厂 upload_max_filesize 也只有 2M、post_max_size 8M。
//
//  为什么必须面板管：这两组值过去既没有入口、也没有默认值，
//  用户只能手工改 brew 的配置（违反"面板里能做完"的产品原则），
//  所以默认值就必须是能用的 —— 面板生成的 vhost 一律带 client_max_body_size，
//  PHP 侧由本包写一份**面板自己的 conf.d 片段**。
//
//  为什么不改 brew 的 php.ini：php.ini 是 Homebrew 的东西，升级/重装会覆盖，
//  用户手改过的内容还会丢失。面板只在 `<brew>/etc/php/<版本>/conf.d/` 下写
//  自己的 `99-zizpanel-limits.ini`（数字前缀保证最后加载、优先级最高），
//  删掉它并重启 php-fpm 就回到出厂值 —— 可逆、可审、不与 brew 争文件。
//
//  为什么选 conf.d 而不是 php-fpm.d 的 php_admin_value：
//  两者都只影响使用该 PHP 版本的站点（面板管的站点/默认站点就是这些），
//  但 conf.d 片段同时被 CLI 与 php-fpm 读取，回读生效值只要跑一次 `php -i`
//  就能看到 FPM 真正会用的那份配置；php_admin_value 则只在 FPM 池里生效、
//  CLI 看不见，回读会更绕。这里选可回读的那一种。
// ============================================================================

// 默认值：默认就要能用，不能让用户先撞 413 才知道要改。
const (
	// DefaultClientMaxBodySize 与 PHP 侧上限匹配（都按 512M 起）。
	DefaultClientMaxBodySize = "512m"
	// DefaultPHPUploadMaxFilesize / DefaultPHPPostMaxSize：必须**同时**放大，
	// post_max_size 小于 upload_max_filesize 时上传永远失败（PHP 会先按 post 判）。
	DefaultPHPUploadMaxFilesize = "512M"
	DefaultPHPPostMaxSize       = "512M"
	DefaultPHPMemoryLimit       = "512M"
	// DefaultPHPMaxExecutionTime 是秒。phpMyAdmin 导入大 SQL 会跑很久，出厂 30 秒不够。
	DefaultPHPMaxExecutionTime = 300

	// PHPLimitsFragmentName 是面板写进 conf.d 的文件名。
	//
	// `99-` 前缀：php 的 conf.d 是按文件名排序加载的，数字越大越靠后。
	// 放在最后才能覆盖 brew 自带片段（如 ext-opcache.ini）里的同名项。
	PHPLimitsFragmentName = "99-zizpanel-limits.ini"
)

// Limits 是一组上传与执行限制。空字段由 Normalize 补默认值。
type Limits struct {
	// ClientMaxBodySize 是 nginx 的 client_max_body_size 值，如 "512m" / "1g"。
	ClientMaxBodySize string `json:"client_max_body_size"`
	// UploadMaxFilesize / PostMaxSize / MemoryLimit 是 PHP 侧的 ini 值，如 "512M"。
	UploadMaxFilesize string `json:"upload_max_filesize"`
	PostMaxSize       string `json:"post_max_size"`
	MemoryLimit       string `json:"memory_limit"`
	// MaxExecutionTime 是 PHP max_execution_time（秒）。
	MaxExecutionTime int `json:"max_execution_time"`
}

// DefaultLimits 返回默认限制（**唯一**默认值来源，config 包也用它）。
func DefaultLimits() Limits {
	return Limits{
		ClientMaxBodySize: DefaultClientMaxBodySize,
		UploadMaxFilesize: DefaultPHPUploadMaxFilesize,
		PostMaxSize:       DefaultPHPPostMaxSize,
		MemoryLimit:       DefaultPHPMemoryLimit,
		MaxExecutionTime:  DefaultPHPMaxExecutionTime,
	}
}

// Normalize 补齐空字段（老配置 / 调用方没传值时不至于生成空指令）。
func (l Limits) Normalize() Limits {
	d := DefaultLimits()
	if strings.TrimSpace(l.ClientMaxBodySize) == "" {
		l.ClientMaxBodySize = d.ClientMaxBodySize
	}
	if strings.TrimSpace(l.UploadMaxFilesize) == "" {
		l.UploadMaxFilesize = d.UploadMaxFilesize
	}
	if strings.TrimSpace(l.PostMaxSize) == "" {
		l.PostMaxSize = d.PostMaxSize
	}
	if strings.TrimSpace(l.MemoryLimit) == "" {
		l.MemoryLimit = d.MemoryLimit
	}
	if l.MaxExecutionTime <= 0 {
		l.MaxExecutionTime = d.MaxExecutionTime
	}
	return l
}

// reSizeValue 匹配 "512m" / "512M" / "1g" / "1073741824"（数字 + 可选单位）。
//
// 上限 10 位数字：既容得下"用字节写的 1G"（1073741824），又能挡住
// `99999999999g` 这种把 nginx/php 当成天文数字的值 —— 那种值在 size 解析里
// 溢出后反而会得到一个很小的上限，症状极其难查。
var reSizeValue = regexp.MustCompile(`^[0-9]{1,10}[kKmMgG]?$`)

// ValidateSizeValue 校验一个尺寸写法，非法时返回人话错误（供 HTTP 400 用）。
func ValidateSizeValue(field, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("%s不能为空（示例：512m / 1g）", field)
	}
	if !reSizeValue.MatchString(v) {
		return fmt.Errorf("%s格式不合法：%q。请写成「数字 + 可选单位」的形式，"+
			"例如 512m、1g、1073741824（单位 k/m/g，不写单位＝字节；不接受空格、小数点或其它字符）",
			field, v)
	}
	if n, ok := ParseSizeBytes(v); ok && n <= 0 {
		return fmt.Errorf("%s不能是 0（那样等于禁止上传）", field)
	}
	return nil
}

// ParseSizeBytes 把 "512m" 解析成字节数（不区分大小写，默认单位＝字节）。
func ParseSizeBytes(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'k', 'K':
		mult = 1024
		v = v[:len(v)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		v = v[:len(v)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n * mult, true
}

// Validate 校验整组限制。返回的第一个错误就是 HTTP 400 要展示的人话。
func (l Limits) Validate() error {
	if err := ValidateSizeValue("nginx 请求体上限（client_max_body_size）", l.ClientMaxBodySize); err != nil {
		return err
	}
	if err := ValidateSizeValue("PHP upload_max_filesize", l.UploadMaxFilesize); err != nil {
		return err
	}
	if err := ValidateSizeValue("PHP post_max_size", l.PostMaxSize); err != nil {
		return err
	}
	if err := ValidateSizeValue("PHP memory_limit", l.MemoryLimit); err != nil {
		return err
	}
	if l.MaxExecutionTime < 1 || l.MaxExecutionTime > 86400 {
		return fmt.Errorf("PHP max_execution_time 应在 1~86400 秒之间（当前 %d）", l.MaxExecutionTime)
	}
	// 交叉校验：post_max_size 小于 upload_max_filesize 时，大型上传一定失败
	// （PHP 先按 post_max_size 截断整个请求）。这种组合必须当场挡住并说清原因，
	// 否则用户会再次看到"上传失败"却不知道为什么。
	post, okPost := ParseSizeBytes(l.PostMaxSize)
	up, okUp := ParseSizeBytes(l.UploadMaxFilesize)
	if okPost && okUp && post < up {
		return fmt.Errorf("PHP post_max_size（%s）不能小于 upload_max_filesize（%s）："+
			"否则文件再小也会因为整个请求超过 post_max_size 而上传失败。"+
			"建议 post_max_size ≥ upload_max_filesize", l.PostMaxSize, l.UploadMaxFilesize)
	}
	// nginx 侧也必须装得下：请求体上限小于 PHP 上传上限时，请求根本到不了 PHP。
	body, okBody := ParseSizeBytes(l.ClientMaxBodySize)
	if okBody && okUp && body < up {
		return fmt.Errorf("nginx 请求体上限（%s）不能小于 PHP upload_max_filesize（%s）："+
			"否则请求会被 nginx 用 413 挡下，PHP 根本收不到文件", l.ClientMaxBodySize, l.UploadMaxFilesize)
	}
	return nil
}

// PHPIniFragment 返回面板写入 conf.d 的 ini 片段内容。
func PHPIniFragment(l Limits) string {
	l = l.Normalize()
	return "; 由 ZizPanel 生成：上传与执行限制（面板设置 → 上传与执行限制）\n" +
		"; 这是面板自己的片段，不是 Homebrew 的 php.ini（brew 升级不会覆盖它）。\n" +
		"; 删除本文件并重启 php-fpm 即恢复该版本的出厂限制。\n" +
		"upload_max_filesize = " + l.UploadMaxFilesize + "\n" +
		"post_max_size = " + l.PostMaxSize + "\n" +
		"memory_limit = " + l.MemoryLimit + "\n" +
		"max_execution_time = " + strconv.Itoa(l.MaxExecutionTime) + "\n"
}

// PHPConfDPath 返回某版本面板限制片段的目标路径（版本号非法时返回空串）。
func PHPConfDPath(brewPrefix, version string) string {
	version = strings.TrimSpace(version)
	if !rePHPVersion.MatchString(version) {
		return ""
	}
	if strings.TrimSpace(brewPrefix) == "" {
		return ""
	}
	return filepath.Join(brewPrefix, "etc", "php", version, "conf.d", PHPLimitsFragmentName)
}

// PHPLimitsResult 描述一次"写入 PHP 限制片段"的结果。
type PHPLimitsResult struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Backup  string `json:"backup,omitempty"`
}

// EnsurePHPLimits 把限制片段写到该 PHP 版本的 conf.d 下（幂等）。
//
// 只在该版本的目录**真实存在**时才写：目录不存在说明这个版本没装，
// 创建一棵空目录树会凭空造出一个"看起来装了 PHP"的版本（卸载后残留同类的坑）。
func EnsurePHPLimits(brewPrefix, version string, l Limits, noBackup bool) (*PHPLimitsResult, error) {
	path := PHPConfDPath(brewPrefix, version)
	if path == "" {
		return nil, fmt.Errorf("无法为 PHP %q 解析限制文件路径（版本号或 Homebrew 前缀不合法）", version)
	}
	verDir := filepath.Join(brewPrefix, "etc", "php", version)
	if st, err := os.Stat(verDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("PHP %s 未安装或配置目录不存在：%s", version, verDir)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建 PHP %s 的 conf.d 目录失败: %w", version, err)
	}
	want := PHPIniFragment(l)
	if existing, err := os.ReadFile(path); err == nil && string(existing) == want {
		return &PHPLimitsResult{Version: version, Path: path, Changed: false}, nil
	}
	old, _ := os.ReadFile(path)
	if err := writeFileAtomicPreserve(path, want); err != nil {
		return nil, fmt.Errorf("写入 PHP %s 的限制片段失败: %w", version, err)
	}
	res := &PHPLimitsResult{Version: version, Path: path, Changed: true}
	// 备份在写入之后：备份失败不该被当成"限制没写进去"。
	if !noBackup && len(old) > 0 {
		if bak, err := backupOnce(path, string(old)); err == nil {
			res.Backup = bak
		}
	}
	return res, nil
}

// ProbePHPLimits 用该版本的 PHP 二进制回读**实际生效**的上传/执行限制。
//
// 为什么优选用同一目录下的 `php-cgi`：PHP 的 **CLI SAPI 会把
// max_execution_time 强制成 0**（CLI 不限制执行时间），所以用 `php -r` 回读
// 时那一项永远是 0 —— 那是 SAPI 的行为，不是我们的片段没生效。实测：
//
//	php -r      → max_execution_time=0   （CLI 强制）
//	php-cgi -f  → max_execution_time=300 （读的是同一份 php.ini + conf.d）
//
// php-cgi 与 php-fpm 都是"非 CLI"的 SAPI，读同一份 php.ini/conf.d，因此它的
// 回读值就是 FPM 拿到的那份配置。没有 php-cgi 时退回 `php -r`，并在结果里把
// max_execution_time 标为 CLI（界面据此说明"这一项 CLI 复核不准"）。
func ProbePHPLimits(binary string) (map[string]string, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return nil, fmt.Errorf("PHP 二进制路径为空，无法回读生效值")
	}
	if _, err := os.Stat(binary); err != nil {
		return nil, fmt.Errorf("PHP 二进制不存在：%s（该版本可能已卸载）", binary)
	}
	code := `echo json_encode([
  "upload_max_filesize"=>ini_get("upload_max_filesize"),
  "post_max_size"=>ini_get("post_max_size"),
  "memory_limit"=>ini_get("memory_limit"),
  "max_execution_time"=>ini_get("max_execution_time"),
]);`

	// 探针脚本写成临时文件（php-cgi 只接受文件，不支持 -r）。
	f, err := os.CreateTemp("", "zp-php-limits-*.php")
	if err != nil {
		return nil, fmt.Errorf("创建 PHP 回读探针失败: %w", err)
	}
	probe := f.Name()
	defer func() { _ = os.Remove(probe) }()
	if _, err := f.WriteString("<?php\n" + code + "\n"); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("写入 PHP 回读探针失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("写入 PHP 回读探针失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	sapi := "php"
	var out []byte
	// 优先 php-cgi（非 CLI SAPI，max_execution_time 不被强制成 0）
	cgi := filepath.Join(filepath.Dir(binary), "php-cgi")
	if st, err := os.Stat(cgi); err == nil && !st.IsDir() {
		sapi = "php-cgi"
		out, err = exec.CommandContext(ctx, cgi, "-q", "-f", probe).Output()
		if err != nil {
			return nil, fmt.Errorf("执行 %s 回读配置失败: %w", cgi, err)
		}
	} else {
		out, err = exec.CommandContext(ctx, binary,
			"-d", "error_reporting=0", "-d", "display_errors=0", "-f", probe).Output()
		if err != nil {
			return nil, fmt.Errorf("执行 %s 回读配置失败: %w", binary, err)
		}
	}
	text := strings.TrimSpace(string(out))
	// 取最后一段 JSON：某些 SAPI/警告会在前面输出别的内容。
	if i := strings.LastIndex(text, "{"); i > 0 {
		text = text[i:]
	}
	vals := map[string]string{}
	if err := json.Unmarshal([]byte(text), &vals); err != nil {
		return nil, fmt.Errorf("解析 %s 的回读输出失败（输出：%s）: %w", sapi, truncateForErr(text), err)
	}
	vals["_sapi"] = sapi
	return vals, nil
}

// truncateForErr 把可能很长的命令输出截断进错误信息，别让报错本身失控。
func truncateForErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// FindClientMaxBodySize 从一份 nginx 配置文本里找出**生效的**
// client_max_body_size（跳过注释行），返回值与行号；没有则返回 ("", 0)。
//
// 用于回读"面板生成的 vhost 到底写了什么"，与 findListenDirective 同一套语义。
func FindClientMaxBodySize(content string) (string, int) {
	for i, ln := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(trimmed, "client_max_body_size")
		if !ok {
			continue
		}
		// nginx 的写法是 `client_max_body_size 512m;`（无 `=`）；这里也容忍
		// 有人写成 `client_max_body_size = 512m;`，两种都要能读出来。
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "="))
		v := strings.TrimSuffix(rest, ";")
		if j := strings.IndexAny(v, ";#"); j >= 0 {
			v = strings.TrimSpace(v[:j])
		}
		if v != "" {
			return v, i + 1
		}
	}
	return "", 0
}
