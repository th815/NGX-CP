// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令（T024 校验 / T031 快照 / T032 原子落盘 / T044 证书落盘）。
//
// CertDeployExecutor 是 T044 的核心算法：把"下发证书"做成可校验、可回滚的原子操作——
// 与 T032 配置落盘同构，但更轻量（单文件对 crt/key，无需 nginx -t 完整上下文）：
//   staging 写两文件 → 同盘 rename 原子切换 → chmod(crt 0644 / key 0600) + chown root:root
//   → nginx -t → reload → 探活(443 TLS)；任一步失败从切换前的快照恢复旧证书并重载。
//
// 关键保证：reload 在切换之后（证书替换即生效），但切换前已拍快照，reload/探活失败
// 会把旧证书还原回去，保证 nginx 始终能起（不残留半截证书导致全部 vhost 起不来）。
// 所有外部命令经 CommandRunner 抽象，单测用 fake 注入，无需真实 nginx 二进制。
package executor

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/th/ngxcp/internal/agent/probe"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/atomicfile"
)

// CertDeployRequest 是一次证书落盘请求（纯数据，便于单测注入）。
type CertDeployRequest struct {
	Domain        string // 主域名，用于文件名 <domain>.{crt,key}
	CertPEM       string // full chain（leaf + intermediate）
	KeyPEM        string // 私钥明文（仅经 mTLS 下发的在途明文，不落盘明文到其它位置）
	SSLDir        string // 落盘目录，默认 /etc/nginx/ssl
	NginxPath     string // nginx 二进制，默认 /usr/sbin/nginx
	Reload        bool   // 落盘后是否 reload，默认 true
	ProbeURL      string // 探活 URL，空则跳过探活
	ObserveWindow time.Duration
	StagingDir    string // 临时目录（默认系统临时目录子目录）
}

// CertProgress 是证书落盘每一步的进度上报。
type CertProgress struct {
	Step    string // snapshot|write|chmod|switch|reload|wait|probe|report|rollback
	Status  string // running|success|failed
	Message string
}

// CertDeployResult 是 CertDeploy 的结果摘要。
type CertDeployResult struct {
	OK        bool
	Error     string
	DeployedAt time.Time
}

// CertDeployExecutor 在节点上执行证书原子落盘（T044）。
type CertDeployExecutor struct {
	run    CommandRunner
	prober Prober
}

// NewCertDeployExecutor 用真实命令执行器构造（Agent 运行时使用 hostexec）。
func NewCertDeployExecutor(run CommandRunner) *CertDeployExecutor {
	return &CertDeployExecutor{run: run}
}

// NewCertDeployExecutorWithRunner 便于单测注入自定义执行函数。
func NewCertDeployExecutorWithRunner(run func(ctx context.Context, name string, args ...string) (string, error)) *CertDeployExecutor {
	return &CertDeployExecutor{run: runnerFunc(run)}
}

// SetProber 注入自定义探活器（默认按 CertDeployRequest.ProbeURL 构造 HTTPProber）。
func (e *CertDeployExecutor) SetProber(p Prober) { e.prober = p }

