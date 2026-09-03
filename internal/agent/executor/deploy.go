// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package executor 实现 Agent 在受管主机上执行的能力型命令（T024 校验 / T031 快照 / T032 原子落盘）。
//
// DeployExecutor 是发布引擎的核心算法：把"下发配置"拆成 9 步流水线，
// 关键保证是「校验在切换之前」—— 语法错误绝不会触达线上（零污染），
// 且探活失败会自动从步骤④的预变更快照恢复。所有外部命令经 CommandRunner 抽象，
// 单测用 fake 注入，无需真实 nginx 二进制（与 T024 同构）。
package executor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/th/ngxcp/internal/domain/backup"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/pkg/atomicfile"
)

// DeployFile 是待下发的一个配置文件（相对 nginx prefix 的路径）。
type DeployFile struct {
	Path    string // 相对 prefix，如 "conf.d/api.conf"
	Content string // 文件原文
	SHA256  string // 可选：步骤②校验用，空则跳过
}

// DeployRequest 是一次原子落盘请求（纯数据，便于单测注入）。
type DeployRequest struct {
	Files         []DeployFile // 待下发文件集合
	Prefix        string       // nginx prefix，默认 /etc/nginx
	NginxPath     string       // nginx 二进制，默认 /usr/sbin/nginx
	ConfPath      string       // 主配置相对 prefix，默认 nginx.conf
	ChangeOrderID *int
	NodeID        int
	StagingDir    string        // 临时目录（默认系统临时目录子目录）
	ObserveWindow time.Duration // 步骤⑦ 观测窗口，默认 5s
	SnapshotPaths []string      // 步骤④ 预变更快照路径，默认 [Prefix]
	ProbeURL      string        // 步骤⑧ 探活 URL，空则跳过探活
	ProbeTimeout  time.Duration // 探活超时，默认 5s
	ReloadArgs    []string      // reload 命令参数，默认 ["-s","reload"]
}

// Progress 是流水线每一步的进度上报。
type Progress struct {
	Step    string // transfer|verify_hash|validate|snapshot|switch|reload|wait|probe|report|rollback
	Status  string // running|success|failed
	Message string
}

// DeployResult 是 Deploy 的结果摘要。
type DeployResult struct {
	SnapshotPath string // 预变更快照 tar.gz 路径（回滚用）
	Restored     bool   // 是否触发了从快照恢复（探活/切换/reload 失败回滚）
}

// Prober 判断变更后的节点是否健康（T032 内置最小 HTTP 实现；T033 会替换为复合探活）。
type Prober interface {
	Probe(ctx context.Context) (ok bool, detail string, err error)
}

// HTTPProber 是最简单的探活：GET 一个 URL，<500 即认为健康。
type HTTPProber struct {
	URL     string
	Timeout time.Duration
	Client  *http.Client
}

// Probe 执行一次 HTTP 探活。
func (p *HTTPProber) Probe(ctx context.Context) (bool, string, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return false, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error(), nil
	}
	defer resp.Body.Close()
	ok := resp.StatusCode < 500
	return ok, fmt.Sprintf("HTTP %d", resp.StatusCode), nil
}

// DeployExecutor 在节点上执行 9 步原子落盘。
type DeployExecutor struct {
	run       CommandRunner
	snapper   *SnapshotExecutor
	validator *Executor // T024 校验执行器（同包）
	prober    Prober
}

// NewDeployExecutor 用真实命令执行器构造（Agent 运行时使用 hostexec）。
func NewDeployExecutor(run CommandRunner) *DeployExecutor {
	return &DeployExecutor{
		run:       run,
		snapper:   NewSnapshotExecutor(),
		validator: NewExecutor(run),
	}
}

// NewDeployExecutorWithRunner 便于单测注入自定义执行函数。
func NewDeployExecutorWithRunner(run func(ctx context.Context, name string, args ...string) (string, error)) *DeployExecutor {
	cr := runnerFunc(run)
	return &DeployExecutor{run: cr, snapper: NewSnapshotExecutor(), validator: NewExecutor(cr)}
}

// SetProber 注入自定义探活器（默认按 DeployRequest.ProbeURL 构造 HTTPProber）。
func (e *DeployExecutor) SetProber(p Prober) { e.prober = p }

