package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var sampleAccounts = []account{{id: "00000000000a"}, {id: "00000000000b"}}

func assertPicked(t *testing.T, r *router, key routeKey, want account) routeAttempt {
	t.Helper()
	got, ok := r.pick(key, nil, nil)
	if !ok || got.id != want.id {
		t.Fatalf("picked %s (ok=%t), want %s", got.id, ok, want.id)
	}
	return got
}

func TestRoutingOrdersAndAffinity(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	order, err := orderedAccounts(sampleAccounts, b.id)
	if err != nil || len(order) != 2 || order[0] != b || order[1] != a {
		t.Fatalf("order %v; error %v", order, err)
	}
	if _, err := orderedAccounts(sampleAccounts, b.id+","+b.id); err == nil {
		t.Fatal("duplicate routing entry was accepted")
	}
	for _, strategy := range []string{fillFirst, roundRobin} {
		t.Run(strategy, func(t *testing.T) {
			r := newRouter(order, strategy, false)
			session := newRouteKey("model", "client-session")
			assertPicked(t, r, session, b)
			assertPicked(t, r, session, b)
			other := newRouteKey("model", "new-session")
			if strategy == fillFirst {
				assertPicked(t, r, other, b)
			} else {
				assertPicked(t, r, other, a)
			}
		})
	}
}

func TestCatalogPreferencesPreserveStickyFailover(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, roundRobin, false)
	now := time.Unix(50000, 0)
	r.now = func() time.Time { return now }
	preferred := map[string]bool{a.id: true}
	key := newRouteKey("model", "session")
	first, ok := r.pick(key, nil, preferred)
	if !ok || first.id != a.id {
		t.Fatal("expected account A")
	}
	r.reject(a, key.model, http.StatusTooManyRequests, "1", nil)
	fallback, ok := r.pick(key, nil, preferred)
	if !ok || fallback.id != b.id {
		t.Fatal("expected account B on cooldown")
	}
	r.confirm(key, fallback)
	now = now.Add(time.Second)
	sticky, ok := r.pick(key, nil, preferred)
	if !ok || sticky.id != b.id {
		t.Fatal("catalog preference overrode the surviving binding")
	}
}

func TestRoundRobinModelPoolsAreIndependent(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, roundRobin, false)
	for _, want := range []account{a, b, a, b} {
		match, ok := r.pick(newRouteKey("only-b", ""), nil, map[string]bool{b.id: true})
		if !ok || match.id != b.id {
			t.Fatal("catalog model was not routed to its account")
		}
		assertPicked(t, r, newRouteKey("both", ""), want)
	}
}

func TestAdaptiveRoutingUpdatesOnlyColdSessions(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, cacheAware, false)
	now := time.Unix(50000, 0)
	r.now = func() time.Time { return now }
	warm := newRouteKey("model", "warm")
	assertPicked(t, r, warm, a)
	assertPicked(t, r, newRouteKey("model", "cold"), b)
	assertPicked(t, r, newRouteKey("model", "another"), a)
	for range minSamples {
		r.observe(a, "model", usageReport(0, 2000))
		r.observe(b, "model", usageReport(1800, 2000))
	}
	assertPicked(t, r, newRouteKey("model", "best"), b)
	assertPicked(t, r, warm, a)
	for range 12 {
		r.observe(a, "model", usageReport(2000, 2000))
		r.observe(b, "model", usageReport(0, 2000))
	}
	assertPicked(t, r, newRouteKey("model", "new-best"), a)
	assertPicked(t, r, newRouteKey("model", "best"), b)
	assertPicked(t, r, newRouteKey("other model", "first"), a)
	now = now.Add(sampleTTL)
	assertPicked(t, r, newRouteKey("model", "after-expiry"), b)
}

