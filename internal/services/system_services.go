package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "embed"
)

// ============================================================================
//  服务注册脚本（tools/system-services.sh）必须随二进制走
//
//  为什么内置（2026-09-17 真机根因，mini）：
//    install.sh 会把 tools/system-services.sh 拷到 /opt/zizpanel/，但
//    **面板自己的在线升级只换二进制，明确跳过 tools/**（internal/upgrade/stage.go
//    里注释写明"不关心的文件（install.sh、tools/ 等）直接跳过"）。
//    于是"装完面板之后又在线升级过"的机器上，这个脚本根本不存在 ——
//    而一键 LNMP 的最后一步正是调它把 nginx/PHP/MySQL 注册成系统级
//    LaunchDaemon。症状与用户反馈完全吻合：brew 装好了、nginx.conf 也改了，
//    最后卡在一句看不懂的"找不到 system-services.sh，无法注册系统级服务"，
//    服务管理里一个都没有（registerLNMPComponents 在那一步之后，永远走不到）。
//
//  所以：脚本内容嵌进二进制。安装方式怎么变、升级多少次，都不会再缺这个文件。
//
//  单一事实来源仍是 tools/system-services.sh（install.sh 也要发它）；
//  assets/ 下这份是构建期快照，由 TestEmbeddedSystemServicesScriptMatchesRepoCopy
//  在漂移时报错 —— 两份不一致时以内置为准（见下）。
// ============================================================================

//go:embed assets/system-services.sh
var embeddedSystemServices string

// materializeSystemServicesScript 把内置脚本落盘，返回 (脚本路径, 是否为临时文件)。
//
// 落盘策略（按顺序）：
//  1. 安装根目录（<WorkDir>/..，即 /opt/zizpanel）—— 便于用户查看与手工重跑；
//     内容与内置一致就直接复用（幂等，不反复写盘）；
//  2. 临时文件 —— 安装目录只读/权限不足时用，保证功能不因此失效。
//
// 覆盖已有副本是**刻意**的：旧副本可能还在用 "php-fpm 必须监听 9000" 做验证，
// 而我们现在的设计是每个版本专属 Unix socket —— 那种旧脚本会在一切正常时
// 以非 0 退出，把已经装好的机器报成失败（"谎报失败"同样不可接受）。
func (m *Manager) materializeSystemServicesScript(ctx context.Context, result *InstallResult) (string, bool, error) {
	if strings.TrimSpace(embeddedSystemServices) == "" {
		return "", false, errors.New("内置的服务注册脚本为空（构建异常），无法注册系统级服务")
	}

	if root := m.installRoot(); root != "" {
		dst := filepath.Join(root, "system-services.sh")
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == embeddedSystemServices {
			return dst, false, nil // 已经是最新的一份，不写盘
		}
		if err := writeExecutableFile(dst, embeddedSystemServices); err == nil {
			// 归属真实用户：安装脚本本来就是给用户看/手工重跑的
			if m.opt.UserName != "" {
				_ = chownTo(m.opt.UserName, dst)
			}
			result.step(ctx, "已把内置的服务注册脚本同步到 "+dst)
			return dst, false, nil
		} else {
			result.step(ctx, "无法写入 "+dst+"（"+err.Error()+"），改用临时脚本")
		}
	}

	f, err := os.CreateTemp("", "zizpanel-system-services-*.sh")
	if err != nil {
		return "", false, fmt.Errorf("无法写出服务注册脚本（安装目录与临时目录都不可写）: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(embeddedSystemServices); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", false, fmt.Errorf("写入临时服务注册脚本失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("关闭临时服务注册脚本失败: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("设置临时服务注册脚本权限失败: %w", err)
	}
	// 临时文件用完就删（由调用方 defer）：它不是用户要保留的东西，
	// 留在 /tmp 只会越堆越多。返回值里的 true 就是在告诉调用方这一点。
	result.step(ctx, "已把内置的服务注册脚本写到临时路径 "+path)
	return path, true, nil
}

// installRoot 返回面板安装根目录（<WorkDir>/..）。
//
// 与 installSystemDaemons 原来的推导一致：WorkDir 是 <root>/work。
// 取不到时返回空串，调用方退到临时目录，不猜。
func (m *Manager) installRoot() string {
	w := strings.TrimSpace(m.opt.WorkDir)
	if w == "" {
		return ""
	}
	return filepath.Dir(filepath.Clean(w))
}

// writeExecutableFile 原子写一个 0755 的可执行脚本。
//
// 原子替换的原因与配置文件相同：写到一半的 shell 脚本被执行会是以
// "语法错误"告终的假失败，排查成本极高。
func writeExecutableFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".zp-syssvc-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
