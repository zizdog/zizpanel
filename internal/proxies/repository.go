package proxies

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/store"
)

// ErrNotFound 表示规则不存在。HTTP 层据此返回 404，而不是把"本来就没有"报成 500。
var ErrNotFound = errors.New("反向代理规则不存在")

// Repository 负责规则的持久化。
type Repository struct{ st *store.Store }

// NewRepository 创建仓库。
func NewRepository(st *store.Store) *Repository { return &Repository{st: st} }

const ruleCols = `id,name,listen,domains,path,target,preserve_host,websocket,enabled,remark,
	ssl_enabled,ssl_cert,ssl_key,ssl_provider,ssl_expires,
	tls_name,standard_headers,redirect_http,lan_forward,forward_port,created_at,updated_at`

func scanRule(sc interface{ Scan(...any) error }) (*Rule, error) {
	var r Rule
	var preserve, ws, enabled, sslEnabled, stdHeaders, redirectHTTP int
	var created, updated string
	if err := sc.Scan(&r.ID, &r.Name, &r.Listen, &r.Domains, &r.Path, &r.Target,
		&preserve, &ws, &enabled, &r.Remark,
		&sslEnabled, &r.SSLCert, &r.SSLKey, &r.SSLProvider, &r.SSLExpires,
		&r.TLSName, &stdHeaders, &redirectHTTP,
		&r.LANForward, &r.ForwardPort,
		&created, &updated); err != nil {
		return nil, err
	}
	r.PreserveHost = preserve == 1
	r.Websocket = ws == 1
	r.Enabled = enabled == 1
	r.SSLEnabled = sslEnabled == 1
	r.StandardHeaders = stdHeaders == 1
	r.RedirectHTTP = redirectHTTP == 1
	// 老库/手工写入的行可能是空串：按契约"默认 auto"补齐，而不是把控制权
	// 交给一个未定义取值（Generate 对空串也是按 auto 的）。
	r.LANForward = r.LANForwardMode()
	r.Created = parseTime(created)
	r.Updated = parseTime(updated)
	return &r, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nowStr() string { return time.Now().Format(time.RFC3339) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// List 返回全部规则（按监听端口、再按 id 排序 —— 同端口上多条规则时顺序稳定）。
func (r *Repository) List(ctx context.Context) ([]*Rule, error) {
	rows, err := r.st.DB().QueryContext(ctx, `SELECT `+ruleCols+` FROM proxies ORDER BY listen, id`)
	if err != nil {
		return nil, fmt.Errorf("查询反向代理规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Rule
	for rows.Next() {
		it, serr := scanRule(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Get 按 id 取一条规则。
func (r *Repository) Get(ctx context.Context, id int64) (*Rule, error) {
	row := r.st.DB().QueryRowContext(ctx, `SELECT `+ruleCols+` FROM proxies WHERE id=?`, id)
	it, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// Create 新建规则并返回带 id 的副本。
func (r *Repository) Create(ctx context.Context, rule *Rule) (*Rule, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	now := nowStr()
	res, err := r.st.DB().ExecContext(ctx,
		`INSERT INTO proxies (name,listen,domains,path,target,preserve_host,websocket,enabled,remark,
		 ssl_enabled,ssl_cert,ssl_key,ssl_provider,ssl_expires,
		 tls_name,standard_headers,redirect_http,lan_forward,forward_port,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rule.Name, rule.Listen, rule.Domains, rule.Path, rule.Target,
		boolInt(rule.PreserveHost), boolInt(rule.Websocket), boolInt(rule.Enabled),
		rule.Remark,
		boolInt(rule.SSLEnabled), rule.SSLCert, rule.SSLKey, rule.SSLProvider, rule.SSLExpires,
		rule.TLSName, boolInt(rule.StandardHeaders), boolInt(rule.RedirectHTTP),
		rule.LANForwardMode(), forwardPortOrZero(rule.ForwardPort),
		now, now)
	if err != nil {
		return nil, fmt.Errorf("保存反向代理规则失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return r.Get(ctx, id)
}

// Update 覆盖保存一条规则。
func (r *Repository) Update(ctx context.Context, rule *Rule) (*Rule, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}
	res, err := r.st.DB().ExecContext(ctx,
		`UPDATE proxies SET name=?,listen=?,domains=?,path=?,target=?,preserve_host=?,
		 websocket=?,enabled=?,remark=?,
		 ssl_enabled=?,ssl_cert=?,ssl_key=?,ssl_provider=?,ssl_expires=?,
		 tls_name=?,standard_headers=?,redirect_http=?,lan_forward=?,forward_port=?,updated_at=? WHERE id=?`,
		rule.Name, rule.Listen, rule.Domains, rule.Path, rule.Target,
		boolInt(rule.PreserveHost), boolInt(rule.Websocket), boolInt(rule.Enabled),
		rule.Remark,
		boolInt(rule.SSLEnabled), rule.SSLCert, rule.SSLKey, rule.SSLProvider, rule.SSLExpires,
		rule.TLSName, boolInt(rule.StandardHeaders), boolInt(rule.RedirectHTTP),
		rule.LANForwardMode(), forwardPortOrZero(rule.ForwardPort),
		nowStr(), rule.ID)
	if err != nil {
		return nil, fmt.Errorf("更新反向代理规则失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return r.Get(ctx, rule.ID)
}

// SetForwardPort 只更新一条规则的回环转发端口。
//
// 单独一个方法（而不是走 Update）是因为"端口分配结果落库"发生在转发器起来
// 之后、而 nginx 配置写入之前；这条 UPDATE 不碰其它字段，避免把内存里可能
// 尚未校验过的改动顺手写进去。port<=0 表示不使用转发。
func (r *Repository) SetForwardPort(ctx context.Context, id int64, port int) error {
	res, err := r.st.DB().ExecContext(ctx,
		`UPDATE proxies SET forward_port=?, updated_at=? WHERE id=?`,
		forwardPortOrZero(port), nowStr(), id)
	if err != nil {
		return fmt.Errorf("保存回环转发端口失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// forwardPortOrZero 把非法端口归一成 0（表示不使用转发）。
func forwardPortOrZero(p int) int {
	if p < 0 || p > 65535 {
		return 0
	}
	return p
}

// Delete 删除一条规则。
func (r *Repository) Delete(ctx context.Context, id int64) error {
	res, err := r.st.DB().ExecContext(ctx, `DELETE FROM proxies WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("删除反向代理规则失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConflictWith 返回与 rule 在"同一监听端口 + 同一域名/路径"上冲突的已有规则。
//
// 为什么要查：两条规则监听同端口、同 server_name、同 location 时，
// nginx 只会用**先加载的那一条**，另一条静默失效 —— 而界面上两条都显示"已启用"，
// 用户完全看不出为什么自己的规则不生效。
func (r *Repository) ConflictWith(ctx context.Context, rule *Rule) (*Rule, error) {
	list, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	mine := normalizeDomains(rule.Domains)
	myPath := strings.TrimRight(rule.Path, "/")
	for _, other := range list {
		if other.ID == rule.ID || other.Listen != rule.Listen {
			continue
		}
		if normalizeDomains(other.Domains) != mine {
			continue
		}
		if strings.TrimRight(other.Path, "/") == myPath {
			return other, nil
		}
	}
	return nil, nil
}

// normalizeDomains 把域名列表归一化成可比较的字符串（排序后拼接）。
func normalizeDomains(s string) string {
	d := SplitDomains(s)
	if len(d) == 0 {
		return ""
	}
	// 简单插入排序：列表很短，且避免为了一个比较引入 sort 的额外分配
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
	return strings.Join(d, ",")
}
