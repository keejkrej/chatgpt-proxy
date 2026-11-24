package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "local-key-used-only-by-tests-12345"

func testProxy(t *testing.T, handler http.HandlerFunc) proxy {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	path := filepath.Join(t.TempDir(), "auth.json")
	putAuth(t, path, "subscription-token")
	client := upstreamClient()
	return proxy{
		endpoint:      upstream.URL + "/backend-api/codex/responses",
		imageEndpoint: upstream.URL + "/backend-api/codex/images/generations",
		editEndpoint:  upstream.URL + "/backend-api/codex/images/edits",
		models:        newModelCatalog(client, upstream.URL+"/backend-api/codex/models?client_version=0.171.0"),
		client:        client, key: testKey, routes: newRouter([]account{{id: "000000000001", path: path}}, roundRobin, false),
	}
}

func putAuth(t *testing.T, path, accessToken string) {
	t.Helper()
	body := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":"account","refresh_token":"never-forward"}}`, accessToken)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func request(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestResponsesJSON(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		for name, want := range map[string]string{
			"Authorization":      "Bearer subscription-token",
			"ChatGPT-Account-Id": "account",
			"Accept":             "text/event-stream",
			"Cookie":             "",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		var body struct {
			Model        string  `json:"model"`
			Stream       bool    `json:"stream"`
			Store        bool    `json:"store"`
			Instructions *string `json:"instructions"`
			Input        []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "example" || !body.Stream || body.Store || body.Instructions == nil || *body.Instructions != "" ||
			len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != "hello" {
			t.Errorf("wrong upstream payload: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "private")
		fmt.Fprint(w, ": ping\r\n\r\ndata:\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"id\":\"second\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"first\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"unindexed\"}}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n")
	})
	req := request(`{"model":"example","input":"hello","store":true}`)
	req.Header.Set("Cookie", "local-secret=x")
	req.Header.Set("ChatGPT-Account-Id", "client-spoof")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response %d %v: %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"id":"resp_1","output":[{"id":"first"},{"id":"second"},{"id":"unindexed"}]}` {
		t.Errorf("response body = %s", got)
	}
}

func TestResponsesStreamsBeforeCompletion(t *testing.T) {
	finish := make(chan struct{})
	defer close(finish)
	const chunk = "data: {\"type\":\"response.created\"}\n"
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chunk+"\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-r.Context().Done():
		}
	})
	server := httptest.NewServer(p)
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(
		`{"model":"example","input":[{"role":"system","content":"test"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	client := server.Client()
	client.Timeout = 5 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected response: %s %v", resp.Status, resp.Header)
	}
	if line, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil || line != chunk {
		t.Fatalf("first line = %q, error = %v", line, err)
	}
}

func TestRejectsBadClientRequests(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected client request reached the upstream")
	})
	for _, tc := range []struct {
		name   string
		edit   func(*http.Request)
		body   string
		status int
	}{
		{"bad token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, "", 401},
		{"foreign host", func(r *http.Request) { r.Host = "example.com:80" }, "", 403},
		{"method", func(r *http.Request) { r.Method = "GET" }, "", 405},
		{"route", func(r *http.Request) { r.URL.Path = "/v1/chat/completions" }, "", 404},
		{"content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, "", 415},
		{"invalid JSON", nil, "{", 400},
		{"invalid input type", nil, `{"model":"example","input":true}`, 400},
		{"no model", nil, `{"input":"hello"}`, 400},
		{"previous response", nil, `{"model":"example","input":"hello","previous_response_id":"prev"}`, 400},
		{"too large", nil, strings.Repeat("x", 8<<20+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, req := httptest.NewRecorder(), request(tc.body)
			if tc.edit != nil {
				tc.edit(req)
			}
			p.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestUpstreamFailureAndCredentialReload(t *testing.T) {
	var hits atomic.Int32
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer subscription-token"
		if hits.Add(1) == 2 {
			want = "Bearer renewed"
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("upstream auth = %q, want %q", r.Header.Get("Authorization"), want)
		}
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	})
	for _, token := range []string{"subscription-token", "renewed"} {
		putAuth(t, p.routes.accounts[0].path, token)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(`{"model":"example","input":"hello"}`))
		if rec.Code != 429 || rec.Header().Get("Retry-After") != "30" || rec.Body.String() != `{"error":{"message":"slow down"}}` {
			t.Fatalf("unexpected rate limit response: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
		}
	}
	if err := os.WriteFile(p.routes.accounts[0].path, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"never-used"}`), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, request(`{"model":"example","input":"hello"}`))
	if rec.Code != 503 || hits.Load() != 2 {
		t.Fatalf("API-key login: status %d, upstream hits %d", rec.Code, hits.Load())
	}
}

func TestUpstreamRedirectNotFollowed(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Error("upstream redirect followed")
			return
		}
		http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, request(`{"model":"example","input":"hello"}`))
	if rec.Code != 502 || rec.Header().Get("Location") != "" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
}

func TestNonStreamingTerminalEvents(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		status     int
	}{
		{`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"id":"kept"}]}}`, `{"output":[{"id":"kept"}],"status":"incomplete"}`, 0},
		{`{"type":"response.failed"}`, "", 502},
		{`{"type":"response.output_text.delta","delta":"partial"}`, "", 502},
		{`{"type":"response.completed","response":null}`, "", 502},
		{`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded"}}}`, "", 400},
		{`{"type":"error","error":{"type":"usage_limit_reached"}}`, "", 429},
		{`{"status":429,"error":{"message":"Too many concurrent requests"}}`, "", 429},
		{`{"status":401,"error":{"message":"Expired"}}`, "", 401},
		{`{"status":429}`, "", 429},
	} {
		body, failure := readResponses(strings.NewReader("data: "+tc.body+"\n\n"), nil, nil)
		var status int
		if failure != nil {
			status = failure.status
		}
		if string(body) != tc.want || status != tc.status {
			t.Errorf("%s: got %s, error status %d; want %s, %d", tc.body, body, status, tc.want, tc.status)
		}
	}
}

