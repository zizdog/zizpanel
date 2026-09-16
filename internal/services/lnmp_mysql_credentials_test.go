package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/mysql"
	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  MySQL root 凭据闭环（安装时限时询问 root 口令）
//
//  这一组测试锁住 2026-09-16 mini 事故的反面：
//    · 装完 MySQL 之后面板**一定**知道 root 口令（写回配置）；
//    · 已经有可用 MySQL 时**不再询问**；
//    · 数据目录不是我们初始化的就**不动**用户的口令；
//    · 口令**绝不**出现在任务步骤/日志里（用假 admin 记录所有交互来断言）。
//
//  全部用注入的假实现，不连真实 MySQL、不等真实时间。
// ============================================================================

// fakeMySQLAdmin 是可注入的假 MySQL 交互层。
type fakeMySQLAdmin struct {
	// pingOK 按口令判断连通性：返回 nil 表示能连上。
	// 刻意用"当前 serverPassword"来建模真实服务器：只有口令对得上才连得上。
	serverPassword string
	// authMismatch 为 true 时模拟"服务器要口令但面板手里没有/不对"：
	// 任何口令都返回 1045。
	authMismatch bool
	// unreachable 为 true 时模拟服务没起来（连接错误，不是认证错误）。
	unreachable bool

	setCalls  []string
	pingCalls []string
	setErr    error
	pingErr   error
}

func (f *fakeMySQLAdmin) Ping(ctx context.Context, password string) error {
	f.pingCalls = append(f.pingCalls, password)
	if f.pingErr != nil {
		return f.pingErr
	}
	if f.unreachable {
		return errors.New("无法连接 MySQL：请确认数据库服务正在运行")
	}
	if f.authMismatch {
		return fmt.Errorf("%w：面板持有的口令被服务器拒绝。原始信息：ERROR 1045", mysql.ErrAuth)
	}
	if password != f.serverPassword {
		return fmt.Errorf("%w：面板持有的口令被服务器拒绝。原始信息：ERROR 1045", mysql.ErrAuth)
	}
	return nil
}

func (f *fakeMySQLAdmin) SetRootPassword(ctx context.Context, newPassword string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, newPassword)
	f.serverPassword = newPassword
	return nil
}

// fakeInput 是可注入的假输入通道。
type fakeInput struct {
	value    string
	provided bool
	asked    []tasks.InputRequest
	timeout  time.Duration
}

func (f *fakeInput) WaitInput(ctx context.Context, req tasks.InputRequest, timeout time.Duration) (string, bool) {
	f.asked = append(f.asked, req)
	f.timeout = timeout
	return f.value, f.provided
}

// credManager 造一个沙箱化的 Manager（带注入的假 admin）。
func credManager(t *testing.T, admin *fakeMySQLAdmin, cred MySQLCredential, setFn func(string) error) *Manager {
	t.Helper()
	m := NewManager(nil, Options{
		UserHome:             t.TempDir(),
		UserName:             "zizdog",
		MySQLCredential:      func() MySQLCredential { return cred },
		SetMySQLRootPassword: setFn,
		MySQLInputTimeout:    5 * time.Second,
	})
	m.mysqlAdminOverride = admin
	// 单测不许真的等 30 秒等 MySQL 起来。
	m.mysqlProbeWaitOverride = 10 * time.Millisecond
	return m
}

// assertNoSecret 断言步骤与告警里都没有口令。
func assertNoSecret(t *testing.T, res *InstallResult, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	for _, s := range res.Steps {
		if strings.Contains(s, secret) {
			t.Errorf("任务步骤里出现了口令：%s", s)
		}
	}
	if strings.Contains(res.Warning, secret) {
		t.Errorf("任务告警里出现了口令：%s", res.Warning)
	}
}

