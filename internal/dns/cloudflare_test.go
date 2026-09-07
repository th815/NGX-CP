// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 tianhao

package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestServer(t *testing.T, store map[string]cfRecord, patchCalled *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/zones":
			out := cfResponse{Success: true, Result: mustJSON([]cfZone{{ID: "z1", Name: "example.com"}})}
			out.ResultInfo.TotalCount = 1
			writeJSON(w, out)
		case path == "/zones/z1/dns_records":
			if r.Method == http.MethodGet {
				recs := make([]cfRecord, 0, len(store))
				for _, rec := range store {
					recs = append(recs, rec)
				}
				writeJSON(w, cfResponse{Success: true, Result: mustJSON(recs)})
			} else if r.Method == http.MethodPost {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				require.Equal(t, "_acme-challenge.example.com", body["name"])
				require.Equal(t, "txtvalue123", body["content"])
				store["r1"] = cfRecord{ID: "r1", Type: "TXT", Name: body["name"].(string), Content: body["content"].(string)}
				writeJSON(w, cfResponse{Success: true, Result: mustJSON(store["r1"])})
			} else {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case path == "/zones/z1/dns_records/r1":
			if r.Method == http.MethodPatch {
				*patchCalled++
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				store["r1"] = cfRecord{ID: "r1", Type: "TXT", Name: "_acme-challenge.example.com", Content: body["content"].(string)}
				writeJSON(w, cfResponse{Success: true, Result: mustJSON(store["r1"])})
			} else if r.Method == http.MethodDelete {
				delete(store, "r1")
				writeJSON(w, cfResponse{Success: true, Result: mustJSON(cfRecord{ID: "r1"})})
			} else {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCloudflare_SetRecord_Create(t *testing.T) {
	store := map[string]cfRecord{}
	srv := newTestServer(t, store, new(int))
	defer srv.Close()

	cf := NewCloudflare("tok")
	cf.BaseURL = srv.URL
	require.NoError(t, cf.SetRecord(context.Background(), "example.com", "_acme-challenge.example.com", "txtvalue123"))
	require.Len(t, store, 1)
	require.Equal(t, "txtvalue123", store["r1"].Content)
}

func TestCloudflare_SetRecord_Update(t *testing.T) {
	store := map[string]cfRecord{"r1": {ID: "r1", Type: "TXT", Name: "_acme-challenge.example.com", Content: "old"}}
	patchCalled := 0
	srv := newTestServer(t, store, &patchCalled)
	defer srv.Close()

	cf := NewCloudflare("tok")
	cf.BaseURL = srv.URL
	require.NoError(t, cf.SetRecord(context.Background(), "example.com", "_acme-challenge.example.com", "txtvalue123"))
	require.Equal(t, "txtvalue123", store["r1"].Content)
	require.Equal(t, 1, patchCalled)
}

func TestCloudflare_SetRecord_Idempotent(t *testing.T) {
	store := map[string]cfRecord{"r1": {ID: "r1", Type: "TXT", Name: "_acme-challenge.example.com", Content: "txtvalue123"}}
	patchCalled := 0
	srv := newTestServer(t, store, &patchCalled)
	defer srv.Close()

	cf := NewCloudflare("tok")
	cf.BaseURL = srv.URL
	// 相同 value：不应触发 PATCH（幂等）。
	require.NoError(t, cf.SetRecord(context.Background(), "example.com", "_acme-challenge.example.com", "txtvalue123"))
	require.Equal(t, 0, patchCalled)
}

func TestCloudflare_DeleteRecord(t *testing.T) {
	store := map[string]cfRecord{"r1": {ID: "r1", Type: "TXT", Name: "_acme-challenge.example.com", Content: "txtvalue123"}}
	srv := newTestServer(t, store, new(int))
	defer srv.Close()

	cf := NewCloudflare("tok")
	cf.BaseURL = srv.URL
	require.NoError(t, cf.DeleteRecord(context.Background(), "example.com", "_acme-challenge.example.com"))
	require.Empty(t, store)
}

func TestCloudflare_Validate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			out := cfResponse{Success: true, Result: mustJSON([]cfZone{{ID: "z1"}})}
			out.ResultInfo.TotalCount = 1
			writeJSON(w, out)
		}))
		defer srv.Close()
		cf := NewCloudflare("tok")
		cf.BaseURL = srv.URL
		require.NoError(t, cf.Validate(context.Background()))
	})

	t.Run("permission_denied", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, cfResponse{Success: false, Errors: []cfError{{Code: 10000, Message: "Invalid API Token"}}})
		}))
		defer srv.Close()
		cf := NewCloudflare("bad-tok")
		cf.BaseURL = srv.URL
		err := cf.Validate(context.Background())
		require.Error(t, err)
	})
}

func TestRegistry(t *testing.T) {
	require.Contains(t, Registered(), "cloudflare")
	p, err := New(Config{Type: "cloudflare", Token: "tok"})
	require.NoError(t, err)
	require.Equal(t, "cloudflare", p.Name())

	_, err = New(Config{Type: "unknown"})
	require.Error(t, err)
}
