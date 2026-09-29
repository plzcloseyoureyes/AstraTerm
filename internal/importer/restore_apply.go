package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nexterm/nexterm/internal/config"
)

// Applying a staged restore (IMP-4). A running process cannot swap its own database, so POST /api/admin/restore
// stages the backup under <data>/restore/ and writes the marker file restoreMarker; the next start applies it before
// the store is opened (internal/server calls ApplyStagedRestore). The live database (with its -wal / -shm files) and,
// when the backup carries one, the system key are moved to <data>/restore/previous-<time>/ first, so nothing is lost.

const (
	restoreDirName = "restore"
	restoreMarker  = "APPLY-ON-RESTART"
	systemKeyName  = "system.key"
)

type restoreMarkerFile struct {
	StagedAt  time.Time `json:"stagedAt"`
	StagedBy  string    `json:"stagedBy,omitempty"`
	SystemKey bool      `json:"systemKey"`
}

func restoreDir(cfg *config.Config) string { return filepath.Join(cfg.DataDir, restoreDirName) }

// writeRestoreMarker requests that the staged restore be applied at the next start.
func writeRestoreMarker(cfg *config.Config, m restoreMarkerFile) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic0600(filepath.Join(restoreDir(cfg), restoreMarker), b)
}

// readRestoreMarker reports whether a restore is pending (marker present).
func readRestoreMarker(cfg *config.Config) (restoreMarkerFile, bool) {
	var m restoreMarkerFile
	b, err := os.ReadFile(filepath.Join(restoreDir(cfg), restoreMarker))
	if err != nil {
		return m, false
	}
	_ = json.Unmarshal(b, &m)
	return m, true
}

// discardStagedRestore removes the pending marker and the staged files (previous-* backups are kept).
func discardStagedRestore(cfg *config.Config) (bool, error) {
	dir := restoreDir(cfg)
	_, pending := readRestoreMarker(cfg)
	var errs []error
	for _, name := range []string{restoreMarker, "nexterm.db", systemKeyName, "RESTORE-README.txt"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return pending, errors.Join(errs...)
}

// ApplyStagedRestore applies a restore staged by POST /api/admin/restore. It must run before the store is opened.
// Without a pending restore it does nothing. A staged database that is missing or not SQLite drops the request (logged)
// and leaves the live data untouched; if moving files fails half-way, the moves are rolled back and the request is kept
// so the next start retries. An error is returned only when the data directory could not be restored to a consistent
// state — the caller must not start then.
func ApplyStagedRestore(cfg *config.Config, log *slog.Logger) (applied bool, err error) {
	if log == nil {
		log = slog.Default()
	}
	marker, pending := readRestoreMarker(cfg)
	if !pending {
		return false, nil
	}
	dir := restoreDir(cfg)
	stagedDB := filepath.Join(dir, "nexterm.db")
	if !isSQLiteFile(stagedDB) {
		log.Error("restore: the staged database is missing or not a SQLite file; the restore request was dropped", "path", stagedDB)
		_ = os.Remove(filepath.Join(dir, restoreMarker))
		return false, nil
	}
	stagedKey := filepath.Join(dir, systemKeyName)
	withKey := fileExists(stagedKey)
	if marker.SystemKey && !withKey {
		log.Error("restore: the staged system key is missing; the restore request was dropped", "path", stagedKey)
		_ = os.Remove(filepath.Join(dir, restoreMarker))
		return false, nil
	}

	prev := filepath.Join(dir, "previous-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(prev, 0o700); err != nil {
		log.Error("restore: cannot create the folder for the current database; restore not applied", "err", err)
		return false, nil
	}
	type move struct{ from, to string }
	var done []move
	rename := func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		done = append(done, move{from, to})
		return nil
	}
	rollback := func(cause error) (bool, error) {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			if err := os.Rename(done[i].to, done[i].from); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return false, fmt.Errorf("restore: applying the staged restore failed (%v) and rolling back failed too (%w); "+
				"repair %s by hand (the previous files are in %s)", cause, errors.Join(errs...), cfg.DataDir, prev)
		}
		log.Error("restore: applying the staged restore failed; the current data was kept and the next start retries", "err", cause)
		return false, nil
	}

	live := cfg.DBPath()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if !fileExists(live + suffix) {
			continue
		}
		if err := rename(live+suffix, filepath.Join(prev, filepath.Base(live)+suffix)); err != nil {
			return rollback(err)
		}
	}
	if withKey {
		liveKey := filepath.Join(cfg.DataDir, systemKeyName)
		if fileExists(liveKey) {
			if err := rename(liveKey, filepath.Join(prev, systemKeyName)); err != nil {
				return rollback(err)
			}
		}
		if err := rename(stagedKey, liveKey); err != nil {
			return rollback(err)
		}
	}
	if err := rename(stagedDB, live); err != nil {
		return rollback(err)
	}
	if err := os.Remove(filepath.Join(dir, restoreMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("restore: cannot remove the restore request marker", "err", err)
	}
	_ = os.Remove(filepath.Join(dir, "RESTORE-README.txt"))
	note := fmt.Sprintf("Restore staged %s applied %s.\nThe database (and system key, if replaced) in use before are kept here.\n",
		marker.StagedAt.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	_ = writeFileAtomic0600(filepath.Join(prev, "README.txt"), []byte(note))
	log.Warn("restore: applied the staged backup", "previous", prev, "systemKey", withKey)
	return true, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isSQLiteFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(backupMagicSQLite))
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return bytes.Equal(head, []byte(backupMagicSQLite))
}
