package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

type pendingPrompt struct {
	userID string
	msg    []byte // pre-marshaled {type:'prompt', prompt}
	ch     chan model.PromptResponse
}

type promptEvent struct {
	Type   string       `json:"type"`
	Prompt model.Prompt `json:"prompt"`
}

type promptCancelEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Prompt asks the user an interactive question on every connected socket and blocks until the first answer, ctx
// cancellation, hub shutdown or Hub.PromptTimeout. The other sockets receive {type:'prompt.cancel'} once answered.
// Pending prompts are replayed to sockets that connect later (page reload). It returns ErrNoInteractiveClient when
// the user has no connected socket.
func (h *Hub) Prompt(ctx context.Context, userID string, p model.Prompt) (model.PromptResponse, error) {
	if !h.HasClient(userID) {
		return model.PromptResponse{}, ErrNoInteractiveClient
	}
	if p.ID == "" {
		p.ID = model.NewID()
	}
	if p.Fields == nil {
		p.Fields = []model.PromptField{}
	}
	msg, err := json.Marshal(promptEvent{Type: model.EvPrompt, Prompt: p})
	if err != nil {
		return model.PromptResponse{}, err
	}
	pd := &pendingPrompt{userID: userID, msg: msg, ch: make(chan model.PromptResponse, 1)}
	h.mu.Lock()
	h.prompts[p.ID] = pd
	h.mu.Unlock()
	h.Publish(userID, msg)

	timeout := h.PromptTimeout
	if timeout <= 0 {
		timeout = DefaultPromptTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var cause error
	select {
	case resp := <-pd.ch:
		return resp, nil
	case <-ctx.Done():
		cause = ctx.Err()
	case <-h.ctx.Done():
		cause = h.ctx.Err()
	case <-timer.C:
		cause = ErrPromptTimeout
	}
	if !h.withdrawPrompt(p.ID, userID) {
		// Answered concurrently: the response is (or is about to be) in the buffered channel.
		select {
		case resp := <-pd.ch:
			return resp, nil
		default:
		}
	}
	return model.PromptResponse{}, cause
}

// withdrawPrompt removes a pending prompt and tells the user's sockets to close it. It returns false when the prompt
// was already answered.
func (h *Hub) withdrawPrompt(id, userID string) bool {
	h.mu.Lock()
	_, ok := h.prompts[id]
	delete(h.prompts, id)
	h.mu.Unlock()
	if ok {
		h.Publish(userID, promptCancelEvent{Type: model.EvPromptCancel, ID: id})
	}
	return ok
}

// answerPrompt delivers the first answer for id (only from a socket of the prompted user) and cancels the prompt on
// the user's other sockets.
func (h *Hub) answerPrompt(from *client, id string, resp model.PromptResponse) {
	h.mu.Lock()
	pd, ok := h.prompts[id]
	if !ok || pd.userID != from.user.ID {
		h.mu.Unlock()
		return
	}
	delete(h.prompts, id)
	var others []*client
	for cid, c := range h.byUser[pd.userID] {
		if cid != from.id {
			others = append(others, c)
		}
	}
	h.mu.Unlock()

	pd.ch <- resp // buffered (1) and only one sender reaches here
	cancel, _ := json.Marshal(promptCancelEvent{Type: model.EvPromptCancel, ID: id})
	for _, c := range others {
		c.enqueue(cancel)
	}
}

// PendingPrompts returns the number of prompts waiting for an answer (diagnostics/tests).
func (h *Hub) PendingPrompts() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.prompts)
}
