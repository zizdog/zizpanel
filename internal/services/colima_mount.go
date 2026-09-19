package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Colima 的挂载表：compose 数据必须落在宿主机看得见的目录里（D13）
//
//  问题（2026-09-16 两台机器真机确认）：
//    compose 项目放在 <WorkDir>/compose/<id>，而 compose 文件里的 `./data`
//    是**相对路径**，由虚拟机里的 docker 守护进程解析。Colima 默认只挂载
//    用户家目录（`~`），所以 <WorkDir>（默认 /opt/zizpanel/work）**不在挂载表里**，
//    数据实际写进了虚拟机的根盘：
//      · Mac 上 `ls <WorkDir>/compose/stirling-pdf/` 只有 docker-compose.yml；
//      · VM 里同一路径下却有 settings.yml + stirling-pdf-DB-2.3.232.mv.db。
//    后果：面板告诉用户的路径下没有数据、卸载时"删残留"只删掉空壳、
//    备份/迁移会漏掉全部真实数据、`colima delete` 会静默毁掉它们。
//
//  修法：把 <WorkDir> 显式写进 colima.yaml 的 `mounts`（实测有效）。
//  为什么不用 `colima start --mount`：那个 flag 会**覆盖**colima.yaml 里的
//  mounts 段（`--save-config` 默认 true），用户自己加的挂载会被抹掉；
//  而且它和 `--mount none` 之类的语义纠缠，不如直接改配置文件可控。
//
//  三条真机依据（都在 2026-09-16 本机验证过，别凭记忆改）：
//   1. mounts **不是** colima 的"固定配置"。源码 setFixedConfigs() 只锁
//      arch / vmType / runtime / mountType / network，mounts 可以后改；
//      实测在**同一个**虚拟机上 stop→start 后新挂载生效，**不需要重建 VM**。
//   2. colima.yaml 里的 `location: ~` 必须**带引号**。YAML 里裸 `~` 是 null，
//      colima 会把它读成空串，启动直接失败：
//        level=fatal msg="error starting vm: overlapping mounts not supported:
//                          '' overlaps '/opt/zizpanel/work/'"
//   3. 已经跑着的实例上 `colima start` 是**空操作**（源码 app.Active() →
//      "already running, ignoring"），所以改完 mounts 必须 stop→start 才生效。
//
//  对**已存在**的机器：挂载生效后，虚拟机里同名目录会被宿主机目录"盖住"。
//  所以先救数据（tar 到宿主机家目录下暂存）再改挂载，改完再把数据放回去，
//  全程写进任务步骤；救不出来就中止并回退配置 —— 绝不静默把用户数据藏起来。
// ============================================================================

// colimaStartTimeout 是 `colima start` 的超时。
//
// 冷启动要下 guest 镜像：Ubuntu 24.04 minimal arm64-docker 是 **332,354,401 B**。
// 实测（2026-09-16）公网 GitHub 只有 ~77 KB/s（≈71 分钟），5 分钟必然被掐断 ——
// 旧值就是 5 分钟，于是"VM 镜像还没下完就被杀"必然发生。
// 现在优先从自建镜像站 预热（本机↔镜像站实测 317MiB 秒级），但镜像站不通时会回落到
// GitHub，所以超时按"公网也能下完"给：90 分钟。
const colimaStartTimeout = 90 * time.Minute

// colimaMountWaitTimeout 是单条 VM 内命令（tar、find）的超时。
const colimaMountWaitTimeout = 3 * time.Minute

