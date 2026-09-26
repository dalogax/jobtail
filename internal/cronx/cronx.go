// Package cronx wraps robfig/cron/v3's schedule parser — only the parser,
// not its scheduler goroutine, since jobtail's own timing comes from a
// systemd timer invoking `jobtail tick` once a minute (see PRD §12 decision 10).
package cronx

import (
	"fmt"
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

func loadLocation(timezone string) (*time.Location, error) {
	if timezone == "" || timezone == "local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", timezone, err)
	}
	return loc, nil
}

// Validate parses expr purely to surface a syntax error early (e.g. from
// `jobtail add`/`edit`), without needing a reference time.
func Validate(expr string) error {
	_, err := parser.Parse(expr)
	return err
}
