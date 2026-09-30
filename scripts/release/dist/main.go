// Command dist cross-compiles AstraTerm for every release platform and packages reproducible archives plus a
// SHA256SUMS file. The release workflow (.github/workflows/release.yml) publishes exactly what it builds.
// It embeds whatever frontend is in internal/webui/dist, so build that first (`make release` does).
//
//	go run ./scripts/release/dist [-version v1.2.3] [-out dist] [-platforms linux/amd64,windows/arm64]
//
// Reproducibility: -trimpath, no VCS stamp, and one timestamp for the embedded build date and every archive entry,
// taken from SOURCE_DATE_EPOCH, else the last commit's time. The same commit, Go version and frontend build give
// byte-identical archives.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultPlatforms is the release matrix (keep in sync with Makefile PLATFORMS and
// scripts/release/notices). An optional third element is the ARM version (GOARM) for 32-bit ARM, e.g. linux/arm/7.
const defaultPlatforms = "darwin/amd64,darwin/arm64,linux/amd64,linux/arm64,linux/arm/7,windows/amd64,windows/arm64,freebsd/amd64"

// Files shipped next to the binaries in every archive (missing ones are skipped with a warning).
var extraFiles = []string{"README.md", "LICENSE", "CHANGELOG.md", "THIRD_PARTY_NOTICES.md"}

func main() {
	version := flag.String("version", "", "version to embed (default: git describe --tags --always --dirty, else \"dev\")")
	out := flag.String("out", "dist", "output directory (emptied first)")
	plats := flag.String("platforms", defaultPlatforms, "comma-separated GOOS/GOARCH[/GOARM] list")
	flag.Parse()

	if *version == "" {
		*version = gitOr("dev", "describe", "--tags", "--always", "--dirty")
	}
	commit := gitOr("", "rev-parse", "--verify", "-q", "HEAD")
	epoch, reproducible := buildEpoch()
	date := time.Unix(epoch, 0).UTC()
	if !reproducible {
		fmt.Fprintln(os.Stderr, "dist: no SOURCE_DATE_EPOCH and no git commit: using the current time (not reproducible)")
	}
	if _, err := os.Stat("internal/webui/dist/index.html"); err != nil {
		fmt.Fprintln(os.Stderr, "dist: warning: internal/webui/dist has no index.html; the binaries will not contain the web UI")
	}

	if err := os.RemoveAll(*out); err != nil {
		fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	var docs []entry
	for _, f := range extraFiles {
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(os.Stderr, "dist: warning: %s missing, not included in the archives\n", f)
			continue
		}
		docs = append(docs, entry{src: f, name: f, mode: 0o644})
	}
	appVersion := numericVersion(*version)
	ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
		*version, commit, date.Format(time.RFC3339))
	var archives []string
	for p := range strings.SplitSeq(*plats, ",") {
		parts := strings.Split(strings.TrimSpace(p), "/")
		if len(parts) < 2 || len(parts) > 3 {
			fail(fmt.Errorf("bad platform %q", p))
		}
		goos, goarch, goarm := parts[0], parts[1], ""
		archName := goarch
		if len(parts) == 3 {
			goarm = parts[2]
			archName += "v" + goarm
		}
		name := fmt.Sprintf("astraterm_%s_%s_%s", strings.TrimPrefix(*version, "v"), goos, archName)
		fmt.Printf("==> %s\n", p)
		dir := filepath.Join(*out, "build", goos+"_"+archName)
		exe := "astraterm"
		if goos == "windows" {
			exe += ".exe"
		}
		bin := filepath.Join(dir, exe)
		if err := build("./cmd/astraterm", bin, ldflags, goos, goarch, goarm, appVersion); err != nil {
			fail(fmt.Errorf("build %s: %w", p, err))
		}
		files := append([]entry{{src: bin, name: exe, mode: 0o755}}, docs...)
		var archive string
		var err error
		if goos == "windows" {
			archive, err = writeZip(filepath.Join(*out, name+".zip"), name, files, date)
		} else {
			archive, err = writeTarGz(filepath.Join(*out, name+".tar.gz"), name, files, date)
		}
		if err != nil {
			fail(err)
		}
		archives = append(archives, archive)
	}
	if err := writeSums(filepath.Join(*out, "SHA256SUMS"), archives); err != nil {
		fail(err)
	}
	fmt.Printf("dist: %d archives and SHA256SUMS in %s (version %s, commit %s, date %s, %s)\n",
		len(archives), *out, *version, orUnknown(commit), date.Format(time.RFC3339), runtime.Version())
}

