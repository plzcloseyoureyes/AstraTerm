package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// openaiProvider implements the OpenAI-compatible chat/completions streaming API (OpenAI, Ollama, LM Studio, vLLM,
// llama.cpp server, Gemini's OpenAI endpoint, …). The API key is optional (local servers).
type openaiProvider struct {
	client  *http.Client
	baseURL string
	apiKey  string
	label   string
}

func (p *openaiProvider) headers() map[string]string {
	h := map[string]string{"Accept": "text/event-stream"}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	return h
}

func (p *openaiProvider) parseErr(status int, b []byte) *ProviderError {
	pe := &ProviderError{Status: status, Label: p.label}
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &body) == nil && len(body.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		var str string
		switch {
		case json.Unmarshal(body.Error, &obj) == nil && obj.Message != "":
			pe.Message, pe.Kind = obj.Message, obj.Type
		case json.Unmarshal(body.Error, &str) == nil && str != "":
			pe.Message = str // Ollama: {"error":"model 'x' not found"}
		}
	}
	if pe.Message == "" {
		txt := strings.TrimSpace(string(b))
		if txt != "" && len(txt) < 300 && !strings.HasPrefix(txt, "<") {
			pe.Message = txt
		} else {
			pe.Message = fmt.Sprintf("HTTP %d", status)
		}
	}
	if status == 401 {
		pe.Message = "the API key was rejected (" + pe.Message + ")"
	}
	return pe
}

func (p *openaiProvider) Open(ctx context.Context, req Request) (Stream, error) {
	msgs := make([]map[string]string, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": req.Model, "messages": msgs, "stream": true}
	if req.MaxTokens > 0 && !strings.Contains(p.baseURL, "api.openai.com") {
		// OpenAI's newer models reject max_tokens (max_completion_tokens instead); other servers expect max_tokens.
		body["max_tokens"] = req.MaxTokens
	}
	sctx, cancel := context.WithCancel(ctx)
	res, err := doRequest(sctx, p.client, http.MethodPost, p.baseURL+"/chat/completions", p.headers(), body, p.label, p.parseErr)
	if err != nil {
		cancel()
		return nil, err
	}
	rd := newIdleReader(res.Body, cancel, idleReadTimeout)
	return &openaiStream{body: rd, sse: newSSEReader(rd), label: p.label}, nil
}

type openaiStream struct {
	body   io.ReadCloser
	sse    *sseReader
	label  string
	finish string
	usage  *Usage
	done   bool
	gotAny bool
}

func (s *openaiStream) Close() error { return s.body.Close() }

func (s *openaiStream) end() (Event, error) {
	s.done = true
	return Event{Type: "done", StopReason: s.finish, Usage: s.usage}, nil
}

func (s *openaiStream) Next() (Event, error) {
	if s.done {
		return Event{}, io.EOF
	}
	for {
		ev, err := s.sse.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if s.finish != "" {
					return s.end() // some servers close without [DONE]
				}
				return Event{}, &ProviderError{Label: s.label, Message: "the response stream ended unexpectedly"}
			}
			return Event{}, err
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return s.end()
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			pe := (&openaiProvider{label: s.label}).parseErr(500, []byte(`{"error":`+string(chunk.Error)+`}`))
			s.done = true
			return Event{}, pe
		}
		if chunk.Usage != nil {
			s.usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		var text string
		reasoning := false
		for _, c := range chunk.Choices {
			text += c.Delta.Content
			if c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" {
				reasoning = true
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				s.finish = normalizeFinish(*c.FinishReason)
			}
		}
		if text != "" {
			s.gotAny = true
			return Event{Type: "text", Text: text}, nil
		}
		if reasoning && !s.gotAny {
			return Event{Type: "thinking"}, nil
		}
	}
}

// normalizeFinish maps OpenAI finish reasons to the Anthropic-style stop reasons the UI understands.
func normalizeFinish(r string) string {
	switch r {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
	return r
}

func (p *openaiProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	h := map[string]string{}
	if p.apiKey != "" {
		h["Authorization"] = "Bearer " + p.apiKey
	}
	res, err := doRequest(ctx, p.client, http.MethodGet, p.baseURL+"/models", h, nil, p.label, p.parseErr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&body); err != nil {
		return nil, &ProviderError{Label: p.label, Status: 502, Message: "unreadable model list"}
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" && len(m.ID) <= 128 {
			out = append(out, ModelInfo{ID: m.ID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 500 {
		out = out[:500]
	}
	return out, nil
}
