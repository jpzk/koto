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

// next returns the next time strictly after `after` that matches the
// expression, in the receiver's local time zone. Returns ok=false if no
// match is found within 4 years (effectively unsatisfiable, e.g.
// `0 0 30 2 *` — February 30th).
func (p parsedCron) next(after time.Time) (time.Time, bool) {
	t := after.Truncate(time.Minute).Add(time.Minute)
	end := t.Add(4 * 365 * 24 * time.Hour)
	for ; t.Before(end); t = t.Add(time.Minute) {
		if !p.min[t.Minute()] {
			continue
		}
		if !p.hour[t.Hour()] {
			continue
		}
		if !p.mon[int(t.Month())] {
			continue
		}
		domOK := p.dom[t.Day()]
		dowOK := p.dow[int(t.Weekday())]
		var dateOK bool
		if p.domStar || p.dowStar {
			// At least one is unrestricted → AND (the * field is always true).
			dateOK = domOK && dowOK
		} else {
			// Both restricted → OR (POSIX cron semantics).
			dateOK = domOK || dowOK
		}
		if dateOK {
			return t, true
		}
	}
	return time.Time{}, false
}
