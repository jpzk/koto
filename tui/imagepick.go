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
}

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
		out = append(out, imageOpt{name: f[0], label: f[1], source: f[2], digest: f[3], created: f[4]})
	}
	if len(out) == 0 {
		return nil, errors.New("no images listed")
	}
	return out, nil
}

// imageLoadedMsg carries an op "known" answer.
type imageLoadedMsg struct {
	opts []imageOpt
	err  error
}

func (p *imagePick) loaded(msg imageLoadedMsg) {
	want := p.want
	if want == "" && p.state == imagesLoaded {
		want = p.cur().name
	}
	if msg.err != nil {
		p.state, p.reason = imagesFailed, msg.err.Error()
		p.opts = append([]imageOpt(nil), imageFallback...)
	} else {
		p.state, p.reason = imagesLoaded, ""
		p.opts = msg.opts
	}
	p.sel = 0
	for i, o := range p.opts {
		if o.name == want {
			p.sel = i
		}
	}
	p.want = ""
	if !p.cur().pinned() {
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
	case p.state == imagesFailed:
		h = "image list unavailable: default only"
	case o.builtin():
		h = "built-in rootfs"
	case o.pinned():
		h = "pinned " + shortDigest(o.digest)
		if a := ageShort(o.created, now); a != "" {
			h += gl(" · ", " . ") + a
		}
		if p.refresh {
			h += gl(" · will update to latest", " . will update to latest")
		} else if focused {
			h += gl(" · r: update to latest", " . r: update to latest")
		}
	default:
		h = "will pull latest"
		if o.source != "" && o.source != "-" {
			h += " (" + o.source + ")"
		}
	}
	return h
}

// step is the catalog op Create runs before the spawn: "add" for a name not
// in the catalog yet, "update" for an armed refresh, "" for none.
func (p *imagePick) step() string {
	o := p.cur()
	switch {
	case o.builtin():
		return ""
	case !o.pinned():
		return "add"
	case p.refresh:
		return "update"
	}
	return ""
}

// imageStream runs one Image rpc, handing every complete output line to
// line (nil = discard). An "error" event or a transport error is the
// returned error.
func imageStream(ctx context.Context, op string, args []string, line func(string)) error {
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
		case "end":
			return nil
		case "error":
			return errors.New(ev.Error)
		}
	}
}

// imageKnownCmd fetches the select's options.
func imageKnownCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var b strings.Builder
		err := imageStream(ctx, "known", nil, func(l string) { b.WriteString(l + "\n") })
		if err != nil {
			logWarn("rpc", "image known: %v", err)
			return imageLoadedMsg{err: err}
		}
		opts, err := parseImageKnown(b.String())
		return imageLoadedMsg{opts: opts, err: err}
	}
}

// imagePullTimeout bounds an add/update: a registry pull of a distro base
// image, unpacked and converted on the host.
const imagePullTimeout = 20 * time.Minute

// newGroupProgressMsg is one line of add/update output, shown in the
// dialog while the submit runs.
type newGroupProgressMsg struct{ line string }

// sendProgress delivers a progress line to the running program (nil in
// tests: dropped).
func sendProgress(line string) {
	if prog != nil {
		prog.Send(newGroupProgressMsg{line: line})
	}
}
