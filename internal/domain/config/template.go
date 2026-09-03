// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package config

import (
	"context"
	"fmt"
	"time"

	"github.com/th/ngxcp/ent"
	"github.com/th/ngxcp/ent/configvariable"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// VariableScope 三级变量的作用域，优先级从低到高：global < cluster < node。
type VariableScope string

const (
	ScopeGlobal  VariableScope = "global"  // 全平台（target_id 恒为 0）
	ScopeCluster VariableScope = "cluster" // 某集群（target_id = cluster_id）
	ScopeNode    VariableScope = "node"    // 单节点（target_id = node_id）
)

// Variable 是三级变量的一行领域表示。
type Variable struct {
	Scope    VariableScope
	TargetID int // cluster_id 或 node_id；global 时为 0
	Key      string
	Value    string
	Secret   bool // 敏感值，API 返回时应打码
}

// ConfigTemplate 是配置模板的领域表示（Go template 语法）。
type ConfigTemplate struct {
	ID        int
	Name      string
	Content   string
	AppliesTo string
	Variables []string // 模板引用的变量清单（自动提取）
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MaskedValue 返回变量对外展示的值：secret 打码为 ******，否则原值。
// 注意：渲染进配置时仍用真实 Value（见 RenderForNode），打码只发生在 API 响应层。
func MaskedValue(v Variable) string {
	if v.Secret {
		return "******"
	}
	return v.Value
}

// TemplateService 模板与三级变量的领域服务，底层用 ent 持久化。
type TemplateService struct {
	client *ent.Client
}

// NewTemplateService 构造模板服务。
func NewTemplateService(client *ent.Client) *TemplateService {
	return &TemplateService{client: client}
}

// CreateTemplate 创建模板。
func (s *TemplateService) CreateTemplate(ctx context.Context, name, content, appliesTo string, variables []string) (*ConfigTemplate, error) {
	create := s.client.ConfigTemplate.Create().
		SetName(name).
		SetContent(content).
		SetAppliesTo(appliesTo)
	if len(variables) > 0 {
		create = create.SetVariables(variables)
	}
	t, err := create.Save(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "创建配置模板失败", err)
	}
	return toDomainTemplate(t), nil
}

// GetTemplate 按 ID 取模板。
func (s *TemplateService) GetTemplate(ctx context.Context, id int) (*ConfigTemplate, error) {
	t, err := s.client.ConfigTemplate.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "模板不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "读取模板失败", err)
	}
	return toDomainTemplate(t), nil
}

// ListTemplates 列出全部模板。
func (s *TemplateService) ListTemplates(ctx context.Context) ([]*ConfigTemplate, error) {
	all, err := s.client.ConfigTemplate.Query().All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "列出模板失败", err)
	}
	out := make([]*ConfigTemplate, 0, len(all))
	for _, t := range all {
		out = append(out, toDomainTemplate(t))
	}
	return out, nil
}

// SetVariable 写入或更新一个三级变量（按 scope+target_id+key 唯一 upsert）。
func (s *TemplateService) SetVariable(ctx context.Context, scope VariableScope, targetID int, key, value string, secret bool) error {
	err := s.client.ConfigVariable.Create().
		SetScope(configvariable.Scope(scope)).
		SetTargetID(targetID).
		SetKey(key).
		SetValue(value).
		SetSecret(secret).
		OnConflictColumns("scope", "target_id", "key").
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "写入变量失败", err)
	}
	return nil
}

// ListVariables 按作用域过滤列出变量（可选过滤）。
func (s *TemplateService) ListVariables(ctx context.Context, scope *VariableScope, targetID *int) ([]Variable, error) {
	q := s.client.ConfigVariable.Query()
	if scope != nil {
		q = q.Where(configvariable.ScopeEQ(configvariable.Scope(*scope)))
	}
	if targetID != nil {
		q = q.Where(configvariable.TargetID(*targetID))
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "列出变量失败", err)
	}
	out := make([]Variable, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDomainVariable(r))
	}
	return out, nil
}

// ResolveVariables 解析某节点的全部生效变量：global < cluster < node 三级合并。
// secret 变量返回真实值（渲染需要），打码由 API 层负责。
func (s *TemplateService) ResolveVariables(ctx context.Context, nodeID int) (map[string]string, error) {
	node, err := s.client.Node.Get(ctx, nodeID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "节点不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "读取节点失败", err)
	}
	clusterID := 0
	if cl, cerr := node.QueryCluster().Only(ctx); cerr == nil {
		clusterID = cl.ID
	} else if !ent.IsNotFound(cerr) {
		return nil, apperr.Wrap(apperr.CodeInternal, "读取节点集群失败", cerr)
	}

	merged := make(map[string]string)
	for _, layer := range []struct {
		scope    VariableScope
		targetID int
	}{{ScopeGlobal, 0}, {ScopeCluster, clusterID}, {ScopeNode, nodeID}} {
		layerVars, lerr := s.queryVars(ctx, layer.scope, layer.targetID)
		if lerr != nil {
			return nil, lerr
		}
		for k, v := range layerVars {
			merged[k] = v // 后写入（更高优先级）覆盖低优先级
		}
	}
	return merged, nil
}

// queryVars 取出某作用域某目标下的全部变量，返回 key→value（真实值）。
func (s *TemplateService) queryVars(ctx context.Context, scope VariableScope, targetID int) (map[string]string, error) {
	rows, err := s.client.ConfigVariable.
		Query().
		Where(
			configvariable.ScopeEQ(configvariable.Scope(scope)),
			configvariable.TargetID(targetID),
		).
		All(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "查询变量失败", err)
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// RenderForNode 解析节点变量后渲染模板，返回该节点的配置文本。
func (s *TemplateService) RenderForNode(ctx context.Context, tmpl *ConfigTemplate, nodeID int) (string, error) {
	vars, err := s.ResolveVariables(ctx, nodeID)
	if err != nil {
		return "", err
	}
	rendered, err := Render(tmpl.Content, vars)
	if err != nil {
		return "", fmt.Errorf("渲染模板 %q 节点 %d: %w", tmpl.Name, nodeID, err)
	}
	return rendered, nil
}

// RenderForNodes 批量渲染到多个节点，返回 nodeID→配置文本。
func (s *TemplateService) RenderForNodes(ctx context.Context, tmpl *ConfigTemplate, nodeIDs []int) (map[int]string, error) {
	out := make(map[int]string, len(nodeIDs))
	for _, id := range nodeIDs {
		rendered, err := s.RenderForNode(ctx, tmpl, id)
		if err != nil {
			return nil, err
		}
		out[id] = rendered
	}
	return out, nil
}

func toDomainTemplate(t *ent.ConfigTemplate) *ConfigTemplate {
	vars := t.Variables
	if vars == nil {
		vars = []string{}
	}
	return &ConfigTemplate{
		ID:        t.ID,
		Name:      t.Name,
		Content:   t.Content,
		AppliesTo: t.AppliesTo,
		Variables: vars,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

func toDomainVariable(v *ent.ConfigVariable) Variable {
	return Variable{
		Scope:    VariableScope(v.Scope),
		TargetID: v.TargetID,
		Key:      v.Key,
		Value:    v.Value,
		Secret:   v.Secret,
	}
}
