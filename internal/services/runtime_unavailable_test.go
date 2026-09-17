package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ============================================================================
//  "运行时不可用"标记
//
//  为什么需要：用户把 Docker/Colima 全删掉后，卸载 compose 记录只会报一句
//  "未找到 docker compose 命令"。有了这个标记，HTTP 层才能补上
//  "没有停止任何容器、可以只删记录"（见 web 的 uninstallFailure）。
//  标记必须**不改写原有文案**，否则用户已经认识的报错会变样。
// ============================================================================

func TestRuntimeUnavailableMarkerPreservesMessage(t *testing.T) {
	plain := errors.New("未找到 docker compose 命令")
	marked := MarkRuntimeUnavailable(plain)

	if !IsRuntimeUnavailable(marked) {
		t.Error("打了标记的错误必须被 IsRuntimeUnavailable 认出")
	}
	if marked.Error() != plain.Error() {
		t.Errorf("标记不许改写文案：got %q want %q", marked.Error(), plain.Error())
	}
	// 被其它错误包装后仍要认得出（Uninstall/driver 链路会再包一层）
	if !IsRuntimeUnavailable(fmt.Errorf("卸载失败: %w", marked)) {
		t.Error("标记必须能穿透 fmt.Errorf(%%w) 包装")
	}
	if IsRuntimeUnavailable(plain) {
		t.Error("没打标记的普通错误不该被当成运行时不可用")
	}
	if MarkRuntimeUnavailable(nil) != nil {
		t.Error("nil 必须原样返回 nil")
	}
}

// TestDriverForWithoutDockerIsMarkedUnavailable 没有 Docker socket 时，
// compose/docker 驱动的构造失败必须带上标记（文案不变）。
func TestDriverForWithoutDockerIsMarkedUnavailable(t *testing.T) {
	m := NewManager(nil, Options{}) // DriverFor 不用 repo，传 nil 即可
	for _, kind := range []Kind{KindCompose, KindDocker} {
		_, err := m.DriverFor(&Service{Name: "x", Kind: kind})
		if err == nil {
			t.Fatalf("%s：没有运行时却构造出了驱动", kind)
		}
		if !IsRuntimeUnavailable(err) {
			t.Errorf("%s：没有运行时必须带 RuntimeUnavailable 标记，实际 %v", kind, err)
		}
		if !strings.Contains(err.Error(), "Docker 不可用") {
			t.Errorf("%s：原有文案不能被改写，实际 %q", kind, err.Error())
		}
	}
}

// TestComposeUninstallWithoutComposeCommandIsMarkedUnavailable 真机现场：
// compose 命令缺失时 `compose down` 根本没执行，错误必须带标记，
// 而且绝不能"假装卸载成功"。
func TestComposeUninstallWithoutComposeCommandIsMarkedUnavailable(t *testing.T) {
	prev := composeBinFn
	composeBinFn = func() (string, []string, bool) { return "", nil, false }
	t.Cleanup(func() { composeBinFn = prev })

	d := &composeDriver{opt: Options{}, svc: &Service{
		Name: "stirling-pdf", Kind: KindCompose, ComposeFile: "/nonexistent/docker-compose.yml",
	}}
	err := d.Uninstall(context.Background())
	if err == nil {
		t.Fatal("没有 compose 命令时必须报错，不能谎报卸载成功")
	}
	if !IsRuntimeUnavailable(err) {
		t.Errorf("compose 命令缺失必须带 RuntimeUnavailable 标记，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "未找到 docker compose 命令") {
		t.Errorf("原有文案不能被改写，实际 %q", err.Error())
	}
}
