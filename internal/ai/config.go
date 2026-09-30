package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
)

// settingsKey is the settings key (global scope = organisation default, user scope = personal override) holding the
// provider configuration. API keys are NOT stored there: they live vault-sealed in the module table ai_keys.
const settingsKey = "aiProvider"

// Provider kinds understood by the backend. Presets (OpenAI, Ollama, LM Studio, Gemini, vLLM…) are UI labels over
// the OpenAI-compatible kind.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// DefaultModel is the default Anthropic model.
const DefaultModel = "claude-sonnet-5"

// DefaultAnthropicURL / DefaultOpenAIURL are the provider base URLs used when none is configured.
const (
	DefaultAnthropicURL = "https://api.anthropic.com"
	DefaultOpenAIURL    = "https://api.openai.com/v1"
)

// Default per-user rate limits (requests), used when the admin has not set any.
const (
	defaultPerMinute = 20
	defaultPerDay    = 1000
)

// ModelInfo describes a selectable model.
type ModelInfo struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// AnthropicModels is the built-in list offered for the Anthropic provider (the provider's /v1/models listing is used
// when reachable).
var AnthropicModels = []ModelInfo{
	{ID: "claude-sonnet-5", Label: "Claude Sonnet 5", Hint: "Fast and capable — recommended"},
	{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Hint: "Deepest reasoning"},
	{ID: "claude-fable-5-1", Label: "Claude Fable 5.1", Hint: "Most capable, slowest"},
	{ID: "claude-haiku-4-5-20251001", Label: "Claude Haiku 4.5", Hint: "Fastest, lowest cost"},
}

