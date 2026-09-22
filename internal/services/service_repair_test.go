package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  "安装了但服务没注册"这一类的门禁（坑 231）
//
//  用户报障：whisper.cpp 卡片写着「已安装·服务未注册」，提示却是
//  "面板会用 brew services start 补上" —— 而它的服务标签 com.zizdog.stt 是面板
//  安装器写的，brew 根本补不出来，用户照着点什么也不会发生。
//
//  这里锁死两件删了会再出事故的事：
//    ① 每个目录条目的"怎么修"都有唯一、真实的结论（hint 不许给出做不到的动作）；
//    ② 每个面板安装器的磁盘产物都被探测（漏掉 = 孤儿态看不见、574 MB 残留清不掉）。
// ============================================================================

// repairActions 是允许出现的动作集合（多了就是界面没接住的新状态）。
var repairActions = map[string]bool{
	ServiceRepairInstalledTab: true,
	ServiceRepairReinstall:    true,
	ServiceRepairEnvInstall:   true,
}

// TestServiceRepairForEveryCatalogAppIsTruthful 遍历目录，四条状态组合都要有真结论。
func TestServiceRepairForEveryCatalogAppIsTruthful(t *testing.T) {
	apps := Catalog()
	if len(apps) == 0 {
		t.Fatal("目录为空 —— 遍历没跑到，门禁等于没跑")
	}
	for _, a := range apps {
		for _, st := range []struct {
			name                       string
			installed, adopted, inLchd bool
		}{
			{"装了+没注册+无记录", true, false, false},
			{"装了+没注册+有记录", true, true, false},
			{"装了+已注册", true, false, true},
			{"没装", false, false, false},
		} {
			got := ServiceRepairFor(a, st.installed, st.adopted, st.inLchd)
			// no_daemon 的应用（phpMyAdmin / ffmpeg / python）与容器类（compose/docker）
			// 本来就没有 launchd 服务 —— 报"服务未注册"是纯粹的误导。
			wantNeeded := st.installed && !st.inLchd && !a.NoDaemon &&
				a.Kind != KindCompose && a.Kind != KindDocker
			if got.Needed != wantNeeded {
				t.Errorf("%s（%s）在 %s 状态下 Needed=%v，应为 %v",
					a.ID, a.Kind, st.name, got.Needed, wantNeeded)
			}
			if !got.Needed {
				if got.Action != "" || got.Hint != "" {
					t.Errorf("%s 在 %s 状态下不需要修，却带了 action=%q hint=%q",
						a.ID, st.name, got.Action, got.Hint)
				}
				continue
			}
			// 需要修 ⇒ 必须给一个**真实**的下一步（有动作 + 有人话）。
			if !repairActions[got.Action] {
				t.Errorf("%s 在 %s 状态下的 action=%q 不是已知动作（界面接不住）",
					a.ID, st.name, got.Action)
			}
			if strings.TrimSpace(got.Hint) == "" {
				t.Errorf("%s 在 %s 状态下没给提示（用户不知道下一步点哪里）", a.ID, st.name)
			}
			// 「已安装」Tab 启动这条只在"brew 能重建这个标签"时成立 ——
			// 它正是 whisper.cpp 那次假提示的根因，必须逐条复核。
			if got.Action == ServiceRepairInstalledTab {
				if !st.adopted {
					t.Errorf("%s 没有面板记录却让它去「已安装」Tab 点启动 —— 那里根本没有这张卡", a.ID)
				}
				if !brewCanRebuildLabel(a) {
					t.Errorf("%s 的标签 %q 不是 brew 能重建的，却给了 brew services 的修法",
						a.ID, a.ServiceLabel)
				}
			}
			if got.Action == ServiceRepairEnvInstall && !IsEnvComponent(a) {
				t.Errorf("%s 不是基础环境，却让它去「网站」页重装基础环境", a.ID)
			}
		}
	}
}

// TestServiceRepairWhisperCppIsNotBrewAdvice 是用户那次报障的逐字回归。
func TestServiceRepairWhisperCppIsNotBrewAdvice(t *testing.T) {
	stt, ok := FindApp(STTAppID)
	if !ok {
		t.Fatal("目录里找不到语音转文字条目 —— 门禁的前提没了")
	}
	got := ServiceRepairFor(stt, true, false, false)
	if got.Needed != true {
		t.Fatalf("whisper.cpp 装了但服务没注册，应该需要修，实际 %+v", got)
	}
	if got.Action != ServiceRepairReinstall {
		t.Errorf("whisper.cpp 的服务由面板安装器注册，只能重跑安装，实际 action=%q", got.Action)
	}
	if strings.Contains(got.Hint, "brew services start") {
		t.Errorf("提示又让用户去 brew services（他的服务标签是 %s，brew 补不出来）：%q",
			stt.ServiceLabel, got.Hint)
	}
	// 记录也还在的形态同样只能重装：brew services start whisper-cpp 建的是
	// homebrew.mxcl.whisper-cpp，跟面板要的 com.zizdog.stt 不是同一个东西。
	if g := ServiceRepairFor(stt, true, true, false); g.Action != ServiceRepairReinstall {
		t.Errorf("whisper.cpp 就算有面板记录也只能重跑安装（brew 补不出 %s），实际 %+v",
			stt.ServiceLabel, g)
	}
	// 反面：真的能靠 brew 补的（记录还在的 brew 标签）必须仍然给那条修法 ——
	// 否则这个门禁只是"永远说重装"，抓不到反向错误。
	brew, ok := FindApp("mariadb")
	if !ok {
		t.Fatal("目录里找不到 mariadb 条目")
	}
	if g := ServiceRepairFor(brew, true, true, false); g.Action != ServiceRepairInstalledTab {
		t.Errorf("brew 标签 + 有记录时应该去「已安装」Tab 点启动，实际 %+v", g)
	}
}

// TestInstallerArtifactProbeCoversEveryPanelInstaller 是"孤儿态/残留"那一半的门禁：
// 每个面板安装器要么有自己的产物路径，要么写明为什么不需要 —— 漏掉一个，
// 用户就会看到一个既没安装入口、也清不掉残留的卡片（stt 的 574 MB 模型就是这样）。
func TestInstallerArtifactProbeCoversEveryPanelInstaller(t *testing.T) {
	checked := 0
	for _, a := range Catalog() {
		if a.PanelInstaller == "" {
			continue
		}
		checked++
		if installerArtifactProbed(a.PanelInstaller) {
			continue
		}
		if strings.TrimSpace(installerArtifactExempt[a.PanelInstaller]) == "" {
			t.Errorf("面板安装器 %q（%s）没有产物探测、也没写明豁免理由 —— "+
				"卸载保留的数据会看不见也清不掉（补 installerArtifactPaths 或 installerArtifactExempt）",
				a.PanelInstaller, a.ID)
		}
	}
	if checked < 10 {
		t.Fatalf("只检查了 %d 个面板安装器，明显不对（目录没读到？）", checked)
	}
}

// TestInstallerArtifactProbeReadsDisk 钉住 stt 那条真实路径（正面 + 反面）。
func TestInstallerArtifactProbeReadsDisk(t *testing.T) {
	home := t.TempDir()
	if InstallerArtifactExists(home, "stt") {
		t.Error("空家目录不该报「有产物」（否则市场会凭空多出一个「残留数据」）")
	}
	models := filepath.Join(home, "stt", "models")
	if err := os.MkdirAll(models, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(models, "ggml-large-v3-turbo-q5_0.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !InstallerArtifactExists(home, "stt") {
		t.Error("模型目录存在时必须报有产物：卸载默认保留的就是它（否则用户清不掉）")
	}
}
