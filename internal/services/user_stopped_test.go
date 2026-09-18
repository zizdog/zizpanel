package services

import (
	"context"
	"testing"
)

// ============================================================================
//  「用户主动停的」与「它自己崩了」必须分得清（2026-09-18 用户报障）
//
//  用户原话："我手动停止了 Qwen3 TTS 和 TtsVoice 音色接收端；然后它们就跑到：
//  2 个需要处理中。软件卡片提示：检查地址 … 检查未通过 … 这是我主动停的，
//  不是运行错误，应该分清楚！"
//
//  运行状态本身两者一模一样（都没在跑），区别只能来自**用户的操作历史**，
//  所以 stop/start 必须把意图落库（Enabled），健康检查与"需要处理"都看它。
// ============================================================================

func TestRememberUserIntentPersistsStopAndStart(t *testing.T) {
	m, repo := newTestManager(t)
	ctx := context.Background()
	svc := &Service{
		Name: "qwen3-tts", DisplayName: "Qwen3 TTS", Kind: KindNative,
		LaunchLabel: "com.zizdog.qwen3tts", Category: "ai", Enabled: true, Managed: true,
	}
	if err := repo.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}

	// 用户点「停止」→ 意图落库（enabled=false）
	if w := m.rememberUserIntent(ctx, svc, "stop"); w != "" {
		t.Errorf("正常写库不该有警告，实际 %q", w)
	}
	got, err := repo.Get(ctx, "qwen3-tts")
	if err != nil {
		t.Fatal(err)
	}
	if !got.StoppedByUser {
		t.Errorf("停止之后 stopped_by_user 必须是 true（否则面板会把它当成'应该在跑却没跑'）: %+v", got)
	}

	// 用户点「启动」→ 意图回到 true
	if w := m.rememberUserIntent(ctx, got, "start"); w != "" {
		t.Errorf("正常写库不该有警告，实际 %q", w)
	}
	again, err := repo.Get(ctx, "qwen3-tts")
	if err != nil {
		t.Fatal(err)
	}
	if again.StoppedByUser {
		t.Errorf("启动之后 stopped_by_user 必须回到 false: %+v", again)
	}
}

func TestSkipHealthForUserStopped(t *testing.T) {
	stopped := &Service{Name: "voicereceiver", StoppedByUser: true}
	running := State{Running: false, Status: "stopped"}
	if !skipHealthForUserStopped(stopped, running) {
		t.Error("用户主动停止的服务必须跳过健康检查（否则会被算成'需要处理'）")
	}

	// 用户期望它跑（Enabled=true）但没跑 → 健康检查照做（这才可能是故障）
	expected := &Service{Name: "voicereceiver"}
	if skipHealthForUserStopped(expected, running) {
		t.Error("用户期望运行的服务不能跳过健康检查 —— 那会把真故障藏起来")
	}

	// 用户主动停过、但它现在其实在跑（用户手工拉起）：仍然要检查
	//（面板要如实反映它现在答不答得上来）。
	if skipHealthForUserStopped(stopped, State{Running: true}) {
		t.Error("主动停过但它仍在跑：不能跳过检查")
	}
	if skipHealthForUserStopped(nil, running) {
		t.Error("没有服务记录时不该走到这个判据（nil 安全）")
	}
}
