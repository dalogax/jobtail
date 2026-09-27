// Benchmarks for the data layer's two hot paths.
//
// Both are hot for the same reason: the dashboard re-runs them on every
// refresh tick, so their cost is paid once per second for as long as
// anyone leaves jobtail open — and Open is paid again by every single CLI
// invocation, including the `jobtail tick` the systemd timer fires once a
// minute forever.
//
// Dataset shapes matter more than absolute numbers here: a store with 200
// runs per job (the default `keep`) behaves nothing like the 1-run store
// the unit tests use, and the difference is exactly what ListJobs'
// correlated subqueries are sensitive to.
package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seedBench builds a store with jobs jobs and runsPer finished runs each.
// It writes through raw SQL in one transaction rather than StartRun /
// FinishRun: seeding 10k runs one committed transaction at a time takes
// longer than the benchmark it sets up.
func seedBench(tb testing.TB, jobs, runsPer int) *Store {
	tb.Helper()
	st, err := Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })

	ctx := context.Background()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	now := time.Now()
	for j := 0; j < jobs; j++ {
		id := fmt.Sprintf("job-%03d", j)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, cron, timezone, enabled, cwd, command,
				max_concurrent, keep, created_at, updated_at)
			VALUES (?, 'cli', '*/5 * * * *', 'local', 1, '/tmp', 'echo hi', 1, 200, ?, ?)`,
			id, timeToStr(now), timeToStr(now)); err != nil {
			tb.Fatal(err)
		}
		for r := 0; r < runsPer; r++ {
			started := now.Add(-time.Duration(r) * time.Minute)
			status := "ok"
			if r%7 == 0 {
				status = "failed"
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO runs (id, job_id, trigger, status, started_at, finished_at,
					exit_code, log_path, duration_ms)
				VALUES (?, ?, 'scheduled', ?, ?, ?, 0, ?, ?)`,
				fmt.Sprintf("%s-run-%05d", id, r), id, status,
				timeToStr(started), timeToStr(started.Add(2*time.Second)),
				fmt.Sprintf("/tmp/logs/%s-run-%05d.log", id, r), 2000); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return st
}

// BenchmarkListJobs is the dashboard's per-tick job-pane query. The shapes
// span a fresh install (5 jobs, a handful of runs) to a year-old one that
// has hit its retention ceiling on every job.
func BenchmarkListJobs(b *testing.B) {
	for _, shape := range []struct{ jobs, runsPer int }{
		{5, 10}, {20, 200}, {50, 200},
	} {
		b.Run(fmt.Sprintf("jobs=%d/runs=%d", shape.jobs, shape.runsPer), func(b *testing.B) {
			st := seedBench(b, shape.jobs, shape.runsPer)
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := st.ListJobs(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkListRuns is the per-tick runs-pane query for the selected job.
func BenchmarkListRuns(b *testing.B) {
	st := seedBench(b, 20, 200)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.ListRuns(ctx, "job-005", 200); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpen is the startup tax: every CLI invocation and every
// once-a-minute `jobtail tick` opens the database from scratch, which
// means running the schema DDL and the additive migrations' PRAGMA probes
// against a database that is already fully migrated.
func BenchmarkOpen(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "open.db")
	st, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO jobs (id, kind, cron, timezone, enabled, cwd,
		command, max_concurrent, keep, created_at, updated_at)
		VALUES ('j','cli','* * * * *','local',1,'/tmp','echo',1,200,?,?)`,
		timeToStr(time.Now()), timeToStr(time.Now())); err != nil {
		b.Fatal(err)
	}
	st.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}
