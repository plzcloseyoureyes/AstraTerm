package vfs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// Shell helpers for exec-capable (SSH) file systems. EVERY path or user value that reaches a command line goes
// through shq; paths are absolute (never start with "-") and commands use "--" where the tool supports it.

// shq quotes s as one POSIX shell word (single quotes; embedded quotes as '\”).
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxShellOutput bounds captured command output.
const maxShellOutput = 32 << 20

// capWriter is a bytes.Buffer that silently drops data beyond max.
type capWriter struct {
	bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.Len(); room < len(p) {
		if room > 0 {
			w.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return w.Buffer.Write(p)
}

// runShell runs cmd and returns its stdout; a non-zero exit status becomes an error derived from stderr.
func runShell(ctx context.Context, x Execer, cmd string, stdin io.Reader) ([]byte, error) {
	out := &capWriter{max: maxShellOutput}
	stderr, code, err := x.Exec(ctx, cmd, stdin, out)
	if err != nil {
		return out.Bytes(), err
	}
	if code != 0 {
		return out.Bytes(), shellError(stderr, code, "command")
	}
	return out.Bytes(), nil
}

// ---- checksums ----------------------------------------------------------------------------------------------------

// checksumTools lists, per algorithm, the commands tried in order (GNU/busybox, BSD/macOS, OpenSSL).
var checksumTools = map[string][]string{
	"md5":    {"md5sum --", "md5 -r", "openssl dgst -md5 -r"},
	"sha1":   {"sha1sum --", "shasum -a 1 --", "openssl dgst -sha1 -r"},
	"sha256": {"sha256sum --", "shasum -a 256 --", "openssl dgst -sha256 -r"},
	"sha512": {"sha512sum --", "shasum -a 512 --", "openssl dgst -sha512 -r"},
}

var hashLen = map[string]int{"md5": 32, "sha1": 40, "sha256": 64, "sha512": 128}

// execChecksum hashes p on the remote host with the first available tool.
func execChecksum(ctx context.Context, x Execer, p, algo string) (string, error) {
	tools, ok := checksumTools[algo]
	if !ok {
		return "", fmt.Errorf("unsupported algorithm %q", algo)
	}
	var b strings.Builder
	for i, t := range tools {
		bin := strings.Fields(t)[0]
		if i == 0 {
			b.WriteString("if ")
		} else {
			b.WriteString("elif ")
		}
		fmt.Fprintf(&b, "command -v %s >/dev/null 2>&1; then %s %s; ", bin, t, shq(p))
	}
	b.WriteString("else echo 'no checksum tool' >&2; exit 127; fi")
	out, err := runShell(ctx, x, b.String(), nil)
	if err != nil {
		return "", err
	}
	return parseHashOutput(out, algo)
}

func parseHashOutput(out []byte, algo string) (string, error) {
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("checksum tool printed nothing")
	}
	h := strings.ToLower(strings.TrimPrefix(f[0], `\`))
	if len(h) != hashLen[algo] {
		return "", fmt.Errorf("unexpected checksum output %q", cleanMsg(firstLine(string(out))))
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", fmt.Errorf("unexpected checksum output %q", cleanMsg(firstLine(string(out))))
	}
	return h, nil
}

// ---- search -------------------------------------------------------------------------------------------------------

// globPattern turns a user pattern into a find -iname glob: text without wildcards means "contains".
func globPattern(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "*"
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return "*" + pattern + "*"
	}
	return pattern
}

// execSearch runs find (and grep for content) under dir and returns matching paths (at most max, plus whether more
// existed). Output is streamed; the command is stopped once enough results arrived.
func execSearch(ctx context.Context, x Execer, dir, pattern, content string, max int) ([]string, bool, error) {
	glob := globPattern(pattern)
	var cmd string
	sep := byte(0)
	if content == "" {
		cmd = fmt.Sprintf("find %s -xdev -mindepth 1 -iname %s -print0 2>/dev/null", shq(dir), shq(glob))
	} else {
		sep = '\n'
		cmd = fmt.Sprintf("find %s -xdev -type f -iname %s -exec grep -l -i -F -e %s -- {} + 2>/dev/null",
			shq(dir), shq(glob), shq(content))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	var results []string
	more := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReaderSize(pr, 64<<10)
		for {
			item, err := br.ReadString(sep)
			item = strings.TrimSuffix(item, string(sep))
			if item != "" && strings.HasPrefix(item, "/") {
				if len(results) >= max {
					more = true
					cancel()
					_, _ = io.Copy(io.Discard, br)
					return
				}
				results = append(results, path.Clean(item))
			}
			if err != nil {
				_, _ = io.Copy(io.Discard, br)
				return
			}
		}
	}()
	_, _, err := x.Exec(ctx, cmd, nil, pw)
	pw.Close()
	<-done
	if err != nil && !more && !errors.Is(err, context.Canceled) {
		return nil, false, err
	}
	if ctx.Err() != nil && !more {
		if perr := context.Cause(ctx); perr != nil && !errors.Is(perr, context.Canceled) {
			return nil, false, perr
		}
	}
	return results, more, nil
}

// ---- archives -----------------------------------------------------------------------------------------------------

// commonParent returns the deepest directory containing every path and the paths relative to it (as "./name").
func commonParent(paths []string) (string, []string) {
	parent := parentDir(paths[0])
	for _, p := range paths[1:] {
		for !isWithin(parentDir(p), parent) {
			parent = parentDir(parent)
		}
	}
	rel := make([]string, len(paths))
	for i, p := range paths {
		rel[i] = "./" + relTo(p, parent)
	}
	return parent, rel
}

// execArchive creates dest (zip or tar.gz) from paths with the remote zip / tar.
func execArchive(ctx context.Context, x Execer, paths []string, dest, format string) error {
	parent, rel := commonParent(paths)
	quoted := make([]string, len(rel))
	for i, r := range rel {
		quoted[i] = shq(r)
	}
	var cmd string
	switch format {
	case "zip":
		cmd = fmt.Sprintf("command -v zip >/dev/null 2>&1 || { echo 'zip: command not found' >&2; exit 127; }; cd %s && rm -f %s && zip -q -r -y %s %s",
			shq(parent), shq(dest), shq(dest), strings.Join(quoted, " "))
	case "tar.gz", "tgz":
		cmd = fmt.Sprintf("cd %s && tar -czf %s %s", shq(parent), shq(dest), strings.Join(quoted, " "))
	case "tar":
		cmd = fmt.Sprintf("cd %s && tar -cf %s %s", shq(parent), shq(dest), strings.Join(quoted, " "))
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
	_, err := runShell(ctx, x, cmd, nil)
	return err
}

// archiveKind classifies an archive by file name.
func archiveKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"), strings.HasSuffix(n, ".jar"), strings.HasSuffix(n, ".war"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(n, ".tar.bz2"), strings.HasSuffix(n, ".tbz2"), strings.HasSuffix(n, ".tbz"):
		return "tar.bz2"
	case strings.HasSuffix(n, ".tar.xz"), strings.HasSuffix(n, ".txz"):
		return "tar.xz"
	case strings.HasSuffix(n, ".tar.zst"), strings.HasSuffix(n, ".tzst"):
		return "tar.zst"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	case strings.HasSuffix(n, ".gz"):
		return "gz"
	case strings.HasSuffix(n, ".bz2"):
		return "bz2"
	case strings.HasSuffix(n, ".xz"):
		return "xz"
	case strings.HasSuffix(n, ".7z"):
		return "7z"
	case strings.HasSuffix(n, ".rar"):
		return "rar"
	}
	return ""
}

// stripCompressExt removes a single-file compression suffix (.gz, .bz2, .xz).
func stripCompressExt(name string) string {
	for _, ext := range []string{".gz", ".bz2", ".xz"} {
		if strings.HasSuffix(strings.ToLower(name), ext) && len(name) > len(ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name + ".out"
}

// execExtract unpacks archive into destDir with the remote tools.
func execExtract(ctx context.Context, x Execer, archive, destDir string) error {
	a, d := shq(archive), shq(destDir)
	var cmd string
	switch archiveKind(archive) {
	case "zip":
		cmd = fmt.Sprintf("unzip -o -q %s -d %s", a, d)
	case "tar.gz":
		cmd = fmt.Sprintf("tar -xzf %s -C %s", a, d)
	case "tar.bz2":
		cmd = fmt.Sprintf("tar -xjf %s -C %s", a, d)
	case "tar.xz":
		cmd = fmt.Sprintf("tar -xJf %s -C %s", a, d)
	case "tar.zst":
		cmd = fmt.Sprintf("zstd -dc %s | tar -xf - -C %s", a, d)
	case "tar":
		cmd = fmt.Sprintf("tar -xf %s -C %s", a, d)
	case "gz":
		cmd = fmt.Sprintf("gzip -dc %s > %s", a, shq(joinPath(destDir, stripCompressExt(baseName(archive)))))
	case "bz2":
		cmd = fmt.Sprintf("bzip2 -dc %s > %s", a, shq(joinPath(destDir, stripCompressExt(baseName(archive)))))
	case "xz":
		cmd = fmt.Sprintf("xz -dc %s > %s", a, shq(joinPath(destDir, stripCompressExt(baseName(archive)))))
	case "7z":
		cmd = fmt.Sprintf("7z x -y -o%s %s >/dev/null", d, a)
	case "rar":
		cmd = fmt.Sprintf("unrar x -o+ -idq %s %s", a, shq(destDir+"/"))
	default:
		return fmt.Errorf("unknown archive type: %s", baseName(archive))
	}
	_, err := runShell(ctx, x, "mkdir -p -- "+d+" && "+cmd, nil)
	return err
}

// ---- owner names (getent) -----------------------------------------------------------------------------------------

// execOwnerNames resolves uids / gids to names with getent (falling back to /etc/passwd and /etc/group).
func execOwnerNames(ctx context.Context, x Execer, uids, gids []int) (users, groups map[int]string, err error) {
	users, groups = map[int]string{}, map[int]string{}
	join := func(ids []int) (string, string) {
		s := make([]string, len(ids))
		for i, id := range ids {
			s[i] = strconv.Itoa(id)
		}
		return strings.Join(s, " "), strings.Join(s, "|")
	}
	var b strings.Builder
	if len(uids) > 0 {
		sp, alt := join(uids)
		fmt.Fprintf(&b, "getent passwd %s 2>/dev/null || grep -E '^[^:]*:[^:]*:(%s):' /etc/passwd 2>/dev/null; ", sp, alt)
	}
	b.WriteString("echo '::astraterm-groups::'; ")
	if len(gids) > 0 {
		sp, alt := join(gids)
		fmt.Fprintf(&b, "getent group %s 2>/dev/null || grep -E '^[^:]*:[^:]*:(%s):' /etc/group 2>/dev/null; ", sp, alt)
	}
	b.WriteString("true")
	out, err := runShell(ctx, x, b.String(), nil)
	if err != nil {
		return users, groups, err
	}
	target := users
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "::astraterm-groups::" {
			target = groups
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		if id, err := strconv.Atoi(f[2]); err == nil {
			if _, dup := target[id]; !dup {
				target[id] = f[0]
			}
		}
	}
	return users, groups, nil
}

// ---- stat output parsing (shell driver) ---------------------------------------------------------------------------

// statFormat is the GNU/busybox stat -c format parsed by parseStatLine: hex mode/size/mtime/uid/gid/user/group/name.
const statFormat = "%f/%s/%Y/%u/%g/%U/%G/%n"

// parseStatLine parses one line of `stat -c statFormat`. The name field is everything after the 7th slash.
func parseStatLine(line string) (*Entry, error) {
	f := strings.SplitN(line, "/", 8)
	if len(f) != 8 {
		return nil, fmt.Errorf("unexpected stat output")
	}
	mode, err := strconv.ParseUint(f[0], 16, 32)
	if err != nil {
		return nil, fmt.Errorf("unexpected stat mode %q", f[0])
	}
	size, _ := strconv.ParseInt(f[1], 10, 64)
	mt, _ := strconv.ParseInt(f[2], 10, 64)
	uid, _ := strconv.Atoi(f[3])
	gid, _ := strconv.Atoi(f[4])
	e := &Entry{
		Path:  f[7],
		Name:  path.Base(f[7]),
		Type:  typeFromMode(uint32(mode)),
		Size:  size,
		Mode:  uint32(mode),
		Mtime: time.Unix(mt, 0).UTC(),
		UID:   intPtr(uid),
		GID:   intPtr(gid),
	}
	if f[5] != "" && f[5] != "UNKNOWN" && f[5] != f[3] {
		e.Owner = f[5]
	}
	if f[6] != "" && f[6] != "UNKNOWN" && f[6] != f[4] {
		e.Group = f[6]
	}
	return e, nil
}
