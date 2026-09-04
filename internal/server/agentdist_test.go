// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao
//
// Join Token 吊销端点的 HTTP 级冒烟测试：签发 → 验签成功 → revoke → 验签立即失败。
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	entnode "github.com/th/ngxcp/ent/node"
	"github.com/th/ngxcp/internal/domain/node"
	"github.com/th/ngxcp/internal/pkg/apperr"
	"github.com/th/ngxcp/internal/repo"
	"github.com/gin-gonic/gin"
)

// TestJoinTokenRevokeEndpoint 验证「独立吊销 Join Token」端点的端到端行为：
//   - IssueJoinToken 签发后，VerifyJoinToken 成功回绑节点；
//   - revokeJoinToken 吊销后，验签立即失败（令牌已 revoked，即时生效）。
func TestJoinTokenRevokeEndpoint(t *testing.T) {
	client, err := repo.Open("sqlite", "file:ngxcp_joinrevoke?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer client.Close()
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	n, err := client.Node.Create().
		SetName("rs-join").
		SetAddress("10.0.0.51").
		SetRole(entnode.RoleRealServer).
		SetStatus(entnode.StatusEnrolling).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	svc := node.New(client, nil)
	ad := &agentDist{nodeSvc: svc}
	ctx := context.Background()

	// 签发节点绑定 Join Token。
	tok, _, err := svc.IssueJoinToken(ctx, n.ID, "real_server", time.Hour)
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	if _, err := svc.VerifyJoinToken(ctx, tok); err != nil {
		t.Fatalf("签发后验签应成功: %v", err)
	}

	// 独立吊销端点（只吊销、不签发）。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/nodes/"+strconv.Itoa(n.ID)+"/join-token/revoke", nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(n.ID)}}
	ad.revokeJoinToken(c)
	if w.Code != http.StatusOK {
		t.Fatalf("revokeJoinToken status = %d, want 200\nbody=%s", w.Code, w.Body.String())
	}

	// 吊销后验签立即失败（即使令牌明文仍在、未过期）。
	if _, err := svc.VerifyJoinToken(ctx, tok); apperr.CodeOf(err) != apperr.CodeUnauthorized {
		t.Fatalf("吊销后验签错误 = %v, want CodeUnauthorized（令牌已吊销）", err)
	}
}
