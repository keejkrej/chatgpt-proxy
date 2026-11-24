package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	codexModelsEndpoint = "https://chatgpt.com/backend-api/codex/models?client_version=0.171.0"
	modelCatalogTTL     = 5 * time.Minute
)

type modelCatalog struct {
	endpoint string
	client   *http.Client
	now      func() time.Time
	mu       sync.Mutex
	entries  map[string]modelSnapshot
}

type modelSnapshot struct {
	token   [32]byte
	expires time.Time
	// Every advertised slug can inform routing, but only visible ones are listed.
	slugs map[string]bool
}

func newModelCatalog(client *http.Client, endpoint string) *modelCatalog {
	return &modelCatalog{client: client, endpoint: endpoint, now: time.Now, entries: make(map[string]modelSnapshot)}
}

func (c *modelCatalog) snapshot(id string, token [32]byte) (modelSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.entries[id]
	return s, ok && s.token == token && c.now().Before(s.expires)
}

func (c *modelCatalog) get(ctx context.Context, a account) (modelSnapshot, bool) {
	auth, err := readCredentials(a.path)
	if err != nil {
		return modelSnapshot{}, false
	}
	token := sha256.Sum256([]byte(auth.AccessToken))
	if cached, ok := c.snapshot(a.id, token); ok {
		return cached, true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	slugs, err := c.fetch(ctx, auth)
	if err != nil {
		return modelSnapshot{}, false
	}
	s := modelSnapshot{token: token, expires: c.now().Add(modelCatalogTTL), slugs: slugs}
	c.mu.Lock()
	c.entries[a.id] = s
	c.mu.Unlock()
	return s, true
}

func (c *modelCatalog) fetch(ctx context.Context, auth credentials) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	setCodexHeaders(req.Header, auth)
	setCodexClientProfile(req.Header)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Codex rejected the catalog request")
	}
	const maxCatalog = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalog+1))
	var payload struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err != nil || len(body) > maxCatalog || json.Unmarshal(body, &payload) != nil || payload.Models == nil {
		return nil, errors.New("invalid Codex catalog")
	}
	slugs := make(map[string]bool, len(payload.Models))
	for _, model := range payload.Models {
		if slug := strings.TrimSpace(model.Slug); slug != "" && len(slug) <= 128 {
			slugs[slug] = slugs[slug] || model.Visibility == "list" || model.Visibility == ""
		}
	}
	return slugs, nil
}

func (c *modelCatalog) serve(w http.ResponseWriter, r *http.Request, accounts []account) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	const maxParallel = 8
	workers := min(maxParallel, len(accounts))
	jobs := make(chan account, len(accounts))
	for _, a := range accounts {
		jobs <- a
	}
	close(jobs)
	results := make(chan modelSnapshot, workers)
	for range workers {
		go func() {
			for a := range jobs {
				s, _ := c.get(ctx, a)
				results <- s
			}
		}()
	}
	ids := map[string]bool{}
	loaded := false
	for range accounts {
		s := <-results
		if s.slugs == nil {
			continue
		}
		loaded = true
		for id, visible := range s.slugs {
			if visible {
				ids[id] = true
			}
		}
	}
	if !loaded {
		writeError(w, http.StatusServiceUnavailable, "models_unavailable", "No Codex model catalog is available. Check your ChatGPT account logins.")
		return
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	slices.Sort(ordered)
	type listedModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Owner   string `json:"owned_by"`
	}
	list := struct {
		Object string        `json:"object"`
		Data   []listedModel `json:"data"`
	}{Object: "list", Data: make([]listedModel, 0, len(ids))}
	for _, id := range ordered {
		list.Data = append(list.Data, listedModel{ID: id, Object: "model", Owner: "openai"})
	}
	body, _ := json.Marshal(list)
	writeJSON(w, http.StatusOK, body)
}

// Catalogs are routing preferences, not a hard allowlist. Unknown model names
// and accounts without a confirmed match can still fall back to upstream.
func (c *modelCatalog) preferred(accounts []account, model string) map[string]bool {
	var preferred map[string]bool
	for _, a := range accounts {
		c.mu.Lock()
		s, cached := c.entries[a.id]
		fresh := cached && c.now().Before(s.expires)
		c.mu.Unlock()
		if _, hasModel := s.slugs[model]; !fresh || !hasModel {
			continue
		}
		auth, err := readCredentials(a.path)
		if err != nil || s.token != sha256.Sum256([]byte(auth.AccessToken)) {
			continue // Renewal or logout invalidates the hint immediately.
		}
		if preferred == nil {
			preferred = make(map[string]bool)
		}
		preferred[a.id] = true
	}
	return preferred
}
