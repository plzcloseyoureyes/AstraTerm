package auth

import (
	"context"
	"errors"
	"os/user"
	"strings"

	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// ensureLocalAccount replaces first-run setup in the desktop app (config.LocalAccount): with no users yet, it creates
// the administrator, named after the operating-system user. The app signs in with its launch token, so the account
// has no usable password until the user sets one (Settings → Security), like an account provisioned by an identity
// provider.
func (s *Service) ensureLocalAccount(ctx context.Context) error {
	n, err := s.d.Store.Users.Count(ctx)
	if err != nil || n > 0 {
		return err
	}
	hash, err := HashPassword(randomToken(32)) // unusable: nobody knows it
	if err != nil {
		return err
	}
	name := localUsername()
	u := &model.User{Username: name, DisplayName: cleanDisplayName("", name), Role: model.RoleAdmin}
	if err := s.d.Store.Users.CreateFirstAdmin(ctx, u, hash); err != nil {
		if errors.Is(err, model.ErrConflict) {
			return nil
		}
		return err
	}
	s.log.Info("created the local account", "username", name)
	return s.setPasswordState(ctx, u.ID, false)
}

// localUsername is the operating-system user name when it is a valid AstraTerm username, else "admin".
func localUsername() string {
	if u, err := user.Current(); err == nil {
		name := u.Username
		if i := strings.LastIndexAny(name, `\/`); i >= 0 { // Windows: DOMAIN\user
			name = name[i+1:]
		}
		if ValidateUsername(name) == nil {
			return name
		}
	}
	return "admin"
}
