// Command precompress replaces the large, compressible files of the built frontend with gzip-compressed copies, so
// the binary embeds only the compressed form (internal/server/spa.go serves it with Content-Encoding: gzip, or
// decompresses it for clients without gzip support). `make web` runs it after the Vite build:
//
//	go run ./internal/webui/precompress internal/webui/dist
//
// Only files under assets/ are touched (content-hashed names, served immutable); index.html and the other top-level
// files stay plain. A file is replaced by name.gz only when that saves at least 10%. The output is deterministic
// (no file name or time stamp in the gzip header), and running the command twice is harmless.
//
// gzip rather than brotli: every browser accepts gzip, also over plain HTTP (browsers advertise br only on HTTPS),
// and the standard library can decompress it, so no dependency is added.
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// compressible lists the extensions worth compressing (woff2, images and archives already are).
var compressible = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".html": true, ".svg": true, ".json": true, ".wasm": true,
	".ttf": true, ".otf": true, ".eot": true, ".txt": true, ".map": true, ".xml": true, ".ico": true,
}

func main() {
	minSize := flag.Int64("min", 1024, "leave files smaller than this many bytes uncompressed")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: precompress [-min bytes] <dist dir>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *minSize); err != nil {
		fmt.Fprintln(os.Stderr, "precompress:", err)
		os.Exit(1)
	}
}

func run(dist string, minSize int64) error {
	root := filepath.Join(dist, "assets")
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("precompress: %s does not exist (frontend not built), nothing to do\n", root)
		return nil
	}
	var files int
	var before, after int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !compressible[strings.ToLower(filepath.Ext(p))] {
			return err
		}
		info, err := d.Info()
		if err != nil || info.Size() < minSize {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // zero Header: no name, no mtime
		if _, err := zw.Write(src); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		if int64(buf.Len())*10 > int64(len(src))*9 {
			return nil // saves less than 10%: keep the plain file
		}
		if err := os.WriteFile(p+".gz", buf.Bytes(), 0o644); err != nil {
			return err
		}
		files++
		before += int64(len(src))
		after += int64(buf.Len())
		return os.Remove(p)
	})
	if err != nil {
		return err
	}
	fmt.Printf("precompress: %d files, %.1f MiB -> %.1f MiB\n", files, float64(before)/(1<<20), float64(after)/(1<<20))
	return nil
}