// Deploy 执行 9 步原子落盘流水线。progress 可为 nil（不发送进度）。
// 任何一步失败：已切换的步骤（⑤⑥⑧）会先从预变更快照恢复再返回错误。
func (e *DeployExecutor) Deploy(ctx context.Context, req DeployRequest, progress chan<- Progress) (*DeployResult, error) {
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
	observe := req.ObserveWindow
	if observe <= 0 {
		observe = 5 * time.Second
	}
	prober := e.prober
	if prober == nil && req.ProbeURL != "" {
		prober = &HTTPProber{URL: req.ProbeURL, Timeout: req.ProbeTimeout}
	}

	staging := req.StagingDir
	if staging == "" {
		d, mkErr := os.MkdirTemp("", "ngxcp-deploy-")
		if mkErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "创建 staging 失败", mkErr)
		}
		staging = d
	}
	defer os.RemoveAll(staging)

	files := make(map[string]string, len(req.Files))
	for _, f := range req.Files {
		files[f.Path] = f.Content
	}

	// ① 传输：写入 staging（保持相对结构，绝不触碰 prefix）
	e.emit(progress, "transfer", "running", "")
	if werr := writeStaging(staging, files); werr != nil {
		e.emit(progress, "transfer", "failed", werr.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "写入 staging 失败", werr)
	}
	e.emit(progress, "transfer", "success", "")

	// ② 校验摘要：传输中断/损坏立即中止
	e.emit(progress, "verify_hash", "running", "")
	if herr := verifyHashes(staging, req.Files); herr != nil {
		e.emit(progress, "verify_hash", "failed", herr.Error())
		return nil, apperr.Wrap(apperr.CodePrecondition, "配置摘要校验失败", herr)
	}
	e.emit(progress, "verify_hash", "success", "")

	// ③ 语法校验：nginx -t -p staging -c conf（★ 必须在切换之前）
	e.emit(progress, "validate", "running", "")
	vresp, verr := e.validator.Validate(ctx, ValidateRequest{
		NginxPath: nginxPath, Prefix: staging, ConfPath: confPath, Files: files,
	})
	if verr != nil || vresp == nil || !vresp.OK {
		e.emit(progress, "validate", "failed", firstValidateErr(vresp, verr))
		return nil, apperr.Wrap(apperr.CodePrecondition, "nginx -t 校验失败，线上零变化", verr)
	}
	e.emit(progress, "validate", "success", "")

	// ④ 预变更快照：抓取当前 prefix，作为回滚点
	e.emit(progress, "snapshot", "running", "")
	snapPaths := req.SnapshotPaths
	if len(snapPaths) == 0 {
		snapPaths = []string{prefix}
	}
	co, serr := e.snapper.Create(ctx, SnapshotRequest{
		Paths: snapPaths, StagingDir: staging + "-snap", NodeID: req.NodeID,
		ChangeOrderID: req.ChangeOrderID, Type: "pre_deploy",
	})
	if serr != nil {
		e.emit(progress, "snapshot", "failed", serr.Error())
		return nil, apperr.Wrap(apperr.CodeInternal, "预变更快照失败", serr)
	}
	e.emit(progress, "snapshot", "success", co.Path)

	// ⑤ 原子切换：逐个 rename 到 prefix（同盘原子 / 跨盘降级 copy）
	e.emit(progress, "switch", "running", "")
	if serr := e.atomicSwitch(staging, prefix, req.Files); serr != nil {
		e.emit(progress, "switch", "failed", serr.Error())
		rbErr := e.restoreAndReload(ctx, co, nginxPath, req.ReloadArgs, progress, "切换失败")
		return &DeployResult{SnapshotPath: co.Path, Restored: true},
			apperr.Wrap(apperr.CodeInternal, "原子切换失败并已回滚", rbErr)
	}
	e.emit(progress, "switch", "success", "")

	// ⑥ 平滑加载
	e.emit(progress, "reload", "running", "")
	if rerr := e.reload(ctx, nginxPath, req.ReloadArgs); rerr != nil {
		e.emit(progress, "reload", "failed", rerr.Error())
		rbErr := e.restoreAndReload(ctx, co, nginxPath, req.ReloadArgs, progress, "reload 失败")
		return &DeployResult{SnapshotPath: co.Path, Restored: true},
			apperr.Wrap(apperr.CodeInternal, "reload 失败并已回滚", rbErr)
	}
	e.emit(progress, "reload", "success", "")

	// ⑦ 等待新 worker 稳定
	e.emit(progress, "wait", "running", "")
	select {
	case <-time.After(observe):
	case <-ctx.Done():
		return &DeployResult{SnapshotPath: co.Path}, ctx.Err()
	}
	e.emit(progress, "wait", "success", "")

	// ⑧ 探活：失败则回滚
	if prober != nil {
		e.emit(progress, "probe", "running", "")
		ok, detail, perr := prober.Probe(ctx)
		if perr != nil || !ok {
			msg := detail
			if perr != nil {
				msg = perr.Error()
			}
			e.emit(progress, "probe", "failed", msg)
			rbErr := e.restoreAndReload(ctx, co, nginxPath, req.ReloadArgs, progress, "探活失败")
			return &DeployResult{SnapshotPath: co.Path, Restored: true},
				apperr.Wrap(apperr.CodePrecondition, "探活失败，已回滚到变更前状态", rbErr)
		}
		e.emit(progress, "probe", "success", detail)
	}

	// ⑨ 上报
	e.emit(progress, "report", "success", "")
	return &DeployResult{SnapshotPath: co.Path, Restored: false}, nil
}

