# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
# Pinned to the toolchain in go.mod so the build never silently downloads another.
# Build on the native platform and cross-compile via GOARCH (pure-Go, CGO off),
# so building a linux/amd64 image on an arm64 host needs no slow emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine AS build

WORKDIR /src

# Module graph first for layer caching. go.sum may be absent for std-lib-only
# binaries (e.g. wol-probe); the COPY/glob tolerates that.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

# BIN selects which command under ./cmd to compile, e.g.
#   --build-arg BIN=onp-controller | onp-wol-agent | wol-probe
ARG BIN
RUN test -n "${BIN}" || (echo "BIN build-arg is required (e.g. --build-arg BIN=onp-controller)" >&2; exit 1)

# Static, reproducible binary: no CGO, stripped, no absolute build paths.
# TARGETOS/TARGETARCH are provided by buildx; default to linux/amd64 otherwise.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}

# ── Privileged runtime stage (onp-shutdown-agent) ────────────────────────────
# The shutdown-agent powers the host off by entering PID 1's namespaces with
# `nsenter` and running the host's `systemctl poweroff`. distroless ships no
# nsenter, so this stage adds util-linux. It runs as root (the DaemonSet is
# privileged) — nsenter into PID 1 needs it. Build with:
#   --target runtime-privileged --build-arg BIN=onp-shutdown-agent
FROM alpine:3.20 AS runtime-privileged
RUN apk add --no-cache util-linux
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]

# ── Runtime stage (default: onp-controller, onp-wol-agent) ────────────────────
# distroless/static:nonroot ships ca-certificates and a nonroot user, which the
# controller needs for its in-cluster TLS client; the static binary needs nothing
# more. wol-agent runs hostNetwork so its L2 broadcast reaches the LAN directly.
# Last stage = default build target, so existing `--build-arg BIN=...` builds
# (no --target) keep getting this minimal image.
FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/app /app

USER nonroot:nonroot
ENTRYPOINT ["/app"]
