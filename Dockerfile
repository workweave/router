# syntax=docker/dockerfile:1
#
# The router uses hugot + onnxruntime_go (CGO; dynamic-links against
# libonnxruntime.so) to run the cluster scorer's Jina v2 embedder
# in-process. That requires a glibc base — Alpine/musl can't load the
# library out of the box. Builder is bookworm-glibc; runtime is
# distroless/cc-debian12 so we get the C runtime without the rest of
# Debian's userland.

ARG ONNXRUNTIME_VERSION=1.25.1
ARG TOKENIZERS_VERSION=v1.27.0
# Cluster-scorer ONNX is hosted on HuggingFace Hub (pattern mirrors
# models/v2/). HF_MODEL_REPO points at the repo; HF_MODEL_REVISION
# pins to a tag/SHA so a deploy doesn't silently pick up a retrained
# model. Bump the revision when scripts/upload_to_hf.py pushes a new
# version. The HF_TOKEN build secret authenticates against private
# repos — pass with `docker build --secret id=hf_token,...`.
ARG HF_MODEL_REPO=jinaai/jina-embeddings-v2-base-code
# Pin to a specific HF commit. Jina hasn't changed weights since
# Apr 2024 so drift risk is low, but pinning eliminates "the build
# silently picked up new weights" surprises. Bump deliberately.
ARG HF_MODEL_REVISION=516f4baf13dec4ddddda8631e019b5737c8bc250

FROM golang:1.25.9-bookworm AS build-stage

ARG ONNXRUNTIME_VERSION
ARG TOKENIZERS_VERSION
ARG HF_MODEL_REPO
ARG HF_MODEL_REVISION
# TARGETARCH is set automatically by buildx (`amd64` or `arm64`) so we
# can pull the matching native ONNX Runtime + libtokenizers tarball.
# Without this, building on Apple Silicon / Graviton picks up x86_64
# binaries and the linker rejects them as "incompatible".
ARG TARGETARCH

# Pull the ONNX Runtime + libtokenizers release tarballs in one layer,
# selecting the right arch per TARGETARCH. The CGO build needs
# onnxruntime headers (-I) at compile time and the .so (-l) at link
# time; the runtime stage copies out just the .so files. daulet's
# libtokenizers ships a static .a, also per-arch.
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) ort_arch=x64;     tok_arch=x86_64  ;; \
      arm64) ort_arch=aarch64; tok_arch=aarch64 ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH"; exit 1 ;; \
    esac; \
    mkdir -p /opt/onnxruntime /opt/libtokenizers; \
    curl --fail --silent --show-error --location \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-${ort_arch}-${ONNXRUNTIME_VERSION}.tgz" \
      -o /tmp/onnxruntime.tgz; \
    tar -xzf /tmp/onnxruntime.tgz -C /opt/onnxruntime --strip-components=1; \
    rm /tmp/onnxruntime.tgz; \
    curl --fail --silent --show-error --location \
      "https://github.com/daulet/tokenizers/releases/download/${TOKENIZERS_VERSION}/libtokenizers.linux-${tok_arch}.tar.gz" \
      -o /tmp/libtokenizers.tar.gz; \
    tar -xzf /tmp/libtokenizers.tar.gz -C /opt/libtokenizers; \
    rm /tmp/libtokenizers.tar.gz

# Pull the embedder artifacts from Jina's official HuggingFace repo.
# It's public — self-hosters and CI build with no token. The
# hf_token build secret is *optional*: if provided (e.g. inside our
# CI to avoid public-rate-limits), curl uses it.
#
# Required files (model + tokenizer) fail the build on miss; the
# small transformers companion JSONs are best-effort. The runtime
# stage copies the whole assets dir into /opt/router/assets/,
# matching defaultAssetsDir in internal/router/cluster/embedder_onnx.go.
#
# File-path mapping (must stay in sync with scripts/hf_files.py):
#   model.onnx              <- onnx/model_quantized.onnx (162 MB INT8)
#   tokenizer.json          <- tokenizer.json
#   {config,tokenizer_config,special_tokens_map}.json -> identity
RUN --mount=type=secret,id=hf_token,required=false \
    set -eux; \
    mkdir -p /opt/router/assets; \
    if [ -s /run/secrets/hf_token ]; then \
      auth_header="Authorization: Bearer $(cat /run/secrets/hf_token)"; \
    else \
      auth_header=""; \
    fi; \
    base="https://huggingface.co/${HF_MODEL_REPO}/resolve/${HF_MODEL_REVISION}"; \
    curl --fail --silent --show-error --location \
      ${auth_header:+--header "$auth_header"} \
      "${base}/onnx/model_quantized.onnx" -o /opt/router/assets/model.onnx; \
    curl --fail --silent --show-error --location \
      ${auth_header:+--header "$auth_header"} \
      "${base}/tokenizer.json" -o /opt/router/assets/tokenizer.json; \
    for f in config.json tokenizer_config.json special_tokens_map.json; do \
      curl --silent --show-error --location \
        ${auth_header:+--header "$auth_header"} \
        --write-out "%{http_code}" \
        "${base}/${f}" -o "/opt/router/assets/${f}" \
        > /tmp/code; \
      code=$(cat /tmp/code); \
      case "$code" in \
        200) ;; \
        404) rm -f "/opt/router/assets/${f}" ;; \
        *) echo "ERROR: unexpected HTTP $code for $f"; exit 1 ;; \
      esac; \
    done; \
    sz=$(stat -c '%s' /opt/router/assets/model.onnx); \
    if [ "$sz" -lt 1048576 ]; then \
      echo "ERROR: model.onnx is only $sz bytes (HF download likely returned a pointer or auth issue)"; \
      exit 1; \
    fi; \
    ls -la /opt/router/assets/

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO_ENABLED=1 (default on bookworm but be explicit) so onnxruntime_go's
# and daulet/tokenizers' CGO bridges compile. CGO_LDFLAGS adds both
# library search paths plus the explicit -lonnxruntime for the dynamic
# load (libtokenizers.a is referenced by daulet/tokenizers' own
# `#cgo LDFLAGS: -ltokenizers`, picked up via the -L flag).
ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/onnxruntime/include" \
    CGO_LDFLAGS="-L/opt/onnxruntime/lib -L/opt/libtokenizers -lonnxruntime"

WORKDIR /app/cmd/router
# `-tags ORT` is required by hugot v0.7+ to enable the ONNX Runtime
# backend; without it `hugot.NewORTSession` errors out at boot and
# main.go fail-opens to the heuristic. Don't drop the tag.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=linux go build -tags ORT -o /server


FROM gcr.io/distroless/cc-debian12 AS build-release-stage

# distroless/cc ships glibc + the C runtime; libonnxruntime.so just
# needs to live somewhere on the linker's search path. /usr/lib is the
# canonical home and is already on the search path on debian12.
COPY --from=build-stage /opt/onnxruntime/lib/libonnxruntime.so* /usr/lib/

# Cluster-scorer assets fetched from HF in the build stage. The Go
# embedder (internal/router/cluster/embedder_onnx.go) reads from this
# directory by default; ROUTER_ONNX_ASSETS_DIR overrides if needed.
COPY --from=build-stage /opt/router/assets/ /opt/router/assets/

WORKDIR /
COPY --from=build-stage /server /server

ARG VERSION
ENV VERSION=$VERSION

ENTRYPOINT ["/server"]