// atomicSwitch 把 staging 下的文件逐个移动到 prefix。
func (e *DeployExecutor) atomicSwitch(staging, prefix string, files []DeployFile) error {
	for _, f := range files {
		src := filepath.Join(staging, f.Path)
		dst := filepath.Join(prefix, f.Path)
		if err := atomicfile.MoveFile(src, dst); err != nil {
			return fmt.Errorf("切换文件 %s 失败: %w", f.Path, err)
		}
	}
	return nil
}

// reload 执行 nginx 平滑加载（默认 `nginx -s reload`）。
func (e *DeployExecutor) reload(ctx context.Context, nginxPath string, args []string) error {
	if len(args) == 0 {
		args = []string{"-s", "reload"}
	}
	out, err := e.run.Output(ctx, nginxPath, args...)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "执行 reload 失败: "+out, err)
	}
	return nil
}

// restoreAndReload 从快照恢复配置并重载（回滚动作）。
func (e *DeployExecutor) restoreAndReload(ctx context.Context, co *backup.ConfigSnapshot, nginxPath string, args []string, progress chan<- Progress, reason string) error {
	e.emit(progress, "rollback", "running", reason)
	if err := e.snapper.Restore(ctx, RestoreRequest{TarPath: co.Path, Root: "/"}); err != nil {
		e.emit(progress, "rollback", "failed", err.Error())
		return err
	}
	if err := e.reload(ctx, nginxPath, args); err != nil {
		e.emit(progress, "rollback", "failed", err.Error())
		return err
	}
	e.emit(progress, "rollback", "success", "")
	return nil
}

// verifyHashes 校验 staging 内文件的 SHA256（未提供摘要的文件跳过）。
func verifyHashes(staging string, files []DeployFile) error {
	for _, f := range files {
		if f.SHA256 == "" {
			continue
		}
		sum, err := sha256File(filepath.Join(staging, f.Path))
		if err != nil {
			return fmt.Errorf("计算 %s 摘要失败: %w", f.Path, err)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("文件 %s 摘要不符: 期望 %s 实际 %s", f.Path, f.SHA256, sum)
		}
	}
	return nil
}

// firstValidateErr 取校验失败的首条可读原因。
func firstValidateErr(v *ValidateResponse, err error) string {
	if err != nil {
		return err.Error()
	}
	if v != nil && len(v.Errors) > 0 {
		return v.Errors[0].Message
	}
	return "未知校验错误"
}

// emit 非阻塞发送进度（channel 满或 nil 时丢弃）。
func (e *DeployExecutor) emit(p chan<- Progress, step, status, msg string) {
	if p == nil {
		return
	}
	select {
	case p <- Progress{Step: step, Status: status, Message: msg}:
	default:
	}
}
