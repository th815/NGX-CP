// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package deploy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStateMachine_Legal：合法迁移应被认可。
func TestStateMachine_Legal(t *testing.T) {
	legal := [][2]OrderStatus{
		{StatusDraft, StatusPendingApproval},
		{StatusPendingApproval, StatusPending},
		{StatusPendingApproval, StatusRejected},
		{StatusPending, StatusRunning},
		{StatusRunning, StatusSuccess},
		{StatusRunning, StatusFailed},
		{StatusFailed, StatusRollingBack},
		{StatusRollingBack, StatusRolledBack},
		{StatusRollingBack, StatusRollbackFailed},
		{StatusRolledBack, StatusPending}, // 回滚后可重提
	}
	for _, tc := range legal {
		assert.True(t, CanTransition(tc[0], tc[1]),
			"期望 %s → %s 为合法迁移", tc[0], tc[1])
	}
}

// TestStateMachine_Illegal：非法迁移必须被拒绝（含验收要求的 success → running）。
func TestStateMachine_Illegal(t *testing.T) {
	illegal := [][2]OrderStatus{
		{StatusSuccess, StatusRunning}, // ★ 验收明确要求的场景
		{StatusDraft, StatusRunning},
		{StatusRunning, StatusDraft},
		{StatusCanceled, StatusPending},
		{StatusRejected, StatusPending},
	}
	for _, tc := range illegal {
		assert.False(t, CanTransition(tc[0], tc[1]),
			"期望 %s → %s 为非法迁移", tc[0], tc[1])
	}
}

// TestStateMachine_Terminal：终止态无出边。
func TestStateMachine_Terminal(t *testing.T) {
	terminal := []OrderStatus{StatusSuccess, StatusRejected, StatusCanceled}
	for _, s := range terminal {
		assert.True(t, IsTerminal(s), "期望 %s 为终止态", s)
	}
	nonTerminal := []OrderStatus{StatusDraft, StatusRunning, StatusRollingBack}
	for _, s := range nonTerminal {
		assert.False(t, IsTerminal(s), "期望 %s 非终止态", s)
	}
}

// TestStateMachine_Allowed：AllowedTransitions 返回完整出边集合。
func TestStateMachine_Allowed(t *testing.T) {
	got := AllowedTransitions(StatusDraft)
	assert.ElementsMatch(t, []OrderStatus{StatusPendingApproval, StatusCanceled}, got)

	got = AllowedTransitions(StatusRunning)
	assert.ElementsMatch(t, []OrderStatus{
		StatusSuccess, StatusFailed, StatusPartialSuccess, StatusRollingBack, StatusCanceled,
	}, got)
}
