package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestInstallAdviceMatchesRealFailures 每个高频失败都要给出**能照着做**的一句建议。
func TestInstallAdviceMatchesRealFailures(t *testing.T) {
	app := App{ID: "demo", Name: "演示应用", Port: 8899}
	cases := []struct {
		name string
		err  string
		want string // 建议里必须出现的关键词（空 = 必须不给建议）
	}{
		{"磁盘满", "Error: No space left on device @ rb_sysopen - /opt/homebrew/x", "磁盘"},
		{"端口被占", "listen tcp 0.0.0.0:8899: bind: address already in use", "8899"},
		{"formula 不存在", "Error: No available formula with the name \"n8n\".", "Homebrew"},
		{"隐私保护", "open /Users/x/Downloads/a: operation not permitted", "隐私"},
		{"架构不对", "bad CPU type in executable", "arm64"},
		{"launchd 拒绝", "Bootstrap failed: 5: Input/output error", "launchd"},
		{"已有任务", "已有一个安装任务在进行中，请等待它结束", "任务中心"},
		{"无关错误不给建议", "checksum mismatch for foo.tar.gz", ""},
		{"空错误", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if c.err != "" {
				err = errors.New(c.err)
			}
			got := InstallAdvice(app, err)
			if c.want == "" {
				if got != "" {
					t.Errorf("不该给建议，实际 %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("建议里应当提到 %q，实际 %q", c.want, got)
			}
		})
	}
}

// TestAppendInstallAdviceKeepsOriginalAndIsIdempotent 原文不许丢，重复追加不许叠加。
func TestAppendInstallAdviceKeepsOriginalAndIsIdempotent(t *testing.T) {
	app := App{ID: "demo", Name: "演示", Port: 1}
	raw := errors.New("Error: No space left on device")
	once := AppendInstallAdvice(app, raw)
	if !strings.Contains(once.Error(), "No space left on device") {
		t.Error("原始报错必须保留（用户/作者要靠它排查）")
	}
	if !strings.Contains(once.Error(), installAdviceMarker) {
		t.Error("应当追加【下一步】")
	}
	twice := AppendInstallAdvice(app, once)
	if twice.Error() != once.Error() {
		t.Errorf("重复追加应当幂等：\n%s\n---\n%s", once.Error(), twice.Error())
	}
	// 不匹配的错误原样返回
	plain := errors.New("some unrelated failure")
	if got := AppendInstallAdvice(app, plain); got != plain {
		t.Errorf("不匹配时应当原样返回，实际 %v", got)
	}
	if AppendInstallAdvice(app, nil) != nil {
		t.Error("nil 应当原样返回")
	}
}

// TestAppendInstallAdviceForUsesCatalog 按 ID 查目录：找得到才加，找不到不动。
func TestAppendInstallAdviceForUsesCatalog(t *testing.T) {
	err := errors.New("bind: address already in use")
	got := AppendInstallAdviceFor("grafana", err)
	if !strings.Contains(got.Error(), "3002") {
		t.Errorf("应当带上 grafana 的真实端口 3002：%v", got)
	}
	// 目录里没有的 ID（例如一键 LNMP 的 "lnmp"）：原样返回，不猜
	plain := AppendInstallAdviceFor("lnmp", err)
	if plain != err {
		t.Errorf("找不到 ID 时应当原样返回，实际 %v", plain)
	}
	_ = fmt.Sprintf
}
