package main

// jobs.go — host-side observability for the guest's background jobs (cs-job).
//
// Job state lives inside workspace.img (/workspace/.cs/jobs/<id>/), which the
// host must never mount — so the daemon reads it through the guest agent's
// exec op. Reads are cached per group and refreshed:
//
//   - lazily while WatchState watchers are attached (stateWatchLoop kicks an
//     async refresh when a running group's mirror is older than
//     jobsRefreshTTL), so the TUI tree stays current without any client
//     polling the guest;
//   - immediately on a job_done ctl event (ctl.go), so completions appear on
//     the next state push instead of a TTL later;
//   - synchronously in the Jobs RPC, which is the "fresh ls" path.
//
// Each job is attributed to the chat session whose turn launched it: cs-job
// records $KOTO_SESSION (exported per turn by entrypoint.sh) into the job
// dir at mint time, normalized here to the wire form ("" = default).

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// jobsRefreshTTL bounds how stale the watched-state job mirror may get before
// stateWatchLoop kicks a background refresh. Each refresh is one guest exec
// (a tiny /bin/sh walk), paid only for running groups while watchers exist.
const jobsRefreshTTL = 3 * time.Second

// jobIDRE matches cs-job ids (mktemp XXXXXXXX basenames). Anything else is
// rejected before an id is ever spliced into a guest shell script.
var jobIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

type jobsCacheEntry struct {
	jobs []JobInfo
	at   time.Time
}

var (
	jobsMu         sync.Mutex
	jobsCache      = map[string]jobsCacheEntry{}
	jobsRefreshing = map[string]bool{}
)

// jobsListScript walks the job dirs and prints one TSV line per job:
// id \t status \t rc \t session \t started \t out_size \t cmd
// cmd is last because it's the only field that may contain runs of
// arbitrary text; its tabs/newlines are squashed to spaces first.
const jobsListScript = `for d in /workspace/.cs/jobs/*/; do
  [ -d "$d" ] || continue
  id=$(basename "$d")
  st=$(cat "$d/status" 2>/dev/null || echo unknown)
  rc=$(cat "$d/rc" 2>/dev/null || echo '')
  sess=$(cat "$d/session" 2>/dev/null || echo '')
  start=$(cat "$d/started" 2>/dev/null || echo 0)
  sz=$(wc -c < "$d/out" 2>/dev/null || echo 0)
  cmd=$(head -c 200 "$d/cmd" 2>/dev/null | tr '\t\n' '  ')
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$st" "$rc" "$sess" "$start" "$sz" "$cmd"
done
true`

// fcReadJobs reads the group's job dirs fresh from the guest. Only call for
// a running group — ensure() is deliberately NOT called here (observability
// must never boot a VM).
func fcReadJobs(g string) ([]JobInfo, error) {
	out, _, err := fcExec(g, jobsListScript, 15*time.Second)
	if err != nil {
		return nil, err
	}
	return parseJobsTSV(out), nil
}

// parseJobsTSV decodes jobsListScript's output. Malformed lines (and ids
// outside jobIDRE) are dropped rather than erroring — the job dir is
// agent-writable, so junk in it must degrade to "not listed", never break
// the whole mirror.
func parseJobsTSV(out string) []JobInfo {
	jobs := []JobInfo{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 7)
		if len(f) < 7 || !jobIDRE.MatchString(f[0]) {
			continue
		}
		sess, err := normalizeSession(f[3])
		if err != nil {
			sess = "" // unattributed (pre-session job dirs) → default
		}
		started, _ := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
		size, _ := strconv.ParseInt(strings.TrimSpace(f[5]), 10, 64)
		// status/rc/cmd are agent-writable files rendered verbatim in the
		// TUI tree; scrub escapes/bidi here, not per client.
		jobs = append(jobs, JobInfo{
			ID: f[0], Status: sanitize(f[1]), RC: sanitize(f[2]), Session: sess,
			Started: started, OutSize: size, Cmd: sanitize(strings.TrimSpace(f[6])),
		})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Started != jobs[j].Started {
			return jobs[i].Started < jobs[j].Started
		}
		return jobs[i].ID < jobs[j].ID
	})
	return jobs
}

// jobsSnapshot returns the cached mirror for g (possibly empty/stale) without
// touching the guest — the cheap read listGroups uses on every state tick.
func jobsSnapshot(g string) []JobInfo {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return jobsCache[g].jobs
}

