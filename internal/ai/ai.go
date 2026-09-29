// Package ai implements AstraTerm's optional AI assistant (RESEARCH TOOL-10, TOOL-11, TOOL-12): a provider proxy
// (Anthropic Messages API, OpenAI-compatible chat/completions) with streamed answers, natural-language → command
// with a risk rating, explain/fix of terminal errors and a sysadmin chat. The browser never talks to the provider:
// keys stay in the vault, context is redacted server-side, requests are rate-limited per user and audited without
// content. Nothing touches the network until a provider has been configured.
//
// Endpoints (all authenticated):
//
//	GET  /api/ai/status                  availability, provider/model, model picker, permissions (no network)
//	GET  /api/ai/config?scope=global|user stored configuration (+ hasKey; the key itself is write-only)
//	PUT  /api/ai/config                  {scope, …Config, apiKey?, reset?}
//	POST /api/ai/test                    {scope, …draft Config, apiKey?} → {ok, latencyMs, model, reply?}
//	GET  /api/ai/models?scope=           models offered by the configured endpoint
//	POST /api/ai/chat                    {messages, context?, mode, model?} → text/event-stream
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/app"
	"github.com/plzcloseyoureyes/astraterm/internal/core"
	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/netguard"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

type handler struct {
	d      *app.Deps
	c      *core.Core
	keys   keyStore
	limits *limiter
	// listLimits bounds model listings (they reach the provider with the organisation key; not billed, but a
	// flood could get the key throttled).
	listLimits *limiter
	secrets    secretCache
	client     *http.Client
	// guarded builds the client for user-routed endpoints (server mode, non-admin personal provider).
	guarded func(user *model.User) *http.Client
}

// Mount registers the module's routes and its table.
func Mount(d *app.Deps, c *core.Core) error {
	h := newHandler(d, c)
	api := d.Router.API()
	api.GET("/ai/status", h.status)
	api.GET("/ai/config", h.getConfig)
	api.PUT("/ai/config", h.putConfig)
	api.POST("/ai/test", h.test)
	api.GET("/ai/models", h.models)
	api.POST("/ai/chat", h.chat, httpx.BodyLimit(4<<20))
	return nil
}

func newHandler(d *app.Deps, c *core.Core) *handler {
	h := &handler{d: d, c: c, limits: newLimiter(), listLimits: newLimiter(), client: newHTTPClient(nil)}
	h.keys = keyStore{h: h}
	h.guarded = func(user *model.User) *http.Client {
		// One transport per request (the user's guard applies to its dials): no idle connections to leak.
		t := defaultTransport()
		t.DisableKeepAlives = true
		return newHTTPClient(netguard.ForUser(d, user).Transport(t))
	}
	return h
}

// ---- status ---------------------------------------------------------------------------------------------------------

type statusView struct {
	Available          bool        `json:"available"`
	Enabled            bool        `json:"enabled"`
	Configured         bool        `json:"configured"`
	Reason             string      `json:"reason,omitempty"` // disabled | not_configured | locked
	Provider           string      `json:"provider,omitempty"`
	Preset             string      `json:"preset,omitempty"`
	Model              string      `json:"model,omitempty"`
	Source             string      `json:"source"`
	Models             []ModelInfo `json:"models"`
	CanPickModel       bool        `json:"canPickModel"`
	CanConfigure       bool        `json:"canConfigure"`
	CanConfigureGlobal bool        `json:"canConfigureGlobal"`
	Mode               string      `json:"mode"`
	Locked             bool        `json:"locked"`
	Limits             struct {
		PerMinute int `json:"perMinute"`
		PerDay    int `json:"perDay"`
	} `json:"limits"`
}

