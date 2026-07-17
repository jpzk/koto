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
	return io.ReadAll(f)
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

func contextBlock(m map[string]any) string {
	rl, _ := m["ratelimit"].(map[string]any)
	if rl == nil {
		rl = map[string]any{}
	}
	u, _ := m["usage"].(map[string]any)
	if u == nil {
		u = map[string]any{}
	}
	now := time.Now()
	util := func(k string) string {
		if f, ok := anyAsFloat(rl[k]); ok {
			return fmt.Sprintf("%.0f%%", f*100)
		}
		return "?"
	}
	resets := func(k string) string {
		if n, ok := anyAsInt(rl[k]); ok {
			return fmtDur(n - now.Unix())
		}
		return "?"
	}
	overage := "?"
	if s, ok := rl["anthropic-ratelimit-unified-overage-status"].(string); ok {
		overage = s
	}
	uget := func(k string) int64 {
		n, _ := anyAsInt(u[k])
		return n
	}
	dur := "?"
	if n, ok := anyAsInt(m["dur_ms"]); ok {
		dur = strconv.FormatInt(n, 10)
	}
	return fmt.Sprintf(
		"<koto-context>\n"+
			"now: %s (%s)\n"+
			"rate-limit: 5h=%s (resets %s) | 7d=%s (resets %s)\n"+
			"overage: %s\n"+
			"last-call: in=%d cache_rd=%d cache_cr=%d out=%d dur=%sms\n"+
			"</koto-context>",
		now.Format("2006-01-02T15:04:05-07:00"),
		now.Format("Monday"),
		util("anthropic-ratelimit-unified-5h-utilization"),
		resets("anthropic-ratelimit-unified-5h-reset"),
		util("anthropic-ratelimit-unified-7d-utilization"),
		resets("anthropic-ratelimit-unified-7d-reset"),
		overage,
		uget("input_tokens"), uget("cache_read_input_tokens"),
		uget("cache_creation_input_tokens"), uget("output_tokens"), dur,
	)
}
