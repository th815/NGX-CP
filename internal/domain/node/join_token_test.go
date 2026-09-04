package node

import (
	"context"
	"testing"
	"time"

	entenrolltoken "github.com/th/ngxcp/ent/enrolltoken"
	entjointoken "github.com/th/ngxcp/ent/jointoken"
	entnode "github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/repo"
)

// TestJoinTokenIssueVerifyRevoke 锁定仿 mmw 的「服务端 join_tokens 表」模型关键不变量：
//   - 签发后验签返回绑定 nodeID（1 token = 1 node）；
//   - 吊销后验签立即失败（revoked 即时生效，无需等过期）；
//   - 轮换（RevokeNodeJoinTokens + 重新签发）使旧令牌失效、新令牌可用。
func TestJoinTokenIssueVerifyRevoke(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:jtoken?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := New(client, nil)

	n, err := client.Node.Create().
		SetName("rs-1").SetAddress("10.0.0.2").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusEnrolling).
		Save(ctx)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	// 1) 签发 + 验签：返回绑定 nodeID。
	tok, _, err := svc.IssueJoinToken(ctx, n.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	gotID, err := svc.VerifyJoinToken(ctx, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if gotID != n.ID {
		t.Fatalf("verify nodeID = %d, want %d（1 token = 1 node）", gotID, n.ID)
	}

	// 2) 吊销后验签立即失败。
	if err := svc.RevokeNodeJoinTokens(ctx, n.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.VerifyJoinToken(ctx, tok); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("吊销后验签错误 = %v, want CodeUnauthorized", err)
	}

	// 3) 轮换：重新签发 → 新令牌可用，旧令牌仍失效（旋转即吊销）。
	tok2, _, err := svc.IssueJoinToken(ctx, n.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if _, err := svc.VerifyJoinToken(ctx, tok2); err != nil {
		t.Fatalf("新令牌验签应成功: %v", err)
	}
	if _, err := svc.VerifyJoinToken(ctx, tok); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("旧令牌在轮换后必须失效: %v", err)
	}
}

// TestJoinTokenEnrollingExpiry 锁定「仅 enrolling 阶段受过期约束」语义：
// enrolling 节点持过期令牌验签失败；已纳管（online）节点即使令牌过期也可重建证书。
func TestJoinTokenEnrollingExpiry(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:jexp?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := New(client, nil)

	en, _ := client.Node.Create().
		SetName("enr").SetAddress("10.0.0.3").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusEnrolling).
		Save(ctx)
	tokE, _, err := svc.IssueJoinToken(ctx, en.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// 强制过期（API 不接受负 ttl，故直接改库内过期时间）。
	_, _ = client.JoinToken.Update().
		Where(entjointoken.HasNodeWith(entnode.ID(en.ID))).
		SetExpiresAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	if _, err := svc.VerifyJoinToken(ctx, tokE); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("enrolling + 过期令牌应拒绝: %v", err)
	}

	on, _ := client.Node.Create().
		SetName("onl").SetAddress("10.0.0.4").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusOnline).
		Save(ctx)
	tokO, _, err := svc.IssueJoinToken(ctx, on.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, _ = client.JoinToken.Update().
		Where(entjointoken.HasNodeWith(entnode.ID(on.ID))).
		SetExpiresAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	if _, err := svc.VerifyJoinToken(ctx, tokO); err != nil {
		t.Fatalf("online + 过期令牌应放行（重建证书）: %v", err)
	}
}

// TestEnrollTokenIssueVerifyRevoke 锁定「服务端 enroll_tokens 表」模型关键不变量：
//   - 签发后验签返回绑定 nodeID（1 token = 1 node）；
//   - 一次性：同一令牌第二次验签立即失败（已使用）；
//   - 过期令牌验签失败；
//   - 吊销后验签立即失败（revoked 即时生效，无需等过期）；
//   - 持久化：数据来自控制面库，重启不丢（此处用独立内存库验证落库语义）。
func TestEnrollTokenIssueVerifyRevoke(t *testing.T) {
	ctx := context.Background()
	client, err := repo.Open("sqlite", "file:etoken?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := New(client, nil)

	n, err := client.Node.Create().
		SetName("rs-e1").SetAddress("10.0.0.9").
		SetRole(entnode.RoleRealServer).SetStatus(entnode.StatusEnrolling).
		Save(ctx)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	// 1) 签发 + 验签：返回绑定 nodeID。
	tok, _, err := svc.IssueEnrollToken(ctx, n.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	gotID, err := svc.VerifyEnrollToken(ctx, tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if gotID != n.ID {
		t.Fatalf("verify nodeID = %d, want %d（1 token = 1 node）", gotID, n.ID)
	}

	// 2) 一次性：第二次验签立即失败（已使用）。
	if _, err := svc.VerifyEnrollToken(ctx, tok); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("重复使用令牌错误 = %v, want CodeUnauthorized", err)
	}

	// 3) 重新签发 → 新令牌可用（供节点轮换场景）。
	tok2, _, err := svc.IssueEnrollToken(ctx, n.ID, time.Hour)
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if _, err := svc.VerifyEnrollToken(ctx, tok2); err != nil {
		t.Fatalf("新令牌验签应成功: %v", err)
	}

	// 4) 过期令牌验签失败（强制改库内过期时间）。
	exp, _, err := svc.IssueEnrollToken(ctx, n.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue exp: %v", err)
	}
	_, _ = client.EnrollToken.Update().
		Where(entenrolltoken.HasNodeWith(entnode.ID(n.ID))).
		SetExpiresAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	if _, err := svc.VerifyEnrollToken(ctx, exp); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("过期令牌应拒绝: %v", err)
	}

	// 5) 吊销：尚未使用的令牌被吊销后验签失败（revoked 即时生效）。
	rev, _, err := svc.IssueEnrollToken(ctx, n.ID, time.Hour)
	if err != nil {
		t.Fatalf("issue rev: %v", err)
	}
	if err := svc.RevokeNodeEnrollTokens(ctx, n.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.VerifyEnrollToken(ctx, rev); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("吊销后验签错误 = %v, want CodeUnauthorized", err)
	}
}
