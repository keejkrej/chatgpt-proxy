package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fetchLocalModels(p proxy, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/models", nil)
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestModelCatalogListsUnionAndInformsRouting(t *testing.T) {
	var catalogs atomic.Int32
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if r.URL.Path == "/backend-api/codex/models" {
			if r.Method != http.MethodGet || r.URL.Query().Get("client_version") != "0.171.0" || r.Header.Get("ChatGPT-Account-Id") != "workspace" {
				t.Errorf("unexpected catalog request: %s %s", r.Method, r.URL)
			}
			catalogs.Add(1)
			if token == "Bearer token-a" {
				fmt.Fprint(w, `{"models":[{"slug":"shared","visibility":"list"},{"slug":"omitted","visibility":"hide"}]}`)
			} else {
				fmt.Fprint(w, `{"models":[{"slug":"shared"},{"slug":"exclusive"}]}`)
			}
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model == "exclusive" && token != "Bearer token-b" || req.Model == "new-model" && token != "Bearer token-a" {
			t.Errorf("incorrect account for %s: %s", req.Model, token)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}), fillFirst)
	if rec := fetchLocalModels(p, false); rec.Code != 401 || catalogs.Load() != 0 {
		t.Fatal("unauthenticated model discovery contacted upstream")
	}
	for range 2 {
		rec := fetchLocalModels(p, true)
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || list.Object != "list" {
			t.Fatalf("catalog status %d: %s", rec.Code, rec.Body.String())
		}
		var ids []string
		for _, m := range list.Data {
			ids = append(ids, m.ID)
		}
		if !slices.Equal(ids, []string{"exclusive", "shared"}) {
			t.Fatalf("unexpected model IDs: %v", ids)
		}
	}
	if catalogs.Load() != 2 {
		t.Fatalf("expected two upstream catalog calls, got %d", catalogs.Load())
	}
	for _, model := range []string{"exclusive", "new-model"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)))
		if rec.Code != 200 {
			t.Fatalf("request status %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func TestModelRoutingPrefersConfirmedAndFallsBack(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if r.URL.Path == "/backend-api/codex/models" {
			if token == "Bearer token-a" {
				w.WriteHeader(503)
			} else {
				fmt.Fprint(w, `{"models":[{"slug":"model"}]}`)
			}
			return
		}
		mu.Lock()
		tokens = append(tokens, token)
		mu.Unlock()
		if token == "Bearer token-b" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}), fillFirst)
	if rec := fetchLocalModels(p, true); rec.Code != 200 {
		t.Fatalf("unexpected catalog status %d", rec.Code)
	}
	for range 2 {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(`{"model":"model","input":"hello"}`))
		if rec.Code != 200 {
			t.Fatalf("routing status %d: %s", rec.Code, rec.Body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(tokens, []string{"Bearer token-b", "Bearer token-a", "Bearer token-a"}) {
		t.Fatalf("unexpected routing order: %v", tokens)
	}
}

func TestModelCatalogFetchesAccountsInParallel(t *testing.T) {
	healthyStarted := make(chan struct{})
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer blocked":
			select {
			case <-healthyStarted:
				w.WriteHeader(503)
			case <-r.Context().Done():
				t.Error("a slow account held up a queued healthy account")
			}
		case "Bearer healthy":
			close(healthyStarted)
			fmt.Fprint(w, `{"models":[{"slug":"healthy"}]}`)
		default:
			w.WriteHeader(503)
		}
	}), fillFirst)
	store := accountStore(t.TempDir())
	accounts := []account{addTestAccount(t, store, "blocked@example.test", "blocked", "blocked")}
	for i := range 7 {
		accounts = append(accounts, addTestAccount(t, store, "unavailable@example.test", fmt.Sprint(i), "unavailable"))
	}
	accounts = append(accounts, addTestAccount(t, store, "healthy@example.test", "healthy", "healthy"))
	p.routes = newRouter(accounts, fillFirst, false)
	rec := fetchLocalModels(p, true)
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("unexpected model response: %d: %s", rec.Code, rec.Body)
	}
}

func TestModelCatalogPartialFailureAndRenewal(t *testing.T) {
	var hits atomic.Int32
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer renewed" {
			fmt.Fprint(w, `{"models":[{"slug":"renewed-model"}]}`)
			return
		}
		w.WriteHeader(401)
	}), fillFirst)
	now := time.Unix(50000, 0)
	p.models.now = func() time.Time { return now }
	if rec := fetchLocalModels(p, true); rec.Code != 503 {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	putAuth(t, p.routes.accounts[1].path, "renewed")
	if rec := fetchLocalModels(p, true); rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("unexpected status: %d: %s", rec.Code, rec.Body.String())
	}
	before := hits.Load()
	now = now.Add(modelCatalogTTL)
	if rec := fetchLocalModels(p, true); rec.Code != 200 || hits.Load()-before != 2 {
		t.Fatalf("cache did not expire: status %d, hits %d", rec.Code, hits.Load()-before)
	}
	putAuth(t, p.routes.accounts[1].path, "expired-again")
	if rec := fetchLocalModels(p, true); rec.Code != 503 {
		t.Fatalf("stale login reused: %d", rec.Code)
	}
}
