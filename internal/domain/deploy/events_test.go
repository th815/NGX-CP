// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package deploy

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSink 收集发出的事件，供断言。
type captureSink struct {
	mu     sync.Mutex
	events []DeployEvent
}

func (c *captureSink) Emit(evt DeployEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
}

func (c *captureSink) all() []DeployEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]DeployEvent, len(c.events))
	copy(out, c.events)
	return out
}

// TestEmit_SubmitEmitsEvent 提交免审批单应发出 pending 事件。
func TestEmit_SubmitEmitsEvent(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	sink := &captureSink{}
	svc.SetEventSink(sink)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "evt", Type: "config", TargetNodes: []int{1}})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))

	evs := sink.all()
	require.Len(t, evs, 1)
	assert.Equal(t, co.ID, evs[0].OrderID)
	assert.Equal(t, string(StatusPending), evs[0].Status)
	assert.NotZero(t, evs[0].Timestamp)
}

// TestEmit_ApproveEmitsEvent 提交+批准应分别发出 pending_approval 与 approved 事件。
func TestEmit_ApproveEmitsEvent(t *testing.T) {
	ctx := context.Background()
	client, _ := newDeployClient(t, true)
	svc := New(client)
	sink := &captureSink{}
	svc.SetEventSink(sink)

	co, err := svc.CreateDraft(ctx, CreateInput{Title: "evt2", Type: "lvs", TargetNodes: []int{1}, CreatedBy: "alice"})
	require.NoError(t, err)
	require.NoError(t, svc.Submit(ctx, co.ID))
	require.NoError(t, svc.Approve(ctx, co.ID, "bob"))

	evs := sink.all()
	require.Len(t, evs, 2) // submit + approve
	assert.Equal(t, string(StatusPendingApproval), evs[0].Status)
	assert.Equal(t, string(StatusPending), evs[1].Status)
	assert.Contains(t, evs[1].Message, "bob")
}