// Config is the stored provider configuration of one scope (JSON in settings key "aiProvider"). Pointer fields
// distinguish "unset" (inherit) from explicit values.
type Config struct {
	// Enabled switches the assistant on/off. Global scope: in server mode the admin must enable it (default off); in
	// desktop mode it defaults to on. User scope: a personal "off" hides the assistant for that user.
	Enabled  *bool  `json:"enabled,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Preset is a UI label (anthropic, openai, ollama, lmstudio, gemini, vllm, custom); informational only.
	Preset    string `json:"preset,omitempty"`
	BaseURL   string `json:"baseUrl,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"` // auto | low | medium | high
	MaxTokens int    `json:"maxTokens,omitempty"`

	// Admin policy (global scope only; ignored in user scope).
	Models             []string `json:"models,omitempty"`             // allowed models for users (empty = any)
	AllowUserConfig    *bool    `json:"allowUserConfig,omitempty"`    // server mode: users may use their own provider + key
	AllowUserModel     *bool    `json:"allowUserModel,omitempty"`     // users may pick another model (default true)
	RateLimitPerMinute int      `json:"rateLimitPerMinute,omitempty"` // 0 = default, < 0 = unlimited
	RateLimitPerDay    int      `json:"rateLimitPerDay,omitempty"`    // 0 = default, < 0 = unlimited
	RedactPatterns     []string `json:"redactPatterns,omitempty"`     // extra RE2 patterns redacted from context
}

var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// normalize validates and cleans a configuration. global selects which fields are meaningful.
func (c *Config) normalize(global bool) error {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	switch c.Provider {
	case "", ProviderAnthropic, ProviderOpenAI:
	default:
		return httpx.BadRequest("provider must be anthropic or openai")
	}
	c.Preset = strings.ToLower(strings.TrimSpace(c.Preset))
	if len(c.Preset) > 32 {
		return httpx.BadRequest("preset is too long")
	}
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	if c.BaseURL != "" {
		u, err := validateBaseURL(c.BaseURL)
		if err != nil {
			return err
		}
		c.BaseURL = u
	}
	c.Model = strings.TrimSpace(c.Model)
	if c.Model != "" && !modelIDRe.MatchString(c.Model) {
		return httpx.BadRequest("model id contains unsupported characters")
	}
	c.Effort = strings.ToLower(strings.TrimSpace(c.Effort))
	switch c.Effort {
	case "", "auto", "low", "medium", "high":
	default:
		return httpx.BadRequest("effort must be auto, low, medium or high")
	}
	if c.MaxTokens < 0 || c.MaxTokens > 64000 {
		return httpx.BadRequest("maxTokens must be between 0 and 64000")
	}
	if !global {
		c.Models, c.AllowUserConfig, c.AllowUserModel, c.RedactPatterns = nil, nil, nil, nil
		c.RateLimitPerMinute, c.RateLimitPerDay = 0, 0
		return nil
	}
	if len(c.Models) > 50 {
		return httpx.BadRequest("at most 50 allowed models")
	}
	models := c.Models[:0]
	seen := map[string]bool{}
	for _, m := range c.Models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if !modelIDRe.MatchString(m) {
			return httpx.BadRequest(fmt.Sprintf("invalid model id %q", m))
		}
		seen[m] = true
		models = append(models, m)
	}
	c.Models = models
	if c.RateLimitPerMinute > 10000 || c.RateLimitPerDay > 1000000 {
		return httpx.BadRequest("rate limit is too high")
	}
	if len(c.RedactPatterns) > 50 {
		return httpx.BadRequest("at most 50 redaction patterns")
	}
	pats := c.RedactPatterns[:0]
	for _, p := range c.RedactPatterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > 500 {
			return httpx.BadRequest("redaction pattern is too long")
		}
		if _, err := regexp.Compile(p); err != nil {
			return httpx.BadRequest(fmt.Sprintf("invalid redaction pattern %q: %v", p, err))
		}
		pats = append(pats, p)
	}
	c.RedactPatterns = pats
	return nil
}

// validateBaseURL accepts absolute http(s) URLs without credentials, query or fragment; it returns the URL without a
// trailing slash.
func validateBaseURL(raw string) (string, error) {
	if len(raw) > 512 {
		return "", httpx.BadRequest("base URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", httpx.BadRequest("base URL must be an absolute http:// or https:// URL")
	}
	if u.User != nil {
		return "", httpx.BadRequest("base URL must not contain credentials (use the API key field)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", httpx.BadRequest("base URL must not contain a query or fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Effective is the resolved configuration for one user.
type Effective struct {
	Enabled    bool
	Provider   string
	Preset     string
	BaseURL    string
	Model      string
	Effort     string
	MaxTokens  int
	KeyScope   string // settings scope whose key is used (global or the user id)
	HasKey     bool
	Source     string // "global" | "user"
	PerMinute  int
	PerDay     int
	Models     []string
	UserModel  bool // user may choose the model
	UserConfig bool // user may use a personal provider/key
	Redact     []string
	// userRouted is set when the provider endpoint was chosen by a non-admin user in server mode: requests then go
	// through the network guard (SSRF protection).
	userRouted bool
}

// Configured reports whether a request could be sent (provider + model, and a key where the provider needs one).
func (e *Effective) Configured() bool {
	if e.Provider == "" || e.Model == "" {
		return false
	}
	return e.Provider != ProviderAnthropic || e.HasKey
}

// URL returns the effective base URL.
func (e *Effective) URL() string {
	if e.BaseURL != "" {
		return e.BaseURL
	}
	if e.Provider == ProviderAnthropic {
		return DefaultAnthropicURL
	}
	return DefaultOpenAIURL
}

// ModelAllowed reports whether the user may use model m.
func (e *Effective) ModelAllowed(m string) bool {
	if m == e.Model {
		return true
	}
	if !e.UserModel || !modelIDRe.MatchString(m) {
		return false
	}
	if len(e.Models) == 0 || e.Source == "user" {
		return true
	}
	return slices.Contains(e.Models, m)
}

func (h *handler) loadScope(ctx context.Context, scope string) (Config, error) {
	var c Config
	ok, err := h.d.Store.Settings.GetJSON(ctx, scope, settingsKey, &c)
	if err != nil {
		// A corrupt value must not take the feature down: treat it as unset.
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		if errors.As(err, &syn) || errors.As(err, &typ) {
			return Config{}, nil
		}
		return Config{}, err
	}
	if !ok {
		return Config{}, nil
	}
	// Stored values may have been written through the generic settings endpoint: never trust them blindly.
	if err := c.normalize(scope == store.ScopeGlobal); err != nil {
		h.d.Log.Warn("ai: ignoring invalid stored configuration", "scope", scopeLabel(scope), "err", err)
		return Config{}, nil
	}
	return c, nil
}

func scopeLabel(scope string) string {
	if scope == store.ScopeGlobal {
		return "global"
	}
	return "user"
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// canUserConfigure reports whether user may keep a personal provider configuration.
func (h *handler) canUserConfigure(user *model.User, global Config) bool {
	return h.d.Cfg.IsDesktop() || user.IsAdmin() || boolOr(global.AllowUserConfig, false)
}

// effective resolves the configuration for user: global defaults, overridden by the user's personal provider (when
// allowed) and personal model choice.
func (h *handler) effective(ctx context.Context, user *model.User) (*Effective, error) {
	global, err := h.loadScope(ctx, store.ScopeGlobal)
	if err != nil {
		return nil, err
	}
	personal, err := h.loadScope(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	e := &Effective{
		Enabled:   boolOr(global.Enabled, h.d.Cfg.IsDesktop()),
		Provider:  global.Provider,
		Preset:    global.Preset,
		BaseURL:   global.BaseURL,
		Model:     global.Model,
		Effort:    global.Effort,
		MaxTokens: global.MaxTokens,
		KeyScope:  store.ScopeGlobal,
		Source:    "global",
		PerMinute: global.RateLimitPerMinute,
		PerDay:    global.RateLimitPerDay,
		Models:    global.Models,
		UserModel: h.d.Cfg.IsDesktop() || user.IsAdmin() || boolOr(global.AllowUserModel, true),
		Redact:    global.RedactPatterns,
	}
	e.UserConfig = h.canUserConfigure(user, global)
	if e.Provider != "" && e.Model == "" && e.Provider == ProviderAnthropic {
		e.Model = DefaultModel
	}
	if personal.Enabled != nil && !*personal.Enabled {
		e.Enabled = false
	}
	if e.UserConfig && personal.Provider != "" {
		e.Provider, e.Preset, e.BaseURL = personal.Provider, personal.Preset, personal.BaseURL
		e.Model = personal.Model
		if e.Model == "" && e.Provider == ProviderAnthropic {
			e.Model = DefaultModel
		}
		if personal.Effort != "" {
			e.Effort = personal.Effort
		}
		if personal.MaxTokens > 0 {
			e.MaxTokens = personal.MaxTokens
		}
		e.KeyScope, e.Source = user.ID, "user"
		e.UserModel = true
		e.userRouted = h.d.Cfg.IsServer() && !user.IsAdmin()
	} else if personal.Model != "" && e.ModelAllowed(personal.Model) {
		e.Model = personal.Model
	}
	if e.PerMinute == 0 {
		e.PerMinute = defaultPerMinute
	}
	if e.PerDay == 0 {
		e.PerDay = defaultPerDay
	}
	has, err := h.keys.has(ctx, e.KeyScope)
	if err != nil {
		return nil, err
	}
	e.HasKey = has
	return e, nil
}

// ---- API key storage (module table ai_keys, vault-sealed with the data key) ------------------------------------

func init() {
	store.RegisterMigration("ai", 1, `
CREATE TABLE IF NOT EXISTS ai_keys (
	scope      TEXT PRIMARY KEY,
	key_enc    BLOB NOT NULL,
	updated_at INTEGER NOT NULL
);`)
}

type keyStore struct {
	h *handler
}

func (k keyStore) has(ctx context.Context, scope string) (bool, error) {
	var n int
	err := k.h.d.Store.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM ai_keys WHERE scope = ?`, scope).Scan(&n)
	return n > 0, err
}

