// Package tlsx 负责面板自身的 HTTPS 证书：自签生成与到期检查。
//
// 为什么默认自签：面板要「装完即可远程访问」，而目标机器可能没有公网域名。
// 自签 + 强密码 + HttpOnly Cookie 在局域网/Tailscale 场景下是安全的，
// 浏览器会提示一次证书不受信任，用户可以手工信任或之后在面板里换成正式证书。
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// HostIPs 返回本机所有非回环 IPv4/IPv6 地址，用于生成证书 SAN。
func HostIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// PrimaryIP 返回最可能的对外访问地址（优先私有网段 IPv4）。
// 安装脚本用它打印"请在浏览器打开 https://x.x.x.x:8443"。
func PrimaryIP() string {
	var fallback string
	for _, ip := range HostIPs() {
		v4 := ip.To4()
		if v4 == nil {
			if fallback == "" {
				fallback = "[" + ip.String() + "]"
			}
			continue
		}
		if v4.IsPrivate() {
			return v4.String()
		}
		if fallback == "" {
			fallback = v4.String()
		}
	}
	if fallback != "" {
		return fallback
	}
	return "127.0.0.1"
}

// EnsureSelfSigned 确保证书存在且未过期，否则重新生成。
// 返回 true 表示本次生成了新证书。
func EnsureSelfSigned(certPath, keyPath string, hosts []string, validDays int) (bool, error) {
	if validDays <= 0 {
		validDays = 3650 // 自签证书给足 10 年，避免到期后打不开面板
	}
	if certOK(certPath, keyPath) {
		return false, nil
	}
	if err := GenerateSelfSigned(certPath, keyPath, hosts, validDays); err != nil {
		return false, err
	}
	return true, nil
}

// CertExpiry 读取证书到期时间。
func CertExpiry(certPath string) (time.Time, error) {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return time.Time{}, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return time.Time{}, fmt.Errorf("证书格式无法识别: %s", certPath)
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return crt.NotAfter, nil
}

// certOK 判断现有证书是否可用（存在、能解析、30 天内不到期）。
func certOK(certPath, keyPath string) bool {
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	exp, err := CertExpiry(certPath)
	if err != nil {
		return false
	}
	return time.Until(exp) > 30*24*time.Hour
}

// GenerateSelfSigned 生成自签 ECDSA P-256 证书（比 RSA 生成快，握手也更快）。
func GenerateSelfSigned(certPath, keyPath string, hosts []string, validDays int) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成私钥失败: %w", err)
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return err
	}

	dnsNames := []string{"localhost", "zizpanel.local"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, h)
	}
	// 把本机网卡地址也加进去，换网络后证书依然有效
	ips = append(ips, HostIPs()...)

	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "ZizPanel",
			Organization: []string{"ZizPanel Self-Signed"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, validDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // 自签需要能自证，标记为 CA 以便部分客户端信任
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("签发证书失败: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = certOut.Close()
		return err
	}
	if err := certOut.Close(); err != nil {
		return err
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		_ = keyOut.Close()
		return err
	}
	return keyOut.Close()
}
