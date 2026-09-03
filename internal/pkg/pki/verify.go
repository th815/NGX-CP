package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ServerTLSConfig 返回控制面 gRPC 服务端 mTLS 配置：
//   - 要求并校验客户端证书（RequireAndVerifyClientCert）
//   - 在 VerifyPeerCertificate 中从客户端证书 Serial 反查 nodeID，并交给 NodeValidator 校验节点可接入
//
// 注意：服务端自身证书（ServerAuth）也由本 CA 签发，故 Agent 侧用同一 CA 证书即可信任。
func (ca *CA) ServerTLSConfig() (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.certPEM) {
		return nil, apperr.New(apperr.CodeInternal, "CA 证书无法加入信任池")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{ca.serverTLS},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("缺少客户端证书")
			}
			peer, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("解析客户端证书失败: %w", err)
			}
			// 显式校验：客户端证书必须由本控制面 CA 签发（不依赖 Go 内建链校验的歧义行为）。
			if !ca.Cert.IsCA {
				return fmt.Errorf("控制面证书不是 CA")
			}
			if err := peer.CheckSignatureFrom(ca.Cert); err != nil {
				return fmt.Errorf("客户端证书非本 CA 签发: %w", err)
			}
			// 显式校验有效期（截止/未生效均拒绝）。
			now := time.Now()
			if now.Before(peer.NotBefore) || now.After(peer.NotAfter) {
				return fmt.Errorf("客户端证书不在有效期内 (not_before=%v not_after=%v)", peer.NotBefore, peer.NotAfter)
			}
			nodeID := int(peer.SerialNumber.Int64())
			if ca.NodeValidator != nil {
				return ca.NodeValidator(nodeID)
			}
			return nil
		},
	}, nil
}

// ClientTLSConfig 构造 Agent 侧 mTLS 配置：携带客户端证书，并信任控制面 CA。
// serverName 必须匹配服务端证书的 DNS SAN（默认 "ngxcp-server"）。
func ClientTLSConfig(caCertPEM, clientCertPEM, clientKeyPEM []byte, serverName string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalid, "客户端证书对无效", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, apperr.New(apperr.CodeInvalid, "CA 证书无效")
	}
	if serverName == "" {
		serverName = serverCommonName
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
