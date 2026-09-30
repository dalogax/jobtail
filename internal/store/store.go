// Package store is jobtail's SQLite-backed data layer: jobs and their runs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  cron            TEXT NOT NULL,
  timezone        TEXT NOT NULL DEFAULT 'local',
  enabled         INTEGER NOT NULL DEFAULT 1,
  cwd             TEXT NOT NULL,
  command         TEXT,
  prompt          TEXT,
  model           TEXT,
  provider        TEXT,
  permission_mode TEXT,
  max_concurrent  INTEGER NOT NULL DEFAULT 1,
  timeout_seconds INTEGER,
  keep            INTEGER NOT NULL DEFAULT 200,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  id            TEXT PRIMARY KEY,
  job_id        TEXT NOT NULL REFERENCES jobs(id),
  trigger       TEXT NOT NULL,
  status        TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  finished_at   TEXT,
  exit_code     INTEGER,
  session_id    TEXT,
  log_path      TEXT NOT NULL,
  duration_ms   INTEGER
);

CREATE INDEX IF NOT EXISTS idx_runs_job ON runs(job_id, started_at DESC);
`

// additiveMigrations runs schema changes that came after the initial
// release, against a database that may already exist without them.
// started_at can't double as an execution-duration basis on its own: for
// a scheduled run it's deliberately the nominal cron slot (not the actual
// moment run-exec began), so that catch-up after a gap advances one slot
// at a time instead of silently skipping backlogged ones — but that same
// property means finished_at-started_at can read as a wildly inflated
// duration (measured up to 60s for a run that actually took 30ms, once
// ticks had fallen behind). duration_ms is measured directly around the
// actual execution instead, so it never conflates scheduling delay with
// real run time.
func additiveMigrations(db *sql.DB) error {
	if err := addColumnIfMissing(db, "runs", "duration_ms", "INTEGER"); err != nil {
		return err
	}
	// provider distinguishes which agent CLI an agent-kind job runs under
	// ("claude" | "opencode" | "codex"); empty/NULL means "claude", the
	// original and still-default behavior, so existing jobs from before
	// multi-provider support need no backfill.
	if err := addColumnIfMissing(db, "jobs", "provider", "TEXT"); err != nil {
		return err
	}
	// precheck is an optional shell gate run before a job's real execution
	// (agent jobs mostly): exit 0 lets the job run (its stdout is appended
	// to the agent prompt as pending-item context), exit 1 marks the run
	// "skipped" without executing the job, exit >=2 marks it "failed".
	// empty/NULL means "no precheck", so existing jobs keep their behavior.
	if err := addColumnIfMissing(db, "jobs", "precheck", "TEXT"); err != nil {
		return err
	}
	// precheck_timeout_seconds bounds the gate itself; empty/NULL means
	// "no timeout".
	if err := addColumnIfMissing(db, "jobs", "precheck_timeout_seconds", "INTEGER"); err != nil {
		return err
	}
	// notify is the comma-separated list of run events that raise a Herdr
	// notification for this job (see runner.NotifyEvents). empty/NULL means
	// the default — failed and timeout, which is what every job did before
	// it was configurable — so existing jobs need no backfill.
	if err := addColumnIfMissing(db, "jobs", "notify", "TEXT"); err != nil {
		return err
	}
	// runner_pid, child_pid and exec_started_at are what lets a run that
	// never finishes be told apart from one that is merely slow (see
	// ActiveRuns). runner_pid is the jobtail process executing the run
	// (run-exec, `jobtail run`, or the dashboard); child_pid is the job's
	// own process, which leads its own process group so it can be killed
	// along with everything it spawned; exec_started_at is when execution
	// actually began, as opposed to started_at's nominal cron slot.
	for _, c := range [][2]string{
		{"runner_pid", "INTEGER"},
		{"child_pid", "INTEGER"},
		{"exec_started_at", "TEXT"},
	} {
		if err := addColumnIfMissing(db, "runs", c[0], c[1]); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, sqlType string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			has = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	if !has {
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + sqlType); err != nil {
			return err
		}
	}
	return nil
}

type Job struct {
	ID       string
	Kind     string // "cli" | "agent"
	Cron     string
	Timezone string // "local" or an IANA name
	Enabled  bool
	Cwd      string
	Command  string
	Prompt   string
	Model    string
	// Provider selects which agent CLI a kind="agent" job runs under:
	// "" (default, meaning "claude"), "opencode", or "codex". Unused for
	// kind="cli" jobs.
	Provider       string
	PermissionMode string
	MaxConcurrent  int
	// TimeoutSeconds is how long a run may take before jobtail kills it
	// and records it as "timeout". 0 means DefaultTimeout — there is no
	// "unlimited": a run that never ends would block the job forever.
	TimeoutSeconds int64
	Keep           int
	// Precheck is an optional shell gate (run like Command, via sh -c in
	// Cwd) evaluated before the job's real execution. "" = no precheck.
	Precheck string
	// PrecheckTimeoutSeconds bounds the precheck itself; 0 = no timeout.
	PrecheckTimeoutSeconds int64
	// Notify lists the run events that raise a Herdr notification,
	// comma-separated ("started,failed"). "" means the default (failed and
	// timeout), "none" turns them off. See runner.NotifyEvents.
	Notify    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultTimeout applies to every job that doesn't set TimeoutSeconds.
const DefaultTimeout = 30 * time.Minute

// Timeout is the job's effective run time limit.
func (j Job) Timeout() time.Duration {
	if j.TimeoutSeconds <= 0 {
		return DefaultTimeout
	}
	return time.Duration(j.TimeoutSeconds) * time.Second
}

type Run struct {
	ID         string
	JobID      string
	Trigger    string // "scheduled" | "manual" | "resume"
	Status     string // "running" | "ok" | "failed" | "timeout" | "skipped_overlap"
	StartedAt  time.Time
	FinishedAt sql.NullTime
	ExitCode   sql.NullInt64
	SessionID  sql.NullString
	LogPath    string
	// DurationMs is measured directly around the actual execution — see
	// additiveMigrations' comment for why this can't just be derived from
	// FinishedAt-StartedAt. Null for runs recorded before this field
	// existed; display code falls back to the timestamp difference then.
	DurationMs sql.NullInt64
}

// JobSummary is a Job plus derived run stats, as shown in `list`/the TUI job pane.
type JobSummary struct {
	Job
	RunCount   int
	LastStatus string // "" if never run
	LastRunAt  sql.NullTime
}

var ErrNotFound = errors.New("not found")
var ErrAlreadyExists = errors.New("already exists")
var ErrOverlap = errors.New("a run is already active for this job")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite: one *connection* per process is simplest and avoids intra-process lock churn; cross-process concurrency is handled by WAL + busy_timeout above.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := additiveMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func timeToStr(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func strToTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func (s *Store) CreateJob(ctx context.Context, j Job) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE id = ?`, j.ID).Scan(&exists); err == nil {
		return ErrAlreadyExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := timeToStr(time.Now())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, kind, cron, timezone, enabled, cwd, command, prompt, model, provider,
			permission_mode, max_concurrent, timeout_seconds, keep, precheck, precheck_timeout_seconds, notify,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Kind, j.Cron, j.Timezone, boolToInt(j.Enabled), j.Cwd,
		nullableStr(j.Command), nullableStr(j.Prompt), nullableStr(j.Model), nullableStr(j.Provider),
		nullableStr(j.PermissionMode),
		j.MaxConcurrent, nullableInt(j.TimeoutSeconds), j.Keep,
		nullableStr(j.Precheck), nullableInt(j.PrecheckTimeoutSeconds), nullableStr(j.Notify), now, now)
	return err
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, cron, timezone, enabled, cwd, command, prompt, model, provider,
		       permission_mode, max_concurrent, timeout_seconds, keep,
		       precheck, precheck_timeout_seconds, notify, created_at, updated_at
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

