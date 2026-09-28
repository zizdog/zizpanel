package web

// api_video_presets_gate_test.go —— 「分辨率五档 + 三项默认值 + 硬件=HEVC 且码率不变」的唯一门禁。
//
// 为什么现有门禁抓不到（本轮新增这三类判据的原因）：
//   · TestVideoCompressGate 只断言 planner 的码率封顶 / 不放大 / "产物更大就删掉"，
//     它的档位只用过 360p/480p/720p —— 此前**根本没有 1080p 与「原始」档**，
//     所以"选 1080p 会不会把小画面放大""原始档是不是真的不缩放"没有任何判据；
//   · TestVideoOptionsGate 断言"选项 → ffmpeg 命令行"，但它钉的是旧默认
//     （默认目标码率、硬件 = h264_videotoolbox），**没有**"硬件档必须是 HEVC
//     且码率参数与 CPU 逐字相同"这一条 —— 于是"选硬件就偷偷加码率/换参"能一路绿；
//   · TestVideoProbeCacheGate 只管"改配置有没有重探"，与档位/默认值无关。
// 用户点名的两条硬原则（绝不放大、绝不变大）在**新档位与新默认值**下必须重新成立，
// 所以这条门禁把它们连同"选项 → 命令行"的映射一起钉住，全部是纯函数 + 假 Runner，
// 不跑真 ffmpeg、不读用户真实文件。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/videoopt"
)

