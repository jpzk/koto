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
	"fmt"
	"os"
	"path/filepath"
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
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// saveImage writes the image bytes into the group's uploads dir under a
// timestamped name and returns the path RELATIVE to the workspace root (so the
// reference embedded in the message resolves to /workspace/.cs/uploads/... from
// the sidecar's point of view).
func saveImage(g string, data []byte, mime string) (string, error) {
	if len(data) > maxImageBytes {
		return "", fmt.Errorf("image too large (%d bytes; max %d)", len(data), maxImageBytes)
	}
	if _, err := uploadsDir(g); err != nil {
		return "", err
	}
	name := fmt.Sprintf("img-%d%s", time.Now().UnixNano(), imageExt(mime))
	rel := filepath.Join(".cs", "uploads", name)
	abs := filepath.Join(vol(g), rel)
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return "", err
	}
	emitLogf("attach", "info", "image group=%s bytes=%d -> %s", g, len(data), rel)
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
