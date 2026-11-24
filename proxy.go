package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"
)

type proxy struct {
	endpoint      string
	imageEndpoint string
	editEndpoint  string
	models        *modelCatalog
	client        *http.Client
	routes        *router
	key           string
}

func (p proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
		writeError(w, http.StatusForbidden, "invalid_host", "Use 127.0.0.1 or localhost.")
		return
	}
	edit := r.URL.Path == "/v1/images/edits"
	image := edit || r.URL.Path == "/v1/images/generations"
	models := r.URL.Path == "/v1/models"
	if r.URL.Path != "/v1/responses" && !image && !models {
		writeError(w, http.StatusNotFound, "not_found", "Unknown API route.")
		return
	}
	method := http.MethodPost
	if models {
		method = http.MethodGet
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use "+method+".")
		return
	}
	if !matchesKey(r.Header.Get("Authorization"), p.key) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "invalid_proxy_key", "Supply the proxy's bearer token.")
		return
	}

	if models {
		p.models.serve(w, r, p.routes.accounts)
		return
	}

	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !(edit && contentType == "multipart/form-data" && params["boundary"] != "")) {
		message := "Use application/json."
		if edit {
			message = "Use multipart/form-data with a boundary, or application/json."
		}
		writeError(w, http.StatusUnsupportedMediaType, "invalid_content_type", message)
		return
	}

	// Limit time spent uploading a client request without timing out model streams.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	_ = controller.SetReadDeadline(time.Time{})
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 8 MiB.")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not read the request body.")
		return
	}
	var payload []byte
	var stream bool
	endpoint, accept := p.endpoint, "text/event-stream"
	if edit {
		payload, err = editImagePayload(body, params["boundary"], contentType == "multipart/form-data")
		endpoint, accept = p.editEndpoint, "application/json"
	} else if image {
		payload, err = imagePayload(body)
		endpoint, accept = p.imageEndpoint, "application/json"
	} else {
		payload, stream, err = codexPayload(body)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	key := requestRouteKey(r.Header, payload)
	var preferred map[string]bool
	if !image && p.models != nil {
		preferred = p.models.preferred(p.routes.accounts, key.model)
	}
	resp, selected, failure := p.send(r.Context(), key, endpoint, accept, payload, preferred)
	if failure != nil {
		writeError(w, failure.status, failure.code, failure.message)
		return
	}
	defer resp.Body.Close()
	contentType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if value := resp.Header.Get("Retry-After"); value != "" {
		w.Header().Set("Retry-After", value)
	}
	if resp.StatusCode >= 400 {
		writeUpstreamError(w, resp, contentType)
		return
	}
	if resp.StatusCode != http.StatusOK || contentType != accept {
		writeError(w, http.StatusBadGateway, "upstream_error", "The Codex backend returned an unexpected response.")
		return
	}
	if image {
		writeImageResponse(w, resp.Body, func(usage json.RawMessage) {
			p.routes.confirm(key, selected)
			p.routes.observe(selected.account, key.model, usage)
		})
		return
	}
	p.writeResponses(r.Context(), w, resp.Body, stream, selected, key)
}

func matchesKey(header, key string) bool {
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got, want := sha256.Sum256([]byte(value)), sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func codexPayload(body []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, false, errors.New("Expected a JSON object.")
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" || len(model) > 128 {
		return nil, false, errors.New("A model name of at most 128 bytes is required.")
	}
	var stream bool
	if raw, ok := fields["stream"]; ok && json.Unmarshal(raw, &stream) != nil {
		return nil, false, errors.New("stream must be a boolean.")
	}
	for _, name := range []string{"previous_response_id", "conversation"} {
		if raw := fields[name]; len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
			return nil, false, errors.New(name + " is not supported; send the full input history.")
		}
		delete(fields, name)
	}
	if raw := fields["background"]; len(raw) != 0 && !bytes.Equal(raw, []byte("false")) && !bytes.Equal(raw, []byte("null")) {
		return nil, false, errors.New("Background responses are not supported.")
	}
	delete(fields, "background")

	input := fields["input"]
	switch {
	case len(input) != 0 && input[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, false, err
		}
		for i, item := range items {
			var message map[string]json.RawMessage
			if json.Unmarshal(item, &message) == nil && bytes.Equal(message["role"], []byte(`"system"`)) {
				message["role"] = json.RawMessage(`"developer"`)
				items[i], _ = json.Marshal(message)
			}
		}
		fields["input"], _ = json.Marshal(items)
	case len(input) != 0 && input[0] == '"':
		fields["input"], _ = json.Marshal([]any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": input}},
		}})
	default:
		return nil, false, errors.New("input must be a string or an array.")
	}

	for _, name := range []string{"temperature", "top_p", "user", "context_management", "truncation", "prompt_cache_options", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		delete(fields, name)
	}
	if err := setCodexIncludes(fields); err != nil {
		return nil, false, err
	}
	for name, value := range map[string]json.RawMessage{
		"instructions":        json.RawMessage(`""`),
		"tools":               json.RawMessage(`[]`),
		"tool_choice":         json.RawMessage(`"auto"`),
		"parallel_tool_calls": json.RawMessage(`true`),
	} {
		if raw := fields[name]; len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			fields[name] = value
		}
	}
	fields["stream"], fields["store"] = json.RawMessage("true"), json.RawMessage("false")
	payload, err := json.Marshal(fields)
	return payload, stream, err
}