func TestCodexCompatibilityFields(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`[]`, `["reasoning.encrypted_content"]`},
		{`["web_search_call.action.sources","unknown","reasoning.encrypted_content"]`, `["reasoning.encrypted_content","web_search_call.action.sources"]`},
	} {
		body := `{"model":"example","input":[{"role":"system","content":"test"}],"include":` + tc.input + `,"temperature":1,"top_p":1,"context_management":[],"user":"ignored"}`
		payload, _, err := codexPayload([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["include"]) != tc.want || !strings.Contains(string(fields["input"]), `"role":"developer"`) {
			t.Errorf("unexpected payload: %s", payload)
		}
		for _, name := range []string{"temperature", "top_p", "context_management", "user"} {
			if fields[name] != nil {
				t.Errorf("payload includes %s: %s", name, payload)
			}
		}
	}
}

func TestTypelessRateLimit(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"status\":429,\"error\":{\"message\":\"Too many concurrent requests\"}}\n\n")
	})
	for _, stream := range []bool{false, true} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(fmt.Sprintf(`{"model":"example","input":"hello","stream":%t}`, stream)))
		want := 429
		if stream {
			want = 200
		}
		if rec.Code != want || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
			t.Errorf("stream=%t: status=%d, body=%s", stream, rec.Code, rec.Body.String())
		}
	}
}

func TestTypedStreamErrorNormalized(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"rate_limit_exceeded\"}}\n\n")
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, request(`{"model":"example","input":"hello","stream":true}`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded","message":`) {
		t.Errorf("unexpected stream: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStreamTerminalHandling(t *testing.T) {
	for _, completed := range []bool{true, false} {
		p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"codex.rate_limits\"}\r")
			fmt.Fprint(w, "data: {\"type\":\"response.metadata\"}\r")
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"kept\"}}\n")
			if completed {
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}")
			}
		})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, request(`{"model":"example","input":"hello","stream":true}`))
		got := rec.Body.String()
		want := `"output":[{"id":"kept"}]`
		if !completed {
			want = `"type":"error"`
		}
		if rec.Code != 200 || strings.Contains(got, "codex.rate_limits") || strings.Contains(got, "response.metadata") || !strings.Contains(got, want) {
			t.Errorf("unexpected stream, completed=%t: %d %s", completed, rec.Code, got)
		}
	}
}
