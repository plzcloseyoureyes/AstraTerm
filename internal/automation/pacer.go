package automation

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
)

// Paced sending (TERM-17, AUTO-6, CC-8): text is typed line by line into one or more sessions with an optional
// per-line and per-character delay and/or waiting for the shell prompt before each next line (OSC 133 prompt marks
// when the shell has integration, else a prompt pattern once output settles). It runs in the backend so browser
// timer throttling in background tabs cannot disturb it, and progress streams as job events. Other modules may reuse
// it (e.g. "send text file" of term-transfer).

// defaultPromptPattern matches the end of a typical shell / network device prompt.
const defaultPromptPattern = `[$#%>❯»:\]]\s*$`

var defaultPromptRe = regexp.MustCompile(defaultPromptPattern)

type pacedSendRequest struct {
	SessionIDs       []string `json:"sessionIds"`
	Text             string   `json:"text"`
	LineDelayMs      int      `json:"lineDelayMs"`
	CharDelayMs      int      `json:"charDelayMs"`
	WaitPrompt       bool     `json:"waitPrompt"`
	PromptPattern    string   `json:"promptPattern"`
	PromptTimeoutMs  int      `json:"promptTimeoutMs"`
	Enter            *bool    `json:"enter"`
	ConfirmDangerous bool     `json:"confirmDangerous"`
}

type pacing struct {
	lineDelay     time.Duration
	charDelay     time.Duration
	waitPrompt    bool
	promptRe      *regexp.Regexp
	promptTimeout time.Duration
	enter         bool
}

func (m *Module) pacedSend(c *echo.Context) error {
	var req pacedSendRequest
	if err := httpx.BindLimit(c, &req, maxSendBytes+64<<10); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	if req.Text == "" {
		return httpx.BadRequest("text is required")
	}
	if len(req.Text) > maxSendBytes {
		return httpx.BadRequest("text is too large")
	}
	if !utf8.ValidString(req.Text) {
		return httpx.BadRequest("text must be valid UTF-8")
	}
	ids, err := uniqueIDs(req.SessionIDs, maxTargets, "session")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := m.ownSession(user, id); err != nil {
			return err
		}
	}
	p := pacing{
		lineDelay:     time.Duration(min(max(req.LineDelayMs, 0), 60000)) * time.Millisecond,
		charDelay:     time.Duration(min(max(req.CharDelayMs, 0), 5000)) * time.Millisecond,
		waitPrompt:    req.WaitPrompt,
		promptTimeout: time.Duration(min(max(req.PromptTimeoutMs, 0), 600000)) * time.Millisecond,
		enter:         req.Enter == nil || *req.Enter,
	}
	if p.promptTimeout == 0 {
		p.promptTimeout = 15 * time.Second
	}
	if req.WaitPrompt {
		pat := strings.TrimSpace(req.PromptPattern)
		if pat == "" {
			p.promptRe = defaultPromptRe
		} else {
			re, err := regexp.Compile(pat)
			if err != nil {
				return httpx.BadRequest("invalid prompt pattern: " + err.Error())
			}
			p.promptRe = re
		}
	}
	if !req.ConfirmDangerous {
		if hits := checkDangerous(req.Text, loadGuardConfig(ctx, m.d.Store, user.ID)); len(hits) > 0 {
			return refuseDangerous(c, hits)
		}
	}
	text := req.Text
	jobID := m.d.Jobs.Start(user, "send", func(jctx context.Context, emit func(any)) error {
		return m.sendPacedAll(jctx, ids, text, p, emit)
	})
	m.audit(c, nil, "automation.send", "", map[string]any{"sessions": len(ids), "bytes": len(text),
		"dangerousConfirmed": req.ConfirmDangerous})
	return c.JSON(http.StatusOK, jobStarted{JobID: jobID})
}

func (m *Module) sendPacedAll(ctx context.Context, ids []string, text string, p pacing, emit func(any)) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			total, err := m.sendPaced(ctx, id, text, p, func(done, total int) {
				emit(progressEvent{Kind: "progress", SessionID: id, Done: done, Total: total})
			}, func(msg string) {
				emit(progressEvent{Kind: "warning", SessionID: id, Message: msg})
			})
			ev := progressEvent{Kind: "done", SessionID: id, Done: total, Total: total, OK: err == nil}
			if err != nil {
				ev.Error = errorText(err)
				mu.Lock()
				failed++
				mu.Unlock()
			}
			emit(ev)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed == len(ids) {
		return fmt.Errorf("nothing could be sent")
	}
	return nil
}