// TestEnsureMySQLRootCredentialGeneratesAndRecords 是主路径：
// 本次初始化的 MySQL（--initialize-insecure ⇒ root 空口令）+ 没有用户输入
// ⇒ 自动生成强随机口令、写进 MySQL、写回面板配置、放进一次性凭据区块。
func TestEnsureMySQLRootCredentialGeneratesAndRecords(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: ""}
	var saved []string
	m := credManager(t, admin, MySQLCredential{User: "root", Socket: "/tmp/mysql.sock"}, func(pw string) error {
		saved = append(saved, pw)
		return nil
	})
	res := &InstallResult{mysqlFreshInit: true}
	// 没有输入通道（无人值守）⇒ 直接自动生成
	if err := m.ensureMySQLRootCredential(context.Background(), res); err != nil {
		t.Fatalf("凭据闭环不该失败: %v", err)
	}
	if len(admin.setCalls) != 1 {
		t.Fatalf("应恰好设置一次 root 口令，实际 %d 次", len(admin.setCalls))
	}
	pw := admin.setCalls[0]
	if len(pw) < 24 {
		t.Errorf("自动生成的口令应 >=24 位（要求），实际 %d 位: %q", len(pw), pw)
	}
	if len(saved) != 1 || saved[0] != pw {
		t.Fatalf("口令必须原样写回面板配置，实际 saved=%v pw=%q", saved, pw)
	}
	// 一次性凭据区块（沿用面板既有做法）
	if len(res.Credentials) != 1 || res.Credentials[0].Value != pw || res.Credentials[0].Key != "mysql_root_password" {
		t.Fatalf("应给出一次性凭据区块，实际 %+v", res.Credentials)
	}
	if strings.Contains(res.Credentials[0].Label, pw) {
		t.Error("凭据区块的说明文字里不该重复口令")
	}
	// 最要命的一条：步骤里不能有口令
	assertNoSecret(t, res, pw)
	joined := strings.Join(res.Steps, " | ")
	if !strings.Contains(joined, "自动生成") {
		t.Errorf("应说明口令是自动生成的：%s", joined)
	}
}

// TestEnsureMySQLRootCredentialUsesUserInput 用户输入优先于自动生成。
func TestEnsureMySQLRootCredentialUsesUserInput(t *testing.T) {
	const chosen = "MyOwn-RootPw-2026"
	admin := &fakeMySQLAdmin{serverPassword: ""}
	var saved string
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(pw string) error { saved = pw; return nil })
	in := &fakeInput{value: chosen, provided: true}
	ctx := WithInput(context.Background(), in)

	res := &InstallResult{mysqlFreshInit: true}
	if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
		t.Fatalf("凭据闭环不该失败: %v", err)
	}
	if len(admin.setCalls) != 1 || admin.setCalls[0] != chosen {
		t.Fatalf("应使用用户输入的口令，实际 %v", admin.setCalls)
	}
	if saved != chosen {
		t.Fatalf("用户输入的口令必须写回配置，实际 %q", saved)
	}
	// 询问内容必须告诉用户"留空/超时＝自动生成"
	if len(in.asked) != 1 {
		t.Fatalf("应恰好问一次，实际 %d 次", len(in.asked))
	}
	if in.asked[0].Key != "mysql_root_password" || !in.asked[0].Secret {
		t.Errorf("询问的结构不对（前端要用它渲染密码框）: %+v", in.asked[0])
	}
	if !strings.Contains(in.asked[0].Hint, "自动生成") {
		t.Errorf("提示里应说明留空＝自动生成: %q", in.asked[0].Hint)
	}
	assertNoSecret(t, res, chosen)
}

// TestEnsureMySQLRootCredentialEmptySubmitGenerates 用户明确留空 ⇒ 自动生成。
func TestEnsureMySQLRootCredentialEmptySubmitGenerates(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t"} {
		admin := &fakeMySQLAdmin{serverPassword: ""}
		var saved string
		m := credManager(t, admin, MySQLCredential{User: "root"}, func(pw string) error { saved = pw; return nil })
		ctx := WithInput(context.Background(), &fakeInput{value: blank, provided: true})

		res := &InstallResult{mysqlFreshInit: true}
		if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
			t.Fatalf("凭据闭环不该失败: %v", err)
		}
		if len(admin.setCalls) != 1 || len(admin.setCalls[0]) < 24 {
			t.Fatalf("留空（%q）时应自动生成强口令，实际 %v", blank, admin.setCalls)
		}
		if saved != admin.setCalls[0] {
			t.Error("自动生成的口令必须写回配置")
		}
		if !strings.Contains(strings.Join(res.Steps, " "), "留空") {
			t.Errorf("应说明用户选择了留空：%v", res.Steps)
		}
	}
}