func usageReport(cached, input int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}`, input, cached))
}

func TestRoutingCooldownDoesNotMoveBackAfterRecovery(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, fillFirst, false)
	now := time.Unix(50000, 0)
	r.now = func() time.Time { return now }
	key := newRouteKey("model", "warm")
	assertPicked(t, r, key, a)
	r.reject(a, "model", http.StatusTooManyRequests, "3", nil)
	fallback := assertPicked(t, r, key, b)
	now = now.Add(3 * time.Second) // Original cooldown can expire before the fallback completes.
	r.confirm(key, fallback)
	assertPicked(t, r, key, b)
	assertPicked(t, r, newRouteKey("model", "new"), a)
}

func TestFailedFallbackDoesNotReplaceBinding(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, fillFirst, false)
	now := time.Unix(50000, 0)
	r.now = func() time.Time { return now }
	key := newRouteKey("model", "warm")
	first := assertPicked(t, r, key, a)
	r.confirm(key, first)
	r.reject(a, "model", http.StatusTooManyRequests, "3", nil)
	assertPicked(t, r, key, b) // The fallback does not complete.
	now = now.Add(3 * time.Second)
	assertPicked(t, r, key, a)
}

func TestQuotaCooldownIsScopedAndHonorsReset(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, fillFirst, false)
	now := time.Unix(50000, 0)
	r.now = func() time.Time { return now }
	r.reject(a, "gpt-image-2", http.StatusTooManyRequests, "", []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`))
	now = now.Add(time.Minute)
	assertPicked(t, r, newRouteKey("gpt-image-2", ""), b)
	assertPicked(t, r, newRouteKey("text", ""), a)
	now = now.Add(29 * time.Minute)
	assertPicked(t, r, newRouteKey("gpt-image-2", ""), a)
}

func TestAdaptiveWarmupBalancesEachModel(t *testing.T) {
	a, b := sampleAccounts[0], sampleAccounts[1]
	r := newRouter(sampleAccounts, cacheAware, false)
	for _, want := range []account{a, b, a, b} {
		for _, model := range []string{"first", "second"} {
			assertPicked(t, r, newRouteKey(model, ""), want)
		}
	}
}

func withTwoAccounts(t *testing.T, p proxy, strategy string) proxy {
	t.Helper()
	store := accountStore(t.TempDir())
	a := addTestAccount(t, store, "a@example.test", "a", "token-a")
	b := addTestAccount(t, store, "b@example.test", "b", "token-b")
	p.routes = newRouter([]account{a, b}, strategy, false)
	return p
}

func TestHTTPRoutingRetriesBeforeStreamAndRebinds(t *testing.T) {
	var callsA, callsB atomic.Int32
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			callsA.Add(1)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
		case "Bearer token-b":
			callsB.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"b\",\"output\":[]}}\n\n")
		default:
			t.Error("unknown upstream authorization")
		}
	}), fillFirst)
	for range 2 {
		req := request(`{"model":"example","input":"hello","stream":true}`)
		req.Header.Set("X-Session-ID", "stable")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"b"`) || rec.Header().Get("Retry-After") != "" {
			t.Fatalf("unexpected response: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
		}
	}
	if callsA.Load() != 1 || callsB.Load() != 2 {
		t.Fatalf("upstream calls: a=%d b=%d", callsA.Load(), callsB.Load())
	}
}

func TestObservedCacheUsageFromBothResponseModes(t *testing.T) {
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		usage := usageReport(128, 256)
		if r.Header.Get("Authorization") == "Bearer token-b" {
			usage = usageReport(256, 256)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":%s}}\n\n", usage)
	}), cacheAware)
	for i := range minSamples * 2 {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(fmt.Sprintf(`{"model":"example","input":"hello","stream":%t}`, i%2 == 0)))
		if rec.Code != 200 {
			t.Fatalf("response %d: %s", rec.Code, rec.Body.String())
		}
	}
	for _, a := range p.routes.accounts {
		score := p.routes.scores[cacheKey{a.id, "example"}]
		if score.samples != minSamples {
			t.Fatalf("expected %d samples, got %d", minSamples, score.samples)
		}
	}
	assertPicked(t, p.routes, newRouteKey("example", "new"), p.routes.accounts[1])
}

func TestNoRetryAfterSSERejection(t *testing.T) {
	var hits atomic.Int32
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"usage_limit_reached\",\"resets_in_seconds\":1800}}\n\n")
	}), fillFirst)
	now := time.Unix(50000, 0)
	p.routes.now = func() time.Time { return now }
	for i, stream := range []bool{true, false} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(fmt.Sprintf(`{"model":"example-%d","input":"hello","stream":%t}`, i, stream)))
		status := 429
		if stream {
			status = 200
		}
		if rec.Code != status || !strings.Contains(rec.Body.String(), "usage_limit_reached") || hits.Load() != int32(i+1) {
			t.Fatalf("response %d %s (hits %d)", rec.Code, rec.Body.String(), hits.Load())
		}
		model := fmt.Sprintf("example-%d", i)
		if until := p.routes.cooldown[cacheKey{p.routes.accounts[0].id, model}]; !until.Equal(now.Add(30 * time.Minute)) {
			t.Fatalf("SSE cooldown for %q ends at %s", model, until)
		}
	}
}
