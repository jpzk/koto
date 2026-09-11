package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 5-field POSIX-style crontab parser. Pure stdlib so we don't violate the
// project's supply-chain rule by pulling robfig/cron. Fields, in order:
//
//   minute (0-59) hour (0-23) day-of-month (1-31) month (1-12) day-of-week (0-6, 0=Sun)
//
// Per field we accept: `*`, `N`, `N-M`, `N,M,K`, `*/N`, `N-M/K`, `N/K`.
// Aliases: @yearly @annually @monthly @weekly @daily @midnight @hourly.
//
// POSIX OR-semantics: when BOTH day-of-month and day-of-week are restricted
// (i.e. neither is `*`), a date matches if either field matches. When at
// least one is `*`, the other field filters normally.

type parsedCron struct {
	min, hour, dom, mon, dow [64]bool
	domStar, dowStar         bool
}

var cronAliases = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

func parseCron(expr string) (parsedCron, error) {
	expr = strings.TrimSpace(expr)
	if alias, ok := cronAliases[expr]; ok {
		expr = alias
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return parsedCron{}, fmt.Errorf("cron: expected 5 fields, got %d (%q)", len(fields), expr)
	}
	var p parsedCron
	var err error
	if p.min, _, err = parseField(fields[0], 0, 59); err != nil {
		return p, fmt.Errorf("minute: %w", err)
	}
	if p.hour, _, err = parseField(fields[1], 0, 23); err != nil {
		return p, fmt.Errorf("hour: %w", err)
	}
	if p.dom, p.domStar, err = parseField(fields[2], 1, 31); err != nil {
		return p, fmt.Errorf("day-of-month: %w", err)
	}
	if p.mon, _, err = parseField(fields[3], 1, 12); err != nil {
		return p, fmt.Errorf("month: %w", err)
	}
	if p.dow, p.dowStar, err = parseField(fields[4], 0, 6); err != nil {
		return p, fmt.Errorf("day-of-week: %w", err)
	}
	return p, nil
}

func parseField(s string, lo, hi int) ([64]bool, bool, error) {
	var mask [64]bool
	star := s == "*"
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			stepStr := part[i+1:]
			n, err := strconv.Atoi(stepStr)
			// Bounded by the field's own span, not merely positive. The
			// expansion below is `for v := from; v <= to; v += step`, so a
			// step near MaxInt64 wrapped v to a NEGATIVE value on the second
			// iteration, left the loop condition true, and panicked on
			// mask[v] — taking the whole daemon down, from any schedule-
			// capable caller including a guest's sched_add (audit M26).
			// Rejecting rather than clamping: a step wider than the range can
			// only ever select `from`, so every value above the span is a
			// typo, and silently accepting it would hide the typo.
			if err != nil || n < 1 || n > hi-lo+1 {
				return mask, false, fmt.Errorf("bad step %q (must be 1..%d)", stepStr, hi-lo+1)
			}
			step = n
			part = part[:i]
		}
		var from, to int
		switch {
		case part == "*":
			from, to = lo, hi
		case strings.Contains(part, "-"):
			i := strings.Index(part, "-")
			f, err := strconv.Atoi(part[:i])
			if err != nil {
				return mask, false, fmt.Errorf("bad range start %q", part)
			}
			t, err := strconv.Atoi(part[i+1:])
			if err != nil {
				return mask, false, fmt.Errorf("bad range end %q", part)
			}
			from, to = f, t
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return mask, false, fmt.Errorf("bad value %q", part)
			}
			if step > 1 {
				// `N/step` expands to N..hi stepping by step.
				from, to = n, hi
			} else {
				from, to = n, n
			}
		}
		if from < lo || to > hi || from > to {
			return mask, false, fmt.Errorf("range %d-%d out of bounds %d-%d", from, to, lo, hi)
		}
		for v := from; v <= to; v += step {
			mask[v] = true
		}
	}
	return mask, star, nil
}

// dateMatches applies POSIX cron's day semantics: day-of-month AND day-of-week
// when at least one field is unrestricted (the `*` is always true), OR when
// both are restricted.
func (p parsedCron) dateMatches(t time.Time) bool {
	domOK, dowOK := p.dom[t.Day()], p.dow[int(t.Weekday())]
	if p.domStar || p.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// next returns the next time strictly after `after` that matches the
// expression, in the receiver's local time zone. Returns ok=false if no match
// is found within 4 years (effectively unsatisfiable, e.g. `0 0 30 2 *` —
// February 30th; four years so a `29 2` leap-day expression still resolves).
//
// It advances by the LARGEST unit that cannot match, not a minute at a time
// (audit M158). The minute-at-a-time walk had to visit every one of the ~2.1
// million minutes in the horizon before it could answer "never" — for an
// expression naming a date that does not exist, which the parser accepts
// because each field is individually in range. That scan runs on the gRPC
// handler with no budget and no cancellation, and toggleSched and the
// due-entry rescheduling run it while holding schedLock, so it blocks every
// scheduler operation for its duration. Skipping to the next candidate month,
// day or hour turns the same answer into a few hundred iterations: a
// non-matching month jumps to the 1st of the next one, a non-matching day to
// the next midnight, a non-matching hour to the next hour.
func (p parsedCron) next(after time.Time) (time.Time, bool) {
	t := after.Truncate(time.Minute).Add(time.Minute)
	end := t.Add(4 * 365 * 24 * time.Hour)
	// Every jump below must move FORWARD. They do, for every zone reasoned
	// through (including both sides of a DST fold), but a loop that fails to
	// advance is the exact failure class this function is being fixed for, so
	// the step is checked rather than trusted: anything that does not advance
	// falls back to one minute.
	step := func(to time.Time) time.Time {
		if to.After(t) {
			return to
		}
		return t.Add(time.Minute)
	}
	for t.Before(end) {
		if !p.mon[int(t.Month())] {
			// First minute of the next month.
			t = step(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0))
			continue
		}
		if !p.dateMatches(t) {
			// Midnight of the next day. AddDate normalises past month ends,
			// which is also what makes the impossible dates terminate: the
			// walk crosses months at day speed instead of minute speed.
			t = step(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1))
			continue
		}
		if !p.hour[t.Hour()] {
			t = step(time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(time.Hour))
			continue
		}
		if !p.min[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}