// get returns the decrypted key of scope ("" when none). httpx.ErrLocked while the vault is locked.
func (k keyStore) get(ctx context.Context, scope string) (string, error) {
	var enc []byte
	err := k.h.d.Store.DB.QueryRowContext(ctx, `SELECT key_enc FROM ai_keys WHERE scope = ?`, scope).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if k.h.d.Vault.Locked() {
		return "", httpx.ErrLocked
	}
	pt, err := k.h.d.Vault.Open(enc)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return "", httpx.ErrLocked
		}
		return "", fmt.Errorf("ai: open API key: %w", err)
	}
	return string(pt), nil
}

func (k keyStore) set(ctx context.Context, scope, key string) error {
	if key == "" {
		_, err := k.h.d.Store.DB.ExecContext(ctx, `DELETE FROM ai_keys WHERE scope = ?`, scope)
		return err
	}
	if k.h.d.Vault.Locked() {
		return httpx.ErrLocked
	}
	enc, err := k.h.d.Vault.Seal([]byte(key))
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			return httpx.ErrLocked
		}
		return err
	}
	_, err = k.h.d.Store.DB.ExecContext(ctx, `INSERT INTO ai_keys(scope, key_enc, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(scope) DO UPDATE SET key_enc = excluded.key_enc, updated_at = excluded.updated_at`,
		scope, enc, store.Now().UnixMilli())
	return err
}

// validateAPIKey rejects obviously broken keys (control characters would corrupt the HTTP header).
func validateAPIKey(k string) error {
	if len(k) > 4096 {
		return httpx.BadRequest("API key is too long")
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f {
			return httpx.BadRequest("API key contains control characters")
		}
	}
	return nil
}
