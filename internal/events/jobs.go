package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexterm/nexterm/internal/model"
)

// Job event kinds ({type:'job', event}).
const (
	JobData  = "data"
	JobDone  = "done"
	JobError = "error"
)

// JobEvent is published to the job owner's sockets.
type JobEvent struct {
	Type  string `json:"type"` // always "job"
	JobID string `json:"jobId"`
	Event string `json:"event"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// JobInfo describes a running job.
type JobInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	StartedAt time.Time `json:"startedAt"`
}

type job struct {
	info   JobInfo
	userID string
	cancel context.CancelFunc
}

// Jobs runs cancellable background tasks whose progress streams to the owner as job events (tools, bulk actions).
type Jobs struct {
	hub *Hub
	ctx context.Context
	log *slog.Logger

	mu   sync.Mutex
	jobs map[string]*job
}

// NewJobs creates a job registry; all jobs are cancelled when ctx is done.
func NewJobs(ctx context.Context, hub *Hub, log *slog.Logger) *Jobs {
	if log == nil {
		log = slog.Default()
	}
	return &Jobs{hub: hub, ctx: ctx, log: log, jobs: map[string]*job{}}
}

// Start runs fn in the background and returns its job ID immediately. emit publishes {event:'data', data}; when fn
// returns, {event:'done'} or {event:'error', error} is published ("canceled" after Cancel). Note that events may
// reach the client before the HTTP response carrying the job ID, so clients should buffer unknown job IDs briefly.
func (j *Jobs) Start(user *model.User, name string, fn func(ctx context.Context, emit func(data any)) error) string {
	id := model.NewID()
	ctx, cancel := context.WithCancel(j.ctx)
	jb := &job{info: JobInfo{ID: id, Name: name, StartedAt: time.Now().UTC()}, userID: user.ID, cancel: cancel}
	j.mu.Lock()
	j.jobs[id] = jb
	j.mu.Unlock()

	userID := user.ID
	go func() {
		defer func() {
			cancel()
			j.mu.Lock()
			delete(j.jobs, id)
			j.mu.Unlock()
		}()
		var finished atomic.Bool
		emit := func(data any) {
			if finished.Load() {
				return
			}
			j.hub.Publish(userID, JobEvent{Type: model.EvJob, JobID: id, Event: JobData, Data: data})
		}
		err := runJob(ctx, fn, emit)
		finished.Store(true)
		switch {
		case err == nil:
			j.hub.Publish(userID, JobEvent{Type: model.EvJob, JobID: id, Event: JobDone})
		case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err())):
			j.hub.Publish(userID, JobEvent{Type: model.EvJob, JobID: id, Event: JobError, Error: "canceled"})
		default:
			j.log.Debug("job failed", "job", name, "id", id, "err", err)
			j.hub.Publish(userID, JobEvent{Type: model.EvJob, JobID: id, Event: JobError, Error: err.Error()})
		}
	}()
	return id
}

func runJob(ctx context.Context, fn func(ctx context.Context, emit func(data any)) error, emit func(any)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("job panicked", "panic", r)
			err = fmt.Errorf("internal error")
		}
	}()
	return fn(ctx, emit)
}

// Cancel stops a job owned by user (admins may cancel any job). Unknown or foreign jobs yield model.ErrNotFound.
func (j *Jobs) Cancel(user *model.User, jobID string) error {
	j.mu.Lock()
	jb := j.jobs[jobID]
	j.mu.Unlock()
	if jb == nil || (jb.userID != user.ID && !user.IsAdmin()) {
		return model.ErrNotFound
	}
	jb.cancel()
	return nil
}

// List returns the running jobs of user.
func (j *Jobs) List(user *model.User) []JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []JobInfo{}
	for _, jb := range j.jobs {
		if jb.userID == user.ID {
			out = append(out, jb.info)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.Before(out[b].StartedAt) })
	return out
}
