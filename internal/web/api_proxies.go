package web

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zizdog/zizpanel/internal/acme"
	"github.com/zizdog/zizpanel/internal/proxies"
	"github.com/zizdog/zizpanel/internal/sites"
	"github.com/zizdog/zizpanel/internal/tlsx"
)

// 「反向代理」独立功能：一条规则 = 监听端口 + 域名/Host + 路径前缀 + 目标地址，
// 不需要站点目录（与「网站管理」的分工）。规则存 proxies 表，配置写进
// vhosts/proxy-<id>.conf（前缀保证不与站点 <域名>.conf 撞名）；写入与 reload
// 复用站点侧 helper（含 nginx -t 校验与失败回滚）。

// proxyLogDir 是反代规则自己的日志目录（与站点日志分开，便于按规则排查）。
// 面板以 root 建目录、nginx 以真实用户写日志，建完必须把归属交还真实用户
// （reload 前由 chownProxyLogTrees 整棵递归修正，否则 reload 静默失败）。
func (s *Server) proxyLogDir() string {
	dir := filepath.Join(s.Cfg.LogRoot, "proxy")
	_ = os.MkdirAll(dir, 0o755)
	s.chownTreeToUser(dir)
	return dir
}

func (s *Server) proxyRepo() *proxies.Repository {
	return proxies.NewRepository(s.Store)
}

// 反向代理「需要用户名密码」（HTTP Basic Auth）：每条规则可单独开启，否则 401。
// 真相存 settings 表键 `proxy_auth.<id>`（**不新增 DB 列**），applyProxy 生成配置前装回 Rule。
// apr1 哈希：本机 nginx 1.31.5 实测**不支持 bcrypt**（2026-09-25 实测）；仓库里只有哈希，明文绝不落盘/进日志。

const proxyAuthSettingPrefix = "proxy_auth."

// proxyAuthConfig 是持久化的鉴权配置（settings 表里一行 JSON）。
type proxyAuthConfig struct {
	Enabled bool   `json:"enabled"`
	User    string `json:"user"`
	Hash    string `json:"hash"`
}

func proxyAuthKey(id int64) string {
	return proxyAuthSettingPrefix + strconv.FormatInt(id, 10)
}

// proxyAuthFile 返回某条规则的 htpasswd 文件路径：放 DataDir 下、0600、属主对齐 DataDir
// —— 与 alignProxyCertOwner 同一条真机教训：nginx 读不到就是 401 到底/配置报错。
func (s *Server) proxyAuthFile(id int64) string {
	return filepath.Join(s.Cfg.DataDir, "proxy-auth", fmt.Sprintf("proxy-%d.htpasswd", id))
}

// loadProxyAuth 读取一条规则的鉴权配置（没有就返回零值 = 未开启）。
func (s *Server) loadProxyAuth(ctx context.Context, id int64) (proxyAuthConfig, error) {
	raw, err := s.Store.GetSetting(ctx, proxyAuthKey(id))
	if err != nil {
		return proxyAuthConfig{}, fmt.Errorf("读取规则 %d 的访问鉴权配置失败：%w", id, err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return proxyAuthConfig{}, nil
	}
	var cfg proxyAuthConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return proxyAuthConfig{}, fmt.Errorf("规则 %d 的访问鉴权配置已损坏（%v）；"+
			"请到该规则的「编辑」里重新设置用户名与密码", id, err)
	}
	return cfg, nil
}

// saveProxyAuth 写入一条规则的鉴权配置（关闭时也写一行 enabled=false，保持显式）。
func (s *Server) saveProxyAuth(ctx context.Context, id int64, cfg proxyAuthConfig) error {
	if !cfg.Enabled {
		cfg = proxyAuthConfig{}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化访问鉴权配置失败：%w", err)
	}
	if err := s.Store.SetSetting(ctx, proxyAuthKey(id), string(b)); err != nil {
		return fmt.Errorf("保存访问鉴权配置失败：%w", err)
	}
	return nil
}

// hydrateProxyAuth 把持久化的鉴权配置装回 Rule（生成 nginx 配置前必须调用）。
// 唯一把 Auth* 字段填进 Rule 的地方：任何"重新生成配置"的路径漏了它，鉴权会被静默
// 抹掉 —— 所以由 applyProxy 统一调用，而不是指望每个调用方自觉。
func (s *Server) hydrateProxyAuth(ctx context.Context, rule *proxies.Rule) error {
	if rule == nil {
		return nil
	}
	cfg, err := s.loadProxyAuth(ctx, rule.ID)
	if err != nil {
		return err
	}
	rule.AuthEnabled = cfg.Enabled
	rule.AuthUser = cfg.User
	rule.AuthHash = cfg.Hash
	if cfg.Enabled {
		rule.AuthFile = s.proxyAuthFile(rule.ID)
	} else {
		rule.AuthFile = ""
	}
	return nil
}

// writeProxyAuthFile 按当前 Rule 的鉴权配置生成/删除 htpasswd 文件：关闭后 vhost
// 不再引用它，留着一份没用的口令哈希躺在盘上没有意义。
func (s *Server) writeProxyAuthFile(rule *proxies.Rule) error {
	if rule == nil {
		return nil
	}
	if !rule.AuthEnabled {
		if rule.ID > 0 {
			_ = os.Remove(s.proxyAuthFile(rule.ID))
		}
		return nil
	}
	if strings.TrimSpace(s.Cfg.DataDir) == "" {
		return fmt.Errorf("数据目录未配置，无法生成访问鉴权的密码文件")
	}
	path := s.proxyAuthFile(rule.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建密码文件目录失败：%w", err)
	}
	// 一行一个用户；哈希里不含换行，用户名在写入前已校验过。
	content := rule.AuthUser + ":" + rule.AuthHash + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("写入密码文件失败：%w", err)
	}
	s.alignProxyAuthOwner(path)
	return nil
}

// alignProxyAuthOwner 把密码文件属主对齐 DataDir（与证书同一条真机教训）：root 用
// 0600 写出的文件 nginx（真实用户）读不到，表现为 permission denied 或一律 401。
func (s *Server) alignProxyAuthOwner(path string) {
	if os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(s.Cfg.DataDir)
	if err != nil {
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil && s.Log != nil {
		s.Log.Warn("调整密码文件 %s 属主失败：%v", path, err)
	}
}

// proxyAuthFromRequest 把请求里的鉴权字段合并到已有配置并校验：auth_password 非空才
// 更新哈希（空串 = 沿用原密码，前端不回显，所以改用户名/备注不会被迫重设密码）；
// 关闭时把用户名与哈希一起清掉；开启时两者都必须在，绝不生成一份没有鉴权的配置。
func proxyAuthFromRequest(req proxyReq, cur proxyAuthConfig) (proxyAuthConfig, error) {
	next := cur
	if req.AuthEnabled != nil {
		next.Enabled = *req.AuthEnabled
	}
	if req.AuthUser != nil {
		next.User = strings.TrimSpace(*req.AuthUser)
	}
	if req.AuthPassword != nil && *req.AuthPassword != "" {
		hash, err := hashProxyBasicAuthPassword(*req.AuthPassword)
		if err != nil {
			return proxyAuthConfig{}, err
		}
		next.Hash = hash
	}
	if !next.Enabled {
		return proxyAuthConfig{Enabled: false}, nil
	}
	if next.User == "" {
		return proxyAuthConfig{}, fmt.Errorf("已开启「需要用户名密码」，请填一个用户名")
	}
	if strings.ContainsAny(next.User, ":\r\n") {
		return proxyAuthConfig{}, fmt.Errorf("用户名不能包含冒号或换行（htpasswd 用冒号分隔用户名与哈希）")
	}
	if len(next.User) > 128 {
		return proxyAuthConfig{}, fmt.Errorf("用户名过长（%d 字符，最多 128）", len(next.User))
	}
	if next.Hash == "" {
		return proxyAuthConfig{}, fmt.Errorf("已开启「需要用户名密码」，请设置密码（编辑已有规则时留空表示沿用原密码）")
	}
	return next, nil
}

// proxyAuthView 回读一条规则**磁盘上生效的**鉴权配置（"保存后回读生效值"）：
// ① htpasswd 文件真在且含该用户与哈希；② 生成的 nginx 配置里真有 auth_basic 与
// auth_basic_user_file。两者都对才 verified=true，否则 note 写清卡在哪一步，界面显示"未复核"。
func (s *Server) proxyAuthView(ctx context.Context, rule *proxies.Rule) map[string]any {
	view := map[string]any{
		"enabled": false, "user": "", "password_set": false,
		"verified": false, "note": "",
	}
	if rule == nil {
		return view
	}
	cfg, err := s.loadProxyAuth(ctx, rule.ID)
	if err != nil {
		view["note"] = "未复核：" + err.Error()
		return view
	}
	view["enabled"] = cfg.Enabled
	view["user"] = cfg.User
	view["password_set"] = cfg.Hash != ""
	if !cfg.Enabled {
		return view
	}
	path := s.proxyAuthFile(rule.ID)
	view["user_file"] = path
	b, ferr := os.ReadFile(path)
	if ferr != nil {
		view["note"] = "未复核：读不到生成的密码文件 " + path + "（" + ferr.Error() + "）"
		return view
	}
	fileUser, hasHash := parseHtpasswdFirstUser(string(b))
	view["user_file_user"] = fileUser
	view["user_file_has_hash"] = hasHash

	vhostPath := filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf")
	view["vhost_path"] = vhostPath
	vh, verr := os.ReadFile(vhostPath)
	if verr != nil {
		view["note"] = "未复核：读不到 nginx 配置文件 " + vhostPath +
			"（规则可能已停用或还没生成；nginx 未加载时鉴权不生效）"
		return view
	}
	content := string(vh)
	hasAuth := strings.Contains(content, "auth_basic ")
	hasFile := strings.Contains(content, "auth_basic_user_file "+path+";")
	view["vhost_has_auth_basic"] = hasAuth
	view["vhost_has_user_file"] = hasFile
	if fileUser != "" && fileUser == cfg.User && hasHash && hasAuth && hasFile {
		view["verified"] = true
		return view
	}
	view["note"] = fmt.Sprintf("未复核：生成的配置与保存值不一致"+
		"（密码文件里的用户=%q、含哈希=%v；nginx 配置含 auth_basic=%v、指向本文件的 auth_basic_user_file=%v）",
		fileUser, hasHash, hasAuth, hasFile)
	return view
}

// parseHtpasswdFirstUser 解析 htpasswd 的第一条有效行，返回 (用户名, 是否带哈希)；
// 只为回读/展示，不做校验（校验由 nginx 自己做）。
func parseHtpasswdFirstUser(content string) (string, bool) {
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		i := strings.Index(ln, ":")
		if i <= 0 {
			return "", false
		}
		return ln[:i], strings.TrimSpace(ln[i+1:]) != ""
	}
	return "", false
}

// hashProxyBasicAuthPassword 生成 apr1 口令哈希（带 8 字符随机盐）。
func hashProxyBasicAuthPassword(password string) (string, error) {
	salt, err := randomApr1Salt()
	if err != nil {
		return "", err
	}
	return apr1Crypt(password, salt), nil
}

// apr1SaltChars 是 apr1 盐使用的字符表（与 apr1 的 itoa64 一致，取前 64 个）。
const apr1SaltChars = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomApr1Salt() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机盐失败：%w", err)
	}
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = apr1SaltChars[int(b)%len(apr1SaltChars)]
	}
	return string(out), nil
}

// apr1Crypt 实现 Apache 的 apr1（MD5 crypt）口令哈希：输出形如 `$apr1$<salt>$<22 字符>`，
// nginx 的 auth_basic_user_file 原生支持（本机 nginx 1.31.5 实测 200/401 正确）。
// 自己实现而不调 `openssl passwd -apr1`：后者把明文放进命令行参数，同机 `ps` 就能看到。
func apr1Crypt(password, salt string) string {
	if len(salt) > 8 {
		salt = salt[:8]
	}
	// 第一轮：md5(password + "$apr1$" + salt)
	h := md5.New()
	_, _ = h.Write([]byte(password))
	_, _ = h.Write([]byte("$apr1$"))
	_, _ = h.Write([]byte(salt))

	// md5(password + salt + password)，按 length 规则混入
	alt := md5.New()
	_, _ = alt.Write([]byte(password))
	_, _ = alt.Write([]byte(salt))
	_, _ = alt.Write([]byte(password))
	altSum := alt.Sum(nil)
	for i := len(password); i > 0; i -= 16 {
		if i > 16 {
			_, _ = h.Write(altSum)
		} else {
			_, _ = h.Write(altSum[:i])
		}
	}
	for i := len(password); i > 0; i >>= 1 {
		if i&1 != 0 {
			_, _ = h.Write([]byte{0})
		} else if len(password) > 0 {
			_, _ = h.Write([]byte{password[0]})
		}
	}
	sum := h.Sum(nil)

	// 1000 轮拉伸
	for i := 0; i < 1000; i++ {
		c := md5.New()
		if i&1 != 0 {
			_, _ = c.Write([]byte(password))
		} else {
			_, _ = c.Write(sum)
		}
		if i%3 != 0 {
			_, _ = c.Write([]byte(salt))
		}
		if i%7 != 0 {
			_, _ = c.Write([]byte(password))
		}
		if i&1 != 0 {
			_, _ = c.Write(sum)
		} else {
			_, _ = c.Write([]byte(password))
		}
		sum = c.Sum(nil)
	}

	var b strings.Builder
	b.WriteString("$apr1$")
	b.WriteString(salt)
	b.WriteByte('$')
	// 按 apr_md5.c 的固定顺序编码 16 字节。
	groups := [][3]int{{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5}}
	for _, g := range groups {
		v := int(sum[g[0]])<<16 | int(sum[g[1]])<<8 | int(sum[g[2]])
		b.WriteString(apr1To64(v, 4))
	}
	b.WriteString(apr1To64(int(sum[11]), 2))
	return b.String()
}

