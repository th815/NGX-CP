// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package runtime 落地 ngxcp-agent 常驻运行时的「接线」：注册（mTLS 引导）→ 重连心跳
// → 响应控制面经心跳命令通道下发的执行型任务（校验/快照/原子落盘/回滚/LVS 权重）。
//
// 设计要点：
//   - Agent 主动外连控制面，节点无入站端口（符合安全基线）。
//   - 注册用一次性 enroll token + 本地生成的 CSR 换发客户端证书，私钥永不出节点。
//   - 已签发的客户端证书持久化到 DataDir，重启免重注册（enroll token 一次性，重注册会失败）。
//   - 所有执行型命令经 Heartbeater 的回调分发到本地 executor（hostexec 真实执行）。
package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	agent "github.com/th/ngxcp/internal/agent"
	agentexec "github.com/th/ngxcp/internal/agent/executor"
	"github.com/th/ngxcp/internal/agent/health"
	"github.com/th/ngxcp/internal/agent/hostexec"
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/th/ngxcp/internal/pkg/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config 是 agent 运行时的引导配置（来自 flag/env/配置文件，不含机密，enroll token 例外）。
type Config struct {
	ControlPlaneAddr string // 控制面 gRPC 地址，如 10.0.0.5:8443
	ServerName       string // mTLS ServerName，默认取 ControlPlaneAddr 的 host
	CACertPath       string // 引导期 CA 证书（注册握手信任用），持久化后改用 DataDir/ca.crt
	EnrollToken      string // 一次性接入令牌（控制面生成，与节点绑定）。与 JoinToken 互斥。
	JoinToken        string // 自注册 Join Token（控制面生成，未绑定节点）。节点自注册时自动建节点。
	Hostname         string // 本机 hostname（证书 SAN + 控制面展示）
	DataDir          string // 数据目录（证书/快照），默认 /var/lib/ngxcp
	NginxPrefix      string // nginx prefix，默认 /etc/nginx
	NginxPath        string // nginx 二进制，默认 /usr/sbin/nginx
	ConfPath         string // 主配置相对 prefix，默认 nginx.conf
	ProbeURL         string // 默认探活 URL（可被变更单覆盖）
	VIPs             []string // LVS-DR 虚拟 IP（/32），供 DR 合规自检 vip_on_lo 校验；空则跳过该项
}

// Runtime 持有 agent 运行所需的执行器与配置。
type Runtime struct {
	cfg      Config
	log      *slog.Logger
	deploy   *agentexec.DeployExecutor
	rollback *agentexec.RollbackExecutor
	snapshot *agentexec.SnapshotExecutor
	ipvs     *agentexec.IPVSExecutor
	cert     *agentexec.CertDeployExecutor
	coll     *agent.Collector
	hbClient agentv1.AgentServiceClient // 用于 ReportCapability 一元 RPC
}