func setCodexIncludes(fields map[string]json.RawMessage) error {
	var requested []string
	if raw := fields["include"]; len(raw) > 0 && json.Unmarshal(raw, &requested) != nil {
		return errors.New("include must be an array of strings.")
	}
	includes := []string{"reasoning.encrypted_content"}
	for _, value := range []string{"web_search_call.action.sources", "web_search_call.results", "output.encrypted_content"} {
		for _, entry := range requested {
			if entry == value {
				includes = append(includes, value)
				break
			}
		}
	}
	fields["include"], _ = json.Marshal(includes)
	return nil
}

func requestRouteKey(headers http.Header, payload []byte) routeKey {
	var body struct {
		Model string          `json:"model"`
		Cache json.RawMessage `json:"prompt_cache_key"`
	}
	_ = json.Unmarshal(payload, &body)
	var session string
	for _, name := range []string{"X-Session-ID", "Session-Id", "Session_id"} {
		if session = strings.TrimSpace(headers.Get(name)); session != "" {
			break
		}
	}
	if session == "" {
		_ = json.Unmarshal(body.Cache, &session)
	}
	return newRouteKey(body.Model, session)
}

func (p proxy) send(ctx context.Context, key routeKey, endpoint, accept string, payload []byte, preferred map[string]bool) (*http.Response, routeAttempt, *responseError) {
	var last *http.Response
	var selected routeAttempt
	tried := make(map[string]bool)
	for {
		a, ok := p.routes.pick(key, tried, preferred)
		if !ok {
			if last != nil {
				return last, selected, nil
			}
			return nil, routeAttempt{}, &responseError{http.StatusServiceUnavailable, "no_account_available", "No ChatGPT account is available. Check usage or renew the login."}
		}
		tried[a.id] = true
		auth, err := readCredentials(a.path)
		if err != nil {
			p.routes.reject(a.account, key.model, http.StatusUnauthorized, "", nil)
			continue
		}
		if last != nil {
			last.Body.Close()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, routeAttempt{}, upstreamError("Could not build the Codex request.")
		}
		setCodexHeaders(req.Header, auth)
		if accept == "application/json" {
			// The direct subscription image route expects the Codex originator,
			// not an arbitrary User-Agent supplied by the downstream SDK.
			setCodexClientProfile(req.Header)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", accept)
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, routeAttempt{}, upstreamError("Could not reach the Codex backend.")
		}
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusTooManyRequests {
			return resp, a, nil
		}
		// Only retry explicit account rejections, never a started stream,
		// ambiguous network failure, or successful image request.
		var quota []byte
		if resp.StatusCode == http.StatusTooManyRequests && len(p.routes.accounts) > 1 {
			// Preserve the original JSON for the caller if there are no alternatives.
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamError+1))
			resp.Body.Close()
			if err != nil {
				body = nil
			}
			if len(body) <= maxUpstreamError {
				quota = body
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
		p.routes.reject(a.account, key.model, resp.StatusCode, resp.Header.Get("Retry-After"), quota)
		last, selected = resp, a
	}
}

const maxUpstreamError = 1 << 20

func writeUpstreamError(w http.ResponseWriter, resp *http.Response, contentType string) {
	// Do not forward HTML error pages or response headers such as Set-Cookie.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamError+1))
	if err == nil && len(body) <= maxUpstreamError && contentType == "application/json" && json.Valid(body) {
		writeJSON(w, resp.StatusCode, body)
	} else {
		writeError(w, resp.StatusCode, "upstream_error", "The Codex backend rejected the request.")
	}
}

func (p proxy) writeResponses(ctx context.Context, w http.ResponseWriter, upstream io.Reader, stream bool, a routeAttempt, key routeKey) {
	observe := func(event codexEvent, raw []byte) {
		switch event.Type {
		case "response.completed", "response.incomplete":
			p.routes.confirm(key, a)
			p.routes.observe(a.account, key.model, event.Response["usage"])
		default:
			failure := event.failure()
			if failure.status == http.StatusUnauthorized || failure.status == http.StatusTooManyRequests {
				p.routes.reject(a.account, key.model, failure.status, "", raw)
			}
		}
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(w)
		_ = controller.Flush()
		writer := flushingWriter{w, controller}
		_, failure := readResponses(upstream, func(name string, payload []byte) bool {
			return writeSSE(writer, name, payload)
		}, observe)
		if failure != nil && ctx.Err() == nil {
			payload, _ := json.Marshal(map[string]any{
				"type": "error", "code": failure.code, "message": failure.message, "param": nil,
			})
			writeSSE(writer, "", payload)
		}
		return
	}

	done, failure := readResponses(upstream, nil, observe)
	if failure != nil {
		writeError(w, failure.status, failure.code, failure.message)
		return
	}
	writeJSON(w, http.StatusOK, done)
}

func setCodexHeaders(headers http.Header, auth credentials) {
	headers.Set("Authorization", "Bearer "+auth.AccessToken)
	headers.Set("ChatGPT-Account-Id", auth.AccountID)
	headers.Set("User-Agent", "chatgpt-proxy")
	headers.Set("Originator", "chatgpt-proxy")
}

func setCodexClientProfile(headers http.Header) {
	headers.Set("Originator", "codex-tui")
	headers.Set("User-Agent", "codex-tui ("+runtime.GOOS+"; "+runtime.GOARCH+") chatgpt-proxy")
}

type flushingWriter struct {
	w          io.Writer
	controller *http.ResponseController
}

func (f flushingWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if err == nil {
		err = f.controller.Flush()
	}
	return n, err
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"type": code, "code": code, "message": message},
	})
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
