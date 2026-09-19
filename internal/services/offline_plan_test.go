package services

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ============================================================================
//  离线包计划 + 离线模式闸门 的契约
//
//  用户原话（2026-09-16）：
//    "要确保应用市场里的所有软件都能顺利安装！有必要的话可以把所有文件卡点
//     都放到镜像站。甚至直接将软件'打包'一键迁移回 mac 系统里。"
//
//  这里锁两件事：
//    · OfflinePlan() 必须**逐条覆盖目录里的每个应用**（27 个），并且把
//      "已知缺口"如实写出来 —— 少一个应用就是"离线打包漏了它"；
//    · MirrorOfflineOnly/MirrorOfflinePreflight 的语义：
//      打开离线模式后**不回落**，缺资源必须返回面向用户的明确错误。
// ============================================================================

// TestOfflinePlanCoversEveryCatalogApp 保证计划不漏应用。
func TestOfflinePlanCoversEveryCatalogApp(t *testing.T) {
	catalog := Catalog()
	plan := OfflinePlan()
	if len(plan) != len(catalog) {
		t.Fatalf("离线计划里的应用数 %d != 目录里的 %d", len(plan), len(catalog))
	}
	seen := map[string]OfflineAppPlan{}
	for _, p := range plan {
		if p.ID == "" {
			t.Fatal("计划里出现了没有 ID 的条目")
		}
		if _, dup := seen[p.ID]; dup {
			t.Fatalf("计划里 %s 出现了两次", p.ID)
		}
		seen[p.ID] = p
	}
	for _, a := range catalog {
		if _, ok := seen[a.ID]; !ok {
			t.Errorf("目录里的 %s 不在离线计划里（离线打包会漏掉它）", a.ID)
		}
	}
}

// TestOfflinePlanTellsTheTruthAboutDelivery 保证每类安装方式都能被计划"接住"：
// 要么给出可镜像的 MirrorPath，要么给出**明确的缺口说明**（不许两者皆无）。
func TestOfflinePlanTellsTheTruthAboutDelivery(t *testing.T) {
	for _, p := range OfflinePlan() {
		hasMirror := false
		for _, a := range p.Artifacts {
			if a.Kind != OfflineKindOther && a.MirrorPath != "" {
				hasMirror = true
			}
		}
		switch p.InstallMethod {
		case "brew", "colima":
			if p.BrewFormula == "" {
				t.Errorf("%s 走 %s，但计划里没给 brew_formula —— 构建工具没法展开依赖",
					p.ID, p.InstallMethod)
			}
		case "binary_release":
			if !hasMirror {
				t.Errorf("%s 走 release 二进制，但计划里没有带 MirrorPath 的制品", p.ID)
			}
		case "compose":
			if len(p.DockerImages) == 0 {
				t.Errorf("%s 走 compose，但计划里没解析出任何镜像名", p.ID)
			}
			if len(p.Gaps) == 0 {
				t.Errorf("%s 的 compose 镜像 tar 还没镜像，必须写进 gaps（不许静默）", p.ID)
			}
		case "site":
			if len(p.Gaps) == 0 {
				t.Errorf("%s 的源码包还没镜像，必须写进 gaps（不许静默）", p.ID)
			}
		case "panel_installer":
			// 面板自研安装器：要么有额外文件，要么明确写"无需额外文件"，
			// 要么有已知缺口。三者必居其一 —— 什么都不说是最糟的。
			if len(p.Artifacts) == 0 && len(p.Gaps) == 0 {
				t.Errorf("%s 既没有制品也没有缺口说明：离线包会看起来"+
					"完整但其实没人知道缺什么", p.ID)
			}
		default:
			t.Errorf("%s 的安装方式 %q 没有对应的计划分支", p.ID, p.InstallMethod)
		}
	}
}

// TestBrewOfflineKeepsHomebrewNaming 锁住 brew 瓶的落盘名与 URL 名的区别。
//
// 历史：第一版把 url_encode 后的名字当成了落盘名（磁盘上真出现 `%40`），
// 本地核对全过、HTTP 侧 404。这条测试把那个区别写死在代码里。
func TestSiteTarballNameStable(t *testing.T) {
	if got := siteTarballName("typecho", "zip"); got != "typecho-latest.zip" {
		t.Errorf("建站包名不稳定：%s", got)
	}
	if got := siteTarballName("wordpress", ""); got != "wordpress-latest.zip" {
		t.Errorf("建站包名（缺 archive 时）应为 zip：%s", got)
	}
}

// TestMirrorOfflineOnlyGate 是离线模式的核心断言。
func TestMirrorOfflineOnlyGate(t *testing.T) {
	ctx := context.Background()

	// 默认（未打开）：什么都不拦，保持"优先+回落"的既有行为。
	mDefault := &Manager{opt: Options{MirrorBase: "https://mirror.example.com:8888"}}
	if mDefault.MirrorOfflineOnly(ctx) {
		t.Error("没打开设置项时不应报告处于离线模式")
	}
	if err := mDefault.MirrorOfflinePreflight(ctx, "x", "https://mirror.example.com:8888/x"); err != nil {
		t.Errorf("非离线模式下预检应当放行，却报错：%v", err)
	}

	// 打开了离线模式但没配镜像基址：自相矛盾，必须明确失败（不能悄悄走外网）。
	mNoBase := &Manager{opt: Options{OfflineOnly: true}}
	if !mNoBase.MirrorOfflineOnly(ctx) {
		t.Error("打开设置项后应报告处于离线模式")
	}
	err := mNoBase.MirrorOfflinePreflight(ctx, "Homebrew 瓶 ffmpeg", "")
	if err == nil {
		t.Fatal("离线模式 + 空镜像基址必须明确失败")
	}
	for _, want := range []string{"离线模式", "禁止回落外网", "mirror_base"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际：%s", want, err.Error())
		}
	}

	// 在线但镜像上缺件：离线模式下必须失败，且说清"缺的是哪个资源"。
	mMiss := &Manager{
		opt: Options{OfflineOnly: true, MirrorBase: "https://mirror.example.com:8888"},
		mirrorFileProbeOverride: func(context.Context, string) (int64, error) {
			return -1, errors.New("镜像站访问不了")
		},
	}
	err = mMiss.MirrorOfflinePreflight(ctx, "ffmpeg 瓶", "https://mirror.example.com:8888/brew/ffmpeg-x.bottle.tar.gz")
	if err == nil {
		t.Fatal("离线模式下镜像缺件必须失败（不许回落到公网）")
	}
	for _, want := range []string{"离线模式", "ffmpeg 瓶", "ffmpeg-x.bottle.tar.gz"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("缺件错误应包含 %q，实际：%s", want, err.Error())
		}
	}

	// 镜像上有：放行。
	mHit := &Manager{
		opt: Options{OfflineOnly: true, MirrorBase: "https://mirror.example.com:8888"},
		mirrorFileProbeOverride: func(context.Context, string) (int64, error) {
			return 123, nil
		},
	}
	if err := mHit.MirrorOfflinePreflight(ctx, "ffmpeg 瓶",
		"https://mirror.example.com:8888/brew/ffmpeg-x.bottle.tar.gz"); err != nil {
		t.Errorf("镜像上有这个资源时离线预检应放行，却报错：%v", err)
	}
}
