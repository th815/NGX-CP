package transport

import (
	"github.com/th/ngxcp/internal/pkg/pki"
	"google.golang.org/grpc/credentials"
)

// ServerCredentials 返回 gRPC 服务端 mTLS 凭证（T014 接线时用于 grpc.Creds(...)）。
func ServerCredentials(ca *pki.CA) (credentials.TransportCredentials, error) {
	cfg, err := ca.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

// ClientCredentials 返回 Agent 侧 mTLS 凭证。
func ClientCredentials(caCertPEM, clientCertPEM, clientKeyPEM []byte, serverName string) (credentials.TransportCredentials, error) {
	cfg, err := pki.ClientTLSConfig(caCertPEM, clientCertPEM, clientKeyPEM, serverName)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}
