package vfs

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Folder compare (FILE-15, MobaFoldersDiff): a dual walk over any two handles (same or different file systems)
// comparing by size + mtime or by checksum.

type compareSide struct {
	FsID string `json:"fsId"`
	Path string `json:"path"`
}

type compareRequest struct {
	Left      compareSide `json:"left"`
	Right     compareSide `json:"right"`
	Recursive bool        `json:"recursive"`
	Mode      string      `json:"mode"`
	Excludes  []string    `json:"excludes"`
	MaxItems  int         `json:"maxItems"`
}

// CompareItem is one difference (or equality) found by a folder compare.
type CompareItem struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Left   *Entry `json:"left,omitempty"`
	Right  *Entry `json:"right,omitempty"`
}

// CompareSummary counts items by status.
type CompareSummary struct {
	Same         int `json:"same"`
	Different    int `json:"different"`
	LeftOnly     int `json:"leftOnly"`
	RightOnly    int `json:"rightOnly"`
	TypeMismatch int `json:"typeMismatch"`
}

// CompareResult is the reply of POST /api/fs/compare.
type CompareResult struct {
	Items     []*CompareItem `json:"items"`
	Truncated bool           `json:"truncated"`
	Summary   CompareSummary `json:"summary"`
}

// Compare statuses.
const (
	cmpSame         = "same"
	cmpDifferent    = "different"
	cmpLeftOnly     = "left-only"
	cmpRightOnly    = "right-only"
	cmpTypeMismatch = "type-mismatch"
)

// mtimeTolerance absorbs FAT / FTP timestamp granularity.
const mtimeTolerance = 2 * time.Second

func (h *handler) compare(c *echo.Context) error {
	var req compareRequest
	if err := httpx.Bind(c, &req); err != nil {
		return err
	}
	user := httpx.UserFrom(c)
	lh, lrel, err := h.reg.Acquire(user, req.Left.FsID)
	if err != nil {
		return err
	}
	defer lrel()
	rh, rrel, err := h.reg.Acquire(user, req.Right.FsID)
	if err != nil {
		return err
	}
	defer rrel()
	lp, err := requirePath(lh, req.Left.Path, "left.path")
	if err != nil {
		return err
	}
	rp, err := requirePath(rh, req.Right.Path, "right.path")
	if err != nil {
		return err
	}
	mode := strings.ToLower(req.Mode)
	switch mode {
	case "", "size-mtime", "sizemtime":
		mode = "size-mtime"
	case "checksum", "hash", "size":
	default:
		return httpx.BadRequest("mode must be size-mtime or checksum")
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Minute)
	defer cancel()
	for _, s := range []struct {
		fsys FS
		p    string
	}{{lh.FS, lp}, {rh.FS, rp}} {
		e, err := s.fsys.Stat(ctx, s.p)
		if err != nil {
			return fsError(err, s.p)
		}
		if e.Type != model.FileTypeDir {
			return httpx.NewError(http.StatusBadRequest, codeFSError, "not a folder: "+s.p)
		}
	}
	res, err := compareTrees(ctx, lh.FS, lp, rh.FS, rp, compareOptions{Recursive: req.Recursive, Mode: mode,
		Excludes: req.Excludes, MaxItems: req.MaxItems})
	if err != nil {
		return fsError(err, lp)
	}
	return c.JSON(http.StatusOK, res)
}

type compareOptions struct {
	Recursive bool
	Mode      string
	Excludes  []string
	MaxItems  int
}

// compareTrees compares two folders (possibly on different file systems).
func compareTrees(ctx context.Context, lfs FS, lp string, rfs FS, rp string, o compareOptions) (*CompareResult, error) {
	if o.MaxItems <= 0 || o.MaxItems > 200000 {
		o.MaxItems = 50000
	}
	cmp := &comparer{ctx: ctx, lfs: lfs, rfs: rfs, o: o, res: &CompareResult{Items: []*CompareItem{}}}
	if err := cmp.dir(lp, rp, ""); err != nil && !errors.Is(err, errTruncated) {
		return nil, err
	}
	if err := cmp.hashPending(); err != nil {
		return nil, err
	}
	for _, it := range cmp.res.Items {
		switch it.Status {
		case cmpSame:
			cmp.res.Summary.Same++
		case cmpDifferent:
			cmp.res.Summary.Different++
		case cmpLeftOnly:
			cmp.res.Summary.LeftOnly++
		case cmpRightOnly:
			cmp.res.Summary.RightOnly++
		case cmpTypeMismatch:
			cmp.res.Summary.TypeMismatch++
		}
	}
	return cmp.res, nil
}

