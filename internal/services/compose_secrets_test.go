package services

// compose_secrets_test.go —— compose 应用「安装时随机密钥」的回归锁。
//
// 这一段能力最危险的一条是**幂等**：Immich 的 DB_PASSWORD 如果在重装/升级时
// 被重新生成，数据库立刻连不上（数据还在、口令变了）。所以这里用单测直接钉住
// prepareComposeProject 这一半（不碰 docker 的那一半）：
//   ① 首次安装：生成随机值 → 写 .env（0600）→ 进安装结果的 Credentials；
//   ② 重装：先读已有 .env，有值一律复用 —— .env 一字未动、结果里也是旧值；
//   ③ 脱敏：明文只允许出现在 Credentials，**绝不许**出现在 Steps/日志字符串里。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secretTestApp 造一个带三类密钥的合成 compose 应用（不联网、不碰真实目录）。
func secretTestApp() App {
	return App{
		ID:   "secret-app",
		Name: "Secret App",
		Kind: KindCompose,
		ComposeYAML: `services:
  app:
    image: example/app:1
    container_name: secret-app
    environment:
      TOKEN: ${TEST_TOKEN}
      PW: ${TEST_PASSWORD}
      B64: ${TEST_B64}
`,
		ComposeSecrets: []ComposeSecret{
			{Env: "TEST_TOKEN", Encoding: SecretHex, Bytes: 32, Label: "测试 token"},
			{Env: "TEST_PASSWORD", Encoding: SecretPassword, Bytes: 24, Label: "测试口令"},
			{Env: "TEST_B64", Encoding: SecretBase64, Bytes: 18},
		},
	}
}

// prepareForTest 跑一次"安装编排里负责落盘的那一半"，返回结果与 compose 文件路径。
func prepareForTest(t *testing.T, m *Manager, app App) (*InstallResult, string) {
	t.Helper()
	res := &InstallResult{App: app.ID, Name: app.Name, Steps: []string{}}
	file, err := m.prepareComposeProject(context.Background(), app, res)
	if err != nil {
		t.Fatalf("prepareComposeProject 失败: %v", err)
	}
	return res, file
}

func credValueOf(res *InstallResult, key string) (string, bool) {
	for _, c := range res.Credentials {
		if c.Key == key {
			return c.Value, true
		}
	}
	return "", false
}

// TestComposeSecretsFirstInstallWritesEnvAndCredentials 是能力的第一条：
// 首次安装必须生成随机值、写进 0600 的 .env，并出现在安装结果的凭据区块。
func TestComposeSecretsFirstInstallWritesEnvAndCredentials(t *testing.T) {
	m, _ := sandboxManager(t)
	app := secretTestApp()

	res, composeFile := prepareForTest(t, m, app)

	if _, err := os.Stat(composeFile); err != nil {
		t.Fatalf("compose 文件没写出来: %v", err)
	}
	// compose 文件里**不能**有明文，只能有 ${VAR} 引用
	yaml, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}

	envPath := filepath.Join(m.composeDir(), app.ID, composeEnvFileName)
	st, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf(".env 没写出来: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env 权限必须是 0600（可能含密钥），实际 %o", perm)
	}
	envText, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseComposeEnv(string(envText))

	for _, s := range app.ComposeSecrets {
		v, ok := credValueOf(res, s.Env)
		if !ok {
			t.Fatalf("凭据区块里缺 %s（生成的明文必须只在这里出现一次）", s.Env)
		}
		if strings.TrimSpace(v) == "" {
			t.Fatalf("%s 生成了空值", s.Env)
		}
		if parsed[s.Env] != v {
			t.Errorf(".env 里的 %s=%q 与凭据区块的 %q 不一致", s.Env, parsed[s.Env], v)
		}
		if strings.Contains(string(yaml), v) {
			t.Errorf("compose 文件里出现了 %s 的明文 —— 必须只用 ${%s} 引用", s.Env, s.Env)
		}
	}

	// 长度契约：hex/base64 按字节数，password 按字符数
	if v, _ := credValueOf(res, "TEST_TOKEN"); len(v) != 64 {
		t.Errorf("hex 32 字节应是 64 个字符，实际 %d（%q）", len(v), v)
	}
	if v, _ := credValueOf(res, "TEST_PASSWORD"); len(v) != 24 {
		t.Errorf("password 24 应恰好 24 个字符，实际 %d", len(v))
	}
	if v, _ := credValueOf(res, "TEST_B64"); len(v) != 24 {
		t.Errorf("base64 18 字节应是 24 个字符，实际 %d", len(v))
	}
	if v, _ := credValueOf(res, "TEST_B64"); strings.ContainsAny(v, "+/=") {
		t.Errorf("base64 用了 URL-safe 无填充，不该出现 + / = ：%q", v)
	}
}

