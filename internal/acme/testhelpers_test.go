package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logRecorder 是一个「记录所有输出」的假 logger，用来断言日志里没有密钥/token。
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, s)
}

// emitf 让 logRecorder 也能当作 provider 的 emit 回调（printf 风格）。
func (r *logRecorder) emitf(format string, args ...any) {
	r.log(fmt.Sprintf(format, args...))
}

func (r *logRecorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// newTestManager 创建一个完全落在 t.TempDir() 里的引擎，绝不碰真实数据目录。
func newTestManager(t *testing.T) (*Manager, *logRecorder, string) {
	t.Helper()
	root := t.TempDir()
	rec := &logRecorder{}
	m := New(root, filepath.Join(root, "wwwroot"), rec.log)
	return m, rec, root
}

// fakeIssuer 生成自签证书，替代真实 CA 交互（单测绝不联网）。
type fakeIssuer struct {
	mu         sync.Mutex
	validDays  int
	lastKeyPEM []byte
	lastPlan   issuePlan
	calls      int
	err        error
	before     func(plan issuePlan) error
}

func (f *fakeIssuer) obtain(ctx context.Context, plan issuePlan) (*obtainedCert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.before != nil {
		if err := f.before(plan); err != nil {
			return nil, err
		}
	}
	f.calls++
	f.lastPlan = plan
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	res, err := makeSelfSigned(plan.domains, f.validDays)
	if err != nil {
		return nil, err
	}
	f.lastKeyPEM = res.key
	return res, nil
}

func (f *fakeIssuer) lastPlanCopy() issuePlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPlan
}

func (f *fakeIssuer) keyPEM() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.lastKeyPEM...)
}

// makeSelfSigned 生成一张覆盖 domains 的自签证书（PEM 链 + 私钥 PEM）。
func makeSelfSigned(domains []string, validDays int) (*obtainedCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domains[0]},
		Issuer:                pkix.Name{CommonName: "ZizPanel Test CA", Organization: []string{"ZizPanel"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Duration(validDays) * 24 * time.Hour),
		DNSNames:              domains,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &obtainedCert{
		fullchain: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:       pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}
