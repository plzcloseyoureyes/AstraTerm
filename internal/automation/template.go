package automation

import (
	"strconv"
	"strings"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

// Snippet placeholders (AUTO-3; the same grammar is implemented in web/src/features/automation/template.ts):
//
//	{{name}}            ask for a value
//	{{name|default}}    ask, pre-filled with a default (a default cannot contain "|")
//	{{name|a|b|c}}      ask with a list of choices (the first one is the default)
//	{{name:secret}}     masked input (never stored)
//	{{host}} {{port}} {{user}} {{title}} {{protocol}} {{date}} {{time}} {{datetime}} {{timestamp}} {{clipboard}}
//	                    built-ins filled automatically (per target session)
//	\{{                 a literal "{{"
//
// Names are [A-Za-z_][A-Za-z0-9_-]*. Anything else between braces (e.g. Go templates "{{.State.Status}}") is kept
// literally.

// TemplateVar describes one placeholder of a template.
type TemplateVar struct {
	Name    string   `json:"name"`
	Default string   `json:"default,omitempty"`
	Choices []string `json:"choices,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
	Builtin bool     `json:"builtin,omitempty"`
}

// builtinVars are filled automatically and never prompted for.
var builtinVars = map[string]bool{
	"host": true, "port": true, "user": true, "title": true, "protocol": true,
	"date": true, "time": true, "datetime": true, "timestamp": true, "clipboard": true,
}

type tmplPart struct {
	text string
	v    *TemplateVar
}

func validVarName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '-')) {
			return false
		}
	}
	return true
}

// parsePlaceholder parses the inside of "{{…}}"; ok=false when it is not a placeholder.
func parsePlaceholder(inner string) (TemplateVar, bool) {
	parts := strings.Split(inner, "|")
	head := strings.TrimSpace(parts[0])
	secret := false
	if i := strings.IndexByte(head, ':'); i >= 0 {
		flag := strings.ToLower(strings.TrimSpace(head[i+1:]))
		if flag != "secret" && flag != "password" {
			return TemplateVar{}, false
		}
		secret = true
		head = strings.TrimSpace(head[:i])
	}
	if !validVarName(head) {
		return TemplateVar{}, false
	}
	v := TemplateVar{Name: head, Secret: secret, Builtin: builtinVars[head]}
	switch alts := parts[1:]; {
	case len(alts) == 1:
		v.Default = strings.TrimSpace(alts[0])
	case len(alts) > 1:
		for _, a := range alts {
			if a = strings.TrimSpace(a); a != "" {
				v.Choices = append(v.Choices, a)
			}
		}
		if len(v.Choices) > 0 {
			v.Default = v.Choices[0]
		}
	}
	return v, true
}

func parseTemplate(s string) []tmplPart {
	var parts []tmplPart
	var lit strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], `\{{`) {
			lit.WriteString("{{")
			i += 3
			continue
		}
		if strings.HasPrefix(s[i:], "{{") {
			end := strings.Index(s[i+2:], "}}")
			if end >= 0 && !strings.ContainsAny(s[i+2:i+2+end], "\r\n{") {
				if v, ok := parsePlaceholder(s[i+2 : i+2+end]); ok {
					if lit.Len() > 0 {
						parts = append(parts, tmplPart{text: lit.String()})
						lit.Reset()
					}
					vv := v
					parts = append(parts, tmplPart{v: &vv})
					i += 2 + end + 2
					continue
				}
			}
			lit.WriteString("{{")
			i += 2
			continue
		}
		lit.WriteByte(s[i])
		i++
	}
	if lit.Len() > 0 {
		parts = append(parts, tmplPart{text: lit.String()})
	}
	return parts
}

// TemplateVars lists the distinct placeholders of s in order of appearance (built-ins included, flagged).
func TemplateVars(s string) []TemplateVar {
	out := []TemplateVar{}
	seen := map[string]int{}
	for _, p := range parseTemplate(s) {
		if p.v == nil {
			continue
		}
		if i, ok := seen[p.v.Name]; ok {
			// Later occurrences may add a default / choices / the secret flag.
			cur := &out[i]
			if cur.Default == "" {
				cur.Default = p.v.Default
			}
			if len(cur.Choices) == 0 {
				cur.Choices = p.v.Choices
			}
			cur.Secret = cur.Secret || p.v.Secret
			continue
		}
		seen[p.v.Name] = len(out)
		out = append(out, *p.v)
	}
	return out
}

// RenderTemplate substitutes placeholders: explicit values first, then built-ins, then defaults. It returns the
// names of placeholders that got no value (they render as "").
func RenderTemplate(s string, values, builtins map[string]string) (string, []string) {
	var b strings.Builder
	var missing []string
	missed := map[string]bool{}
	for _, p := range parseTemplate(s) {
		if p.v == nil {
			b.WriteString(p.text)
			continue
		}
		if v, ok := values[p.v.Name]; ok {
			b.WriteString(v)
			continue
		}
		if p.v.Builtin {
			if v, ok := builtins[p.v.Name]; ok {
				b.WriteString(v)
				continue
			}
		}
		if p.v.Default != "" {
			b.WriteString(p.v.Default)
			continue
		}
		if !missed[p.v.Name] && !p.v.Builtin {
			missed[p.v.Name] = true
			missing = append(missing, p.v.Name)
		}
	}
	return b.String(), missing
}

// sessionBuiltins returns the built-in placeholder values for a target (conn may be nil).
func sessionBuiltins(info model.RuntimeSession, conn *model.Connection, t time.Time) map[string]string {
	m := map[string]string{
		"host":      info.Host,
		"user":      info.Username,
		"title":     info.Title,
		"protocol":  string(info.Protocol),
		"date":      t.Format("2006-01-02"),
		"time":      t.Format("15:04:05"),
		"datetime":  t.Format("2006-01-02 15:04:05"),
		"timestamp": strconv.FormatInt(t.Unix(), 10),
	}
	if conn != nil {
		if conn.Host != "" {
			m["host"] = conn.Host
		}
		if conn.Username != "" {
			m["user"] = conn.Username
		}
		port := conn.Port
		if port == 0 {
			port = model.DefaultPort(conn.Protocol)
		}
		if port > 0 {
			m["port"] = strconv.Itoa(port)
		}
		if m["title"] == "" {
			m["title"] = conn.Name
		}
	}
	return m
}

// connectionBuiltins is sessionBuiltins for a saved connection without a runtime session (batch exec).
func connectionBuiltins(conn *model.Connection, t time.Time) map[string]string {
	return sessionBuiltins(model.RuntimeSession{Title: conn.Name, Host: conn.Host, Username: conn.Username, Protocol: conn.Protocol}, conn, t)
}
