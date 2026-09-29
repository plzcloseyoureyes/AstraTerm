package importer

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // ensure the pure-Go sqlite driver is registered for backup validation

	"github.com/termstead/termstead/internal/app"
	"github.com/termstead/termstead/internal/httpx"
)

// Backup / restore (IMP-4, admin-only).
//
// A backup is a consistent snapshot of the SQLite database made with `VACUUM INTO` (never a live-file copy). The
// database holds every secret sealed by the vault's data key, so a plain backup is only usable on an instance whose
// <data>/system.key is the same (or with the master password) — the snapshot alone does not reveal secrets. Including
// the system key is opt-in and only ever produced inside a passphrase-encrypted archive, so the archive's passphrase
// is the single thing protecting the secrets.
//
// Restore is staged, not applied live: a running process cannot safely swap its own database. The uploaded backup is
// validated (strict archive layout with size limits, read-only SQLite open, integrity check) and written under
// <data>/restore/ together with a request marker; the next start of Termstead applies it before opening the database
// (ApplyStagedRestore, restore_apply.go). DELETE /api/admin/restore discards a staged restore.

const (
	backupMagicSQLite = "SQLite format 3\x00"
	payloadBackup     = "termstead-backup"
	maxRestoreBytes   = 512 << 20
	maxSystemKeyBytes = 4 << 10
)

// vacuumInto writes a consistent snapshot of the database into a fresh private temp directory and returns its path;
// cleanup removes the directory.
func vacuumInto(ctx context.Context, d *app.Deps) (path string, cleanup func(), err error) {
	base := d.Cfg.TmpDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "backup-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "termstead.db") // VACUUM INTO requires the target not to exist
	if _, err := d.Store.DB.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("snapshot database: %w", err)
	}
	return path, cleanup, nil
}

