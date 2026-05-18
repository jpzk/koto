package main

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) parsedCron {
	t.Helper()
	p, err := parseCron(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return p
}

func TestEveryMinute(t *testing.T) {
	p := mustParse(t, "* * * * *")
	now := time.Date(2026, 5, 15, 10, 30, 45, 0, time.UTC)
	nx, ok := p.next(now)
	if !ok || nx != time.Date(2026, 5, 15, 10, 31, 0, 0, time.UTC) {
		t.Fatalf("got %v ok=%v", nx, ok)
	}
}

func TestStepped(t *testing.T) {
	p := mustParse(t, "*/15 * * * *")
	now := time.Date(2026, 5, 15, 10, 7, 0, 0, time.UTC)
	nx, _ := p.next(now)
	if nx.Minute() != 15 {
		t.Fatalf("got minute %d, want 15", nx.Minute())
	}
}

func TestRange(t *testing.T) {
	p := mustParse(t, "0 9-17 * * *")
	now := time.Date(2026, 5, 15, 8, 30, 0, 0, time.UTC)
	nx, _ := p.next(now)
	if nx.Hour() != 9 || nx.Minute() != 0 {
		t.Fatalf("got %v", nx)
	}
	// After 17:00 the same day, should roll to 9 next day.
	nx2, _ := p.next(time.Date(2026, 5, 15, 17, 30, 0, 0, time.UTC))
	if nx2.Day() != 16 || nx2.Hour() != 9 {
		t.Fatalf("got %v", nx2)
	}
}

func TestList(t *testing.T) {
	p := mustParse(t, "0,15,30,45 * * * *")
	if !p.min[0] || !p.min[15] || !p.min[30] || !p.min[45] || p.min[10] {
		t.Fatalf("list expansion wrong")
	}
}

func TestDayOfWeek(t *testing.T) {
	// Mondays at 9am. 2026-05-15 is a Friday.
	p := mustParse(t, "0 9 * * 1")
	nx, _ := p.next(time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC))
	if nx.Weekday() != time.Monday || nx.Hour() != 9 {
		t.Fatalf("got %v (weekday %s)", nx, nx.Weekday())
	}
}

func TestDOMDOWOrSemantics(t *testing.T) {
	// "1st of month OR any Monday" — POSIX OR.
	p := mustParse(t, "0 9 1 * 1")
	// 2026-05-15 is Friday. Next Monday is 2026-05-18. May 1 already past.
	nx, _ := p.next(time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC))
	if nx.Weekday() != time.Monday || nx.Day() != 18 {
		t.Fatalf("got %v", nx)
	}
	// From late May, next "1st OR Monday" is June 1 (Monday).
	nx2, _ := p.next(time.Date(2026, 5, 31, 23, 59, 0, 0, time.UTC))
	if nx2.Month() != 6 || nx2.Day() != 1 {
		t.Fatalf("got %v", nx2)
	}
}

func TestAliasDaily(t *testing.T) {
	p := mustParse(t, "@daily")
	nx, _ := p.next(time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC))
	if nx != time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("got %v", nx)
	}
}

func TestAliasHourly(t *testing.T) {
	p := mustParse(t, "@hourly")
	nx, _ := p.next(time.Date(2026, 5, 15, 10, 30, 0, 0, time.UTC))
	if nx != time.Date(2026, 5, 15, 11, 0, 0, 0, time.UTC) {
		t.Fatalf("got %v", nx)
	}
}

func TestInvalidExpressions(t *testing.T) {
	for _, bad := range []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * 32 * *",
		"* * * 13 *",
		"* * * * 7",
		"abc * * * *",
		"*/0 * * * *",
		"5-3 * * * *",
	} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestUnsatisfiable(t *testing.T) {
	// February 30th never exists.
	if _, err := nextFire("0 0 30 2 *", time.Now()); err == nil {
		t.Fatalf("expected unsatisfiable error")
	}
}