// apr1To64 是 apr1 的自定义 base64 编码（低位在前）。
func apr1To64(v, n int) string {
	out := make([]byte, 0, n)
	for ; n > 0; n-- {
		out = append(out, apr1SaltChars[v&0x3f])
		v >>= 6
	}
	return string(out)
}

// proxyLookupHostFn 是"目标是公网还是局域网"判定用的 DNS 解析器，做成变量是为了
// 单测能钉住解析结果（否则一次 go test 就会去查真实 DNS）。
// 只用于**判定与提示**：真正决定"要不要起转发器"的是 Manager.NeedsForward。
var proxyLookupHostFn = net.LookupHost

// proxyProbeTargetFn 是"面板自己能不能直连目标"的探针，做成变量是为了让
// directLANBlockedAdvice 可单测（真去连局域网地址在单测里不允许）。
var proxyProbeTargetFn = probeTarget

// proxyLANForwardAdvice 是"nginx 直连局域网目标失败"时给用户的话：必须写清是什么
// （macOS 15 本地网络授权）、为什么无头服务器救不了、怎么修（改成经面板转发）。
const proxyLANForwardAdvice = "nginx 连不上局域网目标（macOS 15 本地网络授权）：无头服务器无法弹窗授权；" +
	"建议把本规则改为『经面板转发』（推荐），或到 系统设置 → 隐私与安全性 → 本地网络 里给 nginx 授权"

// handleProxyList 列出全部规则并带上每条规则的实时状态。最要紧的是 `port_listening`：
// "已启用"不等于 nginx 真的在听（端口被占 / nginx 没起来），必须如实显示，
// 否则用户会以为规则生效了却访问不到。
func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	list, err := s.proxyRepo().List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(list))
	for _, rule := range list {
		items = append(items, s.proxyView(r.Context(), rule))
	}
	ok(w, map[string]any{
		"list": items,
		"nginx": map[string]any{
			"installed": s.nginxInstalled(),
			"config":    s.Cfg.NginxConf,
		},
	})
}

// 规则「实时状态」的 TTL 缓存：列表只读缓存，真实探测（目标 3s + 端口拨号 800ms）
// 改为前端渲染后异步触发（AGENTS 第三节坑 165：昂贵探测不许放在列表/首屏路径上）。
// 缓存的旧结果带检测时间展示，绝不把过期缓存当成实时状态。

const (
	// proxyStatusTTL 是"结果还算新鲜"的时长；超过它界面要标出实际检测时间。
	proxyStatusTTL = 60 * time.Second
	// proxyStatusProbeTimeout 是单条规则状态探测的超时。刻意远小于列表接口里原来的 3s：
	// 状态是"锦上添花"，不值得让用户等。
	proxyStatusProbeTimeout = 500 * time.Millisecond
	// proxyStatusProbeConcurrency 是并发探测的条数上限（避免一次开太多 fd）。
	proxyStatusProbeConcurrency = 8
)

// proxyStatusEntry 是一条规则的探测结果缓存项。
type proxyStatusEntry struct {
	Listening bool
	Reachable bool
	Detail    string
	At        time.Time
}

// proxyStatusSnapshot 是给视图/接口用的只读快照。
type proxyStatusSnapshot struct {
	// Known 表示缓存里有没有这条规则的结果（没有 = 从未探测过）。
	Known bool
	// Stale 表示结果已超过 TTL（界面必须标"约 N 秒前检测"，不能当实时）。
	Stale     bool
	Listening bool
	Reachable bool
	Detail    string
	At        time.Time
	Age       time.Duration
}

// proxyStatusCache 是进程内缓存。用包级变量是因为 Server 结构体不在本轮允许改动的
// 文件范围内；键里带 target/listen，避免测试之间同 id 不同规则串结果。
var proxyStatusCache = struct {
	mu sync.Mutex
	m  map[string]proxyStatusEntry
}{m: map[string]proxyStatusEntry{}}

func proxyStatusKey(rule *proxies.Rule) string {
	if rule == nil {
		return ""
	}
	return fmt.Sprintf("%d|%d|%s", rule.ID, rule.Listen, rule.Target)
}

func proxyStatusLookup(rule *proxies.Rule) proxyStatusSnapshot {
	key := proxyStatusKey(rule)
	if key == "" {
		return proxyStatusSnapshot{}
	}
	proxyStatusCache.mu.Lock()
	e, ok := proxyStatusCache.m[key]
	proxyStatusCache.mu.Unlock()
	if !ok {
		return proxyStatusSnapshot{}
	}
	age := time.Since(e.At)
	if age < 0 {
		age = 0
	}
	return proxyStatusSnapshot{
		Known: true, Stale: age > proxyStatusTTL,
		Listening: e.Listening, Reachable: e.Reachable, Detail: e.Detail,
		At: e.At, Age: age,
	}
}

func proxyStatusStore(rule *proxies.Rule, listening, reachable bool, detail string, at time.Time) {
	key := proxyStatusKey(rule)
	if key == "" {
		return
	}
	proxyStatusCache.mu.Lock()
	proxyStatusCache.m[key] = proxyStatusEntry{
		Listening: listening, Reachable: reachable, Detail: detail, At: at,
	}
	proxyStatusCache.mu.Unlock()
}

// proxyStatusResetCache 清空缓存（单测用；生产没有调用方）。
func proxyStatusResetCache() {
	proxyStatusCache.mu.Lock()
	proxyStatusCache.m = map[string]proxyStatusEntry{}
	proxyStatusCache.mu.Unlock()
}

// probeProxyStatuses 并发探测一批规则的状态，写进缓存并返回逐条结果（每条 500ms 超时，
// 信号量限并发）。**绝不在 HTTP 列表路径里调用它**。
func (s *Server) probeProxyStatuses(ctx context.Context, rules []*proxies.Rule) map[int64]proxyStatusSnapshot {
	out := map[int64]proxyStatusSnapshot{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, proxyStatusProbeConcurrency)
	at := time.Now()
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(r *proxies.Rule) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, proxyStatusProbeTimeout)
			defer cancel()
			listening := false
			if r.Enabled {
				listening = portListening(pctx, r.Listen)
			}
			reachable, detail := proxyProbeTargetFn(pctx, r.Target)
			proxyStatusStore(r, listening, reachable, detail, at)
			mu.Lock()
			out[r.ID] = proxyStatusSnapshot{
				Known: true, Listening: listening, Reachable: reachable,
				Detail: detail, At: at,
			}
			mu.Unlock()
		}(rule)
	}
	wg.Wait()
	return out
}

// proxyView 组装一条规则给前端的样子（含实时状态）。
func (s *Server) proxyView(ctx context.Context, rule *proxies.Rule) map[string]any {
	if err := s.hydrateProxyAuth(ctx, rule); err != nil && s.Log != nil {
		s.Log.Warn("读取规则 %d 的鉴权配置失败：%v", rule.ID, err)
	}
	auth := s.proxyAuthView(ctx, rule)

	// 实时状态只读 TTL 缓存：**列表路径绝不跑真实探测**（见上面的缓存说明）。
	st := proxyStatusLookup(rule)
	detail := st.Detail
	if !st.Known {
		detail = "尚未检测：打开本页后会自动探测（也可以点这条规则上的「检测」）"
	}

	host, port, _ := rule.TargetHostPort()
	vhost := filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf")
	_, statErr := os.Stat(vhost)
	// 域名兜底块是否存在：界面要能看出"域名限制到底有没有生效"。
	reject := filepath.Join(s.Cfg.VhostDir, proxies.RejectVhostName(rule.Listen)+".conf")
	_, rejectErr := os.Stat(reject)
	domainGuard := len(proxies.SplitDomains(rule.Domains)) > 0

	// 局域网出口状态：三态 + 是否真的在转发 + 实时连接/流量 + 最近一次转发错误。
	// 目标是私有网段却在直连时，界面必须显眼提示"可能因 macOS 授权失效而 502"。
	fwd, fwdListening := s.forwarders.Status(rule.ID)
	scope := s.forwarders.TargetScope(rule.Target)
	return map[string]any{
		"rule":          rule,
		"id":            rule.ID,
		"name":          rule.Name,
		"listen":        rule.Listen,
		"domains":       rule.Domains,
		"path":          rule.Path,
		"target":        rule.Target,
		"preserve_host": rule.PreserveHost,
		"websocket":     rule.Websocket,
		"enabled":       rule.Enabled,
		"remark":        rule.Remark,
		// 列表/编辑表单要能读回这两个字段（漏了会让"编辑规则"把已保存的
		// SNI 与常用请求头开关悄悄清掉）。
		"tls_name":         rule.TLSName,
		"standard_headers": rule.StandardHeaders,
		"redirect_http":    rule.RedirectHTTP,
		"created_at":       rule.Created,
		"updated_at":       rule.Updated,
		"port_listening":   st.Listening,
		"target_host":      host,
		"target_port":      port,
		"target_ok":        st.Reachable,
		"target_detail":    detail,
		// 状态探测的元信息：界面据此显示"约 N 秒前检测"，绝不把过期缓存当成实时（诚实原则）。
		"status_probed":       st.Known,
		"status_stale":        st.Stale,
		"status_probe_at":     statusProbeAtString(st),
		"status_probe_age_ms": statusProbeAgeMS(st),
		"config_written":      statErr == nil,
		"config_path":         vhost,
		"domain_guard":        domainGuard,
		"reject_written":      rejectErr == nil,
		"reject_path":         reject,
		// HTTPS：既有平铺字段（ssl_enabled 等），也有"证书文件实际内容"的汇总（ssl.days_left）。
		"ssl_enabled":  rule.SSLEnabled,
		"ssl_cert":     rule.SSLCert,
		"ssl_key":      rule.SSLKey,
		"ssl_provider": rule.SSLProvider,
		"ssl_expires":  rule.SSLExpires,
		"ssl":          s.proxySSLView(rule),
		// ---- 访问鉴权（HTTP Basic Auth）----
		"auth":          auth,
		"auth_enabled":  auth["enabled"],
		"auth_user":     auth["user"],
		"auth_verified": auth["verified"],
		"auth_note":     auth["note"],
		// ---- 局域网出口 ----
		"lan_forward":          rule.LANForwardMode(),
		"lan_forward_label":    proxies.LANForwardLabel(rule.LANForwardMode()),
		"forward_port":         rule.ForwardPort,
		"forward_active":       fwdListening,
		"forward_upstream":     fwd.Upstream,
		"forward_active_conns": fwd.ActiveConns,
		"forward_total_conns":  fwd.TotalConns,
		"forward_bytes_in":     fwd.BytesIn,
		"forward_bytes_out":    fwd.BytesOut,
		"forward_last_error":   fwd.LastError,
		"forward_error_count":  fwd.ErrorCount,
		"target_scope":         string(scope),
		// 直连 + 局域网目标 = 随时可能被 macOS 隐私门拦成 502。
		"lan_direct_warning": scope == proxies.ScopePrivate && !fwdListening && rule.Enabled,
	}
}

// statusProbeAtString / statusProbeAgeMS 是给前端的检测时间展示字段。
func statusProbeAtString(st proxyStatusSnapshot) string {
	if !st.Known || st.At.IsZero() {
		return ""
	}
	return st.At.Format(time.RFC3339)
}

func statusProbeAgeMS(st proxyStatusSnapshot) int64 {
	if !st.Known {
		return -1
	}
	return st.Age.Milliseconds()
}

