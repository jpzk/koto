package ociimage

import (
	"io"
	"time"
)

// Structured pull progress. The text log (Client.Logf) says what finished;
// Progress says where an image add is right now, as numbers a client can
// draw a bar from without parsing prose.

// Progress phases, in the order an add goes through them.
const (
	PhaseResolve  = "resolve"      // tag -> digest, manifests, config
	PhaseDownload = "download"     // a layer blob is streaming (Done/Total bytes)
	PhaseVerify   = "verify"       // re-hashing an already cached blob
	PhaseCached   = "cached"       // blob was in the cache, verified
	PhaseConvert  = "convert"      // building the layer's ext4 image (Done/Total = blob bytes consumed)
	PhaseLayerOK  = "layer_cached" // the layer's ext4 image already exists (Done = Total = blob size)
	PhaseSys      = "sysimg"       // first boot: building the group's sys.img (Done/Total = 0/1, 1/1)
	PhaseDone     = "done"         // every layer is ready
)

// Progress is one structured update. Layer is 1-based (0 = not layer
// specific); Done/Total are bytes for download/verify/cached/convert/layer_cached
// (blob bytes), steps for sysimg, 0 otherwise.
type Progress struct {
	Phase  string
	Layer  int
	Layers int
	Done   int64
	Total  int64
}

// ProgressEvery is the minimum gap between two byte updates of one blob:
// about four a second, enough for a smooth bar and a rate without
// flooding the stream.
const ProgressEvery = 250 * time.Millisecond

// countingReader counts bytes read through it and reports the running
// total to emit at most once per every (plus once when total is reached).
// Monotonic by construction: it only ever adds n >= 0.
type countingReader struct {
	r     io.Reader
	total int64
	done  int64
	every time.Duration
	now   func() time.Time
	emit  func(done int64)
	last  time.Time
	sent  int64 // last done value reported (-1 = none)
}

func newCountingReader(r io.Reader, total int64, every time.Duration, now func() time.Time, emit func(int64)) *countingReader {
	if now == nil {
		now = time.Now
	}
	c := &countingReader{r: r, total: total, every: every, now: now, emit: emit, sent: -1}
	c.last = now()
	return c
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		t := c.now()
		final := c.total > 0 && c.done >= c.total
		if c.emit != nil && c.done != c.sent && (final || t.Sub(c.last) >= c.every) {
			c.last, c.sent = t, c.done
			c.emit(c.done)
		}
	}
	return n, err
}

func (c *Client) progress(p Progress) {
	if c.Progress != nil {
		c.Progress(p)
	}
}

// convertClock is the clock BuildLayerProgress throttles on (nil =
// time.Now); tests set a fake one so a small layer yields many updates.
var convertClock func() time.Time
