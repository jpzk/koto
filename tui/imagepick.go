package main

// imagepick.go — the new-group dialog's image select (newgroup.go).
//
// The options come from the daemon, never from this file: the Image rpc op
// "known" answers the well-known base images (daemon/ociimage/known.go, the
// one list) followed by every other catalog name, each with its pinned
// digest and created date, or "-" when it is not in the catalog yet. The TUI
// carries names only; `image add <name>` lets the daemon fill in the ref.
//
// Create with an image that is not in the catalog yet first runs
// `image add <name>` (progress streamed into the dialog), then spawns with
// image=<name>; an add failure stops there, no group is created. A pinned
// image spawns on its pinned digest; "r" on the field arms a refresh, which
// runs `image update <name>` (re-resolve the tag, pin the new digest) before
// the spawn.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"koto-protocol/pb"

	tea "github.com/charmbracelet/bubbletea"
)

const ngImage = "image"

// imageOpt is one row of the op "known" answer.
type imageOpt struct {
	name    string // catalog name spawn sends ("" = daemon default)
	label   string // what the select shows
	source  string // registry ref, "builtin", or "-"
	digest  string // pinned digest, "-" when not in the catalog
	created string // RFC 3339, or "-"
}

func (o imageOpt) builtin() bool { return o.source == "builtin" || o.name == "" }
func (o imageOpt) pinned() bool  { return !o.builtin() && o.digest != "-" && o.digest != "" }

// updatable: `image update <name>` (no ref) re-resolves the catalog's
// recorded source, so a pinned entry without one (a v1 catalog; the daemon
// answers source "-") is not offered "r".
func (o imageOpt) updatable() bool { return o.pinned() && o.source != "-" && o.source != "" }

const (
	imagesLoading = iota
	imagesLoaded
	imagesFailed
)

// imagePick is the select's state.
type imagePick struct {
	state   int
	reason  string // why the list failed (imagesFailed)
	opts    []imageOpt
	sel     int
	refresh bool   // "r": update a pinned image to latest before spawning
	want    string // keep this name selected across a reload
	// gen identifies the outstanding op "known" request: an answer for an
	// older request (a closed dialog, a superseded reload) is dropped.
	gen uint64
	// done is the name whose add/update already succeeded in this dialog
	// (a later step failed): a retry spawns without repeating it, even
	// before the catalog reload lands.
	done string
	// lost is the name that was selected before a reload that no longer
	// lists it: Create refuses until the operator picks again, so it never
	// silently spawns on the default instead.
	lost string
	// stale is why the last reload failed while earlier options are kept.
	stale string
}

// ngGen numbers new-group requests (dialog image lists and submits) so an
// answer can be matched to the request that is still current.
var ngGen atomic.Uint64

func nextNgGen() uint64 { return ngGen.Add(1) }

// imageFallback is the list when op "known" fails (an older daemon, or no
// admin rights): the daemon default only, sent as no image at all.
var imageFallback = []imageOpt{{name: "", label: "default", source: "builtin", digest: "-", created: "-"}}

func (p *imagePick) cur() imageOpt {
	if len(p.opts) == 0 {
		return imageFallback[0]
	}
	return p.opts[((p.sel%len(p.opts))+len(p.opts))%len(p.opts)]
}

func (p *imagePick) cycle(d int) {
	if n := len(p.opts); n > 0 {
		p.sel = ((p.sel+d)%n + n) % n
	}
	p.refresh = false // a refresh is armed for one image, not "the field"
	p.lost = ""       // the operator picked again
}

// parseImageKnown parses the op "known" TSV. Malformed rows are skipped; a
// list with no usable row is an error (the caller falls back).
func parseImageKnown(data string) ([]imageOpt, error) {
	var out []imageOpt
	for _, ln := range strings.Split(data, "\n") {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		f := strings.Split(ln, "\t")
		if len(f) != 5 || f[0] == "" {
			continue
		}
		// Daemon text reaches the dialog: scrub it like every rpc string.
		for i := range f {
			f[i] = scrubVTStrict(f[i])
		}
		out = append(out, imageOpt{name: f[0], label: f[1], source: f[2], digest: f[3], created: f[4]})
	}
	if len(out) == 0 {
		return nil, errors.New("no images listed")
	}
	return out, nil
}

// imageLoadedMsg carries an op "known" answer.
type imageLoadedMsg struct {
	gen  uint64 // the request this answers (imagePick.gen)
	opts []imageOpt
	err  error
}

