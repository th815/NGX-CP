// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	agentv1 "github.com/th/ngxcp/gen/agent/v1"
	"github.com/stretchr/testify/require"
	"github.com/th/ngxcp/internal/domain/config"
	"github.com/th/ngxcp/internal/repo"
)

func newTestConfigStore(t *testing.T) *config.ConfigStore {
	t.Helper()
	client, err := repo.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(context.Background()))
	t.Cleanup(func() { _ = client.Close() })
	return config.New(client)
}

func seedConfigFile(t *testing.T, store *config.ConfigStore) (int, int) {
	ctx := context.Background()
	_, err := store.SyncFromAgent(ctx, 1, []*agentv1.ConfigFile{
		{Path: "/etc/nginx/conf.d/api.conf", Content: "server { listen 80; }\n"},
	})
	require.NoError(t, err)
	files, err := store.ListFiles(ctx, 1)
	require.NoError(t, err)
	require.Len(t, files, 1)
	return files[0].ID, files[0].CurrentRevID
}

func TestConfigHandler_Diff(t *testing.T) {
	ctx := context.Background()
	store := newTestConfigStore(t)
	fid, firstRev := seedConfigFile(t, store)

	// 第二版（manual_edit）。
	rev2, err := store.CreateRevision(ctx, fid, []byte("server { listen 8080; }\n"),
		config.RevisionOpts{Source: config.SourceManualEdit, Author: "web"})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/configs/:id/diff", NewConfigHandler(store).Diff)

	// 缺少参数 → 400。
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/configs/"+strconv.Itoa(fid)+"/diff", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 有效对比 → 200，stats 体现一处修改。
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/configs/"+strconv.Itoa(fid)+
		"/diff?from="+strconv.Itoa(firstRev)+"&to="+strconv.Itoa(rev2.ID), nil)
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &body))
	data, ok := body["data"].(map[string]any)
	require.True(t, ok, "响应应含 data 对象")
	require.EqualValues(t, firstRev, data["from"])
	require.EqualValues(t, rev2.ID, data["to"])
	stats := data["stats"].(map[string]any)
	require.EqualValues(t, 1, stats["added"])
	require.EqualValues(t, 1, stats["deleted"])
	require.EqualValues(t, 1, stats["changed"])
}

func TestConfigHandler_ManualEdit(t *testing.T) {
	ctx := context.Background()
	store := newTestConfigStore(t)
	fid, firstRev := seedConfigFile(t, store)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/configs/:id/manual-edit", NewConfigHandler(store).ManualEdit)

	// 空内容 → 400。
	w0 := httptest.NewRecorder()
	b0, _ := json.Marshal(map[string]string{"content": ""})
	req0, _ := http.NewRequest("POST", "/configs/"+strconv.Itoa(fid)+"/manual-edit", bytes.NewReader(b0))
	req0.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w0, req0)
	require.Equal(t, http.StatusBadRequest, w0.Code)

	// 有效保存 → 200，current_rev 前进。
	w := httptest.NewRecorder()
	b, _ := json.Marshal(map[string]string{"content": "server { listen 80; }\n", "message": "web save"})
	req, _ := http.NewRequest("POST", "/configs/"+strconv.Itoa(fid)+"/manual-edit", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	files, err := store.ListFiles(ctx, 1)
	require.NoError(t, err)
	require.NotEqual(t, firstRev, files[0].CurrentRevID, "保存后 current_rev 应前进")
	require.Equal(t, "manual_edit", files[0].Source)
}