func scanJob(row *sql.Row) (Job, error) {
	var j Job
	var enabled int
	var command, prompt, model, provider, permMode, precheck, notify sql.NullString
	var timeoutSeconds, precheckTimeout sql.NullInt64
	var createdAt, updatedAt string
	err := row.Scan(&j.ID, &j.Kind, &j.Cron, &j.Timezone, &enabled, &j.Cwd, &command, &prompt, &model, &provider,
		&permMode, &j.MaxConcurrent, &timeoutSeconds, &j.Keep,
		&precheck, &precheckTimeout, &notify, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.Enabled = enabled != 0
	j.Command = command.String
	j.Prompt = prompt.String
	j.Model = model.String
	j.Provider = provider.String
	j.PermissionMode = permMode.String
	j.TimeoutSeconds = timeoutSeconds.Int64
	j.Precheck = precheck.String
	j.PrecheckTimeoutSeconds = precheckTimeout.Int64
	j.Notify = notify.String
	j.CreatedAt, err = strToTime(createdAt)
	if err != nil {
		return Job{}, err
	}
	j.UpdatedAt, err = strToTime(updatedAt)
	if err != nil {
		return Job{}, err
	}
	return j, nil
}

// ListJobs is the dashboard's per-refresh job query, so its three correlated
// subqueries are worth a note: they look like the textbook case for a single
// grouped LEFT JOIN over runs, and that rewrite was written, verified to
// produce identical results, benchmarked — and thrown away for being twice as
// slow (1.73 ms vs 0.86 ms at 50 jobs x 200 runs; see
// BenchmarkListJobs). idx_runs_job(job_id, started_at DESC) turns each
// subquery into an index seek or a covered range scan, while GROUP BY has to
// scan every run row and materialize a temporary b-tree to join back. Don't
// "fix" this without measuring it.
func (s *Store) ListJobs(ctx context.Context) ([]JobSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, j.kind, j.cron, j.timezone, j.enabled, j.cwd, j.command, j.prompt, j.model, j.provider,
		       j.permission_mode, j.max_concurrent, j.timeout_seconds, j.keep, j.precheck, j.precheck_timeout_seconds, j.notify,
		       j.created_at, j.updated_at,
		       (SELECT COUNT(*) FROM runs r WHERE r.job_id = j.id) AS run_count,
		       (SELECT r2.status FROM runs r2 WHERE r2.job_id = j.id ORDER BY r2.started_at DESC LIMIT 1) AS last_status,
		       (SELECT r3.started_at FROM runs r3 WHERE r3.job_id = j.id ORDER BY r3.started_at DESC LIMIT 1) AS last_run_at
		FROM jobs j ORDER BY j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobSummary
	for rows.Next() {
		var js JobSummary
		var enabled int
		var command, prompt, model, provider, permMode, precheck, notify, lastStatus, lastRunAt sql.NullString
		var timeoutSeconds, precheckTimeout sql.NullInt64
		var createdAt, updatedAt string
		if err := rows.Scan(&js.ID, &js.Kind, &js.Cron, &js.Timezone, &enabled, &js.Cwd, &command, &prompt, &model, &provider,
			&permMode, &js.MaxConcurrent, &timeoutSeconds, &js.Keep, &precheck, &precheckTimeout, &notify,
			&createdAt, &updatedAt,
			&js.RunCount, &lastStatus, &lastRunAt); err != nil {
			return nil, err
		}
		js.Enabled = enabled != 0
		js.Command = command.String
		js.Prompt = prompt.String
		js.Model = model.String
		js.Provider = provider.String
		js.PermissionMode = permMode.String
		js.TimeoutSeconds = timeoutSeconds.Int64
		js.Precheck = precheck.String
		js.PrecheckTimeoutSeconds = precheckTimeout.Int64
		js.Notify = notify.String
		js.LastStatus = lastStatus.String
		if js.CreatedAt, err = strToTime(createdAt); err != nil {
			return nil, err
		}
		if js.UpdatedAt, err = strToTime(updatedAt); err != nil {
			return nil, err
		}
		if lastRunAt.Valid {
			t, err := strToTime(lastRunAt.String)
			if err != nil {
				return nil, err
			}
			js.LastRunAt = sql.NullTime{Time: t, Valid: true}
		}
		out = append(out, js)
	}
	return out, rows.Err()
}

func (s *Store) SetEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), timeToStr(time.Now()), id)
	if err != nil {
		return err
	}
	return checkRowsAffected(res)
}