// colimaProfileName 从 docker socket 路径推断 colima profile（默认 default）。
func (m *Manager) colimaProfileName() string {
	parts := strings.Split(m.opt.DockerSocket, string(filepath.Separator))
	for i, p := range parts {
		if p == ".colima" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return "default"
}

// colimaInstanceName 是 Lima 实例名：默认 profile 叫 colima，其它叫 colima-<profile>。
func (m *Manager) colimaInstanceName() string {
	if p := m.colimaProfileName(); p != "" && p != "default" {
		return "colima-" + p
	}
	return "colima"
}

// colimaYAMLPath 是 Colima 的权威配置（面板改的就是它）。
func (m *Manager) colimaYAMLPath() string {
	if m.opt.UserHome == "" {
		return ""
	}
	return filepath.Join(m.opt.UserHome, ".colima", m.colimaProfileName(), "colima.yaml")
}

// colimaLimaYAMLPath 是 Colima 每次 start 从 colima.yaml 生成的 Lima 配置 ——
// **挂载表最终生效在这里**，所以它是"挂载到底有没有生效"的判据。
func (m *Manager) colimaLimaYAMLPath() string {
	if m.opt.UserHome == "" {
		return ""
	}
	return filepath.Join(m.opt.UserHome, ".colima", "_lima", m.colimaInstanceName(), "lima.yaml")
}

// colimaMountLocation 返回必须挂进虚拟机的宿主机目录（compose 数据的根）。
func (m *Manager) colimaMountLocation() string {
	if m.opt.WorkDir == "" {
		return ""
	}
	return filepath.Clean(m.opt.WorkDir)
}

// ---------- mounts 段的解析与改写 ----------

var mountLocationValueRe = regexp.MustCompile(`location:\s*(?:"([^"]*)"|'([^']*)'|([^\s#]+))`)

// mountsBlock 返回 `mounts:` 段所在的行区间 [start, end)（end 不含）。
// 找不到返回 (-1, -1)。与 colima.yaml / lima.yaml 的实际格式严格配对。
func mountsBlock(lines []string) (int, int) {
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "mounts:") {
			continue
		}
		// `mounts: []` 这种行内空值写法：整段就在这一行
		if strings.Contains(trimmed, "[]") {
			return i, i + 1
		}
		base := len(ln) - len(strings.TrimLeft(ln, " "))
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if len(lines[j])-len(strings.TrimLeft(lines[j], " ")) <= base {
				return i, j
			}
		}
		return i, len(lines)
	}
	return -1, -1
}

