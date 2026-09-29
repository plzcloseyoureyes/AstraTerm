package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// FakeProvider is a local httptest server speaking the Anthropic Messages API or the OpenAI chat/completions API with
// SSE streaming. It records every request so tests can assert on headers and on what would have left Termstead. No
// real (paid) API is ever contacted.
type FakeProvider struct {
	*httptest.Server
	Kind string // anthropic | openai

	mu       sync.Mutex
	Requests []FakeRequest
	// Reply is the text streamed back (split into several deltas).
	Reply string
	// Status, when non-zero, makes the next requests fail with this HTTP status.
	Status int
	// Thinking emits a thinking block before the text (Anthropic) / reasoning deltas (OpenAI).
	Thinking bool
	// Drop ends the stream without a terminal event.
	Drop bool
	// Hang blocks after the first delta until the client goes away.
	Hang bool
}

// FakeRequest is one recorded request.
type FakeRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    string
}

// NewFakeProvider starts a fake provider of kind (anthropic | openai).
func NewFakeProvider(t testing.TB, kind string) *FakeProvider {
	f := &FakeProvider{Kind: kind, Reply: "Hello from the fake provider."}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Last returns the last recorded request.
func (f *FakeProvider) Last() FakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Requests) == 0 {
		return FakeRequest{}
	}
	return f.Requests[len(f.Requests)-1]
}

// Count returns the number of recorded requests.
func (f *FakeProvider) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

// Set changes the behaviour under the lock.
func (f *FakeProvider) Set(fn func(f *FakeProvider)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *FakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.Requests = append(f.Requests, FakeRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Raw: string(raw)})
	status, reply, thinking, drop, hang := f.Status, f.Reply, f.Thinking, f.Drop, f.Hang
	f.mu.Unlock()

	if strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		if f.Kind == "anthropic" {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"},{"id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama3.2"},{"id":"qwen2.5-coder"}]}`)
		}
		return
	}
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if f.Kind == "anthropic" {
			fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		} else {
			fmt.Fprintf(w, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	chunks := splitReply(reply)
	if f.Kind == "anthropic" {
		send := func(ev, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, data)
			fl.Flush()
		}
		send("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":42,"output_tokens":1}}}`)
		send("ping", `{"type":"ping"}`)
		idx := 0
		if thinking {
			send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
			send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
			send("content_block_stop", `{"type":"content_block_stop","index":0}`)
			idx = 1
		}
		send("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, idx))
		for i, c := range chunks {
			b, _ := json.Marshal(c)
			send("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, idx, b))
			if hang && i == 0 {
				<-r.Context().Done()
				return
			}
		}
		if drop {
			return
		}
		send("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, idx))
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":17}}`)
		send("message_stop", `{"type":"message_stop"}`)
		return
	}
	send := func(data string) {
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
	}
	if thinking {
		send(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hmm"}}]}`)
	}
	for i, c := range chunks {
		b, _ := json.Marshal(c)
		send(fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, b))
		if hang && i == 0 {
			<-r.Context().Done()
			return
		}
	}
	if drop {
		return
	}
	send(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":12}}`)
	send("[DONE]")
}

func splitReply(s string) []string {
	var out []string
	for len(s) > 0 {
		n := 7
		if n > len(s) {
			n = len(s)
		}
		for n < len(s) && (s[n]&0xC0) == 0x80 {
			n++
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

// ParseSSE splits an SSE response body into (event, data) pairs.
func ParseSSE(body string) [][2]string {
	var out [][2]string
	r := newSSEReader(strings.NewReader(body))
	for {
		ev, err := r.Next()
		if err != nil {
			return out
		}
		out = append(out, [2]string{ev.Event, ev.Data})
	}
}
