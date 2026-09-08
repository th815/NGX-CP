// Command ngxcp-agent 是部署在每台 Nginx/Keepalived 节点上的常驻代理。
// 注册（mTLS 引导）→ 重连心跳 → 响应控制面经心跳下发的执行型任务（校验/快照/落盘/回滚/LVS 权重）。
// 配置优先取环境变量，缺失时回落到 flag 默认值（见 runtime.Config）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/th/ngxcp/internal/agent/runtime"
	"github.com/th/ngxcp/internal/pkg/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	controlPlane := flag.String("control-plane", envOr("NGXCP_AGENT_CONTROL_PLANE", ""), "控制面 gRPC 地址，如 10.0.0.5:8443")
	caCert := flag.String("ca-cert", envOr("NGXCP_AGENT_CA_CERT", ""), "引导期 CA 证书路径（注册握手信任用）")
	enrollToken := flag.String("enroll-token", envOr("NGXCP_AGENT_ENROLL_TOKEN", ""), "一次性接入令牌（控制面生成，与节点绑定）。与 join-token 互斥")
	joinToken := flag.String("join-token", envOr("NGXCP_AGENT_JOIN_TOKEN", ""), "自注册 Join Token（控制面生成，未绑定节点）。节点自注册时自动建节点")
	serverName := flag.String("server-name", envOr("NGXCP_AGENT_SERVER_NAME", ""), "mTLS ServerName，默认取控制面地址的 host")
	hostname := flag.String("hostname", envOr("NGXCP_AGENT_HOSTNAME", ""), "本机 hostname（证书 SAN + 控制面展示）")
	dataDir := flag.String("data-dir", envOr("NGXCP_AGENT_DATA_DIR", "/var/lib/ngxcp"), "数据目录（证书/快照）")
	nginxPrefix := flag.String("nginx-prefix", envOr("NGXCP_AGENT_NGINX_PREFIX", "/etc/nginx"), "nginx prefix")
	nginxPath := flag.String("nginx-path", envOr("NGXCP_AGENT_NGINX_PATH", "/usr/sbin/nginx"), "nginx 二进制路径")
	confPath := flag.String("conf-path", envOr("NGXCP_AGENT_CONF_PATH", "nginx.conf"), "主配置相对 prefix")
	probeURL := flag.String("probe-url", envOr("NGXCP_AGENT_PROBE_URL", ""), "默认探活 URL（变更单可覆盖）")
	vips := flag.String("vips", envOr("NGXCP_AGENT_VIPS", ""), "LVS-DR 虚拟 IP 列表（逗号分隔，如 192.0.2.5/32,192.0.2.6/32），供 DR 合规自检 vip_on_lo 校验；空则跳过该项")
	flag.Parse()

	vipsList := splitList(*vips)

	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if *hostname == "" {
		*hostname, _ = os.Hostname()
	}

	cfg := runtime.Config{
		ControlPlaneAddr: *controlPlane,
		ServerName:       *serverName,
		CACertPath:       *caCert,
		EnrollToken:      *enrollToken,
		JoinToken:        *joinToken,
		Hostname:         *hostname,
		DataDir:          *dataDir,
		NginxPrefix:      *nginxPrefix,
		NginxPath:        *nginxPath,
		ConfPath:         *confPath,
		ProbeURL:         *probeURL,
		VIPs:             vipsList,
	}
	if cfg.ControlPlaneAddr == "" || cfg.CACertPath == "" || (cfg.EnrollToken == "" && cfg.JoinToken == "") {
		slog.Error("缺少必填参数", "control_plane", cfg.ControlPlaneAddr, "ca_cert", cfg.CACertPath, "enroll_or_join_token", cfg.EnrollToken == "" && cfg.JoinToken == "")
		fmt.Fprintln(os.Stderr, "用法: ngxcp-agent -control-plane <addr> -ca-cert <path> -enroll-token <token>  (或 -join-token <token>)")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runtime.Run(ctx, cfg); err != nil {
		slog.Error("agent exited", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList 按逗号拆分并裁剪空白，丢弃空项（用于 --vips 等列表参数）。
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
