#!/bin/sh
# transcribe.sh — clawson-whisper entrypoint.
#
# Usage: transcribe.sh <audio-file>
# Prints the transcript to stdout, nothing else. All diagnostics go to stderr so
# the daemon can capture stdout as the clean transcript (see attachments.go ::
# transcribeAudio, which reads cmd.Output()).
#
# whisper-cli needs 16 kHz mono signed-16 WAV, so convert with ffmpeg first.
# ffmpeg sniffs the container format from content, so the input extension is
# advisory only. -nostdin so ffmpeg never tries to read our (empty) stdin.
set -eu

in="${1:?usage: transcribe.sh <audio-file>}"
[ -f "$in" ] || { echo "transcribe: no such file: $in" >&2; exit 2; }

tmp="$(mktemp -d /tmp/whisper.XXXXXX)"
trap 'rm -rf "$tmp"' EXIT
wav="$tmp/audio.wav"

ffmpeg -nostdin -hide_banner -loglevel error -y \
  -i "$in" -ar 16000 -ac 1 -c:a pcm_s16le -f wav "$wav" >&2

# -nt: strip per-segment timestamps. -otxt -of writes the transcript to
# "$tmp/out.txt" deterministically (relying on whisper-cli's stdout framing is
# brittle across builds — it interleaves progress there). We then cat the file
# as the clean stdout the daemon captures.
whisper-cli \
  -m /models/ggml-base.en.bin \
  -f "$wav" \
  -nt \
  -otxt -of "$tmp/out" >&2

cat "$tmp/out.txt"
