// Package cronx wraps robfig/cron/v3's schedule parser — only the parser,
// not its scheduler goroutine, since jobtail's own timing comes from a
// systemd timer invoking `jobtail tick` once a minute (see PRD §12 decision 10).
package cronx

import (
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Next returns the next time the standard 5-field cron expression fires
// strictly after `from`, evaluated in the given timezone ("local" means the
// system's local timezone; anything else is parsed as an IANA name).
func Next(expr, timezone string, from time.Time) (time.Time, error) {
	sched, err := parser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse cron %q: %w", expr, err)
	}
	loc, err := loadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from.In(loc)), nil
}

// Due reports whether a job whose last scheduled fire (or creation time, if
// it has never fired) was `lastFire` should fire again at or before `now`,
// and if so what that fire instant is.
func Due(expr, timezone string, lastFire, now time.Time) (due bool, at time.Time, err error) {
	next, err := Next(expr, timezone, lastFire)
	if err != nil {
		return false, time.Time{}, err
	}
	if next.After(now) {
		return false, time.Time{}, nil
	}
	return true, next, nil
}

// locCache memoizes time.LoadLocation, which reads and parses the zoneinfo
// file every call — it caches nothing but "UTC" and "Local". That read landed
// on two paths that repeat it: `jobtail tick` asks for every enabled job's
// next fire once a minute, and the dashboard's Next column asks for every
// job's on every rebuild of the jobs table. Measured, a job on an IANA
// timezone cost 5.3 µs per Next against 0.93 µs for a local one, all of the
// difference being that lookup. A location for a given name never changes
// within a process, so once is enough.
var locCache sync.Map // timezone name -> *time.Location

func loadLocation(timezone string) (*time.Location, error) {
	if timezone == "" || timezone == "local" {
		return time.Local, nil
	}
	if cached, ok := locCache.Load(timezone); ok {
		return cached.(*time.Location), nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		// Deliberately not cached: a failure is a bad job definition worth
		// re-reporting, and there are only ever a handful of them.
		return nil, fmt.Errorf("load timezone %q: %w", timezone, err)
	}
	locCache.Store(timezone, loc)
	return loc, nil
}

// Validate parses expr purely to surface a syntax error early (e.g. from
// `jobtail add`/`edit`), without needing a reference time.
func Validate(expr string) error {
	_, err := parser.Parse(expr)
	return err
}