// proxySSLView 汇总反代规则证书的展示字段（与站点侧 siteSSLView 同一套口径）：到期时间
// 优先取自**真实证书文件**（数据库里的只是上次写入的快照，acme 续期后可能还没更新）；
// 读不到文件时如实标 renew_hint，绝不显示成"正常"。
func (s *Server) proxySSLView(rule *proxies.Rule) map[string]any {
	v := map[string]any{
		"enabled":        rule.SSLEnabled,
		"provider":       rule.SSLProvider,
		"provider_label": proxies.SSLProviderLabel(rule.SSLProvider),
		"cert_path":      rule.SSLCert,
		"key_path":       rule.SSLKey,
		"expires":        rule.SSLExpires,
		"not_after":      "",
		"days_left":      -1,
		"domains":        []string{},
		"renew_hint":     "",
	}
	if !rule.SSLEnabled || rule.SSLCert == "" {
		return v
	}
	if names := certDomainNames(rule.SSLCert); len(names) > 0 {
		v["domains"] = names
	}
	if notAfter, err := tlsx.CertExpiry(rule.SSLCert); err == nil && !notAfter.IsZero() {
		days := int(time.Until(notAfter).Hours() / 24)
		v["not_after"] = notAfter.Format(time.RFC3339)
		v["expires"] = notAfter.Format("2006-01-02 15:04:05")
		v["days_left"] = days
		switch {
		case days < 0:
			v["renew_hint"] = "证书已过期，请立即续期或重新绑定"
		case days <= certRenewThresholdDays:
			v["renew_hint"] = fmt.Sprintf("证书将在 %d 天内到期", days)
		}
		return v
	}
	v["renew_hint"] = "无法读取证书文件（可能已被删除或权限不足）"
	return v
}

// certDomainNames 读取证书覆盖的域名（SAN，缺省退回 CN）。纯 Go 解析不起 openssl
// 子进程：列表页每次刷新都要显示，不值得 fork。
func certDomainNames(certPath string) []string {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return nil
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	names := append([]string{}, crt.DNSNames...)
	if len(names) == 0 && crt.Subject.CommonName != "" {
		names = append(names, crt.Subject.CommonName)
	}
	return names
}

func (s *Server) nginxInstalled() bool {
	if _, err := os.Stat(s.Cfg.NginxBin); err == nil {
		return true
	}
	return false
}

