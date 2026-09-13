package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, "test-secret-key", 72, 3, 15), st
}

func TestCreateUserValidatesInput(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()

	if _, err := m.CreateUser(ctx, "a", "longenough", true); err == nil {
		t.Fatal("用户名过短应被拒绝")
	}
	if _, err := m.CreateUser(ctx, "zizdog", "short", true); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("短密码应返回 ErrWeakPassword，实际 %v", err)
	}
	u, err := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true)
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	if !u.IsAdmin || u.Username != "zizdog" {
		t.Fatalf("用户属性异常: %+v", u)
	}
	// 密码必须是 bcrypt 哈希，绝不能是明文
	if u.PasswordHash == "PanelTestPw-9x!" || len(u.PasswordHash) < 50 {
		t.Fatalf("密码未被正确哈希: %q", u.PasswordHash)
	}
	if _, err := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true); !errors.Is(err, ErrUserExists) {
		t.Fatalf("重复用户名应返回 ErrUserExists，实际 %v", err)
	}
}

func TestLoginSuccessAndFailure(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	if _, err := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true); err != nil {
		t.Fatal(err)
	}

	// 错误密码
	if _, err := m.Login(ctx, "zizdog", "wrong-password", "", "127.0.0.1", "test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误密码应返回 ErrInvalidCredentials，实际 %v", err)
	}
	// 不存在的用户：错误信息必须与密码错误一致，避免枚举用户名
	if _, err := m.Login(ctx, "nobody", "whatever", "", "127.0.0.1", "test"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("不存在用户应返回同样的错误，实际 %v", err)
	}

	res, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "", "192.168.1.9", "UA")
	if err != nil {
		t.Fatalf("正确密码登录失败: %v", err)
	}
	if res.Token == "" {
		t.Fatal("登录成功必须返回会话令牌")
	}
	if res.NeedTOTP {
		t.Fatal("未开启 2FA 时不应要求验证码")
	}

	// 令牌必须能通过校验
	u, err := m.AuthSession(ctx, res.Token)
	if err != nil {
		t.Fatalf("会话校验失败: %v", err)
	}
	if u.Username != "zizdog" {
		t.Fatalf("会话对应账号错误: %s", u.Username)
	}
	// 登录来源应被记录
	if u.LastLoginIP != "192.168.1.9" {
		t.Fatalf("未记录登录来源 IP: %s", u.LastLoginIP)
	}
	// 数据库里只能是哈希，绝不能是原始令牌
	var stored string
	if err := m.st.DB().QueryRowContext(ctx, `SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == res.Token {
		t.Fatal("会话令牌不能明文存库")
	}
	if stored != HashToken(res.Token) {
		t.Fatal("会话令牌哈希不匹配")
	}
}

func TestAccountLocksAfterMaxFailures(t *testing.T) {
	m, _ := newTestManager(t) // maxFail = 3
	ctx := context.Background()
	if _, err := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Login(ctx, "zizdog", "bad", "", "1.2.3.4", "t"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败密码应返回凭证错误，实际 %v", i+1, err)
		}
	}
	// 达到阈值后，即使密码正确也应被锁定
	if _, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "", "1.2.3.4", "t"); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("应返回账号已锁定，实际 %v", err)
	}
	// 手动解锁后恢复
	u, _ := m.UserByName(ctx, "zizdog")
	if err := m.Unlock(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "", "1.2.3.4", "t"); err != nil {
		t.Fatalf("解锁后应能登录: %v", err)
	}
}

func TestTOTPLoginFlow(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	u, err := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	code := TOTPCodeAt(secret, time.Now())
	if err := m.EnableTOTP(ctx, u.ID, secret, code); err != nil {
		t.Fatalf("开启 2FA 失败: %v", err)
	}
	// 用错误验证码无法开启
	if err := m.EnableTOTP(ctx, u.ID, secret, "000000"); err == nil {
		t.Fatal("错误验证码不应能开启 2FA")
	}

	// 第一步：只给密码 → 必须要求验证码，且不能下发会话
	res, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "", "127.0.0.1", "t")
	if err != nil {
		t.Fatalf("第一步登录失败: %v", err)
	}
	if !res.NeedTOTP || res.Token != "" {
		t.Fatalf("开启 2FA 后第一步不应下发会话: %+v", res)
	}
	var n int
	if err := m.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("未完成 2FA 时不允许创建会话")
	}

	// 第二步：带验证码 → 成功
	res2, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", TOTPCodeAt(secret, time.Now()), "127.0.0.1", "t")
	if err != nil {
		t.Fatalf("带验证码登录失败: %v", err)
	}
	if res2.Token == "" {
		t.Fatal("两步验证通过后应下发会话")
	}
	// 错误验证码必须被拒绝
	if _, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "000000", "127.0.0.1", "t"); err == nil {
		t.Fatal("错误验证码不应通过")
	}
}

func TestTOTPChallengeCannotBeReplayed(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	u, _ := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true)
	secret, _ := NewTOTPSecret()

	ch := m.IssueTOTPChallenge(u.ID)
	uid, err := m.ParseTOTPChallenge(ch)
	if err != nil {
		t.Fatalf("首次解析 challenge 失败: %v", err)
	}
	if uid != u.ID {
		t.Fatalf("challenge 对应账号错误: %d", uid)
	}
	// 重放必须失败
	if _, err := m.ParseTOTPChallenge(ch); err == nil {
		t.Fatal("同一个 challenge 不允许重复使用")
	}
	// 篡改必须失败
	tampered := ch[:len(ch)-4] + "AAAA"
	if _, err := m.ParseTOTPChallenge(tampered); err == nil {
		t.Fatal("被篡改的 challenge 必须被拒绝")
	}
	if _, err := m.ParseTOTPChallenge("not-base64!!"); err == nil {
		t.Fatal("非法 challenge 必须被拒绝")
	}
	_ = secret
}

func TestSessionRevokedOnPasswordChange(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	u, _ := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true)
	res, err := m.Login(ctx, "zizdog", "PanelTestPw-9x!", "", "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AuthSession(ctx, res.Token); err != nil {
		t.Fatal("会话应有效")
	}
	// 旧密码错误时不允许改
	if err := m.ChangePassword(ctx, u.ID, "wrong", "NewPassword123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧密码错误应被拒绝，实际 %v", err)
	}
	if err := m.ChangePassword(ctx, u.ID, "PanelTestPw-9x!", "NewPassword123"); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}
	// 所有会话必须立即失效
	if _, err := m.AuthSession(ctx, res.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("改密码后旧会话必须失效，实际 %v", err)
	}
	// 新密码可登录
	if _, err := m.Login(ctx, "zizdog", "NewPassword123", "", "127.0.0.1", "t"); err != nil {
		t.Fatalf("新密码应能登录: %v", err)
	}
	// 太短的新密码必须拒绝
	if err := m.SetPassword(ctx, u.ID, "123"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("短密码应被拒绝，实际 %v", err)
	}
}

func TestCannotDeleteLastUser(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	u, _ := m.CreateUser(ctx, "only", "PanelTestPw-9x!", true)
	if err := m.DeleteUser(ctx, u.ID); err == nil {
		t.Fatal("不允许删除最后一个账号")
	}
	if _, err := m.CreateUser(ctx, "second", "PanelTestPw-9x!", false); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("有其他账号时应可删除: %v", err)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	u, _ := m.CreateUser(ctx, "zizdog", "PanelTestPw-9x!", true)
	tok, _, err := m.NewSession(ctx, u.ID, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	// 手工把到期时间改到过去
	if _, err := m.st.DB().ExecContext(ctx,
		`UPDATE sessions SET expires_at=? WHERE token_hash=?`,
		time.Now().Add(-time.Hour).Format("2006-01-02 15:04:05"), HashToken(tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AuthSession(ctx, tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("过期会话必须被拒绝，实际 %v", err)
	}
	// 过期会话应被顺手清理
	var n int
	_ = m.st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE token_hash=?`, HashToken(tok)).Scan(&n)
	if n != 0 {
		t.Fatal("过期会话应被删除")
	}
}
