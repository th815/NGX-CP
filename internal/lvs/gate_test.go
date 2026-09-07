// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package lvs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGate_Check(t *testing.T) {
	t.Run("nil_check_passes", func(t *testing.T) {
		g := NewGate(nil)
		require.NoError(t, g.Check(context.Background(), 1))
	})

	t.Run("passed_true", func(t *testing.T) {
		g := NewGate(func(ctx context.Context, id int) (bool, []string, error) {
			return true, nil, nil
		})
		require.NoError(t, g.Check(context.Background(), 1))
	})

	t.Run("passed_false_blocks", func(t *testing.T) {
		g := NewGate(func(ctx context.Context, id int) (bool, []string, error) {
			return false, []string{"vip_on_lo", "arp_ignore"}, nil
		})
		err := g.Check(context.Background(), 1)
		require.Error(t, err)
	})

	t.Run("check_error_surfaced", func(t *testing.T) {
		g := NewGate(func(ctx context.Context, id int) (bool, []string, error) {
			return false, nil, errors.New("store down")
		})
		err := g.Check(context.Background(), 1)
		require.Error(t, err)
	})
}

func TestNodeStatusCompliance_Signature(t *testing.T) {
	// NodeStatusCompliance 依赖 ent 客户端（需 DB），集成见 handler；此处仅确认其满足 ComplianceChecker。
	var _ ComplianceChecker = NodeStatusCompliance(nil)
}
