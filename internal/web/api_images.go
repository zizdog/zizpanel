package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zizdog/zizpanel/internal/files"
	"github.com/zizdog/zizpanel/internal/imgopt"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  图片压缩（应用「图片压缩（libvips）」的能力入口）
//
//  设计取舍（为什么不是"装一个带 WebUI 的服务"）：
//    · 引擎是 libvips 的 `vips` 命令行（原生 arm64、无常驻进程、没有端口），
//      与 ffmpeg 在面板里的定位一模一样：**能力型应用**；
//    · 界面就是面板自己的页面（文件管理工具栏的「🖼️ 图片压缩」），
//      不需要代理、不需要子路径、不会多出一个要守护的服务；
//    · 长任务走**任务中心**（批量压缩可能要几分钟），每个文件一条进度日志 ——
//      用户关掉窗口也能在任务中心看进度与结果。
//
//  安全边界（和文件管理同一套）：
//    · 目录/文件都必须过 files.Manager.Resolve（白名单 + 软链接解析）；
//    · 只处理白名单内的图片扩展名；覆盖模式也只覆盖图片。
// ============================================================================

// imgEngine 返回这台机器上的 libvips 引擎（就地探测，不缓存 —— 用户可能刚装完）。
func (s *Server) imgEngine() imgopt.Engine {
	return imgopt.DetectEngine(s.Cfg.BrewPrefix)
}

// imgDefaultDir 把请求里的目录解析成白名单内的真实路径。
func (s *Server) imgResolveDir(raw string, allowMissing bool) (string, error) {
	mgr := s.fileManager()
	dir := strings.TrimSpace(raw)
	if dir == "" {
		dir = mgr.DefaultDir(s.Cfg.WWWRoot)
		if dir == "" {
			return "", fmt.Errorf("没有可用的默认目录（面板的文件白名单为空）")
		}
		return dir, nil
	}
	real, err := mgr.Resolve(dir, allowMissing)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("目录不存在或读不到: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", real)
	}
	return real, nil
}

