package main

// attachments.go — daemon-side ingestion of image + audio attachments that
// arrive on a SendReq (gRPC). The design goal is that the per-group send queue
// (queue.go) always carries a plain TEXT turn: attachments are materialized to
// the group's workspace and folded into the message string BEFORE enqueueSend,
// so nothing downstream (queue worker, sendNow, FIFO protocol, sidecar
// entrypoint) needs to know an attachment ever existed.
//
//   - Images are saved verbatim under <vol>/.cs/uploads/ and referenced in the
//     message as "[image: <rel>]". The sidecar agent (which has a Read tool and
//     can see its own /workspace) loads the file itself when relevant — the
//     daemon does not push image bytes into the model. Keeps the trust boundary
//     unchanged: bytes land in the group's own writable workspace, nowhere else.
//   - Audio (voice notes) is currently NOT supported: local transcription used a
//     koto-whisper container over the DooD podman socket, and that socket was
//     removed (it was cs_host's last path to host authority — see run-host.sh).
//     The audio wire fields are retained; a request carrying audio is rejected
//     with a clear error rather than silently dropped.
//
// Size caps are enforced before anything touches disk so a malicious/huge
// attachment can't fill the workspace.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"koto-protocol/pb"
)

// Attachment size cap for images. Generous enough for phone photos, tight
// enough that a single SendReq can't dump arbitrary amounts into a workspace.
// (The gRPC server also has its own max-recv-message size; this is a second,
// attachment-specific gate so the error message is meaningful.)
const maxImageBytes = 15 << 20 // 15 MiB

// imageExt maps a MIME type to the file extension we save under. Default .bin
// for anything unrecognized — we still save it (the agent may know what to do
// with it) rather than rejecting, but we don't pretend to know its type.
func imageExt(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}

// uploadsDir returns (and creates) the per-group attachment directory. It lives
// under .cs/ alongside the FIFO + log so it shares the group's workspace mount
// and never leaks into another group.
func uploadsDir(g string) (string, error) {
	d := filepath.Join(vol(g), ".cs", "uploads")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// saveImage writes the image bytes into the group's uploads dir under a
// timestamped name and returns the path RELATIVE to the workspace root (so the
// reference embedded in the message resolves to /workspace/.cs/uploads/... from
// the sidecar's point of view).
// maxUploadsPending caps the bytes waiting in a group's host-side uploads/
// between Sends (audit M10).
const maxUploadsPending = 64 << 20

// uploadsOrphanMaxAge is how long a staged-but-undelivered upload may sit.
const uploadsOrphanMaxAge = 24 * time.Hour

// sweepStaleUploads removes regular files under dir older than age.
func sweepStaleUploads(dir string, age time.Duration) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cut := time.Now().Add(-age)
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || !fi.ModTime().Before(cut) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// dirBytes sums the regular files directly under dir (0 when absent).
func dirBytes(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}

// uploadsMu serializes the quota check and the write that follows it.
//
// dirBytes-then-WriteFile is a check and a use with nothing between them, and
// gRPC serves unary RPCs concurrently — so N image-bearing Sends could all
// read the same under-quota total and all write, putting the spool
// arbitrarily far past maxUploadsPending (audit M55). One lock is the whole
// fix: the write is a few tens of KB and the contention is per group.
var uploadsMu sync.Mutex

// randHex returns n bytes of randomness as hex. crypto/rand, because the
// value's job is to be unguessable to a concurrent writer as well as unique.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on Linux; if it somehow did, a timestamp
		// alone is what this function exists to stop relying on, so fall back
		// to something that still differs per call.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func saveImage(g string, data []byte, mime string) (string, error) {
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("image too large (%d bytes; max %d)", len(data), maxImageBytes)
	}
	dir, err := uploadsDir(g)
	if err != nil {
		return "", err
	}
	uploadsMu.Lock()
	defer uploadsMu.Unlock()
	// Pending (not yet delivered) uploads are bounded per group: every Send
	// with an image costs host disk before the queue applies any
	// backpressure, and host disk exhaustion is the fleet-wide failure
	// (audit M10). Delivered uploads are removed by fcSendMsg.
	// Sweep orphans first. An upload is staged before the queue accepts the
	// send, and delivery is now per-turn and exact (fcTurnUploads), so a file
	// whose turn never ran — a daemon restart between staging and delivery —
	// has no later turn that will claim it. Without this it would hold the
	// group's quota until someone noticed (audit M36). A day is far longer
	// than any legitimate staging-to-delivery gap.
	sweepStaleUploads(dir, uploadsOrphanMaxAge)
	if used := dirBytes(dir); used+int64(len(data)) > maxUploadsPending {
		return "", fmt.Errorf("too many pending uploads for %s (%d bytes waiting; max %d) — send a turn first", g, used, maxUploadsPending)
	}
	// The name carries randomness AND the write is exclusive (audit
	// 2026-09-11 L112). A timestamp alone is not an identity: UnixNano repeats
	// on a coarse clock, and the quota check above is not a uniqueness check —
	// two handlers that land on the same value both resolve to the same
	// relative path, and the truncating write silently replaces the FIRST
	// message's attachment with the second's. The image is what the operator
	// is asking the agent about, so delivering the wrong one is worse than
	// failing.
	//
	// O_EXCL is the part that makes it an invariant rather than a
	// probability, and O_NOFOLLOW because this directory is the group's own.
	// The retry is for the collision O_EXCL reports, which with 64 bits of
	// randomness should never be seen.
	ext := imageExt(mime)
	var rel string
	var f *os.File
	for attempt := 0; ; attempt++ {
		name := fmt.Sprintf("img-%d-%s%s", time.Now().UnixNano(), randHex(8), ext)
		rel = filepath.Join(".cs", "uploads", name)
		var oerr error
		f, oerr = os.OpenFile(filepath.Join(vol(g), rel),
			os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if oerr == nil {
			break
		}
		if !errors.Is(oerr, os.ErrExist) || attempt >= 4 {
			return "", oerr
		}
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(filepath.Join(vol(g), rel))
		return "", werr
	}
	if cerr != nil {
		_ = os.Remove(filepath.Join(vol(g), rel))
		return "", cerr
	}
	emitLogfG("attach", g, "info", "image group=%s bytes=%d -> %s", g, len(data), rel)
	return rel, nil
}

// processAttachments folds any image on the request into the text message the
// queue will carry (image reference appended inline) and returns the final
// string. Audio is rejected: transcription depended on the now-removed DooD
// podman socket, so a request carrying audio fails in-band rather than being
// silently dropped.
func processAttachments(r *pb.SendReq) (string, error) {
	msg := r.GetMsg()
	g := r.GetGroup()

	if len(r.GetAudio()) > 0 {
		return "", fmt.Errorf("audio transcription is disabled (whisper/DooD removed); send text instead")
	}

	if len(r.GetImage()) > 0 {
		rel, err := saveImage(g, r.GetImage(), r.GetImageMime())
		if err != nil {
			return "", err
		}
		msg = msg + "\n[image: " + rel + "]"
	}

	return msg, nil
}
