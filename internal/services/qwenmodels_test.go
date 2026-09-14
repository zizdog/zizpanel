package services

import (
	"context"
	"strings"
	"testing"
)

// TestQwenModelsSingleCloneModel 是这次改动（2026-09-14）的核心断言：
// 清单里**只有一个**模型，且是克隆用的 1.7B-Base-8bit。
//
// 背景：网站侧插件现在只支持「自定义音色」（克隆），预置音色（CustomVoice）
// 整体下线，对应的权重也从两台机器上删掉腾空间了。所以面板不能再列出
// CustomVoice —— 那会渲染出一个"看起来能切过去"的假选项，点它就是失败。
//
// 旧的 TestQwenModelsCoverBothCapabilities（要求 clone + preset 两种能力都在）
// 随这次需求一起删掉了：它锁的是已经作废的契约。
func TestQwenModelsSingleCloneModel(t *testing.T) {
	if len(QwenModels) != 1 {
		t.Fatalf("模型清单里应恰好 1 个模型（预置音色已下线），实际 %d 个", len(QwenModels))
	}
	m := QwenModels[0]
	if m.Role != "clone" {
		t.Errorf("唯一的模型必须是克隆用的（role=clone），实际 role=%q", m.Role)
	}
	if m.Name != qwenDefaultModel {
		t.Errorf("唯一的模型必须就是默认模型 %s，实际 %s", qwenDefaultModel, m.Name)
	}
	if !strings.Contains(m.Name, "1.7B-Base") {
		t.Errorf("模型名应指向 1.7B-Base，实际 %s", m.Name)
	}
	if m.Name == "" || m.Label == "" || m.Note == "" {
		t.Errorf("模型字段不完整: %+v", m)
	}
	if !strings.Contains(m.Name, "Qwen3-TTS") {
		t.Errorf("模型名不像 Qwen3-TTS: %s", m.Name)
	}
}

// TestQwenManifestDropsRetiredModels 防止有人把已经下线的模型加回清单。
//
// 这些名字对应的权重在真机上已经删了（腾空间）；一旦重新出现在清单里，
// 界面会显示"未下载"、用户点"加载"会失败，而网站侧根本不会请求它们。
func TestQwenManifestDropsRetiredModels(t *testing.T) {
	for _, mdl := range QwenModels {
		for _, banned := range []string{"CustomVoice", "0.6B"} {
			if strings.Contains(mdl.Name, banned) {
				t.Errorf("清单里不应再有 %s 模型（网站侧已下线、权重已删）: %s", banned, mdl.Name)
			}
		}
	}
}

// TestQwenDefaultModelIsListed 默认模型必须在列表里，否则插件契约会指向一个不存在的模型。
func TestQwenDefaultModelIsListed(t *testing.T) {
	for _, m := range QwenModels {
		if m.Name == qwenDefaultModel {
			return
		}
	}
	t.Fatalf("默认模型 %s 不在 QwenModels 里", qwenDefaultModel)
}

// TestQwenModelsUseSameQuantization 两个模型的量化级别必须一致。
// 混用不同量化会让音质/速度表现不一致，排查时非常迷惑。
func TestQwenModelsUseSameQuantization(t *testing.T) {
	for _, m := range QwenModels {
		if !strings.HasSuffix(m.Name, "-8bit") {
			t.Errorf("模型 %s 不是 8bit（应与另一个保持一致）", m.Name)
		}
	}
}

// TestSetQwenModelRejectsUnknown 未知模型必须明确报错，不能静默当成成功。
func TestSetQwenModelRejectsUnknown(t *testing.T) {
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	err := m.SetQwenModel(context.Background(), "not/a-real-model")
	if err == nil {
		t.Fatal("未知模型应报错")
	}
	if !strings.Contains(err.Error(), "未知") {
		t.Errorf("错误信息应说明模型未知，实际: %v", err)
	}
}

// TestSetQwenModelRejectsUndownloaded 权重没下好时不能去加载，
// 否则服务端会返回一个难懂的加载失败。
func TestSetQwenModelRejectsUndownloaded(t *testing.T) {
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	err := m.SetQwenModel(context.Background(), QwenModels[0].Name)
	if err == nil {
		t.Fatal("未下载的模型应报错")
	}
	if !strings.Contains(err.Error(), "尚未下载") {
		t.Errorf("错误信息应说明未下载，实际: %v", err)
	}
}

// TestQwenModelsStatusWorksWhenServiceDown 服务没起来时也要能返回"装没装"。
//
// 这一点很重要：用户最需要看到模型状态的时候，往往正是服务挂了的时候。
// 如果这里因为连不上 8880 就整体报错，界面会一片空白，反而看不出问题在哪。
func TestQwenModelsStatusWorksWhenServiceDown(t *testing.T) {
	m := NewManager(nil, Options{UserHome: t.TempDir(), UserName: "zizdog"})
	// 指向一个必然没人监听的端口，模拟"服务没起来"。
	// 直接用 8880 会打到本机真实运行的 Qwen 服务（本机确实有一台），
	// 测出来的就不是被测代码的行为了。
	m.qwenPortOverride = 1
	list := m.QwenModelsStatus(context.Background())
	if len(list) != len(QwenModels) {
		t.Fatalf("服务不可用时仍应返回全部 %d 个模型，实际 %d", len(QwenModels), len(list))
	}
	for _, st := range list {
		if st.Loaded {
			t.Errorf("服务不可用时不应有模型被标记为已加载: %s", st.Name)
		}
		if st.Downloaded {
			t.Errorf("空目录不应报告已下载: %s", st.Name)
		}
	}
}
