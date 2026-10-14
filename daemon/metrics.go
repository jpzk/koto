package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---- metrics tail --------------------------------------------------------

func readTail(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := int64(0)
	if size > int64(max) {
		off = size - int64(max)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	// LimitReader, not a bare ReadAll (audit 2026-09-11 L90). max is the
	// caller's memory budget, and the stat that sized the seek is not the read:
	// metrics.jsonl gains a line per proxied request and a group log a line per
	// output line, so an append between the two made ReadAll return the tail
	// PLUS everything written since — unbounded, then split, parsed and in some
	// callers serialised into a response. The bound has to be on the read.
	return io.ReadAll(io.LimitReader(f, int64(max)))
}

func latestMetric(g string) map[string]any {
	if _, err := os.Stat(METRICS); err != nil {
		return nil
	}
	tail, err := readTail(METRICS, 65536)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			if gv, _ := m["group"].(string); gv == g {
				return m
			}
		}
	}
	return nil
}

func latestMetricAny() map[string]any {
	if _, err := os.Stat(METRICS); err != nil {
		return nil
	}
	tail, err := readTail(METRICS, 65536)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil {
			return m
		}
	}
	return nil
}

func fmtDur(s int64) string {
	if s < 0 {
		return "now"
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm", s/60)
	}
	if s < 86400 {
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, (s%86400)/3600)
}

func anyAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func anyAsInt(v any) (int64, bool) {
	if f, ok := anyAsFloat(v); ok {
		return int64(f), true
	}
	return 0, false
}

// contextBlock renders the <koto-context> header for a turn of a group
// whose budget key is key. m is the group's last metrics record (nil when it
// never made a call): it feeds only the last-call line (and the window
// fallback right after a daemon restart, see budgetCtxAccount).
func contextBlock(key budgetKey, m map[string]any) string {
	u, _ := m["usage"].(map[string]any)
	if u == nil {
		u = map[string]any{}
	}
	now := time.Now()
	acct, _ := budgetCtxAccount(key, m)
	uget := func(k string) int64 {
		n, _ := anyAsInt(u[k])
		return n
	}
	dur := "?"
	if n, ok := anyAsInt(m["dur_ms"]); ok {
		dur = strconv.FormatInt(n, 10)
	}
	var b strings.Builder
	b.WriteString("<koto-context>\n")
	fmt.Fprintf(&b, "now: %s (%s)\n", now.Format("2006-01-02T15:04:05-07:00"), now.Format("Monday"))
	b.WriteString(budgetCtxRateLine(key, acct, now) + "\n")
	if l := budgetCtxExtraLine(key, acct); l != "" {
		b.WriteString(l + "\n")
	}
	if l := budgetCtxBudgetLine(key); l != "" {
		b.WriteString(l + "\n")
	}
	if m != nil {
		fmt.Fprintf(&b, "last-call: in=%d cache_rd=%d cache_cr=%d out=%d dur=%sms\n",
			uget("input_tokens"), uget("cache_read_input_tokens"),
			uget("cache_creation_input_tokens"), uget("output_tokens"), dur)
	}
	b.WriteString("</koto-context>")
	return b.String()
}
