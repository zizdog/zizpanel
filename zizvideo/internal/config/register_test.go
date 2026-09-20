package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllowRegisterDefaultsOff 门禁：默认值=关。
func TestAllowRegisterDefaultsOff(t *testing.T) {
	if Default().AllowRegister {
		t.Fatal("allow_register 默认必须为关")
	}
}

// TestRegisterSwitchPersistsAndReadsBack 门禁：开关写回并回读生效值。
func TestRegisterSwitchPersistsAndReadsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{\"listen\":\"127.0.0.1:1\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sw := NewRegisterSwitch(path, Default().AllowRegister)
	if sw.On() {
		t.Fatal("初始应为关")
	}
	if err := sw.Set(true); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !sw.On() {
		t.Fatal("回读一致后才应生效")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"allow_register": true`) {
		t.Fatalf("配置未写回: %s", raw)
	}
	if !strings.Contains(string(raw), `"listen"`) {
		t.Fatalf("写回不得丢掉其它键: %s", raw)
	}
	if v, err := RegisterFromFile(path); err != nil || !v {
		t.Fatalf("RegisterFromFile = %v, %v", v, err)
	}
	st := sw.State()
	if !st.Verified || !st.AllowRegister || st.Source != "config" {
		t.Fatalf("State 应为已复核的 config 值: %+v", st)
	}

	// 手工把文件改回 false（进程内不变）：必须标未复核，不猜。
	if err := os.WriteFile(path, []byte("{\"allow_register\":false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := sw.State(); st.Verified || st.FileValue == nil || *st.FileValue {
		t.Fatalf("文件与生效值不一致时必须标未复核: %+v", st)
	}
}

// TestRegisterSwitchRefusesWhenNoFile 门禁：没有配置文件时如实拒绝，不谎报成功。
func TestRegisterSwitchRefusesWhenNoFile(t *testing.T) {
	sw := NewRegisterSwitch("", false)
	if err := sw.Set(true); err == nil {
		t.Fatal("没有配置文件时必须拒绝写入")
	}
	if st := sw.State(); st.Verified || st.Source != "default" {
		t.Fatalf("读不到就必须标未复核: %+v", st)
	}
}

// TestRegisterSwitchEnvOverride 门禁：ZV_ALLOW_REGISTER 覆盖时文件改动不生效，
// 必须拒绝写入而不是谎报成功。
func TestRegisterSwitchEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZV_ALLOW_REGISTER", "1")
	sw := NewRegisterSwitch(path, false)
	if !sw.On() {
		t.Fatal("env 覆盖应成为生效值")
	}
	if err := sw.Set(false); err == nil {
		t.Fatal("env 覆盖时必须拒绝写文件")
	}
	if st := sw.State(); !st.EnvOverride || !st.Verified {
		t.Fatalf("env 覆盖应为已复核的 env 值: %+v", st)
	}
}