// TestComposeSecretsReinstallReusesExistingEnv 是**最危险那条**的锁：
// 重装/升级不能重新生成。做法：跑两次安装编排，断言 .env 字节不变、
// mtime 不变（没写盘）、结果里复用的是旧值。
func TestComposeSecretsReinstallReusesExistingEnv(t *testing.T) {
	m, _ := sandboxManager(t)
	app := secretTestApp()

	first, _ := prepareForTest(t, m, app)
	envPath := filepath.Join(m.composeDir(), app.ID, composeEnvFileName)
	before, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	stBefore, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}

	// 第二次安装（模拟重装：磁盘上数据还在，只是重新跑一遍编排）
	time.Sleep(15 * time.Millisecond)
	second, _ := prepareForTest(t, m, app)

	after, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf(".env 被重装改动了！\n之前:\n%s\n之后:\n%s", before, after)
	}
	stAfter, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !stBefore.ModTime().Equal(stAfter.ModTime()) {
		t.Errorf("内容没变时不该重写 .env（mtime 变了：%v → %v）", stBefore.ModTime(), stAfter.ModTime())
	}

	for _, s := range app.ComposeSecrets {
		v1, _ := credValueOf(first, s.Env)
		v2, ok := credValueOf(second, s.Env)
		if !ok {
			t.Fatalf("重装结果里缺 %s（用户看不到复用到的旧口令）", s.Env)
		}
		if v1 != v2 {
			t.Fatalf("重装重新生成了 %s：旧 %q → 新 %q（Immich 的 DB 口令一变数据库就连不上）",
				s.Env, v1, v2)
		}
	}
	joined := strings.Join(second.Steps, "\n")
	if !strings.Contains(joined, "复用") {
		t.Errorf("重装步骤里要如实说明复用了旧密钥，实际：\n%s", joined)
	}
}

// TestComposeSecretsNeverLeakIntoSteps 是脱敏硬约束：
// 生成/复用的明文只能出现在 Credentials，不许出现在 Steps（会被长期保存/转发）。
func TestComposeSecretsNeverLeakIntoSteps(t *testing.T) {
	m, _ := sandboxManager(t)
	app := secretTestApp()

	res, _ := prepareForTest(t, m, app)
	if len(res.Steps) == 0 {
		t.Fatal("安装编排总该有步骤，测试前提不成立")
	}
	joined := strings.Join(res.Steps, "\n")
	for _, c := range res.Credentials {
		if strings.Contains(joined, c.Value) {
			t.Fatalf("密钥明文泄漏进了安装步骤/日志：%s 的值 %q 出现在：\n%s", c.Key, c.Value, joined)
		}
	}
	// 反向：凭据区块里必须真的有它们（否则"脱敏"就是靠什么都不显示实现的）
	if len(res.Credentials) != len(app.ComposeSecrets) {
		t.Errorf("凭据区块应有 %d 条，实际 %d", len(app.ComposeSecrets), len(res.Credentials))
	}
}

