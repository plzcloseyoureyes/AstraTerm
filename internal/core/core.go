// Package core bundles the higher-level managers that feature modules receive in Mount (SPEC §10.1).
package core

import (
	"github.com/plzcloseyoureyes/astraterm/internal/sshx"
	"github.com/plzcloseyoureyes/astraterm/internal/term"
)

// Core holds the shared runtime managers built by internal/server.
type Core struct {
	Sessions *term.Manager
	SSH      *sshx.Pool
}
