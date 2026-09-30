package automation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// ---- snippets (SPEC §6.0 "Snippets & macros") ---------------------------------------------------------------------

type snippetInput struct {
	Name        *string   `json:"name"`
	Folder      *string   `json:"folder"`
	Description *string   `json:"description"`
	Content     *string   `json:"content"`
	Tags        *[]string `json:"tags"`
	SendMode    *string   `json:"sendMode"`
	Shortcut    *string   `json:"shortcut"`
}

func (in *snippetInput) apply(s *model.Snippet) error {
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		s.Name = n
	}
	if in.Folder != nil {
		f, err := cleanFolder(*in.Folder)
		if err != nil {
			return err
		}
		s.Folder = f
	}
	if in.Description != nil {
		if utf8.RuneCountInString(*in.Description) > maxDescriptionLen {
			return httpx.BadRequest("description is too long")
		}
		s.Description = *in.Description
	}
	if in.Content != nil {
		if len(*in.Content) > maxSnippetBytes {
			return httpx.BadRequest("content is too large")
		}
		if !utf8.ValidString(*in.Content) {
			return httpx.BadRequest("content must be valid UTF-8")
		}
		s.Content = *in.Content
	}
	if in.Tags != nil {
		tags, err := cleanTags(*in.Tags)
		if err != nil {
			return err
		}
		s.Tags = tags
	}
	if in.SendMode != nil {
		switch *in.SendMode {
		case model.SendModePaste, model.SendModeExecute:
			s.SendMode = *in.SendMode
		case "":
			s.SendMode = model.SendModePaste
		default:
			return httpx.BadRequest("sendMode must be paste or execute")
		}
	}
	if in.Shortcut != nil {
		sc := strings.TrimSpace(*in.Shortcut)
		if len(sc) > 64 {
			return httpx.BadRequest("shortcut is too long")
		}
		s.Shortcut = sc
	}
	return nil
}