func (h *handler) status(c *echo.Context) error {
	user := httpx.UserFrom(c)
	e, err := h.effective(c.Request().Context(), user)
	if err != nil {
		return err
	}
	v := statusView{
		Enabled: e.Enabled, Configured: e.Configured(), Provider: e.Provider, Preset: e.Preset, Model: e.Model,
		Source: e.Source, CanPickModel: e.UserModel, CanConfigure: e.UserConfig, CanConfigureGlobal: user.IsAdmin(),
		Mode: h.d.Cfg.Mode, Locked: e.HasKey && h.d.Vault.Locked(),
	}
	v.Limits.PerMinute, v.Limits.PerDay = e.PerMinute, e.PerDay
	switch {
	case !e.Enabled:
		v.Reason = "disabled"
	case !v.Configured:
		v.Reason = "not_configured"
	case v.Locked:
		v.Reason = "locked"
	}
	v.Available = v.Reason == "" || v.Reason == "locked" // locked: requests prompt for the unlock
	v.Models = pickerModels(e)
	return c.JSON(http.StatusOK, v)
}

// pickerModels is the model list for the chat picker without any network call.
func pickerModels(e *Effective) []ModelInfo {
	out := []ModelInfo{}
	seen := map[string]bool{}
	add := func(m ModelInfo) {
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	label := func(id string) ModelInfo {
		for _, m := range AnthropicModels {
			if m.ID == id {
				return m
			}
		}
		return ModelInfo{ID: id}
	}
	add(label(e.Model))
	if !e.UserModel {
		return out
	}
	if len(e.Models) > 0 && e.Source == "global" {
		for _, id := range e.Models {
			add(label(id))
		}
		return out
	}
	if e.Provider == ProviderAnthropic {
		for _, m := range AnthropicModels {
			add(m)
		}
	}
	return out
}

// ---- configuration --------------------------------------------------------------------------------------------------

type configView struct {
	Config
	Scope   string `json:"scope"`
	HasKey  bool   `json:"hasKey"`
	Allowed bool   `json:"allowed"` // the caller may change provider/endpoint/key in this scope
}

func (h *handler) scopeFor(c *echo.Context, scope string) (string, error) {
	user := httpx.UserFrom(c)
	switch scope {
	case "global":
		if !user.IsAdmin() {
			return "", httpx.Forbidden("only administrators can change the organisation's AI settings")
		}
		return store.ScopeGlobal, nil
	case "", "user":
		return user.ID, nil
	}
	return "", httpx.BadRequest("scope must be global or user")
}

func (h *handler) getConfig(c *echo.Context) error {
	ctx := c.Request().Context()
	scope, err := h.scopeFor(c, c.QueryParam("scope"))
	if err != nil {
		return err
	}
	cfg, err := h.loadScope(ctx, scope)
	if err != nil {
		return err
	}
	has, err := h.keys.has(ctx, scope)
	if err != nil {
		return err
	}
	allowed := true
	if scope != store.ScopeGlobal {
		global, err := h.loadScope(ctx, store.ScopeGlobal)
		if err != nil {
			return err
		}
		allowed = h.canUserConfigure(httpx.UserFrom(c), global)
	}
	return c.JSON(http.StatusOK, configView{Config: cfg, Scope: scopeLabel(scope), HasKey: has, Allowed: allowed})
}

type putConfigRequest struct {
	Config
	Scope  string  `json:"scope"`
	APIKey *string `json:"apiKey,omitempty"` // omitted = unchanged, "" = delete
	Reset  bool    `json:"reset,omitempty"`  // delete this scope's configuration and key
}

func (h *handler) putConfig(c *echo.Context) error {
	ctx := c.Request().Context()
	user := httpx.UserFrom(c)
	var req putConfigRequest
	if err := httpx.BindLimit(c, &req, 64<<10); err != nil {
		return err
	}
	scope, err := h.scopeFor(c, req.Scope)
	if err != nil {
		return err
	}
	global := scope == store.ScopeGlobal
	if req.Reset {
		if err := h.d.Store.Settings.Delete(ctx, scope, settingsKey); err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		if err := h.keys.set(ctx, scope, ""); err != nil {
			return err
		}
		h.d.Audit.Log(c, "ai.config.reset", scopeLabel(scope), nil)
		h.changed(scope)
		return h.getConfigFor(c, scope)
	}
	cfg := req.Config
	if err := cfg.normalize(global); err != nil {
		return err
	}
	if !global {
		gcfg, err := h.loadScope(ctx, store.ScopeGlobal)
		if err != nil {
			return err
		}
		if !h.canUserConfigure(user, gcfg) && (cfg.Provider != "" || cfg.BaseURL != "" || req.APIKey != nil && *req.APIKey != "") {
			return httpx.Forbidden("your administrator manages the AI provider; you can only choose the model")
		}
	}
	if cfg.Provider == "" && cfg.BaseURL != "" {
		return httpx.BadRequest("choose a provider for this endpoint")
	}
	if req.APIKey != nil {
		k := strings.TrimSpace(*req.APIKey)
		if err := validateAPIKey(k); err != nil {
			return err
		}
		if err := h.keys.set(ctx, scope, k); err != nil {
			return err
		}
	}
	if err := h.d.Store.Settings.SetJSON(ctx, scope, settingsKey, cfg); err != nil {
		return err
	}
	h.d.Audit.Log(c, "ai.config.update", scopeLabel(scope), map[string]any{
		"provider": cfg.Provider, "model": cfg.Model, "keyChanged": req.APIKey != nil,
	})
	h.changed(scope)
	return h.getConfigFor(c, scope)
}

func (h *handler) getConfigFor(c *echo.Context, scope string) error {
	ctx := c.Request().Context()
	cfg, err := h.loadScope(ctx, scope)
	if err != nil {
		return err
	}
	has, err := h.keys.has(ctx, scope)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, configView{Config: cfg, Scope: scopeLabel(scope), HasKey: has, Allowed: true})
}

