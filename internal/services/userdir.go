package services

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ensureUserWritableDir 确保目录存在、归属运行用户、且**以运行用户身份实测可写**。
//
// 为什么必须实测（坑 226）：transmission 那次"加了种子没反应"，根因之一是
// 面板以 root 建了目录、root 写没问题，而以真实用户运行的守护进程写不进去；
// 应用自己还把失败报成 success。凡是要往里落文件的应用（下载器、媒体库），
// 都必须在这里当场失败，而不是让用户面对"服务是绿的、就是不动"。
//
// runAs 与 remedy 只影响错误话术：不同应用的出路不同（transmission 劝退手改
// settings.json；aria2 指向它自己的配置项）。
func (m *Manager) ensureUserWritableDir(ctx context.Context, dir, what, runAs, remedy string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("%s为空：请在面板里指定一个绝对路径", what)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s必须是绝对路径（收到 %q）", what, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建%s %s 失败: %w；%s", what, dir, err, remedy)
	}
	if m.opt.UserName != "" {
		if err := chownTo(m.opt.UserName, dir); err != nil {
			return fmt.Errorf("把%s %s 归属改为 %s 失败: %w", what, dir, m.opt.UserName, err)
		}
	}
	probe := filepath.Join(dir, ".zizpanel-write-test")
	if err := transmissionWriteProbe(m, ctx, probe); err != nil {
		return fmt.Errorf("%s %s 不可写（%s）：%v。%s", what, dir, runAs, err, remedy)
	}
	return nil
}
