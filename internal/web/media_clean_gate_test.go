package web

// media_clean_gate_test.go —— 「🧹 去广告（无损·彻底清理）」的唯一门禁。
//
// 为什么现有门禁抓不到：去广告是**新功能**。视频压缩那条链路（videoopt + 它的门禁）
// 管的是码率规划与重编码，从来不会去清容器标签、附件或多余视频流；文件操作那条
// （files 的复制/移动/删除门禁）只管搬字节，没有任何"无损替换 + 可选 .bak"的判据。
// 所以"重编码了、把源删了、时长对不上也替换、失败还留下 .bak、只清三个标签"都能一路绿。
//
// 一条门禁覆盖整类问题（PATH 垫片假 ffmpeg/ffprobe，绝不真跑真机 ffmpeg、
// 不碰真实媒体文件、不 sudo、不写 /opt/zizpanel）：
//   ① 彻底清理 ⇒ argv 含 -map_metadata -1 + 三条 -metadata:s:* title= + 每个
//      非字体附件/多余视频流/attached_pic 的 -map -0:<idx>，字体附件 idx 绝不出现，
//      且轨道语言与字体附件的 filename/mimetype 被**显式还原**（-map_metadata -1
//      会把它们一起抹掉，matroska 缺 filename 直接 mux 失败），无编码器参数；
//      默认（不勾备份）成功 ⇒ 磁盘上没有任何 .bak，源被新产物替换；
//      结果 Note 逐项计数正确；
//   ② 只有字体附件 + 无任何标签 + 单视频流 ⇒ skipped 且假 ffmpeg 调用 0 次（负向对照）；
//   ③ ffmpeg 失败 ⇒ 源在、记 failed、原因原样显示、无 .bak、无临时文件；
//   ④ 时长核对不过 ⇒ 源在、原因如实报、无 .bak、无临时文件；
//   ⑤ 路径越界 ⇒ 403 且不建任务（plan 与 clean 两个接口）；
//   ⑥ ctx 取消 ⇒ 源在、临时文件清理干净、记 canceled；
//   ⑦ 勾选保留备份 ⇒ .bak 在、内容 == 原文件、结果里 backup 名与 keep_backup 正确；
//   ⑧ 勾选保留备份 + 已有 .bak ⇒ 绝不覆盖，本次落 .bak-2；
//   ⑨ 勾选保留备份 + 替换失败 ⇒ 从 .bak 还原源、不留 .bak、不留临时文件。
// 变异验证见文件末尾注释。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/mediaclean"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// mcFfprobeShim 是假的 ffprobe：记录 argv，按环境变量回答两类查询。
//
//	· 时长查询（format=duration）按"问的是不是临时产物"分别回答；
//	· 其余（inspect 的合并查询）原样输出 MC_INSPECT（JSON）。
const mcFfprobeShim = `#!/bin/sh
LOG="${MC_ARGV_LOG:?}"
{
  printf 'ffprobe'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
} >> "$LOG"
last=""
for a in "$@"; do last="$a"; done
case "$*" in
  *format=duration*)
    case "$last" in
      *zp-clean-*) printf '%s\n' "$MC_OUT_DURATION" ;;
      *) printf '%s\n' "$MC_IN_DURATION" ;;
    esac
    exit 0
    ;;
esac
printf '%s' "$MC_INSPECT"
exit 0
`

// mcFfmpegShim 是假的 ffmpeg：记录 argv，按环境变量成功/失败/写产物/挂住。
const mcFfmpegShim = `#!/bin/sh
LOG="${MC_ARGV_LOG:?}"
{
  printf 'ffmpeg'
  for a in "$@"; do printf ' %s' "$a"; done
  printf '\n'
} >> "$LOG"
if [ "$MC_FFMPEG_EXIT" != "0" ]; then
  printf '%s\n' "$MC_FFMPEG_ERR" >&2
  exit "$MC_FFMPEG_EXIT"
fi
if [ "$MC_FFMPEG_NO_OUT" = "1" ]; then
  exit 0
fi
last=""
for a in "$@"; do last="$a"; done
printf 'zp-shim-cleaned-output' > "$last" || exit 9
if [ "$MC_FFMPEG_SLEEP" != "0" ]; then
  exec /bin/sleep "$MC_FFMPEG_SLEEP"
fi
exit 0
`

