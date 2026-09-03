package transport

import (
	"context"
	"time"

	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Client 是 Agent 侧 gRPC 封装，供 T014 注册与 T015 心跳使用。
type Client struct {
	conn *grpc.ClientConn
	api  agentv1.AgentServiceClient
}

// Dial 建立到控制面的 gRPC 连接。
// 注册阶段（Agent 尚无客户端证书）传入仅 TLS 的 creds（或 insecure，仅限内网）；
// 注册后拿到客户端证书，应以 mTLS creds 重连（见 T012）。
func Dial(ctx context.Context, addr string, creds credentials.TransportCredentials, opts ...grpc.DialOption) (*Client, error) {
	base := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	base = append(base, opts...)
	conn, err := grpc.NewClient(addr, base...)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, api: agentv1.NewAgentServiceClient(conn)}, nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	return c.conn.Close()
}

// Register 调用注册 RPC。
func (c *Client) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.api.Register(ctx, req)
}

// Heartbeat 打开双向流（T015 使用）。
func (c *Client) Heartbeat(ctx context.Context) (agentv1.AgentService_HeartbeatClient, error) {
	return c.api.Heartbeat(ctx)
}

// ReportCapability 上报能力基线。
func (c *Client) ReportCapability(ctx context.Context, req *agentv1.CapabilityReport) (*agentv1.Ack, error) {
	return c.api.ReportCapability(ctx, req)
}