// refreshJobs re-reads g's jobs from the guest and stores the mirror.
// Returns the fresh list. Errors keep the previous cache (a group mid-restart
// shouldn't blank the tree) and are logged at debug — a stopped group simply
// has no reachable agent.
func refreshJobs(g string) []JobInfo {
	if !fcRunning(g) {
		jobsMu.Lock()
		delete(jobsCache, g)
		jobsMu.Unlock()
		return nil
	}
	// Share the in-flight guard with the background refresher (audit M80).
	// The Jobs RPC called straight through, so concurrent callers each issued
	// their own guest exec for the same group — N shells in the guest, N host
	// connections, N daemon goroutines, all producing the same answer. A
	// refresh already running will publish that answer momentarily; serving
	// the current snapshot is both cheaper and no staler than waiting for a
	// duplicate of it.
	jobsMu.Lock()
	if jobsRefreshing[g] {
		jobsMu.Unlock()
		return jobsSnapshot(g)
	}
	jobsRefreshing[g] = true
	jobsMu.Unlock()
	defer func() {
		jobsMu.Lock()
		delete(jobsRefreshing, g)
		jobsMu.Unlock()
	}()
	return refreshJobsNow(g)
}

// refreshJobsNow is the guest exec itself. Callers must already hold the
// per-group in-flight marker — refreshJobs takes it, and kickJobsRefresh sets
// it before spawning its goroutine.
func refreshJobsNow(g string) []JobInfo {
	jobs, err := fcReadJobs(g)
	if err != nil {
		emitLogfG("jobs", g, "debug", "[%s] refresh: %v", g, err)
		return jobsSnapshot(g)
	}
	jobsMu.Lock()
	jobsCache[g] = jobsCacheEntry{jobs: jobs, at: time.Now()}
	jobsMu.Unlock()
	return jobs
}

// kickJobsRefresh starts one background refresh for g unless the mirror is
// fresh enough or a refresh is already in flight. force skips the TTL check
// (job_done trigger). Called from stateWatchLoop's tick and the ctl plane.
func kickJobsRefresh(g string, force bool) {
	jobsMu.Lock()
	if jobsRefreshing[g] || (!force && time.Since(jobsCache[g].at) < jobsRefreshTTL) {
		jobsMu.Unlock()
		return
	}
	jobsRefreshing[g] = true
	jobsMu.Unlock()
	go func() {
		defer func() {
			jobsMu.Lock()
			delete(jobsRefreshing, g)
			jobsMu.Unlock()
		}()
		refreshJobsNow(g)
	}()
}

// decodeB64Lines decodes base64 that may arrive wrapped across lines (the
// guest's busybox base64 wraps at 76 cols).
func decodeB64Lines(s string) (string, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, s)
	b, err := base64.StdEncoding.DecodeString(clean)
	return string(b), err
}

// jobLogsResult is fcJobLogs' parsed guest answer.
type jobLogsResult struct {
	Job       JobInfo
	Output    string
	Truncated bool
}

const (
	jobLogsDefaultTail = int64(4096)
	jobLogsMaxTail     = int64(65536)
)

// fcJobLogs reads one job's metadata plus a base64 output tail from the
// guest. The tail is base64'd guest-side so arbitrary job output survives
// the exec op's line-oriented plumbing untouched.
func fcJobLogs(g, id string, tail int64) (*jobLogsResult, error) {
	if !jobIDRE.MatchString(id) {
		return nil, fmt.Errorf("invalid job id")
	}
	if tail <= 0 {
		tail = jobLogsDefaultTail
	}
	if tail > jobLogsMaxTail {
		tail = jobLogsMaxTail
	}
	script := `D=/workspace/.cs/jobs/` + id + `
[ -d "$D" ] || { echo NOJOB; exit 0; }
printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
  "$(cat "$D/status" 2>/dev/null || echo unknown)" \
  "$(cat "$D/rc" 2>/dev/null || echo '')" \
  "$(cat "$D/session" 2>/dev/null || echo '')" \
  "$(cat "$D/started" 2>/dev/null || echo 0)" \
  "$(wc -c < "$D/out" 2>/dev/null || echo 0)" \
  "$(head -c 200 "$D/cmd" 2>/dev/null | tr '\t\n' '  ')"
tail -c ` + strconv.FormatInt(tail, 10) + ` "$D/out" 2>/dev/null | base64
true`
	out, _, err := fcExec(g, script, 15*time.Second)
	if err != nil {
		return nil, err
	}
	head, rest, _ := strings.Cut(out, "\n")
	if strings.TrimSpace(head) == "NOJOB" {
		return nil, fmt.Errorf("no such job %q in group %q", id, g)
	}
	f := strings.SplitN(head, "\t", 6)
	if len(f) < 6 {
		return nil, fmt.Errorf("unparseable job metadata")
	}
	sess, err := normalizeSession(f[2])
	if err != nil {
		sess = ""
	}
	started, _ := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64)
	size, _ := strconv.ParseInt(strings.TrimSpace(f[4]), 10, 64)
	body, err := decodeB64Lines(rest)
	if err != nil {
		return nil, fmt.Errorf("output decode: %v", err)
	}
	return &jobLogsResult{
		Job: JobInfo{
			ID: id, Status: sanitize(f[0]), RC: sanitize(f[1]), Session: sess,
			Started: started, OutSize: size, Cmd: sanitize(strings.TrimSpace(f[5])),
		},
		Output:    body,
		Truncated: size > int64(len(body)),
	}, nil
}
