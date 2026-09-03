// Package pki 实现控制面 PKI：自签 CA（10 年）+ 服务端证书，以及 Agent 客户端证书签发与 mTLS 校验。
// 安全底线：Agent 私钥在节点本地生成、永不出节点；控制面仅持有 CA 私钥，并用证书 Serial 反查 nodeID。
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

const (
	caCertFilename     = "ca.crt"
	caKeyFilename      = "ca.key"
	serverCertFilename = "server.crt"
	serverKeyFilename  = "server.key"

	caCommonName     = "ngxcp-agent-ca"
	serverCommonName = "ngxcp-server"

	caValidityYears = 10
	serverValidity  = 10 * 365 * 24 * time.Hour
)

// CA 持有控制面根证书、私钥，以及由该 CA 签发的服务端证书。
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	certPEM []byte
	keyPEM  []byte

	serverTLS tls.Certificate // 服务端证书对（ServerAuth），供 gRPC mTLS 使用

	// NodeValidator 校验从客户端证书 Serial 反查到的 nodeID 是否允许接入。
	// 默认放行；生产环境应查询 DB（节点存在、状态正常、未吊销）。
	NodeValidator func(nodeID int) error
}

func defaultAllow(int) error { return nil }

// LoadOrCreateCA 加载 dir 下的 CA；缺失则生成并落盘。
// 权限：ca.key / server.key → 0600（务必备份，丢失需全部 Agent 重新注册）；ca.crt / server.crt → 0644。
func LoadOrCreateCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFilename))
	if err == nil {
		keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFilename))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeUnavailable, "读取 CA 私钥失败", err)
		}
		serverCertPEM, err := os.ReadFile(filepath.Join(dir, serverCertFilename))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeUnavailable, "读取服务端证书失败", err)
		}
		serverKeyPEM, err := os.ReadFile(filepath.Join(dir, serverKeyFilename))
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeUnavailable, "读取服务端私钥失败", err)
		}
		return newCAFromPEM(certPEM, keyPEM, serverCertPEM, serverKeyPEM)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建 PKI 目录失败", err)
	}
	certPEM, keyPEM, serverCertPEM, serverKeyPEM, err := generateCA()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, caCertFilename), certPEM, 0o644); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "写入 CA 证书失败", err)
	}
	if err := os.WriteFile(filepath.Join(dir, caKeyFilename), keyPEM, 0o600); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "写入 CA 私钥失败（应为 0600）", err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverCertFilename), serverCertPEM, 0o644); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "写入服务端证书失败", err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverKeyFilename), serverKeyPEM, 0o600); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "写入服务端私钥失败（应为 0600）", err)
	}
	return newCAFromPEM(certPEM, keyPEM, serverCertPEM, serverKeyPEM)
}

func newCAFromPEM(certPEM, keyPEM, serverCertPEM, serverKeyPEM []byte) (*CA, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "解析 CA 证书失败", err)
	}
	key, err := parseECPrivateKey(keyPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "解析 CA 私钥失败", err)
	}
	serverTLS, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "加载服务端证书对失败", err)
	}
	return &CA{
		Cert:          cert,
		Key:           key,
		certPEM:       certPEM,
		keyPEM:        keyPEM,
		serverTLS:     serverTLS,
		NodeValidator: defaultAllow,
	}, nil
}

// CACertPEM 返回 CA 证书 PEM（供 Agent 侧信任池使用）。
func (ca *CA) CACertPEM() []byte { return ca.certPEM }

func generateCA() (certPEM, keyPEM, serverCertPEM, serverKeyPEM []byte, err error) {
	caKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, nil, nil, nil, apperr.Wrap(apperr.CodeInternal, "生成 CA 密钥失败", e)
	}
	sn, e := randSerial()
	if e != nil {
		return
	}
	caTmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: caCommonName, Organization: []string{"ngxcp"}},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().AddDate(caValidityYears, 0, 0),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		BasicConstraintsValid: true,
		IsCA:                   true,
		MaxPathLenZero:         true,
	}
	caDER, e := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if e != nil {
		return nil, nil, nil, nil, apperr.Wrap(apperr.CodeInternal, "自签 CA 失败", e)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyPEM, e = marshalECKey(caKey)
	if e != nil {
		return
	}

	// 服务端证书（ServerAuth），供控制面 gRPC 向 Agent 出示。
	svrKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return
	}
	svrSN, e := randSerial()
	if e != nil {
		return
	}
	svrTmpl := &x509.Certificate{
		SerialNumber: svrSN,
		Subject:      pkix.Name{CommonName: serverCommonName},
		DNSNames:     []string{serverCommonName, "localhost"},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	svrDER, e := x509.CreateCertificate(rand.Reader, svrTmpl, caTmpl, &svrKey.PublicKey, caKey)
	if e != nil {
		return nil, nil, nil, nil, apperr.Wrap(apperr.CodeInternal, "签发服务端证书失败", e)
	}
	serverCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: svrDER})
	serverKeyPEM, e = marshalECKey(svrKey)
	return
}

// randSerial 生成 128 位正随机序列号。
func randSerial() (*big.Int, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "生成序列号失败", err)
	}
	b[0] &= 0x7f // 保证正数
	return new(big.Int).SetBytes(b), nil
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, apperr.New(apperr.CodeInvalid, "PEM 解码失败")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseECPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, apperr.New(apperr.CodeInvalid, "私钥 PEM 解码失败")
	}
	keyI, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "解析 EC 私钥失败", err)
	}
	key, ok := keyI.(*ecdsa.PrivateKey)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalid, "非 EC 私钥")
	}
	return key, nil
}

func marshalECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "编码 EC 私钥失败", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// EncodePrivateKey 将 EC 私钥编码为 PKCS#8 PEM（Agent 侧持久化用）。
func EncodePrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	return marshalECKey(key)
}
