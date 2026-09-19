package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zizdog/macsaber/internal/execx"
	"github.com/zizdog/macsaber/internal/tool"
)

// ConfirmText 是危险工具要求用户确认的固定文案（前端勾选，后端强校验）。
const ConfirmText = "我已知晓"

// secDequarantine 是**危险工具**打样：去掉文件的 quarantine 扩展属性。
//
// 选它的理由：动作真实、可逆（值会回显，失败时原地恢复），
// 且绝不碰删除/覆盖/改签名这类不可逆操作。
type secDequarantine struct{}

func init() { Add(secDequarantine{}) }

// DangerFloor 是服务端强校验的确认值：缺了直接拒，工具代码不会执行。
func (secDequarantine) DangerFloor() string { return ConfirmText }

// ConfirmOK 额外复核一次：不轻信前端传来的确认字段。
func (secDequarantine) ConfirmOK(in tool.Input) error {
	if in.Str(tool.ConfirmField) != ConfirmText {
		return fmt.Errorf("需要确认「%s」", ConfirmText)
	}
	return nil
}

func (secDequarantine) Meta() tool.Meta {
	_, ok := execx.LookPath("xattr")
	reason := ""
	if !ok {
		reason = "系统缺少 /usr/bin/xattr"
	}
	return tool.Meta{
		ID: "sec.dequarantine", Name: "去除下载隔离", Category: "sec", Icon: "shield",
		Summary:     "去掉文件的 com.apple.quarantine 标记，Gatekeeper 不再拦。",
		Danger:      true,
		DangerFloor: ConfirmText,
		DangerNote:  "危险打样：只动一个扩展属性，原值回显，失败会恢复。",
		Available:   ok, UnavailableReason: reason,
		Params: []tool.Param{
			{Name: "file", Label: "目标文件", Type: tool.TypePath, Required: true,
				Help: "只能读允许的读根内的文件；读写根都要有权限。"},
			{Name: "dry_run", Label: "只看不改", Type: tool.TypeBool, Default: false,
				Help: "勾选后只显示当前隔离标记，不修改。"},
		},
	}
}

func (secDequarantine) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	file := in.Path("file")
	attr := "com.apple.quarantine"

	cur := c.Exec.Run(ctx, 15*time.Second, "xattr", "-p", attr, file)
	hasAttr := cur.ExitCode == 0
	oldVal := strings.TrimSpace(cur.Stdout)
	if !hasAttr && cur.TimedOut {
		return nil, fmt.Errorf("xattr 读取超时被终止")
	}
	if !hasAttr && cur.ExitCode > 1 {
		return nil, fmt.Errorf("xattr 读取失败（退出码 %d）：%s", cur.ExitCode, firstLine(cur.Output()))
	}

	if in.Bool("dry_run", false) {
		msg := "该文件没有下载隔离标记"
		if hasAttr {
			msg = "该文件当前隔离标记：" + oldVal
		}
		return &tool.Result{OK: true, Msg: msg, Data: map[string]any{
			"file": file, "quarantine": oldVal, "present": hasAttr, "changed": false,
		}}, nil
	}
	if !hasAttr {
		return &tool.Result{OK: true, Msg: "该文件没有下载隔离标记，无需处理", Data: map[string]any{
			"file": file, "present": false, "changed": false,
		}}, nil
	}

	del := c.Exec.Run(ctx, 15*time.Second, "xattr", "-d", attr, file)
	if del.ExitCode != 0 {
		return nil, fmt.Errorf("去除失败（退出码 %d）：%s", del.ExitCode, firstLine(del.Output()))
	}
	// 复核：删除命令退出码 0 不等于属性真的没了（坑 F2）。
	again := c.Exec.Run(ctx, 15*time.Second, "xattr", "-p", attr, file)
	if again.ExitCode == 0 {
		// 没删掉就恢复原值，别留一个"半改"的文件。
		_ = c.Exec.Run(ctx, 15*time.Second, "xattr", "-w", attr, oldVal, file)
		return nil, fmt.Errorf("去除后属性仍然存在，已恢复原值：%s", strings.TrimSpace(again.Stdout))
	}
	return &tool.Result{OK: true, Msg: "已去除下载隔离标记", Data: map[string]any{
		"file": file, "previous": oldVal, "present": false, "changed": true,
		"note": "原值已回显，必要时可用 xattr -w 写回",
	}}, nil
}
