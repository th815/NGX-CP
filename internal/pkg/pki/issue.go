package pki

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

const agentCertDefaultTTL = 365 * 24 * time.Hour

// IssueAgentCert 用 Agent 提交的 CSR 签发客户端证书。
// - nodeID 写入证书 Serial（控制面据此从 TLS 反查节点身份，零额外 token）
// - hostname 写入 CN 与 DNS SAN（Go 1.15+ 不再单看 CN，必须带 SAN）
// - ExtKeyUsage = ClientAuth
// 返回签发后的证书与 PEM（Agent 持久化到 /etc/ngxcp/agent.crt，权限 0600）。
func (ca *CA) IssueAgentCert(csrPEM []byte, nodeID int, hostname string, ttl time.Duration) (*x509.Certificate, []byte, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInvalid, "解析 CSR 失败", err)
	}
	if ttl <= 0 {
		ttl = agentCertDefaultTTL
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(int64(nodeID)),
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              []string{hostname},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(ttl),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "签发 Agent 证书失败", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "解析签发结果失败", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, out, nil
}

// RenewAgentCert 用原证书的公钥与身份续签（默认 1 年）。
// 到期前 30 天由控制面触发（T012 设计），保留 Serial=nodeID 以便身份连续。
func (ca *CA) RenewAgentCert(oldCert *x509.Certificate) (*x509.Certificate, []byte, error) {
	nodeID := int(oldCert.SerialNumber.Int64())
	hostname := oldCert.Subject.CommonName
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(int64(nodeID)),
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              oldCert.DNSNames,
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(agentCertDefaultTTL),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, oldCert.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "续签 Agent 证书失败", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "解析续签结果失败", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, out, nil
}

func parseCSR(pemBytes []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, apperr.New(apperr.CodeInvalid, "CSR PEM 解码失败")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "解析 CSR 失败", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "CSR 签名校验失败", err)
	}
	return csr, nil
}
