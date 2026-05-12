package cert

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

const (
	caCertFile   = "ca.pem"
	caKeyFile    = "ca-key.pem"
	serverCertFile = "server.pem"
	serverKeyFile  = "server-key.pem"
)

// EnsureCertificates 确保证书文件存在，如果不存在则自动生成
// 流程：生成本地 CA → 用 CA 签发服务端证书
// 将 ca.pem 导入系统受信任根证书存储后，浏览器不会告警
func EnsureCertificates(dataDir string) (certPath, keyPath string, err error) {
	certDir := filepath.Join(dataDir, "certs")
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create cert directory: %w", err)
	}

	caCertPath := filepath.Join(certDir, caCertFile)
	caKeyPath := filepath.Join(certDir, caKeyFile)
	certPath = filepath.Join(certDir, serverCertFile)
	keyPath = filepath.Join(certDir, serverKeyFile)

	if fileExists(certPath) && fileExists(keyPath) && fileExists(caCertPath) {
		return certPath, keyPath, nil
	}

	if err := generateCACert(caCertPath, caKeyPath); err != nil {
		return "", "", fmt.Errorf("failed to generate CA certificate: %w", err)
	}

	if err := generateServerCert(caCertPath, caKeyPath, certPath, keyPath); err != nil {
		return "", "", fmt.Errorf("failed to generate server certificate: %w", err)
	}

	fmt.Println("📜 TLS 证书已生成 (本地 CA + 服务端证书)")
	fmt.Println("   提示: 将 .switchai/certs/ca.pem 导入系统受信任根证书存储，浏览器将不再告警")
	return certPath, keyPath, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// generateCACert 生成本地 CA 根证书
func generateCACert(caCertPath, caKeyPath string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	caTemplate := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization:       []string{"SwitchAI"},
			OrganizationalUnit: []string{"SwitchAI Local CA"},
			CommonName:         "SwitchAI Local CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create CA cert: %w", err)
	}

	if err := writeCertPEM(caCertPath, caDER); err != nil {
		return err
	}

	return writeKeyPEM(caKeyPath, caKey)
}

// generateServerCert 用 CA 签发服务端证书
func generateServerCert(caCertPath, caKeyPath, serverCertPath, serverKeyPath string) error {
	// 加载 CA
	caCert, caKey, err := loadCACertAndKey(caCertPath, caKeyPath)
	if err != nil {
		return err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	// 收集本机所有 IP 作为 SAN
	sanIPs := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("::1"),
	}
	if ips := getLocalIPs(); len(ips) > 0 {
		sanIPs = append(sanIPs, ips...)
	}

	serverTemplate := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"SwitchAI"},
			CommonName:   "SwitchAI Server",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(825 * 24 * time.Hour), // 约 2 年
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost", "switchai.local"},
		IPAddresses: sanIPs,
	}

	serverDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create server cert: %w", err)
	}

	if err := writeCertPEM(serverCertPath, serverDER); err != nil {
		return err
	}

	return writeKeyPEM(serverKeyPath, serverKey)
}

func loadCACertAndKey(caCertPath, caKeyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	// 加载 CA 证书
	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA cert: %w", err)
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	// 加载 CA 私钥
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(caKeyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decode CA key PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	return caCert, caKey, nil
}

// getLocalIPs 获取本机所有非回环 IP 地址
func getLocalIPs() []net.IP {
	var ips []net.IP
	interfaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

func writeCertPEM(path string, derBytes []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create cert file: %w", err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey) error {
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
}
