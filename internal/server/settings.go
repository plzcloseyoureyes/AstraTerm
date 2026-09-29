package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
	"github.com/plzcloseyoureyes/astraterm/internal/store"
	"github.com/plzcloseyoureyes/astraterm/internal/vault"
)

const (
	maxSettingsBody = 1 << 20
	maxSettingKeys  = 1000
	maxSettingKey   = 128
)

// ---- settings -----------------------------------------------------------------------------------------------------

func (s *Server) mountSettings() {
	api, admin := s.Deps.Router.API(), s.Deps.Router.Admin()
	api.GET("/settings", s.getSettings)
	api.PUT("/settings", s.putSettings)
	admin.GET("/admin/settings", s.getGlobalSettings)
	admin.PUT("/admin/settings", s.putGlobalSettings)
}

func (s *Server) loadScope(ctx context.Context, scope string) (map[string]any, error) {
	raw, err := s.Deps.Store.Settings.All(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			continue // skip corrupt values rather than failing the whole settings object
		}
		out[k] = val
	}
	return out, nil
}

// effectiveSettings returns user settings deep-merged over global settings.
func (s *Server) effectiveSettings(ctx context.Context, userID string) (model.Settings, error) {
	global, err := s.loadScope(ctx, store.ScopeGlobal)
	if err != nil {
		return nil, err
	}
	user, err := s.loadScope(ctx, userID)
	if err != nil {
		return nil, err
	}
	merged, _ := mergePatch(global, user).(map[string]any)
	if merged == nil {
		merged = map[string]any{}
	}
	return model.Settings(merged), nil
}

func (s *Server) getSettings(c *echo.Context) error {
	m, err := s.effectiveSettings(c.Request().Context(), httpx.UserFrom(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, m)
}

func (s *Server) putSettings(c *echo.Context) error {
	u := httpx.UserFrom(c)
	if err := s.patchScope(c, u.ID); err != nil {
		return err
	}
	m, err := s.effectiveSettings(c.Request().Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, m)
}

func (s *Server) getGlobalSettings(c *echo.Context) error {
	m, err := s.loadScope(c.Request().Context(), store.ScopeGlobal)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, model.Settings(m))
}

func (s *Server) putGlobalSettings(c *echo.Context) error {
	if err := s.patchScope(c, store.ScopeGlobal); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "admin.settings.update", store.ScopeGlobal, nil)
	m, err := s.loadScope(c.Request().Context(), store.ScopeGlobal)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, model.Settings(m))
}

// patchScope applies a JSON merge patch (RFC 7396) to the settings of scope: null deletes, objects merge deeply,
// everything else replaces. The read-merge-write runs in one transaction, so concurrent patches (several tabs or
// devices saving at once) cannot lose each other's changes.
func (s *Server) patchScope(c *echo.Context, scope string) error {
	var p map[string]json.RawMessage
	if err := httpx.BindLimit(c, &p, maxSettingsBody); err != nil {
		return err
	}
	if p == nil {
		return httpx.BadRequest("settings must be a JSON object")
	}
	if len(p) > maxSettingKeys {
		return httpx.BadRequest("too many settings keys")
	}
	patch := make(map[string]any, len(p))
	for k, raw := range p {
		if k == "" || len(k) > maxSettingKey {
			return httpx.BadRequest("invalid settings key")
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return httpx.BadRequest("invalid value for setting " + k)
		}
		patch[k] = v
	}
	return s.Deps.Store.Settings.Update(c.Request().Context(), scope, func(cur map[string]json.RawMessage) (map[string]json.RawMessage, []string, error) {
		if len(cur)+len(patch) > maxSettingKeys*2 {
			return nil, nil, httpx.BadRequest("too many settings keys")
		}
		set := map[string]json.RawMessage{}
		var del []string
		for k, v := range patch {
			if v == nil {
				del = append(del, k)
				continue
			}
			var existing any
			if raw, ok := cur[k]; ok {
				_ = json.Unmarshal(raw, &existing) // a corrupt stored value is simply replaced
			}
			b, err := json.Marshal(mergePatch(existing, v))
			if err != nil {
				return nil, nil, httpx.Internal(err)
			}
			set[k] = b
		}
		return set, del, nil
	})
}

// mergePatch implements RFC 7396 JSON merge patch on decoded JSON values.
func mergePatch(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	out := map[string]any{}
	if tm, ok := target.(map[string]any); ok {
		for k, v := range tm {
			out[k] = v
		}
	}
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

// ---- vault --------------------------------------------------------------------------------------------------------

type vaultStatus struct {
	Locked            bool `json:"locked"`
	HasMasterPassword bool `json:"hasMasterPassword"`
}

var errWrongMaster = httpx.NewError(http.StatusForbidden, "wrong_password", "wrong master password")

func (s *Server) mountVault() {
	api, admin := s.Deps.Router.API(), s.Deps.Router.Admin()
	api.GET("/vault/status", s.vaultStatus)
	admin.POST("/vault/unlock", s.unlockVault)
	admin.POST("/vault/lock", s.lockVault)
	admin.POST("/vault/master-password", s.setMasterPassword)
}

func (s *Server) vaultStatus(c *echo.Context) error {
	v := s.Deps.Vault
	return c.JSON(http.StatusOK, vaultStatus{Locked: v.Locked(), HasMasterPassword: v.HasMasterPassword()})
}

func (s *Server) unlockVault(c *echo.Context) error {
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if d := s.vaultL.wait(); d > 0 {
		return httpx.TooManyRequests("too many failed unlock attempts, try again later", int(d/time.Second)+1)
	}
	if err := s.Deps.Vault.Unlock(c.Request().Context(), req.Password); err != nil {
		if errors.Is(err, vault.ErrWrongPassword) {
			s.vaultL.fail()
			s.Deps.Audit.Log(c, "vault.unlock.failed", "", nil)
			return errWrongMaster
		}
		return err
	}
	s.vaultL.reset()
	s.Deps.Audit.Log(c, "vault.unlock", "", nil)
	return s.vaultStatus(c)
}

func (s *Server) lockVault(c *echo.Context) error {
	if err := s.Deps.Vault.Lock(); err != nil {
		return err
	}
	s.Deps.Audit.Log(c, "vault.lock", "", nil)
	return s.vaultStatus(c)
}

func (s *Server) setMasterPassword(c *echo.Context) error {
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	if d := s.vaultL.wait(); d > 0 {
		return httpx.TooManyRequests("too many failed attempts, try again later", int(d/time.Second)+1)
	}
	hadMaster := s.Deps.Vault.HasMasterPassword()
	if err := s.Deps.Vault.SetMasterPassword(c.Request().Context(), req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, vault.ErrWrongPassword) {
			s.vaultL.fail()
			s.Deps.Audit.Log(c, "vault.master_password.failed", "", nil)
			return errWrongMaster
		}
		return err
	}
	s.vaultL.reset()
	action := "vault.master_password.set"
	switch {
	case req.NewPassword == "":
		action = "vault.master_password.remove"
	case hadMaster:
		action = "vault.master_password.change"
	}
	s.Deps.Audit.Log(c, action, "", nil)
	s.Deps.Events.Broadcast(vaultEvent{Type: model.EvVault, Locked: s.Deps.Vault.Locked()})
	return s.vaultStatus(c)
}