// Deploy 执行证书原子落盘流水线。progress 可为 nil（不发送进度）。
// 任何一步失败：若已切换，则从切换前快照恢复旧证书并重载，再返回错误。
func (e *CertDeployExecutor) Deploy(ctx context.Context, req CertDeployRequest, progress chan<- CertProgress) (*CertDeployResult, error) {
	sslDir := req.SSLDir
	if sslDir == "" {
		sslDir = "/etc/nginx/ssl"
	}
	nginxPath := req.NginxPath
	if nginxPath == "" {
		nginxPath = "/usr/sbin/nginx"
	}
	reload := req.Reload
	observe := req.ObserveWindow
	if observe <= 0 {
		observe = 5 * time.Second
	}
	prober := e.prober
	if prober == nil && req.ProbeURL != "" {
		prober = probeAdapter{probe.NewHTTPProbe(req.ProbeURL, 0, observe)}
	}

	crtName := req.Domain + ".crt"
	keyName := req.Domain + ".key"
	crtPath := filepath.Join(sslDir, crtName)
	keyPath := filepath.Join(sslDir, keyName)

	staging := req.StagingDir
	if staging == "" {
		d, mkErr := os.MkdirTemp("", "ngxcp-cert-")
		if mkErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "创建 staging 失败", mkErr)
		}
		staging = d
	}
	defer os.RemoveAll(staging)

	stagingCrt := filepath.Join(staging, crtName)
	stagingKey := filepath.Join(staging, keyName)

	// ① 切换前快照：若目标证书已存在，备份到 staging 以便失败回滚。
	e.emit(progress, "snapshot", "running", "")
	backupCrt := filepath.Join(staging, "old-"+crtName)
	backupKey := filepath.Join(staging, "old-"+keyName)
	if err := snapshotFile(crtPath, backupCrt); err != nil {
		e.emit(progress, "snapshot", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "快照旧证书失败", err)
	}
	if err := snapshotFile(keyPath, backupKey); err != nil {
		e.emit(progress, "snapshot", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "快照旧私钥失败", err)
	}
	e.emit(progress, "snapshot", "success", "")

	// ② 写入 staging（私钥严格 0600，公钥 0644）。
	e.emit(progress, "write", "running", "")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		e.emit(progress, "write", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "创建 staging 目录失败", err)
	}
	if err := os.WriteFile(stagingKey, []byte(req.KeyPEM), 0o600); err != nil {
		e.emit(progress, "write", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "写入私钥 staging 失败", err)
	}
	if err := os.WriteFile(stagingCrt, []byte(req.CertPEM), 0o644); err != nil {
		e.emit(progress, "write", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "写入证书 staging 失败", err)
	}
	e.emit(progress, "write", "success", "")

	// ③ 权限校正（确保即便 umask 干扰也正确）。
	e.emit(progress, "chmod", "running", "")
	if err := os.Chmod(stagingKey, 0o600); err != nil {
		e.emit(progress, "chmod", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "私钥 chmod 失败", err)
	}
	if err := os.Chmod(stagingCrt, 0o644); err != nil {
		e.emit(progress, "chmod", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "证书 chmod 失败", err)
	}
	e.emit(progress, "chmod", "success", "")

	// ④ 原子切换：同盘 rename（跨盘降级 copy 由 atomicfile 处理）。
	e.emit(progress, "switch", "running", "")
	if err := os.MkdirAll(sslDir, 0o755); err != nil {
		e.emit(progress, "switch", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "创建 ssl 目录失败", err)
	}
	if err := atomicfile.MoveFile(stagingKey, keyPath); err != nil {
		e.emit(progress, "switch", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "私钥切换失败", err)
	}
	if err := atomicfile.MoveFile(stagingCrt, crtPath); err != nil {
		// 私钥已切换、证书未切换：回滚私钥，整体失败（nginx 仍能读旧 key）。
		_ = restoreFile(backupKey, keyPath)
		e.emit(progress, "switch", "failed", err.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "证书切换失败", err)
	}
	// chown root:root（Agent 以 root 运行时生效；非 root 尽力而为忽略）。
	chownRoot(keyPath)
	chownRoot(crtPath)
	e.emit(progress, "switch", "success", "")

	// ⑤ reload（先 nginx -t 语法校验，再 reload）。
	if reload {
		e.emit(progress, "reload", "running", "")
		if out, terr := e.run.Output(ctx, nginxPath, "-t"); terr != nil {
			// 语法校验失败：还原旧证书并重载，返回失败（线上零变化）。
			msg := "nginx -t 校验失败: " + out
			if rerr := e.rollbackCert(ctx, nginxPath, backupCrt, backupKey, crtPath, keyPath, progress, msg); rerr != nil {
				return &CertDeployResult{OK: false, Error: msg + "；回滚亦失败: " + rerr.Error()}, nil
			}
			return &CertDeployResult{OK: false, Error: msg + "；已回滚到旧证书"}, nil
		}
		if rerr := e.reload(ctx, nginxPath); rerr != nil {
			msg := "reload 失败"
			if berr := e.rollbackCert(ctx, nginxPath, backupCrt, backupKey, crtPath, keyPath, progress, msg); berr != nil {
				return &CertDeployResult{OK: false, Error: msg + "；回滚亦失败: " + berr.Error()}, nil
			}
			return &CertDeployResult{OK: false, Error: msg + "；已回滚到旧证书"}, nil
		}
		e.emit(progress, "reload", "success", "")
	}

	// ⑥ 等待新 worker 稳定。
	e.emit(progress, "wait", "running", "")
	select {
	case <-time.After(observe):
	case <-ctx.Done():
		return &CertDeployResult{OK: false, Error: ctx.Err().Error()}, nil
	}
	e.emit(progress, "wait", "success", "")

	// ⑦ 探活：失败则回滚。
	if prober != nil {
		e.emit(progress, "probe", "running", "")
		ok, detail, perr := prober.Probe(ctx)
		if perr != nil || !ok {
			msg := detail
			if perr != nil {
				msg = perr.Error()
			}
			e.emit(progress, "probe", "failed", msg)
			if berr := e.rollbackCert(ctx, nginxPath, backupCrt, backupKey, crtPath, keyPath, progress, "探活失败"); berr != nil {
				return &CertDeployResult{OK: false, Error: "探活失败（" + msg + "）；回滚亦失败: " + berr.Error()}, nil
			}
			return &CertDeployResult{OK: false, Error: "探活失败（" + msg + "）；已回滚到旧证书"}, nil
		}
		e.emit(progress, "probe", "success", detail)
	}

	// ⑧ 上报。
	e.emit(progress, "report", "success", "")
	return &CertDeployResult{OK: true, DeployedAt: time.Now()}, nil
}