// changed tells browsers to refresh the assistant status (everyone for global changes, the user otherwise).
func (h *handler) changed(scope string) {
	ev := map[string]any{"type": "ai.status"}
	if scope == store.ScopeGlobal {
		h.d.Events.Broadcast(ev)
	} else {
		h.d.Events.Publish(scope, ev)
	}
}

// ---- providers ------------------------------------------------------------------------------------------------------

func providerLabel(e *Effective) string {
	switch e.Preset {
	case "ollama":
		return "Ollama"
	case "lmstudio":
		return "LM Studio"
	case "gemini":
		return "Gemini"
	case "vllm":
		return "vLLM"
	case "openai":
		return "OpenAI"
	}
	if e.Provider == ProviderAnthropic {
		return "Anthropic"
	}
	return "AI provider"
}

func (h *handler) provider(e *Effective, key string, user *model.User) Provider {
	client := h.client
	if e.userRouted {
		client = h.guarded(user)
	}
	if e.Provider == ProviderAnthropic {
		return &anthropicProvider{client: client, baseURL: e.URL(), apiKey: key}
	}
	return &openaiProvider{client: client, baseURL: e.URL(), apiKey: key, label: providerLabel(e)}
}

// ready loads what is needed to call the provider: the effective configuration must be enabled and configured and
// the key readable (423 while the vault is locked).
func (h *handler) ready(ctx context.Context, user *model.User) (*Effective, string, error) {
	e, err := h.effective(ctx, user)
	if err != nil {
		return nil, "", err
	}
	if !e.Enabled {
		return nil, "", httpx.NewError(http.StatusForbidden, "ai_disabled", "the AI assistant is turned off")
	}
	if !e.Configured() {
		return nil, "", httpx.NewError(http.StatusConflict, "ai_not_configured", "no AI provider is configured yet")
	}
	key, err := h.keys.get(ctx, e.KeyScope)
	if err != nil {
		return nil, "", err
	}
	if e.Provider == ProviderAnthropic && key == "" {
		return nil, "", httpx.NewError(http.StatusConflict, "ai_not_configured", "no API key is configured")
	}
	return e, key, nil
}

