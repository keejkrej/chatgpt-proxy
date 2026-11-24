package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const defaultImageModel = "gpt-image-2"

func imagePayload(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("Expected a JSON object.")
	}
	return normalizedImagePayload(fields)
}

func normalizedImagePayload(fields map[string]json.RawMessage) ([]byte, error) {
	var prompt string
	if json.Unmarshal(fields["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("A prompt is required.")
	}
	var model string
	if raw := fields["model"]; raw == nil || bytes.Equal(raw, []byte("null")) {
		model = defaultImageModel
	} else if json.Unmarshal(raw, &model) != nil || !strings.HasPrefix(model, "gpt-image-") || len(model) > 128 {
		return nil, errors.New("Use a gpt-image model.")
	}
	if raw := fields["stream"]; raw != nil && !bytes.Equal(raw, []byte("false")) && !bytes.Equal(raw, []byte("null")) {
		return nil, errors.New("Streaming image generation is not supported.")
	}
	delete(fields, "stream")
	delete(fields, "user")
	fields["model"], _ = json.Marshal(model)
	return json.Marshal(fields)
}

type imageReference struct {
	URL    string `json:"image_url,omitempty"`
	FileID string `json:"file_id,omitempty"`
}

func editImagePayload(body []byte, boundary string, isMultipart bool) ([]byte, error) {
	var fields map[string]json.RawMessage
	if isMultipart {
		var err error
		fields, err = editMultipartFields(body, boundary)
		if err != nil {
			return nil, err
		}
	} else if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, errors.New("Expected a JSON object.")
	}
	var images []imageReference
	if json.Unmarshal(fields["images"], &images) != nil || len(images) != 1 || !images[0].valid() {
		return nil, errors.New("Supply exactly one image in images, with an image_url or file_id.")
	}
	if raw, ok := fields["mask"]; ok && !bytes.Equal(raw, []byte("null")) {
		var mask imageReference
		if json.Unmarshal(raw, &mask) != nil || !mask.valid() {
			return nil, errors.New("Supply a mask with an image_url or file_id.")
		}
	}
	return normalizedImagePayload(fields)
}

func (ref imageReference) valid() bool {
	if ref.URL == "" {
		return strings.TrimSpace(ref.FileID) != ""
	}
	if ref.FileID != "" {
		return false
	}
	for _, mime := range []string{"image/png", "image/jpeg", "image/webp"} {
		if encoded, ok := strings.CutPrefix(ref.URL, "data:"+mime+";base64,"); ok {
			return encoded != "" && validImageResult(encoded, "")
		}
	}
	return validImageResult("", ref.URL)
}

func editMultipartFields(body []byte, boundary string) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage)
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("Invalid multipart upload.")
		}
		name := part.FormName()
		if name == "image" || name == "image[]" {
			name = "images"
		}
		if name == "" || fields[name] != nil {
			return nil, errors.New("Each multipart field may be supplied only once.")
		}
		data, err := io.ReadAll(part) // The entire request is already bounded by MaxBytesReader.
		if err != nil {
			return nil, errors.New("Could not read multipart field.")
		}
		var value any = string(data)
		switch name {
		case "images", "mask":
			kind := http.DetectContentType(data)
			if part.FileName() == "" || (kind != "image/png" && kind != "image/jpeg" && kind != "image/webp") {
				return nil, errors.New("Upload a PNG, JPEG, or WebP file for image or mask.")
			}
			ref := imageReference{URL: "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(data)}
			if name == "images" {
				value = []imageReference{ref}
			} else {
				value = ref
			}
		case "n", "output_compression", "partial_images":
			n, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return nil, errors.New(name + " must be an integer.")
			}
			value = n
		case "stream":
			switch string(data) {
			case "true", "false":
				value = string(data) == "true"
			default:
				return nil, errors.New("stream must be a boolean.")
			}
		default:
			if part.FileName() != "" {
				return nil, errors.New("Unexpected multipart file field.")
			}
		}
		fields[name], _ = json.Marshal(value)
	}
	return fields, nil
}

func writeImageResponse(w http.ResponseWriter, r io.Reader, completed func(json.RawMessage)) {
	const maxImageResponse = 64 << 20
	body, err := io.ReadAll(io.LimitReader(r, maxImageResponse+1))
	var response struct {
		Usage json.RawMessage `json:"usage"`
		Data  []struct {
			Base64 string `json:"b64_json"`
			URL    string `json:"url"`
		} `json:"data"`
	}
	if err != nil || len(body) > maxImageResponse || json.Unmarshal(body, &response) != nil || len(response.Data) == 0 {
		writeError(w, http.StatusBadGateway, "invalid_image_response", "The Codex backend returned an invalid or oversized image response.")
		return
	}
	for _, image := range response.Data {
		if !validImageResult(image.Base64, image.URL) {
			writeError(w, http.StatusBadGateway, "invalid_image_response", "The Codex backend returned an invalid image result.")
			return
		}
	}
	completed(response.Usage)
	writeJSON(w, http.StatusOK, body)
}

func validImageResult(encoded, address string) bool {
	if encoded != "" {
		// A streaming decoder can stop at padding when it falls on a chunk boundary.
		if i := strings.IndexByte(encoded, '='); (i >= 0 && encoded[i:] != "=" && encoded[i:] != "==") || strings.ContainsAny(encoded, "\r\n") {
			return false
		}
		n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded)))
		return n > 0 && err == nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	_, err = url.QueryUnescape(u.RawQuery)
	return err == nil
}
