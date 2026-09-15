package web

import (
	"net/http"
	"strings"
)

// ============================================================================
//  调用密钥（receiver v1.6.0）：多密钥 + 每密钥额度 + 用量统计
//
//  为什么改成"添加密钥"而不是"更改共享密钥"：
//    · 一把共享密钥意味着**任何一把泄露就要全员换**，而且分不清是谁在用；
//    · 有了每把密钥的额度，才能给不同站点不同的量（单位：字）；
//    · 有了用量统计，才能回答"这个月是谁用掉的"。
//
//  密钥与额度在 keys.json（接收端按 mtime 热加载）—— 加/停/删/改额度
//  **不需要重启**接收端，这一点很要紧：它可能正在替用户合成。
// ============================================================================

// handleVoiceKeys GET /api/v1/voice/receiver/keys
// 返回密钥列表（含量化后的用量）+ 全局合计。
func (s *Server) handleVoiceKeys(w http.ResponseWriter, r *http.Request) {
	view, err := s.svcManager().VoiceKeysView(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w, view)
}

// handleVoiceUsage GET /api/v1/voice/receiver/usage
func (s *Server) handleVoiceUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.svcManager().VoiceUsage(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w, usage)
}

// handleVoiceKeyAdd POST /api/v1/voice/receiver/keys
// body: {name, value?, quota_chars?}
//
// value 留空时由面板生成（ttsv-<32 hex>）。返回体里带明文密钥 ——
// 这是**唯一一次**能直接看到它的机会（之后列表里只给掩码），
// 前端必须把它显示成可复制区块，别让用户自己拼。
func (s *Server) handleVoiceKeyAdd(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Name       string `json:"name"`
		Value      string `json:"value"`
		QuotaChars *int64 `json:"quota_chars"`
	}{}
	if r.ContentLength > 0 {
		if err := decode(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	quota := int64(0)
	if req.QuotaChars != nil {
		quota = *req.QuotaChars
	}
	if quota < 0 {
		fail(w, http.StatusBadRequest, "额度不能是负数")
		return
	}
	key, err := s.svcManager().AddVoiceKey(strings.TrimSpace(req.Name), req.Value, quota, true)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"key": key})
}

// handleVoiceKeyUpdate PATCH /api/v1/voice/receiver/keys/{id}
// body: {name?, quota_chars?, enabled?, value?}
//
// 用指针字段区分"没传"与"传了空值"：额度改成 0 是"不限量"这个有意义的动作。
func (s *Server) handleVoiceKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	req := struct {
		Name       *string `json:"name"`
		QuotaChars *int64  `json:"quota_chars"`
		Enabled    *bool   `json:"enabled"`
		Value      *string `json:"value"`
	}{}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.QuotaChars != nil && *req.QuotaChars < 0 {
		fail(w, http.StatusBadRequest, "额度不能是负数（0 = 不限）")
		return
	}
	key, err := s.svcManager().UpdateVoiceKey(id, req.Name, req.QuotaChars, req.Enabled, req.Value)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"key": key})
}

// handleVoiceKeyDelete DELETE /api/v1/voice/receiver/keys/{id}
//
// 删除后该密钥立刻失效（接收端热加载），历史用量仍保留在统计里 ——
// 所以删除是"撤销访问"，不是"抹掉记录"。
func (s *Server) handleVoiceKeyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.svcManager().DeleteVoiceKey(id); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"id": id})
}
