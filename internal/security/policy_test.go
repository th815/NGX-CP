// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
package security

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/ent"
)

// mockBlock 是 BlockExecutor 的测试替身：记录调用次数、是否要求审批、收到的 IP，
// 用以锁定 T069 动作分流（alert 不调 / semi 要求审批 / auto 不要求审批）。
// 返回零值 *ent.ChangeOrder（无需 DB）即可满足接口签名。
type mockBlock struct {
	calls    int
	lastIP   string
	lastAppr bool
	lastOp   string
	err      error
}

func (m *mockBlock) BlockIP(_ context.Context, ip, _reason, operator string, requireApproval bool) (*ent.ChangeOrder, error) {
	m.calls++
	m.lastIP = ip
	m.lastAppr = requireApproval
	m.lastOp = operator
	if m.err != nil {
		return nil, m.err
	}
	return &ent.ChangeOrder{ID: 1}, nil
}

// sampleWithIP 构造一条形如 Nginx 默认日志的样本，把攻击者 IP 放在首字段（remote_addr
// 位置）——与真实日志一致：ExtractIP 取第一个合法 IP 即来源 IP，语义正确。
func sampleWithIP(ip string) string {
	return fmt.Sprintf(`%s - - [08/Sep/2026:10:00:00 +0000] "GET /?x=1 HTTP/1.1" 200 123 "-" "Mozilla/5.0"`, ip)
}

func TestApplyPolicy_AlertOnlyLeavesEvent(t *testing.T) {
	m := &mockBlock{}
	rule := Rule{ID: "r-5xx-spike", Action: ActionAlert}
	evt := Event{RuleID: "r-5xx-spike", Sample: sampleWithIP("203.0.113.7")}

	applied, err := ApplyPolicy(context.Background(), rule, evt, m, "system(policy)")
	require.NoError(t, err)
	require.Equal(t, ActionAlert, applied)
	require.Equal(t, 0, m.calls, "alert 动作不应触发封禁")
}

func TestApplyPolicy_AutoBlocksWithoutApproval(t *testing.T) {
	m := &mockBlock{}
	rule := Rule{ID: "r-cc-flood", Action: ActionAuto}
	evt := Event{RuleID: "r-cc-flood", Sample: sampleWithIP("203.0.113.45")}

	applied, err := ApplyPolicy(context.Background(), rule, evt, m, "system(policy)")
	require.NoError(t, err)
	require.Equal(t, ActionAuto, applied)
	require.Equal(t, 1, m.calls)
	require.Equal(t, "203.0.113.45", m.lastIP)
	require.False(t, m.lastAppr, "auto 应直接执行（免审批）")
	require.Equal(t, "system(policy)", m.lastOp)
}

func TestApplyPolicy_SemiBlocksWithApproval(t *testing.T) {
	m := &mockBlock{}
	rule := Rule{ID: "r-sql-injection", Action: ActionSemi}
	evt := Event{RuleID: "r-sql-injection", Sample: sampleWithIP("198.51.100.23")}

	applied, err := ApplyPolicy(context.Background(), rule, evt, m, "system(policy)")
	require.NoError(t, err)
	require.Equal(t, ActionSemi, applied)
	require.Equal(t, 1, m.calls)
	require.Equal(t, "198.51.100.23", m.lastIP)
	require.True(t, m.lastAppr, "semi 应进入待审批")
}

func TestApplyPolicy_NoIPDoesNotBlock(t *testing.T) {
	m := &mockBlock{}
	// auto 规则但样本里无合法 IP：绝不能拿空地址去封禁。
	rule := Rule{ID: "r-cc-flood", Action: ActionAuto}
	evt := Event{RuleID: "r-cc-flood", Sample: `just some log line without ip 200 OK`}

	applied, err := ApplyPolicy(context.Background(), rule, evt, m, "system(policy)")
	require.Error(t, err)
	require.Empty(t, applied)
	require.Equal(t, 0, m.calls, "无 IP 时不得调用封禁（防误封空地址）")
}

func TestApplyPolicy_BlockErrorPropagates(t *testing.T) {
	m := &mockBlock{err: fmt.Errorf("deploy rejected")}
	rule := Rule{ID: "r-cc-flood", Action: ActionAuto}
	evt := Event{RuleID: "r-cc-flood", Sample: sampleWithIP("203.0.113.9")}

	_, err := ApplyPolicy(context.Background(), rule, evt, m, "system(policy)")
	require.Error(t, err)
	require.Contains(t, err.Error(), "封禁失败")
}
