// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令（T024 校验 / T031 快照 / T032 原子落盘 / T034 回滚）。
//
// RollbackExecutor 是回滚的核心算法：把"恢复到变更前状态"拆成 8 步流水线，
// 关键保证与 T032 同构——「校验在恢复之前」：先解压快照到 staging 跑 nginx -t，
// 快照配置本身若有问题则**绝不触碰线上**（prefix 保持 deploy 失败态），直接报 rollback_failed。
// 恢复动作复用 T031 的 SnapshotExecutor.Restore（含权限/属主还原）；探活失败也报 rollback_failed。
package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/th/ngxcp/internal/agent/probe"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// RollbackMode 选择回滚策略（T034）。Agent 侧只实现 snapshot 模式；
// revision 模式由控制面在配置版本链层完成，不经此执行器。
type RollbackMode string

const (
	RollbackSnapshot RollbackMode = "snapshot" // 从文件快照恢复（默认，最可靠）
	RollbackRevision RollbackMode = "revision" // 从配置版本链恢复（更快，仅改配置）
)

// RollbackRequest 是一次回滚请求（纯数据，便于单测注入）。
type RollbackRequest struct {
	SnapshotPath  string        // 预变更快照 tar.gz 绝对路径
	Prefix        string        // 真实 nginx prefix，默认 /etc/nginx
	NginxPath     string        // nginx 二进制，默认 /usr/sbin/nginx
	ConfPath      string        // 主配置相对 prefix，默认 nginx.conf
	RestoreRoot   string        // 恢复根（默认 "/"；单测用沙盒祖父目录避免写系统）
	StagingDir    string        // 校验用临时解包目录，空则默认
	ObserveWindow time.Duration // 步骤⑤ 观测窗口，默认 5s
	ProbeURL      string        // 步骤⑥ 探活 URL，空则跳过
	ProbeTimeout  time.Duration // 探活超时，默认 5s
	ReloadArgs    []string      // reload 命令参数，默认 ["-s","reload"]
}

// RollbackResult 是回滚结果摘要。
type RollbackResult struct {
	RolledBack bool   // 是否真正恢复了文件（步骤③ 已执行）
	Error      string // rollback_failed 原因；非空表示回滚本身失败（最危险状态）
}

// RollbackExecutor 在节点上执行回滚（T034）。
type RollbackExecutor struct {
	run       CommandRunner
	snapper   *SnapshotExecutor
	validator *Executor // T024 校验执行器（同包）
	prober    Prober
}

// NewRollbackExecutor 用真实命令执行器构造（Agent 运行时使用 hostexec）。
func NewRollbackExecutor(run CommandRunner) *RollbackExecutor {
	return &RollbackExecutor{run: run, snapper: NewSnapshotExecutor(), validator: NewExecutor(run)}
}

// NewRollbackExecutorWithRunner 便于单测注入自定义执行函数。
func NewRollbackExecutorWithRunner(run func(ctx context.Context, name string, args ...string) (string, error)) *RollbackExecutor {
	cr := runnerFunc(run)
	return &RollbackExecutor{run: cr, snapper: NewSnapshotExecutor(), validator: NewExecutor(cr)}
}

// SetProber 注入自定义探活器（默认按 RollbackRequest.ProbeURL 构造 HTTP 探活）。
func (e *RollbackExecutor) SetProber(p Prober) { e.prober = p }

// SetProbeConfigs 用一组探活配置构造复合探活器（全部通过才健康）。
func (e *RollbackExecutor) SetProbeConfigs(cfgs ...probe.ProbeConfig) error {
	cp, err := probe.Composite(cfgs)
	if err != nil {
		return err
	}
	e.prober = probeAdapter{cp}
	return nil
}