// mcInspect 拼一段 inspect 的 ffprobe JSON：只给用得到的字段（判据全在 Go 侧）。
func mcInspect(streams string, formatTags string) string {
	if streams == "" {
		streams = `{"index":0,"codec_type":"video","disposition":{"attached_pic":0}}`
	}
	if formatTags == "" {
		formatTags = "{}"
	}
	return `{"streams":[` + streams + `],"format":{"tags":` + formatTags + `}}`
}

// 单条流的 JSON 片段构造器（门禁可读性优先）。
func mcStream(index int, codecType string, attachedPic bool, tags string) string {
	ap := 0
	if attachedPic {
		ap = 1
	}
	if tags == "" {
		tags = "{}"
	}
	return `{"index":` + strconv.Itoa(index) + `,"codec_type":"` + codecType +
		`","disposition":{"attached_pic":` + strconv.Itoa(ap) + `},"tags":` + tags + `}`
}

// mcShim 是一次门禁用的假引擎目录（PATH 只指向它，保证摸不到真 ffmpeg）。
type mcShim struct {
	dir string
	log string
}

func newMCShim(t *testing.T) *mcShim {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(mcFfprobeShim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(mcFfmpegShim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MC_ARGV_LOG", logPath)
	t.Setenv("MC_INSPECT", mcInspect("", ""))
	t.Setenv("MC_IN_DURATION", "10")
	t.Setenv("MC_OUT_DURATION", "10")
	t.Setenv("MC_FFMPEG_EXIT", "0")
	t.Setenv("MC_FFMPEG_ERR", "")
	t.Setenv("MC_FFMPEG_NO_OUT", "0")
	t.Setenv("MC_FFMPEG_SLEEP", "0")
	// PATH **只**含垫片目录：services.LocateCommand 的 LookPath 必然命中垫片，
	// 绝不会回退到 /opt/homebrew/bin 的真 ffmpeg。
	t.Setenv("PATH", dir)
	return &mcShim{dir: dir, log: logPath}
}

// calls 返回某个假命令收到的全部 argv（每次一行，已去掉程序名前缀）。
func (s *mcShim) calls(t *testing.T, prog string) []string {
	t.Helper()
	raw, err := os.ReadFile(s.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prog+" ") {
			out = append(out, strings.TrimPrefix(line, prog+" "))
		}
	}
	return out
}

// postMediaClean 直接调 handler（绕过鉴权，与既有 API 单测同一做法）。
func postMediaClean(t *testing.T, h http.HandlerFunc, paths []string) *httptest.ResponseRecorder {
	return postMediaCleanKeep(t, h, paths, false)
}

// postMediaCleanKeep 同上，但带上 keep_backup（勾选"保留原文件为 .bak"）。
func postMediaCleanKeep(t *testing.T, h http.HandlerFunc, paths []string, keepBackup bool) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"paths": paths, "keep_backup": keepBackup})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/media-clean", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// taskIDOf 从 202 响应里取出 task_id 并等任务结束。
func taskIDOf(t *testing.T, srv *Server, rec *httptest.ResponseRecorder) *tasks.Task {
	t.Helper()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("必须 202 + task_id，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.TaskID == "" {
		t.Fatalf("没有拿到 task_id：%s", rec.Body.String())
	}
	return waitTaskDone(t, srv, body.Data.TaskID)
}

// mcResult 取出任务结果（BatchResult）。
func mcResult(t *testing.T, task *tasks.Task) *files.BatchResult {
	t.Helper()
	res, _ := task.Meta().Result.(*files.BatchResult)
	if res == nil {
		t.Fatalf("任务结果类型不对：%#v", task.Meta().Result)
	}
	return res
}

