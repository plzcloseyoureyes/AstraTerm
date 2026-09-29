package rdp

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/termstead/termstead/internal/model"
	"github.com/termstead/termstead/internal/store"
	"github.com/termstead/termstead/internal/term"
)

// Recording of guacd sessions (options.recording): Termstead writes the Guacamole instruction stream the tunnel relays
// from guacd — what guacd's own session recordings contain, and what Guacamole.SessionRecording plays — into
// <data>/recordings/<id>.guac, listed in the module table rdp_recordings (recordings.go). Recording in Termstead
// (instead of guacd's recording-path) keeps the files next to the terminal recordings whichever guacd is used;
// guacd's filesystem is usually a container's. The browser's mouse movements are recorded too (the cursor during
// playback); keystrokes and clipboard contents are not (they may carry passwords).

// recordingKindGuac is the kind of guacd session recordings (Guacamole protocol, code point lengths).
const recordingKindGuac = "guac"

type guacRecorder struct {
	h     *handler
	rs    *recordingStore
	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	meta  *rdpRecording
	size  int64
	err   error
	done  bool
	sized bool
}

// startGuacRecording creates the recording file and its row.
func (h *handler) startGuacRecording(ctx context.Context, s *term.Session, tk *ticket, width, height int) (*guacRecorder, error) {
	if h.d.Cfg == nil || h.d.Cfg.DataDir == "" {
		return nil, errors.New("recording is not available")
	}
	rs, err := h.recordings()
	if err != nil {
		return nil, err
	}
	dir := h.d.Cfg.RecordingsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	meta := &rdpRecording{
		ID:        model.NewID(),
		OwnerID:   s.OwnerID,
		SessionID: s.ID,
		Title:     s.Title(),
		Kind:      recordingKindGuac,
		Width:     width,
		Height:    height,
		StartedAt: store.Now(),
	}
	if tk.conn != nil && tk.conn.ID != "" && tk.conn.OwnerID == s.OwnerID {
		meta.ConnectionID = tk.conn.ID
	}
	meta.Path = filepath.Join(dir, meta.ID+".guac")
	f, err := os.OpenFile(meta.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := rs.create(cctx, meta); err != nil {
		_ = f.Close()
		_ = os.Remove(meta.Path)
		return nil, err
	}
	return &guacRecorder{h: h, rs: rs, f: f, w: bufio.NewWriterSize(f, 64<<10), meta: meta}, nil
}

// write appends encoded instructions (code point lengths). A write error stops the recording, not the session.
func (r *guacRecorder) write(b []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done || r.err != nil {
		return
	}
	n, err := r.w.Write(b)
	r.size += int64(n)
	if err != nil {
		r.err = err
		r.h.log.Warn("rdp: writing the session recording failed", "recording", r.meta.ID, "err", err)
	}
}

// setSize records the remote desktop size (the first one reported) in the recording's row.
func (r *guacRecorder) setSize(width, height int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sized {
		r.sized = true
		r.meta.Width, r.meta.Height = width, height
	}
}

// finish closes the file and completes the row (size, end time).
func (r *guacRecorder) finish() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	if err := r.w.Flush(); err != nil && r.err == nil {
		r.err = err
	}
	_ = r.f.Close()
	ended := store.Now()
	r.meta.EndedAt = &ended
	r.meta.Size = r.size
	meta := *r.meta
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.rs.finish(ctx, &meta); err != nil {
		r.h.log.Warn("rdp: saving the session recording failed", "recording", meta.ID, "err", err)
	}
}
