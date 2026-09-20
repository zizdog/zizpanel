// externalapp.go —— 外部应用的便宜探测 + 以它自己的身份申请访问。
//
// 探测只用回环健康端点 + 端口反查可执行文件：都不碰受保护路径（坑 191）。
// 自检（会弹窗的那一步）在用户显式申请后才发生，且**必须**由该应用自己的进程回答。
package permissions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"strings"
	"time"
)

// DetectedApp 是探测到的实例；Installed=false 时调用方**根本不渲染**这一块。
type DetectedApp struct {
	Spec      ExternalApp
	Installed bool
	Version   string
	ExecPath  string
	Owner     string
	Roots     []string
	Reason    string
	SigningID string
	Port      int
}

// HealthURL 是该实例的回环健康探测地址（主要判据）。
func (d DetectedApp) HealthURL() string {
	path := strings.TrimPrefix(strings.TrimSpace(d.Spec.HealthPath), "/")
	return fmt.Sprintf("http://127.0.0.1:%d/%s", d.Spec.Port, path)
}

// CmdResult 是一条外部命令的真实结果；Code=-1 表示命令没跑起来。
type CmdResult struct {
	Stdout string
	Stderr string
	Code   int
	Err    error
}

// RunFn 执行一条外部命令（单测全部注入假实现）。
type RunFn func(ctx context.Context, name string, args ...string) CmdResult

// Env 是探测/执行依赖的外部动作，单测逐项注入。
type Env struct {
	HTTPGet  func(ctx context.Context, url string) ([]byte, error)
	Run      RunFn
	UserHome func(user string) (string, error)
}

// DefaultEnv 返回真机实现。
func DefaultEnv() Env {
	return Env{HTTPGet: httpGetShort, Run: runCmd, UserHome: userHomeLookup}
}

func userHomeLookup(user string) (string, error) {
	u, err := osuser.Lookup(user)
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func httpGetShort(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 900 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("健康探测返回 %d", resp.StatusCode)
	}
	return body, nil
}

func runCmd(ctx context.Context, name string, args ...string) CmdResult {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := CmdResult{Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Code = ee.ExitCode()
		return res
	}
	res.Code = -1
	res.Err = err
	return res
}

// DetectApp 做便宜探测：回环健康端点为**主要**判据，再用监听端口反查可执行文件；
// 找不到可执行文件就视为未安装（不猜安装路径）。
func DetectApp(ctx context.Context, env Env, spec ExternalApp) DetectedApp {
	d := DetectedApp{Spec: spec, SigningID: spec.SigningID, Port: spec.Port}
	body, err := env.HTTPGet(ctx, d.HealthURL())
	if err != nil {
		d.Reason = fmt.Sprintf("回环 %d 健康探测失败（视为未安装）", spec.Port)
		return d
	}
	if !strings.Contains(strings.ToLower(strings.TrimSpace(string(body))), "ok") {
		d.Reason = fmt.Sprintf("回环 %d 的健康应答不是 ok（视为未安装）", spec.Port)
		return d
	}

	pid := firstLine(env.Run(ctx, "/usr/sbin/lsof", "-nP", "-ti", fmt.Sprintf("tcp:%d", spec.Port), "-sTCP:LISTEN").Stdout)
	if pid == "" {
		d.Reason = fmt.Sprintf("端口 %d 上找不到监听进程（视为未安装）", spec.Port)
		return d
	}
	d.Owner = strings.TrimSpace(env.Run(ctx, "/bin/ps", "-p", pid, "-o", "user=").Stdout)
	if d.Owner == "" {
		d.Reason = fmt.Sprintf("拿不到 %d 进程的属主（视为未安装）", spec.Port)
		return d
	}
	d.ExecPath = executableFromLsof(env.Run(ctx, "/usr/sbin/lsof", "-p", pid, "-a", "-d", "txt", "-Fn").Stdout)
	if d.ExecPath == "" {
		d.Reason = fmt.Sprintf("反查不到 %d 对应的可执行文件（视为未安装）", spec.Port)
		return d
	}

	if v := env.Run(ctx, d.ExecPath, "--version"); v.Code == 0 {
		d.Version = versionFromOutput(v.Stdout)
	}
	if cfg := appConfigPath(ctx, env, pid, d.Owner, spec); cfg != "" && spec.RootsArgs != nil {
		if r := env.Run(ctx, d.ExecPath, spec.RootsArgs(cfg)...); r.Code == 0 {
			d.Roots = parseRoots(r.Stdout)
		}
	}
	d.Installed = true
	return d
}

