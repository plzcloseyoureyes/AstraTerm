package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("export const termstead = 'workspace';\n", 200)
	files := map[string]string{
		"index.html":          "<!doctype html>" + strings.Repeat(" ", 4096), // top level: never touched
		"assets/app-1.js":     big,
		"assets/small-1.js":   "export {}",               // below -min
		"assets/font-1.woff2": strings.Repeat("x", 4096), // not compressible by type
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(dir, 1024); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "assets/small-1.js", "assets/font-1.woff2", "assets/app-1.js.gz"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "assets/app-1.js")); !os.IsNotExist(err) {
		t.Fatal("the plain copy of a compressed asset must be removed")
	}
	first, _ := os.ReadFile(filepath.Join(dir, "assets/app-1.js.gz"))
	zr, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != big {
		t.Fatal("round trip mismatch")
	}
	if zr.Header.Name != "" || !zr.Header.ModTime.IsZero() {
		t.Fatalf("gzip header must be deterministic: %+v", zr.Header)
	}

	// Idempotent: a second run changes nothing.
	if err := run(dir, 1024); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "assets/app-1.js.gz")); !bytes.Equal(first, again) {
		t.Fatal("second run changed the output")
	}

	// A missing dist (frontend not built) is not an error.
	if err := run(filepath.Join(dir, "nope"), 1024); err != nil {
		t.Fatal(err)
	}
}
