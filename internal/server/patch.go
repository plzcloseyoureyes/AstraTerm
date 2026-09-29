package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
)

// patch is a decoded PATCH body: present keys are applied, absent keys are left unchanged, and null clears nullable
// fields.
type patch map[string]json.RawMessage

func decodePatch(c *echo.Context) (patch, error) {
	var p patch
	if err := httpx.Bind(c, &p); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, httpx.BadRequest("request body must be a JSON object")
	}
	return p, nil
}

func (p patch) has(k string) bool { _, ok := p[k]; return ok }

func (p patch) isNull(k string) bool {
	v, ok := p[k]
	return ok && strings.TrimSpace(string(v)) == "null"
}

// into decodes key k into dst when present (null leaves dst's zero value for strings/ids).
func (p patch) into(k string, dst any) error {
	v, ok := p[k]
	if !ok {
		return nil
	}
	if strings.TrimSpace(string(v)) == "null" {
		switch d := dst.(type) {
		case *string:
			*d = ""
			return nil
		case *[]string:
			*d = []string{}
			return nil
		case *model.Options:
			*d = model.Options{}
			return nil
		case *map[string]string:
			*d = nil
			return nil
		}
		return httpx.BadRequest(fmt.Sprintf("field %q must not be null", k))
	}
	// json.Unmarshal into a non-nil map keeps the existing entries (it merges), so a PATCH could never remove an
	// option. Objects like `options` replace the whole value: decode into a fresh map and swap it in.
	if d, ok := dst.(*model.Options); ok {
		fresh := model.Options{}
		if err := json.Unmarshal(v, &fresh); err != nil {
			return httpx.BadRequest(fmt.Sprintf("invalid value for field %q", k))
		}
		*d = fresh
		return nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return httpx.BadRequest(fmt.Sprintf("invalid value for field %q", k))
	}
	return nil
}

// apply decodes several fields, stopping at the first error.
func (p patch) apply(fields map[string]any) error {
	for k, dst := range fields {
		if err := p.into(k, dst); err != nil {
			return err
		}
	}
	return nil
}

// ---- validation helpers -------------------------------------------------------------------------------------------

var secretKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func checkLen(field, v string, min, max int) error {
	n := utf8.RuneCountInString(v)
	if n < min || len(v) > max*4 || n > max {
		if min > 0 && n < min {
			return httpx.BadRequest(fmt.Sprintf("%s is required", field))
		}
		return httpx.BadRequest(fmt.Sprintf("%s must be at most %d characters", field, max))
	}
	return nil
}

func cleanTags(tags []string) ([]string, error) {
	if len(tags) > 64 {
		return nil, httpx.BadRequest("at most 64 tags are allowed")
	}
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if utf8.RuneCountInString(t) > 64 {
			return nil, httpx.BadRequest("tags must be at most 64 characters")
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out, nil
}

// ---- small failure limiter (vault unlock) -------------------------------------------------------------------------

// failLimiter applies exponential backoff after repeated failures (5 free attempts, then 1s, 2s, … up to 5 min).
type failLimiter struct {
	mu          sync.Mutex
	fails       int
	lockedUntil time.Time
}

func (f *failLimiter) wait() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := time.Until(f.lockedUntil); d > 0 {
		return d
	}
	return 0
}

func (f *failLimiter) fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails++
	if f.fails >= 5 {
		shift := min(f.fails-5, 10)
		d := min(time.Second<<shift, 5*time.Minute)
		f.lockedUntil = time.Now().Add(d)
	}
}

func (f *failLimiter) reset() {
	f.mu.Lock()
	f.fails, f.lockedUntil = 0, time.Time{}
	f.mu.Unlock()
}
