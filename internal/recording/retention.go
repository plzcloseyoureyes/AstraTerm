package recording

import (
	"context"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/nexterm/nexterm/internal/httpx"
	"github.com/nexterm/nexterm/internal/model"
)

// Retention (REC-7): the administrator's policy (global settings section "recording") deletes finished recordings
// older than maxAgeDays and then, while everything together exceeds maxTotalMB, the oldest finished ones. Recordings a
// session is still writing are never touched. The cleanup runs 45 s after start-up and every 10 minutes; admins can
// preview or run it with POST /api/admin/recordings/cleanup {dryRun?}.

// RetentionResult summarizes one cleanup run.
type RetentionResult struct {
	DryRun     bool     `json:"dryRun"`
	Deleted    int      `json:"deleted"`
	Bytes      int64    `json:"bytes"`
	ByAge      int      `json:"byAge"`
	BySize     int      `json:"bySize"`
	TotalBytes int64    `json:"totalBytes"` // all recordings before the run
	Failed     int      `json:"failed"`
	IDs        []string `json:"ids,omitempty"`
}

type retentionRow struct {
	rec   model.Recording
	size  int64
	stamp time.Time // end (or start) time used for ordering
}

func (s *Service) applyRetention(ctx context.Context, dryRun bool) (RetentionResult, error) {
	p := s.policy()
	res := RetentionResult{DryRun: dryRun}
	rows, err := s.d.Store.DB.QueryContext(ctx, `SELECT id, owner_id, session_id, connection_id, title, kind, path, size,
		cols, rows, started_at, ended_at FROM recordings ORDER BY COALESCE(ended_at, started_at), id`)
	if err != nil {
		return res, err
	}
	var all []retentionRow
	for rows.Next() {
		it, err := scanItem(itemRowScanner{rows})
		if err != nil {
			rows.Close()
			return res, err
		}
		r := retentionRow{rec: it.Recording, size: it.Size, stamp: it.StartedAt}
		if it.EndedAt != nil {
			r.stamp = *it.EndedAt
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	// Unfinished rows carry size 0: use the file size.
	for i := range all {
		if all[i].rec.EndedAt == nil {
			if fi, err := os.Stat(all[i].rec.Path); err == nil {
				all[i].size = fi.Size()
			}
		}
		res.TotalBytes += all[i].size
	}
	if p.MaxAgeDays <= 0 && p.MaxTotalMB <= 0 {
		return res, nil
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].stamp.Before(all[j].stamp) })
	chosen := map[string]bool{}
	var victims []retentionRow
	pick := func(r retentionRow) {
		chosen[r.rec.ID] = true
		victims = append(victims, r)
	}
	if p.MaxAgeDays > 0 {
		cutoff := s.now().AddDate(0, 0, -p.MaxAgeDays)
		for _, r := range all {
			if r.stamp.Before(cutoff) && !s.isActive(&r.rec) {
				pick(r)
				res.ByAge++
			}
		}
	}
	if p.MaxTotalMB > 0 {
		limit := int64(p.MaxTotalMB) << 20
		remaining := res.TotalBytes
		for _, v := range victims {
			remaining -= v.size
		}
		for _, r := range all {
			if remaining <= limit {
				break
			}
			if chosen[r.rec.ID] || s.isActive(&r.rec) {
				continue
			}
			pick(r)
			res.BySize++
			remaining -= r.size
		}
	}
	for _, v := range victims {
		if !dryRun {
			if err := s.deleteRecording(ctx, &v.rec); err != nil {
				res.Failed++
				continue
			}
		}
		res.Deleted++
		res.Bytes += v.size
		if len(res.IDs) < 1000 {
			res.IDs = append(res.IDs, v.rec.ID)
		}
	}
	if !dryRun && res.Deleted > 0 {
		s.d.Audit.LogUser(ctx, nil, "recording.retention", "", map[string]any{"deleted": res.Deleted, "bytes": res.Bytes,
			"byAge": res.ByAge, "bySize": res.BySize, "maxAgeDays": p.MaxAgeDays, "maxTotalMB": p.MaxTotalMB})
		s.log.Info("retention: recordings deleted", "count", res.Deleted, "bytes", res.Bytes)
	}
	return res, nil
}

// itemRowScanner adapts plain recording rows to scanItem (no connection / owner names).
type itemRowScanner struct {
	rows interface{ Scan(...any) error }
}

func (r itemRowScanner) Scan(dest ...any) error {
	if len(dest) < 12 {
		return r.rows.Scan(dest...)
	}
	if err := r.rows.Scan(dest[:12]...); err != nil {
		return err
	}
	for _, d := range dest[12:] {
		if sp, ok := d.(*string); ok {
			*sp = ""
		}
	}
	return nil
}

func (s *Service) handleCleanup(c *echo.Context) error {
	var body struct {
		DryRun bool `json:"dryRun"`
	}
	if err := httpx.BindOptional(c, &body); err != nil {
		return err
	}
	s.refreshPolicy(c.Request().Context())
	res, err := s.applyRetention(c.Request().Context(), body.DryRun)
	if err != nil {
		return err
	}
	if !body.DryRun {
		s.d.Audit.Log(c, "recording.cleanup", "", map[string]any{"deleted": res.Deleted, "bytes": res.Bytes})
	}
	return c.JSON(http.StatusOK, res)
}
