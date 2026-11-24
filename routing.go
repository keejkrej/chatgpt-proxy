package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fillFirst  = "fill-first"
	roundRobin = "round-robin"
	cacheAware = "cache-aware"
)

const (
	maxBindings = 4096
	bindingTTL  = time.Hour
	sampleTTL   = 30 * time.Minute
	minSamples  = 3
)

type routeKey struct {
	model   string
	session [32]byte
}

func newRouteKey(model, session string) routeKey {
	key := routeKey{model: model}
	if session != "" {
		key.session = sha256.Sum256([]byte(session))
	}
	return key
}

type routeAttempt struct {
	account
	prior string
}

type binding struct {
	id        string
	expiresAt time.Time
}

type cacheKey struct{ id, model string }

type cacheScore struct {
	input, cached float64 // Discounted token counts, not a mean of request percentages.
	samples       int
	updatedAt     time.Time
}

func (s cacheScore) ratio() float64 { return s.cached / s.input }

// router holds process-local routing state. Account order never changes while serving.
// It stores hashes of session keys, not prompts or raw session identifiers.
type router struct {
	accounts []account
	strategy string
	cacheLog bool
	now      func() time.Time

	mu       sync.Mutex
	byModel  map[string]modelCursor
	sticky   map[routeKey]binding
	cooldown map[cacheKey]time.Time
	scores   map[cacheKey]cacheScore
}

type modelCursor struct{ next, cold uint64 }

func makeRoom[K comparable, V any](m map[K]V, key K) {
	if _, ok := m[key]; !ok && len(m) >= maxBindings {
		for old := range m {
			delete(m, old)
			break
		}
	}
}

func newRouter(accounts []account, strategy string, cacheLog bool) *router {
	return &router{
		accounts: accounts, strategy: strategy, cacheLog: cacheLog, now: time.Now,
		sticky: make(map[routeKey]binding), cooldown: make(map[cacheKey]time.Time),
		scores: make(map[cacheKey]cacheScore), byModel: make(map[string]modelCursor),
	}
}

func orderedAccounts(accounts []account, preferred string) ([]account, error) {
	if preferred == "" {
		return accounts, nil
	}
	order := make([]account, 0, len(accounts))
	seen := make(map[string]bool)
	for _, id := range strings.Split(preferred, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("empty account ID in -order")
		}
		a, err := accountByID(accounts, id)
		if err != nil {
			return nil, err
		}
		if seen[a.id] {
			return nil, fmt.Errorf("duplicate account ID in -order")
		}
		seen[a.id] = true
		order = append(order, a)
	}
	for _, a := range accounts {
		if !seen[a.id] {
			order = append(order, a)
		}
	}
	return order, nil
}

func (r *router) pick(key routeKey, tried, preferred map[string]bool) (routeAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	hasSession := key.session != [32]byte{}
	available := func(a account) bool {
		return !tried[a.id] && r.available(a.id, key.model, now)
	}
	hasPreferred := slices.ContainsFunc(r.accounts, func(a account) bool { return preferred[a.id] && available(a) })
	eligible := func(a account) bool {
		return available(a) && (!hasPreferred || preferred[a.id])
	}
	if hasSession {
		if b, ok := r.sticky[key]; ok && now.Before(b.expiresAt) {
			for _, a := range r.accounts {
				if a.id == b.id && available(a) {
					return routeAttempt{a, b.id}, true
				}
			}
		}
	}
	if len(r.accounts) == 0 {
		return routeAttempt{}, false
	}
	index := -1
	cursor := r.byModel[key.model]
	if r.strategy != fillFirst {
		makeRoom(r.byModel, key.model)
		defer func() { r.byModel[key.model] = cursor }()
	}
	if r.strategy == cacheAware {
		cursor.cold++
		if cursor.cold%8 != 0 {
			index = r.bestModelScore(key.model, now, eligible)
		}
	}
	switch r.strategy {
	case fillFirst:
		for i, a := range r.accounts {
			if eligible(a) {
				index = i
				break
			}
		}
	}
	if index < 0 {
		for offset := range r.accounts {
			i := int((cursor.next + uint64(offset)) % uint64(len(r.accounts)))
			if eligible(r.accounts[i]) {
				index = i
				cursor.next = uint64(i + 1)
				break
			}
		}
	}
	if index < 0 {
		return routeAttempt{}, false
	}
	a := r.accounts[index]
	if b := r.sticky[key]; hasSession && !now.Before(b.expiresAt) {
		// Reserve a cold session to keep concurrent first turns together. Rebind
		// an existing session only after a fallback produces a usable result.
		r.bind(key, a.id, now)
	}
	return routeAttempt{a, r.sticky[key].id}, true
}