// buildBackup produces the backup bytes: a raw snapshot, or (when a passphrase is given) a passphrase-encrypted zip
// archive that optionally also carries the system key.
func buildBackup(ctx context.Context, d *app.Deps, includeSystemKey bool, passphrase string) (exportResult, error) {
	if includeSystemKey && passphrase == "" {
		return exportResult{}, httpx.BadRequest("including the system key requires a passphrase-encrypted archive")
	}
	if passphrase != "" {
		if err := checkNewPassphrase(passphrase); err != nil {
			return exportResult{}, err
		}
	}
	dbPath, cleanup, err := vacuumInto(ctx, d)
	if err != nil {
		return exportResult{}, err
	}
	defer cleanup()

	ts := time.Now().Format("20060102-150405")
	if passphrase == "" {
		data, err := os.ReadFile(dbPath)
		if err != nil {
			return exportResult{}, err
		}
		return exportResult{Data: data, Filename: "termstead-backup-" + ts + ".db", ContentType: "application/x-sqlite3"}, nil
	}

	// Encrypted archive: zip {termstead.db [, system.key]} then seal with the passphrase.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	if err := zipAddFile(zw, "termstead.db", dbPath); err != nil {
		return exportResult{}, err
	}
	if includeSystemKey {
		keyPath := filepath.Join(d.Cfg.DataDir, "system.key")
		if err := zipAddFile(zw, "system.key", keyPath); err != nil {
			return exportResult{}, fmt.Errorf("read system key: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return exportResult{}, err
	}
	sealed, err := encrypt(zbuf.Bytes(), passphrase, payloadBackup)
	if err != nil {
		return exportResult{}, err
	}
	return exportResult{Data: sealed, Filename: "termstead-backup-" + ts + ".ntbak", ContentType: "application/octet-stream"}, nil
}

func zipAddFile(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// restoreResult is returned by POST /api/admin/restore.
type restoreResult struct {
	Staged       bool   `json:"staged"`
	StagedDBPath string `json:"stagedDbPath"`
	// ApplyOnRestart: the next start of Termstead applies the staged restore (always true for a new staging).
	ApplyOnRestart bool     `json:"applyOnRestart"`
	Instructions   []string `json:"instructions"`
	Warnings       []string `json:"warnings,omitempty"`
}

// stageRestore validates an uploaded backup, stages it under <data>/restore/ and requests that the next start applies it.
func stageRestore(ctx context.Context, d *app.Deps, content []byte, passphrase, stagedBy string) (restoreResult, error) {
	if len(content) == 0 {
		return restoreResult{}, httpx.BadRequest("empty backup")
	}
	if int64(len(content)) > maxRestoreBytes {
		return restoreResult{}, httpx.BadRequest("backup is too large")
	}
	dbBytes, keyBytes, err := extractBackup(content, passphrase)
	if err != nil {
		return restoreResult{}, err
	}
	restoreDir := filepath.Join(d.Cfg.DataDir, "restore")
	if err := os.MkdirAll(restoreDir, 0o700); err != nil {
		return restoreResult{}, err
	}
	warnings, err := validateSQLite(ctx, d, dbBytes, restoreDir)
	if err != nil {
		return restoreResult{}, err
	}
	dbStaged := filepath.Join(restoreDir, "termstead.db")
	if err := writeFileAtomic0600(dbStaged, dbBytes); err != nil {
		return restoreResult{}, err
	}
	keyStaged := filepath.Join(restoreDir, "system.key")
	_ = os.Remove(keyStaged) // never leave the key of an earlier staged restore next to this one
	res := restoreResult{Staged: true, StagedDBPath: dbStaged, Warnings: warnings}
	if len(keyBytes) > 0 {
		if err := writeFileAtomic0600(keyStaged, keyBytes); err != nil {
			return restoreResult{}, err
		}
	}
	_ = writeFileAtomic0600(filepath.Join(restoreDir, "RESTORE-README.txt"), []byte(restoreReadme(d.Cfg.DataDir, len(keyBytes) > 0)))
	if err := writeRestoreMarker(d.Cfg, restoreMarkerFile{StagedAt: time.Now().UTC(), StagedBy: stagedBy, SystemKey: len(keyBytes) > 0}); err != nil {
		return restoreResult{}, err
	}
	res.ApplyOnRestart = true
	replaced := "database"
	if len(keyBytes) > 0 {
		replaced = "database and system key"
	}
	res.Instructions = append(res.Instructions,
		"Restart Termstead to apply the restore. Everyone is signed out and running sessions end with the restart.",
		fmt.Sprintf("On start, the current %s are moved to %s/previous-<date>/ and replaced by the backup's.", replaced, restoreDir),
		"Changed your mind? Discard the staged restore before restarting.",
	)
	if len(keyBytes) == 0 {
		res.Warnings = append(res.Warnings, "The backup does not include the system key, so its sealed secrets are only usable on the instance whose system.key created it (or after restoring that key), unless the vault uses a master password.")
	}
	return res, nil
}

// extractBackup returns the database bytes and (optionally) the system-key bytes from a raw or encrypted backup.
func extractBackup(content []byte, passphrase string) (db, key []byte, err error) {
	if isEncryptedEnvelope(content) {
		if passphrase == "" {
			return nil, nil, httpx.NewError(http.StatusUnprocessableEntity, "passphrase_required", "this backup is encrypted; a passphrase is required")
		}
		pt, payload, derr := decrypt(content, passphrase)
		if derr != nil {
			return nil, nil, derr
		}
		if payload != payloadBackup {
			return nil, nil, httpx.BadRequest("this encrypted file is not a Termstead backup")
		}
		content = pt
	}
	switch {
	case bytes.HasPrefix(content, []byte(backupMagicSQLite)):
		return content, nil, nil
	case bytes.HasPrefix(content, []byte("PK\x03\x04")):
		return unzipBackup(content)
	default:
		return nil, nil, httpx.BadRequest("unrecognised backup (expected a SQLite snapshot or a Termstead archive)")
	}
}

// unzipBackup reads a Termstead backup archive: exactly "termstead.db" and optionally "system.key" at the top level,
// each at most once and within its size limit (declared and actual — a zip bomb or a lying header is refused, never
// silently truncated).
func unzipBackup(content []byte) (db, key []byte, err error) {
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, nil, httpx.BadRequest("invalid backup archive")
	}
	if len(zr.File) > 4 {
		return nil, nil, httpx.BadRequest("unexpected backup archive layout")
	}
	for _, f := range zr.File {
		var limit int64
		var dst *[]byte
		switch f.Name {
		case "termstead.db":
			limit, dst = maxRestoreBytes, &db
		case "system.key":
			limit, dst = maxSystemKeyBytes, &key
		default:
			return nil, nil, httpx.BadRequest("unexpected file in the backup archive: " + truncate(f.Name, 80))
		}
		if *dst != nil {
			return nil, nil, httpx.BadRequest("duplicate " + f.Name + " in the backup archive")
		}
		if f.UncompressedSize64 > uint64(limit) {
			return nil, nil, httpx.BadRequest(f.Name + " in the backup archive is too large")
		}
		b, rerr := readZipEntry(f, limit)
		if rerr != nil {
			return nil, nil, rerr
		}
		*dst = b
	}
	if len(db) == 0 {
		return nil, nil, httpx.BadRequest("the archive does not contain termstead.db")
	}
	return db, key, nil
}

func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, httpx.BadRequest("unreadable " + f.Name + " in the backup archive")
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, httpx.BadRequest("corrupt " + f.Name + " in the backup archive")
	}
	if int64(len(b)) > limit {
		return nil, httpx.BadRequest(f.Name + " in the backup archive is too large")
	}
	return b, nil
}

