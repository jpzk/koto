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
//   - Audio is transcribed locally by a dedicated clawson-whisper container run
//     over the same DooD podman socket the daemon already uses for sidecars
//     (see ensure()), and the transcript text is merged into the message. The
//     audio bytes never leave the host: whisper.cpp runs offline, no API call.
//
// Size caps are enforced before anything touches disk so a malicious/huge
// attachment can't fill the workspace or wedge whisper.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"clawson-protocol/pb"
)

// whisper emits an all-caps bracketed marker on a line of its own when a segment
// has no speech — most commonly "[BLANK_AUDIO]", also "[SILENCE]", "[MUSIC]",
// "[NOISE]". These are NOT a transcript; without stripping them the literal
// token gets forwarded to the model as the user's message. The all-caps shape
// deliberately spares lowercase event tags like "[laughs]" that can be real
// transcript content.
var whisperMarkerRe = regexp.MustCompile(`^\[[A-Z_ ]+\]$`)

// stripWhisperMarkers drops whole lines that are just a whisper non-speech
// marker, returning the trimmed remainder.
func stripWhisperMarkers(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if whisperMarkerRe.MatchString(strings.TrimSpace(ln)) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// Attachment size caps. Generous enough for phone photos / voice notes, tight
// enough that a single SendReq can't dump arbitrary amounts into a workspace.
// (The gRPC server also has its own max-recv-message size; these are a second,
// attachment-specific gate so the error message is meaningful.)
const (
	maxImageBytes = 15 << 20 // 15 MiB
	maxAudioBytes = 25 << 20 // 25 MiB
)

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

// audioExt maps an audio MIME type to the extension whisper's ffmpeg step reads
// from. ffmpeg sniffs container content, not the extension, so .bin would also
// work — but a correct extension keeps the uploads dir self-documenting and
// matches what a human inspecting the workspace expects.
func audioExt(mime string) string {
	switch mime {
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
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
	emitLogf("info", "attachment image group=%s bytes=%d -> %s", g, len(data), rel)
	return rel, nil
}

// transcribeAudio writes the audio bytes into the uploads dir, then runs the
// clawson-whisper container over DooD podman to transcribe it, returning the
// trimmed transcript. We mount ONLY the group's uploads dir into the whisper
// container (:Z relabel so SELinux permits the read; consistent with the rest
// of the stack using label=disable, but :Z is the narrower choice for a
// single-purpose throwaway mount). The container has no network and is --rm.
//
// whisper-cli needs 16kHz mono WAV; the conversion + invocation are wrapped in
// the image's /transcribe.sh entrypoint (see host/whisper/Dockerfile), so the
// daemon just passes the audio filename and reads stdout.
func transcribeAudio(g string, data []byte, mime string) (string, error) {
	if len(data) > maxAudioBytes {
		return "", fmt.Errorf("audio too large (%d bytes; max %d)", len(data), maxAudioBytes)
	}
	dir, err := uploadsDir(g)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("voice-%d%s", time.Now().UnixNano(), audioExt(mime))
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return "", err
	}
	emitLogf("info", "attachment audio group=%s bytes=%d -> %s (transcribing)", g, len(data), name)

	// Run the transcription with a bounded deadline: a wedged whisper process
	// must not block the gRPC Send handler (and thus the caller) forever. base.en
	// transcribes a minute of audio in a few seconds on CPU; 4 min is a wide
	// margin for a 25 MiB clip.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// Mount only the uploads dir, at the same path inside the whisper container,
	// so the filename we pass resolves. --network=none: whisper.cpp is offline,
	// it must never reach out. --security-opt label=disable matches the rest of
	// the codebase's podman calls (the :Z on the volume additionally relabels
	// for the read, harmless under label=disable).
	args := []string{
		"run", "--rm",
		"--network=none",
		"--security-opt", "label=disable",
		"-v", dir + ":" + dir + ":Z",
		"clawson-whisper:latest",
		abs,
	}
	cmd := exec.CommandContext(ctx, "podman", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper transcribe: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	transcript := stripWhisperMarkers(strings.TrimSpace(string(out)))
	if transcript == "" {
		return "", fmt.Errorf("no speech detected in audio")
	}
	emitLogf("info", "attachment audio group=%s transcript=%dch", g, len(transcript))
	return transcript, nil
}

// processAttachments folds any image/audio on the request into the text message
// the queue will carry. Order: image reference is appended first, then the
// audio transcript (either replacing an empty message or appended on its own
// line). Returns the final message string.
func processAttachments(r *pb.SendReq) (string, error) {
	msg := r.GetMsg()
	g := r.GetGroup()

	if len(r.GetImage()) > 0 {
		rel, err := saveImage(g, r.GetImage(), r.GetImageMime())
		if err != nil {
			return "", err
		}
		msg = msg + "\n[image: " + rel + "]"
	}

	if len(r.GetAudio()) > 0 {
		transcript, err := transcribeAudio(g, r.GetAudio(), r.GetAudioMime())
		if err != nil {
			return "", err
		}
		if msg == "" {
			msg = transcript
		} else {
			msg = msg + "\n" + transcript
		}
	}

	return msg, nil
}