// providerErr converts a provider failure into the API error; red scrubs the message (a provider or a proxy may echo
// the API key or parts of the request back in its error text).
func providerErr(err error, red *Redactor) error {
	var pe *ProviderError
	if errors.As(err, &pe) {
		cp := *pe
		cp.Message, _ = red.Redact(cp.Message)
		return cp.httpError()
	}
	return err
}

// ---- test connection ---------------------------------------------------------------------------------------------

type testRequest struct {
	Config
	Scope  string  `json:"scope"`
	APIKey *string `json:"apiKey,omitempty"`
}

func (h *handler) test(c *echo.Context) error {
	ctx := c.Request().Context()
	user := httpx.UserFrom(c)
	var req testRequest
	if err := httpx.BindLimit(c, &req, 64<<10); err != nil {
		return err
	}
	scope, err := h.scopeFor(c, req.Scope)
	if err != nil {
		return err
	}
	global := scope == store.ScopeGlobal
	draft := req.Config
	if err := draft.normalize(global); err != nil {
		return err
	}
	if draft.Provider == "" {
		return httpx.BadRequest("choose a provider first")
	}
	gcfg, err := h.loadScope(ctx, store.ScopeGlobal)
	if err != nil {
		return err
	}
	if !global && !h.canUserConfigure(user, gcfg) {
		return httpx.Forbidden("your administrator manages the AI provider")
	}
	e := &Effective{Provider: draft.Provider, Preset: draft.Preset, BaseURL: draft.BaseURL, Model: draft.Model, KeyScope: scope,
		userRouted: !global && h.d.Cfg.IsServer() && !user.IsAdmin()}
	if e.Model == "" && e.Provider == ProviderAnthropic {
		e.Model = DefaultModel
	}
	if e.Model == "" {
		return httpx.BadRequest("enter a model name")
	}
	key := ""
	if req.APIKey != nil {
		key = strings.TrimSpace(*req.APIKey)
		if err := validateAPIKey(key); err != nil {
			return err
		}
	} else if key, err = h.keys.get(ctx, scope); err != nil {
		return err
	}
	if e.Provider == ProviderAnthropic && key == "" {
		return httpx.BadRequest("enter an API key")
	}
	perMinute, perDay := gcfg.RateLimitPerMinute, gcfg.RateLimitPerDay
	if perMinute == 0 {
		perMinute = defaultPerMinute
	}
	if perDay == 0 {
		perDay = defaultPerDay
	}
	if err := h.limits.allow(user.ID, perMinute, perDay); err != nil {
		return err
	}
	h.d.Audit.Log(c, "ai.test", scopeLabel(scope), map[string]any{"provider": e.Provider, "model": e.Model})
	red := newRedactor([]string{key}, nil)
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	st, err := h.provider(e, key, user).Open(tctx, Request{
		Model: e.Model, MaxTokens: 256, Effort: "low",
		Messages: []Message{{Role: "user", Content: "Connection test from AstraTerm. Reply with the single word: ready"}},
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return httpx.NewError(http.StatusFailedDependency, "provider_timeout", "the provider did not answer within 60 seconds")
		}
		return providerErr(err, red)
	}
	defer st.Close()
	var reply strings.Builder
	for {
		ev, err := st.Next()
		if err != nil {
			if errors.Is(tctx.Err(), context.DeadlineExceeded) {
				return httpx.NewError(http.StatusFailedDependency, "provider_timeout", "the provider stopped answering")
			}
			return providerErr(err, red)
		}
		if ev.Type == "text" && reply.Len() < 200 {
			reply.WriteString(ev.Text)
		}
		if ev.Type == "done" {
			break
		}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ok": true, "latencyMs": time.Since(start).Milliseconds(), "model": e.Model, "reply": clip(strings.TrimSpace(reply.String()), 200),
	})
}