// imgEngineResponse 是 GET /api/v1/images/engine 的响应。
type imgEngineResponse struct {
	Available   bool     `json:"available"`
	Bin         string   `json:"bin,omitempty"`
	Version     string   `json:"version,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Formats     []string `json:"formats"`
	Quality     int      `json:"default_quality"`
	MaxEdge     []int    `json:"max_edge_choices"`
	Dir         string   `json:"dir,omitempty"`
	ImageCount  int      `json:"image_count"`
	TotalBytes  int64    `json:"total_bytes"`
	ScanError   string   `json:"scan_error,omitempty"`
	MarketAppID string   `json:"market_app_id"`
}

// handleImageEngine 返回引擎状态 + 目标目录里有多少张图（只统计，不改任何东西）。
//
// 界面用它决定：能不能开始压缩、要不要先给一个"去安装"的按钮、
// 以及"这个目录里有 12 张图、共 34 MB"这样能让用户先判断值不值得跑的实话。
func (s *Server) handleImageEngine(w http.ResponseWriter, r *http.Request) {
	eng := s.imgEngine()
	resp := imgEngineResponse{
		Available:   eng.Available(),
		Bin:         eng.Bin,
		Version:     eng.Version,
		Reason:      eng.Reason,
		Formats:     formatStrings(),
		Quality:     imgopt.DefaultQuality,
		MaxEdge:     []int{0, 1280, 1920, 2560, 3840},
		MarketAppID: imgCompressAppID,
	}
	dir, err := s.imgResolveDir(r.URL.Query().Get("dir"), false)
	if err != nil {
		resp.ScanError = err.Error()
		ok(w, resp)
		return
	}
	resp.Dir = dir
	files, total, serr := scanImages(dir, r.URL.Query().Get("recursive") == "1", imgScanMax)
	if serr != nil {
		resp.ScanError = serr.Error()
	} else {
		resp.ImageCount, resp.TotalBytes = len(files), total
	}
	ok(w, resp)
}

// formatStrings 把格式枚举转成前端要的字符串列表。
func formatStrings() []string {
	out := make([]string, 0, len(imgopt.Formats))
	for _, f := range imgopt.Formats {
		out = append(out, string(f))
	}
	return out
}

// imgScanMax 是单次扫描的图片数量上限（防止误选一个大目录把内存/时间耗光）。
const imgScanMax = 2000

// scanImages 扫描目录下的图片（不递归时只看这一层）。
//
// 返回数量与总字节数；超过上限如实报错（不静默截断 —— 静默截断会让用户以为
// "压缩完了"，其实只压了一半）。
func scanImages(dir string, recursive bool, limit int) ([]string, int64, error) {
	var out []string
	var total int64
	add := func(p string, size int64) error {
		if len(out) >= limit {
			return fmt.Errorf("目录里的图片超过 %d 张，请分批处理（或先用「搜索」缩小范围）", limit)
		}
		out = append(out, p)
		total += size
		return nil
	}
	if !recursive {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil, 0, err
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if !imgopt.IsImagePath(p) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if err := add(p, info.Size()); err != nil {
				return nil, 0, err
			}
		}
		sort.Strings(out)
		return out, total, nil
	}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个子目录读不到不阻断整次扫描
		}
		if d.IsDir() {
			// 面板自己的内部目录不进：压缩它们在语义上毫无意义。
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "_logs" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !imgopt.IsImagePath(p) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		return add(p, info.Size())
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(out)
	return out, total, nil
}

// imgCompressAppID 是市场里这个应用的 ID（引擎探测与安装入口共用一处，不写第二遍）。
const imgCompressAppID = "imgcompress"

// imgCompressRequest 是 POST /api/v1/images/compress 的请求体。
type imgCompressRequest struct {
	Dir       string `json:"dir"`
	Recursive bool   `json:"recursive"`
	imgopt.Options
}

// handleImageCompress 批量压缩（走任务中心：202 + task_id）。
func (s *Server) handleImageCompress(w http.ResponseWriter, r *http.Request) {
	var req imgCompressRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	opt, err := req.Options.Normalize()
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 引擎不在就**立刻**拒绝，并给出安装入口 —— 绝不开一个注定失败的任务。
	eng := s.imgEngine()
	if !eng.Available() {
		fail(w, http.StatusConflict, eng.Reason)
		return
	}
	dir, err := s.imgResolveDir(req.Dir, false)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 扫描两次是刻意的：这里要拿到**确定**的清单再开任务（用户在任务开始前
	// 就该知道"这次要处理几张图"），任务里再用这份清单，不再重新扫（避免中途
	// 新上传的文件混进来导致"进度对不上"）。
	targets, totalBytes, err := scanImages(dir, req.Recursive, imgScanMax)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(targets) == 0 {
		fail(w, http.StatusBadRequest, "目录 "+dir+" 里没有可压缩的图片"+
			"（支持 jpg/png/webp/avif/heic/tif/gif/bmp；要看子目录请勾上「包含子目录」）")
		return
	}
	overwrite := opt.Overwrite
	title := fmt.Sprintf("压缩 %d 张图片（%s）", len(targets), humanBytes(totalBytes))

	s.launchTask(w, r, "image_compress", dir, title,
		"image_compress", func(ctx context.Context, log tasks.LogFunc) (any, error) {
			type item struct {
				Src    string `json:"src"`
				Dst    string `json:"dst"`
				Before int64  `json:"before"`
				After  int64  `json:"after"`
				Saved  int64  `json:"saved"`
				Skip   bool   `json:"skipped"`
				Err    string `json:"error,omitempty"`
			}
			res := struct {
				Dir        string `json:"dir"`
				Total      int    `json:"total"`
				Done       int    `json:"done"`
				Failed     int    `json:"failed"`
				Skipped    int    `json:"skipped"`
				BeforeByte int64  `json:"before_bytes"`
				AfterByte  int64  `json:"after_bytes"`
				SavedByte  int64  `json:"saved_bytes"`
				Items      []item `json:"items"`
				Overwrite  bool   `json:"overwrite"`
			}{Dir: dir, Total: len(targets), Overwrite: overwrite}

			// 并发 2：libvips 自己就是多线程的，再叠高并发只会抢内存
			// （文档里的经验值也是 2）。
			type job struct{ path string }
			jobs := make(chan job)
			type outcome struct {
				res imgopt.Result
				err error
			}
			outs := make(chan outcome)
			workers := 2
			if len(targets) < workers {
				workers = len(targets)
			}
			for i := 0; i < workers; i++ {
				go func() {
					for j := range jobs {
						rr, err := eng.CompressFile(ctx, j.path, opt)
						outs <- outcome{rr, err}
					}
				}()
			}
			go func() {
				defer close(jobs)
				for _, t := range targets {
					select {
					case <-ctx.Done():
						return
					case jobs <- job{t}:
					}
				}
			}()

			for i := 0; i < len(targets); i++ {
				oc := <-outs
				rr := oc.res
				it := item{Src: rr.Src, Dst: rr.Dst, Before: rr.Before, After: rr.After}
				if oc.err != nil {
					it.Err = oc.err.Error()
					res.Failed++
					log(tasks.LevelErr, "✗ "+filepath.Base(rr.Src)+"："+oc.err.Error())
				} else if rr.Skipped {
					res.Skipped++
					it.Skip = true
					res.BeforeByte += rr.Before
					// 跳过的文件没有被改写（保留原样），既不算省也不算花。
					log(tasks.LevelWarn, "↷ "+filepath.Base(rr.Src)+"：压完反而更大（"+
						humanBytes(rr.Before)+" → "+humanBytes(rr.After)+"），已保留原文件")
				} else {
					res.Done++
					it.Saved = rr.Saved()
					res.BeforeByte += rr.Before
					res.AfterByte += rr.After
					res.SavedByte += rr.Saved()
					log(tasks.LevelOK, "✓ "+filepath.Base(rr.Src)+"："+humanBytes(rr.Before)+" → "+
						humanBytes(rr.After)+"（省 "+humanBytes(rr.Saved())+"）")
				}
				res.Items = append(res.Items, it)
			}
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("任务被取消（已完成 %d/%d）", res.Done+res.Failed+res.Skipped, res.Total)
			}
			log(tasks.LevelStep, fmt.Sprintf("完成：成功 %d，跳过 %d（压完更大），失败 %d；共省 %s",
				res.Done, res.Skipped, res.Failed, humanBytes(res.SavedByte)))
			if res.Failed == len(targets) {
				return res, fmt.Errorf("全部 %d 张都失败了，第一张的原因见日志", res.Failed)
			}
			return res, nil
		})
}

// humanBytes 把字节数写成人话（日志与标题用）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	i := -1
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// imgWriteError 把 files 包的错误翻译成更贴用户的话（保留原意）。
func imgWriteError(err error) string {
	if errors.Is(err, files.ErrForbidden) {
		return "路径不在面板允许访问的范围内：" + err.Error()
	}
	return err.Error()
}
