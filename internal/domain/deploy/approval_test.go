// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent/schema"
	"github.com/th/ngxcp/internal/pkg/apperr"
)

// TestRequiresApproval 覆盖规则引擎：自动续期豁免 / 显式声明 / 开关关闭。
func TestRequiresApproval(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	// 证书自动续期不论命中什么规则都免审批。
	co, err := svc.CreateDraft(ctx, CreateInput{Title: "rn", Type: "cert_renew", Source: "auto_renew", TargetNodes: []int{1, 2}})
	require.NoError(t, err)
	need, _ := svc.cfg().RequiresApproval(co)
	assert.False(t, need, "auto_renew 不应触发审批")

	// 变更单显式声明 ApprovalRequired=true。
	co2, err := svc.CreateDraft(ctx, CreateInput{
		Title: "exp", Type: "config", Strategy: schema.DeployStrategy{ApprovalRequired: true},
	})
	require.NoError(t, err)
	need, rule := svc.cfg().RequiresApproval(co2)
	assert.True(t, need)
	assert.Equal(t, "strategy.approval_required", rule)

	// 关闭审批总开关后，任何变更单都免审批。
	svc.SetApprovalConfig(&ApprovalConfig{Enabled: false})
	need, _ = svc.cfg().RequiresApproval(co2)
	assert.False(t, need, "审批关闭后不应要求审批")
}

// TestSubmit_ApprovalRequired 命中规则 → pending_approval + 落审批记录。
func TestSubmit_ApprovalRequired(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "need", Type: "lvs", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusPendingApproval), string(got.Status))

	a, err := svc.GetApproval(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, "pending", string(a.Status))
	assert.Equal(t, "LVS 配置变更", a.RequiredBy)
}

// TestSubmit_NoApproval 未命中规则 → 直达 pending，且不创建审批记录。
func TestSubmit_NoApproval(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "plain", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	got, err := svc.Get(ctx, co.ID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusPending), string(got.Status), "未命中规则应直达 pending")

	_, err = svc.GetApproval(ctx, co.ID)
	assert.Error(t, err, "免审批路径不应有审批记录")
}

// TestApprove_SelfApprovalBlocked 不允许审批人审批自己提交的变更单。
func TestApprove_SelfApprovalBlocked(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "self", Type: "lvs", TargetNodes: []int{1}, CreatedBy: "alice"})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	// alice 审批自己 → 应被拒
	err = svc.Approve(ctx, co.ID, "alice")
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInvalid, apperr.CodeOf(err))
	// bob 审批 → 通过
	require.NoError(t, svc.Approve(ctx, co.ID, "bob"))
	got, _ := svc.Get(ctx, co.ID)
	assert.Equal(t, string(StatusPending), string(got.Status))
}

// TestApprove_SelfApprovalAllowedWhenConfigured 开启 allow_self_approval 后允许自审批。
func TestApprove_SelfApprovalAllowedWhenConfigured(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	cfg := DefaultApprovalConfig()
	cfg.AllowSelfApproval = true // 仅放开自审批，保留默认规则使 lvs 仍走审批
	svc.SetApprovalConfig(cfg)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "self2", Type: "lvs", TargetNodes: []int{1}, CreatedBy: "alice"})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	require.NoError(t, svc.Approve(ctx, co.ID, "alice"))
}

// TestReject 拒绝 → 审批记录 rejected + 变更单 rejected。
func TestReject(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "rj", Type: "lvs", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	require.NoError(t, svc.Reject(ctx, co.ID))

	got, _ := svc.Get(ctx, co.ID)
	assert.Equal(t, string(StatusRejected), string(got.Status))
	a, _ := svc.GetApproval(ctx, co.ID)
	assert.Equal(t, "rejected", string(a.Status))
}

// TestExpireApprovals 超时未决 → 标记 expired 且变更单自动 rejected。
func TestExpireApprovals(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	// 超时设为已过去 1 小时，使审批记录立即过期；显式声明需审批以确保生成审批记录。
	svc.SetApprovalConfig(&ApprovalConfig{Enabled: true, AllowSelfApproval: false, Timeout: -time.Hour})

	co, err := svc.CreateDraft(ctx, CreateInput{
		Title: "exp", Type: "lvs", TargetNodes: []int{1}, Strategy: schema.DeployStrategy{ApprovalRequired: true},
	})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	n, err := svc.ExpireApprovals(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	a, _ := svc.GetApproval(ctx, co.ID)
	assert.Equal(t, "expired", string(a.Status))
	got, _ := svc.Get(ctx, co.ID)
	assert.Equal(t, string(StatusRejected), string(got.Status), "超时审批应自动拒绝变更单")
}

// TestListApprovals 列表按状态过滤。
func TestListApprovals(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)

	co1, err := svc.CreateDraft(ctx, CreateInput{Title: "a", Type: "lvs", TargetNodes: []int{1}})
	require.NoError(t, err)
	co2, err := svc.CreateDraft(ctx, CreateInput{Title: "b", Type: "lvs", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co1.ID))
	require.NoError(t, svc.Submit(ctx, co2.ID))

	all, err := svc.ListApprovals(ctx, "")
	require.NoError(t, err)
	assert.Len(t, all, 2)

	pend, err := svc.ListApprovals(ctx, "pending")
	require.NoError(t, err)
	assert.Len(t, pend, 2)
}