// Run 启动 agent 运行时：引导证书 → 建立 mTLS → 运行心跳（含自动重连）。
// 阻塞直到 ctx 取消。
func Run(ctx context.Context, cfg Config) error {
	log := slog.Default()
	if cfg.DataDir == "" {
		cfg.DataDir = "/var/lib/ngxcp"
	}
	if cfg.NginxPrefix == "" {
		cfg.NginxPrefix = "/etc/nginx"
	}
	if cfg.NginxPath == "" {
		cfg.NginxPath = "/usr/sbin/nginx"
	}
	if cfg.ConfPath == "" {
		cfg.ConfPath = "nginx.conf"
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = hostOf(cfg.ControlPlaneAddr)
	}

	mtlsCreds, _, err := bootstrapCert(ctx, cfg, serverName, log)
	if err != nil {
		return fmt.Errorf("bootstrap cert: %w", err)
	}

	// 建立 mTLS 长连接并启动心跳。
	conn, err := grpc.NewClient(cfg.ControlPlaneAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(mtlsCreds)))
	if err != nil {
		return fmt.Errorf("dial control-plane: %w", err)
	}
	defer conn.Close()
	api := agentv1.NewAgentServiceClient(conn)

	rt := &Runtime{
		cfg:      cfg,
		log:      log,
		deploy:   agentexec.NewDeployExecutor(hostexec.NewRealExecutor()),
		rollback: agentexec.NewRollbackExecutor(hostexec.NewRealExecutor()),
		snapshot: agentexec.NewSnapshotExecutor(),
		ipvs:     agentexec.NewIPVSExecutor(hostexec.NewRealExecutor()),
		cert:     agentexec.NewCertDeployExecutor(hostexec.NewRealExecutor()),
		coll:     agent.NewCollector(hostexec.NewRealExecutor(), log),
		hbClient: api,
	}
	cb := agent.HeartbeatCallbacks{
		ReportCapability:  rt.onReportCapability,
		ReportConfigTree:  rt.coll.CollectConfigTree,
		ReportLogTargets:  rt.coll.CollectLogTargets,
		ReportFsProbe:     rt.onReportFsProbe,
		ReportCompliance:  rt.onReportCompliance,
		DeployConfig:      rt.onDeploy,
		RollbackConfig:    rt.onRollback,
		CreateSnapshot:    rt.onCreateSnapshot,
		RestoreSnapshot:   rt.onRestoreSnapshot,
		SetRSWeight:       rt.onSetRSWeight,
		DeployCert:        rt.onDeployCert,
	}

	sc := &agentv1.ServerConfig{HeartbeatIntervalSec: 10, HeartbeatTimeoutSec: 30}
	hbCfg := agent.HeartbeatConfig{Interval: time.Duration(sc.GetHeartbeatIntervalSec()) * time.Second}

	hb := agent.NewHeartbeater(api, hbCfg, cb, log)
	log.Info("agent runtime started", "control_plane", cfg.ControlPlaneAddr, "hostname", cfg.Hostname)
	return hb.Run(ctx)
}

// bootstrapCert 加载/签发客户端证书：若 DataDir 已持久化证书则直接复用，否则用 enroll token 注册。
func bootstrapCert(ctx context.Context, cfg Config, serverName string, log *slog.Logger) (creds *tls.Config, caPEM []byte, err error) {
	certPath := filepath.Join(cfg.DataDir, "client.crt")
	keyPath := filepath.Join(cfg.DataDir, "client.key")
	caPath := filepath.Join(cfg.DataDir, "ca.crt")

	if fileExists(certPath) && fileExists(keyPath) && fileExists(caPath) {
		clientCert, e1 := os.ReadFile(certPath)
		keyPEM, e2 := os.ReadFile(keyPath)
		caPEM, e3 := os.ReadFile(caPath)
		if e1 == nil && e2 == nil && e3 == nil {
			creds, err = pki.ClientTLSConfig(caPEM, clientCert, keyPEM, serverName)
			if err == nil {
				log.Info("reused persisted client certificate")
				return creds, caPEM, nil
			}
		}
	}

	// 首次注册：先以引导 CA 建立仅 TLS 连接（不强制客户端证书）。
	bootstrapCA, rerr := os.ReadFile(cfg.CACertPath)
	if rerr != nil {
		return nil, nil, fmt.Errorf("read bootstrap CA: %w", rerr)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(bootstrapCA) {
		return nil, nil, fmt.Errorf("invalid bootstrap CA")
	}
	regConn, rerr := grpc.NewClient(cfg.ControlPlaneAddr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{ServerName: serverName, RootCAs: caPool})))
	if rerr != nil {
		return nil, nil, fmt.Errorf("dial register: %w", rerr)
	}
	defer regConn.Close()
	regClient := agentv1.NewAgentServiceClient(regConn)

	keyPEM, csrPEM, gerr := pki.GenerateKeyAndCSR(cfg.Hostname)
	if gerr != nil {
		return nil, nil, fmt.Errorf("generate key/csr: %w", gerr)
	}
	resp, rerr := regClient.Register(ctx, &agentv1.RegisterRequest{
		EnrollToken: cfg.EnrollToken,
		JoinToken:   cfg.JoinToken,
		Hostname:    cfg.Hostname,
		Csr:         csrPEM,
	})
	if rerr != nil {
		return nil, nil, fmt.Errorf("register: %w", rerr)
	}
	caPEM = resp.GetCaCert()

	// 持久化，避免重启重注册（enroll token 一次性）。
	_ = os.MkdirAll(cfg.DataDir, 0o700)
	_ = os.WriteFile(certPath, resp.GetClientCert(), 0o600)
	_ = os.WriteFile(keyPath, keyPEM, 0o600)
	_ = os.WriteFile(caPath, caPEM, 0o600)
	log.Info("registered with control-plane", "node_id", resp.GetNodeId(), "cert_expires_at", resp.GetCertExpiresAt())

	creds, err = pki.ClientTLSConfig(caPEM, resp.GetClientCert(), keyPEM, serverName)
	if err != nil {
		return nil, nil, fmt.Errorf("build mtls creds: %w", err)
	}
	return creds, caPEM, nil
}