// ---- model listing ---------------------------------------------------------------------------------------------

func (h *handler) models(c *echo.Context) error {
	ctx := c.Request().Context()
	user := httpx.UserFrom(c)
	e, err := h.effective(ctx, user)
	if err != nil {
		return err
	}
	if q := c.QueryParam("scope"); q != "" {
		// Settings screens list the models of the scope being edited.
		scope, err := h.scopeFor(c, q)
		if err != nil {
			return err
		}
		cfg, err := h.loadScope(ctx, scope)
		if err != nil {
			return err
		}
		if cfg.Provider == "" {
			return c.JSON(http.StatusOK, map[string]any{"models": AnthropicModels, "source": "builtin"})
		}
		has, err := h.keys.has(ctx, scope)
		if err != nil {
			return err
		}
		e = &Effective{Provider: cfg.Provider, Preset: cfg.Preset, BaseURL: cfg.BaseURL, Model: cfg.Model, KeyScope: scope, HasKey: has,
			userRouted: scope != store.ScopeGlobal && h.d.Cfg.IsServer() && !user.IsAdmin()}
	}
	builtin := func(msg string) error {
		out := map[string]any{"models": pickerModels(e), "source": "builtin"}
		if e.Provider == ProviderAnthropic {
			out["models"] = AnthropicModels
		}
		if msg != "" {
			out["error"] = msg
		}
		return c.JSON(http.StatusOK, out)
	}
	if e.Provider == "" || (e.Provider == ProviderAnthropic && !e.HasKey) {
		return builtin("")
	}
	key, err := h.keys.get(ctx, e.KeyScope)
	if err != nil {
		if errors.Is(err, httpx.ErrLocked) {
			return builtin("unlock the vault to list the provider's models")
		}
		return err
	}
	if h.listLimits.allow(user.ID, 20, 2000) != nil {
		return builtin("too many model listings — try again in a minute")
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	list, err := h.provider(e, key, user).Models(lctx)
	if err != nil {
		var pe *ProviderError
		if errors.As(err, &pe) {
			msg, _ := newRedactor([]string{key}, nil).Redact(pe.Error())
			return builtin(msg)
		}
		return builtin("the model list is not available")
	}
	if e.Provider == ProviderAnthropic {
		for i := range list {
			for _, m := range AnthropicModels {
				if m.ID == list[i].ID && m.Hint != "" {
					list[i].Hint = m.Hint
				}
			}
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"models": list, "source": "provider"})
}

// ---- chat -------------------------------------------------------------------------------------------------------

type chatRequest struct {
	Messages []Message   `json:"messages"`
	Context  ChatContext `json:"context"`
	Mode     string      `json:"mode"`
	Model    string      `json:"model"`
}

const (
	maxMessages       = 200
	maxMessageChars   = 100_000
	maxHistoryChars   = 240_000
	maxStreamedOutput = 1 << 20
)

// cleanMessages validates the conversation, merges consecutive turns of the same role, drops leading assistant
// turns and keeps the newest turns within the history budget.
func cleanMessages(in []Message) ([]Message, error) {
	if len(in) == 0 {
		return nil, httpx.BadRequest("messages must not be empty")
	}
	if len(in) > maxMessages {
		in = in[len(in)-maxMessages:]
	}
	var out []Message
	for _, m := range in {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, httpx.BadRequest("message role must be user or assistant")
		}
		if len(m.Content) > maxMessageChars {
			return nil, httpx.BadRequest(fmt.Sprintf("a message is longer than %d characters", maxMessageChars))
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			out[len(out)-1].Content += "\n\n" + m.Content
			continue
		}
		out = append(out, Message{Role: m.Role, Content: m.Content})
	}
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	if len(out) == 0 || out[len(out)-1].Role != "user" {
		return nil, httpx.BadRequest("the last message must be from the user")
	}
	total := 0
	start := len(out) - 1
	for i := len(out) - 1; i >= 0; i-- {
		total += len(out[i].Content)
		if total > maxHistoryChars && i < len(out)-1 {
			break
		}
		start = i
	}
	out = out[start:]
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out, nil
}

