package web

// api_video_pipeline_gate_test.go —— 「按源类型给提示」的接口与前端接线门禁。
//
// 纯判据那一半在 internal/videoopt/pipeline_gate_test.go；这里管两件事：
//  ① 接口**真的把字段下发**（走 handleFileVideoPlan，不是只测纯函数）：pipeline /
//     pipeline_reason / 三个计数 / 逐行结构化事实，含未知与递归混合；
//  ② 前端计划区**只按后端字段渲染**（不许在 JS 里硬编码 4K/10bit 判据），
//     且四类提示分支都在。
//
// 变异测试（必须变红）：把 low_bitrate 与 4k_10bit_highbitrate 判反、
// 把"位深读不到"当成 fast、删掉前端某类分支。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// videoPlanProbe 只取这条门禁关心到的字段（其余照常由既有门禁覆盖）。
type videoPlanProbe struct {
	OK   bool `json:"ok"`
	Data struct {
		Available        bool            `json:"available"`
		Pipeline         string          `json:"pipeline"`
		PipelineReason   string          `json:"pipeline_reason"`
		PipelineFast     int             `json:"pipeline_fast"`
		PipelineSoftware int             `json:"pipeline_software"`
		PipelineUnknown  int             `json:"pipeline_unknown"`
		Rows             []videoopt.Plan `json:"rows"`
	} `json:"data"`
}

