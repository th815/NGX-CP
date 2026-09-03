// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package config 实现配置中心的核心领域逻辑。本文件落地 T026：配置漂移检测。
//
// 漂移 = 节点上的实际配置与平台「期望」版本不一致。三类来源：
//  1. 手工改动 —— 有人在节点上直接 vi 改了配置（最常见、最危险）；
//  2. 同步失败 —— 发布后部分节点成功部分失败；
//  3. 外部程序修改 —— 如 certbot 自动改了 ssl 配置。
//
// 设计要点：对比基准是「平台期望版本」（最新一次平台主动产生的版本，回退到首次 sync 基线），
// 而非 current_revision_id（会被 sync 覆盖）。因此手工改动会被持久化检出，不会悄悄被平台采纳。
// 默认绝不自动修复漂移（auto_remediate=false）—— 手工改动可能是紧急修复，自动覆盖会毁掉它。
package config

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/th/ngxcp/ent"
)

// DriftKind 是漂移的细分类型，便于前端分组展示与告警分级。
const (
	DriftModified = "modified" // 节点内容与期望版本内容不同
	DriftAdded    = "added"    // 节点存在平台未管理的文件（外部新增）
	DriftDeleted  = "deleted"  // 平台管理的文件在节点上被删除
)

// ReportedConfigFile 是节点当前实际配置的领域表示（与 agentv1.ConfigFile 解耦，避免域层依赖 gRPC）。
// SHA 由 Agent 侧 ParseConfigTree 算出（与 ConfigStore.sha256Hex 同算法），用于快速比对。
type ReportedConfigFile struct {
	Path    string
	SHA     string
	Content string
}

// DriftItem 是一条漂移记录。
type DriftItem struct {
	Path        string      `json:"path"`                  // 配置在节点上的路径
	Kind        string      `json:"kind"`                  // modified | added | deleted
	ExpectedSHA string      `json:"expected_sha"`          // 平台期望版本的 blob SHA
	ActualSHA   string      `json:"actual_sha"`            // 节点实际内容的 blob SHA
	Diff        *DiffResult `json:"diff,omitempty"`        // 仅 modified 有意义
	DetectedAt  time.Time   `json:"detected_at"`           // 本次检出时间
	Severity    string      `json:"severity"`              // critical | warning
}

// DriftReport 是某节点一次漂移检测的完整结果。
type DriftReport struct {
	NodeID    int         `json:"node_id"`
	CheckedAt time.Time   `json:"checked_at"`
	Items     []DriftItem `json:"items"`
}

// HasDrift 是否存在任意漂移项。
func (r *DriftReport) HasDrift() bool { return r != nil && len(r.Items) > 0 }

// CriticalCount 返回 critical 级漂移数量。
func (r *DriftReport) CriticalCount() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, it := range r.Items {
		if it.Severity == "critical" {
			n++
		}
	}
	return n
}

// SeverityRule 是「路径模式 → 严重级别」的映射（可配置）。
type SeverityRule struct {
	PathPattern string `json:"path_pattern" yaml:"path_pattern"`
	Severity    string `json:"severity" yaml:"severity"` // critical | warning
}

// DriftConfig 是漂移检测的可配置项。
type DriftConfig struct {
	CheckInterval time.Duration
	AutoAlert     bool
	AutoRemediate bool // ★ 默认 false：绝不自动修复，只告警让人决定
	SeverityRules []SeverityRule
}

// DefaultDriftConfig 返回内建默认配置：5 分钟巡检、告警开启、不自动修复、
// conf.d/*.conf 与 nginx.conf 为 critical。
func DefaultDriftConfig() DriftConfig {
	return DriftConfig{
		CheckInterval: 5 * time.Minute,
		AutoAlert:     true,
		AutoRemediate: false,
		SeverityRules: []SeverityRule{
			{PathPattern: "nginx.conf", Severity: "critical"},
			{PathPattern: "conf.d/*.conf", Severity: "critical"},
		},
	}
}

