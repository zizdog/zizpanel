// installphase.go —— 安装期写下的「面板权限」状态标记（仪表盘常驻提醒的补充说明）。
// 安装脚本写 <DataDir>/install-permissions.json（是否远程安装/有没有当场申请过）；
// 面板只读它选文案 —— "要不要提醒"按权限页真实状态判，授权成功后横幅必须消失。
package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const installPhaseMarkerName = "install-permissions.json"

// InstallPhase 是安装期标记的内容（字段名与 install.sh 写出的 JSON 一致）。
type InstallPhase struct {
	At                 time.Time `json:"at"`
	RemoteInstall      bool      `json:"remote_install"`
	FullDiskRequested  bool      `json:"full_disk_requested"`
	RemovableRequested bool      `json:"removable_requested"`
}

// installPhasePath 拼 DataDir 下的一个标记文件路径（路径片段不进日志）。
func installPhasePath(dataDir, name string) string {
	return filepath.Join(filepath.Clean(dataDir), name)
}

// InstallPhaseMarkerPath 返回安装期标记的完整路径（DataDir 下，只有 root 能写）。
func InstallPhaseMarkerPath(dataDir string) string {
	return installPhasePath(dataDir, installPhaseMarkerName)
}

// ReadInstallPhase 读安装期标记；文件不存在或坏掉都返回 false（**绝不猜**）。
func ReadInstallPhase(dataDir string) (InstallPhase, bool) {
	raw, err := os.ReadFile(InstallPhaseMarkerPath(dataDir))
	if err != nil {
		return InstallPhase{}, false
	}
	var p InstallPhase
	if err := json.Unmarshal(raw, &p); err != nil {
		return InstallPhase{}, false
	}
	return p, true
}

// WriteInstallPhase 写安装期标记（安装脚本写它；门禁也用它造出"安装过"的世界）。
func WriteInstallPhase(dataDir string, p InstallPhase) error {
	if p.At.IsZero() {
		p.At = time.Now()
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Clean(dataDir), 0o700); err != nil {
		return err
	}
	tmp := InstallPhaseMarkerPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, InstallPhaseMarkerPath(dataDir))
}
