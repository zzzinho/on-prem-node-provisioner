# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
# Pinned to the toolchain in go.mod so the build never silently downloads another.
FROM golang:1.26.2-alpine AS build

WORKDIR /src

# Module graph first for layer caching. No go.sum yet — wol-probe uses only the
# standard library — but the COPY/glob tolerates its later appearance.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .

# Static, reproducible binary: no CGO, stripped, no absolute build paths.
# TARGETOS/TARGETARCH are provided by buildx; default to linux/amd64 otherwise.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/wol-probe ./cmd/wol-probe

# ── Runtime stage ─────────────────────────────────────────────────────────────
# scratch: the binary is fully static and does no TLS, so it needs nothing else.
FROM scratch AS runtime

COPY --from=build /out/wol-probe /wol-probe

# NOTE: Wake-on-LAN magic packets are L2 broadcasts. From the default bridge
# network they are NAT'd and will NOT reach the physical LAN — run with
# `--network host` to actually wake a node on the host's segment.
ENTRYPOINT ["/wol-probe"]