// planVideoDir 走真 handler 规划一个目录（假 Runner 的 Probe 按完整路径/文件名回放）。
func planVideoDir(t *testing.T, srv *Server, dir string, body map[string]any) videoPlanProbe {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["dir"] = dir
	rec := postVideoJSON(t, srv.handleFileVideoPlan, body)
	if rec.Code != 200 {
		t.Fatalf("video-plan 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var got videoPlanProbe
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("计划响应不是 JSON：%v（%s）", err, rec.Body.String())
	}
	if !got.Data.Available {
		t.Fatalf("计划不可用（引擎未就绪）：%s", rec.Body.String())
	}
	return got
}

// writeVideoFixture 在 base 下造一个"视频"文件并登记假探测结论。
func writeVideoFixture(t *testing.T, base string, infos map[string]videoopt.MediaInfo, rel string, mi videoopt.MediaInfo) {
	t.Helper()
	path := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake-video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mi.FileBytes == 0 {
		mi.FileBytes = 16
	}
	infos[path] = mi
}

func TestVideoPipelineTipGate(t *testing.T) {
	t.Run("① 4K·10bit·高码率：接口下发 fast + 正确 reason + 结构化事实", func(t *testing.T) {
		srv, _ := newTestServer(t)
		infos := map[string]videoopt.MediaInfo{}
		writeVideoFixture(t, srv.Cfg.WWWRoot, infos, "fast/hi.mkv", videoopt.MediaInfo{
			Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283, FPS: 60,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infos})

		got := planVideoDir(t, srv, filepath.Join(srv.Cfg.WWWRoot, "fast"),
			map[string]any{"preset": "1080p", "encoder": "hardware"})
		if len(got.Data.Rows) != 1 {
			t.Fatalf("应恰好 1 行，实际 %d", len(got.Data.Rows))
		}
		row := got.Data.Rows[0]
		if row.Pipeline != videoopt.PipelineFast || row.PipelineReason != videoopt.PipelineReason4K10BitHighBitrate {
			t.Fatalf("行判据应为 fast/4k_10bit_highbitrate，实际 %s/%s", row.Pipeline, row.PipelineReason)
		}
		if row.SourceBitDepth != 10 || row.SrcBitDepth != 10 || row.SrcBitrateKbps != 25283 ||
			row.SourceFPS != 60 || row.SrcFPS != 60 || !row.CapFPS30 {
			t.Fatalf("结构化事实没下发全：depth=%d/%d kbps=%d fps=%v/%v cap=%v",
				row.SourceBitDepth, row.SrcBitDepth, row.SrcBitrateKbps, row.SourceFPS, row.SrcFPS, row.CapFPS30)
		}
		if got.Data.Pipeline != videoopt.PipelineFast || got.Data.PipelineReason != videoopt.PipelineReason4K10BitHighBitrate {
			t.Fatalf("整体结论应为 fast/4k_10bit_highbitrate，实际 %s/%s",
				got.Data.Pipeline, got.Data.PipelineReason)
		}
		if got.Data.PipelineFast != 1 || got.Data.PipelineSoftware != 0 || got.Data.PipelineUnknown != 0 {
			t.Fatalf("计数不对：fast=%d software=%d unknown=%d",
				got.Data.PipelineFast, got.Data.PipelineSoftware, got.Data.PipelineUnknown)
		}

		// 线上是 JSON，字段名本身也是契约：按**逐字键名**再断言一次
		// （解码进 Go 结构体证明不了键名没写错——踩过 source_* 与 src_* 不一致的坑）。
		dir := filepath.Join(srv.Cfg.WWWRoot, "fast")
		rec := postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": dir, "preset": "1080p", "encoder": "hardware"})
		var wire struct {
			Data struct {
				Pipeline         string `json:"pipeline"`
				PipelineReason   string `json:"pipeline_reason"`
				PipelineFast     int    `json:"pipeline_fast"`
				PipelineSoftware int    `json:"pipeline_software"`
				PipelineUnknown  int    `json:"pipeline_unknown"`
				Rows             []struct {
					Pipeline       string   `json:"pipeline"`
					PipelineReason string   `json:"pipeline_reason"`
					SrcFPS         *float64 `json:"src_fps"`
					SrcBitDepth    *int     `json:"src_bit_depth"`
					SrcBitrateKbps *int     `json:"src_bitrate_kbps"`
					CapFPS30       *bool    `json:"cap_fps30"`
				} `json:"rows"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatalf("响应不是 JSON：%v", err)
		}
		if wire.Data.Pipeline != "fast" || wire.Data.PipelineReason != "4k_10bit_highbitrate" ||
			wire.Data.PipelineFast != 1 {
			t.Fatalf("线上字段缺失/写错：%+v", wire.Data)
		}
		if len(wire.Data.Rows) != 1 {
			t.Fatalf("线上应有 1 行：%+v", wire.Data.Rows)
		}
		r0 := wire.Data.Rows[0]
		if r0.Pipeline != "fast" || r0.PipelineReason != "4k_10bit_highbitrate" {
			t.Errorf("行字段键名/取值不对：%+v", r0)
		}
		if r0.SrcFPS == nil || *r0.SrcFPS != 60 {
			t.Errorf("src_fps 没下发：%+v", r0.SrcFPS)
		}
		if r0.SrcBitDepth == nil || *r0.SrcBitDepth != 10 {
			t.Errorf("src_bit_depth 没下发：%+v", r0.SrcBitDepth)
		}
		if r0.SrcBitrateKbps == nil || *r0.SrcBitrateKbps != 25283 {
			t.Errorf("src_bitrate_kbps 没下发：%+v", r0.SrcBitrateKbps)
		}
		if r0.CapFPS30 == nil || !*r0.CapFPS30 {
			t.Errorf("cap_fps30 没下发：%+v", r0.CapFPS30)
		}
	})

	t.Run("② 8bit / 低码率 / 非 4K：software + 各自 reason", func(t *testing.T) {
		cases := []struct {
			name       string
			mi         videoopt.MediaInfo
			wantReason string
		}{
			{"4K 8bit 高码率", videoopt.MediaInfo{Width: 3840, Height: 1608, BitDepth: 8, VideoKbps: 25000}, videoopt.PipelineReasonBitDepth8},
			{"4K 10bit 低码率", videoopt.MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 3000}, videoopt.PipelineReasonLowBitrate},
			{"1080p 高码率", videoopt.MediaInfo{Width: 1920, Height: 1080, BitDepth: 10, VideoKbps: 25000}, videoopt.PipelineReasonNot4K},
		}
		for _, c := range cases {
			srv, _ := newTestServer(t)
			infos := map[string]videoopt.MediaInfo{}
			mi := c.mi
			mi.DurationSec, mi.HasVideo, mi.HasAudio, mi.FPS = 20, true, true, 24
			writeVideoFixture(t, srv.Cfg.WWWRoot, infos, "soft/a.mp4", mi)
			withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infos})

			got := planVideoDir(t, srv, filepath.Join(srv.Cfg.WWWRoot, "soft"),
				map[string]any{"preset": "480p"})
			row := got.Data.Rows[0]
			if row.Pipeline != videoopt.PipelineSoftware || row.PipelineReason != c.wantReason {
				t.Errorf("%s：应为 software/%s，实际 %s/%s", c.name, c.wantReason, row.Pipeline, row.PipelineReason)
			}
			if got.Data.Pipeline != videoopt.PipelineSoftware || got.Data.PipelineReason != c.wantReason {
				t.Errorf("%s：整体结论应为 software/%s，实际 %s/%s",
					c.name, c.wantReason, got.Data.Pipeline, got.Data.PipelineReason)
			}
			if got.Data.PipelineUnknown != 0 {
				t.Errorf("%s：判据可读，不该有 unknown（%d）", c.name, got.Data.PipelineUnknown)
			}
		}
	})

	t.Run("③ 缺位深 / 缺码率 ⇒ unknown（绝不猜 fast）", func(t *testing.T) {
		cases := []struct {
			name string
			mi   videoopt.MediaInfo
		}{
			{"4K 高码率但位深读不到", videoopt.MediaInfo{Width: 3840, Height: 1608, BitDepth: 0, VideoKbps: 25000}},
			{"4K 10bit 但码率读不到", videoopt.MediaInfo{Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 0}},
		}
		for _, c := range cases {
			srv, _ := newTestServer(t)
			infos := map[string]videoopt.MediaInfo{}
			mi := c.mi
			mi.DurationSec, mi.HasVideo, mi.HasAudio, mi.FPS = 20, true, true, 24
			writeVideoFixture(t, srv.Cfg.WWWRoot, infos, "unk/a.mp4", mi)
			withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infos})

			got := planVideoDir(t, srv, filepath.Join(srv.Cfg.WWWRoot, "unk"),
				map[string]any{"preset": "480p"})
			row := got.Data.Rows[0]
			if row.Pipeline == videoopt.PipelineFast {
				t.Errorf("%s：判据读不到时绝不许承诺快路（row=%s/%s）", c.name, row.Pipeline, row.PipelineReason)
			}
			if row.PipelineReason != videoopt.PipelineReasonUnknown {
				t.Errorf("%s：应为 unknown，实际 %s", c.name, row.PipelineReason)
			}
			if got.Data.PipelineReason != videoopt.PipelineReasonUnknown || got.Data.PipelineUnknown != 1 {
				t.Errorf("%s：整体应为 unknown 且计数 1，实际 %s/%d",
					c.name, got.Data.PipelineReason, got.Data.PipelineUnknown)
			}
		}
	})

	t.Run("④ 混合（含递归形态）：两类都在 ⇒ mixed + 各自计数", func(t *testing.T) {
		srv, _ := newTestServer(t)
		infos := map[string]videoopt.MediaInfo{}
		writeVideoFixture(t, srv.Cfg.WWWRoot, infos, "mix/a_hi.mkv", videoopt.MediaInfo{
			Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283, FPS: 60,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		writeVideoFixture(t, srv.Cfg.WWWRoot, infos, "mix/b_lo.mp4", videoopt.MediaInfo{
			Width: 1280, Height: 720, BitDepth: 8, VideoKbps: 800, FPS: 24,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infos})

		// 非递归：同层两类。
		got := planVideoDir(t, srv, filepath.Join(srv.Cfg.WWWRoot, "mix"), map[string]any{"preset": "480p"})
		if got.Data.Pipeline != "mixed" || got.Data.PipelineFast != 1 || got.Data.PipelineSoftware != 1 {
			t.Errorf("同层混合应为 mixed 且 fast=1/software=1，实际 %s/%d/%d",
				got.Data.Pipeline, got.Data.PipelineFast, got.Data.PipelineSoftware)
		}

		// 递归：子目录各一类，逐行带 rel_path（接口的递归形态同样下发判据）。
		srv2, _ := newTestServer(t)
		infos2 := map[string]videoopt.MediaInfo{}
		writeVideoFixture(t, srv2.Cfg.WWWRoot, infos2, "rec/sub1/a_hi.mkv", videoopt.MediaInfo{
			Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25283, FPS: 60,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		writeVideoFixture(t, srv2.Cfg.WWWRoot, infos2, "rec/sub2/b_lo.mp4", videoopt.MediaInfo{
			Width: 1280, Height: 720, BitDepth: 8, VideoKbps: 800, FPS: 24,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		writeVideoFixture(t, srv2.Cfg.WWWRoot, infos2, "rec/sub3/c_hi2.mkv", videoopt.MediaInfo{
			Width: 3840, Height: 1608, BitDepth: 10, VideoKbps: 25000, FPS: 24,
			DurationSec: 20, HasVideo: true, HasAudio: true,
		})
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100, infos: infos2})

		rec := planVideoDir(t, srv2, filepath.Join(srv2.Cfg.WWWRoot, "rec"),
			map[string]any{"preset": "480p", "recursive": true})
		if rec.Data.Pipeline != "mixed" || rec.Data.PipelineFast != 2 || rec.Data.PipelineSoftware != 1 {
			t.Errorf("递归混合应为 mixed 且 fast=2/software=1，实际 %s/%d/%d",
				rec.Data.Pipeline, rec.Data.PipelineFast, rec.Data.PipelineSoftware)
		}
		for _, r := range rec.Data.Rows {
			if r.RelPath == "" || r.Pipeline == "" || r.PipelineReason == "" {
				t.Errorf("递归行缺 rel_path 或判据字段：%+v", r)
			}
		}
	})

	t.Run("⑤ 前端接线：计划区只按后端字段渲染，四类文案分支都在", func(t *testing.T) {
		src := readAssetJS(t, "files.js")
		tip := jsFuncBody(t, src, "videoPipelineTip")

		// 用的是后端字段（pipeline / pipeline_reason / 三个计数），不是自己重算。
		for _, need := range []string{"plan.pipeline", "pipeline_reason", "pipeline_fast", "pipeline_software", "pipeline_unknown"} {
			if !strings.Contains(tip, need) {
				t.Errorf("videoPipelineTip 没读后端字段 %q —— 前端在猜管线", need)
			}
		}
		// 不许在 JS 里硬编码 4K/10bit/6Mbps 判据（这些阈值只能在 Go 里有一份）。
		for _, banned := range []string{"source_width", "source_bit_depth", "3000", "6000"} {
			if strings.Contains(tip, banned) {
				t.Errorf("videoPipelineTip 里出现判据痕迹 %q —— 前端不该自己判源类型", banned)
			}
		}
		// 四类提示分支的文案（快路 / 软解 / 混合计数 / 判据不足）都必须在。
		for _, branch := range []string{"此片源适合硬件管线", "此片源软件解码更快", "⚡ 硬件管线 ", "🐢 软件解码 ", "源信息不足"} {
			if !strings.Contains(tip, branch) {
				t.Errorf("提示分支缺失：%q", branch)
			}
		}
		// 细节（判据 + 回退）必须在 title 里。
		title := jsFuncBody(t, src, "videoPipelineTitle")
		for _, need := range []string{"判据", "硬件管线失败会自动回退软件解码并写日志", "src_bit_depth", "src_bitrate_kbps"} {
			if !strings.Contains(title, need) {
				t.Errorf("title 里缺 %q", need)
			}
		}
		// 提示必须画在计划区、且在「开始压缩」之前（用户点开始之前就能看到）。
		// 作用域限定在视频压缩弹窗里：文件里另有图片压缩的 draw()，不能拿错。
		modal := jsFuncBody(t, src, "videoCompressModal")
		iTip := strings.Index(modal, "videoPipelineTip(plan)")
		iStart := strings.Index(modal, "text: '开始压缩'")
		if iTip < 0 || iStart < 0 || iTip > iStart {
			t.Errorf("提示没有出现在「开始压缩」之前的计划区：tip=%d start=%d", iTip, iStart)
		}
	})
}