// ── 控制面命令回调：把 proto 任务翻译为本地 executor 请求 ──

func (r *Runtime) onReportCapability(ctx context.Context) error {
	cap, err := r.coll.CollectCapability(ctx)
	if err != nil {
		return err
	}
	_, err = r.hbClient.ReportCapability(ctx, &agentv1.CapabilityReport{Capability: cap})
	return err
}

// onReportFsProbe 运行日志/FS 健康探测并经心跳流 FS_PROBE 上报（T018）。
// 探测项含磁盘使用率、证书有效期、配置权限、日志目录可写、error.log 雪崩、pid 文件存在性。
func (r *Runtime) onReportFsProbe(ctx context.Context) (*agentv1.FsProbeReport, error) {
	exec := hostexec.NewRealExecutor()
	opts := health.FsProbeOpts{
		NginxPrefix:   r.cfg.NginxPrefix,
		NginxConfPath: filepath.Join(r.cfg.NginxPrefix, r.cfg.ConfPath),
		SslDir:        filepath.Join(r.cfg.NginxPrefix, "ssl"),
		NginxPidPath:  "/var/run/nginx.pid",
	}
	return health.RunFsProbe(ctx, exec, opts)
}

// onReportCompliance 运行 DR 合规自检并经心跳流 COMPLIANCE 上报（T052）。
// 角色根据是否部署 keepalived 尽力推导；VIP 列表来自运行时配置（控制面下发的 LVS VIP），
// 空则 vip_on_lo 项标记跳过（不阻断）。检测结果经控制面 SetCompliance 驱动节点 degraded，
// 进而被 LVS 发布门禁（T055）拦截。
func (r *Runtime) onReportCompliance(ctx context.Context) (*agentv1.ComplianceReport, error) {
	exec := hostexec.NewRealExecutor()
	opts := health.ComplianceOpts{
		VIPs:              r.cfg.VIPs,
		Role:              r.resolveRole(exec),
		KeepalivedConfPath: "/etc/keepalived/keepalived.conf",
	}
	return health.RunCompliance(ctx, exec, opts)
}

// resolveRole 根据主机是否部署 keepalived 尽力推导节点角色（仅用于合规报告标注，不影响判定）。
func (r *Runtime) resolveRole(exec hostexec.CommandExecutor) string {
	if exec.Exists("/etc/keepalived/keepalived.conf") {
		return "director"
	}
	return "real_server"
}

func (r *Runtime) onDeploy(ctx context.Context, task *agentv1.SyncConfigTask, onProgress func(*agentv1.DeployProgress)) error {
	files := make([]agentexec.DeployFile, 0, len(task.GetFiles()))
	for _, f := range task.GetFiles() {
		files = append(files, agentexec.DeployFile{Path: f.GetPath(), Content: f.GetContent(), SHA256: f.GetSha256()})
	}
	coID := int(task.GetChangeOrderId())
	req := agentexec.DeployRequest{
		Files:         files,
		Prefix:        defaultStr(task.GetPrefix(), r.cfg.NginxPrefix),
		NginxPath:     defaultStr(task.GetNginxPath(), r.cfg.NginxPath),
		ConfPath:      defaultStr(task.GetConfPath(), r.cfg.ConfPath),
		ChangeOrderID: &coID,
		NodeID:        int(task.GetNodeId()),
		ObserveWindow: dur(task.GetObserveWindowSec(), 5),
		ProbeURL:      defaultStr(task.GetProbeUrl(), r.cfg.ProbeURL),
		ProbeTimeout:  dur(task.GetProbeTimeoutSec(), 5),
	}
	progress := make(chan agentexec.Progress, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			onProgress(&agentv1.DeployProgress{Step: p.Step, Status: p.Status, Message: p.Message})
		}
	}()
	_, err := r.deploy.Deploy(ctx, req, progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return err
}

