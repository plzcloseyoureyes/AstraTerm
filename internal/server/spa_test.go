package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/labstack/echo/v5"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

func TestSPAPrecompressedAssets(t *testing.T) {
	js := strings.Repeat("console.log('nexterm');\n", 200)
	fsys := fstest.MapFS{
		"index.html":            {Data: []byte("<!doctype html><html></html>")},
		"boot.js":               {Data: []byte("/* boot */")},
		"assets/app-abc.js.gz":  {Data: gz(t, js)},
		"assets/font-abc.woff2": {Data: []byte("wOF2....")},
	}
	h := spaHandler(fsys)
	e := echo.New()
	do := func(method, target string, hdr map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		if err := h(e.NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// gzip-capable client: the stored bytes with Content-Encoding.
	rec := do("GET", "/assets/app-abc.js", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("gzip response: %d %v", rec.Code, rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != js {
		t.Fatal("gzip body does not decompress to the asset")
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the gzip response")
	}
	if rec := do("GET", "/assets/app-abc.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation (gzip): %d", rec.Code)
	}

	// Client without gzip: decompressed on the fly, own ETag, correct length.
	for _, ae := range []string{"", "identity", "gzip;q=0"} {
		rec = do("GET", "/assets/app-abc.js", map[string]string{"Accept-Encoding": ae})
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != js ||
			rec.Header().Get("Content-Length") != "4800" {
			t.Fatalf("identity response (%q): %d %v len=%d", ae, rec.Code, rec.Header(), rec.Body.Len())
		}
	}
	idTag := rec.Header().Get("ETag")
	if idTag == "" || idTag == etag {
		t.Fatalf("identity ETag %q must differ from the gzip one %q", idTag, etag)
	}
	if rec := do("GET", "/assets/app-abc.js", map[string]string{"If-None-Match": idTag}); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation (identity): %d", rec.Code)
	}
	if rec := do("HEAD", "/assets/app-abc.js", nil); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d len=%d", rec.Code, rec.Body.Len())
	}

	// Plain files keep working: range requests, no-cache for top-level files, 404 for missing assets, SPA fallback.
	rec = do("GET", "/assets/font-abc.woff2", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-3"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "wOF2" || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain asset range: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do("GET", "/boot.js", nil); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") == "" {
		t.Fatalf("top-level file: %d %v", rec.Code, rec.Header())
	}
	if rec := do("GET", "/assets/missing.js", map[string]string{"Accept-Encoding": "gzip"}); rec.Code != 404 {
		t.Fatalf("missing asset: %d", rec.Code)
	}
	if rec := do("GET", "/sessions/xyz", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<html>") ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("SPA fallback: %d", rec.Code)
	}
}
