package model

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Options is the flat, protocol-specific ConnectionOptions object (SPEC §5.3). It is map-backed so that unknown keys
// round-trip untouched; typed accessors coerce JSON values and fall back to defaults.
//
// Every accessor takes an optional explicit default; without one, the registry default of OptionDefaults is used.
// Empty strings count as "unset" for String.
type Options map[string]any

// OptionDefaults are the documented defaults of well-known option keys (SPEC §5.3).
var OptionDefaults = map[string]any{
	"term":          "xterm-256color",
	"encoding":      "utf-8",
	"backspace":     "del",
	"autoReconnect": false,
	"record":        false,
	"log":           false,
	"monitoring":    true,
	"negotiate":     true,
	"lineEnding":    "crlf",
	"baud":          9600,
	"dataBits":      8,
	"parity":        "none",
	"stopBits":      "1",
	"flowControl":   "none",
	"passive":       true,
	"ftpTls":        "none",
	"shared":        true,
	"scaling":       "fit",
	"rdpEngine":     "ironrdp",
	"security":      "any",
	"resizeMethod":  "display-update",
	"colorDepth":    32,
	"predict":       "adaptive",
	"quality":       6,
	"compression":   2,
}

// MarshalJSON renders a nil Options as {} (never null).
func (o Options) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(map[string]any(o))
}

// Has reports whether key is present with a non-null value.
func (o Options) Has(key string) bool {
	v, ok := o[key]
	return ok && v != nil
}

// Get returns the raw value of key, or its registry default.
func (o Options) Get(key string) any {
	if v, ok := o[key]; ok && v != nil {
		return v
	}
	return OptionDefaults[key]
}

// String returns key as a string. Numbers and booleans are formatted; empty strings are treated as unset.
func (o Options) String(key string, def ...string) string {
	if v, ok := o[key]; ok && v != nil {
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		case int:
			return strconv.Itoa(t)
		case int64:
			return strconv.FormatInt(t, 10)
		case json.Number:
			return t.String()
		case bool:
			return strconv.FormatBool(t)
		}
	}
	if len(def) > 0 {
		return def[0]
	}
	if d, ok := OptionDefaults[key].(string); ok {
		return d
	}
	return ""
}

// Int returns key as an int (accepting JSON numbers and numeric strings).
func (o Options) Int(key string, def ...int) int {
	if v, ok := o[key]; ok && v != nil {
		if n, ok := toInt(v); ok {
			return n
		}
	}
	if len(def) > 0 {
		return def[0]
	}
	if d, ok := OptionDefaults[key].(int); ok {
		return d
	}
	return 0
}

// Float returns key as a float64.
func (o Options) Float(key string, def ...float64) float64 {
	if v, ok := o[key]; ok && v != nil {
		switch t := v.(type) {
		case float64:
			return t
		case int:
			return float64(t)
		case int64:
			return float64(t)
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return f
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f
			}
		}
	}
	if len(def) > 0 {
		return def[0]
	}
	if d, ok := toInt(OptionDefaults[key]); ok {
		return float64(d)
	}
	return 0
}

// Bool returns key as a bool (accepting "true"/"1"/"yes"/"on" strings and non-zero numbers).
func (o Options) Bool(key string, def ...bool) bool {
	if v, ok := o[key]; ok && v != nil {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off", "":
				return false
			}
		default:
			if n, ok := toInt(v); ok {
				return n != 0
			}
		}
	}
	if len(def) > 0 {
		return def[0]
	}
	if d, ok := OptionDefaults[key].(bool); ok {
		return d
	}
	return false
}

// Strings returns key as a string slice. A lone non-empty string is returned as a one-element slice.
func (o Options) Strings(key string) []string {
	v, ok := o[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return append([]string(nil), t...)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			switch s := e.(type) {
			case string:
				out = append(out, s)
			case float64, int, int64, json.Number, bool:
				out = append(out, Options{"v": s}.String("v"))
			}
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	}
	return nil
}

// Map returns key as a nested object (nil when absent or not an object).
func (o Options) Map(key string) map[string]any {
	switch t := o[key].(type) {
	case map[string]any:
		return t
	case Options:
		return t
	}
	return nil
}

// StringMap returns key as a map of strings (e.g. `env`); non-string values are formatted.
func (o Options) StringMap(key string) map[string]string {
	m := o.Map(key)
	if m == nil {
		if sm, ok := o[key].(map[string]string); ok {
			return sm
		}
		return nil
	}
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = Options(m).String(k, "")
	}
	return out
}

// Decode re-marshals key into v (for structured options such as `proxy` or `portKnock`). Missing keys leave v
// untouched and return nil.
func (o Options) Decode(key string, v any) error {
	raw, ok := o[key]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Clone returns a deep copy.
func (o Options) Clone() Options {
	if o == nil {
		return Options{}
	}
	out := make(Options, len(o))
	for k, v := range o {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = cloneValue(e)
		}
		return m
	case Options:
		return t.Clone()
	case []any:
		s := make([]any, len(t))
		for i, e := range t {
			s[i] = cloneValue(e)
		}
		return s
	case []string:
		return append([]string(nil), t...)
	case map[string]string:
		m := make(map[string]string, len(t))
		for k, e := range t {
			m[k] = e
		}
		return m
	}
	return v
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case int32:
		return int(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t > math.MaxInt32*4 || t < -math.MaxInt32*4 {
			return 0, false
		}
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
		if f, err := t.Float64(); err == nil {
			return int(f), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}