// rollbackCert 把旧证书还原回目标路径并 reload，使 nginx 回到变更前状态。
func (e *CertDeployExecutor) rollbackCert(ctx context.Context, nginxPath, backupCrt, backupKey, crtPath, keyPath string, progress chan<- CertProgress, reason string) error {
	e.emit(progress, "rollback", "running", reason)
	if err := restoreFile(backupKey, keyPath); err != nil {
		e.emit(progress, "rollback", "failed", err.Error())
		return err
	}
	if err := restoreFile(backupCrt, crtPath); err != nil {
		e.emit(progress, "rollback", "failed", err.Error())
		return err
	}
	if err := e.reload(ctx, nginxPath); err != nil {
		e.emit(progress, "rollback", "failed", err.Error())
		return err
	}
	e.emit(progress, "rollback", "success", "")
	return nil
}

// reload 执行 nginx 平滑加载（默认 `nginx -s reload`）。
func (e *CertDeployExecutor) reload(ctx context.Context, nginxPath string) error {
	out, err := e.run.Output(ctx, nginxPath, "-s", "reload")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "执行 reload 失败: "+out, err)
	}
	return nil
}

// snapshotFile 若 src 存在则原样复制到 dst（保留权限与内容），不存在则视为无旧文件（no-op）。
func snapshotFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次部署，无旧文件可快照
		}
		return err
	}
	info, statErr := os.Stat(src)
	mode := os.FileMode(0o644)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(dst, data, mode)
}

// restoreFile 若备份存在则原子还原到 dst（dst 当前内容被覆盖）。
func restoreFile(backup, dst string) error {
	if _, err := os.Stat(backup); err != nil {
		if os.IsNotExist(err) {
			// 无备份（首次部署）且当前切换失败：删除已切换的文件，回到"无证书"状态。
			_ = os.Remove(dst)
			return nil
		}
		return err
	}
	return atomicfile.MoveFile(backup, dst)
}

// chownRoot 尽力把文件属主改为 root:root（非 root 运行时忽略错误）。
func chownRoot(path string) {
	_ = os.Chown(path, 0, 0)
}

// emit 非阻塞发送进度（channel 满或 nil 时丢弃）。
func (e *CertDeployExecutor) emit(p chan<- CertProgress, step, status, msg string) {
	if p == nil {
		return
	}
	select {
	case p <- CertProgress{Step: step, Status: status, Message: msg}:
	default:
	}
}
