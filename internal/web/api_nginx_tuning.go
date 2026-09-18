package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zizdog/zizpanel/internal/priv"
)

// ============================================================================
//  Nginx 性能参数（宝塔式「性能调整」表单）
//
//  用户 2026-09-18 给了宝塔的截图并明确要求："参考宝塔的样子" + "这些常用更改
//  应该同时做成功能，而不应该是让用户只能编辑配置原文件"。
//
//  分工（与项目其它地方一致）：
//    · 本文件 = 面板侧的**校验与转发**（人话错误 + 5xx/4xx 语义）；
//    · internal/priv/nginxtuning.go = 真正的改写（备份 → 改写 → nginx -t → 失败回滚
//      → reload → 回读生效值），跑在提权助手里。
//
//  为什么同步返回而不是走任务中心：写一个文件 + `nginx -t` + reload 是**秒级**动作
//  （项目里"秒级动作可同步返回"的约定），而且用户在表单上按下保存后马上要看到
//  "哪一项生效了"的回读结果 —— 包成任务反而多两次跳转。
// ============================================================================

// nginxTuningView 是 GET 的响应（与 helper 的 Data 同形，加一层面板侧信息）。
type nginxTuningView struct {
	priv.NginxTuningReadResult
	// HelperUnavailable 为 true 表示这次读取根本没做成（提权助手不可用），
	// 界面必须显示"读不到"，绝不能显示成"出厂默认值"。
	HelperUnavailable bool `json:"helper_unavailable,omitempty"`
}

// handleNginxTuningGet 读取 nginx 性能参数（含真实生效值回读）。
func (s *Server) handleNginxTuningGet(w http.ResponseWriter, r *http.Request) {
	res, err := s.callHelper(r.Context(), "nginx-tuning-read")
	out := nginxTuningView{}
	if err != nil {
		// 助手不可用时**如实说**：不给一份"出厂默认值"冒充现状（那会让用户
		// 以为自己的 nginx 是默认配置，甚至照着保存一遍把真实配置冲掉）。
		out.HelperUnavailable = true
		out.Values = priv.DefaultTuning()
		out.Error = "读取 nginx 配置失败（提权助手不可用）：" + err.Error() +
			"。面板需要以 root 运行才能读改 nginx.conf；请确认 zizpanel 服务在跑。"
		ok(w, out)
		return
	}
	raw, merr := json.Marshal(res["data"])
	if merr != nil {
		fail(w, http.StatusInternalServerError, "解析助手返回失败: "+merr.Error())
		return
	}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		fail(w, http.StatusInternalServerError, "解析助手返回失败: "+uerr.Error())
		return
	}
	ok(w, out)
}

// handleNginxTuningSave 保存 nginx 性能参数。
func (s *Server) handleNginxTuningSave(w http.ResponseWriter, r *http.Request) {
	var v priv.NginxTuning
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&v); err != nil {
		fail(w, http.StatusBadRequest, "参数不是合法 JSON："+err.Error())
		return
	}
	// 面板侧先校验一次（人话错误直接回给用户，不必惊动助手）。
	if err := priv.ValidateTuning(v); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := priv.TuningEncode(v)
	if err != nil {
		fail(w, http.StatusInternalServerError, "编码参数失败: "+err.Error())
		return
	}
	res, err := s.callHelper(r.Context(), "nginx-tuning-write", "-values", enc)
	if err != nil {
		// 助手的错误里已经包含"nginx -t 输出 / 已回滚"这类关键信息，原样透出。
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, merr := json.Marshal(res["data"])
	if merr != nil {
		fail(w, http.StatusInternalServerError, "解析助手返回失败: "+merr.Error())
		return
	}
	out := nginxTuningView{}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		fail(w, http.StatusInternalServerError, "解析助手返回失败: "+uerr.Error())
		return
	}
	s.audit(r, "nginx_tuning", "nginx", fmt.Sprintf(
		"nginx 性能参数：worker_processes=%s worker_connections=%d keepalive=%d gzip=%v comp=%d client_max_body_size=%dm",
		out.Values.WorkerProcesses, out.Values.WorkerConnections, out.Values.KeepaliveTimeout,
		out.Values.Gzip, out.Values.GzipCompLevel, out.Values.ClientMaxBodySizeMB), true, "")
	ok(w, out)
}