// DeleteJob removes a job and its run history, returning the deleted runs'
// log paths so the caller can also remove those files — DELETE FROM runs
// only removes the database rows, never touches the log files on disk.
func (s *Store) DeleteJob(ctx context.Context, id string) ([]string, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if err := checkRowsAffected(res); err != nil {
		return nil, err
	}

	var logPaths []string
	rows, err := s.db.QueryContext(ctx, `SELECT log_path FROM runs WHERE job_id = ?`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		logPaths = append(logPaths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	_, err = s.db.ExecContext(ctx, `DELETE FROM runs WHERE job_id = ?`, id)
	return logPaths, err
}

// EditJob applies a sparse patch: zero-value fields in patch are left unchanged
// except where the corresponding *Set flag is true.
type JobPatch struct {
	Cron                   *string
	Timezone               *string
	Cwd                    *string
	Command                *string
	Prompt                 *string
	Model                  *string
	Provider               *string
	PermissionMode         *string
	MaxConcurrent          *int
	TimeoutSeconds         *int64
	Keep                   *int
	Precheck               *string
	PrecheckTimeoutSeconds *int64
	Notify                 *string
}

func (s *Store) EditJob(ctx context.Context, id string, p JobPatch) error {
	j, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if p.Cron != nil {
		j.Cron = *p.Cron
	}
	if p.Timezone != nil {
		j.Timezone = *p.Timezone
	}
	if p.Cwd != nil {
		j.Cwd = *p.Cwd
	}
	if p.Command != nil {
		j.Command = *p.Command
	}
	if p.Prompt != nil {
		j.Prompt = *p.Prompt
	}
	if p.Model != nil {
		j.Model = *p.Model
	}
	if p.Provider != nil {
		j.Provider = *p.Provider
	}
	if p.PermissionMode != nil {
		j.PermissionMode = *p.PermissionMode
	}
	if p.MaxConcurrent != nil {
		j.MaxConcurrent = *p.MaxConcurrent
	}
	if p.TimeoutSeconds != nil {
		j.TimeoutSeconds = *p.TimeoutSeconds
	}
	if p.Keep != nil {
		j.Keep = *p.Keep
	}
	if p.Precheck != nil {
		j.Precheck = *p.Precheck
	}
	if p.PrecheckTimeoutSeconds != nil {
		j.PrecheckTimeoutSeconds = *p.PrecheckTimeoutSeconds
	}
	if p.Notify != nil {
		j.Notify = *p.Notify
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET cron=?, timezone=?, cwd=?, command=?, prompt=?, model=?, provider=?, permission_mode=?,
			max_concurrent=?, timeout_seconds=?, keep=?, precheck=?, precheck_timeout_seconds=?, notify=?, updated_at=?
		WHERE id=?`,
		j.Cron, j.Timezone, j.Cwd, nullableStr(j.Command), nullableStr(j.Prompt), nullableStr(j.Model),
		nullableStr(j.Provider), nullableStr(j.PermissionMode), j.MaxConcurrent, nullableInt(j.TimeoutSeconds), j.Keep,
		nullableStr(j.Precheck), nullableInt(j.PrecheckTimeoutSeconds), nullableStr(j.Notify),
		timeToStr(time.Now()), id)
	return err
}

// activeRunCount returns how many runs for jobID are still status='running'.
func (s *Store) activeRunCount(ctx context.Context, jobID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id = ? AND status = 'running'`, jobID).Scan(&n)
	return n, err
}

// StartRun creates a new run row with status='running'. If the job's
// max_concurrent is already met by in-flight runs, it instead records a
// status='skipped_overlap' row and returns ErrOverlap.
func (s *Store) StartRun(ctx context.Context, jobID, runID, trigger, logPath string, startedAt time.Time) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()

	j, err := s.getJobTx(ctx, tx, jobID)
	if err != nil {
		return Run{}, err
	}

	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id = ? AND status = 'running'`, jobID).Scan(&active); err != nil {
		return Run{}, err
	}

	r := Run{ID: runID, JobID: jobID, Trigger: trigger, StartedAt: startedAt, LogPath: logPath}
	overlap := active >= j.MaxConcurrent
	if overlap {
		r.Status = "skipped_overlap"
	} else {
		r.Status = "running"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs (id, job_id, trigger, status, started_at, log_path) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.JobID, r.Trigger, r.Status, timeToStr(r.StartedAt), r.LogPath); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	if overlap {
		return r, ErrOverlap
	}
	return r, nil
}

func (s *Store) getJobTx(ctx context.Context, tx *sql.Tx, id string) (Job, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, kind, cron, timezone, enabled, cwd, command, prompt, model, provider,
		       permission_mode, max_concurrent, timeout_seconds, keep,
		       precheck, precheck_timeout_seconds, notify, created_at, updated_at
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

func (s *Store) SetRunSessionID(ctx context.Context, runID, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET session_id = ? WHERE id = ?`, sessionID, runID)
	return err
}

