package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

type responseError struct {
	status        int
	code, message string
}

func upstreamError(message string) *responseError {
	return &responseError{http.StatusBadGateway, "upstream_error", message}
}

type codexEvent struct {
	Type     string                     `json:"type"`
	Code     string                     `json:"code"`
	Status   json.RawMessage            `json:"status"`
	Error    json.RawMessage            `json:"error"`
	Response map[string]json.RawMessage `json:"response"`
	Item     json.RawMessage            `json:"item"`
	Index    *int                       `json:"output_index"`
}

func (e codexEvent) failure() *responseError {
	for _, raw := range []json.RawMessage{e.Error, e.Response["error"]} {
		var detail struct{ Code, Type string }
		_ = json.Unmarshal(raw, &detail)
		for _, code := range []string{detail.Code, detail.Type, e.Code} {
			switch code {
			case "context_length_exceeded", "context_too_large":
				return &responseError{http.StatusBadRequest, "context_length_exceeded", "The input exceeds the model's context window."}
			case "rate_limit_error", "rate_limit_exceeded", "usage_limit_reached", "insufficient_quota":
				return &responseError{http.StatusTooManyRequests, code, "The Codex account reached a usage limit."}
			}
		}
	}
	var status int
	_ = json.Unmarshal(e.Status, &status)
	switch status {
	case http.StatusUnauthorized:
		return &responseError{status, "codex_login_required", "The Codex backend rejected the login."}
	case http.StatusPaymentRequired:
		return &responseError{status, "codex_payment_required", "The Codex backend requires payment or an eligible plan."}
	case http.StatusTooManyRequests:
		return &responseError{status, "rate_limit_exceeded", "The Codex account reached a usage limit."}
	}
	return upstreamError("Codex reported a failed response.")
}

type outputItems struct {
	indexed   map[int]json.RawMessage
	unindexed []json.RawMessage
}

func (items *outputItems) add(e codexEvent) {
	if len(e.Item) == 0 || e.Item[0] != '{' {
		return
	}
	if e.Index != nil && *e.Index >= 0 {
		items.indexed[*e.Index] = e.Item
	} else {
		items.unindexed = append(items.unindexed, e.Item)
	}
}

// repair fills omitted output without replacing output that Codex already included.
func (items outputItems) repair(response map[string]json.RawMessage) (bool, *responseError) {
	if response == nil {
		return false, upstreamError("Codex terminal event did not contain a response.")
	}
	var output []json.RawMessage
	if raw := response["output"]; raw != nil && json.Unmarshal(raw, &output) != nil {
		return false, upstreamError("Invalid Codex response output.")
	}
	if len(output) > 0 || bytes.Equal(response["output"], []byte("[]")) && len(items.indexed)+len(items.unindexed) == 0 {
		return false, nil
	}
	indexes := make([]int, 0, len(items.indexed))
	for idx := range items.indexed {
		indexes = append(indexes, idx)
	}
	slices.Sort(indexes)
	output = make([]json.RawMessage, 0, len(indexes)+len(items.unindexed))
	for _, idx := range indexes {
		output = append(output, items.indexed[idx])
	}
	output = append(output, items.unindexed...)
	response["output"], _ = json.Marshal(output)
	return true, nil
}

// readResponses shares SSE parsing and terminal handling between JSON and streaming clients.
// emit is nil in JSON mode. A non-nil error after streaming starts becomes an SSE error event.
func readResponses(r io.Reader, emit func(string, []byte) bool, observe func(codexEvent, []byte)) ([]byte, *responseError) {
	const maxStream = 64 << 20
	const maxEvent = 16 << 20
	scan := bufio.NewScanner(io.LimitReader(r, maxStream))
	scan.Split(splitSSELine)
	scan.Buffer(make([]byte, 4096), maxEvent)
	var data bytes.Buffer
	var name string
	items := outputItems{indexed: map[int]json.RawMessage{}}
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) != 0 {
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "data":
				if data.Len()+len(value)+1 > maxEvent {
					return nil, upstreamError("Codex event is too large.")
				}
				data.Write(value)
				data.WriteByte('\n')
			case "event":
				name = string(value)
				continue
			default:
				continue
			}
			// Codex sometimes omits the blank separator, even at EOF. Dispatch
			// complete JSON immediately; accumulate fragments of split frames.
			if payload := bytes.TrimSpace(data.Bytes()); !bytes.Equal(payload, []byte("[DONE]")) &&
				(!bytes.HasSuffix(payload, []byte("}")) || !json.Valid(payload)) {
				continue
			}
		}

		eventName := name
		name = ""
		payload := bytes.TrimSpace(data.Bytes())
		if len(payload) == 0 {
			data.Reset()
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var event codexEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, upstreamError("Invalid Codex event.")
		}
		if event.Type == "" {
			failure := event.failure()
			if failure.status != http.StatusBadGateway || len(event.Error) != 0 && !bytes.Equal(event.Error, []byte("null")) {
				if observe != nil {
					observe(event, payload)
				}
				return nil, failure
			}
			return nil, upstreamError("Invalid Codex event.")
		}
		terminal := false
		switch event.Type {
		case "response.output_item.done":
			items.add(event)
		case "response.completed", "response.incomplete":
			changed, failure := items.repair(event.Response)
			if failure != nil {
				return nil, failure
			}
			if observe != nil {
				observe(event, payload)
			}
			if emit == nil {
				response, _ := json.Marshal(event.Response)
				return response, nil
			}
			if changed {
				var envelope map[string]json.RawMessage
				_ = json.Unmarshal(payload, &envelope)
				envelope["response"], _ = json.Marshal(event.Response)
				payload, _ = json.Marshal(envelope)
			}
			terminal = true
		case "error", "response.failed":
			if observe != nil {
				observe(event, payload)
			}
			if emit == nil || event.Type == "error" {
				return nil, event.failure()
			}
			terminal = true
		}
		if emit != nil && !strings.HasPrefix(event.Type, "codex.") && event.Type != "response.metadata" && event.Type != "keepalive" && !emit(eventName, payload) {
			return nil, nil // The client stopped reading.
		}
		if terminal {
			return nil, nil
		}
		data.Reset()
	}
	if scan.Err() != nil {
		return nil, upstreamError("Could not read the Codex event stream.")
	}
	return nil, upstreamError("Codex event stream ended before a terminal response.")
}

func splitSSELine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		end := i + 1
		if data[i] == '\r' {
			if end == len(data) && !atEOF {
				return 0, nil, nil // Wait to see whether this is CRLF.
			}
			if end < len(data) && data[end] == '\n' {
				end++
			}
		}
		return end, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func writeSSE(w io.Writer, name string, payload []byte) bool {
	var frame bytes.Buffer
	if name != "" {
		frame.WriteString("event: ")
		frame.WriteString(name)
		frame.WriteByte('\n')
	}
	for line := range bytes.SplitSeq(payload, []byte("\n")) {
		frame.WriteString("data: ")
		frame.Write(line)
		frame.WriteByte('\n')
	}
	frame.WriteByte('\n')
	_, err := w.Write(frame.Bytes())
	return err == nil
}