func (h *handler) effortFor(e *Effective, mode string) string {
	switch e.Effort {
	case "low", "medium", "high":
		return e.Effort
	}
	if mode == ModeCommand {
		return "low"
	}
	return "medium"
}

func maxTokensFor(e *Effective, mode string) int {
	switch mode {
	case ModeCommand:
		return 4096
	case ModeExplain:
		return 8192
	}
	if e.MaxTokens > 0 {
		return e.MaxTokens
	}
	return 16000
}

// sessionContext fills server-known facts about a session the caller owns and returns its secrets for redaction.
func (h *handler) sessionContext(ctx context.Context, user *model.User, cc *ChatContext) []string {
	if cc.SessionID == "" || h.c == nil || h.c.Sessions == nil {
		return nil
	}
	s := h.c.Sessions.Get(cc.SessionID)
	if s == nil || s.OwnerID != user.ID {
		return nil
	}
	info := s.Info()
	if cc.SessionInfo == nil {
		cc.SessionInfo = &SessionInfo{}
	}
	si := cc.SessionInfo
	si.Protocol = string(info.Protocol)
	if info.Host != "" {
		si.Host = info.Host
	}
	if info.Username != "" {
		si.Username = info.Username
	}
	if si.Title == "" {
		si.Title = info.Title
	}
	if cwd := s.Cwd(); cwd != "" {
		si.Cwd = cwd
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, secrets, err := s.Resolve(rctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(secrets))
	for _, v := range secrets {
		out = append(out, v)
	}
	return out
}

func (cc *ChatContext) limitInputs() error {
	if len(cc.TerminalText) > 512<<10 || len(cc.Selection) > 512<<10 || (cc.File != nil && len(cc.File.Content) > 1<<20) {
		return httpx.BadRequest("context is too large")
	}
	cc.SessionID = strings.TrimSpace(cc.SessionID)
	if len(cc.SessionID) > 64 {
		cc.SessionID = ""
	}
	return nil
}

func (cc *ChatContext) redact(r *Redactor) int {
	n := 0
	red := func(s *string) {
		var k int
		*s, k = r.Redact(*s)
		n += k
	}
	red(&cc.TerminalText)
	red(&cc.Selection)
	red(&cc.Command)
	if cc.File != nil {
		red(&cc.File.Content)
		red(&cc.File.Path)
	}
	if s := cc.SessionInfo; s != nil {
		for _, p := range []*string{&s.Title, &s.Host, &s.Username, &s.OS, &s.Kernel, &s.Platform, &s.Arch, &s.Shell, &s.Cwd} {
			red(p)
		}
	}
	return n
}

func (h *handler) chat(c *echo.Context) error {
	ctx := c.Request().Context()
	user := httpx.UserFrom(c)
	var req chatRequest
	if err := httpx.BindLimit(c, &req, 4<<20); err != nil {
		return err
	}
	switch req.Mode {
	case "":
		req.Mode = ModeChat
	case ModeChat, ModeCommand, ModeExplain:
	default:
		return httpx.BadRequest("mode must be chat, command or explain")
	}
	msgs, err := cleanMessages(req.Messages)
	if err != nil {
		return err
	}
	if err := req.Context.limitInputs(); err != nil {
		return err
	}
	e, key, err := h.ready(ctx, user)
	if err != nil {
		return err
	}
	modelID := e.Model
	if m := strings.TrimSpace(req.Model); m != "" && m != modelID {
		if !e.ModelAllowed(m) {
			return httpx.NewError(http.StatusForbidden, "model_not_allowed", "this model is not allowed by your administrator")
		}
		modelID = m
	}
	release, err := h.limits.acquire(user.ID)
	if err != nil {
		return err
	}
	defer release()
	if err := h.limits.allow(user.ID, e.PerMinute, e.PerDay); err != nil {
		return err
	}

	// Redaction: the user's stored secrets, the session's live secrets, the provider key, and pattern rules.
	secrets := h.userSecrets(ctx, user)
	secrets = append(secrets, h.sessionContext(ctx, user, &req.Context)...)
	secrets = append(secrets, key)
	red := newRedactor(secrets, e.Redact)
	redactions := req.Context.redact(red)
	for i := range msgs {
		var n int
		msgs[i].Content, n = red.Redact(msgs[i].Content)
		redactions += n
	}
	block := req.Context.render()
	if block != "" {
		last := &msgs[len(msgs)-1]
		last.Content = block + last.Content
	}
	if req.Mode == ModeCommand {
		last := &msgs[len(msgs)-1]
		last.Content += "\n\n(Answer with the JSON object only.)"
	}
	contextChars := len(block)

	h.d.Audit.Log(c, "ai.request", req.Mode, map[string]any{
		"provider": e.Provider, "model": modelID, "context": req.Context.kinds(), "messages": len(msgs), "redactions": redactions,
	})

	pr := h.provider(e, key, user)
	stream, err := pr.Open(ctx, Request{
		Model: modelID, System: systemPrompt(req.Mode), Messages: msgs,
		MaxTokens: maxTokensFor(e, req.Mode), Effort: h.effortFor(e, req.Mode),
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil // client went away
		}
		return providerErr(err, red)
	}
	defer stream.Close()

	w := newSSEWriter(c.Response())
	w.start()
	stop := w.heartbeat(15 * time.Second)
	defer stop()
	_ = w.send("meta", map[string]any{
		"provider": e.Provider, "model": modelID, "mode": req.Mode, "redactions": redactions, "contextChars": contextChars,
	})
	var full strings.Builder
	thinking := false
	for {
		ev, err := stream.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			code, msg := "provider_error", "the answer was interrupted"
			var pe *ProviderError
			if errors.As(err, &pe) {
				code, msg = pe.code(), pe.Error()
				msg, _ = red.Redact(msg)
			} else if errors.Is(err, context.Canceled) {
				code, msg = "provider_timeout", "the provider stopped answering"
			}
			_ = w.send("error", map[string]string{"error": clip(msg, 400), "code": code})
			return nil
		}
		switch ev.Type {
		case "thinking":
			if !thinking {
				thinking = true
				_ = w.send("thinking", map[string]any{})
			}
		case "text":
			if full.Len()+len(ev.Text) > maxStreamedOutput {
				_ = w.send("error", map[string]string{"error": "the answer is too long", "code": "too_long"})
				return nil
			}
			full.WriteString(ev.Text)
			if err := w.send("delta", map[string]string{"text": ev.Text}); err != nil {
				return nil
			}
		case "done":
			if req.Mode == ModeCommand {
				res, ok := parseCommandResult(full.String())
				_ = w.send("result", map[string]any{"command": res.Command, "explanation": res.Explanation, "risk": res.Risk,
					"riskReason": res.RiskReason, "parsed": ok})
			}
			_ = w.send("done", map[string]any{"stopReason": ev.StopReason, "usage": ev.Usage})
			return nil
		}
	}
}

// ---- SSE writer ------------------------------------------------------------------------------------------------

type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *sseWriter) start() {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	_ = s.rc.Flush()
}

func (s *sseWriter) send(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *sseWriter) heartbeat(every time.Duration) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.mu.Lock()
				_, err := fmt.Fprint(s.w, ": ping\n\n")
				if err == nil {
					err = s.rc.Flush()
				}
				s.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(done)
			// Serialize with a heartbeat write in progress before the handler returns (the writer is not usable after).
			s.mu.Lock()
			s.mu.Unlock() //lint:ignore SA2001 barrier: waits for an in-flight heartbeat write
		})
	}
}