// portListening 判断本机某个端口是否有人在听。
func portListening(ctx context.Context, port int) bool {
	if port <= 0 {
		return false
	}
	d := net.Dialer{Timeout: 800 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// probeTarget 探一次目标是否可达（TCP 连接）并给出说明。只做 TCP、不发 HTTP：
// 目标 http/https 与 Host 头由 nginx 处理，这里只回答"这个地址到底通不通"。
func probeTarget(ctx context.Context, target string) (bool, string) {
	host, port, err := (&proxies.Rule{Target: target}).TargetHostPort()
	if err != nil {
		return false, err.Error()
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	start := time.Now()
	c, derr := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if derr != nil {
		return false, fmt.Sprintf("连不上 %s:%d（%v）", host, port, derr)
	}
	_ = c.Close()
	return true, fmt.Sprintf("可达 %s:%d（%dms）", host, port, time.Since(start).Milliseconds())
}

type proxyReq struct {
	Name         *string `json:"name"`
	Listen       *int    `json:"listen"`
	Domains      *string `json:"domains"`
	Path         *string `json:"path"`
	Target       *string `json:"target"`
	PreserveHost *bool   `json:"preserve_host"`
	Websocket    *bool   `json:"websocket"`
	Enabled      *bool   `json:"enabled"`
	Remark       *string `json:"remark"`

	// HTTPS 字段。**签发/选择证书不走这个接口**，走 POST /api/v1/proxies/{id}/ssl
	// （与站点侧 SSL Tab 对称）。带上它们是为了前端"关闭 HTTPS"能一次落库，
	// 以及允许高级用法直接指定已有证书路径。
	SSLEnabled  *bool   `json:"ssl_enabled"`
	SSLCert     *string `json:"ssl_cert"`
	SSLKey      *string `json:"ssl_key"`
	SSLProvider *string `json:"ssl_provider"`
	SSLExpires  *string `json:"ssl_expires"`

	// HTTPS 上游 SNI（留空自动推导）与"一键常用请求头"。
	TLSName         *string `json:"tls_name"`
	StandardHeaders *bool   `json:"standard_headers"`
	RedirectHTTP    *bool   `json:"redirect_http"`

	// 局域网出口三态：auto / on / off。刻意**不接受** forward_port 输入：那是转发器
	// 分配出来的回环端口，让前端指定会让 nginx 指向没人听的端口（或撞上别的服务）。
	LANForward *string `json:"lan_forward"`

	// AuthPassword 是**只写**字段：非空才更新哈希，空串表示沿用原密码（前端不回显）。
	// 响应里永远没有它，也没有哈希（见 Rule.AuthHash 的 json:"-"）。
	AuthEnabled  *bool   `json:"auth_enabled"`
	AuthUser     *string `json:"auth_user"`
	AuthPassword *string `json:"auth_password"`
}

func (req proxyReq) apply(rule *proxies.Rule) {
	if req.Name != nil {
		rule.Name = *req.Name
	}
	if req.Listen != nil {
		rule.Listen = *req.Listen
	}
	if req.Domains != nil {
		rule.Domains = *req.Domains
	}
	if req.Path != nil {
		rule.Path = *req.Path
	}
	if req.Target != nil {
		rule.Target = *req.Target
	}
	if req.PreserveHost != nil {
		rule.PreserveHost = *req.PreserveHost
	}
	if req.Websocket != nil {
		rule.Websocket = *req.Websocket
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.Remark != nil {
		rule.Remark = *req.Remark
	}
	if req.SSLCert != nil {
		rule.SSLCert = *req.SSLCert
	}
	if req.SSLKey != nil {
		rule.SSLKey = *req.SSLKey
	}
	if req.SSLProvider != nil {
		rule.SSLProvider = *req.SSLProvider
	}
	if req.SSLExpires != nil {
		rule.SSLExpires = *req.SSLExpires
	}
	if req.TLSName != nil {
		rule.TLSName = *req.TLSName
	}
	if req.StandardHeaders != nil {
		rule.StandardHeaders = *req.StandardHeaders
	}
	if req.RedirectHTTP != nil {
		rule.RedirectHTTP = *req.RedirectHTTP
	}
	if req.LANForward != nil {
		rule.LANForward = *req.LANForward
	}
	if req.SSLEnabled != nil {
		rule.SSLEnabled = *req.SSLEnabled
		// 关闭 HTTPS 时把证书字段一并清空：留着旧路径会让"已关闭"的规则
		// 在数据库里看起来还引用着某张证书（证书页也据此判断能否删除）。
		if !*req.SSLEnabled {
			rule.SSLCert = ""
			rule.SSLKey = ""
			rule.SSLProvider = ""
			rule.SSLExpires = ""
		}
	}
}

// handleProxyCreate 新建规则：先校验、查冲突、再落库并应用。
func (s *Server) handleProxyCreate(w http.ResponseWriter, r *http.Request) {
	var req proxyReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	rule := &proxies.Rule{Listen: 80, Websocket: true, Enabled: true}
	req.apply(rule)
	if err := rule.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 访问鉴权先解析并校验（此时还没建库）：缺用户名/密码就在建规则之前拒绝，
	// 不留下"规则建好了、鉴权没生效"的半成品。
	authCfg, aerr := proxyAuthFromRequest(req, proxyAuthConfig{})
	if aerr != nil {
		fail(w, http.StatusBadRequest, aerr.Error())
		return
	}
	if !s.nginxInstalled() {
		fail(w, http.StatusConflict, "反向代理需要 nginx：请先到「应用市场 → 网站环境」安装 nginx")
		return
	}
	if other, cerr := s.proxyRepo().ConflictWith(r.Context(), rule); cerr == nil && other != nil {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"和已有规则「%s」（同端口 %d、同域名/路径）冲突：nginx 只会用先加载的那一条，"+
				"请改端口、域名或路径", other.Name, other.Listen))
		return
	}
	if err := s.checkProxySSLPortMix(r.Context(), rule); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 与**站点 vhost** 的冲突：规则还没写盘，所以 selfFile 传空。
	if err := s.checkProxyAgainstSiteVhosts(rule, ""); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	created, err := s.proxyRepo().Create(r.Context(), rule)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 鉴权配置先落库：applyProxy 生成配置时从 settings 把它装回 Rule。
	if err := s.saveProxyAuth(r.Context(), created.ID, authCfg); err != nil {
		_ = s.proxyRepo().Delete(r.Context(), created.ID)
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 后续任何一步失败都会把规则删掉，鉴权配置与它生成的 htpasswd 文件也要一起清掉。
	cleanupCreatedAuth := func() {
		_ = s.saveProxyAuth(r.Context(), created.ID, proxyAuthConfig{})
		_ = os.Remove(s.proxyAuthFile(created.ID))
	}
	// 先起回环转发器（如果需要），再生成 nginx 配置：proxy_pass 里的端口是
	// 转发器分配出来的，顺序反了就会写成一个没人听的端口。
	if err := s.syncForwarder(created); err != nil {
		s.forwarders.Stop(created.ID)
		_ = s.proxyRepo().Delete(r.Context(), created.ID)
		cleanupCreatedAuth()
		fail(w, http.StatusBadGateway, "规则已保存但回环转发器无法启动："+err.Error())
		return
	}
	if created.ForwardPort > 0 {
		if err := s.proxyRepo().SetForwardPort(r.Context(), created.ID, created.ForwardPort); err != nil {
			s.forwarders.Stop(created.ID)
			_ = s.proxyRepo().Delete(r.Context(), created.ID)
			cleanupCreatedAuth()
			fail(w, http.StatusInternalServerError, "回环转发端口落库失败："+err.Error())
			return
		}
	}
	if err := s.applyProxy(r.Context(), created); err != nil {
		// 配置写不进去就把记录删掉，避免留下一条"看着在、其实没生效"的规则
		s.forwarders.Stop(created.ID)
		_ = s.proxyRepo().Delete(r.Context(), created.ID)
		cleanupCreatedAuth()
		fail(w, http.StatusBadGateway, "规则已保存但 nginx 配置应用失败："+err.Error())
		return
	}
	detail := fmt.Sprintf("%d → %s", created.Listen, created.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_create", created.Name,
			"规则已创建并生效（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_create", created.Name, detail, true, "")
	ok(w, s.proxyView(r.Context(), created))
}

// handleProxyUpdate 修改规则：先写新配置，成功后再落库。
func (s *Server) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req proxyReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	next := *cur
	req.apply(&next)
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 访问鉴权：在旧配置基础上合并本次请求（空密码 = 沿用原密码）。
	prevAuth, perr := s.loadProxyAuth(r.Context(), id)
	if perr != nil {
		fail(w, http.StatusInternalServerError, perr.Error())
		return
	}
	authCfg, aerr := proxyAuthFromRequest(req, prevAuth)
	if aerr != nil {
		fail(w, http.StatusBadRequest, aerr.Error())
		return
	}
	if other, cerr := repo.ConflictWith(r.Context(), &next); cerr == nil && other != nil {
		fail(w, http.StatusConflict, fmt.Sprintf(
			"和规则「%s」（同端口 %d、同域名/路径）冲突，nginx 只会用先加载的那一条", other.Name, other.Listen))
		return
	}
	if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 与**站点 vhost** 的冲突（跳过这份规则自己的文件）。
	if err := s.checkProxyAgainstSiteVhosts(&next, fmt.Sprintf("proxy-%d.conf", id)); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 先按新配置写盘（含 nginx -t 校验、失败回滚），成功后才更新数据库：反过来的话
	// 配置写失败会留下"数据库说已改、文件还是旧的"的不一致。
	next.ID = cur.ID
	// 转发器先对齐（起/停/换目标；含不需要转发时把 ForwardPort 清 0），
	// 这样紧接着生成的 proxy_pass 才会用对端口。
	if err := s.syncForwarder(&next); err != nil {
		fail(w, http.StatusBadGateway, "回环转发器无法启动（配置未改动）："+err.Error())
		return
	}
	// SSL 开关切换时，该端口的域名兜底块必须先进入"带证书的中性形态"，
	// 否则中间态会被 nginx 判成 [emerg]（见 stabilizeProxyReject）。
	if next.SSLEnabled != cur.SSLEnabled && next.Enabled {
		cert, key := next.SSLCert, next.SSLKey
		if cert == "" || key == "" {
			cert, key = cur.SSLCert, cur.SSLKey
		}
		if err := s.stabilizeProxyReject(r.Context(), next.Listen, cert, key, next.SSLEnabled); err != nil {
			_ = s.syncForwarder(cur) // 转发器退回旧状态
			fail(w, http.StatusBadGateway, "切换 HTTPS 时调整域名兜底块失败："+err.Error())
			return
		}
	}
	// 鉴权配置先落库再写盘：applyProxy 从 settings 读它生成 auth_basic。
	// 写盘失败时把鉴权配置恢复成旧值，再回滚旧配置（否则回滚出的 vhost 会带着新密码）。
	if err := s.saveProxyAuth(r.Context(), id, authCfg); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.applyProxy(r.Context(), &next); err != nil {
		_ = s.saveProxyAuth(r.Context(), id, prevAuth) // 鉴权配置回滚
		_ = s.applyProxy(r.Context(), cur)             // 回滚成旧配置
		_ = s.syncForwarder(cur)                       // 转发器也跟着回滚（含端口）
		_ = s.syncRejectBlocks(r.Context())            // 兜底块也回到数据库描述的状态
		fail(w, http.StatusBadGateway, "应用新配置失败（已回滚）："+err.Error())
		return
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail := fmt.Sprintf("%d → %s", saved.Listen, saved.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_update", saved.Name,
			"规则已保存并生效（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_update", saved.Name, detail, true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// handleProxyDelete 删除规则：先删配置文件并 reload，再删记录。
func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.removeProxyConfig(r.Context(), cur); err != nil {
		fail(w, http.StatusBadGateway, "删除 nginx 配置失败："+err.Error())
		return
	}
	if err := repo.Delete(r.Context(), id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 规则没了，回环监听器也必须跟着消失：留着就是"删了规则却还开着端口"。
	s.forwarders.Stop(id)
	// 鉴权配置与 htpasswd 文件也一起清掉：留着一份没人引用的口令哈希没有意义。
	if err := s.saveProxyAuth(r.Context(), id, proxyAuthConfig{}); err != nil && s.Log != nil {
		s.Log.Warn("清理规则 %d 的鉴权配置失败：%v", id, err)
	}
	_ = os.Remove(s.proxyAuthFile(id))
	detail := fmt.Sprintf("%d → %s", cur.Listen, cur.Target)
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_delete", cur.Name,
			"规则已删除（"+detail+"）", err)
		return
	}
	s.audit(r, "proxy_delete", cur.Name, detail, true, "")
	ok(w, map[string]any{"msg": "已删除规则「" + cur.Name + "」并移除它的 nginx 配置"})
}

// rejectGuardFailed 把"域名兜底拒绝块没生效"如实上报为非 2xx，并写清"什么已经成功、
// 什么没生效"。绝不再像以前那样只写 detail 仍返回 200 —— 配置写了没被 nginx 加载
// 却说成功，是本项目最贵的教训。
func (s *Server) rejectGuardFailed(w http.ResponseWriter, r *http.Request,
	action, name, done string, err error) {
	s.audit(r, action, name, done+"（域名兜底拒绝块未生效: "+err.Error()+"）", false, err.Error())
	fail(w, http.StatusBadGateway, done+"，但域名兜底拒绝块没有生效："+err.Error()+
		"。副作用：域名对不上的请求可能被转发到后端，请修正后重试")
}

// handleProxyToggle 启用/停用一条规则。停用 = 删配置文件再 reload（而不是注释掉配置）：
// nginx 不加载它就一定不生效，而"注释掉"更容易写错。
func (s *Server) handleProxyToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := *cur
	next.Enabled = !cur.Enabled
	if next.Enabled {
		if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		// 启用时先起转发器（可能在停用期间被停掉了），再写配置。
		if err := s.syncForwarder(&next); err != nil {
			fail(w, http.StatusBadGateway, "启动回环转发器失败："+err.Error())
			return
		}
		if err := s.applyProxy(r.Context(), &next); err != nil {
			// 配置没写成 → 转发器也不能留着（否则会有一个监听器对应一条"没启用"的规则）
			s.forwarders.Stop(next.ID)
			fail(w, http.StatusBadGateway, "启用失败："+err.Error())
			return
		}
	} else {
		if err := s.removeProxyConfig(r.Context(), &next); err != nil {
			fail(w, http.StatusBadGateway, "停用失败："+err.Error())
			return
		}
		// 停用就关掉监听器（ForwardPort 保留，重新启用时复用同一个端口）。
		s.forwarders.Stop(next.ID)
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	action := "已停用"
	if saved.Enabled {
		action = "已启用"
	}
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_toggle", saved.Name,
			"规则"+action+"并已生效", err)
		return
	}
	s.audit(r, "proxy_toggle", saved.Name, action, true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// handleProxyTest 在保存前试一次目标可达性（界面上的「测试连通」按钮），不落库。
// 同时承接列表状态的异步探测：{all:true} 或 {ids:[...]} 时并发探测（每条 ≤500ms）并写进
// TTL 缓存；两种用法共用一个路由，是为了不新增路由（server.go 不在本轮改动范围）。
func (s *Server) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string  `json:"target"`
		IDs    []int64 `json:"ids"`
		All    bool    `json:"all"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.All || len(req.IDs) > 0 {
		list, err := s.proxyRepo().List(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(req.IDs) > 0 {
			want := make(map[int64]bool, len(req.IDs))
			for _, id := range req.IDs {
				want[id] = true
			}
			filtered := make([]*proxies.Rule, 0, len(list))
			for _, rule := range list {
				if want[rule.ID] {
					filtered = append(filtered, rule)
				}
			}
			list = filtered
		}
		res := s.probeProxyStatuses(r.Context(), list)
		items := make([]map[string]any, 0, len(list))
		for _, rule := range list {
			st := res[rule.ID]
			items = append(items, map[string]any{
				"id":             rule.ID,
				"enabled":        rule.Enabled,
				"port_listening": st.Listening,
				"target_ok":      st.Reachable,
				"target_detail":  st.Detail,
				"probed_at":      st.At.Format(time.RFC3339),
			})
		}
		ok(w, map[string]any{
			"list": items,
			// 全局检测时间：界面显示"约 N 秒前检测"时以它为准（每条的 probed_at 相同）。
			"probed_at":   time.Now().Format(time.RFC3339),
			"ttl_seconds": int(proxyStatusTTL.Seconds()),
		})
		return
	}
	reachable, detail := probeTarget(r.Context(), strings.TrimSpace(req.Target))
	ok(w, map[string]any{"ok": reachable, "detail": detail})
}

// 反代「大请求体探测」——按需触发，绝不进列表/首屏（坑 165）。2026-09-18 事故中经反代推
// 368KB 样本一律 500，而那是 nginx **自己**的错误页（请求体缓冲到 client_body_temp，目录
// 不可写就在转给上游之前失败）；这里真的发 ~64KB 请求体并断言拿到的不是 nginx 自己的 500/413 页。

const (
	// proxyBodyProbeSize 是探测请求体大小（64KB）：远大于 nginx 默认 client_body_buffer_size
	// （8k/16k），一定触发"请求体缓冲"路径；又足够小，不会把上游或日志撑爆。
	proxyBodyProbeSize = 64 * 1024
	// proxyBodyProbeTimeout 是单次探测的整体超时。
	proxyBodyProbeTimeout = 6 * time.Second
)

// proxyBodyPostFn 是"发一个带请求体的 POST"的可注入点：生产走 curl，单测替换它
// 就能钉住分类判据（尤其是"nginx 回它自己的 500 页 → 判失败"）。
var proxyBodyPostFn = curlPostBody

// proxyProbeListeningFn 是探测前的"端口在不在听"检查，可注入让单测不必真拨号。
var proxyProbeListeningFn = portListening

// curlPostBody 用 curl 向规则的监听端口 POST 一个请求体，返回 (状态码, 响应体, 错误)。
// 与 curlSite 同一套做法：--resolve 把 Host 钉到 127.0.0.1、-k 跳过证书校验；显式发空的
// `Expect:` 头，否则 curl 会先等 100-continue，请求体根本不会被发出去。
func curlPostBody(ctx context.Context, scheme, host string, port int, path string, body []byte, timeout time.Duration) (code, respBody string, err error) {
	url := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, path)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{
		"-sS", "-k", "--max-time", strconv.Itoa(int(timeout.Seconds())),
		"--resolve", fmt.Sprintf("%s:%d:127.0.0.1", host, port),
		"-H", "Content-Type: application/octet-stream",
		"-H", "Expect:",
		"--data-binary", "@-",
		"-o", "-", "-w", "\n__ZP_CODE__%{http_code}",
		url,
	}
	cmd := execCommand(ctx, "/usr/bin/curl", args...)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return "000", "", err
	}
	s := string(out)
	if i := strings.LastIndex(s, "__ZP_CODE__"); i >= 0 {
		code = strings.TrimSpace(s[i+len("__ZP_CODE__"):])
		s = s[:i]
	}
	return code, s, nil
}

// proxyBodyProbeResult 是一次大请求体探测的结果（字段都给前端展示 / 排障用）。
type proxyBodyProbeResult struct {
	Status     string `json:"status"` // ok / bad / unknown
	OK         bool   `json:"ok"`
	Step       string `json:"step"`   // 坏 / 未能探测发生在哪一步
	Detail     string `json:"detail"` // 给用户看的完整说明
	Code       string `json:"code"`   // HTTP 状态码；"000" = 没拿到响应
	Listen     int    `json:"listen"`
	Target     string `json:"target"`
	ProbeURL   string `json:"probe_url"`
	BodyBytes  int    `json:"body_bytes"`
	BodyPrefix string `json:"body_prefix"` // 响应体开头（排障用；已截断）
}

// looksLikeNginxErrorPage 判断响应体是不是 nginx **自己**生成的错误页：出厂错误页固定
// 带一行 `<hr><center>nginx</center>`。判据刻意很窄（必须同时出现 "nginx" 与出厂页标记）
// —— 宁可漏判成"上游 5xx"，也不要把用户应用自己的 500 页误报成 nginx 的请求体失败。
func looksLikeNginxErrorPage(body string) bool {
	low := strings.ToLower(body)
	if !strings.Contains(low, "nginx") {
		return false
	}
	return strings.Contains(low, "<center>nginx</center>") ||
		strings.Contains(low, "<center>nginx/")
}

// classifyBodyProbe 把一次响应归类成 好 / 坏 / 未能探测，并写清发生在哪一步：413 → 坏（被
// client_max_body_size 拒）；502/504 → 坏（连不上上游，须排在"nginx 自己的错误页"之前）；
// 5xx 且是 nginx 自己的错误页 → 坏（2026-09-18 事故形态）；其它响应 → 好；没拿到响应 → 未能探测。
func classifyBodyProbe(code, body string, listen int) (status, step, detail string) {
	c := strings.TrimSpace(code)
	switch {
	case c == "" || c == "000":
		return "unknown", "发送请求", "没有拿到任何 HTTP 响应（连接被拒 / 被断开 / 超时）"
	case c == "413":
		return "bad", "nginx 请求体上限",
			"nginx 用 413 拒绝了请求体：它超过了 client_max_body_size（可到「设置 → 上传与执行限制」调大）"
	case c == "502" || c == "504":
		return "bad", "转发到上游",
			"nginx 收下了请求体，但转发到上游时得到 " + c + "（目标服务没起来 / 连不上）"
	case strings.HasPrefix(c, "5") && looksLikeNginxErrorPage(body):
		return "bad", "nginx 自身的错误页",
			"nginx 返回了它**自己**的 " + c + " 错误页：请求体在转给上游之前就失败了" +
				"（典型原因是 client_body_temp 不可写或磁盘满）"
	case listen == 80 && strings.Contains(body, sites.LocalhostIndexMarker):
		return "unknown", "落到默认站点",
			"请求落到了面板默认站点（000-default.conf）的占位页，说明这条反代规则没有被 nginx 使用"
	default:
		return "ok", "", "64KB 请求体已被 nginx 接收并转发，上游返回 HTTP " + c
	}
}

// truncateProbeBody 只保留响应体开头，避免把巨大页面塞进接口响应。
func truncateProbeBody(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…（已截断）"
	}
	return s
}

// probeProxyLargeBody 对一条规则做一次"大请求体"探测，结果如实分类：每一步失败都写清
// 在哪一步，绝不把"没探到"说成"通过"。
func (s *Server) probeProxyLargeBody(ctx context.Context, rule *proxies.Rule) proxyBodyProbeResult {
	res := proxyBodyProbeResult{Status: "unknown", BodyBytes: proxyBodyProbeSize}
	if rule != nil {
		res.Listen, res.Target = rule.Listen, rule.Target
	}
	if rule == nil {
		res.Step, res.Detail = "前置检查", "规则不存在"
		return res
	}
	host := proxyProbeHost(rule)
	scheme := probeProxyScheme(rule)
	path := proxyProbePath(rule)
	res.ProbeURL = fmt.Sprintf("%s://%s:%d%s", scheme, host, rule.Listen, path)

	if !rule.Enabled {
		res.Step = "前置检查"
		res.Detail = "规则已停用：nginx 里没有它的配置，无从探测（先启用这条规则）"
		return res
	}
	if !s.nginxInstalled() {
		res.Step = "前置检查"
		res.Detail = "没有找到 nginx（" + s.Cfg.NginxBin + "）：请先到「应用市场 → 网站环境」安装 nginx"
		return res
	}
	if !proxyProbeListeningFn(ctx, rule.Listen) {
		res.Step = "前置检查"
		res.Detail = fmt.Sprintf("127.0.0.1:%d 没有在监听：nginx 没起来、或这条规则没被加载"+
			"（先看规则卡片上的端口状态与 nginx error_log）", rule.Listen)
		return res
	}

	payload := bytes.Repeat([]byte("z"), proxyBodyProbeSize)
	code, body, err := proxyBodyPostFn(ctx, scheme, host, rule.Listen, path, payload, proxyBodyProbeTimeout)
	res.Code = strings.TrimSpace(code)
	res.BodyPrefix = truncateProbeBody(body)
	if err != nil && (res.Code == "" || res.Code == "000") {
		res.Step = "发送请求"
		res.Detail = "未能探测：向 " + res.ProbeURL + " 发送 64KB 请求体时失败：" + err.Error()
		return res
	}
	status, step, detail := classifyBodyProbe(res.Code, body, rule.Listen)
	res.Status, res.Step, res.Detail, res.OK = status, step, detail, status == "ok"
	if err != nil {
		res.Detail += "（curl: " + err.Error() + "）"
	}
	return res
}

// handleProxyBodyProbe 是「大请求体探测」的按需接口：要真的发一个 64KB 请求体，属于
// 昂贵且会碰到真实服务的探测，绝不能挂在列表 / 首屏渲染路径上（AGENTS.md 坑 165）。
func (s *Server) handleProxyBodyProbe(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	rule, err := s.proxyRepo().Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, s.probeProxyLargeBody(r.Context(), rule))
}

// 反向代理 SSL：与「网站管理」的 SSL Tab 同一套体验，挂在反代规则上。self/mkcert/manual 把证书
// 放到 <DataDir>/proxy-certs/<规则>/；acme **直接引用面板证书库路径**，绝不复制（续期是同路径
// 覆盖，复制会让续期后线上还是旧证书）。写盘 → reload → 请求级复核，不通过一律非 2xx。

// proxySSLReq 是 POST /api/v1/proxies/{id}/ssl 的请求体。字段与站点侧 siteSSLReq
// 刻意保持一致（provider/cert/key/extra_san/cert_primary/domain）。
type proxySSLReq struct {
	Provider string   `json:"provider"` // self / mkcert / manual / acme
	Cert     string   `json:"cert"`
	Key      string   `json:"key"`
	ExtraSAN []string `json:"extra_san"`

	// CertPrimary 指定要绑定的 ACME 证书（primary 名）；不给时按规则域名自动匹配。
	CertPrimary string `json:"cert_primary"`
	// Domain 是 cert_primary 的容错写法：前端直接给一个域名也能匹配。
	Domain string `json:"domain"`
}

// proxyCertHosts 返回给自签 / mkcert 用的域名列表。规则可以没有域名，那种情况下没有
// 可放进 SAN 的名字，用 127.0.0.1 兜底（两者都能签 IP），至少不生成一张空 SAN 的证书。
func proxyCertHosts(rule *proxies.Rule) []string {
	domains := proxies.SplitDomains(rule.Domains)
	if len(domains) == 0 {
		return []string{"127.0.0.1"}
	}
	return domains
}

// proxyCertDir 是 self/mkcert/manual 三种来源的证书目录：用 VhostName()（proxy-<id>）
// 而不是规则名 —— 规则名是中文且可能重复，做目录名既危险又不稳。
func (s *Server) proxyCertDir(rule *proxies.Rule) string {
	return filepath.Join(s.Cfg.DataDir, "proxy-certs", rule.VhostName())
}

// alignProxyCertOwner 把证书目录属主对齐 DataDir（与 acme/store.go 同一个真机坑）：root 用
// 0600 写出的私钥、以普通用户运行的 nginx 读不到（`nginx -t` 报 Permission denied、443 连不上）。
// 只对齐属主、不放宽权限；失败只告警，绑定是否真成功由随后的请求级 TLS 复核判定。
func (s *Server) alignProxyCertOwner(dir string) {
	if dir == "" || os.Geteuid() != 0 {
		return
	}
	fi, err := os.Stat(s.Cfg.DataDir)
	if err != nil {
		s.Log.Warn("读不到数据目录属主（%v），反代证书属主未调整（nginx 可能读不到证书）", err)
		return
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	uid, gid := int(st.Uid), int(st.Gid)
	if err := os.Chown(dir, uid, gid); err != nil {
		s.Log.Warn("调整 %s 属主失败：%v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := os.Chown(filepath.Join(dir, e.Name()), uid, gid); err != nil {
			s.Log.Warn("调整 %s 属主失败：%v", filepath.Join(dir, e.Name()), err)
		}
	}
}

// handleProxySSL 为一条反代规则签发/绑定证书（四种来源与站点侧完全一致）。先把新配置
// 写进 nginx（含请求级复核），成功后才落库 —— 避免"数据库说开了 HTTPS、nginx 其实没
// 生效"；失败时尽力把兜底块恢复成数据库描述的状态，并返回非 2xx。
func (s *Server) handleProxySSL(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !s.nginxInstalled() {
		fail(w, http.StatusConflict, "HTTPS 由 nginx 提供：请先到「应用市场 → 网站环境」安装 nginx")
		return
	}
	var req proxySSLReq
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		certPath string
		keyPath  string
		expires  string
	)
	switch req.Provider {
	case "self", "mkcert", "manual":
		certDir := s.proxyCertDir(cur)
		if err := os.MkdirAll(certDir, 0o755); err != nil {
			fail(w, http.StatusInternalServerError, "创建证书目录失败: "+err.Error())
			return
		}
		certPath = filepath.Join(certDir, "fullchain.pem")
		keyPath = filepath.Join(certDir, "privkey.pem")
		hosts := proxyCertHosts(cur)
		switch req.Provider {
		case "self":
			if _, err := s.callHelper(r.Context(), "site-cert-self",
				"--domain", hosts[0], "--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError, "签发自签证书失败: "+err.Error())
				return
			}
		case "mkcert":
			allHosts := append(append([]string{}, hosts...), req.ExtraSAN...)
			if _, err := s.callHelper(r.Context(), "mkcert-issue",
				"--hosts", strings.Join(allHosts, ","),
				"--cert", certPath, "--key", keyPath); err != nil {
				fail(w, http.StatusInternalServerError,
					"mkcert 签发失败: "+err.Error()+"（可先执行 brew install mkcert nss && mkcert -install）")
				return
			}
		case "manual":
			if strings.TrimSpace(req.Cert) == "" || strings.TrimSpace(req.Key) == "" {
				fail(w, http.StatusBadRequest, "手工模式需要提供证书与私钥内容")
				return
			}
			if err := os.WriteFile(certPath, []byte(req.Cert), 0o644); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := os.WriteFile(keyPath, []byte(req.Key), 0o600); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// nginx 以真实用户运行，root 写出的 0600 私钥它读不到 → 对齐属主。
		s.alignProxyCertOwner(certDir)
		notAfter, cerr := tlsx.CertExpiry(certPath)
		if cerr != nil || notAfter.IsZero() {
			fail(w, http.StatusBadRequest, "证书文件无法解析（"+certPath+"）："+errString(cerr)+
				"；请确认粘贴的是 PEM 格式的证书（fullchain）")
			return
		}
		expires = notAfter.Format("2006-01-02 15:04:05")
	case "acme":
		cert, merr := s.matchCertForProxy(cur, req)
		if merr != nil {
			// 400 而不是 500：这是"还没申请证书"这种可预期的用户状态。
			fail(w, http.StatusBadRequest, merr.Error())
			return
		}
		certPath, keyPath = cert.CertPath, cert.KeyPath
		expires = cert.NotAfter.Format("2006-01-02 15:04:05")
		if !dirExists(filepath.Dir(certPath)) || !fileExists(certPath) || !fileExists(keyPath) {
			fail(w, http.StatusBadRequest,
				"证书记录存在但文件缺失（"+certPath+"）：请在「证书」页重新申请或续期后再绑定")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "不支持的证书来源: "+req.Provider)
		return
	}

	next := *cur
	next.SSLEnabled = true
	next.SSLCert = certPath
	next.SSLKey = keyPath
	next.SSLProvider = req.Provider
	next.SSLExpires = expires
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.checkProxySSLPortMix(r.Context(), &next); err != nil {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	// 兜底块先带上证书行：同时改规则 vhost 与兜底块时，中间态若被 nginx 判
	// [emerg] 会让整个切换必然失败（见 stabilizeProxyReject）。
	if err := s.stabilizeProxyReject(r.Context(), next.Listen, next.SSLCert, next.SSLKey, next.SSLEnabled); err != nil {
		fail(w, http.StatusBadGateway, "调整域名兜底块失败："+err.Error())
		return
	}
	// 证书操作会重写整份 vhost，这里顺手确认转发器还在（幂等，通常零成本）。
	if err := s.syncForwarder(&next); err != nil {
		fail(w, http.StatusBadGateway, "回环转发器无法启动："+err.Error())
		return
	}
	if err := s.applyProxy(r.Context(), &next); err != nil {
		_ = s.syncRejectBlocks(r.Context()) // 兜底块回到数据库描述的状态
		fail(w, http.StatusBadGateway, "证书已就绪，但 nginx 配置应用失败（未生效）："+err.Error())
		return
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_ssl", saved.Name,
			"规则「"+saved.Name+"」的证书已绑定并生效", err)
		return
	}
	s.audit(r, "proxy_ssl", saved.Name, "绑定证书 provider="+req.Provider+" 到期="+expires, true, "")
	view := s.proxyView(r.Context(), saved)
	ok(w, map[string]any{
		"msg":            "HTTPS 已启用",
		"rule":           saved,
		"cert":           certPath,
		"key":            keyPath,
		"expires":        expires,
		"provider":       req.Provider,
		"provider_label": proxies.SSLProviderLabel(req.Provider),
		"days_left":      siteSSLDaysLeft(certPath),
		"ssl":            view["ssl"],
	})
}

// handleProxySSLDisable 关闭一条反代规则的 HTTPS。与站点侧不同：关掉 SSL 后规则**仍然
// 监听原端口**（只是回到 HTTP），不做"额外在 80 上 301"那种隐式行为。
func (s *Server) handleProxySSLDisable(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "规则 id 不合法")
		return
	}
	repo := s.proxyRepo()
	cur, err := repo.Get(r.Context(), id)
	if errors.Is(err, proxies.ErrNotFound) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	next := *cur
	next.SSLEnabled = false
	next.SSLCert = ""
	next.SSLKey = ""
	next.SSLProvider = ""
	next.SSLExpires = ""
	if err := next.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if cur.SSLEnabled {
		// 中性形态：兜底块先带证书，规则 vhost 改回非 SSL 后再收敛掉证书行。
		// 这里 next.SSLEnabled=false（正在关 HTTPS），所以 listen 不带 ssl。
		if err := s.stabilizeProxyReject(r.Context(), next.Listen, cur.SSLCert, cur.SSLKey, false); err != nil {
			fail(w, http.StatusBadGateway, "关闭 HTTPS 时调整域名兜底块失败："+err.Error())
			return
		}
	}
	if next.Enabled {
		if err := s.syncForwarder(&next); err != nil {
			fail(w, http.StatusBadGateway, "回环转发器无法启动："+err.Error())
			return
		}
		if err := s.applyProxy(r.Context(), &next); err != nil {
			_ = s.applyProxy(r.Context(), cur)  // 回滚成仍启用 SSL 的配置
			_ = s.syncRejectBlocks(r.Context()) // 兜底块也回到数据库描述的状态
			fail(w, http.StatusBadGateway, "关闭 HTTPS 失败（已回滚）："+err.Error())
			return
		}
	}
	saved, err := repo.Update(r.Context(), &next)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.syncRejectBlocks(r.Context()); err != nil {
		s.rejectGuardFailed(w, r, "proxy_ssl_disable", saved.Name,
			"规则「"+saved.Name+"」已关闭 HTTPS", err)
		return
	}
	s.audit(r, "proxy_ssl_disable", saved.Name, "关闭 HTTPS", true, "")
	ok(w, s.proxyView(r.Context(), saved))
}

// matchCertForProxy 为一条反代规则找到要用的 ACME 证书（复用站点侧 matchCertForNames）：
// 显式 cert_primary/domain 优先，其次按规则域名精确命中，再退到通配。匹配不到就明确报错
// 并指路，**不在这里就地签发**（签发是长任务，必须走任务中心）。
func (s *Server) matchCertForProxy(rule *proxies.Rule, req proxySSLReq) (*acme.Cert, error) {
	names := proxies.SplitDomains(rule.Domains)
	label := strings.Join(names, "、")
	if label == "" {
		label = "该端口上的任意域名"
	}
	return s.matchCertForNames(names, req.CertPrimary, req.Domain, label)
}

// proxiesUsingCert 返回正在引用该证书的反向代理规则（给证书页展示与删除保护用）。
// 判定同样用**证书路径**而不是域名：acme 续期是同路径覆盖，只有路径能精确对应。
func (s *Server) proxiesUsingCert(c *acme.Cert) []string {
	if c == nil || c.CertPath == "" {
		return []string{}
	}
	list, err := s.proxyRepo().List(context.Background())
	if err != nil {
		return []string{}
	}
	var out []string
	for _, r := range list {
		if r == nil || !r.SSLEnabled || r.SSLCert == "" {
			continue
		}
		if filepath.Clean(r.SSLCert) == filepath.Clean(c.CertPath) {
			out = append(out, "反向代理「"+r.Name+"」")
		}
	}
	return out
}

// 反代落盘通道：chown 日志树 → reload → 请求级复核。必须先 chown 再 reload：写 vhost 的提权助手以
// **root** 跑 `nginx -t`，而 `-t` 会把日志文件创建成 root 属主 → 以真实用户运行的 nginx 打不开，
// reload 没加载新配置**但退出码仍是 0**（"报成功却没生效"的根因）；复核就看该规则自己的访问日志增长。

// proxyProbeFn / proxyReloadFn / proxyChownLogsFn 是可注入步骤：
// 生产环境指向真实实现，单测里替换它们，避免碰真实服务与真实 nginx。
var (
	// proxyProbeFn 做一次请求级探测（生产 = curlSite，用 --resolve 钉到 127.0.0.1）。
	proxyProbeFn = curlSite
	// proxyReloadFn 重载 nginx（生产 = s.nginxReload）。注意：它内部**只有** `nginx -s reload`，
	// 不做 `nginx -t`、不 chown、不复核（`-t` 是 writeVhost 助手以 root 跑的），
	// 所以 chown 与复核必须由本文件补上。
	proxyReloadFn = func(s *Server, ctx context.Context) error { return s.nginxReload(ctx) }
	// proxyChownLogsFn 把 nginx 日志树递归交还真实用户。
	proxyChownLogsFn = func(s *Server) { s.chownProxyLogTrees() }
	// proxyWriteVhostFn 写 vhost（生产 = s.writeVhost，helper 内含 nginx -t）。做成变量
	// 只为让单测能覆盖"写盘 → chown → reload → 复核"这条顺序（与 api_sites.go 同一做法）。
	proxyWriteVhostFn = func(s *Server, ctx context.Context, name, content string) error {
		return s.writeVhost(ctx, name, content)
	}
)

// proxyVerifyWait / proxyVerifyEvery / proxyLogSettle 控制复核的等待窗口：reload 只是给 master
// 发信号，新 server 块生效有短延迟；日志又是 worker 在响应之后写的，读大小前要留落盘时间。
// 与站点侧同一口径：间隔 200ms、总窗口 6 秒，两者都可注入（单测不许真睡 6 秒）。
var (
	proxyVerifyWait  = 6 * time.Second
	proxyVerifyEvery = 200 * time.Millisecond
	proxyLogSettle   = 150 * time.Millisecond
)

// proxyProbeTimeout 是单次请求级探测的超时。不能太长：面板要在这个请求里等复核。
const proxyProbeTimeout = 4 * time.Second

// proxyProbe 是一次请求级复核拿到的原始结果。
type proxyProbe struct {
	code string // HTTP 状态码；"000" = 没拿到响应（连不上 / 被 444 断开 / 超时）
	body string
	err  error
}

// chownTreeToUser 把一棵树递归交给面板的"真实用户"：抽出来是为了让反代各条路径只有
// 一种改归属的写法（判据与 applySite 一致：必须是 root、必须是具体用户）。
func (s *Server) chownTreeToUser(path string) {
	if path == "" || s.Cfg.User == "" || s.Cfg.User == "root" || os.Geteuid() != 0 {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	_ = chownTreeTo(path, s.Cfg.User)
}

// chownProxyLogTrees 把 nginx 需要写的日志目录**递归**交给真实用户：必须在 writeVhost
// （内部以 root 跑 `nginx -t`，会创建 root 属主的日志文件）之后、`-s reload` 之前调用。
// 覆盖整棵树而不是只改目录本身 —— 触发故障的正是 `nginx -t` 新建出来的 proxy-*.access.log。
func (s *Server) chownProxyLogTrees() {
	for _, root := range s.proxyLogRoots() {
		s.chownTreeToUser(root)
	}
}

// proxyLogRoots 是反代需要写的日志目录（去重后按"最具体 → 最大"排列）。
func (s *Server) proxyLogRoots() []string {
	roots := make([]string, 0, 3)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, r := range roots {
			if r == p {
				return
			}
		}
		roots = append(roots, p)
	}
	add(filepath.Join(s.Cfg.LogRoot, "proxy")) // 反代规则自己的日志
	add(s.Cfg.LogRoot)                         // 站点 / 默认站点日志的那棵树
	add(filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx"))
	return roots
}

// proxyAccessLogPath 是 Rule.Generate 写进 vhost 的那条 access_log 的路径：命名必须与
// internal/proxies 的 `proxy-<id>.access.log` 一致 —— 复核就靠"它有没有长出新内容"。
func proxyAccessLogPath(logDir string, id int64) string {
	return filepath.Join(logDir, fmt.Sprintf("proxy-%d.access.log", id))
}

// proxyLogSize 返回日志文件大小；不存在按 0 处理（只看"有没有增长"）。
func proxyLogSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// proxyProbePath 是复核要请求的路径：必须落在规则的 location 前缀里。
func proxyProbePath(rule *proxies.Rule) string {
	if p := strings.TrimSpace(rule.Path); p != "" {
		return p
	}
	return "/"
}

// proxyProbeHost 选一个"能命中该规则"的 Host：通配域名（*.example.com）要换成一个能匹配
// 它的具体名字，否则请求会落到兜底拒绝块上，把"配置正常"误判成失败。没有域名时
// internal/proxies 生成的是字面的 `_`，必须原样发 `Host: _` 才能命中。
func proxyProbeHost(rule *proxies.Rule) string {
	for _, d := range proxies.SplitDomains(rule.Domains) {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "*.") {
			return "zp-probe." + d[2:]
		}
		if strings.Contains(d, "*") {
			continue // 其它通配写法无法稳定构造出一个匹配的 Host
		}
		return d
	}
	return "_"
}

// probeProxyScheme 决定复核该发 HTTP 还是 HTTPS：启用 SSL 的规则只监听 TLS，用 http://
// 探测只会拿到 "000"（连接被重置），从而把一条正常规则误判成"没生效"。
func probeProxyScheme(rule *proxies.Rule) string {
	if rule.SSLEnabled {
		return "https"
	}
	return "http"
}

// probeProxyOnce 向规则的监听端口发一次请求（Host 由调用方指定）。
func (s *Server) probeProxyOnce(ctx context.Context, rule *proxies.Rule, host string) proxyProbe {
	code, body, err := proxyProbeFn(ctx, probeProxyScheme(rule), host, rule.Listen,
		proxyProbePath(rule), proxyProbeTimeout)
	return proxyProbe{code: code, body: body, err: err}
}

// proxyProbeServed 判断一次探测的**响应**是否像"被 nginx 处理了"。判据刻意不是"2xx 就算通"：
// 必须真拿到 HTTP 响应（"000" = 连不上/被断开/超时 → 没生效）；80 端口上响应体是默认站点占位页
// → 规则没被加载（只在 80 判）；nginx 自己的 502/504 也算"已加载"。
func proxyProbeServed(p proxyProbe, listen int) bool {
	code := strings.TrimSpace(p.code)
	if code == "" || code == "000" {
		return false
	}
	if listen == 80 && strings.Contains(p.body, sites.LocalhostIndexMarker) {
		return false
	}
	return true
}

// describeProxyProbe 给复核失败一个可读的现场描述。
func describeProxyProbe(p proxyProbe, rule *proxies.Rule) string {
	msg := describeProxyProbePlain(p, rule)
	code := strings.TrimSpace(p.code)
	if code == "502" || code == "504" {
		if directToLAN(rule) {
			// 这是 macOS 15「本地网络」隐私门的典型症状：nginx 连不上局域网，
			// 而面板（Go、linker-signed）能连上。把"是什么 + 怎么修"写在错误里。
			msg += "。" + proxyLANForwardAdvice
		}
	}
	return msg
}

// describeProxyProbePlain 是不含"局域网授权"建议的现场描述（避免同一句话出现两遍）。
func describeProxyProbePlain(p proxyProbe, rule *proxies.Rule) string {
	code := strings.TrimSpace(p.code)
	if code == "" || code == "000" {
		if p.err != nil {
			return "没有拿到任何 HTTP 响应（" + p.err.Error() + "）"
		}
		return "没有拿到任何 HTTP 响应"
	}
	if rule.Listen == 80 && strings.Contains(p.body, sites.LocalhostIndexMarker) {
		return "HTTP " + code + "，但内容是默认站点（000-default）的占位页"
	}
	if code == "502" || code == "504" {
		return "HTTP " + code + "（nginx 已命中该反代规则，但目标 " + rule.Target + " 没响应）"
	}
	return "HTTP " + code
}

// directToLAN 判断"这条规则是 nginx 直连模式、且目标是局域网地址"。只在 502/504 的诊断
// 路径上调用（可能有一次 DNS 解析，失败现场没有性能要求）；解析不了按私有处理。
func directToLAN(rule *proxies.Rule) bool {
	if rule == nil || rule.Forwarding() {
		return false
	}
	host, _, err := rule.TargetHostPort()
	if err != nil {
		return false
	}
	return proxies.ClassifyTarget(host, proxyLookupHostFn) == proxies.ScopePrivate
}

// proxyServeCheck 是一次"规则是否真的生效"复核的完整证据。
type proxyServeCheck struct {
	Served  bool       // nginx 是否真的在用这份配置
	LogGrew bool       // 该规则自己的访问日志是否长出了新内容
	LogPath string     // 该规则的访问日志路径
	Probe   proxyProbe // 最后一次探测的响应
}

// waitProxyServed 轮询到"规则真的生效"，返回完整证据。最硬的证据是**该规则自己的访问
// 日志增长**：只有 nginx 真的加载了这个 server 块、并成功打开了这个日志文件，请求才可能
// 被写进去 —— 而"打不开日志文件"正是 reload 静默失败的原因。
func (s *Server) waitProxyServed(ctx context.Context, rule *proxies.Rule) proxyServeCheck {
	host := proxyProbeHost(rule)
	logPath := proxyAccessLogPath(s.proxyLogDir(), rule.ID)
	deadline := time.Now().Add(proxyVerifyWait)
	chk := proxyServeCheck{LogPath: logPath}
	for {
		before := proxyLogSize(logPath)
		chk.Probe = s.probeProxyOnce(ctx, rule, host)
		if proxyLogSettle > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(proxyLogSettle):
			}
		}
		chk.LogGrew = proxyLogSize(logPath) > before
		chk.Served = chk.LogGrew && proxyProbeServed(chk.Probe, rule.Listen)
		if chk.Served {
			return chk
		}
		if !time.Now().Before(deadline) {
			return chk
		}
		select {
		case <-ctx.Done():
			return chk
		case <-time.After(proxyVerifyEvery):
		}
	}
}

// reloadProxyAndVerify 是反代**写入/更新**后的统一收尾：chown 日志树 → reload →
// 请求级复核（该规则必须真的被 nginx 使用）。复核不通过一律返回错误，调用方据此返回
// 非 2xx（绝不"失败只记日志"）。
func (s *Server) reloadProxyAndVerify(ctx context.Context, rule *proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("配置已写入，但 nginx 重载失败: %w", err)
	}
	chk := s.waitProxyServed(ctx, rule)
	if !chk.Served {
		why := "以 Host=" + proxyProbeHost(rule) + " 请求 127.0.0.1:" +
			strconv.Itoa(rule.Listen) + proxyProbePath(rule) + " 得到 " +
			describeProxyProbe(chk.Probe, rule)
		if !chk.LogGrew {
			why += "，且该规则自己的访问日志 " + chk.LogPath + " 没有任何新增"
		}
		msg := fmt.Sprintf(
			"规则「%s」的配置已写入，nginx 重载也已发出，但 %s 内新配置仍未生效"+
				"（复核发现新配置没有生效）：%s。"+
				"最常见的原因是日志文件属主是 root（写 vhost 时以 root 跑过 `nginx -t`，"+
				"它会在 %s 下创建 root 属主的日志），以真实用户运行的 nginx 打不开它们 → "+
				"reload 失败而退出码仍是 0。请依次检查："+
				"① `nginx -t` 是否通过；"+
				"② `ps -o user,pid,command -p $(cat %s)` 里的用户能否读 %s；"+
				"③ vhost %s 是否被 %s 的 include 覆盖；"+
				"④ 全局 error_log：`tail -n 20 %s` 看有没有 [emerg]。",
			rule.Name, humanWait(proxyVerifyWait), why, filepath.Dir(chk.LogPath),
			filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid"), chk.LogPath,
			filepath.Join(s.Cfg.VhostDir, rule.VhostName()+".conf"), s.Cfg.NginxConf,
			filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"))
		if tail := s.nginxErrorLogTail(5); tail != "" {
			msg += "\n（nginx error_log 末几行）\n" + tail
		}
		return errors.New(msg)
	}
	// HTTPS 规则还要证明"端口上真的端出了这份证书"，光有 HTTP 响应不够。
	if err := s.verifyProxyTLSServed(ctx, rule); err != nil {
		return err
	}
	// 配置确实生效了，但"生效"不等于"能用"：502/504 说明 nginx 连不上上游。直连模式 +
	// 局域网目标 + **面板自己能连上** ⇒ 不是上游挂了，而是 macOS 15 本地网络隐私门只拦
	// 了 nginx。必须明确报错并给出修法，否则用户只看到一个"已生效"的规则和永远 502 的页面。
	if advice := s.directLANBlockedAdvice(ctx, rule, chk.Probe); advice != "" {
		return errors.New(advice)
	}
	return s.verifyProxyDomainGuard(ctx, rule)
}

// directLANBlockedAdvice 在"只可能是 macOS 本地网络授权把 nginx 拦了"时返回诊断文本。
// 判据缺一不可（避免把"上游本身没起来"误诊成授权问题）：① 直连模式；
// ② 探测得 502/504；③ 目标是私有/链路本地地址；④ 面板自己能直连（面板不受这道门限制）。
func (s *Server) directLANBlockedAdvice(ctx context.Context, rule *proxies.Rule, p proxyProbe) string {
	if rule == nil || rule.Forwarding() {
		return ""
	}
	code := strings.TrimSpace(p.code)
	if code != "502" && code != "504" {
		return ""
	}
	if _, _, err := rule.TargetHostPort(); err != nil {
		return ""
	}
	if s.forwarders.TargetScope(rule.Target) != proxies.ScopePrivate {
		return ""
	}
	if ok, _ := proxyProbeTargetFn(ctx, rule.Target); !ok {
		return "" // 面板也连不上 → 上游本身没起来，交给 target_ok 展示
	}
	return fmt.Sprintf("规则「%s」的配置已写入并被 nginx 加载，但请求 127.0.0.1:%d%s 得到 %s —— "+
		"面板自己可以直连 %s，只有 nginx 连不上。%s",
		rule.Name, rule.Listen, proxyProbePath(rule), describeProxyProbePlain(p, rule), rule.Target, proxyLANForwardAdvice)
}

// ---- HTTPS 复核：真实取回对端证书 ----

// tlsPeerInfo 是对端在 TLS 握手里**实际端出来**的证书信息。
type tlsPeerInfo struct {
	NotAfter time.Time
	DNSNames []string
	Subject  string
}

// proxyTLSPeerFn 取回该端口上真实提供的证书（生产用 Go 的 crypto/tls 直接拨号）。刻意
// InsecureSkipVerify：要证明的是"nginx 有没有把这份证书端出来"，不是链路可信性 ——
// 自签证书同样必须能通过复核，所以不能校验证书链。
var proxyTLSPeerFn = probeTLSPeerCert

func probeTLSPeerCert(ctx context.Context, port int, serverName string, timeout time.Duration) (tlsPeerInfo, error) {
	var out tlsPeerInfo
	if port <= 0 {
		return out, fmt.Errorf("监听端口无效：%d", port)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		&tls.Config{
			InsecureSkipVerify: true,
			ServerName:         serverName,
			MinVersion:         tls.VersionTLS12,
		})
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return out, errors.New("TLS 握手成功但没有拿到对端证书")
	}
	leaf := st.PeerCertificates[0]
	return tlsPeerInfo{NotAfter: leaf.NotAfter, DNSNames: leaf.DNSNames, Subject: leaf.Subject.String()}, nil
}

// proxyTLSServerName 选一个能命中该规则的 SNI。`_` 不是合法主机名（没有域名时
// server_name 就这么写），拿它做 SNI 没有意义，改用 localhost 让 nginx 用默认 server 应答。
func proxyTLSServerName(rule *proxies.Rule) string {
	h := strings.TrimSpace(proxyProbeHost(rule))
	if h == "" || h == "_" || strings.Contains(h, "*") {
		return "localhost"
	}
	return h
}

// verifyProxyTLSServed 复核"HTTPS 真的起来了，而且端出来的就是我们配的那份证书"：① 证书文件
// 能解析；② 真实 TLS 握手取回对端 leaf 证书；③ 对端 NotAfter 必须与配置一致（能抓到"加载的
// 还是旧证书"）。文件写进去了、reload 退出码是 0，都不等于能握手成功。
func (s *Server) verifyProxyTLSServed(ctx context.Context, rule *proxies.Rule) error {
	if !rule.SSLEnabled {
		return nil
	}
	want, err := tlsx.CertExpiry(rule.SSLCert)
	if err != nil {
		return fmt.Errorf("规则「%s」已启用 HTTPS，但证书文件读不到（%s）：%v。"+
			"nginx 会因此加载失败或起不来，请先在「SSL 证书」页重新签发/续期",
			rule.Name, rule.SSLCert, err)
	}
	info, derr := proxyTLSPeerFn(ctx, rule.Listen, proxyTLSServerName(rule), proxyProbeTimeout)
	if derr != nil {
		return fmt.Errorf("规则「%s」的 HTTPS 复核失败：在 127.0.0.1:%d 上做 TLS 握手时 %v。"+
			"最常见的原因是 nginx 没有真正重载（证书文件属主是 root 时，以普通用户运行的 nginx "+
			"读不到它 → reload 失败但退出码仍是 0），或该端口上的默认 server 抢先应答了。"+
			"请到「日志中心 → nginx error_log」看 [emerg] 行",
			rule.Name, rule.Listen, derr)
	}
	if !info.NotAfter.Equal(want) {
		return fmt.Errorf("规则「%s」的 HTTPS 复核失败：配置里写的是 %s（到期 %s），"+
			"但 127.0.0.1:%d 实际端出来的证书到期时间是 %s —— 说明这份 ssl_certificate 没有生效",
			rule.Name, rule.SSLCert, want.Format("2006-01-02 15:04:05"),
			rule.Listen, info.NotAfter.Format("2006-01-02 15:04:05"))
	}
	return nil
}

// reloadProxyAndVerifyGone 是反代**删除/停用**后的统一收尾：删除不需要写配置，但
// "删了却没卸载"同样是静默失败（reload 退出码 0），所以 chown + reload + 复核不能省。
func (s *Server) reloadProxyAndVerifyGone(ctx context.Context, rule *proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("配置已删除，但 nginx 重载失败: %w", err)
	}
	return s.waitProxyGone(ctx, rule)
}

// waitProxyGone 轮询到"这条规则真的不再被 nginx 使用"。判据是该规则自己的访问日志不再
// 增长：同一端口上可能还有别的启用规则在应答，"端口有没有响应"区分不出到底是谁在服务，
// 而每条规则的 location 只写自己的 access_log。
func (s *Server) waitProxyGone(ctx context.Context, rule *proxies.Rule) error {
	logPath := proxyAccessLogPath(s.proxyLogDir(), rule.ID)
	prev := proxyLogSize(logPath)
	deadline := time.Now().Add(proxyVerifyWait)
	var last proxyProbe
	for {
		last = s.probeProxyOnce(ctx, rule, proxyProbeHost(rule))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(proxyVerifyEvery):
		}
		cur := proxyLogSize(logPath)
		if cur <= prev {
			return nil // 这条规则的日志不再增长 → 它已经不再被 nginx 使用
		}
		prev = cur
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"规则「%s」的 nginx 配置已删除、重载命令也返回成功，但等待 %s 后复核发现"+
					"**它仍在生效**（每次请求后它自己的访问日志 %s 仍在增长，最后一次响应 %s）。"+
					"这通常意味着 nginx 没有真正重载（日志属主是 root 时 reload 会失败但退出码仍是 0）。"+
					"请检查：`nginx -t`、`ps -o user,pid,command -p $(cat %s)` 的运行用户权限、"+
					"以及全局 error_log（`tail -n 20 %s`）里的 [emerg]。",
				rule.Name, humanWait(proxyVerifyWait), logPath, describeProxyProbe(last, rule),
				filepath.Join(s.Cfg.BrewPrefix, "var", "run", "nginx.pid"),
				filepath.Join(s.Cfg.BrewPrefix, "var", "log", "nginx", "error.log"))
		}
	}
}

// verifyProxyDomainGuard 复核"域名对不上的 Host 不会被误转发到后端"。只有该端口确实应该
// 存在兜底拒绝块（文件在）时才检查：80 端口上 000-default.conf 已占了 default_server，
// 拒绝块写不进去但不算失败（不匹配的 Host 被默认站点接住，漏不到后端）；通配规则跳过。
func (s *Server) verifyProxyDomainGuard(ctx context.Context, rule *proxies.Rule) error {
	if len(proxies.SplitDomains(rule.Domains)) == 0 {
		return nil
	}
	reject := filepath.Join(s.Cfg.VhostDir, proxies.RejectVhostName(rule.Listen)+".conf")
	if _, err := os.Stat(reject); err != nil {
		return nil // 没有兜底块（例如 80 端口被默认站点占了），无从复核
	}
	deadline := time.Now().Add(proxyVerifyWait)
	var last proxyProbe
	for {
		last = s.probeProxyOnce(ctx, rule, "zp-probe.invalid")
		if !proxyProbeServed(last, rule.Listen) {
			return nil // 被兜底块拒绝（444 / 连不上）→ 域名限制生效
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"规则「%s」限制了域名 %q，但复核发现域名对不上的 Host 也被转发到了后端（%s）："+
					"兜底拒绝块 %s 没有生效。这通常意味着 nginx 没有真正重载。"+
					"请到「日志中心 → nginx error_log」看 [emerg] 行",
				rule.Name, rule.Domains, describeProxyProbe(last, rule), reject)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(proxyVerifyEvery):
		}
	}
}

// isDuplicateDefaultServer 判断"兜底拒绝块写不进去"是不是因为该端口已经有别的 default_server
// （典型：000-default.conf 的 `listen 80 default_server`）。这不是失败：那个默认 server 会接住
// 域名对不上的 Host；当失败会让**所有 80 端口上的带域名规则都无法创建**（最常见用法）。
func isDuplicateDefaultServer(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate default server")
}

// applyProxy 生成并应用一条规则的 nginx 配置。与 applySite 一样：失败时撤销本次写入的
// vhost（还原旧内容 / 删掉本次新建的），调用方才能做到"接口非 2xx = 这次没做成、
// 重试不会被上一次的残留挡住"。
func (s *Server) applyProxy(ctx context.Context, rule *proxies.Rule) error {
	if !rule.Enabled {
		return s.removeProxyConfig(ctx, rule)
	}
	// 生成前统一把鉴权配置装回 Rule 并落 htpasswd 文件：所有"重新生成配置"的
	// 路径（新建/更新/启停/绑证书/启动对齐）都经过这里，不会漏掉鉴权。
	if err := s.hydrateProxyAuth(ctx, rule); err != nil {
		return err
	}
	if err := s.writeProxyAuthFile(rule); err != nil {
		return err
	}
	content, err := rule.Generate(s.proxyLogDir())
	if err != nil {
		return err
	}
	snap := s.snapshotVhost(rule.VhostName())
	// 复用站点的写盘通道：它经提权助手做原子写 + nginx -t 校验 + 失败回滚
	if err := proxyWriteVhostFn(s, ctx, rule.VhostName(), content); err != nil {
		return err
	}
	// **写盘之后、reload 之前**把日志树交还真实用户，并在 reload 后做请求级复核。
	if err := s.reloadProxyAndVerify(ctx, rule); err != nil {
		return s.rollbackVhostWrite(ctx, snap, proxyWriteVhostFn, proxyReloadFn, err)
	}
	return nil
}

// syncForwarder 让一条规则的回环转发器与当前配置对齐，并把端口写回 rule.ForwardPort。
// **必须在 applyProxy / Generate 之前调用**：proxy_pass 用的是 rule.ForwardPort，端口由管理器
// 分配（优先复用数据库里的旧值）。不需要转发时停掉监听器并把 ForwardPort 清 0（真的关掉）。
func (s *Server) syncForwarder(rule *proxies.Rule) error {
	if rule == nil {
		return nil
	}
	if !rule.Enabled {
		s.forwarders.Stop(rule.ID)
		return nil
	}
	if _, err := s.forwarders.Ensure(rule); err != nil {
		return err
	}
	return nil
}

// reconcileForwarders 在面板启动时把转发器对齐到数据库里的规则：该起的起、该停的停；
// 端口分配结果落库（重启后复用同一个端口）；端口变了就重写对应 vhost —— 否则 nginx 里的
// proxy_pass 会指向旧端口，表现为"转发器起来了、规则却是 502"。
func (s *Server) reconcileForwarders(ctx context.Context) {
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		s.Log.Warn("读取反向代理规则失败，跳过回环转发器对齐: %v", err)
		return
	}
	changed := s.forwarders.Reconcile(list)
	if len(changed) == 0 {
		return
	}
	changedSet := make(map[int64]bool, len(changed))
	for _, id := range changed {
		changedSet[id] = true
	}
	for _, rule := range list {
		if !changedSet[rule.ID] {
			continue
		}
		if err := s.proxyRepo().SetForwardPort(ctx, rule.ID, rule.ForwardPort); err != nil {
			s.Log.Warn("保存规则 %d 的回环转发端口失败: %v", rule.ID, err)
		}
		if !rule.Enabled {
			continue
		}
		if err := s.applyProxy(ctx, rule); err != nil {
			// nginx 没装/没起来时不要让整个启动失败：转发器已经起了，
			// 下一次保存规则会重新写 vhost。如实记一笔。
			s.Log.Warn("回环转发端口变化后重写规则 %d 的 nginx 配置失败: %v", rule.ID, err)
		}
	}
	if len(changed) > 0 {
		s.Log.Info("已对齐回环转发器：%d 条规则的端口有变化", len(changed))
	}
}

// removeProxyConfig 移除一条规则的配置文件并 reload。复核发现"删了却仍在生效"时
// **把文件还原回去**：那时记录还在、配置却没了，是最典型的不一致状态；还原后重试删除
// 才是干净的。
func (s *Server) removeProxyConfig(ctx context.Context, rule *proxies.Rule) error {
	snap := s.snapshotVhost(rule.VhostName())
	if err := siteDeleteVhostFn(s, ctx, rule.VhostName()); err != nil {
		return err
	}
	if err := s.reloadProxyAndVerifyGone(ctx, rule); err != nil {
		return s.rollbackVhostWrite(ctx, snap, proxyWriteVhostFn, proxyReloadFn, err)
	}
	return nil
}

// syncRejectBlocks 把"兜底拒绝块"与规则集对齐：端口出现带域名的规则 → 补 default_server
// 拒绝块；该端口再无带域名规则 → 删掉（否则会把通配规则一起打死）。收尾走"chown → reload →
// 请求级复核"；判定用**数据库里的**规则集，删域名时会多留一个兜底块（安全侧多余，下次收敛）。
func (s *Server) syncRejectBlocks(ctx context.Context) error {
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	specs := proxyRejectSpecs(list)
	need := map[int]bool{}
	// rep[port] 存该端口上任意一条带域名的启用规则，供复核构造探测请求。
	rep := map[int]*proxies.Rule{}
	for _, r := range list {
		if r.Enabled && len(proxies.SplitDomains(r.Domains)) > 0 {
			need[r.Listen] = true
			if rep[r.Listen] == nil {
				rep[r.Listen] = r
			}
		}
	}

	changed := false
	for port, sp := range specs {
		content, cerr := s.proxyRejectBlockContent(port, sp)
		if cerr != nil {
			return cerr
		}
		if err := s.writeVhost(ctx, proxies.RejectVhostName(port), content); err != nil {
			if isDuplicateDefaultServer(err) {
				// 该端口已经有别的 default_server 了（典型就是 000-default.conf 的
				// `listen 80 default_server`）：不匹配的 Host 会被它接住，我们的兜底块
				// 既写不进去、也不需要。
				delete(need, port)
				delete(rep, port)
				continue
			}
			return fmt.Errorf("写端口 %d 的兜底拒绝块失败：%w", port, err)
		}
		changed = true
	}
	// 清掉已经不需要的兜底块（扫目录，避免依赖内存里的端口集合）
	entries, derr := os.ReadDir(s.Cfg.VhostDir)
	if derr == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "proxy-reject-") || !strings.HasSuffix(name, ".conf") {
				continue
			}
			portStr := strings.TrimSuffix(strings.TrimPrefix(name, "proxy-reject-"), ".conf")
			port, perr := strconv.Atoi(portStr)
			if perr != nil || need[port] {
				continue
			}
			if err := s.deleteVhost(ctx, strings.TrimSuffix(name, ".conf")); err != nil {
				return fmt.Errorf("删除端口 %s 的兜底拒绝块失败：%w", portStr, err)
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.reloadRejectBlocksAndVerify(ctx, need, rep)
}

// proxyRejectSpec 描述某端口上"域名兜底拒绝块"该长什么样。
type proxyRejectSpec struct {
	SSL  bool   // 该端口上是否至少有已启用的 HTTPS 规则
	Cert string // SSL=true 时兜底块要带的证书（否则 nginx [emerg]）
	Key  string
}

// proxyRejectSpecs 从规则集推导出每个端口的兜底块形态。抽成纯函数是为了让单测钉住
// "SSL 端口必须带证书"这条判据（真机实测：同一端口上只要有一个 server 块写了 ssl，
// 所有 server 块都必须有 ssl_certificate，否则 `nginx -t` 直接 [emerg]）。
func proxyRejectSpecs(rules []*proxies.Rule) map[int]proxyRejectSpec {
	out := map[int]proxyRejectSpec{}
	for _, r := range rules {
		if r == nil || !r.Enabled || len(proxies.SplitDomains(r.Domains)) == 0 {
			continue
		}
		sp := out[r.Listen]
		if r.SSLEnabled {
			sp.SSL = true
			if sp.Cert == "" {
				sp.Cert, sp.Key = r.SSLCert, r.SSLKey
			}
		}
		out[r.Listen] = sp
	}
	return out
}

// proxyPortListenMode 判断某端口上**已有 vhost** 用的 listen 协议选项，返回 (hasSSL, uniformSSL)。
// 实测（2026-09-18 生产 error.log）：nginx 要求同一 `0.0.0.0:<port>` 上重复的 listen 选项集一致，
// 否则 `protocol options redefined`；兜底块必须跟随（全 ssl 写 ssl、混合端口绝不写）。
func (s *Server) proxyPortListenMode(port int, selfFile string) (hasSSL, uniformSSL bool) {
	if port <= 0 {
		return false, false
	}
	entries, err := os.ReadDir(s.Cfg.VhostDir)
	if err != nil {
		// 读不到目录：不猜，按"没有 ssl 邻居"处理（生成不带 ssl 的兜底块）。
		return false, false
	}
	uniformSSL = true
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == selfFile+".conf" || !strings.HasSuffix(name, ".conf") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(s.Cfg.VhostDir, name))
		if rerr != nil {
			continue
		}
		// 逐 server 块解析：同一份文件里可能有多个块（如站点 vhost 的 80/443），
		// 按块调用 siteListenPorts 才不会把同端口的两种写法去重掉。
		for _, block := range siteServerBlocks(string(b)) {
			for _, lp := range siteListenPorts(block) {
				if lp.Port != port {
					continue
				}
				if lp.SSL {
					hasSSL = true
				} else {
					uniformSSL = false
				}
			}
		}
	}
	if !hasSSL {
		uniformSSL = false
	}
	return hasSSL, uniformSSL
}

// proxyRejectBlockContent 生成某端口「域名兜底拒绝块」的内容，这是**唯一的生成点**：listen 写
// 不写 ssl、要不要带证书行都在这里定，保证与端口上已有 vhost 的 listen 选项一致，不再产生
// `protocol options redefined` 警告。抽成方法是为了让单测直接钉住生成结果。
func (s *Server) proxyRejectBlockContent(port int, sp proxyRejectSpec) (string, error) {
	if sp.SSL && (sp.Cert == "" || sp.Key == "") {
		return "", fmt.Errorf("端口 %d 上有已启用 HTTPS 的规则，但数据库里没有证书路径："+
			"nginx 要求同一端口上每个 server 块都有证书，无法为域名兜底块提供证书", port)
	}
	hasSSLPeer, uniformSSL := s.proxyPortListenMode(port, proxies.RejectVhostName(port))
	cert, key := "", ""
	if sp.SSL || hasSSLPeer {
		// 端口上只要有任何 server 写 ssl，nginx 就要求该端口**每个** server
		// 都能取到证书；兜底块永远 return 444，证书只是为满足这条要求。
		cert, key = sp.Cert, sp.Key
	}
	// 兜底块写 ssl 的条件：sp.SSL 为真**且**端口上没有任何"不带 ssl 的邻居"（混合端口
	// 再写 ssl 会成为第三种选项集）。不能只看"扫到了 ssl 邻居"：写这份兜底块时，规则自己
	// 的 vhost 可能还没落盘（例如刚启用一条 HTTPS 规则），此时按 sp.SSL 判断才对。
	sslListen := sp.SSL && (uniformSSL || !hasSSLPeer)
	return proxies.GenerateRejectWithCert(port, s.proxyLogDir(), cert, key, sslListen), nil
}

// stabilizeProxyReject 在 SSL 开关切换写盘之前，把该端口的兜底块改成"带证书行"的形态，保证
// 中间态不会让 `nginx -t` 判 [emerg]（真机 nginx 1.31.5 实测：开/关都要同时改规则 vhost 与兜底块，
// 而每次写盘都跑 `nginx -t`；带证书行的 server 任何组合都合法，所以先落它）。targetSSL 决定带不带 ssl。
func (s *Server) stabilizeProxyReject(ctx context.Context, port int, certPath, keyPath string, targetSSL bool) error {
	if certPath == "" || keyPath == "" {
		return nil
	}
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	if !proxies.RejectPorts(list)[port] {
		return nil // 这个端口不需要兜底块（例如全是通配规则）
	}
	sslListen := false
	if targetSSL {
		if _, uniform := s.proxyPortListenMode(port, proxies.RejectVhostName(port)); uniform {
			sslListen = true
		}
	}
	content := proxies.GenerateRejectWithCert(port, s.proxyLogDir(), certPath, keyPath, sslListen)
	return s.writeVhost(ctx, proxies.RejectVhostName(port), content)
}

// checkProxySSLPortMix 拦住"同一端口上 HTTP 与 HTTPS 混用"：只要有一个 server 块写了 `ssl`，
// 整个 listen 端口就按 TLS 处理，另一个"以为自己是 HTTP"的规则会静默失效（用户看不出来）。
// 同时拦住"端口 80 + HTTPS"：80 被 000-default.conf 占着 default_server 且无证书 → [emerg]。
func (s *Server) checkProxySSLPortMix(ctx context.Context, rule *proxies.Rule) error {
	if rule == nil || !rule.Enabled {
		return nil // 停用的规则不写配置，不会造成混用
	}
	if rule.SSLEnabled && rule.Listen == 80 {
		return fmt.Errorf("端口 80 是面板默认站点的 HTTP 端口，不能作为 HTTPS 端口：" +
			"请把监听端口改成 443 或其它端口（nginx 要求同端口的每个 server 块都有证书）")
	}
	list, err := s.proxyRepo().List(ctx)
	if err != nil {
		return err
	}
	for _, other := range list {
		if other == nil || other.ID == rule.ID || !other.Enabled || other.Listen != rule.Listen {
			continue
		}
		if other.SSLEnabled == rule.SSLEnabled {
			continue
		}
		// other 与 rule 的 SSL 一定不同（相同的上面 continue 了），两者的模式互为反面。
		// ⚠️ 这里曾经把两个标签写反（赋值与打印顺序对不上），正好把人往反方向带
		// （2026-09-17 用户实测报障）。这条是 HTTPS ⇒ 那条是 HTTP，反之亦然。
		otherMode, myMode := "HTTP", "HTTPS"
		if !rule.SSLEnabled {
			otherMode, myMode = "HTTPS", "HTTP"
		}
		return fmt.Errorf("端口 %d 上已有%s规则「%s」，而这条是%s：nginx 的同一端口不能同时跑 "+
			"HTTP 与 HTTPS（只要有一个 server 块启用 ssl，整个端口就变成 TLS）。\n"+
			"三条出路：① 让这条也启用 HTTPS（同一端口必须同为 TLS；域名有证书时这条最简单）；"+
			"② 把这条规则改到别的端口；③ 先停用/删除「%s」。\n"+
			"提示：证书来源选「面板证书库」或「粘贴自有证书」时，勾上 HTTPS 会**连证书一起保存**；"+
			"选「自签 / mkcert」时证书要现生成，可以先**取消勾选「启用」**建好规则 → 绑上 HTTPS → 再启用。",
			rule.Listen, otherMode, other.Name, myMode, other.Name)
	}
	return nil
}

// reloadRejectBlocksAndVerify 让兜底拒绝块生效，并复核"域名限制真的落实了"。
func (s *Server) reloadRejectBlocksAndVerify(ctx context.Context, need map[int]bool, rep map[int]*proxies.Rule) error {
	proxyChownLogsFn(s)
	if err := proxyReloadFn(s, ctx); err != nil {
		return fmt.Errorf("兜底拒绝块的配置已写入，但 nginx 重载失败: %w", err)
	}
	for port := range need {
		rule := rep[port]
		if rule == nil {
			continue
		}
		if err := s.verifyProxyDomainGuard(ctx, rule); err != nil {
			return err
		}
	}
	return nil
}

// deleteVhost 删除 vhost 文件（经提权助手）：文件本来就不存在时视为成功（删除是幂等的）。
func (s *Server) deleteVhost(ctx context.Context, name string) error {
	path := filepath.Join(s.Cfg.VhostDir, name+".conf")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	_, err := s.callHelper(ctx, "vhost-delete", name)
	return err
}
