package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestMigrationAddsDurationMsToExistingDB is the regression test for
// opening a real, pre-existing database (like the one on this box) that
// predates the duration_ms column: additiveMigrations must add it via
// ALTER TABLE without erroring, existing rows must stay readable (with
// DurationMs simply invalid, not a crash), and newly finished runs must
// get a real value.
func TestMigrationAddsDurationMsToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build the table exactly as it looked before duration_ms existed,
	// bypassing store.Open's own (already-current) schema.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE jobs (
		  id TEXT PRIMARY KEY, kind TEXT NOT NULL, cron TEXT NOT NULL,
		  timezone TEXT NOT NULL DEFAULT 'local', enabled INTEGER NOT NULL DEFAULT 1,
		  cwd TEXT NOT NULL, command TEXT, prompt TEXT, model TEXT, permission_mode TEXT,
		  max_concurrent INTEGER NOT NULL DEFAULT 1, timeout_seconds INTEGER,
		  keep INTEGER NOT NULL DEFAULT 200, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE runs (
		  id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), trigger TEXT NOT NULL,
		  status TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT, exit_code INTEGER,
		  session_id TEXT, log_path TEXT NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := raw.Exec(`INSERT INTO jobs (id, kind, cron, cwd, created_at, updated_at) VALUES
		('old-job', 'cli', '0 0 * * *', '/tmp', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO runs (id, job_id, trigger, status, started_at, log_path) VALUES
		('old-run', 'old-job', 'manual', 'ok', ?, '/tmp/old-run.log')`, now); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// This is the real code path: opening a pre-existing DB must migrate
	// it, not fail or silently skip the new column.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-duration_ms database: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	oldRun, err := st.GetRun(ctx, "old-run")
	if err != nil {
		t.Fatalf("GetRun on a migrated pre-existing row: %v", err)
	}
	if oldRun.DurationMs.Valid {
		t.Fatalf("a pre-migration row should have no duration_ms, got %+v", oldRun.DurationMs)
	}

	// A run created and finished after the migration must get a real one.
	newRun, err := st.StartRun(ctx, "old-job", "new-run", "manual", "/tmp/new-run.log", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, newRun.ID, "ok", 0, time.Now(), 1234); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, "new-run")
	if err != nil {
		t.Fatal(err)
	}
	if !got.DurationMs.Valid || got.DurationMs.Int64 != 1234 {
		t.Fatalf("want duration_ms=1234, got %+v", got.DurationMs)
	}
}
