# clawson-whisper — offline speech-to-text for voice-note attachments.
#
# The daemon runs this image over its DooD podman socket (see attachments.go ::
# transcribeAudio): `podman run --rm --network=none -v <uploads>:<uploads>:Z
# clawson-whisper:latest <audiofile>`. It prints the transcript to stdout.
#
# Two-stage build:
#   - builder compiles whisper.cpp's whisper-cli at a PINNED release tag.
#   - runtime is a slim Debian with ffmpeg (for the 16kHz-mono-WAV conversion
#     whisper-cli requires) + the static-ish whisper-cli binary + its shared
#     libs + the pinned ggml model, verified by sha256.
#
# SUPPLY-CHAIN PINS (6-week dependency-lag rule; verified 2026-06-15):
#   - base image:   debian:bookworm-20260421-slim  (Debian 12 point release,
#                   dated snapshot tag => reproducible apt set; >6 weeks old)
#   - whisper.cpp:  v1.8.4  (ggml-org/whisper.cpp, commit
#                   9386f239401074690479731c1e41683fbbeac557, 2026-03-19;
#                   MIT license; latest stable tag >=6 weeks old — v1.8.5/.6
#                   are 2026-05-29/06-01, inside the window, so deliberately
#                   NOT used)
#   - ggml model:   ggml-base.en.bin, pinned by repo COMMIT (not :main) +
#                   sha256, from huggingface.co/ggerganov/whisper.cpp
#                   (Georgi Gerganov, MIT). 147964211 bytes.
#                   sha256 a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002
#   - apk/apt pkgs: build-essential, cmake, git, ca-certificates, ffmpeg,
#                   curl — all from the dated Debian snapshot above.

# ---- builder ---------------------------------------------------------------
FROM debian:bookworm-20260421-slim AS builder

# Pinned whisper.cpp release. The git checkout is by the immutable commit the
# tag points at, so a re-tag upstream can't change what we build.
ARG WHISPER_REF=9386f239401074690479731c1e41683fbbeac557

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git init -q . \
 && git remote add origin https://github.com/ggml-org/whisper.cpp \
 && git fetch -q --depth 1 origin "${WHISPER_REF}" \
 && git checkout -q FETCH_HEAD

# Build only whisper-cli (the CLI front-end) + its libs. CPU-only; no CUDA/BLAS
# to keep the image small and dependency-free. cmake install lays the binary +
# shared objects under /opt/whisper.
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release -DWHISPER_BUILD_EXAMPLES=ON \
 && cmake --build build -j"$(nproc)" --target whisper-cli \
 && mkdir -p /opt/whisper/bin /opt/whisper/lib \
 && cp build/bin/whisper-cli /opt/whisper/bin/ \
 && find build -name '*.so*' -exec cp -a {} /opt/whisper/lib/ \;

# ---- model fetch (separate stage so a model re-download doesn't rebuild C++) -
FROM debian:bookworm-20260421-slim AS model
ARG MODEL_SHA256=a03779c86df3323075f5e796cb2ce5029f00ec8869eee3fdfb897afe36c6d002
# Pinned by repo commit (immutable) rather than resolve/main (mutable).
ARG MODEL_URL=https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base.en.bin
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL "${MODEL_URL}" -o /ggml-base.en.bin \
 && echo "${MODEL_SHA256}  /ggml-base.en.bin" | sha256sum -c -

# ---- runtime ---------------------------------------------------------------
FROM debian:bookworm-20260421-slim AS runtime

# ffmpeg only — whisper-cli + its shared libs come from the builder stage.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /opt/whisper/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=builder /opt/whisper/lib/ /usr/local/lib/
COPY --from=model   /ggml-base.en.bin /models/ggml-base.en.bin
RUN ldconfig

COPY transcribe.sh /usr/local/bin/transcribe.sh
RUN chmod +x /usr/local/bin/transcribe.sh

# Run as a non-root user: the container only ever reads one mounted audio file
# and writes a temp WAV; it needs no privileges. Matches the project's
# "non-root inside the container" posture for the sidecars.
RUN useradd -u 1000 -m whisper
USER whisper

ENTRYPOINT ["/usr/local/bin/transcribe.sh"]