// TestEnsureMySQLRootCredentialKeepsUserSpaces 用户口令里的空格必须原样保留：
// 悄悄 trim 会让"照着自己输入的连不上"。
func TestEnsureMySQLRootCredentialKeepsUserSpaces(t *testing.T) {
	const chosen = "  spaced pw 2026  "
	admin := &fakeMySQLAdmin{serverPassword: ""}
	var saved string
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(pw string) error { saved = pw; return nil })
	ctx := WithInput(context.Background(), &fakeInput{value: chosen, provided: true})
	res := &InstallResult{mysqlFreshInit: true}
	if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
		t.Fatalf("凭据闭环不该失败: %v", err)
	}
	if len(admin.setCalls) != 1 || admin.setCalls[0] != chosen {
		t.Fatalf("用户输入的口令必须原样使用，实际 %q", admin.setCalls)
	}
	if saved != chosen {
		t.Fatalf("写回配置的也应是原样的口令，实际 %q", saved)
	}
}

// TestEnsureMySQLRootCredentialTimeoutGenerates 超时 ⇒ 自动生成并继续（不是失败）。
func TestEnsureMySQLRootCredentialTimeoutGenerates(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: ""}
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(string) error { return nil })
	ctx := WithInput(context.Background(), &fakeInput{provided: false})

	res := &InstallResult{mysqlFreshInit: true}
	if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
		t.Fatalf("超时必须自动继续，不该失败: %v", err)
	}
	if len(admin.setCalls) != 1 {
		t.Fatalf("超时后也要把口令设上，实际 %v", admin.setCalls)
	}
	if !strings.Contains(strings.Join(res.Steps, " "), "超时") {
		t.Errorf("应如实说明是超时后自动生成的：%v", res.Steps)
	}
}

// TestEnsureMySQLRootCredentialSkipsWhenAlreadyUsable 已有可用 MySQL ⇒ 不问、不改。
func TestEnsureMySQLRootCredentialSkipsWhenAlreadyUsable(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: "existing-pw-123"}
	m := credManager(t, admin, MySQLCredential{User: "root", Password: "existing-pw-123"}, func(string) error {
		t.Error("已有可用凭据时不该写配置")
		return nil
	})
	in := &fakeInput{value: "ignored", provided: true}
	ctx := WithInput(context.Background(), in)

	res := &InstallResult{}
	if err := m.ensureMySQLRootCredential(ctx, res); err != nil {
		t.Fatalf("自检通过时不该失败: %v", err)
	}
	if len(admin.setCalls) != 0 {
		t.Fatalf("不该改已有口令，实际 %v", admin.setCalls)
	}
	if len(in.asked) != 0 {
		t.Fatal("机器上已有可用 MySQL 时**不要再问密码**")
	}
	if !strings.Contains(strings.Join(res.Steps, " "), "自检通过") {
		t.Errorf("应如实说明自检通过：%v", res.Steps)
	}
}

// TestEnsureMySQLRootCredentialKeepsHandsOffExistingDatadir：
// 数据目录不是本次初始化的（可能是用户的机器），即使 root 空口令也不能替用户改。
func TestEnsureMySQLRootCredentialKeepsHandsOffExistingDatadir(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: ""}
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(string) error {
		t.Error("不该写配置")
		return nil
	})
	res := &InstallResult{mysqlFreshInit: false}
	if err := m.ensureMySQLRootCredential(context.Background(), res); err != nil {
		t.Fatalf("这种情况只该告警，不该让任务失败: %v", err)
	}
	if len(admin.setCalls) != 0 {
		t.Fatalf("不该动已有数据目录上的 root 口令，实际 %v", admin.setCalls)
	}
	if !strings.Contains(res.Warning, "没有设置口令") {
		t.Errorf("应如实告警（空口令＝任何本机程序都能读全部库）：%q", res.Warning)
	}
}

// TestEnsureMySQLRootCredentialFailsOnAuthMismatch：
// 服务器要口令而面板手里的不对/为空 ⇒ **如实失败**并给出可操作提示，
// 而不是让用户之后在页面上撞 1045。
func TestEnsureMySQLRootCredentialFailsOnAuthMismatch(t *testing.T) {
	admin := &fakeMySQLAdmin{authMismatch: true}
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(string) error {
		t.Error("认证都不通过时不该写配置")
		return nil
	})
	res := &InstallResult{}
	err := m.ensureMySQLRootCredential(context.Background(), res)
	if err == nil {
		t.Fatal("凭据不一致必须如实失败")
	}
	if len(admin.setCalls) != 0 {
		t.Fatalf("不该在认证失败的情况下改口令，实际 %v", admin.setCalls)
	}
	for _, want := range []string{"不一致", "skip-grant-tables"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q：%v", want, err)
		}
	}
	if !strings.Contains(res.Warning, "不一致") {
		t.Errorf("告警里也要有：%q", res.Warning)
	}
}