var errTruncated = errors.New("compare truncated")

type comparer struct {
	ctx      context.Context
	lfs, rfs FS
	o        compareOptions
	res      *CompareResult
	pending  []*CompareItem // same size: checksum decides
}

func (c *comparer) excluded(name, rel string) bool {
	for _, g := range c.o.Excludes {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if matchName(g, name) || matchName(g, rel) {
			return true
		}
	}
	return false
}

func (c *comparer) add(it *CompareItem) error {
	if len(c.res.Items) >= c.o.MaxItems {
		c.res.Truncated = true
		return errTruncated
	}
	c.res.Items = append(c.res.Items, it)
	return nil
}

func listOrEmpty(ctx context.Context, fsys FS, p string) (map[string]*Entry, error) {
	entries, err := ListDir(ctx, fsys, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*Entry{}, nil
		}
		return nil, err
	}
	finishEntries(ctx, fsys, entries)
	m := make(map[string]*Entry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m, nil
}

func (c *comparer) dir(lp, rp, rel string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	left, err := listOrEmpty(c.ctx, c.lfs, lp)
	if err != nil {
		return err
	}
	right, err := listOrEmpty(c.ctx, c.rfs, rp)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(left)+len(right))
	for n := range left {
		names = append(names, n)
	}
	for n := range right {
		if _, ok := left[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		r := n
		if rel != "" {
			r = rel + "/" + n
		}
		if c.excluded(n, r) || strings.HasSuffix(n, partSuffix) {
			continue
		}
		le, re := left[n], right[n]
		switch {
		case le == nil:
			if err := c.add(&CompareItem{Path: r, Type: re.Type, Status: cmpRightOnly, Right: re}); err != nil {
				return err
			}
		case re == nil:
			if err := c.add(&CompareItem{Path: r, Type: le.Type, Status: cmpLeftOnly, Left: le}); err != nil {
				return err
			}
		case le.Type != re.Type:
			if err := c.add(&CompareItem{Path: r, Type: le.Type, Status: cmpTypeMismatch, Left: le, Right: re}); err != nil {
				return err
			}
		case le.Type == model.FileTypeDir:
			if c.o.Recursive {
				if err := c.dir(le.Path, re.Path, r); err != nil {
					return err
				}
			}
		case le.Type == model.FileTypeSymlink:
			it := &CompareItem{Path: r, Type: le.Type, Status: cmpSame, Left: le, Right: re}
			if le.LinkTarget != re.LinkTarget {
				it.Status, it.Reason = cmpDifferent, "target"
			}
			if err := c.add(it); err != nil {
				return err
			}
		default:
			it := &CompareItem{Path: r, Type: le.Type, Status: cmpSame, Left: le, Right: re}
			switch {
			case le.Size != re.Size:
				it.Status, it.Reason = cmpDifferent, "size"
			case c.o.Mode == "checksum":
				c.pending = append(c.pending, it)
			case c.o.Mode == "size-mtime":
				d := le.Mtime.Sub(re.Mtime)
				if d < 0 {
					d = -d
				}
				if d > mtimeTolerance {
					it.Status, it.Reason = cmpDifferent, "mtime"
				}
			}
			if err := c.add(it); err != nil {
				return err
			}
		}
	}
	return nil
}

// hashPending decides same-size files by SHA-256 (native checksum tools when available), 4 at a time.
func (c *comparer) hashPending() error {
	if len(c.pending) == 0 {
		return nil
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, it := range c.pending {
		wg.Add(1)
		sem <- struct{}{}
		go func(it *CompareItem) {
			defer wg.Done()
			defer func() { <-sem }()
			lh, err := checksum(c.ctx, c.lfs, it.Left.Path, "sha256")
			var rh string
			if err == nil {
				rh, err = checksum(c.ctx, c.rfs, it.Right.Path, "sha256")
			}
			if err != nil {
				mu.Lock()
				if first == nil && c.ctx.Err() != nil {
					first = c.ctx.Err()
				}
				mu.Unlock()
				it.Status, it.Reason = cmpDifferent, "unreadable"
				return
			}
			if lh != rh {
				it.Status, it.Reason = cmpDifferent, "checksum"
			}
		}(it)
	}
	wg.Wait()
	return first
}