// TestComposeSecretsReusePartialEnvAndPreserveOtherKeys 锁住更细的两条：
//   - .env 里已有值的键**复用**，缺的才生成（不是"文件存在就整体跳过"）；
//   - 用户自己加进 .env 的其它键不被抹掉。
func TestComposeSecretsReusePartialEnvAndPreserveOtherKeys(t *testing.T) {
	m, _ := sandboxManager(t)
	app := secretTestApp()

	dir := filepath.Join(m.composeDir(), app.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const fixed = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	pre := "# 用户自己写的注释\nTEST_TOKEN=" + fixed + "\nMY_CUSTOM_KEY=keep-me\n"
	if err := os.WriteFile(filepath.Join(dir, composeEnvFileName), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	res, _ := prepareForTest(t, m, app)
	if v, _ := credValueOf(res, "TEST_TOKEN"); v != fixed {
		t.Fatalf("已有值必须复用，实际 %q（期望 %q）", v, fixed)
	}
	envText, err := os.ReadFile(filepath.Join(dir, composeEnvFileName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(envText)
	if !strings.Contains(text, "MY_CUSTOM_KEY=keep-me") {
		t.Errorf("用户自己加的键被抹掉了：\n%s", text)
	}
	if !strings.Contains(text, "# 用户自己写的注释") {
		t.Errorf("用户自己的注释被抹掉了：\n%s", text)
	}
	if !strings.Contains(text, "TEST_PASSWORD=") || !strings.Contains(text, "TEST_B64=") {
		t.Errorf("缺失的键没有被补上：\n%s", text)
	}
	// 权限要被纠正成 0600（原来是 0644）
	st, err := os.Stat(filepath.Join(dir, composeEnvFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf(".env 权限应被纠正为 0600，实际 %o", st.Mode().Perm())
	}
}

// TestGenerateSecretValueEncodings 锁住生成器本身：长度、字符集、随机性。
func TestGenerateSecretValueEncodings(t *testing.T) {
	hexV, err := generateSecretValue(ComposeSecret{Env: "X", Encoding: SecretHex, Bytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(hexV) != 32 {
		t.Errorf("hex 16 字节应为 32 字符，实际 %d", len(hexV))
	}
	hexV2, _ := generateSecretValue(ComposeSecret{Env: "X", Encoding: SecretHex, Bytes: 16})
	if hexV == hexV2 {
		t.Error("两次生成不应相同（随机性）")
	}

	pw, err := generateSecretValue(ComposeSecret{Env: "X", Encoding: SecretPassword, Bytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 20 {
		t.Errorf("password 长度错了：%d", len(pw))
	}
	for _, r := range pw {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", r) {
			t.Errorf("password 出现了不可控字符 %q（会破坏 .env / shell）：%q", r, pw)
		}
	}

	b64, err := generateSecretValue(ComposeSecret{Env: "X", Encoding: SecretBase64, Bytes: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(b64) != 16 {
		t.Errorf("base64 12 字节应为 16 字符，实际 %d", len(b64))
	}
	if strings.ContainsAny(b64, "+/=") {
		t.Errorf("base64 应 URL-safe 无填充：%q", b64)
	}

	// 太短的下限被抬到 composeSecretMinBytes（不许生成空口令）
	short, err := generateSecretValue(ComposeSecret{Env: "X", Encoding: SecretHex, Bytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != composeSecretMinBytes*2 {
		t.Errorf("低于下限时应抬到 %d 字节，实际 hex 长度 %d", composeSecretMinBytes, len(short))
	}

	if _, err := generateSecretValue(ComposeSecret{Env: "X", Encoding: "rot13", Bytes: 16}); err == nil {
		t.Error("未知生成方式必须报错，不能悄悄生成一个东西")
	}
}

// TestComposeSecretProblemsCatchesSilentFailures 证明静态校验不是空断言。
func TestComposeSecretProblemsCatchesSilentFailures(t *testing.T) {
	base := secretTestApp()
	if problems := ComposeSecretProblems(base); len(problems) != 0 {
		t.Fatalf("基准声明应该干净，实际：%v", problems)
	}
	cases := []struct {
		name   string
		mutate func(*App)
	}{
		{"Env 为空", func(a *App) { a.ComposeSecrets[0].Env = "" }},
		{"Env 重复", func(a *App) { a.ComposeSecrets[0].Env = "TEST_PASSWORD" }},
		{"生成方式未知", func(a *App) { a.ComposeSecrets[0].Encoding = "md5" }},
		{"长度太短", func(a *App) { a.ComposeSecrets[0].Bytes = 1 }},
		{"compose 里没有引用", func(a *App) {
			a.ComposeYAML = strings.ReplaceAll(a.ComposeYAML, "${TEST_TOKEN}", "hardcoded")
		}},
		{"非 compose 应用也声明密钥", func(a *App) { a.Kind = KindNative }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			a.ComposeSecrets = append([]ComposeSecret(nil), base.ComposeSecrets...)
			tc.mutate(&a)
			if problems := ComposeSecretProblems(a); len(problems) == 0 {
				t.Fatalf("坏声明没有被拦下：%+v", a.ComposeSecrets)
			}
		})
	}
}

// TestCatalogComposeSecretDeclarationsAreSane 让静态门禁覆盖**真实目录**：
// 每个声明的密钥都要能在 compose 里被 ${} 引用、生成方式与长度合法。
// 并且上架的两个应用（Activepieces / Immich）必须真的声明了密钥 ——
// 否则这个能力等于没接线。
func TestCatalogComposeSecretDeclarationsAreSane(t *testing.T) {
	for _, a := range Catalog() {
		if problems := ComposeSecretProblems(a); len(problems) > 0 {
			t.Errorf("%s 的 ComposeSecrets 声明有问题：%v", a.ID, problems)
		}
	}
	want := map[string][]string{
		"activepieces": {"AP_ENCRYPTION_KEY", "AP_JWT_SECRET", "AP_POSTGRES_PASSWORD"},
		"immich":       {"DB_PASSWORD"},
	}
	for id, envs := range want {
		app, ok := FindApp(id)
		if !ok {
			t.Fatalf("目录里没有 %s", id)
		}
		got := map[string]bool{}
		for _, s := range app.ComposeSecrets {
			got[s.Env] = true
		}
		for _, env := range envs {
			if !got[env] {
				t.Errorf("%s 必须声明安装时随机密钥 %s（否则就是写死的默认口令）", id, env)
			}
		}
	}
}

// TestActivepiecesEncryptionKeyIsExactly32HexChars 是一条**上游契约**的锁：
// Activepieces 源码用 Buffer.from(secret,'binary') + aes-256-cbc，
// 密钥必须恰好 32 个字符（16 字节 hex）；给 64 个字符会 Invalid key length。
// 任务原话写的是"各 32 字节 hex"，但 AP_ENCRYPTION_KEY 必须按上游来。
func TestActivepiecesEncryptionKeyIsExactly32HexChars(t *testing.T) {
	app, ok := FindApp("activepieces")
	if !ok {
		t.Fatal("目录里没有 activepieces")
	}
	var found bool
	for _, s := range app.ComposeSecrets {
		if s.Env != "AP_ENCRYPTION_KEY" {
			continue
		}
		found = true
		if s.Encoding != SecretHex {
			t.Errorf("AP_ENCRYPTION_KEY 应为 hex，实际 %q", s.Encoding)
		}
		if s.Bytes != 16 {
			t.Errorf("AP_ENCRYPTION_KEY 必须是 16 字节（32 个 hex 字符），实际 %d 字节", s.Bytes)
		}
	}
	if !found {
		t.Fatal("activepieces 没有声明 AP_ENCRYPTION_KEY")
	}
}

// TestComposeSecretsWiredIntoInstallViaCompose 是结构性断言：
// installViaCompose 必须复用 prepareComposeProject（生成/复用 + 凭据接线都在里面），
// 而不是自己再写一份写盘逻辑 —— 两份实现迟早分叉，而分叉的第一个后果
// 就是"某个应用重装时换了数据库口令"。
func TestComposeSecretsWiredIntoInstallViaCompose(t *testing.T) {
	src, err := os.ReadFile("install.go")
	if err != nil {
		t.Fatalf("读不到 install.go: %v", err)
	}
	if !strings.Contains(string(src), "m.prepareComposeProject(ctx, app, res)") {
		t.Error("installViaCompose 必须调用 prepareComposeProject（否则密钥能力在真实安装路径上没接线）")
	}
}
