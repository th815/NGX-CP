// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Package security 的封禁子系统（T068）：把「封禁/解封一个 IP」转换为一条
// 走 M3 发布流水线的 security_block 变更单。
//
// 设计对齐项目纪律（见 docs/tasks/M6-logs-security.md T068）：
//   - 封禁绝不直接改线上配置，必须走变更单（可校验 / 可灰度 / 可观测 / 可回滚 / 可审批）
//   - 解封同样是一条变更单（把同一路径文件内容替换为「已解封」标记，deny 消失），不能旁路
//   - 实际字节下发依赖 T039 执行器读取 config_revision 内容（与任何配置变更同一条路），
//     T068 只负责生产「正确的变更单 + 配置修订」，不在本任务重造下发路径。
package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/changeorder"
	"github.com/th/ngxcp/ent/configblob"
	"github.com/th/ngxcp/ent/configrevision"
	"github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/ent/schema"
	"github.com/th/ngxcp/internal/domain/deploy"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// blocklistConfDir 是封禁文件在节点上的目录，对齐标准 include conf.d/*.conf;。
// 每个被封 IP 一个独立文件（zz-block-<ip>.conf），便于按 IP 精确解封、互不干扰。
const blocklistConfDir = "conf.d"

// blocklistPath 返回单 IP 封禁文件在节点上的路径。
func blocklistPath(ip string) string {
	return fmt.Sprintf("%s/zz-block-%s.conf", blocklistConfDir, normalizeIP(ip))
}

// normalizeIP 把 IP 转为文件名安全字符串（. 与 : 替换为 _）。
func normalizeIP(ip string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(ip)
}

// BlockService 封装「封禁/解封 → 变更单」的全部逻辑。
// 依赖 ent 客户端（落 blob / revision / 变更单）与 deploy.Service（状态机 + 提交）。
type BlockService struct {
	client *ent.Client
	deploy *deploy.Service
	now    func() time.Time
}

// NewBlockService 构造封禁服务。
func NewBlockService(client *ent.Client, d *deploy.Service) *BlockService {
	return &BlockService{client: client, deploy: d, now: time.Now}
}

// nginxRSNodes 返回当前 online 且承担 Nginx 角色的节点（real_server / director_and_rs）。
// 封禁文件必须下到真正跑 Nginx 的节点才会生效。
func (s *BlockService) nginxRSNodes(ctx context.Context) ([]*ent.Node, error) {
	return s.client.Node.Query().
		Where(
			node.StatusEQ(node.StatusOnline),
			node.RoleIn(node.RoleRealServer, node.RoleDirectorAndRs),
		).
		All(ctx)
}

// BlockIP 封禁一个 IP：为每个 Nginx RS 节点生成 security_block 配置修订，
// 建 security_block 变更单（LVS 优雅灰度 + 自动回滚）并提交进入管线。
//
// operator 写入变更单 created_by 与修订 author；reason 记入变更单 comment。
// 返回已进入 draft→pending/pending_approval 状态机的变更单。
func (s *BlockService) BlockIP(ctx context.Context, ip, reason, operator string) (*ent.ChangeOrder, error) {
	if err := validateIP(ip); err != nil {
		return nil, err
	}
	nodes, err := s.nginxRSNodes(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询 Nginx 节点失败", err)
	}
	if len(nodes) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "当前没有可用的 Nginx RS 节点，无法下发封禁")
	}

	content := blockContent(ip)
	blob, err := s.ensureBlob(ctx, content)
	if err != nil {
		return nil, err
	}

	// 先建 draft 变更单（拿到 ID），再回填修订的 change_order_id，保证审计可追溯。
	co, err := s.deploy.CreateDraft(ctx, deploy.CreateInput{
		Title:       fmt.Sprintf("封禁 IP %s", ip),
		Type:        string(changeorder.TypeSecurityBlock),
		Source:      string(changeorder.SourceAPI),
		TargetNodes: nodeIDs(nodes),
		Strategy: schema.DeployStrategy{
			Mode:             "lvs_graceful",
			AutoRollback:     true,
			ApprovalRequired: false,
		},
		CreatedBy: operator,
		Comment:   reason,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建封禁变更单失败", err)
	}

	revIDs, err := s.createRevisions(ctx, nodes, blob, co.ID, ip, operator, reason, false)
	if err != nil {
		return nil, err
	}
	if _, err := s.client.ChangeOrder.UpdateOneID(co.ID).
		SetConfigRevisionIds(revIDs).
		Save(ctx); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "回写封禁修订到变更单失败", err)
	}

	if err := s.deploy.Submit(ctx, co.ID); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "提交封禁变更单失败", err)
	}
	return s.deploy.Get(ctx, co.ID)
}

