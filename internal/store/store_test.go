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
// predates the duration_ms *and* provider columns: additiveMigrations must
// add both via ALTER TABLE without erroring, existing rows must stay
// readable (with DurationMs invalid and Provider "" — meaning claude, not
// a crash), and newly created/finished rows must get real values.
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

	oldJob, err := st.GetJob(ctx, "old-job")
	if err != nil {
		t.Fatalf("GetJob on a migrated pre-existing row: %v", err)
	}
	if oldJob.Provider != "" {
		t.Fatalf("a pre-migration job should have an empty provider (meaning claude), got %q", oldJob.Provider)
	}

	// A job created after the migration must round-trip a real provider.
	if err := st.CreateJob(ctx, Job{
		ID: "new-job", Kind: "agent", Cron: "0 0 * * *", Timezone: "local", Enabled: true,
		Cwd: "/tmp", Prompt: "hi", Provider: "opencode",
	}); err != nil {
		t.Fatal(err)
	}
	newJob, err := st.GetJob(ctx, "new-job")
	if err != nil {
		t.Fatal(err)
	}
	if newJob.Provider != "opencode" {
		t.Fatalf("want provider=opencode, got %q", newJob.Provider)
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

// Jobs from before the 30-minute default had NULL timeout_seconds, meaning
// "no limit". They must now get the default, while explicit values stand.
func TestUnsetTimeoutMeansTheDefault(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, j := range []Job{
		{ID: "unset", Kind: "cli", Cron: "0 0 * * *", Timezone: "local", Cwd: "/tmp", Command: "true", MaxConcurrent: 1, Keep: 200},
		{ID: "set", Kind: "cli", Cron: "0 0 * * *", Timezone: "local", Cwd: "/tmp", Command: "true", MaxConcurrent: 1, Keep: 200, TimeoutSeconds: 600},
	} {
		if err := st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	unset, _ := st.GetJob(ctx, "unset")
	set, _ := st.GetJob(ctx, "set")
	if unset.Timeout() != DefaultTimeout || set.Timeout() != 10*time.Minute {
		t.Fatalf("timeouts: unset=%s set=%s, want %s/10m", unset.Timeout(), set.Timeout(), DefaultTimeout)
	}
}