// TestEnsureMySQLRootCredentialFailsWhenUnreachable 服务没起来 ⇒ 如实失败（带排查入口）。
func TestEnsureMySQLRootCredentialFailsWhenUnreachable(t *testing.T) {
	admin := &fakeMySQLAdmin{unreachable: true}
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(string) error { return nil })
	res := &InstallResult{}
	err := m.ensureMySQLRootCredential(context.Background(), res)
	if err == nil {
		t.Fatal("连不上 MySQL 必须如实失败")
	}
	if !strings.Contains(err.Error(), "服务管理") {
		t.Errorf("错误信息应给出排查入口：%v", err)
	}
	if len(admin.pingCalls) < 2 {
		t.Error("刚注册完的服务需要重试（不能一次判定就放弃）")
	}
}

// TestEnsureMySQLRootCredentialConfigWriteFailureKeepsCredential：
// 写盘失败是最危险的状态（MySQL 已改、面板没记住），
// 此时必须失败 **并且** 把口令留在一次性凭据区块里 —— 那是唯一还能救回来的地方。
func TestEnsureMySQLRootCredentialConfigWriteFailureKeepsCredential(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: ""}
	m := credManager(t, admin, MySQLCredential{User: "root"}, func(string) error {
		return errors.New("磁盘只读")
	})
	res := &InstallResult{mysqlFreshInit: true}
	err := m.ensureMySQLRootCredential(context.Background(), res)
	if err == nil {
		t.Fatal("写回配置失败必须如实失败")
	}
	if len(res.Credentials) != 1 {
		t.Fatalf("写配置失败时口令必须仍在一次性凭据区块里，实际 %+v", res.Credentials)
	}
	pw := res.Credentials[0].Value
	if admin.serverPassword != pw {
		t.Error("凭据区块里的口令应与真正设进 MySQL 的一致")
	}
	assertNoSecret(t, res, pw)
	if !strings.Contains(res.Warning, "凭据") {
		t.Errorf("告警应指引用户去复制一次性凭据：%q", res.Warning)
	}
}

// TestEnsureMySQLRootCredentialWithoutHooksIsHonest 未接入面板配置时要如实说明跳过。
func TestEnsureMySQLRootCredentialWithoutHooksIsHonest(t *testing.T) {
	admin := &fakeMySQLAdmin{serverPassword: ""}
	m := NewManager(nil, Options{UserHome: t.TempDir()})
	m.mysqlAdminOverride = admin
	res := &InstallResult{mysqlFreshInit: true}
	if err := m.ensureMySQLRootCredential(context.Background(), res); err != nil {
		t.Fatalf("未接入时不该失败（只是跳过）: %v", err)
	}
	if !strings.Contains(strings.Join(res.Steps, " "), "跳过") {
		t.Errorf("应如实说明跳过了凭据闭环：%v", res.Steps)
	}
	if len(admin.setCalls) != 0 {
		t.Error("未接入配置时不该去改 MySQL 口令")
	}
}

// TestRandomMySQLPasswordIsStrongAndSafe 口令的字符集必须"零转义风险"。
func TestRandomMySQLPasswordIsStrongAndSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		pw, err := randomMySQLPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) < 24 {
			t.Fatalf("口令太短（要求 >=24）：%q", pw)
		}
		for _, r := range pw {
			isUpper := r >= 'A' && r <= 'Z'
			isDigit := r >= '2' && r <= '7'
			if !isUpper && !isDigit {
				t.Fatalf("口令含需要转义或易混的字符 %q: %q", r, pw)
			}
		}
		if seen[pw] {
			t.Fatalf("生成了重复的口令: %q", pw)
		}
		seen[pw] = true
	}
}

// TestIsMySQLFormulaCoversBothPaths 两条安装路径都要走凭据闭环。
func TestIsMySQLFormulaCoversBothPaths(t *testing.T) {
	for _, f := range []string{"mysql@8.4", "mysql"} {
		if !isMySQLFormula(f) {
			t.Errorf("%s 应被识别为 MySQL", f)
		}
	}
	for _, f := range []string{"mysql@8.0", "mariadb", "nginx", ""} {
		if isMySQLFormula(f) {
			t.Errorf("%s 不该被识别为 MySQL", f)
		}
	}
}
