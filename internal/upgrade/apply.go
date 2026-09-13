package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Names 是面板与助手的可执行文件名。
const (
	PanelBinary  = "zizpanel"
	HelperBinary = "zizpanel-helper"
)

// WatchdogLabel 是升级看门狗的 LaunchDaemon 标签。
const WatchdogLabel = "cn.zizpanel.upgrade-watchdog"

// Options 是应用升级所需的环境信息。
//
// 所有外部副作用（执行命令、时间）都做成可注入字段，
// 这样回滚逻辑可以在没有 root、没有 launchd 的环境里被测试 ——
// 而回滚恰恰是最需要被测到的部分。
type Options struct {
	Root      string // 安装根目录，如 /opt/zizpanel
	BinDir    string // 二进制目录
	WorkDir   string // 工作目录（staging / state 都在这里）
	PlistDir  string // LaunchDaemon 目录（可覆盖，便于测试）
	Label     string // 面板的 launchd 标签
	HealthURL string // 健康检查地址

	// Run 执行外部命令，默认走 exec.CommandContext。
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Now 取当前时间，便于测试。
	Now func() time.Time

	// 看门狗的重试次数。生产用默认值（约 90 秒 / 60 秒），
	// 测试里调小，否则一个回滚测试要跑两分半。
	// 抽成参数而不是让测试去改脚本文本，是为了让被测试的代码路径
	// 与生产完全一致 —— 只改数字，不改逻辑。
	HealthTries   int
	RollbackTries int
}