// DriftChecker 是漂移检测器的对外抽象（便于 HTTP 层与测试注入 fake）。
type DriftChecker interface {
	Detect(ctx context.Context, nodeID int, actual []ReportedConfigFile) (*DriftReport, error)
	RecordActual(ctx context.Context, nodeID int, actual []ReportedConfigFile) (*DriftReport, error)
	GetReport(nodeID int) (*DriftReport, bool)
	Reports() []*DriftReport
	RunWorker(ctx context.Context, interval time.Duration) error
}

// DriftDetector 是配置漂移检测器：纯函数式 Detect + 内存态报告缓存 + 定时巡检 worker。
type DriftDetector struct {
	client *ent.Client
	store  *ConfigStore
	cfg    DriftConfig

	mu      sync.RWMutex
	reports map[int]*DriftReport
	actuals map[int][]ReportedConfigFile
}

// NewDriftDetector 构造检测器。cfg 为 zero 值时回落到 DefaultDriftConfig（仅补缺字段）。
func NewDriftDetector(client *ent.Client, store *ConfigStore, cfg DriftConfig) *DriftDetector {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = DefaultDriftConfig().CheckInterval
	}
	if cfg.SeverityRules == nil {
		cfg.SeverityRules = DefaultDriftConfig().SeverityRules
	}
	return &DriftDetector{
		client:  client,
		store:   store,
		cfg:     cfg,
		reports: make(map[int]*DriftReport),
		actuals: make(map[int][]ReportedConfigFile),
	}
}

// Detect 对比节点实际配置（actual）与平台期望版本，产出漂移报告。
// actual 由调用方提供（通常来自一次 Agent 配置树上报或手动提交）。
func (d *DriftDetector) Detect(ctx context.Context, nodeID int, actual []ReportedConfigFile) (*DriftReport, error) {
	report := &DriftReport{NodeID: nodeID, CheckedAt: time.Now()}

	expectedFiles, err := d.store.ListFiles(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("列举期望配置失败: %w", err)
	}
	expectedByPath := make(map[string]*FileView, len(expectedFiles))
	for _, f := range expectedFiles {
		expectedByPath[f.Path] = f
	}
	actualByPath := make(map[string]ReportedConfigFile, len(actual))
	for _, a := range actual {
		actualByPath[a.Path] = a
	}

	// 1) 节点存在、平台也管理的文件：比对 SHA 与内容。
	for _, a := range actual {
		if _, ok := expectedByPath[a.Path]; !ok {
			report.Items = append(report.Items, d.makeItem(a.Path, "", a.SHA, DriftAdded, nil))
			continue
		}
		expRev, eerr := d.store.ExpectedRevision(ctx, nodeID, a.Path)
		if eerr != nil {
			return nil, eerr
		}
		if expRev == nil {
			continue // 无基线，跳过（不应发生，但防御性）
		}
		expSHA, eerr := blobSHA(ctx, expRev)
		if eerr != nil {
			return nil, eerr
		}
		if expSHA == a.SHA {
			continue // 一致，无漂移
		}
		var diff *DiffResult
		if expContent, cerr := blobContent(ctx, expRev); cerr == nil {
			diff = DiffNginx(expContent, a.Content)
		}
		report.Items = append(report.Items, d.makeItem(a.Path, expSHA, a.SHA, DriftModified, diff))
	}

	// 2) 平台管理但节点缺失的文件：外部删除。
	for _, ev := range expectedFiles {
		if _, ok := actualByPath[ev.Path]; ok {
			continue
		}
		expRev, eerr := d.store.ExpectedRevision(ctx, nodeID, ev.Path)
		if eerr != nil {
			return nil, eerr
		}
		if expRev == nil {
			continue
		}
		expSHA, _ := blobSHA(ctx, expRev)
		report.Items = append(report.Items, d.makeItem(ev.Path, expSHA, "", DriftDeleted, nil))
	}

	d.alertIfNeeded(report)
	return report, nil
}

