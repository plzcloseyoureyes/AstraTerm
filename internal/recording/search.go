package recording

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/plzcloseyoureyes/astraterm/internal/httpx"
	"github.com/plzcloseyoureyes/astraterm/internal/model"
)

// Search inside recordings (REC-1 log search, REC-2 "search across recordings"):
//
//	GET /api/recordings/{id}/search?q=&regex=1&case=1&limit=   → {matches:[{line?, time?, ts?, text}], total, truncated}
//	GET /api/recordings/search?q=&regex=&case=&kind=&all=1&limit= → {results:[{recording, count, samples}], scanned, truncated}
//
// Text logs match per line (line numbers are 1-based; `ts` is the line's timestamp prefix when logged with
// timestamps). Casts match per line of their plain-text transcript (`time` = seconds since the start, for seeking).

// SearchMatch is one matching line.
type SearchMatch struct {
	Line int      `json:"line,omitempty"`
	Time *float64 `json:"time,omitempty"`
	TS   string   `json:"ts,omitempty"`
	Text string   `json:"text"`
}

type matcher func(string) bool

func newMatcher(q string, isRegex, caseSensitive bool) (matcher, error) {
	if strings.TrimSpace(q) == "" {
		return nil, httpx.BadRequest("q is required")
	}
	if len(q) > 512 {
		return nil, httpx.BadRequest("q is too long")
	}
	if isRegex {
		expr := q
		if !caseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, httpx.BadRequest("invalid regular expression: " + err.Error())
		}
		return re.MatchString, nil
	}
	if caseSensitive {
		return func(s string) bool { return strings.Contains(s, q) }, nil
	}
	lq := strings.ToLower(q)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), lq) }, nil
}

func matcherFrom(c *echo.Context) (matcher, error) {
	return newMatcher(c.QueryParam("q"), truthy(c.QueryParam("regex")), truthy(c.QueryParam("case")))
}

func truthy(v string) bool { return v == "1" || v == "true" || v == "yes" }

var errStopSearch = errors.New("stop")

const maxMatchText = 1000

func clip(s string) string {
	if len(s) <= maxMatchText {
		return s
	}
	s = s[:maxMatchText]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// searchRecording scans one recording, calling found for every match until it returns false. It returns the number
// of matches seen and whether the scan stopped early.
func (s *Service) searchRecording(ctx context.Context, rec *model.Recording, match matcher, found func(SearchMatch) bool) (int, bool, error) {
	path, err := s.safePath(rec.Path)
	if err != nil {
		return 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, errGone
	}
	defer f.Close()
	var r io.Reader = f
	count, stopped := 0, false
	check := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	switch rec.Kind {
	case model.RecordingLog:
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		line := 0
		for sc.Scan() {
			line++
			if line%4096 == 0 {
				if err := check(); err != nil {
					return count, true, err
				}
			}
			text := sc.Text()
			if !match(text) {
				continue
			}
			count++
			m := SearchMatch{Line: line, Text: clip(text)}
			if ts, rest, ok := splitLogTimestamp(text); ok {
				m.TS, m.Text = ts, clip(rest)
			}
			if !found(m) {
				stopped = true
				break
			}
		}
		if err := sc.Err(); err != nil && !stopped {
			return count, false, err
		}
	case model.RecordingAsciicast:
		n := 0
		_, err := castTranscript(r, func(l transcriptLine) error {
			n++
			if n%4096 == 0 {
				if err := check(); err != nil {
					return err
				}
			}
			if !match(l.Text) {
				return nil
			}
			count++
			t := l.Time
			if !found(SearchMatch{Time: &t, Text: clip(l.Text)}) {
				stopped = true
				return errStopSearch
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopSearch) && !errors.Is(err, errNotCast) {
			return count, stopped, err
		}
	default:
		return 0, false, httpx.BadRequest("this recording cannot be searched")
	}
	return count, stopped, nil
}

// splitLogTimestamp splits the "[YYYY-MM-DD HH:MM:SS.mmm] " prefix of timestamped log lines.
func splitLogTimestamp(line string) (string, string, bool) {
	const n = len("[2006-01-02 15:04:05.000] ")
	if len(line) < n || line[0] != '[' || line[n-2] != ']' {
		return "", "", false
	}
	if _, err := time.ParseInLocation("2006-01-02 15:04:05.000", line[1:n-2], time.Local); err != nil {
		return "", "", false
	}
	return line[1 : n-2], line[n:], true
}

func (s *Service) handleSearch(c *echo.Context) error {
	it, u, err := s.item(c)
	if err != nil {
		return err
	}
	match, err := matcherFrom(c)
	if err != nil {
		return err
	}
	limit := 500
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			return httpx.BadRequest("limit must be between 1 and 5000")
		}
		limit = n
	}
	if it.OwnerID != u.ID {
		s.d.Audit.Log(c, "recording.search", it.ID, map[string]any{"ownerId": it.OwnerID})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 30*time.Second)
	defer cancel()
	matches := []SearchMatch{}
	total, truncated, err := s.searchRecording(ctx, &it.Recording, match, func(m SearchMatch) bool {
		if len(matches) >= limit {
			return true // keep counting
		}
		matches = append(matches, m)
		return true
	})
	if errors.Is(err, context.DeadlineExceeded) {
		truncated, err = true, nil
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"matches": matches, "total": total,
		"truncated": truncated || total > len(matches)})
}