func (r *router) bestModelScore(model string, now time.Time, eligible func(account) bool) int {
	best, index := -1.0, -1
	for i, a := range r.accounts {
		if !eligible(a) {
			continue
		}
		s := r.scores[cacheKey{a.id, model}]
		if s.samples < minSamples || now.Sub(s.updatedAt) >= sampleTTL {
			return -1 // Use round-robin until every eligible account has fresh observations.
		}
		if s.ratio() > best {
			index, best = i, s.ratio()
		}
	}
	return index
}

func (r *router) available(id, model string, now time.Time) bool {
	return !now.Before(r.cooldown[cacheKey{id, ""}]) && !now.Before(r.cooldown[cacheKey{id, model}])
}

func (r *router) bind(key routeKey, id string, now time.Time) {
	makeRoom(r.sticky, key)
	r.sticky[key] = binding{id, now.Add(bindingTTL)}
}

func (r *router) confirm(key routeKey, a routeAttempt) {
	if key.session == [32]byte{} {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.sticky[key]
	// Only replace the binding observed when picking; a concurrently completed
	// request may already have rebound the session to another account.
	if r.available(a.id, key.model, now) && (b.id == a.id || b.id == a.prior || !now.Before(b.expiresAt)) {
		r.bind(key, a.id, now)
	}
}

func (r *router) reject(a account, model string, status int, retryAfter string, body []byte) {
	if len(r.accounts) < 2 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	wait := 30 * time.Second
	key := cacheKey{id: a.id}
	if status == http.StatusTooManyRequests {
		key.model = model
		const maxWait = 7 * 24 * time.Hour
		wait = time.Minute
		if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
			wait = time.Duration(min(seconds, int64(maxWait/time.Second))) * time.Second
		} else if date, err := http.ParseTime(retryAfter); err == nil && date.After(now) {
			wait = min(date.Sub(now), maxWait)
		}
		if delay := quotaResetDelay(body, now, maxWait); delay > 0 {
			wait = max(wait, delay)
		}
	}
	until := now.Add(wait)
	if until.After(r.cooldown[key]) {
		makeRoom(r.cooldown, key)
		r.cooldown[key] = until
	}
}

func quotaResetDelay(body []byte, now time.Time, limit time.Duration) time.Duration {
	type quotaError struct {
		Type     string `json:"type"`
		Code     string `json:"code"`
		ResetsAt int64  `json:"resets_at"`
		ResetsIn int64  `json:"resets_in_seconds"`
	}
	var envelope struct {
		quotaError
		Error    quotaError `json:"error"`
		Response struct {
			Error quotaError `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return 0
	}
	for _, q := range []quotaError{envelope.Error, envelope.Response.Error, envelope.quotaError} {
		if q.Type == "usage_limit_reached" || q.Code == "usage_limit_reached" {
			if reset := time.Unix(q.ResetsAt, 0); q.ResetsAt > 0 && reset.After(now) {
				return min(reset.Sub(now), limit)
			}
			if q.ResetsIn > 0 {
				return time.Duration(min(q.ResetsIn, int64(limit/time.Second))) * time.Second
			}
		}
	}
	return 0
}

func (r *router) observe(a account, model string, usage json.RawMessage) {
	if r.strategy != cacheAware && !r.cacheLog {
		return
	}
	var report struct {
		Input   int64 `json:"input_tokens"`
		Details struct {
			Cached *int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if json.Unmarshal(usage, &report) != nil || report.Input <= 0 || report.Details.Cached == nil {
		return // Missing usage is not evidence of a miss.
	}
	cached := *report.Details.Cached
	if cached < 0 || cached > report.Input {
		return
	}
	r.mu.Lock()
	now := r.now()
	key := cacheKey{a.id, model}
	s := r.scores[key]
	makeRoom(r.scores, key)
	if now.Sub(s.updatedAt) >= sampleTTL {
		s = cacheScore{}
	}
	s.input = s.input*0.8 + float64(report.Input)
	s.cached = s.cached*0.8 + float64(cached)
	s.samples = min(minSamples, s.samples+1)
	s.updatedAt = now
	r.scores[key] = s
	r.mu.Unlock()
	if r.cacheLog {
		log.Printf("cache account=%s model=%q cached=%d/%d rolling=%.1f%%", a.id[:12], model, cached, report.Input, 100*s.ratio())
	}
}