// build compiles one package; Windows executables get their icon, version information and manifest from
// packaging/windows/winres.json (a temporary .syso resource file next to the package's sources). The desktop app is
// built separately (desktop/, Tauri) on each platform by the release workflow.
func build(pkg, out, ldflags, goos, goarch, goarm, appVersion string) error {
	if goos == "windows" {
		prefix := filepath.Join(pkg, "rsrc")
		res := exec.Command("go", "tool", "go-winres", "make", "--in", "packaging/windows/winres.json", "--out", prefix,
			"--arch", goarch, "--product-version", appVersion, "--file-version", appVersion)
		res.Stdout, res.Stderr = os.Stdout, os.Stderr
		if err := res.Run(); err != nil {
			return fmt.Errorf("windows resources: %w", err)
		}
		defer os.Remove(prefix + "_windows_" + goarch + ".syso")
	}
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOARM="+goarm, "GOFLAGS=-mod=readonly")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// numericVersion is the X.Y.Z part of a release version (v1.2.3-rc.1 → 1.2.3), as Windows version resources
// require; "0.0.0" for development builds.
func numericVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "0.0.0"
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return "0.0.0"
		}
	}
	return v
}

type entry struct {
	src, name string
	mode      int64
}

func writeTarGz(path, root string, files []entry, mtime time.Time) (string, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // zero header: no name, no time stamp
	tw := tar.NewWriter(zw)
	hdr := func(name string, mode int64, size int64, typ byte) *tar.Header {
		return &tar.Header{Name: name, Mode: mode, Size: size, Typeflag: typ, ModTime: mtime, Format: tar.FormatPAX,
			Uname: "", Gname: "", Uid: 0, Gid: 0}
	}
	if err := tw.WriteHeader(hdr(root+"/", 0o755, 0, tar.TypeDir)); err != nil {
		return "", err
	}
	for _, f := range sorted(files) {
		b, err := os.ReadFile(f.src)
		if err != nil {
			return "", err
		}
		if err := tw.WriteHeader(hdr(root+"/"+f.name, f.mode, int64(len(b)), tar.TypeReg)); err != nil {
			return "", err
		}
		if _, err := tw.Write(b); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeZip(path, root string, files []entry, mtime time.Time) (string, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range sorted(files) {
		b, err := os.ReadFile(f.src)
		if err != nil {
			return "", err
		}
		h := &zip.FileHeader{Name: root + "/" + f.name, Method: zip.Deflate, Modified: mtime}
		h.SetMode(os.FileMode(f.mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			return "", err
		}
		if _, err := w.Write(b); err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeSums writes a `sha256sum -c` / `shasum -a 256 -c` compatible file.
func writeSums(path string, files []string) error {
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, fh)
		fh.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h.Sum(nil)), filepath.Base(f))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func sorted(files []entry) []entry {
	s := append([]entry(nil), files...)
	sort.Slice(s, func(i, j int) bool { return s[i].name < s[j].name })
	return s
}

// buildEpoch returns SOURCE_DATE_EPOCH, else the last commit time, else now (false = not reproducible).
func buildEpoch() (int64, bool) {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
		fail(fmt.Errorf("SOURCE_DATE_EPOCH=%q is not a Unix time", v))
	}
	if v := gitOr("", "log", "-1", "--format=%ct"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, true
		}
	}
	return time.Now().Unix(), false
}

func gitOr(def string, args ...string) string {
	b, err := exec.Command("git", args...).Output()
	if s := strings.TrimSpace(string(b)); err == nil && s != "" {
		return s
	}
	return def
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dist:", err)
	os.Exit(1)
}