// mcMediaFile 在测试沙箱里造一个源文件（返回路径与原始内容）。
func mcMediaFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// mcTemps 返回目录里残留的 .zp-clean-* 临时文件。
func mcTemps(t *testing.T, dir string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(dir, ".zp-clean-*"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// mcBaks 返回目录里全部 .bak / .bak-N。
func mcBaks(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, pat := range []string{"*.bak", "*.bak-*"} {
		got, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
	}
	return out
}

// mcNoBak 断言目录里没有任何 .bak / .bak-N（默认不备份 / 失败 / 取消时一个都不许多出来）。
func mcNoBak(t *testing.T, dir string) {
	t.Helper()
	if got := mcBaks(t, dir); len(got) > 0 {
		t.Errorf("这里绝不该有备份，实际：%v", got)
	}
}

// TestMediaCleanGate 是这一条门禁。
func TestMediaCleanGate(t *testing.T) {
	original := []byte("original-media-bytes-0123456789")

	t.Run("① 彻底清理 ⇒ argv 正确 + 默认不留 .bak + Note 计数", func(t *testing.T) {
		srv, _ := newTestServer(t)
		shim := newMCShim(t)
		// 流布局：0 正片视频（带 title）、1 音频（title + language）、2 attached_pic 封面、
		//          3 多余视频流、4 非字体附件（封面图）、5 字体附件、6 字幕。
		streams := strings.Join([]string{
			mcStream(0, "video", false, `{"title":"正片标题"}`),
			mcStream(1, "audio", false, `{"title":"音轨标题","language":"eng"}`),
			mcStream(2, "video", true, ""),
			mcStream(3, "video", false, ""),
			mcStream(4, "attachment", false, `{"filename":"cover.jpg","mimetype":"image/jpeg"}`),
			mcStream(5, "attachment", false, `{"filename":"font.ttf","mimetype":"application/x-truetype-font"}`),
			mcStream(6, "subtitle", false, ""),
		}, ",")
		t.Setenv("MC_INSPECT", mcInspect(streams, `{"title":"扫码进群","comment":"推广"}`))
		dir := filepath.Join(srv.Cfg.WWWRoot, "g1")
		src := mcMediaFile(t, dir, "ep01.mkv", original)

		task := taskIDOf(t, srv, postMediaClean(t, srv.handleFileMediaClean, []string{src}))
		if got := task.Status(); got != tasks.StatusSucceeded {
			t.Fatalf("任务状态 %s，错误=%v", got, task.Meta().Error)
		}
		res := mcResult(t, task)
		if res.Done != 1 || res.Skipped != 0 || res.Failed != 0 {
			t.Fatalf("应 1 成功/0 跳过/0 失败，实际 done=%d skipped=%d failed=%d（msg=%s）",
				res.Done, res.Skipped, res.Failed, res.Msg)
		}
		if res.KeepBackup {
			t.Error("默认请求的 keep_backup 必须是 false")
		}

		calls := shim.calls(t, "ffmpeg")
		if len(calls) != 1 {
			t.Fatalf("应恰好 1 次 ffmpeg，实际 %d：%v", len(calls), calls)
		}
		line := calls[0]
		padded := " " + line + " "
		for _, want := range []string{
			" -map 0 ", " -c copy ", " -map_chapters 0 ",
			" -map_metadata -1 ",
			" -metadata:s:v title= ", " -metadata:s:a title= ", " -metadata:s:s title= ",
			" -map -0:2 ", " -map -0:3 ", " -map -0:4 ",
			// -map_metadata -1 会连轨道级语言与附件 filename 一起丢，必须逐项还原
			// （否则 matroska 附件直接 mux 失败，真机踩过）。
			" -metadata:s:a:0 language=eng ",
			" -metadata:s:t:0 filename=font.ttf ",
			" -metadata:s:t:0 mimetype=application/x-truetype-font ",
		} {
			if !strings.Contains(padded, want) {
				t.Errorf("argv 缺 %q：%s", want, line)
			}
		}
		// 被删掉的那个非字体附件（输出 t:1）绝不能被写回元数据。
		if strings.Contains(line, "-metadata:s:t:1") {
			t.Errorf("被删掉的附件不该有元数据还原：%s", line)
		}
		// 字体附件（idx 5）与字幕/音轨绝不能被剔掉。
		for _, bad := range []string{"-0:5", "-0:6", "-0:1", "-0:0"} {
			if strings.Contains(line, bad) {
				t.Errorf("字体附件/保留流绝不能被负映射，但 argv 含 %q：%s", bad, line)
			}
		}
		// 恰好 3 条负映射（封面 2 + 多余视频 3 + 非字体附件 4）。
		if n := strings.Count(line, "-map -0:"); n != 3 {
			t.Errorf("应恰 3 条负映射，实际 %d：%s", n, line)
		}
		// -c copy 的反面：出现任何编码器参数都是"重编码了"。
		for _, bad := range []string{"-c:v", "-c:a", "libx264", "libx265", "hevc_videotoolbox",
			"-preset", "-crf", "-b:v", "-q:v", "-c:a aac"} {
			if strings.Contains(line, bad) {
				t.Errorf("绝不重编码，但 argv 含 %q：%s", bad, line)
			}
		}

		// 默认不勾选：磁盘上不许出现任何 .bak；原路径就是新产物。
		mcNoBak(t, dir)
		now, err := os.ReadFile(src)
		if err != nil || string(now) != "zp-shim-cleaned-output" {
			t.Fatalf("原路径应是清理后的新文件：err=%v content=%q", err, now)
		}
		if want := int64(len(original)) - int64(len("zp-shim-cleaned-output")); res.Items[0].SavedBytes != want {
			t.Errorf("省下的字节应为 %d，实际 %d", want, res.Items[0].SavedBytes)
		}
		if res.Items[0].Backup != "" {
			t.Errorf("不保留备份时结果不许带备份名，实际 %q", res.Items[0].Backup)
		}
		// Note 逐项计数（用户要求每项都报）。
		note := res.Items[0].Note
		for _, want := range []string{"去内嵌封面 1 张", "去掉多余视频流 1 条", "去掉附件 1 个",
			"清空轨道标题 2 条", "清空全局标签（comment, title）"} {
			if !strings.Contains(note, want) {
				t.Errorf("Note 缺 %q，实际 %q", want, note)
			}
		}
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("成功后不该留临时文件：%v", got)
		}
	})

	t.Run("② 只有字体附件 + 无标签 + 单视频流 ⇒ skipped 且 0 次 ffmpeg（负向对照）", func(t *testing.T) {
		srv, _ := newTestServer(t)
		shim := newMCShim(t)
		streams := strings.Join([]string{
			mcStream(0, "video", false, ""),
			mcStream(1, "audio", false, ""),
			mcStream(2, "attachment", false, `{"filename":"font.otf","mimetype":"font/otf"}`),
		}, ",")
		t.Setenv("MC_INSPECT", mcInspect(streams, ""))
		dir := filepath.Join(srv.Cfg.WWWRoot, "g2")
		src := mcMediaFile(t, dir, "clean.mp4", original)

		task := taskIDOf(t, srv, postMediaClean(t, srv.handleFileMediaClean, []string{src}))
		res := mcResult(t, task)
		if res.Skipped != 1 || res.Done != 0 || res.Failed != 0 {
			t.Fatalf("干净文件必须记为 1 跳过：done=%d skipped=%d failed=%d", res.Done, res.Skipped, res.Failed)
		}
		if reason := res.Items[0].SkippedReason; !strings.Contains(reason, "本来就是干净的") {
			t.Errorf("跳过原因要写「本来就是干净的」，实际 %q", reason)
		}
		// 负向对照：把"字体保留"改成"字体也删"，这里会变成 done=1 ⇒ 变红。
		if calls := shim.calls(t, "ffmpeg"); len(calls) != 0 {
			t.Fatalf("干净文件一次 ffmpeg 都不能调，实际 %d：%v", len(calls), calls)
		}
		if got, _ := os.ReadFile(src); !bytes.Equal(got, original) {
			t.Error("干净文件必须一个字节都不写")
		}
		mcNoBak(t, dir)
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("不该有临时文件：%v", got)
		}
	})

	t.Run("③ ffmpeg 失败 ⇒ 源在、记 failed、原因原样显示、无 .bak/临时文件", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		t.Setenv("MC_FFMPEG_EXIT", "1")
		t.Setenv("MC_FFMPEG_ERR", "shim-boom: encoder exploded at frame 1")
		dir := filepath.Join(srv.Cfg.WWWRoot, "g3")
		src := mcMediaFile(t, dir, "bad.mkv", original)

		task := taskIDOf(t, srv, postMediaClean(t, srv.handleFileMediaClean, []string{src}))
		res := mcResult(t, task)
		if res.Failed != 1 || res.Done != 0 {
			t.Fatalf("应记 1 失败，实际 done=%d failed=%d", res.Done, res.Failed)
		}
		// 原因必须原样带出 ffmpeg 的 stderr（不许换成"处理失败"）。
		if got := res.Items[0].Error; !strings.Contains(got, "shim-boom: encoder exploded at frame 1") {
			t.Errorf("失败原因要原样显示 ffmpeg 的 stderr，实际 %q", got)
		}
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("失败后源文件必须原样保留：err=%v", err)
		}
		mcNoBak(t, dir)
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("失败后临时文件必须清掉：%v", got)
		}
	})

	t.Run("④ 时长核对不过 ⇒ 源在、原因如实、无 .bak/临时文件", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		t.Setenv("MC_IN_DURATION", "1200")
		t.Setenv("MC_OUT_DURATION", "60") // 差 1140 秒，必须拒绝替换
		dir := filepath.Join(srv.Cfg.WWWRoot, "g4")
		src := mcMediaFile(t, dir, "short.mkv", original)

		task := taskIDOf(t, srv, postMediaClean(t, srv.handleFileMediaClean, []string{src}))
		res := mcResult(t, task)
		if res.Failed != 1 {
			t.Fatalf("时长对不上必须记失败，实际 done=%d skipped=%d failed=%d", res.Done, res.Skipped, res.Failed)
		}
		if got := res.Items[0].Error; !strings.Contains(got, "时长核对不过") {
			t.Errorf("失败原因要如实写「时长核对不过」，实际 %q", got)
		}
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("时长不过时源文件必须原样保留：err=%v", err)
		}
		mcNoBak(t, dir)
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("时长不过时临时文件必须清掉：%v", got)
		}
	})

	t.Run("⑤ 路径越界 ⇒ 403 且不建任务（plan 与 clean 都拒绝）", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		for name, h := range map[string]http.HandlerFunc{
			"media-clean-plan": srv.handleFileMediaCleanPlan,
			"media-clean":      srv.handleFileMediaClean,
		} {
			rec := postMediaClean(t, h, []string{"/etc/passwd"})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s：白名单外的路径必须 403，实际 %d（body=%s）", name, rec.Code, rec.Body.String())
			}
		}
		if n := len(srv.Tasks.List()); n != 0 {
			t.Errorf("越界拒绝时不该创建任务，实际 %d 个", n)
		}
	})

	t.Run("⑥ ctx 取消 ⇒ 源在、临时文件清干净、记 canceled", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		t.Setenv("MC_FFMPEG_SLEEP", "30") // 垫片写完产物就挂住，好让我们在中间取消
		dir := filepath.Join(srv.Cfg.WWWRoot, "g6")
		src := mcMediaFile(t, dir, "slow.mkv", original)

		rec := postMediaClean(t, srv.handleFileMediaClean, []string{src})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("必须 202，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				TaskID string `json:"task_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.TaskID == "" {
			t.Fatalf("没有 task_id：%s", rec.Body.String())
		}
		// 等临时文件真的出现（说明已经跑到写产物那一步），再取消。
		deadline := time.Now().Add(10 * time.Second)
		for len(mcTemps(t, dir)) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("10 秒内没看到临时文件，无法验证取消路径")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := srv.Tasks.Cancel(body.Data.TaskID); err != nil {
			t.Fatalf("取消任务失败：%v", err)
		}
		task := waitTaskDone(t, srv, body.Data.TaskID)
		if got := task.Status(); got != tasks.StatusCanceled {
			t.Fatalf("取消后状态应为 canceled，实际 %s（错误=%v）", got, task.Meta().Error)
		}
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("取消后源文件必须原样保留：err=%v", err)
		}
		mcNoBak(t, dir)
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("取消后临时文件必须清干净：%v", got)
		}
		if res := mcResult(t, task); res.Pending < 1 {
			t.Errorf("取消时应有未处理项（Pending>=1），实际 %d", res.Pending)
		}
	})

	t.Run("⑦ 勾选保留备份 ⇒ .bak 在且内容 == 原文件、结果带备份名", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		dir := filepath.Join(srv.Cfg.WWWRoot, "g7")
		src := mcMediaFile(t, dir, "keep.mkv", original)

		task := taskIDOf(t, srv, postMediaCleanKeep(t, srv.handleFileMediaClean, []string{src}, true))
		res := mcResult(t, task)
		if res.Done != 1 || res.Failed != 0 {
			t.Fatalf("应 1 成功，实际 done=%d failed=%d（msg=%s）", res.Done, res.Failed, res.Msg)
		}
		if !res.KeepBackup {
			t.Error("勾选备份时结果 keep_backup 必须为 true")
		}
		got, err := os.ReadFile(src + ".bak")
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf(".bak 必须存在且是原文件内容：err=%v content=%q", err, got)
		}
		if res.Items[0].Backup != filepath.Base(src)+".bak" {
			t.Errorf("结果应带备份名 %q，实际 %q", filepath.Base(src)+".bak", res.Items[0].Backup)
		}
		if !strings.Contains(res.Msg, "保留为 .bak") {
			t.Errorf("勾选备份时汇总要说明已保留 .bak，实际 %q", res.Msg)
		}
		if got, _ := os.ReadFile(src); string(got) != "zp-shim-cleaned-output" {
			t.Errorf("原路径应是清理后的新文件，实际 %q", got)
		}
	})

	t.Run("⑧ 勾选保留备份 + 已有 .bak ⇒ 绝不覆盖，本次落 .bak-2", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		dir := filepath.Join(srv.Cfg.WWWRoot, "g8")
		src := mcMediaFile(t, dir, "again.mkv", original)
		existing := []byte("existing-backup-must-not-change")
		if err := os.WriteFile(src+".bak", existing, 0o644); err != nil {
			t.Fatal(err)
		}

		task := taskIDOf(t, srv, postMediaCleanKeep(t, srv.handleFileMediaClean, []string{src}, true))
		res := mcResult(t, task)
		if res.Done != 1 || res.Failed != 0 {
			t.Fatalf("应 1 成功，实际 done=%d failed=%d", res.Done, res.Failed)
		}
		if got, _ := os.ReadFile(src + ".bak"); !bytes.Equal(got, existing) {
			t.Errorf("既有 .bak 绝不能被覆盖，实际 %q", got)
		}
		if got, err := os.ReadFile(src + ".bak-2"); err != nil || !bytes.Equal(got, original) {
			t.Errorf("本次备份应落 .bak-2 且是原文件内容：err=%v content=%q", err, got)
		}
		if res.Items[0].Backup != filepath.Base(src)+".bak-2" {
			t.Errorf("结果应带备份名 .bak-2，实际 %q", res.Items[0].Backup)
		}
	})

	t.Run("⑨ 勾选保留备份 + 替换失败 ⇒ 从 .bak 还原源、无 .bak、无临时文件", func(t *testing.T) {
		srv, _ := newTestServer(t)
		newMCShim(t)
		t.Setenv("MC_INSPECT", mcInspect(mcStream(2, "video", true, ""), `{"title":"t"}`))
		dir := filepath.Join(srv.Cfg.WWWRoot, "g9")
		src := mcMediaFile(t, dir, "rename-fail.mkv", original)

		// 注入：只在"把临时文件改名成正式文件"这一步失败（其余都走真 rename）。
		// 必须用 Resolve 后的真实路径比较：macOS 上 /tmp 是 /private/tmp 的软链接。
		realSrc, rerr := srv.fileManager().Resolve(src, false)
		if rerr != nil {
			t.Fatal(rerr)
		}
		restore := mediaclean.SetRenameFuncForTest(func(oldpath, newpath string) error {
			if strings.Contains(filepath.Base(oldpath), ".zp-clean-") && newpath == realSrc {
				return os.ErrInvalid
			}
			return os.Rename(oldpath, newpath)
		})
		t.Cleanup(restore)

		task := taskIDOf(t, srv, postMediaCleanKeep(t, srv.handleFileMediaClean, []string{src}, true))
		res := mcResult(t, task)
		if res.Failed != 1 {
			t.Fatalf("替换失败必须记失败，实际 done=%d failed=%d", res.Done, res.Failed)
		}
		if got := res.Items[0].Error; !strings.Contains(got, "替换失败") {
			t.Errorf("失败原因要说清替换失败，实际 %q", got)
		}
		// 最关键的一条：源必须被还原（绝不能"源没了、只剩 .bak"）。
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("替换失败后源文件必须还原：err=%v", err)
		}
		mcNoBak(t, dir)
		if got := mcTemps(t, dir); len(got) != 0 {
			t.Errorf("替换失败后临时文件必须清掉：%v", got)
		}
	})
}

// 变异验证（均已恢复；回看时判据仍由上面九条断言守着）：
//   A 默认改成保留备份（replaceFile 忽略 keepBackup）⇒ ① 变红（磁盘上出现 .bak）；
//   B 时长核对放宽到 1e9 ⇒ ④ 变红；
//   C 附件判据退回旧的"只删图片附件"名单（字体也删）⇒ ① 变红（argv 含 -0:5、Note 附件计数错）+ ② 变红；
//   D 去掉 -map_metadata -1 ⇒ ① 变红（argv 缺该参数）；
//   E 去掉三条 -metadata:s:* title= ⇒ ① 变红；
//   F 去掉 language / 附件 filename 还原 ⇒ ① 变红（argv 缺 -metadata:s:a:0 language=… 等）。
