package services

import (
	"strings"
	"testing"

	"github.com/zizdog/zizpanel/internal/plugins"
)

// TestEveryAllowedHookHasImplementation 白名单里的每个内建补丁名**必须**有真实实现。
//
// 这是"字段不许是摆设"的那条门禁：
//   - 有人往 plugins.builtinHooks 里加一个名字却没在面板里实现 ⇒ 插件作者写了它也不会生效；
//   - 有人删掉/改名某个实现 ⇒ 白名单里的名字就成了谎话。
//
// 两种情况都在这里失败，并直接指出该补哪里。
func TestEveryAllowedHookHasImplementation(t *testing.T) {
	names := plugins.BuiltinHookNames()
	if len(names) == 0 {
		t.Fatal("内建补丁白名单不该为空（Syncthing 那条补丁就在用）")
	}
	for _, name := range names {
		impl, ok := HookImplementation(name)
		if !ok {
			t.Errorf("白名单里的 %q 没有实现登记：要么在 services/hooks.go 里登记它，\n"+
				"要么把它从 plugins.builtinHooks 里删掉（不许留一个写了也不生效的名字）", name)
			continue
		}
		if impl.Where == "" {
			t.Errorf("%q 的实现登记缺 Where（排障时要能按它找到代码）", name)
		}
		if _, ok := FindApp(impl.AppID); !ok {
			t.Errorf("%q 登记的应用 %q 不在应用目录里（改名了？）", name, impl.AppID)
		}
	}
	// 反向对照：登记簿里也不该有白名单之外的名字（否则等于偷偷支持了没公开的能力）
	for name := range builtinHookImpls {
		if !plugins.HookIsAllowed(name) {
			t.Errorf("实现登记簿里的 %q 不在白名单里：要么加进 plugins.builtinHooks 并写进文档，要么删掉", name)
		}
	}
}

// TestSeedFieldIsRefusedHonestly config.seed 还没有执行通路 ⇒ 校验必须**如实拒绝**。
//
// 为什么值得一条门禁：计划文本过去会写"写入配置（模板 …）"——那是彻头彻尾的谎报。
// 现在拒绝它，作者立刻知道这条路没通，而不是发布了才发现什么都没发生。
func TestSeedFieldIsRefusedHonestly(t *testing.T) {
	_, err := plugins.Parse([]byte(`{
	  "schema": "zizpanel.app/v1", "id": "seeded", "name": "模板应用", "icon": "🧪",
	  "source": {"kind": "brew", "formula": "seeded", "checksum": "sha256"},
	  "run": {"mode": "brew-service"},
	  "config": {"path": "{brew}/etc/seeded.conf", "seed": "template.conf"},
	  "verify": {"any_of": [{"kind": "port", "port": 19997, "timeout": "30s"}]},
	  "health": {"kind": "port", "port": 19997},
	  "uninstall": {"optional_data": ["{brew}/var/seeded"], "formula": "seeded"}
	}`), "seeded.json")
	if err == nil {
		t.Fatal("config.seed 应当被如实拒绝（没有执行通路）")
	}
	if !strings.Contains(err.Error(), "seed") || !strings.Contains(err.Error(), "执行通路") {
		t.Errorf("拒绝理由要写清是「没有执行通路」：%v", err)
	}
	// 反向对照：不带 seed 的同一份声明必须通过
	if _, err := plugins.Parse([]byte(`{
	  "schema": "zizpanel.app/v1", "id": "seeded2", "name": "无模板", "icon": "🧪",
	  "source": {"kind": "brew", "formula": "seeded", "checksum": "sha256"},
	  "run": {"mode": "brew-service"},
	  "config": {"path": "{brew}/etc/seeded.conf", "format": "kv", "set": {"port": "19997"}},
	  "verify": {"any_of": [{"kind": "port", "port": 19997, "timeout": "30s"}]},
	  "health": {"kind": "port", "port": 19997},
	  "uninstall": {"optional_data": ["{brew}/var/seeded"], "formula": "seeded"}
	}`), "seeded2.json"); err != nil {
		t.Fatalf("不带 seed 的声明不该被拒：%v", err)
	}
}
