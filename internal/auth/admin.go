package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/termstead/termstead/internal/httpx"
	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
)

// adminUser is a user as the administration UI sees it: the core User plus security metadata. The extra fields are
// additive, so clients reading only User keep working.
type adminUser struct {
	*model.User
	HasPassword  bool       `json:"hasPassword"`
	Passkeys     int        `json:"passkeys"`
	SSO          []string   `json:"sso"`
	Locked       bool       `json:"locked"`
	LockedUntil  *time.Time `json:"lockedUntil,omitempty"`
	FailedLogins int        `json:"failedLogins"`
	Sessions     int        `json:"sessions"`
}

func (s *Service) handleListUsers(c *echo.Context) error {
	ctx := c.Request().Context()
	users, err := s.d.Store.Users.List(ctx)
	if err != nil {
		return err
	}
	metas, err := s.allUserMeta(ctx)
	if err != nil {
		return err
	}
	var passkeys map[string]int
	if p := s.passkeyProvider(); p != nil {
		if passkeys, err = p.CountAll(ctx); err != nil {
			return err
		}
	}
	var sso map[string][]string
	if p := s.ssoProvider(); p != nil {
		if sso, err = p.LinkedProviders(ctx); err != nil {
			return err
		}
	}
	sessions, err := s.countSessions(ctx)
	if err != nil {
		return err
	}
	now := store.Now()
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		m, ok := metas[u.ID]
		if !ok {
			m = userMeta{PasswordSet: true}
		}
		au := adminUser{User: u, HasPassword: m.PasswordSet, Passkeys: passkeys[u.ID], SSO: sso[u.ID],
			FailedLogins: m.FailedLogins, Sessions: sessions[u.ID]}
		if au.SSO == nil {
			au.SSO = []string{}
		}
		if m.locked(now) {
			t := m.LockedUntil
			au.Locked, au.LockedUntil = true, &t
		}
		out = append(out, au)
	}
	return c.JSON(http.StatusOK, out)
}

