package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicVersion is the Messages API version header value.
const anthropicVersion = "2023-06-01"

// anthropicProvider implements the Anthropic Messages API over raw HTTPS + SSE.
type anthropicProvider struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

func (p *anthropicProvider) headers() map[string]string {
	return map[string]string{"x-api-key": p.apiKey, "anthropic-version": anthropicVersion, "Accept": "text/event-stream"}
}

func (p *anthropicProvider) parseErr(status int, b []byte) *ProviderError {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	pe := &ProviderError{Status: status, Label: "Anthropic"}
	if json.Unmarshal(b, &body) == nil && body.Error.Message != "" {
		pe.Kind, pe.Message = body.Error.Type, body.Error.Message
	} else {
		pe.Message = fmt.Sprintf("HTTP %d", status)
	}
	if status == 401 {
		pe.Message = "the API key was rejected (" + pe.Message + ")"
	}
	return pe
}

// supportsEffort reports whether a model accepts output_config.effort (Claude 4.6+ generation; not Haiku 4.5).
func supportsEffort(model string) bool {
	for _, p := range []string{"claude-sonnet-5", "claude-opus-5", "claude-fable-5", "claude-mythos-5", "claude-opus-4-6",
		"claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (p *anthropicProvider) Open(ctx context.Context, req Request) (Stream, error) {
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": []anthropicContent{{Type: "text", Text: m.Content}}})
	}
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   msgs,
		"stream":     true,
	}
	if req.System != "" {
		body["system"] = []anthropicContent{{Type: "text", Text: req.System}}
	}
	if req.Effort != "" && supportsEffort(req.Model) {
		body["output_config"] = map[string]any{"effort": req.Effort}
	}
	sctx, cancel := context.WithCancel(ctx)
	res, err := doRequest(sctx, p.client, http.MethodPost, p.baseURL+"/v1/messages", p.headers(), body, "Anthropic", p.parseErr)
	if err != nil {
		cancel()
		return nil, err
	}
	body2 := newIdleReader(res.Body, cancel, idleReadTimeout)
	return &anthropicStream{body: body2, sse: newSSEReader(body2)}, nil
}

type anthropicStream struct {
	body       io.ReadCloser
	sse        *sseReader
	usage      Usage
	stopReason string
	done       bool
}

func (s *anthropicStream) Close() error { return s.body.Close() }

func (s *anthropicStream) Next() (Event, error) {
	if s.done {
		return Event{}, io.EOF
	}
	for {
		ev, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The stream ended without message_stop (connection dropped mid-answer).
				return Event{}, &ProviderError{Label: "Anthropic", Status: 0, Message: "the response stream ended unexpectedly"}
			}
			return Event{}, err
		}
		var payload struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if ev.Data == "" {
			continue
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			continue // tolerate unknown / malformed events
		}
		typ := payload.Type
		if typ == "" {
			typ = ev.Event
		}
		switch typ {
		case "message_start":
			s.usage.InputTokens = payload.Message.Usage.InputTokens
			s.usage.OutputTokens = payload.Message.Usage.OutputTokens
		case "content_block_start":
			switch payload.ContentBlock.Type {
			case "thinking", "redacted_thinking":
				return Event{Type: "thinking"}, nil
			case "text":
				if payload.ContentBlock.Text != "" {
					return Event{Type: "text", Text: payload.ContentBlock.Text}, nil
				}
			}
		case "content_block_delta":
			switch payload.Delta.Type {
			case "text_delta":
				if payload.Delta.Text != "" {
					return Event{Type: "text", Text: payload.Delta.Text}, nil
				}
			case "thinking_delta":
				return Event{Type: "thinking"}, nil
			}
		case "message_delta":
			if payload.Delta.StopReason != "" {
				s.stopReason = payload.Delta.StopReason
			}
			if payload.Usage.OutputTokens > 0 {
				s.usage.OutputTokens = payload.Usage.OutputTokens
			}
		case "message_stop":
			s.done = true
			u := s.usage
			return Event{Type: "done", StopReason: s.stopReason, Usage: &u}, nil
		case "error":
			s.done = true
			return Event{}, &ProviderError{Label: "Anthropic", Status: 500, Kind: payload.Error.Type, Message: nonEmpty(payload.Error.Message, "stream error")}
		}
	}
}

func (p *anthropicProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	res, err := doRequest(ctx, p.client, http.MethodGet, p.baseURL+"/v1/models?limit=100",
		map[string]string{"x-api-key": p.apiKey, "anthropic-version": anthropicVersion}, nil, "Anthropic", p.parseErr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return nil, &ProviderError{Label: "Anthropic", Status: 502, Message: "unreadable model list"}
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, ModelInfo{ID: m.ID, Label: m.DisplayName})
		}
	}
	return out, nil
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