// validateSQLite writes dbBytes to a private temp file, opens it read-only and checks it is an intact Termstead
// database. It returns warnings (e.g. a backup from a newer Termstead).
func validateSQLite(ctx context.Context, d *app.Deps, dbBytes []byte, dir string) ([]string, error) {
	if !bytes.HasPrefix(dbBytes, []byte(backupMagicSQLite)) {
		return nil, httpx.BadRequest("the restore payload is not a SQLite database")
	}
	tmpDir, err := os.MkdirTemp(dir, ".verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir) // also removes any -wal / -shm / -journal files the driver created
	tmp := filepath.Join(tmpDir, "check.db")
	if err := writeFile0600(tmp, dbBytes); err != nil {
		return nil, err
	}
	dbh, err := sql.Open("sqlite", tmp+"?_pragma=busy_timeout(2000)&_pragma=query_only(1)")
	if err != nil {
		return nil, httpx.BadRequest("the backup could not be opened")
	}
	defer dbh.Close()
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('users','schema_migrations','connections')`).Scan(&n); err != nil {
		return nil, httpx.BadRequest("the backup is not a valid Termstead database")
	}
	if n < 3 {
		return nil, httpx.BadRequest("the backup does not look like a Termstead database (missing core tables)")
	}
	var check string
	if err := dbh.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil || !strings.EqualFold(check, "ok") {
		return nil, httpx.BadRequest("the backup database is damaged (integrity check failed)")
	}
	return newerSchemaWarnings(ctx, d, dbh), nil
}

// newerSchemaWarnings compares the backup's migrations with the running instance's.
func newerSchemaWarnings(ctx context.Context, d *app.Deps, backup *sql.DB) []string {
	maxVersions := func(q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}) map[string]int {
		out := map[string]int{}
		rows, err := q.QueryContext(ctx, `SELECT module, MAX(version) FROM schema_migrations GROUP BY module`)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			var v int
			if rows.Scan(&m, &v) == nil {
				out[m] = v
			}
		}
		return out
	}
	live, bk := maxVersions(d.Store.DB), maxVersions(backup)
	var newer []string
	for m, v := range bk {
		if lv, ok := live[m]; !ok || v > lv {
			newer = append(newer, m)
		}
	}
	if len(newer) == 0 {
		return nil
	}
	return []string{"The backup was made by a newer or differently equipped Termstead (schema of: " + strings.Join(newer, ", ") + "). Restore it with a matching version."}
}

func writeFile0600(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

// writeFileAtomic0600 writes via a temp file + rename so a concurrent or interrupted restore never leaves a
// half-written staged file.
func writeFileAtomic0600(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(name, 0o600)); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func restoreReadme(dataDir string, withKey bool) string {
	var b strings.Builder
	b.WriteString("Termstead staged restore\n======================\n\n")
	b.WriteString("A restore was staged from the admin UI. Termstead applies it automatically at its next start (as long as\n")
	b.WriteString("the file APPLY-ON-RESTART is present here). To apply it by hand instead, delete APPLY-ON-RESTART and:\n\n")
	b.WriteString("1. Stop Termstead.\n")
	b.WriteString(fmt.Sprintf("2. Back up your current data directory (%s).\n", dataDir))
	b.WriteString(fmt.Sprintf("3. Replace %s with restore/termstead.db and remove termstead.db-wal / termstead.db-shm.\n", filepath.Join(dataDir, "termstead.db")))
	if withKey {
		b.WriteString(fmt.Sprintf("4. Replace %s with restore/system.key.\n", filepath.Join(dataDir, "system.key")))
	}
	b.WriteString("Then start Termstead again.\n")
	return b.String()
}