// RecordActual 提交节点实际配置并立即检测，结果写入内存缓存（供 API 与 worker 复用）。
// 在 node.Service.SaveConfigTree 同步配置树后调用，实现「上报即检测」（契合 T026 陷阱：
// 不每次都跑 nginx -T，仅在心跳/上报时做 SHA 级比对）。
func (d *DriftDetector) RecordActual(ctx context.Context, nodeID int, actual []ReportedConfigFile) (*DriftReport, error) {
	report, err := d.Detect(ctx, nodeID, actual)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.reports[nodeID] = report
	d.actuals[nodeID] = actual
	d.mu.Unlock()
	return report, nil
}

// GetReport 返回某节点最近一次缓存的漂移报告。
func (d *DriftDetector) GetReport(nodeID int) (*DriftReport, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	r, ok := d.reports[nodeID]
	return r, ok
}

// Reports 返回所有已缓存节点的漂移报告（快照切片）。
func (d *DriftDetector) Reports() []*DriftReport {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*DriftReport, 0, len(d.reports))
	for _, r := range d.reports {
		out = append(out, r)
	}
	return out
}

// makeItem 构造一条漂移记录并判定严重级别（按路径规则匹配，未命中默认 warning）。
func (d *DriftDetector) makeItem(p, expSHA, actSHA, kind string, diff *DiffResult) DriftItem {
	return DriftItem{
		Path:        p,
		Kind:        kind,
		ExpectedSHA: expSHA,
		ActualSHA:   actSHA,
		Diff:        diff,
		DetectedAt:  time.Now(),
		Severity:    severityForPath(d.cfg.SeverityRules, p),
	}
}

// severityForPath 按路径模式匹配严重级别；未命中任何规则时默认 warning。
func severityForPath(rules []SeverityRule, p string) string {
	for _, r := range rules {
		if matchSeverityPattern(r.PathPattern, p) {
			return r.Severity
		}
	}
	return "warning"
}

// matchSeverityPattern 支持相对模式匹配绝对路径：先用完整路径试匹配，
// 失败则按 "/" 分段，与路径末尾等长片段比对，使 "conf.d/*.conf" 能命中
// "/etc/nginx/conf.d/api.conf"（取末尾两段 "conf.d/api.conf" 匹配）。
func matchSeverityPattern(pattern, p string) bool {
	if ok, _ := path.Match(pattern, p); ok {
		return true
	}
	segs := strings.Split(pattern, "/")
	if len(segs) <= 1 {
		// 无分隔符的模式（如 "nginx.conf"）仅比对文件名。
		return matchOk(segs[0], path.Base(p))
	}
	pSegs := strings.Split(p, "/")
	if len(pSegs) < len(segs) {
		return false
	}
	tail := pSegs[len(pSegs)-len(segs):]
	return matchOk(pattern, strings.Join(tail, "/"))
}

func matchOk(pattern, s string) bool {
	ok, _ := path.Match(pattern, s)
	return ok
}

// alertIfNeeded 在 auto_alert 开启且检出漂移时打 WARN 日志（告警中心随 M5 接入）。
// auto_remediate 始终不在此处执行任何修复动作。
func (d *DriftDetector) alertIfNeeded(report *DriftReport) {
	if !d.cfg.AutoAlert || !report.HasDrift() {
		return
	}
	for _, it := range report.Items {
		exp := it.ExpectedSHA
		act := it.ActualSHA
		if len(exp) > 8 {
			exp = exp[:8]
		}
		if len(act) > 8 {
			act = act[:8]
		}
		slog.Default().Warn("config drift detected",
			"node_id", report.NodeID, "path", it.Path, "kind", it.Kind,
			"sEVERITY", it.Severity, "expected", exp, "actual", act)
	}
}

// blobSHA 返回某版本关联 blob 的 SHA256。
func blobSHA(ctx context.Context, rev *ent.ConfigRevision) (string, error) {
	b, err := rev.QueryBlob().Only(ctx)
	if err != nil {
		return "", fmt.Errorf("query revision blob: %w", err)
	}
	return b.Sha256, nil
}

// blobContent 返回某版本关联 blob 的内容。
func blobContent(ctx context.Context, rev *ent.ConfigRevision) (string, error) {
	b, err := rev.QueryBlob().Only(ctx)
	if err != nil {
		return "", fmt.Errorf("query revision blob: %w", err)
	}
	return b.Content, nil
}
