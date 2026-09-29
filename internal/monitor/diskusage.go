package monitor

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/termstead/termstead/internal/httpx"
)

// DiskUsage is one level of the disk-usage drill-down (MON-3): the sizes of a directory's subdirectories on the same
// filesystem (`du -xk -d 1`), plus the files directly inside it.
type DiskUsage struct {
	Path    string    `json:"path"`
	Parent  string    `json:"parent,omitempty"`
	Total   int64     `json:"total"`
	Entries []DUEntry `json:"entries"`
	// Partial is set when some directories could not be read (permissions): sizes are then lower bounds.
	Partial bool   `json:"partial,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// DUEntry is a child directory (or the pseudo entry of the files directly in the directory).
type DUEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

const maxDUEntries = 1000

// validRemotePath checks an absolute POSIX path from a request (no control characters) and cleans it.
func validRemotePath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") || len(p) > 4096 {
		return "", httpx.BadRequest("an absolute path is required")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", httpx.BadRequest("invalid character in path")
		}
	}
	return path.Clean(p), nil
}

func (s *Service) diskUsage(ctx context.Context, t *target, dir string, sudo bool) (*DiskUsage, error) {
	if t.host.Platform == platWindows {
		return nil, httpx.NewError(422, "monitor_unavailable", "disk usage drill-down is not supported on Windows hosts")
	}
	dir, err := validRemotePath(dir)
	if err != nil {
		return nil, err
	}
	res, err := s.execArgv(ctx, t, []string{"du", "-xk", "-d", "1", "--", dir}, sudo)
	if err != nil {
		return nil, err
	}
	du := parseDU(res.Stdout, dir)
	if du.Total == 0 && len(du.Entries) == 0 {
		msg := res.errText()
		if permissionDenied(msg) {
			return nil, errPermission(orDefault(msg, "permission denied"))
		}
		return nil, httpx.NewError(422, "command_failed", orDefault(msg, "du returned no data"))
	}
	if !res.ok() || len(res.Stderr) > 0 {
		du.Partial = true
		du.Warning = clip(firstLine(res.errText()), 300)
	}
	return du, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseDU parses `du -xk -d 1 DIR` ("KiB<TAB>path" lines; DIR itself is the total).
func parseDU(out []byte, dir string) *DiskUsage {
	du := &DiskUsage{Path: dir, Entries: []DUEntry{}}
	if dir != "/" {
		du.Parent = path.Dir(dir)
	}
	var childSum int64
	for _, l := range splitLines(out) {
		size, p, ok := strings.Cut(strings.TrimRight(l, "\r"), "\t")
		if !ok || !isNum(strings.TrimSpace(size)) {
			continue
		}
		bytes := atoi64(size) * 1024
		p = path.Clean(p)
		if p == dir {
			du.Total = bytes
			continue
		}
		if path.Dir(p) != dir {
			continue
		}
		du.Entries = append(du.Entries, DUEntry{Name: path.Base(p), Path: p, Size: bytes, Dir: true})
		childSum += bytes
	}
	if files := du.Total - childSum; files > 0 {
		du.Entries = append(du.Entries, DUEntry{Name: "(files)", Path: dir, Size: files})
	}
	sort.SliceStable(du.Entries, func(i, j int) bool { return du.Entries[i].Size > du.Entries[j].Size })
	if len(du.Entries) > maxDUEntries {
		var rest int64
		for _, e := range du.Entries[maxDUEntries-1:] {
			rest += e.Size
		}
		du.Entries = append(du.Entries[:maxDUEntries-1], DUEntry{Name: "(other entries)", Path: dir, Size: rest})
	}
	return du
}