// Rollback 执行 8 步回滚流水线。progress 可为 nil。
// 任何一步失败（尤其恢复后 reload/探活失败）返回含 Error 的 RollbackResult，表示 rollback_failed。
func (e *RollbackExecutor) Rollback(ctx context.Context, req RollbackRequest, progress chan<- Progress) (*RollbackResult, error) {
	prefix := req.Prefix
	if prefix == "" {
		prefix = "/etc/nginx"
	}
	nginxPath := req.NginxPath
	if nginxPath == "" {
		nginxPath = "/usr/sbin/nginx"
	}
	confPath := req.ConfPath
	if confPath == "" {
		confPath = "nginx.conf"
	}
	restoreRoot := req.RestoreRoot
	if restoreRoot == "" {
		restoreRoot = "/"
	}
	observe := req.ObserveWindow
	if observe <= 0 {
		observe = 5 * time.Second
	}
	prober := e.prober
	if prober == nil && req.ProbeURL != "" {
		prober = probeAdapter{probe.NewHTTPProbe(req.ProbeURL, 0, req.ProbeTimeout)}
	}

	staging := req.StagingDir
	if staging == "" {
		d, mkErr := os.MkdirTemp("", "ngxcp-rollback-")
		if mkErr != nil {
			return &RollbackResult{Error: "创建 staging 失败"}, apperr.Wrap(apperr.CodeInternal, "回滚失败：创建 staging", mkErr)
		}
		staging = d
	}
	defer os.RemoveAll(staging)

	if req.SnapshotPath == "" {
		return &RollbackResult{Error: "缺少快照路径"}, apperr.New(apperr.CodeInvalid, "回滚失败：缺少快照路径")
	}

	// ① 解压快照到 staging（Root=staging），仅用于校验，绝不触碰 prefix
	e.emit(progress, "extract", "running", "解压快照到 staging 校验")
	if err := e.snapper.Restore(ctx, RestoreRequest{TarPath: req.SnapshotPath, Root: staging}); err != nil {
		e.emit(progress, "extract", "failed", err.Error())
		return &RollbackResult{Error: "解压快照失败: " + err.Error()},
			apperr.Wrap(apperr.CodeInternal, "回滚失败：解压快照", err)
	}
	e.emit(progress, "extract", "success", "")

	// ② nginx -t 校验 staging 内配置（★ 快照也可能坏；此时 prefix 尚未改动）
	e.emit(progress, "validate", "running", "校验快照配置")
	relPrefix := strings.TrimPrefix(prefix, "/") // "etc/nginx"
	vresp, verr := e.validator.Validate(ctx, ValidateRequest{
		NginxPath: nginxPath, Prefix: filepath.Join(staging, relPrefix), ConfPath: confPath,
	})
	if verr != nil || vresp == nil || !vresp.OK {
		// 快照配置坏 → 不能恢复，prefix 保持 deploy 失败态，回滚失败（最危险）
		msg := firstValidateErr(vresp, verr)
		e.emit(progress, "validate", "failed", msg)
		return &RollbackResult{Error: "快照配置校验失败，无法回滚: " + msg},
			apperr.Wrap(apperr.CodePrecondition, "回滚失败：快照配置不可恢复", verr)
	}
	e.emit(progress, "validate", "success", "")

	// ③ 真正恢复：Restore 到 restoreRoot（原始绝对路径，含权限/属主还原）
	e.emit(progress, "restore", "running", "恢复文件到 "+prefix)
	if err := e.snapper.Restore(ctx, RestoreRequest{TarPath: req.SnapshotPath, Root: restoreRoot}); err != nil {
		e.emit(progress, "restore", "failed", err.Error())
		return &RollbackResult{Error: "恢复快照失败: " + err.Error()},
			apperr.Wrap(apperr.CodeInternal, "回滚失败：恢复快照", err)
	}
	e.emit(progress, "restore", "success", "")

	// ④ 平滑加载
	e.emit(progress, "reload", "running", "")
	if rerr := e.reload(ctx, nginxPath, req.ReloadArgs); rerr != nil {
		e.emit(progress, "reload", "failed", rerr.Error())
		return &RollbackResult{RolledBack: true, Error: "reload 失败: " + rerr.Error()},
			apperr.Wrap(apperr.CodeInternal, "回滚失败：reload", rerr)
	}
	e.emit(progress, "reload", "success", "")

	// ⑤ 等待新 worker 稳定
	e.emit(progress, "wait", "running", "")
	select {
	case <-time.After(observe):
	case <-ctx.Done():
		return &RollbackResult{RolledBack: true, Error: "回滚观测窗口被取消"}, ctx.Err()
	}
	e.emit(progress, "wait", "success", "")

	// ⑥ 探活：失败也报 rollback_failed（恢复已生效，但节点仍不健康）
	if prober != nil {
		e.emit(progress, "probe", "running", "")
		ok, detail, perr := prober.Probe(ctx)
		if perr != nil || !ok {
			msg := detail
			if perr != nil {
				msg = perr.Error()
			}
			e.emit(progress, "probe", "failed", msg)
			return &RollbackResult{RolledBack: true, Error: "回滚后探活失败: " + msg},
				apperr.Wrap(apperr.CodePrecondition, "回滚失败：回滚后探活仍不通过", perr)
		}
		e.emit(progress, "probe", "success", detail)
	}

	// ⑦ 上报
	e.emit(progress, "report", "success", "")
	return &RollbackResult{RolledBack: true}, nil
}

// reload 执行 nginx 平滑加载（默认 `nginx -s reload`）。
func (e *RollbackExecutor) reload(ctx context.Context, nginxPath string, args []string) error {
	if len(args) == 0 {
		args = []string{"-s", "reload"}
	}
	out, err := e.run.Output(ctx, nginxPath, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "执行 reload 失败: "+out, err)
	}
	return nil
}

// emit 非阻塞发送进度（channel 满或 nil 时丢弃）。
func (e *RollbackExecutor) emit(p chan<- Progress, step, status, msg string) {
	if p == nil {
		return
	}
	select {
	case p <- Progress{Step: step, Status: status, Message: msg}:
	default:
	}
}
