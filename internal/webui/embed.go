// Package webui embeds the compiled React frontend (built by `make web` into ./dist). The large assets are embedded
// only gzip-compressed (./precompress, run by `make web`); internal/server/spa.go serves both forms.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded frontend rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
