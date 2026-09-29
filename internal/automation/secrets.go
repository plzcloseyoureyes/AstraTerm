package automation

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/term"
)

// Secret injection (AUTO-9, TERM-33): the browser names a stored secret of the session's connection ("password",
// "sudoPassword", …) and the backend types it into the session. The value never reaches the browser; the audit log
// records the key only.

type injectRequest struct {
	Key   string `json:"key"`
	Enter *bool  `json:"enter"`
}

var errSecretNotFound = httpx.NewError(http.StatusNotFound, "secret_not_found", "no such secret is stored for this session")

// sessionSecret resolves one stored secret of a session for user (who must be allowed to use it, see secretAccess).
func (m *Module) sessionSecret(ctx context.Context, user *model.User, s *term.Session, key string) (string, error) {
	if !validSecretKey(key) {
		return "", httpx.BadRequest("invalid secret key")
	}
	if !secretAccess(user, s) {
		return "", httpx.Forbidden("stored secrets of a shared connection cannot be typed by other users")
	}
	_, secrets, err := s.Resolve(ctx)
	if err != nil {
		if errors.Is(err, model.ErrLocked) || errors.Is(err, httpx.ErrLocked) {
			return "", httpx.ErrLocked
		}
		return "", err
	}
	v, ok := secrets[key]
	if !ok || v == "" {
		return "", errSecretNotFound
	}
	return v, nil
}

func (m *Module) injectSecret(c *echo.Context) error {
	var req injectRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	s, err := m.ownSession(user, c.Param("id"))
	if err != nil {
		return err
	}
	if st, _ := s.State(); st != model.StateConnected {
		return httpx.Conflict("the session is not connected")
	}
	v, err := m.sessionSecret(ctx, user, s, req.Key)
	if err != nil {
		return err
	}
	if err := m.writeSecret(s.ID, v, req.Enter == nil || *req.Enter); err != nil {
		return err
	}
	m.audit(c, nil, "session.inject_secret", s.ID, map[string]any{"key": req.Key, "connectionId": s.Info().ConnectionID})
	return httpx.OK(c)
}

type secretKeysResponse struct {
	Keys       []string `json:"keys"`
	Injectable bool     `json:"injectable"`
	Locked     bool     `json:"locked,omitempty"`
}

// secretKeys lists the names of the secrets that could be typed into the session (for the "send password" menu and
// the password-prompt chip). While the vault is locked the stored names are still listed (locked: true).
func (m *Module) secretKeys(c *echo.Context) error {
	ctx, user := c.Request().Context(), httpx.UserFrom(c)
	s, err := m.ownSession(user, c.Param("id"))
	if err != nil {
		return err
	}
	resp := secretKeysResponse{Keys: []string{}, Injectable: secretAccess(user, s)}
	if !resp.Injectable {
		return c.JSON(http.StatusOK, resp)
	}
	_, secrets, err := s.Resolve(ctx)
	switch {
	case err == nil:
		for k, v := range secrets {
			if v != "" && validSecretKey(k) {
				resp.Keys = append(resp.Keys, k)
			}
		}
	case errors.Is(err, model.ErrLocked) || errors.Is(err, httpx.ErrLocked):
		resp.Locked = true
		if id := s.Info().ConnectionID; id != "" {
			if conn, err := m.d.Store.Connections.Get(ctx, id); err == nil {
				for _, k := range conn.SecretKeys {
					if validSecretKey(k) {
						resp.Keys = append(resp.Keys, k)
					}
				}
			}
		}
	default:
		return err
	}
	sort.Slice(resp.Keys, func(i, j int) bool {
		return secretRank(resp.Keys[i]) < secretRank(resp.Keys[j]) ||
			secretRank(resp.Keys[i]) == secretRank(resp.Keys[j]) && resp.Keys[i] < resp.Keys[j]
	})
	return c.JSON(http.StatusOK, resp)
}

// secretRank orders the typical login secrets first.
func secretRank(k string) int {
	switch k {
	case model.SecretPassword:
		return 0
	case model.SecretSudoPassword:
		return 1
	case "enablePassword":
		return 2
	}
	return 10
}
