package sysinfo

import (
	"context"
	"runtime"
	"testing"
)

// TestChooseCPUTemp 用 M4 实测的传感器现场（真机 dump 出来的）锁住选值规则。
//
// 这台机器上 46 个传感器里有三类容易选错的：
//   - `PMU tcal` = 51.82°C —— **校准基准**，不是芯片温度，选它 CPU 温度会凭空高十几度；
//   - `PMU tdev1` = -22.17°C —— 无效读数，直接取最大值时会掩盖真实数据；
//   - `gas gauge battery` / `NAND CH0 temp` —— 电池与 SSD 温度，不是 CPU。
func TestChooseCPUTemp(t *testing.T) {
	real := []tempSensor{
		{Name: "PMU tcal", Celsius: 51.82}, {Name: "PMU2 tcal", Celsius: 51.82},
		{Name: "PMU tdev1", Celsius: -22.17}, {Name: "PMU2 tdev1", Celsius: -22.09},
		{Name: "PMU2 tdev3", Celsius: -22.09}, {Name: "PMU tdev2", Celsius: 35.25},
		{Name: "PMU2 tdev4", Celsius: 35.77}, {Name: "PMU tdie6", Celsius: 36.29},
		{Name: "PMU tdie1", Celsius: 35.41}, {Name: "PMU2 tdie4", Celsius: 32.77},
		{Name: "gas gauge battery", Celsius: 30.6}, {Name: "NAND CH0 temp", Celsius: 34.0},
	}

	v, note := chooseCPUTemp(real)
	if v != 36.29 {
		t.Errorf("应选中 tdie 里最高的 36.29（不是 tcal 的 51.82、也不是被 -22 拉偏的值），实际 %.2f", v)
	}
	if note == "" {
		t.Error("应说明来源，便于排障")
	}

	// 只有 tcal 与无效值：必须报"取不到"，而不是把 51.82 当 CPU 温度
	v, note = chooseCPUTemp([]tempSensor{
		{Name: "PMU tcal", Celsius: 51.82}, {Name: "PMU tdev1", Celsius: -22.0},
	})
	if v != 0 {
		t.Errorf("只有校准基准时不该给出 CPU 温度，实际 %.2f", v)
	}
	if note == "" {
		t.Error("取不到时也要有原因说明")
	}

	// 没有 tdie 时退到 PMU 系列（老机型/其它芯片的传感器命名可能不同）
	v, _ = chooseCPUTemp([]tempSensor{{Name: "PMU tdev2", Celsius: 41.5}})
	if v != 41.5 {
		t.Errorf("没有 tdie 时应退到 PMU 系列，实际 %.2f", v)
	}

	// 完全没有传感器
	if v, note := chooseCPUTemp(nil); v != 0 || note == "" {
		t.Errorf("空列表应返回 0 + 原因，实际 %.2f %q", v, note)
	}
}

// TestReadTempIOHIDOnAppleSilicon 是这条路径的集成测试：
// 真的起 python3、真的读 IOHID 传感器。只在 arm64 上跑（Intel 走 powermetrics）。
//
// 它的价值在于：以前这里用 `powermetrics --samplers smc`，在 M 系列上**永远拿不到值**，
// 而界面把 0 显示成"不可读取（需 root）"，看起来像权限问题 —— 其实是方法错了。
func TestReadTempIOHIDOnAppleSilicon(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("仅 Apple Silicon 适用")
	}
	v, note := readTempIOHID(context.Background())
	if v <= 0 || v >= 150 {
		t.Fatalf("Apple Silicon 上应能读到合理温度，实际 %.2f（说明：%s）", v, note)
	}
	t.Logf("IOHID 读到 CPU 温度 %.2f°C（%s）", v, note)
}