func (p *imagePick) loaded(msg imageLoadedMsg) {
	want := p.want
	if want == "" && p.state == imagesLoaded {
		want = p.cur().name
	}
	p.want = ""
	if msg.err != nil && p.state == imagesLoaded {
		// A reload failed after a good load: keep the options and the
		// selection the operator is looking at (falling back to default
		// here would make a retried Create spawn on an image not chosen).
		p.stale = scrubVTStrict(msg.err.Error())
		return
	}
	if msg.err != nil {
		p.state, p.reason = imagesFailed, scrubVTStrict(msg.err.Error())
		p.opts = append([]imageOpt(nil), imageFallback...)
	} else {
		p.state, p.reason, p.stale = imagesLoaded, "", ""
		p.opts = msg.opts
	}
	p.sel, p.lost = 0, ""
	found := false
	for i, o := range p.opts {
		if o.name == want {
			p.sel, found = i, true
		}
	}
	if want != "" && !found && msg.err == nil {
		p.lost = want
	}
	if !p.cur().updatable() {
		p.refresh = false
	}
}

// ageShort renders a created date as a compact age ("3d old"); "" when the
// date is missing or unparseable.
func ageShort(created string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return "<1h old"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh old", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd old", int(d.Hours()/24))
	}
}

// shortDigest is "sha256:" plus 12 hex chars.
func shortDigest(d string) string {
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) > i+13 {
		return d[:i+13]
	}
	return d
}

// hint is the line under the select: what Create will do with the image.
func (p *imagePick) hint(focused bool, now time.Time) string {
	if p.state == imagesLoading {
		return gl("loading images…", "loading images...")
	}
	o := p.cur()
	var h string
	switch {
	case p.lost != "":
		h = p.lost + " is no longer listed: pick an image"
	case p.state == imagesFailed:
		h = "image list unavailable: default only"
	case o.builtin():
		h = "built-in rootfs"
	case o.name == p.done:
		h = "in the catalog now (pulled for this group)"
	case o.pinned():
		h = "pinned " + shortDigest(o.digest)
		if a := ageShort(o.created, now); a != "" {
			h += gl(" · ", " . ") + a
		}
		if p.refresh {
			h += gl(" · will update to latest", " . will update to latest")
		} else if focused && o.updatable() {
			h += gl(" · r: update to latest", " . r: update to latest")
		}
	default:
		h = "will pull latest"
		if o.source != "" && o.source != "-" {
			h += " (" + o.source + ")"
		}
	}
	if p.stale != "" {
		h += " (list refresh failed)"
	}
	return h
}

// step is the catalog op Create runs before the spawn: "add" for a name not
// in the catalog yet, "update" for an armed refresh, "" for none.
func (p *imagePick) step() string {
	o := p.cur()
	switch {
	case o.builtin(), o.name == p.done:
		// done: this dialog's add/update of it already succeeded and a
		// later step failed; a retry only spawns.
		return ""
	case !o.pinned():
		return "add"
	case p.refresh && o.updatable():
		return "update"
	}
	return ""
}

// imageStream runs one Image rpc, handing every complete output line to
// line and every structured "progress" event to prog (either nil =
// discard). An "error" event or a transport error is the returned error.
func imageStream(ctx context.Context, op string, args []string, line func(string), prog func(*pb.ImageProgress)) error {
	cl, err := getClient()
	if err != nil {
		return err
	}
	stream, err := cl.Image(ctx, &pb.ImageReq{Op: op, Args: args})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch ev.Event {
		case "data":
			if line != nil {
				for _, l := range strings.Split(strings.TrimRight(ev.Data, "\n"), "\n") {
					line(l)
				}
			}
		case "progress":
			if prog != nil && ev.GetProgress() != nil {
				prog(ev.GetProgress())
			}
		case "end":
			return nil
		case "error":
			return errors.New(ev.Error)
		}
	}
}

// imageKnownCmd fetches the select's options for request gen.
func imageKnownCmd(gen uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var b strings.Builder
		err := imageStream(ctx, "known", nil, func(l string) { b.WriteString(l + "\n") }, nil)
		if err != nil {
			logWarn("rpc", "image known: %v", err)
			return imageLoadedMsg{gen: gen, err: err}
		}
		opts, err := parseImageKnown(b.String())
		return imageLoadedMsg{gen: gen, opts: opts, err: err}
	}
}

// imagePullTimeout bounds an add/update: a registry pull of a distro base
// image, unpacked and converted on the host.
const imagePullTimeout = 20 * time.Minute

// newGroupProgressMsg is one line of add/update output, or (pull != nil)
// one structured progress update received at at, shown in the dialog while
// submit seq runs (a message from another submit is dropped).
type newGroupProgressMsg struct {
	seq  uint64
	line string
	pull *pb.ImageProgress
	at   time.Time
}

// progressSink delivers a progress message to the running program (nil
// program in tests: dropped unless a test swaps the sink).
var progressSink = func(msg tea.Msg) {
	if prog != nil {
		prog.Send(msg)
	}
}

// sendProgress is the line callback for submit seq's image add/update.
func sendProgress(seq uint64) func(string) {
	return func(line string) { progressSink(newGroupProgressMsg{seq: seq, line: line}) }
}