func (r *Runtime) onRollback(ctx context.Context, task *agentv1.RollbackTask, onProgress func(*agentv1.DeployProgress)) error {
	req := agentexec.RollbackRequest{
		SnapshotPath:   task.GetSnapshotPath(),
		Prefix:         defaultStr(task.GetPrefix(), r.cfg.NginxPrefix),
		NginxPath:      defaultStr(task.GetNginxPath(), r.cfg.NginxPath),
		ConfPath:       defaultStr(task.GetConfPath(), r.cfg.ConfPath),
		ObserveWindow:  dur(task.GetObserveWindowSec(), 5),
		ProbeURL:       defaultStr(task.GetProbeUrl(), r.cfg.ProbeURL),
		ProbeTimeout:   dur(task.GetProbeTimeoutSec(), 5),
	}
	progress := make(chan agentexec.Progress, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progress {
			onProgress(&agentv1.DeployProgress{Step: p.Step, Status: p.Status, Message: p.Message})
		}
	}()
	_, err := r.rollback.Rollback(ctx, req, progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return err
}

func (r *Runtime) onCreateSnapshot(ctx context.Context, task *agentv1.SnapshotCreateTask) (*agentv1.SnapshotResult, error) {
	coID := int(task.GetChangeOrderId())
	req := agentexec.SnapshotRequest{
		Paths:         task.GetPaths(),
		IncludeSSL:    task.GetIncludeSsl(),
		StagingDir:    task.GetStagingDir(),
		ChangeOrderID: &coID,
		Type:          task.GetType(),
	}
	snap, err := r.snapshot.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	return &agentv1.SnapshotResult{TaskId: task.GetTaskId(), Ok: true, Path: snap.Path, Size: snap.Size}, nil
}

func (r *Runtime) onRestoreSnapshot(ctx context.Context, task *agentv1.SnapshotRestoreTask) (*agentv1.SnapshotResult, error) {
	if err := r.snapshot.Restore(ctx, agentexec.RestoreRequest{TarPath: task.GetTarPath(), Root: task.GetRoot()}); err != nil {
		return nil, err
	}
	return &agentv1.SnapshotResult{TaskId: task.GetTaskId(), Ok: true}, nil
}

func (r *Runtime) onSetRSWeight(ctx context.Context, task *agentv1.SetRealServerWeightTask) (*agentv1.SetRealServerWeightResult, error) {
	req := agentexec.SetRealServerWeightRequest{
		VIP:     task.GetVip(),
		VIPPort: int(task.GetVipPort()),
		Proto:   task.GetProto(),
		RSAddr:  task.GetRsAddr(),
		RSPort:  int(task.GetRsPort()),
		Weight:  int(task.GetWeight()),
	}
	if err := r.ipvs.SetRealServerWeight(ctx, req); err != nil {
		return nil, err
	}
	return &agentv1.SetRealServerWeightResult{TaskId: task.GetTaskId(), Ok: true}, nil
}

// onDeployCert 把控制面下发的证书落盘任务翻译为本地 CertDeployExecutor 请求（T044）。
// 私钥明文来自 task.KeyPem（仅经 mTLS 在途明文），Agent 落盘后不留存于其它位置。
func (r *Runtime) onDeployCert(ctx context.Context, task *agentv1.DeployCertTask) (*agentv1.DeployCertResult, error) {
	req := agentexec.CertDeployRequest{
		Domain:        task.GetDomain(),
		CertPEM:       task.GetCertPem(),
		KeyPEM:        task.GetKeyPem(),
		SSLDir:        defaultStr(task.GetSslDir(), r.cfg.NginxPrefix+"/ssl"),
		NginxPath:     defaultStr(task.GetNginxPath(), r.cfg.NginxPath),
		Reload:        task.GetReload(),
		ProbeURL:      task.GetProbeUrl(),
		ObserveWindow: dur(task.GetObserveWindowSec(), 5),
	}
	res, err := r.cert.Deploy(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	out := &agentv1.DeployCertResult{TaskId: task.GetTaskId(), Ok: res.OK}
	if !res.OK {
		out.Error = res.Error
	}
	if !res.DeployedAt.IsZero() {
		out.DeployedAt = res.DeployedAt.Unix()
	}
	return out, nil
}

// ── 小工具 ──

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func dur(sec int64, defSec int64) time.Duration {
	if sec <= 0 {
		sec = defSec
	}
	return time.Duration(sec) * time.Second
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
