package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func imageRequest(body string) *http.Request {
	r := request(body)
	r.URL.Path = "/v1/images/generations"
	return r
}

func TestImageGenerationUsesCodexSubscription(t *testing.T) {
	const image = `{"created":123,"data":[{"b64_json":"aGVsbG8="}],"usage":{"input_tokens":256,"input_tokens_details":{"cached_tokens":128}}}`
	p := withTwoAccounts(t, testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/images/generations" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Originator") != "codex-tui" || !strings.HasPrefix(r.UserAgent(), "codex-tui ") {
			t.Errorf("unexpected upstream request: %s, %v", r.URL.Path, r.Header)
		}
		if r.Header.Get("Authorization") == "Bearer token-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-b" || r.Header.Get("ChatGPT-Account-Id") != "workspace" {
			t.Error("wrong upstream credentials")
		}
		var fields map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		if string(fields["model"]) != `"gpt-image-2"` || string(fields["prompt"]) != `"a blue square"` || string(fields["quality"]) != `"low"` || fields["stream"] != nil || fields["user"] != nil {
			t.Errorf("unexpected upstream body: %v", fields)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Set-Cookie", "private")
		fmt.Fprint(w, image)
	}), cacheAware)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, imageRequest(`{"prompt":"a blue square","stream":false,"quality":"low","user":"ignored"}`))
	if rec.Code != 200 || rec.Body.String() != image || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unexpected image response: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	score := p.routes.scores[cacheKey{p.routes.accounts[1].id, defaultImageModel}]
	if score.samples != 1 || score.ratio() != 0.5 {
		t.Fatalf("image cache metrics not observed: %+v", score)
	}
}

func TestImageValidation(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request sent upstream") })
	for _, body := range []string{`{}`, `{"prompt":"image","stream":true}`, `{"prompt":"image","model":"other"}`} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, imageRequest(body))
		if rec.Code != 400 {
			t.Errorf("status %d for %s: %s", rec.Code, body, rec.Body.String())
		}
	}
	payload, err := imagePayload([]byte(`{"prompt":"image","model":"gpt-image-1.5"}`))
	if err != nil || !strings.Contains(string(payload), `"model":"gpt-image-1.5"`) {
		t.Fatalf("explicit image model not preserved: %s; %v", payload, err)
	}
}

func TestInvalidImageEncodingAndURL(t *testing.T) {
	if validImageResult(strings.Repeat("YWFh", 255)+"YQ==YWFh", "") {
		t.Error("accepted encoded data after padding")
	}
	if validImageResult("", "https://example.test/image?sig=%zz") {
		t.Error("accepted a URL with an invalid query escape")
	}
}

func TestImageRejectsUnexpectedSuccessBody(t *testing.T) {
	for _, body := range []string{`{"message":"not an image response"}`, `{"data":[null]}`, `{"data":[{}]}`, `{"data":[{"url":"not a URL"}]}`, `{"data":[{"b64_json":"invalid!!!"}]}`} {
		p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, imageRequest(`{"prompt":"a blue square"}`))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "invalid_image_response") {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
}