// parseMountLocations 读出 mounts 段里所有 location（原样，未展开 ~）。
func parseMountLocations(text string) []string {
	lines := strings.Split(text, "\n")
	start, end := mountsBlock(lines)
	if start < 0 {
		return nil
	}
	var out []string
	for _, ln := range lines[start:end] {
		m := mountLocationValueRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		for _, g := range m[1:] {
			if g != "" {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// normalizeMountLocation 把 location 归一化成可比较的绝对路径（用于去重）。
func normalizeMountLocation(loc, home string) string {
	l := strings.Trim(strings.TrimSpace(loc), `"'`)
	l = strings.TrimRight(l, "/")
	switch {
	case l == "~" || l == "":
		return strings.TrimRight(home, "/")
	case strings.HasPrefix(l, "~/"):
		return filepath.Join(home, strings.TrimPrefix(l, "~/"))
	}
	return l
}

// yamlQuoteValue 在需要时给 YAML 标量加双引号。
// `~` 必须加：裸 `~` 在 YAML 里是 null，colima 会读成空串并启动失败。
func yamlQuoteValue(s string) string {
	if s != "" && !strings.ContainsAny(s, "~ \t:#,\"'[]{}&*!|>%@`") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// upsertColimaMounts 保证 mounts 段里既有 `"~"`（Colima 默认挂的家目录）
// 又有 extra 里的目录，且**保留用户已配置的其它挂载**。
// 返回改写后的文本以及是否真的改了。
func upsertColimaMounts(text string, extra []string, home string) (string, bool) {
	lines := strings.Split(text, "\n")
	start, end := mountsBlock(lines)
	wantWrite := []string{"~"}
	wantWrite = append(wantWrite, extra...)

	if start < 0 {
		// 老版本配置里没有 mounts 段：追加到文件末尾。
		out := append([]string{}, lines...)
		out = append(out, "", "mounts:")
		for _, w := range wantWrite {
			out = append(out, "  - location: "+yamlQuoteValue(w), "    writable: true")
		}
		return strings.Join(out, "\n"), true
	}

	have := map[string]bool{}
	for _, l := range parseMountLocations(text) {
		have[normalizeMountLocation(l, home)] = true
	}
	var missing []string
	for _, w := range wantWrite {
		if !have[normalizeMountLocation(w, home)] {
			missing = append(missing, w)
		}
	}
	if len(missing) == 0 {
		return text, false
	}

	var out []string
	inlineEmpty := end == start+1 && strings.Contains(lines[start], "[]")
	if inlineEmpty {
		// `mounts: []` 后面不能直接跟列表项，必须换成 `mounts:`
		out = append(out, lines[:start]...)
		out = append(out, "mounts:")
	} else {
		out = append(out, lines[:end]...)
	}
	for _, w := range missing {
		out = append(out, "  - location: "+yamlQuoteValue(w), "    writable: true")
	}
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), true
}

// colimaWorkDirMounted 报告 Lima 的挂载表里是否已经有 compose 数据根目录。
func (m *Manager) colimaWorkDirMounted() bool {
	loc := m.colimaMountLocation()
	if loc == "" {
		return false
	}
	p := m.colimaLimaYAMLPath()
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	for _, l := range parseMountLocations(string(b)) {
		if normalizeMountLocation(l, m.opt.UserHome) == loc {
			return true
		}
	}
	return false
}

// ---------- 迁移（只对已存在的虚拟机） ----------

// colimaVMHasWorkDirData 报告虚拟机内 <WorkDir> 下有没有非空内容。
// **只有在挂载尚未生效时调用才有意义**：挂载生效后这里看到的就是宿主机目录。
func (m *Manager) colimaVMHasWorkDirData(ctx context.Context) bool {
	loc := m.colimaMountLocation()
	if loc == "" {
		return false
	}
	out, err := m.runColima(ctx, time.Minute, "ssh", "--", "sh", "-c",
		"test -d "+shellQuote(loc)+" && find "+shellQuote(loc)+" -mindepth 1 -maxdepth 4 -print -quit 2>/dev/null")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// shellQuote 用单引号包住一个参数（只允许出现在拼给 sh -c 的脚本里）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// colimaRescueWorkDir 把虚拟机内 <WorkDir> 的内容打成 tar 落到宿主机家目录下。
//
// 为什么落在家目录：家目录**一定**在 virtiofs 挂载表里，是唯一保证能写回宿主机的路径。
// 返回暂存目录（宿主机路径）。
func (m *Manager) colimaRescueWorkDir(ctx context.Context) (string, error) {
	loc := m.colimaMountLocation()
	home := m.opt.UserHome
	if loc == "" || home == "" {
		return "", fmt.Errorf("未知 compose 数据目录或用户家目录，无法迁移")
	}
	staging := filepath.Join(home, ".zizpanel", "vm-rescue", time.Now().Format("20060102-150405"))
	script := "set -e; mkdir -p " + shellQuote(staging) +
		"; tar -czf " + shellQuote(filepath.Join(staging, "vm-workdir.tgz")) +
		" -C " + shellQuote(loc) + " . ; echo rescued"
	if out, err := m.runColima(ctx, colimaMountWaitTimeout, "ssh", "--", "sh", "-c", script); err != nil {
		return "", fmt.Errorf("从虚拟机导出旧数据失败：%v（%s）", err, tailText(out, 200))
	}
	return staging, nil
}

// colimaRestoreWorkDir 把救出来的数据放回（此时挂载已生效，写的就是宿主机目录）。
func (m *Manager) colimaRestoreWorkDir(ctx context.Context, staging string) error {
	loc := m.colimaMountLocation()
	tgz := filepath.Join(staging, "vm-workdir.tgz")
	script := "set -e; mkdir -p " + shellQuote(loc) + "; tar -xzf " + shellQuote(tgz) + " -C " + shellQuote(loc) + "; echo restored"
	if out, err := m.runColima(ctx, colimaMountWaitTimeout, "ssh", "--", "sh", "-c", script); err != nil {
		return fmt.Errorf("把旧数据放回 %s 失败：%v（%s）；原始数据仍在 %s 里，请手工恢复", loc, err, tailText(out, 200), tgz)
	}
	return nil
}

// ApplyColimaWorkDirMount 保证 compose 数据根目录在虚拟机里也是宿主机上的真实目录。
//
// 幂等；在任何 `colima start` 之前调用。四种情形：
//  1. 还没装过 Colima（读不到 colima.yaml）→ 什么都不做；
//  2. 还没建过虚拟机 → 只写配置，首次 start 自然带上挂载；
//  3. 挂载已生效 → 只写配置（保证幂等），不再动 VM；
//  4. 虚拟机已存在但挂载没生效 → 需要一次 stop→start；若 VM 里已经有数据，
//     **先救出来再改挂载，改完放回去**，救不出来就回退配置并报错。
func (m *Manager) ApplyColimaWorkDirMount(ctx context.Context) error {
	loc := m.colimaMountLocation()
	cfg := m.colimaYAMLPath()
	if loc == "" || cfg == "" {
		return nil
	}
	orig, err := os.ReadFile(cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 还没装 Colima
		}
		return fmt.Errorf("读取 %s 失败：%w", cfg, err)
	}
	updated, changed := upsertColimaMounts(string(orig), []string{loc}, m.opt.UserHome)
	if changed {
		if err := m.writeColimaConfig(cfg, []byte(updated)); err != nil {
			return fmt.Errorf("写入 %s 失败：%w", cfg, err)
		}
		emit(ctx, tasks.LevelStep, "已把 compose 数据目录写进 Colima 挂载配置："+loc+"（"+cfg+"）")
	}
	if m.colimaWorkDirMounted() {
		emit(ctx, tasks.LevelStep, "compose 数据目录已挂载为宿主机真实目录："+loc)
		return nil
	}
	running, _, found := m.colimaFastState()
	if !found {
		emit(ctx, tasks.LevelStep, "虚拟机还没创建：挂载会在首次 colima start 时自动生效")
		return nil
	}

	// 情形 4：需要一次 stop→start。先把 VM 里的旧数据救出来（D13 的历史数据）。
	if !running {
		emit(ctx, tasks.LevelStep, "虚拟机当前是停止的：先启动一次，以便把 VM 内的旧数据导出到宿主机…")
		if _, err := m.runColima(ctx, colimaStartTimeout, "start"); err != nil {
			return fmt.Errorf("启动虚拟机失败（为了导出旧数据）：%w", err)
		}
	}
	staging := ""
	if m.colimaVMHasWorkDirData(ctx) {
		emit(ctx, tasks.LevelWarn, "检测到 "+loc+" 里有只存在于虚拟机内的数据（旧版面板写进去的），先导出到宿主机…")
		staging, err = m.colimaRescueWorkDir(ctx)
		if err != nil {
			// 救不出来就**不动挂载**，否则旧数据会被挂载点盖住、在面板里凭空消失。
			if changed {
				_ = m.writeColimaConfig(cfg, orig)
			}
			return fmt.Errorf("%w；为避免把已有数据盖住，本次**未**改挂载配置，请先把数据导出后再重试", err)
		}
		emit(ctx, tasks.LevelStep, "旧数据已导出到 "+staging+"（宿主机路径，确认无误后可删）")
	}
	emit(ctx, tasks.LevelStep, "正在重启 Docker 虚拟机让挂载生效（stop→start，容器会短暂中断）…")
	if _, err := m.runColima(ctx, colimaStartTimeout, "stop"); err != nil {
		return fmt.Errorf("停止虚拟机失败：%w", err)
	}
	if _, err := m.runColima(ctx, colimaStartTimeout, "start"); err != nil {
		return fmt.Errorf("重启虚拟机失败：%w", err)
	}
	if !m.colimaWorkDirMounted() {
		return fmt.Errorf("重启后挂载仍未生效（%s 不在 %s 里），请检查 Colima 配置", loc, m.colimaLimaYAMLPath())
	}
	emit(ctx, tasks.LevelOK, "compose 数据目录已挂载为宿主机真实目录："+loc)
	if staging != "" {
		if err := m.colimaRestoreWorkDir(ctx, staging); err != nil {
			return err
		}
		emit(ctx, tasks.LevelOK, "旧数据已放回 "+loc+"（备份仍保留在 "+staging+"）")
	}
	return nil
}

// ColimaDataPathHint 返回给用户看的"这个应用的数据到底落在哪"，装进安装步骤里。
//
// 为什么值得单独说：D13 的教训就是"面板说一个路径、数据在另一个路径"。
// 把宿主机真实路径写进日志，用户才能自己去看、去备份。
func (m *Manager) ColimaDataPathHint(appID string) string {
	base := m.composeDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, appID) + "（宿主机真实目录，经 virtiofs 挂进虚拟机；" +
		"compose 里的 ./data 就是它下面的 data/）"
}

// colimaWorkDirMountWarning 在"运行时会写进虚拟机内部"时返回一条要写进安装步骤的警告。
//
// 为什么是警告而不是自动修：修好它需要一次 stop→start（所有容器都会中断），
// 而安装单个应用时用户没有预期会被重启运行时。真正把它修好的是
// ApplyColimaWorkDirMount —— 它在「安装 Docker 运行时」和「服务管理里启动运行时」
// 两条路径上都会执行（那两处用户本来就预期会动虚拟机）。
func (m *Manager) colimaWorkDirMountWarning() string {
	if !m.ColimaInstalled() {
		return ""
	}
	loc := m.colimaMountLocation()
	if loc == "" {
		return ""
	}
	if m.colimaWorkDirMounted() {
		return ""
	}
	_, _, found := m.colimaFastState()
	if !found {
		return "" // 还没建过虚拟机：首次 start 会带上挂载
	}
	return "compose 数据目录 " + loc + " **不在** Docker 虚拟机的挂载表里 —— " +
		"数据会写进虚拟机内部（Mac 上看不见、备份会漏、删虚拟机会一起没）。" +
		"到「服务管理 → Docker 运行时」点一次「重启」即可挂上（会重启容器）"
}
