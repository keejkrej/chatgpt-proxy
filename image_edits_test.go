package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageEditMultipart(t *testing.T) {
	// PNG signature is used for upload media type detection; upstream validates the image itself.
	png := []byte("\x89PNG\r\n\x1a\n")
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/images/edits" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer subscription-token" {
			t.Error("multipart edit did not use the direct Codex JSON route")
		}
		var fields struct {
			Model       string           `json:"model"`
			Prompt      string           `json:"prompt"`
			Images      []imageReference `json:"images"`
			Mask        imageReference   `json:"mask"`
			Compression int              `json:"output_compression"`
			Stream      *bool            `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		ref := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		if fields.Model != defaultImageModel || fields.Prompt != "change the background" || len(fields.Images) != 1 || fields.Images[0].URL != ref || fields.Mask.URL != ref || fields.Compression != 80 || fields.Stream != nil {
			t.Error("unexpected image edit payload")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
	})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, name := range []string{"image[]", "mask"} {
		part, err := form.CreateFormFile(name, "input.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(png); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"prompt": "change the background", "output_compression": "80", "stream": "false"} {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := imageRequest(body.String())
	req.URL.Path = "/v1/images/edits"
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "YQ==") {
		t.Fatalf("edit returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestImageEditJSONValidation(t *testing.T) {
	p := testProxy(t, func(w http.ResponseWriter, r *http.Request) {
		var fields map[string]json.RawMessage
		if r.URL.Path != "/backend-api/codex/images/edits" || json.NewDecoder(r.Body).Decode(&fields) != nil || string(fields["images"]) != `[{"file_id":"file-abc"}]` {
			t.Error("incorrect JSON image edit")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"b64_json":"YQ=="}]}`)
	})
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"prompt":"edit","images":[{"file_id":"file-abc"}],"mask":{"image_url":"https://example.test/mask.png"}}`, 200},
		{`{"prompt":"edit","images":[]}`, 400},
		{`{"prompt":"edit","images":[{"file_id":"file-one"},{"file_id":"file-two"}]}`, 400},
		{`{"prompt":"edit","images":[{"image_url":"file:///secret"}]}`, 400},
		{`{"prompt":"edit","images":[{"file_id":"file-abc"}],"mask":{}}`, 400},
		{`{"prompt":"edit","images":[{"file_id":"file-abc"}],"stream":true}`, 400},
	} {
		req := imageRequest(tc.body)
		req.URL.Path = "/v1/images/edits"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("got %d, want %d for %s: %s", rec.Code, tc.status, tc.body, rec.Body.String())
		}
	}
}
