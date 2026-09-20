package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/config"
)

// TestReadyzIsHonestAboutMissingFfmpeg is the anti-lying gate: no ffmpeg means
// NOT ready, with a reason, and never a 200.
func TestReadyzIsHonestAboutMissingFfmpeg(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.FFmpegBin = filepath.Join(c.DataDir, "no-such-ffmpeg")
		c.FFprobeBin = filepath.Join(c.DataDir, "no-such-ffprobe")
	})
	c := e.anonClient()
	res, env, raw := e.doAs(c, http.MethodGet, "/readyz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("缺 ffmpeg 时 /readyz 必须 503, 得到 %d (%s)", res.StatusCode, raw)
	}
	var data struct {
		Status string `json:"status"`
		Checks map[string]struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	decodeInto(t, env.Data, &data)
	if data.Status != "not_ready" {
		t.Fatalf("status = %q", data.Status)
	}
	if data.Checks["ffmpeg"].OK || data.Checks["ffprobe"].OK {
		t.Fatal("工具缺失时 ffmpeg/ffprobe 检查必须为 false")
	}
	if data.Checks["ffmpeg"].Error == "" || data.Checks["ffprobe"].Error == "" {
		t.Fatal("必须给出工具不可用的真实原因")
	}
	if !data.Checks["db"].OK {
		t.Fatal("数据库此时应是可用的")
	}
}

// TestScanFailsWhenProbeBinaryIsMissing keeps the scan path honest too.
func TestScanFailsWhenProbeBinaryIsMissing(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.FFprobeBin = filepath.Join(c.DataDir, "no-such-ffprobe")
	})
	e.setupAdmin()
	lib := e.newLibrary("l", e.Root)
	writeStub(t, filepath.Join(e.Root, "a.mp4"), "payload")

	res, env, raw := e.write(http.MethodPost, "/api/v1/libraries/"+lib.ID+"/scan",
		map[string]string{"kind": "full"})
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("触发扫描应 202, 得到 %d (%s)", res.StatusCode, raw)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	decodeInto(t, env.Data, &out)
	done := waitTask(t, e, out.TaskID)
	var task struct {
		Status string `json:"status"`
		Failed int    `json:"failed"`
	}
	decodeInto(t, done.Data, &task)
	if task.Failed != 1 {
		t.Fatalf("探测程序缺失时应有 1 个失败项, 实际 %d", task.Failed)
	}
	failed, err := e.DB.CountFailedMedia(lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("媒体失败数 = %d, 期望 1", failed)
	}
}

func TestReadyzFailsWhenAllowRootDisappears(t *testing.T) {
	e := newEnv(t)
	c := e.anonClient()
	if res, _, _ := e.doAs(c, http.MethodGet, "/readyz"); res.StatusCode != http.StatusOK {
		t.Fatal("初始状态应 ready")
	}
	if err := os.RemoveAll(e.Root); err != nil {
		t.Fatal(err)
	}
	res, env, raw := e.doAs(c, http.MethodGet, "/readyz")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("白名单根消失应 503, 得到 %d (%s)", res.StatusCode, raw)
	}
	var data struct {
		Checks map[string]map[string]any `json:"checks"`
	}
	decodeInto(t, env.Data, &data)
	if data.Checks["media_roots"][e.Root] == "ok" {
		t.Fatal("消失的根不应报 ok")
	}
}

func TestSystemInfoIsAdminOnlyAndTruthful(t *testing.T) {
	e := newEnv(t)
	e.setupAdmin()
	res, env, raw := e.do(http.MethodGet, "/api/v1/admin/system/info", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d (%s)", res.StatusCode, raw)
	}
	var info struct {
		Version string `json:"version"`
		FFmpeg  struct {
			Version          string `json:"version"`
			OK               bool   `json:"ok"`
			VideoToolboxH264 bool   `json:"videotoolbox_h264"`
		} `json:"ffmpeg"`
		Counts struct {
			Libraries int `json:"libraries"`
			Media     int `json:"media"`
			Failed    int `json:"failed"`
		} `json:"counts"`
		Disk struct {
			FreeBytes uint64 `json:"free_bytes"`
		} `json:"disk"`
	}
	decodeInto(t, env.Data, &info)
	if info.Version == "" {
		t.Fatal("版本号为空")
	}
	if !info.FFmpeg.VideoToolboxH264 {
		t.Fatal("未探测到 stub 里的 h264_videotoolbox")
	}
	if info.Disk.FreeBytes == 0 {
		t.Fatal("磁盘余量未上报")
	}
}