// argValue 返回 argv 里某个 flag 紧跟的值（没有就返回空串）。
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestVideoPresetsGate 是这一条门禁。
func TestVideoPresetsGate(t *testing.T) {
	// ① 五档分辨率 + 1080p 建议码率常量 + 下拉暴露。
	t.Run("① 分辨率五档（默认 480p）且 1080p 建议码率进下拉", func(t *testing.T) {
		got := map[string]videoopt.Preset{}
		for _, p := range videoopt.Presets() {
			got[p.ID] = p
		}
		if len(got) != 5 {
			t.Fatalf("分辨率必须正好 5 档，实际 %d：%+v", len(got), got)
		}
		for _, id := range []string{"360p", "480p", "720p", "1080p", videoopt.PresetSource} {
			if _, ok := got[id]; !ok {
				t.Errorf("缺少档位 %q", id)
			}
		}
		// 默认 = 480p（用户拍板：默认必须真的在压）。负向对照：改回 "source" ⇒ 红。
		if videoopt.DefaultPresetID != "480p" {
			t.Fatalf("默认档必须是 480p，实际 %q", videoopt.DefaultPresetID)
		}
		def, ok := videoopt.FindPreset(videoopt.DefaultPresetID)
		if !ok || def.CapHeight != 480 {
			t.Fatalf("默认档必须是把画面封顶到 480，实际 %+v", def)
		}
		// 「原始」档仍必须可用（不缩放，CapHeight=0）。
		src, ok := videoopt.FindPreset(videoopt.PresetSource)
		if !ok || !videoopt.IsSourcePreset(src) || src.CapHeight != 0 {
			t.Fatalf("「原始」档必须存在且不缩放，实际 %+v", src)
		}
		// 建议码率常量（网络视频能用下限，实测经验值）必须存在且被下拉暴露。
		if videoopt.Kbps360p != 400 || videoopt.Kbps480p != 800 ||
			videoopt.Kbps720p != 1500 || videoopt.Kbps1080p != 3000 {
			t.Fatalf("建议码率常量不对：%d/%d/%d/%d",
				videoopt.Kbps360p, videoopt.Kbps480p, videoopt.Kbps720p, videoopt.Kbps1080p)
		}
		p1080, _ := videoopt.FindPreset("1080p")
		found := false
		for _, c := range videoopt.BitrateChoices(p1080) {
			if c.KBps == videoopt.Kbps1080p {
				found = true
			}
		}
		// 负向对照：1080p 的建议值没进下拉（或写成别的数）这里必须红。
		if !found {
			t.Errorf("1080p 下拉里必须暴露 %d kbps，实际 %+v", videoopt.Kbps1080p, videoopt.BitrateChoices(p1080))
		}
		// 原始档默认项是"按源分辨率建议"（0 = 逐文件取档位下限），不是"跟随原片"
		//（跟随原片 = 实际码率被压到原码率×0.95 ⇒ 封顶即跳过，默认就什么都压不了）。
		sc := videoopt.BitrateChoices(src)
		if len(sc) < 2 || sc[0].KBps != 0 || !strings.Contains(sc[0].Hint, "1080p→3000") {
			t.Errorf("原始档码率下拉默认项应为「按源分辨率建议」（0，含档位映射细节），实际 %+v", sc)
		}

		// 接口层：默认响应必须回原始档 + 五档 + 1080p 建议码率（前端不写死数字）。
		srv, _ := newTestServer(t)
		withFakeVideoRunner(t, &fakeVideoRunner{outBytes: 100})
		rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": srv.Cfg.WWWRoot})
		if rec.Code != http.StatusOK {
			t.Fatalf("video-plan 应 200：%d %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data struct {
				Preset  string `json:"preset"`
				Presets []struct {
					ID      string `json:"id"`
					CapH    int    `json:"cap_height"`
					Default int    `json:"default_kbps"`
				} `json:"presets"`
				BitrateChoices []struct {
					KBps int `json:"kbps"`
				} `json:"bitrate_choices"`
				Encoder      string `json:"encoder"`
				Mode         string `json:"mode"`
				Quality      int    `json:"quality"`
				EncoderCodec string `json:"encoder_codec"`
				Note         string `json:"note"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		// 默认档位 = 480p（用户拍板：默认必须真的在压）。
		// 负向对照：改回 "source" ⇒ 这条立刻红。
		if videoopt.DefaultPresetID != "480p" || resp.Data.Preset != "480p" {
			t.Errorf("默认档位必须是 480p（常量=%q 接口=%q）", videoopt.DefaultPresetID, resp.Data.Preset)
		}
		if len(resp.Data.Presets) != 5 {
			t.Errorf("接口必须回 5 档，实际 %d", len(resp.Data.Presets))
		}
		var cap1080 int
		for _, p := range resp.Data.Presets {
			if p.ID == "1080p" {
				cap1080 = p.CapH
			}
		}
		if cap1080 != 1080 {
			t.Errorf("1080p 档的封顶高度必须是 1080，实际 %d", cap1080)
		}
		if resp.Data.Encoder != videoopt.EncoderCPU || resp.Data.Mode != videoopt.ModeBitrate {
			t.Errorf("默认三项必须是 cpu + 目标码率，实际 %s/%s/%d",
				resp.Data.Encoder, resp.Data.Mode, resp.Data.Quality)
		}
		if resp.Data.EncoderCodec != "libx264" {
			t.Errorf("CPU 档的 -c:v 必须是 libx264，实际 %q", resp.Data.EncoderCodec)
		}
		// 默认 480p 的下拉必须给出 800 kbps（档位下限）。
		if len(resp.Data.BitrateChoices) == 0 || resp.Data.BitrateChoices[0].KBps != videoopt.Kbps480p {
			t.Errorf("480p 档码率下拉首项应为 %d，实际 %+v", videoopt.Kbps480p, resp.Data.BitrateChoices)
		}
		// 「原始」档的下拉默认项必须是 0 = 按建议值（不是"跟随原片"=封顶即跳过）。
		recSrc := postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": srv.Cfg.WWWRoot, "preset": "source", "mode": "bitrate"})
		var respSrc struct {
			Data struct {
				BitrateChoices []struct {
					KBps int `json:"kbps"`
				} `json:"bitrate_choices"`
			} `json:"data"`
		}
		_ = json.Unmarshal(recSrc.Body.Bytes(), &respSrc)
		if len(respSrc.Data.BitrateChoices) < 2 || respSrc.Data.BitrateChoices[0].KBps != 0 {
			t.Errorf("原始档码率下拉默认项应为建议值（0），实际 %+v", respSrc.Data.BitrateChoices)
		}
		dir1080 := filepath.Join(srv.Cfg.WWWRoot, "presets1080")
		if err := os.MkdirAll(dir1080, 0o755); err != nil {
			t.Fatal(err)
		}
		recQ := postVideoJSON(t, srv.handleFileVideoPlan,
			map[string]any{"dir": dir1080, "preset": "1080p", "mode": "bitrate"})
		var respQ struct {
			Data struct {
				BitrateChoices []struct {
					KBps int `json:"kbps"`
				} `json:"bitrate_choices"`
			} `json:"data"`
		}
		_ = json.Unmarshal(recQ.Body.Bytes(), &respQ)
		found = false
		for _, c := range respQ.Data.BitrateChoices {
			if c.KBps == videoopt.Kbps1080p {
				found = true
			}
		}
		if !found {
			t.Errorf("接口在 1080p 档下没暴露 %d kbps：%s", videoopt.Kbps1080p, recQ.Body.String())
		}
	})

	// ② 绝不放大：720p 源选 1080p / 原始 ⇒ 目标尺寸 ≤ 源尺寸。
	//    负向对照：把 targetSize 的封顶钳制去掉（直接按档位算 1920x1080）这里必红。
	t.Run("② 新档位也不放大（720p 源选 1080p / 原始）", func(t *testing.T) {
		p1080, _ := videoopt.FindPreset("1080p")
		pSource, _ := videoopt.FindPreset(videoopt.PresetSource)
		cases := []struct {
			name           string
			w, h           int
			preset         videoopt.Preset
			wantW, wantH   int
			naiveW, naiveH int // 去掉"只封顶"这条判据后会算出的尺寸（放大值）
		}{
			{"横屏 720p 选 1080p", 1280, 720, p1080, 1280, 720, 1920, 1080},
			{"竖屏 720x1280 选 1080p", 720, 1280, p1080, 720, 1280, 1080, 1920},
			{"竖屏小画面 480x854 选 1080p", 480, 854, p1080, 480, 854, 540, 960},
			{"小画面 640x360 选 原始", 640, 360, pSource, 640, 360, 640, 360},
			{"1080p 选 原始（不缩不放）", 1920, 1080, pSource, 1920, 1080, 1920, 1080},
			{"奇数尺寸选 原始 ⇒ 取偶数且更小", 641, 361, pSource, 640, 360, 640, 360},
			{"1440p 选 1080p ⇒ 真的降下来", 2560, 1440, p1080, 1920, 1080, 1920, 1080},
		}
		for _, c := range cases {
			info := videoopt.MediaInfo{
				Width: c.w, Height: c.h, DurationSec: 5, FileBytes: 1 << 20,
				VideoKbps: 4000, HasVideo: true,
			}
			p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info,
				videoopt.Options{Preset: c.preset, Mode: videoopt.ModeBitrate}, false)
			if p.SkipReason != "" {
				t.Fatalf("%s：不该跳过（%s）", c.name, p.SkipReason)
			}
			if p.TargetWidth > c.w || p.TargetHeight > c.h {
				t.Errorf("%s：目标 %dx%d 超过源 %dx%d（放大了）",
					c.name, p.TargetWidth, p.TargetHeight, c.w, c.h)
			}
			if p.TargetWidth != c.wantW || p.TargetHeight != c.wantH {
				t.Errorf("%s：目标 %dx%d，期望 %dx%d",
					c.name, p.TargetWidth, p.TargetHeight, c.wantW, c.wantH)
			}
			if p.TargetWidth%2 != 0 || p.TargetHeight%2 != 0 {
				t.Errorf("%s：目标 %dx%d 不是偶数", c.name, p.TargetWidth, p.TargetHeight)
			}
			// 负向对照自检：这里列的 naive 尺寸必须真的**超过**源 ——
			// 若某个用例的 naive 不大于源，它就证明不了"钳制在起作用"。
			if c.naiveW > c.w && c.naiveH > c.h {
				continue // 好用例：naive 会放大
			}
			if c.naiveW != c.wantW || c.naiveH != c.wantH {
				t.Errorf("%s：夹具选的 naive 尺寸 %dx%d 既没放大又和期望不同，改夹具",
					c.name, c.naiveW, c.naiveH)
			}
		}
	})

	// ③ 绝不变大：请求码率 ≥ 原码率 ⇒ 实际码率被封顶到 ≤ 原码率×0.95。
	//    负向对照：去掉 planBitrates 里的 min(选, 原×0.95) 钳制 ⇒ 这里会算出 3000 ⇒ 红。
	t.Run("③ 1080p 的 3000 kbps 在低码率源上被封顶（≤ 原×0.95）", func(t *testing.T) {
		p1080, _ := videoopt.FindPreset("1080p")
		info := videoopt.MediaInfo{
			Width: 1920, Height: 1080, DurationSec: 10, FileBytes: 4 << 20,
			VideoKbps: 2000, HasVideo: true,
		}
		// 用户选的确实是 3000（1050p 下拉下限）：证明"3000 进得来"。
		if got := videoopt.ResolveKBps(p1080, 0); got != videoopt.Kbps1080p {
			t.Fatalf("1080p 档默认码率应为 %d，实际 %d", videoopt.Kbps1080p, got)
		}
		p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info,
			videoopt.Options{Preset: p1080, Mode: videoopt.ModeBitrate}, false)
		if p.SkipReason == "" || !p.Capped {
			t.Fatalf("3000 ≥ 原 2000×0.95，必须封顶并如实标跳过：capped=%v reason=%q", p.Capped, p.SkipReason)
		}
		if cap := int(2000 * 0.95); p.VideoKbps > cap {
			t.Errorf("封顶后码率 %d 超过原码率×0.95=%d", p.VideoKbps, cap)
		}
		if p.VideoKbps == videoopt.Kbps1080p {
			t.Errorf("码率没有被封顶（还是 %d）", p.VideoKbps)
		}
		// 封顶的行必须能原样放进 output（不然用户会以为文件丢了）。
		if !p.PlaceInOutput {
			t.Error("封顶即跳过时必须原样放进 output")
		}
		// 同一档位对高码率源：不封顶，且实际码率就是选的 3000。
		info.VideoKbps = 8000
		q := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info,
			videoopt.Options{Preset: p1080, Mode: videoopt.ModeBitrate}, false)
		if q.SkipReason != "" || q.VideoKbps != videoopt.Kbps1080p {
			t.Errorf("8000 kbps 的源选 3000 不该封顶：reason=%q kbps=%d", q.SkipReason, q.VideoKbps)
		}
		if q.VideoKbps > int(8000*0.95) {
			t.Errorf("实际码率 %d 超过原码率×0.95", q.VideoKbps)
		}
	})

	// ④ 硬件档 = HEVC，且码率参数与 CPU **逐字节相同**（用户点名"码率不变！"）。
	t.Run("④ 硬件=hevc_videotoolbox 且码率参数与 CPU 完全一致", func(t *testing.T) {
		base := videoopt.TranscodeRequest{
			Src: "in.mp4", Dst: "out.mp4", Width: 1920, Height: 1080,
			VideoKbps: 3000, AudioKbps: 96, DurationSec: 5,
		}
		cpu := videoopt.TranscodeArgs(withOpts(base, videoopt.EncoderCPU, videoopt.ModeBitrate, 0, false), 0)
		hw := videoopt.TranscodeArgs(withOpts(base, videoopt.EncoderHardware, videoopt.ModeBitrate, 0, false), 0)

		if !argsHave(hw, "-c:v", "hevc_videotoolbox") {
			t.Errorf("硬件加速必须用 HEVC（hevc_videotoolbox），argv=%v", hw)
		}
		if argsHave(hw, "-c:v", "libx264") || argsHave(hw, "h264_videotoolbox", "") {
			t.Errorf("硬件档不该再出现 x264/H.264，argv=%v", hw)
		}
		// 逐字比较码率相关参数：换编码器**一个数字都不许动**。
		for _, flag := range []string{"-b:v", "-maxrate", "-bufsize"} {
			cv, hv := argValue(cpu, flag), argValue(hw, flag)
			if cv == "" || hv == "" || cv != hv {
				t.Errorf("硬件档的 %s 必须与 CPU 完全相同：cpu=%q hw=%q\ncpu=%v\nhw=%v",
					flag, cv, hv, cpu, hw)
			}
		}
		// 负向对照自检：如果实现里偷偷给硬件乘了系数（1.25×3000=3750），
		// 上面那条"逐字相同"必须立刻不成立 —— 这里显式验证比较器抓得住。
		mutated := append([]string{}, hw...)
		for i, a := range mutated {
			if a == "-b:v" && i+1 < len(mutated) {
				mutated[i+1] = "3750k"
			}
		}
		if argValue(mutated, "-b:v") == argValue(cpu, "-b:v") {
			t.Fatal("这条断言抓不到'硬件档偷偷改码率'：比较器失效")
		}

		// 模式 → 参数：CPU+CRF ⇒ -crf 且无 -b:v；目标码率 ⇒ -b:v。
		crf := videoopt.TranscodeArgs(withOpts(base, videoopt.EncoderCPU, videoopt.ModeQuality, 0, false), 0)
		if !argsHave(crf, "-crf", "26") {
			t.Errorf("CPU+质量优先必须带 -crf 26（默认），argv=%v", crf)
		}
		if argsHave(crf, "-b:v", "") {
			t.Errorf("CRF 模式不许出现 -b:v，argv=%v", crf)
		}
		if !argsHave(cpu, "-b:v", "3000k") {
			t.Errorf("目标码率模式必须带 -b:v 3000k，argv=%v", cpu)
		}
		if argsHave(cpu, "-crf", "") {
			t.Errorf("目标码率模式不许出现 -crf，argv=%v", cpu)
		}
		// 硬件 + 质量优先：刻度不同（-q:v，越大越好），默认档位由常量给出。
		hwq := videoopt.TranscodeArgs(withOpts(base, videoopt.EncoderHardware, videoopt.ModeQuality, 0, false), 0)
		if !argsHave(hwq, "-q:v", "45") || argsHave(hwq, "-crf", "") {
			t.Errorf("硬件+质量优先必须带 -q:v 45（实测标定），argv=%v", hwq)
		}
	})

	// ⑤ 原始档的语义：不缩放（目标 = 源），但码率/编码仍按用户选项走。
	t.Run("⑤ 原始档不缩放，只降码率或换编码", func(t *testing.T) {
		pSource, _ := videoopt.FindPreset(videoopt.PresetSource)
		info := videoopt.MediaInfo{
			Width: 1080, Height: 1920, DurationSec: 10, FileBytes: 8 << 20,
			VideoKbps: 4000, HasVideo: true,
		}
		p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info,
			videoopt.Options{Preset: pSource, Encoder: videoopt.EncoderHardware, Mode: videoopt.ModeBitrate}, false)
		if p.TargetWidth != 1080 || p.TargetHeight != 1920 {
			t.Errorf("原始档必须不缩放：目标 %dx%d，源 1080x1920", p.TargetWidth, p.TargetHeight)
		}
		if p.EncoderCodec != "hevc_videotoolbox" {
			t.Errorf("原始档 + 硬件必须记 hevc_videotoolbox，实际 %q", p.EncoderCodec)
		}
		// 建议值：视频码率 ≤ 原码率×0.95（"绝不变大"在原始档也成立）。
		if p.VideoKbps <= 0 || float64(p.VideoKbps) > 4000*0.95+0.5 {
			t.Errorf("原始档的码率 %d 超过原码率×0.95", p.VideoKbps)
		}
	})

	// ⑥「原始」档的建议码率 = min(该源分辨率的档位建议, 源码率×0.7)（用户拍板 A 方案），
	//    且这种建议值**不会**被判"封顶即跳过"（真的会转码）。
	//    负向对照：只取档位建议（忽略 ×0.7）⇒ 三个用例的数值全错 + 1500k 的 720p 会被封顶 ⇒ 红。
	t.Run("⑥ 原始档建议值 = min(档位建议, 源×0.7) 且不被封顶", func(t *testing.T) {
		if videoopt.DefaultMode != videoopt.ModeBitrate {
			t.Fatalf("默认模式必须是目标码率，实际 %q（负向对照：默认 CRF 时这条会红）", videoopt.DefaultMode)
		}
		// 档位映射本身（源码率给到足够高，让"×0.7"那一档不生效）。
		for _, c := range []struct {
			w, h, want int
			name       string
		}{
			{1920, 1080, 3000, "横屏 1080p"},
			{1280, 720, 1500, "横屏 720p"},
			{854, 480, 800, "横屏 480p"},
			{640, 360, 400, "横屏 360p"},
			{720, 1280, 1500, "竖屏 720x1280（按宽）"},
			{1080, 1920, 3000, "竖屏 1080x1920（按宽）"},
			{320, 180, 400, "比 360p 还小 ⇒ 最低档"},
			{2560, 1440, 3000, "4K ⇒ 最高档"},
			{1280, 576, 800, "576 落在 480/720 之间 ⇒ 取更接近的 480 档"},
			{1280, 640, 1500, "640 落在 480/720 之间 ⇒ 取更接近的 720 档"},
		} {
			if got := videoopt.SuggestedKBps(c.w, c.h, 100000); got != c.want {
				t.Errorf("%s：建议 %d kbps，期望 %d", c.name, got, c.want)
			}
		}
		// 用户拍板的三个数：min(档位建议, 源码率×0.7)。
		for _, c := range []struct {
			w, h, srcKbps, want int
			name                string
		}{
			{1280, 720, 1500, 1050, "1500k 的 720p ⇒ min(1500, 1050)"},
			{1920, 1080, 3000, 2100, "3000k 的 1080p ⇒ min(3000, 2100)"},
			{854, 480, 250, 175, "250k 的 480p ⇒ min(800, 175)"},
			// 源码率很高时仍然是档位建议。
			{1280, 720, 12000, 1500, "12000k 的 720p ⇒ min(1500, 8400)=1500"},
		} {
			if got := videoopt.SuggestedKBps(c.w, c.h, c.srcKbps); got != c.want {
				t.Errorf("%s：建议 %d，期望 %d", c.name, got, c.want)
			}
		}

		pSource, _ := videoopt.FindPreset(videoopt.PresetSource)
		// 端到端：这三档建议值都必须**不被封顶、真的能压**（负向对照：只取档位建议 ⇒ 1500 那档被跳过）。
		for _, c := range []struct {
			w, h, srcKbps, want int
		}{
			{1280, 720, 1500, 1050},
			{1920, 1080, 3000, 2100},
			{854, 480, 250, 175},
		} {
			info := videoopt.MediaInfo{
				Width: c.w, Height: c.h, DurationSec: 10, FileBytes: 4 << 20,
				VideoKbps: c.srcKbps, HasVideo: true,
			}
			p := videoopt.PlanOne("v.mp4", "/tmp/v.mp4", "/tmp/out", info, videoopt.Options{
				Preset: pSource, Mode: videoopt.ModeBitrate, // 空 KBps = 默认建议值
			}, false)
			if !p.Runnable() {
				t.Fatalf("%dx%d/源 %d kbps：建议值下必须能压（真的转码），实际跳过=%q（建议 %d）",
					c.w, c.h, c.srcKbps, p.SkipReason, p.SuggestedKbps)
			}
			if p.Capped {
				t.Errorf("%dx%d/源 %d kbps：建议值不该触发封顶（建议 %d / 实际 %d）",
					c.w, c.h, c.srcKbps, p.SuggestedKbps, p.VideoKbps)
			}
			if p.SuggestedKbps != c.want || p.VideoKbps != c.want {
				t.Errorf("%dx%d/源 %d kbps：建议 %d / 实际 %d，期望 %d",
					c.w, c.h, c.srcKbps, p.SuggestedKbps, p.VideoKbps, c.want)
			}
			// 计划行必须写清依据（面板 title/说明要能看出是"原片 70%"还是"源分辨率"）。
			if p.SuggestedFrom != "rate70" {
				t.Errorf("%dx%d：建议依据应为 rate70，实际 %q（note=%q）", c.w, c.h, p.SuggestedFrom, p.Note)
			}
			if !strings.Contains(p.Note, "70%") || !strings.Contains(p.Note, "建议") {
				t.Errorf("%dx%d：说明必须写清「按原片 70%%」的依据，实际 %q", c.w, c.h, p.Note)
			}
		}

		// 用户**手动**把码率拉到 ≥ 原片×0.95 时，封顶即跳过的规则照旧不变。
		low := videoopt.PlanOne("low.mp4", "/tmp/low.mp4", "/tmp/out", videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 10, FileBytes: 1 << 20,
			VideoKbps: 1000, HasVideo: true,
		}, videoopt.Options{Preset: pSource, KBps: 2000, Mode: videoopt.ModeBitrate}, false)
		if !low.Capped || low.VideoKbps > 950 || !low.PlaceInOutput {
			t.Errorf("手动码率 ≥ 原片×0.95 仍要封顶即跳过：capped=%v kbps=%d place=%v",
				low.Capped, low.VideoKbps, low.PlaceInOutput)
		}
	})

	// ⑧ 本次转码用时：结果里带总耗时与单文件耗时，进度窗最终文案也带上。
	//    负向对照：不填耗时（DurationText 空 / DurationSec=0）⇒ 这条红。
	t.Run("⑧ 本次转码用时写进结果与进度", func(t *testing.T) {
		// 文案格式（用户给的例子：1 分 23 秒）。
		if got := videoopt.SummaryDurationText(83*time.Second, 3); got != "本次转码用时 1 分 23 秒（3 个文件）" {
			t.Errorf("耗时文案不对：%q", got)
		}
		if got := videoopt.FormatDuration(83 * time.Second); got != "1 分 23 秒" {
			t.Errorf("FormatDuration(83s)=%q", got)
		}
		if got := videoopt.FormatDuration(30 * time.Millisecond); got != "<1 秒" {
			t.Errorf("亚秒不该谎报 0 秒：%q", got)
		}

		srv, _ := newTestServer(t)
		dir := filepath.Join(srv.Cfg.WWWRoot, "timing")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
			VideoKbps: 4000, HasVideo: true,
		}
		fake := &fakeVideoRunner{outBytes: 100, transcodeDelay: 30 * time.Millisecond, infos: map[string]videoopt.MediaInfo{}}
		for _, n := range []string{"a.mp4", "b.mp4"} {
			p := filepath.Join(dir, n)
			if err := os.WriteFile(p, bytes.Repeat([]byte{5}, 4000), 0o644); err != nil {
				t.Fatal(err)
			}
			fake.infos[p] = info
		}
		withFakeVideoRunner(t, fake)

		rec := postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{"dir": dir, "preset": "480p", "kbps": 800})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("必须 202：%d %s", rec.Code, rec.Body.String())
		}
		var accepted struct {
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.Data.TaskID == "" {
			t.Fatalf("没有拿到 task_id：%s", rec.Body.String())
		}
		task := srv.Tasks.Get(accepted.Data.TaskID)
		select {
		case <-task.Done():
		case <-time.After(15 * time.Second):
			t.Fatal("任务没结束")
		}
		res, ok := task.Meta().Result.(*videoopt.RunResult)
		if !ok || res == nil {
			t.Fatalf("任务结果类型不对：%#v", task.Meta().Result)
		}
		if res.DurationSec <= 0 || res.DurationText == "" {
			t.Errorf("结果必须带本次总耗时：sec=%v text=%q", res.DurationSec, res.DurationText)
		}
		if !strings.Contains(res.DurationText, "本次转码用时") || !strings.Contains(res.DurationText, "2 个文件") {
			t.Errorf("总耗时文案不对：%q", res.DurationText)
		}
		if len(res.Items) != 2 {
			t.Fatalf("应处理 2 个文件，实际 %d", len(res.Items))
		}
		for _, it := range res.Items {
			if it.DurationSec <= 0 || it.DurationText == "" {
				t.Errorf("%s 必须带单文件耗时：%v/%q", it.Name, it.DurationSec, it.DurationText)
			}
		}
		// 进度窗最终文案必须能看到耗时（用户点名）。
		if p := task.Progress(); p == nil || !strings.Contains(p.Message, "本次转码用时") {
			t.Errorf("进度窗最终文案必须带耗时，实际 %+v", task.Progress())
		}
	})

	// ⑦ 只处理选中的视频（names）：带 names ⇒ 只计划/只处理这些；不带 ⇒ 全部；
	//    不存在/非视频的名字逐条如实标记；路径穿越与"全不合法"⇒ 400。
	//    负向对照：实现忽略 names ⇒ 第一条断言（2 行）与执行断言（只压 1 个）立刻红。
	t.Run("⑦ 只处理选中的视频（names）", func(t *testing.T) {
		srv, _ := newTestServer(t)
		fake := &fakeVideoRunner{outBytes: 100, infos: map[string]videoopt.MediaInfo{}}
		withFakeVideoRunner(t, fake)

		dir := filepath.Join(srv.Cfg.WWWRoot, "selected")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		info := videoopt.MediaInfo{
			Width: 1280, Height: 720, DurationSec: 3, FileBytes: 4000,
			VideoKbps: 4000, HasVideo: true, Codec: "h264",
		}
		for _, n := range []string{"a.mp4", "b.mp4", "c.mp4"} {
			p := filepath.Join(dir, n)
			if err := os.WriteFile(p, bytes.Repeat([]byte{4}, 4000), 0o644); err != nil {
				t.Fatal(err)
			}
			fake.infos[p] = info
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a video"), 0o644); err != nil {
			t.Fatal(err)
		}

		type namesPlan struct {
			Data struct {
				Names    []string `json:"names"`
				Runnable int      `json:"runnable"`
				Skipped  int      `json:"skipped"`
				Rows     []struct {
					Name       string `json:"name"`
					SkipReason string `json:"skip_reason"`
				} `json:"rows"`
			} `json:"data"`
		}
		planNames := func(body map[string]any) (*httptest.ResponseRecorder, namesPlan) {
			t.Helper()
			rec := postVideoJSON(t, srv.handleFileVideoPlan, body)
			var out namesPlan
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec, out
		}

		// 带 names：只有这两行（负向对照：忽略 names ⇒ 3 行 ⇒ 红）。
		rec, two := planNames(map[string]any{"dir": dir, "names": []string{"a.mp4", "b.mp4"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("带 names 的计划应 200：%d %s", rec.Code, rec.Body.String())
		}
		if len(two.Data.Rows) != 2 || two.Data.Runnable != 2 {
			t.Fatalf("只选 2 个就必须只有 2 行，实际 rows=%d runnable=%d：%s",
				len(two.Data.Rows), two.Data.Runnable, rec.Body.String())
		}
		got := map[string]bool{}
		for _, r := range two.Data.Rows {
			got[r.Name] = true
			if r.SkipReason != "" {
				t.Errorf("%s 不该被跳过：%s", r.Name, r.SkipReason)
			}
		}
		if !got["a.mp4"] || !got["b.mp4"] || got["c.mp4"] {
			t.Errorf("计划行必须是选中的 a/b，实际 %v", got)
		}
		// 不带 names ⇒ 这个目录里的全部视频（3 个，notes.txt 不算）。
		rec, all := planNames(map[string]any{"dir": dir})
		if rec.Code != http.StatusOK || len(all.Data.Rows) != 3 {
			t.Fatalf("不带 names 必须处理全部 3 个视频，实际 %d：%s", len(all.Data.Rows), rec.Body.String())
		}

		// 混入"不存在"与"不是视频"：逐条如实标记，其余照压（绝不静默忽略）。
		rec, mixed := planNames(map[string]any{
			"dir": dir, "names": []string{"a.mp4", "nope.mp4", "notes.txt"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("部分合法应 200（只是把不合法的如实标出来）：%d %s", rec.Code, rec.Body.String())
		}
		reasons := map[string]string{}
		for _, r := range mixed.Data.Rows {
			reasons[r.Name] = r.SkipReason
		}
		if len(mixed.Data.Rows) != 3 {
			t.Fatalf("3 个名字必须都有对应的一行，实际 %d", len(mixed.Data.Rows))
		}
		if reasons["a.mp4"] != "" {
			t.Errorf("a.mp4 是视频，不该被跳过：%q", reasons["a.mp4"])
		}
		if !strings.Contains(reasons["nope.mp4"], "不存在") {
			t.Errorf("不存在的名字必须如实说「不存在」，实际 %q", reasons["nope.mp4"])
		}
		if !strings.Contains(reasons["notes.txt"], "不是视频") {
			t.Errorf("非视频必须如实说「不是视频」，实际 %q", reasons["notes.txt"])
		}
		if mixed.Data.Skipped != 2 {
			t.Errorf("被拒的 2 条要计入 skipped，实际 %d", mixed.Data.Skipped)
		}

		// 路径安全：带路径/`..` 一律 400（names 只能是本目录下的文件名）。
		for _, bad := range []string{"../a.mp4", "sub/a.mp4", "..", "/tmp/a.mp4"} {
			rec := postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "names": []string{bad}})
			// 必须是**文件名不合法**那条 400（负向对照：去掉路径校验后这里会变成别的错甚至 200）。
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不合法") {
				t.Errorf("非法文件名 %q 必须 400 且说「不合法」，实际 %d：%s", bad, rec.Code, rec.Body.String())
			}
		}
		// 全不合法 ⇒ 400 且说清原因（不是"计划为空、无事可做"）。
		rec = postVideoJSON(t, srv.handleFileVideoPlan, map[string]any{"dir": dir, "names": []string{"notes.txt"}})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不是视频") {
			t.Errorf("全不合法必须 400 并说清原因，实际 %d：%s", rec.Code, rec.Body.String())
		}
		// 压缩接口同样：全不合法不许建一个注定失败的任务。
		rec = postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{"dir": dir, "names": []string{"notes.txt"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("全不合法的压缩请求必须 400，实际 %d：%s", rec.Code, rec.Body.String())
		}

		// 执行：带 names 只压那一个（负向对照：忽略 names ⇒ 3 次转码 ⇒ 红）。
		rec = postVideoJSON(t, srv.handleFileVideoCompress, map[string]any{
			"dir": dir, "preset": "480p", "names": []string{"b.mp4"},
		})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("带 names 的压缩必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var accepted struct {
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.Data.TaskID == "" {
			t.Fatalf("没有拿到 task_id：%s", rec.Body.String())
		}
		task := srv.Tasks.Get(accepted.Data.TaskID)
		if task == nil {
			t.Fatal("任务不存在")
		}
		select {
		case <-task.Done():
		case <-time.After(15 * time.Second):
			t.Fatal("任务没结束")
		}
		if got := task.Status(); got != "succeeded" {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		if len(fake.reqs) != 1 || filepath.Base(fake.reqs[0].Src) != "b.mp4" {
			t.Fatalf("只选 b.mp4 就必须只压它一次，实际 %d 次：%+v", len(fake.reqs), fake.reqs)
		}
		res, ok := task.Meta().Result.(*videoopt.RunResult)
		if !ok || res == nil || res.Done != 1 {
			t.Fatalf("任务结果应只处理 1 个：%#v", task.Meta().Result)
		}

		// 前端那份"选中的是不是视频"的扩展名口径必须与后端一致（同一口径，别各写一份）。
		js, rerr := os.ReadFile(filepath.Join("assets", "js", "files.js"))
		if rerr != nil {
			t.Fatalf("读不到前端源码：%v", rerr)
		}
		src := string(js)
		if !strings.Contains(src, "VIDEO_CANDIDATE_EXT") {
			t.Fatal("前端必须有一个 VIDEO_CANDIDATE_EXT（按后端同一口径筛选中的视频）")
		}
		line := ""
		for _, l := range strings.Split(src, "\n") {
			if strings.Contains(l, "VIDEO_CANDIDATE_EXT =") {
				line = l
			}
		}
		if line == "" {
			t.Fatal("没找到 VIDEO_CANDIDATE_EXT 的定义行")
		}
		for _, ext := range videoopt.VideoExts() {
			if !strings.Contains(line, ext[1:]) { // 去掉前导点，正则里没写点
				t.Errorf("前端候选项少了对齐后端的扩展名 %s（负向对照：两边走样时这里红）\n%s", ext, line)
			}
		}
	})
}
