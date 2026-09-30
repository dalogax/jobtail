package runner

import (
	"context"
	"fmt"
	"time"

	"github.com/dalogax/jobtail/internal/execengine"
	"github.com/dalogax/jobtail/internal/store"
)

// ReapGrace is how far past its max time a run may still be marked
// "running" before ReapStale steps in. The executing process enforces the
// limit itself (Execute's deadline, then SIGTERM, then SIGKILL after a few
// seconds); the reaper is only the backstop for when that process is gone
// or stuck, so it waits long enough never to race a kill already underway.
const ReapGrace = time.Minute

// Reaped describes one run ReapStale finalized.
type Reaped struct {
	RunID  string
	JobID  string
	Status string // "failed" | "timeout"
	Reason string
}

// ReapStale finalizes every run that is marked "running" but can no longer
// finish by itself, so it stops blocking its job's next run:
//
//   - its executing process (run-exec, `jobtail run`, or the dashboard) is
//     gone — killed, crashed, lost to a reboot — and so will never record
//     an outcome. Marked "failed", or "timeout" if it is also past its max
//     time.
//   - it is more than ReapGrace past its max time, however alive its
//     executor claims to be. Marked "timeout". This also covers rows from
//     before jobtail recorded executors at all, and runs whose run-exec
//     never came up.
//
// Either way, whatever is left of the job's process group is killed first.
// It runs at the top of every tick and before every manual run, so a stuck
// run is cleared within a minute of becoming stuck rather than waiting for
// someone to notice the job stopped firing.
func ReapStale(ctx context.Context, st *store.Store, now time.Time) ([]Reaped, error) {
	active, err := st.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []Reaped
	for _, a := range active {
		began := a.StartedAt
		if a.ExecStartedAt.Valid {
			began = a.ExecStartedAt.Time
		}
		overdue := now.After(began.Add(a.MaxTime + ReapGrace))
		runnerGone := a.RunnerPID.Valid && !execengine.ProcessAlive(int(a.RunnerPID.Int64))
		if !overdue && !runnerGone {
			continue
		}

		r := Reaped{RunID: a.ID, JobID: a.JobID, Status: "timeout"}
		switch {
		case runnerGone && !now.After(began.Add(a.MaxTime)):
			r.Status = "failed"
			r.Reason = fmt.Sprintf("the jobtail process executing it (pid %d) exited without recording an outcome", a.RunnerPID.Int64)
		case runnerGone:
			r.Reason = fmt.Sprintf("it exceeded its max time of %s and the jobtail process executing it (pid %d) is gone", a.MaxTime, a.RunnerPID.Int64)
		default:
			r.Reason = fmt.Sprintf("it was still marked running %s past its max time of %s", now.Sub(began.Add(a.MaxTime)).Round(time.Second), a.MaxTime)
		}
		if a.ChildPID.Valid && execengine.KillProcessGroup(int(a.ChildPID.Int64)) {
			r.Reason += fmt.Sprintf("; killed its process group %d", a.ChildPID.Int64)
		}

		appendLog(a.LogPath, "\n[jobtail] reaped: "+r.Reason+"\n")
		dur := now.Sub(began)
		if dur < 0 {
			dur = 0
		}
		if err := st.FinishRun(ctx, a.ID, r.Status, -1, now, dur.Milliseconds()); err != nil {
			return out, err
		}
		notifyFailure(a.JobID, r.Status)
		out = append(out, r)
	}
	return out, nil
}
