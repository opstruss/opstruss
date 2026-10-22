package daemon

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// starBit marks a field written as *, as robfig/cron marks it.
const starBit = 1 << 63

// cronSpec is a cron schedule in its time zone. robfig/cron reads it as
// CronJob of Kubernetes does; when it runs is worked out here, as cronie
// does it around a change of daylight saving time: a job at a fixed time
// that the clock skips runs right after the jump, and one at a fixed time
// that the clock repeats runs once, while a job whose minute or hour is a
// wildcard follows the clock.
type cronSpec struct {
	s     *cron.SpecSchedule
	loc   *time.Location
	fixed bool
}

func parseCron(expr, timezone string) (*cronSpec, error) {
	loc := time.Local
	if timezone != "" {
		var err error
		if loc, err = time.LoadLocation(timezone); err != nil {
			return nil, err
		}
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, err
	}
	s, ok := sched.(*cron.SpecSchedule)
	if !ok {
		return nil, errors.New("not a cron schedule")
	}
	c := &cronSpec{s: s, loc: loc}
	if f := strings.Fields(expr); strings.HasPrefix(expr, "@") {
		c.fixed = expr != "@hourly"
	} else {
		c.fixed = !strings.HasPrefix(f[0], "*") && !strings.HasPrefix(f[1], "*")
	}
	return c, nil
}

func has(bits uint64, n int) bool { return bits&(1<<uint(n)) != 0 }

// dayMatches applies the day of the month and of the week as cron does:
// both must match when either is *, and either may otherwise.
func (c *cronSpec) dayMatches(day time.Time) bool {
	dom, dow := has(c.s.Dom, day.Day()), has(c.s.Dow, int(day.Weekday()))
	if c.s.Dom&starBit != 0 || c.s.Dow&starBit != 0 {
		return dom && dow
	}
	return dom || dow
}

// next is the first time after t when the schedule runs, or the zero time
// when it never does in the next five years.
func (c *cronSpec) next(t time.Time) time.Time {
	t = t.In(c.loc)
	y, m, d := t.Date()
	var best time.Time
	for i := 0; i < 5*366; i++ {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, c.loc)
		// Instants of two days next to a change of time can overlap.
		if !best.IsZero() && day.Sub(best) > 48*time.Hour {
			break
		}
		if !has(c.s.Month, int(day.Month())) || !c.dayMatches(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if !has(c.s.Hour, h) {
				continue
			}
			for mi := 0; mi < 60; mi++ {
				if !has(c.s.Minute, mi) {
					continue
				}
				for _, at := range c.instants(day.Year(), day.Month(), day.Day(), h, mi) {
					if at.After(t) && (best.IsZero() || at.Before(best)) {
						best = at
					}
				}
			}
		}
	}
	return best
}

// instants are when the clock of the time zone shows the minute: none in
// a gap that a change of time skips, two in an hour that it repeats. A
// fixed job runs at the end of a gap instead, and in a repeated hour only
// the first time.
func (c *cronSpec) instants(y int, m time.Month, d, h, mi int) []time.Time {
	wall := time.Date(y, m, d, h, mi, 0, 0, time.UTC)
	_, before := time.Date(y, m, d-1, h, mi, 0, 0, c.loc).Zone()
	_, after := time.Date(y, m, d+1, h, mi, 0, 0, c.loc).Zone()
	var out []time.Time
	for _, off := range []int{before, after} {
		at := wall.Add(-time.Duration(off) * time.Second).In(c.loc)
		if clock(at).Equal(wall) && !slices.ContainsFunc(out, at.Equal) {
			out = append(out, at)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	switch {
	case len(out) == 0 && c.fixed:
		// The clock skips the minute: the first minute after the jump.
		lo, hi := wall.Add(-time.Duration(max(before, after))*time.Second), wall.Add(-time.Duration(min(before, after))*time.Second)
		for at := lo; !at.After(hi); at = at.Add(time.Minute) {
			if !clock(at.In(c.loc)).Before(wall) {
				return []time.Time{at.In(c.loc)}
			}
		}
	case len(out) > 1 && c.fixed:
		return out[:1]
	}
	return out
}

// clock is the date and time that t shows in its time zone, as UTC, so
// that two of them compare as clocks do.
func clock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}
