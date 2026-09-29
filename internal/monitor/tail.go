package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/termstead/termstead/internal/httpx"
)

// Log following (MON-5) over WS /ws/monitor/{id}/tail?path=…[&path=…]|journal=1[&unit=…]&lines=N[&sudo=1].
// Server → client text frames:
//
//	{type:'start', sources:[label…]}                 one label per followed file (or the journal)
//	{type:'lines', lines:[[sourceIndex, text]…]}     batched output (≤ 500 lines / 100 ms)
//	{type:'error', message}                          the follower could not start
//	{type:'end', code, message?}                     the follower exited
//
// Client → server: {type:'stop'} (or just close the socket).

const (
	maxTailFiles   = 8
	maxTailLines   = 5000
	maxTailLineLen = 16 << 10
	tailBatch      = 500
)

type tailRequest struct {
	paths   []string
	journal bool
	unit    string
	lines   int
	sudo    bool
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func parseTailRequest(q url.Values) (tailRequest, error) {
	r := tailRequest{lines: 200, sudo: truthy(q.Get("sudo")), journal: truthy(q.Get("journal"))}
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxTailLines {
			return r, httpx.BadRequest("lines must be between 0 and 5000")
		}
		r.lines = n
	}
	if u := strings.TrimSpace(q.Get("unit")); u != "" {
		if err := validUnit(u); err != nil {
			return r, err
		}
		r.unit, r.journal = u, true
	}
	seen := map[string]bool{}
	for _, p := range q["path"] {
		clean, err := validRemotePath(strings.TrimSpace(p))
		if err != nil {
			return r, err
		}
		if !seen[clean] {
			seen[clean] = true
			r.paths = append(r.paths, clean)
		}
	}
	switch {
	case r.journal && len(r.paths) > 0:
		return r, httpx.BadRequest("follow either files or the journal")
	case !r.journal && len(r.paths) == 0:
		return r, httpx.BadRequest("path or journal is required")
	case len(r.paths) > maxTailFiles:
		return r, httpx.BadRequest("at most 8 files can be followed at once")
	}
	return r, nil
}

func (r tailRequest) argv() []string {
	if r.journal {
		a := []string{"journalctl", "-f", "-n", strconv.Itoa(r.lines), "--no-pager", "-o", "short-iso"}
		if r.unit != "" {
			a = append(a, "-u", r.unit)
		}
		return a
	}
	return append([]string{"tail", "-n", strconv.Itoa(r.lines), "-F", "--"}, r.paths...)
}

func (r tailRequest) sources() []string {
	if r.journal {
		if r.unit != "" {
			return []string{"journal: " + r.unit}
		}
		return []string{"journal"}
	}
	return r.paths
}

type tailLine struct {
	src  int
	text string
}

// followLogs streams a follower's output to ws until either side ends.
func (s *Service) followLogs(ctx context.Context, ws *websocket.Conn, t *target, req tailRequest) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // client → server: stop / close
		defer cancel()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "stop" {
				return
			}
		}
	}()
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(ctx, 15*time.Second)
		defer wcancel()
		return ws.Write(wctx, websocket.MessageText, b)
	}
	if send(map[string]any{"type": "start", "sources": req.sources()}) != nil {
		return
	}
	st, err := s.startFollower(ctx, t, req)
	if err != nil {
		_ = send(map[string]any{"type": "error", "message": publicError(err)})
		_ = ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	// Every exit path (client gone, write failure, follower ended) kills the remote follower, releases its channel and
	// reaps a local process: stop alone would leave an overflow connection referenced / a zombie behind.
	defer st.close()

	lines := make(chan tailLine, 2048)
	go func() {
		defer close(lines)
		readFollower(st.Stdout, req.paths, func(l tailLine) bool {
			select {
			case lines <- l:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	batch := make([][2]any, 0, tailBatch)
	size := 0
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		err := send(map[string]any{"type": "lines", "lines": batch})
		batch, size = batch[:0], 0
		return err == nil
	}
	for open := true; open; {
		select {
		case l, ok := <-lines:
			if !ok {
				open = false
				break
			}
			batch = append(batch, [2]any{l.src, l.text})
			size += len(l.text)
			if (len(batch) >= tailBatch || size >= 256<<10) && !flush() {
				return
			}
		case <-tick.C:
			if !flush() {
				return
			}
		case <-ctx.Done():
			return
		}
	}
	if !flush() {
		return
	}
	res, werr := st.wait()
	end := map[string]any{"type": "end", "code": -1}
	if res != nil {
		end["code"] = res.Code
		if msg := res.errText(); msg != "" {
			end["message"] = msg
		}
	}
	if werr != nil && ctx.Err() == nil {
		end["message"] = werr.Error()
	}
	if ctx.Err() == nil {
		_ = send(end)
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}
}

func (s *Service) startFollower(ctx context.Context, t *target, req tailRequest) (*stream, error) {
	if t.host.Platform == platWindows {
		return nil, httpx.BadRequest("following logs is not supported on Windows hosts")
	}
	argv := req.argv()
	if !req.sudo {
		return t.r.start(ctx, command{sh: watchScript(shJoin(argv)+" </dev/null 2>&1", ""), hold: true})
	}
	pw, err := s.sudoAuth(ctx, t)
	if err != nil {
		return nil, err
	}
	line, doc := sudoLine(argv, pw)
	return t.r.start(ctx, command{sh: watchScript(line+" 2>&1", doc), hold: true})
}

// readFollower splits follower output into lines (long lines truncated), attributing them to files through the
// "==> path <==" headers tail prints when following several files.
func readFollower(r io.Reader, paths []string, emit func(tailLine) bool) {
	index := map[string]int{}
	for i, p := range paths {
		index[p] = i
	}
	br := bufio.NewReaderSize(r, 64<<10)
	cur := 0
	pendingBlank := false
	for {
		line, err := readCappedLine(br, maxTailLineLen)
		if line == "" && err != nil {
			return
		}
		if len(paths) > 1 && strings.HasPrefix(line, "==> ") && strings.HasSuffix(line, " <==") {
			if i, ok := index[line[4:len(line)-4]]; ok {
				cur, pendingBlank = i, false
				continue
			}
		}
		if pendingBlank {
			if !emit(tailLine{src: cur}) {
				return
			}
			pendingBlank = false
		}
		if line == "" && len(paths) > 1 {
			pendingBlank = true // tail separates file sections with a blank line before the next header
		} else if !emit(tailLine{src: cur, text: line}) {
			return
		}
		if err != nil {
			return
		}
	}
}

// readCappedLine reads one line without its terminator, keeping at most max bytes (the rest of the line is dropped).
func readCappedLine(br *bufio.Reader, max int) (string, error) {
	var b []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(b) < max {
			room := max - len(b)
			if len(chunk) > room {
				chunk = chunk[:room]
			}
			b = append(b, chunk...)
		}
		if err != nil {
			if len(b) > 0 {
				return strings.TrimRight(string(b), "\r"), nil
			}
			return "", err
		}
		if !isPrefix {
			return strings.TrimRight(string(b), "\r"), nil
		}
	}
}

// publicError renders an error for a client: typed API errors keep their message, others are summarized.
func publicError(err error) string {
	var he *httpx.HTTPError
	if errors.As(err, &he) && he.Status < 500 {
		return he.Message
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return err.Error()
	}
	return clip(err.Error(), 300)
}