type searchResult struct {
	Recording *RecordingItem `json:"recording"`
	Count     int            `json:"count"`
	Samples   []SearchMatch  `json:"samples"`
}

const (
	searchAllMaxFiles = 300
	searchAllMaxBytes = 1 << 30
	searchAllSamples  = 3
	searchAllPerFile  = 1000
)

func (s *Service) handleSearchAll(c *echo.Context) error {
	match, err := matcherFrom(c)
	if err != nil {
		return err
	}
	f, err := s.parseFilter(c)
	if err != nil {
		return err
	}
	q := c.QueryParam("q")
	f.q = "" // q searches the content here, not the titles
	if v := c.QueryParam("title"); v != "" {
		f.q = v
	}
	limit := 50
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 200 {
			limit = n
		}
	}
	f.limit, f.offset = searchAllMaxFiles, 0
	ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
	defer cancel()
	items, _, err := s.queryItems(ctx, f)
	if err != nil {
		return err
	}
	results := []searchResult{}
	scanned, truncated := 0, false
	var bytesSeen int64
	for _, it := range items {
		if it.Kind != model.RecordingAsciicast && it.Kind != model.RecordingLog {
			continue
		}
		if ctx.Err() != nil || bytesSeen > searchAllMaxBytes || len(results) >= limit {
			truncated = true
			break
		}
		bytesSeen += it.Size
		scanned++
		r := searchResult{Recording: it, Samples: []SearchMatch{}}
		seen := 0
		n, stopped, err := s.searchRecording(ctx, &it.Recording, match, func(m SearchMatch) bool {
			seen++
			if len(r.Samples) < searchAllSamples {
				r.Samples = append(r.Samples, m)
			}
			return seen < searchAllPerFile
		})
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			continue // unreadable file: skip
		}
		r.Count = n
		if stopped {
			truncated = true
		}
		if n > 0 {
			results = append(results, r)
		}
	}
	if len(items) >= searchAllMaxFiles {
		truncated = true
	}
	if u := httpx.UserFrom(c); f.ownerID != u.ID {
		s.d.Audit.Log(c, "recording.search_all", f.ownerID, map[string]any{"query": clip(q), "scanned": scanned, "hits": len(results)})
	}
	return c.JSON(http.StatusOK, map[string]any{"results": results, "scanned": scanned, "truncated": truncated})
}