// UnblockIP 解封一个 IP：为每个 Nginx RS 节点生成「已解封」标记修订（不含 deny 指令），
// 建 security_block 变更单（同一流水线）并提交。下发后原 deny 被覆盖，封禁解除。
// 解封同样是变更单，不能旁路直接删文件。
func (s *BlockService) UnblockIP(ctx context.Context, ip, reason, operator string) (*ent.ChangeOrder, error) {
	if err := validateIP(ip); err != nil {
		return nil, err
	}
	nodes, err := s.nginxRSNodes(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询 Nginx 节点失败", err)
	}
	if len(nodes) == 0 {
		return nil, apperr.New(apperr.CodeInvalid, "当前没有可用的 Nginx RS 节点，无法下发解封")
	}

	content := unblockContent(ip, s.now())
	blob, err := s.ensureBlob(ctx, content)
	if err != nil {
		return nil, err
	}

	co, err := s.deploy.CreateDraft(ctx, deploy.CreateInput{
		Title:       fmt.Sprintf("解封 IP %s", ip),
		Type:        string(changeorder.TypeSecurityBlock),
		Source:      string(changeorder.SourceAPI),
		TargetNodes: nodeIDs(nodes),
		Strategy: schema.DeployStrategy{
			Mode:             "lvs_graceful",
			AutoRollback:     true,
			ApprovalRequired: false,
		},
		CreatedBy: operator,
		Comment:   "unblock: " + reason,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建解封变更单失败", err)
	}

	revIDs, err := s.createRevisions(ctx, nodes, blob, co.ID, ip, operator, reason, true)
	if err != nil {
		return nil, err
	}
	if _, err := s.client.ChangeOrder.UpdateOneID(co.ID).
		SetConfigRevisionIds(revIDs).
		Save(ctx); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "回写解封修订到变更单失败", err)
	}

	if err := s.deploy.Submit(ctx, co.ID); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "提交解封变更单失败", err)
	}
	return s.deploy.Get(ctx, co.ID)
}

// BlockEvent 基于一条安全事件封禁其样本中的来源 IP（一键封禁）。
// 攻击者 IP 在样本里（不在节点字段里），故从 sample 提取第一个合法 IP。
// 若样本中无法提取合法 IP，返回 CodeInvalid。
func (s *BlockService) BlockEvent(ctx context.Context, evt *Event, operator string) (*ent.ChangeOrder, error) {
	ip, ok := ExtractIP(evt.Sample)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalid, "安全事件样本中未找到可封禁的 IP 地址")
	}
	reason := fmt.Sprintf("来自安全事件 #%d（%s）的自动封禁", evt.ID, evt.RuleName)
	return s.BlockIP(ctx, ip, reason, operator)
}

// ExtractIP 从任意文本（日志样本 / 证据）中提取第一个合法 IPv4/IPv6 地址。
// 用于「从安全事件一键封禁」：去掉首尾常见标点后逐个 token 试解析。
func ExtractIP(sample string) (string, bool) {
	for _, f := range strings.Fields(sample) {
		f = strings.Trim(f, ".,;:)\"'")
		if net.ParseIP(f) != nil {
			return f, true
		}
	}
	return "", false
}

// ensureBlob 按内容 SHA256 去重写入 config_blob，返回 blob 实体（内容寻址，相同内容只存一份）。
func (s *BlockService) ensureBlob(ctx context.Context, content string) (*ent.ConfigBlob, error) {
	sum := sha256.Sum256([]byte(content))
	sha := hex.EncodeToString(sum[:])
	// 读穿：已存在则直接复用（去重），避免唯一约束冲突。
	if existing, err := s.client.ConfigBlob.Query().Where(configblob.Sha256(sha)).Only(ctx); err == nil {
		return existing, nil
	} else if !ent.IsNotFound(err) {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询配置内容失败", err)
	}
	blob, err := s.client.ConfigBlob.Create().
		SetSha256(sha).
		SetSize(len(content)).
		SetContent(content).
		Save(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "写入配置内容失败", err)
	}
	return blob, nil
}

// createRevisions 为每个目标节点建一条 security_block 配置修订（引用同一 blob）。
// unblock=true 时 message 标注「解封」。返回修订 ID 列表。
func (s *BlockService) createRevisions(ctx context.Context, nodes []*ent.Node, blob *ent.ConfigBlob, orderID int, ip, operator, reason string, unblock bool) ([]int, error) {
	revIDs := make([]int, 0, len(nodes))
	for _, n := range nodes {
		b := s.client.ConfigRevision.Create().
			SetNodeID(n.ID).
			SetPath(blocklistPath(ip)).
			SetSource(configrevision.SourceSecurityBlock).
			SetChangeOrderID(orderID).
			SetAuthor(operator).
			SetBlob(blob)
		if unblock {
			b.SetMessage(fmt.Sprintf("解封 IP %s：移除 deny 指令（%s）", ip, reason))
		} else {
			b.SetMessage(fmt.Sprintf("封禁 IP %s：%s", ip, reason))
		}
		rev, err := b.Save(ctx)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "写入配置修订失败", err)
		}
		revIDs = append(revIDs, rev.ID)
	}
	return revIDs, nil
}

// blockContent 返回封禁片段：deny 指令（放 http 块，对全 server 生效）。
func blockContent(ip string) string {
	return fmt.Sprintf("deny %s;\n", ip)
}

// unblockContent 返回解封片段：不含 deny 指令的标记文件，下发后覆盖原封禁文件。
func unblockContent(ip string, now time.Time) string {
	return fmt.Sprintf("# unblocked %s at %s (deny removed)\n", ip, now.UTC().Format(time.RFC3339))
}

// nodeIDs 把节点切片投影为 ID 切片。
func nodeIDs(nodes []*ent.Node) []int {
	ids := make([]int, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return ids
}

// validateIP 校验字符串是否为合法 IP 地址。
func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return apperr.New(apperr.CodeInvalid, fmt.Sprintf("非法 IP 地址: %q", ip))
	}
	return nil
}
