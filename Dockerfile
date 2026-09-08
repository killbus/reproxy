# reproxy — single build path for humans and CI.
#
#   docker build -t reproxy .
#   docker build --build-arg VERSION=v0.4.0 -t reproxy:v0.4.0 .
#
# VERSION is injected into the binary via -ldflags and reported by
# `reproxy --version` (default "dev" when unstamped).
#
# Multi-arch without emulation: the builder stage always runs on the build
# MACHINE's platform (--platform=$BUILDPLATFORM) and produces the TARGET
# architecture through GOARCH. A plain `docker build` passes no platform,
# so TARGETARCH defaults to the build machine's architecture — a native
# build, zero behavior change for humans. Asking for another platform
# (docker buildx build --platform linux/arm64 .) cross-compiles instead of
# emulating: CGO is off, so no cross C toolchain is needed. CI exploits the
# same property by building each architecture on its own native runner.

# --- build stage ---------------------------------------------------------
# golang:<major.minor> must match go.mod (go 1.25.1). Patch-level variants
# of the builder image are interchangeable; major.minor is the contract.
# --platform=$BUILDPLATFORM pulls the builder for the machine RUNNING the
# build, not for the target — that is what keeps a foreign-architecture
# build off emulation.
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

ARG VERSION=dev

# Re-expose BuildKit's automatic target-architecture arg (automatic platform
# args are global-scope; a stage must redeclare them to use them). With no
# --platform requested, TARGETARCH is the build machine's arch — native.
ARG TARGETARCH

WORKDIR /src

# Copy the module (go.mod has zero dependencies — no layer-caching dance
# is warranted; the source arrives in one COPY).
COPY . .

# CGO_ENABLED=0: static binary, the only kind a distroless runtime can run —
# and the enabler of GOARCH-only cross-compilation (nothing to emulate, no
# cross C toolchain to build). -trimpath: no absolute paths of the build
# host in the binary.
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/reproxy \
    ./cmd/reproxy

# --- runtime stage -------------------------------------------------------
# distroless static, NOT scratch: reproxy performs outbound HTTPS with
# certificate validation; a bare scratch image has no CA roots and fails
# the first https upstream. distroless static ships the CA bundle.
#
# static-debian13:nonroot is the maintained spelling (the distroless
# README marks bare `distroless/static` aliases "deprecated and no longer
# updated"; the legacy tag still resolves to the same index — digest
# f2ea2709 at implementation time — but a frozen alias is how CA-bundle
# freshness silently goes stale). The `nonroot` tag runs as UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot

COPY --from=builder /out/reproxy /reproxy

# Explicit for documentation (the base already defaults to this user):
# the container never runs as root.
USER nonroot

EXPOSE 8080

ENTRYPOINT ["/reproxy"]