// versionFromOutput 取 `<应用名> <版本>` 一行的最后一段（不写死任何应用名）。
func versionFromOutput(out string) string {
	fields := strings.Fields(firstLine(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// appConfigPath 取实例自己用的 config.json：先看进程参数，再退到冻结的数据目录。
func appConfigPath(ctx context.Context, env Env, pid, owner string, spec ExternalApp) string {
	if p := flagValue(env.Run(ctx, "/bin/ps", "-p", pid, "-o", "args=").Stdout, "--config"); p != "" {
		return p
	}
	if env.UserHome == nil {
		return ""
	}
	home, err := env.UserHome(owner)
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(spec.DataSubdir), "config.json")
}

// executableFromLsof 从 `lsof -p PID -a -d txt -Fn` 里挑出主可执行文件：
// 跳过 /usr/lib 与 /System（dyld），并要求真的是个带执行位的普通文件。
func executableFromLsof(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "n") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "n"))
		if strings.HasPrefix(p, "/usr/lib/") || strings.HasPrefix(p, "/System/") {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p
	}
	return ""
}

// flagValue 从进程参数里取一个 flag 的值。
//
// 不能用 strings.Fields：macOS 的 "Application Support" 带空格，按空格切会把路径截断。
func flagValue(argsLine, name string) string {
	line := " " + strings.TrimSpace(argsLine) + " "
	eq := strings.Index(line, " "+name+"=")
	sp := strings.Index(line, " "+name+" ")
	cut := func(s string) string {
		if j := strings.Index(s, " -"); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	switch {
	case eq >= 0 && (sp < 0 || eq < sp):
		return cut(line[eq+len(name)+3:])
	case sp >= 0:
		return cut(line[sp+len(name)+2:])
	}
	return ""
}

type rootsJSON struct {
	Roots []string `json:"roots"`
}

func parseRoots(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r rootsJSON
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			return r.Roots
		}
	}
	return nil
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// CheckResult 是外部应用自己回答的一次自检结果。
type CheckResult struct {
	Supported bool
	Readable  bool
	Reason    string
}

// CheckAppAccess 以**真实用户**身份调用应用自己的 CLI，让弹窗由它的签名身份触发。
//
// 面板绝不自己去读那个目录：那样弹的是面板的授权，对该应用没用。
// 旧版本没有这个动词时如实报"不支持自检"，**不许**退化去读。
func CheckAppAccess(ctx context.Context, env Env, owner, execPath, verb, path string) (CheckResult, error) {
	if strings.TrimSpace(execPath) == "" {
		return CheckResult{}, errors.New("应用的可执行文件路径未知")
	}
	res := env.Run(ctx, "/usr/bin/sudo", "-n", "-u", owner, execPath, verb, path)
	if res.Err != nil {
		return CheckResult{}, fmt.Errorf("无法运行 %s：%w", execPath, res.Err)
	}
	if strings.Contains(res.Stderr, "sudo:") {
		return CheckResult{}, fmt.Errorf("以 %s 身份运行失败：%s", owner, strings.TrimSpace(firstLine(res.Stderr)))
	}
	if got, ok := parseAccessJSON(res.Stdout); ok {
		return CheckResult{Supported: true, Readable: got.Readable, Reason: got.Reason}, nil
	}
	return CheckResult{Supported: false, Reason: "该应用版本不支持自检，请升级"}, nil
}

type accessJSON struct {
	Path     string `json:"path"`
	Readable *bool  `json:"readable"`
	Reason   string `json:"reason"`
}

type accessAnswer struct {
	Readable bool
	Reason   string
}

func parseAccessJSON(out string) (accessAnswer, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var a accessJSON
		if err := json.Unmarshal([]byte(line), &a); err != nil || a.Readable == nil {
			continue
		}
		return accessAnswer{Readable: *a.Readable, Reason: a.Reason}, true
	}
	return accessAnswer{}, false
}