func (m *Module) ownSnippet(ctx context.Context, user *model.User, id string) (*model.Snippet, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	s, err := m.d.Store.Snippets.Get(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if s.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return s, nil
}

func (m *Module) listSnippets(c *echo.Context) error {
	list, err := m.d.Store.Snippets.ListByOwner(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) getSnippet(c *echo.Context) error {
	s, err := m.ownSnippet(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

func (m *Module) createSnippet(c *echo.Context) error {
	var in snippetInput
	if err := httpx.BindLimit(c, &in, maxSnippetBytes+64<<10); err != nil {
		return err
	}
	user := httpx.UserFrom(c)
	s := &model.Snippet{OwnerID: user.ID, Tags: []string{}, SendMode: model.SendModePaste}
	if in.Name == nil {
		return httpx.BadRequest("name is required")
	}
	if err := in.apply(s); err != nil {
		return err
	}
	if err := m.d.Store.Snippets.Create(c.Request().Context(), s); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s)
}

func (m *Module) patchSnippet(c *echo.Context) error {
	var in snippetInput
	if err := httpx.BindLimit(c, &in, maxSnippetBytes+64<<10); err != nil {
		return err
	}
	ctx := c.Request().Context()
	s, err := m.ownSnippet(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := in.apply(s); err != nil {
		return err
	}
	if err := m.d.Store.Snippets.Update(ctx, s); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

func (m *Module) deleteSnippet(c *echo.Context) error {
	ctx := c.Request().Context()
	s, err := m.ownSnippet(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.d.Store.Snippets.Delete(ctx, s.ID); err != nil {
		return err
	}
	return httpx.OK(c)
}

type snippetRunRequest struct {
	SessionIDs       []string          `json:"sessionIds"`
	Variables        map[string]string `json:"variables"`
	SendMode         string            `json:"sendMode"`
	ConfirmDangerous bool              `json:"confirmDangerous"`
}

type sendResult struct {
	SessionID string `json:"sessionId"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

type sendResults struct {
	Results []sendResult `json:"results"`
}

// snippetText renders a snippet for one target and applies its send mode (execute = press Enter at the end).
func snippetText(content, mode string, values, builtins map[string]string) (string, []string) {
	text, missing := RenderTemplate(content, values, builtins)
	text = normalizeNewlines(text)
	if mode == model.SendModeExecute && !strings.HasSuffix(text, "\r") {
		text += "\r"
	}
	return text, missing
}

func (m *Module) runSnippet(c *echo.Context) error {
	var req snippetRunRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	snip, err := m.ownSnippet(ctx, user, c.Param("id"))
	if err != nil {
		return err
	}
	ids, err := uniqueIDs(req.SessionIDs, maxTargets, "session")
	if err != nil {
		return err
	}
	mode := snip.SendMode
	if req.SendMode != "" {
		if req.SendMode != model.SendModePaste && req.SendMode != model.SendModeExecute {
			return httpx.BadRequest("sendMode must be paste or execute")
		}
		mode = req.SendMode
	}
	if len(req.Variables) > 200 {
		return httpx.BadRequest("too many variables")
	}
	if _, missing := RenderTemplate(snip.Content, req.Variables, nil); len(missing) > 0 {
		return httpx.BadRequest("missing values for: " + strings.Join(missing, ", "))
	}
	type target struct {
		id   string
		text string
		err  error
	}
	t0 := time.Now()
	targets := make([]target, 0, len(ids))
	var all strings.Builder
	for _, id := range ids {
		s, err := m.ownSession(user, id)
		if err != nil {
			targets = append(targets, target{id: id, err: err})
			continue
		}
		text, _ := snippetText(snip.Content, mode, req.Variables, sessionBuiltins(s.Info(), s.Connection(), t0))
		targets = append(targets, target{id: id, text: text})
		all.WriteString(text)
		all.WriteByte('\n')
	}
	if !req.ConfirmDangerous {
		if hits := checkDangerous(all.String(), loadGuardConfig(ctx, m.d.Store, user.ID)); len(hits) > 0 {
			return refuseDangerous(c, hits)
		}
	}
	out := sendResults{Results: make([]sendResult, 0, len(targets))}
	sent := 0
	for _, t := range targets {
		r := sendResult{SessionID: t.id}
		if t.err == nil {
			t.err = m.write(t.id, t.text)
		}
		if t.err != nil {
			r.Error = errorText(t.err)
		} else {
			r.OK = true
			sent++
		}
		out.Results = append(out.Results, r)
	}
	m.audit(c, nil, "automation.snippet.run", snip.ID, map[string]any{"name": snip.Name, "sessions": sent,
		"dangerousConfirmed": req.ConfirmDangerous})
	return c.JSON(http.StatusOK, out)
}

// ---- macros -------------------------------------------------------------------------------------------------------

const (
	defaultMacroWait = 30 * time.Second
	maxMacroWait     = 10 * time.Minute
)

type macroInput struct {
	Name  *string      `json:"name"`
	Steps *[]MacroStep `json:"steps"`
}

// validateMacroSteps checks the limits and the wait patterns / secret names of macro steps.
func validateMacroSteps(steps []MacroStep) error {
	if len(steps) > maxMacroSteps {
		return httpx.BadRequest("too many steps")
	}
	total := 0
	for i, st := range steps {
		bad := func(msg string) error { return httpx.BadRequest(fmt.Sprintf("step %d: %s", i+1, msg)) }
		if len(st.Data) > maxMacroStepBytes {
			return bad("too large")
		}
		if !utf8.ValidString(st.Data) {
			return bad("not valid UTF-8")
		}
		if st.DelayMs < 0 || st.DelayMs > maxMacroDelayMs {
			return bad(fmt.Sprintf("delayMs must be between 0 and %d", maxMacroDelayMs))
		}
		if st.WaitFor != "" {
			if _, err := compilePattern(st.WaitFor); err != nil {
				return bad("invalid wait pattern: " + err.Error())
			}
		}
		if st.TimeoutMs < 0 || st.TimeoutMs > int(maxMacroWait/time.Millisecond) {
			return bad("timeoutMs is out of range")
		}
		if st.Secret != "" && !validSecretKey(st.Secret) {
			return bad("invalid secret name")
		}
		total += len(st.Data) + len(st.WaitFor)
	}
	if total > maxMacroBytes {
		return httpx.BadRequest("macro is too large")
	}
	return nil
}

func (in *macroInput) apply(mc *Macro) error {
	if in.Name != nil {
		n, err := cleanName(*in.Name, "name")
		if err != nil {
			return err
		}
		mc.Name = n
	}
	if in.Steps != nil {
		steps := *in.Steps
		if err := validateMacroSteps(steps); err != nil {
			return err
		}
		if steps == nil {
			steps = []MacroStep{}
		}
		mc.Steps = steps
	}
	return nil
}

func (m *Module) ownMacro(ctx context.Context, user *model.User, id string) (*Macro, error) {
	if !model.ValidID(id) {
		return nil, httpx.ErrNotFound
	}
	mc, err := m.repo.getMacro(ctx, id)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, httpx.ErrNotFound
		}
		return nil, err
	}
	if mc.OwnerID != user.ID {
		return nil, httpx.ErrNotFound
	}
	return mc, nil
}

func (m *Module) listMacros(c *echo.Context) error {
	list, err := m.repo.listMacros(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, list)
}

func (m *Module) getMacro(c *echo.Context) error {
	mc, err := m.ownMacro(c.Request().Context(), httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, mc)
}

func (m *Module) createMacro(c *echo.Context) error {
	var in macroInput
	if err := httpx.BindLimit(c, &in, 4*maxMacroBytes); err != nil {
		return err
	}
	if in.Name == nil {
		return httpx.BadRequest("name is required")
	}
	mc := &Macro{OwnerID: httpx.UserFrom(c).ID, Steps: []MacroStep{}}
	if err := in.apply(mc); err != nil {
		return err
	}
	if err := m.repo.createMacro(c.Request().Context(), mc); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, mc)
}

func (m *Module) patchMacro(c *echo.Context) error {
	var in macroInput
	if err := httpx.BindLimit(c, &in, 4*maxMacroBytes); err != nil {
		return err
	}
	ctx := c.Request().Context()
	mc, err := m.ownMacro(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := in.apply(mc); err != nil {
		return err
	}
	if err := m.repo.updateMacro(ctx, mc); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, mc)
}

func (m *Module) deleteMacro(c *echo.Context) error {
	ctx := c.Request().Context()
	mc, err := m.ownMacro(ctx, httpx.UserFrom(c), c.Param("id"))
	if err != nil {
		return err
	}
	if err := m.repo.deleteMacro(ctx, mc.ID); err != nil {
		return err
	}
	return httpx.OK(c)
}

type macroRunRequest struct {
	SessionIDs       []string `json:"sessionIds"`
	Speed            *float64 `json:"speed"`
	ConfirmDangerous bool     `json:"confirmDangerous"`
}

type jobStarted struct {
	JobID string `json:"jobId"`
	RunID string `json:"runId,omitempty"`
}

// progressEvent is the job data of macro replays and paced sends.
type progressEvent struct {
	Kind      string `json:"kind"` // progress | done | warning
	SessionID string `json:"sessionId"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	OK        bool   `json:"ok,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
}

// macroTypedText is what a macro types, for the dangerous-command guard (stored secrets are not known here and are
// never commands).
func macroTypedText(steps []MacroStep) string {
	var b strings.Builder
	for _, st := range steps {
		if st.Secret != "" {
			b.WriteString("\x15") // the secret is a line of its own: nothing typed before it forms a command with it
		}
		b.WriteString(st.Data)
	}
	return typedText(b.String())
}

// runMacro replays a macro with its recorded timing (scaled by speed; 0 = no delays) on several sessions. Replays
// run in the backend so they keep their timing in background tabs; waitFor steps read the session output.
func (m *Module) runMacro(c *echo.Context) error {
	var req macroRunRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	mc, err := m.ownMacro(ctx, user, c.Param("id"))
	if err != nil {
		return err
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
	speed := 1.0
	if req.Speed != nil {
		speed = *req.Speed
		if speed < 0 || speed > 100 {
			return httpx.BadRequest("speed must be between 0 and 100")
		}
	}
	if len(mc.Steps) == 0 {
		return httpx.BadRequest("the macro has no steps")
	}
	if !req.ConfirmDangerous {
		if hits := checkDangerous(macroTypedText(mc.Steps), loadGuardConfig(ctx, m.d.Store, user.ID)); len(hits) > 0 {
			return refuseDangerous(c, hits)
		}
	}
	steps := append([]MacroStep(nil), mc.Steps...)
	jobID := m.d.Jobs.Start(user, "macro: "+mc.Name, func(jctx context.Context, emit func(any)) error {
		return m.replayMacro(jctx, user, ids, steps, speed, emit)
	})
	m.audit(c, nil, "automation.macro.run", mc.ID, map[string]any{"name": mc.Name, "sessions": len(ids),
		"dangerousConfirmed": req.ConfirmDangerous})
	return c.JSON(http.StatusOK, jobStarted{JobID: jobID})
}

func (m *Module) replayMacro(ctx context.Context, user *model.User, ids []string, steps []MacroStep, speed float64, emit func(any)) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for _, id := range ids {
		wg.Go(func() {
			err := m.replayOne(ctx, user, id, steps, speed, emit)
			ev := progressEvent{Kind: "done", SessionID: id, Done: len(steps), Total: len(steps), OK: err == nil}
			if err != nil {
				ev.Error = errorText(err)
				mu.Lock()
				failed++
				mu.Unlock()
			}
			emit(ev)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed == len(ids) {
		return fmt.Errorf("the macro could not be replayed on any session")
	}
	return nil
}

func (m *Module) replayOne(ctx context.Context, user *model.User, id string, steps []MacroStep, speed float64, emit func(any)) error {
	var (
		r       *reader
		secrets = map[string]string{}
	)
	for _, st := range steps {
		if st.WaitFor != "" {
			s, err := m.ownSession(user, id)
			if err != nil {
				return err
			}
			t := m.acquireTap(s)
			defer t.release()
			// From the start of the current line: a prompt that is already displayed satisfies the first wait.
			r = t.newReader(true)
			break
		}
	}
	lastEmit := time.Time{}
	for i, st := range steps {
		if st.DelayMs > 0 && speed > 0 {
			d := time.Duration(float64(st.DelayMs)/speed) * time.Millisecond
			if err := sleepCtx(ctx, min(d, 10*time.Minute)); err != nil {
				return err
			}
		} else if err := ctx.Err(); err != nil {
			return err
		}
		if st.WaitFor != "" && r != nil {
			re, err := compilePattern(st.WaitFor)
			if err != nil {
				return fmt.Errorf("step %d: %v", i+1, err)
			}
			timeout := defaultMacroWait
			if st.TimeoutMs > 0 {
				timeout = min(time.Duration(st.TimeoutMs)*time.Millisecond, maxMacroWait)
			}
			if _, err := r.expect(ctx, []*regexp.Regexp{re}, timeout); err != nil {
				if errors.Is(err, errExpectTimeout) {
					return fmt.Errorf("step %d: %q did not appear within %s", i+1, st.WaitFor, timeout)
				}
				return err
			}
		}
		if st.Secret != "" {
			v, ok := secrets[st.Secret]
			if !ok {
				s, err := m.ownSession(user, id)
				if err != nil {
					return err
				}
				sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				v, err = m.sessionSecret(sctx, user, s, st.Secret)
				cancel()
				if err != nil {
					return fmt.Errorf("step %d: %s", i+1, errorText(err))
				}
				secrets[st.Secret] = v
			}
			// The secret goes on its own, so observers only ever see it masked; the step's data (usually Enter)
			// follows as ordinary input.
			if err := m.writeSecret(id, v, false); err != nil {
				return err
			}
		}
		if st.Data != "" {
			if err := m.write(id, st.Data); err != nil {
				return err
			}
		}
		if time.Since(lastEmit) >= 150*time.Millisecond {
			lastEmit = time.Now()
			emit(progressEvent{Kind: "progress", SessionID: id, Done: i + 1, Total: len(steps)})
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
