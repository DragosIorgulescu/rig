// Package cronschedule is the schedule calculator backed by robfig/cron's
// standard parser: five-field cron expressions and descriptors such as
// "@daily" and "@every 2h", in local time.
package cronschedule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BaronBonet/rig/internal/core"

	"github.com/robfig/cron/v3"
)

type calculator struct {
	// location is the time zone schedules run in: the machine's own.
	location *time.Location
}

// New returns the cron schedule calculator, which works in the machine's local
// time zone.
func New() core.ScheduleCalculator {
	return calculator{location: time.Now().Location()}
}

// Validate reports why expr is not a schedule that fires: it does not parse,
// names a time zone, or never comes up (a 30th of February).
func (c calculator) Validate(expr string) error {
	schedule, err := parse(expr)
	if err != nil {
		return err
	}
	if schedule.Next(time.Now().In(c.location)).IsZero() {
		return errors.New("it never fires")
	}
	return nil
}

// Next returns the first time after the given time that expr fires, in local
// time.
func (c calculator) Next(expr string, after time.Time) (time.Time, error) {
	schedule, err := parse(expr)
	if err != nil {
		return time.Time{}, err
	}
	next := schedule.Next(after.In(c.location))
	if next.IsZero() {
		return time.Time{}, errors.New("it never fires")
	}
	return next, nil
}

// parse reads expr with the standard parser only. A time zone prefix is
// refused: schedules run in local time, so that "9am" means the user's 9am.
func parse(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	upper := strings.ToUpper(expr)
	if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") {
		return nil, errors.New("time zones are not supported: schedules run in local time")
	}
	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("not a cron schedule: %w", err)
	}
	return schedule, nil
}
