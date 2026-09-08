// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// T060 下发编排：把片段写成配置版本，再交给 M3 变更单流水线执行。
//
// 关键取舍：**不自建下发通道**。片段与任何配置变更走完全相同的路径
// （版本化 → 变更单 → nginx -t → 快照 → 原子落盘 → reload → 探活 → 可回滚），
// 因此日志格式下发天然具备灰度与回滚能力，也不会出现"平台以为发了、实际没发"的
// 第二套状态。这与 T068 封禁片段复用流水线是同一条纪律。
package logfmt

import (
	"context"
	"fmt"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/schema"
	configstore "github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// NodeInspector 提供节点能力基线（由 node.Service 实现）。
type NodeInspector interface {
	GetCapability(ctx context.Context, id int) (*node.CapabilityView, error)
}

// ConfigStore 是本包需要的配置版本化能力（由 config.ConfigStore 实现）。
// 以窄接口声明便于无 DB 单测，也明确本包只做「读配置 + 新增版本」，不改现有内容。
type ConfigStore interface {
	ListFiles(ctx context.Context, nodeID int) ([]*configstore.FileView, error)
	GetCurrentContent(ctx context.Context, fileID int) ([]byte, error)
	EnsureFile(ctx context.Context, nodeID int, path string) (int, error)
	CreateRevision(ctx context.Context, fileID int, content []byte, opts configstore.RevisionOpts) (*configstore.RevisionView, error)
}

// OrderCreator 创建变更单（由 deploy.Service 实现）。
type OrderCreator interface {
	CreateDraft(ctx context.Context, in deploy.CreateInput) (*ent.ChangeOrder, error)
}

// Service 编排标准日志格式的预览与下发。
type Service struct {
	nodes  NodeInspector
	cfg    ConfigStore
	orders OrderCreator
}

// New 构造服务。
func New(nodes NodeInspector, cfg ConfigStore, orders OrderCreator) *Service {
	return &Service{nodes: nodes, cfg: cfg, orders: orders}
}

// ApplyInput 是下发请求。
type ApplyInput struct {
	NodeIDs   []int
	Options   SnippetOptions
	Strategy  schema.DeployStrategy
	CreatedBy string
	Comment   string
}

// NodeApply 是单节点的下发结果。
type NodeApply struct {
	NodeID      int      `json:"node_id"`
	SnippetPath string   `json:"snippet_path"`
	LogPath     string   `json:"log_path"`
	RevisionID  int      `json:"revision_id"`
	SHA256      string   `json:"sha256"`
	Warnings    []string `json:"warnings,omitempty"`
}

// ApplyResult 是整体下发结果。
type ApplyResult struct {
	ChangeOrderID int          `json:"change_order_id"`
	Status        string       `json:"status"`
	FormatName    string       `json:"format_name"`
	Fields        []string     `json:"fields"`
	Nodes         []*NodeApply `json:"nodes"`
}

// Apply 为一批节点生成片段版本并创建一张 draft 变更单。
//
// 先对**全部**节点做可行性检查，任一节点不通过即整体失败：宁可什么都不发，也不要
// 让集群内两台 RS 一台有 JSON 日志一台没有——那会让后续跨节点追踪的结果似真而假。
func (s *Service) Apply(ctx context.Context, in ApplyInput) (*ApplyResult, error) {
	if len(in.NodeIDs) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "node_ids 不能为空")
	}
	plans := make([]*NodePlan, 0, len(in.NodeIDs))
	for _, id := range dedupInts(in.NodeIDs) {
		p, err := s.Plan(ctx, id, in.Options)
		if err != nil {
			return nil, err // 已带 CodePrecondition/CodeInvalid 与节点号
		}
		plans = append(plans, p)
	}

	author := in.CreatedBy
	if author == "" {
		author = "web"
	}
	nodesOut := make([]*NodeApply, 0, len(plans))
	revIDs := make([]int, 0, len(plans))
	targets := make([]int, 0, len(plans))
	for _, p := range plans {
		fileID, err := s.cfg.EnsureFile(ctx, p.NodeID, p.SnippetPath)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "登记配置文件失败", err)
		}
		rev, err := s.cfg.CreateRevision(ctx, fileID, []byte(p.Content), configstore.RevisionOpts{
			Source:  configstore.SourceLogFormat,
			Author:  author,
			Message: fmt.Sprintf("T060 下发标准 JSON 日志格式 %s → %s", p.FormatName, p.LogPath),
		})
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "写入配置版本失败", err)
		}
		revIDs = append(revIDs, rev.ID)
		targets = append(targets, p.NodeID)
		nodesOut = append(nodesOut, &NodeApply{
			NodeID:      p.NodeID,
			SnippetPath: p.SnippetPath,
			LogPath:     p.LogPath,
			RevisionID:  rev.ID,
			SHA256:      rev.SHA256,
			Warnings:    p.Warnings,
		})
	}

	co, err := s.orders.CreateDraft(ctx, deploy.CreateInput{
		Title:             fmt.Sprintf("下发标准 JSON 日志格式（%d 个节点）", len(targets)),
		Type:              "config",
		Source:            "api",
		TargetNodes:       targets,
		ConfigRevisionIDs: revIDs,
		Strategy:          defaultStrategy(in.Strategy),
		CreatedBy:         author,
		Comment:           in.Comment,
	})
	if err != nil {
		// 版本已落库但没有变更单：只是多了几条未下发的历史版本，不影响线上，
		// 用户重试会因内容相同而复用同一 blob，不会污染版本链。
		return nil, err
	}
	return &ApplyResult{
		ChangeOrderID: co.ID,
		Status:        string(co.Status),
		FormatName:    plans[0].FormatName,
		Fields:        FieldKeys(),
		Nodes:         nodesOut,
	}, nil
}

// defaultStrategy 未指定策略时用「串行 + 观测 60s + 自动回滚」。
//
// 为什么不默认 lvs_graceful：日志格式片段只新增一条 access_log，reload 不影响正在处理
// 的连接（nginx reload 本身是优雅的），无需摘除权重；但仍保持串行 + 观测，
// 以便第一台出问题时第二台还没动。
func defaultStrategy(in schema.DeployStrategy) schema.DeployStrategy {
	if in.Mode != "" {
		return in
	}
	return schema.DeployStrategy{
		Mode:          "serial",
		ObserveWindow: 60,
		AutoRollback:  true,
	}
}

// dedupInts 去重并保持首次出现顺序（同一节点重复提交只下发一次）。
func dedupInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if v <= 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
