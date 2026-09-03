// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package deploy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/th/ngxcp/internal/pkg/apperr"
)

// fakeRBClient 可控的回滚触发客户端：failNodes 中的节点回滚失败，calls 记录调用顺序。
type fakeRBClient struct {
	mu        sync.Mutex
	failNodes map[int]bool
	calls     []int
}

func (f *fakeRBClient) RollbackNodeConfig(_ context.Context, nodeID int, _ string) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, nodeID)
	f.mu.Unlock()
	if f.failNodes[nodeID] {
		return false, errors.New("agent rollback boom")
	}
	return true, nil
}

// fakeAlert 记录 CRITICAL 告警。
type fakeAlert struct {
	mu        sync.Mutex
	criticals []string
}

func (f *fakeAlert) Critical(_ context.Context, msg string, _ map[string]any) error {
	f.mu.Lock()
	f.criticals = append(f.criticals, msg)
	f.mu.Unlock()
	return nil
}

// seedRunningOrder 创建一条 running 态的变更单（draft→submit→approve→start）。
func seedRunningOrder(t *testing.T, svc *Service, nodes []int) int {
	t.Helper()
	ctx := context.Background()
	co, err := svc.CreateDraft(ctx, CreateInput{
		Title: "rollback-test", Type: "config", TargetNodes: nodes, CreatedBy: "tester",
	})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	require.NoError(t, svc.Approve(ctx, co.ID, "admin"))
	require.NoError(t, svc.Start(ctx, co.ID))
	return co.ID
}

func TestRollbackChangeOrder_AllSuccess_RolledBack(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	rb := &fakeRBClient{}
	alert := &fakeAlert{}
	svc.SetRollbackClient(rb)
	svc.SetAlertSink(alert)

	orderID := seedRunningOrder(t, svc, []int{1, 2})
	snaps := map[int]string{1: "snap-1", 2: "snap-2"}

	require.NoError(t, svc.RollbackChangeOrder(ctx, orderID, snaps))

	got, err := svc.Get(ctx, orderID)
	require.NoError(t, err)
	assert.Equal(t, string(StatusRolledBack), string(got.Status))
	assert.Empty(t, alert.criticals, "全成功不应触发 CRITICAL")
}

func TestRollbackChangeOrder_NodeFails_RollbackFailedAndCritical(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	rb := &fakeRBClient{failNodes: map[int]bool{2: true}}
	alert := &fakeAlert{}
	svc.SetRollbackClient(rb)
	svc.SetAlertSink(alert)

	orderID := seedRunningOrder(t, svc, []int{1, 2})
	snaps := map[int]string{1: "snap-1", 2: "snap-2"}

	err := svc.RollbackChangeOrder(ctx, orderID, snaps)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rollback_failed")

	got, gerr := svc.Get(ctx, orderID)
	require.NoError(t, gerr)
	assert.Equal(t, string(StatusRollbackFailed), string(got.Status), "单节点失败应置 rollback_failed")
	assert.NotEmpty(t, alert.criticals, "rollback_failed 必须触发 CRITICAL 告警")
}

func TestRollbackChangeOrder_ReverseOrder(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	rb := &fakeRBClient{}
	svc.SetRollbackClient(rb)

	orderID := seedRunningOrder(t, svc, []int{1, 2, 3})
	snaps := map[int]string{1: "s1", 2: "s2", 3: "s3"}
	require.NoError(t, svc.RollbackChangeOrder(ctx, orderID, snaps))

	rb.mu.Lock()
	defer rb.mu.Unlock()
	assert.Equal(t, []int{3, 2, 1}, rb.calls, "回滚必须逆序：最后变更的先回滚")
}

func TestRollbackChangeOrder_NotRollbackableStatus(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	rb := &fakeRBClient{}
	svc.SetRollbackClient(rb)

	// draft 态不允许回滚
	co, err := svc.CreateDraft(ctx, CreateInput{Title: "d", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)

	err = svc.RollbackChangeOrder(ctx, co.ID, map[int]string{1: "s1"})
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInvalid, apperr.CodeOf(err))
}

func TestRollbackChangeOrder_MissingSnapshotPath(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	rb := &fakeRBClient{}
	alert := &fakeAlert{}
	svc.SetRollbackClient(rb)
	svc.SetAlertSink(alert)

	orderID := seedRunningOrder(t, svc, []int{1, 2})
	// 缺节点 2 的快照
	err := svc.RollbackChangeOrder(ctx, orderID, map[int]string{1: "s1"})
	require.Error(t, err)
	assert.Equal(t, string(StatusRollbackFailed), func() string {
		got, _ := svc.Get(ctx, orderID)
		return string(got.Status)
	}(), "缺快照视为回滚失败")
	assert.NotEmpty(t, alert.criticals)
}

func TestRollbackNode_Single_SuccessAndFail(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	alert := &fakeAlert{}
	svc.SetAlertSink(alert)

	// 成功
	rbOK := &fakeRBClient{}
	svc.SetRollbackClient(rbOK)
	require.NoError(t, svc.RollbackNode(ctx, 1, "snap-x", RollbackSnapshot))
	assert.Empty(t, alert.criticals)

	// 失败 → 告警
	rbFail := &fakeRBClient{failNodes: map[int]bool{1: true}}
	svc.SetRollbackClient(rbFail)
	err := svc.RollbackNode(ctx, 1, "snap-x", RollbackSnapshot)
	require.Error(t, err)
	assert.NotEmpty(t, alert.criticals)
}

func TestRollbackNode_NoClientInjected(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client) // 未注入 rbClient
	err := svc.RollbackNode(ctx, 1, "snap-x", RollbackSnapshot)
	require.Error(t, err)
	assert.Equal(t, apperr.CodeInternal, apperr.CodeOf(err))
}
