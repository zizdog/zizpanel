package web

import (
	"io"
	"net/http"
	"strings"

	"github.com/zizdog/zizpanel/internal/services"
)

// ============================================================================
//  音色来源（「服务管理 → 音色接收端 → 详情 → 各来源」）
//
//  为什么要有这个页面：receiver v1.4.0 把所有网站的样本都写进同一个
//  <dir>/ref.wav。多站点场景下后传的会覆盖先传的，用户听到的是别的站的
//  音色，界面上却完全看不出来 —— 这次故障（合成返回 0 字节）的根因之一
//  就是那份被覆盖、且被改名的样本。
//
//  v1.5.0 起样本按来源存到 <dir>/<source>/ref.wav，这里就是那批来源的
//  列表 / 替换 / 删除入口。**解析与转码只在接收端做一次**，面板只转发，
//  否则两边对"什么算合法音频"的判定迟早会漂。
// ============================================================================

// maxVoiceUpload 单次上传上限，与接收端的 MAX_BYTES 一致（20MB）。
const maxVoiceUpload = 20 << 20

// handleVoiceSources 列出各来源。GET /api/v1/voice/receiver/sources
func (s *Server) handleVoiceSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.svcManager().VoiceSources(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if list == nil {
		list = []services.VoiceSource{}
	}
	ok(w, map[string]any{"sources": list})
}

// handleVoiceSourceUpload 上传/替换某个来源的样本，或新建一个来源。
// POST /api/v1/voice/receiver/sources （multipart：source、file）
//
// 走同步请求而不是任务中心：接收端把上传限制在 20MB、转码几十毫秒到一两秒，
// 属于"秒级动作"。真正可能慢的是下载，而那是浏览器在做。
func (s *Server) handleVoiceSourceUpload(w http.ResponseWriter, r *http.Request) {
	// 与文件管理/升级包上传同类：全局 ReadTimeout=30s 会掐断慢速上传，
	// 这里一并解除（20MB 在慢速链路上超过 30 秒并不罕见）。
	if err := allowLongUpload(w, r); err != nil && s.Log != nil {
		s.Log.Warn("延长音色上传读超时失败（超过 30 秒的上传可能被中断）: %v", err)
	}
	// 多留 1MB 给表单本身的开销
	if r.ContentLength > maxVoiceUpload+(1<<20) {
		fail(w, http.StatusRequestEntityTooLarge,
			uploadLimitMessage(r.ContentLength, maxVoiceUpload, "音频文件过大（参考音频不需要这么大）"))
		return
	}
	if err := r.ParseMultipartForm(maxVoiceUpload + (1 << 20)); err != nil {
		if isBodyTooLarge(err) {
			fail(w, http.StatusRequestEntityTooLarge,
				uploadLimitMessage(r.ContentLength, maxVoiceUpload, "音频文件过大（参考音频不需要这么大）"))
			return
		}
		fail(w, http.StatusBadRequest, "表单解析失败（文件可能超过 20MB）："+err.Error())
		return
	}
	source := strings.TrimSpace(r.FormValue("source"))
	if source == "" {
		fail(w, http.StatusBadRequest, "缺少来源标识（source）")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "没有收到音频文件")
		return
	}
	defer file.Close()
	if header.Size > maxVoiceUpload {
		fail(w, http.StatusRequestEntityTooLarge, "文件超过 20MB，参考音频不需要这么大")
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, maxVoiceUpload+1))
	if err != nil {
		fail(w, http.StatusBadRequest, "读取上传文件失败："+err.Error())
		return
	}
	if len(data) > maxVoiceUpload {
		fail(w, http.StatusRequestEntityTooLarge, "文件超过 20MB，参考音频不需要这么大")
		return
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	res, err := s.svcManager().VoiceSourceUpload(r.Context(), source, header.Filename, contentType, data)
	if err != nil {
		// 接收端的 400 是"你传的文件不对"，属于用户输入问题，原样透出
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, res)
}

// handleVoiceSourceDelete 删除某个来源。
// DELETE /api/v1/voice/receiver/sources?source=xxx
func (s *Server) handleVoiceSourceDelete(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		fail(w, http.StatusBadRequest, "缺少来源标识（source）")
		return
	}
	if err := s.svcManager().VoiceSourceDelete(r.Context(), source); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w, map[string]any{"source": services.SanitizeVoiceSource(source)})
}
