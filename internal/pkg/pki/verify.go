package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// ServerTLSConfig 返回控制面**强 mTLS** 配置：要求并校验客户端证书（RequireAndVerifyClientCert）。
// 适用于 Agent 已持有客户端证书后的长连接（心跳 / 能力上报），以及非 gRPC 的 mTLS 场景。
//
// 注意：服务端自身证书（ServerAuth）也由本 CA 签发，故 Agent 侧用同一 CA 证书即可信任。
func (ca *CA) ServerTLSConfig() (*tls.Config, error) {
	cfg, err := ca.baseTLSConfig(tls.RequireAndVerifyClientCert)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// GRPCServerTLSConfig 返回 gRPC 服务端配置：客户端证书「若提供则校验」（VerifyClientCertIfGiven）。
// 原因：Register RPC 在 Agent 尚无客户端证书时通过 TLS+token 引导，不能强制要求客户端证书；
// 其余 RPC（Heartbeat / ReportCapability）由传输层拦截器强制要求客户端证书（见 internal/agent/transport）。
func (ca *CA) GRPCServerTLSConfig() (*tls.Config, error) {
	return ca.baseTLSConfig(tls.VerifyClientCertIfGiven)
}

// baseTLSConfig 构造带服务端证书与显式证书校验的服务端 TLS 配置。
// clientAuth 控制是否强制客户端证书：RequireAndVerifyClientCert（强 mTLS）或 VerifyClientCertIfGiven（gRPC 引导期）。
func (ca *CA) baseTLSConfig(clientAuth tls.ClientAuthType) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.certPEM) {
		return nil, apperr.New(apperr.CodeInternal, "CA 证书无法加入信任池")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{ca.serverTLS},
		ClientAuth:   clientAuth,
		ClientCAs:    pool,
		// 显式校验：不依赖 Go 内建链校验的歧义行为（TLS 1.3 下客户端可能抢先完成握手）。
		// 校验客户端证书必须由本 CA 签发 + 在有效期内 +（可选）NodeValidator 反查 nodeID。
		// clientAuth=VerifyClientCertIfGiven 时允许「缺证书」（注册引导期），由上层 gRPC 拦截器按需强制 mTLS。
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				if clientAuth == tls.RequireAndVerifyClientCert {
					return fmt.Errorf("缺少客户端证书")
				}
				return nil
			}
			return ca.verifyPeer(rawCerts)
		},
	}, nil
}

// verifyPeer 显式校验对端客户端证书：本 CA 签发 + 有效期内 + NodeValidator 反查 nodeID。
func (ca *CA) verifyPeer(rawCerts [][]byte) error {
	peer, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("解析客户端证书失败: %w", err)
	}
	if !ca.Cert.IsCA {
		return fmt.Errorf("控制面证书不是 CA")
	}
	if err := peer.CheckSignatureFrom(ca.Cert); err != nil {
		return fmt.Errorf("客户端证书非本 CA 签发: %w", err)
	}
	now := time.Now()
	if now.Before(peer.NotBefore) || now.After(peer.NotAfter) {
		return fmt.Errorf("客户端证书不在有效期内 (not_before=%v not_after=%v)", peer.NotBefore, peer.NotAfter)
	}
	if ca.NodeValidator != nil {
		return ca.NodeValidator(int(peer.SerialNumber.Int64()))
	}
	return nil
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