// countSessions returns the number of live login sessions per user.
func (s *Service) countSessions(ctx context.Context) (map[string]int, error) {
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM auth_sessions WHERE expires_at > ? GROUP BY user_id`,
		store.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

type createUserRequest struct {
	Username    string     `json:"username"`
	Password    string     `json:"password"`
	DisplayName string     `json:"displayName"`
	Role        model.Role `json:"role"`
}

func (s *Service) handleCreateUser(c *echo.Context) error {
	var req createUserRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	req.Username = strings.TrimSpace(req.Username)
	if err := ValidateUsername(req.Username); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if err := s.Policy(c.Request().Context()).ValidatePasswordFor(req.Password, req.Username); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if req.Role == "" {
		req.Role = model.RoleUser
	}
	if !req.Role.Valid() {
		return httpx.BadRequest("role must be admin or user")
	}
	if req.Role == model.RoleAdmin {
		if err := s.RequireAdminSudo(c); err != nil {
			return err
		}
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return httpx.Internal(err)
	}
	u := &model.User{Username: req.Username, DisplayName: cleanDisplayName(req.DisplayName, req.Username), Role: req.Role}
	if err := s.d.Store.Users.Create(c.Request().Context(), u, hash); err != nil {
		if errors.Is(err, model.ErrConflict) {
			return httpx.Conflict("a user with this username already exists")
		}
		return err
	}
	s.d.Audit.Log(c, "admin.user.create", u.ID, map[string]string{"username": u.Username, "role": string(u.Role)})
	return c.JSON(http.StatusCreated, u)
}

// handleUpdateUser applies {displayName?, role?, disabled?, totpEnabled?: false (reset 2FA)}.
func (s *Service) handleUpdateUser(c *echo.Context) error {
	var patch struct {
		DisplayName *string     `json:"displayName"`
		Role        *model.Role `json:"role"`
		Disabled    *bool       `json:"disabled"`
		TOTPEnabled *bool       `json:"totpEnabled"`
	}
	if err := httpx.Bind(c, &patch); err != nil {
		return err
	}
	me := httpx.UserFrom(c)
	id := c.Param("id")
	ctx := c.Request().Context()

	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, err := s.d.Store.Users.Get(ctx, id)
	if err != nil {
		return err
	}
	wasActiveAdmin := u.Role == model.RoleAdmin && !u.Disabled
	changes := map[string]any{}
	if patch.DisplayName != nil {
		u.DisplayName = cleanDisplayName(*patch.DisplayName, u.Username)
		changes["displayName"] = u.DisplayName
	}
	if patch.Role != nil {
		if !patch.Role.Valid() {
			return httpx.BadRequest("role must be admin or user")
		}
		if *patch.Role != u.Role {
			if err := s.RequireAdminSudo(c); err != nil {
				return err
			}
		}
		u.Role = *patch.Role
		changes["role"] = u.Role
	}
	if patch.Disabled != nil {
		if *patch.Disabled && u.ID == me.ID {
			return httpx.Conflict("you cannot disable your own account")
		}
		u.Disabled = *patch.Disabled
		changes["disabled"] = u.Disabled
	}
	if patch.TOTPEnabled != nil && *patch.TOTPEnabled {
		return httpx.BadRequest("two-factor authentication can only be enabled by the user")
	}
	if patch.TOTPEnabled != nil && u.TOTPEnabled {
		if err := s.RequireAdminSudo(c); err != nil {
			return err
		}
	}
	if wasActiveAdmin && (u.Role != model.RoleAdmin || u.Disabled) {
		n, err := s.d.Store.Users.CountActiveAdmins(ctx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return httpx.Conflict("cannot demote or disable the last administrator")
		}
	}
	if err := s.d.Store.Users.Update(ctx, u); err != nil {
		return err
	}
	if patch.TOTPEnabled != nil && !*patch.TOTPEnabled && u.TOTPEnabled {
		if err := s.d.Store.Users.SetTOTP(ctx, u.ID, nil, false, nil); err != nil {
			return err
		}
		u.TOTPEnabled = false
		changes["totpReset"] = true
	}
	if u.Disabled {
		if err := s.d.Store.AuthSessions.DeleteAllForUser(ctx, u.ID, ""); err != nil {
			return err
		}
		s.d.Events.CloseUser(u.ID)
	}
	s.d.Audit.Log(c, "admin.user.update", u.ID, changes)
	return c.JSON(http.StatusOK, u)
}

func (s *Service) handleDeleteUser(c *echo.Context) error {
	me := httpx.UserFrom(c)
	id := c.Param("id")
	if id == me.ID {
		return httpx.Conflict("you cannot delete your own account")
	}
	ctx := c.Request().Context()
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, err := s.d.Store.Users.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.Role == model.RoleAdmin && !u.Disabled {
		n, err := s.d.Store.Users.CountActiveAdmins(ctx)
		if err != nil {
			return err
		}
		if n <= 1 {
			return httpx.Conflict("cannot delete the last administrator")
		}
	}
	if err := s.d.Store.Users.Delete(ctx, id); err != nil {
		return err
	}
	s.d.Events.CloseUser(id)
	s.d.Audit.Log(c, "admin.user.delete", id, map[string]string{"username": u.Username})
	return httpx.OK(c)
}

func (s *Service) handleResetPassword(c *echo.Context) error {
	var req passwordOnlyRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	id := c.Param("id")
	ctx := c.Request().Context()
	target, err := s.d.Store.Users.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.RequireAdminSudo(c); err != nil {
		return err
	}
	if err := s.Policy(ctx).ValidatePasswordFor(req.Password, target.Username); err != nil {
		return httpx.BadRequest(err.Error())
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return httpx.Internal(err)
	}
	if err := s.d.Store.Users.SetPassword(ctx, id, hash); err != nil {
		return err
	}
	if err := s.setPasswordState(ctx, id, true); err != nil {
		return err
	}
	if err := s.clearLoginFailures(ctx, id, true); err != nil {
		return err
	}
	// Force re-login everywhere, except the admin's own current session when resetting their own password.
	keep := ""
	if id == httpx.UserFrom(c).ID {
		keep = httpx.AuthInfoFrom(c).SessionID
	}
	if err := s.d.Store.AuthSessions.DeleteAllForUser(ctx, id, keep); err != nil {
		return err
	}
	s.closeOtherSockets(id, keep)
	s.d.Audit.Log(c, "admin.user.reset_password", id, nil)
	return httpx.OK(c)
}