// FinishRun records a run's outcome. Only a run that is still "running"
// is updated: once the reaper (runner.ReapStale) has finalized a run, the
// process that was executing it may still get round to reporting — having
// just been killed by that same reaper, say — and must not overwrite the
// verdict with a less accurate one. That case returns nil, not an error.
func (s *Store) FinishRun(ctx context.Context, runID, status string, exitCode int, finishedAt time.Time, durationMs int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, exit_code = ?, finished_at = ?, duration_ms = ?
		WHERE id = ? AND status = 'running'`,
		status, exitCode, timeToStr(finishedAt), durationMs, runID)
	if err != nil {
		return err
	}
	if err := checkRowsAffected(res); !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = s.GetRun(ctx, runID)
	return err // nil if the run exists and was already finished
}

// MarkRunExecuting records which process is executing a run and when it
// actually began. It is what lets ActiveRuns' callers tell a run whose
// executor died apart from one that is still working.
func (s *Store) MarkRunExecuting(ctx context.Context, runID string, runnerPID int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET runner_pid = ?, exec_started_at = ? WHERE id = ?`,
		runnerPID, timeToStr(at), runID)
	return err
}

// SetRunChildPID records the pid of the process the run is currently
// waiting on — the precheck gate, then the job itself. That process leads
// its own process group, so the pid doubles as the group to kill.
func (s *Store) SetRunChildPID(ctx context.Context, runID string, pid int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET child_pid = ? WHERE id = ?`, pid, runID)
	return err
}

// ActiveRun is a status='running' run with what is needed to decide
// whether it is still genuinely running.
type ActiveRun struct {
	Run
	RunnerPID     sql.NullInt64
	ChildPID      sql.NullInt64
	ExecStartedAt sql.NullTime
	// Timeout is the owning job's effective limit.
	Timeout time.Duration
	// Notify is the owning job's notification setting (Job.Notify).
	Notify string
}

// ActiveRuns returns every run still marked "running", across all jobs.
func (s *Store) ActiveRuns(ctx context.Context) ([]ActiveRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.job_id, r.trigger, r.started_at, r.log_path,
		       r.runner_pid, r.child_pid, r.exec_started_at, j.timeout_seconds, j.notify
		FROM runs r JOIN jobs j ON j.id = r.job_id
		WHERE r.status = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveRun
	for rows.Next() {
		var a ActiveRun
		var startedAt string
		var execStartedAt sql.NullString
		var timeoutSeconds sql.NullInt64
		var notify sql.NullString
		if err := rows.Scan(&a.ID, &a.JobID, &a.Trigger, &startedAt, &a.LogPath,
			&a.RunnerPID, &a.ChildPID, &execStartedAt, &timeoutSeconds, &notify); err != nil {
			return nil, err
		}
		a.Status = "running"
		if a.StartedAt, err = strToTime(startedAt); err != nil {
			return nil, err
		}
		if execStartedAt.Valid {
			t, err := strToTime(execStartedAt.String)
			if err != nil {
				return nil, err
			}
			a.ExecStartedAt = sql.NullTime{Time: t, Valid: true}
		}
		a.Timeout = Job{TimeoutSeconds: timeoutSeconds.Int64}.Timeout()
		a.Notify = notify.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, job_id, trigger, status, started_at, finished_at, exit_code, session_id, log_path, duration_ms
		FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

func scanRun(row *sql.Row) (Run, error) {
	var r Run
	var startedAt string
	var finishedAt, sessionID sql.NullString
	var exitCode, durationMs sql.NullInt64
	err := row.Scan(&r.ID, &r.JobID, &r.Trigger, &r.Status, &startedAt, &finishedAt, &exitCode, &sessionID, &r.LogPath, &durationMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if r.StartedAt, err = strToTime(startedAt); err != nil {
		return Run{}, err
	}
	if finishedAt.Valid {
		t, err := strToTime(finishedAt.String)
		if err != nil {
			return Run{}, err
		}
		r.FinishedAt = sql.NullTime{Time: t, Valid: true}
	}
	r.ExitCode = exitCode
	r.DurationMs = durationMs
	r.SessionID = sessionID
	return r, nil
}

func (s *Store) ListRuns(ctx context.Context, jobID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, trigger, status, started_at, finished_at, exit_code, session_id, log_path, duration_ms
		FROM runs WHERE job_id = ? ORDER BY started_at DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var startedAt string
		var finishedAt, sessionID sql.NullString
		var exitCode, durationMs sql.NullInt64
		if err := rows.Scan(&r.ID, &r.JobID, &r.Trigger, &r.Status, &startedAt, &finishedAt, &exitCode, &sessionID, &r.LogPath, &durationMs); err != nil {
			return nil, err
		}
		if r.StartedAt, err = strToTime(startedAt); err != nil {
			return nil, err
		}
		if finishedAt.Valid {
			t, err := strToTime(finishedAt.String)
			if err != nil {
				return nil, err
			}
			r.FinishedAt = sql.NullTime{Time: t, Valid: true}
		}
		r.ExitCode = exitCode
		r.DurationMs = durationMs
		r.SessionID = sessionID
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRun returns the most recent run for a job matching one of the given
// triggers (pass nil for any trigger), or ErrNotFound if there is none.
func (s *Store) LastRun(ctx context.Context, jobID string, triggers ...string) (Run, error) {
	q := `SELECT id, job_id, trigger, status, started_at, finished_at, exit_code, session_id, log_path, duration_ms
		FROM runs WHERE job_id = ?`
	args := []any{jobID}
	if len(triggers) > 0 {
		q += ` AND trigger IN (` + placeholders(len(triggers)) + `)`
		for _, t := range triggers {
			args = append(args, t)
		}
	}
	q += ` ORDER BY started_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, args...)
	return scanRun(row)
}

