package node

import (
	"context"
	"testing"
	"time"

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