// sendPaced types text into one session according to p. It returns the number of lines.
func (m *Module) sendPaced(ctx context.Context, id, text string, p pacing, progress func(done, total int), warn func(string)) (int, error) {
	lines := splitLines(text)
	total := len(lines)
	endsWithNewline := strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\r")
	var r *reader
	if p.waitPrompt {
		mgr := m.sessions()
		if mgr == nil {
			return total, errSessionClosed
		}
		s := mgr.Get(id)
		if s == nil {
			return total, errSessionClosed
		}
		t := m.acquireTap(s)
		defer t.release()
		r = t.newReader(false)
	}
	lastProgress := time.Time{}
	for i, line := range lines {
		last := i == total-1
		var seq int64
		if r != nil {
			seq = r.t.promptCount()
		}
		if p.charDelay > 0 {
			for _, ch := range line {
				if err := m.write(id, string(ch)); err != nil {
					return total, err
				}
				if err := sleepCtx(ctx, p.charDelay); err != nil {
					return total, err
				}
			}
		} else if line != "" {
			if err := m.write(id, line); err != nil {
				return total, err
			}
		}
		if !last || p.enter || endsWithNewline {
			if err := m.write(id, "\r"); err != nil {
				return total, err
			}
		}
		if time.Since(lastProgress) >= 100*time.Millisecond || last {
			lastProgress = time.Now()
			progress(i+1, total)
		}
		if last {
			break
		}
		if r != nil {
			ok, err := r.waitPrompt(ctx, seq, p.promptRe, 150*time.Millisecond, p.promptTimeout)
			if err != nil {
				return total, err
			}
			if !ok {
				warn(fmt.Sprintf("line %d: no prompt within %s, continuing", i+1, p.promptTimeout))
			}
		}
		if p.lineDelay > 0 {
			if err := sleepCtx(ctx, p.lineDelay); err != nil {
				return total, err
			}
		} else if err := ctx.Err(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---- helper endpoints ---------------------------------------------------------------------------------------------

type guardCheckRequest struct {
	Text  string `json:"text"`
	Typed bool   `json:"typed"` // interpret as keystrokes (macros)
}

type guardCheckResponse struct {
	Enabled bool          `json:"enabled"`
	Matches []DangerMatch `json:"matches"`
}

// guardCheck evaluates the dangerous-command guard with the caller's settings (the UI asks before sending).
func (m *Module) guardCheck(c *echo.Context) error {
	var req guardCheckRequest
	if err := httpx.BindLimit(c, &req, maxSendBytes+64<<10); err != nil {
		return err
	}
	cfg := loadGuardConfig(c.Request().Context(), m.d.Store, httpx.UserFrom(c).ID)
	text := req.Text
	if req.Typed {
		text = typedText(text)
	}
	hits := checkDangerous(text, cfg)
	if hits == nil {
		hits = []DangerMatch{}
	}
	return c.JSON(http.StatusOK, guardCheckResponse{Enabled: cfg.Enabled, Matches: hits})
}

type regexTestRequest struct {
	Pattern       string `json:"pattern"`
	CaseSensitive *bool  `json:"caseSensitive"`
	Text          string `json:"text"`
}

type regexMatch struct {
	Line   string   `json:"line"`
	Match  string   `json:"match"`
	Groups []string `json:"groups"`
}

type regexTestResponse struct {
	Valid   bool         `json:"valid"`
	Error   string       `json:"error,omitempty"`
	Matches []regexMatch `json:"matches"`
}

// regexTest validates a pattern with the backend's regex engine (RE2 — JavaScript look-arounds are not supported)
// and shows what it matches in sample text, line by line.
func (m *Module) regexTest(c *echo.Context) error {
	var req regexTestRequest
	if err := httpx.BindLimit(c, &req, 256<<10); err != nil {
		return err
	}
	resp := regexTestResponse{Matches: []regexMatch{}}
	pat := req.Pattern
	if req.CaseSensitive != nil && !*req.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := compilePattern(pat)
	if err != nil {
		resp.Error = err.Error()
		return c.JSON(http.StatusOK, resp)
	}
	resp.Valid = true
	for _, line := range splitLines(req.Text) {
		if len(resp.Matches) >= 100 {
			break
		}
		if sm := re.FindStringSubmatch(line); sm != nil {
			groups := sm[1:]
			if groups == nil {
				groups = []string{}
			}
			resp.Matches = append(resp.Matches, regexMatch{Line: truncateUTF8(line, 500), Match: sm[0], Groups: groups})
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// compilePattern compiles a user pattern with a size limit.
func compilePattern(p string) (*regexp.Regexp, error) {
	if strings.TrimSpace(strings.TrimPrefix(p, "(?i)")) == "" {
		return nil, fmt.Errorf("the pattern is empty")
	}
	if len(p) > 2000 {
		return nil, fmt.Errorf("the pattern is too long")
	}
	re, err := regexp.Compile(p)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "invalid or unsupported Perl syntax") {
			msg += " (look-ahead/look-behind and back-references are not supported)"
		}
		return nil, fmt.Errorf("%s", strings.TrimPrefix(msg, "error parsing regexp: "))
	}
	return re, nil
}

type templateVarsRequest struct {
	Content string `json:"content"`
}

// templateVars lists the placeholders of a snippet text (the browser has the same parser; this keeps API clients
// honest).
func (m *Module) templateVars(c *echo.Context) error {
	var req templateVarsRequest
	if err := httpx.BindLimit(c, &req, maxSnippetBytes+64<<10); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"variables": TemplateVars(req.Content)})
}