func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

// EnabledJobs returns every job with enabled=1.
func (s *Store) EnabledJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, cron, timezone, enabled, cwd, command, prompt, model, provider,
		       permission_mode, max_concurrent, timeout_seconds, keep,
		       precheck, precheck_timeout_seconds, notify, created_at, updated_at
		FROM jobs WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		var command, prompt, model, provider, permMode, precheck, notify sql.NullString
		var timeoutSeconds, precheckTimeout sql.NullInt64
		var createdAt, updatedAt string
		if err := rows.Scan(&j.ID, &j.Kind, &j.Cron, &j.Timezone, &enabled, &j.Cwd, &command, &prompt, &model, &provider,
			&permMode, &j.MaxConcurrent, &timeoutSeconds, &j.Keep,
			&precheck, &precheckTimeout, &notify, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		j.Enabled = enabled != 0
		j.Command = command.String
		j.Prompt = prompt.String
		j.Model = model.String
		j.Provider = provider.String
		j.PermissionMode = permMode.String
		j.TimeoutSeconds = timeoutSeconds.Int64
		j.Precheck = precheck.String
		j.PrecheckTimeoutSeconds = precheckTimeout.Int64
		j.Notify = notify.String
		if j.CreatedAt, err = strToTime(createdAt); err != nil {
			return nil, err
		}
		if j.UpdatedAt, err = strToTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GC prunes runs beyond each job's retention count (job.Keep), deleting the
// oldest rows first. It returns the deleted run IDs so the caller can also
// remove their log files.
func (s *Store) GC(ctx context.Context) ([]string, error) {
	jobs, err := s.allJobIDsAndKeep(ctx)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for jobID, keep := range jobs {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id FROM runs WHERE job_id = ? ORDER BY started_at DESC LIMIT -1 OFFSET ?`, jobID, keep)
		if err != nil {
			return deleted, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return deleted, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id); err != nil {
				return deleted, err
			}
			deleted = append(deleted, id)
		}
	}
	return deleted, nil
}

func (s *Store) allJobIDsAndKeep(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, keep FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var keep int
		if err := rows.Scan(&id, &keep); err != nil {
			return nil, err
		}
		out[id] = keep
	}
	return out, rows.Err()
}

func checkRowsAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