func (o *Options) withDefaults() {
	if o.BinDir == "" {
		o.BinDir = filepath.Join(o.Root, "bin")
	}
	if o.WorkDir == "" {
		o.WorkDir = filepath.Join(o.Root, "work")
	}
	if o.PlistDir == "" {
		o.PlistDir = "/Library/LaunchDaemons"
	}
	if o.Label == "" {
		o.Label = "cn.zizpanel.panel"
	}
	if o.Run == nil {
		o.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HealthTries <= 0 {
		o.HealthTries = 90
	}
	if o.RollbackTries <= 0 {
		o.RollbackTries = 60
	}
}

// stagingDir 返回暂存目录。
func (o *Options) stagingDir() string { return filepath.Join(o.WorkDir, "upgrade", "staging") }

// Apply 执行升级的最后阶段：自检 → 备份 → 原子替换 → 启动看门狗 → 重启。
//
// 返回 nil 不代表"升级成功"，只代表"已经把控制权交给看门狗"。
// 真正的结论由看门狗写入 result.txt，面板重启后通过 LoadState 读到。
//
// 为什么必须这样设计：验证新版是否可用，只能在新版真的起来之后做；
// 而那时发出验证指令的旧进程已经被杀掉了。所以必须有一个**独立于面板进程**
// 的角色来完成验证与回滚 —— 这就是看门狗存在的唯一理由。
func Apply(ctx context.Context, opt Options, staged map[string]string, from, to, source string) error {
	opt.withDefaults()

	// 开始新尝试：先清掉上一次遗留的看门狗结论，
	// 再生成新的 RunID —— 之后只有带这个 ID 的结论才会被采纳。
	ClearResult(opt.WorkDir)

	st := &State{}
	st.Status = StatusApplying
	st.From, st.To, st.Source = from, to, source
	st.RunID = fmt.Sprintf("%d", opt.Now().UnixNano())
	st.StartedAt = opt.Now()
	st.FinishedAt = time.Time{}
	st.Error = ""
	st.Steps = nil
	addStep(opt, st, "开始升级", fmt.Sprintf("从 %s 升级到 %s（来源：%s）", from, to, source), true)

	if err := SaveState(opt.WorkDir, st); err != nil {
		return err
	}

	// ---- 第 1 步：自检暂存的二进制 ----
	// 必须在**替换之前**确认新二进制真的能跑。装了跑不起来的二进制，
	// 就只能指望看门狗回滚了，那是最后一道防线而不是第一道。
	if err := selfTest(ctx, opt, staged, to, st); err != nil {
		st.Status = StatusFailed
		st.Error = err.Error()
		st.Message = "新版自检未通过，已中止（旧版本未做任何改动）"
		st.FinishedAt = opt.Now()
		addStep(opt, st, "新版自检", err.Error(), false)
		_ = SaveState(opt.WorkDir, st)
		return err
	}
	addStep(opt, st, "新版自检", "二进制可执行且版本号正确", true)
	_ = SaveState(opt.WorkDir, st)

	// ---- 第 2 步：备份现有二进制 ----
	if err := backup(opt, st); err != nil {
		st.Status = StatusFailed
		st.Error = err.Error()
		st.Message = "备份旧版本失败，已中止（旧版本未做任何改动）"
		st.FinishedAt = opt.Now()
		addStep(opt, st, "备份旧版本", err.Error(), false)
		_ = SaveState(opt.WorkDir, st)
		return err
	}
	addStep(opt, st, "备份旧版本", "已备份为 *.bak", true)
	_ = SaveState(opt.WorkDir, st)

	// ---- 第 3 步：原子替换 ----
	if err := swap(opt, staged, st); err != nil {
		// 替换中途失败：立刻尝试把备份换回去，不能让面板处于"半个新版本"的状态
		st.Error = err.Error()
		addStep(opt, st, "替换二进制", err.Error(), false)
		if rbErr := restore(opt); rbErr != nil {
			st.Status = StatusFailed
			st.Message = "替换失败且回滚也失败，面板可能无法启动"
			st.Error = err.Error() + "；回滚失败: " + rbErr.Error()
		} else {
			st.Status = StatusRolledBack
			st.Message = "替换失败，已就地恢复旧版本"
		}
		st.FinishedAt = opt.Now()
		_ = SaveState(opt.WorkDir, st)
		return err
	}
	addStep(opt, st, "替换二进制", "已完成原子替换", true)
	_ = SaveState(opt.WorkDir, st)

	// ---- 第 4 步：启动看门狗（必须在重启自己之前） ----
	if err := startWatchdog(ctx, opt, st, from, to); err != nil {
		// 看门狗起不来就不能重启自己 —— 否则新版若失败就没人回滚了
		st.Error = err.Error()
		addStep(opt, st, "启动看门狗", err.Error(), false)
		if rbErr := restore(opt); rbErr != nil {
			st.Status = StatusFailed
			st.Message = "看门狗启动失败且回滚失败，面板可能无法启动"
			st.Error = err.Error() + "；回滚失败: " + rbErr.Error()
		} else {
			st.Status = StatusRolledBack
			st.Message = "看门狗启动失败，已恢复旧版本（未重启）"
		}
		st.FinishedAt = opt.Now()
		_ = SaveState(opt.WorkDir, st)
		return err
	}
	addStep(opt, st, "启动看门狗", "验证与回滚由独立守护进程负责", true)

	// ---- 第 5 步：重启面板 ----
	// 这一步会杀掉我们自己，所以放在最后，且之后不能再依赖任何内存状态。
	st.Status = StatusRestarting
	st.Stage = "正在重启面板"
	st.Message = "新版已就位，正在重启并验证"
	_ = SaveState(opt.WorkDir, st)

	if _, err := opt.Run(ctx, "launchctl", "kickstart", "-k", "system/"+opt.Label); err != nil {
		// kickstart 失败：看门狗仍在运行，它会发现健康检查通不过并回滚。
		// 这里只记录，不返回错误（返回也没人接得住了）。
		addStep(opt, st, "重启面板", "kickstart 失败："+err.Error(), false)
		_ = SaveState(opt.WorkDir, st)
	}
	return nil
}

func addStep(opt Options, st *State, stage, message string, ok bool) {
	st.Steps = append(st.Steps, Step{At: opt.Now(), Stage: stage, Message: message, OK: ok})
}

// selfTest 在暂存目录里试跑新二进制。
//
// 检查两件事：能执行（架构匹配、没有损坏）且版本号正确
// （防止把旧包或不相干的包装上去）。
func selfTest(ctx context.Context, opt Options, staged map[string]string, want string, st *State) error {
	panel, ok := staged[PanelBinary]
	if !ok {
		return errors.New("暂存目录里没有面板主程序")
	}
	out, err := opt.Run(ctx, panel, "version", "--json")
	if err != nil {
		return fmt.Errorf("新版主程序无法执行: %v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("新版主程序版本输出无法解析: %v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	if !sameVersion(v.Version, want) {
		return fmt.Errorf("新版主程序报告的版本是 %q，与期望的 %q 不一致", v.Version, want)
	}

	helper, ok := staged[HelperBinary]
	if !ok {
		return errors.New("暂存目录里没有提权助手")
	}
	out, err = opt.Run(ctx, helper, "selftest")
	if err != nil {
		return fmt.Errorf("新版提权助手无法执行: %v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	var h struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return fmt.Errorf("新版提权助手输出无法解析: %v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	if !h.OK {
		return fmt.Errorf("新版提权助手自检未通过: %s", strings.TrimSpace(string(out)))
	}
	_ = st
	return nil
}

// sameVersion 比较两个版本串，忽略 v 前缀与首尾空白。
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// backup 把现有二进制复制为 *.bak。
func backup(opt Options, st *State) error {
	for _, name := range []string{PanelBinary, HelperBinary} {
		src := filepath.Join(opt.BinDir, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("找不到现有程序 %s，拒绝在状态不明的情况下升级", src)
			}
			return err
		}
		if err := copyFile(src, src+".bak", 0o755); err != nil {
			return fmt.Errorf("备份 %s 失败: %w", name, err)
		}
	}
	_ = st
	return nil
}

// swap 用暂存文件原子替换安装目录里的二进制。
func swap(opt Options, staged map[string]string, st *State) error {
	for _, name := range []string{PanelBinary, HelperBinary} {
		src, ok := staged[name]
		if !ok {
			return fmt.Errorf("暂存目录缺少 %s", name)
		}
		dst := filepath.Join(opt.BinDir, name)
		// 先在同一目录里落一个临时文件再 rename：
		// 跨文件系统的 rename 不是原子的，所以临时文件必须与目标同目录。
		tmp := dst + ".new"
		if err := copyFile(src, tmp, 0o755); err != nil {
			return fmt.Errorf("准备 %s 失败: %w", name, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("替换 %s 失败: %w", name, err)
		}
	}
	_ = st
	return nil
}

// restore 把 *.bak 还原回去。用于"替换失败"与看门狗回滚两条路径。
func restore(opt Options) error {
	var firstErr error
	for _, name := range []string{PanelBinary, HelperBinary} {
		bak := filepath.Join(opt.BinDir, name+".bak")
		if _, err := os.Stat(bak); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("找不到备份 %s，无法回滚", bak)
			}
			continue
		}
		if err := copyFile(bak, filepath.Join(opt.BinDir, name), 0o755); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ProbeVersion 运行一个（尚未安装的）面板二进制，读出它报告的版本。
//
// 用在"手动上传升级包"这条路径上：上传完立刻告诉用户"你传的是 v0.2.0"，
// 而不是等到替换完、重启完才发现传错了文件。
// 同时它天然验证了"这个二进制能在本机执行"（架构匹配、没有损坏）。
func ProbeVersion(ctx context.Context, binPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "version", "--json").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("无法解析版本输出：%v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	if v.Version == "" {
		return "", errors.New("二进制没有报告版本号")
	}
	return v.Version, nil
}
